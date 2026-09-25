// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `query diff` end to end (FR-027, FR-028, FR-035, T040).
//
// Same contract as `query subgraph`: the JSON rendering is the golden serialization, and the
// table is the rendering an on-call engineer reads at three in the morning. The table gets more
// attention here than elsewhere because a ranked list that does not say *why* it is in that
// order is exactly the kind of answer this project exists not to produce.

const rolloutFixture = "rollout-regression-01"

// TestQueryDiffJSONMatchesTheGolden: the command and the fixture harness answer the same bytes.
func TestQueryDiffJSONMatchesTheGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)

	stdout, stderr, code := run(t, context.Background(),
		"--output", "json", "--server", baseURL, "--token", token,
		"query", "diff", "otel.service.name=checkout",
		"--from", "2026-09-01T13:00:00Z", "--to", "2026-09-01T14:32:00Z",
		"--hops", "2", "--direction", "both")
	if code != ExitOK {
		t.Fatalf("query diff: exit %d (stderr %q)", code, stderr)
	}

	want, err := os.ReadFile(filepath.Join(dir, "golden", "diff.checkout-diff.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(string(want)) {
		t.Errorf("`query diff --output json` does not match the recorded golden.\n got: %s\nwant: %s",
			truncateForDiff(stdout), truncateForDiff(string(want)))
	}
}

// TestQueryDiffTableLeadsWithTheRanking: the ranked changes come first, with their components
// and the formula, then the deltas that are the evidence for them.
func TestQueryDiffTableLeadsWithTheRanking(t *testing.T) {
	baseURL, token := queryServer(t, filepath.Join("..", "..", "fixtures", rolloutFixture))

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "diff", "otel.service.name=checkout",
		"--from", "2026-09-01T13:00:00Z", "--to", "2026-09-01T14:32:00Z")
	if code != ExitOK {
		t.Fatalf("query diff: exit %d (stderr %q)", code, stderr)
	}

	for _, want := range []string{
		"window    valid (2026-09-01T13:00:00Z, 2026-09-01T14:32:00Z], observed now",
		"4 ranked change(s); nodes +0/-0/~3",
		"rollout payments to revision 7 (1.4.1)",
		"ROLLOUT",
		"SCALING",
		"unattached",
		"TEMPORAL",
		"nodes changed",
		"service.version",
		"1.4.0",
		"1.4.1",
		"ranking   score = 0.50*temporal",
		"complete: nothing was truncated",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the diff table does not mention %q:\n%s", want, stdout)
		}
	}

	// The culprit is printed first, before the decoys.
	culprit := strings.Index(stdout, "rollout payments to revision 7")
	scaling := strings.Index(stdout, "scale storefront from 3 to 6")
	if culprit < 0 || scaling < 0 || culprit > scaling {
		t.Errorf("the ranked table is not in rank order:\n%s", stdout)
	}
}

// TestQueryDiffUsageErrors: a malformed invocation is refused before a round trip.
func TestQueryDiffUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no window", []string{"query", "diff", "otel.service.name=checkout"}},
		{"only from", []string{"query", "diff", "a=b", "--from", "-1h"}},
		{"reversed window", []string{
			"query", "diff", "a=b", "--from", "2026-09-01T14:32:00Z", "--to", "2026-09-01T13:00:00Z",
		}},
		{"bad focus", []string{"query", "diff", "checkout", "--from", "-1h", "--to", "now"}},
		{"bad instant", []string{"query", "diff", "a=b", "--from", "lunchtime", "--to", "now"}},
		{"negative tau", []string{"query", "diff", "a=b", "--from", "-1h", "--to", "now", "--tau", "-5m"}},
		{"negative margin", []string{
			"query", "diff", "a=b", "--from", "-1h", "--to", "now", "--change-margin", "-1",
		}},
		{"bad direction", []string{
			"query", "diff", "a=b", "--from", "-1h", "--to", "now", "--direction", "sideways",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := run(t, context.Background(), tc.args...)
			if code != ExitUsage {
				t.Errorf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
			}
		})
	}
}
