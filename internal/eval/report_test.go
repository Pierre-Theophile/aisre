// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/eval"
)

// The rows are tested against answers computed by hand, never against another run of the same
// code. That is the only way a report of a report can fail: pass@1 = 2/3 is 2/3 because two of
// three runs passed, and the test says so in those terms.

const testAudit = "../../docs/evaluation/coverage-audit-2026-09.json"

// row finds one published row.
func row(t *testing.T, rows []eval.Row, metric string, scope eval.Scope, fixture string) eval.Row {
	t.Helper()
	for _, candidate := range rows {
		if candidate.Metric == metric && candidate.Scope == scope && candidate.Fixture == fixture {
			return candidate
		}
	}
	t.Fatalf("no row %s [%s/%s] among %d rows", metric, scope, fixture, len(rows))
	return eval.Row{}
}

func value(t *testing.T, r eval.Row) float64 {
	t.Helper()
	got, ok := r.Float()
	if !ok {
		t.Fatalf("row %s was not measured: %s", r.Metric, r.Detail)
	}
	return got
}

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// TestPassAtOneIsTheMeanOverRunsAndPassHatKIsAllOfThem fabricates three runs of one fixture with
// a known answer: two passed, one did not.
func TestPassAtOneIsTheMeanOverRunsAndPassHatKIsAllOfThem(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		Corpus: "test", AuditPath: testAudit, FixtureRoot: "fixtures/incidents", RunsPerFixture: 3,
		Runs: []eval.RunRow{
			{Fixture: "a", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 1},
			{Fixture: "a", Run: 2, Pass: true, CulpritRank: 1, PriorRank: 1},
			{Fixture: "a", Run: 3, Pass: false, CulpritRank: 4, PriorRank: 1},
		},
	})

	near(t, value(t, row(t, report.Rows, eval.MetricPassAt1, eval.ScopeFixture, "a")), 2.0/3.0, "pass@1")
	near(t, value(t, row(t, report.Rows, eval.MetricPassHatK, eval.ScopeFixture, "a")), 0, "pass^k")
	near(t, value(t, row(t, report.Rows, eval.MetricBestOfK, eval.ScopeFixture, "a")), 1, "best-of-k")
	near(t, value(t, row(t, report.Rows, eval.MetricPassAt1, eval.ScopeCorpus, "")), 2.0/3.0, "corpus pass@1")

	// Best-of-k is published and must carry, in the row itself, the sentence that says it is
	// never gated. A row that only said so in the documentation would be a row somebody wires a
	// gate to.
	best := row(t, report.Rows, eval.MetricBestOfK, eval.ScopeCorpus, "")
	if !strings.Contains(best.Detail, "NEVER gated") {
		t.Fatalf("best_of_k detail does not say it is never gated: %q", best.Detail)
	}
	if report.Trials != 3 {
		t.Fatalf("trials = %d, want 3", report.Trials)
	}
}

// TestLiftIsTheInvestigatorsMRRMinusThePriorsOverTheSameRuns computes the answer by hand.
//
// Ranks 1, 2 and 0 (never surfaced) give an investigator MRR of (1 + 0.5 + 0)/3 = 0.5. The prior
// ranked the culprit 2, 2 and 2, so its MRR is 0.5 as well — and the lift is exactly zero, which
// is the value the gate fails on. That is the case worth pinning: "the reasoning layer tied with
// the ranking a `diff` query already produces" must not read as a pass.
func TestLiftIsTheInvestigatorsMRRMinusThePriorsOverTheSameRuns(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs: []eval.RunRow{
			{Fixture: "a", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 2},
			{Fixture: "a", Run: 2, Pass: true, CulpritRank: 2, PriorRank: 2},
			{Fixture: "a", Run: 3, Pass: false, CulpritRank: 0, PriorRank: 2},
		},
	})

	near(t, value(t, row(t, report.Rows, eval.MetricInvestigatorMRR, eval.ScopeCorpus, "")), 0.5, "investigator MRR")
	near(t, value(t, row(t, report.Rows, eval.MetricPriorMRR, eval.ScopeCorpus, "")), 0.5, "prior MRR")
	near(t, value(t, row(t, report.Rows, eval.MetricLift, eval.ScopeCorpus, "")), 0, "lift")
}

// TestHarmIsThePriorFirstAndTheInvestigationNot is FR-061a's definition, and the confidently
// wrong row is the other half of the same fear.
func TestHarmIsThePriorFirstAndTheInvestigationNot(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs: []eval.RunRow{
			// harmed: the prior had it first, the investigation moved it to third.
			{Fixture: "a", Run: 1, Pass: false, CulpritRank: 3, PriorRank: 1, ConfidentlyWrong: true},
			// not harmed: the prior did not have it first either.
			{Fixture: "a", Run: 2, Pass: false, CulpritRank: 3, PriorRank: 4},
			// not harmed: both had it first.
			{Fixture: "a", Run: 3, Pass: true, CulpritRank: 1, PriorRank: 1},
			{Fixture: "a", Run: 4, Pass: true, CulpritRank: 1, PriorRank: 1},
		},
	})

	near(t, value(t, row(t, report.Rows, eval.MetricHarmRate, eval.ScopeCorpus, "")), 0.25, "harm rate")
	near(t, value(t, row(t, report.Rows, eval.MetricConfidentlyWrong, eval.ScopeCorpus, "")), 0.25,
		"confidently wrong")
}

// TestLocalisationAttributionAndMechanismGetPartialCreditSeparately pins the partial-credit rule:
// naming two of three mechanism edges scores 0.67, not 0 and not 1.
func TestLocalisationAttributionAndMechanismGetPartialCreditSeparately(t *testing.T) {
	t.Parallel()

	truthPath := []string{"change", "payments", "checkout"}
	truthVia := []string{"changed-by", "calls"}

	cases := []struct {
		name                                 string
		gotPath, gotVia                      []string
		localisation, attribution, mechanism float64
	}{
		{
			name: "everything right", gotPath: truthPath, gotVia: truthVia,
			localisation: 1, attribution: 1, mechanism: 1,
		},
		{
			// The symptom is placed and the cause is not: the investigation localised but
			// blamed the wrong change, which is a different failure from the reverse.
			name: "localised but not attributed", gotPath: []string{"other", "checkout"},
			gotVia: []string{"calls"}, localisation: 1, attribution: 0, mechanism: 0.5,
		},
		{
			name: "right ends, wrong middle", gotPath: truthPath, gotVia: []string{"deployed-by", "calls"},
			localisation: 1, attribution: 1, mechanism: 0.5,
		},
		{
			name: "nothing right", gotPath: []string{"x", "y"}, gotVia: []string{"z"},
			localisation: 0, attribution: 0, mechanism: 0,
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			t.Parallel()
			credit := eval.Credit(truthPath, each.gotPath, truthVia, each.gotVia)
			near(t, credit.Localisation, each.localisation, "localisation")
			near(t, credit.Attribution, each.attribution, "attribution")
			near(t, credit.Mechanism, each.mechanism, "mechanism")
		})
	}

	// And a three-edge path that got two right is 2/3, which is the number the requirement's
	// "partial credit" is about.
	credit := eval.Credit(
		[]string{"a", "b", "c", "d"}, []string{"a", "b", "c", "d"},
		[]string{"one", "two", "three"}, []string{"one", "two", "other"})
	near(t, credit.Mechanism, 2.0/3.0, "mechanism over three edges")
}

// TestAFixtureAboveItsMissRateIsExcludedFromTheCorpusAndNeverScoresTheEngine is the rule that
// keeps a recording defect from being reported as a reasoning defect.
func TestAFixtureAboveItsMissRateIsExcludedFromTheCorpusAndNeverScoresTheEngine(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs: []eval.RunRow{
			// `good` answers every term and passes.
			{Fixture: "good", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 1, TermsChecked: 20},
			// `thin` misses a fifth of them and fails; it must not drag the corpus down.
			{Fixture: "thin", Run: 1, Pass: false, CulpritRank: 0, PriorRank: 1,
				TermsChecked: 20, NotRecorded: 4, MissRateThreshold: 0.05},
		},
	})

	if len(report.Excluded) != 1 || report.Excluded[0] != "thin" {
		t.Fatalf("excluded = %v, want [thin]", report.Excluded)
	}
	near(t, value(t, row(t, report.Rows, eval.MetricPassAt1, eval.ScopeCorpus, "")), 1,
		"corpus pass@1 over the admitted fixtures only")
	near(t, value(t, row(t, report.Rows, eval.MetricMissRate, eval.ScopeFixture, "thin")), 0.2, "miss rate")
	near(t, value(t, row(t, report.Rows, eval.MetricFixtureAdmitted, eval.ScopeFixture, "thin")), 0, "admitted")

	// The fixture is still measured and still published — excluded from the aggregate is not
	// the same as unreported.
	near(t, value(t, row(t, report.Rows, eval.MetricPassAt1, eval.ScopeFixture, "thin")), 0, "thin pass@1")
	if report.Trials != 1 {
		t.Fatalf("trials = %d, want 1: an excluded fixture contributes no Bernoulli trials", report.Trials)
	}
}

// TestAMetricWithNothingBehindItIsPublishedAsUnmeasuredRatherThanAsZero.
func TestAMetricWithNothingBehindItIsPublishedAsUnmeasuredRatherThanAsZero(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs:      []eval.RunRow{{Fixture: "a", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 1}},
	})

	agreement := row(t, report.Rows, eval.MetricHumanAgreement, eval.ScopeCorpus, "")
	if agreement.Measured() {
		t.Fatalf("human agreement was measured over a corpus with no labels: %v", agreement)
	}
	if !strings.Contains(agreement.Detail, "n/a") {
		t.Fatalf("unmeasured row does not say why: %q", agreement.Detail)
	}
	credit := row(t, report.Rows, eval.MetricLocalisation, eval.ScopeCorpus, "")
	if credit.Measured() {
		t.Fatalf("localisation was scored against a corpus with no causal path: %v", credit)
	}
}

// TestTheCorpusGapLineNamesEveryRemainderCategoryWithNoFixture is T018's wiring: FR-071b asks for
// the gap to be named on every evaluation run, and this is the run naming it.
func TestTheCorpusGapLineNamesEveryRemainderCategoryWithNoFixture(t *testing.T) {
	t.Parallel()

	// An empty directory covers nothing, so every remainder category is a gap.
	empty := t.TempDir()
	report := eval.Build(eval.Input{
		AuditPath: testAudit, FixtureRoot: empty,
		Runs: []eval.RunRow{{Fixture: "a", Run: 1, Pass: true}},
	})

	gaps := row(t, report.Rows, eval.MetricCorpusGaps, eval.ScopeCorpus, "")
	if value(t, gaps) <= 0 {
		t.Fatalf("an empty corpus has no gaps? %v", gaps)
	}
	if !strings.Contains(gaps.Detail, "corpus gap") {
		t.Fatalf("the gap row does not carry the detector's own warning: %q", gaps.Detail)
	}
	// Every category gets a row of its own, so grepping for one finds it.
	perCategory := 0
	for _, r := range report.Rows {
		if strings.HasPrefix(r.Metric, eval.MetricCorpusGaps+".") {
			perCategory++
		}
	}
	if perCategory == 0 {
		t.Fatal("no per-category corpus-gap rows were published")
	}

	// The real corpus covers at least one category, so the two answers differ — which is the
	// evidence the detector is actually being consulted rather than stubbed.
	real := eval.Build(eval.Input{
		AuditPath: testAudit, FixtureRoot: "../../fixtures/incidents",
		Runs: []eval.RunRow{{Fixture: "a", Run: 1, Pass: true}},
	})
	realGaps := row(t, real.Rows, eval.MetricCorpusGaps, eval.ScopeCorpus, "")
	if value(t, realGaps) >= value(t, gaps) {
		t.Fatalf("the real corpus (%v) covers no more than an empty directory (%v)",
			value(t, realGaps), value(t, gaps))
	}
}

// TestAMissingAuditIsNotNoGaps: an evaluation that could not read its audit has not checked the
// corpus, and saying "0 gaps" would be the opposite of the truth.
func TestAMissingAuditIsNotNoGaps(t *testing.T) {
	t.Parallel()

	rows := eval.GapRows(filepath.Join(t.TempDir(), "absent.json"), "fixtures/incidents")
	if len(rows) != 1 || rows[0].Measured() {
		t.Fatalf("a missing audit produced %v, want one unmeasured row", rows)
	}
	if !strings.Contains(rows[0].Detail, "could not be read") {
		t.Fatalf("the row does not say the audit was unreadable: %q", rows[0].Detail)
	}
}

// TestTheMetamorphicRowsCarryTheVariantAndTheGatedCount (T108's rows).
func TestTheMetamorphicRowsCarryTheVariantAndTheGatedCount(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs:      []eval.RunRow{{Fixture: "a", Run: 1, Pass: true}},
		Invariance: []eval.InvarianceResult{
			{Parent: "a", Variant: "a-time-shifted", Transform: "time-shifted", Held: true,
				ParentVerdict: "k8s.change=x", VariantVerdict: "k8s.change=x", ExpectedVerdict: "k8s.change=x"},
			{Parent: "a", Variant: "a-name-permuted", Transform: "name-permuted", Held: false,
				ParentVerdict: "k8s.change=x", VariantVerdict: "k8s.change=y", ExpectedVerdict: "k8s.change=z",
				Detail: "the name permutation moved the verdict"},
		},
	})

	near(t, value(t, row(t, report.Rows, eval.MetricMetamorphicChanges, eval.ScopeCorpus, "")), 1,
		"metamorphic verdict changes")
	held := row(t, report.Rows, eval.MetricMetamorphic+".time-shifted", eval.ScopeFixture, "a-time-shifted")
	near(t, value(t, held), 1, "time-shifted held")
	broke := row(t, report.Rows, eval.MetricMetamorphic+".name-permuted", eval.ScopeFixture, "a-name-permuted")
	near(t, value(t, broke), 0, "name-permuted held")
	if !strings.Contains(broke.Detail, "name permutation") {
		t.Fatalf("the failing row does not carry the checker's own sentence: %q", broke.Detail)
	}
}

// TestDetectionPowerReproducesThePublishedSentence. FR-061 publishes one worked example, and the
// arithmetic here must produce exactly it: 40 trials detects 90 % → 70 % and does not detect
// 90 % → 80 %.
func TestDetectionPowerReproducesThePublishedSentence(t *testing.T) {
	t.Parallel()

	if need := eval.RequiredTrials(0.9, 0.7); need > 40 {
		t.Fatalf("90%%→70%% needs n ≥ %.1f, which 40 trials would not reach; the published "+
			"sentence says it is detectable", need)
	}
	if need := eval.RequiredTrials(0.9, 0.8); need <= 40 {
		t.Fatalf("90%%→80%% needs n ≥ %.1f, which 40 trials would reach; the published sentence "+
			"says it is NOT detectable", need)
	}
	if !math.IsInf(eval.RequiredTrials(0.9, 0.9), 1) {
		t.Fatal("no sample size can distinguish a rate from itself; RequiredTrials must say so")
	}

	power := eval.DetectionPower(40, 0.9)
	if power.Detectable <= 0.7 || power.Detectable >= 0.8 {
		t.Fatalf("detectable at n=40, p0=0.9 is %v; it must sit between the two published "+
			"alternatives", power.Detectable)
	}
	sentence := power.Example(0.70, 0.80)
	if !strings.Contains(sentence, "detects 90 % → 70 % reliably") ||
		!strings.Contains(sentence, "cannot detect 90 % → 80 %") {
		t.Fatalf("the published example did not come out: %q", sentence)
	}
	if !strings.Contains(power.Sentence(), "normal approximation to the binomial") {
		t.Fatalf("a detection-power statement with no method behind it: %q", power.Sentence())
	}

	// A zero-tolerance gate states the complement instead, and it is monotone in n.
	if eval.ZeroToleranceDetectable(39) <= eval.ZeroToleranceDetectable(400) {
		t.Fatal("a bigger corpus must catch a rarer intermittent defect")
	}
}

// TestTheRowsSerialiseAsOneDocumentPerLineWithANullableValue — the shape the gate greps.
func TestTheRowsSerialiseAsOneDocumentPerLineWithANullableValue(t *testing.T) {
	t.Parallel()

	encoded, err := eval.JSONL([]eval.Row{
		eval.NewRow("pass_at_1", eval.ScopeCorpus, "", 0.5, 4, "gated"),
		eval.NotMeasured("human_agreement", eval.ScopeCorpus, "", "n/a — no labels"),
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(encoded), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL wrote %d line(s), want 2", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"metric", "scope", "value", "n"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("row has no %q: %v", key, first)
		}
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second["value"] != nil {
		t.Fatalf("an unmeasured row must serialise its value as null, got %v", second["value"])
	}
}

// TestTheDocumentKeepsItsProseAndRegeneratesOnlyItsTables.
func TestTheDocumentKeepsItsProseAndRegeneratesOnlyItsTables(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "investigation-metrics.md")
	prose := "# Investigation metrics\n\nThis paragraph is written by a person and must survive.\n"
	if err := os.WriteFile(path, []byte(prose), 0o600); err != nil {
		t.Fatal(err)
	}

	first := eval.Build(eval.Input{
		Corpus: "public", AuditPath: testAudit,
		Runs: []eval.RunRow{{Fixture: "a", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 2}},
	})
	if err := first.WriteDoc(path); err != nil {
		t.Fatal(err)
	}
	second := eval.Build(eval.Input{
		Corpus: "private", AuditPath: testAudit,
		Runs: []eval.RunRow{{Fixture: "a", Run: 1, Pass: false, CulpritRank: 0, PriorRank: 2}},
	})
	if err := second.WriteDoc(path); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path) //nolint:gosec // a path this test made.
	if err != nil {
		t.Fatal(err)
	}
	doc := string(got)
	if !strings.Contains(doc, "written by a person and must survive") {
		t.Fatal("the regeneration ate the document's prose")
	}
	if strings.Count(doc, "BEGIN GENERATED") != 1 {
		t.Fatalf("the generated block was appended twice:\n%s", doc)
	}
	if strings.Contains(doc, "public corpus") {
		t.Fatal("the second run did not replace the first run's tables")
	}
	if !strings.Contains(doc, "private corpus") {
		t.Fatal("the second run's tables are not in the document")
	}
}

// TestAModelFreeRunLabelsEveryNumberItPublishes. A model-free number that could be mistaken for
// the production result is the one way this report can mislead a reader who is not looking for it.
func TestAModelFreeRunLabelsEveryNumberItPublishes(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		Corpus: "public", AuditPath: testAudit, ModelFree: true,
		Runs: []eval.RunRow{{Fixture: "a", Run: 1, Pass: true}},
	})
	model := row(t, report.Rows, eval.MetricModelConfig, eval.ScopeCorpus, "")
	if !strings.Contains(model.Detail, "MODEL-FREE") {
		t.Fatalf("a model-free run does not say so: %q", model.Detail)
	}
	if !strings.Contains(report.Markdown(), "MODEL-FREE RUN") {
		t.Fatal("the Markdown summary does not carry the model-free warning")
	}
}

// TestCitationValidityIsCheckedAgainstTheRecordingAndUntraceabilityIsCountedSeparately.
func TestCitationValidityIsCheckedAgainstTheRecordingAndUntraceabilityIsCountedSeparately(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs: []eval.RunRow{{
			Fixture: "a", Run: 1, Pass: true,
			Claims: 4, ValidClaims: 3, Untraceable: 1,
		}},
	})
	near(t, value(t, row(t, report.Rows, eval.MetricCitationValidity, eval.ScopeCorpus, "")), 0.75,
		"citation validity")
	near(t, value(t, row(t, report.Rows, eval.MetricUntraceable, eval.ScopeCorpus, "")), 1,
		"untraceable conclusions")
}

// TestAHorizonTruncatedCitationIsValidAndAnUnrecordedOneIsNot pins the rule that put live
// citation validity at 11 of 12 on a fixture whose `not_recorded` miss rate was 0 of 12.
//
// A world is keyed by the term; the *digest* of a horizon-truncated answer is not a function of
// the key alone. The recorded backend looks the answer up under the clamped key, stamps the
// investigation's observed_at into its coverage — constitution II: the statement belongs in the
// answer, not beside it — and recomputes the digest over what actually leaves the process. So a
// caller that asks the already-clamped window receives the recorded digest and one that asks a
// window running past the horizon receives the annotated one, and comparing the second against
// the world file reported a sound citation as a fabricated one.
//
// The gate is not weakened by this. The truncation statement is the backend's and reaches the
// citation through an evidence item the engine minted from a real answer, so a run cannot claim
// it for an id it invented; and every citation that does *not* carry it is still checked
// digest-for-digest against the recording, which the second and third cases here show.
func TestAHorizonTruncatedCitationIsValidAndAnUnrecordedOneIsNot(t *testing.T) {
	t.Parallel()

	const (
		recordedKey    = "term-key-1"
		recordedDigest = "digest-as-recorded"
	)
	world := map[string]string{recordedKey: recordedDigest}
	truncated := json.RawMessage(`{"coverage":{"truncated_to_horizon":true,"horizon":"2026-09-01T14:32:00Z"}}`)
	plain := json.RawMessage(`{"coverage":{"truncated_to_horizon":false}}`)

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs: eval.RunRowsFrom(
			&eval.RunSet{FixtureID: "a", Outcomes: []*eval.RunOutcome{{
				FixtureID: "a",
				Citations: []eval.Citation{
					// Served straight from the recording: the digests agree.
					{EvidenceID: "e-1", Worker: "metrics", Term: "compare",
						ResponseKey: recordedKey, ResponseDigest: recordedDigest, Digest: plain},
					// Served from the recording and re-digested after the horizon was stamped
					// into it. The digest differs *by construction*, and the citation is sound.
					{EvidenceID: "e-2", Worker: "metrics", Term: "compare",
						ResponseKey: recordedKey, ResponseDigest: "digest-after-annotation", Digest: truncated},
					// A digest that is neither: the recording holds another answer under this
					// key, and nothing says the run's answer was truncated.
					{EvidenceID: "e-3", Worker: "metrics", Term: "onset",
						ResponseKey: recordedKey, ResponseDigest: "some-other-digest", Digest: plain},
					// A key the recording does not hold at all, claimed by an answer that *did*
					// produce a digest. This is the fabrication the gate exists for.
					{EvidenceID: "e-4", Worker: "logs", Term: "new_log_patterns",
						ResponseKey: "term-key-absent", ResponseDigest: "d", Digest: plain},
					// The recording said it holds nothing for this term, and the run recorded
					// that. It is an answer, it is what the miss rate measures, and it is not an
					// invalid citation.
					{EvidenceID: "e-5", Worker: "metrics", Term: "compare", Outcome: "not_recorded",
						ResponseKey: "term-key-absent-2", ResponseDigest: "d2", Digest: plain},
					// A worker that refused. It carries no digest by construction (FR-027).
					{EvidenceID: "e-6", Worker: "graph", Term: "pointers", Outcome: "query_failed"},
				},
			}}},
			[]eval.GradeResult{{Pass: true}},
			eval.GroundTruth{Culprit: "k8s.change=x"},
			eval.RowOptions{WorldDigests: world},
		),
	})

	validity := row(t, report.Rows, eval.MetricCitationValidity, eval.ScopeCorpus, "")
	near(t, value(t, validity), 4.0/6.0, "citation validity")
	if !strings.Contains(validity.Detail, "e-3") {
		t.Errorf("the gate line does not name the first invalid citation: %q", validity.Detail)
	}
	near(t, value(t, row(t, report.Rows, eval.MetricUntraceable, eval.ScopeCorpus, "")), 0,
		"untraceable conclusions")
}

// TestANotRecordedCitationIsTheMissRatesBusinessAndNotTheCitationGates is the same rule stated on
// its own, because it is the one that decides whether two gates measure the same fact twice.
func TestANotRecordedCitationIsTheMissRatesBusinessAndNotTheCitationGates(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		AuditPath: testAudit,
		Runs: eval.RunRowsFrom(
			&eval.RunSet{FixtureID: "a", Outcomes: []*eval.RunOutcome{{
				FixtureID: "a",
				Citations: []eval.Citation{
					{EvidenceID: "e-1", Worker: "metrics", Term: "compare", Outcome: "not_recorded",
						ResponseKey: "nothing-here", ResponseDigest: "d"},
					{EvidenceID: "e-2", Worker: "metrics", Term: "onset", Outcome: "not_recorded",
						ResponseKey: "nor-here", ResponseDigest: "d"},
				},
			}}},
			[]eval.GradeResult{{Pass: true}},
			eval.GroundTruth{Culprit: "k8s.change=x"},
			eval.RowOptions{WorldDigests: map[string]string{"something-else": "d"}},
		),
	})
	near(t, value(t, row(t, report.Rows, eval.MetricCitationValidity, eval.ScopeCorpus, "")), 1,
		"citation validity over not_recorded citations")
}

// TestTheCacheReadRatioIsPublishedAndAModelFreeRunSaysNotApplicable is the metric quickstart §11
// names and nothing implemented (FR-048).
//
// Two halves, and the second is the one that matters. A run that called a model publishes cached
// input over total input — where total input is every input-side class, because a cache *write*
// is input somebody paid full price for. A model-free run publishes `n/a`: it did not fail to hit
// the cache, it never asked, and plotting those as 0 % would show a cache regression on every CI
// run this repository makes without a vendor credential.
func TestTheCacheReadRatioIsPublishedAndAModelFreeRunSaysNotApplicable(t *testing.T) {
	t.Parallel()

	report := eval.Build(eval.Input{
		Corpus: "test", AuditPath: testAudit, RunsPerFixture: 2,
		Runs: []eval.RunRow{
			// 1200 of 1500 on one run, 600 of 1000 on the other: 1800 of 2500 over the corpus.
			{Fixture: "a", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 1,
				CachedInputTokens: 1200, TotalInputTokens: 1500},
			{Fixture: "a", Run: 2, Pass: true, CulpritRank: 1, PriorRank: 1,
				CachedInputTokens: 600, TotalInputTokens: 1000},
		},
	})

	fixtureRow := row(t, report.Rows, eval.MetricCacheReadRatio, eval.ScopeFixture, "a")
	near(t, value(t, fixtureRow), 1800.0/2500.0, "fixture cache_read_ratio")
	corpusRow := row(t, report.Rows, eval.MetricCacheReadRatio, eval.ScopeCorpus, "")
	near(t, value(t, corpusRow), 1800.0/2500.0, "corpus cache_read_ratio")
	// It is a cost figure, and a build that failed on a vendor's cache behaviour would be
	// failing on somebody else's deployment decision.
	if !strings.Contains(corpusRow.Detail, "NEVER gated") {
		t.Errorf("the cache-read ratio does not say it is never gated: %q", corpusRow.Detail)
	}

	modelFree := eval.Build(eval.Input{
		Corpus: "test", AuditPath: testAudit, ModelFree: true, RunsPerFixture: 1,
		Runs: []eval.RunRow{{Fixture: "a", Run: 1, Pass: true, CulpritRank: 1, PriorRank: 1}},
	})
	for _, scope := range []struct {
		scope   eval.Scope
		fixture string
	}{{eval.ScopeFixture, "a"}, {eval.ScopeCorpus, ""}} {
		got := row(t, modelFree.Rows, eval.MetricCacheReadRatio, scope.scope, scope.fixture)
		if got.Measured() {
			t.Errorf("a model-free run published a cache-read ratio of %v; it made no model call",
				got.Value)
		}
		if !strings.Contains(got.Detail, "n/a") {
			t.Errorf("the unmeasured ratio does not say why: %q", got.Detail)
		}
	}
	if !strings.Contains(modelFree.Markdown(), "cache_read_ratio") {
		t.Error("the rendered report does not publish the cache-read ratio")
	}
}
