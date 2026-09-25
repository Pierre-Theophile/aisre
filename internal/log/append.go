// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/log/migrate"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The append path (FR-018 to FR-020, FR-024).
//
// Appending is the only way anything enters the system, and the log is the source of truth the
// graph is projected from (constitution III). Four things happen here and nowhere else:
//
//  1. the event is validated against the published contract (validate.go);
//  2. its source is checked to be registered, so every row has a declared origin;
//  3. its idempotency key is claimed, so a re-delivery is a no-op and a re-delivery with a
//     different body is recorded as an audit finding rather than silently dropped;
//  4. observed time is stamped — from the caller on replay, from the clock otherwise —
//     and stored on the row, which is what makes a replay reproduce the observed intervals
//     of the live run (FR-023).
//
// Nothing here touches the graph. The projector calls Append inside its own transaction so
// that "the event was logged" and "the graph moved" commit together or not at all (FR-022).

// Log appends to and reads back log.events. It is safe for concurrent use.
type Log struct {
	store      *postgres.Store
	accepted   SchemaVersions
	migrations *migrate.Registry
	now        func() time.Time
}

// Option configures a Log.
type Option func(*Log)

// WithSchemaVersions replaces the accepted event schema versions (FR-025).
func WithSchemaVersions(versions SchemaVersions) Option {
	return func(l *Log) { l.accepted = versions }
}

// WithMigrations replaces the event-schema transformation registry (FR-025, constitution IX).
// Production uses migrate.Default(); a test registers its own to exercise a version bump.
func WithMigrations(r *migrate.Registry) Option {
	return func(l *Log) { l.migrations = r }
}

// WithClock replaces the source of observed time. Tests pin it; production leaves it alone.
func WithClock(now func() time.Time) Option {
	return func(l *Log) { l.now = now }
}

// New returns a Log over store.
func New(store *postgres.Store, opts ...Option) *Log {
	l := &Log{
		store:      store,
		accepted:   DefaultSchemaVersions,
		migrations: migrate.Default(),
		now:        func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Store returns the store the log was built on, for callers that need to open the transaction
// an Append will run in.
func (l *Log) Store() *postgres.Store { return l.store }

// SchemaVersions returns the accepted event schema versions.
func (l *Log) SchemaVersions() SchemaVersions { return l.accepted }

// Migrations returns the event-schema transformation registry the append path consults.
func (l *Log) Migrations() *migrate.Registry { return l.migrations }

// migrate brings an envelope up to an accepted schema version before it is validated (FR-025,
// constitution IX, edge case "schema version drift").
//
// Three outcomes, and the order matters. An envelope already at an accepted version is returned
// untouched — the common case, and the one that must cost nothing. An envelope at an older
// version the registry can reach an accepted one from is transformed, and what gets appended is
// the transformed event, so a replay reads the migrated payload and reproduces the same graph
// (FR-023). An envelope at a version nothing can reach is refused with the published reason
// code, and the detail names both what is accepted and what could be transformed, because a
// feeder that is told only "rejected" cannot fix itself (FR-024).
func (l *Log) migrate(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, *Rejection) {
	version := env.GetSchemaVersion()
	if version == "" || len(l.accepted) == 0 || l.accepted.Accepts(version) {
		// Nothing to transform, or nothing to transform *from*: an envelope with no version at
		// all is not drift, it is a malformed event, and Validate says so in the words it has
		// always used.
		return env, nil
	}
	if l.migrations == nil {
		return env, reject(ReasonUnknownSchemaVersion,
			"schema_version %q; accepted: %s", version, l.accepted)
	}
	chain, err := l.migrations.ChainToAny(version, l.accepted)
	if err != nil {
		return env, reject(ReasonUnknownSchemaVersion,
			"schema_version %q; accepted: %s; no transformation reaches one of them (%v)",
			version, l.accepted, err)
	}
	if len(chain) == 0 {
		return env, nil
	}
	migrated, err := chain.Apply(env)
	if err != nil {
		return env, reject(ReasonUnknownSchemaVersion,
			"schema_version %q; the transformation %s failed (%v)", version, chain.Steps(), err)
	}
	return migrated, nil
}

// AppendOptions carries what the caller knows and the envelope does not.
type AppendOptions struct {
	// ObservedAt is the observed-time lower bound to stamp. Zero means "now": the graph
	// assigns observed time at acceptance (FR-019). A replay or a fixture load passes the
	// recorded value so the projection is reproduced exactly (FR-023).
	ObservedAt time.Time
	// Principal is the authenticated individual behind a human decision event (FR-041).
	Principal string
	// Tx, when set, is the transaction the append runs in. The projector passes its own so
	// that append and project are one atomic step; a bare ingest leaves it nil and the append
	// is its own statement.
	Tx pgx.Tx
}

// querier is the subset of pgx both a pool and a transaction provide, so every statement below
// runs unchanged inside or outside a caller's transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (l *Log) querier(tx pgx.Tx) querier {
	if tx != nil {
		return tx
	}
	return l.store.Pool()
}

// Source is a feeder's declared contract, as registered in log.sources.
type Source struct {
	// SourceID is the feeder's identity, e.g. "k8s:prod-eu1".
	SourceID string
	// Kind is the connector family: "k8s", "otel", and later vendors.
	Kind string
	// Ordering is "per_source_sequence" when the feeder numbers its events, "none" otherwise.
	Ordering string
	// ReorderingWindow is how far out of order the feeder may deliver (research §5). The
	// projector never reorders; this is what the shuffle check in FR-048 permutes within.
	ReorderingWindow time.Duration
	// SchemaVersion is the event schema version the feeder emits.
	SchemaVersion string
}

// RegisterSource records a feeder and its declared guarantees. Registering a source that
// already exists is a no-op: log.sources is append-only like everything else in the schema, so
// changing a declaration means a new source id.
func (l *Log) RegisterSource(ctx context.Context, src Source, tx pgx.Tx) error {
	if src.SourceID == "" {
		return errors.New("log: source_id is required")
	}
	if src.Ordering == "" {
		src.Ordering = "none"
	}
	if src.SchemaVersion == "" {
		src.SchemaVersion = firstOr(l.accepted, "1.0.0")
	}
	_, err := l.querier(tx).Exec(ctx, `
		INSERT INTO log.sources (source_id, kind, ordering, reordering_window, schema_version)
		VALUES ($1, $2, $3, make_interval(secs => $4), $5)
		ON CONFLICT (source_id) DO NOTHING`,
		src.SourceID, src.Kind, src.Ordering, src.ReorderingWindow.Seconds(), src.SchemaVersion)
	if err != nil {
		return fmt.Errorf("log: register source %s: %w", src.SourceID, err)
	}
	return nil
}

// Sources lists the registered feeders in id order.
func (l *Log) Sources(ctx context.Context) ([]Source, error) {
	rows, err := l.store.Pool().Query(ctx, `
		SELECT source_id, kind, ordering, reordering_window, schema_version
		FROM log.sources ORDER BY source_id`)
	if err != nil {
		return nil, fmt.Errorf("log: list sources: %w", err)
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var (
			src    Source
			window time.Duration
		)
		if err := rows.Scan(&src.SourceID, &src.Kind, &src.Ordering, &window, &src.SchemaVersion); err != nil {
			return nil, fmt.Errorf("log: list sources: %w", err)
		}
		src.ReorderingWindow = window
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: list sources: %w", err)
	}
	return sources, nil
}

// Append validates env and, if it passes, appends it to the log.
//
// The returned IngestResult is the feeder's answer and never nil on a nil error:
//
//   - APPLIED — the event is in the log, with the observed time it was stamped with;
//   - DUPLICATE_NOOP — the idempotency key was already claimed; nothing changed. When the
//     re-delivered body differs from the first one, log.duplicate_deliveries records that
//     (FR-020), which is the only trace a feeder bug of this kind ever leaves;
//   - REJECTED — validation refused it, or its source is not registered. The event is in
//     log.rejected_events with its reason, and the graph is untouched (FR-024).
//
// An error is returned only for an infrastructure failure. A refused event is a result, not an
// error: a feeder sending one bad event must still be told about it in the same shape as a
// good one.
func (l *Log) Append(ctx context.Context, env *graphv1.EventEnvelope, opts AppendOptions) (*graphv1.IngestResult, error) {
	q := l.querier(opts.Tx)

	// Schema evolution runs first: an event recorded under an older version is transformed up
	// to one this build accepts before anything else looks at it, so validation, projection and
	// replay all see the current shape (FR-025). `received` keeps the envelope as it arrived,
	// because a rejection must be audited in the words the feeder sent.
	received := env
	env, rejection := l.migrate(env)
	if rejection != nil {
		if err := l.recordRejection(ctx, q, received, rejection); err != nil {
			return nil, err
		}
		return rejectedResult(received.GetEventId(), rejection), nil
	}

	if rejection := Validate(env, l.accepted); rejection != nil {
		if err := l.recordRejection(ctx, q, env, rejection); err != nil {
			return nil, err
		}
		return rejectedResult(env.GetEventId(), rejection), nil
	}

	registered, err := l.sourceRegistered(ctx, q, env.GetSourceId())
	if err != nil {
		return nil, err
	}
	if !registered {
		rejection := reject(ReasonUnknownSource,
			"source_id %q is not registered; call RegisterSource first", env.GetSourceId())
		if err := l.recordRejection(ctx, q, env, rejection); err != nil {
			return nil, err
		}
		return rejectedResult(env.GetEventId(), rejection), nil
	}

	row, err := newRow(env, opts, l.now, traceIDFromContext(ctx))
	if err != nil {
		return nil, err
	}

	var (
		observedAt time.Time
		inserted   bool
	)
	err = q.QueryRow(ctx, `
		INSERT INTO log.events (
			event_id, idempotency_key, source_id, source_seq, schema_version, type,
			valid_at, valid_end, valid_from_unknown, source_observed_at, observed_at,
			principal, trace_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT DO NOTHING
		RETURNING observed_at`,
		row.eventID, row.idempotencyKey, row.sourceID, row.sourceSeq, row.schemaVersion, row.eventType,
		row.validAt, row.validEnd, row.validFromUnknown, row.sourceObservedAt, row.observedAt,
		row.principal, row.traceID, row.payload,
	).Scan(&observedAt)
	switch {
	case err == nil:
		inserted = true
	case errors.Is(err, pgx.ErrNoRows):
		inserted = false
	default:
		return nil, fmt.Errorf("log: append %s: %w", row.eventID, err)
	}

	if !inserted {
		return l.recordDuplicate(ctx, q, row)
	}
	return &graphv1.IngestResult{
		EventId:    row.eventID,
		Status:     graphv1.IngestResult_APPLIED,
		ObservedAt: timestamppb.New(observedAt.UTC()),
	}, nil
}

// eventRow is one log.events row, assembled from the envelope plus what only the log knows.
type eventRow struct {
	eventID          string
	idempotencyKey   string
	sourceID         string
	sourceSeq        *int64
	schemaVersion    string
	eventType        string
	validAt          *time.Time
	validEnd         *time.Time
	validFromUnknown bool
	sourceObservedAt *time.Time
	observedAt       time.Time
	principal        *string
	traceID          *string
	payload          []byte
}

func newRow(env *graphv1.EventEnvelope, opts AppendOptions, now func() time.Time, traceID string) (*eventRow, error) {
	payload, err := canonicalBody(env)
	if err != nil {
		return nil, err
	}
	observedAt := opts.ObservedAt
	if observedAt.IsZero() {
		observedAt = now()
	}
	times := validTimes(env)
	row := &eventRow{
		eventID:          env.GetEventId(),
		idempotencyKey:   idempotencyKey(env),
		sourceID:         env.GetSourceId(),
		schemaVersion:    env.GetSchemaVersion(),
		eventType:        EventType(env),
		validAt:          times.validAt,
		validEnd:         times.validEnd,
		validFromUnknown: times.validFromUnknown,
		observedAt:       observedAt.UTC(),
		payload:          payload,
	}
	if env.SourceSeq != nil {
		seq := env.GetSourceSeq()
		row.sourceSeq = &seq
	}
	if ts := env.GetSourceObservedAt(); ts != nil {
		t := ts.AsTime().UTC()
		row.sourceObservedAt = &t
	}
	if opts.Principal != "" {
		principal := opts.Principal
		row.principal = &principal
	}
	if traceID != "" {
		row.traceID = &traceID
	}
	return row, nil
}

// idempotencyKey defaults to the event id (FR-018, data-model.md log.events).
//
// One body derives its own: an `alert_transition` delivered without an explicit key takes the
// published 4-tuple of ADR-0005 D2 (see AlertTransitionKey). That is what makes the webhook and
// the poll behind it one event, and a human declaration observed a dozen times one event, with
// no cooperation required from the transport that delivered it (002 FR-008b).
func idempotencyKey(env *graphv1.EventEnvelope) string {
	if key := env.GetIdempotencyKey(); key != "" {
		return key
	}
	if body, ok := env.GetBody().(*graphv1.EventEnvelope_AlertTransition); ok {
		return AlertTransitionKey(env.GetSourceId(), body.AlertTransition)
	}
	return env.GetEventId()
}

// EventType is the log.events.type value for an envelope: the snake_case name of the body
// oneof field, which is exactly the vocabulary the events_type_check constraint allows.
func EventType(env *graphv1.EventEnvelope) string {
	switch env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return "upsert_node"
	case *graphv1.EventEnvelope_UpsertEdge:
		return "upsert_edge"
	case *graphv1.EventEnvelope_RetractNode:
		return "retract_node"
	case *graphv1.EventEnvelope_RetractEdge:
		return "retract_edge"
	case *graphv1.EventEnvelope_ObserveChange:
		return "observe_change"
	case *graphv1.EventEnvelope_IdentityClaim:
		return "identity_claim"
	case *graphv1.EventEnvelope_CorrelateEntity:
		return "correlate_entity"
	case *graphv1.EventEnvelope_ConfirmMerge:
		return "confirm_merge"
	case *graphv1.EventEnvelope_RejectMerge:
		return "reject_merge"
	case *graphv1.EventEnvelope_SplitEntity:
		return "split_entity"
	case *graphv1.EventEnvelope_ManualMerge:
		return "manual_merge"
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return "source_checkpoint"
	// The feature 001 change package for 002 (ADR-0005 D2/D3). The vocabulary the
	// events_type_check constraint accepts was widened to match in migration 0005.
	case *graphv1.EventEnvelope_RecordInvestigation:
		return "record_investigation"
	case *graphv1.EventEnvelope_SubmitHumanFact:
		return "submit_human_fact"
	case *graphv1.EventEnvelope_ReopenInvestigation:
		return "reopen_investigation"
	case *graphv1.EventEnvelope_LabelInvestigation:
		return "label_investigation"
	case *graphv1.EventEnvelope_AlertTransition:
		return "alert_transition"
	// 003 plan item 2 (FR-029). The vocabulary events_type_check accepts was widened to match
	// in migration 0009.
	case *graphv1.EventEnvelope_ProposeDependency:
		return "propose_dependency"
	case *graphv1.EventEnvelope_ConfirmDependency:
		return "confirm_dependency"
	case *graphv1.EventEnvelope_RejectDependency:
		return "reject_dependency"
	default:
		return ""
	}
}

// BodyMessage returns the envelope's body as a protobuf message, or nil when the oneof is
// unset. It is exported because every consumer of a log row needs the same switch.
func BodyMessage(env *graphv1.EventEnvelope) proto.Message {
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return body.UpsertNode
	case *graphv1.EventEnvelope_UpsertEdge:
		return body.UpsertEdge
	case *graphv1.EventEnvelope_RetractNode:
		return body.RetractNode
	case *graphv1.EventEnvelope_RetractEdge:
		return body.RetractEdge
	case *graphv1.EventEnvelope_ObserveChange:
		return body.ObserveChange
	case *graphv1.EventEnvelope_IdentityClaim:
		return body.IdentityClaim
	case *graphv1.EventEnvelope_CorrelateEntity:
		return body.CorrelateEntity
	case *graphv1.EventEnvelope_ConfirmMerge:
		return body.ConfirmMerge
	case *graphv1.EventEnvelope_RejectMerge:
		return body.RejectMerge
	case *graphv1.EventEnvelope_SplitEntity:
		return body.SplitEntity
	case *graphv1.EventEnvelope_ManualMerge:
		return body.ManualMerge
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return body.SourceCheckpoint
	case *graphv1.EventEnvelope_RecordInvestigation:
		return body.RecordInvestigation
	case *graphv1.EventEnvelope_SubmitHumanFact:
		return body.SubmitHumanFact
	case *graphv1.EventEnvelope_ReopenInvestigation:
		return body.ReopenInvestigation
	case *graphv1.EventEnvelope_LabelInvestigation:
		return body.LabelInvestigation
	case *graphv1.EventEnvelope_AlertTransition:
		return body.AlertTransition
	case *graphv1.EventEnvelope_ProposeDependency:
		return body.ProposeDependency
	case *graphv1.EventEnvelope_ConfirmDependency:
		return body.ConfirmDependency
	case *graphv1.EventEnvelope_RejectDependency:
		return body.RejectDependency
	default:
		return nil
	}
}

// canonicalBody renders the body as canonical JSON. Storing the body rather than the whole
// envelope keeps the row's columns and its payload from saying the same thing twice, and the
// canonical form is what makes "did this re-delivery carry a different payload?" a byte
// comparison rather than a semantic one (FR-020).
func canonicalBody(env *graphv1.EventEnvelope) ([]byte, error) {
	body := BodyMessage(env)
	if body == nil {
		return []byte("{}"), nil
	}
	raw, err := graph.CanonicalJSON(body)
	if err != nil {
		return nil, fmt.Errorf("log: canonicalise %s payload: %w", env.GetEventId(), err)
	}
	return raw, nil
}

type bodyTimes struct {
	validAt          *time.Time
	validEnd         *time.Time
	validFromUnknown bool
}

// validTimes lifts the body's valid-time assertions onto the row, so the log can be queried by
// valid time without opening every payload.
func validTimes(env *graphv1.EventEnvelope) bodyTimes {
	var times bodyTimes
	set := func(ts *timestamppb.Timestamp, dst **time.Time) {
		if ts == nil {
			return
		}
		t := ts.AsTime().UTC()
		*dst = &t
	}
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		set(body.UpsertNode.GetValidAt(), &times.validAt)
		times.validFromUnknown = body.UpsertNode.GetValidFromUnknown()
	case *graphv1.EventEnvelope_UpsertEdge:
		set(body.UpsertEdge.GetValidAt(), &times.validAt)
		times.validFromUnknown = body.UpsertEdge.GetValidFromUnknown()
	case *graphv1.EventEnvelope_RetractNode:
		set(body.RetractNode.GetValidEnd(), &times.validEnd)
	case *graphv1.EventEnvelope_RetractEdge:
		set(body.RetractEdge.GetValidEnd(), &times.validEnd)
	case *graphv1.EventEnvelope_ObserveChange:
		set(body.ObserveChange.GetValidAt(), &times.validAt)
		set(body.ObserveChange.GetValidEnd(), &times.validEnd)
		// Lifted onto the row for the same reason a node's is: a change whose start is genuinely
		// unknown begins at the observation, marked unknown, and a reader has to be able to tell that
		// from a change we happened to date precisely.
		times.validFromUnknown = body.ObserveChange.GetValidFromUnknown()
	case *graphv1.EventEnvelope_SourceCheckpoint:
		set(body.SourceCheckpoint.GetExtentFrom(), &times.validAt)
		set(body.SourceCheckpoint.GetExtentTo(), &times.validEnd)
	// The 001 change package (ADR-0005 D2/D3). Each of these asserts a valid time the same
	// way the older bodies do, and lifting it onto the row is what lets the projector read an
	// alert transition back as the node assertion it is (internal/projector/alert_transition.go)
	// without opening every payload.
	case *graphv1.EventEnvelope_RecordInvestigation:
		set(body.RecordInvestigation.GetStartedAt(), &times.validAt)
		set(body.RecordInvestigation.GetEndedAt(), &times.validEnd)
	case *graphv1.EventEnvelope_SubmitHumanFact:
		set(body.SubmitHumanFact.GetConcernsFrom(), &times.validAt)
		set(body.SubmitHumanFact.GetConcernsTo(), &times.validEnd)
	case *graphv1.EventEnvelope_AlertTransition:
		set(body.AlertTransition.GetTransitionAt(), &times.validAt)
	default:
	}
	return times
}

func (l *Log) sourceRegistered(ctx context.Context, q querier, sourceID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM log.sources WHERE source_id = $1)`, sourceID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("log: look up source %s: %w", sourceID, err)
	}
	return exists, nil
}

// RecordRejection stores a refusal decided outside Validate — a source that never registered,
// a retraction whose ref does not resolve — so that every rejected event ends up in one place
// whichever layer refused it (FR-024). tx may be nil.
func (l *Log) RecordRejection(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, rejection *Rejection) error {
	return l.recordRejection(ctx, l.querier(tx), env, rejection)
}

// recordRejection keeps the refused event for an operator to look at. The raw envelope is
// stored whole: the point of the table is to show what the feeder actually sent.
func (l *Log) recordRejection(ctx context.Context, q querier, env *graphv1.EventEnvelope, rejection *Rejection) error {
	raw, err := graph.CanonicalJSON(env)
	if err != nil {
		raw = []byte("{}")
	}
	identity := env.GetEventId()
	if identity == "" {
		sum := sha256.Sum256(raw)
		identity = "sha256:" + hex.EncodeToString(sum[:])
	}
	var sourceID *string
	if id := env.GetSourceId(); id != "" {
		sourceID = &id
	}
	_, err = q.Exec(ctx, `
		INSERT INTO log.rejected_events (event_id_or_hash, source_id, reason_code, reason_detail, raw)
		VALUES ($1, $2, $3, $4, $5)`,
		identity, sourceID, rejection.ReasonCode, rejection.ReasonDetail, raw)
	if err != nil {
		return fmt.Errorf("log: record rejection of %s: %w", identity, err)
	}
	return nil
}

// recordDuplicate answers a re-delivery. The first delivery wins, always; the audit row says
// whether the second one disagreed with it (FR-020, edge case "duplicate delivery").
func (l *Log) recordDuplicate(ctx context.Context, q querier, row *eventRow) (*graphv1.IngestResult, error) {
	var (
		storedPayload []byte
		observedAt    time.Time
	)
	err := q.QueryRow(ctx, `
		SELECT payload, observed_at FROM log.events
		WHERE idempotency_key = $1 OR event_id = $2
		ORDER BY appended_seq LIMIT 1`, row.idempotencyKey, row.eventID).Scan(&storedPayload, &observedAt)
	if err != nil {
		return nil, fmt.Errorf("log: read first delivery of %s: %w", row.idempotencyKey, err)
	}

	differs := !equalJSON(storedPayload, row.payload)
	sum := sha256.Sum256(row.payload)
	if _, err := q.Exec(ctx, `
		INSERT INTO log.duplicate_deliveries (idempotency_key, payload_differs, raw_hash)
		VALUES ($1, $2, $3)`,
		row.idempotencyKey, differs, hex.EncodeToString(sum[:])); err != nil {
		return nil, fmt.Errorf("log: record duplicate delivery of %s: %w", row.idempotencyKey, err)
	}

	result := &graphv1.IngestResult{
		EventId:    row.eventID,
		Status:     graphv1.IngestResult_DUPLICATE_NOOP,
		ObservedAt: timestamppb.New(observedAt.UTC()),
	}
	if differs {
		result.ReasonCode = "payload_differs"
		result.ReasonDetail = "idempotency key already delivered with a different payload; the first delivery stands"
	}
	return result, nil
}

// equalJSON compares two canonical payloads. Both sides are canonical JSON, so equality is a
// byte comparison; jsonb may have reordered what came back from the database, so the stored
// side is canonicalised again before comparing.
func equalJSON(stored, incoming []byte) bool {
	storedCanonical, err := graph.CanonicalJSON(json.RawMessage(stored))
	if err != nil {
		return string(stored) == string(incoming)
	}
	incomingCanonical, err := graph.CanonicalJSON(json.RawMessage(incoming))
	if err != nil {
		return false
	}
	return string(storedCanonical) == string(incomingCanonical)
}

func rejectedResult(eventID string, rejection *Rejection) *graphv1.IngestResult {
	return &graphv1.IngestResult{
		EventId:      eventID,
		Status:       graphv1.IngestResult_REJECTED,
		ReasonCode:   rejection.ReasonCode,
		ReasonDetail: rejection.ReasonDetail,
	}
}

// traceIDFromContext carries the feeder's trace onto the event row, so an ingestion can be
// followed end to end in the tool's own telemetry. It is provenance about the ingestion, not
// telemetry about the production system, which is why it is allowed to be here at all
// (research §15).
func traceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

func firstOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}
