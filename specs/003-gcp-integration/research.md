# Phase 0 Research: GCP Integration and Vendor-Notice Feeder

**Feature**: 003-gcp-integration | **Date**: 2026-09-20 | **Plan**: [plan.md](./plan.md)

Every unknown the plan carried into Phase 0, resolved. Format per section: **Decision**,
**Rationale**, **Alternatives considered**.

Vendor facts were verified against Google Cloud, Microsoft Learn, Google Workspace and RFC Editor
documentation on 2026-09-20, and in several places against the published Go module source. **Where a
fact could not be verified it is marked `[UNVERIFIED]` and the design is written so that being wrong
about it is survivable.** That marking is not decoration: four of the findings below overturn an
assumption this feature's spec was written on, and one of them was overturned by *discovering the
documentation is silent* rather than by finding a contrary fact.

> **Note on documentation URLs**: Google Cloud documentation moved from `cloud.google.com/…` to
> `docs.cloud.google.com/…`. The old host 301-redirects and several old paths now 404.

---

## §1. Where the code lives, and what it links

**Decision.** In-tree, in the `sre-agent` binary: `internal/feeders/gcp/`,
`internal/feeders/vendornotice/`, `internal/backends/gcp/`, `internal/gcpx/`, `internal/sanitise/`.
The Google Cloud client libraries are linked in. No new public Go package.

**Module set** (versions verified via `proxy.golang.org`):

| purpose | package | module | version |
|---|---|---|---|
| Cloud Run services/revisions | `cloud.google.com/go/run/apiv2` (+ `/runpb`) | `cloud.google.com/go/run` | v1.22.0 |
| Logging reads | `cloud.google.com/go/logging/apiv2` (+ `/loggingpb`) | `cloud.google.com/go/logging` | v1.19.1 |
| Monitoring time series, alert policies | `cloud.google.com/go/monitoring/apiv3/v2` | `cloud.google.com/go/monitoring` | v1.30.0 |
| Monitoring **incidents** (`projects.alerts`) | `google.golang.org/api/monitoring/v3` | `google.golang.org/api` | v0.298.0 |
| Cloud SQL Admin | `google.golang.org/api/sqladmin/v1` | `google.golang.org/api` | v0.298.0 |
| Pub/Sub doorbell | `cloud.google.com/go/pubsub/v2` | `cloud.google.com/go/pubsub/v2` | current |

**Rationale.** `docs/connectors/writing-a-backend.md` recommends `examples/<name>/` as its own module
when the vendor SDK should not be linked into the binary, *"which is the usual case for a backend"*.
This feature goes in-tree against that advice for two reasons it names itself. First, the page's own
"honest limitation": the pieces a conforming response needs — `backend.NewResponse`, the coverage
helpers, `ClampRequest`/`AnnotateHorizon` — *"are all under `internal/` today"*, so an out-of-tree
module in its **own** module path cannot build one at all. Second, and decisively, FR-003, FR-113 and
FR-152 require the feeder half and the backend half to share **one credential** and **one
per-endpoint-class budget** and to report usage separately **in one report**. A process boundary
between them turns a shared token bucket into an IPC problem, for no benefit.

The cost is dependency weight and binary size, and it is Complexity Tracking C1's to justify. The
blast radius is bounded rather than denied: every vendor import is confined to
`internal/feeders/gcp/transport.go` and `internal/backends/gcp`, and binary size before and after is
published in `docs/connectors/gcp.md`.

**Alternatives considered.** *Hand-rolled REST over `net/http`* — rejected: it is less code only
until the first pagination, long-running-operation or partial-failure case, and a bug in any of those
reproductions shows up as a **wrong graph** rather than a failed call. *`examples/gcp-connector/` as
its own Go module with a `replace` directive* — the shape `examples/mcp-feeder` takes; it *can* import
`internal/…`, so the honest-limitation objection is soluble, but the shared-budget objection is not,
and it doubles the operational surface Dana has to approve. *A separate binary* — same objection,
worse ergonomics.

---

## §2. The instant a traffic split took effect

**Decision.** A traffic-shift rollout's valid time is **the `timestamp` of the Cloud Audit Log entry
with `operation.last = true`** for the `UpdateService` long-running operation, correlated to its
request entry by `LogEntry.operation.id`. This is published in
[`contracts/gcp-feeder.md` §3.2](./contracts/gcp-feeder.md) as the definition of "the instant GCP
states" for SC-002. A revision **creation**'s valid time is `Revision.createTime` from the API. A
split observed by the service poll with no corresponding completion entry yet is **held, not emitted
with a guessed instant**, and the checkpoint's extent says so.

**Rationale.** This was the plan's sharpest unknown and the answer is uncomfortable: **the Cloud Run
v2 API has no field that stamps when a traffic split took effect.** What it has is a *convergence
predicate*, spelled out in the `runpb` proto comment on `Service.reconciling`: while reconciliation is
in progress `observed_generation`, `latest_ready_revision`, `traffic_statuses` and `uri` *"will have
transient values that might mismatch the intended state"*, and on success *"the following fields will
match: `traffic` and `traffic_statuses`, `observed_generation` and `generation`,
`latest_ready_revision` and `latest_created_revision`."* A predicate is not a timestamp.

The nearest timestamps are both wrong. `Service.updateTime` is *last-modified of the resource* — it
moves for an image bump, an environment variable or a label, so using it as a traffic-shift instant
would attribute every configuration change to a traffic movement. `Condition.lastTransitionTime` is
closer, but the only `Condition.type` the public contract documents is `"Ready"`; `RoutesReady` and
`ConfigurationsReady` appear in practice and are **`[UNVERIFIED]` — absent from the public proto and
REST reference**, so the design does not build on them however often they show up.

That leaves the audit log, where `UpdateService` being a long-running operation is an advantage: it
writes **two** entries sharing an `operation.id`, with `operation.first = true` on the request and
`operation.last = true` on the completion. The completion entry is the closest thing GCP publishes to
"the split is now serving", and it carries the **principal** as well — which the revision-creation
path needs anyway (below).

Two related findings shape the same code path. **There is no `CreateRevision` audit methodName in
v2**: the Cloud Run audit-logging reference lists only `DeleteRevision` as a write and
`GetRevision`/`ListRevisions` as reads, because revisions are created as a *side effect* of
`CreateService`/`UpdateService`. So a creation change takes its **instant** from
`Revision.createTime` (output-only and documented) and its **actor** from the correlated service
mutation. And `latestCreatedRevision` differing from `latestReadyRevision` is exactly the
"created but not yet serving" window, which is how FR-018's zero-traffic revision is recognised
cheaply between polls.

**Alternatives considered.** *`Service.updateTime`* — rejected above; it is recorded as metadata only.
*Polling frequently enough to observe the transition* — rejected: it would put an upper bound on
valid-time accuracy equal to the poll interval, and SC-002 asks for exactness, not a bound. *Emitting
the split change at the poll instant with an unknown-valid-start marker* — rejected: the instant *is*
available from the audit log, and marking it unknown when a source states it would violate FR-022's
spirit in the opposite direction.

---

## §3. Which Cloud Monitoring query vocabulary to publish

**Decision.** `gcp-monitoring-filter/v3` — the **Monitoring filter language plus `Aggregation`**,
executed by `projects.timeSeries.list` on Monitoring API v3. Not PromQL, not MQL. Registered with the
API surface's version in the name, because the language is not independently versioned. Full grammar
and the mandatory `resource.type` clause are
[`contracts/pointer-vocabularies.md` §2](./contracts/pointer-vocabularies.md).

**Rationale.** The plan carried this unknown because MQL's status was in doubt. It is settled: the
`timeSeries.query` API reference itself says *"We recommend using PromQL instead of MQL"*, the Go
`QueryClient.QueryTimeSeries` binding is marked deprecated, Google's deprecation page records that
support for writing MQL **ended 2025-07-22** and console creation is gone — while also stating *"MQL
is not shut down"* with **no retirement date published**. Deprecated with no end date is the worst
kind of dependency to mint new stored pointers in.

PromQL is Google's recommended *language* and is the tempting answer, and it loses on two specifics.
It is served on Monitoring API **v1**, not v3; and the managed-Prometheus documentation states plainly
that *"the Prometheus HTTP endpoints aren't available in the Cloud Monitoring language-specific client
libraries"* — so there is **no first-party Go binding**, leaving `google.golang.org/api/monitoring/v1`
or the OSS Prometheus client with an OAuth2 transport. Its semantics are also governed by upstream
Prometheus rather than by a Google-versioned contract.

The deciding criterion is FR-083's: a pointer is **stored** and read back years later. The vocabulary
therefore has to be *stable and versioned*, not merely current. The filter language is the only one
of the three that is simultaneously GA, carried on an explicitly versioned Google surface, and bound
in the actively developed first-party GAPIC.

Two consequences recorded rather than discovered later:

- **Server-side aggregation exists and is the design.** `aggregation`/`secondaryAggregation` take
  `alignmentPeriod` (minimum 60 s), `perSeriesAligner`, `crossSeriesReducer` and `groupByFields`. This
  makes constitution IV structural rather than effortful: the summary is computed in GCP and the
  points never leave it. A `compare` is two aligned-and-reduced queries, not a download and a loop.
- **Cloud Run metrics retain for 6 weeks.** `run.googleapis.com/*` is not in the 24-month list, so it
  falls under *"All other Google Cloud metrics… 6 weeks"*. §6 below and
  [`contracts/gcp-telemetry-backend.md` §6](./contracts/gcp-telemetry-backend.md) deal with what that
  does to the published outcome set.

**Alternatives considered.** *PromQL* and *MQL*, above. *Publishing two vocabularies and choosing per
pointer* — rejected: two vocabularies mean two executors, two grammars to document and two ways for a
golden to drift, for a capability nothing in the algebra needs.

---

## §4. Ingestion lag, and how `not_yet_ingested` is decided

**Decision.** For **metrics**, read `MetricDescriptor.metadata.ingestDelay` via
`projects.metricDescriptors.get` and apply

```
cutoff := now − ingestDelay
window.End >  cutoff, no points → NOT_YET_INGESTED
window.End <= cutoff, no points → NO_DATA
```

falling back to the documented per-metric default (**120 s** for Cloud Run) when the field is empty,
**and stating in coverage which source was used**. For **logging** there is no equivalent field: the
lag is estimated as `now − max(receiveTimestamp)` over the newest matching entries and is reported
**as an estimate**, with `ingestion_lag_undetermined` where neither path yields a figure.

**Rationale.** `ingestDelay` is a documented API field and its wording is exactly the guarantee this
needs: *"The delay of data points caused by ingestion. Data points older than this age are guaranteed
to be ingested and available to be read."* That turns FR-104's distinction between `no_data` and
`not_yet_ingested` from a heuristic into a **contractual** boundary, which is the difference between
an investigation concluding "nothing happened" and "I cannot tell yet".

Cloud Run's metric descriptions state the figure in prose too: *"Sampled every 60 seconds. After
sampling, data is not visible for up to 120 seconds."*

Two caveats are carried rather than hidden. `ingestDelay` is a **static declared value per metric
type**, not a live measure of pipeline health. And whether it is actually **populated** for
`run.googleapis.com/request_count` was **`[UNVERIFIED]`** — not checkable without credentials — which
is precisely why the fallback and the "which source" statement exist.

For **Logging**, Google publishes **no freshness SLO and no numeric write-to-queryable latency**; the
only statements are qualitative (*"It can take a few minutes for Logging to receive log entries"*;
*"during periods of heavy load, there could be delays"*), and the Cloud Observability SLA is
availability-only. `receiveTimestamp − timestamp` measures the producer→Logging delay and
`now − max(receiveTimestamp)` the observed end-to-end lag — but **neither measures index visibility**,
the residual between "received" and "queryable". That unobservable residual is the honest reason
`not_yet_ingested` has to exist as an outcome at all rather than being computable away.

**Alternatives considered.** *A fixed configured lag per surface* — rejected for metrics where GCP
publishes a per-metric figure; retained as the **fallback** only. *Probing with a synthetic write to
measure the round trip* — rejected outright: it is a write (FR-005).

---

## §5. Alert transitions — the assumption that was wrong

**Decision.** Read incidents from **`projects.alerts.list` / `.get`** on Monitoring API v3, behind a
**declared capability flag**, with `Alert.openTime` / `closeTime` as the transition instants and
`state` ∈ {`OPEN`, `CLOSED`}. With the flag **off**, `monitor_state` still answers — `NO_DATA` with
coverage naming the absent source — and ALERT nodes still exist from `alertPolicies.list`, which is
GA in the GAPIC. The **Pub/Sub doorbell remains a doorbell**: "poll now", body never parsed, and it is
a supplement that can never be the source of truth because it is push-only and cannot answer a
historical question.

**Rationale.** The plan carried this as *"the single largest design risk in the feature, because User
Story 4 and SC-004 are written as though transitions can be polled"* — and the research **refuted the
pessimistic assumption**: a public read API does exist. The `Alert` resource documentation is explicit
that it *"is a read-only resource that cannot be modified by the accompanied API"*, and the Go binding
exposes only `Get` and `List`. It carries exactly what FR-046 and FR-098 need: `name`
(system-assigned), `state`, **`openTime`**, **`closeTime`**, `resource` and `metric` (the labels
preserved from the generating condition — what the alert watches), and a `policy` snapshot. Closed
incidents retain **13 months**, open ones indefinitely, and an incident with no new data auto-closes
after **7 days**.

So SC-004's exactness is achievable: valid time is Google's reported instant, not a poll observation.

**But the good news arrives with the feature's riskiest dependency, and the design says so.** Two
facts hold together: the API is **Public Preview** under Pre-GA terms, with Google's own warning that
*"the labels in the response are subject to change while this feature is in preview"*; and it is
**not in the first-party GAPIC** — `cloud.google.com/go/monitoring/apiv3/v2` has no `ListAlerts`, so
the only Go binding is `google.golang.org/api/monitoring/v3`, whose package header reads *"This
package is DEPRECATED… maintenance mode"*. A Preview API through a maintenance-mode client is not
something to build a P1 user story on unconditionally, which is why the capability flag is part of the
decision rather than an operational afterthought.

The degradation path is the contract's own answer for a missing source, not a special case: `NO_DATA`
with the absent source named, identical live and recorded, stable for the whole window. The feature
therefore *works* with incident reads off — it just cannot report transitions, and it says which.

**One finding inverts a premise of FR-006.** Pulling a Pub/Sub subscription requires
`pubsub.subscriptions.consume`, and that single permission authorizes `pull`, **`acknowledge`,
`modifyAckDeadline` and `seek`**. `roles/pubsub.viewer` does not contain it and **cannot pull**;
`roles/pubsub.subscriber` is exactly three permissions and all three mutate. **There is no read-only
Pub/Sub pull at any granularity — not even with a custom role**, because the permission that
authorizes reading also authorizes deleting and rewinding. FR-006 frames the exception as *"the
integration acknowledges messages… which changes that subscription's state"*; the sharper truth is
that **holding the grant at all is the exception**, and the acknowledgement is only its most visible
effect. §8 records how the read-only statement must therefore be worded, and the binding is
**resource-level on the one subscription**, never project-level.

**Alternatives considered.** *The Pub/Sub payload as the source of truth* — rejected by FR-050 and by
the fact that it is push-only: it carries `incident_id`, `state`, `started_at`, `ended_at`, `severity`
and the resource, but a channel must have existed **before** the incident, so it cannot answer about
the past and cannot be reconciled after an outage. *Monitoring metrics for incident counts* and *a log
stream carrying incident lifecycle events* — both **`[UNVERIFIED]`, none found documented**; not built
on. *Inferring transitions by polling the condition's own time series and re-evaluating the threshold*
— rejected: it reimplements Google's alerting semantics, including its latency compensation, and would
disagree with the console at exactly the wrong moment.

---

## §6. Cloud Logging has no server-side aggregation, and retention has no outcome

**Decision.** `new_log_patterns` mines templates **client-side** with feature 002's Drain-style miner
over a **bounded sample**, and the bound is published, enforced by the backend, and **stated in
coverage** — `volume_considered`, the truncation criterion, and the sub-window actually covered.
Neither log-based metrics nor Observability Analytics is used. A window older than the vendor's
retention is answered `QUERY_FAILED / REJECTED_BY_BACKEND` with the retention horizon in
`failure_detail`, pending the additive `FailureReason.OUTSIDE_RETENTION` that is plan item 7.

**Rationale, part one — aggregation.** The Logging query language has **no `GROUP BY` and no
`COUNT`**; `entries.list` returns raw entries. Both server-side alternatives are unavailable to a
read-only connector, and for different reasons:

| alternative | why refused |
|---|---|
| **log-based metrics** | creating one is a **mutation** (`logging.logMetrics.create`), which FR-005 forbids. And they are **not retroactive** — *"calculated from logs received after creation"* — so they could not answer a question about a past incident even if writing were permitted. Also chargeable, also 6-week retention, also up to 10 minutes to update |
| **Observability Analytics** | requires **upgrading the log bucket** — a mutation — with entries written before the upgrade unavailable until backfill; and the programmatic path is the BigQuery API against the linked dataset, which **bills BigQuery analysis** |

This is the one place where the technically superior answer is refused on principle rather than on
merit, and it is worth being explicit about the trade: client-side mining over a sample is *less
accurate* than server-side grouping, and it is chosen because the alternative requires writing to the
organisation's project. The compensation is that the sample bound is **published and reported**, so a
consumer knows what the number is a number *of* — which is FR-101's whole purpose.

Two hard facts bound the sampling. `entries.list` is **60 calls per minute per project, cannot be
increased, and is not hierarchical** — shared with every human querying logs in that project, which is
the fact [`contracts/budget.md`](./contracts/budget.md) is built around. And `pageSize` **has no
documented maximum**: it defaults to 50, negative values are rejected, and the widely-cited
1,000-entry and ~10 MB caps appear **nowhere** in Google's reference or quota pages. The two research
passes disagreed on this exact point, and the disagreement is itself the finding — the figure is
folklore. So the backend requests a large page, accepts what comes back, paginates, and enforces its
own entry, byte and wall-clock budget.

One pagination rule is a **correctness** rule, in GCP's own words: *"If a value for `nextPageToken`
appears and the `entries` field is empty, it means that the search found no log entries so far but it
did not have time to search all the possible log entries."* An empty page is therefore **not**
`NO_DATA`. A backend that reported it as one would tell an investigation that nothing happened because
it ran out of time — the single most dangerous confusion available in this feature.

**Rationale, part two — retention is a hole in the published contract.** Cloud Run metrics retain 6
weeks; Logging's `_Required` bucket is 400 days and `_Default` 30. A window older than retention was
**never covered**, so `NO_DATA` — *"the query was valid, the window was covered, and there is nothing
in it"*, and **the only outcome that is evidence nothing happened** — is the wrong answer, for exactly
the reason FR-104 forbids `no_data` for a window that is merely *too recent*. `NOT_YET_INGESTED` is
that mirror case; **there is no outcome for "too old"**, and no `FailureReason` either: the published
set is `REJECTED_BY_BACKEND`, `NOT_PERMITTED`, `RATE_LIMITED`, `TIMED_OUT`, `UNSUPPORTED_POINTER`,
`OUTSIDE_ALGEBRA`. This is a genuine gap, shared with 005 because Datadog has retention too, and it is
plan item 7. The interim answer fails **safe**: a typed failure makes the investigation say *"I could
not look"* rather than *"nothing was there"*.

**Alternatives considered.** *`entries.tail`* — rejected: live-follow only, bidirectional-streaming
(gRPC only, unavailable over REST), and the wrong primitive for a bounded digest over a past window.
*The system metric `logging.googleapis.com/log_entry_count`* — free and server-side, but groups only by
log name and severity, which is useless for mined templates; retained as a cheap corroborating count.
*Asking the operator to pre-create log-based metrics out of band* — rejected: it makes the feature's
accuracy depend on a manual step, and it is still not retroactive.

---

## §7. Quota: GCP reports no usable remaining figure

**Decision.** A **client-side token bucket per (API, project, region)**, seeded from the documented
defaults, is the real-time gate — **self-tracked**. The `serviceruntime` metrics
(`quota/limit` − `quota/rate/net_usage`) are a **slow reconciliation and drift check** —
**vendor-reported** — and never authorise a call the fast loop refused. Monitoring's own limit is
**discovered at startup** because it is undocumented. Every figure in the usage report is labelled
with which loop produced it. Full contract: [`contracts/budget.md`](./contracts/budget.md).

**Documented limits:**

| API | limit | scope | increasable |
|---|---|---|---|
| `logging.entries.list` | **60 / min** | per project, **non-hierarchical** | **no** |
| Cloud Run Admin reads | 3,000 / 60 s | per project **per region** | yes |
| Cloud SQL Admin `get`/`list` | 500 / min | per user per region | — |
| `monitoring.timeSeries.list` | **not published** | per project | — |

**Rationale.** FR-148 assumes a world in which a vendor may report remaining quota. GCP is not that
world, for any of the four APIs:

- **no rate-limit response headers** — no `X-RateLimit-Remaining` or equivalent; the first signal is
  `429 / RESOURCE_EXHAUSTED`. This negative is **`[UNVERIFIED]` as a documented statement** — no
  Google page says "we emit no such headers" — and is inferred from quota-troubleshooting guidance
  that describes only 429-based detection. Confidence: high.
- the **Cloud Quotas API** and **Service Usage API** expose configured *limits*, not consumption;
- the only vendor usage figure is `serviceruntime.googleapis.com/quota/rate/net_usage` (DELTA,
  1-minute sampling, labels `quota_metric`/`service`/`limit_name`) against `quota/limit`. Remaining
  must be **computed**; there is no `remaining` metric; it is **minutes-stale**; and reading it
  **itself consumes `monitoring.query` quota**.

So FR-148's two branches are real but **asymmetric**, and the plan says so rather than implying
parity: the vendor-reported branch exists and is used, as a slow signal; every real-time decision is
self-tracked. A minutes-stale figure cannot gate a 60-second incident.

`monitoring.timeSeries.list` is a genuine documentation gap — three Google quota pages decline to give
a number and redirect to the console Quotas dashboard or `gcloud alpha services quota list`. A budget
expressed as a share of an unknown limit is not a budget, so the limit is discovered at startup from
`quota/limit`, recorded in the checkpoint, and where discovery fails a conservative configured default
applies and the report says it was a fallback. **"Limit unknown" is never treated as "limit
unlimited".**

The 60-call `entries.list` limit deserves its own sentence, because it is the fact that makes this
contract necessary rather than prudent: one call per second, project-wide, **shared with the on-call
who is trying to read the logs during the incident the integration is investigating.** FR-147's
"leave a published reserve unspent" is not politeness; it is the difference between helping and
competing.

**Alternatives considered.** *Relying on 429 and backing off* — rejected as the primary mechanism: by
the time a 429 arrives the human's query may already have been refused. *Reading `serviceruntime`
frequently enough to be current* — rejected: it consumes the quota it measures, and the 1-minute
sampling floor makes it impossible anyway. *A global call budget* — rejected by FR-147: GCP quotas are
per API and per project, and a global figure lets a cheap class starve the binding one.

---

## §8. Proving the credential holds no write permission

**Decision.** Three layers at startup, in this order, and the refusal names which layer objected:

1. **`cloudresourcemanager.projects.testIamPermissions`**, chunked, over an explicitly enumerated
   write-permission set per area read — including
   `iam.serviceAccounts.getAccessToken`/`signJwt`/`signBlob`/`implicitDelegation`, because a principal
   that can impersonate is not read-only however clean the rest looks. Per-resource
   `testIamPermissions` **additionally** where a service offers it (Cloud Run services, Pub/Sub
   subscriptions, DNS managed zones, Compute backend services and URL maps, Logging bucket views).
2. **A local OAuth token-scope check** (`tokeninfo`), because the IAM test answers about the **policy**
   and not about the **token** — a credential minted with `cloud-platform.read-only` is materially
   safer than one with `cloud-platform`, and the IAM test cannot tell them apart.
3. **An explicit operator assertion in configuration** covering, by name, everything the first two
   layers cannot reach (below).

The classifier uses an **explicit allowlist of read verbs** plus an explicit denylist and **fails
closed** on anything unrecognised — never a substring match.

**Rationale.** `testIamPermissions` is the right primitive and it has one excellent property: *"No IAM
role is required to test permissions."* The startup self-check therefore costs the principal
**nothing**, which is what makes it acceptable as an always-on gate. It is, however, a **weaker**
instrument than Kubernetes' `SelfSubjectRulesReview`: it tests a **supplied list** and returns the
held subset. **There is no GCP API that enumerates the permissions a caller holds.**

Every enumeration candidate fails, and mostly for the same reason — *asking costs more privilege than
the connector should hold*:

| candidate | verdict |
|---|---|
| `iam.permissions.queryTestablePermissions` | returns what is **testable on the resource**, not what the caller holds. **Useful as the source of the candidate list** to feed layer 1 |
| `iam.roles.queryGrantableRoles` | a property of the resource; irrelevant to this question |
| **Policy Troubleshooter** | strictly better analysis — it walks the hierarchy and evaluates allow, deny, PAB and conditional bindings — but requires `roles/iam.securityReviewer` **granted on the organization** (**2,547 permissions**, spanning `secretmanager.secrets.list`, `cloudkms.cryptoKeyVersions.list`, `storage.objects.getIamPolicy`). Granting that so a read-only connector can introspect *itself* inverts the control. **Disqualified** |
| **Asset Inventory `analyzeIamPolicy`** | needs `cloudasset.assets.analyzeIamPolicy` + `searchAllIamPolicies` + `searchAllResources`, realistically `roles/cloudasset.viewer` at org or folder scope. **Disqualified** |
| `projects.getIamPolicy` + `roles.get`, expanded by hand | direct bindings on one resource only; misses inherited org/folder bindings, group membership, deny policies and child-resource grants |

**The limit on permissions per call is not documented, and the plan must not claim it is.** No maximum
appears in the v1 or v3 references, the IAM testing guide, four live discovery documents, or the
`google/iam/v1` proto — only the wildcard restriction (*"Permissions with wildcards (such as `*` or
`storage.*`) are not allowed"*). The "100" that surfaces in search results belongs to a **different**
method, `queryTestablePermissions.pageSize` (*"The default is 100, and the maximum is 1,000"*). The
implementation therefore **chunks at a tunable size, tolerates `INVALID_ARGUMENT`, and halves on
failure** — which is what you do about an undocumented limit rather than pretending to know it.

### 8.1 What layer 1 cannot reach, and therefore what layer 3 must assert

In the order they undermine the gate:

1. **IAM inherits downward only**, so a clean *project-level* answer **does not prove** the principal
   lacks write on a child resource — one Cloud Run service, one subscription, one managed zone. And
   **Cloud SQL, Cloud Monitoring and log-entry reads have no per-resource `testIamPermissions` at
   all**, so the gap cannot be closed by enumeration. **This is the single biggest limitation and the
   primary reason the configuration assertion still exists.**
2. `cloudsql.flags.list` **does not exist as a permission**; the `flags` endpoint is project-less and
   gated purely by OAuth scope. There is nothing to assert against — layer 2's business.
3. **Deny policies**: whether `testIamPermissions` subtracts them is **`[UNVERIFIED]`** — not stated
   anywhere. This fails in the **safe** direction (a deny-blocked write may still be reported as held,
   so the integration refuses to start: a false positive, not a false negative).
4. **Conditional bindings**: **`[UNVERIFIED]`** — the method takes no request context, so a
   time- or attribute-conditioned write grant may evaluate differently at test time than at use time.
5. **PAB policies and VPC Service Controls** — not documented as evaluated; VPC-SC is not IAM at all.
6. Everything that is not GCP IAM: in-database `GRANT`s, Workspace OAuth scopes (§9), API keys,
   resources in other projects.

So the honest claim, and the one `docs/connectors/gcp.md` will make: **the gate is a zero-cost,
high-value tripwire that reliably catches the common failure — someone binding `roles/editor` to the
connector's service account at project level — and it is not a proof.** FR-004's clause about naming
the part it could not verify and requiring an operator assertion is not a hedge; it is the only
truthful design.

### 8.2 The roles, and two that disqualify themselves

Extracted from the live role/permission tables:

| area | role | perms | clean? |
|---|---|---|---|
| Cloud Run | `roles/run.viewer` | 50 | **yes** — all `get`/`list`/`getIamPolicy`. `run.routes.invoke` and `run.jobs.run` live in `roles/run.invoker`, not here |
| Cloud Logging | `roles/logging.viewer` | 28 | **yes** |
| Cloud Monitoring | `roles/monitoring.viewer` | 35 | **yes** — has `timeSeries.list` and `alertPolicies.get/list`, and **not** `timeSeries.create` |
| Cloud DNS | `roles/dns.reader` | 22 | **yes** |
| Compute (LBs) | **`roles/compute.viewer`** | 424 | **yes** — and see below |
| Cloud SQL | `roles/cloudsql.viewer` | 67 | **NO — a custom role is required** |
| Pub/Sub pull | — | — | **NO — no read-only pull exists** |

Three findings here are worth more than the table:

**Cloud SQL's viewer role fails FR-003.** `roles/cloudsql.viewer` contains
**`cloudsql.instances.export`** and **`cloudsql.backupRuns.export`**, which authorize a long-running
export that writes a dump to a GCS bucket — data egress with a side effect — plus
`cloudsql.instances.preCheckMajorVersionUpgrade`. There is **no more minimal Cloud SQL predefined read
role**. So the GCP feeder requires a **custom role** with `cloudsql.instances.get`/`list` and
`cloudsql.databases.get`/`list`. This is *stricter* than FR-003's "Viewer-class roles are the ceiling",
not looser — a custom role sits below the ceiling — so it complies, and `docs/connectors/gcp.md` ships
the role definition so Dana does not have to derive it.

**For load balancers, the broader-sounding role is the cleaner one.** `roles/compute.networkViewer`
(340 perms) contains **`trafficdirector.networks.reportMetrics`**, which writes metrics to Traffic
Director. `roles/compute.viewer` (424 perms) does **not**, and its only non-`get`/`list` entries are
non-mutating (`compute.urlMaps.validate`, `compute.instances.troubleshoot`, `getEffectiveFirewalls`,
`getSerialPortOutput`). So **prefer `roles/compute.viewer`** — a conclusion nobody reaches by reading
role names.

**The classifier must not be a substring match.** A rule over `create|update|delete|write|set|insert`
misfires on `cloudasset.assets.export*` (the `export*` family is in the one Asset Inventory role you
want), `compute.urlMaps.validate`, `serviceusage.values.test`, `pubsub.schemas.validate`,
`cloudsql.instances.preCheckMajorVersionUpgrade` and `*.listEffectiveTags`. Hence the explicit
read-verb allowlist, the explicit denylist, and failing closed on the unrecognised.

### 8.3 How the read-only statement must be worded

§5 established that **no read-only Pub/Sub pull exists at any granularity**, because
`pubsub.subscriptions.consume` authorizes `pull`, `acknowledge`, `modifyAckDeadline` and `seek`
together. FR-006 describes the exception as the acknowledgement; the accurate statement is that
**holding `consume` at all is the exception**, and the published read-only statement says so. The
binding is **resource-level on the one operator-created subscription**, never project-level, and the
integration runs correctly with the doorbell absent — which remains the strongest mitigation
available, because it means the grant need not exist at all.

**Alternatives considered.** *Policy Troubleshooter or Asset Inventory as the gate* — disqualified by
privilege cost above. *Trusting a documented role list without testing* — rejected: FR-004 requires
asking, and §8.2 is the evidence that role names mislead. *Skipping the gate because it is not a proof*
— rejected: it catches the failure that actually happens.

---

## §9. Reading the mailbox without altering message state

**Decision.** The mailbox is one `pkg/feeder.Source` behind the notice feeder's source seam, with two
supported paths, and **IMAP is preferred where it is available**:

| path | mechanism | read-only guarantee |
|---|---|---|
| **IMAP** (preferred) | `EXAMINE` **and** `UID FETCH … (BODY.PEEK[])` | **protocol-level**, per mailbox |
| **Gmail API** | service account with domain-wide delegation impersonating the mailbox's own **user account**, scope `gmail.readonly` **alone** | **structural** (the scope cannot reach `messages.modify`), but the grant is **tenant-wide** |
| **Microsoft Graph**, if the mailbox is in M365 | app-only `Mail.Read`, scoped by **RBAC for Applications** | structural, and scoped per mailbox |

**Rationale — IMAP, and why both commands are needed.** RFC 9051 §6.4.5 is explicit: for
`BODY[<section>]` *"The \Seen flag is implicitly set"*, and `BODY.PEEK[<section>]` is *"An alternate
form of BODY[<section>] that does not implicitly set the \Seen flag."* And §6.3.3: *"the selected
mailbox is identified as read-only. **No changes to the permanent state of the mailbox, including
per-user state, are permitted**"*, with the tagged OK required to begin `[READ-ONLY]`.

The subtlety that makes **both** necessary is §6.3.2: *"Read-only access through SELECT differs from
the EXAMINE command in that **certain read-only mailboxes MAY permit the change of permanent state on
a per-user (as opposed to global) basis**."* `\Seen` is exactly per-user state. So a `SELECT` that
reports `[READ-ONLY]` may **still** allow `\Seen` to be set. `EXAMINE` forbids it, and `BODY.PEEK`
never requests it. The feeder asserts `[READ-ONLY]` in the tagged OK **before** fetching and aborts if
it is absent — which is FR-007's *"MUST use a mode that does not, or MUST NOT read at all"*,
implemented rather than promised. `RFC822.*` and `BINARY[...]` behave like `BODY[]` and are never used.

**Rationale — Gmail, and the two things that had to be discovered.** `gmail.metadata` cannot read
bodies (*"but not the email body"*), and the feeder must extract windows from body text, so
**`gmail.readonly` is the minimum that works**; both are equally Restricted scopes, so choosing
`metadata` buys no review relief, only less exposure. `users.messages.get` accepts `gmail.readonly`
while `users.messages.modify` accepts only `mail.google.com/`, `gmail.modify` and
`gmail.modify.restricted` — and `UNREAD` is a **label**, changed only by `modify`/`batchModify`. So a
token bearing `gmail.readonly` **structurally cannot** mark a message read. That conclusion is
**`[UNVERIFIED]` as a quoted sentence** — no Google page says "get does not mark as read" — and is
inferred from the scope tables, where the inference is airtight for the read-only case.

Two structural findings matter more than the scope choice, and both are **`[UNVERIFIED]` as explicit
denials** while being well corroborated:

- **A Google Group cannot be read by any API.** Domain-wide delegation cannot impersonate a group (the
  JWT `sub` must be a real user's primary address), `groupsmigration` is insert-only, and the Groups
  Settings API covers settings. So **if the vendor notices currently land in a Google Group, the
  campaign needs a migration or a forwarding rule into a mailbox user** — and that is a prerequisite,
  not an implementation detail.
- **Gmail mailbox delegation is not exercisable via the API.** `userId` is *"The user's email address.
  The special value `me`…"*, with no delegate-acting parameter; the API can *manage* delegates but not
  act as one.

So the only working Gmail design is a service account with DWD impersonating a **real user account**
that is the shared mailbox. Its residual risk must be stated plainly in
`docs/connectors/vendor-notice.md`: **the DWD grant is tenant-wide.** Registering `gmail.readonly`
lets that service account read *every* mailbox in the domain, and the restriction to one mailbox lives
entirely in the connector's choice of subject — a weaker control than IMAP's per-mailbox credential or
Graph's App RBAC. The compensating controls are a **hard-coded single allowed subject** and
audit-log alerting on the service account. This asymmetry is the reason IMAP is preferred.

**Microsoft Graph**, if the mailbox is there: `Mail.Read` (application) is the minimum that reads
bodies — `Mail.ReadBasic.All` excludes *"body, previewBody, attachments and extensions"* — and note
the naming trap that **`Mail.Read` is *more* privileged than `Mail.ReadBasic`**. `isRead` changes only
by `PATCH`, which `Mail.Read` cannot perform. App-only grants reach every mailbox by default and are
narrowed by **RBAC for Applications**, not by the legacy `New-ApplicationAccessPolicy` that Microsoft
documents as superseded; ordinary Exchange RBAC management scopes do **not** constrain Graph app-only
access. Shared mailboxes are real mailbox objects in Exchange Online, so `/users/{shared}/messages`
works with no impersonation — a genuine architectural advantage over Workspace.

**This settles the design without settling FR-132.** Which mailbox, whose, and by which path remains
the organisation's to answer (C4); what the plan owes is the **guarantee each path can make**, and
that is now established for all three.

**Alternatives considered.** *POP3* — rejected: retrieval is destructive by design. *A forwarding rule
into a purpose-built feed* — viable and worth proposing to whoever owns the mailbox, since it removes
the shared-mailbox access question entirely; recorded as an option, not assumed. *`gmail.metadata`
only* — rejected: it cannot read bodies, so the extraction in FR-071 is impossible.

---

## §10. Status pages, changelogs, and machine-readable windows

**Decision.** Per-vendor **adapters behind a capability probe**, normalising to one internal
`{vendor, product, kind, start, end, status, objects[]}` shape. Three first-class structured sources,
in descending order of reliability:

1. **Atlassian Statuspage** `/api/v2/scheduled-maintenances.json` and `/api/v2/incidents.json`, keyed
   on **`scheduled_for` / `scheduled_until`** (RFC 3339 UTC), with `status` ∈ {`scheduled`,
   `in_progress`, `verifying`, `completed`}. `/api/v2/summary.json` is the efficient single poll.
2. **Vendor-native JSON**, e.g. Google Cloud's `https://status.cloud.google.com/incidents.json`, whose
   `begin` / `end` are RFC 3339 with explicit offsets, alongside `service_name`, `affected_products`
   and `status_impact`.
3. **RFC 9745 `Deprecation` and RFC 8594 `Sunset` response headers**, recorded by the integration's
   **own HTTP client** from every vendor API it already calls, and fed into the same pipeline.

Everything else is best-effort extraction into the same shape, and an HTML-scraping adapter is a
first-class citizen rather than a special case.

**Rationale.** Statuspage is the de-facto standard and gives exactly the two instants the announced
fact needs, which is why it is first. But **coverage is not universal and must be probed, not
assumed**: `githubstatus.com`, `status.datadoghq.com` and `status.openai.com` answer 200, while
`status.stripe.com` and `status.gitlab.com` **404** (moved off Statuspage) and
`status.cloud.microsoft` returns **401**. One Statuspage client would therefore silently fail for a
meaningful fraction of the allowlist — and a source that is unreachable must produce a **gap in the
checkpoint, not silence** (FR-075), so a probe that mistakes "not Statuspage" for "nothing announced"
would defeat the requirement.

The `Deprecation`/`Sunset` decision is the one worth arguing for, because it is nearly free. RFC 9745
is **Standards Track (March 2025)** and RFC 8594 is Informational (2019), and §4 of the former
constrains the pair: *"The timestamp given in the Sunset HTTP header field MUST NOT be earlier than
the one given in the Deprecation header field."* The deprecation instant therefore arrives **in band,
on responses the integration already makes**, with no feed to poll, no allowlist entry and no
credential. For the deprecation third of this feeder it is the only standardised source that exists,
and wiring it into the shared HTTP transport is a few lines.

**One standard is deliberately not used, and the reason is a rule rather than a limitation.**
`draft-gunter-calext-maintenance-notifications` wraps a maintenance window in an RFC 5545 `VEVENT`
with `DTSTART`/`DTEND` plus `X-MAINTNOTE-*` properties, and it has real traction among transit and
infrastructure providers who **attach a conforming `.ics` to the maintenance email** — which is
exactly the mailbox this feeder reads. Even bare `VEVENT` `DTSTART`/`DTEND` from a plain
`text/calendar` attachment would give a machine-readable window for very little code.

**FR-072 forbids it: "Attachments MUST NOT be opened or stored in v1."** So the richest structured
source available by email is out of scope by rule, and this feature extracts from body text instead.
That is recorded here rather than quietly worked around, because it is a real cost and a defensible
choice: an attachment parser is a parser for arbitrary third-party binary content sitting behind the
untrusted-input boundary of FR-073, and v1 declines that surface. If a later increment wants it, the
requirement is the thing to change, with its own decision record — and
`networktocode/circuit-maintenance-parser` is the reference implementation worth mining for
per-provider rules when that happens. The draft's own status is a secondary caution: intended status
Experimental, expired since 2020, never adopted by the `calext` working group.

**Alternatives considered.** *One Statuspage client for every vendor* — rejected by the 404s above.
*HTML scraping only* — rejected as a default: it discards machine-readable windows where they exist,
and `scheduled_for`/`scheduled_until` are exactly the fields FR-062 needs. *Requiring vendors to be on
Statuspage to be allowlisted* — rejected: the allowlist is drawn from the providers the organisation
actually uses, and the audit's most repeated cause arrived by email from one of them.

---

## §11. The two organisational inputs (FR-131, FR-132)

**Decision.** Neither blocks the code. Both are recorded as **campaign** blockers (Complexity Tracking
C3, C4), and the plan proceeds under a stated default for the first.

**FR-131 — project and region scope.** The default the design proceeds under: **the production project
only**, widening to production plus staging once the sanitisation manifest process has run once
end-to-end. The rationale is FR-010's: two projects with the same service name are different entities,
and the resolution rules for that are more safely exercised after the corpus exists than during its
first recording. **The choice is the organisation's**, and the design is scope-agnostic: FR-009 makes
scope configuration that is **recorded in every checkpoint**, so a query can tell "not present" from
"not in scope at that time", and FR-009 also requires the scope be changeable **without losing
history**.

**FR-132 — the mailbox.** §9 establishes what guarantee each access path can make without knowing
which mailbox it is. What the organisation must supply is: which mailbox, its named owner, the
authorised access path, and — **if the notices currently arrive in a Google Group** — a migration or
forwarding rule into a mailbox user, because §9 found no API can read a group's archive. All of it is
recorded in the campaign record together with who authorised it.

**Rationale.** FR-130 is the governing requirement and it points the other way from "wait for the
answers": **recording MUST begin as soon as the credential and mailbox access exist, before the
campaign scope is finally agreed**, because neither topology history nor an announcement stream can be
reconstructed retrospectively. Blocking the plan on FR-131 would destroy the very history the feature
is for. So the design records the scope in force throughout and moves.

**Alternatives considered.** *Blocking the plan until both are answered* — rejected by FR-130.
*Choosing every project in the organisation as the default* — rejected: it maximises quota pressure and
sanitisation surface on the first campaign, which is the run most likely to need re-doing.

---

## §12. Summary of assumptions overturned

Four things this feature's spec or plan assumed turned out otherwise, and each changed a design:

| assumed | actually | consequence |
|---|---|---|
| alerting incidents have no read-only API (plan's largest stated risk) | `projects.alerts.list`/`.get` exist and are documented read-only, with `openTime`/`closeTime` — but **Public Preview**, and bound only in a **deprecated** Go module | SC-004 is achievable; the dependency sits behind a capability flag with a `NO_DATA` degradation path (§5) |
| FR-006: the doorbell's exception is the **acknowledgement** | `pubsub.subscriptions.consume` authorizes pull, ack, `modifyAckDeadline` **and** seek together; `roles/pubsub.viewer` cannot pull; **no read-only pull exists at any granularity** | the exception is **holding the grant**, stated that way, resource-level on one subscription (§5, §8.3) |
| FR-003: a Viewer-class role is the read-only ceiling per area | `roles/cloudsql.viewer` contains `cloudsql.instances.export`; `roles/compute.networkViewer` contains `trafficdirector.networks.reportMetrics` | Cloud SQL needs a **custom role**; load balancers use the broader-but-cleaner `roles/compute.viewer` (§8.2) |
| FR-148: the vendor may report remaining quota | GCP reports none in band, for any of the four APIs | self-tracked token buckets gate; `serviceruntime` reconciles; each figure labelled (§7) |

And two gaps in the **published contract** were found by reading the shipped proto against the
requirements rather than by asking a vendor: no field for the vendor's request identifier (FR-100), and
no outcome for a window older than retention (FR-104). Both are plan items 6 and 7.
