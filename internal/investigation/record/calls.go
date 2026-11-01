// SPDX-License-Identifier: Apache-2.0

package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Evidence kinds, exactly as `investigation.evidence_items.kind` accepts them.
const (
	EvidenceKindAlgebraAnswer    = "algebra_answer"
	EvidenceKindOnsetEstimate    = "onset_estimate"
	EvidenceKindKnowledgeItem    = "knowledge_item"
	EvidenceKindWorkerFailure    = "worker_failure"
	EvidenceKindInjectionAttempt = "injection_attempt"
	EvidenceKindVerifierFinding  = "verifier_finding"
)

// Worker-call outcomes, exactly as `investigation.worker_calls.outcome` accepts them. They are
// six, and never collapsed into "no" (FR-027).
const (
	CallOutcomeAnswered              = "answered"
	CallOutcomeEmpty                 = "empty"
	CallOutcomeFailed                = "failed"
	CallOutcomeTimedOut              = "timed_out"
	CallOutcomeRefusedOutsideAlgebra = "refused_outside_algebra"
	CallOutcomeNotRecorded           = "not_recorded"
)

// ExploratoryPrefix marks a call that serves no hypothesis (FR-018a). The reason follows it.
const ExploratoryPrefix = "exploratory:"

// Exploratory builds the discriminating question of a call that serves no hypothesis.
func Exploratory(reason string) string { return ExploratoryPrefix + reason }

// Evidence errors.
var (
	// ErrMissingCoverage is an evidence item with no coverage block (Invariant 4, FR-014a). It is
	// the `missing_coverage` reason code: a digest with no coverage is a claim about an unknown
	// amount of data.
	ErrMissingCoverage = errors.New("missing_coverage: an evidence item must carry a coverage block (FR-014a)")
	// ErrObservedAfterInvestigation is Invariant 7: outside review mode, an investigation may
	// hold no evidence observed later than itself.
	ErrObservedAfterInvestigation = errors.New(
		"an evidence item observed later than its investigation may not be used outside review mode (Invariant 7)")
	// ErrNoDiscriminatingQuestion is a call that says nothing about what it is meant to settle.
	ErrNoDiscriminatingQuestion = errors.New(
		"every worker call must name the discriminating question it is meant to answer; " +
			"a call serving no hypothesis is recorded as exploratory:<reason> (FR-018a)")
	// ErrMissingDeepLink is an evidence item with neither a deep link nor a reason there is none.
	ErrMissingDeepLink = errors.New(
		"an evidence item must carry a deep link or state why none exists (FR-057d)")
)

// WorkerCall is one request issued, in the order issued, with what came back (FR-033).
type WorkerCall struct {
	// CallID is the call's identifier.
	CallID string
	// Seq is its position in the investigation's single sequence space, shared with model calls.
	Seq int
	// Worker and Capability name where the question went.
	Worker     string
	Capability string
	// TermKey is sha256(canonical(term)) — the world's key.
	TermKey string
	// ServesHypothesisID is the hypothesis the call serves, empty for an exploratory call.
	ServesHypothesisID string
	// DiscriminatingQuestion is what the call is meant to settle. Required (FR-018a).
	DiscriminatingQuestion string
	// Mode is `live` or `recorded`.
	Mode string
	// Outcome is one of the six above.
	Outcome string
	// Duration is how long it took.
	Duration time.Duration
	// CostClass, Backend, RemainingQuota and QuotaShareUsed are recorded per call (FR-047a).
	CostClass      string
	Backend        string
	RemainingQuota map[string]int64
	QuotaShareUsed float64
	// ResponseBytes and Truncated measure it against the published caps.
	ResponseBytes int
	Truncated     bool
	// EvidenceID is the item it produced, where it produced one.
	EvidenceID string
}

// ModelCall is one model request, recorded with the same discipline as a worker call.
type ModelCall struct {
	// ModelCallID is the call's identifier; Seq is its place in the shared sequence space.
	ModelCallID string
	Seq         int
	// Role is `investigator`, `verifier` or `worker_logs_label`.
	Role string
	// ModelID, Effort and Betas are the recorded production configuration.
	ModelID string
	Effort  string
	Betas   []string
	// RequestDigest and ResponseDigest are sha256 of the canonical bodies.
	RequestDigest  string
	ResponseDigest string
	// The four token counts from `usage`.
	InputTokens      int
	CacheWriteTokens int
	CacheReadTokens  int
	OutputTokens     int
	// StopReason includes `refusal`, which is handled and recorded, never silently retried.
	StopReason string
	Duration   time.Duration
}

// ValidateEvidence checks the two rules a caller should hear as a sentence rather than as a
// constraint violation (FR-014a, FR-021, FR-057d).
//
// It does not check Invariant 7 — that needs the investigation's own instants, which
// CheckObservedPin takes.
func ValidateEvidence(item ledger.EvidenceItem) error {
	if item.Coverage == nil {
		return fmt.Errorf("evidence %s: %w", item.ID, ErrMissingCoverage)
	}
	if item.DeepLink == "" && item.DeepLinkAbsentReason == "" {
		return fmt.Errorf("evidence %s: %w", item.ID, ErrMissingDeepLink)
	}
	if item.ValidAt.IsZero() || item.ObservedAt.IsZero() {
		return fmt.Errorf("evidence %s: both instants in force are required (FR-021, Invariant 7)", item.ID)
	}
	return nil
}

// CheckObservedPin is Invariant 7 in Go: outside review mode, an investigation may hold no
// evidence item observed later than its own observed instant.
//
// The database trigger enforces it too. Both exist because the trigger cannot run before the
// engine has decided to use the item, and using an item it may not know is the failure — writing
// it is only where that failure becomes visible.
func CheckObservedPin(item ledger.EvidenceItem, investigationObservedAt time.Time, reviewMode bool) error {
	if reviewMode {
		return nil
	}
	if item.ObservedAt.After(investigationObservedAt) {
		return fmt.Errorf("evidence %s observed at %s, investigation at %s: %w",
			item.ID, item.ObservedAt.UTC().Format(time.RFC3339Nano),
			investigationObservedAt.UTC().Format(time.RFC3339Nano), ErrObservedAfterInvestigation)
	}
	return nil
}

// EvidenceFromResponse builds an evidence item from an algebra request and the response it got
// (FR-021). It is the one place the mapping lives, so that every worker's answers become
// comparable rows.
//
// `graphEventIDs` are the events a graph answer rests on (Invariant 11); they are empty for a
// telemetry answer, which rests on a digest rather than on the log.
func EvidenceFromResponse(
	id string,
	req *investigationv1.AlgebraRequest,
	resp *investigationv1.AlgebraResponse,
	worker, capability, sourceOfTruth string,
	calledAt time.Time,
	graphEventIDs []string,
) ledger.EvidenceItem {
	item := ledger.EvidenceItem{
		ID:             id,
		Kind:           EvidenceKindAlgebraAnswer,
		Worker:         worker,
		Capability:     capability,
		SourceOfTruth:  sourceOfTruth,
		Term:           req.GetTerm(),
		ValidAt:        req.GetValidAt().AsTime().UTC(),
		ObservedAt:     req.GetObservedAt().AsTime().UTC(),
		CalledAt:       calledAt.UTC(),
		Mode:           resp.GetMode(),
		Outcome:        TermOutcomeName(resp.GetOutcome()),
		ResponseDigest: resp.GetResponseDigest(),
		ResponseKey:    resp.GetTermKey(),
		// The coverage block lives on the digest, which is where the backend is obliged to put
		// it; an answer with no digest (a refusal, a `not_recorded`) has none, and the write
		// then fails loudly on Invariant 4 rather than being defaulted into looking complete.
		Coverage:      resp.GetDigest().GetCoverage(),
		GraphEventIDs: graphEventIDs,
		DeepLink:      resp.GetDigest().GetDeepLink(),
		FreeText:      resp.GetDigest().GetFreeText(),
	}
	if t := resp.GetDigest().GetTruncation(); t.GetTruncated() {
		item.Truncated = true
		item.TruncationNote = t.GetWhatWasDropped() + " (criterion: " + t.GetCriterion() + ")"
	}
	if item.DeepLink == "" {
		item.DeepLinkAbsentReason = "the backend returned no human link for this term; " +
			"`aisre worker call " + worker + " " + capability + "` re-runs it by hand"
	}
	return item
}

// TermOutcomeName maps the wire outcome onto the string the schema stores. The six are never
// collapsed (FR-027, Invariant 8).
func TermOutcomeName(o investigationv1.TermOutcome) string {
	switch o {
	case investigationv1.TermOutcome_DIGEST:
		return "digest"
	case investigationv1.TermOutcome_NO_DATA:
		return "no_data"
	case investigationv1.TermOutcome_NOT_YET_INGESTED:
		return "not_yet_ingested"
	case investigationv1.TermOutcome_QUERY_FAILED:
		return "query_failed"
	case investigationv1.TermOutcome_PARTIAL:
		return "partial"
	case investigationv1.TermOutcome_NOT_RECORDED:
		return "not_recorded"
	default:
		return "query_failed"
	}
}

func TermOutcomeProto(s string) investigationv1.TermOutcome {
	switch s {
	case "digest":
		return investigationv1.TermOutcome_DIGEST
	case "no_data":
		return investigationv1.TermOutcome_NO_DATA
	case "not_yet_ingested":
		return investigationv1.TermOutcome_NOT_YET_INGESTED
	case "query_failed":
		return investigationv1.TermOutcome_QUERY_FAILED
	case "partial":
		return investigationv1.TermOutcome_PARTIAL
	case "not_recorded":
		return investigationv1.TermOutcome_NOT_RECORDED
	default:
		return investigationv1.TermOutcome_TERM_OUTCOME_UNSPECIFIED
	}
}

// Proto3Clone is proto.Clone with a nil-safe signature, so the callers above read as one line.
func Proto3Clone(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	return proto.Clone(m)
}

func RefStrings(refs []*graphv1.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, graph.RefFromProto(r).String())
	}
	return out
}

func ParseRefs(raw []byte) []*graphv1.Ref {
	var values []string
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	out := make([]*graphv1.Ref, 0, len(values))
	for _, v := range values {
		ref, err := graph.ParseRef(v)
		if err != nil {
			continue
		}
		out = append(out, ref.Proto())
	}
	return out
}

// TargetRow is the stored shape of a target reference: the reference, its provenance, and — for
// a parsed one — the rule id and version that produced it, and nothing else (FR-002b, U2). An
// attribution adds the decision it came from (009 FR-021).
type TargetRow struct {
	Ref             string `json:"ref"`
	Provenance      string `json:"provenance"`
	RuleID          string `json:"rule_id,omitempty"`
	DecisionEventID string `json:"decision_event_id,omitempty"`
}

func TargetRows(targets []*investigationv1.TargetRef) []TargetRow {
	out := make([]TargetRow, 0, len(targets))
	for _, t := range targets {
		out = append(out, TargetRow{
			Ref:             graph.RefFromProto(t.GetRef()).String(),
			Provenance:      TargetProvenanceName(t.GetProvenance()),
			RuleID:          t.GetRuleId(),
			DecisionEventID: t.GetDecisionEventId(),
		})
	}
	return out
}

func ParseTargets(raw []byte) []*investigationv1.TargetRef {
	var rows []TargetRow
	if len(raw) == 0 || json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	out := make([]*investigationv1.TargetRef, 0, len(rows))
	for _, row := range rows {
		ref, err := graph.ParseRef(row.Ref)
		if err != nil {
			continue
		}
		out = append(out, &investigationv1.TargetRef{
			Ref:             ref.Proto(),
			Provenance:      TargetProvenanceProto(row.Provenance),
			RuleId:          row.RuleID,
			DecisionEventId: row.DecisionEventID,
		})
	}
	return out
}

func TargetProvenanceName(p investigationv1.TargetRefProvenance) string {
	switch p {
	case investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION:
		return "parsed_from_declaration"
	case investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN:
		return "supplied_by_human"
	case investigationv1.TargetRefProvenance_FROM_MONITOR:
		return "from_monitor"
	case investigationv1.TargetRefProvenance_ATTRIBUTION:
		return "attribution"
	default:
		return ""
	}
}

func TargetProvenanceProto(s string) investigationv1.TargetRefProvenance {
	switch s {
	case "parsed_from_declaration":
		return investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION
	case "supplied_by_human":
		return investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN
	case "from_monitor":
		return investigationv1.TargetRefProvenance_FROM_MONITOR
	case "attribution":
		return investigationv1.TargetRefProvenance_ATTRIBUTION
	default:
		return investigationv1.TargetRefProvenance_TARGET_REF_PROVENANCE_UNSPECIFIED
	}
}
