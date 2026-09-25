// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// `error_spans`: the absent source, answered properly (T102; FR-091, FR-087b, SC-011; contract §7).
//
// This organisation has **no trace data source**. The term is still served — a term a backend
// declares and then refuses is a term no planner can reason about — and it is answered the way the
// contract requires of every backend with a missing source:
//
//   - outcome **NO_DATA**;
//   - coverage **names the absent source** and **states that nothing was searched**;
//   - **never** QUERY_FAILED, never an unexplained empty digest, never a substituted source, never
//     a silent single group;
//   - **identical in live and recorded mode**, and **stable for the whole window**.
//
// The point is negative and it is the whole requirement: a consumer **must not** be able to read
// this answer as evidence that there were no error spans. "There is no tracing here" and "the
// traces show no errors" are opposite conclusions, and an empty TraceDigest with a coverage block
// that did not distinguish them would licence the second.
//
// There is no code path below that queries anything, and that is the implementation. It does not
// call a trace API and find nothing; it reports that there is none to call. The term becomes live
// **without a contract change** if Cloud Trace is ever enabled — `gcp-trace-filter/v1` is
// registered and unminted for exactly that reason — and the shape of this answer is what a caller
// would stop seeing.

// TraceSourceAbsent is the sentence every error_spans answer carries. It is a constant rather than
// a formatted string so that the answer is **byte-identical for every edge and every window**:
// a consumer that sees the same sentence twice knows it is a standing fact about the estate rather
// than a finding about the window it asked about.
const TraceSourceAbsent = "the trace data source is absent in this organisation: no Cloud Trace, " +
	"no third-party tracing, and nothing was searched. This is not evidence that there were no " +
	"error spans — it is that error spans cannot be checked here"

// errorSpans answers the term.
func (b *Backend) errorSpans(term *investigationv1.ErrorSpansTerm) (answer, error) {
	window := term.GetWindow()
	coverage, err := b.coverage(coverageInput{
		Entities:   []string{term.GetSrcEntityId(), term.GetDstEntityId()},
		DataSource: "cloud_trace:absent",
		Window:     window,
		Volume:     0,
		Sampling:   "none",
		Criteria:   []string{criterion(CriterionAbsentSource, "cloud_trace")},
		// Not "a lag of zero": there is no pipeline to have a lag. Stating a figure here would
		// let the ingestion cutoff turn this into a NOT_YET_INGESTED that would one day become
		// a NO_DATA — implying the traces arrived and held nothing.
		LagSource:     LagUndetermined,
		NothingSearch: true,
	})
	if err != nil {
		return answer{}, err
	}
	return answer{
		outcome: engine.NoData{Coverage: coverage, AbsentSource: TraceSourceAbsent},
		query: fmt.Sprintf("error_spans(%s -> %s): not executed; no trace data source",
			term.GetSrcEntityId(), term.GetDstEntityId()),
		// The vocabulary a trace pointer WOULD be written in, named so that a reader can see
		// what this backend would execute if the estate ever had a trace source.
		vocabulary: "gcp-trace-filter/v1",
	}, nil
}
