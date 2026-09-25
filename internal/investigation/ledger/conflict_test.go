// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Conflict and non-separability (T056, FR-024, FR-025).

// TestConflictReportsBothSidesAndReflectsThemInTheConfidence is FR-024. Choosing a side without
// stating why is impossible here by construction: both lists are on the report and both ln LRs
// are in the number.
func TestConflictReportsBothSidesAndReflectsThemInTheConfidence(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	for _, id := range []string{"e1", "e2", "e3"} {
		mustAddEvidence(t, l, id)
	}
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)
	mustJudge(t, l, "j2", "h1", "e2", ledger.Refutes, ledger.Moderate)
	mustJudge(t, l, "j3", "h2", "e3", ledger.Supports, ledger.Weak)

	conflicts := l.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("Conflicts() returned %d entries, want 1 (only h1 has evidence both ways)", len(conflicts))
	}
	c := conflicts[0]
	switch {
	case c.HypothesisID != "h1":
		t.Errorf("the conflict is on %s, want h1", c.HypothesisID)
	case len(c.Supporting) != 1 || c.Supporting[0].EvidenceID != "e1":
		t.Errorf("supporting side = %+v, want the judgment on e1", c.Supporting)
	case len(c.Refuting) != 1 || c.Refuting[0].EvidenceID != "e2":
		t.Errorf("refuting side = %+v, want the judgment on e2", c.Refuting)
	}

	// The conflict is in the number: strong support minus moderate refute is a net positive that
	// is strictly smaller than the strong support alone would have produced.
	wantNet := ledger.LnLRStrong - ledger.LnLRModerate
	if c.NetLnLR != wantNet {
		t.Errorf("net ln LR = %v, want %v", c.NetLnLR, wantNet)
	}
	h1, _ := l.Hypothesis("h1")
	if c.Confidence != h1.Confidence || c.Bucket != h1.Bucket {
		t.Errorf("the conflict reports %.6f (%s), the ledger holds %.6f (%s)",
			c.Confidence, c.Bucket, h1.Confidence, h1.Bucket)
	}

	// And it is in the rendering, both sides named, with the evidence ids that back each.
	rendered := l.Render(ledger.RenderContext{})
	for _, want := range []string{"conflicting evidence", "e1 (strong)", "e2 (moderate)"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the turn rendering does not mention %q:\n%s", want, rendered)
		}
	}
}

// TestNonSeparableCandidatesAreReportedTogether is FR-025 and the shape `two-simultaneous-01`
// exists to exercise: two changes the evidence cannot tell apart, reported together, with what
// would separate them named.
func TestNonSeparableCandidatesAreReportedTogether(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	// Identical judgments on both candidates: whatever the priors were, the evidence says the
	// same thing about each, and the only thing separating them is the ranker's prior.
	for _, id := range []string{"e1", "e2"} {
		mustAddEvidence(t, l, id)
	}
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Moderate)
	mustJudge(t, l, "j2", "h2", "e2", ledger.Supports, ledger.Moderate)

	// With priors 0.496 and 0.124 the two candidates are separable: identical evidence leaves
	// the ranker's prior as the thing that tells them apart, and it does.
	//
	// The open hypothesis takes part in the grouping like any other (FR-019a), so a group may
	// well pair h2 with it — "we cannot separate the config change from no observed change at
	// all" is a finding, not an artefact.
	for _, g := range l.NonSeparable() {
		if ids := g.IDs(); slices.Contains(ids, "h1") && slices.Contains(ids, "h2") {
			t.Fatalf("candidates 0.8/0.2 apart were reported as non-separable: %v", ids)
		}
	}

	// Equal ranker scores, equal judgments: nothing separates them.
	even, err := ledger.New("inv-even", l.Prior())
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	for _, id := range []string{"h1", "h2"} {
		if err := even.AddHypothesis(ledger.Hypothesis{
			ID: id, Kind: ledger.KindChange, Statement: "candidate " + id,
		}, 1); err != nil {
			t.Fatalf("add hypothesis: %v", err)
		}
	}
	if err := even.SetNextQuery("h1", nextQuery(), "https://example.invalid/separate-h1"); err != nil {
		t.Fatalf("set next query: %v", err)
	}

	groups := even.NonSeparable()
	if len(groups) != 1 {
		t.Fatalf("NonSeparable() returned %d groups, want 1: %+v", len(groups), groups)
	}
	g := groups[0]
	if ids := g.IDs(); !slices.Contains(ids, "h1") || !slices.Contains(ids, "h2") {
		t.Errorf("group = %v, want both candidates", ids)
	}
	if g.Spread > ledger.SeparationEpsilon {
		t.Errorf("group spread %.6f is wider than the published epsilon %v", g.Spread, ledger.SeparationEpsilon)
	}
	if !g.Separable() {
		t.Error("a group with a recorded next query reports nothing would separate it")
	}

	rendered := even.Render(ledger.RenderContext{})
	for _, want := range []string{"not separable", "what would separate h1", "https://example.invalid/separate-h1"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the turn rendering does not mention %q:\n%s", want, rendered)
		}
	}
}

// TestNonSeparableWithNothingToSeparateThemSaysSo is the honest end of an investigation: two
// candidates at the same confidence and no query anywhere that would tell them apart.
func TestNonSeparableWithNothingToSeparateThemSaysSo(t *testing.T) {
	t.Parallel()

	l, err := ledger.New("inv-stuck", pi38())
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	for _, id := range []string{"h1", "h2"} {
		if err := l.AddHypothesis(ledger.Hypothesis{
			ID: id, Kind: ledger.KindChange, Statement: "candidate " + id,
		}, 1); err != nil {
			t.Fatalf("add hypothesis: %v", err)
		}
	}

	groups := l.NonSeparable()
	if len(groups) != 1 || groups[0].Separable() {
		t.Fatalf("want one group with nothing that would separate it, got %+v", groups)
	}
	if !strings.Contains(l.Render(ledger.RenderContext{}), "none recorded") {
		t.Error("the rendering does not say that nothing recorded would separate the group")
	}
}

// TestRefutedAndExoneratedCandidatesLeaveTheNonSeparableSet: they are not candidates any more, so
// grouping them with a live one would report a choice nobody is being asked to make.
func TestRefutedAndExoneratedCandidatesLeaveTheNonSeparableSet(t *testing.T) {
	t.Parallel()

	l, err := ledger.New("inv-refuted", pi38())
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	for _, id := range []string{"h1", "h2"} {
		if err := l.AddHypothesis(ledger.Hypothesis{
			ID: id, Kind: ledger.KindChange, Statement: "candidate " + id,
		}, 1); err != nil {
			t.Fatalf("add hypothesis: %v", err)
		}
	}
	if err := l.SetStatus("h2", ledger.StatusRefuted, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("refute h2: %v", err)
	}
	if groups := l.NonSeparable(); len(groups) != 0 {
		t.Errorf("a refuted candidate is still being grouped as non-separable: %+v", groups)
	}
}

// pi38 is a π₀ record shaped like the published first audit's: a measured ceiling of about 62%
// (ADR-0004, research §8), rounded to 0.38 so the worked numbers stay readable.
func pi38() audit.PriorRecord {
	return audit.PriorRecord{
		Prior: 0.38, AuditID: "audit-2026-09", Ceiling: 0.62,
		FeederSet: "deploy+vendor", IncidentCount: 13,
	}
}
