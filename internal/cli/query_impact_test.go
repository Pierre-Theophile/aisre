// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `query impact` end to end (FR-029, FR-035, T065).
//
// The same two claims every query command has to hold: `--output json` is the golden
// serialization rather than a second rendering of it, and the table says out loud the one thing
// this command can get catastrophically wrong — which list is which.

// TestQueryImpactJSONMatchesTheGolden: the command and the fixture harness answer the same bytes.
func TestQueryImpactJSONMatchesTheGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "payments-impact")

	stdout, stderr, code := run(t, context.Background(),
		"--output", "json", "--server", baseURL, "--token", token,
		"query", "impact", q.Focus, "--as-of", validAt(q))
	if code != ExitOK {
		t.Fatalf("query impact: exit %d (stderr %q)", code, stderr)
	}

	want, err := os.ReadFile(filepath.Join(dir, "golden", "impact."+q.Name+".json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(string(want)) {
		t.Errorf("`query impact --output json` does not match the recorded golden.\n got: %s\nwant: %s",
			truncateForDiff(stdout), truncateForDiff(string(want)))
	}
}

// TestQueryImpactTableNamesBothDirections: the table prints dependents before dependencies, says
// in words which is which, and renders the heaviest path as the route it is rather than as a
// list of entity ids. Getting the two lists the wrong way round is the failure this asserts
// against — it would page the wrong team and look entirely plausible while doing it.
func TestQueryImpactTableNamesBothDirections(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures", rolloutFixture)
	baseURL, token := queryServer(t, dir)
	q := manifestQuery(t, dir, "payments-impact")

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"query", "impact", q.Focus, "--as-of", validAt(q))
	if code != ExitOK {
		t.Fatalf("query impact: exit %d (stderr %q)", code, stderr)
	}

	for _, want := range []string{
		"downstream — depends on the focus; breaks if it breaks",
		"upstream — the focus depends on these; could be breaking it",
		"HEAVIEST PATH",
		"payments -> checkout -> storefront",
		"payments-db",
		"complete: nothing was truncated",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the impact table does not mention %q:\n%s", want, stdout)
		}
	}

	// checkout depends on payments, payments depends on payments-db: each must be under the
	// heading that says so, and the two headings are in that order.
	down := strings.Index(stdout, "downstream — depends on the focus")
	up := strings.Index(stdout, "upstream — the focus depends on")
	if down < 0 || up < 0 || down > up {
		t.Fatalf("the two sections are missing or in the wrong order:\n%s", stdout)
	}
	if idx := strings.Index(stdout, "checkout"); idx < down || idx > up {
		t.Errorf("checkout is not in the downstream section:\n%s", stdout)
	}
	if idx := strings.Index(stdout, "payments-db"); idx < up {
		t.Errorf("payments-db is not in the upstream section:\n%s", stdout)
	}
}

// TestQueryImpactUsageErrors: a malformed invocation is refused before a round trip.
func TestQueryImpactUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no as-of", []string{"query", "impact", "otel.service.name=payments"}},
		{"bad focus", []string{"query", "impact", "payments", "--as-of", "now"}},
		{"bad instant", []string{"query", "impact", "a=b", "--as-of", "teatime"}},
		{"zero hops", []string{"query", "impact", "a=b", "--as-of", "now", "--max-hops", "0"}},
		{"too many hops", []string{"query", "impact", "a=b", "--as-of", "now", "--max-hops", "99"}},
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
