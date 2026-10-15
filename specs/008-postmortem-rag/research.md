# Research: Postmortem Retrieval Over Full Content

Decisions taken while planning feature 008. Each is reversible and records what was rejected.
Statements about Notion's API are from its public reference at `Notion-Version: 2025-09-03`, and
statements about embeddings endpoints from the OpenAI-compatible `/v1/embeddings` shape that the
named servers document; the ones a task must confirm against a live call are marked **verify**.

## R1. Sections are versioned by known time; valid time stays in the graph

**Decision**: a section version is `(document_ref, label, part, text_digest, text, known_from,
known_to)`. `known_from` is the instant the server accepted it; storing a different text for the same
`(document_ref, label, part)` closes the open version at that instant. A document whose page is
trashed or moved keeps its versions; whether it is a candidate is decided by its graph links (R3).

**Rationale**: the graph already answers "was this document linked to anything at T" bitemporally
(007). The store only has to answer "what did this section say, as far as the agent knew at T", which
is one time axis. Splitting the two keeps the store small and lets any store implement it.

**Rejected**: valid time on sections (Notion's `last_edited_time`). It would let a replay read an edit
the agent received after the pin, if the page's edit time is earlier than the delivery, which is
exactly the leak SC-003 forbids.

## R2. The default store is the agent's own PostgreSQL

**Decision**: `postgres.KnowledgeStore` in the agent's database (migration 0014):

- lexical: a `tsvector` on the `simple` configuration (no stemming, so French and English
  postmortems score alike), GIN-indexed; `websearch_to_tsquery('simple', q)`; `ts_rank_cd` for order;
  top 50 sections known at the pin;
- semantic: vectors as `real[]`; the store keeps an in-process cache of the configured model's vectors
  with their known ranges, refreshed incrementally (the table is append-only); exact cosine over the
  versions known at the pin, top 50.

At the stated scale (≤ 50k sections × 1,024 dimensions ≈ 200 MB as float32, ~10 ms per scan) this is
exact and deterministic.

FR-006 says the default store needs "no external service and no network access". The agent cannot run
without its PostgreSQL, so the default uses it and adds nothing; this is the reading the plan takes,
and quickstart step 1 shows `serve` needing no new configuration.

**Rejected**:
- an embedded file store (bbolt, SQLite) beside Postgres: a second persistence layer with its own
  backup story, for no gain;
- `pgvector`: an extension many managed and self-hosted Postgres installs do not have; it becomes a
  candidate adapter once a deployment outgrows the exact scan, and `storetest` is what it would pass;
- a dedicated vector database (Qdrant, Weaviate, OpenSearch) as default: a new required service.
  Any of them can be plugged in by implementing `pkg/knowledge.Store` (Q6).

## R3. Candidates come from the graph, without the scope filter

**Decision**: `query.Engine.KnowledgeDocuments(ctx, at)` returns every `KNOWLEDGE_DOC` version known at
`at.Observed` with at least one `CONCERNS` edge valid at `at.Valid` and known at `at.Observed`, with
all its linked entity ids, plus the reviewed past investigations already admitted by 007 R3. The worker
marks a candidate `in_scope` when any linked entity is in the request's `entity_ids`. The store is then
asked only for sections of candidate documents.

**Rationale**: the graph remains the authority on what exists and what it concerns, and on time; the
store cannot resurrect a trashed page or invent an unlinked one (FR-002, FR-023).

**Rejected**: asking the store first and filtering by the graph afterwards: a top-50 from the store
could be all out-of-time documents and leave nothing.

## R4. `knowledge/hybrid@1`

**Decision**:

1. The store returns the top 50 sections by lexical rank and the top 50 by semantic rank, each among
   sections of candidate documents known at the pin.
2. Section score = Σ over the two lists of `1 / (60 + rank)` (reciprocal rank fusion, k = 60); a list
   that is absent (no embedder) contributes nothing.
3. Document score = its best section's score; the document carries its best two sections (FR-022).
4. ×1.5 when the document is in scope (R3). An equally matching in-scope document therefore always
   ranks first (SC-004); an out-of-scope document can still outrank a weak in-scope one.
5. Ties: document id, then section label, then part.

`ScorerVersion` becomes `knowledge/hybrid@1`; the old BM25 scorer stays as the lexical ranker of the
memory store so the worker's existing goldens keep a reference.

**Rationale**: RRF needs no score calibration between a `ts_rank_cd` and a cosine, which differ per
store and per model, and is the standard fusion for hybrid retrieval.

**Rejected**: a weighted sum of normalised scores (needs per-model calibration); a model re-ranker
(out of scope, spec §Out of scope).

## R5. One embeddings adapter, for every OpenAI-compatible endpoint

**Decision**: `pkg/knowledge.Embedder` with `Model() ModelID` and `Embed(ctx, texts, purpose)`. One
shipped adapter, `openai_compatible`, configured in the model configuration:

```yaml
embedding:
  provider: openai_compatible
  base_url: https://api.mistral.ai/v1      # example; any server speaking POST /embeddings
  model: mistral-embed
  api_key_env: MISTRAL_API_KEY             # optional: a local server needs none
  dimensions: 1024
```

The same adapter serves Mistral, OpenAI, Ollama (`/v1/embeddings`), vLLM, Hugging Face TEI, LocalAI and
llama.cpp's server (**verify** each in the operator guide's table before listing it as tested). Batches
of 32, 10-second timeout per query embedding, retries on 429/5xx with `Retry-After`. No block, no key,
or a failed call → lexical only, recorded (FR-027).

**Rationale**: one wire format covers hosted and self-hosted servers, which is what the owner asked; a
vendor SDK per provider would multiply dependencies.

**Rejected**: running an embedding model inside the agent binary (ONNX runtime): a cgo dependency and a
model download the operator did not choose; a self-hosted server does the same job behind the adapter.

## R6. Reading Notion page bodies

**Decision**: add `GET /v1/blocks/{id}/children` (paginated, 100 per call) to the published surface.
For each page whose `last_edited_time` moved since its last read: read its children, recurse into
blocks with `has_children` (toggles, callouts, columns, list items) to depth 3, and stop at 1,000
blocks per page (reported). Headings 1–3 start a new section labelled with their text; text-bearing
blocks (paragraph, list items, quote, callout, toggle title, code, table rows) append plain text.
Skipped: `child_page`, `child_database`, `link_to_page`, embeds, files, images, bookmarks, synced
blocks from other pages. A user mention inside rich text is dropped before the text is built (as 007
does for summaries).

Cost: one call per 100 blocks per edited page, inside 007's three-per-second budget; a seven-page
database of typical postmortems is about 10 calls.

**Rejected**: Notion's markdown export (not an API); reading bodies on every poll (unchanged pages
would cost calls for nothing).

## R7. `KnowledgeService.PutSections`

**Decision**: a new Connect RPC on the server, `PutSections(source_id, document_ref, sections[])`,
authorised by the same feeder token and source scoping as `IngestBatch` (a token for
`notion:LDMA` may write only `notion.page=` documents of that source). It is idempotent: a version
whose digest equals the open version is a no-op. A document whose `KNOWLEDGE_DOC` node is not yet
projected is accepted and stored; it becomes a candidate only when the graph says so (R3). The feeder
sends sections after the page's graph events.

**Rejected**: putting sections in graph event props (FR-001); letting feeders write to the store
directly (they would need database credentials and the embedder key).

## R8. The first-wave knowledge question

**Decision**: after the five telemetry questions, one `knowledge_search` with `entity_ids` = the top
candidates' entities and `query_terms` = the triggering alert's title and the values of its
non-identifying labels, limit 5. Its items enter the ledger as knowledge-derived evidence (002 FR-051);
no hypothesis is judged from them. The query is recorded (FR-032).

**Rationale**: the investigator then starts with the most similar postmortems in context, without a
model turn spent asking for them (FR-031).

## R9. ADR-0014

Published before any dependent task: `KnowledgeItem` fields `section_label = 10`, `in_scope = 11`,
`matched_text = 12` (the best section's sanitised text, ≤ 1,200 characters, replacing the summary as
excerpt when present); `KnowledgeDigest.retrieval_mode = 3` (`hybrid` | `lexical_only`) and
`embedding_model = 4`; `knowledge.v1.KnowledgeService`; the `pkg/knowledge` interfaces; the scorer
name; the `embedding:` block. All additive, so `buf breaking` passes and the algebra version does not
move (no term changes; `query_terms` already exists).

## R10. Retrieval evaluation

**Decision**: a retrieval fixture is a directory with the documents' sections, their links, the query
(alert text, entity scope, pinned instants), the expected document and its expected in-scope flag, and
**recorded vectors** for sections and query from one named model. CI replays the vectors through a
`recorded` embedder, so the hybrid path is gated without a provider or a key. `aisre eval retrieval`
prints recall@3 and MRR per fixture and in aggregate, for hybrid and lexical only. Gates: hybrid
recall@3 ≥ 0.90 (SC-001) and hybrid ≥ lexical only; lexical recall@3 ≥ the baseline measured when PR 1
lands, so a regression in either path fails CI.

Public fixtures are synthetic twins; the owner's LDMA postmortems seed a private set (SC-002), never
committed.
