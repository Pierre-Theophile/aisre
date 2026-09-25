// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The origin reference and its pointers (004 T054, T055; FR-014, FR-029, FR-050).

var storefront = github.Repo{Owner: "acme", Name: "storefront"}

// GitHub's numeric repository ids. A change's identity is built from these and never from a name,
// because a rename does not touch them (Edge case 9).
const (
	storefrontID int64 = 555
	monorepoID   int64 = 556
)

// The change's identity is built from the platform's stable identifiers, in the namespace a
// cross-source rule can find it in.
func TestAChangeIsIdentifiedByThePlatformsStableIdentifiers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		got  func() (*graphv1.Ref, bool)
		want string
	}{
		"a deployment": {
			got:  func() (*graphv1.Ref, bool) { return github.DeploymentChangeRef(storefrontID, 4321) },
			want: "repositories/555/deployments/4321",
		},
		"a run, with its attempt": {
			got:  func() (*graphv1.Ref, bool) { return github.RunChangeRef(storefrontID, 99, 2) },
			want: "repositories/555/actions/runs/99/attempts/2",
		},
		"a release": {
			got:  func() (*graphv1.Ref, bool) { return github.ReleaseChangeRef(storefrontID, 7) },
			want: "repositories/555/releases/7",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref, ok := tc.got()
			if !ok {
				t.Fatal("no ref was minted")
			}
			if ref.GetNamespace() != feeder.NSGitHubChange {
				t.Errorf("namespace = %q, want %q; a rule cannot key on a namespace only one package "+
					"can see", ref.GetNamespace(), feeder.NSGitHubChange)
			}
			if ref.GetValue() != tc.want {
				t.Errorf("value = %q, want %q", ref.GetValue(), tc.want)
			}
		})
	}
}

// FR-028: a re-run is a new change, so the attempt is part of the key. Collapsing the two would
// retract a rollout that really happened.
func TestAReRunIsADifferentChangeFromTheAttemptBeforeIt(t *testing.T) {
	t.Parallel()
	first, _ := github.RunChangeRef(storefrontID, 99, 1)
	second, _ := github.RunChangeRef(storefrontID, 99, 2)
	if first.GetValue() == second.GetValue() {
		t.Errorf("attempt 1 and attempt 2 of run 99 are both %q; a re-run is a NEW change (FR-028) and "+
			"one key for both would amend the earlier rollout out of existence", first.GetValue())
	}
}

// FR-014: nothing this connector stores as a link carries a credential. The deployer-supplied URLs are
// where one would arrive, so the selector is assembled from identifiers instead.
func TestNoStoredLinkCarriesACredential(t *testing.T) {
	t.Parallel()
	const token = "ghp_thisIsACredential"

	// A path handed over with a token in its query keeps the object's address and drops the query.
	selector, ok := github.ResourcePath(
		"https://api.github.com/repos/acme/storefront/deployments/4321?access_token=" + token)
	if !ok {
		t.Fatal("a URL carrying a query was refused outright; the path is still the object's address")
	}
	if strings.Contains(selector, token) || strings.ContainsAny(selector, "?#") {
		t.Errorf("the selector is %q; a stored pointer that carries a query is a stored pointer that "+
			"can carry a token (FR-014)", selector)
	}
	if selector != "repos/acme/storefront/deployments/4321" {
		t.Errorf("the selector is %q, want the bare resource path", selector)
	}

	// Userinfo is the other place one hides, and it never appears in a URL's path at all.
	userinfo, ok := github.ResourcePath("https://x-access-token:" + token + "@api.github.com/repos/acme/storefront/releases/7")
	if !ok || strings.Contains(userinfo, token) {
		t.Errorf("the selector is %q, want the path with the credentials in the authority dropped", userinfo)
	}

	// And the human-openable link refuses such a URL whole rather than repairing it.
	if link, ok := github.OriginLink("https://user:" + token + "@github.com/acme/storefront/actions/runs/99"); ok {
		t.Errorf("OriginLink kept %q; a link somebody built with credentials in it is not a link this "+
			"connector stores", link)
	}
}

// The origin link is GitHub's own, and its query and fragment are dropped.
func TestTheOriginLinkIsTheHumanOneWithNoQuery(t *testing.T) {
	t.Parallel()
	got, ok := github.OriginLink("https://github.com/acme/storefront/actions/runs/99?check_suite_focus=true#step:4:1")
	if !ok {
		t.Fatal("GitHub's own html_url was refused")
	}
	if got != "https://github.com/acme/storefront/actions/runs/99" {
		t.Errorf("OriginLink = %q, want the link with its query and fragment dropped", got)
	}
	for _, bad := range []string{"", "   ", "not a url", "javascript:alert(1)", "/relative/path", "ftp://host/x"} {
		if link, ok := github.OriginLink(bad); ok {
			t.Errorf("OriginLink(%q) = %q; a value that is not an http(s) URL is not a link", bad, link)
		}
	}
}

// The SOURCE_LINK pointer addresses the object in GitHub's API, with no host — the host is the
// connector's configuration, and GitHub Enterprise Server serves the same paths elsewhere.
func TestTheSourceLinkPointerCarriesAPathAndNoHost(t *testing.T) {
	t.Parallel()
	pointer, ok := github.DeploymentSourceLink(storefront, 4321)
	if !ok {
		t.Fatal("no pointer was minted")
	}
	switch {
	case pointer.GetKind() != graphv1.PointerKind_SOURCE_LINK:
		t.Errorf("kind = %v, want SOURCE_LINK", pointer.GetKind())
	case pointer.GetVocabulary() != feeder.VocabGitHubResource:
		t.Errorf("vocabulary = %q, want %q", pointer.GetVocabulary(), feeder.VocabGitHubResource)
	case pointer.GetBackendKind() != github.BackendKind:
		t.Errorf("backend kind = %q, want %q", pointer.GetBackendKind(), github.BackendKind)
	case strings.Contains(pointer.GetSelector(), "://") || strings.Contains(pointer.GetSelector(), "github.com"):
		t.Errorf("the selector %q carries a host; a pointer is read back years later and an Enterprise "+
			"Server installation serves the same paths under a different one", pointer.GetSelector())
	case pointer.GetSelector() != "repos/acme/storefront/deployments/4321":
		t.Errorf("the selector is %q", pointer.GetSelector())
	}
}

// FR-029: the LOG pointer links to the run's job logs where the run exposes them, and nothing at all
// where it does not — a pointer at a path this code guessed would send an investigator to a 404.
func TestTheLogPointerExistsOnlyWhereTheRunExposesLogs(t *testing.T) {
	t.Parallel()
	pointer, ok := github.RunLogPointer("https://api.github.com/repos/acme/storefront/actions/runs/99/logs")
	if !ok {
		t.Fatal("a run exposing a logs URL got no LOG pointer")
	}
	if pointer.GetKind() != graphv1.PointerKind_LOG {
		t.Errorf("kind = %v, want LOG", pointer.GetKind())
	}
	if pointer.GetSelector() != github.RunLogsPath(storefront, 99) {
		t.Errorf("the selector is %q, want %q", pointer.GetSelector(), github.RunLogsPath(storefront, 99))
	}
	if _, ok := github.RunLogPointer(""); ok {
		t.Error("a run exposing no logs URL got a pointer anyway; it would send an investigator to a " +
			"path this code invented")
	}
}

// A path can be read back to the identifier it was built from, for a caller holding a selector rather
// than a payload — and a path of another shape is refused rather than half-parsed.
func TestADeploymentPathReadsBackToItsIdentifier(t *testing.T) {
	t.Parallel()
	id, ok := github.DeploymentIDFromPath("repositories/555/deployments/4321")
	if !ok || id != 4321 {
		t.Errorf("DeploymentIDFromPath = %d/%v, want 4321", id, ok)
	}
	for _, bad := range []string{
		"repositories/555/actions/runs/99",
		"repositories/555/deployments/4321/statuses",
		"repositories/555/deployments/not-a-number",
		"repos/acme/storefront/deployments/4321", // an address, not an identity
		"",
	} {
		if id, ok := github.DeploymentIDFromPath(bad); ok {
			t.Errorf("DeploymentIDFromPath(%q) = %d, want a refusal", bad, id)
		}
	}
}
