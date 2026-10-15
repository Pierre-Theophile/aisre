# Quickstart: validating feature 008

A validation guide, not a tutorial. Each step names the requirement it proves.

## Prerequisites

- PostgreSQL 16 reachable as `$PG_DSN`, migrated (`aisre migrate`): no extension beyond 007's.
- `make build` → `bin/aisre`.
- For step 4 only: any OpenAI-compatible embeddings server. Self-hosted example:
  `ollama pull nomic-embed-text` and `base_url: http://localhost:11434/v1`.
- For step 5 only: 007's Notion integration, shared with a postmortem database, secret in
  `SRE_AGENT_NOTION_TOKEN`.

## 1. The default store needs nothing new (FR-005, FR-006, FR-007)

```sh
go test ./pkg/knowledge/... ./internal/store/postgres/ -run 'Conformance|KnowledgeStore'
bin/aisre serve --enable-investigation ...      # no new flag, no new service
```

Expected: `storetest` passes for the memory and Postgres stores; `serve` logs
`knowledge: store=postgres retrieval=lexical_only (no embedding block)`.

## 2. Scope ranks, time gates (US2, US3, SC-003, SC-004)

```sh
go test ./internal/investigation/workers/knowledge/ -run 'Hybrid|InScopeBoost|AsOfSections|CrossService'
bin/aisre fixture verify fixtures/incidents/knowledge-notion-01
```

Expected: an out-of-scope postmortem with the best match is returned and flagged `in_scope: false`; an
equally matching in-scope one ranks above it; a section edited after the pin is matched in its earlier
text; a page created after the pin is never returned.

## 3. Retrieval evaluation (US4, FR-042, FR-043, SC-001)

```sh
bin/aisre eval retrieval fixtures/retrieval/
```

Expected: a table of recall@3 and MRR per fixture for `hybrid` (recorded vectors) and `lexical_only`,
the aggregate, and `gate: pass`. Deleting the body sections from one fixture's `documents.yaml` makes
the gate fail.

## 4. A self-hosted embedder (FR-026–FR-029, SC-006, SC-008)

Add to the model configuration:

```yaml
embedding:
  provider: openai_compatible
  base_url: http://localhost:11434/v1
  model: nomic-embed-text
  dimensions: 768
  query_prefix: "search_query: "
  document_prefix: "search_document: "
```

Restart `serve`. Expected: `knowledge: indexer model=openai_compatible:nomic-embed-text@768 pending=N`
then `pending=0`; a new investigation's knowledge digest says `retrieval_mode: hybrid`. Stop the
embeddings server: the next digest says `lexical_only` and the investigation completes.

## 5. Notion sections (US1, US5, FR-010–FR-015, FR-040, SC-002)

```sh
bin/aisre feed notion --config notion.yaml --check
```

Expected: per page, its sections (`Signals / Symptoms (property, 875 chars)`, `Body`, …) and no section
text. Then run the feeder, declare an incident whose alert carries a symptom found only past the first
600 characters of one postmortem, and see that postmortem cited with `section_label` set, in the top
three.
