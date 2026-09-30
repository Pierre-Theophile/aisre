// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// SC-024 (T089, T091, T092): with `apm_topology` and `changes` off, the default, the connector requests no
// scope for them, declares no operation of theirs, emits nothing derived from them, and every checkpoint
// names them as off. A disabled capability must read as "we were not looking", never as "there was
// nothing there" (FR-008a, FR-008b).
//
// The last clause (every other capability's goldens byte-identical with these on and off) is a test for
// both: TestEveryOtherCapabilityIsByteIdenticalWithChangesOnAndOff (T092) and
// TestEveryOtherCapabilityIsByteIdenticalWithAPMTopologyOnAndOff (T090) run the fixtures that contain none
// of the capability's payloads both ways.
//
// A fixture whose checkpoints state `changes=on` (datadog-events-merge-01) or `apm_topology=on`
// (datadog-apm-topology-01) is that capability's own, and is held to what it may emit instead: for
// changes, the changes and deploy claims of section D; for apm_topology, the services, dependencies,
// hosts and rollouts of section B, and nothing of the other.

// offScopes are the scopes only the disabled capabilities need.
func offScopes() map[string]ddfeeder.Capability {
	out := map[string]ddfeeder.Capability{}
	for _, c := range []ddfeeder.Capability{ddfeeder.CapAPMTopology, ddfeeder.CapChanges} {
		for _, s := range ddfeeder.ReadScopes[c] {
			out[s] = c
		}
	}
	return out
}

func TestADisabledCapabilityRequestsNoScopeAndDeclaresNoCall(t *testing.T) {
	t.Parallel()
	caps := ddfeeder.DefaultCapabilities()
	if caps.Enabled(ddfeeder.CapAPMTopology) || caps.Enabled(ddfeeder.CapChanges) {
		t.Fatalf("the default capabilities %s enable apm_topology or changes", caps)
	}
	off := offScopes()
	if len(off) == 0 {
		t.Fatal("no scope is published for apm_topology or changes; the assertion below would pass vacuously")
	}
	for _, s := range ddfeeder.AllowedScopes(caps) {
		if c, ok := off[s]; ok {
			t.Errorf("scope %q (%s) is allowed with %s off", s, c, c)
		}
	}
	// No declared operation reads APM, spans, traces, the service catalog or the event stream.
	for _, op := range ddfeeder.SurfaceFor(caps).Operations() {
		path := strings.ToLower(string(op))
		for _, area := range []string{"/apm", "/spans", "/trace", "/services", "/service_dependencies", "/events"} {
			if strings.Contains(path, area) && !strings.Contains(path, "/logs/events") {
				t.Errorf("operation %q reads %s with apm_topology and changes off", op, area)
			}
		}
	}
}

// What the enabled capabilities emit, by kind of event and namespace. Anything outside it is derived
// from a capability that is off, or from one this test does not know: either way it is a failure.
var enabledNamespaces = map[string]bool{
	"datadog.service":     true, // logs, monitors: the watched and alerted services
	"datadog.log_service": true, // logs: the C9 correlation key
	"datadog.monitor":     true, // monitors
	"datadog.change":      true, // logs: log-observed rollouts only (checked below)
	"deploy.commit_sha":   true, // logs: a rollout's version reference
	"deploy.image":        true,
	"deploy.release":      true,
	"owner.team":          true, // tags: ownership claims
	"owner.user":          true,
}

// What only the changes capability adds: an event's target may be a Kubernetes deployment.
var changesNamespaces = map[string]bool{"k8s.deployment": true}

// What only apm_topology adds: the hosts a service runs on.
var apmNamespaces = map[string]bool{"datadog.host": true}

// statesAPMOn is statesChangesOn for the apm_topology capability.
func statesAPMOn(t *testing.T, file string) bool {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), "apm_topology=on")
}

// statesChangesOn reports whether the fixture's Datadog checkpoints state the changes capability on:
// then it is that capability's own fixture, held to what it may emit rather than to silence.
func statesChangesOn(t *testing.T, file string) bool {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), "changes=on")
}

func TestADisabledCapabilityIsSilentAndStatedInEveryFixture(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "fixtures", "datadog-*", "events.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no datadog fixture events (%v)", err)
	}
	checkpoints := 0
	for _, file := range files {
		fixture := filepath.Base(filepath.Dir(file))
		changesOn := statesChangesOn(t, file)
		apmOn := statesAPMOn(t, file)
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for sc.Scan() {
			var ev map[string]json.RawMessage
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("%s: %v", fixture, err)
			}
			var source, id string
			_ = json.Unmarshal(ev["sourceId"], &source)
			_ = json.Unmarshal(ev["eventId"], &id)
			if !strings.HasPrefix(source, "datadog:") {
				continue
			}
			for op, body := range ev {
				switch op {
				case "sourceCheckpoint":
					checkpoints++
					var c struct{ Note string }
					_ = json.Unmarshal(body, &c)
					wantChanges, wantAPM := "changes=off", "apm_topology=off"
					if changesOn {
						wantChanges = "changes=on"
					}
					if apmOn {
						wantAPM = "apm_topology=on"
					}
					if !strings.Contains(c.Note, wantAPM) || !strings.Contains(c.Note, wantChanges) {
						t.Errorf("%s %s: the checkpoint does not state %s and %s:\n%s", fixture, id, wantAPM, wantChanges, c.Note)
					}
				case "upsertEdge":
					var e struct{ Type string }
					_ = json.Unmarshal(body, &e)
					if e.Type != "OWNED_BY" && (!apmOn || (e.Type != "CALLS" && e.Type != "RUNS_ON")) {
						t.Errorf("%s %s: a %s edge; only tags' ownership edges are emitted with apm_topology off", fixture, id, e.Type)
					}
				case "observeChange":
					var c struct {
						Props map[string]string
					}
					_ = json.Unmarshal(body, &c)
					bound := c.Props["sre.change.valid_from_is_a_bound"]
					if !changesOn && bound != "first_seen_in_logs" && (!apmOn || bound != "first_seen_in_apm") {
						t.Errorf("%s %s: a change not observed in logs; with changes off and apm_topology off none is emitted", fixture, id)
					}
				}
				if op == "identityClaim" {
					continue // tags: an allowlisted tag names another platform's identity, whatever its namespace
				}
				for _, ns := range namespacesIn(body) {
					if !enabledNamespaces[ns] && (!changesOn || !changesNamespaces[ns]) && (!apmOn || !apmNamespaces[ns]) {
						t.Errorf("%s %s: %s carries namespace %q, which no enabled capability emits", fixture, id, op, ns)
					}
				}
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if checkpoints == 0 {
		t.Fatal("no datadog checkpoint in any fixture; the statement check passed vacuously")
	}
}

// namespacesIn is every `namespace` value anywhere in an event body.
func namespacesIn(raw json.RawMessage) []string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ns, ok := x["namespace"].(string); ok {
				out = append(out, ns)
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}

// SC-024, last clause (T092): every other capability's output is byte-identical with `changes` on and
// off, over the fixtures that contain no events. The two runs read the same payloads; the only thing that
// may differ is the checkpoint's statement of the capability, which is the point of stating it.
func TestEveryOtherCapabilityIsByteIdenticalWithChangesOnAndOff(t *testing.T) {
	t.Parallel()
	off := ddfeeder.DefaultCapabilities()
	on := ddfeeder.DefaultCapabilities()
	on[ddfeeder.CapChanges] = true
	scope := ddfeeder.ChangeScope{Sources: []string{"jenkins"}, Tags: []string{"event_type:deployment"}}

	sets := map[string]struct {
		opts     ddfeeder.Options
		payloads []feeder.Payload
	}{
		"rollouts": {ddfeeder.Options{OrgSlug: "twin"}, rolloutPayloads(t)},
		"tags":     {ddfeeder.Options{OrgSlug: "twin"}, tagsPayloads(t)},
	}
	for _, fx := range monitorFixtures() {
		sets[fx.dir] = struct {
			opts     ddfeeder.Options
			payloads []feeder.Payload
		}{fixtureOptions(), fx.payloadsOf(t)}
	}
	for name, set := range sets {
		offOpts, onOpts := set.opts, set.opts
		offOpts.Capabilities = off
		onOpts.Capabilities, onOpts.Changes = on, scope
		serialise := func(opts ddfeeder.Options, capsAs string) []string {
			var out []string
			for _, ev := range run(t, opts, set.payloads...).Events() {
				if c := ev.GetSourceCheckpoint(); c != nil {
					// The statement of the capability is the one thing that is meant to differ.
					c.Note = strings.ReplaceAll(c.Note, "changes=off", capsAs)
					c.Note = strings.ReplaceAll(c.Note, "changes=on", capsAs)
				}
				raw, err := protojson.MarshalOptions{}.Marshal(ev)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, string(raw))
			}
			return out
		}
		a, b := serialise(offOpts, "changes=?"), serialise(onOpts, "changes=?")
		if len(a) == 0 {
			t.Fatalf("%s: emitted nothing; the comparison would be vacuous", name)
		}
		if len(a) != len(b) {
			t.Errorf("%s: %d events with changes off, %d with it on", name, len(a), len(b))
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s: event %d differs with changes on:\n off %s\n on  %s", name, i, a[i], b[i])
				break
			}
		}
		// And the statement itself differs, so the comparison is not blind to the note it normalised.
		if !strings.Contains(checkpointNotes(run(t, offOpts, set.payloads...)), "changes=off") ||
			!strings.Contains(checkpointNotes(run(t, onOpts, set.payloads...)), "changes=on") {
			t.Errorf("%s: a checkpoint does not state the capability as it was configured", name)
		}
	}
}

// SC-024, last clause (T090): every other capability's output is byte-identical with `apm_topology` on and
// off, over the payloads that contain no topology. The two runs read the same payloads; the only thing
// that may differ is the checkpoint's statement of the capability. Nothing about a watched log source's
// node, its pointers or its rollouts may move because APM is looking too.
func TestEveryOtherCapabilityIsByteIdenticalWithAPMTopologyOnAndOff(t *testing.T) {
	t.Parallel()
	off := ddfeeder.DefaultCapabilities()
	on := ddfeeder.DefaultCapabilities()
	on[ddfeeder.CapAPMTopology] = true
	scope := ddfeeder.TopologyScope{Envs: []string{"production", "staging"}}

	sets := map[string]struct {
		opts     ddfeeder.Options
		payloads []feeder.Payload
	}{
		"rollouts": {ddfeeder.Options{OrgSlug: "twin"}, rolloutPayloads(t)},
		"tags":     {ddfeeder.Options{OrgSlug: "twin"}, tagsPayloads(t)},
	}
	for _, fx := range monitorFixtures() {
		sets[fx.dir] = struct {
			opts     ddfeeder.Options
			payloads []feeder.Payload
		}{fixtureOptions(), fx.payloadsOf(t)}
	}
	for name, set := range sets {
		offOpts, onOpts := set.opts, set.opts
		offOpts.Capabilities = off
		onOpts.Capabilities, onOpts.Topology = on, scope
		serialise := func(opts ddfeeder.Options) []string {
			var out []string
			for _, ev := range run(t, opts, set.payloads...).Events() {
				if c := ev.GetSourceCheckpoint(); c != nil {
					c.Note = strings.ReplaceAll(c.Note, "apm_topology=off", "apm_topology=?")
					c.Note = strings.ReplaceAll(c.Note, "apm_topology=on", "apm_topology=?")
				}
				raw, err := protojson.MarshalOptions{}.Marshal(ev)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, string(raw))
			}
			return out
		}
		a, b := serialise(offOpts), serialise(onOpts)
		if len(a) == 0 {
			t.Fatalf("%s: emitted nothing; the comparison would be vacuous", name)
		}
		if len(a) != len(b) {
			t.Errorf("%s: %d events with apm_topology off, %d with it on", name, len(a), len(b))
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s: event %d differs with apm_topology on:\n off %s\n on  %s", name, i, a[i], b[i])
				break
			}
		}
		if !strings.Contains(checkpointNotes(run(t, offOpts, set.payloads...)), "apm_topology=off") ||
			!strings.Contains(checkpointNotes(run(t, onOpts, set.payloads...)), "apm_topology=on") {
			t.Errorf("%s: a checkpoint does not state the capability as it was configured", name)
		}
	}
}

// The other direction: the topology payloads of the APM fixture, read with apm_topology on, add only
// what section B allows. A log source's node keeps its own props when APM states the same service.
func TestTheTopologyFixtureAddsOnlyWhatSectionBAllows(t *testing.T) {
	t.Parallel()
	em := run(t, topoOptions(), topologyPayloads(t)...)
	for _, ev := range em.Events() {
		switch {
		case ev.GetUpsertEdge() != nil:
			if typ := ev.GetUpsertEdge().GetType(); typ != graphv1.EdgeType_CALLS && typ != graphv1.EdgeType_RUNS_ON {
				t.Errorf("edge type %s", typ)
			}
		case ev.GetObserveChange() != nil:
			if ev.GetObserveChange().GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT {
				t.Errorf("a %s change", ev.GetObserveChange().GetChange().GetKind())
			}
		}
	}
}
