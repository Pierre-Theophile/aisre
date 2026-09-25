// SPDX-License-Identifier: Apache-2.0

// Package investigation holds the engine's self-telemetry: the spans and metrics an operator
// watches while the engine runs, and the cost accounting an owner reads afterwards (T092,
// FR-065, plan §Observability and cost accounting).
package investigation

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The engine's own telemetry, over 001's (T092, FR-065).
//
// It reuses `internal/telemetry`'s discipline rather than its instruments: the names below are
// published in plan.md §Observability and an operator's dashboard is written against them, so
// they change only with the plan. Nothing here is optional and nothing is sampled — an engine
// that cannot say how much it spent is an engine nobody will be allowed to run twice.
//
// **Cost is recorded twice, deliberately** (FR-048). Once as raw token counts per model id and
// class — the provider-independent unit, which aggregates across runs, across models and across
// price changes — and once as a derived monetary figure from a versioned, checked-in price table
// whose version is stored on the investigation. A monetary figure alone would become
// uninterpretable the next time a vendor changes its pricing; token counts alone would not
// answer "what did last month cost?".
//
// A nil *Metrics is safe: every method is a no-op on it, so an engine can be constructed in a
// unit test with no meter provider at all.

// InstrumentationName is the OpenTelemetry instrumentation scope of the engine.
const InstrumentationName = "github.com/Pierre-Theophile/aisre/internal/investigation"

// Instrument names, exactly as plan.md §Observability publishes them.
const (
	// MetricInvestigations counts investigations completed, by outcome and budget profile.
	MetricInvestigations = "investigations_total"
	// MetricInvestigationDuration is wall time per investigation, by profile and phase.
	MetricInvestigationDuration = "investigation_duration_seconds"
	// MetricWorkerCalls counts worker calls by worker, capability, mode and outcome.
	MetricWorkerCalls = "worker_calls_total"
	// MetricWorkerCallLatency is per-call wall time by worker and capability.
	MetricWorkerCallLatency = "worker_call_latency"
	// MetricModelTokens counts tokens by model and class (input, cache_write, cache_read, output).
	MetricModelTokens = "model_tokens_total"
	// MetricInvestigationCost is the derived monetary figure by model, from the versioned price
	// table.
	MetricInvestigationCost = "investigation_cost_units"
	// MetricBackendQuotaShare is the share of a backend's remaining quota one run consumed.
	MetricBackendQuotaShare = "backend_quota_share"
	// MetricLedgerUpdates counts judgments applied, by direction.
	MetricLedgerUpdates = "ledger_updates_total"
	// MetricStopReasons counts why loops stopped.
	MetricStopReasons = "stop_reasons_total"
	// MetricReplayDivergences counts replay divergences by layer (trajectory or world).
	MetricReplayDivergences = "replay_divergences_total"
	// MetricNotRecorded counts `not_recorded` answers by fixture — the miss rate's numerator.
	MetricNotRecorded = "not_recorded_total"
)

// Instruments lists every published instrument name. The coverage test walks it, so an
// instrument added to the plan and not to the code fails by name rather than as an empty panel
// during an incident.
func Instruments() []string {
	return []string{
		MetricInvestigations, MetricInvestigationDuration, MetricWorkerCalls,
		MetricWorkerCallLatency, MetricModelTokens, MetricInvestigationCost,
		MetricBackendQuotaShare, MetricLedgerUpdates, MetricStopReasons,
		MetricReplayDivergences, MetricNotRecorded,
	}
}

// Attribute keys carried by the instruments above.
const (
	AttrOutcome    = "outcome"
	AttrProfile    = "profile"
	AttrPhase      = "phase"
	AttrWorker     = "worker"
	AttrCapability = "capability"
	AttrMode       = "mode"
	AttrModel      = "model"
	AttrClass      = "class"
	AttrBackend    = "backend"
	AttrDirection  = "direction"
	AttrReason     = "reason"
	AttrLayer      = "layer"
	AttrFixture    = "fixture"
)

// Token classes, the four `usage` fields the Messages API reports (FR-048).
const (
	ClassInput      = "input"
	ClassCacheWrite = "cache_write"
	ClassCacheRead  = "cache_read"
	ClassOutput     = "output"
)

// Investigation phases, the spans a duration is attributed to. They are the three an on-call
// cares about: how long until there was *an* answer, how long until there was a *tested* one,
// and how long in total (SC-001, SC-002).
const (
	PhaseProvisional = "provisional"
	PhaseFirstTested = "first_tested"
	PhaseConclusion  = "conclusion"
)

// Span names, exactly as plan.md publishes them: one per investigation, turn, worker call, model
// call and algebra term.
const (
	SpanInvestigation = "investigation"
	SpanTurn          = "investigation.turn"
	SpanWorkerCall    = "investigation.worker_call"
	SpanModelCall     = "investigation.model_call"
	SpanAlgebraTerm   = "investigation.algebra_term"
)

// Span attribute keys. The `sre.` prefix keeps them clear of the OpenTelemetry semantic
// conventions, which own the unprefixed namespace.
const (
	SpanAttrInvestigationID = "sre.investigation.id"
	SpanAttrIncidentID      = "sre.investigation.incident_id"
	SpanAttrProfile         = "sre.investigation.profile"
	SpanAttrOutcome         = "sre.investigation.outcome"
	SpanAttrTurn            = "sre.investigation.turn"
	SpanAttrWorker          = "sre.worker"
	SpanAttrCapability      = "sre.capability"
	SpanAttrMode            = "sre.mode"
	SpanAttrTermKey         = "sre.term.key"
	SpanAttrTermName        = "sre.term.name"
	SpanAttrModel           = "sre.model.id"
	SpanAttrRole            = "sre.model.role"
	SpanAttrHypothesis      = "sre.hypothesis.id"
	SpanAttrQuestion        = "sre.discriminating_question"
)

// latencyBuckets straddle the range that matters here: a recorded worker call answers in
// microseconds, a live vendor query in seconds, a whole investigation in minutes.
var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300,
}

// Metrics holds the engine's instruments. One instance is created at startup and passed down.
type Metrics struct {
	investigations        metric.Int64Counter
	investigationDuration metric.Float64Histogram
	workerCalls           metric.Int64Counter
	workerCallLatency     metric.Float64Histogram
	modelTokens           metric.Int64Counter
	cost                  metric.Float64Counter
	quotaShare            metric.Float64Histogram
	ledgerUpdates         metric.Int64Counter
	stopReasons           metric.Int64Counter
	replayDivergences     metric.Int64Counter
	notRecorded           metric.Int64Counter
}

// NewMetrics creates every instrument on provider. Pass nil to use the global meter provider
// installed by telemetry.Setup.
func NewMetrics(provider metric.MeterProvider) (*Metrics, error) {
	if provider == nil {
		provider = otel.GetMeterProvider()
	}
	meter := provider.Meter(InstrumentationName)
	m := &Metrics{}

	var err error
	if m.investigations, err = meter.Int64Counter(MetricInvestigations,
		metric.WithDescription("Investigations completed, by terminal outcome and budget profile."),
		metric.WithUnit("{investigation}")); err != nil {
		return nil, instrumentError(MetricInvestigations, err)
	}
	if m.investigationDuration, err = meter.Float64Histogram(MetricInvestigationDuration,
		metric.WithDescription("Wall time of an investigation phase, in seconds."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(latencyBuckets...)); err != nil {
		return nil, instrumentError(MetricInvestigationDuration, err)
	}
	if m.workerCalls, err = meter.Int64Counter(MetricWorkerCalls,
		metric.WithDescription("Worker calls issued, by worker, capability, mode and outcome."),
		metric.WithUnit("{call}")); err != nil {
		return nil, instrumentError(MetricWorkerCalls, err)
	}
	if m.workerCallLatency, err = meter.Float64Histogram(MetricWorkerCallLatency,
		metric.WithDescription("Wall time of one worker call, in seconds."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(latencyBuckets...)); err != nil {
		return nil, instrumentError(MetricWorkerCallLatency, err)
	}
	if m.modelTokens, err = meter.Int64Counter(MetricModelTokens,
		metric.WithDescription("Model tokens consumed, by model id and class. The provider-independent unit (FR-048)."),
		metric.WithUnit("{token}")); err != nil {
		return nil, instrumentError(MetricModelTokens, err)
	}
	if m.cost, err = meter.Float64Counter(MetricInvestigationCost,
		metric.WithDescription("Derived monetary cost, by model, from the versioned price table (FR-048)."),
		metric.WithUnit("{unit}")); err != nil {
		return nil, instrumentError(MetricInvestigationCost, err)
	}
	if m.quotaShare, err = meter.Float64Histogram(MetricBackendQuotaShare,
		metric.WithDescription("Share of a backend's remaining quota consumed by one investigation."),
		metric.WithUnit("1"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1)); err != nil {
		return nil, instrumentError(MetricBackendQuotaShare, err)
	}
	if m.ledgerUpdates, err = meter.Int64Counter(MetricLedgerUpdates,
		metric.WithDescription("Judgments applied to the ledger, by direction."),
		metric.WithUnit("{judgment}")); err != nil {
		return nil, instrumentError(MetricLedgerUpdates, err)
	}
	if m.stopReasons, err = meter.Int64Counter(MetricStopReasons,
		metric.WithDescription("Investigation loops stopped, by typed reason."),
		metric.WithUnit("{stop}")); err != nil {
		return nil, instrumentError(MetricStopReasons, err)
	}
	if m.replayDivergences, err = meter.Int64Counter(MetricReplayDivergences,
		metric.WithDescription("Replay divergences, by layer (trajectory or world)."),
		metric.WithUnit("{divergence}")); err != nil {
		return nil, instrumentError(MetricReplayDivergences, err)
	}
	if m.notRecorded, err = meter.Int64Counter(MetricNotRecorded,
		metric.WithDescription("`not_recorded` answers, by fixture: the miss rate's numerator (FR-042b)."),
		metric.WithUnit("{answer}")); err != nil {
		return nil, instrumentError(MetricNotRecorded, err)
	}
	return m, nil
}

func instrumentError(name string, err error) error {
	return fmt.Errorf("investigation telemetry: %s: %w", name, err)
}

// InvestigationCompleted records one finished investigation (FR-065).
func (m *Metrics) InvestigationCompleted(ctx context.Context, outcome, profile string) {
	if m == nil || m.investigations == nil {
		return
	}
	m.investigations.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrOutcome, outcome), attribute.String(AttrProfile, profile)))
}

// ObservePhase records the wall time of one phase (SC-001, SC-002).
func (m *Metrics) ObservePhase(ctx context.Context, profile, phase string, d time.Duration) {
	if m == nil || m.investigationDuration == nil {
		return
	}
	m.investigationDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(AttrProfile, profile), attribute.String(AttrPhase, phase)))
}

// WorkerCall records one worker call and its latency.
func (m *Metrics) WorkerCall(ctx context.Context, worker, capability, mode, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String(AttrWorker, worker), attribute.String(AttrCapability, capability),
		attribute.String(AttrMode, mode), attribute.String(AttrOutcome, outcome))
	if m.workerCalls != nil {
		m.workerCalls.Add(ctx, 1, attrs)
	}
	if m.workerCallLatency != nil {
		m.workerCallLatency.Record(ctx, d.Seconds(), metric.WithAttributes(
			attribute.String(AttrWorker, worker), attribute.String(AttrCapability, capability)))
	}
}

// ModelTokens records one model call's token usage, by class (FR-048).
func (m *Metrics) ModelTokens(ctx context.Context, model string, input, cacheWrite, cacheRead, output int64) {
	if m == nil || m.modelTokens == nil {
		return
	}
	for _, pair := range []struct {
		class string
		n     int64
	}{
		{ClassInput, input}, {ClassCacheWrite, cacheWrite},
		{ClassCacheRead, cacheRead}, {ClassOutput, output},
	} {
		if pair.n == 0 {
			continue
		}
		m.modelTokens.Add(ctx, pair.n, metric.WithAttributes(
			attribute.String(AttrModel, model), attribute.String(AttrClass, pair.class)))
	}
}

// Cost records the derived monetary figure for one model's usage (FR-048). The price-table
// version it was derived from is stored on the investigation, not here: a metric label carrying
// a version would fragment the series every time the table changed.
func (m *Metrics) Cost(ctx context.Context, model string, units float64) {
	if m == nil || m.cost == nil {
		return
	}
	m.cost.Add(ctx, units, metric.WithAttributes(attribute.String(AttrModel, model)))
}

// QuotaShare records the share of a backend's remaining quota one investigation consumed
// (FR-047a).
func (m *Metrics) QuotaShare(ctx context.Context, backend string, share float64) {
	if m == nil || m.quotaShare == nil {
		return
	}
	m.quotaShare.Record(ctx, share, metric.WithAttributes(attribute.String(AttrBackend, backend)))
}

// LedgerUpdate records one judgment applied, by direction.
func (m *Metrics) LedgerUpdate(ctx context.Context, direction string) {
	if m == nil || m.ledgerUpdates == nil {
		return
	}
	m.ledgerUpdates.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrDirection, direction)))
}

// StopReason records why one loop stopped.
func (m *Metrics) StopReason(ctx context.Context, reason string) {
	if m == nil || m.stopReasons == nil {
		return
	}
	m.stopReasons.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrReason, reason)))
}

// ReplayDivergence records one divergence, by layer.
func (m *Metrics) ReplayDivergence(ctx context.Context, layer string) {
	if m == nil || m.replayDivergences == nil {
		return
	}
	m.replayDivergences.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrLayer, layer)))
}

// NotRecorded records one `not_recorded` answer, by fixture (FR-042b).
func (m *Metrics) NotRecorded(ctx context.Context, fixture string) {
	if m == nil || m.notRecorded == nil {
		return
	}
	m.notRecorded.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrFixture, fixture)))
}

// --- spans ---------------------------------------------------------------------------------

// Tracer is the tracer every span in the engine is started from. It delegates to 001's, so the
// engine's spans and the graph's sit in one trace (FR-065).
func Tracer() trace.Tracer { return telemetry.Tracer() }

// StartInvestigation starts the span covering one whole investigation.
func StartInvestigation(ctx context.Context, investigationID, incidentID, profile string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanInvestigation, trace.WithAttributes(
		attribute.String(SpanAttrInvestigationID, investigationID),
		attribute.String(SpanAttrIncidentID, incidentID),
		attribute.String(SpanAttrProfile, profile)))
}

// StartTurn starts the span covering one turn of the loop.
func StartTurn(ctx context.Context, investigationID string, turn int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanTurn, trace.WithAttributes(
		attribute.String(SpanAttrInvestigationID, investigationID),
		attribute.Int(SpanAttrTurn, turn)))
}

// StartWorkerCall starts the span covering one worker call, carrying the hypothesis it serves and
// the discriminating question (FR-018a) so a trace answers "why was this asked?".
func StartWorkerCall(ctx context.Context, worker, capability, mode, hypothesisID, question string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanWorkerCall, trace.WithAttributes(
		attribute.String(SpanAttrWorker, worker),
		attribute.String(SpanAttrCapability, capability),
		attribute.String(SpanAttrMode, mode),
		attribute.String(SpanAttrHypothesis, hypothesisID),
		attribute.String(SpanAttrQuestion, question)))
}

// StartModelCall starts the span covering one model call.
func StartModelCall(ctx context.Context, role, modelID string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanModelCall, trace.WithAttributes(
		attribute.String(SpanAttrRole, role),
		attribute.String(SpanAttrModel, modelID)))
}

// StartAlgebraTerm starts the span covering one algebra term's execution.
func StartAlgebraTerm(ctx context.Context, termName, termKey string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanAlgebraTerm, trace.WithAttributes(
		attribute.String(SpanAttrTermName, termName),
		attribute.String(SpanAttrTermKey, termKey)))
}
