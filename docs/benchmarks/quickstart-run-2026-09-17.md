<!-- SPDX-License-Identifier: Apache-2.0 -->

# Quickstart run, 2026-09-17 (T092)

[`specs/001-temporal-graph-core/quickstart.md`](../../specs/001-temporal-graph-core/quickstart.md)
walked end to end, §1–§6 and §8, literally as written, against the compose PostgreSQL. §7 was
**not** run: it creates a Kubernetes cluster, and the run that matters was already done and
recorded on 2026-09-16. Every drift this found is fixed in the quickstart; this page is what was
run, what it took, and what was wrong.

FR-050 (a) — every fixture-based verification passing in automation — is closed by §5 below.
FR-050 (b) — the live run — is closed by
[`live-run-2026-09-16.md`](./live-run-2026-09-16.md), not by this page.

## Environment

| | |
|---|---|
| Machine | Apple M5, 10 logical cores, 16 GiB, darwin/arm64 |
| Go | go1.27.1, `CGO_ENABLED=0` |
| Binary | `bin/aisre`, built from 47894bb (dirty: the T089/T090 changes) |
| PostgreSQL | 16.13 in Docker (`deploy/docker-compose.yml`), container `sre-agent-postgres` |
| DSN | `postgres://sreagent:sreagent@localhost:5432/<db>?sslmode=disable` |
| kind cluster | none, deliberately |

**Contention.** A second agent was running a replay benchmark against the same PostgreSQL
container for part of this run. Query latencies and the §8 numbers are therefore an upper bound,
not a clean measurement. They passed anyway, which is the only claim being made.

**Databases.** §2, §3 and §4 each want a graph that has seen only their own fixture, so each ran
in a database of its own, created with `docker exec sre-agent-postgres createdb -U sreagent
qs_s2` (and `qs_s3`, `qs_s4`), migrated, and served on `127.0.0.1:8080` one at a time. §5 and §8
used the shared `sreagent` database, whose role has `CREATEDB` — `fixture verify` and `fixture
bench` each create and drop databases of their own and never touch the one the DSN names.

## What was run

| § | Step | Duration | Result |
|---|---|---:|---|
| 1 | `make build` | 4.6 s | `bin/aisre` |
| 1 | `migrate` (per database) | 0.9 s | 4 migrations applied |
| 1 | `serve --auth dev --dev` | — | dev banner, `listening addr=127.0.0.1:8080` |
| 1 | `curl /healthz`, `/readyz` | — | `ok` 200, `ready` 200 |
| 1 | `dev-token --dev --user alice --roles reader,decider` | — | 289-byte JWT |
| 2 | `fixture load fixtures/rollout-regression-01` | 0.21 s | 95 applied, 0 rejected |
| 2 | `query subgraph … --hops 2` | 0.03 s | 16 nodes, 8 edges |
| 2 | `query diff` | 0.03 s | 4 ranked changes, culprit rank 1 |
| 2 | `query impact` | 0.03 s | 2 downstream, 7 upstream, nothing truncated |
| 2 | `query pointers` | 0.03 s | metric, log, trace, source-link |
| 3 | `fixture load fixtures/late-arriving-fact-01` + 2 subgraph queries | 0.3 s | late edge present without `--observed-at`, absent with it |
| 4 | `fixture load fixtures/ambiguous-identity-01` | 0.2 s | loaded |
| 4 | `resolve suggestions` | 0.03 s | 3 rows: 1 conflict, 1 pending, 1 confirmed |
| 4 | `resolve confirm` + `resolve why` | 0.06 s | decision recorded as `sre-agent-dev\|alice`, both claims and both decisions in order |
| 5 | `fixture verify fixtures/*/ --report --db …` | **15.1 s** | **8 of 8 passed**, 0 failed |
| 5 | `make test` (`go test ./...`) | **38.1 s** | **18 packages ok**, 0 failures |
| 6 | `feed k8s --replay … --dry-run --output json` | 0.02 s | 88 events, ids equal to the 63 recorded, all 63 envelopes byte-identical |
| 6 | `go test ./internal/feeders/k8s/...` | — | green (inside `make test`) |
| 7 | — | — | **not run**; recorded evidence linked instead |
| 8 | `fixture bench --scale 1k --queries 200` | **20 m 18 s** | **PASS** |

`fixture verify` and `make test` were run after the projector agent's `/tmp/live/PROJECTOR_DONE`
marker appeared, so they ran against the re-recorded goldens.

### §5 in detail

Every fixture passed all four steps — replay from empty against its goldens, double delivery,
six seeded shuffles inside the declared reordering window, and the expect-rejected contract:

| fixture | events | goldens | notes |
|---|---:|---:|---|
| `ambiguous-identity-01` | 19 | 22 | resolution calibration reported |
| `baseline-topology-01` | 89 | 14 | 1 event refused with its stated reason code, graph unchanged |
| `config-change-01` | 93 | 6 | |
| `feeder-gap-01` | 106 | 8 | |
| `late-arriving-fact-01` | 15 | 4 | |
| `retraction-with-edges-01` | 19 | 6 | |
| `rollout-regression-01` | 95 | 14 | |
| `rollout-regression-02` | 163 | 4 | 4 automated merges, all from certain rule C2 |

Ranking (SC-005): the culprit ranked **first** in every scenario.

| fixture | query | culprit | rank |
|---|---|---|---|
| `rollout-regression-01` | `checkout-diff` | `k8s.change=shop/payments@rev7` | 1 of 4 |
| `rollout-regression-01` | `checkout-diff-1h` | `k8s.change=shop/payments@rev7` | 1 of 3 |
| `rollout-regression-02` | `checkout-diff` | `k8s.change=shop/payments@rev2` | 1 of 2 |

### §8 in detail

`fixture bench --scale 1k --queries 200`, written to `2026-09-17-scale-1k.md`. Not committed to
`docs/benchmarks/`: a laptop run under contention is not the reference measurement, and the
weekly `bench.yml` artifact is.

| source | criterion | target | measured | result |
|---|---|---|---|---|
| SC-001 | subgraph 2-hop as-of, p95 | < 1.00 s | 54 ms | pass |
| SC-002 | diff over 24 h with ranking, p95 | < 2.00 s | 356 ms | pass |
| ADR-0002 | subgraph 2-hop as-of, p95 (2× exit criterion) | < 2.00 s | 54 ms | pass |
| ADR-0002 | diff over 24 h, p95 (2× exit criterion) | < 4.00 s | 356 ms | pass |

Workload: 5,880 entities, 37,428 edge versions, 96,911 entity versions, 25,880 identity claims,
99,509 events over a 90-day valid-time span, 236.8 MiB. Ingestion 438 events/s; full replay 490
events/s at 500 events per transaction. Every query family's p95: `subgraph-2hop-asof` 54 ms,
`subgraph-3hop-capped` 80 ms, `impact` 100 ms, `diff-1h` 184 ms, `diff-7d` 203 ms, `diff-24h`
356 ms.

## Drift found and fixed

Nine, and three of them would have stopped a reader on their first command.

1. **§1 `serve --listen :8080` no longer starts.** The T090 security pass made `--auth dev`
   loopback-only, because the dev signing key is a published constant and a dev server on a
   routable address mints any identity for anyone who can reach the port. The quickstart now
   says `--listen 127.0.0.1:8080` and names `--dev-insecure-listen` as the deliberate escape.
   *(New drift, introduced by this pass; fixing the quickstart is part of the fix.)*
2. **§4 `resolve confirm … k8s.deployment=demo/checkout-svc` fails.** That ref does not exist in
   `ambiguous-identity-01`; the graph refuses it with `ref_unresolvable`, correctly — "a decision
   may not create an entity". The real pending pair is `otel.service.name=notifications` ↔
   `k8s.deployment=ops/notifications-svc`.
3. **§6 `--replay fixtures/baseline-topology-01/payloads/k8s` does not exist**, `--source-id` is
   required, and pointing `--replay` at the whole fixture fails with `decode traces payload:
   invalid character '\xb3'` — that fixture holds both sources' payloads and the k8s feeder
   cannot decode an OTLP protobuf export. The quickstart now uses
   `internal/feeders/k8s/testdata/kind-shop`, that fixture's Kubernetes half.
4. **§6 needs `--cluster-name` and `--namespaces`** to reproduce a recording. Kubernetes does not
   know its own name, so a replay without `--cluster-name sre-agent-demo` emits the cluster as
   `replay` and nothing matches. And the recording was made with `--namespaces shop`: without it
   the feeder's checkpoint note reads "initial informer list complete for all namespaces" instead
   of "… for namespace shop", and exactly one event of 63 stops matching. With both flags the
   replay is 63 of 63 byte-identical. The lesson is worth more than the flags: reproducing a
   recording means reproducing the feeder's *configuration*, not only replaying its payloads.
5. **§6's expected output was wrong in an interesting way.** The dry run prints 88 events where
   the recording has 63 lines. Both are right: a recording keeps only the events the graph
   answered for, so re-assertions that would be `DUPLICATE_NOOP` appear in the dry run and not in
   `events.jsonl`. The ids are equal and 62 of 63 envelopes are byte-identical.
6. **§2's expected nodes named `payments`.** The rollout renames the service, so the node is
   `payments` before 14:20 and `payments-v2` after — which is itself worth seeing, and is why
   `service.name` shows up among the diff's changed properties. The alias still resolves
   (FR-039). The ranking claim was *correct*: rank 1, `hop_distance=1`,
   `time_distance_seconds=720`.
7. **§4's expected output said "one pending suggestion".** There are three, and the mix is the
   fixture's point: a `C3` conflict blocked by a human decision, a `P1` pending, and a `P1`
   already confirmed. It also said the decision would be attributed to `dev:alice`; the principal
   key is `issuer|subject`, so it renders `sre-agent-dev|alice`.
8. **§5 `fixture verify fixtures/*` needs `--db` and a trailing slash**, and its role needs
   `CREATEDB` because every fixture and every shuffle permutation gets a database of its own.
   `make verify` is the same command with the DSN defaulted.
9. **§8 `--out docs/benchmarks/$(date +%F).md` names a directory, not a file** — the report is
   written into it as `<date>-scale-<scale>.md` plus the `.json` the numbers come from. The
   "smoke test" also takes twenty minutes, not one: `1k` is the size of the workload, and the
   generator still writes and replays ~100,000 events.

Also fixed: the prerequisites listed one machine's toolchain state from 2026-09-15; §3 and §4
never said they need a fresh database, though their expected output assumes one; and §7 now says
plainly not to run it, pointing at the recorded evidence and the nightly workflow instead.

## Security checks made during the run

Part of T090, verified here against a live PostgreSQL 16 rather than asserted:

- The read-only role documented in [`deploy/README.md`](../../deploy/README.md) was created with
  the `GRANT` statements exactly as published. `select count(*) from log.events` → 84.
  `insert into log.sources …` → `ERROR: permission denied for table sources`. The roles were
  dropped afterwards.
- `serve --auth dev --dev --listen :8080` is refused with a message naming
  `--dev-insecure-listen`; on `127.0.0.1:8080` it starts and logs the
  `DEV AUTHENTICATION ENABLED` banner.
- `SRE_AGENT_TOKEN=<canary> aisre --help` printed the token before this pass and does not
  now.
- `/healthz` answers `ok` and `/readyz` answers `ready`, with no database detail in either body.

## Open

- `feed k8s --replay` on a fixture holding more than one source's payloads fails on the first
  payload of a kind it does not own, instead of skipping it. The quickstart routes around it;
  the feeder should filter by payload kind. Owned by the feeder work, not fixed here.
- `docs/decisions/ADR-0002` status is not changed by this run. Its exit criteria passed at `1k`
  under contention on a laptop; the reference workload is `10k` on a clean machine, and the
  weekly `bench.yml` artifact is the evidence that should move the status.
- The secrets scanner's baseline (`scripts/known-recording-findings.txt`) accepts the
  applied-configuration annotations in the kind recordings rather than eliminating them. The
  recorder should strip that annotation at record time; see
  [`docs/security.md`](../security.md).
