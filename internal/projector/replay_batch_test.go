// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The batch size of a replay is not part of what a replay produces (FR-023, constitution III).
//
// `Projector.Replay` used to commit once per event, which made a million events a million
// commits and the ADR-0002 replay criterion unreachable by an order of magnitude
// (docs/benchmarks/README.md, "Open finding: replay"). It now applies N events per transaction.
// The claim that makes that change safe is this one: the batch size decides *when* the work is
// committed and nothing else — the same events, in the same append order, with the same
// recorded observed times, therefore the same rows.
//
// So it is checked the way the fixture contract checks a replay: replay every shipped fixture
// at three batch sizes — 1 (the old transaction-per-event loop), 7 (a size that divides no
// fixture evenly, so batches straddle the interesting events) and 500 (the default, which puts
// every fixture in a single transaction) — and require the graphs to be identical, down to
// `produced_by_event_ids`, which is the field a coalescing or ordering mistake would move
// first.

// replayBatchSizes are the three sizes every fixture is replayed at.
var replayBatchSizes = []int{1, 7, projector.DefaultReplayBatchSize}

func TestReplayIsIndependentOfBatchSize(t *testing.T) {
	for _, id := range allShippedFixtures(t) {
		t.Run(id, func(t *testing.T) {
			dir := filepath.Join(fixturesDir, id)
			m := loadManifest(t, dir)
			events := loadEvents(t, filepath.Join(dir, m.Events))
			// A fixture that legitimately emitted NOTHING has nothing to compare: replaying no events
			// at three batch sizes produces three identical empty graphs, which is true and vacuous.
			//
			// This was a `t.Fatalf` when an empty event stream was impossible, and its real purpose was
			// to catch a fixture whose events file had failed to load. `loadEvents` already fails on a
			// missing or unreadable file, so that purpose survives: reaching here with zero events means
			// the file was read and holds none, which `vercel-preview-excluded-01` asserts on purpose —
			// three promoted deployments, every one excluded by its platform-stated target.
			if len(events) == 0 {
				t.Skipf("%s emitted no events, so batch size cannot change its outcome", id)
			}

			var (
				wantSnapshot   string
				wantProvenance string
			)
			for _, size := range replayBatchSizes {
				store := replayAtBatchSize(t, m, events, size)
				gotSnapshot := snapshot(t, store)
				gotProvenance := provenance(t, store)
				assertInvariants(t, store, fmt.Sprintf("after replay at batch size %d", size))

				if wantSnapshot == "" {
					wantSnapshot, wantProvenance = gotSnapshot, gotProvenance
					continue
				}
				if gotSnapshot != wantSnapshot {
					t.Errorf("batch size %d produced a different valid-time state than batch size %d:\n%s",
						size, replayBatchSizes[0], firstDifferingLine(wantSnapshot, gotSnapshot))
				}
				if gotProvenance != wantProvenance {
					t.Errorf("batch size %d produced different produced_by_event_ids than batch size %d:\n%s",
						size, replayBatchSizes[0], firstDifferingLine(wantProvenance, gotProvenance))
				}
			}
		})
	}
}

// replayAtBatchSize appends the fixture's events to a fresh log without projecting them — which
// is the state a replay starts from, and the only honest way to measure one — and then replays
// them at the given batch size.
func replayAtBatchSize(t *testing.T, m manifest, events []fixtureEvent, size int) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	p, store := newProjector(t, m)

	for _, event := range events {
		result, err := p.Log().Append(ctx, event.env, eventlog.AppendOptions{ObservedAt: event.observedAt})
		if err != nil {
			t.Fatalf("append %s: %v", event.env.GetEventId(), err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("append %s: status %s (%s: %s), want APPLIED", event.env.GetEventId(),
				result.GetStatus(), result.GetReasonCode(), result.GetReasonDetail())
		}
	}

	var progressed int64
	report, err := p.ReplayWithOptions(ctx, projector.ReplayOptions{
		BatchSize: size,
		Progress:  func(done, _ int64) { progressed = done },
	})
	if err != nil {
		t.Fatalf("replay at batch size %d: %v", size, err)
	}
	if report.Events != len(events) {
		t.Fatalf("replay at batch size %d projected %d events, want %d", size, report.Events, len(events))
	}
	if progressed != int64(len(events)) {
		t.Errorf("replay at batch size %d reported progress %d, want %d at the end",
			size, progressed, len(events))
	}
	return store
}

// provenance renders every current version's evidence chain, keyed by version id. Version ids
// are deterministic (research §4), so two replays of one log produce the same keys, and the
// value is the array a change in the segmentation would move.
func provenance(t *testing.T, store *postgres.Store) string {
	t.Helper()
	ctx := context.Background()

	var lines []string
	for _, q := range []struct{ label, sql string }{
		{"node", `SELECT version_id, produced_by_event_ids FROM graph.entity_versions ORDER BY version_id`},
		{"edge", `SELECT version_id, produced_by_event_ids FROM graph.edge_versions ORDER BY version_id`},
	} {
		rows, err := store.Pool().Query(ctx, q.sql)
		if err != nil {
			t.Fatalf("read %s provenance: %v", q.label, err)
		}
		for rows.Next() {
			var (
				versionID  string
				producedBy []string
			)
			if err := rows.Scan(&versionID, &producedBy); err != nil {
				rows.Close()
				t.Fatalf("scan %s provenance: %v", q.label, err)
			}
			lines = append(lines, fmt.Sprintf("%s %s %v", q.label, versionID, producedBy))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read %s provenance: %v", q.label, err)
		}
	}
	return strings.Join(lines, "\n")
}

// allShippedFixtures lists every fixture directory, so a fixture added later is replayed here
// without anyone remembering to add it.
func allShippedFixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixturesDir, err)
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(fixturesDir, entry.Name(), "manifest.yaml")); err != nil {
			continue
		}
		out = append(out, entry.Name())
	}
	slices.Sort(out)
	if len(out) == 0 {
		t.Fatalf("no fixtures found under %s", fixturesDir)
	}
	return out
}

// firstDifferingLine renders the first line two dumps disagree on, which points at the offending
// entity without printing two whole graphs.
func firstDifferingLine(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(wantLines), len(gotLines)) {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("  want: %s\n  got:  %s", w, g)
		}
	}
	return "  (the dumps differ but no line does; this is a bug in the test)"
}
