// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"maps"
	"slices"
	"sync"
)

// What a connector reports about its own cycle, per platform area (004 FR-009, T046).
//
// ---------------------------------------------------------------------------------------------
// Why the area is the key, and why exclusions carry a reason
//
// The area is already the unit an operator approves: it is the first column of the published
// operation surface and the grouping a scope is granted in. Reporting per area means the report and
// the page a reader approved are indexed the same way, so "we made 412 calls" is answerable as "412
// against deployments, 19 against workflow runs" without anybody joining two vocabularies.
//
// Exclusions are counted **by reason** rather than as a total, and that is the part worth arguing
// for. FR-019 requires a run, deployment or release that changes nothing to produce no change node
// **and the filter that excluded it to be named**. A bare count cannot distinguish "the environment
// filter excluded 340 preview deployments, as configured" from "the terminal-state filter excluded
// 340 deployments because we misread the status vocabulary" — and the second is a connector that has
// gone silent while reporting a healthy number. The reason is what makes the count readable.
//
// Deferrals are separate from exclusions for the same class of reason: a deferral is work this
// connector chose not to do **yet**, and an exclusion is work it decided was not a change. Adding
// them together would report a connector that ran out of budget as one that found nothing, which is
// exactly the conflation FR-073 exists to prevent at the stop-reason level.

// AreaStats accumulates one cycle's operational facts, per area. The zero value is ready to use and
// safe for concurrent use: a connector polling several areas shares one.
type AreaStats struct {
	mu    sync.Mutex
	areas map[string]*areaCounters
}

type areaCounters struct {
	calls      int
	refusals   int
	emitted    int
	rejected   int
	deferred   int
	exclusions map[string]int
}

// Call records one platform call and whether it was made.
//
// A refused call is recorded rather than dropped, because the artifact an operator reads has to show
// an ATTEMPT: a report that counted only calls that went out would look identical whether the budget
// was comfortable or the connector spent the cycle being refused.
func (s *AreaStats) Call(area string, made bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.countersLocked(area)
	if c == nil {
		return
	}
	if made {
		c.calls++
		return
	}
	c.refusals++
}

// Excluded records one thing the connector decided was not a change, naming the filter that decided
// it (FR-019).
//
// An empty reason is recorded as `unnamed` rather than dropped or silently counted: an exclusion
// nobody can attribute is the exact case this counter exists to make visible, and hiding it would
// leave a connector's silence looking like a configured filter doing its job.
func (s *AreaStats) Excluded(area, reason string) {
	if reason == "" {
		reason = "unnamed"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.countersLocked(area); c != nil {
		c.exclusions[reason]++
	}
}

// Deferred records work this cycle chose not to do yet — a surface dropped for budget, a page not
// fetched. Never folded into exclusions: one is "not a change", the other is "not now".
func (s *AreaStats) Deferred(area string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.countersLocked(area); c != nil {
		c.deferred++
	}
}

// Events records what the cycle emitted and what the graph refused.
func (s *AreaStats) Events(area string, emitted, rejected int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.countersLocked(area); c != nil {
		c.emitted += emitted
		c.rejected += rejected
	}
}

// countersLocked returns the area's counters, creating them on first use. The caller holds s.mu —
// every recorder locks once and does its whole update inside, so there is no window between finding
// the counters and incrementing them, and no way to introduce a second acquisition later.
//
// An empty area name is refused rather than pooled under "": a count nobody can attribute to a
// surface is a count nobody can act on, and pooling would make it look attributed.
func (s *AreaStats) countersLocked(area string) *areaCounters {
	if area == "" {
		return nil
	}
	if s.areas == nil {
		s.areas = map[string]*areaCounters{}
	}
	c, ok := s.areas[area]
	if !ok {
		c = &areaCounters{exclusions: map[string]int{}}
		s.areas[area] = c
	}
	return c
}

// AreaReport is one area's line of the operational report.
type AreaReport struct {
	Area string
	// Calls were made; Refusals were not, because the budget stopped them.
	//
	// A call the published read-only surface refused is NOT here, and deliberately: an unpublished
	// operation carries no spec, so there is no area to attribute it to, and inventing one would put a
	// refusal under a heading the connector never polled. That attempt is in the issuer's request log
	// instead (issue.go, RequestRecord.Blocked), which is where a reader checks FR-004 anyway.
	Calls    int
	Refusals int
	Emitted  int
	Rejected int
	Deferred int
	// Exclusions are counted by the filter that decided, never totalled: a bare count cannot tell a
	// configured filter doing its job from a connector that has gone silent.
	Exclusions map[string]int
}

// Report renders the cycle, areas in name order so two runs of the same shape read the same.
func (s *AreaStats) Report() []AreaReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]AreaReport, 0, len(s.areas))
	for _, area := range slices.Sorted(maps.Keys(s.areas)) {
		c := s.areas[area]
		out = append(out, AreaReport{
			Area: area, Calls: c.calls, Refusals: c.refusals,
			Emitted: c.emitted, Rejected: c.rejected, Deferred: c.deferred,
			Exclusions: maps.Clone(c.exclusions),
		})
	}
	return out
}
