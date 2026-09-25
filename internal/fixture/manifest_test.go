// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// fixturesDir is the shipped fixture corpus, relative to this package.
const fixturesDir = "../../fixtures"

// shipped is every fixture this feature ships (fixtures/README.md). Adding one here is how a
// new fixture joins the harness's own tests.
var shipped = []string{"baseline-topology-01", "late-arriving-fact-01", "retraction-with-edges-01"}

func TestLoadManifestShippedFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id string
		// handAuthored is what the manifest must declare. Since the live run of 2026-09-16
		// baseline-topology-01 is a recording, and the flag is the difference between a
		// fixture whose events.jsonl may be regenerated from payloads/ and one where it is
		// the artifact itself (fixtures/README.md).
		handAuthored bool
		family       string
		sources      int
		queries      int
		rejected     string
		expectCount  int
		windows      map[string]time.Duration
	}{
		{
			id: "baseline-topology-01", family: "baseline-topology", handAuthored: false,
			sources: 2, queries: 7, rejected: "rejected.jsonl", expectCount: 1,
			windows: map[string]time.Duration{"k8s:kind": 60 * time.Second, "otel:kind": 30 * time.Second},
		},
		{
			id: "late-arriving-fact-01", family: "late-arriving-fact", handAuthored: true,
			sources: 1, queries: 2, rejected: "", expectCount: 0,
			windows: map[string]time.Duration{"otel:demo": 30 * time.Second},
		},
		{
			id: "retraction-with-edges-01", family: "retraction-with-edges", handAuthored: true,
			sources: 1, queries: 3, rejected: "", expectCount: 0,
			windows: map[string]time.Duration{"otel:demo": 30 * time.Second},
		},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			m, err := fixture.LoadManifest(filepath.Join(fixturesDir, tc.id))
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}

			if m.ID != tc.id {
				t.Errorf("id = %q, want %q", m.ID, tc.id)
			}
			if m.Family != tc.family {
				t.Errorf("family = %q, want %q", m.Family, tc.family)
			}
			if m.HandAuthored != tc.handAuthored {
				t.Errorf("hand_authored = %v, want %v", m.HandAuthored, tc.handAuthored)
			}
			if m.Events != "events.jsonl" {
				t.Errorf("events = %q, want events.jsonl", m.Events)
			}
			if m.RejectedEvents != tc.rejected {
				t.Errorf("rejected_events = %q, want %q", m.RejectedEvents, tc.rejected)
			}
			if m.SchemaVersion != "1.0.0" {
				t.Errorf("schema_version = %q, want 1.0.0", m.SchemaVersion)
			}
			if len(m.Sources) != tc.sources {
				t.Errorf("sources = %d, want %d", len(m.Sources), tc.sources)
			}
			if len(m.Queries) != tc.queries {
				t.Errorf("queries = %d, want %d", len(m.Queries), tc.queries)
			}
			if len(m.ExpectRejected) != tc.expectCount {
				t.Errorf("expect_rejected = %d, want %d", len(m.ExpectRejected), tc.expectCount)
			}

			// The reordering window drives the shuffle check, so a manifest that parsed
			// "30s" as zero would silently weaken verification. The values are what the two
			// feeders declare in Describe — otel.DeclaredReorderingWindow (30 s, bounded by
			// the aggregation window) and k8s.DefaultReorderingWindow (60 s, a statement about
			// the emitter's flush interval). See the comments on those constants.
			got := m.ReorderingWindows()
			for sourceID, want := range tc.windows {
				if got[sourceID] != want {
					t.Errorf("reordering window of %s = %s, want %s", sourceID, got[sourceID], want)
				}
			}
			if m.Clock.Start.IsZero() || m.Clock.End.IsZero() {
				t.Errorf("clock = [%s, %s], want both bounds set", m.Clock.Start, m.Clock.End)
			}
			if !m.Clock.Start.Before(m.Clock.End) {
				t.Errorf("clock start %s is not before end %s", m.Clock.Start, m.Clock.End)
			}

			// Paths are what everything downstream reads.
			if _, err := os.Stat(m.EventsPath()); err != nil {
				t.Errorf("EventsPath: %v", err)
			}
			if tc.rejected == "" {
				if m.RejectedPath() != "" {
					t.Errorf("RejectedPath = %q, want empty", m.RejectedPath())
				}
			} else if _, err := os.Stat(m.RejectedPath()); err != nil {
				t.Errorf("RejectedPath: %v", err)
			}
		})
	}
}

// TestLoadManifestQueryFields pins the query keys the fixtures use today, including the two the
// format contract's example does not show (direction, per_hop_cap) and the observed-time pin.
func TestLoadManifestQueryFields(t *testing.T) {
	t.Parallel()

	baseline, err := fixture.LoadManifest(filepath.Join(fixturesDir, "baseline-topology-01"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	byName := map[string]fixture.Query{}
	for _, q := range baseline.Queries {
		byName[q.Name] = q
	}

	redis := byName["redis-3hop-cap5"]
	if redis.Kind != "subgraph" || redis.Hops != 3 || redis.PerHopCap != 5 || redis.Direction != "both" {
		t.Errorf("redis-3hop-cap5 = %+v, want a 3-hop both-directions subgraph capped at 5", redis)
	}
	if ref, err := redis.FocusRef(); err != nil {
		t.Errorf("FocusRef: %v", err)
	} else if ref.Namespace != "otel.service.name" || ref.Value != "redis" {
		t.Errorf("focus = %s, want otel.service.name=redis", ref)
	}
	if extent := byName["extent"]; extent.Kind != "extent" || extent.Focus != "" {
		t.Errorf("extent query = %+v, want a focusless extent query", extent)
	}
	if !byName["checkout-2hop-1432"].ObservedAt.IsZero() {
		t.Error("checkout-2hop-1432 pins observed time; the manifest leaves it unset (as known now)")
	}

	late, err := fixture.LoadManifest(filepath.Join(fixturesDir, "late-arriving-fact-01"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	var pinned fixture.Query
	for _, q := range late.Queries {
		if q.Name == "checkout-1432-pinned" {
			pinned = q
		}
	}
	want := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	if !pinned.ObservedAt.Equal(want) {
		t.Errorf("checkout-1432-pinned observed_at = %s, want %s", pinned.ObservedAt, want)
	}
	// The pinned flag is the verifier's, not the manifest's: it selects golden/pinned/.
	if pinned.Pinned {
		t.Error("Pinned is set from YAML; it must only be set by the verifier's second pass")
	}
}

// TestLoadManifestUnknownKeys checks the two halves of the loader's strictness: an unknown
// manifest key is a broken fixture, an unknown query key is forward compatibility.
func TestLoadManifestUnknownKeys(t *testing.T) {
	t.Parallel()

	t.Run("manifest key is refused", func(t *testing.T) {
		t.Parallel()
		dir := writeManifest(t, `
id: bogus-01
family: baseline-topology
schema_version: 1.0.0
sources:
  - source_id: otel:demo
    kind: otel
    ordering: none
    reordering_window: 300s
totally_unexpected: true
`)
		if _, err := fixture.LoadManifest(dir); err == nil {
			t.Fatal("LoadManifest accepted an unknown manifest key; a fixture the loader cannot fully read must not verify")
		}
	})

	t.Run("query key is kept", func(t *testing.T) {
		t.Parallel()
		dir := writeManifest(t, `
id: forward-01
family: baseline-topology
schema_version: 1.0.0
sources:
  - source_id: otel:demo
    kind: otel
    ordering: none
    reordering_window: 300s
queries:
  - name: future
    kind: subgraph
    focus: otel.service.name=checkout
    tau: 30m
`)
		m, err := fixture.LoadManifest(dir)
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		if got := m.Queries[0].Extra["tau"]; got != "30m" {
			t.Errorf("Extra[tau] = %v, want 30m: a query key from a later phase must survive the load", got)
		}
		if m.Queries[0].Kind != "subgraph" {
			t.Errorf("kind = %q, want subgraph", m.Queries[0].Kind)
		}
	})
}

func TestLoadManifestValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
	}{
		{"no id", "family: x\nsources:\n  - source_id: a\n    kind: otel\n"},
		{"no sources", "id: x\nfamily: x\n"},
		{"duplicate source", "id: x\nsources:\n  - source_id: a\n    kind: otel\n  - source_id: a\n    kind: otel\n"},
		{"query without kind", "id: x\nsources:\n  - source_id: a\n    kind: otel\nqueries:\n  - name: q\n"},
		{
			"colliding golden names",
			"id: x\nsources:\n  - source_id: a\n    kind: otel\nqueries:\n  - name: q\n    kind: subgraph\n  - name: q\n    kind: subgraph\n",
		},
		{"expect_rejected without reason", "id: x\nsources:\n  - source_id: a\n    kind: otel\nexpect_rejected:\n  - event_id: e\n"},
		{"bad reordering window", "id: x\nsources:\n  - source_id: a\n    kind: otel\n    reordering_window: sixty\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := fixture.LoadManifest(writeManifest(t, tc.yaml)); err == nil {
				t.Fatal("LoadManifest accepted a manifest it cannot verify against")
			}
		})
	}
}

// writeManifest puts a manifest in a scratch directory and returns it.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fixture.ManifestFile), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}
