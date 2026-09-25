// SPDX-License-Identifier: Apache-2.0

// Package workers holds what the five workers of this feature share: the mechanics of being a
// worker bound to exactly one telemetry backend.
//
// Three of the five — metrics, traces and logs — are the same shape: they declare a set of
// algebra terms, they hold exactly one backend, and answering a call is "check the capability was
// declared, hand the term to the backend, check the answer carries its coverage block". Writing
// that three times would be three places for the coverage check to be forgotten, so it is written
// once here and the three workers differ only in what they declare and in what they do to the
// digest afterwards.
//
// The graph and knowledge workers do not use it: the graph worker's source of truth is feature
// 001's QueryService and the knowledge worker's is the graph plus the documents linked to it.
// Neither has a telemetry backend, which is precisely what the worker/backend split is for
// (plan §Project Structure, "Terminology").
package workers

import (
	"context"
	"fmt"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// Backend is the one telemetry backend a worker holds. It is the published interface, so a
// worker cannot tell a recorded backend from a live one — which is what makes "live and recorded
// produce identical digests" a property of the worker rather than a promise about it.
type Backend = sdk.TelemetryBackend

// Base is a worker over exactly one telemetry backend.
type Base struct {
	// Declaration is what the worker registers itself with.
	Declaration worker.Description
	// Backend is the one source of truth.
	Backend Backend
	// After is an optional hook the worker uses to post-process a digest — the logs worker's
	// model labelling pass is the only one in this feature. It runs after the backend answered
	// and before the coverage check, and whatever it adds it must state in the digest.
	After func(ctx context.Context, req worker.Request, resp *engine.Response) error
}

// Describe returns the declaration.
func (b *Base) Describe() worker.Description { return b.Declaration }

// Call answers one algebra term through the backend.
//
// Four things happen here and nowhere else, so that they happen for every telemetry worker:
// the capability is checked against the declaration, the term is checked against the algebra,
// the mode is stamped on the answer, and the answer is refused if it carries no coverage block.
func (b *Base) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	if b.Backend == nil {
		return worker.Response{}, fmt.Errorf(
			"worker %s has no telemetry backend; a worker bound to no source of truth can answer nothing",
			b.Declaration.Name)
	}
	if !b.Declaration.Declares(req.Capability) {
		declared := make([]string, 0, len(b.Declaration.Capabilities))
		for _, c := range b.Declaration.Capabilities {
			declared = append(declared, c.Name)
		}
		return worker.Response{}, worker.Reject(worker.ReasonUndeclaredCapability,
			"worker %s did not declare capability %s; it declares %v",
			b.Declaration.Name, req.Capability, declared)
	}
	term := req.Algebra.GetTerm()
	name := engine.TermName(term)
	if name == "" {
		return worker.Response{}, engine.OutsideAlgebra("")
	}
	if name != req.Capability {
		return worker.Response{}, worker.Reject(worker.ReasonOutsideAlgebra,
			"worker %s was called for capability %s with a %s term; the capability and the term are the same thing and must agree",
			b.Declaration.Name, req.Capability, name)
	}
	if err := engine.Validate(term); err != nil {
		return worker.Response{}, err
	}

	resp, err := b.Backend.Execute(ctx, req.Algebra)
	if err != nil {
		return worker.Response{}, err
	}
	if b.After != nil {
		if err := b.After(ctx, req, resp); err != nil {
			return worker.Response{}, err
		}
	}
	resp.Mode = req.Mode.String()
	if err := engine.ValidateCoverage(resp); err != nil {
		return worker.Response{}, err
	}
	return worker.Response{
		Worker:     b.Declaration.Name,
		Capability: req.Capability,
		Mode:       req.Mode,
		Algebra:    resp,
	}, nil
}

// Capability is the shorthand for declaring one read-only term at its published cost class.
func Capability(term string) worker.Capability {
	return worker.Capability{
		Name:      term,
		ReadOnly:  true,
		CostClass: engine.PublishedCostClass(term),
	}
}

// BothModes is the mode set every worker declares. A worker that cannot be replayed cannot be
// merged (constitution VIII), so there is no single-mode spelling of this.
func BothModes() []worker.Mode { return []worker.Mode{worker.ModeLive, worker.ModeRecorded} }

// Redaction is the published policy this feature's workers declare, converted to the worker
// SDK's spelling of the same statement.
func Redaction() worker.RedactionPolicy {
	policy := engine.DefaultRedactionPolicy()
	return worker.RedactionPolicy{
		DroppedFields:        policy.GetDroppedFields(),
		PseudonymisedFields:  policy.GetPseudonymisedFields(),
		LogBodiesAsTemplates: policy.GetLogBodiesAsTemplates(),
		PolicyVersion:        policy.GetPolicyVersion(),
	}
}

// SourceOfTruthOf names the one backend a telemetry worker is bound to, for the declaration's
// source_of_truth field. A worker bound to more than one is rejected at registration, so this is
// always exactly one name.
func SourceOfTruthOf(b Backend) string {
	if b == nil {
		return ""
	}
	d := b.Describe()
	return d.Vendor + ":" + d.Name
}

// EmptyDigest is the answer a worker gives when it has nothing of its own to add and the backend
// returned nothing — used only by tests and by the graph worker's unsupported branches. It is
// never NO_DATA: an empty structure this worker built is not evidence that the window was
// searched (FR-027).
func EmptyDigest(req worker.Request, detail string) (*engine.Response, error) {
	return engine.NewResponse(engine.ResponseInput{
		Request: req.Algebra,
		Outcome: engine.QueryFailed{
			Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
			Detail: detail,
		},
		Mode: req.Mode.String(),
	})
}
