// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// verifyNow is the wall clock an unpinned manifest query is answered at. It is a variable so a
// test can move it; production never sets it.
var verifyNow = func() time.Time { return time.Now().UTC() }

// wallClockDependentQueries names every manifest query whose answer depends on the day the
// fixture is verified.
//
// A query with `observed_at` unset is answered "as known now" (fixture-format.md), which is a
// fine thing to ask of a graph and a meaningless thing to record a golden for when the fixture
// holds events the graph has not observed *yet*: on every day before the last observed instant
// the query sees a different prefix of the log, so the golden encodes the recording date and
// starts failing the morning the clock passes an observed instant. That is exactly what happened
// to announced-fact-01 on 2026-09-20 — its events are observed on the 20th and the 25th, its
// goldens were recorded on the 17th, and CI went red when the announcement became visible
// while its cancellation had not. Such a query must pin `observed_at` (usually to `clock.end`,
// which is what the mandatory pinned twin already does).
//
// The rule is deliberately narrow: a fixture whose every event is already observed is free to
// ask "now", because "now" and `clock.end` then see the same log.
func wallClockDependentQueries(m *Manifest, events []Event, now time.Time) []string {
	var latest time.Time
	for _, e := range events {
		if e.ObservedAt.After(latest) {
			latest = e.ObservedAt
		}
	}
	if !latest.After(now) {
		return nil
	}
	var names []string
	for _, q := range m.Queries {
		if q.ObservedAt.IsZero() {
			names = append(names, q.Name)
		}
	}
	sort.Strings(names)
	return names
}

// wallClockDependenceDetail is the failing step's text: it names the queries, the instant the
// fixture is still waiting on, and the fix.
func wallClockDependenceDetail(names []string, events []Event, now time.Time) string {
	var latest time.Time
	for _, e := range events {
		if e.ObservedAt.After(latest) {
			latest = e.ObservedAt
		}
	}
	return fmt.Sprintf("%d manifest quer%s answered \"as known now\" (observed_at unset) but the "+
		"fixture holds events observed as late as %s, after now (%s): the golden would encode the "+
		"verification date. Pin observed_at on %s (clock.end is the usual choice) and re-record.",
		len(names), plural(len(names), "y is", "ies are"),
		latest.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339),
		strings.Join(names, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// The observed-time-pinned pass, and the window it pins to (004 T153).
//
// # Which queries are pinned
//
// The pinned pass exists for queries that do NOT state `observed_at`. Such a query is answered "as
// known now", and a golden for it depends on the day it is compared whenever the fixture holds
// events the wall clock has not reached yet; pinning it to `clock.end` makes it deterministic.
//
// A query that states its own `observed_at` is already deterministic, and it is a deliberate
// bitemporal assertion — "as known at 20:00". The first cut of the pinned pass re-ran it with
// observed time overwritten by `clock.end`, which is a question nobody wrote: sometimes the same
// answer (a duplicate golden asserting nothing new), and sometimes a different and unreviewed one.
// T151's gate found the second kind as empty goldens — `gcp-rollback-01`'s pinned diff held nothing
// while the query its manifest actually states holds the two changes the fixture exists to prove.
// So only unpinned queries are pinned, and 118 duplicate-or-unwritten goldens stop being recorded.
//
// # The window must contain the fixture's own events
//
// Re-measuring T153 found the rest of its cause: NINE fixtures declared a `clock.end` earlier than
// events they hold — by 300 milliseconds in one, by a whole hour in `gcp-rollback-01`. A window that
// ends before its own events is a mis-declaration, not a matter of pinned-pass policy, and pinning to
// it silently drops those events from the answer. Nothing checked it. Now record and verify both
// refuse a fixture whose events fall outside `[clock.start, clock.end]`.

// PinnedQueries is the set the observed-time-pinned pass runs: every query that does not state its
// own `observed_at`, re-dated to `clock.end`. Record, verify and export all use it, so the three
// cannot drift apart — which is how the first cut ended up with the rule written out three times.
func PinnedQueries(m *Manifest) []Query {
	if m.Clock.End.IsZero() {
		return nil
	}
	var out []Query
	for _, q := range m.Queries {
		if !q.ObservedAt.IsZero() {
			continue
		}
		q.ObservedAt = m.Clock.End.UTC()
		q.Pinned = true
		out = append(out, q)
	}
	return out
}

// eventsOutsideClock reports the events observed outside the manifest's declared window, as a
// sentence naming how many, the extreme instant and the fix; empty when every event is inside or the
// manifest declares no clock (which verify refuses on its own terms).
func eventsOutsideClock(m *Manifest, events []Event) string {
	if m.Clock.End.IsZero() {
		return ""
	}
	start, end := m.Clock.Start.UTC(), m.Clock.End.UTC()
	var before, after int
	var earliest, latest time.Time
	for _, e := range events {
		at := e.ObservedAt.UTC()
		if !start.IsZero() && at.Before(start) {
			before++
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
		}
		if at.After(end) {
			after++
			if at.After(latest) {
				latest = at
			}
		}
	}
	var parts []string
	if after > 0 {
		parts = append(parts, fmt.Sprintf("%d event%s observed after clock.end (%s), the latest at %s",
			after, plural(after, "", "s"), end.Format(time.RFC3339Nano), latest.Format(time.RFC3339Nano)))
	}
	if before > 0 {
		parts = append(parts, fmt.Sprintf("%d event%s observed before clock.start (%s), the earliest at %s",
			before, plural(before, "", "s"), start.Format(time.RFC3339Nano), earliest.Format(time.RFC3339Nano)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + ". The manifest's clock declares the window this fixture " +
		"covers, and the pinned pass answers as known at clock.end, so an event outside it is dropped " +
		"from every pinned answer without anything saying so (004 T153). Widen the clock to contain " +
		"the events"
}

// orphanGoldens lists golden files no manifest query produces, relative to the fixture directory.
//
// A golden nothing compares is the worst kind of evidence: it sits in the tree looking like coverage
// and cannot fail. The pinned-pass change in T153 stops recording 118 goldens, and without this
// check every one of them would have stayed on disk indefinitely, unread.
func orphanGoldens(m *Manifest) ([]string, error) {
	want := map[string]bool{}
	for _, q := range m.Queries {
		want[GoldenPath(m.Dir, q)] = true
	}
	for _, q := range PinnedQueries(m) {
		want[GoldenPath(m.Dir, q)] = true
	}
	var orphans []string
	for _, dir := range []string{filepath.Join(m.Dir, GoldenDir), filepath.Join(m.Dir, GoldenDir, PinnedDir)} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fixture: read %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if !want[path] {
				rel, err := filepath.Rel(m.Dir, path)
				if err != nil {
					rel = path
				}
				orphans = append(orphans, rel)
			}
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}
