# The GCP Telemetry Backend Contract

**Name**: `gcp:<org-slug>` | **Vendor**: `gcp` | **Algebra version**: `1.0.0` | **Date**: 2026-09-20

This backend **implements** the published contract; it does not define one. The algebra, the digest
shapes, the coverage block, the join keys, the drill-down handles and the six typed outcomes live in
[`specs/002-investigation-engine/contracts/telemetry-backend.md`](../../002-investigation-engine/contracts/telemetry-backend.md)
and `api/sreagent/investigation/v1/investigation.proto` §1–3 (ADR-0005 D7, FR-087). Nothing below
restates them. What is here is only what is **GCP-specific**: which surface serves which term, how
each digest is derived, what coverage must say about GCP in particular, and the three places GCP does
not fit the contract cleanly.

> **Read the shipped proto, not the contract copy.** `api/.../investigation.proto` carries
> `Coverage.truncated_to_horizon` and `Coverage.horizon` (fields 14–15) that
> `specs/002-.../contracts/investigation.proto` does not. Both are **mandatory** for a horizon-clamped
> answer. See the plan's note on the divergence.

---

## 1. Declaration

```go
backend.Description{
    Name:           "gcp:" + orgSlug,
    Vendor:         "gcp",
    Terms:          []string{"compare", "onset", "new_log_patterns", "error_spans",
                             "errors_by_version", "monitor_state", "exemplars", "drill_down"},
    Capabilities:   …,   // §2, every one ReadOnly: true
    CostClasses:    …,   // §2
    Redaction:      …,   // sanitisation.md, version 1.0.0
    Version:        …,
    AlgebraVersion: "1.0.0",
}
```

**All eight telemetry terms, and nothing else** (FR-087). The graph family is the graph worker's,
answered from the replayed event log; the knowledge family is the knowledge worker's. A backend
declaring a term from either is **rejected at registration**, and so is one declaring a
state-changing capability (`write_capability`) or a term with no cost class.

---

## 2. Which GCP surface serves which term

| term | surface | cost class | window cap |
|---|---|---|---|
| `compare` | Cloud Monitoring `timeSeries.list` | `STANDARD` | one window pair |
| `errors_by_version` | Cloud Monitoring `timeSeries.list` | `STANDARD` | one window pair |
| `onset` | Cloud Monitoring `timeSeries.list`, **computed backend-side** | `EXPENSIVE` | the search window |
| `monitor_state` | Cloud Monitoring alerts (§5) | `CHEAP` | wide |
| `new_log_patterns` | Cloud Logging `entries.list` | `EXPENSIVE` | **the tightest** (§4) |
| `error_spans` | the trace source — **absent here** (§7) | `STANDARD` | n/a |
| `exemplars` | Cloud Logging, on explicit request only | `EXPENSIVE` | capped |
| `drill_down` | whichever surface minted the handle | `CHEAP` | the handle's |

Which surface serves which operation is published (FR-088) so that a reader of a digest knows what
was queried. Nothing outside this table is executed, and **caller-supplied query text is never
executed** — an out-of-algebra request is refused `QUERY_FAILED / OUTSIDE_ALGEBRA` naming what was
asked for **and** what is available (FR-087, SC-015).

---

## 3. Metric terms: server-side aggregation is the whole design

Cloud Monitoring's `timeSeries.list` takes `aggregation` and `secondaryAggregation`, each with
`alignmentPeriod` (minimum 60 s), `perSeriesAligner`, `crossSeriesReducer` and `groupByFields`. This
is what makes constitution IV natural rather than effortful: **the summary is computed in GCP and
the points never leave it**. A `compare` is two aligned-and-reduced queries, not a download and a
loop.

`MetricDigest` per series: identifying labels, point count, the interval actually covered, the
resolution GCP returned, statistics from the published `Statistic` set, and — for a before/after
request — an explicit `Comparison` with direction and magnitude (FR-094, FR-092). **No point-by-point
samples**, which the aggregation makes structurally true rather than a rule to remember.

Bounding facts that go in coverage: `pageSize` above 100,000 is coerced to 100,000; `orderBy` **must
be blank** and points come back most-recent-first; `view` is required (`FULL` | `HEADERS`); page
tokens live 24 hours.

### 3.1 `errors_by_version`

Grouped by the **Cloud Run revision** (FR-089), via `groupByFields: ["resource.labels.revision_name"]`
with `resource.type = "cloud_run_revision"` pinned in the selector — and that pin is a correctness
requirement, not a style one:
[`pointer-vocabularies.md` §2.2](./pointer-vocabularies.md) explains that the same metric is written
against `cloud_run_instance` too, which carries **no** revision label, so an unpinned query merges or
drops groups silently.

The revision values returned are **join keys matching the WORKLOAD nodes in the graph**
(`JoinKeys.version`), which is what lets "the new revision is erroring and the old one is not" be a
fact the digest can *state* rather than a comparison the reader has to make.

The error split comes from `metric.labels.response_code_class`. Two caveats belong in **coverage**,
not in a comment:

1. there is **no dedicated Cloud Run error metric**, so error rate is *derived* from `request_count`;
2. `request_count` **excludes requests that never reached the container** — unauthorized requests, and
   requests rejected at max-instances. So the digest **undercounts** exactly the ingress failures an
   incident is often about, and it says so every time.

### 3.2 `onset`

Served **backend-side** (FR-087a): Cloud Monitoring returns the series **to the backend process**,
which calls `pkg/backend/onset` — the shipped estimator, not a reimplementation — and returns only
`OnsetDigest`: the estimated instant, its uncertainty, the method and its parameters. **The raw
samples do not cross the digest boundary in either direction**, and appear in no response, exemplar
or coverage field.

The published effect-size and persistence criteria, the five `unavailable_reason` values and the
defaults (`k_effect = 3.0`, `min_sustained_points = 5`, …) are 002's and are used unchanged. A
crossing that does not clear the criterion is `unavailable` with `no_onset_detected` — **never** the
earliest crossing, because an instant that describes nothing would key every downstream
causal-ordering decision to noise.

---

## 4. `new_log_patterns`: bounded by necessity, and honest about it

**Cloud Logging has no server-side aggregation.** `entries.list` returns raw entries; the query
language has no `GROUP BY` and no `COUNT`. Research §6 establishes that both server-side alternatives
are unavailable to this backend:

- **log-based metrics** — creating one is a mutation, which FR-005 forbids, and they are **not
  retroactive**, so they could not answer a question about a past incident even if writing were
  permitted;
- **Observability Analytics** — requires **upgrading the log bucket**, also a mutation, with a
  backfill delay and BigQuery analysis charges on the programmatic path.

So templates are mined **client-side**, with feature 002's Drain-style miner, over a **bounded
sample**. The budget is published (`budget.md` §5) and the bound is enforced by the backend rather
than discovered by running out.

What that obliges the digest to do, and this is the part that makes it acceptable under principle V:

| coverage field | what it must say |
|---|---|
| `volume_considered` | how many entries were actually examined |
| `truncation` | that a sample bound was reached, **and which criterion** stopped it — entries, bytes or wall clock |
| `window_actually_covered` | the sub-window the sample actually spans, where pagination stopped early |

`LogDigest` carries masked templates with counts and each template's first-seen instant (FR-090,
FR-095) — never raw entries. The miner version is recorded, because a template set is a function of
the miner.

**One pagination rule is a correctness rule.** GCP's own reference says: *"If a value for
`nextPageToken` appears and the `entries` field is empty, it means that the search found no log
entries so far but it did not have time to search all the possible log entries."* So an empty page is
**not** `NO_DATA`. The backend must distinguish:

| observed | outcome |
|---|---|
| empty page **with** a `nextPageToken` | the search is unfinished — keep going, or report `PARTIAL` with what is missing named |
| empty page **with no** token, window outside the ingestion lag | `NO_DATA` |
| empty page, window **inside** the lag | `NOT_YET_INGESTED` |

A backend that reported the first as `NO_DATA` would tell an investigation that nothing happened
because it ran out of time.

`pageSize`'s maximum is **not documented** and the oft-cited 1,000-entry and 10 MB caps appear
nowhere in Google's reference (research §6). The backend requests a large page, accepts what comes,
paginates, and enforces its own budget — never designs against folklore.

---

## 5. `monitor_state`, and the one risky dependency in this feature

Research §5 **refuted** the assumption this feature was planned against: Cloud Monitoring *does*
expose alerting incidents to a public read API. `projects.alerts.list` / `.get` on Monitoring v3
return an `Alert` whose resource documentation states it *"is a read-only resource that cannot be
modified by the accompanied API"*, with exactly the fields FR-046 and FR-098 need:

| field | use |
|---|---|
| `name` | `projects/P/alerts/ALERT_ID`, system-assigned |
| `state` | `OPEN` \| `CLOSED` |
| **`openTime`, `closeTime`** | **the transition instants Google reports** — FR-046's valid time, and SC-004's exactness |
| `resource`, `metric` | the labels preserved from the generating condition — what the alert watches |
| `policy` | a `PolicySnapshot` of the policy as it was |

`orderBy` supports `open_time` and `close_time`, default `open_time desc`; `pageSize` default 50, max
1,000. Closed incidents retain **13 months**, open ones indefinitely, and an incident with no new data
auto-closes after **7 days**.

### 5.1 It is Public Preview, reached through a deprecated client

Two facts have to be stated together:

1. the API is **Public Preview**, under Pre-GA terms, and Google's own note warns *"the labels in the
   response are subject to change while this feature is in preview"*;
2. it is **not in the first-party GAPIC**. `cloud.google.com/go/monitoring/apiv3/v2` has no
   `ListAlerts`. The only Go binding is `google.golang.org/api/monitoring/v3`, whose package header
   says *"This package is DEPRECATED… maintenance mode"*.

A Preview API through a maintenance-mode client is the single riskiest dependency in this feature, and
this contract does not hide it. The mitigation is structural:

- incident reads sit behind a **declared capability flag**, so the backend and the feeder both run
  with it off;
- with it off, `monitor_state` still answers — `NO_DATA` with coverage **naming the absent source**,
  exactly as `error_spans` does (§7), which is the contract's own answer for a missing source;
- the alert **policy** half (`alertPolicies.list`) is GA in the GAPIC and is unaffected, so ALERT
  nodes exist either way;
- the fallback transport is the **Pub/Sub notification channel payload**, which carries
  `incident_id`, `state`, `started_at`, `ended_at`, `severity` and the resource — but is **push-only**
  and therefore *cannot answer a historical question*, so it is a supplement and never the source of
  truth.

One consequence for the read-only story, recorded because it is counter-intuitive: pulling a Pub/Sub
subscription requires `pubsub.subscriptions.consume`, which is **not in `roles/pubsub.viewer`**, and
acknowledging removes the message from the backlog. So the doorbell is **not** a read-only operation
by IAM or by semantics — which is exactly why FR-006 makes it the **one declared exception**, confined
to a subscription the operator created for it. The plan does not get to call it read-only; it gets to
call it declared.

`MonitorStateDigest` carries the transitions in the window with their instants, the state at the start
and end of the window, the per-group states where the policy has groups, and the transition count
(FR-098).

---

## 6. Retention: a hole in the published outcome set

`run.googleapis.com/*` metrics retain for **6 weeks** (research §3). Cloud Logging's `_Required`
bucket is 400 days and `_Default` is 30 days by default.

A window older than retention is **not** `NO_DATA`. `NO_DATA` means *"the query was valid, the window
was covered, and there is nothing in it"* — and the contract is explicit that only `NO_DATA` is
evidence that nothing happened (FR-104, §4 of the published contract). A window outside retention was
**never covered**, so reporting `NO_DATA` would tell an investigation that nothing happened during a
window nobody could look at. `NOT_YET_INGESTED` is the mirror image of this case — *too recent* — and
there is **no outcome for too old**, and no `FailureReason` for it either: the published set is
`REJECTED_BY_BACKEND`, `NOT_PERMITTED`, `RATE_LIMITED`, `TIMED_OUT`, `UNSUPPORTED_POINTER`,
`OUTSIDE_ALGEBRA`.

This is a **genuine gap in the published contract**, found by this feature and shared with 005 —
Datadog has retention too. It is recorded as item 7 of the plan's
[§What feature 001 still owes this feature](../plan.md#what-feature-001-still-owes-this-feature): one
additive `FailureReason.OUTSIDE_RETENTION`, carrying the retention horizon the vendor states.

**Until it lands**, the backend answers `QUERY_FAILED / REJECTED_BY_BACKEND` with the retention
horizon in `failure_detail` — a typed failure, not a `NO_DATA`. That is the conservative choice: it
makes the investigation say "I could not look" instead of "nothing was there", and it is the error
that fails safe.

---

## 7. `error_spans`: the absent source, answered properly

The organisation has **no trace data source**. The term is still served, and answered the way the
contract requires of **every** backend with a missing source (FR-091, FR-087b):

- outcome **`NO_DATA`**;
- coverage **names the absent source** — the trace data source is absent — **and states that nothing
  was searched**;
- **never** `QUERY_FAILED`, never an unexplained empty digest, never a substituted source, never a
  silent single group;
- **identical in live and recorded mode**, and **stable for the whole window**, so a consumer
  concludes *"this cannot be checked here"* rather than retrying.

The point is negative and it is the whole requirement: a consumer **must not** be able to read this
answer as evidence that there were no error spans (FR-091, SC-011). And because
`gcp-trace-filter/v1` is registered but unminted
([`pointer-vocabularies.md` §4](./pointer-vocabularies.md)), the term becomes live **without a
contract change** if Cloud Trace is ever enabled.

---

## 8. Coverage: what GCP must add

Every digest carries a coverage block or it is invalid. GCP-specific obligations on top of the
published fields:

| field | GCP source |
|---|---|
| `ingestion_lag_seconds` | for metrics, **`MetricDescriptor.metadata.ingestDelay`** — a documented API field: *"Data points older than this age are guaranteed to be ingested and available to be read"*. Fetched via `projects.metricDescriptors.get`, falling back to the per-metric documented default (120 s for Cloud Run) when the field is empty, **and saying which source it used**. For **logging** there is no equivalent field: the lag is estimated from `now − max(receiveTimestamp)` and is reported **as an estimate** |
| `ingestion_lag_undetermined` | true where neither path yields a figure — stated, never omitted |
| `sampling` | the `alignmentPeriod`, `perSeriesAligner` and `crossSeriesReducer` actually applied |
| `truncation` | the criterion, including §4's sample bound |
| `remaining_quota`, `quota_window_seconds`, `quota_undetermined` | **`quota_undetermined = true`** for every real-time answer: GCP reports no in-band remaining figure (`budget.md` §3). The `serviceruntime` figure is minutes-stale and is a reconciliation signal, not a coverage fact |
| `truncated_to_horizon`, `horizon` | from `internal/investigation/backend.ClampRequest` / `AnnotateHorizon` — **called, not re-derived** |
| `data_source` | which GCP surface and which log or metric scope |

`ingestDelay` is what makes `NOT_YET_INGESTED` a **contractual** boundary rather than a guess:

```
cutoff := now − ingestDelay
window.End >  cutoff, no points → NOT_YET_INGESTED
window.End <= cutoff, no points → NO_DATA
```

Two honest caveats: `ingestDelay` is a **static declared value per metric type**, not a live measure
of pipeline health; and whether it is populated for `run.googleapis.com/request_count` specifically
was **not verifiable without credentials** (research §4) — hence the documented fallback and the
"which source" statement.

---

## 9. Join keys

The published mapping is
[`data-model.md` §9](../data-model.md#9-join-keys-the-published-mapping): revision → `version`,
Cloud SQL instance connection name → `pod_or_host`, trace → `trace_ids` (empty here), first-seen →
`first_seen`, with the Cloud Run service in `workload`.

Sanitisation **pseudonymises these rather than dropping them** (FR-102) — the one place the keyed
HMAC's corpus-consistency is load-bearing. A digest whose keys do not join is evidence about nothing.

---

## 10. Two modes, one output

Recorded mode answers from a recorded world with **identical digests** to live mode for the same
request — coverage block and every join key included, not only the summary statistics — and makes
**no network call**, never falling through to a live call to satisfy a miss (FR-106, SC-014).
`not_recorded` is its own outcome for an in-algebra request the world does not hold (FR-105), and the
**miss rate** is published on every verification run against its threshold (FR-108).

`response_digest` is computed by `pkg/backend.ResponseDigest`, which calls
`pkg/backend.NormaliseForDigest` — **not** reimplemented — because the three cleared fields
(`response_digest`, `duration_ms`, `mode`) are what make live and recorded hash the same bytes.

A recorded window is a **world, not a trajectory** (FR-107): the union of every term the deterministic
engine issues for the fixture's own question and the cross product of the algebra over the alert
neighbourhood and the window grid, plus the depth-1 drill-downs those answers mint.

---

## 11. Read-only, and nothing created

Executing a term changes nothing in GCP and **creates no GCP object of any kind** — no saved query,
no log sink, no dashboard, no alerting policy, no analytics dataset or table (FR-112). This is why §4
refuses log-based metrics and Observability Analytics despite their being the technically superior way
to aggregate logs: both require creating something, and there is no configuration in this feature that
enables one.

Every execution draws on the **same budget as the feeder half** and is **reportable separately**
(FR-113), so Dana sees what investigations cost as distinct from what ingestion costs.
