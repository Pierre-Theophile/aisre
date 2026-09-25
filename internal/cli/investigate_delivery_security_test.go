// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
)

// The security pass on the first write path, at the CLI (T116, FR-057f, constitution VII v1.1.0).
//
// `TestInvestigateReportDeliversWithoutFailingTheCommand` covers the happy path and the
// never-blocking rule. What is asserted here is the confinement an operator could otherwise
// unpick by configuration: the delivery credential is its own variable, an unpublished transport
// is refused rather than defaulted, and no credential reaches the terminal.

// declareForDelivery opens an investigation declared in a chat channel and returns its id.
func declareForDelivery(t *testing.T, ctx context.Context, baseURL, token, origin string) string {
	t.Helper()
	out, stderr, code := run(t, ctx, "--output", "json",
		"--server", baseURL, "--token", token,
		"investigate", "declare",
		"--severity", "sev2", "--at", declaredAtRFC, "--title", "checkout 500s",
		"--origin", origin)
	if code != ExitOK {
		t.Fatalf("declare: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(out), &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := inv["investigationId"].(string)
	if id == "" {
		t.Fatal("declare returned no investigation id")
	}
	return id
}

// TestDeliveryNeverFallsBackToTheReadToken: the credential that posts a report "MUST be separate
// from every read credential of FR-008" (FR-057f). The separation is only real if the delivery
// path refuses to borrow the read token when its own variable is unset — which is the ordinary
// state of every deployment with no chat connector.
func TestDeliveryNeverFallsBackToTheReadToken(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()
	id := declareForDelivery(t, ctx, baseURL, token, "slack:C0SEPARATE")

	t.Setenv(EnvDeliveryToken, "")
	out, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "report", id, "--deliver")
	if code != ExitOK {
		t.Fatalf("report --deliver: exit %d (stderr %q)", code, stderr)
	}
	if strings.Contains(out, "delivered to") {
		t.Errorf("a report was delivered with no delivery credential; the read token was "+
			"borrowed:\n%s", out)
	}
	if !strings.Contains(out, "not delivered") || !strings.Contains(out, EnvDeliveryToken) {
		t.Errorf("the refusal does not name the credential it wants ($%s):\n%s", EnvDeliveryToken, out)
	}
	// The read token itself never appears in what the operator sees.
	if strings.Contains(out, token) || strings.Contains(stderr, token) {
		t.Error("the read credential was printed")
	}
}

// TestUnknownSinkIsRefusedNotDefaulted: an unpublished transport id is an error, never silently
// served by whichever sink is compiled in. A report going somewhere nobody chose is the write the
// constitution confines (T116).
func TestUnknownSinkIsRefusedNotDefaulted(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()
	id := declareForDelivery(t, ctx, baseURL, token, "slack:C0SINK")

	t.Setenv(EnvDeliveryToken, "s3cr3t-delivery-only")
	out, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "report", id, "--deliver", "--sink", "slack-webhook")
	if code != ExitOK {
		t.Fatalf("report --deliver --sink: exit %d (stderr %q)", code, stderr)
	}
	if strings.Contains(out, "delivered to") {
		t.Errorf("an unknown sink id was served by the default transport:\n%s", out)
	}
	if !strings.Contains(out, "unknown report-delivery sink") ||
		!strings.Contains(out, render.SinkLog) {
		t.Errorf("the refusal does not say which transports this build publishes:\n%s", out)
	}
}

// TestTheDeliveryCredentialIsNeverPrinted: the secret is never echoed, on any path — delivered,
// refused for an unknown sink, or not configured (constitution VII, FR-038).
func TestTheDeliveryCredentialIsNeverPrinted(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()
	id := declareForDelivery(t, ctx, baseURL, token, "slack:C0SECRET")

	const secret = "xoxb-9999-do-not-print-me"
	t.Setenv(EnvDeliveryToken, secret)
	for _, args := range [][]string{
		{"investigate", "report", id, "--deliver"},
		{"investigate", "report", id, "--deliver", "--sink", "nope"},
		{"--output", "json", "investigate", "report", id, "--deliver"},
	} {
		full := append([]string{"--server", baseURL, "--token", token}, args...)
		out, stderr, _ := run(t, ctx, full...)
		if strings.Contains(out, secret) || strings.Contains(stderr, secret) {
			t.Errorf("the delivery credential was printed by %v:\n%s\n%s", args, out, stderr)
		}
	}
}

// TestFactAgainstAConcludedInvestigationReopensIt is the drift the T118 quickstart run found
// (quickstart §6, FR-057b, contracts/cli.md `investigate fact`).
//
// `fact` recorded the fact and printed "a reopen runs a new linked investigation from this fact",
// leaving the reopen to a command that does not exist at the CLI. Its own `--help` promised the
// opposite — "A fact against a concluded investigation moves it to `reopened` and produces a new
// linked record" — and so did the contract. It now does it.
func TestFactAgainstAConcludedInvestigationReopensIt(t *testing.T) {
	baseURL, token, store := investigationServerFixtureWithStore(t)
	ctx := context.Background()

	id := declareForDelivery(t, ctx, baseURL, token, "slack:C0REOPEN")
	if err := investigationstore.NewLifecycleDAO(store).Conclude(ctx, id, investigationstore.Conclusion{
		Kind:        investigationstore.ConclusionFinal,
		Outcome:     investigationstore.OutcomeUnknown,
		VerdictLine: "no observed change explains this",
	}); err != nil {
		t.Fatalf("conclude %s: %v", id, err)
	}

	out, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "fact", id,
		"--kind", "manual_action",
		"--statement", "the load balancer was drained by hand at 14:05",
		"--entity", "k8s.service=shop/edge-lb")
	if code != ExitOK {
		t.Fatalf("fact: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(out, "reopened as") {
		t.Errorf("a fact against a concluded investigation did not reopen it (FR-057b):\n%s", out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("the output does not say the concluded record is still readable as produced:\n%s", out)
	}
}
