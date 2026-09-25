// SPDX-License-Identifier: Apache-2.0

package log

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Alert intake: one event, two front doors, one key (ADR-0005 D2, ADR-0003 D10, ADR-0004 D4,
// 002 FR-008b).
//
// An alert reaches the graph twice on a good day and five times on a bad one: a webhook fires,
// the poll that the webhook triggered returns the same transition, the poll after that returns
// it again because the monitor is still alerting, and somebody declares an incident about it in
// a chat channel that a connector also observes. All of that is *one* fact — "this monitor
// entered this state at this instant" — and the graph has to say so without asking the
// transports to cooperate.
//
// The published key is a 4-tuple:
//
//	sha256(source, stable alert identifier, group, transition instant)
//
// with one reading for each front door:
//
//   - a monitor transition: the stable alert identifier is the monitor or policy id — never a
//     per-notification id, which changes on every delivery — and the group is the monitor's
//     group key, empty when the monitor has no groups;
//   - a human declaration: the stable alert identifier is the stable identifier of the place
//     the incident lives (the channel, the incident record), the transition instant is the
//     instant of declaration, and the group is EMPTY. A declaration has no groups, and pinning
//     that here rather than leaving it to each connector is what makes the two doors one key.
//
// Because the key is derived rather than supplied, a connector that forgets to set
// `idempotency_key` still gets the right behaviour: the second delivery is DUPLICATE_NOOP, and
// a second delivery whose *payload differs* is recorded as a duplicate-delivery finding, which
// is exactly what a transport quietly rewriting an alert's severity should look like.

// AlertTransitionKey is the published idempotency key of an alert transition.
//
// The instant is normalised to UTC and rendered RFC 3339 with nanosecond precision — the same
// spelling canonical serialisation uses everywhere else — so two connectors that agree on the
// instant agree on the key whatever time zone they read it in.
func AlertTransitionKey(sourceID string, body *graphv1.AlertTransition) string {
	var at string
	if ts := body.GetTransitionAt(); ts != nil {
		at = ts.AsTime().UTC().Format(time.RFC3339Nano)
	}
	return AlertTransitionKeyParts(sourceID, graph.RefFromProto(body.GetMonitor()).String(),
		body.GetGroupKey(), at)
}

// AlertTransitionKeyParts is AlertTransitionKey over the four parts themselves, for a connector
// that has them before it has an envelope.
//
// The parts are joined with a NUL byte, which cannot occur in any of them, so no choice of
// group key can be made to collide with a different monitor.
func AlertTransitionKeyParts(sourceID, stableAlertID, group, transitionAt string) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{sourceID, stableAlertID, group, transitionAt}, "\x00")))
	return "alert:" + hex.EncodeToString(sum[:])
}

// The published transition-state vocabulary (ADR-0005 D2). A connector maps its provider's
// states onto these and nothing else; anything it cannot map is `no_data`, which is filtered.
const (
	AlertStateOK       = "ok"
	AlertStateWarn     = "warn"
	AlertStateAlert    = "alert"
	AlertStateNoData   = "no_data"
	AlertStateDeclared = "declared"
)

// Suppression reasons, published so that a connector's "why did you not emit that?" is
// answerable from its own logs.
const (
	// SuppressNoTransition is a delivery that reports the state the monitor was already in.
	// A monitor that is still alerting is not a new fact.
	SuppressNoTransition = "no_transition"
	// SuppressNoData is a transition into or out of `no_data`. The monitor stopped reporting;
	// that is a fact about the monitor, not about the system it watches, and treating it as
	// one is how a feeder outage becomes an incident.
	SuppressNoData = "no_data"
	// SuppressFlapping is a transition that reverts to the state the series was in before the
	// previous transition, within the dwell window. Two of those a minute apart are noise.
	SuppressFlapping = "flapping"
)

// DefaultAlertDwell is the window inside which a reversal counts as flapping. One minute is
// short enough to keep a real recovery — a rollback that fixes the outage two minutes after it
// started — and long enough to swallow a monitor oscillating on its threshold.
const DefaultAlertDwell = time.Minute

// AlertSeries is the little state a connector keeps per key prefix (source, alert id, group) so
// that it can apply the published filter. It is deliberately not persisted anywhere: losing it
// costs at most one redundant event, and a redundant event is a DUPLICATE_NOOP.
type AlertSeries struct {
	// State is the state the series is in, as last emitted.
	State string
	// PreviousState is the state before that.
	PreviousState string
	// At is the instant of the last emitted transition.
	At time.Time
}

// SuppressAlertTransition is the published feeder-side filter (ADR-0005 D2).
//
// It returns the reason to drop the transition, or "" to emit it. Three rules, and one rule
// that is deliberately absent:
//
//   - a transition to the state the series is already in is dropped (`no_transition`);
//   - anything touching `no_data` is dropped (`no_data`);
//   - a reversal inside the dwell window is dropped (`flapping`);
//   - **a recovery is kept.** `alert → ok` is not noise: it bounds the outage, it is what
//     distinguishes "this is still happening" from "this healed itself at 14:31", and an
//     investigation that cannot see the recovery cannot tell whether the change it is looking
//     at was the fix (ADR-0003 D10, ADR-0004 D4).
//
// The filter is a pure function of the previous transition in the series and the new one, so a
// connector can test its own mapping against it, and so this rule lives in one place rather
// than once per vendor.
func SuppressAlertTransition(prev *AlertSeries, body *graphv1.AlertTransition, dwell time.Duration) string {
	to := body.GetToState()
	from := body.GetFromState()
	if to == AlertStateNoData || from == AlertStateNoData {
		return SuppressNoData
	}
	if prev == nil {
		return ""
	}
	if to == prev.State {
		return SuppressNoTransition
	}
	if dwell <= 0 {
		dwell = DefaultAlertDwell
	}
	if to == prev.PreviousState && !body.GetTransitionAt().AsTime().After(prev.At.Add(dwell)) {
		return SuppressFlapping
	}
	return ""
}
