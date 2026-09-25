// SPDX-License-Identifier: Apache-2.0

package synthetic

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Nobody sees the future (constitution II; Phase 8 Track K-B, K4).
//
// The generator can produce a sample for any instant, which is exactly why it must not. These
// tests hold it to the same rule the recorded backend obeys: past the investigation's observed_at
// there is no telemetry, and a window that reaches there is either cut back — and says so — or
// answered NO_DATA naming the horizon.

// horizonAt is the instant these tests observe the world at: an hour after the change, so that
// half of a two-hour window is observable and half is not.
var horizonAt = changeAt.Add(time.Hour)

func ledgerPointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind:       graphv1.PointerKind_METRIC,
		Selector:   "sel-ledger",
		Vocabulary: "synthetic",
		JoinKeys:   map[string]string{"version": "service.version"},
	}
}

func syntheticBackend(t *testing.T) *Backend {
	t.Helper()
	scenario, _, _, _ := chain(t, ledgerChange())
	scenario.End = horizonAt.Add(4 * time.Hour)
	b, err := New(scenario)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	return b
}

func ask(t *testing.T, b *Backend, term *engine.Term, observedAt time.Time) *engine.Response {
	t.Helper()
	req := &engine.Request{
		Term:                   term,
		ValidAt:                timestamppb.New(observedAt),
		DiscriminatingQuestion: "exploratory:horizon test",
	}
	if !observedAt.IsZero() {
		req.ObservedAt = timestamppb.New(observedAt)
	}
	resp, err := b.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return resp
}

// A window straddling the horizon is answered from the observable half only, and the coverage
// block says the other half was cut. The point count is the load-bearing assertion: a generator
// that clamped the coverage but kept generating would pass a flag check and still hand the
// investigator the future.
func TestSyntheticTruncatesAStraddlingWindow(t *testing.T) {
	t.Parallel()

	b := syntheticBackend(t)
	pair := &investigationv1.WindowPair{
		ReferenceAt:  timestamppb.New(horizonAt.Add(-time.Hour)),
		WidthSeconds: int64(time.Hour / time.Second),
		Baseline:     engine.NewWindow(horizonAt.Add(-2*time.Hour), horizonAt.Add(-time.Hour)),
		Symptom:      engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt.Add(time.Hour)),
	}
	resp := ask(t, b, engine.Compare(ledgerPointer(), pair, investigationv1.Statistic_ERROR_RATE), horizonAt)

	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s, want DIGEST; the observable half of the window is answerable", resp.GetOutcome())
	}
	coverage := resp.GetDigest().GetCoverage()
	if !coverage.GetTruncatedToHorizon() {
		t.Error("coverage does not say the window was truncated to the horizon")
	}
	if at := coverage.GetHorizon().AsTime().UTC(); !at.Equal(horizonAt) {
		t.Errorf("coverage horizon = %s, want %s", at.Format(time.RFC3339), horizonAt.Format(time.RFC3339))
	}
	if !strings.Contains(coverage.GetTruncation(), engine.HorizonTruncation) {
		t.Errorf("truncation = %q, want it to name the horizon criterion", coverage.GetTruncation())
	}
	if at := coverage.GetWindowActuallyCovered().GetEnd().AsTime().UTC(); !at.Equal(horizonAt) {
		t.Errorf("covered window ends at %s, want the horizon %s", at.Format(time.RFC3339), horizonAt.Format(time.RFC3339))
	}

	series := resp.GetDigest().GetMetric().GetSeries()
	if len(series) != 2 {
		t.Fatalf("digest carries %d series, want baseline and symptom", len(series))
	}
	// One minute of resolution over the hour that was observable, not the two hours asked for.
	if got := series[1].GetPointCount(); got != 60 {
		t.Errorf("symptom series holds %d points, want 60; the generator generated past the horizon", got)
	}
	if at := series[1].GetIntervalCovered().GetEnd().AsTime().UTC(); !at.Equal(horizonAt) {
		t.Errorf("symptom interval ends at %s, want the horizon %s", at.Format(time.RFC3339), horizonAt.Format(time.RFC3339))
	}
}

// The truncated answer is the answer to the truncated question: clamping is a narrowing of the
// window, never a rescaling of what was in it.
func TestSyntheticTruncationEqualsAskingTheNarrowerWindow(t *testing.T) {
	t.Parallel()

	b := syntheticBackend(t)
	wide := engine.MonitorState(ledgerPointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt.Add(time.Hour)))
	narrow := engine.MonitorState(ledgerPointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt))

	clamped := ask(t, b, wide, horizonAt)
	direct := ask(t, b, narrow, time.Time{})
	if clamped.GetTermKey() != direct.GetTermKey() {
		t.Errorf("term key of the clamped ask = %q, of the narrow ask = %q; a replay of the clamped ask "+
			"must look up the answer the narrow ask recorded", clamped.GetTermKey(), direct.GetTermKey())
	}
	if got, want := clamped.GetDigest().GetMonitorState().GetStateAtEnd(),
		direct.GetDigest().GetMonitorState().GetStateAtEnd(); got != want {
		t.Errorf("clamped end state = %q, narrow end state = %q", got, want)
	}
}

// A window lying entirely past the horizon is NO_DATA naming the horizon — not an empty digest,
// and not a digest of generated values nobody could have seen.
func TestSyntheticRefusesAWindowPastTheHorizon(t *testing.T) {
	t.Parallel()

	b := syntheticBackend(t)
	future := engine.MonitorState(ledgerPointer(), engine.NewWindow(horizonAt.Add(time.Hour), horizonAt.Add(2*time.Hour)))
	resp := ask(t, b, future, horizonAt)

	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome = %s, want NO_DATA", resp.GetOutcome())
	}
	coverage := resp.GetDigest().GetCoverage()
	if !coverage.GetTruncatedToHorizon() {
		t.Error("coverage does not say it was bounded by the horizon")
	}
	if !strings.Contains(coverage.GetTruncation(), "past_horizon") {
		t.Errorf("truncation = %q, want it to say the window lies past the horizon", coverage.GetTruncation())
	}
	if resp.GetDigest().GetMonitorState() != nil {
		t.Error("the answer carries a monitor digest for a window in the future")
	}
}

// A request that names no observed_at declares no horizon: the generator is also used by benches
// and conformance runs, which are not investigations and have no instant to be honest about.
func TestSyntheticWithoutAnObservedAtAnswersTheWholeWindow(t *testing.T) {
	t.Parallel()

	b := syntheticBackend(t)
	term := engine.MonitorState(ledgerPointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt.Add(time.Hour)))
	resp := ask(t, b, term, time.Time{})

	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s, want DIGEST", resp.GetOutcome())
	}
	if coverage := resp.GetDigest().GetCoverage(); coverage.GetTruncatedToHorizon() {
		t.Error("an answer to a request that declared no horizon claims to have been truncated to one")
	}
}
