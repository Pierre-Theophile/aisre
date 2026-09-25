// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The call budget's fast loop: a client-side token bucket per (API, project, region), which is the
// real-time gate every vendor call passes through (FR-146–FR-149; contracts/budget.md §1–§3).
//
// ---------------------------------------------------------------------------------------------
// Why this is self-tracked rather than asked.
//
// FR-148 says to use the vendor's reported remaining quota where it reports one and self-track where
// it does not. For all four relevant APIs GCP reports **no usable real-time figure**: there are no
// rate-limit response headers, so the first signal the quota is gone is 429/RESOURCE_EXHAUSTED; the
// Cloud Quotas and Service Usage APIs expose configured LIMITS rather than consumption; and the only
// vendor usage figure — Cloud Monitoring's serviceruntime metrics — is minutes-stale and costs
// monitoring.query quota to read (research §7).
//
// So every real-time decision here is self-tracked, quota.go runs the slow vendor-reported loop as a
// reconciliation and drift check that NEVER authorises a call this refused, and the usage report
// labels each figure as one or the other. That is the honest reading of FR-148 rather than a claim
// that both branches are symmetric.
//
// ---------------------------------------------------------------------------------------------
// The human reserve is the point, not a politeness setting.
//
// logging.entries.list is 60 calls per minute PER PROJECT, the limit is not hierarchical, and it is
// shared with every human querying Cloud Logging in that project. One call per second, project-wide,
// during an incident, against the same pool as the on-call trying to read the logs.
//
// So a class carries a steady-state share AND a reserve that is never spent. At the reserve floor
// this integration YIELDS — and says so as a typed reason, because an investigation that stopped for
// quota and one that stopped because nothing was implicated are opposite conclusions, and a consumer
// that cannot tell them apart will read the first as the second at exactly the wrong moment
// (FR-149, SC-022).

// EndpointClass is the unit a budget is expressed in. Budgets are per class rather than one global
// call count because GCP's quotas are per API and per project, and a single figure would let a cheap
// class starve a binding one (FR-147).
type EndpointClass string

// The published classes (contracts/budget.md §1, config/gcp-budget.yaml).
const (
	// ClassLoggingRead is the binding one. Everything in this file exists because of it.
	ClassLoggingRead EndpointClass = "logging.read"
	// ClassMonitoringQuery is binding too, and its limit is not publicly documented, so it is
	// discovered at runtime (quota.go). "Limit unknown" is never read as "limit unlimited".
	ClassMonitoringQuery    EndpointClass = "monitoring.query"
	ClassRunRead            EndpointClass = "run.read"
	ClassSQLAdminRead       EndpointClass = "sqladmin.read"
	ClassMonitoringPolicies EndpointClass = "monitoring.policies"
	ClassComputeRead        EndpointClass = "compute.read"
	// ClassContainerRead is GKE cluster metadata, and nothing inside a cluster (FR-030). Its limit
	// is not publicly documented, so it is metered rather than assumed unlimited — and its volume is
	// one call per project per poll, because `clusters.list` takes `-` as the location wildcard.
	ClassContainerRead EndpointClass = "container.read"
	ClassDNSRead       EndpointClass = "dns.read"
	ClassPubSubPull    EndpointClass = "pubsub.pull"
)

// ClassLimit is one class's published limit and shares.
type ClassLimit struct {
	Class EndpointClass
	// Limit is calls per Period. Zero means no limit is known — which is metered and reported,
	// never treated as unlimited.
	Limit  int
	Period time.Duration
	// Scope decides the bucket key: a limit that is per project keeps one bucket per project, a
	// limit that is per project per region keeps one per pair. Getting this wrong in the generous
	// direction spends someone else's quota.
	Scope BucketScope
	// SteadyStateShare is the fraction of Limit this integration may use, and HumanReserve the
	// fraction it must never touch. They are not required to sum to 1: the gap is headroom that
	// neither side has claimed.
	SteadyStateShare float64
	HumanReserve     float64
	// Discovered records whether Limit came from the vendor at runtime or from a configured
	// fallback, so the usage report can say which (contracts/budget.md §4).
	Discovered bool
}

// BucketScope is what a limit is counted against.
type BucketScope string

const (
	ScopeGlobal        BucketScope = "global"
	ScopeProject       BucketScope = "project"
	ScopeProjectRegion BucketScope = "project_region"
)

// YieldError is the typed refusal FR-149 requires: the integration stopped for QUOTA, which is a
// different fact from stopping because nothing was found.
type YieldError struct {
	Class     EndpointClass
	Key       string
	Remaining int
	Reserve   int
	RetryAt   time.Time
	// Kind says whether this is the floor or the pacing above it (deferral.go). Both are yields and
	// both are RATE_LIMITED to a caller, and they are different facts about the run: a paced call
	// will be allowed shortly, and a call at the reserve will not be allowed until the bucket
	// refills. An operator reading a report needs to tell "we are backing off" from "we are out".
	Kind YieldKind
}

func (e *YieldError) Error() string {
	if e.Kind == YieldPaced {
		return fmt.Sprintf("gcpx: paced on %s for %s: %d call(s) left against a human reserve of %d, "+
			"which is inside the decay band, so calls are spaced to approach the reserve gradually "+
			"rather than sprint to it; retry after %s",
			e.Class, e.Key, e.Remaining, e.Reserve, e.RetryAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("gcpx: yielded on %s for %s: %d call(s) left and %d are the human reserve, "+
		"which is never spent; retry after %s",
		e.Class, e.Key, e.Remaining, e.Reserve, e.RetryAt.UTC().Format(time.RFC3339))
}

// StopReason is what a caller reports when this yield stopped its work. It is always StopQuota and
// never StopEvidence: a call the budget refused established nothing about the window (FR-149).
func (e *YieldError) StopReason() StopReason { return StopQuota }

// Yielded reports whether err is a budget yield, so a caller can tell "we stopped for quota" from
// "we looked and found nothing" without string matching.
func Yielded(err error) (*YieldError, bool) {
	var y *YieldError
	if ok := asYield(err, &y); ok {
		return y, true
	}
	return nil, false
}

// Budget is the fast loop. It is safe for concurrent use: a feeder polling several areas and a
// telemetry backend answering an investigation share one, which is the only way the reserve means
// anything — two budgets would each honour it and together spend twice the share.
type Budget struct {
	mu      sync.Mutex
	limits  map[EndpointClass]ClassLimit
	buckets map[string]*bucket
	now     func() time.Time
	usage   *Usage
}

// bucket also remembers when it last admitted a call, which is what the decay paces against.
type bucket struct {
	tokens     float64
	lastRefill time.Time
	// lastAdmit is when this bucket last let a call through, which is what the decay paces against.
	lastAdmit time.Time
}

// NewBudget builds a budget from the published class limits.
func NewBudget(limits []ClassLimit, usage *Usage) *Budget {
	byClass := make(map[EndpointClass]ClassLimit, len(limits))
	for _, l := range limits {
		byClass[l.Class] = l
	}
	return &Budget{
		limits:  byClass,
		buckets: map[string]*bucket{},
		now:     time.Now,
		usage:   usage,
	}
}

// Take spends one call of class, or refuses with a *YieldError.
//
// A class with no configured limit is METERED AND ALLOWED: the usage report still counts it, and it
// is reported as unlimited-as-far-as-we-know rather than silently uncounted. That is the honest
// handling of an undocumented limit — refusing every call to an API whose limit Google does not
// publish would disable most of the integration on a technicality.
// Issue is how a call is spent: it names the OPERATION, not an endpoint class.
//
// This is the whole of SC-020's enforcement, and the reason `take` below is unexported. An
// operation is looked up in the published read-only list (requestlog.go), refused outright if it is
// not on it, metered against the class the LIST names rather than one the call site chose, and
// recorded in the request log the success criterion is verified from. A caller cannot spend a call
// without naming what it is about to issue, and cannot name something unpublished, because the
// constant would not exist and a string literal is refused.
//
// The order matters: the list is consulted BEFORE any quota is spent, so an unpublished operation
// costs nothing and, more to the point, is refused identically on a nil budget — an unmetered run
// is not a reason to stop checking what may be called.
func (b *Budget) Issue(ctx context.Context, op Operation, project, region string) error {
	spec, err := Issuable(op)
	if err != nil {
		if b != nil {
			b.usage.recordBlocked(op, err.Error())
		}
		return err
	}
	if b == nil {
		// An unmetered run: a unit test, or a replay from disk where no call leaves the process.
		// There is no usage to record into and nothing was issued, which is the honest log.
		return ctx.Err()
	}
	takeErr := b.take(ctx, spec.Class, project, region)
	if ctxErr := ctx.Err(); ctxErr != nil && takeErr != nil {
		// The context died rather than the budget refusing. That is not a request and not a
		// quota refusal, so it is neither counted nor logged.
		return takeErr
	}
	b.usage.recordRequest(op, spec, takeErr == nil)
	return takeErr
}

// take meters one call against an endpoint class. It is unexported so that Issue is the only way to
// spend a call: an exported Take would be a way to reach GCP without naming the operation, and the
// request log SC-020 is verified from would have a hole in it exactly the shape of whoever used it.
func (b *Budget) take(ctx context.Context, class EndpointClass, project, region string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b == nil {
		// A nil budget is an unmetered run — a unit test, or a replay from disk where no call
		// leaves the process. Allowing it is right; a replay that had to construct a budget to
		// read a recording would be a budget about nothing.
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	limit, known := b.limits[class]
	key := bucketKey(class, limit.Scope, project, region)
	if !known || limit.Limit <= 0 {
		// No limit known: metered so the report can say what was spent, allowed so a technicality
		// does not take the feature out. No reserve to breach, so 0, 0.
		b.usage.record(class, key, 1, true, 0, 0)
		return nil
	}

	bkt := b.buckets[key]
	now := b.now()
	if bkt == nil {
		// Seeded at the SHARE, not at the limit. The integration has not spent anything yet, so it
		// starts with everything it may hold — and everything it may hold is its share.
		//
		// Seeding at the limit was the first spelling and it was wrong in a way only the decay made
		// visible: a fresh process could serve `Limit − reserve` calls in its first second, which for
		// the binding class is 42 of 60 rather than the 24 its share allows. `refill` caps at the
		// share, so the excess was unreachable ever again — a cold-start burst only, which is exactly
		// when an integration is most likely to be starting *because* something is wrong and a human
		// is already querying the same project.
		bkt = &bucket{tokens: shareCeiling(limit), lastRefill: now}
		b.buckets[key] = bkt
	}
	refill(bkt, limit, now)

	reserve := float64(limit.Limit) * limit.HumanReserve
	if bkt.tokens-1 < reserve {
		retryAt := now.Add(timeToTokens(bkt, limit, reserve+1))
		b.usage.recordRefusal(class, key, retryAt)
		return &YieldError{
			Class:     class,
			Key:       key,
			Remaining: int(bkt.tokens),
			Reserve:   int(reserve),
			RetryAt:   retryAt,
			Kind:      YieldReserve,
		}
	}

	// The decay (T161, FR-149). Above the band the share is spent freely — it exists to be used. Inside
	// it, calls are spaced in inverse proportion to the headroom left, so the integration approaches
	// the reserve gradually instead of sprinting to it and stopping dead. What a human querying Cloud
	// Logging in the same project sees is the integration backing away rather than crowding them until
	// it hits the wall.
	if wait := pacing(bkt, limit, reserve, now); wait > 0 {
		retryAt := bkt.lastAdmit.Add(wait)
		b.usage.recordRefusal(class, key, retryAt)
		return &YieldError{
			Class:     class,
			Key:       key,
			Remaining: int(bkt.tokens),
			Reserve:   int(reserve),
			RetryAt:   retryAt,
			Kind:      YieldPaced,
		}
	}

	bkt.tokens--
	bkt.lastAdmit = now
	b.usage.record(class, key, 1, true, bkt.tokens, reserve)
	return nil
}

// pacing returns how much longer this call must wait, or zero if it may go now.
//
// The band is measured over the integration's **own** headroom — the tokens between the reserve and the
// ceiling its share allows — and not over the vendor's limit. GCP reports no in-band remaining figure
// for any of these APIs (research §7), so the only remaining quota this process can observe is its own
// bucket, and pacing against a figure nobody publishes would be pacing against a guess.
//
// Inside the band the minimum interval is the steady-state interval divided by the fraction of the band
// still left: at half the band, twice the interval; at a tenth, ten times. The floor is the reserve, and
// the check above it is what stops this function ever being asked to divide by zero.
func pacing(bkt *bucket, limit ClassLimit, reserve float64, now time.Time) time.Duration {
	ceiling := float64(limit.Limit) * limit.SteadyStateShare
	if ceiling <= reserve {
		// A share that does not clear the reserve leaves nothing to pace over. The reserve check
		// above already decides every call in that configuration.
		return 0
	}
	headroom := (bkt.tokens - reserve) / (ceiling - reserve)
	if headroom >= DecayBand {
		return 0
	}
	if headroom <= 0 {
		// Unreachable: the reserve check above returns first. Kept so a future edit that reorders
		// them cannot divide by zero here.
		return limit.Period
	}
	perSecond := float64(limit.Limit) * limit.SteadyStateShare / limit.Period.Seconds()
	if perSecond <= 0 {
		return 0
	}
	steady := time.Duration(float64(time.Second) / perSecond)
	want := time.Duration(float64(steady) * DecayBand / headroom)
	if bkt.lastAdmit.IsZero() {
		// Nothing has been admitted from this bucket yet, so there is no interval to measure. The
		// first call inside the band goes, and the pacing starts from it.
		return 0
	}
	if elapsed := now.Sub(bkt.lastAdmit); elapsed >= want {
		return 0
	}
	return want - now.Sub(bkt.lastAdmit)
}

// refill adds the tokens elapsed time has earned, capped at the share this integration may hold.
//
// The cap is the SHARE, not the limit: holding a full limit's worth of tokens would let a quiet hour
// fund a burst that spends the humans' reserve in one second, which is precisely the failure the
// reserve exists to prevent.
func refill(bkt *bucket, limit ClassLimit, now time.Time) {
	if !now.After(bkt.lastRefill) {
		return
	}
	elapsed := now.Sub(bkt.lastRefill)
	perSecond := float64(limit.Limit) / limit.Period.Seconds()
	bkt.tokens = minFloat(bkt.tokens+elapsed.Seconds()*perSecond, shareCeiling(limit))
	bkt.lastRefill = now
}

// shareCeiling is the most tokens this integration may hold: its share of the limit.
//
// A share that computes to less than one token falls back to the limit, because a ceiling below one
// would refuse every call — and a class configured that way has a configuration problem, not a budget
// that should quietly do nothing.
func shareCeiling(limit ClassLimit) float64 {
	ceiling := float64(limit.Limit) * limit.SteadyStateShare
	if ceiling < 1 {
		return float64(limit.Limit)
	}
	return ceiling
}

// timeToTokens is how long until the bucket holds want tokens.
func timeToTokens(bkt *bucket, limit ClassLimit, want float64) time.Duration {
	perSecond := float64(limit.Limit) / limit.Period.Seconds()
	if perSecond <= 0 {
		return limit.Period
	}
	need := want - bkt.tokens
	if need <= 0 {
		return 0
	}
	return time.Duration(need / perSecond * float64(time.Second))
}

// SetLimit replaces one class's limit, which is how quota.go installs a limit it discovered at
// runtime and how a reconciliation applies an out-of-band quota increase.
func (b *Budget) SetLimit(limit ClassLimit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limits[limit.Class] = limit
	// The buckets keyed to this class are dropped so the next Take reseeds against the new limit.
	// Keeping them would carry a token count that means a different fraction than it did.
	for key := range b.buckets {
		if hasClassPrefix(key, limit.Class) {
			delete(b.buckets, key)
		}
	}
}

// SetClock replaces the budget's clock.
//
// It exists for two callers and no others: a test that needs elapsed time without sleeping for it,
// and a deterministic replay, where a budget reading the wall clock would make a recording's
// refusals depend on how long the replay took. Both are cases where "now" is data rather than a
// fact about the machine.
func (b *Budget) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
}

// Limit returns a class's current limit and whether one is known.
func (b *Budget) Limit(class EndpointClass) (ClassLimit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.limits[class]
	return l, ok
}

func bucketKey(class EndpointClass, scope BucketScope, project, region string) string {
	switch scope {
	case ScopeProjectRegion:
		return string(class) + "|" + project + "|" + region
	case ScopeProject:
		return string(class) + "|" + project
	default:
		return string(class) + "|"
	}
}

func hasClassPrefix(key string, class EndpointClass) bool {
	prefix := string(class) + "|"
	return len(key) >= len(prefix) && key[:len(prefix)] == prefix
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
