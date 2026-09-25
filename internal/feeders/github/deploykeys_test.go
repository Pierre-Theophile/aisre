// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What a GitHub rollout mints, and under which kind (004 T056, T057, T148; FR-026, FR-030, FR-041).

func keysByNamespace(keys []feeder.CorrelationKey) map[string]feeder.CorrelationKey {
	out := map[string]feeder.CorrelationKey{}
	for _, key := range keys {
		out[key.Namespace] = key
	}
	return out
}

const shippedSHA = "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"

// The commit is the join GitHub and Vercel share unconditionally, and it is minted in the namespace
// certain rule C8 keys on.
func TestTheCommitIsMintedInTheNamespaceC8KeysOn(t *testing.T) {
	t.Parallel()
	got := keysByNamespace(github.DeployKeys(storefront, shippedSHA, github.SourceDeploymentSHA, ""))
	commit, ok := got[feeder.NSDeployCommitSHA]
	if !ok {
		t.Fatalf("no commit key was minted; the keys are %+v", got)
	}
	if commit.Value != shippedSHA {
		t.Errorf("value = %q, want the full lower-cased object id", commit.Value)
	}
	if commit.Why != github.SourceDeploymentSHA {
		t.Errorf("Why = %q, want the field it was read from", commit.Why)
	}
	// And the namespace is one C8 actually reads, so the join is not a namespace this package invented.
	if !contains(resolution.DeployIdentifierNamespaces(), commit.Namespace) {
		t.Errorf("%q is not among C8's deploy identifier namespaces %v; a key no rule reads is a "+
			"merge that silently never happens", commit.Namespace, resolution.DeployIdentifierNamespaces())
	}
}

// FR-041: omit rather than invent. A value the platform did not state, or stated in a form the
// normaliser refuses, yields no key at all.
func TestAnIdentifierTheNormaliserRefusesIsOmittedRatherThanRepaired(t *testing.T) {
	t.Parallel()
	for name, sha := range map[string]string{
		"nothing stated":     "",
		"an abbreviated sha": "8f5bd3c",
		"not hex":            "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"whitespace":         "   ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := keysByNamespace(github.DeployKeys(storefront, sha, github.SourceDeploymentSHA, ""))
			if key, ok := got[feeder.NSDeployCommitSHA]; ok {
				t.Errorf("a commit key %q was minted from %q; C8's merges are not proposals, so a "+
					"padded or guessed identifier merges two unrelated rollouts (FR-041)", key.Value, sha)
			}
		})
	}
}

// GitHub states no image for a deployment, so no `deploy.image` is minted. A digest derived from a
// repository name would be a key C8 merges unrelated rollouts on.
func TestNoImageKeyIsInventedFromARepositoryName(t *testing.T) {
	t.Parallel()
	got := keysByNamespace(github.DeployKeys(storefront, shippedSHA, github.SourceDeploymentSHA, "v1.2.3"))
	if key, ok := got[feeder.NSDeployImage]; ok {
		t.Errorf("a deploy.image key %q was minted; GitHub states no image for a deployment", key.Value)
	}
	// The release it DID state is minted, so the assertion above is not "nothing is minted".
	if release, ok := got[feeder.NSDeployRelease]; !ok || release.Value != "v1.2.3" {
		t.Errorf("the release key is %+v, want v1.2.3 from the tag the platform named", release)
	}
}

// FR-030: the repository is neither a node nor a claim nor a correlation key — it is a property.
//
// The claim form is what the fixture corpus rejected. `graph.identity_claims` is constrained
// `UNIQUE (namespace, value, source_id)`, so one identifier from one source belongs to exactly one
// entity — and this connector puts the same repository on every change it makes. The first change
// processed won it and the rest did not, so the graph's state depended on the order events arrived in,
// and four of the seven US1 fixtures failed their shuffle step on it.
func TestTheRepositoryIsAPropertyAndNeitherANodeNorAKey(t *testing.T) {
	t.Parallel()
	value, ok := github.RepositoryProperty(storefront)
	if !ok {
		t.Fatal("no repository property was produced")
	}
	if value != "acme/storefront" {
		t.Errorf("value = %q, want the lower-cased owner/repository", value)
	}
	// It is minted as neither kind, and the deploy keys carry it as a supporting attribute instead —
	// which is where a rule comparing two observations can still read it. A correlation key would now be
	// expressible; it is still not emitted, because no published rule reads `github.repo`.
	for _, key := range github.DeployKeys(storefront, shippedSHA, github.SourceDeploymentSHA, "") {
		if key.Namespace == feeder.NSGitHubRepo {
			t.Errorf("the repository is still minted in its own namespace (%q); no published rule reads "+
				"it, and a key nothing reads only costs a row", key.Value)
		}
		if key.Attrs[github.AttrRepository] != "acme/storefront" {
			t.Errorf("the key %q does not carry the repository as an attribute: %v",
				key.Namespace, key.Attrs)
		}
	}
	// And the narrowing that stopped C1 merging on the namespace still holds, because another connector
	// may yet mint it as an identity of something.
	if resolution.IdentifyingNamespace(feeder.NSGitHubRepo) {
		t.Errorf("%q is treated as identifying, so C1 would merge every entity carrying it — which in "+
			"a monorepo is every service in the repository (FR-030)", feeder.NSGitHubRepo)
	}
}

// GitHub treats both halves of a repository name case-insensitively, so two sources spelling it
// differently must read as one repository rather than two.
func TestTheRepositoryPropertyIsCaseFolded(t *testing.T) {
	t.Parallel()
	lower, _ := github.RepositoryProperty(github.Repo{Owner: "acme", Name: "storefront"})
	upper, _ := github.RepositoryProperty(github.Repo{Owner: "ACME", Name: "Storefront"})
	if lower != upper {
		t.Errorf("`acme/storefront` and `ACME/Storefront` read as %q and %q; GitHub treats both halves "+
			"case-insensitively and two spellings would be two repositories", lower, upper)
	}
	if _, ok := github.RepositoryProperty(github.Repo{Owner: "", Name: "storefront"}); ok {
		t.Error("a repository with no owner produced a value")
	}
}

// The environment rides as a supporting attribute rather than in the value, because C8 compares it and
// a value it was folded into is a value nothing can compare.
func TestTheRepositoryTravelsAsAnAttributeOnTheDeployKeys(t *testing.T) {
	t.Parallel()
	got := keysByNamespace(github.DeployKeys(storefront, shippedSHA, github.SourceDeploymentSHA, ""))
	commit := got[feeder.NSDeployCommitSHA]
	if commit.Attrs[github.AttrRepository] != "acme/storefront" {
		t.Errorf("the commit key's attributes are %v, want the repository alongside it so a reader "+
			"of a merge sees which repository's rollout it came from", commit.Attrs)
	}
	if commit.Value != shippedSHA {
		t.Errorf("value = %q; folding the repository into the value would make the key unjoinable "+
			"with the same commit observed by another source", commit.Value)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
