// SPDX-License-Identifier: Apache-2.0

package retrieval

import "fmt"

// The retrieval gate (008 FR-042, research R10). The lexical floors are the baseline measured on
// fixtures/retrieval when the lexical path landed (008 PR 1, 2026-10-15): 8 of 9 fixtures at rank 1,
// the miss being reworded-01, which shares no word with its postmortem and is the semantic path's
// to find. A change that lowers either fails CI. Moving them is an edit a reviewer sees, made when a
// measurement says the corpus changed.
const (
	// LexicalRecallAt3Floor is the least lexical_only recall@3 the gate accepts.
	LexicalRecallAt3Floor = 0.888889
	// LexicalMRRFloor is the least lexical_only MRR the gate accepts.
	LexicalMRRFloor = 0.888889
)

// Gate returns the reasons the aggregates fail the gate; none means pass. Every mode must cite no
// document a fixture says must never be cited; lexical_only must meet its floors and do at least
// as well as summary_only, 007's retrieval, on both measures.
func Gate(aggregates []Aggregate) []string {
	var failures []string
	byMode := map[string]Aggregate{}
	for _, a := range aggregates {
		byMode[a.Mode] = a
		if a.Violations > 0 {
			failures = append(failures, fmt.Sprintf("%s cited %d document(s) a fixture says must never be cited", a.Mode, a.Violations))
		}
	}
	lexical, ok := byMode[ModeLexicalOnly]
	if !ok {
		return append(failures, "no lexical_only results")
	}
	if lexical.RecallAt3 < LexicalRecallAt3Floor {
		failures = append(failures, fmt.Sprintf("lexical_only recall@3 %.3f is below its floor %.3f", lexical.RecallAt3, LexicalRecallAt3Floor))
	}
	if lexical.MRR < LexicalMRRFloor {
		failures = append(failures, fmt.Sprintf("lexical_only MRR %.3f is below its floor %.3f", lexical.MRR, LexicalMRRFloor))
	}
	if summary, ok := byMode[ModeSummaryOnly]; ok {
		if lexical.RecallAt3 < summary.RecallAt3 || lexical.MRR < summary.MRR {
			failures = append(failures, fmt.Sprintf("lexical_only (recall@3 %.3f, MRR %.3f) does worse than summary_only (%.3f, %.3f)",
				lexical.RecallAt3, lexical.MRR, summary.RecallAt3, summary.MRR))
		}
	}
	return failures
}
