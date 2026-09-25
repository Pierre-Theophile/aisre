// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// The certain rules, tested away from the database (FR-037, research §10).
//
// Each rule is checked for what it fires on *and* for what it refuses to fire on: a certain
// rule merges without a human, so its false positives are the expensive kind (constitution VI).

// fakeStore is a ClaimStore over a fixed slice of claims.
type fakeStore struct {
	claims []resolution.Claim
	// correlations are the stored correlation keys: a value SEVERAL entities may carry, which is why
	// they are a separate slice and not more claims (004 T148).
	correlations []resolution.Correlation
	// owners maps an entity id to the owner entity it has an `owned_by` edge to, which is one of
	// the two facts about the graph — as opposed to about claims — that a rule reads (P3).
	owners map[string]string
	// targets maps a change entity to the entities it was applied to, the other one (C8).
	targets map[string][]string
}

func (s fakeStore) ClaimsFor(_ context.Context, namespace, value string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s.claims {
		if claim.Namespace == namespace && claim.Value == value {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s fakeStore) ClaimsMatchingAttributes(_ context.Context, namespace string, attrs map[string]string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s.claims {
		if claim.Namespace != namespace {
			continue
		}
		match := true
		for key, want := range attrs {
			if claim.Attr(key) != want {
				match = false
				break
			}
		}
		if match {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s fakeStore) ClaimsInNamespaces(_ context.Context, namespaces []string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s.claims {
		if slices.Contains(namespaces, claim.Namespace) {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s fakeStore) SharedOwner(_ context.Context, a, b string) (bool, error) {
	owner, ok := s.owners[a]
	return ok && owner != "" && owner == s.owners[b], nil
}

func (s fakeStore) ChangeTargets(_ context.Context, changeEntityID string) ([]string, error) {
	return s.targets[changeEntityID], nil
}

// CorrelatedWith returns EVERY entity carrying the value, which is the whole difference from ClaimsFor
// and the reason this double does not just filter s.claims: a correlation lookup that returned one
// entity per source would make the monorepo case unreachable in a test.
func (s fakeStore) CorrelatedWith(_ context.Context, namespace, value string) ([]resolution.Correlation, error) {
	var out []resolution.Correlation
	for _, key := range s.correlations {
		if key.Namespace == namespace && key.Value == value {
			out = append(out, key)
		}
	}
	return out, nil
}

// k8sDeclaredClaim is the claim a Kubernetes feeder emits for a workload whose annotation
// declares its OpenTelemetry service name.
func k8sDeclaredClaim(entityID string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-k8s", EntityID: entityID, SourceID: "k8s:demo", EventID: "k8s:demo:claim",
		Namespace: resolution.NamespaceOTelService, Value: "checkout", AppendedSeq: 30,
		Attributes: map[string]string{
			resolution.AttrClaimKey:     "resource.opentelemetry.io/service.name",
			resolution.AttrK8sNamespace: "shop",
			resolution.AttrEnvironment:  "prod",
		},
	}
}

// otelObservedClaim is the claim the topology feeder emits for the service it observed.
func otelObservedClaim(entityID string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-otel", EntityID: entityID, SourceID: "otel:demo", EventID: "otel:demo:claim",
		Namespace: resolution.NamespaceOTelService, Value: "checkout", AppendedSeq: 57,
		Attributes: map[string]string{
			resolution.AttrServiceNamespace: "shop",
			resolution.AttrEnvironment:      "prod",
			resolution.AttrK8sDeployment:    "checkout",
			resolution.AttrK8sNamespace:     "shop",
		},
	}
}

func TestCertainRulesMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	k8sEntity, otelEntity := "entity-k8s", "entity-otel"

	tests := []struct {
		name     string
		claims   []resolution.Claim
		trigger  resolution.Claim
		wantRule string
		wantPair bool
	}{
		{
			name: "C2 wins over C1 and C3 on the workload/service pair",
			// All three rules are true of this pair. C2 is recorded, because "an operator
			// wrote this service name on this workload" is the fact a human can check.
			claims:   []resolution.Claim{k8sDeclaredClaim(k8sEntity), otelObservedClaim(otelEntity)},
			trigger:  otelObservedClaim(otelEntity),
			wantRule: "C2",
			wantPair: true,
		},
		{
			name: "C1 on a plain identifier two sources share",
			claims: []resolution.Claim{
				{ClaimID: "a", EntityID: k8sEntity, SourceID: "k8s:demo", Namespace: "server.address", Value: "redis.internal"},
				{ClaimID: "b", EntityID: otelEntity, SourceID: "otel:demo", Namespace: "server.address", Value: "redis.internal"},
			},
			trigger:  resolution.Claim{ClaimID: "b", EntityID: otelEntity, SourceID: "otel:demo", Namespace: "server.address", Value: "redis.internal"},
			wantRule: "C1",
			wantPair: true,
		},
		{
			name: "C3 when the workload declares nothing but the spans name it",
			claims: []resolution.Claim{
				{ClaimID: "w", EntityID: k8sEntity, SourceID: "k8s:demo",
					Namespace: resolution.NamespaceK8sDeployment, Value: "shop/checkout"},
				{ClaimID: "s", EntityID: otelEntity, SourceID: "otel:demo",
					Namespace: resolution.NamespaceOTelService, Value: "checkout",
					Attributes: map[string]string{
						resolution.AttrK8sDeployment: "checkout",
						resolution.AttrK8sNamespace:  "shop",
					}},
			},
			trigger: resolution.Claim{ClaimID: "s", EntityID: otelEntity, SourceID: "otel:demo",
				Namespace: resolution.NamespaceOTelService, Value: "checkout",
				Attributes: map[string]string{
					resolution.AttrK8sDeployment: "checkout",
					resolution.AttrK8sNamespace:  "shop",
				}},
			wantRule: "C3",
			wantPair: true,
		},
		{
			name: "no match when the environments differ",
			// `checkout` in staging is not `checkout` in production. A certain rule may not
			// gamble on that (FR-007).
			claims: []resolution.Claim{
				k8sDeclaredClaim(k8sEntity),
				func() resolution.Claim {
					claim := otelObservedClaim(otelEntity)
					claim.Attributes[resolution.AttrEnvironment] = "staging"
					delete(claim.Attributes, resolution.AttrK8sDeployment)
					return claim
				}(),
			},
			trigger: func() resolution.Claim {
				claim := otelObservedClaim(otelEntity)
				claim.Attributes[resolution.AttrEnvironment] = "staging"
				delete(claim.Attributes, resolution.AttrK8sDeployment)
				return claim
			}(),
			// C1 still fires: the same identifier from two sources is the same identifier.
			wantRule: "C1",
			wantPair: true,
		},
		{
			name:     "one source claiming an identifier twice is not corroboration",
			claims:   []resolution.Claim{k8sDeclaredClaim(k8sEntity)},
			trigger:  k8sDeclaredClaim(k8sEntity),
			wantPair: false,
		},
		{
			name: "claims already on one entity have nothing to merge",
			claims: []resolution.Claim{
				k8sDeclaredClaim(k8sEntity),
				func() resolution.Claim { claim := otelObservedClaim(k8sEntity); return claim }(),
			},
			trigger:  otelObservedClaim(k8sEntity),
			wantPair: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			matches, err := resolution.Evaluate(ctx, fakeStore{claims: tc.claims}, tc.trigger)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !tc.wantPair {
				if len(matches) != 0 {
					t.Fatalf("Evaluate returned %d matches, want none: %+v", len(matches), matches)
				}
				return
			}
			if len(matches) != 1 {
				t.Fatalf("Evaluate returned %d matches, want exactly one per pair: %+v", len(matches), matches)
			}
			match := matches[0]
			if match.RuleID != tc.wantRule {
				t.Errorf("rule = %s, want %s (rationale: %s)", match.RuleID, tc.wantRule, match.Rationale)
			}
			if !match.Certain {
				t.Error("a rule reached through Evaluate must be certain")
			}
			if match.Score != 1 {
				t.Errorf("score = %v, want 1 for a certain rule", match.Score)
			}
			if match.Rationale == "" {
				t.Error("every merge must carry a human-readable rationale (FR-038)")
			}
			if len(match.SupportingClaimIDs) != 2 {
				t.Errorf("supporting claims = %v, want the two claims the rule stood on (FR-038)",
					match.SupportingClaimIDs)
			}
			if !slices.IsSorted(match.SupportingClaimIDs) {
				t.Errorf("supporting claims %v are not sorted; a replay must record them identically",
					match.SupportingClaimIDs)
			}
		})
	}
}

// TestRegistryIsPublished checks the shape US6 extends: every rule carries an id, a certainty
// flag and a description, and only certain rules are reachable from Evaluate (FR-037).
func TestRegistryIsPublished(t *testing.T) {
	t.Parallel()

	rules := resolution.Rules()
	if len(rules) < 3 {
		t.Fatalf("registry holds %d rules, want at least C1, C2 and C3", len(rules))
	}
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule.ID == "" || rule.Description == "" {
			t.Errorf("rule %+v must have an id and a published description", rule)
		}
		ids = append(ids, rule.ID)
	}
	if !slices.IsSorted(ids) {
		t.Errorf("Rules() returned %v, want them in id order", ids)
	}

	certain := resolution.CertainRules()
	for _, rule := range certain {
		if !rule.Certain {
			t.Errorf("CertainRules returned probable rule %s", rule.ID)
		}
	}
	// Evaluation order is most specific first, so the rule recorded on a pair several rules
	// agree about is the one that explains it best.
	if len(certain) < 3 || certain[0].ID != "C2" {
		t.Errorf("certain rules evaluate in order %v, want the most specific (C2) first", ruleIDs(certain))
	}
}

func ruleIDs(rules []resolution.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.ID)
	}
	return out
}

// The published page and the registry are one list (004; the same both-directions habit as the
// connector pages).
//
// docs/schema/resolution.md §2 is what a reader of this system's guarantees reads, and until now
// nothing compared it to the registry — so 003's C4, C5, C6, C7, P4, P5 and P6 were published in code
// and absent from the page for a whole feature. A page that can drift is a page that has.
func TestThePublishedRulesPageAndTheRegistryAreTheSameList(t *testing.T) {
	t.Parallel()

	page := publishedRuleIDs(t, "../../docs/schema/resolution.md")
	if len(page) == 0 {
		t.Fatal("no rule id was parsed out of docs/schema/resolution.md §2; a comparison against " +
			"nothing is not a comparison")
	}
	registry := map[string]bool{}
	for _, rule := range resolution.Rules() {
		registry[rule.ID] = true
	}
	for id := range page {
		if !registry[id] {
			t.Errorf("docs/schema/resolution.md publishes rule %s, which the registry does not carry", id)
		}
	}
	for id := range registry {
		if !page[id] {
			t.Errorf("the registry carries rule %s, which docs/schema/resolution.md does not publish: "+
				"a rule that can merge entities without appearing on the page a reader checks", id)
		}
	}
}

// publishedRuleIDs reads the bolded ids out of §2's two tables. The parse is narrow — a table row
// whose first cell is `**Cn**` or `**Pn**` — so a rule mentioned in the prose is not mistaken for a
// published one.
func publishedRuleIDs(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("open the published page: %v", err)
	}
	out := map[string]bool{}
	var inSection bool
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = strings.Contains(line, "The published rules")
			continue
		}
		if !inSection || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		id := strings.Trim(strings.TrimSpace(cells[0]), "*")
		if len(id) >= 2 && (id[0] == 'C' || id[0] == 'P') && id[1] >= '0' && id[1] <= '9' {
			out[id] = true
		}
	}
	return out
}
