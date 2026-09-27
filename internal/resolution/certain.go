// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// The certain rules C1, C2 and C3 (research §10).
//
// "Certain" is a strong word here: a certain rule merges two entities with no human in the
// loop, and a wrong merge silently poisons every diff and blast radius downstream
// (constitution VI). Each rule therefore requires an assertion that is only true if someone
// configured it to be true — the same identifier from two independent sources, an annotation
// an operator wrote, a resource attribute the instrumentation set — never a resemblance.
// Resemblance is what the probable rules are for, and they only ever suggest.
//
// Evaluation order is by Specificity: when C2 and C3 both fire on a pair, the merge is
// recorded as C2, because "this workload declares this service name" is the fact an operator
// can check, while C3's resource attributes are a consequence of it.

const certainScore = 1.0

var certainRuleC1 = Rule{
	ID:          "C1",
	Certain:     true,
	Score:       certainScore,
	Specificity: 10,
	Namespaces:  []string{"*"},
	Description: "Two sources assert the same (namespace, value) identifier for different entities. " +
		"The identifier is the same string in the same namespace, so the entities are the same thing " +
		"under one name seen twice.",
	Eval: evalC1,
}

var certainRuleC2 = Rule{
	ID:          "C2",
	Certain:     true,
	Score:       certainScore,
	Specificity: 30,
	Namespaces:  []string{NamespaceOTelService, NamespaceK8sDeployment},
	Description: "A Kubernetes workload declares its OpenTelemetry service name in a configured label or " +
		"annotation (default app.kubernetes.io/name, resource.opentelemetry.io/service.name) and that " +
		"name is claimed by an observed service in the matching namespace (k8s.namespace.name ↔ " +
		"service.namespace) and the same environment.",
	Eval: evalC2,
}

var certainRuleC3 = Rule{
	ID:          "C3",
	Certain:     true,
	Score:       certainScore,
	Specificity: 20,
	Namespaces:  []string{NamespaceOTelService, NamespaceK8sDeployment},
	Description: "A service's spans carry the OpenTelemetry resource attributes k8s.deployment.name and " +
		"k8s.namespace.name, and they name a Kubernetes workload the graph already knows. The " +
		"instrumentation is running inside that workload.",
	Eval: evalC3,
}

// sharedPropertyNamespaces are the published namespaces whose value is a fact SEVERAL entities
// share rather than a name for one, so C1's premise does not hold for them.
//
// C1 says "the same identifier in the same namespace denotes one entity", and for an identifier
// namespace that is true: two sources saying `otel.service.name=checkout` are naming one service.
// For these four it is false, and the consequences are not small:
//
//   - `deploy.commit_sha` — a monorepo run ships one commit to several services, so every change in
//     that run shares the value. C1 would merge them all into one change, certainly, with a score of
//     1.0 and no human in the loop. That is the "merge on the commit alone" alternative
//     research §1.3 rejected, arriving through C1 instead of through C8.
//   - `deploy.image` — the same image runs in staging and in production, and a redeploy of the same
//     digest is a second rollout.
//   - `deploy.release` — `v2.3.0` is a tag many repositories use in the same week.
//   - `github.repo` — a repository is a claim on the service it ships and never a node
//     (004 data-model §2.2), so in a monorepo `checkout` and `web` both claim it. C1 would merge two
//     services because one repository ships both.
//
// They are still claims, and they still do work: C8 keys on the first two, with the agreement on a
// **shared target** that C1 has no way to require. What this list says is only that equality of one
// of these values is not by itself an identity.
//
// The list is deliberately explicit rather than derived from the namespace's prefix. A future
// namespace is non-identifying or not because of what its values mean, and a rule that guessed from
// the spelling would merge on the first namespace somebody named inconsistently.
var sharedPropertyNamespaces = []string{
	NamespaceDeployCommitSHA,
	NamespaceDeployImage,
	NamespaceDeployRelease,
	NamespaceGitHubRepo,
	// A Datadog log service name is the same string in two organisations, or two environments, that
	// mean different services (005 FR-062). C1 would merge them on the value alone; C9 merges only
	// with an environment-agreeing OpenTelemetry service.
	NamespaceDatadogLogService,
}

// IdentifyingNamespace reports whether equality of a value in ns is by itself evidence that two
// entities are one. It is exported so a connector author can check a namespace they are about to
// mint against the rule that will act on it.
func IdentifyingNamespace(ns string) bool { return !slices.Contains(sharedPropertyNamespaces, ns) }

// evalC1: the same identifier, asserted by two sources, currently pointing at two entities.
//
// The rule requires two *different sources*: one source asserting the same identifier twice is
// one opinion, not corroboration, and after the first merge it would be pointing at one entity
// anyway.
//
// And it requires an IDENTIFYING namespace. See sharedPropertyNamespaces: without that guard, the
// deploy vocabulary feature 004 publishes would make C1 merge every change in a monorepo release.
func evalC1(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if !IdentifyingNamespace(claim.Namespace) {
		return nil, nil
	}
	others, err := store.ClaimsFor(ctx, claim.Namespace, claim.Value)
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, other := range others {
		if !distinctClaims(claim, other) || other.SourceID == claim.SourceID {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "C1",
			Certain: true,
			Score:   certainScore,
			Rationale: fmt.Sprintf(
				"sources %s and %s both claim the identifier %s=%s; the same identifier in the same namespace denotes one entity",
				minStr(claim.SourceID, other.SourceID), maxStr(claim.SourceID, other.SourceID),
				claim.Namespace, claim.Value),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// evalC2: a Kubernetes workload and the OpenTelemetry service it declares itself to be.
//
// Both sides claim the identifier `otel.service.name=<x>`, so the candidates come from one
// lookup; what makes it certain rather than a coincidence is that one side read the name from
// a label or annotation an operator set (sre.k8s.claim_key), the other side observed it on
// real spans, and the two agree on namespace and environment.
func evalC2(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
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
		declared, observed, ok := orientC2(claim, other)
		if !ok {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "C2",
			Certain: true,
			Score:   certainScore,
			Rationale: fmt.Sprintf(
				"Kubernetes workload claimed by %s declares %s=%s in %s (namespace %s, environment %s), "+
					"and %s observes that service in service.namespace %s; a workload and the service running on it are one entity",
				declared.SourceID, NamespaceOTelService, claim.Value, declared.Attr(AttrClaimKey),
				declared.Attr(AttrK8sNamespace), declared.Attr(AttrEnvironment),
				observed.SourceID, observed.Attr(AttrServiceNamespace)),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// orientC2 decides which of two claims on the same service name is the declaring Kubernetes
// side and which is the observing telemetry side, and checks the namespace and environment
// agreement the rule requires. It returns ok = false when the pair is not a C2 shape.
func orientC2(a, b Claim) (declared, observed Claim, ok bool) {
	switch {
	case isDeclaredServiceName(a) && !isDeclaredServiceName(b):
		declared, observed = a, b
	case isDeclaredServiceName(b) && !isDeclaredServiceName(a):
		declared, observed = b, a
	default:
		// Neither side, or both sides, read the name off a workload: that is C1's business.
		return Claim{}, Claim{}, false
	}

	k8sNamespace := declared.Attr(AttrK8sNamespace)
	serviceNamespace := observed.Attr(AttrServiceNamespace)
	if k8sNamespace == "" || serviceNamespace == "" || k8sNamespace != serviceNamespace {
		return Claim{}, Claim{}, false
	}
	if !sameEnvironment(declared, observed) {
		return Claim{}, Claim{}, false
	}
	return declared, observed, true
}

// isDeclaredServiceName reports whether a claim carries a service name an operator wrote on
// the workload, rather than one observed on telemetry.
func isDeclaredServiceName(c Claim) bool {
	key := c.Attr(AttrClaimKey)
	return key != "" && slices.Contains(DefaultServiceNameClaimKeys, key)
}

// sameEnvironment requires both sides to state an environment and to agree on it. A missing
// environment is not treated as a match: `checkout` in staging and `checkout` in production
// are different entities, and a certain rule may not gamble on that (FR-007).
func sameEnvironment(a, b Claim) bool {
	env := a.Attr(AttrEnvironment)
	return env != "" && env == b.Attr(AttrEnvironment)
}

// evalC3: spans that say which workload they run in.
//
// Runs in both directions, because either side may be the claim that just arrived: from a
// service claim carrying k8s.deployment.name + k8s.namespace.name to the workload it names,
// and from a workload claim to the service claims that name it.
func evalC3(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	switch claim.Namespace {
	case NamespaceOTelService:
		deployment, namespace := claim.Attr(AttrK8sDeployment), claim.Attr(AttrK8sNamespace)
		if deployment == "" || namespace == "" {
			return nil, nil
		}
		workloads, err := store.ClaimsFor(ctx, NamespaceK8sDeployment, namespace+"/"+deployment)
		if err != nil {
			return nil, err
		}
		var matches []Match
		for _, workload := range workloads {
			if !distinctClaims(claim, workload) {
				continue
			}
			matches = append(matches, c3Match(workload, claim, namespace, deployment))
		}
		return matches, nil

	case NamespaceK8sDeployment:
		namespace, deployment, found := strings.Cut(claim.Value, "/")
		if !found || namespace == "" || deployment == "" {
			return nil, nil
		}
		services, err := store.ClaimsMatchingAttributes(ctx, NamespaceOTelService, map[string]string{
			AttrK8sDeployment: deployment,
			AttrK8sNamespace:  namespace,
		})
		if err != nil {
			return nil, err
		}
		var matches []Match
		for _, service := range services {
			if !distinctClaims(claim, service) {
				continue
			}
			matches = append(matches, c3Match(claim, service, namespace, deployment))
		}
		return matches, nil

	default:
		return nil, nil
	}
}

func c3Match(workload, service Claim, namespace, deployment string) Match {
	return Match{
		RuleID:  "C3",
		Certain: true,
		Score:   certainScore,
		Rationale: fmt.Sprintf(
			"spans claimed by %s for %s=%s carry the resource attributes %s=%s and %s=%s, which name the "+
				"Kubernetes workload claimed by %s; the instrumentation runs inside that workload",
			service.SourceID, NamespaceOTelService, service.Value,
			AttrK8sDeployment, deployment, AttrK8sNamespace, namespace, workload.SourceID),
		EntityA:            workload.EntityID,
		EntityB:            service.EntityID,
		SupportingClaimIDs: supporting(workload, service),
	}
}

// distinctClaims reports whether two claims are different claims about different entities,
// which is the precondition of every rule: a claim never matches itself, and two claims
// already pointing at one entity have nothing left to merge.
func distinctClaims(a, b Claim) bool {
	return a.ClaimID != b.ClaimID && a.EntityID != b.EntityID && a.EntityID != "" && b.EntityID != ""
}

// supporting returns the claim ids a match stands on, deduplicated and ordered so that a
// recorded decision is byte-identical on replay (FR-023, FR-038).
func supporting(claims ...Claim) []string {
	ids := make([]string, 0, len(claims))
	for _, c := range claims {
		if c.ClaimID != "" && !slices.Contains(ids, c.ClaimID) {
			ids = append(ids, c.ClaimID)
		}
	}
	slices.Sort(ids)
	return ids
}

func minStr(a, b string) string {
	if a <= b {
		return a
	}
	return b
}

func maxStr(a, b string) string {
	if a >= b {
		return a
	}
	return b
}
