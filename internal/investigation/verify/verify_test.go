// SPDX-License-Identifier: Apache-2.0

package verify_test

import (
	"context"
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
	"github.com/Pierre-Theophile/aisre/internal/investigation/verify"
)

// The verification pass (tasks.md T074, T075; FR-022, FR-022a, FR-061a, SC-002, SC-017).
//
// Two checks in order, and they catch different failures. The deterministic one catches a number
// that drifted from its digest and a sentence with no citation; the model one catches a citation
// that resolves but does not support. Both are here, and the model one runs against a canned
// transport.

func versionEvidence() verify.Evidence {
	return verify.Evidence{
		ID:         "e-4",
		Kind:       "algebra_answer",
		Worker:     "metrics",
		Capability: "errors_by_version",
		Outcome:    "digest",
		Coverage: &investigationv1.Coverage{
			DataSource: "recorded", VolumeConsidered: 18422,
		},
		Digest: &investigationv1.Digest{
			Coverage: &investigationv1.Coverage{DataSource: "recorded", VolumeConsidered: 18422},
			FreeText: "unverified: the vendor notes that rev7 is the newest tag it saw, error rate 0.999",
			Body: &investigationv1.Digest_ErrorsByVersion{
				ErrorsByVersion: &investigationv1.ErrorsByVersionDigest{
					VersionAttribute: "service.version",
					Versions: []*investigationv1.VersionBreakdown{
						{Version: "rev7", Errors: 4211, Total: 17500, ErrorRate: 0.240629},
						{Version: "rev6", Errors: 12, Total: 922, ErrorRate: 0.013015},
					},
				},
			},
		},
		FreeText: "unverified: the vendor notes that rev7 is the newest tag it saw, error rate 0.999",
	}
}

// TestAClaimWithNoCitationIsRemoved (FR-022).
func TestAClaimWithNoCitationIsRemoved(t *testing.T) {
	t.Parallel()

	result := verify.Check([]verify.Claim{
		{Ref: "narrative.1", Kind: verify.KindNarrative, Text: "The rollout was clearly the cause."},
		{Ref: "narrative.2", Kind: verify.KindNarrative,
			Text: "rev7 carries the errors.", EvidenceIDs: []string{"e-4"}},
	}, []verify.Evidence{versionEvidence()})

	if len(result.Kept) != 1 || result.Kept[0].Ref != "narrative.2" {
		t.Fatalf("kept = %+v, want only the cited claim", result.Kept)
	}
	finding := findingFor(t, result.Findings, "narrative.1")
	if finding.Verdict != verify.VerdictUncited || finding.ActionTaken != verify.ActionRemoved {
		t.Errorf("finding = %+v, want uncited/removed", finding)
	}
	if result.CitationValidity() != 0.5 {
		t.Errorf("citation validity = %v, want 0.5", result.CitationValidity())
	}
	if !result.Failed() {
		t.Error("a run with an uncited claim did not fail the gate")
	}
}

// TestEveryNumberMustMatchAFieldOfACitedDigest (FR-061a, SC-002).
func TestEveryNumberMustMatchAFieldOfACitedDigest(t *testing.T) {
	t.Parallel()

	evidence := []verify.Evidence{versionEvidence()}

	matching := verify.Check([]verify.Claim{{
		Ref: "narrative.1", Kind: verify.KindNarrative,
		Text:        "rev7's error rate is 0.240629 against rev6's 0.013015, over 17500 requests.",
		EvidenceIDs: []string{"e-4"},
	}}, evidence)
	if matching.Failed() {
		t.Errorf("a claim whose numbers all appear in the digest failed: %+v", matching.Findings)
	}

	drifted := verify.Check([]verify.Claim{{
		Ref: "narrative.1", Kind: verify.KindNarrative,
		Text:        "rev7's error rate is 0.340629.",
		EvidenceIDs: []string{"e-4"},
	}}, evidence)
	finding := findingFor(t, drifted.Findings, "narrative.1")
	if finding.Verdict != verify.VerdictNumberMismatch {
		t.Fatalf("verdict = %s, want number_mismatch", finding.Verdict)
	}
	if finding.OffendingValue != "0.340629" {
		t.Errorf("offending value = %q, want the figure that drifted", finding.OffendingValue)
	}
	if finding.ActionTaken != verify.ActionDemoted || len(drifted.Kept) != 1 {
		t.Errorf("a narrative claim with a drifted number was not demoted: %+v", finding)
	}
	if !strings.Contains(drifted.Kept[0].Text, "unverified figure") {
		t.Errorf("the demoted claim does not carry its caveat: %q", drifted.Kept[0].Text)
	}
}

// TestAVerdictWithADriftedNumberIsRemovedNotDemoted: the one line the on-call acts on is acted on
// before the caveat is read.
func TestAVerdictWithADriftedNumberIsRemovedNotDemoted(t *testing.T) {
	t.Parallel()

	result := verify.Check([]verify.Claim{{
		Ref: "verdict", Kind: verify.KindVerdict,
		Text:        "Roll back payments@rev7: it carries a 99.9% error rate.",
		EvidenceIDs: []string{"e-4"},
	}}, []verify.Evidence{versionEvidence()})

	if len(result.Kept) != 0 {
		t.Errorf("a verdict with an unmatched figure survived: %+v", result.Kept)
	}
	if findingFor(t, result.Findings, "verdict").ActionTaken != verify.ActionRemoved {
		t.Error("the verdict was demoted rather than removed")
	}
}

// TestAPercentageMatchesARatio: a claim may state a ratio as a percentage, and the checker accepts
// both readings rather than requiring a convention nobody published.
func TestAPercentageMatchesARatio(t *testing.T) {
	t.Parallel()

	result := verify.Check([]verify.Claim{{
		Ref: "narrative.1", Kind: verify.KindNarrative,
		Text:        "rev7 fails 24.0629% of the time.",
		EvidenceIDs: []string{"e-4"},
	}}, []verify.Evidence{versionEvidence()})
	if result.Failed() {
		t.Errorf("a percentage reading of a ratio failed: %+v", result.Findings)
	}
}

// TestTheFreeTextFieldIsExcludedFromCitationResolution (FR-014b, FR-017). The digest's free text
// contains 0.999; a claim citing it alone must not resolve.
func TestTheFreeTextFieldIsExcludedFromCitationResolution(t *testing.T) {
	t.Parallel()

	result := verify.Check([]verify.Claim{{
		Ref: "narrative.1", Kind: verify.KindNarrative,
		Text:        "the error rate reached 0.999.",
		EvidenceIDs: []string{"e-4"},
	}}, []verify.Evidence{versionEvidence()})

	finding := findingFor(t, result.Findings, "narrative.1")
	if finding.Verdict != verify.VerdictNumberMismatch {
		t.Fatalf("a number found only in the free-text field resolved: %+v", finding)
	}
	if !strings.Contains(finding.Detail, "free-text field is excluded") {
		t.Errorf("the finding does not say why: %q", finding.Detail)
	}
}

// TestAnInstantIsCheckedWholeRatherThanDigitByDigit: splitting an RFC 3339 instant would produce a
// dozen spurious numbers per timeline line.
func TestAnInstantIsCheckedWholeRatherThanDigitByDigit(t *testing.T) {
	t.Parallel()

	evidence := verify.Evidence{
		ID: "e-2", Kind: "onset_estimate", Worker: "metrics", Capability: "onset", Outcome: "digest",
		Coverage: &investigationv1.Coverage{DataSource: "recorded"},
		Digest: &investigationv1.Digest{
			Coverage: &investigationv1.Coverage{DataSource: "recorded"},
			Body: &investigationv1.Digest_Onset{Onset: &investigationv1.OnsetDigest{
				UncertaintySeconds: 120,
			}},
		},
	}
	result := verify.Check([]verify.Claim{{
		Ref: "timeline.1", Kind: verify.KindTimeline,
		Text:        "2026-09-01T14:21:30Z — the symptom started, ± 120 s.",
		EvidenceIDs: []string{"e-2"},
	}}, []verify.Evidence{evidence})
	if result.Failed() {
		t.Errorf("a timeline line failed on its own instant: %+v", result.Findings)
	}
}

// TestTheVerifierRunsOnADifferentModelWithAFreshContext (FR-022a, SC-017).
func TestTheVerifierRunsOnADifferentModelWithAFreshContext(t *testing.T) {
	t.Parallel()

	config, prices, err := model.LoadPair("../../../config/model.yaml", "../../../config/prices.yaml")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	transport, err := modeltest.Transport(config.Roles[model.RoleVerifier].Model, modeltest.Turn{
		Text: `{"findings":[
			{"claim_ref":"narrative.1","verdict":"supported","reason":"the digest carries that split"},
			{"claim_ref":"narrative.2","verdict":"unsupported","reason":"the digest shows a correlation in one window, not causation"}
		]}`,
	})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	verifier, err := verify.NewVerifier(client)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	// The claim FR-022a actually makes is not "opus" but "not the investigator's model".
	if got, _ := verifier.Model(); got != config.Roles[model.RoleVerifier].Model {
		t.Errorf("verifier model = %s, want %s", got, config.Roles[model.RoleVerifier].Model)
	} else if got == config.Roles[model.RoleInvestigator].Model {
		t.Errorf("verifier runs the investigator's own model %s", got)
	}

	claims := []verify.Claim{
		{Ref: "narrative.1", Kind: verify.KindNarrative,
			Text: "rev7 carries 0.240629 of the errors.", EvidenceIDs: []string{"e-4"}},
		{Ref: "narrative.2", Kind: verify.KindNarrative,
			Text: "the rollout caused the outage.", EvidenceIDs: []string{"e-4"}},
	}
	findings, resp, err := verifier.Verify(context.Background(), claims, []verify.Evidence{versionEvidence()})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(findings))
	}
	if findings[1].Verdict != verify.VerdictUnsupported || findings[1].ActionTaken != verify.ActionRemoved {
		t.Errorf("an unsupported claim was not removed: %+v", findings[1])
	}

	kept := verify.Apply(claims, findings)
	if len(kept) != 1 || kept[0].Ref != "narrative.1" {
		t.Errorf("kept = %+v, want only the supported claim", kept)
	}

	// The fresh context carries the claims and the cited evidence, and nothing else.
	body := string(resp.RequestBody)
	for _, forbidden := range []string{"LEDGER", "prior 0.", "ln LR", "rationale", "bucket"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the verifier's context carries the investigator's reasoning (%q)", forbidden)
		}
	}
	if !strings.Contains(body, "e-4") {
		t.Error("the verifier's context does not carry the cited evidence")
	}
}

// TestTheVerifierBriefCarriesOnlyClaimsAndCitedEvidence.
func TestTheVerifierBriefCarriesOnlyClaimsAndCitedEvidence(t *testing.T) {
	t.Parallel()

	cited := versionEvidence()
	uncited := versionEvidence()
	uncited.ID = "e-9"

	brief, err := verify.Brief(
		[]verify.Claim{{Ref: "verdict", Kind: verify.KindVerdict,
			Text: "roll back rev7", EvidenceIDs: []string{"e-4"}}},
		map[string]verify.Evidence{"e-4": cited, "e-9": uncited},
	)
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	if !strings.Contains(brief, "e-4") {
		t.Error("the brief does not carry the cited evidence")
	}
	if strings.Contains(brief, "e-9") {
		t.Error("the brief carries evidence no claim cited; the verifier could support a claim from it")
	}
	if !strings.Contains(brief, "EXCLUDED from citation resolution") {
		t.Error("the brief does not mark the free-text field excluded")
	}
}

// TestAVerifierOnTheSameModelIsRefused (FR-022a).
func TestAVerifierOnTheSameModelIsRefused(t *testing.T) {
	t.Parallel()

	config, prices, err := model.LoadPair("../../../config/model.yaml", "../../../config/prices.yaml")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	same := config.Roles[model.RoleInvestigator]
	config.Roles[model.RoleVerifier] = same

	transport, err := modeltest.Transport(config.Roles[model.RoleVerifier].Model, modeltest.Turn{Text: "{}"})
	if err != nil {
		t.Fatalf("canned: %v", err)
	}
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := verify.NewVerifier(client); err == nil {
		t.Fatal("a verifier on the investigator's own model was accepted")
	}
}

func findingFor(t *testing.T, findings []verify.Finding, ref string) verify.Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.ClaimRef == ref {
			return finding
		}
	}
	t.Fatalf("no finding for %s in %+v", ref, findings)
	return verify.Finding{}
}
