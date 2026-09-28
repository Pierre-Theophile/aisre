// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SC-016 (005 T068): starting from a Datadog monitor id alone, one command answers the alert, what it
// watches, what changed there in the preceding window, who owns it, and executable pointers, in under
// thirty seconds on the recorded corpus (`datadog-monitor-to-owner-01`).
func TestSC016FromADatadogMonitorIDAlone(t *testing.T) {
	baseURL, token := queryServer(t, filepath.Join("..", "..", "fixtures", "datadog-monitor-to-owner-01"))

	started := time.Now()
	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "diff", "datadog.monitor=8101",
		"--at", "2026-09-21T14:40:00Z", "--hops", "2",
		"--observed-at", "2026-09-21T15:00:00Z")
	elapsed := time.Since(started)
	if code != ExitOK {
		t.Fatalf("query diff: exit %d (stderr %q)", code, stderr)
	}
	if elapsed > 30*time.Second {
		t.Errorf("the answer took %s; SC-016 asks for under thirty seconds", elapsed)
	}
	for what, want := range map[string]string{
		"the alert":                    "checkout error rate",
		"what it watches":              "checkout error rate -> checkout  watches",
		"what changed before it fired": "2b3c4d5e6f708192a3b4c5d6e7f80918a2b3c4d5",
		"who owns it":                  "payments",
		"the log pointer":              "datadog-logs/v1  service:checkout env:production",
		"the alert's state":            "sre.alert.state",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("%s: the answer does not contain %q:\n%s", what, want, stdout)
		}
	}
	// The transition does not hide what the monitor is (the alert lane, internal/projector): no
	// definition property reads as removed.
	if row := lineContaining(stdout, "service.name"); row != "" {
		t.Errorf("the monitor's definition changed across its transition: %q\n%s", row, stdout)
	}
	// The long-running commit predates the horizon: it is not a change.
	if strings.Contains(stdout, "1a2b3c4d5e6f708192a3b4c5d6e7f80918a2b3c4") {
		t.Errorf("the commit first seen before the horizon is reported as a change:\n%s", stdout)
	}
}
