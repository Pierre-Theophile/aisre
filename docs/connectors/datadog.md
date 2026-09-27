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

To be written from a recorded run (T088).
