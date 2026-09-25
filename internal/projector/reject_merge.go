// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// A person deciding that two names are *not* one thing (FR-040).
//
// A rejection is the cheapest and most valuable of the four decisions. It costs one command and
// it permanently stops a rule from making a merge that would poison every diff and blast radius
// downstream (constitution VI). It is therefore the one decision the certain-rule pass consults
// on *every* claim, through pairRejected, and it was implemented before the rest of the human
// workflow for that reason.
//
// What it does not do is un-merge. A rejection says "do not merge these"; undoing a merge that
// already happened is `split_entity`, which has to decide which claims go where and so cannot be
// inferred from a refusal.

// applyRejectMerge records a human's refusal of a pair, which blocks every future automated
// merge of it and files the suggestion as rejected.
func (p *Projector) applyRejectMerge(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.RejectMerge, observedAt time.Time, principal string) error {
	a, err := p.decisionEntity(ctx, tx, body.GetEntityA())
	if err != nil {
		return err
	}
	b, err := p.decisionEntity(ctx, tx, body.GetEntityB())
	if err != nil {
		return err
	}
	lo, hi := orderedPair(a, b)
	pairKey := graph.PairKey(lo, hi)

	claims, err := p.claimIDsOf(ctx, tx, lo, hi)
	if err != nil {
		return err
	}
	if err := p.recordDecision(ctx, tx, decisionRecord{
		kind:        decisionReject,
		survivingID: lo,
		mergedID:    hi,
		rationale:   body.GetRationale(),
		claimIDs:    claims,
		principal:   principal,
		observedAt:  observedAt,
		eventID:     env.GetEventId(),
	}); err != nil {
		return err
	}
	return p.setSuggestionStatus(ctx, tx, pairKey, statusRejected,
		decisionID(decisionReject, pairKey, env.GetEventId()))
}
