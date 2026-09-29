<!-- SPDX-License-Identifier: Apache-2.0 -->

# The Datadog connector

> **Status.** The operation table in §2 is enforced: it is
> `internal/feeders/datadog/requestlog.go`'s default surface, and a test compares the two in both
> directions. The cost figures in §4 come from a recorded run (`fixtures/datadog-rate-limited-01`).
> The monitors, logs and tags capabilities have run against a live organisation (2026-09-27, 2026-09-28).
> The `changes` capability (§5) and the `apm_topology` capability (§6) have not: they are built against
> Datadog's published API shapes and are not yet verified against a live organisation (the audited one has
> no tracing, and its key lacks the APM scopes). The gate's behaviour with a narrowly scoped key (research
> §5 O2) is recorded in §3 from the first live run (2026-09-28).

What this connector reads, what it costs, and the proof it can only read.

Contract: [`specs/005-datadog-connector/contracts/read-only-operations.md`](../../specs/005-datadog-connector/contracts/read-only-operations.md).
Platform facts, from the published documentation:
[`research.md`](../../specs/005-datadog-connector/research.md) §2.

## 1. Capabilities

One connector, two halves — a feeder and a telemetry backend — one read-only credential. Each
capability is enabled independently and recorded in every checkpoint; a disabled one requests no
scope, declares no operation and emits nothing (FR-008a, FR-008b).

| capability | default | what it does |
|---|---|---|
| `logs` | on | a service node and a log pointer for every watched log source; the telemetry backend's log terms |
| `monitors` | on | alerts and their transitions; `monitor_state` |
| `tags` | on | owners and identity claims from allowlisted tags |
| `apm_topology` | off | the APM service map as graph nodes and `calls` edges, rollouts from the version APM reports, and the backend's span and APM metric terms (§6); built against the published API shapes, not yet verified live |
| `changes` | off | Datadog's event stream as a change source, for an organisation that posts deployment, configuration or infrastructure events into Datadog (§5); built against the published API shape, not yet verified live |

The host is the configured site's API host (`api.datadoghq.com`, `api.datadoghq.eu`, …). The site is
configuration and never appears in a pointer.

## 2. The published read-only operation surface

Every operation this connector may issue, by name. An operation absent from this list is refused
before any quota is spent.

Two of them are `POST`. Datadog's log search and aggregation take the query as a JSON body, so under
a method-only rule the backend could not answer a single log term. They are **named query
operations** (ADR-0010 item 3): each is declared individually, with the reason it cannot change
state, and no other `POST` — and no `PUT`, `PATCH` or `DELETE` — is admissible at all.

| area | operation | why |
|---|---|---|
| startup | `GET /api/v2/validate_keys` | check that the API and application keys are valid before anything else runs |
| startup | `GET /api/v2/current_user/application_keys/{id}` | read the application key's own scopes, so a write-capable key refuses the start |
| logs | `POST /api/v2/logs/events/search` | named query: returns the log events matching the query in the body, paged; the endpoint has no field that creates, updates or deletes anything |
| logs | `POST /api/v2/logs/analytics/aggregate` | named query: returns counts of the log events matching the query in the body, grouped by facet; the endpoint has no field that creates, updates or deletes anything |
| monitors | `GET /api/v1/monitor` | list monitor definitions with their per-group states |
| monitors | `GET /api/v1/monitor/{monitor_id}` | read one monitor with its per-group states |

**Never declared**, and refused if anyone tries: monitor mute, unmute, create, update or delete;
downtimes; posting events; incidents of any kind; dashboards, notebooks, saved views; log indexes,
pipelines, archives, metrics or restriction queries.

## 3. The credential, and what the startup gate can prove

The keys come from the environment: `DD_API_KEY`, `DD_APP_KEY`, and `DD_APP_KEY_ID` so the gate can read
the application key's own scopes. There is no flag for a key: a key on a command line ends up in
every shell history and every process listing.

Nothing is read or emitted before the gate passes (`internal/feeders/datadog/gate.go`). It proves two
things, and names whatever it could not prove:

1. **The keys work.** `validate_keys` answers. If not, the start is refused, naming the key.
2. **The application key can only read.** The gate reads the key's own scopes. Any scope outside the
   read set of the enabled capabilities refuses the start, and each such scope is named.

| capability | read scopes it needs |
|---|---|
| `logs` | `logs_read_data`, `logs_read_index_data` |
| `monitors` | `monitors_read` |
| `tags` | none: tags ride on the log reads |
| `apm_topology` | `apm_read`, `apm_service_catalog_read` |
| `changes` | `events_read` |

**What the gate cannot prove.** In three cases Datadog does not tell the gate what a key can do:

- the key lacks the permission to read its own scopes;
- no `DD_APP_KEY_ID` was given;
- the key is unscoped, which gives it every permission its user has.

The gate names the case, and the connector does not start without `--assert-read-only "<name>"`. A
named person then asserts the key is read-only, and every checkpoint records it as
`operator_asserted by <name> at <instant>`.

`feed datadog --dry-run` runs the gate and nothing else: no monitor read, no event, no connection to
the graph.

**What the first live run found (2026-09-28, research §5 O2).** A read-only key cannot prove it is
read-only. Plan for the assertion.

| the application key | what the gate says |
|---|---|
| no `DD_APP_KEY_ID` given | refused: the scopes cannot be read |
| unscoped | refused: it carries every permission of its user, writes included |
| scoped to `monitors_read`, `logs_read_data`, `logs_read_index_data` | refused: Datadog will not state the scopes of a key that lacks `user_app_keys` |
| the same, plus `user_app_keys` | refused: `user_app_keys` lets the key manage application keys, and so mint an unscoped one; it is outside the read set |
| scoped to the three read scopes, with `--assert-read-only "<name>"` | **passes**, `operator_asserted by <name>` in every checkpoint |

A key that can only read therefore cannot read its own scopes. `--assert-read-only` is not a
fallback for a misconfigured key: it is how a correctly scoped key starts. Scope the key to the
three read scopes, give `DD_APP_KEY_ID` so an unscoped or over-scoped key is still caught and
refused, and name the person who scoped it.

## 4. What it costs

Every number below is a call count from `fixtures/datadog-rate-limited-01`, which is recorded from the
live poller, the real client and its budget against a twin that answers with rate-limit headers.
`TestTheBudgetHoldsToTheExactCall` checks that the calls the twin served, the calls the budget counted
and the calls the recorded usage report states are one number. None of these counts is an estimate.

### Per unit

| Area | When | Calls | Bucket in the recording |
|---|---|---|---|
| Monitor poll | every `--poll-interval` (20 s by default) | 1 per page of 100 monitors; a list of exactly 100·n reads one more, empty page | `monitors_list` |
| Doorbell ring | at most one extra poll per minimum poll interval | as a monitor poll | as above |
| Discovery | every hour, per watched log source | 12 aggregates: totals, host presence, 7 published conventions, 3 allowlisted tag keys. With `--version-override`, 6: the override replaces the 7 conventions | `logs_analytics` |
| Rollout lookup | every discovery, per source whose stamp was accepted | 1 aggregate, plus 2 searches per value new to the poller | `logs_analytics`, then the log-search bucket |

A restarted poller lists values it has seen before again. That costs 2 searches per value once, and
re-derives ids the graph already holds.

### For a stated estate at the default cadence

Take 50 monitors in scope, 10 watched log sources that are all stamped, and 2 deploys per source per
day.

| Area | Calls per hour |
|---|---|
| Monitor poll | 180 (one page, every 20 s) |
| Discovery | 120 (10 × 12) |
| Rollout lookup | 10 aggregates, plus about 1.7 searches (40 new values a day × 2 ÷ 24) |

Total: about 312 calls an hour. The monitor list takes 180 of them from its own bucket, and log
analytics takes 130 from its bucket. The investigation backend's calls come on top of this. Each
backend builds its own client and budget and reports them separately (FR-084a).

### The budget, and what it gives up first

- **Share and reserve.** `--quota-share` (0.5 by default) is the fraction of what Datadog says is left in
  a bucket (`X-RateLimit-Name`) that the connector may spend in that window. `--quota-reserve` (20 by
  default) is the number of calls it never spends, whatever the share works out to. The share is fixed
  when the window opens. The reserve is checked against Datadog's latest reading, so when the people
  working an incident drain the bucket, the connector stops at the reserve. In the recording, the 14:05
  discovery reads until 20 calls are left and stops, typed `quota`.
- **Small buckets.** A bucket smaller than twice the reserve is held to half its limit instead. The
  first live run met a logs aggregate bucket of 2 calls per window: against a reserve of 20 the
  connector could spend nothing, ever. Now one call per window is the connector's and the other
  stays unspent. `fixtures/datadog-small-bucket-01` records this.
- **A stopped measurement resumes.** When the budget stops a source's measurement, the connector
  waits for the reset Datadog stated. It then resumes over the same window, and a cache answers
  what it had already read. The tick is pushed once, complete, not half-measured.
  - At 2 calls per window, the 12 aggregates of one measurement take 12 windows.
  - The long-running poller resumes on a timer, so monitor polls never wait on discovery.
  - It gives up only when the reset falls after the next discovery would start anyway. It then
    states the source unmeasured, typed `quota`, as before.
  - A stopped rollout lookup is not suspended. The tick goes out with its values marked `quota`,
    and the next interval looks again.
- **The deferral order.** Some areas must leave part of the share to the areas ahead of them.
  - Monitor transitions and definitions (one read) may spend the share to the end.
  - Discovery stops when a quarter of the share is left.
  - The event stream (only under `changes`, §5) stops when 35 % of the share is left.
  - The APM topology read (only under `apm_topology`, §6) stops when 40 % of the share is left.
  - Rollout lookup stops when half of the share is left.

  Under pressure, the connector stops looking for new versions first, and stops reading alert
  transitions last. The recording's 14:00 rollout lookup is deferred this way.
- **429.** A 429 stops the poll as partial, typed `rate_limited`. The connector reads nothing until
  the reset Datadog stated, and a doorbell ring during the wait reads nothing and says so. The first
  call after the reset is drawn from the static allowance, and its response re-derives the share.
- **Stated, never silent.** Every poll marker, and so every checkpoint, carries three things:
  - the typed stop;
  - the areas deferred since the last poll;
  - the usage report: calls per area and bucket, and what is left of each bucket, with its source.

  A partial poll claims no coverage and retracts nothing. What it did not read is unread, not
  absent.

## 5. The `changes` capability

> **Built against the published API shape. Not yet verified against a live organisation.** The audited
> organisation posts no events to Datadog and its application key lacks `events_read`, so the response
> shape, the search syntax and the event link below come from Datadog's documentation and from the
> fixture `datadog-events-merge-01`, a synthetic twin. Expect the first live run to correct them, and read
> the checkpoint's counts before trusting an empty answer.

Changes come from the platforms themselves: Cloud Run revisions, GitHub Actions, Vercel, the cluster.
`changes` is for an organisation that *also* posts deployment, configuration or infrastructure events
into Datadog. Where it does, those events are a second observer of a rollout the platform feeders saw,
and the point of the capability is that the pair appears **once** (SC-014). The connector never merges
them itself (FR-032): it states the identifiers, and the resolution rules C9 and C8 do the rest.

**Enabling it.** Off by default. Name what counts as a change, or it refuses to start:

```
feed datadog --capabilities logs,monitors,tags,changes \
  --change-sources jenkins,argocd --change-tags event_type:deployment
```

| flag | meaning |
|---|---|
| `--change-sources` | event sources (`source_type_name`) whose events are changes; repeatable |
| `--change-tags` | `key:value` tags whose events are changes; repeatable |
| `--events-interval` | how often the stream is read (default 5 minutes) |

An event is in scope when its source is listed **or** it carries a listed tag (case-insensitive). With
neither flag the capability refuses to start, and the flags without the capability are refused too.
Datadog's stream also holds monitor alerts and agent events; which events are changes is the
operator's to say (FR-029). The configuration in force is stated in every events checkpoint, together
with how many events were read and how many the scope left out, so "no change happened" reads
differently from "we were not looking for that kind of change".

**Scope and operation.** `events_read`, declared only when `changes` is on. The one operation is a plain
read; posting an event stays on the never-declared list.

| area | operation | why |
|---|---|---|
| changes | `GET /api/v2/events` | list the events of a window, filtered by the configured sources and tags and paged by cursor; the endpoint reads the event stream and has no field that posts, edits or deletes one |

**What an event becomes.**

- One CHANGE, ref `datadog.change=<event id>`, valid at the instant Datadog recorded, never the poll's.
  An event with no instant is not emitted and is stated in the checkpoint.
- Its kind: `deployment`, `config_change`, `terraform`, `feature_flag`, `scaling` and their synonyms map
  to the published taxonomy. Any other kind, or none, is emitted as **other** with the vendor's kind kept
  as `sre.datadog.event_kind` (FR-028). Nothing is dropped for being unfamiliar.
- Its targets: the service the tags name (`datadog.service=<env>/<service>`, the same node a watched log
  source asserts) and, where both `kube_namespace` and `kube_deployment` are tagged, that Kubernetes
  deployment. An event that names neither is kept and counted unattached; one naming a service the
  graph has not seen attaches when it appears (FR-030).
- Its identifiers, for a **deployment** event only, as correlation keys carrying the environment:
  `deploy.commit_sha` from `git.commit.sha` or `commit_sha` (or a `version` that is a full commit),
  `deploy.image` from a digest-pinned `image`, and `deploy.release` from `version` or `release`. C8 merges
  on the first two, in one environment, when the two changes share a target. A configuration change
  quoting a commit claims none: it did not ship it. An event stating two different commits claims neither.
- Its actor kind (FR-033a, FR-033b), from what Datadog states and never from the actor's name: first a
  `triggered_by` tag in the published vocabulary (`user`, `manual` → person; `ci`, `pipeline`, `bot` →
  automation; `autoscaler`, `scheduler` → controller; `vendor` → vendor), then the event source
  (`jenkins`, `argocd`, `github` and the like → automation; `kubernetes`, `karpenter`, `keda` →
  controller; `aws_health` → vendor). Anything else is **unknown**, and the change carries the evidence
  there was in `sre.change.actor_evidence`. A bot named `alice` and a person named `deploy-bot` are why.

**Recording.** A live recording drops the title and the event's name (free text), pseudonymises the
service, environment and Kubernetes tags, gives a commit the pseudonym the deploy feeders give it so C8
still joins, and drops the tags that name the actor, an image reference and every tag the connector does
not read. The actor's name is dropped rather than pseudonymised, because a people identifier survives in
no form; the actor kind survives, derived before the name went.

**Cost and budget.** One read per events interval when a window has no next page, plus one per further
page (up to 100 events each). It draws on the `changes` area, which yields before discovery and after
rollout detection (§4). A window that does not finish is partial and says so; the next window reaches
back from the last complete one, less five minutes, and Datadog's own event ids make the overlap free.

## 6. The `apm_topology` capability

> **Built against the published API shapes. Not yet verified against a live organisation.** The audited
> organisation has no tracing and its application key lacks `apm_read` and `apm_service_catalog_read`, so
> the request and response shapes, the span facet names (`@peer.service`, `@error.type`, `@duration`), the
> trace metric names (`trace.<span>.hits`, `.errors`, `.duration.by.service.<pct>p`) and the service
> dependencies endpoint below come from Datadog's documentation and from two synthetic twins,
> `datadog-apm-topology-01` (the graph) and `datadog-backend-apm-01` (the backend). Expect the first live
> run to correct them; the metric names are one table (`apmMetrics`, `internal/backends/datadog/apm.go`).

Off by default. With it off nothing below happens: no APM scope, no APM call, no service or dependency
node, no APM rollout, no APM pointer, `error_spans` answers the typed `NO_DATA` naming the absent span
source, and every checkpoint says `apm_topology=off` (FR-008b, SC-023, SC-024). With it on, every other
capability's output is byte-identical (tested both ways).

**Enabling it.** Name the environments to read, or it refuses to start:

```
feed datadog --capabilities logs,monitors,tags,apm_topology --apm-envs production
```

| flag | meaning |
|---|---|
| `--apm-envs` | environments whose services and dependencies are read; repeatable, recorded in every checkpoint |
| `--apm-retract-after` | consecutive complete reads an edge may go unobserved before it is retracted (default 4) |
| `--apm-interval` | how often each environment is read, and the length of its window (default 15 minutes) |

The backend is enabled with the same capability (`Options.APMTopology`); its client must declare the
operations below, and a backend that asks for one it did not declare fails rather than answers.

**Scopes and operations.** `apm_read` and `apm_service_catalog_read`, requested only when `apm_topology`
is on. Three operations, all reads; the one POST is a named query like the two log queries.

| area | operation | why |
|---|---|---|
| topology | `GET /api/v1/service_dependencies` | list the services of an environment and the services each one calls; a read of the service map, with no field that creates, updates or deletes anything |
| topology | `GET /api/v1/query` | read a timeseries of APM trace metrics (hits, errors, duration percentiles) for a scope and a window; a metrics query, with no field that writes a metric |
| topology | `POST /api/v2/spans/analytics/aggregate` | named query: returns counts and duration percentiles of the spans matching the query in the body, grouped by facet; the endpoint has no field that creates, updates or deletes anything |

**What the map becomes (FR-009–FR-017).**

- A SERVICE node for every service Datadog reports in an in-scope environment, on the same ref a watched
  log source uses (`datadog.service=<env>/<service>`), carrying the version APM reports as
  `service.version`, the APM metric pointer (`datadog-apm-metric/v1`, naming the service's busiest
  operation) and the span pointer (`datadog-spans/v1`). A watched log source and APM state one node, built
  from both, because a source's latest assertion of a node replaces its earlier one. The node is dated by
  the window APM first reported it in, an instant Datadog states; Datadog does not say since when a
  service has existed, so that is a bound, and it is what lets an edge valid from the same window find its
  endpoints. It is never the poll's instant.
- A `calls` edge for every dependency, carrying only a weight **class** from the published ordinal scale,
  derived from a count of the spans Datadog retained and then discarded: a lower bound of the traffic. A
  dependency with no retained span in the window carries **no class** and the checkpoint lists it; class 0
  would say traffic was watched and found negligible. Its valid start is the first window it was observed
  in. A class change is believed when a second window agrees, and dated from the first.
- Retraction: an edge unobserved in `--apm-retract-after` consecutive **complete** reads is retracted, its
  valid end the end of the last window it was seen in. A part of a read that did not finish, or was
  refused for quota, neither confirms nor denies anything: nothing is retracted on it, no rollout is
  inferred from it, and the checkpoint declares the gap (`gap_before`). The next window reaches back from
  the last complete one.
- A dependency target Datadog reports and no listing names is a THIRD_PARTY node on the same ref, marked
  `sre.datadog.unlisted_dependency`. A later listing asserts a SERVICE on that ref, which replaces it. It
  is asserted only from a complete dependencies read, and never for a watched log source.
- A version that was not in the previous complete read is a ROLLOUT, `datadog.change=apm/<env>/<service>@<version>@<window>`,
  valid at the window it first appeared in and marked `sre.change.valid_from_is_a_bound=first_seen_in_apm`,
  with a `changed-by` edge to the service and the old and new version recorded. Its actor kind is
  **unknown**, with the evidence in `sre.change.actor_evidence`: APM states the version and never who
  deployed it, and deployment tracking's origin is not read (FR-033a, FR-033b). It carries the version's
  `deploy.commit_sha`, `deploy.image` or `deploy.release` key like a log-observed rollout, so C8 merges it
  with the platform's. The first read of a service is a baseline, not a change.
- Hosts: an INFRA_RESOURCE (`datadog.host=<host>`) and a `runs-on` edge from the service, whose start is
  unknown (FR-017). Only the `host` facet is read: never a container id or a pod name (FR-014).

**What the backend gains.**

- `error_spans(src, dst)` reads the spans `src` emitted towards `dst` (`@peer.service:<dst>`) from the span
  aggregate, grouped by operation and error kind, errors first, latency in milliseconds. The counts are of
  **retained** spans and the coverage says so (`datadog_apm:spans`, sampling `retained: …`): an aggregate
  over retained spans is a sample of the requests. Entity ids are read as `<env>/<service>`, or a bare
  service in `Options.APMEnv`, unless a caller supplies `Options.EntityService`.
- `compare` over a `datadog-apm-metric/v1` pointer states COUNT, RATE, ERROR_RATE, P50, P95 and P99 from
  the trace metrics, which count every ingested request, with a series per version where the service
  stamps one (the version is the join key). A percentile is the mean of Datadog's per-interval
  percentiles, and the coverage says so (`percentile_of_intervals`). Over a log pointer it still refuses a
  latency percentile.
- Not done: `onset` over the APM metrics, and `errors_by_version` from a span's `version`. The log path
  answers `errors_by_version`, and the engine answers it where the logs carry no stamp.

**Recording.** A live recording pseudonymises every environment, service, host and operation name with
the keyed pseudonym for its kind, so the recorded edges join the recorded nodes and log sources; a version
survives, for the join to the deploy feeders; the failure reason is withheld. The payload's window is
`start`/`end`, not `from`/`to`, which the sanitiser drops as an email's sender and recipient.

**Cost and budget.** Per environment per window: one dependencies read and four span aggregates (callee,
version, operation, host), five calls. At the default 15-minute window that is 480 calls a day per
environment, drawn on the `topology` area, which yields when 40 % of the share is left (§4). Backend
terms draw on the investigation area: `error_spans` is one aggregate, `compare` two to four metric
queries per window.

## 7. Where to read next

- [version-stamping.md](version-stamping.md): making each log line name the version that wrote it,
  on every deployment type, and reading the connector's verdict.
- [quickstart](../../specs/005-datadog-connector/quickstart.md): replaying the recorded corpus, then a
  live `--dry-run` and `--once`.
