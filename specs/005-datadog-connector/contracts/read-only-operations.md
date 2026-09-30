# Contract — The Datadog operations the connector can issue

**Date**: 2026-09-27 · **Requirements**: FR-002–FR-004, FR-054, SC-015 · **Decision**: ADR-0010 (Gap G3)

The connector's whole request surface, per capability. It is the literal set the binary can issue:
the metered transport refuses any request not on this list, and `docs/connectors/datadog.md`
publishes it verbatim, tested against the code as the GitHub and Vercel lists are.

---

## 1. Why POST appears on a read-only list

The shipped read-only surface accepts GET and HEAD and panics on anything else
(`pkg/feeder/readonly.go:83`). Datadog's log search and aggregation are **POST** — the query is a
JSON body — so under that rule the backend could not answer a single log term.

The rule is extended, not loosened. A POST is declarable **only as a named operation** with:

- the exact path;
- a published justification quoting the documentation's statement of what it does, and saying why
  it cannot change state;
- a test asserting the operation is on this list and that no other POST is.

Every other POST stays refused at init. So "verifiable from the set of operations the connector can
issue" (FR-004) remains true: the set is this table.

---

## 2. The operations

Host: `https://api.<site>` for the configured site (e.g. `api.datadoghq.eu`); the site is
configuration and never appears in a pointer (FR-037).

| capability | operation | method, path | reads | why it cannot write |
|---|---|---|---|---|
| (startup) | validate keys | `GET /api/v2/validate_keys` | key validity | a GET |
| (startup) | own key's scopes | `GET /api/v2/current_user/application_keys/{id}` | scopes, where permitted | a GET; a refusal is handled (§3) |
| `logs` | search logs | **`POST /api/v2/logs/events/search`** | log events in a window, paged | named query operation: the body is a query, the endpoint is documented as returning logs that match it, and it has no field that creates, updates or deletes anything |
| `logs` | aggregate logs | **`POST /api/v2/logs/analytics/aggregate`** | counts grouped by facet | named query operation, as above; returns buckets |
| `monitors` | list monitors | `GET /api/v1/monitor?group_states=all` | definitions and group states | a GET |
| `monitors` | get monitor | `GET /api/v1/monitor/{id}?group_states=all` | one monitor | a GET |
| `tags` | — | none of its own | tags arrive on the payloads above | — |
| `apm_topology` *(off)* | service dependencies | `GET /api/v1/service_dependencies` (`env`, `start`, `end`) | the services of an environment and the services each calls | a GET. Built against the published shape (T090), not yet verified against a live organisation |
| `apm_topology` *(off)* | query metrics | `GET /api/v1/query` (`query`, `from`, `to`) | APM trace metrics: hits, errors, duration percentiles | a GET; a metrics query has no field that writes a metric |
| `apm_topology` *(off)* | aggregate spans | **`POST /api/v2/spans/analytics/aggregate`** | counts and duration percentiles of spans, grouped by facet | named query operation, as the log aggregate: the body is a query, the endpoint returns buckets, and it has no field that creates, updates or deletes anything |
| `changes` *(off)* | list events | `GET /api/v2/events` (`filter[from]`, `filter[to]`, `filter[query]`, `sort`, `page[limit]`, `page[cursor]`) | the events of a window, filtered by the configured sources and tags, paged by cursor | a GET; the endpoint reads the event stream and has no field that posts, edits or deletes one. Built against the published shape (T092), not yet verified against a live organisation |

Every operation is on the default capabilities' list **only if** its capability is enabled; a disabled
capability's operations are not declared at all (FR-008b), so the transport refuses them.

**Never declared, and refused at init if anyone tries**: monitor mute, unmute, create, update,
delete; downtimes; events post; incidents of any kind; dashboards, notebooks, saved views; log
indexes, pipelines, archives, metrics or restriction queries; any `PUT`, `PATCH` or `DELETE`.

---

## 3. The startup gate (FR-003)

1. `validate_keys` must succeed, else the start is refused naming which key failed.
2. The connector asks for its application key's scopes.
   - **Answered**: any scope outside the published read-scope set for the enabled capabilities
     refuses the start, naming each offending scope.
   - **Refused by Datadog** (the key lacks `user_app_keys`, likely for a narrowly scoped key): the
     connector names what it could not verify and requires `--assert-read-only`, recorded in every
     checkpoint as `operator_asserted`. Without it, it does not start.
3. `--dry-run` runs the gate and nothing else, printing the capabilities, the operations each
   declares, and the gate's verdict.

---

## 4. Read scopes per capability

| capability | Datadog permissions |
|---|---|
| `logs` | `logs_read_data`, `logs_read_index_data` |
| `monitors` | `monitors_read` |
| `apm_topology` *(off)* | `apm_read`, `apm_service_catalog_read` — only when enabled |
| `changes` *(off)* | `events_read` — only when enabled |

No incident permission is requested in any configuration.

`logs_read_data` and `events_read` are named in the endpoint documentation cited in
[research §2.1](../research.md). The other names are **not yet confirmed** against Datadog's
permissions reference; confirming them is part of the task that builds the startup gate, and a name
that turns out wrong is corrected here rather than worked around in code.
