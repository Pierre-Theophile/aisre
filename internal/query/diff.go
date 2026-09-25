// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Diff between two instants, with ranked change candidates (FR-027, FR-028, US2).
//
// This is the query the product thesis rests on. "Checkout was healthy at 13:00 and is failing
// at 14:32; what changed?" is answered by taking the same neighbourhood twice — once as it was
// true at T1, once as it was true at T2, both as known at one observed instant — and saying
// what is different, then listing the change nodes that fall in the window in the order they
// deserve to be looked at (rank.go).
//
// Four decisions shape the answer:
//
//   - One observed instant for both sides. A diff whose two halves were read from different
//     states of knowledge would report the arrival of a late fact as a change in production,
//     which is the exact confusion bitemporality exists to prevent (constitution II). The
//     caller may rewind knowledge with `observed_at`; both sides rewind together.
//
//   - One node universe for both sides. The two walks discover different neighbourhoods — that
//     is what "added" and "removed" mean — so the deltas are computed over the *union* of what
//     either walk reached, and the edges of both sides are re-read over that union. Without it,
//     a node that only became reachable at T2 would have its T1 edges reported as added merely
//     because the T1 walk never went there.
//
//   - Identity is the entity, not the version. Nodes are matched on entity_id and edges on
//     (src, dst, type): a new version of the same fact is a *change*, not a removal followed by
//     an addition. That is what lets scenario 2 hold — "lists no nodes as added or removed that
//     were in fact stable" — while still reporting the property that moved.
//
//   - Changes are found over a slightly wider net than the diff itself. A rollout of a service
//     one hop outside the subgraph can still be the cause; `change_hop_margin` (default 1) is
//     how far past the radius the search for change targets goes, and the extra hop is used for
//     ranking only — it never adds nodes or edges to the diff (FR-027).

// DefaultChangeHopMargin is how many hops past the subgraph radius a change's target may lie
// and still be considered (FR-027). One hop is the honest default: the thing that broke you is
// usually next to you, and a wider net mostly adds noise a ranking then has to push back down.
const DefaultChangeHopMargin = 1

// MaxChangeHopMargin bounds what a caller may ask for, for the same reason MaxHops exists.
const MaxChangeHopMargin = 4

// Diff returns what changed in a neighbourhood between two valid instants, with the change
// candidates of the window ranked by the published score (FR-027, FR-028).
func (e *Engine) Diff(ctx context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	params, err := diffParamsOf(req)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	focusRef := graph.RefFromProto(req.GetSubgraph().GetFocus())
	focusID, err := e.resolveFocus(ctx, focusRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	hood, truncation, err := e.neighbourhood(ctx, r, focusID, params)
	if err != nil {
		return nil, err
	}

	resp := &graphv1.DiffResponse{
		Truncation:     truncation,
		RankingFormula: RankingFormula(params.rank),
	}
	if err := e.nodeDiff(ctx, r, params, hood, resp); err != nil {
		return nil, err
	}
	if err := e.edgeDiff(ctx, params, hood, resp); err != nil {
		return nil, err
	}

	candidates, err := e.changeCandidates(ctx, r, params, hood)
	if err != nil {
		return nil, err
	}
	resp.Changes, resp.ExcludedChanges = Rank(candidates, params.rank)
	if err := e.attachCorrelationKeys(ctx, params.observed, resp.GetChanges()); err != nil {
		return nil, err
	}
	if resp.ChangeTargets, err = e.changeTargets(ctx, r, params, resp.GetChanges()); err != nil {
		return nil, err
	}
	return resp, nil
}

// changeTargets describes the nodes the ranked changes touched, each once, in first-mention order
// (004 T156).
//
// A ranked change names its targets by canonical id, and the target is usually the one thing in the
// window that did not change — the service the rollout landed on — so no node delta describes it.
// Each is read at t2, where the change's effect is; a target that no longer existed at t2 (a deploy
// that replaced it, a service retired inside the window) is read at t1 instead. One the graph cannot
// read at either instant is left out: the id stays on the change, and nothing is made up for it.
func (e *Engine) changeTargets(ctx context.Context, r *redirects, p diffParams, changes []*graphv1.RankedChange) ([]*graphv1.NodeVersion, error) {
	var ids []string
	seen := map[string]bool{}
	for _, c := range changes {
		for _, id := range c.GetTargetEntityIds() {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	after, err := e.loadNodeVersions(ctx, p.asOf(p.t2), r, ids)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, id := range ids {
		if _, ok := after[id]; !ok {
			missing = append(missing, id)
		}
	}
	before, err := e.loadNodeVersions(ctx, p.asOf(p.t1), r, missing)
	if err != nil {
		return nil, err
	}
	out := make([]*graphv1.NodeVersion, 0, len(ids))
	for _, id := range ids {
		if v, ok := after[id]; ok {
			out = append(out, v)
		} else if v, ok := before[id]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// attachCorrelationKeys puts on each ranked change the cross-source keys it carries — the commit it
// shipped, its immutable image, its release — as known at the observed instant (004 T128, SC-016).
//
// They are not on the change's version: a key is shared by many entities by design (T148), so it lives
// in the correlation store, which the projector re-points onto the survivor when C8 merges two
// observations of a rollout. So the canonical change entity carries the keys of every source that
// observed it, and a key two sources both stated is reported once.
//
// A zero observed instant means "as known now", the same default the rest of the read takes.
func (e *Engine) attachCorrelationKeys(ctx context.Context, observed time.Time, changes []*graphv1.RankedChange) error {
	if len(changes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(changes))
	for _, c := range changes {
		ids = append(ids, c.GetChange().GetEntityId())
	}
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT entity_id, namespace, value
		FROM graph.correlation_keys
		WHERE entity_id = ANY($1)
		  AND ($2::timestamptz IS NULL OR observed_at <= $2::timestamptz)
		ORDER BY entity_id, namespace, value`, ids, nullableInstant(observed))
	if err != nil {
		return fmt.Errorf("query: read the changes' correlation keys: %w", err)
	}
	defer rows.Close()
	keys := map[string][]*graphv1.Ref{}
	for rows.Next() {
		var entityID, namespace, value string
		if err := rows.Scan(&entityID, &namespace, &value); err != nil {
			return fmt.Errorf("query: scan a correlation key: %w", err)
		}
		keys[entityID] = append(keys[entityID], &graphv1.Ref{Namespace: namespace, Value: value})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: read the changes' correlation keys: %w", err)
	}
	for _, c := range changes {
		c.CorrelationKeys = keys[c.GetChange().GetEntityId()]
	}
	return nil
}

// nullableInstant is a timestamp for SQL, or NULL for the zero instant.
func nullableInstant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// diffParams is a validated DiffRequest: the two instants, the walk they share, how far past it
// changes are looked for, and the ranking parameters.
type diffParams struct {
	t1, t2   time.Time
	observed time.Time
	spec     sliceSpec
	margin   int
	rank     RankParams
}

// asOf returns the read of one side of the window.
func (p diffParams) asOf(validAt time.Time) AsOf {
	return AsOf{ValidAt: validAt, ObservedAt: p.observed}
}

// diffParamsOf validates the request and applies the published defaults.
func diffParamsOf(req *graphv1.DiffRequest) (diffParams, error) {
	var p diffParams
	if req.GetT1() == nil || req.GetT2() == nil {
		return p, errors.New("query: diff needs both t1 and t2; a diff is a statement about a window")
	}
	p.t1 = req.GetT1().AsTime().UTC()
	p.t2 = req.GetT2().AsTime().UTC()
	if !p.t1.Before(p.t2) {
		return p, fmt.Errorf("query: t1 (%s) must be before t2 (%s)",
			p.t1.Format(time.RFC3339Nano), p.t2.Format(time.RFC3339Nano))
	}
	if ts := req.GetObservedAt(); ts != nil {
		p.observed = ts.AsTime().UTC()
	}

	p.spec = sliceSpecOf(req.GetSubgraph())
	if p.spec.hops > MaxHops {
		return p, fmt.Errorf("query: hops = %d exceeds the maximum of %d", p.spec.hops, MaxHops)
	}

	p.margin = DefaultChangeHopMargin
	if req.ChangeHopMargin != nil {
		p.margin = int(req.GetChangeHopMargin())
	}
	if p.margin > MaxChangeHopMargin {
		return p, fmt.Errorf("query: change_hop_margin = %d exceeds the maximum of %d",
			p.margin, MaxChangeHopMargin)
	}

	reference := p.t2
	if ts := req.GetReferenceAt(); ts != nil {
		reference = ts.AsTime().UTC()
	}
	tau := DefaultTau
	if req.TauSeconds != nil {
		seconds := req.GetTauSeconds()
		if seconds <= 0 {
			return p, fmt.Errorf("query: tau_seconds = %v; the decay constant must be positive", seconds)
		}
		tau = time.Duration(seconds * float64(time.Second))
	}
	p.rank = RankParams{
		Reference: reference,
		Tau:       tau,
		HopCap:    uint32(p.spec.hops + p.margin),
	}
	return p, nil
}

// reachInfo is what the traversal knows about a node the ranking may need: how far it is from
// the focus, and the heaviest traffic class on the way there.
type reachInfo struct {
	hop int
	// weight is the maximum weight class on the heaviest shortest path from the focus
	// (research §9). Edge types that carry no traffic weight contribute 0.
	weight uint32
}

// neighbourhood is the node universe a diff is computed over, plus the wider ring changes may
// be attached to.
type neighbourhood struct {
	// union is every node either walk reached, in discovery order with the focus first.
	union   []string
	inUnion map[string]bool
	// inBefore and inAfter say which walk reached a node. A node the graph still holds but
	// that nothing in the neighbourhood points at any more has *left*, and a diff of a
	// neighbourhood has to say so.
	inBefore, inAfter map[string]bool
	// before and after are the edges of each side whose endpoints are both in the union.
	before, after []edgeRow
	// reach covers the union and the change-margin ring: hop distance and path weight, used by
	// the ranking and by nothing else.
	reach map[string]reachInfo
}

// neighbourhood walks both sides, unions them, re-reads the edges over the union and expands
// the change-margin ring.
func (e *Engine) neighbourhood(ctx context.Context, r *redirects, focusID string, p diffParams) (*neighbourhood, *graphv1.Truncation, error) {
	truncation := &graphv1.Truncation{
		PerHopCap: uint32(p.spec.perHopCap),
		TotalCap:  uint32(p.spec.totalCap),
	}
	beforeWalk, err := e.slice(ctx, p.asOf(p.t1), r, focusID, p.spec, truncation)
	if err != nil {
		return nil, nil, err
	}
	afterWalk, err := e.slice(ctx, p.asOf(p.t2), r, focusID, p.spec, truncation)
	if err != nil {
		return nil, nil, err
	}

	hood := &neighbourhood{
		inUnion:  map[string]bool{},
		inBefore: reachedSet(beforeWalk.order),
		inAfter:  reachedSet(afterWalk.order),
	}
	for _, id := range append(slices.Clone(beforeWalk.order), afterWalk.order...) {
		if hood.inUnion[id] {
			continue
		}
		hood.inUnion[id] = true
		hood.union = append(hood.union, id)
	}

	// The edges are re-read over the whole union rather than taken from the two walks: a node
	// only one walk reached must have both of its sides read, or its edges would look added.
	beforeIn, beforeOut, err := e.incidentEdges(ctx, p.asOf(p.t1), r, hood.union, hood.inUnion, p.spec)
	if err != nil {
		return nil, nil, err
	}
	afterIn, afterOut, err := e.incidentEdges(ctx, p.asOf(p.t2), r, hood.union, hood.inUnion, p.spec)
	if err != nil {
		return nil, nil, err
	}
	hood.before, hood.after = beforeIn, afterIn

	// Everything walked so far, plus the edges that leave the union, is the graph the ranking
	// measures distances over. The ring is expanded from the edges that leave, one hop per unit
	// of margin.
	adjacency := append(slices.Clone(beforeIn), beforeOut...)
	adjacency = append(adjacency, afterIn...)
	adjacency = append(adjacency, afterOut...)

	ring := map[string]bool{}
	known := func(id string) bool { return hood.inUnion[id] || ring[id] }
	frontier := outsideEndpoints(append(slices.Clone(beforeOut), afterOut...), known, ring)
	for hop := 2; hop <= p.margin && len(frontier) > 0; hop++ {
		var next []string
		for _, asOf := range []AsOf{p.asOf(p.t1), p.asOf(p.t2)} {
			_, out, err := e.incidentEdges(ctx, asOf, r, frontier, nil, p.spec)
			if err != nil {
				return nil, nil, err
			}
			adjacency = append(adjacency, out...)
			next = append(next, outsideEndpoints(out, known, ring)...)
		}
		frontier = next
	}

	hood.reach = reachability(focusID, adjacency, p.spec.direction, p.spec.hops+p.margin)
	return hood, truncation, nil
}

// reachedSet indexes the nodes one walk reached.
func reachedSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// incidentEdges reads every edge of one side incident to ids, and splits it: edges whose two
// endpoints are both inside the set, and edges that leave it.
//
// No cap applies. The per-hop cap governs how far an expansion *walks*; once the node universe
// is fixed, reporting only some of the edges between its members would make a diff that depends
// on traversal order rather than on what changed.
func (e *Engine) incidentEdges(
	ctx context.Context,
	asOf AsOf,
	r *redirects,
	ids []string,
	inside map[string]bool,
	spec sliceSpec,
) (in, out []edgeRow, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	if inside == nil {
		inside = map[string]bool{}
		for _, id := range ids {
			inside[id] = true
		}
	}
	rows, err := e.queryFrontierEdges(ctx, asOf, r, ids, spec.direction, spec.filter)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.versionID] {
			continue
		}
		seen[row.versionID] = true
		if inside[row.srcID] && inside[row.dstID] {
			in = append(in, row)
			continue
		}
		out = append(out, row)
	}
	return in, out, nil
}

// outsideEndpoints collects the endpoints of edges that leave a known set, marking them in ring
// so the next expansion does not revisit them.
func outsideEndpoints(rows []edgeRow, known func(string) bool, ring map[string]bool) []string {
	var next []string
	for _, row := range rows {
		for _, id := range [2]string{row.srcID, row.dstID} {
			if known(id) || ring[id] {
				continue
			}
			ring[id] = true
			next = append(next, id)
		}
	}
	slices.Sort(next)
	return next
}

// reachability is the breadth-first pass that gives every node its hop distance from the focus
// and the heaviest traffic class on the way (research §9).
//
// It runs over the union of both sides' edges because a change is ranked against the shape of
// the neighbourhood, not against one instant of it: an edge that existed at T1 and was retracted
// at T2 still says the two services were related while the window ran. Where an edge exists on
// both sides its T2 weight class is the one used, since the ranking is a statement about the
// state at the reference instant.
//
// The walk is level by level, so a node's path weight is final before it is used: for a node at
// level L, the weight is the maximum, over every edge reaching it from level L−1, of that edge's
// class and the weight already established at the other end. Non-`calls` edges have no class
// and count as 0, which is what makes an ownership or config path rank on hop distance alone.
func reachability(focusID string, rows []edgeRow, direction graphv1.Direction, maxHop int) map[string]reachInfo {
	// One (src, dst, type) may appear on both sides of the window; the T2 version of its weight
	// class is the one the ranking uses, and the caller appends the T1 rows first, so the row
	// that supersedes wins. directedAdjacency (impact.go) is that collapse, shared with the
	// impact query so the two can never disagree about which edges exist or what they weigh.
	adjacency := directedAdjacency(rows, direction)

	out := map[string]reachInfo{focusID: {}}
	frontier := []string{focusID}
	for hop := 1; hop <= maxHop && len(frontier) > 0; hop++ {
		next := map[string]uint32{}
		for _, from := range frontier {
			for _, edge := range adjacency[from] {
				if _, known := out[edge.to]; known {
					continue
				}
				weight := max(out[from].weight, edge.class)
				if current, ok := next[edge.to]; !ok || weight > current {
					next[edge.to] = weight
				}
			}
		}
		frontier = frontier[:0]
		for id, weight := range next {
			out[id] = reachInfo{hop: hop, weight: weight}
			frontier = append(frontier, id)
		}
		slices.Sort(frontier)
	}
	return out
}

// ---------- node deltas ----------

// nodeDiff fills in the node halves of the response.
func (e *Engine) nodeDiff(ctx context.Context, r *redirects, p diffParams, hood *neighbourhood, resp *graphv1.DiffResponse) error {
	before, err := e.loadNodeVersions(ctx, p.asOf(p.t1), r, hood.union)
	if err != nil {
		return err
	}
	after, err := e.loadNodeVersions(ctx, p.asOf(p.t2), r, hood.union)
	if err != nil {
		return err
	}

	// A node is in the answer's "before" when the T1 walk reached it *and* it had a version
	// valid then — the same two conditions Subgraph applies — so a node that existed but was
	// not connected to the focus counts as added when an edge brings it in, and a node that is
	// still in the graph but no longer reachable counts as removed.
	for _, id := range hood.union {
		was, existedBefore := before[id]
		is, existsAfter := after[id]
		existedBefore = existedBefore && hood.inBefore[id]
		existsAfter = existsAfter && hood.inAfter[id]
		switch {
		case !existedBefore && existsAfter:
			resp.NodesAdded = append(resp.NodesAdded, is)
		case existedBefore && !existsAfter:
			resp.NodesRemoved = append(resp.NodesRemoved, was)
		case existedBefore && existsAfter:
			if deltas := nodeDeltas(was, is); len(deltas) > 0 {
				resp.NodesChanged = append(resp.NodesChanged, &graphv1.NodeDelta{
					EntityId: id,
					Before:   was,
					After:    is,
					Deltas:   deltas,
				})
			}
		}
	}

	graph.SortNodeVersions(resp.NodesAdded)
	graph.SortNodeVersions(resp.NodesRemoved)
	slices.SortFunc(resp.NodesChanged, func(a, b *graphv1.NodeDelta) int {
		return strings.Compare(a.GetEntityId(), b.GetEntityId())
	})
	return nil
}

// Pseudo-keys: the two facts about a node that are not properties but change the same way, and
// that an operator reads a diff to find out about (FR-027).
const (
	// DeltaKeyDisplayName is the pseudo-key under which a renamed node is reported.
	DeltaKeyDisplayName = "display_name"
	// DeltaKeyType is the pseudo-key under which a retyped node is reported — which happens
	// when a merge resolves a workload and a service into one entity (research §10).
	DeltaKeyType = "type"
	// DeltaKeyWeightClass is the pseudo-key under which an edge's traffic class is reported.
	DeltaKeyWeightClass = "weight_class"
)

// nodeDeltas reports every key whose value differs between two versions of one node.
//
// The comparison is over the flattened property set — the values a consumer reads, with the
// per-source provenance stripped — plus the display name and the resolved type as pseudo-keys.
// A key present on one side only is reported with the other side unset, which is how "the
// property appeared" and "the property was dropped" are told apart from "the value changed".
//
// A node whose properties are identical is not reported at all, even when its version id
// changed: a new version that says the same thing is not a change to production, and reporting
// it would drown the real deltas (US2 scenario 2).
func nodeDeltas(before, after *graphv1.NodeVersion) []*graphv1.PropertyDelta {
	values := func(node *graphv1.NodeVersion) map[string]*structpb.Value {
		out := map[string]*structpb.Value{}
		for key, value := range node.GetProps().GetFields() {
			out[key] = value
		}
		// The pseudo-keys are written last so a property that happens to be called `type`
		// cannot shadow the node's actual type.
		out[DeltaKeyDisplayName] = structpb.NewStringValue(node.GetDisplayName())
		out[DeltaKeyType] = structpb.NewStringValue(graph.NodeTypeFromProto(node.GetType()).String())
		return out
	}
	return propertyDeltas(values(before), values(after))
}

// propertyDeltas compares two flattened property sets, in sorted key order so two runs of the
// same query produce the same bytes.
func propertyDeltas(before, after map[string]*structpb.Value) []*graphv1.PropertyDelta {
	keys := make([]string, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	var deltas []*graphv1.PropertyDelta
	for _, key := range keys {
		was, is := before[key], after[key]
		if proto.Equal(was, is) {
			continue
		}
		deltas = append(deltas, &graphv1.PropertyDelta{Key: key, Old: was, New: is})
	}
	return deltas
}

// ---------- edge deltas ----------

// edgeDiff fills in the edge halves of the response.
func (e *Engine) edgeDiff(ctx context.Context, p diffParams, hood *neighbourhood, resp *graphv1.DiffResponse) error {
	var eventIDs []string
	for _, row := range append(slices.Clone(hood.before), hood.after...) {
		eventIDs = append(eventIDs, row.producedBy...)
	}
	sources, err := e.eventSources(ctx, eventIDs)
	if err != nil {
		return err
	}

	before := edgesByKey(hood.before)
	after := edgesByKey(hood.after)

	keys := make([]string, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	for _, key := range keys {
		was, existedBefore := before[key]
		is, existsAfter := after[key]
		switch {
		case !existedBefore:
			resp.EdgesAdded = append(resp.EdgesAdded, is.edgeVersion(sources))
		case !existsAfter:
			resp.EdgesRemoved = append(resp.EdgesRemoved, was.edgeVersion(sources))
		default:
			deltas := edgeDeltas(was, is)
			if len(deltas) == 0 {
				continue
			}
			resp.EdgesChanged = append(resp.EdgesChanged, &graphv1.EdgeDelta{
				Key:    key,
				Before: was.edgeVersion(sources),
				After:  is.edgeVersion(sources),
				Deltas: deltas,
			})
		}
	}

	graph.SortEdgeVersions(resp.EdgesAdded)
	graph.SortEdgeVersions(resp.EdgesRemoved)
	return nil
}

// edgeDeltaKey is the identity of an edge across the window: the triple that makes two edge
// versions the same relationship, rendered as `<src>|<dst>|<type>`. It is what
// `EdgeDelta.key` carries.
func edgeDeltaKey(row edgeRow) string {
	return row.srcID + "|" + row.dstID + "|" + string(row.typ)
}

// edgesByKey indexes one side of the window by relationship, keeping one row per key.
func edgesByKey(rows []edgeRow) map[string]edgeRow {
	out := make(map[string]edgeRow, len(rows))
	for _, row := range rows {
		key := edgeDeltaKey(row)
		if current, seen := out[key]; seen && !supersedes(current, row) {
			continue
		}
		out[key] = row
	}
	return out
}

// supersedes decides which of two rows speaks for one canonical relationship at one instant.
//
// There should never be two, and the exclusion constraint on (src, dst, type, valid, observed)
// guarantees there is not — for one *stored* endpoint pair. A merge absorbs the edges that named
// the id it merged away (research §4), so as known *now* the stored pair and the canonical pair
// are the same thing and there is exactly one. Rewinding observed time is what still needs this:
// `merged_into` is not versioned, so a read as of an instant before a merge canonicalizes the
// rows that were current then — and two relationships that were separate then are one key now.
//
// The later-starting version wins, ties by version id. It is the one the sources said most
// recently, and the rule is total, so a diff reports the same thing however the rows come back.
func supersedes(current, candidate edgeRow) bool {
	if !candidate.valid.Start.Equal(current.valid.Start) {
		return candidate.valid.Start.After(current.valid.Start)
	}
	return candidate.versionID > current.versionID
}

// edgeDeltas reports the traffic class and properties that differ between two versions of one
// edge. The class is a pseudo-key, like a node's display name: it is not stored in props but it
// is exactly the kind of movement a diff is read for — traffic collapsing on a call path is the
// symptom, not the cause, and it belongs in the answer.
func edgeDeltas(before, after edgeRow) []*graphv1.PropertyDelta {
	values := func(row edgeRow) map[string]*structpb.Value {
		out := map[string]*structpb.Value{}
		if props, _ := row.props.struct_(); props != nil {
			for key, value := range props.GetFields() {
				out[key] = value
			}
		}
		if row.weightClass != nil {
			out[DeltaKeyWeightClass] = structpb.NewNumberValue(float64(*row.weightClass))
		}
		return out
	}
	return propertyDeltas(values(before), values(after))
}
