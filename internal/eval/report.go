// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The investigation report rows (T109; FR-056, FR-059, FR-061a, FR-071b; SC-006, SC-008, SC-013).
//
// This is the file that turns k graded runs over a corpus of incident fixtures into the numbers
// the project is judged on. Everything here is a *rendering* decision; the grading itself — what
// counts as a pass, what `knowability_time` does to an `unknown`, which variant's verdict must
// not move — belongs to the harness and the policy beside it, and is read from `RunRow`.
//
// Four rules the rows encode, each of which is a way an evaluation report otherwise lies.
//
//  1. **The headline is pass@1, and pass^k is published beside it** (FR-061). Best-of-k is
//     computed because it describes capability, and it is emitted with `gated: never` in its
//     detail so that nobody can wire a gate to it by accident.
//  2. **Lift is the whole point.** The investigator's MRR minus the deterministic ranker's, over
//     the same corpus and the same runs (FR-061a). The prior's rank per fixture comes from the
//     fixture's own `ground_truth.prior_rank_of_culprit`, written down before the engine existed,
//     so the comparison cannot be made flattering by re-running the ranker.
//  3. **The miss rate gates the fixture, never the engine.** A fixture whose world could not
//     answer more than `miss_rate_threshold` of the questions asked of it is excluded from every
//     corpus aggregate and listed by name. Letting it score would report a recording defect as a
//     reasoning defect.
//  4. **Reported is not gated.** The culprit's rank, top-k, calibration, cost and every timing
//     are published and trended; the gate never reads them. Each such row says so in its detail,
//     because the separation only survives if it is written down where the number is.

// The published metric names. `scripts/check-report.sh --investigation` names these exact
// strings, so they are constants here and are changed the way a published schema is changed.
const (
	// MetricPassAt1 is the mean outcome over runs — the headline (FR-061).
	MetricPassAt1 = "pass_at_1"
	// MetricPassHatK is 1 only when all k runs passed — the reliability figure.
	MetricPassHatK = "pass_hat_k"
	// MetricBestOfK describes capability and MUST NOT be gated on, ever (FR-060).
	MetricBestOfK = "best_of_k"
	// MetricLift is the investigator's MRR minus the prior's (FR-061a).
	MetricLift = "lift_over_prior"
	// MetricInvestigatorMRR and MetricPriorMRR are the two halves of the lift, published so a
	// lift of zero can be read as "both good" or "both bad".
	MetricInvestigatorMRR = "investigator_mrr"
	MetricPriorMRR        = "prior_mrr"
	// MetricHarmRate is the fraction of runs where the prior put the culprit first and the
	// investigation did not (FR-061a).
	MetricHarmRate = "harm_rate"
	// MetricConfidentlyWrong is a named non-culprit published at `high` or `very_high`.
	MetricConfidentlyWrong = "confidently_wrong"
	// MetricCitationValidity is the fraction of claims resolving to an evidence id whose digest
	// matches the recording (FR-061a). It is a hard 100 % gate.
	MetricCitationValidity = "citation_validity"
	// MetricUntraceable counts conclusions with a claim that resolves to nothing.
	MetricUntraceable = "untraceable_conclusions"
	// MetricCulpritRank and MetricTopKHit are reported, never the headline (FR-059).
	MetricCulpritRank = "culprit_rank"
	MetricTopKHit     = "top_k_hit_rate"
	// MetricLocalisation, MetricAttribution and MetricMechanism are scored separately with
	// partial credit against `causal_path` (FR-061b).
	MetricLocalisation = "localisation"
	MetricAttribution  = "attribution"
	MetricMechanism    = "mechanism"
	// MetricMissRate is the world-replay `not_recorded` miss rate. It gates a fixture's
	// admission and never the engine's score (FR-060).
	MetricMissRate = "not_recorded_miss_rate"
	// MetricFixtureAdmitted is 1 for a fixture inside the scored corpus and 0 for one excluded
	// on its miss rate.
	MetricFixtureAdmitted = "fixture_admitted"
	// MetricOnsetError and MetricOnsetWithinTolerance are published wherever a fixture labels
	// an onset.
	MetricOnsetError           = "onset_error_seconds"
	MetricOnsetWithinTolerance = "onset_within_tolerance"
	// The three published timings (SC-013).
	MetricTimeToProvisional = "time_to_provisional_seconds"
	MetricTimeToFirstTested = "time_to_first_tested_seconds"
	MetricTimeToConclusion  = "time_to_conclusion_seconds"
	// SC-013 is stated as shares of runs inside a deadline, not as means, and a mean hides
	// exactly the runs the target is about: three fast runs and one that took a minute average
	// to something that meets a 5-second target no run met. These are those shares, and
	// MetricRunsPastHardStop the count of runs that ran past the profile's hard stop at all.
	MetricProvisionalWithinTarget = "time_to_provisional_within_target"
	MetricFirstTestedWithinTarget = "time_to_first_tested_within_target"
	MetricConclusionWithinTarget  = "time_to_conclusion_within_target"
	MetricRunsPastHardStop        = "runs_past_hard_stop"
	// MetricWorkerCalls is the call count, with `.<capability>` suffixes for the breakdown.
	MetricWorkerCalls = "worker_calls"
	// MetricBackendQuotaShare is each backend's share of the algebra calls issued.
	MetricBackendQuotaShare = "backend_quota_share"
	// MetricCost is the run's spend from the versioned price table.
	MetricCost = "cost_usd"
	// MetricCacheReadRatio is cached input tokens over total input tokens, from the recorded
	// per-call `Usage` (FR-048, quickstart §11).
	//
	// It is a **cost** figure, not a quality one: the prompt is built so the expensive prefix is
	// stable and the cheap tail moves, and a ratio that collapses between two releases means a
	// silent invalidator crept into the prefix — a thing that shows up as a bill rather than as
	// a wrong answer. It is reported and never gated, because the number is the vendor's cache
	// behaviour as much as ours and a build that failed on it would fail on somebody else's
	// deployment decision.
	MetricCacheReadRatio = "cache_read_ratio"
	// MetricUnknownRate and MetricNonUnknownPrecision are SC-007 and SC-023's pair.
	MetricUnknownRate         = "unknown_rate"
	MetricNonUnknownPrecision = "non_unknown_precision"
	// MetricReplayDivergenceRate and MetricImprovisedReplays are the two replay failures: a
	// recording that did not reproduce, and a `not_recorded` served as though recorded.
	MetricReplayDivergenceRate = "replay_divergence_rate"
	MetricImprovisedReplays    = "improvised_replays"
	// MetricHumanAgreement is agreement with human corrections where labels exist, and
	// MetricHumanLabelRegressions counts labelled cases this run got wrong that it used to get
	// right.
	MetricHumanAgreement        = "human_agreement"
	MetricHumanLabelRegressions = "human_label_regressions"
	// MetricMetamorphic is one invariance check (T108), and MetricMetamorphicChanges the count
	// of variants whose verdict moved.
	MetricMetamorphic        = "metamorphic_invariance"
	MetricMetamorphicChanges = "metamorphic_verdict_changes"
	// MetricCorpusGaps is the corpus-gap line (FR-071b, T018's wiring).
	MetricCorpusGaps = "corpus_gaps"
	// MetricTrials is n for the detection-power statement: fixtures × runs.
	MetricTrials = "trials"
	// MetricFixtures and MetricRuns record the shape of the run.
	MetricFixtures = "fixtures"
	MetricRuns     = "runs"
	// MetricCorpus and MetricModelConfig are provenance: a number with no corpus and no model
	// configuration attached is not a result (FR-061, T113).
	MetricCorpus      = "corpus"
	MetricModelConfig = "model_configuration"
)

// The detail sentences that mark a row's relationship to the gate. They are written into the
// rows rather than only into the documentation so that a reader of the raw JSONL can see which
// numbers can fail a build.
const (
	detailGated      = "gated"
	detailReported   = "reported, not gated"
	detailNeverGated = "reported, NEVER gated (FR-060)"
	detailAdmission  = "gates this fixture's admission to the corpus, never the engine's score"
)

// DefaultMissRateThreshold is the admission bar a fixture that declares none is held to
// (contracts/incident-format.md `world.miss_rate_threshold`).
const DefaultMissRateThreshold = 0.05

// The `page` profile's published deadlines (SC-013, T117). They are the thresholds the three
// share rows are taken against, and they live here rather than in the benchmark that publishes
// them so that the benchmark is a reading of the rows and not a second definition of them.
const (
	TargetTimeToProvisional = 5 * time.Second
	TargetTimeToFirstTested = 120 * time.Second
	TargetTimeToConclusion  = 5 * time.Minute
	HardStop                = 10 * time.Minute
)

// withinTarget is how many of the measured values met a deadline, and how many were measured. A
// run that produced no such instant is not counted as a miss: it is not a measurement.
func withinTarget(values []float64, target time.Duration) (hits, total int) {
	limit := target.Seconds()
	for _, value := range values {
		total++
		if value <= limit {
			hits++
		}
	}
	return hits, total
}

// RunRow is one graded run of one fixture, in the flat shape the report reads.
//
// It is the report's input contract, deliberately separate from the harness's own `RunOutcome`:
// the harness knows how to produce a run, the report knows how to score a corpus, and keeping
// the boundary explicit is what lets every number below be tested against an answer computed by
// hand. `RunRowsFrom` adapts the harness's outcomes into these.
type RunRow struct {
	// Fixture is the incident id, and Run the 1-based index within this fixture's k runs.
	Fixture string
	Run     int

	// Pass is the graded verdict under the run policy, knowability time included: asked before
	// the knowability instant, `unknown` is a pass and naming the culprit is a failure (FR-061).
	Pass bool
	// Outcome is the published outcome word the run reached, e.g. `culprit`, `unknown`,
	// `unobserved`.
	Outcome string
	// Unknown is whether the run concluded `unknown`.
	Unknown bool
	// Correct is whether a non-`unknown` outcome was right; it is the numerator of the
	// precision of non-`unknown` outcomes.
	Correct bool

	// CulpritRank is where the investigation ranked the true culprit, 1-based; 0 means it never
	// surfaced it. PriorRank is the same for the deterministic ranker, read from the fixture's
	// `ground_truth.prior_rank_of_culprit`.
	CulpritRank int
	PriorRank   int
	// ExpectedTopK is the fixture's own k for the top-k hit.
	ExpectedTopK int
	// ConfidentlyWrong is a named non-culprit published at `high` or `very_high` — the harm
	// this project is most afraid of, and a gate in its own right.
	ConfidentlyWrong bool
	// Confidence is the published bucket of the answer, carried for the detail line.
	Confidence string

	// Credit is localisation, attribution and mechanism, each in [0,1], and HasCredit is false
	// for a fixture whose ground truth publishes no causal path.
	Credit    CausalCredit
	HasCredit bool

	// Claims is how many claims the conclusion made, ValidClaims how many resolved to an
	// evidence id whose digest matches the recording, and Untraceable how many resolved to
	// nothing at all.
	Claims      int
	ValidClaims int
	Untraceable int
	// CitationFailure names the first invalid citation and says why it was invalid, so the gate
	// line points at a claim rather than only at a ratio.
	CitationFailure string

	// TermsChecked and NotRecorded are the world-replay denominators: how many algebra terms
	// the run asked for, and how many the recorded world could not answer.
	TermsChecked int
	NotRecorded  int
	// MissedTerms names the terms behind NotRecorded, one entry per miss, so the corpus report
	// can say which part of the cross product the recording is thin in.
	MissedTerms []string
	// MissRateThreshold is the fixture's own admission bar; 0 means DefaultMissRateThreshold.
	MissRateThreshold float64
	// Improvised is a `not_recorded` served as though it had been recorded — the one replay
	// failure that is worse than a divergence, because it produces a plausible answer.
	Improvised bool
	// Diverged is a replay that did not reproduce its recording.
	Diverged bool

	// OnsetErrorSeconds and OnsetWithinTolerance are published only where the fixture labels an
	// onset; both are nil otherwise.
	OnsetErrorSeconds    *float64
	OnsetWithinTolerance *bool

	// The three timings, in seconds, nil where the run did not reach that point.
	TimeToProvisional *float64
	TimeToFirstTested *float64
	TimeToConclusion  *float64

	// WorkerCalls is calls by capability and BackendCalls calls by backend, from which the
	// quota share is computed.
	WorkerCalls  map[string]int
	BackendCalls map[string]int
	// CostUSD is this run's spend.
	CostUSD float64
	// CachedInputTokens and TotalInputTokens are the two halves of the cache-read ratio, summed
	// over this run's model calls from the recorded `Usage`. Total input is every input-side
	// class — uncached input, cache writes and cache reads — because the ratio answers "how much
	// of what we sent did we pay full price for?" and a cache write is input we paid for.
	// Both are zero for a model-free run, which is reported as `n/a` rather than as 0.
	CachedInputTokens int64
	TotalInputTokens  int64

	// HumanLabelAgreed is set only for a case carrying a human correction: true when this run
	// agreed with the label. HumanLabelRegressed is true when a previously-agreeing case now
	// disagrees, which is the gated form.
	HumanLabelAgreed    *bool
	HumanLabelRegressed bool
}

// Input is everything one evaluation run publishes.
type Input struct {
	// Corpus names which corpus produced these numbers — `public` or `private` — because a
	// number with no corpus attached is not a result (ADR-0003 D9, T113).
	Corpus string
	// FixtureRoot is the directory the corpus was read from.
	FixtureRoot string
	// AuditPath is the published coverage audit this run cites (FR-071).
	AuditPath string
	// ModelConfigVersion and ModelConfigDigest identify the model configuration the runs used.
	// FR-061 requires production's configuration and requires it to be recorded.
	ModelConfigVersion string
	ModelConfigDigest  string
	// ModelFree is true when the runs made no model call at all. Every number a model-free run
	// publishes is labelled as such, so it can never be read as the production result.
	ModelFree bool
	// RunsPerFixture is the k the policy used (3, extended to 7 on disagreement).
	RunsPerFixture int
	// Runs are the graded runs, and Invariance the metamorphic checks `CheckInvariance`
	// produced (T108's rows, emitted here because this is where the report writer lives).
	Runs       []RunRow
	Invariance []InvarianceResult
	// GeneratedAt stamps the report. Zero means "now".
	GeneratedAt time.Time
}

// Report is the rows plus the shape a renderer needs.
type Report struct {
	// Input is what produced it, carried so the Markdown can name the corpus and the model.
	Input Input
	// Rows are every published row, fixture rows first, then corpus rows.
	Rows []Row
	// Excluded names the fixtures kept out of the corpus aggregates on their miss rate.
	Excluded []string
	// Trials is fixtures × runs over the admitted fixtures — n for the detection statement.
	Trials int
}

// Build produces the report. It reads the corpus-gap detector itself (FR-071b: the gap must be
// named on every evaluation run, so it is not something a caller can forget to ask for).
func Build(in Input) *Report {
	report := &Report{Input: in}
	byFixture := groupByFixture(in.Runs)

	admitted := make([]RunRow, 0, len(in.Runs))
	for _, name := range sortedKeys(byFixture) {
		runs := byFixture[name]
		fixtureRows, ok := fixtureRows(name, runs)
		report.Rows = append(report.Rows, fixtureRows...)
		if ok {
			admitted = append(admitted, runs...)
			continue
		}
		report.Excluded = append(report.Excluded, name)
	}
	report.Trials = len(admitted)

	report.Rows = append(report.Rows, corpusRows(admitted, report.Excluded, len(byFixture))...)
	// The corpus miss rate is computed over EVERY run, admitted and excluded alike, which is why
	// it is not inside corpusRows. Excluding the thin fixtures from the number that measures
	// thinness would make the corpus rate pass by construction: the fixtures dragging it up are
	// exactly the ones admission removed. (003 T112, FR-108.)
	report.Rows = append(report.Rows, corpusMissRows(in.Runs)...)
	report.Rows = append(report.Rows, invarianceRows(in.Invariance)...)
	report.Rows = append(report.Rows, GapRows(in.AuditPath, in.FixtureRoot)...)
	report.Rows = append(report.Rows, provenanceRows(in, report.Trials, len(byFixture))...)
	return report
}

// groupByFixture keeps the runs of one fixture together and in run order.
func groupByFixture(runs []RunRow) map[string][]RunRow {
	out := map[string][]RunRow{}
	for _, run := range runs {
		out[run.Fixture] = append(out[run.Fixture], run)
	}
	for name := range out {
		sort.SliceStable(out[name], func(i, j int) bool { return out[name][i].Run < out[name][j].Run })
	}
	return out
}

// fixtureRows publishes one fixture's k runs, and reports whether the fixture is admitted to the
// corpus aggregates.
//
// would hide which metrics a fixture publishes — the thing a reader comes here to check.
//
//nolint:funlen // One fixture's published rows are a list, and splitting the list across helpers
func fixtureRows(name string, runs []RunRow) ([]Row, bool) {
	n := len(runs)
	passes := make([]bool, 0, n)
	ranks := make([]int, 0, n)
	priorRanks := make([]int, 0, n)
	var (
		claims, validClaims, untraceable           int
		citationFailure                            string
		terms, notRecorded                         int
		unknowns, nonUnknown, nonUnknownOK         int
		harms, confidentlyWrong, topKHits          int
		diverged, improvised                       int
		localisation, attribution, mechanismCredit []float64
		onsetErrors                                []float64
		onsetInTolerance, onsetLabelled            int
		provisional, firstTested, conclude         []float64
		cost                                       float64
		cachedInput, totalInput                    int64
		workerCalls                                = map[string]int{}
		backendCalls                               = map[string]int{}
	)
	threshold := DefaultMissRateThreshold

	for _, run := range runs {
		passes = append(passes, run.Pass)
		ranks = append(ranks, run.CulpritRank)
		priorRanks = append(priorRanks, run.PriorRank)
		claims += run.Claims
		validClaims += run.ValidClaims
		untraceable += run.Untraceable
		if citationFailure == "" && run.CitationFailure != "" {
			citationFailure = " — " + run.CitationFailure
		}
		terms += run.TermsChecked
		notRecorded += run.NotRecorded
		if run.MissRateThreshold > 0 {
			threshold = run.MissRateThreshold
		}
		if run.Unknown {
			unknowns++
		} else {
			nonUnknown++
			if run.Correct {
				nonUnknownOK++
			}
		}
		if run.PriorRank == 1 && run.CulpritRank != 1 {
			harms++
		}
		if run.ConfidentlyWrong {
			confidentlyWrong++
		}
		if k := expectedTopK(run); run.CulpritRank > 0 && run.CulpritRank <= k {
			topKHits++
		}
		if run.Diverged {
			diverged++
		}
		if run.Improvised {
			improvised++
		}
		if run.HasCredit {
			localisation = append(localisation, run.Credit.Localisation)
			attribution = append(attribution, run.Credit.Attribution)
			mechanismCredit = append(mechanismCredit, run.Credit.Mechanism)
		}
		if run.OnsetErrorSeconds != nil {
			onsetLabelled++
			onsetErrors = append(onsetErrors, *run.OnsetErrorSeconds)
			if run.OnsetWithinTolerance != nil && *run.OnsetWithinTolerance {
				onsetInTolerance++
			}
		}
		provisional = appendIf(provisional, run.TimeToProvisional)
		firstTested = appendIf(firstTested, run.TimeToFirstTested)
		conclude = appendIf(conclude, run.TimeToConclusion)
		cost += run.CostUSD
		cachedInput += run.CachedInputTokens
		totalInput += run.TotalInputTokens
		addCounts(workerCalls, run.WorkerCalls)
		addCounts(backendCalls, run.BackendCalls)
	}

	rows := []Row{
		rate(MetricPassAt1, ScopeFixture, name, passes, detailGated+": the headline, over this fixture's runs"),
		rate(MetricPassHatK, ScopeFixture, name, passes, detailReported+": all k runs passed"),
		bestOfK(name, passes),
	}
	rows = append(rows,
		meanRow(MetricCulpritRank, ScopeFixture, name, ranksAsFloats(ranks),
			detailReported+": where the investigation put the true culprit (0 = never surfaced)"),
		fraction(MetricTopKHit, ScopeFixture, name, topKHits, n, detailReported),
		fraction(MetricHarmRate, ScopeFixture, name, harms, n,
			detailGated+": the prior put the culprit first and the investigation did not"),
		fraction(MetricConfidentlyWrong, ScopeFixture, name, confidentlyWrong, n,
			detailGated+": a non-culprit named at `high` or `very_high`"),
		fraction(MetricCitationValidity, ScopeFixture, name, validClaims, claims,
			detailGated+": every cited evidence id resolves to a digest matching the recording"+citationFailure),
		count(MetricUntraceable, ScopeFixture, name, untraceable, claims, detailGated),
		fraction(MetricUnknownRate, ScopeFixture, name, unknowns, n, detailReported),
		fraction(MetricNonUnknownPrecision, ScopeFixture, name, nonUnknownOK, nonUnknown, detailReported),
		fraction(MetricReplayDivergenceRate, ScopeFixture, name, diverged, n, detailGated+": must be 0"),
		count(MetricImprovisedReplays, ScopeFixture, name, improvised, n, detailGated+": must be 0"),
	)

	missRate, measured := Rate(notRecorded, terms)
	admitted := !measured || missRate <= threshold
	rows = append(rows,
		fraction(MetricMissRate, ScopeFixture, name, notRecorded, terms, fmt.Sprintf(
			"%s (threshold %.2f)", detailAdmission, threshold)),
		boolRow(MetricFixtureAdmitted, ScopeFixture, name, admitted, n, admissionDetail(admitted, missRate, measured, threshold)),
	)

	rows = append(rows,
		creditRow(MetricLocalisation, name, localisation),
		creditRow(MetricAttribution, name, attribution),
		creditRow(MetricMechanism, name, mechanismCredit),
		meanRow(MetricOnsetError, ScopeFixture, name, onsetErrors,
			detailReported+": seconds between the estimated and the labelled onset"),
		fraction(MetricOnsetWithinTolerance, ScopeFixture, name, onsetInTolerance, onsetLabelled, detailReported),
		meanRow(MetricTimeToProvisional, ScopeFixture, name, provisional, detailReported),
		meanRow(MetricTimeToFirstTested, ScopeFixture, name, firstTested, detailReported),
		meanRow(MetricTimeToConclusion, ScopeFixture, name, conclude, detailReported),
		shareWithin(MetricProvisionalWithinTarget, ScopeFixture, name, provisional, TargetTimeToProvisional),
		shareWithin(MetricFirstTestedWithinTarget, ScopeFixture, name, firstTested, TargetTimeToFirstTested),
		shareWithin(MetricConclusionWithinTarget, ScopeFixture, name, conclude, TargetTimeToConclusion),
		pastHardStop(ScopeFixture, name, conclude),
		NewRow(MetricCost, ScopeFixture, name, cost, n, detailReported+": summed over this fixture's runs"),
		cacheReadRatio(ScopeFixture, name, cachedInput, totalInput, n),
		NewRow(MetricPriorMRR, ScopeFixture, name, mrrOrZero(priorRanks), n,
			"the deterministic ranker's MRR from `ground_truth.prior_rank_of_culprit`"),
		NewRow(MetricInvestigatorMRR, ScopeFixture, name, mrrOrZero(ranks), n, detailReported),
		NewRow(MetricLift, ScopeFixture, name, Lift(mrrOrZero(ranks), mrrOrZero(priorRanks)), n,
			detailGated+" at corpus scope only; per fixture it is reported"),
	)
	rows = append(rows, breakdown(MetricWorkerCalls, ScopeFixture, name, workerCalls)...)
	rows = append(rows, quotaShare(ScopeFixture, name, backendCalls)...)
	return rows, admitted
}

// corpusMissRows publishes the world-replay miss rate at corpus scope, against its published
// threshold, plus the per-term breakdown that says where a thin recording is thin (FR-108).
//
// It reports and never gates. A world that does not hold the answer to a reasonable question has
// told us something about the RECORDING — the cross product was too narrow, the window grid too
// coarse, the hop radius too small — and scoring the engine down for it would punish the engine
// for the recorder's omission, and reward an engine that asks fewer questions.
func corpusMissRows(runs []RunRow) []Row {
	var missed, terms int
	counts := map[string]int{}
	for _, run := range runs {
		missed += run.NotRecorded
		terms += run.TermsChecked
		for _, term := range run.MissedTerms {
			counts[term]++
		}
	}
	threshold := backend.MissRateThreshold
	for _, run := range runs {
		if run.MissRateThreshold > 0 {
			threshold = run.MissRateThreshold
			break
		}
	}
	rate, measured := Rate(missed, terms)
	if !measured {
		return []Row{NotMeasured(MetricMissRate, ScopeCorpus, "",
			"no in-algebra term was asked of a recorded world, so there is no miss rate to report")}
	}
	verdict := "inside"
	if rate > threshold {
		verdict = "ABOVE"
	}
	rows := []Row{NewRow(MetricMissRate, ScopeCorpus, "", rate, terms, fmt.Sprintf(
		"%d of %d in-algebra requests were not_recorded — %s the threshold %.2f. %s",
		missed, terms, verdict, threshold, detailAdmission))}
	for _, term := range sortedKeys(counts) {
		rows = append(rows, NewRow(MetricMissRate+"."+term, ScopeCorpus, "",
			float64(counts[term]), counts[term], fmt.Sprintf(
				"%s: %d not_recorded — widen the window grid, the neighbourhood or the "+
					"drill-down depth this term ranges over and re-record", term, counts[term])))
	}
	return rows
}

// corpusRows aggregates the admitted runs. Everything the gate reads at corpus scope is here.
func corpusRows(runs []RunRow, excluded []string, fixtures int) []Row {
	n := len(runs)
	if n == 0 {
		return []Row{NotMeasured(MetricPassAt1, ScopeCorpus, "", fmt.Sprintf(
			"no admitted runs: %d fixture(s) ran and %d were excluded on their miss rate (%s). "+
				"A corpus that scored nothing is not a corpus that passed",
			fixtures, len(excluded), strings.Join(excluded, ", ")))}
	}

	passes := make([]bool, 0, n)
	ranks := make([]int, 0, n)
	priorRanks := make([]int, 0, n)
	var (
		claims, validClaims, untraceable   int
		unknowns, nonUnknown, nonUnknownOK int
		harms, confidentlyWrong, topKHits  int
		diverged, improvised               int
		localisation, attribution, mech    []float64
		humanLabelled, humanAgreed         int
		humanRegressions                   int
		citationFailure                    string
		provisional, firstTested, conclude []float64
		cost                               float64
		cachedInput, totalInput            int64
		workerCalls                        = map[string]int{}
		backendCalls                       = map[string]int{}
		byFixturePasses                    = map[string][]bool{}
	)

	for _, run := range runs {
		passes = append(passes, run.Pass)
		ranks = append(ranks, run.CulpritRank)
		priorRanks = append(priorRanks, run.PriorRank)
		byFixturePasses[run.Fixture] = append(byFixturePasses[run.Fixture], run.Pass)
		claims += run.Claims
		validClaims += run.ValidClaims
		untraceable += run.Untraceable
		if citationFailure == "" && run.CitationFailure != "" {
			citationFailure = " — " + run.CitationFailure
		}
		provisional = appendIf(provisional, run.TimeToProvisional)
		firstTested = appendIf(firstTested, run.TimeToFirstTested)
		conclude = appendIf(conclude, run.TimeToConclusion)
		if run.Unknown {
			unknowns++
		} else {
			nonUnknown++
			if run.Correct {
				nonUnknownOK++
			}
		}
		if run.PriorRank == 1 && run.CulpritRank != 1 {
			harms++
		}
		if run.ConfidentlyWrong {
			confidentlyWrong++
		}
		if k := expectedTopK(run); run.CulpritRank > 0 && run.CulpritRank <= k {
			topKHits++
		}
		if run.Diverged {
			diverged++
		}
		if run.Improvised {
			improvised++
		}
		if run.HasCredit {
			localisation = append(localisation, run.Credit.Localisation)
			attribution = append(attribution, run.Credit.Attribution)
			mech = append(mech, run.Credit.Mechanism)
		}
		if run.HumanLabelAgreed != nil {
			humanLabelled++
			if *run.HumanLabelAgreed {
				humanAgreed++
			}
		}
		if run.HumanLabelRegressed {
			humanRegressions++
		}
		cost += run.CostUSD
		cachedInput += run.CachedInputTokens
		totalInput += run.TotalInputTokens
		addCounts(workerCalls, run.WorkerCalls)
		addCounts(backendCalls, run.BackendCalls)
	}

	investigator := mrrOrZero(ranks)
	prior := mrrOrZero(priorRanks)

	rows := []Row{
		rate(MetricPassAt1, ScopeCorpus, "", passes,
			detailGated+": the aggregate over fixtures × runs (FR-060)"),
		NewRow(MetricPassHatK, ScopeCorpus, "", corpusPassHatK(byFixturePasses), len(byFixturePasses),
			detailReported+": the share of fixtures that passed every one of their k runs"),
		bestOfK("", passes),
		NewRow(MetricInvestigatorMRR, ScopeCorpus, "", investigator, n, detailReported),
		NewRow(MetricPriorMRR, ScopeCorpus, "", prior, n,
			"the deterministic ranker over the same corpus and the same runs (FR-061a)"),
		NewRow(MetricLift, ScopeCorpus, "", Lift(investigator, prior), n,
			detailGated+": must be > 0 — a reasoning layer that does not beat the prior is not worth its cost"),
		fraction(MetricHarmRate, ScopeCorpus, "", harms, n, detailGated+": threshold 5 %"),
		fraction(MetricConfidentlyWrong, ScopeCorpus, "", confidentlyWrong, n, detailGated),
		fraction(MetricCitationValidity, ScopeCorpus, "", validClaims, claims,
			detailGated+": must be 100 %"+citationFailure),
		count(MetricUntraceable, ScopeCorpus, "", untraceable, claims, detailGated+": must be 0"),
		meanRow(MetricCulpritRank, ScopeCorpus, "", ranksAsFloats(ranks), detailReported+", never the headline"),
		fraction(MetricTopKHit, ScopeCorpus, "", topKHits, n, detailReported+", never the headline"),
		creditRow(MetricLocalisation, "", localisation),
		creditRow(MetricAttribution, "", attribution),
		creditRow(MetricMechanism, "", mech),
		fraction(MetricUnknownRate, ScopeCorpus, "", unknowns, n, detailReported),
		fraction(MetricNonUnknownPrecision, ScopeCorpus, "", nonUnknownOK, nonUnknown, detailReported),
		meanRow(MetricTimeToProvisional, ScopeCorpus, "", provisional, detailReported),
		meanRow(MetricTimeToFirstTested, ScopeCorpus, "", firstTested, detailReported),
		meanRow(MetricTimeToConclusion, ScopeCorpus, "", conclude, detailReported),
		shareWithin(MetricProvisionalWithinTarget, ScopeCorpus, "", provisional, TargetTimeToProvisional),
		shareWithin(MetricFirstTestedWithinTarget, ScopeCorpus, "", firstTested, TargetTimeToFirstTested),
		shareWithin(MetricConclusionWithinTarget, ScopeCorpus, "", conclude, TargetTimeToConclusion),
		pastHardStop(ScopeCorpus, "", conclude),
		fraction(MetricReplayDivergenceRate, ScopeCorpus, "", diverged, n, detailGated+": must be 0"),
		count(MetricImprovisedReplays, ScopeCorpus, "", improvised, n, detailGated+": must be 0"),
		NewRow(MetricCost, ScopeCorpus, "", cost, n, detailReported),
		cacheReadRatio(ScopeCorpus, "", cachedInput, totalInput, n),
		count(MetricHumanLabelRegressions, ScopeCorpus, "", humanRegressions, humanLabelled,
			detailGated+": a corrected case this run got wrong"),
	}
	if humanLabelled == 0 {
		rows = append(rows, NotMeasured(MetricHumanAgreement, ScopeCorpus, "",
			"n/a — no case in this corpus carries a human correction to agree with"))
	} else {
		rows = append(rows, fraction(MetricHumanAgreement, ScopeCorpus, "", humanAgreed, humanLabelled,
			detailReported+": agreement with the recorded human correction"))
	}
	if len(excluded) > 0 {
		rows = append(rows, NewRow(MetricFixtureAdmitted, ScopeCorpus, "",
			float64(len(byFixturePasses)), fixtures, fmt.Sprintf(
				"%d of %d fixtures admitted; excluded on their miss rate: %s",
				len(byFixturePasses), fixtures, strings.Join(excluded, ", "))))
	} else {
		rows = append(rows, NewRow(MetricFixtureAdmitted, ScopeCorpus, "",
			float64(len(byFixturePasses)), fixtures, "every fixture was admitted"))
	}
	rows = append(rows, breakdown(MetricWorkerCalls, ScopeCorpus, "", workerCalls)...)
	rows = append(rows, quotaShare(ScopeCorpus, "", backendCalls)...)
	return rows
}

// invarianceRows publishes T108's metamorphic checks, one row per variant plus the gated count.
//
// The results come from `CheckInvariance`, which owns the invariants themselves; this function
// decides only how they are published. A failing check's own sentence is carried through
// verbatim, because it already names the variant, the transform and both verdicts, and
// paraphrasing it here would be a second, worse copy of the message.
func invarianceRows(checks []InvarianceResult) []Row {
	changed := 0
	rows := make([]Row, 0, len(checks)+1)
	for _, check := range checks {
		value := 1.0
		if !check.Held {
			value = 0
			changed++
		}
		detail := fmt.Sprintf("%s of %s: parent answered %q, variant answered %q, invariant requires %q",
			check.Transform, check.Parent, check.ParentVerdict, check.VariantVerdict, check.ExpectedVerdict)
		if check.Detail != "" {
			detail += " — " + check.Detail
		}
		if check.NamedDecoy != "" {
			detail += fmt.Sprintf("; it named the decoy %s", check.NamedDecoy)
		}
		rows = append(rows, NewRow(MetricMetamorphic+"."+check.Transform, ScopeFixture,
			check.Variant, value, 1, detail))
	}
	if len(checks) == 0 {
		// n = 0, value 0. The distinction matters and the row carries it: a zero-tolerance gate
		// over zero observations holds *vacuously*, which is not the same as holding. The gate
		// prints it as NOT RUN with an annotation rather than as a pass, for the same reason the
		// unset pass@1 threshold prints as `unset` — and the n on the row is what tells it which
		// of the two it is looking at.
		return append(rows, NewRow(MetricMetamorphicChanges, ScopeCorpus, "", 0, 0,
			"NOT RUN: no metamorphic variant was checked in this run. `fixture derive` generates "+
				"them and they are not checked in (FR-062a), so this gate held over nothing and is "+
				"not evidence that the invariants hold"))
	}
	return append(rows, count(MetricMetamorphicChanges, ScopeCorpus, "", changed, len(checks),
		detailGated+": any variant whose verdict changed fails the build (SC-020)"))
}

// provenanceRows record what produced the numbers. A run that does not say which corpus and
// which model configuration it used has published an anecdote.
func provenanceRows(in Input, trials, fixtures int) []Row {
	corpus := in.Corpus
	if corpus == "" {
		corpus = "unnamed"
	}
	modelDetail := fmt.Sprintf("model configuration %s (digest %s)",
		orDash(in.ModelConfigVersion), orDash(in.ModelConfigDigest))
	if in.ModelFree {
		modelDetail = "MODEL-FREE: the deterministic engine only, no model call was made. " +
			"These numbers are NOT the production result and must not be published as one"
	}
	return []Row{
		NotMeasured(MetricCorpus, ScopeCorpus, "", fmt.Sprintf("%s corpus, read from %s, citing audit %s",
			corpus, orDash(in.FixtureRoot), orDash(in.AuditPath))),
		NotMeasured(MetricModelConfig, ScopeCorpus, "", modelDetail),
		NewRow(MetricFixtures, ScopeCorpus, "", float64(fixtures), fixtures, "fixtures in this run"),
		NewRow(MetricRuns, ScopeCorpus, "", float64(in.RunsPerFixture), in.RunsPerFixture,
			"runs per fixture (3, extended to 7 only when the first 3 disagree — FR-061)"),
		NewRow(MetricTrials, ScopeCorpus, "", float64(trials), trials,
			"Bernoulli trials behind every gated rate: fixtures × runs over the admitted fixtures"),
	}
}

// ---------- row helpers ----------

func rate(metric string, scope Scope, fixture string, passes []bool, detail string) Row {
	var (
		value float64
		ok    bool
	)
	if metric == MetricPassHatK {
		value, ok = PassHatK(passes)
	} else {
		value, ok = PassAt1(passes)
	}
	if !ok {
		return NotMeasured(metric, scope, fixture, "no runs")
	}
	return NewRow(metric, scope, fixture, value, len(passes), detail)
}

func bestOfK(fixture string, passes []bool) Row {
	scope := ScopeFixture
	if fixture == "" {
		scope = ScopeCorpus
	}
	if len(passes) == 0 {
		return NotMeasured(MetricBestOfK, scope, fixture, "no runs")
	}
	value := 0.0
	for _, pass := range passes {
		if pass {
			value = 1
			break
		}
	}
	return NewRow(MetricBestOfK, scope, fixture, value, len(passes), detailNeverGated)
}

func fraction(metric string, scope Scope, fixture string, hits, total int, detail string) Row {
	value, ok := Rate(hits, total)
	if !ok {
		return NotMeasured(metric, scope, fixture, "nothing to measure it over (n = 0)")
	}
	return NewRow(metric, scope, fixture, value, total, fmt.Sprintf("%d of %d — %s", hits, total, detail))
}

func count(metric string, scope Scope, fixture string, found, over int, detail string) Row {
	return NewRow(metric, scope, fixture, float64(found), over, detail)
}

func boolRow(metric string, scope Scope, fixture string, value bool, n int, detail string) Row {
	numeric := 0.0
	if value {
		numeric = 1
	}
	return NewRow(metric, scope, fixture, numeric, n, detail)
}

// shareWithin publishes how many measured runs met a deadline, as a fraction with the deadline in
// its detail so a reader never has to go looking for which number the share is against.
func shareWithin(metric string, scope Scope, fixture string, values []float64, target time.Duration) Row {
	hits, total := withinTarget(values, target)
	return fraction(metric, scope, fixture, hits, total,
		fmt.Sprintf("%s: share of runs within %s (SC-013, T117)", detailReported, target))
}

// pastHardStop counts the runs that ran past the profile's hard stop. It is a count rather than a
// share because the target is zero and a share of zero reads the same whether one run in four
// overran or one in four hundred.
func pastHardStop(scope Scope, fixture string, conclude []float64) Row {
	within, total := withinTarget(conclude, HardStop)
	return count(MetricRunsPastHardStop, scope, fixture, total-within, total,
		fmt.Sprintf("%s: runs whose conclusion came after the %s hard stop (SC-013, T117)",
			detailReported, HardStop))
}

// cacheReadRatio is cached input over total input, or an explicit `n/a` for a run that called no
// model at all (FR-067's model-free path, which is most of this repository's own CI).
//
// Zero would be a lie there in the direction that matters: a model-free run did not fail to hit
// the cache, it never asked, and a trend line that plots those as 0 % would show a cache
// regression every time the gate ran without a credential.
func cacheReadRatio(scope Scope, fixture string, cached, total int64, n int) Row {
	if total <= 0 {
		return NotMeasured(MetricCacheReadRatio, scope, fixture,
			"n/a — no model call was made, so there was no input to cache "+
				"("+detailNeverGated+")")
	}
	return NewRow(MetricCacheReadRatio, scope, fixture, float64(cached)/float64(total), n,
		fmt.Sprintf("%s: %d of %d input tokens served from the cache", detailNeverGated,
			cached, total))
}

func meanRow(metric string, scope Scope, fixture string, values []float64, detail string) Row {
	value, ok := Mean(values)
	if !ok {
		return NotMeasured(metric, scope, fixture, "no run published this measurement")
	}
	return NewRow(metric, scope, fixture, value, len(values), detail)
}

func creditRow(metric, fixture string, values []float64) Row {
	scope := ScopeFixture
	if fixture == "" {
		scope = ScopeCorpus
	}
	value, ok := Mean(values)
	if !ok {
		return NotMeasured(metric, scope, fixture,
			"this fixture's ground truth publishes no `causal_path`, so there is nothing to score against")
	}
	return NewRow(metric, scope, fixture, value, len(values),
		detailReported+": partial credit against `causal_path` (FR-061b)")
}

func breakdown(metric string, scope Scope, fixture string, counts map[string]int) []Row {
	total := 0
	for _, value := range counts {
		total += value
	}
	rows := []Row{NewRow(metric, scope, fixture, float64(total), len(counts), detailReported)}
	for _, key := range sortedKeys(counts) {
		rows = append(rows, NewRow(metric+"."+key, scope, fixture, float64(counts[key]), counts[key],
			detailReported+": calls to "+key))
	}
	return rows
}

func quotaShare(scope Scope, fixture string, counts map[string]int) []Row {
	total := 0
	for _, value := range counts {
		total += value
	}
	if total == 0 {
		return []Row{NotMeasured(MetricBackendQuotaShare, scope, fixture,
			"no backend call was issued, so there is no quota to share")}
	}
	rows := make([]Row, 0, len(counts))
	for _, key := range sortedKeys(counts) {
		rows = append(rows, NewRow(MetricBackendQuotaShare+"."+key, scope, fixture,
			float64(counts[key])/float64(total), total, detailReported+": share of the algebra calls issued"))
	}
	return rows
}

func admissionDetail(admitted bool, missRate float64, measured bool, threshold float64) string {
	if !measured {
		return "no world-replay term was checked, so the miss rate is unmeasured and the fixture " +
			"is admitted; an absent measurement is not a failing one"
	}
	if admitted {
		return fmt.Sprintf("miss rate %.4f ≤ %.2f", missRate, threshold)
	}
	return fmt.Sprintf(
		"EXCLUDED: miss rate %.4f > %.2f. The recording cannot answer what the engine asks, so this "+
			"fixture's runs are not scored; this gates the fixture, never the engine (FR-060)",
		missRate, threshold)
}

func corpusPassHatK(byFixture map[string][]bool) float64 {
	if len(byFixture) == 0 {
		return 0
	}
	clean := 0
	for _, passes := range byFixture {
		if value, ok := PassHatK(passes); ok && value == 1 {
			clean++
		}
	}
	return float64(clean) / float64(len(byFixture))
}

func expectedTopK(run RunRow) int {
	if run.ExpectedTopK > 0 {
		return run.ExpectedTopK
	}
	return 3
}

func mrrOrZero(ranks []int) float64 {
	value, _ := MeanReciprocalRank(ranks)
	return value
}

func ranksAsFloats(ranks []int) []float64 {
	out := make([]float64, 0, len(ranks))
	for _, rank := range ranks {
		out = append(out, float64(rank))
	}
	return out
}

func appendIf(values []float64, value *float64) []float64 {
	if value == nil {
		return values
	}
	return append(values, *value)
}

func addCounts(into, from map[string]int) {
	for key, value := range from {
		into[key] += value
	}
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

// ---------- Markdown ----------

// Markdown renders the report for the job summary and for
// `docs/evaluation/investigation-metrics.md`. It is the same rows, so the summary and the
// document can never disagree with the JSONL the gate read.
func (r *Report) Markdown() string {
	var out strings.Builder
	generated := r.Input.GeneratedAt
	if generated.IsZero() {
		generated = time.Now().UTC()
	}

	fmt.Fprintf(&out, "### Investigation eval — %s corpus\n\n", orDash(r.Input.Corpus))
	fmt.Fprintf(&out, "Generated %s · fixtures `%s` · audit `%s` · %s\n\n",
		generated.UTC().Format(time.RFC3339), orDash(r.Input.FixtureRoot), orDash(r.Input.AuditPath),
		modelLine(r.Input))
	if r.Input.ModelFree {
		out.WriteString("> **MODEL-FREE RUN.** Every number below comes from the deterministic engine " +
			"with no model call. It is not the production result and must not be published as one.\n\n")
	}

	out.WriteString(r.headlineTable())
	out.WriteString(r.fixtureTable())
	out.WriteString(r.reportedTable())
	out.WriteString(r.invarianceSection())
	out.WriteString(r.gapSection())
	out.WriteString(r.powerSection())
	return out.String()
}

func modelLine(in Input) string {
	if in.ModelFree {
		return "model-free"
	}
	return fmt.Sprintf("model configuration %s (digest %s)",
		orDash(in.ModelConfigVersion), orDash(in.ModelConfigDigest))
}

func (r *Report) headlineTable() string {
	var out strings.Builder
	out.WriteString("#### Gated (corpus)\n\n| metric | value | n | note |\n|---|---:|---:|---|\n")
	for _, metric := range []string{
		MetricPassAt1, MetricLift, MetricHarmRate, MetricConfidentlyWrong, MetricCitationValidity,
		MetricUntraceable, MetricReplayDivergenceRate, MetricImprovisedReplays,
		MetricMetamorphicChanges, MetricHumanLabelRegressions,
	} {
		row, ok := r.row(metric, ScopeCorpus, "")
		if !ok {
			continue
		}
		fmt.Fprintf(&out, "| `%s` | %s | %d | %s |\n", row.Metric, valueOf(row), row.N, row.Detail)
	}
	out.WriteString("\n")
	return out.String()
}

func (r *Report) reportedTable() string {
	var out strings.Builder
	out.WriteString("#### Reported, not gated (corpus)\n\n| metric | value | n |\n|---|---:|---:|\n")
	for _, metric := range []string{
		MetricPassHatK, MetricBestOfK, MetricInvestigatorMRR, MetricPriorMRR, MetricCulpritRank,
		MetricTopKHit, MetricLocalisation, MetricAttribution, MetricMechanism, MetricUnknownRate,
		MetricNonUnknownPrecision, MetricHumanAgreement, MetricCost, MetricCacheReadRatio,
		MetricWorkerCalls,
	} {
		row, ok := r.row(metric, ScopeCorpus, "")
		if !ok {
			continue
		}
		fmt.Fprintf(&out, "| `%s` | %s | %d |\n", row.Metric, valueOf(row), row.N)
	}
	out.WriteString("\n")
	return out.String()
}

func (r *Report) fixtureTable() string {
	var out strings.Builder
	out.WriteString("#### Per fixture\n\n")
	out.WriteString("| fixture | pass@1 | pass^k | mean rank | investigator MRR | prior MRR | lift | citations | miss rate | admitted |\n")
	out.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
	for _, name := range r.fixtures() {
		admitted := "yes"
		if row, ok := r.row(MetricFixtureAdmitted, ScopeFixture, name); ok {
			if value, has := row.Float(); has && value == 0 {
				admitted = "**no**"
			}
		}
		fmt.Fprintf(&out, "| `%s` | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", name,
			r.cell(MetricPassAt1, name), r.cell(MetricPassHatK, name),
			r.cell(MetricCulpritRank, name), r.cell(MetricInvestigatorMRR, name),
			r.cell(MetricPriorMRR, name), r.cell(MetricLift, name),
			r.cell(MetricCitationValidity, name), r.cell(MetricMissRate, name), admitted)
	}
	out.WriteString("\n")
	return out.String()
}

func (r *Report) invarianceSection() string {
	var out strings.Builder
	out.WriteString("#### Metamorphic invariance (FR-062a, SC-020)\n\n")
	rows := r.prefixed(MetricMetamorphic + ".")
	if len(rows) == 0 {
		out.WriteString("No metamorphic variant was run. `fixture derive` generates them; they are not " +
			"checked in.\n\n")
		return out.String()
	}
	out.WriteString("| variant | transform | held | detail |\n|---|---|---|---|\n")
	for _, row := range rows {
		held := "yes"
		if value, ok := row.Float(); ok && value == 0 {
			held = "**NO**"
		}
		fmt.Fprintf(&out, "| `%s` | `%s` | %s | %s |\n", row.Fixture,
			strings.TrimPrefix(row.Metric, MetricMetamorphic+"."), held, row.Detail)
	}
	out.WriteString("\n")
	return out.String()
}

func (r *Report) gapSection() string {
	var out strings.Builder
	out.WriteString("#### Corpus gaps (FR-071b)\n\n")
	if row, ok := r.row(MetricCorpusGaps, ScopeCorpus, ""); ok {
		fmt.Fprintf(&out, "%s\n\n", row.Detail)
	}
	rows := r.prefixed(MetricCorpusGaps + ".")
	if len(rows) == 0 {
		return out.String()
	}
	out.WriteString("| category | covered | detail |\n|---|---|---|\n")
	for _, row := range rows {
		covered := "yes"
		if value, ok := row.Float(); ok && value == 1 {
			covered = "**no**"
		}
		fmt.Fprintf(&out, "| `%s` | %s | %s |\n",
			strings.TrimPrefix(row.Metric, MetricCorpusGaps+"."), covered, row.Detail)
	}
	out.WriteString("\n")
	return out.String()
}

func (r *Report) powerSection() string {
	baseline, ok := 0.0, false
	if row, found := r.row(MetricPassAt1, ScopeCorpus, ""); found {
		baseline, ok = row.Float()
	}
	if !ok {
		return fmt.Sprintf("#### Detection power\n\nNo aggregate pass@1 was measured, so there is "+
			"nothing to state a detection power for. n would have been %d Bernoulli trials.\n\n", r.Trials)
	}
	if baseline <= 0 {
		return fmt.Sprintf("#### Detection power\n\nThe corpus passed **nothing** over %d Bernoulli "+
			"trial(s). A detection power is a statement about regressions from a baseline, and there "+
			"is no baseline here to regress from.\n\n", r.Trials)
	}
	power := DetectionPower(r.Trials, baseline)
	return fmt.Sprintf("#### Detection power\n\n%s\n\nFor reference at this n: %s.\n\n",
		power.Sentence(), power.Example(0.70, 0.80))
}

func (r *Report) row(metric string, scope Scope, fixture string) (Row, bool) {
	for _, row := range r.Rows {
		if row.Metric == metric && row.Scope == scope && row.Fixture == fixture {
			return row, true
		}
	}
	return Row{}, false
}

func (r *Report) prefixed(prefix string) []Row {
	var out []Row
	for _, row := range r.Rows {
		if strings.HasPrefix(row.Metric, prefix) {
			out = append(out, row)
		}
	}
	return out
}

func (r *Report) fixtures() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, row := range r.Rows {
		if row.Scope != ScopeFixture || row.Fixture == "" {
			continue
		}
		if _, ok := seen[row.Fixture]; ok {
			continue
		}
		seen[row.Fixture] = struct{}{}
		out = append(out, row.Fixture)
	}
	sort.Strings(out)
	return out
}

func (r *Report) cell(metric, fixture string) string {
	row, ok := r.row(metric, ScopeFixture, fixture)
	if !ok {
		return "n/a"
	}
	return valueOf(row)
}

func valueOf(row Row) string {
	value, ok := row.Float()
	if !ok {
		return "n/a"
	}
	switch row.Metric {
	case MetricPassAt1, MetricPassHatK, MetricHarmRate, MetricConfidentlyWrong,
		MetricCitationValidity, MetricTopKHit, MetricUnknownRate, MetricNonUnknownPrecision,
		MetricMissRate, MetricReplayDivergenceRate, MetricHumanAgreement, MetricOnsetWithinTolerance,
		MetricCacheReadRatio:
		return percent(&value)
	case MetricUntraceable, MetricImprovisedReplays, MetricMetamorphicChanges,
		MetricHumanLabelRegressions, MetricCorpusGaps, MetricTrials, MetricFixtures, MetricRuns,
		MetricWorkerCalls:
		return fmt.Sprintf("%.0f", value)
	default:
		return fmt.Sprintf("%.4f", value)
	}
}

// ---------- the published document ----------

// The markers that bound the generated tables in `docs/evaluation/investigation-metrics.md`.
// The document's prose is written by people and is never touched; only what is between these
// two lines is regenerated, so a run can refresh the numbers without eating the explanation of
// what they mean.
const (
	docBeginMarker = "<!-- BEGIN GENERATED: eval rows -->"
	docEndMarker   = "<!-- END GENERATED: eval rows -->"
)

// WriteDoc regenerates the tables section of the published metrics document from these rows,
// keeping every line of its prose.
//
// A document with no markers gets the section appended with its markers, so the first run
// installs them and every later run replaces between them.
func (r *Report) WriteDoc(path string) error {
	existing, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied document.
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("eval: read %s: %w", path, err)
		}
		existing = []byte("# Investigation metrics\n")
	}
	updated := spliceDoc(string(existing), r.Markdown())
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("eval: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("eval: write %s: %w", path, err)
	}
	return nil
}

// spliceDoc replaces the generated block, or appends one.
func spliceDoc(doc, generated string) string {
	block := docBeginMarker + "\n\n" + strings.TrimRight(generated, "\n") + "\n\n" + docEndMarker
	begin := strings.Index(doc, docBeginMarker)
	end := strings.Index(doc, docEndMarker)
	if begin >= 0 && end > begin {
		return doc[:begin] + block + doc[end+len(docEndMarker):]
	}
	return strings.TrimRight(doc, "\n") + "\n\n## Latest evaluation run\n\n" + block + "\n"
}

// ---------- adapting the harness's outcomes ----------

// RowOptions is what the adapter needs that a `RunOutcome` does not carry: facts about the
// fixture rather than about the run.
type RowOptions struct {
	// MissRateThreshold is the fixture's own `world.miss_rate_threshold`; 0 means the default.
	MissRateThreshold float64
	// ExpectedTopK is the k the top-k hit is measured at; 0 means 3 (SC-005's published k).
	ExpectedTopK int
	// WorldDigests maps a recorded world's response key to the digest it holds, so a citation
	// can be checked against the recording rather than only against itself. Nil means the
	// structural check only, and the detail line says which check was made.
	WorldDigests map[string]string
	// HumanLabel is the recorded human correction for this fixture, where one exists: the
	// verdict a person said was right. Empty means the case carries no label, which is
	// published as `n/a` and never as a disagreement.
	HumanLabel string
	// PreviouslyAgreed says this labelled case agreed with its label on the previous published
	// run, so a disagreement now is a *regression* rather than a standing difference. It is
	// what FR-060's "any regression against a corrected or human-labelled case" gates on.
	PreviouslyAgreed bool
}

// RunRowsFrom adapts one fixture's graded run set into the report's rows.
//
// The grades come from `Grade`, which owns the knowability rule and the decoy rules; nothing
// here re-decides whether a run passed. What it does decide is how a pass, a rank and a citation
// become numbers — which is the report's job and nobody else's.
func RunRowsFrom(set *RunSet, grades []GradeResult, truth GroundTruth, opts RowOptions) []RunRow {
	if set == nil {
		return nil
	}
	rows := make([]RunRow, 0, len(set.Outcomes))
	for i, outcome := range set.Outcomes {
		grade := GradeResult{}
		if i < len(grades) {
			grade = grades[i]
		}
		rows = append(rows, runRowFrom(outcome, grade, truth, opts))
	}
	return rows
}

// make the reader chase which field came from where.
//
//nolint:funlen // One run's translation is a field-by-field mapping; splitting it would only
func runRowFrom(outcome *RunOutcome, grade GradeResult, truth GroundTruth, opts RowOptions) RunRow {
	row := RunRow{
		Fixture:           outcome.FixtureID,
		Run:               outcome.RunIndex,
		Pass:              grade.Pass,
		Outcome:           outcome.Verdict,
		Unknown:           outcome.IsUnknown(),
		Correct:           grade.Pass && !outcome.IsUnknown(),
		Confidence:        outcome.VerdictBucket,
		ExpectedTopK:      opts.ExpectedTopK,
		MissRateThreshold: opts.MissRateThreshold,
		NotRecorded:       outcome.NotRecorded,
		MissedTerms:       outcome.MissedTerms,
		WorkerCalls:       outcome.WorkerCalls,
		BackendCalls:      backendCallsOf(outcome),
		CostUSD:           spendOf(outcome),
		Diverged:          outcome.ReplayChecked && !outcome.ReplayIdentical,
	}
	row.CachedInputTokens, row.TotalInputTokens = inputTokensOf(outcome)

	// The miss rate is published as a rate; the report wants its two halves so the corpus
	// denominator is the number of terms actually asked for rather than a mean of rates.
	row.TermsChecked = termsChecked(outcome)

	if hypothesis, ok := outcome.Hypothesis(truth.Culprit); ok {
		row.CulpritRank = hypothesis.Rank
	}
	if truth.PriorRankOfCulprit != nil {
		row.PriorRank = *truth.PriorRankOfCulprit
	}
	row.ConfidentlyWrong = confidentlyWrong(outcome, truth)

	if len(truth.CausalPath) > 0 {
		row.HasCredit = true
		row.Credit = Credit(
			pathEntities(truth.CausalPath), outcomePathEntities(outcome.CausalPath),
			pathVia(truth.CausalPath), outcomePathVia(outcome.CausalPath))
	}

	row.Claims, row.ValidClaims, row.Untraceable, row.CitationFailure = checkCitations(outcome.Citations, opts.WorldDigests)
	if row.Untraceable > 0 {
		row.Improvised = false
	}

	if truth.Onset != nil && outcome.Onset != nil && outcome.Onset.Available {
		delta := outcome.Onset.At.Sub(truth.Onset.At).Seconds()
		if delta < 0 {
			delta = -delta
		}
		row.OnsetErrorSeconds = &delta
		tolerance := float64(truth.Onset.ToleranceSeconds)
		within := tolerance > 0 && delta <= tolerance
		row.OnsetWithinTolerance = &within
	}

	row.TimeToProvisional = secondsOf(outcome.TimeToProvisional)
	row.TimeToFirstTested = secondsOf(outcome.TimeToFirstTestedHypothis)
	row.TimeToConclusion = secondsOf(outcome.TimeToConclusion)

	if opts.HumanLabel != "" {
		agreed := outcome.Verdict == opts.HumanLabel
		row.HumanLabelAgreed = &agreed
		row.HumanLabelRegressed = opts.PreviouslyAgreed && !agreed
	}
	return row
}

// termsChecked is the denominator of the miss rate: how many in-algebra questions the run put to
// the recorded world. It is recovered from the published rate rather than counted again, so the
// two can never disagree; a rate of 0 with misses recorded means the outcome published no
// denominator, and the misses are reported over themselves rather than dropped.
func termsChecked(outcome *RunOutcome) int {
	if outcome.MissRate > 0 {
		return int(math.Round(float64(outcome.NotRecorded) / outcome.MissRate))
	}
	if outcome.NotRecorded > 0 {
		return outcome.NotRecorded
	}
	return outcome.WorkerCallsTotal
}

// confidentlyWrong is the harm this project is most afraid of: a non-culprit named at `high` or
// `very_high`. An `unknown` is never confidently wrong — refusing to answer is the behaviour the
// gate is trying to protect, not the one it is trying to catch.
func confidentlyWrong(outcome *RunOutcome, truth GroundTruth) bool {
	if outcome.IsUnknown() || outcome.VerdictClass() == VerdictUnobserved {
		return false
	}
	for _, ref := range outcome.NamedChanges() {
		if ref == truth.Culprit {
			return false
		}
	}
	switch outcome.VerdictBucket {
	case "high", "very_high":
		return true
	default:
		return false
	}
}

// GraphWorker is the worker whose answers the recorded world deliberately does not hold.
//
// This is not an exception carved out to make a number look better; it is the recording contract.
// A graph answer comes from replaying the fixture's own `events.jsonl` into an empty database
// (see `internal/investigation/replay`'s note on why layer 1 does not re-run the engine's control
// flow): recording it as well would create a second source of truth for the same answer, and the
// two would eventually disagree. So a graph citation is checked against the graph's own replay —
// which the trajectory gate already proves is byte-identical on every pull request — and a
// telemetry citation is checked against `world/`.
const GraphWorker = "graph"

// checkCitations computes citation validity's numerator and denominator, and the count of claims
// that resolve to nothing at all (FR-061a, FR-060).
//
// Two different failures, counted separately because they are fixed in different places: a claim
// with no evidence id behind it is **untraceable** and is a gate of its own; a claim whose
// evidence id resolves to a digest the recording does not hold is **invalid**, which is the
// citation-validity gate.
//
// Where a world was supplied, a telemetry citation is checked against it: the key it addresses
// must be in the recording and the digest must be the one the recording holds. A graph citation
// is checked structurally, for the reason `GraphWorker` states — checking it against `world/`
// would be checking it against the wrong recording, and would report 100 % of the graph's
// citations as invalid on a corpus where every one of them is sound.
func checkCitations(citations []Citation, world map[string]string) (claims, valid, untraceable int, why string) {
	for _, citation := range citations {
		claims++
		if strings.TrimSpace(citation.EvidenceID) == "" {
			untraceable++
			continue
		}
		// An answer that is not an answer addresses no digest, and there is nothing here to
		// check it against. It is not a defect and it is not a fabrication: the run asked, the
		// backend said no, and the ledger recorded the refusal with its reason.
		if addressesNoDigest(citation.Outcome) {
			valid++
			continue
		}
		if strings.TrimSpace(citation.ResponseDigest) == "" {
			why = firstReason(why, citation, "the evidence item carries no response digest")
			continue
		}
		if world != nil && citation.Worker != GraphWorker && !horizonTruncated(citation) {
			recorded, held := world[citation.ResponseKey]
			switch {
			case !held:
				why = firstReason(why, citation, "the recording holds no answer under key "+short(citation.ResponseKey))
				continue
			case recorded != citation.ResponseDigest:
				why = firstReason(why, citation, "the recording holds "+short(recorded)+
					" under key "+short(citation.ResponseKey)+", the run cited "+short(citation.ResponseDigest))
				continue
			}
		}
		valid++
	}
	return claims, valid, untraceable, why
}

// addressesNoDigest says the cited answer is one of the published outcomes that produces no
// bounded answer to compare against a recording (FR-027).
//
// `not_recorded` is the load-bearing one, and keeping it out of this gate is what stops two gates
// from measuring the same thing. "The recording does not hold this term" is an *answer*: the
// backend produced it, the trajectory holds it, the ledger recorded it under an evidence id, and a
// conclusion that says "I asked and there was nothing" is resting on exactly that. How much of it
// a fixture produces is the `not_recorded` miss rate, which gates the fixture's admission to the
// corpus and nothing else (FR-060). Counting the same fact again as an *invalid citation* made a
// recording gap fail a 100 % gate about fabricated evidence, and made the citation gate
// unsatisfiable for any fixture whose miss rate was not exactly zero — which is not what a
// 5 % admission bar means.
//
// `query_failed` is the same shape from the other direction: a worker that refused is an answer
// the model must be able to read (FR-027), and it carries no digest by construction.
//
// What the gate still catches is untouched, and it is the thing it exists for: a citation whose
// answer *did* produce a digest must resolve to the digest the recording holds under its key.
func addressesNoDigest(outcome string) bool {
	switch outcome {
	case "not_recorded", "query_failed":
		return true
	default:
		return false
	}
}

// horizonTruncated says the run's own answer states it was cut back to the investigation's
// observed_at, which is why such a citation is checked structurally rather than against the
// recording's digest.
//
// A horizon-truncated answer is **served from the recording and then re-digested**: the recorded
// backend looks the answer up under the clamped key, stamps the horizon into its coverage
// (constitution II — the statement belongs in the answer, not beside it) and recomputes the
// digest over what actually leaves the process. So the digest a run cites is legitimately not the
// digest the world file holds, and the recording's digest is not even a function of the key alone
// — a caller that asks the already-clamped window gets the recorded digest, and one that asks a
// window running past the horizon gets the annotated one. Comparing the two was reporting sound
// citations as invalid: it is what put live citation validity at 11 of 12 on a fixture whose
// `not_recorded` miss rate was 0 of 12, with no wrong evidence id anywhere in the run.
//
// A `past_horizon` answer — NO_DATA derived from the horizon itself, without consulting the world
// at all — carries the same statement and is covered by the same rule, which is right for the
// same reason: the world is not what makes a future window empty.
//
// Nothing about this weakens the gate. The statement is the backend's, not the model's: it
// reaches the citation through the evidence item the engine minted from a real answer, so a run
// cannot claim it for an evidence id it invented, and every citation that is *not* so annotated
// is still checked digest-for-digest against the recording.
func horizonTruncated(citation Citation) bool {
	if len(citation.Digest) == 0 {
		return false
	}
	var body struct {
		Coverage struct {
			TruncatedToHorizon bool `json:"truncated_to_horizon"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal(citation.Digest, &body); err != nil {
		return false
	}
	return body.Coverage.TruncatedToHorizon
}

// firstReason keeps the first invalid citation's reason, so the gate says which claim failed and
// why rather than only that one did. A gate a reader cannot act on is a gate that gets muted.
func firstReason(existing string, citation Citation, detail string) string {
	if existing != "" {
		return existing
	}
	where := citation.Term
	if citation.Pointer != "" {
		where += " over " + citation.Pointer
	}
	return "first invalid: " + citation.EvidenceID + " (" + where + ", outcome " + citation.Outcome + "): " + detail
}

// short is the first 12 characters of a digest or key, which is enough to tell two apart in a
// gate line and short enough to read.
func short(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func backendCallsOf(outcome *RunOutcome) map[string]int {
	out := map[string]int{}
	for key, count := range outcome.WorkerCalls {
		worker, _, found := strings.Cut(key, "/")
		if !found {
			worker = key
		}
		out[worker] += count
	}
	return out
}

func spendOf(outcome *RunOutcome) float64 {
	if outcome.Spend == nil {
		return 0
	}
	return outcome.Spend.GetCostUnits()
}

// inputTokensOf reads the two halves of the cache-read ratio out of the recorded spend.
//
// `tokens_by_model_and_class` is the published provider-independent unit (FR-048): one entry per
// `<model id>:<class>`, with the four classes `input`, `cache_write`, `cache_read` and `output`.
// The ratio is over the three input-side classes summed across every model a run used, because a
// run that put the investigator on one provider and the verifier on another still has one prefix
// discipline and one bill.
func inputTokensOf(outcome *RunOutcome) (cached, total int64) {
	for key, tokens := range outcome.Spend.GetTokensByModelAndClass() {
		class := key
		if idx := strings.LastIndex(key, ":"); idx >= 0 {
			class = key[idx+1:]
		}
		switch class {
		case model.ClassCacheRead:
			cached += tokens
			total += tokens
		case model.ClassInput, model.ClassCacheWrite:
			total += tokens
		}
	}
	return cached, total
}

func secondsOf(d time.Duration) *float64 {
	seconds := d.Seconds()
	return &seconds
}

func pathEntities(steps []fixture.CausalStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Entity)
	}
	return out
}

func pathVia(steps []fixture.CausalStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.Via != "" {
			out = append(out, step.Via)
		}
	}
	return out
}

func outcomePathEntities(steps []PathStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Entity)
	}
	return out
}

func outcomePathVia(steps []PathStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.Via != "" {
			out = append(out, step.Via)
		}
	}
	return out
}
