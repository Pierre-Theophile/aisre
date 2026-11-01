// SPDX-License-Identifier: Apache-2.0

package runner_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	"github.com/Pierre-Theophile/aisre/internal/investigation/verify"
)

// verifierModel is the model config/model.yaml assigns the verifier role, so the canned verdict
// turn is rendered in the body shape that model's provider answers with.
func verifierModel(t *testing.T) string {
	t.Helper()
	config, _, err := model.LoadPair(
		filepath.Join("..", "..", "..", "config", "model.yaml"),
		filepath.Join("..", "..", "..", "config", "prices.yaml"))
	if err != nil {
		t.Fatalf("model config: %v", err)
	}
	return config.Roles[model.RoleVerifier].Model
}

// proposedID is the id the engine gives the investigator's hypothesis in this scenario: the
// ranker's two candidates take h-1 and h-2 before the first turn.
const proposedID = "h-3"

// modelRun is a run in which the investigator proposes one hypothesis, tests it against the
// first wave's evidence and concludes, followed by the verifier's answer.
func modelRun(t *testing.T, verdict string) []modeltest.Turn {
	t.Helper()
	return []modeltest.Turn{
		{
			Text: "The graph puts a second candidate near the subject; recording it.",
			ToolUses: []modeltest.ToolUse{{
				ID: "toolu_v1", Name: engine.ToolProposeHypothesis, Input: map[string]any{
					"kind":               "condition",
					"statement":          "a downstream dependency is saturated",
					"target_entity_refs": []string{paymentsRef},
				},
			}},
			Usage: model.Usage{Input: 12000, CacheWrite: 9000, Output: 300},
		},
		{
			Text: "Recording the judgment.",
			ToolUses: []modeltest.ToolUse{{
				ID: "toolu_v2", Name: engine.ToolProposeJudgments, Input: map[string]any{
					"judgments": []map[string]any{{
						"hypothesis_id": proposedID, "evidence_id": "e-2",
						"direction": "supports", "strength": "moderate",
						"rationale": "payments latency rose with the symptom",
					}},
				},
			}},
			Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 200},
		},
		{Text: "Verdict: the rollout.", Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 120}},
		{
			Model: verifierModel(t),
			Text: `{"findings":[{"claim_ref":"` + verify.HypothesisClaimRef(proposedID) + `","verdict":"` +
				verdict + `","reason":"the cited digest says nothing about saturation"}]}`,
			Usage: model.Usage{Input: 900, Output: 80},
		},
	}
}

// TestAModelClaimTheVerifierRemovesIsWithheld is FR-022c end to end: the investigator's statement
// goes through the fresh-context verifier before the conclusion, the verdict is stored with the
// investigation, and every rendering, including one rebuilt from the wire, withholds it.
func TestAModelClaimTheVerifierRemovesIsWithheld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, modelRun(t, verify.VerdictUnsupported))
	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if final.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED (%q)", final.GetLifecycle(), final.GetStopDetail())
	}
	if final.GetVerificationStatus() != investigationv1.VerificationStatus_VERIFIED {
		t.Fatalf("verification = %v (%q), want VERIFIED", final.GetVerificationStatus(), final.GetVerificationDetail())
	}
	if !strings.Contains(final.GetVerificationDetail(), "1 removed") {
		t.Errorf("verification detail %q does not count the removed claim", final.GetVerificationDetail())
	}

	findings := final.GetVerifierFindings()
	if len(findings) != 1 || findings[0].GetClaimRef() != verify.HypothesisClaimRef(proposedID) ||
		findings[0].GetActionTaken() != verify.ActionRemoved {
		t.Fatalf("findings = %v, want the proposed statement removed", findings)
	}

	// The verifier's exchange is a model call like any other, recorded under its own role.
	var verifierCalls int
	if err := h.store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM investigation.model_calls WHERE investigation_id = $1 AND role = $2`,
		final.GetInvestigationId(), string(model.RoleVerifier)).Scan(&verifierCalls); err != nil {
		t.Fatalf("count verifier calls: %v", err)
	}
	if verifierCalls != 1 {
		t.Errorf("verifier model calls = %d, want 1", verifierCalls)
	}

	// The stored row says the same after a re-read.
	reread, err := h.dao.Get(ctx, final.GetInvestigationId())
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.GetVerificationStatus() != investigationv1.VerificationStatus_VERIFIED ||
		len(reread.GetVerifierFindings()) != 1 {
		t.Errorf("re-read verification = %v with %d findings", reread.GetVerificationStatus(),
			len(reread.GetVerifierFindings()))
	}
	// The concluded message is read back from the row, so rendering it is rendering from the wire.
	report, err := render.FromProto(final)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	text, err := report.Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(text, "a downstream dependency is saturated") {
		t.Error("the removed statement is still published")
	}
	if !strings.Contains(text, "statement withheld (verifier: unsupported)") {
		t.Error("the ranked list does not say the statement was withheld:\n" + text)
	}
	if !strings.Contains(text, "verification: verified") {
		t.Error("the verdict section does not carry the verification line")
	}
}

// TestAKeptClaimIsPublishedAsWritten is the other half: a supported statement is published
// unchanged, under a verified run.
func TestAKeptClaimIsPublishedAsWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, modelRun(t, verify.VerdictSupported))
	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if final.GetVerificationStatus() != investigationv1.VerificationStatus_VERIFIED {
		t.Fatalf("verification = %v (%q), want VERIFIED", final.GetVerificationStatus(), final.GetVerificationDetail())
	}
	report, err := render.FromProto(final)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	text, err := report.Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(text, "a downstream dependency is saturated") {
		t.Error("a kept statement is not published")
	}
}

// TestAVerifierThatDoesNotAnswerLeavesTheRunNotVerified: the investigator's turns are all there
// is, so the verifier's call fails, and the run says it was not verified, and why (plan V3).
func TestAVerifierThatDoesNotAnswerLeavesTheRunNotVerified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	turns := modelRun(t, verify.VerdictSupported)
	h := newHarness(t, turns[:len(turns)-1])
	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if final.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED: a failed verifier does not fail the run", final.GetLifecycle())
	}
	if final.GetVerificationStatus() != investigationv1.VerificationStatus_NOT_VERIFIED {
		t.Fatalf("verification = %v, want NOT_VERIFIED", final.GetVerificationStatus())
	}
	if !strings.Contains(final.GetVerificationDetail(), "verifier failed") {
		t.Errorf("detail %q does not say the verifier failed", final.GetVerificationDetail())
	}
	report, err := render.FromProto(final)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	text, err := report.Human()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(text, "verification: NOT VERIFIED") {
		t.Error("the report does not say the run was not verified")
	}
}

// TestAModelFreeRunIsVerifiedWithNothingToCheck: no model, no model-authored sentence.
func TestAModelFreeRunIsVerifiedWithNothingToCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, nil)
	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if final.GetVerificationStatus() != investigationv1.VerificationStatus_VERIFIED {
		t.Fatalf("verification = %v (%q), want VERIFIED", final.GetVerificationStatus(), final.GetVerificationDetail())
	}
	if !strings.Contains(final.GetVerificationDetail(), "no model-authored claim") {
		t.Errorf("detail = %q", final.GetVerificationDetail())
	}
	if len(final.GetVerifierFindings()) != 0 {
		t.Errorf("a model-free run has findings: %v", final.GetVerifierFindings())
	}
	// The search-space profile is recorded with the investigation, model or not (FR-012a).
	if !strings.Contains(final.GetSearchSpaceProfile(), "focus: ") {
		t.Errorf("the concluded investigation records no search-space profile: %q", final.GetSearchSpaceProfile())
	}
	// And so is the examination report, with the table and mode in force (FR-022b).
	if final.GetExamination().GetExpectationTable() != "v1 (observe)" || len(final.GetExamination().GetHypotheses()) == 0 {
		t.Errorf("examination = %v", final.GetExamination())
	}
}
