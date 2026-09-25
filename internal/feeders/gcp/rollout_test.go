// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// The two rollouts (T051–T056, FR-015–FR-018, contracts/gcp-feeder.md §3.2, §3.3).

var (
	// The fixture clock. A revision created at 14:18, given traffic at 14:20, rolled back at 14:41.
	created   = time.Date(2026, 9, 21, 14, 18, 0, 0, time.UTC)
	shiftedAt = time.Date(2026, 9, 21, 14, 20, 0, 0, time.UTC)
	rolledAt  = time.Date(2026, 9, 21, 14, 41, 0, 0, time.UTC)
)

func statuses(pairs ...any) []*runpb.TrafficTargetStatus {
	var out []*runpb.TrafficTargetStatus
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, &runpb.TrafficTargetStatus{
			Revision: pairs[i].(string),
			Percent:  int32(pairs[i+1].(int)),
		})
	}
	return out
}

// `trafficStatuses` is the observed split and `traffic` is the desired one. The graph records what
// production is actually serving, so during a stuck rollout the two disagree — and the disagreement
// *is* the incident.
func TestTheSplitComesFromTheObservedFieldAndNotTheDesiredOne(t *testing.T) {
	service := &runpb.Service{
		Name: "projects/nova-production/locations/europe-west1/services/checkout",
		// Desired: everything on the new revision. This must not reach the graph.
		Traffic: []*runpb.TrafficTarget{{Revision: "checkout-00042-abc", Percent: 100}},
		// Observed: the rollout has not converged. This is what the graph records.
		TrafficStatuses: statuses("checkout-00041-xyz", 90, "checkout-00042-abc", 10),
		CreateTime:      timestamppb.New(created),
	}
	obs, err := gcpfeeder.ObserveService(service, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveService: %v", err)
	}
	if got := obs.Split.String(); got != "checkout-00041-xyz=90 checkout-00042-abc=10" {
		t.Fatalf("split = %q; reading `traffic` would say the new revision serves 100%% from the "+
			"instant somebody pressed deploy, which is the exact window an investigation asks about", got)
	}
	if percent, _ := obs.Split.Percent("checkout-00042-abc"); percent != 10 {
		t.Errorf("the new revision is recorded at %d%%, not the observed 10%%", percent)
	}
}

// The split is canonical: two reads of one split are one value, so the property does not gain a
// version because Cloud Run reordered a list or split a revision across tags.
func TestTheSplitIsCanonicalSoOneSplitIsOneValue(t *testing.T) {
	forward := gcpfeeder.ObservedTrafficSplit(statuses("b", 40, "a", 60))
	backward := gcpfeeder.ObservedTrafficSplit(statuses("a", 60, "b", 40))
	if !forward.Equal(backward) {
		t.Fatalf("a reordered list produced a different split: %q vs %q", forward, backward)
	}

	// A revision appearing twice — once untagged, once per tag — sums to one share, which is what
	// makes the total 100.
	tagged := []*runpb.TrafficTargetStatus{
		{Revision: "a", Percent: 60},
		{Revision: "a", Percent: 20, Tag: "canary"},
		{Revision: "b", Percent: 20},
	}
	split := gcpfeeder.ObservedTrafficSplit(tagged)
	if percent, _ := split.Percent("a"); percent != 80 {
		t.Fatalf("a revision split across tags summed to %d, want 80", percent)
	}
	if split.Total() != 100 {
		t.Errorf("total = %d, want 100", split.Total())
	}

	// A LATEST allocation Cloud Run has not resolved carries no revision. It is skipped rather than
	// recorded as a revision called "", because "" would become a node.
	unresolved := gcpfeeder.ObservedTrafficSplit([]*runpb.TrafficTargetStatus{{Percent: 100}})
	if len(unresolved.Shares) != 0 {
		t.Errorf("an unresolved allocation became %d shares: %v", len(unresolved.Shares), unresolved.Shares)
	}
}

// A deploy is two changes at two instants. Conflating them blames a revision that was not yet
// serving, or hides one that was created broken and crashed on startup.
func TestADeployIsTwoChangesAtTwoInstants(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	revision := gcpfeeder.Revision{Service: service, Revision: "checkout-00042-abc"}
	actor := actorPolicy().Classify(gcpfeeder.AuthenticationInfo{
		PrincipalEmail: "deploy@nova-production.iam.gserviceaccount.com"}, false)

	creation, err := gcpfeeder.RevisionCreated(revision, created, actor, "op-1", "google.cloud.run.v2.Services.UpdateService")
	if err != nil {
		t.Fatalf("RevisionCreated: %v", err)
	}
	before := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00041-xyz", 100))
	after := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00042-abc", 100))
	shift, err := gcpfeeder.TrafficShift(service, shiftedAt, before, after, actor, "op-1", "google.cloud.run.v2.Services.UpdateService")
	if err != nil {
		t.Fatalf("TrafficShift: %v", err)
	}

	if creation.ValidAt.Equal(shift.ValidAt) {
		t.Fatal("the two rollouts share an instant")
	}
	if !creation.ValidAt.Equal(created) {
		t.Errorf("the creation is valid at %s, want the revision's createTime %s", creation.ValidAt, created)
	}
	if !shift.ValidAt.Equal(shiftedAt) {
		t.Errorf("the shift is valid at %s, want the completion entry's timestamp %s", shift.ValidAt, shiftedAt)
	}
	if creation.Ref.GetValue() == shift.Ref.GetValue() {
		t.Fatal("the two rollouts share a ref, so one would overwrite the other")
	}
	// Each ref is keyed on *its own* instant, which is what makes a rollback a third change rather
	// than an overwrite of the second. Asserting only that the two differ would pass against a pair
	// of refs that carry no instant at all.
	if !strings.Contains(creation.Ref.GetValue(), created.Format(time.RFC3339)) {
		t.Errorf("the creation ref %q is not keyed on the creation instant", creation.Ref.GetValue())
	}
	if !strings.Contains(shift.Ref.GetValue(), shiftedAt.Format(time.RFC3339)) {
		t.Errorf("the shift ref %q is not keyed on the instant the split took effect", shift.Ref.GetValue())
	}
	if !strings.Contains(creation.Ref.GetValue(), revision.Revision) {
		t.Errorf("the creation ref %q is not keyed on the revision", creation.Ref.GetValue())
	}
	if !strings.Contains(shift.Ref.GetValue(), service.Value()) {
		t.Errorf("the shift ref %q is not keyed on the service; FR-017 is about what the service "+
			"serves, so keying on the revision would make a rollback ambiguous", shift.Ref.GetValue())
	}
	if creation.Kind != graphv1.ChangeKind_ROLLOUT || shift.Kind != graphv1.ChangeKind_ROLLOUT {
		t.Errorf("kinds = %s, %s; both are rollouts", creation.Kind, shift.Kind)
	}

	// Distinguishable by a published property, not by parsing a summary string.
	creationProps := propsMap(t, gcpfeeder.RevisionCreatedProps(revision, actor, "op-1", "m"))
	shiftProps := propsMap(t, gcpfeeder.TrafficShiftProps(service, before, after, actor, "op-1", "m"))
	if creationProps[gcpfeeder.PropMovedTraffic] != "false" {
		t.Errorf("the creation change is not marked as not having moved traffic (FR-016): %v", creationProps)
	}
	if shiftProps[gcpfeeder.PropMovedTraffic] != "true" {
		t.Errorf("the shift is not marked as having moved traffic: %v", shiftProps)
	}
	if creationProps[gcpfeeder.PropRolloutKind] != gcpfeeder.RolloutRevisionCreated ||
		shiftProps[gcpfeeder.PropRolloutKind] != gcpfeeder.RolloutTrafficShift {
		t.Errorf("the two rollouts are not distinguishable by %s", gcpfeeder.PropRolloutKind)
	}
}

// A rollback is a third change at its own instant, not a deletion of the second — and keying the ref
// on the service plus the instant is what makes that fall out rather than needing a rule.
func TestARollbackIsAThirdChangeRatherThanADeletionOfTheSecond(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	actor := actorPolicy().Classify(gcpfeeder.AuthenticationInfo{PrincipalEmail: "operator@example.com"}, false)

	old := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00041-xyz", 100))
	neu := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00042-abc", 100))

	forward, err := gcpfeeder.TrafficShift(service, shiftedAt, old, neu, actor, "op-1", "m")
	if err != nil {
		t.Fatalf("TrafficShift: %v", err)
	}
	back, err := gcpfeeder.TrafficShift(service, rolledAt, neu, old, actor, "op-2", "m")
	if err != nil {
		t.Fatalf("TrafficShift: %v", err)
	}
	if forward.Ref.GetValue() == back.Ref.GetValue() {
		t.Fatal("the rollback shares a ref with the rollout, so it would overwrite it rather than " +
			"being its own third change (FR-017)")
	}
	if !back.ValidAt.Equal(rolledAt) {
		t.Errorf("the rollback is valid at %s, want %s", back.ValidAt, rolledAt)
	}
	// Three versions of the split property: the original, the shift, the rollback.
	seen := map[string]bool{old.String(): true, neu.String(): true}
	if len(seen) != 2 {
		t.Fatalf("the two splits are indistinguishable")
	}
	beforeProps := propsMap(t, gcpfeeder.TrafficShiftProps(service, neu, old, actor, "op-2", "m"))
	if beforeProps[gcpfeeder.PropSplitBefore] == "" || beforeProps[gcpfeeder.PropSplitAfter] == "" {
		t.Error("a shift records neither side of the split; a rollback is only recognisable from the pair")
	}
}

// A split observed with no completion entry is **held**, not emitted with a guessed instant. Emitting
// it at the poll time would put a change in the graph at an instant GCP never stated, breaking SC-002
// silently for every shift whose entry was still in flight.
func TestASplitWithNoCompletionEntryIsHeldRatherThanGuessed(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	before := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00041-xyz", 100))
	after := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00042-abc", 100))
	polledAt := time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC)

	outcome, err := gcpfeeder.ShiftFromPoll(service, before, after, polledAt, nil, actorPolicy())
	if err != nil {
		t.Fatalf("ShiftFromPoll: %v", err)
	}
	if outcome.Change != nil {
		t.Fatalf("a shift with no completion entry was emitted at %s; the instant is the "+
			"operation.last entry's timestamp and nothing else (contracts/gcp-feeder.md §3.2)",
			outcome.Change.ValidAt)
	}
	if outcome.Held == nil {
		t.Fatal("the shift was neither emitted nor held, so it was lost")
	}
	if !outcome.Held.ObservedAt.Equal(polledAt) {
		t.Errorf("the hold records %s as observed, want %s", outcome.Held.ObservedAt, polledAt)
	}
	// The checkpoint says so, which is the clause that makes holding honest rather than silent.
	note := gcpfeeder.SummariseHeld([]gcpfeeder.HeldSplit{*outcome.Held})
	if !strings.Contains(note, service.Value()) || !strings.Contains(note, "held since") {
		t.Errorf("the checkpoint note does not state the held shift: %q", note)
	}

	// With the entry, the same shift is emitted at the entry's timestamp.
	completion := &gcpfeeder.Completion{At: shiftedAt, OperationID: "op-1", MethodName: "google.cloud.run.v2.Services.UpdateService"}
	outcome, err = gcpfeeder.ShiftFromPoll(service, before, after, polledAt, completion, actorPolicy())
	if err != nil {
		t.Fatalf("ShiftFromPoll: %v", err)
	}
	if outcome.Held != nil {
		t.Fatal("a dated shift was also held")
	}
	if outcome.Change == nil || !outcome.Change.ValidAt.Equal(shiftedAt) {
		t.Fatalf("the dated shift is valid at %v, want %s", outcome.Change, shiftedAt)
	}
	if outcome.Change.ValidAt.Equal(polledAt) {
		t.Fatal("the shift took the poll time as its instant")
	}
}

// An unchanged split is not a change, and that is an empty outcome rather than an error — so the
// caller's loop is a plain reconcile without a pre-check duplicating the comparison.
func TestAnUnchangedSplitIsNotAChange(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	split := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00042-abc", 100))
	outcome, err := gcpfeeder.ShiftFromPoll(service, split, split, shiftedAt, nil, actorPolicy())
	if err != nil {
		t.Fatalf("ShiftFromPoll: %v", err)
	}
	if outcome.Change != nil || outcome.Held != nil {
		t.Fatalf("an unchanged split produced %+v", outcome)
	}
}

// FR-018: a revision at 0% still exists, still produced its creation change, is not presented as
// serving, and its creation change is never reinterpreted when traffic later arrives.
func TestAZeroTrafficRevisionExistsIsNotServingAndIsNeverReinterpreted(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	revision := gcpfeeder.Revision{Service: service, Revision: "checkout-00042-abc"}
	obs := gcpfeeder.RevisionObservation{Revision: revision, CreateTime: created}

	atZero := gcpfeeder.ObservedTrafficSplit(statuses("checkout-00041-xyz", 100, "checkout-00042-abc", 0))
	exists, serving, produced := gcpfeeder.ZeroTrafficRevision(obs, atZero)
	if !exists || serving || !produced {
		t.Fatalf("a 0%% revision: exists=%v serving=%v produced-creation-change=%v; want true,false,true (FR-018)",
			exists, serving, produced)
	}
	// It is absent from the serving list, which is derived from the split rather than stored
	// beside it, so the two cannot disagree.
	for _, name := range atZero.Serving() {
		if name == revision.Revision {
			t.Fatal("a 0% revision is listed as serving")
		}
	}
	// And it is *mentioned* at 0%, which is a different fact from not being in the split at all.
	if percent, mentioned := atZero.Percent(revision.Revision); !mentioned || percent != 0 {
		t.Errorf("the revision is %d%% mentioned=%v; \"mentioned at 0\" and \"not in the split\" are "+
			"different facts", percent, mentioned)
	}

	// The creation change is never reinterpreted when traffic later arrives.
	if err := gcpfeeder.AssertCreationChangeNotReinterpreted(revision, created, shiftedAt); err == nil {
		t.Fatal("building a creation change at a later instant was accepted; the later movement is " +
			"its own change at its own instant (FR-018)")
	}
	if err := gcpfeeder.AssertCreationChangeNotReinterpreted(revision, created, created); err != nil {
		t.Errorf("building a creation change at its createTime was refused: %v", err)
	}
	// Structurally: re-emitting at a later instant produces a *different* change rather than an
	// update to the first.
	first := gcpfeeder.ChangeRefRevisionCreated(revision, created)
	later := gcpfeeder.ChangeRefRevisionCreated(revision, shiftedAt)
	if first.GetValue() == later.GetValue() {
		t.Fatal("a creation change re-emitted at a later instant addresses the same node, so it " +
			"would overwrite the original retroactively")
	}
}

// A creation change with no instant is not a change; `Revision.createTime` is output-only and
// documented, so a missing one is an error rather than an unknown start (contract §3.3).
func TestACreationChangeWithNoInstantIsRefused(t *testing.T) {
	revision := gcpfeeder.Revision{
		Service:  gcpfeeder.Service{Project: "p", Region: "r", Name: "s"},
		Revision: "s-00001-abc",
	}
	if _, err := gcpfeeder.RevisionCreated(revision, time.Time{}, gcpfeeder.Actor{}, "", ""); err == nil {
		t.Fatal("a creation change with no createTime was built")
	}
	service := gcpfeeder.Service{Project: "p", Region: "r", Name: "s"}
	split := gcpfeeder.ObservedTrafficSplit(statuses("a", 100))
	if _, err := gcpfeeder.TrafficShift(service, time.Time{}, split, split, gcpfeeder.Actor{}, "", ""); err == nil {
		t.Fatal("a traffic shift with no instant was built")
	}
}

// A ref built from a local-zone instant would differ from the same instant read elsewhere, and the
// two would be two changes. Re-reading a poll must be a no-op (FR-076).
func TestAChangeRefIsDeterministicAcrossTimeZones(t *testing.T) {
	revision := gcpfeeder.Revision{
		Service:  gcpfeeder.Service{Project: "p", Region: "r", Name: "s"},
		Revision: "s-00001-abc",
	}
	utc := gcpfeeder.ChangeRefRevisionCreated(revision, created)
	elsewhere := gcpfeeder.ChangeRefRevisionCreated(revision, created.In(time.FixedZone("x", 7*3600)))
	if utc.GetValue() != elsewhere.GetValue() {
		t.Fatalf("the same instant in two zones gave two refs: %q vs %q", utc.GetValue(), elsewhere.GetValue())
	}
}

// Only the revisions whose share actually changed are targets. Including every long-lived revision
// would put them all at hop 0 of every shift.
func TestOnlyTheRevisionsWhoseShareChangedAreTargets(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	before := gcpfeeder.ObservedTrafficSplit(statuses("a", 50, "b", 50, "stable", 0))
	after := gcpfeeder.ObservedTrafficSplit(statuses("a", 100, "b", 0, "stable", 0))
	shift, err := gcpfeeder.TrafficShift(service, shiftedAt, before, after, gcpfeeder.Actor{}, "", "")
	if err != nil {
		t.Fatalf("TrafficShift: %v", err)
	}
	var targets []string
	for _, ref := range shift.Targets {
		targets = append(targets, ref.GetValue())
	}
	joined := strings.Join(targets, " ")
	if !strings.Contains(joined, "/a") || !strings.Contains(joined, "/b") {
		t.Fatalf("the revisions whose share changed are not targets: %v", targets)
	}
	if strings.Contains(joined, "/stable") {
		t.Errorf("a revision whose share did not change is a target: %v", targets)
	}
	if !strings.Contains(joined, service.Value()) {
		t.Errorf("the service is not a target of its own traffic shift: %v", targets)
	}
}
