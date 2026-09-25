// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `query pointers` end to end (FR-030, FR-035, T068).

// TestQueryPointersJSONMatchesTheGolden: the command and the fixture harness answer the same
// bytes.
func TestQueryPointersJSONMatchesTheGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "payments-pointers-1432")

	stdout, stderr, code := run(t, context.Background(),
		"--output", "json", "--server", baseURL, "--token", token,
		"query", "pointers", q.Focus, "--as-of", validAt(q))
	if code != ExitOK {
		t.Fatalf("query pointers: exit %d (stderr %q)", code, stderr)
	}

	want, err := os.ReadFile(filepath.Join(dir, "golden", "pointers."+q.Name+".json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(string(want)) {
		t.Errorf("`query pointers --output json` does not match the recorded golden.\n got: %s\nwant: %s",
			truncateForDiff(stdout), truncateForDiff(string(want)))
	}
}

// TestQueryPointersTableIsGroupedAndDated: the table groups by kind and names the backend and
// vocabulary of every selector, so nothing gets pasted into the wrong query bar. And it answers
// as of the instant it was asked about: before the fixture's 14:00 rename the selectors use the
// old name, which is US5 scenario 2 seen from the command line.
func TestQueryPointersTableIsGroupedAndDated(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)
	before := manifestQuery(t, dir, "payments-pointers-1300")
	after := manifestQuery(t, dir, "payments-pointers-1432")

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "pointers", after.Focus, "--as-of", validAt(after))
	if code != ExitOK {
		t.Fatalf("query pointers: exit %d (stderr %q)", code, stderr)
	}
	for _, want := range []string{
		"focus     payments-v2",
		"METRIC",
		"TRACE",
		"BACKEND",
		"VOCABULARY",
		"SELECTOR",
		"tempo",
		"otel-semconv/1.30",
		`service.name="payments-v2"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the pointers table does not mention %q:\n%s", want, stdout)
		}
	}

	earlier, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "pointers", before.Focus, "--as-of", validAt(before))
	if code != ExitOK {
		t.Fatalf("query pointers at 13:00: exit %d (stderr %q)", code, stderr)
	}
	if strings.Contains(earlier, "payments-v2") {
		t.Errorf("a lookup before the 14:00 rename must use the old name:\n%s", earlier)
	}
	if !strings.Contains(earlier, `service.name="payments"`) {
		t.Errorf("the 13:00 selectors are missing the pre-rename name:\n%s", earlier)
	}
}

// TestQueryPointersUsageErrors: a malformed invocation is refused before a round trip.
func TestQueryPointersUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no as-of", []string{"query", "pointers", "otel.service.name=payments"}},
		{"bad focus", []string{"query", "pointers", "payments", "--as-of", "now"}},
		{"bad instant", []string{"query", "pointers", "a=b", "--as-of", "whenever"}},
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
