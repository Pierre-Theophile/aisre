// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Evidence-item and call recording (T091, FR-018a, FR-021, FR-033, Invariants 5 and 7).
//
// The rule an evidence item exists to serve: **a claim in a report resolves to a row here, and
// that row says exactly what was asked, when, of what, and what came back**. FR-021 lists the
// fields; the schema enforces the ones it can; this file enforces the two it cannot.
//
// **Calls are never deduplicated** (Invariant 5). A repeated identical term produces a second
// `worker_calls` row and a second `evidence_items` row, counts against budget again, and is
// answered identically. That is why RecordCall takes a sequence number rather than a term key as
// its identity: the trajectory is a story in the order it happened, and "we asked twice" is part
// of the story. A DAO that upserted on the term key would quietly rewrite history into something
// tidier than what happened.
//
// **Every call names the hypothesis it serves and the discriminating question** (FR-018a). A call
// that serves none is recorded as `exploratory:<reason>`, which is still an answer — "we were
// looking around, and here is why" — rather than a blank.
//
// **An item observed later than a non-review investigation's observed instant is rejected**
// (Invariant 7). The database trigger does this too; the check here exists so the caller gets a
// sentence rather than a constraint name, and so that the rule holds for an in-memory ledger
// before anything reaches the database.

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

// EvidenceRecorder writes evidence items and the calls that produced them.
type EvidenceRecorder struct {
	dao *InvestigationDAO
}

// NewEvidenceRecorder returns a recorder over the same store as the DAO.
func NewEvidenceRecorder(dao *InvestigationDAO) *EvidenceRecorder { return &EvidenceRecorder{dao: dao} }

// Record writes one call and the evidence item it produced, in one transaction.
//
// The evidence is written first: `worker_calls.evidence_id` references it. A call that produced
// no item — a refusal, a timeout — passes a zero item and gets a row with a null reference, which
// is how "we asked and got nothing" stays visible in the trajectory.
func (r *EvidenceRecorder) Record(ctx context.Context, investigationID string, call WorkerCall, item ledger.EvidenceItem) error {
	if r == nil || r.dao == nil || r.dao.store == nil {
		return errors.New("investigation store: record call: no database")
	}
	return r.dao.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if item.ID != "" {
			if err := InsertEvidenceInTx(ctx, tx, investigationID, item); err != nil {
				return err
			}
			call.EvidenceID = item.ID
		}
		return InsertWorkerCallInTx(ctx, tx, investigationID, call)
	})
}

// InsertWorkerCallInTx writes one `worker_calls` row (FR-033, Invariant 5).
//
// The uniqueness is `(investigation_id, seq)`, never the term key: a repeated identical term is a
// second row and a second charge against budget.
func InsertWorkerCallInTx(ctx context.Context, tx pgx.Tx, investigationID string, call WorkerCall) error {
	if call.DiscriminatingQuestion == "" {
		return fmt.Errorf("investigation store: call %s: %w", call.CallID, ErrNoDiscriminatingQuestion)
	}
	quota, err := json.Marshal(call.RemainingQuota)
	if err != nil {
		return fmt.Errorf("investigation store: call %s: remaining quota: %w", call.CallID, err)
	}
	if call.RemainingQuota == nil {
		quota = nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.worker_calls (
			call_id, investigation_id, seq, worker, capability, term_key, serves_hypothesis_id,
			discriminating_question, mode, outcome, duration_ms, cost_class, backend,
			remaining_quota, quota_share_used, response_bytes, truncated, evidence_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		call.CallID, investigationID, call.Seq, call.Worker, call.Capability, call.TermKey,
		nullable(call.ServesHypothesisID), call.DiscriminatingQuestion, call.Mode, call.Outcome,
		int(call.Duration.Milliseconds()), nullable(call.CostClass), nullable(call.Backend),
		quota, call.QuotaShareUsed, call.ResponseBytes, call.Truncated, nullable(call.EvidenceID),
	); err != nil {
		return fmt.Errorf("investigation store: call %s: %w", call.CallID, err)
	}
	return nil
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

// RecordModelCall writes one `model_calls` row.
func (r *EvidenceRecorder) RecordModelCall(ctx context.Context, investigationID string, call ModelCall) error {
	if r == nil || r.dao == nil || r.dao.store == nil {
		return errors.New("investigation store: record model call: no database")
	}
	betas := call.Betas
	if betas == nil {
		betas = []string{}
	}
	_, err := r.dao.store.Pool().Exec(ctx, `
		INSERT INTO investigation.model_calls (
			model_call_id, investigation_id, seq, role, model_id, effort, betas,
			request_digest, response_digest, input_tokens, cache_write_tokens,
			cache_read_tokens, output_tokens, stop_reason, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		call.ModelCallID, investigationID, call.Seq, call.Role, call.ModelID,
		nullable(call.Effort), betas, call.RequestDigest, call.ResponseDigest,
		call.InputTokens, call.CacheWriteTokens, call.CacheReadTokens, call.OutputTokens,
		nullable(call.StopReason), int(call.Duration.Milliseconds()))
	if err != nil {
		return fmt.Errorf("investigation store: model call %s: %w", call.ModelCallID, err)
	}
	return nil
}

// NextSeq returns the next position in an investigation's single sequence space, which worker
// calls and model calls share. It is read inside the caller's transaction so that two
// concurrent turns cannot take the same number.
func NextSeq(ctx context.Context, tx pgx.Tx, investigationID string) (int, error) {
	var next int
	err := tx.QueryRow(ctx, `
		SELECT coalesce(max(seq), -1) + 1 FROM (
			SELECT seq FROM investigation.worker_calls WHERE investigation_id = $1
			UNION ALL
			SELECT seq FROM investigation.model_calls WHERE investigation_id = $1
		) AS s`, investigationID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("investigation store: next seq for %s: %w", investigationID, err)
	}
	return next, nil
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
		Outcome:        termOutcomeName(resp.GetOutcome()),
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

// termOutcomeName maps the wire outcome onto the string the schema stores. The six are never
// collapsed (FR-027, Invariant 8).
func termOutcomeName(o investigationv1.TermOutcome) string {
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

// EvidenceChain reads an investigation's evidence items in the order they were produced
// (contracts/cli.md `investigate get --chain`).
//
// It is EvidenceChainItems rendered as the published message. The published message has no
// `source_of_truth` field — widening it would be a change to a contract three features implement
// — so a caller checking FR-022 from the rows reads EvidenceChainItems, which carries it.
func (d *InvestigationDAO) EvidenceChain(ctx context.Context, investigationID string) ([]*investigationv1.EvidenceItem, error) {
	items, err := d.EvidenceChainItems(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	out := make([]*investigationv1.EvidenceItem, 0, len(items))
	for _, item := range items {
		out = append(out, EvidenceProto(item))
	}
	return out, nil
}

// EvidenceChainItems reads the evidence chain as the ledger's own type, in call order.
//
// It carries `source_of_truth` as the answering worker declared it at the time (FR-022, 0008),
// which is the whole reason it exists beside EvidenceChain: "may this item carry a hypothesis to
// `supported`?" is answerable from the rows, without a worker registry that may since have been
// renamed or retired.
func (d *InvestigationDAO) EvidenceChainItems(ctx context.Context, investigationID string) ([]ledger.EvidenceItem, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: evidence chain: no database")
	}
	rows, err := d.store.Pool().Query(ctx, `
		SELECT evidence_id, kind, worker, capability, source_of_truth, term, valid_at, observed_at,
		       called_at, mode, outcome, response_digest, response_key, coverage, join_keys,
		       graph_event_ids, coalesce(deep_link, ''), coalesce(deep_link_absent_reason, ''),
		       truncated, coalesce(truncation_note, ''), coalesce(free_text, '')
		FROM investigation.evidence_items WHERE investigation_id = $1
		ORDER BY called_at, evidence_id`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: evidence chain of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []ledger.EvidenceItem
	for rows.Next() {
		var (
			e                             ledger.EvidenceItem
			termRaw, coverageRaw, joinRaw []byte
		)
		if err := rows.Scan(&e.ID, &e.Kind, &e.Worker, &e.Capability, &e.SourceOfTruth, &termRaw,
			&e.ValidAt, &e.ObservedAt, &e.CalledAt, &e.Mode, &e.Outcome, &e.ResponseDigest,
			&e.ResponseKey, &coverageRaw, &joinRaw, &e.GraphEventIDs, &e.DeepLink,
			&e.DeepLinkAbsentReason, &e.Truncated, &e.TruncationNote, &e.FreeText); err != nil {
			return nil, fmt.Errorf("investigation store: scan evidence: %w", err)
		}
		if term, err := unmarshalTerm(termRaw); err == nil {
			e.Term = term
		}
		var coverage investigationv1.Coverage
		if unmarshalMessage(coverageRaw, &coverage) == nil {
			e.Coverage = &coverage
		}
		var joins investigationv1.JoinKeys
		if unmarshalMessage(joinRaw, &joins) == nil {
			e.JoinKeys = &joins
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func termOutcomeProto(s string) investigationv1.TermOutcome {
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

// --- small shared helpers -----------------------------------------------------------------

// proto3Clone is proto.Clone with a nil-safe signature, so the callers above read as one line.
func proto3Clone(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	return proto.Clone(m)
}

func refStrings(refs []*graphv1.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, graph.RefFromProto(r).String())
	}
	return out
}

func parseRefs(raw []byte) []*graphv1.Ref {
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

// targetRow is the stored shape of a target reference: the reference, its provenance, and — for
// a parsed one — the rule id and version that produced it, and nothing else (FR-002b, U2).
type targetRow struct {
	Ref        string `json:"ref"`
	Provenance string `json:"provenance"`
	RuleID     string `json:"rule_id,omitempty"`
}

func targetRows(targets []*investigationv1.TargetRef) []targetRow {
	out := make([]targetRow, 0, len(targets))
	for _, t := range targets {
		out = append(out, targetRow{
			Ref:        graph.RefFromProto(t.GetRef()).String(),
			Provenance: targetProvenanceName(t.GetProvenance()),
			RuleID:     t.GetRuleId(),
		})
	}
	return out
}

func parseTargets(raw []byte) []*investigationv1.TargetRef {
	var rows []targetRow
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
			Ref:        ref.Proto(),
			Provenance: targetProvenanceProto(row.Provenance),
			RuleId:     row.RuleID,
		})
	}
	return out
}

func targetProvenanceName(p investigationv1.TargetRefProvenance) string {
	switch p {
	case investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION:
		return "parsed_from_declaration"
	case investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN:
		return "supplied_by_human"
	case investigationv1.TargetRefProvenance_FROM_MONITOR:
		return "from_monitor"
	default:
		return ""
	}
}

func targetProvenanceProto(s string) investigationv1.TargetRefProvenance {
	switch s {
	case "parsed_from_declaration":
		return investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION
	case "supplied_by_human":
		return investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN
	case "from_monitor":
		return investigationv1.TargetRefProvenance_FROM_MONITOR
	default:
		return investigationv1.TargetRefProvenance_TARGET_REF_PROVENANCE_UNSPECIFIED
	}
}
