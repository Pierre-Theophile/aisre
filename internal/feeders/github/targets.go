// SPDX-License-Identifier: Apache-2.0

package github

import (
	"slices"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What a rollout changed (004 T063–T065, T070; FR-017, SC-010, Edge case 1, Clarifications 2026-09-22).
//
// ---------------------------------------------------------------------------------------------
// Why the mapping is the operator's and cannot come from the repository
//
// A GitHub deployment says which environment it went to. It does not say which service it is. The
// missing half — repository (and environment, and workflow) to the entity that runs in production — is
// the operator's, and the clarification of 2026-09-22 is explicit that it must **never** be read from a
// file committed in the observed repository.
//
// The reason is not tidiness. A file in the observed repository is written by whoever can push to that
// repository, and this connector would then be taking instructions about what its changes target from
// the thing it is observing. A repository that names somebody else's service as its target would
// attach its rollouts to that service's graph, and every investigation of that service would rank a
// stranger's deploy as a candidate cause.
//
// **That is enforced structurally rather than by this comment.** The published read-only surface
// carries no operation that reads repository contents — `GET /repos/{owner}/{repo}/contents/{path}` is
// not on it, and an operation the surface does not carry is refused before any quota is spent. The
// connector cannot read such a file, so it cannot be configured by one. A test asserts that.
//
// # One change per target
//
// FR-017 and SC-010: a run that deploys three services is three changes, not one change with three
// targets. An investigation asks "what changed on checkout" and gets an answer about checkout; one
// change with three targets would put the same node at hop 0 of three unrelated services and rank it
// three times for three different onsets.
//
// They share one origin reference and one `deploy.commit_sha`, which is what keeps them recognisable
// as one pipeline run — and what lets C8 merge each of them with the platform feeder's observation of
// the same service's rollout, separately and correctly.
//
// # A change with no target is still a change
//
// Edge case 1: where nothing names a target, the run is emitted as **one unattached change** rather
// than dropped. A deploy that happened is a fact whether or not this connector can say what it
// touched, and a target that appears later attaches to it through the projector's pending queue
// (T137) rather than requiring the change to be re-observed.

// TargetRule is one line of the operator's mapping.
//
// Environment and Workflow narrow it. Both empty means the rule applies to every rollout in the
// repository, which is the right default for a repository holding one service and the wrong one for a
// monorepo — so a monorepo's rules name their environment or their workflow, and the connector does
// not guess which kind of repository it is looking at.
type TargetRule struct {
	// Environment limits the rule to one deployment environment, compared case-insensitively.
	Environment string
	// Workflow limits the rule to one workflow name.
	Workflow string
	// Namespace and Value are the entity this rollout targets, in the graph's own vocabulary —
	// `k8s.deployment` and `shop/checkout`, `gcp.cloudrun.service` and `proj/region/storefront`.
	//
	// The graph's vocabulary rather than GitHub's, because the target is a thing that runs in
	// production and GitHub has no name for it. A connector inventing one would mint a claim no other
	// source could join.
	Namespace string
	Value     string
}

// Ref renders the rule's target.
func (r TargetRule) Ref() *graphv1.Ref { return feeder.Ref(r.Namespace, r.Value) }

func (r TargetRule) valid() bool {
	return strings.TrimSpace(r.Namespace) != "" && strings.TrimSpace(r.Value) != ""
}

// matches reports whether the rule applies to one rollout. An empty narrowing field matches
// everything; a set one must match exactly, case aside.
func (r TargetRule) matches(environment, workflow string) bool {
	if e := strings.TrimSpace(r.Environment); e != "" && !strings.EqualFold(e, strings.TrimSpace(environment)) {
		return false
	}
	if w := strings.TrimSpace(r.Workflow); w != "" && !strings.EqualFold(w, strings.TrimSpace(workflow)) {
		return false
	}
	return true
}

// TargetMap is the operator-owned repository↔service mapping, keyed by `owner/repository`.
type TargetMap struct {
	Repositories map[string][]TargetRule
}

// TargetsFor returns the entities a rollout in this repository changed, sorted and deduplicated.
//
// It reads **only** the repository, the environment and the workflow. Nothing else about the payload
// reaches it — not the deployment's `ref`, not its `task`, not the arbitrary JSON its creator attached
// — because every one of those is written by whoever triggered the deploy, and a target taken from
// them is a target chosen by the observed system.
//
// No match is not an error. It is Edge case 1, and the caller emits one unattached change.
func (m TargetMap) TargetsFor(repo Repo, environment, workflow string) []*graphv1.Ref {
	key, ok := feeder.Repository(repo.Owner, repo.Name)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []*graphv1.Ref
	for _, rule := range m.Repositories[key] {
		if !rule.valid() || !rule.matches(environment, workflow) {
			continue
		}
		ref := rule.Ref()
		id := ref.GetNamespace() + "/" + ref.GetValue()
		if seen[id] {
			// Two rules naming the same target — an environment rule and a catch-all, say — is one
			// target. Emitting it twice would be two changes for one service from one deploy.
			continue
		}
		seen[id] = true
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b *graphv1.Ref) int {
		if c := strings.Compare(a.GetNamespace(), b.GetNamespace()); c != 0 {
			return c
		}
		return strings.Compare(a.GetValue(), b.GetValue())
	})
	return out
}

// Rollouts is what one observed deploy becomes: one change per target, or one unattached change.
type Rollouts struct {
	// Changes is one entry per target, each with its own identity and the same origin.
	Changes []TargetedChange
	// Unattached is true where nothing named a target and the single change stands for the run
	// itself (Edge case 1).
	Unattached bool
}

// TargetedChange is one change, for one target.
type TargetedChange struct {
	// Ref is this change's identity. It carries the target, because FR-017 makes each target its own
	// change and two changes cannot share an identity.
	Ref *graphv1.Ref
	// Target is the entity it changed. Nil on an unattached change.
	Target *graphv1.Ref
}

// SplitByTarget builds one change per target from a base resource path.
//
// `basePath` is the deployment's or run's resource path — the same string the origin reference and the
// SOURCE_LINK pointer are built from, so all three agree by construction.
func SplitByTarget(basePath string, targets []*graphv1.Ref) (Rollouts, bool) {
	base, ok := ResourcePath(basePath)
	if !ok {
		return Rollouts{}, false
	}
	if len(targets) == 0 {
		ref, ok := changeRef(base)
		if !ok {
			return Rollouts{}, false
		}
		return Rollouts{Changes: []TargetedChange{{Ref: ref}}, Unattached: true}, true
	}
	out := Rollouts{Changes: make([]TargetedChange, 0, len(targets))}
	for _, target := range targets {
		ref, ok := changeRef(base + "/targets/" + target.GetNamespace() + "/" + target.GetValue())
		if !ok {
			return Rollouts{}, false
		}
		out.Changes = append(out.Changes, TargetedChange{Ref: ref, Target: target})
	}
	return out, true
}

// Namespaces are the distinct namespaces this mapping targets, sorted.
//
// A deploy feeder's Description cannot enumerate these statically, and that is a fact about what it is
// for rather than a gap: the namespaces it MINTS are fixed (`github.change`, `github.repo`,
// `deploy.*`), but the entities it attaches changes to belong to whichever connector owns them —
// `k8s.deployment`, `gcp.cloudrun.service` — and which of those an installation touches is the
// operator's mapping. Declaring a fixed list would either exclude a real target, which the conformance
// harness rejects, or declare every namespace in the SDK, which declares nothing.
func (m TargetMap) Namespaces() []string {
	seen := map[string]bool{}
	for _, rules := range m.Repositories {
		for _, rule := range rules {
			if !rule.valid() {
				continue
			}
			seen[strings.TrimSpace(rule.Namespace)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	slices.Sort(out)
	return out
}
