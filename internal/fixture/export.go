// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Exporting a live graph for comparison with a replayed one (T083, SC-010, FR-050 (b)).
//
// `fixture record` answers "what does the graph say?" about a database it builds itself, from
// the fixture's own events. Export answers the same question about a database somebody else
// built — the live one a feeder has been writing to for the last twenty minutes — and writes
// the answers under the same file names, in the same canonical serialization.
//
// That makes the SC-010 claim a file comparison and nothing cleverer:
//
//	aisre fixture export --db <live> --manifest fixtures/x/manifest.yaml --out /tmp/live
//	diff -r /tmp/live fixtures/x/golden
//
// Silence is the proof that the graph built live and the graph rebuilt from the recording are
// the same graph, entity ids, valid intervals, observed times and extent block included. Not
// "the same up to timestamps": the same bytes.
//
// Three properties make that possible and are worth stating, because each is a decision made
// elsewhere that this file depends on:
//
//   - the recorder writes the observed time the *graph* assigned at acceptance, not a clock of
//     its own, and replay applies each event with that value (FR-023, pkg/feeder/record);
//   - canonical serialization is pinned (graph.CanonicalJSON), so two encoders of the same
//     message agree byte for byte;
//   - a canonical entity id is a pure function of the identity claim seen first, and a replay
//     sees the same claims in the same order as the live run did.
//
// Export never writes to the database it is given and never creates one. It is the only part of
// the fixture package that is safe to point at a production deployment, and it stays that way:
// read the manifest, run its queries, write files.

// ExportOptions configures an export run.
type ExportOptions struct {
	// Out is the directory the canonical answers are written to. It is created if needed and
	// laid out exactly like a fixture's golden/: `<kind>.<name>.json`, plus `pinned/` for the
	// observed-time-pinned pass, so `diff -r <out> <fixture>/golden` is the whole check.
	Out string
	// Compare is a fixture directory whose golden/ the export is diffed against. Empty skips
	// the comparison and only writes the files.
	Compare string
}

// ExportReport is what an export run produced.
type ExportReport struct {
	// FixtureID is the manifest's id.
	FixtureID string `json:"fixture_id"`
	// Out is the directory that was written.
	Out string `json:"out"`
	// Written lists the files written, relative to Out, in manifest order.
	Written []string `json:"written"`
	// Skipped names the queries whose kind this build cannot answer, as `<kind>.<name>`.
	Skipped []string `json:"skipped"`
	// Pinned reports whether the observed-time-pinned pass ran, i.e. the manifest has a clock
	// end.
	Pinned bool `json:"pinned"`
	// Comparison is filled when ExportOptions.Compare named a fixture.
	Comparison *ExportComparison `json:"comparison,omitempty"`
}

// ExportComparison is the SC-010 verdict: the exported answers against the recorded goldens.
type ExportComparison struct {
	// Fixture is the directory whose golden/ was compared.
	Fixture string `json:"fixture"`
	// Identical is how many files matched byte for byte.
	Identical int `json:"identical"`
	// Differing names the files whose bytes differ, relative to golden/.
	Differing []string `json:"differing"`
	// MissingFromExport names goldens the export did not produce — a query the recording
	// answers and the live graph does not.
	MissingFromExport []string `json:"missing_from_export"`
	// MissingFromGoldens names exported files the fixture has no golden for — a query the
	// live graph answers and the recording does not.
	MissingFromGoldens []string `json:"missing_from_goldens"`
	// Equal is true only when every file on both sides matched.
	Equal bool `json:"equal"`
}

// Export runs every query of the manifest at path against store and writes the canonical
// answers under opts.Out.
//
// store is the caller's, already open, and is neither created nor dropped nor written to: this
// is a read of a graph somebody else built. path may name the manifest file or the directory
// holding it.
//
// newRunner builds the query runner; a nil newRunner, or one returning nil, is an error rather
// than an empty export, because an export that exported nothing compares equal to nothing and
// would read as a pass.
func Export(
	ctx context.Context,
	store *postgres.Store,
	path string,
	newRunner func(*postgres.Store) QueryRunner,
	opts ExportOptions,
) (*ExportReport, error) {
	if newRunner == nil {
		return nil, errors.New("fixture: export needs a query runner; there would be nothing to export")
	}
	if opts.Out == "" {
		return nil, errors.New("fixture: export needs an output directory")
	}
	m, err := LoadManifest(ManifestDir(path))
	if err != nil {
		return nil, err
	}
	runner := newRunner(store)
	if runner == nil {
		return nil, errors.New("fixture: export needs a query runner; there would be nothing to export")
	}

	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: create %s: %w", opts.Out, err)
	}
	report := &ExportReport{FixtureID: m.ID, Out: opts.Out, Pinned: !m.Clock.End.IsZero()}
	if err := exportPass(ctx, runner, m, m.Queries, opts.Out, report); err != nil {
		return nil, err
	}
	if report.Pinned {
		// The pinned directory is created before the pass, not as a side effect of the first
		// file written, so that an export whose pinned queries were all skipped still says
		// "this export ran a pinned pass" — the same statement `fixture record` makes.
		pinnedDir := filepath.Join(opts.Out, PinnedDir)
		if err := os.MkdirAll(pinnedDir, 0o755); err != nil {
			return nil, fmt.Errorf("fixture: create %s: %w", pinnedDir, err)
		}
		if err := exportPass(ctx, runner, m, PinnedQueries(m), opts.Out, report); err != nil {
			return nil, err
		}
	}

	if opts.Compare == "" {
		return report, nil
	}
	comparison, err := CompareExport(opts.Out, opts.Compare)
	if err != nil {
		return nil, err
	}
	report.Comparison = comparison
	return report, nil
}

// ManifestDir turns either spelling of `--manifest` — the file or the directory holding it —
// into the directory LoadManifest wants. A caller who typed the path of the file they were
// looking at should not have to think about which one the loader takes.
func ManifestDir(path string) string {
	if filepath.Base(path) == ManifestFile {
		return filepath.Dir(path)
	}
	return path
}

// exportPass runs one set of queries and writes their answers under out.
func exportPass(ctx context.Context, runner QueryRunner, m *Manifest, queries []Query, out string, report *ExportReport) error {
	for _, q := range queries {
		got, err := runner.Run(ctx, q)
		if errors.Is(err, ErrUnsupportedQueryKind) {
			report.Skipped = append(report.Skipped, skipName(q))
			continue
		}
		if err != nil {
			return fmt.Errorf("fixture: export %s: run query %s.%s: %w", m.ID, q.Kind, q.Name, err)
		}
		// The export is laid out exactly like a fixture's golden/ — `<kind>.<name>.json`, and
		// `pinned/<kind>.<name>.json` for the pinned pass — which is what makes `diff -r` the
		// comparison. It is rooted at the export directory, never at a fixture, so nothing here
		// can overwrite a recorded golden. The spelling is GoldenPath's, one level up.
		path := filepath.Join(out, exportName(q))
		if err := WriteGolden(path, got); err != nil {
			return err
		}
		relative, err := filepath.Rel(out, path)
		if err != nil {
			relative = path
		}
		report.Written = append(report.Written, filepath.ToSlash(relative))
	}
	return nil
}

// exportName is a query's file name inside an export, relative to its root. It mirrors
// GoldenPath, which cannot be reused directly because that one is rooted at a fixture.
func exportName(q Query) string {
	name := q.Kind + "." + q.Name + ".json"
	if q.Pinned {
		return filepath.Join(PinnedDir, name)
	}
	return name
}

// CompareExport diffs an export directory against a fixture's golden/ and returns the verdict.
//
// The comparison is over the union of both sides: a golden the export did not produce and an
// exported file the fixture has no golden for are both reported, because either one means the
// live graph and the replayed one answer different sets of questions — which is a parity failure
// as surely as differing bytes are.
func CompareExport(out, fixtureDir string) (*ExportComparison, error) {
	golden := filepath.Join(fixtureDir, GoldenDir)
	exported, err := jsonFiles(out)
	if err != nil {
		return nil, err
	}
	recorded, err := jsonFiles(golden)
	if err != nil {
		return nil, err
	}

	comparison := &ExportComparison{Fixture: fixtureDir}
	for _, name := range union(exported, recorded) {
		_, inExport := exported[name]
		_, inGolden := recorded[name]
		switch {
		case !inExport:
			comparison.MissingFromExport = append(comparison.MissingFromExport, name)
		case !inGolden:
			comparison.MissingFromGoldens = append(comparison.MissingFromGoldens, name)
		default:
			same, err := sameFileBytes(filepath.Join(out, name), filepath.Join(golden, name))
			if err != nil {
				return nil, err
			}
			if same {
				comparison.Identical++
			} else {
				comparison.Differing = append(comparison.Differing, name)
			}
		}
	}
	comparison.Equal = len(comparison.Differing) == 0 &&
		len(comparison.MissingFromExport) == 0 && len(comparison.MissingFromGoldens) == 0
	return comparison, nil
}

// jsonFiles lists the .json files under dir and its pinned/ subdirectory, keyed by the path
// they are named by on both sides ("subgraph.x.json", "pinned/subgraph.x.json").
func jsonFiles(dir string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, sub := range []string{"", PinnedDir} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fixture: read %s: %w", filepath.Join(dir, sub), err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			out[filepath.ToSlash(filepath.Join(sub, entry.Name()))] = struct{}{}
		}
	}
	return out, nil
}

func union(a, b map[string]struct{}) []string {
	seen := map[string]bool{}
	for name := range a {
		seen[name] = true
	}
	for name := range b {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sameFileBytes compares two files, ignoring one trailing newline on either side — the same
// insignificant byte CompareGolden normalizes, and for the same reason.
func sameFileBytes(a, b string) (bool, error) {
	left, err := os.ReadFile(a)
	if err != nil {
		return false, fmt.Errorf("fixture: read %s: %w", a, err)
	}
	right, err := os.ReadFile(b)
	if err != nil {
		return false, fmt.Errorf("fixture: read %s: %w", b, err)
	}
	return strings.TrimRight(string(left), "\n") == strings.TrimRight(string(right), "\n"), nil
}

// Markdown renders the report for a terminal or a job summary.
func (r *ExportReport) Markdown() string {
	var out strings.Builder
	fmt.Fprintf(&out, "## %s — exported\n\n", r.FixtureID)
	fmt.Fprintf(&out, "- out: %s\n", r.Out)
	fmt.Fprintf(&out, "- files written: %d\n", len(r.Written))
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&out, "- skipped (kind not supported by this build): %s\n",
			strings.Join(r.Skipped, ", "))
	}
	if !r.Pinned {
		out.WriteString("- no clock.end in the manifest, so no observed-time-pinned pass\n")
	}
	if r.Comparison != nil {
		out.WriteString("\n" + r.Comparison.Verdict() + "\n")
	}
	return out.String()
}

// Verdict is the one line SC-010 is reported as.
func (c *ExportComparison) Verdict() string {
	if c.Equal {
		return fmt.Sprintf("SC-010 live vs replayed: IDENTICAL — %d files byte for byte against %s",
			c.Identical, filepath.Join(c.Fixture, GoldenDir))
	}
	var problems []string
	if len(c.Differing) > 0 {
		problems = append(problems, fmt.Sprintf("%d differ (%s)", len(c.Differing), strings.Join(c.Differing, ", ")))
	}
	if len(c.MissingFromExport) > 0 {
		problems = append(problems, fmt.Sprintf("%d only in goldens (%s)",
			len(c.MissingFromExport), strings.Join(c.MissingFromExport, ", ")))
	}
	if len(c.MissingFromGoldens) > 0 {
		problems = append(problems, fmt.Sprintf("%d only in the export (%s)",
			len(c.MissingFromGoldens), strings.Join(c.MissingFromGoldens, ", ")))
	}
	return fmt.Sprintf("SC-010 live vs replayed: DIFFERS — %d identical, %s",
		c.Identical, strings.Join(problems, "; "))
}
