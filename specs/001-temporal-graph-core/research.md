# Research: Temporal System Graph Core

**Feature**: 001-temporal-graph-core | **Date**: 2026-09-15

Each section: Decision, Rationale, Alternatives considered. Items marked *benchmark-gated*
have explicit exit criteria that could reverse the decision.

## 1. Language

**Decision**: Go (latest stable at implementation start, minimum 1.25), `CGO_ENABLED=0`.

**Rationale**:
- Single static binary and trivial cross-compilation satisfy Principle X directly.
- The two reference feeders and every planned connector are long-running daemons talking to
  APIs; Go's concurrency model and standard library fit that shape.
- First-class ecosystem where it matters here: `client-go` (Kubernetes), the OpenTelemetry Go
  SDK and OTLP proto types, `pgx`, ConnectRPC, `buf`.
- Reference workload (10k/100k/1M) is well within Go's performance envelope; GC pauses are
  irrelevant at these latencies.
- Contribution surface: SREs and platform engineers overwhelmingly read and write Go.

**Alternatives considered**:
- *Rust*: better raw performance and memory control, not needed at this scale; slower
  contribution velocity; Kubernetes and OTel ecosystems less mature. Rejected.
- *Python*: fastest prototyping, best for the later LLM layer, but no static binary, weaker
  for long-running connectors, and the graph core is a correctness-critical component where a
  type system pays off. Rejected for the core; the agent layer may use it later.
- *TypeScript/Node*: good API ergonomics, weak fit for daemons and binary distribution.
  Rejected.

## 2. Storage (benchmark-gated)

**Decision**: PostgreSQL 16+ as both the append-only event log and the materialized
bitemporal graph. Graph traversal is done in-process over an "as-of adjacency slice" loaded
with one or two indexed queries. No dedicated graph database in v1.

**Comparison**:

| Criterion | A. Postgres log + materialized bitemporal tables | B. Embedded engine (SQLite, or Badger/Pebble KV + in-memory graph with snapshots) | C. Dedicated graph DB (Neo4j, Memgraph, Dgraph, or Apache AGE on Postgres) |
|---|---|---|---|
| Bitemporal as-of indexing | Native: `tstzrange` columns + GiST, `&&`/`@>` operators; multicolumn with entity id. | SQLite: no range types, emulate with two columns and composite indexes; KV: hand-rolled interval index. | Property graphs have no native bitemporal model; must encode intervals as properties and filter in traversal (no index on interval overlap in most engines). |
| Exact replay / transactional projection | ACID, one event per tx, `SERIALIZABLE` available. | SQLite: ACID single-writer, fine. KV: manual. | ACID per engine; AGE inherits Postgres. |
| Traversal 2–3 hops, 100k edges | Recursive CTE possible but slow with temporal predicates; instead load as-of adjacency (indexed range scan) and BFS in Go. Estimated: hundreds of ms. | In-memory graph: fastest traversal, but snapshotting per as-of instant is the hard part. | Native traversal fastest for deep paths; irrelevant at 2–3 hops. |
| Diff T1→T2 | Two as-of slices + set difference, plus change-node range query; all indexable. | Same logic; interval scans slower without range index. | Same logic; interval filtering unindexed. |
| Operational burden for an SRE team | One well-known service; backups, HA, managed offerings everywhere. | Zero extra service (best), but a single-node file; HA and backups are DIY. | A second stateful service with its own failure modes; AGE reduces this to a Postgres extension but adds an unusual dependency. |
| Single binary (Principle X) | Binary is single; Postgres is an external dependency. | Fully self-contained. | Violates; second service. |
| Ecosystem / tooling | Excellent. | Good (SQLite), thin (KV). | Good but vendor-specific query languages (Cypher/GQL). |
| Risk | Low. | Medium: reimplementing bitemporal indexing correctly. | Medium-high: bitemporal semantics unsupported; licensing (Neo4j) and lock-in. |

**Rationale**: Principle X says no graph DB without a benchmark proving need. The distinctive
workload here is *temporal filtering*, not deep traversal. Range types with GiST are exactly
the index for it. Traversal depth is tiny. Option A wins on correctness risk and operational
familiarity. Option B is attractive for a self-contained developer experience and is kept as
a possible *future* `--store=sqlite` for laptops, but not in v1 (two storage backends double
the surface of the hardest component).

**Query strategy** (what the benchmark measures):
1. *As-of slice*: `SELECT ... FROM graph.edge_versions WHERE valid @> $t AND observed @> $o
   AND (src_id = ANY($frontier) OR dst_id = ANY($frontier))` iterated per hop with caps;
   GiST index on `(valid, observed)` plus btree on `src_id`, `dst_id`. Two hops = two round
   trips.
2. *Diff*: slice at T1, slice at T2, set-difference in Go; change nodes via
   `valid && tstzrange($t1,$t2]` intersected with target ids in the union slice ± margin.
3. *Impact*: as-of slice expanded to the cap, Dijkstra-like on weight class.

**Benchmark plan** (`bench.yml`, results committed under `docs/benchmarks/`):
- Generator produces a synthetic organisation: 10k entities, 100k edges (power-law degree,
  hub nodes), 1M events spanning 90 days including 50k change nodes and 200k identity claims.
- Measurements at p50/p95/p99 over 1,000 randomized queries each: subgraph 2-hop as-of;
  subgraph 3-hop with caps; diff over 1 h, 24 h, 7 d; impact; replay throughput (events/s);
  full replay wall time.
- **Exit criteria that reopen the decision**: SC-001 (2-hop p95 < 1 s) or SC-002 (24 h diff
  p95 < 2 s) missed by more than 2× after index tuning, or replay of 1M events > 60 min. Only
  then evaluate Option C (Apache AGE first, since it keeps a single service), with the same
  harness.

## 3. Bitemporal representation

**Decision**: Version rows. Each `entity_versions` / `edge_versions` row holds `valid
tstzrange NOT NULL`, `observed tstzrange NOT NULL`, `valid_from_unknown bool`,
`valid_to_unknown bool`, a JSONB `props`, JSONB `pointers`, and `produced_by_event_ids
text[]`. Current knowledge = `upper_inf(observed)`. Correction = `UPDATE` the old row's
`observed` upper bound (the one permitted mutation, and only ever from open to closed) +
INSERT new row. Retraction = INSERT a new version with `valid` upper bound set, closing the
previous version's `observed`.

**Rationale**: matches FR-010 to FR-014 literally; both intervals indexable; "as known at
T_o" is a single predicate `observed @> T_o`. Unknown boundaries need flags because a range
cannot distinguish "unbounded" from "unknown" (FR-011): storage uses the first observation
time as the physical lower bound with `valid_from_unknown = true`.

**Alternatives**: Snapshot-per-observation (space explosion); event-only with reconstruction
at query time (too slow for 1M events); SQL:2011 system-versioned tables (Postgres has no
native support). Rejected.

## 4. Deterministic identifiers and replay

**Decision**:
- `event_id`: supplied by the source; must be globally unique; convention
  `<source_id>:<opaque>`.
- `idempotency_key`: supplied by the source; defaults to `event_id`; unique index.
- `observed_at` (observed lower bound): assigned by the log at append time and *stored in the
  event row*; the projector reads it from the row, so replay uses recorded time (FR-023).
- `entity_id`: `sha256(namespace || 0x00 || value)` of the first identity claim, base32,
  truncated to 26 chars. Merges keep the surviving id (deterministic rule: the id that first
  appeared in the log, tie-break lexicographic) and record redirects.
- `version_id`: `sha256(entity_id || event_id || valid_from)` (implemented as
  `graph.SegmentVersionID`); edges likewise with `(src, dst, type)`. The valid lower bound is
  part of the id because one upsert can produce several segments (the remainder of a split
  version plus the new fact), so `(entity, event)` alone is not unique.
- Canonical serialization for goldens: canonical protobuf-JSON (sorted keys, RFC 3339 UTC with
  protojson's 0/3/6/9 fractional digits, 64-bit integers as strings), arrays ordered by id.

**Implementation notes recorded after Phase 2 (2026-09-15)**:
- *Merge absorbs*: when two entities merge, the loser's current versions are closed in observed
  time and their assertions re-applied to the survivor through the same segmentation. Leaving
  them dangling made the graph order-dependent (proved by the shuffle test). Old rows remain
  queryable as of any observed instant inside their interval.
- *Merge absorbs edges too* (corrected 2026-09-16): every edge version still current in observed
  time whose `src_id` or `dst_id` is the loser is closed the same way, and what produced it is
  re-applied to the canonical endpoints — assertions through the ordinary edge segmentation,
  `changed_by` rows (which no assertion stands behind) carried over as they are. Not doing this
  was a defect, not a decision: the exclusion constraint keys on the raw endpoint columns, so a
  relationship asserted before the merge and re-asserted after it produced two rows that were
  both valid at the same instant in two different weight classes, and nothing refused them.
  Current edge endpoints are therefore always canonical; the query layer still follows
  `merged_into` on both endpoints, because a read rewound to an observed instant *before* a merge
  must still surface the rows that named the loser then.
- *Point-in-time changes* are stored over `[valid_at, valid_at + 1µs)`, the shortest non-empty
  range, so an open-ended change does not intersect every future diff window.
- *`changed_by` direction*: `src = target entity, dst = change node`.
- *Primary assertion*: display name and weight class come from the greatest
  `(valid_at, source_id, event_id)` among contributing sources (deterministic, order-free).
- *Pointers are unioned, not primary-picked* (corrected 2026-09-16, US5): a node version's
  pointer list is the union of every contributing source's current assertion, deduplicated by
  `(kind, backend_kind, vocabulary, selector)` and stored in `(kind, backend_kind, selector,
  vocabulary)` order. Taking them from the primary assertion was a defect, not a decision: a
  merged Kubernetes workload / OpenTelemetry service kept only the winner's list, so
  `query pointers` on the baseline fixture returned the Kubernetes `LOG` and `SOURCE_LINK`
  selectors and none of the OpenTelemetry `METRIC` and `TRACE` ones — and which half survived
  depended on which feeder happened to assert the later valid time. Two sources naming two
  backends are both right; a pointer is data about *where to look* (constitution IV) and losing
  one loses an investigation path, so US5 scenario 1 ("at least one metric, one log and one
  trace selector") was unanswerable on any merged entity. The display name stays primary-picked,
  because a thing has one name at a time; properties stay per-source, because two values for one
  key is a disagreement and the graph never picks a winner. Attributes are not part of a
  pointer's identity: two sources agreeing on where to look name one pointer, and the first in
  the stored order supplies its attributes.
- *Ref resolution* is canonical-id-first: an upsert creates or finds `EntityID(ns, value)`
  and never merges as a side effect; only `identity_claim` events trigger rules.
- *Replay commits in batches* (added 2026-09-17): `Projector.ReplayWithOptions` applies
  `BatchSize` events per transaction, 500 by default (`Replay` is the default-options wrapper).
  FR-023 is untouched — the same events, in the same append order, each with the observed time
  recorded on its own log row, applied one at a time by the same handlers — and the shipped
  fixtures are replayed at batch sizes 1, 7 and 500 in CI with byte-identical snapshots and
  identical `produced_by_event_ids` (`internal/projector/replay_batch_test.go`). Constitution
  III's "one event at a time" is about *incremental application*, and it still holds: no batch
  rebuild from a source system, no truncate-and-reload, no folding several events into one
  write. The transaction boundary is a durability decision. Its one cost is stated rather than
  hidden: a failing event fails its batch, so a replay that dies part-way leaves the projection
  at the last committed batch boundary. That is acceptable because a replay is not a live path —
  it rebuilds into an empty projection and the remedy for a failed one is to run it again from
  empty, never to resume in place; `Apply`, which *is* the live path, is still one event per
  transaction. The reason is measured: one transaction per event replayed at ~39 events/s
  against the ingestion path's 435 (docs/benchmarks/README.md), and the ADR-0002 60-minute
  criterion was missed by the loop, not by the store. The replay reads the log through
  `log.Page`, a `LIMIT`ed read, rather than by stopping an unbounded `Iterate` early: the driver
  drains a result set when its rows are closed, so an early-stopped page still paid for the rest
  of the log and made the replay quadratic — 63% of the profile, and more of the win than the
  batching itself. Measured end to end at 37 min 44 s for 995 222 events, 440 events/s
  (docs/benchmarks/2026-09-17-replay.md), which meets the ADR-0002 criterion and misses plan.md's
  30-minute goal.

**Rationale**: no random ids anywhere in the projection means the same log produces the same
bytes (FR-023, full replay in log order). Note that `entity_id` depends on which claim the
log saw *first*, so the **shuffle** check (FR-048) compares valid-time state *up to entity-id
relabeling*: entities are matched by their alias sets, then versions and edges are compared
structurally. Byte-identity is required for replay, structural identity for shuffle. **Alternatives**: ULIDs at projection (non-deterministic across replay, rejected);
sequence ids (depend on insertion order across sources, rejected).

## 5. Event ordering and out-of-order handling

**Decision**: Sources declare `ordering: per_source_sequence | none` and a
`reordering_window` in `Describe`. The projector never reorders the log. Valid-time state is
order-independent by construction: every upsert carries the source-asserted valid time, and
the projector inserts versions by valid interval, splitting or closing overlapping versions of
the same entity so the final valid-time partition is a pure function of the event *set*.
Observed time records actual arrival (FR-019, FR-021).

**Rationale**: satisfies FR-021 without buffering. The shuffle test in FR-048 verifies it.

**Order-independent retraction** (implementation note, 2026-09-17). The partition is a function
of the set of *(assertion, retraction)* facts, **including assertion instants**. Coalescing
neighbouring segments that carry equal content is a storage decision, and it used to lose one of
those instants: a re-assertion folded into an earlier segment left no trace of when it was made,
so a retraction arriving afterwards truncated a segment it should have split, and the interval
the re-assertion had opened disappeared. That was the documented ordering caveat, and it was why
both reference feeders had to declare reordering windows narrow enough to forbid the swap.

The fix is three small rules in `internal/projector/segments.go`:

1. a coalesced segment remembers, per source, the **first** instant that source restated it
   (`rememberRestatement`). First and not latest, deliberately: a telemetry feeder restates an
   unchanged edge every aggregation window, and remembering the latest would rewrite the version
   row on every window — one version per window per edge, which is exactly what coalescing
   exists to prevent. Remembering the first is idempotent, so the cost is one extra version row
   per series, once, and `TestUpsertNodeNoop` pins that it does not grow with the feeder's
   schedule;
2. every planner **expands** a segment back into the sub-segments those instants imply before
   doing anything else (`expandRestatements`), and re-coalesces afterwards, which round-trips
   exactly — a plan that changes nothing writes nothing;
3. a retraction therefore **splits** rather than truncates, and what survives it at or after its
   instant is filtered per source: a source that spoke only before the retraction is retracted
   even when a different source keeps the interval alive.

What this does *not* fix, stated because feeder declarations depend on it: a retraction delivered
before the assertions it ends has nothing in the projection to cut and leaves nothing behind, so
it is still lost. Neither reference feeder can produce that — both emit a retraction only for
something they have already asserted. With the swap survived, `k8s.DefaultReorderingWindow` went
back to 60 s; `otel.DeclaredReorderingWindow` stays 30 s because that is what its own flush
behaviour supports, not because of the graph.

## 6. Public API

**Decision**: ConnectRPC (`connectrpc.com/connect`) serving gRPC, gRPC-Web and
JSON-over-HTTP from one protobuf definition, on one port, in the same binary. Schema in
`api/sreagent/graph/v1/graph.proto`, managed with `buf` (lint, `breaking` against `main`,
generated docs). OpenAPI 3 rendered from the proto for the JSON surface.

**Rationale**: the owner asked for "gRPC + JSON gateway or equivalent"; ConnectRPC removes the
gateway process and the duplicated annotations, and is curl-friendly for SREs.
**Alternatives**: grpc-go + grpc-gateway (two servers, more code, rejected); REST-only with
OpenAPI (loses streaming ingestion and typed clients, rejected); GraphQL (poor fit for
as-of/diff semantics and for typed event ingestion, rejected).

## 7. Authentication and authorisation

**Decision**: OIDC bearer tokens validated against a configured issuer (the organisation's
IdP). Principal = `(issuer, subject, email)`. Two roles mapped from a configurable claim:
`reader` (all queries), `decider` (resolution decisions; implies reader). Feeders authenticate
with a `feeder` role token scoped to their `source_id`. For fixtures, CI and laptops, a
`--auth=dev` provider issues signed tokens for named test users from a local file; it refuses
to start unless `--dev` is set and logs a banner.

**Rationale**: FR-041/041a require individual identity from v1; OIDC is what every
organisation already has. **Alternatives**: shared token (owner rejected); local user store
(more code, worse security); mTLS client certs (poor UX for humans).

## 8. Traffic weight classes

**Decision**: `calls` edges carry `weight_class` ∈ {0: observed-but-negligible, 1: < 0.1
rps, 2: < 1 rps, 3: < 10 rps, 4: < 100 rps, 5: ≥ 100 rps} computed per aggregation window
(default 5 min) from span counts; a new version is emitted only when the class changes
(hysteresis: one full window at the new class). Raw counts are never stored (ADR-0001 D1).

## 9. Ranking score (published, FR-028)

**Decision**: for a change `c` with hop distance `h` to the focus node (via changed-by
target), time distance `Δt = T_ref − c.valid_start` (seconds, 0 if inside window but after
T_ref), and path weight class `w` (max class along the heaviest path, 0 for unattached):

```
temporal   = exp(−Δt / τ)              τ = 1800 s by default (caller-adjustable)
topological= 1 / (1 + h)                unattached: h = hop_cap + 1
traffic    = (1 + w) / 6
score      = 0.5·temporal + 0.35·topological + 0.15·traffic
```

All components returned with the item. Tie-break: lexicographic `change_id`. Edges other
than `calls` have no weight class; on a path they contribute class 0 to `traffic` and to the
impact query's "heaviest path", so impact ordering falls back to hop distance for them. No learned
weights in v1; SC-005 (top-3 ≥ 90 % on regression fixtures) decides whether to tune.

## 10. Entity resolution rules (published, FR-037)

**Certain rules** (auto-merge):
- C1: identical `(namespace, value)` claim from two sources.
- C2: Kubernetes workload with annotation/label declaring the OTel service name (configurable
  keys, default `app.kubernetes.io/name` and `resource.opentelemetry.io/service.name`) equal
  to an observed `otel.service.name` claim in the same `k8s.namespace` ↔
  `service.namespace` and same environment.
- C3: OTel resource attributes `k8s.deployment.name` + `k8s.namespace.name` present on spans
  and equal to a Kubernetes workload claim.

**Probable rules** (suggest only): P1 normalized-name equality (`checkout-svc` ≈ `checkout`
after stripping configured suffixes) → 0.7; P2 same normalized name and same environment →
0.85; P3 shared owner + P1 → 0.8. Scores are documented constants; calibration is reported
from fixtures where ground truth is known.

**Type resolution on merge**: a Kubernetes workload and the OpenTelemetry service running on
it are *one entity under two names* (the brief's core claim), so C2/C3 merge them rather than
linking them with an edge; this keeps rollouts, scaling and config changes at hop 0 from the
service the alert names. The merged node's `type` is chosen by a published precedence,
`SERVICE > WORKLOAD > THIRD_PARTY > INFRA_RESOURCE > DB_SCHEMA > CONFIG > FEATURE_FLAG >
OWNER > ALERT > CHANGE`, and every asserted type is kept in `facets`. A type change caused by
a merge is a new version (bitemporal, auditable). Two entities whose asserted types are both
below WORKLOAD and differ (e.g. CONFIG vs OWNER) are never auto-merged by probable rules.

**Precedence**: human decision > certain rule > probable suggestion. A human `reject` blocks
future automated merges of that pair permanently; a human `confirm` merges and pins.

## 11. OTLP receiver for the topology feeder

**Decision**: implement OTLP/gRPC and OTLP/HTTP (protobuf and JSON) trace endpoints in-process
using `go.opentelemetry.io/proto/otlp` types only. Per window, derive per `(service,
peer_service | db.system+server.address | http host)` edge counts; emit `UpsertNode`,
`UpsertEdge` (with weight class), `IdentityClaim`, and `ObserveChange(kind=rollout)` on
`service.version` transitions. An edge not observed for `--retract-after` windows (default 6, i.e. 30 min) gets a
`RetractEdge` with `valid_end` = end of the last window in which it was seen (FR-042). Spans
are dropped at window end. Sampling is tolerated (weight
classes are coarse); `sampling.probability` attribute, if present, scales counts.

The **declared reordering window is a constant, 30 s** (`otel.DeclaredReorderingWindow`), and
deliberately not `--window`. The feeder emits window by window in window-start order and never
reorders; the only events a consumer must tolerate out of order are the ones inside a single
flush. Declaring the aggregation window instead used to license swapping a `RetractEdge` with a
re-assertion of the same edge one window later, which the projector did not survive; it does now
(§5, "Order-independent retraction"), and the declaration stays 30 s because that is what the
feeder's own flush behaviour supports. The aggregator's own lateness
allowance — how long a window is held open for straggling spans — stays the aggregation window;
the two numbers describe different things.

**Alternatives**: embedding the OpenTelemetry Collector receiver framework (heavy dependency
tree, rejected); pulling a service map from a vendor API (vendor-specific; becomes the Datadog
connector in feature 002).

## 12. Kubernetes feeder

**Decision**: `client-go` shared informers on Deployments, StatefulSets, DaemonSets, Jobs,
CronJobs, Services, Ingresses, ConfigMaps, Secrets (metadata only), Nodes, Namespaces.
- Workload upsert with props (`k8s.*` OTel conventions), `runs-on` → cluster and node pool
  (from node labels), `exposed-via` → Service/Ingress nodes, `owned-by` → owner nodes from
  configurable label keys, `depends-on` → config nodes referenced by env/volumes.
- Change nodes: `rollout` when pod template hash / `deployment.kubernetes.io/revision`
  changes (actor from `kubernetes.io/change-cause` when present); `scaling` on replica delta
  (HPA or manual, distinguished by managedFields when available); `config_change` when a
  referenced ConfigMap/Secret `resourceVersion` changes (value hash for ConfigMaps, version
  only for Secrets).
- Sequence = `resourceVersion` per object; checkpoint per resync. The reordering window was
  researched as 60 s and is **5 s as implemented** (`k8s.DefaultReorderingWindow`): the feeder
  reads its informers from one goroutine and never reorders, so what it must promise is the
  emitter's flush interval plus a whole initial-list burst, not a guess about the watch. 60 s
  was wide enough to license swapping a reconnection retraction with a re-assertion of the same
  object seconds later, which the projector does not survive (`fixtures/feeder-gap-01`, T080-T084).
- Startup permission check via `SelfSubjectRulesReview`; refuses if any write verb is granted
  on watched resources (FR-046).
- Recorded mode: replays saved watch events (`payloads/*.jsonl`) through the same handler.

## 13. Fixture format and verification

**Decision**: one directory per fixture: `manifest.yaml` (id, description, schema version,
sources, reordering windows, ground truth for regression families), `payloads/<source>/`
(recorded raw payloads), `events.jsonl` (canonical event stream as accepted, with recorded
observed times), `golden/<query>.json` (canonical outputs for every query in section D at
listed instants). `aisre fixture verify` runs replay, double-delivery and shuffle; `fixture
record` regenerates goldens (reviewed in PR diffs).

## 14. Migrations and the append-only guarantee

**Decision**: SQL migrations embedded in the binary (`aisre migrate`). The application
role has `INSERT, SELECT` on `log.events` and no `UPDATE/DELETE`; `graph.*_versions` allow
`INSERT` and the single `UPDATE` of `observed` upper bound via a `SECURITY DEFINER` function.
Migrations that would drop history are refused by a CI check on migration files.

## 15. Self-observability

**Decision**: OTel Go SDK with OTLP exporter; metrics and spans listed in plan.md. Trace
context propagated from feeders into the log as `trace_id` on each event row for
end-to-end debugging (provenance, not telemetry payload).

## 16. Testing strategy

- Unit: interval algebra, version splitting, ranking, rules, canonical serialization.
- Property (`rapid`): order-independence of valid-time state; idempotency; merge/split
  round-trip.
- Replay/golden: all fixtures (FR-047/048).
- Contract: buf lint/breaking; JSON and gRPC parity tests on ConnectRPC handlers.
- Benchmark: section 2.
- Live e2e nightly: kind + OpenTelemetry Demo, record → compare (FR-050 b).

## 17. Repository conventions

Apache-2.0, SPDX headers, DCO, `CONTRIBUTING.md`, `CODEOWNERS`, `SECURITY.md`, ADRs in
`docs/decisions/`, conventional commits, semver tags for the binary and a separate
`schema/vX.Y.Z` tag for the proto package.

## Resolved unknowns from Technical Context

All fields in plan.md Technical Context are filled; no NEEDS CLARIFICATION remains.
Deferred to implementation (non-blocking): exact GiST vs. SP-GiST choice (benchmark), default
fan-out caps (start at 50 per hop, 500 total), aggregation window default (5 min).

Risk noted for FR-016 (unlimited history): `log.events` and `*_versions` are declared
range-partitionable by `observed_at` / `lower(observed)` from the first migration so that
partitioning by month can be enabled later without a rewrite; the benchmark generator's 90-day
span is the v1 validation horizon, not a limit.
