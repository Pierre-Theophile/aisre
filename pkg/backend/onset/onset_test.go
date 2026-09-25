// SPDX-License-Identifier: Apache-2.0

package onset

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"
)

// Tests for the onset estimator (tasks.md T045 and T046).
//
// T046 is the important one and it is the last test in this file: the same recorded series must
// produce the same instant and the same uncertainty to six decimals on arm64 and amd64, which
// CI enforces by running this package on both runners. The test pins exact digits rather than
// comparing against a tolerance, because a tolerance is exactly the thing that would hide the
// fused-multiply-add divergence this discipline exists to prevent.

var origin = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

// stepSeries is a flat series that steps up at stepAt, with a small deterministic wobble so the
// robust scale is not degenerate. It is the shape of a rollout regression: nothing, then an
// error rate that rises and stays risen.
func stepSeries(n, stepAt int, low, high float64) []Point {
	series := make([]Point, 0, n)
	for i := range n {
		value := low
		if i >= stepAt {
			value = high
		}
		value += float64((i*37)%7) * 0.001
		series = append(series, Point{At: origin.Add(time.Duration(i) * time.Minute), Value: value})
	}
	return series
}

func searchWindow(n int) Window {
	return Window{Start: origin, End: origin.Add(time.Duration(n) * time.Minute)}
}

func TestEstimateOnset(t *testing.T) {
	t.Parallel()

	flat := make([]Point, 0, 60)
	for i := range 60 {
		flat = append(flat, Point{At: origin.Add(time.Duration(i) * time.Minute), Value: 0.02})
	}
	noisyNoShift := make([]Point, 0, 60)
	for i := range 60 {
		noisyNoShift = append(noisyNoShift, Point{
			At:    origin.Add(time.Duration(i) * time.Minute),
			Value: 0.02 + float64((i*13)%5)*0.001,
		})
	}

	tests := []struct {
		name            string
		series          []Point
		search          Window
		wantUnavailable bool
		wantReason      string
		wantAt          time.Time
		wantDirection   string
	}{
		{
			name:          "step up is found at the first point of the new regime",
			series:        stepSeries(120, 81, 0.01, 0.24),
			search:        searchWindow(120),
			wantAt:        origin.Add(81 * time.Minute),
			wantDirection: "up",
		},
		{
			name:          "step down is found and reported as down",
			series:        stepSeries(120, 81, 0.40, 0.02),
			search:        searchWindow(120),
			wantAt:        origin.Add(81 * time.Minute),
			wantDirection: "down",
		},
		{
			name:          "an early step is found early",
			series:        stepSeries(120, 20, 0.01, 0.24),
			search:        searchWindow(120),
			wantAt:        origin.Add(20 * time.Minute),
			wantDirection: "up",
		},
		{
			name:            "a flat series has nothing to depart from",
			series:          flat,
			search:          searchWindow(60),
			wantUnavailable: true,
			wantReason:      ReasonFlat,
		},
		{
			name:            "noise without a sustained shift never crosses",
			series:          noisyNoShift,
			search:          searchWindow(60),
			wantUnavailable: true,
			wantReason:      ReasonNoCrossing,
		},
		{
			name:            "too few points is a refusal, not a guess",
			series:          stepSeries(5, 3, 0.01, 0.5),
			search:          searchWindow(5),
			wantUnavailable: true,
			wantReason:      ReasonTooSparse,
		},
		{
			name:            "an empty series is absent, not flat",
			series:          nil,
			search:          searchWindow(60),
			wantUnavailable: true,
			wantReason:      ReasonAbsent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EstimateOnset(tc.series, tc.search, Params{})

			if got.Unavailable != tc.wantUnavailable {
				t.Fatalf("unavailable = %v (%s), want %v", got.Unavailable, got.Reason, tc.wantUnavailable)
			}
			if tc.wantUnavailable {
				if got.Reason != tc.wantReason {
					t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
				}
				if !got.At.IsZero() {
					t.Errorf("an unavailable estimate carries an instant %s; the engine must fall back to the alert instant and exonerate nobody on timing alone", got.At)
				}
				return
			}
			if !got.At.Equal(tc.wantAt) {
				t.Errorf("onset = %s, want %s", got.At.Format(time.RFC3339), tc.wantAt.Format(time.RFC3339))
			}
			if got.Direction != tc.wantDirection {
				t.Errorf("direction = %q, want %q", got.Direction, tc.wantDirection)
			}
			if got.UncertaintySeconds <= 0 {
				t.Errorf("uncertainty = %d; an estimate with no uncertainty is a claim, not evidence", got.UncertaintySeconds)
			}
			if got.Method != MethodName {
				t.Errorf("method = %q, want %q", got.Method, MethodName)
			}
		})
	}
}

// TestOrderIndependence: the estimate is a function of the series, not of the vendor's
// pagination. A backend that returns points out of order must get the same answer.
func TestOrderIndependence(t *testing.T) {
	t.Parallel()

	ordered := stepSeries(120, 81, 0.01, 0.24)
	shuffled := make([]Point, len(ordered))
	for i, pt := range ordered {
		shuffled[(i*47)%len(ordered)] = pt
	}

	want := EstimateOnset(ordered, searchWindow(120), Params{})
	got := EstimateOnset(shuffled, searchWindow(120), Params{})

	if !got.At.Equal(want.At) || got.UncertaintySeconds != want.UncertaintySeconds || got.Statistic != want.Statistic {
		t.Fatalf("a reordered series produced a different estimate:\n ordered: %s ±%ds stat %.6f\nshuffled: %s ±%ds stat %.6f",
			want.At.Format(time.RFC3339), want.UncertaintySeconds, want.Statistic,
			got.At.Format(time.RFC3339), got.UncertaintySeconds, got.Statistic)
	}
}

// TestBaselineFallbackWidens: a window with no seasonal history falls back to a flat baseline,
// says so, and widens the uncertainty rather than suppressing the estimate.
func TestBaselineFallbackWidens(t *testing.T) {
	t.Parallel()

	got := EstimateOnset(stepSeries(120, 81, 0.01, 0.24), searchWindow(120), Params{})
	if !got.BaselineFallback {
		t.Fatalf("a 2-hour window has no previous day in it; the baseline must fall back and say so")
	}

	// The same shift with a period short enough that the window does hold history: the
	// fallback is off and the uncertainty is not doubled.
	withHistory := EstimateOnset(stepSeries(120, 81, 0.01, 0.24), searchWindow(120),
		Params{SeasonalPeriodSeconds: 60})
	if withHistory.BaselineFallback {
		t.Errorf("a window holding %d seasonal periods still reported a fallback", SeasonalPeriods)
	}
	if withHistory.UncertaintySeconds > got.UncertaintySeconds {
		t.Errorf("the fallback estimate (±%ds) is not wider than the seasonal one (±%ds); a baseline that cannot tell a daily shape from a shift must widen the interval",
			got.UncertaintySeconds, withHistory.UncertaintySeconds)
	}
}

// TestRound6AtEveryStep documents the discipline rather than the result: every helper that
// accumulates rounds, so a value that reaches a comparison never carries more than six decimals.
func TestRound6AtEveryStep(t *testing.T) {
	t.Parallel()

	values := []float64{1.0 / 3.0, 2.0 / 3.0, math.Pi, -math.Pi}
	for _, v := range values {
		rounded := round6(v)
		if scaled := rounded * 1e6; math.Abs(scaled-math.Round(scaled)) > 1e-6 {
			t.Errorf("round6(%v) = %v, which is not a whole number of millionths", v, rounded)
		}
	}
	if got := round6(math.NaN()); got != 0 {
		t.Errorf("round6(NaN) = %v; a NaN reaching a comparison is a divergence waiting to happen", got)
	}
	if got := round6(math.Inf(1)); got != 0 {
		t.Errorf("round6(+Inf) = %v", got)
	}
}

// TestDeterminismFixedSeries is SC-019: a fixed series produces exact digits, and they are the
// same digits on arm64 and amd64. CI runs this package on both runners (ci.yml), so a change
// that reintroduces an unrounded accumulation fails here on one architecture and not the other
// — which is precisely the failure this test exists to catch.
//
// The expected values below were produced by this implementation and are pinned deliberately.
// If the arithmetic changes, this test must be updated **and** the estimator's Version bumped,
// because every recorded onset answer in every world becomes stale at the same moment.
func TestDeterminismFixedSeries(t *testing.T) {
	t.Parallel()

	// A fixed, hand-written series: 40 points a minute apart, flat at 0.010 with a repeating
	// three-value wobble, stepping to 0.240 at index 25 (13:25:00Z).
	values := []float64{
		0.010, 0.012, 0.011, 0.010, 0.012, 0.011, 0.010, 0.012, 0.011, 0.010,
		0.012, 0.011, 0.010, 0.012, 0.011, 0.010, 0.012, 0.011, 0.010, 0.012,
		0.011, 0.010, 0.012, 0.011, 0.010, 0.240, 0.242, 0.241, 0.240, 0.242,
		0.241, 0.240, 0.242, 0.241, 0.240, 0.242, 0.241, 0.240, 0.242, 0.241,
	}
	series := make([]Point, 0, len(values))
	for i, v := range values {
		series = append(series, Point{At: origin.Add(time.Duration(i) * time.Minute), Value: v})
	}

	const (
		wantAt          = "2026-09-01T13:25:00Z"
		wantUncertainty = int64(120)
		wantStatistic   = "0.187386"
		wantDirection   = "up"
	)

	// Run it many times: an estimator whose answer depends on map iteration order — the
	// seasonal baseline is built from a map — would be caught here rather than in a fixture
	// months later.
	for range 64 {
		got := EstimateOnset(series, Window{Start: origin, End: origin.Add(40 * time.Minute)}, Params{})
		if got.Unavailable {
			t.Fatalf("the pinned series produced no estimate: %s", got.Reason)
		}
		if at := got.At.UTC().Format(time.RFC3339); at != wantAt {
			t.Fatalf("onset = %s, want %s", at, wantAt)
		}
		if got.UncertaintySeconds != wantUncertainty {
			t.Fatalf("uncertainty = %d, want %d", got.UncertaintySeconds, wantUncertainty)
		}
		if stat := fmt.Sprintf("%.6f", got.Statistic); stat != wantStatistic {
			t.Fatalf("statistic = %s, want %s (six decimals, identical on every architecture)", stat, wantStatistic)
		}
		if got.Direction != wantDirection {
			t.Fatalf("direction = %q, want %q", got.Direction, wantDirection)
		}
	}
}

// ---- Phase 8 Track J: false positives on flat series -----------------------------------------

// wobbledFlat reproduces, bit for bit, the series `pkg/backend/synthetic` generates for a service
// nothing has happened to: a per-entity baseline multiplied by a seeded per-instant wobble of
// ±4 %. The arithmetic is duplicated here rather than imported because synthetic imports this
// package, and a test that cannot be written without a cycle is a test that would not be written.
func wobbledFlat(seed, selector string, start time.Time, points int) []Point {
	unit := func(parts ...string) float64 {
		h := sha256.New()
		for _, part := range parts {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
		sum := h.Sum(nil)
		n := binary.BigEndian.Uint32(sum[:4])
		return round6(float64(n%1_000_000) / 1_000_000.0)
	}
	base := round6(0.004 + 0.008*unit(seed, selector, "error"))
	out := make([]Point, 0, points)
	for i := range points {
		at := start.Add(time.Duration(i) * time.Minute)
		wobble := round6(unit(seed, selector, "wobble", at.UTC().Format(time.RFC3339))*0.08 - 0.04)
		out = append(out, Point{At: at, Value: round6(base * round6(1+wobble))})
	}
	return out
}

// TestFlatSeriesWithWobbleHasNoOnset is the `adjacent-decoy-01` regression, reproduced exactly.
//
// The seed, the selector and the window are the ones that fixture's recorded world holds. Over
// them the CUSUM does cross — noise accumulates past h given ninety points to do it in — and the
// pre-Track-J estimator published 13:27 ± 840 s on a series in which nothing whatsoever happened.
// Forty minutes before the true onset at 14:07, with an uncertainty that does not reach it.
//
// The criterion is what refuses it: the level after the crossing is 1.30 scaled MADs from the
// level before it, and three is the published floor. The answer is now `no_onset_detected` with
// confidence 0, which the engine already treats the way it treats NO_DATA — it falls back to the
// alert instant, says so, and exonerates nobody on timing alone.
func TestFlatSeriesWithWobbleHasNoOnset(t *testing.T) {
	t.Parallel()

	const (
		seed     = "adjacent-decoy-01"
		selector = `service.name="checkout" AND service.namespace="shop" AND metric.name="http.server.request.duration"`
	)
	start := time.Date(2026, 9, 1, 13, 2, 0, 0, time.UTC)
	series := wobbledFlat(seed, selector, start, 90)
	search := Window{Start: start, End: start.Add(90 * time.Minute)}

	got := EstimateOnset(series, search, Params{})
	if !got.Unavailable {
		t.Fatalf("a flat series with seeded wobble produced an onset at %s ±%ds (effect %.6f MADs); "+
			"a confident instant on a series in which nothing happened keys every causal-ordering decision after it to nothing",
			got.At.Format(time.RFC3339), got.UncertaintySeconds, got.EffectSizeMADs)
	}
	if got.Reason != ReasonNoOnsetDetected {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonNoOnsetDetected)
	}
	if got.Confidence != 0 {
		t.Errorf("confidence = %v on an unavailable estimate, want 0", got.Confidence)
	}
	if !got.At.IsZero() {
		t.Errorf("an unavailable estimate carries the instant %s", got.At)
	}

	// The crossing itself is still there — this is the criterion refusing it, not the CUSUM
	// failing to fire. Relaxing the criterion to nothing reproduces the old answer exactly, which
	// is what makes this a fix rather than a coincidence.
	relaxed := EstimateOnset(series, search, Params{MinEffectSizeMADs: 1e-7, MinSustainedPoints: 1})
	if relaxed.Unavailable {
		t.Fatalf("with the criterion relaxed the series produced no estimate (%s); this test is then not pinning what it claims to", relaxed.Reason)
	}
	if at := relaxed.At.UTC().Format(time.RFC3339); at != "2026-09-01T13:27:00Z" {
		t.Errorf("the pre-Track-J answer was %s, want 2026-09-01T13:27:00Z", at)
	}
	if relaxed.EffectSizeMADs > MinEffectSizeMADs {
		t.Errorf("the refused crossing measured %.6f MADs, which is at or above the published floor of %v", relaxed.EffectSizeMADs, float64(MinEffectSizeMADs))
	}
}

// TestStepSurvivesTheCriterion: the shape the method is specified against is untouched by it, and
// carries full confidence.
func TestStepSurvivesTheCriterion(t *testing.T) {
	t.Parallel()

	got := EstimateOnset(stepSeries(120, 81, 0.01, 0.24), searchWindow(120), Params{})
	if got.Unavailable {
		t.Fatalf("a 24x step produced no estimate: %s", got.Reason)
	}
	if !got.At.Equal(origin.Add(81 * time.Minute)) {
		t.Errorf("onset = %s, want %s", got.At.Format(time.RFC3339), origin.Add(81*time.Minute).Format(time.RFC3339))
	}
	if got.EffectSizeMADs < MinEffectSizeMADs {
		t.Errorf("effect size %.6f MADs on a 24x step", got.EffectSizeMADs)
	}
	if got.Confidence != 1 {
		t.Errorf("confidence = %v on a 24x step, want 1", got.Confidence)
	}
}

// rampSeries is a flat series that ramps linearly from low to high over `over` points starting at
// rampAt, then holds. It is the shape a saturating resource has, and the shape `slow-burn-01`
// describes in prose.
func rampSeries(n, rampAt, over int, low, high float64) []Point {
	series := make([]Point, 0, n)
	for i := range n {
		value := low
		switch {
		case i >= rampAt+over:
			value = high
		case i >= rampAt:
			value = low + (high-low)*float64(i-rampAt)/float64(over)
		}
		value += float64((i*37)%7) * 0.001
		series = append(series, Point{At: origin.Add(time.Duration(i) * time.Minute), Value: round6(value)})
	}
	return series
}

// TestRampOnsetIsNearTheRampStart: a degradation that grows rather than steps is still found, and
// found near where it began rather than where it finished.
//
// "Near" is the right word and the tolerance is the estimate's own: a ramp has no single instant
// at which it began to be visible, so the method is asked to land within its published
// uncertainty of the foot of the ramp, not on it.
func TestRampOnsetIsNearTheRampStart(t *testing.T) {
	t.Parallel()

	const rampAt = 40
	got := EstimateOnset(rampSeries(120, rampAt, 30, 0.01, 0.24), searchWindow(120), Params{})
	if got.Unavailable {
		t.Fatalf("a ramp produced no estimate: %s", got.Reason)
	}
	want := origin.Add(rampAt * time.Minute)
	tolerance := time.Duration(got.UncertaintySeconds) * time.Second
	if tolerance < 10*time.Minute {
		tolerance = 10 * time.Minute
	}
	if delta := got.At.Sub(want); delta < -tolerance || delta > tolerance {
		t.Errorf("onset = %s, want within %s of the foot of the ramp at %s (off by %s)",
			got.At.Format(time.RFC3339), tolerance, want.Format(time.RFC3339), delta)
	}
	if got.Direction != "up" {
		t.Errorf("direction = %q, want up", got.Direction)
	}
	if got.EffectSizeMADs < MinEffectSizeMADs {
		t.Errorf("effect size %.6f MADs on a ramp to 24x", got.EffectSizeMADs)
	}
}

// TestShiftAtTheTailIsNotAnOnset: a level that changes in the last three samples of the window has
// not been observed to persist. Calling it an onset is a prediction, not a measurement.
func TestShiftAtTheTailIsNotAnOnset(t *testing.T) {
	t.Parallel()

	// Enough points for the CUSUM to reach its threshold, with the shift placed so that fewer
	// than MinSustainedPoints follow it.
	series := stepSeries(120, 118, 0.01, 0.9)
	got := EstimateOnset(series, searchWindow(120), Params{})
	if !got.Unavailable {
		t.Fatalf("a shift in the last two samples produced an onset at %s", got.At.Format(time.RFC3339))
	}
	if got.Reason != ReasonNoOnsetDetected && got.Reason != ReasonNoCrossing {
		t.Errorf("reason = %q", got.Reason)
	}
}

// TestCriterionParametersAreInTheDigest: an estimate states the arithmetic that produced it,
// including the two parameters Track J added. A recorded world that does not carry them cannot be
// re-derived.
func TestCriterionParametersAreInTheDigest(t *testing.T) {
	t.Parallel()

	params := DefaultParams().Map()
	for key, want := range map[string]float64{
		"k_effect":             MinEffectSizeMADs,
		"min_sustained_points": MinSustainedPoints,
	} {
		got, ok := params[key]
		if !ok {
			t.Errorf("method_parameters carries no %q", key)
			continue
		}
		if got != want {
			t.Errorf("method_parameters[%q] = %v, want %v", key, got, want)
		}
	}
}

// TestCriterionIsDeterministic: the criterion is arithmetic like the rest of the method, and the
// onset determinism job in CI runs this package on both architectures. Two runs over one series
// must agree on the instant, the effect size and the confidence to the digit.
func TestCriterionIsDeterministic(t *testing.T) {
	t.Parallel()

	series := stepSeries(120, 81, 0.01, 0.24)
	want := EstimateOnset(series, searchWindow(120), Params{})
	for range 64 {
		got := EstimateOnset(series, searchWindow(120), Params{})
		if !got.At.Equal(want.At) || got.EffectSizeMADs != want.EffectSizeMADs || got.Confidence != want.Confidence {
			t.Fatalf("two runs disagreed: %s effect %.6f conf %.6f, then %s effect %.6f conf %.6f",
				want.At.Format(time.RFC3339), want.EffectSizeMADs, want.Confidence,
				got.At.Format(time.RFC3339), got.EffectSizeMADs, got.Confidence)
		}
	}
}
