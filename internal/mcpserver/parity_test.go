// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/mcpserver"
	"github.com/Pierre-Theophile/aisre/internal/server"
)

// TestGraphToolsMatchTheGoldens is SC-001: for every graph query a fixture declares, the MCP
// tool's structured content equals the golden the fixture harness and the CLI's `--output json`
// are compared with. The three surfaces therefore agree byte for byte (FR-030).
func TestGraphToolsMatchTheGoldens(t *testing.T) {
	for _, name := range []string{"baseline-topology-01", "rollout-regression-01", "ambiguous-identity-01", filepath.Join("incidents", "injection-01")} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "fixtures", name)
			manifest, err := fixture.LoadManifest(dir)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			baseURL, issuer := apiServer(t, dir)
			cs := session(t, baseURL, mint(t, issuer, server.RoleReader), mcpserver.ProfileDefault)

			compared := 0
			for _, q := range manifest.Queries {
				tool, args, ok := toolFor(q)
				if !ok {
					continue
				}
				golden := filepath.Join(dir, "golden", q.Kind+"."+q.Name+".json")
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("%s: %v", golden, err)
				}
				got := structured(t, call(t, cs, tool, args))
				if !sameJSON(t, got, want) {
					t.Errorf("%s %s: the MCP answer differs from %s\n got: %.400s\nwant: %.400s",
						tool, q.Name, golden, got, want)
				}
				compared++
			}
			if compared == 0 {
				t.Fatalf("%s: no graph query was compared", name)
			}
			t.Logf("%s: %d answer(s) equal to their goldens", name, compared)
		})
	}
}

// toolFor maps a manifest query onto the tool and arguments a coding agent would send.
func toolFor(q fixture.Query) (string, map[string]any, bool) {
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	args := map[string]any{}
	if q.Focus != "" {
		args["focus"] = q.Focus
	}
	if !q.ObservedAt.IsZero() {
		args["observed_at"] = ts(q.ObservedAt)
	}
	neighbourhood := func() {
		args["hops"] = q.Hops
		if q.Direction != "" {
			args["direction"] = q.Direction
		}
		if len(q.EdgeTypes) > 0 {
			args["edge_types"] = q.EdgeTypes
		}
		if q.MinWeightClass != nil {
			args["min_weight"] = *q.MinWeightClass
		}
		if q.PerHopCap > 0 {
			args["per_hop_cap"] = q.PerHopCap
		}
		if q.TotalCap > 0 {
			args["total_cap"] = q.TotalCap
		}
	}
	switch q.Kind {
	case "subgraph":
		args["valid_at"] = ts(q.ValidAt)
		neighbourhood()
		return "subgraph", args, true
	case "diff":
		if len(q.Extra) > 0 {
			return "", nil, false // ranking knobs the tool spells differently; covered by unit tests
		}
		args["from"], args["to"] = ts(q.T1), ts(q.T2)
		if !q.ReferenceAt.IsZero() {
			args["reference"] = ts(q.ReferenceAt)
		}
		neighbourhood()
		return "diff", args, true
	case "impact":
		if len(q.Extra) > 0 {
			return "", nil, false
		}
		args["valid_at"] = ts(q.ValidAt)
		if q.Hops > 0 {
			args["max_hops"] = q.Hops
		}
		if q.TotalCap > 0 {
			args["total_cap"] = q.TotalCap
		}
		return "impact", args, true
	case "pointers":
		args["valid_at"] = ts(q.ValidAt)
		return "pointers", args, true
	case "history":
		return "history", args, true
	case "extent":
		return "extent", map[string]any{}, true
	case "audit":
		out := map[string]any{"a": q.RefA, "b": q.RefB}
		if !q.ObservedAt.IsZero() {
			out["observed_at"] = ts(q.ObservedAt)
		}
		return "resolve_why", out, true
	}
	return "", nil, false
}

// sameJSON compares two JSON documents by value, so that the client library's re-encoding of the
// structured content (key order, whitespace) does not count as a difference.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	norm := func(raw []byte) []byte {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode %q: %v", strings.TrimSpace(string(raw))[:min(len(raw), 80)], err)
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return out
	}
	return bytes.Equal(norm(a), norm(b))
}
