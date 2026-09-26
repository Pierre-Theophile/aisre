// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// The six US1 fixtures (T063–T066), each written against the one thing it has to prove.
//
// Every query is **observed-time pinned**. The rule is in fixture-format.md and the reason is not
// fastidiousness: an unpinned query answers "as of now", so its golden encodes the day it was
// recorded and starts failing on some later date for no reason anybody can find. Feature 002 was
// caught by exactly that on 2026-09-20.

// pinnedAt is the instant every golden is pinned to: after the last observation in every fixture.
var pinnedAt = fixtureStart.Add(6 * time.Hour)

// cycleGap is how far apart successive poll cycles *arrive*. It is longer than the feeder's declared
// reordering window, and that is what makes the shuffle step test something real rather than noise.
//
// The shuffle keeps the observed-time slots where they are and permutes which event lands in which —
// so two events whose arrivals are inside one window may swap their observed times. For a poller that
// asserts the same entity on every cycle, swapping the observed times of two cycles means closing an
// observed interval before it opened, which the projector refuses. Spacing the cycles beyond the
// window means the shuffle permutes the events *within* one cycle, which is the reordering a poll can
// genuinely produce, and leaves the cycle order alone, which it cannot.
//
// It also models the cadence honestly: `logging.entries.list` is 60 calls per minute per project and
// shared with humans (contracts/budget.md), so a Cloud Run poll every half hour is what the budget
// actually affords — not every thirty seconds.
const cycleGap = 30 * time.Minute

// cycleAt returns the arrival instant of the nth poll cycle. The *content's* instants — a revision's
// createTime, an audit entry's timestamp — are unaffected: a poll at 15:00 observing a revision
// created at 14:18 is the ordinary case, and keeping the two apart is what lets valid time stay exact
// while observed time stays coarse.
func cycleAt(n int) time.Time { return fixtureStart.Add(time.Duration(n) * cycleGap) }

// T063 — the baseline twin. Services, revisions, the split, the pointers, and nothing changing.
func baselineTopologyFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{
		"team":        twinTeam,
		"environment": "production",
		"service":     twinService,
		// Two labels that must become nothing: an unlisted key, and an allowlisted key whose value
		// measures something (FR-124, FR-126). A fixture that carried only well-behaved labels would
		// not exercise either rule.
		"jira-ticket": "PLAT-1",
		"component":   "250ms",
	}
	return fixtureSpec{
		dir:    "fixtures/gcp-baseline-topology-01",
		family: "gcp-topology",
		description: "One Cloud Run service with two revisions, all traffic on the older one, " +
			"observed by a single complete poll. Nothing changes, so every query at any instant after " +
			"the first observation returns the same graph. Exercises the identifier namespaces with " +
			"project and region in the value, the addressing ref being claimed, the label allowlist " +
			"and the measurement rule, the OWNER node with an unknown valid start, and the pointers " +
			"every node carries.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceJSON(
				"uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureCreated,
				twinRevOld, twinRevNew, map[string]int{twinRevOld: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(0),
				twinRevisionJSON(twinRevNew, fixtureCreated, "42"),
				twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
			pollPayloadAt(cycleAt(0), "complete", ""),
		},
		queries: `queries:
  # The serving topology as of the end of the clock. Pinned, so the golden does not encode the day
  # it was recorded (fixture-format.md).
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The pointers the service carries, which is what FR-079 is asserted against.
  - name: storefront-pointers
    kind: pointers
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The extent, which states the scope in force and the horizon: "not present" and "not in scope at
  # that time" are different answers (FR-009).
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// T064 — the two rollouts at their two distinct instants. This is the fixture SC-002 lives on.
func rolloutTrafficShiftFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	return fixtureSpec{
		dir:    "fixtures/gcp-rollout-traffic-shift-01",
		family: "gcp-rollout",
		description: "A revision created at 14:18 and given 100% of the traffic at 14:20, asserting " +
			"**two** rollout changes at those two distinct instants: the creation at the API's " +
			"createTime marked as not having moved traffic, and the shift at the operation.last audit " +
			"entry's timestamp marked as having moved it. The request entry at 14:19:30 is present and " +
			"is deliberately not the answer — dating from it would put the change at the moment " +
			"somebody pressed deploy rather than the moment production served differently.",
		payloads: []feeder.Payload{
			// Cycle 0: the old revision serves everything.
			servicesPayloadAt(t, cycleAt(0), twinServiceJSON(
				"uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureStart,
				twinRevOld, twinRevOld, map[string]int{twinRevOld: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// Cycle 1: the new revision exists (created 14:18), and the audit pair for the shift has
			// been read — requested 14:19:30, completed 14:20:00. The audit page arrives a cycle
			// *before* the poll that notices the split, which is both realistic and what keeps the
			// two independent: within one cycle, a services poll landing before the audit page would
			// hold the shift instead of dating it, and the shuffle would be testing that ordering
			// rather than the feeder's valid-time logic.
			revisionsPayloadAt(t, cycleAt(1), twinRevisionJSON(twinRevNew, fixtureCreated, "42")),
			auditPayloadAt(cycleAt(1), twinAuditPair(
				"op-shift-1", fixtureShifted.Add(-30*time.Second), fixtureShifted)),
			pollPayloadAt(cycleAt(1), "complete", ""),
			// Cycle 2: the poll observes the new split, and the completion entry dates it.
			servicesPayloadAt(t, cycleAt(2), twinServiceJSON(
				"uid-service-1", 42, fixtureStart.Add(-720*time.Hour), fixtureShifted,
				twinRevNew, twinRevNew, map[string]int{twinRevNew: 100}, labels)),
			pollPayloadAt(cycleAt(2), "complete", ""),
		},
		queries: `queries:
  # The two rollouts, at their two instants. A diff over the window that contains both must list
  # both, and the one that moved traffic must be distinguishable from the one that did not.
  - name: storefront-diff-over-the-deploy
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureCreated.Add(-time.Minute)) + `
    t2: ` + rfc3339(fixtureShifted.Add(time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # A window that ends between the two instants: the creation is a candidate, the shift is not yet.
  # This is the query that fails if both changes share an instant.
  - name: storefront-diff-between-the-two-instants
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureCreated.Add(-time.Minute)) + `
    t2: ` + rfc3339(fixtureCreated.Add(time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # What was serving at 14:19 — after the revision was created and before traffic moved.
  - name: storefront-asof-1419
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(fixtureCreated.Add(time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
`,
	}
}

// T065a — the zero-traffic revision: it exists, it is not serving, and its creation change stands.
func revisionZeroTrafficFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	return fixtureSpec{
		dir:    "fixtures/gcp-revision-zero-traffic-01",
		family: "gcp-rollout",
		description: "A revision created at 14:18 that never receives traffic: the split names it at " +
			"0%. It still exists as a node and still produced its creation change (FR-018), it is not " +
			"presented as serving, and no traffic-shift change is emitted because the split never " +
			"changed.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceJSON(
				"uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureStart,
				twinRevOld, twinRevOld, map[string]int{twinRevOld: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// Cycle 1: the new revision is created and appears in the split at 0%, which is a
			// different fact from not being in the split at all.
			revisionsPayloadAt(t, cycleAt(1), twinRevisionJSON(twinRevNew, fixtureCreated, "42")),
			servicesPayloadAt(t, cycleAt(1), twinServiceJSON(
				"uid-service-1", 42, fixtureStart.Add(-720*time.Hour), fixtureCreated,
				twinRevOld, twinRevNew, map[string]int{twinRevOld: 100, twinRevNew: 0}, labels)),
			pollPayloadAt(cycleAt(1), "complete", ""),
		},
		queries: `queries:
  # The revision exists and is not serving. Both halves are in one answer.
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The creation change is a candidate cause even though no traffic moved: a revision that crashed
  # on startup is the case this exists for.
  - name: storefront-diff-over-the-creation
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureCreated.Add(-time.Minute)) + `
    t2: ` + rfc3339(fixtureCreated.Add(2*time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}

// T065b — the rollback: three versions of the split, and the third change is its own change.
func rollbackFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	return fixtureSpec{
		dir:    "fixtures/gcp-rollback-01",
		family: "gcp-rollout",
		description: "Traffic moved to the new revision at 14:20 and back to the old one at 14:41. " +
			"The split property has three versions in valid time, the rollback is its own third change " +
			"at its own instant rather than a deletion of the second (FR-017), and a query as of 14:30 " +
			"reports the new revision serving.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceJSON(
				"uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureStart,
				twinRevOld, twinRevOld, map[string]int{twinRevOld: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(0),
				twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41"),
				twinRevisionJSON(twinRevNew, fixtureCreated, "42")),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// Cycle 1 reads the audit page for the forward shift at 14:20.
			auditPayloadAt(cycleAt(1), twinAuditPair(
				"op-shift-1", fixtureShifted.Add(-30*time.Second), fixtureShifted)),
			pollPayloadAt(cycleAt(1), "complete", ""),
			// Cycle 2 notices the new split, dated 14:20.
			servicesPayloadAt(t, cycleAt(2), twinServiceJSON(
				"uid-service-1", 42, fixtureStart.Add(-720*time.Hour), fixtureShifted,
				twinRevNew, twinRevNew, map[string]int{twinRevNew: 100}, labels)),
			pollPayloadAt(cycleAt(2), "complete", ""),
			// Cycle 3 reads the audit page for the rollback at 14:41. Who rolled back is *not* in a
			// sanitised entry — see twinAuditPair — so what distinguishes the two changes here is
			// their instants and their splits, which is what this fixture is for.
			auditPayloadAt(cycleAt(3), twinAuditPair(
				"op-shift-2", fixtureRolledBack.Add(-20*time.Second), fixtureRolledBack)),
			pollPayloadAt(cycleAt(3), "complete", ""),
			// Cycle 4 notices the split is back, dated 14:41.
			servicesPayloadAt(t, cycleAt(4), twinServiceJSON(
				"uid-service-1", 43, fixtureStart.Add(-720*time.Hour), fixtureRolledBack,
				twinRevOld, twinRevNew, map[string]int{twinRevOld: 100}, labels)),
			pollPayloadAt(cycleAt(4), "complete", ""),
		},
		queries: `queries:
  # As of 14:30 the new revision is serving. This is the query that fails if the rollback were
  # recorded as a correction of the forward shift rather than as its own change.
  - name: storefront-asof-1430
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
  # As of the end of the clock the old revision is serving again.
  - name: storefront-asof-end
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
  # Both shifts in one window: two changes, not one.
  - name: storefront-diff-over-both-shifts
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureShifted.Add(-time.Minute)) + `
    t2: ` + rfc3339(fixtureRolledBack.Add(time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}

// T066a — the recreated name: not one continuous entity.
func serviceRecreatedFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	recreatedAt := cycleAt(2)
	return fixtureSpec{
		dir:    "fixtures/gcp-service-recreated-01",
		family: "gcp-topology",
		description: "A service deleted and recreated under the same name, recognised by the change in " +
			"`Service.uid`. The old entity is retracted and the new one created: they are **not** one " +
			"continuous entity (FR-025), and the evidence linking them is emitted as claims for the " +
			"resolution layer rather than merged by the feeder. The new service serves a different " +
			"revision, so an implementation that carried the split comparison across the recreation " +
			"would emit a traffic shift that never happened.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceJSON(
				"uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureStart,
				twinRevOld, twinRevOld, map[string]int{twinRevOld: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// Cycle 3 sees a different uid under the same name.
			servicesPayloadAt(t, cycleAt(3), twinServiceJSON(
				"uid-service-2", 1, recreatedAt, recreatedAt,
				twinRevNew, twinRevNew, map[string]int{twinRevNew: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(3), twinRevisionJSON(twinRevNew, recreatedAt, "42")),
			pollPayloadAt(cycleAt(3), "complete", ""),
		},
		queries: `queries:
  # Before the recreation: the first service, serving the old revision. Asked an hour before the
  # first poll, inside the first uid's life — it is retracted at its last sighting, the first poll.
  - name: storefront-asof-before-recreation
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(fixtureStart.Add(-time.Hour)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The hole: after the first uid's last sighting and before the second uid's createTime. Nothing
  # the platform stated covers it, so nothing is asserted there (contracts/gcp-feeder.md §3.4).
  - name: storefront-in-the-gap
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(cycleAt(1)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
    expect_empty: "between the first uid's last sighting and the second uid's creation nobody stated what existed under this name, so nothing is asserted; an implementation that carried the first service across the recreation would answer here"
  # After: the second service. The retraction of the first is what makes these two answers
  # different rather than one entity whose configuration moved.
  - name: storefront-asof-after-recreation
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}

// T066b — the partial poll: zero retractions and a declared gap.
func partialPollFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	return fixtureSpec{
		dir:    "fixtures/gcp-partial-poll-01",
		family: "gcp-topology",
		description: "Two services observed by a complete poll, then a poll that fails part-way: the " +
			"second poll sees only one of them. It emits **zero retractions**, declares the gap in its " +
			"checkpoint, and does not count toward the silence rule's consecutive-poll total (FR-012, " +
			"§8.1). This is the single rule that keeps a network blip from looking like the " +
			"organisation deleting half its estate.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0),
				twinServiceJSON("uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureStart,
					twinRevOld, twinRevOld, map[string]int{twinRevOld: 100}, labels),
			),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// The next three polls read nothing and say why. Nothing is retracted, and none of them
			// counts toward the silence rule's total.
			pollPayloadAt(cycleAt(1), "partial",
				"the "+twinRegion+" services.list call timed out after 2 pages"),
			pollPayloadAt(cycleAt(2), "partial",
				"the "+twinRegion+" services.list call timed out after 1 page"),
			pollPayloadAt(cycleAt(3), "partial",
				"the "+twinRegion+" services.list call returned 503"),
		},
		queries: `queries:
  # The service is still there after three partial polls. Three complete polls would have retracted
  # it; three partial ones are not evidence of absence.
  - name: storefront-asof-end
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The extent declares the gap, which is what distinguishes "nothing happened" from "nobody was
  # watching" (FR-026).
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// T110 — the refusal path (SC-013, FR-024, contracts/telemetry-backend.md §11).
//
// A pointer says WHERE to look; it never carries what was found (constitution IV). The event log
// enforces that at the boundary: a property holding a measurement — a latency sample, a request
// count, a series — is REJECTED with the published reason code `telemetry_payload`, recorded in
// `log.rejected_events`, and the graph is left exactly as it was.
//
// The refused event cannot come from the feeder, and that is the whole reason this fixture is
// hand-authored on top of a recorded stream. The GCP feeder never emits a telemetry payload — it
// mints Monitoring filters and Logging queries, which are addresses — so a fixture built only from
// what the feeder produces could not tell an enforced rule from an unexercised one. This one
// submits the event the feeder would never send and asserts that the log refuses it.
//
// What it asserts is negative and it is the point: after the refusal the service node is still
// there, still carries its pointers, and carries **no part of a digest anywhere** — no sample, no
// count, no series, not even a summary statistic. A graph that quietly absorbed the numbers would
// still answer every query correctly, and the violation would only be visible in a byte the next
// reviewer did not read.
func telemetryRejectionFixture(t *testing.T) fixtureSpec {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	serviceRef := twinProject + "/" + twinRegion + "/" + twinService

	// Three shapes, one rule, and each reaches it by a different mechanism — which is the point
	// of having three. A fixture carrying only the first would pass a validator that matched on
	// one key name and nothing else.
	//
	//  1. an array of latency samples: caught by SHAPE. A list of two or more numbers is a
	//     series whatever it is called, so renaming the property does not get it in;
	//  2. a nested time series: caught by RECURSION into the struct, where each point's `value`
	//     is a published denied key. Burying a payload one level down is the obvious next
	//     attempt after the first refusal;
	//  3. a single metric value nested under a denied key: caught by the DENIED KEY list
	//     directly. It is the sharpest contrast with the rule's limit below — the same
	//     measurement, `902.5`, is REFUSED under `metric_value` and ACCEPTED as a bare
	//     `request_count`, because the check keys on the name and the shape and has nothing else
	//     to key on.
	//
	// An earlier version used an exemplar trace id here, which is the violation a GCP feeder is
	// most likely to reach for in good faith. The repository's own secrets guard refused the
	// fixture, and it was right to: a committed recording must contain no raw trace payload
	// anywhere, including inside an event the graph refuses, because the file is greppable
	// forever. Shortening the value to slip under the guard's pattern would have been working
	// around the tool rather than with it.
	samples := rejectedEventJSON(t, "gcp:twin:telemetry-samples-1", serviceRef, map[string]any{
		"service.name":                   twinService,
		"sre.gcp.request_latency_p99_ms": []float64{182.4, 194.1, 233.7, 411.2, 902.5},
	})
	series := rejectedEventJSON(t, "gcp:twin:telemetry-series-1", serviceRef, map[string]any{
		"service.name": twinService,
		"sre.gcp.timeseries": map[string]any{
			"metric": "run.googleapis.com/request_count",
			"points": []any{
				map[string]any{"at": "2026-09-21T14:20:00Z", "value": 1204},
				map[string]any{"at": "2026-09-21T14:21:00Z", "value": 1311},
			},
		},
	})
	denied := rejectedEventJSON(t, "gcp:twin:telemetry-denied-key-1", serviceRef, map[string]any{
		"service.name":   twinService,
		"sre.gcp.sample": map[string]any{"metric_value": 902.5},
	})

	return fixtureSpec{
		dir:    "fixtures/gcp-telemetry-rejection-01",
		family: "gcp-refusal",
		description: "The refusal path: three events carrying telemetry in a node property — an " +
			"array of latency samples, a time series nested one level down, and a single metric " +
			"value under a denied key — are each REJECTED with the published reason code " +
			"`telemetry_payload`, and the service node they address is left carrying its pointers " +
			"and no part of a digest anywhere. A pointer says where to look and never what was " +
			"found (constitution IV); this fixture is what makes that an enforced rule rather than " +
			"a convention, because the feeder under test would never emit one of these and a " +
			"fixture built only from its own output could not tell the two apart. It also records " +
			"the rule's LIMIT, which a fixture that only showed successes would hide: a lone " +
			"scalar such as `request_count: 48213` is NOT refused, because the check keys on " +
			"shape and on the published denied-key list, and one number is indistinguishable " +
			"from a replica count or a generation. That boundary is a stated property of the " +
			"rule rather than an oversight, and it belongs where a reader meets it.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceJSON(
				"uid-service-1", 41, fixtureStart.Add(-720*time.Hour), fixtureStart,
				twinRevOld, twinRevOld, map[string]int{twinRevOld: 100}, labels)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
			pollPayloadAt(cycleAt(0), "complete", ""),
		},
		rejected: []string{samples, series, denied},
		expectRejected: []record.Rejection{
			{EventID: "gcp:twin:telemetry-samples-1", ReasonCode: "telemetry_payload"},
			{EventID: "gcp:twin:telemetry-denied-key-1", ReasonCode: "telemetry_payload"},
			{EventID: "gcp:twin:telemetry-series-1", ReasonCode: "telemetry_payload"},
		},
		queries: `queries:
  # The graph after the three refusals. The golden is the assertion: the service is here with its
  # pointers, and no property of it or of anything within two hops holds a measurement.
  - name: storefront-2hop-after-the-refusals
    kind: subgraph
    focus: gcp.cloudrun.service=` + serviceRef + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}

// rejectedEventJSON renders one hand-authored event addressed at the service the recorded stream
// created. It targets the SAME node on purpose: a refusal that landed on a node nobody else
// describes would prove only that an orphan was dropped, where this proves that an existing node
// was not amended.
func rejectedEventJSON(t *testing.T, eventID, serviceRef string, props map[string]any) string {
	t.Helper()
	event := map[string]any{
		"eventId":          eventID,
		"idempotencyKey":   eventID,
		"schemaVersion":    "1.0.0",
		"sourceId":         "gcp:twin",
		"sourceObservedAt": rfc3339(cycleAt(1)),
		"upsertNode": map[string]any{
			"displayName": twinService,
			"props":       props,
			"ref":         map[string]any{"namespace": "gcp.cloudrun.service", "value": serviceRef},
			"type":        "SERVICE",
			"validAt":     rfc3339(fixtureStart),
		},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode %s: %v", eventID, err)
	}
	return string(encoded)
}

// The alert instants. The task names an incident opening 02:07 and closing 02:51; these are the same
// 44-minute incident placed inside the fixture's own clock hour, so that observed time follows valid
// time by seconds rather than by half a day. A twelve-hour lag would have been a valid bitemporal
// statement and a misleading fixture: every `sampled` marker beside it would read as though the
// connector had been asleep.
var (
	alertOpened = fixtureStart.Add(7 * time.Minute)  // 14:07
	alertClosed = fixtureStart.Add(51 * time.Minute) // 14:51
)

// The twin's policies. The identifiers are the numeric form GCP assigns, and neither is derived from
// the organisation (§7).
const (
	twinPolicyUngrouped = "1122334455"
	twinPolicyGrouped   = "5566778899"
)

// T125 — one incident, both transports, exactly one transition in effect (SC-004).
//
// The fixture delivers the same incident **three times**: the poll that first saw it open, a second
// poll while it was still open — which is what a doorbell-triggered poll looks like, and what a poll
// on its own cadence looks like while the monitor is still alerting — and a third after it closed.
// Six deliveries' worth of state, and the graph ends with exactly two transitions: the open at 14:07
// and the recovery at 14:51.
//
// That is the property the published 4-tuple buys, and it is asserted three ways at once by
// `fixture verify`: the golden pins the state history, the double-delivery step re-delivers every
// event and requires DUPLICATE_NOOP, and the shuffle permutes the arrival order and requires the
// same valid-time state. Order independence is the half that is easy to believe without checking.
func alertTransitionFixture(t *testing.T) fixtureSpec {
	t.Helper()
	policy := twinPolicyJSON(twinPolicyUngrouped, "storefront 5xx above 1%", "ERROR", nil)
	return fixtureSpec{
		dir:              "fixtures/gcp-alert-transition-01",
		family:           "gcp-alerts",
		incidentsEnabled: true,
		description: "An alerting incident that opens at 14:07 and closes at 14:51, delivered three " +
			"times: the poll that first saw it, a second poll while it was still open — which is " +
			"both what a doorbell-triggered poll looks like and what the next scheduled poll looks " +
			"like — and a third after it closed. The graph ends with exactly two transitions at " +
			"Google's own instants, because the idempotency key is the published 4-tuple " +
			"(source, policy, group, transition instant) rather than anything per-delivery. The " +
			"recovery is KEPT: it bounds the outage and is what tells an investigation whether the " +
			"change it is looking at was the fix. The re-read while still open is suppressed as " +
			"`no_transition` and the suppression is stated in the checkpoint rather than applied " +
			"silently.",
		payloads: []feeder.Payload{
			alertPoliciesPayloadAt(t, cycleAt(0), policy),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// Cycle 1: the incident is open.
			alertsPayloadAt(t, cycleAt(1), twinIncidentJSON(
				"inc-0001", twinPolicyUngrouped, "OPEN", alertOpened, time.Time{}, "")),
			pollPayloadAt(cycleAt(1), "complete", ""),
			// Cycle 2: still open. One fact, read twice.
			alertsPayloadAt(t, cycleAt(2), twinIncidentJSON(
				"inc-0001", twinPolicyUngrouped, "OPEN", alertOpened, time.Time{}, "")),
			pollPayloadAt(cycleAt(2), "complete", ""),
			// Cycle 3: closed. The opening half is suppressed; the recovery is emitted.
			alertsPayloadAt(t, cycleAt(3), twinIncidentJSON(
				"inc-0001", twinPolicyUngrouped, "CLOSED", alertOpened, alertClosed, "")),
			pollPayloadAt(cycleAt(3), "complete", ""),
		},
		queries: `queries:
  # The alert and its state history. Pinned, so the golden does not encode the day it was recorded.
  - name: alert-2hop
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The state while the incident was open. This is the query that fails if the recovery is dated at
  # the poll rather than at Google's closeTime: at 14:30 the alert must still be alerting.
  - name: alert-asof-1430
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(fixtureStart.Add(30*time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
  # And after the recovery. The pair of as-of reads IS the state history: alerting at 14:30,
  # recovered at 14:55, both from one recorded stream. A node_history read would have said it in one
  # answer, and this build's fixture recorder does not support that kind — so the assertion is made
  # with the kinds it does: two instants either side of Google's closeTime.
  - name: alert-asof-1455
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(fixtureStart.Add(55*time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
`,
	}
}

// T126a — N alerting groups, exactly N alerts, zero policy-level alerts (FR-049, SC-005).
func groupedAlertFixture(t *testing.T) fixtureSpec {
	t.Helper()
	policy := twinPolicyJSON(twinPolicyGrouped, "storefront 5xx by revision", "ERROR",
		[]string{"resource.labels.revision_name"})
	return fixtureSpec{
		dir:              "fixtures/gcp-grouped-alert-01",
		family:           "gcp-alerts",
		incidentsEnabled: true,
		description: "A policy grouped by revision, with both revisions alerting: two alerting " +
			"groups produce exactly two alerts, each with its own state history naming its policy, " +
			"and the policy-level entity is never reported as alerting because a group is (SC-005). " +
			"The old revision recovers and the new one does not, which is the shape that makes " +
			"\"the new revision is failing and the old one is not\" a fact the graph states rather " +
			"than a comparison a reader has to make. The alerting entity is the GROUP because the " +
			"projector writes an alert's state onto the node its monitor ref names: two groups " +
			"sharing one ref would mean the last poll wins, and an investigation asking whether " +
			"the alert is firing would get whichever group was read last.",
		payloads: []feeder.Payload{
			alertPoliciesPayloadAt(t, cycleAt(0), policy),
			pollPayloadAt(cycleAt(0), "complete", ""),
			// Both revisions open.
			alertsPayloadAt(t, cycleAt(1),
				twinIncidentJSON("inc-old", twinPolicyGrouped, "OPEN", alertOpened, time.Time{}, twinRevOld),
				twinIncidentJSON("inc-new", twinPolicyGrouped, "OPEN", alertOpened.Add(time.Minute), time.Time{}, twinRevNew)),
			pollPayloadAt(cycleAt(1), "complete", ""),
			// The old revision's group recovers; the new one's does not.
			alertsPayloadAt(t, cycleAt(2),
				twinIncidentJSON("inc-old", twinPolicyGrouped, "CLOSED", alertOpened, alertClosed, twinRevOld),
				twinIncidentJSON("inc-new", twinPolicyGrouped, "OPEN", alertOpened.Add(time.Minute), time.Time{}, twinRevNew)),
			pollPayloadAt(cycleAt(2), "complete", ""),
		},
		queries: `queries:
  # Both groups, and the policy. Three ALERT nodes: the definition and one per group.
  - name: grouped-alert-2hop
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyGrouped + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The old revision's group, which recovered, and the new one's, which did not. Two focused reads
  # rather than one history read, because those are the kinds this build's fixture recorder
  # supports. They make the fixture's own point: the two groups have separate states.
  - name: grouped-alert-old-revision
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyGrouped + `#resource.labels.revision_name=` + twinRevOld + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
  - name: grouped-alert-new-revision
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyGrouped + `#resource.labels.revision_name=` + twinRevNew + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
`,
	}
}

// T126b — a forged doorbell changes nothing (FR-050).
//
// A doorbell is not a payload, so the fixture models what a forged one actually *causes*: an extra
// poll, earlier than the schedule. The assertion is that the extra poll's events are every one of
// them a DUPLICATE_NOOP and the graph does not move — which `fixture verify`'s double-delivery step
// checks over the whole stream, and which this fixture also states in its own payloads by polling
// twice inside one cycle gap.
//
// What a forged notification cannot do is asserted where it is enforceable: in
// `doorbell_test.go`, by the shape of `Doorbell.Ring`, which has nowhere to receive a body.
func forgedDoorbellFixture(t *testing.T) fixtureSpec {
	t.Helper()
	policy := twinPolicyJSON(twinPolicyUngrouped, "storefront 5xx above 1%", "ERROR", nil)
	incident := twinIncidentJSON("inc-0001", twinPolicyUngrouped, "OPEN", alertOpened, time.Time{}, "")
	return fixtureSpec{
		dir:              "fixtures/gcp-forged-doorbell-01",
		family:           "gcp-alerts",
		incidentsEnabled: true,
		description: "What a forged doorbell costs: one extra poll, and nothing else. The stream " +
			"polls the same open incident twice — the scheduled poll, and the early poll a forged " +
			"or replayed notification would trigger — and the graph ends with exactly one " +
			"transition and one state. The doorbell's body reaches nothing, which is enforced by " +
			"`Doorbell.Ring` having nowhere to receive it rather than by a rule this fixture could " +
			"only observe indirectly; what the fixture proves is the other half, that an extra poll " +
			"creates, alters and retracts nothing. Polling is the source of truth, and this corpus " +
			"runs with the doorbell absent.",
		payloads: []feeder.Payload{
			alertPoliciesPayloadAt(t, cycleAt(0), policy),
			pollPayloadAt(cycleAt(0), "complete", ""),
			alertsPayloadAt(t, cycleAt(1), incident),
			pollPayloadAt(cycleAt(1), "complete", ""),
			// The forged doorbell's early poll: the same truth, read again.
			alertsPayloadAt(t, cycleAt(2), incident),
			pollPayloadAt(cycleAt(2), "complete", ""),
		},
		queries: `queries:
  # One alert, one state — after two polls of the same incident. The instant is inside the open
  # interval, so a second transition invented by the extra poll would show here as a different
  # state or a split version.
  - name: forged-doorbell-asof-1430
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(fixtureStart.Add(30*time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
  - name: forged-doorbell-2hop
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}

// T127 — the capability flag off, and the degradation shipped tested rather than assumed (T117).
func alertTransitionsUnavailableFixture(t *testing.T) fixtureSpec {
	t.Helper()
	policy := twinPolicyJSON(twinPolicyUngrouped, "storefront 5xx above 1%", "ERROR", nil)
	return fixtureSpec{
		dir:              "fixtures/gcp-alert-transitions-unavailable-01",
		family:           "gcp-alerts",
		incidentsEnabled: false,
		description: "The incident capability OFF, which is the default. `projects.alerts` is " +
			"Public Preview and its only Go binding is a maintenance-mode client, so reading it is " +
			"opted into deliberately — and this fixture is what makes the degradation a tested path " +
			"rather than an assumed one. The ALERT node still exists, with its display name, its " +
			"severity, its conditions and its pointers, because `alertPolicies.list` is GA and " +
			"unaffected. What is absent is the state history: no transition, no `sre.alert.state`, " +
			"and nothing that reads as \"this alert has not fired\". The backend's half of the same " +
			"answer — monitor_state returning NO_DATA with coverage naming the absent source — is " +
			"asserted in internal/backends/gcp, because a digest is not a graph fact and has no " +
			"golden here.",
		payloads: []feeder.Payload{
			alertPoliciesPayloadAt(t, cycleAt(0), policy),
			pollPayloadAt(cycleAt(0), "complete", ""),
		},
		queries: `queries:
  # The alert exists and is not alerting: a definition with no state history.
  - name: unavailable-alert-2hop
    kind: subgraph
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The condition pointers are there: the GA policy read is unaffected by the capability flag, so
  # an investigation still knows where to look even though nothing can tell it whether the alert
  # fired.
  - name: unavailable-alert-pointers
    kind: pointers
    focus: gcp.monitoring.alert_policy=projects/` + twinProject + `/alertPolicies/` + twinPolicyUngrouped + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// --- US5 (T139, T140) -----------------------------------------------------------------------------

// sqlFlagChangedAt is when the flag change happened, and it is the **audit entry's** instant rather
// than a poll's: the poll at 14:00 saw `max_connections=200`, the poll at 14:30 saw 500, and only the
// audit stream says it took effect at 14:12 and who did it (contracts/gcp-feeder.md §4, §5.2).
var sqlFlagChangedAt = fixtureStart.Add(12 * time.Minute)

// T139a — the flag change, the derived dependency, and the configuration version.
//
// This is the fixture the Cloud SQL half of US5 lives on. Two polls, and everything it asserts falls
// out of the difference between them plus one audit entry:
//
//   - the instance is an INFRA_RESOURCE with its connection name claimed, which is what FR-120's
//     certain rule resolves on;
//   - a flag changed, and the change is dated at the audit entry's instant with the caller's
//     classification — never at the poll;
//   - the maintenance window changed too, and produced **no** change node: it is a recurring policy
//     (T133);
//   - the service attaches the instance, so a `depends-on` edge is asserted with the evidence that
//     derived it — and no proposal, because the dependency is derivable;
//   - the secret moved from version 4 to version 5, and what the graph holds is the two **version
//     references** and no secret material (T137).
func cloudSQLFlagChangeFixture(t *testing.T) fixtureSpec {
	t.Helper()
	before := twinSQLInstanceJSON(twinInstance,
		map[string]string{"max_connections": "200"}, "db-custom-4-16384", "")
	after := twinSQLInstanceJSON(twinInstance,
		map[string]string{"max_connections": "500", "log_min_duration_statement": "250"},
		"db-custom-8-32768", "")
	return fixtureSpec{
		dir:    "fixtures/gcp-cloudsql-flag-change-01",
		family: "gcp-cloudsql",
		description: "A Cloud SQL instance whose `max_connections` flag changes between two polls, " +
			"dated at the audit entry's instant, alongside a secret moving version and a Cloud Run " +
			"service that attaches the instance. Exercises the instance connection name as a claim, " +
			"the flag catalogue annotating a change that requires a restart, a past maintenance read " +
			"from the operation history with the provider as its actor and a backup operation read " +
			"past, the maintenance window staying a property and never becoming a change node, the " +
			"derived depends-on edge carrying the evidence that derived it, and a configuration " +
			"version holding fingerprints and secret version references rather than values. The " +
			"twin's audit entries carry no principal, because a sanitised entry has no such field at " +
			"all (FR-135), so the flag change's actor is unclassified here and the actor ladder is " +
			"asserted from memory in this package's tests rather than faked in a recording.",
		payloads: []feeder.Payload{
			// Cycle 0: the baseline. The catalogue arrives first because it annotates what follows,
			// and on its own it produces no events at all.
			sqlFlagsPayloadAt(t, cycleAt(0),
				twinSQLFlagJSON("max_connections", true),
				twinSQLFlagJSON("log_min_duration_statement", false)),
			sqlInstancesPayloadAt(t, cycleAt(0), before),
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "4",
				[]string{twinConnectionName(twinInstance)},
				map[string]string{"LOG_LEVEL": "info"})),
			// The revision is the immutable snapshot of what was deployed, so the configuration —
			// including the Cloud SQL attachment the depends-on edge is derived from — is on it.
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "4",
				[]string{twinConnectionName(twinInstance)},
				map[string]string{"LOG_LEVEL": "info"})),
			pollPayloadAt(cycleAt(0), "complete", ""),

			// Cycle 1: the audit entry that dates the flag edit, then the poll that shows it — and a
			// new revision carrying the rotated secret, which is what a configuration change IS on
			// Cloud Run: a new immutable revision. No traffic moves to it, so there is no shift.
			auditPayloadAt(cycleAt(1), twinSQLAuditPageJSON(twinInstance, sqlFlagChangedAt)),
			sqlInstancesPayloadAt(t, cycleAt(1), after),
			// The instance's operation history, filtered to the two types the contract names. It
			// carries a maintenance that happened and a backup that did not happen for this
			// connector's purposes: the backup is read past, which is what says the filter is applied
			// client-side as well as server-side. The maintenance's actor is the PROVIDER — GCP
			// performed it — and the change states that it is **not** correlated with any
			// announcement, because GCP publishes no identifier linking the two.
			sqlOperationsPayloadAt(t, cycleAt(1),
				twinSQLOperationJSON("op-maintenance-1", "MAINTENANCE", twinInstance,
					fixtureStart.Add(-6*time.Hour), fixtureStart.Add(-6*time.Hour+18*time.Minute)),
				twinSQLOperationJSON("op-backup-1", "BACKUP_VOLUME", twinInstance,
					fixtureStart.Add(-3*time.Hour), fixtureStart.Add(-3*time.Hour+4*time.Minute))),
			servicesPayloadAt(t, cycleAt(1), twinServiceConfigJSON("uid-service-1", 42,
				map[string]int{twinRevOld: 100}, "5",
				[]string{twinConnectionName(twinInstance)},
				map[string]string{"LOG_LEVEL": "debug"})),
			revisionsPayloadAt(t, cycleAt(1), twinRevisionConfigJSON(twinRevNew,
				fixtureCreated, "42", "5",
				[]string{twinConnectionName(twinInstance)},
				map[string]string{"LOG_LEVEL": "debug"})),
			pollPayloadAt(cycleAt(1), "complete", ""),
		},
		queries: `queries:
  # The instance and what changed about it. Pinned, so the golden does not encode the day it was
  # recorded (fixture-format.md).
  - name: instance-2hop
    kind: subgraph
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinInstance + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The service side of the derived dependency: the edge must be there, and it must carry the
  # evidence that derived it (FR-028).
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # What changed in the window the edit falls in, which is what "the second question after what
  # changed" is asked with.
  - name: instance-diff
    kind: diff
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinInstance + `
    t1: ` + rfc3339(fixtureStart) + `
    t2: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The pointers the instance carries, which FR-079 is asserted against.
  - name: instance-pointers
    kind: pointers
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinInstance + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The extent, which states the scope in force: "not present" and "not in scope" are different
  # answers (FR-009).
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// T139b — the instance no configuration names: **no invented edge**, and a pending suggestion.
//
// The whole fixture is one negative and one positive. The service defines `BILLING_LEDGER_DSN`, which
// resembles the instance `billing-ledger` closely enough to be worth a person's attention; nothing in
// the configuration carries the instance's connection name, so the dependency cannot be derived. The
// graph must therefore hold **no** `depends-on` edge between the two — the subgraph golden is where
// that is asserted, because an invented edge would appear in it — and the event stream must hold one
// `propose_dependency` with its evidence, its score and its rule.
//
// It also holds the GKE cluster, for the same reason: the cluster is a node, and nothing below it is
// (FR-030). A node pool appearing in this golden would be the failure that rule exists to prevent.
func proposedDependencyFixture(t *testing.T) fixtureSpec {
	t.Helper()
	return fixtureSpec{
		dir:    "fixtures/gcp-proposed-dependency-01",
		family: "gcp-cloudsql",
		description: "A Cloud SQL instance that nothing in any service's configuration names, and a " +
			"service whose environment-variable name resembles it. No depends-on edge is asserted — " +
			"the subgraph golden is where that absence is checked — and one proposed dependency is " +
			"raised carrying its evidence, its score and its rule, once per cycle. Also holds the GKE " +
			"cluster as an INFRA_RESOURCE with the bare name the Kubernetes connector uses, and " +
			"nothing below it: the payload carries a node pool and the graph must not.",
		payloads: []feeder.Payload{
			sqlInstancesPayloadAt(t, cycleAt(0),
				twinSQLInstanceJSON(twinLedger, nil, "db-custom-2-8192", "")),
			gkeClustersPayloadAt(t, cycleAt(0), twinGKEClusterJSON(twinCluster, twinRegion)),
			// The service names no connection name anywhere: the variable holding the DSN is sourced
			// from Secret Manager, which is exactly the case FR-029 exists for — the dependency is
			// real and the evidence for it is a name.
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "4", nil,
				map[string]string{"BILLING_LEDGER_DSN_FROM": "secret-manager"})),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "4", nil,
				map[string]string{"BILLING_LEDGER_DSN_FROM": "secret-manager"})),
			pollPayloadAt(cycleAt(0), "complete", ""),

			// A second cycle, and it is not padding: a proposal is refused if either endpoint is
			// unknown, so it is emitted at the poll marker AFTER the cycle that asserted them — by
			// which time they are in the graph whatever order this cycle's events arrive in. The
			// proposal in this fixture therefore lands here, and the shuffle is what says so
			// (internal/feeders/gcp/apply_us5.go, emitPendingProposals).
			sqlInstancesPayloadAt(t, cycleAt(1),
				twinSQLInstanceJSON(twinLedger, nil, "db-custom-2-8192", "")),
			servicesPayloadAt(t, cycleAt(1), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "4", nil,
				map[string]string{"BILLING_LEDGER_DSN_FROM": "secret-manager"})),
			revisionsPayloadAt(t, cycleAt(1), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "4", nil,
				map[string]string{"BILLING_LEDGER_DSN_FROM": "secret-manager"})),
			pollPayloadAt(cycleAt(1), "complete", ""),
		},
		queries: `queries:
  # The service's neighbourhood. The instance must NOT be in it: a proposal is never an edge, and no
  # query traverses one (FR-029).
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The instance's neighbourhood, from the other side, for the same reason.
  - name: ledger-2hop
    kind: subgraph
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinLedger + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The cluster, and nothing below it (FR-030).
  - name: cluster-2hop
    kind: subgraph
    focus: gcp.gke.cluster=` + twinProject + `/` + twinRegion + `/` + twinCluster + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// The announced window, and the instants the maintenance twin is read at.
//
// They mirror vendor-maintenance-future-01 exactly, because T140 asks for the same query behaviour:
// read once while the window is still ahead, and once during it. The point of asserting the same
// behaviour from a *platform* feeder is that the announced-fact semantics are the data model's and not
// the vendor-notice connector's — the same state machine, the same ranking exclusion, a different
// source.
var (
	sqlMaintenanceAnnounced = time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	sqlMaintenanceDeadline  = time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	sqlMaintenanceReadAt    = fixtureStart.Add(2 * time.Hour)
	sqlMaintenanceInWindow  = time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
)

// T140 — the announced maintenance: valid time leads observed time by eleven days.
//
// One poll, read on 21 September, announcing a window on 2 October. Everything it asserts is about the
// asymmetry: the valid interval is the announced window emitted unchanged (FR-062), observed time is
// not moved forward to match it (FR-063), and a read as of a valid instant inside the window and an
// observed instant at or after the poll returns the change — an ordinary bitemporal query rather than a
// special case (FR-064).
func cloudSQLScheduledMaintenanceFixture(t *testing.T) fixtureSpec {
	t.Helper()
	scheduled := fmt.Sprintf(`{"startTime": %q, "canReschedule": true, "canDefer": true, `+
		`"scheduleDeadlineTime": %q}`, rfc3339(sqlMaintenanceAnnounced), rfc3339(sqlMaintenanceDeadline))
	return fixtureSpec{
		dir:    "fixtures/gcp-cloudsql-scheduled-maintenance-01",
		family: "gcp-cloudsql",
		start:  fixtureStart,
		// The clock runs to after the announced window, so the fixture's own extent covers the
		// instant the second query reads at. It mirrors vendor-maintenance-future-01's span.
		end: sqlMaintenanceAnnounced.Add(3 * time.Hour),
		description: "A Cloud SQL instance read on 21 September announcing a maintenance window on 2 " +
			"October: valid time leads observed time by eleven days, which is the same announced-fact " +
			"shape as vendor-maintenance-future-01 from a platform feeder instead of a notice reader. " +
			"Exercises the announced window emitted unchanged, observed time never moved forward to " +
			"match it, the VENDOR actor kind, and the announcement state defaulting to announced and " +
			"staying there while the window is ahead.",
		payloads: []feeder.Payload{
			sqlInstancesPayloadAt(t, sqlMaintenanceReadAt,
				twinSQLInstanceJSON(twinInstance, map[string]string{"max_connections": "200"},
					"db-custom-4-16384", scheduled)),
			pollPayloadAt(sqlMaintenanceReadAt, "complete", ""),
		},
		queries: `queries:
  # The instance and its announced change, as known on the day it was read. Pinned, so the golden
  # does not encode the day it was recorded (fixture-format.md).
  - name: instance-2hop-as-read
    kind: subgraph
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinInstance + `
    valid_at: ` + rfc3339(sqlMaintenanceInWindow) + `
    observed_at: ` + rfc3339(sqlMaintenanceReadAt.Add(time.Hour)) + `
    hops: 2
    direction: both
  # The same graph read during the announced window, eleven days after the poll. The change is valid
  # here and was observed eleven days earlier: an ordinary bitemporal read, which is FR-064's whole
  # claim.
  - name: instance-2hop-in-window
    kind: subgraph
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinInstance + `
    valid_at: ` + rfc3339(sqlMaintenanceInWindow) + `
    observed_at: ` + rfc3339(sqlMaintenanceInWindow) + `
    hops: 2
    direction: both
  # And read BEFORE the window, which must not return it as something that has happened: a change
  # whose valid start is after the reference instant is not a candidate cause (FR-065).
  - name: instance-2hop-before-window
    kind: subgraph
    focus: gcp.cloudsql.instance=` + twinProject + `/` + twinRegion + `/` + twinInstance + `
    valid_at: ` + rfc3339(sqlMaintenanceReadAt.Add(time.Hour)) + `
    observed_at: ` + rfc3339(sqlMaintenanceReadAt.Add(time.Hour)) + `
    hops: 2
    direction: both
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(sqlMaintenanceReadAt.Add(time.Hour)) + `
`,
	}
}

// --- US6 (T151, T152) -----------------------------------------------------------------------------

// T151, T148 — the taxonomy and its fallback, in one fixture.
//
// Five entries, chosen to be FR-038's minimum coverage plus the case FR-039 is about:
//
//	SetIamPolicy                 → IAM_CHANGE      (the taxonomy names it, so "other" would be a regression)
//	UpdateQuotaOverride          → QUOTA_CHANGE    (likewise)
//	serviceusage.services.enable → CONFIG_CHANGE   (a project's configuration changing)
//	AddSecretVersion             → CHANGE_KIND_OTHER, carrying GCP's own operation name
//	DeleteBucket                 → CHANGE_KIND_OTHER, and its resource is in no namespace this
//	                               connector mints in, so the change carries the resource name and
//	                               NO target ref rather than minting a ref another connector owns
//
// Every one of them also carries GCP's operation name, not only the two that fall back to "other":
// FR-039 requires it where the taxonomy has no equivalent, and recording it always is what makes a
// mapped change checkable against the console.
func auditOtherKindFixture(t *testing.T) fixtureSpec {
	t.Helper()
	service := "projects/" + twinProject + "/locations/" + twinRegion + "/services/" + twinService
	return fixtureSpec{
		dir:    "fixtures/gcp-audit-other-kind-01",
		family: "gcp-audit",
		description: "Five admin-activity entries covering FR-038's minimum — an IAM policy change, a " +
			"quota change, a service enablement, a secret version added and a resource deleted — " +
			"mapped to the published taxonomy where one fits and to CHANGE_KIND_OTHER carrying GCP's " +
			"own operation name where none does. The deletion names a resource in no namespace this " +
			"connector mints in, so its change carries the resource name and no target ref rather " +
			"than minting a ref another connector owns. The twin's entries carry no principal, " +
			"because a sanitised entry has no such field at all (FR-135), so every actor here is " +
			"unclassified and the ladder is asserted from memory in this package's tests.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "", nil, nil)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "", nil, nil)),
			pollPayloadAt(cycleAt(0), "complete", ""),

			auditPayloadAt(cycleAt(1), twinAuditGeneralPage(
				twinAuditEntryJSON("iam-1", "", "run.googleapis.com",
					"google.cloud.run.v2.Services.SetIamPolicy", service, fixtureStart.Add(5*time.Minute), 0),
				twinAuditEntryJSON("quota-1", "", "serviceusage.googleapis.com",
					"google.api.serviceusage.v1beta1.ServiceUsage.UpdateQuotaOverride",
					"projects/"+twinProject, fixtureStart.Add(7*time.Minute), 0),
				twinAuditEntryJSON("enable-1", "", "serviceusage.googleapis.com",
					"serviceusage.services.enable", "projects/"+twinProject,
					fixtureStart.Add(9*time.Minute), 0),
				twinAuditEntryJSON("secret-1", "", "secretmanager.googleapis.com",
					"google.cloud.secretmanager.v1.SecretManagerService.AddSecretVersion",
					"projects/"+twinProject+"/secrets/storefront-api-token",
					fixtureStart.Add(11*time.Minute), 0),
				twinAuditEntryJSON("delete-1", "", "storage.googleapis.com",
					"storage.buckets.delete", "projects/_/buckets/"+twinProject+"-assets",
					fixtureStart.Add(13*time.Minute), 0),
			)),
			pollPayloadAt(cycleAt(1), "complete", ""),
		},
		queries: `queries:
  # The service and the IAM change against it. Pinned, so the golden does not encode the day it was
  # recorded (fixture-format.md).
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # Every change in the window, which is where the five kinds are checked — and where a change that
  # was dropped rather than mapped would be visible as an absence.
  - name: storefront-diff
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureStart) + `
    t2: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The extent, which states the audit scope in force: "no change happened" and "we were not looking
  # for that kind of change" are different answers (FR-040).
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// onsetAt and scaledAt are the two instants T152 turns on: a symptom, and the autoscaler reacting.
var (
	onsetAt  = fixtureStart.Add(20 * time.Minute)
	scaledAt = onsetAt.Add(4 * time.Minute)
)

// T152 — the autoscaling adjustment four minutes after onset.
//
// The point is feature 002's causal ordering: a CI principal deploying is a candidate **cause**, and
// an autoscaler reacting four minutes after onset is a candidate **effect**. The kind is what lets the
// ranker exonerate it, and collapsing both into one "robot" kind removes the distinction (ADR-0005
// D1).
//
// What this fixture can prove is the **timing and the taxonomy**: the change is a SCALING valid at
// onset + 4 minutes, so a diff window that starts at onset finds it after the symptom rather than
// before. What it cannot prove is the actor kind, because CONTROLLER rests on
// `serviceAccountDelegationInfo[].firstPartyPrincipal` — the only documented signal that the caller is
// a Google platform agent (research §2) — and a sanitised entry carries no such field at all. That
// half is asserted from memory, end to end through the feeder, in
// TestAPlatformPrincipalIsClassifiedAsAController. The split is stated rather than worked around: the
// alternative was allow-listing a principal field in `scripts/check-no-secrets.sh` to make a fixture
// of my own pass, and that guard exists to stop exactly that.
func auditControllerEffectFixture(t *testing.T) fixtureSpec {
	t.Helper()
	return fixtureSpec{
		dir:    "fixtures/gcp-audit-controller-effect-01",
		family: "gcp-audit",
		description: "An autoscaling adjustment four minutes after symptom onset, emitted as a SCALING " +
			"change valid at the instant GCP states — so a diff window opening at onset finds it " +
			"after the symptom rather than before it, which is what lets feature 002's causal " +
			"ordering treat it as a candidate effect. The actor kind CONTROLLER is NOT asserted here: " +
			"it rests on serviceAccountDelegationInfo[].firstPartyPrincipal, the only documented " +
			"signal that a caller is a Google platform agent, and a sanitised entry carries no such " +
			"field at all (FR-135) — so that half is asserted from memory in this package's tests.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "", nil, nil)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "", nil, nil)),
			pollPayloadAt(cycleAt(0), "complete", ""),

			auditPayloadAt(cycleAt(1), twinAuditGeneralPage(
				twinAuditEntryJSON("scale-1", "", "compute.googleapis.com",
					"compute.autoscalers.patch",
					"projects/"+twinProject+"/locations/"+twinRegion+"/services/"+twinService,
					scaledAt, 0),
			)),
			pollPayloadAt(cycleAt(1), "complete", ""),
		},
		queries: `queries:
  # The window from onset onwards: the scaling change is inside it, four minutes in.
  - name: storefront-diff-from-onset
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(onsetAt) + `
    t2: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The window that ENDS at onset: the scaling change is not in it, which is the assertion that makes
  # "candidate effect" mean something — a change after the symptom cannot have caused it.
  - name: storefront-diff-before-onset
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureStart) + `
    t2: ` + rfc3339(onsetAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
    expect_empty: "the window ends at onset and the scaling change comes after it, so nothing is in it: a change after the symptom cannot have caused it, and that absence is what makes candidate effect mean something"
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// --- US8 (T165) -----------------------------------------------------------------------------------

// T165 — the run the budget cut short.
//
// The graph-visible half of a quota-exhausted run is the **checkpoint**, and everything this fixture
// asserts is in it:
//
//   - the poll is `partial`, and a partial poll **retracts nothing** (§8.1). The topology the first
//     cycle observed is still there afterwards, which is the property that keeps a quota squeeze from
//     looking like the organisation deleting its estate;
//   - the areas dropped are the published order's first ones — load balancers and DNS, then the general
//     audit stream — and never something a P1 story depends on;
//   - the stop reason is `quota_exhausted`, which is **not** evidence of absence. A poll that ran out
//     of its share and a poll that found nothing are opposite conclusions, and the checkpoint is where
//     a reader tells them apart (FR-149).
//
// The backend half — `QUERY_FAILED / RATE_LIMITED` with the earliest retry instant, and a usage report
// matching the recording to the exact call — is asserted in internal/gcpx and internal/backends/gcp,
// because a budget refusal produces no graph event at all and a fixture that claimed to assert it
// would be asserting nothing.
func quotaExhaustedFixture(t *testing.T) fixtureSpec {
	t.Helper()
	return fixtureSpec{
		dir:    "fixtures/gcp-quota-exhausted-01",
		family: "gcp-budget",
		description: "A complete poll followed by one the call budget cut short: the checkpoint states " +
			"the areas dropped from the published deferral order, the typed stop reason " +
			"quota_exhausted — which is never evidence of absence — and the gap. The partial poll " +
			"retracts nothing, so the topology the first cycle observed is still there afterwards, " +
			"which is what keeps a quota squeeze from looking like the estate being deleted.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "", nil, nil)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "", nil, nil)),
			pollPayloadAt(cycleAt(0), "complete", ""),

			// The squeeze. No service payload at all this cycle: that is what "deferred" means, and
			// the checkpoint is the only place it is visible.
			deferredPollPayloadAt(cycleAt(1),
				"the logging.read share reached its human reserve, so the poll stopped before it had "+
					"covered its extent",
				gcpx.AreaLoadBalancersAndDNS, gcpx.AreaGeneralAuditStream),
		},
		queries: `queries:
  # The extent, which is the whole fixture: the scope, the deferred areas, the stop reason and the gap.
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
  # And the topology, which a partial poll must leave exactly as it was (§8.1). A retraction here
  # would be the failure this rule exists to prevent.
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}

// --- US9 (T170) -----------------------------------------------------------------------------------

// twinLBHost is the host name the load balancer serves. It is a `.test` name, which is reserved and
// resolves nowhere: a twin must not carry a name that could be a real organisation's.
const twinLBHost = "shop.twin.test"

// T170 — the exposure relations and both change kinds.
//
// The chain is walked end to end from stated fields: a forwarding rule targets a proxy, the proxy's URL
// map routes a host to a backend service, the backend service's backend is a serverless NEG, and the
// NEG's `cloudRun.service` names the service. Every hop is somebody's configuration read verbatim, which
// is what makes the `exposed-via` edge an assertion rather than a name match — and the edge carries the
// chain so a reviewer can check it hop by hop.
//
// The DNS switch is dated by the audit entry and not by the poll: the second cycle's record set answers
// a different address, and the `dns.changes.create` entry is what says when. A poll sees a record, not a
// switch, and the DNS poll runs every thirty minutes — long enough for a change dated at the poll to
// make a reader exonerate the wrong thing.
func lbDNSFixture(t *testing.T) fixtureSpec {
	t.Helper()
	const (
		rule    = "shop-https"
		urlMap  = "shop-urlmap"
		backend = "shop-backend"
		neg     = "shop-neg"
	)
	switched := fixtureStart.Add(25 * time.Minute)
	return fixtureSpec{
		dir:    "fixtures/gcp-lb-dns-01",
		family: "gcp-exposure",
		description: "A Cloud Run service exposed through a global HTTPS load balancer, derived hop by " +
			"hop from stated fields — forwarding rule, URL map, backend service, serverless NEG, and " +
			"the NEG's cloudRun.service — with the host names and the chain on the relation rather " +
			"than as nodes. The A record then answers a different address, and the switch is dated at " +
			"the Cloud DNS audit entry's instant and not at the poll. The twin's entries carry no " +
			"principal, because a sanitised entry has no such field at all (FR-135).",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "", nil, nil)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "", nil, nil)),
			computeItemsPayloadAt(t, gcpfeeder.PayloadForwardingRules, cycleAt(0),
				twinForwardingRuleJSON(rule, urlMap)),
			computeItemsPayloadAt(t, gcpfeeder.PayloadURLMaps, cycleAt(0),
				twinURLMapJSON(urlMap, twinLBHost, backend)),
			computeItemsPayloadAt(t, gcpfeeder.PayloadBackendServices, cycleAt(0),
				twinBackendServiceJSON(backend, neg)),
			computeItemsPayloadAt(t, gcpfeeder.PayloadNetworkEndpointGroups, cycleAt(0),
				twinNEGJSON(neg, twinService)),
			twinDNSPayloadAt(t, cycleAt(0), "twin-zone",
				twinDNSRecordJSON(twinLBHost+".", "A", "34.111.0.7")),
			pollPayloadAt(cycleAt(0), "complete", ""),

			// The switch. The record answers a different address, and the audit entry dates it.
			twinDNSPayloadAt(t, cycleAt(1), "twin-zone",
				twinDNSRecordJSON(twinLBHost+".", "A", "34.111.0.9")),
			auditPayloadAt(cycleAt(1), twinDNSAuditPage(switched)),
			pollPayloadAt(cycleAt(1), "complete", ""),
		},
		queries: `queries:
  # The service and what it is exposed through. Pinned, so the golden does not encode the day it was
  # recorded (fixture-format.md).
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The load balancer from the other side, which is where the relation's host names and derivation
  # chain are checked.
  - name: load-balancer-2hop
    kind: subgraph
    focus: gcp.lb=projects/` + twinProject + `/global/forwardingRules/shop-https
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The window the switch falls in: the DNS_SWITCH is valid at the audit entry's instant, not the poll's.
  - name: storefront-diff
    kind: diff
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    t1: ` + rfc3339(fixtureStart) + `
    t2: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
`,
	}
}

// T169, T170 — the run with both surfaces omitted.
//
// The assertion is a **presence**, not an absence: the checkpoint names both surfaces and why, so a
// reader who asks "what is this service exposed through?" and gets nothing can tell "we did not look"
// from "it is not exposed". Those are opposite answers and the second is the confident kind of wrong.
//
// It is both surfaces or neither. Reading the load balancers without DNS would produce host names
// nothing resolves, and DNS without the load balancers would produce records pointing at addresses no
// node carries — either half alone is a graph that looks complete and answers wrongly.
func lbDNSOmittedFixture(t *testing.T) fixtureSpec {
	t.Helper()
	return fixtureSpec{
		dir:    "fixtures/gcp-lb-dns-omitted-01",
		family: "gcp-exposure",
		description: "The same estate with the load balancers and Cloud DNS deliberately not read, " +
			"which FR-057 makes the ordinary case: they are P3 and the first surface the deferral " +
			"order drops. The checkpoint names both surfaces and the reason, so a reader who gets no " +
			"exposure relation can tell \"we did not look\" from \"it is not exposed\" — and every " +
			"other requirement holds unchanged, which is what the topology golden asserts.",
		payloads: []feeder.Payload{
			servicesPayloadAt(t, cycleAt(0), twinServiceConfigJSON("uid-service-1", 41,
				map[string]int{twinRevOld: 100}, "", nil, nil)),
			revisionsPayloadAt(t, cycleAt(0), twinRevisionConfigJSON(twinRevOld,
				fixtureStart.Add(-24*time.Hour), "41", "", nil, nil)),
			pollPayloadAt(cycleAt(0), "complete", ""),
		},
		omittedSurfaces: gcpfeeder.OmitBoth(gcpfeeder.SurfaceOmittedForBudget),
		queries: `queries:
  # The extent, which is the whole fixture: both surfaces named, with the reason.
  - name: extent
    kind: extent
    observed_at: ` + rfc3339(pinnedAt) + `
  # And the topology, unchanged: omitting a P3 surface leaves every other requirement alone (FR-057).
  - name: storefront-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
`,
	}
}
