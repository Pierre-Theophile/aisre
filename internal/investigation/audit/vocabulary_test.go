// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// TestCategoriesMatchTheMigration is the test that keeps two lists from drifting: the categories
// this package publishes and the ones `investigation.coverage_audit_items.category` accepts. A
// value added to one and not the other would be an audit that runs, publishes a ceiling, and
// then fails at the INSERT — or worse, one that quietly narrows the category set the next
// auditor can choose from.
//
// The constraint is defined by 0006 and redefined, six values wider, by 0007. The test reads
// whichever migration defines it last rather than naming one, so the next widening is a
// migration and a constant and nothing else.
func TestCategoriesMatchTheMigration(t *testing.T) {
	t.Parallel()

	file, fromMigration := latestCategoryConstraint(t)

	published := audit.Categories()
	if len(fromMigration) != len(published) {
		t.Fatalf("%s accepts %d categories, the package publishes %d:\n  migration: %v\n  package:   %v",
			file, len(fromMigration), len(published), fromMigration, published)
	}
	for i, category := range published {
		if string(category) != fromMigration[i] {
			t.Errorf("category %d: package has %q, %s has %q", i, category, file, fromMigration[i])
		}
	}
}

// TestTheCategoryVocabularyOnlyEverGrows: 0007 widened the set additively, and the eleven values
// 0006 accepted before `other` must still be accepted, in the same order. A row written under
// the twelve has to stay a row that reads.
func TestTheCategoryVocabularyOnlyEverGrows(t *testing.T) {
	t.Parallel()

	original := []string{
		"flag_flip", "iac_apply", "db_migration", "certificate_expiry", "scheduled_job",
		"third_party_outage", "traffic_shift", "latent_bug", "client_side_configuration",
		"business_data_change", "credential_leak", "other",
	}
	published := audit.Categories()
	if len(published) < len(original) {
		t.Fatalf("the published set has shrunk to %d values: %v", len(published), published)
	}

	// Every original value is still published, and their relative order is unchanged.
	at := -1
	for _, want := range original {
		found := -1
		for i, category := range published {
			if string(category) == want {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("category %q was removed from the published set", want)
		}
		if found <= at {
			t.Errorf("category %q moved ahead of a value that used to precede it", want)
		}
		at = found
	}

	// And the six 0007 added are there, after the originals apart from the catch-all.
	for _, want := range []audit.Category{
		audit.CategoryDeployment, audit.CategoryConfigurationChange, audit.CategoryCloudMaintenance,
		audit.CategoryVendorAPIChange, audit.CategoryCapacityLimit, audit.CategoryInfrastructureIncident,
	} {
		if !want.Valid() {
			t.Errorf("category %q added by 0007 is not published", want)
		}
	}
	if published[len(published)-1] != audit.CategoryOther {
		t.Errorf("last published category = %q, want other — the catch-all sorts last",
			published[len(published)-1])
	}
}

// latestCategoryConstraint returns the name of the highest-numbered migration that DEFINES
// `coverage_audit_items_category_check`, and the values it accepts in their declared order. A
// migration that only drops the constraint on its way to redefining it does not count, and
// comments are stripped first so that a value named in prose is not read as a member.
func latestCategoryConstraint(t *testing.T) (string, []string) {
	t.Helper()

	dir := filepath.Join("..", "..", "store", "postgres", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	comments := regexp.MustCompile(`(?m)--.*$`)
	// `CONSTRAINT coverage_audit_items_category_check CHECK (category IS NULL OR category IN (…))`
	definition := regexp.MustCompile(
		`(?s)CONSTRAINT\s+coverage_audit_items_category_check\s+CHECK\s*\((.*?)\)\)`)
	value := regexp.MustCompile(`'([a-z_]+)'`)

	for _, name := range names {
		raw, readErr := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a test-owned path.
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		block := definition.FindStringSubmatch(comments.ReplaceAllString(string(raw), ""))
		if len(block) != 2 {
			continue
		}
		var values []string
		for _, quoted := range value.FindAllStringSubmatch(block[1], -1) {
			values = append(values, quoted[1])
		}
		return name, values
	}
	t.Fatalf("no migration in %s defines coverage_audit_items_category_check", dir)
	return "", nil
}

// TestClassificationsMatchTheMigration does the same for the three-value classification set.
func TestClassificationsMatchTheMigration(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "store", "postgres", "migrations", "0006_investigation.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for _, classification := range []audit.Classification{
		audit.ClassificationCausePresent,
		audit.ClassificationCauseAbsent,
		audit.ClassificationUndecidable,
	} {
		if !strings.Contains(string(raw), "'"+string(classification)+"'") {
			t.Errorf("the migration does not accept classification %q", classification)
		}
	}
}

func TestClosedSetsValidateTheirOwnMembers(t *testing.T) {
	t.Parallel()

	for _, category := range audit.Categories() {
		if !category.Valid() {
			t.Errorf("published category %q reports itself invalid", category)
		}
	}
	if audit.Category("not_a_category").Valid() {
		t.Error("an invented category reports itself valid")
	}

	for _, verdict := range audit.Verdicts() {
		if !verdict.Valid() {
			t.Errorf("published verdict %q reports itself invalid", verdict)
		}
	}
	if audit.Verdict("maybe").Valid() {
		t.Error("an invented verdict reports itself valid")
	}

	for _, class := range audit.CauseClasses() {
		if !class.Valid() {
			t.Errorf("published cause class %q reports itself invalid", class)
		}
	}
	for _, reason := range audit.Reasons() {
		if !reason.Valid() {
			t.Errorf("published reason %q reports itself invalid", reason)
		}
	}
	for _, reason := range audit.ExclusionReasons() {
		if !reason.Valid() {
			t.Errorf("published exclusion reason %q reports itself invalid", reason)
		}
	}
	for _, annotation := range audit.Annotations() {
		if !annotation.Valid() {
			t.Errorf("published annotation %q reports itself invalid", annotation)
		}
	}
}

// TestCauseClassesThatOweFixtures: FR-071b's obligation attaches to the two classes on which the
// correct engine answer is that no observed change explains the symptom.
func TestCauseClassesThatOweFixtures(t *testing.T) {
	t.Parallel()

	tests := map[audit.CauseClass]bool{
		audit.CauseChangeInduced:    false,
		audit.CauseNotChangeInduced: true,
		audit.CauseUnobserved:       true,
	}
	for class, want := range tests {
		if got := class.NeedsFixture(); got != want {
			t.Errorf("%s.NeedsFixture() = %v, want %v", class, got, want)
		}
	}
}
