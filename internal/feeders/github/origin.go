// SPDX-License-Identifier: Apache-2.0

package github

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The origin reference and the pointers hanging off it (004 T054, T055; FR-014, FR-029, FR-050).
//
// ---------------------------------------------------------------------------------------------
// Why the selector is built from identifiers and never from a URL the payload gave us
//
// FR-014 asks for an origin link carrying **no credential and no token**, and the cheapest way to
// honour that is to have no URL from the payload in the path at all. A GitHub deployment status
// carries three URLs — `target_url`, `environment_url`, `log_url` — and every one of them is
// **supplied by whoever created the status**, not by GitHub. They point wherever that person's deploy
// tool pointed them, they routinely carry query strings, and a query string is where a token ends up.
//
// So the selector is assembled here from the repository and the object's numeric id, in the
// `github-resource/v1` vocabulary, and the deployer-supplied URLs are not minted as pointers at all.
// A pointer this connector emits addresses an object in GitHub's API; it is not a redirect to
// somewhere a third party chose.
//
// The one URL that IS GitHub's own is a workflow run's `logs_url`, which is why the LOG pointer below
// can exist — and it is still reduced to a path in the same vocabulary rather than carried as a URL,
// so that the two pointers read the same way and neither carries a host or a query.
//
// # Why the host is not in the selector
//
// pkg/feeder's vocabulary documents it: a pointer is read back years later, and GitHub Enterprise
// Server serves the same paths under a different host. The host belongs to the connector's
// configuration, not to the object.
//
// # Why reading the logs is still out of scope
//
// A LOG pointer is a **link**, and doc.go's promise is about reading: a run's conclusion is a fact
// about a rollout, its logs are the application's own output, and this feeder fetches none of it. The
// pointer exists so that a person investigating can go and look — which is the whole of FR-029.

// BackendKind names the system a pointer addresses.
const BackendKind = "github"

// The change's identity is built from numeric ids and never from a name (Edge case 9, T070).
//
// `repositories/{repository_id}/…`, which is a real GitHub path, and not `repos/{owner}/{repo}/…`,
// which is the one the API is usually read through. The difference is the whole of Edge case 9: a
// repository can be renamed and a workflow retitled, and neither is a new change — but an identity
// spelled with the name would become a DIFFERENT identity the moment somebody renamed the repository,
// and the graph would retract that repository's entire history and create it again under the new name.
//
// The name still appears in the SOURCE_LINK pointer and in the origin link, which are addresses rather
// than identities and which GitHub redirects after a rename. Keeping the two apart is what lets both
// be right.

// DeploymentChangeRef identifies the change a deployment is, in the `github.change` namespace.
func DeploymentChangeRef(repositoryID, deploymentID int64) (*graphv1.Ref, bool) {
	return changeRef(stablePath(repositoryID, "deployments", deploymentID))
}

// RunChangeRef identifies the change a workflow run is, keyed on the run **and its attempt**.
//
// The attempt is in the key because FR-028 makes a re-run a NEW change rather than an amendment of
// the earlier one: somebody pressed the button again, production moved again, and collapsing the two
// would retract a rollout that really happened.
func RunChangeRef(repositoryID, runID int64, attempt int) (*graphv1.Ref, bool) {
	path := stablePath(repositoryID, "actions/runs", runID)
	if attempt > 0 {
		path += "/attempts/" + strconv.Itoa(attempt)
	}
	return changeRef(path)
}

// ReleaseChangeRef identifies the change a release is.
func ReleaseChangeRef(repositoryID, releaseID int64) (*graphv1.Ref, bool) {
	return changeRef(stablePath(repositoryID, "releases", releaseID))
}

// stablePath renders `repositories/{repository_id}/{collection}/{id}`.
func stablePath(repositoryID int64, collection string, id int64) string {
	return fmt.Sprintf("repositories/%d/%s/%d", repositoryID, collection, id)
}

// ObjectIDFromPath reads an object's numeric ids back out of a stable identity path, for a caller
// holding an identity rather than a payload. `collection` may itself contain a slash (`actions/runs`).
func ObjectIDFromPath(path, collection string) (repositoryID, objectID int64, ok bool) {
	normalised, valid := ResourcePath(path)
	if !valid {
		return 0, 0, false
	}
	rest, found := strings.CutPrefix(normalised, "repositories/")
	if !found {
		return 0, 0, false
	}
	repoPart, rest, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	idText, found := strings.CutPrefix(rest, collection+"/")
	if !found || idText == "" || strings.Contains(idText, "/") {
		return 0, 0, false
	}
	repository, err := strconv.ParseInt(repoPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	object, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return repository, object, true
}

// OriginLink is the human-openable link recorded on the change (ChangeFact.OriginRef).
//
// It is GitHub's own `html_url` rather than one of the deployer-supplied URLs, and its query and
// fragment are dropped: a stored link that carries a query is a stored link that can carry a token
// (FR-014). A value that is not an http(s) URL yields nothing rather than being recorded as a link
// to somewhere unknown.
func OriginLink(htmlURL string) (string, bool) {
	trimmed := strings.TrimSpace(htmlURL)
	if trimmed == "" {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", false
	}
	if parsed.User != nil {
		// `https://user:token@host/…`. Dropped whole rather than repaired: a link somebody built with
		// credentials in it is not a link this connector stores.
		return "", false
	}
	parsed.RawQuery, parsed.Fragment, parsed.User = "", "", nil
	return parsed.String(), true
}

// The SOURCE_LINK pointers, one per object kind.
//
// Structured rather than taking a path, so that a call site cannot assemble a wrong one — and so the
// path builders below have real callers rather than being exported on the promise that something will
// use them later. The selector is the object's **address** in GitHub's API, which carries the
// repository's name: addresses are not identities, GitHub redirects a renamed repository's paths, and
// a human following the link wants the name they know. The identity is `…ChangeRef`, above, and it
// carries numeric ids only.
func DeploymentSourceLink(repo Repo, deploymentID int64) (*graphv1.Pointer, bool) {
	return sourceLink(deploymentPath(repo, deploymentID))
}

// RunSourceLink is the SOURCE_LINK pointer for one workflow run.
func RunSourceLink(repo Repo, runID int64) (*graphv1.Pointer, bool) {
	return sourceLink(runPath(repo, runID))
}

// ReleaseSourceLink is the SOURCE_LINK pointer for one release.
func ReleaseSourceLink(repo Repo, releaseID int64) (*graphv1.Pointer, bool) {
	return sourceLink(releasePath(repo, releaseID))
}

func sourceLink(path string) (*graphv1.Pointer, bool) {
	selector, ok := ResourcePath(path)
	if !ok {
		return nil, false
	}
	return feeder.SourceLinkPointer(BackendKind, feeder.VocabGitHubResource, selector, nil), true
}

// RunLogPointer is the LOG pointer for a workflow run's job logs, where the run exposes them.
//
// `logsURL` is GitHub's own `logs_url`, and it is reduced to a path in the resource vocabulary: the
// host is configuration and a query string is where a token would be. A run that exposes no logs URL
// yields no pointer, rather than one pointing at a path this code guessed.
func RunLogPointer(logsURL string) (*graphv1.Pointer, bool) {
	selector, ok := ResourcePath(logsURL)
	if !ok {
		return nil, false
	}
	return feeder.NewPointer(
		graphv1.PointerKind_LOG, BackendKind, feeder.VocabGitHubResource, selector, nil), true
}

// Paths, assembled from identifiers. Each is the path template of a published operation with its
// parameters filled in, which is why they read the same as [Surface]'s table.
func deploymentPath(repo Repo, id int64) string {
	return fmt.Sprintf("repos/%s/%s/deployments/%d", repo.Owner, repo.Name, id)
}

func runPath(repo Repo, id int64) string {
	return fmt.Sprintf("repos/%s/%s/actions/runs/%d", repo.Owner, repo.Name, id)
}

func releasePath(repo Repo, id int64) string {
	return fmt.Sprintf("repos/%s/%s/releases/%d", repo.Owner, repo.Name, id)
}

// RunLogsPath is the path a run's logs live at, for a run that exposes them.
func RunLogsPath(repo Repo, id int64) string { return runPath(repo, id) + "/logs" }

// ResourcePath normalises anything that identifies a GitHub object into a `github-resource/v1`
// selector: no scheme, no host, no leading slash, no query and no fragment.
//
// It takes a URL as readily as a path because the payloads hand over both, and it strips rather than
// refuses for the parts that are configuration (scheme, host) — but it **drops the query and the
// fragment**, which is the clause FR-014 turns on. An `?access_token=…` on a URL somebody handed us
// is exactly the thing a stored pointer must not carry, and stripping it is not a repair: the path is
// still the object's address, and whatever the query said was not part of the object's identity.
func ResourcePath(stated string) (string, bool) {
	trimmed := strings.TrimSpace(stated)
	if trimmed == "" {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	path := parsed.Path
	if path == "" {
		// Not a URL: a bare path, which is what this package's own builders produce.
		path = trimmed
	}
	// A token could also be sitting in userinfo (`https://user:token@host/…`), which never appears in
	// Path — dropping everything but the path handles it by construction rather than by inspection.
	path = strings.Trim(path, "/")
	if path == "" {
		return "", false
	}
	// GitHub's API paths for these objects always start with `repos/`; an installation-wide path does
	// not, and neither is refused here because this vocabulary addresses whatever GitHub addresses.
	if strings.ContainsAny(path, "?#") {
		// Unreachable through url.Parse, and asserted rather than assumed: a hand-built path that
		// carried a query would otherwise be stored as one.
		return "", false
	}
	return path, true
}

func changeRef(path string) (*graphv1.Ref, bool) {
	selector, ok := ResourcePath(path)
	if !ok {
		return nil, false
	}
	return feeder.Ref(feeder.NSGitHubChange, selector), true
}

// DeploymentIDFromPath reads the deployment's numeric id out of a stable identity path.
func DeploymentIDFromPath(path string) (int64, bool) {
	_, id, ok := ObjectIDFromPath(path, "deployments")
	return id, ok
}
