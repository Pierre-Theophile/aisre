// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// One event, one transaction (FR-022, FR-023).
//
// Apply is the only way the graph moves. It appends the event to the log and projects it in a
// single transaction, so the two can never disagree: either the log has the event and the graph
// reflects it, or neither happened. There is no batch path and no "rebuild from the source
// system" path — constitution III forbids both, and a full replay of the log is the only
// permitted rebuild.
//
// Replay is that rebuild. It walks the log in append order and projects each event with the
// observed time recorded *on the row*, never the replay clock, which is what makes a replayed
// graph identical to the live one down to its observed intervals (FR-023). It does not append:
// the events are already there.

// Projector applies logged events to the bitemporal projection.
type Projector struct {
	store   *postgres.Store
	log     *eventlog.Log
	metrics *telemetry.Metrics
}

// Option configures a Projector.
type Option func(*Projector)

// WithLog supplies the log to append through. By default the projector builds its own over the
// same store; a caller that has already configured accepted schema versions or a clock passes
// it here.
func WithLog(l *eventlog.Log) Option {
	return func(p *Projector) { p.log = l }
}

// WithMetrics supplies the instruments from plan.md §"Observability of the tool itself".
//
// The counting belongs here rather than in the ingest handler because this is the only place
// every event goes through: an RPC, a `fixture load`, a replay and the benchmark generator all
// call Apply, and a metric wired one layer up would have silently reported zero for three of
// the four. A nil *telemetry.Metrics — the default — makes every record a no-op, so a unit
// test constructs a projector unchanged.
func WithMetrics(m *telemetry.Metrics) Option {
	return func(p *Projector) { p.metrics = m }
}

// New returns a Projector over store.
func New(store *postgres.Store, opts ...Option) *Projector {
	p := &Projector{store: store}
	for _, opt := range opts {
		opt(p)
	}
	if p.log == nil {
		p.log = eventlog.New(store)
	}
	return p
}

// Log returns the event log the projector appends through.
func (p *Projector) Log() *eventlog.Log { return p.log }

// Store returns the store the projector writes to.
func (p *Projector) Store() *postgres.Store { return p.store }

// Metrics returns the instruments the projector records to. It may be nil, and every method on
// a nil *telemetry.Metrics is a no-op.
func (p *Projector) Metrics() *telemetry.Metrics { return p.metrics }

// RegisterSource records a feeder before it may append (FR-018).
func (p *Projector) RegisterSource(ctx context.Context, src eventlog.Source) error {
	return p.log.RegisterSource(ctx, src, nil)
}

// ApplyOptions carries what the envelope does not say.
type ApplyOptions struct {
	// ObservedAt is the observed-time lower bound to stamp. Zero means now (FR-019); a fixture
	// load or a replay passes the recorded value (FR-023).
	ObservedAt time.Time
	// Principal is the authenticated individual behind a human decision (FR-041).
	Principal string
}

// Apply appends env to the log and projects it, in one transaction.
//
// The result is the same three-way answer the log gives: APPLIED, DUPLICATE_NOOP (the event was
// already delivered; nothing changed) or REJECTED (with the reason code and the offending
// field). Only an infrastructure failure returns an error.
func (p *Projector) Apply(ctx context.Context, env *graphv1.EventEnvelope, observedAt time.Time) (*graphv1.IngestResult, error) {
	return p.ApplyWithOptions(ctx, env, ApplyOptions{ObservedAt: observedAt})
}

// ApplyWithOptions is Apply with a principal attached.
func (p *Projector) ApplyWithOptions(ctx context.Context, env *graphv1.EventEnvelope, opts ApplyOptions) (*graphv1.IngestResult, error) {
	var result *graphv1.IngestResult
	err := p.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = p.applyInTx(ctx, tx, env, opts)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Anything this event queued for re-evaluation is drained now, in transactions of its own. It
	// cannot be done inside the transaction above: a merge closes the observed interval of what it
	// absorbs, and close_observed refuses to close one at the instant it opened, so a second merge in
	// one transaction is refused (retrigger.go, migration 0011).
	//
	// A drain failure is returned rather than swallowed, and the event stays applied: the queue is
	// durable, so the work is still there for the next drain.
	if err := p.DrainPendingResolution(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// ApplyInTx projects env inside a transaction the caller owns, for a caller batching several
// events into one unit of work. The same atomicity rule applies: if it returns an error the
// caller must roll back.
//
// It does NOT drain the re-evaluation queue, because that needs transactions of its own and this one
// belongs to the caller. A caller batching events must call DrainPendingResolution between batches,
// or a merge that depends on the graph moving — C8's — will not be made. internal/fixture's loader is
// the worked example.
func (p *Projector) ApplyInTx(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, opts ApplyOptions) (*graphv1.IngestResult, error) {
	return p.applyInTx(ctx, tx, env, opts)
}

func (p *Projector) applyInTx(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, opts ApplyOptions) (*graphv1.IngestResult, error) {
	// One span and one latency sample per event, whatever called us (plan.md §Observability).
	eventType := eventlog.EventType(env)
	sourceID := env.GetSourceId()
	ctx, span := telemetry.StartSpan(ctx, telemetry.SpanProjectorApply,
		attribute.String(telemetry.SpanAttrEventID, env.GetEventId()),
		attribute.String(telemetry.SpanAttrEventType, eventType),
		attribute.String(telemetry.SpanAttrSourceID, sourceID),
	)
	defer span.End()
	start := time.Now()

	result, err := p.applyProjected(ctx, tx, env, opts)
	p.metrics.ObserveEventApply(ctx, eventType, sourceID, time.Since(start))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "apply failed")
		return nil, err
	}

	span.SetAttributes(attribute.String(telemetry.SpanAttrResult, result.GetStatus().String()))
	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		p.metrics.EventApplied(ctx, eventType, sourceID)
	case graphv1.IngestResult_REJECTED:
		span.SetAttributes(attribute.String(telemetry.SpanAttrReason, result.GetReasonCode()))
		p.metrics.EventRejected(ctx, result.GetReasonCode())
	case graphv1.IngestResult_DUPLICATE_NOOP, graphv1.IngestResult_STATUS_UNSPECIFIED:
		// A re-delivery changed nothing, so it counts as neither applied nor rejected.
	}
	return result, nil
}

// applyProjected is applyInTx without the instrumentation: validate, append, project.
func (p *Projector) applyProjected(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, opts ApplyOptions) (*graphv1.IngestResult, error) {
	// Resolvability is checked before the append, not after: an upsert creates what it names,
	// but a retraction naming an entity the graph has never seen is a rejected event, and a
	// rejected event must not be in log.events (data-model.md "Validation rules", FR-024).
	rejection, err := p.checkResolvable(ctx, tx, env, opts.Principal)
	if err != nil {
		return nil, err
	}
	if rejection != nil {
		if err := p.log.RecordRejection(ctx, tx, env, rejection); err != nil {
			return nil, err
		}
		return &graphv1.IngestResult{
			EventId:      env.GetEventId(),
			Status:       graphv1.IngestResult_REJECTED,
			ReasonCode:   rejection.ReasonCode,
			ReasonDetail: rejection.ReasonDetail,
		}, nil
	}

	result, err := p.log.Append(ctx, env, eventlog.AppendOptions{
		ObservedAt: opts.ObservedAt,
		Principal:  opts.Principal,
		Tx:         tx,
	})
	if err != nil {
		return nil, err
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		// Rejected events never reach the graph; a duplicate delivery is a no-op by
		// definition (FR-020).
		return result, nil
	}

	observedAt := result.GetObservedAt().AsTime().UTC()
	if err := p.project(ctx, tx, env, observedAt, opts.Principal); err != nil {
		return nil, err
	}
	return result, nil
}

// project dispatches one accepted event to the handler for its type.
func (p *Projector) project(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, observedAt time.Time, principal string) error {
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return p.applyUpsertNode(ctx, tx, env, body.UpsertNode, observedAt)
	case *graphv1.EventEnvelope_UpsertEdge:
		return p.applyUpsertEdge(ctx, tx, env, body.UpsertEdge, observedAt)
	case *graphv1.EventEnvelope_RetractNode:
		return p.applyRetractNode(ctx, tx, env, body.RetractNode, observedAt)
	case *graphv1.EventEnvelope_RetractEdge:
		return p.applyRetractEdge(ctx, tx, env, body.RetractEdge, observedAt)
	case *graphv1.EventEnvelope_ObserveChange:
		return p.applyObserveChange(ctx, tx, env, body.ObserveChange, observedAt)
	case *graphv1.EventEnvelope_IdentityClaim:
		return p.applyIdentityClaim(ctx, tx, env, body.IdentityClaim, observedAt, principal)
	case *graphv1.EventEnvelope_CorrelateEntity:
		return p.applyCorrelateEntity(ctx, tx, env, body.CorrelateEntity, observedAt, principal)
	case *graphv1.EventEnvelope_RejectMerge:
		return p.applyRejectMerge(ctx, tx, env, body.RejectMerge, observedAt, principal)
	case *graphv1.EventEnvelope_ConfirmMerge:
		return p.applyConfirmMerge(ctx, tx, env, body.ConfirmMerge, observedAt, principal)
	case *graphv1.EventEnvelope_ManualMerge:
		return p.applyManualMerge(ctx, tx, env, body.ManualMerge, observedAt, principal)
	case *graphv1.EventEnvelope_SplitEntity:
		return p.applySplitEntity(ctx, tx, env, body.SplitEntity, observedAt, principal)
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return p.applyCheckpoint(ctx, tx, env, body.SourceCheckpoint, observedAt)
	// The feature 001 change package for 002 (ADR-0005 D2/D3).
	case *graphv1.EventEnvelope_RecordInvestigation:
		return p.applyRecordInvestigation(ctx, tx, env, body.RecordInvestigation, observedAt)
	case *graphv1.EventEnvelope_SubmitHumanFact:
		return p.applySubmitHumanFact(ctx, tx, env, body.SubmitHumanFact, observedAt)
	case *graphv1.EventEnvelope_ReopenInvestigation:
		return p.applyReopenInvestigation(ctx, tx, env, body.ReopenInvestigation, observedAt)
	case *graphv1.EventEnvelope_LabelInvestigation:
		return p.applyLabelInvestigation(ctx, tx, env, body.LabelInvestigation, observedAt)
	case *graphv1.EventEnvelope_AlertTransition:
		return p.applyAlertTransition(ctx, tx, env, body.AlertTransition, observedAt)
	// 003 plan item 2 (FR-029, FR-122). A proposal is never an edge; only the confirmation is.
	case *graphv1.EventEnvelope_ProposeDependency:
		return p.applyProposeDependency(ctx, tx, env, body.ProposeDependency, observedAt)
	case *graphv1.EventEnvelope_ConfirmDependency:
		return p.applyConfirmDependency(ctx, tx, env, body.ConfirmDependency, observedAt, principal)
	case *graphv1.EventEnvelope_RejectDependency:
		return p.applyRejectDependency(ctx, tx, env, body.RejectDependency, observedAt, principal)
	default:
		return fmt.Errorf("projector: event %s has no body", env.GetEventId())
	}
}

// checkResolvable refuses, before anything is appended, an event the graph cannot honour:
// a retraction whose target it has never seen, a human decision with no authenticated
// individual behind it (FR-041), or a human decision naming an entity it does not know.
//
// The check has to happen here rather than in the handler because a refused event must not be
// in log.events at all (data-model.md "Validation rules", FR-024). A decision recorded without
// a principal is precisely what SC-011 counts as a failure, so it is refused, not stored.
func (p *Projector) checkResolvable(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, principal string) (*eventlog.Rejection, error) {
	unresolvable := func(field string, ref graph.Ref) *eventlog.Rejection {
		return &eventlog.Rejection{
			ReasonCode: eventlog.ReasonRefUnresolvable,
			ReasonDetail: fmt.Sprintf("%s %s names an entity the graph has never observed; "+
				"a retraction may not create one", field, ref),
		}
	}
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_RetractNode:
		ref := graph.RefFromProto(body.RetractNode.GetRef())
		if _, found, err := p.lookupRef(ctx, tx, ref); err != nil {
			return nil, err
		} else if !found {
			return unresolvable("retract_node.ref", ref), nil
		}
	case *graphv1.EventEnvelope_RetractEdge:
		for field, ref := range map[string]graph.Ref{
			"retract_edge.src": graph.RefFromProto(body.RetractEdge.GetSrc()),
			"retract_edge.dst": graph.RefFromProto(body.RetractEdge.GetDst()),
		} {
			if _, found, err := p.lookupRef(ctx, tx, ref); err != nil {
				return nil, err
			} else if !found {
				return unresolvable(field, ref), nil
			}
		}
	case *graphv1.EventEnvelope_ConfirmMerge:
		return p.checkDecision(ctx, tx, principal, "confirm_merge",
			body.ConfirmMerge.GetEntityA(), body.ConfirmMerge.GetEntityB())
	case *graphv1.EventEnvelope_RejectMerge:
		return p.checkDecision(ctx, tx, principal, "reject_merge",
			body.RejectMerge.GetEntityA(), body.RejectMerge.GetEntityB())
	case *graphv1.EventEnvelope_ManualMerge:
		return p.checkDecision(ctx, tx, principal, "manual_merge",
			body.ManualMerge.GetEntityA(), body.ManualMerge.GetEntityB())
	case *graphv1.EventEnvelope_SplitEntity:
		refs := []string{body.SplitEntity.GetEntityId()}
		for _, ref := range body.SplitEntity.GetDetachClaims() {
			refs = append(refs, graph.RefFromProto(ref).String())
		}
		return p.checkDecision(ctx, tx, principal, "split_entity", refs...)
	// 003 plan item 2. A proposal may not create either endpoint, and a decision may not invent
	// the proposal it decides.
	case *graphv1.EventEnvelope_ProposeDependency:
		return p.checkProposalRefs(ctx, tx, "propose_dependency",
			body.ProposeDependency.GetSrc(), body.ProposeDependency.GetDst())
	case *graphv1.EventEnvelope_ConfirmDependency:
		return p.checkProposalDecision(ctx, tx, principal, "confirm_dependency",
			body.ConfirmDependency.GetSrc(), body.ConfirmDependency.GetDst(),
			body.ConfirmDependency.GetType())
	case *graphv1.EventEnvelope_RejectDependency:
		return p.checkProposalDecision(ctx, tx, principal, "reject_dependency",
			body.RejectDependency.GetSrc(), body.RejectDependency.GetDst(),
			body.RejectDependency.GetType())
	default:
	}
	return nil, nil
}

// checkDecision enforces FR-041 and resolvability for one human decision.
func (p *Projector) checkDecision(ctx context.Context, tx pgx.Tx, principal, field string, refs ...string) (*eventlog.Rejection, error) {
	if strings.TrimSpace(principal) == "" {
		return &eventlog.Rejection{
			ReasonCode: eventlog.ReasonMissingPrincipal,
			ReasonDetail: field + " carries no authenticated individual; the graph does not accept a " +
				"resolution decision from an anonymous or shared credential (FR-041)",
		}, nil
	}
	for i, raw := range refs {
		if strings.TrimSpace(raw) == "" {
			return &eventlog.Rejection{
				ReasonCode:   eventlog.ReasonMissingRef,
				ReasonDetail: fmt.Sprintf("%s names no entity in position %d", field, i+1),
			}, nil
		}
		_, found, err := p.lookupDecisionRef(ctx, tx, raw)
		if err != nil {
			return nil, err
		}
		if !found {
			return &eventlog.Rejection{
				ReasonCode: eventlog.ReasonRefUnresolvable,
				ReasonDetail: fmt.Sprintf("%s names %q, which the graph has never observed; a decision "+
					"may not create an entity", field, raw),
			}, nil
		}
	}
	return nil, nil
}

// applyIdentityClaim stores the claim and then runs the published rules (FR-036, FR-037).
//
// Storing first is not an implementation detail: constitution VI requires every claim to be
// recorded *before* any merge decision is taken, so that the audit query can show what the
// graph knew when it decided.
func (p *Projector) applyIdentityClaim(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.IdentityClaim, observedAt time.Time, principal string) error {
	subject := graph.RefFromProto(body.GetSubject())
	claim := graph.RefFromProto(body.GetClaim())

	entityID, err := p.resolveRef(ctx, tx, subject, graph.NodeTypeUnspecified, env.GetEventId(), env.GetSourceId(), observedAt)
	if err != nil {
		return err
	}
	attributes, err := structJSON(body.GetAttributes())
	if err != nil {
		return err
	}
	if err := p.storeClaim(ctx, tx, entityID, claim, attributes, env.GetSourceId(), env.GetEventId(), observedAt); err != nil {
		return err
	}
	claimID := graph.ClaimID(claim.Namespace, claim.Value, env.GetSourceId())
	return p.runResolutionRules(ctx, tx, claimID, env.GetEventId(), observedAt, principal)
}

// applyCorrelateEntity stores a correlation key and then runs the rules that read one (004 T148).
//
// It mirrors applyIdentityClaim deliberately — resolve the subject, store, run the rules — and the one
// difference is the whole reason the event exists: storeCorrelation keys on the ENTITY as well as the
// value, so many entities may carry one key. An identity claim cannot do that, and a correlation
// stored as an identity lands on whichever entity was processed first.
func (p *Projector) applyCorrelateEntity(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.CorrelateEntity, observedAt time.Time, principal string) error {
	subject := graph.RefFromProto(body.GetSubject())
	key := graph.RefFromProto(body.GetKey())

	entityID, err := p.resolveRef(ctx, tx, subject, graph.NodeTypeUnspecified, env.GetEventId(), env.GetSourceId(), observedAt)
	if err != nil {
		return err
	}
	attributes, err := structJSON(body.GetAttributes())
	if err != nil {
		return err
	}
	if err := p.storeCorrelation(ctx, tx, entityID, key, attributes, env.GetSourceId(), env.GetEventId(), observedAt); err != nil {
		return err
	}
	return p.runCorrelationRules(ctx, tx, entityID, key, env.GetSourceId(), env.GetEventId(), observedAt, principal)
}

// applyCheckpoint records a feeder's coverage statement (FR-052).
func (p *Projector) applyCheckpoint(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.SourceCheckpoint, observedAt time.Time) error {
	cp := eventlog.Checkpoint{
		SourceID:  env.GetSourceId(),
		At:        observedAt,
		GapBefore: body.GetGapBefore(),
		Note:      body.GetNote(),
	}
	if ts := body.GetExtentFrom(); ts != nil {
		cp.From = ts.AsTime().UTC()
	}
	if ts := body.GetExtentTo(); ts != nil {
		cp.To = ts.AsTime().UTC()
	}
	return p.log.Checkpoint(ctx, cp, tx)
}

// ReplayReport is what a replay did.
type ReplayReport struct {
	// Events is how many log rows were projected.
	Events int
	// LastAppendedSeq is the log position the replay reached.
	LastAppendedSeq int64
}

// DefaultReplayBatchSize is how many events share one transaction during a replay when
// ReplayOptions leaves BatchSize at zero. It is the figure the benchmark's ingestion path uses
// (fixture.DefaultBenchBatch), for the reason below.
const DefaultReplayBatchSize = 500

// ReplayOptions configures a replay.
type ReplayOptions struct {
	// BatchSize is how many events share one transaction. Zero means
	// DefaultReplayBatchSize; one restores the transaction-per-event loop.
	BatchSize int
	// Progress is called after each committed batch with the number of events projected so
	// far and the number the log holds. Nil discards it. A replay of a million events takes
	// long enough that a caller with no way to say where it is will be killed or cancelled
	// by somebody who assumed it had hung.
	Progress func(done, total int64)
}

func (o ReplayOptions) batchSize() int {
	if o.BatchSize <= 0 {
		return DefaultReplayBatchSize
	}
	return o.BatchSize
}

// Replay projects the whole log, in append order, into the current projection, with the default
// options.
func (p *Projector) Replay(ctx context.Context) (*ReplayReport, error) {
	return p.ReplayWithOptions(ctx, ReplayOptions{})
}

// ReplayWithOptions projects the whole log, in append order, into the current projection.
//
// It must be run against an empty projection: the graph tables are append-only and a version
// row cannot be written twice, so replaying onto a populated projection collides — which is
// the correct failure, because a "truncate and reload" would violate constitution III. Tests
// get a fresh database from pgtest; an operator rebuilds into a fresh schema.
//
// Events are not re-appended. Each row's stored observed_at is used, so the rebuilt projection
// has the observed intervals the live one had (FR-023).
//
// # Why a batch, and what it does not change
//
// FR-023 is about *what* a replay produces: the same events, in the same order, with the same
// recorded observed times, therefore the same graph down to its bytes. Nothing here touches any
// of that. Events are still read in append order and projected one at a time, by the same
// handlers, in the same sequence; the only thing BatchSize changes is how often the work is
// committed.
//
// It changes it by a lot. `Apply` is one event per transaction because it is the *live* path,
// where the log row and the projection it implies must land together or not at all, and where
// the next event may arrive from anywhere. A replay is neither: the log is already written,
// nothing else is writing, and the run is a rebuild into an empty projection. Committing once
// per event made a million events a million commits — measured at ~39 events/s against 435 for
// the benchmark's ingestion path, which batches 500 per transaction, and the ADR-0002 60-minute
// criterion was missed by that loop rather than by the store (docs/benchmarks/README.md).
//
// Constitution III's "graph state MUST be updated incrementally, one event at a time" is about
// *incremental application*: no batch rebuild from a source system, no truncate-and-reload, no
// folding several events into one write. That still holds — each event is still applied on its
// own, against the state the events before it produced. The transaction boundary is a durability
// decision, not a semantic one.
//
// The cost is stated rather than hidden: **a failing event fails its whole batch and the
// replay**, so a replay that dies part-way leaves the projection at the last committed batch
// boundary rather than at the last event. That is acceptable precisely because a replay is not a
// live path — it rebuilds into an empty projection and the remedy for a failed one is to run it
// again from empty, never to resume in place. A caller that wants event-level recovery passes
// `BatchSize: 1` and gets the old loop back. (research §4, "Implementation notes".)
func (p *Projector) ReplayWithOptions(ctx context.Context, opts ReplayOptions) (*ReplayReport, error) {
	report := &ReplayReport{}
	total, err := p.logSize(ctx)
	if err != nil {
		return nil, err
	}

	size := opts.batchSize()
	batch := make([]*eventlog.Record, 0, size)
	cursor := int64(0)
	for {
		batch = batch[:0]
		// The log is read a page at a time rather than into one slice, so that a million-event
		// replay holds a page in memory and not a million payloads. The page is a `LIMIT` query
		// and not an early-stopped iteration: the driver drains a result set when its rows are
		// closed, so stopping after 500 rows of an unbounded scan still pays for the other
		// 994,722 — measured, and the reason the first batched replay was no faster than it had
		// to be (docs/benchmarks/2026-09-17-replay.md).
		if err := p.log.Page(ctx, cursor, size, func(record *eventlog.Record) error {
			batch = append(batch, record)
			return nil
		}); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}

		if err := p.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			for _, record := range batch {
				if err := p.project(ctx, tx, record.Envelope, record.ObservedAt, record.Principal); err != nil {
					return fmt.Errorf("projector: replay %s (appended_seq %d): %w",
						record.Envelope.GetEventId(), record.AppendedSeq, err)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}

		report.Events += len(batch)
		report.LastAppendedSeq = batch[len(batch)-1].AppendedSeq
		cursor = report.LastAppendedSeq
		if opts.Progress != nil {
			opts.Progress(int64(report.Events), total)
		}
	}
	return report, nil
}

// logSize counts the events a replay will project, for the progress callback.
func (p *Projector) logSize(ctx context.Context) (int64, error) {
	var total int64
	if err := p.store.Pool().QueryRow(ctx, `SELECT count(*) FROM log.events`).Scan(&total); err != nil {
		return 0, fmt.Errorf("projector: count log events: %w", err)
	}
	return total, nil
}
