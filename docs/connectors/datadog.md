<!-- SPDX-License-Identifier: Apache-2.0 -->

# The Datadog connector

> **Status: partial (005 T004–T008).** The operation table in §2 is enforced: it is
> `internal/feeders/datadog/requestlog.go`'s default surface, and a test compares the two in both
> directions. Nothing on this page has run against a live organisation yet; the startup gate's
> behaviour with a narrowly scoped key (research §5 O2) and the cost figures (T088) are filled in from
> recorded runs, never estimated.

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
| `apm_topology` | off | APM service map, metrics and spans — not built until an organisation needs it |
| `changes` | off | Datadog's event stream as a change source — not built until an organisation needs it |

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

To be written with T058 and the first `--dry-run` (research §5 O2): whether a narrowly scoped
application key can read its own scopes, and when `--assert-read-only` is therefore required.

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
- **The deferral order.** Some areas must leave part of the share to the areas ahead of them.
  - Monitor transitions and definitions (one read) may spend the share to the end.
  - Discovery stops when a quarter of the share is left.
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
