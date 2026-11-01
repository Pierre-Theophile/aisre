// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/investigation/record"

	"github.com/jackc/pgx/v5"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The lifecycle (T082, FR-006, FR-007, FR-057b, SC-024, data-model §State transitions).
//
// Four statuses and no fifth:
//
//	running ──► concluded(final)      every hypothesis tested, or diminishing returns
//	        ├─► concluded(partial)    budget exhausted, worker unavailable, refused
//	        └─► failed                intake or engine failure
//
//	concluded ──(a fact or answer bearing on it)──► reopened
//	reopened  ──► a NEW row linked by reopens_investigation_id, which runs → concluded
//
// **There is no state in which the engine waits for a human** (FR-006, FR-057). That is not an
// omission to be filled in later: it is the property that makes the engine safe to point at a
// pager. A human fact arrives into a *running* investigation as evidence and into a *concluded*
// one as a reopen; neither is a state the engine sits in.
//
// **The terminal outcome is a separate property from the status.** `concluded(partial)` says how
// the run ended; `unknown` says what it found. An investigation that budget-exhausted with a
// ranked answer and one that concluded finally with `unknown` are different things, and
// collapsing them would lose the distinction SC-024 measures.
//
// **A concluded row is immutable** (FR-007, Invariant 6). Every write below that touches a
// non-running row is a guarded UPDATE whose WHERE clause carries the permitted source state, so
// the rule is enforced by the statement rather than by the caller remembering it: an UPDATE that
// matches no row returns ErrNotRunning or ErrNotConcluded rather than silently doing nothing.
// The one permitted transition on a concluded row is `lifecycle = 'reopened'`, and all it
// records is that a child exists.

// LifecycleDAO moves an investigation through its states.
type LifecycleDAO struct {
	store *postgres.Store
}

// NewLifecycleDAO returns a DAO over the given store.
func NewLifecycleDAO(store *postgres.Store) *LifecycleDAO { return &LifecycleDAO{store: store} }

// Conclude moves a running investigation to `concluded` (FR-006).
//
// It is the only way a run ends well, and it is guarded: an investigation that is not running
// returns ErrNotRunning, so a second conclusion — a retry, a duplicated callback — cannot rewrite
// the first one's verdict.
func (d *LifecycleDAO) Conclude(ctx context.Context, investigationID string, c record.Conclusion) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: conclude: no database")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("investigation store: conclude %s: %w", investigationID, err)
	}
	endedAt := c.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	return d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return ConcludeInTx(ctx, tx, investigationID, c, endedAt)
	})
}

// ConcludeInTx is Conclude inside a transaction the caller owns — the engine's path, where the
// conclusion, the final ledger write and the decision record are one unit of work.
func ConcludeInTx(ctx context.Context, tx pgx.Tx, investigationID string, c record.Conclusion, endedAt time.Time) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("investigation store: conclude %s: %w", investigationID, err)
	}
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	tag, err := tx.Exec(ctx, `
		UPDATE investigation.investigations SET
			lifecycle = $2,
			conclusion_kind = $3,
			outcome = $4,
			stop_reason = $5,
			stop_detail = $6,
			verdict_line = $7,
			rollback_candidate = $8,
			ended_at = $9,
			provisional = false,
			recording_key = coalesce(nullif($10, ''), recording_key),
			recording_digest = coalesce(nullif($11, ''), recording_digest),
			decision_event_id = coalesce(nullif($12, ''), decision_event_id),
			onset_estimate_evidence_id = coalesce(nullif($13, ''), onset_estimate_evidence_id),
			verification_status = $15,
			verification_detail = $16
		WHERE investigation_id = $1 AND lifecycle = $14`,
		investigationID, record.LifecycleConcluded, c.Kind, c.Outcome,
		record.StopReasonName(c.StopReason), c.StopDetail, nullable(c.VerdictLine),
		nullable(c.RollbackCandidate), endedAt.UTC(),
		c.RecordingKey, c.RecordingDigest, c.DecisionEventID, c.OnsetEstimateEvidenceID,
		record.LifecycleRunning, nullable(c.VerificationStatus), nullable(c.VerificationDetail))
	if err != nil {
		return fmt.Errorf("investigation store: conclude %s: %w", investigationID, err)
	}
	if tag.RowsAffected() == 0 {
		return guardFailure(ctx, tx, investigationID, record.ErrNotRunning)
	}
	return insertVerifierFindings(ctx, tx, investigationID, c.VerifierFindings)
}

// insertVerifierFindings writes every claim's verdict beside the conclusion (FR-022a). A finding
// is keyed by its claim within the investigation, so a retried conclusion writes nothing twice.
func insertVerifierFindings(ctx context.Context, tx pgx.Tx, investigationID string, findings []record.VerifierFinding) error {
	for _, f := range findings {
		if _, err := tx.Exec(ctx, `
			INSERT INTO investigation.verifier_findings
				(finding_id, investigation_id, claim_ref, verdict, offending_value,
				 cited_evidence_id, note, action_taken)
			VALUES ($1, $2, $3, $4, $5,
			        (SELECT evidence_id FROM investigation.evidence_items WHERE evidence_id = nullif($6, '')),
			        $7, $8)
			ON CONFLICT (finding_id) DO NOTHING`,
			investigationID+":"+f.ClaimRef, investigationID, f.ClaimRef, f.Verdict,
			nullable(f.OffendingValue), f.CitedEvidenceID, nullable(f.Note), f.ActionTaken); err != nil {
			return fmt.Errorf("investigation store: verifier finding %s of %s: %w", f.ClaimRef, investigationID, err)
		}
	}
	return nil
}

// Fail moves a running investigation to `failed` (FR-006): an intake or engine failure, which is
// a terminal state like any other and carries no conclusion kind.
func (d *LifecycleDAO) Fail(ctx context.Context, investigationID, detail string) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: fail: no database")
	}
	return d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE investigation.investigations SET
				lifecycle = $2, outcome = $3, stop_reason = $4, stop_detail = $5,
				ended_at = now(), provisional = false
			WHERE investigation_id = $1 AND lifecycle = $6`,
			investigationID, record.LifecycleFailed, record.OutcomeFailed,
			record.StopReasonName(investigationv1.StopReason_ENGINE_FAILED), detail, record.LifecycleRunning)
		if err != nil {
			return fmt.Errorf("investigation store: fail %s: %w", investigationID, err)
		}
		if tag.RowsAffected() == 0 {
			return guardFailure(ctx, tx, investigationID, record.ErrNotRunning)
		}
		return nil
	})
}

// MarkReopened is the single permitted transition on a concluded row (FR-007, FR-057b).
//
// It records that a child exists and nothing else: the verdict, the ledger and the evidence of
// the concluded run stay exactly as produced, which is what "the concluded record MUST remain
// readable exactly as produced" means.
func (d *LifecycleDAO) MarkReopened(ctx context.Context, investigationID string) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: reopen: no database")
	}
	return d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return MarkReopenedInTx(ctx, tx, investigationID)
	})
}

// MarkReopenedInTx is MarkReopened inside a caller's transaction, so the parent's status and the
// child row land together.
func MarkReopenedInTx(ctx context.Context, tx pgx.Tx, investigationID string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE investigation.investigations SET lifecycle = $2
		WHERE investigation_id = $1 AND lifecycle = $3`,
		investigationID, record.LifecycleReopened, record.LifecycleConcluded)
	if err != nil {
		return fmt.Errorf("investigation store: reopen %s: %w", investigationID, err)
	}
	if tag.RowsAffected() == 0 {
		// Already reopened is not an error: a second fact bearing on the same concluded
		// investigation reopens it once and links a second child.
		var lifecycle string
		if err := tx.QueryRow(ctx,
			`SELECT lifecycle FROM investigation.investigations WHERE investigation_id = $1`,
			investigationID).Scan(&lifecycle); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("investigation store: reopen %s: %w", investigationID, record.ErrNotFound)
			}
			return fmt.Errorf("investigation store: reopen %s: %w", investigationID, err)
		}
		if lifecycle != record.LifecycleReopened {
			return fmt.Errorf("investigation store: reopen %s (lifecycle %s): %w",
				investigationID, lifecycle, record.ErrNotConcluded)
		}
	}
	return nil
}

// LifecycleOf reads an investigation's status, conclusion kind and outcome.
func (d *LifecycleDAO) LifecycleOf(ctx context.Context, investigationID string) (lifecycle, conclusionKind, outcome string, err error) {
	if d == nil || d.store == nil {
		return "", "", "", errors.New("investigation store: lifecycle: no database")
	}
	var kind, out *string
	err = d.store.Pool().QueryRow(ctx, `
		SELECT lifecycle, conclusion_kind, outcome
		FROM investigation.investigations WHERE investigation_id = $1`,
		investigationID).Scan(&lifecycle, &kind, &out)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", "", fmt.Errorf("investigation store: lifecycle %s: %w", investigationID, record.ErrNotFound)
	case err != nil:
		return "", "", "", fmt.Errorf("investigation store: lifecycle %s: %w", investigationID, err)
	}
	if kind != nil {
		conclusionKind = *kind
	}
	if out != nil {
		outcome = *out
	}
	return lifecycle, conclusionKind, outcome, nil
}

// LifecycleOf reads an investigation's status from the investigation DAO, so a caller that
// already holds one does not need a second DAO to ask the one question every human-channel path
// asks: has this concluded?
func (d *InvestigationDAO) LifecycleOf(ctx context.Context, investigationID string) (lifecycle, conclusionKind, outcome string, err error) {
	return (&LifecycleDAO{store: d.store}).LifecycleOf(ctx, investigationID)
}

// guardFailure turns a guarded UPDATE that matched nothing into the error that says why: the row
// is missing, or it is not in the state the transition permits.
func guardFailure(ctx context.Context, tx pgx.Tx, investigationID string, guard error) error {
	var lifecycle string
	if err := tx.QueryRow(ctx,
		`SELECT lifecycle FROM investigation.investigations WHERE investigation_id = $1`,
		investigationID).Scan(&lifecycle); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("investigation store: %s: %w", investigationID, record.ErrNotFound)
		}
		return fmt.Errorf("investigation store: %s: %w", investigationID, err)
	}
	return fmt.Errorf("investigation store: %s (lifecycle %s): %w", investigationID, lifecycle, guard)
}
