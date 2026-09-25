// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

// The posterior rule (T052, FR-023, SC-004, research §8, plan §Design Decision 6).
//
// Confidence is a normalised naive-Bayes posterior over a closed hypothesis set that always
// contains the open hypothesis:
//
//	ln posterior_i = ln prior_i + Σ_j ln LR_ij,   normalised by log-sum-exp, rounded to 6 decimals.
//
// Three properties of that line are the reason it is written this way rather than any other.
//
//   - It is *order independent*. Addition commutes, so the posterior cannot depend on the order
//     judgments arrived in. That is the formal statement of "two runs that gather the same
//     evidence agree", and posterior_property_test.go asserts it over every permutation rapid
//     can find.
//   - It is *reproducible from the rows*. The ln LR values are published constants stored on the
//     judgment, so a ledger recomputed in 2029 from the database alone reproduces the number a
//     report published in 2026 — even if the table below is revised in between, because the
//     revision writes new rows and never rewrites old ones.
//   - It *sums to one exactly*. Not "to within floating point": the six-decimal rounding is a
//     largest-remainder apportionment over 1 000 000 units, so the set sums to 1.000000 on the
//     nose. The database's deferred trigger allows 1e-6 of slack and independent rounding of 50
//     hypotheses can drift 25 times that, so rounding each number on its own would make the
//     invariant a coin toss on wide investigations. See roundToUnitSum.
//
// The independence assumption between judgments is wrong in the usual way — two digests from the
// same window are correlated — and the mitigations are the published ones: at most one judgment
// per (evidence item, hypothesis) pair, and coarse buckets until calibration earns a finer scale.

// LedgerRuleVersion is the published version of the rule in this file *and* of the LR table and
// the bucket boundaries below. It is recorded on every investigation
// (`investigations.ledger_rule_version`) and carried in the exported Ledger, because a confidence
// is only interpretable against the rule that produced it. It moves when any constant here moves.
//
//nolint:revive // "ledger.LedgerRuleVersion" stutters, but this name is published in the schema.
const LedgerRuleVersion = "1.0.0"

// The published likelihood ratios (research §8). A `supports` judgment multiplies the odds by the
// ratio; a `refutes` judgment divides by it, which in log space is the same constant negated.
//
// The numbers are a scale, not a measurement: 1.5 is "this nudges it", 50 is "it would be strange
// for this to be true and the hypothesis false". They stay coarse on purpose — four rungs an SRE
// can hold in their head, and no rung that single-handedly decides an investigation, since even a
// decisive judgment leaves the open hypothesis with a share of the mass.
const (
	LRWeak     = 1.5
	LRModerate = 3.0
	LRStrong   = 10.0
	LRDecisive = 50.0
	LRNeutral  = 1.0
)

// The published ln LR constants: ln of the ratios above, rounded to six decimals.
//
// They are written out rather than computed with math.Log at init for the reason the whole file
// exists: `judgments.ln_lr` is numeric(12,6), so a value with more precision than this could not
// survive a round-trip through the database, and a posterior recomputed from the rows would not
// reproduce the live one. LnLRMatchesTheRatios in posterior_test.go asserts each constant is the
// six-decimal rounding of its ratio, so the table cannot drift from its own documentation.
const (
	LnLRWeak     = 0.405465
	LnLRModerate = 1.098612
	LnLRStrong   = 2.302585
	LnLRDecisive = 3.912023
	LnLRNeutral  = 0.0
)

// LikelihoodRatio returns the published LR for a strength, ignoring direction.
func LikelihoodRatio(s Strength) (float64, error) {
	switch s {
	case Weak:
		return LRWeak, nil
	case Moderate:
		return LRModerate, nil
	case Strong:
		return LRStrong, nil
	case Decisive:
		return LRDecisive, nil
	default:
		return 0, fmt.Errorf("ledger: %w: strength %q is not on the published scale", ErrVocabulary, s)
	}
}

// LnLR returns the signed log-likelihood ratio a (direction, strength) pair contributes.
//
// The sign is the direction and nothing else, which is what the schema's
// `judgments_direction_sign_check` asserts from the other side: supports raises the odds, refutes
// lowers them by the reciprocal ratio, neutral moves nothing whatever strength it claims.
func LnLR(d Direction, s Strength) (float64, error) {
	if d == Neutral {
		return LnLRNeutral, nil
	}
	var ln float64
	switch s {
	case Weak:
		ln = LnLRWeak
	case Moderate:
		ln = LnLRModerate
	case Strong:
		ln = LnLRStrong
	case Decisive:
		ln = LnLRDecisive
	default:
		return 0, fmt.Errorf("ledger: %w: strength %q is not on the published scale", ErrVocabulary, s)
	}
	switch d {
	case Supports:
		return ln, nil
	case Refutes:
		return -ln, nil
	default:
		return 0, fmt.Errorf("ledger: %w: direction %q is not supports, refutes or neutral", ErrVocabulary, d)
	}
}

// Bucket is one published coarse confidence bucket, carrying its range so that every rendering
// states what the bucket means rather than assuming the reader knows (FR-023).
type Bucket struct {
	// Name is the published bucket name.
	Name string `json:"name"`
	// Low is the inclusive lower bound.
	Low float64 `json:"range_low"`
	// High is the upper bound: exclusive for every bucket but the last, which includes 1.0.
	High float64 `json:"range_high"`
}

// String renders a bucket the way the compact table and the report both spell it.
func (b Bucket) String() string {
	if b.Name == "" {
		return ""
	}
	closing := ")"
	if b.High >= 1 {
		closing = "]"
	}
	return fmt.Sprintf("%s [%.2f, %.2f%s", b.Name, b.Low, b.High, closing)
}

// The five published buckets (FR-023, research §8). Coarse until the corpus justifies finer: a
// bucket is only meaningful once enough hypotheses have landed in it to measure its accuracy,
// and SC-004 sets that bar at 20 per bucket per evaluation run.
var buckets = []Bucket{
	{Name: "very_low", Low: 0.00, High: 0.10},
	{Name: "low", Low: 0.10, High: 0.30},
	{Name: "moderate", Low: 0.30, High: 0.60},
	{Name: "high", Low: 0.60, High: 0.85},
	{Name: "very_high", Low: 0.85, High: 1.00},
}

// Buckets returns the published buckets in order, lowest first. The slice is a copy: the
// boundaries are published, so a caller cannot move them by accident.
func Buckets() []Bucket { return slices.Clone(buckets) }

// BucketFor returns the published bucket a confidence falls in. Ranges are half-open except the
// top one, which closes at 1.0 so a certainty has somewhere to live.
func BucketFor(confidence float64) Bucket {
	for _, b := range buckets {
		if confidence < b.High {
			return b
		}
	}
	return buckets[len(buckets)-1]
}

// bucketIndex is BucketFor's position, which widening needs.
func bucketIndex(confidence float64) int {
	for i, b := range buckets {
		if confidence < b.High {
			return i
		}
	}
	return len(buckets) - 1
}

// PriorInput is one candidate's published ranker score, as the ranking formula produced it
// (001 §ranking, `docs/schema/ranking.md`).
type PriorInput struct {
	// HypothesisID names the hypothesis the score belongs to.
	HypothesisID string
	// Score is the published ranker score. Scores are normalised over the candidate set, so
	// their scale does not matter; their ratios do.
	Score float64
}

// Priors turns ranker scores and π₀ into the ledger's priors (research §8).
//
// A candidate's prior is its score normalised over the candidate set and scaled by `1 − π₀`; the
// open hypothesis takes π₀ itself, which is the measured probability that the cause is not
// visible to this deployment's feeders at all. The returned map is keyed by hypothesis id and
// contains one entry per input plus `openID`; the values are six-decimal multiples that sum to
// exactly 1, so a ledger with no judgments yet has posteriors equal to its priors.
//
// Two degenerate cases, both deliberate:
//
//   - π₀ = 1 (no audit has been published) leaves every candidate at prior 0. That is the honest
//     reading of "nothing measured says any cause is observable", and it is what makes an
//     engine with no coverage audit report `unknown` rather than a confident guess.
//   - scores that are all zero, or no candidates at all, put the whole mass on the open
//     hypothesis for the same reason.
func Priors(scores []PriorInput, pi0 float64, openID string) (map[string]float64, error) {
	if openID == "" {
		return nil, fmt.Errorf("ledger: priors: the open hypothesis needs an id")
	}
	if math.IsNaN(pi0) || pi0 < 0 || pi0 > 1 {
		return nil, fmt.Errorf("ledger: priors: π₀ = %v is not a probability", pi0)
	}

	total := 0.0
	for _, s := range scores {
		if math.IsNaN(s.Score) || s.Score < 0 {
			return nil, fmt.Errorf("ledger: priors: %s has score %v; a ranker score is non-negative",
				s.HypothesisID, s.Score)
		}
		total += s.Score
	}

	weights := make([]weight, 0, len(scores)+1)
	candidateMass := 1 - pi0
	for _, s := range scores {
		share := 0.0
		if total > 0 {
			share = candidateMass * s.Score / total
		}
		weights = append(weights, weight{id: s.HypothesisID, value: share})
	}
	// Whatever the candidates did not take belongs to the open hypothesis: exactly π₀ when there
	// are candidates with scores, and everything when there are not.
	openShare := pi0
	if total == 0 {
		openShare = 1
	}
	weights = append(weights, weight{id: openID, value: openShare})

	return roundToUnitSum(weights), nil
}

// Posteriors computes every hypothesis's confidence from the priors and the judgments (FR-023).
//
// It is the whole of the ledger rule: no argument is a model output, no field of the result is a
// model output, and calling it twice on the same rows returns the same bytes. Judgments naming a
// hypothesis outside the set are ignored rather than rejected, so that a ledger rendered from a
// filtered subset still computes — the DAO is where a dangling judgment is refused.
//
// The result is keyed by hypothesis id, every value a six-decimal multiple, summing to exactly 1
// when the priors do (Invariant 10).
func Posteriors(hypotheses []Hypothesis, judgments []Judgment) map[string]float64 {
	if len(hypotheses) == 0 {
		return map[string]float64{}
	}

	// Σ ln LR per hypothesis. A map, so the sum is over a set rather than over an arrival order.
	sums := make(map[string]float64, len(hypotheses))
	for _, h := range hypotheses {
		sums[h.ID] = 0
	}
	for _, j := range judgments {
		if _, ok := sums[j.HypothesisID]; !ok {
			continue
		}
		sums[j.HypothesisID] += j.LnLR
	}

	// ln prior + Σ ln LR, with a zero prior staying zero: no amount of evidence resurrects a
	// hypothesis the priors gave no mass, which is the arithmetic saying what π₀ = 1 means.
	logits := make([]weight, 0, len(hypotheses))
	maxLogit := math.Inf(-1)
	for _, h := range hypotheses {
		if h.Prior <= 0 {
			logits = append(logits, weight{id: h.ID, value: math.Inf(-1)})
			continue
		}
		logit := math.Log(h.Prior) + sums[h.ID]
		logits = append(logits, weight{id: h.ID, value: logit})
		if logit > maxLogit {
			maxLogit = logit
		}
	}

	// Numerically stable log-sum-exp: subtracting the maximum keeps exp() inside its range for
	// the decisive-judgment ledgers where the difference between the best and worst logit is
	// dozens of nats.
	if math.IsInf(maxLogit, -1) {
		// Every prior was zero. Nothing is believable; the caller's priors are the bug, and the
		// honest answer is a flat zero rather than a fabricated distribution.
		out := make(map[string]float64, len(hypotheses))
		for _, h := range hypotheses {
			out[h.ID] = 0
		}
		return out
	}
	total := 0.0
	for i := range logits {
		if math.IsInf(logits[i].value, -1) {
			logits[i].value = 0
			continue
		}
		logits[i].value = math.Exp(logits[i].value - maxLogit)
		total += logits[i].value
	}
	for i := range logits {
		logits[i].value /= total
	}

	return roundToUnitSum(logits)
}

// weight is an (id, share) pair on its way to being rounded.
type weight struct {
	id    string
	value float64
}

// unitsPerProbability is the six-decimal grid the schema stores confidences on: numeric(9,6).
const unitsPerProbability = 1_000_000

// roundToUnitSum rounds shares to six decimals so that they still sum to exactly 1.
//
// Rounding each share on its own is the obvious thing and it is wrong here. Each rounding can
// move a share by up to 5e-7, so 50 hypotheses can drift 2.5e-5 from 1 — twenty-five times the
// tolerance the deferred `hypotheses_posteriors_sum_to_one` trigger allows, which would make
// Invariant 10 fail on exactly the wide investigations it matters for.
//
// So the shares are apportioned instead: each takes its floor in millionths, and the millionths
// left over go to the largest fractional remainders, ties broken by hypothesis id. That is the
// largest-remainder method, it is deterministic on any architecture, it never moves a share by
// more than one millionth, and the total is 1 000 000 millionths by construction.
//
// A share of exactly zero keeps a remainder of zero and therefore never receives a leftover unit:
// the number of strictly positive remainders is always at least the number of units to hand out.
func roundToUnitSum(shares []weight) map[string]float64 {
	type apportioned struct {
		id        string
		units     int64
		remainder float64
	}

	items := make([]apportioned, 0, len(shares))
	assigned := int64(0)
	for _, s := range shares {
		value := s.value
		if math.IsNaN(value) || value < 0 {
			value = 0
		}
		scaled := value * unitsPerProbability
		units := int64(math.Floor(scaled))
		items = append(items, apportioned{id: s.id, units: units, remainder: scaled - float64(units)})
		assigned += units
	}

	leftover := int64(unitsPerProbability) - assigned
	if leftover > 0 {
		order := slices.Clone(items)
		slices.SortFunc(order, func(a, b apportioned) int {
			if c := cmp.Compare(b.remainder, a.remainder); c != 0 {
				return c
			}
			return cmp.Compare(a.id, b.id)
		})
		bonus := make(map[string]int64, leftover)
		for i := int64(0); i < leftover && i < int64(len(order)); i++ {
			bonus[order[i].id]++
		}
		for i := range items {
			items[i].units += bonus[items[i].id]
		}
	}

	out := make(map[string]float64, len(items))
	for _, item := range items {
		out[item.id] = float64(item.units) / unitsPerProbability
	}
	return out
}

// round6 is the project's six-decimal rounding, the same discipline the ranking formula and the
// coverage audit already publish (Invariant 1, SC-019).
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
