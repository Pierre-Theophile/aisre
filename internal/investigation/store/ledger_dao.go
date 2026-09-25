// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The ledger's data-access layer (T051, data-model §investigation.hypotheses / .judgments /
// .evidence_items).
//
// One rule shapes this whole file: **an investigation's hypotheses are written as a set, in one
// transaction**. `hypotheses_posteriors_sum_to_one` is a DEFERRABLE INITIALLY DEFERRED constraint
// trigger, so it runs once at commit rather than after each row — which is the only way a
// normalised distribution can be written at all, since every intermediate state of a row-by-row
// write has confidences that do not sum to one. Save is therefore the only way to write
// hypotheses, and it takes the whole set.
//
// The second rule is that nothing here computes. Confidences arrive from `internal/investigation/
// ledger` and are copied; `ln_lr` arrives on the judgment and is copied. A DAO that recomputed
// could disagree with the ledger that produced the report, and then the database and the report
// would be two sources of truth for one number (FR-023).
//
// An evidence item's *source-of-truth declaration* (`WorkerDescription.source_of_truth`) is a
// column as of 0008. The ledger still enforces `unsupported_status` at write time with the item
// in memory — that check has to happen before the transaction — but the declaration is now stored
// beside the answer, so a reader checks FR-022 from the rows alone rather than re-deriving it
// from a worker registry that may since have been renamed, retired or re-pointed.

// LedgerDAO reads and writes the ledger tables of schema `investigation`.
type LedgerDAO struct {
	store *postgres.Store
}

// NewLedgerDAO returns a DAO over the given store.
func NewLedgerDAO(store *postgres.Store) *LedgerDAO { return &LedgerDAO{store: store} }

// Save writes an entire ledger — evidence items, hypotheses and judgments — in one transaction.
//
// The order is forced by the foreign keys and by the deferred trigger: evidence first (a judgment
// references it), then the whole hypothesis set (so the sum is checked once, at commit), then the
// judgments. A caller who already holds a transaction uses SaveInTx.
func (d *LedgerDAO) Save(ctx context.Context, l *ledger.Ledger) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: save ledger: no database")
	}
	return d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return SaveLedgerInTx(ctx, tx, l)
	})
}

// SaveLedgerInTx is Save inside a transaction the caller owns — the engine's path, where the
// ledger write and the rest of a turn's bookkeeping are one unit of work.
func SaveLedgerInTx(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
	if l == nil {
		return errors.New("investigation store: save ledger: no ledger")
	}
	investigationID := l.InvestigationID()
	for _, e := range l.EvidenceItems() {
		if err := InsertEvidenceInTx(ctx, tx, investigationID, e); err != nil {
			return err
		}
	}
	if err := SaveHypothesesInTx(ctx, tx, investigationID, l.Hypotheses()); err != nil {
		return err
	}
	for _, j := range l.Judgments() {
		if err := InsertJudgmentInTx(ctx, tx, investigationID, j); err != nil {
			return err
		}
	}
	return nil
}

// SaveHypothesesInTx writes an investigation's hypotheses as a set (Invariant 10).
//
// Every hypothesis is upserted, so a re-save after a judgment writes the renormalised
// distribution over the same rows; the deferred trigger then checks the sum once at commit. A
// caller that writes one hypothesis on its own will pass the CHECKs and fail at commit, which is
// exactly what should happen — a single hypothesis is not a distribution.
func SaveHypothesesInTx(ctx context.Context, tx pgx.Tx, investigationID string, hypotheses []ledger.Hypothesis) error {
	if len(hypotheses) == 0 {
		return fmt.Errorf("investigation store: save hypotheses: investigation %s has none; every "+
			"investigation carries at least the open hypothesis (FR-019a)", investigationID)
	}
	for _, h := range hypotheses {
		nextQuery, err := marshalTerm(h.NextQuery)
		if err != nil {
			return fmt.Errorf("investigation store: hypothesis %s: %w", h.ID, err)
		}
		// The array columns are NOT NULL with an empty default: "this hypothesis names no target
		// entity" is an empty array, not a null one.
		targets := h.TargetEntityIDs
		if targets == nil {
			targets = []string{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO investigation.hypotheses (
				hypothesis_id, investigation_id, kind, statement, candidate_change_entity_id,
				target_entity_ids, causal_role, actor_kind, prior, status, untested_reason,
				confidence, confidence_bucket, widened, widened_reason, rank, rationale,
				knowledge_derived, knowledge_confirmed, next_query, next_query_deep_link)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
			        $18, $19, $20, $21)
			ON CONFLICT (hypothesis_id) DO UPDATE SET
				kind = EXCLUDED.kind,
				statement = EXCLUDED.statement,
				candidate_change_entity_id = EXCLUDED.candidate_change_entity_id,
				target_entity_ids = EXCLUDED.target_entity_ids,
				causal_role = EXCLUDED.causal_role,
				actor_kind = EXCLUDED.actor_kind,
				prior = EXCLUDED.prior,
				status = EXCLUDED.status,
				untested_reason = EXCLUDED.untested_reason,
				confidence = EXCLUDED.confidence,
				confidence_bucket = EXCLUDED.confidence_bucket,
				widened = EXCLUDED.widened,
				widened_reason = EXCLUDED.widened_reason,
				rank = EXCLUDED.rank,
				rationale = EXCLUDED.rationale,
				knowledge_derived = EXCLUDED.knowledge_derived,
				knowledge_confirmed = EXCLUDED.knowledge_confirmed,
				next_query = EXCLUDED.next_query,
				next_query_deep_link = EXCLUDED.next_query_deep_link`,
			h.ID, investigationID, string(h.Kind), h.Statement, nullable(h.CandidateChangeEntityID),
			targets, string(h.CausalRole), nullable(h.ActorKind), h.Prior, string(h.Status),
			nullable(h.UntestedReason), h.Confidence, h.Bucket.Name, h.Widened, nullable(h.WidenedReason),
			h.Rank, h.Rationale, h.KnowledgeDerived, h.KnowledgeConfirmed, nextQuery,
			nullable(h.NextQueryDeepLink),
		); err != nil {
			return fmt.Errorf("investigation store: write hypothesis %s: %w", h.ID, err)
		}
	}
	return nil
}

// InsertEvidenceInTx writes one evidence item (FR-021, Invariant 4).
//
// It is an insert and not an upsert on purpose: an evidence item is a record of one call, and a
// repeated call produces a second item rather than overwriting the first (Invariant 5).
func InsertEvidenceInTx(ctx context.Context, tx pgx.Tx, investigationID string, e ledger.EvidenceItem) error {
	term, err := marshalTerm(e.Term)
	if err != nil {
		return fmt.Errorf("investigation store: evidence %s: %w", e.ID, err)
	}
	if term == nil {
		// `term` is NOT NULL: an evidence item that was not an algebra call — a human fact, a
		// verifier finding — records the empty term rather than a null one.
		term = []byte(`{}`)
	}
	coverage, err := marshalMessage(e.Coverage)
	if err != nil {
		return fmt.Errorf("investigation store: evidence %s coverage: %w", e.ID, err)
	}
	if coverage == nil {
		return fmt.Errorf("investigation store: evidence %s has no coverage block (%s, FR-014a)",
			e.ID, ledger.ReasonMissingCoverage)
	}
	joinKeys, err := marshalMessage(e.JoinKeys)
	if err != nil {
		return fmt.Errorf("investigation store: evidence %s join keys: %w", e.ID, err)
	}
	if joinKeys == nil {
		joinKeys = []byte(`{}`)
	}
	// `graph_event_ids` is NOT NULL with an empty default: an answer that rests on no graph
	// event carries an empty array, not a null one.
	graphEventIDs := e.GraphEventIDs
	if graphEventIDs == nil {
		graphEventIDs = []string{}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.evidence_items (
			evidence_id, investigation_id, kind, worker, capability, source_of_truth, term,
			valid_at, observed_at, called_at, mode, outcome, response_digest, response_key,
			coverage, join_keys, graph_event_ids, deep_link, deep_link_absent_reason, truncated,
			truncation_note, free_text)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
		        $19, $20, $21, $22)
		ON CONFLICT (evidence_id) DO NOTHING`,
		e.ID, investigationID, e.Kind, e.Worker, e.Capability, e.SourceOfTruth, term,
		e.ValidAt.UTC(), e.ObservedAt.UTC(), e.CalledAt.UTC(), e.Mode, e.Outcome, e.ResponseDigest,
		e.ResponseKey, coverage, joinKeys, graphEventIDs, nullable(e.DeepLink),
		nullable(e.DeepLinkAbsentReason), e.Truncated, nullable(e.TruncationNote),
		nullable(e.FreeText),
	); err != nil {
		return fmt.Errorf("investigation store: write evidence %s: %w", e.ID, err)
	}
	return nil
}

// InsertJudgmentInTx writes one judgment (FR-020a).
//
// The (hypothesis, evidence) uniqueness is the schema's, and a violation of it is returned as the
// ledger's published `duplicate_judgment` rather than as a raw constraint error, so a caller
// matching on reason codes sees the same vocabulary from the database as from the ledger.
func InsertJudgmentInTx(ctx context.Context, tx pgx.Tx, investigationID string, j ledger.Judgment) error {
	recordedAt := j.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	var strength any
	if j.Strength != "" {
		strength = string(j.Strength)
	} else {
		// A neutral judgment carries no strength in the ledger; the column is NOT NULL and its
		// CHECK accepts only the four published values, so it is recorded at the weakest rung —
		// inert either way, since direction neutral pins ln_lr to zero.
		strength = string(ledger.Weak)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO investigation.judgments (
			judgment_id, investigation_id, hypothesis_id, evidence_id, direction, strength,
			ln_lr, source, worker_call_id, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (judgment_id) DO NOTHING`,
		j.ID, investigationID, j.HypothesisID, j.EvidenceID, string(j.Direction), strength,
		j.LnLR, string(j.Source), nullable(j.WorkerCallID), recordedAt.UTC(),
	)
	if err != nil {
		if isUniqueViolation(err, "judgments_one_per_pair") {
			return &ledger.RejectionError{
				ReasonCode: ledger.ReasonDuplicateJudgment,
				Detail: fmt.Sprintf("evidence item %s has already moved hypothesis %s",
					j.EvidenceID, j.HypothesisID),
			}
		}
		return fmt.Errorf("investigation store: write judgment %s: %w", j.ID, err)
	}
	_ = tag
	return nil
}

// LedgerRows is an investigation's ledger as the database holds it: the rows, with no ledger
// object around them.
//
// It is what a replay, a grader or a calibration job reads. Recomputing `ledger.Posteriors` over
// Hypotheses and Judgments must reproduce every stored confidence exactly — Invariant 1, and the
// reason `prior` and `ln_lr` are columns at all.
type LedgerRows struct {
	// InvestigationID is the run.
	InvestigationID string
	// Hypotheses are in published rank order.
	Hypotheses []ledger.Hypothesis
	// Judgments are in recorded order.
	Judgments []ledger.Judgment
	// Evidence is in call order.
	Evidence []ledger.EvidenceItem
}

// Load reads an investigation's ledger rows.
func (d *LedgerDAO) Load(ctx context.Context, investigationID string) (*LedgerRows, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: load ledger: no database")
	}
	rows := &LedgerRows{InvestigationID: investigationID}
	var err error
	if rows.Hypotheses, err = d.loadHypotheses(ctx, investigationID); err != nil {
		return nil, err
	}
	if rows.Judgments, err = d.loadJudgments(ctx, investigationID); err != nil {
		return nil, err
	}
	if rows.Evidence, err = d.loadEvidence(ctx, investigationID); err != nil {
		return nil, err
	}
	// Invariant 9's readable half: an exonerated hypothesis carries the onset estimate's
	// evidence id, which the rows hold on the exoneration judgment rather than on the
	// hypothesis. Putting it back here means a reader never has to know that.
	for i := range rows.Hypotheses {
		if rows.Hypotheses[i].Status != ledger.StatusExonerated {
			continue
		}
		for _, j := range rows.Judgments {
			if j.HypothesisID == rows.Hypotheses[i].ID && j.Source == ledger.SourceExoneration {
				rows.Hypotheses[i].OnsetEvidenceID = j.EvidenceID
				break
			}
		}
	}
	return rows, nil
}

func (d *LedgerDAO) loadHypotheses(ctx context.Context, investigationID string) ([]ledger.Hypothesis, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT hypothesis_id, kind, statement, coalesce(candidate_change_entity_id, ''),
		       target_entity_ids, causal_role, coalesce(actor_kind, ''), prior, status,
		       coalesce(untested_reason, ''), confidence, confidence_bucket, widened,
		       coalesce(widened_reason, ''), rank, rationale, knowledge_derived,
		       knowledge_confirmed, next_query, coalesce(next_query_deep_link, '')
		FROM investigation.hypotheses
		WHERE investigation_id = $1
		ORDER BY rank`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: read hypotheses of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []ledger.Hypothesis
	for rows.Next() {
		var (
			h         ledger.Hypothesis
			kind      string
			role      string
			status    string
			bucket    string
			nextQuery []byte
		)
		if err := rows.Scan(&h.ID, &kind, &h.Statement, &h.CandidateChangeEntityID,
			&h.TargetEntityIDs, &role, &h.ActorKind, &h.Prior, &status, &h.UntestedReason,
			&h.Confidence, &bucket, &h.Widened, &h.WidenedReason, &h.Rank, &h.Rationale,
			&h.KnowledgeDerived, &h.KnowledgeConfirmed, &nextQuery, &h.NextQueryDeepLink,
		); err != nil {
			return nil, fmt.Errorf("investigation store: scan hypothesis: %w", err)
		}
		h.Kind, h.CausalRole, h.Status = ledger.Kind(kind), ledger.CausalRole(role), ledger.Status(status)
		for _, b := range ledger.Buckets() {
			if b.Name == bucket {
				h.Bucket = b
			}
		}
		if h.NextQuery, err = unmarshalTerm(nextQuery); err != nil {
			return nil, fmt.Errorf("investigation store: hypothesis %s next query: %w", h.ID, err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (d *LedgerDAO) loadJudgments(ctx context.Context, investigationID string) ([]ledger.Judgment, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT judgment_id, hypothesis_id, evidence_id, direction, strength, ln_lr, source,
		       coalesce(worker_call_id, ''), recorded_at
		FROM investigation.judgments
		WHERE investigation_id = $1
		ORDER BY recorded_at, judgment_id`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: read judgments of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []ledger.Judgment
	for rows.Next() {
		var (
			j                           ledger.Judgment
			direction, strength, source string
		)
		if err := rows.Scan(&j.ID, &j.HypothesisID, &j.EvidenceID, &direction, &strength,
			&j.LnLR, &source, &j.WorkerCallID, &j.RecordedAt,
		); err != nil {
			return nil, fmt.Errorf("investigation store: scan judgment: %w", err)
		}
		j.Direction, j.Source = ledger.Direction(direction), ledger.JudgmentSource(source)
		if j.Direction != ledger.Neutral {
			j.Strength = ledger.Strength(strength)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (d *LedgerDAO) loadEvidence(ctx context.Context, investigationID string) ([]ledger.EvidenceItem, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT evidence_id, kind, worker, capability, source_of_truth, term, valid_at, observed_at,
		       called_at, mode, outcome, response_digest, response_key, coverage, join_keys,
		       graph_event_ids, coalesce(deep_link, ''), coalesce(deep_link_absent_reason, ''),
		       truncated, coalesce(truncation_note, ''), coalesce(free_text, '')
		FROM investigation.evidence_items
		WHERE investigation_id = $1
		ORDER BY called_at, evidence_id`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: read evidence of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []ledger.EvidenceItem
	for rows.Next() {
		var (
			e                        ledger.EvidenceItem
			term, coverage, joinKeys []byte
		)
		if err := rows.Scan(&e.ID, &e.Kind, &e.Worker, &e.Capability, &e.SourceOfTruth, &term,
			&e.ValidAt, &e.ObservedAt, &e.CalledAt, &e.Mode, &e.Outcome, &e.ResponseDigest,
			&e.ResponseKey, &coverage, &joinKeys, &e.GraphEventIDs, &e.DeepLink,
			&e.DeepLinkAbsentReason, &e.Truncated, &e.TruncationNote, &e.FreeText,
		); err != nil {
			return nil, fmt.Errorf("investigation store: scan evidence: %w", err)
		}
		if e.Term, err = unmarshalTerm(term); err != nil {
			return nil, fmt.Errorf("investigation store: evidence %s term: %w", e.ID, err)
		}
		e.Coverage = &investigationv1.Coverage{}
		if err := unmarshalMessage(coverage, e.Coverage); err != nil {
			return nil, fmt.Errorf("investigation store: evidence %s coverage: %w", e.ID, err)
		}
		e.JoinKeys = &investigationv1.JoinKeys{}
		if err := unmarshalMessage(joinKeys, e.JoinKeys); err != nil {
			return nil, fmt.Errorf("investigation store: evidence %s join keys: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// marshalTerm renders an algebra term as canonical protobuf JSON for a jsonb column.
func marshalTerm(term *investigationv1.AlgebraTerm) ([]byte, error) {
	if term == nil {
		return nil, nil
	}
	return marshalMessage(term)
}

// marshalMessage renders a protobuf message for a jsonb column, with stable bytes.
//
// protojson randomises its whitespace on purpose, to stop callers depending on its byte
// stability. These columns end up inside digests and golden diffs, so the output is re-encoded
// through encoding/json — the same discipline internal/graph's canonical serializer applies, for
// the same reason.
func marshalMessage(m proto.Message) ([]byte, error) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return nil, nil
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("normalise: %w", err)
	}
	return json.Marshal(generic)
}

func unmarshalMessage(raw []byte, m proto.Message) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, m); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func unmarshalTerm(raw []byte) (*investigationv1.AlgebraTerm, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}
	term := &investigationv1.AlgebraTerm{}
	if err := unmarshalMessage(raw, term); err != nil {
		return nil, err
	}
	return term, nil
}

// isUniqueViolation reports whether err is PostgreSQL's unique violation on the named constraint.
// It is how a schema constraint is translated back into the ledger's published reason code.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// nullable turns an empty string into a SQL NULL, so a column that means "absent" holds NULL
// rather than an empty string that looks like a value.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
