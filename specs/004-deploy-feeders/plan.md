# Implementation Plan: Deploy Feeders — GitHub and Vercel

**Branch**: `004-deploy-feeders` | **Date**: 2026-09-22 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/004-deploy-feeders/spec.md`

## Summary

Two thin feeders — GitHub and Vercel — over **one published deployment-as-change mapping**: a
completed deployment of one target becomes a ROLLOUT change node, valid at the instant the platform
says production moved, carrying an actor, an actor kind, a link back to the run, and the identity
claims (`deploy.commit_sha`, `deploy.image`, `deploy.release`) that let another source observing the
same rollout be recognised as observing the same thing. An environment-variable change becomes a
CONFIG change naming the key and never the value.

The feature exists because of a measurement, not a hunch. The coverage audit found the true cause of
**0 of 13** production incidents would have been in the graph with feature 001's feeders alone, and
**3 of 13** with deploy feeders (ADR-0004 D2). Two of those three shipped through GitHub Actions and
Vercel; the third was a Cloud Run revision, which feature 003 has already delivered. The private
re-run of that audit on 2026-09-22 sharpens the target further: of the ceiling's 8 of 13, **6 are
realised today and the two that are not are exactly this feature's** — a Vercel deploy and a deploy
on the agent platform, both `change_induced`, both attributed to a feeder nobody has written.

**The central property is "one rollout, not two" (SC-004).** A rollout observed by a deploy feeder
and by a platform feeder must appear once in the ranked change list, not as two adjacent entries.
Neither feeder merges anything itself — resolution is the graph's (constitution VI) — so this
feature's job is to make the merge *possible* by minting identifiers another source states too, and
then to prove the merge happens.

**The approach is therefore contract-first.** The specification's own "Dependencies on feature 001"
section names six schema items that belong to feature 001 rather than to a connector. Three of them
turn out to be unmet in the shipped code (§Phase 0 below), and one of those — a certain rule for a
shared deploy identifier — is the rule SC-004 rests on. Feature 003 built its GCP rules on exactly
this pattern and learned the cost of skipping it: C4, C5 and C7 were published, registered, evaluated
on every claim and **never fired**, because nothing in the corpus paired a GCP claim with another
source's. Two of those three had latent defects that only a cross-source fixture exposed. This plan
front-loads the contract work and the cross-source fixture so the same gap cannot open twice.

## Technical Context

**Language/Version**: Go 1.27 (floor; the repository tracks the latest stable toolchain)

**Primary Dependencies**: `pkg/feeder` (feature 001's published feeder SDK: events, refs, pointers,
props, ids, `emit`, `record`, `source`, `testkit`); `internal/resolution` (the published rule set);
the GitHub REST API v3 and the Vercel REST API. No new storage dependency.

**Storage**: PostgreSQL 16, unchanged. These feeders write no schema of their own — they emit
graph events through the published event log, exactly as `internal/feeders/{k8s,otel,gcp,vendornotice}`
do.

**Testing**: `go test ./...`; replayable fixtures under `fixtures/` verified by
`aisre fixture verify` (replay-from-empty to golden, double-delivery no-op, seeded shuffle);
`scripts/check-report.sh` for the metric gates; `golangci-lint`; `buf lint` and `buf breaking` for
every schema change.

**Target Platform**: Linux server, same single binary (`cmd/aisre`). Two new CLI subcommands in
the established `feed <connector>` shape.

**Project Type**: Two connectors inside the existing single-module Go project, plus additive schema
changes to feature 001's published contracts.

**Performance Goals**: SC-003 — a rollout is queryable within the poll interval + 1 minute at p95
with polling alone, or 1 minute at p95 with a doorbell. SC-013 — a full cycle stays inside its
configured share of the platform's remaining quota and leaves the published reserve unspent, with
the usage report matching the recording **to the exact call**.

**Constraints**: Read-only credentials only, with write-incapability **verifiable from the set of
operations the feeder can issue, not only from one run's behaviour** (FR-004, SC-007). No secret
value in any form — not plaintext, ciphertext, truncation or hash (FR-038, SC-006). No telemetry
payload reaches the graph (SC-008). Sanitisation happens in the connector before anything touches
disk, including on an aborted run (FR-062).

**Scale/Scope**: One GitHub organisation and one Vercel team per source (FR-007). Scope is the
credential's own grant: for GitHub the App installation's repository selection, enumerated from the
platform at startup (FR-008, clarified 2026-09-22). Five user stories, 76 functional requirements,
18 success criteria.

## Constitution Check

*GATE: must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Verdict | How this feature satisfies it |
|---|---|---|
| **I. Graph First** | ✅ | Deployments become change nodes with edges to the targets they touch. A repository is an identity claim on the service it ships, never a node in its own right (FR-030) — the graph stays a model of the running system, not a code index. |
| **II. Bitemporal or Nothing** | ✅ | Valid time is the platform's stated completion instant and never the run's start, the build instant, or the instant the feeder learned of it; where the platform states none, the valid start is marked **unknown** rather than guessed (FR-012). Observed time is the graph's, at acceptance. Neither clock corrects the other (Edge case 14). |
| **III. Event-Sourced Ingestion** | ✅ | Event identifiers are a pure function of the payload, and a rollout's idempotency key is (source, origin run or deployment identifier, attempt, target) (FR-020). Double-delivery is a no-op and the shuffle is order-independent (SC-011). |
| **IV. Telemetry Stays in Its Backend** | ✅ | Neither feeder implements a telemetry backend, executes a pointer or returns a digest (FR-001). Logs are `LOG` pointers on the change, never content (data mapping). SC-008 asserts zero telemetry payloads over the whole corpus, with the rejection path exercised by a fixture (FR-067). |
| **V. Evidence-First** | ✅ | Every change carries an origin reference — the thing an SRE opens — and it carries no credential and no token (FR-014). The rollback flag is set only where the platform states it, never inferred from commit ordering, age or a revert-shaped message (FR-016, SC-009). |
| **VI. Entity Resolution Is Auditable** | ✅ | Neither feeder merges anything itself and neither suppresses its own observation (FR-043). An unmergeable pair becomes a resolution **suggestion** with its score and rationale, never a silent merge and never two unrelated changes (FR-044). |
| **VII. Read-Only by Default** | ✅ | Read-only credentials, startup permission introspection, and **no configuration that enables a write** (Out of scope). Where the platform cannot report a permission the feeder names what it could not verify and requires the operator to assert read-only — it must not assume (FR-003). |
| **VIII. Evaluation From Day One** | ⚠️ *planned, and gated on the owner* | Fixtures ship with the code and the conformance triad is CI-gated (FR-066, SC-011). But constitution VIII's "recorded real-world payloads" bar needs a recording campaign, which needs a credential, a private corpus and **named human signatories** (FR-064). 003 hit exactly this and it remains its only open work. Recorded in Complexity Tracking below. |
| **IX. Open Schema** | ✅ | Everything these feeders need that feature 001 lacks is a schema change in 001 with its own decision record, not a local extension (spec Assumptions). Phase 0 turns that principle into a ledger. |
| **X. Simplicity Over Cleverness** | ✅ | Two feeders over one mapping rather than two mappings. No repository nodes, no PR/review/diff ingestion, no inference of anything the platform does not state. |

**Gate result: PASS**, with one planned and disclosed shortfall under VIII, which is a property of
the organisation's provisioning rather than of the design.

## Phase 0 findings that shape the plan: what feature 001 still owes

The specification was written on 2026-09-17, before feature 003 shipped. Its "Dependencies on
feature 001" section lists six contract items; checking them against the code as merged today gives
a different answer for each, and this ledger is the main planning output:

| # | Item the spec asks 001 for | Status in the code today | Consequence |
|---|---|---|---|
| 1 | **Actor kind on a change** | ✅ **Delivered.** `ActorKind` in `api/sreagent/graph/v1/graph.proto` — `PERSON`, `AUTOMATION`, `CONTROLLER`, `VENDOR`, `UNKNOWN`, with `ACTOR_KIND_UNSPECIFIED` distinct from `UNKNOWN` (ADR-0005 D1) | No work. The spec's fallback ("hold the kind in a declared property") is dead and must not be implemented. |
| 2 | **Claim-namespace registry** for `deploy.commit_sha`, `deploy.image`, `deploy.release`, `github.repo`, `github.change`, `vercel.project`, `vercel.change`, **with value normalisation** | ❌ **Absent.** `pkg/feeder/ref.go` registers Kubernetes- and OpenTelemetry-shaped namespaces only; none of the seven exists | **Phase 2 work in 001.** A registry that does not fix the value form does not deliver "one rollout, not two": 003's C4 already proved that a spelling difference between two sides is a rule that silently never fires. |
| 3 | **A certain rule for a shared deploy identifier** | ❌ **Absent.** Published certain rules are C1–C3 (core), C4/C5/C7 (GCP) and C6 (vendor allowlist). **The next free id is C8** | **The rule SC-004 rests on.** Without it the feature's central property is unmeetable. |
| 4 | **A rollback marker on a change** | ❌ **Absent.** `rollback_candidate` exists in `investigation.proto` but is a different thing — the engine's *suggested* rollback target, not an observation that one happened | **Phase 2 work in 001.** Per the spec this is "a decision for 001, not for a connector": a published boolean naming the restored deployment, or a distinct change kind. |
| 5 | **"Unattached" and automatic attachment** | ⚠️ **Partly delivered.** `RankedChange.unattached` and `target_entity_ids` exist in `graph.proto`; the *attachment convention* — when a late-arriving target adopts an unattached change — still needs confirming | Phase 0 research task. SC-018 requires attachment within one polling interval. |
| 6 | **Pointer vocabulary registry entries** for the GitHub and Vercel resource links | ❌ **Absent.** `pkg/feeder/pointer.go` registers `otel-semconv/1.30`, `k8s-resource/v1` and the three GCP vocabularies only | **Phase 2 work in 001**, with the published statement of why each selector cannot be expressed in OpenTelemetry semantic conventions (constitution IV). |

**So four of the six are real work, and one of them gates the feature's headline property.** This is
the same shape feature 003 opened with ("the seven additive contract items"), and it is why the
implementation phases below put the contract work first and the cross-source fixture early rather
than last.

### The lesson from 003 that this plan encodes

003 published C4, C5 and C7 and **none of them fired anywhere in the corpus**, because every fixture
was single-source. The two success criteria measured over them were satisfied by having nothing to
measure — 100% of no merges, 95% of no pairs. Building one cross-source fixture found that C4 merged
the wrong entity and was order-dependent, and that C7 was structurally unfireable.

SC-004 and SC-017 are the same claims in this feature's vocabulary. So the cross-source fixture —
one rollout seen by a deploy feeder *and* by the Cloud Run feeder — is **not a late verification
task here; it is the acceptance test for the C8 rule**, written in the same phase.

### Phase 2F's checkpoint is met by the unit half alone, and that is recorded rather than assumed

Phase 2F was written as fifteen tasks, and **ten of them name a fixture a later phase creates**:
`github-deployment-01`, `github-partial-poll-01`, `github-doorbell-forged-01`, `vercel-promotion-01`.
For those ten the dependency runs the other way round from the phase order — the story creates the
recording the task is validated against — so they cannot be completed where they were written, and
the phase's stated checkpoint (*"user stories can now begin in parallel"*) does not depend on them.

So the checkpoint is met by the **five unit tasks**: T032 (the read-only startup gate), T035 (the
budget), T036 (the reserve and the typed quota stop), T045 (the skew observation) and T046 (per-area
telemetry), joined later by T143 (the metered door) and T142 (the skew's call sites). Those five are
what "neither feeder can spend a call it has not declared" rests on, and none of them needs a
recording. The ten moved into the story that records their fixture — nine into US1, T034 into US2 —
and **T141 in [tasks.md](./tasks.md) records the move** so the split is visible rather than inferred
from which boxes are ticked.

The alternative was to retarget the ten at something a unit test could satisfy, and it was refused:
a task whose fixture does not exist yet is honest, whereas a gate rewritten to fit the evidence
available is how a gate quietly stops gating. The same reasoning applies to T034, which is
**reported rather than closed** — `feed vercel --dry-run` prints `token regime: NOT ESTABLISHED`
and names the field whose spelling is unknown, instead of printing a regime it had guessed.

## Project Structure

### Documentation (this feature)

```text
specs/004-deploy-feeders/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source code (repository root)

```text
api/sreagent/graph/v1/graph.proto        # the rollback marker (item 4)

pkg/feeder/
├── ref.go                               # the seven claim namespaces + normalisation (item 2)
└── pointer.go                           # the GitHub and Vercel vocabularies (item 6)

internal/resolution/
└── deploy.go                            # C8: one shared deploy identifier, one rollout (item 3)

internal/feeders/
├── github/                              # the GitHub feeder
└── vercel/                              # the Vercel feeder

internal/cli/
├── feed_github.go                       # `aisre feed github`
└── feed_vercel.go                       # `aisre feed vercel`

fixtures/
├── github-*/                            # per user story
├── vercel-*/
└── deploy-cross-source-merge-01/        # the C8 acceptance fixture

docs/
├── connectors/github.md, vercel.md      # the published read-only operation lists
└── schema/                              # registry updates for namespaces and vocabularies
```

**Structure Decision**: the established connector layout, unchanged. Each feeder is a package under
`internal/feeders/` built on `pkg/feeder`, surfaced by one `feed_<connector>.go` in `internal/cli`,
with its fixtures under `fixtures/` — the shape `k8s`, `otel`, `gcp` and `vendornotice` already
share. The only new directory outside that pattern is `internal/resolution/deploy.go`, which sits
with the other published rules rather than inside either connector, because a rule a connector owns
is a rule the other connector cannot be measured against.

## Complexity Tracking

| Violation | Why needed | Simpler alternative rejected because |
|---|---|---|
| **Constitution VIII: real recorded payloads deferred** | The bar needs a recording campaign, which needs a read-only credential from the platform owner, a private corpus, and named human signatories on the sanitisation manifest (FR-064). None can be supplied by this work. | Shipping synthetic twins alone and calling the connector stable is exactly what VIII forbids. The plan instead builds synthetic structural twins that the public suite verifies (FR-060, SC-011), keeps the campaign as declared open work, and reuses 003's `fixture campaign` gates so the campaign is a data task and not a code task when the credential arrives. |
| **Four contract changes in feature 001 before this feature's own code** | Items 2, 3, 4 and 6 above are schema, and constitution IX says schema belongs to 001 with its own decision record. Item 3 gates SC-004. | Implementing them privately inside each connector is the local extension IX forbids, and it would guarantee the failure 003 recorded: two sources spelling one identifier differently is a certain rule that never fires, silently. |
