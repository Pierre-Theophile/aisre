# Quickstart: validating feature 008

A validation guide, not a tutorial. Each step names the requirement it proves.

## Prerequisites

- PostgreSQL 16 reachable as `$PG_DSN`, migrated (`aisre migrate`): no extension beyond 007's.
- `make build` → `bin/aisre`.
- For step 4 only: any OpenAI-compatible embeddings server. Self-hosted example:
  `ollama pull nomic-embed-text` and `base_url: http://localhost:11434/v1`, or llama.cpp's server
  (`python -m llama_cpp.server --model nomic-embed-text-v1.5.Q4_K_M.gguf --embedding true --port 8000`,
  `base_url: http://localhost:8000/v1`), which is what the recorded run below used.
- For step 5 only: 007's Notion integration, shared with a postmortem database, secret in
  `SRE_AGENT_NOTION_TOKEN`.

## 1. The default store needs nothing new (FR-005, FR-006, FR-007)

```sh
go test ./pkg/knowledge/... ./internal/store/postgres/ -run 'Conformance|KnowledgeStore'
bin/aisre serve --enable-investigation ...      # no new flag, no new service
```

Expected: `storetest` passes for the memory and Postgres stores; with no `embedding:` block `serve`
logs `knowledge: store=postgres retrieval=lexical_only`. `--knowledge-store memory` swaps the store
with no other change (SC-008).

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

Restart `serve`. Expected: `knowledge: store=postgres retrieval=hybrid openai_compatible:nomic-embed-text@768`,
`knowledge: semantic indexer started`, then `knowledge: semantic index ready; hybrid retrieval serves
from now on`; a new investigation's knowledge digest says `retrieval_mode: hybrid`. Stop the embeddings
server: the next search says `lexical_only` with the reason in its executed query, and the
investigation completes. With `api_key_env` naming an unset variable, `serve` logs
`knowledge: retrieval is lexical only: the embedding block names $VAR, which is not set`.

## 5. Notion sections (US1, US5, FR-010–FR-015, FR-040, SC-002)

```sh
bin/aisre feed notion --config notion.yaml --check
```

Expected: per page, its sections (`Signals / Symptoms (property, 875 chars)`, `Body`, …) and no section
text. Then run the feeder, declare an incident whose alert carries a symptom found only past the first
600 characters of one postmortem, and see that postmortem cited with `section_label` set, in the top
three.

## Run of 2026-10-15 (T041)

Walked on a Linux container with PostgreSQL 16, the twins' corpus and a self-hosted llama.cpp server
(`llama-cpp-python`, `nomic-embed-text-v1.5.Q4_K_M.gguf`, 768 dimensions, CPU only). Ollama's
installer could not be fetched from that container, so llama.cpp stood in; both speak the same
`/v1/embeddings`.

- §1: `storetest` passed on both stores; `serve` without an embedding block logged
  `retrieval=lexical_only`, and with `--knowledge-store memory` logged `store=memory`.
- §2: the scope and as-of tests passed; `fixture verify` passed for knowledge-notion-01, -02 and the
  new knowledge-asof-01 (0 citations of later knowledge).
- §3: with Mistral-recorded vectors, hybrid recall@3 1.000 and MRR 0.889, lexical 0.889 / 0.889,
  `gate: pass`. With vectors from the self-hosted nomic model on the same corpus: hybrid recall@3
  1.000, MRR 0.889, gate pass.
- §4: `serve` logged `retrieval=hybrid openai_compatible:nomic-embed-text-v1.5@768`, the indexer
  started and reported the index ready; 16 section vectors were stored. Without the API key variable
  set it logged the lexical-only reason above and served.
- §5: covered on the twin (`feed notion --check` golden); the live workspace is T042, owner-gated.
