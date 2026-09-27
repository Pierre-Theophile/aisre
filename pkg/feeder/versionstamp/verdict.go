// SPDX-License-Identifier: Apache-2.0

package versionstamp

import (
	"fmt"
	"strings"
	"time"
)

// Deciding which attribute carries a service's version (005 FR-040c, T029; contract §3).
//
// The decision is a pure function of measurements a backend makes — how many of the service's lines,
// and of its error lines, carry each candidate over a window — so it is the same for every backend and
// testable without one. The backend measures; this decides.
//
// A candidate is accepted by SHARE, not by stability. A deployment stamp is on every line the service
// writes, while a field a library logs on a few lines of its own is not: 005's audit found an SDK
// version on 255 of 37.2 million lines, all start-up lines. Whether the value changed during the window
// is not a criterion, because a service that did not deploy that week has a constant, correct version.

// Published defaults (contract §3). They are configuration; the verdict records the values used.
const (
	DefaultLineShare      = 0.95
	DefaultErrorLineShare = 0.95
	DefaultWindow         = time.Hour
)

// Thresholds are the acceptance shares in force.
type Thresholds struct {
	LineShare      float64
	ErrorLineShare float64
}

// DefaultThresholds returns the published defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{LineShare: DefaultLineShare, ErrorLineShare: DefaultErrorLineShare}
}

// Measurement is one candidate's counts over the discovery window.
type Measurement struct {
	Candidate Candidate
	// Lines and ErrorLines are how many of the service's lines, and of its error-level lines, carry
	// the candidate.
	Lines, ErrorLines int64
}

// Totals are the service's own counts over the same window.
type Totals struct {
	Lines, ErrorLines int64
}

// Source says whether the accepted attribute was discovered or named by an operator.
type Source string

const (
	// SourceDiscovered is an attribute the share test chose from the convention list.
	SourceDiscovered Source = "discovered"
	// SourceOperator is a per-service override. It is subject to the same share test, so a typo
	// reads as "not found" rather than as a silent empty split.
	SourceOperator Source = "operator"
)

// CandidateResult is one candidate's line in the verdict.
type CandidateResult struct {
	Label          string
	LineShare      float64
	ErrorLineShare float64
	Accepted       bool
	Reason         string
}

// Verdict is the outcome of discovery, recorded on the pointer and in the checkpoint.
type Verdict struct {
	// Attribute is the accepted candidate's label, empty when none qualified.
	Attribute string
	// Accepted is the accepted candidate itself, meaningful when Attribute is set.
	Accepted           Candidate
	Source             Source
	Window             time.Duration
	Thresholds         Thresholds
	Candidates         []CandidateResult
	ConventionsVersion string
}

// Stamped reports whether a version attribute was accepted.
func (v Verdict) Stamped() bool { return v.Attribute != "" }

// Decide applies the share test to measurements in the order given — the convention list's order, or
// the override alone — and accepts the first that passes. Every candidate tried is in the verdict with
// its shares and a reason, so a reader can see why each one was, or was not, the version.
func Decide(measured []Measurement, totals Totals, th Thresholds, window time.Duration, source Source) Verdict {
	verdict := Verdict{
		Source: source, Window: window, Thresholds: th, ConventionsVersion: ConventionsVersion,
	}
	for _, m := range measured {
		result := CandidateResult{
			Label:          m.Candidate.Label(),
			LineShare:      share(m.Lines, totals.Lines),
			ErrorLineShare: share(m.ErrorLines, totals.ErrorLines),
		}
		switch {
		case verdict.Stamped():
			result.Reason = "not needed: " + verdict.Attribute + " was accepted first"
		case totals.Lines == 0:
			result.Reason = "the service wrote no lines in the window, so no share can be measured"
		case result.LineShare < th.LineShare:
			result.Reason = fmt.Sprintf("on %s of lines, below the %s line share", pct(result.LineShare), pct(th.LineShare))
		case totals.ErrorLines > 0 && result.ErrorLineShare < th.ErrorLineShare:
			result.Reason = fmt.Sprintf("on %s of lines but %s of error lines, below the %s error-line share; "+
				"errors split by it would fall mostly into an unlabelled group",
				pct(result.LineShare), pct(result.ErrorLineShare), pct(th.ErrorLineShare))
		default:
			result.Accepted = true
			result.Reason = fmt.Sprintf("on %s of lines and %s of error lines", pct(result.LineShare), errorShareText(result, totals))
			verdict.Attribute, verdict.Accepted = result.Label, m.Candidate
		}
		verdict.Candidates = append(verdict.Candidates, result)
	}
	return verdict
}

// String renders the verdict deterministically, for the checkpoint note and the engine's NO_DATA.
func (v Verdict) String() string {
	parts := make([]string, 0, len(v.Candidates))
	for _, c := range v.Candidates {
		state := "rejected"
		if c.Accepted {
			state = "accepted"
		}
		parts = append(parts, fmt.Sprintf("%s: %s, %s", c.Label, state, c.Reason))
	}
	head := "no version stamp"
	if v.Stamped() {
		head = "version stamp " + v.Attribute + " (" + string(v.Source) + ")"
	}
	return fmt.Sprintf("%s; conventions %s over %s: %s", head, v.ConventionsVersion, v.Window, strings.Join(parts, "; "))
}

func share(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(n) / float64(total)
}

func pct(f float64) string {
	s := fmt.Sprintf("%.4f", f*100)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

func errorShareText(r CandidateResult, totals Totals) string {
	if totals.ErrorLines == 0 {
		return "no error lines in the window (the error-line criterion could not bind)"
	}
	return pct(r.ErrorLineShare)
}
