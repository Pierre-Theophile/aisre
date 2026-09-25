// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// Lifecycle, the human channel, evidence recording and report delivery against a real database
// (T082, T083, T084, T087, T091; FR-006, FR-007, FR-054, FR-057a, FR-057b, FR-057e, FR-057f).
//
// Every test here is about a rule the schema and the DAO enforce together. A test with a mocked
// database would assert that the SQL was spelled the way the test expected it to be spelled.

const (
	lifecycleIncidentID = "inc-lifecycle-01"
	lifecyclePrincipal  = "oidc|alice"
)

var lifecycleAt = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

func newLifecycleFixture(ctx context.Context, t *testing.T, store *postgres.Store, id string) *investigationstore.InvestigationDAO {
	t.Helper()
	dao := investigationstore.NewInvestigationDAO(store)
	opened, err := dao.Open(ctx,
		investigationstore.Incident{
			IncidentID:             lifecycleIncidentID,
			CanonicalSubjectID:     "e:checkout",
			OpenedAt:               lifecycleAt,
			LastSymptomAt:          lifecycleAt,
			AssociationRuleVersion: intake.AssociationRuleVersion,
		},
		newInvestigation(id),
		declaredSymptom(id),
	)
	if err != nil {
		t.Fatalf("open investigation: %v", err)
	}
	if opened != id {
		t.Fatalf("open returned %q, want %q", opened, id)
	}
	return dao
}

func newInvestigation(id string) investigationstore.NewInvestigation {
	return investigationstore.NewInvestigation{
		InvestigationID:   id,
		IncidentID:        lifecycleIncidentID,
		ValidAt:           lifecycleAt,
		ObservedAt:        lifecycleAt,
		WindowStart:       lifecycleAt.Add(-90 * time.Minute),
		WindowEnd:         lifecycleAt,
		Profile:           "page",
		Requester:         lifecyclePrincipal,
		AlgebraVersion:    "1.0.0",
		LedgerRuleVersion: ledger.LedgerRuleVersion,
		SchemaVersion:     "1.0.0",
		StartedAt:         lifecycleAt,
	}
}

func declaredSymptom(id string) *investigationv1.Symptom {
	return &investigationv1.Symptom{
		SymptomId:         "sym-" + id,
		OriginSystem:      "slack:acme",
		OriginRef:         "#incident-checkout",
		Transport:         intake.TransportHumanDeclared,
		Statement:         "checkout is throwing 500s",
		FiredAt:           timestamppb.New(lifecycleAt),
		Origin:            investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
		Severity:          "sev2",
		Title:             "checkout is throwing 500s",
		DeclaringIdentity: lifecyclePrincipal,
		IdempotencyKey:    intake.DeclarationKey("slack:acme", "C0123STABLE-"+id, lifecycleAt),
		NamedIdentifiers:  []*graphv1.Ref{{Namespace: "k8s.service", Value: "shop/checkout"}},
		TargetRefs: []*investigationv1.TargetRef{{
			Ref:        &graphv1.Ref{Namespace: "k8s.service", Value: "shop/checkout"},
			Provenance: investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN,
		}},
		ResolvedEntityIds: []string{"e:checkout"},
	}
}

func TestOpenIsIdempotentOnThePublishedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-idem-01")

	// FR-008b: re-delivery of a symptom whose key has been seen returns the existing
	// investigation and opens no second one.
	second := newInvestigation("inv-idem-02")
	again, err := dao.Open(ctx, investigationstore.Incident{
		IncidentID: lifecycleIncidentID, CanonicalSubjectID: "e:checkout",
		OpenedAt: lifecycleAt, LastSymptomAt: lifecycleAt,
	}, second, declaredSymptom("inv-idem-01"))
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if again != "inv-idem-01" {
		t.Fatalf("re-delivery opened %q; FR-008b makes it a no-op returning inv-idem-01", again)
	}
	rows, err := dao.List(ctx, investigationstore.ListFilter{IncidentID: lifecycleIncidentID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("incident has %d investigations, want 1 (FR-008a)", len(rows))
	}
}

func TestLifecycleRunningToConcluded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-conclude-01")
	lc := investigationstore.NewLifecycleDAO(store)

	lifecycle, _, _, err := lc.LifecycleOf(ctx, "inv-conclude-01")
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if lifecycle != investigationstore.LifecycleRunning {
		t.Fatalf("lifecycle = %q, want running", lifecycle)
	}

	if err := lc.Conclude(ctx, "inv-conclude-01", investigationstore.Conclusion{
		Kind:              investigationstore.ConclusionFinal,
		Outcome:           investigationstore.OutcomeRanked,
		StopReason:        investigationv1.StopReason_COMPLETED,
		VerdictLine:       "shop/payments@rev7 caused it",
		RollbackCandidate: "k8s.change=shop/payments@rev7",
		EndedAt:           lifecycleAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("conclude: %v", err)
	}

	lifecycle, kind, outcome, err := lc.LifecycleOf(ctx, "inv-conclude-01")
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if lifecycle != investigationstore.LifecycleConcluded ||
		kind != investigationstore.ConclusionFinal ||
		outcome != investigationstore.OutcomeRanked {
		t.Fatalf("got (%s, %s, %s), want (concluded, final, ranked)", lifecycle, kind, outcome)
	}

	// FR-007, Invariant 6: a completed investigation is immutable. A second conclusion — a retry,
	// a duplicated callback — cannot rewrite the first one's verdict.
	err = lc.Conclude(ctx, "inv-conclude-01", investigationstore.Conclusion{
		Kind: investigationstore.ConclusionFinal, Outcome: investigationstore.OutcomeUnknown,
		VerdictLine: "actually we do not know",
	})
	if !errors.Is(err, investigationstore.ErrNotRunning) {
		t.Fatalf("second conclude = %v, want ErrNotRunning (FR-007)", err)
	}
	inv, err := dao.Get(ctx, "inv-conclude-01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if inv.GetVerdictLine() != "shop/payments@rev7 caused it" {
		t.Errorf("the verdict was rewritten to %q", inv.GetVerdictLine())
	}
	if inv.GetProvisional() {
		t.Error("a concluded investigation is still marked provisional")
	}
}

func TestBudgetExhaustionIsAlwaysAPartialConclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	newLifecycleFixture(ctx, t, store, "inv-budget-01")
	lc := investigationstore.NewLifecycleDAO(store)

	err := lc.Conclude(ctx, "inv-budget-01", investigationstore.Conclusion{
		Kind: investigationstore.ConclusionFinal, Outcome: investigationstore.OutcomeBudgetExhausted,
	})
	if !errors.Is(err, investigationstore.ErrBudgetExhaustedIsPartial) {
		t.Fatalf("error = %v, want ErrBudgetExhaustedIsPartial (FR-006)", err)
	}
	if err := lc.Conclude(ctx, "inv-budget-01", investigationstore.Conclusion{
		Kind:       investigationstore.ConclusionPartial,
		Outcome:    investigationstore.OutcomeBudgetExhausted,
		StopReason: investigationv1.StopReason_BUDGET_EXHAUSTED_STOP,
	}); err != nil {
		t.Fatalf("partial conclude: %v", err)
	}
}

func TestFailedIsATerminalStateWithNoConclusionKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	newLifecycleFixture(ctx, t, store, "inv-failed-01")
	lc := investigationstore.NewLifecycleDAO(store)

	if err := lc.Fail(ctx, "inv-failed-01", "the graph was unreachable"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	lifecycle, kind, outcome, err := lc.LifecycleOf(ctx, "inv-failed-01")
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if lifecycle != investigationstore.LifecycleFailed || outcome != investigationstore.OutcomeFailed {
		t.Fatalf("got (%s, %s), want (failed, failed)", lifecycle, outcome)
	}
	if kind != "" {
		t.Errorf("conclusion kind = %q on a failed run; a failure is not a conclusion", kind)
	}
}

// TestReopenIsTheOnlyTransitionOnAConcludedRow is FR-007 + FR-057b: the parent keeps its verdict
// and grows only a status; the work is a new linked row.
func TestReopenIsTheOnlyTransitionOnAConcludedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-reopen-parent")
	lc := investigationstore.NewLifecycleDAO(store)

	// Reopening a running investigation is refused: there is nothing concluded to reopen.
	if err := lc.MarkReopened(ctx, "inv-reopen-parent"); !errors.Is(err, investigationstore.ErrNotConcluded) {
		t.Fatalf("reopen while running = %v, want ErrNotConcluded", err)
	}
	if err := lc.Conclude(ctx, "inv-reopen-parent", investigationstore.Conclusion{
		Kind: investigationstore.ConclusionFinal, Outcome: investigationstore.OutcomeUnknown,
		VerdictLine: "no observed change explains this", EndedAt: lifecycleAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("conclude: %v", err)
	}

	child := newInvestigation("inv-reopen-child")
	child.ReopensInvestigationID = "inv-reopen-parent"
	fact, err := dao.ReopenWithFact(ctx, "inv-reopen-parent", child, &investigationv1.HumanFact{
		Kind:        investigationstore.FactKindCorrection,
		Statement:   "the affected services are shop/checkout and shop/payments",
		EntityIds:   []string{"e:checkout", "e:payments"},
		Author:      lifecyclePrincipal,
		SubmittedAt: timestamppb.New(lifecycleAt.Add(2 * time.Hour)),
	})
	if err != nil {
		t.Fatalf("reopen with fact: %v", err)
	}
	if fact.GetWeightClass() != investigationstore.WeightClassStrong {
		t.Errorf("weight class = %q, want strong and never decisive (FR-057a)", fact.GetWeightClass())
	}

	parent, err := dao.Get(ctx, "inv-reopen-parent")
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parent.GetLifecycle() != investigationv1.Lifecycle_REOPENED {
		t.Errorf("parent lifecycle = %v, want REOPENED", parent.GetLifecycle())
	}
	// "The concluded record MUST remain readable exactly as produced" (FR-057b).
	if parent.GetVerdictLine() != "no observed change explains this" {
		t.Errorf("the parent's verdict changed to %q", parent.GetVerdictLine())
	}
	if parent.GetOutcome() != investigationv1.InvestigationOutcome_UNKNOWN {
		t.Errorf("the parent's outcome changed to %v", parent.GetOutcome())
	}
	if len(parent.GetFacts()) != 0 {
		t.Errorf("the fact landed on the parent; it belongs to the child that will use it")
	}

	kid, err := dao.Get(ctx, "inv-reopen-child")
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if kid.GetReopensInvestigationId() != "inv-reopen-parent" {
		t.Errorf("the child does not link to its parent: %q", kid.GetReopensInvestigationId())
	}
	if kid.GetLifecycle() != investigationv1.Lifecycle_RUNNING {
		t.Errorf("child lifecycle = %v, want RUNNING: a reopened investigation runs again", kid.GetLifecycle())
	}
	if len(kid.GetFacts()) != 1 {
		t.Fatalf("the child carries %d facts, want 1", len(kid.GetFacts()))
	}
	if kid.GetFacts()[0].GetEvidenceId() == "" {
		t.Error("the fact did not become an evidence item (FR-057a)")
	}
}

func TestHumanFactNeverCarriesDecisiveWeight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-fact-01")

	_, err := dao.SubmitFact(ctx, "inv-fact-01", &investigationv1.HumanFact{
		Kind: investigationstore.FactKindManualAction, Statement: "I restarted a pod",
		Author: lifecyclePrincipal, WeightClass: "decisive",
	})
	if !errors.Is(err, investigationstore.ErrDecisiveHumanFact) {
		t.Fatalf("error = %v, want ErrDecisiveHumanFact: a person who is certain is still a "+
			"person (FR-057a)", err)
	}

	_, err = dao.SubmitFact(ctx, "inv-fact-01", &investigationv1.HumanFact{
		Kind: "hunch", Statement: "something feels off", Author: lifecyclePrincipal,
	})
	if !errors.Is(err, investigationstore.ErrUnknownFactKind) {
		t.Fatalf("error = %v, want ErrUnknownFactKind", err)
	}

	_, err = dao.SubmitFact(ctx, "inv-fact-01", &investigationv1.HumanFact{
		Kind: investigationstore.FactKindManualAction, Statement: "I restarted a pod",
	})
	if !errors.Is(err, investigationstore.ErrAnonymousPrincipal) {
		t.Fatalf("error = %v, want ErrAnonymousPrincipal (FR-066)", err)
	}
}

// TestHumanFactAgainstARunningInvestigationNeverBlocks is SC-024: the fact is written and the
// run is untouched. There is no waiting state to enter.
func TestHumanFactAgainstARunningInvestigationNeverBlocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-fact-running")
	lc := investigationstore.NewLifecycleDAO(store)

	fact, err := dao.SubmitFact(ctx, "inv-fact-running", &investigationv1.HumanFact{
		Kind:      investigationstore.FactKindVendorNotice,
		Statement: "the provider reported a regional incident from 14:10",
		Author:    lifecyclePrincipal,
		Concerns: &graphv1.Interval{
			Start: timestamppb.New(lifecycleAt.Add(-22 * time.Minute)),
		},
	})
	if err != nil {
		t.Fatalf("submit fact: %v", err)
	}
	lifecycle, _, _, err := lc.LifecycleOf(ctx, "inv-fact-running")
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if lifecycle != investigationstore.LifecycleRunning {
		t.Fatalf("lifecycle = %q after a fact; the engine never waits for a human (FR-006, FR-057)",
			lifecycle)
	}

	// The contradiction is stated, never resolved (FR-057a): both the fact and the evidence
	// that disagrees with it stay.
	if err := dao.RecordContradiction(ctx, fact.GetFactId(), "ev-metrics-42"); err != nil {
		t.Fatalf("record contradiction: %v", err)
	}
	facts, err := dao.FactsOf(ctx, "inv-fact-running")
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if len(facts) != 1 || len(facts[0].GetContradictedByEvidenceIds()) != 1 {
		t.Fatalf("contradiction not recorded: %+v", facts)
	}
	if facts[0].GetStatement() == "" {
		t.Error("the fact was dropped rather than kept beside the contradiction")
	}
}

func TestReviewAndLabelAreAdditiveAndAttributable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-review-01")
	lc := investigationstore.NewLifecycleDAO(store)
	if err := lc.Conclude(ctx, "inv-review-01", investigationstore.Conclusion{
		Kind: investigationstore.ConclusionFinal, Outcome: investigationstore.OutcomeRanked,
		VerdictLine: "shop/payments@rev7 caused it",
	}); err != nil {
		t.Fatalf("conclude: %v", err)
	}

	if _, err := dao.RecordReview(ctx, "inv-review-01", &investigationv1.HumanReview{
		ValidatedRootCause: "unobserved",
		Rationale:          "it was a client-side config push we do not observe",
		Author:             lifecyclePrincipal,
		DecidedAt:          timestamppb.New(lifecycleAt.Add(time.Hour)),
		Amendments: []*investigationv1.Hypothesis{{
			HypothesisId: "h-rev7", Status: investigationv1.HypothesisStatus_REFUTED, Rank: 3,
		}},
	}); err != nil {
		t.Fatalf("record review: %v", err)
	}
	// Additive, never an edit: a second review is a second row and the first stays.
	if _, err := dao.RecordReview(ctx, "inv-review-01", &investigationv1.HumanReview{
		ValidatedRootCause: "k8s.change=shop/payments@rev7",
		Rationale:          "on reflection the rollout was the cause after all",
		Author:             lifecyclePrincipal,
		DecidedAt:          timestamppb.New(lifecycleAt.Add(2 * time.Hour)),
	}); err != nil {
		t.Fatalf("second review: %v", err)
	}

	inv, err := dao.Get(ctx, "inv-review-01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(inv.GetReviews()) != 2 {
		t.Fatalf("reviews = %d, want 2: a review is additive, never an edit (FR-054)", len(inv.GetReviews()))
	}
	if inv.GetVerdictLine() != "shop/payments@rev7 caused it" {
		t.Errorf("a review rewrote the engine's verdict to %q", inv.GetVerdictLine())
	}
	if got := inv.GetReviews()[0].GetAmendments(); len(got) != 1 ||
		got[0].GetStatus() != investigationv1.HypothesisStatus_REFUTED {
		t.Errorf("amendments did not round-trip: %+v", got)
	}

	if _, err := dao.RecordReview(ctx, "inv-review-01", &investigationv1.HumanReview{
		ValidatedRootCause: "unobserved", Author: "",
	}); !errors.Is(err, investigationstore.ErrAnonymousPrincipal) {
		t.Errorf("an anonymous review was accepted (FR-066)")
	}

	if _, err := dao.RecordLabel(ctx, "inv-review-01", &investigationv1.Label{
		WasThisRight: true, Author: lifecyclePrincipal,
		LabelledAt: timestamppb.New(lifecycleAt.Add(3 * time.Hour)),
	}); err != nil {
		t.Fatalf("record label: %v", err)
	}
	inv, err = dao.Get(ctx, "inv-review-01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(inv.GetLabels()) != 1 || !inv.GetLabels()[0].GetWasThisRight() {
		t.Fatalf("label not recorded: %+v", inv.GetLabels())
	}

	agreement, err := dao.Agreement(ctx)
	if err != nil {
		t.Fatalf("agreement: %v", err)
	}
	if agreement.Reviewed == 0 {
		t.Error("agreement counts no reviewed investigation (FR-056)")
	}
	if agreement.Labelled != 1 || agreement.LabelledRight != 1 {
		t.Errorf("label counts = (%d, %d), want (1, 1)", agreement.Labelled, agreement.LabelledRight)
	}
}

// TestEvidenceRecordingKeepsEveryCall is Invariant 5 and FR-018a/FR-021.
func TestEvidenceRecordingKeepsEveryCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-evidence-01")
	recorder := investigationstore.NewEvidenceRecorder(dao)

	item := func(id string) ledger.EvidenceItem {
		return ledger.EvidenceItem{
			ID: id, Kind: investigationstore.EvidenceKindAlgebraAnswer,
			Worker: "metrics", Capability: "errors_by_version",
			ValidAt: lifecycleAt, ObservedAt: lifecycleAt, CalledAt: lifecycleAt,
			Mode: "recorded", Outcome: "digest", ResponseDigest: "sha256:abc",
			Coverage: &investigationv1.Coverage{DataSource: "metrics", VolumeConsidered: 4200},
			DeepLink: "https://metrics.invalid/q",
		}
	}
	call := func(seq int, id, evidenceID string) investigationstore.WorkerCall {
		return investigationstore.WorkerCall{
			CallID: id, Seq: seq, Worker: "metrics", Capability: "errors_by_version",
			TermKey: "term-identical", DiscriminatingQuestion: "did rev7 raise the error rate?",
			Mode: "recorded", Outcome: investigationstore.CallOutcomeAnswered,
			Duration: 40 * time.Millisecond, CostClass: "standard", Backend: "metrics-recorded",
			EvidenceID: evidenceID,
		}
	}

	// The same term twice: two calls, two evidence items, never deduplicated.
	if err := recorder.Record(ctx, "inv-evidence-01", call(0, "call-0", "ev-0"), item("ev-0")); err != nil {
		t.Fatalf("record first: %v", err)
	}
	if err := recorder.Record(ctx, "inv-evidence-01", call(1, "call-1", "ev-1"), item("ev-1")); err != nil {
		t.Fatalf("record second: %v", err)
	}
	chain, err := dao.EvidenceChain(ctx, "inv-evidence-01")
	if err != nil {
		t.Fatalf("evidence chain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("evidence chain has %d items; a repeated identical term is never deduplicated "+
			"(Invariant 5)", len(chain))
	}
	for _, e := range chain {
		if e.GetCoverage() == nil {
			t.Errorf("evidence %s has no coverage block (Invariant 4)", e.GetEvidenceId())
		}
		if e.GetDeepLink() == "" && e.GetDeepLinkAbsentReason() == "" {
			t.Errorf("evidence %s carries neither a deep link nor a reason (FR-057d)", e.GetEvidenceId())
		}
		if e.GetValidAt() == nil || e.GetObservedAt() == nil {
			t.Errorf("evidence %s is missing an instant in force (Invariant 7)", e.GetEvidenceId())
		}
	}

	// FR-018a: a call that serves no hypothesis is recorded as exploratory, with a reason.
	exploratory := call(2, "call-2", "ev-2")
	exploratory.DiscriminatingQuestion = investigationstore.Exploratory("scanning the neighbourhood")
	if err := recorder.Record(ctx, "inv-evidence-01", exploratory, item("ev-2")); err != nil {
		t.Fatalf("record exploratory: %v", err)
	}
	blank := call(3, "call-3", "")
	blank.DiscriminatingQuestion = ""
	if err := recorder.Record(ctx, "inv-evidence-01", blank, ledger.EvidenceItem{}); !errors.Is(
		err, investigationstore.ErrNoDiscriminatingQuestion) {
		t.Fatalf("error = %v, want ErrNoDiscriminatingQuestion (FR-018a)", err)
	}
}

// TestObservedTimePinRejectsEvidenceFromTheFuture is Invariant 7, enforced by the trigger.
func TestObservedTimePinRejectsEvidenceFromTheFuture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-pin-01")
	recorder := investigationstore.NewEvidenceRecorder(dao)

	late := ledger.EvidenceItem{
		ID: "ev-late", Kind: investigationstore.EvidenceKindAlgebraAnswer,
		Worker: "metrics", Capability: "compare",
		ValidAt: lifecycleAt, ObservedAt: lifecycleAt.Add(time.Hour), CalledAt: lifecycleAt,
		Mode: "live", Outcome: "digest",
		Coverage: &investigationv1.Coverage{DataSource: "metrics"},
		DeepLink: "https://metrics.invalid/q",
	}
	// The Go-side check names the rule before the database does.
	if err := investigationstore.CheckObservedPin(late, lifecycleAt, false); !errors.Is(
		err, investigationstore.ErrObservedAfterInvestigation) {
		t.Fatalf("CheckObservedPin = %v, want ErrObservedAfterInvestigation", err)
	}
	if err := investigationstore.CheckObservedPin(late, lifecycleAt, true); err != nil {
		t.Errorf("review mode is the recorded exception (FR-005); got %v", err)
	}
	// And the trigger refuses the row whatever the caller did.
	err := recorder.Record(ctx, "inv-pin-01", investigationstore.WorkerCall{
		CallID: "call-late", Seq: 0, Worker: "metrics", Capability: "compare",
		TermKey: "t", DiscriminatingQuestion: "q", Mode: "live",
		Outcome: investigationstore.CallOutcomeAnswered,
	}, late)
	if err == nil || !strings.Contains(err.Error(), "Invariant 7") {
		t.Fatalf("the database accepted evidence from after the pin: %v", err)
	}
}

func TestValidateEvidenceNamesTheRuleItBroke(t *testing.T) {
	t.Parallel()

	base := ledger.EvidenceItem{
		ID: "ev", ValidAt: lifecycleAt, ObservedAt: lifecycleAt,
		Coverage: &investigationv1.Coverage{DataSource: "metrics"},
		DeepLink: "https://metrics.invalid/q",
	}
	if err := investigationstore.ValidateEvidence(base); err != nil {
		t.Fatalf("a complete item was rejected: %v", err)
	}
	noCoverage := base
	noCoverage.Coverage = nil
	if err := investigationstore.ValidateEvidence(noCoverage); !errors.Is(
		err, investigationstore.ErrMissingCoverage) {
		t.Errorf("error = %v, want ErrMissingCoverage (FR-014a)", err)
	}
	noLink := base
	noLink.DeepLink = ""
	if err := investigationstore.ValidateEvidence(noLink); !errors.Is(
		err, investigationstore.ErrMissingDeepLink) {
		t.Errorf("error = %v, want ErrMissingDeepLink (FR-057d)", err)
	}
}

// --- report delivery (T087, FR-057f, constitution VII v1.1.0) ------------------------------

func TestDeliveryIsEditedInPlaceAndNeverBlocksAConclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-delivery-01")
	lc := investigationstore.NewLifecycleDAO(store)

	first, err := dao.RecordDelivery(ctx, investigationstore.DeliveryRecord{
		InvestigationID: "inv-delivery-01", TargetSystem: "slack:acme", TargetRef: "C0123STABLE",
		ExternalMessageRef: "msg-1", RenderingDigest: "sha256:aaa",
		Outcome: investigationstore.DeliveryDelivered, At: lifecycleAt,
	})
	if err != nil {
		t.Fatalf("record delivery: %v", err)
	}
	if first.GetUpdateCount() != 1 {
		t.Errorf("update count = %d, want 1", first.GetUpdateCount())
	}

	// A second delivery to the same place edits in place: one row, the same external reference,
	// a higher update count and the original first-delivered instant (FR-057f).
	second, err := dao.RecordDelivery(ctx, investigationstore.DeliveryRecord{
		InvestigationID: "inv-delivery-01", TargetSystem: "slack:acme", TargetRef: "C0123STABLE",
		RenderingDigest: "sha256:bbb",
		Outcome:         investigationstore.DeliveryDelivered, At: lifecycleAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if second.GetUpdateCount() != 2 {
		t.Errorf("update count = %d, want 2", second.GetUpdateCount())
	}
	if second.GetExternalMessageRef() != "msg-1" {
		t.Errorf("external message ref = %q, want the first one kept: a delivery is edited in "+
			"place, never re-posted (FR-057f)", second.GetExternalMessageRef())
	}
	if !second.GetFirstDeliveredAt().AsTime().Equal(lifecycleAt) {
		t.Errorf("first_delivered_at moved to %s", second.GetFirstDeliveredAt().AsTime())
	}
	deliveries, err := dao.DeliveriesOf(ctx, "inv-delivery-01")
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want one row per (investigation, target)", len(deliveries))
	}

	// A failed delivery is recorded and the investigation still concludes (FR-057f).
	if _, err := dao.RecordDelivery(ctx, investigationstore.DeliveryRecord{
		InvestigationID: "inv-delivery-01", TargetSystem: "slack:acme", TargetRef: "C0123STABLE",
		RenderingDigest: "sha256:ccc", Outcome: investigationstore.DeliveryFailed,
		FailureDetail: "the chat connector returned 503", At: lifecycleAt.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("failed delivery: %v", err)
	}
	if err := lc.Conclude(ctx, "inv-delivery-01", investigationstore.Conclusion{
		Kind: investigationstore.ConclusionFinal, Outcome: investigationstore.OutcomeRanked,
		VerdictLine: "shop/payments@rev7 caused it",
	}); err != nil {
		t.Fatalf("a failed delivery blocked the conclusion; FR-057f forbids that: %v", err)
	}

	// A failure with no detail is refused: a failure nobody can read is not a record.
	if _, err := dao.RecordDelivery(ctx, investigationstore.DeliveryRecord{
		InvestigationID: "inv-delivery-01", TargetSystem: "slack:acme", TargetRef: "C0999",
		RenderingDigest: "sha256:ddd", Outcome: investigationstore.DeliveryFailed,
	}); !errors.Is(err, investigationstore.ErrDeliveryFailureNeedsDetail) {
		t.Errorf("error = %v, want ErrDeliveryFailureNeedsDetail", err)
	}
}

func TestOpenIncidentsFeedsTheAssociationRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	newLifecycleFixture(ctx, t, store, "inv-open-incidents")
	dao := investigationstore.NewInvestigationDAO(store)

	open, err := dao.OpenIncidents(ctx, lifecycleAt.Add(5*time.Minute), 10*time.Minute)
	if err != nil {
		t.Fatalf("open incidents: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open incidents = %d, want 1", len(open))
	}
	if open[0].IncidentID != lifecycleIncidentID {
		t.Errorf("incident = %q, want %q", open[0].IncidentID, lifecycleIncidentID)
	}
	if len(open[0].EntityIDs) != 1 || open[0].EntityIDs[0] != "e:checkout" {
		t.Errorf("entities = %v, want the symptom's resolved entity", open[0].EntityIDs)
	}
	// Far outside the window: nothing to attach to.
	far, err := dao.OpenIncidents(ctx, lifecycleAt.Add(6*time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("open incidents: %v", err)
	}
	if len(far) != 0 {
		t.Errorf("open incidents six hours later = %d, want 0", len(far))
	}
}

// TestADirectUpdateOfAConcludedRowIsRefusedByTheDatabase is FR-007 as 0008 enforces it.
//
// Until 0008, immutability was a guarded UPDATE in this DAO: every statement that touched a
// non-running row carried `AND lifecycle = 'running'` in its WHERE clause, which is exactly as
// strong as every future author remembering to write it. This test goes round the DAO entirely
// and writes the UPDATE by hand — which is what a migration, a maintenance script or a new code
// path would do — and asserts the database refuses it. The reopen transition, in the same test,
// still goes through, because that is the one change a concluded row takes (FR-057b).
func TestADirectUpdateOfAConcludedRowIsRefusedByTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-immutable-01")
	lc := investigationstore.NewLifecycleDAO(store)

	// While it is running, a direct update is ordinary: a run in flight is written to constantly.
	if _, err := store.Pool().Exec(ctx,
		`UPDATE investigation.investigations SET verdict_line = $2 WHERE investigation_id = $1`,
		"inv-immutable-01", "still working"); err != nil {
		t.Fatalf("update a running investigation: %v", err)
	}

	if err := lc.Conclude(ctx, "inv-immutable-01", investigationstore.Conclusion{
		Kind: investigationstore.ConclusionFinal, Outcome: investigationstore.OutcomeRanked,
		VerdictLine: "payments@rev7 caused it", RollbackCandidate: "k8s.change=shop/payments@rev7",
		EndedAt: lifecycleAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("conclude: %v", err)
	}

	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{
			"rewriting the verdict",
			`UPDATE investigation.investigations SET verdict_line = $2 WHERE investigation_id = $1`,
			[]any{"inv-immutable-01", "actually it was the config change"},
		},
		{
			"quietly changing the outcome",
			`UPDATE investigation.investigations SET outcome = $2 WHERE investigation_id = $1`,
			[]any{"inv-immutable-01", investigationstore.OutcomeUnknown},
		},
		{
			"reopening while also editing something else",
			`UPDATE investigation.investigations SET lifecycle = 'reopened', verdict_line = $2
			 WHERE investigation_id = $1`,
			[]any{"inv-immutable-01", "edited on the way past"},
		},
		{
			"moving it back to running",
			`UPDATE investigation.investigations SET lifecycle = 'running' WHERE investigation_id = $1`,
			[]any{"inv-immutable-01"},
		},
	} {
		if _, err := store.Pool().Exec(ctx, tc.sql, tc.args...); err == nil {
			t.Errorf("%s was accepted on a concluded investigation (FR-007)", tc.name)
		}
	}

	// The verdict is exactly as produced.
	inv, err := dao.Get(ctx, "inv-immutable-01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if inv.GetVerdictLine() != "payments@rev7 caused it" {
		t.Errorf("the verdict is now %q; a concluded record reads exactly as produced", inv.GetVerdictLine())
	}

	// And the one permitted transition still goes through.
	if err := lc.MarkReopened(ctx, "inv-immutable-01"); err != nil {
		t.Fatalf("the reopen transition was refused: %v", err)
	}
	inv, err = dao.Get(ctx, "inv-immutable-01")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if inv.GetLifecycle() != investigationv1.Lifecycle_REOPENED {
		t.Errorf("lifecycle = %v after reopen, want REOPENED", inv.GetLifecycle())
	}
	if inv.GetVerdictLine() != "payments@rev7 caused it" {
		t.Errorf("the reopen changed the verdict to %q", inv.GetVerdictLine())
	}
}
