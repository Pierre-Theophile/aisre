// SPDX-License-Identifier: Apache-2.0

// Package onset is the reference implementation of the algebra's `onset` term: a seasonal-naive
// baseline, a two-sided CUSUM over the standardised residual, and a binary-segmentation refinement
// of the alarm instant (tasks.md T045, research §4, plan §Design Decisions 5).
//
// It lives in the public SDK because every backend calls it rather than inventing its own. The
// *algorithm* is this feature's, versioned with this feature's algebra; only its *execution*
// happens on the vendor side of the digest boundary, which is what satisfies FR-029a and
// constitution IV at the same time: change-point detection needs the series, and the series may
// never reach the investigator. What crosses the boundary is the estimate — an instant, an
// uncertainty, the method and its parameters — which is evidence like any other.
//
// # Why this method
//
// The question is "when did this first depart from normal and stay departed", not "what is the
// optimal segmentation of this series". A seasonal-naive baseline (the median of the same clock
// offset over the previous k periods) fits nothing, tunes nothing and degrades honestly when
// history is short. A two-sided CUSUM over the standardised residual is O(n), has one published
// threshold h and one published drift allowance k, and its run length at the crossing *is* the
// uncertainty. Binary segmentation inside the alarm window sharpens the instant without
// introducing a penalty parameter that would be tuned on eight fixtures and overfit.
//
// # Determinism across architectures
//
// Goldens are compared byte for byte and CI runs on both amd64 and arm64, so the estimate must
// be identical on both. The hazard is real: Go permits a compiler to fuse `a*b + c` into a
// single fused multiply-add on arm64 and not on amd64, and the two differ in the last bits.
//
// The discipline, taken from the ranking formula for the same reason, is **rounding at each
// accumulation step**: every value that feeds a running total is rounded to six decimals before
// it is stored, so no intermediate ever carries bits that could have been fused differently.
// The result is that both architectures do arithmetic on the same six-decimal quantities, and
// the sequence of comparisons — which is what actually selects the change point — cannot
// diverge. Every accumulation in this file goes through round6; there are no exceptions, and
// TestDeterminismFixedSeries pins the exact digits.
package onset

import (
	"math"
	"sort"
	"time"
)

// MethodName is the published method identifier recorded in every onset digest.
const MethodName = "seasonal_naive_cusum_binseg"

// Version is the estimator's own version. It moves when the arithmetic moves, which invalidates
// every recorded onset answer and is therefore a published schema change.
const Version = "1.0.0"

// The published parameters. They are constants rather than knobs because a threshold an operator
// can turn is a threshold that gets turned until the fixture passes.
const (
	// DriftAllowance (k) is how far, in robust sigmas, a point may depart from the baseline
	// before the CUSUM starts accumulating. It is the slack that stops ordinary noise from
	// walking the statistic upward.
	DriftAllowance = 0.5
	// DecisionThreshold (h) is how much accumulated departure raises the alarm. With k = 0.5 a
	// sustained one-sigma shift crosses h = 5 in ten points; a single five-sigma spike does not.
	DecisionThreshold = 5.0
	// SeasonalPeriods is how many previous periods the seasonal-naive baseline takes the median
	// of at each clock offset.
	SeasonalPeriods = 3
	// SeasonalPeriodSeconds is one day. A week is the second period the method allows for, and
	// is used when a day's history is not available but a week's is.
	SeasonalPeriodSeconds = 86400
	// MinPoints is the fewest points an estimate may be made from. Below it the answer is
	// `unavailable` with a reason, never a guess.
	MinPoints = 8
	// MADScale converts a median absolute deviation into a standard-deviation-equivalent for a
	// normal distribution. It is a published constant of the method.
	MADScale = 1.4826
	// MinEffectSizeMADs (k_effect) is how far, in scaled median absolute deviations of the
	// baseline, the post-onset level must sit from the baseline level before a crossing is
	// reported as an onset at all. Three is the published default: under the generator's own
	// ±4 % wobble a five-point mean would have to be roughly eight standard errors away to reach
	// it, so seeded noise cannot manufacture an onset, while every real degradation in this
	// corpus clears it by more than an order of magnitude.
	MinEffectSizeMADs = 3.0
	// MinSustainedPoints is how many points the shifted level must be held for. Five at a
	// one-minute resolution is five minutes, which is the shortest thing this project is willing
	// to call a symptom rather than a spike — and it is also the duration the corpus's own
	// monitors are specified over ("above 5 % for 5 minutes").
	MinSustainedPoints = 5
)

// Point is one sample of the series, as the backend read it from its vendor. Points never leave
// the backend process: this type exists on the vendor side of the digest boundary only.
type Point struct {
	// At is the sample instant.
	At time.Time
	// Value is the sample.
	Value float64
}

// Params are the method's parameters, recorded in the digest so the estimate states how it was
// produced. Zero fields take the published defaults.
type Params struct {
	// DriftAllowance is k.
	DriftAllowance float64
	// DecisionThreshold is h.
	DecisionThreshold float64
	// SeasonalPeriods is how many previous periods the baseline medians over.
	SeasonalPeriods int
	// SeasonalPeriodSeconds is the period length.
	SeasonalPeriodSeconds int64
	// MinPoints is the minimum series length.
	MinPoints int
	// MinEffectSizeMADs is k_effect: the minimum effect size, in scaled median absolute
	// deviations of the baseline, a crossing must reach to be reported as an onset.
	MinEffectSizeMADs float64
	// MinSustainedPoints is the minimum run of points the shifted level must be held for.
	MinSustainedPoints int
}

// DefaultParams returns the published parameters.
func DefaultParams() Params {
	return Params{
		DriftAllowance:        DriftAllowance,
		DecisionThreshold:     DecisionThreshold,
		SeasonalPeriods:       SeasonalPeriods,
		SeasonalPeriodSeconds: SeasonalPeriodSeconds,
		MinPoints:             MinPoints,
		MinEffectSizeMADs:     MinEffectSizeMADs,
		MinSustainedPoints:    MinSustainedPoints,
	}
}

func (p Params) withDefaults() Params {
	d := DefaultParams()
	if p.DriftAllowance <= 0 {
		p.DriftAllowance = d.DriftAllowance
	}
	if p.DecisionThreshold <= 0 {
		p.DecisionThreshold = d.DecisionThreshold
	}
	if p.SeasonalPeriods <= 0 {
		p.SeasonalPeriods = d.SeasonalPeriods
	}
	if p.SeasonalPeriodSeconds <= 0 {
		p.SeasonalPeriodSeconds = d.SeasonalPeriodSeconds
	}
	if p.MinPoints <= 0 {
		p.MinPoints = d.MinPoints
	}
	if p.MinEffectSizeMADs <= 0 {
		p.MinEffectSizeMADs = d.MinEffectSizeMADs
	}
	if p.MinSustainedPoints <= 0 {
		p.MinSustainedPoints = d.MinSustainedPoints
	}
	return p
}

// Map renders the parameters as the digest's method_parameters map, so an estimate carries the
// arithmetic that produced it.
func (p Params) Map() map[string]float64 {
	p = p.withDefaults()
	return map[string]float64{
		"k":                       p.DriftAllowance,
		"h":                       p.DecisionThreshold,
		"seasonal_periods":        float64(p.SeasonalPeriods),
		"seasonal_period_seconds": float64(p.SeasonalPeriodSeconds),
		"min_points":              float64(p.MinPoints),
		"k_effect":                p.MinEffectSizeMADs,
		"min_sustained_points":    float64(p.MinSustainedPoints),
	}
}

// The published reasons an estimate is unavailable. Each is a statement the engine can act on:
// none of them is "we did not find anything", because a method that cannot tell "nothing
// happened" from "we could not look" exonerates innocent changes on timing alone.
const (
	// ReasonTooSparse is fewer than MinPoints samples in the search window.
	ReasonTooSparse = "too_sparse"
	// ReasonFlat is a series with no variation to measure a departure against.
	ReasonFlat = "flat"
	// ReasonNoCrossing is a series that varies but never accumulates past the threshold.
	ReasonNoCrossing = "no_crossing"
	// ReasonAbsent is an empty series.
	ReasonAbsent = "absent"
	// ReasonNoOnsetDetected is a series in which the CUSUM crossed but the crossing did not
	// describe a real level shift: the post-crossing level is not MinEffectSizeMADs away from the
	// baseline's own dispersion, or it is not held for MinSustainedPoints.
	//
	// This is the reason that separates "nothing happened here" from "we could not look", and it
	// exists because the alternative is worse than useless. A CUSUM over a wide search window
	// will eventually accumulate past its threshold on noise alone; returning the earliest such
	// crossing puts a confident instant on a flat series, and every causal-ordering decision
	// downstream of it — which changes started before the symptom, which are exonerated as
	// effects — is then keyed to an instant that describes nothing. An estimate with confidence 0
	// and a stated reason is the honest answer, and the engine already knows what to do with it:
	// it falls back to the alert instant, says so, and exonerates nobody on timing alone.
	ReasonNoOnsetDetected = "no_onset_detected"
)

// Estimate is what the method produced. It is the whole of what crosses the digest boundary.
type Estimate struct {
	// At is the estimated instant the symptom began.
	At time.Time
	// UncertaintySeconds is the half-width of the interval around At: the CUSUM's run length at
	// the crossing, widened when the baseline had to fall back.
	UncertaintySeconds int64
	// Method is MethodName.
	Method string
	// Params are the parameters in force.
	Params Params
	// Unavailable says no estimate was made, and Reason says why. The engine falls back to the
	// alert instant, says so in the output, and exonerates nobody on timing alone.
	Unavailable bool
	// Reason is one of the published reasons above.
	Reason string
	// BaselineFallback says the seasonal-naive baseline had too little history and a flat
	// baseline was used instead, which is why the uncertainty is wider.
	BaselineFallback bool
	// Examined is the window the estimate was made over, which may be narrower than the one
	// asked for.
	Examined Window
	// Direction is "up" or "down": which side of the CUSUM crossed. It is recorded because an
	// error rate that fell and a latency that rose are different stories.
	Direction string
	// Statistic is the binary-segmentation statistic at the chosen point, at six decimals. It
	// is the number a reviewer compares when two runs disagree.
	Statistic float64
	// EffectSizeMADs is how far the post-onset level sits from the baseline level, in scaled
	// median absolute deviations of the baseline. It is the number the criterion is applied to
	// and the number a reviewer disputes.
	EffectSizeMADs float64
	// Confidence is 0 whenever the estimate is unavailable — that is what "no onset detected"
	// means numerically — and `min(1, EffectSizeMADs / (2·k_effect))` otherwise, so an estimate
	// exactly at the criterion carries 0.5 and one at twice the criterion carries 1.
	//
	// It has no field of its own in `OnsetDigest`, which this feature's algebra version does not
	// change: on the wire, confidence 0 is `unavailable: true` with `unavailable_reason:
	// no_onset_detected`. A numeric confidence is a digest field the next algebra version can add
	// without breaking anything; inventing one here would be a breaking change to buy a number
	// nothing reads yet.
	Confidence float64
}

// Window is the half-open interval an estimate examined.
type Window struct {
	// Start is inclusive.
	Start time.Time
	// End is exclusive.
	End time.Time
}

// Estimate runs the published method over series within search. The series is sorted and
// de-duplicated first, so a backend that returns points out of order gets the same answer as one
// that does not: the estimate is a function of the *series*, not of the vendor's pagination.
func EstimateOnset(series []Point, search Window, params Params) Estimate {
	p := params.withDefaults()
	out := Estimate{Method: MethodName, Params: p, Examined: search}

	points := prepare(series, search)
	if len(points) == 0 {
		out.Unavailable, out.Reason = true, ReasonAbsent
		return out
	}
	out.Examined = Window{Start: points[0].At, End: points[len(points)-1].At.Add(time.Second)}
	if len(points) < p.MinPoints {
		out.Unavailable, out.Reason = true, ReasonTooSparse
		return out
	}

	baseline, fallback := seasonalNaiveBaseline(points, p)
	out.BaselineFallback = fallback

	residuals := make([]float64, len(points))
	for i, pt := range points {
		residuals[i] = round6(pt.Value - baseline[i])
	}
	sigma := robustScale(residuals)
	if sigma == 0 {
		out.Unavailable, out.Reason = true, ReasonFlat
		return out
	}

	alarm, runStart, direction, ok := cusum(residuals, sigma, p)
	if !ok {
		out.Unavailable, out.Reason = true, ReasonNoCrossing
		return out
	}

	change, statistic := refine(residuals, refinementStart(runStart, alarm), alarm)

	// The crossing is a candidate, not an answer. A CUSUM over a wide enough search window
	// accumulates past its threshold on noise alone, and the earliest such crossing is a
	// confident instant describing nothing. Before the instant is published it has to survive a
	// test against the series' own dispersion (T-note: Phase 8 Track J).
	effect, ok := effectSize(points, change, direction, p)
	if !ok {
		out.Unavailable, out.Reason = true, ReasonNoOnsetDetected
		return out
	}

	resolution := resolutionSeconds(points)
	uncertainty := int64(alarm-runStart) * resolution
	if uncertainty < resolution {
		uncertainty = resolution
	}
	if fallback {
		// A flat baseline cannot tell a daily shape from a shift, so the interval is widened
		// rather than the estimate suppressed — and the widening is declared, not hidden.
		uncertainty *= 2
	}

	out.At = points[change].At
	out.UncertaintySeconds = uncertainty
	out.Direction = direction
	out.Statistic = statistic
	out.EffectSizeMADs = effect
	out.Confidence = confidenceOf(effect, p.MinEffectSizeMADs)
	return out
}

// effectSize measures how far the level after `change` sits from the level before it, in scaled
// median absolute deviations of the *baseline* segment, and says whether that clears the
// published criterion.
//
// Two conditions, both necessary, and both stated in units the series supplies rather than in
// absolute numbers a fixture could be tuned against:
//
//  1. **Effect size.** The mean of the run beginning at `change` must sit at least
//     `MinEffectSizeMADs` scaled MADs of the pre-change segment away from that segment's mean, on
//     the side the CUSUM crossed. Scaling by the baseline's own dispersion is what makes the test
//     mean the same thing on an error rate of 0.4 % and one of 40 %.
//  2. **Persistence.** There must be at least `MinSustainedPoints` points from `change` to the
//     end of the series. A shift that begins in the last three samples of a window has not been
//     observed to persist; calling it an onset is a prediction, not a measurement.
//
// A pre-change segment of fewer than two points cannot supply a dispersion, so a crossing at the
// very head of the window is refused for the same reason: there is no baseline to be different
// from.
func effectSize(points []Point, change int, direction string, p Params) (float64, bool) {
	if change < 2 || change >= len(points) {
		return 0, false
	}
	if len(points)-change < p.MinSustainedPoints {
		return 0, false
	}

	pre := make([]float64, 0, change)
	for _, pt := range points[:change] {
		pre = append(pre, pt.Value)
	}
	spread := dispersion(pre)
	if spread <= 0 {
		// A baseline with no dispersion at all: robustScale already refused this series as flat,
		// so reaching here means the pre-segment alone is constant while the whole window is not.
		// Any departure from a constant baseline is infinitely many MADs, so the criterion is met
		// and the effect size is reported at the cap.
		return round6(2 * p.MinEffectSizeMADs), true
	}

	post := make([]float64, 0, p.MinSustainedPoints)
	for _, pt := range points[change : change+p.MinSustainedPoints] {
		post = append(post, pt.Value)
	}
	delta := round6(meanOf(post) - meanOf(pre))
	if direction == "down" {
		delta = -delta
	}
	if delta <= 0 {
		// The crossing says one thing and the levels say another. That is not an onset.
		return 0, false
	}
	effect := round6(delta / spread)
	return effect, effect >= p.MinEffectSizeMADs
}

// dispersion is the baseline's scaled median absolute deviation, with the same mean-absolute
// fallback robustScale uses when more than half the values are identical.
func dispersion(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	centre := median(values)
	deviations := make([]float64, len(values))
	for i, v := range values {
		deviations[i] = round6(math.Abs(round6(v - centre)))
	}
	if scale := round6(MADScale * median(deviations)); scale > 0 {
		return scale
	}
	var total float64
	for _, d := range deviations {
		total = round6(total + d)
	}
	return round6(total / float64(len(deviations)))
}

// meanOf is the mean, accumulated with rounding at each step like everything else here.
func meanOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, v := range values {
		total = round6(total + v)
	}
	return round6(total / float64(len(values)))
}

// confidenceOf maps an effect size onto [0, 1]: 0.5 at exactly the criterion, 1 at twice it and
// beyond. It is a published monotone function of the effect size and nothing else, so two runs
// that agree on the series agree on the confidence.
func confidenceOf(effect, criterion float64) float64 {
	if criterion <= 0 {
		return 0
	}
	value := round6(effect / round6(2*criterion))
	if value > 1 {
		return 1
	}
	if value < 0 {
		return 0
	}
	return value
}

// prepare sorts, de-duplicates and clips the series to the search window. Two samples at one
// instant is a vendor artefact, and keeping both would make the answer depend on which arrived
// first.
func prepare(series []Point, search Window) []Point {
	out := make([]Point, 0, len(series))
	for _, pt := range series {
		at := pt.At.UTC().Truncate(time.Second)
		if !search.Start.IsZero() && at.Before(search.Start.UTC()) {
			continue
		}
		if !search.End.IsZero() && !at.Before(search.End.UTC()) {
			continue
		}
		out = append(out, Point{At: at, Value: round6(pt.Value)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	deduped := out[:0]
	for i, pt := range out {
		if i > 0 && pt.At.Equal(deduped[len(deduped)-1].At) {
			deduped[len(deduped)-1] = pt // last write wins, deterministically
			continue
		}
		deduped = append(deduped, pt)
	}
	return deduped
}

// seasonalNaiveBaseline returns the baseline at each point: the median of the values at the same
// clock offset over the previous SeasonalPeriods periods. Where a point has no history at all,
// the whole baseline falls back to the median of the first quarter of the window, and the caller
// widens the uncertainty and says so.
func seasonalNaiveBaseline(points []Point, p Params) ([]float64, bool) {
	byInstant := make(map[int64]float64, len(points))
	for _, pt := range points {
		byInstant[pt.At.Unix()] = pt.Value
	}

	baseline := make([]float64, len(points))
	hasHistory := make([]bool, len(points))
	var withHistory int
	for i, pt := range points {
		history := make([]float64, 0, p.SeasonalPeriods)
		for back := 1; back <= p.SeasonalPeriods; back++ {
			at := pt.At.Unix() - int64(back)*p.SeasonalPeriodSeconds
			if v, ok := byInstant[at]; ok {
				history = append(history, v)
			}
		}
		if len(history) == 0 {
			hasHistory[i] = false
			continue
		}
		hasHistory[i] = true
		withHistory++
		baseline[i] = median(history)
	}

	// A flat baseline from the earliest quarter of the window — the part least likely to hold
	// the shift we are looking for — fills in wherever there is no seasonal history. Every
	// window has such a gap at its head, so the presence of one is not itself a fallback.
	head := len(points) / 4
	if head < p.MinPoints {
		head = p.MinPoints
	}
	if head > len(points) {
		head = len(points)
	}
	values := make([]float64, 0, head)
	for _, pt := range points[:head] {
		values = append(values, pt.Value)
	}
	flat := median(values)
	for i := range baseline {
		if !hasHistory[i] {
			baseline[i] = flat
		}
	}

	// The fallback is declared when the seasonal baseline covers less than half the window: at
	// that point the estimate can no longer tell a daily shape from a shift, and the caller
	// widens the uncertainty and says so rather than pretending otherwise.
	return baseline, withHistory*2 < len(points)
}

// robustScale is the median absolute deviation of the residuals, scaled to a
// standard-deviation equivalent. It is robust because the shift we are looking for is in the
// data we are scaling by: a plain standard deviation would grow with the very departure it is
// supposed to measure.
func robustScale(residuals []float64) float64 {
	centre := median(residuals)
	deviations := make([]float64, len(residuals))
	for i, r := range residuals {
		deviations[i] = round6(math.Abs(round6(r - centre)))
	}
	scale := round6(MADScale * median(deviations))
	if scale > 0 {
		return scale
	}
	// A MAD of zero means more than half the residuals are identical, which happens on a
	// step-shaped series. Fall back to the mean absolute deviation, accumulated with rounding.
	var total float64
	for _, d := range deviations {
		total = round6(total + d)
	}
	return round6(total / float64(len(deviations)))
}

// cusum runs the two-sided CUSUM over the standardised residual and returns the index that
// raised the alarm, the index the run that raised it started at, and which side crossed.
//
// Every accumulation is rounded: this is the loop the determinism of the whole estimate rests
// on, because the comparison `sPlus > h` is what selects the alarm index and a last-bit
// difference in sPlus is a different index on a series that sits near the threshold.
func cusum(residuals []float64, sigma float64, p Params) (alarm, runStart int, direction string, ok bool) {
	var sPlus, sMinus float64
	plusStart, minusStart := 0, 0

	for i, r := range residuals {
		z := round6(r / sigma)

		next := round6(sPlus + round6(z-p.DriftAllowance))
		if next < 0 {
			next = 0
		}
		if sPlus == 0 && next > 0 {
			plusStart = i
		}
		sPlus = next

		next = round6(sMinus + round6(-z-p.DriftAllowance))
		if next < 0 {
			next = 0
		}
		if sMinus == 0 && next > 0 {
			minusStart = i
		}
		sMinus = next

		// The upward side is tested first, so a series that crosses both at the same index
		// reports "up". That is a published tie-break, not an accident: the symptom that
		// brought us here is almost always something rising.
		if sPlus > p.DecisionThreshold {
			return i, plusStart, "up", true
		}
		if sMinus > p.DecisionThreshold {
			return i, minusStart, "down", true
		}
	}
	return 0, 0, "", false
}

// refinementStart is the left edge of the window binary segmentation searches: as many points
// before the run started as the run itself took, plus one.
//
// Searching only [runStart, alarm] would be searching inside the new regime, where every point
// looks alike and the split is arbitrary. Reaching back the same distance puts the old regime on
// the left of the window, which is what makes the maximising split the boundary between them.
func refinementStart(runStart, alarm int) int {
	span := alarm - runStart
	if span < 1 {
		span = 1
	}
	lo := runStart - span - 1
	if lo < 0 {
		lo = 0
	}
	return lo
}

// refine sharpens the instant by binary segmentation over the residuals of the alarm window: the
// split that maximises the standardised difference of means between the two sides.
//
// The statistic is |mean(left) − mean(right)| · sqrt(nl·nr/n), the usual binary-segmentation
// criterion, computed from prefix sums that are rounded at each step. Ties take the earliest
// index, so the answer is the first instant consistent with the data rather than an arbitrary
// one.
func refine(residuals []float64, from, to int) (index int, statistic float64) {
	if to <= from {
		return to, 0
	}
	segment := residuals[from : to+1]
	n := len(segment)
	if n < 2 {
		return from, 0
	}

	prefix := make([]float64, n+1)
	for i, r := range segment {
		prefix[i+1] = round6(prefix[i] + r)
	}
	total := prefix[n]

	best, bestAt := -1.0, 0
	for split := 1; split < n; split++ {
		left := round6(prefix[split] / float64(split))
		right := round6(round6(total-prefix[split]) / float64(n-split))
		weight := round6(math.Sqrt(round6(float64(split) * float64(n-split) / float64(n))))
		value := round6(math.Abs(round6(left-right)) * weight)
		if value > best {
			best, bestAt = value, split
		}
	}
	// The split index is the first point of the right-hand segment: the first sample that
	// belongs to the new regime, which is what "when did it start" means.
	return from + bestAt, round6(best)
}

// resolutionSeconds is the median gap between consecutive samples, which is the finest instant
// the estimate can honestly claim.
func resolutionSeconds(points []Point) int64 {
	if len(points) < 2 {
		return 1
	}
	gaps := make([]float64, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		gaps = append(gaps, float64(points[i].At.Unix()-points[i-1].At.Unix()))
	}
	gap := int64(median(gaps))
	if gap < 1 {
		return 1
	}
	return gap
}

// median is the usual one, taking the mean of the two middle values for an even count. The mean
// is rounded, like every other accumulation here.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return round6(round6(sorted[mid-1]+sorted[mid]) / 2)
}

// round6 is the project's published rounding: six decimals, applied at every accumulation step
// so that no intermediate can carry bits an architecture might have fused differently.
func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}
