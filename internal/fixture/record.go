// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Recording goldens (FR-049, contracts/fixture-format.md §Recording).
//
// A golden is not a thing anybody writes by hand: it is what the graph answered, frozen, so
// that the next change that alters the answer has to say so in a pull request diff. Record
// produces them — replay the fixture into a database of its own, run every query the manifest
// lists, write each canonical response to `golden/<kind>.<name>.json`, and, when the manifest
// declares a clock end, run the whole set again with observed time pinned to it into
// `golden/pinned/`.
//
// Two rules keep recording honest:
//
//   - `events.jsonl` is never touched. Re-deriving the input from the output is how a harness
//     stops being able to fail; the events are the fixture's evidence and only a re-recording
//     from `payloads/` may rewrite them (fixtures/README.md).
//   - A query kind this build cannot answer is skipped, not written empty. An empty golden
//     would later pass a comparison against an implementation that has since been written, and
//     the regression would be invisible.
//
// The pinned directory is created whenever the manifest has a clock end, even if every pinned
// query was skipped. `fixture verify` requires it (T080): a fixture with no pinned pass fails
// its replay step rather than quietly verifying one pass fewer, so an empty directory is a
// meaningful statement — "this fixture records a pinned pass; every query it names was skipped
// by this build".

// RecordReport is what a recording run did.
type RecordReport struct {
	// FixtureID is the manifest's id.
	FixtureID string `json:"fixture_id"`
	// Events is how many events were replayed to build the graph the queries ran against.
	Events int `json:"events"`
	// Written lists the golden files written, relative to the fixture directory, in the order
	// the manifest lists the queries.
	Written []string `json:"written"`
	// Skipped names the queries whose kind this build cannot answer, as `<kind>.<name>`.
	Skipped []string `json:"skipped"`
	// Pinned reports whether the observed-time-pinned pass ran, i.e. the manifest has a
	// clock end.
	Pinned bool `json:"pinned"`
	// Removed lists golden files no query produces any more, deleted rather than left to look
	// like coverage (T153).
	Removed []string `json:"removed,omitempty"`
}

// Record replays the fixture in dir into a fresh database from newStore and rewrites its
// goldens.
//
// newRunner builds the query runner for the loaded store; a nil newRunner, or one that returns
// nil, is an error rather than a silent no-op, because a recording that records nothing looks
// exactly like a fixture whose queries all pass.
func Record(ctx context.Context, newStore StoreFactory, dir string, newRunner func(*postgres.Store) QueryRunner) (*RecordReport, error) {
	if newRunner == nil {
		return nil, errors.New("fixture: record needs a query runner; there would be nothing to record")
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	events, err := ReadEvents(m.EventsPath())
	if err != nil {
		return nil, err
	}
	// The manifest's human decisions are part of the event stream, not something applied around
	// it, so every pass below — replay, double-delivery, shuffle — sees them (FR-040, SC-007).
	events, err = WithHumanDecisions(m, events)
	if err != nil {
		return nil, err
	}
	// Refused before any database is opened: a window that ends before the fixture's own events
	// would record pinned goldens missing them, and nothing downstream could tell (T153).
	if outside := eventsOutsideClock(m, events); outside != "" {
		return nil, fmt.Errorf("fixture: record %s: %s", m.ID, outside)
	}

	store, cleanup, err := newStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("fixture: record %s: open store: %w", m.ID, err)
	}
	defer cleanup()

	loaded, err := load(ctx, projector.New(store), m, events, LoadOptions{})
	if err != nil {
		return nil, err
	}
	if loaded.Applied != len(events) {
		return nil, fmt.Errorf("fixture: record %s: %d of %d events applied (%d duplicate, %d rejected); goldens recorded from an incomplete graph would be wrong",
			m.ID, loaded.Applied, len(events), loaded.DuplicateNoop, loaded.Rejected)
	}

	runner := newRunner(store)
	if runner == nil {
		return nil, errors.New("fixture: record needs a query runner; there would be nothing to record")
	}

	report := &RecordReport{FixtureID: m.ID, Events: len(events), Pinned: !m.Clock.End.IsZero()}
	if err := recordPass(ctx, runner, m, m.Queries, report); err != nil {
		return nil, err
	}
	if !report.Pinned {
		return report, nil
	}

	// The pinned directory is the flag verify reads, so it is created before the pass rather
	// than as a side effect of the first file written.
	pinnedDir := filepath.Join(m.Dir, GoldenDir, PinnedDir)
	if err := os.MkdirAll(pinnedDir, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: create %s: %w", pinnedDir, err)
	}
	if err := recordPass(ctx, runner, m, PinnedQueries(m), report); err != nil {
		return nil, err
	}
	// Goldens no query produces any more are removed, not left behind. Recording rewrites the
	// whole answer set, so a file it did not write is one nothing will ever compare — and verify
	// refuses those, so leaving them would only move the failure to the next run (T153).
	orphans, err := orphanGoldens(m)
	if err != nil {
		return nil, err
	}
	for _, rel := range orphans {
		if err := os.Remove(filepath.Join(m.Dir, rel)); err != nil {
			return nil, fmt.Errorf("fixture: remove stale golden %s: %w", rel, err)
		}
		report.Removed = append(report.Removed, rel)
	}
	return report, nil
}

// recordPass runs one set of queries and writes their goldens.
func recordPass(ctx context.Context, runner QueryRunner, m *Manifest, queries []Query, report *RecordReport) error {
	for _, q := range queries {
		got, err := runner.Run(ctx, q)
		if errors.Is(err, ErrUnsupportedQueryKind) {
			report.Skipped = append(report.Skipped, skipName(q))
			continue
		}
		if err != nil {
			return fmt.Errorf("fixture: record %s: run query %s.%s: %w", m.ID, q.Kind, q.Name, err)
		}
		// T151: an answer that holds nothing is refused rather than written, unless the manifest says
		// the emptiness is the point. See internal/fixture/empty.go for what this costs and why.
		if err := checkGolden(m, q, got); err != nil {
			return err
		}
		path := GoldenPath(m.Dir, q)
		if err := WriteGolden(path, got); err != nil {
			return err
		}
		relative, err := filepath.Rel(m.Dir, path)
		if err != nil {
			relative = path
		}
		report.Written = append(report.Written, filepath.ToSlash(relative))
	}
	return nil
}

func skipName(q Query) string {
	name := q.Kind + "." + q.Name
	if q.Pinned {
		return PinnedDir + "/" + name
	}
	return name
}

// Markdown renders the report for a terminal or a pull request comment.
func (r *RecordReport) Markdown() string {
	var out strings.Builder
	fmt.Fprintf(&out, "## %s — recorded\n\n", r.FixtureID)
	fmt.Fprintf(&out, "- events replayed: %d\n", r.Events)
	fmt.Fprintf(&out, "- goldens written: %d\n", len(r.Written))
	for _, path := range r.Written {
		fmt.Fprintf(&out, "  - %s\n", path)
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&out, "- skipped (kind not supported by this build): %s\n",
			strings.Join(r.Skipped, ", "))
	}
	if !r.Pinned {
		out.WriteString("- no clock.end in the manifest, so no observed-time-pinned pass\n")
	}
	return out.String()
}
