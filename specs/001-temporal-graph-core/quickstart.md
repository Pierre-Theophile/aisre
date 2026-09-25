# Quickstart: run and validate the temporal graph core

Validation guide for feature 001. Commands refer to [contracts/cli.md](./contracts/cli.md).

Walked end to end against the compose PostgreSQL on 2026-09-17; the run, its timings and the
drift it found are in
[`docs/benchmarks/quickstart-run-2026-09-17.md`](../../docs/benchmarks/quickstart-run-2026-09-17.md).

## Prerequisites

- Go (latest stable, ≥ 1.25), Docker (for Postgres via compose and testcontainers).
- `buf` (schema lint/generate). Optional: `kind` and `kubectl` for the live run of §7.
- Install what you are missing with
  `brew install go bufbuild/buf/buf kind kubectl libpq`.

Sections §2, §3 and §4 each assume a graph that has seen only their own fixture. Give each one a
database of its own — the commands below do — or the node counts and the ranking will not match
what is written here.

## 1. Build and start

```sh
make build                                   # → bin/aisre
docker compose -f deploy/docker-compose.yml up -d postgres

export PG_DSN='postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable'
bin/aisre migrate --db "$PG_DSN"

# --auth dev signs tokens with a published key, so it binds loopback only. Pass
# --dev-insecure-listen if you really mean to expose it (docs/security.md).
bin/aisre serve --db "$PG_DSN" --listen 127.0.0.1:8080 --auth dev --dev &
export SRE_AGENT_TOKEN=$(bin/aisre dev-token --dev --user alice --roles reader,decider)
```

`serve` applies no migrations unless you pass `--migrate`; without either it starts and reports
not-ready rather than migrating a database an older binary might have to read.

Expected: `serve` logs the dev-auth banner (`DEV AUTHENTICATION ENABLED`) and
`listening addr=127.0.0.1:8080`; `curl 127.0.0.1:8080/healthz` → `ok`, `/readyz` → `ready`.

## 2. Load a fixture and query it (User Stories 1, 2, 4, 5)

```sh
bin/aisre fixture load fixtures/rollout-regression-01        # 95 events applied
bin/aisre query subgraph otel.service.name=checkout --as-of 2026-09-01T14:32:00Z --hops 2
bin/aisre query diff otel.service.name=checkout --from 2026-09-01T13:00:00Z --to 2026-09-01T14:32:00Z
bin/aisre query impact otel.service.name=payments --as-of 2026-09-01T14:32:00Z
bin/aisre query pointers otel.service.name=payments --as-of 2026-09-01T14:32:00Z
```

Expected:

- **subgraph**: 16 nodes and 8 edges around `checkout` — `storefront`, `inventory`,
  `payments-v2`, the third parties `payments-db`, `redis` and `stripe`, the owners
  `team-checkout` and `team-payments`, the cluster and node pool, both Services and the Ingress
  — each with its valid interval.
- **diff**: four ranked changes. Rank 1 is the `payments` rollout to revision 7, score ≈ 0.61,
  `hop_distance=1`, `time_distance_seconds=720` (rendered as `12m0s`). Rank 3 is the
  `checkout-worker` rollout, marked `unattached` at hop 4 — a decoy that is near in time and far
  in topology, which is the point of the fixture.
- **impact**: two downstream dependents (`checkout` at hop 1, `storefront` at hop 2 by the
  heaviest path) and seven upstream dependencies.
- **pointers**: metric, log, trace and source-link pointers, selectors in OTel semantic
  conventions.

Note that the rollout **renames** the service: the node is `payments` before 14:20 and
`payments-v2` after, which is why the diff shows `service.name` among the changed properties.
`otel.service.name=payments` still resolves to it — an identifier that was ever valid stays
resolvable (FR-039).

## 3. Bitemporal check (Story 1 scenario 4, Story 7)

A fresh database, so that the late-arriving fact is the only thing in it:

```sh
createdb -h localhost -U sreagent qs_late          # or: docker exec sre-agent-postgres createdb -U sreagent qs_late
export PG_DSN='postgres://sreagent:sreagent@localhost:5432/qs_late?sslmode=disable'
bin/aisre migrate --db "$PG_DSN"
bin/aisre serve --db "$PG_DSN" --listen 127.0.0.1:8080 --auth dev --dev &

bin/aisre fixture load fixtures/late-arriving-fact-01
bin/aisre query subgraph otel.service.name=checkout --as-of 2026-09-01T14:32:00Z
bin/aisre query subgraph otel.service.name=checkout --as-of 2026-09-01T14:32:00Z --observed-at 2026-09-01T14:32:00Z
```

Expected: the first includes the late-learned edge `checkout -> inventory-search`; the second,
asking what was *known* at 14:32, excludes it. Same valid instant, two different answers — that
is the whole of principle II in two commands.

## 4. Entity resolution (Story 6)

Again on a database of its own:

```sh
createdb -h localhost -U sreagent qs_identity
export PG_DSN='postgres://sreagent:sreagent@localhost:5432/qs_identity?sslmode=disable'
bin/aisre migrate --db "$PG_DSN"
bin/aisre serve --db "$PG_DSN" --listen 127.0.0.1:8080 --auth dev --dev &

bin/aisre fixture load fixtures/ambiguous-identity-01
bin/aisre resolve suggestions
bin/aisre resolve confirm otel.service.name=notifications k8s.deployment=ops/notifications-svc --reason "same service"
bin/aisre resolve why otel.service.name=notifications k8s.deployment=ops/notifications-svc
```

Expected from `resolve suggestions`: **three** rows, and the mix is the point of the fixture.

| score | rule | status | pair |
|---|---|---|---|
| 1.00 | `C3` | `conflict` | `checkout-legacy` ↔ `checkout` — blocked by a human decision that pins one of them |
| 0.70 | `P1` | `pending` | `otel.service.name=notifications` ↔ `k8s.deployment=ops/notifications-svc` |
| 0.70 | `P1` | `confirmed` | the checkout pair the fixture already decided |

Zero automated merges from a probable rule (SC-007): `P1` is probable, so it can only suggest.

After the confirm, `why` answers `are the SAME entity`, lists both identity claims with the
source and observed time that produced them, and shows two decisions in order: the `P1`
suggestion at score 0.70, then the human `confirm` at score 1.00 by `sre-agent-dev|alice`. The
principal key is `issuer|subject`; the `sre-agent-dev` issuer is deliberately not a URL, so a
dev identity can never be mistaken for one from a real provider.

## 5. Verification harness (Story 3, Story 7, FR-048)

`fixture verify` gives every fixture, and every shuffle permutation, a database of its own, so
its role needs `CREATEDB`; the database the DSN names is only the maintenance connection.

```sh
export PG_DSN='postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable'
bin/aisre fixture verify fixtures/*/ --db "$PG_DSN"            # replay + double-delivery + shuffle
bin/aisre fixture verify fixtures/*/ --report --db "$PG_DSN"   # rank of culprit, calibration
go test ./...                                                      # unit, property, contract tests
```

`make verify` is the same thing with the DSN defaulted. Expected: exit 0; the report shows
culprit rank ≤ 3 for every rollout-regression fixture. Exit 4 means a fixture did not match its
goldens.

## 6. Write a feeder from recordings (Story 3, connector author)

```sh
bin/aisre feed k8s --replay internal/feeders/k8s/testdata/kind-shop \
                       --source-id k8s:kind --cluster-name sre-agent-demo \
                       --namespaces shop \
                       --dry-run --output json | head
go test ./internal/feeders/k8s/...                 # uses pkg/feeder/testkit against the fixture
```

Four things the flags are saying, and each of them is a rule a connector author will meet:

- `--source-id` is required. It is the identity every event is stamped with and the one a feeder
  token is scoped to; there is no default because guessing it would let a feeder write under
  somebody else's name.
- `--cluster-name` is required to reproduce a recording, because Kubernetes does not know its own
  name — the feeder cannot derive it, so a replay without it emits `replay` and the events will
  not match what was recorded.
- Point `--replay` at a recording of **one** source. `fixtures/baseline-topology-01` holds both
  the `k8s:kind` and the `otel:kind` payloads, and the k8s feeder cannot decode an OTLP protobuf
  export; `internal/feeders/k8s/testdata/kind-shop` is that fixture's Kubernetes half.
- Reproduce the **configuration** the recording was made with, not just its payloads.
  `--namespaces shop` is what that run watched, and the feeder says so in its checkpoint note; a
  replay that watched everything emits a different note and one event stops matching. The
  fixture's `manifest.yaml` is where a recording's shape is written down.

Expected: 88 events on stdout whose event ids are exactly the 63 in the recording's
`events.jsonl`, all 63 envelopes byte-identical. The counts differ because the dry run prints
every event the feeder emits, while a recording keeps only the ones the graph answered for — a
re-assertion that would be a `DUPLICATE_NOOP` appears here and not there. `go test` runs
`testkit.Conformance`, which makes that comparison for you.

Writing your own connector: [`docs/connectors/writing-a-feeder.md`](../../docs/connectors/writing-a-feeder.md),
and [`docs/connectors/checklist.md`](../../docs/connectors/checklist.md) before the pull request.

## 7. Live run (FR-050 b, SC-010)

**Do not run this as part of a quickstart pass.** It creates a Kubernetes cluster, and the run
that matters has already been done and recorded. Read the evidence instead:

- [`docs/benchmarks/live-run-2026-09-16.md`](../../docs/benchmarks/live-run-2026-09-16.md) — the
  run itself: environment, timeline, the graph built live, the graph replayed from the
  recording of that same run, and every way they differed (§7 is the recorded-equals-live
  comparison that closes FR-050 b).
- `fixtures/baseline-topology-01`, `rollout-regression-02`, `config-change-01` and
  `feeder-gap-01` are the recordings that run produced. They are what CI replays.
- [`.github/workflows/e2e-nightly.yml`](../../.github/workflows/e2e-nightly.yml) does it again
  every night, unattended: kind up, both feeders, the scripted incidents, record, verify.

If you do want to reproduce it by hand, `deploy/kind/README.md` is the walkthrough and
`deploy/kind/up.sh` is the entry point; budget half an hour and a spare CPU.

## 8. Benchmark (research §2)

`--out` names a **directory**; the report is written into it as `<date>-scale-<scale>.md` plus
the `.json` the numbers come from. Omit it to print the report instead.

```sh
bin/aisre fixture bench --scale 1k  --queries 200 --out /tmp/bench   # smoke: ~20 min
bin/aisre fixture bench --scale 10k --out /tmp/bench                 # the reference workload
```

"Smoke" is about the size of the workload, not the length of the run: even at `1k` the generator
writes ~100,000 events and then replays every one of them, which is twenty minutes on a laptop.
Run it when you have changed the storage layer, not on every pass through this page.

Expected: 2-hop subgraph p95 < 1 s, 24 h diff p95 < 2 s. `fixture bench` exits 4 when a
published criterion is missed, which is what makes `bench.yml` a gate rather than a report. If
the reference workload misses by > 2× after tuning, open the storage decision per research §2
exit criteria. Do not commit a run from a laptop into `docs/benchmarks/` as if it were the
reference measurement — the weekly workflow's artifact is.
