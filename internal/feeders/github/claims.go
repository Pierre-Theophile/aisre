// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What a GitHub rollout mints, and under which of the two kinds (004 T056, T057, T148; FR-026,
// FR-030, FR-041; contracts/deploy-claims.md).
//
// ---------------------------------------------------------------------------------------------
// What is minted, and what is deliberately not
//
// Two deploy correlation keys, and no claim at all in the deploy vocabulary:
//
//	deploy.commit_sha  the commit this rollout shipped — the join GitHub and Vercel share unconditionally
//	deploy.image       omitted: GitHub states no image for a deployment
//	deploy.release     where a deployment or a run names one
//	github.repo        the repository, as a PROPERTY of the change (RepositoryProperty)
//
// FR-041's rule runs through all of them: **omit rather than invent**. A namespace whose value the
// platform did not state is a namespace this connector leaves alone — a `deploy.image` guessed from a
// repository name would be a key certain rule C8 merges two unrelated rollouts on, and C8's merges are
// not proposals.
//
// # Why a correlation key and not an identity claim
//
// Because a commit does not NAME a rollout. One commit ships to three services in a monorepo run and
// again on a redeploy, so several changes carry the value and none of them owns it —
// `graph.identity_claims` is unique per (namespace, value, source) and could hold it for exactly one.
// These were claims first, and the fixture corpus proved it cannot work: four of seven US1 fixtures
// failed the shuffle step because the graph's state depended on which change the projector reached
// first. T148 made the distinction a kind of its own, and the event log now refuses any of these
// namespaces as a claim (internal/log, ReasonCorrelationAsIdentity).
//
// # Why the repository is neither a claim nor a node
//
// FR-030. A repository is not a thing that runs in production; it is a name several rollouts share.
// Making it a node would put an entity on the graph that nothing serves traffic from, and — worse —
// certain rule C1 would merge every service in a monorepo into one entity, because they would all
// carry the same identifying claim. It is a property of the change (RepositoryProperty) rather than
// even a correlation key, because no published rule correlates on it: a key nothing reads is a key
// that only costs a row.
//
// # Why the environment is a supporting attribute rather than part of the value
//
// C8 merges two observations of one rollout when they agree on a deploy identifier **and** on the
// environment. Folding the environment into the value — `acme/storefront@production` — would make the
// key unjoinable with the same repository observed by another source that spells its environments
// differently, and it would make the environment invisible to a rule that needs to compare it. So it
// rides as an attribute, which is what contracts/deploy-claims.md §2.1 specifies and what the Cloud Run
// side already emits.

// The supporting attribute names. They are the spellings the published rules read; a key whose
// attribute a rule cannot find is a rule that never fires, silently.
const (
	// AttrClaimSource names what the identifier was read from, as the key's evidence.
	AttrClaimSource = "sre.github.claim_source"
	// AttrRepository carries the repository alongside a deploy key, so a reader of a merge can see
	// which repository's rollout it came from without joining anything else.
	AttrRepository = "sre.github.repository"
)

// The evidence names, published because they are what a reviewer reads to tell a stated identifier
// from a derived one.
const (
	SourceDeploymentSHA = "deployment.sha"
	SourceRunHeadSHA    = "workflow_run.head_sha"
	SourceReleaseTag    = "release.tag_name"
	SourceDeploymentRef = "deployment.ref"
	SourceRepository    = "repository.full_name"
)

// DeployKeys mints the deploy correlation keys for one rollout.
//
// `commitSHA` is the commit the platform stated — a deployment's `sha` or a run's `head_sha` — and
// `releaseTag` the release it named, where it named one. Each is normalised by pkg/feeder's own
// normaliser and **omitted** where the normaliser refuses it, which is how an abbreviated sha stays
// out of the graph rather than being padded into a guess.
func DeployKeys(repo Repo, commitSHA, commitSource, releaseTag string) []feeder.CorrelationKey {
	var keys []feeder.CorrelationKey
	repository, repositoryStated := feeder.Repository(repo.Owner, repo.Name)

	attrs := func() map[string]string {
		if !repositoryStated {
			return nil
		}
		return map[string]string{AttrRepository: repository}
	}

	if value, ok := feeder.CommitSHA(commitSHA); ok {
		source := strings.TrimSpace(commitSource)
		if source == "" {
			source = SourceDeploymentSHA
		}
		keys = append(keys, feeder.CorrelationKey{
			Namespace: feeder.NSDeployCommitSHA, Value: value, Why: source, Attrs: attrs(),
		})
	}
	if value, ok := feeder.Release(releaseTag); ok {
		keys = append(keys, feeder.CorrelationKey{
			Namespace: feeder.NSDeployRelease, Value: value, Why: SourceReleaseTag, Attrs: attrs(),
		})
	}
	// No deploy.image. GitHub states no image for a deployment, and a digest this connector derived
	// from a repository name would be a key C8 merges two unrelated rollouts on.
	return keys
}

// RepositoryProperty is the repository a change belongs to, as a **property**: neither a claim nor a
// correlation key.
//
// It was a claim first, and the fixture corpus proved that cannot work. `graph.identity_claims` is
// constrained `UNIQUE (namespace, value, source_id)`, so one identifier value from one source belongs
// to exactly one entity — and this connector emits the same repository on every change it makes. The
// first change processed won the claim and the rest did not, which made the graph's state depend on
// the order the events arrived in. Four of the seven US1 fixtures failed the shuffle step on exactly
// that, which is what the shuffle step is for. That finding is what T148 generalised: an identifier
// several entities share is not merely non-identifying, it is **not expressible as a claim at all**.
//
// A correlation key would now be expressible, and it is still not emitted, because no published rule
// reads `github.repo`. C8 keys on `deploy.commit_sha` and `deploy.image`; the repository already
// travels as a supporting attribute on those keys, where a rule comparing two observations can see it.
// A key nothing reads costs a row and earns nothing. It creates no repository node either way
// (FR-030).
func RepositoryProperty(repo Repo) (string, bool) {
	return feeder.Repository(repo.Owner, repo.Name)
}
