// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"fmt"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The metrics worker (tasks.md T043, T044, T045; FR-030, FR-009a, FR-042b, FR-014b).
//
// Three capabilities, no model:
//
//   - `compare(pointer, window pair, statistic)` — baseline against symptom over one selector,
//     with the direction and magnitude of the difference and **whether it separates the
//     hypothesis** (FR-030). Separability is the field that stops a comparison from being
//     mistaken for a verdict: two windows that do not differ have not exonerated anything, they
//     have failed to discriminate, and those are different sentences.
//   - `errors_by_version(pointer, window, version attribute)` — the error rate split by the
//     deployed version tag named by `Pointer.join_keys["version"]` (ADR-0005 D4). This is the
//     single most decisive telemetry question in a rollout regression, and it is decisive only
//     because 001's pointers now say which tag carries the version.
//   - `onset(pointer, search window, method)` — served backend-side, because the series may not
//     cross the digest boundary (constitution IV). The worker passes it through; the arithmetic
//     is pkg/backend/onset and runs where the samples are.
//
// `monitor_state` is declared here too, because the intake needs the transition history and a
// recovery is evidence (ADR-0003 D10), and because a monitor's state is a metric-shaped fact
// about the same pointer.
//
// There is no model anywhere in this worker and `ContainsModel` is false. A comparison is
// arithmetic; anything a model added would be narrative, and narrative that is not derived from
// the arithmetic is not evidence (FR-009a).

// Name is the worker's published name.
const Name = "metrics"

// Version is the worker's own version, recorded with every answer.
const Version = "0.1.0"

// Worker is the metrics worker over exactly one telemetry backend.
type Worker struct {
	base workers.Base
}

var _ worker.Worker = (*Worker)(nil)

// New returns the metrics worker over one backend.
func New(backend workers.Backend) *Worker {
	w := &Worker{}
	w.base = workers.Base{
		Declaration: worker.Description{
			Name:          Name,
			SourceOfTruth: workers.SourceOfTruthOf(backend),
			Capabilities: []worker.Capability{
				workers.Capability(engine.TermCompare),
				workers.Capability(engine.TermOnset),
				workers.Capability(engine.TermErrorsByVersion),
				workers.Capability(engine.TermMonitorState),
			},
			ContainsModel: false,
			Redaction:     workers.Redaction(),
			Modes:         workers.BothModes(),
			Version:       Version,
		},
		Backend: backend,
		After:   annotate,
	}
	return w
}

// Describe returns the declaration.
func (w *Worker) Describe() worker.Description { return w.base.Describe() }

// Call answers one term.
func (w *Worker) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	return w.base.Call(ctx, req)
}

// annotate adds what the worker knows and the backend does not: whether a comparison separates
// the hypothesis it was asked to settle, and a bounded, unverified note saying so in words.
//
// The backend computes the numbers; deciding whether those numbers discriminate is the worker's
// job, because separability is a statement about the *question*, and the question is the
// worker's contract, not the vendor's.
func annotate(_ context.Context, req worker.Request, resp *engine.Response) error {
	metric := resp.GetDigest().GetMetric()
	if metric == nil {
		return nil
	}
	pair := req.Algebra.GetTerm().GetCompare().GetWindows()
	for _, comparison := range metric.GetComparisons() {
		comparison.Separable = separates(comparison, pair)
	}
	return nil
}

// SeparationThreshold is the published relative difference below which a comparison is reported
// as not separating the hypothesis. It is deliberately generous: reporting a real difference as
// non-separable costs one more query, and reporting noise as separable exonerates or convicts a
// change on nothing.
const SeparationThreshold = 0.20

// Separates is the rule itself, over the one number it reads: a relative difference of this
// magnitude discriminates, and anything smaller does not.
//
// It is exported because it must be applied in exactly one place by everyone who applies it. A
// backend that generates comparisons — pkg/backend/synthetic is the one in this feature — states
// `separable` on the comparisons it produces, and the worker re-states it on every answer it
// passes on. Two spellings of the same rule is one spelling too many: where they disagree, the
// worker's annotation changes a digest that was already hashed, and the recording that results
// carries a `response_digest` its own content does not hash to.
func Separates(relativeDelta float64) bool {
	if relativeDelta < 0 {
		relativeDelta = -relativeDelta
	}
	return relativeDelta >= SeparationThreshold
}

func separates(comparison *investigationv1.Comparison, pair *investigationv1.WindowPair) bool {
	if comparison.GetBaseline() == 0 && comparison.GetSymptom() == 0 {
		return false
	}
	if pair.GetBaseline().GetStart() == nil || pair.GetSymptom().GetStart() == nil {
		// Without two windows there is nothing to separate. A single-window answer is a
		// measurement, not a comparison.
		return false
	}
	return Separates(comparison.GetRelativeDelta())
}

// CompareTerm builds a `compare` request for a pointer, a window pair and a statistic, carrying
// the hypothesis it serves and the question it is meant to settle. Every call carries its
// purpose, and both are recorded with it (FR-018a).
func CompareTerm(pointer *graphv1.Pointer, reference time.Time, width time.Duration,
	statistic investigationv1.Statistic, hypothesisID, question string,
) *engine.Request {
	return &engine.Request{
		Term:                   engine.Compare(pointer, engine.NewWindowPair(reference, width), statistic),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
}

// OnsetTerm builds an `onset` request over a search window.
func OnsetTerm(pointer *graphv1.Pointer, search *engine.Window, hypothesisID, question string) *engine.Request {
	return &engine.Request{
		Term:                   engine.Onset(pointer, search, investigationv1.OnsetMethod_SEASONAL_CUSUM),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
}

// ErrorsByVersionTerm builds an `errors_by_version` request, reading the version attribute from
// the pointer's own join keys (ADR-0005 D4). A pointer that does not declare one is refused
// rather than guessed at: splitting a metric by the wrong tag produces a confident wrong answer,
// which is worse than no answer.
func ErrorsByVersionTerm(pointer *graphv1.Pointer, window *engine.Window, hypothesisID, question string) (*engine.Request, error) {
	attribute := pointer.GetJoinKeys()[JoinRoleVersion]
	if attribute == "" {
		return nil, engine.Reject(engine.ReasonOutsideAlgebra,
			"pointer %q declares no %q join key, so there is no attribute to split the error rate by; ADR-0005 D6 publishes the role set and docs/schema/pointers.md the vocabulary",
			pointer.GetSelector(), JoinRoleVersion)
	}
	return &engine.Request{
		Term:                   engine.ErrorsByVersion(pointer, window, attribute),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}, nil
}

// MonitorStateTerm builds a `monitor_state` request.
func MonitorStateTerm(pointer *graphv1.Pointer, window *engine.Window, hypothesisID, question string) *engine.Request {
	return &engine.Request{
		Term:                   engine.MonitorState(pointer, window),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
}

// The published join-key roles, from ADR-0005 D6 and docs/schema/pointers.md. They are named
// here because this worker is the one that reads them.
const (
	// JoinRoleVersion names the attribute carrying the deployed version.
	JoinRoleVersion = "version"
	// JoinRoleWorkload names the attribute carrying the workload.
	JoinRoleWorkload = "workload"
	// JoinRolePod names the attribute carrying the pod.
	JoinRolePod = "pod"
	// JoinRoleHost names the attribute carrying the host.
	JoinRoleHost = "host"
	// JoinRoleTrace names the attribute carrying the trace id.
	JoinRoleTrace = "trace"
)

// Describe the verdict of one comparison in the words an evidence item uses. It is a rendering
// helper rather than a claim: the numbers are in the digest and this is how they read.
func Describe(comparison *investigationv1.Comparison) string {
	if comparison == nil {
		return "no comparison"
	}
	if !comparison.GetSeparable() {
		return fmt.Sprintf(
			"%s moved %.1f%% between the windows, which is below the %.0f%% separation threshold: this comparison does not discriminate, and failing to discriminate is not evidence of innocence",
			comparison.GetStatistic(), comparison.GetRelativeDelta()*100, SeparationThreshold*100)
	}
	return fmt.Sprintf("%s went %s %.1f%% (%.6f → %.6f)",
		comparison.GetStatistic(), comparison.GetDirection(), comparison.GetRelativeDelta()*100,
		comparison.GetBaseline(), comparison.GetSymptom())
}
