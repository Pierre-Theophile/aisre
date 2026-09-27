# Feature Specification: Datadog Connector

**Feature Branch**: `005-datadog-connector`

**Created**: 2026-09-17

**Status**: Draft

> **Renamed from 003 and rescoped by ADR-0004 on 2026-09-17.** The September 2026 coverage audit
> found that this organisation has no distributed tracing anywhere and uses Datadog for **one
> service's logs and a handful of monitors**. The APM service-map story that justified building
> this connector first therefore has no input here. Datadog becomes feature **005**, after the
> GCP integration (003) and the deploy feeders (004), and its scope narrows to three things:
> a **log-search telemetry backend**, **monitor intake**, and an **optional** APM topology
> capability that is **off by default** for organisations that do have tracing. What was removed
> or demoted is listed under **[Removed in rescoping](#removed-in-rescoping)** so a reviewer can
> see the delta rather than diff two drafts.

**Input**: User description: "The vendor connector for the observability estate that actually
exists: Datadog. One connector with two halves sharing one read-only client. The half that
matters here is a **telemetry backend** that executes the investigation engine's query algebra
live over the services whose **logs** live in Datadog and returns bounded digests. The second
half is **monitor intake**: every monitor becomes an ALERT node and every state transition an
idempotent `alert.transition` event, polled as the truth with an optional webhook as a doorbell.
APM topology — the service list, the service map, span search — is an **optional capability, off
by default**, for organisations that have tracing; this one does not. Recorded, sanitised
payloads from a real organisation are the tests. Read-only, no writes of any kind."

## Why this feature exists

Feature 001 built a substrate and proved it with two vendor-neutral feeders against a disposable
cluster. Feature 002 builds an investigation engine and proves it against recorded fixtures.
Feature 003 gives the graph the cloud platform the organisation actually runs on, and feature 004
gives it the deploys. What none of them can do is **look at the telemetry**.

That is this feature, and the coverage audit (`docs/evaluation/coverage-audit-2026-09.md`)
decides how much of it is worth building. The audit's findings, restated as the premises of this
specification:

- The organisation has **no distributed tracing anywhere**. The OpenTelemetry topology feeder has
  no input, and neither does Datadog APM. A service map read from a vendor that observes no spans
  is an empty answer, not a small one.
- Datadog holds **one service's logs and a handful of monitors**. That is the whole footprint.
- The changes that cause incidents here are Cloud Run revisions, GitHub Actions deployments and
  Vercel promotions — features 003 and 004 — and **vendor notices**, not events posted to
  Datadog's event stream.
- Incidents are declared by creating a dated severity channel in Slack. The organisation's
  incident-management product is **unused**, in Datadog and everywhere else.

So three things become true with this feature, and one thing that an earlier draft promised does
not:

1. **The pointers can be executed.** Feature 001 established that the graph stores *where to
   look*, never what was measured. That promise has never been cashed against Datadog: the
   algebra's `new_log_patterns` and `errors_by_version` are answerable from **logs alone**, and
   they are the two operations that matter for the one service whose logs live here. `compare`
   runs over log-derived metrics and over monitor queries. `error_spans` answers a typed
   `no_data` whose coverage says **no tracing** — which is a true, auditable answer, and a far
   better one than a service map assembled from nothing.
2. **The monitors are real alerts.** Every monitor in the organisation becomes an ALERT node with
   its state history in valid time, and every transition an idempotent `alert.transition` event.
   The audit found that monitor transitions caught few of the incidents — the Slack channel is the
   real trigger, and spec 002 owns that path — but a monitor that fired is evidence either way,
   and the graph's ALERT node type has had no feeder behind it until now.
3. **The estate is real.** Tag cardinality, renamed services, environments that disagree, a
   shared rate limit the humans are also spending during an incident: these only appear against a
   real organisation, and this is where the connector contract meets one.

And the thing that is no longer promised: **this connector is not where the topology comes
from.** For an organisation with tracing, the APM capability of section B still maps the service
list and the service map exactly as specified — but it is off unless someone turns it on, and
nothing else in this feature depends on it.

The connector is deliberately one connector with two halves, because they are one credential,
one rate-limit budget and one vocabulary. The feeder half emits events; the telemetry backend
half never emits anything into the graph and never stores a payload in it.

## Clarifications

### Session 2026-09-17

The three open questions were put to an outside reviewer (`docs/reviews/2026-09-17-external-review-002-003.md`),
whose answers the owner adopts. They are recorded here and applied throughout the specification.
The `(Q1)`, `(Q2)` and `(Q3)` markers that used to mean "undecided" now name the decision below.

**Superseded in part by ADR-0004 (same day, after the coverage audit).** Q1's answer about
Datadog Incident Management — "deferred as a graph source, pulled as evaluation labels" — is now
moot: the audit found the product **unused**, and the organisation declares incidents by creating
a dated severity channel in Slack. Incidents leave this specification entirely; the labels and
the intake path both belong to spec 002 (ADR-0004 D4). The rest of Q1 (record by incident
density, fault-injection game days, boring windows, start the feeder before the scope is settled)
stands unchanged, as do Q2 (sanitisation) and Q3 (polling with a doorbell) in full.

- Q: Scope of the first recording campaign — which Datadog environment(s) and which services, and
  is Datadog Incident Management in use? → A: Choose the flow by **incident density, not business
  importance**: record where incidents actually happen, because an important but quiet estate
  teaches nothing. Schedule **fault-injection game days in staging** as a source of labelled
  ground truth the project controls. Record **boring periods and false-positive alerts** too,
  where the correct answer is "nothing implicated". **Start the feeder immediately**, before the
  scope is finally settled, because topology history cannot be reconstructed after the fact.
  ~~Datadog Incident Management is deferred as a graph source, but incident records are pulled as
  labels for the evaluation corpus.~~ **Superseded by ADR-0004** — the product is unused and both
  halves leave this specification; see the paragraph below and **Removed in rescoping**.
  (FR-007, FR-070a–FR-070d, US7)
- Q: Sanitisation policy — which tag keys and payload fields may be recorded verbatim, which must
  be hashed, which must be dropped, and who signs a recording off? → A: **People identifiers are
  dropped, not hashed** — a hashed email address is still personal data. **Infrastructure
  identifiers are pseudonymised with a keyed HMAC**, consistent across a corpus so joins survive.
  **Log messages are stored as mined templates with their variables masked**, never as raw lines.
  **Monitor bodies are dropped**; the name, query, thresholds and tags are kept. Sanitisation
  happens **in the connector, before anything touches disk**, and the same boundary governs what
  may be sent to a model provider. At commit an **independent secrets-and-entropy scan** runs, and
  **canary tokens** seeded into the source data are asserted absent. **Real production recordings
  are not committed to the public repository in v1**: the corpus is private with private CI, and
  the public repository carries **synthetic structural twins**. Sign-off is a **signed manifest
  carrying the policy version and the content hash**, four-eyes once a second person exists.
  (FR-052, FR-069a, FR-072–FR-076, FR-076a–FR-076d, SC-011, SC-017, SC-018)
- Q: Alert intake — a Datadog monitor webhook (push) or polling of monitor state transitions
  (pull) in v1? → A: **Polling in v1**, every 15–30 seconds, which is small next to monitor
  evaluation lag. Intake is modelled as **one more feeder emitting idempotent `alert.transition`
  events keyed on the published 4-tuple (source, stable alert identifier, group, transition
  instant)** (ADR-0005 D2). Webhook and polling are **two transports
  for the same event** and may run together — push for speed, pull for truth. The webhook is a
  **doorbell**: it enqueues "poll now", the connector re-reads state from the API and **never
  trusts the body**; a shared-secret header plus rate limiting is sufficient. Filter transitions
  **by tag**; **ignore flapping and no-data transitions** as investigation triggers; **recoveries
  are evidence**. (FR-024, FR-025, FR-025a–FR-025c, SC-003, SC-020)

### Session 2026-09-17 (c)

Remediation of the `/speckit-analyze` cross-artifact pass over features 002–005. Findings closed
here: **X1** (the algebra's published home and the eight telemetry terms), **X2** (the actor-kind
set), **X3** (the `alert.transition` idempotency key) and **I12** (enum names in normative text).

- Q: Which algebra terms must this backend serve, and where is the term list published? (X1 —
  FR-040a, FR-040b, Key Entities "Query algebra operation") → A: the list is **not restated here**.
  `specs/002-investigation-engine/contracts/telemetry-backend.md` §1 is its single home (ADR-0005
  D7), and this backend serves that document's **telemetry family in full — all eight terms**
  (`compare`, `onset`, `new_log_patterns`, `error_spans`, `errors_by_version`, `monitor_state`,
  `exemplars`, `drill_down`), never the graph or knowledge families. The rescoping answer is
  unchanged and now covers all eight: a term whose data source this organisation lacks answers
  `no_data` with a coverage block naming the absent source (FR-040b, FR-049b), which is what
  `error_spans` does without tracing. `onset` is served **backend-side** — the Datadog query
  returns the series to the backend process, which runs the published change-point method there and
  returns only the onset digest — so raw samples never cross the digest boundary.
- Q: What is the published actor-kind set, and what keys an `alert.transition`? (X2, X3, I12 —
  FR-033a, FR-033b, FR-025, Key Entities "Actor kind") → A: `{PERSON, AUTOMATION, CONTROLLER,
  VENDOR, UNKNOWN}` (ADR-0005 D1), replacing this spec's earlier `human/system/vendor/unknown`:
  `system` maps to `CONTROLLER` or `AUTOMATION` by derivation from what Datadog states, else
  `UNKNOWN`. Enum names are used in normative text with the prose gloss in parentheses. The
  `alert.transition` key is the 4-tuple **(source, stable alert identifier, group, transition
  instant)** (ADR-0005 D2, 002 FR-008b) — for Datadog: source `datadog`, the monitor identifier,
  the monitor group, and the transition instant Datadog reports.

### Session 2026-09-27

A read-only look at the organisation's Datadog logs before planning (7 days, 2026-09-20 to 27)
found that the assumption behind `errors_by_version` did not hold. The logs came from one source
(LiveKit Cloud voice agents, about 37 M lines a week, 3,606 of them error-level), deployed by a
platform none of features 003 and 004 feeds. The only `version` present was the agent SDK's
(`1.3.6`, on 255 worker-start lines, constant all week), and no revision, image, commit or release
attribute existed on any line. Datadog's SQL `version` and `env` columns came back empty even where
search showed a value, because both were remapped from custom attributes.

- Q: How does `errors_by_version` work when the logs carry no deploy identifier, and how does it
  stay independent of how a service is deployed? → A: **The service stamps its own logs, and the
  connector joins on what the stamp says, not on who deployed it.** The project is generic: it must
  work for Cloud Run, Kubernetes, Vercel, a vendor-hosted runtime like LiveKit Cloud, a VM, or any
  deployment type added later. So (1) the connector **discovers** which log attribute carries the
  deployed version from a published, ordered list of conventions, configurable per service, and
  mints it as the log pointer's `join_keys["version"]` (FR-040c); (2) each version value is
  **normalised into the platform-neutral `deploy.*` vocabulary** — a full commit sha, an image
  digest, or a release identifier — so an `errors_by_version` group names the same identifier a
  deploy feeder's change claims, whichever feeder that is (FR-040d); (3) where no convention
  matches, the answer is `NO_DATA` naming every convention searched (FR-040b); and (4) the project
  publishes how to stamp the version for each deployment type, with the full commit sha as the
  recommended value because it is the one identifier every deploy source states (FR-040e). The
  other two options — accept `NO_DATA` only, or build a feeder for the one platform this
  organisation uses — were rejected: the first leaves the term permanently unanswerable, the
  second ties a generic contract to one vendor. (FR-040b–FR-040e, US5 scenarios 8–10, Assumptions)
- Q: What happens to a service whose deploy platform no feeder covers? → A: It must still be
  investigable, since the project is generic. The connector asserts a SERVICE node for every
  watched log source and resolution merges it with any other feeder's node for the same service
  (FR-040f), and a change in the version stamped on its logs becomes a
  rollout, dated from first sight as a bound and merged by C8 with any deploy feeder's record of
  the same rollout (FR-040g). The earlier rule — a log source nobody else knows is an unattached
  claim — made exactly this organisation's Datadog service uninvestigable. (FR-040f, FR-040g,
  Assumptions, Data mapping)

## Personas

- **Sam, on-call SRE.** Is paged by a Datadog monitor at 14:32. Today Sam pivots by hand from the
  monitor to the service page, the deployment list, the log search and three dashboards. Sam
  wants the graph to already know what that monitor watches, what changed around it, and who owns
  it — and wants the links that open Datadog at the right query.
- **Casey, connector author.** Wrote and maintains this connector. Wants the vendor's shape to
  map onto the published event schema with no special cases in the graph, wants the recorded
  payloads to be the test, and wants to know, when Datadog changes an answer, which fixture goes
  red.
- **Priya, Datadog administrator.** Provisions the credential. Will not hand out a key that can
  mute a monitor or close an incident. Owns the organisation's API rate limits and its bill, and
  needs to see what this connector costs her before it runs, and how much of the budget it is
  using while it runs.
- **The investigation engine (machine consumer, feature 002).** Asks for a pointer to be executed
  over a window around an instant and consumes the answer as structure: bounded, summarised,
  evidence-carrying. It must be able to replay the same investigation from recorded responses
  and get the same answer.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - APM service-map topology (Priority: P3 — OPTIONAL capability, off by default)

> **Conditional (ADR-0004 D3).** This story applies **only** to an organisation that has
> distributed tracing in Datadog APM. The audited organisation has none, so the capability is
> **off by default** and this story contributes nothing to it. The mapping is kept, unchanged and
> specified, because it is correct for organisations that do have tracing and because deleting it
> would mean re-deriving it later. Nothing else in this feature depends on it.

An organisation with tracing has its services, their dependencies and their environments in
Datadog APM. With the `apm_topology` capability enabled, the connector reads the service list and
the service dependency map for the in-scope environments and turns them into SERVICE nodes and
weighted `calls` edges, each carrying the environment and the version Datadog reports, each with
a Datadog identity claim so the graph can work out that `datadog.service=prod/checkout` and the
OpenTelemetry `checkout` are one thing.

**Why this priority**: It was P1 while this connector was expected to be the source of the
organisation's topology. It is P3 because it is not: features 003 and 004 supply the nodes and
the changes, and a service map read from a vendor that observes no spans is empty rather than
small. An enabled-capability organisation gets exactly the behaviour specified here; a disabled
one must get the behaviour specified in FR-008b, which is silence with a stated reason.

**Independent Test**: With the capability enabled, run the connector against the recorded service
and dependency payloads from an organisation's Datadog, from an empty graph, and compare the
resulting graph to the golden output. With the capability disabled — the default — run the same
fixtures and assert that no APM scope is requested, no APM call is issued, no service node is
created from APM, and every other story's goldens are unchanged.

**Acceptance Scenarios**:

1. **Given** recorded Datadog APM payloads for one environment, **When** the connector runs from
   an empty graph, **Then** every service Datadog lists for that environment appears as a SERVICE
   node with its environment and, where Datadog reports one, its version; and every dependency
   Datadog reports appears as a `calls` edge carrying a traffic weight class.
2. **Given** the same payloads, **When** the events are inspected, **Then** each service carries
   an identity claim in the `datadog.service` namespace and every other identifier Datadog knows
   for it (its Kubernetes workload tags, its team tag) is a claim too, and the connector has
   merged nothing itself.
3. **Given** a dependency edge that Datadog reported in earlier windows and does not report for
   the declared number of consecutive windows, **When** the connector runs, **Then** the edge is
   retracted with a valid end inside the window in which it was last seen, not deleted.
4. **Given** a service whose Datadog-reported version changes between two polls, **When** the
   connector runs, **Then** a rollout change node is emitted with the instant Datadog first saw
   the new version as its valid time, and the service's version property changes at that instant.
5. **Given** a service that exists in the graph from the Kubernetes and OpenTelemetry feeders,
   **When** the Datadog claims arrive, **Then** the published certain rules merge them into one
   entity and `resolve why` names the rule, the score and the supporting claims.
6. **Given** the default configuration, in which the `apm_topology` capability is **off**, **When**
   the connector runs, **Then** it requests no APM scope, issues no APM call, creates no service
   node, no `calls` edge and no APM-derived rollout change, states in its checkpoint that the
   capability was disabled — so a reader can tell "no dependencies" from "we were not looking" —
   and every other story behaves exactly as it does with the capability enabled.

---

### User Story 2 - Monitors become alerts, and an alert can start an investigation (Priority: P1)

The organisation's monitors are its definition of "something is wrong". The connector turns each
in-scope monitor into an ALERT node linked to the entities it watches, and each monitor state
transition into an event whose valid time is the instant of the transition. Sam — or the
investigation engine — can name a monitor by its Datadog id and get the alert, what it watches,
its state history and the instant it last fired.

Intake is itself a feeder: every transition becomes an idempotent `alert.transition` event keyed
on monitor, group and transition instant. In v1 the transport is **polling every 15–30 seconds**
(Q3). An optional Datadog webhook is a **doorbell** and nothing more: it enqueues "poll now", the
connector re-reads state from the API, and the notification body is never read as data. Push and
pull may run at the same time — push for speed, pull for truth — without the transition being
recorded twice. Transitions are filtered by tag; flapping and no-data transitions do not start an
investigation; a recovery is a transition like any other and is kept as evidence.

**Why this priority**: This is one of the two halves the rescoping kept. The graph's ALERT node
type has had no feeder behind it, and a monitor that fired is evidence whether or not it was what
a human noticed first. The audit qualifies rather than demotes it: monitor transitions caught few
of the thirteen incidents, and the trigger that would have fired on **every** one of them is a
human creating a dated severity channel in Slack — which emits the same `alert.transition` event
through a Slack transport and is specified in 002 (ADR-0004 D4). The two intakes are the same
event type on purpose; this one supplies the monitors, their state history and their queries as
pointers, which the Slack path cannot.

**Independent Test**: Replay the recorded monitor definitions and state transitions of a real
incident window, ask for the alert by its Datadog monitor id, and compare the alert, its watched
entities and its state history to the golden output.

**Acceptance Scenarios**:

1. **Given** recorded monitor definitions, **When** the connector runs, **Then** each monitor is
   an ALERT node identified by its Datadog monitor id, carrying its name, type, severity where
   Datadog states one, its query in Datadog's own vocabulary as a pointer, and a link back to the
   monitor.
2. **Given** a monitor whose query names services, environments or Kubernetes objects the graph
   knows, **When** the connector runs, **Then** the alert is linked to those entities, and a
   monitor whose targets cannot be resolved produces an alert that is explicitly unattached
   rather than dropped.
3. **Given** a recorded sequence OK → ALERT at 14:32 → OK at 15:10, **When** the connector runs,
   **Then** the alert's state has valid intervals `[14:32, 15:10)` for the alerting state, with
   observed times reflecting the poll — scheduled, or triggered by a doorbell — at which the
   connector learned of each transition (Q3), a query as of 14:45 reports the monitor as
   alerting, and the recovery at 15:10 is emitted as its own transition and kept as evidence
   rather than discarded.
4. **Given** a monitor with a group-by that alerts on two groups at different instants, **When**
   the connector runs, **Then** each alerting group is its own alert with its own state history,
   and each names the monitor it belongs to.
5. **Given** an alert identified by its Datadog monitor id (and group where it has one), **When**
   the investigation engine asks for it, **Then** it receives the alert, its watched entities, the
   instant of the transition to use as the investigation's reference instant, and the pointers to
   execute — with no raw telemetry in the answer.
6. **Given** the same transition delivered both by a doorbell-triggered poll and by the next
   scheduled poll, **When** both are processed, **Then** exactly one `alert.transition` is in
   effect, because the idempotency key is (monitor, group, transition instant), and the alert's
   state history is unchanged by the second delivery.
7. **Given** an inbound notification that is forged, replayed or malformed, **When** the connector
   receives it, **Then** its body is never read as data, it costs at most one extra poll within
   the configured rate limit, and no alert, state version or event is created, altered or
   retracted as a result.
8. **Given** a monitor that flaps eight times in ten minutes and a monitor that goes to no-data,
   **When** the connector runs, **Then** every transition is recorded in the state history, and
   neither starts an investigation — the suppression is stated in the intake record rather than
   applied silently.

---

### User Story 3 - Deployments and changes the vendor already recorded (Priority: P3 — conditional)

> **Conditional (ADR-0004 D1, D2).** Changes now come from the platforms themselves: Cloud Run
> revisions and vendor notices in feature 003, GitHub Actions and Vercel deployments in feature
> 004. This story applies only where an organisation posts deployment, configuration or
> infrastructure events into Datadog's event stream, and it is governed by the `changes`
> capability, off by default. The mapping and the "one rollout, not two" behaviour are kept
> unchanged, because where such events exist they are a second observer of a rollout the platform
> feeders also saw, and collapsing the pair is exactly what the claims in FR-031 are for.

Datadog knows about deployments where the organisation tells it: APM deployment tracking reports
a new version of a service, and the events stream carries deployment, configuration and
infrastructure events that the organisation's pipelines post. With the `changes` capability
enabled, the connector turns them into CHANGE nodes with a kind, an actor and an actor kind where
they are known, a link back to the origin, and `changed-by` edges to their targets.

**Why this priority**: It was P1 when Datadog's change stream was expected to be the only place
many changes were recorded. It is P3 because features 003 and 004 read the platforms directly,
which is both more complete and closer to the truth: a deployment event is posted to Datadog by a
pipeline that may forget, while a Cloud Run revision or a Vercel promotion is the production
change itself. What survives at full strength is the **merge**: a Datadog-observed deployment
must resolve into one change node with the platform-observed rollout, never two (SC-014).

**Independent Test**: Replay the recorded events and deployment payloads of a window containing a
known culprit deployment among decoys, run the ranked diff from feature 001, and check that the
culprit ranks in the top three with a rationale citing its time and hop distance.

**Acceptance Scenarios**:

1. **Given** recorded Datadog events over a window, **When** the connector runs, **Then** each
   in-scope event becomes a CHANGE node with a kind from the published taxonomy, the instant
   Datadog recorded as its valid time, the actor where the event names one, a link back to the
   event, and `changed-by` edges to every target it names that the graph can resolve.
2. **Given** an event naming a target the graph does not know, **When** the connector runs,
   **Then** the change is kept and marked unattached, and it attaches automatically if the target
   later appears.
3. **Given** a deployment that the Kubernetes feeder also observed, **When** both sources have
   been ingested, **Then** the ranked change list for the affected service shows that rollout
   **once**, not twice — see the edge case on duplicate observation.
4. **Given** a Datadog event whose kind has no equivalent in the published change taxonomy,
   **When** the connector runs, **Then** it is emitted under the taxonomy's "other" kind with the
   vendor's own kind recorded, never silently dropped.

---

### User Story 4 - Every Datadog-derived node says where to look, in Datadog's words (Priority: P1)

Every node this connector creates, and every node in the graph whose logs live in Datadog, carries
pointers: a **log search query**, the **monitor** and its query, and the dashboards Datadog
associates with it. Where the `apm_topology` capability is enabled, the APM request, error and
latency metric queries and a trace/span search filter join them; where it is not, those pointers
are **not written**, because a pointer nobody can execute is a dead link and the graph should say
"no tracing" by omission rather than by lying. The selectors are written in Datadog's own query
vocabulary, because that is the query that actually runs; the attributes that say *which entity
the pointer is about* stay in OpenTelemetry semantic conventions, so a future backend can still
recognise the entity.

Note the direction this story now runs in: the nodes that need Datadog log pointers are mostly
**not** Datadog-derived. They are the Cloud Run services of feature 003 and the projects of
feature 004, for the one service whose logs Datadog holds. Attaching a pointer to a node another
feeder created is additive and is exactly what FR-039 already requires.

**Why this priority**: Pointers are the contract between this feature and the investigation
engine. Story 5 cannot execute a pointer that story 4 did not write, and Sam gets value from them
on day one by not hunting through tabs.

**Independent Test**: Ask for the pointers of a Datadog-derived service as of an instant and
compare to the golden output; check every selector against Datadog's documented query grammar and
open each link by hand once.

**Acceptance Scenarios**:

1. **Given** a service whose logs live in Datadog, **When** Sam asks for its pointers, **Then** the
   answer contains one log pointer, the monitor pointers of every monitor that watches it, and a
   dashboard pointer where Datadog associates one — each naming `datadog` as the backend and a
   versioned Datadog vocabulary, and each carrying OpenTelemetry attributes identifying the
   entity; and **where the `apm_topology` capability is enabled**, it additionally contains at
   least one metric pointer per published APM signal (request rate, error rate, latency), one
   trace pointer and a link back to the service in Datadog. With the capability disabled, zero
   trace pointers and zero APM metric pointers are written.
2. **Given** a service that was renamed in Datadog at 14:00, **When** Sam asks for its pointers as
   of 13:00, **Then** the selectors use the old service name; as of 14:32, the new one.
3. **Given** a node that also carries pointers from the Kubernetes or OpenTelemetry feeders,
   **When** Sam asks for its pointers, **Then** both sets are present — the Datadog pointers do
   not replace them.
4. **Given** any Datadog pointer, **When** its selector is inspected, **Then** it names no backend
   instance, no organisation, no URL host and no credential — only the query.

---

### User Story 5 - Log search as a telemetry backend: a bounded digest from the logs Datadog holds (Priority: P1)

The investigation engine has a hypothesis: "payments started throwing a new error after the 14:20
rollout". It hands the connector an algebra operation, a pointer and a window around an instant.
The connector executes the query against Datadog and returns a **digest**: a summary small enough
to reason over — log pattern counts with a capped number of redacted samples, errors grouped by
version, a before/after comparison — never a dump of the payload. The answer carries the exact
query that was run, Datadog's request identifiers, the coverage block and the instants involved,
so the conclusion can be audited later.

For this organisation the algebra resolves as follows, and this is the heart of the rescoping:

- **`new_log_patterns`** and **`errors_by_version`** are answered **from logs alone**, for the
  services whose logs live in Datadog. No span, no APM metric and no service map is needed for
  either, which is why they survive a scope in which there is no tracing.
- **`compare`** is answered over **log-derived metrics and monitor queries** — a count of matching
  log lines over two windows, a monitor's evaluated query — where no APM metric exists.
- **`error_spans`** returns a typed **`no_data`** whose coverage states **"no tracing"**: no span
  data source is configured for this organisation. It is not an error, not an `unknown`, and not
  a silent empty answer. Where spans do exist — the `apm_topology` capability enabled, or a future
  span source — the same operation is answered normally.

**Why this priority**: This is the half of the feature the rescoping kept whole, and the half
feature 002 cannot fake. Feature 003 supplies the project's first live telemetry backend over
Cloud Logging and Cloud Monitoring; this one is the second, which is what turns the backend
contract from an interface with one implementation into an interface that has been proved
interchangeable. It is also where constitution IV is either honoured or quietly broken.

**Independent Test**: Against recorded Datadog responses, execute each supported pointer kind
over a window and compare the digest to the golden digest; then run the same request live against
the organisation and check the digests match for a window whose data has settled.

**Acceptance Scenarios**:

1. **Given** a metric pointer and a request for the 30 minutes before and after 14:20, **When**
   the engine executes it, **Then** it receives, per series, a bounded summary of each window and
   an explicit before/after comparison, plus the query as sent, the request identifiers and the
   instant of execution — and no individual sample values beyond the published summary shape.
2. **Given** a log pointer over a window containing 400,000 matching lines, **When** the engine
   executes it, **Then** it receives counts grouped by pattern, at most the published number of
   patterns and at most the published number of redacted sample lines per pattern, and an explicit
   statement of what was truncated and by which criterion.
3. **Given** a window in which the query is valid and matches nothing, **When** the engine
   executes it, **Then** the answer is an explicit "no data in this window", distinct from
   "unknown" and distinct from an error.
4. **Given** Datadog rejects the query or rate-limits the request, **When** the engine executes
   it, **Then** the answer names the failure kind, echoes the query, states when the request may
   be retried where Datadog says so, and never fabricates a partial result.
5. **Given** the same request executed against recorded responses and live, **When** both digests
   are compared, **Then** they are identical, and nothing in the digest depends on the connection
   rather than on the response or on configuration.
6. **Given** any digest produced in the whole test corpus, **When** the graph's own validation is
   run over it, **Then** no part of it has been written into the graph as a node, an edge, a
   property or an event.
7. **Given** an organisation with no span data source — the default — **When** the engine asks
   `error_spans` over any edge and window, **Then** the answer is a typed `no_data` whose coverage
   block states that no tracing is configured and names what was searched, distinct from a
   failure, distinct from `unknown`, distinct from `not_yet_ingested`, and identical in recorded
   and live mode.
8. **Given** a service whose logs live in Datadog and a window around a rollout, **When** the
   engine asks `new_log_patterns` and `errors_by_version`, **Then** both are answered from log
   data alone, with no APM metric, span or service-map call issued, and the coverage block states
   the log index searched, the volume considered and the indexing lag.
9. **Given** a service whose logs carry its full commit sha in the attribute its pointer names,
   deployed by any platform a deploy feeder covers, **When** the engine asks `errors_by_version`
   over a window spanning a rollout, **Then** each group names the `deploy.commit_sha` ref of its
   version, and the group for the new version resolves to the same CHANGE node the deploy feeder
   emitted — identically whether that feeder is Cloud Run, GitHub Actions, Vercel or Kubernetes.
10. **Given** a service whose logs carry no attribute in the published convention list, or only
   an SDK version constant across services and days, **When** the engine asks `errors_by_version`,
   **Then** the answer is `no_data` whose coverage names each convention searched and why each was
   rejected, and the log pointer records the same outcome from discovery.

---

### User Story 6 - Tags become owners and identity claims (Priority: P2)

Datadog's tags carry the organisation's idea of ownership: `team`, `owner`, `service`, `env`, and
whatever else the service catalog holds. The connector turns team and owner tags into OWNER nodes
with `owned-by` edges, and turns every tag that names an identity in another system into an
identity claim.

**Why this priority**: Ownership is what Sam needs after "what changed?" — who to call. It is
lower than P1 because the graph is useful without it, and because tag hygiene is the thing most
likely to vary between organisations.

**Independent Test**: Replay recorded tag and service catalog payloads and compare the owner
nodes, ownership edges and claims to the golden output.

**Acceptance Scenarios**:

1. **Given** services carrying a team tag, **When** the connector runs, **Then** each distinct
   team is one OWNER node and each service has an `owned-by` edge to its team, with an unknown
   valid start where Datadog does not say since when.
2. **Given** a service whose tags name a Kubernetes namespace and workload, **When** the connector
   runs, **Then** those are identity claims with their supporting attributes, and the connector
   merges nothing itself.
3. **Given** two tag keys that name the same team differently, **When** both are observed, **Then**
   both claims are kept and the resolution layer decides, surfacing a suggestion rather than
   picking.
4. **Given** a tag key that is not on the configured allowlist, **When** the connector runs,
   **Then** it does not become a property, a node or a claim (Q2).

---

### User Story 7 - The organisation's own payloads, sanitised, are the tests (Priority: P2)

Casey runs a recording campaign against the organisation's Datadog: a baseline window, an
incident window with a real monitor transition and a real deployment, and a window with a
rate-limit response in it. Every recording is sanitised to a written contract, scanned, signed
off and committed as the connector's fixture. Anything that cannot be sanitised is not committed.

Two things shape the campaign (Q1, Q2). The windows are chosen by **incident density, not
business importance**, supplemented by fault-injection game days in staging, which are the only
labelled ground truth the project controls, and by deliberately boring windows and
false-positive alerts whose correct answer is "nothing implicated". And in v1 the sanitised real
recordings live in a **private corpus with private CI**: the public repository carries
**synthetic structural twins** — the same shapes, the same event sequences, the same digest
contract, no real identifier anywhere — so the public conformance suite stays meaningful without
a real payload ever leaving the organisation.

**Why this priority**: Constitution VIII says a connector with synthetic-only test data cannot be
marked stable, so this is what makes the connector real rather than a demo — but the connector can
be written and reviewed before the campaign runs.

**Independent Test**: The committed fixtures replay from empty to their goldens, are delivered
twice with no change, are shuffled inside the declared reordering window with no change to
valid-time state, and pass the secret and personal-data scan. Separately, the public repository's
synthetic twins replay to their own goldens with the private corpus absent.

**Acceptance Scenarios**:

1. **Given** a recorded campaign, **When** the fixtures are verified, **Then** each replays from
   empty to its golden graph, double delivery changes nothing, and shuffling inside the declared
   window changes no valid-time state.
2. **Given** a recording that contains host names, user emails, organisation identifiers or
   customer data, **When** it is sanitised, **Then** every people identifier is **dropped, not
   hashed**; every infrastructure identifier is replaced by a **keyed HMAC pseudonym** that is the
   same everywhere it occurs in the corpus, so joins survive; and the automated scan passes.
3. **Given** a monitor whose notification message contains a webhook target or a credential,
   **When** the recording is sanitised, **Then** the message body is removed entirely rather than
   redacted in place, and the monitor's name, query, thresholds and tags are kept.
4. **Given** a sanitised recording, **When** it is committed, **Then** it carries a **signed
   manifest** naming the sanitisation policy version, the content hash, the individual who signed
   it off and when, and what was dropped; and where a second person exists, a second signature
   (Q2).
5. **Given** the recorded telemetry responses, **When** feature 002 replays an investigation,
   **Then** it gets the same digests without any call to Datadog.
6. **Given** canary tokens seeded into the source data before a campaign runs, **When** the
   recording is committed, **Then** an independent secrets-and-entropy scan — run by something
   other than the sanitiser — finds none of them, and a surviving canary fails the commit rather
   than raising a warning.
7. **Given** a raw Datadog response, **When** it is captured, **Then** it is sanitised inside the
   connector before any part of it reaches disk, and the same boundary is what decides what may be
   sent to a model provider.
8. **Given** the public repository alone, **When** the conformance suite runs, **Then** it passes
   against the synthetic structural twins, and no real production recording is present in it.

---

### User Story 8 - The connector stays inside Priya's budget (Priority: P2)

Priya needs to know what this costs before it runs and while it runs. The connector declares a
call budget per interval, plans its polling to stay inside it, backs off when Datadog says to,
degrades in a stated order when the budget is short, and reports what it has used.

The budget is not a flat call count. It is expressed as a **share of the remaining vendor quota
read from Datadog's own response headers**, per endpoint class — the span search and aggregate
class is the tight one, on the order of 300 requests per hour — because that quota is **shared
with the humans working the incident**, and the agent must never be the reason an SRE's search
is throttled. Query window width is capped per pointer kind as well, so that one wide question
cannot spend an hour of quota on its own.

**Why this priority**: A connector that trips the organisation's rate limits is a connector that
gets its credential revoked, and the damage lands on the humans sharing that limit, not on us.
It is P2 because the P1 stories are testable at fixture scale before budgeting exists.

**Independent Test**: Replay a recorded run containing rate-limit responses and check that the
connector backs off as the response instructs, that its usage report matches the number of calls
the recording contains, and that a run configured under a small budget drops work in the declared
order rather than at random.

**Acceptance Scenarios**:

1. **Given** a configured budget expressed as a share of the remaining quota, **When** the
   connector plans a cycle, **Then** it reads the remaining quota from Datadog's response headers,
   stays within its share per endpoint class, and defers the work it cannot do in the **published**
   priority order with the deferral reported.
2. **Given** Datadog answers with a rate-limit response, **When** the connector receives it,
   **Then** it waits at least as long as the response asks, does not lose the position it had
   reached, and records the event in its own operational telemetry.
3. **Given** a poll that fails part-way through, **When** the cycle ends, **Then** the checkpoint
   states the extent actually covered and declares the gap, so a later query can see that the
   answer for that window is incomplete.
4. **Given** any run, **When** Priya asks what it used, **Then** she sees calls made per Datadog
   area, the proportion of the configured budget consumed, the share of the **remaining vendor
   quota per endpoint class** it took, and the number of rate-limit responses received.
5. **Given** an incident during which humans are consuming the span search and aggregate quota,
   **When** the connector plans work in that class, **Then** the remaining quota it reads from the
   response headers has fallen, its own allowance falls with it, and it yields rather than
   competing — leaving at least the published reserve for humans.
6. **Given** a request whose window is wider than the published cap for its pointer kind, **When**
   it is planned, **Then** it is narrowed to the cap or refused with the cap named, never issued
   as asked.

---

### Note where User Story 9 was — Datadog Incident Management

**Removed by ADR-0004.** The earlier draft carried a P3 story reading declared incidents as
alert-shaped context, and a commitment to pull incident records as **labels** for the evaluation
corpus even while the graph representation was deferred. The coverage audit found the product
**unused** in the audited organisation: incidents are declared by creating a dated severity
channel in Slack, and that channel is the trigger that would have fired on every incident in the
corpus (ADR-0004 D4). Both halves therefore leave this specification:

- The **graph representation** is gone. The connector creates no incident node, requests no
  incident scope, and reads nothing from that area. There is no configuration that enables it.
- The **evaluation labels** move to spec 002, which owns the Slack intake and therefore owns the
  window, the implicated services and the declaration and resolution instants that make a recorded
  window gradeable. The label contract never depended on Datadog; it depended on the organisation
  having somewhere it declares incidents.

Nothing else in this feature depended on the story, which is why removing it costs no
requirement elsewhere. If an organisation that does use Datadog Incident Management ever needs
it, it is a later increment on the published connector contract, not a variant of this one.

---

### Edge Cases

- **More than one Datadog organisation, or more than one environment.** A service name is unique
  only inside one organisation and one environment. Identity must carry both, so that
  `prod/checkout` in one organisation is never silently merged with `prod/checkout` in another,
  and `staging/checkout` is never merged with `prod/checkout`. Each organisation is a separate
  source with its own identifier, budget and checkpoints.
- **A service renamed in Datadog.** Datadog does not rename a service; a new name appears and the
  old one goes quiet. The connector must not report this as a deletion and a creation of unrelated
  things: the old service is retracted only under the published silence rule, the new one is
  created, and any evidence linking them (same Kubernetes workload tags, same team, an overlapping
  version) becomes claims so the resolution layer can decide in the open. A rename must never be
  guessed from name similarity alone.
- **A flapping monitor.** A monitor that transitions eight times in ten minutes must produce eight
  state intervals, not one smeared interval and not eight alerts. Because the connector polls in
  v1 (Q3), the checkpoint must say so and the state history must be marked as sampled rather than
  complete for that window. A flapping monitor must not start eight investigations: flapping is
  recorded as state and suppressed as a trigger, and the suppression is stated.
- **A monitor with a group-by (multi-alert).** One monitor, many groups, each with its own state.
  Each alerting group is one alert, named by monitor and group, and the monitor itself is not
  reported as alerting because one group is.
- **Pagination or a rate limit in the middle of a poll.** A poll that has read half of a paged
  answer knows half a truth. The connector must not emit retractions derived from a partial read,
  must checkpoint only the extent it completed, and must declare the gap.
- **A credential that turns out to be write-capable.** The connector refuses to start and names
  what the credential can do, in the same shape the Kubernetes feeder's refusal takes. If Datadog
  cannot report a given scope, the connector says which part it could not verify and the
  operator's assertion is recorded in configuration rather than assumed.
- **A recording that would leak customer data.** If sanitisation cannot make a payload safe — a
  log sample that is a customer record, a monitor message naming an individual — the payload is
  dropped from the recording and the fixture documents the drop. A fixture is never committed on
  the promise of being cleaned later.
- **Clock skew between Datadog and the graph.** Valid time comes from Datadog's timestamps and
  observed time from the graph. The connector never corrects one with the other; where the two
  disagree by more than a stated threshold the difference is reported in the connector's own
  operational telemetry so the skew is visible.
- **The same deployment seen by the Kubernetes feeder and by Datadog.** Both sources are telling
  the truth. The connector emits its own change with identity claims naming everything it knows
  about the deployment — target service, version, revision or image identifier, instant — so that
  the published certain rules can merge it with the Kubernetes feeder's change into one change
  node. Where no shared identifier exists, the pair must be raised as a resolution suggestion and
  must never be merged by the connector itself. The desired behaviour, and the fixture that pins
  it, is that the ranked change list shows the rollout **once**; two adjacent entries for one
  rollout is the failure this case guards against.
- **A monitor that watches something the graph has never seen.** Kept as an unattached alert, with
  the unresolvable target recorded, and attached automatically if the target later appears.
- **A service in the dependency map that is not in the service list.** Treated as a third-party
  dependency rather than a service, and flagged so that a later list can promote it.
- **Datadog changes an answer's shape.** A payload that no longer parses fails loudly against its
  fixture rather than silently producing fewer events, and the connector reports the rejection
  with the field that broke.
- **A forged, replayed or malformed doorbell notification.** The inbound endpoint is a doorbell,
  so the worst an attacker who guesses the URL can achieve is one extra poll of Datadog inside the
  configured rate limit. The body is never parsed as data, no state is written from it, and a
  notification without the shared-secret header is dropped and counted.
- **A window that is inside the backend's indexing lag.** A query over the last two minutes may
  legitimately return nothing yet. This is `not_yet_ingested`, distinct from `no_data` and from a
  failure; the digest states the lag it observed, and the investigation engine may retry rather
  than concluding.
- **A recorded-mode request the world recording does not cover.** Answered `not_recorded` — never
  `no_data`, never a silent fall-through to a live call. The share of in-algebra requests that
  land here is the fixture's miss rate and is reported.
- **A request outside the published query algebra.** Refused, naming the operation asked for and
  the operations available. The backend is not a query proxy and free-form query text supplied by
  a caller is never executed.
- **An autoscaler-originated change that looks like a deployment.** A replica count moved by the
  autoscaler at 14:33 is very likely an *effect* of the incident, not its cause. It must be typed
  as `CONTROLLER` so the ranker's causal ordering can treat it differently from a `PERSON`-driven
  rollout, and the actor kind must never be guessed from the actor string alone.
- **Vendor quota already spent by humans.** During an incident the SREs are searching too. The
  connector reads what is left from the response headers, takes only its share, keeps the
  published reserve free, and says in its output that it stopped for quota rather than for lack
  of evidence.
- **A canary token that survives sanitisation.** The commit fails. A canary that reaches a
  committed artifact means the sanitiser has a hole, and the correct response is to stop
  committing, not to remove that one token.
- **A capability that is turned off.** The default configuration has `apm_topology` and `changes`
  off. "No dependencies in the graph" must never be readable as "Datadog reported no
  dependencies": the checkpoint names the capabilities in force, the connector requests no scope
  and issues no call for a disabled one, and every criterion that depends on it is reported as
  *not applicable* rather than passed (FR-008a, FR-008b, SC-001, SC-024). Half-enabling —
  requesting the scope but not reading, or reading but not emitting — is the failure this guards
  against.
- **An algebra operation whose data source does not exist.** `error_spans` in an organisation with
  no tracing is the case that will actually occur, on every investigation, for the lifetime of
  this deployment. It must answer a typed `no_data` naming the missing source, identically in
  recorded and live mode, stably for the whole window, so an investigation concludes "this cannot
  be checked here" once rather than retrying (FR-049b, SC-023). An approximation assembled from
  log lines would be worse than the empty answer, because it would be cited as if it were span
  evidence.
- **A service whose logs are not in Datadog.** Most of them, here. The connector attaches no log
  pointer, claims nothing about it, and a log query aimed at it answers `no_data` with coverage
  naming the index that was searched — not `query_failed`, and not a silent empty digest.

## Requirements *(mandatory)*

### Functional Requirements

#### A. One connector, two halves, one read-only credential

- **FR-001**: The connector MUST consist of exactly two halves over one shared client and one
  shared credential: a **feeder** that emits typed graph events, and a **telemetry backend** that
  executes pointer queries and returns digests. The telemetry backend MUST NOT emit any graph
  event and MUST NOT write anything into the graph.
- **FR-002**: The connector MUST use read-only Datadog credentials. It MUST declare the exact
  read scopes it requires **per enabled capability** (FR-008a): logs, monitors and tags always;
  APM, service catalog, spans and metrics only where `apm_topology` is enabled; events only where
  `changes` is enabled. It MUST NOT request a scope for a disabled capability, and MUST NOT
  request any incident scope at all.
- **FR-003**: At startup the connector MUST ask Datadog what its credential is permitted to do
  and MUST refuse to start if the credential grants write access in any area it reads, naming the
  offending permissions. Where Datadog cannot report a permission, the connector MUST name the
  part it could not verify and MUST require the operator to assert read-only in configuration;
  it MUST NOT assume.
- **FR-004**: The connector MUST NOT call any Datadog operation that changes state in Datadog —
  including muting or resolving monitors, creating or updating incidents, posting events,
  scheduling downtime, or editing dashboards, tags or the service catalog — and this MUST be
  verifiable by inspecting the set of operations the connector is able to issue, not only by
  inspecting its behaviour on one run.
- **FR-005**: The connector MUST be usable in two modes with identical output: live against
  Datadog, and offline against recorded payloads and recorded responses. The recorded mode is the
  test. Nothing in any event or any digest may derive from the connection itself — only from a
  payload or from configuration.
- **FR-006**: Each Datadog organisation the connector reads MUST be a distinct source with its
  own source identifier, credential, budget and checkpoints. One connector instance MUST NOT mix
  two organisations into one source.
- **FR-007**: The set of environments and services the connector reads MUST be configurable, and
  the connector MUST record in its checkpoints which scope it was configured with, so that a query
  can tell "not present" from "not in scope". The scope MUST be changeable without losing history,
  because recording starts before the campaign scope is finally agreed (Q1, FR-070d).
- **FR-008**: The connector MUST emit its own operational telemetry: calls made per Datadog area,
  rate-limit responses, poll duration and lag, events emitted and rejected, digests produced,
  digest truncations, and observed clock skew between Datadog timestamps and graph observed time.
- **FR-008a**: The connector MUST be organised as a set of named **capabilities**, each
  independently enabled or disabled in configuration, each declaring the Datadog areas and read
  scopes it needs. At minimum: **`logs`** — the log-search telemetry backend (section F), on by
  default; **`monitors`** — monitor and alert intake (section C), on by default; **`tags`** —
  ownership and identity claims (section H), on by default; **`apm_topology`** — the service list,
  service map, APM metric and span pointers and APM deployment tracking (section B), **OPTIONAL
  and off by default**; **`changes`** — the Datadog events stream as a change source (section D),
  **OPTIONAL and off by default**. The set of capabilities in force MUST be recorded in every
  checkpoint and in every fixture manifest.
- **FR-008b**: A disabled capability MUST be **silent and stated**, never partially active. For a
  disabled capability the connector MUST request no scope for its areas, MUST issue no call to
  them, MUST emit no node, edge, change, claim or pointer derived from them, and MUST state in its
  checkpoint that the capability was disabled — so that a query can tell "Datadog reported no
  dependencies" from "we were not looking at dependencies". Every requirement outside the disabled
  capability's section MUST hold unchanged, and every other capability's goldens MUST be
  byte-identical with it on and off.

#### B. Topology from Datadog APM *(capability `apm_topology`, OPTIONAL, off by default)*

> Every requirement in this section is conditional on the `apm_topology` capability being enabled
> (FR-008a). With it disabled — the default, and the audited organisation's configuration — FR-008b
> governs instead: no APM scope, no APM call, no service node, no `calls` edge, no APM-derived
> rollout, no trace or APM metric pointer, and a checkpoint that says so.

- **FR-009**: The feeder MUST create a SERVICE node for every service Datadog reports in an
  in-scope environment, carrying at least the service name, the environment, and the version
  Datadog reports where it reports one.
- **FR-010**: The feeder MUST create a `calls` edge for every service-to-service dependency
  Datadog reports for an in-scope environment, carrying a traffic weight class drawn from the
  published ordinal scale. It MUST NOT carry a request rate, a count or any other measurement.
- **FR-011**: A dependency edge's valid interval MUST be the Datadog query window the dependency
  was observed in. An edge not observed for a configured number of consecutive windows MUST be
  retracted with a valid end inside the last window it was observed in.
- **FR-012**: The feeder MUST NOT retract anything on the basis of a partial read. A poll that
  did not complete its pagination MUST emit no retraction for the unread part and MUST declare
  the gap in its checkpoint.
- **FR-013**: The feeder MUST emit a rollout change node when the version Datadog reports for a
  service changes, with the instant Datadog first observed the new version as the change's valid
  time, the old and new versions recorded, and a `changed-by` edge to the service.
- **FR-014**: The feeder MUST create nodes for the hosts and infrastructure Datadog associates
  with an in-scope service where it reports them, as infrastructure resources with `runs-on`
  edges, and MUST NOT create one node per container instance or per ephemeral pod replica.
- **FR-015**: A dependency target that Datadog reports but does not list as a service MUST be
  represented as a third-party dependency rather than as a service, and MUST be marked so that a
  later service listing can promote it.
- **FR-016**: The feeder MUST emit a source checkpoint on start, on resync and after any gap,
  stating the extent it actually covered and whether it was watching immediately before that
  extent.
- **FR-017**: Where Datadog does not state since when a fact has been true, the feeder MUST mark
  the valid start as unknown. It MUST NOT substitute the poll instant.

#### C. Monitors, alerts and state transitions

- **FR-018**: The feeder MUST create an ALERT node for every in-scope monitor, identified by its
  Datadog monitor identifier, carrying at least the monitor name, the monitor type, the severity
  or priority Datadog states, the notification targets' team names where they are on the
  allowlist, and the monitor's query as a pointer in Datadog's own vocabulary.
- **FR-019**: The feeder MUST link an alert to the entities the monitor watches, derived from the
  monitor's query and tags, using the graph's published relationship taxonomy. An alert whose
  targets cannot be resolved MUST be kept and marked unattached, and MUST attach automatically if
  a target later appears.
- **FR-020**: A monitor state transition MUST be an event whose **valid time is the instant of the
  transition as Datadog reports it**, never the instant the connector learned of it. The observed
  time is assigned by the graph on acceptance.
- **FR-021**: The alert's state MUST be versioned in valid time, so that a query as of any instant
  reports the state the monitor was in at that instant, including no-data and warning states where
  the monitor has them.
- **FR-022**: A monitor with a group-by MUST produce one alert per alerting group, each identified
  by the monitor and the group, each with its own state history; the monitor-level entity MUST NOT
  be reported as alerting because one of its groups is.
- **FR-023**: The connector MUST be able to hand an alert to the investigation engine by Datadog
  monitor identifier (and group where the monitor has one), returning the alert, the entities it
  watches, the transition instant to use as the investigation's reference instant, and the
  pointers to execute. The answer MUST contain no telemetry payload.
- **FR-024**: **Polling is the v1 intake transport** (Q3), at a configurable interval whose
  default MUST lie between 15 and 30 seconds — small next to monitor evaluation lag. Where the
  connector learns of transitions by polling it MUST state in the alert's state history that the
  window is sampled at the poll interval, so that a consumer can tell a complete history from a
  sampled one.
- **FR-025**: Alert intake MUST be modelled as **one more feeder**, emitting an `alert.transition`
  event for each monitor state transition. The event's idempotency key MUST be the published
  4-tuple **(source, stable alert identifier, group, transition instant)** (ADR-0005 D2, 002
  FR-008b) — here: source `datadog`, the monitor identifier as the stable alert identifier, the
  monitor's group, and the transition instant as Datadog reports it — so that the same transition
  learned more than once, or by more than one transport, is one event (Q3).
- **FR-025a**: Webhook and polling MUST be **two transports for the same event** and MUST be
  usable together — push for speed, pull for truth. Running both MUST NOT produce two transitions,
  two alerts or two state versions, and the result MUST NOT depend on the order in which the two
  transports deliver.
- **FR-025b**: An inbound Datadog notification MUST act only as a **doorbell**: it MUST enqueue
  "poll now" and MUST NOT be a source of data. The connector MUST re-read state from the Datadog
  API and MUST NOT parse, trust or store the notification body. The endpoint MUST require a
  shared-secret header and MUST be rate limited, so that a forged, replayed or malformed
  notification costs at most one extra poll and can never create, alter or retract an alert, a
  state version or an event.
- **FR-025c**: Intake MUST be filterable by tag. **Flapping and no-data transitions MUST NOT
  trigger an investigation**, MUST still be recorded in the alert's state history, and the
  suppression MUST be stated rather than applied silently. A **recovery transition MUST be emitted
  and is evidence**; it MUST NOT be discarded as uninteresting.
- **FR-026**: The connector MUST NOT change any monitor's state, mute, downtime or notification
  configuration (restating FR-004 for the area where the temptation is greatest).

#### D. Changes *(capability `changes`, OPTIONAL, off by default)*

> Conditional on the `changes` capability (FR-008a). Changes come from the platforms themselves —
> Cloud Run revisions and vendor notices in feature 003, GitHub Actions and Vercel deployments in
> feature 004 — so this section applies only where an organisation also posts deployment,
> configuration or infrastructure events into Datadog. The claims of FR-031 and the "one rollout,
> not two" behaviour of SC-014 are the reason the mapping is kept rather than deleted: where such
> events exist they are a **second observer** of a rollout the platform feeders already saw, and
> collapsing that pair is the whole point. FR-033a and FR-033b (actor kind) apply to every change
> this connector creates, including the rollout changes of FR-013 under `apm_topology`.

- **FR-027**: The feeder MUST create a CHANGE node for each in-scope Datadog event that describes
  a change to the production system, with a kind from the published change taxonomy, the instant
  Datadog recorded as valid time, a human-readable summary, the actor where the event names one,
  a reference back to the event in Datadog, and `changed-by` edges to the targets it names.
- **FR-028**: A Datadog event kind with no equivalent in the published taxonomy MUST be emitted
  under the taxonomy's "other" kind with the vendor's own kind recorded as a property. It MUST NOT
  be dropped.
- **FR-029**: Which Datadog event sources and tags are treated as changes MUST be configurable,
  and the configuration in force MUST be recorded in the checkpoint, so that a later reader can
  tell "no change happened" from "we were not looking for that kind of change".
- **FR-030**: A change whose target cannot be resolved MUST be kept and marked unattached.
- **FR-031**: The feeder MUST emit, for every change it creates, every identifier it knows that
  another source could also know — the target service, the version, the revision or image
  identifier, the pipeline or commit reference where the event carries one — as identity claims,
  so that the same real-world change observed by two sources can be resolved into one change node
  by the published rules rather than by this connector.
- **FR-032**: The connector MUST NOT merge its change with another source's change itself, and
  MUST NOT suppress its own observation because another source may have reported the same thing.
- **FR-033**: Where two sources report the same change and no published rule can merge them, the
  pair MUST appear as a resolution suggestion with its score and rationale.
- **FR-033a**: Every CHANGE node the feeder creates — including the rollout changes of FR-013 —
  MUST carry an **actor kind** drawn from feature 001's published set (ADR-0005 D1), whose minimum
  membership is `PERSON` ("human-originated"), `AUTOMATION` (a CI, bot or deploy principal acting
  on a person's behalf), `CONTROLLER` (an autoscaler, scheduler or platform automation reacting to
  state), `VENDOR` ("vendor-originated") and `UNKNOWN`. This replaces the earlier
  `human/system/vendor/unknown` wording: what was `system` maps to `CONTROLLER` or `AUTOMATION`
  according to what Datadog states about the trigger, and to `UNKNOWN` where neither is derivable.
  The actor kind is a separate field from the actor's name and MUST NOT be inferred from that name
  alone.
- **FR-033b**: The connector MUST derive the actor kind from what Datadog states — the event
  source, the event tags, and APM deployment tracking's origin — wherever it is derivable, and
  MUST record `UNKNOWN` together with the evidence that was available where it is not. The
  investigation engine's causal ordering depends on this distinction: a scaling change made by an
  autoscaler after symptom onset is a candidate **effect**, not a candidate cause.

#### E. Pointers in Datadog's vocabulary

- **FR-034**: Every node the feeder creates MUST carry pointers. For a service these MUST include
  at least: metric pointers for the APM request rate, error rate and latency signals; a log search
  pointer; a trace or span search pointer; a link back to the service in Datadog; and a dashboard
  pointer where Datadog associates one. For an alert these MUST include the monitor's query and a
  link to the monitor.
- **FR-035**: Pointer selectors MUST be written in Datadog's own query vocabulary, because the
  selector is the query that will actually be executed. The vocabulary MUST be named and versioned
  as a published vocabulary, and the published documentation MUST state why these selectors cannot
  be expressed in OpenTelemetry semantic conventions (constitution IV).
- **FR-036**: The attributes that identify *which entity* a pointer is about MUST be expressed in
  OpenTelemetry semantic conventions, so that a reader or a future backend can recognise the
  entity without understanding Datadog's grammar.
- **FR-037**: A pointer MUST name the backend family only. It MUST NOT contain an organisation
  identifier, a site or region host, an account, a credential or any URL that embeds one, except
  where the pointer is itself a link back to Datadog, in which case the link MUST carry no
  credential.
- **FR-038**: Pointers MUST be versioned in valid time with the node they belong to, so that
  asking for a service's pointers as of an earlier instant returns the selectors that were true
  then.
- **FR-039**: A Datadog pointer MUST be additive: it MUST NOT replace or suppress pointers that
  other sources have attached to the same entity.

### Data mapping *(what Datadog objects become)*

Observed time is assigned by the graph at acceptance in every row; the column below records what
the connector controls — the cadence at which it learns things and what it says when it did not
look. "Unknown start" means the connector MUST mark the valid start unknown rather than guess.

Rows marked *(apm_topology)* and *(changes)* belong to capabilities that are **off by default**
(FR-008a): with the capability disabled the row produces nothing at all, and the checkpoint says
so (FR-008b). Every unmarked row is always in force.

| Datadog object | Graph element | Identity and refs | Valid time | Learning cadence and gaps |
|---|---|---|---|---|
| *(apm_topology)* Service in an in-scope environment (service list / service catalog entry) | SERVICE node with environment and version properties, plus pointers | ref in the `datadog.service` namespace, value `<env>/<name>`; claims for every other identifier Datadog knows (OpenTelemetry service name, Kubernetes namespace and workload, team) | unknown start unless Datadog states a first-seen instant; end only under the silence rule | catalog poll; a failed poll checkpoints a gap and retracts nothing |
| *(apm_topology)* Service dependency in the service map | `calls` edge with a traffic weight class | source and target refs in `datadog.service` | the Datadog query window the dependency was observed in | dependency poll per window; absence for N consecutive windows retracts with a valid end inside the last window observed |
| *(apm_topology)* Version change reported by APM deployment tracking | CHANGE node, kind rollout, with an actor kind where derivable and a `changed-by` edge to the service | ref in the `datadog.change` namespace keyed by service, version and first-seen instant; claims for version, revision or image identifier | the instant Datadog first observed the new version | deployment poll; a version seen only after a gap is emitted with the instant Datadog states, and the gap stands in the checkpoint |
| Monitor definition | ALERT node with its query as a pointer and a link to the monitor | ref in the `datadog.monitor` namespace, value the monitor identifier | unknown start (Datadog states when a monitor was created, not when it became meaningful); retracted when the monitor is deleted or leaves scope | monitor poll |
| Monitor state transition | `alert.transition` event, projected as a new version of the alert's state | same ref; a grouped monitor uses monitor identifier and group; idempotency key (monitor, group, transition instant) | **the transition instant Datadog reports** | poll every 15–30 s in v1, optionally woken by a doorbell webhook (Q3); a polled history is marked sampled |
| Monitor group (multi-alert) | one ALERT per alerting group | ref carries monitor identifier and group | the group's own transition instants | as above |
| *(changes)* Datadog event describing a change | CHANGE node with kind, actor, **actor kind** (human / system / vendor / unknown), origin link, `changed-by` edges | ref in the `datadog.change` namespace, value the event identifier; claims for every shared identifier (FR-031) | the instant the event carries | events poll over a moving window; a partial read checkpoints a gap |
| Team or owner tag | OWNER node and `owned-by` edge | ref in the `owner.team` namespace | unknown start | tag or catalog poll |
| Service, environment and version tags | node properties and identity claims | claims in the namespace the tag names | as the node version they belong to | with the node |
| *(apm_topology)* Host associated with an in-scope service | INFRA_RESOURCE node and `runs-on` edge | ref in the `datadog.host` namespace; claims for the Kubernetes node or cloud instance identifier where the host's tags state one | unknown start; retracted under the silence rule | host poll |
| A service whose logs live in Datadog, however its node got into the graph (features 003, 004, 001) | **a log pointer and the monitor pointers added to the existing node**, plus a SERVICE node this connector asserts for every watched log source, merged by a certain rule with another feeder's node for the same service where there is one (FR-040f) | claims in the Datadog log-source namespace, with the log service, source and environment attributes the index is keyed on | with the node version | log-configuration poll; additive, never replacing another source's pointers (FR-039) |
| *(apm_topology)* Dependency target absent from the service list | THIRD_PARTY node | ref in `datadog.service` marked as unlisted | the window it was observed in | with the dependency poll |
| A change in the version a watched service stamps on its logs (FR-040c) | CHANGE node, kind rollout, actor `UNKNOWN` unless stated, `changed-by` edge to the service | ref in the `datadog.change` namespace keyed by service, version and first-seen instant; the `deploy.*` claim of the version (FR-040d), so C8 merges it with a deploy feeder's rollout | the first instant a line with the new version was indexed, **marked as a bound** | discovery and error polls; nothing is inferred from a version with no `deploy.*` form (FR-040g) |
| Metric, log, trace and dashboard queries | **pointers on the node**, never nodes | — | with the node version | with the node |
| Incident (Datadog Incident Management) | **nothing — removed by ADR-0004.** No node, no evaluation label, no namespace declared, no scope requested, nothing read | — | — | never polled |
| Metric samples, log lines, spans, and any aggregate of them | **nothing — refused at ingestion** | — | — | retrieved at query time by the telemetry backend, returned as a digest, never stored |

#### F. The telemetry backend contract

- **FR-040**: The telemetry backend MUST accept a request naming: a pointer (or a node reference
  and a pointer kind), a reference instant, and a window specification, and MUST return a digest
  or an explicit failure. It MUST support the **log** pointer kind and MUST answer a monitor-state
  request for an alert in every configuration; it MUST support the metric and trace pointer kinds
  **where a data source for them exists**, and MUST answer per FR-049b where one does not. Every
  request MUST be one operation of the published query algebra (FR-040a).
- **FR-040a**: The telemetry backend MUST serve a **small, published, typed query algebra**, and
  MUST serve nothing outside it. **The algebra is published in one place and this specification does
  not restate it**: `specs/002-investigation-engine/contracts/telemetry-backend.md` §1 and
  `investigation.proto` §1–3 (ADR-0005 D7). From that document this backend MUST serve the **whole
  telemetry family — all eight terms**: `compare`, `onset`, `new_log_patterns`, `error_spans`,
  `errors_by_version`, `monitor_state`, `exemplars` and `drill_down`. It MUST NOT serve the graph
  family (typed wrappers over feature 001's query RPCs, answered from the replayed event log and
  never recorded into a world) or the knowledge family. Every request MUST be one named operation
  with typed arguments. Free-form query text supplied by a caller MUST NOT be executed, and an
  operation outside the algebra MUST be refused, naming what was asked for and what is available.
  The algebra is versioned with the digest contract (FR-056), and it is simultaneously the replay
  boundary, the vendor abstraction, the sanitisation point and the prompt-injection barrier.
- **FR-040b**: The whole algebra MUST be answerable **without tracing and without APM**, for the
  services whose logs live in Datadog, in the following way — this is what the rescoping requires
  of the backend and it is testable operation by operation:
  - **`new_log_patterns(pointer, window)`** MUST be answered from **log data alone**: mined
    patterns over the window compared with the baseline window, with counts, first-seen instants
    and the published cap on patterns and exemplars. It MUST NOT require a span, an APM metric or
    a service map.
  - **`errors_by_version(pointer, window)`** MUST be answered from **log data alone** where the
    pointer's `join_keys["version"]` names an attribute the logs carry (FR-040c), grouping
    error-level lines by it, each group carrying its `deploy.*` identifier where the value has one
    (FR-040d). How the service is deployed MUST NOT matter: the attribute is the service's own
    stamp, not something a deploy feeder supplies. Where the pointer names no version attribute,
    or names one no line in the window carries, the operation MUST answer `no_data` with coverage
    naming **every convention searched**, never a silent single group. *(Corrected 2026-09-27: this
    bullet used to say the logs carry a version "when the services are fed by features 003 and
    004". Feeding a service's deploys says nothing about its logs, and the audit found a service
    whose deploy platform no feature feeds.)*
  - **`compare(pointer, window_a, window_b)`** MUST be answerable over **log-derived counts and
    over monitor queries** where no APM metric exists, and the digest MUST state which of the two
    it used.
  - **`error_spans(edge, window)`** MUST answer per FR-049b where no span data source exists.
  - **`onset(pointer, search_window, method)`** MUST be served **backend-side**: the Datadog query
    (metric, or log-derived counts where no APM metric exists) returns the series **to the backend
    process**, which runs the published change-point method there and returns only the onset digest
    — the estimated instant, its uncertainty, the method and its parameters. Raw samples MUST NOT
    cross the digest boundary in either direction. The coverage block names which of the two
    sources the series came from.
  - **`monitor_state(pointer, window)`** MUST be answerable in **every** configuration, since a
    monitor exists wherever an alert does (FR-040).
  - **`exemplars(handle, limit)`** and **`drill_down(handle)`** MUST be answerable for every handle
    a digest of this backend minted, and MUST answer per FR-049b for a handle whose underlying data
    source is absent.
  The connector MUST NOT synthesise an APM answer from logs, and MUST NOT report a log-derived
  comparison as if it were a metric one: the coverage block names the data source it actually
  used, in every case.
- **FR-040c**: The connector MUST discover the attribute that carries a service's deployed version
  from a **published, ordered list of conventions**, and MUST let an operator override it per
  service. The default list, most specific first: Datadog's reserved `version` (unified service
  tagging, set by `DD_VERSION`); the OpenTelemetry `service.version`; `git.commit.sha` (Datadog
  source-code integration); `container.image.digest`; then the platform-specific revision
  attributes the vocabulary registry names. A candidate MUST be accepted only where it is present
  on at least a published share of the service's lines **and** of its error-level lines over the
  discovery window: a deployment stamp is on every line the service writes, while a field a
  library logs on a few lines of its own (an SDK or runtime version on start-up lines) is not.
  Whether the value changed during the window MUST NOT be a criterion, because a service that did
  not deploy that week has a constant, correct version. A rejected candidate MUST be named with the
  share it reached, so the discovery record says why it was skipped. *(Corrected during planning,
  2026-09-27: the first wording rejected a value "constant across every service and every day",
  which would also have rejected a real version that did not change.)*
  Discovery reads the **raw attribute** (`@version`, `@service.version`), never Datadog's derived
  SQL column alone, which can be empty where search shows a value. It is a cheap, bounded facet
  query per service per discovery interval, and its outcome — the attribute chosen, or every
  convention tried and why each failed — is recorded on the log pointer and in the checkpoint.
- **FR-040d**: Each version value an `errors_by_version` group carries MUST be normalised with the
  published `deploy.*` normalisers (`pkg/feeder` `deployref.go`): a full commit sha to
  `deploy.commit_sha`, an image digest to `deploy.image`, and any other stable identifier to
  `deploy.release`. The group MUST name that ref so the engine can resolve it to the CHANGE node
  that claims it, **from whichever feeder claimed it** — Cloud Run, GitHub Actions, Vercel,
  Kubernetes, or a feeder not yet written. A value that normalises to nothing (an abbreviated sha,
  a mutable image tag) MUST still be a group, carrying the raw value and a stated reason it names
  no deploy identifier, never dropped and never guessed into a longer form.
- **FR-040e**: The project MUST publish, with the connector, how to stamp the deployed version on
  logs for each deployment type it documents — at least Kubernetes, Cloud Run, Vercel, GitHub
  Actions-built containers, and a vendor-hosted runtime whose platform the graph does not feed —
  recommending the **full commit sha** as the value, because it is the identifier every deploy
  source states and so the one that always joins (FR-040d). The guide MUST state what each
  alternative loses: an image digest joins only to sources that state digests, a release name only
  to sources that state the same name, and a semantic version joins to nothing unless a deploy
  source states it.
- **FR-040f**: For every log source the connector is configured to watch, the connector MUST
  assert a SERVICE node, ref in the `datadog.service` namespace valued `<env>/<service>`, with
  claims for every other identifier the logs state (the OpenTelemetry service name, the platform's
  own service or agent identifier where present), carrying the log pointer, the monitor pointers
  and the version join key. It MUST do so **whether or not another feeder has asserted the same
  service**, and MUST NOT consult the graph to decide: what a connector emits must not depend on
  what other sources happened to deliver first (constitution III). Where another feeder's node is
  the same service, a published certain rule merges the two and the pointers are additive on the
  merged entity (FR-039, FR-059); where none is, the service is still investigable. It MUST NOT
  assert a node for a log source it is not configured to watch. Its valid start is unknown unless
  Datadog states a first-seen instant. *(Corrected during planning, 2026-09-27: the first wording
  asserted the node only "where no other feeder has asserted" the service, which made the
  connector's output depend on delivery order.)*
- **FR-040g**: When the version a watched service stamps on its logs changes (FR-040c), the
  connector MUST emit a CHANGE node of kind rollout with actor kind `UNKNOWN` unless the logs state
  one, a `changed-by` edge to the service, and claims for the new version's `deploy.*` identifier
  (FR-040d), so that certain rule C8 merges it with any deploy feeder's observation of the same
  rollout. Its valid start MUST be the first instant a line carrying the new version was indexed,
  and it MUST be recorded as a **bound** (the rollout happened at or before it), because logs state
  when a version was first seen running, not when it was deployed. Where a deploy feeder states the
  deploy instant, the merged change carries both, and the stated one is the rollout instant. A
  version that alternates between two values inside one window (a canary or a rolling deploy)
  MUST NOT emit a change per alternation: it emits one on first sight of each value and records
  the overlap. No change is inferred from a value that normalises to no `deploy.*` identifier.
- **FR-041**: The window specification MUST support at least: an explicit interval, and a
  before/after pair around the reference instant with a caller-supplied duration. When a
  before/after pair is asked for, the digest MUST include an explicit comparison between the two
  windows, not two unrelated summaries.
- **FR-042**: The request MUST record the reference instant it was asked about and the instant at
  which it was executed. Observed-time pinning has no meaning against a live telemetry backend,
  which has no memory of what it used to say; the answer MUST therefore state the execution
  instant, and a reproducible investigation MUST use recorded responses rather than a re-execution
  (FR-050).
- **FR-043**: A metric digest MUST consist of, per series: the series' identifying tags, the
  number of points, the interval actually covered, the resolution Datadog returned, summary
  statistics from a published set, and — for a before/after request — the direction and magnitude
  of the change. It MUST NOT contain the point-by-point samples.
- **FR-044**: A log digest MUST consist of counts grouped by a published grouping (at least
  status and pattern), the top patterns by count up to a published limit, and at most a published
  number of sample lines per pattern, each redacted by rule and truncated to a published length.
- **FR-044a**: The backend MUST be able to return **bounded, sanitised exemplars** — for example
  one stack trace per error kind or one example line per log pattern — but MUST do so **only when
  the caller asks for them explicitly**, never by default. Exemplars MUST be redacted by the same
  rules as any other sample (FR-052), MUST be capped by the published limits, and MUST count
  towards the digest's size bound.
- **FR-045**: A trace or span digest MUST consist of counts and latency summary statistics grouped
  by operation and error kind, the top error kinds up to a published limit, and identifiers that
  let a human open the traces concerned. It MUST NOT contain span payloads.
- **FR-046**: A monitor-state digest MUST consist of the transitions in the window with their
  instants, the state at the start and end of the window, the per-group states where the monitor
  has groups, and the number of transitions.
- **FR-047**: Every digest MUST be bounded by published limits: maximum series, maximum groups or
  patterns, maximum samples per group, maximum length per sample, and a maximum total size. Every
  truncation MUST be stated explicitly in the digest, naming what was dropped and the criterion
  used to choose what was kept.
- **FR-048**: Every digest MUST carry its evidence: the exact query as sent, the pointer and node
  it came from, the requested window and reference instant, the instants of execution, the
  identifier Datadog returns for the request where it returns one, and the connector and
  vocabulary versions.
- **FR-048a**: Every digest MUST carry a **coverage** block, and a digest without one MUST be
  invalid. Coverage MUST state at minimum: what was searched (which entities, which index or data
  source, and the window actually covered); the data volume considered; the sampling Datadog
  applied or the backend requested; what was truncated and by which criterion; and the backend's
  **indexing lag** at the instant of execution. A coverage field that cannot be determined MUST be
  stated as undetermined, never omitted.
- **FR-048b**: Every digest MUST preserve the **join keys** that let its answer be correlated with
  the graph and with other digests: the version tag, the pod or host identity, trace and span
  identifiers, and the first-seen instant of each pattern, error kind or series. Sanitisation MUST
  pseudonymise these consistently (FR-073) rather than drop them — a digest whose keys do not join
  is evidence about nothing.
- **FR-048c**: Every digest MUST carry **drill-down handles**: for each group, pattern, series or
  error kind it reports, a handle the caller can present back to the backend to obtain the
  narrower answer, and a deep link that opens the same query over the same window in Datadog for a
  human. A drill-down handle MUST be a reference, never an embedded payload.
- **FR-049**: The backend MUST distinguish, as separate and explicitly named **typed outcomes**: a
  digest; **`no_data`** (the query was valid and matched nothing in the window); **`query_failed`**
  (with a reason from a published set — rejected by Datadog with Datadog's reason, not permitted
  because the credential lacks the scope, rate limited with the earliest retry instant Datadog
  states, or timed out); **`not_yet_ingested`** (the window falls inside the backend's indexing
  lag, so nothing may be concluded from an empty answer); and **partial** (some of the answer was
  retrieved, with what is missing named). `no_data` MUST NOT be reported as unknown, MUST NOT be
  reported as an error, and MUST NOT be reported for a window that is merely too recent.
- **FR-049a**: In recorded mode the backend MUST add the outcome **`not_recorded`** for an
  in-algebra request the recording does not cover. `not_recorded` MUST be distinct from `no_data`
  and from `query_failed`, and the backend MUST NOT fall through to a live call to satisfy it.
- **FR-049b**: Where an algebra operation needs a data source the organisation does not have —
  `error_spans` with **no tracing**, an APM metric with no APM — the backend MUST answer
  **`no_data`** with a coverage block that names the **missing data source** as the reason ("no
  span data source configured for this organisation") and names what it did search. It MUST NOT
  answer `query_failed`, MUST NOT answer `unknown`, MUST NOT answer an empty digest with no
  explanation, and MUST NOT substitute a different data source. The answer MUST be identical in
  recorded and live mode, and MUST be stable for the whole window rather than varying per call, so
  that an investigation can conclude "this cannot be checked here" rather than retrying.
- **FR-050**: The backend MUST run in a recorded mode that answers from recorded Datadog responses
  with **identical digests** to the live mode for the same request, and MUST make no network call
  in that mode. Identity MUST include the coverage block (FR-048a) and the join keys (FR-048b),
  not only the summary statistics. Recorded responses MUST be addressable by the request that
  produced them.
- **FR-050a**: A recorded window MUST be a **world recording**, not a trajectory: the recording
  MUST cover the cross product of the query algebra (FR-040a) over the alert neighbourhood and the
  window, not only the queries one investigation happened to ask. A non-deterministic consumer
  that asks a different but in-algebra question of the same window MUST still be answerable from
  the recording without a live call.
- **FR-050b**: The **miss rate** — the share of in-algebra requests over a recorded window that
  return `not_recorded` — MUST be measured and reported as a fixture-quality metric on every
  verification run, against a published threshold.
- **FR-051**: No digest, and no part of one, may be written into the graph as a node, an edge, a
  property or an event. Digests are transient results handed to the caller; where the caller keeps
  one, it keeps it in its own investigation record, not in the graph (constitution IV and V).
- **FR-052**: The backend MUST redact by rule before a sample leaves the process: at minimum email
  addresses, credentials and tokens, and the identifiers named by the sanitisation policy (Q2).
  Redaction MUST be applied in live mode as well as when recording, so that a live investigation
  cannot surface what a recording would not be allowed to keep. In particular, people identifiers
  MUST be **dropped rather than hashed** in live mode too, and log content MUST be reduced to
  masked templates before it leaves the process (FR-072a, FR-075).
- **FR-053**: The backend MUST accept the same pointers the feeder emits without translation. A
  pointer the backend cannot execute MUST be reported as unsupported, naming the pointer kind and
  vocabulary, rather than executed as something else.
- **FR-054**: Executing a pointer MUST NOT change anything in Datadog and MUST NOT create a
  Datadog object of any kind, including saved views, notebooks or scheduled queries.
- **FR-055**: Every execution MUST draw on the same budget as the feeder half (section J) and MUST
  be reportable separately from the feeder's usage, so Priya can see what investigations cost.
- **FR-056**: The digest shapes, their published limits, the failure outcomes and the evidence
  fields are a published contract, versioned like the schema, because the investigation engine and
  its recorded fixtures depend on them.

#### G. Identity and resolution

- **FR-057**: The connector MUST mint identity claims and MUST NOT merge anything itself. Every
  identifier Datadog knows about an entity — including the one the connector addresses it by —
  MUST be a claim with the supporting attributes a rule needs (at minimum the environment, and the
  Kubernetes namespace, workload and cluster where the tags state them).
- **FR-058**: The connector MUST introduce and publish its identifier namespaces. At minimum: a
  Datadog monitor namespace and a Datadog log-source namespace, always; and, where the
  corresponding capability is enabled, a Datadog service namespace whose value carries **both the
  environment and the name**, a Datadog host namespace and a Datadog change namespace. Hosts are
  infrastructure resources, not services. The connector MUST NOT declare or emit a Datadog
  incident namespace.
- **FR-059**: A **certain** rule MUST be published for the case that matters most: a Datadog
  service and an OpenTelemetry service are one entity when the service names are equal, the stated
  environments are equal, and, where both sides state a Kubernetes namespace or cluster, those
  agree. This is certain rather than probable because Datadog APM ingests OpenTelemetry and maps
  the OpenTelemetry service name onto its own service tag: equality of name and environment is a
  configured assertion of identity, not a resemblance. A missing environment on either side MUST
  NOT satisfy it.
- **FR-060**: A **certain** rule MUST be published for the case where a Datadog service's own tags
  name a Kubernetes workload the graph already knows (namespace and workload name, and cluster
  where stated): the instrumentation is running inside that workload, so they are one entity.
- **FR-061**: **Probable** rules MUST cover the cases where the above do not hold but the evidence
  is suggestive: normalised-name equality without an agreed environment; a Datadog host name equal
  to a known cluster node name without corroborating tags; a shared owning team with a similar
  name. Probable rules MUST only produce suggestions.
- **FR-062**: Disagreement MUST surface, never resolve silently. Where the rules would merge a
  pair a human has rejected, or would merge a third entity into a pair a human has pinned, the
  result MUST be a conflict for review. Where two Datadog organisations or two environments
  produce the same service name, the entities MUST remain separate.
- **FR-063**: Every merge involving a Datadog claim MUST be explainable by the graph's audit
  query: the claims, the rule, the score, the rationale and any human decision, in observed order.
- **FR-064**: The connector MUST NOT reuse an existing published namespace for something it does
  not mean, and MUST NOT emit a reference in a namespace it has not declared.

#### H. Tags and ownership

- **FR-065**: Team and owner tags MUST become OWNER nodes with `owned-by` edges from the entities
  they own, with an unknown valid start where Datadog does not state one.
- **FR-066**: The tag keys the connector reads MUST be governed by an explicit allowlist. A tag
  key not on the allowlist MUST NOT become a property, a node or a claim (Q2).
- **FR-067**: Tag values MUST be treated as identifiers and names, never as measurements. A tag
  whose value is a number that measures something MUST NOT become a property.
- **FR-068**: Where two tag keys name the same owner differently, both claims MUST be kept and the
  resolution layer MUST decide in the open.

#### I. Recordings, sanitisation and evaluation

- **FR-069**: The connector's test corpus MUST be **recorded real payloads** from the
  organisation's Datadog, sanitised, committed as fixtures with their resulting event streams and
  golden query outputs. Synthetic-only data is insufficient for the connector to be marked stable.
- **FR-069a**: Real production recordings MUST NOT be committed to the **public** repository in
  v1. The sanitised corpus MUST live in a **private repository with private CI**, and the public
  repository MUST carry **synthetic structural twins**: the same event shapes, the same sequences,
  the same digest, coverage and failure contract, with no identifier derived from the
  organisation. The public conformance suite MUST run against the twins, and the private corpus
  MUST run the same suite (Q2).
- **FR-070**: The campaign MUST cover at minimum: a baseline window over the in-scope
  environment(s); an incident window containing a real monitor transition and the change that
  preceded it; a window containing a rate-limit response; and a window containing a poll that
  failed part-way (Q1).
- **FR-070a**: Campaign windows MUST be selected by **incident density** — the flows where
  incidents actually occur — and MUST NOT be selected by business importance. The campaign record
  MUST state the density evidence the selection rests on (Q1).
- **FR-070b**: The campaign MUST include **fault-injection game days in staging**, each recorded
  with the injected fault as its ground-truth label, because they are the only labelled ground
  truth the project controls.
- **FR-070c**: The campaign MUST include **boring windows and false-positive alerts**, labelled
  with the correct answer "nothing implicated", so that the corpus can measure whether the system
  is willing to implicate nothing.
- **FR-070d**: Topology recording MUST begin as soon as a read-only credential exists, **before**
  the campaign scope is finally agreed, because topology history cannot be reconstructed
  retrospectively. The scope in force MUST be recorded in the checkpoint throughout, so a later
  reader can tell "not present" from "not in scope at that time" (Q1).
- **FR-071**: Recorded **telemetry backend responses** MUST be committed alongside, addressable by
  the request that produced them, so that the investigation engine can replay an investigation
  with no call to Datadog and get identical digests.
- **FR-072**: A **sanitisation contract** MUST be published as part of this feature and MUST
  state, per field and per tag key, exactly one of: recorded verbatim (on an allowlist), replaced
  by a keyed pseudonym, or dropped. It MUST cover at minimum host names, IP addresses, user names
  and email addresses, organisation and account identifiers, credentials and tokens, customer
  identifiers appearing in log samples, and free-text fields. Its version MUST be recorded in
  every fixture manifest (Q2).
- **FR-072a**: The contract MUST assign the dispositions the review settled (Q2): **people
  identifiers are dropped, never hashed** — a hashed email address is still personal data and MUST
  NOT be treated as anonymised; **infrastructure identifiers** (service, host, cluster, namespace,
  workload, pod, team, environment) are **pseudonymised with a keyed HMAC**; **free-text fields
  and message bodies are dropped** unless a published rule says otherwise.
- **FR-073**: Identifiers that must remain identifiers for the fixture to be meaningful MUST be
  pseudonymised with a **keyed HMAC**, **consistently across a whole corpus** — the same input
  always yields the same pseudonym — so that relationships, and the digest join keys of FR-048b,
  survive sanitisation. Neither the key nor any mapping table MUST be committed.
- **FR-074**: Monitor notification message **bodies MUST be dropped entirely** rather than redacted
  in place, because they routinely carry webhook targets, handles and occasionally credentials.
  The monitor's name, query, thresholds and tags MUST be kept, because they are what the graph
  needs.
- **FR-075**: Log messages MUST be stored as **mined templates with their variables masked**,
  never as raw lines: the template is the shape that repeats, and every value that varies between
  occurrences MUST be masked. Where an exemplar is kept at all (FR-044a), it MUST be redacted by
  rule and capped in number and length by the same published limits the live digest uses, so that
  a recording can never contain more than a live answer would have shown.
- **FR-076**: Every committed recording MUST carry a **signed manifest** stating the sanitisation
  policy version, the content hash of what was signed, the named individual who signed it off, the
  date, and what was dropped; and, where a second person exists, a second signature (four-eyes).
  A payload that cannot be made safe MUST be dropped from the recording and the drop MUST be
  documented (Q2).
- **FR-076a**: Sanitisation MUST happen **in the connector, before anything touches disk**. No
  unsanitised Datadog payload may be written to disk, to a log or to any artifact at any point,
  including during a failed or aborted run.
- **FR-076b**: Every committed artifact MUST pass an **independent secrets-and-entropy scan** at
  commit, implemented separately from the sanitiser so that one defect cannot both leak and pass.
- **FR-076c**: **Canary tokens** MUST be seeded into the source data before a campaign and MUST be
  asserted absent from every committed artifact. A surviving canary MUST fail the commit rather
  than raise a warning.
- **FR-076d**: The same sanitisation boundary MUST govern what leaves the process towards a
  **model provider**: nothing may be sent to a model that would not be permitted into a recording
  under the same contract and policy version.
- **FR-077**: Every fixture MUST satisfy the published conformance suite: replay from empty to its
  golden graph, double delivery as a no-op, and shuffling within the declared reordering window
  leaving valid-time state unchanged.
- **FR-078**: The connector MUST pass a **zero-payload check**: over the entire recorded corpus,
  every event it emits passes the graph's telemetry-payload validation, and the telemetry backend
  half emits no graph event at all. At least one fixture MUST contain an event that must always be
  refused, so the rejection path is exercised.
- **FR-079**: A **live parity check** MUST be documented as in feature 001's live run: the graph
  built live against the organisation MUST equal the graph built from the recording of the same
  run, and a digest computed live over a settled window MUST equal the digest computed from the
  recorded response for the same request.
- **FR-080**: Coverage MUST be measurable against Datadog's own answer: the set of services the
  connector produced for an environment compared with the set Datadog lists for it, and the set of
  dependency edges compared with Datadog's service map for the same window, with the differences
  enumerated rather than summarised.

#### J. Budget, rate limits and operability

- **FR-081**: The connector MUST have a configurable call budget and MUST plan each polling cycle
  and each pointer execution to stay inside it.
- **FR-081a**: The budget MUST be expressed as a **share of the remaining vendor quota**, read from
  Datadog's own rate-limit response headers, rather than only as a flat call count. Where Datadog
  returns no such header for an endpoint, the connector MUST fall back to the configured static
  budget and MUST say in its usage report that it did so.
- **FR-081b**: Budgets MUST be **per endpoint class**, not global, because Datadog's limits are per
  endpoint — the span search and aggregate class is the binding one, on the order of 300 requests
  per hour. That quota is **shared with the humans working the incident**: the connector MUST leave
  a published reserve unspent, and MUST reduce its own allowance as the remaining quota falls.
- **FR-082**: When the budget cannot cover a full cycle, the connector MUST defer work in a
  **published** priority order, MUST report what it deferred and why, and MUST reflect the reduced
  coverage in its checkpoints. Stopping for quota MUST be reported as a typed reason, distinct
  from stopping for lack of evidence.
- **FR-082a**: The **width of a query window** MUST be capped, per pointer kind and cost class. A
  request wider than its cap MUST be narrowed to the cap or refused with the cap named; it MUST
  NOT be issued as asked.
- **FR-083**: The connector MUST honour Datadog's rate-limit responses, waiting at least as long
  as the response asks, and MUST resume from where it stopped rather than restarting the cycle.
- **FR-084**: The connector MUST report its usage: calls per Datadog area, share of the configured
  budget consumed, rate-limit responses received, and the feeder's and telemetry backend's usage
  separately.
- **FR-084a**: The usage report MUST additionally express consumption as a **share of the remaining
  vendor quota per endpoint class**, alongside the absolute counts, and MUST show how much of the
  published human reserve was left untouched.
- **FR-085**: A failed or partial poll MUST produce a checkpoint that states the extent actually
  covered and declares the gap, so a consumer can see that an answer for that window is
  incomplete.
- **FR-086**: The connector MUST report observed clock skew between the timestamps Datadog
  supplies and the graph's observed time, and MUST NOT correct one with the other.
- **FR-087**: The connector MUST document what it costs to run against an estate of a stated size:
  calls per hour per area at the default cadence, and the Datadog-side products and volumes it
  reads, so that Priya can approve it before it runs.

#### K. Incidents — removed by the rescoping

Datadog Incident Management leaves this specification entirely (ADR-0004; see the note where User
Story 9 was, and **Removed in rescoping** below). **FR-088, FR-088a, FR-089 and FR-090 are
deleted**, and nothing is renumbered in their place. Two things are worth stating so that nothing
is lost with them:

- The prohibition FR-089 carried is already absolute under **FR-004**, which forbids every
  state-changing Datadog operation by name, incidents included. Removing the section removes a
  requirement, not a safeguard.
- The evaluation labels FR-070e and FR-088a promised — which window was an incident, which
  services were implicated, when it was declared and resolved — are still needed. They now come
  from **spec 002**, through the Slack severity channel that is the organisation's real incident
  declaration (ADR-0004 D4). The label contract never depended on Datadog.

### Key Entities

- **Capability**: a named, independently enabled part of the connector — `logs`, `monitors`,
  `tags`, `apm_topology`, `changes` — with the Datadog areas and read scopes it needs. Which ones
  are in force is recorded in every checkpoint and every fixture manifest, because a disabled
  capability must read as "we were not looking", never as "there was nothing there".
- **Log source**: the Datadog log index, service and source attributes that identify one
  service's logs. It is what a log pointer selects on, and in this organisation it is the whole
  footprint of the connector's telemetry half.
- **Datadog service** *(apm_topology)*: a service in one environment of one Datadog organisation.
  Becomes a SERVICE node; identified by environment and name; carries version, tags, ownership and
  pointers.
- **Service dependency**: a directed, windowed relationship between two Datadog services. Becomes
  a `calls` edge with a traffic weight class.
- **Monitor**: a Datadog alert definition with a query, a type, a severity and optional groups.
  Becomes an ALERT node; its state transitions become versions in valid time.
- **Monitor group**: one alerting dimension of a grouped monitor. Becomes its own alert.
- **Datadog change event**: a deployment, configuration or infrastructure event Datadog recorded.
  Becomes a CHANGE node with a kind, an actor, an origin link and targets.
- **Tag**: a key and value Datadog attaches to an object. Becomes a property, an owner, or an
  identity claim, according to the allowlist — or nothing.
- **Datadog pointer**: kind, backend family `datadog`, a versioned Datadog query vocabulary, the
  selector, and OpenTelemetry attributes identifying the entity.
- **Digest**: the bounded, structured answer to one pointer execution over one window, with its
  evidence and its explicit truncations. Never stored in the graph.
- **Digest request**: pointer or node reference, pointer kind, reference instant, window
  specification, and the limits to apply.
- **Recorded response**: one Datadog answer, sanitised, addressable by the request that produced
  it, committed so an investigation can be replayed offline.
- **Recording campaign**: a set of windows recorded from the organisation, their sanitisation
  record, the sign-off, and what was dropped.
- **Alert transition**: one monitor state change, keyed on monitor, group and transition instant.
  It is the unit of intake, whichever transport delivered it.
- **Query algebra operation**: one named, typed question the telemetry backend will answer, and the
  only kind of thing it will answer. The term list, its three families and which of them a recorded
  world holds live in one place — `specs/002-investigation-engine/contracts/telemetry-backend.md`
  §1 — and are **not** restated here; this backend serves that document's telemetry family in full
  (all eight terms).
- **Coverage**: the mandatory part of a digest that states what was searched, over how much data,
  with what sampling, what was truncated and by which criterion, and how far behind the index was.
- **World recording**: the cross product of the query algebra over one alert neighbourhood and one
  window, together with its miss rate.
- **Actor kind**: one member of feature 001's published set (ADR-0005 D1) — `PERSON` ("a person"),
  `AUTOMATION` (a CI, bot or deploy principal acting on a person's behalf), `CONTROLLER` (an
  autoscaler, scheduler or platform automation), `VENDOR` ("the vendor") or `UNKNOWN` ("something
  the connector cannot type").
- **Call budget**: the share of remaining vendor quota per endpoint class the connector may spend,
  its published deferral order, its window-width caps, and its usage report.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001** *(conditional on `apm_topology`)*: For every in-scope environment, at least 95% of the
  services Datadog lists appear as service nodes within one polling interval, and every difference
  between the two sets is enumerated with a reason. **With the capability disabled — the default,
  and this organisation's configuration — the criterion is reported as not applicable and MUST NOT
  pass vacuously**: what is measured instead is that zero service nodes were created from APM and
  zero APM calls were issued (SC-024).
- **SC-002** *(conditional on `apm_topology`)*: At least 95% of the dependency edges Datadog's
  service map reports for a window appear as `calls` edges valid over that window, and every
  difference is enumerated. The same not-applicable rule as SC-001 applies with the capability
  disabled.
- **SC-003**: For every monitor transition in the recorded corpus, the alert's valid time equals
  the transition instant Datadog reported, exactly, in 100% of cases; and the delay between the
  transition instant and the observed time is at or below the configured poll interval plus one
  minute at the 95th percentile with polling alone, or one minute at the 95th percentile when a
  doorbell webhook is configured as well (Q3); and where both transports deliver, exactly one
  `alert.transition` exists per transition in 100% of cases.
- **SC-004**: A grouped monitor with N alerting groups produces exactly N alerts, and zero
  monitor-level alerts caused by a group alerting, across the recorded corpus.
- **SC-005**: At least 99% of pointer executions over the recorded corpus and over a live
  verification run succeed or return an explicit "no data"; every remaining case carries a named
  failure outcome with its evidence, and zero return an unexplained empty answer.
- **SC-006**: 100% of digests are within the published size bounds, and 100% of truncated digests
  state what was dropped and by which criterion.
- **SC-007**: Zero telemetry payloads reach the graph: over the whole recorded corpus, 100% of
  emitted events pass the telemetry-payload validation, the telemetry backend half emits zero
  graph events, and the rejection path is exercised by at least one fixture.
- **SC-008**: A full polling cycle over the in-scope estate costs no more than the documented
  number of calls per hour per Datadog area, never exceeds its configured share of the **remaining
  vendor quota** reported in Datadog's rate-limit response headers for any endpoint class, and
  leaves the published human reserve unspent in 100% of cycles; usage is visible at all times and
  matches the number of calls in the recording to the exact call.
- **SC-009**: Digests computed from recorded responses are identical to digests computed live for
  the same request over a settled window, byte for byte, for 100% of the parity set.
- **SC-010**: The graph built live against the organisation equals the graph built from the
  recording of the same run, byte for byte, as in feature 001's live-run parity check.
- **SC-011**: 100% of committed recordings pass the automated secret and personal-data scan and
  carry a named sign-off; zero committed payloads contain a host name, email address, credential
  or customer identifier that the sanitisation contract does not permit.
- **SC-012**: Every shipped fixture replays from empty to its goldens, double-delivers as a no-op,
  and shuffles within the declared reordering window without changing valid-time state, on every
  verification run, with zero tolerance.
- **SC-013**: Zero automated merges result from probable rules; every automated merge involving a
  Datadog claim is explainable by the audit query in 100% of cases; and for services that exist in
  both Datadog and the Kubernetes or OpenTelemetry sources with an agreed environment, at least
  95% merge automatically under a certain rule.
- **SC-014**: A rollout observed by both the Kubernetes feeder and Datadog appears exactly once in
  the ranked change list, in the fixture recorded for that case.
- **SC-015**: Zero write operations are issued against Datadog: every request the connector is
  capable of issuing is on a published read-only list, verified from the connector's own recorded
  request log over the full corpus and the live run.
- **SC-016**: Sam, starting from a Datadog monitor identifier alone, obtains the alert, what it
  watches, what changed around it in the preceding window, who owns it, and executable pointers,
  in one command and under thirty seconds on the recorded corpus.
- **SC-017**: **Canary survival is zero.** Of the canary tokens seeded into the source data before
  each recording campaign, 100% are absent from every committed artifact, and a surviving canary
  fails the commit rather than raising a warning, on every verification run.
- **SC-018**: 100% of committed artifacts pass an **independent** secrets-and-entropy scan and a
  personal-data scan run separately from the sanitiser; zero people identifiers appear in any
  committed artifact in any form, **hashed forms included**; and zero unsanitised payloads reach
  disk at any point, verified over the full corpus including aborted runs.
- **SC-019**: Digests answered from the recorded corpus equal the live digests for the same request
  over a settled window on **every field including the whole coverage block** — what was searched,
  data volume, sampling, truncation, indexing lag — and on every join key, for 100% of the parity
  set; and the world-recording **miss rate** (in-algebra requests answered `not_recorded`) is at or
  below the published threshold and is reported on every verification run.
- **SC-020**: A forged, replayed or malformed doorbell notification **costs at most one poll** and
  zero graph writes: across the adversarial fixture set, 100% of such notifications produce no
  alert, no state version and no event, and the polls they induce never exceed the configured rate
  limit.
- **SC-021**: 100% of telemetry backend requests, over the recorded corpus and the live
  verification run, are one operation of the published query algebra; zero free-form queries are
  executed; and every out-of-algebra request is refused naming both the operation asked for and
  the operations available.
- **SC-022**: **Every monitor in the organisation becomes an ALERT node within one poll
  interval.** 100% of the monitors Datadog lists for the in-scope tags are present as alert nodes
  no later than one polling interval after the connector first sees them, measured against
  Datadog's own monitor list rather than against a sample; every difference between the two sets
  is enumerated with a reason; and a monitor that is deleted or leaves scope is retracted rather
  than left to go stale. This is the coverage criterion the rescoping promotes in place of SC-001:
  unlike the service list, the monitor list is real in this organisation, small, and complete.
- **SC-023**: The algebra is answerable **without tracing**: over the recorded corpus and the live
  verification run, 100% of `new_log_patterns` and `errors_by_version` requests for services whose
  logs live in Datadog are answered from log data alone, issuing zero span, APM-metric or
  service-map calls; 100% of `compare` requests state in their coverage block whether they used a
  log-derived count or a monitor query; and 100% of `error_spans` requests return a typed
  `no_data` whose coverage names the missing span data source — zero return `query_failed`, zero
  return `unknown`, and zero return an unexplained empty digest.
- **SC-024**: A **disabled capability is silent and stated**: with `apm_topology` and `changes` off
  — the default — the connector issues zero calls to their Datadog areas, requests zero scopes for
  them, emits zero nodes, edges, changes, claims or pointers derived from them, and 100% of
  checkpoints name the capabilities that were in force. Every other capability's goldens are
  byte-identical with these capabilities on and off.

## Assumptions

- Feature 002, the investigation engine, is specified and implemented first against recorded
  data; features 003 and 004 arrive before this one (ADR-0004). Feature 003 supplies the project's
  **first** live telemetry backend, over Cloud Logging and Cloud Monitoring; this feature supplies
  the **second**, which is what proves the backend contract is interchangeable rather than shaped
  around one vendor. The digest contract in section F is the seam and is versioned across all
  three.
- **Datadog is not the organisation's production stack.** It holds one service's logs and a
  handful of monitors; the applications run on Cloud Run and Vercel and ship from GitHub Actions,
  and there is no distributed tracing anywhere. Everything about the scope of this specification
  follows from that. One Datadog organisation is in scope for the first campaign; a second
  organisation is a second source and needs no schema change.
- **Where APM does exist** — another organisation, or this one later — a Datadog service name is
  normally the OpenTelemetry `service.name`, because Datadog APM ingests OpenTelemetry. FR-059
  depends on this and states the conditions under which it is a certain rule rather than a
  resemblance. With `apm_topology` off it is never exercised, and the certain rule that matters
  instead is the one linking a log source to the service feature 003 or 004 created.
- **How a service is deployed is not assumed.** It may run on a platform a feeder covers (Cloud
  Run, Kubernetes, Vercel) or on one none does (a vendor-hosted runtime, a VM, a platform added
  later). The connector asserts a node for every watched log source in its own namespace with
  identity claims (FR-040f); where a feeder also reports the service, resolution merges the two and
  the pointers are additive, and where none does, the connector's node is the one investigated. *(Corrected 2026-09-27: this used to
  say every such service already had a node from features 003, 004 or 001, and that an unknown
  log source was an unattached claim. The audit found the organisation's only Datadog service
  deployed on a platform no feature feeds, which under that rule could never be investigated.)*
- **A service stamps its deployed version on its own logs** (FR-040c, FR-040e). Where it does
  not, `errors_by_version` answers `no_data` naming what was searched, and no rollout is inferred
  from logs (FR-040g); nothing else in this feature depends on the stamp.
- The connector polls (Q3). Polling every 15–30 seconds is the v1 intake; a push path exists only
  as a doorbell that triggers a poll, and everything else is a pull at a configurable cadence.
  Nothing in the design depends on the webhook existing, and nothing reads its body.
- Traffic weight on `calls` edges remains the published coarse ordinal class, where the
  `apm_topology` capability is enabled at all. The rate Datadog reports is used to choose a class
  and is never stored.
- Datadog's own retention and rollup rules mean that a query over an old window may return
  different resolution than it did at the time. Live-versus-recorded digest parity (SC-009) is
  therefore asserted over windows inside retention and at a fixed resolution.
- The published edge taxonomy has no relation meaning "watches". Linking an alert to what it
  watches uses the closest published relation, and whether the taxonomy should gain one is a
  schema question for planning, not a licence for this connector to invent an edge type. It is
  collected, with the other schema gaps, under **Dependencies on feature 001**.
- Hosts are infrastructure resources. The connector does not create a node per container or per
  pod replica; the Kubernetes feeder owns that level of detail where it runs.
- The sanitisation contract, the digest limits, the tag allowlist, the poll cadences and the call
  budget are configuration with published defaults, not constants, and the configuration in force
  is recorded in checkpoints and fixture manifests.
- Feature 001's connector contract, event schema, resolution rules, pointer schema and fixture
  format are used unchanged. Anything this connector needs that they lack is a schema change
  with its own decision record, not a local extension; the list is **Dependencies on feature 001**.
- The corpus is split in two (Q2). The sanitised real recordings live in a private repository with
  private CI; the public repository carries synthetic structural twins (FR-069a). Any claim in the
  public repository is a claim about the twins, and a property that can only be demonstrated on
  real payloads is labelled as such rather than implied.
- Actor kind is derivable often, not always. Datadog states enough to type an autoscaler action or
  a pipeline deployment in the common cases; where it does not, `unknown` with the evidence that
  was available is the answer, and the consumer degrades rather than guesses (FR-033b).
- The query algebra is the whole surface (FR-040a). Feature 002's workers ask only algebra
  operations, which is what makes a world recording finite (FR-050a) and what lets the worker
  boundary double as the sanitisation point and the prompt-injection barrier. A reasoning path
  that needs a question outside the algebra is a request to extend the algebra, not to bypass it.
- The fault-injection game days need a staging environment that resembles production closely
  enough to be worth recording, and an owner to schedule them. Where neither exists, the
  labelled-ground-truth part of the corpus is empty and the evaluation is weaker by exactly that
  much (FR-070b).
- Incident labels for the evaluation corpus come from **spec 002**, through the dated severity
  channel the organisation creates in Slack (ADR-0004 D4). This connector supplies none of them
  and reads no incident-management product. An organisation that does declare incidents in Datadog
  would need a later increment, not a configuration flag.
- **Capabilities are the unit of scope** (FR-008a). A capability is off unless someone turns it
  on, an off capability is silent and says so, and every criterion that depends on one is reported
  as not applicable rather than passed vacuously (SC-001, SC-002, SC-024). This is what lets one
  specification serve an organisation with no tracing and an organisation with full APM without
  either of them reading a specification full of exceptions.

## Dependencies

- Feature 001: the event schema, the feeder contract, the published resolution rules, the pointer
  schema, the fixture format and the conformance suite.
- Feature 002: the consumer of the query algebra, the digest contract and the alert intake;
  specified in parallel. The algebra (FR-040a), the digest shapes, the coverage block and the
  failure outcomes in section F must be agreed between the two and versioned together. Feature 002
  also owns the Slack intake and the incident labels that used to be promised here.
- **Feature 003 (GCP integration)**: the project's **first** live telemetry backend and the
  nodes most of this connector's log pointers attach to. The digest, coverage and typed-outcome
  contract in section F is shared with it and must be published **once**; where 003 has already
  published it, this feature implements it rather than restating it. Its Cloud Run rollouts are
  what SC-014 merges against.
- **Feature 004 (deploy feeders)**: the deploy claim keys — `deploy.commit_sha`, `deploy.image`,
  `deploy.release` — that FR-031 mints for a Datadog-observed deployment. Where the `changes`
  capability is enabled, this connector must use those published keys and their normalisation, not
  keys of its own, or "one rollout, not two" fails silently.
- The organisation: a read-only Datadog credential provisioned by Priya (FR-002); a private
  repository and private CI to hold the corpus (FR-069a); a staging environment and an owner for
  the fault-injection game days (FR-070b); and the signing key and signatories for the
  sanitisation manifests (FR-076).

## Dependencies on feature 001

Everything below is a **schema or contract change in feature 001**, not a local extension here
(constitution IX). Each is stated as what this connector needs and what it does until it exists.

> **After the rescoping, most of this list is no longer this feature's to ask for first.** The
> actor kind, the unattached-and-auto-attach convention, the `alert.transition` event type with
> its idempotency-key convention, and the published home for the digest, coverage and
> query-algebra contract are all needed by features 002, 003 and 004, which now arrive earlier.
> Each must be satisfied **once**, in 001, not three times in three connectors. What remains
> specific to this feature is the "watches" relation, the alert-state vocabulary with its
> "sampled" marker, and the Datadog pointer-vocabulary registry entry.

- **A "watches" relation.** The published edge taxonomy has `calls`, `depends-on`, `runs-on`,
  `deployed-by`, `owned-by`, `exposed-via` and `changed-by`. None of them means "this alert
  watches this entity", which is the most-used edge in this feature (FR-019). Until the taxonomy
  gains one, the connector uses the closest published relation and records the substitution; it
  does not invent an edge type.
- **An actor kind on a change.** A change carries an actor as a free-text name and nothing that
  says whether the actor was a person or an autoscaler (FR-033a, FR-033b). Feature 002's causal
  ordering cannot exonerate an autoscaler reaction without it. Needs a published set —
  `{PERSON, AUTOMATION, CONTROLLER, VENDOR, UNKNOWN}`, settled in ADR-0005 D1 — on the change node
  and in the change-observation event.
- **An `alert.transition` event type, or its published mapping.** The ingestion contract's event
  bodies are upserts, retractions, change observations, identity claims and checkpoints; there is
  no typed alert transition (FR-025). Either feature 001 gains one, or it publishes the mapping
  onto the existing events **together with the idempotency-key convention** — the 4-tuple (source,
  stable alert identifier, group, transition instant), settled in ADR-0005 D2 — because two
  transports emitting the same transition (FR-025a) is exactly what that key exists to defeat.
- **A published alert-state vocabulary, and a "sampled" marker on a state history.** An alert's
  state lives in free-form node properties today. This connector needs the state values (ok, warn,
  alert, no-data) published, and needs a published way to say that a history is *sampled at an
  interval* rather than complete (FR-024); without it a consumer cannot tell a quiet monitor from
  an unwatched one.
- **A published convention for "unattached", and for automatic attachment.** Alerts and changes
  whose targets the graph does not yet know are kept and attached when the target appears
  (FR-019, FR-030). The marker and the attachment rule belong to feature 001, not to each
  connector inventing its own.
- **A registry entry for the pointer vocabulary.** A pointer's vocabulary is a free string. The
  Datadog query vocabulary (FR-035) needs a registered, versioned name and the published statement
  of why its selectors cannot be expressed in OpenTelemetry semantic conventions (constitution IV).
- **A published home for the digest, coverage and query-algebra contract.** The pointer schema
  says where to look; nothing published says what an executed pointer answers with. The query
  algebra (FR-040a), the digest shapes, the coverage block (FR-048a), the join keys (FR-048b), the
  drill-down handles (FR-048c) and the typed outcomes (FR-049, FR-049a, including `not_recorded`)
  are a versioned public contract shared with feature 002 and need a published home.
- **Nothing else.** The node taxonomy (service, alert, change, third-party, infrastructure
  resource, owner), the bitemporal model, the ordinal traffic weight on `calls`, the identity-claim
  and resolution events, and the checkpoint's gap declaration are sufficient as published.

## Removed in rescoping

What ADR-0004 changed, in one place, so a reviewer can see the delta without diffing two drafts.
**Nothing that survives has been renumbered**: deleted requirement numbers are retired, not
reused.

### Removed outright

| What | Where it was | Why |
|---|---|---|
| **Datadog Incident Management as a graph source** | User Story 9; FR-088, FR-090; the incident row of the mapping table; the `datadog.incident` namespace in FR-058; the incident scope in FR-002 | The product is **unused** in the audited organisation. Incidents are declared by creating a dated severity channel in Slack (ADR-0004 D4). |
| **Incident records as evaluation labels** | FR-070e, FR-088a; User Story 9's fourth scenario | The labels are still needed; they now come from spec 002 through the Slack intake. The contract never depended on Datadog. |
| **The prohibition on writing to incidents** | FR-089 | Redundant, not relaxed: FR-004 already forbids every state-changing Datadog operation by name, incidents included. |
| **"The first live telemetry backend in the project"** | User Story 5's rationale; the Assumptions | Feature 003 is now first, over Cloud Logging and Cloud Monitoring. This connector is the second, which is a better test of the contract than being the only one. |
| **"The topology is real" as a reason this feature exists** | the Why section | There is no tracing in this organisation, so a vendor service map is an empty answer rather than a small one. |

### Demoted

| What | From | To | Why |
|---|---|---|---|
| **APM service-map topology** — service list, dependency map, hosts, third-party promotion, APM deployment tracking, APM metric and trace pointers | User Story 1, P1; section B unconditional | User Story 1, **P3**, capability `apm_topology`, **OPTIONAL and off by default** (FR-008a, FR-008b) | ADR-0004 D3. No tracing here; the mapping is kept intact and correct for organisations that have it. |
| **Datadog's event stream as a change source** | User Story 3, P1; section D unconditional | User Story 3, **P3**, capability `changes`, **off by default** | ADR-0004 D1 and D2. Changes come from Cloud Run, vendor notices, GitHub Actions and Vercel — the platforms themselves — which is more complete than events a pipeline may forget to post. The merge behaviour (FR-031, SC-014) is kept at full strength. |
| **SC-001 and SC-002**, coverage against Datadog's own service list and service map | unconditional pass/fail gates | **conditional on `apm_topology`**, reported as *not applicable* when it is off, and explicitly forbidden from passing vacuously | The sets they compare are empty here, so an unconditional criterion would be either a false pass or a false failure. |

### Added by the rescoping

- `FR-008a`, `FR-008b` — the capability model, and the rule that a disabled capability is silent
  **and stated** so that "not looking" never reads as "nothing there".
- `FR-040b` — the whole algebra answerable without tracing and without APM: `new_log_patterns`
  and `errors_by_version` from logs alone, `compare` over log-derived counts and monitor queries.
- `FR-049b` — the typed answer when a data source does not exist: `no_data` with a coverage block
  naming the missing source, never `query_failed`, never `unknown`, never an unexplained empty
  digest.
- `SC-022` — every monitor in the organisation becomes an ALERT node within one poll interval.
  This is the coverage criterion that replaces SC-001 as the one that actually bites here: the
  monitor list is real, small and complete.
- `SC-023` — the algebra is answerable without tracing, measured operation by operation.
- `SC-024` — a disabled capability issues zero calls, emits nothing, and says so in every
  checkpoint.

### Kept unchanged

Sanitisation and the private corpus (FR-069a, FR-072–FR-076d, SC-011, SC-017, SC-018); the quota
model as a share of remaining vendor quota per endpoint class with a human reserve (FR-081–FR-087,
SC-008); alert intake as a feeder with two transports, polling as truth and the webhook as a
doorbell, **verbatim** (FR-024, FR-025, FR-025a–FR-025c, SC-003, SC-020); identity, claims and the
published certain and probable rules (FR-057–FR-064, SC-013); the digest, coverage, join-key,
drill-down and typed-outcome contract (FR-040a, FR-043–FR-050b); "one rollout, not two" (SC-014);
and the read-only posture in full (FR-002–FR-004, FR-054, SC-015).

## Out of scope for this feature

- **Any write to Datadog**, of any kind: muting or resolving monitors, scheduling downtime,
  creating or updating incidents, posting events, editing dashboards, tags, the service catalog,
  saved views or notebooks. There is no configuration that enables one.
- Datadog as a store for the graph or for the event log.
- Datadog products this connector does not read: real user monitoring, synthetics, security
  products, CI visibility, profiling, database monitoring, cloud cost, and log or metric
  configuration surfaces. Reading them is a later increment, not a variant of this one.
- Ingesting dashboards or notebooks as graph content. They are links.
- Storing any metric, log, span or aggregate of them in the graph.
- A Datadog MCP transport. The published finding stands: for Datadog a plain versioned API
  connector beats the MCP surface, because the API is versioned and the MCP surface is not.
- The Google Cloud integration and the vendor-notice feeder (**feature 003**), the deploy feeders
  for GitHub and Vercel (**feature 004**), and the Slack intake for human-declared incidents
  (**feature 002**). They arrive before this one and they own the nodes, the changes and the
  trigger; this connector's job is to attach pointers to what they created and to answer questions
  about it.
- Any language-model reasoning. This feature supplies structure to the investigation engine; it
  does not reason.
- Autonomous remediation, proposal or otherwise.
- **Real production recordings in the public repository**, in v1. They live in the private corpus
  with private CI; the public repository gets synthetic structural twins (FR-069a).
- **Any telemetry query outside the published algebra.** The backend is not a general query proxy
  and does not execute caller-supplied query text; extending the algebra is a contract change
  (FR-040a).
- **Datadog Incident Management, entirely.** Not as a graph source, not as a source of evaluation
  labels, not as a requested scope. The product is unused in this organisation and the labels come
  from spec 002 (ADR-0004; **Removed in rescoping**). There is no configuration that enables it.
- **APM topology and the Datadog event stream by default.** Both are capabilities that are off
  unless someone turns them on (FR-008a), and an organisation with no tracing is expected to leave
  `apm_topology` off for ever rather than run it against an empty service map.
- **Datadog as the organisation's change source.** Cloud Run revisions, vendor notices, GitHub
  Actions runs and Vercel promotions are read from the platforms themselves in features 003 and
  004. Where the `changes` capability is enabled, this connector is a *second observer* whose
  contribution is a claim that lets the pair merge (SC-014) — never a replacement.
- **Synthesising an APM-shaped answer out of logs.** `error_spans` with no spans answers `no_data`
  with "no tracing" in its coverage (FR-049b); it does not approximate a span summary from log
  lines, and a log-derived comparison is never reported as a metric one (FR-040b).
- **A webhook as a source of truth.** The inbound endpoint is a doorbell that triggers a poll;
  there is no configuration that makes the notification body authoritative (FR-025b).
- Symptom-onset estimation, causal ordering, the hypothesis ledger and the grading of a
  non-deterministic investigator. Those are feature 002's; this connector supplies the actor kind,
  the digests and the recorded world they consume.
