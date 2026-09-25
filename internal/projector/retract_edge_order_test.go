// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// A retraction and a re-assertion of the same subject may arrive either way round (FR-013,
// FR-021, segments.go).
//
// This file used to pin the opposite. A retraction leaves no row of its own: it cuts the
// versions it finds and is then forgotten, so a retraction delivered *before* a later
// re-assertion of the same edge used to lose the interval the re-assertion had opened. The two
// upserts coalesced into one segment — coalesce compares values, not which event asserted them —
// and the retraction truncated the whole of it.
//
// The scenario below is `fixtures/feeder-gap-01` reduced to three events:
//
//	upsert    inventory -> redis   valid_at  10:40
//	retract   inventory -> redis   valid_end 10:45
//	upsert    inventory -> redis   valid_at  11:55   (same weight, same props)
//
// The answer, in either delivery order, is that the call path was seen, went quiet, and came
// back: [10:40, 10:45) and [11:55, ∞). What makes it order-independent is that a coalesced
// segment now remembers the first instant each source restated it, and every planner expands a
// segment back into the sub-segments those instants imply before doing anything else — so the
// retraction splits [10:40, ∞) at 11:55 and cuts only the part in front of it
// (segments.go, rememberRestatement and expandRestatements).
//
// **The limit that remains**, stated here because a feeder's declared reordering window depends
// on it: a retraction delivered before the assertions it should cut — before *all* of them, or
// before one whose valid instant precedes the retraction's — is still forgotten, because there
// is nothing in the projection for it to cut and it leaves nothing behind. The permutations
// below are therefore the ones in which each retraction follows the assertions it ends, which is
// the guarantee the graph makes and the one `fixture verify`'s shuffle step exercises.

// The source is the merge test's telemetry feeder, already registered by newMergeProjector
// with a five-minute window: the declared window is not what this test is about, the projector's
// behaviour under a given delivery order is.
var (
	orderInventory = graph.Ref{Namespace: "otel.service.name", Value: "inventory"}
	orderRedis     = graph.Ref{Namespace: "server.address", Value: "redis.shop.svc.cluster.local"}
)

func retractEdgeEvent(id, source string, src, dst graph.Ref, validEnd time.Time) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       source,
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_RetractEdge{RetractEdge: &graphv1.RetractEdge{
			Src:      &graphv1.Ref{Namespace: src.Namespace, Value: src.Value},
			Dst:      &graphv1.Ref{Namespace: dst.Namespace, Value: dst.Value},
			Type:     graphv1.EdgeType_CALLS,
			ValidEnd: timestamppb.New(validEnd),
		}},
	}
}

func retractNodeEvent(id, source string, ref graph.Ref, validEnd time.Time) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       source,
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_RetractNode{RetractNode: &graphv1.RetractNode{
			Ref:      &graphv1.Ref{Namespace: ref.Namespace, Value: ref.Value},
			ValidEnd: timestamppb.New(validEnd),
		}},
	}
}

func TestRetractionAndReassertionAreOrderIndependent(t *testing.T) {
	seen := mustTime("2026-09-16T10:40:00Z")
	gone := mustTime("2026-09-16T10:45:00Z")
	again := mustTime("2026-09-16T11:55:00Z")

	events := map[string]*graphv1.EventEnvelope{
		"src-node":  nodeEvent("src-node", mergeSourceOtel, orderInventory, graphv1.NodeType_SERVICE, seen),
		"dst-node":  nodeEvent("dst-node", mergeSourceOtel, orderRedis, graphv1.NodeType_THIRD_PARTY, seen),
		"edge-1040": callsEvent("edge-1040", mergeSourceOtel, orderInventory, orderRedis, 2, seen),
		"retract":   retractEdgeEvent("retract", mergeSourceOtel, orderInventory, orderRedis, gone),
		"edge-1155": callsEvent("edge-1155", mergeSourceOtel, orderInventory, orderRedis, 2, again),
	}

	inventory := graph.EntityID(orderInventory.Namespace, orderInventory.Value)
	redis := graph.EntityID(orderRedis.Namespace, orderRedis.Value)
	want := []string{
		fmt.Sprintf("%s -calls-> %s valid=[2026-09-16T10:40:00Z,2026-09-16T10:45:00Z) weight=2", inventory, redis),
		fmt.Sprintf("%s -calls-> %s valid=[2026-09-16T11:55:00Z,) weight=2", inventory, redis),
	}

	for _, tc := range []struct {
		name  string
		order []string
	}{{
		name:  "in-order",
		order: []string{"src-node", "dst-node", "edge-1040", "retract", "edge-1155"},
	}, {
		name:  "re-assertion-before-retraction",
		order: []string{"src-node", "dst-node", "edge-1040", "edge-1155", "retract"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			store := applyOrder(t, events, tc.order)
			got := currentEdges(t, store)
			if !slices.Equal(got, want) {
				t.Errorf("current edges =\n  %s\nwant (the call path was seen, went quiet, and came back)\n  %s",
					strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			assertInvariants(t, store, "after "+tc.name)
		})
	}
}

// TestRetractReassertRetractPermutations is the same property driven over every legal delivery
// order of a longer sequence: seen, gone, seen again, gone again — for an edge and for a node.
//
// "Legal" is the guarantee stated at the top of this file: a retraction is delivered after the
// assertions whose valid instants it ends. Every such order must produce the valid-time state
// the in-order delivery produces, which is the only thing FR-021 talks about.
func TestRetractReassertRetractPermutations(t *testing.T) {
	var (
		t1 = mustTime("2026-09-16T10:40:00Z")
		t2 = mustTime("2026-09-16T10:45:00Z")
		t3 = mustTime("2026-09-16T11:55:00Z")
		t4 = mustTime("2026-09-16T12:30:00Z")
	)

	for _, subject := range []struct {
		name string
		// facts are the four events under permutation, in valid-time order, each with the
		// instant it asserts or ends at.
		facts []orderedFact
		// fixed events are applied first in every permutation: an edge needs its endpoints,
		// and a retraction of a node the graph has never seen is a rejected event by design.
		fixed []string
		state func(*testing.T, *postgres.Store) []string
	}{{
		name:  "edge",
		fixed: []string{"src-node", "dst-node"},
		facts: []orderedFact{
			{name: "up-1", at: t1, assertion: true},
			{name: "down-1", at: t2},
			{name: "up-2", at: t3, assertion: true},
			{name: "down-2", at: t4},
		},
		state: currentEdges,
	}, {
		name:  "node",
		fixed: []string{"node-1"},
		facts: []orderedFact{
			// The node's first assertion is fixed rather than permuted: `retract_node` on an
			// entity the graph has never observed is refused before it is appended
			// (apply.go, checkResolvable), so an order beginning with a retraction is not a
			// delivery order, it is a rejected event.
			{name: "node-down-1", at: t2},
			{name: "node-up-2", at: t3, assertion: true},
			{name: "node-down-2", at: t4},
		},
		state: currentNodeVersions,
	}} {
		t.Run(subject.name, func(t *testing.T) {
			events := map[string]*graphv1.EventEnvelope{
				"src-node":    nodeEvent("src-node", mergeSourceOtel, orderInventory, graphv1.NodeType_SERVICE, t1),
				"dst-node":    nodeEvent("dst-node", mergeSourceOtel, orderRedis, graphv1.NodeType_THIRD_PARTY, t1),
				"up-1":        callsEvent("up-1", mergeSourceOtel, orderInventory, orderRedis, 2, t1),
				"down-1":      retractEdgeEvent("down-1", mergeSourceOtel, orderInventory, orderRedis, t2),
				"up-2":        callsEvent("up-2", mergeSourceOtel, orderInventory, orderRedis, 2, t3),
				"down-2":      retractEdgeEvent("down-2", mergeSourceOtel, orderInventory, orderRedis, t4),
				"node-1":      nodeEvent("node-1", mergeSourceOtel, orderRedis, graphv1.NodeType_THIRD_PARTY, t1),
				"node-down-1": retractNodeEvent("node-down-1", mergeSourceOtel, orderRedis, t2),
				"node-up-2":   nodeEvent("node-up-2", mergeSourceOtel, orderRedis, graphv1.NodeType_THIRD_PARTY, t3),
				"node-down-2": retractNodeEvent("node-down-2", mergeSourceOtel, orderRedis, t4),
			}

			inOrder := append(slices.Clone(subject.fixed), factNames(subject.facts)...)
			want := subject.state(t, applyOrder(t, events, inOrder))

			orders := legalOrders(subject.facts)
			if len(orders) < 2 {
				t.Fatalf("only %d legal orders; the permutation is not exercising anything", len(orders))
			}
			for _, order := range orders {
				full := append(slices.Clone(subject.fixed), order...)
				name := strings.Join(order, ",")
				t.Run(name, func(t *testing.T) {
					store := applyOrder(t, events, full)
					got := subject.state(t, store)
					if !slices.Equal(got, want) {
						t.Errorf("delivery order %s produced\n  %s\nwant the in-order state\n  %s",
							name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
					}
					assertInvariants(t, store, "after "+name)
				})
			}
			t.Logf("%d legal delivery orders checked", len(orders))
		})
	}
}

// orderedFact is one event in a permuted sequence: what it is called, the valid instant it
// speaks about, and whether it asserts (as opposed to retracts).
type orderedFact struct {
	name      string
	at        time.Time
	assertion bool
}

func factNames(facts []orderedFact) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.name)
	}
	return out
}

// legalOrders enumerates every permutation in which each retraction is delivered after every
// assertion whose valid instant is at or before the retraction's end.
//
// That constraint is the guarantee, not a convenience: a retraction leaves no row behind, so one
// delivered before the fact it ends has nothing to cut and is lost. Enumerating only the legal
// orders keeps the test about the property that is claimed rather than about the one that is
// not — and the count is logged, so a change that quietly shrinks the space is visible.
func legalOrders(facts []orderedFact) [][]string {
	var out [][]string
	var walk func(placed []int, remaining []int)
	walk = func(placed []int, remaining []int) {
		if len(remaining) == 0 {
			order := make([]string, 0, len(placed))
			for _, i := range placed {
				order = append(order, facts[i].name)
			}
			out = append(out, order)
			return
		}
		for pos, i := range remaining {
			if !facts[i].assertion && !assertionsPlaced(facts, placed, facts[i].at) {
				continue
			}
			next := append(slices.Clone(remaining[:pos]), remaining[pos+1:]...)
			walk(append(slices.Clone(placed), i), next)
		}
	}
	all := make([]int, len(facts))
	for i := range all {
		all[i] = i
	}
	walk(nil, all)
	return out
}

// assertionsPlaced reports whether every assertion at or before `end` has already been delivered.
func assertionsPlaced(facts []orderedFact, placed []int, end time.Time) bool {
	for i, f := range facts {
		if !f.assertion || f.at.After(end) {
			continue
		}
		if !slices.Contains(placed, i) {
			return false
		}
	}
	return true
}

// currentNodeVersions renders every entity version current in observed time, the way
// currentEdges does for edges.
func currentNodeVersions(t *testing.T, store *postgres.Store) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT entity_id, valid, display_name
		FROM graph.entity_versions
		WHERE upper_inf(observed)
		ORDER BY entity_id, lower(valid)`)
	if err != nil {
		t.Fatalf("read current entity versions: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			entityID, name string
			valid          postgres.TimeRange
		)
		if err := rows.Scan(&entityID, &valid, &name); err != nil {
			t.Fatalf("scan current entity version: %v", err)
		}
		out = append(out, fmt.Sprintf("%s valid=%s name=%q", entityID, valid, name))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read current entity versions: %v", err)
	}
	return out
}
