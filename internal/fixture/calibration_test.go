// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// The calibration arithmetic (T096, T098; reused by T110). No database, no model, no network.

// TestEveryPublishedBucketGetsARowEvenWhenItIsEmpty: a table that omitted its empty buckets would
// hide the finding that the corpus never produces a confidence in that range.
func TestEveryPublishedBucketGetsARowEvenWhenItIsEmpty(t *testing.T) {
	t.Parallel()

	table := fixture.Compute(nil, fixture.MinBucketSamples)
	if len(table.Rows) != 5 {
		t.Fatalf("the table has %d rows, want the five published buckets", len(table.Rows))
	}
	for _, row := range table.Rows {
		if !row.UnderPowered {
			t.Errorf("bucket %s holds nothing and is not reported as under-powered", row.Bucket.Name)
		}
	}
	if table.Brier != nil || table.LogLoss != nil {
		t.Error("an empty corpus was given a Brier score; zero is a perfect score and reporting one " +
			"for nothing scored is a lie in the shape of a number")
	}
}

// TestABucketBelowTheBarIsReportedRatherThanScored (SC-004).
func TestABucketBelowTheBarIsReportedRatherThanScored(t *testing.T) {
	t.Parallel()

	observations := []fixture.Observation{
		{Confidence: 0.90, Correct: true},
		{Confidence: 0.95, Correct: false},
		{Confidence: 0.88, Correct: true},
	}
	table := fixture.Compute(observations, 20)
	row := bucket(t, table, "very_high")
	if row.Count != 3 {
		t.Fatalf("the very_high bucket holds %d, want 3", row.Count)
	}
	if !row.UnderPowered {
		t.Error("three hypotheses were scored as though the bucket were powered")
	}
	markdown := table.RenderMarkdown()
	if !strings.Contains(markdown, "under-powered (n < 20)") {
		t.Errorf("the markdown scores an under-powered bucket:\n%s", markdown)
	}

	// The same three, with the bar lowered, are scored — and the observed accuracy is the
	// fraction, not the mean confidence.
	powered := fixture.Compute(observations, 2)
	row = bucket(t, powered, "very_high")
	if row.UnderPowered {
		t.Error("the bucket is still reported as under-powered with the bar at 2")
	}
	if math.Abs(row.ObservedAccuracy-2.0/3.0) > 1e-9 {
		t.Errorf("observed accuracy %v, want 2/3", row.ObservedAccuracy)
	}
}

// TestBrierAndLogLossAreTheTextbookScores, on numbers small enough to check by hand.
func TestBrierAndLogLossAreTheTextbookScores(t *testing.T) {
	t.Parallel()

	table := fixture.Compute([]fixture.Observation{
		{Confidence: 1.0, Correct: true},
		{Confidence: 0.0, Correct: false},
	}, 1)
	if table.Brier == nil || *table.Brier > 1e-12 {
		t.Errorf("Brier = %v, want 0 for two perfectly confident correct calls", table.Brier)
	}
	if table.LogLoss == nil || *table.LogLoss > 1e-12 {
		t.Errorf("log-loss = %v, want ~0", table.LogLoss)
	}

	// A confident mistake is finite rather than infinite, because of the published clamp.
	wrong := fixture.Compute([]fixture.Observation{{Confidence: 1.0, Correct: false}}, 1)
	if wrong.LogLoss == nil || math.IsInf(*wrong.LogLoss, 0) {
		t.Errorf("log-loss = %v on a confidently wrong call; the clamp is there so one mistake does "+
			"not make the score meaningless", wrong.LogLoss)
	}
	if math.Abs(*wrong.Brier-1.0) > 1e-9 {
		t.Errorf("Brier = %v on a confidently wrong call, want 1", *wrong.Brier)
	}
}

// TestHalfRightIsHalfScored: the observed accuracy of a bucket is the fraction correct, and the
// mean confidence beside it is what it is being compared against.
func TestHalfRightIsHalfScored(t *testing.T) {
	t.Parallel()

	table := fixture.Compute([]fixture.Observation{
		{Confidence: 0.5, Correct: true},
		{Confidence: 0.5, Correct: false},
	}, 1)
	row := bucket(t, table, "moderate")
	if row.ObservedAccuracy != 0.5 || row.MeanConfidence != 0.5 {
		t.Errorf("accuracy %v, mean confidence %v; want 0.5 and 0.5", row.ObservedAccuracy, row.MeanConfidence)
	}
	if math.Abs(*table.Brier-0.25) > 1e-9 {
		t.Errorf("Brier = %v, want 0.25", *table.Brier)
	}
}

// TestCollectingOverTheCorpusOpensNothing: the collector reads the corpus and reports what it can
// measure, saying so when it can measure nothing rather than printing an empty table as a
// calibrated one.
func TestCollectingOverTheCorpusOpensNothing(t *testing.T) {
	t.Parallel()

	table, err := fixture.CollectCalibration("../../fixtures/incidents", fixture.MinBucketSamples)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if table.Fixtures == 0 {
		t.Error("the collector found no incident fixtures")
	}
	if table.Scored == 0 && table.Note == "" {
		t.Error("nothing was scored and the report does not say why; \"0 hypotheses\" must never read " +
			"as \"perfectly calibrated\"")
	}
	markdown := table.RenderMarkdown()
	for _, want := range []string{"reliability table", "zero model calls", "very_high"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, markdown)
		}
	}
}

// TestTheTableRendersCanonically: two runs over the same corpus produce the same bytes, so a CI
// diff means something changed.
func TestTheTableRendersCanonically(t *testing.T) {
	t.Parallel()

	observations := []fixture.Observation{{Fixture: "a", Confidence: 0.7, Correct: true}}
	first, err := fixture.Compute(observations, 1).JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	second, err := fixture.Compute(observations, 1).JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if string(first) != string(second) {
		t.Error("two renderings of the same table differ")
	}
}

func bucket(t *testing.T, table *fixture.Reliability, name string) fixture.BucketRow {
	t.Helper()
	for _, row := range table.Rows {
		if row.Bucket.Name == name {
			return row
		}
	}
	t.Fatalf("no %s row", name)
	return fixture.BucketRow{}
}

// ---------- the paired comparison (T110, SC-004) ----------

// observationsFor builds one fixture's worth of scored hypotheses at a stated confidence.
func observationsFor(name string, n int, confidence float64, correct int) []fixture.Observation {
	out := make([]fixture.Observation, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fixture.Observation{
			Fixture:      name,
			HypothesisID: fmt.Sprintf("h-%d", i+1),
			Confidence:   confidence,
			Correct:      i < correct,
		})
	}
	return out
}

// TestCalibrationIsPairedOnTheFixturesTheTwoRunsShare is the property SC-004 asks for and the one
// a naive subtraction of two published Brier scores does not have.
//
// The previous run scored two fixtures. The current run scored one of those two, plus a new one
// that is *perfectly* calibrated. An unpaired comparison would report a large improvement, and
// all of it would be the new fixture. The paired comparison must see through that: it compares
// only the shared fixture, whose score did not move.
func TestCalibrationIsPairedOnTheFixturesTheTwoRunsShare(t *testing.T) {
	shared := observationsFor("rollout-regression-01-incident", 10, 0.9, 5)

	previous := fixture.Compute(append(append([]fixture.Observation(nil), shared...),
		observationsFor("slow-burn-01", 10, 0.9, 5)...), 20)
	current := fixture.Compute(append(append([]fixture.Observation(nil), shared...),
		observationsFor("a-brand-new-fixture", 200, 0.99, 198)...), 20)

	delta := fixture.CompareCalibration(current, previous)

	if len(delta.Paired) != 1 || delta.Paired[0] != "rollout-regression-01-incident" {
		t.Fatalf("paired on %v, want only the shared fixture", delta.Paired)
	}
	if len(delta.OnlyInCurrent) != 1 || delta.OnlyInCurrent[0] != "a-brand-new-fixture" {
		t.Errorf("only_in_current = %v", delta.OnlyInCurrent)
	}
	if len(delta.OnlyInPrevious) != 1 || delta.OnlyInPrevious[0] != "slow-burn-01" {
		t.Errorf("only_in_previous = %v", delta.OnlyInPrevious)
	}
	if delta.BrierDelta == nil {
		t.Fatal("no Brier movement was computed over the paired fixture")
	}
	if *delta.BrierDelta != 0 {
		t.Errorf("Brier moved by %v over a fixture whose score did not change; the new fixture leaked "+
			"into the comparison", *delta.BrierDelta)
	}
	if delta.Scored != 10 || delta.PreviousScored != 10 {
		t.Errorf("scored %d now and %d before, want 10 and 10 over the one shared fixture",
			delta.Scored, delta.PreviousScored)
	}
	// And the unpaired numbers really are different, or the test above would prove nothing.
	if *current.Brier == *previous.Brier {
		t.Error("the two unpaired Brier scores are equal, so the pairing was not tested")
	}
}

// TestAPairedComparisonReportsMovementAndItsDirection.
func TestAPairedComparisonReportsMovementAndItsDirection(t *testing.T) {
	previous := fixture.Compute(observationsFor("f", 40, 0.9, 20), 20) // badly calibrated
	current := fixture.Compute(observationsFor("f", 40, 0.9, 36), 20)  // 0.9 claimed, 0.9 observed
	delta := fixture.CompareCalibration(current, previous)

	if delta.BrierDelta == nil || *delta.BrierDelta >= 0 {
		t.Errorf("Brier delta %v; a run that got better must report a negative movement", delta.BrierDelta)
	}
	if delta.Improved == nil || !*delta.Improved {
		t.Error("the improvement is not reported")
	}
	if delta.LogLossDelta == nil || *delta.LogLossDelta >= 0 {
		t.Errorf("log-loss delta %v", delta.LogLossDelta)
	}

	var moved bool
	for _, bucket := range delta.Buckets {
		if bucket.Bucket.Name != "very_high" {
			continue
		}
		if bucket.Count != 40 || bucket.PreviousCount != 40 {
			t.Errorf("very_high holds %d now and %d before", bucket.Count, bucket.PreviousCount)
		}
		if bucket.UnderPowered || bucket.PreviousUnderPowered {
			t.Error("a bucket holding 40 hypotheses is reported under-powered against a bar of 20")
		}
		if bucket.AccuracyDelta == nil || *bucket.AccuracyDelta <= 0 {
			t.Errorf("very_high accuracy delta %v, want an improvement", bucket.AccuracyDelta)
		}
		moved = true
	}
	if !moved {
		t.Error("the very_high bucket is not in the table")
	}
	if !strings.Contains(delta.RenderMarkdown(), "paired") {
		t.Error("the markdown does not say what it was paired on")
	}
}

// TestAnUnderPoweredBucketHasNoMovement: subtracting two numbers that do not mean anything
// produces a third that does not either, and printing it is how a calibration report becomes
// decoration.
func TestAnUnderPoweredBucketHasNoMovement(t *testing.T) {
	previous := fixture.Compute(observationsFor("f", 3, 0.9, 1), 20)
	current := fixture.Compute(observationsFor("f", 3, 0.9, 3), 20)
	delta := fixture.CompareCalibration(current, previous)
	for _, bucket := range delta.Buckets {
		if bucket.AccuracyDelta != nil {
			t.Errorf("bucket %s reports a movement of %v over %d and %d hypotheses",
				bucket.Bucket.Name, *bucket.AccuracyDelta, bucket.Count, bucket.PreviousCount)
		}
	}
	// The aggregate scores are still compared, because a regression in them is visible even where
	// no per-bucket accuracy means anything.
	if delta.BrierDelta == nil {
		t.Error("no Brier movement over a small but non-empty intersection")
	}
}

// TestTwoRunsThatShareNoFixtureAreNotCompared: reporting zero movement between two disjoint
// corpora is the one number a reader must never see.
func TestTwoRunsThatShareNoFixtureAreNotCompared(t *testing.T) {
	delta := fixture.CompareCalibration(
		fixture.Compute(observationsFor("a", 10, 0.9, 9), 20),
		fixture.Compute(observationsFor("b", 10, 0.9, 1), 20))
	if delta.BrierDelta != nil {
		t.Errorf("a movement of %v was reported between two disjoint corpora", *delta.BrierDelta)
	}
	if delta.Note == "" {
		t.Error("nothing says why the comparison was not made")
	}
	if !strings.Contains(delta.RenderMarkdown(), "share no fixture") {
		t.Errorf("the markdown does not explain itself:\n%s", delta.RenderMarkdown())
	}
}

// TestAMissingPreviousReportIsNotZeroMovement.
func TestAMissingPreviousReportIsNotZeroMovement(t *testing.T) {
	delta := fixture.CompareCalibration(fixture.Compute(observationsFor("a", 10, 0.9, 9), 20), nil)
	if delta.BrierDelta != nil || delta.Note == "" {
		t.Errorf("a delta against nothing reported %+v", delta)
	}
}

// TestAReportRoundTripsThroughItsOwnFile: `--previous` reads the file the last run published, so
// the file has to carry everything the pairing needs — the per-fixture split above all.
func TestAReportRoundTripsThroughItsOwnFile(t *testing.T) {
	report := fixture.Compute(append(observationsFor("a", 10, 0.9, 9), observationsFor("b", 10, 0.5, 5)...), 20)
	path := filepath.Join(t.TempDir(), "calibration.json")
	if err := fixture.WriteCalibration(report, path, ""); err != nil {
		t.Fatalf("WriteCalibration: %v", err)
	}
	back, err := fixture.ReadCalibration(path)
	if err != nil {
		t.Fatalf("ReadCalibration: %v", err)
	}
	if len(back.ByFixture) != 2 {
		t.Fatalf("the written report carries %d per-fixture rows; without them nothing can be paired",
			len(back.ByFixture))
	}
	delta := fixture.CompareCalibration(report, back)
	if len(delta.Paired) != 2 || delta.BrierDelta == nil || *delta.BrierDelta != 0 {
		t.Errorf("a report paired against itself reported %+v", delta)
	}
}
