// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"errors"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What a rollout changed (004 T063–T065, T070; FR-017, SC-010, Edge case 1).

var monorepo = github.Repo{Owner: "acme", Name: "monorepo"}

func targetMap() github.TargetMap {
	return github.TargetMap{Repositories: map[string][]github.TargetRule{
		"acme/monorepo": {
			{Environment: "production", Namespace: feeder.NSK8sDeployment, Value: "shop/checkout"},
			{Environment: "production", Namespace: feeder.NSK8sDeployment, Value: "shop/catalogue"},
			{Environment: "production", Workflow: "Deploy search", Namespace: feeder.NSK8sDeployment, Value: "shop/search"},
			{Environment: "staging", Namespace: feeder.NSK8sDeployment, Value: "staging/checkout"},
		},
		"acme/storefront": {
			{Namespace: feeder.NSK8sDeployment, Value: "shop/storefront"},
		},
	}}
}

func values(refs []*graphv1.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.GetValue())
	}
	return out
}

// The mapping is read by repository, environment and workflow, and the narrowing works in both
// directions: a rule naming a workflow does not fire for another, and a rule naming none fires for all.
func TestTheMappingIsNarrowedByEnvironmentAndWorkflow(t *testing.T) {
	t.Parallel()
	m := targetMap()
	for name, tc := range map[string]struct {
		repo        github.Repo
		environment string
		workflow    string
		want        []string
	}{
		"production, no workflow named": {
			repo: monorepo, environment: "production",
			want: []string{"shop/catalogue", "shop/checkout"},
		},
		"production, the search workflow": {
			repo: monorepo, environment: "production", workflow: "Deploy search",
			want: []string{"shop/catalogue", "shop/checkout", "shop/search"},
		},
		"staging is a different list": {
			repo: monorepo, environment: "staging",
			want: []string{"staging/checkout"},
		},
		"a rule naming neither fires for everything": {
			repo: storefront, environment: "production", workflow: "whatever",
			want: []string{"shop/storefront"},
		},
		"a repository nobody mapped": {
			repo: github.Repo{Owner: "acme", Name: "unmapped"}, environment: "production",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := values(m.TargetsFor(tc.repo, tc.environment, tc.workflow))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("TargetsFor = %v, want %v (sorted, so a golden does not churn)", got, tc.want)
			}
		})
	}
}

// T064: the target never comes from the observed repository — and that is enforced by the published
// operation surface rather than by intention. An operation the surface does not carry is refused
// before any quota is spent, so this connector CANNOT read a file from the repository it is watching.
func TestTheConnectorCannotReadAFileFromTheObservedRepository(t *testing.T) {
	t.Parallel()
	for _, op := range []feeder.ReadOperation{
		"GET /repos/{owner}/{repo}/contents/{path}",
		"GET /repos/{owner}/{repo}/git/trees/{tree_sha}",
		"GET /repos/{owner}/{repo}/git/blobs/{file_sha}",
	} {
		_, err := github.Issuable(op)
		var unpublished *feeder.UnpublishedOperationError
		if !errors.As(err, &unpublished) {
			t.Errorf("%q is issuable; the repository↔service mapping must never come from a file in the "+
				"observed repository, and the surest way to honour that is for the connector to be "+
				"incapable of reading one (Clarifications 2026-09-22)", op)
		}
	}
}

// And nothing the person who triggered the deploy wrote reaches the mapping. A `ref`, a `task` or the
// arbitrary JSON attached to a deployment are all chosen by the observed system, and a target taken
// from one of them is a target chosen by the thing being observed.
func TestNothingTheDeployerWroteChoosesATarget(t *testing.T) {
	t.Parallel()
	m := targetMap()
	// The repository is unmapped, and the deployment's own fields name a service that IS mapped. A
	// mapping that read the payload would attach this rollout to somebody else's service.
	got := m.TargetsFor(github.Repo{Owner: "acme", Name: "someone-elses-repo"}, "production", "Deploy search")
	if len(got) != 0 {
		t.Errorf("an unmapped repository resolved to %v; the mapping is the operator's and a repository "+
			"naming a target it does not own would put its deploys at hop 0 of a stranger's service",
			values(got))
	}
}

// FR-017 and SC-010: one change per target, each with its own identity.
func TestARunDeployingThreeServicesIsThreeChanges(t *testing.T) {
	t.Parallel()
	targets := targetMap().TargetsFor(monorepo, "production", "Deploy search")
	base := "repos/acme/monorepo/deployments/4321"

	got, ok := github.SplitByTarget(base, targets)
	if !ok {
		t.Fatal("the split was refused")
	}
	if got.Unattached {
		t.Error("a deploy with three targets is reported as unattached")
	}
	if len(got.Changes) != 3 {
		t.Fatalf("a run deploying three services became %d change(s); one change with three targets "+
			"would sit at hop 0 of three unrelated services and rank three times for three onsets",
			len(got.Changes))
	}
	seen := map[string]bool{}
	for _, change := range got.Changes {
		if change.Target == nil {
			t.Errorf("a targeted change carries no target: %+v", change)
			continue
		}
		if seen[change.Ref.GetValue()] {
			t.Errorf("two changes share the identity %q; two changes cannot be one node",
				change.Ref.GetValue())
		}
		seen[change.Ref.GetValue()] = true
		if !strings.HasPrefix(change.Ref.GetValue(), base+"/") {
			t.Errorf("the change %q is not built from the deployment's resource path %q, so its "+
				"identity, its origin link and its pointer would not agree", change.Ref.GetValue(), base)
		}
		if !strings.Contains(change.Ref.GetValue(), change.Target.GetValue()) {
			t.Errorf("the change %q does not carry its target %q", change.Ref.GetValue(), change.Target.GetValue())
		}
	}
}

// They share one origin reference and one commit key, which is what keeps them recognisable as one
// pipeline run — and what lets C8 merge each of them with the platform's own observation separately.
//
// It is also the case that forced the key kind. Every change in this split carries the SAME
// `deploy.commit_sha`, and `graph.identity_claims` is unique per (namespace, value, source): as claims,
// one of these changes would have got the value and the others nothing, according to which the
// projector reached first (004 T148).
func TestThreeChangesShareOneOriginAndOneCommit(t *testing.T) {
	t.Parallel()
	targets := targetMap().TargetsFor(monorepo, "production", "")
	base := "repos/acme/monorepo/deployments/4321"
	got, _ := github.SplitByTarget(base, targets)

	link, ok := github.OriginLink("https://github.com/acme/monorepo/actions/runs/99")
	if !ok {
		t.Fatal("no origin link")
	}
	keys := github.DeployKeys(monorepo, shippedSHA, github.SourceDeploymentSHA, "")
	if len(keys) != 1 || keys[0].Value != shippedSHA {
		t.Fatalf("the deploy keys are %+v, want one commit key", keys)
	}
	// One link and one key value serve every change; the identities are what differ.
	for _, change := range got.Changes {
		if link == "" || change.Ref.GetValue() == "" {
			t.Fatal("empty link or identity")
		}
	}
	if len(got.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(got.Changes))
	}
	if got.Changes[0].Ref.GetValue() == got.Changes[1].Ref.GetValue() {
		t.Error("the two changes share an identity")
	}
}

// Edge case 1: a deploy nothing maps is one unattached change, not a dropped one. A deploy that
// happened is a fact whether or not this connector can say what it touched.
func TestADeployWithNoTargetIsOneUnattachedChange(t *testing.T) {
	t.Parallel()
	got, ok := github.SplitByTarget("repos/acme/unmapped/deployments/7", nil)
	if !ok {
		t.Fatal("the split was refused")
	}
	if !got.Unattached {
		t.Error("a deploy with no target is not reported as unattached, so a caller cannot count it")
	}
	if len(got.Changes) != 1 {
		t.Fatalf("a deploy with no target became %d changes, want exactly one", len(got.Changes))
	}
	if got.Changes[0].Target != nil {
		t.Errorf("the unattached change carries the target %v", got.Changes[0].Target)
	}
	if got.Changes[0].Ref.GetValue() != "repos/acme/unmapped/deployments/7" {
		t.Errorf("the unattached change is %q, want the deployment's own path", got.Changes[0].Ref.GetValue())
	}
}

// Two rules naming the same target — an environment rule and a catch-all, say — are one target.
// Emitting it twice would be two changes for one service from one deploy.
func TestTwoRulesNamingOneTargetAreOneChange(t *testing.T) {
	t.Parallel()
	m := github.TargetMap{Repositories: map[string][]github.TargetRule{
		"acme/storefront": {
			{Namespace: feeder.NSK8sDeployment, Value: "shop/storefront"},
			{Environment: "production", Namespace: feeder.NSK8sDeployment, Value: "shop/storefront"},
		},
	}}
	got := m.TargetsFor(storefront, "production", "")
	if len(got) != 1 {
		t.Errorf("two rules naming `shop/storefront` resolved to %v, want one target", values(got))
	}
}

// A rule missing half its target is skipped rather than minting a ref into an empty namespace.
func TestAnIncompleteRuleIsSkipped(t *testing.T) {
	t.Parallel()
	m := github.TargetMap{Repositories: map[string][]github.TargetRule{
		"acme/storefront": {
			{Namespace: feeder.NSK8sDeployment},
			{Value: "shop/storefront"},
			{Namespace: "   ", Value: "   "},
			{Namespace: feeder.NSK8sDeployment, Value: "shop/storefront"},
		},
	}}
	got := m.TargetsFor(storefront, "production", "")
	if len(got) != 1 || got[0].GetValue() != "shop/storefront" {
		t.Errorf("TargetsFor = %v, want only the complete rule", values(got))
	}
}
