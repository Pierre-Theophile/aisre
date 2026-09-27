# ADR-0010: The shared contract additions for feature 005

- Status: **Proposed** (2026-09-27)
- Deciders: project owner (approval), Claude (design, documented here)
- Source: `specs/005-datadog-connector/plan.md` §"Phase 0 findings", `research.md` §1.2, spec
  clarification Session 2026-09-27 (FR-040b–FR-040g). Written under `specs/005-datadog-connector/tasks.md` T003.

## Context

Feature 005 is the Datadog connector. The owner's 2026-09-27 decision is that `errors_by_version`
works from a version the service stamps on its own logs, and that it must work **whatever deployed the
service** — Cloud Run, Kubernetes, Vercel, a vendor-hosted runtime, a VM, a platform added later.

Checking the specification against the shipped code found four things that are not Datadog's to
decide, because they change a published contract or a published rule (constitution IX). Two more gaps
— a GitHub-shaped quota reader and a GCP-shaped doorbell and campaign record — are internal and are
fixed in the tasks without a decision record.

## Decision

### Item 1 — a version group names its deploy identifier

`VersionBreakdown` gains `deploy_ref` (a `sreagent.graph.v1.Ref` in `deploy.commit_sha`,
`deploy.image` or `deploy.release`) and `deploy_ref_absent_reason` (`ABBREVIATED_SHA`, `MUTABLE_TAG`,
`BARE_DIGEST`, `NOT_A_STABLE_IDENTIFIER`). **Additive**, investigation.v1 MINOR; absent when unset, so
no golden moves.

It is what makes the join deployment-agnostic: a group names the same identifier a deploy feeder's
change claims, so the engine resolves "the version whose errors rose" to the change that shipped it
without knowing which platform did. It is filled by `pkg/feeder/versionstamp`, the same rule for every
backend; the GCP backend fills it too.

### Item 2 — `errors_by_version` on a pointer with no version stamp is answered by the engine

Today the engine refuses the term as `OUTSIDE_ALGEBRA` when the pointer names no version attribute, so
no backend ever sees it and the specification's "`NO_DATA` naming every convention searched" cannot
happen. **The engine answers it instead**: `NO_DATA`, coverage absent source "no version stamp on this
pointer", listing the pointer's discovery verdict — each candidate with its shares and why it was
rejected.

The answer is a function of the pointer alone. It needs no backend call and no world entry, so every
recorded world's miss rate is unchanged, and it is identical for every backend.

### Item 3 — named query operations on the read-only surface

The shipped surface admits GET and HEAD and panics on anything else. Datadog's log search and
aggregation are `POST`, with the query in the body. **A `POST` becomes declarable only as an individual
named operation** carrying a published justification that it cannot change state, listed in
`docs/connectors/datadog.md` and tested. Every other non-GET/HEAD method still panics at init.

Constitution VII is unchanged: the guarantee is still that the set of requests a connector can issue
is a published list of reads. What changes is that a read is proved per operation, from its
documentation, rather than per HTTP method.

### Item 4 — C9: a Datadog log service is an OpenTelemetry service, in the same environment

A new **certain** rule. A `datadog.log_service` claim and an `otel.service.name` claim (observed, or a
platform's declared name) are one entity when the names are equal after the published normalisation,
**both environments are stated and equal**, and any Kubernetes namespace or cluster both sides state
agrees. It is the specification's FR-059, generalised from APM services to log sources.

Datadog does not claim `otel.service.name` directly because C1 merges on any identifying namespace
without checking the environment, and a staging service would merge into production. That C1 hazard
predates this feature; it is recorded as a known limit for its own change, not fixed here.

## Consequences

- `sreagent.investigation.v1` takes a MINOR bump; `buf breaking` is clean; no event-log migration.
- The algebra's published `errors_by_version` row gains the engine-side answer (002 contract §1).
- The read-only surface has two kinds of entry, and a reviewer reads the named ones individually.
- A Datadog log service merges with the same service from Cloud Run, Kubernetes or OpenTelemetry
  only in the same environment, and `resolve why` names C9.
- A log-observed rollout can then merge with a deploy feeder's through the existing C8, because the
  two services are one entity.

## Alternatives considered

- **Widen the term so backends answer an unstamped pointer.** Every recorded world would need
  extending, and every backend would implement the same refusal differently.
- **Allow `POST` by method.** The read-only guarantee would become a matter of trusting each connector.
- **Let Datadog claim `otel.service.name`.** C1 would merge across environments.
- **Put the version conventions in the Datadog package.** The next log backend would re-derive them,
  which is how two sources end up spelling one join differently (003's C4).
