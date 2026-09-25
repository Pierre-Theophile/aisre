// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// Nobody sees the future (constitution II; Phase 8 Track K-B, K4).
//
// The rule these tests hold the recorded backend to is not "windows past observed_at are
// unusual": it is that telemetry after the investigation's observation instant *does not exist*,
// so a backend may neither answer from it nor narrow a window to avoid it without saying so.

// horizon is the instant these tests observe the world at.
var horizonAt = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

func requestAt(term *engine.Term, observedAt time.Time) *engine.Request {
	req := &engine.Request{
		Term:                   term,
		ValidAt:                timestamppb.New(observedAt),
		DiscriminatingQuestion: "exploratory:horizon test",
	}
	if !observedAt.IsZero() {
		req.ObservedAt = timestamppb.New(observedAt)
	}
	return req
}

func TestClampWindowStatesWhereItStands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		start   time.Time
		end     time.Time
		want    engine.HorizonState
		wantEnd time.Time
	}{
		{"wholly before", horizonAt.Add(-2 * time.Hour), horizonAt.Add(-time.Hour), engine.WithinHorizon, horizonAt.Add(-time.Hour)},
		{"ends exactly at the horizon", horizonAt.Add(-time.Hour), horizonAt, engine.WithinHorizon, horizonAt},
		{"straddles the horizon", horizonAt.Add(-time.Hour), horizonAt.Add(time.Hour), engine.TruncatedToHorizon, horizonAt},
		{"starts at the horizon", horizonAt, horizonAt.Add(time.Hour), engine.PastHorizon, horizonAt.Add(time.Hour)},
		{"wholly after", horizonAt.Add(time.Hour), horizonAt.Add(2 * time.Hour), engine.PastHorizon, horizonAt.Add(2 * time.Hour)},
	}
	for _, tc := range tests {
		got, state := engine.ClampWindow(engine.NewWindow(tc.start, tc.end), horizonAt)
		if state != tc.want {
			t.Errorf("%s: state = %v, want %v", tc.name, state, tc.want)
		}
		if at := got.GetEnd().AsTime().UTC(); !at.Equal(tc.wantEnd) {
			t.Errorf("%s: clamped end = %s, want %s", tc.name, at.Format(time.RFC3339), tc.wantEnd.Format(time.RFC3339))
		}
	}
}

// A compare whose baseline is answerable and whose symptom is not is truncated rather than
// refused: the half that existed is still evidence, and the digest says the other half was cut.
func TestClampTermTruncatesTheStraddlingHalfAndLeavesTheAskAlone(t *testing.T) {
	t.Parallel()

	pair := engine.NewWindowPair(horizonAt.Add(-30*time.Minute), time.Hour)
	term := engine.Compare(pointer(), pair, investigationv1.Statistic_ERROR_RATE)

	clamped, state := engine.ClampTerm(term, horizonAt)
	if state != engine.TruncatedToHorizon {
		t.Fatalf("state = %v, want TruncatedToHorizon", state)
	}
	symptom := clamped.GetCompare().GetWindows().GetSymptom()
	if at := symptom.GetEnd().AsTime().UTC(); !at.Equal(horizonAt) {
		t.Errorf("clamped symptom ends at %s, want the horizon %s", at.Format(time.RFC3339), horizonAt.Format(time.RFC3339))
	}
	if at := term.GetCompare().GetWindows().GetSymptom().GetEnd().AsTime().UTC(); !at.Equal(horizonAt.Add(30 * time.Minute)) {
		t.Errorf("the term as asked was mutated: symptom ends at %s, want %s; a recorder must be able to "+
			"file what was asked", at.Format(time.RFC3339), horizonAt.Add(30*time.Minute).Format(time.RFC3339))
	}
}

// A compare referenced *at* the horizon asks for a symptom half that lies entirely in the future.
// There is no non-empty clamp of such a window, so the term is refused as a whole rather than
// answered from its baseline half alone: a comparison against nothing is not a comparison.
func TestClampTermRefusesATermWithAWhollyFutureWindow(t *testing.T) {
	t.Parallel()

	term := engine.Compare(pointer(), engine.NewWindowPair(horizonAt, time.Hour), investigationv1.Statistic_ERROR_RATE)
	if _, state := engine.ClampTerm(term, horizonAt); state != engine.PastHorizon {
		t.Fatalf("state = %v, want PastHorizon for a compare whose symptom half begins at the horizon", state)
	}
}

// A request that declares no observed_at has no horizon: a bench or a conformance run is not an
// investigation, and clamping one to an instant it never named would be the backend inventing
// policy.
func TestClampRequestWithoutAnObservedAtClampsNothing(t *testing.T) {
	t.Parallel()

	term := engine.MonitorState(pointer(), engine.NewWindow(horizonAt, horizonAt.Add(time.Hour)))
	req := requestAt(term, time.Time{})
	got, _, state := engine.ClampRequest(req)
	if state != engine.WithinHorizon {
		t.Fatalf("state = %v, want WithinHorizon for a request with no observed_at", state)
	}
	if got != req {
		t.Error("ClampRequest copied a request it had no horizon for")
	}
}

// recordedWorld records one answer under the key of term and returns the backend over it.
func recordedWorld(t *testing.T, term *engine.Term, coverageEnd time.Time) *engine.Recorded {
	t.Helper()
	dir := t.TempDir()
	recorder, err := sdk.NewRecorderWithOptions(dir, sdk.RecordOptions{
		DrillDownDepth:         1,
		RedactionPolicyVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	coverage, err := engine.CoverageInput{
		DataSource:               "metrics",
		WindowCovered:            engine.NewWindow(horizonAt.Add(-time.Hour), coverageEnd),
		VolumeConsidered:         120,
		Sampling:                 "none",
		IngestionLagUndetermined: true,
		QuotaUndetermined:        true,
		ExecutedAt:               horizonAt,
	}.Coverage()
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	req := requestAt(term, horizonAt)
	resp, err := engine.NewResponse(engine.ResponseInput{
		Request: req,
		Outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body: &investigationv1.Digest_MonitorState{MonitorState: &investigationv1.MonitorStateDigest{
				StateAtStart: "OK", StateAtEnd: "ALERT",
			}},
			Coverage: coverage,
		}},
		Mode:           "live",
		CostClass:      engine.PublishedCostClass(engine.TermName(term)),
		BackendVersion: "test",
	})
	if err != nil {
		t.Fatalf("new response: %v", err)
	}
	if err := recorder.Record(context.Background(), req, resp); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := recorder.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	world, err := sdk.LoadWorld(dir)
	if err != nil {
		t.Fatalf("load world: %v", err)
	}
	return engine.NewRecordedFromWorld(world)
}

// The recorded backend answers a window lying entirely past the horizon with NO_DATA naming the
// horizon — never NOT_RECORDED, which would blame the fixture for a fact about time, and never
// from the world, which cannot hold the future either.
func TestRecordedRefusesAWindowPastTheHorizon(t *testing.T) {
	t.Parallel()

	future := engine.MonitorState(pointer(), engine.NewWindow(horizonAt.Add(time.Hour), horizonAt.Add(2*time.Hour)))
	backend := recordedWorld(t, engine.MonitorState(pointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt)), horizonAt)

	resp, err := backend.Execute(context.Background(), requestAt(future, horizonAt))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome = %s, want NO_DATA; a window in the future was not searched and could not be",
			resp.GetOutcome())
	}
	coverage := resp.GetDigest().GetCoverage()
	if !coverage.GetTruncatedToHorizon() {
		t.Error("coverage does not say it was truncated to the horizon")
	}
	if at := coverage.GetHorizon().AsTime().UTC(); !at.Equal(horizonAt) {
		t.Errorf("coverage horizon = %s, want %s", at.Format(time.RFC3339), horizonAt.Format(time.RFC3339))
	}
	if !strings.Contains(coverage.GetTruncation(), engine.HorizonTruncation) {
		t.Errorf("truncation = %q, want it to name the horizon criterion", coverage.GetTruncation())
	}
	if misses := backend.Misses(); len(misses.Missed) != 0 {
		t.Errorf("the horizon answer was counted as a miss (%v); a fixture is not incomplete because time is", misses.Missed)
	}
}

// A window straddling the horizon is served from the world under the key of the window that was
// actually answerable, and the answer says it was cut back — including when the recording itself
// was made by a backend that had no horizon to respect.
func TestRecordedTruncatesAStraddlingWindowAndSaysSo(t *testing.T) {
	t.Parallel()

	answerable := engine.MonitorState(pointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt))
	// The recording overstates its own coverage: it claims an hour past the horizon.
	backend := recordedWorld(t, answerable, horizonAt.Add(time.Hour))

	asked := engine.MonitorState(pointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt.Add(time.Hour)))
	resp, err := backend.Execute(context.Background(), requestAt(asked, horizonAt))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s, want DIGEST; the answerable half of the window is in the world", resp.GetOutcome())
	}
	coverage := resp.GetDigest().GetCoverage()
	if !coverage.GetTruncatedToHorizon() {
		t.Error("a truncated answer does not say it was truncated to the horizon")
	}
	if at := coverage.GetWindowActuallyCovered().GetEnd().AsTime().UTC(); !at.Equal(horizonAt) {
		t.Errorf("covered window ends at %s, want the horizon %s; the recording's own claim may not outlive it",
			at.Format(time.RFC3339), horizonAt.Format(time.RFC3339))
	}
	digest, err := sdk.ResponseDigest(resp)
	if err != nil {
		t.Fatalf("response digest: %v", err)
	}
	if digest != resp.GetResponseDigest() {
		t.Error("the response digest was not recomputed over the answer that actually left the backend")
	}
}

// A window wholly inside the horizon is served unchanged: the rule costs an ordinary answer
// nothing, which is what makes it safe to apply to every call.
func TestRecordedLeavesAnAnswerableWindowAlone(t *testing.T) {
	t.Parallel()

	term := engine.MonitorState(pointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt))
	backend := recordedWorld(t, term, horizonAt)

	resp, err := backend.Execute(context.Background(), requestAt(term, horizonAt))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s, want DIGEST", resp.GetOutcome())
	}
	if coverage := resp.GetDigest().GetCoverage(); coverage.GetTruncatedToHorizon() {
		t.Error("an answer wholly inside the horizon claims to have been truncated to it")
	}
}
