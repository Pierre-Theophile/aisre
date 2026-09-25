// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// The checked-in September 2026 aggregate (T015, FR-069a, SC-022).
//
// FR-069a says the first audit is "a recorded result rather than a procedure still to be run",
// and SC-022 says it is published before any accuracy gate is set. `docs/evaluation/
// coverage-audit-2026-09.md` is the human half of that; `coverage-audit-2026-09.json` is the
// machine-readable half the ceiling guard, `compare` and the ledger's π₀ all read.
//
// The JSON is transcribed from the Markdown rather than emitted by a run, because the incident
// list it was measured over is private. These tests are what keep the two halves honest: every
// number the Markdown states is asserted against the JSON, so the pair cannot drift, and the
// numbers the JSON deliberately does not carry are asserted absent rather than silently zero.

func publishedAggregate(t *testing.T) *audit.Result {
	t.Helper()
	path := filepath.Join("..", "..", "..", "docs", "evaluation", "coverage-audit-2026-09.json")
	result, err := audit.LoadResult(path)
	if err != nil {
		t.Fatalf("load the published aggregate: %v", err)
	}
	return result
}

func TestPublishedAggregateMatchesItsMarkdown(t *testing.T) {
	t.Parallel()
	result := publishedAggregate(t)

	if result.AuditID != "2026-09" {
		t.Errorf("audit id = %q, want 2026-09", result.AuditID)
	}
	if !closeTo(result.Ceiling, 0.615385) {
		t.Errorf("ceiling = %v, want 0.615385 (about 62 %%)", result.Ceiling)
	}
	if !closeTo(result.Prior, 0.384615) {
		t.Errorf("π₀ = %v, want 0.384615", result.Prior)
	}
	if result.ClassifiableCount != 13 {
		t.Errorf("classifiable count = %d, want 13", result.ClassifiableCount)
	}
	if result.Corpus.ExcludedCount != 3 {
		t.Errorf("excluded = %d, want 3 (two security incidents, one unclassifiable)",
			result.Corpus.ExcludedCount)
	}

	// 0 of 13, 3 of 13, 8 of 13 — the three figures SC-022 names.
	ladder := map[string]int{"feature-001": 0, "+deploy": 3, "+vendor-notice": 8}
	if len(result.FeederSets) != len(ladder) {
		t.Fatalf("feeder sets = %d, want %d", len(result.FeederSets), len(ladder))
	}
	for _, set := range result.FeederSets {
		want, ok := ladder[set.Name]
		if !ok {
			t.Errorf("unexpected feeder set %q", set.Name)
			continue
		}
		if set.Observed != want {
			t.Errorf("%s: observed = %d of %d, want %d of 13", set.Name, set.Observed, set.Classifiable, want)
		}
		if set.Classifiable != 13 {
			t.Errorf("%s: classifiable = %d, want 13", set.Name, set.Classifiable)
		}
		// 15 % symptom-only on every rung, as the published table's middle column reads.
		if set.SymptomOnly != 2 {
			t.Errorf("%s: symptom-only = %d, want 2", set.Name, set.SymptomOnly)
		}
		if !closeTo(set.Ceiling, float64(want)/13) {
			t.Errorf("%s: ceiling = %v, want %v", set.Name, set.Ceiling, float64(want)/13)
		}
		if !closeTo(set.Prior, 1-set.Ceiling) {
			t.Errorf("%s: π₀ = %v, want %v", set.Name, set.Prior, 1-set.Ceiling)
		}
	}

	// And the published Markdown still says the same thing.
	markdownPath := filepath.Join("..", "..", "..", "docs", "evaluation", "coverage-audit-2026-09.md")
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatalf("read the published markdown: %v", err)
	}
	for _, want := range []string{"13 classifiable production incidents", "62 %", "23 %", "15 %"} {
		if !strings.Contains(string(markdown), want) {
			t.Errorf("the published markdown no longer states %q; the JSON beside it now disagrees", want)
		}
	}
}

// TestPublishedAggregateNamesTheRemainderCategories: FR-071b's four categories have to be named
// in the machine-readable aggregate, because that is what makes `audit coverage gaps` able to
// report them as corpus gaps on every evaluation run.
func TestPublishedAggregateNamesTheRemainderCategories(t *testing.T) {
	t.Parallel()
	result := publishedAggregate(t)

	// The latent-bug incident is symptom-visible (its delivery errors show) and so is not in the
	// unobservable remainder; the remainder is the three incidents no feeder set can see at all.
	want := map[audit.Category]audit.CauseClass{
		audit.CategoryClientSideConfiguration: audit.CauseNotChangeInduced,
		audit.CategoryBusinessDataChange:      audit.CauseNotChangeInduced,
		audit.CategoryCredentialLeak:          audit.CauseUnobserved,
	}
	if len(result.Remainder) != len(want) {
		t.Fatalf("remainder categories = %d, want %d: %+v", len(result.Remainder), len(want), result.Remainder)
	}
	for _, row := range result.Remainder {
		class, ok := want[row.Category]
		if !ok {
			t.Errorf("unexpected remainder category %q", row.Category)
			continue
		}
		if len(row.Classes) != 1 || row.Classes[0] != class {
			t.Errorf("%s: classes = %v, want [%s]", row.Category, row.Classes, class)
		}
	}

	// And T102's four fixtures close every one of them. This is FR-071b's corpus obligation
	// measured against the published audit and the shipped corpus at the same time, which is
	// the only pairing that means anything: the remainder is a property of the audit, the
	// fixtures are a property of the repository, and the requirement is that the second covers
	// the first.
	gaps, err := audit.DetectGaps(result, filepath.Join("..", "..", "..", audit.FixtureRoot))
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}
	if !gaps.OK() {
		t.Errorf("corpus gaps against the published audit: %v (%s)", gaps.Gaps, gaps.Warning)
	}
	for _, covered := range gaps.Covered {
		if len(covered.Fixtures) == 0 {
			t.Errorf("%s is reported covered by no fixture", covered.Category)
		}
	}
}

// TestPublishedAggregateIsComputedAndAggregateOnly: the published file is the output of
// `audit coverage --aggregate-only` over the owner's private transcription, so its provenance is
// computed, and it carries no incident-level detail, ever (FR-069a).
func TestPublishedAggregateIsComputedAndAggregateOnly(t *testing.T) {
	t.Parallel()
	result := publishedAggregate(t)

	if result.Provenance != audit.ProvenanceComputed {
		t.Errorf("provenance = %q, want %q", result.Provenance, audit.ProvenanceComputed)
	}
	if len(result.Items) != 0 {
		t.Errorf("the published aggregate carries %d items, want none", len(result.Items))
	}
}

// TestPublishedAggregateBoundsAGate: the point of publishing it in this shape is that the
// ceiling guard can read it (FR-071, T017).
func TestPublishedAggregateBoundsAGate(t *testing.T) {
	t.Parallel()
	result := publishedAggregate(t)

	report, err := audit.Guard(&audit.GuardInput{Criteria: []audit.Criterion{
		{ID: "SC-001", Metric: "pass@1", Target: 0.60, Annotation: "[ceiling-bounded]", Audit: "2026-09"},
		{ID: "SC-006", Metric: "top hypothesis", Target: 0.75, Annotation: "[ceiling-bounded]", Audit: "2026-09"},
	}}, result)
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if report.Passed != 1 || report.Failed != 1 {
		t.Errorf("passed/failed = %d/%d, want 1/1 (0.60 is under 0.615385, 0.75 is over)",
			report.Passed, report.Failed)
	}
}

// TestPriorFromThePublishedAggregate is the number ADR-0005 D9 quotes: π₀ = 0.38 today.
func TestPriorFromThePublishedAggregate(t *testing.T) {
	t.Parallel()

	record := audit.PriorRecordFromAudit(publishedAggregate(t))
	if !closeTo(record.Prior, 0.384615) {
		t.Errorf("π₀ = %v, want 0.384615", record.Prior)
	}
	if record.AuditID != "2026-09" {
		t.Errorf("audit id = %q, want 2026-09", record.AuditID)
	}
	if record.FeederSet != "+vendor-notice" {
		t.Errorf("feeder set = %q, want +vendor-notice", record.FeederSet)
	}
}
