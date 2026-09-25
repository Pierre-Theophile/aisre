// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// The probable rules (research §10, FR-037, ADR-0001 D6).
//
// A probable rule's false positives are cheap — somebody reads a suggestion and says no — so the
// tests here are less about what P1 refuses to match than about two things a reviewer has to be
// able to check by reading: that the normalization is exactly what the documentation says, and
// that nothing a probable rule produces is ever marked certain.

func k8sWorkloadClaim(entityID, value string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-k8s-" + value, EntityID: entityID, EntityType: graph.NodeTypeWorkload,
		SourceID: "k8s:shop", EventID: "k8s:shop:claim:" + value, AppendedSeq: 10,
		Namespace: resolution.NamespaceK8sDeployment, Value: value,
		Attributes: map[string]string{resolution.AttrK8sNamespace: "shop"},
	}
}

func otelServiceClaim(entityID, value string, attrs map[string]string) resolution.Claim {
	if attrs == nil {
		attrs = map[string]string{}
	}
	attrs[resolution.AttrServiceNamespace] = "shop"
	return resolution.Claim{
		ClaimID: "claim-otel-" + value, EntityID: entityID, EntityType: graph.NodeTypeService,
		SourceID: "otel:shop", EventID: "otel:shop:claim:" + value, AppendedSeq: 20,
		Namespace: resolution.NamespaceOTelService, Value: value, Attributes: attrs,
	}
}

func TestNormalizeNameTable(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// The suffixes the default list strips.
		{"checkout-svc", "checkout"},
		{"checkout-service", "checkout"},
		{"checkout-deploy", "checkout"},
		{"checkout-deployment", "checkout"},
		{"checkout-app", "checkout"},
		// Stripping repeats, so a doubled suffix reduces all the way.
		{"checkout-svc-deploy", "checkout"},
		// A Kubernetes identifier is `<namespace>/<name>`; only the name is the name.
		{"shop/checkout-svc", "checkout"},
		{"ops/notifications-svc", "notifications"},
		// Case folding and separator unification.
		{"Checkout_SVC", "checkout"},
		{"check-out", "checkout"},
		{"checkout.svc", "checkout"},
		{"  checkout  ", "checkout"},
		// A name that *is* a suffix is left alone: `svc` is a service called svc.
		{"svc", "svc"},
		{"app", "app"},
		// Digits survive; a version suffix is not in the strip list and so distinguishes names.
		{"checkout-v2", "checkoutv2"},
		{"checkout2", "checkout2"},
		// Nothing normalizes to nothing.
		{"", ""},
		{"---", ""},
	}
	for _, tc := range cases {
		if got := resolution.NormalizeName(tc.in, resolution.DefaultNameSuffixes); got != tc.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestP1MatchesNormalizedNamesAcrossSources(t *testing.T) {
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	service := otelServiceClaim("entity-service", "checkout", nil)
	store := fakeStore{claims: []resolution.Claim{workload, service}}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(matches), matches)
	}
	match := matches[0]
	if match.RuleID != "P1" {
		t.Errorf("rule = %q, want P1", match.RuleID)
	}
	if match.Certain {
		t.Error("a probable match is marked certain; nothing may merge on it (SC-007)")
	}
	if match.Score != resolution.ScoreP1 {
		t.Errorf("score = %v, want %v", match.Score, resolution.ScoreP1)
	}
	if match.PairKey() != "entity-service|entity-workload" {
		t.Errorf("pair key = %q", match.PairKey())
	}
}

func TestP1DoesNotMatchTwoObservedServices(t *testing.T) {
	// Two telemetry service names that resemble each other are two services with similar names,
	// which is the normal state of a system. The rules compare a Kubernetes name with an
	// observed one.
	a := otelServiceClaim("entity-a", "checkout", nil)
	b := otelServiceClaim("entity-b", "checkout-svc", nil)
	store := fakeStore{claims: []resolution.Claim{a, b}}

	matches, err := resolution.Evaluate(context.Background(), store, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matches = %+v, want none", matches)
	}
}

func TestP2AddsEnvironmentAgreement(t *testing.T) {
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	workload.Attributes[resolution.AttrEnvironment] = "prod"
	service := otelServiceClaim("entity-service", "checkout",
		map[string]string{resolution.AttrEnvironment: "prod"})
	store := fakeStore{claims: []resolution.Claim{workload, service}}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(matches), matches)
	}
	if matches[0].RuleID != "P2" || matches[0].Score != resolution.ScoreP2 {
		t.Errorf("rule = %q score = %v, want P2 at %v", matches[0].RuleID, matches[0].Score, resolution.ScoreP2)
	}
}

func TestP2NeedsBothEnvironments(t *testing.T) {
	// One side stating an environment is not agreement. The pair still matches P1, at 0.70.
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	service := otelServiceClaim("entity-service", "checkout",
		map[string]string{resolution.AttrEnvironment: "prod"})
	store := fakeStore{claims: []resolution.Claim{workload, service}}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].RuleID != "P1" {
		t.Fatalf("matches = %+v, want one P1 match", matches)
	}
}

func TestP2DoesNotMatchAcrossEnvironments(t *testing.T) {
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	workload.Attributes[resolution.AttrEnvironment] = "staging"
	service := otelServiceClaim("entity-service", "checkout",
		map[string]string{resolution.AttrEnvironment: "prod"})
	store := fakeStore{claims: []resolution.Claim{workload, service}}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].RuleID != "P1" {
		t.Fatalf("matches = %+v, want one P1 match: a disagreeing environment is not P2 evidence", matches)
	}
}

func TestP3AddsSharedOwner(t *testing.T) {
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	service := otelServiceClaim("entity-service", "checkout", nil)
	store := fakeStore{
		claims: []resolution.Claim{workload, service},
		owners: map[string]string{"entity-workload": "team-checkout", "entity-service": "team-checkout"},
	}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(matches), matches)
	}
	if matches[0].RuleID != "P3" || matches[0].Score != resolution.ScoreP3 {
		t.Errorf("rule = %q score = %v, want P3 at %v", matches[0].RuleID, matches[0].Score, resolution.ScoreP3)
	}
}

func TestP2OutranksP3OnTheSamePair(t *testing.T) {
	// Both corroborations hold; the more specific rule is the one recorded, so a decision is
	// explained by the strongest evidence rather than by whichever rule ran first.
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	workload.Attributes[resolution.AttrEnvironment] = "prod"
	service := otelServiceClaim("entity-service", "checkout",
		map[string]string{resolution.AttrEnvironment: "prod"})
	store := fakeStore{
		claims: []resolution.Claim{workload, service},
		owners: map[string]string{"entity-workload": "team-checkout", "entity-service": "team-checkout"},
	}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].RuleID != "P2" {
		t.Fatalf("matches = %+v, want one P2 match", matches)
	}
}

func TestP3NeedsTheSameOwner(t *testing.T) {
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	service := otelServiceClaim("entity-service", "checkout", nil)
	store := fakeStore{
		claims: []resolution.Claim{workload, service},
		owners: map[string]string{"entity-workload": "team-checkout", "entity-service": "team-payments"},
	}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].RuleID != "P1" {
		t.Fatalf("matches = %+v, want one P1 match", matches)
	}
}

func TestProbableTypeGuardRefusesUnrelatedKinds(t *testing.T) {
	// research §10: two entities whose asserted types are both below WORKLOAD and differ are
	// never matched by a probable rule. A config map and an owner that share a name are a
	// coincidence, not evidence.
	workload := k8sWorkloadClaim("entity-config", "shop/checkout-svc")
	workload.EntityType = graph.NodeTypeConfig
	service := otelServiceClaim("entity-owner", "checkout", nil)
	service.EntityType = graph.NodeTypeOwner
	store := fakeStore{claims: []resolution.Claim{workload, service}}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matches = %+v, want none: the type guard should have refused the pair", matches)
	}
}

func TestProbableTypeGuardAllowsWorkloadAndService(t *testing.T) {
	// The case the graph exists to merge (ADR-0001 D7) must not be caught by the guard.
	workload := k8sWorkloadClaim("entity-workload", "shop/checkout-svc")
	service := otelServiceClaim("entity-service", "checkout", nil)
	store := fakeStore{claims: []resolution.Claim{workload, service}}

	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v, want one", matches)
	}
}

func TestCertainMatchSuppressesTheProbableOne(t *testing.T) {
	// C2 and P1 both fire on this pair. Only the certain one is reported: the pair is about to
	// be merged, and a suggestion to merge what is already merged is noise.
	declared := resolution.Claim{
		ClaimID: "claim-k8s-declared", EntityID: "entity-workload", EntityType: graph.NodeTypeWorkload,
		SourceID: "k8s:shop", EventID: "k8s:shop:claim", AppendedSeq: 10,
		Namespace: resolution.NamespaceOTelService, Value: "checkout",
		Attributes: map[string]string{
			resolution.AttrClaimKey:     "app.kubernetes.io/name",
			resolution.AttrK8sNamespace: "shop",
			resolution.AttrEnvironment:  "prod",
		},
	}
	observed := otelServiceClaim("entity-service", "checkout",
		map[string]string{resolution.AttrEnvironment: "prod"})
	store := fakeStore{claims: []resolution.Claim{declared, observed}}

	matches, err := resolution.Evaluate(context.Background(), store, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(matches), matches)
	}
	if !matches[0].Certain || matches[0].RuleID != "C2" {
		t.Errorf("match = %s certain=%v, want a certain C2", matches[0].RuleID, matches[0].Certain)
	}
}

func TestProbableRulesArePublishedAsProbable(t *testing.T) {
	// The closed set. A probable rule that appears without being listed here is a rule nobody
	// reviewed the score of, which is what this test is for — so feature 003's P4, P5 and P6 are
	// added rather than the check being loosened.
	want := map[string]float64{
		"P1": resolution.ScoreP1, "P2": resolution.ScoreP2, "P3": resolution.ScoreP3,
		"P4": resolution.ScoreP4, "P5": resolution.ScoreP5, "P6": resolution.ScoreP6,
		"P7": resolution.ScoreP7,
	}
	seen := map[string]bool{}
	for _, rule := range resolution.ProbableRules() {
		if rule.Certain {
			t.Errorf("rule %s is in ProbableRules but declares itself certain", rule.ID)
		}
		score, ok := want[rule.ID]
		if !ok {
			t.Errorf("unexpected probable rule %s", rule.ID)
			continue
		}
		if rule.Score != score {
			t.Errorf("rule %s score = %v, want %v (research §10)", rule.ID, rule.Score, score)
		}
		if rule.Description == "" {
			t.Errorf("rule %s has no published description (FR-037)", rule.ID)
		}
		seen[rule.ID] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("rule %s is not registered", id)
		}
	}
}
