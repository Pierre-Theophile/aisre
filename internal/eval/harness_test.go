// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The harness over the corpus's own MVP incident (T107, T111).
//
// These tests run the real engine over the real fixture — its replayed graph and its recorded
// world — because everything this package publishes is a claim about what a run produces, and a
// claim about a run tested only against a hand-built `RunOutcome` is a claim about the test.

func TestMain(m *testing.M) { pgtest.TestMain(m) }

const (
	repoRoot        = "../.."
	incidentFixture = repoRoot + "/fixtures/incidents/rollout-regression-01-incident"
	modelYAML       = repoRoot + "/config/model.yaml"
	pricesYAML      = repoRoot + "/config/prices.yaml"
	auditPath       = repoRoot + "/docs/evaluation/coverage-audit-2026-09.json"
)

// storeFactory hands the harness a database of its own, built by pgtest rather than by a DSN, so
// the test needs no environment.
func storeFactory(t *testing.T) fixture.StoreFactory {
	t.Helper()
	return func(context.Context) (*postgres.Store, func(), error) {
		return pgtest.Open(t), func() {}, nil
	}
}

func newHarness(t *testing.T, kind string) *eval.Harness {
	t.Helper()
	harness, err := eval.NewHarness(context.Background(), eval.Options{
		Dir:        incidentFixture,
		NewStore:   storeFactory(t),
		AuditPath:  auditPath,
		Kind:       kind,
		ModelYAML:  modelYAML,
		PricesYAML: pricesYAML,
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	t.Cleanup(harness.Close)
	return harness
}

// TestTheHarnessInvestigatesTheFixtureAndGathersWhatAScorerNeeds is the shape test for
// `RunOutcome`: every field a report row is computed from has to be filled in by one run, or the
// scorer would have to re-run the engine to read it.
func TestTheHarnessInvestigatesTheFixtureAndGathersWhatAScorerNeeds(t *testing.T) {
	outcome, err := newHarness(t, eval.KindModelFree).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if outcome.FixtureID != "rollout-regression-01-incident" {
		t.Errorf("fixture id %q", outcome.FixtureID)
	}
	if outcome.RunIndex != 1 {
		t.Errorf("run index %d, want 1: pass@1 is the first run", outcome.RunIndex)
	}
	if outcome.ModelConfigDigest == "" {
		t.Error("no model configuration digest; FR-061 requires the configuration to be recorded on every run")
	}
	if len(outcome.ModelIDs) != 0 {
		t.Errorf("a model-free run recorded model ids %v; it calls no model", outcome.ModelIDs)
	}
	if len(outcome.Ledger) == 0 {
		t.Fatal("the outcome carries no ledger; there is nothing to score")
	}
	if outcome.StopReason == "" || outcome.Outcome == "" {
		t.Errorf("stop %q outcome %q; both are published (FR-045b)", outcome.StopReason, outcome.Outcome)
	}
	if outcome.TrajectoryDigest == "" || outcome.RunID == "" {
		t.Error("the run identifies no recording")
	}
	if outcome.WorkerCallsTotal == 0 {
		t.Error("no worker calls were counted; the first wave issues several")
	}
	if outcome.Spend == nil {
		t.Error("no spend report; SC-008 publishes worker calls and cost per investigation")
	}
	if len(outcome.Citations) == 0 {
		t.Error("no citations; citation validity is computed over them")
	}
	cited := 0
	for _, citation := range outcome.Citations {
		if len(citation.Digest) > 0 {
			cited++
		}
	}
	if cited == 0 {
		t.Error("no citation carries the digest it addresses; the decisive-evidence predicates are " +
			"evaluated against those, and a citation with no answer behind it cannot be checked")
	}
	if outcome.Onset == nil {
		t.Error("no onset estimate; the causal ordering rests on one")
	}
	t.Logf("verdict %q (%s, %.4f), stop %s, %d worker calls, miss rate %.4f, %d citations",
		outcome.Verdict, outcome.VerdictBucket, outcome.VerdictConfidence, outcome.StopReason,
		outcome.WorkerCallsTotal, outcome.MissRate, len(outcome.Citations))
}

// TestTheDeterministicEngineAnswersTheSameThingEveryRun.
//
// The property the run policy rests on: a model-free run over a replayed graph and a recorded
// world is a function of the fixture. If it were not, `extend to 7 when the first 3 disagree`
// would fire on every fixture and the extension would measure the harness rather than the engine.
func TestTheDeterministicEngineAnswersTheSameThingEveryRun(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	set, err := harness.RunUnderPolicy(context.Background(), eval.DefaultRunPolicy())
	if err != nil {
		t.Fatalf("RunUnderPolicy: %v", err)
	}
	if len(set.Outcomes) != eval.DefaultRuns {
		t.Fatalf("%d runs, want %d: the deterministic engine agrees with itself and is not extended",
			len(set.Outcomes), eval.DefaultRuns)
	}
	if set.Disagreed {
		t.Errorf("the three runs disagreed: %v", set.Verdicts())
	}
	first := set.First()
	for _, outcome := range set.Outcomes[1:] {
		if outcome.TrajectoryDigest != first.TrajectoryDigest {
			t.Errorf("run %d recorded a different trajectory (%s) from run 1 (%s)",
				outcome.RunIndex, outcome.TrajectoryDigest, first.TrajectoryDigest)
		}
	}
	if set.ModelConfigDigest == "" || set.ModelConfigMismatch {
		t.Error("the set does not state one model configuration; FR-061 requires it recorded")
	}
}

// TestTheFakeModelRunRecordsTheModelItCalled: the second wiring, which is what puts model records
// into the corpus at all.
func TestTheFakeModelRunRecordsTheModelItCalled(t *testing.T) {
	outcome, err := newHarness(t, eval.KindFakeModel).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(outcome.ModelIDs) == 0 {
		t.Error("a fake-model run recorded no model ids; the run goes through the real client")
	}
	t.Logf("verdict %q, models %v, digest %s", outcome.Verdict, outcome.ModelIDs, outcome.ModelConfigDigest)
}

// TestGradingTheCorpusFixtureAtItsOwnInstants is the knowability rule against the real run
// (T111, FR-061b, SC-007).
//
// `rollout-regression-01-incident` fires at 14:32 and its decisive evidence exists at 14:25:30,
// when the first five-minute aggregation window containing the 14:20 onset is observed. That
// makes it the **ordinary** case: asked at the alert instant the culprit must be named, and
// asked at any instant before 14:25:30 — a minute after the rollout, say — `unknown` is the
// correct answer and naming the culprit is a guess scored as one.
//
// It used to be the other way round, because the fixture's decisive windows ran to 14:35, three
// minutes past the instant the investigation observes the world at. Nobody sees the future
// (constitution II, Phase 8 Track K-B): those windows now end at `fired_at`, and a knowability
// instant that rested on them had to move with them. Both directions are still asserted, so a
// grader that ignored the instant could not pass this test in either.
func TestGradingTheCorpusFixtureAtItsOwnInstants(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	outcome, err := harness.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	truth := harness.Manifest().Incident.GroundTruth
	question := harness.Manifest().Incident.Question

	if !truth.KnowabilityTime.Before(question.FiredAt) {
		t.Fatalf("the knowability instant %s is not before the alert instant %s; the decisive evidence "+
			"of this fixture is stated over windows that end at the alert instant, so it cannot become "+
			"knowable after it", truth.KnowabilityTime, question.FiredAt)
	}

	// One minute after the rollout and well before the five sustained points that settle it.
	early := truth.KnowabilityTime.Add(-5 * time.Minute)
	before := eval.Grade(outcome, truth, early)
	if !before.BeforeKnowability {
		t.Fatalf("asked at %s, before the knowability instant %s, the grader did not say so",
			early, truth.KnowabilityTime)
	}
	if before.Expected != eval.VerdictUnknown {
		t.Errorf("asked before knowability the expected answer is %q, not %q",
			eval.VerdictUnknown, before.Expected)
	}
	if before.Pass != (outcome.Verdict == eval.VerdictUnknown) {
		t.Errorf("asked at %s the run answered %q and graded pass=%t; before the knowability instant "+
			"exactly `unknown` passes", early.Format("15:04"), outcome.Verdict, before.Pass)
	}

	// The alert instant is now on the ordinary side of the rule, which is the case the corpus
	// actually runs: `internal/cli` grades every fixture at its own `fired_at`.
	after := eval.Grade(outcome, truth, question.FiredAt)
	if after.BeforeKnowability {
		t.Error("graded at the alert instant, the question is reported as before knowability")
	}
	if after.Expected != truth.Culprit {
		t.Errorf("asked after knowability the expected answer is the culprit %q, got %q",
			truth.Culprit, after.Expected)
	}
	if len(after.Decoys) != len(truth.Decoys) {
		t.Errorf("%d decoys graded, %d declared", len(after.Decoys), len(truth.Decoys))
	}
	if len(after.Decisive) != len(truth.DecisiveEvidence) {
		t.Errorf("%d decisive predicates evaluated, %d declared",
			len(after.Decisive), len(truth.DecisiveEvidence))
	}
	for _, predicate := range after.Decisive {
		t.Logf("decisive %s %s %v: satisfied=%t %s",
			predicate.FieldPath, predicate.Op, predicate.Value, predicate.Satisfied, predicate.Detail)
	}
	for _, decoy := range after.Decoys {
		t.Logf("decoy %s (%s): named=%t reason_matches=%t %s",
			decoy.Entity, decoy.CausalRole, decoy.Named, decoy.ReasonMatches, decoy.Detail)
	}
	t.Logf("after knowability: pass=%t verdict=%q failures=%v", after.Pass, after.Got, after.Failures)
}

// TestGradingIsDeterministic: the same run graded twice grades the same. A grader that read a map
// in iteration order would pass once and fail the build the next morning.
func TestGradingIsDeterministic(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	outcome, err := harness.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	truth := harness.Manifest().Incident.GroundTruth
	first := eval.Grade(outcome, truth, truth.KnowabilityTime)
	for i := 0; i < 5; i++ {
		again := eval.Grade(outcome, truth, truth.KnowabilityTime)
		if again.Pass != first.Pass || len(again.Failures) != len(first.Failures) {
			t.Fatalf("grade %d differs from grade 1: %+v vs %+v", i+2, again, first)
		}
	}
}

// TestTheDecisiveEvidenceIsSatisfiableAgainstWhatTheRunCited.
//
// The predicate evaluator against real digests, which is the only way to know the field paths in
// the corpus address anything at all. A predicate that resolves to nothing in every cited answer
// is either a fixture bug or an evaluator bug, and both are worth failing over — but the
// assertion is deliberately "at least one", because whether *every* predicate is satisfiable
// depends on what the engine chose to ask, and that is what the report grades rather than what
// this package guarantees.
func TestTheDecisiveEvidenceIsSatisfiableAgainstWhatTheRunCited(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	outcome, err := harness.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	truth := harness.Manifest().Incident.GroundTruth

	satisfied := 0
	for _, predicate := range truth.DecisiveEvidence {
		result := eval.SatisfiedBy(predicate, outcome.Citations)
		if result.Satisfied {
			satisfied++
			continue
		}
		t.Logf("not satisfied: %s %s %v over %s — %s",
			result.FieldPath, result.Op, result.Value, result.Term, result.Detail)
	}
	if satisfied == 0 {
		t.Errorf("none of %s's %d decisive predicates is satisfiable against the %d answers the run "+
			"cited; either the field paths address nothing or the run cited nothing",
			harness.Manifest().ID, len(truth.DecisiveEvidence), len(outcome.Citations))
	}
	t.Logf("%d of %d decisive predicates satisfied by the run's own citations",
		satisfied, len(truth.DecisiveEvidence))
}

// TestAHarnessRefusesAFixtureWithNoIncidentBlock: a 001 fixture has no question, and a harness
// that quietly investigated nothing would report a clean `unknown` for it.
func TestAHarnessRefusesAFixtureWithNoIncidentBlock(t *testing.T) {
	_, err := eval.NewHarness(context.Background(), eval.Options{
		Dir:      filepath.Join(repoRoot, "fixtures", "rollout-regression-01"),
		NewStore: storeFactory(t),
	})
	if err == nil {
		t.Fatal("a fixture with no `incident:` block was accepted")
	}
	t.Logf("refused: %v", err)
}
