// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The run's own rows: incidents, investigations, symptoms (data-model §Schema investigation).
//
// One rule runs through this file: **an investigation covers an incident, not an alert**
// (FR-008a). So an incident is upserted before an investigation opens, symptoms attach to the
// incident rather than to the investigation, and a re-delivered symptom is a no-op on its
// published idempotency key rather than a second incident (FR-008b).
//
// Reads return the wire `Investigation` message directly. That is deliberate: the CLI, the RPC
// and the renderer all want the same shape, and a second in-memory struct here would be a second
// place for a field to be forgotten.

// InvestigationDAO reads and writes the incident, investigation and symptom rows.
type InvestigationDAO struct {
	store *postgres.Store
	proj  *projector.Projector
}

// DAOOption configures an InvestigationDAO.
type DAOOption func(*InvestigationDAO)

// WithEventLog gives the DAO the projector its human channel emits through (FR-057a/b/e).
//
// It is an option rather than a parameter because the three human writes are the only ones that
// emit: everything else the DAO writes is working material of a run, which is rebuildable and
// deliberately not eventful (constitution III). A DAO built without it still writes every row —
// which is what the store's own unit tests want — and emits nothing.
func WithEventLog(proj *projector.Projector) DAOOption {
	return func(d *InvestigationDAO) { d.proj = proj }
}

// NewInvestigationDAO returns a DAO over the given store.
func NewInvestigationDAO(store *postgres.Store, opts ...DAOOption) *InvestigationDAO {
	d := &InvestigationDAO{store: store}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Store is the database behind the DAO, for a caller that needs its own transaction.
func (d *InvestigationDAO) Store() *postgres.Store { return d.store }

// Incident is one incident row.
type Incident struct {
	// IncidentID is the deterministic incident identifier (intake.IncidentID).
	IncidentID string
	// CanonicalSubjectID is the graph entity the first symptom resolved to. Empty is permitted
	// and means "the first symptom resolved to nothing", which is the FR-002b path.
	CanonicalSubjectID string
	// OpenedAt is the first symptom's instant; LastSymptomAt moves as symptoms attach.
	OpenedAt      time.Time
	LastSymptomAt time.Time
	// AssociationRuleVersion is the published rule that grouped symptoms into this incident.
	AssociationRuleVersion string
}

// NewInvestigation is everything needed to open a run.
type NewInvestigation struct {
	// InvestigationID is the stable identifier printed in every rendering.
	InvestigationID string
	// IncidentID is the incident it covers.
	IncidentID string
	// ReopensInvestigationID links a reopened run to its parent (FR-057b). Empty on a first run.
	ReopensInvestigationID string
	// ValidAt, ObservedAt, ReviewMode and Window come from intake.
	ValidAt     time.Time
	ObservedAt  time.Time
	ReviewMode  bool
	WindowStart time.Time
	WindowEnd   time.Time
	// Profile is the budget profile applied (FR-006).
	Profile string
	// Requester is the authenticated identity that asked (FR-066). Required: the schema's FK
	// into graph.principals is what makes `anonymous_principal` unrepresentable.
	Requester string
	// ModelConfig is the recorded production model configuration (FR-061).
	ModelConfig *structpb.Struct
	// AlgebraVersion, LedgerRuleVersion and SchemaVersion are the published versions in force.
	AlgebraVersion    string
	LedgerRuleVersion string
	SchemaVersion     string
	// StartedAt is when the run began. Zero means now.
	StartedAt time.Time
}

// ErrAnonymousPrincipal is an investigation with no authenticated requester (FR-066). It is the
// `anonymous_principal` reason code of data-model §Validation rules.
var ErrAnonymousPrincipal = errors.New("anonymous_principal: an investigation must name the authenticated identity that requested it (FR-066)")

// Open writes the incident, the investigation and its symptoms in one transaction.
//
// It returns the investigation id that now covers the incident. When the symptom's published key
// has been seen before, nothing is written and the existing investigation's id comes back
// instead — which is FR-008b's "re-delivery is a no-op" as a single call rather than as a
// convention every caller has to remember.
func (d *InvestigationDAO) Open(
	ctx context.Context,
	incident Incident,
	inv NewInvestigation,
	symptoms ...*investigationv1.Symptom,
) (string, error) {
	if d == nil || d.store == nil {
		return "", errors.New("investigation store: open: no database")
	}
	if inv.Requester == "" {
		return "", fmt.Errorf("investigation store: open %s: %w", inv.InvestigationID, ErrAnonymousPrincipal)
	}
	var out string
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// FR-008b: a symptom already seen means the incident is already under investigation.
		for _, s := range symptoms {
			existing, found, err := investigationForKey(ctx, tx, s.GetIdempotencyKey())
			if err != nil {
				return err
			}
			if found {
				out = existing
				return nil
			}
		}
		if err := EnsureIncidentInTx(ctx, tx, incident); err != nil {
			return err
		}
		if err := EnsurePrincipalInTx(ctx, tx, inv.Requester); err != nil {
			return err
		}
		if err := insertInvestigationInTx(ctx, tx, inv); err != nil {
			return err
		}
		for _, s := range symptoms {
			if err := InsertSymptomInTx(ctx, tx, incident.IncidentID, s); err != nil {
				return err
			}
		}
		out = inv.InvestigationID
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// Attach records a symptom against an incident already under investigation (FR-008a): the later
// arrival joins rather than starting a second run. It returns the investigation covering the
// incident, and does nothing when the symptom's key has been seen.
func (d *InvestigationDAO) Attach(ctx context.Context, incidentID string, s *investigationv1.Symptom) (string, error) {
	if d == nil || d.store == nil {
		return "", errors.New("investigation store: attach: no database")
	}
	var out string
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := InsertSymptomInTx(ctx, tx, incidentID, s); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE investigation.incidents
			SET last_symptom_at = greatest(last_symptom_at, $2)
			WHERE incident_id = $1`, incidentID, s.GetFiredAt().AsTime()); err != nil {
			return fmt.Errorf("investigation store: attach to incident %s: %w", incidentID, err)
		}
		return tx.QueryRow(ctx, `
			SELECT investigation_id FROM investigation.investigations
			WHERE incident_id = $1 ORDER BY started_at DESC LIMIT 1`, incidentID).Scan(&out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("investigation store: attach to incident %s: %w", incidentID, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

// EnsureIncidentInTx upserts an incident row, moving `last_symptom_at` forward only.
func EnsureIncidentInTx(ctx context.Context, tx pgx.Tx, in Incident) error {
	ruleVersion := in.AssociationRuleVersion
	if ruleVersion == "" {
		ruleVersion = intake.AssociationRuleVersion
	}
	last := in.LastSymptomAt
	if last.IsZero() || last.Before(in.OpenedAt) {
		last = in.OpenedAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.incidents
			(incident_id, canonical_subject_id, opened_at, last_symptom_at, association_rule_version)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (incident_id) DO UPDATE SET
			last_symptom_at = greatest(investigation.incidents.last_symptom_at, EXCLUDED.last_symptom_at),
			canonical_subject_id = coalesce(nullif(investigation.incidents.canonical_subject_id, ''),
			                                EXCLUDED.canonical_subject_id)`,
		in.IncidentID, in.CanonicalSubjectID, in.OpenedAt.UTC(), last.UTC(), ruleVersion,
	); err != nil {
		return fmt.Errorf("investigation store: incident %s: %w", in.IncidentID, err)
	}
	return nil
}

// EnsurePrincipalInTx upserts the authenticated identity behind a row. Every FK into
// `graph.principals` in this schema goes through it, so that "who did this?" is answerable from
// the rows after the identity provider has forgotten.
func EnsurePrincipalInTx(ctx context.Context, tx pgx.Tx, principal string) error {
	if principal == "" {
		return ErrAnonymousPrincipal
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.principals (principal) VALUES ($1)
		ON CONFLICT (principal) DO UPDATE SET last_seen = now()`, principal); err != nil {
		return fmt.Errorf("investigation store: record principal %s: %w", principal, err)
	}
	return nil
}

func insertInvestigationInTx(ctx context.Context, tx pgx.Tx, inv NewInvestigation) error {
	startedAt := inv.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	modelConfig := []byte("{}")
	if inv.ModelConfig != nil {
		raw, err := json.Marshal(inv.ModelConfig.AsMap())
		if err != nil {
			return fmt.Errorf("investigation store: investigation %s: model config: %w",
				inv.InvestigationID, err)
		}
		modelConfig = raw
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.investigations (
			investigation_id, incident_id, reopens_investigation_id, valid_at, observed_at,
			review_mode, "window", profile, lifecycle, provisional, requester, model_config,
			algebra_version, ledger_rule_version, schema_version, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, tstzrange($7, $8), $9, $10, true, $11, $12, $13, $14, $15, $16)`,
		inv.InvestigationID, inv.IncidentID, nullable(inv.ReopensInvestigationID),
		inv.ValidAt.UTC(), inv.ObservedAt.UTC(), inv.ReviewMode,
		inv.WindowStart.UTC(), inv.WindowEnd.UTC(), inv.Profile, LifecycleRunning,
		inv.Requester, modelConfig, inv.AlgebraVersion, inv.LedgerRuleVersion,
		inv.SchemaVersion, startedAt.UTC(),
	); err != nil {
		return fmt.Errorf("investigation store: investigation %s: %w", inv.InvestigationID, err)
	}
	return nil
}

// InsertSymptomInTx writes one symptom. A symptom whose published key has been seen is a no-op
// (FR-008b): re-delivery, however many transports observed it, records one symptom.
func InsertSymptomInTx(ctx context.Context, tx pgx.Tx, incidentID string, s *investigationv1.Symptom) error {
	if s == nil {
		return errors.New("investigation store: symptom: none given")
	}
	if s.GetDeclaringIdentity() != "" {
		if err := EnsurePrincipalInTx(ctx, tx, s.GetDeclaringIdentity()); err != nil {
			return err
		}
	}
	named, err := json.Marshal(refStrings(s.GetNamedIdentifiers()))
	if err != nil {
		return fmt.Errorf("investigation store: symptom %s: named identifiers: %w", s.GetSymptomId(), err)
	}
	targets, err := json.Marshal(targetRows(s.GetTargetRefs()))
	if err != nil {
		return fmt.Errorf("investigation store: symptom %s: target refs: %w", s.GetSymptomId(), err)
	}
	resolved := s.GetResolvedEntityIds()
	if resolved == nil {
		resolved = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.symptoms (
			symptom_id, incident_id, origin_system, origin_ref, transport, actor_kind, statement,
			fired_at, severity, title, declaring_identity, idempotency_key, named_identifiers,
			target_refs, resolved_entity_ids, resolution_evidence_id, attached_as_additional,
			grouping_evidence_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		s.GetSymptomId(), incidentID, s.GetOriginSystem(), s.GetOriginRef(), s.GetTransport(),
		actorKindOf(s), s.GetStatement(), s.GetFiredAt().AsTime(),
		nullable(s.GetSeverity()), nullable(s.GetTitle()), nullable(s.GetDeclaringIdentity()),
		s.GetIdempotencyKey(), named, targets, resolved,
		nullable(s.GetResolutionEvidenceId()), s.GetAttachedAsAdditional(),
		nullable(s.GetGroupingEvidenceId()),
	); err != nil {
		return fmt.Errorf("investigation store: symptom %s: %w", s.GetSymptomId(), err)
	}
	return nil
}

// actorKindOf is the symptom row's actor kind: `human` for a declaration, `monitor` otherwise.
// It is derived from the transport rather than stored twice, because the schema already refuses
// a `human_declared` row that is not `human`.
func actorKindOf(s *investigationv1.Symptom) string {
	if s.GetTransport() == intake.TransportHumanDeclared ||
		s.GetOrigin() == investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED {
		return intake.ActorKindHuman
	}
	return intake.ActorKindMonitor
}

// InvestigationForKey returns the investigation covering the incident a symptom key belongs to.
// It is the FR-008b lookup: "have we seen this transition or declaration before?".
func (d *InvestigationDAO) InvestigationForKey(ctx context.Context, key string) (string, bool, error) {
	if d == nil || d.store == nil {
		return "", false, errors.New("investigation store: lookup: no database")
	}
	var id string
	var found bool
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, found, err = investigationForKey(ctx, tx, key)
		return err
	})
	return id, found, err
}

func investigationForKey(ctx context.Context, tx pgx.Tx, key string) (string, bool, error) {
	if key == "" {
		return "", false, nil
	}
	var id string
	err := tx.QueryRow(ctx, `
		SELECT i.investigation_id
		FROM investigation.symptoms s
		JOIN investigation.investigations i ON i.incident_id = s.incident_id
		WHERE s.idempotency_key = $1
		ORDER BY i.started_at DESC
		LIMIT 1`, key).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("investigation store: look up key %s: %w", key, err)
	}
	return id, true, nil
}

// OpenIncidents lists the incidents a symptom arriving at `at` might attach to — everything whose
// last symptom falls inside `within` of it — with the entities their symptoms resolved to. It is
// what the association rule consumes (FR-008c).
func (d *InvestigationDAO) OpenIncidents(ctx context.Context, at time.Time, within time.Duration) ([]intake.OpenIncident, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: open incidents: no database")
	}
	rows, err := d.store.Pool().Query(ctx, `
		SELECT c.incident_id,
		       coalesce((SELECT i.investigation_id FROM investigation.investigations i
		                 WHERE i.incident_id = c.incident_id
		                 ORDER BY i.started_at DESC LIMIT 1), ''),
		       c.opened_at, c.last_symptom_at,
		       coalesce((SELECT array_agg(DISTINCT e) FROM investigation.symptoms s,
		                 unnest(s.resolved_entity_ids) AS e
		                 WHERE s.incident_id = c.incident_id), '{}'::text[])
		FROM investigation.incidents c
		WHERE c.status = 'open'
		  AND c.last_symptom_at >= $1 AND c.opened_at <= $2
		ORDER BY c.last_symptom_at DESC`,
		at.UTC().Add(-within), at.UTC().Add(within))
	if err != nil {
		return nil, fmt.Errorf("investigation store: open incidents: %w", err)
	}
	defer rows.Close()

	var out []intake.OpenIncident
	for rows.Next() {
		var oi intake.OpenIncident
		if err := rows.Scan(&oi.IncidentID, &oi.InvestigationID, &oi.OpenedAt,
			&oi.LastSymptomAt, &oi.EntityIDs); err != nil {
			return nil, fmt.Errorf("investigation store: open incidents: %w", err)
		}
		out = append(out, oi)
	}
	return out, rows.Err()
}

// Get reads one investigation, with its symptoms, human facts, reviews, labels and deliveries.
// The ledger is loaded separately by LedgerDAO, because a caller listing a hundred rows does not
// want a hundred ledgers.
func (d *InvestigationDAO) Get(ctx context.Context, investigationID string) (*investigationv1.Investigation, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: get: no database")
	}
	inv, err := d.scanOne(ctx, `WHERE investigation_id = $1`, investigationID)
	if err != nil {
		return nil, err
	}
	if err := d.loadChildren(ctx, inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// ListFilter narrows List (contracts/cli.md `investigate list`).
type ListFilter struct {
	// IncidentID lists the runs covering one incident, including reopens.
	IncidentID string
	// Lifecycle lists the runs in one status.
	Lifecycle string
	// Since lists the runs started at or after an instant.
	Since time.Time
	// Limit bounds the page; zero means DefaultListLimit.
	Limit int
}

// DefaultListLimit is the page size `investigate list` uses when none is given.
const DefaultListLimit = 50

// List reads investigations newest first. It does not load children: a list is an index, and a
// caller that wants one row's evidence asks for that row.
func (d *InvestigationDAO) List(ctx context.Context, f ListFilter) ([]*investigationv1.Investigation, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: list: no database")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := d.store.Pool().Query(ctx, investigationSelect+`
		WHERE ($1 = '' OR incident_id = $1)
		  AND ($2 = '' OR lifecycle = $2)
		  AND ($3::timestamptz IS NULL OR started_at >= $3)
		ORDER BY started_at DESC
		LIMIT $4`, f.IncidentID, f.Lifecycle, nullableTime(f.Since), limit)
	if err != nil {
		return nil, fmt.Errorf("investigation store: list: %w", err)
	}
	defer rows.Close()

	var out []*investigationv1.Investigation
	for rows.Next() {
		inv, err := scanInvestigation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

const investigationSelect = `
	SELECT investigation_id, incident_id, coalesce(reopens_investigation_id, ''),
	       valid_at, observed_at, review_mode,
	       lower("window"), upper("window"),
	       profile, lifecycle, coalesce(conclusion_kind, ''), coalesce(outcome, ''),
	       coalesce(stop_reason, ''), coalesce(stop_detail, ''),
	       coalesce(verdict_line, ''), coalesce(rollback_candidate, ''), provisional,
	       requester, model_config, algebra_version, ledger_rule_version, schema_version,
	       coalesce(recording_key, ''), coalesce(recording_digest, ''),
	       coalesce(decision_event_id, ''), coalesce(onset_estimate_evidence_id, ''),
	       started_at, ended_at
	FROM investigation.investigations`

func (d *InvestigationDAO) scanOne(ctx context.Context, where string, args ...any) (*investigationv1.Investigation, error) {
	rows, err := d.store.Pool().Query(ctx, investigationSelect+" "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("investigation store: get: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("investigation store: get: %w", err)
		}
		return nil, fmt.Errorf("investigation store: %v: %w", args, ErrNotFound)
	}
	return scanInvestigation(rows)
}

func scanInvestigation(rows pgx.Rows) (*investigationv1.Investigation, error) {
	var (
		inv                                          investigationv1.Investigation
		validAt, observedAt, windowStart, windowEnd  time.Time
		startedAt                                    time.Time
		endedAt                                      *time.Time
		lifecycle, conclusionKind, outcome, stopName string
		modelConfig                                  []byte
		profile                                      string
	)
	if err := rows.Scan(
		&inv.InvestigationId, &inv.IncidentId, &inv.ReopensInvestigationId,
		&validAt, &observedAt, &inv.ReviewMode,
		&windowStart, &windowEnd,
		&profile, &lifecycle, &conclusionKind, &outcome,
		&stopName, &inv.StopDetail,
		&inv.VerdictLine, &inv.RollbackCandidate, &inv.Provisional,
		&inv.Requester, &modelConfig, &inv.AlgebraVersion, &inv.LedgerRuleVersion,
		&inv.SchemaVersion, &inv.RecordingKey, &inv.RecordingDigest,
		&inv.DecisionEventId, &inv.OnsetEstimateEvidenceId,
		&startedAt, &endedAt,
	); err != nil {
		return nil, fmt.Errorf("investigation store: scan investigation: %w", err)
	}

	inv.ValidAt = timestamppb.New(validAt.UTC())
	inv.ObservedAt = timestamppb.New(observedAt.UTC())
	inv.Window = &investigationv1.Window{
		Start: timestamppb.New(windowStart.UTC()),
		End:   timestamppb.New(windowEnd.UTC()),
	}
	inv.Lifecycle = LifecycleProto(lifecycle)
	inv.ConclusionKind = ConclusionKindProto(conclusionKind)
	inv.Outcome = OutcomeProto(outcome)
	inv.StopReason = StopReasonFromName(stopName)
	inv.StartedAt = timestamppb.New(startedAt.UTC())
	if endedAt != nil {
		inv.EndedAt = timestamppb.New(endedAt.UTC())
	}
	if len(modelConfig) > 0 {
		var m map[string]any
		if err := json.Unmarshal(modelConfig, &m); err == nil {
			if s, err := structpb.NewStruct(m); err == nil {
				inv.ModelConfig = s
			}
		}
	}
	// The budget profile lives on the row and on the spend; carrying it here as the spend's
	// limits name keeps `investigate list --output json` self-describing without a second query.
	if profile != "" {
		inv.Spend = &investigationv1.BudgetSpend{Limits: &investigationv1.BudgetProfile{Name: profile}}
	}
	return &inv, nil
}

func (d *InvestigationDAO) loadChildren(ctx context.Context, inv *investigationv1.Investigation) error {
	symptoms, err := d.symptomsOf(ctx, inv.GetIncidentId())
	if err != nil {
		return err
	}
	inv.Symptoms = symptoms

	facts, err := d.FactsOf(ctx, inv.GetInvestigationId())
	if err != nil {
		return err
	}
	inv.Facts = facts

	reviews, err := d.ReviewsOf(ctx, inv.GetInvestigationId())
	if err != nil {
		return err
	}
	inv.Reviews = reviews

	labels, err := d.LabelsOf(ctx, inv.GetInvestigationId())
	if err != nil {
		return err
	}
	inv.Labels = labels

	deliveries, err := d.DeliveriesOf(ctx, inv.GetInvestigationId())
	if err != nil {
		return err
	}
	inv.Deliveries = deliveries
	return nil
}

func (d *InvestigationDAO) symptomsOf(ctx context.Context, incidentID string) ([]*investigationv1.Symptom, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT symptom_id, origin_system, origin_ref, transport, actor_kind, statement, fired_at,
		       coalesce(severity, ''), coalesce(title, ''), coalesce(declaring_identity, ''),
		       idempotency_key, named_identifiers, target_refs, resolved_entity_ids,
		       coalesce(resolution_evidence_id, ''), attached_as_additional,
		       coalesce(grouping_evidence_id, '')
		FROM investigation.symptoms WHERE incident_id = $1 ORDER BY fired_at, symptom_id`,
		incidentID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: symptoms of %s: %w", incidentID, err)
	}
	defer rows.Close()

	var out []*investigationv1.Symptom
	for rows.Next() {
		var (
			s                 investigationv1.Symptom
			actorKind         string
			firedAt           time.Time
			named, targetsRaw []byte
		)
		if err := rows.Scan(&s.SymptomId, &s.OriginSystem, &s.OriginRef, &s.Transport, &actorKind,
			&s.Statement, &firedAt, &s.Severity, &s.Title, &s.DeclaringIdentity,
			&s.IdempotencyKey, &named, &targetsRaw, &s.ResolvedEntityIds,
			&s.ResolutionEvidenceId, &s.AttachedAsAdditional, &s.GroupingEvidenceId); err != nil {
			return nil, fmt.Errorf("investigation store: scan symptom: %w", err)
		}
		s.FiredAt = timestamppb.New(firedAt.UTC())
		s.Origin = investigationv1.IntakeOrigin_INTAKE_ORIGIN_MONITOR
		if actorKind == intake.ActorKindHuman {
			s.Origin = investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED
		}
		s.NamedIdentifiers = parseRefs(named)
		s.TargetRefs = parseTargets(targetsRaw)
		out = append(out, &s)
	}
	return out, rows.Err()
}

// SetProvisional flips the anytime flag off once a tested answer exists (FR-046a). It is the one
// update a running investigation takes outside conclusion.
func (d *InvestigationDAO) SetProvisional(ctx context.Context, investigationID string, provisional bool) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: set provisional: no database")
	}
	tag, err := d.store.Pool().Exec(ctx, `
		UPDATE investigation.investigations SET provisional = $2
		WHERE investigation_id = $1 AND lifecycle = $3`,
		investigationID, provisional, LifecycleRunning)
	if err != nil {
		return fmt.Errorf("investigation store: set provisional on %s: %w", investigationID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("investigation store: set provisional on %s: %w", investigationID, ErrNotRunning)
	}
	return nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
