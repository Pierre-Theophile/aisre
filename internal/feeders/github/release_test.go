// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Releases, ties and the two facts this feeder refuses to check (004 T066–T071).

var publishedAt = time.Date(2026, 3, 1, 3, 14, 0, 0, time.UTC)

// FR-025: a release is a rollout where the operator says the repository ships that way.
func TestAReleaseIsARolloutWhereTheRepositoryShipsByRelease(t *testing.T) {
	t.Parallel()
	policy := github.ReleasePolicy{ShipsByRelease: []string{"acme/storefront"}}
	got := policy.ReadRelease(storefront, github.Release{
		ID: 7, TagName: "v1.2.3", PublishedAt: publishedAt,
	})
	switch {
	case !got.Emit:
		t.Fatal("a published release in a repository that ships by release was not emitted")
	case got.Kind != graphv1.ChangeKind_ROLLOUT:
		t.Errorf("kind = %v, want ROLLOUT", got.Kind)
	case !got.ValidAt.Equal(publishedAt):
		t.Errorf("ValidAt = %v, want the publication instant %v", got.ValidAt, publishedAt)
	case got.Why != github.ReleaseWhyShipsByRelease:
		t.Errorf("Why = %q", got.Why)
	}
}

// Edge case 10: a release that is not a rollout is NEVER dropped. It becomes the taxonomy's `other`
// kind carrying GitHub's own object kind, because an operator asking what happened around 03:14 is
// asking about their estate and not about this connector's opinion of what is interesting.
func TestAReleaseThatIsNotARolloutIsRecordedRatherThanDropped(t *testing.T) {
	t.Parallel()
	var policy github.ReleasePolicy
	got := policy.ReadRelease(storefront, github.Release{ID: 7, TagName: "v1.2.3", PublishedAt: publishedAt})
	switch {
	case !got.Emit:
		t.Fatal("a published release was dropped; Edge case 10 is explicit that it is never dropped")
	case got.Kind != graphv1.ChangeKind_CHANGE_KIND_OTHER:
		t.Errorf("kind = %v, want CHANGE_KIND_OTHER", got.Kind)
	case got.KindOther == "":
		t.Error("CHANGE_KIND_OTHER with no KindOther; the schema requires the name in that case and a " +
			"change nobody can name is a change nobody can read")
	case got.ObjectKind != github.ObjectKindRelease:
		t.Errorf("ObjectKind = %q, want GitHub's own word so the change stays readable after the "+
			"taxonomy flattened it", got.ObjectKind)
	case !got.ValidAt.Equal(publishedAt):
		t.Errorf("ValidAt = %v, want the publication instant", got.ValidAt)
	}
}

// A draft has not been published, so nothing has happened.
func TestADraftReleaseIsNotAChange(t *testing.T) {
	t.Parallel()
	policy := github.ReleasePolicy{ShipsByRelease: []string{"acme/storefront"}}
	for name, release := range map[string]github.Release{
		"a draft":                 {ID: 7, TagName: "v1.2.3", Draft: true, PublishedAt: publishedAt},
		"published at no instant": {ID: 8, TagName: "v1.2.4"},
	} {
		got := policy.ReadRelease(storefront, release)
		if got.Emit {
			t.Errorf("%s was emitted as a change (%+v); a draft is not a fact about production", name, got)
		}
		if got.Why != github.ReleaseWhyDraft {
			t.Errorf("%s was refused for %q", name, got.Why)
		}
	}
}

// A pre-release in a repository that ships by release is still a rollout: shipping a release candidate
// to production is still shipping, and GitHub's flag is about the release's status rather than about
// whether it went anywhere.
func TestAPreReleaseStillShipsWhereTheRepositoryShipsByRelease(t *testing.T) {
	t.Parallel()
	policy := github.ReleasePolicy{ShipsByRelease: []string{"acme/storefront"}}
	got := policy.ReadRelease(storefront, github.Release{
		ID: 9, TagName: "v2.0.0-rc1", Prerelease: true, PublishedAt: publishedAt,
	})
	if got.Kind != graphv1.ChangeKind_ROLLOUT {
		t.Errorf("a published pre-release is %v, want ROLLOUT", got.Kind)
	}
}

// T066 / FR-028 / Edge case 3: a re-run is keyed on (run, attempt, target), so it is a new change and
// the earlier one is neither amended nor retracted.
func TestAReRunIsANewChangePerTargetAndAmendsNothing(t *testing.T) {
	t.Parallel()
	targets := targetMap().TargetsFor(monorepo, "production", "")

	first, _ := github.RunChangeRef(monorepoID, 99, 1)
	second, _ := github.RunChangeRef(monorepoID, 99, 2)
	firstSplit, _ := github.SplitByTarget(first.GetValue(), targets)
	secondSplit, _ := github.SplitByTarget(second.GetValue(), targets)

	if len(firstSplit.Changes) != 2 || len(secondSplit.Changes) != 2 {
		t.Fatalf("the two attempts produced %d and %d changes, want 2 each",
			len(firstSplit.Changes), len(secondSplit.Changes))
	}
	for i := range firstSplit.Changes {
		a, b := firstSplit.Changes[i].Ref.GetValue(), secondSplit.Changes[i].Ref.GetValue()
		if a == b {
			t.Errorf("attempt 1 and attempt 2 of run 99 share the identity %q for target %s; a re-run "+
				"is a NEW change and one identity for both would amend the earlier rollout out of "+
				"existence (FR-028)", a, firstSplit.Changes[i].Target.GetValue())
		}
	}
}

// T067: a re-run that fails or deploys nothing produces no change, because nothing in production moved.
func TestAReRunThatFailsProducesNoChange(t *testing.T) {
	t.Parallel()
	got := github.ReadStatuses([]github.DeploymentStatus{
		status(github.StateFailure, completedAt),
		status(github.StateInProgress, builtAt),
		status(github.StateQueued, runStarted),
	})
	if got.RolledOut {
		t.Error("a re-run that failed was read as a rollout")
	}
	if !got.Attempted {
		t.Error("the failed attempt was not recorded; it is still a fact worth having")
	}
	// A run whose statuses are all progress is the "deployed nothing" case.
	empty := github.ReadStatuses([]github.DeploymentStatus{status(github.StateQueued, runStarted)})
	if empty.RolledOut || empty.Attempted {
		t.Errorf("a run that deployed nothing produced %+v, want neither a rollout nor an attempt", empty)
	}
}

// Edge case 8: the commit is what the deployment record states, even where it no longer exists. The
// deployment did ship that sha, and the record of what shipped is the fact.
func TestTheCommitIsKeptEvenWhereItNoLongerExists(t *testing.T) {
	t.Parallel()
	// An orphaned sha — well-formed, and pointing at nothing after a force-push. The feeder does not
	// check, so this is indistinguishable from a live one, which is the point.
	const orphaned = "0000000000000000000000000000000000000001"
	got := keysByNamespace(github.DeployKeys(storefront, orphaned, github.SourceDeploymentSHA, ""))
	key, ok := got[feeder.NSDeployCommitSHA]
	if !ok {
		t.Fatal("no commit key was minted for a sha that no longer resolves")
	}
	if key.Value != orphaned {
		t.Errorf("value = %q, want the sha the deployment record states", key.Value)
	}
	// And checking would need an operation this connector does not carry, so the promise is structural.
	if _, err := github.Issuable("GET /repos/{owner}/{repo}/commits/{ref}"); err == nil {
		t.Error("a commit-reachability operation is issuable; checking would cost a call per deploy on " +
			"a path this connector deliberately does not carry")
	}
}

// Edge case 9: a repository rename is a property change, never a delete and a create.
//
// The identity is `repositories/{repository_id}/…` — GitHub's numeric id, which a rename does not
// touch — and not `repos/{owner}/{repo}/…`, which it does. The first version of this code used the
// name, and this test is what found it: the same deployment observed before and after a rename was
// two different nodes, so the graph would have retracted that repository's entire history and created
// it again under the new name.
func TestARepositoryRenameDoesNotChangeAChangesIdentity(t *testing.T) {
	t.Parallel()
	const repositoryID = 555

	before, okBefore := github.DeploymentChangeRef(repositoryID, 4321)
	after, okAfter := github.DeploymentChangeRef(repositoryID, 4321)
	if !okBefore || !okAfter {
		t.Fatal("no identity was minted")
	}
	if before.GetValue() != after.GetValue() {
		t.Fatalf("the same deployment has two identities, %q and %q", before.GetValue(), after.GetValue())
	}
	if strings.Contains(before.GetValue(), "storefront") || strings.Contains(before.GetValue(), "acme") {
		t.Errorf("the identity %q carries a name; a rename would make it a different identity and the "+
			"graph would retract the repository's whole history (Edge case 9)", before.GetValue())
	}
	repo, deployment, ok := github.ObjectIDFromPath(before.GetValue(), "deployments")
	if !ok || repo != repositoryID || deployment != 4321 {
		t.Errorf("the identity read back as repository %d, deployment %d (%v)", repo, deployment, ok)
	}

	// And the LINK still carries the name, because an address is not an identity: GitHub redirects a
	// renamed repository's paths, and a human following the link wants the name they know.
	pointer, ok := github.DeploymentSourceLink(storefront, 4321)
	if !ok || !strings.Contains(pointer.GetSelector(), "storefront") {
		t.Errorf("the source link is %q; keeping the address and the identity apart is what lets both "+
			"be right", pointer.GetSelector())
	}
}

// Edge case 11: two deployments completing in the same second are marked ambiguous rather than
// ordered by this connector. Which of two changes came first is the whole of a causal argument.
func TestTwoDeploymentsInTheSameSecondAreMarkedAmbiguous(t *testing.T) {
	t.Parallel()
	tied := completedAt
	got := github.TieAtSameInstant([]time.Time{
		tied,
		tied.Add(500 * time.Millisecond), // the same second: GitHub states instants to the second
		completedAt.Add(time.Minute),
		{},
	})
	switch {
	case !got[0] || !got[1]:
		t.Errorf("two deployments completing in the same second are marked %v, want both ambiguous — "+
			"breaking the tie would invent an ordering GitHub never stated", got[:2])
	case got[2]:
		t.Error("a deployment a minute apart from the others is marked ambiguous")
	case got[3]:
		t.Error("a deployment with no instant is marked ambiguous; it has no position to be ambiguous " +
			"about, and marking it would report an ordering problem where the problem is a missing date")
	}
}
