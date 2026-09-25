// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	graphworker "github.com/Pierre-Theophile/aisre/internal/investigation/workers/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
	"github.com/Pierre-Theophile/aisre/pkg/worker"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// The verdict rule (Phase 8 Track K-A, K1, FR-022, constitution V).
//
// The metamorphic `culprit-deleted` variant is the test that matters here: the symptom is exactly
// what it was, and the change that caused it is gone from the graph. Every candidate the ranker
// can still offer is a decoy by construction, so the only answer the evidence supports is
// `unobserved` — with the symptom still localised on the subject.

const decoyChange = "k8s.change=shop/storefront@rev4"

// culpritDeletedGraph is the fake graph with the culprit's change deleted: the rollout that
// actually degraded payments is not in the diff any more, and a pre-onset decoy on storefront is
// all the ranker has left to offer.
type culpritDeletedGraph struct{ fakeGraph }

func (g *culpritDeletedGraph) Diff(_ context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	reference := req.GetT2().AsTime().UTC()
	if req.GetReferenceAt() != nil {
		reference = req.GetReferenceAt().AsTime().UTC()
	}
	params := query.RankParams{Reference: reference, Tau: query.DefaultTau, HopCap: 3}
	ranked, _ := query.Rank([]query.Candidate{
		{
			Change: changeNode(decoyChange, onsetAt.Add(-3*time.Minute),
				graphv1.ChangeKind_ROLLOUT, graphv1.ActorKind_PERSON),
			TargetIDs: []string{storefrontRef}, Hop: 1, WeightClass: 3,
		},
	}, params)
	return &graphv1.DiffResponse{Changes: ranked, RankingFormula: query.RankingFormula(params)}, nil
}

// newGraphHarness is newHarness with the graph family swapped, so a test can delete the culprit
// from the topology without touching the telemetry the symptom is made of.
func newGraphHarness(t *testing.T, graph graphworker.QueryService) *harness {
	t.Helper()

	backend, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("synthetic backend: %v", err)
	}
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{
		graphworker.New(graph),
		metrics.New(backend),
		logs.New(backend),
		traces.New(backend),
	} {
		if err := registry.Register(w); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	workers, err := engine.NewWorkers(registry, worker.RetryPolicy{})
	if err != nil {
		t.Fatalf("workers: %v", err)
	}
	clock := &testClock{now: firedAt}
	manager, err := budget.New(budget.PageProfile(), budget.OperatorCaps{}, clock.fn())
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	e, err := engine.New(engine.Config{
		InvestigationID: "inv-test-culprit-deleted",
		Subject: engine.Subject{
			EntityRef: checkoutRef,
			Statement: "checkout error rate above threshold",
			FiredAt:   firedAt,
			Origin:    "monitor:12345",
			Priority:  "P1",
			Window: &investigationv1.Window{
				Start: timestamppb.New(windowStart),
				End:   timestamppb.New(firedAt),
			},
		},
		Instants: engine.Instants{ValidAt: firedAt, ObservedAt: firedAt},
		Prior:    audit.PriorRecord{Prior: 0.38, AuditID: "audit-2026-09", Ceiling: 0.62, IncidentCount: 120},
		Workers:  workers,
		Budget:   manager,
		Mode:     worker.ModeRecorded,
		Clock:    clock.fn(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return &harness{engine: e, budget: manager, clock: clock}
}

// TestWithTheCulpritDeletedTheVerdictIsUnobserved is K1's headline: the engine does not name the
// only candidate left when the evidence does not support it.
func TestWithTheCulpritDeletedTheVerdictIsUnobserved(t *testing.T) {
	t.Parallel()

	h := newGraphHarness(t, &culpritDeletedGraph{})
	stop, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stop.Reason != engine.StopCompleted {
		t.Fatalf("stop = %s: %s", stop.Reason, stop.Detail)
	}

	verdict := h.engine.Verdict()
	if verdict.Kind != engine.VerdictUnobserved {
		t.Errorf("verdict = %s (%s), want %s: the culprit is not in the graph, so no observed change "+
			"explains the symptom", verdict.Kind, verdict.Line(), engine.VerdictUnobserved)
	}
	// `unobserved` without the symptom localised is a shrug, not a finding (SC-023).
	if verdict.Symptom != checkoutRef {
		t.Errorf("the verdict localises the symptom on %q, want the subject %q", verdict.Symptom, checkoutRef)
	}
	if !strings.Contains(verdict.Line(), checkoutRef) {
		t.Errorf("the verdict line does not name the subject:\n%s", verdict.Line())
	}

	ranked := h.engine.Ledger().Hypotheses()
	if top := ranked[0]; top.Kind != ledger.KindNoObservedChange || top.Status != ledger.StatusSupported {
		t.Errorf("the top hypothesis is %s (%s, %s), want the open hypothesis supported",
			top.ID, top.Kind, top.Status)
	}
	decoy := hypothesisFor(t, h.engine.Ledger(), decoyChange)
	if decoy.Status != ledger.StatusRefuted {
		t.Errorf("the decoy %s is %s, want %s: its own target did not move across the reference instant",
			decoy.ID, decoy.Status, ledger.StatusRefuted)
	}
	switch decoy.Bucket.Name {
	case "high", "very_high":
		t.Errorf("the decoy is reported in %s at %.6f; a candidate the evidence does not support is "+
			"never published above %s", decoy.Bucket.Name, decoy.Confidence, ledger.UnevidencedCeiling)
	}
	if verdict.Change == decoyChange {
		t.Errorf("the verdict names the decoy %s", decoyChange)
	}
}

// TestTheCulpritIsNamedWhenTheEvidenceSupportsIt is the same rule read the other way: the verdict
// rule must not make the engine mute on the incidents it can solve.
func TestTheCulpritIsNamedWhenTheEvidenceSupportsIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, "")
	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	verdict := h.engine.Verdict()
	if verdict.Kind != engine.VerdictNamed {
		t.Fatalf("verdict = %s (%s), want %s", verdict.Kind, verdict.Line(), engine.VerdictNamed)
	}
	if verdict.Change != rolloutChange {
		t.Errorf("the verdict names %q, want the culprit %q", verdict.Change, rolloutChange)
	}
	if verdict.Symptom != checkoutRef {
		t.Errorf("the verdict localises the symptom on %q, want the subject %q", verdict.Symptom, checkoutRef)
	}
	named, ok := h.engine.Ledger().Hypothesis(verdict.HypothesisID)
	if !ok {
		t.Fatalf("the named hypothesis %s is not in the ledger", verdict.HypothesisID)
	}
	if _, supported := h.engine.Ledger().EvidencedSupport(named.ID); !supported {
		t.Error("the named hypothesis carries no supporting judgment from telemetry evidence or a human fact")
	}
}

// The decoy reasons (Phase 8 Track K-A, K3, FR-031, incident-format §"Decoy roles").
//
// A decoy left `inconclusive` with nothing said about it is a hole in the report: the reader
// cannot tell whether it was ruled out or merely skipped. Every candidate the wave meets leaves
// with a status and a sentence.

const (
	coincidentDecoy = "k8s.change=shop/storefront@rev4"
	secondDecoy     = "k8s.change=shop/storefront@rev5"
	cappedDecoy     = "k8s.change=shop/storefront@rev6"
	staleDecoy      = "k8s.change=shop/storefront@rev1"
)

// decoyGraph ranks the culprit, three coincident decoys and one change far older than the onset.
type decoyGraph struct{ fakeGraph }

func (g *decoyGraph) Diff(_ context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	reference := req.GetT2().AsTime().UTC()
	if req.GetReferenceAt() != nil {
		reference = req.GetReferenceAt().AsTime().UTC()
	}
	params := query.RankParams{Reference: reference, Tau: query.DefaultTau, HopCap: 3}
	candidate := func(id string, at time.Time, target string) query.Candidate {
		return query.Candidate{
			Change:    changeNode(id, at, graphv1.ChangeKind_ROLLOUT, graphv1.ActorKind_PERSON),
			TargetIDs: []string{target}, Hop: 1, WeightClass: 1,
		}
	}
	ranked, _ := query.Rank([]query.Candidate{
		candidate(rolloutChange, rolloutAt, paymentsRef),
		candidate(coincidentDecoy, onsetAt.Add(-3*time.Minute), storefrontRef),
		candidate(secondDecoy, onsetAt.Add(-4*time.Minute), storefrontRef),
		candidate(cappedDecoy, onsetAt.Add(-5*time.Minute), storefrontRef),
		candidate(staleDecoy, onsetAt.Add(-70*time.Minute), storefrontRef),
	}, params)
	return &graphv1.DiffResponse{Changes: ranked, RankingFormula: query.RankingFormula(params)}, nil
}

func TestEveryDecoyLeavesTheWaveWithAStatedReason(t *testing.T) {
	t.Parallel()

	h := newGraphHarness(t, &decoyGraph{})
	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	book := h.engine.Ledger()

	// The coincident decoy was tested: its own target did not move across the onset, and the wave
	// says so as a refutation rather than leaving it inconclusive.
	decoy := hypothesisFor(t, book, coincidentDecoy)
	if decoy.Status != ledger.StatusRefuted {
		t.Errorf("the coincident decoy %s is %s, want %s: storefront's error rate is flat across the "+
			"onset (rationale: %s)", decoy.ID, decoy.Status, ledger.StatusRefuted, decoy.Rationale)
	}
	if decoy.Rationale == "" {
		t.Errorf("the coincident decoy %s carries no rationale", decoy.ID)
	}

	// The stale change is older than 2τ before the onset. It is reported untested — never refuted,
	// because old is not innocent — with the reason and the exact query that would test it.
	stale := hypothesisFor(t, book, staleDecoy)
	if stale.Status != ledger.StatusUntested {
		t.Errorf("the stale change %s is %s, want %s", stale.ID, stale.Status, ledger.StatusUntested)
	}
	if !strings.Contains(stale.UntestedReason, "before the reference instant") {
		t.Errorf("the stale change's reason does not say why it was skipped: %q", stale.UntestedReason)
	}
	if stale.NextQuery == nil {
		t.Errorf("the stale change %s carries no next query; FR-031 hands over the query that would "+
			"test it", stale.ID)
	}

	// The candidate beyond the wave's cap is reported untested too, with its own reason.
	capped := hypothesisFor(t, book, cappedDecoy)
	if capped.Status != ledger.StatusUntested {
		t.Errorf("the candidate beyond the cap %s is %s, want %s (reason: %q)",
			capped.ID, capped.Status, ledger.StatusUntested, capped.UntestedReason)
	}
	if !strings.Contains(capped.UntestedReason, "top 3") {
		t.Errorf("the capped candidate's reason does not name the wave's cap: %q", capped.UntestedReason)
	}

	// And none of that cost the culprit its answer.
	verdict := h.engine.Verdict()
	if verdict.Kind != engine.VerdictNamed || verdict.Change != rolloutChange {
		t.Errorf("verdict = %s %q, want the culprit named", verdict.Kind, verdict.Change)
	}

	rendered := book.Render(ledger.RenderContext{})
	for _, want := range []string{"untested (never scored as refuted", stale.ID, capped.ID} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the turn rendering does not carry %q:\n%s", want, rendered)
		}
	}
}

// TestOnsetIsEstimatedOnTheCandidateCausesToo is K2: the subject is not the only series that knows
// when the symptom started, and the earliest confident estimate is the reference.
func TestOnsetIsEstimatedOnTheCandidateCausesToo(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, "")
	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	onset := h.engine.Onset()
	if onset == nil || !onset.Available {
		t.Fatalf("no onset was estimated: %v", onset)
	}
	// The cause's own series was measured, which is what makes a decisive predicate written over
	// `estimated_onset` on the cause satisfiable at all.
	var cause *engine.OnsetEstimate
	for i := range onset.Considered {
		if onset.Considered[i].EntityRef == paymentsRef {
			cause = &onset.Considered[i]
		}
	}
	if cause == nil {
		t.Fatalf("onset was estimated on %d series, none of them the culprit's target %s",
			len(onset.Considered), paymentsRef)
	}
	if !cause.Available || cause.EvidenceID == "" {
		t.Errorf("the onset estimate on the culprit's target is %v; want an available estimate with "+
			"an evidence item behind it", cause)
	}

	// The reference is the earliest confident estimate, and its own series is named.
	for _, each := range onset.Considered {
		if each.Available && each.At.Before(onset.At) {
			t.Errorf("the reference onset is %s but %s puts it at %s; the earliest confident estimate "+
				"is the reference", onset.At, each.EntityRef, each.At)
		}
	}
	if onset.EntityRef != paymentsRef {
		t.Errorf("the onset used was estimated on %s, want the culprit's target %s: it is the tighter "+
			"estimate at the same instant", onset.EntityRef, paymentsRef)
	}
	if onset.PointerID == "" {
		t.Error("the onset does not say which pointer it came from")
	}
	if !strings.Contains(onset.Line(), onset.EntityRef) {
		t.Errorf("the onset line does not name the series it came from:\n%s", onset.Line())
	}
	if detail := onset.Detail(); !strings.Contains(detail, paymentsRef) || !strings.Contains(detail, "← used") {
		t.Errorf("the onset detail does not list the estimates considered:\n%s", detail)
	}

	// The estimate is in the ledger as evidence, filed under the published onset kind.
	item, ok := h.engine.Ledger().Evidence(cause.EvidenceID)
	if !ok {
		t.Fatalf("the onset evidence %s is not in the ledger", cause.EvidenceID)
	}
	if item.Kind != ledger.EvidenceKindOnsetEstimate {
		t.Errorf("the onset evidence is filed as %s, want %s", item.Kind, ledger.EvidenceKindOnsetEstimate)
	}
	if item.Term.GetOnset().GetPointer().GetSelector() == "" {
		t.Error("the onset evidence does not carry the pointer it was measured on")
	}
}
