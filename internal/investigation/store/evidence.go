// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Pierre-Theophile/aisre/internal/investigation/record"

	"github.com/jackc/pgx/v5"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
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
func (r *EvidenceRecorder) Record(ctx context.Context, investigationID string, call record.WorkerCall, item ledger.EvidenceItem) error {
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
func InsertWorkerCallInTx(ctx context.Context, tx pgx.Tx, investigationID string, call record.WorkerCall) error {
	if call.DiscriminatingQuestion == "" {
		return fmt.Errorf("investigation store: call %s: %w", call.CallID, record.ErrNoDiscriminatingQuestion)
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

// RecordModelCall writes one `model_calls` row.
func (r *EvidenceRecorder) RecordModelCall(ctx context.Context, investigationID string, call record.ModelCall) error {
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
		out = append(out, record.EvidenceProto(item))
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

// --- small shared helpers -----------------------------------------------------------------
