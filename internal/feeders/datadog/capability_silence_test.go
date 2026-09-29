// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
)

// SC-024, asserted before any APM or event-stream code exists (T089, T091): with `apm_topology` and
// `changes` off, the default, the connector requests no scope for them, declares no operation of theirs,
// emits nothing derived from them, and every checkpoint names them as off. A disabled capability must
// read as "we were not looking", never as "there was nothing there" (FR-008a, FR-008b).
//
// The last clause of SC-024 (every other capability's goldens byte-identical with these on and off) is
// not applicable while they are not built: ParseCapabilities refuses to enable them
// (TestAnUnbuiltCapabilityCannotBeEnabled), so there is no "on" run to compare. It becomes a test with
// the phase that builds either.

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

func TestADisabledCapabilityIsSilentAndStatedInEveryFixture(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "fixtures", "datadog-*", "events.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no datadog fixture events (%v)", err)
	}
	checkpoints := 0
	for _, file := range files {
		fixture := filepath.Base(filepath.Dir(file))
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
					if !strings.Contains(c.Note, "apm_topology=off") || !strings.Contains(c.Note, "changes=off") {
						t.Errorf("%s %s: the checkpoint does not state apm_topology and changes as off:\n%s", fixture, id, c.Note)
					}
				case "upsertEdge":
					var e struct{ Type string }
					_ = json.Unmarshal(body, &e)
					if e.Type != "OWNED_BY" {
						t.Errorf("%s %s: a %s edge; only tags' ownership edges are emitted with apm_topology off", fixture, id, e.Type)
					}
				case "observeChange":
					var c struct {
						Props map[string]string
					}
					_ = json.Unmarshal(body, &c)
					if c.Props["sre.change.valid_from_is_a_bound"] != "first_seen_in_logs" {
						t.Errorf("%s %s: a change not observed in logs; with changes off none is emitted", fixture, id)
					}
				}
				if op == "identityClaim" {
					continue // tags: an allowlisted tag names another platform's identity, whatever its namespace
				}
				for _, ns := range namespacesIn(body) {
					if !enabledNamespaces[ns] {
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
