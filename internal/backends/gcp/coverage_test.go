// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"errors"
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	gcpbackend "github.com/Pierre-Theophile/aisre/internal/backends/gcp"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The live digest boundary's own tests (T042, FR-110).

func liveSanitiser(t *testing.T) *sanitise.Sanitiser {
	t.Helper()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 5)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	return s
}

func liveRedactor(t *testing.T, version string) *backend.Redactor {
	t.Helper()
	policy := backend.DefaultRedactionPolicy()
	policy.PolicyVersion = version
	r, err := backend.NewRedactor(policy, []byte("a corpus key for the digest side"))
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	return r
}

func logResponse(templates ...string) *backend.Response {
	patterns := make([]*investigationv1.LogPattern, 0, len(templates))
	for _, template := range templates {
		patterns = append(patterns, &investigationv1.LogPattern{
			Template: template,
			JoinKeys: &investigationv1.JoinKeys{Workload: "storefront"},
			Count:    3,
		})
	}
	return &investigationv1.AlgebraResponse{
		Digest: &investigationv1.Digest{
			Body: &investigationv1.Digest_Log{
				Log: &investigationv1.LogDigest{Patterns: patterns},
			},
		},
	}
}

// A live answer is reduced to masked templates before it leaves the process: the asymmetry FR-110
// closes is that a live investigation must not surface what a recording could not keep.
func TestALiveLogDigestIsMaskedBeforeItLeavesTheProcess(t *testing.T) {
	resp := logResponse("payment failed for 10.4.2.19 after 1200ms")
	err := gcpbackend.SanitiseThenRedact(liveSanitiser(t), liveRedactor(t, sanitise.PolicyVersion), resp)
	if err != nil {
		t.Fatalf("SanitiseThenRedact: %v", err)
	}
	patterns := resp.GetDigest().GetLog().GetPatterns()
	if len(patterns) != 1 {
		t.Fatalf("the digest holds %d patterns, want 1", len(patterns))
	}
	if strings.Contains(patterns[0].GetTemplate(), "10.4.2.19") {
		t.Fatalf("a live template still carries an address: %q", patterns[0].GetTemplate())
	}
	if patterns[0].GetTemplate() != backend.MaskLine("payment failed for 10.4.2.19 after 1200ms") {
		t.Fatalf("the live template is not feature 002's masking: %q", patterns[0].GetTemplate())
	}
	// And the join key was pseudonymised by 002's redactor, so the answer still joins.
	if got := patterns[0].GetJoinKeys().GetWorkload(); !strings.HasPrefix(got, backend.PseudonymPrefix) {
		t.Fatalf("the join key left the process in the clear as %q", got)
	}
}

// A live log line naming a person by handle is dropped from the answer, not returned.
func TestALivePatternNamingAPersonIsDroppedFromTheAnswer(t *testing.T) {
	resp := logResponse("rolled back by @jane-doe", "connection reset by peer")
	err := gcpbackend.SanitiseThenRedact(liveSanitiser(t), liveRedactor(t, sanitise.PolicyVersion), resp)
	if err != nil {
		t.Fatalf("SanitiseThenRedact: %v", err)
	}
	patterns := resp.GetDigest().GetLog().GetPatterns()
	if len(patterns) != 1 {
		t.Fatalf("the digest holds %d patterns, want 1 (the other named a person)", len(patterns))
	}
	if strings.Contains(patterns[0].GetTemplate(), "jane") {
		t.Fatalf("the surviving pattern is the one naming a person: %q", patterns[0].GetTemplate())
	}
}

// A canary in a live log template is refused. This is the assertion that separates this boundary from
// feature 002's redactor, which masks the same template and knows nothing about canaries — so a probe
// that removes the sanitisation loop fails here even though the answer would still be masked.
func TestACanaryInALiveLogTemplateIsRefused(t *testing.T) {
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 5)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	set := sanitise.NewCanarySet(key)
	seeded, err := set.Seed("a source", sanitise.CanaryFreeText)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	resp := logResponse("request failed for " + seeded[0].Token)
	err = gcpbackend.SanitiseThenRedact(s.WithCanaries(set), liveRedactor(t, sanitise.PolicyVersion), resp)
	if err == nil {
		t.Fatal("a live log template carrying a canary was returned")
	}
	var survived *sanitise.CanarySurvivedError
	if !errors.As(err, &survived) {
		t.Fatalf("the refusal is %T, not *CanarySurvivedError: %v", err, err)
	}
}

// Two policies that disagree about a version produce a corpus whose joins work on half the fixtures,
// so the mismatch is refused rather than resolved.
func TestADeclaredPolicyVersionThatDisagreesWithTheContractIsRefused(t *testing.T) {
	err := gcpbackend.SanitiseThenRedact(liveSanitiser(t), liveRedactor(t, "0.9.0"), logResponse("anything"))
	if err == nil {
		t.Fatal("a redactor declaring a different policy version was accepted")
	}
	if !strings.Contains(err.Error(), "0.9.0") || !strings.Contains(err.Error(), sanitise.PolicyVersion) {
		t.Fatalf("the refusal does not name both versions: %q", err.Error())
	}
}

// Neither half of the boundary is optional.
func TestTheLiveBoundaryRefusesAMissingSanitiserOrRedactor(t *testing.T) {
	if err := gcpbackend.SanitiseThenRedact(nil, liveRedactor(t, sanitise.PolicyVersion), logResponse("x")); err == nil {
		t.Fatal("a live answer was finished with no sanitiser (FR-110)")
	}
	if err := gcpbackend.SanitiseThenRedact(liveSanitiser(t), nil, logResponse("x")); err == nil {
		t.Fatal("a live answer was finished with no redactor")
	}
}

// The declaration is derived from the table, so it cannot drift from the dispositions applied.
func TestTheDeclaredRedactionPolicyIsDerivedFromTheTable(t *testing.T) {
	policy := sanitise.ContractPolicy()
	declared := gcpbackend.DeclaredRedactionPolicy(policy)

	if declared.GetPolicyVersion() != policy.Version() {
		t.Fatalf("the declaration says %q and the table is %q", declared.GetPolicyVersion(), policy.Version())
	}
	if !declared.GetLogBodiesAsTemplates() {
		t.Fatal("the declaration does not claim log bodies are templates, so a log digest would be rejected")
	}
	dropped := map[string]bool{}
	for _, field := range declared.GetDroppedFields() {
		dropped[field] = true
	}
	for _, field := range policy.Fields(sanitise.Dropped) {
		if !dropped[field] {
			t.Errorf("the table drops %s and the declaration does not name it", field)
		}
	}
	if !dropped[backend.FieldPeopleIdentifiers] {
		t.Error("the declaration does not name people_identifiers, which is what makes a series tag " +
			"naming a person a rejection rather than a redaction")
	}
	pseudonymised := map[string]bool{}
	for _, field := range declared.GetPseudonymisedFields() {
		pseudonymised[field] = true
	}
	for _, field := range policy.Fields(sanitise.Pseudonym) {
		if !pseudonymised[field] {
			t.Errorf("the table pseudonymises %s and the declaration does not name it", field)
		}
	}
	// The join keys 002 pseudonymises must be declared too, or a live digest would leave them clear.
	for _, field := range []string{backend.FieldJoinKeyWorkload, backend.FieldJoinKeyPodOrHost} {
		if !pseudonymised[field] {
			t.Errorf("the declaration does not name %s, so a live join key would leave in the clear", field)
		}
	}
}
