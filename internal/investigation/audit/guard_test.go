// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// guardAgainstSynthetic runs the ceiling guard over one criterion and returns its finding. The
// audit in force is synthetic-01, whose ceiling is 0.615385.
func guardAgainstSynthetic(t *testing.T, criterion audit.Criterion) audit.GuardFinding {
	t.Helper()
	report, err := audit.Guard(&audit.GuardInput{Criteria: []audit.Criterion{criterion}},
		runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true}))
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(report.Findings))
	}
	return report.Findings[0]
}

func TestGuardChecksOnlyTheCeilingBoundedCriteria(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		criterion audit.Criterion
		want      audit.GuardOutcome
		detail    string
	}{
		{
			name: "a ceiling-bounded target below the ceiling passes",
			criterion: audit.Criterion{
				ID: "SC-001", Metric: "pass@1", Target: 0.55,
				Annotation: "[ceiling-bounded]", Audit: "synthetic-01",
			},
			want: audit.GuardPass,
		},
		{
			name: "a ceiling-bounded target exactly at the ceiling passes",
			criterion: audit.Criterion{
				ID: "SC-006", Metric: "top hypothesis is the root cause", Target: 0.615385,
				Annotation: "ceiling-bounded", Audit: "synthetic-01",
			},
			want: audit.GuardPass,
		},
		{
			name: "a ceiling-bounded target above the ceiling fails",
			criterion: audit.Criterion{
				ID: "SC-001", Metric: "pass@1", Target: 0.8,
				Annotation: "[ceiling-bounded]", Audit: "synthetic-01",
			},
			want:   audit.GuardFail,
			detail: "exceeds the ceiling",
		},
		{
			name: "a ceiling-bounded target that cites no audit fails",
			criterion: audit.Criterion{
				ID: "SC-021", Metric: "lift where the prior fails", Target: 0.5,
				Annotation: "[ceiling-bounded]",
			},
			want:   audit.GuardFail,
			detail: "cites no audit",
		},
		{
			name: "a ceiling-bounded target citing a different audit fails",
			criterion: audit.Criterion{
				ID: "SC-001", Metric: "pass@1", Target: 0.5,
				Annotation: "[ceiling-bounded]", Audit: "2026-09",
			},
			want:   audit.GuardFail,
			detail: "cites audit",
		},
		{
			name: "a percentage rather than a fraction fails",
			criterion: audit.Criterion{
				ID: "SC-001", Metric: "pass@1", Target: 55,
				Annotation: "[ceiling-bounded]", Audit: "synthetic-01",
			},
			want:   audit.GuardFail,
			detail: "outside [0, 1]",
		},
		{
			name: "a latency criterion is not checked and is not scaled",
			criterion: audit.Criterion{
				ID: "SC-005", Metric: "time to provisional", Target: 1,
				Annotation: "[not ceiling-bounded: latency]",
			},
			want:   audit.GuardNotChecked,
			detail: "NOT scaled down",
		},
		{
			name: "a validity criterion well above the ceiling is not checked",
			criterion: audit.Criterion{
				ID: "SC-025", Metric: "citation validity", Target: 1,
				Annotation: "[not ceiling-bounded: validity]",
			},
			want: audit.GuardNotChecked,
		},
		{
			name: "a precision criterion is not checked",
			criterion: audit.Criterion{
				ID: "SC-023", Metric: "unobserved ranks first", Target: 0.9,
				Annotation: "not ceiling-bounded: precision",
			},
			want: audit.GuardNotChecked,
		},
		{
			name: "an invariance criterion is not checked",
			criterion: audit.Criterion{
				ID: "SC-003", Metric: "replay divergence", Target: 1,
				Annotation: "[not ceiling-bounded: invariance]",
			},
			want: audit.GuardNotChecked,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			finding := guardAgainstSynthetic(t, test.criterion)
			if finding.Outcome != test.want {
				t.Errorf("outcome = %q, want %q (%s)", finding.Outcome, test.want, finding.Detail)
			}
			if test.detail != "" && !strings.Contains(finding.Detail, test.detail) {
				t.Errorf("detail = %q, want it to mention %q", finding.Detail, test.detail)
			}
		})
	}
}

// TestGuardFailsLoudlyOnAnUnannotatedCriterion is the clause T017 calls out by name: the guard
// fails loudly if a criterion carries no annotation at all.
func TestGuardFailsLoudlyOnAnUnannotatedCriterion(t *testing.T) {
	t.Parallel()

	for _, annotation := range []string{"", "   ", "probably fine", "[bounded]"} {
		t.Run("annotation="+annotation, func(t *testing.T) {
			t.Parallel()
			finding := guardAgainstSynthetic(t, audit.Criterion{
				ID: "SC-099", Metric: "something new", Target: 0.99, Annotation: annotation,
			})
			if finding.Outcome != audit.GuardFail {
				t.Fatalf("outcome = %q, want fail", finding.Outcome)
			}
			if !strings.Contains(finding.Detail, "ceiling annotation") {
				t.Errorf("detail = %q, want it to name the missing annotation", finding.Detail)
			}
		})
	}
}

// TestGuardFailsAMisAnnotatedCriterion: re-annotating is how a target escapes the guard in one
// direction and how a latency target gets wrongly scaled down in the other. FR-071 forbids both.
func TestGuardFailsAMisAnnotatedCriterion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		criterion audit.Criterion
	}{
		{
			name: "a ceiling-bounded criterion annotated as free",
			criterion: audit.Criterion{
				ID: "SC-001", Metric: "pass@1", Target: 0.95,
				Annotation: "[not ceiling-bounded: validity]",
			},
		},
		{
			name: "a latency criterion annotated as ceiling-bounded",
			criterion: audit.Criterion{
				ID: "SC-005", Metric: "time to provisional", Target: 0.6,
				Annotation: "[ceiling-bounded]", Audit: "synthetic-01",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			finding := guardAgainstSynthetic(t, test.criterion)
			if finding.Outcome != audit.GuardFail {
				t.Fatalf("outcome = %q, want fail (%s)", finding.Outcome, finding.Detail)
			}
			if !strings.Contains(finding.Detail, "the specification publishes it as") {
				t.Errorf("detail = %q, want it to cite the published table", finding.Detail)
			}
		})
	}
}

// TestPublishedAnnotationsMatchTheSpecTable pins the three ceiling-bounded rows FR-071 names.
func TestPublishedAnnotationsMatchTheSpecTable(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"SC-001", "SC-006", "SC-021"} {
		annotation, ok := audit.PublishedAnnotation(id)
		if !ok {
			t.Errorf("%s has no published annotation", id)
			continue
		}
		if !annotation.Bounded() {
			t.Errorf("%s is %q, want ceiling-bounded", id, annotation)
		}
	}
	for _, id := range []string{"SC-002", "SC-003", "SC-005", "SC-007", "SC-016", "SC-023", "SC-025"} {
		annotation, ok := audit.PublishedAnnotation(id)
		if !ok {
			t.Errorf("%s has no published annotation", id)
			continue
		}
		if annotation.Bounded() {
			t.Errorf("%s is ceiling-bounded, want not", id)
		}
	}
	if _, ok := audit.PublishedAnnotation("SC-999"); ok {
		t.Error("an unpublished criterion has a published annotation")
	}
}

func TestGuardReportCounts(t *testing.T) {
	t.Parallel()

	input := &audit.GuardInput{Criteria: []audit.Criterion{
		{ID: "SC-001", Target: 0.5, Annotation: "[ceiling-bounded]", Audit: "synthetic-01"},
		{ID: "SC-006", Target: 0.9, Annotation: "[ceiling-bounded]", Audit: "synthetic-01"},
		{ID: "SC-005", Target: 0.95, Annotation: "[not ceiling-bounded: latency]"},
	}}
	report, err := audit.Guard(input, runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true}))
	if err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if report.Checked != 2 || report.Passed != 1 || report.Failed != 1 || report.NotChecked != 1 {
		t.Errorf("checked/passed/failed/not-checked = %d/%d/%d/%d, want 2/1/1/1",
			report.Checked, report.Passed, report.Failed, report.NotChecked)
	}
	if report.OK() {
		t.Error("a report with a failure says it is OK")
	}
	if report.AuditID != "synthetic-01" || !closeTo(report.Ceiling, wantCeiling) {
		t.Errorf("report names audit %q ceiling %v, want synthetic-01 / %v",
			report.AuditID, report.Ceiling, wantCeiling)
	}
}

func TestGuardRefusesToRunWithoutAnAudit(t *testing.T) {
	t.Parallel()

	input := &audit.GuardInput{Criteria: []audit.Criterion{
		{ID: "SC-001", Target: 0.5, Annotation: "[ceiling-bounded]", Audit: "synthetic-01"},
	}}
	_, err := audit.Guard(input, nil)
	if err == nil {
		t.Fatal("Guard ran with no audit to check against")
	}
	if !strings.Contains(err.Error(), "FR-071") {
		t.Errorf("error = %v, want it to cite FR-071", err)
	}
}

func TestGuardRefusesAnEmptyReport(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true})
	if _, err := audit.Guard(&audit.GuardInput{}, result); err == nil {
		t.Error("Guard accepted a report publishing no criteria")
	}
	if _, err := audit.Guard(nil, result); err == nil {
		t.Error("Guard accepted a missing report")
	}
}

// TestParseGuardInputShapes: the report writer that will feed this guard does not exist yet
// (T109/T112), so all three plausible shapes are read.
func TestParseGuardInputShapes(t *testing.T) {
	t.Parallel()

	const criterion = `{"id":"SC-001","metric":"pass@1","target":0.5,"annotation":"[ceiling-bounded]","audit":"synthetic-01"}`
	tests := []struct {
		name string
		raw  string
	}{
		{"object with criteria", `{"criteria":[` + criterion + `]}`},
		{"bare array", `[` + criterion + `]`},
		{"one per line", criterion + "\n" + criterion + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input, err := audit.ParseGuardInput([]byte(test.raw))
			if err != nil {
				t.Fatalf("ParseGuardInput: %v", err)
			}
			if len(input.Criteria) == 0 {
				t.Fatal("no criteria read")
			}
			if input.Criteria[0].ID != "SC-001" {
				t.Errorf("id = %q, want SC-001", input.Criteria[0].ID)
			}
		})
	}

	for _, bad := range []string{"", "   ", "not json at all", `{"criteria": "nope"}`} {
		if _, err := audit.ParseGuardInput([]byte(bad)); err == nil {
			t.Errorf("ParseGuardInput accepted %q", bad)
		}
	}
}
