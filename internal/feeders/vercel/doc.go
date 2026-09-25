// SPDX-License-Identifier: Apache-2.0

// Package vercel is the Vercel deploy feeder: it turns a project's deployments and its production
// environment-variable changes into change nodes on the graph (feature 004, US2 and US3; FR-031).
//
// # What it reads
//
// Exactly the operations in [Surface], published in docs/connectors/vercel.md and nowhere else: the
// deployments (list and get), the projects, and the environment-variable **metadata**.
//
// # What it deliberately does not read
//
//   - **The decrypted value of an environment variable, ever** (FR-038). The key, its environments and
//     its version are what a CONFIG change is about; the value is the secret itself, and a feeder that
//     asked for it would be putting production credentials into the graph. The endpoint on the surface
//     returns metadata, and the query that would decrypt is not on it.
//   - **Build and function logs.** A deployment's state is a fact about a rollout; its build output is
//     the application's own text.
//   - **The deployed files.** `GET /v6/deployments/{id}/files` and the file-contents endpoint are
//     absent: the graph joins on the commit sha, not on what was built from it.
//   - **Team members and access groups.** The actor on a change comes from the payload recording the
//     change (`creator.type`, FR-037); who else can log in is a different subject.
//
// # Ready is not live: why this feeder reads `readySubstate`
//
// Vercel reports both `readyState`/`state` and, once `READY`, a `readySubstate` of `STAGED`,
// `ROLLING` or `PROMOTED` — documented as tracking whether the deployment has seen production
// traffic. `READY` means built and available, not serving.
//
// So a rollout is `target=production` **and** `readySubstate=PROMOTED`, and its valid time is the
// instant it became the production deployment rather than the instant it finished building (research
// §3.1, US2 acceptance scenario 5). Treating `READY` as live would make every staged deployment a
// change claiming production moved when it had not.
//
// For the same reason `isRollbackCandidate` is not a rollback: it says a deployment *can* be rolled
// back to, not that one happened (research §5.2).
//
// # Read-only, with no exception at all
//
// Constitution VII permits no write to any production system, and FR-004 asks for write-incapability
// verifiable from **the set of operations the feeder can issue** rather than from one clean run.
// [Surface] is that set: every call looks its operation up in it, and an operation it does not carry
// is refused before any quota is spent.
//
// Nothing here is promoted, rolled back, redeployed, cancelled or deleted, and no project setting or
// environment variable is changed. Unlike feature 003's GCP connector there is no declared state
// change of any kind, so the surface's state-change count is zero
// (contracts/read-only-operations.md §2).
//
// # Scope is configuration inside the grant, and the feeder says so
//
// A Vercel token authenticates as a user or a team; the project filter is a query parameter the
// caller chooses, not a grant the platform enforces on the token. That is the opposite regime from
// GitHub's installation selection, and FR-008 asks the connector to be honest about which it is in:
// the operator's configured project list is a **narrowing of what the token could read**, not a
// boundary the platform holds. Confirming whether a token can itself be pinned to a project set is
// research §5.3, still open.
package vercel
