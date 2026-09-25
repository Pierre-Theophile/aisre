// SPDX-License-Identifier: Apache-2.0

// Package github is the GitHub deploy feeder: it turns a repository's deployments, workflow runs and
// releases into change nodes on the graph (feature 004, US1; FR-021).
//
// # What it reads
//
// Exactly the operations in [Surface], published in docs/connectors/github.md and nowhere else. In
// one sentence: the installation's repository selection, and then per repository the deployments,
// their statuses, the workflow runs and the releases — plus the free rate-limit endpoint the budget
// opens each cycle against.
//
// # What it deliberately does not read
//
// This list is as much of the contract as the one above, because an operator granting an App
// installation is granting more than this package uses:
//
//   - **Secrets and Actions variables, in any form.** Not their values, not their names, not the fact
//     that one exists. A deploy feeder has no question that needs them.
//   - **Job logs and step output.** A workflow run's conclusion is a fact about a rollout; its logs
//     are the application's own output, and reading them would put arbitrary build text — which
//     routinely contains tokens people pasted — into the graph.
//   - **Repository contents, diffs and file trees.** The commit sha is an identifier the graph joins
//     on (FR-041); the code at that sha is not evidence about a rollout.
//   - **Issues, pull requests, reviews and comments.** Human discussion is not a change to a
//     production system, and it is the part of a repository most likely to carry someone's personal
//     data.
//   - **Members, teams and their permissions.** The actor on a change comes from the payload that
//     records the change (FR-013); the organisation's membership graph is a different subject.
//
// # Read-only, and why that is checkable rather than asserted
//
// Constitution VII permits no write to any production system, and FR-004 asks for something stronger
// than a clean run: write-incapability must be verifiable from **the set of operations the feeder can
// issue**. [Surface] is that set. It is a value in the program, every call looks its operation up in
// it, and an operation it does not carry is refused before any quota is spent.
//
// A REST operation carries its own answer here, which is why this is stronger than classifying a
// vendor's permission names: each operation is spelled `"METHOD path"`, and `GET` and `HEAD` are
// reads by the definition of the verbs. Unlike feature 003's GCP connector there is **no declared
// state change at all** — nothing is acknowledged, nothing is dispatched, and the surface's
// state-change count is zero (contracts/read-only-operations.md §2).
//
// The inbound webhook is a doorbell in the other direction: something calls us. It triggers a poll
// and its body is never read (FR-053), so it issues no GitHub operation of its own.
//
// # Scope comes from the grant, not from a file
//
// FR-008: the repositories this feeder may read are **what the credential was granted** — the App
// installation's repository selection — and the feeder enumerates that selection from GitHub at
// startup rather than trusting a list someone wrote down. A repository outside the selection is not
// skipped by policy; it is unreachable.
package github
