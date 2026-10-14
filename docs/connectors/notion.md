<!-- SPDX-License-Identifier: Apache-2.0 -->

# The Notion connector

Written for the **workspace administrator** who creates the Notion integration and shares databases
with it, and the operator who runs `aisre feed notion`.

The connector registers the pages of the Notion databases an operator names (postmortems, runbooks)
as knowledge documents in the graph, linked to the services they concern. An investigation can then
cite "the checkout postmortem of 2026-05-14" when it looks at checkout. The connector reads page
titles, up to three named summary properties, one named property that says which services a page
concerns, and the page's dates. It never reads a page's body, never reads a people property, and
never writes anything to Notion.

Specification: [`specs/007-notion-knowledge/`](../../specs/007-notion-knowledge/spec.md).
Contract: [`contracts/notion-connector.md`](../../specs/007-notion-knowledge/contracts/notion-connector.md).
Shared names and decisions: [ADR-0013](../decisions/ADR-0013-shared-contract-additions-for-007.md).

## 1. The integration, and what it is shared with

1. In Notion, **Settings → Connections → Develop or manage integrations → New integration**. Make it
   an **internal** integration in the workspace whose databases you will name.
2. Under **Capabilities**, tick **Read content** and nothing else: no Update content, no Insert
   content, no comment capability, and **No user information**.
3. Copy the secret (`ntn_…`) into `SRE_AGENT_NOTION_TOKEN` on the host that runs the connector.
4. Open each database the connector should read and **… → Connections → add the integration**. The
   integration sees exactly what is shared with it; sharing a database is what puts it in scope.

Notion does not report an integration's capabilities through its API, so the connector cannot check
step 2 itself. The operator states it instead: the configuration must carry
`acknowledge_read_only: [notion.capabilities]`, or `feed notion` exits with code 3 before any call.
The run records its credential evidence as `operator_asserted`, which is what it is (ADR-0013
item 4). What the connector **can** prove is the list of requests it is able to make, below.

At start the connector reads `GET /v1/users/me` and refuses to run if the integration's workspace name
is not the `workspace` label in the configuration, so a secret from another workspace cannot feed this
source.

## 2. The published read-only operation surface

Every request the connector can make. An operation absent from this list is refused by the issuer
before it leaves the process, and the attempt is recorded. The data-source query is a `POST` because
Notion puts the filter in the body; it is declared as a **named query** with the reason it changes
nothing, and `PUT`, `PATCH` and `DELETE` are refused whatever is declared.

| area | operation | why |
|---|---|---|
| auth | `GET /v1/users/me` | the integration's own bot user, whose workspace name is checked against the configured label (FR-003) |
| binding | `GET /v1/databases/{id}` | a configured database's title and data sources, to resolve the binding at start (FR-010, research R5) |
| pages | `POST /v1/data_sources/{id}/query` | named query: Notion documents it as returning the pages of a data source that match a filter and sort; its body carries only filter, sorts, start_cursor and page_size, and no field creates, updates or deletes anything (FR-040) |
| pages | `GET /v1/pages/{id}` | one registered page no longer returned by its data source, read once to tell trashed or moved from gone (FR-043) |
| pages | `GET /v1/pages/{id}/properties/{property_id}` | the rest of a named summary or entity property cut at 25 references in a query result (research R7) |

**The table above and the code are one list.** It lives in
[`internal/notionx/client.go`](../../internal/notionx/client.go), and a test parses this page and
compares it with `notionx.Surface` in both directions. This surface is **enforced**: the client has no
other path to the network.

## 3. Configuration

```yaml
workspace: acme                     # the label in the source id, notion:acme; must be the integration's workspace name
principal: oidc:riley@example.com   # who signs this configuration; recorded as configured_by
acknowledge_read_only: [notion.capabilities]
poll_interval: 15m
reconcile_every: 24h
databases:
  - database: 0a1b2c3d4e5f60718293a4b5c6d7e8f9
    kind: postmortem                # postmortem | runbook
    entity_property: Services       # select, multi-select, status or relation
    entities:                       # value → graph entity; for a relation, keys are related page ids
      Checkout: k8s.service=checkout
      Payments: k8s.service=payments
    summary_properties: [Summary, Root cause, Resolution]   # at most 3: title, rich text, select or status
    authored_property: Incident date                       # optional date; else the page's creation time
  - database: 1b2c3d4e5f60718293a4b5c6d7e8f90a
    kind: runbook
    entity_property: Service
    entities:
      Checkout: k8s.service=checkout
    summary_properties: [Purpose]
```

A page is registered only when its entity property carries at least one value the map names. A value
the map does not name links nothing and is reported, so an operator can extend the map rather than
guess why a postmortem is never cited.

## 4. What is registered

Each page becomes one `KNOWLEDGE_DOC` node, `notion.page=<page id>`, with:

| prop | value |
|---|---|
| display name | the page's title, plain text |
| `kind` | the binding's kind |
| `summary` | the named properties, plain text, joined with ` — `, at most 600 characters (`summary_truncated` says when cut) |
| `location` | the page's URL |
| `authored_at` | the named date, else the page's creation time |
| `registered_by` | `notion:<workspace>` |
| `configured_by` | the configuration's `principal` |
| `link_provenance` | `notion/notion-db@1 configured by <principal>` |

and one `CONCERNS` edge per mapped value, carrying `rule`, `database`, `property` and `property_value`: the
clause that made the link. A user mention inside a summary property is removed before anything is
stored.

**Time.** A page's links are valid from its creation time when it is first registered, and from its
`last_edited_time` for every later change. A value removed from the entity property ends its link at
the edit that removed it. A page moved to the trash, archived or moved out of the database ends all
its links at its last edit; a page that answers 404 ends them at the last instant the connector saw
it. The node itself is never deleted, so an investigation replayed as of an earlier instant still
cites what was true then.

**When it is read.** Every `poll_interval` (default 15 minutes) the connector queries each data source
for pages edited since the last poll, less two minutes of overlap. At start and every
`reconcile_every` (default daily) it lists every page and reads, once each, the registered pages the
list no longer returns. A database that cannot be read is a coverage gap: nothing is retracted for it.

## 5. What it costs

Notion allows an average of three requests per second per integration and reports nothing about what
is left. The connector spends at most three calls in each second (`static_fallback` in its usage
report) and waits for the next second rather than failing. A `429` closes the budget until the instant
Notion's `Retry-After` names; if that is more than 30 seconds away the poll is cut short and reported
as a coverage gap, never as "no pages".

## 6. Running it

```sh
export SRE_AGENT_NOTION_TOKEN=ntn_…
aisre feed notion --config notion.yaml              # poll and reconcile until stopped
aisre feed notion --config notion.yaml --once       # one reconciliation, then exit
aisre feed notion --config notion.yaml --record dir # also record the sanitised exchange
aisre feed notion --config notion.yaml --replay dir # read a recording instead of Notion
```

`--state <file>` (default `notion-state.json` beside the configuration) is where the connector keeps
what it registered, so that a restart can still notice a page that disappeared while it was stopped.

Exit codes are the CLI's: 0 ok, 1 usage or configuration, 2 Notion unreachable at start, 3 the
credential refused or the acknowledgement missing.

## 7. Recordings

A recording (`--record`) is sanitised before anything reaches disk (FR-050): page, database,
data-source and workspace ids are pseudonymised; the page URL is dropped (a replay rebuilds a URL
from the pseudonymised id); titles, summaries and mapped values are kept, because they are what
retrieval ranks and what the map reads. People never enter a payload: the connector does not decode
`created_by`, `last_edited_by` or people properties at all. A recording of a real workspace stays
private; the public repository carries only the synthetic twin
[`fixtures/notion-knowledge-twin-01`](../../fixtures/notion-knowledge-twin-01/manifest.yaml).
