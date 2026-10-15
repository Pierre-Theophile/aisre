# Implementation Plan: Postmortem Retrieval Over Full Content

**Branch**: `008-postmortem-rag` | **Date**: 2026-10-15 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/008-postmortem-rag/spec.md`, approved by the owner on
2026-10-15 with all five defaults (Q1–Q5) and Q6 (pluggability), and constitution 1.2.0.

## Summary

Three pieces, in dependency order:

1. **A knowledge store and a hybrid ranker behind published interfaces** (`pkg/knowledge`,
   `internal/investigation/workers/knowledge`): documents are stored as versioned sections outside
   the graph; the knowledge worker ranks sections lexically and semantically, fuses the two, boosts
   documents linked to the investigation's scope instead of filtering on it, and answers as of the
   investigation's pinned instants. Two stores ship: an in-memory one (tests, fixtures, the
   evaluation harness) and a PostgreSQL one in the agent's own database, the default. Both pass one
   public conformance suite that any third-party store can run.
2. **Sections from Notion** (`internal/notionx`, `internal/feeders/notion`, a new
   `KnowledgeService.PutSections` RPC): the feeder reads each page's named properties and body,
   sanitises them, and sends them as sections beside the graph events it already emits.
3. **Embeddings, the first-wave search and the retrieval gate**: an `Embedder` interface with one
   shipped adapter for any OpenAI-compatible endpoint (hosted or self-hosted), a server-side indexer
   that embeds new sections, one knowledge search in the deterministic first wave, and a retrieval
   evaluation that CI gates on.

**The approach is interface-first.** The owner's rule for an open-source agent (2026-10-15) is that
every external dependency is pluggable, optional and self-hostable. So each dependency this feature
adds (the store, the embedding provider) is an interface with a published conformance suite, a
default that needs nothing beyond what the agent already runs, and configuration that names a
vendor only as an example.

## Phase 0 — gaps found against the code

| # | gap | where | resolution |
|---|---|---|---|
| G1 | The worker ranks a `Document` built from graph props (title + ≤600-char summary); there is no place to keep section text, and 002 FR-049a forbade one until constitution 1.2.0 | `knowledge/knowledge.go`, `knowledge/graph.go` | `pkg/knowledge.Store` with sections versioned by known time (R1, R2) |
| G2 | Retrieval is a hard filter: `Corpus.Scoped` returns only documents linked to the requested entities, and `keepLinked` re-checks it | `knowledge.go:124`, `knowledge.go:275` | candidates are every document known at the pin with a valid `CONCERNS` edge to anything; scope becomes a published boost (R4); `keepLinked` is replaced by an in-scope flag |
| G3 | `KnowledgeItem` has no field for the matched section, the in-scope flag or the retrieval mode | `api/sreagent/investigation/v1/investigation.proto:282` | additive fields 10–12 and `KnowledgeDigest.retrieval_mode`; **ADR-0014** before any schema-dependent task (R9) |
| G4 | Feeders reach the server only through `IngestBatch`, which carries graph events; section text must not become a graph event (FR-001) | `pkg/feeder/emit`, `internal/server/ingest.go` | a separate `KnowledgeService.PutSections` RPC with the same token and source scoping (R7) |
| G5 | The Notion client's published surface has no block read, so the body cannot be fetched | `internal/notionx/client.go`, `docs/connectors/notion.md` §2 | add `GET /v1/blocks/{id}/children` to the surface and the doc table together (R6) |
| G6 | The model configuration has chat roles only, validated against a provider/model table | `internal/investigation/model/config.go` | an optional `embedding:` block, validated separately, whose provider set is `openai_compatible` (R5) |
| G7 | The first wave asks five telemetry questions and no knowledge question | `internal/investigation/engine/firstwave.go` | a sixth, `knowledge_search` from the triggering alert's text (R8) |

## Technical Context

**Language/Version**: Go 1.27 (floor; the repository tracks the latest stable toolchain)

**Primary Dependencies**: `internal/investigation/workers/knowledge` (worker, BM25, citation);
`internal/query` (`KnowledgeCandidates`, generalised to an unscoped read, R3); `pkg/feeder/emit`
(Connect client patterns); `internal/sanitise` (`policy_notion.go`); `internal/eval` (harness). The
Notion REST API at `Notion-Version: 2025-09-03`, plus `GET /v1/blocks/{id}/children`. Any
OpenAI-compatible `POST {base_url}/embeddings` endpoint. **No new Go module**: the embeddings call is
one hand-written HTTP request, the Postgres store uses the driver already in `go.mod`.

**Storage**: PostgreSQL 16, the agent's own database (no new service): migration `0014_knowledge.sql`
adds schema `knowledge` with `section_versions` (text, `tsvector` on the `simple` configuration,
known-time range) and `section_embeddings` (vector as `real[]`, model id). Semantic search is exact
cosine in process over a cache of the current model's vectors (R2). No extension is required; the
existing `btree_gist` is reused for the range index.

**Testing**: `go test ./...`; `pkg/knowledge/storetest` run against both shipped stores; the
knowledge worker's goldens re-run under hybrid ranking; a retrieval evaluation over
`fixtures/retrieval/*` with recorded embeddings (R10); a recorded Notion double with block reads;
`aisre fixture verify`; `golangci-lint`; `scripts/check-specs.sh`.

**Target Platform**: Linux server, the same single binary: the store and indexer inside `serve`, the
feeder in `aisre feed notion`.

**Project Type**: a change to an existing worker, a new storage interface with two implementations,
a new ingest RPC, and an extension of an existing connector, inside the single-module Go project.

**Performance Goals**: a `knowledge_search` stays in the worker's `cheap` class: one lexical query,
one in-process cosine scan, both bounded (R2: ≤ 50k sections, ~10 ms scan). SC-007: an edited page is
retrievable with its new text within 20 minutes at the default poll, p95; embedding lag is reported as
the coverage's ingestion lag.

**Constraints**: section text never enters the graph (FR-001); no person, email or secret is stored
(FR-013); retrieval as of the pin (FR-023); no embedding provider required (FR-027, FR-029); Notion's
three requests per second (007 FR-044) now also pays for block reads.

**Scale/Scope**: tens to low thousands of documents, about ten sections each; one embedding model at
a time per deployment.

## Constitution Check

*GATE: passes before Phase 0; re-checked after design (no change).* Against constitution 1.2.0.

| principle | verdict | how |
|---|---|---|
| I. Graph First | pass, with the 1.2.0 scope entry | Documents, links and their time stay in the graph; only section text and its vectors live in the knowledge store, which 1.2.0's RAG entry permits. Candidates are still read from the graph (R3) before the store is asked for text |
| II. Bitemporal | pass | Links are read at valid and known instants from the graph; sections at the known instant from the store's own range (R1) |
| III. Event-sourced | pass | Graph events unchanged. Sections are an append-only, idempotent side store keyed by content digest; a replay from empty plus the recorded sections rebuilds the same index |
| IV. Telemetry stays home | pass | Only durable knowledge is stored; the worker still refuses telemetry capabilities |
| V. Evidence-first | pass | Each item cites the matched section, the link provenance, the in-scope flag, the scorer and embedding model versions and the retrieval mode; the query text is in the evidence chain |
| VI. Resolution auditable | pass | Entities still come only from the operator's map; nothing is inferred from text |
| VII. Read-only by default | pass | One new Notion `GET`; the embeddings call sends section text to a provider the operator chose, and none is required |
| VIII. Evaluation | pass | A retrieval evaluation with a CI gate (FR-042) beside the investigation audit |
| IX. Open schema | pass | ADR-0014 publishes the proto fields, the RPC, the store and embedder interfaces and the scorer `knowledge/hybrid@1` before code depends on them |
| X. Simplicity | pass, see Complexity Tracking | No vector database, no new module, one embeddings adapter that covers every OpenAI-compatible server |
| Scope: RAG only over linked durable knowledge | pass | A document with no `CONCERNS` edge is never indexed or returned (FR-002) |

## Project Structure

### Documentation (this feature)

```text
specs/008-postmortem-rag/
├── spec.md
├── plan.md              # this file
├── research.md          # R1–R10
├── data-model.md        # section, version, embedding, search, fixture
├── contracts/
│   └── knowledge-store.md   # Store and Embedder interfaces, PutSections, proto fields, config, scorer
├── quickstart.md        # validation run
├── checklists/
│   └── requirements.md
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
docs/decisions/ADR-0014-shared-contract-additions-for-008.md
docs/knowledge/retrieval.md                      # operator guide: stores, embedders, evaluation

api/sreagent/investigation/v1/investigation.proto  # KnowledgeItem 10–12, KnowledgeDigest.retrieval_mode
api/sreagent/knowledge/v1/knowledge.proto          # KnowledgeService.PutSections

pkg/knowledge/                     # public: what a third-party store or embedder implements
├── store.go                       # Store, Section, SectionVersion, LexicalHit, SemanticHit
├── embedder.go                    # Embedder, ModelID
├── memory/                        # in-memory Store (tests, fixtures, eval harness)
└── storetest/                     # conformance suite any Store runs

internal/
├── store/postgres/
│   ├── migrations/0014_knowledge.sql
│   └── knowledge_store.go         # the default Store
├── embedding/
│   ├── openaicompat.go            # the one shipped Embedder adapter
│   └── indexer.go                 # embeds new section versions for the configured model
├── query/knowledge.go             # KnowledgeDocuments: every linked document at T (R3)
├── investigation/
│   ├── workers/knowledge/
│   │   ├── hybrid.go              # fusion, boost, per-document grouping (knowledge/hybrid@1)
│   │   └── knowledge.go           # candidates from the graph, text from the store
│   ├── engine/firstwave.go        # the knowledge question (R8)
│   └── model/config.go            # the embedding block
├── server/knowledge.go            # PutSections handler, source scoping
├── notionx/client.go              # + GET /v1/blocks/{id}/children
├── feeders/notion/sections.go     # properties and body → sections
├── sanitise/policy_notion.go      # mentions, emails, secret shapes in section text
└── cli/feed_notion.go             # --check lists sections

fixtures/
├── notion-knowledge-twin-02/      # twin with bodies and long sections
└── retrieval/                     # retrieval fixtures with recorded query and section vectors
```

**Structure Decision**: the interfaces live in `pkg/knowledge` because they are the extension point
the owner asked for: a third-party store or embedder is a Go package that imports `pkg/knowledge` and
runs `storetest`, the same way connectors import `pkg/feeder`. The shipped implementations live in
`internal`, beside the code that already talks to Postgres and HTTP.

## Delivery

Three pull requests, each green and merged on its own, as 007 was built:

1. **PR 1 — store, ranker, schema (US2, US3, US4 lexical).** ADR-0014; `pkg/knowledge` with the
   memory store and `storetest`; hybrid ranking with lexical only; scope as boost; as-of sections;
   proto fields; the retrieval evaluation in lexical-only mode with its CI gate.
2. **PR 2 — sections from Notion, stored in Postgres (US1 lexical, US5).** Migration 0014 and the
   Postgres store (passing `storetest`); `PutSections`; block reads; section building and
   sanitisation; `--check` reporting sections; twin 02.
3. **PR 3 — embeddings and the first wave (US1, SC-001).** `Embedder`, the OpenAI-compatible adapter,
   the indexer, the `embedding:` configuration and fallback; the first-wave knowledge question; the
   hybrid evaluation gate on recorded vectors; the operator guide.

Then the owner-gated steps carried over from 007: record the LDMA workspace (007 T030, now with
sections) and measure SC-002 on it.

## Design decisions (detail in research.md)

- **R1 — Sections are versioned by known time.** Each version carries `known_from`/`known_to`; a new
  text closes the previous version. Valid time comes from the graph's links, not the store.
- **R2 — The default store is the agent's own Postgres.** Lexical by `tsvector` (`simple`), semantic by
  exact cosine over an in-process cache. FR-006's "no external service" is read as "nothing beyond the
  database the agent already requires"; an embedded file store was rejected (R2).
- **R3 — Candidates come from the graph, unscoped.** Every `KNOWLEDGE_DOC` (and reviewed investigation)
  known at the pin with at least one `CONCERNS` edge valid at T; the in-scope flag is whether one of
  those edges points into the requested entities.
- **R4 — `knowledge/hybrid@1`.** Reciprocal rank fusion (k = 60) of the lexical and semantic section
  rankings, best section per document, ×1.5 when in scope, ties by document id then label.
- **R5 — One embeddings adapter.** `openai_compatible` (`POST {base_url}/embeddings`) serves Mistral,
  OpenAI, Ollama, vLLM, Hugging Face TEI, LocalAI and llama.cpp; anything else implements `Embedder`.
- **R6 — Notion bodies.** Block children read to depth 3 for pages whose `last_edited_time` moved;
  headings start sections; media, embeds, child pages and databases are skipped.
- **R7 — `PutSections`.** A separate RPC, same token and source scoping as `IngestBatch`, idempotent by
  content digest; the server stores, the indexer embeds.
- **R8 — The first-wave question.** One `knowledge_search` from the alert's title and labels over the
  top candidates' entities, limit 5, no model in the loop.
- **R9 — ADR-0014.** Proto fields, the RPC, the interfaces, the scorer and the config block, published
  before code depends on them.
- **R10 — Retrieval evaluation.** Recall@3 and MRR per fixture; CI replays recorded vectors so the
  hybrid gate runs without a provider; thresholds 0.9 (hybrid) and the measured lexical baseline.

## Complexity Tracking

| item | why it is needed | simpler alternative rejected because |
|---|---|---|
| A store outside the graph | FR-001: section text may not enter the graph; constitution 1.2.0 | storing sections as node props would put content in the event log and every replay |
| Two interfaces in `pkg/` | the owner's pluggability rule (Q6) | a single built-in store and provider would hard-wire a vendor into an open-source agent |
| In-process cosine scan instead of an ANN index | exact, deterministic (FR-024), no extension required, cheap at the stated scale | `pgvector` is not available on every managed Postgres and adds a required extension; it remains an adapter option |
