// SPDX-License-Identifier: Apache-2.0

package store

import (
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The stored ledger as the published message (FR-064, FR-053).
//
// `internal/investigation/ledger` renders a *live* ledger — one that is still computing — and its
// converters hang off that object. This renders the *rows*, which is what an export, a `get
// --ledger` and a replay read: the run is over, the numbers are in the database, and the job is
// to copy them.
//
// Copying is the whole discipline, and it is the same one ledger/export.go opens with: nothing
// here recomputes. Rebuilding a live ledger from the rows and asking it for its own proto would
// recompute every prior, bucket, rank and rationale from the scores — and the scores are not in
// the rows, only their results are. An exporter that recomputed could disagree with the row it
// exported, and then which one is the investigation?

// Proto renders stored ledger rows as the published `investigationv1.Ledger`.
//
// Hypotheses come out in the rank order the rows carry, and each names the evidence for and
// against it, sorted — a hypothesis is a *set* of evidence, and two runs that gathered the same
// evidence in different orders must export the same bytes (FR-024).
func (rows *LedgerRows) Proto() *investigationv1.Ledger {
	if rows == nil {
		return nil
	}
	out := &investigationv1.Ledger{
		InvestigationId:   rows.InvestigationID,
		LedgerRuleVersion: ledger.LedgerRuleVersion,
	}
	for _, b := range ledger.Buckets() {
		out.Buckets = append(out.Buckets, &investigationv1.ConfidenceBucket{
			Name: b.Name, RangeLow: b.Low, RangeHigh: b.High,
		})
	}
	for _, h := range rows.Hypotheses {
		out.Hypotheses = append(out.Hypotheses, hypothesisProtoOf(h, rows.Judgments))
	}
	for _, j := range rows.Judgments {
		out.Judgments = append(out.Judgments, JudgmentProto(j))
	}
	for _, e := range rows.Evidence {
		out.Evidence = append(out.Evidence, EvidenceProto(e))
	}
	return out
}

func hypothesisProtoOf(h ledger.Hypothesis, judgments []ledger.Judgment) *investigationv1.Hypothesis {
	out := &investigationv1.Hypothesis{
		HypothesisId:            h.ID,
		Kind:                    hypothesisKindProto(h.Kind),
		Statement:               h.Statement,
		CandidateChangeEntityId: h.CandidateChangeEntityID,
		TargetEntityIds:         h.TargetEntityIDs,
		CausalRole:              causalRoleProto(h.CausalRole),
		ActorKind:               h.ActorKind,
		Prior:                   h.Prior,
		Status:                  hypothesisStatusProto(h.Status),
		UntestedReason:          h.UntestedReason,
		Confidence:              h.Confidence,
		Bucket: &investigationv1.ConfidenceBucket{
			Name: h.Bucket.Name, RangeLow: h.Bucket.Low, RangeHigh: h.Bucket.High,
		},
		Widened:            h.Widened,
		WidenedReason:      h.WidenedReason,
		Rank:               uint32(max(h.Rank, 0)), //nolint:gosec // a rank is 1..n over this run's rows
		Rationale:          h.Rationale,
		KnowledgeDerived:   h.KnowledgeDerived,
		KnowledgeConfirmed: h.KnowledgeConfirmed,
		NextQuery:          h.NextQuery,
		NextQueryDeepLink:  h.NextQueryDeepLink,
	}
	for _, j := range judgments {
		if j.HypothesisID != h.ID {
			continue
		}
		out.JudgmentIds = append(out.JudgmentIds, j.ID)
		switch j.Direction {
		case ledger.Supports:
			out.SupportingEvidenceIds = append(out.SupportingEvidenceIds, j.EvidenceID)
		case ledger.Refutes:
			out.RefutingEvidenceIds = append(out.RefutingEvidenceIds, j.EvidenceID)
		case ledger.Neutral:
		}
	}
	slices.Sort(out.JudgmentIds)
	slices.Sort(out.SupportingEvidenceIds)
	slices.Sort(out.RefutingEvidenceIds)
	return out
}

// JudgmentProto renders one stored judgment.
func JudgmentProto(j ledger.Judgment) *investigationv1.Judgment {
	out := &investigationv1.Judgment{
		JudgmentId:   j.ID,
		HypothesisId: j.HypothesisID,
		EvidenceId:   j.EvidenceID,
		Direction:    judgmentDirectionProto(j.Direction),
		Strength:     judgmentStrengthProto(j.Strength),
		LnLr:         j.LnLR,
		Source:       string(j.Source),
		WorkerCallId: j.WorkerCallID,
	}
	if !j.RecordedAt.IsZero() {
		out.RecordedAt = timestamppb.New(j.RecordedAt.UTC())
	}
	return out
}

// EvidenceProto renders one stored evidence item.
func EvidenceProto(e ledger.EvidenceItem) *investigationv1.EvidenceItem {
	out := &investigationv1.EvidenceItem{
		EvidenceId:           e.ID,
		Kind:                 e.Kind,
		Worker:               e.Worker,
		Capability:           e.Capability,
		Term:                 e.Term,
		Mode:                 e.Mode,
		Outcome:              termOutcomeProto(e.Outcome),
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

func hypothesisKindProto(k ledger.Kind) investigationv1.HypothesisKind {
	switch k {
	case ledger.KindChange:
		return investigationv1.HypothesisKind_CHANGE
	case ledger.KindCondition:
		return investigationv1.HypothesisKind_CONDITION
	case ledger.KindNoObservedChange:
		return investigationv1.HypothesisKind_NO_OBSERVED_CHANGE
	default:
		return investigationv1.HypothesisKind_HYPOTHESIS_KIND_UNSPECIFIED
	}
}

func hypothesisStatusProto(s ledger.Status) investigationv1.HypothesisStatus {
	switch s {
	case ledger.StatusProposed:
		return investigationv1.HypothesisStatus_PROPOSED
	case ledger.StatusSupported:
		return investigationv1.HypothesisStatus_SUPPORTED
	case ledger.StatusRefuted:
		return investigationv1.HypothesisStatus_REFUTED
	case ledger.StatusInconclusive:
		return investigationv1.HypothesisStatus_INCONCLUSIVE
	case ledger.StatusUntested:
		return investigationv1.HypothesisStatus_UNTESTED
	case ledger.StatusExonerated:
		return investigationv1.HypothesisStatus_EXONERATED
	default:
		return investigationv1.HypothesisStatus_HYPOTHESIS_STATUS_UNSPECIFIED
	}
}

func causalRoleProto(r ledger.CausalRole) investigationv1.CausalRole {
	switch r {
	case ledger.RoleCause:
		return investigationv1.CausalRole_CAUSE
	case ledger.RoleCandidateEffect:
		return investigationv1.CausalRole_CANDIDATE_EFFECT
	default:
		return investigationv1.CausalRole_CAUSAL_ROLE_UNSPECIFIED
	}
}

func judgmentDirectionProto(d ledger.Direction) investigationv1.JudgmentDirection {
	switch d {
	case ledger.Supports:
		return investigationv1.JudgmentDirection_SUPPORTS
	case ledger.Refutes:
		return investigationv1.JudgmentDirection_REFUTES
	case ledger.Neutral:
		return investigationv1.JudgmentDirection_NEUTRAL
	default:
		return investigationv1.JudgmentDirection_JUDGMENT_DIRECTION_UNSPECIFIED
	}
}

func judgmentStrengthProto(s ledger.Strength) investigationv1.JudgmentStrength {
	switch s {
	case ledger.Weak:
		return investigationv1.JudgmentStrength_WEAK
	case ledger.Moderate:
		return investigationv1.JudgmentStrength_MODERATE
	case ledger.Strong:
		return investigationv1.JudgmentStrength_STRONG
	case ledger.Decisive:
		return investigationv1.JudgmentStrength_DECISIVE
	default:
		return investigationv1.JudgmentStrength_JUDGMENT_STRENGTH_UNSPECIFIED
	}
}
