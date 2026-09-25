// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The decision record (T059, FR-033 to FR-035, SC-010).

var (
	decisionStart = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	decisionEnd   = time.Date(2026, 9, 1, 14, 36, 30, 0, time.UTC)
)

// decisionInput is a concluded investigation ready to be recorded.
func decisionInput(t *testing.T) investigationstore.DecisionInput {
	t.Helper()

	l := savedLedger(t)
	if err := l.SetStatus("h1", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		t.Fatalf("set supported: %v", err)
	}
	return investigationstore.DecisionInput{
		Ledger:         l,
		Subjects:       []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout"}},
		TargetEntities: []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
		StartedAt:      decisionStart,
		EndedAt:        decisionEnd,
		Outcome:        "ranked",
		StopReason:     "diminishing_returns",
		VerdictLine:    "the payments rollout to rev7 is the most likely cause",
		Requester:      testPrincipal,
		Spend: map[string]any{
			"model_calls": 9, "worker_calls": 14, "wall_time_seconds": 270,
		},
		ModelConfig:     map[string]any{"investigator": "claude-sonnet-4-5", "effort": "high"},
		RecordingKey:    testInvestigationID,
		RecordingDigest: strings.Repeat("9f", 32),
	}
}

// TestDecisionRecordPassesTheValidator is the positive half of SC-010: a record built from the
// ledger carries identifiers, statuses, confidences and digests — and 001's validator accepts it.
func TestDecisionRecordPassesTheValidator(t *testing.T) {
	t.Parallel()

	env, err := decisionInput(t).Envelope()
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if rejection := eventlog.Validate(env, eventlog.DefaultSchemaVersions); rejection != nil {
		t.Fatalf("the decision record was rejected: %s", rejection.Error())
	}

	// The idempotency key is the investigation id: that is what makes "exactly one event per
	// investigation" hold through a retry or a restart.
	if env.GetIdempotencyKey() != testInvestigationID {
		t.Errorf("idempotency key = %q, want the investigation id %q",
			env.GetIdempotencyKey(), testInvestigationID)
	}

	body := env.GetBody().(*graphv1.EventEnvelope_RecordInvestigation).RecordInvestigation
	hypotheses := body.GetHypotheses().GetFields()["sre.investigation.hypotheses"].GetStructValue().AsMap()
	ranked, _ := hypotheses["ranked"].([]any)
	if len(ranked) != 3 {
		t.Fatalf("the record carries %d hypotheses, want 3 including the open one", len(ranked))
	}
	top, _ := ranked[0].(map[string]any)
	for _, field := range []string{"id", "kind", "status", "confidence", "bucket", "bucket_low", "bucket_high", "rank", "rationale"} {
		if _, ok := top[field]; !ok {
			t.Errorf("the recorded hypothesis has no %s: %v", field, top)
		}
	}
	if top["id"] != "h1" || top["status"] != string(ledger.StatusSupported) {
		t.Errorf("the top hypothesis recorded as %v", top)
	}
	// The confidence in the record is the ledger's, not a rounding of it.
	h1, _ := decisionInput(t).Ledger.Hypothesis("h1")
	if top["confidence"] != h1.Confidence {
		t.Errorf("recorded confidence %v, ledger %v", top["confidence"], h1.Confidence)
	}
	// The open hypothesis is in the record like any other (FR-019a).
	var sawOpen bool
	for _, entry := range ranked {
		if m, _ := entry.(map[string]any); m["kind"] == string(ledger.KindNoObservedChange) {
			sawOpen = true
		}
	}
	if !sawOpen {
		t.Error("the decision record does not carry the no_observed_change hypothesis")
	}

	// And the link to the bulk beside the graph, which is the only way a reader reaches the
	// evidence itself (FR-035).
	if body.GetRecordingKey() == "" || body.GetRecordingDigest() == "" {
		t.Error("the record does not link to its recording")
	}
}

// TestDecisionRecordIsRejectedWhenASampleSneaksIn is SC-010 itself: the allow-list and the
// denylist, checked from this side of the boundary.
func TestDecisionRecordIsRejectedWhenASampleSneaksIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*graphv1.RecordInvestigation)
		wantCode   string
		wantDetail string
	}{
		{
			name: "a series smuggled into the spend property",
			mutate: func(body *graphv1.RecordInvestigation) {
				body.Spend = mustStruct(t, map[string]any{
					"sre.investigation.spend": map[string]any{
						"error_rate": []any{0.01, 0.04, 0.31, 0.62},
					},
				})
			},
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "numeric samples",
		},
		{
			name: "a denylisted key inside the allow-listed namespace",
			mutate: func(body *graphv1.RecordInvestigation) {
				body.ModelConfig = mustStruct(t, map[string]any{
					"sre.investigation.model_config": map[string]any{"value": 12.5},
				})
			},
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "value",
		},
		{
			name: "a property outside the allow-listed namespace",
			mutate: func(body *graphv1.RecordInvestigation) {
				body.Spend = mustStruct(t, map[string]any{"spend": map[string]any{"calls": 3}})
			},
			wantCode:   eventlog.ReasonPropNamespace,
			wantDetail: "sre.investigation.",
		},
		{
			name: "a digest's content pasted into the hypotheses property",
			mutate: func(body *graphv1.RecordInvestigation) {
				body.Hypotheses = mustStruct(t, map[string]any{
					"sre.investigation.hypotheses": strings.Repeat("x", 5<<10),
				})
			},
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "above the 4096 byte property limit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, err := decisionInput(t).Envelope()
			if err != nil {
				t.Fatalf("build envelope: %v", err)
			}
			body := env.GetBody().(*graphv1.EventEnvelope_RecordInvestigation).RecordInvestigation
			tc.mutate(body)

			rejection := eventlog.Validate(env, eventlog.DefaultSchemaVersions)
			if rejection == nil {
				t.Fatalf("the tampered decision record was accepted; want %s", tc.wantCode)
			}
			if rejection.ReasonCode != tc.wantCode {
				t.Errorf("reason code = %s, want %s (detail: %s)",
					rejection.ReasonCode, tc.wantCode, rejection.ReasonDetail)
			}
			if !strings.Contains(rejection.ReasonDetail, tc.wantDetail) {
				t.Errorf("reason detail = %q, want it to mention %q", rejection.ReasonDetail, tc.wantDetail)
			}
		})
	}
}

// TestDecisionRecordStaysInsideThePropertyCap is why the detail ladder exists: fifty hypotheses
// with statements and rationales do not fit in a graph property, and the record says how much
// detail survived rather than silently dropping it.
func TestDecisionRecordStaysInsideThePropertyCap(t *testing.T) {
	t.Parallel()

	in := decisionInput(t)
	l := in.Ledger
	for i := range 60 {
		id := "h-extra-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := l.AddHypothesis(ledger.Hypothesis{
			ID: id, Kind: ledger.KindChange,
			Statement: strings.Repeat("a long statement about a candidate change ", 4),
		}, 1); err != nil {
			t.Fatalf("add hypothesis %s: %v", id, err)
		}
	}

	env, err := in.Envelope()
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if rejection := eventlog.Validate(env, eventlog.DefaultSchemaVersions); rejection != nil {
		t.Fatalf("a wide investigation's record was rejected: %s", rejection.Error())
	}

	body := env.GetBody().(*graphv1.EventEnvelope_RecordInvestigation).RecordInvestigation
	hypotheses := body.GetHypotheses().GetFields()["sre.investigation.hypotheses"].GetStructValue().AsMap()
	if hypotheses["detail_level"] == investigationstore.DetailFull {
		t.Error("63 hypotheses with statements and rationales fitted at full detail; the cap is not being applied")
	}
	if total, _ := hypotheses["total"].(float64); int(total) != len(l.Hypotheses()) {
		t.Errorf("the record says %v hypotheses in total, the ledger has %d", total, len(l.Hypotheses()))
	}
	if count, _ := hypotheses["count"].(float64); count == 0 {
		t.Error("the record carries no hypotheses at all")
	}
	// Whatever was dropped, the record says so rather than leaving a reader to count.
	if count, _ := hypotheses["count"].(float64); int(count) < len(l.Hypotheses()) {
		if truncated, _ := hypotheses["truncated"].(bool); !truncated {
			t.Error("the record dropped hypotheses without saying so")
		}
	}
}

// TestEmitDecisionRecordWritesExactlyOneEvent is constitution III's "one event per transaction",
// with the idempotency the conclusion needs: emitting twice records once.
func TestEmitDecisionRecordWritesExactlyOneEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	proj := projector.New(store)
	if err := proj.RegisterSource(ctx, eventlog.Source{
		SourceID:      investigationstore.DecisionRecordSourceID,
		Kind:          "investigation",
		Ordering:      "none",
		SchemaVersion: investigationstore.DecisionRecordSchemaVersion,
	}); err != nil {
		t.Fatalf("register source: %v", err)
	}

	in := decisionInput(t)
	result, err := investigationstore.EmitDecisionRecord(ctx, proj, in)
	if err != nil {
		t.Fatalf("emit decision record: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("first emit = %s, want APPLIED", result.GetStatus())
	}

	// The investigation row now names the event, and carries the recording it points at.
	var eventID, recordingKey, recordingDigest string
	if err := store.Pool().QueryRow(ctx, `
		SELECT coalesce(decision_event_id, ''), coalesce(recording_key, ''), coalesce(recording_digest, '')
		FROM investigation.investigations WHERE investigation_id = $1`,
		testInvestigationID).Scan(&eventID, &recordingKey, &recordingDigest); err != nil {
		t.Fatalf("read investigation: %v", err)
	}
	if eventID != result.GetEventId() || recordingKey != in.RecordingKey || recordingDigest != in.RecordingDigest {
		t.Errorf("the investigation row was not linked to its decision record: event %q, recording %q/%q",
			eventID, recordingKey, recordingDigest)
	}

	// Exactly one: a second call records nothing.
	second, err := investigationstore.EmitDecisionRecord(ctx, proj, in)
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}
	if second.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Errorf("second emit = %s, want DUPLICATE_NOOP", second.GetStatus())
	}

	var events int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM log.events WHERE type = 'record_investigation'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("the log holds %d record_investigation events, want exactly 1", events)
	}

	// And nothing else from the run reached the log: judgments, worker calls and ledger updates
	// are never events (constitution III, data-model §"What this feature writes into the graph").
	var others int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM log.events WHERE type <> 'record_investigation'`).Scan(&others); err != nil {
		t.Fatalf("count other events: %v", err)
	}
	if others != 0 {
		t.Errorf("the run emitted %d events other than its decision record", others)
	}
}

// TestEmitDecisionRecordReportsARejectionRatherThanFailing is the SC-010 path end to end: a
// tampered record is refused, the reason is recorded, and the graph is untouched.
func TestEmitDecisionRecordReportsARejectionRatherThanFailing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	proj := projector.New(store)
	if err := proj.RegisterSource(ctx, eventlog.Source{
		SourceID:      investigationstore.DecisionRecordSourceID,
		Kind:          "investigation",
		Ordering:      "none",
		SchemaVersion: investigationstore.DecisionRecordSchemaVersion,
	}); err != nil {
		t.Fatalf("register source: %v", err)
	}

	in := decisionInput(t)
	in.Spend = map[string]any{"latency_samples": []any{12.0, 18.0, 240.0}}

	result, err := investigationstore.EmitDecisionRecord(ctx, proj, in)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Fatalf("emit = %s, want REJECTED", result.GetStatus())
	}
	if result.GetReasonCode() != eventlog.ReasonPropNamespace &&
		result.GetReasonCode() != eventlog.ReasonTelemetryPayload {
		t.Errorf("reason code = %s, want %s or %s", result.GetReasonCode(),
			eventlog.ReasonPropNamespace, eventlog.ReasonTelemetryPayload)
	}

	var rejected int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM log.rejected_events`).Scan(&rejected); err != nil {
		t.Fatalf("count rejections: %v", err)
	}
	if rejected != 1 {
		t.Errorf("log.rejected_events holds %d rows, want 1", rejected)
	}

	var decisionEventID string
	if err := store.Pool().QueryRow(ctx, `
		SELECT coalesce(decision_event_id, '') FROM investigation.investigations
		WHERE investigation_id = $1`, testInvestigationID).Scan(&decisionEventID); err != nil {
		t.Fatalf("read investigation: %v", err)
	}
	if decisionEventID != "" {
		t.Errorf("a rejected decision record still linked the investigation to event %q", decisionEventID)
	}
}

// mustStruct builds a property struct for a tampering case.
func mustStruct(t *testing.T, value map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(value)
	if err != nil {
		t.Fatalf("build struct: %v", err)
	}
	return s
}
