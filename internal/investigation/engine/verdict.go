// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The verdict rule (Phase 8 Track K-A, K1, FR-022, SC-023, constitution V).
//
// The ranker is a prior. It says which changes are near the subject in time and topology, which is
// a very good reason to look at them and no reason at all to believe one of them. An engine that
// promotes its top prior candidate to the answer is an engine that always has an answer, including
// on the incidents where the cause is not in its data — and an answer that is always available is
// an answer nobody can trust when it matters.
//
// So the rule has three arms, and the middle one is the one that was missing:
//
//	named       the hypothesis carries a supporting judgment from telemetry evidence or a human
//	            fact, and is therefore `supported` in the ledger's own status machine
//	unknown     nothing qualifies yet and the run can still test: the honest interim answer
//	unobserved  the run finished and nothing qualified — every candidate untested, refuted,
//	            exonerated or tested-and-not-separated. The symptom is still localised on the
//	            subject, because "we do not know what changed" and "we do not know what is broken"
//	            are different things and only the first one is true here.
//
// `unobserved` is a *finding*, not a shrug: it is what the culprit-deleted metamorphic variant
// asks for, and it is the answer on every incident whose cause no feeder in this deployment can
// see (FR-071b, SC-023). It is reached only from a stop that means the run finished — a run cut
// short by a budget, a dead worker or a refusal has not established that nothing explains the
// symptom, and says `unknown` instead.

// VerdictKind is one of the three published verdict kinds.
type VerdictKind string

// The three verdict kinds.
const (
	// VerdictNamed names a hypothesis the evidence supports.
	VerdictNamed VerdictKind = "named"
	// VerdictUnknown is the honest interim answer: nothing observed supports anything yet.
	VerdictUnknown VerdictKind = "unknown"
	// VerdictUnobserved is the terminal answer when the run finished and no observed change
	// explains the symptom.
	VerdictUnobserved VerdictKind = "unobserved"
)

// Verdict is the investigation's answer at one instant, derived from the ledger and never stated
// by a model.
type Verdict struct {
	// Kind is which of the three arms applies.
	Kind VerdictKind
	// HypothesisID is the hypothesis the verdict rests on: the named one, or the open hypothesis
	// for `unobserved`. It is empty for `unknown` only when the ledger holds nothing at all.
	HypothesisID string
	// Change is the candidate change as a person writes it, for a named verdict.
	Change string
	// Statement is the hypothesis's own claim.
	Statement string
	// Confidence and Bucket are the ledger's, reported as the ledger reports them — which for a
	// hypothesis nothing observed supports is never above `moderate`.
	Confidence float64
	Bucket     string
	// Symptom is the subject the symptom sits on. It is carried on every verdict, including
	// `unobserved`, because the localisation is the part of the answer that survives not knowing
	// the cause (SC-023).
	Symptom string
	// Reason is the one sentence that says why this is the answer.
	Reason string
}

// Line renders the verdict as a report or a stream carries it.
func (v Verdict) Line() string {
	switch v.Kind {
	case VerdictNamed:
		return fmt.Sprintf("Verdict: %s (%s, %.6f). %s", v.Change, v.Bucket, v.Confidence, v.Reason)
	case VerdictUnobserved:
		return fmt.Sprintf("Verdict: unobserved — no observed change explains the symptom on %s (%s). %s",
			v.Symptom, v.Bucket, v.Reason)
	default:
		return fmt.Sprintf("Verdict: unknown — the symptom is on %s and nothing observed supports a "+
			"cause yet. %s", v.Symptom, v.Reason)
	}
}

// Verdict is the answer the run would publish if it were asked now.
//
// It is a function of the ledger, so it moves as the evidence does and it is the same answer for
// the stream, the report and a grader. Nothing here writes: reading the verdict never changes it.
func (e *Engine) Verdict() Verdict {
	verdict := Verdict{
		Kind:    VerdictUnknown,
		Symptom: e.subject.EntityRef,
		Reason: "no hypothesis carries a supporting judgment from telemetry evidence or a human fact; " +
			"prior-only mass is not confidence (FR-022)",
	}
	open, hasOpen := e.ledger.Hypothesis(e.ledger.OpenHypothesisID())
	for _, h := range e.ledger.Hypotheses() {
		if h.Kind == ledger.KindNoObservedChange || h.Status != ledger.StatusSupported {
			continue
		}
		judgment, ok := e.ledger.EvidencedSupport(h.ID)
		if !ok {
			// Unreachable while the status machine holds; it is checked rather than assumed
			// because this is the one place the rule is worth checking twice.
			continue
		}
		return Verdict{
			Kind:         VerdictNamed,
			HypothesisID: h.ID,
			Change:       e.nameOf(h.CandidateChangeEntityID),
			Statement:    h.Statement,
			Confidence:   h.Confidence,
			Bucket:       h.Bucket.Name,
			Symptom:      e.subject.EntityRef,
			Reason: fmt.Sprintf("supported by %s (%s %s from %s)",
				judgment.EvidenceID, judgment.Direction, judgment.Strength, judgment.Source),
		}
	}
	if hasOpen {
		verdict.HypothesisID = open.ID
		verdict.Statement = open.Statement
		verdict.Confidence = open.Confidence
		verdict.Bucket = open.Bucket.Name
		if open.Status == ledger.StatusSupported {
			verdict.Kind = VerdictUnobserved
			verdict.Reason = e.unobservedReason()
		}
	}
	return verdict
}

// concludeVerdict settles the answer at a terminal stop (K1).
//
// Two things happen here and nowhere else. Every candidate still sitting at `proposed` is recorded
// `untested` with the reason and the query that would test it, because FR-031 forbids both scoring
// it as refuted and dropping it. And where nothing earned support, the open hypothesis is named:
// the run looked, nothing it could observe explains the symptom, and `unobserved` is the finding.
//
// It runs only for a stop that means the run *finished*. A budget that bound, a worker that could
// not answer and a refusal all leave the question open, and an investigation that answered
// `unobserved` because it was cut off would be claiming to have looked when it had not.
func (e *Engine) concludeVerdict(stop Stop) error {
	if err := e.recordUntestedCandidates("the investigation stopped before this candidate was tested"); err != nil {
		return err
	}
	switch stop.Reason {
	case StopCompleted, StopDiminishingReturns:
	case StopBudgetExhausted, StopWorkerUnavailable, StopRefused, StopFailed:
		return nil
	default:
		return nil
	}
	for _, h := range e.ledger.Hypotheses() {
		if h.Kind == ledger.KindNoObservedChange {
			continue
		}
		if h.Status == ledger.StatusSupported {
			// Something was named, and it was named under the rule: nothing to conclude.
			return nil
		}
	}
	open := e.ledger.OpenHypothesisID()
	if err := e.ledger.SetStatus(open, ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
		// The ledger refuses it exactly when a rival does carry evidenced support, which the loop
		// above has already ruled out; a refusal here is therefore a rule disagreeing with itself
		// and is reported rather than swallowed.
		return fmt.Errorf("engine: conclude unobserved: %w", err)
	}
	digest, err := e.ledger.Digest()
	if err != nil {
		return err
	}
	e.trajectory.LedgerUpdate(nil, digest)
	return nil
}

// unobservedReason is the sentence an `unobserved` verdict carries: what the run did, and what it
// found, in the terms a reader can check.
func (e *Engine) unobservedReason() string {
	var tested, refuted, exonerated, untested int
	for _, h := range e.ledger.Hypotheses() {
		if h.Kind == ledger.KindNoObservedChange {
			continue
		}
		switch h.Status {
		case ledger.StatusRefuted:
			refuted++
			tested++
		case ledger.StatusExonerated:
			exonerated++
		case ledger.StatusInconclusive:
			tested++
		case ledger.StatusUntested:
			untested++
		case ledger.StatusProposed, ledger.StatusSupported:
		}
	}
	return fmt.Sprintf("%d candidate(s) were tested (%d refuted), %d exonerated on the onset and %d left "+
		"untested with the query that would test them; none earned a supporting judgment from telemetry "+
		"evidence or a human fact, so the symptom on %s is localised and its cause is not observed by this "+
		"deployment's feeders (FR-071b, SC-023)",
		tested, refuted, exonerated, untested, e.subject.EntityRef)
}

// recordUntestedCandidates files every candidate the run never reached as `untested`, with the
// reason and the exact next query (FR-031).
//
// A `proposed` hypothesis at the end of a run is a hole in the report: it was neither believed nor
// ruled out, and a reader cannot tell which. `untested` with a reason and a query is the same fact
// stated so that a person can act on it.
func (e *Engine) recordUntestedCandidates(reason string) error {
	for _, h := range e.ledger.Hypotheses() {
		if h.Kind == ledger.KindNoObservedChange || h.Status != ledger.StatusProposed {
			continue
		}
		term, err := e.NextQueryFor(h)
		if err != nil {
			return err
		}
		if term == nil {
			continue
		}
		if err := e.ledger.SetStatus(h.ID, ledger.StatusUntested, ledger.StatusUpdate{
			Reason:    reason,
			NextQuery: term,
		}); err != nil {
			return fmt.Errorf("engine: record untested %s: %w", h.ID, err)
		}
	}
	return nil
}

// NextQueryFor is the exact query that would test a hypothesis nobody tested (FR-031, FR-045).
//
// It is the first-wave question, asked of the candidate's own target: the before/after comparison
// over the target's error rate, referenced where the wave would have referenced it. Where the run
// never learned the target's telemetry selectors — which is the commonest reason a candidate went
// untested at all — the next query is the pointers read that would produce them, because handing
// over a query that names a pointer id nobody holds is handing over nothing.
//
// The term is built through the same decoder every call goes through, so the query printed in a
// report is a query that can be run, not an approximation of one.
func (e *Engine) NextQueryFor(h ledger.Hypothesis) (*investigationv1.AlgebraTerm, error) {
	target := e.targetRefOf(h)
	reference := e.subject.FiredAt
	if onset := e.Onset(); onset != nil {
		reference = onset.ReferenceUsed
	}
	pair := ComparePairs(reference, e.at.ObservedAt, e.Lookback())[0]

	tool, fields := "pointers", map[string]any{
		"entity_ref":              target,
		"discriminating_question": "which telemetry selectors describe " + target + "?",
	}
	if metric := e.pointerOfKind(target, graphv1.PointerKind_METRIC); metric != "" {
		tool, fields = "compare", map[string]any{
			"pointer_id":              metric,
			"reference_at":            pair.GetReferenceAt().AsTime().UTC().Format(time.RFC3339),
			"width_seconds":           pair.GetWidthSeconds(),
			"statistic":               "error_rate",
			"serves_hypothesis_id":    h.ID,
			"discriminating_question": "did the error rate on " + target + " move across the reference instant?",
		}
	}
	req, err := DecodeCall(tool, jsonObject(fields), e.catalogue, e.at)
	if err != nil {
		return nil, fmt.Errorf("engine: next query for %s: %w", h.ID, err)
	}
	return req.GetTerm(), nil
}

// targetRefOf is the entity whose telemetry would show a hypothesis's effect, as planFirstWave
// reads it: the change's first target, or the subject where the graph never described one.
func (e *Engine) targetRefOf(h ledger.Hypothesis) string {
	for _, id := range h.TargetEntityIDs {
		if ref := e.catalogue.RefFor(id); ref != "" {
			return ref
		}
	}
	return e.subject.EntityRef
}
