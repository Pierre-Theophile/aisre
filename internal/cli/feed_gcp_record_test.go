// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `feed gcp --record` on a live run is refused (T160, FR-137).
//
// FR-137: *"Sanitisation MUST happen in the connector, before anything touches disk. No unsanitised
// GCP payload and no announcement body may be written to disk, to a log or to any artifact at any
// point, including during a failed or aborted run."*
//
// The boundary that enforces it exists — `internal/feeders/gcp.SanitisedRecorder`, which cannot be
// constructed without a Sanitiser and is the only route from the connector to a Sink. What does not
// exist yet is anything routing `--record` through it: the flag goes to `record.Wrap`, which writes
// the payload it was handed. For the synthetic twins that is harmless, because they are clean by
// construction. For a real GCP response it would write the response to disk, which is the one thing
// FR-137 forbids.
//
// # Why this test exists while the hazard is unreachable
//
// A live run is refused today anyway, because the live poller is not wired. So `--record` cannot
// currently be reached on a live path and no unsanitised byte can be written — which is exactly the
// situation in which the requirement gets forgotten, because nothing fails. The refusal is on the
// FLAG rather than beside the poller, so whoever wires the poller meets it and has to wire the
// sanitiser with it.
//
// If this test ever fails because the message changed, the question to ask is not "which string do I
// update" but "does `--record` now route through the SanitisedRecorder". If it does, this test should
// be replaced by one that records a payload and asserts the sanitiser ran.
func TestRecordingALiveGCPRunIsRefusedUntilSanitisationIsWired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "campaign")

	_, stderr, code := run(t, t.Context(), "feed", "gcp",
		"--org", "twin",
		"--projects", "twin-production",
		"--regions", "europe-west1",
		"--record", dir)

	if code == ExitOK {
		t.Fatal("`feed gcp --record` on a live run exited zero; recording a live GCP response " +
			"without the sanitiser writes it to disk unsanitised (FR-137)")
	}
	// The refusal has to name FR-137 and say why an afterwards-cleanup is not available, because the
	// obvious reaction to a refused recording is to take it anyway and clean it later — and by then
	// the bytes have been written.
	for _, want := range []string{"FR-137", "unsanitised", "cannot be cleaned afterwards"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q; it reads: %s", want, stderr)
		}
	}

	// And nothing was created. "Refused before the bytes existed" is the claim, so a directory left
	// behind would falsify it even with no payload in it: an empty campaign directory is what a
	// human then copies a raw recording into.
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("%s was created by a refused run; the refusal must come before anything touches "+
			"disk (FR-137)", dir)
	}
}

// And the refusal is about a LIVE run, not about `--record` itself: a recording of a replay is
// refused for an unrelated reason, and conflating the two would hide whichever one gets fixed first.
func TestRecordingAReplayIsRefusedForItsOwnReason(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "campaign")

	_, stderr, code := run(t, t.Context(), "feed", "gcp",
		"--org", "twin",
		"--projects", "twin-production",
		"--regions", "europe-west1",
		"--replay", "fixtures/gcp-baseline-topology-01",
		"--record", dir)

	if code == ExitOK {
		t.Fatal("`--record` with `--replay` exited zero; re-recording a replay produces a fixture " +
			"of a fixture")
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("the refusal does not name the mutual exclusion; it reads: %s", stderr)
	}
	if strings.Contains(stderr, "FR-137") {
		t.Error("a replay recording was refused on FR-137 grounds; the two refusals are about " +
			"different things and collapsing them hides whichever is fixed first")
	}
}
