// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"sync"
	"time"
)

// Observed clock skew between a platform's timestamps and this graph's observed time
// (003 FR-153, 004 FR-058, T045).
//
// The whole of this file's contract is one word in both requirements: skew is **reported** and
// **never used to correct** either clock.
//
// ---------------------------------------------------------------------------------------------
// Why it is here rather than in one connector, and what that move found
//
// It arrived as `internal/gcpx/skew.go`, for GCP. Nothing in it is about GCP: it observes a (vendor
// instant, local instant) pair, keeps the distribution, and publishes it. Feature 004 needs exactly
// the same thing for two more platforms, so it is here — one implementation rather than three copies
// of the same eighty lines, which is where the next fix would have gone into one of them.
//
// Moving it surfaced something worth stating plainly: **nothing ever called it.** `Observe` had no
// caller anywhere in the repository, `Report` had none, `Instruments.Skew` was never invoked, and no
// test touched any of it. So 003's FR-153 — "the skew MUST be reported" — was machinery that existed
// and never ran, which is the third time this shape has turned up here after C4, C5 and C7: published,
// wired into an interface, and silently never reached. This file now has tests, and the wiring into a
// feeder's cycle is tracked rather than assumed.
//
// Why a correction would be worse than the skew. This graph is bitemporal, and its two time
// dimensions mean different things: valid time is when a fact was true in the world, observed time
// is when this system learned it. A vendor timestamp is evidence about the first; the log's append
// instant is the second. Silently shifting either to make them agree would destroy the one property
// that makes the pair worth having — that a reader can tell "it happened at 14:21" from "we found
// out at 14:38" — and it would do so invisibly, producing a graph that is subtly wrong rather than
// one that is visibly skewed.
//
// So this measures, publishes the measurement as operational telemetry, and stops. An operator with
// a two-minute skew gets to see a two-minute skew.

// DefaultSkewThreshold is the stated threshold FR-058 asks for: beyond this, the skew is worth an
// operator's attention rather than merely worth recording.
//
// One minute, and the number is a judgement rather than a measurement: below it, a platform's
// timestamp and a poll's arrival differ by less than the poll intervals this connector runs at, so the
// difference explains nothing an operator could act on. Above it, a rollout's valid time and the
// instant the graph learned of it are far enough apart to change which change a diff window catches,
// which is exactly when somebody needs told.
const DefaultSkewThreshold = time.Minute

// Skew is the running observation for one source of vendor timestamps.
type Skew struct {
	// Threshold is what counts as worth reporting. Zero means DefaultSkewThreshold; a negative
	// threshold would make every sample exceed it, so it is treated as the default too rather than
	// silently turning the report into noise.
	Threshold time.Duration

	mu      sync.Mutex
	samples int
	total   time.Duration
	min     time.Duration
	max     time.Duration
	last    time.Duration
	lastAt  time.Time
	// exceeded counts the samples beyond the threshold, so a report can say how often rather than only
	// how far. A single outlier and a persistently skewed clock are different problems.
	exceeded int
}

// Observe records one (vendor instant, local instant) pair.
//
// Positive skew means the vendor's clock reads AHEAD of this process's. Both directions are kept
// rather than an absolute value: a vendor running ahead and one running behind cause different
// symptoms — the first makes facts appear to arrive from the future, the second makes a poll look
// like it missed something — and an absolute value would hide which one is happening.
func (s *Skew) Observe(vendorAt, localAt time.Time) {
	if s == nil || vendorAt.IsZero() || localAt.IsZero() {
		return
	}
	d := vendorAt.Sub(localAt)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.samples == 0 || d < s.min {
		s.min = d
	}
	if s.samples == 0 || d > s.max {
		s.max = d
	}
	s.samples++
	s.total += d
	s.last = d
	s.lastAt = localAt.UTC()
	if abs(d) > s.threshold() {
		s.exceeded++
	}
}

// threshold resolves the configured value, treating zero and anything negative as the default.
func (s *Skew) threshold() time.Duration {
	if s.Threshold <= 0 {
		return DefaultSkewThreshold
	}
	return s.Threshold
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// SkewReport is what reaches operational telemetry and the checkpoint.
type SkewReport struct {
	Samples int
	Mean    time.Duration
	Min     time.Duration
	Max     time.Duration
	Last    time.Duration
	LastAt  time.Time
	// Threshold is what was in force, carried so a report is readable without the configuration that
	// produced it.
	Threshold time.Duration
	// Exceeded is how many samples were beyond it. Reported alongside Samples rather than as a rate,
	// because one outlier in a thousand and a thousand in a thousand are different problems and a
	// percentage hides which.
	Exceeded int
}

// Beyond reports whether the skew is past the threshold often enough to be worth an operator's
// attention. It is a question about the observation and not a state of the connector: nothing is
// corrected either way (FR-058).
func (r SkewReport) Beyond() bool { return r.Exceeded > 0 }

// Report returns the current observation. Mean is zero when nothing has been observed, which is
// distinguishable from a measured zero by Samples — a distinction that matters because "the clocks
// agree" and "we never looked" are different claims.
func (s *Skew) Report() SkewReport {
	if s == nil {
		return SkewReport{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := SkewReport{
		Samples: s.samples, Min: s.min, Max: s.max, Last: s.last, LastAt: s.lastAt,
		Threshold: s.threshold(), Exceeded: s.exceeded,
	}
	if s.samples > 0 {
		rep.Mean = s.total / time.Duration(s.samples)
	}
	return rep
}
