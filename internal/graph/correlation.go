// SPDX-License-Identifier: Apache-2.0

package graph

import "slices"

// Correlation keys: the second way two things can be related (004 T148).
//
// ---------------------------------------------------------------------------------------------
// Why identity was not enough
//
// The graph had one way to say two observations are about the same thing: an **identity claim**, a
// name for one entity. `otel.service.name=checkout` is such a name — two sources saying it are naming
// one service, and certain rule C1 merges on exactly that.
//
// Most of what an SRE agent reasons with is not that. "The same commit", "the same image", "the same
// release", "the same trace", "the same build": each is a fact that **many** entities share. A commit
// ships three services in a monorepo and ships them again on the next redeploy. These are not names,
// they are **correlation keys** — and the difference is not academic, because `graph.identity_claims`
// is constrained `UNIQUE (namespace, value, source_id)`: one identifier value from one source belongs
// to exactly one entity. A correlation key stored there lands on whichever entity the projector
// happened to process first, and the graph's state then depends on the order events arrived in.
//
// That is not a hypothetical. It is what four of feature 004's seven US1 fixtures failed their shuffle
// step on, and the shuffle step exists to find precisely this.
//
// # The distinction was already known, and written in the wrong place
//
// internal/resolution's `sharedPropertyNamespaces` describes it exactly — *"equality of one of these
// values is not by itself an identity"* — as a hand-maintained exception list inside one rule, while
// the storage underneath went on asserting the opposite. This package is where that stops being an
// exception and becomes a kind.
//
// # What follows from separating them
//
// Identity gets **stronger**, which is the part worth noticing. With the correlation namespaces moved
// out, `identity_claims` is genuinely injective again, so C1's premise — equality of an identifier is
// evidence of sameness — holds without a list of cases where it does not.
//
// And correlation gets a shape every future cross-source rule can reuse: **correlated, then
// corroborated**. C8 was the first rule of that shape (a shared deploy identifier, plus an agreed
// environment, plus a target the graph already merged) and it had to smuggle its key through the
// identity table to exist. The next such rule — the same trace across two backends, the same incident
// across two vendors — does not.
//
// # Why the registry is explicit rather than derived from a prefix
//
// A namespace is a correlation key or an identity because of what its values MEAN, not because of how
// it is spelled. A rule that guessed from `deploy.` would be wrong the first time somebody publishes
// an identifier under that prefix, and wrong silently.
//
// # What the registry cannot say, and why `github.repo` is not in it
//
// The refusal this registry drives sees a namespace and not a subject. So a namespace belongs here only
// if its values name NOTHING — if there is no entity anywhere that such a value could legitimately
// address. The three `deploy.*` namespaces are of that kind: a commit, an image digest and a release
// tag are facts about rollouts, and no node is addressed by one.
//
// `github.repo` is not, and it was in an earlier draft of this list. `acme/storefront` does name exactly
// one repository, and the deploy feeders address the TARGET of a rollout by it — which mints an entity,
// and `createEntity` gives every entity a primary identity claim in the namespace that addressed it. So
// a registry holding `github.repo` would promise a refusal the projector itself breaks.
//
// Its real danger is a wrong SUBJECT rather than a wrong namespace: claimed on a change, it is shared by
// every change the repository ships, and claimed on a service, by every service in a monorepo. That is
// what `sharedPropertyNamespaces` in internal/resolution guards, by keeping C1 from merging on it, and
// no feeder emits it as a claim at all — the GitHub connector carries the repository as a property of
// the change (internal/feeders/github/claims.go, RepositoryProperty).

// The correlation namespaces this schema publishes.
//
// Each is a value several entities legitimately share. They are refused as identity claims
// (internal/log/validate.go), so the mistake of minting one as a name is caught at the event log
// rather than found later as an order-dependent graph.
const (
	// CorrelationDeployCommitSHA is the commit a rollout shipped. A monorepo run ships one commit to
	// several services, and a redeploy ships the same commit again.
	CorrelationDeployCommitSHA = "deploy.commit_sha"
	// CorrelationDeployImage is the digest-pinned image a rollout deployed. The same image runs in
	// staging and in production, and a redeploy of one digest is a second rollout.
	CorrelationDeployImage = "deploy.image"
	// CorrelationDeployRelease is the release identifier a rollout shipped. `v2.3.0` is a tag many
	// repositories use in the same week.
	CorrelationDeployRelease = "deploy.release"
)

// CorrelationNamespaces is every namespace this schema publishes as a correlation key, sorted.
//
// A connector may correlate in a namespace outside this list — a new source names things this list
// has never heard of — but a namespace two connectors mean the same thing by must be spelled the same,
// which is what a published list is for. What the list DECIDES is narrower and firm: a namespace named
// here may never be used as an identity claim.
var CorrelationNamespaces = []string{
	CorrelationDeployCommitSHA,
	CorrelationDeployImage,
	CorrelationDeployRelease,
}

// IsCorrelationNamespace reports whether ns is published as a correlation key, and therefore may not
// be asserted as an identity.
//
// It is the check `internal/log` enforces at the event log and the one a connector author can run
// against a namespace they are about to mint.
func IsCorrelationNamespace(ns string) bool { return slices.Contains(CorrelationNamespaces, ns) }
