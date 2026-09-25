// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The span vocabulary (plan.md §"Observability of the tool itself", FR-051, research §15).
//
// plan.md names three spans and this file is where their names live, for the same reason the
// instrument names live in metrics.go: a trace query written against `projector.apply` is an
// operator-facing contract, and a span renamed in a refactor silently empties a dashboard.
//
// Every span is started through a tracer taken from the *global* provider. That is deliberate:
// telemetry.Setup installs the provider, and code that runs before it — a fixture load, a unit
// test, `aisre fixture bench` — must behave identically with no provider at all. The OTel
// global tracer delegates to the real provider as soon as one is installed, so caching the
// tracer once is safe and keeps the hot path (one span per applied event) off the global
// registry lock.

// Span names, exactly as published in plan.md. They change only with the plan.
const (
	// SpanProjectorApply covers one event applied to the graph: validation, append and
	// projection, inside the transaction that commits them together.
	SpanProjectorApply = "projector.apply"
	// SpanFeederWindow covers one aggregation window closed and emitted by a windowing feeder
	// (the OTLP one).
	SpanFeederWindow = "feeder.window"
	// SpanFeederSync covers one list/sync batch of a polling feeder (the Kubernetes one).
	SpanFeederSync = "feeder.sync"
	// SpanQueryPrefix prefixes the per-RPC query span, e.g. "query.Subgraph". The RPC's short
	// name is also carried as the `rpc` attribute, so a backend can group on either.
	SpanQueryPrefix = "query."
	// SpanResolutionPrefix prefixes the per-RPC span of a human resolution decision, e.g.
	// "resolution.confirm". The decision it produces gets its own `projector.apply` span
	// underneath, so a trace shows the RPC and the event it became.
	SpanResolutionPrefix = "resolution."
)

// Span attribute keys. The `sre.` prefix keeps them clear of the OpenTelemetry semantic
// conventions, which own the unprefixed namespace.
const (
	// SpanAttrEventID is the event's globally unique id.
	SpanAttrEventID = "sre.event.id"
	// SpanAttrEventType is the event's body type, e.g. "upsert_node".
	SpanAttrEventType = "sre.event.type"
	// SpanAttrSourceID is the feeder that produced the event, or whose window is closing.
	SpanAttrSourceID = "sre.source.id"
	// SpanAttrResult is the three-way ingestion outcome: APPLIED, DUPLICATE_NOOP or REJECTED.
	SpanAttrResult = "sre.apply.result"
	// SpanAttrReason is the machine-readable rejection code, set only on a rejection.
	SpanAttrReason = "sre.reject.reason"
	// SpanAttrRPC is the query procedure's short name, e.g. "Subgraph".
	SpanAttrRPC = "sre.rpc"
	// SpanAttrWindowStart and SpanAttrWindowEnd bound a feeder window, RFC 3339.
	SpanAttrWindowStart = "sre.window.start"
	SpanAttrWindowEnd   = "sre.window.end"
	// SpanAttrEventCount is how many events one window or sync batch emitted.
	SpanAttrEventCount = "sre.events.count"
)

// The tracer is cached, but keyed on the provider it came from.
//
// A span is started once per applied event, so resolving the tracer through the global registry
// every time would put a mutex and a map lookup on the hottest path in the system. Caching it
// outright is not an option either: OpenTelemetry's global provider only back-fills already
// issued tracers on the *first* SetTracerProvider, so a tracer cached before a second one is
// installed would keep writing into the first — which is exactly what happens in a test binary
// that installs a recorder after some other test has installed an SDK.
//
// So the provider is read (one atomic load) and compared with the one the cached tracer came
// from; the tracer is rebuilt only when it has changed.
var (
	tracerMu       sync.RWMutex
	cachedProvider trace.TracerProvider
	cachedTracer   trace.Tracer
)

// Tracer returns the tracer every span in this package is started from. Components outside
// the hot path may use their own scope; this one exists so the published spans are all
// attributable to one instrumentation scope.
func Tracer() trace.Tracer {
	provider := otel.GetTracerProvider()

	tracerMu.RLock()
	if provider == cachedProvider && cachedTracer != nil {
		t := cachedTracer
		tracerMu.RUnlock()
		return t
	}
	tracerMu.RUnlock()

	tracerMu.Lock()
	defer tracerMu.Unlock()
	if provider != cachedProvider || cachedTracer == nil {
		cachedProvider, cachedTracer = provider, provider.Tracer(InstrumentationName)
	}
	return cachedTracer
}

// StartSpan starts a span on the shared tracer.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartFeederWindow starts the `feeder.window` span for one aggregation window (plan.md
// §Observability: "spans … per feeder window").
func StartFeederWindow(ctx context.Context, sourceID, start, end string) (context.Context, trace.Span) {
	return StartSpan(ctx, SpanFeederWindow,
		attribute.String(SpanAttrSourceID, sourceID),
		attribute.String(SpanAttrWindowStart, start),
		attribute.String(SpanAttrWindowEnd, end),
	)
}

// StartFeederSync starts the `feeder.sync` span for one list/sync batch.
func StartFeederSync(ctx context.Context, sourceID, note string) (context.Context, trace.Span) {
	return StartSpan(ctx, SpanFeederSync,
		attribute.String(SpanAttrSourceID, sourceID),
		attribute.String("sre.sync.note", note),
	)
}
