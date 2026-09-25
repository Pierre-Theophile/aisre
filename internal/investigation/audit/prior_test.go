// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"math"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// TestPriorFromCeiling pins π₀ = 1 − ceiling and its six-decimal rounding (ADR-0005 D9).
func TestPriorFromCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ceiling float64
		want    float64
	}{
		{"the published 2026-09 ceiling", 0.615385, 0.384615},
		{"nothing observable: all the mass on the open hypothesis", 0, 1},
		{"everything observable", 1, 0},
		{"rounded to six decimals", 1.0 / 3.0, 0.666667},
		{"a ceiling below zero is clamped", -0.5, 1},
		{"a ceiling above one is clamped", 1.5, 0},
		{"NaN is maximum ignorance, not zero", math.NaN(), 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := audit.PriorFromCeiling(test.ceiling); !closeTo(got, test.want) {
				t.Errorf("PriorFromCeiling(%v) = %v, want %v", test.ceiling, got, test.want)
			}
		})
	}
}

// TestPriorRoundsToSixDecimals: the ceiling column is numeric(9,6), so a prior with more
// precision than that could not survive a round trip through the database, and a replayed
// confidence would not be byte-identical to the live one.
func TestPriorRoundsToSixDecimals(t *testing.T) {
	t.Parallel()

	for _, ceiling := range []float64{1.0 / 3.0, 1.0 / 7.0, 2.0 / 13.0, 0.6153846153846154} {
		prior := audit.PriorFromCeiling(ceiling)
		if scaled := prior * 1e6; math.Abs(scaled-math.Round(scaled)) > 1e-6 {
			t.Errorf("PriorFromCeiling(%v) = %v, which is not a six-decimal value", ceiling, prior)
		}
	}
}

// TestPriorFromAudit is the hook Phase 5's ledger calls.
func TestPriorFromAudit(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	if got := audit.PriorFromAudit(result); !closeTo(got, wantPrior) {
		t.Errorf("PriorFromAudit = %v, want %v", got, wantPrior)
	}

	// Selecting a lower rung of the ladder moves π₀ with it: it is a property of the
	// deployment's feeder set, not a constant of the reasoning layer.
	weakest := runFixture(t, "synthetic-01", audit.Options{FeederSet: "feature-001"})
	if got := audit.PriorFromAudit(weakest); !closeTo(got, 1) {
		t.Errorf("PriorFromAudit with the 001 feeders alone = %v, want 1", got)
	}

	// A re-run after a connector ships moves it the other way (FR-071a).
	after := runFixture(t, "synthetic-02", audit.Options{})
	if got := audit.PriorFromAudit(after); !closeTo(got, 0.307692) {
		t.Errorf("PriorFromAudit after the re-run = %v, want 0.307692", got)
	}
}

// TestPriorFromNoAuditIsTotalIgnorance states the default explicitly: no measured ceiling means
// no evidence that any cause is observable, so the open hypothesis carries all the prior mass.
// A silent 0 would publish the opposite claim.
func TestPriorFromNoAuditIsTotalIgnorance(t *testing.T) {
	t.Parallel()

	if got := audit.PriorFromAudit(nil); got != 1 {
		t.Errorf("PriorFromAudit(nil) = %v, want 1", got)
	}
	record := audit.PriorRecordFromAudit(nil)
	if record.Prior != 1 {
		t.Errorf("PriorRecordFromAudit(nil).Prior = %v, want 1", record.Prior)
	}
	if record.AuditID != "" {
		t.Errorf("PriorRecordFromAudit(nil).AuditID = %q, want empty", record.AuditID)
	}
}

// TestPriorRecordKeepsAHistoricalConfidenceInterpretable: ADR-0005 D9 records π₀ with the audit
// it came from, so a confidence stored years ago can still be read.
func TestPriorRecordKeepsAHistoricalConfidenceInterpretable(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	record := audit.PriorRecordFromAudit(result)

	if !closeTo(record.Prior, wantPrior) {
		t.Errorf("prior = %v, want %v", record.Prior, wantPrior)
	}
	if record.AuditID != "synthetic-01" {
		t.Errorf("audit id = %q, want synthetic-01", record.AuditID)
	}
	if !closeTo(record.Ceiling, wantCeiling) {
		t.Errorf("ceiling = %v, want %v", record.Ceiling, wantCeiling)
	}
	if record.FeederSet != "+vendor-notice" {
		t.Errorf("feeder set = %q, want +vendor-notice", record.FeederSet)
	}
	if record.IncidentCount != 13 {
		t.Errorf("incident count = %d, want 13", record.IncidentCount)
	}
	if record.RunAt.IsZero() {
		t.Error("run_at is zero")
	}
	// Prior and ceiling must always be readable as complements of each other.
	if !closeTo(record.Prior+record.Ceiling, 1) {
		t.Errorf("prior %v + ceiling %v != 1", record.Prior, record.Ceiling)
	}
}
