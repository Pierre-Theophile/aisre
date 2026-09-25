// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// Unit tests for the blast radius (T064), over graphs built here rather than loaded from a
// fixture. The fixtures prove the story end to end; each of these proves one published rule on
// the smallest graph that can show it, so a failure names the rule that broke.
//
// The rules under test are the ones an operator would otherwise have to take on trust: which
// list a node lands in, what "heaviest path" means when the shortest path is not the heaviest,
// what an edge with no traffic weight does to the order, and that a cap says so.

// ---------- builders ----------

// edge adds an edge of any type between two nodes of the test namespace, for the cases that
// need a relationship carrying no traffic weight.
func (b *builder) edge(src, dst string, edgeType graphv1.EdgeType) {
	b.t.Helper()
	b.apply(&graphv1.UpsertEdge{
		Src:     &graphv1.Ref{Namespace: testNS, Value: src},
		Dst:     &graphv1.Ref{Namespace: testNS, Value: dst},
		Type:    edgeType,
		ValidAt: timestamppb.New(baseValid),
	}, time.Time{})
}

// askImpact runs an impact query at the standard instant and returns the response.
func askImpact(t *testing.T, e *query.Engine, req *graphv1.ImpactRequest) *graphv1.ImpactResponse {
	t.Helper()
	if req.GetAsOf() == nil {
		req.AsOf = &graphv1.AsOf{ValidAt: timestamppb.New(queryAt)}
	}
	resp, err := e.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	return resp
}

// itemNames is the display names of one side of an impact answer, in the order the engine put
// them — the order is part of the contract, so it is never sorted here.
func itemNames(items []*graphv1.ImpactItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.GetNode().GetDisplayName())
	}
	return out
}

// item finds one side's entry for a display name.
func item(t *testing.T, items []*graphv1.ImpactItem, displayName string) *graphv1.ImpactItem {
	t.Helper()
	for _, it := range items {
		if it.GetNode().GetDisplayName() == displayName {
			return it
		}
	}
	t.Fatalf("no item for %q; the list holds %v", displayName, itemNames(items))
	return nil
}

// pathNames renders an item's heaviest path as display names, so an assertion reads like the
// route it is checking rather than like a list of base32 ids.
func pathNames(t *testing.T, resp *graphv1.ImpactResponse, focusID string, focusName string, path []string) string {
	t.Helper()
	names := map[string]string{focusID: focusName}
	for _, items := range [][]*graphv1.ImpactItem{resp.GetDownstream(), resp.GetUpstream()} {
		for _, it := range items {
			names[it.GetNode().GetEntityId()] = it.GetNode().GetDisplayName()
		}
	}
	hops := make([]string, 0, len(path))
	for _, id := range path {
		name, ok := names[id]
		if !ok {
			name = id
		}
		hops = append(hops, name)
	}
	return strings.Join(hops, "->")
}

// focusIDOf is the entity id the paths of an answer start from.
func focusIDOf(resp *graphv1.ImpactResponse) string {
	for _, items := range [][]*graphv1.ImpactItem{resp.GetDownstream(), resp.GetUpstream()} {
		for _, it := range items {
			if path := it.GetHeaviestPath(); len(path) > 0 {
				return path[0]
			}
		}
	}
	return ""
}

// ---------- the direction mapping ----------

// TestImpactSeparatesDependentsFromDependencies is US4 scenario 1, on the smallest graph that
// can show it: with `checkout calls payments` and `payments calls payments-db`, the impact of
// payments lists checkout as a downstream dependent and payments-db as an upstream dependency,
// each at hop 1 with the traffic weight of the edge.
//
// This is the assertion that pins the published mapping. `downstream` is the edges pointing
// INTO the focus — the opposite of what `--direction down` follows on a subgraph — and getting
// it backwards would page the wrong team.
func TestImpactSeparatesDependentsFromDependencies(t *testing.T) {
	b := newBuilder(t)
	for _, name := range []string{"checkout", "payments", "payments-db"} {
		b.node(name)
	}
	b.calls("checkout", "payments", 3, time.Time{}, time.Time{})
	b.calls("payments", "payments-db", 4, time.Time{}, time.Time{})

	resp := askImpact(t, b.engine(), &graphv1.ImpactRequest{Focus: focus("payments")})

	if got := itemNames(resp.GetDownstream()); !slices.Equal(got, []string{"checkout"}) {
		t.Errorf("downstream = %v, want [checkout]: it is what depends on payments", got)
	}
	if got := itemNames(resp.GetUpstream()); !slices.Equal(got, []string{"payments-db"}) {
		t.Errorf("upstream = %v, want [payments-db]: it is what payments depends on", got)
	}
	if got := item(t, resp.GetDownstream(), "checkout"); got.GetHopDistance() != 1 || got.GetWeightClass() != 3 {
		t.Errorf("checkout: hop %d weight %d, want hop 1 weight 3", got.GetHopDistance(), got.GetWeightClass())
	}
	if got := item(t, resp.GetUpstream(), "payments-db"); got.GetHopDistance() != 1 || got.GetWeightClass() != 4 {
		t.Errorf("payments-db: hop %d weight %d, want hop 1 weight 4", got.GetHopDistance(), got.GetWeightClass())
	}
}

// ---------- the heaviest path ----------

// TestImpactReportsTheHeaviestPathNotTheShortest is US4 scenario 2. storefront reaches payments
// two ways: directly over a quiet edge, and through checkout over two louder ones. It must
// appear once, at its *shortest* hop distance, with the *heaviest* path shown and the number of
// alternatives stated — three answers to three different questions, which may disagree.
func TestImpactReportsTheHeaviestPathNotTheShortest(t *testing.T) {
	b := newBuilder(t)
	for _, name := range []string{"storefront", "checkout", "payments"} {
		b.node(name)
	}
	b.calls("checkout", "payments", 3, time.Time{}, time.Time{})
	b.calls("storefront", "checkout", 2, time.Time{}, time.Time{})
	b.calls("storefront", "payments", 1, time.Time{}, time.Time{}) // the direct, quiet route

	resp := askImpact(t, b.engine(), &graphv1.ImpactRequest{Focus: focus("payments")})

	// Heaviest first: checkout's single route bottlenecks at 3, storefront's best at 2.
	if got := itemNames(resp.GetDownstream()); !slices.Equal(got, []string{"checkout", "storefront"}) {
		t.Fatalf("downstream = %v, want [checkout storefront] (weight desc)", got)
	}

	store := item(t, resp.GetDownstream(), "storefront")
	if store.GetHopDistance() != 1 {
		t.Errorf("storefront hop = %d, want 1: the direct edge is the shortest way in",
			store.GetHopDistance())
	}
	if store.GetWeightClass() != 2 {
		t.Errorf("storefront weight = %d, want 2: the heaviest route bottlenecks on storefront->checkout",
			store.GetWeightClass())
	}
	if store.GetAlternativePaths() != 1 {
		t.Errorf("storefront alternative_paths = %d, want 1: the direct edge is the other way in",
			store.GetAlternativePaths())
	}
	got := pathNames(t, resp, focusIDOf(resp), "payments", store.GetHeaviestPath())
	if got != "payments->checkout->storefront" {
		t.Errorf("storefront heaviest path = %s, want payments->checkout->storefront", got)
	}
}

// TestImpactUnweightedEdgesRankLast: an edge type that carries no traffic contributes class 0 to
// a path (research §9), so a route over it ties every other unweighted route and the ordering
// falls back to hop distance. The `calls` dependency outranks the node pool the service runs on,
// which is the behaviour that keeps an impact list readable.
func TestImpactUnweightedEdgesRankLast(t *testing.T) {
	b := newBuilder(t)
	for _, name := range []string{"payments", "payments-db", "general", "cluster"} {
		b.node(name)
	}
	b.calls("payments", "payments-db", 3, time.Time{}, time.Time{})
	b.edge("payments", "general", graphv1.EdgeType_RUNS_ON)
	b.edge("general", "cluster", graphv1.EdgeType_RUNS_ON)

	resp := askImpact(t, b.engine(), &graphv1.ImpactRequest{Focus: focus("payments")})

	if got := itemNames(resp.GetUpstream()); !slices.Equal(got, []string{"payments-db", "general", "cluster"}) {
		t.Fatalf("upstream = %v, want [payments-db general cluster]: weight 3 first, then class 0 by hop", got)
	}
	if got := item(t, resp.GetUpstream(), "general"); got.GetWeightClass() != 0 {
		t.Errorf("runs_on weight = %d, want 0: an edge type with no traffic weight counts as class 0",
			got.GetWeightClass())
	}
	if got := item(t, resp.GetUpstream(), "cluster"); got.GetHopDistance() != 2 {
		t.Errorf("cluster hop = %d, want 2", got.GetHopDistance())
	}
}

// ---------- bounds ----------

// TestImpactRadiusBoundsTheBlastRadius: max_hops is exactly how far the answer reaches.
func TestImpactRadiusBoundsTheBlastRadius(t *testing.T) {
	b := newBuilder(t)
	b.chain() // a -4-> b -3-> c -1-> d

	one := uint32(1)
	resp := askImpact(t, b.engine(), &graphv1.ImpactRequest{Focus: focus("b"), MaxHops: &one})
	if got := itemNames(resp.GetUpstream()); !slices.Equal(got, []string{"c"}) {
		t.Errorf("one hop out of b = %v, want [c]", got)
	}
	if got := itemNames(resp.GetDownstream()); !slices.Equal(got, []string{"a"}) {
		t.Errorf("one hop into b = %v, want [a]", got)
	}

	three := uint32(3)
	resp = askImpact(t, b.engine(), &graphv1.ImpactRequest{Focus: focus("b"), MaxHops: &three})
	if got := itemNames(resp.GetUpstream()); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("three hops out of b = %v, want [c d]", got)
	}
}

// TestImpactTotalCapTruncatesAndSaysSo: a cap that bites is reported, with the reason and the
// caps that were in force, exactly as a subgraph reports it (FR-026, FR-029).
func TestImpactTotalCapTruncatesAndSaysSo(t *testing.T) {
	b := newBuilder(t)
	b.node("hub")
	for _, name := range []string{"c1", "c2", "c3", "c4", "c5"} {
		b.node(name)
		b.calls(name, "hub", 2, time.Time{}, time.Time{})
	}

	cap := uint32(3)
	resp := askImpact(t, b.engine(), &graphv1.ImpactRequest{Focus: focus("hub"), TotalCap: &cap})

	if !resp.GetTruncation().GetTruncated() {
		t.Fatalf("expected truncation with total_cap 3, got %+v", resp.GetTruncation())
	}
	if resp.GetTruncation().GetReason() != query.ReasonTotalCap {
		t.Errorf("reason = %q, want %q", resp.GetTruncation().GetReason(), query.ReasonTotalCap)
	}
	if resp.GetTruncation().GetTotalCap() != 3 {
		t.Errorf("total_cap = %d, want 3", resp.GetTruncation().GetTotalCap())
	}
	if n := len(resp.GetDownstream()); n > 2 {
		t.Errorf("%d dependents past a total cap of 3 (the focus counts against it)", n)
	}
}

// TestImpactMissingValidInstantIsInvalidArgument: the graph answers as of an instant and never
// defaults it (constitution II).
func TestImpactMissingValidInstantIsInvalidArgument(t *testing.T) {
	b := newBuilder(t)
	b.node("a")

	_, err := b.engine().Impact(context.Background(), &graphv1.ImpactRequest{Focus: focus("a")})
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("impact without an as-of: code %v (%v), want InvalidArgument", got, err)
	}
}

// TestImpactRadiusPastTheMaximumIsRefused: asking for more than MaxHops is a bad request, not a
// silently clamped one — a caller who thinks they asked for twelve hops must not be told they
// got them.
func TestImpactRadiusPastTheMaximumIsRefused(t *testing.T) {
	b := newBuilder(t)
	b.node("a")

	hops := uint32(query.MaxHops + 1)
	_, err := b.engine().Impact(context.Background(), &graphv1.ImpactRequest{
		Focus:   focus("a"),
		AsOf:    &graphv1.AsOf{ValidAt: timestamppb.New(queryAt)},
		MaxHops: &hops,
	})
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("impact with %d hops: code %v (%v), want InvalidArgument", hops, got, err)
	}
}

// TestImpactUnknownFocusIsNotFound: a name the graph has never heard of and a name with nothing
// valid at the instant are different answers (FR-039).
func TestImpactUnknownFocusIsNotFound(t *testing.T) {
	b := newBuilder(t)
	b.node("a")

	_, err := b.engine().Impact(context.Background(), &graphv1.ImpactRequest{
		Focus: focus("nothing-here"),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(queryAt)},
	})
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("impact of an unknown node: code %v (%v), want NotFound", got, err)
	}
}
