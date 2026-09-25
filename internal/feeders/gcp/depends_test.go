// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// Dependencies, derived and proposed (T134, T135; FR-028, FR-029).

var ordersPrimary = gcpfeeder.SQLInstance{Project: sqlProject, Region: sqlRegion, Name: sqlInstance}

func cloudSQLVolume(connections ...string) *runpb.Volume {
	return &runpb.Volume{
		Name:       "cloudsql",
		VolumeType: &runpb.Volume_CloudSqlInstance{CloudSqlInstance: &runpb.CloudSqlInstance{Instances: connections}},
	}
}

// The revision attaches the instance: an operator wrote it down and Cloud Run acts on it, so the edge
// is asserted with the deriving evidence recorded.
func TestAnAttachedInstanceDerivesAnEdgeWithItsEvidence(t *testing.T) {
	config := observeConfig(t, serviceWithConfig(7, nil,
		[]*runpb.Volume{cloudSQLVolume(ordersPrimary.ConnectionName())}), fingerprintKey)
	deps := gcpfeeder.DeriveDependencies(config, nil)
	if len(deps.Derived) != 1 {
		t.Fatalf("Derived = %+v, want one edge", deps.Derived)
	}
	derived := deps.Derived[0]
	if derived.Instance.Value() != ordersPrimary.Value() {
		t.Errorf("instance = %q, want %q", derived.Instance.Value(), ordersPrimary.Value())
	}
	if derived.Derivation != gcpfeeder.DerivationCloudSQLVolume {
		t.Errorf("derivation = %q, want the volume attachment", derived.Derivation)
	}
	props := propsMap(t, derived.Props())
	if !strings.Contains(props[gcpfeeder.PropDependencyEvidence], "cloudSqlInstance") {
		t.Errorf("evidence = %q, does not name the field it was read from (FR-028)",
			props[gcpfeeder.PropDependencyEvidence])
	}
	if props[gcpfeeder.PropDependencyConnectionName] != ordersPrimary.ConnectionName() {
		t.Errorf("connection name = %q, want the one string a reviewer can check against a deployment",
			props[gcpfeeder.PropDependencyConnectionName])
	}
	// And no proposal: a proposal for an edge that exists reads as though the derivation had failed.
	if len(deps.Proposed) != 0 {
		t.Errorf("a derived dependency was also proposed: %+v", deps.Proposed)
	}
	edge, err := derived.EdgeFact(created)
	if err != nil {
		t.Fatalf("EdgeFact: %v", err)
	}
	if edge.Type != graphv1.EdgeType_DEPENDS_ON {
		t.Errorf("edge type = %s, want DEPENDS_ON", edge.Type)
	}
	if !edge.ValidAt.Equal(created) {
		t.Errorf("valid_at = %s, want the instant supplied", edge.ValidAt)
	}
}

// An environment variable whose VALUE is a connection name derives the edge too — and the value is
// read rather than recorded: the evidence names the variable and says what it contained, not what.
func TestAConnectionNameInAValueDerivesAnEdgeWithoutStoringTheValue(t *testing.T) {
	dsn := "postgres://appuser:hunter2@/orders?host=/cloudsql/" + ordersPrimary.ConnectionName()
	config := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", dsn),
	}, nil), fingerprintKey)
	deps := gcpfeeder.DeriveDependencies(config, nil)
	if len(deps.Derived) != 1 || deps.Derived[0].Derivation != gcpfeeder.DerivationEnvVarValue {
		t.Fatalf("Derived = %+v, want one edge derived from the variable's value", deps.Derived)
	}
	props := propsMap(t, deps.Derived[0].Props())
	for key, value := range props {
		if strings.Contains(value, "hunter2") || strings.Contains(value, "postgres://") {
			t.Fatalf("evidence property %s carries the value it was derived from: %q", key, value)
		}
	}
	if !strings.Contains(props[gcpfeeder.PropDependencyEvidence], "DATABASE_URL") {
		t.Errorf("evidence does not name the variable: %q", props[gcpfeeder.PropDependencyEvidence])
	}
	// The connection name itself is recorded, and that is deliberate: it is a published identifier
	// the console shows and the feeder claims, not a configuration value.
	if props[gcpfeeder.PropDependencyConnectionName] != ordersPrimary.ConnectionName() {
		t.Errorf("connection name = %q, want it recorded", props[gcpfeeder.PropDependencyConnectionName])
	}
}

// A host name that merely contains colons is not a connection name. A substring search would assert a
// dependency on an instance that does not exist.
func TestAColonInAHostnameIsNotAConnectionName(t *testing.T) {
	for _, value := range []string{
		"redis://cache.internal:6379/0",
		"a:b",
		"fe80::1",
		"host=orders.internal port=5432",
	} {
		config := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
			literalEnv("SOME_URL", value),
		}, nil), fingerprintKey)
		if len(config.EnvValuesNamingInstances) != 0 {
			t.Errorf("%q was read as naming an instance: %v", value, config.EnvValuesNamingInstances)
		}
	}
}

// The case FR-029 exists for: nothing names the connection name, a variable name resembles the
// instance, and the result is a **proposal with no edge**.
func TestAResemblingVariableNameProposesAndNeverAsserts(t *testing.T) {
	config := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("ORDERS_PRIMARY_DSN", "held-in-secret-manager"),
	}, nil), fingerprintKey)
	deps := gcpfeeder.DeriveDependencies(config, []gcpfeeder.SQLInstance{ordersPrimary})
	if len(deps.Derived) != 0 {
		t.Fatalf("an edge was asserted on a variable NAME: %+v", deps.Derived)
	}
	if len(deps.Proposed) != 1 {
		t.Fatalf("Proposed = %+v, want one suggestion", deps.Proposed)
	}
	proposal := deps.Proposed[0]
	if proposal.RuleID != gcpfeeder.DependencyRuleEnvVarName {
		t.Errorf("rule = %q, want %q", proposal.RuleID, gcpfeeder.DependencyRuleEnvVarName)
	}
	if proposal.Score != gcpfeeder.ScoreD1 {
		t.Errorf("score = %v, want the published %v", proposal.Score, gcpfeeder.ScoreD1)
	}
	if !strings.Contains(proposal.Rationale, "ORDERS_PRIMARY_DSN") {
		t.Errorf("rationale does not name the variable: %q", proposal.Rationale)
	}
	fact, err := proposal.Fact()
	if err != nil {
		t.Fatalf("Fact: %v", err)
	}
	if fact.Type != graphv1.EdgeType_DEPENDS_ON {
		t.Errorf("proposed type = %s, want DEPENDS_ON", fact.Type)
	}
	if fact.Src.GetValue() == fact.Dst.GetValue() {
		t.Error("the proposal names one entity at both ends")
	}
	if fact.Evidence == nil || len(fact.Evidence.GetFields()) == 0 {
		t.Error("the proposal carries no evidence; a reviewer would decide from the score alone")
	}
}

// A two-letter instance name is inside almost every variable name in existence, so the rule refuses
// to fire on one — exactly as the identity rule P4 does.
func TestAShortInstanceNameDoesNotPropose(t *testing.T) {
	short := gcpfeeder.SQLInstance{Project: sqlProject, Region: sqlRegion, Name: "db"}
	config := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("DB_HOST", "somewhere"),
	}, nil), fingerprintKey)
	if deps := gcpfeeder.DeriveDependencies(config, []gcpfeeder.SQLInstance{short}); len(deps.Proposed) != 0 {
		t.Fatalf("a two-letter instance name proposed a dependency: %+v", deps.Proposed)
	}
	// And the bound is the same one the identity rule applies to the same evidence.
	if gcpfeeder.MinInstanceNameMatch != resolution.MinInstanceNameMatch {
		t.Errorf("the feeder's bound is %d and the rule's is %d; the same observation must not be "+
			"held to two standards", gcpfeeder.MinInstanceNameMatch, resolution.MinInstanceNameMatch)
	}
}

// Two projects are two environments until somebody says otherwise: a staging service whose variable
// happens to name a production instance is a coincidence of naming conventions (FR-010).
func TestAnInstanceInAnotherProjectIsNotProposed(t *testing.T) {
	other := gcpfeeder.SQLInstance{Project: "nova-staging", Region: sqlRegion, Name: sqlInstance}
	config := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("ORDERS_PRIMARY_DSN", "held-in-secret-manager"),
	}, nil), fingerprintKey)
	if deps := gcpfeeder.DeriveDependencies(config, []gcpfeeder.SQLInstance{other}); len(deps.Proposed) != 0 {
		t.Fatalf("an instance in another project was proposed: %+v", deps.Proposed)
	}
}

// A suggestion about an instance nobody has observed is a suggestion about nothing.
func TestNothingIsProposedBeforeAnyInstanceIsObserved(t *testing.T) {
	config := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("ORDERS_PRIMARY_DSN", "held-in-secret-manager"),
	}, nil), fingerprintKey)
	deps := gcpfeeder.DeriveDependencies(config, nil)
	if len(deps.Proposed) != 0 || len(deps.Derived) != 0 {
		t.Fatalf("something was emitted with no instance observed: %+v", deps)
	}
}

// A connection name Cloud Run reported that cannot be parsed is not turned into an edge under a
// guessed ref: a guessed ref resolves against something, and that something is not what anybody meant.
func TestAnUnparseableConnectionNameDerivesNothing(t *testing.T) {
	config := observeConfig(t, serviceWithConfig(7, nil,
		[]*runpb.Volume{cloudSQLVolume("not-a-connection-name")}), fingerprintKey)
	if deps := gcpfeeder.DeriveDependencies(config, nil); len(deps.Derived) != 0 {
		t.Fatalf("an unparseable connection name produced an edge: %+v", deps.Derived)
	}
}

// A proposal's score must be rankable, or the suggestion sits at whichever end of the review queue
// the sort happens to put it.
func TestAProposalWithAnUnrankableScoreIsRefused(t *testing.T) {
	for _, score := range []float64{0, -1, 1.5} {
		proposal := gcpfeeder.DependencyProposal{
			Service:  gcpfeeder.Service{Project: sqlProject, Region: sqlRegion, Name: "checkout"},
			Instance: ordersPrimary, RuleID: gcpfeeder.DependencyRuleEnvVarName, Score: score,
		}
		if _, err := proposal.Fact(); err == nil {
			t.Errorf("a proposal with score %v was accepted", score)
		}
	}
}
