# Feature Specification: Topology Discovery and Reconciliation

**Feature Branch**: `012-topology-discovery`

**Created**: 2026-11-01

**Status**: Draft — defaults Q1–Q10 proposed, awaiting the owner's approval

**Input**: User description: "The master agent that queries the bitemporal graph does not have the
context of my infrastructure: it won't understand precisely that this service talks to that one, that
this front-end gives access to that part of that service. Even with access to the Terraform, how would
it tie it all together? I imagined a service that does auto-discovery but can also work from
declarations, very ergonomic on the client side: you declare your three or four microservices and the
front-end, the agent rebuilds the real graph of the infrastructure, proposes it back, and the client
only has to validate." Then: "It must not work only thanks to Kubernetes. A typical estate can be
Kubernetes, but it can just as well be, like mine, mostly Cloud Run on GCP, serverless, with a little
Kubernetes running GPU clusters on another cloud provider. Some customers will have no Kubernetes at
all." (owner, 2026-11-01, translated from French)

## Why this feature exists

The constitution's thesis is that accuracy comes from the graph. The investigator reasons over a
subgraph, an impact set and a diff, so the edges between services are the map it navigates by. Today
that map is thin exactly where the owner runs production:

- **No service-to-service edge on Cloud Run unless traces or APM supply one.** The GCP feeder (003)
  emits `runs_on`, `depends_on` (Cloud SQL), `exposed_via` and `owned_by`. It does not read IAM
  invoker bindings, Pub/Sub push subscriptions, Eventarc triggers or Cloud Scheduler jobs, which are
  how serverless services call each other.
- **Asynchronous flows are invisible.** The OpenTelemetry feeder ignores `messaging.*` attributes and
  span kind, so a producer span without an address yields no edge.
- **Edges do not say where they come from.** An edge's provenance is the events that produced it.
  Nothing records whether it was *declared* by configuration or *observed* in traffic, so nobody can
  tell a stale declaration from a transient call.
- **Nobody owns the reconciled picture.** Feature 009 gives the agent its own list of services and
  left topology to the feeders on purpose. Each feeder asserts its own edges, and no step compares
  them, proposes the result to a person, or remembers what that person decided.

The literature review of 2026-11-01 (F3) found that the dominant failure of LLM investigators is
omission: decisive evidence that was reachable and never queried. Its best-performing defence is a
**Grounder**, a profile of the search space handed to the agent before its first query. The 002
amendment of the same day requires that profile (002 FR-012a) and an examination report that names
what was not queried (002 FR-022b). Both read what this feature builds.

## Clarifications

### Session 2026-11-01 — defaults proposed for the owner's approval

Each default is written into the requirements below. Following 006–009, they are defaults rather than
`[NEEDS CLARIFICATION]` markers: the owner approves or changes them on the spec PR.

- Q1: What does the core schema know about runtimes? → **Default: nothing.** The core keeps 001's node
  and edge types. Every entity gains a **layer** facet: `entrypoint`, `service`, `workload`,
  `managed_resource`, `host`, `external`. Every entity can also carry a **platform** facet using
  OpenTelemetry `cloud.provider` and `cloud.platform` values. Kubernetes, Cloud Run, Lambda, Cloud
  Functions and virtual machines are **adapters** that map onto these. Instances are never entities:
  topology is held at workload or revision level, because instances churn and their identity carries
  no diagnostic meaning (review F5, F6). (FR-001–FR-005)
- Q2: How does an edge say where it comes from? → **Default: a provenance class on every edge
  assertion.** The classes are:
  - `observed`: traffic was seen, in traces, APM dependencies or flow data;
  - `declared`: configuration says the flow is allowed or wired, for example an invoker permission, a
    subscription, a route, an endpoint reference or a declared file;
  - `suggested`: a probable rule or a model proposes the flow; it never asserts it.

  The class sits next to the source and the producing events. It is a minor, additive schema change.
  (FR-010–FR-013)
- Q3: How are declared and observed edges combined? → **Default: a reconciliation state per
  service-level edge, computed as a rebuildable projection.** The states are:
  - `confirmed`: declared and observed;
  - `declared_only`: possibly stale, or a path too rare for the observation window;
  - `observed_only`: an undeclared dependency;
  - `stopped`: observed and confirmed before, no longer observed while still declared or validated;
  - `ended`: observed only, then retracted, with nothing declaring or validating it;
  - `rejected`: refused by a person.

  `intermittent` is a flag on an edge, not a state (Q7).

  The projection is never a stored truth of its own (constitution I). (FR-020–FR-025)
- Q4: How does the client validate? → **Default: a topology plan, reviewed like a diff.**
  `aisre topology plan` lists what changed since the last review: new edges with their provenance and
  evidence, new groupings of workloads under a catalog entry, edges that stopped, and conflicts. Each
  item is accepted or rejected as a human decision (001 FR-040). A rejection is durable: the item is
  not proposed again unless a source of a different provenance class later asserts it, and then it is
  listed as a conflict. An unreviewed edge is usable at once and is marked unreviewed wherever it is
  shown or cited, so a fresh install is not blocked on review, as in 009. (FR-030–FR-036)
- Q5: Which new sources does this feature add? → **Default: the serverless gap first, then the rest.**
  - P1, Cloud Run (extending 003's feeder; each is read-only):
    - IAM `roles/run.invoker` bindings, as `declared` "may call" edges;
    - Pub/Sub push subscriptions, Eventarc triggers and Cloud Scheduler jobs, as `declared`
      asynchronous or scheduled edges to their target service;
    - the URL map's host and path rules, carried on the existing `exposed_via` edge, so a route says
      which part of the front door reaches which service. Under 003 FR-149 the load-balancer path is
      no longer the first work deferred; 003 FR-057 still lets it be omitted when the credential lacks
      the permission, and the omission is then a blind spot (FR-082);
    - environment variables whose value names a known endpoint, as `declared` edges. The graph keeps
      only the key name and the matched entity, as 003 FR-033 and FR-034 require. 003 FR-034 is
      amended exactly as it was on 2026-10-09 for Cloud SQL connection names: a recording may keep
      the matched endpoint, pseudonymised, so that the match replays (FR-048).
  - P1, OpenTelemetry: `messaging.system`, `messaging.destination.name` and span kind
    (producer/consumer), as asynchronous edges through the broker resource.
  - P2, Kubernetes: the Kubernetes feeder already emits `exposed_via` from Ingress rules and Service
    selectors. This feature adds Gateway API routes and the provenance class `declared` on all three.
  - P2: a **declared topology file** (GitOps), imported as human decisions, in the same file family as
    009's catalog file.

  Terraform stays out, as 003 decided; the audit log captures its effects. (FR-040–FR-049)
- Q6: How are asynchronous flows represented? → **Default: through the broker, with a mode.** Edges
  follow the data: the producer has a `calls` edge *to* the topic or queue (a `managed_resource`)
  with `role = publish`, and the topic has a `calls` edge *to* each consumer with `role = consume`.
  The published modes are `sync`, `async` and `scheduled` (a scheduler invoking a target). A derived
  service-to-service edge with `mode = async` is projected for navigation. These are additive
  attributes; no new edge type is introduced. Propagation differs by direction, so the role
  matters: a dead consumer builds a backlog, a dead producer starves the consumer. (FR-050–FR-052)
- Q7: How long before an observed-only edge is proposed, and how is flapping avoided? → **Default: a
  published observation window, initially 7 days.** That is long enough for rare paths (batch, failover,
  weekly jobs) to appear despite sampling. A proposal is raised once per state change. An edge that
  alternates faster than the window is reported once as `intermittent`, not re-proposed each time. The
  feeders' own retraction windows (OTel 6×5 min, Datadog 4 reads) still drive the graph's valid
  intervals; this window drives only proposals. (FR-024, FR-025)
- Q8: How are estates that span clouds or platforms handled? → **Default: no network discovery.** An
  edge that crosses a platform or provider boundary comes from traces or declarations only. A boundary
  that the catalog says is crossed, with no edge evidence, is a **blind spot** reported in the
  search-space profile. eBPF and flow-log discovery stay out (ADR-0019 D5): they need a privileged
  agent per host and cannot run on managed serverless. (FR-060–FR-062)
- Q9: How does this relate to 009? → **Default: the catalog names, the topology connects.** A catalog
  entry says what a service is and what it is called in each source. This feature says which entities
  are linked and how. A plan item may propose grouping workloads under an existing catalog entry; it
  never creates or renames an entry, and it never merges entities (001 SC-007, 009 FR-051).
  (FR-070–FR-072)
- Q10: What does the investigator receive? → **Default: the search-space profile of 002 FR-012a,
  enriched.** For the investigation's scope, it gets:
  - each edge with its provenance classes, reconciliation state, mode and review status;
  - each entity with its layer, its platform and the signal families its pointers cover;
  - the blind spots: entities with no pointer, declared-only edges, uncovered boundaries, lagging
    sources.

  002 FR-022b's examination report reads the same profile to decide what was reachable.
  (FR-080–FR-083)

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A Cloud Run estate with no Kubernetes gets a usable topology on day one (Priority: P1)

An operator connects GCP and OpenTelemetry. The estate is a front door (load balancer, URL map) and
six Cloud Run services, one Cloud SQL instance and two Pub/Sub topics. There is no Kubernetes. Within
the first ingestion, before any traffic window has elapsed, the graph shows:
- the routes from the front door to each service, with their host and path;
- the invoker permissions between services, as declared edges;
- the push subscriptions, as declared asynchronous edges.

As traces arrive, the declared edges that see traffic become confirmed, and an undeclared call to an
external payment API appears as observed-only.

**Why this priority**: it is the owner's estate and the gap the audit found. Without it the
investigator's impact set on Cloud Run is the alerting service alone.

**Independent Test**: replay a Cloud Run fixture (recorded IAM policies, subscriptions, URL map, and an
OTel stream) with no Kubernetes feeder. Assert:
- the expected edges with their provenance classes and reconciliation states;
- that `impact` on the database returns the services that depend on it;
- that no step requires a Kubernetes object.

**Acceptance Scenarios**:

1. **Given** an invoker binding from service A's identity on service B, **Then** a declared edge A→B
   exists with the binding as evidence.
2. **Given** a push subscription from topic T to service C and an OTel producer span from A on T,
   **Then** A→T (publish, observed), T→C (consume, declared) and a derived async edge A⇢C exist.
3. **Given** a URL map rule `api.example.com/candidates/*` → backend → serverless NEG → service D,
   **Then** D's `exposed_via` edge carries that host and path.
4. **Given** the same estate described by Datadog APM instead of OTel, **Then** the reconciled states
   are the same (vendor-free twin, as 009 SC-006).

---

### User Story 2 - The client reviews the proposed topology as a plan (Priority: P1)

After the first week, the operator runs `aisre topology plan`. It shows:
- 14 services grouped under 9 catalog entries;
- 37 edges: 22 confirmed, 9 declared-only, 6 observed-only;
- 2 proposed groupings and 1 conflict.

The operator accepts the observed-only call to the payment API. They reject a declared-only edge left
by an old invoker binding, with the note "decommissioned". They accept one grouping. A week later the
plan lists only what changed since that review.

**Why this priority**: the owner asked for validation, not configuration. A topology nobody reviewed is
a guess the investigator cannot weigh.

**Independent Test**: replay one week, run the plan, apply recorded decisions, replay a second week.
Assert:
- the second plan contains only new items;
- the rejected edge is not proposed again;
- a full replay reproduces every decision.

**Acceptance Scenarios**:

1. **Given** unreviewed items, **Then** each appears once in the plan with its provenance, evidence and
   proposed action.
2. **Given** a rejection, **When** the same source asserts the edge again, **Then** it is not proposed.
   **When** a source of another provenance class asserts it, **Then** it is listed as a conflict.
3. **Given** an accepted edge, **Then** it is rendered as reviewed everywhere it is cited, with the
   reviewer's identity.
4. **Given** a declared topology file in the repository, **Then** importing it records its items as
   human decisions attributed to the importing identity, and it never silently undoes a later decision
   made on the command line.

---

### User Story 3 - The investigator knows where to look, and says where it did not (Priority: P1)

A Cloud Run service raises errors. The investigation's search-space profile lists its callers and
callees with their reconciliation states, which signal families each one's pointers cover, and two
blind spots: a callee with no metrics pointer, and a declared-only edge to a queue. The investigator
queries the latency of the confirmed callees and finds nothing, then queries the queue's backlog
metric, which the profile listed as reachable. The backlog explains the errors. Had it stopped before
querying the backlog, 002 FR-022b would have held the hypothesis at *inconclusive* and named the
unqueried family.

**Why this priority**: this is the failure the owner reported on 2026-11-01, an agent that concluded
without reading the metrics that mattered, and the one the literature measured as dominant.

**Independent Test**: an incident fixture whose decisive evidence is on an asynchronous dependency, run
twice: with the profile, and with the profile emptied. Assert that the run with the profile reaches
the culprit, or, if it stops early, that its examination report names the unqueried decisive family.

**Acceptance Scenarios**:

1. **Given** an investigation scope, **Then** its profile is recorded with the investigation and
   re-renders identically from the replayed event log.
2. **Given** an entity with no pointer for a signal family, **Then** that gap is listed as a blind spot,
   not as an omission.

---

### User Story 4 - A flow that stops is a fact, not an absence (Priority: P2)

At 14:02 a deploy changes a service's configuration, and its calls to the inventory service stop. The
inventory service raises no error; it just stops receiving requests. The confirmed edge becomes
`stopped` at 14:02 and the graph's `diff` for the window reports it. The retraction reaches the graph
only after the feeder's retraction window (30 minutes for OpenTelemetry, 60 for Datadog). A live
investigation pinned before that sees the stop through 002 FR-046b's comparison of the callee's request
rate. A replay or a later investigation sees the stopped edge itself in `diff` and in the first wave.

**Why this priority**: the literature's clearest case of a shared blind spot. Agents that walk the call
chain, and statistical rankers that score only rises, both missed a service that stops receiving calls
(review F1).

**Independent Test**: a fixture where the root cause produces only a stopped flow. Assert the edge's
state, its presence in `diff`, and its presence in the first wave.

**Acceptance Scenarios**:

1. **Given** a confirmed edge that the feeder retracts, **Then** its state becomes `stopped` from the
   retraction's valid instant.
2. **Given** an edge that was only ever observed and is retracted, **Then** it is reported as ended, not
   as `ended`, not as `stopped`, and raises no plan item until the observation window elapses.
3. **Given** a live investigation pinned inside the retraction window, **Then** the callee's fall in
   request rate is in its first wave.

---

### User Story 5 - An estate that spans two clouds (Priority: P2)

The owner's API on Cloud Run calls an inference service on a GPU Kubernetes cluster at another
provider, through a public endpoint. No network path is visible between them. The OTel spans from the
API carry `server.address` of the inference endpoint. The cluster's own feeder knows the inference
service. The edge between them is observed from traces and declared by the API's environment-variable
reference. Both entities carry their platform facet. If the traces stop carrying the address, the
boundary appears as a blind spot in the profile.

**Why this priority**: the owner's real test estate, and the general shape of mixed estates.

**Independent Test**: a fixture with two platforms and one cross-boundary edge, with and without its
trace evidence.

**Acceptance Scenarios**:

1. **Given** a cross-boundary call in traces, **Then** the edge is observed and both ends carry their
   platform.
2. **Given** a catalog entry spanning both platforms with no edge evidence, **Then** the profile lists a
   blind spot naming the boundary.

### Edge Cases

- **An invoker binding to `allUsers` or a whole domain**: it declares public reachability, not a
  dependency. It yields no service-to-service edge, and the service is marked publicly invocable.
- **One identity used by many services**: a binding to a shared service account declares edges from
  every service running as that account. They are proposed as one grouped plan item with the account
  as the reason, not as N separate items.
- **An environment variable that names an endpoint through a secret**: the secret's value is never
  read. If the key alone matches no known endpoint, no edge is asserted.
- **A traffic split across revisions**: edges attach to the service; revisions keep `runs_on`, so a
  split never doubles the topology.
- **A topic with no subscriber**: the publish edge exists and the derived async edge does not. The topic
  is listed as a dead end in the plan, once.
- **Sampling hides a rare call**: it stays declared-only until observed. It is never proposed for
  rejection because it is rare.
- **Two plan items contradict each other**, for example an accepted edge and a rejected grouping that
  includes it: the most recent human decision wins, and the conflict is listed (009 edge case).
- **A text field contains an instruction to the engine**, such as a description or a label: it is data
  (002 FR-017).

## Requirements *(mandatory)*

### A. Runtime-agnostic entities

- **FR-001**: The core graph schema MUST name no runtime, orchestrator or cloud vendor. Runtime-specific
  knowledge MUST live in adapters that map their objects onto 001's node and edge types.
- **FR-002**: Every entity MUST be able to carry a **layer** facet from the published set
  `entrypoint`, `service`, `workload`, `managed_resource`, `host`, `external`.
- **FR-003**: Every entity MUST be able to carry a **platform** facet expressed in OpenTelemetry
  `cloud.provider` and `cloud.platform` values, or a documented extension where none exists
  (constitution IV).
- **FR-004**: Short-lived instances (containers, pods, function invocations) MUST NOT become entities.
  Topology MUST be held at workload or revision level.
- **FR-005**: Every requirement of this feature MUST be satisfiable on an estate with no Kubernetes
  connector, and the fixture corpus MUST prove it (FR-091).

### B. Edge provenance

- **FR-010**: Every edge assertion MUST carry a provenance class from `observed`, `declared`,
  `suggested`, alongside its source and producing events.
- **FR-011**: A `suggested` edge MUST NOT be asserted into the graph. It exists only as a plan item
  until a human accepts it or a declared or observed source asserts it.
- **FR-012**: An edge asserted by several sources MUST keep each assertion with its own class and
  source; no assertion overwrites another.
- **FR-013**: The provenance class MUST be part of the published schema under a minor version bump,
  with a migration that marks every existing edge with the class implied by its source.

### C. Reconciliation

- **FR-020**: For every service-level edge, the system MUST compute a reconciliation state among
  `confirmed`, `declared_only`, `observed_only`, `stopped`, `ended`, `rejected`, as of any pair of
  instants.
- **FR-021**: The reconciliation state MUST be a projection rebuildable from the event log, with no
  state of its own (constitution I, III).
- **FR-022**: An edge MUST become `stopped` when a previously observed edge is retracted while it is
  still declared or has been accepted by a person. The transition's valid instant MUST be the
  retraction's.
- **FR-023**: The graph's `diff` MUST report state transitions to `stopped` inside its window, as
  changes of the edge.
- **FR-024**: An `observed_only` edge MUST NOT raise a plan item before the published observation
  window has elapsed since its first observation.
- **FR-025**: An edge whose observation alternates faster than the observation window MUST be flagged
  `intermittent`, reported once, and MUST NOT raise one plan item per alternation. The flag does not
  change its state.

### D. The topology plan and human decisions

- **FR-030**: The operator MUST be able to list, from the command line and the API, a topology plan:
  every unreviewed edge, grouping, stopped edge and conflict since the last review, each with its
  provenance, evidence and proposed action.
- **FR-031**: The operator MUST be able to accept or reject each plan item, singly or by group, as a
  human decision with authenticated identity and instant (001 FR-040, FR-041).
- **FR-032**: A rejected item MUST NOT be proposed again from a source of the same provenance class. An
  assertion from a different class MUST be listed as a conflict, not applied.
- **FR-033**: An unreviewed edge MUST be usable by queries and investigations at once, and MUST be
  marked unreviewed wherever it is shown or cited.
- **FR-034**: The operator MUST be able to declare edges and groupings in a versioned file, in the same
  file family as 009's catalog file. An import MUST be recorded as human decisions by the importing
  identity, and MUST NOT silently undo a later decision.
- **FR-035**: Decisions MUST persist across replays and re-reconciliation, MUST be undoable with their
  history, and MUST apply from their recorded instant: investigations pinned before a decision replay
  unchanged.
- **FR-036**: An audit query MUST answer, for any edge, "why does this edge exist, in which state, and
  who reviewed it?"

### E. New sources

- **FR-040**: The GCP feeder MUST read IAM policies on Cloud Run services and assert a `declared` edge
  from every service whose runtime identity holds `roles/run.invoker` on another service.
- **FR-041**: Bindings to `allUsers`, `allAuthenticatedUsers` or a domain MUST NOT yield an edge. They
  MUST mark the service as publicly invocable.
- **FR-042**: The GCP feeder MUST read Pub/Sub push subscriptions, Eventarc triggers and Cloud Scheduler
  jobs, and MUST assert `declared` edges to their target service: from the topic with
  `mode = async, role = consume` for a push subscription or an Eventarc trigger, and from the job with
  `mode = scheduled` for a scheduler job.
- **FR-043**: The GCP feeder MUST carry the URL map's host and path rules on the `exposed_via` edge that
  003 already asserts. 003 FR-149 is amended so the load-balancer path is no longer the first work
  deferred under budget. Where 003 FR-057 omits it for lack of permission, the omission MUST be
  reported as a blind spot (FR-082).
- **FR-044**: The GCP feeder MUST match each environment-variable value against the published
  endpoints of known entities at ingestion. It MUST assert a `declared` edge on a match, and MUST keep
  only the key name and the matched entity in the graph (003 FR-033, FR-034). 003 FR-034 MUST be
  amended, as on 2026-10-09 for Cloud SQL connection names, so that a recording may keep the matched
  endpoint, pseudonymised, and the match replays. No other part of the value is recorded.
- **FR-045**: The OpenTelemetry feeder MUST read `messaging.system`, `messaging.destination.name` and
  span kind, and MUST assert `observed` edges with `mode = async` and `role = publish | consume` to the
  broker resource.
- **FR-046**: The Datadog feeder's service-dependency edges MUST carry the class `observed`.
- **FR-047**: The Kubernetes feeder's existing `exposed_via` edges from Ingress rules and Service
  selectors MUST carry the class `declared`, and the feeder MUST also read Gateway API routes into
  equivalent edges (priority P2).
- **FR-048**: Every new source MUST use read-only credentials (constitution VII) and MUST ship with
  recorded, sanitised real payloads (constitution VIII).
- **FR-049**: Terraform, Cloud Asset Inventory, flow logs and eBPF MUST NOT be sources in this feature.

### F. Asynchronous edges

- **FR-050**: Asynchronous flows MUST be represented as `calls` edges that follow the data, from the
  producer to the broker resource (`role = publish`) and from the broker to each consumer
  (`role = consume`), with `mode = async`. The published modes are `sync`, `async` and `scheduled`.
  No new edge type is introduced.
- **FR-051**: A derived service-to-service edge with `mode = async` MUST be projected when a publish
  and a consume share a broker resource. It MUST be marked derived and cite both halves.
- **FR-052**: `impact` and the propagation paths of 002 FR-019b MUST be able to traverse asynchronous
  edges, and MUST distinguish their direction of effect by role.

### G. Boundaries

- **FR-060**: An edge across a platform or provider boundary MUST come from traces, APM or a
  declaration. No network-level inference is in scope.
- **FR-061**: An entity group (a catalog entry or a declared boundary) that spans platforms with no edge
  evidence across the boundary MUST be reported as a blind spot.
- **FR-062**: Platform facets MUST be preserved on both ends of every cross-boundary edge.

### H. Relation to 009 and 001

- **FR-070**: A plan item MAY propose grouping workloads under an existing catalog entry. It MUST NOT
  create, rename or merge a catalog entry, and MUST NOT merge entities (001 SC-007, 009 FR-051).
- **FR-071**: Edges MUST connect graph entities, not catalog aliases. The plan and the profile MUST be
  renderable grouped by catalog entry.
- **FR-072**: 001's resolution rules MUST be unchanged by this feature. Matching an endpoint to an
  entity (FR-044) is a correlation, not an identity claim.

### I. The search-space profile

- **FR-080**: The system MUST produce, for any scope and pair of instants, the search-space profile of
  002 FR-012a, enriched with each edge's provenance classes, reconciliation state, mode, role and
  review status.
- **FR-081**: The profile MUST list, for each entity, its layer and platform, and the signal families
  its pointers cover per connected source.
- **FR-082**: The profile MUST list blind spots: entities with no pointer for a signal family,
  `declared_only` edges, boundaries without evidence (FR-061), and sources disconnected or lagging.
- **FR-083**: The profile MUST be deterministic for a given graph state and MUST stay within a published
  size cap. When the cap truncates, the truncation MUST be stated, as 002 FR-014 requires of digests.

### J. Evaluation

- **FR-090**: A topology evaluation MUST report, on labelled fixtures:
  - edge precision and recall per provenance class;
  - accuracy of the reconciliation state;
  - the number of plan items a reviewer needs to reach a validated topology;
  - the rate of re-proposals after rejection, which MUST be zero.
- **FR-091**: The fixture set MUST include:
  - a Cloud Run estate with no Kubernetes;
  - a mixed estate spanning two providers (User Story 5);
  - a vendor-free twin of a Datadog case;
  - a stopped-flow incident (User Story 4);
  - an asynchronous-culprit incident (User Story 3).
- **FR-092**: 002's evaluation MUST be run with and without the profile on the incident fixtures, and
  MUST report the difference in omission rate and propagation-path edge F1 (002 FR-059a).

### Key Entities

- **Layer and platform facets**: where an entity sits (entry point, service, workload, managed
  resource, host, external) and what it runs on, in OpenTelemetry terms.
- **Edge assertion**: one source's claim that an edge exists, with its provenance class, mode, role and
  evidence.
- **Reconciliation state**: the computed status of a service-level edge across its assertions and human
  decisions.
- **Topology plan item**: an edge, grouping, stopped edge or conflict awaiting review, with its proposed
  action.
- **Topology decision**: a human acceptance or rejection, with identity, instant and the item it
  decides.
- **Search-space profile**: the per-investigation map of 002 FR-012a, enriched by this feature.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the Cloud Run fixture with no Kubernetes, at least 90 % of the labelled
  service-to-service edges exist after the first ingestion and before any traffic window. At least 95 %
  exist after one observation window.
- **SC-002**: Edge precision is at least 95 % for `observed` and `declared` assertions on the labelled
  fixtures.
- **SC-003**: A new operator reaches a validated topology of the owner's estate by reviewing plan items
  only, without writing configuration, in under 30 minutes for up to 20 services.
- **SC-004**: Zero items are re-proposed after a rejection from the same provenance class, across a full
  replay.
- **SC-005**: On the stopped-flow fixture, in 100 % of runs: the stopped edge is in the window's `diff`
  and in the first wave when the investigation is pinned after the feeder's retraction window; and the
  callee's fall in request rate is in the first wave when it is pinned before.
- **SC-006**: On the incident fixtures, the omission rate (002 FR-059a) with the profile is lower than
  without it, and propagation-path edge F1 is higher. Both are reported paired on the same fixtures and
  models.
- **SC-007**: The vendor-free twin reaches the same reconciled states as its Datadog twin.
- **SC-008**: No requirement fails on the fixture with no Kubernetes connector.

## Out of scope for this feature

- Network-level discovery: eBPF, flow logs, packet capture (ADR-0019 D5).
- Terraform state or plans as a source (003's decision stands).
- Inferring edges from metric correlation alone. A correlation may be a `suggested` plan item in a later
  feature; it is never asserted.
- Writing the reconciled topology back to any external catalog or IaC.
- Learning which signal families are decisive from past investigations. The expectation table of 002
  FR-022b is published and versioned by hand, and learning it belongs to the gated learning direction
  of ADR-0019 D4.
- A graphical topology viewer beyond what the command line and API render.

## Assumptions

- Features 001–009 are merged, and the 002 amendment of 2026-11-01 is approved. This feature extends
  003's GCP feeder, the OTel and Kubernetes feeders of 001, and 005's Datadog feeder; it replaces none
  of them.
- Estates hold tens to a few hundred services. The reference workload of constitution X still bounds
  query budgets, and no dedicated graph database is introduced.
- The owner's estate (Cloud Run on GCP, a GPU Kubernetes cluster at another provider, Datadog) is the
  first live case, recorded under the live-installation item of ADR-0016 D5.
- IAM policies on Cloud Run are already readable with `roles/run.viewer`, which the 003 access request
  lists. The Pub/Sub, Eventarc and Cloud Scheduler viewer roles are not listed, and
  `roles/compute.viewer` is listed only as optional. The access request is amended before the recording
  campaign.
