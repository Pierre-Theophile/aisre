# Phase 0 research — Datadog Connector

**Date**: 2026-09-27 · **Plan**: [plan.md](./plan.md) · **Spec**: [spec.md](./spec.md)

Three kinds of finding are recorded here, and they are not interchangeable. **§1 is read out of this
repository's code**: what features 001–004 have shipped, checked file by file against what the
specification (written 2026-09-17) assumed. **§2 is read out of Datadog's published documentation**,
cited per fact. **§3 is read out of the organisation's own Datadog**, from the read-only look on
2026-09-27 that led to the spec's clarification. Where a fact could not be established from any of
them it is listed in §5 as open, with the question it is open on.

---

## 1. What the code delivers, and what it does not

### 1.1 Delivered, and used as-is

| item | evidence | use |
|---|---|---|
| The telemetry-backend SDK: `Describe`/`Execute`, the eight telemetry terms, the refusal of everything else | `pkg/backend/backend.go:23-137`; `registry.go:132-211` | implement it; the spec's section F is already a published contract |
| The six typed outcomes, `NO_DATA`, `NOT_YET_INGESTED` and `NOT_RECORDED` among them | `pkg/backend/backend.go:60-78`; `investigation.proto:107,117` | FR-049, FR-049a |
| Cost classes, checked at registration | `backend.go:79-90, 198-240`; `registry.go:48-97` | one class per term |
| The coverage block, with an undetermined flag per field | `investigation.proto:140-161` | FR-048a |
| Recorded mode, world recordings, the miss rate | `internal/investigation/backend/recorded.go`; `missrate.go`; `pkg/backend/record.go` | FR-050–FR-050b unchanged |
| The GCP backend, a complete reference implementation | `internal/backends/gcp/*` | followed file for file; its `absent(...)` is the FR-049b pattern |
| `ALERT` and `WATCHES` | `graph.proto:32, 44` | **the spec's "watches" dependency is already met** |
| `alert.transition` with `sampled`, the 4-tuple key, the state vocabulary, flap suppression | `graph.proto:235, 471-503`; `internal/log/alert.go:25-127`; `internal/feeders/gcp/monitoring.go:1020-1062` | FR-024, FR-025, FR-025c; the GCP feeder is the pattern |
| Actor kind | `graph.proto:65-72` | FR-033a |
| Unattached alerts and changes, auto-attach | `internal/projector/attach.go`; ADR-0009 item 5 | FR-019, FR-030 |
| The `deploy.*` normalisers, and `deploy.*` as correlation keys rather than identity claims | `pkg/feeder/deployref.go`; `pkg/feeder/claim.go:50-100` | FR-040d; a Datadog rollout emits a correlation key with `deployment.environment.name`, as `internal/feeders/gcp/map.go:315,323` does |
| C8 | `internal/resolution/deploy.go:66-213` | FR-040g's merge; its preconditions in §1.3 |

### 1.2 The six gaps

Each is stated with the decision taken; plan.md tabulates them with their resolution.

**G1 — a version group cannot name its deploy identifier.** `VersionBreakdown`
(`investigation.proto:242`) carries the version, counts, rate, join keys and a drill-down.
**Decision**: add `deploy_ref` and `deploy_ref_absent_reason` (investigation.v1 MINOR, ADR-0010).
They are generic: the GCP backend fills `deploy_ref` where a revision's image digest is known, so the
property is proved on two backends rather than one.

**G2 — `errors_by_version` with no attribute never reaches a backend.** The engine refuses it as
outside the algebra (`internal/investigation/backend/algebra.go:380`), and the world cross product
only enumerates the term when a pointer carries `join_keys["version"]`
(`internal/cli/worker_record.go:776`). FR-040b's "`NO_DATA` naming every convention searched" is
therefore unreachable. **Decision**: the **engine** answers it, from the pointer alone: no version join
key ⇒ `NO_DATA`, coverage naming "no version stamp on this pointer" and the discovery verdict's
candidates. *Alternatives rejected*: widening the term so the backend answers it (every recorded world
would need extending, and every backend would re-implement the same refusal); leaving it unasked (the
investigation would never learn why the split is unavailable, which FR-040b exists to prevent).

**G3 — the read-only surface refuses POST.** `readMethods = {"GET","HEAD"}` and
`MustReadOnlySurface` panics otherwise (`pkg/feeder/readonly.go:83`). Datadog's log search and
aggregation are POST (§2.1). **Decision**: named query operations — a POST is declarable only as an
individual operation carrying a published justification, listed in `docs/connectors/datadog.md` and
tested; the method rule stays for everything else (ADR-0010). *Alternatives rejected*: allowing POST
by method, which turns FR-004's "verifiable from the operation list" into trust; not answering log
terms, which leaves the backend contract proved on one vendor.

**G4 — no environment-checked rule for a Datadog log service.** C1 accepts any namespace outside the
shared-property list (`internal/resolution/certain.go:99-136`), so a Datadog claim of
`otel.service.name` would merge a staging and a production service; nothing else reads a Datadog
namespace. **Decision**: claim `datadog.log_service` with the environment attribute, and publish
**C9**: equal normalised name **and** equal stated environment (and agreeing Kubernetes namespace or
cluster where both state one). It is the spec's FR-059 generalised from APM services to log sources.
**The C1 environment hazard predates this feature** and is recorded for its own change, not fixed here.

**G5 — the quota reader is GitHub's.** It reads `X-RateLimit-Reset` as an epoch and the family from
`X-RateLimit-Resource` (`pkg/feeder/quota.go:88-113`). Datadog's reset is relative and its family is
named differently (§2.3). **Decision**: a Datadog reader beside the GitHub one, tested against
recorded headers.

**G6 — the doorbell and the campaign record are GCP-shaped.** The doorbell's payload-free `Ring` and
token bucket are right, but it drains Pub/Sub, lives in the GCP package and has no HTTP endpoint or
secret check (`internal/feeders/gcp/doorbell.go`); the campaign record's scope is projects, regions and
a mailbox (`internal/campaign/record.go:76-91`). **Decision**: lift the doorbell into
`pkg/feeder/doorbell` with an HTTP transport requiring the shared-secret header (FR-025b); add
organisation, site, environments and indexes to the campaign scope.

### 1.3 Preconditions that change individual requirements

- **C8 excludes `deploy.release`** (`deploy.go:63-66`), requires both changes to state the same
  `deployment.environment.name` (`:168-174`), and requires a shared target after merges (`:183,
  201-207`). So a log-observed rollout merges only once C9 has made the two services one entity, and
  only for a commit sha or an image digest.
- **The image normaliser refuses a bare digest** (`deployref.go:67`), which is what
  `container.image.digest` usually holds. The convention is the pair name + digest; a digest alone is
  a group with `BARE_DIGEST` and no ref.
- **The sanitiser has no per-source tables**; `NewPolicy` is documented as the way 005 builds its own
  (`internal/sanitise/policy.go:115`), and an unassigned field fails closed (`:83-95`).
- **Nothing Datadog-specific exists in Go** beyond test fixtures and proto comments; there is no Slack
  intake package either (declarations go through `intake/declared.go`).

---

## 2. Datadog — established from the published documentation

Each fact is cited. "Not established" means the documentation was read and does not say; those
items are carried to §5 rather than assumed.

### 2.1 The query surface

- **Search logs** is `POST /api/v2/logs/events/search`: `page.limit` up to 1000, a cursor from
  `meta.page.after`, sort `timestamp` or `-timestamp`, and each item carries the log's custom
  attributes in `attributes.attributes`. The response states `meta.status` (`done` or `timeout`) and
  `meta.warnings`, which is how a partial answer is recognised. Needs `logs_read_data`.
  [search-logs-post](https://docs.datadoghq.com/api/latest/logs/search-logs-post/),
  [pagination guide](https://docs.datadoghq.com/logs/guide/collect-multiple-logs-with-pagination/)
- **Aggregate** is `POST /api/v2/logs/analytics/aggregate`: `group_by[].facet` is required and is
  "the name of the facet", `missing` names the bucket for logs without it, `limit` defaults to 10
  and the product of limits must not exceed 10 000.
  [aggregate-events](https://docs.datadoghq.com/api/latest/logs/aggregate-events/)
- **Facets are what analytics groups on.** "Facets on Reserved Attributes and most Standard
  Attributes are available by default"; search does not need a facet, analytics does.
  [facets](https://docs.datadoghq.com/logs/explorer/facets/). The reserved attributes are host,
  source, status, service, trace_id and message — **`version` and `env` are not among them**; they are
  unified-service-tagging *tags*.
  [attributes naming convention](https://docs.datadoghq.com/logs/log_configuration/attributes_naming_convention/)
- **There is no public log-patterns API.** The Logs API reference lists send, aggregate, search, and
  the configuration sections; Log Patterns is a UI feature that clusters on `message`.
  [logs API](https://docs.datadoghq.com/api/latest/logs/),
  [patterns](https://docs.datadoghq.com/logs/explorer/analytics/patterns/)
- **Monitors**: `GET /api/v1/monitor` and `GET /api/v1/monitor/{id}` take `group_states`; each group
  carries `status` (Alert, Warn, No Data, OK, Ignored, Skipped, Unknown) and `last_triggered_ts`,
  `last_resolved_ts`, `last_nodata_ts`, `last_notified_ts`.
  [get a monitor](https://docs.datadoghq.com/api/latest/monitors/get-a-monitors-details/). There is
  no transition-history endpoint; alert events in the Events API v2 carry `monitor_id`,
  `monitor_groups`, `status` and `timestamp`, and need `events_read`.
  [search events](https://docs.datadoghq.com/api/latest/events/search-events/)
- **Sites**: the EU site's API host is `api.datadoghq.eu`.
  [sites](https://docs.datadoghq.com/getting_started/site/)

**Consequence: the query operations are POST.** That is Gap G3, and the reason ADR-0010 exists.

### 2.2 Credentials

- `GET /api/v1/validate` checks an API key; `GET /api/v2/validate_keys` checks both keys. Neither
  returns scopes. [validate keys](https://docs.datadoghq.com/api/latest/key-management/validate-api-and-application-keys/)
- `GET /api/v2/current_user/application_keys/{id}` returns `attributes.scopes`, but requires the
  `user_app_keys` permission. [application keys](https://docs.datadoghq.com/api/latest/key-management/get-one-application-key-owned-by-current-user/)
- An unscoped application key has every permission of the user who created it.
  [API and app keys](https://docs.datadoghq.com/account_management/api-app-keys/)

**Decision (FR-003).** At startup the connector validates both keys, then asks for its own key's
scopes. If Datadog answers, any write scope refuses the start, naming it. If Datadog refuses the
question — likely for a key scoped narrowly enough to be safe — the connector names what it could not
verify and requires `--assert-read-only`, recorded in the checkpoint as `operator_asserted`, exactly
as the Vercel feeder does. It never assumes.

### 2.3 Rate limits

Headers: `X-RateLimit-Limit`, `X-RateLimit-Period` (seconds, calendar-aligned),
`X-RateLimit-Remaining`, `X-RateLimit-Reset` (**seconds until** the reset, not an epoch) and
`X-RateLimit-Name`; a limit breach is a 429, and "different endpoints can share the same name".
**No default figures are published** for logs search, aggregate or monitors.
[rate limits](https://docs.datadoghq.com/api/latest/rate-limits/)

**Decision.** The endpoint class is keyed by `X-RateLimit-Name`, not by endpoint path, because shared
buckets are shared names (FR-081b). Where no header comes back, the static configured budget applies
and the usage report says so (FR-081a). This is Gap G5's reader.

### 2.4 How a version reaches a log line

- **Unified service tagging** sets `version` from the `tags.datadoghq.com/version` Kubernetes label,
  the `com.datadoghq.tags.version` Docker label, or `DD_VERSION`; the labels cover log collection and
  the tracing libraries inject the value into logs when log injection is on. It is **not universal**:
  on ECS Fargate with Fluent Bit or FireLens it covers metrics and traces only.
  [unified service tagging](https://docs.datadoghq.com/getting_started/tagging/unified_service_tagging/)
- **OpenTelemetry** `service.version` maps to `version`; `DD_VERSION` is not read by OTel SDKs, which
  use `OTEL_RESOURCE_ATTRIBUTES`. (same page)
- **No preprocessing step remaps a JSON `version` field**: the preprocessing list is date, message,
  status, service, host, source, trace_id and span_id, and no processor is a version remapper. A
  library's own `version` field therefore stays the *attribute* `@version`, distinct from the *tag*
  `version`. [pipelines](https://docs.datadoghq.com/logs/log_configuration/pipelines/),
  [processors](https://docs.datadoghq.com/logs/log_configuration/processors/)
- **Source-code integration** sets `git.commit.sha` from `DD_GIT_COMMIT_SHA` or the OCI
  `org.opencontainers.image.revision` label, and is documented as attached to every **span**; its
  presence on every log line is not established.
  [service mapping](https://docs.datadoghq.com/source_code/service-mapping/)

**Consequence for FR-040c.** The tag `version` and the attribute `@version` are **two candidates**,
tried separately, and the share criterion decides between them. That is what separates the audit's
case (the SDK's `@version` on start-up lines) from a real stamp (the `version` tag on every line).

---

## 3. The organisation's Datadog, read on 2026-09-27

Read-only searches over seven days (2026-09-20 to 27), with the owner's approval, on the EU site.

- **One log source**, a vendor-hosted voice-agent platform (named in the private corpus, not here), ≈ 37.2 M lines, of which 3 606 error-level and
  46 680 warnings; the rest debug and info. Four agent identifiers, two carrying nearly all traffic.
- **No deploy identifier on any line.** Candidates searched: `version` (tag and attribute),
  `agent_version`, `revision`, `image`, `commit`, `git.commit.sha`, `release`, `deployment`,
  `deployment_id`, `build`.
- **`version` present on 255 lines only** — every one a `starting worker` line — with the value
  `1.3.6` beside `rtc-version 1.0.19`: the agent SDK's version, constant all week.
- **Worker starts are not deploys**: 10–30 a day, each a fresh worker, with the timing of scaling.
- **Datadog's SQL `version` and `env` columns were empty** where search showed values, because the
  values came from custom attributes. Discovery therefore queries attributes and tags explicitly.

### 3.1 What the audit sets as the discovery thresholds

The audit gives one clear negative: a candidate on 255 of 37.2 M lines (0.0007 %). A real stamp set by
unified service tagging or `service.version` is on every line the service writes. **Decision**: the
published default is **≥ 95 % of lines and ≥ 95 % of error-level lines** over a discovery window of
one hour, configurable per service; the verdict states the shares either way. The error-level share
matters on its own, because `errors_by_version` groups errors: a stamp missing from exactly the lines
a crash handler writes would split errors into a large unlabelled group.

---

## 4. Decisions carried from 003 and 004, and the few taken here

- **No Datadog client library.** The surface is five endpoints in the default capabilities. Hand-
  written requests over the metered transport make the published operation list the literal set of
  requests the binary can issue (FR-004), which a generated client with hundreds of methods does not.
- **`new_log_patterns` is mined backend-side** from a bounded, paged sample (§2.1: no patterns API),
  as the GCP backend does over Cloud Logging. The miner, its caps and its coverage wording are reused;
  if `internal/backends/gcp/logpatterns.go` proves GCP-specific it is lifted into `pkg/backend`.
- **`errors_by_version` prefers the aggregate API**, grouping by the discovered attribute, with a
  second aggregate without the error clause for `total`. Where the attribute is not groupable (§5
  O1), it is answered from a paged search up to the published cap, and coverage states the sample.
- **Monitor transitions come from `group_states`.** Each group's `last_triggered_ts`,
  `last_resolved_ts` and `last_nodata_ts` are Datadog-stated instants, and valid time is taken from
  them, never from the poll (FR-020). A transition that opened and closed between two polls is
  invisible to this, which is exactly what the `sampled` marker states (ADR-0009 item 3). Backfilling
  from the Events API would need `events_read` for the default capability; it is **not built in v1**
  and is recorded as a later increment.
- **The doorbell carries no data**, the budget reserves a share for humans, window widths are
  capped per term: 003's designs, reused (Gap G6 for the doorbell's shared home).

---

## 5. Still open, with the question each is open on

| # | Question | Why it matters | How it closes |
|---|---|---|---|
| O1 | Does the aggregate API group by a non-faceted attribute, and is the `version` tag faceted by default? | Decides whether `errors_by_version` counts exactly or from a sample | One read-only aggregate call per case against the organisation, recorded as a fixture payload, before the backend task that depends on it |
| O2 | Can a narrowly scoped read-only application key read its own scopes? | Decides whether FR-003 is verified or operator-asserted in practice | The first `--dry-run` with the provisioned key; both outcomes are already handled |
| O3 | When C8 merges a log-observed rollout with a deploy feeder's, which valid start does the merged change carry? | FR-040g requires the stated instant to be the rollout instant | `datadog-log-rollout-merge-01`; if the projector keeps the earlier bound, the fix is in the merge, planned with the fixture |
| O4 | How does a vendor-hosted runtime (the organisation's voice-agent platform) expose the deployed commit to the process? | The stamping guide must give a working recipe for the one service Datadog holds today | Documented recipe is build-time: bake the commit into the image and export it as `DD_VERSION` / `service.version`. Whether the platform also offers a runtime variable is checked against its own documentation when the guide is written |
