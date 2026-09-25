// SPDX-License-Identifier: Apache-2.0

package backend

import "context"

// Recorder writes a world: one digest per algebra term, in the published layout, keyed by the
// canonicalised term and both instants (FR-036, FR-037).
//
//	world/
//	├── index.json       # algebra version, hop radius, drill-down depth, window grid,
//	│                    #   term_key → file, term count, not_recorded count, miss rate,
//	│                    #   redaction policy version
//	└── <term_key>.json  # one digest per term
//
// A world is not a trajectory. It covers the cross product of the algebra over the alert
// neighbourhood and the window grid, plus the depth-1 drill-downs those answers mint, so a
// consumer that asks a different but in-algebra question is still served. A term the world does
// not hold is answered NOT_RECORDED — never an approximation, and never a fall-through to a
// live call.
type Recorder interface {
	// Record writes one answer into the world, keyed by the request's canonicalised term.
	// Recording the same term twice with different answers is an error: a world is a map, and
	// a key with two values is a world that cannot be replayed deterministically.
	Record(ctx context.Context, req *AlgebraRequest, resp *AlgebraResponse) error
	// Close writes world/index.json and returns the index digest, which is checked on load.
	Close(ctx context.Context) (string, error)
}

// LiveCoverageRecorder is a Recorder that can be told, once the passes that established it have
// run, which of its answers a live investigator asked for.
//
// It is a second interface rather than a third method on Recorder because the coverage is not
// known when the recorder is built — a live pass is what discovers it — and because a Recorder
// that never sees a live pass must keep writing exactly the bytes it wrote before.
type LiveCoverageRecorder interface {
	Recorder
	// DeclareLiveCoverage names the live provenance to write into the index. The keys it names
	// must all have been recorded, or Close refuses: a world that declares coverage it does not
	// hold would fail on the next load rather than here.
	DeclareLiveCoverage(coverage *LiveCoverage)
}
