// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Subgraph as-of: the foundational read (FR-026, US1).
//
// "What did the system around `checkout` look like at 14:32?" is answered by expanding the
// neighbourhood breadth-first over as-of slices: one indexed query per hop, each restricted to
// the frontier of the previous one. Nothing is loaded that the traversal does not reach.
//
// # Direction
//
// The two directions are named from the point of view of a failing service, because that is
// the question an on-call engineer is asking:
//
//	UPSTREAM   — edges pointing INTO the focus. Its callers and dependents: who is hurt.
//	DOWNSTREAM — edges pointing OUT of the focus. Its callees and dependencies: what could
//	             be hurting it.
//
// So `checkout calls payments` makes payments DOWNSTREAM of checkout and checkout UPSTREAM of
// payments; `checkout runs_on nodepool` makes the node pool a downstream dependency of
// checkout. BOTH, the default, follows edges in either direction and is what an alert triage
// wants: everything one hop away, whichever way the arrow points.
//
// # Caps
//
// The per-hop cap is applied per frontier node and the total cap to the whole node set
// (defaults 50 and 500, research §2). A hub — a database forty services talk to — is the case
// they exist for: expanding it would drag in most of the graph and answer a question nobody
// asked. When a cap bites, the response says so, says which cap, and names the nodes whose
// expansion was cut, so the caller can re-ask with a bigger cap or a narrower filter rather
// than quietly believing a partial answer (edge case "hub explosion").
//
// # Before recorded history
//
// An as-of instant earlier than anything the graph has observed returns an empty result
// flagged `before_recorded_history`, not an error and not a truncation: the graph is not
// hiding anything, it simply was not watching yet (edge case "query before history",
// FR-052). `truncated` stays false, because nothing was cut.

// Defaults applied to a SubgraphRequest that leaves them unset (research §2, FR-026).
const (
	// DefaultHops is the neighbourhood radius: far enough to reach a dependency of a
	// dependency, close enough to stay readable.
	DefaultHops = 2
	// DefaultPerHopCap bounds the fan-out expanded from any one node.
	DefaultPerHopCap = 50
	// DefaultTotalCap bounds the nodes in the whole response.
	DefaultTotalCap = 500
	// MaxHops bounds what a caller may ask for. Beyond three hops a neighbourhood is the
	// whole graph and the caller wants Impact (FR-029), not a subgraph.
	MaxHops = 8
)

// Truncation reasons, as they appear in Truncation.reason. They are part of the published
// query contract (docs/schema/queries.md).
const (
	// ReasonPerHopCap means at least one node's fan-out was cut to the per-hop cap.
	ReasonPerHopCap = "per_hop_cap"
	// ReasonTotalCap means the expansion stopped at the total node cap.
	ReasonTotalCap = "total_cap"
	// ReasonBeforeHistory means the as-of instant precedes the graph's earliest observation.
	ReasonBeforeHistory = "before_recorded_history"
)

// Subgraph returns the neighbourhood of a focus node as of an instant (FR-026).
//
// The response is complete in itself: every node carries its intervals, type, facets, aliases,
// properties, pointers and provenance; every edge carries its intervals, type and traffic
// weight; and the extent says how much of reality the graph claims to have seen, so a consumer
// can judge the answer (FR-034, FR-052).
func (e *Engine) Subgraph(ctx context.Context, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error) {
	asOf := AsOfFromProto(req.GetAsOf())
	if err := asOf.Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	spec := sliceSpecOf(req)
	if spec.hops > MaxHops {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("query: hops = %d exceeds the maximum of %d", spec.hops, MaxHops))
	}

	focusRef := graph.RefFromProto(req.GetFocus())
	focusID, err := e.resolveFocus(ctx, focusRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	extent, err := e.log.Extent(ctx)
	if err != nil {
		return nil, err
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	truncation := &graphv1.Truncation{
		PerHopCap: uint32(spec.perHopCap),
		TotalCap:  uint32(spec.totalCap),
	}
	resp := &graphv1.SubgraphResponse{Truncation: truncation, Extent: extent}

	// Before recorded history is decided on the focus alone: if the graph has a version of the
	// focus valid at the instant, it was watching something then, whatever the log's earliest
	// observation says about the rest.
	focusVersions, err := e.loadNodeVersions(ctx, asOf, r, []string{focusID})
	if err != nil {
		return nil, err
	}
	if _, ok := focusVersions[focusID]; !ok && beforeHistory(asOf, extent) {
		truncation.Reason = ReasonBeforeHistory
		return resp, nil
	}

	walk, err := e.slice(ctx, asOf, r, focusID, spec, truncation)
	if err != nil {
		return nil, err
	}

	versions, err := e.loadNodeVersions(ctx, asOf, r, walk.order)
	if err != nil {
		return nil, err
	}

	sources, err := e.eventSources(ctx, walk.edgeEventIDs())
	if err != nil {
		return nil, err
	}
	for _, row := range walk.edges {
		if _, ok := versions[row.srcID]; !ok {
			continue
		}
		if _, ok := versions[row.dstID]; !ok {
			continue
		}
		resp.Edges = append(resp.Edges, row.edgeVersion(sources))
	}
	for _, id := range walk.order {
		if version, ok := versions[id]; ok {
			resp.Nodes = append(resp.Nodes, version)
		}
	}
	resp.Focus = versions[focusID]

	graph.SortNodeVersions(resp.Nodes)
	graph.SortEdgeVersions(resp.Edges)
	return resp, nil
}

// sliceSpec is the non-temporal half of a neighbourhood walk: how far to go, which edges to
// follow, and where to stop. Two reads of the same neighbourhood at two instants — which is
// what a diff is — differ only in their AsOf, so the shape of the walk is a value the caller
// can build once and hand to both (FR-027).
type sliceSpec struct {
	hops      int
	direction graphv1.Direction
	filter    edgeFilter
	perHopCap int
	totalCap  int
}

// sliceSpecOf resolves a subgraph request into the walk it describes, applying the published
// defaults. It is shared by Subgraph and Diff so the two cannot drift.
func sliceSpecOf(req *graphv1.SubgraphRequest) sliceSpec {
	hops, perHopCap, totalCap := subgraphLimits(req)
	return sliceSpec{
		hops:      hops,
		direction: directionOf(req),
		filter:    filterOf(req),
		perHopCap: perHopCap,
		totalCap:  totalCap,
	}
}

// slice walks the neighbourhood of focusID as of one instant and returns what it reached.
//
// It is the piece Subgraph and Diff share: one breadth-first expansion over as-of slices,
// recording the nodes in discovery order, the edges crossed, the hop each node was found at,
// and whatever a cap cut. Truncation is written into the caller's record, because a diff merges
// the truncations of its two walks into one statement about the answer.
func (e *Engine) slice(
	ctx context.Context,
	asOf AsOf,
	r *redirects,
	focusID string,
	spec sliceSpec,
	truncation *graphv1.Truncation,
) (*expansion, error) {
	walk := &expansion{
		engine:     e,
		asOf:       asOf,
		redirects:  r,
		direction:  spec.direction,
		filter:     spec.filter,
		perHopCap:  spec.perHopCap,
		totalCap:   spec.totalCap,
		truncation: truncation,
		visited:    map[string]bool{focusID: true},
		order:      []string{focusID},
		hop:        map[string]int{focusID: 0},
		edgeSeen:   map[string]bool{},
	}
	if err := walk.run(ctx, spec.hops); err != nil {
		return nil, err
	}
	return walk, nil
}

// subgraphLimits resolves the request's radius and caps against the published defaults.
func subgraphLimits(req *graphv1.SubgraphRequest) (hops, perHopCap, totalCap int) {
	hops = DefaultHops
	if req.GetHops() > 0 {
		hops = int(req.GetHops())
	}
	perHopCap = DefaultPerHopCap
	if req.PerHopCap != nil {
		perHopCap = int(req.GetPerHopCap())
	}
	totalCap = DefaultTotalCap
	if req.TotalCap != nil {
		totalCap = int(req.GetTotalCap())
	}
	return hops, perHopCap, totalCap
}

func directionOf(req *graphv1.SubgraphRequest) graphv1.Direction {
	if req.GetDirection() == graphv1.Direction_DIRECTION_UNSPECIFIED {
		return graphv1.Direction_BOTH
	}
	return req.GetDirection()
}

func filterOf(req *graphv1.SubgraphRequest) edgeFilter {
	filter := edgeFilter{minWeightClass: req.MinWeightClass}
	for _, t := range req.GetEdgeTypes() {
		if edgeType := graph.EdgeTypeFromProto(t); edgeType.Valid() {
			filter.types = append(filter.types, edgeType)
		}
	}
	return filter
}

// beforeHistory reports whether the as-of instant precedes everything the log has observed.
//
// The comparison is against the log's earliest *observed* time rather than the earliest valid
// time on purpose: the question is "was the graph watching yet?", and a fact observed at 13:01
// may perfectly well be valid from 12:00. An empty log is before history by definition.
func beforeHistory(asOf AsOf, extent *graphv1.Extent) bool {
	earliest := extent.GetEarliestObserved()
	if earliest == nil {
		return true
	}
	return asOf.ValidAt.Before(earliest.AsTime())
}

// expansion is one breadth-first walk over as-of slices.
type expansion struct {
	engine     *Engine
	asOf       AsOf
	redirects  *redirects
	direction  graphv1.Direction
	filter     edgeFilter
	perHopCap  int
	totalCap   int
	truncation *graphv1.Truncation

	visited map[string]bool
	// order is the discovery order of the nodes: focus first, then hop by hop. It is what the
	// total cap counts and what decides which nodes a cut keeps.
	order []string
	// hop is the radius each node was discovered at, the focus being 0. Breadth-first
	// discovery makes it the shortest distance over the edges the filter allows.
	hop   map[string]int
	edges []edgeRow
	// edgeSeen deduplicates across hops. One edge is incident to both of its endpoints, so a
	// walk that reaches both — `checkout -> payments` found from checkout at hop 1 and from
	// payments at hop 2 — meets the same version twice and must report it once.
	edgeSeen map[string]bool
}

// run expands the neighbourhood hop by hop, stopping at the radius or at the total cap.
func (x *expansion) run(ctx context.Context, hops int) error {
	frontier := slices.Clone(x.order)
	for depth := 1; depth <= hops; depth++ {
		if len(frontier) == 0 {
			return nil
		}
		rows, truncatedAt, err := x.engine.loadFrontierEdges(
			ctx, x.asOf, x.redirects, frontier, x.direction, x.filter, x.perHopCap)
		if err != nil {
			return err
		}
		if len(truncatedAt) > 0 {
			x.cut(ReasonPerHopCap, truncatedAt...)
		}

		var next []string
		for _, row := range rows {
			if !x.edgeSeen[row.versionID] {
				x.edgeSeen[row.versionID] = true
				x.edges = append(x.edges, row)
			}
			for _, endpoint := range [2]string{row.srcID, row.dstID} {
				if x.visited[endpoint] {
					continue
				}
				if len(x.order) >= x.totalCap {
					// The total cap is a property of the answer, not of one node, so the node
					// whose expansion hit it is what gets named.
					x.cut(ReasonTotalCap, row.otherThan(endpoint))
					return nil
				}
				x.visited[endpoint] = true
				x.order = append(x.order, endpoint)
				if x.hop != nil {
					x.hop[endpoint] = depth
				}
				next = append(next, endpoint)
			}
		}
		frontier = next
	}
	return nil
}

// cut records a truncation. The first reason recorded wins, because it is the one that shaped
// the answer; every node a cap cut at is listed whatever the reason.
func (x *expansion) cut(reason string, at ...string) {
	x.truncation.Truncated = true
	if x.truncation.Reason == "" {
		x.truncation.Reason = reason
	}
	for _, id := range at {
		if !slices.Contains(x.truncation.TruncatedAtEntityIds, id) {
			x.truncation.TruncatedAtEntityIds = append(x.truncation.TruncatedAtEntityIds, id)
		}
	}
}

// edgeEventIDs is every event behind the walked edges, for the one provenance lookup that
// resolves them to their sources (FR-034).
func (x *expansion) edgeEventIDs() []string {
	var ids []string
	for _, row := range x.edges {
		ids = append(ids, row.producedBy...)
	}
	return ids
}
