// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Changes, attached and dangling (FR-003, FR-004, edge case "dangling change").

func changeEvent(id string, targets ...string) *graphv1.EventEnvelope {
	refs := make([]*graphv1.Ref, 0, len(targets))
	for _, target := range targets {
		refs = append(refs, &graphv1.Ref{Namespace: "otel.service.name", Value: target})
	}
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       propSourceID(0),
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{
			Ref: &graphv1.Ref{Namespace: "k8s.rollout", Value: id},
			Change: &graphv1.Change{
				Kind:      graphv1.ChangeKind_ROLLOUT,
				Summary:   "rolled out " + strings.Join(targets, ", "),
				Actor:     "alice",
				OriginRef: "deploy/" + id,
			},
			Targets: refs,
			ValidAt: timestamppb.New(mustTime("2026-09-01T14:20:00Z")),
		}},
	}
}

func TestObserveChangeLinksResolvedTargets(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	node := upsertSpec{source: 0, entity: "payments", name: "payments",
		validAt: mustTime("2026-09-01T13:00:00Z"), props: map[string]string{"service.version": "1.0"}}
	if _, err := p.Apply(ctx, node.event("src:0:payments"), mustTime("2026-09-01T13:01:00Z")); err != nil {
		t.Fatalf("apply node: %v", err)
	}
	if _, err := p.Apply(ctx, changeEvent("rev7", "payments"), mustTime("2026-09-01T14:21:00Z")); err != nil {
		t.Fatalf("apply change: %v", err)
	}

	changeID := graph.EntityID("k8s.rollout", "rev7")
	targetID := graph.EntityID("otel.service.name", "payments")

	var (
		typ    string
		facets []string
	)
	if err := store.Pool().QueryRow(ctx,
		`SELECT type, facets FROM graph.entities WHERE entity_id = $1`, changeID).Scan(&typ, &facets); err != nil {
		t.Fatalf("read change entity: %v", err)
	}
	if typ != "change" {
		t.Errorf("change entity type = %q, want change (change is a node type, not an annotation)", typ)
	}

	var (
		valid  postgres.TimeRange
		change []byte
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid, change FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, changeID).Scan(&valid, &change); err != nil {
		t.Fatalf("read change version: %v", err)
	}
	if !valid.Start.Equal(mustTime("2026-09-01T14:20:00Z")) {
		t.Errorf("change valid start = %s, want 14:20", valid.Start)
	}
	if valid.EndUnbounded {
		t.Error("a change with no stated end must still have a bounded valid interval, " +
			"or every past change would intersect every future diff window")
	}
	if !strings.Contains(string(change), "ROLLOUT") || !strings.Contains(string(change), "alice") {
		t.Errorf("change payload = %s, want the kind, summary and actor (FR-004)", change)
	}

	var edges int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'changed_by' AND upper_inf(observed)`,
		targetID, changeID).Scan(&edges); err != nil {
		t.Fatalf("read changed_by edge: %v", err)
	}
	if edges != 1 {
		t.Errorf("changed_by edges = %d, want 1 from the target to the change", edges)
	}
}

// TestObserveChangeAttachesLater is the edge case: a change that names a node the graph has not
// seen is kept and flagged, and the edge appears by itself when the node does.
func TestObserveChangeAttachesLater(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	if _, err := p.Apply(ctx, changeEvent("rev8", "inventory"), mustTime("2026-09-01T14:21:00Z")); err != nil {
		t.Fatalf("apply change: %v", err)
	}

	changeID := graph.EntityID("k8s.rollout", "rev8")
	var props []byte
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, changeID).Scan(&props); err != nil {
		t.Fatalf("read change version: %v", err)
	}
	if !strings.Contains(string(props), "otel.service.name=inventory") {
		t.Fatalf("props = %s, want the unresolved target recorded, not dropped", props)
	}

	// The node turns up; the edge must appear with both events as its evidence.
	node := upsertSpec{source: 0, entity: "inventory", name: "inventory",
		validAt: mustTime("2026-09-01T13:00:00Z"), props: map[string]string{"service.version": "1.0"}}
	if _, err := p.Apply(ctx, node.event("src:0:inventory"), mustTime("2026-09-01T14:30:00Z")); err != nil {
		t.Fatalf("apply node: %v", err)
	}

	targetID := graph.EntityID("otel.service.name", "inventory")
	var evidence []string
	if err := store.Pool().QueryRow(ctx, `
		SELECT produced_by_event_ids FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'changed_by' AND upper_inf(observed)`,
		targetID, changeID).Scan(&evidence); err != nil {
		t.Fatalf("read changed_by edge: %v", err)
	}
	if len(evidence) != 2 {
		t.Errorf("produced_by_event_ids = %v, want both the change event and the node event (FR-034)", evidence)
	}

	// And the change no longer claims the target is unattached.
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, changeID).Scan(&props); err != nil {
		t.Fatalf("read change version: %v", err)
	}
	if strings.Contains(string(props), projector.UnattachedTargetsProp) {
		t.Errorf("props = %s, want the attached target removed from the unattached list", props)
	}
	// The correction kept the old version (constitution II).
	if total := countRows(t, store,
		`SELECT count(*) FROM graph.entity_versions WHERE change IS NOT NULL`); total != 2 {
		t.Errorf("change versions = %d, want 2: the correction closes the first, never deletes it", total)
	}
}

// TestRetractEdge checks the plain edge retraction path: the current version is closed in
// observed time and replaced by one with a bounded valid interval, with no cascade marker.
func TestRetractEdge(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	for _, name := range []string{"checkout", "payments"} {
		spec := upsertSpec{source: 0, entity: name, name: name,
			validAt: mustTime("2026-09-01T13:00:00Z"), props: map[string]string{"service.version": "1.0"}}
		if _, err := p.Apply(ctx, spec.event("src:0:"+name), mustTime("2026-09-01T13:01:00Z")); err != nil {
			t.Fatalf("apply node %s: %v", name, err)
		}
	}

	edge := &graphv1.EventEnvelope{
		EventId: "src:0:edge", IdempotencyKey: "src:0:edge",
		SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
			Src:     &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
			Dst:     &graphv1.Ref{Namespace: "otel.service.name", Value: "payments"},
			Type:    graphv1.EdgeType_CALLS,
			ValidAt: timestamppb.New(mustTime("2026-09-01T13:00:00Z")),
		}},
	}
	if _, err := p.Apply(ctx, edge, mustTime("2026-09-01T13:02:00Z")); err != nil {
		t.Fatalf("apply edge: %v", err)
	}

	retract := &graphv1.EventEnvelope{
		EventId: "src:0:retract", IdempotencyKey: "src:0:retract",
		SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_RetractEdge{RetractEdge: &graphv1.RetractEdge{
			Src:      &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
			Dst:      &graphv1.Ref{Namespace: "otel.service.name", Value: "payments"},
			Type:     graphv1.EdgeType_CALLS,
			ValidEnd: timestamppb.New(mustTime("2026-09-01T14:10:00Z")),
		}},
	}
	if _, err := p.Apply(ctx, retract, mustTime("2026-09-01T14:11:00Z")); err != nil {
		t.Fatalf("apply retraction: %v", err)
	}

	var (
		valid       postgres.TimeRange
		consequence *string
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid, closed_as_consequence_of FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND upper_inf(observed)`,
		graph.EntityID("otel.service.name", "checkout"),
		graph.EntityID("otel.service.name", "payments")).Scan(&valid, &consequence); err != nil {
		t.Fatalf("read edge: %v", err)
	}
	if valid.EndUnbounded || !valid.End.Equal(mustTime("2026-09-01T14:10:00Z")) {
		t.Errorf("valid = %s, want it bounded at 14:10", valid)
	}
	if consequence != nil {
		t.Errorf("closed_as_consequence_of = %q, want unset: this edge was retracted in its own right", *consequence)
	}
	if total := countRows(t, store, `SELECT count(*) FROM graph.edge_versions`); total != 2 {
		t.Errorf("edge versions = %d, want 2: the retraction adds a version, it does not delete one", total)
	}

	// A retraction naming something the graph has never seen is rejected, not applied
	// (data-model.md "Validation rules": upserts create, retractions may not).
	stray := &graphv1.EventEnvelope{
		EventId: "src:0:stray", IdempotencyKey: "src:0:stray",
		SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_RetractNode{RetractNode: &graphv1.RetractNode{
			Ref:      &graphv1.Ref{Namespace: "otel.service.name", Value: "never-seen"},
			ValidEnd: timestamppb.New(mustTime("2026-09-01T14:10:00Z")),
		}},
	}
	result, err := p.Apply(ctx, stray, mustTime("2026-09-01T14:12:00Z"))
	if err != nil {
		t.Fatalf("apply stray retraction: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED || result.GetReasonCode() != "ref_unresolvable" {
		t.Errorf("status = %s reason = %q, want REJECTED/ref_unresolvable",
			result.GetStatus(), result.GetReasonCode())
	}
	if events := countRows(t, store, `SELECT count(*) FROM log.events WHERE event_id = 'src:0:stray'`); events != 0 {
		t.Error("a rejected retraction was appended to the log")
	}
}

// TestUpsertEdgeCreatesPlaceholderEndpoint checks that an edge whose endpoint nothing has
// described yet is still stored, with the endpoint minted as a placeholder (FR-021).
func TestUpsertEdgeCreatesPlaceholderEndpoint(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	edge := &graphv1.EventEnvelope{
		EventId: "src:0:edge", IdempotencyKey: "src:0:edge",
		SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
			Src:     &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
			Dst:     &graphv1.Ref{Namespace: "server.address", Value: "api.stripe.com"},
			Type:    graphv1.EdgeType_CALLS,
			ValidAt: timestamppb.New(mustTime("2026-09-01T13:00:00Z")),
		}},
	}
	if _, err := p.Apply(ctx, edge, mustTime("2026-09-01T13:02:00Z")); err != nil {
		t.Fatalf("apply edge: %v", err)
	}

	// The namespace decides the provisional type; no facet is recorded, because no source
	// asserted one.
	for ref, wantType := range map[graph.Ref]string{
		{Namespace: "otel.service.name", Value: "checkout"}:    "service",
		{Namespace: "server.address", Value: "api.stripe.com"}: "third_party",
	} {
		var (
			typ    string
			facets []string
		)
		if err := store.Pool().QueryRow(ctx,
			`SELECT type, facets FROM graph.entities WHERE entity_id = $1`,
			graph.EntityID(ref.Namespace, ref.Value)).Scan(&typ, &facets); err != nil {
			t.Fatalf("read %s: %v", ref, err)
		}
		if typ != wantType {
			t.Errorf("%s type = %q, want %q", ref, typ, wantType)
		}
		if len(facets) != 0 {
			t.Errorf("%s facets = %v, want none: a placeholder is an inference, not an assertion", ref, facets)
		}
	}

	var props []byte
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`,
		graph.EntityID("otel.service.name", "checkout")).Scan(&props); err != nil {
		t.Fatalf("read placeholder version: %v", err)
	}
	if !strings.Contains(string(props), projector.PlaceholderProp) {
		t.Errorf("props = %s, want %s", props, projector.PlaceholderProp)
	}

	// The real node arrives and replaces everything its source said before.
	node := upsertSpec{source: 0, entity: "checkout", name: "checkout",
		validAt: mustTime("2026-09-01T13:00:00Z"), props: map[string]string{"service.version": "1.0"}}
	if _, err := p.Apply(ctx, node.event("src:0:checkout"), mustTime("2026-09-01T13:03:00Z")); err != nil {
		t.Fatalf("apply node: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`,
		graph.EntityID("otel.service.name", "checkout")).Scan(&props); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if strings.Contains(string(props), projector.PlaceholderProp) {
		t.Errorf("props = %s, want the placeholder gone once the node was described", props)
	}
}

// TestReplayReproducesTheProjection is FR-023 in miniature: replaying the log into an empty
// projection reproduces it, using the observed times recorded on the rows rather than the
// replay clock.
func TestReplayReproducesTheProjection(t *testing.T) {
	ctx := context.Background()
	dir := "../../fixtures/retraction-with-edges-01"
	m := loadManifest(t, dir)
	events := loadEvents(t, filepath.Join(dir, m.Events))

	live, liveStore := newProjector(t, m)
	applyAll(t, live, events)
	want := snapshot(t, liveStore)

	// A second database with the same log but no projection: copy the log by re-appending
	// through the same events, then project it with Replay alone.
	replayed, replayStore := newProjector(t, m)
	for _, event := range events {
		if _, err := replayed.Log().Append(ctx, event.env, eventlog.AppendOptions{ObservedAt: event.observedAt}); err != nil {
			t.Fatalf("append %s: %v", event.env.GetEventId(), err)
		}
	}
	report, err := replayed.Replay(ctx)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if report.Events != len(events) {
		t.Errorf("replayed %d events, want %d", report.Events, len(events))
	}
	if got := snapshot(t, replayStore); got != want {
		t.Errorf("replay produced a different graph\nlive:\n%s\nreplayed:\n%s", want, got)
	}

	// Replay did not append anything: the events were already in the log.
	if got := countRows(t, replayStore, `SELECT count(*) FROM log.events`); got != len(events) {
		t.Errorf("log holds %d events after replay, want %d: Replay must not re-append", got, len(events))
	}
}

// A change's own properties and pointers reach the graph (004).
//
// They could not before: `ObserveChange` had no field for either, so a feeder that computed them had
// nowhere to put them — feature 003's Cloud Run feeder built the property its contract publishes
// ("the creation change is marked as not having moved production traffic") and then discarded it,
// which left the clause unmet and `sre.gcp.moved_production_traffic` appearing exactly once in the
// repository, in its own declaration.
func TestObserveChangeCarriesItsOwnPropsAndPointers(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	node := upsertSpec{source: 0, entity: "payments", name: "payments",
		validAt: mustTime("2026-09-01T13:00:00Z")}
	if _, err := p.Apply(ctx, node.event("src:0:payments"), mustTime("2026-09-01T13:01:00Z")); err != nil {
		t.Fatalf("apply node: %v", err)
	}

	event := changeEvent("rev8", "payments")
	body := event.GetObserveChange()
	body.Props = mustStruct(t, map[string]any{
		"deployment.environment.name":      "production",
		"sre.gcp.moved_production_traffic": false,
		"sre.github.actor_rung":            "GitHub types the account as a user",
	})
	body.Pointers = []*graphv1.Pointer{{
		Kind:        graphv1.PointerKind_SOURCE_LINK,
		BackendKind: "github",
		Vocabulary:  "github-resource/v1",
		Selector:    "repos/acme/storefront/deployments/4321",
	}}
	if _, err := p.Apply(ctx, event, mustTime("2026-09-01T14:21:00Z")); err != nil {
		t.Fatalf("apply change: %v", err)
	}

	changeID := graph.EntityID("k8s.rollout", "rev8")
	var props, pointers []byte
	if err := store.Pool().QueryRow(ctx, `
		SELECT props, pointers FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, changeID).Scan(&props, &pointers); err != nil {
		t.Fatalf("read the change version: %v", err)
	}

	// One record per key, not a list: a change is a RECORD node, and a record has one version
	// asserted by one event rather than a fact several sources corroborate.
	var stored map[string]struct {
		Value    any    `json:"value"`
		SourceID string `json:"source_id"`
		EventID  string `json:"event_id"`
	}
	if err := json.Unmarshal(props, &stored); err != nil {
		t.Fatalf("decode props: %v", err)
	}
	for key, want := range map[string]any{
		"deployment.environment.name":      "production",
		"sre.gcp.moved_production_traffic": false,
	} {
		record, ok := stored[key]
		if !ok {
			t.Errorf("the change carries no %q; a property a feeder computed and the graph dropped is a "+
				"property no query can ever read", key)
			continue
		}
		if record.Value != want {
			t.Errorf("%s = %v, want %v", key, record.Value, want)
		}
		if record.EventID != "rev8" {
			t.Errorf("%s is attributed to event %q, want the event that asserted it", key, record.EventID)
		}
	}
	// `false` in particular: FR-016 wants the creation change MARKED as not having moved traffic,
	// which is a positive statement, and a property dropped because it was falsy would read as "we did
	// not say".
	if record, ok := stored["sre.gcp.moved_production_traffic"]; !ok || record.Value == nil {
		t.Error("a false property was dropped or stored as null; the mark is a positive statement and a " +
			"property omitted because it was falsy would read as `we did not say`")
	}

	if !strings.Contains(string(pointers), "repos/acme/storefront/deployments/4321") {
		t.Errorf("the change carries no pointers (%s); the projector has always had the column and a "+
			"change observation never filled it", pointers)
	}
}

// The projector's own account of what it could not resolve wins over a feeder claiming the same key.
// A feeder writing it is a feeder that thinks it resolves targets, and the graph has no way to check
// such a claim — but refusing the whole event would lose a change that really happened.
func TestAFeederCannotOverwriteTheUnattachedTargetList(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	event := changeEvent("rev9", "never-upserted")
	event.GetObserveChange().Props = mustStruct(t, map[string]any{
		projector.UnattachedTargetsProp: []any{"a lie"},
		"deployment.environment.name":   "production",
	})
	if _, err := p.Apply(ctx, event, mustTime("2026-09-01T14:21:00Z")); err != nil {
		t.Fatalf("apply change: %v", err)
	}

	var props []byte
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, graph.EntityID("k8s.rollout", "rev9")).Scan(&props); err != nil {
		t.Fatalf("read the change version: %v", err)
	}
	if strings.Contains(string(props), "a lie") {
		t.Errorf("a feeder overwrote the graph's account of its own resolution: %s", props)
	}
	if !strings.Contains(string(props), "never-upserted") {
		t.Errorf("the projector's unattached list is missing: %s", props)
	}
	if !strings.Contains(string(props), "production") {
		t.Errorf("the feeder's other properties were dropped along with the refused one: %s", props)
	}

	// And the case that actually needs the guard: every target resolved, so the projector writes no
	// unattached list of its own and a feeder's claim would be the only thing in the key. The first
	// version of this test used an unresolvable target, where the projector's own assignment happens to
	// overwrite the lie — so the guard could be deleted and the test still passed.
	node := upsertSpec{source: 0, entity: "resolved", name: "resolved",
		validAt: mustTime("2026-09-01T13:00:00Z")}
	if _, err := p.Apply(ctx, node.event("src:0:resolved"), mustTime("2026-09-01T13:01:00Z")); err != nil {
		t.Fatalf("apply node: %v", err)
	}
	clean := changeEvent("rev10", "resolved")
	clean.GetObserveChange().Props = mustStruct(t, map[string]any{
		projector.UnattachedTargetsProp: []any{"a lie nobody can check"},
	})
	if _, err := p.Apply(ctx, clean, mustTime("2026-09-01T14:22:00Z")); err != nil {
		t.Fatalf("apply change: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, graph.EntityID("k8s.rollout", "rev10")).Scan(&props); err != nil {
		t.Fatalf("read the change version: %v", err)
	}
	if strings.Contains(string(props), "a lie nobody can check") {
		t.Errorf("with every target resolved, a feeder's claim about what the graph could not resolve "+
			"survived: %s", props)
	}
}

func mustStruct(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	out, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("build struct: %v", err)
	}
	return out
}
