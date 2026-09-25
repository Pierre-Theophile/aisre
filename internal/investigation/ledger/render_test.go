// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The two renderings (T057, FR-020b): the compact turn-scoped table and the export a grader reads
// without replaying the run.

// TestRenderCarriesEverythingTheTurnNeeds walks the checklist FR-020b and FR-045 set for the text
// the engine re-injects each turn.
func TestRenderCarriesEverythingTheTurnNeeds(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)
	if err := l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("set supported: %v", err)
	}
	if err := l.SetStatus("h2", ledger.StatusUntested, ledger.StatusUpdate{
		Reason:            "no metric pointer of the required kind on checkout",
		NextQuery:         nextQuery(),
		NextQueryDeepLink: "https://example.invalid/compare-h2",
	}); err != nil {
		t.Fatalf("set untested: %v", err)
	}

	rendered := l.Render(ledger.RenderContext{
		Budgets: []ledger.BudgetLine{
			{Name: "wall_time", Remaining: 118, Limit: 300, Unit: "s"},
			{Name: "worker_calls", Remaining: 12, Limit: 40, Unit: "calls"},
		},
		StopConditions: []string{
			"15% of every budget is reserved for the closing synthesis",
			"diminishing returns below 0.01 of movement",
		},
	})

	want := []string{
		// identity and the rule in force
		"inv-0007", "rule " + ledger.LedgerRuleVersion, "π₀ 0.380000", "audit-2026-09",
		// the published buckets, each carrying its range (FR-023)
		"very_low [0.00, 0.10)", "very_high [0.85, 1.00]",
		// one line per hypothesis: id, kind, status, prior, confidence, bucket, counts, statement
		"h1", "change", "supported", "+1/-0", "payments@rev7 caused the checkout error rate",
		// the open hypothesis, rendered like any other (FR-019a)
		"no_observed_change", l.OpenHypothesisID(),
		// the untested hypothesis, with its reason and the exact next query (FR-031)
		"untested", "no metric pointer of the required kind on checkout",
		"next query: compare", "https://example.invalid/compare-h2",
		// budget remaining and the stop conditions in force (FR-045)
		"budget remaining", "wall_time 118/300 s", "worker_calls 12/40 calls",
		"stop conditions in force", "diminishing returns below 0.01 of movement",
	}
	for _, w := range want {
		if !strings.Contains(rendered, w) {
			t.Errorf("the turn rendering does not contain %q:\n%s", w, rendered)
		}
	}

	// Deterministic: the same ledger renders the same bytes, which is what makes a recorded
	// trajectory comparable at all.
	if again := l.Render(ledger.RenderContext{}); again != l.Render(ledger.RenderContext{}) {
		t.Error("two renderings of the same ledger differ")
	}
	// A ledger with no budget context renders without those two lines rather than with empty ones.
	if bare := l.Render(ledger.RenderContext{}); strings.Contains(bare, "budget remaining") {
		t.Error("a ledger with no budget context still rendered a budget line")
	}
}

// TestRenderOrdersHypothesesByRank: the table is the ranked list FR-019 calls the primary output.
func TestRenderOrdersHypothesesByRank(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h2", "e1", ledger.Supports, ledger.Decisive)

	hypotheses := l.Hypotheses()
	for i, h := range hypotheses {
		if h.Rank != i+1 {
			t.Errorf("hypothesis %s is at position %d with rank %d", h.ID, i+1, h.Rank)
		}
		if i > 0 && hypotheses[i-1].Confidence < h.Confidence {
			t.Errorf("rank order is not by confidence: %s (%.6f) before %s (%.6f)",
				hypotheses[i-1].ID, hypotheses[i-1].Confidence, h.ID, h.Confidence)
		}
	}
	if hypotheses[0].ID != "h2" {
		t.Errorf("a decisively supported candidate is ranked %d, not first", hypotheses[0].Rank)
	}
}

// TestExportIsReadableWithoutReplayingTheRun is FR-020b's real requirement: the canonical JSON
// carries the priors, the typed judgments and the evidence ids, so a grader can re-derive the
// conclusion rather than take it on trust.
func TestExportIsReadableWithoutReplayingTheRun(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustAddEvidence(t, l, "e2")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)
	mustJudge(t, l, "j2", "h1", "e2", ledger.Refutes, ledger.Weak)

	exported := l.Proto()
	switch {
	case exported.GetInvestigationId() != "inv-0007":
		t.Errorf("exported investigation id = %q", exported.GetInvestigationId())
	case exported.GetLedgerRuleVersion() != ledger.LedgerRuleVersion:
		t.Errorf("exported rule version = %q, want %q",
			exported.GetLedgerRuleVersion(), ledger.LedgerRuleVersion)
	case len(exported.GetBuckets()) != 5:
		t.Errorf("the export carries %d buckets, want the five published ones", len(exported.GetBuckets()))
	case len(exported.GetHypotheses()) != 3:
		t.Errorf("the export carries %d hypotheses, want 3 including the open one",
			len(exported.GetHypotheses()))
	case len(exported.GetJudgments()) != 2:
		t.Errorf("the export carries %d judgments, want 2", len(exported.GetJudgments()))
	case len(exported.GetEvidence()) != 2:
		t.Errorf("the export carries %d evidence items, want 2", len(exported.GetEvidence()))
	}

	top := exported.GetHypotheses()[0]
	if top.GetHypothesisId() != "h1" {
		t.Fatalf("the first exported hypothesis is %s, want h1", top.GetHypothesisId())
	}
	switch {
	case len(top.GetSupportingEvidenceIds()) != 1 || top.GetSupportingEvidenceIds()[0] != "e1":
		t.Errorf("supporting evidence ids = %v, want [e1]", top.GetSupportingEvidenceIds())
	case len(top.GetRefutingEvidenceIds()) != 1 || top.GetRefutingEvidenceIds()[0] != "e2":
		t.Errorf("refuting evidence ids = %v, want [e2]", top.GetRefutingEvidenceIds())
	case top.GetPrior() == 0 || top.GetConfidence() == 0:
		t.Errorf("the export lost the prior or the confidence: %+v", top)
	case top.GetBucket().GetName() == "":
		t.Error("the export carries a confidence with no bucket")
	case top.GetRationale() == "":
		t.Error("the export carries no rationale")
	}

	// The ln LR travels with the judgment, so the posterior is reproducible from the export
	// alone (Invariant 1).
	if exported.GetJudgments()[0].GetLnLr() != ledger.LnLRStrong {
		t.Errorf("exported ln LR = %v, want the published %v",
			exported.GetJudgments()[0].GetLnLr(), ledger.LnLRStrong)
	}

	raw, err := l.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("the exported ledger is not valid JSON: %s", raw)
	}
	// Canonical means stable: the same ledger, the same bytes, every time and on every machine.
	again, err := l.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical json (second): %v", err)
	}
	if string(raw) != string(again) {
		t.Error("two canonical renderings of the same ledger differ")
	}

	digest, err := l.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if len(digest) != 64 {
		t.Errorf("ledger digest = %q, want a hex sha256", digest)
	}

	// A judgment arriving changes the digest: "the ledger did not move" has to be checkable.
	mustAddEvidence(t, l, "e3")
	mustJudge(t, l, "j3", "h2", "e3", ledger.Supports, ledger.Moderate)
	moved, err := l.Digest()
	if err != nil {
		t.Fatalf("digest after judging: %v", err)
	}
	if moved == digest {
		t.Error("judging did not change the ledger digest")
	}
}

// TestExportCarriesNoTelemetry is constitution IV at this boundary: an evidence item in the export
// is a digest reference, a coverage block and join keys — never a sample.
func TestExportCarriesNoTelemetry(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")

	raw, err := l.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	for _, forbidden := range []string{"\"samples\"", "\"value\"", "\"logBody\"", "\"traceId\""} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("the exported ledger contains %s:\n%s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), "responseDigest") || !strings.Contains(string(raw), "coverage") {
		t.Errorf("the exported evidence carries neither a digest nor a coverage block:\n%s", raw)
	}
}
