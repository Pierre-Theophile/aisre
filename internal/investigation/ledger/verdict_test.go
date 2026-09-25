// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The verdict rule (Phase 8 Track K-A, K1, FR-022, FR-023, constitution V).
//
// Prior-only mass is not confidence. A ranker that likes one candidate and a run that tested
// nothing produce a posterior near 1 and know nothing, and the reported bucket has to say so.

// TestAHypothesisNothingObservedSupportsRendersNoHigherThanModerate is the rule at its sharpest:
// a hypothesis holding 0.9 of the posterior on the prior alone is reported `moderate`, and the
// computed number is left exactly where the rule put it.
func TestAHypothesisNothingObservedSupportsRendersNoHigherThanModerate(t *testing.T) {
	t.Parallel()

	l, err := ledger.New("inv-0009", audit.PriorRecord{
		Prior: 0.05, AuditID: "audit-2026-09", Ceiling: 0.95, IncidentCount: 40,
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	if err := l.AddHypothesis(ledger.Hypothesis{
		ID: "h1", Kind: ledger.KindChange, Statement: "payments@rev7 caused the checkout error rate",
		CandidateChangeEntityID: "k8s.change=shop/payments@rev7",
	}, 0.95); err != nil {
		t.Fatalf("add h1: %v", err)
	}
	if err := l.AddHypothesis(ledger.Hypothesis{
		ID: "h2", Kind: ledger.KindChange, Statement: "the storefront scaling caused it",
		CandidateChangeEntityID: "k8s.change=shop/storefront@scale-1",
	}, 0.05); err != nil {
		t.Fatalf("add h2: %v", err)
	}

	top, ok := l.Hypothesis("h1")
	if !ok {
		t.Fatal("h1 is not in the ledger")
	}
	if top.Confidence < 0.85 {
		t.Fatalf("h1 confidence = %.6f, want the prior-only mass to compute above 0.85 for this test to "+
			"say anything", top.Confidence)
	}
	if ledger.BucketFor(top.Confidence).Name != "very_high" {
		t.Fatalf("the computed bucket for %.6f is %s, want very_high", top.Confidence,
			ledger.BucketFor(top.Confidence).Name)
	}
	if top.Bucket.Name != ledger.UnevidencedCeiling {
		t.Errorf("h1 is reported in %s at posterior %.6f with nothing observed behind it, want %s "+
			"(prior-only mass is not confidence)", top.Bucket.Name, top.Confidence, ledger.UnevidencedCeiling)
	}
	if l.Evidenced("h1") {
		t.Error("h1 is reported as evidenced with no judgment on it at all")
	}

	rendered := l.Render(ledger.RenderContext{})
	if !strings.Contains(rendered, "prior-only mass is not confidence") {
		t.Errorf("the turn rendering does not state the verdict rule:\n%s", rendered)
	}

	// The same hypothesis, once a telemetry answer supports it: the cap lifts and the computed
	// bucket is reported, because now there is something observed behind the number.
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)
	top, _ = l.Hypothesis("h1")
	if top.Bucket.Name != "very_high" {
		t.Errorf("h1 is reported in %s after a telemetry support at %.6f, want the computed bucket "+
			"very_high", top.Bucket.Name, top.Confidence)
	}
	if !l.Evidenced("h1") {
		t.Error("h1 is not reported as evidenced after a supporting judgment from a telemetry answer")
	}
	if err := l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Errorf("set supported after a telemetry support: %v", err)
	}
}

// TestAGraphAnswerAloneNeverNamesAHypothesis is the other half of the rule: the graph answer that
// put a candidate on the list cannot also be the evidence that it is the cause.
func TestAGraphAnswerAloneNeverNamesAHypothesis(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	graphAnswer := ledger.EvidenceItem{
		ID:                   "e-graph",
		Kind:                 "graph_answer",
		Worker:               "graph",
		Capability:           "diff",
		SourceOfTruth:        "graph:sreagent.graph.v1.QueryService",
		Mode:                 "recorded",
		Outcome:              "digest",
		ResponseDigest:       "sha256:e-graph",
		ResponseKey:          "graph/e-graph",
		Coverage:             &investigationv1.Coverage{DataSource: "graph", VolumeConsidered: 4},
		GraphEventIDs:        []string{"ev-1"},
		DeepLinkAbsentReason: "the graph has no query UI to link to",
	}
	if err := l.AddEvidence(graphAnswer); err != nil {
		t.Fatalf("add graph evidence: %v", err)
	}
	mustJudge(t, l, "j-graph", "h1", "e-graph", ledger.Supports, ledger.Strong)

	if _, ok := l.EvidencedSupport("h1"); ok {
		t.Error("a graph answer counts as evidenced support; it is what ranked the candidate, not a " +
			"measurement of this incident")
	}
	err := l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{})
	assertRejected(t, err, ledger.ReasonUnsupportedStatus)
}

// TestTheOpenHypothesisIsSupportedOnlyWhenNothingElseIs pins the terminal `unobserved` rule: the
// remainder becomes the answer when every rival failed to earn support, and never over a rival the
// evidence backs.
func TestTheOpenHypothesisIsSupportedOnlyWhenNothingElseIs(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	open := l.OpenHypothesisID()
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Refutes, ledger.Moderate)

	if !l.Evidenced(open) {
		t.Error("the open hypothesis is not evidenced although a rival was refuted on telemetry; " +
			"what the run ruled out is what carries the remainder")
	}
	if err := l.SetStatus(open, ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("support the open hypothesis with every rival unsupported: %v", err)
	}

	// A rival that does earn its own `supported` takes the answer back: the remainder is the
	// answer only while nothing else is.
	mustAddEvidence(t, l, "e2")
	mustJudge(t, l, "j2", "h2", "e2", ledger.Supports, ledger.Strong)
	if err := l.SetStatus("h2", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("support h2 on a telemetry answer: %v", err)
	}
	err := l.SetStatus(open, ledger.StatusSupported, ledger.StatusUpdate{})
	assertRejected(t, err, ledger.ReasonUnsupportedStatus)
}
