// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The five event bodies the feature 001 change package adds (ADR-0005 D2/D3, 002 FR-035,
// FR-057a/b/e), projected end to end against a real database.
//
// What each test is really checking is a rule that would be easy to break quietly:
//
//   - a decision record is a bounded node with edges to what it was about, and no telemetry;
//   - a reopen links a new investigation to the old one and does NOT touch the old one;
//   - a label records a human judgement without editing the conclusion it judges;
//   - a human fact reaches the graph as a relationship, not as a rewrite;
//   - an alert transition is an ordinary node assertion, so a second one segments the first
//     instead of colliding with it.

func investigationEvent(id string, body any) *graphv1.EventEnvelope {
	env := &graphv1.EventEnvelope{
		EventId:       id,
		SourceId:      propSourceID(0),
		SchemaVersion: "1.0.0",
	}
	switch b := body.(type) {
	case *graphv1.RecordInvestigation:
		env.Body = &graphv1.EventEnvelope_RecordInvestigation{RecordInvestigation: b}
	case *graphv1.SubmitHumanFact:
		env.Body = &graphv1.EventEnvelope_SubmitHumanFact{SubmitHumanFact: b}
	case *graphv1.ReopenInvestigation:
		env.Body = &graphv1.EventEnvelope_ReopenInvestigation{ReopenInvestigation: b}
	case *graphv1.LabelInvestigation:
		env.Body = &graphv1.EventEnvelope_LabelInvestigation{LabelInvestigation: b}
	case *graphv1.AlertTransition:
		env.Body = &graphv1.EventEnvelope_AlertTransition{AlertTransition: b}
	}
	return env
}

func at(s string) *timestamppb.Timestamp { return timestamppb.New(mustTime(s)) }

func conclusion(id string) *graphv1.RecordInvestigation {
	return &graphv1.RecordInvestigation{
		InvestigationId: id,
		Subjects:        []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout"}},
		TargetEntities:  []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
		StartedAt:       at("2026-09-01T14:32:00Z"),
		EndedAt:         at("2026-09-01T14:36:30Z"),
		Outcome:         "ranked",
		StopReason:      "diminishing_returns",
		VerdictLine:     "the payments rollout to rev7 is the most likely cause",
		Hypotheses: mustProtoStruct(map[string]any{
			"sre.investigation.hypotheses": map[string]any{
				"h1": map[string]any{"status": "supported", "confidence": 0.72, "bucket": "likely"},
			},
		}),
		RecordingKey:    id,
		RecordingDigest: "9f2c7a1b4e5d6c8a0b1c2d3e4f5061728394a5b6c7d8e9f0a1b2c3d4e5f60718",
		Requester:       "sre@example.com",
	}
}

func TestRecordInvestigationWritesABoundedDecisionRecord(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, investigationEvent("inv:e1", conclusion("inv-0007")), "2026-09-01T14:36:31Z")

	entityID := projector.InvestigationRef("inv-0007").EntityID()
	var (
		typ    string
		facets []string
	)
	if err := store.Pool().QueryRow(ctx,
		`SELECT type, facets FROM graph.entities WHERE entity_id = $1`, entityID).Scan(&typ, &facets); err != nil {
		t.Fatalf("read investigation entity: %v", err)
	}
	if typ != string(graph.NodeTypeInvestigation) {
		t.Errorf("entity type = %q, want investigation", typ)
	}

	var (
		valid postgres.TimeRange
		props map[string]json.RawMessage
		name  string
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid, props, display_name FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, entityID).Scan(&valid, &props, &name); err != nil {
		t.Fatalf("read investigation version: %v", err)
	}
	if !valid.Start.Equal(mustTime("2026-09-01T14:32:00Z")) || valid.EndUnbounded ||
		!valid.End.Equal(mustTime("2026-09-01T14:36:30Z")) {
		t.Errorf("valid = %v, want the bounded run interval [14:32, 14:36:30)", valid)
	}
	for _, key := range []string{
		projector.InvestigationOutcomeProp,
		projector.InvestigationStopReasonProp,
		projector.InvestigationVerdictProp,
		projector.InvestigationDigestProp,
		projector.InvestigationHypothesesProp,
	} {
		if _, ok := props[key]; !ok {
			t.Errorf("the decision record does not carry %s", key)
		}
	}
	if name != "the payments rollout to rev7 is the most likely cause" {
		t.Errorf("display_name = %q, want the verdict line", name)
	}

	// INVESTIGATED edges to both the subject and the target, over the run's interval.
	for _, service := range []string{"checkout", "payments"} {
		dst := graph.EntityID("otel.service.name", service)
		var edgeValid postgres.TimeRange
		if err := store.Pool().QueryRow(ctx, `
			SELECT valid FROM graph.edge_versions
			WHERE src_id = $1 AND dst_id = $2 AND type = 'investigated' AND upper_inf(observed)`,
			entityID, dst).Scan(&edgeValid); err != nil {
			t.Fatalf("read investigated edge to %s: %v", service, err)
		}
		if !edgeValid.Start.Equal(mustTime("2026-09-01T14:32:00Z")) {
			t.Errorf("investigated edge to %s starts at %s, want the run's start", service, edgeValid.Start)
		}
	}

	if err := projector.CheckInvariants(ctx, store); err != nil {
		t.Errorf("invariants broken by the new edge kind: %v", err)
	}
}

// TestReopenDoesNotRewriteTheParent is 002 FR-057e and the reason reopen exists as its own
// event: a concluded investigation is immutable, so the revised answer is a new investigation
// linked to the old one, and the old one's version is byte-for-byte what it was.
func TestReopenDoesNotRewriteTheParent(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, investigationEvent("inv:e1", conclusion("inv-0007")), "2026-09-01T14:36:31Z")

	parentID := projector.InvestigationRef("inv-0007").EntityID()
	before := versionIDs(t, store, parentID)

	apply(t, p, investigationEvent("inv:e2", &graphv1.ReopenInvestigation{
		ParentInvestigationId: "inv-0007",
		ChildInvestigationId:  "inv-0008",
		Cause:                 "human_fact",
		CauseRef:              "fact-0001",
	}), "2026-09-01T15:10:00Z")

	if after := versionIDs(t, store, parentID); !equalStrings(before, after) {
		t.Errorf("the parent's versions changed from %v to %v; a concluded investigation is immutable",
			before, after)
	}

	childID := projector.InvestigationRef("inv-0008").EntityID()
	var props map[string]json.RawMessage
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'investigated' AND upper_inf(observed)`,
		childID, parentID).Scan(&props); err != nil {
		t.Fatalf("read the reopen link: %v", err)
	}
	if _, ok := props[projector.InvestigationReopenCause]; !ok {
		t.Errorf("the reopen link does not record its cause: %v", props)
	}

	// The child exists as an investigation from the moment it is named, before it concludes.
	var typ string
	if err := store.Pool().QueryRow(ctx,
		`SELECT type FROM graph.entities WHERE entity_id = $1`, childID).Scan(&typ); err != nil {
		t.Fatalf("read child entity: %v", err)
	}
	if typ != string(graph.NodeTypeInvestigation) {
		t.Errorf("child entity type = %q, want investigation", typ)
	}
}

// TestLabelRecordsAJudgementWithoutEditingTheConclusion: the label adds props in a new version
// of the same valid interval, so the conclusion is unchanged and what the graph said before the
// label is still readable as-known-at (constitution II).
func TestLabelRecordsAJudgementWithoutEditingTheConclusion(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, investigationEvent("inv:e1", conclusion("inv-0007")), "2026-09-01T14:36:31Z")
	apply(t, p, investigationEvent("inv:e2", &graphv1.LabelInvestigation{
		InvestigationId: "inv-0007",
		WasThisRight:    true,
		Author:          "sre@example.com",
		LabelledAt:      at("2026-09-01T15:02:00Z"),
	}), "2026-09-01T15:02:01Z")

	entityID := projector.InvestigationRef("inv-0007").EntityID()
	var (
		valid postgres.TimeRange
		props map[string]json.RawMessage
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid, props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, entityID).Scan(&valid, &props); err != nil {
		t.Fatalf("read labelled version: %v", err)
	}
	if !valid.Start.Equal(mustTime("2026-09-01T14:32:00Z")) {
		t.Errorf("the label moved the run's interval to %v", valid)
	}
	for _, key := range []string{
		projector.InvestigationLabelProp, projector.InvestigationLabelledByProp,
		projector.InvestigationVerdictProp, projector.InvestigationOutcomeProp,
	} {
		if _, ok := props[key]; !ok {
			t.Errorf("the labelled version lost %s", key)
		}
	}

	var superseded int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM graph.entity_versions
		WHERE entity_id = $1 AND NOT upper_inf(observed)`, entityID).Scan(&superseded); err != nil {
		t.Fatalf("count superseded versions: %v", err)
	}
	if superseded != 1 {
		t.Errorf("superseded versions = %d, want the pre-label conclusion kept", superseded)
	}
}

// TestLabelBeforeTheConclusionSurvivesIt is the order-independence half (FR-021): a label that
// arrives before the record it labels is carried forward by the record rather than overwritten.
func TestLabelBeforeTheConclusionSurvivesIt(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, investigationEvent("inv:e2", &graphv1.LabelInvestigation{
		InvestigationId: "inv-0007",
		WasThisRight:    false,
		Author:          "sre@example.com",
		LabelledAt:      at("2026-09-01T15:02:00Z"),
	}), "2026-09-01T15:02:01Z")
	apply(t, p, investigationEvent("inv:e1", conclusion("inv-0007")), "2026-09-01T15:03:00Z")

	entityID := projector.InvestigationRef("inv-0007").EntityID()
	var props map[string]json.RawMessage
	if err := store.Pool().QueryRow(ctx, `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, entityID).Scan(&props); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, ok := props[projector.InvestigationLabelProp]; !ok {
		t.Error("the conclusion dropped the label that preceded it")
	}
	if _, ok := props[projector.InvestigationVerdictProp]; !ok {
		t.Error("the conclusion did not land")
	}
}

// TestHumanFactBecomesAConcernsEdge: the fact reaches the graph as a relationship over the
// interval it concerns, carrying its author, and the investigation's own version is untouched.
func TestHumanFactBecomesAConcernsEdge(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, investigationEvent("inv:e1", conclusion("inv-0007")), "2026-09-01T14:36:31Z")
	entityID := projector.InvestigationRef("inv-0007").EntityID()
	before := versionIDs(t, store, entityID)

	apply(t, p, investigationEvent("inv:e2", &graphv1.SubmitHumanFact{
		InvestigationId: "inv-0007",
		Kind:            "observation",
		Statement:       "the canary is still on the old build",
		Entities:        []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
		ConcernsFrom:    at("2026-09-01T14:00:00Z"),
		ConcernsTo:      at("2026-09-01T15:00:00Z"),
		Author:          "sre@example.com",
		SubmittedAt:     at("2026-09-01T14:41:00Z"),
	}), "2026-09-01T14:41:01Z")

	if after := versionIDs(t, store, entityID); !equalStrings(before, after) {
		t.Error("a human fact must not rewrite the investigation it is pushed at")
	}

	var (
		valid      postgres.TimeRange
		props      map[string]json.RawMessage
		producedBy []string
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid, props, produced_by_event_ids FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'concerns' AND upper_inf(observed)`,
		entityID, graph.EntityID("otel.service.name", "payments")).Scan(&valid, &props, &producedBy); err != nil {
		t.Fatalf("read concerns edge: %v", err)
	}
	if !valid.Start.Equal(mustTime("2026-09-01T14:00:00Z")) || valid.EndUnbounded ||
		!valid.End.Equal(mustTime("2026-09-01T15:00:00Z")) {
		t.Errorf("concerns valid = %v, want the interval the fact named", valid)
	}
	if _, ok := props[projector.InvestigationFactAuthorProp]; !ok {
		t.Error("the concerns edge does not name the author; a fact nobody signed is not evidence")
	}
	if len(producedBy) != 1 || producedBy[0] != "inv:e2" {
		t.Errorf("produced_by = %v, want the fact's own event", producedBy)
	}

	// A second fact about the same entity over an overlapping interval widens the one
	// relationship rather than colliding with it.
	apply(t, p, investigationEvent("inv:e3", &graphv1.SubmitHumanFact{
		InvestigationId: "inv-0007",
		Kind:            "correction",
		Statement:       "it was rolled back at 15:30",
		Entities:        []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
		ConcernsFrom:    at("2026-09-01T14:50:00Z"),
		ConcernsTo:      at("2026-09-01T15:30:00Z"),
		Author:          "sre@example.com",
		SubmittedAt:     at("2026-09-01T15:31:00Z"),
	}), "2026-09-01T15:31:01Z")

	var merged postgres.TimeRange
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'concerns' AND upper_inf(observed)`,
		entityID, graph.EntityID("otel.service.name", "payments")).Scan(&merged); err != nil {
		t.Fatalf("read merged concerns edge: %v", err)
	}
	if !merged.Start.Equal(mustTime("2026-09-01T14:00:00Z")) || !merged.End.Equal(mustTime("2026-09-01T15:30:00Z")) {
		t.Errorf("merged concerns valid = %v, want the union [14:00, 15:30)", merged)
	}
	if err := projector.CheckInvariants(ctx, store); err != nil {
		t.Errorf("invariants broken: %v", err)
	}
}

func alertEvent(id, from, to, when string) *graphv1.EventEnvelope {
	return investigationEvent(id, &graphv1.AlertTransition{
		Monitor:      &graphv1.Ref{Namespace: "datadog.monitor", Value: "42"},
		GroupKey:     "env:prod,service:checkout",
		TransitionAt: at(when),
		FromState:    from,
		ToState:      to,
		Watches:      []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout"}},
		Transport:    "poll",
		Severity:     "sev2",
		Title:        "checkout 5xx rate above 2%",
	})
}

// TestAlertTransitionIsAnOrdinaryNodeAssertion: the alert's state is a fact from the transition
// instant until the next one contradicts it, so two transitions produce two segments rather
// than an overlap — which is exactly what would have gone wrong had this been projected by hand
// instead of through the machinery that has segmented facts since 001.
func TestAlertTransitionIsAnOrdinaryNodeAssertion(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	apply(t, p, alertEvent("alert:e1", "ok", "alert", "2026-09-01T14:21:00Z"), "2026-09-01T14:21:05Z")
	apply(t, p, alertEvent("alert:e2", "alert", "ok", "2026-09-01T14:38:00Z"), "2026-09-01T14:38:05Z")

	alertID := graph.EntityID("datadog.monitor", "42")
	rows, err := store.Pool().Query(ctx, `
		SELECT valid, props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) ORDER BY lower(valid)`, alertID)
	if err != nil {
		t.Fatalf("read alert versions: %v", err)
	}
	defer rows.Close()

	type version struct {
		valid      postgres.TimeRange
		state      string
		unattached any
	}
	var versions []version
	for rows.Next() {
		var (
			valid postgres.TimeRange
			// `any` rather than `string`: a node's props are heterogeneous by design — the
			// unattached-target and unattached-watches lists are arrays — so a scan target that
			// only admits strings fails on a node that carries either.
			props map[string]struct {
				Value any `json:"value"`
			}
		)
		if err := rows.Scan(&valid, &props); err != nil {
			t.Fatalf("scan alert version: %v", err)
		}
		state, _ := props[projector.AlertStateProp].Value.(string)
		versions = append(versions, version{
			valid:      valid,
			state:      state,
			unattached: props[projector.AlertUnattachedWatchesProp].Value,
		})
	}
	if len(versions) != 2 {
		t.Fatalf("alert versions = %d, want two segments (alerting, then recovered)", len(versions))
	}
	if versions[0].state != "alert" || versions[1].state != "ok" {
		t.Errorf("states = %q then %q, want alert then ok: the recovery is kept, it is evidence",
			versions[0].state, versions[1].state)
	}
	if versions[0].valid.EndUnbounded || !versions[0].valid.End.Equal(mustTime("2026-09-01T14:38:00Z")) {
		t.Errorf("the first segment runs to %v, want it cut at the recovery", versions[0].valid)
	}

	// WATCHES, which 003 FR-048 changed the timing of.
	//
	// Nothing has described `checkout` yet, and an alert whose target cannot be resolved is now
	// KEPT AND MARKED UNATTACHED rather than given an edge to a minted placeholder. So there is no
	// edge yet, and the alert says so.
	//
	// The old behaviour created the edge immediately against a placeholder entity — one with no type
	// and no description. That looked harmless and was not: an investigation walking WATCHES out of
	// a firing alert could not tell "watches a service we know about" from "watches a name we have
	// never seen", because both are an edge to an entity.
	checkoutID := graph.EntityID("otel.service.name", "checkout")
	var edges int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'watches' AND upper_inf(observed)`,
		alertID, checkoutID).Scan(&edges); err != nil {
		t.Fatalf("count watches edges: %v", err)
	}
	if edges != 0 {
		t.Errorf("%d watches edge(s) to an entity nothing has described; 003 FR-048 requires the "+
			"target be marked unattached instead", edges)
	}
	// Asserted across the current versions rather than on one of them: which segment of a
	// segmented node carries a property is 001's planning decision, and pinning it here would make
	// this test fail on a change to segmentation that has nothing to do with FR-048.
	var named bool
	for _, v := range versions {
		list, _ := v.unattached.([]any)
		if len(list) == 1 && list[0] == "otel.service.name=checkout" {
			named = true
		}
	}
	if !named {
		t.Errorf("no current alert version names the unresolved target; the gap is not stated "+
			"anywhere, which is what FR-048 requires. versions = %#v", versions)
	}

	// And it attaches the moment the target appears — dated from the FIRST transition instant, not
	// from the discovery. The alert has been watching since it was configured to; dating the edge
	// from when the graph happened to learn about the target would be an arrival-order artefact of
	// exactly the kind FR-021 forbids.
	apply(t, p, &graphv1.EventEnvelope{
		EventId: "node:e1", IdempotencyKey: "node:e1", SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: "checkout",
			ValidAt:     at("2026-09-01T14:00:00Z"),
		}},
	}, "2026-09-01T14:45:00Z")

	var watchValid postgres.TimeRange
	var producedBy []string
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid, produced_by_event_ids FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'watches' AND upper_inf(observed)`,
		alertID, checkoutID).Scan(&watchValid, &producedBy); err != nil {
		t.Fatalf("read watches edge after the target appeared: %v", err)
	}
	// Two-sided provenance: the transitions that named the target and the event that created it
	// (FR-048, mirroring FR-034 for changes).
	if len(producedBy) < 2 {
		t.Errorf("produced_by_event_ids = %v, want both halves: the transition(s) that named the "+
			"target and the event that finally created it", producedBy)
	}
	if !slices.Contains(producedBy, "node:e1") {
		t.Errorf("produced_by_event_ids = %v, missing the event that created the target", producedBy)
	}
	if !watchValid.Start.Equal(mustTime("2026-09-01T14:21:00Z")) {
		t.Errorf("watches edge starts at %s, want the first transition instant", watchValid.Start)
	}
	if err := projector.CheckInvariants(ctx, store); err != nil {
		t.Errorf("invariants broken by the new edge kind: %v", err)
	}
}

// TestAlertTransitionIsIdempotentOnTheFourTuple is 002 FR-008b end to end: a webhook and the
// poll behind it carry different event ids and are still one event, because the key is derived
// from the published 4-tuple rather than supplied by the transport.
func TestAlertTransitionIsIdempotentOnTheFourTuple(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	first := alertEvent("alert:webhook", "ok", "alert", "2026-09-01T14:21:00Z")
	second := alertEvent("alert:poll", "ok", "alert", "2026-09-01T14:21:00Z")

	if got := apply(t, p, first, "2026-09-01T14:21:05Z"); got != graphv1.IngestResult_APPLIED {
		t.Fatalf("first delivery = %s, want APPLIED", got)
	}
	result, err := p.Apply(ctx, second, mustTime("2026-09-01T14:21:30Z"))
	if err != nil {
		t.Fatalf("apply the poll: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Errorf("the poll behind the webhook = %s, want DUPLICATE_NOOP", result.GetStatus())
	}

	var versions int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM graph.entity_versions WHERE entity_id = $1`,
		graph.EntityID("datadog.monitor", "42")).Scan(&versions); err != nil {
		t.Fatalf("count alert versions: %v", err)
	}
	if versions != 1 {
		t.Errorf("alert versions = %d, want one: two transports, one transition", versions)
	}
}

// TestInvestigationEventsSurviveReplay is FR-023 for the new bodies: the whole point of
// putting a decision record in the log is that rebuilding the graph from the log reproduces it,
// down to the evidence arrays. An alert transition is the interesting one — it is read back out
// of the log as a node and edge assertion, so a replay exercises that path rather than the
// in-memory one.
func TestInvestigationEventsSurviveReplay(t *testing.T) {
	ctx := context.Background()
	events := []struct {
		env        *graphv1.EventEnvelope
		observedAt string
	}{
		{alertEvent("alert:e1", "ok", "alert", "2026-09-01T14:21:00Z"), "2026-09-01T14:21:05Z"},
		{alertEvent("alert:e2", "alert", "ok", "2026-09-01T14:38:00Z"), "2026-09-01T14:38:05Z"},
		{investigationEvent("inv:e1", conclusion("inv-0007")), "2026-09-01T14:36:31Z"},
		{investigationEvent("inv:e2", &graphv1.SubmitHumanFact{
			InvestigationId: "inv-0007",
			Kind:            "observation",
			Statement:       "the canary is still on the old build",
			Entities:        []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
			ConcernsFrom:    at("2026-09-01T14:00:00Z"),
			Author:          "sre@example.com",
			SubmittedAt:     at("2026-09-01T14:41:00Z"),
		}), "2026-09-01T14:41:01Z"},
		{investigationEvent("inv:e3", &graphv1.ReopenInvestigation{
			ParentInvestigationId: "inv-0007",
			ChildInvestigationId:  "inv-0008",
			Cause:                 "human_fact",
			CauseRef:              "inv:e2",
		}), "2026-09-01T15:10:00Z"},
		{investigationEvent("inv:e4", &graphv1.LabelInvestigation{
			InvestigationId: "inv-0007",
			WasThisRight:    true,
			Author:          "sre@example.com",
			LabelledAt:      at("2026-09-01T15:02:00Z"),
		}), "2026-09-01T15:11:00Z"},
	}

	live := openStore(t)
	liveProjector := projector.New(live)
	for _, event := range events {
		apply(t, liveProjector, event.env, event.observedAt)
	}
	assertInvariants(t, live, "after applying the investigation events")

	// A replay starts from a log that has the events and a projection that has nothing, which
	// is what the fixture harness reproduces (replay_batch_test.go).
	rebuilt := openStore(t)
	replayed := projector.New(rebuilt)
	for _, event := range events {
		result, err := replayed.Log().Append(ctx, event.env,
			eventlog.AppendOptions{ObservedAt: mustTime(event.observedAt)})
		if err != nil {
			t.Fatalf("append %s: %v", event.env.GetEventId(), err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("append %s: %s (%s)", event.env.GetEventId(),
				result.GetStatus(), result.GetReasonDetail())
		}
	}
	if _, err := replayed.Replay(ctx); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if want, got := snapshot(t, live), snapshot(t, rebuilt); want != got {
		t.Errorf("replay produced a different valid-time state:\n%s", firstDifferingLine(want, got))
	}
	if want, got := provenance(t, live), provenance(t, rebuilt); want != got {
		t.Errorf("replay produced different evidence:\n%s", firstDifferingLine(want, got))
	}
}

func apply(t *testing.T, p *projector.Projector, env *graphv1.EventEnvelope, observedAt string) graphv1.IngestResult_Status {
	t.Helper()
	result, err := p.Apply(context.Background(), env, mustTime(observedAt))
	if err != nil {
		t.Fatalf("apply %s: %v", env.GetEventId(), err)
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		t.Fatalf("apply %s rejected: %s %s", env.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return result.GetStatus()
}

func versionIDs(t *testing.T, store *postgres.Store, entityID string) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT version_id FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) ORDER BY version_id`, entityID)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustProtoStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
