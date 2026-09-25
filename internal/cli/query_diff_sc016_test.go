// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SC-016 (004 T128): starting from an alert instant alone, the in-scope changes of the preceding window,
// each with its actor, actor kind, the commit it shipped, whether it was a rollback and a link that opens
// the run — in one command, under thirty seconds, on the recorded corpus.
//
// The corpus is `deploy-cross-source-merge-01`, where the rollout an investigation cares about was seen by
// two connectors and merged by C8: so this also checks that the answer carries what BOTH sources said —
// the pipeline's actor and run, the platform's revision — rather than whichever arrived last.

func TestSC016ChangesFromAnAlertInstantAloneWithTheirProvenance(t *testing.T) {
	baseURL, token := queryServer(t, filepath.Join("..", "..", "fixtures", "deploy-cross-source-merge-01"))

	started := time.Now()
	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "diff", "gcp.cloudrun.service=twin-production/europe-west1/storefront",
		"--at", "2026-09-21T14:40:00Z",
		"--observed-at", "2026-09-21T20:00:00Z")
	elapsed := time.Since(started)
	if code != ExitOK {
		t.Fatalf("query diff --at: exit %d (stderr %q)", code, stderr)
	}
	if elapsed > 30*time.Second {
		t.Errorf("the answer took %s; SC-016 asks for under thirty seconds", elapsed)
	}

	// The window is the two hours before the alert, and the ranking is measured to the alert instant.
	if !strings.Contains(stdout, "window    valid (2026-09-21T12:40:00Z, 2026-09-21T14:40:00Z]") {
		t.Errorf("--at did not derive the two-hour window before the alert:\n%s", stdout)
	}
	if !strings.Contains(stdout, "reference = 2026-09-21T14:40:00Z") {
		t.Errorf("the ranking is not measured to the alert instant:\n%s", stdout)
	}

	// Each change names the services it landed on, not their canonical ids (004 T156): `orders` did not
	// change in the window, so only the answer's change_targets can describe it.
	if !strings.Contains(stdout, "orders-00018-ddd, orders ") {
		t.Errorf("the orders revision's TARGET does not name the orders service:\n%s", stdout)
	}

	// The provenance table, and the merged production rollout's row in it.
	for _, header := range []string{"ACTOR", "ACTOR-KIND", "COMMIT", "ROLLBACK", "LINK"} {
		if !strings.Contains(stdout, header) {
			t.Errorf("the provenance table has no %s column:\n%s", header, stdout)
		}
	}
	row := lineContaining(stdout, "https://github.com/acme/storefront/actions/runs/9821")
	if row == "" {
		t.Fatalf("no change links the run that shipped the production rollout:\n%s", stdout)
	}
	for _, want := range []string{"release-runner", "AUTOMATION", "9f8e7d6c5b4a"} {
		if !strings.Contains(row, want) {
			t.Errorf("the merged rollout's row lacks %q (the pipeline's actor, its kind, the commit): %q", want, row)
		}
	}
}

// And through --at on a fixture with a stated rollback: the column says so and names what was restored.
func TestSC016TheRollbackColumnSaysWhatWasRestored(t *testing.T) {
	baseURL, token := queryServer(t, filepath.Join("..", "..", "fixtures", "deploy-rollback-01"))

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "diff", "vercel.project=prj_storefront",
		"--at", "2026-09-21T15:00:00Z", "--window", "1h",
		"--observed-at", "2026-09-21T20:00:00Z")
	if code != ExitOK {
		t.Fatalf("query diff --at: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "yes → dpl_good") {
		t.Errorf("the rollback column does not name the restored deployment:\n%s", stdout)
	}
}

// --at and --from/--to together are a usage error: --at derives the window itself.
func TestSC016AtAndAnExplicitWindowAreRefused(t *testing.T) {
	_, stderr, code := run(t, context.Background(),
		"--server", "http://127.0.0.1:1", "--token", "x",
		"query", "diff", "otel.service.name=checkout",
		"--at", "2026-09-21T14:40:00Z", "--from", "2026-09-21T12:00:00Z")
	if code != ExitUsage || !strings.Contains(stderr, "--at") {
		t.Errorf("exit %d, stderr %q; want a usage error naming --at", code, stderr)
	}
}

func lineContaining(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
