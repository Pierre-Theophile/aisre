// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"context"
	"fmt"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The logs worker (tasks.md T048; FR-009a, FR-014).
//
// Algorithmic first, model second, and the digest says what the model added.
//
// The miner in drain.go runs on the backend side of the digest boundary and produces masked
// templates with counts. Only then may an optional `claude-haiku-4-5` pass label those
// templates — **templates and counts only, never a raw line** — and whatever it contributes is
// written into `LogDigest.model_added` so that a reader can subtract it. That ordering is
// FR-009a and it is not negotiable: a model that saw the lines first would be doing the
// reduction, and the reduction is the evidence.
//
// Exemplars are a separate algebra term behind a minted handle, returned **only on explicit
// request**, capped, redacted and counted against the response size bound (FR-014, FR-044a).
// "On explicit request" has to be a term or it is a side channel.

// Name is the worker's published name.
const Name = "logs"

// Version is the worker's own version, recorded with every answer.
const Version = "0.1.0"

// LabellingModelID is the model the optional labelling pass uses when one is configured. It is
// declared, never discovered: a worker whose answer a model touched says so (FR-009a).
const LabellingModelID = "claude-haiku-4-5"

// Labeller is the optional pass over mined templates. It sees templates and counts and nothing
// else — the signature is the enforcement, because there is no parameter through which a raw
// line could reach it.
//
// It returns a label per template, by template text, and a sentence saying what it added. A
// labeller that returns nothing is a labeller that added nothing, and the digest says so.
type Labeller interface {
	// ModelID is the model that runs, recorded with every call.
	ModelID() string
	// Label maps template text to a short label. It may return fewer labels than templates.
	Label(ctx context.Context, templates []LabelInput) (map[string]string, string, error)
}

// LabelInput is one template as the labeller sees it: masked text, counts, and whether it is new
// in the window. No instants, no identifiers, no line.
type LabelInput struct {
	// Template is the masked template.
	Template string
	// Count is how many lines matched it in the window.
	Count int64
	// BaselineCount is how many matched it in the baseline window.
	BaselineCount int64
	// NewInWindow says it does not appear in the baseline window at all.
	NewInWindow bool
}

// Worker is the logs worker over exactly one telemetry backend, with an optional labeller.
type Worker struct {
	base      workers.Base
	labeller  Labeller
	modelUsed bool
}

var _ worker.Worker = (*Worker)(nil)

// New returns the logs worker with the labelling pass off. This is the default: the algorithm
// already produces the evidence, and the model's addition must be measurable before it is paid
// for (research §5).
func New(backend workers.Backend) *Worker { return NewWithLabeller(backend, nil) }

// NewWithLabeller returns the logs worker with an optional labelling pass. Passing a non-nil
// labeller flips ContainsModel in the declaration, which is what makes the model declared rather
// than discovered.
func NewWithLabeller(backend workers.Backend, labeller Labeller) *Worker {
	w := &Worker{labeller: labeller, modelUsed: labeller != nil}
	modelID := ""
	if labeller != nil {
		modelID = labeller.ModelID()
	}
	w.base = workers.Base{
		Declaration: worker.Description{
			Name:          Name,
			SourceOfTruth: workers.SourceOfTruthOf(backend),
			Capabilities: []worker.Capability{
				workers.Capability(engine.TermNewLogPatterns),
				workers.Capability(engine.TermExemplars),
				workers.Capability(engine.TermDrillDown),
			},
			ContainsModel: labeller != nil,
			ModelID:       modelID,
			Redaction:     workers.Redaction(),
			Modes:         workers.BothModes(),
			Version:       Version,
		},
		Backend: backend,
		After:   w.after,
	}
	return w
}

// Describe returns the declaration.
func (w *Worker) Describe() worker.Description { return w.base.Describe() }

// Call answers one term.
func (w *Worker) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	return w.base.Call(ctx, req)
}

// after orders the mined templates and, where a labeller is configured, runs it over the
// templates and records what it added.
func (w *Worker) after(ctx context.Context, req worker.Request, resp *engine.Response) error {
	if digest := resp.GetDigest().GetLog(); digest != nil {
		// The ordering — new templates first, then the most frequent — is imposed on every
		// digest as it is built (internal/investigation/backend/digest.go), before the
		// cardinality cap drops from the tail. Re-ordering here would change the bytes of an
		// answer relative to the recording it came from and make a byte-for-byte golden
		// impossible.
		return w.label(ctx, digest)
	}
	if digest := resp.GetDigest().GetExemplars(); digest != nil {
		// Exemplars are returned only when the caller asked for them explicitly. A backend
		// that returned them unasked has opened a side channel, and the worker closes it here
		// rather than trusting every backend to have read the contract.
		if !req.Algebra.GetWantExemplars() && req.Capability != engine.TermExemplars {
			digest.Exemplars = nil
			return engine.Reject(engine.ReasonUndeclaredRedaction,
				"backend returned exemplars for a %s call that did not ask for them; exemplars are returned only on explicit request (FR-014)",
				req.Capability)
		}
	}
	return nil
}

// label runs the optional pass and writes its contribution into the digest. The labels go into
// the free-text-free part of the digest — the status field of the pattern they describe — and
// `model_added` says in one sentence what the model contributed over the algorithm, so that a
// reader can tell the two apart and a grader can subtract one from the other.
func (w *Worker) label(ctx context.Context, digest *investigationv1.LogDigest) error {
	digest.ModelUsed = false
	if w.labeller == nil {
		return nil
	}
	inputs := make([]LabelInput, 0, len(digest.GetPatterns()))
	for _, pattern := range digest.GetPatterns() {
		inputs = append(inputs, LabelInput{
			Template:      pattern.GetTemplate(),
			Count:         pattern.GetCount(),
			BaselineCount: pattern.GetBaselineCount(),
			NewInWindow:   pattern.GetNewInWindow(),
		})
	}
	labels, added, err := w.labeller.Label(ctx, inputs)
	if err != nil {
		// A labelling failure is not a log-mining failure. The templates stand; the digest
		// says the model did not run and why, and the investigation continues.
		digest.ModelAdded = "labelling pass failed and contributed nothing: " + err.Error()
		return nil
	}
	if len(labels) == 0 {
		digest.ModelAdded = "labelling pass ran and contributed nothing"
		digest.ModelUsed = true
		return nil
	}
	labelled := 0
	for _, pattern := range digest.GetPatterns() {
		if label, ok := labels[pattern.GetTemplate()]; ok && label != "" {
			pattern.Status = strings.TrimSpace(pattern.GetStatus() + " " + sanitiseLabel(label))
			labelled++
		}
	}
	digest.ModelUsed = true
	digest.ModelAdded = fmt.Sprintf("%s labelled %d of %d templates; every count, template and join key above is the miner's (%s). %s",
		w.labeller.ModelID(), labelled, len(digest.GetPatterns()), MinerVersion, strings.TrimSpace(added))
	return nil
}

// sanitiseLabel bounds and masks what the model wrote. The labeller's output is content, and
// content is data: it may not carry an instruction, an identifier or a payload back into the
// digest (FR-017).
func sanitiseLabel(label string) string {
	const maxLabel = 48
	label = engine.MaskLine(label)
	if len(label) > maxLabel {
		label = label[:maxLabel]
	}
	return "[" + strings.TrimSpace(label) + "]"
}

// NewLogPatternsTerm builds a `new_log_patterns` request for a pointer, a window and a baseline
// window, carrying the hypothesis it serves and the question it is meant to settle.
func NewLogPatternsTerm(pointer *graphv1.Pointer, window, baseline *engine.Window,
	hypothesisID, question string,
) *engine.Request {
	return &engine.Request{
		Term:                   engine.NewLogPatterns(pointer, window, baseline),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
}

// ExemplarsTerm builds an `exemplars` request behind a handle a previous answer minted. There is
// no constructor taking a selector, deliberately: a caller that could compose one would have an
// unbounded argument space and a side channel around the digest.
func ExemplarsTerm(handle *engine.Handle, limit uint32, hypothesisID, question string) *engine.Request {
	return &engine.Request{
		Term:                   engine.Exemplars(handle, limit),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
		WantExemplars:          true,
	}
}

// DrillDownTerm builds a `drill_down` request behind a minted handle.
func DrillDownTerm(handle *engine.Handle, hypothesisID, question string) *engine.Request {
	return &engine.Request{
		Term:                   engine.DrillDown(handle),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
}

// Describe renders one pattern in the words an evidence item uses.
func Describe(pattern *investigationv1.LogPattern) string {
	if pattern == nil {
		return "no pattern"
	}
	if pattern.GetNewInWindow() {
		return fmt.Sprintf("NEW %q ×%d (absent from the baseline window)", pattern.GetTemplate(), pattern.GetCount())
	}
	return fmt.Sprintf("%q ×%d (baseline ×%d)", pattern.GetTemplate(), pattern.GetCount(), pattern.GetBaselineCount())
}
