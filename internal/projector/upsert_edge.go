// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Projecting upsert_edge (FR-006, FR-021).
//
// An edge is versioned exactly like a node — same segmentation, same per-source property
// records, same corrections — with the identity being the (source, destination, type) triple
// instead of an entity id, and one extra field: the traffic weight class on `calls` edges,
// which is a coarse published ordinal and never a raw count (ADR-0001 D1, research §8).
//
// Endpoints must exist, because the version table has foreign keys to graph.entities. A
// topology feeder routinely reports `checkout → payments` before anything has described
// `payments`, and dropping the edge until the node turns up would make the graph depend on
// arrival order, which FR-021 forbids. So an unseen endpoint is created as a *placeholder*: a
// real entity row with a type inferred from the ref's namespace (refs.go), no facet, and — as
// soon as an upsert_edge is the only thing that knows about it — a version carrying the single
// property `sre.placeholder = true`. The first upsert_node for it replaces that, because an
// assertion replaces everything its source previously said about the entity.

// edgeKey identifies an edge version series.
type edgeKey struct {
	srcID string
	dstID string
	typ   graph.EdgeType
}

// edgeAssertion is one upsert_edge event read back from the log.
type edgeAssertion struct {
	eventID     string
	sourceID    string
	assertedAt  time.Time
	fromUnknown bool
	props       map[string]*structpb.Value
	weightClass *uint32
}

// edgeRow is one graph.edge_versions row as stored.
type edgeRow struct {
	versionID             string
	valid                 postgres.TimeRange
	fromUnknown           bool
	toUnknown             bool
	weightClass           *uint32
	props                 propSet
	producedBy            []string
	closedAsConsequenceOf string
}

type edgeContent struct {
	props       propSet
	weightClass *uint32
}

func (c edgeContent) equals(row *edgeRow) bool {
	return c.sameAs(edgeContent{props: row.props, weightClass: row.weightClass})
}

func (c edgeContent) sameAs(o edgeContent) bool {
	return c.props.valuesEqual(o.props) && sameWeight(c.weightClass, o.weightClass)
}

// edgeContentEqual is the comparator the segment planner coalesces with.
func edgeContentEqual(assertions map[string]edgeAssertion) func(a, b segment) bool {
	return func(a, b segment) bool {
		return materializeEdge(a, assertions).sameAs(materializeEdge(b, assertions))
	}
}

func sameWeight(a, b *uint32) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// applyUpsertEdge projects one upsert_edge event.
func (p *Projector) applyUpsertEdge(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.UpsertEdge, observedAt time.Time) error {
	srcRef := graph.RefFromProto(body.GetSrc())
	dstRef := graph.RefFromProto(body.GetDst())
	validAt := assertedInstant(body.GetValidAt(), env.GetSourceObservedAt(), observedAt)

	srcID, err := p.resolveEndpoint(ctx, tx, srcRef, env, validAt, observedAt)
	if err != nil {
		return err
	}
	dstID, err := p.resolveEndpoint(ctx, tx, dstRef, env, validAt, observedAt)
	if err != nil {
		return err
	}
	if srcID == dstID {
		// After a merge the two ends of an edge can turn out to be one entity. A self-edge
		// carries no information and the exclusion constraint would treat it as a normal
		// series, so it is dropped rather than stored.
		return nil
	}

	key := edgeKey{srcID: srcID, dstID: dstID, typ: graph.EdgeTypeFromProto(body.GetType())}

	existing, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}
	assertions, err := p.edgeAssertions(ctx, tx, key.typ, edgeEventIDs(existing, env.GetEventId()))
	if err != nil {
		return err
	}
	assertions[env.GetEventId()] = edgeAssertion{
		eventID:     env.GetEventId(),
		sourceID:    env.GetSourceId(),
		assertedAt:  validAt,
		fromUnknown: body.GetValidFromUnknown(),
		props:       body.GetProps().GetFields(),
		weightClass: body.WeightClass,
	}

	planned := planUpsert(edgeSegmentsOf(existing, assertions), env.GetSourceId(), env.GetEventId(),
		validAt, body.GetValidFromUnknown(), edgeAssertedAtFunc(assertions), edgeContentEqual(assertions))

	return p.writeEdgeSegments(ctx, tx, key, existing, planned, assertions, env.GetEventId(), observedAt)
}

// resolveEndpoint resolves one end of an edge, minting a placeholder when nothing has
// described it yet.
//
// It follows the merge chain, always: an edge is stored under canonical ids and only canonical
// ids, which is what makes the exclusion constraint's `(src, dst, type)` key mean one
// relationship. The rows written before a merge are brought into line by absorbEdges (refs.go);
// this is the other half of that guarantee, for the rows written after it.
func (p *Projector) resolveEndpoint(ctx context.Context, tx pgx.Tx, ref graph.Ref, env *graphv1.EventEnvelope, validAt, observedAt time.Time) (string, error) {
	entityID := graph.EntityID(ref.Namespace, ref.Value)
	if _, found, err := p.entity(ctx, tx, entityID); err != nil {
		return "", err
	} else if found {
		return p.follow(ctx, tx, entityID)
	}

	if err := p.createEntity(ctx, tx, entityID, ref, graph.NodeTypeUnspecified, env.GetEventId(), env.GetSourceId(), observedAt); err != nil {
		return "", err
	}
	if err := p.markPlaceholder(ctx, tx, entityID, env, validAt, observedAt); err != nil {
		return "", err
	}
	return entityID, nil
}

// markPlaceholder gives a minted endpoint a version saying only that it is a placeholder, so a
// query returns "something called this exists, nothing has described it" rather than an entity
// with no versions at all. The property is attributed to the edge's source and event, so the
// first upsert_node from that source replaces it like any other assertion.
//
// It inherits the edge's `valid_from_unknown`, and that is not cosmetic. The flag is part of a
// segment's identity: planUpsert leaves it alone when a later assertion lands on a segment that
// already starts at the same instant, so a placeholder minted with the flag clear would keep it
// clear for ever once the real node arrived. The entity would then read as "valid from exactly
// here" or "valid from at least here" according to whether an edge or its endpoint reached the
// graph first, which is the arrival-order dependence FR-021 forbids.
func (p *Projector) markPlaceholder(ctx context.Context, tx pgx.Tx, entityID string, env *graphv1.EventEnvelope, validAt, observedAt time.Time) error {
	content := nodeContent{
		props: propSet{PlaceholderProp: {{
			Value:    true,
			SourceID: env.GetSourceId(),
			EventID:  env.GetEventId(),
		}}},
		conflicts: []string{},
		facets:    []graph.NodeType{},
	}
	seg := segment{
		start:       validAt,
		fromUnknown: env.GetUpsertEdge().GetValidFromUnknown(),
		assertions:  map[string]string{env.GetSourceId(): env.GetEventId()},
	}
	return p.insertNodeVersion(ctx, tx, entityID, seg, content, env.GetEventId(), observedAt)
}

func (p *Projector) currentEdgeRows(ctx context.Context, tx pgx.Tx, key edgeKey) ([]*edgeRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT version_id, valid, valid_from_unknown, valid_to_unknown, weight_class, props,
		       produced_by_event_ids, coalesce(closed_as_consequence_of, '')
		FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = $3 AND upper_inf(observed)
		ORDER BY lower(valid)`, key.srcID, key.dstID, string(key.typ))
	if err != nil {
		return nil, fmt.Errorf("projector: read edge versions: %w", err)
	}
	defer rows.Close()

	var out []*edgeRow
	for rows.Next() {
		var (
			row   edgeRow
			props []byte
			class *int16
		)
		if err := rows.Scan(&row.versionID, &row.valid, &row.fromUnknown, &row.toUnknown,
			&class, &props, &row.producedBy, &row.closedAsConsequenceOf); err != nil {
			return nil, fmt.Errorf("projector: scan edge version: %w", err)
		}
		if class != nil {
			value := uint32(*class)
			row.weightClass = &value
		}
		if err := json.Unmarshal(props, &row.props); err != nil {
			return nil, fmt.Errorf("projector: decode edge props of %s: %w", row.versionID, err)
		}
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read edge versions: %w", err)
	}
	return out, nil
}

func edgeEventIDs(rows []*edgeRow, extra ...string) []string {
	ids := slices.Clone(extra)
	for _, row := range rows {
		ids = append(ids, row.producedBy...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (p *Projector) edgeAssertions(ctx context.Context, tx pgx.Tx, typ graph.EdgeType, eventIDs []string) (map[string]edgeAssertion, error) {
	out := map[string]edgeAssertion{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	// As in nodeAssertions: an `alert_transition` asserts its WATCHES edges, so it is read
	// back as the edge assertion it is. One transition asserts several watch edges, but they
	// differ only in their destination — same instant, no props, no weight class — so one
	// decoded assertion serves every series the event touched.
	rows, err := tx.Query(ctx, `
		SELECT event_id, source_id, coalesce(valid_at, source_observed_at, observed_at), valid_from_unknown, payload, type
		FROM log.events
		WHERE event_id = ANY($1) AND type IN ('upsert_edge', 'alert_transition')`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("projector: read edge assertions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			assertion edgeAssertion
			payload   []byte
			eventType string
		)
		if err := rows.Scan(&assertion.eventID, &assertion.sourceID, &assertion.assertedAt,
			&assertion.fromUnknown, &payload, &eventType); err != nil {
			return nil, fmt.Errorf("projector: scan edge assertion: %w", err)
		}
		body := &graphv1.UpsertEdge{}
		if eventType == "alert_transition" {
			transition := &graphv1.AlertTransition{}
			if err := protojson.Unmarshal(payload, transition); err != nil {
				return nil, fmt.Errorf("projector: decode alert transition %s: %w", assertion.eventID, err)
			}
			body = AlertWatchAssertion(transition, nil)
		} else if err := protojson.Unmarshal(payload, body); err != nil {
			return nil, fmt.Errorf("projector: decode edge assertion %s: %w", assertion.eventID, err)
		}
		// Only an assertion of THIS series' edge type is one. A version's evidence can name an event
		// that asserted a different edge: a change that attached late records the event that created
		// its target, and when that was a `runs_on` edge minting the service, reading it here made the
		// `changed_by` series look asserted by it — open-ended from that edge's instant — but only in
		// the orders where an edge minted the target first (003 T184).
		if graph.EdgeTypeFromProto(body.GetType()) != typ {
			continue
		}
		assertion.assertedAt = assertion.assertedAt.UTC()
		assertion.props = body.GetProps().GetFields()
		assertion.weightClass = body.WeightClass
		out[assertion.eventID] = assertion
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read edge assertions: %w", err)
	}
	return out, nil
}

func edgeAssertedAtFunc(assertions map[string]edgeAssertion) func(string) time.Time {
	return func(eventID string) time.Time { return assertions[eventID].assertedAt }
}

// edgeSegmentsOf is segmentsOf for an edge series: same recovery of contributing assertions and
// restatements from produced_by_event_ids, plus the cascade marker the row carries.
func edgeSegmentsOf(rows []*edgeRow, assertions map[string]edgeAssertion) []segment {
	out := make([]segment, 0, len(rows))
	for _, row := range rows {
		stored, boundary := foldAssertions(row.producedBy, func(id string) (string, time.Time, bool) {
			assertion, ok := assertions[id]
			return assertion.sourceID, assertion.assertedAt, ok
		})
		out = append(out, segment{
			versionID:             row.versionID,
			start:                 row.valid.Start,
			end:                   endOf(row.valid),
			fromUnknown:           row.fromUnknown,
			toUnknown:             row.toUnknown,
			assertions:            stored.assertions,
			restatements:          stored.restatements,
			boundary:              boundary,
			closedAsConsequenceOf: row.closedAsConsequenceOf,
		})
	}
	return out
}

func materializeEdge(seg segment, assertions map[string]edgeAssertion) edgeContent {
	sources := sortedKeys(seg.assertions)
	props := buildProps(sources, func(sourceID string) (map[string]*structpb.Value, string, string) {
		assertion := assertions[seg.assertions[sourceID]]
		return assertion.props, sourceID, assertion.eventID
	})
	content := edgeContent{props: props}
	primary := pickPrimary(sources, func(sourceID string) (time.Time, string) {
		assertion := assertions[seg.assertions[sourceID]]
		return assertion.assertedAt, assertion.eventID
	})
	if primary != "" {
		content.weightClass = assertions[seg.assertions[primary]].weightClass
	}
	return content
}

func (p *Projector) writeEdgeSegments(ctx context.Context, tx pgx.Tx, key edgeKey, existing []*edgeRow, planned []segment, assertions map[string]edgeAssertion, eventID string, observedAt time.Time) error {
	type plannedRow struct {
		seg     segment
		content edgeContent
	}
	rows := make([]plannedRow, 0, len(planned))
	for _, seg := range planned {
		rows = append(rows, plannedRow{seg: seg, content: materializeEdge(seg, assertions)})
	}

	byRange := map[string]*edgeRow{}
	for _, row := range existing {
		byRange[rangeKey(row.valid.Start, endOf(row.valid))] = row
	}

	keep := map[string]bool{}
	var inserts []plannedRow
	for _, row := range rows {
		stored, ok := byRange[rangeKey(row.seg.start, row.seg.end)]
		if ok && stored.fromUnknown == row.seg.fromUnknown && stored.toUnknown == row.seg.toUnknown &&
			stored.closedAsConsequenceOf == row.seg.closedAsConsequenceOf && row.content.equals(stored) &&
			slices.Equal(stored.producedBy, row.seg.producedBy()) {
			keep[stored.versionID] = true
			continue
		}
		inserts = append(inserts, row)
	}

	for _, row := range existing {
		if keep[row.versionID] {
			continue
		}
		if err := closeObserved(ctx, tx, "edge_versions", row.versionID, observedAt, eventID); err != nil {
			return err
		}
	}
	for _, row := range inserts {
		if err := p.insertEdgeVersion(ctx, tx, key, row.seg, row.content, eventID, observedAt); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) insertEdgeVersion(ctx context.Context, tx pgx.Tx, key edgeKey, seg segment, content edgeContent, eventID string, observedAt time.Time) error {
	props, err := json.Marshal(content.props)
	if err != nil {
		return fmt.Errorf("projector: encode edge props: %w", err)
	}
	var weight any
	if content.weightClass != nil && key.typ == graph.EdgeTypeCalls {
		weight = int16(*content.weightClass)
	}
	versionID := graph.EdgeSegmentVersionID(key.srcID, key.dstID, string(key.typ), eventID, seg.start)
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.edge_versions (
			version_id, src_id, dst_id, type, valid, observed, valid_from_unknown,
			valid_to_unknown, weight_class, props, produced_by_event_ids, closed_as_consequence_of)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		versionID, key.srcID, key.dstID, string(key.typ), seg.valid(),
		postgres.OpenTimeRange(observedAt.UTC()), seg.fromUnknown, seg.toUnknown, weight, props,
		seg.producedBy(), nullableString(seg.closedAsConsequenceOf)); err != nil {
		return fmt.Errorf("projector: insert edge version %s->%s: %w", key.srcID, key.dstID, err)
	}
	return nil
}
