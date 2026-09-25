// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The deploy rules (feature 004 T014–T018, FR-045, SC-004;
// specs/004-deploy-feeders/contracts/deploy-claims.md §2).
//
//	C8  certain   two change observations from different sources state the same deploy identifier
//	              for a target they share, in the same environment
//	P7  probable  they state the same deploy identifier but share no target — the monorepo case
//
// SC-004 is "one rollout, not two": a rollout observed by a deploy feeder and by a platform feeder
// appears once in the ranked change list. C8 is the rule that property rests on.
//
// ---------------------------------------------------------------------------------------------
// Why "the same target" is a question about the graph and not about a string
//
// The two sources do not spell a target the same way and never will. GitHub says
// `github.repo=acme/storefront`; Cloud Run says `gcp.cloudrun.service=proj/region/storefront`.
// A rule comparing the identifiers two sources state would be a rule that can never fire — which is
// exactly how C7 shipped: it read one namespace on both sides, so the only pair it could match was
// one that could not exist.
//
// What makes the two the same target is that **resolution already merged them**, by C1, C4 or C5.
// So C8 asks the graph: the entities each change is attached to, after redirects, via
// `ClaimStore.ChangeTargets`. The comparison is **set intersection** — the two changes have a target
// in common — rather than a guess at which of a change's several targets is "the service". 003's
// revision-created change names both the Cloud Run service and the revision, and only the service
// side can ever agree with GitHub's.
//
// # Why the four-way table needs only two of its rows written
//
// The contract's table has four rows, and two of them are structural rather than coded:
//
//	target agrees, identifier agrees        → C8 merges
//	target agrees, identifier DISAGREES     → no pair is ever formed: candidates are looked up by
//	                                          equal value, so two changes shipping different commits
//	                                          never meet here. That is the redeploy case, and the
//	                                          defect 003's C4 shipped with — collapsing a rollout
//	                                          with the one it replaced — is unreachable by
//	                                          construction rather than avoided by a check.
//	target DISAGREES, identifier agrees     → P7 suggests
//	unknown either side                     → no claim, so no candidate. A missing claim is never
//	                                          agreement (FR-041).
//
// # Why the suggestion is a second rule and not a non-certain match from C8
//
// A rule's `Certain` flag says it may merge automatically, and `CertainRules()` is what the
// projector trusts. A single rule that is listed as certain and sometimes emits a suggestion would
// make that listing a half-truth and make a recorded decision's rule id ambiguous about whether it
// merged — the same unreadability `Register`'s duplicate-id panic exists to prevent. So the monorepo
// case is P7, published in its own right. The contract's §2 records this refinement.

// The deploy claim namespaces C8 keys on. `deploy.release` is deliberately absent: a release
// identifier is whatever the platform states, so two sources sharing one is not evidence that they
// are quoting the same thing — `v2.3.0` is a tag many repositories use in the same week.
var deployIdentifierNamespaces = []string{NamespaceDeployCommitSHA, NamespaceDeployImage}

const (
	// NamespaceDeployCommitSHA is the full hex commit object id a rollout shipped.
	NamespaceDeployCommitSHA = graph.CorrelationDeployCommitSHA
	// NamespaceDeployImage is the digest-pinned image reference a rollout deployed.
	NamespaceDeployImage = graph.CorrelationDeployImage
	// NamespaceDeployRelease is the release identifier a rollout shipped. C8 does not key on it — see
	// deployIdentifierNamespaces.
	NamespaceDeployRelease = graph.CorrelationDeployRelease
	// NamespaceGitHubRepo is the repository a change came from. It names one repository, so it is not a
	// correlation namespace (internal/graph/correlation.go) — but claimed on anything else it is shared,
	// which is what C1 is narrowed away from.
	NamespaceGitHubRepo = "github.repo"

	// AttrDeployEnvironment is the environment a change was applied in, the OpenTelemetry
	// `deployment.environment.name`. It is a supporting **attribute** and never part of a claim's
	// value, so one namespace serves production and staging (FR-042).
	AttrDeployEnvironment = "deployment.environment.name"
)

// ScoreP7 is the published confidence of a shared deploy identifier across different targets.
//
// High for a probable rule, because the identifier itself is strong evidence — the two sources are
// quoting one commit — and the only thing in doubt is whether one commit shipped to one target or
// several. That is a question a person answers in a second by looking, which is what a suggestion is
// for.
const ScoreP7 = 0.7

var certainRuleC8 = Rule{
	ID:      "C8",
	Certain: true,
	Score:   certainScore,
	// Above C1's 10, because "both sources name the commit this rollout shipped" is an explanation a
	// reviewer can check against a deployment, while C1's "two sources used the same identifier" is
	// a tautology. Below C3's 20 and C4's 25: those explain a merge by what the thing *is*, and this
	// explains it by what somebody shipped.
	Specificity: 15,
	Namespaces:  deployIdentifierNamespaces,
	Description: "Two change observations from different sources state the same deploy identifier " +
		"(deploy.commit_sha, or deploy.image by digest) for a target they share, in the same " +
		"environment. The identifier is one the platforms quote rather than resemble, and the shared " +
		"target is read from the graph — so the rule rests on a merge resolution already made, not " +
		"on two sources spelling a service name the same way.",
	EvalCorrelation: evalC8,
}

var probableRuleP7 = Rule{
	ID:          "P7",
	Certain:     false,
	Score:       ScoreP7,
	Specificity: 15,
	Namespaces:  deployIdentifierNamespaces,
	Description: "Two change observations from different sources state the same deploy identifier " +
		"but share no target. Suggestion only: one commit shipped to several services is the " +
		"monorepo case, which FR-017 makes N changes rather than one, so merging would erase the " +
		"distinction between them.",
	EvalCorrelation: evalP7,
}

func init() { Register(certainRuleC8, probableRuleP7) }

// DeployRules returns the two rules feature 004 publishes, in id order.
func DeployRules() []Rule { return []Rule{certainRuleC8, probableRuleP7} }

// DeployIdentifierNamespaces returns the namespaces C8 and P7 key on.
//
// It is exported for the projector's re-trigger: rules run when a claim is stored, so a change whose
// TARGET set changes afterwards — a late attachment, or a merge of two target identities — needs its
// deploy claims re-evaluated, and the projector has to know which claims those are without hard-coding
// the strings a second time (T137, contracts/deploy-claims.md §2.1).
func DeployIdentifierNamespaces() []string {
	return append([]string(nil), deployIdentifierNamespaces...)
}

// evalC8 and evalP7 are the same search over different verdicts, so they share one pass and differ
// only in which half of it they report.
func evalC8(ctx context.Context, store ClaimStore, key Correlation) ([]Match, error) {
	merges, _, err := deployPairs(ctx, store, key)
	return merges, err
}

func evalP7(ctx context.Context, store ClaimStore, key Correlation) ([]Match, error) {
	_, suggestions, err := deployPairs(ctx, store, key)
	return suggestions, err
}

// deployPairs finds every other source's change stating the same deploy identifier and splits them
// by whether the two changes share a target.
func deployPairs(ctx context.Context, store ClaimStore, key Correlation) (merges, suggestions []Match, err error) {
	if !slices.Contains(deployIdentifierNamespaces, key.Namespace) {
		return nil, nil, nil
	}
	if !validDeployIdentifier(key.Namespace, key.Value) {
		return nil, nil, nil
	}
	// A deploy key belongs on a CHANGE. One of this shape on anything else is a source saying
	// something the vocabulary does not mean, and merging on it would put a change together with a
	// service.
	if key.EntityType != graph.NodeTypeChange {
		return nil, nil, nil
	}
	environment := strings.TrimSpace(key.Attr(AttrDeployEnvironment))
	if environment == "" {
		// Both sides must STATE an environment. Two unstated environments are not an agreed one, and
		// on a certain rule the cost of treating them as agreed is merging a staging rollout with the
		// production rollout of the same commit — which is the ordinary shape of a promotion.
		return nil, nil, nil
	}

	// Every entity carrying this key, which is where the two kinds differ: an identity lookup returns
	// at most one entity per source because an identifier names one thing, and this returns as many as
	// share the value (004 T148).
	candidates, err := store.CorrelatedWith(ctx, key.Namespace, key.Value)
	if err != nil {
		return nil, nil, err
	}
	mine, err := store.ChangeTargets(ctx, key.EntityID)
	if err != nil {
		return nil, nil, err
	}

	for _, other := range candidates {
		switch {
		case other.EntityID == key.EntityID:
			continue
		case other.SourceID == key.SourceID:
			// One source asserting its own commit twice is one opinion, not corroboration — the
			// guard C1 and C7 carry.
			continue
		case other.EntityType != graph.NodeTypeChange:
			continue
		case strings.TrimSpace(other.Attr(AttrDeployEnvironment)) != environment:
			continue
		}
		theirs, err := store.ChangeTargets(ctx, other.EntityID)
		if err != nil {
			return nil, nil, err
		}
		shared := sharedTargets(mine, theirs)
		if len(shared) > 0 {
			merges = append(merges, deployMatch("C8", certainScore, true, key, other, environment, shared))
			continue
		}
		suggestions = append(suggestions, deployMatch("P7", ScoreP7, false, key, other, environment, nil))
	}
	return merges, suggestions, nil
}

// deployMatch renders one verdict, with the identifier in the rationale rather than the fact that
// two sources agreed: a reviewer can check a commit against a deployment, and cannot check "they
// agreed".
func deployMatch(ruleID string, score float64, certain bool, key, other Correlation, environment string, shared []string) Match {
	var rationale string
	if certain {
		rationale = fmt.Sprintf(
			"sources %s and %s each observed a change shipping %s=%s in environment %s, and the two "+
				"changes share the target %s; the identifier is one both platforms quote rather than "+
				"resemble, and the shared target is a merge resolution already made (FR-045, SC-004)",
			minStr(key.SourceID, other.SourceID), maxStr(key.SourceID, other.SourceID),
			key.Namespace, key.Value, environment, strings.Join(shared, ", "))
	} else {
		rationale = fmt.Sprintf(
			"sources %s and %s each observed a change shipping %s=%s in environment %s, but the two "+
				"changes share no target: one commit shipped to several targets is the monorepo case, "+
				"which is N changes rather than one (FR-017), so this is a suggestion and not a merge",
			minStr(key.SourceID, other.SourceID), maxStr(key.SourceID, other.SourceID),
			key.Namespace, key.Value, environment)
	}
	return Match{
		RuleID:             ruleID,
		Certain:            certain,
		Score:              score,
		Rationale:          rationale,
		EntityA:            key.EntityID,
		EntityB:            other.EntityID,
		SupportingClaimIDs: []string{minStr(key.CorrelationID, other.CorrelationID), maxStr(key.CorrelationID, other.CorrelationID)},
	}
}

// sharedTargets is the intersection, sorted and deduplicated, so a rationale reads the same on a
// replay.
func sharedTargets(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	in := make(map[string]bool, len(b))
	for _, target := range b {
		in[target] = true
	}
	var out []string
	for _, target := range a {
		if in[target] {
			out = append(out, target)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// validDeployIdentifier is the shape check a rule applies to a value it did not mint.
//
// pkg/feeder's normalisers already guarantee these forms, and this repeats them on purpose: the
// claim reaching here came off the log, possibly from a connector outside this repository, and on a
// *certain* rule the cost of admitting a degenerate value is merging every change that carries it.
// It is the same reason C7 re-checks a connection name it did not build.
func validDeployIdentifier(namespace, value string) bool {
	switch namespace {
	case NamespaceDeployCommitSHA:
		return (len(value) == 40 || len(value) == 64) && isLowerHex(value)
	case NamespaceDeployImage:
		name, digest, found := strings.Cut(value, "@")
		if !found || strings.TrimSpace(name) == "" {
			return false
		}
		algorithm, hex, found := strings.Cut(digest, ":")
		return found && algorithm != "" && isLowerHex(hex)
	default:
		return false
	}
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
