---

description: "Task list for feature 003 — GCP integration and vendor-notice feeder"
---

# Tasks: GCP Integration and Vendor-Notice Feeder

**Input**: Design documents from `/specs/003-gcp-integration/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[data-model.md](./data-model.md), [contracts/](./contracts/), [quickstart.md](./quickstart.md)

**Tests**: **Required, not optional.** Constitution VIII — *"Every feature MUST ship with replayable
fixtures… A feature PR without fixtures MUST NOT merge"* — plus FR-127 (recorded real payloads are
the corpus; synthetic-only is insufficient for a connector to be marked stable), FR-142 (every
fixture satisfies the published conformance suite), SC-017/SC-018/SC-019 (zero-tolerance gates), and
`CONTRIBUTING.md` §*"A feeder without a fixture is not merged"*. Fixture and test tasks are
therefore first-class here, interleaved with implementation rather than deferred.

**Organization**: grouped by the nine user stories of spec.md, in their priority order.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US9]`, mapping to spec.md's user stories. Setup, Foundational and Polish
  tasks carry no story label.

## Path conventions

Per [plan.md §Project Structure](./plan.md#project-structure): one Go module, one binary, no new
public Go package. New code lives in `internal/feeders/gcp/`, `internal/feeders/vendornotice/`,
`internal/backends/gcp/`, `internal/gcpx/` and `internal/sanitise/`. Public fixtures (synthetic
structural twins) live in `fixtures/`; the real sanitised corpus lives in the **private** repository.
Every Go file opens with `// SPDX-License-Identifier: Apache-2.0`.

## Two things to read before starting

1. **The spec's `CHANGE_KIND_OTHER` fallback is dead.** FR-060 says to emit `deprecation` and
   `vendor_incident` under `CHANGE_KIND_OTHER` *until the taxonomy names them*. It named them on
   2026-09-18 (ADR-0006 D3). Use `ChangeKind.DEPRECATION` and `VENDOR_INCIDENT`. A fixture asserting
   an `OTHER`-kind deprecation asserts a regression.
2. **Read the shipped proto, not the contract copy.**
   `api/sreagent/investigation/v1/investigation.proto` carries `Coverage.truncated_to_horizon` and
   `Coverage.horizon` (fields 14–15) that `specs/002-…/contracts/investigation.proto` does not. Both
   are mandatory on a horizon-clamped answer.

---

## Phase 1: Setup

**Purpose**: package skeletons and configuration. No vendor module is added yet — a module with no
importer is dropped by `go mod tidy`, so the six Google Cloud modules arrive in T024,
with the transport seam (T023) that imports them.

- [X] T001 [P] Create package skeletons with `doc.go` and SPDX headers for `internal/gcpx/`, `internal/sanitise/`, `internal/feeders/gcp/`, `internal/feeders/vendornotice/` and `internal/backends/gcp/`, each `doc.go` stating the package's one responsibility and citing its contract in `specs/003-gcp-integration/contracts/`; fixture: n/a
- [X] T002 [P] Create `config/gcp.yaml` with the published defaults: project and region scope (empty, to be set per FR-131), revision history horizon, silence-rule consecutive-poll count, poll cadences per area, alert poll interval defaulting inside 15–30 s, label allowlist, environment mapping from project and labels, and the deployment-automation principal list; fixture: n/a
- [X] T003 [P] Create `config/vendors.yaml` with the allowlist shape — vendor slug, products, host names, status-page and changelog endpoints — populated with the five provider families of spec.md Assumptions as the published default; fixture: n/a
- [X] T004 [P] Create `config/gcp-budget.yaml` per [contracts/budget.md](./contracts/budget.md): endpoint classes, per-class documented limits, steady-state shares, human reserve floors, window caps per cost class, and the deferral order; fixture: n/a
- [X] T005 [P] Create `docs/connectors/gcp.md` skeleton with the sections FR-154 and SC-020 require: the read-only role set per area, the published read-only operation list, calls per hour per area at the default cadence, the GCP-side products and volumes read, and binary size before and after; fixture: n/a
- [X] T006 [P] Create `docs/connectors/vendor-notice.md` skeleton with the mailbox owner's sections: what is extracted, what is never stored, the per-path read-only guarantee, and the tenant-wide-grant residual risk of research §9; fixture: n/a
- [X] T007 Extend `internal/cli/root.go` to register the `feed gcp`, `feed vendor-notice` and `fixture campaign` command groups as stubs returning "not implemented", so the CLI surface exists before it does anything; fixture: n/a

**Checkpoint**: packages and configuration exist; nothing reads GCP yet.

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ CRITICAL**: no user story work can begin until this phase completes. Three sub-phases, in order:
the published contract additions (2A) gate everything, the credential and budget (2B) gate every
vendor call, and sanitisation (2C) gates every recording and every live digest.

### 2A. The seven additive contract items

Each is a MINOR bump with no event-log transformation, and each is one decision for **003 and 005
together** — hence one ADR, not seven. **Item 1 is first: the backend cannot execute a pointer whose
vocabulary is unregistered.** See [plan.md §What feature 001 still owes this feature](./plan.md#what-feature-001-still-owes-this-feature).

- [X] T008 Write `docs/decisions/ADR-0009-shared-contract-additions-for-003-and-005.md` recording all seven decisions with their rationale, stating that items 6 and 7 were found by reading the shipped proto against FR-100 and FR-104 rather than inherited from the spec's dependency list, and naming the semver impact of each; fixture: n/a
- [X] T009 **Item 1** — register the three pointer vocabularies in `pkg/feeder/pointer.go`: `VocabGCPMonitoringFilter = "gcp-monitoring-filter/v3"`, `VocabGCPLoggingQuery = "gcp-logging-query/v2"`, `VocabGCPTraceFilter = "gcp-trace-filter/v1"`, each with the constructor comment stating why its selectors are not OTel-expressible; fixture: unit
- [X] T010 **Item 1** — document the three vocabularies' selector grammars in `docs/schema/pointers.md` under §Adding one, including the substantive "why not OTel" paragraph for the Logging query language (severity comparison, RE2 and boolean structure are not attribute equalities) per [contracts/pointer-vocabularies.md §3.1](./contracts/pointer-vocabularies.md); fixture: n/a
- [X] T011 [P] **Item 1** — add `pkg/feeder/pointer_test.go` cases asserting each new vocabulary constant round-trips through `NewPointer`, that a pointer carries no credential (FR-082), and that the trace vocabulary is registered but never minted by any feeder (FR-085); fixture: unit
- [X] T012 **Item 2** — add the proposed-dependency event body to `api/sreagent/graph/v1/graph.proto`: a suggestion that an *edge* exists between two refs, carrying edge type, supporting evidence, a score, a rationale and the source, distinct from the entity-identity suggestion resolution already has; fixture: gcp-proposed-dependency-01
- [X] T013 **Item 2** — project the proposed dependency in `internal/projector/`, persisting it as a pending suggestion that is **never an edge** until confirmed, with the migration under `internal/log/migrate/`, and confirm/reject paths that persist across replay and outrank any automated match (FR-029, FR-122); fixture: gcp-proposed-dependency-01
- [X] T014 [P] **Item 2** — extend `internal/cli/resolve_suggestions.go` and `resolve_confirm.go`/`resolve_reject.go` to list and decide proposed dependencies alongside identity suggestions; fixture: unit
- [X] T015 **Item 3** — add the `sampled` marker to the alert state history in `api/sreagent/graph/v1/graph.proto` and `internal/log/alert.go`: a published way to say "this history is sampled at interval *d*" rather than complete (FR-051); fixture: gcp-alert-transition-01
- [X] T016 **Item 4** — add the announcement-state vocabulary (`announced`, `confirmed`, `cancelled`, `superseded`) to `api/sreagent/graph/v1/graph.proto`, defaulting to `announced`, with `internal/log/` validation refusing a promotion to `confirmed` without an observation that the window happened (FR-066); fixture: vendor-maintenance-future-01
- [X] T017 **Item 5** — extend `internal/projector/attach.go` so unattachment and automatic attachment cover **alerts** and their `WATCHES` edges, not only change targets, recording the same two-sided provenance the change path records (FR-048); fixture: gcp-alert-transition-01
- [X] T018 [P] **Item 5** — add `internal/projector/attach_test.go` cases for an alert whose targets are unknown at ingestion and attach when a target later appears, asserting the edge's `produced_by_event_ids` names both halves; fixture: unit
- [X] T019 **Item 6** — add `string vendor_request_id` to `Digest` in `api/sreagent/investigation/v1/investigation.proto`, so FR-100's *"the identifier GCP returns for the request where it returns one"* has a home that is neither `coverage.data_source` nor the uncitable `free_text`; fixture: unit
- [X] T020 **Item 7** — add `OUTSIDE_RETENTION` to `FailureReason` in `api/sreagent/investigation/v1/investigation.proto`, carrying the retention horizon the vendor states, as the mirror of `NOT_YET_INGESTED` for a window that is too **old** (FR-104, research §6); fixture: unit
- [X] T021 Update `specs/002-investigation-engine/contracts/telemetry-backend.md` and its `investigation.proto` copy for items 6 and 7, and reconcile the drift the plan found — the contract copy lacks `Coverage.truncated_to_horizon` and `Coverage.horizon` that the shipped proto has; fixture: n/a
- [X] T022 Run `make gen` and commit the regenerated code; verify `buf lint` and `buf breaking --against '.git#branch=main'` report every addition as additive (MINOR), and re-record only the goldens an added field actually moves, asserting byte identity **positively** for every other fixture as ADR-0006 did — the regenerated code lands in `api/sreagent/graph/v1/` and `api/sreagent/investigation/v1/`; fixture: all existing fixtures

**Checkpoint 2A**: the published contract carries everything this feature needs. Nothing below may add a schema field.

### 2B. The credential, the read-only gate and the budget

- [X] T023 Create `internal/feeders/gcp/transport.go`: one narrow interface per GCP area returning decoded payloads, so live and recorded mode share one code path above it and FR-008's "nothing derives from the connection" is structural. This is the **only** file that imports a Google Cloud client library; fixture: unit
- [X] T024 Add the six vendor modules to `go.mod` per [research.md §1](./research.md): `cloud.google.com/go/run`, `cloud.google.com/go/logging`, `cloud.google.com/go/monitoring`, `cloud.google.com/go/pubsub/v2`, `google.golang.org/api` (for `sqladmin/v1` and `monitoring/v3`), and run `go mod tidy`; record binary size before and after in `docs/connectors/gcp.md` (Complexity Tracking C1); fixture: n/a
- [X] T025 Implement `internal/gcpx/auth.go`: Application Default Credentials, the read-only scope set, and one credential shared by the GCP feeder and the GCP telemetry backend and by nothing else (FR-003); fixture: unit
- [X] T026 Implement `internal/gcpx/permissions.go` **layer 1** of the read-only gate: chunked `cloudresourcemanager.projects.testIamPermissions` over an enumerated write-permission set per area read, plus per-resource `testIamPermissions` where a service offers one (Cloud Run services, Pub/Sub subscriptions, DNS managed zones, Compute backend services and URL maps, Logging bucket views). **The per-call permission limit is undocumented** (research §8) — chunk at a tunable size, tolerate `INVALID_ARGUMENT`, halve on failure; never assert a documented limit; fixture: gcp-lb-dns-01
- [X] T027 Implement the gate's verb classifier in `internal/gcpx/permissions.go` as an explicit **read-verb allowlist** plus an explicit denylist, failing **closed** on anything unrecognised. A substring rule over `create|update|delete|write|set|insert` misfires on `cloudasset.assets.export*`, `compute.urlMaps.validate`, `serviceusage.values.test`, `pubsub.schemas.validate`, `cloudsql.instances.preCheckMajorVersionUpgrade` and `*.listEffectiveTags` (research §8.2); fixture: vendor-not-allowlisted-01
- [X] T028 Include the impersonation permissions in the tested set — `iam.serviceAccounts.getAccessToken`, `signJwt`, `signBlob`, `implicitDelegation` — because a principal that can impersonate is not read-only however clean the rest looks (research §8), in `internal/gcpx/permissions.go`; fixture: unit
- [X] T029 Implement **layer 2** in `internal/gcpx/tokenscope.go`: a local OAuth token-scope check, because `testIamPermissions` answers about the IAM **policy** and not about the **token**, and `cloud-platform.read-only` is materially safer than `cloud-platform` (research §8.1 item 6); fixture: unit
- [X] T030 Implement **layer 3** in `internal/gcpx/assert.go`: the named operator assertion in configuration for what layers 1 and 2 cannot reach — child-resource bindings (IAM inherits downward only), Cloud SQL / Monitoring / log-entry reads having no per-resource test, `cloudsql.flags.list` having no permission at all, deny policies and conditional bindings being undocumented for this method. The refusal **names the layer that objected** and names what it could not verify (FR-004); fixture: gcp-cloudsql-flag-change-01
- [X] T031 [P] Add `internal/gcpx/permissions_test.go`: a principal holding `roles/editor` is refused naming the offending permissions; a read-only principal starts; the classifier does not false-positive on the six named permissions of T027; an untestable area produces the assertion requirement rather than an assumption; fixture: unit
- [X] T032 Implement `internal/gcpx/budget.go`: a client-side **token bucket per (API, project, region)** seeded from the documented defaults — `logging.entries.list` 60/min per project non-hierarchical, Cloud Run Admin 3,000/60 s per project per region, Cloud SQL Admin 500/min per user per region — as the real-time gate. Self-tracked, per [contracts/budget.md §3](./contracts/budget.md); fixture: gcp-quota-exhausted-01
- [X] T033 Implement `internal/gcpx/quota.go`: the slow reconciliation loop over `serviceruntime.googleapis.com/quota/limit` − `quota/rate/net_usage`, labelled **vendor-reported**, which never authorises a call the fast loop refused; plus startup **discovery** of Monitoring's undocumented `timeSeries.list` limit, recorded in the checkpoint, with a conservative configured fallback that says it was a fallback. "Limit unknown" is never "limit unlimited"; fixture: gcp-quota-exhausted-01
- [X] T034 Implement backoff in `internal/gcpx/quota.go`: honour `429`/`RESOURCE_EXHAUSTED` for at least as long as the response asks where it says so, else truncated exponential from 1 s, resuming from the position reached rather than restarting the cycle (FR-151); fixture: gcp-quota-exhausted-01
- [X] T035 [P] Implement `internal/gcpx/usage.go`: the usage report of FR-152 — calls per GCP area, share of configured budget, share of remaining quota per endpoint class **with each figure labelled vendor-reported or self-tracked**, human reserve left untouched, quota refusals, and the feeders' and backend's usage **separately** (FR-113); fixture: gcp-quota-exhausted-01
- [X] T036 [P] Implement `internal/gcpx/skew.go`: observed clock skew between GCP timestamps and graph observed time, reported in operational telemetry and **never** used to correct either (FR-153); fixture: unit
- [X] T037 [P] Implement `internal/gcpx/otel.go`: the OTel instrumentation of FR-011 — calls per area, quota refusals, poll duration and lag, events emitted and rejected, notices read/extracted/dropped with reason, digests and truncations, observed skew; fixture: unit

### 2C. Sanitisation

One contract, three call sites. Must exist before the first recording: FR-137 puts sanitisation
before disk, and a recording taken without it cannot be cleaned afterwards.

- [X] T038 Implement `internal/sanitise/policy.go`: per field and per label key, exactly one of verbatim / keyed pseudonym / dropped, per [contracts/sanitisation.md §2](./contracts/sanitisation.md). **A field with no disposition fails the commit** rather than defaulting either way; fixture: unit
- [X] T039 Implement `internal/sanitise/hmac.go`: `pseudonym(kind, value)` as a keyed HMAC typed by `kind`, **consistent across a whole corpus** so relationships and digest join keys survive, with neither the key nor any mapping table ever committed (FR-135); fixture: the private corpus
- [X] T040 Implement `internal/sanitise/drop.go`: people identifiers **dropped, never hashed**, in live mode as well as recording — a hashed principal address is still personal data (FR-135, SC-019); fixture: unit
- [X] T041 Implement `internal/sanitise/canary.go`: canary tokens seeded into source data before a campaign and asserted absent from every committed artifact, where **a survivor fails the commit** rather than warning (FR-139, SC-018); fixture: the private corpus
- [X] T042 Wire sanitisation into all three call sites and assert it at each: the recording path, the **live digest** path (FR-110), and the **model provider** boundary (FR-141) — `internal/feeders/gcp/transport.go` and `internal/feeders/vendornotice/sanitise.go` for recording, `internal/backends/gcp/coverage.go` for the live digest, and `internal/investigation/model/` for the provider boundary; fixture: unit
- [X] T043 Extend `scripts/check-no-secrets.sh` with the personal-data scan, **implemented separately from the sanitiser** so one defect cannot both leak and pass (FR-138), and extend its self-test (`scripts/check-no-secrets_test.sh`) to plant a people identifier and a canary and prove both are caught; fixture: unit
- [X] T044 [P] Add `internal/sanitise/sanitise_test.go`: corpus-consistent pseudonyms join across payload, audit entry, digest join key and golden; a people identifier never survives in any form including hashed; an unassigned field fails; log content reduces to masked templates before leaving the process; fixture: unit
- [X] T045 Write `docs/security/sanitisation.md`: the contract, the scans, the signed manifest, the private/public split, and §4's deliberate decision **not** to fuzz timestamps (valid-time exactness is asserted to the instant by SC-002 and SC-004; the disclosure is accepted and signed off); fixture: n/a

**Checkpoint**: foundation ready. The credential is proved read-only, every vendor call is budgeted, nothing reaches disk unsanitised, and the published contract carries all seven additions. User stories may now begin.

---

## Phase 3: User Story 1 — The serving topology, from Cloud Run (Priority: P1) 🎯 MVP

**Goal**: Cloud Run services and revisions, their traffic splits, and the two kinds of rollout change
become graph nodes and changes — the story that lifts the coverage number off zero, because in this
organisation the deployed unit *is* a Cloud Run revision.

**Independent Test**: run the feeder against recorded Cloud Run and Cloud Audit Log payloads for one
project, from an empty graph, and compare to the golden. Delivers a queryable picture of what is
serving traffic, with no alerting, no vendor notices and no telemetry execution present.

- [X] T046 [US1] Implement `internal/feeders/gcp/feeder.go`: `Describe()` returning source id `gcp:<org-slug>`, kind `gcp`, `Ordering: none` and the **measured** reordering window, plus `Run()` calling `Validate()` and the T026–T030 read-only gate **before emitting anything**; fixture: gcp-audit-other-kind-01
- [X] T047 [US1] Implement `internal/feeders/gcp/identity.go`: the namespaces of [data-model.md §2](./data-model.md#2-identifier-namespaces) with project and region **in the value**, and the rule that the addressing ref is **also** a claim (FR-115); fixture: gcp-baseline-topology-01
- [X] T048 [US1] Publish the **certain** resolution rules of FR-117 and FR-118 in `internal/resolution/`: an observed service whose own attributes name the Cloud Run revision it runs in is that revision's service (the instrumentation is running inside it), and a service declaring an OpenTelemetry service name that an observed service claims **with both sides stating the same environment** — a missing environment on either side MUST NOT satisfy it; fixture: gcp-baseline-topology-01
- [X] T049 [P] [US1] Implement `internal/feeders/gcp/labels.go`: the label allowlist (a key not on it becomes nothing — not a property, node or claim), the published configurable environment mapping from project and labels, OWNER nodes with `owned-by` edges, and the rule that a label value measuring something never becomes a property (FR-124–FR-126); fixture: vendor-not-allowlisted-01
- [X] T050 [P] [US1] Publish the **probable** rules of FR-121 in `internal/resolution/`: normalised name equality without an agreed environment, an instance name inside a service's environment variable name, and a shared owning label with a similar name — each producing **suggestions only, never an automated merge** — and assert FR-123's audit query explains every merge involving a GCP claim via `resolve why`; fixture: gcp-baseline-topology-01
- [X] T051 [US1] Implement `internal/feeders/gcp/cloudrun.go`: SERVICE nodes from `projects.locations.services.list` and WORKLOAD nodes from `projects.locations.services.revisions.list` (passing `-` as the service id to list across a location in one call), with the traffic split taken from **`trafficStatuses`** (observed) and never `traffic` (desired, may not have converged); fixture: gcp-rollback-01
- [X] T052 [US1] Implement the traffic-split property in `internal/feeders/gcp/cloudrun.go` as a set of revision names with percentages **versioned in valid time**, never a measurement and never derived from observed traffic, so a rollback gives the property three versions (FR-015); fixture: gcp-rollback-01
- [X] T053 [US1] Implement `internal/feeders/gcp/rollout.go` creation changes: a `ROLLOUT` change valid at `Revision.createTime` (output-only and documented, so `unknown` is not accepted here), marked as **not having moved production traffic**, with `changed-by` edges to service and revision (FR-016); fixture: gcp-revision-zero-traffic-01
- [X] T054 [US1] Implement `internal/feeders/gcp/rollout.go` traffic-shift changes valid at **the `operation.last = true` audit completion entry's `timestamp`**, correlated to its request entry by `LogEntry.operation.id` — the published definition of "the instant GCP states" for SC-002. `Service.updateTime` is recorded as metadata only and is **never** a change instant ([contracts/gcp-feeder.md §3.2](./contracts/gcp-feeder.md)); fixture: gcp-rollback-01
- [X] T055 [US1] Implement the held-split rule: a split observed by the poll with **no** corresponding completion entry yet is **held, not emitted with a guessed instant**, and the checkpoint's extent says so, in `internal/feeders/gcp/rollout.go`; fixture: gcp-partial-poll-01
- [X] T056 [US1] Implement the zero-traffic revision rule in `internal/feeders/gcp/rollout.go`: a revision at 0% still exists as a node and still produced its creation change, is **not** presented as serving, and its creation change is **never reinterpreted** when traffic later arrives — the later movement is its own change (FR-018); fixture: gcp-revision-zero-traffic-01
- [X] T057 [US1] Implement `internal/feeders/gcp/actorkind.go`: the derivation ladder of [data-model.md §4.1](./data-model.md#41-actor-kind-is-derived-from-a-classification-never-from-the-string), reading `principalEmail`, `principalSubject`, `serviceAccountKeyName` and **`serviceAccountDelegationInfo[].firstPartyPrincipal`** — the only documented signal that the caller is a Google first-party principal, and therefore the evidence for `CONTROLLER`; fixture: gcp-audit-controller-effect-01
- [X] T058 [US1] Enforce in `actorkind.go` that `callerSuppliedUserAgent` is recorded as evidence and **never** decisive (GCP's own reference: *"not authenticated and should be treated accordingly"*), that `ACTOR_KIND_UNKNOWN` carries the evidence available, and that it is distinct from `ACTOR_KIND_UNSPECIFIED` — a feeder writing `UNKNOWN` where it learned nothing is claiming to have looked; fixture: gcp-audit-controller-effect-01
- [X] T059 [P] [US1] Implement `internal/feeders/gcp/pointers.go`: the pointers of FR-079 in the three registered vocabularies, with `resource.type = "cloud_run_revision"` **mandatory** in every metric selector (an unpinned query picks up `cloud_run_instance` series that carry no revision label), attributes in OTel semantic conventions, versioned in valid time, additive, and carrying no credential; fixture: gcp-baseline-topology-01
- [X] T060 [P] [US1] Implement `internal/feeders/gcp/state.go`: checkpoints carrying the scope in force, the revision history horizon, the audit filters, the environment mapping, the declared reordering window and observed lag, omitted surfaces, and the discovered Monitoring limit ([contracts/gcp-feeder.md §8](./contracts/gcp-feeder.md)); fixture: gcp-partial-poll-01
- [X] T061 [US1] Implement the silence rule and the history horizon in `state.go`: retraction after *N* consecutive **complete** polls with a valid end inside the last poll the entity was observed in, the audit-log deletion instant winning where stated, a **partial poll retracting nothing and not counting toward N**, and a query reaching past the horizon being told so (FR-012, FR-023, FR-024); fixture: gcp-partial-poll-01
- [X] T062 [US1] Implement the deleted-and-recreated rule: a service deleted and recreated under the same name is **not** one continuous entity — the old is retracted, the new created, and any evidence linking them is emitted as claims for the resolution layer (FR-025), in `internal/feeders/gcp/state.go`; fixture: gcp-service-recreated-01
- [X] T063 [P] [US1] Author fixture `fixtures/gcp-baseline-topology-01/` — synthetic structural twin: services, revisions, traffic split, pointers, goldens for the manifest queries, with **observed_at pinned** per `fixture-format.md`'s rule; fixture: gcp-baseline-topology-01
- [X] T064 [P] [US1] Author fixture `fixtures/gcp-rollout-traffic-shift-01/` — a revision created at 14:18 and given 100% at 14:20, asserting **two** rollout changes with the two distinct instants and the traffic-shift marked as having moved traffic; fixture: gcp-rollout-traffic-shift-01
- [X] T065 [P] [US1] Author fixture `fixtures/gcp-revision-zero-traffic-01/` and `fixtures/gcp-rollback-01/` — the zero-traffic revision, and traffic moved at 14:20 and back at 14:41 giving the split property three versions, with a query as of 14:30 reporting the new revision serving; fixture: gcp-revision-zero-traffic-01, gcp-rollback-01
- [X] T066 [P] [US1] Author fixture `fixtures/gcp-service-recreated-01/` and `fixtures/gcp-partial-poll-01/` — the recreated-name case, and a poll that fails part-way asserting **zero retractions** and a declared gap; fixture: gcp-service-recreated-01, gcp-partial-poll-01. **Done 2026-09-26: both fixtures are committed and verify.** `gcp-partial-poll-01` already did. `gcp-service-recreated-01` was held out on the theory that a retraction and a re-assertion "do not commute", and that theory was wrong. The projector lets an assertion made at or after a retraction resurrect the entity in any order. Building the fixture found three defects behind the failure, all fixed. (1) **The GCP feeder froze every Cloud Run service at its first poll.** The service's event id was its ref alone, and an event id is an idempotency key, so every later poll was a DUPLICATE_NOOP. The recreated service was retracted and never re-asserted, and a traffic split that moved never reached the node: `gcp-rollback-01`'s committed golden had the old revision serving at 14:30, against its own manifest. The id now carries the uid and `updateTime` (`serviceEventID`), and a later state is valid from `updateTime` (`LaterStateFact`), an unknown start where the platform states none. (2) **The owner edge had the same flaw**, so a recreated service lost its owner. Its id now carries the subject's `createTime`. (3) **A node retraction's cascade dropped `changed_by` edges that began after it.** Such an edge is written from an `observe_change`, so the planner recovers no assertion for it and `planRetract` dropped it whenever the retraction was applied after the change. The cascade now leaves alone a version that starts at or after the retraction's end and that no edge assertion produced (`laterUnassertedVersion`). Without it the recreation shuffle fails. All GCP fixtures were regenerated and every changed golden was reviewed: 31 changed, 23 only in event ids, version ids or provenance, and 8 are corrections. `gcp-rollback-01`, `gcp-rollout-traffic-shift-01` and `gcp-revision-zero-traffic-01` now show the service's split and generation moving at the platform's instants. `contracts/gcp-feeder.md` §3.4 is corrected. Left open and recorded as T183 and T184: the same frozen-node defect in four other GCP node kinds, a known limit after a feeder restart, and two projector order-dependences found on the way.
- [X] T067 [US1] Wire `internal/cli/feed_gcp.go`: `feed gcp --projects --regions --dry-run --record`, where `--dry-run` performs the startup gate and nothing else; fixture: gcp-baseline-topology-01
- [X] T068 [US1] Implement coverage measurement against GCP's own answer (FR-144): the feeder's service and revision sets versus GCP's lists inside the history horizon, with differences **enumerated rather than summarised**, in `internal/feeders/gcp/coverage.go`; fixture: gcp-partial-poll-01

**Checkpoint**: US1 independently testable. `fixture verify --filter 'gcp-baseline-topology-01,gcp-rollout-*,gcp-revision-*,gcp-rollback-01,gcp-service-recreated-01,gcp-partial-poll-01'` green; SC-001, SC-002 and SC-003 measurable.

---

## Phase 4: User Story 2 — A vendor notice becomes a change that has not happened yet (Priority: P1)

**Goal**: the largest coverage gain in the audit, and the project's first **announced facts** — a
change whose valid time begins after its observed time.

**Independent Test**: replay one maintenance email, one status-page incident, one changelog
deprecation, one cancellation and one duplicate; compare change nodes, valid intervals, targets and
corrections to the golden. Then ask the graph, as of the announced window, what is expected to
change; and as of a recorded incident's instant, what changes intersect it.

- [X] T069 [US2] Implement `internal/feeders/vendornotice/feeder.go`: `Describe()` with source id `vendor-notice:<org-slug>`, a **separate credential, cadence, budget and checkpoint** from the GCP feeder (FR-002), working with any subset of the three sources present; fixture: vendor-maintenance-future-01
- [X] T070 [US2] Implement `internal/feeders/vendornotice/allowlist.go`: the vendor × product allowlist, where a non-allowlisted announcement is **dropped, counted, and visible as a drop with its reason** in operational telemetry, never emitted "just in case" (FR-059); fixture: vendor-not-allowlisted-01
- [X] T071 [US2] Implement `internal/feeders/vendornotice/extract.go`: typed extraction of the six fields, validated against the published schema, treating announcement text as **untrusted input from outside the organisation** that is never an instruction, and **failing loudly naming the field** rather than emitting a guess (FR-073); fixture: vendor-unextractable-01
- [X] T072 [US2] Implement the unextracted path in `extract.go`: an announcement with no recognisable vendor, window or product is recorded as **unextracted** with its reason and message pointer so a human can see what the feeder could not read, and is **not emitted as a change** (FR-074); fixture: vendor-unextractable-01
- [X] T073 [US2] Implement `internal/feeders/vendornotice/announce.go`: the CHANGE node with actor kind `VENDOR`, the deterministic `vendor.notice` ref of FR-076, and the **announced window as the valid interval** emitted unchanged even when it begins after the read instant (FR-062). Use `ChangeKind.DEPRECATION` and `VENDOR_INCIDENT` — the spec's `OTHER` fallback is dead; fixture: gcp-audit-controller-effect-01
- [X] T074 [US2] Implement the announced-fact invariants in `announce.go`: observed time is **never** moved forward to match an announced window (FR-063); a vague window marks the valid start **unknown** and never stores the vendor's wording in place of a timestamp (FR-069); fixture: vendor-maintenance-future-01
- [X] T075 [US2] Implement the announcement-state machine in `announce.go` on T016's vocabulary: default `announced`, never promoted to `confirmed` without an observation that the window happened, and an announced window that passes in silence stays `announced` **for ever** (FR-066); fixture: vendor-maintenance-future-01
- [X] T076 [US2] Implement corrections in `announce.go`: a cancellation and a reschedule each close the observation and open a new one on the **same** change ref, **never** rewriting the valid interval and **never** creating a second change node, so "what did we believe on 20 September about 2 October?" stays answerable (FR-067, FR-068); fixture: vendor-cancellation-01, vendor-reschedule-01
- [X] T077 [P] [US2] Implement `internal/feeders/vendornotice/vendor.go`: THIRD_PARTY nodes for allowlisted vendors with host-name claims, satisfying FR-119's *certain* rule — a configured assertion, not a resemblance (FR-077); fixture: vendor-not-allowlisted-01
- [X] T078 [US2] Implement duplicate handling: identity claims naming vendor, product, window and the vendor's notice identifier, letting **the published rules merge them**; the feeder merges nothing and **does not suppress its own observation**; where no shared identifier exists the pair is raised as a resolution suggestion (FR-070), in `internal/feeders/vendornotice/announce.go`; fixture: vendor-duplicate-two-sources-01
- [X] T079 [US2] Implement `internal/feeders/vendornotice/source_mailbox.go`: read-only and **state-preserving** (FR-007). For IMAP use `EXAMINE` **and** `UID FETCH … (BODY.PEEK[])` — both are needed, because RFC 9051 §6.3.2 permits a read-only `SELECT` to change per-user state and `\Seen` is exactly that — asserting `[READ-ONLY]` in the tagged OK **before** fetching and aborting if absent; for the Gmail API use `gmail.readonly` alone (which structurally cannot reach `messages.modify`) against a shared mailbox provisioned as a **real user account**, since no API can read a Google Group's archive (research §9); fixture: vendor-maintenance-future-01
- [X] T080 [P] [US2] Implement `internal/feeders/vendornotice/source_statuspage.go`: an Atlassian Statuspage adapter keyed on `scheduled_for`/`scheduled_until` from `/api/v2/scheduled-maintenances.json` and `/api/v2/incidents.json`, **behind a capability probe** — coverage is not universal (Stripe and GitLab 404, `status.cloud.microsoft` 401), and a probe mistaking "not Statuspage" for "nothing announced" would defeat FR-075; fixture: gcp-cloudsql-scheduled-maintenance-01
- [X] T081 [P] [US2] Implement `internal/feeders/vendornotice/source_changelog.go`: RSS/Atom and JSON-feed adapters plus a vendor-native JSON adapter (e.g. `status.cloud.google.com/incidents.json`, whose `begin`/`end` are RFC 3339 with explicit offsets), with an HTML-scraping adapter as a **first-class** citizen rather than a special case; fixture: vendor-maintenance-future-01
- [X] T082 [P] [US2] Implement `Deprecation`/`Sunset` header capture in `internal/gcpx/` and the notice feeder's shared HTTP transport: RFC 9745 (Standards Track) and RFC 8594, recorded from every vendor API the integration already calls and fed into the same pipeline — the only standardised deprecation source, and near-free (research §10); fixture: unit
- [X] T083 [US2] Implement cadence and gap handling in all three sources: configurable cadence, respecting any publisher-stated limit, identifying itself honestly, and an unreachable source producing **a gap in the checkpoint, not silence** (FR-075), across `internal/feeders/vendornotice/source_mailbox.go`, `source_statuspage.go` and `source_changelog.go`; fixture: gcp-partial-poll-01
- [X] T084 [US2] Implement `internal/feeders/vendornotice/sanitise.go`: the body never reaches disk, a log or any artifact **at any point including on a failed or aborted run**; only the six typed fields, a bounded summary built **from the extracted fields** (not the text), and a pointer to the message identifier survive (FR-071, FR-072); fixture: unit
- [X] T085 [US2] Implement per-cycle reporting (FR-078): announcements read, extracted, dropped by allowlist, failed to extract, and merged — so *"the graph knows about no upcoming vendor change"* is distinguishable from *"the feeder read nothing"*, in `internal/feeders/vendornotice/feeder.go`; fixture: vendor-not-allowlisted-01
- [X] T086 [P] [US2] Author fixture `fixtures/vendor-maintenance-future-01/` — the announced fact, joining 001's `announced-fact-01`: a window read 17 September for 2 October, with goldens for the three queries of [quickstart.md §2](./quickstart.md); fixture: vendor-maintenance-future-01
- [X] T087 [P] [US2] Author fixtures `fixtures/vendor-cancellation-01/` and `fixtures/vendor-reschedule-01/` — corrections, asserting the prior belief recoverable by an observed-time query, **zero** valid intervals rewritten, and one change node with two recoverable windows; fixture: vendor-cancellation-01, vendor-reschedule-01
- [X] T088 [P] [US2] Author fixtures `fixtures/vendor-duplicate-two-sources-01/`, `fixtures/vendor-not-allowlisted-01/` and `fixtures/vendor-unextractable-01/` — exactly one change node from two sources merged by a published rule, a counted drop, and a visible extraction failure; fixture: vendor-duplicate-two-sources-01, vendor-not-allowlisted-01, vendor-unextractable-01
- [X] T089 [US2] Add a property test asserting the announced-fact invariants across generated inputs: observed time never in the future; a cancellation never closes a valid interval; a change whose valid start is after a ranked list's reference instant is excluded **for that stated reason** rather than by scoring low (FR-065, SC-007), in `internal/feeders/vendornotice/announce_property_test.go`; fixture: unit
- [X] T090 [US2] Wire `internal/cli/feed_vendor_notice.go`: `feed vendor-notice --allowlist --record`, with a credential distinct from the GCP feeder's; fixture: vendor-not-allowlisted-01

**Checkpoint**: US2 independently testable. SC-006, SC-007, SC-008, SC-009 and SC-010 measurable; the pattern the feature exists to defeat is demonstrable on the twins.

---

## Phase 5: User Story 3 — The engine runs the algebra against Cloud Logging and Cloud Monitoring (Priority: P1)

**Goal**: the first **live** telemetry backend — feature 001's pointers become executable, and
feature 002 stops being a replayer of recordings.

**Independent Test**: against recorded GCP responses, execute every operation of the algebra over a
window and compare each digest to its golden; then run the same requests live and check the digests
match field for field including the whole coverage block for a settled window.

- [X] T091 [US3] Implement `internal/backends/gcp/backend.go`: `Describe()` declaring vendor `gcp`, **all eight telemetry terms and nothing else**, one published cost class each, `ReadOnly: true` on every capability, the redaction policy naming `contracts/sanitisation.md` version 1.0.0, and `Execute()` refusing an out-of-algebra request `QUERY_FAILED / OUTSIDE_ALGEBRA` naming what was asked and what is available. Executing a term **creates no GCP object of any kind** — no saved query, log sink, dashboard, alerting policy, analytics dataset or table (FR-112), which is why §4 refuses log-based metrics and Observability Analytics despite their being the better way to aggregate logs; fixture: gcp-rollout-traffic-shift-01
- [X] T092 [US3] Implement `internal/backends/gcp/coverage.go`: the mandatory coverage block, calling `internal/investigation/backend.ClampRequest` and `AnnotateHorizon` rather than re-deriving the horizon rule, and setting `truncated_to_horizon`/`horizon` from the **shipped** proto. Record **the reference instant the request asked about and `coverage.executed_at`, the instant it was executed** (FR-093) — and note why there is no third: observed-time pinning has no meaning against a live backend, which has no memory of what it used to say, so a reproducible investigation uses recorded responses rather than a re-execution; fixture: gcp-rollout-traffic-shift-01
- [X] T093 [US3] Implement ingestion lag in `coverage.go`: for metrics, `MetricDescriptor.metadata.ingestDelay` via `projects.metricDescriptors.get` — *"Data points older than this age are guaranteed to be ingested"* — falling back to the documented 120 s for Cloud Run when empty **and stating which source was used**; for logging, `now − max(receiveTimestamp)` reported **as an estimate**, with `ingestion_lag_undetermined` where neither yields a figure; fixture: gcp-rollout-traffic-shift-01
- [X] T094 [US3] Set `quota_undetermined = true` on every real-time answer, because GCP reports no in-band remaining figure and the `serviceruntime` figure is a minutes-stale reconciliation signal rather than a coverage fact ([contracts/budget.md §3](./contracts/budget.md)), in `internal/backends/gcp/coverage.go`; fixture: gcp-rollout-traffic-shift-01
- [X] T095 [P] [US3] Implement `internal/backends/gcp/joinkeys.go`: the published mapping of [data-model.md §9](./data-model.md#9-join-keys-the-published-mapping) — revision → `version`, instance connection name → `pod_or_host`, trace → `trace_ids` (empty here), first-seen → `first_seen`, Cloud Run service → `workload` — **pseudonymised, never dropped**; fixture: gcp-rollout-traffic-shift-01
- [X] T096 [US3] Implement `internal/backends/gcp/compare.go` against `timeSeries.list` using **server-side** `aggregation`/`secondaryAggregation` (`alignmentPeriod` ≥ 60 s, `perSeriesAligner`, `crossSeriesReducer`, `groupByFields`), returning per-series statistics and an explicit `Comparison` — and **no point-by-point samples**, which the aggregation makes structural; fixture: gcp-rollout-traffic-shift-01
- [X] T097 [US3] Implement `internal/backends/gcp/errors_by_version.go` grouping by `resource.labels.revision_name` with `resource.type = "cloud_run_revision"` pinned, so the revision values are join keys matching WORKLOAD nodes (FR-089); fixture: gcp-rollout-traffic-shift-01
- [X] T098 [US3] State in every `request_count`-derived digest's coverage that error rate is **derived** (there is no dedicated Cloud Run error metric, so it comes from `response_code_class`) and that `request_count` **excludes requests that never reached the container** — the 401/403 at the ingress and the 429/503 at max-instances, exactly the failures an incident is often about, in `internal/backends/gcp/coverage.go`; fixture: gcp-rollout-traffic-shift-01
- [X] T099 [US3] Implement `internal/backends/gcp/onset.go` **backend-side**: Cloud Monitoring returns the series to the backend process, which calls `pkg/backend/onset` — the shipped estimator, not a reimplementation — and returns only `OnsetDigest`. The raw samples cross the digest boundary in **neither** direction and appear in no response, exemplar or coverage field (FR-087a); fixture: gcp-rollout-traffic-shift-01
- [X] T100 [US3] Implement `internal/backends/gcp/logpatterns.go` against `entries.list`, mining templates **client-side** with feature 002's Drain-style miner over a **bounded sample**, because Cloud Logging has no server-side aggregation and both alternatives require a mutation of the organisation's project (research §6). Report `volume_considered`, the truncation criterion, and the sub-window actually covered; fixture: gcp-rollout-traffic-shift-01
- [X] T101 [US3] Implement the pagination-correctness rule in `logpatterns.go`: an empty page **with** a `nextPageToken` means the search is unfinished — keep going or report `PARTIAL` naming what is missing — and is **never** `NO_DATA`. Only an empty page with no token, outside the ingestion lag, is `NO_DATA`. Do not design against the undocumented ~1,000-entry and ~10 MB page caps; request a large page, accept what comes, enforce the feature's own budget; fixture: gcp-rollout-traffic-shift-01
- [X] T102 [US3] Implement `internal/backends/gcp/error_spans.go`: the term is **served** and answers `NO_DATA` with coverage **naming the trace data source as absent** and stating nothing was searched — identical live and recorded, stable for the whole window — so a consumer concludes "this cannot be checked here" rather than "there were no error spans" (FR-091, SC-011); fixture: gcp-rollout-traffic-shift-01
- [X] T103 [P] [US3] Implement `internal/backends/gcp/exemplars.go`: bounded, redacted exemplars returned **only on explicit request**, never by default, capped by the same published limits a recording uses, counting toward the digest size bound (FR-096); fixture: gcp-rollout-traffic-shift-01
- [X] T104 [P] [US3] Implement `internal/backends/gcp/drilldown.go`: a handle per group, template, series or error kind plus a console deep link opening the same query over the same window — a **reference, never an embedded payload** (FR-103); fixture: gcp-rollout-traffic-shift-01
- [X] T105 [US3] Implement the published digest bounds in `internal/backends/gcp/backend.go`: maximum series, groups or templates, exemplars per group, length per exemplar and total size, with **every truncation stated explicitly** naming what was dropped and the criterion used to choose what was kept (FR-099), and the window specification supporting an explicit interval and a before/after pair whose digest carries an explicit comparison rather than two unrelated summaries (FR-092); fixture: gcp-rollout-traffic-shift-01
- [X] T106 [US3] Implement the retention answer: a window older than the vendor's retention (6 weeks for `run.googleapis.com/*`) answers `QUERY_FAILED / OUTSIDE_RETENTION` (T020) carrying the horizon — **never** `NO_DATA`, which would tell an investigation nothing happened during a window nobody could look at; fixture: unit
- [X] T107 [US3] Implement recorded mode: identical digests to live for the same request including the whole coverage block and every join key, **no network call**, never falling through to a live call, with `NOT_RECORDED` for an in-algebra request the world does not hold; compute `response_digest` via `pkg/backend.ResponseDigest` (which calls `NormaliseForDigest`) rather than reimplementing the three cleared fields; fixture: gcp-rollout-traffic-shift-01
- [X] T108 [US3] Implement `FR-111`: accept the pointers the feeders emit **without translation**, reporting an unexecutable pointer `QUERY_FAILED / UNSUPPORTED_POINTER` naming the kind and vocabulary rather than executing it as something else; fixture: gcp-baseline-topology-01
- [X] T109 [P] [US3] Author `fixtures/gcp-rollout-traffic-shift-01/world/` — a **world**, not a trajectory (FR-107): the union of every term the deterministic engine issues for the fixture's question and the cross product of the algebra over the alert neighbourhood and window grid, plus depth-1 drill-downs; fixture: gcp-rollout-traffic-shift-01
- [X] T110 [P] [US3] Author fixture `fixtures/gcp-telemetry-rejection-01/` — SC-013's refusal path: an event carrying a metric sample is **REJECTED** with a published reason code, asserting zero digests and zero parts of one anywhere in the graph; fixture: gcp-telemetry-rejection-01
- [X] T111 [US3] Register the backend in `internal/cli/worker_backend.go` so `backend list` prints vendor `gcp`, the eight terms, their cost classes and window caps, and `read_only: true` on every row; fixture: unit
- [X] T112 [US3] Implement the miss-rate metric (FR-108): the share of in-algebra requests over a recorded world answered `NOT_RECORDED`, reported on every verification run against its published threshold, extending `internal/investigation/backend/missrate.go` and `scripts/check-report.sh`; fixture: gcp-rollout-traffic-shift-01

**Checkpoint**: US3 independently testable. SC-011, SC-012, SC-013, SC-014 and SC-015 measurable; `error_spans` answers honestly with no tracing present.

---

## Phase 6: User Story 4 — Cloud Monitoring alerts, on the shared intake (Priority: P1)

**Goal**: GCP alert policies become ALERT nodes and their transitions become `alert.transition`
events under the **same** convention feature 002's human-declared intake uses, so the engine sees one
trigger per incident.

**Independent Test**: replay recorded policies and the transitions of a real incident window, ask for
the alert by policy identifier, compare the alert, what it watches and its state history to the
golden — then deliver the same transition twice, by both transports, and check the history is
unchanged.

- [X] T113 [US4] Implement `internal/feeders/gcp/monitoring.go`: ALERT nodes from `projects.alertPolicies.list` (GA in the GAPIC) carrying the policy identifier as identity, the display name as a **claim** never as identity, severity only where stated, and each condition's filter as a pointer in the registered Monitoring vocabulary; fixture: gcp-alert-transition-01
- [X] T114 [US4] Implement `WATCHES` linking in `monitoring.go` derived from condition filters and resource labels, with an unresolvable-target alert **kept and marked unattached** and attached automatically when a target appears, on T017's convention (FR-048); fixture: gcp-alert-transition-01
- [X] T115 [US4] Implement transition reads from **`projects.alerts.list`/`.get`** behind a **declared capability flag**, using `Alert.openTime`/`closeTime` as the transition instants and `state` ∈ {`OPEN`, `CLOSED`}. The API is **Public Preview** and its only Go binding is the **deprecated** `google.golang.org/api/monitoring/v3` — the flag exists because of that, not as an operational convenience (research §5); fixture: gcp-alert-transitions-unavailable-01
- [X] T116 [US4] Implement the `monitor_state` digest in `internal/backends/gcp/monitor_state.go`: the transitions in the window with their instants, the state at the start and end of the window, the per-group states where the policy has groups, and the transition count (FR-098); fixture: gcp-grouped-alert-01
- [X] T117 [US4] Implement the flag-off degradation: `monitor_state` answers `NO_DATA` with coverage **naming the absent source**, ALERT nodes still exist from the GA policy read, and the feature works with transitions unavailable — saying which, in `internal/backends/gcp/monitor_state.go`; fixture: gcp-alert-transitions-unavailable-01
- [X] T118 [US4] Emit `alert.transition` (001 event body 25) with valid time **the transition instant Google reports**, never the instant the feeder learned of it, keyed on 001's derived 4-tuple `(source, stable alert identifier, group, transition instant)` = source `gcp`, policy identifier, group, Google's instant (FR-046), in `internal/feeders/gcp/monitoring.go`; fixture: gcp-alert-transition-01
- [X] T119 [US4] Implement per-group alerts (FR-049): a policy opening incidents per grouped resource produces **one alert per alerting group**, each with its own state history naming its policy, and the policy-level entity is **never** reported as alerting because one group is (SC-005), in `internal/feeders/gcp/monitoring.go`; fixture: gcp-grouped-alert-01
- [X] T120 [US4] Mark every polled history **sampled** at the poll interval on T015's marker, so a consumer can tell a quiet policy from an unwatched one (FR-051), in `internal/feeders/gcp/monitoring.go`; fixture: gcp-alert-transition-01
- [X] T121 [US4] Implement suppression (FR-052): flapping and no-data transitions do not trigger an investigation, are **still recorded** in the state history, and the suppression is **stated** rather than applied silently; a closing transition is emitted and **kept as evidence**, in `internal/feeders/gcp/monitoring.go`; fixture: gcp-alert-transition-01
- [X] T122 [US4] Implement `internal/feeders/gcp/doorbell.go`: the Pub/Sub path means **"poll now" and nothing else** — body never parsed, trusted or stored; endpoint authenticated and rate limited; a forged, replayed or malformed notification costs at most one extra poll and creates, alters or retracts **nothing**; fixture: gcp-forged-doorbell-01
- [X] T123 [US4] Document the doorbell's grant honestly in `docs/connectors/gcp.md` and the read-only statement: `pubsub.subscriptions.consume` authorizes pull, ack, `modifyAckDeadline` **and** seek together, `roles/pubsub.viewer` cannot pull, and **no read-only pull exists at any granularity, not even with a custom role** — so FR-006's exception is **holding the grant**, bound resource-level to one operator-created subscription, with the integration running correctly when it is absent; fixture: n/a
- [X] T124 [US4] Implement `FR-053`: hand an alert to the investigation engine by policy identifier and group, returning the alert, the entities it watches, the transition instant to use as the reference instant, and the pointers to execute — **with no telemetry payload**, in `internal/feeders/gcp/monitoring.go` exposed through `internal/cli/feed_gcp.go`; fixture: gcp-alert-transition-01
- [X] T125 [P] [US4] Author fixture `fixtures/gcp-alert-transition-01/` — an incident opening 02:07 and closing 02:51, with both transports delivering and **exactly one** `alert.transition` in effect regardless of arrival order (SC-004); fixture: gcp-alert-transition-01
- [X] T126 [P] [US4] Author fixtures `fixtures/gcp-grouped-alert-01/` and `fixtures/gcp-forged-doorbell-01/` — N alerting groups producing exactly N alerts and zero policy-level alerts, and a forged notification changing nothing; fixture: gcp-grouped-alert-01, gcp-forged-doorbell-01
- [X] T127 [P] [US4] Author fixture `fixtures/gcp-alert-transitions-unavailable-01/` — the capability flag **off**, asserting `monitor_state` answers `NO_DATA` with the absent source named and ALERT nodes still present, so the degradation path ships **tested** rather than assumed; fixture: gcp-alert-transitions-unavailable-01
- [X] T128 [US4] Add a test asserting convergence with feature 002's human-declared intake: a GCP transition and a Slack declaration for the same underlying alert produce `alert.transition` events under the same convention and the engine sees **one** trigger, not two shapes (FR-047), in `internal/feeders/gcp/monitoring_test.go`; fixture: unit

**Checkpoint**: all four P1 stories complete. The MVP is deliverable: topology, announced vendor changes, an executable algebra and a shared alert intake.

---

## Phase 7: User Story 5 — Cloud SQL, configuration and secret versions (Priority: P2)

**Goal**: the instances behind the services, the configuration the revisions were deployed with, and
the dependency edges that can be **derived** — with the rest raised for a human rather than guessed.

**Independent Test**: replay recorded Cloud SQL and Cloud Run configuration payloads and compare the
instances, derived edges, proposed dependencies awaiting confirmation, and configuration and
maintenance changes to the golden.

- [X] T129 [US5] Implement `internal/feeders/gcp/cloudsql.go`: INFRA_RESOURCE nodes from `instances.list` via **`google.golang.org/api/sqladmin/v1`** — *not* the preview GAPIC, whose `SqlInstancesClient.List` returns an iterator over **warnings** rather than instances (research §4) — with an **unknown** valid start where GCP states none; fixture: gcp-cloudsql-flag-change-01
- [X] T130 [P] [US5] Publish FR-120's **certain** rule in `internal/resolution/`: two sources claiming the same Cloud SQL **instance connection name** are one entity, with the claim minted by `internal/feeders/gcp/cloudsql.go` (FR-035); fixture: vendor-duplicate-two-sources-01
- [X] T131 [US5] Implement flag and settings changes in `cloudsql.go`: a `CONFIG_CHANGE` valid at the audit entry's instant, naming what changed with old and new values **only where GCP states them**, with actor, actor kind and a `changed-by` edge to the instance (FR-031); fixture: gcp-audit-controller-effect-01
- [X] T132 [US5] Implement maintenance in `cloudsql.go`: past maintenance from `operations.list` filtered on `operationType` ∈ {`MAINTENANCE`, `RESCHEDULE_MAINTENANCE`}; **scheduled** maintenance from `scheduledMaintenance.startTime` — a genuinely future-dated instant — as a `CLOUD_MAINTENANCE` change with actor kind `VENDOR` whose valid interval is the announced window, **reusing US2's announced-fact machinery rather than reimplementing it** (FR-032); fixture: gcp-audit-controller-effect-01
- [X] T133 [US5] Treat `settings.maintenanceWindow` and `settings.denyMaintenancePeriods` as a recurring **policy**: properties of the instance that **never** become change nodes, in `internal/feeders/gcp/cloudsql.go`; fixture: gcp-cloudsql-flag-change-01
- [X] T134 [US5] Implement `internal/feeders/gcp/depends.go`: a `depends-on` edge asserted where a service's configuration, connection settings or labels name an instance, **with the deriving evidence recorded** (FR-028); fixture: gcp-cloudsql-flag-change-01
- [X] T135 [US5] Implement the proposed dependency in `depends.go` on T012–T013's convention: suggestive-but-insufficient evidence raises a **proposed dependency** with its evidence, score and rationale — **never an edge** — raised once per cycle and not re-raised while pending (FR-029); fixture: gcp-proposed-dependency-01
- [X] T136 [US5] Implement `internal/feeders/gcp/config.go`: CONFIG nodes for what a revision was deployed with — environment variable **names** and value **fingerprints**, mounted configuration references, and secret **version references** — with a configuration change when any changes between revisions (FR-033); fixture: gcp-cloudsql-flag-change-01
- [X] T137 [US5] Enforce in `config.go` that **no secret material and no value the published rules classify as sensitive is stored** — only that the entry changed and the version it moved to (FR-034). A fingerprint answers "did this change?" without storing what it is; fixture: gcp-cloudsql-flag-change-01
- [X] T138 [P] [US5] Implement `internal/feeders/gcp/gke.go`: the one inference cluster as an INFRA_RESOURCE with identity claims naming it as the Kubernetes feeder knows it, and **nothing below it** — no workload, no pod, no container (FR-030); fixture: gcp-baseline-topology-01
- [X] T139 [P] [US5] Author fixtures `fixtures/gcp-cloudsql-flag-change-01/` and `fixtures/gcp-proposed-dependency-01/` — a flag changed at 03:12 with actor and actor kind, and an instance no configuration names producing **no invented edge** and a pending suggestion; fixture: gcp-cloudsql-flag-change-01, gcp-proposed-dependency-01
- [X] T140 [P] [US5] Author fixture `fixtures/gcp-cloudsql-scheduled-maintenance-01/` — a future-dated scheduled maintenance following US2's announced-fact semantics, asserting the same query behaviour as `vendor-maintenance-future-01`; fixture: gcp-cloudsql-scheduled-maintenance-01

**Checkpoint**: US5 independently testable; the second question after "what changed" is answerable.

---

## Phase 8: User Story 6 — Cloud Audit Logs as the general change stream (Priority: P2)

**Goal**: the catch-all that stops the graph's answer to "what changed" being "only the things we
wrote a special case for".

**Independent Test**: replay a recorded admin-activity window containing a known culprit among
decoys, run feature 001's ranked diff, and check the culprit ranks in the top three with a rationale
citing its time and hop distance.

- [X] T141 [US6] Implement `internal/feeders/gcp/auditlog.go` reading the **admin-activity** stream via `entries.list` with the `logName` filter, `resourceNames` ≤ 100, filter ≤ 20,000 characters, and `orderBy: "timestamp desc"` (Google's own advice for recently ingested entries); fixture: gcp-audit-other-kind-01
- [X] T142 [US6] Enforce that the **data-access** stream is not read and **no permission for it is requested** (FR-042), and assert this from the published read-only operation list, enforced in `internal/feeders/gcp/auditlog.go` and stated in `docs/connectors/gcp.md`; fixture: unit
- [X] T143 [US6] Implement the pagination rule in `auditlog.go`: `nextPageToken` present with `entries` empty means the search is **not finished** — an implementation that stops there under-reads its own window while the checkpoint claims an extent it did not cover; fixture: gcp-partial-poll-01
- [X] T144 [US6] Implement valid/observed time discipline: `timestamp` is the event instant and the **only** field used as valid time; `receiveTimestamp` is the delivery instant used **only** for lag measurement and watermarking (FR-041), in `internal/feeders/gcp/auditlog.go`; fixture: gcp-audit-other-kind-01
- [X] T145 [US6] Implement the measured reordering window in `auditlog.go`: **Google publishes no ingestion-delay or out-of-order bound** (research §3), so re-query a trailing overlap window every poll, deduplicate on **`insertId`**, continuously measure the `receiveTimestamp − timestamp` distribution and **report it** so the configured window is justified from data; fixture: vendor-duplicate-two-sources-01
- [X] T146 [US6] Implement late arrivals: an entry arriving **after** its extent was checkpointed is **still emitted** with the entry's own instant as valid time, and the gap stands in the checkpoint — which is what feature 002's reopening path exists for, in `internal/feeders/gcp/auditlog.go`; fixture: gcp-partial-poll-01
- [X] T147 [US6] Implement the taxonomy mapping in `auditlog.go`: a kind from the published taxonomy where one fits, else `CHANGE_KIND_OTHER` **with GCP's own operation name recorded** and never dropped. `IAM_CHANGE` and `QUOTA_CHANGE` now exist, so the "other" fallback no longer covers them (FR-039); fixture: gcp-audit-other-kind-01
- [X] T148 [US6] Implement coverage of FR-038's minimum: IAM policy changes, quota changes, scaling and capacity changes, resource deletions and service-enablement changes, with `changed-by` edges to every resource the entry names that the graph can resolve, in `internal/feeders/gcp/auditlog.go`; fixture: gcp-audit-other-kind-01
- [X] T149 [US6] Make the in-scope log types, services and operations **configurable** and record the configuration in force in the checkpoint, so a later reader can tell "no change happened" from "we were not looking for that kind of change" (FR-040), in `internal/feeders/gcp/auditlog.go` with the keys in `config/gcp.yaml`; fixture: gcp-partial-poll-01
- [X] T150 [US6] Implement identity claims per FR-043: the target resource name, the revision, the image digest, and the **pipeline or commit reference** the entry carries — which is how feature 004's Vercel and GitHub Actions changes will resolve with these rather than duplicate them, in `internal/feeders/gcp/auditlog.go`; fixture: vendor-duplicate-two-sources-01
- [X] T151 [P] [US6] Author fixture `fixtures/gcp-audit-other-kind-01/` — an operation with no taxonomy equivalent emitted under "other" with GCP's operation name, never dropped; fixture: gcp-audit-other-kind-01
- [X] T152 [P] [US6] Author fixture `fixtures/gcp-audit-controller-effect-01/` — an autoscaling adjustment by a platform principal four minutes after symptom onset, asserting actor kind `CONTROLLER` so feature 002's causal ordering can treat it as a candidate **effect** rather than a candidate cause; fixture: gcp-audit-controller-effect-01

**Checkpoint**: US6 independently testable; the ranked diff finds a culprit among decoys.

---

## Phase 9: User Story 7 — The organisation's own payloads, sanitised, are the tests (Priority: P2)

**Goal**: the real corpus. **Phases 3–8 authored synthetic structural twins for the public
repository; this phase records the real payloads into the private one** (FR-128) — and per FR-130,
recording **starts as soon as the credential and mailbox access exist**, in parallel with every phase
above, because neither topology history nor an announcement stream can be reconstructed afterwards.

**Independent Test**: committed fixtures replay from empty to their goldens, double-deliver as
no-ops, shuffle inside the declared reordering window, and pass the independent secret and
personal-data scan. Separately, the public twins replay to their own goldens with the private corpus
absent.

- [X] T153 [US7] *(done 2026-09-22 for `record|sanitise|scan|sign`; **`parity` is T159 and is not registered**, because it compares a live run against the recording of the same run and there is no live side until the poller and a credential exist — the help says so rather than offering a command that cannot work. `record` runs FIRST, not last: the other gates read the campaign record, so a recording with none cannot be gated at all, and `--add-scope` performs both halves of a scope change in one step because doing only the first leaves a record whose windows are not continuous. **Two of the five names promise more than they can deliver and the commands say so.** `sanitise` is not the first cleaning — FR-137 puts that in the connector before anything touches disk, so by the time a campaign directory exists the cleaning has either happened or the recording is already unsafe; it asserts and stamps. `scan` does **not** satisfy FR-138, because this binary contains the sanitiser and a check run from inside it shares every defect the sanitiser has by construction; it names `scripts/check-no-secrets.sh` instead of reporting a gate it cannot be. The people check needs no key so the gate runs in CI where the corpus key is deliberately absent; with a key present a surviving canary is attributed to its campaign, and the report says which ran. **Two defects found by pointing the gate at a committed fixture.** (1) `PeopleInArtifact` had no exemption for a GCP **service account**, which is email-shaped infrastructure — a Cloud Run revision carries `runtime@<project>.iam.gserviceaccount.com`, so every real GCP recording would have failed the commit gate, and a gate that fails on the ordinary case is one somebody switches off. Exempted on the Google-owned domain itself, which cannot hold a human mailbox, and probed against a too-broad match. (2) `WriteManifest` enforced the second-signature count, which made a two-person sign-off **unreachable**: the first signature could never be written, so the second person had nothing to countersign. Split into `CheckShape` (write) and `Check` (commit); the CLI test caught it)* Implement `internal/cli/fixture_campaign.go`: `fixture campaign record|sanitise|scan|sign|parity`, with sanitisation already applied **in the connector** during recording (T042) and this pass being the corpus-level stamp of the policy version, not the first cleaning; fixture: the private corpus
- [X] T154 [US7] *(done 2026-09-22 in `internal/campaign/` rather than in the CLI file the task names: the record is data with invariants, and business logic in a cobra command is logic no test reaches without the command harness. `internal/cli/fixture_campaign.go` is the thin layer over it. **`scopes` is a SERIES, not a field**, and that is the task's substance: FR-130 has recording begin before the scope is agreed, so the scope *changes during* the campaign, and "what was in scope" is a set of windows with instants. A reader a year later finds no events for a project over a week and has to choose between "it was quiet" and "we were not looking" — opposite conclusions about one silence. So the windows are half-open `[From, To)` like valid time, only the last may be open, and **continuity is validated**: a gap makes the record answer "not in scope" for an instant nobody decided about, and an overlap gives two answers to one question. `ScopeAt` returns *no window* outside the campaign rather than the nearest one, because the nearest one would turn "we were not recording" into "this is what we recorded". The mailbox carries the access path, **who authorised it and when** — an authorisation is an event by a named person, and a config file naming a destination does not record that anyone agreed to it — and the subscribed-member path is refused without a note, because it reaches that person's whole mailbox and a record that did not say so would understate what was allowed. **No mailbox address is stored**: `mailbox_address` carries the reason instead, so a reader who looks for it finds the reason rather than concluding somebody forgot. Four code-level probes: continuity unchecked, the window closed at its end, `ScopeAt` falling back to the nearest window, and `Update` validating the change instead of the result)* Implement the campaign record: the scope in force throughout (FR-130), the access path for the mailbox and who authorised it (FR-132), and the first campaign's project and region scope (FR-131), written by `internal/cli/fixture_campaign.go` into the campaign directory's `campaign.yaml`; fixture: the private corpus
- [X] T155 [US7] *(done 2026-09-22 in `internal/sanitise/manifest.go`, written as `sanitisation.yaml` beside the fixture manifest rather than inside it — `fixture record` regenerates the fixture manifest, and a regeneration that silently reset the signatures would defeat the gate. **Three decisions carry it.** (1) A signature is an *attestation*: a name and a date, nothing verified against a key. There is deliberately no `Verify` for signatures, the type says so and the written file carries an `attestation` field saying so, because a reader who took it for crypto would trust it more than it deserves — and what FR-140 is actually for is that somebody read this, which no key can record. (2) **The content hash is over the recording, not the manifest.** A hash of the manifest would be a signature on a description: change a payload and the manifest still matches itself. `HashRecording` walks the directory hashing `len:path:len:contents` per file in lexical order — the lengths are what stop `ab` holding `c` hashing the same as `a` holding `bc` — and excludes the manifest, which cannot be inside its own hash. `Verify` names *what* changed (added / removed / edited) rather than only that something did. (3) **"Where a second person exists" cannot be decided by a manifest**, since nothing in a directory says so, so the count comes from the campaign record's signatories and one signature is refused against two declared — a flag on the command is how a single sign-off waves itself through. Two signatures from one person are refused as one, case-insensitively. `Countersign` **re-verifies before adding**: a second signature on content that changed since the first is worse than none, because it reads as two reviews of one recording. Six probes: the manifest inside its own hash, no length prefixes, one person counted twice, the second-signature rule removed, countersigning without re-verifying, and `WriteManifest` overwriting a sign-off)* Implement the signed manifest (FR-140): the sanitisation policy **version**, the content hash of what was signed, the named signatory, the date, **what was dropped**, and a second signature where a second person exists. A payload that cannot be made safe is **dropped and the drop documented** — never committed on a promise to clean it later, in `internal/cli/fixture_campaign.go`, emitted as each recording's `manifest.yaml`; fixture: the private corpus
- [ ] T156 [US7] Run the recording campaign covering FR-129's minimum: a baseline window; an incident window with a real alert transition and the change that preceded it; a quota-exhausted response; a poll that failed part-way; and real vendor notices including **a future-dated maintenance window, a deprecation, a provider incident, a cancellation, a reschedule and a duplicate delivered by two sources**; plus at least one window whose correct answer is that nothing is implicated, into `fixtures-private/campaign-<date>/` in the private repository; fixture: the private corpus
- [ ] T157 [US7] Commit the sanitised corpus and its recorded **telemetry backend responses** (FR-133) to the private repository, addressable by the request that produced them, so feature 002 replays an investigation with **no call to GCP** and gets identical digests, under `fixtures-private/` and `fixtures-private/*/world/` in the private repository; fixture: the private corpus
- [X] T158 [US7] *(done 2026-09-22 as `.github/workflows/corpus.yml` in the private repository. Its README said it was owed four gates **before** the first campaign and that until then every commit there should be treated as unguarded; all four now run per push and PR: FR-138's independent scan plus its self-test, FR-139's canary and personal-data check, FR-140's signed manifest, and FR-142's conformance suite. The tooling is checked out from the public repository rather than vendored, because a second copy of the sanitiser is one that can drift and FR-142's claim is that the private repository runs *the same* suite. **The empty-corpus case is the one a gate gets wrong**, so corpus presence is established once in its own step and every later step is gated on that answer rather than on its own ability to find files — `check-no-secrets.sh` exits 0 on "nothing to scan" and `fixture verify` over no directory is a pass over zero fixtures, so either would have reported a silent green for ever. **Two findings, both in the public repository.** `sanitise.Manifest.Check` is documented as *the commit gate* and had **no caller** outside its own unit tests, so a recording could be committed unsigned, with a stale hash, or with one signature where two were declared — `fixture campaign verify` is now that caller. And `Manifest.Verify` promised in its own doc comment to name the file whose contents changed while the code could only say "one of these was edited": the manifest held paths and no per-file digest, so `recording:` is now `{path, sha256}` and a mismatch names what was edited, added or removed. Verified by running the whole chain locally — record, scan, sign, verify, replay, report — and confirming the gate refuses an unsigned recording, refuses a lone signature on a two-signatory campaign, and names `events.jsonl` when a signed recording is edited)* Set up private CI in the private repository running **the same conformance suite** as the public one over the real corpus (FR-128), as `.github/workflows/corpus.yml` in the private repository; fixture: the private corpus
- [X] T159 [US7] *(done 2026-09-22 as `internal/fixture/parity.go` plus `fixture campaign parity`. **Two assertions, and they fail for different reasons** — which is the reason neither is optional. The GRAPH half catches a connector that behaves differently when recording: a code path taken only with `--record`, an instant read from the clock rather than the payload, an order that reached the emitter differently. The DIGEST half catches a backend deriving an answer from the **connection** rather than the response (FR-008) — and that one produces *identical graphs and different digests*, so a check of the graph alone would pass exactly the case FR-008 exists to prevent. The graph comparison is the structural snapshot the shuffle step already uses, not raw event bytes: two runs legitimately mint different event ids, and a check failing on those would fail every parity run and be switched off. The world comparison is the **bytes**, because a recorded response is what the digest was computed from and canonicalising would hide the field a reordered map turned into; a world file's name is its request's digest, so a name on one side only is reported as a different failure from the same request answered differently — and answering *more* requests is still a failure, since a live run reading something the recording lacks is one the recording cannot replay. Both sides are recordings, because a live run's output **is** one, which is what makes the check runnable and testable today; the run FR-143 asks for still needs the poller and a credential. Refuses to report parity over two empty directories, and an absent world is not a failure — a topology recording legitimately has none. **One finding, in the test rather than the code**: the first mutation dropped the last event and parity correctly held, because that event is a `sourceCheckpoint` and moves no valid-time state — the mutation asserted nothing until it removed the observed side of the cross-source merge instead. Four probes: the digest half skipped, an extra live request tolerated, parity over nothing passing, and an absent world treated as a difference)* Implement the live parity check (FR-143, SC-016): the graph built live equals the graph built from the recording of the same run, byte for byte; and a digest computed live over a settled window equals the digest from the recorded response for the same request, on every field including the whole coverage block, as `fixture campaign parity` in `internal/cli/fixture_campaign.go`; fixture: the private corpus
- [X] T160 [US7] *(done 2026-09-22: in `internal/sanitise/sanitise_test.go`, with **three triggers** because the path has three shapes and conflating them leaves a hole. A people identifier is dropped from a payload that is still written; an **unassigned field** aborts the recording of that payload entirely and nothing of it reaches the sink; and an **encoder that reintroduces** something the field walk never saw is caught on the bytes — that third one is why `RecordBytes` asserts the encoded bytes rather than the map, and deleting that assertion left the test green until the third trigger existed. Probed four ways: writing before asserting, tolerating an unassigned field, removing the @handle detector, and never reaching the sink at all. **Two findings.** The first attempt used `@example.com`, which `PeopleInArtifact` exempts on purpose as an RFC 2606 documentation name, so the encoder case proved nothing until it used an @handle — an exemption worth knowing about before relying on that assertion. The second is the reason this task also touched `internal/cli/feed_gcp.go`: **`--record` on a live run wrote raw payloads**, because nothing routes it through `SanitisedRecorder`, which is used only by its own tests. The hazard is unreachable today — the live poller is not wired, so `--record` cannot be reached live — which is exactly when a requirement gets forgotten, so the refusal is now on the FLAG and whoever wires the poller meets it. Two CLI tests, both probed, assert the refusal names FR-137 and that nothing is created on disk before it)* Add the aborted-run containment test: leave an unsanitised payload deliberately, abort the run, and assert the scan finds nothing on disk — FR-137's "including during a failed or aborted run" made executable rather than asserted, in `internal/sanitise/sanitise_test.go`; fixture: unit

**Checkpoint**: the connector may be marked **stable** — constitution VIII's bar, which synthetic-only data cannot clear.

---

## Phase 10: User Story 8 — The integration stays inside Dana's quota (Priority: P2)

**Goal**: the policy, reporting and degradation on top of Phase 2B's token buckets. The mechanism is
foundational; **this story is the published behaviour around it**.

**Independent Test**: replay a recorded run containing quota-exhausted responses and check the
integration backs off as instructed, that its usage report matches the number of calls in the
recording **exactly**, and that a run under a small budget drops work in the published order rather
than at random.

- [X] T161 [US8] Implement the share-of-remaining policy in `internal/gcpx/budget.go`: 40% of `logging.read` with a ≥30% human reserve, 50% of `monitoring.query` with ≥25%, decaying toward the reserve floor as remaining quota falls, and **yielding** at the floor; fixture: gcp-quota-exhausted-01
- [X] T162 [US8] Implement the published **deferral order** (FR-149): load balancers and DNS first, then the general audit stream, then Cloud SQL polling, and only then anything the P1 stories depend on — reporting what was deferred and why, and reflecting reduced coverage in the checkpoints, in `internal/gcpx/budget.go` with the order in `config/gcp-budget.yaml`; fixture: gcp-quota-exhausted-01
- [X] T163 [US8] Implement window caps per operation and cost class (FR-150): a request wider than its cap is **narrowed to the cap or refused with the cap named**, never issued as asked — with the tightest cap on `new_log_patterns`, the term that spends `logging.read`, in `internal/gcpx/budget.go` and enforced per term in `internal/backends/gcp/backend.go`; fixture: gcp-quota-exhausted-01
- [X] T164 [US8] Implement the typed stop reason: stopping for quota is reported **distinctly from** stopping for lack of evidence, because an investigation that ran out of quota and one that found nothing implicated are opposite conclusions (FR-149), in `internal/gcpx/budget.go`; fixture: gcp-quota-exhausted-01
- [X] T165 [P] [US8] Author fixture `fixtures/gcp-quota-exhausted-01/` — a quota-exhausted response asserting the instructed backoff, no lost position, `QUERY_FAILED / RATE_LIMITED` with the earliest retry instant where GCP states one, and a usage report matching the recording **to the exact call** (SC-022); fixture: gcp-quota-exhausted-01
- [X] T166 [US8] Document the cost in `docs/connectors/gcp.md` (FR-154): calls per hour per area at the default cadence against an estate of a stated size, and the GCP-side products and volumes read — so Dana approves a **number** before it runs; fixture: n/a

**Checkpoint**: US8 independently testable; the integration cannot make an incident worse.

---

## Phase 11: User Story 9 — Load balancers and DNS, where they are cheap (Priority: P3)

**Goal**: the cheapest remaining change sources. **Explicitly the first thing cut if the budget is
short** (FR-057).

**Independent Test**: replay recorded load-balancer and DNS payloads and compare the exposure
relations and change nodes to the golden.

- [X] T167 [P] [US9] Implement `internal/feeders/gcp/lbdns.go`: `exposed-via` relations from an in-scope service to its load balancer with host names as properties, read with **`roles/compute.viewer`** — not `roles/compute.networkViewer`, which grants `trafficdirector.networks.reportMetrics`, a write (research §8.2); fixture: gcp-lb-dns-01
- [X] T168 [P] [US9] Implement DNS and backend changes in `lbdns.go`: a `DNS_SWITCH` change valid at the audit entry's instant with record name, old and new targets where GCP states them, actor and actor kind; a backend swap as a `CONFIG_CHANGE` with the superseded `exposed-via` relation **retracted at the instant of the swap rather than deleted**; fixture: gcp-audit-controller-effect-01
- [X] T169 [US9] Implement the budget-and-permission omission (FR-057): where either surface is outside the budget or the credential's permissions, read **neither**, state the omission in the checkpoint as a **scope statement**, and leave every other requirement unchanged, in `internal/feeders/gcp/lbdns.go`; fixture: gcp-lb-dns-01
- [X] T170 [P] [US9] Author fixture `fixtures/gcp-lb-dns-01/` — exposure relations and both change kinds, plus a run with the surfaces omitted asserting the scope statement rather than silence; fixture: gcp-lb-dns-01

**Checkpoint**: all nine stories complete.

---

## Phase 12: Polish, and the gate the feature is actually measured by

- [X] T171 *(done 2026-09-22 in the private repository as `coverage-audit-2026-09-22.md` and `.json`, because per-incident attribution is per-incident detail and the public repository carries only the aggregate. **8 of 13, ADR-0004's figure, met exactly; the ceiling did not move.** The attribution is `audit coverage compare` between consecutive rungs rather than a judgement written by hand: three incidents land at `+deploy` and five at `+vendor-notice`, INC-11 among them — the announced maintenance window a human found in an email six days late, which is the incident this feature was argued from. **The finding is that 003 raises no ceiling on this corpus, and that it was structurally certain.** 003 shipped far more than the rung credits — alert transitions, Cloud SQL configuration and flags, secret versions, the audit stream, load balancers and DNS — and none of it moves an incident from not observable to observed. The two tempting cases are checked one at a time: the Secret Manager feeder observes a GCP secret's *version* changing, not an external vendor's key leaking from elsewhere; and the Cloud SQL feeder observes the instance, not application table rows in the product database, which it deliberately never reads. What 003 adds here is explanation, not recall, and the ceiling bounds recall. FR-070's own ranking predicted it: the five missing causes are one each of `latent_bug`, `client_side_configuration`, `business_data_change`, `credential_leak` and `capacity_limit`, and not one is a control-plane change — the feeder worth writing next is an application-configuration feeder, not more cloud coverage. **The documented command does not exist in the shape this task gives it**: `--feeders otel,k8s,gcp,vendor-notice` is `--feeder-set <name>` against the rungs the input declares, a tenth instance of the drift T179 catalogued. §4 of the document states what the re-run does and does not re-decide: it recomputes the arithmetic, the classification, the attribution and the remainder's split, and checks the 2026-09-17 `observability` judgements against the connector as built — but it does not replay thirteen incidents through the graph, which needs T156. Every row of the per-incident table was cross-checked programmatically against the run's own JSON: 13 rows, 0 mismatches)* **Re-run the coverage audit** (FR-145, SC-023): `audit coverage --feeders otel,k8s,gcp,vendor-notice` by its published method over the same thirteen incidents, publishing `docs/evaluation/coverage-audit-<date>.md` with **per-incident attribution** — whether the cause was in the graph at alert time, **which feeder** supplied it, and for the remainder whether the correct answer is `unobserved` or `not_change_induced` with its reason. **The target is at least 8 of 13**; fixture: the 13-incident corpus
- [ ] T172 Demonstrate SC-024 rather than asserting it: for at least one recorded incident whose true cause was a missed vendor notice, show the notice's change node in the **top three** of the ranked change list at the alert's reference instant — the pattern this feature exists to defeat, via `investigate --replay … --explain-ranking` in `internal/cli/investigate.go`, recorded in `docs/evaluation/coverage-audit-<date>.md`; fixture: the private corpus
- [ ] T173 Verify SC-025: from an alert policy identifier alone, obtain the alert, what it watches, what changed around it including any announced vendor window intersecting it, who owns it, and executable pointers — in **one command and under thirty seconds** on the recorded corpus, driven by `internal/cli/investigate.go` per [quickstart.md §10](./quickstart.md) and recorded in `specs/003-gcp-integration/quickstart-run-<date>.md`; fixture: the private corpus
- [X] T174 *(done 2026-09-22: the capability half, which is the half the criterion actually asks for — "verifiable from the set of operations the integration can issue, not only from one run's behaviour". It landed as `internal/gcpx/requestlog.go`: the published list is now a Go table, `Budget.Issue` is the only door to a call and takes the OPERATION, deriving its endpoint class from the table, and `Budget.take` is unexported so nothing can spend a call without naming what it issues. Four assertions, each probed against a planted violation: every published operation is a `list` or a `get` with exactly one declared state change (the doorbell); an unpublished operation is refused before any quota is spent, on a nil budget too; `docs/connectors/gcp.md` §2 is parsed and compared to the enforced list in both directions including the area column; and every live reader method that reaches a Google client first issues a constant-named published operation (`internal/feeders/gcp/readonly_test.go`, go/ast over transport.go). The request log records BLOCKED attempts as well as served calls, which is what lets `ReadOnlyHonoured` fail — it could not otherwise, since `Issue` consults the list first. The backend half is in the same log by construction: it spends the same budget through the same readers and its rows say `backend`. **Two findings.** §1's custom Cloud SQL role granted `cloudsql.databases.list`, which no call site issues — removed, since the page's own rule is that the role is assembled from what the connector issues. And three published names are METHODS rather than IAM permissions (`cloudsql.operations.list`, `cloudsql.flags.list`, `pubsub.subscriptions.pull`) because no permission of their own exists; the code said they were all permissions and now says which are not. **The live-run half is open**, and the page says so rather than implying a run that did not happen: it needs a real credential against a real organisation, the same blocker as US7)* Verify SC-020 from the integration's **own recorded request log** over the full corpus and the live run: every operation the integration is capable of issuing is on the published read-only list — the **backend half included**, which creates no GCP object when it executes a term (FR-112) — and the only state change anywhere is the doorbell acknowledgement, asserted from the request log and published in `docs/connectors/gcp.md`; fixture: all 003 fixtures
- [X] T175 *(done 2026-09-22: the clauses split three ways. **Clause 1** — zero automated merges from probable rules — was gated only on `.metrics.calibration`, which `calibrationReport` returns nil for unless the fixture's family is `ambiguous-identity`, so every GCP and vendor fixture passed it by not being measured by it. It is now held over all 50 fixtures from `decisions_by_rule`, which every reported fixture emits, together with a new assertion that an `auto_merge` naming no rule is a failure — `resolve why` renders the rule per decision, so such a merge is unexplainable whatever else is beside it. **Clauses 2 and 3** measured nothing at all: every fixture in the corpus was single-source, so no GCP claim had ever met another source's, C4/C5/C7 had never fired anywhere, and "100% of cross-source merges are explainable" and "≥95% merge under a certain rule" were both satisfied by having nothing to measure. `fixtures/gcp-cross-source-merge-01` ends that — two connectors, one entity, C4 firing for the first time in the corpus — and `crossSourceReport` measures both figures over the pairs its manifest labels, guarding the LABEL too (a pair claimed by one source, or with no agreed environment on both sides, is outside the criterion and fails rather than counting). Six gate assertions, each probed against a planted report, plus two probed end to end against planted code and fixture faults. When no fixture labels a pair the gate says NOT MEASURED and annotates the run, because a metric absent everywhere reads exactly like one that passed. **Building it found two defects in C4**, both recorded in the commits: it merged the observed service with the revision rather than its Cloud Run service, collapsing two revisions into one entity across a rollout; and it was order-dependent, so a permutation that applied a revision before its service left the merge unmade for ever. The ≥95% figure is now **100% of 2 labelled pairs**: C4 on a service whose telemetry name differs from its Cloud Run name, and **C5** on one that declares `OTEL_SERVICE_NAME`, with no Cloud Run attributes on the export at all so the merge rests entirely on the declaration. Both had never fired anywhere. Two labelled `distinct_pairs` hold the other half — the two revisions, and the two services — because a rule loose enough to merge everything would satisfy the rate alone. **C7 remains un-exercised and un-exercisable today**: it resolves two sources reporting the same Cloud SQL *connection name*, and no second source in this repository reports one — the OpenTelemetry feeder claims `server.address` for a dependency, which is a host and port. That is a real gap rather than a missing fixture, and it is the follow-up)* Verify SC-021: zero automated merges from probable rules; 100% of merges involving a GCP or vendor claim explainable by `resolve why`; ≥95% automatic merging under a certain rule for entities present in both GCP and another source with an agreed environment, via `resolve why` and asserted in `scripts/check-report.sh`; fixture: all 003 fixtures
- [X] T176 Wire the new fixtures into `.github/workflows/ci.yml`'s replay step and extend `scripts/check-report.sh` with this feature's report assertions: miss rate against threshold, digest parity, coverage-vs-GCP enumerated differences, budget to the exact call, body containment, canary survival zero; fixture: all 003 fixtures
- [X] T177 [P] Write `docs/schema/announced-facts.md`: the semantics 001 publishes and this feature is the first consumer of — an announced fact is legal, observed time is never in the future, the ranker's exclusion is stated rather than accidental, the announcement-state vocabulary, and a cancellation being a correction not a retraction; fixture: n/a
- [X] T178 [P] Complete `docs/connectors/gcp.md` and `docs/connectors/vendor-notice.md`: the custom Cloud SQL role definition (because `roles/cloudsql.viewer` grants `instances.export`), the three-layer gate and **what it cannot prove**, the doorbell grant's honest description, and the mailbox paths' per-path guarantees with the DWD tenant-wide residual risk; fixture: n/a
- [X] T179 *(done 2026-09-22, recorded in [quickstart-run-2026-09-22.md](./quickstart-run-2026-09-22.md): **§0–§6 and §11 reproduce; §7, §8, §9 need a real GCP credential and §10 needs the private incident corpus, so they were not run.** Nine documented commands did not exist in the shape the quickstart gave them, and **not one was a code defect** — every one was the doc naming a flag the CLI does not have, or a flag where the CLI takes a positional argument: `migrate up`, `fixture verify --filter` (seven occurrences), the step names `idempotency`/`refusal`, `query subgraph --focus/--valid-at`, `query diff --t1/--t2/--reference-at`, `query history --ref/--both-dimensions`, `resolve why --a/--b`, `worker backend`, and `-v` (which is the global version flag). All fixed. Two open items recorded rather than resolved: an excluded announced change is excluded by being absent from the candidate set, so the reason an operator sees is "touched no neighbourhood" rather than `post_reference` — which of the two §2(c) intends is a design question; and `fixture campaign` is **not implemented**, so §8 is unreachable with or without a credential, which is US7's work and already tracked as blocked. The §2 announced-fact scenario — the one the feature exists for — reproduces exactly: the same valid-time question at two observed instants gives opposite answers)* Reproduce [quickstart.md](./quickstart.md) §1–§11 end to end on a clean worktree and record the run, as feature 002 did in `quickstart-run-2026-09-18.md`, fixing every doc and code drift it exposes; fixture: all 003 fixtures
- [X] T180 *(done 2026-09-22 at each push, and in full at `0232fb1`: `make gen` reproducible with no diff; `go vet ./...` clean; `golangci-lint run ./...` **0 issues**; `go test -count=1 ./...` all packages; `CGO_ENABLED=1 go test -race ./...` clean; `buf lint` and `buf breaking` against `origin/main` clean; `scripts/check-no-secrets.sh` clean and its self-test reporting "every rule fired on its planted sample"; `scripts/check-specs.sh` and its self-test likewise; `scripts/check-migrations.sh` 10 files; and `fixture verify fixtures/*/ --report` over **50 fixtures, 354 goldens, all passed**, with `scripts/check-report.sh` reporting every assertion held. Two things this task's list does not mention and that were run anyway because they are where the gate has teeth: the report gate's new SC-021 assertions over the whole corpus, and every new assertion probed against a planted violation. `make verify` is the fixture step above with `$PG_DSN` set; PostgreSQL 16 from the repository's own embedded distribution, run as an unprivileged user)* Run the full local gate before the PR: `make gen build test lint verify`, `CGO_ENABLED=1 go test -race ./...`, `buf lint`, `buf breaking`, `scripts/check-no-secrets.sh` and its self-test, `scripts/check-migrations.sh`, and `fixture verify` over every new fixture; fixture: unit
- [X] T183 Apply T066's fix to the four other GCP node kinds whose event id is still their ref alone, and which are therefore frozen at their first poll: Cloud SQL instances, GKE clusters, load balancers and alert policies (`apply_us5.go`, `monitoring.go`). The evidence is `gcp-cloudsql-flag-change-01`: the 14:12 flag change is emitted as a change event, but the instance node's `sre.gcp.sql_database_flags` never moves. Each needs its own version stamp: Cloud SQL `settingsVersion` or `etag`, GKE `etag`, a forwarding rule's `fingerprint`, an alert policy's `mutationRecord`. Only the alert policy states an update instant, so the others date a later state as an unknown start at the observation. The same restart limit as T184 applies. Every golden that changes must be reviewed as a correction, as T066's were; fixture: gcp-cloudsql-flag-change-01 **Done 2026-09-27, and not with platform stamps.** The task proposed each kind's own version stamp (`settingsVersion`, `etag`, `fingerprint`), but the recorded twins carry none of them, and a Cloud SQL instance, a GKE cluster and a forwarding rule state no update instant either. So each state is named by a digest of what is asserted: display name, properties and pointers (`stateAssertion`, `nodestate.go`). The first state a run sees keeps the valid start its `NodeFact` already gives it, and its id is the ref plus the digest. An unchanged re-poll repeats the id already sent and stays a no-op. A changed state gets the digest plus the observation instant in its id, so a change back to an earlier state is not dropped as that state's duplicate. It is dated from the platform's stated last-change instant where there is one, which is the alert policy's `mutationRecord` (now read into `MutateTime`), and otherwise marked unknown from the observation (FR-011). `TestEachStateOfAReReadNodeHasItsOwnID` and `TestALaterStateIsDatedFromAStatedInstant` cover it. **The defect was visible in exactly one fixture:** in `gcp-cloudsql-flag-change-01`, the instance at 20:00 now has `max_connections=500` and `log_min_duration_statement=250`, valid from the 14:30 observation, where the committed golden still said `max_connections=200`. Four goldens there are corrections. The other 13 goldens changed only in event ids, version ids or provenance, and all 67 fixtures pass. The restart limit is the same as the Cloud Run one and stays with T184.
- [ ] T184 Close T066's restart limit, and fix the two projector order-dependences found trying to close it. **The limit:** the feeder remembers uids per run, so after a restart its first poll dates a service's current state from `createTime`, and a change made while it was down loses to the previous run's later state. The natural fix is an existence assertion (keyed on `createTime`) plus a state assertion (keyed on `updateTime`) on every first poll. It was implemented and broke two shuffles for reasons in the projector, not the feeder. (a) A placeholder minted for an edge endpoint keeps its `valid_from_unknown` after a real assertion from earlier reaches back over it. `coalesce` then refuses to merge across it, and a boundary survives that exists only because the edge arrived first (`gcp-lb-dns-01`, the `exposed-via` edge at 14:00). (b) Under a C8 or C1 merge, a `changed_by` edge was re-planned as open-ended rather than keeping the change's own one-microsecond interval, and a merged node split at the second assertion's instant (`gcp-cross-source-merge-01`). Fix (a) and (b), then reinstate the two-assertion first poll; fixture: gcp-lb-dns-01, gcp-cross-source-merge-01
- [X] T182 Supply **the observed side of C4** (FR-117): an OpenTelemetry service running on Cloud Run must claim the revision it runs in, or the rule can never fire. The GCP feeder now emits the revision-locating attributes on its own claims (`sre.gcp.revision_name`, `sre.gcp.project`, `sre.gcp.region`), but nothing translates the resource attributes the GCP OTel detector sets — `cloud.platform=gcp_cloud_run`, `faas.version`, `cloud.region`, `cloud.account.id` — onto the claim feature 002's aggregator emits, so the matching half is absent. Carry them in `internal/feeders/otel/aggregate.go` and `emit.go`, gated on `cloud.platform`, and regenerate the 002 fixtures the claim attributes appear in. Until it lands, C4 is published and dead, which is what T175's 95% figure will measure; fixture: gcp-baseline-topology-01

- [X] T181 *(done 2026-09-22: the record and the open follow-up list are at the end of this file. **The feature is not closed, and the record says so**: items 1 and 4 are asserted (replay and conformance green over 50 fixtures; quickstart §0–§6 and §11), item 3 is asserted for the capability half and open for the live run, and item 2 cannot be asserted from this repository at all because the thirteen-incident detail is private. Eight follow-ups recorded beside it)* Write the definition-of-done record in this file asserting the four items against the runs on origin, as feature 002's T125 did: replay and conformance green over every new fixture, the coverage audit re-run published with per-incident attribution, SC-020's read-only verification from the request log, and quickstart reproduced — with the open follow-up list recorded beside it, in this file `specs/003-gcp-integration/tasks.md`; fixture: the 13-incident corpus

---

## Dependencies & Execution Order

### Phase dependencies

- **Phase 1 (Setup)**: no dependencies.
- **Phase 2A (contract additions)**: depends on Setup. **Blocks everything** — and T009–T011 (the
  pointer vocabularies) block Phase 5 specifically, because a backend cannot execute a pointer whose
  vocabulary is unregistered.
- **Phase 2B (credential, gate, budget)**: depends on 2A's `make gen` (T022). Blocks every vendor
  call, because FR-004 forbids emitting anything before the credential is proved read-only.
- **Phase 2C (sanitisation)**: depends on Setup; independent of 2A and 2B and can run in parallel with
  either. Blocks the first **recording** (Phase 9) and the live digest path (Phase 5).
- **Phases 3–6 (the four P1 stories)**: all depend on Phase 2. US1 → US4 have a real ordering
  dependency only where noted below; otherwise they are parallelisable.
- **Phases 7–10 (P2)**, **Phase 11 (P3)**: depend on Phase 2, and on the P1 story they extend.
- **Phase 12 (Polish)**: depends on every story whose result it measures. T171 (the coverage audit)
  needs US1, US2 and US6 at minimum — those are the feeders that supply causes.

### Story dependencies

- **US1 (P1)**: depends only on Phase 2. **The MVP.**
- **US2 (P1)**: depends on Phase 2 plus T016 (announcement state). Independent of US1 — a different
  source, credential and cadence entirely.
- **US3 (P1)**: depends on Phase 2 plus T009–T011 (vocabularies) and US1's `pointers.go` (T059) for
  something to execute against.
- **US4 (P1)**: depends on Phase 2 plus T015 (sampled marker) and T017 (alert unattachment). Its
  `WATCHES` targets resolve better with US1 present but it does not require it — an unattached alert
  is a supported state.
- **US5 (P2)**: depends on US1 (services whose configuration names an instance) and on T012–T013
  (proposed dependency). Reuses US2's announced-fact machinery for scheduled maintenance.
- **US6 (P2)**: depends on Phase 2; shares `auditlog.go` reading with US1's T054, so sequence T054
  before T141 to avoid two readers of the same stream.
- **US7 (P2)**: depends on Phase 2C and on a credential existing. **Recording starts in parallel with
  Phases 3–8, not after them** (FR-130).
- **US8 (P2)**: depends on Phase 2B's buckets; its fixtures need at least one story making calls.
- **US9 (P3)**: depends on Phase 2. Cut first under budget.

### Within each story

- Fixtures and implementation are interleaved, not sequenced: a fixture asserts the behaviour the
  task beside it implements, and constitution VIII means neither ships without the other.
- Nodes before the changes that target them; claims with the entity that mints them.
- Checkpoint discipline (T060, T061) before any retraction path.

### Parallel opportunities

- **Phase 1**: T001–T006 all `[P]`.
- **Phase 2**: 2C (T038–T045) runs fully parallel to 2A and 2B. Within 2A, T011 and T018 are `[P]`.
  Within 2B, T035–T037 are `[P]`.
- **The four P1 stories** can be staffed in parallel once Phase 2 closes, with the two sequencing
  notes above (T059 before US3's execution path; T054 before T141).
- **Every fixture-authoring task** marked `[P]` is parallel to every other — they touch different
  directories.
- **US7's recording runs in parallel with everything**, and must.

---

## Parallel Example: Phase 2

```bash
# Three tracks at once, once Setup closes:
Track A (contract):     T008 → T009 → T010 → T012 → T013 → T015 → T016 → T017 → T019 → T020 → T021 → T022
Track B (sanitisation): T038 → T039 → T040 → T041 → T042 → T043 → T045
Track C (parallel):     T011, T018, T044 alongside their tracks

# Then, once T022 lands:
Track D (credential):   T023 → T024 → T025 → T026 → T027 → T028 → T029 → T030 → T032 → T033 → T034
Track E (parallel):     T031, T035, T036, T037
```

## Parallel Example: the four P1 stories

```bash
# Once Phase 2 closes, four developers:
Developer A: US1  (T046–T068)   ← the MVP; do T059 early, US3 needs it
Developer B: US2  (T069–T090)   ← fully independent source
Developer C: US4  (T113–T128)   ← needs T015 and T017 only
Developer D: US7  (T153–T156)   ← START RECORDING NOW (FR-130)

# US3 (T091–T112) starts as soon as T059 exists.
```

---

## Implementation Strategy

### MVP first (User Story 1 only)

1. Phase 1 (Setup) → Phase 2 (Foundational, all three sub-phases).
2. Phase 3 (US1).
3. **STOP and VALIDATE**: `fixture verify --filter 'gcp-baseline-topology-01,gcp-rollout-*'`; confirm
   SC-001, SC-002 and SC-003 on the twins. The graph now holds what is serving traffic, with the two
   rollout kinds distinguished and their instants exact.
4. This is the increment that moves the coverage number off **0 of 13**.

### Incremental delivery

1. Setup + Foundational → foundation ready, credential proved, nothing unsanitised can reach disk.
2. **+ US1** → what is serving traffic. *(MVP)*
3. **+ US2** → the missed vendor notice, defeated. The largest single coverage gain in the audit.
4. **+ US3** → pointers become executable; feature 002 can answer about right now.
5. **+ US4** → the shared alert intake; both front doors converge on one trigger.
6. **↳ the four P1 stories are the feature's thesis.** Re-run the coverage audit here (T171) even
   before the P2 work: if it does not reach 8 of 13, the P2 stories will not rescue it, and knowing
   that early is worth more than finishing the list.
7. **+ US5, US6** → the database behind the service, and the catch-all change stream.
8. **+ US7** → the real corpus; the connector may be marked stable.
9. **+ US8** → the integration cannot make an incident worse.
10. **+ US9** → the cheap remainder. Cut first if the budget is short.

### Parallel team strategy

Phase 2 as one team (its three tracks are genuinely independent). Then the four P1 stories in
parallel per the example above, with **one person on the recording campaign from day one** — that is
not a resourcing preference, it is FR-130: topology history and an announcement stream cannot be
reconstructed retrospectively, and every day of delay is a day of corpus that does not exist.

---

## Definition of done for 003

Tied to 001's FR-050 discipline and 002's T120 record, written by T181:

1. **Replay and conformance are green over every new fixture** in `ci.yml`: every fixture replays
   from empty to its goldens, every event double-delivers as a `DUPLICATE_NOOP`, every fixture is
   permuted inside its declared reordering window with no valid-time state moving, and every fixture
   compares at least one golden (FR-142, SC-017).
2. **The coverage audit has been re-run and published** after this feature's connectors, with
   **per-incident attribution** — whether the cause was in the graph at alert time, which feeder
   supplied it, and for the remainder whether the correct answer is `unobserved` or
   `not_change_induced` with its reason. The target is at least 8 of 13 (FR-145, SC-023).
3. **SC-020's read-only verification holds from the integration's own request log**, over the full
   corpus **and the live run**: every operation the integration is capable of issuing is on the
   published read-only list, the backend half included, and the only state change anywhere is the
   doorbell acknowledgement (FR-005, FR-112).
4. **Quickstart §1–§11 reproduce** on a clean machine.

### Closure record (T181, 2026-09-22) — **the feature is NOT closed**

Two of the four items cannot be asserted from this repository, and saying so is the record. Written
at `0232fb1`, against the CI runs attached to this branch's commits.

1. **Replay and conformance green — ASSERTED.** `ci.yml` job `test (go test + fixtures)` green on
   `4806baf` and `15fbcd5` (8 of 8 jobs each). Locally at `0232fb1`:
   `fixture verify fixtures/*/ --report` over **50 fixtures, 354 goldens, all passed`, and
   `scripts/check-report.sh` reports every assertion held, including the two new SC-021 gates and
   the per-fixture cross-source figures. `CGO_ENABLED=1 go test -race ./...` green;
   `go vet`, `golangci-lint run ./...` 0 issues; `buf lint` and `buf breaking` against `main` clean;
   `make gen` reproducible with no diff; `check-no-secrets.sh`, `check-specs.sh`,
   `check-migrations.sh` and both guard self-tests clean.
2. **Coverage audit — NOT ASSERTED, and cannot be from here.** T171 and T172 measure the
   thirteen-incident corpus, whose incident-level detail is kept in a private corpus and is not in
   this repository (stated in `docs/evaluation/coverage-audit-2026-09.md`). The audit method and the
   published ceiling are in the repository; the incidents are not. This item closes on the owner's
   machine, not in CI.
3. **SC-020 — the CAPABILITY half asserted, the live run not.** The half the criterion actually
   leans on — "verifiable from the set of operations the integration can issue, not only from one
   run's behaviour" — holds and is enforced rather than reviewed: `Budget.Issue` is the only way to
   spend a call, it takes the operation, `Budget.take` is unexported, and four assertions hold the
   list, the refusal, the page-to-code agreement and the call sites (T174, each probed against a
   planted violation). The **live run** needs a real credential against a real organisation and is
   open, which is US7's blocker. `docs/connectors/gcp.md` §2 says so on the page rather than
   implying a run that did not happen.
4. **Quickstart — §0–§6 and §11 ASSERTED, §7–§10 not.** Reproduced 2026-09-22 and recorded in
   [`quickstart-run-2026-09-22.md`](./quickstart-run-2026-09-22.md). §7, §8 and §9 need a real GCP
   credential; §8 additionally needs the corpus HMAC key and a named human signatory; §10 needs the
   private corpus. Nine documented commands did not exist in the shape the quickstart gave them and
   are fixed; none was a code defect.

### Open follow-up list (T181)

Recorded beside the closure record because an unlisted follow-up is a follow-up nobody does.

| # | item | why it is open |
|---|---|---|
| 1 | **US7, T156 and T157** — running the campaign and committing the corpus | Needs a real GCP credential, mailbox access authorised by an owner, and a **named human signatory** on the manifest (FR-140) — the last of which no automation can supply, by design. T153–T155 and T158–T160 are done: `fixture campaign` is implemented and the private repository's commit gates run. |
| 2 | **T172** — the missed-notice demonstration | Needs a recorded incident to rank over, so it waits on T156's campaign. T171 is done: the audit re-run and its per-incident attribution are in the private repository, 8 of 13. |
| 3 | **T173** — SC-025 in one command under thirty seconds | Same private corpus. |
| 4 | **T174's live-run half** — SC-020 from a live request log | Same credential as item 1. |
| 5 | ~~**C7 has never fired and cannot yet**~~ | **Closed 2026-09-22**, and the recorded reason was wrong in a way worth keeping. It was not only that no second source reported a connection name: `evalC7` searched `gcp.cloudsql.instance` on **both** sides while refusing two claims from one source, and the GCP feeder is the only source claiming there — so the only pair it could match was one that cannot exist. Data alone would not have fixed it. The observed side is a dependency's own address: a client reaching Cloud SQL through the connector's socket has `server.address=/cloudsql/<project>:<region>:<instance>`, the connection name verbatim, which `internal/feeders/otel` now lifts onto the claim; C7 compares across the two namespaces. The fixture carries a third labelled pair, so **SC-021's n is 3, not 2, and all three certain GCP rules fire**. Two findings while building it, both recorded in the commit: reusing the C4 pair's service for the new export silently killed C4, and a mistyped namespace in the ground truth made the new pair vanish from SC-021's denominator with nothing failing — the gate now refuses an unresolved labelled pair. |
| 6 | ~~**`Pointer.join_keys` on the GCP feeder's metric pointers**~~ | **Closed 2026-09-22.** Minted on every executable GCP pointer, derived from the `resource.type` the selector pins so a pointer cannot name a field its own query is unable to return. It re-recorded **19** fixtures, not six. The gap was larger than "a missing field": `metrics.ErrorsByVersionTerm` refuses a pointer that declares no `version` role, so **no GCP pointer could serve `errors_by_version` at all** — `gcp-rollout-traffic-shift-01`, whose whole subject is a traffic shift between two revisions, went from 0 such terms in its world layer to 22. Also fixed the other end: the backend now refuses a term asking to split by a field it does not group by, instead of echoing the name and reporting numbers grouped by another. |
| 7 | **`post_reference` is never the reason an operator sees** for an excluded announced change | The exclusion happens, but by absence from the candidate set, so the printed reason is "touched no neighbourhood". Which of the two §2(c) intends is a design question (see the quickstart run record). |
| 8 | **SC-021's cross-source figures rest on n = 3** | Was n = 2; C7's pair (item 5) makes it three, one per certain GCP rule, all merging at 100%. Still a real measurement over a small n, and more labelled pairs is still the work. The gate announces NOT MEASURED rather than passing vacuously if they go away, and since 2026-09-22 also **fails** a labelled pair whose reference the graph never saw, which is how a pair added to widen the criterion used to disappear from the denominator in silence. |
| 9 | ~~**T183** — four more GCP node kinds frozen at their first poll~~ | **Closed 2026-09-27**: each state is named by a digest of what it asserts; see T183. |
| 10 | **T184** — the restart limit, and two projector order-dependences | A change made while the feeder is down reaches the node only at the next change. The fix exposed placeholder and merge planning defects that come first. |

---

## Notes

- `[P]` = different files, no dependency on an incomplete task.
- **Fixture-first is a constitution rule, not a style**: §Development Workflow says a task that
  cannot name its fixture is not ready. Every implementation task above either names its fixture or
  belongs to a story whose fixtures are listed in the same phase.
- **Two spec statements are stale and must not be implemented**: FR-060's `CHANGE_KIND_OTHER`
  fallback for `deprecation`/`vendor_incident` (the taxonomy named them in ADR-0006), and any reading
  of FR-148 that expects a usable vendor-reported remaining quota (GCP reports none in band).
- **Three design compromises are deliberate** and must survive review rather than be "fixed":
  `new_log_patterns` samples client-side (the alternatives require writing to the organisation's
  project); the doorbell's grant is not read-only (no read-only Pub/Sub pull exists); and the startup
  IAM gate is a tripwire rather than a proof (IAM inherits downward only). Each is disclosed in the
  artifact a reader would consult.
- Commit after each task or logical group. Stop at any checkpoint to validate a story independently.
