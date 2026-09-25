<!-- SPDX-License-Identifier: Apache-2.0 -->

# Benchmarks

This directory holds the evidence behind the storage decision.

Constitution X does not allow a dedicated graph database to be introduced "unless the
implementation plan demonstrates, with a reproducible benchmark on the reference workload
(~10k nodes / 100k edges / 1M events), that the recommended general-purpose store cannot meet
the query budget for subgraph-as-of and diff queries." [ADR-0002](../decisions/ADR-0002-language-storage-api.md)
chose PostgreSQL on that basis and wrote down the numbers that would reopen the question. The
files here are the runs that answer it.

## What is in here

| file | what it is |
|---|---|
| `<date>-scale-10k.md` | a run of the reference workload, human-readable |
| `<date>-scale-10k.json` | the same run, for a trend line or a diff |
| `<date>-scale-1k.md` | the same harness at a tenth of the size, when a run is worth keeping |
| `<date>-replay.md` | a replay measurement on its own (`--replay-only`), when the replay is what moved |
| `live-run-2026-09-16.md` | the live-versus-recorded run (FR-050 b, SC-010) — not a performance benchmark |
| `investigation-page-profile-2026-09-18.md` | the `page` profile end to end (T117, SC-005, SC-013): the three latencies, the hard stop, ledger recomputation and trajectory replay, and the evidence that the first wave rather than the model carries the 120-second target |

Reports are committed by a person, not by CI. `bench.yml` uploads its report as an artifact and
renders it into the job summary; a maintainer downloads the ones worth keeping and commits them
with the machine they were measured on. A bot pushing here would turn the history of an
architecture decision into the history of a CI queue.

## Running it

```sh
aisre fixture bench \
  --db "$PG_DSN" \
  --scale 10k \
  --seed 1 \
  --queries 1000 \
  --out docs/benchmarks
```

The role in `--db` needs `CREATEDB`: every run works in databases of its own, created and
dropped by the run, and never reads or writes the one the DSN names. Two are used — one for the
ingestion and the queries, one for the replay measurement, which by definition has to start from
an empty projection.

Useful flags: `--scale 1k` for a run that finishes in minutes, `--skip-replay` to leave out the
longest single step and `--replay-only` to measure *nothing but* that step, `--replay-batch N`
for the events per transaction it uses, `--keep-db` to keep the generated databases so a query
can be profiled with `EXPLAIN ANALYZE`, and `--note` for a line about the machine in the
report's environment table.

Exit code 4 means a published criterion was missed — the same code `fixture verify` uses for a
regression, so CI does not have to parse the report to know.

There is also a `1k` smoke test inside `go test`, guarded so it does not run by default:

```sh
SRE_AGENT_BENCH=1 PG_DSN=... go test ./internal/fixture -run TestBenchSmoke -v
```

It measures nothing anyone should quote. What it proves is that the harness still works — that
the generator produces the shape it claims, that every query family answers, that the report
evaluates its criteria — because the week the benchmark is needed is the week a silently broken
one would be discovered.

## The workload

Generated deterministically from `--seed` (research §2):

| | 10k | 1k |
|---|---:|---:|
| entities | 10 000 | 1 000 |
| relationships | 100 000 | 10 000 |
| change nodes | 50 000 | 5 000 |
| identity claims | 200 000 | 20 000 |
| events | 1 000 000 | 100 000 |
| valid-time span | 90 days | 90 days |

The relationship count is an upper bound rather than an exact figure: the generator refuses a
duplicate `(src, dst, type)` triple, and preferential attachment makes collisions common once
the hubs have formed, so a run lands a few percent short — 95 222 of 100 000 at `10k`, seed 1.
The report prints what was asked for and what the graph ended up holding, side by side, so the
difference is never hidden. Every other count is exact.

Three properties of the shape matter more than the totals.

**The degree distribution is a power law**, built by preferential attachment, so the graph has
hubs — the shared database, the auth service — the way a real estate does. A uniform graph would
make every 2-hop neighbourhood the same size and the p95 meaningless.

**Queries focus on the interesting nodes.** Every randomized query picks its focus from the
services plus the 200 highest-degree entities, not uniformly from the population: a uniform draw
would spend most of its samples on leaves with two neighbours.

**The graph has been through entity resolution.** Claims from two synthetic sources —
`bench:k8s` declaring service names on workloads, `bench:otel` observing them on spans — make
certain rule C2 merge about 30% of services, so every read follows the merge redirects rather
than reading a graph that was never resolved.

The as-of instant of every query is drawn from the whole 90-day span, not from the end of it: a
question about last Tuesday has to look through every version written since, and that is the
bitemporal cost the storage decision is being judged on.

## What is measured

Per research §2, p50/p95/p99 over 1,000 randomized queries of each family:

| family | what it is |
|---|---|
| `subgraph-2hop-asof` | 2 hops, both directions, random focus, random valid instant |
| `subgraph-3hop-capped` | 3 hops with the published caps (50 per hop, 500 total) |
| `diff-1h`, `diff-24h`, `diff-7d` | diff over a 2-hop neighbourhood with ranked changes |
| `impact` | blast radius, weight-ranked |

plus ingestion throughput (append and project, one transaction-worth of events at a time), full
replay wall time (project only, from the log into an empty projection) and the resulting
database size.

## The thresholds

| source | criterion | gate? |
|---|---|---|
| SC-001 | 2-hop subgraph as-of, p95 < 1 s | yes |
| SC-002 | 24 h diff with ranking, p95 < 2 s | yes |
| ADR-0002 | the same two at 2× (2 s and 4 s), and a full replay under 60 min | yes |
| plan.md | sustained ingestion ≥ 500 events/s, replay < 30 min | reported, not a gate |

The first two are the product promise. The third is the line that reopens the storage decision:
missing an ADR-0002 criterion *after index tuning* is not a performance bug to be quietly tuned
away, it is a finding that says "evaluate Apache AGE", and the report says so in those words.

## Closed finding: replay meets its budget at 1M events

The `2026-09-16` run at `10k` skipped the replay step, and the reason was a finding worth keeping
here in full, because it is the shape of mistake this directory exists to catch.

A separate replay of the same 995 222-event log, measured in the same session on the same
machine, was still at 66% (638 471 of 969 459 entity versions) after **4 h 29 min**, projecting
about 39 events/s. The ADR-0002 criterion is 60 minutes and plan.md's goal is 30; both were
missed, by roughly 4.5× and 9×, and the run was stopped rather than carried to a number that
could only be worse.

It was never a storage finding, and it must not be read as one. Ingestion of the *same* events —
append plus project, through the same projector — ran at 435 events/s in the same run, an order
of magnitude faster. The difference was not the queries and not the indexes: `Projector.Replay`
opened **one transaction per event**, so a million events were a million commits, while the
benchmark's ingestion path batches 500 events into each transaction.

**Closed on 2026-09-17**: the same log, the same store, the same indexes, replayed in
**37 min 44 s** at 440 events/s. The ADR-0002 gate is met with 22 minutes to spare; plan.md's
30-minute goal is missed by about 8 and stays a goal. The numbers, the profile and the second
bug that batching alone did not fix — the replay's own page read was unbounded, so the driver
drained the rest of the log on every page, which is where most of the win turned out to be — are
in [`2026-09-17-replay.md`](2026-09-17-replay.md).

Measure it with `--replay-only`, which writes the generated log to a database of its own and
replays it into an empty projection without paying for the ingestion and the 6 000 queries:

```sh
aisre fixture bench --db "$PG_DSN" --scale 10k --seed 1 --replay-only
```

`--replay-batch N` sets the events per transaction (default 500); `--replay-batch 1` reproduces
the 2026-09-16 loop, and nothing but the wall time changes — the fixtures are replayed at batch
sizes 1, 7 and 500 in CI and must produce byte-identical snapshots.

## Reading a regression

A number that has moved is a question about a query plan, not a reason to change the storage. The
sequence that found the index in migration `0004` was:

1. run with `--keep-db`, so the generated database survives the run;
2. turn on `log_min_duration_statement` and find which statement the time is actually in;
3. `EXPLAIN (ANALYZE, BUFFERS)` that statement and look at which index it used, and at how many
   buffers each side of a `BitmapOr` read;
4. fix the index, re-run the same seed and the same query count, and report both numbers.

In that case the incoming half of the adjacency slice — `dst_id = ANY($frontier) AND valid @> $t`
— was being served by the exclusion-constraint index with `dst_id` as a non-leading column, and
was reading 121 204 buffers to the outgoing half's 1 428. One GiST index led by `dst_id` took the
statement from 232 ms to 8 ms and the 24 h diff p95 at `1k` from 1 340 ms to 237 ms.
