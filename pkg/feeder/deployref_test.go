// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The deploy vocabulary's value forms (004 FR-041, FR-046; T011, T012, T013).
//
// The property under test throughout is **omission over invention**: where a platform states nothing
// a namespace can carry, the claim is absent. It is asymmetric on purpose — a missing claim costs a
// merge that could have happened, an invented one merges two rollouts that are not the same rollout.

const (
	sha1Commit   = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	sha256Commit = "9f8e7d6c5b4a39281706f5e4d3c2b1a0987654329f8e7d6c5b4a39281706f5e4"
)

// An abbreviated sha is omitted rather than normalised into a guess (T011).
func TestAnAbbreviatedCommitIsOmittedRatherThanGuessedAt(t *testing.T) {
	t.Parallel()

	for _, abbreviated := range []string{
		"9f8e7d6",         // the seven a UI shows
		"9f8e7d6c5b4a",    // twelve
		sha1Commit[:39],   // one short of the full sha-1
		sha1Commit + "0",  // one long, so not a sha of either length
		sha256Commit[:63], // one short of the full sha-256
		"9f8e7d6c5b4a39281706f5e4d3c2b1a09876543g", // 40 characters, not all hex
		"",                // nothing stated
		"   ",             // nothing but space
		"refs/heads/main", // a ref, not a commit
	} {
		if value, ok := feeder.CommitSHA(abbreviated); ok {
			t.Errorf("CommitSHA(%q) returned %q; an abbreviated or non-hex commit must be omitted, "+
				"because a prefix is shared by many commits and a claim on it would merge rollouts of "+
				"different code", abbreviated, value)
		}
		if ref, ok := feeder.CommitSHARef(abbreviated); ok || ref != nil {
			t.Errorf("CommitSHARef(%q) minted %v; a claim with no stated value is absent, not empty",
				abbreviated, ref)
		}
	}
}

// A full sha is carried, lower-cased, at either of git's two object-id lengths.
func TestAFullCommitIsCarriedLowerCasedAtEitherLength(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ stated, want string }{
		{sha1Commit, sha1Commit},
		{strings.ToUpper(sha1Commit), sha1Commit},
		{"  " + sha1Commit + "\n", sha1Commit},
		{sha256Commit, sha256Commit},
		{strings.ToUpper(sha256Commit), sha256Commit},
	} {
		got, ok := feeder.CommitSHA(tc.stated)
		if !ok {
			t.Errorf("CommitSHA(%q) refused a full commit", tc.stated)
			continue
		}
		if got != tc.want {
			t.Errorf("CommitSHA(%q) = %q, want %q", tc.stated, got, tc.want)
		}
	}
}

// An image stating only a mutable tag is omitted; the digest form is what is carried, and a tag
// alongside a digest is dropped so the two spellings of one image are one value.
func TestOnlyTheImmutableImageFormIsCarried(t *testing.T) {
	t.Parallel()

	const digest = "sha256:1f2e3d4c5b6a798807162534435261708f9e0d1c2b3a49586776859403f2e1d0"

	for _, tag := range []string{
		"europe-west1-docker.pkg.dev/acme/apps/storefront:latest",
		"europe-west1-docker.pkg.dev/acme/apps/storefront:v3.2.1",
		"storefront",
		"",
		"europe-west1-docker.pkg.dev/acme/apps/storefront@",           // an @ with nothing after it
		"europe-west1-docker.pkg.dev/acme/apps/storefront@notadigest", // no algorithm
		"@" + digest, // no name
	} {
		if value, ok := feeder.Image(tag); ok {
			t.Errorf("Image(%q) returned %q; a reference with no immutable digest must be omitted, "+
				"because two unrelated rollouts both running a tag would merge into one", tag, value)
		}
	}

	name := "europe-west1-docker.pkg.dev/acme/apps/storefront"
	for _, stated := range []string{
		name + "@" + digest,
		name + ":v3.2.1@" + digest,
		"  " + name + ":latest@" + strings.ToUpper(digest) + " ",
	} {
		got, ok := feeder.Image(stated)
		if !ok {
			t.Errorf("Image(%q) refused a digest-pinned reference", stated)
			continue
		}
		if got != name+"@"+digest {
			t.Errorf("Image(%q) = %q, want %q; a tag beside a digest must be dropped or the same "+
				"image has two values", stated, got, name+"@"+digest)
		}
	}

	// A registry path is case-sensitive in the general case, so the name half is left as stated even
	// though the digest half is folded. Rewriting it would invent a second image.
	mixed := "Registry.Example/Acme/Storefront@" + digest
	if got, ok := feeder.Image(mixed); !ok || got != mixed {
		t.Errorf("Image(%q) = %q (ok=%v); the name half must be carried exactly as stated", mixed, got, ok)
	}
}

// The repository is lower-cased, because GitHub treats both halves case-insensitively and two
// sources spelling it differently would otherwise be two repositories.
func TestTheRepositoryIsLowerCasedInBothHalves(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ owner, repo, want string }{
		{"acme", "storefront", "acme/storefront"},
		{"Acme", "Storefront", "acme/storefront"},
		{" ACME ", "\tStorefront\n", "acme/storefront"},
	} {
		got, ok := feeder.Repository(tc.owner, tc.repo)
		if !ok || got != tc.want {
			t.Errorf("Repository(%q, %q) = %q (ok=%v), want %q", tc.owner, tc.repo, got, ok, tc.want)
		}
	}
	if got, ok := feeder.RepositorySlug("Acme/Storefront"); !ok || got != "acme/storefront" {
		t.Errorf("RepositorySlug = %q (ok=%v), want acme/storefront", got, ok)
	}
	for _, bad := range []string{"", "acme", "acme/", "/storefront", "acme/store/front", "ac me/storefront"} {
		if got, ok := feeder.RepositorySlug(bad); ok {
			t.Errorf("RepositorySlug(%q) returned %q; a half-stated repository is not a repository", bad, got)
		}
	}
	// Whitespace AROUND a half is trimmed rather than refused: that is a normalisation, not an
	// invention, since no legal repository name carries a space.
	if got, ok := feeder.RepositorySlug(" acme / storefront "); !ok || got != "acme/storefront" {
		t.Errorf("RepositorySlug with surrounding space = %q (ok=%v), want acme/storefront", got, ok)
	}
}

// The environment is a supporting attribute, not part of a target claim's value (T012, FR-042).
//
// One namespace distinguishes production from staging: `checkout` in production and `checkout` in
// staging are one namespace and one value with different attributes, so a rule can require agreement
// on the environment without the namespace multiplying. The strongest form of this assertion is that
// no normaliser here *takes* an environment — the signatures enforce it — and this test carries the
// rest: the value is identical for the same commit in two environments, and the published place the
// environment lives is an attribute name rather than a namespace.
func TestTheEnvironmentIsASupportingAttributeAndNotPartOfTheValue(t *testing.T) {
	t.Parallel()

	production, okProd := feeder.CommitSHARef(sha1Commit)
	staging, okStage := feeder.CommitSHARef(sha1Commit)
	if !okProd || !okStage {
		t.Fatal("a full commit was refused")
	}
	if production.GetValue() != staging.GetValue() || production.GetNamespace() != staging.GetNamespace() {
		t.Errorf("the same commit produced %v and %v; the environment must not reach the value",
			production, staging)
	}

	if feeder.IsWellKnownNamespace(feeder.AttrDeploymentEnvironment) {
		t.Errorf("%q is published as an identifier namespace; the environment is an attribute, and a "+
			"namespace per environment is one entity per environment", feeder.AttrDeploymentEnvironment)
	}
	for _, ns := range []string{
		feeder.NSDeployCommitSHA, feeder.NSDeployImage, feeder.NSDeployRelease,
		feeder.NSGitHubRepo, feeder.NSGitHubChange, feeder.NSVercelProject, feeder.NSVercelChange,
	} {
		if strings.Contains(ns, "production") || strings.Contains(ns, "staging") ||
			strings.Contains(ns, "environment") {
			t.Errorf("namespace %q names an environment; one namespace serves every environment", ns)
		}
	}
}

// Every namespace this feature emits is inside the SDK's declared set (T013, FR-010).
func TestEveryDeployNamespaceIsPublishedBySDK(t *testing.T) {
	t.Parallel()

	for _, ns := range []string{
		feeder.NSDeployCommitSHA, feeder.NSDeployImage, feeder.NSDeployRelease,
		feeder.NSGitHubRepo, feeder.NSGitHubChange, feeder.NSVercelProject, feeder.NSVercelChange,
	} {
		if !feeder.IsWellKnownNamespace(ns) {
			t.Errorf("%q is minted by this feature but is not in WellKnownNamespaces; certain rule C8 "+
				"keys on it, and a rule cannot key on a string the SDK does not publish", ns)
		}
	}
	// And the list is sorted, because it is read as a published set and a reordering diff is noise.
	for i := 1; i < len(feeder.WellKnownNamespaces); i++ {
		if feeder.WellKnownNamespaces[i-1] >= feeder.WellKnownNamespaces[i] {
			t.Errorf("WellKnownNamespaces is not sorted: %q before %q",
				feeder.WellKnownNamespaces[i-1], feeder.WellKnownNamespaces[i])
		}
	}
}

// A stable identifier is trimmed and refused when empty, and the guard says plainly what it cannot
// check rather than pretending to check it.
func TestStableIdentifierRefusesWhatItCanAndSaysSoAboutTheRest(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", "   ", "\t", "dpl_ 123", "two words"} {
		if got, ok := feeder.StableIdentifier(bad); ok {
			t.Errorf("StableIdentifier(%q) returned %q", bad, got)
		}
	}
	if got, ok := feeder.StableIdentifier("  dpl_9aBc  "); !ok || got != "dpl_9aBc" {
		t.Errorf("StableIdentifier = %q (ok=%v), want dpl_9aBc", got, ok)
	}
	// Release goes through the same guard and is deliberately NOT lower-cased: a release identifier is
	// a tag a human chose, and case-folding it would invent one the platform does not use.
	if got, ok := feeder.Release("v2.3.0-RC1"); !ok || got != "v2.3.0-RC1" {
		t.Errorf("Release = %q (ok=%v), want v2.3.0-RC1 unchanged", got, ok)
	}
}
