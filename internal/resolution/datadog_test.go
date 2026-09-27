// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// C9: a Datadog log service is an OpenTelemetry service, in the same environment (005 T015–T016).
// The Datadog side is a correlation key and the OpenTelemetry side an identity claim; the rule runs
// from both.

func datadogLogServiceKey(entityID, source, name, environment string) resolution.Correlation {
	attrs := map[string]string{}
	if environment != "" {
		attrs[resolution.AttrEnvironment] = environment
	}
	return resolution.Correlation{
		CorrelationID: "corr-dd-" + source + "-" + entityID, EntityID: entityID,
		EntityType: graph.NodeTypeService, SourceID: source, EventID: source + ":corr:" + name,
		Namespace: resolution.NamespaceDatadogLogService, Value: name, Attributes: attrs,
	}
}

// evalBothWays evaluates the pair from each side: the correlation arriving second, and the claim.
func evalBothWays(t *testing.T, key resolution.Correlation, claim resolution.Claim) (fromKey, fromClaim bool) {
	t.Helper()
	store := fakeStore{claims: []resolution.Claim{claim}, correlations: []resolution.Correlation{key}}
	mk, err := resolution.EvaluateCorrelation(context.Background(), store, key)
	if err != nil {
		t.Fatalf("EvaluateCorrelation: %v", err)
	}
	mc, err := resolution.Evaluate(context.Background(), store, claim)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return hasRule(mk, "C9"), hasRule(mc, "C9")
}

// The merge fires from either side, so whichever arrives second finds the first.
func TestC9MergesALogServiceWithAnOTelServiceInTheSameEnvironment(t *testing.T) {
	key := datadogLogServiceKey("entity-dd", "datadog:org", "checkout", "production")
	for name, claim := range map[string]resolution.Claim{
		"an observed name":          observedServiceNameClaim("entity-otel", "checkout", "production"),
		"a Cloud Run declared name": gcpDeclaredServiceNameClaim("entity-cloudrun", "checkout", "production"),
	} {
		fromKey, fromClaim := evalBothWays(t, key, claim)
		if !fromKey || !fromClaim {
			t.Errorf("%s: C9 fired from the correlation side %v and from the claim side %v; it must fire "+
				"from both, or the graph depends on arrival order", name, fromKey, fromClaim)
		}
	}
}

// The environment is the whole rule: missing on either side, or disagreeing, never merges.
func TestC9RefusesAMissingOrDisagreeingEnvironment(t *testing.T) {
	for _, tc := range []struct{ name, logEnv, otelEnv string }{
		{"neither side states one", "", ""},
		{"only the log side states one", "production", ""},
		{"only the OTel side states one", "", "production"},
		{"the two disagree", "staging", "production"},
	} {
		key := datadogLogServiceKey("entity-dd", "datadog:org", "checkout", tc.logEnv)
		claim := observedServiceNameClaim("entity-otel", "checkout", tc.otelEnv)
		if a, b := evalBothWays(t, key, claim); a || b {
			t.Errorf("C9 fired with %s; a certain rule may not gamble on staging being production", tc.name)
		}
	}
}

// Names compare exactly: a one-sided case-insensitive match would depend on arrival order.
func TestC9ComparesNamesExactly(t *testing.T) {
	key := datadogLogServiceKey("entity-dd", "datadog:org", "checkout", "production")
	claim := observedServiceNameClaim("entity-otel", "Checkout", "production")
	if a, b := evalBothWays(t, key, claim); a || b {
		t.Error("C9 merged `checkout` with `Checkout`")
	}
}

// Stated Kubernetes placements that disagree keep the two apart.
func TestC9RequiresAgreeingKubernetesPlacement(t *testing.T) {
	key := datadogLogServiceKey("entity-dd", "datadog:org", "checkout", "production")
	key.Attributes[resolution.AttrK8sCluster] = "eu-1"
	claim := observedServiceNameClaim("entity-otel", "checkout", "production")
	claim.Attributes[resolution.AttrK8sCluster] = "us-1"
	if a, b := evalBothWays(t, key, claim); a || b {
		t.Error("C9 merged across two clusters")
	}
	claim.Attributes[resolution.AttrK8sCluster] = "eu-1"
	if a, b := evalBothWays(t, key, claim); !a || !b {
		t.Error("C9 did not merge with agreeing clusters")
	}
}

// Two organisations' log services with one name are two services (FR-062): no rule merges two
// correlation keys, and the namespace is not identifying.
func TestTwoOrganisationsSameLogServiceNeverMerge(t *testing.T) {
	a := datadogLogServiceKey("entity-org-a", "datadog:org-a", "checkout", "production")
	b := datadogLogServiceKey("entity-org-b", "datadog:org-b", "checkout", "production")
	store := fakeStore{correlations: []resolution.Correlation{a, b}}
	for _, key := range []resolution.Correlation{a, b} {
		matches, err := resolution.EvaluateCorrelation(context.Background(), store, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Errorf("two organisations' log services merged: %+v", matches)
		}
	}
	if resolution.IdentifyingNamespace(resolution.NamespaceDatadogLogService) {
		t.Error("datadog.log_service is identifying, so C1 would merge on the name alone")
	}
}

// One source stating both sides is one opinion, not corroboration.
func TestC9NeedsTwoSources(t *testing.T) {
	key := datadogLogServiceKey("entity-dd", "otel:nova", "checkout", "production")
	claim := observedServiceNameClaim("entity-otel", "checkout", "production")
	if a, b := evalBothWays(t, key, claim); a || b {
		t.Error("C9 merged two observations from one source")
	}
}
