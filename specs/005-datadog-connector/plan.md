# Implementation Plan: Datadog Connector

**Branch**: `005-datadog-connector` | **Date**: 2026-09-27 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/005-datadog-connector/spec.md`, as clarified on
2026-09-27 (FR-040b–FR-040g).

## Summary

One connector, two halves, one read-only credential: a **log-search telemetry backend** that answers
the investigation engine's eight telemetry terms from Datadog as bounded digests, and a **feeder**
that turns monitors into ALERT nodes with idempotent `alert.transition` events and attaches Datadog
log and monitor pointers to the services whose logs Datadog holds. APM topology and Datadog's event
stream are specified capabilities, **off by default**, and are planned last.

**The project is generic, and this feature is where that is tested hardest.** The audit behind the
2026-09-27 clarification found that the organisation's only Datadog log source runs on a platform no
feature feeds (vendor-hosted voice agents), and that its logs carry no deploy identifier at all. So
the plan does not assume how a service is deployed:

- **The service stamps its own logs.** The connector discovers which attribute carries the deployed
  version from a published convention list (FR-040c), and every version value is normalised into
  the platform-neutral `deploy.*` vocabulary (FR-040d). An `errors_by_version` group then names the
  same identifier a deploy feeder's change claims — Cloud Run, GitHub Actions, Vercel, Kubernetes, or
  a feeder not yet written — with no Datadog-specific join.
- **Every watched log source is a service node**, merged by a certain rule with another feeder's
  node for the same service where there is one (FR-040f), so a service on an unfed platform is
  still investigable.
- **A version change seen in logs is a rollout**, dated from first sight as a bound and merged by C8
  with any deploy feeder's record of the same rollout (FR-040g). For a service nothing else deploys,
  it is the only deploy signal there is.
- **Where there is no stamp, the answer says so**: `errors_by_version` answers `NO_DATA` naming every
  convention searched, and a published guide says how to stamp each deployment type (FR-040e).

**The approach is contract-first, as in 003 and 004.** Most of what 005 needs is already shipped; a
Phase 0 check of the spec against the code (below) found **six gaps that would block or silently
break the connector**, two of them outside any Datadog package. They are planned first.

## Technical Context

**Language/Version**: Go 1.27 (floor; the repository tracks the latest stable toolchain)

**Primary Dependencies**: `pkg/feeder` (events, refs, claims, pointers, `deployref.go`, quota,
read-only surface, `record`, `source`, `testkit`); `pkg/backend` (the telemetry-backend SDK, world
recordings, `onset`); `internal/resolution` (the published rules, C8); `internal/sanitise`;
`internal/campaign`; the Datadog HTTP API (Logs Search and Aggregate v2, Monitors v1; APM and
Events only under their capabilities). **No Datadog client library**: the surface is a handful of
endpoints, and a published read-only operation list is easier to prove over hand-written requests
than over a generated client (constitution X; research §4).

**Storage**: PostgreSQL 16, unchanged. The connector emits graph events through the published event
log and stores nothing of its own; digests are never stored (FR-051).

**Testing**: `go test ./...`; fixtures under `fixtures/datadog-*` verified by `aisre fixture verify`
(replay from empty, double delivery, seeded shuffle); recorded backend worlds replayed with the
miss rate reported (FR-050b); `scripts/check-report.sh`; `golangci-lint`; `buf lint` and
`buf breaking` for the schema items.

**Target Platform**: Linux server, the same single binary. `aisre feed datadog` and a `datadog`
backend registration, in the established shapes.

**Project Type**: A connector (feeder + backend) inside the existing single-module Go project, plus
additive contract changes to features 001 and 002.

**Performance Goals**: SC-003 — a transition is queryable within the poll interval + 1 minute at
p95 by polling alone. SC-016 — monitor id to alert, watched entities, preceding changes, owner and
pointers in one command under 30 s on the recorded corpus. SC-008 — a cycle stays inside its share
of the remaining quota per endpoint class, leaving the human reserve unspent, matching the
recording to the call.

**Constraints**: Read-only, **verifiable from the operation list** (FR-004, SC-015) — including the
POST query endpoints, which is Gap 3 below. No telemetry payload in the graph (SC-007). Sanitise in
the connector before anything touches disk (FR-076a). Live and recorded digests identical,
coverage and join keys included (SC-019). Nothing a connector emits may depend on what another
source delivered first (constitution III; the reason FR-040f was corrected).

**Scale/Scope**: One Datadog organisation per source (FR-006), on any Datadog site; the audited
organisation is on the EU site. Eight user stories, of which US1 and US3 are conditional
capabilities off by default. Today's footprint is one log source (≈37 M lines/week) and a handful
of monitors; the design does not depend on that.

## Constitution Check

*GATE: must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Verdict | How this feature satisfies it |
|---|---|---|
| **I. Graph First** | ✅ | The feeder half emits events only; the backend half returns digests to the engine and writes nothing (FR-001, FR-051). The investigation reaches Datadog only through pointers on graph nodes. |
| **II. Bitemporal or Nothing** | ✅ | Transition valid time is Datadog's stated instant (FR-020). A log-observed rollout is dated from first sight **and marked as a bound**, never presented as the deploy instant (FR-040g). Unknown starts are marked, never guessed (FR-017). |
| **III. Event-Sourced Ingestion** | ✅ | Transition key is the published 4-tuple (FR-025). **FR-040f was corrected during planning** because its first wording made the connector's output depend on what other feeders had already delivered; a node is now asserted for every watched log source and resolution merges. |
| **IV. Telemetry Stays in Its Backend** | ✅ | Pointers only. Digests are bounded, carry coverage, and are never written back (FR-047–FR-051). The Datadog vocabularies publish why their selectors are not OTel semantic conventions (FR-035), while entity attributes stay in OTel terms (FR-036). |
| **V. Evidence-First** | ✅ | Every digest carries the query as sent, Datadog's request id, coverage and join keys (FR-048–FR-048c). `NO_DATA` names what was searched — for `errors_by_version`, every version convention tried and why each failed (FR-040b). |
| **VI. Entity Resolution Is Auditable** | ✅ | The connector merges nothing. A new certain rule, **C9**, merges a Datadog log service with an OpenTelemetry service only on equal name **and** equal stated environment (FR-059); a Datadog rollout merges with another source's through the existing C8. Both are explainable by `resolve why`. |
| **VII. Read-Only by Default** | ✅ *with a decision record* | Every operation is on a published read-only list, and a write-capable credential is refused at startup (FR-003, FR-004). **Datadog's query endpoints are POST**, which the shipped read-only surface refuses outright (Gap 3). The plan extends the surface with named, individually justified query operations rather than loosening the method check — an ADR, not a local exception. |
| **VIII. Evaluation From Day One** | ⚠️ *planned, gated on the owner* | Synthetic structural twins ship with the code and are CI-gated (FR-069a). Real recorded payloads need a credential, a private corpus and named signatories (FR-076), exactly as 003 and 004. Recorded in Complexity Tracking. |
| **IX. Open Schema** | ✅ | The deploy ref on a version group, the Datadog vocabularies, C9 and the read-only query operations are published changes in 001/002 with a decision record (ADR-0010), not local extensions. The `datadog.*` namespaces stay local, as `gcp.*` do, because only this connector reads them. |
| **X. Simplicity Over Cleverness** | ✅ | No Datadog SDK dependency. The GCP backend is the reference implementation and is followed file for file. The two off-by-default capabilities are planned last and may be deferred without affecting any P1/P2 story (FR-008b). |

**Gate result: PASS**, with one decision record owed before implementation (ADR-0010, Gaps 1–4
below) and one disclosed shortfall under VIII.

## Phase 0 findings that shape the plan: what the code already delivers, and six gaps

The full ledger is [research.md §1](./research.md). In short, **delivered and used as-is**: the
backend SDK with its eight terms, six typed outcomes, cost classes and coverage block; recorded
mode, world recordings and the miss rate; `alert.transition` with the 4-tuple key, the `sampled`
marker and flap/no-data suppression; the ALERT node and the WATCHES edge (the spec's "watches"
dependency is met); actor kind; unattached-and-auto-attach for alerts and changes; the `deploy.*`
normalisers; C8. The GCP backend and GCP monitoring feeder are the reference implementations.

**The gaps**, in the order they are planned:

| # | Gap | Evidence | Consequence | Resolution |
|---|---|---|---|---|
| 1 | **A version group cannot name its deploy identifier.** `VersionBreakdown` has version, counts, rate, join keys and a drill-down, and no `deploy.*` ref | `investigation.proto:242` | FR-040d is unmeetable | Add `deploy_ref` and `deploy_ref_absent_reason` to `VersionBreakdown` (investigation.v1 MINOR). Generic: the GCP backend fills it too where a revision's image digest is known |
| 2 | **`errors_by_version` with no version attribute never reaches a backend.** The engine refuses the term as outside the algebra | `internal/investigation/backend/algebra.go:380` | FR-040b's "`NO_DATA` naming every convention searched" cannot happen | The **engine** answers it: a pointer with no version join key yields `NO_DATA` whose coverage names the pointer's recorded discovery outcome. Deterministic from the pointer, so no backend call, no world entry, no miss-rate change, and identical for every backend |
| 3 | **The read-only surface allows GET and HEAD only**; Datadog's log search and aggregate endpoints are POST | `pkg/feeder/readonly.go:83` | The connector cannot declare its own core queries | Extend the surface with **named query operations**: a POST is declarable only as an individual operation carrying a published justification that it does not change state, listed in `docs/connectors/datadog.md` and tested. The method rule stays for everything else (ADR-0010) |
| 4 | **No certain rule merges a Datadog log service with anything but C1, and C1 ignores environment** | `internal/resolution/certain.go:99-136` | Claiming `otel.service.name` directly would merge a staging and a production service | Datadog claims its log service in `datadog.log_service` with the environment attribute; **C9** merges it with an `otel.service.name` claim only on equal name and equal stated environment (FR-059, generalised to log sources). The C1 hazard itself predates this feature and is recorded, not fixed here |
| 5 | **The quota reader parses GitHub's headers.** Datadog's `X-RateLimit-Reset` is seconds-until-reset and its family is `X-RateLimit-Name` | `pkg/feeder/quota.go:88-113` | Reset read as 1970, every endpoint class named "unnamed"; FR-081a–b silently wrong | A Datadog header reader beside the GitHub one, fixture-tested against recorded headers |
| 6 | **The doorbell and the campaign record are GCP-shaped** | `internal/feeders/gcp/doorbell.go`; `internal/campaign/record.go:76-91` | FR-025b needs an HTTP endpoint with a shared secret; a Datadog campaign has no scope fields | Lift the doorbell (payload-free `Ring`, token bucket) into a shared package with an HTTP transport; add Datadog scope fields (organisation, site, environments, indexes) to the campaign record |

Three smaller facts change individual requirements rather than the plan's shape, and are carried
into the contracts:

- **C8 excludes `deploy.release`** and requires both sides to state the same
  `deployment.environment.name` and to share a target after merges. A version stamped as a release
  name therefore produces a log-observed rollout that never merges; the stamping guide says so and
  recommends the full commit sha (FR-040e).
- **The image normaliser refuses a bare digest** (`sha256:…` with no name), which is what
  `container.image.digest` usually holds. The image convention is therefore the pair
  `container.image.name` + `container.image.digest`; a digest alone becomes a group with a stated
  reason and no ref.
- **`deploy.*` are correlation keys, not identity claims** (`pkg/feeder/claim.go:50-100`), and must
  carry `deployment.environment.name`, as the GCP feeder's do.

### The lesson from 003 and 004 that this plan encodes

A certain rule nobody's fixture exercises is a rule nobody knows works: 003's C4 and C7 never fired
until a cross-source fixture existed, and then C4 turned out to merge the wrong entity. So C9 and
the log-observed rollout's merge through C8 are proved by **cross-source fixtures written in the
same phase as the rules**, including the pair that must stay apart (same name, different
environment).

## Project Structure

### Documentation (this feature)

```text
specs/005-datadog-connector/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   ├── datadog-feeder.md
│   ├── datadog-telemetry-backend.md
│   ├── version-stamping.md
│   ├── pointer-vocabularies.md
│   └── read-only-operations.md
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source code (repository root)

```text
api/sreagent/investigation/v1/investigation.proto  # VersionBreakdown.deploy_ref (Gap 1)

pkg/feeder/
├── readonly.go                     # named query operations (Gap 3)
├── quota.go                        # the Datadog header reader (Gap 5)
├── pointer.go                      # datadog-logs/v1, datadog-monitor/v1
├── doorbell/                       # lifted from internal/feeders/gcp, plus HTTP (Gap 6)
└── versionstamp/                   # convention list, discovery verdict, deploy.* normalisation

internal/investigation/backend/
└── algebra.go                      # engine-side NO_DATA for an unstamped pointer (Gap 2)

internal/resolution/
└── datadog.go                      # C9: log service ↔ OTel service, name + environment (Gap 4)

internal/feeders/datadog/           # the feeder half: monitors, transitions, log sources, rollouts
internal/backends/datadog/          # the backend half, file-for-file after internal/backends/gcp
internal/sanitise/                  # the Datadog field table (NewPolicy)
internal/campaign/                  # Datadog scope fields (Gap 6)

internal/cli/
└── feed_datadog.go                 # `aisre feed datadog`

fixtures/datadog-*/                 # per story, plus the cross-source merges

docs/
├── connectors/datadog.md           # operation list, cost, capabilities
├── connectors/version-stamping.md  # FR-040e: stamping per deployment type
├── schema/pointers.md              # the two vocabularies
└── decisions/ADR-0010-*.md         # Gaps 1–4
```

**Structure Decision**: the established connector layout — a feeder under `internal/feeders/`, a
backend under `internal/backends/`, one `feed_<connector>.go`, fixtures under `fixtures/` — with two
new shared packages. `pkg/feeder/versionstamp` holds the convention list and the normalisation,
because FR-040c–FR-040e are deliberately **not Datadog's**: any log backend, including a later one,
answers `errors_by_version` the same way, and a rule only one package can see is a rule the next
backend re-derives differently. `pkg/feeder/doorbell` is lifted rather than copied so GCP and Datadog
share one implementation of "a notification is never data".

## Implementation phases

The ordering follows the gaps, then the priorities. Each phase names the fixtures that close it.

1. **Contracts (Gaps 1–5), with ADR-0010.** Schema field, engine-side `NO_DATA`, named query
   operations, C9 with its apart-pair, the Datadog quota reader. Nothing Datadog-specific depends on
   anything else until this lands. *Fixtures*: unit tests plus `datadog-log-service-merge-01`
   (C9 merges, and the staging/production pair stays apart).
2. **Version stamping, generic (`pkg/feeder/versionstamp`).** Convention list, share thresholds,
   the discovery verdict, normalisation into `deploy.*`. Backend-agnostic by construction; the GCP
   backend fills `deploy_ref` in the same phase so the property is proved on two backends.
3. **Backend, P1 (US5).** All eight terms over logs and monitors, typed outcomes, coverage, recorded
   mode. `errors_by_version` groups by the discovered attribute via the aggregate API. *Fixtures*:
   `datadog-backend-logs-01` (world recording, miss rate), `datadog-errors-by-version-01` (stamped,
   groups with `deploy_ref`), `datadog-unstamped-01` (the audit's shape: SDK version on start-up
   lines only, `NO_DATA` naming each convention and its share).
4. **Feeder, P1 (US2, US4).** Monitors to ALERT nodes, transitions by polling with the doorbell,
   group handling, pointers, watched log sources as SERVICE nodes (FR-040f). *Fixtures*:
   `datadog-monitor-transitions-01`, `datadog-grouped-monitor-01`, `datadog-doorbell-forged-01`.
5. **Log-observed rollouts (FR-040g).** *Fixtures*: `datadog-log-rollout-01` (an unfed platform: the
   rollout stands alone, marked as a bound) and `datadog-log-rollout-merge-01` (the same commit
   deployed by Cloud Run: **one** rollout after C9 then C8, carrying the stated instant).
6. **P2 (US6, US7, US8).** Tags and owners; sanitisation table, campaign scope and twins; budget and
   usage reporting with the reserve.
7. **Conditional P3 (US1, US3).** `apm_topology` and `changes`. Deferrable without affecting any
   earlier phase, because a disabled capability is silent and stated (FR-008b, SC-024).
8. **The stamping guide and the quickstart run.** `docs/connectors/version-stamping.md`, then the
   quickstart executed end to end and recorded, as 003 and 004 did.

## Complexity Tracking

| Violation | Why needed | Simpler alternative rejected because |
|---|---|---|
| **Constitution VIII: real recorded payloads deferred** | The campaign needs a read-only Datadog credential from the administrator, a private corpus and named signatories (FR-076). None can be supplied by this work. | Calling the connector stable on synthetic twins is what VIII forbids. The twins are CI-gated now, and the campaign reuses `fixture campaign`, so it is a data task when the credential arrives. |
| **POST query operations on a read-only surface** (Gap 3) | Datadog's log search and aggregation are POST. Without them the backend cannot answer any log term. | Allowing POST by method would make the read-only guarantee a matter of trust. Refusing Datadog would leave the telemetry contract proved on one backend. Named operations, each justified and listed, keep FR-004's "verifiable from the operation list" true. |
| **Two new shared packages** (`versionstamp`, `doorbell`) | FR-040c–e must behave identically on every log backend; the doorbell must be the same code on GCP and Datadog. | Putting them in the Datadog packages would make the next backend re-derive the convention list, which is how two sources end up spelling one join differently — 003's C4. |
| **A new certain rule, C9** (Gap 4) | A Datadog log service must merge with the same service from another feeder, and only in the same environment. | Claiming `otel.service.name` directly lets C1 merge across environments; relying on probable rules leaves every Datadog-only service a suggestion, so FR-040g's rollouts would never reach C8. |
