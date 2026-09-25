// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Projecting upsert_node (FR-005, FR-012, FR-014, FR-021).
//
// An upsert says: from this valid instant onwards, this source asserts these facts about this
// entity. What it does *not* say is anything about the instants before it, or about when the
// facts stop being true, or about what other sources assert. Everything this file does follows
// from taking that literally:
//
//   - the valid timeline is cut wherever the fold of "each source's latest assertion" changes,
//     so an event arriving late splits the version it lands in rather than overwriting it
//     (segments.go);
//   - a property carries the source and the event that asserted it, so two sources that
//     disagree produce one version holding both records with the key named in `conflicts` —
//     the projector never picks a winner (edge case "conflicting sources");
//   - nothing is updated in place: a version whose content changes has its observed interval
//     closed and a new row opened (constitution II);
//   - an upsert that says exactly what is already known writes nothing at all, so a feeder
//     re-emitting an unchanged node every window does not cut the timeline into windows.

// nodeAssertion is one upsert_node event, read back from the log.
//
// Versions are stored materialized, but the *assertions behind them* are what the segmentation
// reasons about, and the log is the only place they live. Reading them back per event costs a
// small indexed lookup and buys an important property: a segment is fully described by the set
// of events that produced it, so the projection is always recomputable from the log.
type nodeAssertion struct {
	eventID     string
	sourceID    string
	assertedAt  time.Time
	fromUnknown bool
	displayName string
	props       map[string]*structpb.Value
	pointers    []*graphv1.Pointer
	nodeType    graph.NodeType
	// ref is the identity the assertion named. It is what lets a split tell the assertions
	// about the entity being detached from the assertions about the one staying behind.
	ref graph.Ref
}

// nodeRow is one graph.entity_versions row as stored.
type nodeRow struct {
	versionID   string
	valid       postgres.TimeRange
	fromUnknown bool
	toUnknown   bool
	displayName string
	props       propSet
	conflicts   []string
	pointers    []*graphv1.Pointer
	facets      []graph.NodeType
	change      []byte
	producedBy  []string
}

// nodeContent is what a planned segment will hold.
type nodeContent struct {
	displayName string
	props       propSet
	conflicts   []string
	pointers    []*graphv1.Pointer
	facets      []graph.NodeType
	change      []byte
}

func (c nodeContent) equals(row *nodeRow) bool {
	return c.sameAs(nodeContent{
		displayName: row.displayName,
		props:       row.props,
		conflicts:   row.conflicts,
		pointers:    row.pointers,
		facets:      row.facets,
		change:      row.change,
	})
}

// sameAs compares what two versions say, ignoring which event said it. Provenance differing is
// not a change of fact (see coalesce in segments.go).
func (c nodeContent) sameAs(o nodeContent) bool {
	return c.displayName == o.displayName &&
		c.props.valuesEqual(o.props) &&
		slices.Equal(c.conflicts, o.conflicts) &&
		samePointers(c.pointers, o.pointers) &&
		slices.Equal(c.facets, o.facets) &&
		sameJSON(c.change, o.change)
}

// nodeContentEqual is the comparator the segment planner coalesces with.
func nodeContentEqual(assertions map[string]nodeAssertion, facets []graph.NodeType) func(a, b segment) bool {
	return func(a, b segment) bool {
		return materializeNode(a, assertions, facets).sameAs(materializeNode(b, assertions, facets))
	}
}

func sameJSON(a, b []byte) bool {
	normalize := func(raw []byte) string {
		if len(raw) == 0 {
			return ""
		}
		out, err := graph.CanonicalJSON(json.RawMessage(raw))
		if err != nil {
			return string(raw)
		}
		return string(out)
	}
	return normalize(a) == normalize(b)
}

// applyUpsertNode projects one upsert_node event.
func (p *Projector) applyUpsertNode(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.UpsertNode, observedAt time.Time) error {
	ref := graph.RefFromProto(body.GetRef())
	nodeType := graph.NodeTypeFromProto(body.GetType())

	entityID, err := p.resolveRef(ctx, tx, ref, nodeType, env.GetEventId(), env.GetSourceId(), observedAt)
	if err != nil {
		return err
	}
	// Record the claim the ref itself is, so the entity is findable under this name from every
	// source that uses it, and so the certain rules have something to compare (FR-036).
	if err := p.storeClaim(ctx, tx, entityID, ref, nil, env.GetSourceId(), env.GetEventId(), observedAt); err != nil {
		return err
	}
	if _, err := p.addFacet(ctx, tx, entityID, nodeType); err != nil {
		return err
	}
	entity, found, err := p.entity(ctx, tx, entityID)
	if err != nil || !found {
		return err
	}

	validAt := assertedInstant(body.GetValidAt(), env.GetSourceObservedAt(), observedAt)
	existing, err := p.currentNodeRows(ctx, tx, entityID)
	if err != nil {
		return err
	}

	assertions, err := p.nodeAssertions(ctx, tx, eventIDsOf(existing, env.GetEventId()))
	if err != nil {
		return err
	}
	assertions[env.GetEventId()] = nodeAssertion{
		eventID:     env.GetEventId(),
		sourceID:    env.GetSourceId(),
		assertedAt:  validAt,
		fromUnknown: body.GetValidFromUnknown(),
		displayName: body.GetDisplayName(),
		props:       body.GetProps().GetFields(),
		pointers:    body.GetPointers(),
		nodeType:    nodeType,
	}

	planned := planUpsert(segmentsOf(existing, assertions), env.GetSourceId(), env.GetEventId(), validAt,
		body.GetValidFromUnknown(), assertedAtFunc(assertions), nodeContentEqual(assertions, entity.facets))

	return p.writeNodeSegments(ctx, tx, entityID, existing, planned, assertions, entity.facets, env.GetEventId(), observedAt)
}

// assertedInstant is the valid lower bound an assertion takes. A source that knows a fact is
// true but not since when says so with valid_from_unknown; the row then uses the first
// observation as its physical bound and the flag says it is a placeholder, never a guess
// (FR-011, research §3).
func assertedInstant(ts, sourceObservedAt *timestamppb.Timestamp, observedAt time.Time) time.Time {
	if ts != nil {
		return ts.AsTime().UTC()
	}
	// No stated start (FR-011): the fact is dated from when the SOURCE learned it where the feeder
	// said, and from the ingest instant only otherwise (004 T154). The ingest instant is assigned as
	// events are applied, so two events in one arrival window get different ones depending on the
	// order they arrived in, and a node's valid start — a statement about the world — would then
	// depend on delivery order, which FR-021 forbids. applyObserveChange made the same correction for
	// changes; this is it for nodes and edges.
	if sourceObservedAt != nil {
		return sourceObservedAt.AsTime().UTC()
	}
	return observedAt.UTC()
}

// currentNodeRows reads the versions of an entity that are current in observed time, ordered
// by valid lower bound. "Current" means the observed interval is still open: that is the whole
// definition of "what the graph believes now" (research §3).
func (p *Projector) currentNodeRows(ctx context.Context, tx pgx.Tx, entityID string) ([]*nodeRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT version_id, valid, valid_from_unknown, valid_to_unknown, display_name,
		       props, conflicts, pointers, facets, change, produced_by_event_ids
		FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)
		ORDER BY lower(valid)`, entityID)
	if err != nil {
		return nil, fmt.Errorf("projector: read versions of %s: %w", entityID, err)
	}
	defer rows.Close()

	var out []*nodeRow
	for rows.Next() {
		var (
			row      nodeRow
			props    []byte
			pointers []byte
			facets   []string
			change   []byte
		)
		if err := rows.Scan(&row.versionID, &row.valid, &row.fromUnknown, &row.toUnknown,
			&row.displayName, &props, &row.conflicts, &pointers, &facets, &change, &row.producedBy); err != nil {
			return nil, fmt.Errorf("projector: scan version of %s: %w", entityID, err)
		}
		if err := json.Unmarshal(props, &row.props); err != nil {
			return nil, fmt.Errorf("projector: decode props of %s: %w", row.versionID, err)
		}
		decoded, err := decodePointers(pointers)
		if err != nil {
			return nil, err
		}
		row.pointers = decoded
		row.facets = facetsFromStrings(facets)
		row.change = change
		if row.conflicts == nil {
			row.conflicts = []string{}
		}
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read versions of %s: %w", entityID, err)
	}
	return out, nil
}

func decodePointers(raw []byte) ([]*graphv1.Pointer, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("projector: decode pointers: %w", err)
	}
	out := make([]*graphv1.Pointer, 0, len(items))
	for _, item := range items {
		pointer := &graphv1.Pointer{}
		if err := protojson.Unmarshal(item, pointer); err != nil {
			return nil, fmt.Errorf("projector: decode pointer: %w", err)
		}
		out = append(out, pointer)
	}
	return out, nil
}

// segmentsOf turns stored rows into the planner's view of them: which assertion each source
// contributes, which assertion each source last restated it with, and which events only shaped
// the bounds.
//
// The assertions are recovered from produced_by_event_ids rather than stored separately,
// because the array already *is* that set: a version's evidence is exactly the assertions it
// folds plus the boundary events that cut it (FR-034). A row that coalesced a restatement lists
// both events, so the earliest is the contributing one — "since when do we believe this" — and
// the latest is the restatement whose instant expandRestatements turns back into a boundary.
func segmentsOf(rows []*nodeRow, assertions map[string]nodeAssertion) []segment {
	out := make([]segment, 0, len(rows))
	for _, row := range rows {
		stored, boundary := foldAssertions(row.producedBy, func(id string) (string, time.Time, bool) {
			assertion, ok := assertions[id]
			return assertion.sourceID, assertion.assertedAt, ok
		})
		out = append(out, segment{
			versionID:    row.versionID,
			start:        row.valid.Start,
			end:          endOf(row.valid),
			fromUnknown:  row.fromUnknown,
			toUnknown:    row.toUnknown,
			assertions:   stored.assertions,
			restatements: stored.restatements,
			boundary:     boundary,
		})
	}
	return out
}

// storedAssertions is what foldAssertions recovers from one produced_by array.
type storedAssertions struct {
	assertions   map[string]string
	restatements map[string]string
}

// foldAssertions splits a produced_by array into, per source, the two earliest assertions in it
// — the contributing one and the first restatement (segments.go, rememberRestatement) — plus the
// events that asserted nothing. `of` answers "which source and which valid instant is this
// event, and is it an assertion at all?".
//
// Ties on the instant are broken by event id, so a produced_by array folds the same way whatever
// order it happens to list its events in (FR-023).
func foldAssertions(producedBy []string, of func(string) (string, time.Time, bool)) (storedAssertions, []string) {
	type bound struct {
		id string
		at time.Time
	}
	bySource := map[string][]bound{}
	var boundary []string
	for _, id := range producedBy {
		sourceID, at, ok := of(id)
		if !ok {
			boundary = append(boundary, id)
			continue
		}
		bySource[sourceID] = append(bySource[sourceID], bound{id: id, at: at})
	}

	out := storedAssertions{assertions: make(map[string]string, len(bySource))}
	for sourceID, bounds := range bySource {
		slices.SortFunc(bounds, func(a, b bound) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return cmp.Compare(a.id, b.id)
		})
		out.assertions[sourceID] = bounds[0].id
		if len(bounds) > 1 {
			out.restate(sourceID, bounds[1].id)
		}
	}
	return out, boundary
}

func (s *storedAssertions) restate(sourceID, eventID string) {
	if s.restatements == nil {
		s.restatements = map[string]string{}
	}
	s.restatements[sourceID] = eventID
}

func endOf(r postgres.TimeRange) time.Time {
	if r.EndUnbounded {
		return time.Time{}
	}
	return r.End
}

func eventIDsOf(rows []*nodeRow, extra ...string) []string {
	ids := slices.Clone(extra)
	for _, row := range rows {
		ids = append(ids, row.producedBy...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// nodeAssertions reads upsert_node events back from the log by id. Events of other types are
// skipped: a retraction listed in produced_by_event_ids shaped the interval, it did not assert
// content.
func (p *Projector) nodeAssertions(ctx context.Context, tx pgx.Tx, eventIDs []string) (map[string]nodeAssertion, error) {
	out := map[string]nodeAssertion{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	// `alert_transition` is read back here too, because it *is* a node assertion about the
	// alert: state from this instant until contradicted (alert_transition.go). Reading it as
	// anything else would make a second transition on the same monitor fold as though the
	// first had asserted nothing, and the version it produced would lose its state.
	rows, err := tx.Query(ctx, `
		SELECT event_id, source_id, coalesce(valid_at, source_observed_at, observed_at), valid_from_unknown, payload, type
		FROM log.events
		WHERE event_id = ANY($1) AND type IN ('upsert_node', 'alert_transition')`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("projector: read node assertions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			assertion nodeAssertion
			payload   []byte
			eventType string
		)
		if err := rows.Scan(&assertion.eventID, &assertion.sourceID, &assertion.assertedAt,
			&assertion.fromUnknown, &payload, &eventType); err != nil {
			return nil, fmt.Errorf("projector: scan node assertion: %w", err)
		}
		body := &graphv1.UpsertNode{}
		if eventType == "alert_transition" {
			transition := &graphv1.AlertTransition{}
			if err := protojson.Unmarshal(payload, transition); err != nil {
				return nil, fmt.Errorf("projector: decode alert transition %s: %w", assertion.eventID, err)
			}
			body = AlertNodeAssertion(transition)
		} else if err := protojson.Unmarshal(payload, body); err != nil {
			return nil, fmt.Errorf("projector: decode node assertion %s: %w", assertion.eventID, err)
		}
		assertion.assertedAt = assertion.assertedAt.UTC()
		assertion.displayName = body.GetDisplayName()
		assertion.props = body.GetProps().GetFields()
		assertion.pointers = body.GetPointers()
		assertion.nodeType = graph.NodeTypeFromProto(body.GetType())
		assertion.ref = graph.RefFromProto(body.GetRef())
		out[assertion.eventID] = assertion
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read node assertions: %w", err)
	}
	return out, nil
}

func assertedAtFunc(assertions map[string]nodeAssertion) func(string) time.Time {
	return func(eventID string) time.Time {
		return assertions[eventID].assertedAt
	}
}

// materializeNode folds the assertions a segment carries into the row it will store.
//
// Three different rules, because the three kinds of field mean different things:
//
//   - **Properties** keep one record per source, so a disagreement is preserved rather than
//     resolved (data-model.md, edge case "conflicting sources").
//   - **The display name** is a single value with no sensible union — a thing has one name at a
//     time — so it comes from the *primary* assertion: the latest by valid time, with source and
//     event id as deterministic tie-breaks, so the answer does not depend on arrival order.
//   - **Pointers are unioned across every source asserting the segment.** They are not a value
//     the sources disagree about; they are places to look, and two sources naming two different
//     backends are both right. Taking them from the primary assertion — which is what this did
//     until US5 — silently dropped half the answer whenever a workload and a service merged: the
//     Kubernetes feeder's LOG and SOURCE_LINK pointers won on valid time and the OpenTelemetry
//     feeder's METRIC and TRACE pointers vanished, so `query pointers` on a merged service could
//     not satisfy US5 scenario 1 ("at least one metric, one log and one trace selector"). A
//     pointer is data about *where to look*, and losing one loses an investigation path.
//
// The union is deduplicated by everything that identifies a pointer — kind, backend kind,
// vocabulary and selector — so two sources naming the same place say it once, and ordered by
// (kind, backend kind, selector, vocabulary) so the stored bytes are a pure function of the set
// rather than of which source was read first.
//
// Attributes are not part of the identity: two sources that agree on where to look but describe
// the entity with different attribute sets are naming one pointer, and the first by the order
// above supplies its attributes.
func materializeNode(seg segment, assertions map[string]nodeAssertion, facets []graph.NodeType) nodeContent {
	sources := sortedKeys(seg.assertions)
	props := buildProps(sources, func(sourceID string) (map[string]*structpb.Value, string, string) {
		assertion := assertions[seg.assertions[sourceID]]
		return assertion.props, sourceID, assertion.eventID
	})
	content := nodeContent{
		props:     props,
		conflicts: props.conflicts(),
		facets:    facets,
	}
	pointers := make([][]*graphv1.Pointer, 0, len(sources))
	for _, sourceID := range sources {
		pointers = append(pointers, assertions[seg.assertions[sourceID]].pointers)
	}
	content.pointers = unionPointers(pointers)

	primary := pickPrimary(sources, func(sourceID string) (time.Time, string) {
		assertion := assertions[seg.assertions[sourceID]]
		return assertion.assertedAt, assertion.eventID
	})
	if primary != "" {
		content.displayName = assertions[seg.assertions[primary]].displayName
	}
	if content.conflicts == nil {
		content.conflicts = []string{}
	}
	return content
}

// unionPointers merges every source's pointer list into one, deduplicated and ordered as
// materializeNode documents. It returns nil for an empty union, so a node nobody attached a
// pointer to stores no pointer list rather than an empty one.
func unionPointers(lists [][]*graphv1.Pointer) []*graphv1.Pointer {
	var out []*graphv1.Pointer
	seen := map[string]bool{}
	for _, list := range lists {
		for _, pointer := range list {
			if pointer == nil {
				continue
			}
			key := pointerIdentity(pointer)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, pointer)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortStableFunc(out, comparePointerOrder)
	return out
}

// pointerIdentity is everything about a pointer that decides *where to look*: two pointers with
// the same identity are the same instruction, whoever said it.
func pointerIdentity(p *graphv1.Pointer) string {
	return strconv.Itoa(int(p.GetKind())) + "\x00" + p.GetBackendKind() +
		"\x00" + p.GetVocabulary() + "\x00" + p.GetSelector()
}

// comparePointerOrder is the stored order: by kind in the enum's own order — which runs METRIC,
// LOG, TRACE, DASHBOARD, SOURCE_LINK, the order an investigation uses them in — then by backend,
// selector and vocabulary, which is total over a pointer's identity.
func comparePointerOrder(a, b *graphv1.Pointer) int {
	if c := cmp.Compare(a.GetKind(), b.GetKind()); c != 0 {
		return c
	}
	if c := cmp.Compare(a.GetBackendKind(), b.GetBackendKind()); c != 0 {
		return c
	}
	if c := cmp.Compare(a.GetSelector(), b.GetSelector()); c != 0 {
		return c
	}
	return cmp.Compare(a.GetVocabulary(), b.GetVocabulary())
}

// writeNodeSegments reconciles the planned segmentation against what is stored.
//
// A planned segment that matches a stored one exactly is left alone — that is the no-op path,
// and it is what keeps a re-delivered or repeated assertion from touching the graph. Everything
// else is a correction: the stored row's observed interval is closed and the planned row is
// inserted with an open one (FR-012). Closing happens before inserting so the exclusion
// constraint on (entity, valid, observed) never sees the two overlap.
//
// "Matches exactly" includes `produced_by_event_ids`, which is what makes a segment's first
// restatement durable: a re-assertion of unchanged content over the same interval writes one
// version — the one that records that the fact was said again, and when — and every re-assertion
// after it writes nothing, because the segment remembers only the first (segments.go,
// rememberRestatement). Leaving provenance out of the comparison was what lost the instant a
// later retraction needs; putting the *latest* restatement in it instead would write a version
// per feeder window, which is the thing coalesce exists to prevent.
func (p *Projector) writeNodeSegments(ctx context.Context, tx pgx.Tx, entityID string, existing []*nodeRow, planned []segment, assertions map[string]nodeAssertion, facets []graph.NodeType, eventID string, observedAt time.Time) error {
	type plannedRow struct {
		seg     segment
		content nodeContent
	}
	rows := make([]plannedRow, 0, len(planned))
	for _, seg := range planned {
		rows = append(rows, plannedRow{seg: seg, content: materializeNode(seg, assertions, facets)})
	}

	byRange := map[string]*nodeRow{}
	for _, row := range existing {
		byRange[rangeKey(row.valid.Start, endOf(row.valid))] = row
	}

	keep := map[string]bool{}
	var inserts []plannedRow
	for _, row := range rows {
		stored, ok := byRange[rangeKey(row.seg.start, row.seg.end)]
		if ok && stored.fromUnknown == row.seg.fromUnknown && stored.toUnknown == row.seg.toUnknown &&
			row.content.equals(stored) && slices.Equal(stored.producedBy, row.seg.producedBy()) {
			keep[stored.versionID] = true
			continue
		}
		inserts = append(inserts, row)
	}

	for _, row := range existing {
		if keep[row.versionID] {
			continue
		}
		if err := closeObserved(ctx, tx, "entity_versions", row.versionID, observedAt, eventID); err != nil {
			return err
		}
	}
	for _, row := range inserts {
		if err := p.insertNodeVersion(ctx, tx, entityID, row.seg, row.content, eventID, observedAt); err != nil {
			return err
		}
	}
	return nil
}

func rangeKey(start, end time.Time) string {
	if end.IsZero() {
		return start.UTC().Format(time.RFC3339Nano) + "/"
	}
	return start.UTC().Format(time.RFC3339Nano) + "/" + end.UTC().Format(time.RFC3339Nano)
}

func (p *Projector) insertNodeVersion(ctx context.Context, tx pgx.Tx, entityID string, seg segment, content nodeContent, eventID string, observedAt time.Time) error {
	props, err := json.Marshal(content.props)
	if err != nil {
		return fmt.Errorf("projector: encode props of %s: %w", entityID, err)
	}
	pointers, err := pointersJSON(content.pointers)
	if err != nil {
		return err
	}
	var change any
	if len(content.change) > 0 {
		change = content.change
	}
	versionID := graph.SegmentVersionID(entityID, eventID, seg.start)
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.entity_versions (
			version_id, entity_id, display_name, valid, observed, valid_from_unknown,
			valid_to_unknown, props, conflicts, pointers, change, produced_by_event_ids, facets)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		versionID, entityID, content.displayName, seg.valid(), postgres.OpenTimeRange(observedAt.UTC()),
		seg.fromUnknown, seg.toUnknown, props, content.conflicts, pointers, change,
		seg.producedBy(), facetStrings(content.facets)); err != nil {
		return fmt.Errorf("projector: insert version of %s: %w", entityID, err)
	}
	return nil
}
