// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// The GCP rules (T048, T050, T130, FR-117, FR-118, FR-120, FR-121, FR-123).
//
// Five rules, and the split between them is the whole of ADR-0001 D6: a **certain** rule may merge on
// its own, a **probable** rule produces a suggestion and nothing merges without a person.
//
//	C4  certain   an observed service's attributes name the Cloud Run revision it runs in
//	C5  certain   a Cloud Run service declares an OTel service name, an observed service claims it,
//	              and both sides state the same environment
//	C7  certain   two sources claim the same Cloud SQL instance connection name — the instance as
//	              GCP lists it, and the address a caller reached it at
//	P4  probable  an instance name appears inside a service's environment-variable name
//	P5  probable  a shared owning label with a similar name
//
// FR-121's third suggestive case — normalised name equality without an agreed environment — is
// already P1, which compares normalised names across the namespaces it is given. It is extended to
// the GCP namespaces here rather than duplicated as a sixth rule: two rules that fire on the same
// pair for the same reason make a recorded decision unreadable, which is exactly what Register's
// duplicate-id panic exists to prevent one spelling of.
//
// # C4's observed side (T182, landed)
//
// The rule needs both halves, and for a while it had one. The GCP feeder carried the three
// revision-locating attributes on its own claims, and feature 002's aggregator did not translate the
// resource attributes the GCP OpenTelemetry detector sets — so no observed service claim carried
// `sre.gcp.revision_name`, C4 found nothing to pair with, and it was published, registered, evaluated
// on every claim and **silently never fired**.
//
// `internal/feeders/otel` now carries them: `faas.version` is the revision name on Cloud Run,
// `cloud.account.id` the project and `cloud.region` the region, all three gated on
// `cloud.platform=gcp_cloud_run` and emitted **all together or not at all**. The gate and the
// all-or-nothing rule are both about this rule being *certain*: `faas.version` is an alias on AWS
// Lambda, and a revision name is unique within a service rather than globally, so either shortcut
// would have this rule merging unrelated entities with no human in the loop.
//
// The pairing is asserted end to end in `internal/feeders/otel`'s tests rather than here, because this
// package may not import a feeder — a rule has to be testable without a connector present — so the
// assertion lives on the side where the import is legal.
//
// # What it merged with, corrected 2026-09-22 (T175)
//
// Landing the observed side made a second defect reachable, and it was a worse one. FR-117 says "the
// observed service and the revision's **Cloud Run service** are one entity", the rationale the rule
// records said the same, and the code merged the observed service with the **revision**.
//
// Over one poll that is invisible. Over a rollout it is not. The instrumentation reports
// `faas.version` for whichever revision it is running in — 42, then 43 after a deploy — both on the
// one subject `otel.service=checkout`. So C4 fired twice, certain both times, merging the observed
// service with revision 42 and with revision 43; and two certain merges sharing a side put the other
// two sides together. **The two revisions became one entity**, with no human in the loop. That erases
// the traffic split, the rollout, and the ranking whose entire job is to say which revision is
// failing — US1's whole subject, dissolved by US1's own rule.
//
// It was latent rather than active: nothing in the corpus paired a GCP claim with another source's,
// so C4 never fired anywhere and the bug was unreachable. That is also why it survived review —
// and it is the argument for the cross-source fixture T175 asks for, because a rule nothing
// exercises is a rule whose defects are all latent.
//
// See c4ServiceOf for the fix and why finding no service claim means not firing.
//
// # Why C4, C5 and C7 are certain and everything else is not
//
// C4 rests on the instrumentation *being inside* the revision: a span whose resource attributes carry
// `sre.gcp.revision_name` was emitted by a process running in that revision. That is not a
// resemblance, it is a report from inside the thing.
//
// C5 rests on somebody having configured it: an operator wrote `OTEL_SERVICE_NAME` on the service, and
// telemetry arrived claiming that name. The declaration is the evidence.
//
// C7 rests on the string being **what the proxy dials**. A Cloud SQL instance connection name is
// globally unique, and every client that reaches the instance is configured with it verbatim — a
// `--add-cloudsql-instances` flag, a sidecar argument, a connection string. Two sources reporting
// that string are not resembling each other, they are quoting the same configured identifier.
//
// # Why C7 is not C1 with extra steps
//
// C1 already merges two sources that assert the same (namespace, value) pair, and where both
// sources happen to spell the connection name as the identifier itself, C1 would fire. C7 exists
// for the case that is actually common: the two sources address the instance **differently** —
// `<project>/<region>/<instance>`, the fully qualified resource name, a name an operator typed —
// and carry the connection name as a supporting attribute of whichever identifier they do use. C7
// compares the attribute, so the values need not be the same string. Its Specificity sits above
// C1's so that when both fire, the recorded reason is the connection name rather than "two sources
// used the same identifier" — the first is a fact a reviewer can check against a deployment, the
// second is a tautology.
//
// Everything else — a name that looks like another name, an instance mentioned in an environment
// variable, two things owned by teams with similar names — is a resemblance. Resemblances are how
// `checkout` in staging gets merged with `checkout` in production, so they suggest and stop.

// The GCP identifier namespaces the rules compare. They are re-declared here rather than imported
// from `internal/feeders/gcp` on purpose: `internal/resolution` must not depend on a feeder, or the
// rules would only be testable with a connector present — and the whole design of this package is
// that a rule is testable without Postgres and without a source system.
const (
	// NamespaceGCPCloudRunService is `gcp.cloudrun.service`, valued `<project>/<region>/<service>`.
	NamespaceGCPCloudRunService = "gcp.cloudrun.service"
	// NamespaceGCPCloudRunRevision is `gcp.cloudrun.revision`.
	NamespaceGCPCloudRunRevision = "gcp.cloudrun.revision"
	// NamespaceGCPSQLInstance is `gcp.cloudsql.instance`.
	NamespaceGCPSQLInstance = "gcp.cloudsql.instance"
)

// Claim attribute keys the GCP rules read. They match what the feeder emits; a rule and a feeder that
// spell an attribute differently is a rule that never fires, and it fails silently.
const (
	// AttrGCPRevisionName is `sre.gcp.revision_name`: which Cloud Run revision a fact is about.
	AttrGCPRevisionName = "sre.gcp.revision_name"
	// AttrGCPProject is `sre.gcp.project`.
	AttrGCPProject = "sre.gcp.project"
	// AttrGCPRegion is `sre.gcp.region`.
	AttrGCPRegion = "sre.gcp.region"
	// AttrGCPRevisionOf is `sre.gcp.revision_of`: the service a revision belongs to, as the feeder
	// addresses it.
	AttrGCPRevisionOf = "sre.gcp.revision_of"
	// AttrGCPDeclaredServiceName marks a claim as the OTel service name a Cloud Run service
	// *declares*, rather than one observed on telemetry. C5 requires the declaring side, and
	// without this marker the rule cannot tell which side is which — so it would fire on two
	// observed names, which is P1's business.
	AttrGCPDeclaredServiceName = "sre.gcp.declared_service_name"
	// AttrGCPEnvVarNames is the set of environment-variable *names* a service defines, joined by
	// spaces. P4 reads it. The names only: a value is configuration and is dropped (FR-034), and
	// the name is enough for the suggestion this rule makes.
	AttrGCPEnvVarNames = "sre.gcp.env_var_names"
	// AttrGCPInstanceName is the bare Cloud SQL instance name, for P4's substring test.
	AttrGCPInstanceName = "sre.gcp.instance_name"
	// AttrGCPSQLConnectionName is the Cloud SQL **instance connection name**,
	// `<project>:<region>:<instance>` — the string a client is configured with and the one the
	// Cloud SQL proxy dials. C7 resolves on it (FR-120), and the GCP feeder emits it on every claim
	// it makes about an instance (FR-035). The two spellings are asserted equal in
	// internal/feeders/gcp's tests: a rule and a feeder that spell an attribute differently is a
	// rule that never fires, and it fails silently.
	AttrGCPSQLConnectionName = "sre.gcp.instance_connection_name"
)

// The probable scores. They are published constants rather than tuned numbers: a score a reader
// cannot look up is a score they cannot argue with.
const (
	// ScoreP4 is an instance named inside an environment-variable name. Reasonably strong — a
	// service that mentions a database in `ORDERS_PRIMARY_DSN` usually talks to it — and still a
	// resemblance, because the variable may be dead configuration.
	ScoreP4 = 0.6
	// ScoreP5 is a shared owning label with a similar name. Weak on its own; it is here because
	// FR-121 names it, and because it corroborates a name match that would otherwise be thrown
	// away.
	ScoreP5 = 0.4
)

var certainRuleC4 = Rule{
	ID:          "C4",
	Certain:     true,
	Score:       certainScore,
	Specificity: 25,
	Namespaces: []string{
		NamespaceOTelService, NamespaceGCPCloudRunRevision, NamespaceGCPCloudRunService,
	},
	Description: "An observed service's resource attributes name the Cloud Run revision it is " +
		"running in (sre.gcp.revision_name, with the project and region that disambiguate it). The " +
		"instrumentation is running inside that revision, so the observed service and the revision's " +
		"Cloud Run service are one entity. The merge is with the SERVICE the revision belongs to and " +
		"never with the revision, which is a version of it: the revision is cited as the evidence.",
	Eval: evalC4,
}

var certainRuleC5 = Rule{
	ID:          "C5",
	Certain:     true,
	Score:       certainScore,
	Specificity: 30,
	Namespaces:  []string{NamespaceOTelService, NamespaceGCPCloudRunService},
	Description: "A Cloud Run service declares an OpenTelemetry service name in a configured " +
		"environment variable or label, an observed service claims that name, and both sides state " +
		"the same environment. Certain because somebody configured it to be true. A missing " +
		"environment on either side does not satisfy it.",
	Eval: evalC5,
}

// c7Namespaces are the namespaces a Cloud SQL connection name can be claimed in, and the list is
// the fix for the way this rule was dead.
//
// It read one namespace on both sides, which made it unfireable by construction rather than for
// want of data: `gcp.sql.instance` is claimed by the GCP feeder and by nothing else, and the rule
// already refuses two claims from the same source. So the only pair it could ever match was one
// that could not exist. It was published, registered, evaluated on every claim, and could not fire
// — the same failure C4 had, arriving by a different route: C4 had one half of its data missing,
// this had the half that existed excluded by the query.
//
// An observed dependency claims in `server.address`, because the address is how the caller reached
// the instance. On Cloud Run and GKE that address is the Unix socket the Cloud SQL connector mounts
// at `/cloudsql/<project>:<region>:<instance>`, so the caller's own address *contains* the
// connection name verbatim — which is precisely the "quoting one configured identifier" this rule
// rests on, seen from the client end. internal/feeders/otel lifts it onto the claim's attributes;
// the two halves are asserted against each other in that package's tests, because this one may not
// import a feeder.
var c7Namespaces = []string{NamespaceGCPSQLInstance, NamespaceServerAddress}

// c7Namespace reports whether a claim namespace can carry a connection name.
func c7Namespace(ns string) bool { return slices.Contains(c7Namespaces, ns) }

var certainRuleC7 = Rule{
	ID:      "C7",
	Certain: true,
	Score:   certainScore,
	// Above C1's 10 and below C3's 20: high enough that a pair C1 also fires on is recorded as C7,
	// and not so high as to claim this is a better explanation than "the instrumentation is running
	// inside this workload". See the file comment.
	Specificity: 15,
	Namespaces:  c7Namespaces,
	Description: "Two sources claim the same Cloud SQL instance connection name " +
		"(<project>:<region>:<instance>), carried as the claim attribute " +
		"sre.gcp.instance_connection_name. The connection name is globally unique and is the " +
		"string every client that reaches the instance is configured with, so two sources " +
		"reporting it are quoting one configured identifier rather than resembling each other.",
	Eval: evalC7,
}

var probableRuleP4 = Rule{
	ID:          "P4",
	Certain:     false,
	Score:       ScoreP4,
	Specificity: 15,
	Namespaces:  []string{NamespaceGCPCloudRunService, NamespaceGCPSQLInstance},
	Description: "A Cloud SQL instance's name appears inside the name of an environment variable a " +
		"Cloud Run service defines. Suggestion only: the variable may be dead configuration, and a " +
		"dependency edge asserted on a variable name is an edge nobody can check.",
	Eval: evalP4,
}

var probableRuleP5 = Rule{
	ID:          "P5",
	Certain:     false,
	Score:       ScoreP5,
	Specificity: 5,
	Namespaces:  []string{NamespaceGCPCloudRunService, NamespaceOTelService, NamespaceK8sDeployment},
	Description: "Two entities share an owning team and their names are similar after " +
		"normalisation. Suggestion only, and the weakest published rule: one team owns many " +
		"similarly named things, which is what a naming convention is for.",
	Eval: evalP5,
}

func init() { Register(certainRuleC4, certainRuleC5, certainRuleC7, probableRuleP4, probableRuleP5) }

// GCPRules returns the five rules this feature publishes, in id order. It is what the contract
// documentation and `aisre rules` print for feature 003.
func GCPRules() []Rule {
	return []Rule{certainRuleC4, certainRuleC5, certainRuleC7, probableRuleP4, probableRuleP5}
}

// evalC4: an observed service says which Cloud Run revision it runs in (FR-117).
//
// It runs in both directions, because either side may be the claim that just arrived: from an
// observed service claim carrying the revision attributes, and from a revision claim to the observed
// services that name it.
//
// The project and region are **required**, not optional corroboration. A revision name is unique
// within a service, not globally (data-model.md §2), so `checkout-00042-abc` in two projects is two
// revisions — and a rule that matched on the revision name alone would merge across projects, which
// is precisely the failure FR-010 exists to prevent.
// It runs from all THREE claims the rule stands on, and the third one is not redundant. The match
// needs the observed claim, the revision claim that links them, and the service claim that is
// merged; whichever arrives last has to be able to complete it, or the rule's answer would depend
// on the order a poll happened to deliver in. A fixture's shuffle step permutes events inside one
// cycle precisely to find that, and a rule that fires only when the service claim is applied before
// the revision claim would produce a different graph under permutation — which is the definition of
// order dependence, in a rule that merges with no human in the loop.
func evalC4(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	switch claim.Namespace {
	case NamespaceOTelService:
		return c4FromObserved(ctx, store, claim)
	case NamespaceGCPCloudRunRevision:
		return c4FromRevision(ctx, store, claim)
	case NamespaceGCPCloudRunService:
		return c4FromService(ctx, store, claim)
	default:
		return nil, nil
	}
}

// c4FromService starts from the Cloud Run service — the claim that is merged — and walks out to the
// revisions that belong to it and the observed services naming those revisions.
//
// It is the arm that makes the rule order-independent. Without it, a poll that applied the revision
// claim before the service claim left the merge unmade for ever: nothing would trigger C4 again.
func c4FromService(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	// The service's ADDRESSING value, `<project>/<region>/<service>` (data-model.md §2). Its other
	// claim is the fully qualified resource name, six segments, and derives nothing — the same shape
	// test c4ServiceOf makes from the other end.
	//
	// The project and region are read from the value here rather than from attributes, because the
	// feeder does not carry the locating three on a SERVICE claim: they identify a revision. That is
	// not a weakening of FR-010. The candidate revisions are selected by a string prefix that
	// already pins project, region and service, and the project and region this rule then matches on
	// are the REVISION claim's own attributes — so a cross-project pair is refused by the same
	// comparison as in the other two arms, not by trusting a parse.
	parts := strings.Split(claim.Value, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, nil
	}
	project, region := parts[0], parts[1]

	revisions, err := store.ClaimsMatchingAttributes(ctx, NamespaceGCPCloudRunRevision, map[string]string{
		AttrGCPProject: project,
		AttrGCPRegion:  region,
	})
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, revision := range revisions {
		// Only this service's revisions, and only its addressing claim: four segments beginning
		// with the service's own three.
		if !strings.HasPrefix(revision.Value, claim.Value+"/") ||
			strings.Count(revision.Value, "/") != 3 {
			continue
		}
		name := revision.Attr(AttrGCPRevisionName)
		if name == "" {
			continue
		}
		observed, err := store.ClaimsMatchingAttributes(ctx, NamespaceOTelService, map[string]string{
			AttrGCPRevisionName: name,
			AttrGCPProject:      project,
			AttrGCPRegion:       region,
		})
		if err != nil {
			return nil, err
		}
		for _, other := range observed {
			if !distinctClaims(other, claim) {
				continue
			}
			matches = append(matches, c4Match(other, revision, claim, name, project, region))
		}
	}
	return matches, nil
}

// c4FromObserved starts from the observed service and looks up the revision it names.
func c4FromObserved(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	revision, project, region := claim.Attr(AttrGCPRevisionName), claim.Attr(AttrGCPProject), claim.Attr(AttrGCPRegion)
	if revision == "" || project == "" || region == "" {
		// Any of the three missing means the claim does not locate a revision. A partial match
		// here would be a merge across projects.
		return nil, nil
	}
	candidates, err := store.ClaimsMatchingAttributes(ctx, NamespaceGCPCloudRunRevision, map[string]string{
		AttrGCPRevisionName: revision,
		AttrGCPProject:      project,
		AttrGCPRegion:       region,
	})
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, other := range candidates {
		service, err := c4ServiceOf(ctx, store, other)
		if err != nil {
			return nil, err
		}
		if service.ClaimID == "" || !distinctClaims(claim, service) {
			continue
		}
		matches = append(matches, c4Match(claim, other, service, revision, project, region))
	}
	return matches, nil
}

// c4FromRevision starts from the revision and looks up the observed services that name it.
func c4FromRevision(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	revision, project, region := claim.Attr(AttrGCPRevisionName), claim.Attr(AttrGCPProject), claim.Attr(AttrGCPRegion)
	if revision == "" || project == "" || region == "" {
		return nil, nil
	}
	candidates, err := store.ClaimsMatchingAttributes(ctx, NamespaceOTelService, map[string]string{
		AttrGCPRevisionName: revision,
		AttrGCPProject:      project,
		AttrGCPRegion:       region,
	})
	if err != nil {
		return nil, err
	}
	service, err := c4ServiceOf(ctx, store, claim)
	if err != nil {
		return nil, err
	}
	if service.ClaimID == "" {
		return nil, nil
	}
	var matches []Match
	for _, other := range candidates {
		if !distinctClaims(other, service) {
			continue
		}
		matches = append(matches, c4Match(other, claim, service, revision, project, region))
	}
	return matches, nil
}

// c4ServiceOf returns the Cloud Run SERVICE claim the revision belongs to, which is the entity C4
// merges the observed service with.
//
// ---------------------------------------------------------------------------------------------
// Why not the revision, which is what this rule merged with until 2026-09-22.
//
// The rationale C4 records has always said "the observed service and the revision's Cloud Run
// service are one entity", and the code merged the observed service with the REVISION. Over a
// single poll the difference is invisible. Over a rollout it is not, and it is not a cosmetic
// difference: the instrumentation reports `faas.version=…-00042-…`, then after a deploy
// `…-00043-…`, both on the one subject `otel.service=checkout`. So C4 fired twice, certain both
// times, merging the observed service with revision 42 and with revision 43 — and two certain
// merges sharing a side put the other two sides together. **The two revisions became one entity**,
// with no human in the loop, which erases the traffic split, the rollout, and the ranking that
// exists to say which revision is failing. US1's entire subject is the distinction C4 was
// dissolving.
//
// The observed service and the Cloud Run service ARE one thing seen twice; the revision is a
// version of it, and a version is not the thing. Merging with the service is idempotent across a
// rollout — every revision of one service derives the same service claim — which is what makes the
// fixed rule safe to leave certain.
//
// The service is derived from the revision's ADDRESSING value, `<project>/<region>/<service>/<revision>`
// (data-model.md §2): drop the last segment. A revision's other claims — the fully qualified resource
// name, the image reference, the image digest — carry the same locating attributes but do not have
// that shape, so they derive nothing and C4 does not fire from them. That is not a gap: the addressing
// claim is emitted for every revision (FR-115), so the rule is reached exactly once per revision
// instead of once per claim about it.
//
// Finding no service claim means NOT FIRING. A certain rule that fell back to the revision when the
// service was not yet in the graph would be a rule whose correctness depended on arrival order.
func c4ServiceOf(ctx context.Context, store ClaimStore, revision Claim) (Claim, error) {
	project, region := revision.Attr(AttrGCPProject), revision.Attr(AttrGCPRegion)
	if project == "" || region == "" {
		return Claim{}, nil
	}
	parts := strings.Split(revision.Value, "/")
	if len(parts) != 4 || parts[0] != project || parts[1] != region || parts[2] == "" {
		// Not the addressing ref. See the comment above: the resource name, the image reference
		// and the digest all land here and derive nothing.
		return Claim{}, nil
	}
	serviceValue := strings.Join(parts[:3], "/")
	claims, err := store.ClaimsFor(ctx, NamespaceGCPCloudRunService, serviceValue)
	if err != nil {
		return Claim{}, err
	}
	for _, candidate := range claims {
		if candidate.EntityID != "" {
			return candidate, nil
		}
	}
	return Claim{}, nil
}

func c4Match(observed, revision, service Claim, revisionName, project, region string) Match {
	return Match{
		RuleID:  "C4",
		Certain: true,
		Score:   certainScore,
		Rationale: fmt.Sprintf(
			"%s observes service %s=%s carrying %s=%s in project %s region %s, and %s knows that "+
				"revision as %s=%s, which belongs to %s=%s; the instrumentation is running inside that "+
				"revision, so the observed service and the revision's Cloud Run service are one entity "+
				"(FR-117)",
			observed.SourceID, observed.Namespace, observed.Value,
			AttrGCPRevisionName, revisionName, project, region,
			revision.SourceID, revision.Namespace, revision.Value,
			service.Namespace, service.Value),
		EntityA: observed.EntityID,
		// The SERVICE, not the revision. See c4ServiceOf.
		EntityB: service.EntityID,
		// The revision claim is cited even though it is not one of the merged entities: it is the
		// evidence, and `resolve why` showing the merge without the claim that caused it would be
		// an explanation with the reason left out.
		SupportingClaimIDs: supporting(observed, revision, service),
	}
}

// evalC5: a declared OTel service name, claimed by an observed service, in the same environment
// (FR-118).
//
// The environment requirement is the whole rule. `checkout` in staging and `checkout` in production
// are different entities, and this is a *certain* rule — it merges without asking. So a missing
// environment on either side does **not** satisfy it, which is stated in FR-118 and asserted in
// gcp_test.go against a claim that omits one. The tempting reading, "if only one side states an
// environment, trust it", is how a staging service gets merged into production topology.
func evalC5(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if claim.Namespace != NamespaceOTelService {
		return nil, nil
	}
	others, err := store.ClaimsFor(ctx, claim.Namespace, claim.Value)
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, other := range others {
		if !distinctClaims(claim, other) {
			continue
		}
		declared, observed, ok := orientC5(claim, other)
		if !ok {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "C5",
			Certain: true,
			Score:   certainScore,
			Rationale: fmt.Sprintf(
				"Cloud Run service claimed by %s declares %s=%s (environment %s), and %s observes that "+
					"service in the same environment; somebody configured it to be true (FR-118)",
				declared.SourceID, NamespaceOTelService, claim.Value, declared.Attr(AttrEnvironment),
				observed.SourceID),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// orientC5 decides which of two claims on one service name is the declaring Cloud Run side, and
// enforces the environment agreement.
func orientC5(a, b Claim) (declared, observed Claim, ok bool) {
	switch {
	case isGCPDeclaredServiceName(a) && !isGCPDeclaredServiceName(b):
		declared, observed = a, b
	case isGCPDeclaredServiceName(b) && !isGCPDeclaredServiceName(a):
		declared, observed = b, a
	default:
		// Neither side, or both sides, is a Cloud Run declaration. Two observed names on one
		// identifier is C1's business.
		return Claim{}, Claim{}, false
	}
	if !sameEnvironment(declared, observed) {
		return Claim{}, Claim{}, false
	}
	return declared, observed, true
}

// isGCPDeclaredServiceName reports whether a claim is the OTel service name a Cloud Run service
// declares, rather than one telemetry reported.
func isGCPDeclaredServiceName(c Claim) bool {
	return c.Attr(AttrGCPDeclaredServiceName) != ""
}

// evalC7: two sources claiming one Cloud SQL instance connection name (FR-120, T130).
//
// The connection name is read from the claim's **attributes**, not from its value, so the two sides
// may address the instance however they like. That is the case the rule is for: one source knows the
// instance as `<project>/<region>/<instance>`, another as `projects/P/instances/I`, and both carry
// `<project>:<region>:<instance>` because that is the string their configuration contains.
//
// Two guards, and each is the difference between a rule and an accident:
//
//   - **two different sources.** One source asserting its own connection name twice is one opinion,
//     not corroboration — and after the first merge both claims point at one entity anyway. This is
//     C1's guard and it is here for C1's reason;
//   - **the connection name has to be shaped like one.** Three colon-separated non-empty parts. A
//     source that emits the attribute as `""`, `":"` or `"::"` would otherwise merge every instance
//     that made the same mistake into one entity, automatically and with a score of 1.0.
func evalC7(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if !c7Namespace(claim.Namespace) {
		return nil, nil
	}
	connection := claim.Attr(AttrGCPSQLConnectionName)
	if !validConnectionName(connection) {
		return nil, nil
	}
	var candidates []Claim
	for _, ns := range c7Namespaces {
		found, err := store.ClaimsMatchingAttributes(ctx, ns, map[string]string{
			AttrGCPSQLConnectionName: connection,
		})
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, found...)
	}
	var matches []Match
	for _, other := range candidates {
		if !distinctClaims(claim, other) || other.SourceID == claim.SourceID {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "C7",
			Certain: true,
			Score:   certainScore,
			Rationale: fmt.Sprintf(
				"sources %s and %s both claim the Cloud SQL instance connection name %s (%s as %s=%s, "+
					"%s as %s=%s); the connection name is globally unique and is the string every client "+
					"that reaches the instance is configured with, so both are quoting one configured "+
					"identifier (FR-120)",
				minStr(claim.SourceID, other.SourceID), maxStr(claim.SourceID, other.SourceID), connection,
				claim.SourceID, claim.Namespace, claim.Value,
				other.SourceID, other.Namespace, other.Value),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// validConnectionName reports whether value has the shape of an instance connection name:
// `<project>:<region>:<instance>` with no empty part.
//
// It is a shape check and not a validation of the parts, because GCP's project, region and instance
// naming rules are GCP's to change. What it rules out is the empty and near-empty strings a source
// emits when it has no connection name and fills the attribute anyway — which, on a *certain* rule,
// would merge every such instance into one entity without asking anybody.
func validConnectionName(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

// evalP4: an instance name inside an environment-variable name (FR-121).
//
// It reads the variable **names** and never their values. A value is configuration, it is classified
// sensitive (FR-034) and it is dropped before anything reaches disk — so a rule that needed values
// would be a rule that could only run against unsanitised data, which means it could never run
// against the corpus.
//
// The substring test is normalised on both sides: `orders-primary` appears in `ORDERS_PRIMARY_DSN` as
// `ORDERS_PRIMARY`, so hyphens, underscores and case are folded before comparing. A bare-name match
// is required to be at least four characters, because a two-letter instance name is inside almost
// every variable name in existence.
func evalP4(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	switch claim.Namespace {
	case NamespaceGCPSQLInstance:
		return p4FromInstance(ctx, store, claim)
	case NamespaceGCPCloudRunService:
		return p4FromService(ctx, store, claim)
	default:
		return nil, nil
	}
}

// MinInstanceNameMatch is the shortest instance name P4 will match inside a variable name. Below it
// the rule fires on coincidence: `db` is inside `DB_HOST`, `DATABASE_URL` and every other variable a
// service defines.
const MinInstanceNameMatch = 4

func p4FromInstance(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	instance := claim.Attr(AttrGCPInstanceName)
	if instance == "" {
		instance = bareName(claim.Value)
	}
	if len(instance) < MinInstanceNameMatch {
		return nil, nil
	}
	candidates, err := store.ClaimsInNamespaces(ctx, []string{NamespaceGCPCloudRunService})
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, service := range candidates {
		if !distinctClaims(claim, service) {
			continue
		}
		if variable, hit := envVarNaming(service, instance); hit {
			matches = append(matches, p4Match(service, claim, instance, variable))
		}
	}
	return matches, nil
}

func p4FromService(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if claim.Attr(AttrGCPEnvVarNames) == "" {
		return nil, nil
	}
	candidates, err := store.ClaimsInNamespaces(ctx, []string{NamespaceGCPSQLInstance})
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, instanceClaim := range candidates {
		if !distinctClaims(claim, instanceClaim) {
			continue
		}
		instance := instanceClaim.Attr(AttrGCPInstanceName)
		if instance == "" {
			instance = bareName(instanceClaim.Value)
		}
		if len(instance) < MinInstanceNameMatch {
			continue
		}
		if variable, hit := envVarNaming(claim, instance); hit {
			matches = append(matches, p4Match(claim, instanceClaim, instance, variable))
		}
	}
	return matches, nil
}

func p4Match(service, instance Claim, instanceName, variable string) Match {
	return Match{
		RuleID:  "P4",
		Certain: false,
		Score:   ScoreP4,
		Rationale: fmt.Sprintf(
			"Cloud Run service %s defines an environment variable named %s, which contains the Cloud "+
				"SQL instance name %s known to %s as %s=%s. Probable only: the variable may be dead "+
				"configuration, and nothing is merged until a person decides (FR-121)",
			service.Value, variable, instanceName, instance.SourceID, instance.Namespace, instance.Value),
		EntityA:            service.EntityID,
		EntityB:            instance.EntityID,
		SupportingClaimIDs: supporting(service, instance),
	}
}

// envVarNaming reports whether any of a service's environment-variable names contains the instance
// name, folding separators and case on both sides.
func envVarNaming(service Claim, instance string) (string, bool) {
	needle := foldSeparators(instance)
	if needle == "" {
		return "", false
	}
	for _, variable := range strings.Fields(service.Attr(AttrGCPEnvVarNames)) {
		if strings.Contains(foldSeparators(variable), needle) {
			return variable, true
		}
	}
	return "", false
}

// foldSeparators lower-cases and removes the separators that differ between a GCP resource name and
// an environment-variable name: `orders-primary` and `ORDERS_PRIMARY` are the same string here.
func foldSeparators(value string) string {
	return strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(value))
}

// bareName returns the last path segment of a slash-separated identifier, which is the instance name
// inside `<project>/<region>/<instance>`.
func bareName(value string) string {
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		return value[idx+1:]
	}
	return value
}

// evalP5: a shared owning team and a similar name (FR-121).
//
// The weakest published rule, and it is here because FR-121 names it. One team owns many similarly
// named things — that is what a naming convention is *for* — so a shared owner alone is almost no
// evidence, and it is required to accompany a normalised-name match rather than stand alone.
//
// It delegates the owner question to the store, because "do these two entities have an `owned_by`
// edge to the same owner" is a question about the graph rather than about claims, and a rule cannot
// answer it alone.
func evalP5(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if !p5Namespace(claim.Namespace) {
		return nil, nil
	}
	normalised := NormalizeName(claim.Value, DefaultNameSuffixes)
	if normalised == "" {
		return nil, nil
	}
	candidates, err := store.ClaimsInNamespaces(ctx, []string{
		NamespaceGCPCloudRunService, NamespaceOTelService, NamespaceK8sDeployment,
	})
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, other := range candidates {
		if !distinctClaims(claim, other) || !p5Namespace(other.Namespace) {
			continue
		}
		if NormalizeName(bareName(other.Value), DefaultNameSuffixes) != NormalizeName(bareName(claim.Value), DefaultNameSuffixes) {
			continue
		}
		if !typesMayMatch(claim.EntityType, other.EntityType) {
			continue
		}
		shared, err := store.SharedOwner(ctx, claim.EntityID, other.EntityID)
		if err != nil {
			return nil, err
		}
		if !shared {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "P5",
			Certain: false,
			Score:   ScoreP5,
			Rationale: fmt.Sprintf(
				"%s=%s and %s=%s normalise to the same name and have an owned_by edge to the same "+
					"owner. Probable only, and the weakest published rule: one team owns many similarly "+
					"named things, which is what a naming convention is for (FR-121)",
				claim.Namespace, claim.Value, other.Namespace, other.Value),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

func p5Namespace(ns string) bool {
	switch ns {
	case NamespaceGCPCloudRunService, NamespaceOTelService, NamespaceK8sDeployment:
		return true
	default:
		return false
	}
}
