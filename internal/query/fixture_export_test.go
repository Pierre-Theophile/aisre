// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// SC-010 as a test that needs no cluster (T083).
//
// The claim `fixture export` exists to check is that the graph built *live* and the graph built
// from a recording of the same run answer identically, byte for byte. The nightly job checks it
// the only way it can be checked for real — against a live database a feeder has been writing to
// — and that run cannot happen here.
//
// What can happen here is the other half of the claim: that an export of a graph is byte-
// identical to the goldens recorded from the same graph. `fixture record` builds a database of
// its own and writes `golden/`; Export reads a database somebody else built and writes files
// under the same names. Loading a fixture into a store and exporting it therefore has to
// reproduce the fixture's own goldens exactly, for every fixture the project ships, or the
// nightly comparison would be comparing two different serializers and its silence would mean
// nothing.
//
// The queries this build cannot answer are skipped on both sides — they have no golden either —
// so the comparison is over the files that exist, and the test additionally refuses an export
// that produced none.
func TestExportReproducesTheShippedGoldens(t *testing.T) {
	ctx := context.Background()
	for _, name := range allShippedFixtures(t) {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(fixturesDir, name)
			store := pgtest.Open(t)
			if _, err := fixture.Load(ctx, projector.New(store), dir, fixture.LoadOptions{}); err != nil {
				t.Fatalf("Load: %v", err)
			}

			out := t.TempDir()
			report, err := fixture.Export(ctx, store, filepath.Join(dir, fixture.ManifestFile),
				newRunner, fixture.ExportOptions{Out: out, Compare: dir})
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if len(report.Written) == 0 {
				t.Fatal("the export wrote no files; an empty export compares equal to nothing")
			}
			if !report.Pinned {
				t.Error("the export ran no observed-time-pinned pass; every fixture states a clock end")
			}
			if report.Comparison == nil {
				t.Fatal("no comparison was made")
			}
			if !report.Comparison.Equal {
				t.Errorf("export differs from the recorded goldens: %s", report.Comparison.Verdict())
			}
			t.Logf("%s: %d files, %s", name, len(report.Written), report.Comparison.Verdict())
		})
	}
}

// TestExportComparisonNoticesADifference is the other direction: the comparison has to be able
// to fail, or the test above would prove nothing. One exported byte is changed and the verdict
// must name that file.
func TestExportComparisonNoticesADifference(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(fixturesDir, "late-arriving-fact-01")
	store := pgtest.Open(t)
	if _, err := fixture.Load(ctx, projector.New(store), dir, fixture.LoadOptions{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := t.TempDir()
	report, err := fixture.Export(ctx, store, dir, newRunner, fixture.ExportOptions{Out: out})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	victim := filepath.Join(out, report.Written[0])
	raw, err := os.ReadFile(victim) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("read %s: %v", victim, err)
	}
	if err := os.WriteFile(victim, append([]byte("{\"tampered\":true,"), raw[1:]...), 0o600); err != nil {
		t.Fatalf("write %s: %v", victim, err)
	}

	comparison, err := fixture.CompareExport(out, dir)
	if err != nil {
		t.Fatalf("CompareExport: %v", err)
	}
	if comparison.Equal {
		t.Fatal("a tampered export compared equal to the goldens")
	}
	if !strings.Contains(comparison.Verdict(), report.Written[0]) {
		t.Errorf("the verdict does not name the file that differs: %s", comparison.Verdict())
	}
}

// allShippedFixtures is every fixture directory under fixtures/, so a fixture added without
// being added to a list is still covered.
//
// A directory with no manifest.yaml is not a fixture: `fixtures/incidents/` is a group holding
// feature 002's incident fixtures, not a fixture itself. It is skipped here the same way
// internal/projector's copy of this helper skips it.
func allShippedFixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixturesDir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(fixturesDir, entry.Name(), "manifest.yaml")); err != nil {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		t.Fatal("no fixtures found")
	}
	return names
}
