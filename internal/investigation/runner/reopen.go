// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
)

// Reopening (FR-057b, FR-007, SC-024).
//
// A concluded investigation is immutable. A fact that bears on one therefore does not edit it: it
// moves the parent to `reopened` — the single transition a concluded row takes — and opens a
// **new linked row** that runs under the same rules and concludes on its own. The parent's
// verdict, ledger and evidence stay exactly as produced, which is what "the concluded record MUST
// remain readable exactly as produced" means and what makes a reopen safe to run against a record
// somebody has already read.
//
// The child inherits the parent's *question*, not its answer: the same incident, the same subject
// references, the same two instants, the same window. Re-pinning to "now" would make the reopen a
// different investigation wearing the parent's name — it would see everything learned since,
// including the things that were learned *because* of the parent's conclusion.
//
// The fact itself lands on the child, as evidence weighted `strong` and never decisive. Where the
// telemetry contradicts it, both are kept and the contradiction is reported: the engine does not
// adjudicate between a person and a measurement (FR-057a, research §8).

// Reopen runs a new linked investigation after a fact bore on a concluded one (FR-057b).
func (r *Runner) Reopen(
	ctx context.Context,
	parentID string,
	fact *investigationv1.HumanFact,
	requester string,
) (*investigationv1.Investigation, error) {
	if parentID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("reopen: no investigation id"))
	}
	if fact == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("reopen: a reopen carries the fact that bears on the concluded investigation (FR-057b)"))
	}
	if requester == "" {
		return nil, investigationstore.ErrAnonymousPrincipal
	}
	if fact.GetAuthor() == "" {
		fact.Author = requester
	}
	if fact.GetSubmittedAt() == nil {
		fact.SubmittedAt = timestamppb.New(r.now())
	}

	parent, err := r.dao.Get(ctx, parentID)
	if err != nil {
		return nil, err
	}
	rn, err := r.childOf(parent, fact, requester)
	if err != nil {
		return nil, err
	}

	child := r.newInvestigation(rn, parentID)
	stored, err := r.dao.ReopenWithFact(ctx, parentID, child, fact)
	if err != nil {
		return nil, err
	}
	rn.fact = stored
	return r.execute(ctx, rn, nil)
}

// childOf builds the child run from the parent's own row: the same incident, the same subject,
// the same instants and the same window.
func (r *Runner) childOf(
	parent *investigationv1.Investigation,
	fact *investigationv1.HumanFact,
	requester string,
) (*run, error) {
	symptom := openingSymptomOf(parent)
	if symptom == nil {
		return nil, fmt.Errorf("reopen %s: the parent carries no symptom to inherit a subject from",
			parent.GetInvestigationId())
	}
	in := &intake.Intake{
		Symptom:    symptom,
		ValidAt:    parent.GetValidAt().AsTime().UTC(),
		ObservedAt: parent.GetObservedAt().AsTime().UTC(),
		ReviewMode: parent.GetReviewMode(),
		Window:     parent.GetWindow(),
		Lookback: parent.GetWindow().GetEnd().AsTime().
			Sub(parent.GetWindow().GetStart().AsTime()),
		UnresolvedTargets: len(symptom.GetNamedIdentifiers()) == 0,
	}

	// The parent already recorded what its identifiers resolved to, with the audit that justifies
	// it. Re-running the audit here would ask the graph a question at the same pinned instant and
	// get the same answer, at the cost of a second evidence item claiming to be a fresh reading.
	set := resolutionFromParent(symptom)
	profile := r.profileFor(parent.GetSpend().GetLimits().GetName(), symptom.GetSeverity())

	return &run{
		investigationID: childIDFor(parent.GetInvestigationId(), fact),
		incidentID:      parent.GetIncidentId(),
		intake:          in,
		resolution:      set,
		profile:         profile,
		requester:       requester,
		subjectRef:      subjectRefOf(set),
		subjects:        subjectsOf(in, set),
		startedAt:       r.now(),
	}, nil
}

// openingSymptomOf is the symptom that opened the incident: the earliest one that did not attach
// to an investigation already running.
func openingSymptomOf(parent *investigationv1.Investigation) *investigationv1.Symptom {
	var opener *investigationv1.Symptom
	for _, s := range parent.GetSymptoms() {
		if s.GetAttachedAsAdditional() {
			continue
		}
		if opener == nil || s.GetFiredAt().AsTime().Before(opener.GetFiredAt().AsTime()) {
			opener = s
		}
	}
	if opener == nil && len(parent.GetSymptoms()) > 0 {
		opener = parent.GetSymptoms()[0]
	}
	return opener
}

// resolutionFromParent rebuilds the resolution set from what the parent's symptom row recorded.
//
// It carries no evidence items: the parent's audits are the parent's, and the child's ledger
// records only what the child established. What the child needs from them is the *subject*, which
// is the reference the alert named — that is on the symptom row.
func resolutionFromParent(symptom *investigationv1.Symptom) intake.ResolutionSet {
	set := intake.ResolutionSet{EntityIDs: symptom.GetResolvedEntityIds()}
	resolved := len(symptom.GetResolvedEntityIds()) > 0
	for _, named := range symptom.GetNamedIdentifiers() {
		ref := graph.RefFromProto(named)
		if ref.IsZero() {
			continue
		}
		res := intake.Resolution{Given: ref, Resolved: resolved}
		if resolved {
			res.EntityID = symptom.GetResolvedEntityIds()[0]
		}
		set.Resolutions = append(set.Resolutions, res)
		if !resolved {
			set.Unresolved = append(set.Unresolved, ref)
		}
	}
	return set
}

// childIDFor derives the child's identifier from the parent and the fact that reopened it, so
// that re-delivering the same fact reopens once rather than opening a chain of children.
func childIDFor(parentID string, fact *investigationv1.HumanFact) string {
	seed := parentID + "|" + fact.GetFactId()
	if fact.GetFactId() == "" {
		seed = parentID + "|" + fact.GetAuthor() + "|" +
			fact.GetSubmittedAt().AsTime().UTC().Format(time.RFC3339Nano) + "|" + fact.GetStatement()
	}
	sum := sha256.Sum256([]byte(seed))
	return "inv-" + hex.EncodeToString(sum[:])[:16]
}
