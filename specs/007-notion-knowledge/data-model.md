# Data Model: Notion Knowledge Connector

No table and no event type is added. Everything below is a property set on the existing
`KNOWLEDGE_DOC` node and `CONCERNS` edge (002 T027, ADR-0005 D3), operator configuration, or a feeder
checkpoint.

## Database binding (configuration, signed)

| field | type | rule |
|---|---|---|
| `database` | Notion database id | required; unique in the configuration |
| `kind` | `postmortem` \| `runbook` | required (FR-010) |
| `entity_property` | property name | required; select, multi-select or relation (FR-030) |
| `entities` | map value → graph ref (`<namespace>=<value>`) | required; for a relation, keys are related page ids |
| `summary_properties` | up to 3 property names | optional; title, rich text, select (FR-021) |
| `authored_property` | property name | optional; a date; else the page's `created_time` (FR-020) |

The configuration also carries `workspace` (the label in the source id), `principal` (the operator
who signs it), and `acknowledge_read_only: [notion.capabilities]` (FR-003).

## `KNOWLEDGE_DOC` node (one per page)

| field | value |
|---|---|
| ref | `notion.page=<page id>` (ADR-0013) |
| display name | page title, plain text |
| `kind` | the binding's kind; on conflict, the first binding's (Edge Cases) |
| `summary` | named properties joined with ` — `, plain text, people removed, ≤ 600 characters, `…` when truncated |
| `summary_truncated` | bool |
| `location` | the page's URL |
| `authored_at` | RFC 3339 |
| `registered_by` | `notion:<workspace>` |
| `configured_by` | the signing principal |
| `registered_at` | RFC 3339, the observing poll |
| `link_provenance` | `notion/notion-db@1 configured by <principal>` |
| valid from | the page's `last_edited_time` for the version |

These are the props `LinkRequest.Events` already writes, plus `summary_truncated` and
`configured_by`. The worker reads `kind`, `summary`, `location`, `authored_at`, `link_provenance`.

## `CONCERNS` edge (one per mapped value)

| field | value |
|---|---|
| src → dst | `notion.page=<id>` → the mapped entity |
| `rule` | `notion/notion-db@1` |
| `database`, `property`, `property_value` | what produced the link (FR-033); `value` is a telemetry-sample key the graph refuses |
| valid | from the page version that first carried the value, to the `retract_edge` end of R6 |

## Page lifecycle

```text
seen in a bound data source, ≥1 mapped value ──► registered (node + edges)
registered ── value added / removed ──────────► edge upserted / retracted at last_edited_time
registered ── title / summary edited ─────────► new node version at last_edited_time
registered ── trashed / moved out ────────────► all edges retracted at last_edited_time; node kept
registered ── gone (404) ─────────────────────► all edges retracted at last seen; node kept
retracted  ── restored ───────────────────────► edges upserted again, new interval
seen, no mapped value ────────────────────────► not registered; reported (`no mapped entity`)
```

## Event ids

`sha256(source id, ref, "node" | "edge:<entity>", content digest)`, so a re-poll of an unchanged page
emits ids the log already holds (FR-041). Content digest covers title, summary, kind, location and
authored instant for the node, and nothing but the endpoints for an edge.

## Corpus query result (`knowledge.Document`)

Unchanged type. `HopsFromFocus` is 0 (research R1). Past investigations fill it as in research R3.

## Feeder checkpoint

Per data source: `watermark` (newest `last_edited_time` processed), `last_reconciled_at`, and the set
of page ids registered with their last-seen instant, so absence can be detected (R6).
