// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// The run policy (T111, FR-061).
//
// Three sentences from FR-061, and each one is a line of code below rather than a comment:
//
//  1. **Each fixture runs 3 times, extended to 7 only when the first 3 disagree.** Not "runs
//     until it agrees", which would be sampling until the answer is convenient, and not "always
//     7", which would triple the cost of a corpus that mostly agrees with itself. Disagreement is
//     the signal that three samples are not enough to describe this fixture.
//  2. **pass@1 is the headline, and it is the *first* run**, because production gets one run.
//     Not the best run, not the mean of the best three: the first.
//  3. **Majority-of-three is never the gate.** A fixture solved 80 % of the time passes a
//     majority-of-three about 90 % of the time, and eight such fixtures would let a green build
//     mean nothing. The per-run outcomes are therefore all published, and nothing in this file
//     reduces them to one boolean.
//
// The model configuration is recorded on **every** outcome rather than once per set, because the
// set is what gets aggregated and a row whose configuration had to be looked up elsewhere is a
// row that will one day be read next to the wrong configuration.

// DefaultRuns and DefaultExtendTo are FR-061's published numbers.
const (
	DefaultRuns     = 3
	DefaultExtendTo = 7
)

// RunPolicy is how many times a fixture is investigated.
type RunPolicy struct {
	// Runs is the first pass. Zero means DefaultRuns.
	Runs int `json:"runs"`
	// ExtendTo is the total the set is extended to when the first Runs verdicts disagree. Zero
	// means DefaultExtendTo; a value at or below Runs disables the extension, which is legal
	// and is what a caller that wants exactly k runs asks for.
	ExtendTo int `json:"extend_to"`
}

// DefaultRunPolicy is FR-061's policy: three runs, extended to seven on disagreement.
func DefaultRunPolicy() RunPolicy { return RunPolicy{Runs: DefaultRuns, ExtendTo: DefaultExtendTo} }

// normalised fills in the published defaults.
func (p RunPolicy) normalised() RunPolicy {
	if p.Runs <= 0 {
		p.Runs = DefaultRuns
	}
	if p.ExtendTo == 0 {
		p.ExtendTo = DefaultExtendTo
	}
	if p.ExtendTo < p.Runs {
		p.ExtendTo = p.Runs
	}
	return p
}

// RunSet is one fixture's runs under one policy.
type RunSet struct {
	// FixtureID is the incident, and Policy the policy that produced the set.
	FixtureID string    `json:"fixture_id"`
	Policy    RunPolicy `json:"policy"`
	// Outcomes are the runs in the order they were made. Every one of them is published:
	// nothing here collapses them into a verdict, because majority-of-three is not the gate.
	Outcomes []*RunOutcome `json:"outcomes"`
	// Disagreed says the first `Policy.Runs` verdicts were not all equal, and Extended that the
	// set was therefore extended.
	Disagreed bool `json:"disagreed"`
	Extended  bool `json:"extended"`
	// ModelConfigDigest is the configuration every run in the set used. A set whose runs
	// disagree about it is a set that must not be aggregated, so it is reported as empty and
	// ModelConfigMismatch says so.
	ModelConfigDigest   string `json:"model_config_digest"`
	ModelConfigMismatch bool   `json:"model_config_mismatch,omitempty"`
}

// RunFunc is one investigation. The index is 1-based, so a caller that wants to vary anything
// per run — a seed, a recording path — has the run number to do it with.
type RunFunc func(ctx context.Context, runIndex int) (*RunOutcome, error)

// Execute runs a fixture under the policy.
//
// The extension is decided **once**, on the first `Runs` verdicts, and never re-decided: a policy
// that kept extending while the answers disagreed would run until it was lucky.
func (p RunPolicy) Execute(ctx context.Context, fixtureID string, run RunFunc) (*RunSet, error) {
	policy := p.normalised()
	set := &RunSet{FixtureID: fixtureID, Policy: policy}
	for i := 1; i <= policy.Runs; i++ {
		outcome, err := run(ctx, i)
		if err != nil {
			return nil, err
		}
		outcome.RunIndex = i
		set.Outcomes = append(set.Outcomes, outcome)
	}
	set.Disagreed = !sameVerdict(set.Outcomes)
	if set.Disagreed && policy.ExtendTo > policy.Runs {
		set.Extended = true
		for i := policy.Runs + 1; i <= policy.ExtendTo; i++ {
			outcome, err := run(ctx, i)
			if err != nil {
				return nil, err
			}
			outcome.RunIndex = i
			set.Outcomes = append(set.Outcomes, outcome)
		}
	}
	set.recordConfiguration()
	return set, nil
}

// RunSetOf builds a set from outcomes that were already produced — an `eval.yml` leg that ran the
// corpus through the Batch API and is reading the answers back, or a test.
func RunSetOf(fixtureID string, policy RunPolicy, outcomes []*RunOutcome) *RunSet {
	normalised := policy.normalised()
	set := &RunSet{FixtureID: fixtureID, Policy: normalised, Outcomes: outcomes}
	first := outcomes
	if len(first) > normalised.Runs {
		first = first[:normalised.Runs]
	}
	set.Disagreed = !sameVerdict(first)
	set.Extended = len(outcomes) > normalised.Runs
	set.recordConfiguration()
	return set
}

// recordConfiguration reads the configuration the set ran under, refusing to state one when the
// runs do not agree.
func (s *RunSet) recordConfiguration() {
	for _, outcome := range s.Outcomes {
		switch {
		case s.ModelConfigDigest == "":
			s.ModelConfigDigest = outcome.ModelConfigDigest
		case s.ModelConfigDigest != outcome.ModelConfigDigest:
			s.ModelConfigMismatch = true
			s.ModelConfigDigest = ""
			return
		}
	}
}

// Verdicts are the per-run verdicts, in run order.
func (s *RunSet) Verdicts() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Outcomes))
	for _, outcome := range s.Outcomes {
		out = append(out, outcome.Verdict)
	}
	return out
}

// DistinctVerdicts are the verdicts the set produced, sorted, with duplicates removed. A set with
// more than one is a set whose fixture the engine does not answer reliably, which is the fact
// pass^k exists to publish.
func (s *RunSet) DistinctVerdicts() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, verdict := range s.Verdicts() {
		if _, dup := seen[verdict]; dup {
			continue
		}
		seen[verdict] = struct{}{}
		out = append(out, verdict)
	}
	sort.Strings(out)
	return out
}

// First is the run pass@1 is computed from: production gets one run, and this is it.
func (s *RunSet) First() *RunOutcome {
	if s == nil || len(s.Outcomes) == 0 {
		return nil
	}
	return s.Outcomes[0]
}

// Grade grades every run in the set against the ground truth, at the instant the question was
// asked. The per-run grades are returned in run order; nothing is reduced.
func (s *RunSet) Grade(truth GroundTruth, askedAt time.Time) []GradeResult {
	if s == nil {
		return nil
	}
	out := make([]GradeResult, 0, len(s.Outcomes))
	for _, outcome := range s.Outcomes {
		out = append(out, Grade(outcome, truth, askedAt))
	}
	return out
}

// String renders the set as the one line a job summary shows.
func (s *RunSet) String() string {
	if s == nil {
		return ""
	}
	note := ""
	if s.Extended {
		note = fmt.Sprintf(", extended to %d because the first %d disagreed", len(s.Outcomes), s.Policy.Runs)
	}
	return fmt.Sprintf("%s: %d run(s), verdict(s) %v%s", s.FixtureID, len(s.Outcomes), s.DistinctVerdicts(), note)
}

func sameVerdict(outcomes []*RunOutcome) bool {
	if len(outcomes) < 2 {
		return true
	}
	first := outcomes[0].Verdict
	for _, outcome := range outcomes[1:] {
		if outcome.Verdict != first {
			return false
		}
	}
	return true
}

// RunUnderPolicy investigates one fixture the published number of times through this harness.
//
// It is the ordinary way to use a harness: the harness is built once — one database, one replay,
// one recorded world — and the policy decides how many times the engine runs over it.
func (h *Harness) RunUnderPolicy(ctx context.Context, policy RunPolicy) (*RunSet, error) {
	return policy.Execute(ctx, h.manifest.ID, func(ctx context.Context, _ int) (*RunOutcome, error) {
		return h.Run(ctx)
	})
}
