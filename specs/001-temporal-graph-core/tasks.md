---

description: "Task list for feature 001 — Temporal System Graph Core"
---

# Tasks: Temporal System Graph Core

**Input**: Design documents from `/specs/001-temporal-graph-core/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Required. Constitution VIII makes fixtures and tests non-optional; every task below
names the fixture(s) it validates against, or `bench` for the reference workload, or `unit`
when a fixture is not the right instrument.

**Organization**: Setup → Foundational → one phase per user story in spec priority order →
Polish. Each story phase ends at a checkpoint where `aisre fixture verify` on the named
fixtures passes.

## Format: `[ID] [P?] [Story] Description → FR refs; fixture: <ids>`

- **[P]**: parallelizable (different files, no unmet dependency)
- **[USn]**: user story from spec.md
- Fixture ids refer to directories under `fixtures/` (contracts/fixture-format.md)

## Fixture inventory (created progressively, all present by Phase 9)

| id | family | created in | exercises |
|---|---|---|---|
| `baseline-topology-01` | baseline-topology | T024 (hand-authored events), payloads added T058/T063 | subgraph, impact, pointers, extent |
| `rollout-regression-01` | rollout-regression | T043 | diff + ranking, culprit top-3 |
| `config-change-01` | config-change | T066 | K8s config change nodes, diff |
| `late-arriving-fact-01` | late-arriving-fact | T029 | observed-time pinning |
| `ambiguous-identity-01` | ambiguous-identity | T077 | probable rules, human confirm, audit, replay precedence |
| `retraction-with-edges-01` | retraction-with-edges | T030 | retract cascade |
| `feeder-gap-01` | feeder-gap | T067 | checkpoints, gaps, extent |
| `rollout-regression-02` | rollout-regression (recorded) | T082 | ranking on real payloads |
| `bench` | reference workload | T091 | SC-001, SC-002, replay throughput |

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: repository skeleton, toolchain, CI, licensing

- [x] T001 Create repo layout from plan.md: `cmd/aisre/`, `api/sreagent/graph/v1/`, `internal/{log,projector,graph,query,resolution,store/postgres,server,feeders/otel,feeders/k8s,fixture,telemetry,cli}`, `pkg/feeder/`, `fixtures/`, `deploy/`, `docs/{schema,connectors,benchmarks}`, `.github/workflows/` with placeholder `doc.go` files → plan §Project Structure; fixture: n/a
- [x] T002 Initialize Go module `go.mod` (module path per ADR-0002, TODO-org placeholder), `Makefile` (`build`, `test`, `lint`, `gen`, `verify`), `.golangci.yml`, `.editorconfig` → plan §Technical Context; fixture: n/a
- [x] T003 [P] Add `LICENSE` (Apache-2.0), `NOTICE`, SPDX header lint rule in `.golangci.yml`, `CONTRIBUTING.md` (DCO, fixture-or-no-merge rule), `CODEOWNERS`, `SECURITY.md` → constitution VIII/IX, plan §CI; fixture: n/a
- [x] T004 [P] Copy `specs/001-temporal-graph-core/contracts/graph.proto` to `api/sreagent/graph/v1/graph.proto`; add `buf.yaml`, `buf.gen.yaml` (go + connect-go + doc generators), `make gen` target; commit generated code under `api/sreagent/graph/v1/` → FR-001, FR-002, FR-005, FR-006, FR-053; fixture: n/a
- [x] T005 [P] Add `deploy/docker-compose.yml` (Postgres 16 with `PG_DSN` env, healthcheck) and `deploy/README.md` → quickstart §1; fixture: n/a
- [x] T006 [P] Add `.github/workflows/ci.yml`: buf lint + breaking (against main), golangci-lint, `go test ./...` with Postgres service container, `govulncheck`; jobs fail on any red → plan §CI; fixture: all (later)
- [x] T007 [P] Add `docs/schema/README.md` rendering pipeline (buf doc → markdown) and `docs/schema/temporal-model.md` explaining valid/observed semantics, unknown flags, correction vs retraction (from spec §B, research §3) → FR-053, constitution IX; fixture: n/a

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: storage, log, projector core, validation, auth, server, CLI root, fixture harness, first fixture. Nothing story-specific here, but US1–US7 all depend on it.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

### Storage and domain types

- [x] T008 Implement `internal/graph/interval.go`: `Interval` type with half-open semantics, unknown flags, `Contains`, `Overlaps`, `Split`, canonical JSON; unit tests in `interval_test.go` incl. property tests with `rapid` → FR-010, FR-011; fixture: unit
- [x] T009 [P] Implement `internal/graph/ids.go`: deterministic `EntityID(ns, value)`, `VersionID(entityID, eventID)`, `EdgeVersionID(src,dst,type,eventID)`, `ClaimID`; golden unit tests pin exact strings → research §4; fixture: unit
- [x] T010 [P] Implement `internal/graph/canonical.go`: canonical JSON serializer (sorted keys, RFC 3339 nanos UTC, ordered arrays) for protobuf messages and fixture files; unit tests → contracts/fixture-format.md §Canonical; fixture: unit
- [x] T011 Write migrations in `internal/store/postgres/migrations/0001_log.sql` (schemas `log`: sources, events, rejected_events, duplicate_deliveries, checkpoints; grants INSERT/SELECT only; partition-ready by observed_at) and `0002_graph.sql` (entities, entity_versions, edge_versions with exclusion constraints and GiST indexes, identity_claims, resolution_decisions, suggestions, principals; `close_observed()` SECURITY DEFINER function) → FR-005, FR-006, FR-016 (partition-ready, no horizon), data-model.md, research §14; fixture: unit (migration up/down test)
- [x] T012 Implement `internal/store/postgres/store.go`: pool setup, `Migrate()`, `tstzrange` codec for `Interval`, tx helper; `internal/store/postgres/testing.go` with testcontainers + embedded-postgres fallback; smoke test → plan §Storage; fixture: unit
- [x] T013 Add CI guard `scripts/check-migrations.sh` refusing `DROP`/`DELETE`/`TRUNCATE` on `log.*` or `graph.*_versions` in migration files; wire into `ci.yml` → constitution III, research §14; fixture: n/a

### Event log and validation

- [x] T014 Implement `internal/log/validate.go`: schema-version check, typed-body check, missing valid-time, telemetry-payload denylist (keys, numeric arrays, >4 KiB props), secret-value check, unknown type; returns `reason_code`/`detail`; table-driven tests incl. one rejected sample per reason → FR-009, FR-024; fixture: unit
- [x] T015 Implement `internal/log/append.go`: `Append(ctx, env) (IngestResult)` assigning `observed_at`, idempotency-key unique handling → `DUPLICATE_NOOP` + `duplicate_deliveries` row when payload differs, `rejected_events` on validation failure, source registration check; tests against Postgres → FR-018, FR-019, FR-020, FR-024; fixture: unit
- [x] T016 [P] Implement `internal/log/read.go`: iterate events by `appended_seq` (replay order), by source, by observed range; `Extent()` (earliest/latest observed, per-source checkpoints, gaps) → FR-023, FR-052; fixture: unit

### Projector core

- [x] T017 Implement `internal/projector/refs.go`: resolve `Ref` → `entity_id` via `identity_claims` (following `merged_into`), create entity + primary claim on first sight; certain rules C1, C2, C3 (research §10) applied here via a minimal `internal/resolution` rule registry (certain rules only; probable rules arrive in T072), merged type by precedence + `facets` (ADR-0001 D7) → FR-036, FR-037 (certain rules); fixture: unit
- [x] T018 Implement `internal/projector/upsert_node.go`: version insertion with valid-range splitting/closing of overlapping versions, per-key prop provenance and `conflicts`, no-op when nothing differs, `produced_by_event_ids` (SC-006 traceability); OTel-conventional prop key validation (FR-007); order-independence property test (apply permutations, compare valid-time state) → FR-007, FR-012, FR-014, FR-021, edge case "conflicting sources"; fixture: unit
- [x] T019 [P] Implement `internal/projector/upsert_edge.go` (same semantics, `weight_class`), `retract_node.go` (close valid, cascade to edges with `closed_as_consequence_of`), `retract_edge.go` → FR-013, edge case "retracting a node with live edges"; fixture: unit
- [x] T020 [P] Implement `internal/projector/observe_change.go`: change entity + `changed-by` edges, unattached when target unresolved, `attach.go` re-attaching on later target appearance → FR-003, FR-004, edge case "dangling change"; fixture: unit
- [x] T021 Implement `internal/projector/apply.go`: one event per transaction, dispatch by type, `source_checkpoint` → `log.checkpoints`, idempotent re-apply is a no-op; `Replay(ctx)` iterating the log with recorded `observed_at` → FR-022, FR-023; fixture: unit (until T024)

### Auth, server, telemetry, CLI root

- [x] T022 [P] Implement `internal/server/auth.go`: OIDC verifier (issuer, audience, role claim → `reader`/`decider`/`feeder`), dev provider (`--auth dev --dev`, signed local tokens, banner), principal extraction; reject anonymous; tests with a fake issuer → FR-041, FR-041a, research §7; fixture: unit
- [x] T023 [P] Implement `internal/telemetry/otel.go`: OTel SDK setup (OTLP exporter, resource), metric instruments listed in plan §Observability, `slog` with trace correlation → FR-051; fixture: unit
- [x] T024 Hand-author `fixtures/baseline-topology-01/{manifest.yaml,events.jsonl}` (services checkout, payments, storefront, payments-db, workloads, owners, pointers; valid from 2026-09-01T13:00Z) — no goldens yet → FR-047; fixture: baseline-topology-01
- [x] T025 Implement `internal/server/ingest.go`: `IngestService` (streaming + batch + RegisterSource) wired to log append + projector apply, `feeder` role required and `source_id` scoped; `internal/server/server.go` ConnectRPC mux, healthz, auth interceptor; test: ingest `baseline-topology-01/events.jsonl` via gRPC and via JSON → FR-017, FR-035; fixture: baseline-topology-01
- [x] T026 Implement `cmd/aisre/main.go` + `internal/cli/root.go` (cobra, `--output`, `--server`, token loading), `serve`, `migrate`, `dev-token`, `extent` commands → contracts/cli.md; fixture: baseline-topology-01 (`extent` after load)
- [x] T027 Implement `internal/fixture/load.go` and `cli/fixture_load.go`: apply `events.jsonl` with recorded observed times through IngestService → FR-047; fixture: baseline-topology-01
- [x] T028 Implement `internal/fixture/verify.go` skeleton and `cli/fixture_verify.go`: fresh DB → load → run manifest queries (query dispatch table filled by later phases) → byte-compare goldens; double-delivery pass asserting all `DUPLICATE_NOOP`; shuffle pass within `reordering_window` comparing structurally up to entity-id relabeling (alias-set matching) → FR-048, research §4; fixture: baseline-topology-01 (no goldens yet → verify reports "0 queries")
- [x] T029 [P] Hand-author `fixtures/late-arriving-fact-01` (edge learned at 15:00 about 14:20) → FR-015, US1 scenario 4; fixture: late-arriving-fact-01
- [x] T030 [P] Hand-author `fixtures/retraction-with-edges-01` (node retracted at 14:10 with two live edges) → FR-013 cascade; fixture: retraction-with-edges-01

**Checkpoint**: `make test` green; `fixture load` + `fixture verify --skip-goldens` pass on the three hand-authored fixtures (replay idempotent, shuffle structural equality holds).

---

## Phase 3: User Story 1 — See the system as it was at the moment of the alert (P1) 🎯 MVP

**Goal**: `Subgraph` query as-of (valid + observed), N hops, direction, edge filters, caps, truncation; CLI `query subgraph`.

**Independent Test**: `fixture verify fixtures/baseline-topology-01 fixtures/late-arriving-fact-01 fixtures/retraction-with-edges-01` passes with subgraph goldens.

- [x] T031 [US1] Implement `internal/query/slice.go`: as-of adjacency slice loader (`valid @> $t AND observed @> $o`, frontier by ids, per-hop and total caps, truncation record) → FR-026, research §2; fixture: baseline-topology-01
- [x] T032 [US1] Implement `internal/query/subgraph.go`: BFS over slices with direction and edge-type/min-weight filters, node version hydration (props with provenance, pointers, aliases), deterministic ordering; unit tests for hub-cap truncation → FR-026, edge case "hub explosion"; fixture: baseline-topology-01
- [x] T033 [US1] Implement `internal/server/query_subgraph.go` (`QueryService.Subgraph`, `reader` role) and `internal/cli/query_subgraph.go` (table + json output) → FR-035; fixture: baseline-topology-01
- [x] T034 [US1] Add manifest queries + record goldens: `baseline-topology-01` (checkout 2-hop at 14:32; 3-hop hitting the shared DB with cap 5 to exercise truncation), `late-arriving-fact-01` (14:32 observed now vs observed 14:32), `retraction-with-edges-01` (14:00 vs 14:32); wire into `fixture verify` dispatch → US1 scenarios 1–4; fixture: baseline-topology-01, late-arriving-fact-01, retraction-with-edges-01
- [x] T035 [P] [US1] Edge case: as-of before recorded history returns empty + `before_recorded_history` flag; add golden query to `baseline-topology-01` (as-of 2026-08-01) → edge case "query before history"; fixture: baseline-topology-01
- [x] T036 [P] [US1] Add `docs/schema/queries.md` §Subgraph documenting parameters, caps defaults (50/hop, 500 total), truncation semantics → FR-053; fixture: n/a

**Checkpoint**: US1 fixtures green in CI; quickstart §2 first command and §3 both reproduce.

---

## Phase 4: User Story 2 — See what changed, ranked (P1)

**Goal**: `Diff` between T1 and T2 with node/edge deltas and ranked change list with exposed score components; CLI `query diff`.

**Independent Test**: `fixture verify fixtures/rollout-regression-01 --report` shows culprit rank 1 and all goldens match.

- [x] T037 [US2] Implement `internal/query/diff.go`: two slices (T1, T2 at same observed), set difference on version identity, `PropertyDelta` per changed key, edge deltas → FR-027; fixture: rollout-regression-01
- [x] T038 [US2] Implement `internal/query/changes.go`: change nodes whose valid range intersects `(T1,T2]` with targets in subgraph ± `change_hop_margin`; include unattached → FR-027, FR-028; fixture: rollout-regression-01
- [x] T039 [US2] Implement `internal/query/rank.go`: published score (research §9: temporal/topological/traffic, τ default 1800 s), components on each item, tie-break by change id, `ranking_formula` string; unit tests with hand-computed expectations → FR-028; fixture: unit + rollout-regression-01
- [x] T040 [US2] Implement `internal/server/query_diff.go` and `internal/cli/query_diff.go` (`--from --to --reference --tau --hops`) → FR-035; fixture: rollout-regression-01
- [x] T041 [P] [US2] Implement `internal/fixture/report.go`: for `rollout-regression` family, rank of `ground_truth.culprit_change`, top-k hit; `--report` output as markdown + JSON → SC-005, constitution V; fixture: rollout-regression-01
- [x] T042 [P] [US2] Edge case tests: two changes with equal time and hop distance → deterministic order and `tie_break` populated; unattached change ranked below attached with same time → FR-028; fixture: rollout-regression-01
- [x] T043 [US2] Hand-author `fixtures/rollout-regression-01` (payments rollout 14:20, decoys 13:10/13:40/14:25 on unrelated services, one unattached change) with manifest `ground_truth`; record diff goldens → US2 scenarios 1–4; fixture: rollout-regression-01
- [x] T044 [P] [US2] Add `docs/schema/ranking.md` publishing the formula, defaults and tie-break → FR-028, FR-053; fixture: n/a

**Checkpoint**: quickstart §2 diff command reproduces; `--report` shows culprit rank 1.

---

## Phase 5: User Story 3 — Write a feeder and prove it with recorded payloads (P1)

**Goal**: public `pkg/feeder` SDK with recorder + testkit; OpenTelemetry topology feeder; Kubernetes feeder; recorded payloads for the baseline fixture.

**Independent Test**: `go test ./internal/feeders/...` passes via testkit against `baseline-topology-01/payloads`; `feed k8s --replay ... --dry-run` reproduces `events.jsonl` for its source.

### SDK

- [x] T045 [US3] Implement `pkg/feeder/feeder.go`: `Feeder`, `Description`, `Source`, `Payload`, `Emitter` interfaces; `pkg/feeder/ids.go` (`NewID`), `ref.go`, `props.go` (typed setters, denylist check), `weight.go` (`WeightClass(rps)` per research §8) with unit tests → FR-045, contracts/feeder-sdk.md; fixture: unit
- [x] T046 [P] [US3] Implement `pkg/feeder/emit/connect.go`: `Emitter` over `IngestService` (batching, retries, feeder token) and `emit/memory.go` (in-memory for tests) → FR-017; fixture: unit
- [x] T047 [P] [US3] Implement `pkg/feeder/record/`: `Wrap(Source, dir)` payload tee, `Emitter(em, dir)` events tee with server `observed_at` → FR-044, FR-049; fixture: unit
- [x] T048 [US3] Implement `pkg/feeder/testkit/`: `Run`, `Shuffle`, `DoubleDeliver` per contracts/feeder-sdk.md; documented in `docs/connectors/writing-a-feeder.md` → FR-045, FR-049, SC-008; fixture: baseline-topology-01 (once T058/T063 land)
- [x] T049 [P] [US3] Implement `pkg/feeder/source/file.go`: recorded-payload `Source` reading `payloads/<kind>/*.jsonl|*.pb` in `Seq` order → FR-044; fixture: unit

### OpenTelemetry topology feeder

- [x] T050 [US3] Implement `internal/feeders/otel/receiver.go`: OTLP/gRPC and OTLP/HTTP (protobuf + JSON) trace endpoints using `go.opentelemetry.io/proto/otlp`; each `ExportTraceServiceRequest` becomes a `Payload` → FR-042, research §11; fixture: unit
- [x] T051 [US3] Implement `internal/feeders/otel/aggregate.go`: per-window (default 5 m) derivation of services (name, namespace, version, environment), callee edges (`peer.service`, `db.system`+`server.address`, HTTP host → third-party nodes), counts → weight class with hysteresis; spans dropped at window end; unit tests on synthetic spans → FR-042, research §8; fixture: unit
- [x] T052 [US3] Implement `internal/feeders/otel/emit.go`: `UpsertNode`, `UpsertEdge`, `IdentityClaim` (`otel.service.name`, `k8s.deployment.name`+namespace when present), `ObserveChange(rollout)` on `service.version` transition, `RetractEdge` after `--retract-after` windows, pointers (trace/metric/log selectors in OTel vocabulary), `SourceCheckpoint` per window → FR-042, FR-008; fixture: baseline-topology-01
- [x] T053 [US3] Implement `internal/feeders/otel/feeder.go` (`Feeder` impl, `Describe`: ordering none, window 300 s) and `internal/cli/feed_otel.go` (`--record`, `--replay`, `--dry-run`) → FR-044; fixture: baseline-topology-01
- [x] T054 [P] [US3] Negative test: feeder attempting to emit a span attribute map as props is rejected by `Emitter` validation with `telemetry_payload`; add the rejected event to `baseline-topology-01/manifest.yaml` `expect_rejected` and to `fixture verify` → US3 scenario 4, FR-009, SC-009; fixture: unit

### Kubernetes feeder

- [x] T055 [US3] Implement `internal/feeders/k8s/informers.go`: client-go shared informers for Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, Services, Ingresses, ConfigMaps, Secrets (metadata only), Nodes, Namespaces; watch events → `Payload` with `resourceVersion` as `Seq` → FR-043, research §12; fixture: unit (fake clientset)
- [x] T056 [US3] Implement `internal/feeders/k8s/permissions.go`: `SelfSubjectRulesReview` at start; refuse if any write verb on watched resources → FR-046, constitution VII; fixture: unit
- [x] T057 [US3] Implement `internal/feeders/k8s/map_workloads.go`: workload nodes (`k8s.*` conventions), `runs-on` → cluster / node pool nodes, `exposed-via` → Service/Ingress nodes, `owned-by` → owner nodes from `--owner-labels`, `depends-on` → config nodes (ConfigMap value hash, Secret version only), identity claims (`k8s.deployment`, `app.kubernetes.io/name`, OTel annotation), pointers (`k8s` source links, log selector by labels) → FR-043, FR-008, edge case "secrets"; fixture: baseline-topology-01
- [x] T058 [US3] Record `fixtures/baseline-topology-01/payloads/k8s/` from a kind cluster running the demo app (`deploy/kind/`), regenerate `events.jsonl` with `fixture record`, re-record goldens; verify diff vs hand-authored version is only additive → FR-044, FR-047; fixture: baseline-topology-01
- [x] T059 [US3] Implement `internal/feeders/k8s/map_changes.go`: `rollout` (pod-template hash / revision annotation, actor from change-cause), `scaling` (replica delta), `config_change` (referenced ConfigMap/Secret `resourceVersion` change) → FR-043, FR-003; fixture: config-change-01 (T066), rollout-regression-01
- [x] T060 [US3] Implement `internal/feeders/k8s/feeder.go` (`Describe`: per_source_sequence, window 60 s, checkpoints on start/resync) and `internal/cli/feed_k8s.go` → FR-044; fixture: baseline-topology-01
- [x] T061 [P] [US3] Testkit conformance tests `internal/feeders/k8s/feeder_test.go` and `internal/feeders/otel/feeder_test.go` (Run, Shuffle, DoubleDeliver) → US3 scenarios 1–3; fixture: baseline-topology-01
- [x] T062 [P] [US3] Add `deploy/kind/cluster.yaml` and `deploy/kind/otel-demo/` kustomization exporting OTLP to host → quickstart §7 prerequisite; fixture: n/a
- [x] T063 [US3] Record `fixtures/baseline-topology-01/payloads/otel/` from the same kind run (OTLP protobuf files), regenerate events and goldens → FR-044, SC-010; fixture: baseline-topology-01

**Checkpoint**: both feeders pass testkit; `fixture verify fixtures/baseline-topology-01` green from recorded payloads; quickstart §6 reproduces.

---

## Phase 6: User Story 4 — Blast radius (P2)

**Goal**: `Impact` query: downstream dependents and upstream dependencies with hop, heaviest path, alternatives, weight.

**Independent Test**: impact goldens in `baseline-topology-01` match.

- [x] T064 [US4] Implement `internal/query/impact.go`: bounded expansion on as-of slice, heaviest-path (max-min weight class, non-calls edges = 0), shortest hop per node, alternative path count, ordering weight → hop → entity id → FR-029, research §9 note; fixture: baseline-topology-01, rollout-regression-01
- [x] T065 [US4] Implement `internal/server/query_impact.go`, `internal/cli/query_impact.go`; add manifest queries (payments impact; storefront reached by two paths) and record goldens → FR-029, US4 scenarios 1–2; fixture: baseline-topology-01, config-change-01, rollout-regression-01 (all recorded)
- [x] T066 [P] [US4] Record `fixtures/config-change-01` (ConfigMap edit on payments at 14:15 via kind + `kubectl`), goldens for diff and impact → FR-043, FR-047; fixture: config-change-01
- [x] T067 [P] [US4] Record `fixtures/feeder-gap-01` (stop k8s feeder 14:00–15:00, delete a Service during the gap, restart) and goldens for subgraph + `extent` showing the gap; verify retraction valid_end inside gap and observed at reconnection → FR-052, edge case "feeder gap"; fixture: feeder-gap-01. (RECORDED 2026-09-16, VERIFIED after the Kubernetes feeder learned to notice its own gap: `--state-dir` holds the last checkpoint and the objects asserted, the informers push it as a `start` marker payload before the initial list, and the reconnection diffs the new list against it — `internal/feeders/k8s/state.go`, `Feeder.reconcile`. The goldens now show one gap for `k8s:kind` in `extent` and none for `otel:kind`, `k8s.service=shop/checkout` bounded at the reconnection with its observed interval starting there, and the `exposed-via` edge into it closed with `closed_as_consequence_of`. One deviation, stated in the fixture manifest: the valid end is the gap's upper bound rather than an instant strictly inside it, because `RetractNode` carries no `end_unknown` and the schema is frozen this phase — the `gap_before` checkpoint beside it is what says the truth lies in the hole.)

**Checkpoint**: quickstart §2 impact command reproduces; four fixtures green.

---

## Phase 7: User Story 5 — Know where to look (P2)

**Goal**: `Pointers` query grouped by kind, names valid at the as-of instant.

**Independent Test**: pointer goldens in `baseline-topology-01` match, including a pre-rename instant.

- [x] T068 [US5] Implement `internal/query/pointers.go` (group by kind, as-of aliases) and `internal/server/query_pointers.go`, `internal/cli/query_pointers.go` → FR-030; fixture: baseline-topology-01, rollout-regression-01. Recording US5 scenario 1 exposed a projector defect and its fix: a node version's pointer list was taken from the primary assertion, so a merged workload/service kept one feeder's pointers and dropped the other's; `materializeNode` now unions them across sources (research §4, docs/schema/pointers.md), and every fixture's goldens were re-recorded
- [x] T069 [US5] Add a rename event (payments → payments-v2 at 14:00, display name, `service.name` and pointer selectors together) and goldens at 13:00 and 14:32 → US5 scenario 2; fixture: **rollout-regression-01**, not baseline-topology-01 as originally written: baseline is a *recorded* fixture whose events.jsonl may only be rewritten by a cluster run (fixtures/README.md), so the rename went into the hand-authored fixture that already carries a change window. rollout-regression-01 also gained a direct `storefront → payments` calls edge at weight class 1, which is what gives US4 scenario 2 its two routes.
- [x] T070 [P] [US5] Implement `internal/query/history.go` (`NodeHistory`: all versions across observed time + decisions), server + CLI `query history` → FR-032, FR-034; fixture: retraction-with-edges-01, rollout-regression-01, ambiguous-identity-01 (NodeHistory is node-only per the proto, so late-arriving-fact-01's late *edge* is covered by its subgraph goldens instead)
- [x] T071 [P] [US5] Document pointer vocabulary in `docs/schema/pointers.md` (kinds, backend kinds, OTel semconv selectors, how a vendor connector maps them) and extend `docs/schema/queries.md` with Impact, Pointers and Node history → FR-008, FR-053; fixture: n/a

**Checkpoint**: quickstart §2 pointers command reproduces.

---

## Phase 8: User Story 6 — Correct the graph's idea of "the same thing" (P2)

**Goal**: probable rules → suggestions; human confirm/reject/split/merge as events with precedence; audit and suggestions queries; `decider` role.

**Independent Test**: `fixture verify fixtures/ambiguous-identity-01` passes including replay-after-confirm and conflict surfacing.

- [x] T072 [US6] Extend `internal/resolution/rules.go` (registry and certain rules C1–C3 exist from T017): add P1–P3 probable rules with documented scores; unit tests per rule → FR-037, research §10; fixture: unit
- [x] T073 [US6] Implement the resolution pass in `internal/projector/{refs.go,suggest.go}` (the rules stay in `internal/resolution`, which touches no database): on each new claim run rules; certain → merge (`merged_into`, claims re-pointed, decision row with rationale/claims); probable → `suggestions` upsert; human decision precedence check → conflict status instead of override → FR-038, FR-040, SC-007 (zero probable auto-merges asserted by verify), US6 scenario 3; fixture: ambiguous-identity-01
- [x] T074 [US6] Implement projector handlers `confirm_merge.go`, `reject_merge.go` (pair block list), `manual_merge.go`, `split_entity.go` (new entity from detached claims, history preserved) recording `principal` → FR-039, FR-040; fixture: ambiguous-identity-01
- [x] T075 [US6] Implement `internal/server/resolution.go` (`ResolutionService`, `decider` role, stamps principal, source_id `human`) and `internal/cli/resolve_{confirm,reject,merge,split}.go` (exit 3 on missing identity; verify asserts every decision row has a principal) → FR-041, FR-041a, SC-011, contracts/cli.md; fixture: ambiguous-identity-01
- [x] T076 [P] [US6] Implement `internal/query/audit.go` (`ResolutionAudit` with `observed_at`), `suggestions.go` (paged), server + CLI `resolve why`, `resolve suggestions` → FR-031, FR-033; fixture: ambiguous-identity-01
- [x] T077 [US6] Author `fixtures/ambiguous-identity-01` (otel `checkout` vs k8s `checkout-svc` matched only by P1; manifest `human_decisions` confirm by `dev:alice` at 14:35; later contradicting claim) and goldens for suggestions (before), audit (after), subgraph (merged view), history; replay-after-confirm asserts the merge survives (SC-007) → US6 scenarios 1–4; fixture: ambiguous-identity-01
- [x] T078 [US6] Extend `fixture verify --report` with resolution metrics: certain-rule precision, probable-score calibration against `ground_truth` pairs → constitution V calibration; fixture: ambiguous-identity-01
- [x] T079 [P] [US6] Add `docs/schema/resolution.md` publishing rules, scores, precedence and the audit query → FR-037, FR-053; fixture: n/a

**Checkpoint**: quickstart §4 reproduces; human decision survives `fixture verify` replay.

---

## Phase 9: User Story 7 — Reproduce an incident exactly (P3)

**Goal**: full harness in CI, observed-time pinning end to end, live-vs-recorded equality.

**Independent Test**: `fixture verify fixtures/* --report` green in CI; nightly kind run passes.

- [x] T080 [US7] Make `fixture verify` run every manifest query with `observed_at` pinned to `clock.end` as a second golden set (`golden/pinned/`); record for all fixtures → FR-015, US7 scenario 2, ADR-0001 D2; fixture: all
- [x] T081 [US7] Wire `aisre fixture verify fixtures/* --report` into `ci.yml` with job summary; fail on any mismatch → SC-003, SC-004, constitution VIII; fixture: all
- [x] T082 [US7] Add `.github/workflows/e2e-nightly.yml`: kind + otel-demo, both feeders with `--record`, triggered rollout, `fixture record` + `fixture verify` on the recording against the live graph (canonical export via `fixture export`); save the recording as `fixtures/rollout-regression-02` so the ranking family also has real payloads → FR-050 (b), SC-010, constitution VIII; fixture: live recording → rollout-regression-02
- [x] T083 [P] [US7] Implement `internal/fixture/export.go`: canonical export of the live graph for comparison with a replayed one (`aisre fixture export`) → SC-010; fixture: baseline-topology-01
- [x] T084 [P] [US7] Write `docs/evaluation/replayable-incident.md`: the triple (snapshot at T + telemetry window pointers + validated root cause) as fixture manifest fields, reserved for the agent feature → constitution VIII definition; fixture: n/a

**Checkpoint**: seven fixtures + pinned goldens green; nightly e2e green once.

---

## Phase 10: Polish & Cross-Cutting Concerns

- [X] T085 [P] Self-telemetry coverage: spans per event apply / RPC / feeder window; metrics from plan §Observability; test asserting instruments exist → FR-051; fixture: unit
- [X] T086 [P] `internal/server/extent.go` + CLI `extent` return gaps per source; golden in `feeder-gap-01` → FR-052; fixture: feeder-gap-01. (Verified 2026-09-17, no code change needed: the RPC is `internal/server/query_extent.go` — named for its sibling query handlers rather than for this task — and it returns `log.Extent`’s per-source checkpoints and `gap_before` intervals straight from the log rather than from the projection, because a gap is a statement about what was *delivered*. `internal/cli/extent.go` renders them as SOURCE / LAST CHECKPOINT / GAPS / GAP INTERVALS. `fixtures/feeder-gap-01/golden/extent.extent.json` and its observed-time-pinned twin carry one gap for `k8s:kind`, `[10:43:42.722705Z, 12:00:16.519Z)`, and none for `otel:kind`; both are compared on every `fixture verify` run, and `internal/server/parity_test.go` covers the RPC over gRPC and JSON.)
- [X] T087 [P] Contract parity tests `internal/server/parity_test.go`: every RPC via gRPC and via JSON returns canonical-equal bodies → plan §Testing; fixture: baseline-topology-01
- [X] T088 [P] Event schema evolution hook: `internal/log/migrate/` with a no-op `v1→v1` transformer and a test that fixtures declare `schema_version` and are rejected when unknown → FR-025, edge case "schema version drift"; fixture: baseline-topology-01 (mutated copy)
- [X] T089 [P] `docs/connectors/writing-a-feeder.md` walkthrough timed by a fresh reader; update per feedback → SC-008; fixture: baseline-topology-01
- [X] T090 Security pass: token audience checks, `feeder` token scoped to `source_id`, no secret values in any fixture (grep guard in CI), read-only DSN for query-only deployments documented → FR-046, constitution VII; fixture: all (grep guard)
- [X] T091 Implement `internal/fixture/bench.go` + CLI `fixture bench --scale 10k`: generator (10k entities, 100k edges power-law, 1M events, 50k changes, 200k claims), measurements per research §2, markdown report to `docs/benchmarks/`; `.github/workflows/bench.yml` weekly → SC-001, SC-002, ADR-0002 exit criteria; fixture: bench
- [X] T092 Run quickstart.md §1–§8 end to end on a clean machine; fix drift; record the run in `docs/benchmarks/` and `docs/decisions/ADR-0002` status → accepted → FR-050; fixture: all
- [X] T093 [P] (Optional spike, not required for 001 acceptance) `examples/mcp-feeder/`: minimal feeder consuming an MCP server via the official Go MCP SDK, emitting owner nodes + identity claims + pointers from a mock server; recorded payloads + testkit → plan §Connector SDK, ADR-0001 D4; fixture: examples/mcp-feeder/testdata/directory-01; write-up: docs/connectors/mcp.md

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (1)** → **Foundational (2)** → stories. Phase 2 blocks everything.
- **US1 (3)** first: every other query reuses the as-of slice (T031).
- **US2 (4)** needs T031/T032 (slice + hydration) and T020 (change nodes).
- **US3 (5)** needs T025 (IngestService) and T028 (verify); its recordings (T058, T063) upgrade `baseline-topology-01` from hand-authored to recorded, so US1/US2 goldens are re-recorded there.
- **US4 (6)**, **US5 (7)** need US1 only. **US6 (8)** needs T017 (refs/claims) and T025. **US7 (9)** needs all fixtures.
- **Polish (10)** after US7, except T085/T087/T088 which can start after Phase 2.

### Parallel Opportunities

- Phase 1: T003–T007 in parallel after T001/T002.
- Phase 2: T008/T009/T010 parallel; T014–T016 after T012; T017–T020 after T014; T022/T023/T029/T030 anytime after T010.
- After Phase 2: US1 and the SDK half of US3 (T045–T049) in parallel; US4/US5/US6 in parallel once US1 lands; feeders (T050–T063) in parallel with US2.

### Parallel Example: Phase 5 (US3)

```bash
# SDK core first (T045), then in parallel:
Task: "pkg/feeder/emit (T046)"   Task: "pkg/feeder/record (T047)"   Task: "pkg/feeder/source/file (T049)"
# Then two feeder tracks in parallel:
Track A: T050 → T051 → T052 → T053 → T063
Track B: T055 → T056 → T057 → T059 → T060 → T058
# Then conformance: T061
```

---

## Implementation Strategy

### MVP (US1 on hand-authored fixtures)

1. Phases 1–2, then Phase 3. Stop. `fixture verify` on three hand-authored fixtures green.
2. Demo: quickstart §2 (first command) and §3.

### Increment 2 (the thesis)

3. Phase 4 (diff + ranking) → `--report` shows culprit rank 1 on `rollout-regression-01`.

### Increment 3 (real data)

4. Phase 5 (SDK + feeders + recordings) → baseline fixture now from real payloads.

### Increment 4 (P2 stories, parallel)

5. Phases 6–8. Then Phase 9 turns the harness into the CI gate and adds the live run.

### Definition of done for 001 (FR-050)

- `fixture verify fixtures/* --report` green in CI (a), and one green `e2e-nightly` run with
  recorded == live (b). T092 closes.

## Notes

- Every task touching `fixtures/` regenerates goldens with `fixture record`; golden diffs are
  reviewed in the PR like code.
- Hand-authored fixtures (T024, T029, T030, T043, T077) are legitimate: the event schema is
  public and connector-independent. Recorded payloads replace or supplement them as feeders
  land (T058, T063, T066, T067).
- `TODO-org` in module path and `sre-agent` binary name are placeholders pending the project
  name.

### Bug fixes

- **2026-09-16 — merges did not absorb edges.** A merge closed the loser's *node* versions and
  re-applied them to the survivor but left its edges pointing at the id that was merged away.
  `payments → payments-db`, asserted at 13:03 under the pre-merge OpenTelemetry id and
  re-asserted at 14:20 under the survivor, therefore became two `edge_versions` rows that were
  both valid at 14:32: the exclusion constraint keys on the raw `src_id`/`dst_id`, so it saw two
  unrelated series and refused neither, and `Subgraph` returned the relationship twice. Fixed in
  `internal/projector/refs.go` (`absorbEdges`), which closes the stale rows with the merge event
  and re-applies what produced them to the canonical endpoints through the ordinary edge
  segmentation. `projector.CheckInvariants` (new, `internal/projector/invariants.go`) is the
  assertion the database cannot make and runs after every fixture replay, re-delivery and
  shuffle. The re-weight of `payments → payments-db` that Phase 4 had removed because of the bug
  is back in `fixtures/rollout-regression-01`, so the `checkout-diff` goldens now carry the
  `edges_changed` entry that proves it. `research.md` §4 records the corrected behaviour.
  No task boxes were ticked or unticked by this fix.

- **2026-09-17 — a retraction delivered before a re-assertion lost the interval.** Coalescing
  two neighbouring segments with equal content threw away the instant of the later assertion, so
  a `retract_edge`/`retract_node` arriving afterwards truncated a segment it should have split:
  `upsert@10:40, upsert@11:55, retract@10:45` left `[10:40, 10:45)` and lost `[11:55, ∞)`, while
  the same three events in valid-time order produced both. It was the documented ordering caveat
  in `planRetract`, pinned by `TestRetractionBeforeAReassertionIsOrderDependent`, and it was why
  `k8s.DefaultReorderingWindow` had been cut from 60 s to 5 s. Fixed in
  `internal/projector/segments.go`: a coalesced segment remembers, per source, the **first**
  instant that source restated it (`rememberRestatement` — first and not latest, so a feeder
  restating an unchanged fact every window costs one extra version row once rather than one per
  window); every planner expands a segment back into the sub-segments those instants imply before
  doing anything else (`expandRestatements`) and re-coalesces afterwards; a retraction therefore
  splits rather than truncates, and what survives it at or after its instant is filtered per
  source. The guardrail test is now `TestRetractionAndReassertionAreOrderIndependent`, beside
  `TestRetractReassertRetractPermutations` (every legal delivery order of a seen/gone/seen/gone
  sequence, for a node and for an edge) and a `rapid` property test over mixed upsert/retract
  streams. `k8s.DefaultReorderingWindow` is back to **60 s**, all eight fixture manifests declare
  it, and all eight pass the shuffle step at the wider window.
  `fixtures/rollout-regression-02`'s goldens were re-recorded: three `shop/payments` edges the
  Kubernetes feeder re-asserts identically across the rollout now carry both event ids in their
  evidence chain and an observed start at the re-assertion, with their valid intervals and the
  whole node/edge/change set unchanged. `research.md` §5 records the behaviour and the one
  ordering that is still not survived — a retraction delivered before the assertions it ends has
  nothing in the projection to cut. No task boxes were ticked or unticked by this fix.

- **2026-09-17 — replay committed one transaction per event.** `Projector.Replay` opened a
  transaction per event, which made a 1M-event replay a million commits: measured at ~39 events/s
  against the ingestion path's 435, so the ADR-0002 60-minute replay criterion was missed by that
  loop rather than by the store (`docs/benchmarks/README.md`, "Open finding: replay").
  `Projector.ReplayWithOptions` now applies `BatchSize` events per transaction, 500 by default,
  streaming the log a batch at a time so a million-event replay holds a batch in memory and not a
  million payloads; `Replay` is the default-options wrapper and `BatchSize: 1` restores the old
  loop. FR-023 is untouched — the same events, in the same append order, each with the observed
  time recorded on its own log row — and `internal/projector/replay_batch_test.go` replays every
  shipped fixture at batch sizes 1, 7 and 500 and requires identical snapshots and identical
  `produced_by_event_ids`. `fixture verify`'s replay step (`fixture.LoadOptions.BatchSize`) and
  `fixture bench` are wired to it; `fixture bench` gained `--replay-batch` and `--replay-only`.
  Measured at scale 10k in `docs/benchmarks/2026-09-17-replay.md`. No task boxes were ticked or
  unticked by this fix.
