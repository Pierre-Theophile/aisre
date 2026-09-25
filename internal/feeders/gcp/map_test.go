// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The emission path end to end (T046, T051–T056): what one poll actually puts in the graph.

// servicePayload renders a Cloud Run service list response as the fixture format stores it.
func servicePayload(t *testing.T, services ...string) []byte {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(services))
	for _, svc := range services {
		raw = append(raw, json.RawMessage(svc))
	}
	body, err := json.Marshal(map[string]any{"services": raw})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

const checkoutService = `{
  "name": "projects/nova-production/locations/europe-west1/services/checkout",
  "uid": "uid-1",
  "generation": "7",
  "observedGeneration": "7",
  "createTime": "2026-09-01T09:00:00Z",
  "updateTime": "2026-09-21T14:18:00Z",
  "labels": {"team": "payments", "environment": "production", "jira-ticket": "PLAT-4412"},
  "latestReadyRevision": "projects/nova-production/locations/europe-west1/services/checkout/revisions/checkout-00041-xyz",
  "latestCreatedRevision": "projects/nova-production/locations/europe-west1/services/checkout/revisions/checkout-00042-abc",
  "trafficStatuses": [{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": "checkout-00041-xyz", "percent": 100}],
  "traffic": [{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": "checkout-00042-abc", "percent": 100}],
  "template": {"containers": [{"image": "europe-docker.pkg.dev/p/r/checkout@sha256:abc", "env": [{"name": "OTEL_SERVICE_NAME", "value": "checkout"}]}]}
}`

// One service poll asserts the node, every claim including the one it is addressed by, the owner and
// its edge, and the pointers — and it asserts the *observed* split.
func TestOneServicePollAssertsTheNodeItsClaimsAndItsOwner(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	at := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: at, Bytes: servicePayload(t, checkoutService)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	node := firstNode(em.events, gcpfeeder.NSService)
	if node == nil {
		t.Fatal("no SERVICE node was asserted")
	}
	if node.GetRef().GetValue() != "nova-production/europe-west1/checkout" {
		t.Fatalf("the node is addressed as %q", node.GetRef().GetValue())
	}
	props := node.GetProps().GetFields()
	if got := props[gcpfeeder.PropTrafficSplit].GetListValue().GetValues(); len(got) != 1 ||
		got[0].GetStringValue() != "checkout-00041-xyz=100" {
		t.Fatalf("the split is %v; the desired `traffic` field says checkout-00042-abc=100 and must "+
			"not reach the graph", got)
	}
	if props[gcpfeeder.PropEnvironment].GetStringValue() != "production" {
		t.Errorf("environment = %q", props[gcpfeeder.PropEnvironment].GetStringValue())
	}
	if _, leaked := props["sre.gcp.label.jira-ticket"]; leaked {
		t.Error("an unlisted label reached the node's properties (FR-124)")
	}
	if props[gcpfeeder.PropTracePointerAbsent].GetStringValue() == "" {
		t.Error("the absence of a trace pointer is not stated on the node (FR-085)")
	}
	if len(node.GetPointers()) == 0 {
		t.Error("the node carries no pointers (FR-079)")
	}

	// The addressing ref is claimed, and so is the declared OTel name.
	claims := claimValues(em.events)
	if !claims["gcp.cloudrun.service=nova-production/europe-west1/checkout"] {
		t.Error("the addressing ref was not claimed (FR-115)")
	}
	if !claims["otel.service.name=checkout"] {
		t.Error("the declared OpenTelemetry service name was not claimed (FR-118)")
	}

	// The owner, with its valid start as a stated **bound** (FR-125 as revised 2026-09-21): the
	// earliest instant the labelled service state is known to have held, which is the service's own
	// createTime — and marked as a bound, because the instant the team came to own the service is not
	// a fact a label carries.
	owner := firstNode(em.events, feeder.NSOwnerTeam)
	if owner == nil {
		t.Fatal("no OWNER node was asserted (FR-125)")
	}
	wantBound := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC) // the service's createTime
	if got := owner.GetValidAt().AsTime(); !got.Equal(wantBound) {
		t.Errorf("the OWNER node's valid start is %s, want the owned service's createTime %s; the "+
			"bound is the earliest instant the labelled state is known to have held", got, wantBound)
	}
	if owner.GetValidFromUnknown() {
		t.Error("the OWNER node leaves its valid start unknown. That is unsatisfiable alongside the " +
			"graph's placeholder rule: an owned-by edge and both its endpoints must agree on the flag, " +
			"and the owned Cloud Run service has a documented createTime (FR-125, revised)")
	}
	// The bound is marked as a bound, or a reader takes it for the day the team took the service on.
	if reason := owner.GetProps().GetFields()[gcpfeeder.PropOwnerValidFromIsABound].GetStringValue(); reason == "" {
		t.Errorf("the OWNER node does not record that its valid start is a bound (FR-125): %v",
			owner.GetProps().GetFields())
	}
	if !hasEdge(em.events, graphv1.EdgeType_OWNED_BY) {
		t.Error("no owned-by edge was asserted")
	}
	// The edge agrees with both endpoints on the flag, which is the whole point of the revision.
	for _, ev := range em.events {
		if edge := ev.GetUpsertEdge(); edge != nil && edge.GetType() == graphv1.EdgeType_OWNED_BY {
			if edge.GetValidFromUnknown() {
				t.Error("the owned-by edge leaves its start unknown while its endpoints state one; " +
					"an edge-minted placeholder inherits the edge's flag, so the entity would read " +
					"differently according to whether the edge or its endpoint arrived first")
			}
			if got := edge.GetValidAt().AsTime(); !got.Equal(wantBound) {
				t.Errorf("the owned-by edge's valid start is %s, want %s", got, wantBound)
			}
		}
	}

	// The first observation of a service emits no rollout: there is nothing to have changed from,
	// and emitting one would claim a rollout at the instant the connector was first run.
	//
	// Both halves are asserted. Losing the guard does not emit a *change* — there is no audit entry
	// to date one — it emits a spurious **hold**, which would then sit in every checkpoint claiming
	// an undated traffic shift for a service that never moved.
	if n := countChanges(em.events); n != 0 {
		t.Errorf("the first poll emitted %d changes", n)
	}
	if held := f.Held(); len(held) != 0 {
		t.Errorf("the first poll held %d traffic shifts: %v. There is nothing to have changed from, "+
			"so a hold here would claim an undated shift for a service that never moved", len(held), held)
	}
}

// A second poll whose split differs holds the shift rather than dating it from the poll: the instant
// is the audit completion entry's timestamp and nothing else.
func TestASplitChangeAcrossTwoPollsIsHeldUntilTheAuditEntryArrives(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	moved := strings.Replace(checkoutService,
		`"revision": "checkout-00041-xyz", "percent": 100`,
		`"revision": "checkout-00042-abc", "percent": 100`, 1)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 19, 0, 0, time.UTC),
			Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 21, 0, 0, time.UTC),
			Bytes: servicePayload(t, moved)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 21, 0, 0, time.UTC),
			Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countChanges(em.events); n != 0 {
		t.Fatalf("%d rollout changes were emitted with no audit completion entry; the instant is the "+
			"operation.last entry's timestamp, and a guessed one breaks SC-002 silently", n)
	}
	held := f.Held()
	if len(held) != 1 {
		t.Fatalf("held = %d, want 1", len(held))
	}
	if held[0].Before.String() != "checkout-00041-xyz=100" || held[0].After.String() != "checkout-00042-abc=100" {
		t.Errorf("the hold does not carry both sides: %s -> %s", held[0].Before, held[0].After)
	}
}

// A revision poll asserts the node, its claims, its runs-on edge and its creation change — at the
// API's `createTime`, marked as not having moved traffic.
func TestOneRevisionPollAssertsItsCreationChangeAtItsCreateTime(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	const revision = `{
      "name": "projects/nova-production/locations/europe-west1/services/checkout/revisions/checkout-00042-abc",
      "uid": "rev-uid-1",
      "createTime": "2026-09-21T14:18:00Z",
      "labels": {"team": "payments"},
      "containers": [{"image": "europe-docker.pkg.dev/p/r/checkout@sha256:abc"}],
      "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]
    }`
	body, err := json.Marshal(map[string]any{"revisions": []json.RawMessage{json.RawMessage(revision)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadRevisions, At: time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC), Bytes: body},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	node := firstNode(em.events, gcpfeeder.NSRevision)
	if node == nil {
		t.Fatal("no WORKLOAD node was asserted for the revision")
	}
	if node.GetType() != graphv1.NodeType_WORKLOAD {
		t.Errorf("node type = %s, want WORKLOAD", node.GetType())
	}
	if !hasEdge(em.events, graphv1.EdgeType_RUNS_ON) {
		t.Error("no runs-on edge from the revision to its service")
	}

	change := firstChange(em.events)
	if change == nil {
		t.Fatal("no creation change was emitted (FR-016)")
	}
	want := time.Date(2026, 9, 21, 14, 18, 0, 0, time.UTC)
	if got := change.GetValidAt().AsTime(); !got.Equal(want) {
		t.Fatalf("the creation change is valid at %s, want the revision's createTime %s", got, want)
	}
	if change.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT {
		t.Errorf("kind = %s, want ROLLOUT", change.GetChange().GetKind())
	}
	// No audit entry yet, so the source said nothing about who acted — UNSPECIFIED, not UNKNOWN.
	if got := change.GetChange().GetActorKind(); got != graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		t.Errorf("actor kind = %s with no audit entry; UNKNOWN would claim the feeder looked", got)
	}
	// The digest is claimed, which is the identifier a build pipeline also knows.
	if !claimValues(em.events)["gcp.cloudrun.revision=sha256:abc"] {
		t.Error("the image digest was not claimed")
	}

	// The mark this test's own heading has always promised, asserted from 004 onwards.
	//
	// Until then it could not be: `ObserveChange` had no property field, so the feeder built
	// RevisionCreatedProps and discarded it, and contract §3.3's "the creation change is marked as not
	// having moved production traffic" was a clause nothing met. The value is `false` and it is
	// PRESENT, which is the whole of FR-016 — an absent property reads as "we did not say", and the
	// requirement is a positive statement that this rollout moved nothing.
	props := change.GetProps().GetFields()
	moved, stated := props[gcpfeeder.PropMovedTraffic]
	switch {
	case !stated:
		t.Errorf("the creation change carries no %s; FR-016 requires it MARKED as not having moved "+
			"traffic, and an absent property reads as `we did not say` (contract §3.3)",
			gcpfeeder.PropMovedTraffic)
	case moved.GetBoolValue():
		t.Errorf("%s is true on a revision creation; creating a revision moves no traffic",
			gcpfeeder.PropMovedTraffic)
	}
	if got := props[gcpfeeder.PropRolloutKind].GetStringValue(); got != gcpfeeder.RolloutRevisionCreated {
		t.Errorf("%s = %q, want %q; the two rollouts must be distinguishable by a published property "+
			"rather than by parsing a summary string", gcpfeeder.PropRolloutKind, got,
			gcpfeeder.RolloutRevisionCreated)
	}
}

// A recreated name retracts the old service and says why in the checkpoint: a retraction event has
// nowhere to carry the reason, and without it a reader cannot tell a recreation from a deletion
// followed by a coincidence.
func TestARecreatedServiceIsRetractedAndExplainedInTheCheckpoint(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	recreated := strings.Replace(checkoutService, `"uid": "uid-1"`, `"uid": "uid-2"`, 1)
	recreated = strings.Replace(recreated, `"createTime": "2026-09-01T09:00:00Z"`, `"createTime": "2026-09-21T10:00:00Z"`, 1)
	// The new service serves a *different* revision. Without that difference, comparing the new
	// service's split against the old one's would find nothing and this test would pass against an
	// implementation that carried the comparison across the recreation.
	recreated = strings.Replace(recreated,
		`"revision": "checkout-00041-xyz", "percent": 100`,
		`"revision": "checkout-00100-new", "percent": 100`, 1)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
			Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC),
			Bytes: servicePayload(t, recreated)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasRetraction(em.events) {
		t.Fatal("the recreated service's predecessor was not retracted (FR-025)")
	}
	// And no traffic shift was carried across the two, because the old entity's split is not this
	// entity's history. The new service serves a different revision, so an implementation that kept
	// comparing would hold a shift here.
	if n := countChanges(em.events); n != 0 {
		t.Errorf("%d changes were emitted across a recreation; comparing the new service's split "+
			"against the old one's would emit a rollout that never happened", n)
	}
	if held := f.Held(); len(held) != 0 {
		t.Errorf("%d traffic shifts were held across a recreation: %v. The two services are not one "+
			"continuous entity, so the old one's split is not a state the new one moved from (FR-025)",
			len(held), held)
	}
}

// With the completion entry, the same split change is dated at **that entry's** timestamp — not at
// the poll that noticed it, and not at the request entry that preceded it.
//
// This is the whole of SC-002 end to end: the poll detects *that* the split changed, the audit log
// supplies *when* and *who*, and the two are correlated by `operation.id`.
func TestASplitChangeWithACompletionEntryIsDatedFromThatEntry(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	moved := strings.Replace(checkoutService,
		`"revision": "checkout-00041-xyz", "percent": 100`,
		`"revision": "checkout-00042-abc", "percent": 100`, 1)

	// The request entry at 14:19:30 and the completion entry at 14:20:00. The poll that notices runs
	// at 14:25. Only one of those three instants is the answer.
	const auditPage = `{"entries":[
      {"insertId":"a1","logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T14:19:30Z","receiveTimestamp":"2026-09-21T14:19:35Z",
       "operation":{"id":"op-77","producer":"run.googleapis.com","first":true},
       "protoPayload":{"serviceName":"run.googleapis.com",
         "methodName":"google.cloud.run.v2.Services.UpdateService",
         "resourceName":"projects/nova-production/locations/europe-west1/services/checkout",
         "authenticationInfo":{"principalEmail":"deploy@nova-production.iam.gserviceaccount.com"},
         "requestMetadata":{"callerIp":"203.0.113.42","callerSuppliedUserAgent":"gcloud/456.0.0"}}},
      {"insertId":"a2","logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T14:20:00Z","receiveTimestamp":"2026-09-21T14:20:04Z",
       "operation":{"id":"op-77","producer":"run.googleapis.com","last":true},
       "protoPayload":{"serviceName":"run.googleapis.com",
         "methodName":"google.cloud.run.v2.Services.UpdateService",
         "resourceName":"projects/nova-production/locations/europe-west1/services/checkout",
         "authenticationInfo":{"principalEmail":"service-1@gcp-sa-run.iam.gserviceaccount.com"}}}
    ]}`

	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 19, 0, 0, time.UTC),
			Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadAuditEntries, At: time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC),
			Bytes: []byte(auditPage)},
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC),
			Bytes: servicePayload(t, moved)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if held := f.Held(); len(held) != 0 {
		t.Fatalf("the shift was held despite a completion entry: %v", held)
	}

	var shift *graphv1.ObserveChange
	for _, ev := range em.events {
		if change := ev.GetObserveChange(); change != nil &&
			strings.HasPrefix(change.GetRef().GetValue(), "rollout-traffic/") {
			shift = change
		}
	}
	if shift == nil {
		t.Fatal("no traffic-shift change was emitted")
	}
	want := time.Date(2026, 9, 21, 14, 20, 0, 0, time.UTC)
	if got := shift.GetValidAt().AsTime(); !got.Equal(want) {
		t.Fatalf("the shift is valid at %s, want the operation.last entry's timestamp %s. "+
			"14:19:30 is the request entry (the operation had not happened) and 14:25 is the poll "+
			"(the feeder only just noticed)", got, want)
	}

	// The principal comes from the **request** entry, not the completion entry: the completion entry
	// of an UpdateService operation carries the service agent, and attributing the rollout to it
	// would type a human's or a pipeline's deploy as CONTROLLER — which is the actor kind feature
	// 002 uses to exonerate.
	if got := shift.GetChange().GetActorKind(); got != graphv1.ActorKind_AUTOMATION {
		t.Fatalf("actor kind = %s, want AUTOMATION from the request entry's principal; the completion "+
			"entry names the service agent, and taking it would exonerate the deploy that caused the "+
			"outage (ADR-0005 D1)", got)
	}
}

// A duplicate entry is not a second change. Every poll re-queries a trailing overlap window, so
// deduplicating on `insertId` — GCP's own duplicate key — is what stops one shift being dated twice.
func TestADuplicateAuditEntryIsNotASecondChange(t *testing.T) {
	idx := gcpfeeder.NewAuditIndex()
	page := []byte(`{"entries":[
      {"insertId":"a1","logName":"projects/p/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T14:20:00Z",
       "protoPayload":{"methodName":"google.cloud.run.v2.Services.UpdateService",
         "resourceName":"projects/nova-production/locations/europe-west1/services/checkout"}}]}`)
	first, err := idx.Ingest(page, auditArrival)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	second, err := idx.Ingest(page, auditArrival)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("fresh entries = %d then %d, want 1 then 0", first, second)
	}
	svc := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	if idx.Take(svc) == nil {
		t.Fatal("the entry did not produce a completion")
	}
	// Consumed once: a second poll observing the same shift finds nothing and holds, which is
	// correct — the split has not changed again.
	if again := idx.Take(svc); again != nil {
		t.Fatalf("the completion was consumed twice, dating one shift as two changes: %+v", again)
	}
}

// An entry from the data-access stream is a mistake worth refusing: it is never read and no
// permission for it is requested (FR-042).
func TestAnEntryFromTheDataAccessStreamIsRefused(t *testing.T) {
	idx := gcpfeeder.NewAuditIndex()
	_, err := idx.Ingest([]byte(`{"entries":[
      {"insertId":"d1","logName":"projects/p/logs/cloudaudit.googleapis.com%2Fdata_access",
       "timestamp":"2026-09-21T14:20:00Z"}]}`), auditArrival)
	if err == nil {
		t.Fatal("an entry from the data-access stream was processed quietly")
	}
	if !errors.Is(err, gcpfeeder.ErrDataAccessStream) {
		t.Fatalf("the refusal is %v, want ErrDataAccessStream", err)
	}
}

// A read is not a change: a prefix match on the Cloud Run service API would also catch GetService.
func TestAReadMethodIsNotAChange(t *testing.T) {
	idx := gcpfeeder.NewAuditIndex()
	if _, err := idx.Ingest([]byte(`{"entries":[
      {"insertId":"r1","logName":"projects/p/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T14:20:00Z",
       "protoPayload":{"methodName":"google.cloud.run.v2.Services.GetService",
         "resourceName":"projects/nova-production/locations/europe-west1/services/checkout"}}]}`), auditArrival); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	svc := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	if completion := idx.Take(svc); completion != nil {
		t.Fatalf("GetService produced a datable completion: %+v", completion)
	}
}

// A request entry alone dates nothing: the operation had not happened yet, and dating from it would
// put the change at the moment somebody pressed deploy rather than the moment production served
// differently.
func TestARequestEntryAloneDatesNothing(t *testing.T) {
	idx := gcpfeeder.NewAuditIndex()
	if _, err := idx.Ingest([]byte(`{"entries":[
      {"insertId":"q1","logName":"projects/p/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T14:19:30Z",
       "operation":{"id":"op-1","first":true},
       "protoPayload":{"methodName":"google.cloud.run.v2.Services.UpdateService",
         "resourceName":"projects/nova-production/locations/europe-west1/services/checkout"}}]}`), auditArrival); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	svc := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	if completion := idx.Take(svc); completion != nil {
		t.Fatalf("a request entry with no completion dated a shift at %s", completion.At)
	}
	if idx.PendingRequests() != 1 {
		t.Errorf("pending requests = %d, want 1; a poll ending with pending requests has rollouts "+
			"in flight, which the checkpoint states", idx.PendingRequests())
	}
}

func firstNode(events []*graphv1.EventEnvelope, namespace string) *graphv1.UpsertNode {
	for _, ev := range events {
		node := ev.GetUpsertNode()
		if node != nil && node.GetRef().GetNamespace() == namespace {
			return node
		}
	}
	return nil
}

func firstChange(events []*graphv1.EventEnvelope) *graphv1.ObserveChange {
	for _, ev := range events {
		if change := ev.GetObserveChange(); change != nil {
			return change
		}
	}
	return nil
}

func countChanges(events []*graphv1.EventEnvelope) int {
	n := 0
	for _, ev := range events {
		if ev.GetObserveChange() != nil && ev.GetObserveChange().GetChange().GetKind() == graphv1.ChangeKind_ROLLOUT {
			if strings.HasPrefix(ev.GetObserveChange().GetRef().GetValue(), "rollout-traffic/") {
				n++
			}
		}
	}
	return n
}

func claimValues(events []*graphv1.EventEnvelope) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		if claim := ev.GetIdentityClaim(); claim != nil {
			out[claim.GetClaim().GetNamespace()+"="+claim.GetClaim().GetValue()] = true
		}
	}
	return out
}

func hasEdge(events []*graphv1.EventEnvelope, kind graphv1.EdgeType) bool {
	for _, ev := range events {
		if edge := ev.GetUpsertEdge(); edge != nil && edge.GetType() == kind {
			return true
		}
	}
	return false
}

func hasRetraction(events []*graphv1.EventEnvelope) bool {
	for _, ev := range events {
		if ev.GetRetractNode() != nil {
			return true
		}
	}
	return false
}

// The actor of a Cloud SQL configuration change comes from the audit entry, and it comes from the
// **request** entry (T131, FR-031).
//
// This lives here rather than in a fixture for the reason contracts/sanitisation.md §7 draws: a
// sanitised recording has no `authenticationInfo` block at all — FR-135 drops a principal, it does
// not hash or rename one, and `scripts/check-no-secrets.sh` fails the build on the field name — so
// the twin cannot carry one. In memory it can, nothing is written to disk, and the classification is
// asserted end to end through the feeder rather than on the ladder in isolation.
func TestAnAuditPrincipalReachesTheCloudSQLConfigChange(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}

	instance := func(maxConnections string) []byte {
		return []byte(`{"items":[{
          "name":"orders-primary","project":"nova-production","region":"europe-west1",
          "connectionName":"nova-production:europe-west1:orders-primary",
          "databaseVersion":"POSTGRES_16","state":"RUNNABLE","createTime":"2026-03-01T09:00:00Z",
          "settings":{"tier":"db-custom-4-16384","databaseFlags":[
            {"name":"max_connections","value":"` + maxConnections + `"}]}}]}`)
	}
	// The request entry carries the caller; the completion entry carries the service agent and the
	// instant. Both halves are needed and they come from different entries.
	const auditPage = `{"entries":[
      {"insertId":"s1","logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T03:11:30Z","receiveTimestamp":"2026-09-21T03:11:35Z",
       "operation":{"id":"op-sql-1","producer":"cloudsql.googleapis.com","first":true},
       "protoPayload":{"serviceName":"cloudsql.googleapis.com",
         "methodName":"cloudsql.instances.update",
         "resourceName":"projects/nova-production/instances/orders-primary",
         "authenticationInfo":{"principalEmail":"deploy@nova-production.iam.gserviceaccount.com"}}},
      {"insertId":"s2","logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
       "timestamp":"2026-09-21T03:12:00Z","receiveTimestamp":"2026-09-21T03:12:06Z",
       "operation":{"id":"op-sql-1","producer":"cloudsql.googleapis.com","last":true},
       "protoPayload":{"serviceName":"cloudsql.googleapis.com",
         "methodName":"cloudsql.instances.update",
         "resourceName":"projects/nova-production/instances/orders-primary",
         "authenticationInfo":{"principalEmail":"service-1@gcp-sa-cloud-sql.iam.gserviceaccount.com"}}}
    ]}`

	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadSQLInstances, At: time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC),
			Bytes: instance("200")},
		{Kind: gcpfeeder.PayloadAuditEntries, At: time.Date(2026, 9, 21, 3, 30, 0, 0, time.UTC),
			Bytes: []byte(auditPage)},
		{Kind: gcpfeeder.PayloadSQLInstances, At: time.Date(2026, 9, 21, 3, 30, 0, 0, time.UTC),
			Bytes: instance("500")},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if held := f.HeldSQLConfigs(); len(held) != 0 {
		t.Fatalf("the diff was held despite a completion entry: %v", held)
	}

	var change *graphv1.ObserveChange
	for _, ev := range em.events {
		if c := ev.GetObserveChange(); c != nil &&
			strings.HasPrefix(c.GetRef().GetValue(), "sql-config/") {
			change = c
		}
	}
	if change == nil {
		t.Fatal("no Cloud SQL configuration change was emitted")
	}
	want := time.Date(2026, 9, 21, 3, 12, 0, 0, time.UTC)
	if got := change.GetValidAt().AsTime(); !got.Equal(want) {
		t.Fatalf("the change is valid at %s, want the operation.last entry's timestamp %s. 03:11:30 "+
			"is the request entry (the edit had not taken effect) and 03:30 is the poll (the feeder "+
			"only just noticed)", got, want)
	}
	if got := change.GetChange().GetActorKind(); got != graphv1.ActorKind_AUTOMATION {
		t.Fatalf("actor kind = %s, want AUTOMATION from the request entry's principal; the completion "+
			"entry names the service agent, and taking it would type a pipeline's edit as CONTROLLER "+
			"— the actor kind feature 002 uses to exonerate (ADR-0005 D1)", got)
	}
	// The instance is the target, which is what gives it a `changed-by` edge (FR-031).
	if len(change.GetTargets()) != 1 ||
		change.GetTargets()[0].GetValue() != "nova-production/europe-west1/orders-primary" {
		t.Errorf("targets = %v, want the instance", change.GetTargets())
	}
}

// With no audit entry the diff is HELD, not dated at the poll: a configuration change at an instant
// GCP never stated is wrong by up to one poll interval on every edit, and silently.
func TestACloudSQLDiffWithNoAuditEntryIsHeldAndStated(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	instance := func(maxConnections string) []byte {
		return []byte(`{"items":[{
          "name":"orders-primary","project":"nova-production","region":"europe-west1",
          "connectionName":"nova-production:europe-west1:orders-primary",
          "databaseVersion":"POSTGRES_16","state":"RUNNABLE","createTime":"2026-03-01T09:00:00Z",
          "settings":{"tier":"db-custom-4-16384","databaseFlags":[
            {"name":"max_connections","value":"` + maxConnections + `"}]}}]}`)
	}
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadSQLInstances, At: time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC),
			Bytes: instance("200")},
		{Kind: gcpfeeder.PayloadSQLInstances, At: time.Date(2026, 9, 21, 3, 30, 0, 0, time.UTC),
			Bytes: instance("500")},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, ev := range em.events {
		if c := ev.GetObserveChange(); c != nil &&
			strings.HasPrefix(c.GetRef().GetValue(), "sql-config/") {
			t.Fatalf("a configuration change was emitted with no audit entry to date it: %s at %s",
				c.GetRef().GetValue(), c.GetValidAt().AsTime())
		}
	}
	held := f.HeldSQLConfigs()
	if len(held) != 1 {
		t.Fatalf("held = %v, want the one diff that could not be dated", held)
	}
	if !strings.Contains(held[0].String(), "held since") {
		t.Errorf("the held diff does not say it is held: %q", held[0].String())
	}
	if !strings.Contains(held[0].Diff.Summary(), "flag") {
		t.Errorf("the held diff does not say what changed: %q", held[0].Diff.Summary())
	}
}

// auditArrival is the instant an audit page is treated as having arrived in these tests. The index
// measures its staleness bound from the *arrival* and not from an entry's own timestamp, so a test
// that passed the zero instant would release every entry to the catch-all immediately (audit.go).
var auditArrival = time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC)
