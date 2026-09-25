// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// Lifecycle values, exactly as `investigation.investigations.lifecycle` accepts them.
const (
	// LifecycleRunning is an investigation in flight.
	LifecycleRunning = "running"
	// LifecycleConcluded is one that reached a terminal state, qualified partial or final.
	LifecycleConcluded = "concluded"
	// LifecycleReopened is a concluded investigation that a later fact bore on. The row itself
	// is unchanged apart from this status; the new work is a new row.
	LifecycleReopened = "reopened"
	// LifecycleFailed is an intake or engine failure.
	LifecycleFailed = "failed"
)

// Conclusion kinds, exactly as the schema accepts them.
const (
	// ConclusionPartial is a run cut short: budget exhausted, a worker unavailable, a refusal.
	ConclusionPartial = "partial"
	// ConclusionFinal is a run that tested what there was to test.
	ConclusionFinal = "final"
)

// Terminal outcomes, exactly as the schema accepts them. They are a separate property from the
// lifecycle (FR-006).
const (
	// OutcomeRanked is a ranked answer.
	OutcomeRanked = "ranked"
	// OutcomeUnknown is the valid terminal outcome of FR-026.
	OutcomeUnknown = "unknown"
	// OutcomeBudgetExhausted is a run stopped by its budget. Always a partial conclusion.
	OutcomeBudgetExhausted = "budget_exhausted"
	// OutcomeFailed is an engine failure.
	OutcomeFailed = "failed"
)

// Lifecycle errors. They are distinct from a database failure because a caller has to be able to
// tell "you cannot do that" from "the database is down".
var (
	// ErrNotFound is an investigation id that names no row.
	ErrNotFound = errors.New("investigation not found")
	// ErrNotRunning is an attempt to conclude or fail an investigation that is not running. A
	// completed investigation is immutable (FR-007).
	ErrNotRunning = errors.New("investigation is not running; a completed investigation is immutable (FR-007)")
	// ErrNotConcluded is an attempt to reopen an investigation that has not concluded.
	ErrNotConcluded = errors.New("only a concluded investigation can be reopened (FR-057b)")
	// ErrBudgetExhaustedIsPartial is a final conclusion claimed for a budget-exhausted run. The
	// schema refuses it too; refusing it here names the rule.
	ErrBudgetExhaustedIsPartial = errors.New("budget exhaustion is a partial conclusion, never a final one (FR-006)")
)

// LifecycleDAO moves an investigation through its states.
type LifecycleDAO struct {
	store *postgres.Store
}

// NewLifecycleDAO returns a DAO over the given store.
func NewLifecycleDAO(store *postgres.Store) *LifecycleDAO { return &LifecycleDAO{store: store} }

// Conclusion is the terminal state of one run.
type Conclusion struct {
	// Kind is ConclusionPartial or ConclusionFinal.
	Kind string
	// Outcome is one of the four terminal outcomes, separate from Kind (FR-006).
	Outcome string
	// StopReason and StopDetail are the typed reason the loop stopped.
	StopReason investigationv1.StopReason
	StopDetail string
	// VerdictLine is the one line the on-call acts on (FR-057c).
	VerdictLine string
	// RollbackCandidate names the change that could be rolled back, or is empty — in which case
	// the verdict says there is none. Naming one is not proposing a remediation (FR-028): it is
	// naming the change the evidence points at.
	RollbackCandidate string
	// EndedAt is when the run stopped. Zero means now.
	EndedAt time.Time
	// RecordingKey, RecordingDigest and DecisionEventID link the row to the recording beside the
	// graph and to the decision record inside it.
	RecordingKey    string
	RecordingDigest string
	DecisionEventID string
	// OnsetEstimateEvidenceID is the onset estimate the ranking referenced, where there was one.
	OnsetEstimateEvidenceID string
}

func (c Conclusion) validate() error {
	switch c.Kind {
	case ConclusionPartial, ConclusionFinal:
	default:
		return fmt.Errorf("conclusion kind %q: want %q or %q", c.Kind, ConclusionPartial, ConclusionFinal)
	}
	switch c.Outcome {
	case OutcomeRanked, OutcomeUnknown, OutcomeBudgetExhausted, OutcomeFailed:
	default:
		return fmt.Errorf("outcome %q: want one of %s, %s, %s, %s",
			c.Outcome, OutcomeRanked, OutcomeUnknown, OutcomeBudgetExhausted, OutcomeFailed)
	}
	if c.Outcome == OutcomeBudgetExhausted && c.Kind == ConclusionFinal {
		return ErrBudgetExhaustedIsPartial
	}
	return nil
}

// Conclude moves a running investigation to `concluded` (FR-006).
//
// It is the only way a run ends well, and it is guarded: an investigation that is not running
// returns ErrNotRunning, so a second conclusion — a retry, a duplicated callback — cannot rewrite
// the first one's verdict.
func (d *LifecycleDAO) Conclude(ctx context.Context, investigationID string, c Conclusion) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: conclude: no database")
	}
	if err := c.validate(); err != nil {
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
func ConcludeInTx(ctx context.Context, tx pgx.Tx, investigationID string, c Conclusion, endedAt time.Time) error {
	if err := c.validate(); err != nil {
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
			onset_estimate_evidence_id = coalesce(nullif($13, ''), onset_estimate_evidence_id)
		WHERE investigation_id = $1 AND lifecycle = $14`,
		investigationID, LifecycleConcluded, c.Kind, c.Outcome,
		stopReasonName(c.StopReason), c.StopDetail, nullable(c.VerdictLine),
		nullable(c.RollbackCandidate), endedAt.UTC(),
		c.RecordingKey, c.RecordingDigest, c.DecisionEventID, c.OnsetEstimateEvidenceID,
		LifecycleRunning)
	if err != nil {
		return fmt.Errorf("investigation store: conclude %s: %w", investigationID, err)
	}
	if tag.RowsAffected() == 0 {
		return guardFailure(ctx, tx, investigationID, ErrNotRunning)
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
			investigationID, LifecycleFailed, OutcomeFailed,
			stopReasonName(investigationv1.StopReason_ENGINE_FAILED), detail, LifecycleRunning)
		if err != nil {
			return fmt.Errorf("investigation store: fail %s: %w", investigationID, err)
		}
		if tag.RowsAffected() == 0 {
			return guardFailure(ctx, tx, investigationID, ErrNotRunning)
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
		investigationID, LifecycleReopened, LifecycleConcluded)
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
				return fmt.Errorf("investigation store: reopen %s: %w", investigationID, ErrNotFound)
			}
			return fmt.Errorf("investigation store: reopen %s: %w", investigationID, err)
		}
		if lifecycle != LifecycleReopened {
			return fmt.Errorf("investigation store: reopen %s (lifecycle %s): %w",
				investigationID, lifecycle, ErrNotConcluded)
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
		return "", "", "", fmt.Errorf("investigation store: lifecycle %s: %w", investigationID, ErrNotFound)
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
			return fmt.Errorf("investigation store: %s: %w", investigationID, ErrNotFound)
		}
		return fmt.Errorf("investigation store: %s: %w", investigationID, err)
	}
	return fmt.Errorf("investigation store: %s (lifecycle %s): %w", investigationID, lifecycle, guard)
}

// stopReasonName maps the wire enum onto the string the schema stores. `STOP_REASON_UNSPECIFIED`
// stores NULL rather than a made-up reason.
func stopReasonName(r investigationv1.StopReason) any {
	switch r {
	case investigationv1.StopReason_COMPLETED:
		return "completed"
	case investigationv1.StopReason_BUDGET_EXHAUSTED_STOP:
		return "budget_exhausted"
	case investigationv1.StopReason_DIMINISHING_RETURNS:
		return "diminishing_returns"
	case investigationv1.StopReason_WORKER_UNAVAILABLE:
		return "worker_unavailable"
	case investigationv1.StopReason_REFUSED:
		return "refused"
	case investigationv1.StopReason_ENGINE_FAILED:
		return "failed"
	default:
		return nil
	}
}

// StopReasonFromName is stopReasonName's inverse, for reading a row back.
func StopReasonFromName(s string) investigationv1.StopReason {
	switch s {
	case "completed":
		return investigationv1.StopReason_COMPLETED
	case "budget_exhausted":
		return investigationv1.StopReason_BUDGET_EXHAUSTED_STOP
	case "diminishing_returns":
		return investigationv1.StopReason_DIMINISHING_RETURNS
	case "worker_unavailable":
		return investigationv1.StopReason_WORKER_UNAVAILABLE
	case "refused":
		return investigationv1.StopReason_REFUSED
	case "failed":
		return investigationv1.StopReason_ENGINE_FAILED
	default:
		return investigationv1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// LifecycleProto maps a stored lifecycle onto the wire enum.
func LifecycleProto(s string) investigationv1.Lifecycle {
	switch s {
	case LifecycleRunning:
		return investigationv1.Lifecycle_RUNNING
	case LifecycleConcluded:
		return investigationv1.Lifecycle_CONCLUDED
	case LifecycleReopened:
		return investigationv1.Lifecycle_REOPENED
	case LifecycleFailed:
		return investigationv1.Lifecycle_FAILED
	default:
		return investigationv1.Lifecycle_LIFECYCLE_UNSPECIFIED
	}
}

// ConclusionKindProto maps a stored conclusion kind onto the wire enum.
func ConclusionKindProto(s string) investigationv1.ConclusionKind {
	switch s {
	case ConclusionPartial:
		return investigationv1.ConclusionKind_CONCLUSION_PARTIAL
	case ConclusionFinal:
		return investigationv1.ConclusionKind_CONCLUSION_FINAL
	default:
		return investigationv1.ConclusionKind_CONCLUSION_KIND_UNSPECIFIED
	}
}

// OutcomeProto maps a stored terminal outcome onto the wire enum.
func OutcomeProto(s string) investigationv1.InvestigationOutcome {
	switch s {
	case OutcomeRanked:
		return investigationv1.InvestigationOutcome_RANKED
	case OutcomeUnknown:
		return investigationv1.InvestigationOutcome_UNKNOWN
	case OutcomeBudgetExhausted:
		return investigationv1.InvestigationOutcome_BUDGET_EXHAUSTED
	case OutcomeFailed:
		return investigationv1.InvestigationOutcome_INVESTIGATION_FAILED
	default:
		return investigationv1.InvestigationOutcome_INVESTIGATION_OUTCOME_UNSPECIFIED
	}
}

// OutcomeName is OutcomeProto's inverse.
func OutcomeName(o investigationv1.InvestigationOutcome) string {
	switch o {
	case investigationv1.InvestigationOutcome_RANKED:
		return OutcomeRanked
	case investigationv1.InvestigationOutcome_UNKNOWN:
		return OutcomeUnknown
	case investigationv1.InvestigationOutcome_BUDGET_EXHAUSTED:
		return OutcomeBudgetExhausted
	case investigationv1.InvestigationOutcome_INVESTIGATION_FAILED:
		return OutcomeFailed
	default:
		return ""
	}
}
