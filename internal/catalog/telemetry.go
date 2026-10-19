// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// The attributor's own telemetry (009 T038, constitution's self-observability gate): how many
// decisions each layer takes, how many leave a signal `unknown`, and what the optional classifier
// costs and how often it falls back. Instruments come from the global providers Setup installs; a
// process that installs none gets the no-op ones.

// InstrumentationName names the attributor's tracer and meter.
const InstrumentationName = "github.com/Pierre-Theophile/aisre/internal/catalog"

// Metric names.
const (
	MetricDecisions          = "sre.attribution.decisions"
	MetricClassifierDuration = "sre.attribution.classifier.duration"
	MetricClassifierFallback = "sre.attribution.classifier.fallbacks"
)

type telemetry struct {
	tracer             trace.Tracer
	decisions          metric.Int64Counter
	classifierDuration metric.Float64Histogram
	classifierFallback metric.Int64Counter
}

// WithMeterProvider and WithTracerProvider replace the global providers, for tests.
func WithMeterProvider(p metric.MeterProvider) Option {
	return func(a *Attributor) { a.meterProvider = p }
}

// WithTracerProvider replaces the global tracer provider.
func WithTracerProvider(p trace.TracerProvider) Option {
	return func(a *Attributor) { a.tracerProvider = p }
}

func newTelemetry(mp metric.MeterProvider, tp trace.TracerProvider) telemetry {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	meter := mp.Meter(InstrumentationName)
	t := telemetry{tracer: tp.Tracer(InstrumentationName)}
	// An instrument that fails to register is a no-op one: telemetry never stops attribution.
	t.decisions, _ = meter.Int64Counter(MetricDecisions,
		metric.WithDescription("Attribution decisions appended, by the layer that decided (unknown when none did)."),
		metric.WithUnit("{decision}"))
	t.classifierDuration, _ = meter.Float64Histogram(MetricClassifierDuration,
		metric.WithDescription("Wall time of one classifier call, in seconds."),
		metric.WithUnit("s"))
	t.classifierFallback, _ = meter.Int64Counter(MetricClassifierFallback,
		metric.WithDescription("Classifier calls that failed or timed out, leaving the decision to layers 0-2."),
		metric.WithUnit("{call}"))
	return t
}

func (t telemetry) decided(ctx context.Context, layer string) {
	if t.decisions != nil {
		t.decisions.Add(ctx, 1, metric.WithAttributes(attribute.String("layer", layer)))
	}
}

func (t telemetry) classified(ctx context.Context, classifier string, took time.Duration, failed bool) {
	attrs := metric.WithAttributes(attribute.String("classifier", classifier))
	if t.classifierDuration != nil {
		t.classifierDuration.Record(ctx, took.Seconds(), attrs)
	}
	if failed && t.classifierFallback != nil {
		t.classifierFallback.Add(ctx, 1, attrs)
	}
}
