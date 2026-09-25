// SPDX-License-Identifier: Apache-2.0

package github

import "github.com/Pierre-Theophile/aisre/pkg/feeder"

// The published read-only operation surface (004 FR-004, SC-007; docs/connectors/github.md §2).
//
// Every constant below appears on that page, and a test compares the two in both directions — a row
// on the page the process would refuse fails the build, and so does an operation the process carries
// that the page does not publish. That matters more than it sounds: the page is what an operator
// approves, and an approval that can drift from the system is an approval of a document.
//
// The generic half — what makes an operation admissible, and the refusal when it is not — is
// pkg/feeder's, shared with every other REST connector so that no connector can define it away. What
// lives here is only this platform's table.

const (
	// The grant itself. FR-008: the repositories in scope are what the installation was granted,
	// enumerated from GitHub rather than read from a file.
	OpInstallationRepositories feeder.ReadOperation = "GET /installation/repositories"

	// Deployments and their statuses: the rollout and how it ended (US1).
	OpDeployments        feeder.ReadOperation = "GET /repos/{owner}/{repo}/deployments"
	OpDeployment         feeder.ReadOperation = "GET /repos/{owner}/{repo}/deployments/{deployment_id}"
	OpDeploymentStatuses feeder.ReadOperation = "GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses"

	// Workflow runs: the deploy pipeline whose conclusion a rollout hangs off.
	OpWorkflowRuns feeder.ReadOperation = "GET /repos/{owner}/{repo}/actions/runs"
	OpWorkflowRun  feeder.ReadOperation = "GET /repos/{owner}/{repo}/actions/runs/{run_id}"

	// Releases: a tagged release is a change even where no deployment records it.
	OpReleases feeder.ReadOperation = "GET /repos/{owner}/{repo}/releases"
	OpRelease  feeder.ReadOperation = "GET /repos/{owner}/{repo}/releases/{release_id}"

	// The budget's opening reading. Documented as **not counting against the primary rate limit** —
	// but the same documentation says it CAN count against the secondary one, so it is cheap rather
	// than free and the budget reads it once per cycle rather than whenever it feels like it
	// (FR-071, research §2).
	OpRateLimit feeder.ReadOperation = "GET /rate_limit"
)

// Platform is the name this connector's refusals are raised under.
const Platform = "github"

// Surface is the set of operations this connector may issue. It is built with the Must form so a
// table containing a write fails at program start rather than in a test somebody can forget to run.
var Surface = feeder.MustReadOnlySurface(Platform, map[feeder.ReadOperation]feeder.ReadOperationSpec{
	OpInstallationRepositories: {
		Area: "scope",
		Why:  "the repositories the installation was granted, enumerated rather than configured",
	},
	OpDeployments: {
		Area: "deployments",
		Why:  "a deployment is a rollout of a commit to an environment",
	},
	OpDeployment: {
		Area: "deployments",
		Why:  "one rollout, when a status names a deployment the list window no longer covers",
	},
	OpDeploymentStatuses: {
		Area: "deployments",
		Why:  "how the rollout ended, and when; no status is designated terminal by the API",
	},
	OpWorkflowRuns: {
		Area: "workflow runs",
		Why:  "the deploy pipeline, its conclusion and the actor that started it",
	},
	OpWorkflowRun: {
		Area: "workflow runs",
		Why:  "one run, to close an extent whose last page is already out of the list window",
	},
	OpReleases: {
		Area: "releases",
		Why:  "a tagged release is a change even where no deployment records it",
	},
	OpRelease: {
		Area: "releases",
		Why:  "one release, by id, when a deployment references it",
	},
	OpRateLimit: {
		Area: "budget",
		Why:  "the remaining quota the cycle's budget is a share of; free against the primary limit, so once a cycle",
	},
})

// Issuable decides whether an operation may be issued at all, before any quota is spent.
//
// It is the read-only gate on its own, independent of the budget: an unmetered run — a replay from
// disk, a unit test — is never a reason to stop checking what may be called. The metered door that
// also spends quota is `Issue`, which arrives with the budget (T035) and calls this first.
func Issuable(op feeder.ReadOperation) (feeder.ReadOperationSpec, error) {
	return Surface.Issuable(op)
}
