// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// InstrumentationName is the instrumentation scope for every instrument in this package.
const InstrumentationName = "github.com/Pierre-Theophile/aisre/internal/telemetry"

// Instrument names, exactly as published in plan.md §"Observability of the tool itself".
// They are part of the operator-facing contract: dashboards and alerts are written against
// these names, so they change only with the plan.
const (
	MetricEventsApplied       = "events_applied_total"
	MetricEventsRejected      = "events_rejected_total"
	MetricEventApplyLatency   = "event_apply_latency"
	MetricQueryLatency        = "query_latency"
	MetricFeederLag           = "feeder_lag_seconds"
	MetricResolutionDecisions = "resolution_decisions_total"
	MetricSuggestionsPending  = "suggestions_pending"
)

// Attribute keys carried by the instruments above.
const (
	AttrType   = "type"
	AttrSource = "source"
	AttrReason = "reason"
	AttrRPC    = "rpc"
	AttrKind   = "kind"
	AttrBy     = "by"
)

// latencyBuckets are explicit histogram boundaries in seconds. They straddle the range that
// matters for this tool: sub-millisecond event application at one end, the multi-second query
// budget at the other.
var latencyBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// FeederLag is one source's ingestion lag, reported by the feeder_lag_seconds observable.
type FeederLag struct {
	// Source is the feeder's `source_id`, e.g. "k8s:prod" or "otel:demo".
	Source string
	// Seconds is how far behind the source the graph currently is.
	Seconds float64
}

// FeederLagFunc reports the current lag of every source it knows about. It is called on every
// metric collection, so it must be cheap and must not block.
type FeederLagFunc func(ctx context.Context) ([]FeederLag, error)

// SuggestionsPendingFunc reports how many resolution suggestions are awaiting a human
// decision (FR-033). It is called on every metric collection.
type SuggestionsPendingFunc func(ctx context.Context) (int64, error)

// Metrics holds the instruments listed in plan.md §"Observability of the tool itself" and
// satisfies FR-051 for the graph and both feeders. One instance is created at startup and
// passed to the ingestion, query and resolution layers.
//
// The zero value is not usable; call NewMetrics. A nil *Metrics is safe: every method is a
// no-op on it, so a component can be constructed without telemetry in unit tests.
type Metrics struct {
	eventsApplied       metric.Int64Counter
	eventsRejected      metric.Int64Counter
	eventApplyLatency   metric.Float64Histogram
	queryLatency        metric.Float64Histogram
	feederLag           metric.Float64ObservableGauge
	resolutionDecisions metric.Int64Counter
	suggestionsPending  metric.Int64ObservableGauge

	registration metric.Registration

	mu             sync.RWMutex
	lagBySource    map[string]float64
	lagFuncs       []FeederLagFunc
	suggestionFunc SuggestionsPendingFunc
}

// NewMetrics creates every instrument on provider and registers the callback that drives the
// two observable gauges. Pass nil to use the global meter provider installed by Setup.
func NewMetrics(provider metric.MeterProvider) (*Metrics, error) {
	if provider == nil {
		provider = otel.GetMeterProvider()
	}
	meter := provider.Meter(InstrumentationName)
	m := &Metrics{lagBySource: map[string]float64{}}

	var err error
	if m.eventsApplied, err = meter.Int64Counter(
		MetricEventsApplied,
		metric.WithDescription("Events successfully applied to the graph, by event type and source."),
		metric.WithUnit("{event}"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricEventsApplied, err)
	}
	if m.eventsRejected, err = meter.Int64Counter(
		MetricEventsRejected,
		metric.WithDescription("Events rejected at ingestion, by rejection reason."),
		metric.WithUnit("{event}"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricEventsRejected, err)
	}
	if m.eventApplyLatency, err = meter.Float64Histogram(
		MetricEventApplyLatency,
		metric.WithDescription("Wall time to apply one event to the graph, in seconds."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(latencyBuckets...),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricEventApplyLatency, err)
	}
	if m.queryLatency, err = meter.Float64Histogram(
		MetricQueryLatency,
		metric.WithDescription("Wall time to serve one query RPC, in seconds."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(latencyBuckets...),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricQueryLatency, err)
	}
	if m.resolutionDecisions, err = meter.Int64Counter(
		MetricResolutionDecisions,
		metric.WithDescription("Entity-resolution decisions recorded, by kind and by whom (rule or principal)."),
		metric.WithUnit("{decision}"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricResolutionDecisions, err)
	}
	if m.feederLag, err = meter.Float64ObservableGauge(
		MetricFeederLag,
		metric.WithDescription("How far behind its source each feeder currently is, in seconds."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricFeederLag, err)
	}
	if m.suggestionsPending, err = meter.Int64ObservableGauge(
		MetricSuggestionsPending,
		metric.WithDescription("Entity-resolution suggestions awaiting a human decision."),
		metric.WithUnit("{suggestion}"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: %s: %w", MetricSuggestionsPending, err)
	}

	m.registration, err = meter.RegisterCallback(m.observe, m.feederLag, m.suggestionsPending)
	if err != nil {
		return nil, fmt.Errorf("telemetry: register observable callback: %w", err)
	}
	return m, nil
}

// Close unregisters the observable callback. Call it when the meter provider outlives the
// component that owns these instruments; Setup's shutdown covers the usual case.
func (m *Metrics) Close() error {
	if m == nil || m.registration == nil {
		return nil
	}
	return m.registration.Unregister()
}

// observe is the single callback behind both observable gauges.
func (m *Metrics) observe(ctx context.Context, obs metric.Observer) error {
	m.mu.RLock()
	lagFuncs := make([]FeederLagFunc, len(m.lagFuncs))
	copy(lagFuncs, m.lagFuncs)
	pushed := make(map[string]float64, len(m.lagBySource))
	for source, seconds := range m.lagBySource {
		pushed[source] = seconds
	}
	suggestionFunc := m.suggestionFunc
	m.mu.RUnlock()

	for source, seconds := range pushed {
		obs.ObserveFloat64(m.feederLag, seconds, metric.WithAttributes(attribute.String(AttrSource, source)))
	}
	for _, fn := range lagFuncs {
		lags, err := fn(ctx)
		if err != nil {
			return err
		}
		for _, lag := range lags {
			obs.ObserveFloat64(m.feederLag, lag.Seconds,
				metric.WithAttributes(attribute.String(AttrSource, lag.Source)))
		}
	}
	if suggestionFunc != nil {
		pending, err := suggestionFunc(ctx)
		if err != nil {
			return err
		}
		obs.ObserveInt64(m.suggestionsPending, pending)
	}
	return nil
}

// RegisterFeederLag adds a pull-style source of feeder lag, consulted on every collection.
// Use it when the lag can be computed on demand (for example from `log.checkpoints`).
func (m *Metrics) RegisterFeederLag(fn FeederLagFunc) {
	if m == nil || fn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lagFuncs = append(m.lagFuncs, fn)
}

// SetFeederLag records the current lag of one source, push-style. Use it from inside a feeder
// loop, where the lag is known as a side effect of the work.
func (m *Metrics) SetFeederLag(source string, lag time.Duration) {
	if m == nil || source == "" {
		return
	}
	// A feeder may assert coverage slightly ahead of the wall clock — a window closed at its
	// end instant, a checkpoint dated to the future edge of a batch. Negative lag is not a
	// thing an operator can act on, so it is reported as caught up.
	seconds := lag.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lagBySource[source] = seconds
}

// RegisterSuggestionsPending installs the callback behind the suggestions_pending gauge. The
// last registration wins, because there is exactly one pending-suggestion count.
func (m *Metrics) RegisterSuggestionsPending(fn SuggestionsPendingFunc) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suggestionFunc = fn
}

// EventApplied counts one event applied to the graph.
func (m *Metrics) EventApplied(ctx context.Context, eventType, source string) {
	if m == nil {
		return
	}
	m.eventsApplied.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrType, eventType),
		attribute.String(AttrSource, source),
	))
}

// EventRejected counts one event refused at ingestion. reason is the machine-readable
// rejection code returned to the feeder, never free text.
func (m *Metrics) EventRejected(ctx context.Context, reason string) {
	if m == nil {
		return
	}
	m.eventsRejected.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrReason, reason)))
}

// ObserveEventApply records how long applying one event took.
func (m *Metrics) ObserveEventApply(ctx context.Context, eventType, source string, d time.Duration) {
	if m == nil {
		return
	}
	m.eventApplyLatency.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(AttrType, eventType),
		attribute.String(AttrSource, source),
	))
}

// ObserveQuery records how long one query RPC took. rpc is the procedure's short name, e.g.
// "Subgraph" or "Diff".
func (m *Metrics) ObserveQuery(ctx context.Context, rpc string, d time.Duration) {
	if m == nil {
		return
	}
	m.queryLatency.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String(AttrRPC, rpc)))
}

// ResolutionDecision counts one entity-resolution decision. kind is confirm, reject, merge or
// split; by is "rule:<id>" for an automated merge or the principal key (`iss|sub`) for a human
// decision, so that the metric matches what the decision record stores (FR-038, FR-041).
func (m *Metrics) ResolutionDecision(ctx context.Context, kind, by string) {
	if m == nil {
		return
	}
	m.resolutionDecisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKind, kind),
		attribute.String(AttrBy, by),
	))
}

// TimeQuery is the one call every QueryService handler wraps its engine call in: it starts the
// per-RPC span (`query.<rpc>`, plan.md §Observability) and records query_latency{rpc}. It
// returns fn's error unchanged.
//
// Span and metric are started together on purpose. They are the two halves of the same
// question — "which RPC was slow, and on what request?" — and a handler that remembered one
// and forgot the other is the failure mode this method exists to make impossible. It works on
// a nil *Metrics: the span is still recorded, because tracing is configured by the global
// provider and not by this struct.
func (m *Metrics) TimeQuery(ctx context.Context, rpc string, fn func() error) error {
	ctx, span := StartSpan(ctx, SpanQueryPrefix+rpc, attribute.String(SpanAttrRPC, rpc))
	defer span.End()

	start := time.Now()
	err := fn()
	m.ObserveQuery(ctx, rpc, time.Since(start))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
	}
	return err
}
