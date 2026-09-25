// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The slow loop, runtime limit discovery, and backing off (FR-148, FR-151; contracts/budget.md
// §3, §4, §7).
//
// ---------------------------------------------------------------------------------------------
// The slow loop is a DRIFT CHECK, never an authority.
//
// It reads serviceruntime.googleapis.com/quota/limit minus quota/rate/net_usage, which is the only
// usage figure GCP reports at all. Three properties decide how it may be used: it is minutes stale
// (net_usage is a DELTA metric on 1-minute sampling), it costs monitoring.query quota to read, and
// it is a reconciliation of what already happened rather than a statement about what is left right
// now.
//
// So it catches two things worth catching — an out-of-band quota increase an operator made, and this
// integration's own accounting drifting from GCP's — and it NEVER authorises a call the fast loop
// refused. A minutes-stale figure cannot authorise anything during a 60-second incident, and wiring
// it as an authority is the one mistake that would turn a safety mechanism into the cause.

// QuotaReader reads the vendor's own usage metrics. An interface for the same reason IAMTester is:
// a reconciliation loop that can only be exercised against live GCP is one nobody exercises.
type QuotaReader interface {
	// ReadLimit returns the configured limit for a service's named limit, as GCP reports it.
	ReadLimit(ctx context.Context, project, service, limitName string) (int, error)
	// ReadNetUsage returns consumption over the sampling window.
	ReadNetUsage(ctx context.Context, project, service, limitName string) (int, error)
}

// Reconciliation is one slow-loop result.
type Reconciliation struct {
	Class EndpointClass
	// VendorRemaining is limit − net_usage, as computed from the vendor's figures. Labelled
	// vendor-reported in the usage report, and minutes stale by construction.
	VendorRemaining int
	// SelfTracked is what the fast loop believed at the same moment.
	SelfTracked int
	// Drift is SelfTracked − VendorRemaining. A large positive drift means this integration thinks
	// it has more headroom than GCP does, which is the direction that ends in a 429.
	Drift int
	// At is when the reconciliation ran.
	At time.Time
	// Err is why it could not run. A failed reconciliation is reported, never silently treated as
	// agreement.
	Err string
}

// DiscoverMonitoringLimit finds Cloud Monitoring's timeSeries.list limit at startup.
//
// Google does not publish this number — its quota pages decline to give one and redirect the reader
// to the Quotas dashboard — and a budget expressed as a share of an unknown limit is not a budget.
// So it is discovered, recorded in the checkpoint so a later reader knows what share was a share OF,
// and on failure falls back to a conservative configured default that the usage report labels as a
// FALLBACK rather than as discovered.
//
// "Limit unknown" is never read as "limit unlimited". That is the whole of contracts/budget.md §4.
func DiscoverMonitoringLimit(ctx context.Context, reader QuotaReader, project string, fallback int, usage *Usage) (ClassLimit, FigureSource) {
	limit := ClassLimit{
		Class:            ClassMonitoringQuery,
		Period:           time.Minute,
		Scope:            ScopeProject,
		SteadyStateShare: 0.50,
		HumanReserve:     0.25,
	}
	if reader != nil {
		discovered, err := reader.ReadLimit(ctx, project, "monitoring.googleapis.com", "TimeSeriesQueriesPerMinutePerProject")
		if err == nil && discovered > 0 {
			limit.Limit = discovered
			limit.Discovered = true
			usage.SetLimitSource(ClassMonitoringQuery, string(ClassMonitoringQuery)+"|"+project, SourceVendorReported)
			return limit, SourceVendorReported
		}
	}
	limit.Limit = fallback
	limit.Discovered = false
	usage.SetLimitSource(ClassMonitoringQuery, string(ClassMonitoringQuery)+"|"+project, SourceFallback)
	return limit, SourceFallback
}

// Reconcile runs the slow loop for one class and reports the drift. It returns the reconciliation
// rather than acting on it: applying a vendor figure is SetLimit's job and an operator's decision,
// and a function that both measured and applied would make the "never authorises" rule a matter of
// who called it.
func Reconcile(ctx context.Context, reader QuotaReader, budget *Budget, class EndpointClass, project, service, limitName string) Reconciliation {
	rec := Reconciliation{Class: class, At: time.Now().UTC()}
	if reader == nil {
		rec.Err = "no quota reader configured; the slow loop did not run"
		return rec
	}
	limit, err := reader.ReadLimit(ctx, project, service, limitName)
	if err != nil {
		rec.Err = fmt.Sprintf("read limit: %v", err)
		return rec
	}
	used, err := reader.ReadNetUsage(ctx, project, service, limitName)
	if err != nil {
		rec.Err = fmt.Sprintf("read net usage: %v", err)
		return rec
	}
	rec.VendorRemaining = limit - used
	if l, ok := budget.Limit(class); ok {
		rec.SelfTracked = int(float64(l.Limit) * l.SteadyStateShare)
	}
	rec.Drift = rec.SelfTracked - rec.VendorRemaining
	return rec
}

// ---------------------------------------------------------------------------------------------
// Backing off (FR-151, contracts/budget.md §7).

// Backoff is the published policy: honour what the response asks where it says, else truncated
// exponential from 1s, and resume from the position reached rather than restarting the cycle.
type Backoff struct {
	Initial    time.Duration
	Max        time.Duration
	Multiplier float64
	Jitter     bool
}

// DefaultBackoff is Google's documented guidance, and config/gcp-budget.yaml's defaults.
func DefaultBackoff() Backoff {
	return Backoff{Initial: time.Second, Max: time.Minute, Multiplier: 2.0, Jitter: true}
}

// Delay returns how long to wait before attempt n (0-based), honouring a vendor-stated retry
// instant where there is one.
//
// The vendor's instant wins whenever it is LONGER. Waiting less than the response asked is how a
// backoff turns into a second rate-limit breach, and the asymmetry is deliberate: a server that
// says "wait 30s" knows something this client does not.
func (b Backoff) Delay(attempt int, retryAfter time.Duration) time.Duration {
	computed := time.Duration(float64(b.Initial) * math.Pow(b.Multiplier, float64(attempt)))
	if computed > b.Max {
		computed = b.Max
	}
	if b.Jitter && computed > 0 {
		// Full jitter: uniform in [0, computed]. It spreads a fleet's retries instead of
		// synchronising them into a second thundering herd against the same 60/min pool.
		computed = time.Duration(rand.Int64N(int64(computed) + 1))
	}
	if retryAfter > computed {
		return retryAfter
	}
	return computed
}

// RateLimited reports whether err is a quota refusal, and how long the vendor asked for.
//
// It recognises both transports this integration uses: googleapi.Error for the REST clients
// (sqladmin, the deprecated monitoring/v3) and a gRPC status for the GAPIC ones.
func RateLimited(err error) (retryAfter time.Duration, ok bool) {
	if err == nil {
		return 0, false
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == 429 || apiErr.Code == 403 && strings.Contains(apiErr.Message, "quota") {
			if h := apiErr.Header.Get("Retry-After"); h != "" {
				if secs, perr := time.ParseDuration(h + "s"); perr == nil {
					return secs, true
				}
			}
			return 0, true
		}
	}

	if st, okStatus := status.FromError(err); okStatus && st.Code() == codes.ResourceExhausted {
		for _, d := range st.Details() {
			if ri, isRetry := d.(interface {
				GetRetryDelay() interface{ AsDuration() time.Duration }
			}); isRetry {
				if rd := ri.GetRetryDelay(); rd != nil {
					return rd.AsDuration(), true
				}
			}
		}
		return 0, true
	}
	return 0, false
}
