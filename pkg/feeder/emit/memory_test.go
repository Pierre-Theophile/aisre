// SPDX-License-Identifier: Apache-2.0

package emit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
)

func testDescription() feeder.Description {
	return feeder.Description{
		SourceID:         "k8s:demo",
		Kind:             "k8s",
		Ordering:         feeder.OrderingPerSourceSequence,
		ReorderingWindow: 60 * time.Second,
	}
}

func validAt(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, "2026-09-01T13:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

func upsert(t *testing.T, d feeder.Description, id, name string) *graphv1.EventEnvelope {
	t.Helper()
	return feeder.UpsertNode(d, id, feeder.NodeFact{
		Ref:     feeder.Ref(feeder.NSK8sDeployment, name),
		Type:    graphv1.NodeType_WORKLOAD,
		ValidAt: validAt(t),
	})
}

func TestMemoryEmitterAppliesAndDeduplicates(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d)
	ctx := t.Context()

	ev := upsert(t, d, "k8s:demo:deploy:shop/checkout@rv1", "shop/checkout")
	result, err := em.Emit(ctx, ev)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("first delivery = %s, want APPLIED", result.GetStatus())
	}
	if result.GetObservedAt() == nil {
		t.Error("no observed time was stamped")
	}

	// FR-020: a second delivery of a seen idempotency key changes nothing.
	again, err := em.Emit(ctx, ev)
	if err != nil {
		t.Fatalf("second Emit: %v", err)
	}
	if again.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Fatalf("second delivery = %s, want DUPLICATE_NOOP", again.GetStatus())
	}
	if got := len(em.Events()); got != 1 {
		t.Errorf("kept %d events, want 1: a duplicate must not be recorded twice", got)
	}
	if got := len(em.Results()); got != 2 {
		t.Errorf("kept %d results, want one per Emit call", got)
	}
}

func TestMemoryEmitterRefusesTelemetry(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d)

	// The rejection the baseline fixture ships: a numeric array in props (SC-009).
	bad := upsert(t, d, "k8s:demo:bad-props-1", "shop/checkout")
	bad.GetUpsertNode().Props = mustNumericSeries(t)

	result, err := em.Emit(t.Context(), bad)
	if err == nil {
		t.Fatal("strict mode accepted a telemetry payload without an error")
	}
	var rejection *feeder.Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("Emit returned %T, want a *feeder.Rejection", err)
	}
	if rejection.ReasonCode != feeder.ReasonTelemetryPayload {
		t.Errorf("reason %q, want %q", rejection.ReasonCode, feeder.ReasonTelemetryPayload)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Errorf("result status %s, want REJECTED", result.GetStatus())
	}
	if got := len(em.Rejected()); got != 1 {
		t.Errorf("Rejected() has %d entries, want 1", got)
	}
	if got := len(em.Events()); got != 0 {
		t.Errorf("a refused event was kept (%d events)", got)
	}
}

func TestMemoryEmitterNonStrictMirrorsTheServer(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d, emit.WithStrict(false))

	bad := upsert(t, d, "k8s:demo:bad-props-1", "shop/checkout")
	bad.GetUpsertNode().Props = mustNumericSeries(t)

	result, err := em.Emit(t.Context(), bad)
	if err != nil {
		t.Fatalf("non-strict Emit returned an error: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Fatalf("status %s, want REJECTED", result.GetStatus())
	}
}

func TestMemoryEmitterRefusesAnotherSourcesEvent(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d)

	other := testDescription()
	other.SourceID = "otel:demo"
	if _, err := em.Emit(t.Context(), upsert(t, other, "otel:demo:svc:checkout", "shop/checkout")); err == nil {
		t.Fatal("an emitter scoped to k8s:demo accepted an otel:demo event (FR-046)")
	}
}

func TestMemoryEmitterCheckpointAndFlush(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d)
	ctx := context.Background()

	from := validAt(t)
	to := from.Add(80 * time.Second)
	if err := em.Checkpoint(ctx, feeder.CheckpointFact{ExtentFrom: from, ExtentTo: to, GapBefore: true}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	events := em.Events()
	if len(events) != 1 {
		t.Fatalf("Checkpoint emitted %d events, want 1", len(events))
	}
	ckpt := events[0].GetSourceCheckpoint()
	if ckpt == nil {
		t.Fatal("the checkpoint event has no source_checkpoint body")
	}
	if !ckpt.GetGapBefore() {
		t.Error("gap_before was dropped")
	}
	if got, want := events[0].GetEventId(), feeder.CheckpointID(d, to); got != want {
		t.Errorf("checkpoint event id %q, want the deterministic %q", got, want)
	}

	if err := em.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if em.Flushes() != 1 {
		t.Errorf("Flushes() = %d, want 1", em.Flushes())
	}
}

func TestMemoryEmitterReset(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d)
	ev := upsert(t, d, "k8s:demo:deploy:shop/checkout@rv1", "shop/checkout")
	if _, err := em.Emit(t.Context(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	em.Reset()
	if len(em.Events()) != 0 || len(em.Results()) != 0 {
		t.Fatal("Reset left something behind")
	}
	result, err := em.Emit(t.Context(), ev)
	if err != nil {
		t.Fatalf("Emit after Reset: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Errorf("after Reset the same event answered %s, want APPLIED", result.GetStatus())
	}
}

func TestMemoryEmitterClonesTheEnvelope(t *testing.T) {
	t.Parallel()
	d := testDescription()
	em := emit.NewMemoryEmitter(d)
	ev := upsert(t, d, "k8s:demo:deploy:shop/checkout@rv1", "shop/checkout")
	if _, err := em.Emit(t.Context(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ev.GetUpsertNode().DisplayName = "mutated after emitting"
	if got := em.Events()[0].GetUpsertNode().GetDisplayName(); got != "" {
		t.Errorf("mutating the caller's envelope changed a recorded event: %q", got)
	}
}
