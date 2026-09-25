// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// The GCP rules (T048, T050, FR-117, FR-118, FR-121).
//
// Each certain rule is checked for what it fires on **and** for what it refuses to fire on: a
// certain rule merges without a human, so its false positives are the expensive kind.

// gcpCloudRunServiceClaim is the addressing claim the GCP feeder emits for every service (FR-115).
// C4 needs it, because the entity it merges an observed service with is the SERVICE and not the
// revision — see c4ServiceOf.
func gcpCloudRunServiceClaim(entityID, service, project, region string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-gcp-svc-" + project + "-" + service, EntityID: entityID,
		EntityType: graph.NodeTypeService, SourceID: "gcp:nova",
		EventID: "gcp:nova:claim:" + service, AppendedSeq: 20,
		Namespace: resolution.NamespaceGCPCloudRunService,
		Value:     project + "/" + region + "/" + service,
	}
}

func gcpRevisionClaim(entityID, revision, project, region string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-gcp-rev-" + revision, EntityID: entityID, EntityType: graph.NodeTypeWorkload,
		SourceID: "gcp:nova", EventID: "gcp:nova:claim:" + revision, AppendedSeq: 30,
		Namespace: resolution.NamespaceGCPCloudRunRevision,
		Value:     project + "/" + region + "/checkout/" + revision,
		Attributes: map[string]string{
			resolution.AttrGCPRevisionName: revision,
			resolution.AttrGCPProject:      project,
			resolution.AttrGCPRegion:       region,
		},
	}
}

func observedInRevisionClaim(entityID, service, revision, project, region string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-otel-" + service + "-" + revision, EntityID: entityID,
		EntityType: graph.NodeTypeService, SourceID: "otel:nova",
		EventID: "otel:nova:claim:" + service, AppendedSeq: 40,
		Namespace: resolution.NamespaceOTelService, Value: service,
		Attributes: map[string]string{
			resolution.AttrGCPRevisionName: revision,
			resolution.AttrGCPProject:      project,
			resolution.AttrGCPRegion:       region,
		},
	}
}

// C4: the instrumentation is running inside that revision. Not a resemblance — a report from inside
// the thing.
func TestC4MergesAnObservedServiceWithTheCloudRunServiceItRunsIn(t *testing.T) {
	service := gcpCloudRunServiceClaim("entity-service", "checkout", "nova-production", "europe-west1")
	revision := gcpRevisionClaim("entity-revision", "checkout-00042-abc", "nova-production", "europe-west1")
	observed := observedInRevisionClaim("entity-observed", "checkout", "checkout-00042-abc", "nova-production", "europe-west1")
	store := fakeStore{claims: []resolution.Claim{service, revision, observed}}

	// It runs in both directions, because either side may be the claim that just arrived.
	for _, trigger := range []resolution.Claim{observed, revision} {
		matches, err := resolution.Evaluate(context.Background(), store, trigger)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !hasRule(matches, "C4") {
			t.Fatalf("C4 did not fire from %s=%s (FR-117)", trigger.Namespace, trigger.Value)
		}
		for _, match := range matches {
			if match.RuleID != "C4" {
				continue
			}
			if !match.Certain || match.Score != 1.0 {
				t.Errorf("C4 fired certain=%v score=%v", match.Certain, match.Score)
			}
			if !strings.Contains(match.Rationale, "checkout-00042-abc") {
				t.Errorf("the rationale does not name the revision: %q", match.Rationale)
			}
			// The merged pair is the observed service and the Cloud Run SERVICE. It is not the
			// revision, and that is the whole of this rule's 2026-09-22 correction: over a
			// rollout, merging with the revision made two revisions one entity.
			if match.EntityA != "entity-observed" || match.EntityB != "entity-service" {
				t.Errorf("C4 merged %s with %s, want entity-observed with entity-service: the "+
					"observed service and the Cloud Run service are one thing seen twice, and a "+
					"revision is a version of it", match.EntityA, match.EntityB)
			}
			if !strings.Contains(match.Rationale, "nova-production/europe-west1/checkout") {
				t.Errorf("the rationale does not name the service the revision belongs to: %q", match.Rationale)
			}
			// Three claims: the observation, the revision that links them, and the service that
			// is merged. The revision is cited although it is not merged — it is the evidence,
			// and an explanation with the reason left out is not one.
			if len(match.SupportingClaimIDs) != 3 {
				t.Errorf("the match cites %d claims, want 3 (observed, revision, service)",
					len(match.SupportingClaimIDs))
			}
		}
	}
}

// The rollout. This is the case the rule got wrong, and it is a case about a *certain* rule, so
// getting it wrong merged production entities with no human in the loop.
//
// The instrumentation reports `faas.version` for whichever revision it is running in — 42, then 43
// after a deploy — both on the one subject `otel.service=checkout`. When C4 merged the observed
// service with the REVISION it fired twice, certain both times, and two certain merges sharing a
// side put the other two sides together: revision 42 and revision 43 became one entity. That erases
// the traffic split, the rollout and the ranking whose entire job is to say which revision is
// failing.
func TestC4DoesNotMergeTwoRevisionsOfOneServiceAcrossARollout(t *testing.T) {
	service := gcpCloudRunServiceClaim("entity-service", "checkout", "nova-production", "europe-west1")
	revOld := gcpRevisionClaim("entity-rev-42", "checkout-00042-abc", "nova-production", "europe-west1")
	revNew := gcpRevisionClaim("entity-rev-43", "checkout-00043-def", "nova-production", "europe-west1")
	// One subject, two observations: before the deploy and after it.
	obsOld := observedInRevisionClaim("entity-observed", "checkout", "checkout-00042-abc", "nova-production", "europe-west1")
	obsNew := observedInRevisionClaim("entity-observed", "checkout", "checkout-00043-def", "nova-production", "europe-west1")
	store := fakeStore{claims: []resolution.Claim{service, revOld, revNew, obsOld, obsNew}}

	// Every claim is a trigger, because any of them may be the one that just arrived.
	merged := map[[2]string]bool{}
	for _, trigger := range []resolution.Claim{obsOld, obsNew, revOld, revNew, service} {
		matches, err := resolution.Evaluate(context.Background(), store, trigger)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		for _, match := range matches {
			if match.RuleID != "C4" || !match.Certain {
				continue
			}
			a, b := match.EntityA, match.EntityB
			if a > b {
				a, b = b, a
			}
			merged[[2]string{a, b}] = true
		}
	}

	if len(merged) == 0 {
		t.Fatal("C4 fired on nothing across a rollout; the rule is meant to pair the observed " +
			"service with the Cloud Run service on every revision it reports")
	}
	// Exactly one pair, and it is the observed service with the Cloud Run service. Merging with
	// the service is idempotent across a rollout — every revision derives the same service — which
	// is what makes this rule safe to leave certain.
	for pair := range merged {
		if pair != [2]string{"entity-observed", "entity-service"} {
			t.Errorf("C4 merged %s with %s across a rollout. Any pair other than the observed "+
				"service and the Cloud Run service transitively merges two revisions into one "+
				"entity, which erases the traffic split and the rollout", pair[0], pair[1])
		}
	}
	if len(merged) != 1 {
		t.Errorf("C4 produced %d distinct certain merges across a rollout (%v), want 1: two "+
			"certain merges sharing a side put the other two sides together", len(merged), merged)
	}
}

// C4 fires whichever of its three claims arrives last (order independence).
//
// This is the property the third arm exists for, and it is not theoretical: a poll delivers the
// service claim and the revision claims in one batch, a fixture's shuffle step permutes events
// inside one cycle to find order dependence, and the arms from the observed service and from the
// revision both need the service claim to be there already. Without an arm triggered BY the service
// claim, a permutation that applied a revision before its service left the merge unmade for ever —
// nothing would trigger C4 again — so the same events would build two different graphs.
func TestC4FiresWhicheverOfItsThreeClaimsArrivesLast(t *testing.T) {
	service := gcpCloudRunServiceClaim("entity-service", "checkout", "nova-production", "europe-west1")
	revision := gcpRevisionClaim("entity-revision", "checkout-00042-abc", "nova-production", "europe-west1")
	observed := observedInRevisionClaim("entity-observed", "checkout", "checkout-00042-abc", "nova-production", "europe-west1")
	all := []resolution.Claim{service, revision, observed}

	// Every arrival order: whichever claim lands last is the trigger, with the other two already in
	// the graph. Six permutations, three distinct triggers.
	for _, last := range all {
		t.Run("last="+last.Namespace, func(t *testing.T) {
			store := fakeStore{claims: all}
			matches, err := resolution.Evaluate(context.Background(), store, last)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			var fired int
			for _, match := range matches {
				if match.RuleID != "C4" {
					continue
				}
				fired++
				if match.EntityA != "entity-observed" || match.EntityB != "entity-service" {
					t.Errorf("C4 merged %s with %s, want entity-observed with entity-service",
						match.EntityA, match.EntityB)
				}
			}
			if fired == 0 {
				t.Errorf("C4 did not fire with %s=%s arriving last and the other two already in "+
					"the graph. The same events in a different order would build a different "+
					"graph, which is the order dependence a shuffle exists to find",
					last.Namespace, last.Value)
			}
		})
	}
}

// And the service arm refuses across projects exactly as the other two do: the project and region it
// matches on are the REVISION claim's attributes, not a parse of the service's value.
func TestC4ServiceArmRefusesACrossProjectObservation(t *testing.T) {
	service := gcpCloudRunServiceClaim("entity-service", "checkout", "nova-production", "europe-west1")
	revision := gcpRevisionClaim("entity-revision", "checkout-00042-abc", "nova-production", "europe-west1")
	// Same revision NAME, another project. A revision name is unique within a service, not globally.
	observed := observedInRevisionClaim("entity-observed", "checkout", "checkout-00042-abc", "nova-staging", "europe-west1")

	store := fakeStore{claims: []resolution.Claim{service, revision, observed}}
	matches, err := resolution.Evaluate(context.Background(), store, service)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C4") {
		t.Error("the service arm merged an observation from another project (FR-010)")
	}
}

// A certain rule whose correctness depends on arrival order is not certain. With the revision in
// the graph and its service not yet, C4 refuses rather than falling back to the revision.
func TestC4RefusesWhenTheCloudRunServiceIsNotInTheGraphYet(t *testing.T) {
	revision := gcpRevisionClaim("entity-revision", "checkout-00042-abc", "nova-production", "europe-west1")
	observed := observedInRevisionClaim("entity-observed", "checkout", "checkout-00042-abc", "nova-production", "europe-west1")
	store := fakeStore{claims: []resolution.Claim{revision, observed}}

	for _, trigger := range []resolution.Claim{observed, revision} {
		matches, err := resolution.Evaluate(context.Background(), store, trigger)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if hasRule(matches, "C4") {
			t.Errorf("C4 fired from %s=%s with no Cloud Run service claim in the graph; falling "+
				"back to the revision is the behaviour that merged two revisions into one",
				trigger.Namespace, trigger.Value)
		}
	}
}

// A revision name is unique within a service, not globally. A rule that matched on the name alone
// would merge across projects — precisely the failure FR-010 exists to prevent.
func TestC4RefusesToMergeAcrossProjectsOrRegions(t *testing.T) {
	// The service claim is present throughout, so a refusal here is the project-or-region rule
	// refusing and not the absent-service rule above doing it for the wrong reason.
	service := gcpCloudRunServiceClaim("entity-service", "checkout", "nova-production", "europe-west1")
	revision := gcpRevisionClaim("entity-revision", "checkout-00042-abc", "nova-production", "europe-west1")

	cases := []struct {
		name     string
		observed resolution.Claim
	}{
		{"another project", observedInRevisionClaim("e2", "checkout", "checkout-00042-abc", "nova-staging", "europe-west1")},
		{"another region", observedInRevisionClaim("e3", "checkout", "checkout-00042-abc", "nova-production", "us-central1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fakeStore{claims: []resolution.Claim{service, revision, tc.observed}}
			matches, err := resolution.Evaluate(context.Background(), store, tc.observed)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if hasRule(matches, "C4") {
				t.Fatalf("C4 merged a revision name across %s; a revision name is unique within a "+
					"service, not globally (FR-010)", tc.name)
			}
		})
	}

	// And a claim missing any of the three locates no revision at all.
	for _, missing := range []string{resolution.AttrGCPRevisionName, resolution.AttrGCPProject, resolution.AttrGCPRegion} {
		observed := observedInRevisionClaim("e4", "checkout", "checkout-00042-abc", "nova-production", "europe-west1")
		delete(observed.Attributes, missing)
		store := fakeStore{claims: []resolution.Claim{service, revision, observed}}
		matches, err := resolution.Evaluate(context.Background(), store, observed)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if hasRule(matches, "C4") {
			t.Errorf("C4 fired on a claim missing %s; a partial match here is a merge across projects", missing)
		}
	}
}

func gcpDeclaredServiceNameClaim(entityID, name, environment string) resolution.Claim {
	attrs := map[string]string{resolution.AttrGCPDeclaredServiceName: "OTEL_SERVICE_NAME"}
	if environment != "" {
		attrs[resolution.AttrEnvironment] = environment
	}
	return resolution.Claim{
		ClaimID: "claim-gcp-declared-" + name + "-" + environment, EntityID: entityID,
		EntityType: graph.NodeTypeService, SourceID: "gcp:nova",
		EventID: "gcp:nova:claim:" + name, AppendedSeq: 30,
		Namespace: resolution.NamespaceOTelService, Value: name, Attributes: attrs,
	}
}

func observedServiceNameClaim(entityID, name, environment string) resolution.Claim {
	attrs := map[string]string{}
	if environment != "" {
		attrs[resolution.AttrEnvironment] = environment
	}
	return resolution.Claim{
		ClaimID: "claim-otel-declared-" + name + "-" + environment, EntityID: entityID,
		EntityType: graph.NodeTypeService, SourceID: "otel:nova",
		EventID: "otel:nova:claim:" + name, AppendedSeq: 40,
		Namespace: resolution.NamespaceOTelService, Value: name, Attributes: attrs,
	}
}

// C5: somebody configured it to be true. The declaration is the evidence.
func TestC5MergesADeclaredServiceNameInTheSameEnvironment(t *testing.T) {
	declared := gcpDeclaredServiceNameClaim("entity-cloudrun", "checkout", "production")
	observed := observedServiceNameClaim("entity-observed", "checkout", "production")
	store := fakeStore{claims: []resolution.Claim{declared, observed}}

	matches, err := resolution.Evaluate(context.Background(), store, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !hasRule(matches, "C5") {
		t.Fatalf("C5 did not fire (FR-118): %+v", matches)
	}
}

// FR-118 states it explicitly: a missing environment on either side MUST NOT satisfy it. The
// tempting reading — "if only one side states an environment, trust it" — is how a staging service
// gets merged into production topology, and C5 merges without asking.
func TestC5RefusesAMissingOrDisagreeingEnvironment(t *testing.T) {
	cases := []struct {
		name                string
		declaredEnv, obsEnv string
	}{
		{"neither side states one", "", ""},
		{"only the Cloud Run side states one", "production", ""},
		{"only the observed side states one", "", "production"},
		{"the two disagree", "production", "staging"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			declared := gcpDeclaredServiceNameClaim("entity-cloudrun", "checkout", tc.declaredEnv)
			observed := observedServiceNameClaim("entity-observed", "checkout", tc.obsEnv)
			store := fakeStore{claims: []resolution.Claim{declared, observed}}
			matches, err := resolution.Evaluate(context.Background(), store, observed)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if hasRule(matches, "C5") {
				t.Fatalf("C5 fired with %s; `checkout` in staging is not `checkout` in production, "+
					"and a certain rule may not gamble on that (FR-118)", tc.name)
			}
		})
	}
}

// Two *observed* names on one identifier is C1's business, not C5's: without the declaring side
// there is no configuration to rest on.
func TestC5RequiresADeclaringSide(t *testing.T) {
	a := observedServiceNameClaim("entity-a", "checkout", "production")
	b := observedServiceNameClaim("entity-b", "checkout", "production")
	b.ClaimID, b.SourceID = "claim-otel-b", "otel:other"
	store := fakeStore{claims: []resolution.Claim{a, b}}
	matches, err := resolution.Evaluate(context.Background(), store, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C5") {
		t.Fatal("C5 fired with no declaring side")
	}
}

func gcpServiceClaim(entityID, value string, envVars string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-gcp-svc-" + value, EntityID: entityID, EntityType: graph.NodeTypeService,
		SourceID: "gcp:nova", EventID: "gcp:nova:claim:" + value, AppendedSeq: 30,
		Namespace: resolution.NamespaceGCPCloudRunService, Value: value,
		Attributes: map[string]string{resolution.AttrGCPEnvVarNames: envVars},
	}
}

func gcpInstanceClaim(entityID, instance string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-gcp-sql-" + instance, EntityID: entityID, EntityType: graph.NodeTypeInfraResource,
		SourceID: "gcp:nova", EventID: "gcp:nova:claim:" + instance, AppendedSeq: 30,
		Namespace:  resolution.NamespaceGCPSQLInstance,
		Value:      "nova-production/europe-west1/" + instance,
		Attributes: map[string]string{resolution.AttrGCPInstanceName: instance},
	}
}

// P4 suggests and stops: the variable may be dead configuration, and a dependency edge asserted on a
// variable name is an edge nobody can check.
func TestP4SuggestsAnInstanceNamedInAnEnvironmentVariableAndNeverMerges(t *testing.T) {
	service := gcpServiceClaim("entity-service", "nova-production/europe-west1/checkout",
		"PORT ORDERS_PRIMARY_DSN LOG_LEVEL")
	instance := gcpInstanceClaim("entity-instance", "orders-primary")
	store := fakeStore{claims: []resolution.Claim{service, instance}}

	for _, trigger := range []resolution.Claim{service, instance} {
		matches, err := resolution.Evaluate(context.Background(), store, trigger)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !hasRule(matches, "P4") {
			t.Fatalf("P4 did not fire from %s (FR-121)", trigger.Namespace)
		}
		for _, match := range matches {
			if match.RuleID != "P4" {
				continue
			}
			if match.Certain {
				t.Fatal("P4 produced a certain match; probable rules suggest and never merge (FR-121)")
			}
			if match.Score != resolution.ScoreP4 {
				t.Errorf("score = %v, want %v", match.Score, resolution.ScoreP4)
			}
			if !strings.Contains(match.Rationale, "ORDERS_PRIMARY_DSN") {
				t.Errorf("the rationale does not name the variable: %q", match.Rationale)
			}
			if !strings.Contains(match.Rationale, "Probable only") {
				t.Errorf("the rationale does not say it is a suggestion: %q", match.Rationale)
			}
		}
	}
}

// A two-letter instance name is inside almost every variable name in existence, so the rule would
// fire on coincidence.
func TestP4RefusesAnInstanceNameTooShortToMeanAnything(t *testing.T) {
	service := gcpServiceClaim("entity-service", "nova-production/europe-west1/checkout", "DB_HOST DATABASE_URL")
	instance := gcpInstanceClaim("entity-instance", "db")
	store := fakeStore{claims: []resolution.Claim{service, instance}}
	matches, err := resolution.Evaluate(context.Background(), store, instance)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "P4") {
		t.Fatalf("P4 fired on a %d-character instance name; below %d the rule fires on coincidence",
			len("db"), resolution.MinInstanceNameMatch)
	}

	// A service that mentions nothing does not match either, or the rule would fire on everything.
	quiet := gcpServiceClaim("entity-quiet", "nova-production/europe-west1/search", "PORT LOG_LEVEL")
	longInstance := gcpInstanceClaim("entity-instance-2", "orders-primary")
	store = fakeStore{claims: []resolution.Claim{quiet, longInstance}}
	matches, err = resolution.Evaluate(context.Background(), store, longInstance)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "P4") {
		t.Error("P4 fired on a service whose variables name nothing")
	}
}

// P5 is the weakest published rule and requires both a shared owner and a normalised-name match: one
// team owns many similarly named things, which is what a naming convention is for.
func TestP5RequiresBothASharedOwnerAndANameMatch(t *testing.T) {
	cloudRun := gcpServiceClaim("entity-cloudrun", "nova-production/europe-west1/checkout", "")
	observed := otelServiceClaim("entity-observed", "checkout-svc", nil)

	withOwner := fakeStore{
		claims: []resolution.Claim{cloudRun, observed},
		owners: map[string]string{"entity-cloudrun": "team-payments", "entity-observed": "team-payments"},
	}
	matches, err := resolution.Evaluate(context.Background(), withOwner, cloudRun)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !hasRule(matches, "P5") {
		t.Fatalf("P5 did not fire with a shared owner and a name match: %+v", matches)
	}
	for _, match := range matches {
		if match.RuleID == "P5" && match.Certain {
			t.Fatal("P5 produced a certain match")
		}
	}

	// No shared owner, no P5 — a name match alone is P1's business.
	withoutOwner := fakeStore{claims: []resolution.Claim{cloudRun, observed}}
	matches, err = resolution.Evaluate(context.Background(), withoutOwner, cloudRun)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "P5") {
		t.Fatal("P5 fired with no shared owner")
	}
}

// Every published rule carries what a recorded merge has to explain: an id, a description, a score,
// and — for a certain rule — a score of exactly 1.0 (FR-123, constitution VI).
func TestEveryGCPRuleCanExplainItself(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range resolution.GCPRules() {
		seen[rule.ID] = true
		if rule.Description == "" {
			t.Errorf("rule %s has no published description", rule.ID)
		}
		if len(rule.Namespaces) == 0 {
			t.Errorf("rule %s names no namespaces", rule.ID)
		}
		if rule.Certain && rule.Score != 1.0 {
			t.Errorf("certain rule %s carries score %v, want 1.0", rule.ID, rule.Score)
		}
		if !rule.Certain && (rule.Score <= 0 || rule.Score >= 1) {
			t.Errorf("probable rule %s carries score %v, which is not a suggestion", rule.ID, rule.Score)
		}
	}
	for _, id := range []string{"C4", "C5", "C7", "P4", "P5"} {
		if !seen[id] {
			t.Errorf("rule %s is not published by GCPRules", id)
		}
	}
	// And they are in the registry the CLI and the documentation print.
	registered := map[string]bool{}
	for _, rule := range resolution.Rules() {
		registered[rule.ID] = true
	}
	for id := range seen {
		if !registered[id] {
			t.Errorf("rule %s is not registered", id)
		}
	}
}

func hasRule(matches []resolution.Match, id string) bool {
	for _, match := range matches {
		if match.RuleID == id {
			return true
		}
	}
	return false
}

// C7: two sources claiming one Cloud SQL instance connection name (FR-120, T130).

func sqlInstanceClaim(claimID, entityID, sourceID, value, connection string) resolution.Claim {
	claim := resolution.Claim{
		ClaimID: claimID, EntityID: entityID, EntityType: graph.NodeTypeInfraResource,
		SourceID: sourceID, EventID: sourceID + ":claim:" + claimID, AppendedSeq: 50,
		Namespace: resolution.NamespaceGCPSQLInstance, Value: value,
	}
	if connection != "" {
		claim.Attributes = map[string]string{resolution.AttrGCPSQLConnectionName: connection}
	}
	return claim
}

// The case the rule is for: the two sources address the instance differently and agree on the
// connection name. A rule that compared the identifier VALUES would find nothing here.
func TestC7MergesTwoSourcesOnTheInstanceConnectionName(t *testing.T) {
	gcp := sqlInstanceClaim("claim-gcp-sql", "entity-a", "gcp:nova",
		"nova-production/europe-west1/orders-primary", "nova-production:europe-west1:orders-primary")
	other := sqlInstanceClaim("claim-k8s-sql", "entity-b", "k8s:nova",
		"projects/nova-production/instances/orders-primary", "nova-production:europe-west1:orders-primary")

	store := fakeStore{claims: []resolution.Claim{gcp, other}}
	matches, err := resolution.Evaluate(context.Background(), store, gcp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !hasRule(matches, "C7") {
		t.Fatalf("C7 did not fire on two sources claiming one connection name: %+v", matches)
	}
	for _, match := range matches {
		if match.RuleID != "C7" {
			continue
		}
		if !match.Certain || match.Score != 1.0 {
			t.Errorf("C7 match is certain=%v score=%v, want true and 1.0", match.Certain, match.Score)
		}
		if !strings.Contains(match.Rationale, "nova-production:europe-west1:orders-primary") {
			t.Errorf("rationale does not quote the connection name it merged on: %q", match.Rationale)
		}
		if len(match.SupportingClaimIDs) != 2 {
			t.Errorf("supporting claims = %v, want both claims", match.SupportingClaimIDs)
		}
	}
}

// One source asserting its own connection name twice is one opinion, not corroboration.
func TestC7DoesNotFireOnOneSourceTwice(t *testing.T) {
	first := sqlInstanceClaim("claim-a", "entity-a", "gcp:nova",
		"nova-production/europe-west1/orders-primary", "nova-production:europe-west1:orders-primary")
	second := sqlInstanceClaim("claim-b", "entity-b", "gcp:nova",
		"projects/nova-production/instances/orders-primary", "nova-production:europe-west1:orders-primary")

	store := fakeStore{claims: []resolution.Claim{first, second}}
	matches, err := resolution.Evaluate(context.Background(), store, first)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C7") {
		t.Fatal("C7 fired on two claims from one source")
	}
}

// The guard that matters most: a source that fills the attribute with a malformed string must not
// merge every instance that made the same mistake — automatically, and with a score of 1.0.
func TestC7RefusesAConnectionNameThatIsNotShapedLikeOne(t *testing.T) {
	for _, connection := range []string{"", ":", "::", "nova-production:europe-west1",
		"nova-production::orders-primary", "a:b:c:d"} {
		mine := sqlInstanceClaim("claim-a", "entity-a", "gcp:nova", "one", connection)
		theirs := sqlInstanceClaim("claim-b", "entity-b", "k8s:nova", "two", connection)
		store := fakeStore{claims: []resolution.Claim{mine, theirs}}
		matches, err := resolution.Evaluate(context.Background(), store, mine)
		if err != nil {
			t.Fatalf("Evaluate(%q): %v", connection, err)
		}
		if hasRule(matches, "C7") {
			t.Errorf("C7 fired on the connection name %q", connection)
		}
	}
}

// Two different instances are two entities, however alike their names.
func TestC7DoesNotMergeTwoDifferentInstances(t *testing.T) {
	primary := sqlInstanceClaim("claim-a", "entity-a", "gcp:nova",
		"nova-production/europe-west1/orders-primary", "nova-production:europe-west1:orders-primary")
	replica := sqlInstanceClaim("claim-b", "entity-b", "k8s:nova",
		"nova-production/europe-west1/orders-replica", "nova-production:europe-west1:orders-replica")

	store := fakeStore{claims: []resolution.Claim{primary, replica}}
	matches, err := resolution.Evaluate(context.Background(), store, primary)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C7") {
		t.Fatal("C7 merged two instances with different connection names")
	}
}

// The same instance in two projects is two instances: the project is inside the connection name, so
// this is the cross-project merge FR-010 exists to prevent, and it must not happen.
func TestC7DoesNotMergeAcrossProjects(t *testing.T) {
	production := sqlInstanceClaim("claim-a", "entity-a", "gcp:nova",
		"nova-production/europe-west1/orders", "nova-production:europe-west1:orders")
	staging := sqlInstanceClaim("claim-b", "entity-b", "k8s:nova",
		"nova-staging/europe-west1/orders", "nova-staging:europe-west1:orders")

	store := fakeStore{claims: []resolution.Claim{production, staging}}
	matches, err := resolution.Evaluate(context.Background(), store, production)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C7") {
		t.Fatal("C7 merged one instance name across two projects")
	}
}

// C7 outranks C1 on a pair both fire on, so the recorded reason is the connection name rather than
// "two sources used the same identifier".
func TestC7OutranksC1WhenBothFire(t *testing.T) {
	value := "nova-production/europe-west1/orders-primary"
	connection := "nova-production:europe-west1:orders-primary"
	mine := sqlInstanceClaim("claim-a", "entity-a", "gcp:nova", value, connection)
	theirs := sqlInstanceClaim("claim-b", "entity-b", "k8s:nova", value, connection)

	store := fakeStore{claims: []resolution.Claim{mine, theirs}}
	matches, err := resolution.Evaluate(context.Background(), store, mine)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no match on two sources claiming one identifier")
	}
	if matches[0].RuleID != "C7" {
		t.Errorf("first match is %s, want C7: the most specific rule that fires is the recorded reason", matches[0].RuleID)
	}
}
