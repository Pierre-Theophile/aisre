# Implementation Plan: GCP Integration and Vendor-Notice Feeder

**Branch**: `003-gcp-integration` | **Date**: 2026-09-20 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/003-gcp-integration/spec.md`

## Summary

Ship the first **live** connector: two feeders and one telemetry backend over one GCP
integration, all read-only. The **GCP feeder** turns Cloud Run services and revisions, their
traffic splits, Cloud Run configuration and secret *version references*, Cloud SQL instances and
their flag and maintenance events, the Cloud Audit Logs admin-activity stream, Cloud Monitoring
alert policies and their transitions, and — where they are cheap — load balancers and Cloud DNS,
into typed graph events. The **vendor-notice feeder** turns provider maintenance, deprecation and
incident announcements from a shared mailbox, status pages and changelog feeds into CHANGE nodes
with `VENDOR` as the actor kind and **the announced window as their valid interval** — the
project's first facts whose valid time begins after their observed time. The **GCP telemetry
backend** executes feature 002's published query algebra against Cloud Monitoring and Cloud
Logging and returns bounded digests, making feature 001's pointers executable for the first time.

The feature exists because the coverage audit
([`docs/evaluation/coverage-audit-2026-09.md`](../../docs/evaluation/coverage-audit-2026-09.md))
found the true cause was a node or change in the graph at alert time for **0 of 13** real
incidents under feature 001's feeders alone, and for **8 of 13** with these two feeders present.
Re-running that audit by its published method and reaching 8 is SC-023, and it is the gate this
plan is written backwards from.

Technical approach, from [research.md](./research.md): **Go**, in the existing `sre-agent`
binary, as `internal/feeders/gcp/`, `internal/feeders/vendornotice/` and
`internal/backends/gcp/` behind the existing `pkg/feeder` and `pkg/backend` SDKs — no new public
Go surface, because both SDKs already publish exactly the contract these three parts implement.
The GCP client libraries are linked in (Complexity Tracking C1); every vendor call goes through
one narrow transport seam per GCP area so the recorded mode is the same code path. Two feeders
means **two source identifiers, two credentials, two cadences, two budgets and two checkpoints**
(FR-002), sharing nothing but the repository. The read-only credential is proved at startup by
asking GCP, in the shape `internal/feeders/k8s/permissions.go` established (FR-004). Sanitisation
runs **in the connector, before anything touches disk** (FR-137), and the real recordings live in
the private repository `sre-agent-private` while the public repository carries synthetic
structural twins (FR-128).

The feature 001 change package ([ADR-0006](../../docs/decisions/ADR-0006-feature-001-change-package.md),
landed 2026-09-18) already supplies most of what this spec's "Dependencies on feature 001" asked
for. What it does **not** yet supply is five small additive items, enumerated and scoped in
[§What feature 001 still owes this feature](#what-feature-001-still-owes-this-feature); they are
this feature's first work package, exactly as the 001 change package was feature 002's.

## Technical Context

**Language/Version**: Go, same floor as 001 and 002 (`go 1.27`, latest stable toolchain),
`CGO_ENABLED=0`, one static binary.

**Primary Dependencies**: everything 001 and 002 already have (`pgx/v5`,
`connectrpc.com/connect`, `buf`, `cobra`, OTel SDK, `go-oidc/v3`, `anthropic-sdk-go`) plus the
Google Cloud client libraries for the areas read, and one mailbox client. The set, the reason
each is a client library rather than hand-rolled REST, and the rejected alternative are
Complexity Tracking C1; the exact module list and versions are research §1. Nothing else is
added: the change-point method is `pkg/backend/onset` (already shipped), the log template miner
is feature 002's Drain-style miner, and the digest builders, coverage helpers and horizon rule
are `internal/investigation/backend`.

**Storage**: none of its own. The feeders emit events into feature 001's event log through
`pkg/feeder.Emitter`; the graph is its projection, as ever. The telemetry backend **stores
nothing at all** (FR-001, FR-109) — a digest is handed to the caller and is gone. Recorded
payloads and recorded backend responses are files in the fixture layout 001 and 002 published
(`payloads/`, `world/`), addressed by request key + digest + size. Checkpoints are 001's
`SourceCheckpoint` events. The only new persistent artifact in this feature is the **campaign
record** (FR-129–FR-131, FR-140), which is a signed manifest in the private corpus, not a table.

**Testing**: `go test` unit and table-driven tests per GCP area; `aisre fixture verify` over
the recorded fixtures — replay from empty to golden, double-delivery as a no-op, shuffle inside
the declared reordering window (FR-142, SC-017); `pkg/backend/testkit` replaying the recorded
world through the live backend implementation and diffing digests field for field including the
whole coverage block (FR-106, SC-014); the world-recording **miss rate** reported on every
verification run against its published threshold (FR-108); property tests for the
announced-fact invariants (observed time never in the future; a cancellation closes an
observation and never a valid interval) and for idempotency of `alert.transition` across both
transports; a **live parity check** in the shape of 001's (FR-143, SC-016); and the
independent secrets-and-entropy and personal-data scans plus canary assertion at commit
(FR-138, FR-139, SC-018, SC-019), which are `scripts/check-no-secrets.sh` extended rather than a
second scanner.

**Target Platform**: Linux and macOS (amd64, arm64), static binary and OCI image, as 001 and 002.

**Project Type**: single Go module, one binary, extended — `aisre feed gcp`, `feed
vendor-notice`, and the GCP backend registered for `worker backend`/`serve`, plus the campaign
and sanitisation subcommands under `fixture`.

**Performance Goals**: alert transition to observed time at or below the poll interval plus one
minute at p95 with polling alone, or one minute at p95 with a doorbell configured (SC-004); a
full polling cycle over the in-scope estate inside its documented calls-per-hour per GCP area and
inside its configured share of remaining quota for every endpoint class, with the published human
reserve unspent in 100% of cycles (SC-022, FR-146–FR-152); Sam's one-command answer from an alert
policy identifier under thirty seconds on the recorded corpus (SC-025); ≥95% of the Cloud Run
services GCP lists present as nodes within one polling interval, every difference enumerated
(SC-001, FR-144).

**Constraints**: read-only credentials toward GCP and toward the mailbox, proved by asking rather
than asserted (FR-004, FR-007), with **exactly one** declared state change anywhere — the
acknowledgement of messages on the operator's own doorbell subscription (FR-006, SC-020); no
metric sample, log line, span or aggregate of them in the graph or in any event (FR-109, SC-013);
**no announcement body, address, display name, subject or attachment anywhere at any point,
including on a failed or aborted run** (FR-071, FR-072, SC-010); valid time always from the
source's own timestamp and observed time always from the graph, neither correcting the other
(FR-063, FR-153); every digest bounded, coverage-carrying and join-key-carrying (FR-099–FR-102);
announcement text treated as untrusted input from outside the organisation and never as an
instruction (FR-073); nothing sent to a model provider that would not be permitted into a
recording under the same contract version (FR-141).

**Scale/Scope**: the estate as the coverage audit describes it — order 10¹ Cloud Run services
across a small number of projects and one or two regions, order 10² live revisions inside the
configured history horizon, order 10¹ Cloud SQL instances, order 10¹–10² Cloud Monitoring alert
policies, one GKE cluster recorded as an infrastructure resource and nothing below it, and an
allowlist of five vendor families. The alert poll runs at 15–30 s; everything else runs on its
own slower cadence. A recorded world for one incident window is feature 002's shape: the cross
product of the eight telemetry terms over a ≤3-hop neighbourhood × a window grid, order 10³
terms.

**Unknowns carried into Phase 0** (all resolved in [research.md](./research.md)): which Cloud
Monitoring query vocabulary is the one to publish as a versioned pointer vocabulary, given that
MQL's status is in doubt; whether Cloud Monitoring exposes alerting *incidents* to a read-only
API at all, and what the honest fallback is if it does not — this is the single largest design
risk in the feature, because User Story 4 and SC-004 are written as though transitions can be
polled; how to distinguish a revision creation instant from the instant a traffic split took
effect, and from which surface; whether a Cloud Audit Log entry states that a principal is a
platform service agent, which FR-020 needs in order not to infer actor kind from the principal
string; whether Cloud Logging can group counts server-side or whether a digest must bound what
it pulls; whether GCP reports remaining quota in-band anywhere, which decides how much of
FR-148 is vendor-reported and how much is self-tracked; the correct API for proving a credential
holds no write permission, and which permissions cannot be tested that way; the read-only
mailbox access path and whether reading can be guaranteed not to alter message state; and the
machine-readable shapes of status-page and changelog sources.

**Carried as configuration the organisation must supply** — the spec's two remaining
`[NEEDS CLARIFICATION]` markers, both of which are inputs rather than design questions, and
neither of which blocks this plan:

| marker | what is unresolved | what this plan does about it |
|---|---|---|
| **FR-131** ✅ **answered 2026-09-21** — the production project, all regions, for the first campaign; scope itself is operator configuration | which GCP projects and regions are in scope for the first recording campaign | the scope is configuration recorded in every checkpoint (FR-009, FR-130) and in the campaign record. The design is scope-agnostic and the scope is changeable without losing history, which is *why* FR-009 is written that way. research §11 records the default the plan proceeds under — production only, widening to production plus staging — and names the decision as the organisation's. |
| **FR-132** ✅ **partly answered 2026-09-21** — a shared engineering mailbox is the intended source, but its *kind* is unconfirmed and per-user delivery turned out to be the normal case, so FR-132a/FR-132b now make it a first-class path and make the address configuration. Authorising the read is still an owner action. | which shared mailbox, who owns it, and by what read-only path | the mailbox is one implementation of `pkg/feeder.Source` behind the notice feeder's source seam (research §9). The plan specifies the seam and the read-only guarantee each candidate path can offer; which path is used is configuration and an owner's authorisation, recorded in the campaign record. The feeder is buildable, testable and shippable against recorded notices with the question open. |

Both are surfaced again in [§Complexity Tracking](#complexity-tracking) as blocking items for the
**campaign**, not for the code, and FR-130 is explicit that recording must *start* before the
scope is finally agreed.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| # | Principle | Gate | Status |
|---|-----------|------|--------|
| I | Graph first | The two feeders expose data to consumers by exactly one route: typed events into the event log (FR-001). The telemetry backend exposes nothing to the graph at all — it emits no graph event, and a digest is a transient answer to its caller (FR-001, FR-109, SC-013). Nothing in this feature reads or writes any other shared state; it has no store of its own. | PASS |
| II | Bitemporal or nothing | Every node and edge carries both intervals; valid time always comes from GCP's or the vendor's own timestamp and observed time always from the graph at acceptance (FR-063, FR-153). Where the source does not state since when a fact was true, the valid start is marked **unknown** and never backfilled with the poll instant (FR-022, FR-027). This is the first feature whose value depends on the *asymmetry* — an announced fact's valid time leads its observed time, observed time is never in the future (FR-062–FR-064) — and a cancellation is a **correction of the observation**, never a rewrite of the valid interval (FR-067, FR-068). Retractions close valid intervals; nothing is deleted (FR-023, FR-025). | PASS |
| III | Event-sourced ingestion | Every emission is a typed 001 event with a deterministic idempotency key (FR-076 for notices, the published 4-tuple for `alert.transition` at FR-046). Re-delivery is a no-op and is a fixture (FR-142). Both feeders declare a reordering window and tolerate delayed delivery **by observed time**, never by reordering their own log (FR-041, FR-075). A partial poll checkpoints the extent it actually covered, declares the gap, and retracts nothing for the part it did not read (FR-012, FR-026) — which is the rule that keeps a failed poll from looking like a deletion. No batch rebuild, no truncate-and-reload. | PASS |
| IV | Telemetry stays in its backend | The whole point of the feature's third part. The graph receives **pointers** in GCP's own executable vocabularies (FR-079–FR-085) and never a sample; the backend returns **digests** — per-series statistics, masked log templates, counts and comparisons — and the point-by-point samples never leave the vendor (FR-094, FR-090, FR-097). `onset` is computed **backend-side** precisely so the series cannot cross the boundary (FR-087a). Pointer *selectors* are in GCP's vocabularies because that is what will actually be executed, and the *entity attributes* are in OpenTelemetry semantic conventions so a reader can recognise the entity without GCP's grammar (FR-080, FR-081) — the documented "why not OTel" the principle requires. SC-013 makes the rejection path a fixture. | PASS |
| V | Evidence-first | Every digest carries its own evidence — the exact query as sent, the pointer and node, the window, the reference and execution instants, GCP's request identifier and the connector and vocabulary versions (FR-100) — and a mandatory **coverage** block without which it is invalid (FR-101). Every derived dependency edge records the evidence that derived it (FR-028); every unconfirmed one is a **proposed dependency** carrying its suggestive evidence rather than an invented edge (FR-029). The six typed outcomes are never collapsed: `no_data` is the only one that is evidence that nothing happened (FR-104, FR-105). `unknown` is first-class for actor kind (FR-020) and for a vague announced window (FR-069). | PASS |
| VI | Entity resolution is auditable | Both feeders mint **identity claims and merge nothing themselves** (FR-115, FR-021, FR-035, FR-043, FR-070), including for the case they are most tempted by — one announcement arriving by email and by status page (FR-070, SC-009). Four **certain** rules and the **probable** ones are published (FR-117–FR-121); probable rules produce suggestions only, and SC-021 asserts zero automated merges from them. Two projects with the same service name stay separate (FR-010); a human decision persists across replay and outranks any automated match (FR-122); every merge is answerable by the audit query (FR-123, SC-021). | PASS |
| VII | Read-only by default | Read-only service account for the GCP half and read-only mailbox access for the notice half, **proved by asking GCP what the principal may do and refusing to start if it holds any write permission in an area it reads** (FR-004), with the untestable remainder named and asserted in configuration rather than assumed. No operation that changes state in the organisation's projects, verifiable from the set of operations the integration *can* issue, not only from one run's behaviour (FR-005, SC-020). **One** declared exception: acknowledging messages on a dedicated doorbell subscription the operator created, which the integration must run correctly without (FR-006). No remediation of any kind. This feature uses none of the constitution v1.1.0 report-delivery exception. | PASS |
| VIII | Evaluation from day one | Recorded **real** sanitised payloads are the corpus, and synthetic-only data is explicitly insufficient for either feeder or the backend to be called stable (FR-127) — the principle's own words. The campaign's minimum contents are enumerated (FR-129) including the awkward windows: a quota-exhausted response, a poll that failed part-way, a cancellation, a reschedule and a duplicate by two sources. Fixtures extend 001's format; the conformance suite is 001's (FR-142); at least one fixture exercises the refusal path. Recorded backend responses are committed beside the events so an investigation replays with no call to GCP (FR-133). SC-023 re-runs the coverage audit, which is the only measurement that tests the feature's thesis rather than its code. | PASS |
| IX | Open schema | This feature publishes: the GCP and vendor identifier **namespaces** (FR-116), the three GCP **pointer vocabularies** with their registered versioned names and the documented reason their selectors are not OTel-expressible (FR-080), the **sanitisation contract** (FR-134), and the digest/coverage/join-key contract *by implementing* 002's published home rather than restating it (FR-087, FR-114, ADR-0005 D7). The five outstanding 001 additions are additive `sreagent.graph.v1` changes taking a MINOR bump with no event-log transformation, gated by `buf breaking` — and they belong to 001, not to this feature as a local extension, exactly as the spec's "Dependencies on feature 001" says. | PASS |
| X | Simplicity over cleverness | No new service, no new store, no new public Go package: the three parts are implementations of `pkg/feeder.Feeder` and `pkg/backend.TelemetryBackend`, both already published. No graph database. The cost is the **GCP client libraries** — a real dependency increase, argued with its rejected alternatives in Complexity Tracking C1 — and one mailbox client, C2. Nothing is built speculatively: the data-access log stream is not read and no permission for it is requested (FR-042); load balancers and DNS are P3 and the first thing cut (FR-057); no trace pointer is minted where there is no tracing (FR-085). | PASS with 2 justified items |
| §Replay CI | Replay, idempotency, calibration on **every** CI run | (a) and (b) are 001's existing fixture verification, which these fixtures extend (FR-142), plus the digest-parity and miss-rate runs (FR-106, FR-108). (c) calibration: **this feature emits no confidence score.** It emits resolution-rule scores, which are 001's and calibrated there, and typed outcomes, which are not probabilistic. The clause therefore binds vacuously, and the plan says so rather than inventing a metric to report — but it does not bind vacuously for the *proposed dependency*, whose rationale carries a score, so that score joins 001's resolution calibration rather than starting a second table. | PASS |
| §Fixture-first | Every task names the fixture it validates against | The constitution's wording is per **task**, not per feature: *"tasks MUST reference the replay fixtures they validate against."* So `tasks.md` carries a `; fixture: <name\|unit\|n/a>` note on **every** task, as specs/001 (93/93) and specs/002 (120/120) do — enforced by `scripts/check-specs.sh` rule 7. The fixture inventory is additionally fixed in [§Evaluation and CI](#evaluation-and-ci) before `/speckit-tasks` runs, and the awkward cases — partial poll, quota exhaustion, cancellation, reschedule, two-source duplicate, deleted-and-recreated service, forged doorbell — each get a named fixture rather than a unit test only. | PASS |
| §Self-observability | OTel about its own operation | FR-011 and FR-152 are the principle stated as requirements: calls per GCP area, quota refusals, poll duration and lag, events emitted and rejected, notices read/extracted/dropped with reason, digests and truncations, and observed clock skew. The feeders' and the backend's usage are reported **separately** (FR-113) because Dana needs to know what investigations cost as distinct from what ingestion costs. | PASS |

**Gate result: PASS.** Proceed to Phase 0. The post-design re-check is at the end of this
document.

## Project Structure

### Documentation (this feature)

```text
specs/003-gcp-integration/
├── plan.md                        # This file
├── research.md                    # Phase 0: every decision with rationale and alternatives
├── data-model.md                  # Phase 1: entities, refs, valid-time rules, state machines
├── quickstart.md                  # Phase 1: run, record, replay, verify, re-run the audit
├── contracts/
│   ├── gcp-feeder.md              # source id, namespaces, per-area mapping, checkpoints,
│   │                              #   the read-only role set and the startup refusal
│   ├── vendor-notice-feeder.md    # sources, allowlist, typed extraction, announced-fact
│   │                              #   semantics, corrections, what is never stored
│   ├── gcp-telemetry-backend.md   # the eight terms against GCP surfaces, the digest mapping,
│   │                              #   coverage fields, join-key mapping, outcomes, cost classes
│   ├── pointer-vocabularies.md    # the three registered GCP vocabularies + why not OTel
│   ├── sanitisation.md            # FR-134: per field and per label key — verbatim, pseudonym
│   │                              #   or dropped; canaries; manifests; the private/public split
│   └── budget.md                  # endpoint classes, share-of-remaining, reserve, deferral
│                                  #   order, window caps, the usage report
├── checklists/requirements.md     # (existing)
└── tasks.md                       # Phase 2 (/speckit-tasks) — NOT created by /speckit-plan
```

### Source Code (repository root)

Additions only; everything in 001's and 002's tree stays where it is.

```text
api/
└── sreagent/graph/v1/                  # EXISTING — gains the five additive items of
                                        #   §What feature 001 still owes this feature (MINOR)

internal/
├── feeders/
│   ├── k8s/, otel/                     # EXISTING — untouched; the GKE cluster stays theirs
│   ├── gcp/                            # NEW — the GCP feeder
│   │   ├── feeder.go                   #   Describe/Run, cycle planning, deferral order
│   │   ├── permissions.go              #   FR-004: ask GCP, refuse to start, name the offenders
│   │   ├── transport.go                #   one narrow seam per GCP area; live and recorded share it
│   │   ├── cloudrun.go                 #   services, revisions → SERVICE, WORKLOAD
│   │   ├── rollout.go                  #   creation vs traffic-shift rollouts (FR-016–FR-018)
│   │   ├── config.go                   #   env vars, secret VERSION refs → CONFIG (FR-033, FR-034)
│   │   ├── cloudsql.go                 #   instances, flags, maintenance (FR-027–FR-032)
│   │   ├── depends.go                  #   derived edges + proposed dependencies (FR-028, FR-029)
│   │   ├── auditlog.go                 #   admin-activity → the change taxonomy (FR-037–FR-044)
│   │   ├── monitoring.go               #   alert policies → ALERT; transitions → alert.transition
│   │   ├── doorbell.go                 #   Pub/Sub: "poll now" and nothing else (FR-050, FR-006)
│   │   ├── lbdns.go                    #   P3: exposed-via, DNS switch (FR-055–FR-057)
│   │   ├── gke.go                      #   the cluster as INFRA_RESOURCE + claims only (FR-030)
│   │   ├── actorkind.go                #   principal classification (FR-019, FR-020)
│   │   ├── labels.go                   #   label allowlist, environment mapping, OWNER nodes
│   │   ├── pointers.go                 #   minting in the three GCP vocabularies (FR-079–FR-085)
│   │   ├── identity.go                 #   namespaces and claims (FR-115, FR-116)
│   │   ├── state.go                    #   checkpoints, history horizon, the silence rule
│   │   └── testdata/                   #   synthetic structural twins (FR-128)
│   └── vendornotice/                   # NEW — the vendor-notice feeder
│       ├── feeder.go                   #   Describe/Run; a different credential and cadence
│       ├── source_mailbox.go           #   read-only, state-preserving (FR-007)
│       ├── source_statuspage.go        #   polled, cadence-limited, honest user agent (FR-075)
│       ├── source_changelog.go
│       ├── allowlist.go                #   vendors × products; drops counted and visible (FR-059)
│       ├── extract.go                  #   typed extraction; untrusted input barrier (FR-073)
│       ├── announce.go                 #   announced window, state, cancellation, reschedule
│       ├── vendor.go                   #   THIRD_PARTY nodes and host-name claims (FR-077)
│       └── testdata/
├── backends/
│   └── gcp/                            # NEW — the GCP telemetry backend
│       ├── backend.go                  #   Describe/Execute; the eight terms, cost classes
│       ├── compare.go                  #   Cloud Monitoring (FR-088, FR-092, FR-094)
│       ├── onset.go                    #   backend-side via pkg/backend/onset (FR-087a)
│       ├── logpatterns.go              #   Cloud Logging → masked templates (FR-090, FR-095)
│       ├── errors_by_version.go        #   grouped by Cloud Run revision (FR-089)
│       ├── error_spans.go              #   no trace source → typed no_data (FR-091)
│       ├── monitor_state.go            #   (FR-098)
│       ├── exemplars.go                #   explicit request only, capped, redacted (FR-096)
│       ├── drilldown.go                #   handles and deep links (FR-103)
│       ├── coverage.go                 #   the mandatory block, incl. observed ingestion lag
│       ├── joinkeys.go                 #   the published mapping (FR-102)
│       └── testdata/world/
├── gcpx/                               # NEW — what both halves of the GCP integration share
│   ├── auth.go                         #   ADC, the read-only scope set, one credential
│   ├── budget.go                       #   per-endpoint-class share-of-remaining + reserve
│   ├── quota.go                        #   vendor-reported vs self-tracked; backoff (FR-151)
│   ├── usage.go                        #   the report, feeder and backend counted apart (FR-152)
│   └── skew.go                         #   observed clock skew, reported, never corrected
├── sanitise/                           # NEW — the sanitisation contract, executable
│   ├── policy.go                       #   per field and per label key: verbatim/pseudonym/drop
│   ├── hmac.go                         #   keyed, corpus-consistent, so join keys still join
│   ├── drop.go                         #   people identifiers dropped, never hashed (FR-135)
│   └── canary.go                       #   seeded tokens asserted absent (FR-139)
├── projector/                          # EXISTING — gains alert unattachment/attachment and the
│                                       #   sampled-history marker (001 items 3 and 5 below)
├── query/, resolution/, log/           # EXISTING — gain the 001 items below, additively
└── cli/
    ├── feed_gcp.go                     # NEW
    ├── feed_vendor_notice.go           # NEW
    └── fixture_campaign.go             # NEW — record, sanitise, sign, scan, verify a campaign

fixtures/                               # PUBLIC repo: synthetic structural twins only (FR-128)
├── gcp-baseline-topology-01/
├── gcp-rollout-traffic-shift-01/
├── gcp-revision-zero-traffic-01/
├── gcp-rollback-01/
├── gcp-partial-poll-01/
├── gcp-quota-exhausted-01/
├── gcp-alert-transition-01/            #   both transports, one transition
├── gcp-forged-doorbell-01/
├── gcp-grouped-alert-01/
├── gcp-service-recreated-01/
├── gcp-audit-other-kind-01/
├── gcp-cloudsql-flag-change-01/
├── gcp-proposed-dependency-01/
├── vendor-maintenance-future-01/       #   the announced fact (joins 001's announced-fact-01)
├── vendor-cancellation-01/
├── vendor-reschedule-01/
├── vendor-duplicate-two-sources-01/
├── vendor-not-allowlisted-01/
├── vendor-unextractable-01/
└── gcp-telemetry-rejection-01/         #   SC-013: an event carrying a sample is refused

docs/
├── connectors/
│   ├── gcp.md                          # NEW — Dana's page: roles, quotas, cost, what it reads
│   └── vendor-notice.md                # NEW — the mailbox owner's page: what is kept, what is not
├── schema/
│   ├── pointers.md                     # EXISTING — gains the three GCP vocabularies
│   └── announced-facts.md              # NEW — the semantics 001 publishes, 003's first consumer
├── security/
│   └── sanitisation.md                 # NEW — the contract, the scans, the manifest, the split
├── evaluation/
│   └── coverage-audit-2026-1x.md       # NEW — the re-run, with per-incident attribution (SC-023)
└── decisions/
    └── ADR-0009-…                      # the five 001 additions, decided once for 003 and 005

.github/workflows/
├── ci.yml                              # EXISTING + the new fixtures, buf breaking on the additions
└── (private repo) corpus.yml           # the private CI that runs the same suite over real payloads
```

**Structure Decision**: one Go module, one binary, no new public Go package. `pkg/feeder` and
`pkg/backend` already publish exactly the two contracts this feature implements, so the three
parts are `internal/` implementations of published interfaces — which is also what keeps the
protobuf contract the public surface (constitution IX) rather than a Go API. Three new
`internal/` packages exist for reasons, not symmetry: `internal/gcpx` because FR-002 says the two
feeders share **no** credential while FR-003 and FR-113 say the GCP feeder and the GCP backend
share **one**, so the shared half needs a home that the notice feeder cannot reach into;
`internal/backends/` because a vendor backend is a connector half like a feeder and belongs
beside `internal/feeders/`, not inside the engine that calls it; and `internal/sanitise` because
FR-137 puts sanitisation in the connector before disk, FR-141 puts the same boundary in front of
the model provider, and FR-110 puts it in the live digest path too — one contract, three call
sites, so it cannot live in any one of them.

**Terminology**, to keep three similar words apart: the **GCP integration** is the whole feature.
Its **GCP feeder** and its **GCP telemetry backend** are the two halves that share one credential
and one quota budget and nothing else. The **vendor-notice feeder** is a separate source with its
own everything, shipped in the same feature because 8 of 13 is the number for the pair.

## Design decisions (summary; every one is argued in research.md)

1. **In the binary, not out of tree** (research §1, Complexity Tracking C1). `docs/connectors/
   writing-a-backend.md` says the usual place for a backend whose vendor SDK should not be linked
   in is `examples/<name>/` as its own module. This feature goes in-tree anyway, because FR-003,
   FR-113 and FR-152 require the feeder half and the backend half to share one credential, one
   per-endpoint-class budget and one usage report with the two halves counted separately — and a
   module boundary between them turns that shared budget into an IPC problem for no gain. The
   cost is dependency weight and binary size, which is C1's to justify.
2. **One narrow transport seam per GCP area** (research §2). Every vendor call goes through an
   interface whose methods return decoded payloads, so live and recorded mode are the same code
   path above it and FR-008's "nothing may derive from the connection itself" is structural
   rather than reviewed. It is also the only place a GCP client library is imported, which is
   what makes C1's blast radius auditable.
3. **Polling is the source of truth; the doorbell is a bell** (FR-050, research §5). The Pub/Sub
   notification path enqueues "poll now" and its body is never parsed, trusted or stored. This is
   both the ordering answer (two transports, one transition, order-independent, because the key
   is derived from the transition itself) and the security answer (a forged notification costs one
   extra poll inside the rate limit and can create, alter or retract nothing).
4. **The read-only proof is a question asked of GCP, not a promise** (FR-004, research §8),
   mirroring `internal/feeders/k8s/permissions.go`. Where a permission cannot be tested the
   integration names the part it could not verify and requires the operator's assertion in
   configuration — it does not assume, and it does not quietly widen what it tests to what it can.
5. **The announced fact is an ordinary bitemporal fact, and the ranker's exclusion is a contract**
   (FR-062–FR-065, research §6). 001 already accepts `valid_at > observed_at` and already exposes
   `signed_time_distance_seconds` / `post_reference` with a two-sided decay (ADR-0006 D1/D2), so
   what is left is the *stated* rule that a change whose valid start is after the reference
   instant is excluded **for that reason** rather than by scoring low, plus the announcement-state
   vocabulary. Both are 001 additions, below.
6. **A cancellation is a correction, a reschedule is a correction, and neither is a new node**
   (FR-067, FR-068). The event identifier is deterministic from the vendor and the vendor's notice
   identifier (FR-076), so the second reading addresses the same change; the observed interval
   closes and a new observation opens; the valid interval is never rewritten. "What did we believe
   on 20 September about 2 October?" stays answerable, which is the property the fixtures assert.
7. **The backend implements 002's contract; it does not restate it** (FR-087, ADR-0005 D7). All
   eight telemetry terms, the six typed outcomes, the horizon rule from
   `internal/investigation/backend/horizon.go`, `pkg/backend.NormaliseForDigest` for the response
   digest, and `pkg/backend/onset` for the change-point method — called, not reimplemented, which
   is what makes a GCP digest and a Datadog digest comparable.
8. **`error_spans` answers `no_data` with the absent source named, and that is a feature**
   (FR-091, SC-011). The organisation has no tracing. The term is served, the coverage block says
   the trace data source is absent and that nothing was searched, and the answer is identical live
   and recorded and stable for the whole window — so the engine concludes "this cannot be checked
   here" instead of "there were no error spans", and the term becomes live without a contract
   change if Cloud Trace is ever enabled.
9. **Sanitisation is one executable contract with three call sites** (FR-110, FR-134–FR-137,
   FR-141), and the scan that checks it is implemented separately from the sanitiser (FR-138) so
   one defect cannot both leak and pass. People identifiers are **dropped, not hashed**, in live
   mode as well as in recordings; infrastructure identifiers get a keyed HMAC that is consistent
   across the whole corpus so the digest join keys still join.
10. **The corpus is split, and the public repository never sees a real payload** (FR-128). Real
    sanitised recordings and their goldens live in `sre-agent-private` with its own CI running the
    same conformance suite; the public repository carries synthetic structural twins with the same
    shapes, sequences and contracts. Any claim the public repository makes is a claim about the
    twins, labelled as such.

## What feature 001 still owes this feature

The spec's "Dependencies on feature 001" was written on 2026-09-17. The feature 001 change
package landed on 2026-09-18 ([ADR-0006](../../docs/decisions/ADR-0006-feature-001-change-package.md)),
and it closed most of the list. Re-auditing the list against the code as it stands:

| the spec asked 001 for | status | evidence |
|---|---|---|
| change kinds `deprecation`, `vendor_incident`, and `iam_change` / `quota_change` for the audit stream | **landed** | `ChangeKind.DEPRECATION = 10`, `VENDOR_INCIDENT = 11`, `IAM_CHANGE = 12`, `QUOTA_CHANGE = 13` in `api/sreagent/graph/v1/graph.proto`. FR-060's "until the taxonomy names them, emit as other" fallback is **dead** and this feature must not use it. |
| an actor kind with a member for automation acting on a person's behalf | **landed** | `ActorKind { PERSON, AUTOMATION, CONTROLLER, VENDOR, ACTOR_KIND_UNKNOWN }`, on `Change`, on `RankedChange`, in the change-observation body. `ACTOR_KIND_UNSPECIFIED` is distinct from `ACTOR_KIND_UNKNOWN` — "the source said nothing" vs "an actor was observed and could not be classified" — which FR-020 needs and should use precisely. |
| future-dated valid time: the semantics, not the shape | **landed, less one item** | ADR-0006 D-future publishes the semantic; `fixtures/announced-fact-01` pins it and is derived from *this* spec's FR-062–FR-068; the ranker exposes `signed_time_distance_seconds` and `post_reference` with a published two-sided decay (`internal/query/rank.go`). **Still owed**: the *announcement state* vocabulary (item 4 below). |
| an `alert.transition` event type with the idempotency-key convention | **landed** | event body 25; the derived key `sha256(source, stable alert identifier, group, transition instant)` and the published state vocabulary in `internal/log/alert.go`; projected by `internal/projector/alert_transition.go`. Flapping and no-data suppression is there; recoveries are kept. FR-046 and FR-047 are satisfied by *using* it. |
| a "watches" relation | **landed** | `EdgeType.WATCHES = 8`. |
| a published alert-state vocabulary, and a "sampled" marker on a state history | **half landed** | the vocabulary is `internal/log/alert.go` (`ok`, `warn`, `alert`, `no_data`, `declared`). **Still owed**: the sampled marker (item 3 below). |
| a convention for "unattached", and for automatic attachment | **half landed** | `internal/projector/attach.go` does it for **change** targets, with the edge's provenance naming both halves. **Still owed**: the same for **alerts** and their `WATCHES` edges, which FR-048 needs (item 5 below). |
| a join-key vocabulary | **landed; needs a published mapping, not a schema change** | `JoinKeys { version, workload, pod_or_host, trace_ids, span_ids, first_seen }` and `Pointer.join_keys` with roles `version`, `workload`, `pod`, `host`, `trace`. FR-102's four keys map onto it: revision → `version`, instance identifier → `pod_or_host`, trace identifier → `trace_ids`, first-seen instant → `first_seen`. That mapping is this feature's to **publish** in `contracts/gcp-telemetry-backend.md`; it is not a 001 change. |
| a published home for the digest, coverage and query-algebra contract | **landed** | ADR-0005 D7: `specs/002-investigation-engine/contracts/telemetry-backend.md` and `investigation.proto` §1–3; `pkg/backend`, `docs/schema/algebra.md`, `docs/schema/digests.md`. But see item 6 below: the contract has **no field for the vendor's request identifier**, which FR-100 requires. |
| registry entries for the pointer vocabularies | **not landed** | `pkg/feeder/pointer.go` declares `otel-semconv/1.30` and `k8s-resource/v1` only. The three GCP vocabularies are item 1 below. |
| a proposed-dependency convention | **not landed** | nothing in the schema or the resolution layer can suggest that an **edge** exists, as distinct from suggesting two entities are one. Item 2 below. |

So **seven** additive items remain, and they are this feature's first work package. Five are
`sreagent.graph.v1` MINOR additions with no event-log transformation; items 6 and 7 are MINOR
additions to `sreagent.investigation.v1`. Each is one decision for **003 and 005 together**, which is
why they belong in an ADR rather than in this plan. Items 1–5 were inherited from the spec's
dependency list; **items 6 and 7 were found by reading the shipped proto against FR-100 and FR-104**,
and both concern the digest's honesty rather than its shape:

1. **Three registered pointer vocabularies** — the Cloud Logging filter vocabulary, the Cloud
   Monitoring query vocabulary and the trace filter vocabulary, each with a versioned name in
   `pkg/feeder/pointer.go`, its selector grammar documented in `docs/schema/pointers.md`, and the
   published statement of why its selectors cannot be expressed in OpenTelemetry semantic
   conventions (FR-080, constitution IV). Which Cloud Monitoring language gets the name is
   research §3, and is the one item here with real uncertainty behind it.
2. **A proposed-dependency convention** — a way to suggest that *this service probably depends on
   that instance*, carrying the suggestive evidence, its score and its rationale, confirmable or
   rejectable by a human, persisting across replay, and **never** an edge until confirmed
   (FR-029). Resolution's existing suggestion path is about entity identity and cannot carry it.
   Without this the only options are inventing an edge or losing the evidence.
3. **A `sampled` marker on a state history** — a published way to say "this history is sampled at
   interval *d*" rather than complete, so a consumer can tell a quiet policy from an unwatched one
   (FR-051). Polling is the source of truth for alerts, so every GCP alert history is sampled, and
   a history that does not say so is a history that overstates itself.
4. **An announcement-state vocabulary** — `announced`, `confirmed`, `cancelled`, `superseded`,
   defaulting to `announced` and never promoted to `confirmed` without an observation that the
   window happened (FR-066). ADR-0006 published that a future-dated fact is legal; this is the
   vocabulary that keeps it from being read as an occurrence.
5. **Unattachment and automatic attachment for alerts** — `internal/projector/attach.go`'s
   convention extended to `WATCHES`, so an alert whose targets the graph does not yet know is kept
   and marked unattached and attaches when a target appears (FR-048), with the same two-sided
   provenance the change path records.
6. **A field for the vendor's request identifier on `Digest`** — found by reading
   `api/sreagent/investigation/v1/investigation.proto` against FR-100, which requires every digest
   to carry *"the identifier GCP returns for the request where it returns one"* as part of its
   evidence. There is no such field. `Digest` carries `executed_query`, `deep_link`,
   `backend_version` and `vocabulary`, and `Coverage` carries `executed_at`, but nothing holds the
   vendor's own request id. The two available workarounds are both wrong: `coverage.data_source` is
   the index or metric namespace and means something else, and `free_text` is explicitly *"flagged
   unverified and never citable as evidence on its own"* — and evidence that is not citable is not
   evidence. So this is one additive `string vendor_request_id` on `Digest`, which is a **002
   contract addition** rather than a 001 one, and it is shared with 005 because Datadog returns a
   request id too. It is the smallest of the six and the only one this plan discovered rather than
   inherited. Until it lands, a GCP digest satisfies every clause of FR-100 except the request
   identifier, and the omission is stated rather than smuggled into a text field.
7. **A `FailureReason` for a window older than the vendor's retention** — the second item this plan
   discovered rather than inherited, and the more consequential of the two. Cloud Run metrics retain
   for **6 weeks** (research §3); a window older than that returns nothing, and *nothing* here is not
   `NO_DATA`. The published contract is explicit that `NO_DATA` means *"the query was valid, the
   window was covered, and there is nothing in it"* and that **only** `NO_DATA` is evidence that
   nothing happened. A window outside retention was never covered, so answering `NO_DATA` would tell
   an investigation that nothing happened during a window nobody could look at — the exact defect
   FR-104 forbids for a window that is merely *too recent*. `NOT_YET_INGESTED` is that mirror case,
   and **there is no outcome and no failure reason for "too old"**: the published set is
   `REJECTED_BY_BACKEND`, `NOT_PERMITTED`, `RATE_LIMITED`, `TIMED_OUT`, `UNSUPPORTED_POINTER`,
   `OUTSIDE_ALGEBRA`. So this is one additive `FailureReason.OUTSIDE_RETENTION` carrying the
   retention horizon the vendor states — a `sreagent.investigation.v1` MINOR addition, shared with
   005 because Datadog has retention too.

**A note on where to read the contract.** `specs/002-investigation-engine/contracts/investigation.proto`
has drifted from the shipped `api/sreagent/investigation/v1/investigation.proto`: the shipped copy
has `Coverage.truncated_to_horizon` and `Coverage.horizon` (fields 14–15) and a `FinalHypothesis`
message that the contract copy does not. This feature implements against the **shipped** proto and
`pkg/backend`'s aliases of it, which is what `buf breaking` gates and what the engine actually
reads; the divergence is feature 002's housekeeping, not a blocker here, but a task that reads the
contract copy alone would build a digest missing two mandatory coverage fields.

Until an item lands, the feature's behaviour is stated rather than improvised: item 1 blocks
pointer minting for the areas it names and therefore blocks the backend's execution path, so it is
first; item 2's absence means a non-derivable dependency is **held, not emitted** (never guessed
into an edge); item 3's absence means the alert history is correct but silent about being sampled,
which is a published gap rather than a lie; item 4's absence means an announced change carries the
vendor-stated kind and no state, so a consumer cannot distinguish announced from confirmed, which
is exactly what FR-066 exists to prevent and therefore gates User Story 2's completion; item 5's
absence means an unresolved alert is kept unattached but is not attached automatically later; and
item 6's absence means the evidence chain is complete but for GCP's request identifier, which the
digest reports as undetermined rather than putting somewhere it does not belong; and item 7's
absence means a window older than retention is answered `QUERY_FAILED / REJECTED_BY_BACKEND` with the
retention horizon in `failure_detail`, which is the conservative substitute — it makes the
investigation say *"I could not look"* instead of *"nothing was there"*, and it is the error that
fails safe.

## Complexity Tracking

> Filled because the Constitution Check records two justified items under principle X, and
> because two spec-level clarifications remain that the plan must not bury.

| Violation | Why needed | Simpler alternative rejected because |
|---|---|---|
| **C1. The Google Cloud client libraries linked into the binary** (Cloud Run Admin, Cloud SQL Admin, Logging, Monitoring, Pub/Sub, and DNS/Compute where FR-055 is in scope) — a substantial dependency increase over 001+002, against principle X's "single static binary with minimal external dependencies" | The feature's entire subject is one vendor's APIs. The libraries carry the authentication, retry, pagination and long-running-operation semantics that a hand-rolled client would have to reproduce per area, and a bug in any of those reproductions shows up as a *wrong graph* rather than as a failed call. Linking them in — rather than shipping the connector as its own module under `examples/` — is required by FR-003, FR-113 and FR-152: the feeder half and the backend half share one credential and one per-endpoint-class budget and must report their usage separately in one report, which a process boundary would turn into an IPC problem. | **Hand-rolled REST over `net/http`** was rejected: it is less code only until the first pagination or partial-failure edge case, and the areas read are many. **`examples/gcp-connector/` as its own module** (the shape `docs/connectors/writing-a-backend.md` recommends, and `examples/mcp-feeder` takes) was rejected for the shared-budget reason above, and because the response builders a conforming digest needs (`backend.NewResponse`, the coverage helpers, `ClampRequest`/`AnnotateHorizon`) are all under `internal/` today — that page says so itself. The blast radius is bounded instead: every vendor import is confined to `internal/feeders/gcp/transport.go` and `internal/backends/gcp`, so the dependency is auditable and the seam is where recorded mode already lives. Binary size before and after is published in `docs/connectors/gcp.md`. |
| **C2. A mailbox client** for the vendor-notice feeder | FR-058 names a shared mailbox as one of three notice sources, and the coverage audit found the single most repeated cause in the corpus was an announcement that arrived in one. The dependency is one client for whichever access path the organisation authorises (FR-132). | **Status pages and changelogs only** was rejected: the audit's repeated cause arrived by *email*, weeks in advance, and dropping the mailbox drops the coverage gain the feature exists for. **A generic IMAP library** may in fact be the answer and is cheaper than a vendor SDK; research §9 decides, and the seam is `pkg/feeder.Source`, so the choice is one file either way and is deferrable past the code. |
| **C3. FR-131** ✅ **closed 2026-09-21** — the production project, all regions | An organisational input, not a design question. | Not a violation of the constitution and not a blocker: FR-009 makes scope configuration recorded in every checkpoint, and FR-130 requires recording to **start** before the scope is agreed precisely because topology history cannot be reconstructed afterwards. Carried as a **campaign** blocker with a stated default (research §11), not a code blocker. |
| **C4. FR-132** ✅ **closed as a design question 2026-09-21**, and the answer changed the design: per-user delivery is a first-class path (FR-132a), not a fallback, because Google addresses platform notices to each project's Essential Contacts and to billing and owner principals rather than to a shared address. Authorising the read stays an owner action. | which mailbox, whose, by what read-only path | As C3. | The feeder is buildable and testable against recorded notices with the question open, because the mailbox is one `Source` implementation. What the plan owes is the *guarantee* each candidate path can make about not altering message state (FR-007), which research §9 establishes without knowing which mailbox it is. Carried as a campaign blocker. |

## Evaluation and CI

The gate is SC-023, and it is not a code metric: **re-run the coverage audit by its published
method over the same thirteen incidents and show the true cause was a node or change in the graph
at alert time for at least 8 of them**, with per-incident attribution naming which feeder supplied
the cause, and every remaining incident classified `unobserved` or `not_change_induced` with its
reason (FR-145). The audit is already a command (`internal/cli/audit_coverage.go`) with a
published format (`docs/evaluation/coverage-audit-format.md`), so the task is to re-run it, not to
build it — which is the point of having built it first.

Beneath that, on every CI run:

| gate | what it asserts | requirement |
|---|---|---|
| fixture replay | every fixture replays from empty to its goldens; double delivery is a no-op; shuffling inside the declared reordering window changes no valid-time state | FR-142, SC-017 |
| digest parity | recorded and live digests are identical field for field **including the whole coverage block and every join key** over a settled window | FR-106, FR-143, SC-014 |
| miss rate | the share of in-algebra requests over a recorded world answered `not_recorded`, against its published threshold | FR-107, FR-108 |
| telemetry rejection | no digest, and no part of one, is in the graph; the backend emits zero graph events; at least one fixture exercises the refusal | FR-109, SC-013 |
| read-only | every operation the integration *can* issue is on the published read-only list, from its own recorded request log | FR-005, SC-020 |
| body containment | zero message bodies, addresses, display names, verbatim subjects or attachments in the graph, on disk, in a log or in any artifact — **including on aborted runs** | FR-071, SC-010 |
| canary survival | zero; a surviving canary **fails the commit** rather than warning | FR-139, SC-018 |
| independent scan | secrets-and-entropy and personal-data scans pass, implemented separately from the sanitiser; zero people identifiers in any form, hashed included | FR-138, SC-019 |
| coverage vs GCP | the feeder's service, revision and alert-policy sets against GCP's own lists, with differences **enumerated** rather than summarised | FR-144, SC-001 |
| budget | no cycle exceeds its share of remaining quota for any endpoint class; the human reserve is unspent in 100% of cycles; usage matches the recording to the exact call | FR-146–FR-152, SC-022 |

The private repository runs the same suite over the real sanitised corpus; the public repository
runs it over the synthetic twins (FR-128). A property that can only be demonstrated on real
payloads is labelled as such rather than implied.

---

## Constitution Check — post-design re-evaluation

*Re-checked after Phase 1, against [`research.md`](./research.md), [`data-model.md`](./data-model.md),
[`contracts/`](./contracts/) and [`quickstart.md`](./quickstart.md).*

Phase 0 changed four things the pre-Phase-0 check took for granted. Each is re-examined here rather
than left to the reader to notice.

| # | Principle | What the design now says | Status |
|---|-----------|--------------------------|--------|
| I | Graph first | Unchanged and strengthened: the backend has **no store and no checkpoint**, because it has no position in a log it never writes to (data-model.md §1). The feeders' only route to a consumer is a typed event. | PASS |
| II | Bitemporal or nothing | Strengthened by research §2 and §4. Valid time is now a **defined quantity** in the one case where GCP publishes no timestamp: a traffic shift's instant is the `operation.last = true` audit completion entry, published in `contracts/gcp-feeder.md` §3.2, and a split with no completion entry yet is **held rather than emitted with a guess**. `receiveTimestamp` is confined to lag measurement and may never be valid time. Cloud SQL's `scheduledMaintenance.startTime` turned out to be genuinely future-dated, so the announced-fact machinery serves the platform feeder too — with **one** implementation, not two (data-model.md §5). | PASS |
| III | Event-sourced ingestion | Strengthened, and one requirement got harder in a useful way. Research §3 found that **Google publishes no ingestion-delay or out-of-order bound for Cloud Audit Logs**, so FR-041's "declare your reordering window" cannot cite a vendor number. The window is therefore **measured and recorded**: a trailing overlap re-query every poll, dedupe on `insertId`, and the observed `receiveTimestamp − timestamp` distribution reported so the configured window is justified from data (`contracts/gcp-feeder.md` §5.2). A declared window backed by measurement is a stronger compliance story than one backed by a citation. | PASS |
| IV | Telemetry stays in its backend | Strengthened in one place and **honestly weakened in another**. Strengthened: Cloud Monitoring's server-side `Aggregation` means metric summaries are computed in GCP and the points never leave it, so "no samples in a digest" is structural (research §3). Weakened: **Cloud Logging has no server-side aggregation**, and both server-side alternatives — log-based metrics and Observability Analytics — require a **mutation** of the organisation's project and are refused under principle VII. So `new_log_patterns` mines client-side over a **bounded sample**, which is less accurate than server-side grouping. The principle is not violated: no log line reaches the graph, and templates are masked before they leave the process. What the design owes instead is **disclosure** — `volume_considered`, the truncation criterion, and the sub-window actually covered, in every such digest (`contracts/gcp-telemetry-backend.md` §4). | PASS, with the trade-off recorded |
| V | Evidence-first | One clause is **not fully satisfiable today**, and the plan says so rather than quietly dropping it. FR-100 requires the vendor's request identifier in a digest's evidence, and the published contract has **no field for it** (item 6). The two available workarounds are both wrong — `coverage.data_source` means the index or metric namespace, and `free_text` is *"flagged unverified and never citable as evidence on its own"*. Until the additive field lands, the digest **reports it as undetermined**. Everything else strengthens: research §4 turns `not_yet_ingested` into a **contractual** boundary via `MetricDescriptor.metadata.ingestDelay` rather than a heuristic, and §6's pagination rule stops an unfinished search being reported as `NO_DATA`. | PASS with 1 stated shortfall (item 6) |
| VI | Entity resolution is auditable | Unchanged. Worth restating because the design keeps meeting the temptation: the two-source duplicate notice (SC-009), the vendor-to-third-party match (FR-119) and the Cloud SQL dependency (FR-029) are all cases where merging or asserting locally would be easy. All three go through claims and published rules, and the one that has nowhere to go — the proposed **edge** — is **held rather than invented** (item 2). | PASS |
| VII | Read-only by default | **This is where Phase 0 changed the most, and every change made the design stricter.** Three findings: (a) `roles/cloudsql.viewer` contains `cloudsql.instances.export` and `cloudsql.backupRuns.export`, so Cloud SQL requires a **custom role** — *below* FR-003's viewer-class ceiling, not above it; (b) `roles/compute.networkViewer` contains `trafficdirector.networks.reportMetrics`, so the **broader-sounding `roles/compute.viewer` is the cleaner one**; (c) **no read-only Pub/Sub pull exists at any granularity** — `pubsub.subscriptions.consume` authorizes pull, ack, `modifyAckDeadline` and seek together, and no custom role can separate them. So FR-006's exception is re-worded: **holding the grant is the exception**, not merely the acknowledgement, the binding is resource-level on one operator-created subscription, and the integration runs correctly with the doorbell absent. The gate itself is honest about its reach: `testIamPermissions` costs **no IAM role** to call and reliably catches the failure that happens, and it is **not a proof**, because IAM inherits downward only and Cloud SQL, Monitoring and log-entry reads have no per-resource check. Hence three layers — the IAM test, a **local token-scope check**, and a named operator assertion for what neither reaches (research §8). | PASS — stricter than the pre-design check assumed |
| VIII | Evaluation from day one | Unchanged in substance, sharper in inventory. The fixture list is fixed before `/speckit-tasks` runs and the awkward cases each have a named fixture. One addition from research §5: because incident reads sit behind a capability flag, the corpus must cover **both** states — a fixture with transitions and one where `monitor_state` answers `NO_DATA` with the absent source named — or the degradation path ships untested. | PASS |
| IX | Open schema | Seven additive items, each one decision for 003 and 005 together, each a MINOR bump with no event-log transformation, gated by `buf breaking`. Five inherited; **two found by reading the shipped proto against the requirements** (items 6 and 7). The three pointer vocabularies are registered with their selector grammars and their "why not OTel" paragraphs — and for the Logging query language that paragraph is substantive rather than formulaic: severity comparison, RE2 and boolean structure are not attribute equalities (`contracts/pointer-vocabularies.md` §3.1). | PASS |
| X | Simplicity over cleverness | The dependency bill is now exact (research §1): six modules, of which two are `google.golang.org/api` and the rest first-party GAPICs. Two choices went **against** the obvious library, both recorded so they are not "modernised" back into bugs: Cloud SQL uses `google.golang.org/api/sqladmin/v1` because the preview GAPIC's `SqlInstancesClient.List` returns an iterator over **warnings** rather than instances; and incident reads use the deprecated `google.golang.org/api/monitoring/v3` because the maintained GAPIC has no `ListAlerts` at all. Nothing speculative is built: no data-access stream, no trace pointers minted where there is no tracing, no attachment parser (§10). | PASS with 2 justified items (C1, C2) |
| §Replay CI | replay, idempotency, calibration every run | (a) and (b) unchanged. (c) calibration still binds vacuously on confidence — this feature emits none — except for the proposed dependency's score, which joins 001's resolution calibration rather than starting a second table. | PASS |
| §Fixture-first | every task names its fixture | **Corrected after a `/speckit-analyze` pass.** An earlier version of this row, and of the pre-Phase-0 one above, claimed PASS on the grounds that the fixture *inventory* was fixed — a per-feature property standing in for a per-task MUST, which is the dilution the constitution's own amendment procedure forbids. `tasks.md` now carries the note on all 181 tasks and `scripts/check-specs.sh` rule 7 requires it, so the claim is mechanical rather than argued. Research also added one pair of fixtures (the capability-flag states, above) that the pre-design check had no reason to know about. | PASS |
| §Self-observability | OTel about its own operation | Two additions that research made mandatory rather than nice: the observed **audit-log lag distribution** (because no vendor bound exists to declare instead) and the **discovered Monitoring quota limit** (because none is published). Both are recorded in checkpoints, not only emitted. | PASS |

**Gate result: PASS.** No unjustified deviation. Two justified complexity items (C1, C2), two
organisational inputs carried as campaign blockers (C3, C4), and **one stated shortfall** — FR-100's
vendor request identifier — which is item 6 of the seven additive contract changes and is reported as
undetermined until it lands.

### The three places this design is less clean than the spec imagined, collected

A reviewer should not have to assemble these from ten pages:

1. **`new_log_patterns` samples.** Cloud Logging cannot aggregate server-side and both ways to make it
   do so require writing to the organisation's project. The digest therefore reports a bounded sample
   and says so. *(constitution IV, honestly; research §6)*
2. **The doorbell's grant is not read-only.** Not the acknowledgement — the grant. There is no IAM
   configuration that makes a Pub/Sub pull read-only. *(FR-006 re-worded; research §5, §8.3)*
3. **The startup gate is a tripwire, not a proof.** IAM inherits downward only and three of the
   services read have no per-resource permission test, so a clean project-level answer does not prove
   the absence of a write grant on a child resource. FR-004's operator assertion is the only truthful
   completion of the check. *(research §8.1)*

Each is disclosed in the artifact a reader would consult, not only here: (1) in the backend contract's
coverage obligations, (2) in the feeder contract's read-only statement, (3) in
`docs/connectors/gcp.md`, which is what Dana signs off.

### What `tasks.md` should start with

1. **The seven additive contract items** (§What feature 001 still owes this feature), as one ADR and
   one work package, in dependency order — **item 1, the three pointer vocabularies, is first**,
   because the backend cannot execute a pointer whose vocabulary is not registered.
2. **`internal/gcpx`**: the credential, the three-layer read-only gate, and the token-bucket budget —
   before any area is read, because FR-004 forbids emitting anything before the credential is proved.
3. **`internal/sanitise`**: the contract executable, with its independent scan and canary assertion —
   before the first recording, because FR-137 puts sanitisation before disk and a recording taken
   without it cannot be cleaned afterwards.
4. **Cloud Run topology and the two rollouts** (User Story 1), which is what lifts the coverage number
   off zero, with `contracts/gcp-feeder.md` §3.2's instant as its exactness gate.
5. **The vendor-notice feeder** (User Story 2), which is the largest coverage gain and the first
   announced fact.
6. **The telemetry backend** (User Story 3), then alerts (User Story 4), then P2 and P3 in the spec's
   own priority order — which is also the budget's deferral order, and deliberately so.

And in parallel with all of it, from the moment the credential and mailbox access exist:
**start recording** (FR-130). Topology history and an announcement stream cannot be reconstructed
retrospectively, and the scope in force is recorded in every checkpoint precisely so that starting
before the scope is agreed costs nothing later.
