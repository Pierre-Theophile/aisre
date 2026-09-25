// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// `backend list` is the operator-facing half of "budgets are expressed per backend AND per cost
// class" (T111, FR-047a): a term that does not appear here with a class has no way to be budgeted
// for, and is therefore not callable.

// The GCP backend's declaration is printable without a credential, and it prints the four things
// an operator needs to decide whether to let a term run: the term, its class, its window cap and
// that answering it changes nothing.
func TestBackendListPrintsTheGCPDeclaration(t *testing.T) {
	stdout, stderr, code := run(t, context.Background(), "backend", "list", "--gcp", "acme")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "gcp:acme (vendor gcp") {
		t.Errorf("the GCP backend is not listed:\n%s", stdout)
	}
	for _, term := range []string{
		"compare", "onset", "new_log_patterns", "error_spans",
		"errors_by_version", "monitor_state", "exemplars", "drill_down",
	} {
		if !strings.Contains(stdout, term) {
			t.Errorf("term %s is not listed:\n%s", term, stdout)
		}
	}
	// GCP reports no in-band remaining figure, and the row says so rather than promising a
	// number the budget manager would then wait for.
	if !strings.Contains(stdout, "quota_undetermined") {
		t.Errorf("the quota row does not state that GCP reports no figure:\n%s", stdout)
	}
}

// The JSON rendering carries read_only and the window cap per row, because the point of a declared
// capability is that a reader does not have to take it on trust.
func TestBackendListJSONCarriesReadOnlyAndTheWindowCap(t *testing.T) {
	stdout, stderr, code := run(t, context.Background(), "--output", "json", "backend", "list", "--gcp", "acme")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var payload struct {
		Backends []struct {
			Vendor string `json:"vendor"`
			Terms  []struct {
				Term             string `json:"term"`
				CostClass        string `json:"cost_class"`
				MaxWindowSeconds int64  `json:"max_window_seconds"`
				ReadOnly         bool   `json:"read_only"`
			} `json:"terms"`
		} `json:"backends"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("the JSON rendering does not parse: %v\n%s", err, stdout)
	}
	if len(payload.Backends) != 1 || payload.Backends[0].Vendor != "gcp" {
		t.Fatalf("expected one gcp backend, got %+v", payload.Backends)
	}
	if got := len(payload.Backends[0].Terms); got != 8 {
		t.Fatalf("got %d terms, want the eight telemetry terms", got)
	}
	for _, row := range payload.Backends[0].Terms {
		if !row.ReadOnly {
			t.Errorf("%s is not read_only; executing a term creates no GCP object of any kind", row.Term)
		}
		if row.MaxWindowSeconds <= 0 {
			t.Errorf("%s declares no window cap", row.Term)
		}
		switch row.CostClass {
		case "cheap", "standard", "expensive":
		default:
			t.Errorf("%s declares cost class %q, which is outside the published closed set",
				row.Term, row.CostClass)
		}
	}
}

// Without --gcp and without a recording, the command says what this build ships rather than
// printing an empty table a reader would take for "nothing is available".
func TestBackendListWithNoBackendSaysWhatIsAvailable(t *testing.T) {
	stdout, stderr, code := run(t, context.Background(), "backend", "list")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "no telemetry backend registered") {
		t.Errorf("unexpected output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--gcp") {
		t.Errorf("the message does not say how to print the live backend's declaration:\n%s", stdout)
	}
}
