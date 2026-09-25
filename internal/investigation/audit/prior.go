// SPDX-License-Identifier: Apache-2.0

package audit

import "math"

// π₀, the prior of *no observed change explains this* (ADR-0005 D9, FR-019a, FR-020, FR-023).
//
// This is the hook Phase 5's ledger calls. The rule is one line — π₀ = 1 − ceiling — and every
// other line here exists to keep that one honest.
//
// Why it lives in this package rather than in the ledger: the number is a property of the
// deployment's feeder set, measured by the audit, not a tuning constant of the reasoning layer.
// Putting it here makes the dependency point the right way — the ledger reads the audit, the
// audit never reads the ledger — and means a re-run of the audit after a connector ships
// (FR-071a) moves π₀ without anyone editing the ledger.
//
// Why six decimals: `investigation.coverage_audits.ceiling` is `numeric(9,6)`, so a prior with
// more precision than that could not survive a round-trip through the database, and a confidence
// recorded today has to remain reproducible from the stored ceiling years later. Rounding at the
// boundary is what makes the replayed number byte-identical to the live one.
//
// Why the audit id travels with the value: a historical confidence is only interpretable if the
// ceiling it was computed against is recoverable. PriorRecord carries both.

// PriorFromAudit returns π₀ for the audit's feeder set in force: 1 − ceiling, rounded to six
// decimals.
//
// A nil audit returns 1.0, and that is deliberate rather than defensive. No audit means no
// measured ceiling, and an engine with no measured ceiling has no evidence that any cause is
// observable at all — so the whole prior mass belongs to *no observed change explains this*. A
// silent 0 here would publish the opposite claim, that everything is visible, which is exactly
// the unearned confidence User Story 0 exists to prevent.
func PriorFromAudit(result *Result) float64 {
	if result == nil {
		return 1
	}
	return PriorFromCeiling(result.Ceiling)
}

// PriorFromCeiling returns π₀ = 1 − ceiling, rounded to six decimals and clamped to [0, 1].
func PriorFromCeiling(ceiling float64) float64 {
	if math.IsNaN(ceiling) {
		return 1
	}
	return round6(clamp01(1 - clamp01(ceiling)))
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// PriorRecord is π₀ together with everything needed to read it again years later. The ledger
// records one of these on every investigation (ADR-0005 D9).
type PriorRecord struct {
	// Prior is π₀ itself.
	Prior float64 `json:"prior"`
	// AuditID names the audit it came from.
	AuditID string `json:"audit_id"`
	// Ceiling is the measured ceiling it was derived from.
	Ceiling float64 `json:"ceiling"`
	// FeederSet is the graph configuration that ceiling was measured against.
	FeederSet string `json:"feeder_set"`
	// IncidentCount is the count the ceiling rests on (FR-069).
	IncidentCount int `json:"incident_count"`
	// RunAt is when the audit was carried out.
	RunAt Instant `json:"run_at"`
}

// PriorRecordFromAudit builds the ledger's record of π₀ from a published audit.
//
// A nil audit yields the same 1.0 PriorFromAudit does, with an empty audit id — which reads, in
// a stored ledger, as "this investigation ran before any audit was published", and is the only
// honest thing such a row can say.
func PriorRecordFromAudit(result *Result) PriorRecord {
	if result == nil {
		return PriorRecord{Prior: 1}
	}
	return PriorRecord{
		Prior:         PriorFromAudit(result),
		AuditID:       result.AuditID,
		Ceiling:       result.Ceiling,
		FeederSet:     result.FeederSetInForce,
		IncidentCount: result.ClassifiableCount,
		RunAt:         result.RunAt,
	}
}
