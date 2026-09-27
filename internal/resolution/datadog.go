// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"context"
	"fmt"
)

// C9: a Datadog log service is an OpenTelemetry service, in the same environment (005 FR-059,
// ADR-0010 item 4; specs/005-datadog-connector/data-model.md §4).
//
// A service's logs in Datadog and the same service reported by Cloud Run, Kubernetes or OpenTelemetry
// are one entity when the names are equal and both sides state the same environment. It is certain,
// not probable, because a Datadog service name for a log source is configured to be the service's
// name — DD_SERVICE, or OpenTelemetry's service.name as Datadog maps it — so equality within an
// environment is an assertion somebody made, not a resemblance.
//
// Three things are deliberate:
//
//   - The environment is the whole rule, as in C5. `checkout` in staging and in production are
//     different entities, and a missing environment on either side does not satisfy it. It is why
//     Datadog does not claim otel.service.name directly: C1 would merge on the name alone.
//   - The names compare exactly. Datadog lowercases service names, so a mixed-case OpenTelemetry
//     name will not match. A case-insensitive match from one side only would make the outcome depend
//     on which claim arrived first; exact equality is symmetric, and the near miss is left to the
//     probable rules as a suggestion.
//   - Where both sides state a Kubernetes namespace or cluster, those must agree too.
//
// It is a cross-kind rule: the Datadog side is a correlation key (a service name the same source
// states for its production and its staging node is shared, not owned — see
// internal/feeders/datadog/logsources.go) and the OpenTelemetry side is an identity claim. It runs from
// both, and each half looks up only the other kind, so the pair is found once by whichever arrives
// second (Rule.CrossKind).
var certainRuleC9 = Rule{
	ID:          "C9",
	Certain:     true,
	Score:       certainScore,
	Specificity: 30,
	Namespaces:  []string{NamespaceDatadogLogService, NamespaceOTelService},
	Description: "A Datadog log service and an OpenTelemetry service (observed, or a platform's " +
		"declared name) carry the same name, both state the same environment, and any Kubernetes " +
		"namespace or cluster both state agrees. Certain because the log service's name is configured " +
		"to be the service's. A missing environment on either side does not satisfy it.",
	Eval:            evalC9FromClaim,
	EvalCorrelation: evalC9FromCorrelation,
	CrossKind:       true,
}

func init() { Register(certainRuleC9) }

// c9Side is what C9 compares, from either kind.
type c9Side struct {
	entity, source, value, env, evidence string
	attr                                 func(string) string
}

func evalC9FromClaim(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if claim.Namespace != NamespaceOTelService {
		return nil, nil
	}
	logs, err := store.CorrelatedWith(ctx, NamespaceDatadogLogService, claim.Value)
	if err != nil {
		return nil, err
	}
	otel := c9Side{claim.EntityID, claim.SourceID, claim.Value, claim.Attr(AttrEnvironment), claim.ClaimID, claim.Attr}
	var matches []Match
	for _, c := range logs {
		log := c9Side{c.EntityID, c.SourceID, c.Value, c.Attr(AttrEnvironment), c.CorrelationID, c.Attr}
		if m, ok := c9Match(log, otel, claim.EntityID); ok {
			matches = append(matches, m)
		}
	}
	return matches, nil
}

func evalC9FromCorrelation(ctx context.Context, store ClaimStore, key Correlation) ([]Match, error) {
	if key.Namespace != NamespaceDatadogLogService {
		return nil, nil
	}
	claims, err := store.ClaimsFor(ctx, NamespaceOTelService, key.Value)
	if err != nil {
		return nil, err
	}
	log := c9Side{key.EntityID, key.SourceID, key.Value, key.Attr(AttrEnvironment), key.CorrelationID, key.Attr}
	var matches []Match
	for _, c := range claims {
		otel := c9Side{c.EntityID, c.SourceID, c.Value, c.Attr(AttrEnvironment), c.ClaimID, c.Attr}
		if m, ok := c9Match(log, otel, key.EntityID); ok {
			matches = append(matches, m)
		}
	}
	return matches, nil
}

// c9Match applies the rule to one log side and one OpenTelemetry side. trigger is the entity of the
// event being evaluated, which becomes EntityA as every rule records it.
func c9Match(log, otel c9Side, trigger string) (Match, bool) {
	switch {
	case log.entity == otel.entity, log.source == otel.source, log.value != otel.value:
		return Match{}, false
	case log.env == "" || log.env != otel.env:
		return Match{}, false
	}
	for _, attr := range []string{AttrK8sNamespace, AttrK8sCluster} {
		if x, y := log.attr(attr), otel.attr(attr); x != "" && y != "" && x != y {
			return Match{}, false
		}
	}
	a, b := log.entity, otel.entity
	if trigger == otel.entity {
		a, b = otel.entity, log.entity
	}
	return Match{
		RuleID:  "C9",
		Certain: true,
		Score:   certainScore,
		Rationale: fmt.Sprintf(
			"%s states the Datadog log service %s=%s in environment %s, and %s claims %s=%s in the "+
				"same environment; the log service's name is configured to be the service's (FR-059)",
			log.source, NamespaceDatadogLogService, log.value, log.env,
			otel.source, NamespaceOTelService, otel.value),
		EntityA:            a,
		EntityB:            b,
		SupportingClaimIDs: []string{minStr(log.evidence, otel.evidence), maxStr(log.evidence, otel.evidence)},
	}, true
}
