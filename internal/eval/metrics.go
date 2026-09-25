// SPDX-License-Identifier: Apache-2.0

package eval

// The published row shape and the arithmetic behind every number in it (T109, T112; FR-059,
// FR-061, FR-061a).
//
// This file deliberately knows nothing about how a run was produced. `harness.go` and `run.go`
// run the corpus, `policy.go` and `knowability.go` decide what a pass is, `report.go` turns
// graded runs into rows — and everything here is arithmetic over numbers that are already in
// hand, so each one can be tested against an answer computed by hand rather than against another
// run of the same code.
//
// A metric that was not measured is **not** a zero. Every row carries a nullable value, and a
// null prints as `n/a` with a reason: a corpus with no human labels has no agreement figure, and
// publishing 0.00 for it would read as "the engine agreed with nobody".

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Scope is the population a row's value was computed over.
type Scope string

// The two published scopes. There are exactly two because there are exactly two questions:
// "how did this fixture go" and "how did the corpus go". Anything between them (a family, a
// provenance kind) is a slice a reader can take from the fixture rows themselves.
const (
	// ScopeFixture is one incident fixture over its k runs.
	ScopeFixture Scope = "fixture"
	// ScopeCorpus is every fixture × every run.
	ScopeCorpus Scope = "corpus"
)

// Row is one published metric (FR-059). It is the unit `--report-json` writes, one JSON
// document per line, and the unit `scripts/check-report.sh --investigation` gates on.
//
// The shape is flat on purpose. A nested report is pleasant to write and miserable to gate: the
// gate has to name a metric, a scope and a fixture, and every one of those has to survive a
// `jq -r` in a shell script that a reviewer can read.
type Row struct {
	// Metric is the published metric name, stable across runs because the gate names it.
	// A per-key breakdown spells the key into the name — `worker_calls.metrics/compare` —
	// rather than adding a dimension nothing else needs.
	Metric string `json:"metric"`
	// Scope is `fixture` or `corpus`.
	Scope Scope `json:"scope"`
	// Fixture is the incident id for a fixture-scoped row, and empty for a corpus-scoped one.
	Fixture string `json:"fixture,omitempty"`
	// Value is the measurement, or nil when the metric was not measured. Nil is not zero.
	Value *float64 `json:"value"`
	// N is how many observations the value rests on — runs, claims, hypotheses, trials. A rate
	// with no n behind it is not a measurement, so every rate carries one.
	N int `json:"n"`
	// Detail is the sentence a reader needs to interpret the number: why it is null, what the
	// breakdown was, which variant diverged.
	Detail string `json:"detail,omitempty"`
}

// NewRow builds a measured row.
func NewRow(metric string, scope Scope, fixture string, value float64, n int, detail string) Row {
	return Row{Metric: metric, Scope: scope, Fixture: fixture, Value: &value, N: n, Detail: detail}
}

// NotMeasured builds a row whose value is absent, with the reason in Detail. It exists so that
// "we did not measure this" is published as loudly as a number, rather than by omission.
func NotMeasured(metric string, scope Scope, fixture, reason string) Row {
	return Row{Metric: metric, Scope: scope, Fixture: fixture, Value: nil, N: 0, Detail: reason}
}

// Measured reports whether the row carries a value.
func (r Row) Measured() bool { return r.Value != nil }

// Float returns the row's value, and 0 with false when it was not measured.
func (r Row) Float() (float64, bool) {
	if r.Value == nil {
		return 0, false
	}
	return *r.Value, true
}

// String renders the row the way the human summary prints it.
func (r Row) String() string {
	scope := string(r.Scope)
	if r.Fixture != "" {
		scope = r.Fixture
	}
	value := "n/a"
	if r.Value != nil {
		value = fmt.Sprintf("%.4f", *r.Value)
	}
	out := fmt.Sprintf("%s [%s] = %s (n=%d)", r.Metric, scope, value, r.N)
	if r.Detail != "" {
		out += " — " + r.Detail
	}
	return out
}

// JSONL renders rows as the one-document-per-line stream `--report-json` writes.
func JSONL(rows []Row) ([]byte, error) {
	var out strings.Builder
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("eval: encode row %s: %w", row.Metric, err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return []byte(out.String()), nil
}

// ---------- the arithmetic ----------

// ReciprocalRank is 1/rank for a 1-based rank, and 0 when the culprit was not ranked at all.
//
// Zero for "not ranked" is the standard convention and it is the honest one here: a ranker that
// never surfaced the culprit contributed nothing to the mean, which is exactly what a reciprocal
// rank of an infinite rank would be.
func ReciprocalRank(rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1 / float64(rank)
}

// MeanReciprocalRank averages ReciprocalRank over the ranks given. An empty set has no mean and
// says so.
func MeanReciprocalRank(ranks []int) (float64, bool) {
	if len(ranks) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, rank := range ranks {
		sum += ReciprocalRank(rank)
	}
	return sum / float64(len(ranks)), true
}

// Lift is the investigator's MRR minus the deterministic ranker's over the same corpus and the
// same runs (FR-061a). It is the project's honest floor: a reasoning layer that does not beat
// the ranking a `diff` query already produces is not worth its cost.
func Lift(investigator, prior float64) float64 { return investigator - prior }

// PassAt1 is the mean outcome over runs — the headline, because production gets one run
// (FR-061).
func PassAt1(passes []bool) (float64, bool) {
	if len(passes) == 0 {
		return 0, false
	}
	won := 0
	for _, pass := range passes {
		if pass {
			won++
		}
	}
	return float64(won) / float64(len(passes)), true
}

// PassHatK is 1 when every one of the k runs passed and 0 otherwise — the reliability figure
// published beside pass@1 (FR-061). It is not a gate and it is not best-of-k, which must never
// be gated on at all.
func PassHatK(passes []bool) (float64, bool) {
	if len(passes) == 0 {
		return 0, false
	}
	for _, pass := range passes {
		if !pass {
			return 0, true
		}
	}
	return 1, true
}

// CausalCredit is localisation, attribution and mechanism scored separately, each in [0,1]
// (FR-059, FR-061b).
//
// Three scores rather than one because they fail independently and the failures mean different
// things: an investigation that localised the symptom but blamed the wrong change is a different
// animal from one that named the right change by the wrong mechanism, and a single blended score
// would let either hide inside the other.
type CausalCredit struct {
	// Localisation is whether the symptom end of the ground-truth causal path was identified.
	Localisation float64 `json:"localisation"`
	// Attribution is whether the cause end was identified.
	Attribution float64 `json:"attribution"`
	// Mechanism is the share of the path's `via` edges the answer reproduced, in order.
	Mechanism float64 `json:"mechanism"`
}

// Credit scores one answer's causal path against the ground truth's, with partial credit.
//
// `truthPath` and `gotPath` are the entity chains, and `truthVia` and `gotVia` the mechanism
// edges between them (len(via) == len(path)-1 when the path is well formed). Scoring is:
//
//   - localisation — 1 when the answer's symptom entity (the last element) is the truth's;
//   - attribution — 1 when the answer's cause entity (the first element) is the truth's;
//   - mechanism — the fraction of the truth's `via` edges that appear in the answer's, in the
//     same order, which is the partial credit FR-061b asks for: naming two of three hops right
//     is not the same as naming none.
//
// An empty ground-truth path scores nothing and is reported as unmeasured by the caller rather
// than as zero.
func Credit(truthPath, gotPath, truthVia, gotVia []string) CausalCredit {
	credit := CausalCredit{}
	if len(truthPath) == 0 {
		return credit
	}
	if len(gotPath) > 0 && gotPath[len(gotPath)-1] == truthPath[len(truthPath)-1] {
		credit.Localisation = 1
	}
	if len(gotPath) > 0 && gotPath[0] == truthPath[0] {
		credit.Attribution = 1
	}
	if len(truthVia) == 0 {
		return credit
	}
	credit.Mechanism = orderedOverlap(truthVia, gotVia)
	return credit
}

// orderedOverlap is the length of the longest common subsequence over the length of want. It is
// the partial-credit rule: hops named in the right order count, hops invented do not subtract
// (they show up in attribution instead), and a path that got two of three edges right scores
// 0.67 rather than 0.
func orderedOverlap(want, got []string) float64 {
	if len(want) == 0 {
		return 0
	}
	table := make([][]int, len(want)+1)
	for i := range table {
		table[i] = make([]int, len(got)+1)
	}
	for i := 1; i <= len(want); i++ {
		for j := 1; j <= len(got); j++ {
			if want[i-1] == got[j-1] {
				table[i][j] = table[i-1][j-1] + 1
				continue
			}
			table[i][j] = max(table[i-1][j], table[i][j-1])
		}
	}
	return float64(table[len(want)][len(got)]) / float64(len(want))
}

// Rate is hits/total, and false when total is 0. It is the one place the report decides that an
// empty denominator is not a zero rate.
func Rate(hits, total int) (float64, bool) {
	if total <= 0 {
		return 0, false
	}
	return float64(hits) / float64(total), true
}

// Mean averages a sample, and is false on an empty one.
func Mean(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values)), true
}

// ---------- detection power ----------

// The method, stated once and implemented once (FR-061, T112).
//
// Every run of the corpus is a set of Bernoulli trials: n = fixtures × runs, each either a pass
// or a failure. A threshold is only a gate if it says what regression it can actually catch at
// that n, and the published sentence — "8 fixtures × 5 runs = 40 Bernoulli trials detects
// 90 % → 70 % reliably and cannot detect 90 % → 80 %" — is the output of exactly this formula,
// not a slogan bolted on beside it.
//
// The test is the normal approximation to the binomial, one-sided, α = 0.05, power 0.8:
//
//	n ≥ ( z_α·√(p₀(1−p₀)) + z_β·√(p₁(1−p₁)) )² / (p₀ − p₁)²
//
// The approximation is stated rather than hidden. At these n and these p it is within a trial or
// two of the exact binomial, and being exact would not change a single verdict the gate prints;
// what would change a verdict is pretending a 40-trial corpus can see a ten-point regression.

const (
	// PowerAlpha is the one-sided significance level the detection-power statement uses.
	PowerAlpha = 0.05
	// PowerTarget is the power the statement is made at.
	PowerTarget = 0.8
	// zAlpha is Φ⁻¹(1 − PowerAlpha).
	zAlpha = 1.6448536269514722
	// zBeta is Φ⁻¹(PowerTarget).
	zBeta = 0.8416212335729143
)

// RequiredTrials is the n the one-sided normal-approximation test needs to distinguish a true
// rate of p1 from a baseline of p0 at α = 0.05 with power 0.8.
//
// It is symmetric in the direction of the difference, because both directions are real gates: a
// pass@1 gate fails when the rate DROPS and a harm-rate gate fails when it RISES, and the same
// test answers both. It returns +Inf when p1 equals p0: there is no sample size at which "no
// change at all" is detectable, and returning a large finite number would invite somebody to go
// and collect it.
func RequiredTrials(p0, p1 float64) float64 {
	if p1 == p0 {
		return math.Inf(1)
	}
	numerator := zAlpha*math.Sqrt(p0*(1-p0)) + zBeta*math.Sqrt(p1*(1-p1))
	return (numerator * numerator) / ((p0 - p1) * (p0 - p1))
}

// The two directions a gate can be regressed in. A `min` gate — pass@1, citation validity —
// fails when the rate falls; a `max` gate — harm rate, confidently wrong — fails when it rises.
// A detection-power statement that assumed the first would say "no regression is detectable" of
// every threshold of the second kind, which is both wrong and demoralising.
const (
	// DirectionMin is a gate whose regression is a fall.
	DirectionMin = "min"
	// DirectionMax is a gate whose regression is a rise.
	DirectionMax = "max"
)

// Power is what a gate at this n can see.
type Power struct {
	// N is the number of Bernoulli trials — fixtures × runs.
	N int `json:"n"`
	// Baseline is the rate the gate is set at.
	Baseline float64 `json:"baseline"`
	// Alpha and Target are the test's published parameters.
	Alpha  float64 `json:"alpha"`
	Target float64 `json:"power"`
	// Direction is DirectionMin or DirectionMax: which way a regression moves this gate.
	Direction string `json:"direction"`
	// Detectable is the nearest true rate on the regressing side of Baseline that this n can
	// distinguish from it. A regression that stops short of it is invisible to the gate, and
	// saying so is the whole point of publishing it.
	Detectable float64 `json:"detectable_at"`
	// Method names the test, so a number is never published without the assumption behind it.
	Method string `json:"method"`
}

// DetectionPower computes what a gate of n trials at this baseline can detect when a regression
// is a FALL — pass@1, citation validity, lift.
func DetectionPower(n int, baseline float64) Power {
	return DetectionPowerFor(n, baseline, DirectionMin)
}

// DetectionPowerFor computes the same statement for either direction.
func DetectionPowerFor(n int, baseline float64, direction string) Power {
	if direction != DirectionMax {
		direction = DirectionMin
	}
	power := Power{
		N:         n,
		Baseline:  baseline,
		Alpha:     PowerAlpha,
		Target:    PowerTarget,
		Direction: direction,
		Method: fmt.Sprintf("normal approximation to the binomial, one-sided, α = %.2f, power = %.1f",
			PowerAlpha, PowerTarget),
	}
	if n <= 0 || baseline < 0 || baseline > 1 {
		power.Detectable = math.NaN()
		return power
	}
	// `far` is the most extreme regression there is, and `near` the baseline itself. Bisect for
	// the nearest point to the baseline that is still detectable: `far` is always detectable and
	// `near` never is, so the invariant holds at every step and 60 halvings put the answer well
	// inside printing precision.
	far := 0.0
	if direction == DirectionMax {
		far = 1.0
	}
	near := baseline
	if RequiredTrials(baseline, far) > float64(n) {
		// Not even a total collapse — or a total failure — is detectable at this n.
		power.Detectable = math.NaN()
		return power
	}
	for range 60 {
		mid := (far + near) / 2
		if RequiredTrials(baseline, mid) <= float64(n) {
			far = mid
			continue
		}
		near = mid
	}
	power.Detectable = far
	return power
}

// ZeroToleranceDetectable is the detection power of a gate whose threshold is zero — "any
// untraceable conclusion fails", "any improvised replay fails", "any metamorphic verdict change
// fails".
//
// Such a gate needs no significance test: it fires on the first occurrence. What it can miss is
// a defect that only happens sometimes, and the honest statement is the complement. A defect
// occurring independently with probability p survives n trials unseen with probability (1−p)ⁿ,
// so the gate catches it with 95 % confidence exactly when
//
//	p ≥ 1 − 0.05^(1/n)
//
// At n = 39 that is 7.4 %: a defect that shows up in one run in twenty will usually be caught,
// and one that shows up in one run in a hundred will usually not.
func ZeroToleranceDetectable(n int) float64 {
	if n <= 0 {
		return math.NaN()
	}
	return 1 - math.Pow(PowerAlpha, 1/float64(n))
}

// ZeroToleranceSentence is the line printed beside a zero-threshold gate.
func ZeroToleranceSentence(n int) string {
	p := ZeroToleranceDetectable(n)
	if math.IsNaN(p) {
		return "no trials: this gate cannot detect anything"
	}
	return fmt.Sprintf(
		"fires on the first occurrence; over n = %d trials it catches an intermittent defect with "+
			"95 %% confidence when that defect occurs in ≥ %.1f %% of runs (1 − α^(1/n), α = %.2f)",
		n, p*100, PowerAlpha)
}

// Sentence is the line printed beside a threshold. It never prints a number without the
// regression that number can and cannot see.
func (p Power) Sentence() string {
	if math.IsNaN(p.Detectable) {
		return fmt.Sprintf(
			"n = %d Bernoulli trials at a %.1f %% baseline detects NO regression at α = %.2f with power %.1f "+
				"(%s); grow the corpus before reading this gate as evidence",
			p.N, p.Baseline*100, p.Alpha, p.Target, p.Method)
	}
	if p.Direction == DirectionMax {
		return fmt.Sprintf(
			"n = %d Bernoulli trials detects a rise from %.1f %% to %.1f %% or above; a regression that "+
				"stops below %.1f %% is invisible at this n (%s)",
			p.N, p.Baseline*100, p.Detectable*100, p.Detectable*100, p.Method)
	}
	return fmt.Sprintf(
		"n = %d Bernoulli trials detects a drop from %.0f %% to %.1f %% or below; a regression to "+
			"anything above %.1f %% is invisible at this n (%s)",
		p.N, p.Baseline*100, p.Detectable*100, p.Detectable*100, p.Method)
}

// Example renders the "detects x → y, cannot detect x → z" form for two named alternatives,
// which is the sentence FR-061 and T112 publish verbatim.
func (p Power) Example(detectable, undetectable float64) string {
	verb := func(p1 float64) string {
		need := RequiredTrials(p.Baseline, p1)
		if need <= float64(p.N) {
			return fmt.Sprintf("detects %.0f %% → %.0f %% reliably (needs n ≥ %.0f)",
				p.Baseline*100, p1*100, math.Ceil(need))
		}
		return fmt.Sprintf("cannot detect %.0f %% → %.0f %% (needs n ≥ %.0f)",
			p.Baseline*100, p1*100, math.Ceil(need))
	}
	return fmt.Sprintf("%d Bernoulli trials %s and %s", p.N, verb(detectable), verb(undetectable))
}

// ---------- small shared helpers ----------

// sortedKeys returns a map's keys in a stable order, so a report of the same run is the same
// bytes twice.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// percent renders a rate for a human summary, and `n/a` for one that was not measured.
func percent(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f %%", *value*100)
}
