// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The exported ledger (T057, FR-020b).
//
// FR-020b asks for a ledger "readable by graders and reviewers without replaying the run", and
// that is a stronger requirement than it looks. It means the export has to carry the evidence
// ids, the typed judgments and the priors — not only the conclusion — because a grader's question
// is never "what did it decide" but "would I have decided that from this". It also means the
// export must be *stable*: two runs that reached the same ledger have to produce the same bytes,
// or a diff between two trajectories is unreadable noise.
//
// So there are three renderings of one structure, and they are all the same structure:
//
//   - Proto is the wire form, `investigationv1.Ledger`, which the RPC and the export command
//     return;
//   - CanonicalJSON is the recording and replay form, through 001's canonical serializer — sorted
//     keys, RFC 3339 UTC, unpopulated fields omitted — so a ledger digest means something;
//   - Render (render.go) is the compact table the engine re-injects into the model's context each
//     turn.
//
// Nothing here computes: every number in the export was computed by posterior.go and is being
// copied. That is deliberate, and it is why the export can be trusted as evidence of what the
// ledger held: an exporter that recomputed could disagree with the rows, and then which one is
// the investigation?

// Proto renders the ledger as the published `investigationv1.Ledger` message.
//
// Hypotheses come out in published rank order, and each carries the evidence ids for and against
// it, so that a reader with the message alone can see both sides of a conflict without joining
// anything (FR-024).
func (l *Ledger) Proto() *investigationv1.Ledger {
	out := &investigationv1.Ledger{
		InvestigationId:   l.investigationID,
		LedgerRuleVersion: LedgerRuleVersion,
	}
	for _, b := range buckets {
		out.Buckets = append(out.Buckets, &investigationv1.ConfidenceBucket{
			Name: b.Name, RangeLow: b.Low, RangeHigh: b.High,
		})
	}
	for _, h := range l.Hypotheses() {
		out.Hypotheses = append(out.Hypotheses, l.hypothesisProto(h))
	}
	for _, j := range l.judgments {
		out.Judgments = append(out.Judgments, judgmentProto(j))
	}
	for _, e := range l.EvidenceItems() {
		out.Evidence = append(out.Evidence, evidenceProto(e))
	}
	return out
}

func (l *Ledger) hypothesisProto(h Hypothesis) *investigationv1.Hypothesis {
	out := &investigationv1.Hypothesis{
		HypothesisId:            h.ID,
		Kind:                    kindProto(h.Kind),
		Statement:               h.Statement,
		CandidateChangeEntityId: h.CandidateChangeEntityID,
		TargetEntityIds:         h.TargetEntityIDs,
		CausalRole:              roleProto(h.CausalRole),
		ActorKind:               h.ActorKind,
		Prior:                   h.Prior,
		Status:                  statusProto(h.Status),
		UntestedReason:          h.UntestedReason,
		Confidence:              h.Confidence,
		Bucket: &investigationv1.ConfidenceBucket{
			Name: h.Bucket.Name, RangeLow: h.Bucket.Low, RangeHigh: h.Bucket.High,
		},
		Widened:            h.Widened,
		WidenedReason:      h.WidenedReason,
		Rank:               uint32(h.Rank), //nolint:gosec // a rank is 1..n over the ledger's own rows
		Rationale:          h.Rationale,
		KnowledgeDerived:   h.KnowledgeDerived,
		KnowledgeConfirmed: h.KnowledgeConfirmed,
		NextQuery:          h.NextQuery,
		NextQueryDeepLink:  h.NextQueryDeepLink,
	}
	for _, j := range l.judgments {
		if j.HypothesisID != h.ID {
			continue
		}
		out.JudgmentIds = append(out.JudgmentIds, j.ID)
		switch j.Direction {
		case Supports:
			out.SupportingEvidenceIds = append(out.SupportingEvidenceIds, j.EvidenceID)
		case Refutes:
			out.RefutingEvidenceIds = append(out.RefutingEvidenceIds, j.EvidenceID)
		case Neutral:
		}
	}
	// Sorted, not in arrival order. A hypothesis is a *set* of evidence for and against, and two
	// runs that gathered the same evidence in different orders must export the same bytes —
	// otherwise a grader diffing two trajectories reads permutation noise as disagreement. The
	// judgments list on the Ledger itself keeps recording order, because that one really is a
	// sequence.
	slices.Sort(out.JudgmentIds)
	slices.Sort(out.SupportingEvidenceIds)
	slices.Sort(out.RefutingEvidenceIds)
	return out
}

func judgmentProto(j Judgment) *investigationv1.Judgment {
	out := &investigationv1.Judgment{
		JudgmentId:   j.ID,
		HypothesisId: j.HypothesisID,
		EvidenceId:   j.EvidenceID,
		Direction:    directionProto(j.Direction),
		Strength:     strengthProto(j.Strength),
		LnLr:         j.LnLR,
		Source:       string(j.Source),
		WorkerCallId: j.WorkerCallID,
	}
	if !j.RecordedAt.IsZero() {
		out.RecordedAt = timestamppb.New(j.RecordedAt.UTC())
	}
	return out
}

func evidenceProto(e EvidenceItem) *investigationv1.EvidenceItem {
	out := &investigationv1.EvidenceItem{
		EvidenceId:           e.ID,
		Kind:                 e.Kind,
		Worker:               e.Worker,
		Capability:           e.Capability,
		Term:                 e.Term,
		Mode:                 e.Mode,
		Outcome:              outcomeProto(e.Outcome),
		ResponseDigest:       e.ResponseDigest,
		ResponseKey:          e.ResponseKey,
		Coverage:             e.Coverage,
		JoinKeys:             e.JoinKeys,
		GraphEventIds:        e.GraphEventIDs,
		DeepLink:             e.DeepLink,
		DeepLinkAbsentReason: e.DeepLinkAbsentReason,
		Truncated:            e.Truncated,
	}
	if !e.ValidAt.IsZero() {
		out.ValidAt = timestamppb.New(e.ValidAt.UTC())
	}
	if !e.ObservedAt.IsZero() {
		out.ObservedAt = timestamppb.New(e.ObservedAt.UTC())
	}
	if !e.CalledAt.IsZero() {
		out.CalledAt = timestamppb.New(e.CalledAt.UTC())
	}
	return out
}

// CanonicalJSON renders the ledger through 001's canonical serializer: the form a recording holds
// and a replay compares byte for byte (research §9).
func (l *Ledger) CanonicalJSON() ([]byte, error) {
	raw, err := graph.CanonicalJSON(l.Proto())
	if err != nil {
		return nil, fmt.Errorf("ledger: canonical json: %w", err)
	}
	return raw, nil
}

// Digest is the SHA-256 of the canonical JSON, hex-encoded: the `ledger_digest` a trajectory's
// `ledger_update` record carries, and the number that makes "the ledger did not change" a
// checkable statement rather than an impression.
func (l *Ledger) Digest() (string, error) {
	raw, err := l.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// KindProto is the published mapping from a ledger kind to its proto value. It is exported
// because the trajectory's final-ledger record needs the same mapping the decision record uses,
// and two mappings of one enum is one mapping too many.
func KindProto(k Kind) investigationv1.HypothesisKind { return kindProto(k) }

// StatusProto is the published mapping from a ledger status to its proto value.
func StatusProto(s Status) investigationv1.HypothesisStatus { return statusProto(s) }

func kindProto(k Kind) investigationv1.HypothesisKind {
	switch k {
	case KindChange:
		return investigationv1.HypothesisKind_CHANGE
	case KindCondition:
		return investigationv1.HypothesisKind_CONDITION
	case KindNoObservedChange:
		return investigationv1.HypothesisKind_NO_OBSERVED_CHANGE
	default:
		return investigationv1.HypothesisKind_HYPOTHESIS_KIND_UNSPECIFIED
	}
}

func statusProto(s Status) investigationv1.HypothesisStatus {
	switch s {
	case StatusProposed:
		return investigationv1.HypothesisStatus_PROPOSED
	case StatusSupported:
		return investigationv1.HypothesisStatus_SUPPORTED
	case StatusRefuted:
		return investigationv1.HypothesisStatus_REFUTED
	case StatusInconclusive:
		return investigationv1.HypothesisStatus_INCONCLUSIVE
	case StatusUntested:
		return investigationv1.HypothesisStatus_UNTESTED
	case StatusExonerated:
		return investigationv1.HypothesisStatus_EXONERATED
	default:
		return investigationv1.HypothesisStatus_HYPOTHESIS_STATUS_UNSPECIFIED
	}
}

func roleProto(r CausalRole) investigationv1.CausalRole {
	switch r {
	case RoleCause:
		return investigationv1.CausalRole_CAUSE
	case RoleCandidateEffect:
		return investigationv1.CausalRole_CANDIDATE_EFFECT
	default:
		return investigationv1.CausalRole_CAUSAL_ROLE_UNSPECIFIED
	}
}

func directionProto(d Direction) investigationv1.JudgmentDirection {
	switch d {
	case Supports:
		return investigationv1.JudgmentDirection_SUPPORTS
	case Refutes:
		return investigationv1.JudgmentDirection_REFUTES
	case Neutral:
		return investigationv1.JudgmentDirection_NEUTRAL
	default:
		return investigationv1.JudgmentDirection_JUDGMENT_DIRECTION_UNSPECIFIED
	}
}

func strengthProto(s Strength) investigationv1.JudgmentStrength {
	switch s {
	case Weak:
		return investigationv1.JudgmentStrength_WEAK
	case Moderate:
		return investigationv1.JudgmentStrength_MODERATE
	case Strong:
		return investigationv1.JudgmentStrength_STRONG
	case Decisive:
		return investigationv1.JudgmentStrength_DECISIVE
	default:
		return investigationv1.JudgmentStrength_JUDGMENT_STRENGTH_UNSPECIFIED
	}
}

// outcomeProto maps the six published outcomes, which are never collapsed into one another
// (FR-027, Invariant 8).
func outcomeProto(outcome string) investigationv1.TermOutcome {
	switch outcome {
	case "digest":
		return investigationv1.TermOutcome_DIGEST
	case "no_data":
		return investigationv1.TermOutcome_NO_DATA
	case "not_yet_ingested":
		return investigationv1.TermOutcome_NOT_YET_INGESTED
	case "query_failed":
		return investigationv1.TermOutcome_QUERY_FAILED
	case "not_recorded":
		return investigationv1.TermOutcome_NOT_RECORDED
	case "partial":
		return investigationv1.TermOutcome_PARTIAL
	default:
		return investigationv1.TermOutcome_TERM_OUTCOME_UNSPECIFIED
	}
}
