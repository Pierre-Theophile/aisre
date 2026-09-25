// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// `fixture verify --report` on a rollout-regression fixture (T041, SC-005,
// contracts/cli.md §Fixtures).
//
// Verification says the graph still answers what it answered yesterday. The report says whether
// what it answers is *right*: on a fixture whose culprit is written down, where does the
// published ranking put it? That number is the one SC-005 is stated in, so it has to be
// reachable from the command line and not only from a Go test.

// TestFixtureVerifyReportMeasuresTheRanking: the Markdown rendering carries a Ranking section
// naming the culprit and its rank, and the JSON rendering carries the same as data.
func TestFixtureVerifyReportMeasuresTheRanking(t *testing.T) {
	store := pgtest.Open(t)
	dsn := store.Pool().Config().ConnString()

	stdout, stderr, code := run(t, context.Background(),
		"fixture", "verify", "--db", dsn, "--report", "--shuffles", "0",
		"../../fixtures/rollout-regression-01")
	if code != ExitOK {
		t.Fatalf("fixture verify --report: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"### Ranking (SC-005)",
		"k8s.change=shop/payments@rev7",
		"1 of 4",
		"yes (k=3)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not mention %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, code = run(t, context.Background(),
		"--output", "json", "fixture", "verify", "--db", dsn, "--report", "--shuffles", "0",
		"../../fixtures/rollout-regression-01")
	if code != ExitOK {
		t.Fatalf("fixture verify --report --output json: exit %d (stderr %s)", code, stderr)
	}
	var report struct {
		Passed  bool `json:"passed"`
		Metrics struct {
			Ranking map[string]struct {
				Culprit    string `json:"culprit_change"`
				Rank       int    `json:"rank"`
				TopKHit    bool   `json:"top_k_hit"`
				Candidates int    `json:"candidates"`
			} `json:"ranking"`
			Calibration any `json:"calibration"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &report); err != nil {
		t.Fatalf("decode verify report %s: %v", stdout, err)
	}
	if !report.Passed {
		t.Errorf("the fixture did not verify: %s", stdout)
	}
	metrics, ok := report.Metrics.Ranking["checkout-diff"]
	if !ok {
		t.Fatalf("no ranking metrics for checkout-diff in %s", stdout)
	}
	if metrics.Rank != 1 || !metrics.TopKHit || metrics.Candidates != 4 {
		t.Errorf("ranking metrics = %+v, want rank 1, a top-k hit, 4 candidates", metrics)
	}
	if metrics.Culprit != "k8s.change=shop/payments@rev7" {
		t.Errorf("culprit = %q, want the payments rollout", metrics.Culprit)
	}
	// Calibration is US6's to fill in; until then the key is present and null, so a consumer
	// can tell "not measured" from "measured as zero" (constitution V).
	if report.Metrics.Calibration != nil {
		t.Errorf("calibration = %v, want null until the resolution phase measures it",
			report.Metrics.Calibration)
	}
}
