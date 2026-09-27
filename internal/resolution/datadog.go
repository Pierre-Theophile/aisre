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
// It runs from either side, so whichever claim arrives second finds the first.
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
	Eval: evalC9,
}

func init() { Register(certainRuleC9) }

func evalC9(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	var other string
	switch claim.Namespace {
	case NamespaceDatadogLogService:
		other = NamespaceOTelService
	case NamespaceOTelService:
		other = NamespaceDatadogLogService
	default:
		return nil, nil
	}
	candidates, err := store.ClaimsFor(ctx, other, claim.Value)
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, candidate := range candidates {
		if !distinctClaims(claim, candidate) || candidate.SourceID == claim.SourceID {
			continue
		}
		if !sameEnvironment(claim, candidate) || !kubernetesAgrees(claim, candidate) {
			continue
		}
		logSide, otelSide := claim, candidate
		if claim.Namespace == NamespaceOTelService {
			logSide, otelSide = candidate, claim
		}
		matches = append(matches, Match{
			RuleID:  "C9",
			Certain: true,
			Score:   certainScore,
			Rationale: fmt.Sprintf(
				"%s states the Datadog log service %s=%s in environment %s, and %s claims %s=%s in the "+
					"same environment; the log service's name is configured to be the service's (FR-059)",
				logSide.SourceID, NamespaceDatadogLogService, logSide.Value, logSide.Attr(AttrEnvironment),
				otelSide.SourceID, NamespaceOTelService, otelSide.Value),
			EntityA:            claim.EntityID,
			EntityB:            candidate.EntityID,
			SupportingClaimIDs: supporting(claim, candidate),
		})
	}
	return matches, nil
}

// kubernetesAgrees holds unless both sides state a Kubernetes namespace, or both a cluster, and they
// differ.
func kubernetesAgrees(a, b Claim) bool {
	for _, attr := range []string{AttrK8sNamespace, AttrK8sCluster} {
		if x, y := a.Attr(attr), b.Attr(attr); x != "" && y != "" && x != y {
			return false
		}
	}
	return true
}
