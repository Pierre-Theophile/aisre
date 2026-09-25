// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// compareFixtures runs both synthetic audits and compares them, older first.
func compareFixtures(t *testing.T, opts audit.Options) *audit.Comparison {
	t.Helper()
	before := runFixture(t, "synthetic-01", opts)
	after := runFixture(t, "synthetic-02", opts)
	comparison, err := audit.Compare(before, after)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return comparison
}

// TestCompareAttributesTheMovementToTheFeedersAdded is FR-071a's main clause: both runs
// readable, the ceiling comparable, the movement attributed to the feeders that were added.
func TestCompareAttributesTheMovementToTheFeedersAdded(t *testing.T) {
	t.Parallel()
	comparison := compareFixtures(t, audit.Options{})

	if !comparison.Comparable {
		t.Errorf("the two runs are reported as not comparable: %v", comparison.Caveats)
	}
	if !comparison.Moved {
		t.Fatal("the ceiling is reported as unmoved, but it went 8/13 → 9/13")
	}
	if !closeTo(comparison.CeilingDelta, 0.076923) {
		t.Errorf("ceiling delta = %v, want 0.076923", comparison.CeilingDelta)
	}
	if !closeTo(comparison.PriorDelta, -0.076923) {
		t.Errorf("π₀ delta = %v, want -0.076923", comparison.PriorDelta)
	}
	if !closeTo(comparison.After.Ceiling, 0.692308) {
		t.Errorf("after ceiling = %v, want 0.692308 (9 of 13)", comparison.After.Ceiling)
	}
	if !closeTo(comparison.After.Prior, 0.307692) {
		t.Errorf("after π₀ = %v, want 0.307692", comparison.After.Prior)
	}

	if got, want := comparison.Added, []string{"+edge-cdn"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("added feeder sets = %v, want %v", got, want)
	}
	for _, feeder := range []string{"cdn.config_changes", "edge.routing"} {
		if !contains(comparison.AddedFeeders, feeder) {
			t.Errorf("added feeders = %v, want it to include %q", comparison.AddedFeeders, feeder)
		}
	}
	// A re-run that adds a rung necessarily changes the input file; that is a note, not a
	// reason to refuse the comparison FR-071a asks for.
	if len(comparison.Notes) == 0 {
		t.Error("the changed input digest was not noted")
	}
}

// TestCompareReportsAFeederSetThatDidNotMove is the clause that shapes the whole file: "a
// ceiling that did not move MUST be reported as such rather than omitted" (FR-071a).
func TestCompareReportsAFeederSetThatDidNotMove(t *testing.T) {
	t.Parallel()
	comparison := compareFixtures(t, audit.Options{})

	want := map[string]struct {
		moved bool
		note  string
	}{
		"feature-001":    {false, "did not move"},
		"+deploy":        {false, "did not move"},
		"+vendor-notice": {false, "did not move"},
		"+edge-cdn":      {false, "new feeder set"},
	}
	if len(comparison.FeederSets) != len(want) {
		t.Fatalf("feeder set rows = %d, want %d — every rung must be reported, moved or not",
			len(comparison.FeederSets), len(want))
	}
	for _, movement := range comparison.FeederSets {
		expected, ok := want[movement.Name]
		if !ok {
			t.Errorf("unexpected feeder set row %q", movement.Name)
			continue
		}
		if movement.Moved != expected.moved {
			t.Errorf("%s: moved = %v, want %v", movement.Name, movement.Moved, expected.moved)
		}
		if movement.Note != expected.note {
			t.Errorf("%s: note = %q, want %q", movement.Name, movement.Note, expected.note)
		}
	}
}

// TestCompareOfIdenticalRunsSaysTheCeilingDidNotMove: the sentence has to be produced, not
// inferred from an empty diff.
func TestCompareOfIdenticalRunsSaysTheCeilingDidNotMove(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	comparison, err := audit.Compare(result, result)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comparison.Moved {
		t.Error("comparing a run with itself reports a movement")
	}
	if !strings.Contains(comparison.Summary, "did not move") {
		t.Errorf("summary = %q, want it to say the ceiling did not move", comparison.Summary)
	}
	if !strings.Contains(comparison.Summary, "no feeders were added") {
		t.Errorf("summary = %q, want it to say no feeders were added", comparison.Summary)
	}
	if len(comparison.Incidents) != 0 {
		t.Errorf("incident movements = %d, want none", len(comparison.Incidents))
	}
	for _, movement := range comparison.FeederSets {
		if movement.Note != "did not move" {
			t.Errorf("%s: note = %q, want %q", movement.Name, movement.Note, "did not move")
		}
	}
}

// TestCompareNamesTheIncidentThatMoved: attribution is per incident when both runs carry items.
func TestCompareNamesTheIncidentThatMoved(t *testing.T) {
	t.Parallel()
	comparison := compareFixtures(t, audit.Options{})

	if len(comparison.Incidents) != 1 {
		t.Fatalf("incident movements = %d, want exactly 1: %+v",
			len(comparison.Incidents), comparison.Incidents)
	}
	moved := comparison.Incidents[0]
	if moved.IncidentRef != "SYN-09" {
		t.Errorf("moved incident = %q, want SYN-09", moved.IncidentRef)
	}
	if moved.VerdictBefore != audit.VerdictSymptomOnly || moved.VerdictAfter != audit.VerdictObserved {
		t.Errorf("SYN-09 moved %s → %s, want symptom_only → observed",
			moved.VerdictBefore, moved.VerdictAfter)
	}
	if moved.Category != audit.CategoryTrafficShift {
		t.Errorf("SYN-09 category = %q, want traffic_shift", moved.Category)
	}
	if len(moved.AttributedTo) == 0 {
		t.Error("the movement is attributed to no feeders")
	}
}

// TestCompareOfAggregatesStillWorks: a published audit carries no items, and FR-071a's clauses
// are all aggregate-level, so the comparison must not require them.
func TestCompareOfAggregatesStillWorks(t *testing.T) {
	t.Parallel()
	comparison := compareFixtures(t, audit.Options{AggregateOnly: true})

	if !comparison.Comparable {
		t.Errorf("aggregates reported as not comparable: %v", comparison.Caveats)
	}
	if !closeTo(comparison.CeilingDelta, 0.076923) {
		t.Errorf("ceiling delta = %v, want 0.076923", comparison.CeilingDelta)
	}
	if len(comparison.Incidents) != 0 {
		t.Errorf("incident movements = %d, want none without items", len(comparison.Incidents))
	}
	if len(comparison.FeederSets) != 4 {
		t.Errorf("feeder set rows = %d, want 4", len(comparison.FeederSets))
	}
}

// TestCompareRefusesToCallDifferentCorporaComparable: two ceilings can both be true and not be
// comparable, and saying so is more useful than a silent subtraction.
func TestCompareRefusesToCallDifferentCorporaComparable(t *testing.T) {
	t.Parallel()

	before := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true})
	after := runFixture(t, "synthetic-02", audit.Options{AggregateOnly: true})
	after.Corpus.Label = "a-different-organisation"

	comparison, err := audit.Compare(before, after)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comparison.Comparable {
		t.Error("two different corpora are reported as comparable")
	}
	if len(comparison.Caveats) == 0 {
		t.Fatal("no caveat explains why they are not comparable")
	}
	if !strings.Contains(comparison.Summary, "NOT directly comparable") {
		t.Errorf("summary = %q, want it to carry the caveat", comparison.Summary)
	}
	// Both ceilings are still printed: refusing to print is not more honest than printing
	// with a caveat.
	if !closeTo(comparison.Before.Ceiling, wantCeiling) || !closeTo(comparison.After.Ceiling, 0.692308) {
		t.Error("an incomparable comparison dropped one of the ceilings")
	}
}

func TestCompareDetectsADifferentDenominator(t *testing.T) {
	t.Parallel()

	before := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true})
	after := runFixture(t, "synthetic-02", audit.Options{AggregateOnly: true})
	after.ClassifiableCount = 20

	comparison, err := audit.Compare(before, after)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comparison.Comparable {
		t.Error("ceilings over different denominators are reported as comparable")
	}
	if !strings.Contains(strings.Join(comparison.Caveats, " "), "different denominators") {
		t.Errorf("caveats = %v, want one about the denominators", comparison.Caveats)
	}
}

func TestCompareMarkdownCarriesBothRungsAndTheMovement(t *testing.T) {
	t.Parallel()
	markdown := compareFixtures(t, audit.Options{}).Markdown()

	for _, want := range []string{
		"synthetic-01", "synthetic-02", "did not move", "new feeder set", "SYN-09", "+edge-cdn",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("comparison markdown is missing %q\n---\n%s", want, markdown)
		}
	}
}

func TestCompareRequiresBothRuns(t *testing.T) {
	t.Parallel()

	if _, err := audit.Compare(nil, runFixture(t, "synthetic-01", audit.Options{})); err == nil {
		t.Error("Compare accepted a missing first run")
	}
	if _, err := audit.Compare(runFixture(t, "synthetic-01", audit.Options{}), nil); err == nil {
		t.Error("Compare accepted a missing second run")
	}
}

func TestParseResultRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"not an audit", `{"hello": "world"}`},
		{"no audit id", `{"feeder_sets": [{"name": "base"}]}`},
		{"no feeder sets", `{"audit_id": "x"}`},
		{"unknown field", `{"audit_id": "x", "feeder_sets": [{"name": "base"}], "surprise": 1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := audit.ParseResult([]byte(test.raw)); err == nil {
				t.Error("ParseResult accepted input it should refuse")
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
