// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

func sliceOf(p feeder.Payload) feeder.Source { return source.NewSliceSource([]feeder.Payload{p}) }

// Section D: Datadog's event stream as a change source (T092; FR-027–FR-033b).
//
// The payloads are built from the published Events API v2 response shape, with synthetic names; nothing
// here has run against a live organisation.

const (
	evCommitA = "5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b"
	evCommitB = "6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c"
	evImage   = "ghcr.io/twin/payments@sha256:00000000000000000000000000000000000000000000000000000000000000ab"
)

// evt is one event of the twin, in the API's shape.
type evt struct {
	id, title, source, kind string
	at                      time.Time
	tags                    []string
}

func (e evt) json() map[string]any {
	inner := map[string]any{"title": e.title, "source_type_name": e.source}
	if e.kind != "" {
		inner["evt"] = map[string]any{"type": e.kind}
	}
	return map[string]any{
		"id": e.id, "type": "event",
		"attributes": map[string]any{
			"timestamp": e.at.UTC().Format(time.RFC3339Nano), "tags": e.tags, "attributes": inner,
		},
	}
}

func eventsPayload(t *testing.T, when time.Time, events ...evt) feeder.Payload {
	t.Helper()
	data := make([]map[string]any, 0, len(events))
	for _, e := range events {
		data = append(data, e.json())
	}
	raw, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadEvents, At: when, Bytes: raw}
}

func eventsPoll(t *testing.T, when time.Time, m ddfeeder.EventsMarker) feeder.Payload {
	t.Helper()
	if m.Outcome == "" {
		m.Outcome = "complete"
	}
	if m.Window.From.IsZero() {
		m.Window = ddfeeder.DiscoveryWindow{From: when.Add(-10 * time.Minute), To: when}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadEventsPoll, At: when, Bytes: raw}
}

func changesOptions() ddfeeder.Options {
	return ddfeeder.Options{
		OrgSlug: "twin", Site: "datadoghq.eu",
		Capabilities: ddfeeder.Capabilities{ddfeeder.CapLogs: true, ddfeeder.CapMonitors: true, ddfeeder.CapTags: true, ddfeeder.CapChanges: true},
		Changes:      ddfeeder.ChangeScope{Sources: []string{"jenkins", "custom_pipeline"}, Tags: []string{"event_type:deployment"}},
	}
}

func deployEvent() evt {
	return evt{id: "evt-1001", title: "Deployed payments to prod", source: "jenkins", kind: "deployment", at: hms(14, 9, 30),
		tags: []string{"env:prod", "service:payments", "git.commit.sha:" + evCommitA, "image:" + evImage,
			"version:v2.4.0", "user:release-runner", "kube_namespace:shop", "kube_deployment:payments"}}
}

func correlationsOf(em *emit.MemoryEmitter) map[string]string {
	out := map[string]string{}
	for _, ev := range em.Events() {
		if c := ev.GetCorrelateEntity(); c != nil && c.GetSubject().GetNamespace() == ddfeeder.NSChange {
			out[c.GetKey().GetNamespace()] = c.GetKey().GetValue() + " env=" +
				c.GetAttributes().GetFields()[feeder.AttrDeploymentEnvironment].GetStringValue()
		}
	}
	return out
}

func eventChanges(em *emit.MemoryEmitter) []*graphv1.ObserveChange {
	var out []*graphv1.ObserveChange
	for _, c := range changes(em) {
		if c.GetRef().GetNamespace() == ddfeeder.NSChange && c.GetProps().GetFields()[feeder.PropChangeValidFromIsABound] == nil {
			out = append(out, c)
		}
	}
	return out
}

// FR-027, FR-031: a deploy event is one ROLLOUT dated by Datadog's instant, attached to what its tags
// name, carrying every identifier it states as a correlation key in the published deploy namespaces,
// each with the environment.
func TestADeployEventIsOneRolloutWithItsIdentifiers(t *testing.T) {
	t.Parallel()
	em := run(t, changesOptions(), eventsPayload(t, hm(14, 20), deployEvent()), eventsPoll(t, hm(14, 20), ddfeeder.EventsMarker{Pages: 1, Read: 3}))
	got := eventChanges(em)
	if len(got) != 1 {
		t.Fatalf("%d changes, want 1", len(got))
	}
	c := got[0]
	if c.GetRef().GetValue() != "evt-1001" || c.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT ||
		!c.GetValidAt().AsTime().Equal(hms(14, 9, 30)) {
		t.Errorf("change %v", c)
	}
	if c.GetChange().GetSummary() != "Deployed payments to prod" {
		t.Errorf("summary %q", c.GetChange().GetSummary())
	}
	var targets []string
	for _, tr := range c.GetTargets() {
		targets = append(targets, tr.GetNamespace()+"="+tr.GetValue())
	}
	if strings.Join(targets, ",") != "datadog.service=prod/payments,k8s.deployment=shop/payments" {
		t.Errorf("targets %v", targets)
	}
	if c.GetChange().GetOriginRef() != "https://app.datadoghq.eu/event/event?id=evt-1001" || len(c.GetPointers()) != 1 {
		t.Errorf("origin %q pointers %v", c.GetChange().GetOriginRef(), c.GetPointers())
	}
	want := map[string]string{
		feeder.NSDeployCommitSHA: evCommitA + " env=prod",
		feeder.NSDeployImage:     "ghcr.io/twin/payments@sha256:00000000000000000000000000000000000000000000000000000000000000ab env=prod",
		feeder.NSDeployRelease:   "v2.4.0 env=prod",
	}
	got2 := correlationsOf(em)
	for ns, v := range want {
		if got2[ns] != v {
			t.Errorf("%s: %q, want %q", ns, got2[ns], v)
		}
	}
	if len(got2) != len(want) {
		t.Errorf("correlations %v", got2)
	}
}

// FR-031: only a rollout claims a deploy identifier. A configuration change that quotes the commit did not
// ship it, and C8 would merge it with the rollout that did.
func TestOnlyARolloutClaimsADeployIdentifier(t *testing.T) {
	t.Parallel()
	cfg := evt{id: "evt-1002", title: "Changed the payments config", source: "jenkins", kind: "config_change", at: hms(14, 12, 0),
		tags: []string{"env:prod", "service:payments", "git.commit.sha:" + evCommitA}}
	em := run(t, changesOptions(), eventsPayload(t, hm(14, 20), cfg))
	got := eventChanges(em)
	if len(got) != 1 || got[0].GetChange().GetKind() != graphv1.ChangeKind_CONFIG_CHANGE {
		t.Fatalf("changes %v", got)
	}
	if keys := correlationsOf(em); len(keys) != 0 {
		t.Errorf("a configuration change claimed %v", keys)
	}
}

// FR-031: an event stating two commits claims neither, and says so.
func TestTwoCommitsAreNeitherClaimed(t *testing.T) {
	t.Parallel()
	e := deployEvent()
	e.tags = append(e.tags, "commit_sha:"+evCommitB)
	em := run(t, changesOptions(), eventsPayload(t, hm(14, 20), e), eventsPoll(t, hm(14, 21), ddfeeder.EventsMarker{}))
	if _, claimed := correlationsOf(em)[feeder.NSDeployCommitSHA]; claimed {
		t.Error("a commit was claimed from an event that states two")
	}
	if !strings.Contains(checkpointNotes(em), "states two deploy.commit_sha") {
		t.Errorf("notes:\n%s", checkpointNotes(em))
	}
}

// FR-031: a version that is a full commit is a commit; a release label is the release form.
func TestAVersionThatIsACommitIsACommit(t *testing.T) {
	t.Parallel()
	e := evt{id: "evt-1003", source: "jenkins", kind: "deploy", at: hms(14, 9, 0),
		tags: []string{"env:prod", "service:payments", "version:" + evCommitB}}
	keys := correlationsOf(run(t, changesOptions(), eventsPayload(t, hm(14, 20), e)))
	if keys[feeder.NSDeployCommitSHA] != evCommitB+" env=prod" || len(keys) != 1 {
		t.Errorf("keys %v", keys)
	}
}

// FR-028: a kind the taxonomy does not name is "other" with the vendor's kind kept; so is a kind not stated.
func TestAnUnknownKindIsOtherWithTheVendorsKind(t *testing.T) {
	t.Parallel()
	failover := evt{id: "evt-1004", title: "Failed over the payments database", source: "custom_pipeline", kind: "db_failover",
		at: hms(14, 30, 0), tags: []string{"env:prod", "service:payments"}}
	bare := evt{id: "evt-1005", source: "custom_pipeline", at: hms(14, 31, 0), tags: []string{"env:prod", "service:payments"}}
	em := run(t, changesOptions(), eventsPayload(t, hm(14, 40), failover, bare), eventsPoll(t, hm(14, 41), ddfeeder.EventsMarker{}))
	got := eventChanges(em)
	if len(got) != 2 {
		t.Fatalf("%d changes, want 2 (none dropped)", len(got))
	}
	for i, wantOther := range []string{"db_failover", "unstated"} {
		c := got[i]
		if c.GetChange().GetKind() != graphv1.ChangeKind_CHANGE_KIND_OTHER || c.GetChange().GetKindOther() != wantOther {
			t.Errorf("change %d: %v", i, c.GetChange())
		}
	}
	if got[0].GetProps().GetFields()[ddfeeder.PropEventKind].GetStringValue() != "db_failover" {
		t.Errorf("the vendor's kind is not kept as a property: %v", got[0].GetProps())
	}
	if !strings.Contains(checkpointNotes(em), "no equivalent in the published taxonomy") {
		t.Errorf("notes:\n%s", checkpointNotes(em))
	}
}

// FR-030: a change that names no target is kept, and counted unattached.
func TestAnEventWithNoTargetIsKeptAndCountedUnattached(t *testing.T) {
	t.Parallel()
	e := evt{id: "evt-1006", title: "Rotated a certificate", source: "custom_pipeline", kind: "secret_rotation", at: hms(14, 5, 0),
		tags: []string{"env:prod"}}
	em := run(t, changesOptions(), eventsPayload(t, hm(14, 20), e), eventsPoll(t, hm(14, 21), ddfeeder.EventsMarker{}))
	got := eventChanges(em)
	if len(got) != 1 || len(got[0].GetTargets()) != 0 {
		t.Fatalf("changes %v", got)
	}
	if !strings.Contains(checkpointNotes(em), "unattached changes (FR-030)") {
		t.Errorf("notes:\n%s", checkpointNotes(em))
	}
}

// FR-033a, FR-033b: the actor kind comes from the trigger tag and the source, never from the name.
func TestTheActorKindComesFromEvidenceNeverFromTheName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		source   string
		tags     []string
		want     graphv1.ActorKind
		actor    string
		evidence string
	}{
		{"a CI source", "jenkins", []string{"user:release-runner"}, graphv1.ActorKind_AUTOMATION, "release-runner", ""},
		{"an autoscaler source", "karpenter", nil, graphv1.ActorKind_CONTROLLER, "", ""},
		{"a provider's health source", "aws_health", nil, graphv1.ActorKind_VENDOR, "", ""},
		{"a person's trigger on a pipeline outranks the tool", "jenkins", []string{"triggered_by:manual", "user:dev-pat"}, graphv1.ActorKind_PERSON, "dev-pat", ""},
		{"a bot-looking name on an unlisted source is unknown", "custom_pipeline", []string{"user:deploy-bot"}, graphv1.ActorKind_ACTOR_KIND_UNKNOWN, "deploy-bot",
			"an actor is named, and a name does not classify itself"},
		{"a person-looking name on a CI source is automation", "jenkins", []string{"user:dev-pat"}, graphv1.ActorKind_AUTOMATION, "dev-pat", ""},
		{"nothing at all", "custom_pipeline", nil, graphv1.ActorKind_ACTOR_KIND_UNKNOWN, "", "no trigger tag"},
		{"a trigger outside the vocabulary", "custom_pipeline", []string{"triggered_by:alice"}, graphv1.ActorKind_ACTOR_KIND_UNKNOWN, "",
			`trigger tag value "alice" is on no published list`},
	} {
		e := evt{id: "evt-2001", source: tc.source, kind: "deployment", at: hms(14, 0, 0),
			tags: append([]string{"env:prod", "service:payments"}, tc.tags...)}
		got := eventChanges(run(t, changesOptions(), eventsPayload(t, hm(14, 20), e)))
		if len(got) != 1 {
			t.Fatalf("%s: %d changes", tc.name, len(got))
		}
		c := got[0]
		if c.GetChange().GetActorKind() != tc.want || c.GetChange().GetActor() != tc.actor {
			t.Errorf("%s: kind %s actor %q, want %s %q", tc.name, c.GetChange().GetActorKind(), c.GetChange().GetActor(), tc.want, tc.actor)
		}
		evidence := c.GetProps().GetFields()[ddfeeder.PropActorEvidence].GetStringValue()
		if tc.want == graphv1.ActorKind_ACTOR_KIND_UNKNOWN && !strings.Contains(evidence, tc.evidence) {
			t.Errorf("%s: an unknown actor kind carries the evidence %q, want it to say %q", tc.name, evidence, tc.evidence)
		}
		if tc.want != graphv1.ActorKind_ACTOR_KIND_UNKNOWN && evidence != "" {
			t.Errorf("%s: a classified actor carries evidence %q", tc.name, evidence)
		}
	}
}

// FR-032: the connector emits its own observation whatever another source saw, and a re-delivery (the
// window overlap) re-sends ids already sent.
func TestAnEventRedeliveredIsTheSameEvent(t *testing.T) {
	t.Parallel()
	first := run(t, changesOptions(), eventsPayload(t, hm(14, 20), deployEvent()))
	twice := run(t, changesOptions(), eventsPayload(t, hm(14, 20), deployEvent()), eventsPayload(t, hm(14, 25), deployEvent()))
	ids := func(em *emit.MemoryEmitter) map[string]bool {
		out := map[string]bool{}
		for _, ev := range em.Events() {
			out[ev.GetEventId()] = true
		}
		return out
	}
	a, b := ids(first), ids(twice)
	if len(a) != len(b) {
		t.Fatalf("a redelivery minted new ids: %d vs %d", len(a), len(b))
	}
	for id := range b {
		if !a[id] {
			t.Errorf("id %s only in the redelivery", id)
		}
	}
}

// FR-008b: with the capability off, an events payload is refused, not quietly emitted.
func TestEventsAreRefusedWithTheCapabilityOff(t *testing.T) {
	t.Parallel()
	f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []feeder.Payload{eventsPayload(t, hm(14, 20), deployEvent()), eventsPoll(t, hm(14, 20), ddfeeder.EventsMarker{})} {
		em := emit.NewMemoryEmitter(f.Describe())
		err := f.Run(context.Background(), sliceOf(p), em)
		if err == nil || !strings.Contains(err.Error(), "changes capability off") {
			t.Errorf("%s: err %v", p.Kind, err)
		}
		if len(em.Events()) != 0 {
			t.Errorf("%s: %d events emitted with the capability off", p.Kind, len(em.Events()))
		}
	}
}

// An event Datadog gives no instant is not dated at the poll.
func TestAnUndatedEventIsNotEmittedAtThePollInstant(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"data":[{"id":"evt-3001","attributes":{"tags":["env:prod","service:payments"],"attributes":{"source_type_name":"jenkins"}}}]}`)
	em := run(t, changesOptions(), feeder.Payload{Kind: ddfeeder.PayloadEvents, At: hm(14, 20), Bytes: raw},
		eventsPoll(t, hm(14, 21), ddfeeder.EventsMarker{}))
	if n := len(eventChanges(em)); n != 0 {
		t.Errorf("%d changes from an undated event", n)
	}
	if !strings.Contains(checkpointNotes(em), "evt-3001: Datadog states no instant") {
		t.Errorf("notes:\n%s", checkpointNotes(em))
	}
}

// The reader is tolerant of the types Datadog varies: a numeric id, an epoch-millisecond instant.
func TestTheReaderIsTolerantOfVariedTypes(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"data":[{"id":4711,"attributes":{"timestamp":1789999200000,"tags":["env:prod","service:payments"],` +
		`"attributes":{"source_type_name":"jenkins","evt":{"id":9,"type":"deployment"}}}}]}`)
	got := eventChanges(run(t, changesOptions(), feeder.Payload{Kind: ddfeeder.PayloadEvents, At: hm(14, 20), Bytes: raw}))
	if len(got) != 1 || got[0].GetRef().GetValue() != "4711" || got[0].GetValidAt().AsTime().UnixMilli() != 1789999200000 {
		t.Fatalf("changes %v", got)
	}
}

// FR-029: the configuration in force is in the checkpoint, and a partial window declares its gap.
func TestTheEventsCheckpointStatesTheScopeAndTheGap(t *testing.T) {
	t.Parallel()
	em := run(t, changesOptions(),
		eventsPayload(t, hm(14, 20), deployEvent()),
		eventsPoll(t, hm(14, 20), ddfeeder.EventsMarker{Pages: 1, Read: 40, OutOfScope: 39}),
		eventsPoll(t, hm(14, 30), ddfeeder.EventsMarker{Outcome: "partial", Reason: "page 1: datadog answered 504"}))
	notes := checkpointNotes(em)
	for _, want := range []string{"sources [jenkins, custom_pipeline], tags [event_type:deployment]", "changes=on",
		"events read: 40; out of scope: 39", "partial: page 1: datadog answered 504", "unread, not absent"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes do not say %q:\n%s", want, notes)
		}
	}
	var checkpoints []*graphv1.SourceCheckpoint
	for _, ev := range em.Events() {
		if c := ev.GetSourceCheckpoint(); c != nil {
			checkpoints = append(checkpoints, c)
		}
	}
	if len(checkpoints) != 2 || checkpoints[0].GetGapBefore() || !checkpoints[1].GetGapBefore() {
		t.Fatalf("checkpoints %v", checkpoints)
	}
	if !checkpoints[1].GetExtentFrom().AsTime().Equal(checkpoints[1].GetExtentTo().AsTime()) {
		t.Errorf("a partial window claims an extent: %v", checkpoints[1])
	}
}

// FR-029: the scope is validated, and a scope with the capability off is refused.
func TestTheChangeScopeIsValidated(t *testing.T) {
	t.Parallel()
	on := ddfeeder.Capabilities{ddfeeder.CapChanges: true}
	for _, tc := range []struct {
		name string
		opts ddfeeder.Options
		want string
	}{
		{"nothing selects", ddfeeder.Options{OrgSlug: "twin", Capabilities: on}, "names no event source or tag"},
		{"a malformed tag", ddfeeder.Options{OrgSlug: "twin", Capabilities: on, Changes: ddfeeder.ChangeScope{Tags: []string{"deployment"}}}, "key:value"},
		{"a source with a space", ddfeeder.Options{OrgSlug: "twin", Capabilities: on, Changes: ddfeeder.ChangeScope{Sources: []string{"my tool"}}}, "single event source"},
		{"a scope with the capability off", ddfeeder.Options{OrgSlug: "twin", Changes: ddfeeder.ChangeScope{Sources: []string{"jenkins"}}}, "capability is off"},
	} {
		if _, err := ddfeeder.New(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
	scope := ddfeeder.ChangeScope{Sources: []string{"jenkins", "argocd"}, Tags: []string{"event_type:deployment"}}
	if got := scope.Query(); got != `source:jenkins OR source:argocd OR tags:"event_type:deployment"` {
		t.Errorf("query %q", got)
	}
}

// ---- the poller -------------------------------------------------------------------------------------

// eventPager serves pages of raw events, recording the queries it was asked.
type eventPager struct {
	pages   []string // each page's body
	failAt  int      // the page index that fails, or -1
	queries []ddfeeder.EventsQuery
	reads   int64
}

func (p *eventPager) ListEventsPage(_ context.Context, q ddfeeder.EventsQuery) ([]byte, error) {
	atomic.AddInt64(&p.reads, 1)
	p.queries = append(p.queries, q)
	idx := len(p.queries) - 1
	if idx == p.failAt {
		return nil, errors.New("datadog answered 504")
	}
	if idx >= len(p.pages) {
		return []byte(`{"data":[]}`), nil
	}
	return []byte(p.pages[idx]), nil
}

func eventPage(after string, events ...evt) string {
	data := make([]map[string]any, 0, len(events))
	for _, e := range events {
		data = append(data, e.json())
	}
	body := map[string]any{"data": data}
	if after != "" {
		body["meta"] = map[string]any{"page": map[string]any{"after": after}}
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func eventsPoller(pager *eventPager, out *collected, now *time.Time) *ddfeeder.Poller {
	return &ddfeeder.Poller{
		Events: pager, Changes: changesOptions().Changes, Capabilities: changesOptions().Capabilities,
		Push: out.push, Now: func() time.Time { return *now },
	}
}

func lastEventsMarker(t *testing.T, out *collected) ddfeeder.EventsMarker {
	t.Helper()
	var m ddfeeder.EventsMarker
	if err := json.Unmarshal(out.payloads[len(out.payloads)-1].Bytes, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The poller applies the scope: only in-scope events are pushed, the marker counts the rest, and the
// pages are followed by cursor.
func TestTheEventsPollKeepsTheScopeAndFollowsTheCursor(t *testing.T) {
	t.Parallel()
	monitorAlert := evt{id: "evt-9001", title: "monitor triggered", source: "monitor_alert", at: hms(14, 1, 0), tags: []string{"env:prod"}}
	tagged := evt{id: "evt-9002", source: "someone", at: hms(14, 2, 0), tags: []string{"Event_Type:Deployment", "env:prod", "service:payments"}}
	pager := &eventPager{failAt: -1, pages: []string{
		eventPage("cursor-2", deployEvent(), monitorAlert),
		eventPage("", tagged),
	}}
	now := hm(14, 20)
	var out collected
	if err := eventsPoller(pager, &out, &now).PollEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out.kinds() != "events,events,events-poll" {
		t.Fatalf("payloads %s", out.kinds())
	}
	if got := len(pager.queries); got != 2 || pager.queries[1].Cursor != "cursor-2" || pager.queries[0].Cursor != "" {
		t.Fatalf("queries %+v", pager.queries)
	}
	m := lastEventsMarker(t, &out)
	if m.Outcome != "complete" || m.Read != 3 || m.OutOfScope != 1 || m.Pages != 2 {
		t.Errorf("marker %+v", m)
	}
	if !pager.queries[0].To.Equal(now) || !pager.queries[0].From.Equal(now.Add(-ddfeeder.DefaultHistory)) {
		t.Errorf("first window %v to %v", pager.queries[0].From, pager.queries[0].To)
	}
	// What was pushed holds no out-of-scope event.
	if strings.Contains(string(out.payloads[0].Bytes), "evt-9001") {
		t.Error("an out-of-scope event was pushed")
	}
}

// The next window reaches back from the last complete one by the overlap; a partial window does not move
// it, so its gap is read again.
func TestTheNextWindowReachesBackFromTheLastCompleteOne(t *testing.T) {
	t.Parallel()
	pager := &eventPager{failAt: -1}
	now := hm(14, 20)
	var out collected
	p := eventsPoller(pager, &out, &now)
	if err := p.PollEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = hm(14, 25)
	pager.failAt = len(pager.queries) // the next read fails
	if err := p.PollEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := lastEventsMarker(t, &out); m.Outcome != "partial" || !strings.Contains(m.Reason, "504") {
		t.Fatalf("marker %+v", m)
	}
	now = hm(14, 30)
	if err := p.PollEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := pager.queries[len(pager.queries)-1]
	if want := hm(14, 20).Add(-ddfeeder.DefaultEventsOverlap); !last.From.Equal(want) || !last.To.Equal(hm(14, 30)) {
		t.Errorf("window %v to %v, want from %v (the last complete window's end less the overlap)", last.From, last.To, want)
	}
}

// The poller is silent with the capability off: it reads nothing and pushes nothing.
func TestTheEventsPollIsSilentWithTheCapabilityOff(t *testing.T) {
	t.Parallel()
	pager := &eventPager{failAt: -1}
	now := hm(14, 20)
	var out collected
	p := eventsPoller(pager, &out, &now)
	p.Capabilities = ddfeeder.DefaultCapabilities()
	if err := p.PollEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pager.reads != 0 || len(out.payloads) != 0 {
		t.Errorf("%d reads, %d payloads with the capability off", pager.reads, len(out.payloads))
	}
}

// A page Datadog says is incomplete ends the window as partial, after its events are pushed.
func TestAnIncompletePageEndsTheWindowPartial(t *testing.T) {
	t.Parallel()
	body := strings.TrimSuffix(eventPage("", deployEvent()), "}") + `,"meta":{"status":"timeout","page":{"after":"more"}}}`
	pager := &eventPager{failAt: -1, pages: []string{body}}
	now := hm(14, 20)
	var out collected
	if err := eventsPoller(pager, &out, &now).PollEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out.kinds() != "events,events-poll" || lastEventsMarker(t, &out).Outcome != "partial" {
		t.Fatalf("payloads %s, marker %+v", out.kinds(), lastEventsMarker(t, &out))
	}
	if len(pager.queries) != 1 {
		t.Errorf("%d reads after an incomplete page", len(pager.queries))
	}
}
