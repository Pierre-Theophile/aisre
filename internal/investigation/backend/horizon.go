// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// Nobody sees the future (constitution principle II; tasks.md Phase 8 Track K-B, K4).
//
// An investigation observes the world at one instant — its `observed_at`. Telemetry after that
// instant does not exist: not "is not interesting", not "is out of policy", but *does not
// exist*, because the investigation is being replayed at, or reasoned about as of, that instant.
// A backend that answers a window running past it is inventing evidence the investigator could
// not have had, and an accuracy number computed over such an answer measures nothing.
//
// The rule is therefore enforced in the backends rather than trusted to the planner:
//
//   - a window that extends past the horizon is truncated to it, and the coverage block says so
//     (`truncated_to_horizon`, `horizon`, and the `truncation` criterion), because a narrowed
//     window that does not announce itself is worse than a refusal;
//   - a window that lies entirely past the horizon is `NO_DATA` with that reason, not an empty
//     digest with a shrug: the window was not searched, it could not have been;
//   - a request that declares no horizon is left alone. `observed_at` is the investigation's to
//     state, and a backend used outside an investigation (a bench, a testkit conformance run)
//     has no horizon to clamp to.
//
// The clamp is applied to the term *before* the answer is produced, so a generator generates no
// point past the horizon and a replay looks up the key it would have looked up anyway; the
// coverage annotation is applied to the response afterwards, so the statement is carried by the
// answer rather than kept beside it (FR-037).

// HorizonTruncation is the criterion written into `coverage.truncation` when a window was cut
// back to the investigation's observed_at.
const HorizonTruncation = "horizon"

// HorizonState says how the windows of one term stand relative to a horizon.
type HorizonState int

const (
	// WithinHorizon is a term whose windows all end at or before the horizon, or a request that
	// declared no horizon. Nothing is clamped.
	WithinHorizon HorizonState = iota
	// TruncatedToHorizon is a term with at least one window straddling the horizon: it starts at
	// or before it and ends after it. The window is cut back to the horizon.
	TruncatedToHorizon
	// PastHorizon is a term whose windows all start at or after the horizon. There is nothing to
	// search, and the honest answer is NO_DATA naming the horizon.
	PastHorizon
)

// String renders the state for a coverage criterion or a test failure.
func (s HorizonState) String() string {
	switch s {
	case TruncatedToHorizon:
		return "truncated_to_horizon"
	case PastHorizon:
		return "past_horizon"
	default:
		return "within_horizon"
	}
}

// HorizonOf returns the horizon a request declares — its `observed_at` — and whether it declared
// one. A zero or absent observed_at is no horizon: the backend clamps nothing.
func HorizonOf(req *Request) (time.Time, bool) {
	ts := req.GetObservedAt()
	if ts == nil {
		return time.Time{}, false
	}
	at := ts.AsTime().UTC()
	if at.IsZero() || at.Unix() <= 0 {
		return time.Time{}, false
	}
	return at, true
}

// HorizonReason is the sentence a NO_DATA or a coverage block carries when the horizon is what
// bounded the answer. It names the instant, so that a reader of a recorded world can tell "this
// window holds nothing" from "this window is in the future".
func HorizonReason(horizon time.Time, state HorizonState) string {
	return HorizonTruncation + ":" + state.String() + ":" + horizon.UTC().Format(time.RFC3339)
}

// ClampWindow cuts one window back to the horizon. It returns the window to search and how it
// stands: a window entirely at or after the horizon is PastHorizon and is returned unchanged,
// because there is no window left to search.
func ClampWindow(window *Window, horizon time.Time) (*Window, HorizonState) {
	if window.GetStart() == nil || window.GetEnd() == nil || horizon.IsZero() {
		return window, WithinHorizon
	}
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()
	if !end.After(horizon) {
		return window, WithinHorizon
	}
	if !start.Before(horizon) {
		return window, PastHorizon
	}
	clamped := proto.Clone(window).(*Window)
	clamped.End = timestamppb.New(horizon)
	return clamped, TruncatedToHorizon
}

// ClampTerm cuts every window of a term back to the horizon, returning the term to answer and
// the state of the term as a whole.
//
// A term is PastHorizon as soon as *any* of its windows lies entirely at or after the horizon —
// not only when all of them do. A window is half-open and non-empty by contract (`requireWindow`),
// so there is no clamp of a wholly-future window that is still a window: a compare whose symptom
// half begins at the horizon is a question about the future with a baseline attached, and
// answering the baseline half alone would return a comparison against nothing under a name that
// says otherwise. The planner's job is to ask a comparison that ends at the horizon; the
// backend's job is to refuse to pretend it did.
//
// Handle-bearing terms (`exemplars`, `drill_down`) carry no window of their own — the handle was
// minted by an earlier answer that was itself clamped — so they are left alone.
func ClampTerm(term *Term, horizon time.Time) (*Term, HorizonState) {
	if term == nil || horizon.IsZero() {
		return term, WithinHorizon
	}
	windows := termWindows(term)
	if len(windows) == 0 {
		return term, WithinHorizon
	}

	clamped := proto.Clone(term).(*Term)
	targets := termWindows(clamped)
	truncated, past, bounded := 0, 0, 0
	for i, window := range windows {
		next, state := ClampWindow(window, horizon)
		if window.GetStart() == nil || window.GetEnd() == nil {
			continue
		}
		bounded++
		switch state {
		case TruncatedToHorizon:
			truncated++
			// Assigning the field rather than the message keeps the clone's identity (and its
			// internal state) intact; only the end instant moves.
			targets[i].End = next.GetEnd()
		case PastHorizon:
			past++
		case WithinHorizon:
		}
	}
	switch {
	case past > 0:
		return term, PastHorizon
	case truncated > 0:
		return clamped, TruncatedToHorizon
	default:
		return term, WithinHorizon
	}
}

// ClampRequest applies ClampTerm to a request's term, returning the request to answer, the
// horizon it declared and the state. The original request is never mutated: a caller that
// records the request as asked must be able to.
func ClampRequest(req *Request) (*Request, time.Time, HorizonState) {
	horizon, ok := HorizonOf(req)
	if !ok {
		return req, time.Time{}, WithinHorizon
	}
	clampedTerm, state := ClampTerm(req.GetTerm(), horizon)
	if state != TruncatedToHorizon {
		return req, horizon, state
	}
	clamped := proto.Clone(req).(*Request)
	clamped.Term = clampedTerm
	return clamped, horizon, state
}

// AnnotateHorizon writes the truncation into a response's coverage block: the horizon, the flag,
// the criterion, and the covered window cut back to the horizon. It is a no-op for a response
// with no coverage (a QUERY_FAILED) or a state that clamped nothing.
func AnnotateHorizon(resp *Response, horizon time.Time, state HorizonState) {
	if resp == nil || state == WithinHorizon || horizon.IsZero() {
		return
	}
	coverage := resp.GetDigest().GetCoverage()
	if coverage == nil {
		return
	}
	AnnotateCoverageHorizon(coverage, horizon, state)
}

// AnnotateCoverageHorizon is AnnotateHorizon for a coverage block a backend still holds in hand.
func AnnotateCoverageHorizon(coverage *investigationv1.Coverage, horizon time.Time, state HorizonState) {
	if coverage == nil || state == WithinHorizon || horizon.IsZero() {
		return
	}
	coverage.TruncatedToHorizon = true
	coverage.Horizon = timestamppb.New(horizon.UTC())
	if window := coverage.GetWindowActuallyCovered(); window.GetEnd() != nil &&
		window.GetEnd().AsTime().UTC().After(horizon) {
		window.End = timestamppb.New(horizon.UTC())
		if window.GetStart() != nil && window.GetStart().AsTime().UTC().After(horizon) {
			window.Start = timestamppb.New(horizon.UTC())
		}
	}
	reason := HorizonReason(horizon, state)
	switch {
	case coverage.GetTruncation() == "":
		coverage.Truncation = reason
	case !strings.Contains(coverage.GetTruncation(), reason):
		coverage.Truncation = coverage.GetTruncation() + ";" + reason
	}
}

// termWindows returns pointers to the windows a term carries, in a stable order, so that a clamp
// of a cloned term can be written back into the same positions.
func termWindows(term *Term) []*Window {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return []*Window{t.Compare.GetWindows().GetBaseline(), t.Compare.GetWindows().GetSymptom()}
	case *investigationv1.AlgebraTerm_Onset:
		return []*Window{t.Onset.GetSearchWindow()}
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return []*Window{t.NewLogPatterns.GetWindow(), t.NewLogPatterns.GetBaselineWindow()}
	case *investigationv1.AlgebraTerm_ErrorSpans:
		return []*Window{t.ErrorSpans.GetWindow()}
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return []*Window{t.ErrorsByVersion.GetWindow()}
	case *investigationv1.AlgebraTerm_MonitorState:
		return []*Window{t.MonitorState.GetWindow()}
	default:
		// Handle-bearing and graph terms carry no window of their own.
		return nil
	}
}
