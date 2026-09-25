// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
)

// Calibration (tasks.md T096 and T098; **reused verbatim by T110**; FR-023, SC-004, plan F9,
// constitution §Replay CI (c)).
//
// This file is deliberately its own file and its own set of pure functions, because the same
// arithmetic serves two cadences and must not fork between them:
//
//   - `ci.yml` calls it over **recorded trajectories, with zero model calls**, on every pull
//     request. Confidences are ledger-computed and deterministic, so a replay reproduces them
//     exactly and a per-PR reliability table costs nothing;
//   - `eval.yml` (T110) calls the same `Reliability` over **live runs** across the corpus × k,
//     and pairs the Brier score against the previous version on the same fixtures.
//
// T110 adds the paired comparison and the publishing; it does not re-derive any of the numbers
// below. If you are here from T110: take `Observation`, `Reliability` and `RenderMarkdown` as
// they are.
//
// Three things the table states that a bare accuracy number does not.
//
//  1. **The bucket's own range.** A reliability table is a claim that "moderate" means something,
//     and the only way to read it is against the range the bucket publishes (FR-023).
//  2. **Under-powered buckets are reported as under-powered, never scored.** SC-004 sets the bar
//     at 20 hypotheses per bucket per evaluation run. A bucket holding three hypotheses has an
//     observed accuracy of 0, 0.33, 0.67 or 1.0 and none of those numbers means anything; printing
//     one as though it did is how a calibration report becomes decoration.
//  3. **Brier and log-loss over everything scored**, so that a distribution which is well
//     calibrated bucket by bucket but confidently wrong overall still shows up.

// MinBucketSamples is the published bar SC-004 sets: a bucket holding fewer than this many
// hypotheses is reported as under-powered rather than scored.
const MinBucketSamples = 20

// logLossEpsilon clamps a probability away from 0 and 1 so that a single confident mistake does
// not make the log-loss infinite. It is the usual 1e-15; the clamp is published rather than
// silent because it is the one place the score is not the textbook formula.
const logLossEpsilon = 1e-15

// Observation is one scored hypothesis: what the engine believed and whether it was right.
type Observation struct {
	// Fixture is the incident the hypothesis was about.
	Fixture string `json:"fixture"`
	// RunID names the recorded run it came from.
	RunID string `json:"run_id"`
	// HypothesisID is the ledger id.
	HypothesisID string `json:"hypothesis_id"`
	// Statement is what was believed, kept so a reviewer can read a mis-calibrated bucket.
	Statement string `json:"statement,omitempty"`
	// Confidence is the ledger-computed posterior.
	Confidence float64 `json:"confidence"`
	// Correct is whether the hypothesis is true of the fixture's ground truth.
	Correct bool `json:"correct"`
}

// BucketRow is one row of the reliability table.
type BucketRow struct {
	// Bucket is the published bucket, range and all.
	Bucket ledger.Bucket `json:"bucket"`
	// Count is how many hypotheses landed in it.
	Count int `json:"count"`
	// Correct is how many of them were right.
	Correct int `json:"correct"`
	// ObservedAccuracy is Correct/Count, and is meaningless — and reported as such — when the
	// bucket is under-powered.
	ObservedAccuracy float64 `json:"observed_accuracy"`
	// MeanConfidence is the average confidence of the hypotheses in the bucket, which is what
	// the observed accuracy is being compared against.
	MeanConfidence float64 `json:"mean_confidence"`
	// UnderPowered is true when Count < MinSamples.
	UnderPowered bool `json:"under_powered"`
}

// Reliability is the whole table plus the two aggregate scores.
type Reliability struct {
	// MinSamples is the bar under-powered was judged against, stated so a reader never has to
	// guess which run used which bar.
	MinSamples int `json:"min_samples"`
	// Rows are the five published buckets, lowest first, including the empty ones.
	Rows []BucketRow `json:"rows"`
	// Scored is how many hypotheses the table rests on.
	Scored int `json:"scored"`
	// UnderPoweredBuckets is how many of the five hold fewer than MinSamples.
	UnderPoweredBuckets int `json:"under_powered_buckets"`
	// Brier is the mean squared error of the confidences against the outcomes, and LogLoss the
	// mean negative log likelihood. Both are nil when nothing was scored: zero is a perfect
	// Brier score and reporting one for an empty corpus would be a lie in the shape of a
	// number.
	Brier   *float64 `json:"brier"`
	LogLoss *float64 `json:"log_loss"`
	// Fixtures is how many fixtures contributed, and Trajectories how many recorded runs were
	// read.
	Fixtures     int `json:"fixtures"`
	Trajectories int `json:"trajectories"`
	// ByFixture is each fixture's own contribution, sorted by id. It exists so two runs can be
	// **paired on the fixtures they share** (SC-004, T110): the restriction has to happen before
	// the average, and an average cannot be un-averaged, so a report that published only the
	// aggregate could not be paired at all.
	ByFixture []ScoredFixture `json:"by_fixture,omitempty"`
	// Note explains an empty table, so that "0 hypotheses" is never read as "perfectly
	// calibrated".
	Note string `json:"note,omitempty"`
}

// Compute builds the reliability table from observations.
//
// Every published bucket gets a row, including the ones nothing landed in: a table that omitted
// its empty buckets would hide the fact that the corpus never produces a `very_low` confidence,
// which is itself a calibration finding.
func Compute(observations []Observation, minSamples int) *Reliability {
	if minSamples <= 0 {
		minSamples = MinBucketSamples
	}
	out := &Reliability{MinSamples: minSamples, Scored: len(observations)}

	byBucket := map[string][]Observation{}
	for _, obs := range observations {
		name := ledger.BucketFor(obs.Confidence).Name
		byBucket[name] = append(byBucket[name], obs)
	}
	for _, bucket := range ledger.Buckets() {
		rows := byBucket[bucket.Name]
		row := BucketRow{Bucket: bucket, Count: len(rows)}
		var sum float64
		for _, obs := range rows {
			sum += obs.Confidence
			if obs.Correct {
				row.Correct++
			}
		}
		if row.Count > 0 {
			row.ObservedAccuracy = float64(row.Correct) / float64(row.Count)
			row.MeanConfidence = sum / float64(row.Count)
		}
		row.UnderPowered = row.Count < minSamples
		if row.UnderPowered {
			out.UnderPoweredBuckets++
		}
		out.Rows = append(out.Rows, row)
	}

	if len(observations) == 0 {
		return out
	}
	var brier, logLoss float64
	for _, obs := range observations {
		outcome := 0.0
		if obs.Correct {
			outcome = 1.0
		}
		p := clampProbability(obs.Confidence)
		brier += (p - outcome) * (p - outcome)
		if obs.Correct {
			logLoss -= math.Log(p)
		} else {
			logLoss -= math.Log(1 - p)
		}
	}
	brier /= float64(len(observations))
	logLoss /= float64(len(observations))
	out.Brier = &brier
	out.LogLoss = &logLoss
	out.ByFixture = perFixture(observations)
	return out
}

// perFixture splits the observations by fixture and scores each one on its own, which is what
// makes a paired comparison possible.
func perFixture(observations []Observation) []ScoredFixture {
	byFixture := map[string][]Observation{}
	for _, obs := range observations {
		byFixture[obs.Fixture] = append(byFixture[obs.Fixture], obs)
	}
	names := make([]string, 0, len(byFixture))
	for name := range byFixture {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ScoredFixture, 0, len(names))
	for _, name := range names {
		rows := byFixture[name]
		row := ScoredFixture{Fixture: name, Scored: len(rows), Buckets: map[string]BucketCounts{}}
		var brier, logLoss float64
		for _, obs := range rows {
			outcome := 0.0
			if obs.Correct {
				outcome = 1.0
				row.Correct++
			}
			p := clampProbability(obs.Confidence)
			brier += (p - outcome) * (p - outcome)
			if obs.Correct {
				logLoss -= math.Log(p)
			} else {
				logLoss -= math.Log(1 - p)
			}
			name := ledger.BucketFor(obs.Confidence).Name
			counts := row.Buckets[name]
			counts.Count++
			counts.SumConfidence += obs.Confidence
			if obs.Correct {
				counts.Correct++
			}
			row.Buckets[name] = counts
		}
		if len(rows) > 0 {
			brier /= float64(len(rows))
			logLoss /= float64(len(rows))
			row.Brier, row.LogLoss = &brier, &logLoss
		}
		out = append(out, row)
	}
	return out
}

func clampProbability(p float64) float64 {
	switch {
	case p < logLossEpsilon:
		return logLossEpsilon
	case p > 1-logLossEpsilon:
		return 1 - logLossEpsilon
	default:
		return p
	}
}

// JSON renders the table canonically, so two runs over the same corpus produce the same bytes.
func (r *Reliability) JSON() ([]byte, error) { return graph.CanonicalJSON(r) }

// RenderMarkdown is the job-summary table. It is written for a reviewer scrolling a CI page, so
// every under-powered bucket says so in the accuracy column rather than printing a number a
// reader would take at face value.
func (r *Reliability) RenderMarkdown() string {
	var b strings.Builder
	b.WriteString("### Calibration — the reliability table over the five published buckets\n\n")
	fmt.Fprintf(&b, "Computed from %d recorded trajectory(ies) across %d fixture(s) with **zero model calls**. "+
		"A bucket holding fewer than %d hypotheses is reported as under-powered rather than scored (SC-004).\n\n",
		r.Trajectories, r.Fixtures, r.MinSamples)
	b.WriteString("| bucket | range | n | correct | observed accuracy | mean confidence |\n")
	b.WriteString("|---|---|---:|---:|---|---:|\n")
	for _, row := range r.Rows {
		accuracy := fmt.Sprintf("%.3f", row.ObservedAccuracy)
		if row.UnderPowered {
			accuracy = fmt.Sprintf("under-powered (n < %d)", r.MinSamples)
		}
		closing := ")"
		if row.Bucket.High >= 1 {
			closing = "]"
		}
		fmt.Fprintf(&b, "| %s | [%.2f, %.2f%s | %d | %d | %s | %.3f |\n",
			row.Bucket.Name, row.Bucket.Low, row.Bucket.High, closing,
			row.Count, row.Correct, accuracy, row.MeanConfidence)
	}
	b.WriteString("\n")
	if r.Brier == nil {
		fmt.Fprintf(&b, "**Brier: not scored. Log-loss: not scored.** %d hypotheses.\n", r.Scored)
	} else {
		fmt.Fprintf(&b, "**Brier %.6f · log-loss %.6f** over %d hypotheses.\n", *r.Brier, *r.LogLoss, r.Scored)
	}
	if r.Note != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Note)
	}
	return b.String()
}

// CollectCalibration walks the incident fixtures under root and gathers every scored hypothesis
// it can, with **no model call and no database**.
//
// Where the observations come from, and where they do not:
//
//   - a fixture's recorded `trajectories/` are read and validated, because a corpus whose
//     recordings do not parse must not silently score as "nothing to report". Each recording's
//     **stop record carries the belief state the run ended at** — hypothesis, posterior, published
//     bucket, status — and that is what its hypotheses are scored from;
//   - a **decision record** beside them — `investigation.json`, which `investigate export`
//     writes — carries the same thing for a run that was exported rather than recorded, and is
//     used where there is one.
//
// A recording made before the stop record carried a final ledger scores nothing, and says so,
// rather than being read as a run that believed nothing: a trajectory records the *judgments* a
// run applied, and re-deriving the posterior from them here would mean reimplementing the ledger
// inside the scorer.
//
// The two sources are not summed. A fixture with both would otherwise contribute its hypotheses
// twice and halve its own standard error for free, so the decision record wins where there is one
// — it is the exported answer, and the recordings beside it are the runs that produced it.
func CollectCalibration(root string, minSamples int) (*Reliability, error) {
	dirs, err := incidentDirs(root)
	if err != nil {
		return nil, err
	}
	var (
		observations []Observation
		trajectories int
		scoredFrom   int
	)
	for _, dir := range dirs {
		m, err := LoadManifest(dir)
		if err != nil {
			return nil, err
		}
		if !m.IsIncident() {
			continue
		}
		files, err := replay.Files(dir)
		if err != nil {
			return nil, err
		}
		var fromTrajectories []Observation
		for _, path := range files {
			traj, err := replay.Read(path)
			if err != nil {
				return nil, err
			}
			trajectories++
			fromTrajectories = append(fromTrajectories, observationsFromTrajectory(m, traj)...)
		}
		found, err := observationsFromDecisionRecord(m)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			found = fromTrajectories
		}
		if len(found) > 0 {
			scoredFrom++
			observations = append(observations, found...)
		}
	}

	out := Compute(observations, minSamples)
	out.Fixtures = len(dirs)
	out.Trajectories = trajectories
	if len(observations) == 0 {
		out.Note = "0 hypotheses scored. A hypothesis is scored from the belief state a run ended at: the " +
			"`final_ledger` on a recorded trajectory's stop record, or the decision record `investigate " +
			"export` writes as `investigation.json`. No fixture in this corpus carries either, so every " +
			"bucket is empty and every bucket is reported as under-powered. This line is the corpus saying " +
			"it cannot measure calibration yet, not a calibrated corpus."
	} else if out.UnderPoweredBuckets == len(out.Rows) {
		out.Note = fmt.Sprintf(
			"Every bucket is under-powered: %d hypotheses over %d recorded run(s) of %d fixture(s), against "+
				"a bar of %d per bucket (SC-004). The Brier score below is computed over what there is and is "+
				"reported so that a regression in it is visible, but no per-bucket accuracy in this table "+
				"means anything yet. The corpus reaches the bar by growing, not by lowering it.",
			out.Scored, out.Trajectories, scoredFrom, out.MinSamples)
	}
	return out, nil
}

// observationsFromTrajectory scores the belief state one recorded run ended at.
//
// The `no_observed_change` hypothesis is scored like any other, which is the point of FR-071b:
// where the truth is that no change explains the symptom, the run that says so confidently is the
// one that is right, and a scorer that skipped that row would be unable to tell a correct
// "nothing changed" from a failure to find anything.
func observationsFromTrajectory(m *Manifest, traj *replay.Trajectory) []Observation {
	final := traj.FinalLedger()
	if len(final) == 0 {
		return nil
	}
	truth := strings.TrimSpace(m.Incident.GroundTruth.Culprit)
	out := make([]Observation, 0, len(final))
	for _, h := range final {
		out = append(out, Observation{
			Fixture:      m.ID,
			RunID:        traj.RunID,
			HypothesisID: h.GetHypothesisId(),
			Statement:    h.GetStatement(),
			Confidence:   h.GetConfidence(),
			Correct:      finalHypothesisIsTrue(h, truth),
		})
	}
	return out
}

// finalHypothesisIsTrue grades one recorded final hypothesis against the fixture's ground truth,
// by the same rule `hypothesisIsTrue` applies to a decision record's.
func finalHypothesisIsTrue(h *investigationv1.FinalHypothesis, truth string) bool {
	return namesCulprit(h.GetKind(), truth,
		append([]string{h.GetCandidateChangeEntityId()}, h.GetCandidateChangeEntityRefs()...)...)
}

// observationsFromDecisionRecord reads `investigation.json` beside a fixture, when there is one,
// and scores each hypothesis against the fixture's own ground truth.
func observationsFromDecisionRecord(m *Manifest) ([]Observation, error) {
	path := filepath.Join(m.Dir, "investigation.json")
	raw, err := os.ReadFile(path) //nolint:gosec // inside the fixture directory the caller named
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fixture: %s: %w", path, err)
	}
	inv := &investigationv1.Investigation{}
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshal.Unmarshal(raw, inv); err != nil {
		return nil, fmt.Errorf("fixture: %s is not a decision record: %w", path, err)
	}
	truth := strings.TrimSpace(m.Incident.GroundTruth.Culprit)
	out := make([]Observation, 0, len(inv.GetLedger().GetHypotheses()))
	for _, h := range inv.GetLedger().GetHypotheses() {
		out = append(out, Observation{
			Fixture:      m.ID,
			RunID:        inv.GetInvestigationId(),
			HypothesisID: h.GetHypothesisId(),
			Statement:    h.GetStatement(),
			Confidence:   h.GetConfidence(),
			Correct:      hypothesisIsTrue(h, truth),
		})
	}
	return out, nil
}

// hypothesisIsTrue grades one hypothesis against the fixture's ground truth.
//
// A change hypothesis is true when it names the culprit change. Where the ground truth is
// `unobserved` or `not_change_induced`, every change hypothesis is false and the
// `no_observed_change` hypothesis is the true one (FR-071b, SC-023) — which is the whole reason
// the four unobservable-remainder fixtures exist.
func hypothesisIsTrue(h *investigationv1.Hypothesis, truth string) bool {
	return namesCulprit(h.GetKind(), truth, h.GetCandidateChangeEntityId())
}

// namesCulprit is the one grading rule, applied to both the decision record's hypotheses and a
// recorded run's final ledger.
//
// It accepts the canonical entity id **or** any reference the graph published for that change,
// because a ground truth is hand-written in the origin system's vocabulary
// (`k8s.change=shop/payments@rev7`) while the graph names the entity by an opaque id that
// identity resolution may have merged several refs onto. Matching on one spelling only would
// grade every correct answer wrong, and a calibration table whose every row reads "0 correct" is
// indistinguishable from an engine that never gets anything right.
func namesCulprit(kind investigationv1.HypothesisKind, truth string, spellings ...string) bool {
	if truth == CulpritUnobserved || strings.HasPrefix(truth, CulpritNotChangeInduced) {
		return kind == investigationv1.HypothesisKind_NO_OBSERVED_CHANGE
	}
	for _, spelling := range spellings {
		if spelling != "" && spelling == truth {
			return true
		}
	}
	return false
}

// incidentDirs lists the immediate subdirectories of root that hold a manifest, sorted. root
// itself is accepted when it is a fixture, so the command takes either a corpus or one fixture.
func incidentDirs(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, ManifestFile)); err == nil {
		return []string{root}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("fixture: read %s: %w", root, err)
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// WriteCalibration writes the machine-readable table and the markdown summary, either of which
// may be empty to skip it.
func WriteCalibration(r *Reliability, jsonPath, summaryPath string) error {
	if jsonPath != "" {
		body, err := r.JSON()
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonPath, append(body, '\n'), 0o600); err != nil {
			return fmt.Errorf("fixture: write %s: %w", jsonPath, err)
		}
	}
	if summaryPath != "" {
		if err := os.WriteFile(summaryPath, []byte(r.RenderMarkdown()), 0o600); err != nil {
			return fmt.Errorf("fixture: write %s: %w", summaryPath, err)
		}
	}
	return nil
}

// ---------- pairing against the previous version (T110; FR-023, SC-004, plan F9) ----------

// The paired comparison, and the one sentence that explains why it is not a subtraction of two
// numbers somebody printed last month.
//
// A Brier score is an average over whatever happened to be scored. Two runs over *different*
// corpora produce two Brier scores whose difference means nothing: add one easy fixture and the
// score improves, remove a hard one and it improves again, and neither movement is a change in
// the engine. SC-004 asks for the score "paired against the previous version **on the same
// fixtures**", and that is what `CompareCalibration` does — it intersects the two reports'
// fixtures, recomputes both aggregates over that intersection, and names the fixtures it dropped
// from each side so the reader can see what the pairing cost.
//
// This is why `Reliability` carries `ByFixture`. A report that published only the aggregate could
// not be paired at all: the restriction has to happen before the average, and an average cannot
// be un-averaged.

// CalibrationReport is the published name of the reliability table, so a caller pairing two of
// them reads `CompareCalibration(current, previous CalibrationReport)` rather than a name that
// says nothing about what it is for.
type CalibrationReport = Reliability

// ScoredFixture is one fixture's contribution to the table, kept so two runs can be paired on
// the fixtures they share.
type ScoredFixture struct {
	// Fixture is the incident id.
	Fixture string `json:"fixture"`
	// Scored and Correct are the hypotheses this fixture contributed.
	Scored  int `json:"scored"`
	Correct int `json:"correct"`
	// Brier and LogLoss are this fixture's own scores, nil when it scored nothing.
	Brier   *float64 `json:"brier"`
	LogLoss *float64 `json:"log_loss"`
	// Buckets is the per-bucket tally, keyed by the published bucket name, so a paired
	// comparison can restate the reliability table over the intersection rather than over
	// whatever each run happened to hold.
	Buckets map[string]BucketCounts `json:"buckets,omitempty"`
}

// BucketCounts is one bucket's tally within one fixture.
type BucketCounts struct {
	Count         int     `json:"count"`
	Correct       int     `json:"correct"`
	SumConfidence float64 `json:"sum_confidence"`
}

// CalibrationDelta is two reports paired on the fixtures they share.
type CalibrationDelta struct {
	// Paired are the fixtures present in both, sorted. OnlyInCurrent and OnlyInPrevious are the
	// ones dropped from each side, named rather than counted: "we paired on 7 of 9 fixtures" is
	// not something a reader can check, and "unknown-feeder-gap-01 is new" is.
	Paired         []string `json:"paired"`
	OnlyInCurrent  []string `json:"only_in_current,omitempty"`
	OnlyInPrevious []string `json:"only_in_previous,omitempty"`
	// Scored and PreviousScored are the hypotheses each side contributed over the intersection.
	Scored         int `json:"scored"`
	PreviousScored int `json:"previous_scored"`
	// The two scores over the intersection, and the movement. A negative delta is an
	// improvement, because both are error measures; the field names say which way round they
	// are and `Improved` says it in one boolean so nobody has to remember.
	Brier           *float64 `json:"brier"`
	PreviousBrier   *float64 `json:"previous_brier"`
	BrierDelta      *float64 `json:"brier_delta"`
	LogLoss         *float64 `json:"log_loss"`
	PreviousLogLoss *float64 `json:"previous_log_loss"`
	LogLossDelta    *float64 `json:"log_loss_delta"`
	Improved        *bool    `json:"improved"`
	// Buckets is the per-bucket movement over the intersection, lowest bucket first.
	Buckets []BucketDelta `json:"buckets"`
	// MinSamples is the bar under-powered was judged against on both sides.
	MinSamples int `json:"min_samples"`
	// Note explains a comparison that could not be made, so an empty delta is never read as "no
	// change".
	Note string `json:"note,omitempty"`
}

// BucketDelta is one bucket's movement between two runs, over the paired fixtures.
type BucketDelta struct {
	Bucket ledger.Bucket `json:"bucket"`
	// Count and PreviousCount are how many hypotheses landed in it on each side.
	Count         int `json:"count"`
	PreviousCount int `json:"previous_count"`
	// ObservedAccuracy and PreviousObservedAccuracy are Correct/Count on each side; both are
	// meaningless, and reported as such, where the bucket is under-powered.
	ObservedAccuracy         float64 `json:"observed_accuracy"`
	PreviousObservedAccuracy float64 `json:"previous_observed_accuracy"`
	// AccuracyDelta is nil where either side is under-powered: subtracting two numbers that do
	// not mean anything produces a third that does not either, and printing it is how a
	// calibration report becomes decoration.
	AccuracyDelta *float64 `json:"accuracy_delta"`
	// UnderPowered says the bucket is under-powered on that side, over the paired fixtures.
	UnderPowered         bool `json:"under_powered"`
	PreviousUnderPowered bool `json:"previous_under_powered"`
}

// CompareCalibration pairs two reliability tables on the fixtures they share.
//
// Nothing is compared outside that intersection. A fixture the corpus gained since the previous
// run has no previous score to be paired against, and one it lost has nothing to pair with; both
// are named in the result and neither contributes a number.
func CompareCalibration(current, previous *CalibrationReport) *CalibrationDelta {
	out := &CalibrationDelta{MinSamples: MinBucketSamples}
	if current == nil || previous == nil {
		out.Note = "one of the two reports is missing, so nothing was paired. A calibration delta needs " +
			"both sides; reporting zero movement from one would be a claim nobody made."
		return out
	}
	if current.MinSamples > 0 {
		out.MinSamples = current.MinSamples
	}

	mine, theirs := indexByFixture(current), indexByFixture(previous)
	for _, name := range sortedFixtureNames(mine) {
		if _, both := theirs[name]; both {
			out.Paired = append(out.Paired, name)
		} else {
			out.OnlyInCurrent = append(out.OnlyInCurrent, name)
		}
	}
	for _, name := range sortedFixtureNames(theirs) {
		if _, both := mine[name]; !both {
			out.OnlyInPrevious = append(out.OnlyInPrevious, name)
		}
	}
	if len(out.Paired) == 0 {
		out.Note = fmt.Sprintf(
			"the two runs share no fixture, so no paired comparison was made. The current run scored "+
				"%d fixture(s) and the previous %d; a Brier score compared across different corpora moves "+
				"when the corpus changes and says nothing about the engine (SC-004).",
			len(mine), len(theirs))
		out.Buckets = emptyBucketDeltas()
		return out
	}

	out.Scored, out.Brier, out.LogLoss = restrict(mine, out.Paired)
	out.PreviousScored, out.PreviousBrier, out.PreviousLogLoss = restrict(theirs, out.Paired)
	if out.Brier != nil && out.PreviousBrier != nil {
		delta := *out.Brier - *out.PreviousBrier
		out.BrierDelta = &delta
		improved := delta < 0
		out.Improved = &improved
	}
	if out.LogLoss != nil && out.PreviousLogLoss != nil {
		delta := *out.LogLoss - *out.PreviousLogLoss
		out.LogLossDelta = &delta
	}
	out.Buckets = bucketDeltas(mine, theirs, out.Paired, out.MinSamples)
	if out.Brier == nil || out.PreviousBrier == nil {
		out.Note = fmt.Sprintf(
			"the %d paired fixture(s) scored %d hypotheses now and %d before; a side that scored nothing "+
				"has no Brier score to be paired against, so the movement is reported as absent rather "+
				"than as zero.", len(out.Paired), out.Scored, out.PreviousScored)
	}
	return out
}

// restrict recomputes the two aggregate scores over a subset of the fixtures.
//
// The recomputation is a weighted mean of the per-fixture scores, weighted by how many hypotheses
// each contributed, which is exactly what the unrestricted average was — an average over
// hypotheses, not over fixtures. Weighting by fixture instead would let a fixture that scored two
// hypotheses count as much as one that scored two hundred.
func restrict(byFixture map[string]ScoredFixture, paired []string) (scored int, brier, logLoss *float64) {
	var brierSum, logLossSum float64
	var haveBrier, haveLogLoss bool
	for _, name := range paired {
		row := byFixture[name]
		scored += row.Scored
		if row.Brier != nil {
			brierSum += *row.Brier * float64(row.Scored)
			haveBrier = true
		}
		if row.LogLoss != nil {
			logLossSum += *row.LogLoss * float64(row.Scored)
			haveLogLoss = true
		}
	}
	if scored == 0 {
		return 0, nil, nil
	}
	if haveBrier {
		value := brierSum / float64(scored)
		brier = &value
	}
	if haveLogLoss {
		value := logLossSum / float64(scored)
		logLoss = &value
	}
	return scored, brier, logLoss
}

// bucketDeltas restates the reliability table over the paired fixtures, both sides.
func bucketDeltas(mine, theirs map[string]ScoredFixture, paired []string, minSamples int) []BucketDelta {
	out := make([]BucketDelta, 0, len(ledger.Buckets()))
	for _, bucket := range ledger.Buckets() {
		row := BucketDelta{Bucket: bucket}
		mineCount, mineCorrect := tally(mine, paired, bucket.Name)
		theirsCount, theirsCorrect := tally(theirs, paired, bucket.Name)
		row.Count, row.PreviousCount = mineCount, theirsCount
		if mineCount > 0 {
			row.ObservedAccuracy = float64(mineCorrect) / float64(mineCount)
		}
		if theirsCount > 0 {
			row.PreviousObservedAccuracy = float64(theirsCorrect) / float64(theirsCount)
		}
		row.UnderPowered = mineCount < minSamples
		row.PreviousUnderPowered = theirsCount < minSamples
		if !row.UnderPowered && !row.PreviousUnderPowered {
			delta := row.ObservedAccuracy - row.PreviousObservedAccuracy
			row.AccuracyDelta = &delta
		}
		out = append(out, row)
	}
	return out
}

// tally adds one bucket's counts across the paired fixtures.
func tally(byFixture map[string]ScoredFixture, paired []string, bucket string) (count, correct int) {
	for _, name := range paired {
		counts := byFixture[name].Buckets[bucket]
		count += counts.Count
		correct += counts.Correct
	}
	return count, correct
}

func emptyBucketDeltas() []BucketDelta {
	out := make([]BucketDelta, 0, len(ledger.Buckets()))
	for _, bucket := range ledger.Buckets() {
		out = append(out, BucketDelta{Bucket: bucket, UnderPowered: true, PreviousUnderPowered: true})
	}
	return out
}

func indexByFixture(report *CalibrationReport) map[string]ScoredFixture {
	out := make(map[string]ScoredFixture, len(report.ByFixture))
	for _, row := range report.ByFixture {
		out[row.Fixture] = row
	}
	return out
}

func sortedFixtureNames(byFixture map[string]ScoredFixture) []string {
	names := make([]string, 0, len(byFixture))
	for name := range byFixture {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// JSON renders the delta canonically.
func (d *CalibrationDelta) JSON() ([]byte, error) { return graph.CanonicalJSON(d) }

// RenderMarkdown is the paired comparison as a job summary shows it.
func (d *CalibrationDelta) RenderMarkdown() string {
	var b strings.Builder
	b.WriteString("### Calibration — paired against the previous version on the same fixtures\n\n")
	if len(d.Paired) == 0 {
		fmt.Fprintf(&b, "%s\n", d.Note)
		return b.String()
	}
	fmt.Fprintf(&b, "Paired on %d fixture(s): %s.\n\n", len(d.Paired), strings.Join(d.Paired, ", "))
	if len(d.OnlyInCurrent) > 0 {
		fmt.Fprintf(&b, "- new in this run, not paired: %s\n", strings.Join(d.OnlyInCurrent, ", "))
	}
	if len(d.OnlyInPrevious) > 0 {
		fmt.Fprintf(&b, "- only in the previous run, not paired: %s\n", strings.Join(d.OnlyInPrevious, ", "))
	}
	if len(d.OnlyInCurrent) > 0 || len(d.OnlyInPrevious) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "| score | previous | now | movement |\n|---|---:|---:|---|\n")
	fmt.Fprintf(&b, "| Brier | %s | %s | %s |\n",
		formatScore(d.PreviousBrier), formatScore(d.Brier), formatMovement(d.BrierDelta))
	fmt.Fprintf(&b, "| log-loss | %s | %s | %s |\n",
		formatScore(d.PreviousLogLoss), formatScore(d.LogLoss), formatMovement(d.LogLossDelta))
	fmt.Fprintf(&b, "\nOver %d hypotheses now and %d before. Both are error measures: a negative "+
		"movement is an improvement.\n\n", d.Scored, d.PreviousScored)

	b.WriteString("| bucket | n before | n now | accuracy before | accuracy now | movement |\n")
	b.WriteString("|---|---:|---:|---|---|---|\n")
	for _, row := range d.Buckets {
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %s |\n",
			row.Bucket.Name, row.PreviousCount, row.Count,
			formatAccuracy(row.PreviousObservedAccuracy, row.PreviousUnderPowered, d.MinSamples),
			formatAccuracy(row.ObservedAccuracy, row.UnderPowered, d.MinSamples),
			formatMovement(row.AccuracyDelta))
	}
	if d.Note != "" {
		fmt.Fprintf(&b, "\n%s\n", d.Note)
	}
	return b.String()
}

func formatScore(value *float64) string {
	if value == nil {
		return "not scored"
	}
	return fmt.Sprintf("%.6f", *value)
}

func formatMovement(value *float64) string {
	if value == nil {
		return "not comparable"
	}
	return fmt.Sprintf("%+.6f", *value)
}

func formatAccuracy(value float64, underPowered bool, minSamples int) string {
	if underPowered {
		return fmt.Sprintf("under-powered (n < %d)", minSamples)
	}
	return fmt.Sprintf("%.3f", value)
}

// ReadCalibration reads a machine-readable table written by WriteCalibration, so `--previous`
// takes the file the last run published rather than a number somebody typed.
func ReadCalibration(path string) (*CalibrationReport, error) {
	body, err := os.ReadFile(path) //nolint:gosec // a report path the caller named
	if err != nil {
		return nil, fmt.Errorf("fixture: read %s: %w", path, err)
	}
	report := &CalibrationReport{}
	if err := json.Unmarshal(body, report); err != nil {
		return nil, fmt.Errorf("fixture: %s is not a calibration report: %w", path, err)
	}
	return report, nil
}
