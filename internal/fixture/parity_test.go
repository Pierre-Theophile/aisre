// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/fixture/fixturetest"
)

// The live parity check (T159; FR-143, SC-014, SC-016).
//
// Parity is tested against two recordings rather than against a live run, because a live run's output
// *is* a recording — which is also what makes the check runnable at all without a credential. What is
// asserted is that it catches the two failures FR-143 names, and that it refuses to report parity over
// nothing.

// copyRecording copies a fixture's events, manifest and world into a new directory.
func copyRecording(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	for _, name := range []string{"manifest.yaml", "events.jsonl"} {
		body, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(src, "world"))
	if err != nil {
		return // a recording with no world; parity's digest half then has nothing to say
	}
	if err := os.MkdirAll(filepath.Join(dst, "world"), 0o755); err != nil {
		t.Fatalf("mkdir world: %v", err)
	}
	// Three world files are enough: this asserts the comparison, not the corpus.
	for i, e := range entries {
		if i >= 3 || e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(src, "world", e.Name()))
		if err != nil {
			t.Fatalf("read world/%s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, "world", e.Name()), body, 0o644); err != nil {
			t.Fatalf("write world/%s: %v", e.Name(), err)
		}
	}
}

const parityFixture = "../../fixtures/gcp-cross-source-merge-01"

// renumber rewrites an event's appendedSeq, which is the line number a loader checks.
func renumber(t *testing.T, line string, want, was int) string {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("parse event %d: %v", was, err)
	}
	event["appendedSeq"] = want
	out, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode event %d: %v", was, err)
	}
	return string(out)
}

// Two recordings of the same run have parity. Without this the failure cases below prove nothing:
// a check that always failed would pass every one of them.
func TestParityHoldsBetweenTwoCopiesOfOneRecording(t *testing.T) {
	newStore := fixturetest.PgtestFactory(t)
	root := t.TempDir()
	live, recorded := filepath.Join(root, "live"), filepath.Join(root, "recorded")
	copyRecording(t, parityFixture, live)
	copyRecording(t, parityFixture, recorded)

	got, err := fixture.Parity(t.Context(), newStore, live, recorded)
	if err != nil {
		t.Fatalf("Parity: %v", err)
	}
	if !got.Passed() {
		t.Fatalf("two copies of one recording do not have parity:\ngraph: %s\nworld: %s",
			got.GraphDetail, got.WorldDetail)
	}
	// And it compared something. Parity over nothing is the emptiest possible pass.
	if got.Events == 0 {
		t.Error("parity reports 0 events compared, so the graph half held over nothing")
	}
	if got.WorldEntries == 0 {
		t.Error("parity reports 0 world entries, so the digest half held over nothing")
	}
}

// The graph half: a feeder that behaves differently when recording.
func TestParityCatchesAGraphThatDiffers(t *testing.T) {
	newStore := fixturetest.PgtestFactory(t)
	root := t.TempDir()
	live, recorded := filepath.Join(root, "live"), filepath.Join(root, "recorded")
	copyRecording(t, parityFixture, live)
	copyRecording(t, parityFixture, recorded)

	// Drop one SOURCE's events from the live side — the shape of a connector that took a different
	// branch, or stopped, while recording.
	//
	// The first attempt dropped the LAST event and parity held, correctly: that event is a
	// `sourceCheckpoint`, which moves no valid-time state, so the two graphs really were identical
	// and the mutation asserted nothing. A mutation has to change the thing being compared, and
	// removing the observed side of the cross-source merge does: C4 cannot fire, so the merged
	// entity is two entities.
	path := filepath.Join(live, "events.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the live events: %v", err)
	}
	var kept []string
	var dropped int
	for i, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.Contains(line, `"sourceId":"otel:twin"`) {
			dropped++
			continue
		}
		// `appendedSeq` is the line number, so the stream has to be renumbered or the loader
		// refuses it — which is itself the rule `record.Emitter` had to learn.
		kept = append(kept, renumber(t, line, len(kept)+1, i))
	}
	if dropped == 0 {
		t.Fatal("no otel:twin event was dropped; the fixture's shape changed and this mutation " +
			"would assert nothing")
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write the filtered live events: %v", err)
	}

	got, err := fixture.Parity(t.Context(), newStore, live, recorded)
	if err != nil {
		t.Fatalf("Parity: %v", err)
	}
	if got.GraphEqual {
		t.Error("parity holds with an event missing from the live side; the graph half catches a " +
			"feeder that behaves differently when it is recording, and this is that")
	}
	if !strings.Contains(got.GraphDetail, "differ") {
		t.Errorf("the graph detail does not say the graphs differ: %s", got.GraphDetail)
	}
	// The world is untouched, so the digest half must still hold — the two halves fail for
	// different reasons and a check that collapsed them would hide whichever was fixed first.
	if !got.WorldEqual {
		t.Errorf("the digest half failed on a graph-only difference: %s", got.WorldDetail)
	}
	if got.Passed() {
		t.Error("Passed() is true with the graph half failing")
	}
}

// The digest half: a backend deriving an answer from the connection rather than from the response
// (FR-008). It produces identical graphs and different digests, which is why the graph half alone
// would pass the case FR-008 exists to prevent.
func TestParityCatchesADigestThatDiffersWhileTheGraphMatches(t *testing.T) {
	newStore := fixturetest.PgtestFactory(t)
	root := t.TempDir()
	live, recorded := filepath.Join(root, "live"), filepath.Join(root, "recorded")
	copyRecording(t, parityFixture, live)
	copyRecording(t, parityFixture, recorded)

	entries, err := os.ReadDir(filepath.Join(live, "world"))
	if err != nil || len(entries) == 0 {
		t.Skipf("the parity fixture has no world to perturb: %v", err)
	}

	t.Run("a response answered differently", func(t *testing.T) {
		victim := filepath.Join(live, "world", entries[0].Name())
		body, err := os.ReadFile(victim)
		if err != nil {
			t.Fatalf("read the world file: %v", err)
		}
		// One byte of the recorded response, which is the whole point: the digest is computed from
		// what the API returned, so any field differing is a parity failure.
		if err := os.WriteFile(victim, append(body, ' '), 0o644); err != nil {
			t.Fatalf("perturb the world file: %v", err)
		}
		defer func() { _ = os.WriteFile(victim, body, 0o644) }()

		got, err := fixture.Parity(t.Context(), newStore, live, recorded)
		if err != nil {
			t.Fatalf("Parity: %v", err)
		}
		if got.WorldEqual {
			t.Error("parity holds with a recorded response differing by one byte; a backend " +
				"deriving an answer from the connection rather than the response produces exactly " +
				"this — identical graphs, different digests (FR-008)")
		}
		if !strings.Contains(got.WorldDetail, "answered differently") {
			t.Errorf("the world detail does not say a request was answered differently: %s", got.WorldDetail)
		}
		// The graph is untouched, so its half must hold.
		if !got.GraphEqual {
			t.Errorf("the graph half failed on a world-only difference: %s", got.GraphDetail)
		}
	})

	t.Run("a request the live run answered and the recording does not", func(t *testing.T) {
		extra := filepath.Join(live, "world", "0000000000000000000000000000000000000000000000000000000000000000.json")
		if err := os.WriteFile(extra, []byte("{\"term\":\"compare\"}\n"), 0o644); err != nil {
			t.Fatalf("add a world file: %v", err)
		}
		defer func() { _ = os.Remove(extra) }()

		got, err := fixture.Parity(t.Context(), newStore, live, recorded)
		if err != nil {
			t.Fatalf("Parity: %v", err)
		}
		if got.WorldEqual {
			t.Error("parity holds with the live run answering a request the recording does not; " +
				"that is a live run the recording cannot replay")
		}
		if !strings.Contains(got.WorldDetail, "cannot replay") {
			t.Errorf("the world detail does not say the recording cannot replay it: %s", got.WorldDetail)
		}
	})
}

// Parity over nothing is refused. Two empty directories are identical, and reporting that as parity
// would be the emptiest pass there is.
func TestParityRefusesToHoldOverNothing(t *testing.T) {
	newStore := fixturetest.PgtestFactory(t)
	root := t.TempDir()
	live, recorded := filepath.Join(root, "live"), filepath.Join(root, "recorded")
	for _, dir := range []string{live, recorded} {
		copyRecording(t, parityFixture, dir)
		// Empty the event stream and remove the world, leaving a valid manifest over nothing.
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o644); err != nil {
			t.Fatalf("empty the events: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(dir, "world")); err != nil {
			t.Fatalf("remove the world: %v", err)
		}
	}

	_, err := fixture.Parity(t.Context(), newStore, live, recorded)
	if err == nil {
		t.Fatal("parity held over two empty recordings")
	}
	if !strings.Contains(err.Error(), "held over nothing") {
		t.Errorf("the refusal reads %q", err)
	}
}

// A recording with no world is not a parity failure: a topology recording legitimately has none, and
// failing on that would fail every parity run over one.
func TestARecordingWithNoWorldIsNotADigestFailure(t *testing.T) {
	newStore := fixturetest.PgtestFactory(t)
	root := t.TempDir()
	live, recorded := filepath.Join(root, "live"), filepath.Join(root, "recorded")
	for _, dir := range []string{live, recorded} {
		copyRecording(t, parityFixture, dir)
		if err := os.RemoveAll(filepath.Join(dir, "world")); err != nil {
			t.Fatalf("remove the world: %v", err)
		}
	}

	got, err := fixture.Parity(t.Context(), newStore, live, recorded)
	if err != nil {
		t.Fatalf("Parity: %v", err)
	}
	if !got.WorldEqual {
		t.Errorf("a pair of recordings with no world reports a digest failure: %s", got.WorldDetail)
	}
	if !strings.Contains(got.WorldDetail, "nothing to compare") {
		t.Errorf("the detail does not say the digest half had nothing to compare: %s", got.WorldDetail)
	}
	// The graph half still ran, so this is not passing because nothing was checked.
	if !got.GraphEqual || got.Events == 0 {
		t.Errorf("the graph half did not run: equal=%v events=%d", got.GraphEqual, got.Events)
	}
}
