// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// `query subgraph` end to end (FR-026, FR-035).
//
// The claim these tests hold to account is the one FR-035 makes twice over: the same read is
// available to a person and to a machine, and the machine's rendering is the canonical
// serialization goldens are written in. So the JSON the command prints is compared with the
// recorded golden byte for byte — if those two ever drift, a golden stops being reproducible
// from the command line and the fixture harness and the CLI have quietly forked.

// queryServer loads a fixture into a database of its own and serves it, returning the base URL
// and a reader token for it.
func queryServer(t *testing.T, fixtureDir string) (baseURL, token string) {
	t.Helper()

	store := pgtest.Open(t)
	p := projector.New(store)
	if _, err := fixture.Load(context.Background(), p, fixtureDir, fixture.LoadOptions{}); err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	srv, err := server.New(server.Config{
		Listen:    "127.0.0.1:0",
		Auth:      devAuthenticator(t),
		Projector: p,
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	http := httptest.NewServer(srv.Handler())
	t.Cleanup(http.Close)

	return http.URL, mintReaderToken(t)
}

// baselineFixture is the recorded fixture these tests read. Since the live run of 2026-09-16 it
// is a recording, so every instant in it moves whenever it is re-recorded; nothing below names
// a timestamp, and manifestQuery is how each test asks the fixture what to ask the server.
func baselineFixture() string {
	return filepath.Join("..", "..", "fixtures", "baseline-topology-01")
}

// manifestQuery returns the named query of a fixture's manifest, so that a test asks exactly
// what the golden beside it answers.
func manifestQuery(t *testing.T, dir, name string) fixture.Query {
	t.Helper()
	m, err := fixture.LoadManifest(dir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, q := range m.Queries {
		if q.Name == name {
			return q
		}
	}
	t.Fatalf("%s declares no query named %q", dir, name)
	return fixture.Query{}
}

// validAt renders a manifest query's valid time the way --as-of takes it.
func validAt(q fixture.Query) string { return q.ValidAt.UTC().Format(time.RFC3339) }

// TestQuerySubgraphJSONMatchesTheGolden: `--output json` is the golden serialization, not a
// second rendering of it.
func TestQuerySubgraphJSONMatchesTheGolden(t *testing.T) {
	dir := baselineFixture()
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "checkout-2hop")

	stdout, stderr, code := run(t, context.Background(),
		"--output", "json", "--server", baseURL, "--token", token,
		"query", "subgraph", q.Focus,
		"--as-of", validAt(q), "--hops", "2", "--direction", "both")
	if code != ExitOK {
		t.Fatalf("query subgraph: exit %d (stderr %q)", code, stderr)
	}

	want, err := os.ReadFile(filepath.Join(dir, "golden", "subgraph."+q.Name+".json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(string(want)) {
		t.Errorf("`query subgraph --output json` does not match the recorded golden.\n got: %s\nwant: %s",
			truncateForDiff(stdout), truncateForDiff(string(want)))
	}
}

// TestQuerySubgraphTableIsReadable: the human rendering names the focus, lists the nodes and
// edges, and states that nothing was truncated. A truncation that a person could miss is the
// failure mode this table exists to avoid (FR-026).
func TestQuerySubgraphTableIsReadable(t *testing.T) {
	dir := baselineFixture()
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "checkout-2hop")

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "subgraph", q.Focus, "--as-of", validAt(q))
	if code != ExitOK {
		t.Fatalf("query subgraph: exit %d (stderr %q)", code, stderr)
	}
	for _, want := range []string{
		"focus     checkout (service[workload])",
		"observed now",
		"payments",
		"storefront -> checkout",
		"complete: nothing was truncated",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table output does not mention %q:\n%s", want, stdout)
		}
	}
}

// TestQuerySubgraphReportsTruncation: the hub case, seen from the command line.
func TestQuerySubgraphReportsTruncation(t *testing.T) {
	dir := baselineFixture()
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "redis-3hop-cap5")

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "subgraph", q.Focus,
		"--as-of", validAt(q), "--hops", "3", "--per-hop-cap", "5")
	if code != ExitOK {
		t.Fatalf("query subgraph: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "TRUNCATED (per_hop_cap") {
		t.Errorf("a capped walk must say so:\n%s", stdout)
	}
}

// TestQuerySubgraphUsageErrors: a malformed invocation costs a round trip to nobody and exits 1.
func TestQuerySubgraphUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no as-of", []string{"query", "subgraph", "otel.service.name=checkout"}},
		{"bad focus", []string{"query", "subgraph", "checkout", "--as-of", "now"}},
		{"bad direction", []string{"query", "subgraph", "a=b", "--as-of", "now", "--direction", "sideways"}},
		{"bad edge type", []string{"query", "subgraph", "a=b", "--as-of", "now", "--edge-types", "teleports"}},
		{"bad instant", []string{"query", "subgraph", "a=b", "--as-of", "yesterday"}},
		{"weight out of range", []string{"query", "subgraph", "a=b", "--as-of", "now", "--min-weight", "9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := run(t, context.Background(), tc.args...)
			if code != ExitUsage {
				t.Errorf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
			}
		})
	}
}

// TestParseInstant covers the relative spellings an on-call engineer actually types.
func TestParseInstant(t *testing.T) {
	absolute, err := parseInstant("--as-of", "2026-09-01T14:32:00Z")
	if err != nil {
		t.Fatalf("absolute: %v", err)
	}
	if absolute.Format("15:04") != "14:32" {
		t.Errorf("absolute = %s, want 14:32 UTC", absolute)
	}

	before := time.Now().UTC()
	relative, err := parseInstant("--as-of", "-30m")
	if err != nil {
		t.Fatalf("relative: %v", err)
	}
	if drift := relative.Sub(before.Add(-30 * time.Minute)); drift < 0 || drift > time.Minute {
		t.Errorf("-30m resolved to %s, want half an hour before now", relative)
	}

	if empty, err := parseInstant("--observed-at", ""); err != nil || !empty.IsZero() {
		t.Errorf("empty = %v, %v; want the zero instant meaning unset", empty, err)
	}
	if _, err := parseInstant("--as-of", "half past"); err == nil {
		t.Error("an unparseable instant must be a usage error")
	}
}

// truncateForDiff keeps a failed byte comparison readable: canonical JSON is one very long
// line, and printing two of them in full helps nobody.
func truncateForDiff(s string) string {
	const limit = 400
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
