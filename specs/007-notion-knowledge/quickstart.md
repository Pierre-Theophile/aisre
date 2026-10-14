# Quickstart: validating feature 007

A validation guide, not a tutorial. Each step names the requirement it proves.

## Prerequisites

- PostgreSQL 16 reachable as `$PG_DSN`, migrated (`aisre migrate`).
- `make build` → `bin/aisre`.
- For the live steps only: a Notion internal integration with **Read content** only, shared with a
  postmortem database and a runbook database, its secret in `SRE_AGENT_NOTION_TOKEN`.

## 1. The knowledge worker answers live (US2, FR-005–FR-007)

```sh
bin/aisre knowledge link notion.page=demo-runbook --kind runbook --title "Checkout pool exhaustion" \
  --summary "raise the pool before scaling workers" --entity k8s.service=checkout \
  --principal oidc:riley@example.com
bin/aisre serve --enable-investigation ...        # knowledge worker registered by default
bin/aisre investigate declare --service k8s.service=checkout ...   # flags as in contracts/cli.md of 002
```

Expected: the investigation's evidence holds a `knowledge_search` answered by `knowledge`, citing
`runbook notion.page=demo-runbook`, with provenance and age. With `--no-knowledge-worker`, the term
is reported unavailable and the investigation still concludes.

## 2. Scoping and time travel through the harness (SC-002, FR-006, FR-007)

```sh
go test ./internal/eval/ -run 'KnowledgeScope|KnowledgeNotion'
bin/aisre fixture verify fixtures/incidents/knowledge-notion-01
```

Expected: the linked postmortem is cited; the out-of-subgraph one and the one authored after the pin
never are; replaying twice gives the same candidate set.

## 3. The feeder from a recording (US3, FR-040–FR-043)

```sh
bin/aisre fixture verify fixtures/notion-knowledge-twin-01
```

Expected: replay from empty, double delivery and shuffle all yield one node per page, one edge per
mapped value, the edited page as a later version, the trashed page's edges ended at its own edit
instant, the restored page as a new interval.

## 4. Read-only proof (FR-001, SC-004)

```sh
go test ./internal/notionx/ -run Surface
```

Expected: the surface equals the table in `docs/connectors/notion.md` in both directions, and a
recorded run issued nothing else.

## 5. Live check (US5)

```sh
bin/aisre feed notion --config notion.yaml --check
```

Expected: the workspace label matches; each database lists its data sources, resolved properties,
registered and skipped pages with reasons; without `acknowledge_read_only` the command exits 3.

## 6. Sanitisation (SC-005)

```sh
bin/aisre fixture campaign scan <recording dir>
```

Expected: no person, email, page body or canary token from a dropped property.
