// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// C9: a Datadog log service is an OpenTelemetry service, in the same environment (005 T015–T016).

func datadogLogServiceClaim(entityID, source, name, environment string) resolution.Claim {
	attrs := map[string]string{}
	if environment != "" {
		attrs[resolution.AttrEnvironment] = environment
	}
	return resolution.Claim{
		ClaimID: "claim-dd-" + source + "-" + name + "-" + environment, EntityID: entityID,
		EntityType: graph.NodeTypeService, SourceID: source,
		EventID: source + ":claim:" + name, AppendedSeq: 50,
		Namespace: resolution.NamespaceDatadogLogService, Value: name, Attributes: attrs,
	}
}

func evalBothWays(t *testing.T, a, b resolution.Claim) (fromA, fromB bool) {
	t.Helper()
	store := fakeStore{claims: []resolution.Claim{a, b}}
	ma, err := resolution.Evaluate(context.Background(), store, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	mb, err := resolution.Evaluate(context.Background(), store, b)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return hasRule(ma, "C9"), hasRule(mb, "C9")
}

// The merge fires from either side, so whichever claim arrives second finds the first.
func TestC9MergesALogServiceWithAnOTelServiceInTheSameEnvironment(t *testing.T) {
	logs := datadogLogServiceClaim("entity-dd", "datadog:org", "checkout", "production")
	for name, otel := range map[string]resolution.Claim{
		"an observed name":          observedServiceNameClaim("entity-otel", "checkout", "production"),
		"a Cloud Run declared name": gcpDeclaredServiceNameClaim("entity-cloudrun", "checkout", "production"),
	} {
		fromLogs, fromOTel := evalBothWays(t, logs, otel)
		if !fromLogs || !fromOTel {
			t.Errorf("%s: C9 fired from the log side %v and from the OTel side %v; it must fire from both", name, fromLogs, fromOTel)
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
		logs := datadogLogServiceClaim("entity-dd", "datadog:org", "checkout", tc.logEnv)
		otel := observedServiceNameClaim("entity-otel", "checkout", tc.otelEnv)
		if a, b := evalBothWays(t, logs, otel); a || b {
			t.Errorf("C9 fired with %s; a certain rule may not gamble on staging being production", tc.name)
		}
	}
}

// Names compare exactly: a one-sided case-insensitive match would depend on arrival order.
func TestC9ComparesNamesExactly(t *testing.T) {
	logs := datadogLogServiceClaim("entity-dd", "datadog:org", "checkout", "production")
	otel := observedServiceNameClaim("entity-otel", "Checkout", "production")
	if a, b := evalBothWays(t, logs, otel); a || b {
		t.Error("C9 merged `checkout` with `Checkout`")
	}
}

// Stated Kubernetes namespaces or clusters that disagree keep the two apart.
func TestC9RequiresAgreeingKubernetesPlacement(t *testing.T) {
	logs := datadogLogServiceClaim("entity-dd", "datadog:org", "checkout", "production")
	logs.Attributes[resolution.AttrK8sCluster] = "eu-1"
	otel := observedServiceNameClaim("entity-otel", "checkout", "production")
	otel.Attributes[resolution.AttrK8sCluster] = "us-1"
	if a, b := evalBothWays(t, logs, otel); a || b {
		t.Error("C9 merged across two clusters")
	}
	otel.Attributes[resolution.AttrK8sCluster] = "eu-1"
	if a, b := evalBothWays(t, logs, otel); !a || !b {
		t.Error("C9 did not merge with agreeing clusters")
	}
}

// Two Datadog organisations stating the same log service name are two sources and two services
// (FR-062): neither C1 nor C9 merges them.
func TestTwoOrganisationsSameLogServiceNeverMerge(t *testing.T) {
	a := datadogLogServiceClaim("entity-org-a", "datadog:org-a", "checkout", "production")
	b := datadogLogServiceClaim("entity-org-b", "datadog:org-b", "checkout", "production")
	store := fakeStore{claims: []resolution.Claim{a, b}}
	for _, claim := range []resolution.Claim{a, b} {
		matches, err := resolution.Evaluate(context.Background(), store, claim)
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

// One source claiming both sides is one opinion, not corroboration.
func TestC9NeedsTwoSources(t *testing.T) {
	logs := datadogLogServiceClaim("entity-dd", "otel:nova", "checkout", "production")
	otel := observedServiceNameClaim("entity-otel", "checkout", "production")
	if a, b := evalBothWays(t, logs, otel); a || b {
		t.Error("C9 merged two claims from one source")
	}
}
