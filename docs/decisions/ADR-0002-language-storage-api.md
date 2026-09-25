# ADR-0002: Go, PostgreSQL, ConnectRPC for the graph core

- Status: proposed (accepted on plan approval for feature 001); all benchmark-gated exit criteria met as of 2026-09-17
- Date: 2026-09-15
- Supersedes: none. Details and alternatives: `specs/001-temporal-graph-core/research.md` §1, §2, §6.

## Decision

- **Language**: Go, single static binary (`CGO_ENABLED=0`).
- **Storage**: PostgreSQL 16+ as append-only event log and materialized bitemporal graph
  (range types + GiST; traversal in-process over as-of adjacency slices). No dedicated graph
  database.
- **API**: ConnectRPC serving gRPC, gRPC-Web and JSON from one protobuf schema managed with
  buf.
- **Identity**: OIDC bearer tokens; dev provider for fixtures and CI.

## Benchmark-gated exit criteria (constitution X)

Reopen the storage decision only if, after index tuning on the reference workload (10k
entities / 100k edges / 1M events), the 2-hop subgraph as-of p95 exceeds 2 s, the 24 h diff
p95 exceeds 4 s, or a full replay of 1M events exceeds 60 min. First candidate to evaluate
then: Apache AGE (keeps a single service).

**Status of the criteria, 2026-09-17: all three met; none reopens the decision.**

| criterion | target | measured | where |
|---|---|---|---|
| 2-hop subgraph as-of, p95 | < 2 s | 144 ms | [`2026-09-16-scale-10k.md`](../benchmarks/2026-09-16-scale-10k.md) |
| 24 h diff, p95 | < 4 s | 488 ms | same run |
| full replay of 1M events | < 60 min | **37 min 44 s** | [`2026-09-17-replay.md`](../benchmarks/2026-09-17-replay.md) |

The replay criterion was **open** between 2026-09-16 and 2026-09-17 and is now closed. It was
missed by 4.5× on 2026-09-16, and the honest reading at the time was already that the miss
belonged to the replay loop rather than to PostgreSQL: the same events ingested through the same
projector ran an order of magnitude faster, because ingestion batched 500 events per transaction
and `Projector.Replay` committed one per event. Batching the replay loop — and bounding its own
paged read of the log, which turned out to cost more than the commits — closed it on the same
store, the same schema and the same indexes, with no new index and no change to what a replay
produces. Nothing here revises the decision; it removes the one measurement that could have.

## Consequences

- One external service to operate (Postgres); justified in plan.md Complexity Tracking.
- Public surface is the protobuf schema, not Go types; `pkg/feeder` is the only public Go API.
- An embedded store for laptops (`--store=sqlite`) is explicitly deferred, not rejected.
