// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Projecting retract_edge (FR-013).
//
// The same rule as a node retraction, applied to one (source, destination, type) series: the
// current version's observed interval is closed and a version with a bounded valid interval
// replaces it. `closed_as_consequence_of` stays empty — it marks edges that ended because
// their node did, and this edge ended because a source said so.
//
// The topology feeder emits this when it has not seen a call for its configured number of
// windows, with the valid end set to the end of the last window in which it *was* seen
// (research §11) — never to "now", which would claim knowledge the feeder does not have.

func (p *Projector) applyRetractEdge(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.RetractEdge, observedAt time.Time) error {
	srcRef := graph.RefFromProto(body.GetSrc())
	dstRef := graph.RefFromProto(body.GetDst())

	srcID, found, err := p.lookupRef(ctx, tx, srcRef)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("projector: retract_edge %s: %s does not resolve", env.GetEventId(), srcRef)
	}
	dstID, found, err := p.lookupRef(ctx, tx, dstRef)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("projector: retract_edge %s: %s does not resolve", env.GetEventId(), dstRef)
	}

	key := edgeKey{srcID: srcID, dstID: dstID, typ: graph.EdgeTypeFromProto(body.GetType())}
	validEnd := body.GetValidEnd().AsTime().UTC()

	existing, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		// Nothing is true to stop being true. The event stays in the log as evidence that the
		// feeder said so; the projection has nothing to change.
		return nil
	}
	assertions, err := p.edgeAssertions(ctx, tx, edgeEventIDs(existing))
	if err != nil {
		return err
	}
	planned := planRetract(edgeSegmentsOf(existing, assertions), env.GetEventId(), validEnd, "",
		edgeAssertedAtFunc(assertions), edgeContentEqual(assertions))
	return p.writeEdgeSegments(ctx, tx, key, existing, planned, assertions, env.GetEventId(), observedAt)
}
