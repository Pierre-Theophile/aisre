# Pointers: where to look

The graph does not store telemetry. It stores the sentence that finds it.

That is constitution IV, and it is the rule that decides what this project is. A metric sample,
a log line, a span or any aggregate of them is refused at ingestion with the offending field
named (FR-009, `reason_code: telemetry_payload`). What a node carries instead is a list of
**pointers**: for each one, the kind of telemetry, the backend that owns it, the vocabulary the
selector is written in, and the selector itself.

The payoff is that backends are interchangeable and the graph stays small. The cost is that a
pointer has to be precise enough to be executed by something that has never seen this graph —
which is what the rest of this document is about.

- The message is `Pointer` in `api/sreagent/graph/v1/graph.proto`.
- The query that reads them back is `QueryService.Pointers`, documented in
  [`queries.md`](queries.md#pointers).
- The helpers that mint them are `pkg/feeder/pointer.go`, which is what a connector author uses.

---

## The shape

```proto
message Pointer {
  PointerKind kind = 1;               // METRIC | LOG | TRACE | DASHBOARD | SOURCE_LINK
  string backend_kind = 2;            // "prometheus", "loki", "tempo", "k8s", "github", …
  string vocabulary = 3;              // "otel-semconv/1.30", "k8s-resource/v1", "url", …
  string selector = 4;                // the query text, in that vocabulary
  map<string, string> attributes = 5; // OTel resource attributes identifying the entity
  map<string, string> join_keys = 6;  // published role → the field that plays it here
}
```

All four scalar fields are required in practice: a pointer missing any of them cannot be
executed by a consumer that does not already know what it meant (FR-008).

`attributes` and `selector` are not the same thing and neither is redundant. The **selector** is
what you run; the **attributes** are what the pointer is *about*. They differ whenever the
selector is narrower or wider than the identity:

```
attributes: {deployment.environment.name: prod, k8s.namespace.name: shop, k8s.deployment.name: payments}
selector:   k8s.namespace.name="shop" AND k8s.deployment.name="payments"
```

The environment is in the attributes and not in the selector, because the Loki instance being
queried only holds production. A reader can still see which environment this pointer refers to
without the selector having to filter for it.

---

## Join keys: which field plays which role

`errors_by_version(pointer, window)` — "is the new version failing and the old one not" — is
expressible only if something says **which field carries the deployed version**. That field is
spelled differently in every vocabulary: `service.version` in OpenTelemetry semantic conventions,
`version` as a Datadog tag, `resource.labels.revision_name` in a Cloud Monitoring filter. The role
is the same; the spelling is the backend's.

So a pointer may carry a small map from a **published role** to the field that plays it here
(ADR-0005 D6). The role set is **closed** — five roles, and no more — because a set of roles anyone
may extend is not a vocabulary, it is a comment:

| Role | What plays it |
|---|---|
| `version` | the deployed version of the thing being observed |
| `workload` | the workload, deployment or service the telemetry belongs to |
| `pod` | the individual replica |
| `host` | the node or host the replica runs on |
| `trace` | the trace identifier, so an exemplar can be followed into a trace backend |

Three rules decide what a connector writes here, and each of them exists because of a specific way
this goes wrong:

1. **The spelling is the one that goes into the query, not the one that comes out of a result.**
   A Cloud Monitoring series' label map is keyed by the bare `revision_name`; the filter language
   has no such field, only `resource.labels.revision_name`. A join key in the result's spelling
   names nothing the query can group by.
2. **A role the pointer's backend cannot express is left out, never guessed.** A guessed role is
   the failure with no symptom: splitting a metric by a tag that does not exist returns one merged
   group, and one merged group is a confident answer that the new version is no worse than the old.
   An absence is honest; `pkg/feeder`'s `WithJoinKeys` silently drops any role outside the five, so
   the roles a connector declares are worth asserting in that connector's own tests, where a typo
   is still visible.
3. **The role a feeder declares and the field its backend groups by are one string.** Nothing at
   runtime compares them. Where both ends live in this repository, a test asserts the identity
   rather than a shared variable carrying it, so that each package keeps its own grammar and a
   rename fails loudly.

An empty map is omitted by canonical serialisation exactly as every other empty map is, so a
pointer with no join keys produces a byte-identical golden — which is what let this field be added
to a shipped schema additively. The recordings made *before* the field are replayed with
`feeder.PointerCompatNoJoinKeys` and keep reproducing themselves; every recording made since
carries the keys.

Which roles each vocabulary can express is stated with that vocabulary below.

---

## Kinds

| Kind | What it selects | Typical backends |
|---|---|---|
| `METRIC` | one instrument of one entity | `prometheus`, `datadog` |
| `LOG` | the log stream of one entity | `loki`, `elasticsearch` |
| `TRACE` | the spans of one entity | `tempo`, `jaeger`, `datadog` |
| `DASHBOARD` | a prepared view of one entity | `grafana`, `datadog` |
| `SOURCE_LINK` | the entity in the system the feeder read it from | `k8s`, `github` |

`SOURCE_LINK` is the odd one and it earns its place: it is how an operator gets from a node back
to the object that produced it — the Kubernetes resource, the repository, the team page. It is
also the only kind that is not telemetry at all, which is why its vocabulary is usually not
OpenTelemetry semantic conventions.

A pointer whose `kind` a source left unset is **not dropped**: the query groups it under
`POINTER_KIND_UNSPECIFIED`. A selector nobody classified is still a place to look.

---

## Backend kinds seen today

The reference feeders emit four, and all four are configurable rather than hard-coded, because
which backend holds the logs is a deployment fact and not a schema fact:

| Backend kind | Emitted by | Configured with |
|---|---|---|
| `tempo` | OpenTelemetry topology feeder | `--trace-backend` |
| `prometheus` | OpenTelemetry topology feeder | `--metric-backend` |
| `loki` | Kubernetes feeder | `--log-backend` |
| `k8s` | Kubernetes feeder | — (it is the API server the feeder reads) |

`github` appears in the hand-authored fixtures on owner nodes (`orgs/shop/teams/team-data`), as
the shape a source link to a non-Kubernetes system takes.

`backend_kind` names a **family**, never an instance. There is no URL in a pointer and no
tenant: "which Prometheus" is a deployment concern that belongs to whatever executes the
pointer, not to a graph that is supposed to outlive three monitoring migrations.

---

## Vocabularies

### `otel-semconv/1.30` — the canonical one

An attribute selector in OpenTelemetry semantic conventions:

```
key="value" AND key="value" [AND metric.name="…"]
```

Values are quoted and backslash-escaped; keys appear in the order the emitter named them, or
sorted when it named none, so the same facts always render the same bytes. This is the
vocabulary that makes backends interchangeable — every one of them can translate an attribute
equality into its own query language — and constitution IV requires a pointer that *cannot* be
expressed in it to document why.

`metric.name` is the one key in a selector that is not a resource attribute. It names the
instrument rather than the entity, so it belongs in the selector and never in `attributes`.

```
TRACE   tempo       service.name="payments" AND service.namespace="shop"
METRIC  prometheus  service.name="payments" AND service.namespace="shop" AND metric.name="http.server.request.duration"
LOG     loki        k8s.namespace.name="shop" AND k8s.deployment.name="payments"
```

**Join roles.** `version` → `service.version`, `workload` → `k8s.deployment.name`. The OTel feeder
mints both on its trace and metric pointers; the Kubernetes feeder's log pointer declares `workload`
and `pod` → `k8s.pod.name` and **no `version`**, because the deployed version is not an attribute a
Kubernetes log stream carries. Same vocabulary, different roles: what a role maps to depends on what
the *selector's own source* can return, not on what the vocabulary could in principle spell.

### `k8s-resource/v1` — a resource path

A Kubernetes API path identifying one object, used for `SOURCE_LINK`:

```
apps/v1/namespaces/shop/deployments/payments
v1/namespaces/shop/configmaps/checkout-config
v1/nodes?labelSelector=sre.node_pool%3Dgeneral
```

The last one is a node pool, which is not a Kubernetes object but a label over nodes, so the
pointer is a list query rather than a path. The label key is whatever `--node-pool-labels` was
set to (`sre.node_pool` first, then `node-pool`), so a golden that records the selector records
the deployment's configuration along with it.

### `url` — a dashboard

There is no OpenTelemetry vocabulary for "a dashboard", which is the documented reason
`DashboardPointer` does not use one. The selector is the dashboard's URL or identifier in its
backend.

**Join roles: none.** A URL has no tags for a role to name. The same is true of `k8s-resource/v1`
above and of every `SOURCE_LINK`: they address an object, they do not return series to be grouped.

### `github-resource/v1` — a GitHub REST path

**Backend kind** `github` · **kind** `SOURCE_LINK` only · added by feature 004

A path under GitHub's REST API identifying one object, **without the host**:

```
repos/acme/storefront/deployments/1042
repos/acme/storefront/deployments/1042/statuses
repos/acme/storefront/actions/runs/778812
repos/acme/storefront/releases/55
```

Without the host on purpose. A pointer is stored and read back years later, and a GitHub Enterprise
Server installation serves the same paths under a different host — so putting the host in the
selector would make one estate's pointers unreadable against another's, for a fact that belongs to
the connector's configuration rather than to the object.

### `vercel-resource/v1` — a Vercel REST path

**Backend kind** `vercel` · **kind** `SOURCE_LINK` only · added by feature 004

A path under Vercel's REST API identifying one object, **with its API version** and without the host:

```
v13/deployments/dpl_9aBc
v9/projects/prj_7xYz
v9/projects/prj_7xYz/env
```

With the version, unlike GitHub's, because Vercel versions per path and serves the same object at
several versions with different shapes. A selector that dropped it would not identify what was read.

### Why neither needs a constitution IV justification in the usual form

Constitution IV requires a pointer that cannot be expressed in `otel-semconv/1.30` to document why.
For a metric or a log pointer that is a real question, and §`gcp-monitoring-filter/v3` below answers
it at length. For these two the question **does not arise in that form**, and saying so is the
answer:

they do not select a **series**. They address one object by its place in a REST API. There is no
attribute equality that means "the deployment with id 1042 in this repository", because
`otel-semconv` is a vocabulary of attributes on telemetry and a deployment is not telemetry.
Inventing one would produce a selector no backend could execute — which is the failure the "document
why" clause exists to catch, arriving from the opposite direction.

It is the same argument `k8s-resource/v1` and `url` already rest on. And unlike the three GCP
vocabularies, these need no split between an untranslatable selector and portable attributes: a
`SOURCE_LINK` carries no query, so there is nothing for a future backend to have to recognise.

**Join roles: none**, for both. A join key says which attribute of a series plays which role, and a
source link selects an object — a deployment has no version attribute to group by, because it *is*
the version.

### `gcp-monitoring-filter/v3` — a Cloud Monitoring filter

**Backend kind** `gcp-monitoring` · **executed by** `projects.timeSeries.list`, Monitoring API v3 ·
added by feature 003
([contract](../../specs/003-gcp-integration/contracts/pointer-vocabularies.md))

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

**Why not `otel-semconv`.** The filter language carries right-hand functions, the substring and
key-existence operator `:`, and an `Aggregation` that is part of what the pointer selects rather than
a property of the entity. None of those is an attribute equality.

> **`resource.type` is mandatory in every selector, and that is a correctness rule.**
> `run.googleapis.com/request_count` and `request_latencies` are written against **both**
> `cloud_run_revision` **and** `cloud_run_instance`, and `cloud_run_instance` carries labels
> `project_id`, `instance_name`, `location` — **no `revision_name`**. A selector that does not pin
> `resource.type` silently picks up series with no revision, and `errors_by_version` then merges or
> drops groups without saying so.

**Join roles**, and they depend on the `resource.type` the selector pins — which is the reason the
mapping is derived from that pin rather than passed in beside it:

| `resource.type` | `version` | `workload` | `pod` | `host` | `trace` |
|---|---|---|---|---|---|
| `cloud_run_revision` | `resource.labels.revision_name` | `resource.labels.service_name` | — | — | — |
| `cloudsql_database` | — | — | — | `resource.labels.database_id` | — |
| anything else | — | — | — | — | — |

Every dash is a fact about GCP rather than a gap:

- **no `pod` on Cloud Run** — the per-instance label is `instance_id` and it exists only on
  `cloud_run_instance`, the resource every selector here pins *away* from because it carries no
  `revision_name`. The two are mutually exclusive: one pointer cannot carry both a revision and an
  instance;
- **no `host` on Cloud Run** — it is serverless. There is no node and no label naming one;
- **no `version` on Cloud SQL** — a managed database has no deployed version in its metric labels.
  The engine version is a property of the instance, stored on the node by the configuration path;
  it is not a label any series is grouped by, so splitting an error rate by it is not a query that
  exists;
- **no `workload` on Cloud SQL** — `cloudsql_database` carries no `service_name`. Which services
  talk to an instance is an edge in the graph, not a tag on the telemetry;
- **`host` rather than `pod` for the Cloud SQL instance** — the instance is what the series is *of*,
  which is the role a replica plays elsewhere, and it is the reading the GCP telemetry backend
  already publishes in mapping `database_id` to `JoinKeys.pod_or_host` (FR-102). The feeder naming
  a different role would put the two ends of one join on different fields;
- **no `trace` anywhere** — this estate has no trace data source (FR-085), and `gcp-trace-filter/v1`
  is registered and deliberately unminted for that reason;
- **`anything else`** is the row that matters for what comes next. An **alert policy's condition
  filter** is minted exactly as the policy's author wrote it, and may pin no `resource.type` at all.
  Declaring `version → resource.labels.revision_name` on a filter watching a Pub/Sub subscription
  would name a label that does not exist, and a consumer splitting by it would get a confident
  answer over one merged group. So those pointers carry no join keys.

`resource.labels.revision_name` is also the one field `errors_by_version` groups by and the value it
reports as the digest's `version_attribute`. A term asking to split by any other field is **refused**
rather than answered: the grouping is fixed, so answering would report a breakdown by a field it was
not grouped by.

The language is **not independently versioned** — it is versioned only by the enclosing API surface,
which is why the name carries `/v3`. The API version *is* the vocabulary version.

### `gcp-logging-query/v2` — a Logging query

**Backend kind** `gcp-logging` · **executed by** `logging.entries.list`, Logging API v2 · added by
feature 003

```
resource.type = "cloud_run_revision"
  AND resource.labels.service_name = "<service>"
  AND resource.labels.location = "<region>"
  AND severity >= WARNING
```

Operators: `AND` `OR` `NOT` (and `-`), the comparisons, `:` (has / substring), and `=~` / `!~` for
RE2 regular expressions. Maximum filter length **20,000 characters**; `resourceNames` at most **100**
per request.

**Why not `otel-semconv`**, at length, because this is the clearest case and constitution IV requires
the paragraph rather than a reference to one. `otel-semconv/1.30` is an **attribute-equality**
vocabulary: `key="value" AND key="value"`, and nothing else. The Logging query language is a **query
language**, and the pointers this feature needs use the parts of it that have no equality form:

- **severity comparison** — `severity >= WARNING` is an ordering over an open enumeration, not a set
  membership. Flattening it into `severity="WARNING" OR severity="ERROR" OR …` would change what the
  pointer matches the next time Google adds a severity level, silently, years after the pointer was
  stored;
- **RE2 regular expressions** via `=~` and `!~`, which have no equality encoding at all;
- **field-existence tests** (`labels.foo:*`), which assert presence rather than a value;
- **negation and parenthesised boolean structure**, which an `AND`-only grammar cannot express.

A pointer is stored and read back years later (FR-083), so a lossy translation is not a cosmetic
concern: it is a pointer that quietly stops matching what it was written to match.

**No server-side aggregation exists.** `entries.list` returns raw entries; the query language has no
`GROUP BY` and no `COUNT`. Both server-side alternatives are unavailable to a read-only connector —
creating a log-based metric is a mutation and is not retroactive, and Observability Analytics
requires upgrading the log bucket, also a mutation. So `new_log_patterns` mines templates
**client-side over a bounded sample**, and the bound is published rather than incidental.

**Join roles** are the `cloud_run_revision` row of the table above, spelled identically: a Cloud Run
log entry carries `resource.labels.revision_name` and `resource.labels.service_name` as resource
labels, so `version` and `workload` are both expressible here — unlike a Kubernetes log stream, where
`version` is absent.

### `gcp-trace-filter/v1` — registered and deliberately unminted

**Backend kind** `gcp-trace` · added by feature 003

This organisation has **no trace data source**. The vocabulary is registered and **no pointer is
minted in it**, because FR-085 requires that distinction: the absence must be a stated fact about the
entity, so a consumer can tell *"no trace pointer because there is no tracing"* from *"nobody wrote
one"*. `pkg/feeder`'s own test asserts that no feeder mints one.

Registering the name now is what lets `error_spans` become a live operation **with no schema change**
if Cloud Trace is ever enabled. Until then the term is served, answering `NO_DATA` with the absent
source named.

### `datadog-logs/v1` — a Datadog log-search query

**Backend kind** `datadog` · **executed by** `POST /api/v2/logs/events/search` and
`POST /api/v2/logs/analytics/aggregate`, both named query operations (ADR-0010 item 3) · added by
feature 005

```
service:voice-agent env:production
```

A published subset of Datadog's log-search syntax: `service:<v>` and `env:<v>` always, `index:<v>`
where configured, and at most the tag and attribute terms the feeder itself writes. No free text and
no wildcard on `service`. The organisation, the site host and any credential are never in it (005
FR-037): the site is configuration.

**Why not `otel-semconv`.** Datadog's grammar distinguishes a **tag** (`version:x`) from an
**attribute** (`@version:x`), and they are different fields with different contents: a library that
logs its own `version` in a JSON body produces the *attribute* `@version`, which is not the
deployment's `version` *tag*, and 005's audit found exactly that — an SDK version on start-up lines
that a tag-blind selector would have read as the service's version. `otel-semconv/1.30` has one flat
attribute space and cannot state the distinction, so a translated selector could silently match the
wrong field.

**Join roles.** `version` is the attribute version-stamp discovery accepted, spelled as the grammar
spells it (`version` for the tag, `@version` for the attribute); absent when discovery found none,
which is when the engine answers `errors_by_version` itself (ADR-0010 item 2). `host` is `host` where
the logs carry one. `workload`, `pod` and `trace` are omitted: the default capabilities cannot express
them.

### `datadog-monitor/v1` — a Datadog monitor's query

**Backend kind** `datadog` · **executed by** `GET /api/v1/monitor/{monitor_id}` · added by feature 005

The monitor's query string exactly as Datadog stores it, plus the monitor id. Never rewritten. The
pointer's kind is the kind of what the monitor queries — `METRIC` for a metric monitor, `LOG` for a
log monitor — and a monitor whose type has no published kind gets a `SOURCE_LINK` only.

**Why not `otel-semconv`.** Monitor queries span Datadog's metric, log and composite grammars, each
with its own aggregation, grouping and threshold syntax (`avg(last_5m):sum:… > 0.5`). None of it is
expressible as attribute equality, and a monitor query is only meaningful in its monitor type's
grammar.

**Join roles.** None: a monitor's `by {…}` group names are its grouping, not the entity's, and are
carried as attributes.

### Adding one

A new vocabulary is a schema change (constitution IX): version it in the name the way the ones above
are, document the selector grammar here, and say in the constructor's comment why the pointer could
not be expressed in `otel-semconv`.

Two rules the GCP vocabularies make concrete, and which any vendor vocabulary inherits:

- **The selector is the vendor's language; the attributes stay OTel.** The selector is what will
  actually be executed, and one translated into OTel and back can silently stop matching. The
  attributes are what lets a reader, or a future backend, recognise which *entity* the pointer is
  about without understanding the vendor's grammar. The entity is portable; the query is not.
- **Where the vendor's language is not independently versioned, the name carries the API surface's
  version**, and the pointer thereby records which grammar it was written in.

---

## How a vendor connector maps them

A connector never invents a pointer shape. It answers three questions per entity and uses the
helpers in `pkg/feeder/pointer.go`:

1. **Which attributes identify this thing?** Build the OTel resource attribute map once. This
   is the same map the connector puts in the node's `props`, so the pointer and the node cannot
   drift apart.
2. **Which of those attributes narrow the query?** Pass them to `OTelSelector(attrs, keys...)`
   in the order a reader would write them. Naming the keys is the normal case; omitting them
   sorts every attribute into the selector, which is usually wider than you want.
3. **Which backend owns it?** Take it from a flag, not from a constant.

```go
attrs := map[string]string{
    "service.name":                "payments",
    "service.namespace":           "shop",
    "deployment.environment.name": "prod",
}
selector := feeder.OTelSelector(attrs, "service.name", "service.namespace")

pointers := []*graphv1.Pointer{
    feeder.TracePointer(opts.TraceBackend, selector, attrs),
    feeder.MetricPointer(opts.MetricBackend,
        feeder.MetricSelector("http.server.request.duration", attrs, "service.name", "service.namespace"),
        attrs),
}
```

For a system the feeder reads *from* rather than queries, use `SourceLinkPointer` with the
resource path and its own vocabulary. For anything else, `NewPointer` takes all five fields.

Two rules a connector must not break:

- **No telemetry in `props`.** The graph refuses it (FR-009), and the refusal names your field.
  If you want a number in the graph, it is a property of the *entity*, not a measurement of it.
- **Pointers move with the entity, in valid time.** Emit the whole pointer list on every
  `upsert_node`; it is versioned like any other property and the query answers as of an instant.

---

## Pointers and time

A pointer belongs to a node **version**, not to an entity (FR-008: "Pointers MUST be versioned
in time like any other property"). Asking for the pointers of `payments` at 13:00 returns the
selectors that were true at 13:00, and asking at 14:32 returns the ones that are true now:

```sh
aisre query pointers otel.service.name=payments --as-of 2026-09-01T13:00:00Z
# TRACE  tempo  service.name="payments" AND service.namespace="shop"

aisre query pointers otel.service.name=payments --as-of 2026-09-01T14:32:00Z
# TRACE  tempo  service.name="payments-v2" AND service.namespace="shop"
```

That is US5 scenario 2, and `fixtures/rollout-regression-01` is its fixture: payments is renamed
at 14:00 — display name, `service.name` and the selectors move together — and the two recorded
goldens `pointers.payments-pointers-1300` and `pointers.payments-pointers-1432` differ in the
selectors, not only in the name. An investigator working a 13:00 incident gets the query that
worked at 13:00.

`--observed-at` rewinds the other dimension: which pointer set the graph *knew about* at an
instant, as opposed to which was true. A selector corrected at 15:00 for a rename that happened
at 14:00 is visible as-of-now and invisible as known at 14:32.

### Aliases and names

The query returns the node version alongside its pointers, with the `aliases` the graph holds
for the entity, so a consumer can see under which names the selectors were written. A merged
entity answers under **either** of its names — `query pointers k8s.deployment=shop/checkout` and
`query pointers otel.service.name=checkout` return the same node and the same pointers, because
the reference is resolved through `merged_into` before anything is read (FR-039).

### Pointers are unioned across sources

A node version's pointer list is the **union** of what every source asserting that version says,
not one source's list (`internal/projector/upsert_node.go`, `materializeNode`). This is the one
place the projector treats a multi-source field neither as a conflict nor as a winner-takes-all:

- **Properties** keep one record per source, because two sources asserting different values for
  one key disagree, and the graph never picks a winner.
- **The display name** comes from the primary assertion — the latest by valid time — because a
  thing has one name at a time.
- **Pointers are unioned**, because two sources naming two different backends are *both right*.
  "Look in Loki" and "look in Tempo" are not competing answers to one question; they are two
  answers to two questions, and dropping either loses an investigation path.

The union is deduplicated by everything that identifies a pointer — kind, backend kind,
vocabulary and selector — so two sources naming the same place say it once, and stored in
(kind, backend kind, selector, vocabulary) order so the bytes are a function of the set rather
than of which source was read first. Attributes are not part of the identity: two sources that
agree on where to look but describe the entity differently are naming one pointer, and the first
in that order supplies its attributes.

This is what makes US5 scenario 1 answerable on a merged entity. A Kubernetes workload and the
OpenTelemetry service running on it are one node; the Kubernetes feeder contributes `LOG` and
`SOURCE_LINK`, the OpenTelemetry feeder contributes `METRIC` and `TRACE`, and the node carries
all four. Taking the primary assertion's list instead — which is what the projector did until
US5 — silently dropped half of them, and which half depended on which feeder happened to assert
the later valid time.

It also means each source's pointers move on their own schedule. In
`fixtures/rollout-regression-01`, only the OpenTelemetry side of `payments` is renamed at 14:00,
so across that instant the `TRACE` and `METRIC` selectors change and the Kubernetes `LOG` and
`SOURCE_LINK` ones do not — the deployment is still `shop/payments`. Both facts are in one
answer, which is the whole point.

---

## Reading them back

```sh
# Grouped by kind, as of an instant.
aisre query pointers otel.service.name=payments --as-of 2026-09-01T14:32:00Z

# The canonical JSON a golden holds.
aisre query pointers otel.service.name=payments --as-of now --output json
```

The response is `by_kind`, a map keyed by the `PointerKind` **name** — `METRIC`, `LOG`, `TRACE`,
`DASHBOARD`, `SOURCE_LINK` — so a consumer with no copy of the enum can still read it. A kind
the node has nothing for is **absent**, not present and empty. Within a kind the order is
`backend_kind`, then `selector`, then `vocabulary`, which is total over what identifies a
pointer and is what lets a response be frozen as a golden and compared byte for byte.
