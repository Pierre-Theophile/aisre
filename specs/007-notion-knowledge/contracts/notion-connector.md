# Contract: Notion Knowledge Connector

The published surface of feature 007. ADR-0013 adds the shared names; this document is their detail.

## 1. Published names (ADR-0013)

| name | value |
|---|---|
| source kind | `notion` |
| source id | `notion:<workspace label>`; the label is checked against `users/me` at start |
| document ref namespace | `notion.page` (value: the page id, dashed form) |
| rule | `notion/notion-db@1` |
| registering identity | `notion:<workspace label>`, with `configured_by` = the signing principal |
| published kinds fed | `postmortem`, `runbook` (of 002's `decision`, `investigation`, `postmortem`, `runbook`) |

## 2. Operation list (read-only proof)

| operation | when | named query |
|---|---|---|
| `GET /v1/users/me` | start, `--check` | no |
| `GET /v1/databases/{id}` | start, `--check`, reconciliation | no |
| `POST /v1/data_sources/{id}/query` | every poll and reconciliation | **yes**: returns pages matching a filter; no field creates, updates or deletes |
| `GET /v1/pages/{id}` | a registered page no longer returned | no |
| `GET /v1/pages/{id}/properties/{property_id}` | a named property truncated at 25 references | no |

Nothing else is compiled. `docs/connectors/notion.md` carries this table and a test parses it and
compares it with `notionx.Surface` in both directions, as for the other REST connectors.

## 3. Credential

An internal integration secret (`ntn_` or legacy `secret_` prefix) in `SRE_AGENT_NOTION_TOKEN`.
Evidence `operator_asserted`; the configuration must carry
`acknowledge_read_only: [notion.capabilities]`, stating the integration was created with **Read
content** only, without Update content, Insert content, comment capabilities or user information.
Missing acknowledgement → exit 3 (`ExitAuth`) before any call.

## 4. Configuration

```yaml
workspace: acme                     # label in the source id
principal: oidc:riley@example.com   # who signs this configuration
acknowledge_read_only: [notion.capabilities]
poll_interval: 15m
reconcile_every: 24h
databases:
  - database: 0a1b2c3d4e5f60718293a4b5c6d7e8f9
    kind: postmortem
    entity_property: Services
    entities:
      Checkout: k8s.service=checkout
      Payments: k8s.service=payments
    summary_properties: [Summary, Root cause, Resolution]
    authored_property: Incident date
  - database: 1b2c3d4e5f60718293a4b5c6d7e8f90a
    kind: runbook
    entity_property: Service
    entities:
      Checkout: k8s.service=checkout
    summary_properties: [Purpose]
```

## 5. `aisre feed notion`

| flag | meaning |
|---|---|
| `--config <file>` | the configuration above (required) |
| `--check` | read everything, emit nothing, print the report of §6 and exit |
| `--once` | one reconciliation, then exit |
| `--db <dsn>` | PostgreSQL DSN, default `$PG_DSN` |
| `--record <dir>` | record the sanitised exchange for `fixture record` |

Exit codes are the CLI's (`internal/cli/root.go`): 0 ok, 1 usage, 2 Notion unreachable at start,
3 refused credential or missing acknowledgement.

## 6. Registration report (`--check`, and at the end of each reconciliation)

Per database: kind, data sources, properties resolved, pages registered, pages skipped with their
reason, and the budget spent. Reasons are a closed set: `no mapped entity`, `unmapped value <v>`,
`in another bound database`, `archived`, `outside the configured databases` (count only). Page
titles are printed; summaries and page bodies are not.

## 7. `serve`

`serve --enable-investigation` registers the knowledge worker by default; `--no-knowledge-worker`
leaves it out, and `knowledge_search` is then reported unavailable (spec US2 scenario 2).
