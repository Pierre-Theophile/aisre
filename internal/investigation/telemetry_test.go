// SPDX-License-Identifier: Apache-2.0

package investigation_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Pierre-Theophile/aisre/internal/investigation"
)

// Self-telemetry coverage (T092, FR-065, plan §Observability and cost accounting).
//
// plan.md publishes eleven instrument names and five span names. This test is what keeps them
// from being sixteen names with no call site behind them: it drives every recording method and
// every span helper, then asserts that each published name came back with at least one data
// point.
//
// An instrument that stops being recorded fails here by name, which is the failure an operator
// would otherwise only discover as an empty dashboard panel during an incident.

func TestEveryPublishedInstrumentIsRecorded(t *testing.T) {
	ctx := context.Background()

	spans := tracetest.NewSpanRecorder()
	previousTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(previousTracer) })

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })

	metrics, err := investigation.NewMetrics(provider)
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}

	// One of everything, in the order an investigation produces it.
	metrics.ObservePhase(ctx, "page", investigation.PhaseProvisional, 900*time.Millisecond)
	metrics.ObservePhase(ctx, "page", investigation.PhaseFirstTested, 12*time.Second)
	metrics.ObservePhase(ctx, "page", investigation.PhaseConclusion, 95*time.Second)
	metrics.WorkerCall(ctx, "metrics", "errors_by_version", "recorded", "answered", 40*time.Millisecond)
	metrics.ModelTokens(ctx, "claude-fable-5-1", 12_000, 4_000, 8_000, 900)
	metrics.Cost(ctx, "claude-fable-5-1", 0.184)
	metrics.QuotaShare(ctx, "metrics-vendor", 0.02)
	metrics.LedgerUpdate(ctx, "supports")
	metrics.StopReason(ctx, "completed")
	metrics.ReplayDivergence(ctx, "trajectory")
	metrics.NotRecorded(ctx, "rollout-regression-01-incident")
	metrics.InvestigationCompleted(ctx, "ranked", "page")

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	recorded := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			recorded[m.Name] = true
		}
	}
	for _, name := range investigation.Instruments() {
		if !recorded[name] {
			t.Errorf("instrument %q is published in plan.md §Observability and has no call site "+
				"behind it", name)
		}
	}

	// Cost is recorded twice, and both halves must be present: raw token counts per model and
	// class, and the derived monetary figure (FR-048). One without the other is either a number
	// nobody can aggregate or a number nobody can reinterpret after a price change.
	if !recorded[investigation.MetricModelTokens] || !recorded[investigation.MetricInvestigationCost] {
		t.Error("cost accounting is incomplete: FR-048 requires both the token counts and the " +
			"derived monetary figure")
	}
}

func TestEveryPublishedSpanIsStarted(t *testing.T) {
	ctx := context.Background()

	spans := tracetest.NewSpanRecorder()
	previousTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(previousTracer) })

	invCtx, invSpan := investigation.StartInvestigation(ctx, "inv-01", "inc-01", "page")
	turnCtx, turnSpan := investigation.StartTurn(invCtx, "inv-01", 1)
	_, workerSpan := investigation.StartWorkerCall(turnCtx, "metrics", "errors_by_version",
		"recorded", "h-rev7", "did rev7 raise the error rate?")
	_, modelSpan := investigation.StartModelCall(turnCtx, "investigator", "claude-fable-5-1")
	_, termSpan := investigation.StartAlgebraTerm(turnCtx, "errors_by_version", "sha256:abc")
	termSpan.End()
	modelSpan.End()
	workerSpan.End()
	turnSpan.End()
	invSpan.End()

	started := map[string]bool{}
	for _, s := range spans.Ended() {
		started[s.Name()] = true
	}
	for _, name := range []string{
		investigation.SpanInvestigation, investigation.SpanTurn, investigation.SpanWorkerCall,
		investigation.SpanModelCall, investigation.SpanAlgebraTerm,
	} {
		if !started[name] {
			t.Errorf("span %q is published in plan.md §Observability and is never started", name)
		}
	}
}

// TestNilMetricsIsSafe is what lets the engine be constructed in a unit test with no meter
// provider at all: every method is a no-op on a nil *Metrics.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *investigation.Metrics
	ctx := context.Background()
	m.InvestigationCompleted(ctx, "ranked", "page")
	m.ObservePhase(ctx, "page", investigation.PhaseConclusion, time.Second)
	m.WorkerCall(ctx, "metrics", "compare", "live", "answered", time.Second)
	m.ModelTokens(ctx, "m", 1, 1, 1, 1)
	m.Cost(ctx, "m", 1)
	m.QuotaShare(ctx, "b", 0.1)
	m.LedgerUpdate(ctx, "supports")
	m.StopReason(ctx, "completed")
	m.ReplayDivergence(ctx, "world")
	m.NotRecorded(ctx, "fixture")
}
