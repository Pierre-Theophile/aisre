// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Coverage against GCP's own answer (T068, FR-144).

// The differences are enumerated, never summarised. "97% coverage" over 300 services is nine missing
// services, and *which* nine decides whether the number is fine or whether the connector is blind to
// an entire region.
func TestTheDifferencesAreEnumeratedRatherThanSummarised(t *testing.T) {
	report := gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface:   gcpfeeder.SurfaceServices,
		Project:   "nova-production",
		Region:    "europe-west1",
		GCPListed: []string{"checkout", "orders", "payments", "search"},
		Produced:  []string{"checkout", "orders"},
	})
	if len(report.Missing) != 2 {
		t.Fatalf("missing = %d, want 2", len(report.Missing))
	}
	rendered := report.String()
	for _, name := range []string{"payments", "search"} {
		if !strings.Contains(rendered, name) {
			t.Errorf("the report does not name the missing service %q; a summary cannot be acted "+
				"on (FR-144):\n%s", name, rendered)
		}
	}
	if report.Complete() {
		t.Error("a report with missing services reported itself complete")
	}
	if report.Matched != 2 || report.Comparable != 4 {
		t.Errorf("matched=%d comparable=%d, want 2 and 4", report.Matched, report.Comparable)
	}
	// The denominator is stated rather than left for a reader to reconstruct.
	if !strings.Contains(rendered, "2 of 4 matched") {
		t.Errorf("the report does not state its denominator:\n%s", rendered)
	}
}

// Extra is not symmetric with missing. A revision GCP has garbage-collected is still a fact about
// the past, so it is reported separately and does not count against coverage — otherwise a connector
// would improve its score by forgetting history.
func TestAnExtraEntityInsideTheHorizonDoesNotCountAgainstCoverage(t *testing.T) {
	horizonStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	report := gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface:   gcpfeeder.SurfaceRevisions,
		GCPListed: []string{"checkout-00042-abc"},
		Produced:  []string{"checkout-00042-abc", "checkout-00041-xyz"},
		Horizon:   gcpfeeder.Horizon{Earliest: horizonStart, Reason: gcpfeeder.HorizonConfigured},
		CreatedAt: map[string]time.Time{
			"checkout-00042-abc": horizonStart.Add(24 * time.Hour),
			"checkout-00041-xyz": horizonStart.Add(12 * time.Hour),
		},
	})
	if !report.Complete() {
		t.Fatalf("a garbage-collected revision made the report incomplete: %s", report)
	}
	if len(report.Extra) != 1 {
		t.Fatalf("extra = %d, want 1", len(report.Extra))
	}
	if report.Extra[0].Why != gcpfeeder.DiffExtraInsideHorizon {
		t.Errorf("reason = %q, want the inside-horizon reading", report.Extra[0].Why)
	}

	// Outside any plausible retention it is a stale assertion the silence rule should have
	// retracted — a different bug with a different fix, so it reads differently.
	stale := gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface:   gcpfeeder.SurfaceRevisions,
		GCPListed: []string{},
		Produced:  []string{"ancient"},
		Horizon:   gcpfeeder.Horizon{Earliest: horizonStart, Reason: gcpfeeder.HorizonConfigured},
		CreatedAt: map[string]time.Time{"ancient": horizonStart.Add(-30 * 24 * time.Hour)},
	})
	if len(stale.Extra) != 1 || stale.Extra[0].Why != gcpfeeder.DiffExtraOutsideHorizon {
		t.Fatalf("a stale assertion read as %+v", stale.Extra)
	}
}

// A revision GCP lists that predates the horizon is not a gap: the feeder was never asked to know
// about it, and counting it would make the coverage number a function of the estate's age.
func TestARevisionBelowTheHorizonIsNotAGap(t *testing.T) {
	horizonStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	report := gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface:   gcpfeeder.SurfaceRevisions,
		GCPListed: []string{"recent", "ancient"},
		Produced:  []string{"recent"},
		Horizon:   gcpfeeder.Horizon{Earliest: horizonStart, Reason: gcpfeeder.HorizonConfigured},
		CreatedAt: map[string]time.Time{
			"recent":  horizonStart.Add(24 * time.Hour),
			"ancient": horizonStart.Add(-24 * time.Hour),
		},
	})
	if !report.Complete() {
		t.Fatalf("a revision below the horizon counted as missing: %s", report)
	}
	if len(report.BelowHorizon) != 1 || report.Comparable != 1 {
		t.Fatalf("belowHorizon=%d comparable=%d, want 1 and 1", len(report.BelowHorizon), report.Comparable)
	}
	// It is reported rather than silently dropped, so a reader can see it was considered.
	if !strings.Contains(report.String(), "ancient") {
		t.Errorf("the excluded revision is not reported:\n%s", report)
	}
}

// An undated entry is compared, and therefore counted if absent: a revision the feeder did not
// produce is a real gap until somebody shows otherwise.
func TestAnUndatedEntryIsCountedRatherThanExcused(t *testing.T) {
	report := gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface:   gcpfeeder.SurfaceRevisions,
		GCPListed: []string{"undated"},
		Produced:  nil,
		Horizon:   gcpfeeder.Horizon{Earliest: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	if report.Complete() {
		t.Fatal("an undated entry the feeder did not produce was excused by the horizon")
	}
}

// Surfaces are compared separately: a connector can be complete in one and blind in another, and one
// blended number would hide that.
func TestSurfacesAreReportedSeparately(t *testing.T) {
	var audit gcpfeeder.Audit
	audit.Add(gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface: gcpfeeder.SurfaceServices, Project: "nova-production", Region: "europe-west1",
		GCPListed: []string{"checkout"}, Produced: []string{"checkout"},
	}))
	audit.Add(gcpfeeder.Compare(gcpfeeder.Comparison{
		Surface: gcpfeeder.SurfaceAlertPolicies, Project: "nova-production", Region: "europe-west1",
		GCPListed: []string{"policy-1", "policy-2"}, Produced: nil,
	}))
	if audit.Complete() {
		t.Fatal("an audit with a blind surface reported itself complete")
	}
	if audit.MissingTotal() != 2 {
		t.Errorf("missing total = %d, want 2", audit.MissingTotal())
	}
	rendered := audit.String()
	if !strings.Contains(rendered, string(gcpfeeder.SurfaceServices)) ||
		!strings.Contains(rendered, string(gcpfeeder.SurfaceAlertPolicies)) {
		t.Errorf("the audit blends the surfaces:\n%s", rendered)
	}
	if !strings.Contains(rendered, "policy-1") || !strings.Contains(rendered, "policy-2") {
		t.Errorf("the audit does not enumerate the blind surface's gaps:\n%s", rendered)
	}
	// Deterministic: it reaches a report, and a report that reorders makes a diff unreadable.
	for i := 0; i < 5; i++ {
		if again := audit.String(); again != rendered {
			t.Fatalf("the audit is not deterministic")
		}
	}
}

// Nothing to compare is a ratio of 1 rather than a division by zero, and a complete audit says so.
func TestAnEmptyComparisonIsCompleteRatherThanUndefined(t *testing.T) {
	report := gcpfeeder.Compare(gcpfeeder.Comparison{Surface: gcpfeeder.SurfaceServices})
	if !report.Complete() || report.Ratio() != 1 {
		t.Fatalf("an empty comparison: complete=%v ratio=%v", report.Complete(), report.Ratio())
	}
	var audit gcpfeeder.Audit
	audit.Add(report)
	if !strings.Contains(audit.String(), "every surface complete") {
		t.Errorf("a complete audit does not say so:\n%s", audit)
	}
}
