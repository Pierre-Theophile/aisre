// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Extent consultation (T080, FR-032).
//
// Before relying on a window, ask the graph what it actually covers. A feeder that was down for
// eleven minutes in the middle of the look-back window does not make the window empty; it makes
// it *partly unknown*, and an investigation that cannot tell those apart will happily report
// "no change was deployed" about a window in which it could not have seen a deployment.
//
// So: consult `Extent`, intersect every source's gaps with the window, and carry what overlaps
// into three places —
//
//  1. the evidence log, as one `graph_answer` item citing the `graph.extent` term;
//  2. the affected hypotheses, whose bucket is widened (FR-045) because a test that ran over a
//     gap was cut short whether or not it looked like it;
//  3. the rendering, where the gap is named with its source and its interval.
//
// The rule for "affected" is deliberately coarse: any overlap at all between a gap and the
// window the evidence rests on. A finer rule — "the gap covers more than x% of the window" —
// would be a judgement about how much missing data matters, and that judgement belongs to the
// person reading the report, who can see the interval.

// EvidenceKindGraphAnswer is the published evidence kind of a graph answer.
const EvidenceKindGraphAnswer = "graph_answer"

// ExtentSource answers the graph's extent question (feature 001's QueryService.Extent).
type ExtentSource interface {
	Extent(ctx context.Context, req *graphv1.ExtentRequest) (*graphv1.Extent, error)
}

// Gap is one source gap overlapping the window the investigation reasons over.
type Gap struct {
	// SourceID is the feeder whose coverage is missing.
	SourceID string
	// Start and End bound the gap. End is zero when the gap is still open — the source has not
	// come back — which is a materially different statement from a closed gap.
	Start time.Time
	End   time.Time
	// EndOpen reports that the gap has no known end.
	EndOpen bool
	// OverlapStart and OverlapEnd are the part of the gap that falls inside the window.
	OverlapStart time.Time
	OverlapEnd   time.Time
	// OverlapSeconds is how much of the window the gap covers.
	OverlapSeconds float64
}

// String renders the gap for a report line.
func (g Gap) String() string {
	end := "open"
	if !g.EndOpen {
		end = g.End.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s: %s..%s (%.0fs inside the window)",
		g.SourceID, g.Start.UTC().Format(time.RFC3339), end, g.OverlapSeconds)
}

// ExtentReport is the result of consulting the extent for one window.
type ExtentReport struct {
	// Gaps are the source gaps overlapping the window, sorted by source then start.
	Gaps []Gap
	// EarliestObserved and LatestObserved bound what the graph knows at all.
	EarliestObserved time.Time
	LatestObserved   time.Time
	// BeforeHistory is true when the window starts before the graph's earliest observation: the
	// investigation is asking about a time the graph has no history for, which is not a gap in a
	// source but a limit of the whole substrate.
	BeforeHistory bool
	// Evidence is the consultation as an evidence item (FR-032).
	Evidence ledger.EvidenceItem
}

// GapAffected reports whether the window is affected by any known gap at all.
func (r *ExtentReport) GapAffected() bool {
	return r != nil && (len(r.Gaps) > 0 || r.BeforeHistory)
}

// Summary is the sentence the rendering carries when the window is gap-affected.
func (r *ExtentReport) Summary() string {
	if r == nil || !r.GapAffected() {
		return ""
	}
	var parts []string
	if r.BeforeHistory {
		parts = append(parts, fmt.Sprintf("the window starts before the graph's earliest "+
			"observation (%s)", r.EarliestObserved.UTC().Format(time.RFC3339)))
	}
	for _, g := range r.Gaps {
		parts = append(parts, g.String())
	}
	return "coverage gap in force over this window — " + strings.Join(parts, "; ")
}

// SourceIDs lists the sources with a gap over the window.
func (r *ExtentReport) SourceIDs() []string {
	if r == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, g := range r.Gaps {
		if _, ok := seen[g.SourceID]; ok {
			continue
		}
		seen[g.SourceID] = struct{}{}
		out = append(out, g.SourceID)
	}
	sort.Strings(out)
	return out
}

// ConsultExtent asks the graph what it covers over the investigation's window and flags every
// gap that overlaps it (FR-032).
//
// A nil ExtentSource is not an error: it produces a report whose evidence item records
// `query_failed`, which is one of the four answers FR-027 keeps distinct from "nothing happened".
func ConsultExtent(ctx context.Context, src ExtentSource, in *Intake) (*ExtentReport, error) {
	if in == nil || in.Window == nil {
		return nil, fmt.Errorf("consult extent: %w", ErrNoInstant)
	}
	windowStart := in.Window.GetStart().AsTime()
	windowEnd := in.Window.GetEnd().AsTime()

	report := &ExtentReport{}
	if src == nil {
		report.Evidence = extentEvidence(in, "query_failed",
			"no extent source configured; the window's coverage is unknown", nil)
		return report, nil
	}

	extent, err := src.Extent(ctx, &graphv1.ExtentRequest{})
	if err != nil {
		return nil, fmt.Errorf("consult extent: %w", err)
	}
	if ts := extent.GetEarliestObserved(); ts != nil {
		report.EarliestObserved = ts.AsTime().UTC()
		report.BeforeHistory = windowStart.Before(report.EarliestObserved)
	}
	if ts := extent.GetLatestObserved(); ts != nil {
		report.LatestObserved = ts.AsTime().UTC()
	}

	for _, source := range extent.GetSources() {
		for _, interval := range source.GetGaps() {
			gap, ok := overlap(source.GetSourceId(), interval, windowStart, windowEnd)
			if ok {
				report.Gaps = append(report.Gaps, gap)
			}
		}
	}
	sort.Slice(report.Gaps, func(i, j int) bool {
		if report.Gaps[i].SourceID != report.Gaps[j].SourceID {
			return report.Gaps[i].SourceID < report.Gaps[j].SourceID
		}
		return report.Gaps[i].Start.Before(report.Gaps[j].Start)
	})

	outcome, detail := "no_data", "no coverage gap overlaps this window"
	if report.GapAffected() {
		outcome, detail = "partial", report.Summary()
	}
	report.Evidence = extentEvidence(in, outcome, detail, report)
	return report, nil
}

// overlap intersects one gap interval with the window. An interval with no start is treated as
// unbounded below and one with no end as unbounded above, which is what `start_unknown` and
// `end_unknown` mean on the wire.
func overlap(sourceID string, interval *graphv1.Interval, windowStart, windowEnd time.Time) (Gap, bool) {
	start := windowStart
	if ts := interval.GetStart(); ts != nil && !interval.GetStartUnknown() {
		start = ts.AsTime().UTC()
	}
	endOpen := interval.GetEnd() == nil || interval.GetEndUnknown()
	end := windowEnd
	if !endOpen {
		end = interval.GetEnd().AsTime().UTC()
	}

	overlapStart, overlapEnd := start, end
	if overlapStart.Before(windowStart) {
		overlapStart = windowStart
	}
	if overlapEnd.After(windowEnd) {
		overlapEnd = windowEnd
	}
	if !overlapStart.Before(overlapEnd) {
		return Gap{}, false
	}
	return Gap{
		SourceID:       sourceID,
		Start:          start,
		End:            end,
		EndOpen:        endOpen,
		OverlapStart:   overlapStart,
		OverlapEnd:     overlapEnd,
		OverlapSeconds: overlapEnd.Sub(overlapStart).Seconds(),
	}, true
}

// FlagGapAffected marks every hypothesis whose evidence falls inside a known source gap
// (FR-032), widening its reported bucket outward and recording why.
//
// It widens rather than refutes on purpose: a gap is missing information, and missing
// information makes an estimate less certain, never more wrong in a known direction. Widening
// down would be a claim that the hypothesis is less likely, which the gap does not support.
//
// It returns the hypothesis ids it widened.
func FlagGapAffected(l *ledger.Ledger, report *ExtentReport) ([]string, error) {
	if l == nil || !report.GapAffected() {
		return nil, nil
	}
	reason := report.Summary()
	var widened []string
	for _, h := range l.Hypotheses() {
		if h.Widened {
			// Already widened for another reason; widening twice would overstate the loss.
			continue
		}
		if err := l.Widen(h.ID, ledger.WidenUp, reason); err != nil {
			return widened, fmt.Errorf("flag gap-affected hypothesis %s: %w", h.ID, err)
		}
		widened = append(widened, h.ID)
	}
	return widened, nil
}

func extentEvidence(in *Intake, outcome, detail string, report *ExtentReport) ledger.EvidenceItem {
	coverage := &investigationv1.Coverage{
		DataSource:               "graph.extent",
		WindowActuallyCovered:    in.Window,
		ExecutedAt:               timestampOf(in.ObservedAt),
		QuotaUndetermined:        true,
		VolumeUndetermined:       true,
		IngestionLagUndetermined: true,
	}
	if report != nil {
		coverage.SearchedEntities = report.SourceIDs()
		coverage.Truncation = report.Summary()
		coverage.VolumeConsidered = int64(len(report.Gaps))
		coverage.VolumeUndetermined = false
	}
	return ledger.EvidenceItem{
		ID:            "ev-extent-" + in.Symptom.GetSymptomId(),
		Kind:          EvidenceKindGraphAnswer,
		Worker:        "graph",
		Capability:    "extent",
		SourceOfTruth: "graph",
		Term:          backend.GraphExtent(&graphv1.ExtentRequest{}),
		ValidAt:       in.ValidAt.UTC(),
		ObservedAt:    in.ObservedAt.UTC(),
		CalledAt:      in.ObservedAt.UTC(),
		Mode:          backend.ModeLive,
		Outcome:       outcome,
		Coverage:      coverage,
		DeepLink:      "aisre extent",
		FreeText:      detail,
	}
}
