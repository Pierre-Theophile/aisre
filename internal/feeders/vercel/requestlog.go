// SPDX-License-Identifier: Apache-2.0

package vercel

import "github.com/Pierre-Theophile/aisre/pkg/feeder"

// The published read-only operation surface (004 FR-004, SC-007; docs/connectors/vercel.md §2).
//
// Every constant below appears on that page, and a test compares the two in both directions. The
// generic half — what makes an operation admissible, and the refusal when it is not — is pkg/feeder's,
// shared with every other REST connector; what lives here is only this platform's table.
//
// Vercel versions its API in the path, so a version is part of an operation's identity: `/v7` and
// `/v13` below are the versions this connector is written against, and changing one is a change to the
// published surface rather than an implementation detail.

const (
	// Deployments: the rollout itself (US2).
	OpDeployments feeder.ReadOperation = "GET /v7/deployments"
	OpDeployment  feeder.ReadOperation = "GET /v13/deployments/{idOrUrl}"

	// Projects: the mapping from a deployment to the service it serves (FR-034).
	OpProjects feeder.ReadOperation = "GET /v9/projects"
	OpProject  feeder.ReadOperation = "GET /v9/projects/{idOrName}"

	// Environment-variable METADATA. The decrypted value is never requested (FR-038); see the package
	// comment, and the page's note that this version is provisional until research §5.3 closes.
	OpProjectEnv feeder.ReadOperation = "GET /v9/projects/{idOrName}/env"
)

// Platform is the name this connector's refusals are raised under.
const Platform = "vercel"

// Surface is the set of operations this connector may issue. Built with the Must form so a table
// containing a write fails at program start rather than in a test somebody can forget to run.
var Surface = feeder.MustReadOnlySurface(Platform, map[feeder.ReadOperation]feeder.ReadOperationSpec{
	OpDeployments: {
		Area: "deployments",
		Why:  "the rollouts, their target, their state and their production substate",
	},
	OpDeployment: {
		Area: "deployments",
		Why:  "one deployment, when the list window no longer covers a referenced id",
	},
	OpProjects: {
		Area: "projects",
		Why:  "the project a deployment belongs to, which is what maps it to a service",
	},
	OpProject: {
		Area: "projects",
		Why:  "one project, by id or name, as the operator configured it",
	},
	OpProjectEnv: {
		Area: "configuration",
		Why:  "environment-variable keys, environments and versions; never a decrypted value",
	},
})

// Issuable decides whether an operation may be issued at all, before any quota is spent.
//
// Independent of the budget by design: an unmetered run — a replay from disk, a unit test — is never a
// reason to stop checking what may be called. The metered door that also spends quota is `Issue`,
// which arrives with the budget (T035–T036) and calls this first.
func Issuable(op feeder.ReadOperation) (feeder.ReadOperationSpec, error) {
	return Surface.Issuable(op)
}
