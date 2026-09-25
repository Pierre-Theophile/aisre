// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

func instantAt(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestAnUnpinnedQueryOverFutureObservationsIsWallClockDependent is the rule, on the shape that
// broke: events observed on the 20th and 25th, verified on the 19th, one query unpinned.
func TestAnUnpinnedQueryOverFutureObservationsIsWallClockDependent(t *testing.T) {
	t.Parallel()
	m := &Manifest{Queries: []Query{
		{Name: "pinned", ObservedAt: instantAt(t, "2026-10-03T00:00:00Z")},
		{Name: "as-known-now"},
	}}
	events := []Event{
		{ObservedAt: instantAt(t, "2026-09-20T09:00:00Z")},
		{ObservedAt: instantAt(t, "2026-09-25T10:00:00Z")},
	}

	got := wallClockDependentQueries(m, events, instantAt(t, "2026-09-19T12:00:00Z"))
	if len(got) != 1 || got[0] != "as-known-now" {
		t.Fatalf("dependent queries = %v, want [as-known-now]", got)
	}

	// Once every observation is in the past, "now" and clock.end read the same log.
	if got := wallClockDependentQueries(m, events, instantAt(t, "2026-10-04T00:00:00Z")); got != nil {
		t.Fatalf("after the last observation the rule must be silent, got %v", got)
	}

	detail := wallClockDependenceDetail([]string{"as-known-now"}, events, instantAt(t, "2026-09-19T12:00:00Z"))
	for _, want := range []string{"as-known-now", "2026-09-25T10:00:00Z", "clock.end"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not name %q", detail, want)
		}
	}
}

// TestVerifyRefusesAWallClockDependentFixtureBeforeOpeningAStore takes the shipped
// announced-fact-01, strips the pins from its queries, and verifies it on a day before its last
// observation: the replay step fails naming the queries, and no store is ever asked for.
func TestVerifyRefusesAWallClockDependentFixtureBeforeOpeningAStore(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("..", "..", "fixtures", "announced-fact-01")
	for _, name := range []string{"manifest.yaml", "events.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "manifest.yaml" {
			text := string(raw)
			const pin = "    observed_at: 2026-10-03T00:00:00Z\n"
			if !strings.Contains(text, pin) {
				t.Fatalf("the shipped manifest no longer carries the pin this test strips")
			}
			// Strip every pin: the guard must name each query it leaves unpinned.
			text = strings.ReplaceAll(text, pin, "")
			raw = []byte(text)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	previous := verifyNow
	verifyNow = func() time.Time { return instantAt(t, "2026-09-19T12:00:00Z") }
	t.Cleanup(func() { verifyNow = previous })

	storeAsked := errors.New("a store was opened before the wall-clock guard ran")
	newStore := func(context.Context) (*postgres.Store, func(), error) { return nil, nil, storeAsked }

	report, err := Verify(context.Background(), newStore, dir, VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify returned %v; the guard must report a failed step, not an error", err)
	}
	if report.Passed {
		t.Fatal("a fixture whose golden depends on the verification date passed")
	}
	if len(report.Steps) != 1 || report.Steps[0].Name != StepReplay || report.Steps[0].Passed {
		t.Fatalf("steps = %+v, want one failed replay step", report.Steps)
	}
	if !strings.Contains(report.Steps[0].Detail, "checkout-diff-forward") {
		t.Errorf("detail does not name the unpinned query: %s", report.Steps[0].Detail)
	}
}
