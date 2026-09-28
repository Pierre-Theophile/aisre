---

description: "Task list for feature 005 — the Datadog connector"
---

# Tasks: Datadog Connector

**Input**: Design documents from `/specs/005-datadog-connector/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[data-model.md](./data-model.md), [contracts/](./contracts/), [quickstart.md](./quickstart.md)

**Tests**: **Required, not optional.** Constitution VIII (a feature without replayable fixtures does
not merge), FR-077 (every fixture passes the conformance suite), FR-078 (the zero-payload check with
an exercised rejection path) and the zero-tolerance criteria SC-006, SC-007, SC-012, SC-015, SC-017,
SC-018 and SC-021. Fixture tasks are interleaved with implementation, never deferred.

**Organization**: one foundational phase for the six gaps of [plan.md](./plan.md) and the generic
version-stamp package, then the user stories in priority order: US5, US2 and US4 (P1), the
log-observed rollouts that complete US5's scenario 9, US6, US7 and US8 (P2), and the two
off-by-default capabilities US1 and US3 (P3, **deferrable**).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US8]`. Setup, Foundational and Polish tasks carry no story label.
- Every task names the fixture it validates against; `unit` means a Go test in the package named.

## Path conventions

Per [plan.md §Project Structure](./plan.md#project-structure): feeder in `internal/feeders/datadog/`,
backend in `internal/backends/datadog/` following `internal/backends/gcp/` file for file, CLI in
`internal/cli/feed_datadog.go`, shared packages in `pkg/feeder/versionstamp/` and
`pkg/feeder/doorbell/`, fixtures in `fixtures/datadog-*/`.

## Why Phase 2 comes first, and is large

[research.md §1.2](./research.md) found six gaps outside any Datadog package. Three would **silently**
break the connector rather than fail: a GitHub-shaped quota reader reads Datadog's reset as 1970 (G5);
a Datadog claim of `otel.service.name` lets C1 merge staging into production (G4); and without G2 the
spec's `NO_DATA` for an unstamped service can never be produced. The version-stamp package is in the
same phase because it is deliberately **not Datadog's** (FR-040c–FR-040e): the GCP backend adopts it in
the same phase, so the rule is proved on two backends before the first Datadog call.

---

## Phase 1: Setup

**Purpose**: the package skeletons, the decision record, and the published operation list, so nothing
can issue an unpublished request.

- [X] T001 [P] Create `internal/feeders/datadog/doc.go` stating the feeder half's scope per capability, its read-only posture, and that it never reads the graph to decide what to emit (FR-001, FR-040f); fixture: unit
- [X] T002 [P] Create `internal/backends/datadog/doc.go` stating the backend half's scope: all eight terms, digests only, nothing written to the graph (FR-001, FR-051); fixture: unit
- [X] T003 [P] Write `docs/decisions/ADR-0010-shared-contract-additions-for-005.md` recording Gaps G1–G4 of [plan.md](./plan.md) — the version group's deploy ref, the engine-side `NO_DATA`, named query operations, C9 — each with context, decision and the alternatives rejected in [research.md §1.2](./research.md); fixture: unit
- [X] T004 Write `docs/connectors/datadog.md`: capabilities and their scopes, and the operation table of [contracts/read-only-operations.md](./contracts/read-only-operations.md) §2 verbatim, marked **not yet enforced** until T008; fixture: unit

**Checkpoint**: the decision record exists before any schema moves.

---

## Phase 2: Foundational — the six gaps, and version stamps for every backend

**⚠️ CRITICAL**: no user story work begins until this phase is complete. Every schema change is
**additive**: `buf breaking` stays clean and no golden moves.

### 2A — Named query operations on the read-only surface (G3)

- [X] T005 Extend `pkg/feeder/readonly.go` with named query operations: a `POST` is admissible only as an individual `ReadOperationSpec` carrying a non-empty published justification; any other non-GET/HEAD still panics at init ([contracts/read-only-operations.md](./contracts/read-only-operations.md) §1); fixture: unit
- [X] T006 Assert a planted unnamed `POST`, a `PUT`, a `PATCH` and a `DELETE` each panic at init, and a named query without a justification is refused; fixture: unit
- [X] T007 Write `internal/feeders/datadog/requestlog.go`: the Datadog operation table per capability, with only enabled capabilities' operations declared (FR-008b); fixture: unit
- [X] T008 Assert `docs/connectors/datadog.md` and the enforced table are the same list in both directions, and that the never-declared list of [read-only-operations.md](./contracts/read-only-operations.md) §2 is refused (FR-004, SC-015); fixture: unit

### 2B — The deploy ref on a version group (G1)

- [X] T009 Add `deploy_ref` (`sreagent.graph.v1.Ref`) and `deploy_ref_absent_reason` (enum `ABBREVIATED_SHA`, `MUTABLE_TAG`, `BARE_DIGEST`, `NOT_A_STABLE_IDENTIFIER`) to `VersionBreakdown` in `api/sreagent/investigation/v1/investigation.proto`; regenerate with `make gen` ([data-model.md §7.1](./data-model.md)); fixture: unit
- [X] T010 Run `buf lint` and `buf breaking --against '.git#branch=main'`, and confirm every existing golden is byte-identical because both fields are absent when unset; fixture: unit

### 2C — `NO_DATA` for an unstamped pointer, answered by the engine (G2)

- [X] T011 In `internal/investigation/backend/algebra.go`, replace the `OUTSIDE_ALGEBRA` refusal of an `errors_by_version` term with no `version_attribute` by an engine answer: `NO_DATA`, coverage absent source "no version stamp on this pointer", listing the pointer's discovery-verdict candidates with their shares; no backend call ([contracts/version-stamping.md](./contracts/version-stamping.md) §5); fixture: unit
- [X] T012 Assert the answer is identical in live and recorded mode, needs no world entry, leaves every existing world's miss rate unchanged, and is the same whichever backend owns the pointer; fixture: unit
- [X] T013 Update [`specs/002-investigation-engine/contracts/telemetry-backend.md`](../002-investigation-engine/contracts/telemetry-backend.md) §1's `errors_by_version` row to state the engine-side answer, citing ADR-0010; fixture: unit

### 2D — C9: a Datadog log service is an OpenTelemetry service, in the same environment (G4)

- [X] T014 Declare the local namespaces `datadog.service`, `datadog.log_service`, `datadog.monitor`, `datadog.change` in `internal/feeders/datadog/refs.go`, with `datadog.log_service` carrying `deployment.environment.name` as a supporting attribute (FR-058); fixture: unit
- [X] T015 Write `internal/resolution/datadog.go` declaring **C9** per [data-model.md §4](./data-model.md): `datadog.log_service` = `otel.service.name` after normalisation, both environments stated and equal, agreeing Kubernetes namespace/cluster where both state one; certain; specificity above C1 (FR-059); fixture: datadog-log-service-merge-01
- [X] T016 Assert an unstated environment on either side never fires, two claims from one source never merge, and a Cloud Run **declared** OTel name counts as the OTel side; fixture: unit
- [X] T017 Build `fixtures/datadog-log-service-merge-01`: a Datadog log service and a Cloud Run service with one OTel name in `production` (merge), and the same name in `staging` (must stay apart), with `ground_truth.cross_source_pairs` and `distinct_pairs`; fixture: datadog-log-service-merge-01
- [X] T018 Re-evaluate C9 when a claim's entity merges or a late claim arrives, as C8 does (`internal/projector/retrigger.go`), and assert the fixture's shuffle across six seeds; fixture: datadog-log-service-merge-01 **Done 2026-09-27, differently from written.** Building the fixture found that the Datadog side must be a correlation key (one source states one name for its production and staging nodes, and as an identity claim the name landed on whichever was processed first — the shuffle caught it). C9 therefore pairs a correlation with a claim, and the fix for arrival order is not a re-trigger but running from both: `Rule.CrossKind`, the one case allowed both evaluators, each looking up only the other kind. The fixture gives each arm a pair of its own (`voice-agent` watched before the Cloud Run poll, `billing` after), since the two sources arrive further apart than the shuffle window; deleting either arm fails its golden, and deleting the environment check fails all four.
- [X] T019 Probe T015–T016 by reverting each and confirm the fixture's goldens or shuffle fail; extend `scripts/check-report.sh` so `auto_merge/C9` is counted; fixture: datadog-log-service-merge-01
- [X] T020 [P] Add C9 to `docs/schema/resolution.md` with its condition table, and record the **pre-existing** C1 environment hazard (C1 merges `otel.service.name` across environments) as a known limit for its own change; fixture: datadog-log-service-merge-01

### 2E — The Datadog quota reader (G5)

- [X] T021 Add a Datadog header reader beside the GitHub one in `pkg/feeder/quota.go`: `X-RateLimit-Reset` as seconds from now, `X-RateLimit-Period`, bucket named by `X-RateLimit-Name` ([research.md §2.3](./research.md)); fixture: unit
- [X] T022 Assert against recorded Datadog headers that the reset is in the future, two endpoints sharing a name share a bucket, and a response with no headers falls back to the static budget and says so (FR-081a); fixture: unit

### 2F — The shared doorbell and the Datadog campaign scope (G6)

- [X] T023 Lift the payload-free `Ring` and token bucket from `internal/feeders/gcp/doorbell.go` into `pkg/feeder/doorbell/`, leaving the GCP feeder on the shared package with its goldens unchanged; fixture: gcp-forged-doorbell-01 (existing, goldens unchanged)
- [X] T024 Add an HTTP transport to `pkg/feeder/doorbell/`: requires the configured shared-secret header, never reads the body, drops and counts a missing or wrong secret, rate-limited (FR-025b); fixture: unit
- [X] T025 [P] Add Datadog scope fields (organisation, site, environments, indexes, log sources) to `internal/campaign/record.go` and make the parity step's credential message platform-neutral; fixture: unit

### 2G — The pointer vocabularies and the bound marker

- [X] T026 [P] Register `datadog-logs/v1` and `datadog-monitor/v1` in `pkg/feeder/pointer.go` and `docs/schema/pointers.md` per [contracts/pointer-vocabularies.md](./contracts/pointer-vocabularies.md), each with its reason for not being OTel; `pkg/feeder/vocabularies_test.go` must pass; fixture: unit
- [X] T027 [P] Add the published property `sre.change.valid_from_is_a_bound` to `pkg/feeder/props.go`, and move 003's owner-local marker onto it only if no golden moves (else leave 003's key and record why); fixture: unit **Done: 003's key left as it is** — it marks an owner rather than a change, and moving it would rewrite GCP goldens for no change in meaning; the reason is on the new constant.

### 2H — Version stamps, for any deployment type (FR-040c–FR-040e)

- [X] T028 Create `pkg/feeder/versionstamp/conventions.go`: the ordered convention list v1.0.0 of [contracts/version-stamping.md](./contracts/version-stamping.md) §2, tag and attribute as **separate candidates**, with a per-service override; fixture: unit
- [X] T029 Implement the verdict in `pkg/feeder/versionstamp/verdict.go`: per candidate the line share and error-line share, acceptance at the published 95 % / 95 % defaults over a one-hour window, first accepted wins, every rejection with its reason ([data-model.md §3](./data-model.md)); fixture: unit
- [X] T030 Assert stability is **not** a criterion (a constant value on every line is accepted) and the audit's case is rejected: 255 of 37.2 M lines, all on start-up lines, 0 % of error lines; fixture: unit
- [X] T031 Implement `pkg/feeder/versionstamp/normalise.go`: value → `deploy.commit_sha` / `deploy.image` (name + digest pair) / `deploy.release`, else an absent reason, using `pkg/feeder/deployref.go` only ([contracts/version-stamping.md](./contracts/version-stamping.md) §4); fixture: unit
- [X] T032 Assert each absent reason: an abbreviated sha is `ABBREVIATED_SHA` and never padded, `:latest` is `MUTABLE_TAG`, a bare `sha256:…` is `BARE_DIGEST`; fixture: unit
- [X] T033 Fill `deploy_ref` in the **GCP** backend's `errors_by_version` (`internal/backends/gcp/errors_by_version.go`) where a revision's image digest is known, via `versionstamp`, and re-record the affected GCP goldens as additions only; fixture: gcp-rollout-traffic-shift-01 **Done 2026-09-27, differently from written.** The backend sees only the revision label from Cloud Monitoring; the image digest lives in the graph, and reading it there would make a telemetry backend a graph reader. So each group's revision name goes through `versionstamp.Normalise` like any stamped value and becomes `deploy.release` — a Cloud Run revision name is a platform-assigned release identifier. It joins to nothing until a feeder correlates on it (T100). No golden moved: no GCP fixture's goldens hold an `errors_by_version` digest, and all 68 fixtures verify.
- [X] T100 Correlate each Cloud Run rollout's revision name as `deploy.release` in the GCP feeder (`internal/feeders/gcp/rollout.go`), beside its image digest, so a GCP `errors_by_version` group and a log stamped `DD_VERSION=$K_REVISION` both resolve to the rollout. C8 still excludes `deploy.release`, so this is a lookup join, never a merge; add `K_REVISION` as the zero-effort Cloud Run recipe in the stamping guide (T093); fixture: gcp-rollout-traffic-shift-01
  - *T100 notes:* `RolloutDeployKeys` adds `deploy.release=<revision name>` to every Cloud Run
    rollout, normalised with `feeder.Release`, the same function the backend's `versionstamp.Normalise`
    uses. A test proves the two values are equal. The GCP fixtures were regenerated with their
    manifests unchanged. Their goldens change only by the new correlations and by observation
    instants shifted by the added events. No merge, ranking or node moved. The stamping guide gives
    `DD_VERSION=$K_REVISION` as Cloud Run's zero-effort recipe.

**Checkpoint**: the schema is extended, C9 fires and holds its apart-pair, the engine answers an
unstamped pointer, and two backends share one version-stamp rule. User stories may begin.

---

## Phase 3: User Story 5 — The log-search telemetry backend (Priority: P1) 🎯 MVP

**Goal**: all eight telemetry terms answered from Datadog logs and monitors, without tracing, as
bounded digests, identical live and recorded.

**Independent test**: `fixtures/datadog-backend-logs-01` replays every in-algebra request from its
world recording with the miss rate at or below the threshold, and zero APM or span calls.

- [X] T034 [US5] Write `internal/backends/datadog/transport.go`: the metered transport over T007's table and T021's quota reader, the configured site's API host, and `meta.status`/`meta.warnings` surfaced for `PARTIAL`; fixture: unit **Done in `internal/datadogx/` rather than the backend package**, because the feeder and the backend share one credential and one client, as GCP's halves share `internal/gcpx` (FR-001). `Do` refuses an unpublished operation before anything leaves the process (tested), keys travel as headers, the latest reading is kept per `X-RateLimit-Name` bucket, a non-2xx is a typed `StatusError` whose retry delay comes from the relative reset, and `Meta.Partial` reads `meta.status`/`meta.warnings`.
- [X] T035 [US5] Write `internal/backends/datadog/backend.go` and `describe.go`: the declaration of [contracts/datadog-telemetry-backend.md](./contracts/datadog-telemetry-backend.md) §1, cost classes and window caps of §2, registration against `pkg/backend`; fixture: unit
- [X] T036 [US5] Write `internal/backends/datadog/selector.go`: parse and validate a `datadog-logs/v1` selector (service and env pinned; no free text), refusing anything else as `UNSUPPORTED_POINTER` (FR-053); fixture: unit
- [X] T037 [US5] Write `internal/backends/datadog/logs.go`: paged search and aggregate calls, request canonicalisation for the evidence block, retention and lag detection ([contracts/datadog-telemetry-backend.md](./contracts/datadog-telemetry-backend.md) §4); fixture: datadog-backend-logs-01
- [X] T038 [US5] Implement `compare` in `internal/backends/datadog/compare.go` over aggregate counts per window, coverage stating log-derived counts (FR-040b); fixture: datadog-backend-logs-01
- [X] T039 [US5] Implement `onset` in `internal/backends/datadog/onset.go`: aggregate count timeseries, change point via `pkg/backend/onset`, no sample crossing the digest boundary; fixture: datadog-backend-logs-01
- [X] T040 [US5] Implement `new_log_patterns` in `internal/backends/datadog/logpatterns.go`: bounded paged sample, backend-side mining with variables masked, sample stated against the window's aggregate total; reuse 003's miner, lifting it into `pkg/backend` if it proves GCP-specific ([research.md §4](./research.md)); fixture: datadog-backend-logs-01
- [ ] T041 [US5] Close research §5 O1 before T042: record one read-only aggregate grouped by a faceted and by a non-faceted attribute, and by the `version` tag, as sanitised fixture payloads, and state the answer in [research.md](./research.md) §5; fixture: datadog-errors-by-version-01
- [ ] T042 [US5] Implement `errors_by_version` in `internal/backends/datadog/errors_by_version.go`: two aggregates (errors, total) grouped by the term's attribute with the `missing` sentinel, each group normalised via `versionstamp` into `deploy_ref` or its reason; the sampled fallback where O1 says the attribute is not groupable ([contracts/datadog-telemetry-backend.md](./contracts/datadog-telemetry-backend.md) §3.1); fixture: datadog-errors-by-version-01
- [X] T043 [US5] Implement `monitor_state` in `internal/backends/datadog/monitor_state.go` from `group_states`, marked sampled where derived from polls (FR-046); fixture: datadog-backend-logs-01
- [X] T044 [US5] Implement `error_spans` as `NO_DATA` naming "no span data source configured for this organisation" while `apm_topology` is off (FR-049b); fixture: datadog-backend-logs-01
- [X] T045 [US5] Implement `exemplars` (explicit request only, masked, capped) and `drill_down` (handles are references; deep links carry no credential) in `internal/backends/datadog/exemplars.go` and `drilldown.go` (FR-044a, FR-048c); fixture: datadog-backend-logs-01
- [X] T046 [US5] Write `internal/backends/datadog/coverage.go` and `failure.go`: every coverage field of FR-048a with undetermined where it cannot be known; outcomes per [contracts/datadog-telemetry-backend.md](./contracts/datadog-telemetry-backend.md) §4, a 429's retry from the relative reset; fixture: datadog-backend-logs-01
- [X] T047 [US5] Write `internal/backends/datadog/joinkeys.go`: version (raw stamp), host, first-seen instants, pseudonymised consistently when recorded (FR-048b); fixture: datadog-backend-logs-01
  - *Done notes (T035–T047):* the files are grouped by what they share rather than one per task —
    `backend.go` (declaration, T035), `selector.go`, `logs.go` (failure mapping, coverage, handles,
    deep links: T037, T046), `sample.go` (the bounded newest-first sample and its PARTIAL rule),
    `patterns.go` (`new_log_patterns`, `drill_down`, `exemplars`, join keys: T040, T045, T047),
    `compare.go`, `onset.go`, `errors_by_version.go`, `monitor_state.go`. The 003 miner is reused
    as is (`internal/investigation/workers/logs`); it is not GCP-specific, so nothing was lifted.
    `compare` over logs states COUNT, RATE and ERROR_RATE and refuses a latency percentile. A
    `datadog-monitor/v1` pointer carries its id in the published attribute `datadog.monitor.id`.
    T042 is written except its sampled fallback, which waits on T041 (O1). The recorded-mode
    identity over the twin is T048/T051.
- [X] T048 [US5] Build `fixtures/datadog-backend-logs-01`: a synthetic structural twin with a world recording over the full algebra cross product, and one event the graph must refuse (FR-078); fixture: datadog-backend-logs-01
- [X] T049 [P] [US5] Build `fixtures/datadog-errors-by-version-01`: a stamped service whose errors rise with a new commit, a group of abbreviated shas, and a group of lines with no stamp; goldens show `deploy_ref` per group; fixture: datadog-errors-by-version-01
- [X] T050 [P] [US5] Build `fixtures/datadog-unstamped-01`: the audit's shape — an SDK `@version` on start-up lines only — where the pointer carries no join key and `errors_by_version` answers the engine's `NO_DATA` listing each candidate's shares; fixture: datadog-unstamped-01
  - *Status (T048–T051):* both twins are generated by `internal/backends/datadog/fixture_*_test.go`
    under `SRE_AGENT_GEN_FIXTURES=1`. `world/` holds the cross product (21 answers; 7 for T049), and
    the package's tests replay it on every run against a client that must never be called, asserting
    live/recorded identity field for field, the story's facts, zero span or APM requests (SC-023) and
    the out-of-algebra refusal (SC-021). `fixture verify` covers the graph half and the refusal.
    **T050 waits on log-source discovery (Phase 5, T064–T065):** the engine's `NO_DATA` names every
    convention searched, but each candidate's shares come from the feeder's verdict, which nothing
    measures yet. The fixture is built when discovery is.
- [X] T051 [US5] Assert live/recorded digest identity including coverage and join keys over the recorded twin (SC-019), zero APM/span calls in the request log (SC-023), and every out-of-algebra request refused naming both lists (SC-021); fixture: datadog-backend-logs-01

**Checkpoint**: the backend answers the whole algebra from logs and monitors; MVP complete.

---

## Phase 4: User Story 2 — Monitors become alerts (Priority: P1)

**Goal**: every in-scope monitor an ALERT node; every transition an idempotent `alert.transition`,
dated by Datadog, polled with an optional doorbell.

**Independent test**: `fixtures/datadog-monitor-transitions-01` — OK → ALERT 14:32 → OK 15:10 gives
`[14:32, 15:10)`, history marked sampled, the recovery kept.

- [X] T052 [US2] Write `internal/feeders/datadog/monitors.go`: paged `GET /api/v1/monitor?group_states=all`, tag filter (FR-025c), definitions asserted as ALERT nodes on change only with the monitor query pointer and link ([data-model.md §2](./data-model.md)); fixture: datadog-monitor-transitions-01
- [X] T053 [US2] Derive transitions in `internal/feeders/datadog/transitions.go` by comparing group states with the previous poll, dated from `last_triggered_ts` / `last_resolved_ts` / `last_nodata_ts`, stated as undated in the checkpoint where none corresponds, never the poll instant ([contracts/datadog-feeder.md](./contracts/datadog-feeder.md) §3); fixture: datadog-monitor-transitions-01
- [X] T054 [US2] Emit through `pkg/feeder`'s `alert.transition` builder with the 4-tuple key, `sampled` and `sampled_interval_seconds`, flapping and no-data suppression stated, recoveries emitted; fixture: datadog-monitor-transitions-01
- [X] T055 [US2] Resolve watched entities from the monitor query and tags into WATCHES edges, unresolved ones recorded unattached for auto-attach (FR-019); fixture: datadog-monitor-transitions-01
- [X] T056 [US2] Retract a deleted monitor or one leaving scope, never on a partial read, with the gap declared (FR-012, FR-085); fixture: datadog-monitor-transitions-01
  - *Done notes (T052–T056, T059, T060, T062):* `feeder.go`, `monitors.go`, `transitions.go`. A
    transition is every stated instant newer than the newest already emitted for its group, so one that
    opened and closed between two polls is still delivered and a restart re-derives the same ids. A
    status change with no stated instant is **not emitted** and is stated in the checkpoint (the schema
    refuses a transition without its instant; the contract's "unknown start" line is corrected). A
    `discovery` payload asserts the watched log sources (T063 builds on it). The transitions fixture's
    shuffle step found an arrival-order bug in the projector, fixed here: a WATCHES edge attached
    after its target arrived was dated from the alert's earliest version of any kind — the monitor
    definition — instead of its first transition, as the direct path dates it
    (`internal/projector/attach.go`).
- [X] T057 [US2] Wire the doorbell (T024) into the feeder's poll loop so a ring enqueues "poll now" only; fixture: datadog-doorbell-forged-01
- [X] T058 [US2] Write `internal/cli/feed_datadog.go`: `aisre feed datadog` with `--site`, `--watch`, `--capabilities`, `--assert-read-only`, `--dry-run`, `--once`, and the startup gate of [contracts/read-only-operations.md](./contracts/read-only-operations.md) §3; fixture: unit
  - *Done notes (T057, T058, T061):* `poller.go` turns live reads into the same payloads a
    recording holds; a doorbell ring calls `Poller.PollNow`, non-blocking and coalescing, and the body
    is never read. `gate.go` is the startup gate: an unscoped application key carries every permission
    of its user and is **not** evidence of read-only, so it needs `--assert-read-only` like a key whose
    scopes Datadog will not state; the assertion is written into every checkpoint. `aisre feed
    datadog` refuses `--record` on a live run until the sanitised tee exists (T080). The permission
    names in contract §4 other than `logs_read_data` and `events_read` remain unconfirmed against
    Datadog's reference.
- [X] T059 [US2] Build `fixtures/datadog-monitor-transitions-01` (the 14:32/15:10 sequence, a flapping monitor, a no-data monitor, a monitor watching an unknown entity that later appears); fixture: datadog-monitor-transitions-01
- [X] T060 [P] [US2] Build `fixtures/datadog-grouped-monitor-01`: one monitor alerting on two groups at different instants gives two alerts and no monitor-level alert (SC-004); fixture: datadog-grouped-monitor-01
- [X] T061 [P] [US2] Build `fixtures/datadog-doorbell-forged-01`: forged, replayed and malformed rings cost at most one poll and write nothing; the same transition via doorbell and schedule is one event (SC-003, SC-020); fixture: datadog-doorbell-forged-01
- [X] T062 [US2] Assert SC-003's exactness (valid time equals Datadog's instant in 100 % of transitions) and SC-022 (every listed monitor is an ALERT within one poll, differences enumerated) over the fixtures; fixture: datadog-monitor-transitions-01

**Checkpoint**: an alert can be named by monitor id and handed to the engine with its pointers.

---

## Phase 5: User Story 4 — Pointers, and a node for every watched log source (Priority: P1)

**Goal**: every watched log source is a SERVICE node carrying a `datadog-logs/v1` pointer with its
version join key and discovery verdict, merged by C9 where another feeder reports the same service.

**Independent test**: ask for the pointers of a watched service as of two instants and compare to
goldens; a Cloud Run service with the same OTel name and environment shows both pointer sets.

- [X] T063 [US4] Write `internal/feeders/datadog/logsources.go`: for every configured `<env>/<service>`, assert the SERVICE node and its `datadog.log_service` claim every discovery interval, **never consulting the graph** (FR-040f); fixture: datadog-log-source-01
- [X] T064 [US4] Run version-stamp discovery per log source in `internal/feeders/datadog/discovery.go` via `versionstamp`, one bounded query per candidate, drawn from the budget and never inside an investigation ([contracts/version-stamping.md](./contracts/version-stamping.md) §3); fixture: datadog-log-source-01
- [X] T065 [US4] Mint the log pointer per [data-model.md §5](./data-model.md): selector pinning service and env, OTel attributes, `join_keys` version (spelled as tag or `@`attribute) and host, the verdict attached; unchanged verdict ⇒ unchanged event id; fixture: datadog-log-source-01
- [X] T066 [US4] Assert no selector contains an organisation, site host or credential (FR-037), pointers are versioned in valid time (FR-038), and Datadog pointers are additive on a C9-merged node (FR-039); fixture: datadog-log-source-01
- [X] T067 [US4] Build `fixtures/datadog-log-source-01`: one stamped source, one unstamped source, a verdict that changes between two windows, and a Cloud Run service C9 merges with; fixture: datadog-log-source-01
  - *Done notes (T063–T067, T050):* a `discovery` payload carries, per watched source, presence
    counts the live poller measured (`datadogx.Measurer`: all lines, error lines, lines with a host,
    and per candidate, each grouped by status — two calls plus one per candidate). The feeder decides
    the verdict with `versionstamp.Decide` and mints the pointer; the verdict's class rides on the node
    as `sre.version_stamp.*` (the Pointer message has no field for it), its shares in the checkpoint,
    so an unchanged verdict is an unchanged id. `datadog-log-source-01` holds the stamped, unstamped and
    changing sources and the C9 merge with Cloud Run; its unstamped `search` is also T050's audit shape
    — `TestTheUnstampedPointerIsAnsweredByTheEngine` runs errors_by_version on the pointer as recorded
    and gets the engine's NO_DATA, so no separate `datadog-unstamped-01` was built. The discovery reads
    do not yet draw on a quota budget (the Datadog feeder has none yet; FR-082 is Phase 7's).
  - *T068 deferred:* SC-016's path needs the owner (tags, T076) and the preceding changes (log-observed
    rollouts, Phase 6); it is asserted when those exist rather than against a graph that cannot answer.
- [X] T068 [US4] Assert SC-016's path end to end on the recorded corpus: monitor id → alert, watched entities, preceding changes, owner, executable pointers, in one command under 30 s; fixture: datadog-log-source-01
  - *T068 notes:* the corpus is its own fixture, `datadog-monitor-to-owner-01`, and not
    datadog-log-source-01, which has no monitor, owner or rollout. `TestSC016FromADatadogMonitorIDAlone`
    runs `query diff datadog.monitor=<id> --at <trigger> --hops 2`. The answer names the alert, the
    WATCHES edge, the log-observed rollout, the owner and the log pointer. The table output now prints
    the pointers of the changed entities, which it did not before. Found on the way: a transition, an
    assertion from the same source, hid the monitor's definition properties and pointers from the
    transition onwards. On the owner's decision, the projector now folds a transition in its source's
    alert lane, beside the definition (internal/projector/alert_transition.go); five alert fixtures'
    goldens gained the properties and pointers back.

**Checkpoint**: a service on any platform — including one no feeder covers — is investigable.

---

## Phase 6: Log-observed rollouts (FR-040g; completes User Story 5 scenario 9)

**Goal**: a new stamped version seen in a watched service's logs is a ROLLOUT, dated from first sight
as a bound, merged by C9 then C8 with a deploy feeder's rollout of the same commit.

**Independent test**: `fixtures/datadog-log-rollout-merge-01` shows **one** rollout in the ranked
change list, carrying the stated instant.

- [X] T069 [US5] Write `internal/feeders/datadog/rollouts.go` per [contracts/datadog-feeder.md](./contracts/datadog-feeder.md) §4: values per interval, first indexed line within the published 7-day horizon, id from value and first-seen instant only, `deploy.*` correlation key with `deployment.environment.name`, `valid_from_is_a_bound`, actor kind unspecified; fixture: datadog-log-rollout-01
- [X] T070 [US5] Assert release-form and unreferenceable values emit no change and are counted; alternation emits once per value with the overlap recorded; a value first seen beyond the horizon emits nothing; fixture: datadog-log-rollout-01
- [X] T071 [US5] Assert restart safety: two feeder runs over the same recorded interval emit identical ids, with no process state carried between them; fixture: datadog-log-rollout-01
- [X] T072 [US5] Build `fixtures/datadog-log-rollout-01`: a service on a platform no feeder covers deploys twice; each rollout stands alone, marked as a bound; fixture: datadog-log-rollout-01
- [X] T073 [US5] Build `fixtures/datadog-log-rollout-merge-01`: the same commit deployed by Cloud Run (stated instant) and first seen in Datadog logs later; C9 then C8 give one rollout; the pair with the same commit in another environment stays apart; fixture: datadog-log-rollout-merge-01
- [X] T074 [US5] Close research §5 O3: establish which valid start the merged change carries; if it is the bound, fix the merge so the stated instant wins while the bound stays visible, and record it; fixture: datadog-log-rollout-merge-01
- [X] T075 [US5] Assert US5 scenario 9: the new version's `errors_by_version` group names the same `deploy.commit_sha` the merged change claims, and the engine resolves it to that change; fixture: datadog-log-rollout-merge-01

  - *Done notes (T069–T075):* the live poller lists, for a stamped source, the stamp's new values and
    the instant of each one's first indexed line within the seven-day horizon (one aggregate, then two
    searches per new value: the first line, and whether any line precedes the horizon); the values ride
    on the discovery payload, and `rollouts.go` turns each commit or image into a ROLLOUT whose id is
    the value and that instant only. Its actor is left **unspecified**, not UNKNOWN: the logs name no
    actor, and UNKNOWN would claim one was observed (data-model §6 said UNKNOWN; corrected here). The
    overlap of a canary's values is stated in the checkpoint rather than on the changes: a change
    emitted at first sight cannot later gain a property without a second event under the same id.
    O3 is closed in research §5: the merged change carries the stated instant. T075's engine half is
    `internal/investigation/engine/versionchange.go`: an errors_by_version answer names, under each
    group, the ranked change whose sources stated the same `deploy.*` reference.

**Checkpoint**: "did errors start with this deploy?" is answered the same way whatever deployed it.

---

## Phase 7: User Story 6 — Tags become owners and identity claims (Priority: P2)

**Independent test**: replay tag payloads; owners, `owned-by` edges and claims match goldens; a key off
the allowlist becomes nothing.

- [X] T076 [US6] Write `internal/feeders/datadog/tags.go`: the published allowlist, team/owner tags to OWNER nodes and `owned-by` edges with unknown valid start (FR-065, FR-066); fixture: datadog-tags-01
- [X] T077 [US6] Emit identity claims for allowlisted identifier tags, never measurements (FR-067); keep both claims where two keys name one owner differently (FR-068); fixture: datadog-tags-01
- [X] T078 [US6] Build `fixtures/datadog-tags-01` including an off-allowlist key and a numeric tag that must not become a property; fixture: datadog-tags-01

- *Done notes (T076–T078):* the tags read are the ones on a watched log source's own lines, measured
  at each discovery tick (one aggregate per allowlisted key: `team`, `owner`, and the
  `kube_namespace`+`kube_deployment` pair). A value is the source's only on the published line share;
  the rest is stated in the checkpoint. Owners are OWNER nodes and `owned-by` edges from the log
  source, valid from unknown like the source itself; the pair is an identity claim
  `k8s.deployment=<ns>/<name>`. Monitor tags are not read for ownership: a monitor's owner is its
  watched service's. `IsMeasurement` moved from the GCP feeder to `pkg/feeder`, shared.

---

## Phase 8: User Story 7 — The organisation's payloads, sanitised, are the tests (Priority: P2)

**Independent test**: the public twins pass the conformance suite with the private corpus absent; a
seeded canary fails the commit.

- [X] T079 [US7] Build the Datadog sanitisation table with `sanitise.NewPolicy` in `internal/sanitise/datadog.go`: people identifiers dropped, infrastructure identifiers keyed-HMAC, monitor message bodies dropped, log lines as masked templates, unassigned fields failing closed (FR-072–FR-075); fixture: unit
- [X] T080 [US7] Wire the table into the backend (live redaction too, FR-052) and into a recording tee for the feeder, so no unsanitised byte reaches disk even on an aborted run (FR-076a); fixture: unit
- [X] T081 [US7] Assert over every Datadog fixture: canaries absent, the independent scan clean, zero people identifiers in any form (SC-017, SC-018); fixture: datadog-backend-logs-01
  - *Done notes (T080, T081):* the backend already refused a live run without its sanitiser and
    redactor (Phase 3). The feeder's live `--record` goes through `internal/feeders/deployrecord`'s tee
    with a Datadog pre-pass (`recording.go`): it keeps only the fields the feeder reads — a monitor's
    creator, options, message, roles and anything Datadog adds later never reach the recording —
    reduces a monitor's query to its identifier terms (a metric name or a search phrase is free text no
    pass can vouch for; the pointer executes by id), and rewrites every identifier term in queries,
    tags, group keys, sources and tag values with the corpus key's pseudonym, so the recording still
    joins. The `datadog.*` rows in `sanitise.ContractPolicy` decide every field after that, failing
    closed. The recording's events are derived by a shadow feeder in the recording's vocabulary. Tests:
    canaries (person, free text; infrastructure where the pre-pass hashes it) never survive, a canary
    in a field the table would hash ends the run, and every committed Datadog fixture carries no
    people identifier and no canary token.
- [ ] T082 [US7] Run the campaign against the organisation per quickstart §8: record, scan, sign, verify, parity — **blocked on a read-only key and a named signatory**; fixture: private corpus
- [ ] T083 [US7] Commit the signed private corpus and run the same suite in private CI; record the live parity result (SC-009, SC-010) — **blocked on T082**; fixture: private corpus

---

## Phase 9: User Story 8 — The connector stays inside the budget (Priority: P2)

**Independent test**: replay a recording with 429s; back-off honoured, usage matches the recording to
the call, work deferred in the published order.

- [X] T084 [US8] Implement the budget in `internal/feeders/datadog/budget.go` and the backend's equivalent: share of remaining quota per `X-RateLimit-Name` bucket, the human reserve untouched, window caps per term (FR-081–FR-082a); fixture: datadog-rate-limited-01
- [X] T085 [US8] Implement the published deferral order (transitions, definitions, discovery, rollouts) with the typed quota-stop reason, and resume-after-429 (FR-082, FR-083); fixture: datadog-rate-limited-01
- [X] T086 [US8] Implement the usage report: calls per area and bucket, share of remaining quota, reserve untouched, feeder and backend separately (FR-084, FR-084a); fixture: datadog-rate-limited-01
- [X] T087 [US8] Build `fixtures/datadog-rate-limited-01` with 429s, falling remaining quota during an incident, and a partial poll; assert SC-008 to the exact call; fixture: datadog-rate-limited-01
- [X] T088 [P] [US8] Document the cost at the default cadence for a stated estate in `docs/connectors/datadog.md` (FR-087), from the recorded run, not an estimate; fixture: datadog-rate-limited-01
  - *T084–T088 notes:* the budget is `internal/datadogx/budget.go` over pkg/feeder's QuotaBudget, keyed by
    the bucket Datadog names; areas and typed stops are `internal/feeders/datadog/areas.go`. The backend
    maps a budget stop to RATE_LIMITED, never a timeout. The budget now forgets a reading once its window
    has reset (`QuotaBudget.Expire`); without it, a 429's "nothing left" would have refused every later
    call. A partial poll now claims no coverage, so the gap it declares never ends before it starts.
    SC-008 is asserted by `TestTheBudgetHoldsToTheExactCall`.

---

## Phase 10: User Story 1 — APM topology (Priority: P3, off by default, **deferrable**)

Built only when an organisation with tracing needs it. Until then SC-001/SC-002 report not
applicable and SC-024 is asserted instead.

- [ ] T089 [US1] Assert SC-024 now, before any APM code exists: with `apm_topology` off, zero APM scopes, calls and derived events, the checkpoint names the capability as disabled; fixture: datadog-backend-logs-01
- [ ] T090 [US1] Register the APM metric and span vocabularies, declare the APM operations under the capability, and implement section B (FR-009–FR-017) with its fixture and the on/off golden identity of every other capability — **deferred until needed**; fixture: datadog-apm-topology-01

---

## Phase 11: User Story 3 — Datadog's event stream as a change source (Priority: P3, off by default, **deferrable**)

- [ ] T091 [US3] Assert the `changes` capability is silent and stated while off, as T089; fixture: datadog-backend-logs-01
- [ ] T092 [US3] Implement section D (FR-027–FR-033b) with `events_read` declared only under the capability, claims through the published `deploy.*` keys, and a fixture where a Datadog deploy event and a Kubernetes rollout appear once (SC-014) — **deferred until needed**; fixture: datadog-events-merge-01

---

## Phase 12: Polish & cross-cutting concerns

- [X] T093 Write `docs/connectors/version-stamping.md` per [contracts/version-stamping.md](./contracts/version-stamping.md) §7: one recipe per deployment type, the two warnings, and how to read the connector's verdict (FR-040e); fixture: unit
- [ ] T094 Close research §5 O4: check the organisation's voice-agent platform's own documentation for a runtime commit variable, and add it to the guide's vendor-hosted section if it exists, else keep the build-time recipe; fixture: unit
- [X] T095 [P] Complete `docs/connectors/datadog.md`: capabilities, the gate and what it cannot prove (O2), the operation list (now enforced), cost; fixture: unit
- [X] T096 [P] Update `docs/schema/digests.md` (or the page that documents `VersionBreakdown`) for `deploy_ref`; fixture: unit
- [X] T097 Run the full local gate: `make gen build test lint verify`, `go test -race ./...`, `buf lint`, `buf breaking`, `check-no-secrets.sh` and self-test, `check-specs.sh`, `check-migrations.sh`, `fixture verify --report fixtures/*/` and `check-report.sh`; fixture: every datadog-* fixture
  - *T097 notes, 2026-09-27:* all checks pass:
    - `make generate-check` (the generated code is up to date);
    - `buf lint`, and `buf breaking` against main;
    - `go test -race ./...`;
    - golangci-lint;
    - `check-no-secrets.sh` and its self-test;
    - `check-specs.sh` and `check-migrations.sh`;
    - `fixture verify --report` on 79 fixtures, and `check-report.sh` (every assertion held).
- [X] T098 Reproduce [quickstart.md](./quickstart.md) §0–§6 on a clean worktree and record the run as `quickstart-run-<date>.md`, fixing every doc and code drift it exposes; fixture: datadog-backend-logs-01
  - *T098 notes:* run on 2026-09-27 and recorded in
    [quickstart-run-2026-09-27.md](./quickstart-run-2026-09-27.md). §0–§6 pass. It found four places
    where the file had drifted: two test patterns that matched nothing or too little, a fixture that
    was never built, and fixtures added later that the file did not name. All four are fixed in the
    quickstart.
- [ ] T099 With a read-only key: run quickstart §7 (`--dry-run`, then `--once` against the organisation), record the startup gate's verdict (O2) and the first live version-stamp verdict — **blocked on the key**; fixture: private corpus

---

## Dependencies & execution order

- **Phase 1** has no dependencies. T003 (ADR-0010) precedes every schema task in Phase 2.
- **Phase 2** blocks every story. Inside it: 2A before any Datadog call; 2B before 2H's T033 and
  Phase 3; 2D's C9 before Phase 5's merge assertions and Phase 6; 2H before Phases 3 and 5.
- **Phase 3 (US5)**, **Phase 4 (US2)** and **Phase 5 (US4)** depend only on Phase 2 and can run in
  parallel; T041 (O1) precedes T042.
- **Phase 6** depends on Phases 3 and 5 (the version groups and the log-source nodes).
- **Phases 7–9** depend on Phase 2 and the transports of Phases 3–4; T082–T083 and T099 are blocked
  on the organisation.
- **Phases 10–11** are deferrable and depend on nothing after Phase 2 except T089/T091.
- **Phase 12** depends on every story being built.

## Parallel opportunities

- Phase 1: T001–T003 in parallel.
- Phase 2: 2A, 2B, 2E, 2F and 2G are independent of each other; 2C follows 2B; 2D and 2H each stand
  alone once T014 exists.
- Phases 3, 4 and 5 in parallel after Phase 2; fixture builds marked [P] within each.

## Implementation strategy

**MVP is Phases 1–3**: the six gaps closed, version stamps generic across two backends, and the Datadog
backend answering the whole algebra. For the organisation today that is the valuable half: its one
Datadog service is a log source, and the investigation engine can question it on the first day.

Then Phases 4 and 5 (monitors, and a node for every log source), then Phase 6 — which is what makes a
service on an unfed platform show its deploys — then the P2 stories, the guide and the quickstart run.
Phases 10 and 11 wait for an organisation that needs them.

## What is blocked on the organisation

| task | needs |
|---|---|
| T041 | a read-only key, for one aggregate call per case (or the owner's approval to run it through the read-only Datadog tools as on 2026-09-27) |
| T082, T083 | a read-only Datadog key and a **named human signatory** on the manifest (FR-076) |
| T099 | the read-only key |
| — | the version stamp on the organisation's own service, per `docs/connectors/version-stamping.md` — until then its verdict reads "no stamp", which is the correct answer |

Everything else is buildable now.
