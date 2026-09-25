// SPDX-License-Identifier: Apache-2.0

package traces

import (
	"context"
	"fmt"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The traces worker (tasks.md T047; FR-011, FR-009a, research §6).
//
// Algorithmic only, no model. What an investigator needs from traces is "which call on this path
// started failing, and by how much", which is arithmetic over groupings the backend can already
// do. Anything a model would add would be narrative, and narrative that is not derived from the
// grouping is not evidence.
//
// Two capabilities:
//
//   - `error_spans(edge, window)` — counts and latency statistics by operation and error kind
//     over the spans that traverse a **named graph edge**. Asking about an edge rather than a
//     service is the point: "payments is slow" is a symptom, "checkout's call to payments is
//     slow" is a location, and the graph is what makes the second askable.
//   - `compare` over the same groupings, for baseline against symptom.
//
// Trace identifiers survive as join keys and as drill-down handles. They are pseudonymised, not
// dropped, because a digest whose keys do not join is evidence about nothing: the whole value of
// a trace id here is that the same trace appears in the log digest and the span digest.

// Name is the worker's published name.
const Name = "traces"

// Version is the worker's own version, recorded with every answer.
const Version = "0.1.0"

// Worker is the traces worker over exactly one telemetry backend.
type Worker struct {
	base workers.Base
}

var _ worker.Worker = (*Worker)(nil)

// New returns the traces worker over one backend.
func New(backend workers.Backend) *Worker {
	w := &Worker{}
	w.base = workers.Base{
		Declaration: worker.Description{
			Name:          Name,
			SourceOfTruth: workers.SourceOfTruthOf(backend),
			Capabilities: []worker.Capability{
				workers.Capability(engine.TermErrorSpans),
				workers.Capability(engine.TermCompare),
			},
			ContainsModel: false,
			Redaction:     workers.Redaction(),
			Modes:         workers.BothModes(),
			Version:       Version,
		},
		Backend: backend,
	}
	return w
}

// Describe returns the declaration.
func (w *Worker) Describe() worker.Description { return w.base.Describe() }

// Call answers one term.
func (w *Worker) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	return w.base.Call(ctx, req)
}

// ErrorSpansTerm builds an `error_spans` request for one graph edge, carrying the hypothesis it
// serves and the question it is meant to settle.
func ErrorSpansTerm(srcEntityID, dstEntityID string, edgeType graphv1.EdgeType,
	window *engine.Window, hypothesisID, question string,
) *engine.Request {
	return &engine.Request{
		Term:                   engine.ErrorSpans(srcEntityID, dstEntityID, edgeType, window),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
}

// ErrorRate is the fraction of spans in a digest that carry an error kind, at six decimals. It
// is a rendering of what the digest already says, not a second computation of it.
func ErrorRate(digest *investigationv1.TraceDigest) float64 {
	var total, errored int64
	for _, group := range digest.GetGroups() {
		total += group.GetCount()
		if group.GetErrorKind() != "" {
			errored += group.GetCount()
		}
	}
	if total == 0 {
		return 0
	}
	return float64(int64(float64(errored)/float64(total)*1e6+0.5)) / 1e6
}

// Describe renders one span group in the words an evidence item uses.
func Describe(group *investigationv1.SpanGroup) string {
	if group == nil {
		return "no group"
	}
	kind := group.GetErrorKind()
	if kind == "" {
		kind = "no error"
	}
	return fmt.Sprintf("%s (%s): %d spans, p95 %.6f", group.GetOperation(), kind,
		group.GetCount(), group.GetLatency()["P95"])
}
