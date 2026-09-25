// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The deploy vocabulary's value forms (feature 004 FR-041, FR-046;
// specs/004-deploy-feeders/contracts/deploy-claims.md §1).
//
// ---------------------------------------------------------------------------------------------
// Why a normaliser and not a documented convention
//
// A namespace whose value form is unfixed does not deliver the join it exists for. Feature 003
// shipped three certain rules — C4, C5, C7 — that were published, registered, evaluated on every
// claim and never fired; two of them because the two sides spelled one identifier differently and
// nothing compared them. So the value form is a function here rather than a sentence in a document,
// and every connector that mints one of these namespaces calls it.
//
// # Omission over invention
//
// Every function below returns `(value, ok)` and ok is false where the platform stated nothing this
// namespace can carry. The caller **omits the claim**; it does not mint an empty one, and it does not
// widen a partial value into a guess. FR-041 states this and the reason is asymmetric: a missing
// claim costs a merge that could have happened, while an invented one merges two rollouts that are
// not the same rollout — and a confidently wrong "one change" is the failure this project keeps
// finding.
//
// Two places that principle is stricter than the contract's prose, both recorded in §1 of the
// contract:
//
//   - **An image stating only a tag is omitted.** The contract asks for "the digest form where the
//     platform states both". Where a platform states only a mutable tag there is no digest to prefer,
//     and the honest answer is no claim: two unrelated rollouts both running `:latest` would merge
//     into one under C8. A tag is a name for whatever is there now, not for what was deployed.
//   - **A commit is accepted at 40 or 64 hex digits.** The contract fixes 40, which is git's SHA-1
//     object id. Git's SHA-256 object format is 64, and refusing it would silently drop a claim a
//     repository genuinely stated. The two lengths cannot collide, so accepting both costs nothing.
//     Anything shorter is an abbreviation and is refused.

// CommitSHA normalises a stated commit identifier to NSDeployCommitSHA's value form: the full hex
// object id, lower-cased.
//
// An abbreviated sha — the `a1b2c3d` a platform shows in a UI — returns false. It is not padded, not
// prefix-matched and not stored: a 7-hex prefix is shared by many commits across an organisation's
// repositories, and a claim on it would merge rollouts of different code.
func CommitSHA(stated string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(stated))
	if len(value) != 40 && len(value) != 64 {
		return "", false
	}
	if !isHex(value) {
		return "", false
	}
	return value, true
}

// Image normalises a stated container image reference to NSDeployImage's value form: the digest
// form, `<name>@<algorithm>:<hex>`, with any tag dropped.
//
// A reference stating only a tag returns false — see the file comment. The digest half is
// lower-cased because a digest is hex; the name half is left exactly as stated, because a registry
// path is case-sensitive in the general case and rewriting it would invent a second image.
func Image(stated string) (string, bool) {
	value := strings.TrimSpace(stated)
	at := strings.LastIndex(value, "@")
	if at < 0 {
		return "", false // a tag, or a bare name: nothing immutable was stated
	}
	name, digest := value[:at], strings.ToLower(value[at+1:])
	// Drop a tag that precedes the digest: `repo:v3@sha256:…` and `repo@sha256:…` are one image, and
	// a tag in the value would make them two.
	if slash := strings.LastIndex(name, "/"); strings.LastIndex(name, ":") > slash {
		name = name[:strings.LastIndex(name, ":")]
	}
	algorithm, hex, found := strings.Cut(digest, ":")
	if name == "" || !found || algorithm == "" || hex == "" || !isHex(hex) {
		return "", false
	}
	return name + "@" + algorithm + ":" + hex, true
}

// Release normalises a stated release identifier to NSDeployRelease's value form, which the contract
// leaves as the platform states it: trimmed, and refused when there is nothing to carry.
//
// Deliberately not lower-cased. A release identifier is a tag a human chose — `v2.3.0-RC1` — and
// case-folding it would invent an identifier the platform does not use.
func Release(stated string) (string, bool) { return StableIdentifier(stated) }

// Repository normalises a repository to NSGitHubRepo's value form, `<owner>/<repository>`,
// lower-cased.
//
// Lower-cased because GitHub treats both halves case-insensitively: `Acme/Storefront` and
// `acme/storefront` are one repository, and two sources spelling it differently would otherwise be
// two.
func Repository(owner, repository string) (string, bool) {
	o := strings.ToLower(strings.TrimSpace(owner))
	r := strings.ToLower(strings.TrimSpace(repository))
	if o == "" || r == "" || strings.ContainsAny(o+r, " \t\n/") {
		return "", false
	}
	return o + "/" + r, true
}

// RepositorySlug normalises a repository already spelled `<owner>/<repository>`, which is how most
// payloads state it.
func RepositorySlug(stated string) (string, bool) {
	owner, repository, found := strings.Cut(strings.TrimSpace(stated), "/")
	if !found {
		return "", false
	}
	return Repository(owner, repository)
}

// StableIdentifier is the guard the change namespaces and NSVercelProject share: a value the platform
// assigned, trimmed, refused when empty or when it carries whitespace.
//
// It is deliberately the weakest check that is actually true. "The platform's stable identifier, never
// the display name" is not a property of a string — `checkout-api` is a plausible id and a plausible
// name — so no function here can enforce it. What enforces it is the connector reading the id field,
// and a test asserting that the ref changes when the id changes and not when the name does. Pretending
// otherwise here would be a guard that cannot fail.
func StableIdentifier(stated string) (string, bool) {
	value := strings.TrimSpace(stated)
	if value == "" || strings.ContainsAny(value, " \t\n\r") {
		return "", false
	}
	return value, true
}

// CommitSHARef builds the NSDeployCommitSHA ref for a stated commit, or nothing.
func CommitSHARef(stated string) (*graphv1.Ref, bool) {
	value, ok := CommitSHA(stated)
	if !ok {
		return nil, false
	}
	return Ref(NSDeployCommitSHA, value), true
}

// ImageRef builds the NSDeployImage ref for a stated image reference, or nothing.
func ImageRef(stated string) (*graphv1.Ref, bool) {
	value, ok := Image(stated)
	if !ok {
		return nil, false
	}
	return Ref(NSDeployImage, value), true
}

// ReleaseRef builds the NSDeployRelease ref for a stated release identifier, or nothing.
func ReleaseRef(stated string) (*graphv1.Ref, bool) {
	value, ok := Release(stated)
	if !ok {
		return nil, false
	}
	return Ref(NSDeployRelease, value), true
}

// RepositoryRef builds the NSGitHubRepo ref for an owner and repository, or nothing.
func RepositoryRef(owner, repository string) (*graphv1.Ref, bool) {
	value, ok := Repository(owner, repository)
	if !ok {
		return nil, false
	}
	return Ref(NSGitHubRepo, value), true
}

// RepositorySlugRef builds the NSGitHubRepo ref from an `<owner>/<repository>` slug, or nothing.
func RepositorySlugRef(stated string) (*graphv1.Ref, bool) {
	value, ok := RepositorySlug(stated)
	if !ok {
		return nil, false
	}
	return Ref(NSGitHubRepo, value), true
}

// VercelProjectRef builds the NSVercelProject ref from a project id, or nothing.
func VercelProjectRef(id string) (*graphv1.Ref, bool) {
	value, ok := StableIdentifier(id)
	if !ok {
		return nil, false
	}
	return Ref(NSVercelProject, value), true
}

// isHex reports whether every byte is a lower-case hex digit. The caller lower-cases first, so an
// upper-case digit reaching here is a bug rather than input.
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}
