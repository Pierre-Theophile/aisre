// SPDX-License-Identifier: Apache-2.0

package telemetry_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// Self-telemetry coverage (T085, FR-051, plan.md §"Observability of the tool itself").
//
// plan.md publishes seven instrument names. This test is the thing that keeps them from being
// seven names with no call site behind them: it drives one fixture through the real projector
// and the real query engine with a manual-reader meter provider attached, then asserts that
// every published name came back with at least one data point.
//
// `ambiguous-identity-01` is the fixture because it is the only one that exercises the whole
// list in one load: two sources (feeder lag), a certain rule that merges and a probable rule
// that only suggests (resolution decisions of kind suggest and confirm), a pair left pending
// (suggestions_pending), a human confirmation and a split, and a handful of events that the
// graph refuses (events_rejected_total).
//
// An instrument that stops being recorded fails here by name, which is the failure an operator
// would otherwise only discover as an empty dashboard panel during an incident.

const coverageFixture = "../../fixtures/ambiguous-identity-01"

// instruments is exactly plan.md's list. Adding one to the plan means adding it here.
var instruments = []string{
	telemetry.MetricEventsApplied,
	telemetry.MetricEventsRejected,
	telemetry.MetricEventApplyLatency,
	telemetry.MetricQueryLatency,
	telemetry.MetricFeederLag,
	telemetry.MetricResolutionDecisions,
	telemetry.MetricSuggestionsPending,
}

func TestEveryPublishedInstrumentIsRecorded(t *testing.T) {
	ctx := context.Background()
	store := pgtest.Open(t)

	// Spans are checked in the same pass: the global tracer provider is what every span in the
	// tool is started from (telemetry/trace.go), so a recorder installed here catches them all.
	spans := tracetest.NewSpanRecorder()
	previousTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(previousTracer) })

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	metrics, err := telemetry.NewMetrics(provider)
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Close() })

	proj := projector.New(store, projector.WithMetrics(metrics))
	proj.RegisterObservables(metrics)

	report, err := fixture.Load(ctx, proj, coverageFixture, fixture.LoadOptions{
		Principal: "sre-agent-dev|alice",
	})
	if err != nil {
		t.Fatalf("load %s: %v", coverageFixture, err)
	}
	if report.Applied == 0 {
		t.Fatal("fixture applied no events; the counters below would prove nothing")
	}

	// events_rejected_total needs the rejection path, which this fixture does not exercise on
	// its own, so one deliberately invalid event is submitted: a property carrying a metric
	// sample, which constitution IV refuses by name. It is the rejection an operator most wants
	// counted, and asserting the reason code keeps this from silently becoming some other one.
	rejected, err := proj.Apply(ctx, telemetryPayloadEvent(t), time.Time{})
	if err != nil {
		t.Fatalf("apply telemetry-payload event: %v", err)
	}
	if rejected.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Fatalf("telemetry-payload event: status = %s, want REJECTED", rejected.GetStatus())
	}

	// query_latency is recorded by the handler wrapper, so the query has to go through it —
	// calling the engine directly would leave the instrument empty and the test dishonest.
	engine := query.NewEngine(store)
	err = metrics.TimeQuery(ctx, "Suggestions", func() error {
		_, qerr := engine.Suggestions(ctx, &graphv1.SuggestionsRequest{})
		return qerr
	})
	if err != nil {
		t.Fatalf("Suggestions: %v", err)
	}
	err = metrics.TimeQuery(ctx, "Subgraph", func() error {
		_, qerr := engine.Subgraph(ctx, &graphv1.SubgraphRequest{
			Focus: graph.Ref{Namespace: "otel.service.name", Value: "checkout"}.Proto(),
			AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(mustTime(t, "2026-09-01T15:05:00Z"))},
			Hops:  2,
		})
		return qerr
	})
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	recorded := index(collected)

	for _, name := range instruments {
		if _, ok := recorded[name]; !ok {
			t.Errorf("%s: no data point; plan.md publishes this instrument but nothing records it", name)
		}
	}

	// The two that must say something specific, not merely exist.
	if pending := gaugeValue(t, recorded, telemetry.MetricSuggestionsPending); pending <= 0 {
		t.Errorf("%s = %d, want > 0: %s leaves a probable match awaiting a decision",
			telemetry.MetricSuggestionsPending, pending, coverageFixture)
	}
	for _, kind := range []string{"suggest", "confirm"} {
		if !hasAttr(recorded, telemetry.MetricResolutionDecisions, telemetry.AttrKind, kind) {
			t.Errorf("%s has no data point with %s=%s", telemetry.MetricResolutionDecisions,
				telemetry.AttrKind, kind)
		}
	}
	if !hasAttrKey(recorded, telemetry.MetricFeederLag, telemetry.AttrSource) {
		t.Errorf("%s carries no %s attribute; lag must be reported per source",
			telemetry.MetricFeederLag, telemetry.AttrSource)
	}
	for _, rpc := range []string{"Subgraph", "Suggestions"} {
		if !hasAttr(recorded, telemetry.MetricQueryLatency, telemetry.AttrRPC, rpc) {
			t.Errorf("%s has no data point with %s=%s", telemetry.MetricQueryLatency,
				telemetry.AttrRPC, rpc)
		}
	}

	assertSpans(t, spans.Ended())
}

// assertSpans checks the span half of plan.md §Observability: one span per applied event, one
// per RPC. (`feeder.window` and `feeder.sync` belong to the two connectors and are exercised by
// their own conformance runs; nothing in a fixture load closes a window.)
func assertSpans(t *testing.T, ended []sdktrace.ReadOnlySpan) {
	t.Helper()

	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, span := range ended {
		byName[span.Name()] = append(byName[span.Name()], span)
	}

	applies := byName[telemetry.SpanProjectorApply]
	if len(applies) == 0 {
		t.Fatalf("no %s span was recorded; every applied event must produce one",
			telemetry.SpanProjectorApply)
	}
	for _, want := range []string{telemetry.SpanAttrEventType, telemetry.SpanAttrSourceID, telemetry.SpanAttrResult} {
		if !spanHasAttr(applies, want) {
			t.Errorf("%s spans carry no %s attribute", telemetry.SpanProjectorApply, want)
		}
	}

	for _, rpc := range []string{"Subgraph", "Suggestions"} {
		name := telemetry.SpanQueryPrefix + rpc
		if len(byName[name]) == 0 {
			t.Errorf("no %s span was recorded; every query RPC must produce one", name)
		}
	}
}

func spanHasAttr(spans []sdktrace.ReadOnlySpan, key string) bool {
	for _, span := range spans {
		for _, kv := range span.Attributes() {
			if string(kv.Key) == key {
				return true
			}
		}
	}
	return false
}

// index collects every instrument that produced at least one data point, by name.
func index(rm metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	out := map[string]metricdata.Metrics{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if points(m) == 0 {
				continue
			}
			out[m.Name] = m
		}
	}
	return out
}

func points(m metricdata.Metrics) int {
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		return len(data.DataPoints)
	case metricdata.Sum[float64]:
		return len(data.DataPoints)
	case metricdata.Gauge[int64]:
		return len(data.DataPoints)
	case metricdata.Gauge[float64]:
		return len(data.DataPoints)
	case metricdata.Histogram[float64]:
		return len(data.DataPoints)
	case metricdata.Histogram[int64]:
		return len(data.DataPoints)
	default:
		return 0
	}
}

func gaugeValue(t *testing.T, recorded map[string]metricdata.Metrics, name string) int64 {
	t.Helper()
	m, ok := recorded[name]
	if !ok {
		return 0
	}
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok || len(gauge.DataPoints) == 0 {
		return 0
	}
	return gauge.DataPoints[0].Value
}

// hasAttr reports whether any of the instrument's data points carries key=value.
func hasAttr(recorded map[string]metricdata.Metrics, name, key, value string) bool {
	return anyAttr(recorded, name, func(k, v string) bool { return k == key && v == value })
}

// hasAttrKey reports whether any data point carries the key at all.
func hasAttrKey(recorded map[string]metricdata.Metrics, name, key string) bool {
	return anyAttr(recorded, name, func(k, _ string) bool { return k == key })
}

func anyAttr(recorded map[string]metricdata.Metrics, name string, match func(key, value string) bool) bool {
	m, ok := recorded[name]
	if !ok {
		return false
	}
	var sets []map[string]string
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range data.DataPoints {
			sets = append(sets, attrs(dp.Attributes.ToSlice()))
		}
	case metricdata.Gauge[float64]:
		for _, dp := range data.DataPoints {
			sets = append(sets, attrs(dp.Attributes.ToSlice()))
		}
	case metricdata.Gauge[int64]:
		for _, dp := range data.DataPoints {
			sets = append(sets, attrs(dp.Attributes.ToSlice()))
		}
	case metricdata.Histogram[float64]:
		for _, dp := range data.DataPoints {
			sets = append(sets, attrs(dp.Attributes.ToSlice()))
		}
	}
	for _, set := range sets {
		for k, v := range set {
			if match(k, v) {
				return true
			}
		}
	}
	return false
}

func attrs(kvs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return parsed
}

// telemetryPayloadEvent is one event the graph must refuse: a node property carrying a metric
// sample rather than a pointer to one (constitution IV, FR-009).
func telemetryPayloadEvent(t *testing.T) *graphv1.EventEnvelope {
	t.Helper()
	props, err := structpb.NewStruct(map[string]any{"metric_value": 0.42})
	if err != nil {
		t.Fatalf("build props: %v", err)
	}
	return &graphv1.EventEnvelope{
		EventId:       "otel:shop:coverage-telemetry-payload",
		SourceId:      "otel:shop",
		SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         graph.Ref{Namespace: "otel.service.name", Value: "checkout"}.Proto(),
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: "checkout",
			Props:       props,
			ValidAt:     timestamppb.New(mustTime(t, "2026-09-01T15:05:00Z")),
		}},
	}
}
