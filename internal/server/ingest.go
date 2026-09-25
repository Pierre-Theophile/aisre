// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The ingestion endpoint (FR-017, FR-018, FR-046).
//
// Two rules shape every method here.
//
// A rejected event is an answer, not a failure. The graph refuses events for a living —
// telemetry in a property, a missing valid time, a retraction of something it never saw — and
// a feeder that sent one bad event in a stream of ten thousand must be told about that one and
// allowed to carry on. So REJECTED is streamed back like any other result and only a transport
// or database failure ends the call.
//
// A batch is a loop, not a transaction. Constitution III fixes the unit of work at one event:
// each is appended and projected in its own transaction, so a batch of ten that fails on the
// fourth leaves the first three applied and reports the rest. The alternative — one
// transaction per batch — would make the graph's state depend on how a feeder chose to packet
// its events, which is exactly the coupling event sourcing exists to remove.
//
// Both are scoped by FR-046: every event is checked against the `source_id` on the caller's
// token before it is applied, so a compromised feeder can only lie about its own source.

// IngestService implements sreagent.graph.v1.IngestService.
type IngestService struct {
	projector *projector.Projector
	metrics   *telemetry.Metrics
	logger    *slog.Logger
	tracer    trace.Tracer
}

var _ graphv1connect.IngestServiceHandler = (*IngestService)(nil)

// NewIngestService returns the ingestion handler. metrics may be nil (every method on a nil
// *telemetry.Metrics is a no-op) and logger defaults to slog.Default().
func NewIngestService(p *projector.Projector, metrics *telemetry.Metrics, logger *slog.Logger) *IngestService {
	if logger == nil {
		logger = slog.Default()
	}
	return &IngestService{
		projector: p,
		metrics:   metrics,
		logger:    logger,
		tracer:    otel.Tracer(instrumentationName),
	}
}

// Ingest is the bidirectional stream feeders hold open (FR-017). It reads envelopes until the
// client half-closes, answering each with its IngestResult in order.
//
// The stream survives a rejected event and dies on a broken database: those are the only two
// outcomes a feeder has to distinguish, and conflating them would make a feeder retry events
// the graph has permanently refused.
func (s *IngestService) Ingest(ctx context.Context, stream *connect.BidiStream[graphv1.EventEnvelope, graphv1.IngestResult]) error {
	for {
		env, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		result, err := s.apply(ctx, env)
		if err != nil {
			return err
		}
		if err := stream.Send(result); err != nil {
			return err
		}
	}
}

// IngestBatch applies a batch one event at a time and returns one result per event, in the
// order they were sent. See the file comment: the batch is a convenience for a feeder without
// a streaming client, never a unit of atomicity.
func (s *IngestService) IngestBatch(ctx context.Context, req *connect.Request[graphv1.IngestBatchRequest]) (*connect.Response[graphv1.IngestBatchResponse], error) {
	events := req.Msg.GetEvents()
	results := make([]*graphv1.IngestResult, 0, len(events))
	for i, env := range events {
		result, err := s.apply(ctx, env)
		if err != nil {
			return nil, fmt.Errorf("events[%d]: %w", i, err)
		}
		results = append(results, result)
	}
	return connect.NewResponse(&graphv1.IngestBatchResponse{Results: results}), nil
}

// RegisterSource records a feeder's declared contract before it may append (FR-018). The
// caller must hold the feeder role and the source it registers must be the one its token is
// scoped to, so registration cannot be used to claim another feeder's identity.
func (s *IngestService) RegisterSource(ctx context.Context, req *connect.Request[graphv1.RegisterSourceRequest]) (*connect.Response[graphv1.RegisterSourceResponse], error) {
	msg := req.Msg
	if msg.GetSourceId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("register_source: source_id is required"))
	}
	if err := RequireSource(ctx, msg.GetSourceId()); err != nil {
		return nil, err
	}
	if msg.GetReorderingWindowSeconds() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("register_source: reordering_window_seconds may not be negative"))
	}

	src := eventlog.Source{
		SourceID:         msg.GetSourceId(),
		Kind:             msg.GetKind(),
		Ordering:         msg.GetOrdering(),
		ReorderingWindow: time.Duration(msg.GetReorderingWindowSeconds()) * time.Second,
		SchemaVersion:    msg.GetSchemaVersion(),
	}
	if err := s.projector.RegisterSource(ctx, src); err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("register source %s: %w", src.SourceID, err))
	}
	s.logger.Info("source registered",
		"source", src.SourceID, "kind", src.Kind, "ordering", src.Ordering,
		"reordering_window", src.ReorderingWindow)
	return connect.NewResponse(&graphv1.RegisterSourceResponse{}), nil
}

// apply authorizes, applies and instruments one event. It returns an error only for something
// the caller should retry or fix at the transport level; a refused event comes back as a
// REJECTED result with a nil error.
func (s *IngestService) apply(ctx context.Context, env *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	if env == nil {
		// The one rejection the projector never sees, so it is the one this service counts:
		// an envelope with nothing in it cannot be applied, appended or audited, and without
		// this line a feeder sending a stream of them would show up nowhere (FR-051).
		s.metrics.EventRejected(ctx, eventlog.ReasonUntyped)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ingest: empty envelope"))
	}
	// FR-046: the token's source scope is checked before anything is written, and before the
	// event id is even logged, so a caller cannot probe another source's idempotency keys.
	if err := RequireSource(ctx, env.GetSourceId()); err != nil {
		return nil, err
	}

	eventType := eventlog.EventType(env)
	ctx, span := s.tracer.Start(ctx, "ingest.apply", trace.WithAttributes(
		attribute.String(telemetry.SpanAttrEventID, env.GetEventId()),
		attribute.String(telemetry.SpanAttrEventType, eventType),
		attribute.String(telemetry.SpanAttrSourceID, env.GetSourceId()),
	))
	defer span.End()

	// Zero observed time means "stamp it now" (FR-019). A recorded observed time is only ever
	// replayed from the log itself (FR-023) or loaded by `fixture load`, never accepted from
	// the wire: a feeder that could choose its own observed time could rewrite what the graph
	// claims to have known.
	//
	// The counting happens one layer down: events_applied_total, events_rejected_total and
	// event_apply_latency are recorded by the projector, which is the one call site every event
	// passes through — an RPC, a fixture load, a replay, the benchmark. Counting again here
	// would double every number an operator reads.
	result, err := s.projector.Apply(ctx, env, time.Time{})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "apply failed")
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("apply event %s: %w", env.GetEventId(), err))
	}

	span.SetAttributes(attribute.String(telemetry.SpanAttrResult, result.GetStatus().String()))
	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		s.logger.Debug("event applied",
			"event_id", result.GetEventId(), "type", eventType, "source", env.GetSourceId())
	case graphv1.IngestResult_REJECTED:
		s.logger.Warn("event rejected",
			"event_id", result.GetEventId(), "type", eventType, "source", env.GetSourceId(),
			"reason", result.GetReasonCode(), "detail", result.GetReasonDetail())
	case graphv1.IngestResult_DUPLICATE_NOOP:
		s.logger.Debug("duplicate event ignored",
			"event_id", result.GetEventId(), "source", env.GetSourceId())
	case graphv1.IngestResult_STATUS_UNSPECIFIED:
		// The log never returns this; treating it as an infrastructure bug rather than
		// silently reporting success keeps the three-way contract honest.
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("ingest: event %s got no status from the projector", env.GetEventId()))
	}
	return result, nil
}
