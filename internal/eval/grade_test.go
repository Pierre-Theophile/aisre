// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// The grading rules and the run policy, over hand-built outcomes (T108, T111).
//
// Nothing here opens a database. The harness tests run the real engine and assert what a real
// run produces; these assert what the *rules* do with a run, including the runs the corpus does
// not currently produce — a culprit-deleted variant that names a decoy, three runs that disagree,
// a predicate over a field that is not there. A rule tested only against the runs that exist
// today is a rule that stops holding the first time the engine improves.

const (
	culprit = "k8s.change=shop/payments@rev7"
	decoy   = "k8s.change=shop/storefront@scale-1415"
	knowAt  = "2026-09-01T14:35:00Z"
	firedAt = "2026-09-01T14:32:00Z"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// truthNaming builds the smallest well-formed ground truth that names a culprit.
func truthNaming(t *testing.T, name string) eval.GroundTruth {
	t.Helper()
	return eval.GroundTruth{
		Culprit: name,
		CausalPath: []fixture.CausalStep{
			{Entity: name, Via: "changed-by", Role: "cause"},
			{Entity: "otel.service.name=checkout", Role: "symptom"},
		},
		KnowabilityTime: mustTime(t, knowAt),
	}
}

// outcomeNaming builds an outcome whose verdict is the given change.
func outcomeNaming(name string) *eval.RunOutcome {
	return &eval.RunOutcome{
		FixtureID: "rollout-regression-01-incident",
		RunIndex:  1,
		Verdict:   name,
		Outcome:   "ranked",
		CausalPath: []eval.PathStep{
			{Entity: name, Via: "changed-by", Role: "cause"},
			{Entity: "otel.service.name=checkout", Role: "symptom"},
		},
		Ledger: []eval.Hypothesis{{
			ID: "h-1", Rank: 1, Kind: "change", Status: "supported",
			CandidateChangeEntityID: name, Posterior: 0.9, Bucket: "very_high",
		}},
	}
}

func outcomeUnknown() *eval.RunOutcome {
	return &eval.RunOutcome{
		FixtureID: "rollout-regression-01-incident",
		RunIndex:  1,
		Verdict:   eval.VerdictUnknown,
		Outcome:   eval.VerdictUnknown,
		CausalPath: []eval.PathStep{
			{Entity: "otel.service.name=checkout", Role: "symptom"},
		},
	}
}

func outcomeUnobserved() *eval.RunOutcome {
	out := outcomeUnknown()
	out.Verdict = eval.VerdictUnobserved
	out.Outcome = "ranked"
	out.Ledger = []eval.Hypothesis{{
		ID: "h-open", Rank: 1, Kind: "no_observed_change", Status: "supported",
		Posterior: 0.8, Bucket: "high",
	}}
	return out
}

// TestKnowabilityMakesUnknownTheRightAnswer is FR-061b's rule in both directions.
func TestKnowabilityMakesUnknownTheRightAnswer(t *testing.T) {
	truth := truthNaming(t, culprit)
	before, after := mustTime(t, firedAt), mustTime(t, knowAt)

	for _, tc := range []struct {
		name    string
		outcome *eval.RunOutcome
		askedAt time.Time
		want    bool
	}{
		{"unknown before knowability passes", outcomeUnknown(), before, true},
		{"naming the culprit before knowability fails", outcomeNaming(culprit), before, false},
		{"naming a decoy before knowability fails", outcomeNaming(decoy), before, false},
		{"unknown after knowability fails", outcomeUnknown(), after, false},
		{"naming the culprit after knowability passes", outcomeNaming(culprit), after, true},
		{"naming a decoy after knowability fails", outcomeNaming(decoy), after, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eval.Grade(tc.outcome, truth, tc.askedAt)
			if got.Pass != tc.want {
				t.Errorf("pass=%t want %t; expected %q, got %q, failures %v",
					got.Pass, tc.want, got.Expected, got.Got, got.Failures)
			}
			if !got.Pass && len(got.Failures) == 0 {
				t.Error("a failing grade states no reason; a gate nobody can read is a gate nobody fixes")
			}
		})
	}
}

// TestTheKnowabilityFailureNamesTheGuess: the message has to say *why* a right answer failed, or
// the next reader will "fix" the engine by making it guess earlier.
func TestTheKnowabilityFailureNamesTheGuess(t *testing.T) {
	got := eval.Grade(outcomeNaming(culprit), truthNaming(t, culprit), mustTime(t, firedAt))
	if got.Pass {
		t.Fatal("naming the culprit before it was knowable passed")
	}
	if len(got.Failures) == 0 || !contains(got.Failures[0], "guess") {
		t.Errorf("the failure does not say the answer was a guess: %v", got.Failures)
	}
}

// TestUnobservedGroundTruthNeedsTheSymptomLocalised is SC-023: `unobserved` with nothing behind
// it is a shrug.
func TestUnobservedGroundTruthNeedsTheSymptomLocalised(t *testing.T) {
	truth := eval.GroundTruth{
		Culprit:         fixture.CulpritUnobserved,
		CausalPath:      []fixture.CausalStep{{Entity: "otel.service.name=checkout", Role: "symptom"}},
		KnowabilityTime: mustTime(t, knowAt),
	}
	after := mustTime(t, knowAt)

	if got := eval.Grade(outcomeUnobserved(), truth, after); !got.Pass {
		t.Errorf("`unobserved` with the symptom localised failed: %v", got.Failures)
	}
	if got := eval.Grade(outcomeNaming(culprit), truth, after); got.Pass {
		t.Error("naming a change where the truth is `unobserved` passed")
	}

	unlocalised := outcomeUnobserved()
	unlocalised.CausalPath = nil
	got := eval.Grade(unlocalised, truth, after)
	if got.Pass {
		t.Error("`unobserved` localising nothing passed")
	}
	if got.Localised {
		t.Error("an outcome with no causal path is reported as localised")
	}
}

// TestNotChangeInducedExpectsItsCategory: the two spellings of the unobservable remainder are
// the same answer to "did an observed change cause this", and the category is a label the fixture
// carries.
func TestNotChangeInducedExpectsItsCategory(t *testing.T) {
	truth := eval.GroundTruth{
		Culprit:         "not_change_induced: saturation",
		CausalPath:      []fixture.CausalStep{{Entity: "otel.service.name=checkout", Role: "symptom"}},
		KnowabilityTime: mustTime(t, knowAt),
	}
	got := eval.Grade(outcomeUnobserved(), truth, mustTime(t, knowAt))
	if !got.Pass {
		t.Errorf("an `unobserved` answer failed a not_change_induced ground truth: %v", got.Failures)
	}
	if got.Expected != "not_change_induced: saturation" {
		t.Errorf("expected verdict %q, want the category spelled out", got.Expected)
	}
}

// TestNamingADecoyFailsAndTheReasonTableIsReportedSeparately.
//
// Two different failures, and the grade keeps them apart: naming a decoy is a wrong answer and is
// in `Pass`; ruling one out for the wrong reason is a finding about *how*, and is in
// `DecoyReasonsMatch`.
func TestNamingADecoyFailsAndTheReasonTableIsReportedSeparately(t *testing.T) {
	truth := truthNaming(t, culprit)
	truth.Decoys = []fixture.Decoy{{Entity: decoy, CausalRole: "coincident", Why: "unrelated"}}
	after := mustTime(t, knowAt)

	named := outcomeNaming(decoy)
	if got := eval.Grade(named, truth, after); got.Pass {
		t.Error("naming a declared decoy passed")
	} else if len(got.Decoys) != 1 || !got.Decoys[0].Named {
		t.Errorf("the decoy was not reported as named: %+v", got.Decoys)
	}

	// The culprit named, the decoy present but never tested: the verdict is right and the
	// reason is not.
	right := outcomeNaming(culprit)
	right.Ledger = append(right.Ledger, eval.Hypothesis{
		ID: "h-2", Rank: 2, Kind: "change", Status: "proposed",
		CandidateChangeEntityID: decoy, Posterior: 0.05, Bucket: "very_low",
	})
	got := eval.Grade(right, truth, after)
	if !got.Pass {
		t.Errorf("the right verdict failed on a decoy reason: %v", got.Failures)
	}
	if got.DecoyReasonsMatch || got.PassWithReasons {
		t.Error("an untested `coincident` decoy was reported as ruled out for the right reason")
	}

	// The same decoy, now tested: the contract's substance is met.
	right.Ledger[1].Status = "inconclusive"
	right.Ledger[1].JudgmentIDs = []string{"j-1", "j-2"}
	got = eval.Grade(right, truth, after)
	if !got.DecoyReasonsMatch || !got.PassWithReasons {
		t.Errorf("a tested `coincident` decoy was not accepted: %+v", got.Decoys)
	}
}

// TestAnOutOfScopeDecoyMustNotBeInvented: the one role whose expected behaviour is absence.
func TestAnOutOfScopeDecoyMustNotBeInvented(t *testing.T) {
	truth := truthNaming(t, culprit)
	truth.Decoys = []fixture.Decoy{{Entity: "k8s.change=shop/reporting@rev3", CausalRole: "out_of_scope"}}
	after := mustTime(t, knowAt)

	clean := outcomeNaming(culprit)
	if got := eval.Grade(clean, truth, after); !got.DecoyReasonsMatch {
		t.Errorf("an out-of-scope change that never became a candidate was marked a mismatch: %+v", got.Decoys)
	}

	invented := outcomeNaming(culprit)
	invented.Ledger = append(invented.Ledger, eval.Hypothesis{
		ID: "h-9", Rank: 4, Kind: "change", Status: "proposed",
		CandidateChangeEntityID: "k8s.change=shop/reporting@rev3",
	})
	if got := eval.Grade(invented, truth, after); got.DecoyReasonsMatch {
		t.Error("an out-of-scope change ranked as a candidate was accepted; it is excluded, not out-argued")
	}
}

// ---------- the predicate evaluator ----------

// TestThePredicateEvaluatorOverADigest is the table-driven test of the field-path grammar and the
// comparison set, over a digest shaped like the ones the corpus actually produces.
func TestThePredicateEvaluatorOverADigest(t *testing.T) {
	digest := json.RawMessage(`{
	  "errors_by_version": {
	    "versions": [
	      {"version": "rev6", "errors": "2",  "total": "1000", "error_rate": 0.002,
	       "join_keys": {"version": "rev6"}},
	      {"version": "rev7", "errors": "107","total": "1000", "error_rate": 0.107,
	       "join_keys": {"version": "rev7"}}
	    ],
	    "version_attribute": "service.version"
	  },
	  "coverage": {"data_source": "metrics"}
	}`)
	spans := json.RawMessage(`{
	  "trace": {"groups": [
	    {"operation": "POST /charge", "error_kind": "deadline_exceeded",
	     "join_keys": {"version": "rev7"}, "count": "31"},
	    {"operation": "POST /charge", "error_kind": "connection_reset",
	     "join_keys": {"version": "rev6"}, "count": "2"}
	  ]}
	}`)
	onset := json.RawMessage(`{"onset": {"estimated_onset": "2026-09-01T14:20:00Z",
	   "uncertainty_seconds": "120", "method": "SEASONAL_CUSUM"}}`)
	nodes := json.RawMessage(`{"graph": {"nodes": [
	   {"display_name": "checkout"}, {"display_name": "payments"}]}}`)

	for _, tc := range []struct {
		name   string
		digest json.RawMessage
		path   string
		op     string
		value  any
		want   bool
	}{
		{"a selected element's number is above a bound", digest, "versions[version=rev7].error_rate", "gt", 0.05, true},
		{"and the other version is not", digest, "versions[version=rev6].error_rate", "gt", 0.05, false},
		{"lt over the same field", digest, "versions[version=rev6].error_rate", "lt", 0.05, true},
		{"gte at the boundary", digest, "versions[version=rev7].error_rate", "gte", 0.107, true},
		{"lte at the boundary", digest, "versions[version=rev7].error_rate", "lte", 0.107, true},
		{"ne over a number", digest, "versions[version=rev7].error_rate", "ne", 0.5, true},
		{"eq over a string field", digest, "version_attribute", "eq", "service.version", true},
		{"a nested selector key", spans, "groups[error_kind=deadline_exceeded].join_keys.version", "eq", "rev7", true},
		{"the same selector, the wrong version", spans, "groups[error_kind=connection_reset].join_keys.version", "eq", "rev7", false},
		{"a star selector reaches every element", nodes, "nodes[*].display_name", "contains", "payments", true},
		{"not_contains over a star selector", nodes, "nodes[*].display_name", "not_contains", "reporting", true},
		{"an instant compares as an instant", onset, "estimated_onset", "eq", mustTime(t, "2026-09-01T14:20:00Z"), true},
		{"an instant that is not the labelled one", onset, "estimated_onset", "eq", mustTime(t, "2026-09-01T14:25:00Z"), false},
		{"an instant ordering", onset, "estimated_onset", "lt", mustTime(t, "2026-09-01T14:32:00Z"), true},
		{"present over a field that is there", onset, "estimated_onset", "present", nil, true},
		{"absent over a field that is not", onset, "unavailable_reason", "absent", nil, true},
		{"absent over a field that is", onset, "estimated_onset", "absent", nil, false},
		{"a field path that addresses nothing", digest, "versions[version=rev9].error_rate", "gt", 0.0, false},
		{"a selector over a field that is not a list", digest, "version_attribute[a=b].c", "eq", "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			predicate := fixture.EvidencePredicate{
				Term:      map[string]any{familyOf(tc.digest): nil},
				FieldPath: tc.path,
				Op:        tc.op,
				Value:     tc.value,
			}
			got := eval.SatisfiedBy(predicate, []eval.Citation{{
				EvidenceID: "e-1", Term: familyOf(tc.digest), Digest: tc.digest,
			}})
			if got.Satisfied != tc.want {
				t.Errorf("satisfied=%t want %t (%s)", got.Satisfied, tc.want, got.Detail)
			}
			if got.Satisfied && got.EvidenceID != "e-1" {
				t.Errorf("a satisfied predicate names evidence %q", got.EvidenceID)
			}
		})
	}
}

// familyOf reads the single wrapper key of a test digest, which is the algebra family a citation
// of it carries.
func familyOf(digest json.RawMessage) string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(digest, &object); err != nil {
		return ""
	}
	for _, key := range []string{"errors_by_version", "onset", "compare", "graph"} {
		if _, ok := object[key]; ok {
			return key
		}
	}
	if _, ok := object["trace"]; ok {
		return "error_spans"
	}
	if _, ok := object["metric"]; ok {
		return "compare"
	}
	return ""
}

// TestAPredicateIsOnlyCheckedAgainstItsOwnFamily: a predicate about `errors_by_version` satisfied
// by a `compare` answer would be satisfied by a coincidence of field names.
func TestAPredicateIsOnlyCheckedAgainstItsOwnFamily(t *testing.T) {
	predicate := fixture.EvidencePredicate{
		Term:      map[string]any{"errors_by_version": nil},
		FieldPath: "versions[version=rev7].error_rate",
		Op:        "gt",
		Value:     0.05,
	}
	got := eval.SatisfiedBy(predicate, []eval.Citation{{
		EvidenceID: "e-1",
		Term:       "compare",
		Digest:     json.RawMessage(`{"versions": [{"version": "rev7", "error_rate": 0.9}]}`),
	}})
	if got.Satisfied {
		t.Error("a compare answer satisfied an errors_by_version predicate")
	}
	if !contains(got.Detail, "no errors_by_version answer") {
		t.Errorf("the detail does not say what was missing: %q", got.Detail)
	}
}

// TestAMalformedFieldPathIsReportedRatherThanSilentlyFalse.
func TestAMalformedFieldPathIsReportedRatherThanSilentlyFalse(t *testing.T) {
	for _, path := range []string{"", "versions[version=rev7", "[version=rev7].error_rate"} {
		if _, err := eval.Resolve(map[string]any{}, path); err == nil {
			t.Errorf("field path %q was accepted", path)
		}
	}
}

// ---------- the run policy ----------

// TestThreeRunsExtendToSevenOnlyWhenTheyDisagree is FR-061's policy, in both directions.
func TestThreeRunsExtendToSevenOnlyWhenTheyDisagree(t *testing.T) {
	agreeing := func(context.Context, int) (*eval.RunOutcome, error) { return outcomeNaming(culprit), nil }
	set, err := eval.DefaultRunPolicy().Execute(context.Background(), "f", agreeing)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(set.Outcomes) != eval.DefaultRuns || set.Extended || set.Disagreed {
		t.Errorf("%d runs, extended=%t, disagreed=%t; three agreeing runs are not extended",
			len(set.Outcomes), set.Extended, set.Disagreed)
	}

	// A fixture the engine answers differently each time: the set is extended once, to seven,
	// and not further.
	calls := 0
	flapping := func(context.Context, int) (*eval.RunOutcome, error) {
		calls++
		if calls%2 == 0 {
			return outcomeUnknown(), nil
		}
		return outcomeNaming(culprit), nil
	}
	set, err = eval.DefaultRunPolicy().Execute(context.Background(), "f", flapping)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !set.Disagreed || !set.Extended {
		t.Fatalf("three disagreeing runs were not extended: %+v", set.Verdicts())
	}
	if len(set.Outcomes) != eval.DefaultExtendTo {
		t.Errorf("%d runs, want %d; the extension happens once, not until the answer is convenient",
			len(set.Outcomes), eval.DefaultExtendTo)
	}
	if len(set.DistinctVerdicts()) != 2 {
		t.Errorf("distinct verdicts %v, want two", set.DistinctVerdicts())
	}
}

// TestPassAtOneIsTheFirstRunAndNeverTheMajority: production gets one run, and the set publishes
// every run rather than a vote.
func TestPassAtOneIsTheFirstRunAndNeverTheMajority(t *testing.T) {
	truth := truthNaming(t, culprit)
	after := mustTime(t, knowAt)
	set := eval.RunSetOf("f", eval.DefaultRunPolicy(), []*eval.RunOutcome{
		outcomeUnknown(), outcomeNaming(culprit), outcomeNaming(culprit),
	})
	if first := set.First(); first.Verdict != eval.VerdictUnknown {
		t.Errorf("pass@1 read %q; it is the first run, not the majority", first.Verdict)
	}
	grades := set.Grade(truth, after)
	if len(grades) != 3 {
		t.Fatalf("%d grades for 3 runs", len(grades))
	}
	if grades[0].Pass {
		t.Error("the first run answered `unknown` after knowability and graded as a pass")
	}
	if !grades[1].Pass || !grades[2].Pass {
		t.Error("runs 2 and 3 named the culprit and did not grade as passes")
	}
}

// TestAPolicyStopsOnTheFirstError: an evaluation that swallowed a run's failure would publish a
// pass rate over the runs that happened to work.
func TestAPolicyStopsOnTheFirstError(t *testing.T) {
	boom := errors.New("the world could not be loaded")
	_, err := eval.DefaultRunPolicy().Execute(context.Background(), "f",
		func(context.Context, int) (*eval.RunOutcome, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Errorf("Execute returned %v, want the run's own error", err)
	}
}

// TestASetRefusesToStateOneConfigurationWhenItsRunsDisagree: FR-061 requires the configuration
// recorded, and a set that averaged two of them would be publishing a number about neither.
func TestASetRefusesToStateOneConfigurationWhenItsRunsDisagree(t *testing.T) {
	a, b := outcomeNaming(culprit), outcomeNaming(culprit)
	a.ModelConfigDigest, b.ModelConfigDigest = "aaa", "bbb"
	set := eval.RunSetOf("f", eval.DefaultRunPolicy(), []*eval.RunOutcome{a, b})
	if !set.ModelConfigMismatch || set.ModelConfigDigest != "" {
		t.Errorf("a set over two configurations states %q", set.ModelConfigDigest)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
