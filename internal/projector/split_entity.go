// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Undoing a merge (FR-039, US6 scenario 4, data-model.md §State transitions).
//
// A merge must be reversible and must keep its full history, and those two requirements decide
// almost everything about this file.
//
// *Reversible* is why the split does not simply delete a redirect. The entity that was merged
// away still exists, still owns its old version rows, and a read rewound to an observed instant
// before the split must still find the merged view — that is US6 scenario 4 in one sentence.
// So nothing is deleted and no row is rewritten; the split closes observed intervals and opens
// new ones, exactly as a correction does (constitution II).
//
// *Keeps its history* is why the detached claims move to a **new** entity rather than back to
// the one they came from. Resurrecting the merged-away id would give one id two lives with a gap
// in the middle, and every query that resolved through it would have to know which life it meant.
// Instead the old id stays merged away and its redirect is re-pointed at the new entity, so every
// name it was ever known by still resolves — to the entity the person says owns it now.
//
// What the two entities then look like is re-derived, not patched. The assertions behind the
// merged entity's current versions are partitioned by the identity they named: the ones naming a
// detached identifier rebuild the new entity, the rest rebuild the original. Re-deriving rather
// than subtracting is what keeps the result a pure function of the assertion set, which is the
// same property that makes a merge order-independent (research §5).
//
// Known limitation, documented rather than hidden (docs/schema/resolution.md): edges are not
// re-pointed by a split. A relationship that was asserted against the detached identifier stays
// on the original entity until its source asserts it again, at which point it lands on the new
// one through ordinary segmentation. Re-deriving edges would need the same partition applied to
// the edge fold, and no fixture reaches it yet; when one does, it belongs here.

// applySplitEntity detaches claims from an entity into a new one (FR-039).
func (p *Projector) applySplitEntity(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.SplitEntity, observedAt time.Time, principal string) error {
	original, err := p.decisionEntity(ctx, tx, body.GetEntityId())
	if err != nil {
		return err
	}
	detachRefs := make([]graph.Ref, 0, len(body.GetDetachClaims()))
	for _, ref := range body.GetDetachClaims() {
		detachRefs = append(detachRefs, graph.RefFromProto(ref))
	}
	if len(detachRefs) == 0 {
		return fmt.Errorf("projector: split of %s names no claims to detach; there would be nothing to split off",
			body.GetEntityId())
	}

	created, err := p.splitEntityID(ctx, tx, detachRefs[0], env.GetEventId())
	if err != nil {
		return err
	}

	// The claims move first, so that everything below reads the identity the person decided on
	// rather than the one the rules had settled on.
	if err := p.detachClaims(ctx, tx, original, created, detachRefs, env, observedAt); err != nil {
		return err
	}
	if err := p.rebuildAfterSplit(ctx, tx, original, created, detachRefs, env, observedAt); err != nil {
		return err
	}

	rationale := body.GetRationale()
	if rationale == "" {
		rationale = fmt.Sprintf("split by %s", principal)
	}
	claims, err := p.claimIDsOf(ctx, tx, original, created)
	if err != nil {
		return err
	}
	pairKey := graph.PairKey(original, created)
	if err := p.recordDecision(ctx, tx, decisionRecord{
		kind:        decisionSplit,
		survivingID: original,
		mergedID:    created,
		score:       HumanScore,
		rationale:   rationale,
		claimIDs:    claims,
		principal:   principal,
		observedAt:  observedAt,
		eventID:     env.GetEventId(),
	}); err != nil {
		return err
	}

	// A split is the only thing that lifts a pin: the person who merged these has now said they
	// are not one entity, so the merge decisions that were freezing them are superseded and the
	// rules are free to speak about them again (suggest.go, FR-040).
	splitID := decisionID(decisionSplit, pairKey, env.GetEventId())
	if err := p.supersedePinsOn(ctx, tx, original, splitID); err != nil {
		return err
	}
	return p.setSuggestionStatus(ctx, tx, pairKey, statusRejected, splitID)
}

// splitEntityID mints the id the detached claims will live under: the id their first identifier
// hashes to, or a decision-derived one when that id is already taken (graph.SplitEntityID).
func (p *Projector) splitEntityID(ctx context.Context, tx pgx.Tx, first graph.Ref, eventID string) (string, error) {
	preferred := graph.EntityID(first.Namespace, first.Value)
	_, taken, err := p.entity(ctx, tx, preferred)
	if err != nil {
		return "", err
	}
	if !taken {
		return preferred, nil
	}
	return graph.SplitEntityID(preferred, eventID), nil
}

// detachClaims creates the new entity, moves the named claims onto it, and re-points the
// redirect of every entity those claims were minted from.
//
// Re-pointing the redirect is what makes `query subgraph k8s.deployment=shop/checkout-svc`
// answer with the new entity after the split and with the merged one before it: the old id is
// still a name for something, and after the split that something is the detached entity.
func (p *Projector) detachClaims(ctx context.Context, tx pgx.Tx, original, created string, refs []graph.Ref, env *graphv1.EventEnvelope, observedAt time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.entities (entity_id, type, facets, created_by_event_id)
		VALUES ($1, $2, '{}'::text[], $3)
		ON CONFLICT (entity_id) DO NOTHING`,
		created, string(namespaceDefaultType(refs[0].Namespace)), env.GetEventId()); err != nil {
		return fmt.Errorf("projector: create split entity %s: %w", created, err)
	}

	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `
			UPDATE graph.identity_claims SET entity_id = $3
			WHERE namespace = $1 AND value = $2 AND entity_id = $4`,
			ref.Namespace, ref.Value, created, original); err != nil {
			return fmt.Errorf("projector: detach claim %s: %w", ref, err)
		}

		minted := graph.EntityID(ref.Namespace, ref.Value)
		if minted == created {
			continue
		}
		row, found, err := p.entity(ctx, tx, minted)
		if err != nil {
			return err
		}
		if !found || row.mergedInto == "" {
			continue
		}
		target, err := p.follow(ctx, tx, minted)
		if err != nil {
			return err
		}
		if target != original {
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE graph.entities SET merged_into = $2 WHERE entity_id = $1`, minted, created); err != nil {
			return fmt.Errorf("projector: re-point redirect of %s to %s: %w", minted, created, err)
		}
	}

	// A change that named one of the detached identifiers and has been waiting for it can now
	// attach to the entity that actually owns it.
	for _, ref := range refs {
		if err := p.attachWaiting(ctx, tx, created, ref, env.GetEventId(), observedAt); err != nil {
			return err
		}
	}
	return nil
}

// rebuildAfterSplit re-derives both entities' versions from the assertions behind the original's
// current ones, partitioned by the identity each assertion named.
func (p *Projector) rebuildAfterSplit(ctx context.Context, tx pgx.Tx, original, created string, refs []graph.Ref, env *graphv1.EventEnvelope, observedAt time.Time) error {
	rows, err := p.currentNodeRows(ctx, tx, original)
	if err != nil {
		return err
	}
	assertions, err := p.nodeAssertions(ctx, tx, eventIDsOf(rows))
	if err != nil {
		return err
	}

	detachedRefs := make([]string, 0, len(refs))
	for _, ref := range refs {
		detachedRefs = append(detachedRefs, refString(ref))
	}

	var kept, detached []nodeAssertion
	for _, eventID := range sortedKeys(assertions) {
		assertion := assertions[eventID]
		if slices.Contains(detachedRefs, refString(assertion.ref)) {
			detached = append(detached, assertion)
			continue
		}
		kept = append(kept, assertion)
	}

	if err := p.writeSplitSide(ctx, tx, original, rows, kept, assertions, env, observedAt); err != nil {
		return err
	}
	if len(detached) == 0 {
		// Nothing described the detached identity in its own right — it was a bare claim. The
		// new entity gets the same placeholder version a minted edge endpoint gets, so a query
		// answers "something called this exists and nothing has described it" rather than
		// returning an entity with no versions at all.
		return p.markPlaceholder(ctx, tx, created, env, observedAt, observedAt)
	}
	return p.writeSplitSide(ctx, tx, created, nil, detached, assertions, env, observedAt)
}

// writeSplitSide rebuilds one side of a split from the assertions it keeps.
func (p *Projector) writeSplitSide(ctx context.Context, tx pgx.Tx, entityID string, existing []*nodeRow, items []nodeAssertion, assertions map[string]nodeAssertion, env *graphv1.EventEnvelope, observedAt time.Time) error {
	slices.SortFunc(items, func(a, b nodeAssertion) int {
		return cmpAssertion(a.assertedAt, a.sourceID, a.eventID, b.assertedAt, b.sourceID, b.eventID)
	})

	facets := make([]graph.NodeType, 0, len(items))
	for _, item := range items {
		if item.nodeType != graph.NodeTypeUnspecified {
			facets = append(facets, item.nodeType)
		}
	}
	facets = orderFacets(facets)
	if err := p.setFacets(ctx, tx, entityID, facets); err != nil {
		return err
	}

	var planned []segment
	for _, item := range items {
		planned = planUpsert(planned, item.sourceID, item.eventID, item.assertedAt,
			item.fromUnknown, assertedAtFunc(assertions), nodeContentEqual(assertions, facets))
	}
	for i := range planned {
		// The split is why these rows have the shape they have, so it belongs to their
		// evidence (FR-034).
		planned[i].boundary = append(slices.Clone(planned[i].boundary), env.GetEventId())
	}
	return p.writeNodeSegments(ctx, tx, entityID, existing, planned, assertions, facets,
		env.GetEventId(), observedAt)
}

// supersedePinsOn marks every confirm and manual_merge decision naming an entity as overtaken by
// the split, which is what lifts the pin those decisions put on it.
func (p *Projector) supersedePinsOn(ctx context.Context, tx pgx.Tx, entityID, byDecisionID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.resolution_decisions SET superseded_by = $2
		WHERE kind IN ('confirm', 'manual_merge') AND superseded_by IS NULL
		  AND (surviving_id = $1 OR merged_id = $1)`, entityID, byDecisionID); err != nil {
		return fmt.Errorf("projector: supersede pins on %s: %w", entityID, err)
	}
	return nil
}
