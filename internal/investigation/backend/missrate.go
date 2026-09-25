// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Miss-rate accounting (tasks.md T040; FR-042c, SC-018; contracts/incident-format.md
// §"Grading rules that are easy to get wrong").
//
// The miss rate is **in-algebra requests answered NOT_RECORDED, over in-algebra requests
// served**. It is published on every verification run, per fixture and per corpus, and it gates
// **a fixture's admission to the corpus — never the engine's score**.
//
// That distinction is the whole task. A world that does not hold the answer to a reasonable
// question has told us something about the *recording*: the cross product was too narrow, the
// window grid too coarse, the hop radius too small. Scoring the engine down for it would punish
// the engine for the recorder's omission and, worse, would reward an engine that asks fewer
// questions. So a fixture above the threshold is reported **insufficient** and is not scored at
// all, which is a louder signal than a low score and one that names its own remedy: re-record
// the world.
//
// Requests outside the algebra are counted separately and are not misses. They are a defect in
// the caller, not a gap in the recording, and folding them in would let a buggy engine make a
// good fixture look insufficient.

// MissRateThreshold is the published admission threshold: a fixture whose miss rate exceeds it
// is reported as insufficient rather than silently scored. It is one of the numbers carried as
// an initial value and marked for calibration once the corpus exists (research §20).
const MissRateThreshold = 0.05

// Miss is one in-algebra term a world did not hold.
type Miss struct {
	// TermKey is the key that missed, so a reviewer can look for it in world/index.json.
	TermKey string `json:"term_key"`
	// Term is the published term name, which is what says whether the gap is in the window
	// grid, the neighbourhood or the drill-down depth.
	Term string `json:"term"`
}

// MissReport is one backend's accounting over one run.
type MissReport struct {
	// WorldDir is the world these numbers are about.
	WorldDir string `json:"world_dir"`
	// WorldSize is how many answers the world holds.
	WorldSize int `json:"world_size"`
	// Served is in-algebra requests answered from the world.
	Served int `json:"served"`
	// Missed is the distinct in-algebra terms the world did not hold.
	Missed []Miss `json:"missed,omitempty"`
	// Refused is requests outside the algebra. They are not misses: they are a defect in the
	// caller, and counting them as misses would let a buggy engine condemn a good fixture.
	Refused int `json:"refused"`
}

// NotRecorded is how many distinct in-algebra terms missed.
func (r MissReport) NotRecorded() int { return len(r.Missed) }

// InAlgebra is the denominator: everything the world was asked that the algebra publishes.
func (r MissReport) InAlgebra() int { return r.Served + r.NotRecorded() }

// MissRate is NOT_RECORDED over in-algebra requests, rounded to six decimals — the same
// rounding discipline the ranking formula and the posterior rule use, for the same reason: a
// number that is compared byte for byte must be produced digit for digit.
//
// A run that asked nothing has a miss rate of 0: there is no evidence of a gap, and inventing
// one would report every unexercised fixture as insufficient.
func (r MissReport) MissRate() float64 {
	total := r.InAlgebra()
	if total == 0 {
		return 0
	}
	return round6(float64(r.NotRecorded()) / float64(total))
}

// Sufficient reports whether the fixture is admissible to the corpus at the given threshold. A
// zero or negative threshold means the published one.
func (r MissReport) Sufficient(threshold float64) bool {
	if threshold <= 0 {
		threshold = MissRateThreshold
	}
	return r.MissRate() <= threshold
}

// Verdict is the published sentence for a report: what the miss rate was, what the threshold
// was, and — when the fixture is insufficient — which terms to widen the recording for, rather
// than a score.
func (r MissReport) Verdict(fixtureID string, threshold float64) string {
	if threshold <= 0 {
		threshold = MissRateThreshold
	}
	rate := r.MissRate()
	if r.Sufficient(threshold) {
		return fmt.Sprintf("%s: miss rate %.6f of %d in-algebra requests (threshold %.6f) — admitted",
			fixtureID, rate, r.InAlgebra(), threshold)
	}
	return fmt.Sprintf(
		"%s: miss rate %.6f of %d in-algebra requests exceeds the threshold %.6f — INSUFFICIENT, not scored. "+
			"%d term(s) were asked and not held (%s); re-record the world with `fixture record-world` after widening the fixture's window grid, hop radius or drill-down depth. "+
			"This gates the fixture, never the engine.",
		fixtureID, rate, r.InAlgebra(), threshold, r.NotRecorded(), strings.Join(r.MissedTerms(), ", "))
}

// MissedTerms lists the distinct term names that missed, sorted and de-duplicated, because
// "compare missed 40 times" and "compare missed once" call for the same remedy.
func (r MissReport) MissedTerms() []string {
	seen := make(map[string]struct{}, len(r.Missed))
	out := make([]string, 0, len(r.Missed))
	for _, miss := range r.Missed {
		if _, dup := seen[miss.Term]; dup {
			continue
		}
		seen[miss.Term] = struct{}{}
		out = append(out, miss.Term)
	}
	sort.Strings(out)
	return out
}

// MissCountsByTerm is how many times each published term missed, which is the number that says
// WHERE the recording is thin.
//
// The distinction from MissedTerms is the point of having both. "compare missed" tells a reviewer
// which remedy to reach for; "compare missed 40 times and drill_down once" tells them whether the
// window grid is one notch too coarse or the whole neighbourhood is wrong. A re-record is
// expensive enough that the difference is worth publishing.
func (r MissReport) MissCountsByTerm() map[string]int {
	out := make(map[string]int, len(r.Missed))
	for _, miss := range r.Missed {
		out[miss.Term]++
	}
	return out
}

// TermMissRate is one published term's miss accounting across a corpus.
type TermMissRate struct {
	// Term is the published term name.
	Term string `json:"term"`
	// Missed is how many in-algebra requests for it were answered NOT_RECORDED.
	Missed int `json:"missed"`
}

// CorpusMissReport aggregates per-fixture reports into the corpus number published on every
// verification run.
type CorpusMissReport struct {
	// Fixtures maps a fixture id to its report.
	Fixtures map[string]MissReport `json:"fixtures"`
	// Threshold is the admission threshold in force.
	Threshold float64 `json:"threshold"`
}

// NewCorpusMissReport returns an empty aggregate at the published threshold.
func NewCorpusMissReport(threshold float64) *CorpusMissReport {
	if threshold <= 0 {
		threshold = MissRateThreshold
	}
	return &CorpusMissReport{Fixtures: make(map[string]MissReport), Threshold: threshold}
}

// Add records one fixture's report.
func (c *CorpusMissReport) Add(fixtureID string, report MissReport) {
	c.Fixtures[fixtureID] = report
}

// MissRate is the corpus rate: misses over in-algebra requests across every fixture, not the
// mean of the per-fixture rates. A corpus of one busy fixture and nine quiet ones should not
// hide the busy one's gaps behind nine zeros.
func (c *CorpusMissReport) MissRate() float64 {
	var missed, total int
	for _, report := range c.Fixtures {
		missed += report.NotRecorded()
		total += report.InAlgebra()
	}
	if total == 0 {
		return 0
	}
	return round6(float64(missed) / float64(total))
}

// Insufficient lists the fixtures above the threshold, sorted. They are reported rather than
// scored (SC-018).
func (c *CorpusMissReport) Insufficient() []string {
	out := make([]string, 0)
	for id, report := range c.Fixtures {
		if !report.Sufficient(c.Threshold) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ByTerm is the corpus miss count per published term, sorted worst first and then by name.
//
// It is reported beside the corpus rate rather than instead of it, because the two answer
// different questions: the rate says whether the corpus is admissible, and this says which part
// of the cross product to widen when it is not.
func (c *CorpusMissReport) ByTerm() []TermMissRate {
	counts := map[string]int{}
	for _, report := range c.Fixtures {
		for term, n := range report.MissCountsByTerm() {
			counts[term] += n
		}
	}
	out := make([]TermMissRate, 0, len(counts))
	for term, missed := range counts {
		out = append(out, TermMissRate{Term: term, Missed: missed})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Missed != out[j].Missed {
			return out[i].Missed > out[j].Missed
		}
		return out[i].Term < out[j].Term
	})
	return out
}

// Sufficient reports whether the corpus as a whole is inside the threshold. A corpus can sit
// inside it while individual fixtures do not, which is why both are published: the per-fixture
// number decides admission, and this one says whether the corpus is worth admitting from.
func (c *CorpusMissReport) Sufficient() bool { return c.MissRate() <= c.Threshold }

// Markdown is the report row published on every verification run.
func (c *CorpusMissReport) Markdown() string {
	var b strings.Builder
	b.WriteString("| fixture | in-algebra requests | not_recorded | miss rate | admitted |\n")
	b.WriteString("|---|---:|---:|---:|---|\n")
	ids := make([]string, 0, len(c.Fixtures))
	for id := range c.Fixtures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		report := c.Fixtures[id]
		admitted := "yes"
		if !report.Sufficient(c.Threshold) {
			admitted = "**no — insufficient, not scored**"
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %.6f | %s |\n",
			id, report.InAlgebra(), report.NotRecorded(), report.MissRate(), admitted)
	}
	fmt.Fprintf(&b, "| **corpus** | | | %.6f | threshold %.6f |\n", c.MissRate(), c.Threshold)

	// Which part of the cross product is thin, where anything missed at all. A corpus with no
	// misses says so rather than printing an empty table, because an empty table reads as a
	// report that did not run.
	byTerm := c.ByTerm()
	if len(byTerm) == 0 {
		b.WriteString("\nno term missed: every in-algebra question was held by the world it was asked of.\n")
		return b.String()
	}
	b.WriteString("\n| term | not_recorded |\n|---|---:|\n")
	for _, row := range byTerm {
		fmt.Fprintf(&b, "| %s | %d |\n", row.Term, row.Missed)
	}
	return b.String()
}

func sortMisses(misses []Miss) {
	sort.Slice(misses, func(i, j int) bool {
		if misses[i].Term != misses[j].Term {
			return misses[i].Term < misses[j].Term
		}
		return misses[i].TermKey < misses[j].TermKey
	})
}

// round6 rounds to six decimals, the project's published rounding for every number that is
// compared byte for byte.
func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}
