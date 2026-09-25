// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Releases, ordering ties, and the two facts this feeder refuses to check (004 T068–T071; FR-025,
// FR-028, Edge cases 3, 8, 9, 10, 11).
//
// ---------------------------------------------------------------------------------------------
// A release is a rollout only where the operator says the repository ships that way
//
// Some repositories deploy by publishing a release; most publish releases as a changelog and deploy
// some other way. Nothing in the payload distinguishes them, so FR-025 makes it configuration — and
// Edge case 10 is explicit that a release which is *not* a rollout is **never dropped**. It becomes a
// change of the taxonomy's `other` kind, carrying GitHub's own object kind as a property.
//
// Dropping it would be the worse error in the direction that matters. An operator asking "what
// happened around 03:14" is asking about their estate, not about this connector's opinion of which
// events are interesting, and a release published minutes before an incident is exactly the kind of
// thing they want to see even when it shipped nothing.
//
// # Two things this feeder does not verify
//
// **Edge case 8: the commit.** `deploy.commit_sha` is what the deployment record states, even where
// the commit no longer exists — a force-push can orphan it, a branch can be deleted. The feeder does
// not check reachability, and that is deliberate: the deployment *did* ship that sha, the record of
// what shipped is the fact, and checking would cost a call per deploy on the one API path this
// connector deliberately does not carry. A sha that resolves to nothing is still the answer to "what
// shipped", and it still joins with a platform feeder that observed the same rollout.
//
// **Edge case 9: the name.** Every key is a numeric id, so a repository rename is a property change
// rather than a delete and a create. A key built from `owner/name` would retract a repository's whole
// history the day somebody renamed it.
//
// # Edge case 11: two deployments completing in the same second
//
// The platform's stated ordering is preserved and the ambiguity is recorded. Breaking the tie would
// be inventing an ordering GitHub did not state, and it is the one thing an investigation must not
// have invented for it: which of two changes came first is the whole of a causal argument.

// PropObjectKind carries GitHub's own name for what the change was, on a change the taxonomy has no
// kind for. It is how a release stays readable after being mapped to `other`.
const PropObjectKind = "sre.github.object_kind"

// PropOrderingAmbiguous marks a change whose position among its neighbours the platform did not
// settle. See TieAtSameInstant.
const PropOrderingAmbiguous = "sre.github.ordering_ambiguous"

// The object kinds this feeder names.
const (
	ObjectKindRelease    = "release"
	ObjectKindDeployment = "deployment"
	ObjectKindWorkflow   = "workflow_run"
)

// ReleasePolicy is the operator's statement about which repositories ship by publishing a release.
type ReleasePolicy struct {
	// ShipsByRelease is the repositories, `owner/name`, whose releases are rollouts.
	ShipsByRelease []string
}

// ReleaseChange is how one release is recorded.
type ReleaseChange struct {
	Kind graphv1.ChangeKind
	// KindOther names the kind when Kind is CHANGE_KIND_OTHER, which the schema requires in that case.
	KindOther string
	// ObjectKind is GitHub's own word for the object, recorded as a property either way so a reader
	// of a change of the `other` kind can still see what it was.
	ObjectKind string
	// ValidAt is the instant the release was published. A draft has none, which is the point: a draft
	// is not a fact about production.
	ValidAt time.Time
	// Emit is false only for a release the platform has not published.
	Emit bool
	// Why says what decided, for the checkpoint and for a reviewer.
	Why string
}

// The published reasons.
const (
	ReleaseWhyShipsByRelease = "the operator lists this repository as shipping by release"
	ReleaseWhyNotARollout    = "a published release in a repository that does not ship by release; " +
		"recorded as `other` rather than dropped, because an operator asking what happened is asking " +
		"about their estate rather than about this connector's opinion of what is interesting"
	ReleaseWhyDraft = "a draft release: nothing has been published, so nothing has happened"
)

// ReadRelease decides how one release is recorded.
//
// A pre-release is a rollout like any other where the repository ships by release: shipping a release
// candidate to production is still shipping, and GitHub's `prerelease` flag is about the release's
// status rather than about whether it went anywhere.
func (p ReleasePolicy) ReadRelease(repo Repo, release Release) ReleaseChange {
	if release.Draft || release.PublishedAt.IsZero() {
		return ReleaseChange{ObjectKind: ObjectKindRelease, Emit: false, Why: ReleaseWhyDraft}
	}
	out := ReleaseChange{
		ObjectKind: ObjectKindRelease,
		ValidAt:    release.PublishedAt,
		Emit:       true,
	}
	if slug, ok := repositorySlug(repo); ok && listedFold(p.ShipsByRelease, slug) {
		out.Kind = graphv1.ChangeKind_ROLLOUT
		out.Why = ReleaseWhyShipsByRelease
		return out
	}
	out.Kind = graphv1.ChangeKind_CHANGE_KIND_OTHER
	out.KindOther = ObjectKindRelease
	out.Why = ReleaseWhyNotARollout
	return out
}

func repositorySlug(repo Repo) (string, bool) {
	owner, name := strings.TrimSpace(repo.Owner), strings.TrimSpace(repo.Name)
	if owner == "" || name == "" {
		return "", false
	}
	return strings.ToLower(owner + "/" + name), true
}

// TieAtSameInstant reports which of a set of instants are shared with another in the set.
//
// It is how Edge case 11 is honoured: two deployments completing in the same second are recorded in
// the order the platform stated them, and each is marked ambiguous — rather than the connector
// breaking the tie by an ordering GitHub never gave. Which of two changes came first is the whole of a
// causal argument, and it is the one thing an investigation must not have invented for it.
//
// The return is by index, so a caller keeps the platform's own order and does not have to sort.
func TieAtSameInstant(instants []time.Time) []bool {
	out := make([]bool, len(instants))
	counts := map[int64]int{}
	for _, instant := range instants {
		if instant.IsZero() {
			continue
		}
		counts[instant.Unix()]++
	}
	for i, instant := range instants {
		if instant.IsZero() {
			continue
		}
		out[i] = counts[instant.Unix()] > 1
	}
	return out
}
