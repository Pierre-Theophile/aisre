// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// A person deciding that two names are one thing (FR-038, FR-039, FR-040, FR-041).
//
// A human decision is an ordinary event in the log. That is the whole design: it is appended
// like any feeder event, it is idempotent like any feeder event, and a replay from empty
// projects it again in log order and reaches the same graph (SC-007). Nothing about a merge a
// person made is stored outside the log, so nothing about it can be lost in a rebuild.
//
// It differs from a feeder event in exactly two ways:
//
//   - it carries a principal, and it is refused outright without one (FR-041). An anonymous
//     decision is not a weaker decision, it is not a decision at all: constitution VI requires a
//     human override to be attributable, and a row whose `principal` is null could not be
//     defended in the postmortem it exists for;
//   - it outranks every rule, for ever. The merge it makes is pinned: a later rule that would
//     merge either side into a third entity is recorded as a conflict instead (suggest.go).
//
// `confirm_merge` and `manual_merge` are the same operation with different provenance — one
// accepts a suggestion the graph made, the other is a merge a person thought of themselves —
// so they share this code and differ only in the decision kind recorded, which is what makes
// "how many of our merges did the graph propose?" answerable from the decisions table.

// HumanScore is the confidence recorded for a human decision. A person is not 0.9 sure; they
// have decided, and the score exists so that a downstream consumer reading decisions uniformly
// does not have to special-case the human ones (FR-038).
const HumanScore = 1.0

// applyConfirmMerge records a person accepting a suggested merge.
func (p *Projector) applyConfirmMerge(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.ConfirmMerge, observedAt time.Time, principal string) error {
	return p.applyHumanMerge(ctx, tx, env, decisionConfirm,
		body.GetEntityA(), body.GetEntityB(), body.GetRationale(), observedAt, principal)
}

// applyManualMerge records a merge a person asked for without a suggestion behind it.
func (p *Projector) applyManualMerge(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.ManualMerge, observedAt time.Time, principal string) error {
	return p.applyHumanMerge(ctx, tx, env, decisionManualMerge,
		body.GetEntityA(), body.GetEntityB(), body.GetRationale(), observedAt, principal)
}

// applyHumanMerge is the body both share.
func (p *Projector) applyHumanMerge(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, kind, refA, refB, rationale string, observedAt time.Time, principal string) error {
	a, err := p.decisionEntity(ctx, tx, refA)
	if err != nil {
		return err
	}
	b, err := p.decisionEntity(ctx, tx, refB)
	if err != nil {
		return err
	}
	if rationale == "" {
		rationale = fmt.Sprintf("%s by %s", kind, principal)
	}

	claims, err := p.claimIDsOf(ctx, tx, a, b)
	if err != nil {
		return err
	}
	match := resolution.Match{
		Certain:            true,
		Score:              HumanScore,
		Rationale:          rationale,
		EntityA:            a,
		EntityB:            b,
		SupportingClaimIDs: claims,
	}

	if a == b {
		// The rules got there first, or the person confirmed twice. There is nothing left to
		// merge, but the decision still matters: it is what pins the entity against a later
		// rule changing its mind (FR-040), and it is what the audit query shows as the
		// deciding evidence (US6 scenario 2).
		pairKey := graph.PairKey(a, b)
		if err := p.recordDecision(ctx, tx, decisionRecord{
			kind: kind, survivingID: a, mergedID: b, score: HumanScore,
			rationale: rationale, claimIDs: claims, principal: principal,
			observedAt: observedAt, eventID: env.GetEventId(),
		}); err != nil {
			return err
		}
		return p.setSuggestionStatus(ctx, tx, pairKey, statusConfirmed,
			decisionID(kind, pairKey, env.GetEventId()))
	}

	// A human decision outranks a rejection of the same pair by the same authority: a person
	// changing their mind is exactly what FR-040's "reversible" means, so the earlier rejection
	// is superseded rather than treated as a veto.
	if err := p.supersedeDecisions(ctx, tx, []string{decisionReject}, a, b,
		decisionID(kind, graph.PairKey(a, b), env.GetEventId())); err != nil {
		return err
	}

	merged, err := p.merge(ctx, tx, match, a, b, env.GetEventId(), observedAt, mergeOptions{
		kind:      kind,
		principal: principal,
	})
	if err != nil {
		return err
	}
	if merged == "" {
		return nil
	}
	survivor, err := p.follow(ctx, tx, merged)
	if err != nil {
		return err
	}
	// Nodes and edges are folded exactly as a rule-driven merge folds them: a decision a person
	// made must leave the same graph a certain rule would have left, or the two paths would
	// diverge and a replay could not be compared with a live run (FR-023, research §4).
	if err := p.absorb(ctx, tx, survivor, []string{merged}, env.GetEventId(), observedAt); err != nil {
		return err
	}
	return p.absorbEdges(ctx, tx, []string{merged}, env.GetEventId(), observedAt)
}

// supersedeDecisions marks earlier decisions about a pair as overtaken by a new one, so that
// `superseded_by IS NULL` keeps meaning "still in force" (data-model.md
// §graph.resolution_decisions). History is not rewritten: the old row stays, with a pointer to
// what replaced it.
func (p *Projector) supersedeDecisions(ctx context.Context, tx pgx.Tx, kinds []string, a, b, byDecisionID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.resolution_decisions SET superseded_by = $4
		WHERE kind = ANY($3) AND superseded_by IS NULL
		  AND ((surviving_id = $1 AND merged_id = $2) OR (surviving_id = $2 AND merged_id = $1))`,
		a, b, kinds, byDecisionID); err != nil {
		return fmt.Errorf("projector: supersede %v decisions on %s/%s: %w", kinds, a, b, err)
	}
	return nil
}

// claimIDsOf lists the claims currently pointing at any of the given entities, which is the
// evidence a decision stood on (FR-038).
func (p *Projector) claimIDsOf(ctx context.Context, tx pgx.Tx, entityIDs ...string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT claim_id FROM graph.identity_claims WHERE entity_id = ANY($1) ORDER BY claim_id`,
		slices.Compact(slices.Sorted(slices.Values(entityIDs))))
	if err != nil {
		return nil, fmt.Errorf("projector: read claims of %v: %w", entityIDs, err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("projector: scan claim id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read claims of %v: %w", entityIDs, err)
	}
	return ids, nil
}

// idNamespacePrefix addresses an entity by its canonical id rather than by a source's name for
// it, `id:<entity_id>` (contracts/cli.md). The query layer publishes the same pseudo-namespace;
// it is spelled out here rather than imported because the query layer reads the projection and
// importing it back would be a cycle.
const idNamespacePrefix = "id:"

// ErrDecisionRefUnresolvable is returned when a human decision names something the graph has
// never seen. It never escapes applyInTx: the event is refused before it is appended.
var ErrDecisionRefUnresolvable = errors.New("projector: the decision names an entity the graph has never observed")

// decisionEntity resolves one side of a human decision to a canonical entity id.
//
// A decision may name an entity three ways, which are the three spellings contracts/cli.md
// publishes for a reference:
//
//	otel.service.name=checkout   a name a source used, resolved through claims and merges
//	id:JBSWY3DP...               the canonical id directly
//	JBSWY3DP...                  a bare canonical id, which is what an API client that read one
//	                             out of a previous answer will send back
//
// Naming an entity by a *name* rather than by an id is what makes a human decision replayable
// into a freshly built graph: canonical ids depend on which claim the log saw first (research
// §4), so a fixture that pinned them would break the moment its events were re-recorded.
func (p *Projector) decisionEntity(ctx context.Context, tx pgx.Tx, raw string) (string, error) {
	id, found, err := p.lookupDecisionRef(ctx, tx, raw)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %q", ErrDecisionRefUnresolvable, raw)
	}
	return id, nil
}

// lookupDecisionRef resolves a decision reference without failing when it is unknown, so that
// the pre-append check can turn "unknown" into a rejected event rather than an error.
func (p *Projector) lookupDecisionRef(ctx context.Context, tx pgx.Tx, raw string) (string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, nil
	}
	if rest, ok := strings.CutPrefix(trimmed, idNamespacePrefix); ok {
		return p.entityByID(ctx, tx, rest)
	}
	if ref, err := graph.ParseRef(trimmed); err == nil {
		return p.lookupRef(ctx, tx, ref)
	}
	return p.entityByID(ctx, tx, trimmed)
}

func (p *Projector) entityByID(ctx context.Context, tx pgx.Tx, entityID string) (string, bool, error) {
	if _, found, err := p.entity(ctx, tx, entityID); err != nil || !found {
		return "", false, err
	}
	id, err := p.follow(ctx, tx, entityID)
	return id, err == nil, err
}
