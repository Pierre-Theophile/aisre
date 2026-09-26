// SPDX-License-Identifier: Apache-2.0

package record_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

func testDescription() feeder.Description {
	return feeder.Description{
		SourceID:         "k8s:demo",
		Kind:             "k8s",
		Ordering:         feeder.OrderingPerSourceSequence,
		ReorderingWindow: 60 * time.Second,
	}
}

// numericSeries is the property shape the graph always refuses: a list of numbers is a sample
// series whatever it is called (constitution IV).
func numericSeries(t *testing.T) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{"latency_samples": []any{1.0, 2.0, 3.0}})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}

func base(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, "2026-09-01T13:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

// TestPayloadRoundTrip is the FR-044 promise in one test: what Wrap writes, FileSource reads
// back, payload for payload and in the same order.
func TestPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := base(t)
	original := []feeder.Payload{
		{Kind: "deployments", At: at, Seq: 1001, Bytes: []byte(`{"name":"checkout"}`)},
		{Kind: "services", At: at.Add(time.Second), Seq: 1002, Bytes: []byte(`{"name":"checkout-svc"}`)},
		{Kind: "deployments", At: at.Add(2 * time.Second), Seq: 1003, Bytes: []byte{0x0a, 0x03, 0x66, 0x6f, 0x6f}},
	}

	rec := record.Wrap(source.NewSliceSource(original), dir)
	for {
		if _, err := rec.Next(t.Context()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}
	if rec.Count() != len(original) {
		t.Fatalf("recorded %d payloads, want %d", rec.Count(), len(original))
	}

	// Extensions follow the bytes, not the kind: the protobuf payload is not called .json.
	for name, want := range map[string]bool{
		"payloads/deployments/000001.json": true,
		"payloads/deployments/000002.pb":   true,
		"payloads/services/000001.json":    true,
		"payloads/index.jsonl":             true,
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v (%v)", name, err == nil, want, err)
		}
	}

	replayed, err := source.ReadPayloads(dir)
	if err != nil {
		t.Fatalf("ReadPayloads: %v", err)
	}
	if len(replayed) != len(original) {
		t.Fatalf("replayed %d payloads, want %d", len(replayed), len(original))
	}
	for i := range original {
		if replayed[i].Kind != original[i].Kind ||
			replayed[i].Seq != original[i].Seq ||
			!replayed[i].At.Equal(original[i].At) ||
			string(replayed[i].Bytes) != string(original[i].Bytes) {
			t.Errorf("payload %d round-tripped as %+v, want %+v", i, replayed[i], original[i])
		}
	}

	first, last := rec.Extent()
	if !first.Equal(at) || !last.Equal(at.Add(2*time.Second)) {
		t.Errorf("Extent = (%s, %s), want (%s, %s)", first, last, at, at.Add(2*time.Second))
	}
}

func TestPayloadRecorderSanitizesTheKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := record.Wrap(source.NewSliceSource([]feeder.Payload{
		{Kind: "../../escape", Bytes: []byte(`{}`)},
	}), dir)
	if _, err := rec.Next(t.Context()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "payloads")); err != nil {
		t.Fatalf("payloads directory: %v", err)
	}
	// Nothing was written outside the fixture directory.
	if entries, err := os.ReadDir(filepath.Join(dir, "payloads")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 2 { // the sanitized kind directory and index.jsonl
		t.Fatalf("payloads holds %d entries, want the kind directory and the index", len(entries))
	}
}

// TestEventRoundTrip is the other half: what record.Emitter writes, internal/fixture reads back
// as a loadable event stream, with the observed times the graph assigned (FR-023, FR-049).
func TestEventRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := testDescription()
	at := base(t)
	observed := at.Add(2 * time.Second)

	memory := emit.NewMemoryEmitter(d, emit.WithStrict(false), emit.WithMemoryClock(func() time.Time { return observed }))
	rec := record.Emitter(memory, dir)
	ctx := t.Context()

	accepted := []*graphv1.EventEnvelope{
		feeder.UpsertNode(d, "k8s:demo:deploy:shop/checkout@rv1001", feeder.NodeFact{
			Meta:        feeder.Meta{Seq: 1001},
			Ref:         feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			Type:        graphv1.NodeType_WORKLOAD,
			DisplayName: "checkout",
			ValidAt:     at,
		}),
		feeder.UpsertEdge(d, "k8s:demo:edge:runs-on:shop/checkout@rv1001", feeder.EdgeFact{
			Src:     feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			Dst:     feeder.Ref(feeder.NSK8sCluster, "shop-prod"),
			Type:    graphv1.EdgeType_RUNS_ON,
			ValidAt: at,
		}),
	}
	for _, ev := range accepted {
		if _, err := rec.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	// One event the graph must always refuse, so the recording also states its rejection
	// contract (SC-009).
	bad := feeder.UpsertNode(d, "k8s:demo:bad-props-1", feeder.NodeFact{
		Ref:     feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
		Type:    graphv1.NodeType_WORKLOAD,
		ValidAt: at,
		Props:   numericSeries(t),
	})
	if _, err := rec.Emit(ctx, bad); err != nil {
		t.Fatalf("Emit of a refused event returned an error: %v", err)
	}
	// A re-delivery is a no-op and must not be written a second time.
	if _, err := rec.Emit(ctx, accepted[0]); err != nil {
		t.Fatalf("re-Emit: %v", err)
	}
	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}
	if got := rec.Accepted(); got != 2 {
		t.Fatalf("Accepted() = %d, want 2", got)
	}

	events, err := fixture.ReadEvents(filepath.Join(dir, record.EventsFile))
	if err != nil {
		t.Fatalf("the recorded events.jsonl is not loadable: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events.jsonl holds %d lines, want 2", len(events))
	}
	for i, event := range events {
		if event.Envelope.GetEventId() != accepted[i].GetEventId() {
			t.Errorf("line %d is %s, want %s", i+1, event.Envelope.GetEventId(), accepted[i].GetEventId())
		}
		if !event.ObservedAt.Equal(observed) {
			t.Errorf("line %d observedAt = %s, want the graph's %s", i+1, event.ObservedAt, observed)
		}
		if event.AppendedSeq != int64(i+1) {
			t.Errorf("line %d appendedSeq = %d, want %d", i+1, event.AppendedSeq, i+1)
		}
	}

	rejected, err := fixture.ReadRejected(filepath.Join(dir, record.RejectedFile))
	if err != nil {
		t.Fatalf("the recorded rejected.jsonl is not loadable: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Envelope.GetEventId() != "k8s:demo:bad-props-1" {
		t.Fatalf("rejected.jsonl = %v", rejected)
	}

	rejections := rec.Rejections()
	if len(rejections) != 1 || rejections[0].ReasonCode != feeder.ReasonTelemetryPayload {
		t.Fatalf("Rejections() = %+v", rejections)
	}
}

func TestEventLineIsCanonical(t *testing.T) {
	t.Parallel()
	d := testDescription()
	ev := feeder.UpsertNode(d, "k8s:demo:deploy:shop/checkout@rv1001", feeder.NodeFact{
		Meta:    feeder.Meta{Seq: 1001},
		Ref:     feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
		Type:    graphv1.NodeType_WORKLOAD,
		ValidAt: base(t),
	})
	line, err := record.EventLine(ev, base(t).Add(2*time.Second), 1)
	if err != nil {
		t.Fatalf("EventLine: %v", err)
	}
	got := string(line)
	// Keys sorted, so appendedSeq comes first and the log-added fields are plain JSON of the
	// documented shapes: an integer and an RFC 3339 string in protojson's spelling.
	if !strings.HasPrefix(got, `{"appendedSeq":1,`) {
		t.Errorf("line does not start with the sorted, log-added appendedSeq: %s", got)
	}
	if !strings.Contains(got, `"observedAt":"2026-09-01T13:00:02Z"`) {
		t.Errorf("line does not carry the observed time in canonical form: %s", got)
	}
	// 64-bit integers are JSON strings per the proto JSON mapping.
	if !strings.Contains(got, `"sourceSeq":"1001"`) {
		t.Errorf("sourceSeq is not a JSON string: %s", got)
	}
	if strings.Contains(got, "\n") {
		t.Error("a line contains a newline")
	}
}

// TestWriteManifest checks the one thing that matters about the skeleton: the fixture loader
// accepts it.
func TestWriteManifest(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "recorded-fixture-01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := base(t)

	// A payload index, so the clock is derived from it rather than stated.
	rec := record.Wrap(source.NewSliceSource([]feeder.Payload{
		{Kind: "deployments", At: at, Bytes: []byte(`{}`)},
		{Kind: "deployments", At: at.Add(90 * time.Minute), Bytes: []byte(`{}`)},
	}), dir)
	for {
		if _, err := rec.Next(t.Context()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, record.EventsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := record.WriteManifest(dir, record.Manifest{
		Family:      "baseline-topology",
		Description: "recorded by the SDK round-trip test",
		Sources:     []record.ManifestSource{record.SourceOf(testDescription())},
		ExpectRejected: []record.Rejection{
			{EventID: "k8s:demo:bad-props-1", ReasonCode: feeder.ReasonTelemetryPayload},
		},
	})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	m, err := fixture.LoadManifest(dir)
	if err != nil {
		t.Fatalf("the written manifest is not loadable: %v", err)
	}
	if m.ID != "recorded-fixture-01" {
		t.Errorf("id = %q, want the directory name", m.ID)
	}
	if m.HandAuthored {
		t.Error("a recording must not be marked hand_authored")
	}
	if len(m.Sources) != 1 || m.Sources[0].SourceID != "k8s:demo" || m.Sources[0].ReorderingWindow != time.Minute {
		t.Errorf("sources = %+v", m.Sources)
	}
	if !m.Clock.Start.Equal(at) || !m.Clock.End.Equal(at.Add(90*time.Minute)) {
		t.Errorf("clock = %s..%s, want it derived from the payload index", m.Clock.Start, m.Clock.End)
	}
	if m.SchemaVersion != feeder.SchemaVersion || m.SDKVersion != feeder.SDKVersion {
		t.Errorf("versions = %q / %q", m.SchemaVersion, m.SDKVersion)
	}
	if len(m.ExpectRejected) != 1 || m.ExpectRejected[0].ReasonCode != feeder.ReasonTelemetryPayload {
		t.Errorf("expect_rejected = %+v", m.ExpectRejected)
	}
}

func TestWriteManifestNeedsASource(t *testing.T) {
	t.Parallel()
	if err := record.WriteManifest(t.TempDir(), record.Manifest{ID: "x"}); err == nil {
		t.Error("a manifest with no sources was written")
	}
}

// batchingEmitter answers nothing, which is what a batching emitter does until it flushes.
type batchingEmitter struct{}

func (batchingEmitter) Emit(context.Context, *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	return nil, nil
}
func (batchingEmitter) Checkpoint(context.Context, feeder.CheckpointFact) error { return nil }
func (batchingEmitter) Flush(context.Context) error                             { return nil }

// TestEventRecorderRefusesABatchingEmitter is the live run's finding 1 as a test: recording
// through an emitter that answers later wrote nothing and said nothing, leaving a directory
// that looked like a fixture and replayed as an empty graph.
func TestEventRecorderRefusesABatchingEmitter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := testDescription()
	rec := record.Emitter(batchingEmitter{}, dir)

	_, err := rec.Emit(t.Context(), feeder.UpsertNode(d, "k8s:demo:deploy:shop/checkout@rv1", feeder.NodeFact{
		Ref:     feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
		Type:    graphv1.NodeType_WORKLOAD,
		ValidAt: base(t),
	}))
	if !errors.Is(err, record.ErrBatchingEmitter) {
		t.Fatalf("Emit returned %v, want ErrBatchingEmitter", err)
	}
	if !errors.Is(rec.Err(), record.ErrBatchingEmitter) {
		t.Errorf("Err() = %v, want the failure remembered", rec.Err())
	}
	if _, statErr := os.Stat(filepath.Join(dir, record.EventsFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("an events.jsonl was written for an event the graph never answered for")
	}
}

// The derived window covers the EVENTS as well as the payloads (004 T153).
//
// A feeder emits its events after the payload carrying them has arrived, and the last checkpoint after
// the stream has closed, so a window taken from the payload index alone ends just before the
// recording's own final events. Three committed recordings were cut short that way, by 50 ms to 1.1 s,
// and the pinned pass — which answers as known at clock.end — silently dropped those events.
func TestTheDerivedWindowCoversEventsObservedAfterTheLastPayload(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "recorded-fixture-02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := base(t)
	rec := record.Wrap(source.NewSliceSource([]feeder.Payload{
		{Kind: "deployments", At: at, Bytes: []byte(`{}`)},
	}), dir)
	for {
		if _, err := rec.Next(t.Context()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	// The checkpoint the feeder wrote after the stream closed: 300 ms after the only payload.
	lastEvent := at.Add(300 * time.Millisecond)
	line := `{"observedAt":"` + lastEvent.Format(time.RFC3339Nano) + `","appendedSeq":1}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, record.EventsFile), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "baseline-topology", Description: "window test",
		Sources: []record.ManifestSource{record.SourceOf(testDescription())},
	}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	m, err := fixture.LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Clock.End.Before(lastEvent) {
		t.Errorf("clock.end = %s, before the last event observed at %s: the window excludes the "+
			"recording's own checkpoint", m.Clock.End.Format(time.RFC3339Nano), lastEvent.Format(time.RFC3339Nano))
	}
	if !m.Clock.Start.Equal(at) {
		t.Errorf("clock.start = %s, want the first payload's arrival %s", m.Clock.Start, at)
	}
}

// Two sources recording into one directory, both with a payload kind called `deployments`, must each
// keep their bytes. The second recorder used to write `deployments/000001.json` over the first's, and
// the index then named one file twice (004 T113, deploy-cross-source-merge-01).
func TestTwoRecordersIntoOneDirectoryNeverOverwriteEachOther(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	first, second := record.Writer(dir), record.Writer(dir)
	if err := first.Record(feeder.Payload{Kind: "deployments", At: at, Bytes: []byte(`[{"id":4321}]`)}); err != nil {
		t.Fatal(err)
	}
	if err := second.Record(feeder.Payload{Kind: "deployments", At: at, Bytes: []byte(`{"deployments":[]}`)}); err != nil {
		t.Fatal(err)
	}

	src, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatalf("open recording: %v", err)
	}
	var bodies []string
	for {
		p, err := src.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		bodies = append(bodies, string(p.Bytes))
	}
	if len(bodies) != 2 || bodies[0] != `[{"id":4321}]` || bodies[1] != `{"deployments":[]}` {
		t.Fatalf("the recording replays %q; want both payloads, each once, in order", bodies)
	}
}
