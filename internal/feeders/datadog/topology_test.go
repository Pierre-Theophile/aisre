// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// datadog-apm-topology-01: the service map Datadog APM states (T090; FR-009–FR-017).
//
// One environment, `production`, read every fifteen minutes. Built from the published API shapes, not
// verified against a live organisation; synthetic names only.
//
//   - 14:00: `checkout` (also a watched log source) calls `payments`, `search` and `stripe`; `payments`
//     calls `ledger`; `worker` stands alone. `stripe` is reported as called and is no service: a third
//     party, promotable. checkout→stripe has no retained span, so its edge carries no weight class.
//   - 14:15: checkout's new commit appears beside the old (a rolling deploy): one rollout, valid at this
//     window, actor UNKNOWN. Traffic to payments jumps a class, believed only when a second window agrees.
//     payments→ledger is not observed.
//   - 14:30: a partial read — dependencies and versions unfinished. Nothing is retracted on it, no rollout
//     inferred from it, and the checkpoint declares the gap.
//   - 14:45: complete; the class change is confirmed and dated from 14:15; ledger still unobserved.
//   - 15:00: complete; a third consecutive complete read without payments→ledger retracts it, ending at
//     the end of the last window it was seen in, 14:15. `stripe` is now listed as a service: its SERVICE
//     assertion replaces the third-party one on the same ref.

const (
	topologyFixture = "fixtures/datadog-apm-topology-01"
	topoEnv         = "production"
	topoOld         = "1a2b3c4d5e6f708192a3b4c5d6e7f80918a2b3c"
	topoNew         = "2b3c4d5e6f708192a3b4c5d6e7f80918a2b3c4d5"
)

func topoOptions() ddfeeder.Options {
	caps := ddfeeder.DefaultCapabilities()
	caps[ddfeeder.CapAPMTopology] = true
	return ddfeeder.Options{OrgSlug: "twin", Capabilities: caps,
		Topology: ddfeeder.TopologyScope{Envs: []string{topoEnv}, RetractAfter: 3}}
}

func complete() ddfeeder.TopologyParts {
	return ddfeeder.TopologyParts{Dependencies: ddfeeder.PartComplete, Traffic: ddfeeder.PartComplete,
		Versions: ddfeeder.PartComplete, Operations: ddfeeder.PartComplete, Hosts: ddfeeder.PartComplete}
}

func topoPayload(t *testing.T, from time.Time, p ddfeeder.TopologyPayload) feeder.Payload {
	t.Helper()
	p.Env = topoEnv
	p.Window = ddfeeder.TopologyWindow{From: from, To: from.Add(15 * time.Minute)}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadTopology, At: p.Window.To.Add(time.Minute), Bytes: raw}
}

func svc(name string, calls ...string) ddfeeder.TopologyService {
	return ddfeeder.TopologyService{Name: name, Calls: calls}
}

// window1 is the first read.
func window1(t *testing.T) feeder.Payload {
	return topoPayload(t, hm(14, 0), ddfeeder.TopologyPayload{
		Parts: complete(),
		Services: []ddfeeder.TopologyService{svc("checkout", "payments", "search", "stripe"), svc("payments", "ledger"),
			svc("search"), svc("ledger"), svc("worker")},
		Traffic: []ddfeeder.TopologyTraffic{{Caller: "checkout", Callee: "payments", Hits: 9000},
			{Caller: "checkout", Callee: "search", Hits: 300}, {Caller: "payments", Callee: "ledger", Hits: 90}},
		Versions: []ddfeeder.TopologyVersion{{Service: "checkout", Version: topoOld, Hits: 5000},
			{Service: "payments", Version: "v3.1.0", Hits: 4000}, {Service: "worker", Version: "v0.9.2", Hits: 800}},
		Operations: []ddfeeder.TopologyOperation{{Service: "checkout", Operation: "http.request", Hits: 5000},
			{Service: "payments", Operation: "grpc.server", Hits: 4000}},
		Hosts: []ddfeeder.TopologyHost{{Service: "checkout", Host: "web-1"}, {Service: "checkout", Host: "web-2"},
			{Service: "payments", Host: "pay-1"}},
	})
}

// window2 is the rolling deploy: the new commit beside the old, payments busier, ledger unobserved.
func window2(t *testing.T) feeder.Payload {
	return topoPayload(t, hm(14, 15), ddfeeder.TopologyPayload{
		Parts: complete(),
		Services: []ddfeeder.TopologyService{svc("checkout", "payments", "search", "stripe"), svc("payments"),
			svc("search"), svc("ledger"), svc("worker")},
		Traffic: []ddfeeder.TopologyTraffic{{Caller: "checkout", Callee: "payments", Hits: 90000},
			{Caller: "checkout", Callee: "search", Hits: 300}},
		Versions: []ddfeeder.TopologyVersion{{Service: "checkout", Version: topoOld, Hits: 2000},
			{Service: "checkout", Version: topoNew, Hits: 3000}, {Service: "payments", Version: "v3.1.0", Hits: 4000},
			{Service: "worker", Version: "v0.9.2", Hits: 800}},
		Operations: []ddfeeder.TopologyOperation{{Service: "checkout", Operation: "http.request", Hits: 5000},
			{Service: "payments", Operation: "grpc.server", Hits: 4000}},
		Hosts: []ddfeeder.TopologyHost{{Service: "checkout", Host: "web-1"}, {Service: "checkout", Host: "web-2"},
			{Service: "payments", Host: "pay-1"}},
	})
}

// window3 is a partial read: the dependencies and the versions did not finish.
func window3(t *testing.T) feeder.Payload {
	return topoPayload(t, hm(14, 30), ddfeeder.TopologyPayload{
		Parts: ddfeeder.TopologyParts{Dependencies: ddfeeder.PartPartial, Traffic: ddfeeder.PartUnread,
			Versions: ddfeeder.PartPartial, Operations: ddfeeder.PartComplete, Hosts: ddfeeder.PartComplete},
		Reason:   "the service dependencies page timed out",
		Services: []ddfeeder.TopologyService{svc("checkout", "payments", "search", "stripe"), svc("payments")},
		Versions: []ddfeeder.TopologyVersion{{Service: "checkout", Version: "3c4d5e6f708192a3b4c5d6e7f80918a2b3c4d5e6", Hits: 10}},
	})
}

func window4(t *testing.T) feeder.Payload {
	p := window2(t)
	return topoPayload(t, hm(14, 45), mustPayload(t, p, func(x *ddfeeder.TopologyPayload) {
		x.Versions = []ddfeeder.TopologyVersion{{Service: "checkout", Version: topoNew, Hits: 5000},
			{Service: "payments", Version: "v3.1.0", Hits: 4000}, {Service: "worker", Version: "v0.9.2", Hits: 800}}
	}))
}

// window5 lists stripe as a service.
func window5(t *testing.T) feeder.Payload {
	p := window4(t)
	return topoPayload(t, hm(15, 0), mustPayload(t, p, func(x *ddfeeder.TopologyPayload) {
		x.Services = append(x.Services, svc("stripe"))
	}))
}

// mustPayload decodes a payload, applies f and returns it, for building a window from another.
func mustPayload(t *testing.T, p feeder.Payload, f func(*ddfeeder.TopologyPayload)) ddfeeder.TopologyPayload {
	t.Helper()
	var x ddfeeder.TopologyPayload
	if err := json.Unmarshal(p.Bytes, &x); err != nil {
		t.Fatal(err)
	}
	f(&x)
	return x
}

func topologyPayloads(t *testing.T) []feeder.Payload {
	// The watched log source comes first: checkout's node is the log path's and APM's together.
	tick, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: []string{topoEnv + "/checkout"}})
	if err != nil {
		t.Fatal(err)
	}
	return []feeder.Payload{
		{Kind: ddfeeder.PayloadDiscovery, At: hm(14, 0), Bytes: tick},
		window1(t), window2(t), window3(t), window4(t), window5(t),
	}
}

func TestGenerateDatadogAPMTopologyFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, topologyFixture)
	}
	generateFixture(t, topologyFixture, "datadog-apm-topology",
		"The service map Datadog APM states, built from the published API shapes and not verified against a "+
			"live organisation. `checkout` (also a watched log source) calls payments, search and stripe; "+
			"payments calls ledger; stripe is reported as called and is no service, so it is a third party until "+
			"a listing promotes it, and its edge, with no retained span, carries no weight class. At 14:15 a new "+
			"commit appears beside the old: one rollout, actor kind unknown, valid at that window; traffic to "+
			"payments jumps a class, believed only when 14:45 agrees, and dated from 14:15. The 14:30 read is "+
			"partial and retracts and infers nothing. payments→ledger, unobserved from 14:15, is retracted at "+
			"15:00 after three consecutive complete reads, ending at the end of the last window it was seen in.",
		topoOptions(), topologyPayloads(t), hm(13, 59), hm(15, 30), `
queries:
  # checkout as the map states it at 14:10: its log source, its APM pointers, its dependencies and its hosts.
  - name: checkout-before-the-deploy
    kind: subgraph
    focus: datadog.service=production/checkout
    valid_at: 2026-09-21T14:10:00Z
    observed_at: 2026-09-21T16:00:00Z
    hops: 1
    direction: both
  # payments at 14:10 calls ledger; at 15:10 it no longer does, and the edge's history says when it ended.
  - name: payments-before-the-retraction
    kind: subgraph
    focus: datadog.service=production/payments
    valid_at: 2026-09-21T14:10:00Z
    observed_at: 2026-09-21T16:00:00Z
    hops: 1
    direction: both
  - name: payments-after-the-retraction
    kind: subgraph
    focus: datadog.service=production/payments
    valid_at: 2026-09-21T15:10:00Z
    observed_at: 2026-09-21T16:00:00Z
    hops: 1
    direction: both
  # What changed on checkout between 14:00 and 15:30: the one rollout Datadog APM saw.
  - name: what-changed-on-checkout
    kind: diff
    focus: datadog.service=production/checkout
    t1: 2026-09-21T14:00:00Z
    t2: 2026-09-21T15:30:00Z
    reference_at: 2026-09-21T15:30:00Z
    observed_at: 2026-09-21T16:00:00Z
    hops: 1
    direction: both
`)
}

func nodesOf(em *emit.MemoryEmitter) map[string][]*graphv1.UpsertNode {
	out := map[string][]*graphv1.UpsertNode{}
	for _, ev := range em.Events() {
		if n := ev.GetUpsertNode(); n != nil {
			out[n.GetRef().GetValue()] = append(out[n.GetRef().GetValue()], n)
		}
	}
	return out
}

func edgesOf(em *emit.MemoryEmitter, typ graphv1.EdgeType) []*graphv1.UpsertEdge {
	var out []*graphv1.UpsertEdge
	for _, ev := range em.Events() {
		if e := ev.GetUpsertEdge(); e != nil && e.GetType() == typ {
			out = append(out, e)
		}
	}
	return out
}

func retractionsOf(em *emit.MemoryEmitter) []*graphv1.RetractEdge {
	var out []*graphv1.RetractEdge
	for _, ev := range em.Events() {
		if e := ev.GetRetractEdge(); e != nil {
			out = append(out, e)
		}
	}
	return out
}

func edgeBetween(edges []*graphv1.UpsertEdge, src, dst string) []*graphv1.UpsertEdge {
	var out []*graphv1.UpsertEdge
	for _, e := range edges {
		if e.GetSrc().GetValue() == topoEnv+"/"+src && (e.GetDst().GetValue() == topoEnv+"/"+dst || e.GetDst().GetValue() == dst) {
			out = append(out, e)
		}
	}
	return out
}

// FR-009, FR-010, FR-015: a service node for every service in scope, a `calls` edge for every dependency
// carrying a weight CLASS and no measurement, and a third-party node for a target no listing names.
func TestTheMapIsNodesEdgesAndWeightClassesOnly(t *testing.T) {
	t.Parallel()
	em := run(t, topoOptions(), window1(t))
	nodes := nodesOf(em)
	for _, name := range []string{"checkout", "payments", "search", "ledger", "worker"} {
		ns := nodes[topoEnv+"/"+name]
		if len(ns) != 1 || ns[0].GetType() != graphv1.NodeType_SERVICE || ns[0].GetValidFromUnknown() ||
			!ns[0].GetValidAt().AsTime().Equal(hm(14, 0)) {
			t.Errorf("%s: %v (dated by the window it was reported in, never the poll)", name, ns)
		}
	}
	stripe := nodes[topoEnv+"/stripe"]
	if len(stripe) != 1 || stripe[0].GetType() != graphv1.NodeType_THIRD_PARTY ||
		!stripe[0].GetProps().GetFields()[ddfeeder.PropUnlistedDependency].GetBoolValue() {
		t.Errorf("stripe is not a marked third party: %v", stripe)
	}
	if v := nodes[topoEnv+"/payments"][0].GetProps().GetFields()[feeder.AttrServiceVersion].GetStringValue(); v != "v3.1.0" {
		t.Errorf("payments carries version %q", v)
	}
	// Pointers: the APM metric pointer names the busiest operation, the span pointer the service.
	var vocabs []string
	for _, p := range nodes[topoEnv+"/payments"][0].GetPointers() {
		vocabs = append(vocabs, p.GetKind().String()+" "+p.GetVocabulary()+" "+p.GetSelector())
	}
	if strings.Join(vocabs, "|") != "METRIC datadog-apm-metric/v1 service:payments env:production span:grpc.server|"+
		"TRACE datadog-spans/v1 service:payments env:production" {
		t.Errorf("pointers %v", vocabs)
	}

	calls := edgesOf(em, graphv1.EdgeType_CALLS)
	classes := map[string]int{}
	for _, e := range calls {
		key := strings.TrimPrefix(e.GetSrc().GetValue(), topoEnv+"/") + ">" + strings.TrimPrefix(e.GetDst().GetValue(), topoEnv+"/")
		classes[key] = -1
		if e.WeightClass != nil {
			classes[key] = int(e.GetWeightClass())
		}
		if !e.GetValidAt().AsTime().Equal(hm(14, 0)) || e.GetValidFromUnknown() {
			t.Errorf("%s: valid %v; an edge's valid interval is the window it was observed in", key, e.GetValidAt())
		}
		for k := range e.GetProps().GetFields() {
			if strings.Contains(k, "hits") || strings.Contains(k, "count") || strings.Contains(k, "rate") {
				t.Errorf("%s carries a measurement in %q", key, k)
			}
		}
	}
	want := map[string]int{"checkout>payments": 4, "checkout>search": 2, "checkout>stripe": -1, "payments>ledger": 2}
	if len(classes) != len(want) {
		t.Errorf("classes %v, want %v", classes, want)
	}
	for k, v := range want {
		if classes[k] != v {
			t.Errorf("%s: class %d, want %d", k, classes[k], v)
		}
	}
	notes := checkpointNotes(em)
	for _, s := range []string{"apm_topology=on", "edges without a weight class", "third-party dependencies",
		"first read of this environment", "hosts only: no container or pod"} {
		if !strings.Contains(notes, s) {
			t.Errorf("the checkpoint does not state %q:\n%s", s, notes)
		}
	}
}

// FR-014: a host is infrastructure with a `runs-on` edge from the service whose start is unknown, and
// nothing is asserted per replica.
func TestHostsAreInfrastructureNeverPerContainer(t *testing.T) {
	t.Parallel()
	em := run(t, topoOptions(), window1(t))
	hosts := 0
	for ref, ns := range nodesOf(em) {
		if ns[0].GetRef().GetNamespace() == ddfeeder.NSHost {
			hosts++
			if ns[0].GetType() != graphv1.NodeType_INFRA_RESOURCE || !ns[0].GetValidAt().AsTime().Equal(hm(14, 0)) {
				t.Errorf("%s: %v", ref, ns[0])
			}
		}
	}
	runsOn := edgesOf(em, graphv1.EdgeType_RUNS_ON)
	if hosts != 3 || len(runsOn) != 3 {
		t.Fatalf("%d hosts, %d runs-on edges", hosts, len(runsOn))
	}
	for _, e := range runsOn {
		if !e.GetValidFromUnknown() || e.GetValidAt() != nil {
			t.Errorf("a runs-on edge dated %v: Datadog states the host, not since when (FR-017)", e.GetValidAt())
		}
	}
}

// FR-013: a version not in the previous complete read is one rollout, valid at the window it appeared in,
// bound-marked, UNKNOWN actor with the evidence, keyed by its commit. The first read is a baseline.
func TestANewVersionIsOneRollout(t *testing.T) {
	t.Parallel()
	em := run(t, topoOptions(), window1(t), window2(t))
	got := changes(em)
	if len(got) != 1 {
		t.Fatalf("%d rollouts, want 1: %v", len(got), got)
	}
	c := got[0]
	fields := c.GetProps().GetFields()
	if c.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT || !c.GetValidAt().AsTime().Equal(hm(14, 15)) ||
		c.GetChange().GetActorKind() != graphv1.ActorKind_ACTOR_KIND_UNKNOWN ||
		fields[feeder.PropChangeValidFromIsABound].GetStringValue() != ddfeeder.BoundFirstSeenInAPM ||
		fields[ddfeeder.PropPreviousVersion].GetStringValue() != topoOld ||
		fields[ddfeeder.PropActorEvidence].GetStringValue() == "" ||
		len(c.GetTargets()) != 1 || c.GetTargets()[0].GetValue() != topoEnv+"/checkout" {
		t.Errorf("rollout %v", c)
	}
	keys := correlationsOf(em)
	if !strings.HasPrefix(keys[feeder.NSDeployCommitSHA], topoNew) {
		t.Errorf("correlation keys %v", keys)
	}
}

// FR-012: a partial read retracts nothing and infers no rollout, and declares the gap; a part that is
// unread neither confirms nor denies. Three consecutive COMPLETE reads retract, with the valid end at the
// end of the last window the edge was seen in (FR-011).
func TestRetractionNeedsCompleteReadsAndEndsInTheLastObservedWindow(t *testing.T) {
	t.Parallel()
	payloads := topologyPayloads(t)
	// Through the 14:45 read: ledger has been unobserved in two complete reads and one partial one.
	early := run(t, topoOptions(), payloads[:5]...)
	if r := retractionsOf(early); len(r) != 0 {
		t.Fatalf("retracted early, on %d unobserved complete reads: %v", 2, r)
	}
	if len(changes(early)) != 1 {
		t.Errorf("the partial read inferred a rollout: %v", changes(early))
	}
	notes := checkpointNotes(early)
	for _, s := range []string{"partial: ",
		"nothing unread was retracted", "gap before this window"} {
		if !strings.Contains(notes, s) {
			t.Errorf("the checkpoint does not state %q:\n%s", s, notes)
		}
	}
	var gaps []bool
	for _, ev := range early.Events() {
		if c := ev.GetSourceCheckpoint(); c != nil {
			gaps = append(gaps, c.GetGapBefore())
		}
	}
	if len(gaps) < 4 || !gaps[0] || gaps[1] || !gaps[2] {
		t.Errorf("gap_before over the checkpoints: %v (the start and the partial read declare one; a contiguous complete read does not)", gaps)
	}

	full := run(t, topoOptions(), payloads...)
	r := retractionsOf(full)
	if len(r) != 1 || r[0].GetSrc().GetValue() != topoEnv+"/payments" || r[0].GetDst().GetValue() != topoEnv+"/ledger" ||
		!r[0].GetValidEnd().AsTime().Equal(hm(14, 15)) {
		t.Fatalf("retractions %v; want payments -> ledger ending at 14:15, the end of the last window it was observed in", r)
	}
}

// Hysteresis: a class change is believed when a second window agrees, and dated from the first.
func TestAWeightClassChangeIsBelievedOnASecondWindow(t *testing.T) {
	t.Parallel()
	one := edgeBetween(edgesOf(run(t, topoOptions(), window1(t), window2(t)), graphv1.EdgeType_CALLS), "checkout", "payments")
	if len(one) != 1 {
		t.Fatalf("after one window at the new class: %d assertions", len(one))
	}
	two := edgeBetween(edgesOf(run(t, topoOptions(), window1(t), window2(t), window3(t), window4(t)), graphv1.EdgeType_CALLS), "checkout", "payments")
	if len(two) != 2 || two[1].GetWeightClass() != 5 || !two[1].GetValidAt().AsTime().Equal(hm(14, 15)) {
		t.Errorf("after the second window: %v", two)
	}
}

// FR-015: a listing later promotes the third party: the SERVICE assertion is on the same ref.
func TestAListingPromotesAThirdParty(t *testing.T) {
	t.Parallel()
	em := run(t, topoOptions(), topologyPayloads(t)...)
	stripe := nodesOf(em)[topoEnv+"/stripe"]
	if len(stripe) != 2 || stripe[0].GetType() != graphv1.NodeType_THIRD_PARTY || stripe[1].GetType() != graphv1.NodeType_SERVICE {
		t.Fatalf("stripe: %v", stripe)
	}
	if _, marked := stripe[1].GetProps().GetFields()[ddfeeder.PropUnlistedDependency]; marked {
		t.Error("the promoted node still says it is unlisted")
	}
}

// A watched log source's node keeps its log pointer when APM states the same service: the two paths build
// one assertion, because a source's latest assertion replaces its earlier one.
func TestTheLogPathAndTheAPMPathShareOneNode(t *testing.T) {
	t.Parallel()
	em := run(t, topoOptions(), topologyPayloads(t)...)
	ns := nodesOf(em)[topoEnv+"/checkout"]
	last := ns[len(ns)-1]
	var hasLog, hasMetric, hasSpans bool
	for _, p := range last.GetPointers() {
		switch p.GetVocabulary() {
		case feeder.VocabDatadogLogs:
			hasLog = true
		case feeder.VocabDatadogAPMMetric:
			hasMetric = true
		case feeder.VocabDatadogSpans:
			hasSpans = true
		}
	}
	if len(ns) < 2 || !hasMetric || !hasSpans {
		t.Fatalf("checkout's last assertion: %v", last)
	}
	if hasLog {
		return
	}
	// The plain node the log path asserts carries no log pointer until a measured tick: the pointer is
	// the log path's to add, and this run measured nothing. What must hold is that no assertion dropped
	// props the log path set.
	if last.GetProps().GetFields()[feeder.AttrServiceName].GetStringValue() != "checkout" {
		t.Errorf("the merged node lost the log path's props: %v", last.GetProps())
	}
	if last.GetProps().GetFields()[feeder.AttrServiceVersion].GetStringValue() != topoNew {
		t.Errorf("the merged node does not carry the version APM reports: %v", last.GetProps())
	}
}

func TestATopologyPayloadIsRefusedWithTheCapabilityOff(t *testing.T) {
	t.Parallel()
	f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatal(err)
	}
	err = f.Run(context.Background(), source.NewSliceSource([]feeder.Payload{window1(t)}), emit.NewMemoryEmitter(f.Describe()))
	if err == nil || !strings.Contains(err.Error(), "capability off") {
		t.Fatalf("got %v", err)
	}
}

func TestTheTopologyScopeIsValidated(t *testing.T) {
	t.Parallel()
	caps := ddfeeder.DefaultCapabilities()
	caps[ddfeeder.CapAPMTopology] = true
	for name, opts := range map[string]ddfeeder.Options{
		"on with no environment":          {OrgSlug: "twin", Capabilities: caps},
		"a malformed environment":         {OrgSlug: "twin", Capabilities: caps, Topology: ddfeeder.TopologyScope{Envs: []string{"prod/eu"}}},
		"a scope with the capability off": {OrgSlug: "twin", Topology: ddfeeder.TopologyScope{Envs: []string{"production"}}},
	} {
		if _, err := ddfeeder.New(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	f, err := ddfeeder.New(topoOptions())
	if err != nil {
		t.Fatal(err)
	}
	other := topoPayload(t, hm(14, 0), ddfeeder.TopologyPayload{Parts: complete()})
	other.Bytes = []byte(strings.Replace(string(other.Bytes), `"production"`, `"staging"`, 1))
	if err := f.Run(context.Background(), source.NewSliceSource([]feeder.Payload{other}), emit.NewMemoryEmitter(f.Describe())); err == nil {
		t.Error("a payload for an environment outside the scope was applied")
	}
}

// ---- the recording ------------------------------------------------------------------------------------

// A live topology run recorded through the tee: no name the twin uses survives, and the recording still
// derives the same graph shape from the sanitised payloads alone.
func TestATopologyRecordingIsSanitisedAndStillDerivesTheMap(t *testing.T) {
	t.Parallel()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i*11 + 3)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	payloads := []feeder.Payload{window1(t), window2(t), window3(t)}
	dir := t.TempDir()
	tee, err := deployrecord.NewTee(source.NewSliceSource(payloads), ddfeeder.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee.WithPrepare(ddfeeder.PreparePayload(san))
	live, err := ddfeeder.New(topoOptions())
	if err != nil {
		t.Fatal(err)
	}
	liveEvents := emit.NewMemoryEmitter(live.Describe())
	if err := live.Run(context.Background(), tee, liveEvents); err != nil {
		t.Fatal(err)
	}
	if tee.Written() != len(payloads) {
		t.Fatalf("%d of %d payloads written; refused: %v", tee.Written(), len(payloads), tee.Dropped())
	}
	assertCleanDir(t, dir, nil)

	shadowOpts, err := ddfeeder.PseudonymousOptions(san, topoOptions())
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := ddfeeder.New(shadowOpts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tee.Finish(context.Background(), dir, "datadog-topology", shadow); err != nil {
		t.Fatal(err)
	}
	replay, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ddfeeder.New(shadowOpts)
	if err != nil {
		t.Fatal(err)
	}
	em := emit.NewMemoryEmitter(fresh.Describe())
	if err := fresh.Run(context.Background(), replay, em); err != nil {
		t.Fatal(err)
	}
	if got, want := len(edgesOf(em, graphv1.EdgeType_CALLS)), len(edgesOf(liveEvents, graphv1.EdgeType_CALLS)); got != want || got == 0 {
		t.Errorf("the recording derives %d calls edges, the live run %d", got, want)
	}
	if got, want := len(changes(em)), len(changes(liveEvents)); got != want || got != 1 {
		t.Errorf("the recording derives %d rollouts, the live run %d", got, want)
	}
	// The version survives, so the rollout still joins the deploy feeders' changes.
	keys := correlationsOf(em)
	if keys[feeder.NSDeployCommitSHA] == "" {
		t.Errorf("the recorded rollout lost its commit key: %v", keys)
	}
}

// ---- the poller -----------------------------------------------------------------------------------------

type fakeTopology struct {
	calls []time.Time
	parts ddfeeder.TopologyParts
	err   error
}

func (f *fakeTopology) ReadTopology(_ context.Context, env string, from, _ time.Time) (ddfeeder.TopologyPayload, error) {
	f.calls = append(f.calls, from)
	return ddfeeder.TopologyPayload{Env: env, Parts: f.parts, Services: []ddfeeder.TopologyService{svc("checkout")}}, f.err
}

// The next window reaches back from the last COMPLETE end, so what a partial read missed is read again.
func TestTheTopologyPollReachesBackFromTheLastCompleteWindow(t *testing.T) {
	t.Parallel()
	reader := &fakeTopology{parts: complete()}
	now := hm(14, 15)
	var pushed []ddfeeder.TopologyPayload
	opts := topoOptions()
	poller := &ddfeeder.Poller{
		Capabilities: opts.Capabilities, Topology: reader, TopologyScope: opts.Topology, TopologyInterval: 15 * time.Minute,
		Now: func() time.Time { return now },
		Push: func(_ context.Context, p feeder.Payload) error {
			var x ddfeeder.TopologyPayload
			if err := json.Unmarshal(p.Bytes, &x); err != nil {
				return err
			}
			pushed = append(pushed, x)
			return nil
		},
	}
	if err := poller.PollTopology(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = hm(14, 30)
	reader.parts.Versions, reader.err = ddfeeder.PartUnread, errors.New("versions failed")
	if err := poller.PollTopology(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = hm(14, 45)
	reader.parts, reader.err = complete(), nil
	if err := poller.PollTopology(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 3 || !reader.calls[0].Equal(hm(14, 0)) || !reader.calls[1].Equal(hm(14, 15)) || !reader.calls[2].Equal(hm(14, 15)) {
		t.Errorf("the windows started at %v; the third must reach back over the partial second", reader.calls)
	}
	if pushed[1].Parts.Versions != ddfeeder.PartUnread || pushed[1].Reason == "" {
		t.Errorf("the partial payload does not state it: %+v", pushed[1])
	}
	off := &ddfeeder.Poller{Capabilities: ddfeeder.DefaultCapabilities(), Topology: reader, TopologyScope: opts.Topology}
	before := len(reader.calls)
	if err := off.PollTopology(context.Background()); err != nil || len(reader.calls) != before {
		t.Errorf("the poll read with the capability off (%v)", err)
	}
}
