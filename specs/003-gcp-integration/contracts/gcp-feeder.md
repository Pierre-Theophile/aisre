# The GCP Feeder Contract

**Source id**: `gcp:<org-slug>` | **Kind**: `gcp` | **Feature**: 003
**Schema version**: `1.0.0` | **Ordering**: `none` | **Date**: 2026-09-20

A `pkg/feeder.Feeder` over the platform the organisation actually runs on. It reads Cloud Run,
Cloud SQL, Cloud Run configuration, the Cloud Audit Logs admin-activity stream, Cloud Monitoring
alert policies and their transitions, the GKE cluster's metadata, and — where they fit the budget —
load balancers and Cloud DNS. It writes nothing anywhere.

Entity shapes, refs, valid-time rules and the two state machines are
[`data-model.md`](../data-model.md); vendor-API facts are [`research.md`](../research.md); quota is
[`budget.md`](./budget.md). This document is the feeder's own contract: its declaration, its
startup refusal, how each area is read, and what its checkpoints promise.

---

## 1. Declaration

```go
feeder.Description{
    SourceID:         "gcp:" + orgSlug,
    Kind:             "gcp",
    SchemaVersion:    "1.0.0",
    Ordering:         feeder.OrderingNone,   // §5.2: polls, not a sequenced stream
    ReorderingWindow: cfg.ReorderingWindow,  // §5.2: measured, not assumed
    RequiredScopes:   …,                     // §2
    Namespaces:       …,                     // data-model.md §2
}
```

`Ordering` is `none` and not `per-source-sequence` because the feeder's inputs are polls of
independent APIs plus a log stream with **no published ordering guarantee** (§5.2). Declaring a
sequence the source does not provide would make `testkit.Shuffle` weaker than reality, which is the
one direction a declaration must never err in.

---

## 2. Read-only, proved by asking

The credential is **one read-only service account**, shared with the GCP telemetry backend and with
nothing else (FR-003). The roles requested per area, and the reason each is the ceiling rather than
the convenience, are [`research.md` §8](../research.md). No role granting any write is requested,
and a Viewer-class or `*.viewer`-class role is the ceiling in every area.

### 2.1 The startup refusal

At startup the integration **asks GCP what its principal may do** and **refuses to start** if the
principal holds any write, create, update, delete or deploy permission in an area it reads, naming
the offending permissions (FR-004). The mechanism is
`cloudresourcemanager.projects.testIamPermissions` over the enumerated write-permission set for
each area read; the batching limit and the exact set are research §8.

This is the shape `internal/feeders/k8s/permissions.go` established, and the reasoning transfers
verbatim: *"No component MUST write to any production system" is not a promise a connector can make
by being careful: it is a property of the credential it holds, and the only honest way to state it
is to ask the system.* A project-owner service account pointed at production is a loaded gun
whether or not the code calls a mutating method.

**Where a permission cannot be tested**, the integration names the part it could not verify and
**requires the operator to assert read-only in configuration**. It does not assume, and it does not
narrow what it tests to what it happens to be able to test — the untestable remainder is reported,
not absorbed.

### 2.2 The one declared exception

Where an operator configures a **Pub/Sub doorbell** (§6.3), the integration acknowledges messages
on a **dedicated subscription the operator created for this purpose**. Acknowledging changes that
subscription's state, and that is the only state change this integration performs anywhere
(FR-006, SC-020). It is declared in the read-only statement, it is confined to that subscription,
and the integration **must run correctly with the doorbell absent** — which is also how the
fixtures run. It never creates, deletes or reconfigures the subscription, the topic or the
notification channel.

### 2.3 The read-only operation list is published

FR-005 and SC-020 require that read-only be verifiable **from the set of operations the integration
is able to issue**, not only from one run's behaviour. The set is published in
`docs/connectors/gcp.md` and asserted from the integration's **own recorded request log** over the
full corpus and the live run — so a reviewer checks a list against a log rather than reading code
for absences.

---

## 3. Cloud Run: the topology, and the two rollouts

### 3.1 Reading

`projects.locations.services.list` per in-scope project and region, and
`projects.locations.services.revisions.list` — which accepts `-` as the service id to list
revisions across every service in a location in one call, and returns them sorted by creation time
descending, which is exactly the order a history horizon wants to walk.

Traffic comes from two fields and the distinction matters: **`traffic[]` is the desired split** and
**`trafficStatuses[]` is the observed one** (output-only). The graph records what production is
actually serving, so `trafficStatuses` is the source of the `traffic_split` property — never
`traffic`, which is an intention that may not have converged.

### 3.2 The instant a traffic split took effect — the hardest fact in this feature

FR-017 requires a rollout change **valid at the instant the new split took effect**, and SC-002
requires that instant to equal "the instant GCP states", exactly, in 100% of cases. Research §2
establishes an awkward fact: **the Cloud Run v2 API has no field that stamps it.** What exists is a
*convergence predicate* — `reconciling`, and `traffic` vs `trafficStatuses`, and
`observedGeneration` vs `generation` — and `Service.updateTime`, which moves for **any** change to
the resource (an image bump, an environment variable, a label) and is therefore not a traffic-shift
timestamp.

The instant is taken from the **Cloud Audit Logs**. `UpdateService` is a long-running operation, so
it writes **two** entries correlated by `LogEntry.operation.id`: the request entry with
`operation.first = true` and the completion entry with `operation.last = true`. **The `timestamp` of
the `operation.last = true` entry is the published definition of "the instant the split took
effect"**, and this contract states it so that SC-002's "the instant GCP states" is a defined
quantity rather than an argument during review.

Three consequences are contract:

1. A traffic shift's valid time is **not** available from the Cloud Run API alone. The service poll
   detects *that* the split changed; the audit log supplies *when* and *who*. A split observed by
   the poll with no corresponding completion entry yet is **held**, not emitted with a guessed
   instant — and the checkpoint's extent says so.
2. `Service.updateTime` is **never** used as a change instant. It is recorded as metadata only.
3. `Condition.type` values beyond `"Ready"` — `RoutesReady`, `ConfigurationsReady` — are **not in
   the public contract** (research §2) and are not relied on, however often they appear in practice.

### 3.3 Revision creation

`Revision.createTime` is output-only and documented, so the creation change's valid time comes
from the API and `unknown` is not accepted here. There is **no `CreateRevision` audit methodName in
v2** (research §2) — revisions are created as a side effect of `CreateService`/`UpdateService` — so
the *principal* for a creation change comes from the correlated audit entry for that service
mutation, while the *instant* comes from `Revision.createTime`.

The creation change is marked **as not having moved production traffic**, and:

- a revision holding **0% of traffic still exists as a node and still produced its creation
  change** (FR-018);
- its creation change is **never reinterpreted** when traffic later moves to it — the later movement
  is its own change, at its own instant;
- `latestCreatedRevision` and `latestReadyRevision` differing is precisely the "created but not yet
  serving" window, and is how the feeder recognises the state cheaply between polls.

### 3.4 A recreation is one identifier with a hole in it

A service deleted and recreated under one name is **not one continuous entity** (FR-025), and
bitemporally that is one identifier carrying two valid intervals with a gap between them: the
predecessor retracted at the last instant its uid was observed, the successor valid from its own
`createTime`, and the span between them left as the genuine ignorance it is.

Two consequences are contract.

The retraction's instant is the **last instant the old uid was observed** — not the successor's
`createTime`, which says when the successor began and nothing about when the predecessor ended, and
emphatically not the predecessor's own `createTime`, which would close its interval at its own start
and erase its history.

And **each state of a service is named by the platform's version stamp**, the uid plus `updateTime`.
That is what lets the successor be asserted at all, since a name-only event id made its assertion a
duplicate of the predecessor's, and it is what lets any later state reach the node. A uid's first
poll asserts the service's existence from `createTime`, and every poll where `updateTime` is later
asserts its current state from `updateTime`, each under an id carrying that instant. A restarted
feeder therefore re-sends ids already sent and re-dates nothing, and a change made while it was down
is dated where the platform dates it (T066, T184). A later poll with no stated update instant asserts
a state only if what it saw differs from the last one asserted, from the observation and marked
unknown (FR-011).

The predecessor's last state can begin at its last sighting, so a recreation's retraction never ends
at or before the start of the last state asserted for the old uid: it is moved to one microsecond
after it. A retraction ending at that start would leave the state standing, and the old service would
reappear in the gap between the deletion and the successor (T184).

*Corrected 2026-09-26 (003 T066).* This section used to say the retraction and the re-assertion
"do not commute", so a conformance shuffle could never pass on a recreation. That was wrong. The
projector's segment planner lets an assertion made at or after a retraction resurrect the entity in
any delivery order. The failures had two causes, both now fixed. First, the successor's assertion
was dropped as a duplicate, because a service's event id was its ref alone. Second, the successor's
`changed_by` edge was dropped whenever the retraction was applied after it, because the retraction's
cascade treated an edge no edge assertion produced as asserted before the retraction. `fixtures/gcp-service-recreated-01` is committed
and passes all four conformance steps.

---

### 3.5 Actor and actor kind

The derivation ladder is [`data-model.md` §4.1](../data-model.md#41-actor-kind-is-derived-from-a-classification-never-from-the-string).
The fields it reads, all from `protoPayload.authenticationInfo`:
`principalEmail`, `principalSubject`, `serviceAccountKeyName` (present ⟹ static-key auth, a CI
signal), and **`serviceAccountDelegationInfo[].firstPartyPrincipal`**, which research §2 establishes
as **the only documented signal that the caller is a Google first-party principal** — and is
therefore the evidence for `CONTROLLER`.

`requestMetadata.callerSuppliedUserAgent` is useful for telling gcloud from Terraform from a custom
CI client, and GCP's own reference says it **"is not authenticated and should be treated
accordingly"** — so it is recorded as evidence and never as the deciding factor. Where the ladder
reaches its end the kind is `ACTOR_KIND_UNKNOWN` **with the evidence that was available recorded**,
which is not the same as `ACTOR_KIND_UNSPECIFIED` (data-model.md §4.1).

---

## 4. Cloud SQL

`instances.list` (`maxResults` default 500, max 1000) for instances, engine and version,
`settings.databaseFlags` and `settings`; `operations.list` filtered on `operationType` for **past**
maintenance (`MAINTENANCE`, `RESCHEDULE_MAINTENANCE`), with `insertTime` / `startTime` / `endTime`;
`flags.list` for the flag catalogue that makes a flag change interpretable.

**Scheduled maintenance is a genuinely future-dated instant.**
`DatabaseInstance.scheduledMaintenance` carries `startTime` — *"The start time of any upcoming
scheduled maintenance for this instance"* — plus `canReschedule` and `scheduleDeadlineTime`
(research §4). So FR-032 is satisfiable exactly as written: a scheduled Cloud SQL maintenance is a
`CLOUD_MAINTENANCE` change with actor kind `VENDOR` whose valid interval is the announced window,
and the **announced-fact semantics of the vendor-notice feeder apply unchanged** — the same state
machine, the same correction rules, the same ranking exclusion. This is the one place the platform
feeder emits an announced fact, and it must not grow a second implementation of §5 of the data
model.

`settings.maintenanceWindow` and `settings.denyMaintenancePeriods` are a recurring **policy**, not
an event: they are properties of the instance, and they never become change nodes.

**The Go client is `google.golang.org/api/sqladmin/v1`, not the GAPIC.** Research §4 found the
preview GAPIC `cloud.google.com/go/sql/apiv1` (v0.1.0) returns `*ApiWarningIterator` from
`SqlInstancesClient.List` — an iterator over warnings rather than over instances — which is a
code-generation defect that makes it unusable. This is recorded here because it is the kind of
choice a later contributor would otherwise "modernise" back into a bug.

### 4.1 Dependencies, derived and proposed

Where a service's configuration, connection settings or labels name an instance, a `depends-on`
edge is asserted **with the deriving evidence recorded** (FR-028). Where it cannot be derived, a
**proposed dependency** is raised carrying the suggestive evidence, its score and its rationale —
and **no edge** (FR-029). Raised once per cycle, not re-raised while pending. Until plan item 2
lands, the suggestion is **held rather than emitted**, because the alternatives are inventing an
edge and losing the evidence.

---

## 5. The Cloud Audit Logs admin-activity stream

### 5.1 Reading

`logging.entries.list` with

```
logName="projects/PROJECT_ID/logs/cloudaudit.googleapis.com%2Factivity"
AND timestamp >= "<from>" AND timestamp < "<to>"
```

plus the configured service and operation filters. `resourceNames` takes at most 100 entries, the
filter at most 20,000 characters, and `orderBy` is `"timestamp asc"` by default — Google's own
advice is to set `"timestamp desc"` when reading recently ingested entries, because the ascending
default *"may take a long time to fetch matching logs that are only recently ingested."*

`pageSize` defaults to 50, and **its maximum is not documented** (research §6): the widely-cited
1,000-entry page cap and ~10 MB response cap appear nowhere in Google's reference or quota pages.
The feeder therefore does not design against them. It requests a large page, **accepts whatever
comes back**, paginates, and enforces **its own** hard entry, byte and wall-clock budget — stating
in the checkpoint where it stopped and by which criterion. A limit taken from folklore is a limit
that changes without a changelog.

One pagination rule is a correctness rule rather than an efficiency one: **if `nextPageToken`
appears but `entries` is empty, the search is not finished.** An implementation that stops on an
empty page silently under-reads its own window, and the checkpoint would then claim an extent it did
not cover.

**The data-access stream is not read and no permission for it is requested** (FR-042). Admin
activity is always written and cannot be disabled, which is why it is a dependable change stream;
data access is off by default, enormous, and disproportionate to its value for change detection.

### 5.2 The reordering window is measured, not assumed

FR-041 requires the feeder to **declare** its reordering window. Research §3 establishes that
**Google publishes no ingestion-delay or out-of-order bound for Cloud Audit Logs** — the only
statements are qualitative ("during periods of heavy load, there could be delays"). There is no
number to cite.

So the window is **configuration, seeded from measurement, and recorded in the checkpoint**:

- every poll re-queries a **trailing overlap window** rather than starting where the last one ended;
- entries are deduplicated on **`insertId`**, which GCP defines as its duplicate key;
- the feeder continuously measures `receiveTimestamp − timestamp` and **reports the observed
  distribution** in its operational telemetry, so the configured window can be justified from data
  and widened when the data says so;
- an entry arriving **after** the extent it belongs to has been checkpointed is **still emitted**,
  with the entry's own instant as valid time — and the gap stands in the checkpoint. Changes learned
  after an investigation has run are exactly what feature 002's reopening path exists for.

`timestamp` is the event instant and is the **only** field used as valid time. `receiveTimestamp` is
the delivery instant and is used **only** for lag measurement and watermarking, never as valid
time — GCP's reference defines both, and conflating them would make every delayed entry a lie about
when production changed.

### 5.3 Taxonomy

A kind from the published taxonomy where one fits; otherwise `CHANGE_KIND_OTHER` **with GCP's own
operation name recorded** and never dropped (FR-039). `IAM_CHANGE` and `QUOTA_CHANGE` now exist in
the taxonomy (ADR-0006 D3), so the "other" fallback no longer covers them and a fixture asserting
otherwise would assert a regression. Which log types, services and operations count as changes is
**configurable, and the configuration in force is recorded in the checkpoint** (FR-040) — so a later
reader can tell "no change happened" from "we were not looking for that kind of change".

---

## 6. Cloud Monitoring: policies, and the transition problem

`projects.alertPolicies.list` gives the ALERT nodes: policy identifier, display name, severity where
stated, and each condition's filter as a pointer in Cloud Monitoring's own vocabulary
([`pointer-vocabularies.md`](./pointer-vocabularies.md)).

Transitions are the harder half, and how they are obtained — including whether a read-only API for
alerting **incidents** exists at all — is research §5, with the chosen mechanism and its fallback
recorded there. What this contract fixes regardless of mechanism:

### 6.1 The event

An `alert.transition` (001 event body 25) whose **valid time is the transition instant Google
reports**, never the instant the feeder learned of it, and whose idempotency key is 001's derived
4-tuple `(source, stable alert identifier, group, transition instant)` — source `gcp`, the policy
identifier, the policy's group, Google's instant (FR-046).

### 6.2 What the key buys

- **two transports, one transition**, order-independent (FR-050, SC-004);
- **two intakes, one trigger** — a GCP transition and a human declaration through feature 002's
  Slack transport converge on one event shape and one convention (FR-046, FR-047);
- **grouped policies stay separate**: N alerting groups produce exactly N alerts, and the
  policy-level entity is never reported as alerting because one group is (FR-049, SC-005).

### 6.3 The doorbell

A Pub/Sub notification channel **may** be configured. It means **"poll now" and nothing else**:

| rule | consequence |
|---|---|
| its body is **never parsed, trusted or stored** | a forged or replayed notification cannot inject a state |
| the endpoint or subscription is authenticated and **rate limited** | a flood costs at most the rate limit |
| a forged, replayed or malformed notification costs **at most one extra poll** | and creates, alters or retracts **no** alert, state version or event (FR-050) |
| polling is the source of truth | the integration runs correctly with the doorbell absent, and the fixtures run that way |

Acknowledging a message on the operator's dedicated subscription is §2.2's single declared state
change.

---

## 7. GKE, load balancers and DNS

**GKE**: the cluster, as an INFRA_RESOURCE with identity claims naming it as the Kubernetes feeder
knows it — and **nothing below it**. No workload, no pod, no container (FR-030). The one inference
cluster belongs to the 001 Kubernetes feeder, and a second node for the same workload is the failure
this rule exists to prevent.

**Load balancers and Cloud DNS**: P3 (FR-055–FR-057), and the **first thing cut** under budget.
Where either surface is outside the budget or outside the credential's permissions the feeder reads
**neither**, states the omission in its checkpoint **as a scope statement**, and every other
requirement holds unchanged. A superseded `exposed-via` relation is **retracted at the instant of
the swap**, not deleted.

---

## 8. Checkpoints

A `SourceCheckpoint` on start, on resync and after any gap (FR-026), stating the extent actually
covered and whether the feeder was watching immediately before it. What a checkpoint carries beyond
the extent, and why each is there:

| recorded | so that a later reader can tell |
|---|---|
| the **project and region scope in force** (FR-009) | "not present" from "not in scope at that time" — and the scope is changeable without losing history, because recording starts before the campaign scope is agreed (FR-130) |
| the **revision history horizon** (FR-024) | "there was no revision" from "we did not look that far back" |
| the **audit-log type, service and operation filters** in force (FR-040) | "no change happened" from "we were not looking for that kind of change" |
| the **environment mapping** in force | which published mapping produced a node's environment, since the organisation has no uniform label |
| the **declared reordering window** and the observed lag (§5.2) | whether a gap was expected |
| **omitted surfaces** as a scope statement (FR-057) | a deliberate omission from a failure |
| the **discovered Monitoring quota limit** ([`budget.md` §4](./budget.md)) | what a "share of remaining" was a share of |

### 8.1 A partial poll retracts nothing

A poll that fails part-way produces a checkpoint stating the extent **actually** covered, declaring
the gap, and **emitting no retraction for the part it did not read** (FR-012). It does not count
toward the silence rule's consecutive-poll total, because only a **complete** poll is evidence of
absence. This is the single rule that keeps a network blip from looking like the organisation
deleting half its estate, and `gcp-partial-poll-01` is its fixture.

---

## 9. Conformance

| gate | assertion | requirement |
|---|---|---|
| replay | every fixture replays from empty to its golden | FR-142 |
| idempotency | double delivery is a no-op, including both doorbell and poll delivering one transition | SC-004, SC-017 |
| shuffle | permuting inside the declared reordering window changes no valid-time state | FR-041, SC-017 |
| rollout exactness | every traffic shift's valid time equals §3.2's defined instant, exactly, in 100% of cases; zero zero-traffic revisions presented as having moved traffic | SC-002 |
| actor kind | non-`unknown` for ≥90% of principal-attributed changes; 100% of the remainder carry their evidence; **zero** derived from the principal string alone | SC-003 |
| grouped alerts | N alerting groups ⟹ exactly N alerts and zero policy-level alerts | SC-005 |
| coverage vs GCP | the service, revision and alert-policy sets against GCP's own lists, differences **enumerated** not summarised | FR-144, SC-001 |
| read-only | every issuable operation on the published read-only list, from the integration's own request log | FR-005, SC-020 |
| partial poll | zero retractions from an incomplete poll; the gap declared | FR-012 |
| live parity | the graph built live equals the graph built from the recording of the same run | FR-143, SC-016 |
