// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Causal ordering (T081, FR-029, FR-029a–d, SC-009, SC-019).
//
// The question this answers is the one that separates a useful investigation from a plausible one:
// *did this change happen before the symptom started, or after it?* A change that started after
// onset by more than the onset's own uncertainty is a candidate **effect** — the autoscaler
// reacting to the incident, the retry storm, the failover — and presenting it as a suspect is how
// an investigation talks an on-call into rolling back something that was helping.
//
// Five rules, and each one is a sentence of the requirement:
//
//  1. **Pointers come from the graph, valid at the investigation instant.** The engine never
//     constructs a selector of its own for an entity the graph can describe. The tool schemas make
//     that structural: they take a pointer id, and the ids exist only because a `pointers` answer
//     minted them.
//  2. **Onset comes from the metrics worker**, as an algebra term evaluated backend-side, because
//     the series may not cross the digest boundary (constitution IV). It is never a synthetic
//     reference instant the engine computed for itself.
//  3. **Candidates are re-obtained with `reference_at` set to the estimated onset, and never
//     re-ranked.** The ranker's order is the graph's published contract. Disagreement with it is
//     expressed as a hypothesis with evidence, not as a quiet reordering.
//  4. **A candidate later than onset by more than the uncertainty is typed a candidate effect and
//     exonerated**, with the onset estimate cited, unless independent evidence supports it as a
//     cause. The exoneration is a first-class judgment — `decisive`, `refutes`, sourced
//     `exoneration` — and appears in the rendering as prominently as a support (FR-029c).
//  5. **Where onset cannot be estimated, the engine says so, falls back to the alert instant, and
//     exonerates nobody on timing alone.** This is the rule that keeps the whole mechanism honest:
//     the alert instant is when someone noticed, not when it started, and exonerating against it
//     would rule out the causes that take a few minutes to become visible.

// OnsetCandidates is how many ranked candidates the engine estimates onset on, beside the subject
// (Phase 8 K2, FR-029).
//
// Three: the same top of the list the first wave tests, so the two steps agree about which
// candidates are worth spending a cheap call on, and so the extra calls stay inside the `page`
// profile's own cheap-class budget.
const OnsetCandidates = 3

// Onset is the estimated symptom onset, with everything a reader needs to judge it.
type Onset struct {
	// At is the estimate. Zero when the worker could not produce one.
	At time.Time
	// Uncertainty is the estimate's own uncertainty, which is the tolerance an exoneration must
	// clear before it is made.
	Uncertainty time.Duration
	// Method and Parameters are the published method and its parameters.
	Method     string
	Parameters map[string]float64
	// EvidenceID is the `onset_estimate` evidence item an exoneration rests on (Invariant 9).
	EvidenceID string
	// Available says whether an estimate was produced; Reason says why not when it was not.
	Available bool
	Reason    string
	// ReferenceUsed is the instant the ranking was actually taken against: the onset when there
	// is one, the alert instant when there is not.
	ReferenceUsed time.Time
	// FellBackToAlert records that the alert instant was used, so the rendering can say so.
	FellBackToAlert bool
	// EntityRef and PointerID name where the estimate came from: the subject, or a candidate
	// cause's own target. An onset is only interpretable against the series it was measured on,
	// and an investigation that referenced everything to the *subject's* onset could never
	// satisfy a decisive predicate written about the cause's (Phase 8 K2).
	EntityRef string
	PointerID string
	// Considered is every estimate the engine obtained, in the order it obtained them, including
	// the ones it did not choose. It is what makes "the earliest confident onset" checkable
	// rather than asserted.
	Considered []OnsetEstimate
}

// OnsetEstimate is one estimate on one series.
type OnsetEstimate struct {
	// EntityRef is the entity whose series it was measured on and PointerID the pointer used.
	EntityRef string
	PointerID string
	// At, Uncertainty, Method and Parameters are the digest's.
	At          time.Time
	Uncertainty time.Duration
	Method      string
	Parameters  map[string]float64
	// EvidenceID is the `onset_estimate` evidence item in the ledger.
	EvidenceID string
	// Available says whether the worker produced an estimate; Reason says why not.
	Available bool
	Reason    string
}

// Line renders one estimate for the onset sentence.
func (o OnsetEstimate) Line() string {
	if !o.Available {
		return fmt.Sprintf("%s: no estimate (%s)", o.EntityRef, o.Reason)
	}
	return fmt.Sprintf("%s: %s ± %s (%s, evidence %s)",
		o.EntityRef, o.At.Format(time.RFC3339), o.Uncertainty, o.Method, o.EvidenceID)
}

// Line is the sentence the rendering carries about onset. It is required reading for anything that
// depends on the ordering, so it says both the estimate and its uncertainty, or says plainly that
// there is none.
func (o *Onset) Line() string {
	if o == nil {
		return "Symptom onset: not estimated."
	}
	if !o.Available {
		return fmt.Sprintf(
			"Symptom onset: could not be estimated (%s). The alert instant %s is used as the ranking "+
				"reference instead, and nothing is exonerated on timing alone (FR-029b).",
			o.Reason, o.ReferenceUsed.Format(time.RFC3339))
	}
	return fmt.Sprintf(
		"Symptom onset: %s ± %s, estimated by %s from the metrics worker on %s (evidence %s). "+
			"Candidates are ranked against it.",
		o.At.Format(time.RFC3339), o.Uncertainty, o.Method, o.EntityRef, o.EvidenceID)
}

// Detail lists every series the engine estimated onset on, chosen or not. A report carries it
// beside Line, because "the earliest confident onset" is a claim about a set and a reader cannot
// check it against one number.
func (o *Onset) Detail() string {
	if o == nil || len(o.Considered) == 0 {
		return ""
	}
	lines := make([]string, 0, len(o.Considered))
	for _, each := range o.Considered {
		chosen := ""
		if each.Available && each.EntityRef == o.EntityRef && each.PointerID == o.PointerID {
			chosen = " ← used"
		}
		lines = append(lines, "  "+each.Line()+chosen)
	}
	return "onset estimates considered (earliest confident wins):\n" + strings.Join(lines, "\n")
}

// Onset returns the onset estimate, once there is one.
func (e *Engine) Onset() *Onset {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.onset
}

// EstimateOnset asks the metrics worker when the symptom started — on the subject, and on the
// targets of the top ranked candidate causes.
//
// It is the metrics worker rather than the engine because the estimate needs the series and the
// series may not cross the digest boundary. What comes back is an instant, an uncertainty, the
// method and its parameters — a digest like any other (research §4).
//
// **The subject is not the only series that knows when this started** (Phase 8 K2). A rollout on
// payments shows up on payments first and on its caller a little later, so an onset estimated only
// on the subject is systematically late — late enough to exonerate the cause on the clock, and late
// enough that a decisive predicate written about the *cause's* onset can never be satisfied,
// because the run never measured it. So the engine also estimates onset on the top
// `OnsetCandidates` candidates' targets, cheap calls made inside the budget like any other, and
// takes the **earliest confident** estimate as the reference. The estimate it used names the series
// it came from, and every estimate it considered stays in the ledger as evidence.
//
// The fallback chain is unchanged and still ends where it did: the subject's own onset, then the
// alert instant with the published "could not be estimated" line (FR-029b).
func (e *Engine) EstimateOnset(ctx context.Context) (*Onset, error) {
	onset := &Onset{ReferenceUsed: e.subject.FiredAt, FellBackToAlert: true, EntityRef: e.subject.EntityRef}

	subject, err := e.estimateOnsetOn(ctx, e.subject.EntityRef,
		"when did the symptom actually start, as distinct from when the monitor noticed?")
	if err != nil {
		return nil, err
	}
	onset.Considered = append(onset.Considered, subject)
	if subject.EvidenceID != "" {
		onset.EvidenceID = subject.EvidenceID
	}

	for _, ref := range e.onsetCandidateTargets() {
		estimate, err := e.estimateOnsetOn(ctx, ref,
			"when did the symptom start on "+ref+", the target of a candidate cause?")
		if err != nil {
			return nil, err
		}
		onset.Considered = append(onset.Considered, estimate)
	}

	chosen, ok := earliestConfident(onset.Considered)
	switch {
	case ok:
		onset.Available = true
		onset.FellBackToAlert = false
		onset.At = chosen.At
		onset.Uncertainty = chosen.Uncertainty
		onset.Method = chosen.Method
		onset.Parameters = chosen.Parameters
		onset.EvidenceID = chosen.EvidenceID
		onset.EntityRef = chosen.EntityRef
		onset.PointerID = chosen.PointerID
		onset.ReferenceUsed = chosen.At
	default:
		onset.Reason = subject.Reason
		if onset.Reason == "" {
			onset.Reason = "no series the investigation could reach produced an onset estimate"
		}
	}
	e.setOnset(onset)
	return onset, nil
}

// earliestConfident picks the estimate the ranking is referenced to: the earliest instant any
// series the investigation could reach puts the start of the symptom at.
//
// Earliest, because a symptom propagates outward and the first series to move is the one nearest
// the cause; an onset taken from the subject alone is the arrival time of the symptom, not its
// start, and every ordering decision made against it is late by the propagation delay.
//
// Two estimates at the same instant are separated by their own uncertainty — the tighter estimate
// is the more confident one, and it is usually the series nearest the cause, which is the series a
// decisive predicate about the cause is written over. A remaining tie goes to the order the
// estimates were obtained (the subject first, then the candidates in ranked order), so two runs
// over the same world choose the same one.
func earliestConfident(estimates []OnsetEstimate) (OnsetEstimate, bool) {
	var best OnsetEstimate
	var found bool
	for _, each := range estimates {
		if !each.Available || each.At.IsZero() {
			continue
		}
		switch {
		case !found, each.At.Before(best.At):
			best, found = each, true
		case each.At.Equal(best.At) && each.Uncertainty > 0 &&
			(best.Uncertainty == 0 || each.Uncertainty < best.Uncertainty):
			best = each
		}
	}
	return best, found
}

// onsetCandidateTargets are the entities the top ranked candidates changed, in ranked order,
// without the subject (already estimated) and without repeats.
func (e *Engine) onsetCandidateTargets() []string {
	seen := map[string]bool{e.subject.EntityRef: true}
	var out []string
	for _, h := range e.ledger.Hypotheses() {
		if len(out) >= OnsetCandidates {
			break
		}
		if h.Kind != ledger.KindChange || h.Status == ledger.StatusExonerated {
			continue
		}
		for _, id := range h.TargetEntityIDs {
			ref := e.catalogue.RefFor(id)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
			break
		}
	}
	return out
}

// estimateOnsetOn estimates onset on one entity's metric series, minting its pointers first where
// the investigation has not earned them yet.
//
// Every failure is a reason rather than an error: an entity with no metric pointer, a budget that
// refused the call and a worker that could not answer are three different things a reader may want
// to know, and none of them is a reason to abandon the investigation.
func (e *Engine) estimateOnsetOn(ctx context.Context, entityRef, question string) (OnsetEstimate, error) {
	estimate := OnsetEstimate{EntityRef: entityRef}

	if len(e.catalogue.PointersFor(entityRef)) == 0 {
		answer, err := e.pointersFor(ctx, entityRef)
		if err != nil {
			return estimate, err
		}
		if answer != nil && answer.Refused != nil {
			estimate.Reason = "the pointers read was not issued: " + answer.Refused.Reason
			return estimate, nil
		}
	}
	pointerID := e.pointerOfKind(entityRef, graphv1.PointerKind_METRIC)
	if pointerID == "" {
		estimate.Reason = entityRef + " has no metric pointer, so there is no series to estimate onset from"
		return estimate, nil
	}
	estimate.PointerID = pointerID

	// The search window is `OnsetSearchWindow`'s, which `fixture record-world` calls too: the
	// engine and the recorder derive the same interval from the same question, so this term keys
	// to the answer a recording holds rather than to one nobody recorded (plan.go).
	search := OnsetSearchWindow(e.subject.FiredAt, e.Lookback())
	req, err := DecodeCall("onset", jsonObject(map[string]any{
		"pointer_id":              pointerID,
		"search_start":            search.GetStart().AsTime().Format(time.RFC3339),
		"search_end":              search.GetEnd().AsTime().Format(time.RFC3339),
		"discriminating_question": question,
	}), e.catalogue, e.at)
	if err != nil {
		return estimate, err
	}
	answer, err := e.Call(ctx, req)
	if err != nil {
		return estimate, err
	}
	if answer.Refused != nil {
		estimate.Reason = "the onset query was not issued: " + answer.Refused.Reason
		return estimate, nil
	}
	estimate.EvidenceID = answer.EvidenceID

	digest := answer.Response.GetDigest().GetOnset()
	switch {
	case answer.Response.GetOutcome() != investigationv1.TermOutcome_DIGEST:
		estimate.Reason = fmt.Sprintf("the onset query on %s returned %s",
			entityRef, outcomeName(answer.Response.GetOutcome()))
	case digest == nil:
		estimate.Reason = "the onset answer carried no onset digest"
	case digest.GetUnavailable():
		estimate.Reason = digest.GetUnavailableReason()
	case digest.GetEstimatedOnset() == nil:
		estimate.Reason = "the onset digest carried no instant"
	default:
		estimate.Available = true
		estimate.At = digest.GetEstimatedOnset().AsTime().UTC()
		estimate.Uncertainty = time.Duration(digest.GetUncertaintySeconds()) * time.Second
		estimate.Method = digest.GetMethod().String()
		estimate.Parameters = digest.GetMethodParameters()
	}
	return estimate, nil
}

func (e *Engine) setOnset(onset *Onset) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onset = onset
}

// OrderCausally re-obtains the ranked candidates against the estimated onset and applies the
// causal typing: exonerate the candidates that start after it.
//
// It returns the hypothesis ids it exonerated, in ledger order.
func (e *Engine) OrderCausally(ctx context.Context) ([]string, error) {
	onset := e.Onset()
	if onset == nil {
		return nil, fmt.Errorf("engine: causal ordering ran before onset was estimated; " +
			"the reference instant is the onset, never one the engine invented (FR-029)")
	}

	question := "which changes near the subject started before the symptom did?"
	if !onset.Available {
		question = "which changes near the subject fall inside the window? (onset could not be estimated, " +
			"so the alert instant is the reference and nothing is exonerated on timing alone)"
	}
	answer, err := e.rankedChanges(ctx, onset.ReferenceUsed, question)
	if err != nil {
		return nil, err
	}
	if answer.Refused != nil {
		return nil, nil
	}

	diff, ok := answer.Graph.(*graphv1.DiffResponse)
	if !ok {
		return nil, nil
	}
	// A candidate the onset-referenced ranking surfaces and the alert-referenced one did not is
	// added rather than ignored: the reference instant moved, so the candidate set may have.
	if err := e.seed(diff); err != nil {
		return nil, err
	}
	// A platform-stated rollback away from a candidate is an operator's judgement about it, and it does
	// not depend on the onset: it is credited whether or not one could be estimated (004 T155).
	if _, err := e.CreditRollbacks(diff, answer.EvidenceID); err != nil {
		return nil, err
	}

	if !onset.Available {
		// FR-029b: exonerate nobody on timing alone. The ranking still moved to the alert instant
		// and the run says so; nothing is ruled out.
		return nil, nil
	}

	var exonerated []string
	for _, change := range diff.GetChanges() {
		hypothesisID := e.hypothesisFor(change.GetChange().GetEntityId())
		if hypothesisID == "" {
			continue
		}
		if !isCandidateEffect(change, onset.Uncertainty) {
			continue
		}
		if e.hasIndependentSupport(hypothesisID) {
			// "unless independent evidence supports it as a cause" — a change that started after
			// onset but that a source-of-truth worker has already tied to the symptom is not ruled
			// out by the clock.
			continue
		}
		reason := fmt.Sprintf(
			"it started %ds after the estimated onset %s, which is more than the estimate's own "+
				"uncertainty of %s; a change that starts after the symptom is a candidate effect of it, "+
				"not a cause (actor kind %s)",
			-change.GetSignedTimeDistanceSeconds(), onset.At.Format(time.RFC3339), onset.Uncertainty,
			change.GetActorKind())
		if _, err := e.ledger.Exonerate(hypothesisID, ledger.Exoneration{
			JudgmentID:      e.nextJudgmentID(),
			OnsetEvidenceID: onset.EvidenceID,
			Reason:          reason,
			Strength:        ledger.Decisive,
		}); err != nil {
			return nil, fmt.Errorf("engine: exonerate %s: %w", hypothesisID, err)
		}
		exonerated = append(exonerated, hypothesisID)
	}

	if len(exonerated) > 0 {
		digest, err := e.ledger.Digest()
		if err != nil {
			return nil, err
		}
		e.trajectory.LedgerUpdate(nil, digest)
	}
	return exonerated, nil
}

// isCandidateEffect is the published test: later than the reference by more than the onset's own
// uncertainty.
//
// The uncertainty is what makes it safe. A change three seconds after an onset estimated to ±120 s
// is not later than onset in any sense the data supports, and exonerating it would be arithmetic
// pretending to be evidence.
func isCandidateEffect(change *graphv1.RankedChange, uncertainty time.Duration) bool {
	if !change.GetPostReference() {
		return false
	}
	after := time.Duration(-change.GetSignedTimeDistanceSeconds()) * time.Second
	return after > uncertainty
}

// hasIndependentSupport reports whether a hypothesis already carries a supporting judgment from a
// source-of-truth worker — the "unless independent evidence supports it as a cause" clause.
func (e *Engine) hasIndependentSupport(hypothesisID string) bool {
	for _, j := range e.ledger.JudgmentsFor(hypothesisID) {
		if j.Direction != ledger.Supports || j.Source == ledger.SourceExoneration {
			continue
		}
		item, ok := e.ledger.Evidence(j.EvidenceID)
		if ok && item.SourceOfTruth != "" {
			return true
		}
	}
	return false
}
