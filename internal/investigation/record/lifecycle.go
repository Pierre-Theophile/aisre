// SPDX-License-Identifier: Apache-2.0

package record

import (
	"errors"
	"fmt"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

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
	// VerificationStatus is VerificationVerified or VerificationNotVerified, and VerificationDetail
	// says why in one sentence (FR-022c). Empty only on a path that never ran the engine.
	VerificationStatus string
	VerificationDetail string
	// SearchSpaceProfile is the rendered search-space profile the first brief carried
	// (FR-012a). Empty on a path that never read the neighbourhood.
	SearchSpaceProfile string
	// Examination is the examination report (FR-022b), nil on a path that never ran the engine.
	Examination *investigationv1.ExaminationReport
	// PropagationPaths are the paths of the supported and top-ranked hypotheses (FR-019b), nil on
	// a path that never ran the engine.
	PropagationPaths *investigationv1.PropagationPaths
	// VerifierFindings is every claim's verdict, written in the same transaction as the
	// conclusion: a removed claim is part of how the answer was reached (FR-022a).
	VerifierFindings []VerifierFinding
}

// The published verification statuses (FR-022c), as the schema stores them.
const (
	VerificationVerified    = "verified"
	VerificationNotVerified = "not_verified"
)

// VerifierFinding is one claim's verdict as `investigation.verifier_findings` stores it.
type VerifierFinding struct {
	ClaimRef        string
	Verdict         string
	OffendingValue  string
	CitedEvidenceID string
	Note            string
	ActionTaken     string
}

// VerificationStatusProto maps the stored status onto the wire enum.
func VerificationStatusProto(status string) investigationv1.VerificationStatus {
	switch status {
	case VerificationVerified:
		return investigationv1.VerificationStatus_VERIFIED
	case VerificationNotVerified:
		return investigationv1.VerificationStatus_NOT_VERIFIED
	default:
		return investigationv1.VerificationStatus_VERIFICATION_STATUS_UNSPECIFIED
	}
}

// VerificationStatusName maps the wire enum onto the stored status; unspecified is empty.
func VerificationStatusName(status investigationv1.VerificationStatus) string {
	switch status {
	case investigationv1.VerificationStatus_VERIFIED:
		return VerificationVerified
	case investigationv1.VerificationStatus_NOT_VERIFIED:
		return VerificationNotVerified
	default:
		return ""
	}
}

// Validate checks the kind and outcome are ones the schema accepts and agree with each other.
func (c Conclusion) Validate() error {
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
	switch c.VerificationStatus {
	case "", VerificationVerified, VerificationNotVerified:
	default:
		return fmt.Errorf("verification status %q: want %q or %q", c.VerificationStatus,
			VerificationVerified, VerificationNotVerified)
	}
	return nil
}

// StopReasonName maps the wire enum onto the string the schema stores. `STOP_REASON_UNSPECIFIED`
// stores NULL rather than a made-up reason.
func StopReasonName(r investigationv1.StopReason) any {
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

// StopReasonFromName is StopReasonName's inverse, for reading a row back.
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
