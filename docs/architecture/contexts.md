# Bounded contexts and the dependency rule

This page names the parts of aisre and the direction dependencies are allowed to point. The
rules that can be checked mechanically are enforced by `internal/archtest`, which runs with
`go test ./...`: a change that breaks one fails CI.

## The rule

Dependencies point inwards, towards the domain:

```
cmd/aisre ──► internal/cli (composition root: wires everything, owns no rules)
                 │
                 ▼
  adapters ──► application (use cases) ──► domain ◄── ports (pkg/, consumer-side interfaces)
  postgres,     runner, intake, engine,      graph, event, ledger,
  vendor SDKs,  projector, query, catalog    resolution, mask, knowledge
  connectors
```

- A **domain** package holds value objects, aggregates and pure rules. It never links a database
  driver, a vendor SDK, or a cloud or cluster client, even transitively.
- An **application** package orchestrates a use case. It depends on domain packages and on ports,
  and receives adapters from the composition root.
- An **adapter** implements a port against one technology (Postgres, Datadog, GCP, Slack, Notion,
  an LLM vendor, an embedding server).
- The **public SDK** (`pkg/`) is a set of ports plus their conformance kits. It is held to the
  domain standard: a connector built on it never links a database driver.

## Contexts

| Context | Domain | Application | Adapters | Ports |
|---|---|---|---|---|
| Graph and ingestion | `internal/graph`, `internal/event`, `internal/resolution` | `internal/projector`, `internal/query` | `internal/log` (Postgres event log), `internal/feeders/*`, `internal/store/postgres` | `pkg/feeder` |
| Investigation | `internal/investigation/ledger`, `internal/mask`, `internal/injection`, `internal/logmine` | `intake`, `engine`, `verify`, `runner`, `workers/*`, `render` | `internal/backends/*`, `investigation/store`, `investigation/model/anthropic` and the built-in `mistral` and `openai_compatible` adapters, `render/slacksink` | `model.Adapter` (registered with `model.RegisterProvider`), `pkg/backend`, `pkg/worker`, consumer-side interfaces (`graph.QueryService`, `chat.History`, `render.Sink`, …) |
| Knowledge | `pkg/knowledge`, `internal/knowledgedoc` | `internal/embedding` (indexer) | `internal/store/postgres`, `pkg/knowledge/memory`, `embedding` OpenAI-compatible adapter | `knowledge.Store`, `knowledge.Embedder` |
| Catalog and attribution | `pkg/attribution`, `internal/catalog` (rules, discovery, decision model) | `catalog.Attributor` | `internal/catalog/pgcatalog` (Postgres and the projector), `internal/catalog/classify` | `catalog.Store`, `catalog.EventLog`, `attribution.Classifier` |
| Evaluation | — | `internal/eval`, `internal/fixture`, `internal/investigation/replay` | `internal/fixture/graphquery` | — |
| Shared vendor clients | — | — | `internal/datadogx`, `internal/gcpx`, `internal/slackx`, `internal/notionx` | — |

Cross-context rules:

- The graph read side (`query`, `server`) does not depend on the investigation engine or on the
  evaluation tooling. Evaluation depends on the read side, never the reverse.
- Shared utilities (`internal/event`, `internal/mask`, `internal/sanitise`, `internal/injection`,
  `internal/logmine`, `internal/knowledgedoc`) are leaves: they do not import an application layer.
- Ingestion (`internal/feeders/*`) does not import the investigation context. A telemetry backend
  does not import the workers or the engine that consume it, and a worker does not import the
  engine that orchestrates it.
- A shared vendor client (`datadogx`, `gcpx`) may import the feeder whose source interfaces it
  implements: that is the adapter depending on the port, which is the allowed direction.
- Every external dependency an operator might want to replace (LLM, embeddings, knowledge store,
  telemetry backends, feeders) sits behind a port and ships at least one adapter that can run
  self-hosted.

## Known gaps

These are tracked and not yet enforced:

- `investigation/runner` still takes `*postgres.Store` and calls the DAOs in
  `investigation/store` directly. The catalog (`catalog.Store`, `catalog.EventLog`) and the server's
  principal registry (`server.PrincipalRecorder`) now go through consumer-side interfaces; the
  runner is next, and needs the pure parts of `investigation/store` split from its SQL first.
- Protobuf types double as the domain model (ADR-0002). This is a deliberate trade-off; new
  aggregates should keep the generated types at their edges.
