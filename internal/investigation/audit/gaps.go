// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Corpus-gap detection (T018, FR-071b).
//
// The audit's unobservable remainder is the set of incidents on which the correct answer is
// "no observed change explains this". FR-071b turns that from an apology into corpus work:
// every category of the remainder owes the evaluation corpus at least one fixture whose ground
// truth is `unobserved` or `not_change_induced` carrying that category, graded under SC-023,
// **and a category with no such fixture must be reported as a gap on every evaluation run**.
//
// This file is the detection half only. The wiring into the evaluation report belongs to T109,
// in Phase 8, where the report writer lives. That split is deliberate: the detector is pure and
// testable today, before a single incident fixture exists, and today it correctly reports every
// remainder category as a gap — `fixtures/incidents/` holds a README and nothing else.
//
// A gap is a warning, not a build failure. The corpus being incomplete is a fact about the
// corpus, and failing the build over it would only teach people to stop measuring; the
// evaluation report names the gap on every run instead, which is what FR-071b asks for.
//
// The manifest is read with a deliberately tolerant, self-contained parser rather than through
// `internal/fixture`'s loader. The incident manifest's `incident:` block arrives with T097 and
// the 001 loader is strict about unknown keys, so borrowing it would make this detector fail on
// exactly the fixtures it exists to find.

// FixtureRoot is where incident fixtures live (contracts/incident-format.md).
const FixtureRoot = "fixtures/incidents"

// Gap is one category of the unobservable remainder with no fixture to its name.
type Gap struct {
	// Category is the cause category the corpus is missing.
	Category Category `json:"category"`
	// Classes are the cause classes a fixture for it must carry as ground truth.
	Classes []CauseClass `json:"classes"`
	// Incidents is how many audited incidents fall in this category.
	Incidents int `json:"incidents"`
	// Share is that count over the classifiable set.
	Share float64 `json:"share"`
	// Want is the ground truth a fixture closing this gap has to publish.
	Want string `json:"want"`
}

// CategoryCoverage is one remainder category that the corpus does cover.
type CategoryCoverage struct {
	// Category is the covered category.
	Category Category `json:"category"`
	// Fixtures are the fixture directory names that cover it.
	Fixtures []string `json:"fixtures"`
}

// GapReport is the detector's whole answer.
type GapReport struct {
	// AuditID and FeederSet identify the remainder that was checked.
	AuditID   string `json:"audit_id"`
	FeederSet string `json:"feeder_set"`
	// FixtureRoot is the directory that was scanned.
	FixtureRoot string `json:"fixture_root"`
	// FixturesScanned is how many fixture directories carried a readable manifest.
	FixturesScanned int `json:"fixtures_scanned"`
	// GroundTruthFound is how many of them declared an `unobserved` or `not_change_induced`
	// ground truth with a category.
	GroundTruthFound int `json:"ground_truth_found"`
	// Covered is the remainder categories the corpus covers.
	Covered []CategoryCoverage `json:"covered"`
	// Gaps is the remainder categories it does not (FR-071b).
	Gaps []Gap `json:"gaps"`
	// Unreadable names manifests that could not be parsed, so a broken fixture reads as a
	// broken fixture rather than as a missing category.
	Unreadable []string `json:"unreadable,omitempty"`
	// Warning is the line an evaluation run prints when there are gaps, and is empty when
	// there are none.
	Warning string `json:"warning,omitempty"`
}

// OK reports whether the corpus covers every category of the remainder.
func (r *GapReport) OK() bool { return len(r.Gaps) == 0 }

// fixtureGroundTruth is one fixture's declared ground truth, reduced to what gap detection
// needs.
type fixtureGroundTruth struct {
	// Fixture is the directory name.
	Fixture string
	// Class is `unobserved` or `not_change_induced`.
	Class CauseClass
	// Category is the category the fixture claims to represent.
	Category Category
}

// DetectGaps reports which categories of the audit's unobservable remainder have no fixture
// under root. A root that does not exist is not an error: it means every category is a gap,
// which is exactly the state the corpus is in before T102 lands.
func DetectGaps(result *Result, root string) (*GapReport, error) {
	if result == nil {
		return nil, fmt.Errorf("audit: gaps: no audit result")
	}
	if root == "" {
		root = FixtureRoot
	}

	report := &GapReport{
		AuditID:     result.AuditID,
		FeederSet:   result.FeederSetInForce,
		FixtureRoot: root,
		Covered:     []CategoryCoverage{},
		Gaps:        []Gap{},
	}

	found, scanned, unreadable, err := scanFixtures(root)
	if err != nil {
		return nil, err
	}
	report.FixturesScanned = scanned
	report.GroundTruthFound = len(found)
	report.Unreadable = unreadable

	byCategory := map[Category][]string{}
	for _, truth := range found {
		byCategory[truth.Category] = append(byCategory[truth.Category], truth.Fixture)
	}

	for _, row := range result.Remainder {
		fixtures := byCategory[row.Category]
		if len(fixtures) > 0 {
			slices.Sort(fixtures)
			report.Covered = append(report.Covered, CategoryCoverage{
				Category: row.Category,
				Fixtures: fixtures,
			})
			continue
		}
		report.Gaps = append(report.Gaps, Gap{
			Category:  row.Category,
			Classes:   slices.Clone(row.Classes),
			Incidents: row.Count,
			Share:     row.Share,
			Want:      wantGroundTruth(row),
		})
	}

	if len(report.Gaps) > 0 {
		names := make([]string, len(report.Gaps))
		for i, gap := range report.Gaps {
			names[i] = string(gap.Category)
		}
		report.Warning = fmt.Sprintf(
			"corpus gap: %d of the %d unobservable-remainder categories of audit %s have no fixture "+
				"under %s (%s). FR-071b: each needs one whose ground truth is `unobserved` or "+
				"`not_change_induced` carrying that category, graded under SC-023",
			len(report.Gaps), len(result.Remainder), result.AuditID, root, strings.Join(names, ", "))
	}
	return report, nil
}

func wantGroundTruth(row CategoryCount) string {
	classes := make([]string, len(row.Classes))
	for i, class := range row.Classes {
		classes[i] = string(class)
	}
	if len(classes) == 0 {
		classes = []string{string(CauseUnobserved), string(CauseNotChangeInduced)}
	}
	return fmt.Sprintf("a fixture whose incident.ground_truth is %s with category %s",
		strings.Join(classes, " or "), row.Category)
}

func scanFixtures(root string) (found []fixtureGroundTruth, scanned int, unreadable []string, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil, nil
		}
		return nil, 0, nil, fmt.Errorf("audit: gaps: read %s: %w", root, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "manifest.yaml")
		raw, readErr := os.ReadFile(path) //nolint:gosec // path is built from the scanned root.
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				unreadable = append(unreadable, path)
			}
			continue
		}
		scanned++
		truth, ok, parseErr := groundTruthOf(entry.Name(), raw)
		if parseErr != nil {
			unreadable = append(unreadable, path)
			continue
		}
		if ok {
			found = append(found, truth)
		}
	}
	return found, scanned, unreadable, nil
}

// groundTruthOf extracts `incident.ground_truth` from a manifest.
//
// The published shape writes the culprit either as the scalar `unobserved` or as
// `not_change_induced: <category>`, and a fixture may carry the category in a sibling field
// instead. All of those spellings are read; anything else is simply "no remainder ground truth
// here", which is the right answer for an ordinary incident fixture with a real culprit.
func groundTruthOf(fixture string, raw []byte) (fixtureGroundTruth, bool, error) {
	var manifest struct {
		Incident struct {
			GroundTruth map[string]any `yaml:"ground_truth"`
		} `yaml:"incident"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return fixtureGroundTruth{}, false, fmt.Errorf("audit: gaps: %s: %w", fixture, err)
	}
	groundTruth := manifest.Incident.GroundTruth
	if groundTruth == nil {
		return fixtureGroundTruth{}, false, nil
	}

	class, category := readCulprit(groundTruth["culprit"])
	if class == "" {
		return fixtureGroundTruth{}, false, nil
	}
	if category == "" {
		category = Category(stringOf(groundTruth["category"]))
	}
	if !category.Valid() {
		return fixtureGroundTruth{}, false, nil
	}
	return fixtureGroundTruth{Fixture: fixture, Class: class, Category: category}, true, nil
}

// readCulprit reads the culprit field in any of its published spellings.
func readCulprit(value any) (CauseClass, Category) {
	switch typed := value.(type) {
	case string:
		return readCulpritString(typed)
	case map[string]any:
		for _, class := range []CauseClass{CauseNotChangeInduced, CauseUnobserved} {
			if nested, ok := typed[string(class)]; ok {
				return class, readNestedCategory(nested)
			}
		}
	}
	return "", ""
}

func readCulpritString(value string) (CauseClass, Category) {
	trimmed := strings.TrimSpace(value)
	if trimmed == string(CauseUnobserved) {
		return CauseUnobserved, ""
	}
	prefix := string(CauseNotChangeInduced) + ":"
	if rest, ok := strings.CutPrefix(trimmed, prefix); ok {
		return CauseNotChangeInduced, Category(strings.TrimSpace(rest))
	}
	if trimmed == string(CauseNotChangeInduced) {
		return CauseNotChangeInduced, ""
	}
	return "", ""
}

func readNestedCategory(value any) Category {
	switch typed := value.(type) {
	case string:
		return Category(strings.TrimSpace(typed))
	case map[string]any:
		return Category(stringOf(typed["category"]))
	default:
		return ""
	}
}

func stringOf(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}
