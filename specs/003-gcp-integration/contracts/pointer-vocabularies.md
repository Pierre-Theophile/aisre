# The GCP Pointer Vocabularies

**Feature**: 003-gcp-integration | **Date**: 2026-09-20 | Requirements: FR-079–FR-085

A pointer says **where to look**, never what was measured (constitution IV). Feature 001 published
three vocabularies — `otel-semconv/1.30`, `k8s-resource/v1` and `url` — and
[`docs/schema/pointers.md` §Adding one](../../../docs/schema/pointers.md) states that a new
vocabulary is **a schema change**: version it in the name, document its selector grammar, and say
why the pointer could not be expressed in `otel-semconv`.

This feature adds **three**. They are item 1 of the plan's
[§What feature 001 still owes this feature](../plan.md#what-feature-001-still-owes-this-feature) and
are the first thing to land, because the telemetry backend cannot execute a pointer whose vocabulary
is not registered.

---

## 1. The split every GCP pointer makes

FR-080 and FR-081 together say something that looks contradictory until you see the two halves:

| part of the pointer | vocabulary | why |
|---|---|---|
| `selector` | **GCP's own query language** | it is what will actually be **executed**. A selector translated into OTel and back is a selector that can silently stop matching |
| `attributes` | **OpenTelemetry semantic conventions** | so a reader, or a future backend, can recognise **which entity** the pointer is about without understanding GCP's grammar |

So `Pointer.attributes` on a Cloud Run service pointer carries `service.name`,
`cloud.region`, `cloud.account.id`, `cloud.platform = "gcp_cloud_run"` — OTel throughout — while
`Pointer.selector` carries a Monitoring filter or a Logging query. The entity is portable; the query
is not, and pretending otherwise is the failure mode constitution IV's "document why" clause exists
to catch.

Two further rules, both easy to violate by accident:

- **Versioned in valid time with the node** (FR-083): asking for a service's pointers as of an
  earlier instant returns the selectors that were true then.
- **Additive** (FR-084): a GCP pointer never replaces or suppresses a pointer another source
  attached to the same entity.

And a pointer carries **no credential**, and no account or organisation identifier beyond the
project identifier its own selector requires — including a console deep link, which carries no
credential either (FR-082).

---

## 2. `gcp-monitoring-filter/v3` — metric pointers

**Backend kind**: `gcp-monitoring` | **Executed by**: `projects.timeSeries.list`, Monitoring API v3

### 2.1 Why this and not PromQL or MQL

Google offers three read vocabularies and research §3 settles the choice:

| candidate | status | verdict |
|---|---|---|
| **monitoring filter + `Aggregation`**, API v3 | GA; on an explicitly versioned Google surface; bound in the actively developed first-party GAPIC `cloud.google.com/go/monitoring/apiv3/v2` | **chosen** |
| **PromQL**, API **v1** | current and Google's recommended *language*, but served on v1, and *"the Prometheus HTTP endpoints aren't available in the Cloud Monitoring language-specific client libraries"* — no first-party Go binding — and its semantics are governed by upstream Prometheus rather than by a Google-versioned contract | rejected for a **stored** selector |
| **MQL**, `timeSeries.query` | **deprecated**: the API reference says *"We recommend using PromQL instead of MQL"*, the Go binding is marked deprecated, support for writing it ended 2025-07-22, and console creation is gone. Not shut down, but not a thing to mint new pointers in | rejected |

The deciding criterion is the one FR-083 implies: a pointer is **stored** and read back years later.
A selector's vocabulary therefore has to be stable and versioned, not merely current — and the
filter language is the only one of the three carried on a versioned surface with a maintained
first-party client.

**The one honest caveat**: the filter language is **not independently versioned**. It is versioned
only by the enclosing API surface, which is why the registered name is `…/v3` — the API version *is*
the vocabulary version, and the pointer records it.

### 2.2 Grammar

Selector = a Monitoring filter, plus the aggregation the digest needs:

```
metric.type = "run.googleapis.com/request_count"
  AND resource.type = "cloud_run_revision"
  AND resource.labels.project_id = "<project>"
  AND resource.labels.location = "<region>"
  AND resource.labels.service_name = "<service>"
```

Operators: `=` `!=` `>` `<` `>=` `<=` `:` (substring / key existence), `AND` `OR` `NOT`, and the
right-hand functions `starts_with()`, `ends_with()`, `has_substring()`, `one_of()`,
`monitoring.regex.full_match()`. Filterable fields: `project`, `metric.type`, `metric.labels.*`,
`resource.type`, `resource.labels.*`, `group.id`, `metadata.system_labels.*`,
`metadata.user_labels.*`.

> **`resource.type` is mandatory in every selector this feature mints, and that is a correctness
> rule.** `run.googleapis.com/request_count` and `request_latencies` are written against **both**
> `cloud_run_revision` **and** `cloud_run_instance`, and `cloud_run_instance` has labels
> `project_id`, `instance_name`, `location` — **no `revision_name`**. A selector that does not pin
> `resource.type = "cloud_run_revision"` will silently pick up series carrying no revision, and
> `errors_by_version` will then merge or drop groups without saying so. FR-089 and SC-002 both
> depend on this one clause.

### 2.3 The published signals

| signal | metric type | how the digest derives it |
|---|---|---|
| request rate | `run.googleapis.com/request_count` (DELTA, INT64) | `ALIGN_RATE` |
| **error rate** | `run.googleapis.com/request_count` | grouped by `metric.labels.response_code_class` (`2xx`/`4xx`/`5xx`) — **there is no dedicated Cloud Run error metric**, so this is derived, and the digest says so |
| latency | `run.googleapis.com/request_latencies` (DELTA, DISTRIBUTION, ms) | `ALIGN_PERCENTILE_99/95/50` |
| Cloud SQL | the instance's CPU, memory and connection metrics on `cloudsql_database` | at least one metric pointer per instance (FR-079) |

**A caveat that must reach the coverage block, not a code comment.** `request_count` *"excludes
requests that are not reaching your container instances (for example, unauthorized requests or when
maximum number of instances is reached)"*. So an error-rate digest built on it **undercounts
front-end rejections** — the 401/403 at the ingress and the 429/503 at max-instances, which are
exactly the failures an incident is often about. Every digest derived from `request_count` states
this in its coverage, because a number that silently omits the interesting failures is worse than no
number.

Also recorded here because it bites at ranking time: `request_latencies` is measured *"from when the
request reaches the running container to when it exits… it does not include container startup
latency"*. Where cold start matters, `request_latency/e2e_latencies` is the metric — and it is
**BETA**, so the pointer that names it says so.

### 2.4 Join keys: which field plays which role

A pointer carries `join_keys`, a map from a **published role** to the field that plays it in *this*
vocabulary (ADR-0005 D6; [`docs/schema/pointers.md`](../../../docs/schema/pointers.md) holds the role
registry). Without it `errors_by_version` is not expressible at all: the worker refuses a pointer
that declares no `version` role rather than guessing at a tag, because splitting a metric by the
wrong field returns one merged group — a confident answer that the new revision is no worse than the
old.

The mapping is **derived from the `resource.type` the selector pins**, not passed in beside it, so a
pointer cannot declare a field its own query is unable to return:

| `resource.type` | `version` | `workload` | `pod` | `host` | `trace` |
|---|---|---|---|---|---|
| `cloud_run_revision` | `resource.labels.revision_name` | `resource.labels.service_name` | — | — | — |
| `cloudsql_database` | — | — | — | `resource.labels.database_id` | — |
| anything else, including an alert policy's own condition filter | — | — | — | — | — |

The spelling is the **filter field**, `resource.labels.revision_name` — not the bare `revision_name`
a returned series' label map is keyed by. Both exist and they are not interchangeable: one goes into
a query, the other reads a result, and a join key in the result's spelling names no field the filter
language has.

Every dash is a fact about GCP rather than a gap, and the reasons are recorded in
[`docs/schema/pointers.md`](../../../docs/schema/pointers.md#gcp-monitoring-filterv3--a-cloud-monitoring-filter):
Cloud Run's per-instance label lives only on `cloud_run_instance` (the resource every selector here
pins away from), Cloud Run is serverless so there is no host, a managed database has no deployed
version in its metric labels and no `service_name`, and this estate has no trace source (FR-085).
An **alert policy's condition filter** is the row that decides the general case: it is minted exactly
as the policy's author wrote it and may pin no `resource.type` at all, so it carries no join keys.

Two identities hold this together, and both are asserted by tests rather than by review:

- `resource.labels.revision_name` is the field the feeder mints as `join_keys["version"]` **and** the
  one field [`gcp-telemetry-backend.md`](./gcp-telemetry-backend.md)'s `errors_by_version` groups by.
  Nothing at runtime compares the two, so a rename on either side would leave a pointer advertising a
  field the query never groups by — with one merged row as the only symptom;
- a term asking to split by any **other** field is **refused** rather than answered. The grouping is
  fixed at one field, so answering would report a breakdown by a field it was not grouped by.

### 2.4 Retention, which is a hole in the published outcome set

`run.googleapis.com/*` metrics retain for **6 weeks** (research §3). A pointer executed against an
older window returns nothing — and *"nothing"* here does **not** mean `NO_DATA`, because the window
was never covered. See [`gcp-telemetry-backend.md` §6](./gcp-telemetry-backend.md), where this is
recorded as a gap in the published outcome set rather than papered over.

---

## 3. `gcp-logging-query/v2` — log pointers

**Backend kind**: `gcp-logging` | **Executed by**: `logging.entries.list`, Logging API v2

Official name: the **Logging query language**. Like the Monitoring filter it is **not independently
versioned** — hence `…/v2`, the API surface's version.

```
resource.type = "cloud_run_revision"
  AND resource.labels.service_name = "<service>"
  AND resource.labels.location = "<region>"
  AND severity >= WARNING
```

Operators: `AND` `OR` `NOT` (and `-`), the comparisons, `:` (has / substring), and `=~` / `!~` for
RE2 regular expressions. Maximum filter length **20,000 characters**; `resourceNames` at most
**100** per request.

### 3.1 Why a log pointer is not OTel-expressible

`otel-semconv/1.30` is an **attribute-equality** vocabulary — *"key=value AND key=value"*
(`docs/schema/pointers.md`). The Logging query language is a query language: severity comparison
(`severity >= WARNING`), regular expressions (`=~`), field-existence tests, negation, and
parenthesised boolean structure. None of those is an attribute equality, and the pointers this
feature needs use them. Flattening a severity range into an equality set would change what the
pointer matches the next time Google adds a severity level.

This paragraph exists because constitution IV **requires** it: a pointer that cannot be expressed in
OTel semantic conventions must document why.

### 3.2 Join keys

The `cloud_run_revision` row of §2.4, spelled identically: a Cloud Run log entry carries
`resource.labels.revision_name` and `resource.labels.service_name` as resource labels, so both
`version` and `workload` are expressible on a log pointer here. That is a difference from the
Kubernetes feeder, whose log pointer declares no `version` because a Kubernetes log stream does not
carry one — the same role in the same position, expressible in one estate and not the other, which is
the whole reason the pointer has to say rather than the consumer assume.

### 3.3 The constraint that shapes `new_log_patterns`

**Cloud Logging has no server-side aggregation.** `entries.list` returns raw entries; the query
language has no `GROUP BY` and no `COUNT`. Research §6 establishes that the two server-side
alternatives are both **unavailable to a read-only connector**:

| alternative | why it is refused |
|---|---|
| **log-based metrics** | creating one is a **mutation** (`logging.logMetrics.create`), which FR-005 forbids outright. And it is **not retroactive** — *"calculated from logs received after creation"* — so it could not answer a question about a past incident even if writing were permitted |
| **Observability Analytics** (BigQuery-backed SQL) | requires **upgrading the log bucket**, a one-time mutation; entries written before the upgrade are unavailable until backfill; and the programmatic path bills BigQuery analysis |

So `new_log_patterns` mines templates **client-side over a bounded sample**, and the bound is
published rather than incidental. What that means for the digest — the sample size, the budget it
stopped at, and why this is stated in coverage rather than hidden — is
[`gcp-telemetry-backend.md` §4](./gcp-telemetry-backend.md).

`entries.tail` is **not** used: it is live-follow, bidirectional-streaming (gRPC only, not
available over REST), and it is the wrong primitive for a bounded digest over a past window.

---

## 4. `gcp-trace-filter/v1` — trace pointers, registered and unminted

**Backend kind**: `gcp-trace` | **Executed by**: Cloud Trace, where a trace data source exists

**The organisation has no tracing** (Assumptions). The vocabulary is registered anyway, and no
pointer is minted in it, because FR-085 requires exactly that distinction: a trace pointer must not
be minted **as if** a trace source existed, and the absence must be **a stated fact about the
entity** — so a consumer can tell *"no trace pointer because there is no tracing"* from *"nobody
wrote one"*.

Registering the name now is what lets `error_spans` become a live operation **without a contract
change** if Cloud Trace is ever enabled (FR-091, Assumptions). Until then the term is served,
answering `NO_DATA` with the absent source named.

---

## 5. `url` — the console deep link

Reused from 001, unchanged, for `SOURCE_LINK` pointers: a link back to the Cloud Run service, the
Cloud SQL instance or the alert policy in the GCP console. There is no OTel vocabulary for "a
console page", which is 001's already-documented reason this pointer kind does not use one.

The link **carries no credential** (FR-082). A console URL that embedded one would put it in the
graph, in every golden, and in every fixture in the public repository.

---

## 6. Registration

In `pkg/feeder/pointer.go`, beside the existing constants:

```go
// VocabGCPMonitoringFilter is a Cloud Monitoring filter plus its Aggregation, executed by
// projects.timeSeries.list on Monitoring API v3. The language is not independently versioned,
// so the name carries the API surface's version. Not otel-semconv-expressible: see
// specs/003-gcp-integration/contracts/pointer-vocabularies.md §2.
VocabGCPMonitoringFilter = "gcp-monitoring-filter/v3"

// VocabGCPLoggingQuery is the Logging query language, executed by logging.entries.list on
// Logging API v2. Not otel-semconv-expressible — severity comparison, RE2 and boolean
// structure are not attribute equalities: ibid. §3.1.
VocabGCPLoggingQuery = "gcp-logging-query/v2"

// VocabGCPTraceFilter is registered and deliberately unminted: this organisation has no trace
// data source, and FR-085 requires that absence be a stated fact rather than a fabricated
// pointer: ibid. §4.
VocabGCPTraceFilter = "gcp-trace-filter/v1"
```

Each gets its selector grammar in `docs/schema/pointers.md` and its "why not OTel" paragraph, which
is what makes it a schema addition rather than a string.

The backend **accepts the pointers the feeders emit without translation** (FR-111). A pointer it
cannot execute is reported `QUERY_FAILED / UNSUPPORTED_POINTER`, **naming the pointer kind and
vocabulary** — never executed as something else, which is the failure that would return a confident
answer to a question nobody asked.
