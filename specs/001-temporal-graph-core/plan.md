# Implementation Plan: Temporal System Graph Core

**Branch**: `001-temporal-graph-core` | **Date**: 2026-09-15 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-temporal-graph-core/spec.md`

## Summary

Build the substrate of the AI SRE agent: a bitemporal, event-sourced graph of a production
system, exposed through an authenticated gRPC/JSON API and a CLI, fed by two reference feeders
(OpenTelemetry topology from OTLP spans, Kubernetes workloads and changes), with conservative
auditable entity resolution and a replay-fixture harness that is also the CI gate.

Technical approach, from [research.md](./research.md): **Go** single static binary;
**PostgreSQL** as both the append-only event log and the materialized bitemporal graph
(range types + GiST), with graph traversal done in-process over an as-of adjacency slice;
**ConnectRPC** for one server that speaks gRPC, gRPC-Web and JSON from a single protobuf
schema; **OIDC** for individual identity; **OpenTelemetry** for self-observability. No
dedicated graph database in v1; a benchmark plan decides whether that ever changes.

## Technical Context

**Language/Version**: Go, latest stable at implementation start (minimum 1.25), `CGO_ENABLED=0`.

**Primary Dependencies**: `pgx/v5` (Postgres driver), `connectrpc.com/connect` (gRPC + JSON
API), `buf` toolchain (protobuf schema, lint, breaking-change checks), `cobra` (CLI),
`go.opentelemetry.io/otel` SDK (self-telemetry), `go.opentelemetry.io/proto/otlp` (OTLP
message types for the topology feeder's receiver), `k8s.io/client-go` (informers for the
Kubernetes feeder), `github.com/coreos/go-oidc/v3` (identity), embedded SQL migrations applied through `pgx` (research §14).

**Storage**: PostgreSQL 16+. Single database, two logical areas: `log` (append-only events,
rejected events, source checkpoints) and `graph` (materialized bitemporal versions). Tests
run against a real Postgres via `testcontainers-go` (Docker) with an `embedded-postgres`
fallback for Docker-less CI runners.

**Testing**: `go test` with table-driven unit tests; replay/golden tests driven by fixture
directories; property tests for order-independence and idempotency (`pgregory.net/rapid`);
benchmark suite (`go test -bench`) on a generated reference workload; nightly live end-to-end
on a `kind` cluster running the OpenTelemetry demo application.

**Target Platform**: Linux and macOS servers/containers (amd64, arm64). Distributed as a
static binary and an OCI image.

**Project Type**: Single Go module producing one binary (`sre-agent`, working name) with
subcommands: `serve` (API + ingestion), `feed otel`, `feed k8s`, `query *`, `resolve *`,
`fixture *`, `migrate`. Public packages for the feeder SDK.

**Performance Goals**: SC-001 subgraph as-of, 2 hops, p95 < 1 s; SC-002 diff over 24 h with
ranking, p95 < 2 s; sustained ingestion ≥ 500 events/s on a laptop-class Postgres; replay of
1M events < 30 min.

**Constraints**: read-only credentials toward every source; no telemetry payloads stored;
append-only history; every response element traceable to event ids; deterministic replay
(canonical serialization byte-identical); individual authentication on every API call.

**Scale/Scope**: reference workload ~10k entities, ~100k edges, ~1M events, ~50k change
nodes, ~200k identity claims. Single organisation per deployment.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| # | Principle | Gate | Status |
|---|-----------|------|--------|
| I | Graph first | Every capability is an event type in `contracts/graph.proto` or a query RPC over the graph. Feeders have no other output path. Caches (adjacency slice, ranking) are derivations rebuildable from the log. | PASS |
| II | Bitemporal or nothing | `entity_versions` and `edge_versions` carry `valid` and `observed` `tstzrange` columns, both NOT NULL; every query RPC takes `valid_at` and optional `observed_at`. Unknown boundaries are explicit flags, not sentinels. | PASS |
| III | Event-sourced ingestion | `log.events` is append-only (no UPDATE/DELETE grants for the app role). Projector applies one event per transaction. Idempotency key unique index. Replay reads observed time from the log. No truncate-and-reload path exists; `migrate` only alters schema. | PASS |
| IV | Telemetry stays in its backend | Event validation rejects fields matching the telemetry-payload denylist (`metric_value`, `log_body`, span attributes beyond the topology whitelist). Pointers are `Pointer` messages with OTel-conventional selectors. OTLP receiver keeps spans in memory for the aggregation window only, never persists them. | PASS |
| V | Evidence-first | Every returned node/edge version carries `produced_by_event_ids`; ranking items carry score components; resolution audit RPC returns claims, rules, scores, decisions. `unknown` is representable in intervals and confidence. Calibration reporting is a fixture metric (`fixture verify --report`). | PASS |
| VI | Entity resolution auditable | Claims stored before merge; `resolution_decisions` records rule, score, rationale, claim ids, principal; human decisions are events with precedence; audit RPC exists. | PASS |
| VII | Read-only by default | Feeders check granted permissions at start (K8s `SelfSubjectRulesReview`; OTLP receiver is passive). The tool's own Postgres is exempt per constitution. No remediation code. | PASS |
| VIII | Evaluation from day one | Seven fixture families in FR-047 live in `fixtures/`; CI job `replay` runs replay, double-delivery and shuffle checks; benchmark job runs on the reference workload. | PASS |
| IX | Open schema | `contracts/graph.proto` is the published schema; `buf breaking` gates PRs; `docs/schema/` renders it; semver on the package. | PASS |
| X | Simplicity over cleverness | One binary, one external service (Postgres). No graph DB. Benchmark plan in research.md decides any future change. Each dependency justified in Complexity Tracking. | PASS with 2 justified items |

**Post-Phase-1 re-check** (after data model and contracts were written): all gates still
pass. Two adjustments made during the self-analysis: (1) conflicting facts from different
sources are kept side by side with provenance instead of being resolved by the projector
(Principle V, edge case "conflicting sources"); (2) the shuffle check compares valid-time
state structurally because canonical ids derive from the first-seen claim (Principle III).

## Project Structure

### Documentation (this feature)

```text
specs/001-temporal-graph-core/
├── plan.md              # This file
├── research.md          # Phase 0: decisions, storage comparison, benchmark plan
├── data-model.md        # Phase 1: tables, invariants, state transitions
├── quickstart.md        # Phase 1: run and validate end to end
├── contracts/
│   ├── graph.proto      # Public schema: events, entities, queries (source of truth)
│   ├── cli.md           # CLI command contract
│   ├── feeder-sdk.md    # Connector SDK contract
│   └── fixture-format.md# Replay fixture and golden output format
└── tasks.md             # Phase 2 (/speckit-tasks)
```

### Source Code (repository root)

```text
cmd/
└── sre-agent/                 # single binary entrypoint (cobra root)

api/
└── sreagent/graph/v1/         # graph.proto (+ buf.yaml, buf.gen.yaml at repo root)

internal/
├── log/                       # append-only event log: append, dedupe, read range, checkpoints
├── projector/                 # applies events to bitemporal tables, one per tx
├── graph/                     # domain types: Entity, Edge, Version, Interval, Pointer
├── query/                     # as-of slice loader, BFS expansion, diff, ranking, impact
├── resolution/                # claims, rules (certain/probable), merge/split, overrides
├── store/postgres/            # SQL, migrations (embedded), range helpers
├── server/                    # ConnectRPC handlers, authn (OIDC), authz (read/decide roles)
├── feeders/
│   ├── otel/                  # OTLP receiver (gRPC+HTTP), window aggregator → events
│   └── k8s/                   # informers → events (workloads, configs, changes, owners)
├── fixture/                   # load/record/verify, canonical serialization, golden diff
├── telemetry/                 # OTel SDK setup for the tool itself
└── cli/                       # command implementations (query, resolve, fixture, feed)

pkg/
└── feeder/                    # PUBLIC feeder SDK: Feeder interface, Emitter, Recorder, testkit

fixtures/
├── baseline-topology/
├── rollout-regression/
├── config-change/
├── late-arriving-fact/
├── ambiguous-identity/
├── retraction-with-edges/
└── feeder-gap/

deploy/
├── docker-compose.yml         # Postgres + sre-agent for local use
└── kind/                      # kind config + otel-demo manifests for live e2e

docs/
├── schema/                    # rendered protobuf docs, temporal model, taxonomy
├── decisions/                 # ADRs
└── connectors/                # how to write a feeder

.github/workflows/             # ci.yml (lint, unit, replay), bench.yml, e2e-nightly.yml
LICENSE                        # Apache-2.0
CONTRIBUTING.md  CODEOWNERS  SECURITY.md
```

**Structure Decision**: single Go module, single binary. `pkg/feeder` is the only public Go
API and is versioned with the schema. Everything else is `internal/` so the protobuf contract,
not Go types, is the public surface.

**Terminology**: *connector* is the general concept (an integration with one external
system); *feeder* is the concrete implementation of a connector against the SDK. The two are
used accordingly throughout the plan artifacts.

## Design Decisions (summary, details in research.md)

1. **Go** over Rust/Python: static binary, first-class OTel and Kubernetes client libraries,
   trivial cross-compilation, adequate performance for the reference workload.
2. **Postgres as log + materialized bitemporal graph** over an embedded engine or a graph DB:
   one dependency, range types and GiST make as-of queries indexable, transactional
   projection gives exact replay, and the traversal depth (2–3 hops on 100k edges) is small
   enough to do in-process over an as-of adjacency slice. Benchmark plan defines the exit
   criteria that would justify revisiting.
3. **ConnectRPC** over grpc-go + grpc-gateway: one handler serves gRPC, gRPC-Web and
   JSON/HTTP from the same protobuf; no second gateway process; curl-able.
4. **OIDC bearer tokens** for individual identity, with a dev-only static identity provider
   for fixtures and CI. Two roles: `reader` (queries) and `decider` (resolution decisions).
5. **Deterministic identifiers**: entity ids derive from the first identity claim (namespace +
   value hash); event ids and idempotency keys come from sources; observed time is read from
   the log on replay. This is what makes replay byte-identical.
6. **OTLP receiver in-process** for the topology feeder (gRPC and HTTP/protobuf), using the
   OTLP proto types only, not the collector framework. Spans live only in the aggregation
   window.
7. **Kubernetes via informers**; changes derived from Deployment revision annotations,
   ReplicaSet transitions, replica count deltas and ConfigMap/Secret `resourceVersion`
   changes (secrets by version only).
8. **Ranking** = published closed-form score with exposed components; no learned weights in
   v1. Tie-break: earlier change id lexicographically.
9. **Fixtures** = directory with `payloads/`, `events.jsonl`, `golden/*.json`, `manifest.yaml`.
   Canonical JSON serialization (sorted keys, RFC 3339 UTC, stable ordering) for diffing.

## Connector SDK (how a feeder is written, tested, versioned)

Contract in [contracts/feeder-sdk.md](./contracts/feeder-sdk.md). In short: implement
`feeder.Feeder` (`Describe`, `Run(ctx, Emitter)`), emit typed events through `Emitter`,
declare ordering guarantees and reordering window in `Describe`. The SDK provides a
`Recorder` that captures raw source payloads and emitted events, and a `testkit` that replays
recorded payloads through the feeder and diffs against golden events and golden graph. A
feeder is versioned with semver; its fixtures pin the event schema version they were recorded
under. Live mode and recorded mode share the same code path behind a `Source` interface.

## Observability of the tool itself

`internal/telemetry` configures the OTel SDK (OTLP exporter, resource attributes
`service.name=sre-agent`, `service.version`). Spans: per event applied, per RPC, per feeder
window. Metrics: `events_applied_total{type,source}`, `events_rejected_total{reason}`,
`event_apply_latency`, `query_latency{rpc}`, `feeder_lag_seconds{source}`,
`resolution_decisions_total{kind,by}`, `suggestions_pending`. Logs: structured `slog` with
trace correlation.

## CI, licensing, contribution model

- **CI (GitHub Actions)**: `ci.yml` on PR: `buf lint`, `buf breaking` against `main`,
  `golangci-lint`, `go test ./...` (unit + replay + double-delivery + shuffle on all fixtures),
  `aisre fixture verify fixtures/* --report` (ranking and calibration metrics published as
  a job summary, Principle V), `govulncheck`. `bench.yml` weekly and on demand: reference workload benchmark, results
  committed to `docs/benchmarks/`. `e2e-nightly.yml`: kind + otel-demo live run, compare to
  recording (FR-050 b).
- **License**: Apache-2.0 with `LICENSE` and SPDX headers. Fixtures include a `NOTICE` on the
  demo application's license.
- **Contribution**: DCO sign-off; `CONTRIBUTING.md`; `CODEOWNERS`; schema changes require an
  ADR under `docs/decisions/` and pass `buf breaking`; feeders contributed under
  `internal/feeders/<name>` with fixtures or they are not merged (Principle VIII).

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| External PostgreSQL service (Principle X, single binary / minimal deps) | Durable append-only log, range indexes for as-of queries, transactions for exact projection, operational familiarity for SREs. | Embedded engine (SQLite/Badger) would make the binary fully self-contained, but SQLite lacks range types and GiST, forcing hand-rolled bitemporal indexing; a KV store would force reimplementing transactions and query planning. Both raise correctness risk on the hardest part of the system. See research.md §2. |
| OIDC dependency and identity provider requirement (Principle X) | Spec FR-041a requires individual identity on every call from v1 (owner decision). | A shared token was rejected by the owner. Rolling our own user store would be more code and less secure. A static dev provider keeps fixtures and CI free of an external IdP. |

## Phase 0 and Phase 1 outputs

- [research.md](./research.md) — all Technical Context unknowns resolved; storage comparison
  and benchmark plan.
- [data-model.md](./data-model.md) — tables, invariants, state transitions.
- [contracts/graph.proto](./contracts/graph.proto), [contracts/cli.md](./contracts/cli.md),
  [contracts/feeder-sdk.md](./contracts/feeder-sdk.md),
  [contracts/fixture-format.md](./contracts/fixture-format.md).
- [quickstart.md](./quickstart.md) — run, load a fixture, query, verify, live demo.
