// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Reading the log back (FR-023, FR-052).
//
// Replay is the reason this file exists: iterating in `appended_seq` order with each row's
// stored `observed_at` is what lets the projector rebuild the graph and get the same observed
// intervals as the live run, rather than the intervals of the replay clock. The log is never
// reordered — not by valid time, not by source sequence — because the order it was appended in
// is a fact about what the graph knew and when (constitution III, research §5).
//
// Extent answers the other half: how much of reality does this graph claim to have seen? A
// consumer that does not know a feeder was disconnected for an hour cannot judge an answer
// that came out of that hour, so gaps are first-class (FR-052, edge case "feeder gap").

// Record is one row of log.events: the envelope as appended, plus what the log added.
type Record struct {
	// AppendedSeq is the physical log order. It is the cursor Iterate resumes from and is
	// never part of an identifier (data-model.md).
	AppendedSeq int64
	// ObservedAt is the observed-time lower bound assigned at append. A replay must project
	// with this value, never with the replay clock (FR-023).
	ObservedAt time.Time
	// Principal is the authenticated individual behind a human decision event (FR-041).
	Principal string
	// TraceID correlates the append with the tool's own telemetry (research §15).
	TraceID string
	// Type is the log.events.type value, e.g. "upsert_node".
	Type string
	// Envelope is the event as the source sent it, rebuilt from the row's columns and its
	// canonical payload.
	Envelope *graphv1.EventEnvelope
}

// Checkpoint is a feeder's statement of coverage: "everything between From and To has been
// delivered", optionally preceded by a gap it knows it missed (FR-052).
type Checkpoint struct {
	SourceID string
	// At is when the checkpoint was recorded. Zero means now.
	At time.Time
	// From and To bound what the feeder claims to have delivered.
	From time.Time
	To   time.Time
	// GapBefore marks that the feeder was disconnected before From and cannot vouch for what
	// happened in between (edge case "feeder gap").
	GapBefore bool
	Note      string
}

// Iterate calls fn for every event with appended_seq greater than from, in append order.
// Passing 0 iterates the whole log. Stopping early is done by returning an error from fn,
// which Iterate returns unchanged.
func (l *Log) Iterate(ctx context.Context, from int64, fn func(*Record) error) error {
	return l.iterate(ctx, `
		SELECT appended_seq, observed_at, principal, trace_id, type, event_id, idempotency_key,
		       source_id, source_seq, schema_version, source_observed_at, payload
		FROM log.events
		WHERE appended_seq > $1
		ORDER BY appended_seq`, fn, from)
}

// Page iterates at most `limit` events after `from`, in append order.
//
// It exists for the batched replay (projector.ReplayWithOptions), which walks the whole log a
// page at a time so that a million-event replay holds a page in memory and not a million
// payloads. The `LIMIT` is load-bearing rather than cosmetic: a caller that used Iterate and
// stopped reading after N rows would still pay for the rest, because the driver drains the
// result set when the rows are closed — measured at 99 ms of server time and half a million
// discarded rows *per page* on the 1M-event reference workload, which turned a linear replay
// into a quadratic one (docs/benchmarks/2026-09-17-replay.md).
func (l *Log) Page(ctx context.Context, from int64, limit int, fn func(*Record) error) error {
	return l.iterate(ctx, `
		SELECT appended_seq, observed_at, principal, trace_id, type, event_id, idempotency_key,
		       source_id, source_seq, schema_version, source_observed_at, payload
		FROM log.events
		WHERE appended_seq > $1
		ORDER BY appended_seq
		LIMIT $2`, fn, from, limit)
}

// BySource iterates one source's events in append order, which is the order that source's
// declared sequence numbers are checked against (research §5).
func (l *Log) BySource(ctx context.Context, sourceID string, from int64, fn func(*Record) error) error {
	return l.iterate(ctx, `
		SELECT appended_seq, observed_at, principal, trace_id, type, event_id, idempotency_key,
		       source_id, source_seq, schema_version, source_observed_at, payload
		FROM log.events
		WHERE source_id = $1 AND appended_seq > $2
		ORDER BY appended_seq`, fn, sourceID, from)
}

// ByObservedRange iterates the events observed in the half-open interval [from, to), in append
// order. A zero `to` means "up to now".
func (l *Log) ByObservedRange(ctx context.Context, from, to time.Time, fn func(*Record) error) error {
	upper := any(nil)
	if !to.IsZero() {
		upper = to.UTC()
	}
	return l.iterate(ctx, `
		SELECT appended_seq, observed_at, principal, trace_id, type, event_id, idempotency_key,
		       source_id, source_seq, schema_version, source_observed_at, payload
		FROM log.events
		WHERE observed_at >= $1 AND ($2::timestamptz IS NULL OR observed_at < $2)
		ORDER BY appended_seq`, fn, from.UTC(), upper)
}

func (l *Log) iterate(ctx context.Context, sql string, fn func(*Record) error, args ...any) error {
	rows, err := l.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("log: read events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return err
		}
		if err := fn(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("log: read events: %w", err)
	}
	return nil
}

func scanRecord(rows pgx.Rows) (*Record, error) {
	var (
		record           Record
		principal        *string
		traceID          *string
		eventID          string
		idempotencyKey   string
		sourceID         string
		sourceSeq        *int64
		schemaVersion    string
		sourceObservedAt *time.Time
		payload          []byte
	)
	if err := rows.Scan(
		&record.AppendedSeq, &record.ObservedAt, &principal, &traceID, &record.Type,
		&eventID, &idempotencyKey, &sourceID, &sourceSeq, &schemaVersion,
		&sourceObservedAt, &payload,
	); err != nil {
		return nil, fmt.Errorf("log: scan event: %w", err)
	}
	record.ObservedAt = record.ObservedAt.UTC()
	record.Principal = deref(principal)
	record.TraceID = deref(traceID)

	env := &graphv1.EventEnvelope{
		EventId:        eventID,
		IdempotencyKey: idempotencyKey,
		SourceId:       sourceID,
		SourceSeq:      sourceSeq,
		SchemaVersion:  schemaVersion,
	}
	if sourceObservedAt != nil {
		env.SourceObservedAt = timestamppb.New(sourceObservedAt.UTC())
	}
	if err := setBody(env, record.Type, payload); err != nil {
		return nil, err
	}
	record.Envelope = env
	return &record, nil
}

// setBody rebuilds the oneof from the row's type and canonical payload. The type column and
// the payload are written together in one INSERT, so a row that does not round-trip here is a
// corrupted log, not a schema drift; it is reported as such rather than skipped.
func setBody(env *graphv1.EventEnvelope, eventType string, payload []byte) error {
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: false}
	decode := func(m proto.Message) error {
		if err := unmarshal.Unmarshal(payload, m); err != nil {
			return fmt.Errorf("log: decode %s payload of %s: %w", eventType, env.GetEventId(), err)
		}
		return nil
	}
	switch eventType {
	case "upsert_node":
		body := &graphv1.UpsertNode{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: body}
	case "upsert_edge":
		body := &graphv1.UpsertEdge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: body}
	case "retract_node":
		body := &graphv1.RetractNode{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RetractNode{RetractNode: body}
	case "retract_edge":
		body := &graphv1.RetractEdge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RetractEdge{RetractEdge: body}
	case "observe_change":
		body := &graphv1.ObserveChange{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ObserveChange{ObserveChange: body}
	case "identity_claim":
		body := &graphv1.IdentityClaim{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: body}
	case "confirm_merge":
		body := &graphv1.ConfirmMerge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ConfirmMerge{ConfirmMerge: body}
	case "reject_merge":
		body := &graphv1.RejectMerge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RejectMerge{RejectMerge: body}
	case "split_entity":
		body := &graphv1.SplitEntity{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_SplitEntity{SplitEntity: body}
	case "manual_merge":
		body := &graphv1.ManualMerge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ManualMerge{ManualMerge: body}
	case "source_checkpoint":
		body := &graphv1.SourceCheckpoint{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_SourceCheckpoint{SourceCheckpoint: body}
	// The feature 001 change package for 002 (ADR-0005 D2/D3). A replay reads every row back
	// through here, so a type missing from this switch is a type that cannot be replayed —
	// which is the same thing as a type that cannot be stored (constitution III).
	case "record_investigation":
		body := &graphv1.RecordInvestigation{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RecordInvestigation{RecordInvestigation: body}
	case "submit_human_fact":
		body := &graphv1.SubmitHumanFact{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_SubmitHumanFact{SubmitHumanFact: body}
	case "reopen_investigation":
		body := &graphv1.ReopenInvestigation{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ReopenInvestigation{ReopenInvestigation: body}
	case "label_investigation":
		body := &graphv1.LabelInvestigation{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_LabelInvestigation{LabelInvestigation: body}
	case "alert_transition":
		body := &graphv1.AlertTransition{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_AlertTransition{AlertTransition: body}
	// The 003 dependency package (FR-029, plan item 2). All three are here for the reason stated
	// above: a type this switch does not know is a type that cannot be replayed, which is the same
	// thing as a type that cannot be stored. A proposal that could be appended and not read back
	// would make a replay produce a different graph from the live one — and the decisions are
	// durable precisely so that they survive a replay (FR-122).
	case "propose_dependency":
		body := &graphv1.ProposeDependency{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ProposeDependency{ProposeDependency: body}
	case "confirm_dependency":
		body := &graphv1.ConfirmDependency{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: body}
	case "reject_dependency":
		body := &graphv1.RejectDependency{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RejectDependency{RejectDependency: body}
	// 004 T148: a value several entities share. Here for the reason the two comments above give, and this
	// is the case that proved the reason is not rhetorical: the type was added to Validate and to the
	// append path and missed here, so every recorded fixture appended cleanly and then failed to replay.
	// A type this switch does not know is a type that cannot be stored.
	case "correlate_entity":
		body := &graphv1.CorrelateEntity{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: body}
	default:
		return fmt.Errorf("log: event %s has unknown type %q", env.GetEventId(), eventType)
	}
	return nil
}

// LastAppendedSeq returns the highest appended_seq in the log, or 0 when it is empty. It is
// the cursor a consumer resumes an Iterate from.
func (l *Log) LastAppendedSeq(ctx context.Context) (int64, error) {
	var seq *int64
	if err := l.store.Pool().QueryRow(ctx,
		`SELECT max(appended_seq) FROM log.events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("log: read last appended_seq: %w", err)
	}
	if seq == nil {
		return 0, nil
	}
	return *seq, nil
}

// Checkpoint records a feeder's coverage statement.
func (l *Log) Checkpoint(ctx context.Context, cp Checkpoint, tx pgx.Tx) error {
	if cp.SourceID == "" {
		return errors.New("log: checkpoint needs a source_id")
	}
	at := cp.At
	if at.IsZero() {
		at = l.now()
	}
	_, err := l.querier(tx).Exec(ctx, `
		INSERT INTO log.checkpoints (source_id, checkpoint_at, extent_from, extent_to, gap_before, note)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		cp.SourceID, at.UTC(), nullableTime(cp.From), nullableTime(cp.To), cp.GapBefore, cp.Note)
	if err != nil {
		return fmt.Errorf("log: record checkpoint for %s: %w", cp.SourceID, err)
	}
	return nil
}

// Extent reports how much of reality the graph claims to have seen: the observed-time span of
// the whole log, and per source its last checkpoint and the gaps it admitted to (FR-052).
//
// A gap is the interval between the coverage a source had already claimed and the point it
// picked up again after a disconnection, so a consumer can say "this answer comes from a
// window the Kubernetes feeder was not watching" instead of quietly trusting it.
func (l *Log) Extent(ctx context.Context) (*graphv1.Extent, error) {
	extent := &graphv1.Extent{}

	var earliest, latest *time.Time
	if err := l.store.Pool().QueryRow(ctx,
		`SELECT min(observed_at), max(observed_at) FROM log.events`).Scan(&earliest, &latest); err != nil {
		return nil, fmt.Errorf("log: read log extent: %w", err)
	}
	if earliest != nil {
		extent.EarliestObserved = timestamppb.New(earliest.UTC())
	}
	if latest != nil {
		extent.LatestObserved = timestamppb.New(latest.UTC())
	}

	sources, err := l.sourceExtents(ctx)
	if err != nil {
		return nil, err
	}
	extent.Sources = sources
	return extent, nil
}

func (l *Log) sourceExtents(ctx context.Context) ([]*graphv1.SourceExtent, error) {
	rows, err := l.store.Pool().Query(ctx, `
		SELECT source_id, checkpoint_at, extent_from, extent_to, gap_before
		FROM log.checkpoints
		ORDER BY source_id, checkpoint_at, checkpoint_id`)
	if err != nil {
		return nil, fmt.Errorf("log: read checkpoints: %w", err)
	}
	defer rows.Close()

	bySource := map[string]*graphv1.SourceExtent{}
	var order []string
	coveredTo := map[string]time.Time{}

	for rows.Next() {
		var (
			sourceID     string
			checkpointAt time.Time
			from, to     *time.Time
			gapBefore    bool
		)
		if err := rows.Scan(&sourceID, &checkpointAt, &from, &to, &gapBefore); err != nil {
			return nil, fmt.Errorf("log: read checkpoints: %w", err)
		}
		se, ok := bySource[sourceID]
		if !ok {
			se = &graphv1.SourceExtent{SourceId: sourceID}
			bySource[sourceID] = se
			order = append(order, sourceID)
		}
		se.LastCheckpoint = timestamppb.New(checkpointAt.UTC())

		if gapBefore {
			gap := &graphv1.Interval{}
			if prev, ok := coveredTo[sourceID]; ok {
				gap.Start = timestamppb.New(prev.UTC())
			} else {
				// Nothing was claimed before, so the gap's start is genuinely unknown
				// (FR-011: never guess a timestamp in place of unknown).
				gap.StartUnknown = true
			}
			if from != nil {
				gap.End = timestamppb.New(from.UTC())
			} else {
				gap.EndUnknown = true
			}
			se.Gaps = append(se.Gaps, gap)
		}
		if to != nil {
			coveredTo[sourceID] = *to
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: read checkpoints: %w", err)
	}

	extents := make([]*graphv1.SourceExtent, 0, len(order))
	for _, id := range order {
		extents = append(extents, bySource[id])
	}
	return extents, nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
