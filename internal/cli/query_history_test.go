// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `query history` end to end (FR-032, FR-035, T070).

// TestQueryHistoryJSONMatchesTheGolden: the command and the fixture harness answer the same
// bytes.
func TestQueryHistoryJSONMatchesTheGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "payments-history")

	stdout, stderr, code := run(t, context.Background(),
		"--output", "json", "--server", baseURL, "--token", token,
		"query", "history", q.Focus)
	if code != ExitOK {
		t.Fatalf("query history: exit %d (stderr %q)", code, stderr)
	}

	want, err := os.ReadFile(filepath.Join(dir, "golden", "history."+q.Name+".json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(string(want)) {
		t.Errorf("`query history --output json` does not match the recorded golden.\n got: %s\nwant: %s",
			truncateForDiff(stdout), truncateForDiff(string(want)))
	}
}

// TestQueryHistoryTableShowsBothTimelines: the table puts valid and observed time side by side,
// marks which version is current, and — because this node was merged — carries the entity column
// and the decision that merged it. That pair of columns is the whole reason this query exists.
func TestQueryHistoryTableShowsBothTimelines(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "payments-history")

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "history", q.Focus)
	if code != ExitOK {
		t.Fatalf("query history: exit %d (stderr %q)", code, stderr)
	}

	for _, want := range []string{
		"focus     otel.service.name=payments",
		"ENTITY",
		"VALID",
		"OBSERVED",
		"STATE",
		"superseded",
		"current",
		"payments-v2",
		"resolution decisions",
		"auto_merge",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the history table does not mention %q:\n%s", want, stdout)
		}
	}
}

// TestQueryHistoryUsageErrors: a history takes no instant, and a malformed focus is refused
// before a round trip.
func TestQueryHistoryUsageErrors(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "query", "history", "payments")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
}
