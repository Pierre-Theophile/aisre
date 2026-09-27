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

// Projecting retract_node (FR-013, FR-014, edge case "retracting a node with live edges").
//
// A retraction is not a delete. It says the fact stopped being true at a valid instant, and it
// is itself an observation with its own observed start: a query asking as of an observed time
// before the retraction arrived still sees the node as true, for ever (FR-013). So the node's
// current version has its observed interval closed and a new version is inserted carrying the
// same content with a bounded valid interval — two rows where there was one, and nothing
// removed.
//
// The cascade is the part that is easy to get wrong. A node that stopped existing cannot still
// be calling anything, so its live edges are closed at the same valid instant; and because
// nobody emitted a retract_edge for them, each cascaded edge version records
// `closed_as_consequence_of` = the node version that caused it, so the graph can explain why
// an edge ended without an event of its own.

func (p *Projector) applyRetractNode(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.RetractNode, observedAt time.Time) error {
	ref := graph.RefFromProto(body.GetRef())
	entityID, found, err := p.lookupRef(ctx, tx, ref)
	if err != nil {
		return err
	}
	if !found {
		// checkResolvable refuses this before the append; reaching here means the entity
		// disappeared between the two, which cannot happen inside one transaction.
		return fmt.Errorf("projector: retract_node %s: %s does not resolve", env.GetEventId(), ref)
	}
	validEnd := body.GetValidEnd().AsTime().UTC()

	existing, err := p.currentNodeRows(ctx, tx, entityID)
	if err != nil {
		return err
	}
	assertions, err := p.nodeAssertions(ctx, tx, eventIDsOf(existing))
	if err != nil {
		return err
	}
	entity, entityFound, err := p.entity(ctx, tx, entityID)
	if err != nil || !entityFound {
		return err
	}

	planned := planRetract(segmentsOf(existing, assertions), env.GetEventId(), validEnd, "",
		assertedAtFunc(assertions), nodeContentEqual(assertions, entity.facets))
	if err := p.writeNodeSegments(ctx, tx, entityID, existing, planned, assertions, entity.facets, env.GetEventId(), observedAt); err != nil {
		return err
	}

	// The version that now ends at validEnd is the one the cascade points at: it is the
	// evidence that the node stopped being true, which is why its edges did too.
	retracting := retractingVersionID(entityID, planned, env.GetEventId(), validEnd)
	return p.cascadeEdges(ctx, tx, entityID, validEnd, retracting, env.GetEventId(), observedAt)
}

// retractingVersionID is the id of the planned version whose valid interval ends exactly at the
// retraction instant, or "" when the node had no live version reaching that far.
func retractingVersionID(entityID string, planned []segment, eventID string, validEnd time.Time) string {
	for _, seg := range planned {
		if !seg.end.IsZero() && seg.end.Equal(validEnd) {
			return graph.SegmentVersionID(entityID, eventID, seg.start)
		}
	}
	return ""
}

// cascadeEdges closes every live edge touching the entity at the same valid instant.
func (p *Projector) cascadeEdges(ctx context.Context, tx pgx.Tx, entityID string, validEnd time.Time, consequenceOf, eventID string, observedAt time.Time) error {
	keys, err := p.liveEdgeKeys(ctx, tx, entityID, validEnd)
	if err != nil {
		return err
	}
	for _, key := range keys {
		existing, err := p.currentEdgeRows(ctx, tx, key)
		if err != nil {
			return err
		}
		assertions, err := p.edgeAssertions(ctx, tx, key.typ, edgeEventIDs(existing))
		if err != nil {
			return err
		}
		existing = slices.DeleteFunc(existing, func(row *edgeRow) bool {
			return laterUnassertedVersion(row, assertions, validEnd)
		})
		if len(existing) == 0 {
			continue
		}
		planned := planRetract(edgeSegmentsOf(existing, assertions), eventID, validEnd, consequenceOf,
			edgeAssertedAtFunc(assertions), edgeContentEqual(assertions))
		if err := p.writeEdgeSegments(ctx, tx, key, existing, planned, assertions, eventID, observedAt); err != nil {
			return err
		}
	}
	return nil
}

// liveEdgeKeys lists the edge series touching an entity that are still true past validEnd, in a
// deterministic order so a replay cascades them identically (FR-023).
//
// entityID is canonical — lookupRef followed the merge chain to get here — and so is every
// current edge endpoint, because a merge absorbs the edges that named the id it merged away
// (refs.go, absorbEdges). The cascade therefore finds an entity's edges under one id and not
// several, which is what stops a merged node's edges outliving it.
func (p *Projector) liveEdgeKeys(ctx context.Context, tx pgx.Tx, entityID string, validEnd time.Time) ([]edgeKey, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT src_id, dst_id, type
		FROM graph.edge_versions
		WHERE (src_id = $1 OR dst_id = $1)
		  AND upper_inf(observed)
		  AND (upper_inf(valid) OR upper(valid) > $2)
		ORDER BY src_id, dst_id, type`, entityID, validEnd.UTC())
	if err != nil {
		return nil, fmt.Errorf("projector: read live edges of %s: %w", entityID, err)
	}
	defer rows.Close()

	var keys []edgeKey
	for rows.Next() {
		var (
			key     edgeKey
			typeStr string
		)
		if err := rows.Scan(&key.srcID, &key.dstID, &typeStr); err != nil {
			return nil, fmt.Errorf("projector: scan live edge of %s: %w", entityID, err)
		}
		key.typ = graph.EdgeType(typeStr)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read live edges of %s: %w", entityID, err)
	}
	return keys, nil
}

// laterUnassertedVersion reports an edge version the cascade must leave alone: one that begins at or
// after the retraction's end and that no edge assertion produced.
//
// A `changed_by` edge is that kind of version. linkChange writes it from an `observe_change` with the
// change's own interval, so the planner recovers no assertion for it, and planRetract drops a
// segment with no assertion at or after the end. The cascade therefore removed every change edge on
// the node that happened to be in the graph when the retraction was applied — including a change
// that began AFTER the retraction's end, such as a recreated service's first rollout. Whether that
// edge survived depended on whether the retraction or the change was applied first, and
// gcp-service-recreated-01's shuffle is what found it (003 T066).
//
// A version that begins at or after the end does not "outlive" the endpoint the cascade exists to
// protect — it is about a later time — so leaving it alone is the cascade's own rule applied to a
// fact that carries no assertion to apply it through. Versions that do carry an assertion are
// still decided by planRetract, which keeps them when they were asserted at or after the end.
func laterUnassertedVersion(row *edgeRow, assertions map[string]edgeAssertion, validEnd time.Time) bool {
	if row.valid.Start.Before(validEnd) {
		return false
	}
	for _, id := range row.producedBy {
		if _, asserted := assertions[id]; asserted {
			return false
		}
	}
	return true
}
