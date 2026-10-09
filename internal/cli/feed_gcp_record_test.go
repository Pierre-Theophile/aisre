// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `feed gcp --record` on a live run is sanitised in the connector, and cannot start without the
// corpus key (T187, FR-137).
//
// FR-137: *"Sanitisation MUST happen in the connector, before anything touches disk. No unsanitised
// GCP payload and no announcement body may be written to disk, to a log or to any artifact at any
// point, including during a failed or aborted run."*
//
// Until T187 a live recording was refused outright, because nothing routed `--record` through a
// sanitiser. It now goes through the same tee the deploy feeders use, and the sanitiser is built
// before anything else — so the one way left to write an unsanitised byte is to run without a key,
// and that is refused before a directory exists. That the bytes written ARE sanitised is
// internal/feeders/gcp's TestALiveRecordingLeavesNoIdentifierOnDisk.
func TestRecordingALiveGCPRunNeedsTheCorpusKey(t *testing.T) {
	t.Setenv("SRE_AGENT_CORPUS_KEY", "")
	dir := filepath.Join(t.TempDir(), "campaign")

	_, stderr, code := run(t, t.Context(), "feed", "gcp",
		"--org", "twin",
		"--projects", "twin-production",
		"--regions", "europe-west1",
		"--record", dir)

	if code == ExitOK {
		t.Fatal("`feed gcp --record` on a live run with no corpus key exited zero; it cannot sanitise (FR-137)")
	}
	for _, want := range []string{"FR-137", "before anything touches disk"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q; it reads: %s", want, stderr)
		}
	}
	// Refused before the bytes existed: a directory left behind would falsify it even empty, because
	// an empty campaign directory is what a human then copies a raw recording into.
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("%s was created by a refused run; the refusal must come before anything touches disk (FR-137)", dir)
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
