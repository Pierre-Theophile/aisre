# ADR-0001: Product scope, connector model and early design decisions

- Status: accepted
- Date: 2026-09-15
- Deciders: project owner (product requirements), Claude (technical decisions, documented here)

## Context

The project owner's requirement is a product one: **build the best possible AI SRE agent**.
Technical choices below are delegated to the implementation team, on the condition that each
is documented and reversible. This ADR records the first batch, taken while bootstrapping the
constitution and before writing the first feature spec.

## Product vision (owner's words, lightly edited)

The graph is consumed by the SRE agent itself. The agent connects to every data source an
on-call engineer would open during an incident:

- telemetry: logs, metrics, traces (Datadog, GCP Cloud Monitoring/Logging, ...)
- code: GitHub, GitLab
- delivery: CI/CD pipelines
- people: developer chat (Slack, ...)
- knowledge: docs (Notion, ...), postmortems, the agent's own memory of past incidents

The graph tells the agent **where to search** in those sources. It is the map; the sources are
the territory.

## Decisions

### D1. Edge weights are coarse, bitemporal properties emitted by feeders

The `calls` edge carries a traffic weight (a small set of ordinal classes or a coarse
request-rate bucket) with its own valid time, emitted by the topology feeder. Raw counts stay
in the telemetry backend. This keeps Principle IV (no telemetry payloads) intact while making
impact subgraphs computable without a round-trip to the backend.

Owner position: no preference, delegated.

### D2. Two observed-time defaults: "now" for humans, "incident instant" for evaluation

Interactive queries default to valid-time `T`, observed-time `now` (best current knowledge,
including corrections learned after `T`). The evaluation harness pins observed-time to the
incident instant so that a replay can only use what the system knew at the time.

Owner position: delegated.

### D3. Feature 001 contains no LLM

Feature 001 is the substrate only: graph model, event ingestion, query contract, entity
resolution, two reference feeders, replay fixtures. Candidate ranking by temporal and
topological proximity is a deterministic graph query. The agent (LLM reasoning, hypothesis
testing through pointers, RAG over durable knowledge) is a later feature that consumes 001's
query outputs.

Owner position: "possible yes" — accepted.

### D4. Connectors pull from external sources; the SDK is source-agnostic

The owner's intent is to connect to **any** external data source through its API or MCP
surface (Datadog, GCP, Notion, GitHub, Slack, ...), not to sit inside a telemetry pipeline.
Therefore:

- The connector SDK MUST support **pull** connectors (poll or subscribe to a vendor API or
  MCP server) as the primary model, and MAY support push connectors (e.g. an OTLP receiver)
  as a secondary model.
- Every connector, whatever its transport, emits the same typed events into the graph
  (Principle III) and uses OpenTelemetry semantic conventions as its vocabulary
  (Principle IV).
- Feature 001 still ships exactly two reference feeders, as originally scoped:
  - **OpenTelemetry topology feeder**: derives service topology from span data. To stay
    vendor-neutral and testable without credentials, its reference implementation consumes
    OTLP; its fixtures are recorded OTLP payloads. A Datadog-backed variant that pulls the
    same topology from the Datadog API is the first connector of a later feature and MUST
    produce identical graph events for identical topology.
  - **Kubernetes feeder**: workloads, rollouts and config changes as change nodes, via the
    Kubernetes watch API.
- Connectors for GitHub/GitLab, CI/CD, Slack, Notion, postmortems and agent memory are
  planned as features 002+ and are out of scope for 001.

### D5. One graph instance per organisation

A single graph models one organisation. Environment, region, cluster and account are node
properties or nodes, not separate graph instances.

### D6. Entity resolution is conservative; humans correct via CLI/API

Automated merges happen only above a high confidence threshold. Matches below it are stored
as **suggestions**: the agent may surface them ("these two may be the same service") but the
graph treats them as distinct entities until an SRE confirms. Confirmation, rejection and
manual merge/split are done through a CLI or API command in 001 (no UI). Every human decision
is an event in the log, persists across replays and re-resolution, and outranks any automated
score (Principle VI).

Owner position: chose "never merge unless sure or confirmed" over auto-merge; accepted.

### D7. A workload and its service are one entity; type resolves by precedence

Surfaced while authoring the first fixtures (2026-09-15): a Kubernetes Deployment and the
OpenTelemetry service running on it are asserted with different node types (WORKLOAD vs
SERVICE). They are merged into one entity, per the brief ("the same entity under five
names"), so rollouts, scaling and config changes sit at hop 0 from the service an alert names.
The resolved `type` follows a published precedence (SERVICE first); all asserted types are kept
in `facets`. Details in research.md §10. Alternative rejected: keeping them as two nodes linked
by an edge, which pushes every deployment change one hop away from the alerting service and
weakens ranking.

## Consequences

- The first spec (001) stays minimal and vendor-neutral; the product breadth the owner wants
  arrives through the connector SDK, one connector per feature.
- The connector SDK design in the plan must be judged primarily on how easy it is to write a
  pull connector against a vendor API or MCP server, with recorded payloads as tests.
- Any of D1–D7 can be reversed by a new ADR; none is baked into the constitution.
