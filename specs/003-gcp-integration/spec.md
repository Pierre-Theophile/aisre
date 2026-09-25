# Feature Specification: GCP Integration and Vendor-Notice Feeder

**Feature Branch**: `003-gcp-integration`

**Created**: 2026-09-17

**Status**: Draft

**Input**: User description: "The first vendor connector for the temporal system graph and the
first live telemetry backend for the investigation engine, retargeted by the coverage audit onto
the platform the organisation actually runs. Two feeders and one backend, shipped as one feature:
a GCP feeder that turns Cloud Run services and revisions, Cloud SQL instances, Cloud Run
configuration, Cloud Audit Logs and Cloud Monitoring alert policies into typed graph events; a
vendor-notice feeder that turns provider maintenance, deprecation and incident announcements —
from a shared mailbox, public status pages and API changelogs — into future-dated change nodes
with a vendor actor; and a GCP telemetry backend that executes the investigation engine's typed
query algebra against Cloud Logging, Cloud Monitoring and, where it exists, Cloud Trace, and
returns bounded digests. Read-only. Recorded, sanitised payloads from a real organisation are the
tests."

## Why this feature exists

Feature 001 built a substrate and proved it with two vendor-neutral feeders against a disposable
cluster. Feature 002 builds an investigation engine and proves it against recorded fixtures. The
coverage audit (`docs/evaluation/coverage-audit-2026-09.md`) then asked the only question that
matters about a substrate — *would the true cause have been in the graph at alert time?* — and
answered it for thirteen real production incidents:

| Feeder set | Cause is a node or change at alert time |
|---|---|
| Feature 001 feeders alone (OpenTelemetry spans, Kubernetes) | **0 of 13** |
| plus deploy feeders for the actual platforms | 3 of 13 |
| plus vendor change feeders | **8 of 13** |

Zero. Not a tuning problem, a coverage problem: the organisation runs its core applications on
Cloud Run and Vercel with Cloud SQL, Hasura and Mongo behind them, has exactly one Kubernetes
cluster (inference), has no distributed tracing anywhere, and declares incidents by opening a
dated severity channel in Slack. The OpenTelemetry topology feeder has no input at all here. And
the single most repeated cause across the corpus was **a vendor notice nobody read** — a
maintenance window, a deprecation, a provider incident, announced by email weeks in advance and
then rediscovered at 02:10 under a page.

ADR-0004 reordered the roadmap around that finding, and this feature is its first decision (D1).
Three things become true for the first time:

1. **The topology is the one that actually serves traffic.** Cloud Run services and revisions,
   the traffic split between them, the Cloud SQL instances behind them and the configuration and
   secret versions they were deployed with, taken from the platform that runs them rather than
   from a demo application in a disposable cluster.
2. **The graph can hold a fact about the future.** A maintenance window announced today for the
   second of next month is a change whose *valid* time has not arrived while its *observed* time
   is now. The bitemporal model was designed for exactly this and has never been asked for it.
   This is the first feature whose value depends on it, and the "missed maintenance email" is the
   pattern it defeats.
3. **The pointers can be executed.** Feature 001 established that the graph stores *where to
   look*, never what was measured, and no backend has yet executed a pointer. Cloud Logging and
   Cloud Monitoring are where this organisation's telemetry actually lives, so they are the first
   backend to make the promise real — and the one that turns feature 002 from a replayer of
   recordings into something that can answer a question about right now.

It is one feature and not three because the GCP feeder, the vendor-notice feeder and the GCP
telemetry backend are what lift the observable ceiling *together*: 8 of 13 is the number for the
pair of feeders plus the backend that can test the hypotheses they raise, and no subset of them
reaches it. It ships as two feeders over one integration because the platform half is one
credential, one quota budget and one vocabulary, while the vendor-notice half is a different
credential, a different cadence and a different trust model, and pretending otherwise would put
an inbox and a service account behind the same switch.

## Clarifications

### Session 2026-09-17

The owner's decisions in ADR-0004, taken from the coverage audit, are recorded here and applied
throughout the specification. They are decisions, not open questions.

- Q: Which connector is built first, and for which platform? → A: **The GCP integration, plus a
  vendor-notice feeder**, as one feature (ADR-0004 D1). Cloud Run revisions and Cloud SQL become
  change and infrastructure nodes; Cloud Logging and Cloud Monitoring become the first live
  telemetry backend; a thin vendor-notice feeder turns provider announcements into change nodes
  with a vendor actor. Datadog is rescoped and deferred to feature 005 (D3); GitHub Actions and
  Vercel deploys are feature 004 (D2).
- Q: What does the Kubernetes feeder cover now that the estate is serverless? → A: **The one
  inference cluster, and nothing else.** GKE stays with the feature 001 Kubernetes feeder; this
  feature reads no GKE workload and creates no node the Kubernetes feeder owns (FR-030, Out of
  scope).
- Q: Where do alerts come from, given that monitor transitions caught few of the corpus
  incidents? → A: **Both.** Cloud Monitoring transitions *and* human-declared incidents (ADR-0004
  D4). The Slack severity-channel transport is specified in feature 002's intake; this feature is
  responsible only for making GCP alert transitions emit the **same** `alert.transition` event
  under the **same** idempotency-key convention, so the two intakes converge on one alert
  (FR-046, FR-047).
- Q: What happens to the recordings from the owner's organisation? → A: The ADR-0003 D9 pattern,
  unchanged: sanitised **in the connector before anything touches disk**, people identifiers
  dropped rather than hashed, infrastructure identifiers pseudonymised with a keyed HMAC, logs
  reduced to mined templates, email bodies never stored at all, canary-tested, signed, kept in a
  **private corpus with private CI** — and the public repository ships **synthetic structural
  twins** (FR-127–FR-145).
- Q: How is this feature known to have worked? → A: By **re-running the coverage audit**. The
  published audit method, over the same corpus, must show the true cause was a node or change in
  the graph at alert time for at least **8 of the 13** incidents — the ADR-0004 number — with the
  per-incident attribution published (SC-023).

### Session 2026-09-17 (c)

Remediation of the `/speckit-analyze` cross-artifact pass over features 002–005. Findings closed
here: **X1** (the algebra's published home and the eight telemetry terms), **X2** (the actor-kind
set), **X3** (the `alert.transition` idempotency key) and **I12** (enum names in normative text).

- Q: Which algebra terms must this backend serve, and where is the term list published? (X1 —
  FR-087, FR-087a, FR-087b, Key Entities "Query algebra operation") → A: the list is **not restated
  here**. `specs/002-investigation-engine/contracts/telemetry-backend.md` §1 is its single home
  (ADR-0005 D7), and this backend serves that document's **telemetry family in full — all eight
  terms** (`compare`, `onset`, `new_log_patterns`, `error_spans`, `errors_by_version`,
  `monitor_state`, `exemplars`, `drill_down`), never the graph or knowledge families. `onset` is
  served **backend-side**: Cloud Monitoring returns the series to the backend process, which runs
  the published change-point method and returns only the onset digest, so raw samples never cross
  the digest boundary. A term whose data source this organisation lacks answers `no_data` with a
  coverage block naming the absent source — `error_spans` without a trace data source being the
  case that actually applies (FR-091).
- Q: What is the published actor-kind set, and what keys an `alert.transition`? (X2, X3, I12 —
  FR-019, FR-046, Key Entities "Actor kind") → A: `{PERSON, AUTOMATION, CONTROLLER, VENDOR,
  UNKNOWN}` (ADR-0005 D1), written with the enum name and the prose gloss in parentheses; and the
  4-tuple **(source, stable alert identifier, group, transition instant)** (ADR-0005 D2, 002
  FR-008b), which for GCP is source `gcp`, the alert policy identifier, the policy group and the
  transition instant Google reports.

### Session 2026-09-21

The two organisational inputs carried as campaign blockers since session 2026-09-17 are answered
here by the owner, closing C3 and C4 in [plan.md](./plan.md#complexity-tracking).

- Q: Which GCP projects and regions are in scope for the first recording campaign? (FR-131) → A:
  **the production project, all regions**, for the first campaign. Anything beyond that is
  **operator configuration**, not a constant: `config/gcp.yaml` holds the scope, FR-009 records the
  scope in force in every checkpoint, and no code may assume a project name. This matches the
  default research §11 proceeded under, so no design changes.
- Q: Which shared mailbox carries the vendor notices, who owns it, and by what read-only path?
  (FR-132) → A: a **shared engineering mailbox** is the intended source. Two things about it were
  established before writing the feeder, and the second changes the design:
  1. The address's **kind is unconfirmed** — Workspace mailbox or Google Group — and the kind decides
     the path, because a Group's archive is readable by no API. If it is a Group, the feeder reads a
     subscribed member's mailbox. Confirming this is an owner action, as is authorising the read.
  2. **Per-user delivery is the normal case, not the exception.** Google Cloud sends platform notices
     to each project's **Essential Contacts** and to its billing and owner principals; a shared
     address receives them only where someone has configured it as a contact. So a feeder that reads
     only a shared mailbox would miss exactly the announcements the coverage audit found mattered.
     **FR-132a** makes per-user delivery a first-class path and **FR-132b** makes the address,
     path and sender allowlist configuration, with a loud failure when a configured mailbox yields
     nothing.

  `essentialcontacts.contacts.list` is a read-only call, so the connector can *report* where the
  vendor is configured to send notices for each in-scope project — which tells an operator which
  mailboxes to authorise. That is recorded as a follow-up task rather than built now, because it
  locates a notice stream without reading one.

## Personas

- **Sam, on-call SRE.** Is paged at 02:10. Today Sam opens the Cloud Run console, the revision
  list, the Cloud Logging query they keep in a browser tab, and then — an hour later, if at all —
  finds the provider's maintenance email in a mailbox nobody watches. Sam wants the graph to
  already hold the revision that shipped, the traffic split that moved, the configuration that
  changed, *and* the vendor window that was announced three weeks ago, all as changes on one
  ranked list, with links that open GCP at the right query.
- **Casey, connector author.** Writes and maintains both feeders and the backend. Wants GCP's
  shape to map onto the published event schema with no special cases in the graph, wants the
  recorded payloads to be the test, and wants to know, when Google changes an answer, which
  fixture goes red.
- **Dana, the GCP project owner.** Provisions the service account and owns the organisation's API
  quotas and its Cloud Logging and Cloud Monitoring bill. Will not hand out a principal that can
  deploy a revision, change an instance flag or delete a log sink. Also owns — or has to find the
  person who owns — the shared mailbox the vendor notices arrive in, and needs to know exactly
  what a tool reading that mailbox keeps and what it throws away.
- **The investigation engine (machine consumer, feature 002).** Asks for one operation of the
  typed query algebra over a window around an instant and consumes the answer as structure:
  bounded, summarised, coverage-carrying, evidence-carrying. It must be able to replay the same
  investigation from recorded responses and get the same answer.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The serving topology, from Cloud Run (Priority: P1)

Sam's organisation runs its core applications as Cloud Run services. The feeder reads the
services and their revisions for the in-scope projects and regions and turns them into SERVICE
and WORKLOAD nodes, with the traffic split between revisions as a property of the service, the
environment derived from the project and its labels, and the region and project recorded. Every
revision creation and every traffic shift becomes a ROLLOUT change node, each carrying the
principal that performed it from the Cloud Audit Logs and an **actor kind** that says whether a
person, a deployment pipeline, the platform itself or the vendor did it.

**Why this priority**: Everything else in this feature hangs off these nodes — an alert with no
service, a change with no target and a pointer with no node are all unattached. It is also the
story that lifts the coverage number off zero: in this organisation the deployed unit *is* a
Cloud Run revision, and until it is a node the graph has nothing to say about a deploy.

**Independent Test**: Run the feeder against recorded Cloud Run and Cloud Audit Log payloads for
one project, from an empty graph, and compare the resulting graph to the golden output. Delivers
a queryable picture of what is serving traffic, with no alerting, no vendor notices and no
telemetry execution present.

**Acceptance Scenarios**:

1. **Given** recorded Cloud Run payloads for an in-scope project and region, **When** the feeder
   runs from an empty graph, **Then** every service appears as a SERVICE node carrying its
   project, region and derived environment; every revision it reports appears as a WORKLOAD node
   with a `runs-on`-style relation to its service; and the service carries the traffic split
   across revisions as a property, never as a measurement.
2. **Given** a revision created at 14:18 and given 100% of traffic at 14:20, **When** the feeder
   runs, **Then** two ROLLOUT change nodes exist — one for the revision's creation valid at
   14:18, one for the traffic shift valid at 14:20 — each stating whether it moved production
   traffic, and the traffic-shift change is the one whose valid time is the instant what
   production served changed.
3. **Given** the Cloud Audit Log entry for that deployment names a user principal, **When** the
   feeder runs, **Then** the change carries that principal as its actor and an actor kind of
   `PERSON`; **and given** the entry names a principal on the configured deployment-automation
   list, **Then** the actor kind is `AUTOMATION`; **and given** the entry names a platform service
   agent, **Then** the actor kind is `CONTROLLER` — and in no case is the kind inferred from the
   principal string alone.
4. **Given** the same payloads, **When** the events are inspected, **Then** each service and
   revision carries identity claims for every identifier GCP knows about it — the fully qualified
   resource name, the container image reference, the revision name, the configured service name
   and environment labels — and the feeder has merged nothing itself.
5. **Given** a Cloud Run service the feeder has seen and that GCP no longer lists for the
   declared number of consecutive polls, **When** the feeder runs, **Then** it is retracted with
   a valid end inside the last poll it was observed in, not deleted — and nothing is retracted
   from a poll that failed part-way.
6. **Given** any node this feeder creates, **When** Sam asks for its pointers, **Then** it
   carries the Cloud Logging, Cloud Monitoring and source-link pointers of section H, versioned
   in valid time with the node.

---

### User Story 2 - A vendor notice becomes a change that has not happened yet (Priority: P1)

The most repeated cause in the corpus was an announcement nobody read. The vendor-notice feeder
watches a configured allowlist of the organisation's providers — the LLM API provider, the GPU
host, the voice-infrastructure vendor, the serverless-GPU vendor, the ATS vendors — across three
sources: a shared mailbox that receives maintenance notices, deprecations and incident emails;
the vendors' public status pages; and their API changelog feeds. Each announcement becomes a
CHANGE node with an actor kind of **vendor**, a kind of maintenance, deprecation or vendor
incident, and — this is the new thing — a **valid time equal to the window the vendor announced**,
which is usually in the future while the observed time is now. It is attached to the THIRD_PARTY
node for that vendor, and through the graph's existing dependency edges to the services that
depend on it.

Email bodies are never stored. The feeder extracts the fields that matter — vendor, product,
kind, window, affected resources — and keeps a pointer to the message identifier. Nothing else
from the message survives.

**Why this priority**: This is the single largest coverage gain in the audit and the reason the
feature exists. It is also the first fact in the project's history whose valid time starts after
its observed time, so it is where the bitemporal model is either honoured or quietly bent.

**Independent Test**: Replay a recorded set of notices — one maintenance email, one status-page
incident, one changelog deprecation, one cancellation and one duplicate — and compare the change
nodes, their valid intervals, their targets and their corrections to the golden output. Then ask
the graph, as of the announced window, what is expected to change; and ask it, as of the instant
of a recorded incident, what changes intersect that instant. The missed maintenance window is on
the second list.

**Acceptance Scenarios**:

1. **Given** a maintenance notice read on 17 September announcing a window from 02:00 to 04:00 on
   2 October, **When** the feeder runs, **Then** a CHANGE node exists whose valid interval is
   `[02:00, 04:00)` on 2 October and whose observed interval opens on 17 September; a query as of
   valid time 2 October 03:00 as known on 18 September returns it; and a query as of valid time
   18 September does not.
2. **Given** that same change, **When** the ranked change list for an incident on 18 September is
   produced, **Then** the announced-but-not-yet-valid change is **not** a candidate cause, and it
   is excluded because its valid start is after the reference instant rather than because it
   scored low.
3. **Given** an incident at 02:10 on 2 October on a service that depends on that vendor, **When**
   the ranked change list is produced, **Then** the maintenance change is on it, attached through
   the vendor's THIRD_PARTY node, with its announcement date visible.
4. **Given** the notice is cancelled on 25 September, **When** the feeder reads the cancellation,
   **Then** the announcement's observed interval is closed and a new version records that it no
   longer stands; history is not overwritten; and a query as of observed time 20 September still
   reports that the window was expected.
5. **Given** the notice is rescheduled to 9 October, **When** the feeder reads the reschedule,
   **Then** the change is corrected — the superseded announced window and the new one are both
   recoverable by an observed-time query — and no second, independent change node is created for
   the same announcement.
6. **Given** the same maintenance window announced both by email and by the vendor's status page,
   **When** both are read, **Then** exactly one change node results, because both carry identity
   claims naming the vendor, the product and the window, and the published rules merge them —
   the feeder merges nothing itself.
7. **Given** a notice about a vendor or a product that is not on the configured allowlist,
   **When** the feeder runs, **Then** nothing is emitted, the drop is counted, and the reason is
   visible in the feeder's own operational telemetry rather than silent.
8. **Given** any notice from any source, **When** the resulting event is inspected, **Then** it
   contains no message body, no sender or recipient address, no display name and no free-text
   field copied verbatim from the message — only the extracted fields, a bounded derived summary,
   and a pointer to the message identifier.
9. **Given** an announced window that passes, **When** the vendor confirms completion, **Then**
   the change records the confirmation; **and given** the vendor says nothing, **Then** the change
   remains marked as announced and unconfirmed, and is never silently promoted to something that
   is known to have happened.

---

### User Story 3 - The engine runs the algebra against Cloud Logging and Cloud Monitoring (Priority: P1)

The investigation engine has a hypothesis: "the checkout service's error rate rose after the
14:20 traffic shift". It hands the backend **one operation of the published query algebra** —
`compare`, `new_log_patterns`, `error_spans` or `errors_by_version` — with a pointer and a window
around a reference instant. The backend executes it against Cloud Monitoring or Cloud Logging and
returns a **digest**: a bounded summary with a mandatory coverage block, join keys, drill-down
handles and its own evidence. Never a dump. Never a free-form query.

**Why this priority**: This is the first live telemetry backend in the project and the half of
this feature that feature 002 cannot fake. It is also where constitution IV is either honoured or
quietly broken.

**Independent Test**: Against recorded GCP responses, execute every operation of the algebra over
a window and compare each digest to its golden; then run the same requests live against the
organisation and check that the digests match, field for field including the whole coverage
block, for a window whose data has settled.

**Acceptance Scenarios**:

1. **Given** `compare` over a metric pointer and the thirty minutes either side of 14:20, **When**
   the engine executes it, **Then** it receives, per series, a bounded summary of each window and
   an explicit before/after comparison, the query exactly as sent, the execution instant and the
   platform's own request identifier — and no point-by-point samples.
2. **Given** `errors_by_version` over a window spanning a traffic shift, **When** the engine
   executes it, **Then** the answer is grouped by the Cloud Run revision label, so that "the new
   revision is erroring and the old one is not" is a fact the digest can state, and the revision
   values are join keys that match the WORKLOAD nodes in the graph.
3. **Given** `new_log_patterns` over a window containing 400,000 matching log entries, **When**
   the engine executes it, **Then** it receives counts grouped by mined pattern with variables
   masked, at most the published number of patterns, at most the published number of redacted
   exemplars per pattern and only when exemplars were asked for explicitly, and an explicit
   statement of what was truncated and by which criterion.
4. **Given** `error_spans` and an organisation with no tracing, **When** the engine executes it,
   **Then** the outcome is the typed `no_data` whose coverage block states that the trace data
   source is absent and that nothing was searched, so that the engine cannot read it as evidence
   that there were no error spans.
5. **Given** a window inside Cloud Logging's ingestion lag, **When** the engine executes an
   operation, **Then** the outcome is `not_yet_ingested`, distinct from `no_data` and from a
   failure, and the digest states the lag it observed.
6. **Given** a request that is not an operation of the algebra, or that carries caller-supplied
   query text, **When** it arrives, **Then** it is refused, naming the operation asked for and
   the operations available, and nothing is executed.
7. **Given** the same request executed against recorded responses and live over a settled window,
   **When** the two digests are compared, **Then** they are identical on every field including
   the whole coverage block and every join key, and nothing in either depends on the connection
   rather than on the response or on configuration.
8. **Given** any digest produced anywhere in the corpus, **When** the graph's own validation runs
   over it, **Then** no part of it has been written into the graph as a node, an edge, a property
   or an event.

---

### User Story 4 - Cloud Monitoring alerts, on the shared intake (Priority: P1)

The organisation's Cloud Monitoring alert policies are one of its two definitions of "something
is wrong" — the other is a human opening a severity channel, which feature 002 owns. The feeder
turns each in-scope alert policy into an ALERT node linked to what it watches, and each incident
opening or closing into an idempotent `alert.transition` event whose valid time is the transition
instant Google reports. Polling is the source of truth at a cadence of 15 to 30 seconds; a
Pub/Sub notification channel may be configured as a **doorbell** that only means "poll now".

**Why this priority**: Without it the ALERT node type has no GCP feeder behind it and half the
intake is missing. It is P1 rather than higher only because the audit showed most incidents in
this organisation are noticed by people first.

**Independent Test**: Replay the recorded alert policies and incident transitions of a real
incident window, ask for the alert by its policy identifier, and compare the alert, what it
watches, and its state history to the golden output — then deliver the same transition twice, by
both transports, and check the history is unchanged.

**Acceptance Scenarios**:

1. **Given** recorded alert policy definitions, **When** the feeder runs, **Then** each in-scope
   policy is an ALERT node identified by its policy identifier, carrying its display name, its
   conditions' filters as pointers in Cloud Monitoring's own vocabulary, the severity where one
   is stated, and a link back to the policy.
2. **Given** a policy whose condition names a Cloud Run service, a Cloud SQL instance or a label
   the graph knows, **When** the feeder runs, **Then** the alert is linked to those entities; and
   a policy whose targets cannot be resolved produces an alert that is explicitly unattached
   rather than dropped, and attaches automatically if a target later appears.
3. **Given** a recorded sequence of an incident opening at 02:07 and closing at 02:51, **When**
   the feeder runs, **Then** the alert's alerting state has valid interval `[02:07, 02:51)` with
   observed times reflecting the poll at which each was learned, a query as of 02:20 reports it
   alerting, the state history states that it is sampled at the poll interval, and the closure is
   emitted as its own transition and kept as evidence.
4. **Given** a policy that opens incidents per grouped resource, **When** two resources alert at
   different instants, **Then** each is its own alert with its own state history naming the policy
   it belongs to, and the policy itself is not reported as alerting because one group is.
5. **Given** the same transition delivered by a doorbell-triggered poll and by the next scheduled
   poll, **When** both are processed, **Then** exactly one `alert.transition` is in effect,
   because the idempotency key is the alert policy, the group and the transition instant as
   Google reports it, and the result does not depend on which transport arrived first.
6. **Given** a forged, replayed or malformed doorbell notification, **When** it arrives, **Then**
   its body is never read as data, it costs at most one extra poll inside the configured rate
   limit, and no alert, state version or event is created, altered or retracted as a result.
7. **Given** a human-declared incident arriving through feature 002's Slack transport for the same
   underlying alert, **When** both intakes have run, **Then** both produced `alert.transition`
   events under the same published convention and the engine sees one investigation trigger, not
   two competing shapes.

---

### User Story 5 - Cloud SQL, configuration and secret versions (Priority: P2)

Behind the Cloud Run services are Cloud SQL instances, and in front of the revisions are the
environment variables, configuration values and secret-version references they were deployed
with. The feeder turns Cloud SQL instances into INFRA_RESOURCE nodes with `depends-on` edges from
the services that use them, turns Cloud Run configuration into CONFIG nodes with configuration
changes, and turns instance setting and database-flag changes and Cloud SQL maintenance events
into change nodes.

Where the dependency can be derived — from a service's connection configuration, its Cloud SQL
instance connection name, or a label that names the instance — the edge is asserted. Where it
cannot, the feeder emits a **claim for a human to confirm** and never guesses an edge into
existence.

**Why this priority**: "Which database does this service talk to" is the second question after
"what changed", and a flag change or a maintenance event on the instance is a change the audit
corpus contains. It is P2 because the P1 stories are complete without it.

**Independent Test**: Replay recorded Cloud SQL and Cloud Run configuration payloads and compare
the instances, the derived dependency edges, the proposed dependencies awaiting confirmation, and
the configuration and maintenance changes to the golden output.

**Acceptance Scenarios**:

1. **Given** recorded Cloud SQL payloads for an in-scope project, **When** the feeder runs,
   **Then** each instance is an INFRA_RESOURCE node carrying its project, region, database engine
   and version, with an unknown valid start where GCP does not state when it became true.
2. **Given** a Cloud Run service whose configuration names a Cloud SQL instance connection,
   **When** the feeder runs, **Then** a `depends-on` edge exists from the service to that
   instance, with the evidence that derived it recorded.
3. **Given** a Cloud SQL instance that no service's configuration names, **When** the feeder runs,
   **Then** the instance exists with no invented edge, and any suggestive but insufficient
   evidence appears as a proposed dependency for a human to confirm or reject, never as an edge.
4. **Given** a database flag changed at 03:12, **When** the feeder runs, **Then** a configuration
   change node exists valid at 03:12 with the flag named, the old and new values where GCP states
   them, the principal and its actor kind, and a `changed-by` edge to the instance.
5. **Given** a Cloud SQL maintenance event, **When** the feeder runs, **Then** it is a change of
   the maintenance kind with an actor kind of vendor, and where the maintenance is *scheduled*
   rather than past its valid time is the announced window and it follows the future-dated
   semantics of User Story 2.
6. **Given** a Cloud Run revision deployed with a new value of an environment variable or a new
   secret **version reference**, **When** the feeder runs, **Then** a CONFIG node and a
   configuration change exist naming what changed, and **no secret material and no configuration
   value that the published rules classify as sensitive is stored** — only that it changed, and
   which version it moved to.

---

### User Story 6 - Cloud Audit Logs as the general change stream (Priority: P2)

Beyond deploys, the things that break production are IAM edits, quota changes, scaling
adjustments and deletions. The Cloud Audit Logs' admin-activity stream records all of them with a
principal and an instant. The feeder reads that stream for the in-scope projects and turns each
in-scope entry into a CHANGE node with a kind from the published taxonomy, the principal, an
actor kind, a link back to the log entry and `changed-by` edges to the resources it names.

**Why this priority**: It is the catch-all that stops the graph's answer to "what changed" being
"only the things we wrote a special case for". P2 because the deploy path — the highest-value
slice — is already covered by User Story 1.

**Independent Test**: Replay a recorded admin-activity window containing a known culprit among
decoys, run feature 001's ranked diff, and check the culprit ranks in the top three with a
rationale citing its time and hop distance.

**Acceptance Scenarios**:

1. **Given** a recorded admin-activity window, **When** the feeder runs, **Then** each in-scope
   entry becomes a CHANGE node with a kind from the published taxonomy, the entry's timestamp as
   its valid time, the principal as actor, an actor kind, a link back to the entry, and
   `changed-by` edges to every resource it names that the graph can resolve.
2. **Given** an entry whose operation has no equivalent in the published change taxonomy — an IAM
   policy edit, a quota change — **When** the feeder runs, **Then** it is emitted under the
   taxonomy's "other" kind with GCP's own operation name recorded, never silently dropped.
3. **Given** an entry naming a resource the graph does not know, **When** the feeder runs, **Then**
   the change is kept and marked unattached, and attaches automatically if the resource later
   appears.
4. **Given** an autoscaling adjustment made by a platform principal at 02:14, four minutes after
   symptom onset, **When** the feeder runs, **Then** its actor kind is `CONTROLLER`, so that feature
   002's causal ordering can treat it as a candidate *effect* rather than a candidate cause.
5. **Given** which log types, services and operations are in scope is configurable, **When** the
   feeder runs, **Then** the configuration in force is recorded in the checkpoint, so a later
   reader can tell "no change happened" from "we were not looking for that kind of change".
6. **Given** an admin-activity entry that arrives later than the instant it describes, **When** it
   is read, **Then** its valid time is the instant in the entry and its observed time is when the
   feeder learned of it; the feeder never substitutes one for the other and never reorders its
   own log to compensate.

---

### User Story 7 - The organisation's own payloads, sanitised, are the tests (Priority: P2)

Casey runs a recording campaign against the organisation's GCP projects and its vendor-notice
sources: a baseline window, an incident window with a real alert transition and the change that
preceded it, a window containing a quota-exhausted response, a window containing a poll that
failed part-way, and a set of real vendor notices including one cancellation and one duplicate.
Every recording is sanitised in the connector before anything touches disk, scanned by something
other than the sanitiser, canary-tested, signed off and committed to a **private** corpus. The
public repository carries **synthetic structural twins**.

**Why this priority**: Constitution VIII says a connector with synthetic-only test data cannot be
marked stable. P2 because the connector can be written and reviewed before the campaign runs —
but topology and notice recording must **start** before the campaign scope is settled, because
neither history can be reconstructed afterwards.

**Independent Test**: The committed fixtures replay from empty to their goldens, double-deliver
as no-ops, shuffle inside the declared reordering window without changing valid-time state, and
pass the independent secret and personal-data scan. Separately, the public repository's synthetic
twins replay to their own goldens with the private corpus absent.

**Acceptance Scenarios**:

1. **Given** a recorded campaign, **When** the fixtures are verified, **Then** each replays from
   empty to its golden graph, double delivery changes nothing, and shuffling inside the declared
   window changes no valid-time state.
2. **Given** a recording containing principal email addresses, project numbers, instance
   connection names, IP addresses or customer data in log samples, **When** it is sanitised,
   **Then** every people identifier is **dropped, not hashed**; every infrastructure identifier is
   replaced by a keyed-HMAC pseudonym that is the same everywhere it occurs in the corpus, so the
   digest join keys still join; and the independent scan passes.
3. **Given** a vendor-notice email that contains an API key, a password reset link or a customer
   name, **When** the feeder processes it, **Then** the body never reaches disk, a log, or any
   artifact — including on a failed or aborted run — and the extracted fields carry none of it.
4. **Given** a sanitised recording, **When** it is committed, **Then** it carries a signed
   manifest naming the sanitisation policy version, the content hash, who signed it off and when,
   and what was dropped; and where a second person exists, a second signature.
5. **Given** canary tokens seeded into the source data before a campaign, **When** the recording
   is committed, **Then** an independent secrets-and-entropy scan finds none of them, and a
   surviving canary **fails the commit** rather than raising a warning.
6. **Given** the recorded telemetry responses, **When** feature 002 replays an investigation,
   **Then** it gets identical digests with no call to GCP.
7. **Given** the public repository alone, **When** the conformance suite runs, **Then** it passes
   against the synthetic structural twins and no real production recording is present in it.

---

### User Story 8 - The integration stays inside Dana's quota (Priority: P2)

Dana needs to know what this costs before it runs and while it runs. The integration declares a
budget per endpoint class, plans its polling and its pointer executions to stay inside it, backs
off when GCP refuses for quota, degrades in a published order when the budget is short, and
reports what it used — always leaving a published reserve for the humans who are querying Cloud
Logging during the same incident.

**Why this priority**: An integration that exhausts the project's Logging read quota during an
incident makes the incident worse, and the damage lands on the humans sharing that quota. P2
because the P1 stories are testable at fixture scale before budgeting exists.

**Independent Test**: Replay a recorded run containing quota-exhausted responses and check that
the integration backs off as instructed, that its usage report matches the number of calls in the
recording exactly, and that a run configured under a small budget drops work in the published
order rather than at random.

**Acceptance Scenarios**:

1. **Given** a budget expressed as a share of the remaining quota per endpoint class, **When** a
   cycle is planned, **Then** the integration stays inside its share and defers what it cannot do
   in the published priority order, with the deferral reported.
2. **Given** that GCP reports remaining quota for some endpoints and not others, **When** the
   integration plans, **Then** it uses the reported figure where there is one and falls back to
   tracking its own consumption against the configured limit where there is not — and says in its
   usage report which figures were vendor-reported and which were self-tracked.
3. **Given** a quota-exhausted or throttled response, **When** it is received, **Then** the
   integration waits at least as long as the response asks where it says, does not lose the
   position it had reached, and records the event in its own operational telemetry.
4. **Given** an incident during which humans are querying Cloud Logging, **When** the integration
   plans work in that class, **Then** it reduces its own allowance, leaves the published reserve
   unspent, yields rather than competing, and says in its output that it stopped for quota rather
   than for lack of evidence.
5. **Given** a request whose window is wider than the published cap for its operation and cost
   class, **When** it is planned, **Then** it is narrowed to the cap or refused with the cap
   named, never issued as asked.
6. **Given** any run, **When** Dana asks what it used, **Then** she sees calls per GCP area, the
   share of the configured budget consumed, the share of remaining quota per endpoint class, how
   much of the human reserve was left untouched, and the feeders' and the telemetry backend's
   usage separately.

---

### User Story 9 - Load balancers and DNS, where they are cheap (Priority: P3)

The organisation's services are reached through load balancers and Cloud DNS records. Where the
mapping can be read cheaply, the feeder records how a service is exposed as an `exposed-via`
relation, and turns DNS record changes and load-balancer backend changes into change nodes of the
DNS-switch and configuration kinds.

**Why this priority**: A DNS or backend change is a real cause and a cheap one to represent, but
it is the rarest in the corpus and the easiest to add later. It is explicitly the first thing cut
if the budget is short.

**Independent Test**: Replay recorded load-balancer and DNS payloads and compare the exposure
relations and change nodes to the golden output.

**Acceptance Scenarios**:

1. **Given** a load balancer whose backend is an in-scope Cloud Run service, **When** the feeder
   runs, **Then** an `exposed-via` relation exists from the service to the load balancer entity,
   with the host names it serves recorded as properties.
2. **Given** a Cloud DNS record changed at 11:04, **When** the feeder runs, **Then** a change node
   of the DNS-switch kind exists valid at 11:04 with the record name, the old and new targets
   where GCP states them, the principal and its actor kind.
3. **Given** a load-balancer backend swapped from one service to another, **When** the feeder
   runs, **Then** a change node exists and the old `exposed-via` relation is retracted at the
   instant of the swap rather than deleted.
4. **Given** that reading either surface would exceed the configured budget or requires a
   permission the credential does not have, **When** the feeder runs, **Then** it reads neither,
   says so in its checkpoint as a scope statement, and every other story is unaffected.

---

### Edge Cases

- **A revision created with 0% traffic.** Creating a revision that serves nothing did not change
  what production does. Both the creation and the (absent) traffic shift must be representable:
  the creation is a rollout marked as not serving, and it must not be ranked as if it had moved
  traffic. A revision that later receives traffic produces its own traffic-shift change at that
  instant — the creation change is not retroactively reinterpreted.
- **A traffic split rolled back.** Traffic moved to a new revision at 14:20 and back to the old
  one at 14:41. Both are rollout changes with their own valid instants; the second is not a
  deletion of the first, and the service's split property has three versions in valid time. A
  query as of 14:30 must report the new revision serving.
- **A notice that is cancelled or rescheduled.** The announcement was a fact about the future
  that is no longer expected to come true. This is a **correction, not a retraction**: the
  observed interval of the announcement closes and a new observation opens; the valid interval is
  not rewritten to pretend the announcement never named that window. "What did we believe on 20
  September about 2 October?" must still be answerable.
- **The same notice arriving by email and by status page.** One real-world announcement, two
  sources, and the feeder must not produce two changes. Both observations carry identity claims
  naming the vendor, the product and the announced window; the published rules merge them; the
  feeder merges nothing itself. Where no shared identifier exists the pair is raised as a
  resolution suggestion. The failure this guards against is two adjacent entries for one window in
  the ranked change list.
- **A notice for a product the organisation does not use.** Dropped by the vendor-and-product
  allowlist, counted, and visible as a drop in the feeder's operational telemetry — never emitted
  "just in case", because an unused product's maintenance window is noise that dilutes the one
  signal this feeder exists to carry.
- **An announced window that passes in silence.** The vendor never confirms. The change remains
  announced and unconfirmed for ever; it is never promoted to "happened", and a consumer that
  needs to know the difference can see it.
- **Cloud Audit Log delivery delay.** An entry describing 14:20 can arrive at 14:29. Valid time is
  the instant in the entry, observed time is when it was read, the reordering window is declared,
  and a poll that ends before a delayed entry arrives must declare its extent honestly rather than
  imply the window was complete. Changes learned after an investigation has run are exactly what
  feature 002's reopening path exists for.
- **The Logging read quota is exhausted mid-investigation.** The outcome is a typed failure naming
  quota, with the earliest retry instant where GCP states one, never a partial result presented as
  whole and never an empty answer that reads like "nothing happened".
- **A project with thousands of revisions.** Listing every revision of every service since the
  beginning of time is neither affordable nor useful. The feeder's history horizon is configurable
  and recorded in the checkpoint, revisions outside it are not fabricated, and a query that
  reaches past the horizon is told it has, so "no revision" is never confused with "we did not
  look that far back".
- **The sanitiser meets an email carrying credentials.** A maintenance email with an API key, a
  support thread with a customer name, a bounce message with a personal address. The body never
  reaches disk in any form. If the extracted fields themselves cannot be made safe, the notice is
  dropped from the recording and the drop is documented; a fixture is never committed on the
  promise of being cleaned later.
- **Two projects with the same service name.** `checkout` in the staging project and `checkout` in
  the production project are different entities. Identity carries project and region, and they are
  never merged on name alone.
- **A Cloud Run service deleted and recreated with the same name.** Not one continuous entity. The
  old one is retracted at the deletion instant recorded in the audit log, the new one is created,
  and any evidence linking them is a claim for the resolution layer — never a silent continuation.
- **The GKE cluster.** The one inference cluster is the Kubernetes feeder's, and this feeder must
  not create a second node for the same workload. It records the cluster as an infrastructure
  resource with identity claims so the two views resolve into one entity, and emits nothing below
  that level.
- **A write-capable service account.** The integration refuses to start and names exactly which
  permissions offend, in the same shape the Kubernetes feeder's refusal takes. Where a permission
  cannot be tested, it names the part it could not verify and requires the operator's assertion in
  configuration rather than assuming.
- **The doorbell subscription is itself a GCP object.** Acknowledging a notification changes state
  on a subscription. This is the one mutation the integration performs, it is confined to a
  dedicated subscription the operator creates for it, it is declared as a named exception in the
  read-only statement, and the integration must run correctly with the doorbell absent.
- **Clock skew between GCP and the graph.** Valid time comes from GCP's timestamps, observed time
  from the graph. Neither corrects the other; a disagreement beyond a stated threshold is reported
  in the integration's own operational telemetry so the skew is visible.
- **A recorded-mode request the world recording does not cover.** Answered `not_recorded` — never
  `no_data`, never a silent fall-through to a live call. The share of in-algebra requests that land
  here is the fixture's miss rate and is reported.
- **A request outside the published query algebra.** Refused, naming the operation asked for and
  the operations available. The backend is not a query proxy and never executes caller-supplied
  query text.
- **GCP changes an answer's shape.** A payload that no longer parses fails loudly against its
  fixture rather than silently producing fewer events, and the integration reports the rejection
  with the field that broke.

## Requirements *(mandatory)*

### Functional Requirements

#### A. One integration, two feeders, one backend, read-only credentials

- **FR-001**: The feature MUST consist of exactly three parts: a **GCP feeder** that emits typed
  graph events from the platform; a **vendor-notice feeder** that emits typed graph events from
  provider announcements; and a **GCP telemetry backend** that executes pointer queries and returns
  digests. The telemetry backend MUST NOT emit any graph event and MUST NOT write anything into
  the graph.
- **FR-002**: The two feeders MUST be distinct sources with distinct source identifiers,
  credentials, cadences, budgets and checkpoints. They MUST NOT share a credential, and a failure
  or a scope change in one MUST NOT affect the other's history.
- **FR-003**: The GCP feeder and the telemetry backend MUST use one **read-only service account**
  and MUST declare the exact read-only roles they require per GCP area they read (Cloud Run, Cloud
  SQL, Cloud Audit Logs, Cloud Monitoring, Cloud Logging, Cloud Trace where present, load
  balancing and Cloud DNS where in scope). Viewer-class and log/monitoring-viewer-class roles are
  the ceiling; no role granting any write MUST be requested.
- **FR-004**: At startup the integration MUST ask GCP what its principal is permitted to do — by
  testing the permissions it cares about, or by listing the effective roles bound to it — and MUST
  refuse to start if the principal holds any write, create, update, delete or deploy permission in
  an area it reads, naming the offending permissions. Where a permission cannot be tested, it MUST
  name the part it could not verify and MUST require the operator to assert read-only in
  configuration; it MUST NOT assume.
- **FR-005**: The integration MUST NOT call any GCP operation that changes state in the
  organisation's projects — including deploying a revision, shifting traffic, editing a service,
  instance, flag, policy, record or IAM binding, creating a log sink, a saved query, a dashboard, a
  BigQuery dataset or a notification channel — and this MUST be verifiable by inspecting the set of
  operations the integration is able to issue, not only its behaviour on one run.
- **FR-006**: Exactly one exception to FR-005 is permitted and MUST be declared as such: where an
  operator configures a **Pub/Sub doorbell** (FR-050), the integration acknowledges messages on a
  **dedicated subscription created by the operator for this purpose**, which changes that
  subscription's state and nothing else. The integration MUST run correctly with the doorbell
  absent, and MUST NOT create, delete or reconfigure the subscription, the topic or the
  notification channel.
- **FR-007**: The vendor-notice feeder MUST use read-only access to the shared mailbox and MUST
  NOT send, reply to, forward, delete, move, archive, label or change the read state of any
  message. Where the access mechanism would alter message state as a side effect of reading, the
  feeder MUST use a mode that does not, or MUST NOT read at all.
- **FR-008**: Every part MUST be usable in two modes with identical output: live against GCP and
  the notice sources, and offline against recorded payloads and recorded responses. The recorded
  mode is the test. Nothing in any event or any digest may derive from the connection itself — only
  from a payload or from configuration.
- **FR-009**: The set of GCP projects and regions the feeder reads MUST be configurable, and the
  scope in force MUST be recorded in the checkpoints, so a query can tell "not present" from "not
  in scope". The scope MUST be changeable without losing history, because recording starts before
  the campaign scope is finally agreed (FR-130).
- **FR-010**: Each GCP project MUST be a distinct scope within the GCP feeder's source, and
  identity MUST carry the project, so two projects that use the same service or instance name are
  never silently merged.
- **FR-011**: The integration MUST emit its own operational telemetry: calls made per GCP area,
  quota refusals, poll duration and lag, events emitted and rejected, notices read, extracted and
  dropped with the reason, digests produced, digest truncations, and observed clock skew between
  GCP timestamps and graph observed time.
- **FR-012**: A poll that fails part-way MUST produce a checkpoint stating the extent actually
  covered and declaring the gap, and MUST emit no retraction for the part it did not read.

#### B. Cloud Run topology, revisions and rollouts

- **FR-013**: The feeder MUST create a SERVICE node for every Cloud Run service in an in-scope
  project and region, carrying at least the service name, the project, the region and the
  environment derived from the project and its labels by a published, configurable mapping.
- **FR-014**: The feeder MUST create a WORKLOAD node for every Cloud Run revision it reports,
  carrying at least the revision name, the container image reference and the service it belongs
  to, related to its service by the published relation for "this runs that".
- **FR-015**: The traffic split across revisions MUST be a property of the service version — a set
  of revision names with their percentages — and MUST be versioned in valid time, so that a query
  as of any instant reports the split that was in force then. It MUST NOT be expressed as a
  measurement and MUST NOT be derived from observed traffic.
- **FR-016**: The feeder MUST emit a ROLLOUT change node when a revision is **created**, valid at
  the instant GCP states the revision was created, with a `changed-by` edge to the service and to
  the revision, stating explicitly that the creation did not by itself move production traffic.
- **FR-017**: The feeder MUST emit a separate ROLLOUT change node when the **traffic split
  changes**, valid at the instant the new split took effect, recording the split before and after
  and a `changed-by` edge to the service. This is the change whose valid time is the instant what
  production served changed, and the two rollout kinds MUST be distinguishable by a published
  property.
- **FR-018**: A revision that holds 0% of traffic MUST still exist as a node and MUST still have
  produced its creation change; it MUST NOT be presented as serving, and its creation change MUST
  NOT be reinterpreted when traffic later moves to it — the later movement is its own change.
- **FR-019**: Every change this feeder emits MUST carry an **actor** — the principal GCP records
  for the operation — and an **actor kind** from feature 001's published set (ADR-0005 D1), whose
  minimum membership is `PERSON` ("a person"), `AUTOMATION` ("automation acting on a person's
  behalf" — a CI or deployment principal), `CONTROLLER` ("the platform acting on its own" —
  autoscaling, service agents, scheduled platform operations), `VENDOR` ("the vendor") and
  `UNKNOWN`.
- **FR-020**: The actor kind MUST be derived from the principal's class — established from a
  configured classification of principals and from what GCP states about the caller — and MUST NOT
  be inferred from the principal string alone. Where it cannot be established, the kind MUST be
  `unknown` with the evidence that was available recorded.
- **FR-021**: The feeder MUST emit identity claims for every identifier GCP knows about a service
  or revision — the fully qualified resource name, the service name, the revision name, the
  container image reference and digest, the configured OpenTelemetry service name where the
  service declares one, and the environment and team labels on the allowlist — and MUST merge
  nothing itself.
- **FR-022**: Where GCP does not state since when a fact has been true, the feeder MUST mark the
  valid start as unknown. It MUST NOT substitute the poll instant.
- **FR-023**: A service or revision the feeder has seen and that GCP no longer lists for a
  configured number of consecutive complete polls MUST be retracted with a valid end inside the
  last poll it was observed in, never deleted. A deletion recorded in the audit log MUST retract at
  the instant the audit log states instead.
- **FR-024**: The feeder MUST have a configurable **history horizon** for revisions and MUST record
  it in the checkpoint, so that a query reaching past it is told so rather than being answered
  "there was no revision".
- **FR-025**: A service deleted and later recreated under the same name MUST NOT be presented as
  one continuous entity. The old entity is retracted and the new one created; any evidence linking
  them is emitted as claims for the resolution layer to decide in the open.
- **FR-026**: The feeder MUST emit a source checkpoint on start, on resync and after any gap,
  stating the extent it covered and whether it was watching immediately before that extent.

#### C. Cloud SQL, configuration and infrastructure dependencies

- **FR-027**: The feeder MUST create an INFRA_RESOURCE node for every Cloud SQL instance in an
  in-scope project, carrying at least the instance name, project, region, database engine and
  version, with an unknown valid start where GCP does not state one.
- **FR-028**: Where a Cloud Run service's configuration, connection settings or labels name a Cloud
  SQL instance, the feeder MUST assert a `depends-on` edge from the service to the instance and
  MUST record the evidence that derived it.
- **FR-029**: Where the dependency cannot be derived from configuration, the feeder MUST emit a
  **proposed dependency** for a human to confirm or reject, carrying the suggestive evidence and
  its rationale. It MUST NOT assert an edge on suggestive evidence, and MUST NOT drop the
  suggestion silently.
- **FR-030**: The feeder MUST NOT create nodes for GKE workloads, pods, containers or any object
  the feature 001 Kubernetes feeder owns. It MAY record the cluster itself as an infrastructure
  resource with identity claims so that the two views resolve into one entity, and MUST emit
  nothing below that level.
- **FR-031**: The feeder MUST emit a change node for a Cloud SQL instance settings or database
  flag change, valid at the instant the audit log states, naming what changed and its old and new
  values where GCP states them, with actor and actor kind and a `changed-by` edge to the instance.
- **FR-032**: The feeder MUST emit a change node of the **cloud maintenance** kind with actor kind
  `vendor` for a Cloud SQL maintenance event. Where the maintenance is **scheduled and has not yet
  occurred**, its valid interval MUST be the announced window and the future-dated semantics of
  section G MUST apply unchanged.
- **FR-033**: The feeder MUST create a CONFIG node for the configuration a Cloud Run revision was
  deployed with — environment variable names and value fingerprints, mounted configuration
  references, and secret **version references** — and MUST emit a configuration change when any of
  them changes between revisions, with a `changed-by` edge to the service and the revision.
- **FR-034**: The feeder MUST NOT store secret material, and MUST NOT store the value of any
  configuration entry that the published rules classify as sensitive. It MUST record only that the
  entry changed, and the version reference it moved to.
- **FR-035**: The feeder MUST emit identity claims for the instance connection name and every other
  identifier another source could also know about an instance, so that a Cloud SQL instance seen by
  two sources resolves into one entity by the published rules.
- **FR-036**: A change whose target the graph cannot resolve MUST be kept and marked unattached,
  and MUST attach automatically if the target later appears.

#### D. Cloud Audit Logs as the general change stream

- **FR-037**: The feeder MUST read the Cloud Audit Logs' **admin activity** stream for the in-scope
  projects and MUST create a CHANGE node for every in-scope entry that describes a change to the
  production system, carrying a kind from the published taxonomy, the entry's timestamp as valid
  time, a human-readable summary, the principal as actor, an actor kind, a reference back to the
  entry, and `changed-by` edges to every resource it names that the graph can resolve.
- **FR-038**: The feeder MUST cover at minimum, where present in the stream: IAM policy changes,
  quota changes, scaling and capacity changes, resource deletions, and service-enablement changes.
- **FR-039**: An audit-log operation with no equivalent in the published change taxonomy MUST be
  emitted under the taxonomy's "other" kind with GCP's own operation name recorded as a property.
  It MUST NOT be dropped.
- **FR-040**: Which log types, GCP services and operations are treated as changes MUST be
  configurable, and the configuration in force MUST be recorded in the checkpoint, so a later
  reader can tell "no change happened" from "we were not looking for that kind of change".
- **FR-041**: The feeder MUST declare its reordering window for the audit-log stream and MUST
  tolerate delayed delivery within it by observed time, never by reordering its own log. An entry
  that arrives after the extent it belongs to has been checkpointed MUST still be emitted with the
  entry's own instant as valid time.
- **FR-042**: The feeder MUST NOT read the **data access** log stream in v1, and MUST request no
  permission for it, because its volume, cost and sensitivity are out of proportion to its value
  for change detection.
- **FR-043**: The feeder MUST emit, for every change it creates, every identifier it knows that
  another source could also know — the target resource name, the revision, the image digest, the
  pipeline or commit reference the entry carries — as identity claims, so that the same real-world
  change observed by two sources resolves into one change node by the published rules rather than
  by this feeder.
- **FR-044**: The feeder MUST NOT merge its change with another source's change itself, MUST NOT
  suppress its own observation because another source may have reported the same thing, and where
  no published rule can merge the pair the pair MUST appear as a resolution suggestion with its
  score and rationale.

#### E. Cloud Monitoring alerts and alert transitions

- **FR-045**: The feeder MUST create an ALERT node for every in-scope Cloud Monitoring alert
  policy, identified by its policy identifier, carrying at least the display name, the severity
  where one is stated, each condition's filter as a pointer in Cloud Monitoring's own vocabulary,
  and a link back to the policy.
- **FR-046**: An alert policy's transitions MUST be emitted as `alert.transition` events under the
  **same published event type or mapping, and the same idempotency-key convention, that feature
  002's intake uses**. The key MUST be the published 4-tuple **(source, stable alert identifier,
  group, transition instant)** (ADR-0005 D2, 002 FR-008b) — here: source `gcp`, the alert policy
  identifier as the stable alert identifier, the policy's group, and the transition instant as
  Google reports it. The valid time MUST be the transition instant, never the instant the feeder
  learned of it.
- **FR-047**: Human-declared incidents are **not** this feature's intake. Feature 002 owns the
  Slack severity-channel transport; this feature's sole obligation towards it is that GCP alert
  transitions and human declarations converge on the same event shape and the same key convention,
  so that the engine sees one trigger per incident.
- **FR-048**: The feeder MUST link an alert to the entities its conditions watch, derived from the
  conditions' filters and resource labels, using the graph's published relationship taxonomy. An
  alert whose targets cannot be resolved MUST be kept and marked unattached, and MUST attach
  automatically if a target later appears.
- **FR-049**: The alert's state MUST be versioned in valid time so that a query as of any instant
  reports the state the policy was in then, and a policy that opens incidents per grouped resource
  MUST produce one alert per alerting group, each with its own state history; the policy-level
  entity MUST NOT be reported as alerting because one of its groups is.
- **FR-050**: **Polling is the source of truth**, at a configurable interval whose default MUST lie
  between 15 and 30 seconds. A Pub/Sub notification channel MAY be configured as a **doorbell**: it
  MUST only enqueue "poll now", its body MUST never be parsed, trusted or stored, the endpoint or
  subscription MUST be authenticated and rate limited, and a forged, replayed or malformed
  notification MUST cost at most one extra poll and MUST never create, alter or retract an alert, a
  state version or an event. Running both transports MUST NOT produce two transitions and the
  result MUST NOT depend on their order.
- **FR-051**: Where the feeder learns of transitions by polling it MUST state in the alert's state
  history that the window is **sampled** at the poll interval, so a consumer can tell a complete
  history from a sampled one.
- **FR-052**: Intake MUST be filterable by label and by policy. Flapping and no-data transitions
  MUST NOT trigger an investigation, MUST still be recorded in the state history, and the
  suppression MUST be stated rather than applied silently. A closing transition MUST be emitted and
  is evidence; it MUST NOT be discarded as uninteresting.
- **FR-053**: The integration MUST be able to hand an alert to the investigation engine by policy
  identifier (and group where it has one), returning the alert, the entities it watches, the
  transition instant to use as the reference instant, and the pointers to execute. The answer MUST
  contain no telemetry payload.
- **FR-054**: The integration MUST NOT change any alert policy, notification channel, snooze or
  incident state (restating FR-005 for the area where the temptation is greatest).

#### F. Load balancers and DNS (lowest priority, cheapest first)

- **FR-055**: Where a load balancer's backend is an in-scope Cloud Run service and the mapping can
  be read within the configured budget, the feeder MUST record how the service is exposed as an
  `exposed-via` relation to the load-balancer entity, with the host names it serves as properties.
- **FR-056**: A Cloud DNS record change MUST become a change node of the **DNS switch** kind valid
  at the instant the audit log states, with the record name, the old and new targets where GCP
  states them, the actor and the actor kind; a load-balancer backend change MUST become a change of
  the configuration kind, and the superseded `exposed-via` relation MUST be retracted at the
  instant of the swap rather than deleted.
- **FR-057**: Where either surface is outside the budget or outside the credential's permissions,
  the feeder MUST read neither, MUST state the omission in its checkpoint as a scope statement, and
  every other requirement MUST hold unchanged.

#### G. The vendor-notice feeder, and facts about the future

- **FR-058**: The vendor-notice feeder MUST read provider announcements from three source kinds: a
  configured **shared mailbox**; configured **public status-page feeds**; and configured **API
  changelog feeds**. Each source MUST be independently enableable, and the feeder MUST work with
  any subset of them present.
- **FR-059**: The feeder MUST act on an explicit **allowlist of vendors and their products**.
  An announcement whose vendor or product is not on the allowlist MUST be dropped, counted, and
  visible as a drop with its reason in the feeder's operational telemetry; it MUST NOT be emitted.
- **FR-060**: Each in-scope announcement MUST become a CHANGE node with an **actor kind of
  `vendor`**, an actor naming the vendor, a link back to the announcement, and a kind of: **cloud
  maintenance** for a maintenance window; **deprecation** for an announced removal or
  breaking change; **vendor incident** for a provider-declared incident. Until the published
  taxonomy names the latter two, they MUST be emitted under the taxonomy's "other" kind with
  `deprecation` or `vendor_incident` recorded as the vendor-stated kind (Dependencies on feature
  001).
- **FR-061**: The change's targets MUST include the **THIRD_PARTY node for the vendor**. Where the
  announcement names specific affected resources — a region, an API endpoint, a model or product
  name — those MUST be emitted as identity claims so the resolution layer can attach them; the
  feeder MUST NOT invent a dependency edge from a service to a vendor, and MUST NOT duplicate a
  `depends-on` edge the graph already holds.
- **FR-062**: A change node's **valid interval MUST be the window the vendor announced**. Where
  that window begins after the instant the announcement was read, the event MUST be emitted with
  the announced valid start unchanged, and the graph MUST accept it: an **announced fact** is a
  fact whose valid time begins after its observed time, and no ingestion rule may reject one.
- **FR-063**: Observed time MUST remain the instant the graph accepted the observation and MUST
  NOT be moved forward to match an announced window. The asymmetry is one-directional: valid time
  may lead observed time; observed time is never in the future.
- **FR-064**: A read as of valid instant `T_v` and observed instant `T_o` MUST return an announced
  change when `T_v` falls inside the announced window and `T_o` is at or after the instant the
  announcement was observed. "What is expected to change on 2 October, as known today" MUST
  therefore be an ordinary bitemporal query and not a special case.
- **FR-065**: A change whose valid start is **after** the reference instant of a ranked change list
  MUST NOT be a candidate cause, and MUST be excluded for that stated reason rather than by scoring
  low. A forward-looking read MUST be an explicitly forward query with a future valid instant, and
  MUST NOT be reachable by accident from a backward-looking one.
- **FR-066**: Every announced change MUST carry a published **announcement state** distinguishing
  at minimum: announced, confirmed, cancelled and superseded. It MUST default to announced and MUST
  NOT be promoted to confirmed without an observation that says the window happened.
- **FR-067**: A **cancellation** MUST be recorded as a correction: the observed interval of the
  announcement closes and a new observation opens recording that it no longer stands. It MUST NOT
  rewrite the valid interval, so a query as of an earlier observed instant still reports what was
  believed then.
- **FR-068**: A **reschedule** MUST be recorded as a correction of the same change and MUST NOT
  create a second, independent change node. Both the superseded window and the new one MUST be
  recoverable by an observed-time query.
- **FR-069**: Where a vendor states a window only vaguely, the valid start MUST be marked unknown
  rather than guessed, and the vendor's own wording MUST NOT be stored in place of a timestamp.
- **FR-070**: The same announcement arriving from two sources MUST produce **one** change node. The
  feeder MUST emit identity claims naming the vendor, the product, the announced window and the
  vendor's own notice identifier where it states one, and MUST let the published rules merge them.
  The feeder MUST NOT merge, and MUST NOT suppress its own observation because another source may
  carry the same announcement.
- **FR-071**: **No message body may ever be stored**, in the graph, on disk, in a log or in any
  artifact, at any point, including on a failed or aborted run. The feeder MUST extract only typed
  fields — vendor, product, announcement kind, announced window, affected resources, the vendor's
  notice identifier — plus a bounded derived summary, and MUST keep a **pointer to the message
  identifier** so a human can open the original in the mailbox.
- **FR-072**: The feeder MUST NOT store sender or recipient addresses, display names, message
  subjects copied verbatim, quoted threads, or attachments. Attachments MUST NOT be opened or
  stored in v1. The derived summary MUST be built from the extracted fields and MUST be subject to
  the same redaction rules and the same scans as any other committed content.
- **FR-073**: Announcement text is **untrusted input from outside the organisation**. Extraction
  MUST produce values validated against the published typed schema, and no text from any
  announcement MUST ever be treated as an instruction by any component of the system. An extraction
  that cannot produce a valid typed result MUST fail loudly, naming the field, rather than emit a
  guess.
- **FR-074**: An announcement the feeder cannot confidently place — no recognisable vendor, no
  window, no product — MUST be recorded as unextracted with its reason and its message pointer, so
  a human can see what the feeder could not read. It MUST NOT be emitted as a change.
- **FR-075**: Status-page and changelog polling MUST run at a configurable cadence, MUST respect
  any cadence limit the publisher states, and MUST identify itself honestly. A source that is
  unreachable MUST produce a gap in the checkpoint, not silence.
- **FR-076**: Each announcement's event identifier MUST be deterministic from the vendor, the
  vendor's notice identifier (or, absent one, the source, the product and the announced window),
  so that re-reading the same announcement is a no-op and the same announcement read from two
  sources carries the evidence needed to merge.
- **FR-077**: The feeder MUST create a THIRD_PARTY node for every allowlisted vendor it has an
  announcement for, if one does not already exist, carrying the vendor's name and the products on
  the allowlist, and MUST emit identity claims — including the host names the allowlist maps to
  the vendor — so it resolves with third-party nodes other feeders created from observed outbound
  dependencies.
- **FR-078**: The feeder MUST report, per cycle, how many announcements it read, extracted,
  dropped by allowlist, failed to extract, and merged, so that "the graph knows about no upcoming
  vendor change" can be distinguished from "the feeder read nothing".

### Data mapping *(what GCP and vendor objects become)*

Observed time is assigned by the graph at acceptance in every row and is never in the future; the
columns below record what the feeder controls — the valid time it asserts, the cadence at which it
learns things, and what it says when it did not look. "Unknown start" means the feeder MUST mark
the valid start unknown rather than guess.

| Source object | Graph element | Identity and refs | Valid time | Learning cadence and gaps |
|---|---|---|---|---|
| Cloud Run service in an in-scope project and region | SERVICE node with project, region, environment and traffic-split properties, plus pointers | ref `gcp.cloudrun.service` = `<project>/<region>/<service>`; claims for the fully qualified resource name, the declared OpenTelemetry service name, environment and team labels | unknown start unless GCP states a creation instant; end at the audit-log deletion instant, else under the silence rule | service poll; a failed poll checkpoints a gap and retracts nothing |
| Cloud Run revision | WORKLOAD node related to its service, with image reference | ref `gcp.cloudrun.revision` = `<project>/<region>/<service>/<revision>`; claims for the image reference and digest | the revision creation instant GCP states | service and revision poll, bounded by the configured history horizon |
| Revision creation | CHANGE node, kind rollout, marked as not having moved traffic, with actor and actor kind | ref `gcp.change` keyed by the revision and the creation instant; claims for image digest, pipeline or commit reference where the audit entry carries one | the revision creation instant | revision poll corroborated by the audit log |
| Traffic split change | CHANGE node, kind rollout, marked as having moved traffic, recording the split before and after | ref `gcp.change` keyed by the service and the instant the split took effect | the instant the new split took effect | service poll; a split seen only after a gap is emitted with the instant GCP states and the gap stands in the checkpoint |
| Cloud Run service configuration, environment variables, secret **version references** | CONFIG node and a configuration change; values fingerprinted, sensitive values never stored | ref `gcp.config` keyed by service and configuration generation | the instant of the configuration revision | with the service poll and the audit log |
| Cloud SQL instance | INFRA_RESOURCE node with engine, version, project, region | ref `gcp.cloudsql.instance` = `<project>/<region>/<instance>`; claims for the instance connection name | unknown start; retracted under the silence rule or at the audit-log deletion instant | instance poll |
| Service → Cloud SQL dependency derived from configuration | `depends-on` edge with the deriving evidence recorded | source and target refs as above | as the service version that names it | with the service poll |
| Service → Cloud SQL dependency **not** derivable | a **proposed dependency** for a human to confirm — never an edge | — | — | raised once per cycle, not re-raised while pending |
| Cloud SQL settings or database flag change | CHANGE node, kind configuration change, with actor and actor kind | ref `gcp.change` = audit entry identifier | the audit entry instant | audit-log poll over a moving window |
| Cloud SQL maintenance event | CHANGE node, kind cloud maintenance, actor kind `vendor` | ref `gcp.change` keyed by instance and window | the maintenance window; **announced windows are future-dated** and follow section G | maintenance poll and audit log |
| Cloud Audit Log admin-activity entry describing a change | CHANGE node with a taxonomy kind (or "other" plus GCP's operation name), actor, actor kind, origin link, `changed-by` edges | ref `gcp.change` = the entry's unique identifier; claims for every shared identifier | the instant in the entry, never the read instant | audit-log poll over a moving window with a declared reordering window; a partial read checkpoints a gap |
| Cloud Audit Log **data access** stream | **nothing — not read in v1**, and no permission requested | — | — | — |
| Cloud Monitoring alert policy | ALERT node with its conditions' filters as pointers and a link to the policy | ref `gcp.monitoring.alert_policy` = the policy identifier | unknown start; retracted when the policy is deleted or leaves scope | policy poll |
| Cloud Monitoring incident transition | `alert.transition` event, projected as a new version of the alert's state | same ref; a grouped policy uses policy identifier and group; idempotency key (policy, group, transition instant) | **the transition instant Google reports** | poll every 15–30 s, optionally woken by a Pub/Sub doorbell; a polled history is marked sampled |
| Load balancer serving an in-scope service | `exposed-via` relation to the load-balancer entity, host names as properties | ref `gcp.lb` = the fully qualified resource name | unknown start; retracted at the audit-log instant of a backend swap | load-balancer poll, lowest priority, omitted under budget with the omission stated |
| Cloud DNS record change | CHANGE node, kind DNS switch, with actor and actor kind | ref `gcp.change` = audit entry identifier | the audit entry instant | audit-log poll |
| GKE cluster (the one inference cluster) | **the cluster as an infrastructure resource with identity claims only**; every workload, pod and container belongs to the feature 001 Kubernetes feeder | ref `gcp.gke.cluster`; claims naming the cluster as the Kubernetes feeder knows it | unknown start | cluster poll, once per cycle |
| Allowlisted vendor | THIRD_PARTY node with its allowlisted products | ref `vendor` = the vendor slug; claims for the host names the allowlist maps to it | unknown start | created on first announcement, or already present from an observed dependency |
| Vendor maintenance notice | CHANGE node, kind cloud maintenance, actor kind `vendor`, announcement state | ref `vendor.notice` = `<vendor>/<notice identifier>`; claims for vendor, product, window | **the announced window — valid start may be in the future of observed time** | mailbox, status-page and changelog polls; the same notice from two sources merges by claim |
| Vendor deprecation notice | CHANGE node, kind `deprecation` (taxonomy "other" until the taxonomy names it), actor kind `vendor` | as above | the announced effective date, normally in the future; unknown start where the vendor is vague | as above |
| Vendor status-page incident | CHANGE node, kind `vendor_incident` (taxonomy "other" until the taxonomy names it), actor kind `vendor` | as above | the incident window the vendor states, closed when the vendor closes it | status-page poll |
| A cancelled or rescheduled notice | **a correction of the existing change**, never a new change and never a rewritten valid interval | same ref | the new announced window, with the superseded one recoverable by observed-time query | on the next read of that source |
| Announcement message body, sender, recipients, subject, attachments | **nothing — never stored anywhere**; only a pointer to the message identifier | — | — | — |
| Metric samples, log entries, spans, and any aggregate of them | **nothing — refused at ingestion** | — | — | retrieved at query time by the telemetry backend, returned as a digest, never stored |

#### H. Pointers in GCP's vocabulary

- **FR-079**: Every node the feeders create MUST carry pointers. For a Cloud Run service these MUST
  include at least: metric pointers for the published request-rate, error-rate and latency signals;
  a log pointer; a link back to the service in the GCP console; and a trace pointer where a trace
  data source is present. For an alert these MUST include each condition's filter and a link to the
  policy. For a Cloud SQL instance these MUST include at least one metric pointer and a link back.
- **FR-080**: Pointer selectors MUST be written in the GCP query vocabulary that will actually be
  executed — the Cloud Logging filter vocabulary for log pointers, the Cloud Monitoring query
  vocabulary for metric pointers, the trace filter vocabulary for trace pointers. Each vocabulary
  MUST be named and versioned as a published vocabulary, and the published documentation MUST state
  why these selectors cannot be expressed in OpenTelemetry semantic conventions (constitution IV).
- **FR-081**: The attributes that identify *which entity* a pointer is about MUST be expressed in
  OpenTelemetry semantic conventions, so a reader or a future backend can recognise the entity
  without understanding GCP's grammar.
- **FR-082**: A pointer MUST name the backend family and the query vocabulary only. It MUST NOT
  embed a credential, and MUST NOT carry an account or organisation identifier beyond the project
  identifier the selector itself requires; where the pointer is a link back into the GCP console
  the link MUST carry no credential.
- **FR-083**: Pointers MUST be versioned in valid time with the node they belong to, so asking for
  a service's pointers as of an earlier instant returns the selectors that were true then.
- **FR-084**: A GCP pointer MUST be additive: it MUST NOT replace or suppress pointers other
  sources have attached to the same entity.
- **FR-085**: Where the organisation has no trace data source, trace pointers MUST NOT be minted as
  if one existed. The absence MUST be a stated fact about the entity, so that a consumer can tell
  "no trace pointer because there is no tracing" from "nobody wrote one".

#### I. The telemetry backend contract

- **FR-086**: The telemetry backend MUST accept a request naming a pointer (or a node reference and
  a pointer kind), a reference instant and a window specification, and MUST return a digest or an
  explicit typed failure. Every request MUST be one operation of the published query algebra.
- **FR-087**: The backend MUST serve a **small, published, typed query algebra** and MUST serve
  nothing outside it. **The algebra is published in one place and this specification does not
  restate it**: `specs/002-investigation-engine/contracts/telemetry-backend.md` §1 and
  `investigation.proto` §1–3 (ADR-0005 D7). From that document this backend MUST serve the **whole
  telemetry family — all eight terms**: `compare`, `onset`, `new_log_patterns`, `error_spans`,
  `errors_by_version`, `monitor_state`, `exemplars` and `drill_down`. It MUST NOT serve the graph
  family (typed wrappers over feature 001's query RPCs, answered from the replayed event log and
  never recorded into a world) or the knowledge family. Every request MUST be one named operation
  with typed arguments. **Free-form query text supplied by a caller MUST NOT be executed**, and an
  operation outside the algebra MUST be refused, naming what was asked for and what is available.
  The algebra is versioned with the digest contract, and it is simultaneously the replay boundary,
  the vendor abstraction, the sanitisation point and the prompt-injection barrier.
- **FR-087a**: `onset(pointer, search_window, method)` MUST be served **backend-side**: the Cloud
  Monitoring query returns the series **to the backend process**, which runs the published
  change-point method there and returns only the onset digest — the estimated instant, its
  uncertainty, the method and its parameters. The raw samples MUST NOT cross the digest boundary in
  either direction, and MUST NOT appear in any response, exemplar or coverage field.
- **FR-087b**: Where a term's data source is absent in this organisation, the backend MUST answer
  the typed outcome `no_data` with a coverage block **naming the absent source** and naming what was
  in fact searched — never `query_failed`, never an unexplained empty digest, never a substituted
  source. The answer MUST be identical in live and recorded mode and stable for the whole window.
  `error_spans` with no trace data source is the case this organisation actually has (FR-091);
  `monitor_state`, `exemplars` and `drill_down` follow the same rule wherever their source is
  missing.
- **FR-088**: `compare` and `errors_by_version` MUST be executed against Cloud Monitoring;
  `new_log_patterns` against Cloud Logging; `error_spans` against the trace data source where one
  is present. Which GCP surface serves which operation MUST be published, so a reader knows what
  was queried.
- **FR-089**: `errors_by_version` MUST group by the **Cloud Run revision** so that "the new
  revision is failing and the old one is not" is a fact the digest can state, and the revision
  values it returns MUST be join keys matching the workload nodes in the graph.
- **FR-090**: `new_log_patterns` MUST return log content only as **mined templates with their
  variable parts masked**, with counts per template and the first-seen instant of each, never as
  raw entries.
- **FR-091**: Where the organisation has **no trace data source**, `error_spans` MUST return the
  typed outcome `no_data` with a coverage block stating that the trace data source is **absent** and
  that nothing was searched. A consumer MUST NOT be able to read that answer as evidence that there
  were no error spans.
- **FR-092**: The window specification MUST support at least an explicit interval and a
  before/after pair around the reference instant with a caller-supplied duration. When a
  before/after pair is asked for, the digest MUST include an explicit comparison of the two windows,
  not two unrelated summaries.
- **FR-093**: The request MUST record the reference instant it was asked about and the instant at
  which it was executed. Observed-time pinning has no meaning against a live telemetry backend,
  which has no memory of what it used to say; a reproducible investigation MUST therefore use
  recorded responses rather than a re-execution.
- **FR-094**: A metric digest MUST consist of, per series: the series' identifying labels, the
  number of points, the interval actually covered, the resolution GCP returned, summary statistics
  from a published set, and — for a before/after request — the direction and magnitude of the
  change. It MUST NOT contain the point-by-point samples.
- **FR-095**: A log digest MUST consist of counts grouped by a published grouping (at least
  severity and mined template), the top templates by count up to a published limit, and at most a
  published number of exemplars per template.
- **FR-096**: The backend MUST be able to return **bounded, sanitised exemplars** — one example
  line per template, one stack trace per error kind — but MUST do so **only when the caller asks
  for them explicitly**, never by default. Exemplars MUST be redacted by the same rules as any other
  sample, MUST be capped by the published limits, and MUST count towards the digest's size bound.
- **FR-097**: A span digest MUST consist of counts and latency summary statistics grouped by
  operation and error kind, the top error kinds up to a published limit, and identifiers that let a
  human open the traces concerned. It MUST NOT contain span payloads.
- **FR-098**: An alert-state digest MUST consist of the transitions in the window with their
  instants, the state at the start and end of the window, the per-group states where the policy has
  groups, and the number of transitions.
- **FR-099**: Every digest MUST be bounded by published limits: maximum series, maximum groups or
  templates, maximum exemplars per group, maximum length per exemplar, and a maximum total size.
  Every truncation MUST be stated explicitly, naming what was dropped and the criterion used to
  choose what was kept.
- **FR-100**: Every digest MUST carry its evidence: the exact query as sent, the pointer and node it
  came from, the requested window and reference instant, the execution instant, the identifier GCP
  returns for the request where it returns one, and the connector and vocabulary versions.
- **FR-101**: Every digest MUST carry a **coverage** block, and a digest without one MUST be
  invalid. Coverage MUST state at minimum: what was searched (which entities, which GCP surface and
  which log or metric scope, and the window actually covered); the data volume considered; any
  sampling or alignment GCP applied or the backend requested; what was truncated and by which
  criterion; and the backend's **ingestion lag** at the instant of execution. A coverage field that
  cannot be determined MUST be stated as undetermined, never omitted.
- **FR-102**: Every digest MUST preserve the **join keys** that let its answer be correlated with
  the graph and with other digests: at minimum the Cloud Run **revision**, the **instance
  identifier**, the **trace identifier** where one exists, and the **first-seen instant** of each
  template, error kind or series. Sanitisation MUST pseudonymise these consistently rather than
  drop them — a digest whose keys do not join is evidence about nothing.
- **FR-103**: Every digest MUST carry **drill-down handles**: for each group, template, series or
  error kind it reports, a handle the caller can present back to the backend for the narrower
  answer, and a deep link that opens the same query over the same window in the GCP console for a
  human. A handle MUST be a reference, never an embedded payload.
- **FR-104**: The backend MUST distinguish, as separate and explicitly named **typed outcomes**: a
  digest; **`no_data`** (the query was valid and matched nothing in the window); **`query_failed`**
  (with a reason from a published set — rejected by GCP with GCP's reason, not permitted because
  the credential lacks the role, quota exhausted with the earliest retry instant where GCP states
  one, or timed out); **`not_yet_ingested`** (the window falls inside the ingestion lag, so nothing
  may be concluded from an empty answer); and **partial** (some of the answer was retrieved, with
  what is missing named). `no_data` MUST NOT be reported as unknown, MUST NOT be reported as an
  error, and MUST NOT be reported for a window that is merely too recent.
- **FR-105**: In recorded mode the backend MUST add the outcome **`not_recorded`** for an in-algebra
  request the recording does not cover. It MUST be distinct from `no_data` and from `query_failed`,
  and the backend MUST NOT fall through to a live call to satisfy it.
- **FR-106**: The backend MUST run in a recorded mode that answers from recorded GCP responses with
  **identical digests** to live mode for the same request, and MUST make no network call in that
  mode. Identity MUST include the whole coverage block and every join key, not only the summary
  statistics. Recorded responses MUST be addressable by the request that produced them.
- **FR-107**: A recorded window MUST be a **world recording**, not a trajectory: it MUST cover the
  cross product of the query algebra over the alert neighbourhood and the window, not only the
  queries one investigation happened to ask. A non-deterministic consumer asking a different but
  in-algebra question of the same window MUST still be answerable without a live call.
- **FR-108**: The **miss rate** — the share of in-algebra requests over a recorded window answered
  `not_recorded` — MUST be measured and reported as a fixture-quality metric on every verification
  run, against a published threshold.
- **FR-109**: No digest, and no part of one, may be written into the graph as a node, an edge, a
  property or an event. Digests are transient results handed to the caller; where the caller keeps
  one, it keeps it in its own investigation record.
- **FR-110**: The backend MUST redact by rule before a sample leaves the process: at minimum email
  addresses, credentials and tokens, and the identifiers named by the sanitisation contract.
  Redaction MUST apply in live mode as well as when recording, so a live investigation cannot
  surface what a recording would not be allowed to keep. People identifiers MUST be **dropped
  rather than hashed** in live mode too, and log content MUST be reduced to masked templates before
  it leaves the process.
- **FR-111**: The backend MUST accept the pointers the feeders emit without translation. A pointer
  it cannot execute MUST be reported as unsupported, naming the pointer kind and vocabulary, rather
  than executed as something else.
- **FR-112**: Executing a pointer MUST NOT change anything in GCP and MUST NOT create a GCP object
  of any kind — no saved query, no log sink, no dashboard, no alerting policy, no analytics dataset
  or table.
- **FR-113**: Every execution MUST draw on the same budget as the feeder half and MUST be reportable
  separately, so Dana can see what investigations cost as distinct from what ingestion costs.
- **FR-114**: The digest shapes, their published limits, the failure outcomes, the coverage block,
  the join keys and the evidence fields are a **published contract, versioned like the schema**,
  because the investigation engine and its recorded fixtures depend on them.

#### J. Identity, resolution, labels and ownership

- **FR-115**: Both feeders MUST mint identity claims and MUST NOT merge anything themselves. Every
  identifier GCP or a vendor knows about an entity — including the one the feeder addresses it by —
  MUST be a claim with the supporting attributes a rule needs (at minimum the project, the region
  and the environment, and the Kubernetes cluster and namespace where labels state them).
- **FR-116**: The feeders MUST introduce and publish their identifier namespaces, and MUST NOT emit
  a reference in a namespace they have not declared. At minimum: a Cloud Run service namespace and
  a Cloud Run revision namespace whose values carry **project and region**; a Cloud SQL instance
  namespace; a configuration namespace; a change namespace; a Cloud Monitoring alert-policy
  namespace; a GKE cluster namespace; a vendor namespace; and a vendor-notice namespace.
- **FR-117**: A **certain** rule MUST be published for the case where an observed service's own
  attributes name the Cloud Run revision it is running in: the instrumentation is running inside
  that revision, so the observed service and the revision's Cloud Run service are one entity.
- **FR-118**: A **certain** rule MUST be published for the case where a Cloud Run service declares
  an OpenTelemetry service name in a configured environment variable or label, that name is claimed
  by an observed service, and both sides state the **same environment**. This is certain because
  somebody configured it to be true. A missing environment on either side MUST NOT satisfy it.
- **FR-119**: A **certain** rule MUST be published for the case where the vendor allowlist maps a
  vendor to one or more host names and an observed outbound dependency's server address matches
  one: the vendor the notices are about and the third party the graph already observed are one
  entity. This rests on a configured assertion, not on a resemblance.
- **FR-120**: A **certain** rule MUST be published for the case where two sources claim the same
  Cloud SQL instance connection name.
- **FR-121**: **Probable** rules MUST cover the suggestive cases — normalised name equality without
  an agreed environment; an instance name appearing inside a service's environment variable name; a
  shared owning label with a similar name — and MUST only produce suggestions, never automated
  merges.
- **FR-122**: Disagreement MUST surface, never resolve silently. Two projects, two regions or two
  environments that produce the same name MUST remain separate entities; a pair a human has
  rejected MUST NOT be re-merged; a human decision MUST persist across replay and take precedence
  over any automated match.
- **FR-123**: Every merge involving a GCP or vendor claim MUST be explainable by the graph's audit
  query: the claims, the rule, the score, the rationale and any human decision, in observed order.
- **FR-124**: The GCP labels the feeder reads MUST be governed by an explicit **allowlist**. A label
  key not on the allowlist MUST NOT become a property, a node or a claim.
- **FR-125**: Team and owner labels on the allowlist MUST become OWNER nodes with `owned-by` edges
  from the entities they own. The valid start is a **bound**: the earliest instant at which the
  labelled state of the owned entity is known to have held, which for a Cloud Run service is its
  `createTime`. The bound MUST be stated rather than left unknown, and the node MUST record that it
  *is* a bound — the instant the team came to own the entity is not a fact a label carries, and a
  reader must not mistake the bound for it. Where two label keys name the same owner differently,
  both claims MUST be kept and the resolution layer MUST decide in the open.

  *(Revised 2026-09-21. The original wording required an unknown valid start, which is
  unsatisfiable alongside the graph's placeholder rule: `markPlaceholder` gives an edge-minted
  endpoint the **edge's** `valid_from_unknown`, so that an entity does not read differently
  according to whether an edge or its endpoint arrived first — and an `owned-by` edge therefore has
  to agree with both of its endpoints. A Cloud Run service has a documented `createTime`, so its
  start is known; requiring the owner's to be unknown left one endpoint disagreeing with the edge
  whichever flag it carried, which the shuffle check exposes. Surfaced by US1's fixtures; the
  decision is to take the bound and say it is one.)*
- **FR-126**: Label values MUST be treated as identifiers and names, never as measurements. A label
  whose value is a number that measures something MUST NOT become a property.

#### K. Recordings, sanitisation and evaluation

- **FR-127**: The test corpus MUST be **recorded real payloads** from the organisation's GCP
  projects and its vendor-notice sources, sanitised, committed as fixtures with their resulting
  event streams and golden query outputs. Synthetic-only data is insufficient for either feeder or
  the backend to be marked stable.
- **FR-128**: Real production recordings MUST NOT be committed to the **public** repository. The
  sanitised corpus MUST live in a **private repository with private CI**, and the public repository
  MUST carry **synthetic structural twins**: the same event shapes, the same sequences, the same
  digest, coverage and failure contract, with no identifier derived from the organisation. The
  public conformance suite MUST run against the twins and the private corpus MUST run the same
  suite.
- **FR-129**: The campaign MUST cover at minimum: a baseline window over the in-scope projects; an
  incident window containing a real alert transition and the change that preceded it; a window
  containing a quota-exhausted response; a window containing a poll that failed part-way; **a set
  of real vendor notices including at least one future-dated maintenance window, one deprecation,
  one provider incident, one cancellation, one reschedule and one duplicate delivered by two
  sources**; and at least one window whose correct answer is that nothing is implicated.
- **FR-130**: Topology and notice recording MUST begin as soon as the read-only credential and
  mailbox access exist, **before** the campaign scope is finally agreed, because neither topology
  history nor an announcement stream can be reconstructed retrospectively. The scope in force MUST
  be recorded in the checkpoint throughout, so a later reader can tell "not present" from "not in
  scope at that time".
- **FR-131**: The first campaign's scope MUST be recorded explicitly in the campaign record. The
  first campaign's scope is **the production project, all regions** (owner's decision, session
  2026-09-21; the project name is configuration, see below). Scope beyond that first campaign is
  **operator configuration**, not a published constant: `config/gcp.yaml` carries the project and
  region lists, every checkpoint records the scope in force (FR-009), and **no code may assume a
  project name.**
- **FR-132**: The organisation MUST provide read-only access to the mailbox that receives vendor
  notices, and the access path MUST be recorded in the campaign record together with who authorised
  it. A shared engineering mailbox is the owner's intended source (session 2026-09-21). Whether that
  address is a Workspace mailbox or a Google Group is **unconfirmed and decides the access path**: a
  Group's archive is readable by no API, in which case the feeder reads a **subscribed member's**
  mailbox instead. Who authorises the read remains an owner action.
- **FR-132a**: The feeder MUST support **per-user delivery as a first-class path**, not as a
  fallback. Google Cloud addresses platform notices to the per-project **Essential Contacts** and to
  billing and owner principals, so a notice stream generally arrives in individuals' mailboxes and
  reaches a shared address only where someone has configured it to. A design that reads only a shared
  mailbox would therefore miss the notices this feature exists to capture.
- **FR-132b**: The feeder MUST NOT hard-code any mailbox address, domain or sender. The address, the
  access path and the sender allowlist are configuration. A campaign whose configured mailbox yields
  no notices MUST **fail loudly at campaign start** rather than record an empty notice stream, since
  an empty stream is indistinguishable from a working feeder in a quiet week.
- **FR-133**: Recorded **telemetry backend responses** MUST be committed alongside, addressable by
  the request that produced them, so the investigation engine can replay an investigation with no
  call to GCP and get identical digests.
- **FR-134**: A **sanitisation contract** MUST be published as part of this feature and MUST state,
  per field and per label key, exactly one of: recorded verbatim (on an allowlist), replaced by a
  keyed pseudonym, or dropped. It MUST cover at minimum principal email addresses, project numbers
  and identifiers, instance connection names, host names, IP addresses, resource names, customer
  identifiers appearing in log samples, secret and configuration values, and every free-text field.
  Its version MUST be recorded in every fixture manifest.
- **FR-135**: **People identifiers MUST be dropped, never hashed** — a hashed principal address is
  still personal data and MUST NOT be treated as anonymised. **Infrastructure identifiers** —
  project, service, revision, instance, cluster, namespace, host, team, environment — MUST be
  pseudonymised with a **keyed HMAC**, **consistently across a whole corpus**, so that
  relationships and the digest join keys survive sanitisation. Neither the key nor any mapping
  table MUST be committed.
- **FR-136**: **Free-text fields and message bodies MUST be dropped** unless a published rule says
  otherwise, and log entries MUST be stored as **mined templates with their variables masked**,
  never as raw lines. Where an exemplar is kept at all it MUST be redacted by rule and capped in
  number and length by the same published limits the live digest uses, so a recording can never
  contain more than a live answer would have shown.
- **FR-137**: Sanitisation MUST happen **in the connector, before anything touches disk**. No
  unsanitised GCP payload and no announcement body may be written to disk, to a log or to any
  artifact at any point, including during a failed or aborted run.
- **FR-138**: Every committed artifact MUST pass an **independent secrets-and-entropy scan** and a
  personal-data scan at commit, implemented separately from the sanitiser so that one defect cannot
  both leak and pass.
- **FR-139**: **Canary tokens** MUST be seeded into the source data before a campaign and MUST be
  asserted absent from every committed artifact. A surviving canary MUST fail the commit rather
  than raise a warning.
- **FR-140**: Every committed recording MUST carry a **signed manifest** stating the sanitisation
  policy version, the content hash of what was signed, the named individual who signed it off, the
  date and what was dropped; and, where a second person exists, a second signature. A payload that
  cannot be made safe MUST be dropped from the recording and the drop MUST be documented.
- **FR-141**: The same sanitisation boundary MUST govern what leaves the process towards a **model
  provider**: nothing may be sent to a model that would not be permitted into a recording under the
  same contract and policy version.
- **FR-142**: Every fixture MUST satisfy the published conformance suite: replay from empty to its
  golden graph, double delivery as a no-op, and shuffling within the declared reordering window
  leaving valid-time state unchanged. At least one fixture MUST contain an event that must always
  be refused, so the rejection path is exercised.
- **FR-143**: A **live parity check** MUST be documented as in feature 001's live run: the graph
  built live against the organisation MUST equal the graph built from the recording of the same
  run, and a digest computed live over a settled window MUST equal the digest computed from the
  recorded response for the same request.
- **FR-144**: Coverage MUST be measurable against GCP's own answer: the set of Cloud Run services
  the feeder produced for a project compared with the set GCP lists, the set of revisions compared
  with GCP's revision list inside the history horizon, and the set of alert policies compared with
  GCP's, with the differences enumerated rather than summarised.
- **FR-145**: After this feature, the **coverage audit MUST be re-run** by its published method over
  the same incident corpus, and its result MUST be published with per-incident attribution: for
  each incident, whether the true cause was a node or change in the graph at alert time, which
  feeder would have supplied it, and — where it was not — whether the correct engine answer is
  `unobserved` or `not_change_induced`.

#### L. Quota, budget and operability

- **FR-146**: The integration MUST have a configurable call budget and MUST plan each polling cycle
  and each pointer execution to stay inside it.
- **FR-147**: Budgets MUST be **per endpoint class**, not global, because GCP's quotas are per API
  and per project — the Cloud Logging read class and the Cloud Monitoring query class are the
  binding ones. Those quotas are **shared with the humans working the incident**: the integration
  MUST leave a published reserve unspent and MUST reduce its own allowance as the remaining quota
  falls.
- **FR-148**: The budget MUST be expressed as a **share of the remaining quota** per endpoint class.
  Where GCP reports remaining quota for an endpoint the integration MUST use the reported figure;
  where it does not, the integration MUST track its own consumption against the configured limit and
  MUST state in its usage report which figures were vendor-reported and which were self-tracked.
- **FR-149**: When the budget cannot cover a full cycle, the integration MUST defer work in a
  **published** priority order — load balancers and DNS first, then the general audit stream, before
  anything on which the P1 stories depend — MUST report what it deferred and why, and MUST reflect
  the reduced coverage in its checkpoints. Stopping for quota MUST be reported as a typed reason,
  distinct from stopping for lack of evidence.
- **FR-150**: The **width of a query window** MUST be capped per operation and cost class. A request
  wider than its cap MUST be narrowed to the cap or refused with the cap named; it MUST NOT be
  issued as asked.
- **FR-151**: The integration MUST honour GCP's quota and throttling responses, waiting at least as
  long as the response asks where it says so, and MUST resume from where it stopped rather than
  restarting the cycle.
- **FR-152**: The integration MUST report its usage: calls per GCP area, share of the configured
  budget consumed, share of remaining quota per endpoint class, how much of the published human
  reserve was left untouched, quota refusals received, and the feeders' and the telemetry backend's
  usage separately.
- **FR-153**: The integration MUST report observed clock skew between GCP's timestamps and the
  graph's observed time, and MUST NOT correct one with the other.
- **FR-154**: The integration MUST document what it costs to run against an estate of a stated size:
  calls per hour per area at the default cadence, and the GCP-side products and volumes it reads, so
  Dana can approve it before it runs.

### Key Entities

- **Cloud Run service**: a deployable service in one project and region. Becomes a SERVICE node;
  identified by project, region and name; carries environment, traffic split, labels, ownership and
  pointers.
- **Cloud Run revision**: one immutable version of a service's code and configuration. Becomes a
  WORKLOAD node; the unit `errors_by_version` groups by and a digest join key.
- **Traffic split**: the percentages of traffic across a service's revisions at an instant. A
  versioned property of the service, never a measurement.
- **Rollout change**: either the creation of a revision or a movement of the traffic split. Two
  distinguishable rollout changes with different valid instants and different meanings.
- **Actor kind**: one member of feature 001's published set (ADR-0005 D1) — `PERSON` ("a person"),
  `AUTOMATION` ("automation acting on a person's behalf"), `CONTROLLER` ("the platform acting on its
  own"), `VENDOR` ("a vendor") or `UNKNOWN` ("something the feeder cannot type").
- **Cloud SQL instance**: a managed database instance. Becomes an INFRA_RESOURCE node with
  `depends-on` edges from the services whose configuration names it.
- **Proposed dependency**: suggestive but insufficient evidence that a service depends on an
  infrastructure resource, raised for a human to confirm or reject. Never an edge.
- **Cloud Run configuration**: the environment variables, configuration references and secret
  version references a revision was deployed with. Becomes a CONFIG node; values are fingerprinted
  and sensitive values are never stored.
- **Audit-log change**: an admin-activity entry describing a change to the production system.
  Becomes a CHANGE node with a taxonomy kind, an actor, an actor kind, an origin link and targets.
- **Alert policy**: a Cloud Monitoring definition of "something is wrong". Becomes an ALERT node;
  its incident transitions become versions of its state in valid time.
- **Alert transition**: one alert state change, keyed on policy, group and transition instant. The
  unit of intake, whichever transport delivered it, and shared with feature 002's human-declared
  intake.
- **Vendor**: an allowlisted third-party provider with its allowlisted products. Becomes a
  THIRD_PARTY node, resolved with the third parties observed dependencies already produced.
- **Vendor notice**: one provider announcement — maintenance, deprecation or incident. Becomes a
  CHANGE node with an actor kind of vendor and an announced window as its valid interval.
- **Announced fact**: a fact whose valid time begins after its observed time. The vendor notice is
  the first one in the project, and its semantics — acceptance, query, ranking exclusion,
  correction — are stated in section G.
- **Announcement state**: announced, confirmed, cancelled or superseded. An announcement is never
  silently promoted to something known to have happened.
- **Query algebra operation**: one named, typed question the telemetry backend will answer, and the
  only kind of thing it will answer. The term list, its three families and which of them a recorded
  world holds live in one place — `specs/002-investigation-engine/contracts/telemetry-backend.md`
  §1 — and are **not** restated here; this backend serves that document's telemetry family in full
  (all eight terms).
- **Digest**: the bounded, structured answer to one algebra operation over one window, with its
  coverage, join keys, drill-down handles, evidence and explicit truncations. Never stored in the
  graph.
- **Coverage**: the mandatory part of a digest stating what was searched, over how much data, with
  what sampling, what was truncated and by which criterion, and how far behind ingestion was.
- **Join keys**: the revision, the instance identifier, the trace identifier and each series' or
  template's first-seen instant — the fields that let a digest be correlated with the graph and with
  other digests, pseudonymised consistently rather than dropped.
- **World recording**: the cross product of the query algebra over one alert neighbourhood and one
  window, together with its miss rate.
- **Recording campaign**: a set of recorded windows and notices, their sanitisation record, the
  sign-off, and what was dropped.
- **Call budget**: the share of remaining quota per endpoint class the integration may spend, its
  published deferral order, its window-width caps, and its usage report.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every in-scope project and region, at least 95% of the Cloud Run services GCP
  lists appear as service nodes within one polling interval, and every difference between the two
  sets is enumerated with a reason.
- **SC-002**: For every traffic shift in the recorded corpus, a rollout change exists whose valid
  time equals the instant GCP states, exactly, in 100% of cases; and zero revisions created with no
  traffic are presented as having moved traffic.
- **SC-003**: Actor kind is stated as something other than unknown for at least 90% of the changes
  in the recorded corpus that GCP attributes to a principal; 100% of the remainder carry the
  evidence that was available; and zero actor kinds are derived from the principal string alone.
- **SC-004**: For every Cloud Monitoring transition in the recorded corpus the alert's valid time
  equals the transition instant Google reported, exactly, in 100% of cases; the delay from
  transition to observed time is at or below the poll interval plus one minute at the 95th
  percentile with polling alone, or one minute at the 95th percentile when a doorbell is configured;
  and where both transports deliver, exactly one `alert.transition` exists per transition in 100% of
  cases.
- **SC-005**: A grouped alert policy with N alerting groups produces exactly N alerts and zero
  policy-level alerts caused by a group alerting, across the recorded corpus.
- **SC-006**: For 100% of the vendor notices in the corpus, the resulting change's valid interval
  equals the window the vendor announced and its observed interval opens at the read instant; and a
  query as of a valid instant inside that window, observed after the read, returns it in 100% of
  cases.
- **SC-007**: Zero changes whose valid start is after a ranked list's reference instant appear as
  candidate causes anywhere in the corpus, and 100% of such exclusions state that reason rather than
  a low score.
- **SC-008**: 100% of cancellations and reschedules in the corpus are recorded as corrections: the
  prior belief remains recoverable by an observed-time query in every case, and zero valid intervals
  are rewritten.
- **SC-009**: An announcement delivered by two sources produces exactly one change node in 100% of
  the duplicate fixtures, and zero duplicate pairs are merged by a feeder rather than by a published
  rule.
- **SC-010**: Zero message bodies, sender or recipient addresses, display names, subjects copied
  verbatim, or attachments reach the graph, disk, a log or any artifact, over the whole corpus
  including aborted runs.
- **SC-011**: At least 99% of pointer executions over the recorded corpus and over a live
  verification run succeed or return an explicit typed `no_data`; every remaining case carries a
  named failure outcome with its evidence; and zero return an unexplained empty answer.
- **SC-012**: 100% of digests are within the published size bounds, carry a coverage block and carry
  their join keys; and 100% of truncated digests state what was dropped and by which criterion.
- **SC-013**: Zero telemetry payloads reach the graph: over the whole recorded corpus, 100% of
  emitted events pass the telemetry-payload validation, the telemetry backend emits zero graph
  events, and the rejection path is exercised by at least one fixture.
- **SC-014**: Digests answered from the recorded corpus equal the live digests for the same request
  over a settled window on **every field including the whole coverage block** and on every join key,
  for 100% of the parity set; and the world-recording miss rate is at or below the published
  threshold and is reported on every verification run.
- **SC-015**: 100% of telemetry backend requests, over the recorded corpus and the live verification
  run, are one operation of the published query algebra; zero free-form queries are executed; and
  every out-of-algebra request is refused naming both the operation asked for and the operations
  available.
- **SC-016**: The graph built live against the organisation equals the graph built from the
  recording of the same run, byte for byte, as in feature 001's live-run parity check.
- **SC-017**: Every shipped fixture replays from empty to its goldens, double-delivers as a no-op,
  and shuffles within the declared reordering window without changing valid-time state, on every
  verification run, with zero tolerance.
- **SC-018**: **Canary survival is zero.** Of the canary tokens seeded into the source data before
  each campaign, 100% are absent from every committed artifact, and a surviving canary fails the
  commit rather than raising a warning.
- **SC-019**: 100% of committed artifacts pass an independent secrets-and-entropy scan and a
  personal-data scan run separately from the sanitiser; zero people identifiers appear in any
  committed artifact in any form, **hashed forms included**; and zero unsanitised payloads reach
  disk at any point, verified over the full corpus including aborted runs.
- **SC-020**: Zero write operations are issued against the organisation's GCP resources: every
  request the integration is capable of issuing is on a published read-only list, verified from its
  own recorded request log over the full corpus and the live run. The only state change anywhere is
  the acknowledgement of messages on the operator's dedicated doorbell subscription, and it is
  declared in the read-only statement.
- **SC-021**: Zero automated merges result from probable rules; 100% of automated merges involving a
  GCP or vendor claim are explainable by the audit query; and for entities present in both GCP and
  another source with an agreed environment, at least 95% merge automatically under a certain rule.
- **SC-022**: A full polling cycle over the in-scope estate costs no more than the documented number
  of calls per hour per GCP area, never exceeds its configured share of remaining quota for any
  endpoint class, and leaves the published human reserve unspent in 100% of cycles; usage is visible
  at all times and matches the number of calls in the recording to the exact call.
- **SC-023**: **The coverage audit, re-run by its published method over the same thirteen-incident
  corpus after this feature, shows the true cause was a node or change in the graph at alert time
  for at least 8 of the 13 incidents** — the ADR-0004 figure — with per-incident attribution
  published naming which feeder supplied the cause, and with every remaining incident classified as
  `unobserved` or `not_change_induced` with its reason.
- **SC-024**: For at least one recorded incident whose true cause was a missed vendor notice, the
  notice's change node appears in the top three of the ranked change list at the alert's reference
  instant — the pattern this feature exists to defeat, demonstrated rather than asserted.
- **SC-025**: Sam, starting from an alert policy identifier or a human declaration alone, obtains
  the alert, what it watches, what changed around it in the preceding window including any announced
  vendor window that intersects it, who owns it, and executable pointers, in one command and under
  thirty seconds on the recorded corpus.

## Assumptions

- Feature 002, the investigation engine, is specified and implemented first against recorded data.
  This feature supplies it with the first live telemetry backend and the first platform feeder; the
  query algebra and digest contract in section I are the seam between the two and are versioned
  together.
- The organisation's platform is as the coverage audit describes it: core applications on Cloud Run
  and Vercel, Cloud SQL, Hasura and Mongo behind them, one GKE cluster for inference, no
  distributed tracing anywhere, and incidents declared by opening a dated severity channel in
  Slack.
- **No trace data source exists.** `error_spans` is therefore specified to answer `no_data` with a
  coverage block stating the source is absent (FR-091), and to become a live operation without a
  contract change if Cloud Trace is later enabled. The feature does not depend on tracing existing,
  and no requirement here assumes it.
- Vercel and GitHub Actions deploys are feature 004. This feature covers the GCP side of a deploy —
  the revision and the traffic shift — and emits the pipeline and commit identifiers the audit
  entries carry as claims, so that feature 004's changes can resolve with these rather than
  duplicate them.
- The vendor allowlist is configuration with a published default drawn from the organisation's
  actual providers: an LLM API provider, a GPU host, a voice-infrastructure vendor, a
  serverless-GPU vendor and the ATS vendors. Adding a vendor is configuration, not a code change.
- The shared mailbox is an ordinary mailbox that receives provider mail, not a purpose-built feed.
  Most of what arrives in it is not a notice, so the allowlist and the extraction failure path
  (FR-074) carry real traffic and are not edge cases.
- Announcement text is untrusted input from outside the organisation and is treated as such
  throughout (FR-073). The typed extraction boundary is the same boundary that governs what may be
  sent to a model provider.
- GCP does not report remaining quota as uniformly as some vendors do. Where it does not, the
  integration self-tracks and says so (FR-148); the reserve for humans is honoured either way.
- Cloud Run revision names are stable and unique within a service, and the revision label is
  present on the metrics `errors_by_version` groups by. Where a metric lacks it, the operation
  reports reduced coverage rather than grouping by something else.
- Environment is derived from the project and its labels by a published, configurable mapping,
  because this organisation does not carry a uniform environment label. The mapping in force is
  recorded in the checkpoint.
- Feature 001's connector contract, event schema, resolution rules, pointer schema and fixture
  format are used unchanged. Anything this feature needs that they lack is a schema change with its
  own decision record, not a local extension; the list is **Dependencies on feature 001**.
- The corpus is split in two. The sanitised real recordings live in a private repository with
  private CI; the public repository carries synthetic structural twins. Any claim made in the public
  repository is a claim about the twins, and a property that can only be demonstrated on real
  payloads is labelled as such rather than implied.
- The coverage audit's method is published and repeatable, and the same thirteen-incident corpus is
  still available to re-run it against. SC-023 depends on that; if the corpus grows, the target is
  restated as the same **62%** proportion rather than the absolute count.
- The sanitisation contract, the digest limits, the label and vendor allowlists, the poll cadences,
  the history horizon and the call budget are configuration with published defaults, not constants,
  and the configuration in force is recorded in checkpoints and fixture manifests.

## Dependencies

- **Feature 001**: the event schema, the feeder contract, the published resolution rules, the
  pointer schema, the fixture format and the conformance suite — plus the schema changes listed
  below.
- **Feature 002**: the consumer of the query algebra, the digest contract and the alert intake,
  specified in parallel. The algebra, the digest shapes, the coverage block, the join keys and the
  failure outcomes must be agreed between the two and versioned together. Feature 002 owns the
  human-declared incident transport; this feature owns only the convergence of GCP transitions on
  the same event and key (FR-046, FR-047).
- **Feature 005 (Datadog)**: shares the actor-kind set, the `alert.transition` convention, the
  digest and coverage contract and the sanitisation contract. Where the two specifications ask for
  the same schema change, it is one change agreed once, not two.
- **The organisation**: a read-only GCP service account provisioned by Dana; read-only access to
  the vendor-notice mailbox with a named owner; a private repository and private CI to hold the
  corpus; the signing key and signatories for the sanitisation manifests; and the incident corpus
  and audit method needed to re-run the coverage audit (SC-023).

## Dependencies on feature 001

Everything below is a **schema or contract change in feature 001**, not a local extension here
(constitution IX). Each is stated as what this feature needs and what it does until it exists.

- **Change kinds for vendor announcements.** The published change taxonomy names rollout, IaC
  apply, flag flip, secret rotation, scaling, migration, DNS switch, cloud maintenance,
  configuration change and "other". It has no **`deprecation`** and no **`vendor_incident`**, and
  those two are the second and third most common shapes this feature emits. Until the taxonomy
  gains them, both are emitted as `CHANGE_KIND_OTHER` with the vendor-stated kind recorded, which
  the schema already supports — but "other" is not a kind a ranker or a consumer can reason about,
  so the taxonomy addition is the right answer. The audit stream raises the same question for
  `iam_change` and `quota_change`.
- **An actor kind on a change, with a member for automation acting on a person's behalf.** A change
  carries an actor as a free-text name and nothing that says whether the actor was a person, a
  deploy pipeline, an autoscaler or a vendor (FR-019, FR-020). Feature 005 proposes the set human /
  system / vendor / unknown. This feature needs one more distinction — a CI or deployment principal
  is not a person and is not an autoscaler, and conflating it with either loses the exoneration
  feature 002's causal ordering depends on. Either the set gains a member for automation acting on
  a person's behalf, or a published **principal class** field sits beside the actor kind. It is one
  decision for 003 and 005 together.
- **Future-dated valid time: the semantics, not the shape.** The shape already allows it — the
  interval type carries free timestamps with unknown-start and unknown-end flags, and ingestion
  validation requires only that a valid start be stated or explicitly marked unknown, so nothing
  rejects a valid start later than the observed start today. What is **not** published is the
  meaning, and this feature is the first that depends on it (FR-062–FR-068):
  1. a published statement that an **announced fact** — valid time beginning after observed time —
     is legal, and that observed time is never in the future;
  2. the as-of read contract for a **future valid instant**, so "what is expected to be true on 2
     October, as known today" is an ordinary query;
  3. an explicit rule in the ranker that a change whose valid start is **after** the reference
     instant is **excluded as a candidate cause with that stated reason** — today the candidate
     rule turns on interval intersection, which happens to exclude it, but an accident is not a
     contract and the temporal term's floor at zero would otherwise make a future change look
     maximally recent;
  4. a published **announcement state** vocabulary (announced, confirmed, cancelled, superseded)
     so that an announcement is never silently read as an occurrence;
  5. the confirmation that a **cancellation is a correction, not a retraction** — it closes the
     observation, not the valid interval.
- **An `alert.transition` event type, or its published mapping, with the idempotency-key
  convention.** The ingestion contract's bodies are upserts, retractions, change observations,
  identity claims, merge decisions and checkpoints; there is no typed alert transition (FR-046).
  Either feature 001 gains one, or it publishes the mapping onto the existing events **together
  with** the key convention — because two transports for one transition, and two intakes (GCP and
  human-declared) for one incident, are exactly what that key exists to defeat.
- **A "watches" relation.** The published edge taxonomy has calls, depends-on, runs-on,
  deployed-by, owned-by, exposed-via and changed-by. None of them means "this alert watches this
  entity", which is the most-used edge in this feature (FR-048). Until the taxonomy gains one, the
  feeder uses the closest published relation and records the substitution; it does not invent an
  edge type.
- **A published alert-state vocabulary and a "sampled" marker on a state history.** This feature
  needs the state values published and needs a published way to say a history is *sampled at an
  interval* rather than complete (FR-051); without it a consumer cannot tell a quiet policy from an
  unwatched one.
- **A proposed-dependency convention.** Resolution today suggests that two *entities* are one. It
  has no way to suggest that an *edge* exists — that this service probably depends on that instance
  — and this feature needs one for every Cloud SQL dependency it cannot derive from configuration
  (FR-029). Without it the only options are inventing an edge or losing the evidence, and both are
  wrong.
- **A published convention for "unattached", and for automatic attachment.** Alerts and changes
  whose targets the graph does not yet know are kept and attached when the target appears (FR-036,
  FR-048). The marker and the attachment rule belong to feature 001, not to each feeder inventing
  its own.
- **A join-key vocabulary.** The digest contract requires join keys that survive sanitisation
  (FR-102). The names and meanings of the keys this feature emits — the **revision**, the
  **instance identifier**, the **trace identifier**, the **first-seen instant** — must be published
  once and shared with features 002 and 005, so that a digest from one backend joins a digest from
  another.
- **Registry entries for the pointer vocabularies.** A pointer's vocabulary is a free string. The
  Cloud Logging filter vocabulary, the Cloud Monitoring query vocabulary and the trace filter
  vocabulary each need a registered, versioned name and the published statement of why their
  selectors cannot be expressed in OpenTelemetry semantic conventions (constitution IV).
- **A published home for the digest, coverage and query-algebra contract.** The pointer schema says
  where to look; nothing published says what an executed pointer answers with. The algebra, the
  digest shapes, the coverage block, the join keys, the drill-down handles and the typed outcomes
  (including `not_recorded`) are a versioned public contract shared with features 002 and 005 and
  need a published home.
- **Nothing else.** The node taxonomy (service, workload, infra resource, config, third party,
  owner, alert, change), the bitemporal model, the identity-claim and resolution events, the
  unknown-valid-start marker and the checkpoint's gap declaration are sufficient as published.

## Out of scope for this feature

- **Any write to the organisation's GCP resources**, of any kind: deploying a revision, shifting
  traffic, editing a service, an instance, a flag, a policy, a record or an IAM binding, creating a
  log sink, a saved query, a dashboard, a notification channel or an analytics dataset. There is no
  configuration that enables one. The single declared exception is acknowledging messages on the
  operator's own doorbell subscription (FR-006).
- **Any write to the vendor-notice sources**: no sending, replying, forwarding, deleting, moving,
  labelling or marking read.
- **GKE workloads, pods and containers.** The one inference cluster belongs to the feature 001
  Kubernetes feeder. This feature records the cluster as an infrastructure resource with identity
  claims and nothing below it.
- **Datadog.** Feature 005, rescoped by ADR-0004 D3.
- **GitHub Actions and Vercel deploys.** Feature 004, ADR-0004 D2. This feature emits the pipeline
  and commit identifiers GCP's audit entries carry as claims so those changes resolve with these,
  and does nothing else towards them.
- **The Slack severity-channel intake.** ADR-0004 D4 places it in feature 002. This feature is
  responsible only for GCP transitions using the same event and key convention.
- **GCP products this feature does not read**: BigQuery, Pub/Sub as a data source, Dataflow, Spanner,
  Firestore, Memorystore, Cloud Storage, Cloud Functions, App Engine, Cloud Build as a change
  source, Artifact Registry, Cloud Armor, Security Command Center, Recommender, billing and cost.
  Reading them is a later increment, not a variant of this one.
- **The Cloud Audit Logs data-access stream.** Not read, no permission requested (FR-042).
- **Storing any metric, log entry, span or aggregate of them in the graph.**
- **Storing any announcement body, address, subject or attachment anywhere.**
- **Any telemetry query outside the published algebra.** The backend is not a general query proxy
  and does not execute caller-supplied query text; extending the algebra is a contract change.
- **A doorbell as a source of truth.** The Pub/Sub notification path triggers a poll and nothing
  else; there is no configuration that makes a notification body authoritative.
- **Real production recordings in the public repository.** They live in the private corpus with
  private CI; the public repository gets synthetic structural twins.
- **Infrastructure-as-code state as a source.** Terraform or equivalent plan and apply records are
  a change source this feature does not read; GCP's audit log records the effect, which is what this
  feature represents.
- **Any language-model reasoning.** This feature supplies structure to the investigation engine; it
  does not reason. Symptom-onset estimation, causal ordering, the hypothesis ledger and the grading
  of a non-deterministic investigator are feature 002's.
- **Autonomous remediation**, proposal or otherwise.
