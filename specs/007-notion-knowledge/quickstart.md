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
go test ./internal/eval/ ./internal/investigation/workers/knowledge/ -run 'Notion|Planted|PostmortemAlone|GraphCorpus'
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

Without a live integration, the same report over the twin's recording (the configuration as in
`internal/cli/feed_notion_test.go`):

```sh
bin/aisre feed notion --config twin.yaml --check --replay fixtures/notion-knowledge-twin-01
```

## 6. Sanitisation (SC-005)

```sh
bin/aisre fixture campaign scan <recording dir>       # a recording campaign (T030)
scripts/check-no-secrets.sh fixtures/notion-knowledge-twin-01 fixtures/incidents/knowledge-notion-01
go test ./internal/feeders/notion/ ./internal/notionx/ -run 'Scan|NoPerson|Operations'
```

Expected: no person, email, page body or canary token from a dropped property. The twin plants
canaries in a user mention and an unbound property, and the client test plants them in `created_by`
and a people property; none survives.

## Run 2026-10-14 (the twin; §5 live waits on T030)

| step | result |
|---|---|
| §1 | Partly shown. On a database loaded with `knowledge-notion-01` and `serve --enable-investigation --recording-root fixtures/incidents/knowledge-notion-01`, `investigate otel.service.name=checkout --at 2026-09-01T14:32:00Z` concluded (exit 0), outcome `unknown`, with `k8s.change=shop/payments@rev7` marked supported by four source-of-truth answers; no coverage audit was published in that fresh database, so π₀ = 1 and every change candidate sat at prior 0. It asked no `knowledge_search`: with no model configured the run is the deterministic first wave, which does not include retrieval, so the citation needs a model to appear in a served run. That the worker is registered by default is `TestTheKnowledgeWorkerIsRegisteredByDefault`; that it answers over the served graph is §2. `worker list` and `worker call` do not show the knowledge worker, because they build workers from a recorded world and the knowledge worker reads the graph. |
| §2 | ok. The linked postmortem is cited with its identifier, linked entities, provenance `notion/notion-db@1 configured by oidc:operator-twin`, age (110 days) and location, in each of the three declared runs; the reporting postmortem and the one written after the alert never are; the fixture's three `knowledge_search` predicates are satisfied by the worker's answer; the planted instruction is quoted in the excerpt and the verdict and worker calls equal knowledge-scope-01's; a hypothesis resting on the postmortem alone is refused `supported`. `fixture verify fixtures/incidents/knowledge-notion-01` passes every step. |
| §3 | ok. 30 events from empty, 5 goldens; doubled all `DUPLICATE_NOOP`; 6 shuffles agree. |
| §4 | ok. `TestSurfaceIsTheFivePublishedReads`, `TestSurfaceMatchesThePublishedPage`. |
| §5 | Over the twin's recording only (above): data sources, each configured property found or not, registered titles, each page not registered with its reason. Live waits on the integration (T030). |
| §6 | ok. `check-no-secrets.sh` clean over both fixtures; it refuses each Notion person shape and a canary when planted (`TestTheIndependentScanRefusesANotionPerson`). `fixture campaign scan` needs a campaign directory, which only T030 produces. |

Finding while walking §2: the fixture's predicates first used `documents[*].document_ref`, copied
from knowledge-scope-01, which never resolves against a knowledge digest
(`knowledge.items[*].document_id`). knowledge-notion-01 now uses the real paths; knowledge-scope-01
still carries the old ones and is left for its own change.
