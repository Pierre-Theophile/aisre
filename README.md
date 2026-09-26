<!-- SPDX-License-Identifier: Apache-2.0 -->

# aisre

*`aisre` is a working name; the final one is not chosen yet. Inside the code, identifiers and
recorded fixtures still say `sre-agent`.*

An AI SRE agent whose substrate is a **bitemporal, event-sourced graph of the production
system**.

The thesis, in five lines:

1. Every SRE investigation is the same two questions: *what did the system look like at 14:32,
   and what changed since 13:00?*
2. Neither is answerable from telemetry, because telemetry records measurements and neither of
   those questions is about a measurement.
3. So the substrate is a graph of services, workloads, infrastructure, config, owners and
   **changes**, carrying two time dimensions: when a fact was true, and when we learned it.
4. The graph — not the language model — is where accuracy comes from. Competitors share the
   same models and the same telemetry; the substrate is the differentiator.
5. The schema is the contribution, and it is open, regardless of which agent ends up sitting on
   top of it.

Feature 001 is the substrate. Features 002 to 004 put an investigator on top of it and feed it the
cloud platform and the deploys an organisation actually runs.

## Status

| feature | state |
|---|---|
| [001 Temporal graph core](specs/001-temporal-graph-core/spec.md) | complete |
| [002 Investigation engine](specs/002-investigation-engine/spec.md) | complete; [quickstart run](specs/002-investigation-engine/quickstart-run-2026-09-18.md) on a clean machine |
| [003 GCP integration](specs/003-gcp-integration/spec.md) | built and verified on synthetic twins ([quickstart run](specs/003-gcp-integration/quickstart-run-2026-09-22.md)); the recording campaign against a real estate and what can only be shown on it wait on a credential and a named signatory |
| [004 Deploy feeders](specs/004-deploy-feeders/spec.md) (GitHub, Vercel) | built and verified on synthetic twins; waits on the same campaign, and on a real project-scoped Vercel token to establish the token regime |
| [005 Datadog connector](specs/005-datadog-connector/spec.md) | specified; not yet planned |

Each feature's `tasks.md` says exactly what is open and why. What follows is feature 001's part.

What works, today, against a PostgreSQL 16 and nothing else:

- **Bitemporal storage.** Every node and edge carries a valid interval and an observed interval.
  Corrections never overwrite: they close an observed interval and open a new one. Nothing is
  ever deleted. A fact with an unknown start says so rather than guessing.
- **An event log as the source of truth**, with the graph as its projection. Typed events,
  idempotency keys, one event per transaction, replay from empty reproducing the live graph byte
  for byte.
- **Queries**: `subgraph` as of any instant in both dimensions, `diff` over a window with
  changes ranked by temporal, topological and traffic proximity, `impact` (blast radius,
  upstream and downstream, weighted), `pointers` (where the telemetry lives), `history` (every
  version across observed time), `extent` (what each source has actually seen, and its gaps).
  Human-readable and canonical-JSON output from the same command.
- **Entity resolution in the open.** Every identifier a source reports is stored as a claim
  before any merge. Published match rules; only *certain* rules merge automatically, probable
  ones produce suggestions. Every merge records its rule, score and rationale; human decisions
  survive replay and outrank any automated rule; `resolve why` answers "why are these two the
  same entity?" for any merged node.
- **Two reference feeders**, OpenTelemetry topology and Kubernetes, written against the same
  public SDK any third-party connector uses, each running identically live and from recordings.
- **Fixtures as the evaluation harness.** Seven recorded fixture families, replayed in CI from
  empty, double-delivered, and shuffled inside each source's declared reordering window.
- **Authentication from version one.** Every query and every decision names an individual
  (OIDC in production, a local dev provider for fixtures and laptops). Feeder tokens are scoped
  to one source.
- **Self-observability**: OpenTelemetry traces, metrics and logs about its own operation.

The record: [spec](specs/001-temporal-graph-core/spec.md) ·
[plan](specs/001-temporal-graph-core/plan.md) ·
[tasks](specs/001-temporal-graph-core/tasks.md) ·
[constitution](.specify/memory/constitution.md) ·
[decisions](docs/decisions/) · [the live run that closed it](docs/benchmarks/live-run-2026-09-16.md)

Not built, deliberately: any chat or graphical interface, and any write to a production system.
The investigator proposes and never acts, and autonomous remediation is out of scope until the
constitution is amended to permit it.

## Quickstart

```sh
make build
docker compose -f deploy/docker-compose.yml up -d postgres
export PG_DSN='postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable'
bin/aisre migrate --db "$PG_DSN"
bin/aisre serve --db "$PG_DSN" --listen 127.0.0.1:8080 --auth dev --dev &
export SRE_AGENT_TOKEN=$(bin/aisre dev-token --dev --user alice --roles reader,decider)

bin/aisre fixture load fixtures/rollout-regression-01
bin/aisre query diff otel.service.name=checkout \
  --from 2026-09-01T13:00:00Z --to 2026-09-01T14:32:00Z
```

That last command is the product in one line: four changes in the window, ranked, with the
rollout that caused the regression first and a decoy that is near in time and far in topology
third.

The whole walkthrough — every query, the bitemporal check, entity resolution, the verification
harness, the feeders, the benchmark — is
[`specs/001-temporal-graph-core/quickstart.md`](specs/001-temporal-graph-core/quickstart.md).
Running a database: [`deploy/README.md`](deploy/README.md).

## Architecture

```
  sources                 ingestion            storage                  reads
  ───────                 ─────────            ───────                  ─────

  Kubernetes API ─┐                         ┌─ log.events ──┐
                  ├─ feeder ─┐              │  append-only  │
  OTLP spans ─────┘  (SDK)   │              │  the truth    │
                             ├── serve ─────┤               ├── query subgraph --as-of
  your system ──── your      │  (Connect:   │               │   query diff     (ranked)
                   feeder ───┘   gRPC +     └─ graph.*  ────┘   query impact   (blast radius)
                                 JSON)         bitemporal       query pointers (where to look)
                                               projection       resolve why    (identity audit)
```

- **Feeders** turn one external system into typed events. They never write to the graph
  directly and never answer questions about their source; they emit, and the graph does the rest.
  One public Go package, [`pkg/feeder`](pkg/feeder), proven by two unlike sources before a third
  was added.
- **The event log** (`log.*`) is append-only and is the source of truth. Every event carries an
  idempotency key; re-delivery is a no-op; replaying the log from empty reproduces the graph.
- **The projection** (`graph.*`) is bitemporal entity and edge versions, identity claims,
  resolution decisions and suggestions. It is a derivation: drop it and rebuild it from the log.
- **Queries** read the projection as of a valid instant and an observed instant. Every element of
  every response is traceable to the events that produced it.
- **Telemetry stays in its backend.** No metric sample, log line or span is ever stored. Nodes
  carry *pointers*: selectors in OpenTelemetry semantic conventions saying how to fetch that
  telemetry from the system that owns it.
- **Fixtures are the evaluation harness**, not a test suite bolted on afterwards. A fixture is
  recorded payloads plus the events they must produce plus golden query outputs; `fixture verify`
  replays each from empty, delivers every event twice, and shuffles within each source's
  reordering window. Regression on a golden fails CI. See [`fixtures/README.md`](fixtures/README.md).

One static binary, no cgo, plus PostgreSQL. No dedicated graph database: the
[reference-workload benchmark](docs/benchmarks/) is the evidence for that decision, and
[ADR-0002](docs/decisions/ADR-0002-language-storage-api.md) is the decision.

## The schema

The graph schema and its temporal model are the primary public artifact of this project
(constitution IX). They are published as versioned, machine-readable definitions with human
documentation beside them:

| | |
|---|---|
| Event and query contract | [`api/sreagent/graph/v1/graph.proto`](api/sreagent/graph/v1/graph.proto) |
| Node, edge and change taxonomy, property conventions | [`docs/schema/README.md`](docs/schema/README.md) |
| Bitemporal semantics | [`docs/schema/temporal-model.md`](docs/schema/temporal-model.md) |
| Query contract | [`docs/schema/queries.md`](docs/schema/queries.md) |
| Pointers | [`docs/schema/pointers.md`](docs/schema/pointers.md) |
| Identity resolution rules | [`docs/schema/resolution.md`](docs/schema/resolution.md) |
| Change ranking | [`docs/schema/ranking.md`](docs/schema/ranking.md) |
| Fixture format | [`fixtures/README.md`](fixtures/README.md) |

Schema changes follow semantic versioning; `buf breaking` runs on every pull request, and a
breaking change ships with a migration expressed as an event-log transformation.

## Writing a connector

Everything you need is [`docs/connectors/writing-a-feeder.md`](docs/connectors/writing-a-feeder.md)
and the published `pkg/feeder` API. Budget a day. The conformance suite is one test function:

```go
func TestConformance(t *testing.T) {
    testkit.Conformance(t, &myfeeder.Feeder{SourceID: "myvendor:prod-eu1"},
        "testdata/myvendor-topology-01")
}
```

It needs no database. A connector without a fixture is not merged — walk
[`docs/connectors/checklist.md`](docs/connectors/checklist.md) before opening the pull request.

## Security

sre-agent never writes to a production system. Feeders use read-only credentials and refuse to
start without them; there is no remediation path, no API call that changes anything outside this
database. The trust boundaries, the role model, what a compromised feeder token can and cannot
do, and the findings of the security pass are in [`docs/security.md`](docs/security.md).
Reporting a vulnerability: [`SECURITY.md`](SECURITY.md).

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) — DCO sign-off, what a pull request needs, and the review
questions that are not about correctness ("Does this bypass the graph?", "Is this bitemporal?").
[`.specify/memory/constitution.md`](.specify/memory/constitution.md) supersedes every other
practice here; read it before proposing a design.

```sh
make            # generate, build, test, lint
make verify     # replay every fixture against its goldens
make help       # everything else
```

## License

Apache-2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
