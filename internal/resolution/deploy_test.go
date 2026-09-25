// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// C8 and P7, the deploy rules (004 T014–T018, T148, FR-045, SC-004).
//
// SC-004 is "one rollout, not two". These tests are written the way 003's taught us to write them:
// every case that must NOT merge is asserted beside the one that must, because a certain rule merges
// with no human in the loop and its false positives are the expensive kind.
//
// They are driven through EvaluateCorrelation and not Evaluate, because a deploy identifier is a
// CORRELATION KEY and not an identity claim (T148). The distinction is the point of the rule: a commit
// is shared by every change in a monorepo run, and a store keyed on "one identifier names one entity"
// could hold it for exactly one of them.

const (
	deployCommit      = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	otherDeployCommit = "1122334455667788990011223344556677889900"
	storefront        = "entity-service-storefront"
)

// deployKey is one source's commit key on a change.
func deployKey(id, source, changeEntity, commit, environment string) resolution.Correlation {
	return resolution.Correlation{
		CorrelationID: id, EntityID: changeEntity, EntityType: graph.NodeTypeChange,
		SourceID: source, EventID: source + ":event", AppendedSeq: 10,
		Namespace: resolution.NamespaceDeployCommitSHA, Value: commit,
		Attributes: map[string]string{resolution.AttrDeployEnvironment: environment},
	}
}

// One rollout, two sources: the property this whole feature exists to deliver.
func TestC8MergesTwoSourcesObservingOneRollout(t *testing.T) {
	t.Parallel()

	github := deployKey("key-gh", "github:acme", "entity-change-gh", deployCommit, "production")
	cloudrun := deployKey("key-gcp", "gcp:acme", "entity-change-gcp", deployCommit, "production")
	store := fakeStore{
		correlations: []resolution.Correlation{github, cloudrun},
		targets: map[string][]string{
			"entity-change-gh":  {storefront, "entity-repo"},
			"entity-change-gcp": {storefront, "entity-revision-42"},
		},
	}

	matches, err := resolution.EvaluateCorrelation(context.Background(), store, github)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	match, ok := matchOf(matches, "C8")
	if !ok {
		t.Fatalf("C8 did not fire on two sources observing one rollout; matches: %v", matchRuleIDs(matches))
	}
	if !match.Certain || match.Score != 1.0 {
		t.Errorf("C8 match is certain=%v score=%v, want certain 1.0", match.Certain, match.Score)
	}
	// The rationale names the identifier a reviewer can check against a deployment, and the target
	// the two changes share, rather than the fact that two sources agreed.
	for _, want := range []string{deployCommit, "production", storefront} {
		if !strings.Contains(match.Rationale, want) {
			t.Errorf("C8's rationale does not name %q: %s", want, match.Rationale)
		}
	}
	if len(match.SupportingClaimIDs) != 2 {
		t.Errorf("C8 stood on %d keys, want both sides: %v", len(match.SupportingClaimIDs), match.SupportingClaimIDs)
	}
	// And P7 does not also fire: a pair a certain rule matched is never reported as a suggestion.
	if _, ok := matchOf(matches, "P7"); ok {
		t.Error("P7 also fired on a pair C8 merged; a suggestion to merge what is merged is noise")
	}
}

// Target agrees, commit does not: a redeploy, which is two rollouts. This is the defect 003's C4
// shipped with — collapsing a rollout with the one it replaced.
func TestC8DoesNotMergeARedeploy(t *testing.T) {
	t.Parallel()

	first := deployKey("key-gh", "github:acme", "entity-change-gh", deployCommit, "production")
	second := deployKey("key-gcp", "gcp:acme", "entity-change-gcp", otherDeployCommit, "production")
	store := fakeStore{
		correlations: []resolution.Correlation{first, second},
		targets: map[string][]string{
			"entity-change-gh":  {storefront},
			"entity-change-gcp": {storefront},
		},
	}

	matches, err := resolution.EvaluateCorrelation(context.Background(), store, first)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{"C8", "P7"} {
		if _, ok := matchOf(matches, id); ok {
			t.Errorf("%s fired on two different commits shipped to one target; that is a redeploy, "+
				"and merging it collapses a rollout with the one it replaced", id)
		}
	}
}

// Commit agrees, target does not: the monorepo case, which FR-017 makes N changes. It is a
// suggestion and never a merge.
func TestP7SuggestsTheMonorepoCaseAndC8RefusesIt(t *testing.T) {
	t.Parallel()

	github := deployKey("key-gh", "github:acme", "entity-change-gh", deployCommit, "production")
	vercel := deployKey("key-vercel", "vercel:acme", "entity-change-vercel", deployCommit, "production")
	store := fakeStore{
		correlations: []resolution.Correlation{github, vercel},
		targets: map[string][]string{
			"entity-change-gh":     {"entity-service-checkout"},
			"entity-change-vercel": {"entity-service-web"},
		},
	}

	matches, err := resolution.EvaluateCorrelation(context.Background(), store, github)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := matchOf(matches, "C8"); ok {
		t.Error("C8 merged one commit shipped to two targets; FR-017 makes that N changes, and a " +
			"certain merge here would erase the distinction between them")
	}
	match, ok := matchOf(matches, "P7")
	if !ok {
		t.Fatalf("P7 did not suggest the monorepo case; matches: %v", matchRuleIDs(matches))
	}
	if match.Certain {
		t.Error("P7's match is certain; nothing merges without a person (ADR-0001 D6)")
	}
	if !strings.Contains(match.Rationale, "monorepo") {
		t.Errorf("P7's rationale does not say what the pair is: %s", match.Rationale)
	}
}

// A missing claim is not agreement, and neither is a missing environment or a missing target.
func TestC8RefusesEveryFormOfNotKnowing(t *testing.T) {
	t.Parallel()

	base := func() (resolution.Correlation, resolution.Correlation, map[string][]string) {
		return deployKey("key-gh", "github:acme", "entity-change-gh", deployCommit, "production"),
			deployKey("key-gcp", "gcp:acme", "entity-change-gcp", deployCommit, "production"),
			map[string][]string{
				"entity-change-gh":  {storefront},
				"entity-change-gcp": {storefront},
			}
	}

	for name, mutate := range map[string]func(*resolution.Correlation, *resolution.Correlation, map[string][]string){
		"the arriving side states no environment": func(a, _ *resolution.Correlation, _ map[string][]string) {
			a.Attributes = nil
		},
		"the other side states no environment": func(_, b *resolution.Correlation, _ map[string][]string) {
			b.Attributes = nil
		},
		"the environments differ": func(_, b *resolution.Correlation, _ map[string][]string) {
			b.Attributes[resolution.AttrDeployEnvironment] = "staging"
		},
		"neither change has a target yet": func(_, _ *resolution.Correlation, targets map[string][]string) {
			delete(targets, "entity-change-gh")
			delete(targets, "entity-change-gcp")
		},
		"both keys come from one source": func(_, b *resolution.Correlation, _ map[string][]string) {
			b.SourceID = "github:acme"
		},
		"the key is on a service rather than a change": func(a, _ *resolution.Correlation, _ map[string][]string) {
			a.EntityType = graph.NodeTypeService
		},
		"the other key is on a service rather than a change": func(_, b *resolution.Correlation, _ map[string][]string) {
			b.EntityType = graph.NodeTypeService
		},
		"the commit is an abbreviation": func(a, b *resolution.Correlation, _ map[string][]string) {
			a.Value, b.Value = deployCommit[:12], deployCommit[:12]
		},
		"the commit is not hex": func(a, b *resolution.Correlation, _ map[string][]string) {
			a.Value, b.Value = strings.Repeat("g", 40), strings.Repeat("g", 40)
		},
		"the commit is upper-cased": func(a, b *resolution.Correlation, _ map[string][]string) {
			a.Value, b.Value = strings.ToUpper(deployCommit), strings.ToUpper(deployCommit)
		},
	} {
		arriving, other, targets := base()
		mutate(&arriving, &other, targets)
		store := fakeStore{correlations: []resolution.Correlation{arriving, other}, targets: targets}

		matches, err := resolution.EvaluateCorrelation(context.Background(), store, arriving)
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", name, err)
		}
		if _, ok := matchOf(matches, "C8"); ok {
			t.Errorf("C8 merged although %s; a certain rule merges with no human in the loop", name)
		}
	}
}

// The image identifier works the same way, and only in its digest form: a tag is mutable, and two
// rollouts sharing one would merge into a single change.
func TestC8MergesOnAnImageDigestAndRefusesATag(t *testing.T) {
	t.Parallel()

	const digest = "eu.pkg.dev/acme/apps/storefront@sha256:" +
		"1f2e3d4c5b6a798807162534435261708f9e0d1c2b3a49586776859403f2e1d0"

	imageKey := func(id, source, changeEntity, value string) resolution.Correlation {
		key := deployKey(id, source, changeEntity, value, "production")
		key.Namespace = resolution.NamespaceDeployImage
		return key
	}
	targets := map[string][]string{
		"entity-change-gh":  {storefront},
		"entity-change-gcp": {storefront},
	}

	pinned := imageKey("key-gh", "github:acme", "entity-change-gh", digest)
	store := fakeStore{
		correlations: []resolution.Correlation{pinned, imageKey("key-gcp", "gcp:acme", "entity-change-gcp", digest)},
		targets:      targets,
	}
	matches, err := resolution.EvaluateCorrelation(context.Background(), store, pinned)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := matchOf(matches, "C8"); !ok {
		t.Errorf("C8 did not fire on a shared image digest; matches: %v", matchRuleIDs(matches))
	}

	for _, unpinned := range []string{
		"eu.pkg.dev/acme/apps/storefront:latest",
		"eu.pkg.dev/acme/apps/storefront",
		"eu.pkg.dev/acme/apps/storefront@latest",
		"@sha256:1f2e",
	} {
		tagged := imageKey("key-gh", "github:acme", "entity-change-gh", unpinned)
		store := fakeStore{
			correlations: []resolution.Correlation{tagged, imageKey("key-gcp", "gcp:acme", "entity-change-gcp", unpinned)},
			targets:      targets,
		}
		matches, err := resolution.EvaluateCorrelation(context.Background(), store, tagged)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if _, ok := matchOf(matches, "C8"); ok {
			t.Errorf("C8 merged on %q, which pins no digest; two rollouts sharing a tag are not one "+
				"rollout", unpinned)
		}
	}
}

// deploy.release is deliberately not a C8 identifier: `v2.3.0` is a tag many repositories use in the
// same week, so two sources sharing one is not evidence that they are quoting the same thing.
func TestC8DoesNotKeyOnAReleaseIdentifier(t *testing.T) {
	t.Parallel()

	release := func(id, source, changeEntity string) resolution.Correlation {
		key := deployKey(id, source, changeEntity, "v2.3.0", "production")
		key.Namespace = "deploy.release"
		return key
	}
	arriving := release("key-gh", "github:acme", "entity-change-gh")
	store := fakeStore{
		correlations: []resolution.Correlation{arriving, release("key-gcp", "gcp:acme", "entity-change-gcp")},
		targets: map[string][]string{
			"entity-change-gh":  {storefront},
			"entity-change-gcp": {storefront},
		},
	}
	matches, err := resolution.EvaluateCorrelation(context.Background(), store, arriving)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{"C8", "P7"} {
		if _, ok := matchOf(matches, id); ok {
			t.Errorf("%s fired on a shared release identifier, which is not a published C8 identifier", id)
		}
	}
	if !namespacesInclude(resolution.DeployRules()[0].Namespaces, resolution.NamespaceDeployCommitSHA) {
		t.Error("C8 does not publish deploy.commit_sha among its namespaces")
	}
}

// C1 must not treat the deploy vocabulary as an identity.
//
// This is the defect registering the seven namespaces made reachable, and it is worse than the one
// C8 was written to avoid. C1 fires on ANY namespace — `Namespaces: ["*"]` — so two sources sharing a
// `deploy.commit_sha` were a certain merge with no target check at all: every change in a monorepo
// release collapsing into one, score 1.0, no human in the loop. `github.repo` is worse still, because
// a repository is a claim on the service it ships: in a monorepo, two SERVICES share the value.
//
// T148 added a second and stronger guard — the event log now refuses a claim in one of these
// namespaces outright (internal/log, ReasonCorrelationAsIdentity) — and this one stays, for two
// reasons. The log is append-only: a database replaying events appended before that refusal existed
// still has such claims to project. And a guard that exists only at the front door is a guard that
// stops applying the moment anything writes to the projection by another route.
func TestC1DoesNotMergeOnAValueSeveralEntitiesShare(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		namespace string
		value     string
		typ       graph.NodeType
		why       string
	}{
		{"a monorepo release's commit", resolution.NamespaceDeployCommitSHA, deployCommit,
			graph.NodeTypeChange, "one commit ships to several services, so every change in the run shares it"},
		{"an image digest", resolution.NamespaceDeployImage,
			"eu.pkg.dev/acme/apps/storefront@sha256:" + strings.Repeat("ab", 32),
			graph.NodeTypeChange, "the same image runs in staging and in production"},
		{"a release tag", resolution.NamespaceDeployRelease, "v2.3.0",
			graph.NodeTypeChange, "many repositories use the same tag in the same week"},
		{"a repository shipping two services", resolution.NamespaceGitHubRepo, "acme/shop",
			graph.NodeTypeService, "a repository is a claim on the service it ships, never a node"},
	} {
		arriving := resolution.Claim{
			ClaimID: "claim-a", EntityID: "entity-a", EntityType: tc.typ, SourceID: "github:acme",
			EventID: "a", Namespace: tc.namespace, Value: tc.value,
		}
		other := arriving
		other.ClaimID, other.EntityID, other.SourceID, other.EventID = "claim-b", "entity-b", "gcp:acme", "b"
		store := fakeStore{claims: []resolution.Claim{arriving, other}}

		matches, err := resolution.Evaluate(context.Background(), store, arriving)
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", tc.name, err)
		}
		if _, ok := matchOf(matches, "C1"); ok {
			t.Errorf("C1 merged two entities sharing %s (%s): %s", tc.name, tc.namespace, tc.why)
		}
		if resolution.IdentifyingNamespace(tc.namespace) {
			t.Errorf("%s is published as an identifying namespace", tc.namespace)
		}
	}
}

// And the guard is a guard and not an off switch: C1 still merges on a namespace that does name one
// entity, including the two change namespaces the deploy feeders address their changes by.
func TestC1StillMergesOnAnIdentifyingNamespace(t *testing.T) {
	t.Parallel()

	for _, namespace := range []string{
		resolution.NamespaceOTelService,
		"github.change",
		"vercel.change",
		"vercel.project",
	} {
		arriving := resolution.Claim{
			ClaimID: "claim-a", EntityID: "entity-a", EntityType: graph.NodeTypeChange,
			SourceID: "github:acme", EventID: "a", Namespace: namespace, Value: "the-one-thing",
		}
		other := arriving
		other.ClaimID, other.EntityID, other.SourceID, other.EventID = "claim-b", "entity-b", "gcp:acme", "b"
		store := fakeStore{claims: []resolution.Claim{arriving, other}}

		matches, err := resolution.Evaluate(context.Background(), store, arriving)
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", namespace, err)
		}
		if _, ok := matchOf(matches, "C1"); !ok {
			t.Errorf("C1 did not fire on %s, which names one entity; the guard is meant to narrow C1, "+
				"not to switch it off", namespace)
		}
		if !resolution.IdentifyingNamespace(namespace) {
			t.Errorf("%s is published as non-identifying, but a value in it names one thing", namespace)
		}
	}
}

func matchOf(matches []resolution.Match, ruleID string) (resolution.Match, bool) {
	for _, match := range matches {
		if match.RuleID == ruleID {
			return match, true
		}
	}
	return resolution.Match{}, false
}

func matchRuleIDs(matches []resolution.Match) []string {
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.RuleID)
	}
	return out
}

func namespacesInclude(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
