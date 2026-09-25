// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// TestReadEventsShippedFixtures parses every line of every shipped fixture strictly. It is the
// cheapest guard there is against the event stream drifting away from the published schema
// (constitution IX): a field the proto does not have fails the parse rather than being dropped.
func TestReadEventsShippedFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id     string
		events int
	}{
		{"baseline-topology-01", 89},
		{"late-arriving-fact-01", 15},
		{"retraction-with-edges-01", 19},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			m, err := fixture.LoadManifest(filepath.Join(fixturesDir, tc.id))
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			events, err := fixture.ReadEvents(m.EventsPath())
			if err != nil {
				t.Fatalf("ReadEvents: %v", err)
			}
			if len(events) != tc.events {
				t.Fatalf("read %d events, want %d", len(events), tc.events)
			}

			sources := map[string]bool{}
			for _, src := range m.Sources {
				sources[src.SourceID] = true
			}
			for i, event := range events {
				if event.Envelope.GetEventId() == "" {
					t.Errorf("event %d has no event_id", i+1)
				}
				if event.Envelope.GetBody() == nil {
					t.Errorf("%s has no body; an untyped event cannot be applied", event.Envelope.GetEventId())
				}
				if event.ObservedAt.IsZero() {
					t.Errorf("%s has no observedAt", event.Envelope.GetEventId())
				}
				if event.AppendedSeq != int64(i+1) {
					t.Errorf("%s has appendedSeq %d, want %d", event.Envelope.GetEventId(), event.AppendedSeq, i+1)
				}
				if !sources[event.Envelope.GetSourceId()] {
					t.Errorf("%s comes from %q, which the manifest does not declare",
						event.Envelope.GetEventId(), event.Envelope.GetSourceId())
				}
				if i > 0 && event.ObservedAt.Before(events[i-1].ObservedAt) {
					t.Errorf("%s is observed before the line above it; events.jsonl is ordered by observed time",
						event.Envelope.GetEventId())
				}
			}
		})
	}
}

func TestReadRejected(t *testing.T) {
	t.Parallel()

	m, err := fixture.LoadManifest(filepath.Join(fixturesDir, "baseline-topology-01"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	rejected, err := fixture.ReadRejected(m.RejectedPath())
	if err != nil {
		t.Fatalf("ReadRejected: %v", err)
	}
	if len(rejected) != 1 {
		t.Fatalf("read %d rejected events, want 1", len(rejected))
	}
	event := rejected[0]
	if got, want := event.Envelope.GetEventId(), "otel:kind:bad-span-props-1"; got != want {
		t.Errorf("event_id = %q, want %q", got, want)
	}
	// The log never accepted it, so it never assigned an observed time; the source's own
	// timestamp is all there is (fixtures/README.md §rejected.jsonl).
	if !event.ObservedAt.IsZero() {
		t.Errorf("observedAt = %s, want unset", event.ObservedAt)
	}
	if event.Envelope.GetSourceObservedAt() == nil {
		t.Error("sourceObservedAt is unset; it is what the submitter stamps instead")
	}
}

// observedAtPattern matches the log-added observed time of a canonical events.jsonl line.
// Matching it rather than naming a literal keeps this test working across a re-recording,
// which moves every timestamp in the fixture (live run 2026-09-16).
var observedAtPattern = regexp.MustCompile(`"observedAt":"[^"]+",`)

func TestReadEventsRejectsBrokenLines(t *testing.T) {
	t.Parallel()

	valid := firstLine(t, filepath.Join(fixturesDir, "baseline-topology-01", "events.jsonl"))
	if !observedAtPattern.MatchString(valid) {
		t.Fatalf("the first line of the baseline fixture carries no observedAt to remove: %s", valid)
	}

	tests := []struct {
		name string
		line string
		want string
	}{
		{"not json", "{", "JSON"},
		{
			"unknown schema field",
			strings.Replace(valid, `{"appendedSeq":1,`, `{"appendedSeq":1,"notAField":1,`, 1),
			"published event schema",
		},
		{"no observedAt", observedAtPattern.ReplaceAllString(valid, ""), "observedAt"},
		{"appendedSeq is not the line number", strings.Replace(valid, `"appendedSeq":1`, `"appendedSeq":7`, 1), "line number"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, []byte(tc.line+"\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := fixture.ReadEvents(path)
			if err == nil {
				t.Fatal("ReadEvents accepted a line it should have refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	t.Run("rejected file needs no observedAt", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "rejected.jsonl")
		line := strings.Replace(valid, `"observedAt":"2026-09-01T13:00:02Z",`, "", 1)
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := fixture.ReadRejected(path); err != nil {
			t.Fatalf("ReadRejected: %v", err)
		}
	})
}

func firstLine(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	line, _, found := strings.Cut(string(raw), "\n")
	if !found {
		t.Fatalf("%s has no newline", path)
	}
	return line
}
