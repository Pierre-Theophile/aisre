// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// The corpus-gap line (T018's wiring half, moved here by analyze O2; FR-071b).
//
// `audit.DetectGaps` has existed since Phase 2 and has never been called by anything that
// publishes. That is the defect this file closes: detection with no wiring is a function with a
// test, not a report line, and FR-071b asks for the gap to be **named on every evaluation run**.
//
// Three properties the wiring has to preserve, each of which the detector already decided and
// this file must not quietly re-decide:
//
//  1. **A gap is a warning, not a build failure.** The corpus being incomplete is a fact about
//     the corpus; failing the build over it would teach people to stop measuring. The row is
//     emitted, `check-report.sh` prints it, and nothing exits non-zero because of it.
//  2. **Every remainder category is named**, covered or not, because "3 gaps" is not actionable
//     and "latent_bug, credential_leak and business_data_change have no fixture" is.
//  3. **A missing audit is not "no gaps".** An evaluation run that could not read its audit
//     publishes an unmeasured row saying so, which is the difference between a clean corpus and
//     an unchecked one.

// GapRows is the corpus-gap line: one row naming every audit category with no fixture, plus one
// row per gap so a reader who greps for a category finds it.
//
// `auditPath` is the published audit this run cites (FR-071 requires every run to name one) and
// `fixtureRoot` the directory the corpus was scanned in.
func GapRows(auditPath, fixtureRoot string) []Row {
	result, err := audit.LoadResult(auditPath)
	if err != nil {
		return []Row{NotMeasured(MetricCorpusGaps, ScopeCorpus, "", fmt.Sprintf(
			"the coverage audit at %s could not be read (%v), so the corpus was not checked for "+
				"gaps; FR-071b needs an audit to have a remainder to check against", auditPath, err))}
	}
	report, err := audit.DetectGaps(result, fixtureRoot)
	if err != nil {
		return []Row{NotMeasured(MetricCorpusGaps, ScopeCorpus, "", fmt.Sprintf(
			"corpus-gap detection over %s failed: %v", fixtureRoot, err))}
	}
	return GapRowsFrom(report)
}

// GapRowsFrom renders an already-computed gap report, so a caller that has one (the CLI, a test)
// does not read the audit twice.
func GapRowsFrom(report *audit.GapReport) []Row {
	if report == nil {
		return []Row{NotMeasured(MetricCorpusGaps, ScopeCorpus, "", "no gap report was produced")}
	}

	detail := report.Warning
	if detail == "" {
		detail = fmt.Sprintf(
			"every unobservable-remainder category of audit %s has at least one fixture under %s "+
				"(%d fixtures scanned, %d carrying remainder ground truth)",
			report.AuditID, report.FixtureRoot, report.FixturesScanned, report.GroundTruthFound)
	}
	if len(report.Unreadable) > 0 {
		detail += fmt.Sprintf("; %d manifest(s) could not be parsed and were not counted as coverage: %s",
			len(report.Unreadable), strings.Join(report.Unreadable, ", "))
	}

	rows := []Row{NewRow(MetricCorpusGaps, ScopeCorpus, "",
		float64(len(report.Gaps)), len(report.Gaps)+len(report.Covered), detail)}

	for _, gap := range report.Gaps {
		rows = append(rows, NewRow(MetricCorpusGaps+"."+string(gap.Category), ScopeCorpus, "",
			1, gap.Incidents, fmt.Sprintf(
				"no fixture covers %s (%d of the audited incidents, %.1f %% of the classifiable set); want %s",
				gap.Category, gap.Incidents, gap.Share*100, gap.Want)))
	}
	for _, covered := range report.Covered {
		rows = append(rows, NewRow(MetricCorpusGaps+"."+string(covered.Category), ScopeCorpus, "",
			0, len(covered.Fixtures), fmt.Sprintf("covered by %s",
				strings.Join(covered.Fixtures, ", "))))
	}
	return rows
}

// CoverageCeiling reads the ceiling the published audit measured, which bounds every
// recall- and coverage-dependent threshold (FR-071). It is read from the same file the gap rows
// cite, so a run cannot bound its threshold against one audit and name another.
func CoverageCeiling(auditPath string) (float64, string, error) {
	result, err := audit.LoadResult(auditPath)
	if err != nil {
		return 0, "", fmt.Errorf("eval: read coverage audit %s: %w", auditPath, err)
	}
	return result.Ceiling, result.AuditID, nil
}
