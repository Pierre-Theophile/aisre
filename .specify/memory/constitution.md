<!--
Sync Impact Report (amendment 2026-09-17)
- Version change: 1.0.0 → 1.1.0 (MINOR: materially expanded guidance on Principle VII)
- Modified principles: VII. Read-Only by Default — added the confined "report delivery"
  exception (post and edit the engine's own investigation report in a discussion surface,
  scoped credential, non-blocking, no production system). Requested by specs/002 FR-057f,
  argued in specs/002 plan F6 and ADR-0005 D8; the analyze pass (finding C1) required an
  amendment rather than an ADR-level exception. Owner decision 2026-09-17.
- Added sections: none. Removed sections: none.
- Templates requiring updates: none (Constitution Check rows in plans cite the principle by
  number; specs/002 plan row VII to be updated to cite v1.1.0).
- Follow-up TODOs: ADR-0005 D8 to reference this amendment instead of standing alone.

Sync Impact Report (initial ratification)
- Version change: (template) → 1.0.0
- Modified principles: none (initial ratification)
- Added sections:
  - Core Principles I–X
  - Scope & Non-Goals
  - Definitions
  - Development Workflow & Quality Gates
  - Governance
- Removed sections: none
- Templates requiring updates:
  - .specify/templates/plan-template.md — ✅ no change needed; Constitution Check section is
    populated at plan time from this file
  - .specify/templates/spec-template.md — ✅ no change needed
  - .specify/templates/tasks-template.md — ✅ no change needed
- Follow-up TODOs:
  - TODO(PROJECT_NAME): "sre-agent" is the repository working name; replace with the public
    project name once chosen.
-->

# sre-agent Constitution

The project is an AI SRE agent whose substrate is a **bitemporal, event-sourced graph of the
production system**. The graph — not the language model — is where accuracy comes from. Every
rule below exists to protect that substrate.

## Core Principles

### I. Graph First

Every feature MUST be expressible as one of exactly two things: a **read** of the temporal
graph (a query with an as-of instant) or an **event** into the temporal graph (a typed,
idempotent event that changes graph state). A feature that reads from or writes to any other
shared state to produce its result is non-compliant and MUST be rejected at plan review.

- Agent reasoning (hypothesis generation, ranking, root-cause proposals) MUST take graph query
  results as input, never raw connector payloads or raw telemetry.
- Connectors MUST NOT expose data to consumers except by emitting events into the graph.
- Caches, indexes and materialized views are permitted only as derivations of the graph that
  can be dropped and rebuilt from the event log without loss.

Rationale: competitors share the same models and the same telemetry; the substrate is the only
durable source of differentiation. Anything that bypasses it erodes it.

### II. Bitemporal or Nothing

Every node and every edge MUST carry two time dimensions:

- **Valid time** `[valid_from, valid_to)`: the interval during which the fact was true in the
  production system.
- **Observed time** `[observed_from, observed_to)`: the interval during which the system knew
  the fact (a.k.a. transaction time).

Rules:

- A node or edge without both intervals MUST be rejected at ingestion.
- Every query endpoint MUST accept an `as_of` parameter for **both** dimensions (valid time and
  observed time) and MUST return the graph exactly as it was true at valid-time `T_v` as known
  at observed-time `T_o`. Omitting `T_o` means "as known now".
- Corrections MUST NOT overwrite history: a correction closes the observed interval of the old
  fact and opens a new one. Retractions close the valid interval. Nothing is ever deleted from
  history.
- Facts with unknown valid-time boundaries MUST carry an explicit `unknown` marker, never a
  guessed timestamp.

Rationale: the core question — "what did the system look like at 14:32, and what changed since
13:00?" — is unanswerable without distinguishing when something was true from when we learned it.

### III. Event-Sourced Ingestion

The event log is the source of truth; the graph is a projection of it.

- Sources MUST emit **typed** events conforming to the published event schema. Untyped or
  free-form payloads MUST be rejected.
- Every event MUST carry an **idempotency key**; re-delivery of an event with a seen key MUST be
  a no-op.
- Every event MUST be **replayable**: replaying the full event log from empty MUST reproduce a
  graph that is byte-for-byte identical (after canonical serialization) to the live graph.
  A CI job MUST verify this on every fixture.
- Graph state MUST be updated **incrementally**, one event at a time. Batch rebuilds, full
  re-syncs that drop state, and "truncate and reload" patterns are forbidden in production code
  paths. (A full replay from the event log is the only permitted rebuild.)
- Ordering guarantees MUST be stated per source; the ingestion layer MUST tolerate out-of-order
  delivery within a source's declared window by using observed time, never by reordering the log.

Rationale: replayability is what makes incidents reproducible, connectors testable, and the
graph trustworthy.

### IV. Telemetry Stays in Its Backend

The graph MUST NOT store metric, log or trace payloads.

- Nodes and edges MAY carry **pointers**: query expressions (SLI definitions, log selectors,
  span filters, dashboard links) that describe how to retrieve telemetry about them from the
  backend that owns it.
- Any schema field or event type that would persist a metric sample, a log line, a span, or an
  aggregate thereof MUST be rejected.
- Backends MUST be interchangeable behind a pointer interface. **OpenTelemetry semantic
  conventions** are the canonical vocabulary for resource and span attributes; a pointer that
  cannot be expressed in OTel terms MUST document why.
- Telemetry retrieved at query time (to test a hypothesis) is transient and MUST NOT be written
  back to the graph, except as a *decision record* under Principle V.

Rationale: the graph's value is knowing *where to look* and *what to compare*, not duplicating
an ocean of data that already has a home.

### V. Evidence-First

Every conclusion, ranking, resolution decision or hypothesis the system produces MUST be
traceable to the events and queries that produced it.

- Outputs MUST carry a machine-readable **evidence chain**: the event IDs, graph query
  parameters, pointer queries and their results that led to the output.
- Confidence MUST be explicit, numeric, and **calibrated**: a stated confidence of 0.8 MUST be
  correct roughly 80% of the time on the evaluation set. Calibration MUST be measured and
  reported in CI.
- `unknown` is a valid, first-class answer for any question. Producing a guess where the
  evidence does not support one is a defect.
- The language model MUST reason over graph structures (subgraphs, diffs, ranked candidates),
  not over unstructured telemetry dumps. Prompts that inline raw telemetry are non-compliant.

Rationale: an SRE will only trust — and a postmortem will only accept — a conclusion they can
audit.

### VI. Entity Resolution Is Auditable

A pod, a Datadog service, a repository, a Terraform resource, a trace tag and a Slack channel
may be the same entity under five names. Resolving them is the hard problem, and it MUST be
done in the open.

- Every identity claim from a source MUST be stored as a claim, with its source and observed
  time, before any merge.
- Every merge MUST record: the match rule(s) applied, the **confidence score**, and a
  human-readable rationale.
- Merges and splits MUST be reversible and MUST keep their full history (Principle II applies).
- A **human override** MUST persist across replays and re-resolution, and MUST take precedence
  over any automated match regardless of score.
- The resolution layer MUST expose an audit query: "why are these two identifiers the same
  entity?" answerable for any merged node.

Rationale: a wrong merge silently poisons every downstream diff and blast-radius computation;
a correct one that cannot be explained will be undone by the next skeptical operator.

### VII. Read-Only by Default

No component MUST write to any production system.

- Connectors MUST request and use read-only credentials. A connector that requires write scope
  MUST be rejected.
- Remediation, if and when it exists, MUST be delivered as **proposal + policy + sandbox**:
  a proposed action with its evidence chain, a policy engine that gates it, and execution only
  in an isolated environment. Autonomous remediation against production is out of scope until
  this constitution is amended to permit it.
- The tool's own state stores (event log, graph, decision records) are not "production systems"
  for this rule.
- **Confined exception, report delivery.** A component MAY post, and later edit in place, its
  own investigation report into a discussion surface where the incident is being handled (a
  chat thread, a ticket). This is the only write permitted outside the tool's own stores. It
  MUST use a credential scoped to writing that report and nothing else, separate from every
  read credential; it MUST be non-blocking and never a precondition of an investigation's
  terminal state; it MUST carry only the rendered report and its evidence links; and the
  surface MUST NOT be a system that serves production traffic or holds production
  configuration. Any wider write requires a further amendment.

Rationale: trust is earned by observation before action; a read-only tool cannot cause the
incident it is investigating. Telling the on-call what was found, where they already are, is
observation reported, not action taken.

### VIII. Evaluation From Day One

Evaluation is derived from the data model; it is not a separate project.

- Every feature MUST ship with **replayable fixtures**: recorded event streams plus golden
  outputs (graph snapshots, query results, rankings). A feature PR without fixtures MUST NOT
  merge.
- Every connector MUST ship with **recorded real-world payloads** (sanitized) as its test
  corpus. Synthetic-only test data is insufficient for a connector to be marked stable.
- A **replayable incident** is defined as: a graph snapshot at T + the telemetry window
  pointers + the validated root cause. The eval harness MUST accept this triple as its input
  format.
- Regression on any golden fixture MUST fail CI.

Rationale: the substrate thesis is falsifiable only if accuracy is measured on replayable
incidents, and a harness bolted on later will not match the data model.

### IX. Open Schema

The graph schema and its temporal model are the primary public artifact of this project.

- The node taxonomy, edge taxonomy, change-node taxonomy, property conventions, bitemporal
  semantics, event schema and query contract MUST be published as versioned, machine-readable
  definitions (e.g. protobuf/OpenAPI/JSON Schema) with accompanying human documentation.
- Schema changes MUST follow semantic versioning. Breaking changes MUST ship with a migration
  path expressed as an event-log transformation, so that Principle III still holds.
- Any internal type that appears in a public query result or event payload is part of the
  schema and subject to this rule.

Rationale: an open, stable schema is what lets third parties write feeders and consumers, and
is the project's contribution regardless of which agent sits on top.

### X. Simplicity Over Cleverness

- The system MUST build and run as a **single static binary** with minimal external
  dependencies.
- No dedicated graph database MAY be introduced unless the implementation plan demonstrates,
  with a reproducible benchmark on the reference workload (~10k nodes / 100k edges / 1M
  events), that the recommended general-purpose store cannot meet the query budget for
  subgraph-as-of and diff queries.
- Every new dependency, service, or abstraction layer MUST be justified in the plan's
  Complexity Tracking table with the simpler alternative that was rejected and why.
- YAGNI applies: features not required by the current specification MUST NOT be built
  speculatively.

Rationale: an SRE tool that is itself hard to operate will not be adopted by SREs.

## Scope & Non-Goals

The following are explicitly **out of scope** until this constitution is amended:

- Commercial packaging, licensing tiers, or hosted-service concerns.
- UI polish beyond what is needed to inspect the graph and evidence chains.
- Autonomous remediation against production systems (see Principle VII).
- RAG over telemetry. Retrieval-augmented generation is permitted **only** over durable
  knowledge — postmortems, runbooks, ADRs, past agent decisions — and every such document MUST
  be linked to the graph nodes it concerns. The graph is the RAG index.

Licensing: the project is released under **Apache-2.0**. Contributions MUST be compatible.

## Definitions

- **Node types** (minimum): service, workload, infra resource, config, feature flag, DB schema,
  third-party dependency, owner, alert, **change**.
- **Change** is a first-class node type, not an annotation. Change kinds include at minimum:
  rollout, IaC apply, flag flip, secret rotation, scaling, migration, DNS switch, cloud
  maintenance.
- **Edge types** (minimum): calls, depends-on, runs-on, deployed-by, owned-by, exposed-via,
  changed-by.
- **Pointer**: a query expression attached to a node or edge that identifies telemetry in an
  external backend. Pointers are data about *where to look*, never the telemetry itself.
- **Impact subgraph**: the N-hop upstream/downstream neighbourhood of a node, weighted by
  observed traffic, extracted as of a given instant.
- **Replayable incident**: graph snapshot at T + telemetry window pointers + validated root
  cause.

## Development Workflow & Quality Gates

- **Constitution Check** is a mandatory gate in every implementation plan. Each plan MUST list
  every principle and state how the feature complies or why a deviation is justified. A plan
  with an unjustified deviation MUST NOT proceed to tasks.
- **Fixture-first**: tasks MUST reference the replay fixtures they validate against. A task
  that cannot name its fixture is not ready.
- **Replay CI**: every CI run MUST (a) replay all fixtures from empty and diff against golden
  outputs, (b) verify idempotency by double-delivering every fixture event, (c) report
  calibration metrics for any component emitting confidence scores.
- **Schema review**: any change to a published schema requires a version bump, a changelog
  entry, and — for breaking changes — a migration expressed as an event-log transformation.
- **Self-observability**: the tool MUST emit OpenTelemetry traces, metrics and logs about its
  own operation. A component with no OTel instrumentation is not release-ready.
- **Code review**: reviewers MUST verify compliance with this constitution explicitly, not only
  correctness. "Does this bypass the graph?" and "Is this bitemporal?" are required review
  questions.

## Governance

- This constitution supersedes all other project practices, templates and conventions. Where a
  template or guideline conflicts with it, the constitution wins and the template MUST be fixed.
- **Amendment procedure**: an amendment is proposed as a pull request modifying this file,
  MUST include an updated Sync Impact Report, MUST state the version bump and its rationale,
  and MUST identify any existing specs, plans or code made non-compliant along with a migration
  plan. Amendments are accepted by the project maintainer(s).
- **Versioning policy** (semantic): MAJOR for removing or redefining a principle in a
  backward-incompatible way; MINOR for adding a principle or section or materially expanding
  guidance; PATCH for clarifications and wording that do not change meaning.
- **Compliance review**: every `/speckit-plan` output MUST pass the Constitution Check; every
  `/speckit-analyze` run MUST report constitution violations as CRITICAL findings; every
  release MUST include a statement that replay CI and calibration reporting passed.
- Runtime development guidance for agents and contributors lives in `CLAUDE.md` at the
  repository root and MUST defer to this document.

**Version**: 1.1.0 | **Ratified**: 2026-09-15 | **Last Amended**: 2026-09-17
