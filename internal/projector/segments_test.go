// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// The order-independence property, driven hard (FR-021, research §5).
//
// This is the same property the fixture shuffle and the database-backed permutation test check,
// but at the level where it is actually decided: the segment planner. Nothing here touches
// Postgres, so rapid can throw hundreds of assertion sets at it per second and shrink any
// counter-example down to the two or three events that break it.
//
// The claim under test: for any set of assertions, applying them in any order yields the same
// valid-time partition — the same intervals carrying the same per-source assertions.

// sameSources is content equality for the planner tests: "the same source asserted it", which
// is what a materialized comparison reduces to when every assertion from a source carries
// distinct values (the generators give every event its own value).
func sameSources(a, b segment) bool {
	if len(a.assertions) != len(b.assertions) {
		return false
	}
	for source, event := range a.assertions {
		if b.assertions[source] != event {
			return false
		}
	}
	return true
}

type testAssertion struct {
	sourceID string
	eventID  string
	at       time.Time
	fromUnk  bool
}

func planAll(assertions []testAssertion, order []int) []segment {
	assertedAt := func(eventID string) time.Time {
		for _, a := range assertions {
			if a.eventID == eventID {
				return a.at
			}
		}
		return time.Time{}
	}
	var segments []segment
	for _, index := range order {
		a := assertions[index]
		segments = planUpsert(segments, a.sourceID, a.eventID, a.at, a.fromUnk, assertedAt, sameSources)
	}
	return segments
}

// render describes a partition the way a query would see it: intervals and who asserted what.
func render(segments []segment) string {
	var out strings.Builder
	for _, seg := range segments {
		end := "∞"
		if !seg.end.IsZero() {
			end = seg.end.Format(time.RFC3339)
		}
		pairs := make([]string, 0, len(seg.assertions))
		for source, event := range seg.assertions {
			pairs = append(pairs, source+"="+event)
		}
		slices.Sort(pairs)
		fmt.Fprintf(&out, "[%s,%s) unknown=%v %s\n",
			seg.start.Format(time.RFC3339), end, seg.fromUnknown, strings.Join(pairs, ","))
	}
	return out.String()
}

func TestPlanUpsertIsOrderIndependent(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

	rapid.Check(t, func(rt *rapid.T) {
		count := rapid.IntRange(2, 7).Draw(rt, "assertions")
		seen := map[string]bool{}
		var assertions []testAssertion
		for i := range count {
			source := fmt.Sprintf("src:%d", rapid.IntRange(0, 2).Draw(rt, fmt.Sprintf("source%d", i)))
			at := base.Add(time.Duration(rapid.IntRange(0, 5).Draw(rt, fmt.Sprintf("valid%d", i))) * time.Hour)
			// One source may not assert twice at one instant: that is a correction, decided in
			// observed time, and the later delivery is meant to win.
			key := source + at.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			assertions = append(assertions, testAssertion{
				sourceID: source,
				eventID:  fmt.Sprintf("e%d", i),
				at:       at,
				fromUnk:  false,
			})
		}
		if len(assertions) < 2 {
			rt.Skip("need at least two distinct assertions")
		}

		inOrder := make([]int, len(assertions))
		for i := range inOrder {
			inOrder[i] = i
		}
		order := rapid.Permutation(inOrder).Draw(rt, "order")

		want := render(planAll(assertions, inOrder))
		got := render(planAll(assertions, order))
		if want != got {
			rt.Fatalf("partition depends on arrival order\nassertions: %+v\nin order %v:\n%sorder %v:\n%s",
				assertions, inOrder, want, order, got)
		}
	})
}

// TestPlanUpsertPartitionsAreDisjoint pins the invariant the exclusion constraint enforces in
// the database: at any observed instant an entity's current versions have pairwise
// non-overlapping valid ranges (data-model.md).
func TestPlanUpsertPartitionsAreDisjoint(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

	rapid.Check(t, func(rt *rapid.T) {
		count := rapid.IntRange(1, 8).Draw(rt, "assertions")
		var assertions []testAssertion
		seen := map[string]bool{}
		for i := range count {
			source := fmt.Sprintf("src:%d", rapid.IntRange(0, 2).Draw(rt, fmt.Sprintf("source%d", i)))
			at := base.Add(time.Duration(rapid.IntRange(0, 5).Draw(rt, fmt.Sprintf("valid%d", i))) * time.Hour)
			key := source + at.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			assertions = append(assertions, testAssertion{
				sourceID: source, eventID: fmt.Sprintf("e%d", i), at: at,
			})
		}
		order := make([]int, len(assertions))
		for i := range order {
			order[i] = i
		}

		segments := planAll(assertions, order)
		for i := 1; i < len(segments); i++ {
			prev, cur := segments[i-1], segments[i]
			if prev.end.IsZero() {
				rt.Fatalf("segment %d runs to infinity but is followed by another:\n%s", i-1, render(segments))
			}
			if cur.start.Before(prev.end) {
				rt.Fatalf("segments %d and %d overlap:\n%s", i-1, i, render(segments))
			}
		}
		for _, seg := range segments {
			if !seg.end.IsZero() && !seg.start.Before(seg.end) {
				rt.Fatalf("empty segment %s:\n%s", seg.valid(), render(segments))
			}
		}
	})
}

// TestPlanRetractRespectsLaterAssertions checks the resurrection rule: a retraction does not
// remove a version built from an assertion whose own valid time is after the retraction end.
func TestPlanRetractRespectsLaterAssertions(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	assertions := []testAssertion{
		{sourceID: "src:0", eventID: "early", at: base},
		{sourceID: "src:0", eventID: "late", at: base.Add(2 * time.Hour)},
	}
	segments := planAll(assertions, []int{0, 1})
	assertedAt := func(eventID string) time.Time {
		for _, a := range assertions {
			if a.eventID == eventID {
				return a.at
			}
		}
		return time.Time{}
	}

	retracted := planRetract(segments, "retraction", base.Add(time.Hour), "", assertedAt, sameSources)
	if len(retracted) != 2 {
		t.Fatalf("segments after retraction = %d, want 2:\n%s", len(retracted), render(retracted))
	}
	if got := retracted[0].end; !got.Equal(base.Add(time.Hour)) {
		t.Errorf("first segment ends at %s, want the retraction instant %s", got, base.Add(time.Hour))
	}
	if !retracted[1].start.Equal(base.Add(2*time.Hour)) || !retracted[1].end.IsZero() {
		t.Errorf("the later assertion was not kept:\n%s", render(retracted))
	}
}

// ---------- retractions in the mix ----------

// testFact is one event in a mixed upsert/retract sequence: an assertion by a source, or a
// retraction, at a valid instant.
type testFact struct {
	sourceID string
	eventID  string
	at       time.Time
	retract  bool
}

// planFacts folds a delivery order into a partition, using the same planners the projector does.
func planFacts(facts []testFact, order []int) []segment {
	assertedAt := func(eventID string) time.Time {
		for _, f := range facts {
			if f.eventID == eventID {
				return f.at
			}
		}
		return time.Time{}
	}
	var segments []segment
	for _, index := range order {
		f := facts[index]
		if f.retract {
			segments = planRetract(segments, f.eventID, f.at, "", assertedAt, sameSources)
			continue
		}
		segments = planUpsert(segments, f.sourceID, f.eventID, f.at, false, assertedAt, sameSources)
	}
	return segments
}

// legalDeliveryOrders is the constraint of retract_edge_order_test.go, at the planner level: a
// retraction may be delivered in any position after every assertion whose valid instant it ends.
// A retraction that arrives before the fact it cuts leaves nothing behind and is lost, which is
// the one asymmetry the graph does not claim to survive.
func legalDeliveryOrder(facts []testFact, rt *rapid.T) []int {
	placed := make([]int, 0, len(facts))
	remaining := make([]int, len(facts))
	for i := range remaining {
		remaining[i] = i
	}
	ready := func(i int) bool {
		if !facts[i].retract {
			return true
		}
		for j, f := range facts {
			if f.retract || f.at.After(facts[i].at) {
				continue
			}
			if !slices.Contains(placed, j) {
				return false
			}
		}
		return true
	}
	for step := 0; len(remaining) > 0; step++ {
		var choices []int
		for _, i := range remaining {
			if ready(i) {
				choices = append(choices, i)
			}
		}
		pick := choices[rapid.IntRange(0, len(choices)-1).Draw(rt, fmt.Sprintf("pick%d", step))]
		placed = append(placed, pick)
		remaining = slices.DeleteFunc(remaining, func(i int) bool { return i == pick })
	}
	return placed
}

// TestPlanRetractIsOrderIndependent is FR-021 with retractions in the stream.
//
// Delivering the same assertions and retractions in any legal order must produce the same
// valid-time partition. This is the property the projector could not previously make — a
// retraction delivered before a later re-assertion of the same subject truncated the segment the
// re-assertion had coalesced into — and it is why a coalesced segment remembers the first instant
// each source restated it (segments.go).
func TestPlanRetractIsOrderIndependent(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

	rapid.Check(t, func(rt *rapid.T) {
		count := rapid.IntRange(2, 7).Draw(rt, "facts")
		seen := map[string]bool{}
		var facts []testFact
		for i := range count {
			retract := rapid.Bool().Draw(rt, fmt.Sprintf("retract%d", i))
			source := fmt.Sprintf("src:%d", rapid.IntRange(0, 1).Draw(rt, fmt.Sprintf("source%d", i)))
			at := base.Add(time.Duration(rapid.IntRange(0, 5).Draw(rt, fmt.Sprintf("valid%d", i))) * time.Hour)
			// One source may not assert twice at one instant, and two retractions at one
			// instant are one retraction: both are corrections decided in observed time.
			key := fmt.Sprintf("%v|%s|%s", retract, source, at)
			if retract {
				key = "retract|" + at.String()
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			facts = append(facts, testFact{
				sourceID: source, eventID: fmt.Sprintf("e%d", i), at: at, retract: retract,
			})
		}
		if len(facts) < 2 {
			rt.Skip("need at least two distinct facts")
		}

		// The reference is valid-time order, which is always legal: every retraction follows
		// every assertion it ends.
		reference := make([]int, len(facts))
		for i := range reference {
			reference[i] = i
		}
		slices.SortStableFunc(reference, func(a, b int) int { return facts[a].at.Compare(facts[b].at) })

		want := render(planFacts(facts, reference))
		got := render(planFacts(facts, legalDeliveryOrder(facts, rt)))
		if want != got {
			rt.Fatalf("partition depends on arrival order\nfacts: %+v\nin valid-time order:\n%sdelivered:\n%s",
				facts, want, got)
		}
	})
}
