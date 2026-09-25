// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"errors"
	"fmt"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The status machine and the write-time rules (T055, FR-020, FR-022, FR-023, FR-031, FR-051,
// data-model §State transitions and §Validation rules).
//
// A status is a claim about what the evidence did, so every status that claims something has to
// name the evidence behind it:
//
//	proposed ──► supported     ≥ 1 supports judgment from a source-of-truth worker
//	         ├─► refuted       the net evidence bears against it
//	         ├─► inconclusive  tested, nothing separated it
//	         ├─► untested      no pointer of the needed kind, or no data for the window
//	         └─► exonerated    starts after onset by more than the onset's uncertainty
//
// Two of those rules are the reason this file exists rather than a setter:
//
//   - `supported` is refused with `unsupported_status` unless a `supports` judgment rests on an
//     evidence item whose worker declares a source of truth. A model saying a thing is supported
//     is not a thing being supported, and a hypothesis resting only on a document is
//     knowledge-derived and may not reach `supported` until a source-of-truth worker confirms it
//     (FR-051).
//   - `untested` requires both a reason and the exact next query. FR-031 forbids scoring it as
//     refuted and forbids dropping it, which leaves exactly one honest thing to do with it:
//     report it, say why it was not tested, and hand over the query that would test it.
//
// A status never moves back to `proposed`. Everything else may move, because the ledger is a live
// belief state and evidence arriving late is the normal case, not the exception.

// StatusUpdate carries what a status needs to be legal.
type StatusUpdate struct {
	// Reason is why a hypothesis is untested (FR-031). Required for StatusUntested.
	Reason string
	// NextQuery is the exact algebra term that would test it, and NextQueryDeepLink the link a
	// person can follow instead (FR-031, FR-045). NextQuery is required for StatusUntested.
	NextQuery         *investigationv1.AlgebraTerm
	NextQueryDeepLink string
	// OnsetEvidenceID is the onset estimate an exoneration rests on (Invariant 9). Required for
	// StatusExonerated; Exonerate fills it in for the usual path.
	OnsetEvidenceID string
}

// SetStatus moves a hypothesis's status, refusing the moves the published rules forbid.
func (l *Ledger) SetStatus(hypothesisID string, status Status, update StatusUpdate) error {
	i, ok := l.index[hypothesisID]
	if !ok {
		return fmt.Errorf("ledger: set status: hypothesis %s is not in the ledger", hypothesisID)
	}
	h := &l.hypotheses[i]

	switch status {
	case StatusProposed:
		return fmt.Errorf("ledger: set status %s: a hypothesis never returns to %s; what was learned stays learned",
			hypothesisID, StatusProposed)
	case StatusSupported:
		if err := l.checkSupported(*h); err != nil {
			return err
		}
	case StatusUntested:
		if update.Reason == "" || update.NextQuery == nil {
			return fmt.Errorf("ledger: set status %s: %s carries a reason and the exact next query; it is "+
				"never scored as refuted and never dropped (FR-031)", hypothesisID, StatusUntested)
		}
	case StatusExonerated:
		if update.OnsetEvidenceID == "" {
			return fmt.Errorf("ledger: set status %s: %s carries the onset estimate's evidence id (Invariant 9)",
				hypothesisID, StatusExonerated)
		}
		if _, ok := l.evidence[update.OnsetEvidenceID]; !ok {
			return fmt.Errorf("ledger: set status %s: onset evidence %s is not in the ledger",
				hypothesisID, update.OnsetEvidenceID)
		}
	case StatusRefuted, StatusInconclusive:
	default:
		return fmt.Errorf("ledger: set status %s: %w: status %q", hypothesisID, ErrVocabulary, status)
	}

	h.Status = status
	h.UntestedReason = update.Reason
	if status != StatusUntested {
		h.UntestedReason = ""
	}
	if update.NextQuery != nil {
		h.NextQuery = update.NextQuery
		h.NextQueryDeepLink = update.NextQueryDeepLink
	}
	if update.OnsetEvidenceID != "" {
		h.OnsetEvidenceID = update.OnsetEvidenceID
	}
	l.recompute()
	return nil
}

// checkSupported is the `unsupported_status` rule: `supported` needs a qualifying judgment.
func (l *Ledger) checkSupported(h Hypothesis) error {
	if h.KnowledgeDerived && !h.KnowledgeConfirmed {
		return rejectf(ReasonUnsupportedStatus,
			"hypothesis %s rests only on a document; it stays unconfirmed until a source-of-truth worker "+
				"confirms it (FR-051)", h.ID)
	}
	if h.Kind == KindNoObservedChange {
		return l.checkOpenSupported(h)
	}
	if _, ok := l.EvidencedSupport(h.ID); ok {
		return nil
	}
	return rejectf(ReasonUnsupportedStatus,
		"hypothesis %s has no supporting judgment from telemetry evidence or a human fact; prior-only mass "+
			"is not confidence and a hypothesis nothing observed supports is never named (FR-022, constitution V)",
		h.ID)
}

// checkOpenSupported is the same rule read for the open hypothesis, which nothing can support
// directly.
//
// "No observed change explains this" is the one claim that is carried by what the run ruled
// *out*: it becomes the answer exactly when every rival the run could reach failed to earn the
// support that would let it be named. So it is allowed to `supported` only when no rival is
// `supported` — which is what makes `unobserved` a finding rather than a shrug, and what stops it
// being asserted over a candidate the evidence actually backs.
//
// A rival that carries one supporting judgment and two refuting ones does not block it: that
// hypothesis is refuted, the run looked at it and ruled it out, and a single digest leaning one
// way inside a refuted case is not a reason to withhold the only answer left.
func (l *Ledger) checkOpenSupported(h Hypothesis) error {
	for _, other := range l.hypotheses {
		if other.ID == h.ID || other.Status != StatusSupported {
			continue
		}
		return rejectf(ReasonUnsupportedStatus,
			"the open hypothesis cannot be supported while %s is %s (FR-022)", other.ID, StatusSupported)
	}
	return nil
}

// EvidencedSupport returns the first supporting judgment a hypothesis carries that rests on
// telemetry evidence or on a human fact, and whether there is one (Phase 8 K1, FR-022).
//
// This is the published verdict rule, stated once here so the status machine, the reported bucket
// and the engine's conclusion all read it the same way: **a hypothesis may be named only if
// something observed supports it.** A prior is a reason to look, not a reason to believe; a graph
// answer says a change happened near the subject, which is what put the hypothesis on the list in
// the first place and cannot also be the evidence that it is the cause; and a document says what
// happened last time. None of the three is an observation of this incident.
//
// The judgments are read in recorded order, so the answer is stable across replays.
func (l *Ledger) EvidencedSupport(hypothesisID string) (Judgment, bool) {
	for _, j := range l.judgments {
		if j.HypothesisID != hypothesisID || j.Direction != Supports {
			continue
		}
		// A rollback away from the change is a person's judgement about this incident, recorded by the
		// platform: an observation of the incident's response rather than a graph answer that merely
		// says a change happened nearby (004 T155).
		if j.Source == SourceHumanFact || j.Source == SourceRollback {
			return j, true
		}
		if e, ok := l.evidence[j.EvidenceID]; ok && TelemetryEvidence(e) {
			return j, true
		}
	}
	return Judgment{}, false
}

// EvidenceKindAlgebraAnswer is the kind a telemetry worker's answer is filed under, spelled as the
// schema spells it.
const EvidenceKindAlgebraAnswer = "algebra_answer"

// TelemetryEvidence reports whether an evidence item is an observation of this incident: an
// answer from a source-of-truth telemetry worker, or an onset estimate derived from one.
//
// It is a function of the row rather than of a worker registry, for the reason EvidenceItem's own
// documentation gives: a ledger read back in 2029 must still be able to tell what its confidences
// rested on, and the registry will have moved on by then.
func TelemetryEvidence(e EvidenceItem) bool {
	switch e.Kind {
	case EvidenceKindAlgebraAnswer, EvidenceKindOnsetEstimate:
		return e.SourceOfTruth != ""
	default:
		// `graph_answer`, `knowledge_item`, `resolution_audit`, `injection_attempt`: each is a
		// statement about the world's structure or about a document, and neither is a measurement
		// of this incident.
		return false
	}
}

// Evidenced reports whether a hypothesis's confidence rests on anything observed: a supporting
// judgment from telemetry evidence or a human fact, or — for the open hypothesis — a
// telemetry-backed refutation of a rival. It is what caps the reported bucket, and what the engine
// reads to decide whether anything may be named at all.
func (l *Ledger) Evidenced(hypothesisID string) bool {
	i, ok := l.index[hypothesisID]
	if !ok {
		return false
	}
	return l.evidencedBelief(l.hypotheses[i])
}

// evidencedBelief reports whether a hypothesis's confidence rests on anything observed, which is
// what caps the bucket it is reported in (see reportedBucket).
//
// For the open hypothesis the evidence is the refutations of its rivals: a run that tested the
// candidates and ruled them out has measured something, and a run that tested nothing has not.
func (l *Ledger) evidencedBelief(h Hypothesis) bool {
	if _, ok := l.EvidencedSupport(h.ID); ok {
		return true
	}
	if h.Kind != KindNoObservedChange {
		return false
	}
	for _, j := range l.judgments {
		if j.HypothesisID == h.ID || j.Direction != Refutes {
			continue
		}
		if e, ok := l.evidence[j.EvidenceID]; ok && TelemetryEvidence(e) {
			return true
		}
	}
	return false
}

// Exoneration is a change ruled out by the onset estimate (FR-029c, FR-057c).
type Exoneration struct {
	// JudgmentID is the id of the judgment the exoneration records. An exoneration is evidence
	// like any other and moves the confidence through a judgment like any other.
	JudgmentID string
	// OnsetEvidenceID is the onset estimate the exoneration rests on. It must already be in the
	// ledger as an evidence item.
	OnsetEvidenceID string
	// Strength is the strength of the refuting judgment. Empty means Decisive: a change that
	// starts after onset by more than the onset's own uncertainty is not a candidate cause, and
	// that is an argument from the clock rather than an inference from a digest.
	Strength Strength
	// Reason is the sentence the rendering shows beside the exoneration.
	Reason string
	// WorkerCallID is the call the onset estimate came from, where there was one.
	WorkerCallID string
}

// Exonerate rules a change out on the onset estimate, as a first-class finding (FR-029c).
//
// An exoneration is not a deletion and not a silent demotion. It does three things, all of them
// visible in the export: it records a refuting judgment sourced `exoneration`, which is what
// actually moves the confidence; it re-roles the hypothesis as a candidate *effect* of the
// incident rather than a candidate cause; and it sets the status, carrying the onset estimate's
// evidence id so a reader can check the clock for themselves. "This change was not the cause, and
// here is the onset it starts after" is one of the most useful things an investigation produces —
// FR-057c renders it as prominently as a support.
func (l *Ledger) Exonerate(hypothesisID string, ex Exoneration) (Judgment, error) {
	if _, ok := l.index[hypothesisID]; !ok {
		return Judgment{}, fmt.Errorf("ledger: exonerate: hypothesis %s is not in the ledger", hypothesisID)
	}
	if ex.JudgmentID == "" {
		return Judgment{}, errors.New("ledger: exonerate: a judgment id is required")
	}
	if ex.Reason == "" {
		return Judgment{}, fmt.Errorf("ledger: exonerate %s: a reason is required; an exoneration is reported, "+
			"not applied silently (FR-057c)", hypothesisID)
	}
	if ex.Strength == "" {
		ex.Strength = Decisive
	}
	evidence, ok := l.evidence[ex.OnsetEvidenceID]
	if !ok {
		return Judgment{}, fmt.Errorf("ledger: exonerate %s: onset evidence %s is not in the ledger",
			hypothesisID, ex.OnsetEvidenceID)
	}
	if evidence.Kind != EvidenceKindOnsetEstimate {
		return Judgment{}, fmt.Errorf("ledger: exonerate %s: evidence %s is a %s, not an %s",
			hypothesisID, ex.OnsetEvidenceID, evidence.Kind, EvidenceKindOnsetEstimate)
	}

	judgment, err := l.Judge(Judgment{
		ID:           ex.JudgmentID,
		HypothesisID: hypothesisID,
		EvidenceID:   ex.OnsetEvidenceID,
		Direction:    Refutes,
		Strength:     ex.Strength,
		Source:       SourceExoneration,
		WorkerCallID: ex.WorkerCallID,
	})
	if err != nil {
		return Judgment{}, err
	}

	i := l.index[hypothesisID]
	l.hypotheses[i].CausalRole = RoleCandidateEffect
	// The reason is kept per exoneration rather than appended to the rationale string, because
	// the rationale is rebuilt from the rows on every later mutation and an appended reason would
	// be lost the next time anything else moved (see Ledger.exonerations).
	l.exonerations[hypothesisID] = ex.Reason
	if err := l.SetStatus(hypothesisID, StatusExonerated, StatusUpdate{
		OnsetEvidenceID: ex.OnsetEvidenceID,
	}); err != nil {
		return Judgment{}, err
	}
	l.hypotheses[i].Rationale = l.rationale(l.hypotheses[i])
	return judgment, nil
}

// EvidenceKindOnsetEstimate is the evidence kind an exoneration must rest on, spelled as the
// schema spells it.
const EvidenceKindOnsetEstimate = "onset_estimate"

// MarkKnowledge records that a hypothesis came from a document, and whether a source-of-truth
// worker has since confirmed it (FR-051).
//
// An unconfirmed knowledge-derived hypothesis may not reach `supported`: a runbook saying this
// happened last time is a reason to look, not a finding.
func (l *Ledger) MarkKnowledge(hypothesisID string, derived, confirmed bool) error {
	i, ok := l.index[hypothesisID]
	if !ok {
		return fmt.Errorf("ledger: mark knowledge: hypothesis %s is not in the ledger", hypothesisID)
	}
	h := &l.hypotheses[i]
	if derived && !confirmed && h.Status == StatusSupported {
		return rejectf(ReasonUnsupportedStatus,
			"hypothesis %s is %s; marking it unconfirmed knowledge-derived would leave a supported claim "+
				"resting on a document (FR-051)", hypothesisID, StatusSupported)
	}
	h.KnowledgeDerived = derived
	h.KnowledgeConfirmed = confirmed
	return nil
}

// SetNextQuery records the exact query that would move a hypothesis, without changing its status.
// The budget manager uses it to name what it left untested (FR-045).
func (l *Ledger) SetNextQuery(hypothesisID string, term *investigationv1.AlgebraTerm, deepLink string) error {
	i, ok := l.index[hypothesisID]
	if !ok {
		return fmt.Errorf("ledger: set next query: hypothesis %s is not in the ledger", hypothesisID)
	}
	l.hypotheses[i].NextQuery = term
	l.hypotheses[i].NextQueryDeepLink = deepLink
	return nil
}
