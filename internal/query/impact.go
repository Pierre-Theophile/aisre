// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Blast radius: who breaks if this breaks, and what could be breaking it (FR-029, US4).
//
// Impact is the subgraph read asked twice and answered as two ordered lists rather than as a
// picture. It is the query that decides who gets paged, so the two lists are named from the
// point of view of the thing that is failing — and that naming is, deliberately, the *opposite*
// of the one `direction` uses on a subgraph request:
//
//	downstream — entities that DEPEND ON the focus. Reached by following edges that point
//	             INTO it, transitively. These are the callers and dependents: who breaks if
//	             the focus breaks. In subgraph terms this walk is DIRECTION UPSTREAM.
//	upstream   — entities the focus DEPENDS ON. Reached by following edges that point OUT of
//	             it, transitively. These are the callees and dependencies: what could be
//	             breaking the focus. In subgraph terms this walk is DIRECTION DOWNSTREAM.
//
// The inversion is not an accident and it is not a bug. `direction` names the arrows a
// traversal follows; `downstream`/`upstream` name the flow of *consequence* through them, which
// runs the other way (US4 scenario 1: "checkout appears as a downstream dependent of payments"
// while `checkout —calls→ payments` makes checkout UPSTREAM of payments in subgraph terms). The
// mapping is stated in docs/schema/queries.md and is part of the published contract.
//
// # The heaviest path
//
// An item is reported once, at its shortest hop distance, with one path shown — and the path
// shown is not the shortest one. It is the **heaviest**: the path whose *weakest* link carries
// the most traffic, because a blast radius is decided by the narrowest part of the route the
// failure travels along, not by the loudest. Formally the heaviest path maximizes the minimum
// weight class along it; ties go to the path with fewer hops, then to the lexicographically
// smallest sequence of entity ids, so two runs over the same graph name the same path.
//
// Edges that carry no traffic weight — everything but `calls` — count as class 0 on a path
// (research §9). An ownership or configuration route therefore has weight 0, every such item
// ties on weight, and the ordering falls back to hop distance, which is exactly what research
// §9 says impact should do.
//
// `hop_distance` and `heaviest_path` are independent answers to two different questions and may
// disagree: a service reachable directly over a quiet edge and indirectly over two loud ones has
// hop distance 1 and a two-hop heaviest path. That is the shape US4 scenario 2 asks to see.
//
// # Alternative paths
//
// `alternative_paths` counts the distinct simple paths from the focus to the item, up to
// `max_hops`, *other than* the one shown — 0 means "there is only this way in". Enumeration
// stops at MaxAlternativePaths, so the value saturates rather than growing without bound, and a
// reported 100 reads as "100 or more".

// Defaults and bounds applied to an ImpactRequest (FR-029, research §2).
const (
	// DefaultImpactHops is how far a blast radius reaches when the caller does not say. Three
	// hops is one more than a subgraph's default: impact is asked when the question has
	// already stopped being "what is next to me".
	DefaultImpactHops = 3
	// MaxAlternativePaths bounds the alternative-path count, and with it the enumeration
	// behind it. A value equal to the cap means "at least this many".
	MaxAlternativePaths = 100
	// maxPathExtensions bounds the whole enumeration for one side of one query. It exists so
	// that `--max-hops 8` from a hub cannot turn a read into a combinatorial walk; hitting it
	// is reported as a truncation, never silently.
	maxPathExtensions = 250_000
	// unboundedWeight is the bottleneck weight of the empty path, so that min() over the first
	// edge yields that edge's class. Weight classes run 0..5, so no real path reaches it.
	unboundedWeight = ^uint32(0)
)

// ReasonPathBudget is the truncation reason recorded when the alternative-path enumeration ran
// out of budget before it had explored every route. It joins the published set declared in
// subgraph.go (per_hop_cap, total_cap, before_recorded_history) and is the only one that impact
// can report and a subgraph cannot.
const ReasonPathBudget = "path_budget"

// Impact returns the weighted blast radius of a node as of an instant (FR-029).
//
// Both directions are expanded, each over its own as-of slice, and reported separately. The
// response is ordered by weight class descending, then hop distance ascending, then entity id,
// which is total — so the answer is a pure function of the graph and the request and can be
// frozen as a golden.
func (e *Engine) Impact(ctx context.Context, req *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error) {
	asOf := AsOfFromProto(req.GetAsOf())
	if err := asOf.Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// `max_hops` is optional, so unset and zero are different statements: unset takes the
	// published default, zero asks for a blast radius with nothing in it and is refused rather
	// than silently read as "default".
	hops := DefaultImpactHops
	if req.MaxHops != nil {
		hops = int(req.GetMaxHops())
	}
	switch {
	case hops < 1:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("query: max_hops = 0; a blast radius reaches at least one hop"))
	case hops > MaxHops:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("query: max_hops = %d exceeds the maximum of %d", hops, MaxHops))
	}
	totalCap := DefaultTotalCap
	if req.TotalCap != nil {
		totalCap = int(req.GetTotalCap())
	}

	focusRef := graph.RefFromProto(req.GetFocus())
	focusID, err := e.resolveFocus(ctx, focusRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	truncation := &graphv1.Truncation{
		PerHopCap: uint32(DefaultPerHopCap),
		TotalCap:  uint32(totalCap),
	}
	resp := &graphv1.ImpactResponse{Truncation: truncation}

	// Before recorded history is decided on the focus alone, exactly as Subgraph decides it:
	// a blast radius of nothing and a blast radius the graph was not yet watching for are
	// different answers and must not render identically (FR-052).
	extent, err := e.log.Extent(ctx)
	if err != nil {
		return nil, err
	}
	focusVersions, err := e.loadNodeVersions(ctx, asOf, r, []string{focusID})
	if err != nil {
		return nil, err
	}
	if _, ok := focusVersions[focusID]; !ok && beforeHistory(asOf, extent) {
		truncation.Reason = ReasonBeforeHistory
		return resp, nil
	}

	spec := sliceSpec{hops: hops, perHopCap: DefaultPerHopCap, totalCap: totalCap}

	// Dependents first: the list an on-call engineer reads to decide who to tell.
	spec.direction = graphv1.Direction_UPSTREAM
	if resp.Downstream, err = e.impactSide(ctx, asOf, r, focusID, spec, truncation); err != nil {
		return nil, err
	}
	spec.direction = graphv1.Direction_DOWNSTREAM
	if resp.Upstream, err = e.impactSide(ctx, asOf, r, focusID, spec, truncation); err != nil {
		return nil, err
	}
	return resp, nil
}

// impactSide expands one direction and turns what it reached into ranked items.
func (e *Engine) impactSide(
	ctx context.Context,
	asOf AsOf,
	r *redirects,
	focusID string,
	spec sliceSpec,
	truncation *graphv1.Truncation,
) ([]*graphv1.ImpactItem, error) {
	walk, err := e.slice(ctx, asOf, r, focusID, spec, truncation)
	if err != nil {
		return nil, err
	}
	if len(walk.order) <= 1 {
		return nil, nil
	}
	versions, err := e.loadNodeVersions(ctx, asOf, r, walk.order)
	if err != nil {
		return nil, err
	}

	// One adjacency, two readings of it: reachability() gives the shortest hop distance the
	// ranking already agrees on, and the path enumeration gives the heaviest route and how many
	// others there are.
	adjacency := directedAdjacency(walk.edges, spec.direction)
	reach := reachability(focusID, walk.edges, spec.direction, spec.hops)
	paths, exhausted := impactPaths(focusID, adjacency, spec.hops)
	if exhausted {
		truncation.Truncated = true
		if truncation.Reason == "" {
			truncation.Reason = ReasonPathBudget
		}
	}

	items := make([]*graphv1.ImpactItem, 0, len(walk.order))
	for _, id := range walk.order {
		if id == focusID {
			continue
		}
		version, ok := versions[id]
		if !ok {
			// The walk crossed an edge whose endpoint has no version valid at the instant.
			// Subgraph drops such a node from the answer and so does impact: an item with no
			// node to show is not an answer.
			continue
		}
		routes := paths[id]
		if len(routes) == 0 {
			continue
		}
		best := routes[0]
		items = append(items, &graphv1.ImpactItem{
			Node:             version,
			HopDistance:      uint32(reach[id].hop),
			HeaviestPath:     best.nodes,
			AlternativePaths: uint32(min(len(routes)-1, MaxAlternativePaths)),
			WeightClass:      best.minWeight,
		})
	}
	slices.SortFunc(items, compareImpactItems)
	return items, nil
}

// compareImpactItems is the published order: heaviest first, then nearest, then by entity id
// so the key is total and the answer is reproducible (FR-029).
func compareImpactItems(a, b *graphv1.ImpactItem) int {
	if a.GetWeightClass() != b.GetWeightClass() {
		if a.GetWeightClass() > b.GetWeightClass() {
			return -1
		}
		return 1
	}
	if a.GetHopDistance() != b.GetHopDistance() {
		if a.GetHopDistance() < b.GetHopDistance() {
			return -1
		}
		return 1
	}
	return strings.Compare(a.GetNode().GetEntityId(), b.GetNode().GetEntityId())
}

// ---------- adjacency ----------

// weightedLink is one directed step of a walk: where it goes and how much traffic it carries.
// Edge types with no traffic weight contribute class 0 (research §9).
type weightedLink struct {
	to    string
	class uint32
}

// directedAdjacency collapses edge rows into the adjacency a traversal follows, in the
// direction asked for.
//
// It is shared by the ranking's reachability pass and by the impact query's path enumeration,
// so the two can never disagree about which edges exist or what they weigh. One (src, dst,
// type) may appear more than once — the two sides of a diff window, or a canonicalized pair
// read from before a merge — and `supersedes` picks the row that speaks for the relationship;
// the first-seen order of the keys is kept so the adjacency is built the same way on every run.
func directedAdjacency(rows []edgeRow, direction graphv1.Direction) map[string][]weightedLink {
	latest := map[string]edgeRow{}
	var order []string
	for _, row := range rows {
		key := edgeDeltaKey(row)
		current, seen := latest[key]
		if !seen {
			order = append(order, key)
		}
		if !seen || supersedes(current, row) {
			latest[key] = row
		}
	}

	adjacency := map[string][]weightedLink{}
	for _, key := range order {
		row := latest[key]
		var class uint32
		if row.weightClass != nil {
			class = *row.weightClass
		}
		if direction != graphv1.Direction_UPSTREAM {
			adjacency[row.srcID] = append(adjacency[row.srcID], weightedLink{to: row.dstID, class: class})
		}
		if direction != graphv1.Direction_DOWNSTREAM {
			adjacency[row.dstID] = append(adjacency[row.dstID], weightedLink{to: row.srcID, class: class})
		}
	}
	return adjacency
}

// ---------- paths ----------

// impactPath is one simple path from the focus to a node: the entity ids it passes through,
// focus first and node last, and the weight class of its weakest link.
type impactPath struct {
	nodes     []string
	minWeight uint32
}

// comparePaths is the published definition of "heaviest": the path whose weakest link is
// loudest, then the shorter one, then the lexicographically smaller sequence of entity ids.
// The last clause is what makes the choice total, and therefore reproducible.
func comparePaths(a, b impactPath) int {
	if a.minWeight != b.minWeight {
		if a.minWeight > b.minWeight {
			return -1
		}
		return 1
	}
	if len(a.nodes) != len(b.nodes) {
		return len(a.nodes) - len(b.nodes)
	}
	return slices.Compare(a.nodes, b.nodes)
}

// impactPaths enumerates the simple paths from the focus, level by level, and returns them per
// node in heaviest-first order.
//
// Each node keeps at most MaxAlternativePaths+1 paths, so the enumeration is bounded and the
// count saturates at the published cap. Truncating the kept set cannot lose the heaviest route:
// the best path to a node is always rank 1 of its own list, so extending only the kept paths
// still extends the one every heavier route would have been built from. It reports whether the
// global extension budget ran out, which the caller turns into a truncation.
func impactPaths(focusID string, adjacency map[string][]weightedLink, maxHops int) (map[string][]impactPath, bool) {
	const budget = MaxAlternativePaths + 1

	all := map[string][]impactPath{}
	frontier := map[string][]impactPath{
		focusID: {{nodes: []string{focusID}, minWeight: unboundedWeight}},
	}
	extensions := 0
	exhausted := false

	for hop := 1; hop <= maxHops && len(frontier) > 0 && !exhausted; hop++ {
		next := map[string][]impactPath{}
		// Sorted, so a budget that runs out cuts in the same place on every run.
		froms := make([]string, 0, len(frontier))
		for from := range frontier {
			froms = append(froms, from)
		}
		slices.Sort(froms)

		for _, from := range froms {
			for _, path := range frontier[from] {
				for _, link := range adjacency[from] {
					if slices.Contains(path.nodes, link.to) {
						continue // a simple path never revisits a node
					}
					if extensions >= maxPathExtensions {
						exhausted = true
						break
					}
					extensions++
					nodes := make([]string, len(path.nodes), len(path.nodes)+1)
					copy(nodes, path.nodes)
					next[link.to] = append(next[link.to], impactPath{
						nodes:     append(nodes, link.to),
						minWeight: min(path.minWeight, link.class),
					})
				}
				if exhausted {
					break
				}
			}
			if exhausted {
				break
			}
		}

		for id, found := range next {
			slices.SortFunc(found, comparePaths)
			if len(found) > budget {
				found = found[:budget]
			}
			next[id] = found
			merged := append(all[id], found...)
			slices.SortFunc(merged, comparePaths)
			if len(merged) > budget {
				merged = merged[:budget]
			}
			all[id] = merged
		}
		frontier = next
	}

	// A route that leaves the focus and comes back is a cycle, not a blast radius item.
	delete(all, focusID)
	return all, exhausted
}
