// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func newLog(t *testing.T) (*eventlog.Log, *postgres.Store) {
	t.Helper()
	store := pgtest.Open(t)
	l := eventlog.New(store)
	if err := l.RegisterSource(context.Background(), eventlog.Source{
		SourceID:         "otel:demo",
		Kind:             "otel",
		Ordering:         "none",
		ReorderingWindow: 5 * time.Minute,
		SchemaVersion:    "1.0.0",
	}, nil); err != nil {
		t.Fatalf("register source: %v", err)
	}
	return l, store
}

func TestAppendStampsObservedTime(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)

	observedAt := mustTime("2026-09-01T13:04:20Z")
	result, err := l.Append(ctx, envelope(serviceNode()), eventlog.AppendOptions{ObservedAt: observedAt})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("status = %s (%s: %s), want APPLIED",
			result.GetStatus(), result.GetReasonCode(), result.GetReasonDetail())
	}
	if got := result.GetObservedAt().AsTime(); !got.Equal(observedAt) {
		t.Errorf("observed_at = %s, want the caller's %s (FR-023: replay uses recorded time)", got, observedAt)
	}

	// Without an explicit observed time the log stamps its own (FR-019).
	second := envelope(serviceNode())
	second.EventId = "otel:demo:e2"
	second.IdempotencyKey = "otel:demo:e2"
	before := time.Now().UTC().Add(-time.Second)
	result, err = l.Append(ctx, second, eventlog.AppendOptions{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := result.GetObservedAt().AsTime(); got.Before(before) {
		t.Errorf("observed_at = %s, want a value stamped at acceptance", got)
	}
}

func TestAppendDuplicateIsNoop(t *testing.T) {
	ctx := context.Background()
	l, store := newLog(t)

	env := envelope(serviceNode())
	observedAt := mustTime("2026-09-01T13:04:20Z")
	if _, err := l.Append(ctx, env, eventlog.AppendOptions{ObservedAt: observedAt}); err != nil {
		t.Fatalf("first Append: %v", err)
	}

	result, err := l.Append(ctx, env, eventlog.AppendOptions{ObservedAt: observedAt.Add(time.Minute)})
	if err != nil {
		t.Fatalf("second Append: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Fatalf("status = %s, want DUPLICATE_NOOP (FR-020)", result.GetStatus())
	}
	if got := result.GetObservedAt().AsTime(); !got.Equal(observedAt) {
		t.Errorf("observed_at = %s, want the first delivery's %s", got, observedAt)
	}
	if count := countRows(t, store, `SELECT count(*) FROM log.events`); count != 1 {
		t.Errorf("log holds %d events, want 1", count)
	}
	if differs := countRows(t, store,
		`SELECT count(*) FROM log.duplicate_deliveries WHERE payload_differs`); differs != 0 {
		t.Errorf("payload_differs rows = %d, want 0 for an identical re-delivery", differs)
	}

	// A re-delivery with the same key but a different body is the dangerous case: the first
	// delivery still wins, and the discrepancy is recorded for audit (FR-020).
	altered := envelope(serviceNode())
	altered.GetUpsertNode().DisplayName = "checkout-renamed"
	result, err = l.Append(ctx, altered, eventlog.AppendOptions{ObservedAt: observedAt.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("third Append: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Fatalf("status = %s, want DUPLICATE_NOOP", result.GetStatus())
	}
	if result.GetReasonCode() != "payload_differs" {
		t.Errorf("reason_code = %q, want payload_differs", result.GetReasonCode())
	}
	if differs := countRows(t, store,
		`SELECT count(*) FROM log.duplicate_deliveries WHERE payload_differs`); differs != 1 {
		t.Errorf("payload_differs rows = %d, want 1", differs)
	}
}

func TestAppendRejections(t *testing.T) {
	ctx := context.Background()
	l, store := newLog(t)

	// An unregistered source is refused: every row in the log must have a declared origin.
	stray := envelope(serviceNode())
	stray.SourceId = "otel:unknown"
	result, err := l.Append(ctx, stray, eventlog.AppendOptions{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED || result.GetReasonCode() != eventlog.ReasonUnknownSource {
		t.Fatalf("status = %s reason = %q, want REJECTED/%s",
			result.GetStatus(), result.GetReasonCode(), eventlog.ReasonUnknownSource)
	}

	// A validation failure is recorded and never reaches the log (FR-024).
	bad := envelope(serviceNode())
	bad.EventId = "otel:demo:bad"
	bad.IdempotencyKey = "otel:demo:bad"
	bad.GetUpsertNode().Props = mustStruct(map[string]any{"latency_samples": []any{1.0, 2.0}})
	result, err = l.Append(ctx, bad, eventlog.AppendOptions{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED ||
		result.GetReasonCode() != eventlog.ReasonTelemetryPayload {
		t.Fatalf("status = %s reason = %q, want REJECTED/%s",
			result.GetStatus(), result.GetReasonCode(), eventlog.ReasonTelemetryPayload)
	}
	if count := countRows(t, store, `SELECT count(*) FROM log.events`); count != 0 {
		t.Errorf("log holds %d events, want 0: a rejected event never enters the log", count)
	}
	if count := countRows(t, store, `SELECT count(*) FROM log.rejected_events`); count != 2 {
		t.Errorf("rejected_events rows = %d, want 2", count)
	}
}

func TestIterateAndExtent(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)

	base := mustTime("2026-09-01T13:00:00Z")
	for i := range 3 {
		env := envelope(serviceNode())
		env.EventId = "otel:demo:e" + string(rune('1'+i))
		env.IdempotencyKey = env.EventId
		if _, err := l.Append(ctx, env, eventlog.AppendOptions{
			ObservedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var seen []string
	if err := l.Iterate(ctx, 0, func(record *eventlog.Record) error {
		seen = append(seen, record.Envelope.GetEventId())
		if record.Envelope.GetUpsertNode().GetRef().GetValue() != "checkout" {
			t.Errorf("payload did not round-trip for %s", record.Envelope.GetEventId())
		}
		return nil
	}); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if len(seen) != 3 || seen[0] != "otel:demo:e1" || seen[2] != "otel:demo:e3" {
		t.Fatalf("Iterate returned %v, want the three events in append order", seen)
	}

	// Resuming from a cursor skips what has already been seen.
	var resumed int
	if err := l.Iterate(ctx, 2, func(*eventlog.Record) error { resumed++; return nil }); err != nil {
		t.Fatalf("Iterate from cursor: %v", err)
	}
	if resumed != 1 {
		t.Errorf("Iterate(from=2) saw %d events, want 1", resumed)
	}

	var inRange int
	if err := l.ByObservedRange(ctx, base, base.Add(2*time.Minute), func(*eventlog.Record) error {
		inRange++
		return nil
	}); err != nil {
		t.Fatalf("ByObservedRange: %v", err)
	}
	if inRange != 2 {
		t.Errorf("ByObservedRange saw %d events, want 2 (half-open interval)", inRange)
	}

	var bySource int
	if err := l.BySource(ctx, "otel:demo", 0, func(*eventlog.Record) error { bySource++; return nil }); err != nil {
		t.Fatalf("BySource: %v", err)
	}
	if bySource != 3 {
		t.Errorf("BySource saw %d events, want 3", bySource)
	}

	// Two checkpoints, the second admitting a gap before it (FR-052).
	if err := l.Checkpoint(ctx, eventlog.Checkpoint{
		SourceID: "otel:demo", At: base.Add(5 * time.Minute),
		From: base, To: base.Add(5 * time.Minute),
	}, nil); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := l.Checkpoint(ctx, eventlog.Checkpoint{
		SourceID: "otel:demo", At: base.Add(65 * time.Minute),
		From: base.Add(60 * time.Minute), To: base.Add(65 * time.Minute),
		GapBefore: true, Note: "feeder reconnected",
	}, nil); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	extent, err := l.Extent(ctx)
	if err != nil {
		t.Fatalf("Extent: %v", err)
	}
	if got := extent.GetEarliestObserved().AsTime(); !got.Equal(base) {
		t.Errorf("earliest observed = %s, want %s", got, base)
	}
	if got := extent.GetLatestObserved().AsTime(); !got.Equal(base.Add(2 * time.Minute)) {
		t.Errorf("latest observed = %s, want %s", got, base.Add(2*time.Minute))
	}
	if len(extent.GetSources()) != 1 {
		t.Fatalf("extent lists %d sources, want 1", len(extent.GetSources()))
	}
	source := extent.GetSources()[0]
	if got := source.GetLastCheckpoint().AsTime(); !got.Equal(base.Add(65 * time.Minute)) {
		t.Errorf("last checkpoint = %s, want the most recent one", got)
	}
	if len(source.GetGaps()) != 1 {
		t.Fatalf("extent lists %d gaps, want 1", len(source.GetGaps()))
	}
	gap := source.GetGaps()[0]
	if got := gap.GetStart().AsTime(); !got.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("gap start = %s, want the end of the previous coverage %s", got, base.Add(5*time.Minute))
	}
	if got := gap.GetEnd().AsTime(); !got.Equal(base.Add(60 * time.Minute)) {
		t.Errorf("gap end = %s, want where the feeder picked up again", got)
	}
}

func TestAppendSourceObservedAtIsProvenanceOnly(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t)

	env := envelope(serviceNode())
	env.SourceObservedAt = timestamppb.New(mustTime("2026-09-01T12:00:00Z"))
	observedAt := mustTime("2026-09-01T13:04:20Z")
	result, err := l.Append(ctx, env, eventlog.AppendOptions{ObservedAt: observedAt})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// FR-019: a source may say when it first learned the fact; that never replaces the
	// graph's own observed time.
	if got := result.GetObservedAt().AsTime(); !got.Equal(observedAt) {
		t.Errorf("observed_at = %s, want %s", got, observedAt)
	}
	if err := l.Iterate(ctx, 0, func(record *eventlog.Record) error {
		if got := record.Envelope.GetSourceObservedAt().AsTime(); !got.Equal(mustTime("2026-09-01T12:00:00Z")) {
			t.Errorf("source_observed_at = %s, want it kept as provenance", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
}

func countRows(t *testing.T, store *postgres.Store, sql string) int {
	t.Helper()
	var count int
	if err := store.Pool().QueryRow(context.Background(), sql).Scan(&count); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return count
}
