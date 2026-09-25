// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The integration's telemetry about its own operation (FR-011, FR-051).
//
// Constitution §Self-observability is the reason this exists at every layer rather than at the
// edges: an integration that cannot say what it is doing to somebody else's quota is exactly the
// kind of thing this project is built to make visible in other systems. The instruments below are
// the ones FR-011 enumerates — calls per area, quota refusals, poll duration and lag, events
// emitted and rejected, notices read and dropped with reason, digests and truncations, and observed
// skew.
//
// It is an interface rather than a direct dependency on the OTel SDK for one reason: a feeder under
// test, and a replay from a recording, should not need a meter provider to run. A nil Instruments
// is a valid one, and self-observability is never a precondition for feeding — the same rule
// feederTelemetry follows in internal/cli.

// Instruments is what the integration reports about itself.
type Instruments interface {
	// Call records one vendor call: which area, which class, whether it was allowed, how long.
	Call(ctx context.Context, area string, class EndpointClass, allowed bool, d time.Duration)
	// QuotaRefusal records a yield or a vendor 429, with the class and the retry instant, so a
	// reader can tell a budget yield from a rate-limit breach.
	QuotaRefusal(ctx context.Context, class EndpointClass, retryAt time.Time, vendorSaid bool)
	// Poll records one poll cycle for an area: how long it took, and how far behind the source
	// it ended up.
	Poll(ctx context.Context, area string, d, lag time.Duration)
	// Events records what a cycle emitted and what the graph refused.
	Events(ctx context.Context, area string, emitted, rejected int)
	// Notices records the vendor-notice feeder's line: read, extracted, and dropped with a
	// reason. The reason matters more than the count — most mailbox traffic is not a notice, so
	// "dropped" is the normal path and a count without reasons cannot distinguish it from a
	// broken extractor.
	Notices(ctx context.Context, read, extracted int, droppedByReason map[string]int)
	// Digest records one telemetry-backend answer: the term, its outcome, and whether it was
	// truncated by a window cap.
	Digest(ctx context.Context, term, outcome string, truncated bool)
	// Skew publishes the observed clock skew. Reported, never used to correct (FR-153).
	//
	// The type is pkg/feeder's: the observation is not GCP-shaped and three copies of it would be
	// three places to fix the next thing (004 T045).
	Skew(ctx context.Context, report feeder.SkewReport)
}

// NopInstruments discards everything. It is the zero value a test or a replay runs with.
type NopInstruments struct{}

func (NopInstruments) Call(context.Context, string, EndpointClass, bool, time.Duration) {}
func (NopInstruments) QuotaRefusal(context.Context, EndpointClass, time.Time, bool)     {}
func (NopInstruments) Poll(context.Context, string, time.Duration, time.Duration)       {}
func (NopInstruments) Events(context.Context, string, int, int)                         {}
func (NopInstruments) Notices(context.Context, int, int, map[string]int)                {}
func (NopInstruments) Digest(context.Context, string, string, bool)                     {}
func (NopInstruments) Skew(context.Context, feeder.SkewReport)                          {}

var _ Instruments = NopInstruments{}
