// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

func TestSetupWithoutEndpointSucceedsAndShutsDownCleanly(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envProtocol, "")

	shutdown, err := Setup(context.Background(), Config{
		ServiceVersion: "0.1.0-test",
		Environment:    "test",
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Instrumentation must work with no collector: a span is created and dropped.
	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Shutdown is idempotent, because a process can stop through more than one path.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

func TestSetupHonoursPropagators(t *testing.T) {
	t.Setenv(envEndpoint, "")
	shutdown, err := Setup(context.Background(), Config{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	fields := otel.GetTextMapPropagator().Fields()
	var hasTraceparent, hasBaggage bool
	for _, f := range fields {
		switch f {
		case "traceparent":
			hasTraceparent = true
		case "baggage":
			hasBaggage = true
		}
	}
	if !hasTraceparent || !hasBaggage {
		t.Fatalf("propagator fields = %v, want traceparent and baggage", fields)
	}
}

func TestSetupRejectsUnknownProtocol(t *testing.T) {
	t.Setenv(envEndpoint, "http://127.0.0.1:4318")
	t.Setenv(envProtocol, "carrier-pigeon")
	if _, err := Setup(context.Background(), Config{Logger: discardLogger()}); err == nil {
		t.Fatal("want an error for an unsupported OTLP protocol")
	}
}

func TestConfigResolution(t *testing.T) {
	t.Setenv(envEndpoint, "http://collector:4317")
	t.Setenv(envProtocol, "http/protobuf")
	t.Setenv(envHeaders, "x-key=abc, x-other = def")
	t.Setenv(envService, "from-env")

	cfg := Config{}
	if got := cfg.endpoint(); got != "http://collector:4317" {
		t.Errorf("endpoint = %q", got)
	}
	protocol, err := cfg.protocol()
	if err != nil || protocol != ProtocolHTTP {
		t.Errorf("protocol = %q, %v", protocol, err)
	}
	if got := cfg.headers(); got["x-key"] != "abc" || got["x-other"] != "def" {
		t.Errorf("headers = %v", got)
	}
	if got := cfg.serviceName(); got != "from-env" {
		t.Errorf("serviceName = %q, want the OTEL_SERVICE_NAME value", got)
	}

	explicit := Config{ServiceName: "sre-agent", Endpoint: "localhost:4317", Protocol: ProtocolGRPC}
	if got := explicit.endpoint(); got != "localhost:4317" {
		t.Errorf("explicit endpoint = %q", got)
	}
	if got := explicit.serviceName(); got != DefaultServiceName {
		t.Errorf("explicit serviceName = %q", got)
	}
	if protocol, err := explicit.protocol(); err != nil || protocol != ProtocolGRPC {
		t.Errorf("explicit protocol = %q, %v", protocol, err)
	}
}

func TestDefaultServiceName(t *testing.T) {
	t.Setenv(envService, "")
	if got := (Config{}).serviceName(); got != DefaultServiceName {
		t.Fatalf("serviceName = %q, want %q", got, DefaultServiceName)
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

func TestMetricsRecordEveryInstrument(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })

	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	m.EventApplied(ctx, "upsert_node", "k8s:prod")
	m.EventApplied(ctx, "upsert_edge", "otel:demo")
	m.EventRejected(ctx, "MISSING_VALID_FROM")
	m.ObserveEventApply(ctx, "upsert_node", "k8s:prod", 3*time.Millisecond)
	m.ObserveQuery(ctx, "Subgraph", 120*time.Millisecond)
	m.ResolutionDecision(ctx, "confirm", "sre-agent-dev|alice")
	m.SetFeederLag("k8s:prod", 2500*time.Millisecond)
	m.RegisterFeederLag(func(context.Context) ([]FeederLag, error) {
		return []FeederLag{{Source: "otel:demo", Seconds: 0.5}}, nil
	})
	m.RegisterSuggestionsPending(func(context.Context) (int64, error) { return 7, nil })

	if err := m.TimeQuery(ctx, "Diff", func() error { return nil }); err != nil {
		t.Fatalf("TimeQuery: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := map[string]metricdata.Aggregation{}
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			got[metric.Name] = metric.Data
		}
	}
	for _, name := range []string{
		MetricEventsApplied, MetricEventsRejected, MetricEventApplyLatency,
		MetricQueryLatency, MetricFeederLag, MetricResolutionDecisions, MetricSuggestionsPending,
	} {
		if _, ok := got[name]; !ok {
			t.Errorf("instrument %q not collected", name)
		}
	}

	if sum, ok := got[MetricEventsApplied].(metricdata.Sum[int64]); ok {
		if len(sum.DataPoints) != 2 {
			t.Errorf("%s: %d series, want 2 (one per type/source pair)", MetricEventsApplied, len(sum.DataPoints))
		}
	} else {
		t.Errorf("%s: unexpected aggregation %T", MetricEventsApplied, got[MetricEventsApplied])
	}

	if hist, ok := got[MetricQueryLatency].(metricdata.Histogram[float64]); ok {
		if len(hist.DataPoints) != 2 {
			t.Errorf("%s: %d series, want one per rpc", MetricQueryLatency, len(hist.DataPoints))
		}
	} else {
		t.Errorf("%s: unexpected aggregation %T", MetricQueryLatency, got[MetricQueryLatency])
	}

	if gauge, ok := got[MetricFeederLag].(metricdata.Gauge[float64]); ok {
		if len(gauge.DataPoints) != 2 {
			t.Errorf("%s: %d series, want push and pull sources", MetricFeederLag, len(gauge.DataPoints))
		}
	} else {
		t.Errorf("%s: unexpected aggregation %T", MetricFeederLag, got[MetricFeederLag])
	}

	if gauge, ok := got[MetricSuggestionsPending].(metricdata.Gauge[int64]); ok {
		if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != 7 {
			t.Errorf("%s: %+v, want a single value of 7", MetricSuggestionsPending, gauge.DataPoints)
		}
	} else {
		t.Errorf("%s: unexpected aggregation %T", MetricSuggestionsPending, got[MetricSuggestionsPending])
	}
}

func TestMetricsObservableCallbackErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })

	m, err := NewMetrics(provider)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	sentinel := errors.New("database unavailable")
	m.RegisterSuggestionsPending(func(context.Context) (int64, error) { return 0, sentinel })

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); !errors.Is(err, sentinel) {
		t.Fatalf("Collect err = %v, want the callback's error", err)
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	ctx := context.Background()
	m.EventApplied(ctx, "t", "s")
	m.EventRejected(ctx, "r")
	m.ObserveEventApply(ctx, "t", "s", time.Second)
	m.ObserveQuery(ctx, "Subgraph", time.Second)
	m.ResolutionDecision(ctx, "confirm", "who")
	m.SetFeederLag("s", time.Second)
	m.RegisterFeederLag(func(context.Context) ([]FeederLag, error) { return nil, nil })
	m.RegisterSuggestionsPending(func(context.Context) (int64, error) { return 0, nil })
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

func TestLoggerInjectsTraceAndSpanIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LogConfig{Output: &buf, Level: slog.LevelDebug})

	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "apply-event")
	logger.InfoContext(ctx, "applied event", "event_id", "evt-1")
	span.End()

	record := decodeLogLine(t, buf.Bytes())
	if got, want := record[LogKeyTraceID], span.SpanContext().TraceID().String(); got != want {
		t.Errorf("%s = %v, want %q", LogKeyTraceID, got, want)
	}
	if got, want := record[LogKeySpanID], span.SpanContext().SpanID().String(); got != want {
		t.Errorf("%s = %v, want %q", LogKeySpanID, got, want)
	}
	if record["event_id"] != "evt-1" {
		t.Errorf("record lost its own attributes: %v", record)
	}
}

func TestLoggerOmitsTraceIDsOutsideASpan(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LogConfig{Output: &buf})
	logger.Info("no span here")

	record := decodeLogLine(t, buf.Bytes())
	if _, ok := record[LogKeyTraceID]; ok {
		t.Errorf("unexpected %s outside a span: %v", LogKeyTraceID, record)
	}
}

func TestLoggerAttrsAndGroupsKeepTraceCorrelation(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LogConfig{
		Output: &buf,
		Attrs:  []slog.Attr{slog.String("service", DefaultServiceName)},
	}).With("component", "projector")

	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, span := provider.Tracer("test").Start(context.Background(), "apply-event")
	logger.WarnContext(ctx, "conflicting sources")
	span.End()

	record := decodeLogLine(t, buf.Bytes())
	if record["service"] != DefaultServiceName || record["component"] != "projector" {
		t.Errorf("attrs lost: %v", record)
	}
	if record[LogKeyTraceID] != span.SpanContext().TraceID().String() {
		t.Errorf("trace correlation lost after With: %v", record)
	}
}

func TestLoggerHonoursLevelAndFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LogConfig{Output: &buf, Level: slog.LevelWarn})
	logger.Info("filtered out")
	if buf.Len() != 0 {
		t.Fatalf("info logged below the configured level: %s", buf.String())
	}
	logger.Warn("kept")
	if !bytes.Contains(buf.Bytes(), []byte(`"msg":"kept"`)) {
		t.Fatalf("want JSON output, got: %s", buf.String())
	}

	buf.Reset()
	NewLogger(LogConfig{Output: &buf, Format: LogFormatText}).Warn("kept")
	if !bytes.Contains(buf.Bytes(), []byte("msg=kept")) {
		t.Fatalf("want text output, got: %s", buf.String())
	}
}

func TestParseLogLevel(t *testing.T) {
	level, err := ParseLogLevel(" debug ")
	if err != nil || level != slog.LevelDebug {
		t.Fatalf("got %v, %v", level, err)
	}
	if _, err := ParseLogLevel("chatty"); err == nil {
		t.Fatal("want an error for an unknown level")
	}
}

// ---------------------------------------------------------------------------

func decodeLogLine(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		t.Fatalf("no log output")
	}
	var record map[string]any
	if err := json.Unmarshal(lines[0], &record); err != nil {
		t.Fatalf("decode log line %q: %v", lines[0], err)
	}
	return record
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}
