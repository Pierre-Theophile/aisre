// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The probable rules P1, P2 and P3 (research §10, FR-037, ADR-0001 D6).
//
// A probable rule never merges anything. It says "these two look like the same thing, and here
// is how much I would bet on it", and the graph stores that as a suggestion for a person to
// decide (SC-007: zero automated merges from probable rules). That is the whole difference
// between this file and certain.go, and it is why the evidence a probable rule stands on is
// allowed to be a resemblance rather than a configured fact: `checkout-svc` and `checkout`
// probably are one service, and a resolver that merged them on that basis would be wrong often
// enough to poison every downstream diff.
//
// The three rules share one comparison — normalized-name equality across a Kubernetes name and
// an observed telemetry service name — and differ in what corroborates it:
//
//	P1  normalized names are equal                     0.70
//	P3  P1 and both entities have the same owner       0.80
//	P2  P1 and both entities state the same environment 0.85
//
// The scores are published constants, not tuned parameters. They are calibrated against fixture
// ground truth by `fixture verify --report` (constitution V), and a change to one of them is a
// schema change that shows up in a pull request diff.

// Published probable scores (research §10).
const (
	// ScoreP1 is normalized-name equality alone.
	ScoreP1 = 0.7
	// ScoreP2 is normalized-name equality plus an agreeing environment. It is the highest
	// probable score because the environment is the attribute that most often separates two
	// genuinely different entities that share a name (`checkout` in staging and in prod).
	ScoreP2 = 0.85
	// ScoreP3 is normalized-name equality plus a shared owner.
	ScoreP3 = 0.8
)

// AttrOwner is not an attribute: P3's corroboration is an `owned_by` edge in the graph, read
// through ClaimStore.SharedOwner, because ownership is a relationship the Kubernetes feeder
// asserts as an edge rather than as a claim attribute (research §12).

// DefaultNameSuffixes are stripped from a name before two names are compared (research §10,
// "stripping configured suffixes"). They are the suffixes operators add to a Kubernetes object
// to say what kind of object it is, which carries no identity: `checkout-svc`, `checkout-deploy`
// and `checkout` are three spellings of one name.
//
// A deployment may configure its own list; this is the default every fixture uses and the one
// docs/schema/resolution.md publishes.
var DefaultNameSuffixes = []string{"-svc", "-service", "-deploy", "-deployment", "-app"}

// probableNamespaces are the identifier namespaces the probable rules read candidates from.
var probableNamespaces = []string{NamespaceOTelService, NamespaceK8sDeployment}

// nodeTypePrecedence is the published type precedence (research §10, ADR-0001 D7), highest
// first. It is repeated here rather than imported from the projector because it is part of the
// *published rule set*: the type guard below is a statement about which entities a probable
// rule may pair, and a reader of the rules must be able to check it without reading the
// projector.
var nodeTypePrecedence = []graph.NodeType{
	graph.NodeTypeService,
	graph.NodeTypeWorkload,
	graph.NodeTypeThirdParty,
	graph.NodeTypeInfraResource,
	graph.NodeTypeDBSchema,
	graph.NodeTypeConfig,
	graph.NodeTypeFeatureFlag,
	graph.NodeTypeOwner,
	graph.NodeTypeAlert,
	graph.NodeTypeChange,
}

var probableRuleP1 = Rule{
	ID:          "P1",
	Certain:     false,
	Score:       ScoreP1,
	Specificity: 10,
	Namespaces:  []string{NamespaceOTelService, NamespaceK8sDeployment},
	Description: "A Kubernetes name and an observed OpenTelemetry service name are equal after " +
		"normalization: the last path segment, case-folded, with separators unified, configured " +
		"suffixes (-svc, -service, -deploy, -deployment, -app) stripped and non-alphanumerics removed.",
	Eval: evalP1,
}

var probableRuleP2 = Rule{
	ID:          "P2",
	Certain:     false,
	Score:       ScoreP2,
	Specificity: 30,
	Namespaces:  []string{NamespaceOTelService, NamespaceK8sDeployment},
	Description: "P1 and both sides state the same deployment environment. Two things that share a " +
		"normalized name in one environment are much more likely to be one thing than two that " +
		"share it across environments.",
	Eval: evalP2,
}

var probableRuleP3 = Rule{
	ID:          "P3",
	Certain:     false,
	Score:       ScoreP3,
	Specificity: 20,
	Namespaces:  []string{NamespaceOTelService, NamespaceK8sDeployment},
	Description: "P1 and both entities have an owned_by edge to the same owner entity. A shared owner " +
		"is weaker evidence than a shared environment because one team owns many services.",
	Eval: evalP3,
}

// init registers the probable rules with the published registry. rules.go deliberately leaves
// the registry open so that US6 can add P1..P3 without editing the file that defines C1..C3.
func init() { Register(probableRuleP1, probableRuleP2, probableRuleP3) }

// ProbableRules returns the rules that may only suggest, most specific first, ties broken by
// id. It is the probable half of CertainRules and has the same contract.
func ProbableRules() []Rule {
	out := make([]Rule, 0, len(registry))
	for _, rule := range registry {
		if !rule.Certain {
			out = append(out, rule)
		}
	}
	slices.SortFunc(out, compareBySpecificity)
	return out
}

// NormalizeName reduces a name to the form the probable rules compare (research §10).
//
// The steps, in order, and why each one:
//
//  1. the last `/`-separated segment — a `k8s.deployment` identifier is `<namespace>/<name>`
//     and the namespace is not part of the name;
//  2. case-folding — Kubernetes names are lower-case, telemetry service names often are not;
//  3. every run of non-alphanumeric characters becomes a single `-`, so `checkout_svc` and
//     `checkout.svc` compare as `checkout-svc`;
//  4. configured suffixes are stripped, repeatedly, so `checkout-svc-deploy` reduces to
//     `checkout`; a strip that would empty the name is refused, because `svc` is a name;
//  5. the remaining separators are removed, so `check-out` and `checkout` compare equal.
//
// The empty string is returned for a name that normalizes to nothing, and a caller must treat
// that as "no comparable name" rather than as a name two things can share.
func NormalizeName(value string, suffixes []string) string {
	name := value
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	name = strings.ToLower(strings.TrimSpace(name))

	var separated strings.Builder
	lastWasSeparator := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			separated.WriteRune(r)
			lastWasSeparator = false
			continue
		}
		if !lastWasSeparator && separated.Len() > 0 {
			separated.WriteByte('-')
		}
		lastWasSeparator = true
	}
	name = strings.TrimRight(separated.String(), "-")

	for {
		stripped := false
		for _, suffix := range suffixes {
			trimmed, ok := strings.CutSuffix(name, suffix)
			if !ok || trimmed == "" {
				continue
			}
			name = strings.TrimRight(trimmed, "-")
			stripped = name != ""
			break
		}
		if !stripped {
			break
		}
	}
	return strings.ReplaceAll(name, "-", "")
}

// evalP1 fires on normalized-name equality alone.
func evalP1(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	return probableMatches(ctx, store, claim, "P1", ScoreP1, nil)
}

// evalP2 fires on normalized-name equality plus an agreeing environment. Both sides must state
// one: a missing environment is not agreement, for the same reason C2 refuses it.
func evalP2(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	return probableMatches(ctx, store, claim, "P2", ScoreP2,
		func(_ context.Context, _ ClaimStore, a, b Claim) (bool, string, error) {
			if !sameEnvironment(a, b) {
				return false, "", nil
			}
			return true, fmt.Sprintf("both state environment %s", a.Attr(AttrEnvironment)), nil
		})
}

// evalP3 fires on normalized-name equality plus a shared owner, which is a fact about the graph
// rather than about the claims, so it is read through the store.
func evalP3(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	return probableMatches(ctx, store, claim, "P3", ScoreP3,
		func(ctx context.Context, store ClaimStore, a, b Claim) (bool, string, error) {
			shared, err := store.SharedOwner(ctx, a.EntityID, b.EntityID)
			if err != nil || !shared {
				return false, "", err
			}
			return true, "both are owned by the same owner entity", nil
		})
}

// corroboration is the extra condition a probable rule requires beyond normalized-name
// equality. It returns whether the condition holds and the clause to put in the rationale.
type corroboration func(ctx context.Context, store ClaimStore, a, b Claim) (bool, string, error)

// probableMatches is the body every probable rule shares: find the claims whose names normalize
// to the same thing as claim's, on the other side of the Kubernetes/telemetry divide, and keep
// the ones the rule's own corroboration accepts.
//
// The candidate scan reads every claim in the two namespaces the rules compare. That is honest
// about what the rule does — normalized equality is not an indexable predicate without storing
// the normalized form — and it is bounded by the number of distinct service and workload names,
// not by the size of the graph. If it ever becomes the bottleneck, the fix is a stored
// normalized column, not a cheaper rule.
func probableMatches(ctx context.Context, store ClaimStore, claim Claim, ruleID string, score float64, also corroboration) ([]Match, error) {
	side, ok := nameSideOf(claim)
	if !ok {
		return nil, nil
	}
	normalized := NormalizeName(claim.Value, DefaultNameSuffixes)
	if normalized == "" {
		return nil, nil
	}

	candidates, err := store.ClaimsInNamespaces(ctx, probableNamespaces)
	if err != nil {
		return nil, err
	}

	var matches []Match
	for _, other := range candidates {
		if !distinctClaims(claim, other) {
			continue
		}
		otherSide, ok := nameSideOf(other)
		if !ok || otherSide == side {
			// One telemetry service name resembling another telemetry service name is two
			// services with similar names, which is the normal state of a system. The rules
			// compare a name Kubernetes knows with a name telemetry reports.
			continue
		}
		if NormalizeName(other.Value, DefaultNameSuffixes) != normalized {
			continue
		}
		if !typesMayMatch(claim.EntityType, other.EntityType) {
			continue
		}
		extra := ""
		if also != nil {
			held, clause, err := also(ctx, store, claim, other)
			if err != nil {
				return nil, err
			}
			if !held {
				continue
			}
			extra = "; " + clause
		}

		kubernetes, observed := claim, other
		if side == nameSideObserved {
			kubernetes, observed = other, claim
		}
		matches = append(matches, Match{
			RuleID:  ruleID,
			Certain: false,
			Score:   score,
			Rationale: fmt.Sprintf(
				"%s names %s=%s and %s observes %s=%s; both normalize to %q%s. "+
					"Probable only: nothing is merged until a person decides (FR-037)",
				kubernetes.SourceID, kubernetes.Namespace, kubernetes.Value,
				observed.SourceID, observed.Namespace, observed.Value, normalized, extra),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// The two sides a comparable name can come from.
const (
	// nameSideKubernetes is a name an operator gave a Kubernetes object: a `k8s.deployment`
	// identifier, or a service name read off a label or annotation (sre.k8s.claim_key).
	nameSideKubernetes = "kubernetes"
	// nameSideObserved is a service name telemetry reported.
	nameSideObserved = "observed"
)

// nameSideOf classifies a claim, reporting false for one the probable rules do not compare.
func nameSideOf(c Claim) (string, bool) {
	switch c.Namespace {
	case NamespaceK8sDeployment:
		return nameSideKubernetes, true
	case NamespaceOTelService:
		if isDeclaredServiceName(c) {
			return nameSideKubernetes, true
		}
		return nameSideObserved, true
	default:
		return "", false
	}
}

// typesMayMatch applies the published type guard (research §10): "two entities whose asserted
// types are both below WORKLOAD and differ are never auto-merged by probable rules".
//
// Above and at WORKLOAD the guard does not apply, because that is exactly the case the graph
// exists to merge: a WORKLOAD and the SERVICE running on it are one entity (ADR-0001 D7). Below
// it the types are unrelated kinds of thing — a CONFIG is not an OWNER — and a resemblance
// between their names is a coincidence, not evidence.
//
// An unknown type on either side is not a reason to refuse: an entity nothing has described yet
// carries a provisional type from its namespace, and refusing would make the suggestion depend
// on which event arrived first.
func typesMayMatch(a, b graph.NodeType) bool {
	if a == b || a == graph.NodeTypeUnspecified || b == graph.NodeTypeUnspecified {
		return true
	}
	return !belowWorkload(a) || !belowWorkload(b)
}

func belowWorkload(t graph.NodeType) bool {
	rank := slices.Index(nodeTypePrecedence, t)
	workload := slices.Index(nodeTypePrecedence, graph.NodeTypeWorkload)
	return rank > workload
}
