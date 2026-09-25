// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// writeFixture writes a minimal incident fixture manifest carrying one ground truth.
func writeFixture(t *testing.T, root, name, manifest string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// TestTheShippedCorpusCoversEveryRemainderCategory is FR-071b, measured against the fixtures
// this repository actually ships.
//
// When T018 wrote this detector the assertion was the opposite one — `fixtures/incidents/` held
// a README and nothing else, so every category of the unobservable remainder was a gap and the
// test said so. T102 landed the four fixtures that close it, and this is what the test became:
// the corpus covers every category of the 2026-09 audit's remainder, and each covering fixture
// is named so that deleting one fails here rather than quietly lowering the corpus's coverage.
//
// A gap is a warning rather than a build failure in the evaluation report (a corpus being
// incomplete is a fact about the corpus, and failing the build over it teaches people to stop
// measuring). It is an assertion *here*, because here the question is not "is the corpus
// complete today" but "did the fixtures we shipped to close FR-071b stay shipped".
func TestTheShippedCorpusCoversEveryRemainderCategory(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	report, err := audit.DetectGaps(result, filepath.Join("..", "..", "..", audit.FixtureRoot))
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}

	if !report.OK() {
		t.Fatalf("corpus gaps: %v (%s)", report.Gaps, report.Warning)
	}
	if report.Warning != "" {
		t.Errorf("no gaps, but a warning was produced: %s", report.Warning)
	}
	if len(report.Covered) != len(result.Remainder) {
		t.Fatalf("covered = %d categories, remainder = %d", len(report.Covered), len(result.Remainder))
	}

	// Which fixture closes which category is the part worth pinning: a category that became
	// "covered" because somebody renamed an unrelated fixture into the right category would
	// otherwise pass silently.
	covering := map[audit.Category][]string{}
	for _, covered := range report.Covered {
		if len(covered.Fixtures) == 0 {
			t.Errorf("%s is reported covered by no fixture", covered.Category)
		}
		covering[covered.Category] = covered.Fixtures
	}
	for category, fixture := range map[audit.Category]string{
		audit.CategoryLatentBug:               "unobserved-latent-bug-01",
		audit.CategoryClientSideConfiguration: "unobserved-client-config-01",
		audit.CategoryBusinessDataChange:      "unobserved-business-data-01",
		audit.CategoryCredentialLeak:          "unobserved-credential-leak-01",
	} {
		fixtures, inRemainder := covering[category]
		if !inRemainder {
			// Not every category is in every audit's remainder — `latent_bug` is
			// symptom-visible under the feeder set the published 2026-09 audit has in force,
			// and `credential_leak` is not in this synthetic one. The fixture is shipped for
			// all four regardless, because FR-071b names all four and a feeder set that stops
			// seeing a symptom puts its category back into the remainder.
			continue
		}
		if !slices.Contains(fixtures, fixture) {
			t.Errorf("%s is covered by %v, want %s among them", category, fixtures, fixture)
		}
	}
	if report.GroundTruthFound < 4 {
		t.Errorf("ground truths found = %d; the corpus ships four remainder fixtures, one per "+
			"category FR-071b names", report.GroundTruthFound)
	}
}

// TestDetectGapsReportsAnEmptyCorpusAsAllGaps is the same detector over a corpus that has none
// of the fixtures, which is what the report has to say on a repository where somebody points it
// at the wrong directory — and what it said here before T102 landed.
func TestDetectGapsReportsAnEmptyCorpusAsAllGaps(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	report, err := audit.DetectGaps(result, t.TempDir())
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}

	if len(report.Gaps) != len(result.Remainder) {
		t.Fatalf("gaps = %d, want %d (one per remainder category, the corpus being empty)",
			len(report.Gaps), len(result.Remainder))
	}
	if len(report.Covered) != 0 {
		t.Errorf("covered = %v, want none", report.Covered)
	}
	if report.OK() {
		t.Error("a corpus with no fixtures reports no gaps")
	}
	if report.Warning == "" {
		t.Error("gaps were found but no warning was produced")
	}
	if !strings.Contains(report.Warning, "FR-071b") {
		t.Errorf("warning = %q, want it to cite FR-071b", report.Warning)
	}
	for _, gap := range report.Gaps {
		if gap.Want == "" {
			t.Errorf("%s: no statement of what a fixture must carry", gap.Category)
		}
		if gap.Incidents == 0 {
			t.Errorf("%s: gap reported with no incidents behind it", gap.Category)
		}
	}
}

// TestDetectGapsReadsTheGroundTruthSpellings: the published shape writes the culprit as a bare
// `unobserved` or as `not_change_induced: <category>`, and a fixture may carry the category in a
// sibling field. All of them have to be read, or a covered category reads as a gap.
func TestDetectGapsReadsTheGroundTruthSpellings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "unobserved-latent-bug-01", `
id: unobserved-latent-bug-01
incident:
  ground_truth:
    culprit: unobserved
    category: latent_bug
`)
	writeFixture(t, root, "unobserved-client-config-01", `
id: unobserved-client-config-01
incident:
  ground_truth:
    culprit: "not_change_induced: client_side_configuration"
`)
	writeFixture(t, root, "unobserved-business-data-01", `
id: unobserved-business-data-01
incident:
  ground_truth:
    culprit:
      not_change_induced: business_data_change
`)
	// An ordinary incident fixture with a real culprit covers nothing and must not be
	// mistaken for coverage of anything.
	writeFixture(t, root, "rollout-regression-01-incident", `
id: rollout-regression-01-incident
incident:
  ground_truth:
    culprit: k8s.change=shop/payments@rev7
`)
	// A 001 fixture with no incident block at all is simply skipped.
	writeFixture(t, root, "baseline-topology-01", "id: baseline-topology-01\n")

	result := runFixture(t, "synthetic-01", audit.Options{})
	report, err := audit.DetectGaps(result, root)
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}

	if !report.OK() {
		t.Errorf("gaps = %+v, want none: every remainder category has a fixture", report.Gaps)
	}
	if got, want := len(report.Covered), 3; got != want {
		t.Fatalf("covered categories = %d, want %d", got, want)
	}
	if report.GroundTruthFound != 3 {
		t.Errorf("ground truths found = %d, want 3", report.GroundTruthFound)
	}
	if report.FixturesScanned != 5 {
		t.Errorf("fixtures scanned = %d, want 5", report.FixturesScanned)
	}
	if report.Warning != "" {
		t.Errorf("warning = %q, want none", report.Warning)
	}
}

func TestDetectGapsReportsAPartiallyCoveredCorpus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "unobserved-latent-bug-01", `
incident:
  ground_truth:
    culprit: unobserved
    category: latent_bug
`)

	result := runFixture(t, "synthetic-01", audit.Options{})
	report, err := audit.DetectGaps(result, root)
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}

	if len(report.Covered) != 1 || report.Covered[0].Category != audit.CategoryLatentBug {
		t.Fatalf("covered = %+v, want latent_bug alone", report.Covered)
	}
	if report.Covered[0].Fixtures[0] != "unobserved-latent-bug-01" {
		t.Errorf("covering fixture = %q, want unobserved-latent-bug-01", report.Covered[0].Fixtures[0])
	}
	if len(report.Gaps) != 2 {
		t.Fatalf("gaps = %d, want 2", len(report.Gaps))
	}
	for _, gap := range report.Gaps {
		if gap.Category == audit.CategoryLatentBug {
			t.Error("a covered category is reported as a gap")
		}
	}
}

// TestDetectGapsOnAMissingRootIsNotAnError: before T102 the directory may not even exist, and
// "there is nothing there" is a gap report, not a crash.
func TestDetectGapsOnAMissingRootIsNotAnError(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	report, err := audit.DetectGaps(result, filepath.Join(t.TempDir(), "nothing-here"))
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}
	if report.FixturesScanned != 0 {
		t.Errorf("fixtures scanned = %d, want 0", report.FixturesScanned)
	}
	if len(report.Gaps) != len(result.Remainder) {
		t.Errorf("gaps = %d, want %d", len(report.Gaps), len(result.Remainder))
	}
}

// TestDetectGapsReportsAnUnreadableManifestAsSuch: a broken fixture must read as a broken
// fixture, not as a missing category.
func TestDetectGapsReportsAnUnreadableManifestAsSuch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "broken-01", "incident: [this is not a mapping\n")

	result := runFixture(t, "synthetic-01", audit.Options{})
	report, err := audit.DetectGaps(result, root)
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}
	if len(report.Unreadable) != 1 {
		t.Errorf("unreadable = %v, want one entry", report.Unreadable)
	}
}

func TestDetectGapsRequiresAnAudit(t *testing.T) {
	t.Parallel()

	if _, err := audit.DetectGaps(nil, t.TempDir()); err == nil {
		t.Error("DetectGaps ran without an audit")
	}
}

// TestDetectGapsOnAnAuditWithNoRemainder: an organisation whose every cause is observable owes
// the corpus nothing, and the detector must say so rather than invent categories.
func TestDetectGapsOnAnAuditWithNoRemainder(t *testing.T) {
	t.Parallel()

	list, err := audit.ParseList([]byte(`
version: 1
audit_id: t-no-remainder
run_at: 2026-09-17
author: test|auditor
corpus: {label: test-org, from: 2026-01-01, to: 2026-09-01}
feeder_sets: [{name: base, order: 1, feeders: [otel.spans]}]
incidents:
  - ref: T-01
    alert_at: 2026-02-01
    cause: {category: iac_apply, class: change_induced}
    observability: {base: observed}
`))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	result, err := audit.Run(list, audit.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	report, err := audit.DetectGaps(result, t.TempDir())
	if err != nil {
		t.Fatalf("DetectGaps: %v", err)
	}
	if !report.OK() || len(report.Gaps) != 0 {
		t.Errorf("gaps = %+v, want none", report.Gaps)
	}
}
