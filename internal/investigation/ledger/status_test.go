// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The status machine and the write-time rules (T055, FR-020, FR-022, FR-031, FR-051).

// nextQuery is a legal algebra term to hand an untested hypothesis: the compare the engine would
// run if it had the budget.
func nextQuery() *investigationv1.AlgebraTerm {
	return &investigationv1.AlgebraTerm{
		Term: &investigationv1.AlgebraTerm_Compare{
			Compare: &investigationv1.CompareTerm{Statistic: investigationv1.Statistic_ERROR_RATE},
		},
	}
}

// TestSupportedNeedsAQualifyingJudgment is the `unsupported_status` rule (FR-022): a hypothesis is
// supported by a source-of-truth worker's evidence or it is not supported at all.
func TestSupportedNeedsAQualifyingJudgment(t *testing.T) {
	t.Parallel()

	t.Run("no judgment at all", func(t *testing.T) {
		t.Parallel()
		l := openLedger(t)
		assertRejected(t, l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}),
			ledger.ReasonUnsupportedStatus)
	})

	t.Run("a supporting judgment from a worker that is not a source of truth", func(t *testing.T) {
		t.Parallel()
		l := openLedger(t)
		e := evidenceItem("e-knowledge")
		e.Kind = "knowledge_item"
		e.SourceOfTruth = ""
		if err := l.AddEvidence(e); err != nil {
			t.Fatalf("add evidence: %v", err)
		}
		mustJudge(t, l, "j1", "h1", "e-knowledge", ledger.Supports, ledger.Moderate)
		assertRejected(t, l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}),
			ledger.ReasonUnsupportedStatus)
	})

	t.Run("a refuting judgment does not qualify either", func(t *testing.T) {
		t.Parallel()
		l := openLedger(t)
		mustAddEvidence(t, l, "e1")
		mustJudge(t, l, "j1", "h1", "e1", ledger.Refutes, ledger.Strong)
		assertRejected(t, l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}),
			ledger.ReasonUnsupportedStatus)
	})

	t.Run("a supporting judgment from a source-of-truth worker qualifies", func(t *testing.T) {
		t.Parallel()
		l := openLedger(t)
		mustAddEvidence(t, l, "e1")
		mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)
		if err := l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
			t.Fatalf("set supported: %v", err)
		}
		h, _ := l.Hypothesis("h1")
		if h.Status != ledger.StatusSupported {
			t.Errorf("status = %s, want %s", h.Status, ledger.StatusSupported)
		}
	})
}

// TestKnowledgeOnlyHypothesisMayNotBeSupported is FR-051: a runbook saying this happened last time
// is a reason to look, not a finding.
func TestKnowledgeOnlyHypothesisMayNotBeSupported(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)
	if err := l.MarkKnowledge("h1", true, false); err != nil {
		t.Fatalf("mark knowledge: %v", err)
	}
	assertRejected(t, l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}),
		ledger.ReasonUnsupportedStatus)

	// Confirmed by a source-of-truth worker, it may.
	if err := l.MarkKnowledge("h1", true, true); err != nil {
		t.Fatalf("mark knowledge confirmed: %v", err)
	}
	if err := l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("set supported after confirmation: %v", err)
	}
}

// TestUntestedCarriesAReasonAndANextQuery is FR-031: never scored as refuted, never dropped, and
// it says what would test it.
func TestUntestedCarriesAReasonAndANextQuery(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	before, _ := l.Hypothesis("h1")

	if err := l.SetStatus("h1", ledger.StatusUntested, ledger.StatusUpdate{}); err == nil {
		t.Fatal("untested was accepted with no reason and no next query")
	}
	if err := l.SetStatus("h1", ledger.StatusUntested, ledger.StatusUpdate{
		Reason: "no metric pointer of the required kind on checkout",
	}); err == nil {
		t.Fatal("untested was accepted with a reason but no next query")
	}

	err := l.SetStatus("h1", ledger.StatusUntested, ledger.StatusUpdate{
		Reason:            "no metric pointer of the required kind on checkout",
		NextQuery:         nextQuery(),
		NextQueryDeepLink: "https://example.invalid/compare",
	})
	if err != nil {
		t.Fatalf("set untested: %v", err)
	}

	after, _ := l.Hypothesis("h1")
	// Not scored as refuted: no judgment was recorded, so the confidence is exactly what it was.
	if after.Confidence != before.Confidence {
		t.Errorf("untested moved the confidence from %.6f to %.6f; it is never scored as refuted",
			before.Confidence, after.Confidence)
	}
	if after.UntestedReason == "" || after.NextQuery == nil || after.NextQueryDeepLink == "" {
		t.Errorf("untested hypothesis lost its reason or its next query: %+v", after)
	}
	// Not dropped: it is still in the ledger, still ranked.
	if after.Rank == 0 {
		t.Error("an untested hypothesis lost its rank")
	}
}

// TestExonerationIsFirstClassEvidence is FR-029c and FR-057c: an exoneration is a judgment, a
// re-role and a status — never a silent drop.
func TestExonerationIsFirstClassEvidence(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	onset := evidenceItem("e-onset")
	onset.Kind = "onset_estimate"
	onset.Worker = "onset"
	onset.Capability = "onset"
	if err := l.AddEvidence(onset); err != nil {
		t.Fatalf("add onset evidence: %v", err)
	}
	before, _ := l.Hypothesis("h1")

	judgment, err := l.Exonerate("h1", ledger.Exoneration{
		JudgmentID:      "j-exo",
		OnsetEvidenceID: "e-onset",
		Reason:          "the rollout starts 11 minutes after onset, well past the onset's ±2 min uncertainty",
	})
	if err != nil {
		t.Fatalf("exonerate: %v", err)
	}

	if judgment.Direction != ledger.Refutes || judgment.Strength != ledger.Decisive {
		t.Errorf("the exoneration judgment is %s/%s, want %s/%s",
			judgment.Direction, judgment.Strength, ledger.Refutes, ledger.Decisive)
	}
	if judgment.Source != ledger.SourceExoneration {
		t.Errorf("the exoneration judgment's source is %s, want %s", judgment.Source, ledger.SourceExoneration)
	}

	after, _ := l.Hypothesis("h1")
	switch {
	case after.Status != ledger.StatusExonerated:
		t.Errorf("status = %s, want %s", after.Status, ledger.StatusExonerated)
	case after.CausalRole != ledger.RoleCandidateEffect:
		t.Errorf("causal role = %s, want %s", after.CausalRole, ledger.RoleCandidateEffect)
	case after.OnsetEvidenceID != "e-onset":
		t.Errorf("the exoneration does not carry the onset evidence id: %+v", after)
	case after.Confidence >= before.Confidence:
		t.Errorf("exonerating h1 left it at %.6f (was %.6f); the judgment must move the number",
			after.Confidence, before.Confidence)
	}
	assertLedgerSumsToOne(t, l)

	// The evidence has to be an onset estimate: an exoneration is an argument from the clock.
	if _, err := l.Exonerate("h2", ledger.Exoneration{
		JudgmentID: "j-exo2", OnsetEvidenceID: "e-onset", Reason: "x",
	}); err != nil {
		t.Fatalf("exonerate h2: %v", err)
	}
	mustAddEvidence(t, l, "e-not-onset")
	if _, err := l.Exonerate("h1", ledger.Exoneration{
		JudgmentID: "j-exo3", OnsetEvidenceID: "e-not-onset", Reason: "x",
	}); err == nil {
		t.Error("an exoneration resting on something other than an onset estimate was accepted")
	}
}

// TestStatusNeverReturnsToProposed is the one transition the machine forbids outright.
func TestStatusNeverReturnsToProposed(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	if err := l.SetStatus("h1", ledger.StatusInconclusive, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("set inconclusive: %v", err)
	}
	if err := l.SetStatus("h1", ledger.StatusProposed, ledger.StatusUpdate{}); err == nil {
		t.Error("a hypothesis was moved back to proposed; what was learned stays learned")
	}
	// Everything else may move: evidence arriving late is the normal case.
	if err := l.SetStatus("h1", ledger.StatusRefuted, ledger.StatusUpdate{}); err != nil {
		t.Errorf("inconclusive → refuted was refused: %v", err)
	}
	if err := l.SetStatus("h1", "believed", ledger.StatusUpdate{}); err == nil {
		t.Error("a status outside the published vocabulary was accepted")
	}
}

// TestAnExonerationRationaleSurvivesLaterEvidence is the regression Phase 7 Track D found.
//
// The reason an exoneration rests on — "this change starts after the onset, outside its
// uncertainty" — used to be appended to the hypothesis's rationale string at the moment it was
// recorded. Every later mutation of the ledger calls recompute, recompute rebuilds every
// rationale from the rows, and the appended sentence disappeared. On a quiet investigation the
// exoneration was the last thing that happened and nobody noticed; on a real one it is followed
// by more evidence, and the exonerated hypothesis then reported its status with no argument for
// it. An exoneration with no stated reason is a verdict without the case.
func TestAnExonerationRationaleSurvivesLaterEvidence(t *testing.T) {
	t.Parallel()

	const reason = "the scaling starts 5 minutes after the estimated onset, " +
		"outside the onset's ±90 s uncertainty"

	l := openLedger(t)
	onset := evidenceItem("e-onset")
	onset.Kind = "onset_estimate"
	onset.Worker = "onset"
	onset.Capability = "onset"
	if err := l.AddEvidence(onset); err != nil {
		t.Fatalf("add onset evidence: %v", err)
	}
	if _, err := l.Exonerate("h2", ledger.Exoneration{
		JudgmentID:      "j-exo",
		OnsetEvidenceID: "e-onset",
		Reason:          reason,
	}); err != nil {
		t.Fatalf("exonerate: %v", err)
	}

	exonerated, _ := l.Hypothesis("h2")
	if !strings.Contains(exonerated.Rationale, reason) {
		t.Fatalf("the exoneration's reason is not in the rationale straight away:\n%s", exonerated.Rationale)
	}

	// Anything else landing in the ledger recomputes every rationale. Before the fix this is
	// where the reason was lost.
	mustAddEvidence(t, l, "e-later")
	mustJudge(t, l, "j-later", "h1", "e-later", ledger.Supports, ledger.Strong)

	exonerated, _ = l.Hypothesis("h2")
	if !strings.Contains(exonerated.Rationale, reason) {
		t.Errorf("a later judgment elsewhere in the ledger erased the exoneration's reason:\n%s",
			exonerated.Rationale)
	}
	if got := l.ExonerationReason("h2"); got != reason {
		t.Errorf("ExonerationReason(h2) = %q, want the recorded reason", got)
	}
	if got := l.ExonerationReason("h1"); got != "" {
		t.Errorf("ExonerationReason(h1) = %q; h1 was never exonerated", got)
	}

	// FR-029c: the rendering is where an on-call reads it, and it carries the reason beside the
	// onset evidence the exoneration rests on.
	rendered := l.Render(ledger.RenderContext{})
	if !strings.Contains(rendered, reason) {
		t.Errorf("the exonerated section does not state why:\n%s", rendered)
	}
}

// A rollback is an operator's judgement, capped as a human fact is: Strong at most, and observed support
// that may name a change (004 T155).
func TestARollbackJudgmentIsCappedAtStrongAndCountsAsObservedSupport(t *testing.T) {
	t.Parallel()
	l, hypothesis, evidence := rollbackLedger(t)

	if _, err := l.Judge(ledger.Judgment{
		ID: "j-decisive", HypothesisID: hypothesis, EvidenceID: evidence,
		Direction: ledger.Supports, Strength: ledger.Decisive, Source: ledger.SourceRollback,
	}); err == nil {
		t.Fatal("a Decisive rollback was accepted; one operator action must not end an investigation")
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j-strong", HypothesisID: hypothesis, EvidenceID: evidence,
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceRollback,
	}); err != nil {
		t.Fatalf("a Strong rollback was refused: %v", err)
	}
	if _, ok := l.EvidencedSupport(hypothesis); !ok {
		t.Error("a rollback away from the change does not count as observed support")
	}
}

// rollbackLedger is a ledger with one change hypothesis and one graph answer to judge it on.
func rollbackLedger(t *testing.T) (*ledger.Ledger, string, string) {
	t.Helper()
	l := openLedger(t)
	item := evidenceItem("e-diff")
	item.Kind = "graph_answer"
	item.Worker = "graph"
	item.Capability = "diff"
	item.SourceOfTruth = ""
	if err := l.AddEvidence(item); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	return l, "h1", "e-diff"
}
