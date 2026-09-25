# Feature Specification: Temporal System Graph Core

**Feature Branch**: `001-temporal-graph-core`

**Created**: 2026-09-15

**Status**: Draft

**Input**: User description: "The graph core of an AI SRE agent: a bitemporal, event-sourced
graph of the production system with a typed event ingestion contract, a query contract
(subgraph as-of, diff with ranked changes, blast radius, pointer lookup), auditable entity
resolution with persistent human overrides, two minimal reference feeders (OpenTelemetry
topology from spans, Kubernetes workloads and changes), and replayable fixtures with golden
outputs. No LLM, no UI, read-only."

## Why this feature exists

Every AI SRE agent has the same models and the same telemetry. What none of them has is a
trustworthy answer to the two questions an on-call engineer actually asks first:

1. *What did the system around this service look like at the moment of the alert?*
2. *What changed between "it was fine" and "it is not"?*

This feature builds the substrate that answers exactly those two questions, with full history,
with evidence, and without ever copying telemetry out of the systems that own it. Everything
the agent does later — hypothesis generation, testing through pointers, retrieval of runbooks —
is a consumer of this feature. Nothing in this feature depends on a language model.

## Clarifications

### Session 2026-09-15

- Q: How far back must an SRE be able to ask "what did the system look like at this time"?
  → A: Unlimited for now; revisit only if it becomes a cost or product problem. (FR-016)
- Q: How must the identity of a person confirming or rejecting a match be recorded in version
  one? → A: Individual login from the start; every decision tied to an authenticated
  individual. (FR-041, FR-041a)
- Q: When is the feature done? → A: Automated fixture tests pass and a live run against a
  disposable cluster with a demo application produces a matching graph. (FR-050)

## Personas

- **Sam, on-call SRE.** Gets paged at 14:32. Has ten browser tabs open. Wants to know what
  changed and who to call. In this feature Sam interacts through a command-line tool; later,
  the agent does this on Sam's behalf.
- **Casey, connector author.** Maintains an integration with one external system (a telemetry
  vendor, a cloud provider, a chat tool). Wants to turn that system's data into graph events
  with as little ceremony as possible, and prove it works with recorded payloads.
- **The agent (future consumer).** Not built here. Every query in this feature is designed so
  that a machine can consume its output as structure, not prose.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See the system as it was at the moment of the alert (Priority: P1)

Sam receives an alert on `checkout` at 14:32. Sam asks for the neighbourhood of `checkout` as
it was at 14:32: the services it calls and is called by, the workloads it runs as, the
infrastructure and configuration it depends on, who owns it, and which third-party
dependencies it touches. Each relationship shows how much traffic flows over it. Each node
shows where its telemetry lives.

**Why this priority**: This is the foundational read. Without it nothing else (diff, blast
radius, hypothesis testing) has an input.

**Independent Test**: Load the baseline fixture, ask for the 2-hop neighbourhood of a named
service at a fixed instant, compare to the golden output. Deliverable is a correct, complete,
time-accurate picture with no other capability present.

**Acceptance Scenarios**:

1. **Given** the baseline fixture is loaded, **When** Sam asks for the 2-hop neighbourhood of
   `checkout` as of 14:32, **Then** the result contains exactly the nodes and edges valid at
   14:32 (as known now), each with its valid interval, its observed interval, its type, its
   traffic weight where applicable, and its pointers.
2. **Given** a `calls` edge from `checkout` to `payments` existed until 14:10 and was then
   retracted, **When** Sam asks as of 14:32, **Then** the edge is absent; **When** Sam asks as
   of 14:00, **Then** the edge is present.
3. **Given** a hub node (a shared database called by 40 services), **When** Sam asks for a
   3-hop neighbourhood that would cross it, **Then** the result is truncated according to the
   stated caps and explicitly says what was truncated and why.
4. **Given** a fact was learned at 15:00 about the state at 14:20, **When** Sam asks as of
   14:32 with observed time "now", **Then** the fact is included; **When** Sam asks as of 14:32
   as known at 14:32, **Then** the fact is excluded.

---

### User Story 2 - See what changed, ranked by how likely it is to matter (Priority: P1)

Sam knows `checkout` was healthy at 13:00 and is failing at 14:32. Sam asks what changed in
and around `checkout` between those instants. The answer is a diff of the neighbourhood (nodes
and edges added, removed, or altered) plus a ranked list of change events — rollouts,
configuration edits, scaling actions — ordered so that changes close in time to the failure
and close in topology to `checkout` come first. Each ranked item says why it got its rank.

**Why this priority**: This is the product thesis. A ranked, explainable list of candidate
causes is what the agent will later turn into hypotheses. Ranking quality is the headline
metric of the project.

**Independent Test**: Load a fixture containing a known culprit change among decoy changes;
run the diff; the culprit ranks in the top three and its ranking rationale cites both its
temporal and topological proximity.

**Acceptance Scenarios**:

1. **Given** the "rollout regression" fixture where `payments` was rolled out at 14:20 and
   three unrelated services were rolled out between 13:00 and 14:32, **When** Sam diffs the
   2-hop neighbourhood of `checkout` between 13:00 and 14:32, **Then** the `payments` rollout
   is ranked first, and the rationale states its hop distance (1) and its time distance to
   14:32 (12 minutes).
2. **Given** the same fixture, **When** Sam asks for the diff, **Then** the response lists the
   `payments` version property as *changed* with old and new values, and lists no nodes as
   added or removed that were in fact stable.
3. **Given** a change whose target could not be resolved to any known node, **When** it falls
   inside the window, **Then** it appears in the ranked list flagged as *unattached* with a
   lower rank, not silently dropped.
4. **Given** two changes with identical time and hop distance, **When** ranked, **Then** their
   relative order is deterministic and documented (tie-break rule stated in the output).

---

### User Story 3 - Write a feeder and prove it with recorded payloads (Priority: P1)

Casey has recordings of what an external system emits (for example, a stream of Kubernetes
watch responses). Casey writes a feeder that turns those recordings into typed graph events.
Casey runs the recordings through the feeder, inspects the resulting graph, freezes it as a
golden output, and commits recordings plus golden output as the feeder's test. Any later
change to the feeder that alters the graph fails the test.

**Why this priority**: The product's breadth (Datadog, GCP, GitHub, Slack, Notion, ...) arrives
through feeders. If writing one is painful or untestable, the product never gets wide.

**Independent Test**: Take the recorded Kubernetes payloads shipped with this feature, run them
through the Kubernetes feeder from an empty graph, compare the graph to the golden output.

**Acceptance Scenarios**:

1. **Given** a recorded payload stream and an empty graph, **When** Casey runs the feeder,
   **Then** every emitted event conforms to the published event schema, and the resulting graph
   matches the golden output exactly.
2. **Given** Casey delivers the same recording twice, **When** the second run finishes,
   **Then** the graph is unchanged and every duplicate event is reported as a no-op.
3. **Given** Casey shuffles the recording within the source's declared reordering window,
   **When** the feeder runs, **Then** the valid-time state of the graph is identical to the
   in-order run.
4. **Given** Casey emits an event that would store a raw metric sample or log line, **When**
   the event is ingested, **Then** it is rejected with a reason naming the offending field.

---

### User Story 4 - Know the blast radius (Priority: P2)

Sam wants to know who is affected if `payments` degrades, and what `payments` itself depends
on. Sam asks for the impact of `payments` as of now and gets two ordered lists: downstream
dependents (who breaks) and upstream dependencies (what could break it), each with hop
distance, the path taken, and the traffic weight along it.

**Why this priority**: Blast radius drives who Sam pages and how loudly. It reuses the
subgraph read, so it is cheap to deliver once Story 1 exists.

**Independent Test**: Run the impact query on the baseline fixture and compare to golden
output.

**Acceptance Scenarios**:

1. **Given** the baseline fixture, **When** Sam asks for the impact of `payments`, **Then**
   `checkout` appears as a downstream dependent at hop 1 with its traffic weight, and the
   `payments-db` appears as an upstream dependency at hop 1.
2. **Given** two paths from `payments` to `storefront` (direct and via `checkout`), **When**
   Sam asks for impact, **Then** `storefront` appears once, at its shortest hop distance, with
   the heaviest path shown and the alternative count stated.

---

### User Story 5 - Know where to look (Priority: P2)

Sam has a suspect node and wants to open the right dashboards, log searches and trace views
without hunting. Sam asks for the pointers of `payments` as of 14:32 and gets, grouped by kind,
the selectors that identify its telemetry in whichever backend owns it, plus links back to the
source systems (the deployment, the repository, the owning team's channel where known).

**Why this priority**: Pointers are how the agent will later test hypotheses. For Sam today
they save minutes of tab-hunting.

**Independent Test**: Pointer lookup on the baseline fixture matches golden output and every
pointer is expressed in the published backend-neutral vocabulary.

**Acceptance Scenarios**:

1. **Given** the baseline fixture, **When** Sam asks for pointers of `payments`, **Then** the
   result includes at least one metric selector, one log selector and one trace selector, each
   naming the backend kind it targets and the vocabulary it uses.
2. **Given** `payments` was renamed at 14:00, **When** Sam asks for pointers as of 13:00,
   **Then** the selectors use the old name.

---

### User Story 6 - Correct the graph's idea of "the same thing" (Priority: P2)

The graph has seen `checkout` from the telemetry feeder and `checkout-svc` from Kubernetes. It
is not certain they are the same service, so it keeps them separate and lists the pair as a
suggestion. Sam confirms they are the same with one command. From then on, queries treat them
as one entity, the merge is explained on request, and the decision survives any rebuild of the
graph. Sam can equally reject a suggestion or split a wrong automated merge.

**Why this priority**: A wrong merge poisons every diff and blast radius. A conservative
resolver plus cheap corrections is how the graph earns trust.

**Independent Test**: Load the "ambiguous identity" fixture, confirm the suggested pair,
replay the full event log from empty, verify the merge is still in effect and the audit query
explains it.

**Acceptance Scenarios**:

1. **Given** two entities matched only by a non-certain rule, **When** ingestion completes,
   **Then** they remain separate nodes and the pair is listed as a suggestion with its score
   and rationale.
2. **Given** Sam confirms the suggestion, **When** Sam queries either name, **Then** the same
   canonical entity is returned, and the audit query lists the human confirmation as the
   deciding evidence.
3. **Given** a human confirmation exists, **When** a new identity claim arrives that an
   automated rule would score as "different", **Then** the entities stay merged and the
   conflict is surfaced for review.
4. **Given** Sam splits a previously merged entity, **When** Sam asks as of a time before the
   split, **Then** history shows the merged view; as of after the split, the separate view.

---

### User Story 7 - Reproduce an incident exactly (Priority: P3)

A reviewer wants to check what the graph could have known at the moment of a past incident.
They load the incident's recorded events, ask for the neighbourhood as of the incident instant
with observed time pinned to that same instant, and get a result identical to what would have
been returned live at the time. Golden outputs recorded then still match now.

**Why this priority**: This is the seed of the evaluation harness. It is cheap once bitemporal
queries exist, and it is what keeps the project honest.

**Independent Test**: For every shipped fixture, replay from empty and compare every golden
query output.

**Acceptance Scenarios**:

1. **Given** any shipped fixture, **When** replayed from an empty graph, **Then** every golden
   output matches exactly.
2. **Given** a fixture containing a fact learned after the incident instant, **When** the
   neighbourhood is queried with observed time pinned to the incident instant, **Then** the
   late fact is absent.

---

### Edge Cases

- **Out-of-order arrival**: a source delivers an event whose valid time precedes events already
  processed. Valid-time state must converge to the same result; observed time records actual
  arrival.
- **Duplicate delivery**: the same idempotency key arrives twice, possibly with a different
  payload. The first wins; the second is a no-op and the discrepancy is logged for audit.
- **Conflicting sources**: two feeders assert incompatible facts about the same entity (for
  example, different owners). Both claims are kept with provenance; the graph exposes the
  conflict rather than picking silently.
- **Unknown valid-time boundaries**: a source knows a fact is true now but not since when. The
  start is recorded as unknown and queries treat it as "from the earliest observation".
- **Clock skew**: sources disagree on time by seconds to minutes. Valid time comes from the
  source; observed time comes from the graph; both are exposed so skew is visible, never
  silently corrected.
- **Feeder gap**: a feeder disconnects for an hour and reconnects. Facts it would have retracted
  during the gap are retracted with a valid end inside the gap and an observed start at
  reconnection; the gap itself is recorded so queries can flag reduced confidence.
- **Hub explosion**: a shared database or message bus connects hundreds of services. Neighbour
  expansion must cap fan-out per hop and per query and state the truncation.
- **Dangling change**: a change event names a target the graph cannot resolve. The change node
  is kept, marked unattached, and becomes attached automatically if the target later appears.
- **Retracting a node with live edges**: the node's edges are retracted at the same valid end,
  and the retraction records that they were closed as a consequence.
- **Edge direction disagreement**: two sources report a relationship in opposite directions.
  Both edges are kept with provenance; the query layer exposes the conflict.
- **Query before history**: an as-of instant earlier than the first observation returns an
  empty result flagged as "before recorded history", not an error.
- **Schema version drift**: a feeder emits an event schema version the graph does not know. The
  event is rejected and the feeder is told which versions are accepted.
- **Secrets**: a configuration node representing a secret carries only its version identifier,
  never its value. An event attempting to carry a value is rejected.

## Requirements *(mandatory)*

### Functional Requirements

#### A. Graph data model

- **FR-001**: The graph MUST support at least these node types: service, workload,
  infrastructure resource, configuration, feature flag, database schema, third-party
  dependency, owner, alert, change.
- **FR-002**: The graph MUST support at least these edge types: calls, depends-on, runs-on,
  deployed-by, owned-by, exposed-via, changed-by.
- **FR-003**: Change MUST be a node type. Every change node MUST carry a change kind from a
  published taxonomy that includes at least: rollout, infrastructure-as-code apply, feature
  flag flip, secret rotation, scaling, migration, DNS switch, cloud provider maintenance. The
  taxonomy MUST be extensible without a breaking schema change.
- **FR-004**: Every change node MUST carry: the kind, a human-readable summary, the actor who
  performed it where known, a reference back to the change in its origin system, and its
  target(s) expressed as changed-by edges.
- **FR-005**: Every node MUST carry: a stable canonical identifier, a type, a display name, its
  set of aliases (identifiers in other systems), typed properties, pointers, provenance (which
  source and which events produced it), and both temporal intervals defined in section B.
- **FR-006**: Every edge MUST carry: source node, target node, type, typed properties,
  provenance, both temporal intervals, and, for `calls` edges, a traffic weight expressed as a
  small published ordinal scale rather than a raw count.
- **FR-007**: Property names MUST follow OpenTelemetry semantic conventions where a convention
  exists; otherwise they MUST use a documented namespace. Environment, region, cluster and
  account MUST be expressed as properties or nodes within one graph, never as separate graphs.
- **FR-008**: A pointer MUST consist of: a kind (metric, log, trace, dashboard, source link), a
  backend kind it targets, and a selector expressed in the published backend-neutral
  vocabulary. Pointers MUST be versioned in time like any other property.
- **FR-009**: The graph MUST reject any event or property that would store a metric sample, log
  line, span, or aggregate thereof, and MUST name the offending field in the rejection.

#### B. Bitemporal semantics

- **FR-010**: Every node and edge version MUST carry a valid interval (when the fact was true in
  the production system) and an observed interval (when the graph knew it). Both are
  half-open: start inclusive, end exclusive. A missing interval MUST cause rejection.
- **FR-011**: Either boundary of the valid interval MAY be marked unknown. An unknown start is
  treated in queries as "no earlier than the first observation"; an unknown end as "still
  true". Guessing a timestamp in place of unknown is prohibited.
- **FR-012**: A correction MUST close the observed interval of the previous version and open a
  new version with a new observed start. The previous version MUST remain queryable as of any
  observed time within its interval.
- **FR-013**: A retraction MUST set the valid end of the fact. The retraction is itself an
  observation with its own observed start; a query with observed time before that start MUST
  still see the fact as true.
- **FR-014**: No version of any node or edge is ever deleted. History is append-only.
- **FR-015**: Every query MUST accept a valid-time instant and an optional observed-time
  instant. When observed time is omitted, "now" is used. Results MUST reflect the graph exactly
  as true at the valid instant and as known at the observed instant.
- **FR-016**: The graph MUST answer as-of queries for any instant since its first
  observation, with no history horizon. Retention limits, if ever introduced, require a spec
  amendment; the design MUST NOT assume one.

#### C. Event ingestion contract

- **FR-017**: Sources MUST communicate with the graph exclusively by emitting typed events.
  The event set MUST include at least: upsert node, upsert edge, retract node, retract edge,
  observe change, identity claim, and the human resolution decisions (confirm merge, reject
  merge, split entity). Feeders MAY also emit a source checkpoint event to mark the extent of
  what they have observed.
- **FR-018**: Every event MUST carry: a unique event identifier, an idempotency key, the source
  identifier, the event schema version, the valid time asserted by the source, and, where the
  source has one, a per-source sequence number.
- **FR-019**: The graph MUST assign the observed start at acceptance. A source MAY additionally
  state when it first learned the fact; that value is kept as provenance and does not replace
  the graph's observed time.
- **FR-020**: An event whose idempotency key has been seen MUST be acknowledged as a no-op and
  MUST NOT alter the graph. If its payload differs from the first delivery, the discrepancy
  MUST be recorded for audit.
- **FR-021**: The graph MUST accept out-of-order events. For events from a single source, the
  valid-time state after processing MUST be independent of arrival order.
- **FR-022**: The graph MUST apply events incrementally, one at a time. Any code path that
  drops graph state and reloads it from anything other than the event log is prohibited.
- **FR-023**: Replaying the complete event log from an empty graph MUST reproduce a graph
  identical, under canonical serialization, to the graph produced live. Observed times in a
  replay MUST come from the log, not from the replay clock.
- **FR-024**: Events that fail validation (malformed, untyped, unknown schema version,
  telemetry payload, missing intervals) MUST be rejected with a machine-readable reason,
  recorded in a rejected-events store for audit, and MUST NOT alter the graph.
- **FR-025**: The event schema MUST be versioned. Breaking changes MUST ship with a
  transformation from the previous version so that historical logs remain replayable.

#### D. Query contract

- **FR-026 (Subgraph as-of)**: Given a node reference, a valid instant, an optional observed
  instant, a hop count, a direction (upstream, downstream, both), optional edge-type filters
  and an optional minimum traffic weight, the graph MUST return the nodes and edges of that
  neighbourhood with all their properties, intervals, weights and pointers. The response MUST
  state any truncation applied (per-hop and total caps) and the reason.
- **FR-027 (Diff)**: Given a subgraph specification (as in FR-026) and two valid instants T1 <
  T2, the graph MUST return: nodes added, nodes removed, nodes changed (with old and new values
  per property), edges added, removed and changed, and the list of change nodes whose valid
  interval intersects the window and whose targets lie within the subgraph or within a stated
  extra hop margin.
- **FR-028 (Change ranking)**: The change list in FR-027 MUST be ranked by a published score
  combining temporal proximity (to T2 by default, or to a caller-supplied reference instant)
  and topological proximity (hop distance from the focus node, weighted by traffic). Each ranked
  item MUST expose its score components. Ties MUST be broken deterministically by a documented
  rule. Unattached changes MUST be included at reduced rank, never omitted.
- **FR-029 (Impact)**: Given a node reference and an as-of instant, the graph MUST return
  downstream dependents and upstream dependencies, each with hop distance, the heaviest path,
  the number of alternative paths, and the traffic weight along the heaviest path, ordered by
  weight then hop distance.
- **FR-030 (Pointers)**: Given a node reference and an as-of instant, the graph MUST return its
  pointers grouped by kind, using the names and aliases valid at that instant.
- **FR-031 (Resolution audit)**: Given two identifiers, the graph MUST answer whether they
  resolve to the same entity and why: the claims involved, the rules applied, the scores, and
  any human decisions, in the order they were observed.
- **FR-032 (Node history)**: Given a node reference, the graph MUST return all its versions
  across observed time, so that corrections are visible.
- **FR-033 (Suggestions)**: The graph MUST list pending resolution suggestions with score and
  rationale, filterable by node.
- **FR-034**: Every element in every query response MUST be traceable to the event identifiers
  that produced it, either inline or through FR-032.
- **FR-035**: All queries MUST be available through a command-line interface producing both
  human-readable and machine-readable output, and through a programmatic interface for future
  consumers. Read-only credentials MUST suffice for every query.

#### E. Entity resolution

- **FR-036**: Every identifier a source reports MUST be stored as an identity claim with: the
  identifier namespace (for example Kubernetes deployment, telemetry service name, repository,
  chat channel), the identifier value, supporting attributes, the source, and observed time.
  Claims MUST be stored before any merge decision is taken.
- **FR-037**: Match rules MUST be published. Each rule MUST declare the namespaces it compares,
  the conditions it requires, and whether it is *certain* or *probable*. Only certain rules
  MAY cause automated merges. Probable rules MUST produce suggestions.
- **FR-038**: Every merge, automated or human, MUST record: the entities merged, the rule or
  human decision, the confidence score, a human-readable rationale, and the claims that
  supported it.
- **FR-039**: Merges and splits MUST be reversible and MUST keep full history (section B
  applies). An identifier merged into another MUST remain resolvable to the surviving canonical
  entity.
- **FR-040**: A human decision (confirm, reject, split, manual merge) MUST be recorded as an
  event, MUST persist across replays and re-resolution, and MUST take precedence over any
  automated rule regardless of score. If a later automated rule disagrees with a human
  decision, the disagreement MUST be surfaced as a conflict, not applied.
- **FR-041**: Every human decision MUST record the authenticated identity of the individual
  who made it and when. The graph MUST NOT accept a resolution decision from an anonymous or
  shared credential.
- **FR-041a**: Every query and every human decision MUST be made through an authenticated
  individual identity from the first version. The authentication mechanism is a planning
  decision, but the requirement that an individual be identifiable is not.

#### F. Reference feeders

- **FR-042 (Topology feeder)**: A feeder MUST derive from distributed-tracing span data,
  expressed in OpenTelemetry conventions: service nodes (name, namespace, version,
  environment), `calls` edges between services with a traffic weight class per aggregation
  window, third-party dependency nodes for external systems observed as callees (databases,
  queues, external hosts), identity claims for every service name seen, and a rollout change
  node when a service's version changes. It MUST NOT emit span payloads. An edge not observed
  for a configurable number of windows MUST be retracted.
- **FR-043 (Kubernetes feeder)**: A feeder MUST derive from Kubernetes cluster state and its
  change stream: workload nodes (deployments, stateful sets, daemon sets, jobs), runs-on edges
  to cluster and node-pool infrastructure nodes, configuration nodes for config maps and
  secrets carrying only a version identifier, exposed-via edges for services and ingresses,
  owner nodes derived from configurable labels or annotations, identity claims linking
  workloads to telemetry service names where labels or annotations provide them, and change
  nodes of kinds rollout (image or revision change), scaling (replica change), and
  configuration change.
- **FR-044**: Both feeders MUST run in two modes with identical output: live against the real
  source, and offline against recorded payloads. The recorded mode is the test.
- **FR-045**: Both feeders MUST be written against the same feeder interface that any future
  connector (telemetry vendor, cloud provider, code host, chat, docs) will use, so that the
  interface is proven by two unlike sources before a third is added.
- **FR-046**: Feeders MUST use read-only credentials and MUST refuse to start with credentials
  that grant write access where the source allows that distinction.

#### G. Replay fixtures and golden outputs

- **FR-047**: This feature MUST ship at least these fixtures, each consisting of recorded source
  payloads, the resulting event stream, and golden outputs for every query in section D:
  baseline topology; rollout regression (one culprit among decoys); configuration change;
  late-arriving fact; ambiguous identity with human confirmation; retraction of a node with
  live edges; feeder gap and reconnection.
- **FR-048**: A verification run MUST, for every fixture: replay from empty and compare to
  golden outputs; deliver every event twice and verify no change; shuffle events within the
  declared reordering window and verify identical valid-time state.
- **FR-049**: The fixture format MUST be documented so that a connector author can produce a
  fixture for a new feeder without reading the graph's source code.
- **FR-050**: Acceptance of this feature requires both: (a) every fixture-based verification
  in FR-048 passing in automation, and (b) a documented live run in which both reference feeders
  are pointed at a disposable Kubernetes cluster running a demo application with tracing
  enabled, and the resulting graph matches the graph produced from recordings of that same
  run.

#### H. Operability

- **FR-051**: The graph and both feeders MUST emit telemetry about their own operation (event
  throughput, rejections, query latency, resolution decisions, feeder lag) in OpenTelemetry
  form.
- **FR-052**: The graph MUST expose its current extent: earliest and latest observed time,
  per-source last checkpoint, and known gaps, so that consumers can judge how complete an
  answer is.
- **FR-053**: The published schema (node types, edge types, change taxonomy, property
  conventions, temporal model, event schema, query contract, pointer vocabulary, match rules)
  MUST be versioned and documented as a public artifact of this feature.

### Key Entities

- **Entity (node)**: a thing in the production system or a change to it. Has a canonical
  identifier, type, display name, aliases, properties, pointers, provenance, valid interval,
  observed interval. Exists in versions.
- **Relationship (edge)**: a typed, directed link between two entities with properties,
  provenance, both intervals, and a traffic weight class for `calls`.
- **Change**: an entity of type change with a kind, summary, actor, origin reference, and
  changed-by edges to its targets. May be unattached.
- **Pointer**: kind, backend kind, backend-neutral selector. Attached to an entity version.
- **Event**: the unit of ingestion. Identifier, idempotency key, source, schema version, valid
  time, optional source sequence, payload of one event type.
- **Identity claim**: namespace, value, attributes, source, observed time. Input to
  resolution.
- **Resolution decision**: merge, split, confirm, reject. Rule or human, score, rationale,
  supporting claims, actor, time. Recorded as events.
- **Suggestion**: a pending probable match awaiting a human decision.
- **Source**: a registered feeder with its declared ordering guarantees, reordering window,
  and checkpoints.
- **Fixture**: recorded payloads + event stream + golden outputs, identified and versioned.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the reference workload (about 10,000 entities, 100,000 relationships,
  1,000,000 events), Sam receives a 2-hop neighbourhood as of any instant in under 1 second in
  95% of requests.
- **SC-002**: On the same workload, a diff over a 24-hour window with ranked changes returns in
  under 2 seconds in 95% of requests.
- **SC-003**: Every shipped fixture replays from empty to a graph identical to its golden
  outputs, on every verification run, with zero tolerance.
- **SC-004**: Double delivery of every fixture produces zero graph changes; shuffled delivery
  within the reordering window produces identical valid-time state.
- **SC-005**: On the "rollout regression" fixture family, the true culprit change ranks in the
  top three candidates in at least 90% of scenarios, and first in at least 70%.
- **SC-006**: 100% of query response elements are traceable to their producing events.
- **SC-007**: Zero automated merges result from probable rules; 100% of human decisions
  survive a full replay and re-resolution.
- **SC-008**: A connector author who has never seen the codebase can, using only the published
  schema and fixture documentation, turn a recorded payload set into a passing feeder test
  within one working day.
- **SC-009**: Zero telemetry payloads can be stored: the rejection path is exercised by a
  fixture and passes.
- **SC-010**: Both reference feeders produce identical graphs in live and recorded modes on the
  same underlying data, demonstrated on a disposable cluster running a demo application.
- **SC-011**: 100% of recorded human decisions carry an authenticated individual identity;
  zero decisions are accepted anonymously.

## Assumptions

- The graph is the substrate for a later agent feature; this feature ships no language model,
  no chat interface and no graphical interface. The command-line tool stands in for the agent.
- One graph instance models one organisation. Environments, clusters, regions and accounts are
  properties or nodes inside it (ADR-0001, D5).
- Traffic weight on `calls` edges is a coarse ordinal class emitted by the topology feeder, not
  a raw count (ADR-0001, D1).
- Interactive queries default observed time to "now"; the future evaluation harness pins it to
  the incident instant (ADR-0001, D2).
- Candidate ranking is deterministic and part of the query contract; no model participates
  (ADR-0001, D3).
- Feeders are the general connector model. The two shipped here are vendor-neutral so the
  feature is testable without vendor accounts; vendor connectors (telemetry vendor, cloud
  provider, code host, chat, docs) follow as separate features using the same interface
  (ADR-0001, D4).
- Automated merges happen only through certain rules; everything else is a suggestion until a
  human decides, via the command-line tool (ADR-0001, D6).
- Owner nodes in this feature come only from Kubernetes labels and annotations with
  configurable keys; richer ownership (on-call schedules, chat channels) arrives with later
  connectors.
- Feature flag, database schema, infrastructure-as-code, secret rotation, DNS and cloud
  maintenance change kinds exist in the taxonomy but have no feeder in this feature.
- Fan-out caps for neighbourhood expansion have sensible defaults and are caller-adjustable;
  exact defaults are a planning decision.
- Fixtures use recorded payloads from a demo application and a disposable cluster, sanitized;
  no customer data.
- Both the event log and query-able history are retained indefinitely (FR-016). Storage growth
  is monitored; a retention policy would be a future spec amendment.
- Individual authentication is required from version one (FR-041a). The mechanism (for
  example, single sign-on through the organisation's identity provider) is decided in planning.

## Out of scope for this feature

- Any language-model reasoning, hypothesis generation or testing through pointers.
- Retrieval over postmortems, runbooks or documents.
- Any connector beyond the two reference feeders.
- Any write to a production system, including proposed remediation.
- Graphical or chat interfaces.
- Multi-organisation tenancy.
