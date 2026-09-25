// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
)

// The rendering (T085, T086, FR-019, FR-026, FR-028, FR-057c, FR-057d, SC-025).
//
// The order is the contract, so the first test asserts it as an order: the four section headers
// appear once each, in the published sequence, and nothing appears before the verdict.

var at = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

// syntheticLedger is a hand-built ledger with everything the rendering has to be able to show:
// a supported candidate, an exonerated one, an untested one with a reason and a next query, and
// the mandatory open hypothesis.
func syntheticLedger(t *testing.T) *ledger.Ledger {
	t.Helper()

	l, err := ledger.New("inv-render-01", audit.PriorRecord{
		Prior: 0.38, AuditID: "audit-2026-09", Ceiling: 0.62, IncidentCount: 13,
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	for _, h := range []struct {
		id        string
		statement string
		culprit   string
		score     float64
	}{
		{"h-rev7", "shop/payments@rev7 caused the checkout error rate", "k8s.change=shop/payments@rev7", 0.7},
		{"h-cfg", "the config map change caused it", "k8s.change=shop/config@rev3", 0.2},
		{"h-node", "the node pool rotation caused it", "k8s.change=pool/rotate-9", 0.1},
	} {
		if err := l.AddHypothesis(ledger.Hypothesis{
			ID: h.id, Kind: ledger.KindChange, Statement: h.statement,
			CandidateChangeEntityID: h.culprit, TargetEntityIDs: []string{"e:payments"},
			ActorKind: "automation",
		}, h.score); err != nil {
			t.Fatalf("add hypothesis %s: %v", h.id, err)
		}
	}
	for _, e := range []ledger.EvidenceItem{
		{
			ID: "ev-errors", Kind: "algebra_answer", Worker: "metrics",
			Capability: "errors_by_version", SourceOfTruth: "metrics",
			ValidAt: at, ObservedAt: at, CalledAt: at, Mode: "recorded", Outcome: "digest",
			Coverage: &investigationv1.Coverage{DataSource: "metrics", VolumeConsidered: 4200},
			DeepLink: "https://metrics.invalid/q?version=rev7",
			FreeText: "error rate 0.31 on rev7 against 0.004 on rev6",
		},
		{
			ID: "ev-onset", Kind: "onset_estimate", Worker: "metrics", Capability: "onset",
			SourceOfTruth: "metrics",
			ValidAt:       at, ObservedAt: at, CalledAt: at, Mode: "recorded", Outcome: "digest",
			Coverage: &investigationv1.Coverage{DataSource: "metrics", VolumeConsidered: 900},
			DeepLink: "https://metrics.invalid/onset",
			FreeText: "onset estimated at 14:18Z ± 90s",
		},
	} {
		if err := l.AddEvidence(e); err != nil {
			t.Fatalf("add evidence %s: %v", e.ID, err)
		}
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j-rev7", HypothesisID: "h-rev7", EvidenceID: "ev-errors",
		Direction: ledger.Supports, Strength: ledger.Decisive, Source: ledger.SourceFirstWave,
		RecordedAt: at,
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if err := l.SetStatus("h-rev7", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("set supported: %v", err)
	}
	if _, err := l.Exonerate("h-node", ledger.Exoneration{
		JudgmentID:      "j-node-exonerated",
		OnsetEvidenceID: "ev-onset",
		Reason:          "the rotation started 11 minutes after the estimated onset",
	}); err != nil {
		t.Fatalf("exonerate: %v", err)
	}
	if err := l.SetStatus("h-cfg", ledger.StatusUntested, ledger.StatusUpdate{
		Reason: "no log pointer of the needed kind on shop/config",
		NextQuery: backend.GraphPointers(&graphv1.PointersRequest{
			Focus: &graphv1.Ref{Namespace: "k8s.service", Value: "shop/config"},
		}),
		NextQueryDeepLink: "aisre query pointers k8s.service=shop/config --as-of 2026-09-01T14:32:00Z",
	}); err != nil {
		t.Fatalf("set untested: %v", err)
	}
	return l
}

func syntheticReport(t *testing.T) *render.Report {
	t.Helper()
	l := syntheticLedger(t)
	report := render.NewReport(&investigationv1.Investigation{
		InvestigationId: "inv-render-01",
		IncidentId:      "inc-render-01",
		Lifecycle:       investigationv1.Lifecycle_CONCLUDED,
		ConclusionKind:  investigationv1.ConclusionKind_CONCLUSION_FINAL,
		Outcome:         investigationv1.InvestigationOutcome_RANKED,
		StartedAt:       timestamppb.New(at),
	}, l)
	report.Timeline = []render.TimelineEntry{
		{
			At: at.Add(-14 * time.Minute), Kind: render.TimelineChange,
			Statement: "shop/payments rolled out rev7", EntityID: "e:payments",
			EvidenceID: "ev-errors", DeepLink: "https://metrics.invalid/q?version=rev7",
		},
		{
			At: at.Add(-14 * time.Minute), Kind: render.TimelineOnset,
			Statement: "symptom onset estimated", EvidenceID: "ev-onset",
			DeepLink: "https://metrics.invalid/onset", Uncertainty: 90 * time.Second,
		},
	}
	report.Narrative = "rev7 shipped fourteen minutes before the onset and the error rate on that " +
		"version is two orders of magnitude above rev6."
	return report
}

// TestRenderingOrderIsThePublishedOrder is FR-057c as a test on the text: verdict, then the
// ranked list, then the timeline, then the narrative. The narrative never precedes the verdict.
func TestRenderingOrderIsThePublishedOrder(t *testing.T) {
	t.Parallel()

	out, err := syntheticReport(t).Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	previous := -1
	for _, section := range render.SectionOrder {
		header := "## " + section
		idx := strings.Index(out, header)
		if idx < 0 {
			t.Fatalf("section %q is missing from the rendering", section)
		}
		if strings.Count(out, header) != 1 {
			t.Errorf("section %q appears %d times, want once", section, strings.Count(out, header))
		}
		if idx <= previous {
			t.Fatalf("section %q at %d comes before the previous section at %d; the published "+
				"order is %v (FR-057c)", section, idx, previous, render.SectionOrder)
		}
		previous = idx
	}

	// The verdict is the first thing in the document, ahead of every section header.
	if !strings.HasPrefix(out, "## "+render.SectionVerdict) {
		t.Errorf("the rendering does not start with the verdict:\n%s", firstLines(out, 3))
	}
}

func TestVerdictNamesTheDecisiveFactAndTheRollbackCandidate(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	line := report.VerdictLine()
	if !strings.Contains(line, "rollback candidate: k8s.change=shop/payments@rev7") {
		t.Errorf("verdict = %q, want it to name the rollback candidate (FR-057c)", line)
	}
	if !strings.Contains(line, "decisive fact") || !strings.Contains(line, "ev-errors") {
		t.Errorf("verdict = %q, want it to name the decisive fact and cite it", line)
	}
}

func TestVerdictSaysWhenThereIsNoRollbackCandidate(t *testing.T) {
	t.Parallel()

	l, err := ledger.New("inv-open-01", audit.PriorRecord{
		Prior: 0.9, AuditID: "audit-2026-09", Ceiling: 0.1, IncidentCount: 13,
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	report := render.NewReport(&investigationv1.Investigation{InvestigationId: "inv-open-01"}, l)
	line := report.VerdictLine()
	if !strings.Contains(line, "there is no rollback candidate") {
		t.Errorf("verdict = %q, want it to state plainly that there is none (FR-057c)", line)
	}
	if !strings.Contains(line, "no observed change explains this") {
		t.Errorf("verdict = %q, want the open hypothesis named", line)
	}
}

// TestExonerationsAreRenderedAsProminentlyAsSupports is FR-029c/FR-057c: an exoneration is a
// finding, not a footnote. It appears in the ranked list, under its hypothesis, in the same
// shape as a support, with a deep link.
func TestExonerationsAreRenderedAsProminentlyAsSupports(t *testing.T) {
	t.Parallel()

	out, err := syntheticReport(t).Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ranked := section(out, render.SectionRanked)
	if !strings.Contains(ranked, "exonerates") {
		t.Fatalf("the ranked list does not render the exoneration:\n%s", ranked)
	}
	// Same indentation as a support: both are "   <label> (<strength>): …".
	supportIndent := indentOf(ranked, "supports (")
	exonerateIndent := indentOf(ranked, "exonerates (")
	if supportIndent != exonerateIndent {
		t.Errorf("support is indented %d and the exoneration %d; FR-057c renders them as "+
			"prominently as each other", supportIndent, exonerateIndent)
	}
	if !strings.Contains(ranked, "ev-onset") {
		t.Errorf("the exoneration does not cite the onset estimate it rests on (Invariant 9):\n%s", ranked)
	}
}

// TestEveryEvidenceItemCarriesADeepLinkOrSaysWhyNot is FR-057d over the whole document.
func TestEveryEvidenceItemCarriesADeepLinkOrSaysWhyNot(t *testing.T) {
	t.Parallel()

	l := syntheticLedger(t)
	// One item deliberately has no link, and must therefore say why.
	if err := l.AddEvidence(ledger.EvidenceItem{
		ID: "ev-human", Kind: "human_fact", Worker: "human", Capability: "manual_action",
		ValidAt: at, ObservedAt: at, CalledAt: at, Mode: "live", Outcome: "digest",
		Coverage:             &investigationv1.Coverage{DataSource: "human"},
		DeepLinkAbsentReason: "a human fact is not a query",
		FreeText:             "I restarted a payments pod by hand at 14:20",
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j-human", HypothesisID: "h-cfg", EvidenceID: "ev-human",
		Direction: ledger.Refutes, Strength: ledger.Strong, Source: ledger.SourceHumanFact,
		RecordedAt: at,
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}

	report := render.NewReport(&investigationv1.Investigation{InvestigationId: "inv-render-01"}, l)
	out, err := report.Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, id := range []string{"ev-errors", "ev-onset", "ev-human"} {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, "["+id+"]") {
				continue
			}
			if !strings.Contains(line, "http") && !strings.Contains(line, "no deep link:") {
				t.Errorf("evidence %s is cited with neither a deep link nor a reason there is "+
					"none (FR-057d): %q", id, line)
			}
		}
	}
}

// --- the `unknown` outcome (T086, FR-026) --------------------------------------------------

func TestUnknownNamesWhatWouldResolveIt(t *testing.T) {
	t.Parallel()

	l := syntheticLedger(t)
	report := render.NewReport(&investigationv1.Investigation{
		InvestigationId: "inv-unknown-01",
		Outcome:         investigationv1.InvestigationOutcome_UNKNOWN,
	}, l)
	report.Resolutions = render.ResolutionsFor(l)

	out, err := report.Unknown()
	if err != nil {
		t.Fatalf("render unknown: %v", err)
	}
	if !strings.Contains(out, "what would resolve this:") {
		t.Fatalf("an `unknown` rendering must name what would resolve it (FR-026):\n%s", out)
	}
	if !strings.Contains(out, render.ResolutionAddPointer) {
		t.Errorf("the untested hypothesis's reason names a missing pointer, so the resolution "+
			"kind should be %q:\n%s", render.ResolutionAddPointer, out)
	}
	if !strings.Contains(out, "h-cfg") {
		t.Errorf("the resolution does not say which hypothesis it would change:\n%s", out)
	}
	// The hypotheses considered, with their statuses and evidence, are still there: `unknown`
	// is a result and is rendered like one.
	for _, want := range []string{"h-rev7", "h-node", "supported", "exonerated", "untested"} {
		if !strings.Contains(out, want) {
			t.Errorf("an `unknown` rendering omits %q; FR-026 lists the hypotheses considered "+
				"with their statuses and evidence", want)
		}
	}
}

func TestUnknownWithNoResolvingActionIsRefused(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	report.Resolutions = nil
	_, err := report.Unknown()
	if !errors.Is(err, render.ErrUnknownWithoutResolution) {
		t.Fatalf("error = %v, want ErrUnknownWithoutResolution: `unknown` with no way forward "+
			"leaves the reader nothing to do (FR-026)", err)
	}
}

// --- the no-remediation guard (FR-028, SC-025) ---------------------------------------------

func TestGuardRejectsRemediation(t *testing.T) {
	t.Parallel()

	rejected := []string{
		"Next steps: roll back shop/payments to rev6.",
		"You should restart the payments deployment.",
		"Run kubectl rollout undo deployment/payments -n shop",
		"Remediation: scale up the node pool.",
		"To fix this, revert the config change.",
	}
	for _, line := range rejected {
		if err := render.Guard(line); !errors.Is(err, render.ErrRemediationProposed) {
			t.Errorf("Guard(%q) = %v, want ErrRemediationProposed (FR-028)", line, err)
		}
	}

	accepted := []string{
		"rollback candidate: k8s.change=shop/payments@rev7",
		"shop/payments@rev7 caused the checkout error rate (high [0.80–0.95])",
		"what would resolve this:",
		"- [add_pointer] test \"the config map change caused it\": no log pointer of the needed kind",
		"- [widen_window] investigate a wider window: the onset falls outside it",
		"check the errors_by_version digest for rev6 over the baseline window",
		"confirm the identity of otel.service.name=checkout",
		"the node pool rotation started 11 minutes after the estimated onset and is exonerated",
		// A human fact reporting what a person already did is evidence, not a proposal
		// (FR-057a). A guard that refused it would make the human channel unusable.
		"refutes (strong): I restarted a payments pod by hand at 14:20 [ev-human]",
	}
	for _, line := range accepted {
		if err := render.Guard(line); err != nil {
			t.Errorf("Guard(%q) = %v, want nil: naming a candidate or a thing to investigate is "+
				"a diagnosis, not a remediation (FR-028, FR-057c)", line, err)
		}
	}
}

func TestHumanRefusesToEmitARenderingTheGuardRejects(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	report.Narrative = "The fix is to roll back shop/payments to rev6 immediately."
	if _, err := report.Human(); !errors.Is(err, render.ErrRemediationProposed) {
		t.Fatalf("error = %v, want the rendering refused rather than trimmed (FR-028)", err)
	}
}

// --- the two surfaces carry the same claims (FR-064) ----------------------------------------

func TestMachineAndHumanCarryTheSameVerdict(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	human, err := report.Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	machine := report.Machine()
	if machine.GetVerdictLine() == "" {
		t.Fatal("the machine rendering carries no verdict line")
	}
	if !strings.Contains(human, machine.GetVerdictLine()) {
		t.Errorf("the two renderings disagree:\nmachine: %q\nhuman:\n%s",
			machine.GetVerdictLine(), firstLines(human, 4))
	}
	if len(machine.GetLedger().GetHypotheses()) != len(report.Ledger.Hypotheses) {
		t.Errorf("the machine ledger has %d hypotheses, the rendering %d",
			len(machine.GetLedger().GetHypotheses()), len(report.Ledger.Hypotheses))
	}
}

// TestFromProtoRendersTheSameOrder is the CLI's path: a report rebuilt from the wire message
// renders the same sections in the same order as one built from the live ledger.
func TestFromProtoRendersTheSameOrder(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	machine := report.Machine()

	rebuilt, err := render.FromProto(machine)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	out, err := rebuilt.Human()
	if err != nil {
		t.Fatalf("render rebuilt: %v", err)
	}
	previous := -1
	for _, s := range render.SectionOrder {
		idx := strings.Index(out, "## "+s)
		if idx <= previous {
			t.Fatalf("section %q out of order in the wire-side rendering", s)
		}
		previous = idx
	}
	if !strings.Contains(out, machine.GetVerdictLine()) {
		t.Errorf("the wire-side rendering lost the verdict line")
	}
	// Ranks and statuses survive the round trip, which is what makes the two surfaces
	// interchangeable for a reviewer (FR-064).
	for _, want := range []string{"supported", "exonerated", "untested"} {
		if !strings.Contains(out, want) {
			t.Errorf("the wire-side rendering lost status %q", want)
		}
	}
}

func section(out, name string) string {
	start := strings.Index(out, "## "+name)
	if start < 0 {
		return ""
	}
	rest := out[start+len(name)+3:]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		return rest[:next]
	}
	return rest
}

func indentOf(text, needle string) int {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return len(line) - len(strings.TrimLeft(line, " "))
		}
	}
	return -1
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// A rollback judgment says in words that production was rolled back away from the change (004 T155).
// Its evidence is a graph answer, so "supports: graph.diff" would hide the one thing the reader needs:
// that an operator already judged this change to be the problem.
func TestARollbackJudgmentIsRenderedAsTheOperatorsJudgement(t *testing.T) {
	t.Parallel()

	l := syntheticLedger(t)
	if err := l.AddEvidence(ledger.EvidenceItem{
		ID: "ev-diff", Kind: "graph_answer", Worker: "graph", Capability: "diff",
		ValidAt: at, ObservedAt: at, CalledAt: at, Mode: "recorded", Outcome: "digest",
		Coverage: &investigationv1.Coverage{DataSource: "graph"},
		DeepLink: "aisre query diff otel.service.name=checkout",
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j-rollback", HypothesisID: "h-rev7", EvidenceID: "ev-diff",
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceRollback,
		RecordedAt: at,
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	out, err := render.NewReport(&investigationv1.Investigation{InvestigationId: "inv-render-01"}, l).Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(section(out, render.SectionRanked),
		"supports — production was rolled back away from this change (strong)") {
		t.Errorf("the rollback judgment is not rendered as the operator's judgement:\n%s",
			section(out, render.SectionRanked))
	}
	// And the verdict does not send an on-call to a button someone already pressed.
	line := render.NewReport(&investigationv1.Investigation{InvestigationId: "inv-render-01"}, l).VerdictLine()
	if !strings.Contains(line, "rollback candidate: k8s.change=shop/payments@rev7 (already rolled back by an operator)") {
		t.Errorf("verdict = %q, want the candidate named and marked as already rolled back", line)
	}
}
