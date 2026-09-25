// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// The digest contract (tasks.md T034; FR-014, FR-014a, FR-014b, FR-027, FR-037).

func coverage(t *testing.T) *investigationv1.Coverage {
	t.Helper()
	c, err := engine.CoverageInput{
		SearchedEntities: []string{"entity-payments"},
		DataSource:       "metrics",
		WindowCovered:    engine.NewWindow(origin.Add(-time.Hour), origin),
		VolumeConsidered: 120,
		Sampling:         "none",
		ExecutedAt:       origin,
		IngestionLag:     30 * time.Second,
	}.Coverage()
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	return c
}

func request() *engine.Request {
	return &engine.Request{
		Term:                   engine.MonitorState(pointer(), engine.NewWindow(origin.Add(-time.Hour), origin)),
		DiscriminatingQuestion: "did the monitor recover?",
	}
}

// TestTheSixOutcomesAreDistinct: only NO_DATA is evidence that nothing happened, each renders
// differently, and the collapse FR-027 names cannot be written by accident.
func TestTheSixOutcomesAreDistinct(t *testing.T) {
	t.Parallel()

	outcomes := []engine.Outcome{
		engine.DigestOutcome{Body: &investigationv1.Digest{Coverage: coverage(t)}},
		engine.NoData{Coverage: coverage(t)},
		engine.NotYetIngested{Coverage: coverage(t)},
		engine.QueryFailed{Reason: investigationv1.FailureReason_RATE_LIMITED, Detail: "the vendor said slow down"},
		engine.Partial{Body: &investigationv1.Digest{Coverage: coverage(t)}, Missing: "the last five minutes"},
		engine.NotRecorded{TermKey: "abc", Coverage: coverage(t)},
	}

	kinds := make(map[investigationv1.TermOutcome]struct{}, len(outcomes))
	renders := make(map[string]struct{}, len(outcomes))
	nothingHappened := 0
	for _, outcome := range outcomes {
		kinds[outcome.Kind()] = struct{}{}
		renders[outcome.Render()] = struct{}{}
		if outcome.MeansNothingHappened() {
			nothingHappened++
		}
	}
	if len(kinds) != 6 {
		t.Errorf("the six outcomes collapsed to %d enum values", len(kinds))
	}
	if len(renders) != 6 {
		t.Errorf("the six outcomes render to %d distinct strings; a reader who cannot tell them apart has been told nothing", len(renders))
	}
	if nothingHappened != 1 {
		t.Errorf("%d outcomes claim to be evidence that nothing happened; exactly one — NO_DATA — may (FR-027)", nothingHappened)
	}
	if noData := (engine.NoData{}); !noData.MeansNothingHappened() {
		t.Error("NO_DATA is not evidence that nothing happened")
	}
}

// TestEveryDigestCarriesCoverage: a response with no coverage block is rejected
// `missing_coverage`, because "this window was searched and held nothing" and "we do not know
// what was searched" are different sentences (FR-014a).
func TestEveryDigestCarriesCoverage(t *testing.T) {
	t.Parallel()

	_, err := engine.NewResponse(engine.ResponseInput{
		Request: request(),
		Outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{}},
		}},
		Mode: engine.ModeLive,
	})
	if engine.ReasonOf(err) != engine.ReasonMissingCoverage {
		t.Fatalf("reason = %q, want %q (%v)", engine.ReasonOf(err), engine.ReasonMissingCoverage, err)
	}

	// A QUERY_FAILED is the one outcome allowed to carry none: a query that never ran covered
	// nothing.
	resp, err := engine.NewResponse(engine.ResponseInput{
		Request: request(),
		Outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_TIMED_OUT, Detail: "took too long"},
		Mode:    engine.ModeLive,
	})
	if err != nil {
		t.Fatalf("a failed query with no coverage was rejected: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED {
		t.Errorf("outcome = %s, want QUERY_FAILED", resp.GetOutcome())
	}
}

func TestCoverageRefusesWhatAReaderCannotAct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input engine.CoverageInput
		want  string
	}{
		{"no data source", engine.CoverageInput{WindowCovered: engine.NewWindow(origin, origin.Add(time.Hour)), ExecutedAt: origin}, "data source"},
		{"no covered window", engine.CoverageInput{DataSource: "metrics", ExecutedAt: origin}, "window actually covered"},
		{"no execution instant", engine.CoverageInput{DataSource: "metrics", WindowCovered: engine.NewWindow(origin, origin.Add(time.Hour))}, "execution instant"},
		{"a negative volume", engine.CoverageInput{DataSource: "metrics", WindowCovered: engine.NewWindow(origin, origin.Add(time.Hour)), ExecutedAt: origin, VolumeConsidered: -1}, "negative volume"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.input.Coverage()
			if engine.ReasonOf(err) != engine.ReasonMissingCoverage {
				t.Fatalf("reason = %q, want %q (%v)", engine.ReasonOf(err), engine.ReasonMissingCoverage, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}

	// An undetermined field is *stated*, never omitted.
	c, err := engine.CoverageInput{
		DataSource:               "metrics",
		WindowCovered:            engine.NewWindow(origin, origin.Add(time.Hour)),
		ExecutedAt:               origin,
		VolumeUndetermined:       true,
		IngestionLagUndetermined: true,
		QuotaUndetermined:        true,
	}.Coverage()
	if err != nil {
		t.Fatalf("coverage with undetermined fields: %v", err)
	}
	if !c.GetVolumeUndetermined() || !c.GetIngestionLagUndetermined() || !c.GetQuotaUndetermined() {
		t.Error("an undetermined field was omitted rather than stated")
	}
}

// TestTruncationIsWrittenIntoTheResponse: a replay must reproduce the truncation the
// investigator actually saw, so it lives in the response rather than beside it (FR-037).
func TestTruncationIsWrittenIntoTheResponse(t *testing.T) {
	t.Parallel()

	patterns := make([]*investigationv1.LogPattern, 0, engine.MaxLogPatterns+20)
	for i := range engine.MaxLogPatterns + 20 {
		patterns = append(patterns, &investigationv1.LogPattern{
			Template: fmt.Sprintf("template <num> number %d", i),
			Count:    int64(engine.MaxLogPatterns + 20 - i),
		})
	}

	resp, err := engine.NewResponse(engine.ResponseInput{
		Request: request(),
		Outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body:     &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{Patterns: patterns}},
			Coverage: coverage(t),
		}},
		Mode: engine.ModeLive,
	})
	if err != nil {
		t.Fatalf("new response: %v", err)
	}

	got := resp.GetDigest().GetLog().GetPatterns()
	if len(got) != engine.MaxLogPatterns {
		t.Fatalf("kept %d patterns, want the cap of %d", len(got), engine.MaxLogPatterns)
	}
	truncation := resp.GetDigest().GetTruncation()
	if !truncation.GetTruncated() {
		t.Fatal("the response was truncated and does not say so")
	}
	if truncation.GetCriterion() != engine.CriterionCardinalityCap {
		t.Errorf("criterion = %q, want %q", truncation.GetCriterion(), engine.CriterionCardinalityCap)
	}
	if !strings.Contains(truncation.GetWhatWasDropped(), "patterns") {
		t.Errorf("what was dropped = %q, which does not name the rows", truncation.GetWhatWasDropped())
	}
	// The cap drops from the tail of the published ordering, so the most frequent survive.
	if got[0].GetCount() < got[len(got)-1].GetCount() {
		t.Error("the cap dropped the most interesting rows; the ordering must put them first")
	}
}

// TestFreeTextIsFlaggedUnverifiedAndBounded: there is at most one free-text field per digest, it
// is flagged unverified wherever it is rendered or recorded, and it is truncated into itself.
func TestFreeTextIsFlaggedUnverifiedAndBounded(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a plausible-sounding sentence. ", 100)
	resp, err := engine.NewResponse(engine.ResponseInput{
		Request:  request(),
		Outcome:  engine.DigestOutcome{Body: &investigationv1.Digest{Coverage: coverage(t)}},
		Mode:     engine.ModeLive,
		FreeText: long,
	})
	if err != nil {
		t.Fatalf("new response: %v", err)
	}

	text := resp.GetDigest().GetFreeText()
	if !strings.HasPrefix(text, engine.FreeTextPrefix) {
		t.Errorf("free text %q is not flagged unverified", text)
	}
	if len(text) > engine.MaxFreeTextBytes+len(engine.FreeTextPrefix) {
		t.Errorf("free text is %d bytes, over the %d-byte cap", len(text), engine.MaxFreeTextBytes)
	}
	if !resp.GetDigest().GetTruncation().GetTruncated() {
		t.Error("the free text was truncated and the response does not say so")
	}
}

// TestResponseKeyAndDigestAreComputed: every answer is keyed by its term and digested by its
// content, and the digest ignores wall-clock time, which is a property of the call rather than
// of the answer.
func TestResponseKeyAndDigestAreComputed(t *testing.T) {
	t.Parallel()

	build := func(duration time.Duration) *engine.Response {
		resp, err := engine.NewResponse(engine.ResponseInput{
			Request:  request(),
			Outcome:  engine.DigestOutcome{Body: &investigationv1.Digest{Coverage: coverage(t)}},
			Mode:     engine.ModeLive,
			Duration: duration,
		})
		if err != nil {
			t.Fatalf("new response: %v", err)
		}
		return resp
	}

	fast, slow := build(time.Millisecond), build(9*time.Second)
	wantKey, err := engine.TermKey(request().GetTerm())
	if err != nil {
		t.Fatalf("term key: %v", err)
	}
	if fast.GetTermKey() != wantKey {
		t.Errorf("term key = %q, want %q", fast.GetTermKey(), wantKey)
	}
	if fast.GetResponseDigest() != slow.GetResponseDigest() {
		t.Errorf("two answers that agree on every fact have different digests (%s, %s); duration is not part of what the same answer means",
			fast.GetResponseDigest(), slow.GetResponseDigest())
	}
}

// TestOutcomeOfRoundTrips: a consumer reading a recorded world gets back the same six types a
// live call produces.
func TestOutcomeOfRoundTrips(t *testing.T) {
	t.Parallel()

	for _, outcome := range []engine.Outcome{
		engine.DigestOutcome{Body: &investigationv1.Digest{Coverage: coverage(t)}},
		engine.NoData{Coverage: coverage(t)},
		engine.NotYetIngested{Coverage: coverage(t)},
		engine.QueryFailed{Reason: investigationv1.FailureReason_NOT_PERMITTED, Detail: "read-only credential"},
		engine.Partial{Body: &investigationv1.Digest{Coverage: coverage(t)}, Missing: "one group"},
		engine.NotRecorded{TermKey: "abc", Coverage: coverage(t)},
	} {
		t.Run(outcome.Kind().String(), func(t *testing.T) {
			t.Parallel()
			resp, err := engine.NewResponse(engine.ResponseInput{
				Request: request(),
				Outcome: outcome,
				Mode:    engine.ModeRecorded,
			})
			if err != nil {
				t.Fatalf("new response: %v", err)
			}
			got, err := engine.OutcomeOf(resp)
			if err != nil {
				t.Fatalf("outcome of: %v", err)
			}
			if got.Kind() != outcome.Kind() {
				t.Errorf("round trip = %s, want %s", got.Kind(), outcome.Kind())
			}
			if got.MeansNothingHappened() != outcome.MeansNothingHappened() {
				t.Errorf("round trip changed whether %s is evidence that nothing happened", outcome.Kind())
			}
		})
	}
}

// TestNoDataNamesAnAbsentSource: a term whose data source the organisation does not have is
// still answered, and the answer says which source is missing so a consumer concludes "this
// cannot be checked here" instead of retrying.
func TestNoDataNamesAnAbsentSource(t *testing.T) {
	t.Parallel()

	resp, err := engine.NewResponse(engine.ResponseInput{
		Request: request(),
		Outcome: engine.NoData{Coverage: coverage(t), AbsentSource: "trace data"},
		Mode:    engine.ModeLive,
	})
	if err != nil {
		t.Fatalf("new response: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome = %s, want NO_DATA", resp.GetOutcome())
	}
	if !strings.Contains(resp.GetDigest().GetCoverage().GetTruncation(), "trace data") {
		t.Errorf("the coverage block does not name the absent source: %q", resp.GetDigest().GetCoverage().GetTruncation())
	}
	outcome, err := engine.OutcomeOf(resp)
	if err != nil {
		t.Fatalf("outcome of: %v", err)
	}
	if !strings.Contains(outcome.Render(), "cannot be checked here") {
		t.Errorf("the rendering does not tell a consumer to stop retrying: %q", outcome.Render())
	}
}
