// SPDX-License-Identifier: Apache-2.0

package render

import (
	"fmt"
	"sort"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The rendering's view of a ledger.
//
// The renderer has to work in two places that do not share a process: inside the engine, where
// the live `*ledger.Ledger` is in memory, and inside the CLI, which has only the
// `Investigation` message that came back over the wire. Both must produce the *same* text —
// FR-064 says the two surfaces carry the same claims, and a second rendering path would be a
// second set of claims waiting to drift.
//
// So the renderer reads a view: a flat, already-ordered projection with no behaviour. FromLedger
// builds it from the engine's ledger and FromProtoLedger from the wire message, and everything
// after that is one code path.

// HypothesisView is one ranked hypothesis, as the rendering needs it.
type HypothesisView struct {
	ID                      string
	Kind                    string
	Statement               string
	CandidateChangeEntityID string
	Status                  string
	UntestedReason          string
	OnsetEvidenceID         string
	Bucket                  string
	Widened                 bool
	WidenedReason           string
	WidenedDirection        string
	Rank                    int
	Rationale               string
}

// JudgmentView is one typed judgment.
type JudgmentView struct {
	HypothesisID string
	EvidenceID   string
	Direction    string
	Strength     string
	Source       string
	LnLR         float64
}

// EvidenceView is one evidence item, as a citation needs it.
type EvidenceView struct {
	ID                   string
	Kind                 string
	Worker               string
	Capability           string
	Outcome              string
	DeepLink             string
	DeepLinkAbsentReason string
	FreeText             string
	Truncated            bool
	TruncationNote       string
}

// LedgerView is the whole projection: hypotheses in rank order, judgments, and evidence by id.
type LedgerView struct {
	Hypotheses []HypothesisView
	Judgments  []JudgmentView
	Evidence   map[string]EvidenceView
}

// Empty reports whether there is nothing to render.
func (v LedgerView) Empty() bool { return len(v.Hypotheses) == 0 }

// JudgmentsFor returns the judgments bearing on one hypothesis, in the order they were recorded.
func (v LedgerView) JudgmentsFor(hypothesisID string) []JudgmentView {
	var out []JudgmentView
	for _, j := range v.Judgments {
		if j.HypothesisID == hypothesisID {
			out = append(out, j)
		}
	}
	return out
}

// EvidenceByID returns one evidence item.
func (v LedgerView) EvidenceByID(id string) (EvidenceView, bool) {
	item, ok := v.Evidence[id]
	return item, ok
}

// FromLedger projects the engine's in-memory ledger.
func FromLedger(l *ledger.Ledger) LedgerView {
	if l == nil {
		return LedgerView{Evidence: map[string]EvidenceView{}}
	}
	view := LedgerView{Evidence: map[string]EvidenceView{}}
	for _, h := range l.Hypotheses() {
		view.Hypotheses = append(view.Hypotheses, HypothesisView{
			ID:                      h.ID,
			Kind:                    string(h.Kind),
			Statement:               h.Statement,
			CandidateChangeEntityID: h.CandidateChangeEntityID,
			Status:                  string(h.Status),
			UntestedReason:          h.UntestedReason,
			OnsetEvidenceID:         h.OnsetEvidenceID,
			Bucket:                  h.Bucket.String(),
			Widened:                 h.Widened,
			WidenedReason:           h.WidenedReason,
			WidenedDirection:        string(h.WidenedDirection),
			Rank:                    h.Rank,
			Rationale:               h.Rationale,
		})
	}
	for _, j := range l.Judgments() {
		view.Judgments = append(view.Judgments, JudgmentView{
			HypothesisID: j.HypothesisID,
			EvidenceID:   j.EvidenceID,
			Direction:    string(j.Direction),
			Strength:     string(j.Strength),
			Source:       string(j.Source),
			LnLR:         j.LnLR,
		})
	}
	for _, e := range l.EvidenceItems() {
		view.Evidence[e.ID] = EvidenceView{
			ID: e.ID, Kind: e.Kind, Worker: e.Worker, Capability: e.Capability,
			Outcome: e.Outcome, DeepLink: e.DeepLink,
			DeepLinkAbsentReason: e.DeepLinkAbsentReason, FreeText: e.FreeText,
			Truncated: e.Truncated, TruncationNote: e.TruncationNote,
		}
	}
	sortByRank(view.Hypotheses)
	return view
}

// FromProtoLedger projects the wire message. It is what the CLI renders from.
func FromProtoLedger(l *investigationv1.Ledger) LedgerView {
	view := LedgerView{Evidence: map[string]EvidenceView{}}
	if l == nil {
		return view
	}
	for _, h := range l.GetHypotheses() {
		view.Hypotheses = append(view.Hypotheses, HypothesisView{
			ID:                      h.GetHypothesisId(),
			Kind:                    hypothesisKindWord(h.GetKind()),
			Statement:               h.GetStatement(),
			CandidateChangeEntityID: h.GetCandidateChangeEntityId(),
			Status:                  hypothesisStatusWord(h.GetStatus()),
			UntestedReason:          h.GetUntestedReason(),
			Bucket:                  bucketWord(h.GetBucket()),
			Widened:                 h.GetWidened(),
			WidenedReason:           h.GetWidenedReason(),
			Rank:                    int(h.GetRank()),
			Rationale:               h.GetRationale(),
		})
	}
	for _, j := range l.GetJudgments() {
		view.Judgments = append(view.Judgments, JudgmentView{
			HypothesisID: j.GetHypothesisId(),
			EvidenceID:   j.GetEvidenceId(),
			Direction:    judgmentDirectionWord(j.GetDirection()),
			Strength:     judgmentStrengthWord(j.GetStrength()),
			Source:       j.GetSource(),
			LnLR:         j.GetLnLr(),
		})
	}
	for _, e := range l.GetEvidence() {
		view.Evidence[e.GetEvidenceId()] = EvidenceView{
			ID: e.GetEvidenceId(), Kind: e.GetKind(), Worker: e.GetWorker(),
			Capability: e.GetCapability(), Outcome: termOutcomeWord(e.GetOutcome()),
			DeepLink: e.GetDeepLink(), DeepLinkAbsentReason: e.GetDeepLinkAbsentReason(),
			Truncated: e.GetTruncated(),
		}
	}
	sortByRank(view.Hypotheses)
	return view
}

func sortByRank(hypotheses []HypothesisView) {
	sort.SliceStable(hypotheses, func(i, j int) bool {
		if hypotheses[i].Rank != hypotheses[j].Rank {
			return hypotheses[i].Rank < hypotheses[j].Rank
		}
		return hypotheses[i].ID < hypotheses[j].ID
	})
}

func bucketWord(b *investigationv1.ConfidenceBucket) string {
	if b == nil {
		return ""
	}
	// The bucket is always rendered with its range (FR-023): a coarse label with no numbers is
	// the kind of "high confidence" that means whatever the reader wants it to mean.
	return fmt.Sprintf("%s [%.2f–%.2f]", b.GetName(), b.GetRangeLow(), b.GetRangeHigh())
}

func hypothesisKindWord(k investigationv1.HypothesisKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "HYPOTHESIS_KIND_"))
}

func hypothesisStatusWord(s investigationv1.HypothesisStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "HYPOTHESIS_STATUS_"))
}

func judgmentDirectionWord(d investigationv1.JudgmentDirection) string {
	return strings.ToLower(strings.TrimPrefix(d.String(), "JUDGMENT_DIRECTION_"))
}

func judgmentStrengthWord(s investigationv1.JudgmentStrength) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "JUDGMENT_STRENGTH_"))
}

func termOutcomeWord(o investigationv1.TermOutcome) string {
	return strings.ToLower(strings.TrimPrefix(o.String(), "TERM_OUTCOME_"))
}
