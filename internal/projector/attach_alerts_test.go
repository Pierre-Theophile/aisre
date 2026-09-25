// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"slices"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Late attachment for alerts (003 FR-048, plan item 5).
//
// The behaviour these pin, and why it is worth a file: an alert whose watched entity has not been
// observed is KEPT AND MARKED UNATTACHED rather than given an edge to a minted placeholder. The
// edge appears when the target does, carrying both halves of its provenance.
//
// The old behaviour created the edge immediately against a placeholder — an entity with no type and
// no description. That is worse than no edge for one specific reason: an investigation walking
// WATCHES out of a firing alert could not tell "watches a service we know about" from "watches a
// name we have never seen", because both are an edge to an entity. Marking it unattached makes the
// second a stated fact.

func watchAlertEvent(id, watched, when string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_AlertTransition{AlertTransition: &graphv1.AlertTransition{
			Monitor:      &graphv1.Ref{Namespace: "gcp.alert", Value: "policy-7"},
			TransitionAt: at(when),
			FromState:    "ok", ToState: "alert",
			Watches:   []*graphv1.Ref{{Namespace: "otel.service.name", Value: watched}},
			Transport: "poll",
			Title:     "checkout error rate",
		}},
	}
}

func serviceNodeEvent(id, name, validAt string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         &graphv1.Ref{Namespace: "otel.service.name", Value: name},
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: name,
			ValidAt:     at(validAt),
		}},
	}
}

func watchesEdge(t *testing.T, store *postgres.Store, alertID, watchedID string) (postgres.TimeRange, []string, bool) {
	t.Helper()
	var valid postgres.TimeRange
	var producedBy []string
	err := store.Pool().QueryRow(context.Background(), `
		SELECT valid, produced_by_event_ids FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'watches' AND upper_inf(observed)`,
		alertID, watchedID).Scan(&valid, &producedBy)
	if err != nil {
		return postgres.TimeRange{}, nil, false
	}
	return valid, producedBy, true
}

func placeholderCount(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM graph.entity_versions
		WHERE upper_inf(observed) AND props ? $1`, projector.PlaceholderProp).Scan(&n); err != nil {
		t.Fatalf("count placeholders: %v", err)
	}
	return n
}

func TestAnAlertWhoseTargetIsUnknownIsMarkedUnattachedRatherThanGivenAPlaceholder(t *testing.T) {
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, watchAlertEvent("a:e1", "checkout", "2026-09-01T14:21:00Z"), "2026-09-01T14:21:05Z")

	alertID := graph.EntityID("gcp.alert", "policy-7")
	checkoutID := graph.EntityID("otel.service.name", "checkout")

	if _, _, found := watchesEdge(t, store, alertID, checkoutID); found {
		t.Error("a WATCHES edge was created to an entity nothing has described")
	}
	if n := placeholderCount(t, store); n != 0 {
		t.Errorf("%d placeholder entit(ies) minted; the unresolved target should be a stated gap "+
			"on the alert, not an entity nobody described", n)
	}
	// The alert itself exists and is complete — it is kept, not dropped (FR-048).
	var alertVersions int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM graph.entity_versions WHERE entity_id = $1 AND upper_inf(observed)`,
		alertID).Scan(&alertVersions); err != nil {
		t.Fatalf("count alert versions: %v", err)
	}
	if alertVersions == 0 {
		t.Error("the alert was dropped; an alert whose target does not resolve must be kept")
	}
}

func TestAnAlertAttachesWhenItsTargetAppears(t *testing.T) {
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, watchAlertEvent("a:e1", "checkout", "2026-09-01T14:21:00Z"), "2026-09-01T14:21:05Z")
	apply(t, p, serviceNodeEvent("n:e1", "checkout", "2026-09-01T14:00:00Z"), "2026-09-01T14:30:00Z")

	alertID := graph.EntityID("gcp.alert", "policy-7")
	checkoutID := graph.EntityID("otel.service.name", "checkout")

	valid, producedBy, found := watchesEdge(t, store, alertID, checkoutID)
	if !found {
		t.Fatal("the edge did not appear when the target did; FR-048 requires automatic attachment")
	}

	// Dated from the transition, not from the discovery. Dating it from when the graph happened to
	// learn about the target would make the edge's valid time an arrival-order artefact, which is
	// what FR-021 forbids.
	if want := mustTime("2026-09-01T14:21:00Z"); !valid.Start.Equal(want) {
		t.Errorf("edge starts at %s, want the transition instant %s", valid.Start, want)
	}
	if !valid.EndUnbounded {
		t.Errorf("edge ends at %v; an alert goes on watching until something says otherwise, so an "+
			"end would be an invention", valid.End)
	}

	// Two-sided provenance (FR-048, mirroring FR-034 for changes): an operator asking why this edge
	// exists sees the transition that named the target AND the event that created it.
	for _, want := range []string{"a:e1", "n:e1"} {
		if !slices.Contains(producedBy, want) {
			t.Errorf("produced_by_event_ids = %v, missing %q", producedBy, want)
		}
	}

	// And the gap is closed: nothing still says the target is unattached.
	var stillUnattached int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) AND props ? $2`,
		alertID, projector.AlertUnattachedWatchesProp).Scan(&stillUnattached); err != nil {
		t.Fatalf("count unattached: %v", err)
	}
	if stillUnattached != 0 {
		t.Errorf("%d current alert version(s) still name the target as unattached after it "+
			"attached; the list must say what is STILL missing", stillUnattached)
	}
}

func TestAnAlertThatFiredAndRecoveredGetsOneWatchesEdge(t *testing.T) {
	store := openStore(t)
	p := projector.New(store)

	// Two transitions before the target appears, so the alert has two current state segments and
	// both were waiting.
	apply(t, p, watchAlertEvent("a:e1", "checkout", "2026-09-01T14:21:00Z"), "2026-09-01T14:21:05Z")
	apply(t, p, watchAlertEvent("a:e2", "checkout", "2026-09-01T14:38:00Z"), "2026-09-01T14:38:05Z")
	apply(t, p, serviceNodeEvent("n:e1", "checkout", "2026-09-01T14:00:00Z"), "2026-09-01T14:45:00Z")

	alertID := graph.EntityID("gcp.alert", "policy-7")
	checkoutID := graph.EntityID("otel.service.name", "checkout")

	var edges int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'watches' AND upper_inf(observed)`,
		alertID, checkoutID).Scan(&edges); err != nil {
		t.Fatalf("count watches edges: %v", err)
	}
	// What it watches is not a property of a state segment: the alert watched the same thing through
	// every transition, so one relationship is one edge.
	if edges != 1 {
		t.Errorf("%d WATCHES edges for one relationship; an alert's state is segmented but what it "+
			"watches is not", edges)
	}

	valid, _, found := watchesEdge(t, store, alertID, checkoutID)
	if !found {
		t.Fatal("no watches edge")
	}
	// From the FIRST transition, not the last — the watch began when the alert did.
	if want := mustTime("2026-09-01T14:21:00Z"); !valid.Start.Equal(want) {
		t.Errorf("edge starts at %s, want the first transition %s", valid.Start, want)
	}
}

func TestAnAlertWhoseTargetIsAlreadyKnownGetsItsEdgeImmediately(t *testing.T) {
	store := openStore(t)
	p := projector.New(store)

	// The ordinary case, unchanged by FR-048: nothing is deferred when nothing is missing.
	apply(t, p, serviceNodeEvent("n:e1", "checkout", "2026-09-01T14:00:00Z"), "2026-09-01T14:10:00Z")
	apply(t, p, watchAlertEvent("a:e1", "checkout", "2026-09-01T14:21:00Z"), "2026-09-01T14:21:05Z")

	alertID := graph.EntityID("gcp.alert", "policy-7")
	checkoutID := graph.EntityID("otel.service.name", "checkout")

	valid, _, found := watchesEdge(t, store, alertID, checkoutID)
	if !found {
		t.Fatal("no watches edge for a target the graph already knew")
	}
	if want := mustTime("2026-09-01T14:21:00Z"); !valid.Start.Equal(want) {
		t.Errorf("edge starts at %s, want the transition instant %s", valid.Start, want)
	}
	var unattached int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) AND props ? $2`,
		alertID, projector.AlertUnattachedWatchesProp).Scan(&unattached); err != nil {
		t.Fatalf("count unattached: %v", err)
	}
	if unattached != 0 {
		t.Errorf("%d version(s) carry an unattached list when nothing was unattached; an alert "+
			"whose targets all resolve must produce exactly the node it did before FR-048", unattached)
	}
}
