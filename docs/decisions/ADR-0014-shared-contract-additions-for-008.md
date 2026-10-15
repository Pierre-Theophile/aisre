# ADR-0014: The shared contract additions for feature 008

- Status: **Accepted** (2026-10-15)
- Deciders: project owner (the 008 scope, its defaults, the pluggability rule, the plan and the
  tasks, 2026-10-15), Claude (design, documented here)
- Source: `specs/008-postmortem-rag/plan.md` §"Phase 0 — gaps found against the code" (G1–G7),
  `research.md` R1–R10, `contracts/knowledge-store.md`. Written under
  `specs/008-postmortem-rag/tasks.md` T001.

## Context

Feature 007 made a running deployment retrieve postmortems, but only over what the graph holds about
them: a title and a summary of at most 600 characters. Feature 008 retrieves over their content.
Constitution 1.2.0 (amended 2026-10-15) sets the boundary: the graph scopes and dates the RAG index,
a document's content lives in a knowledge store outside the graph, versioned by the instant each
version became known, and the graph itself carries no document content.

The project is open source, and the owner's rule is that every external dependency is pluggable,
self-hosted options included. So the store and the embedding model are interfaces a third party
implements, and the wire a third-party source writes to is published. Each item below is read by
more than one package or by people outside the project, so it is decided here (constitution IX).

## Decision

### Item 1 — the investigation digest says what was matched and whether it was in scope

All additive to `sreagent.investigation.v1`; the algebra version does not move, because no term
changes (`knowledge_search` already carries `query_terms`).

| message | field | number | meaning |
|---|---|---|---|
| `KnowledgeItem` | `section_label` | 10 | label of the best-matching section; empty when nothing matched |
| `KnowledgeItem` | `in_scope` | 11 | a linked entity is one of the request's `entity_ids` |
| `KnowledgeItem` | `matched_text` | 12 | that section's sanitised text, at most 1,200 characters |
| `KnowledgeDigest` | `retrieval_mode` | 3 | `hybrid` or `lexical_only` |
| `KnowledgeDigest` | `embedding_model` | 4 | `provider:model@dimensions`, empty when lexical only |

`excerpt` keeps its meaning: the summary registered with the document.

### Item 2 — `sreagent.knowledge.v1.KnowledgeService/PutSections`

A new published package, the write path of the knowledge store. A source sends a document's whole
current section set; sections it no longer sends are closed at that instant. Refusals:
`PermissionDenied` for a source or document namespace the caller's token does not own;
`InvalidArgument` for a section over 1,200 characters, an empty label, or more than 500 sections.
Reads never go through this service: the investigation engine's `knowledge_search` term is the only
way to retrieve.

### Item 3 — `pkg/knowledge.Store` and `pkg/knowledge.Embedder`

The two interfaces of `contracts/knowledge-store.md` §1–§2, in a public package so a program that
embeds the agent can supply its own. `pkg/knowledge/storetest.Run` is the conformance suite: a store
is conformant when it passes it unmodified. Two stores ship: an in-memory one (tests, the evaluation)
and the agent's own PostgreSQL (the default, feature 008 PR 2), which needs no extension and no
service beyond the database the agent already requires.

### Item 4 — the scorer `knowledge/hybrid@1`

1. Candidates come from the graph: every `KNOWLEDGE_DOC` with a `CONCERNS` edge valid at the valid
   instant and known at the observed instant, plus reviewed past investigations (007 R3). Scope is
   no longer a filter.
2. Every candidate carries one section synthesised from the text the graph already holds about it:
   `Summary` (its title and registered summary) for a document, `Verdict` for a past investigation.
3. The lexical ranking orders, by BM25 (k1 1.2, b 0.75), the union of the store's top 50 lexical
   sections among candidate documents known at the observed instant and the synthesised sections.
   The store selects; the worker orders, so two conformant stores give the same ranking.
4. The semantic ranking is the store's top 50 by cosine to the query's vector, when an embedder is
   configured.
5. A section scores Σ `1 / (60 + rank)` over the rankings it appears in (reciprocal rank fusion,
   k = 60). A document scores its best section's score, ×1.5 when it is in scope.
6. Ties are broken by document reference, then section label, then part. Every accumulation is
   rounded to six decimals.
7. An in-scope document no section of which matched is still returned, at score 0, after every
   matched document. An out-of-scope document is returned only when one of its sections matched.

A change to any constant is a new scorer version.

### Item 5 — the `embedding:` block of the model configuration

| key | required | meaning |
|---|---|---|
| `provider` | yes | `openai_compatible`; a Go program embedding the agent may register more |
| `base_url` | yes | the server's API root; `POST {base_url}/embeddings` |
| `model` | yes | the model name the server expects |
| `dimensions` | yes | vector length; a response of another length is refused |
| `api_key_env` | no | environment variable holding a bearer token; absent for local servers |
| `query_prefix`, `document_prefix` | no | strings prepended per purpose |

Optional. Absent, or its key variable unset, or the call failing: retrieval is lexical only and the
digest says `retrieval_mode: lexical_only`. The investigation never fails for it.

## Alternatives rejected

- **pgvector as the default store** (research R2): an extension many managed and self-hosted
  PostgreSQL installs lack. It remains a candidate store behind `pkg/knowledge.Store`.
- **A dedicated vector database as the default** (research R2): a new required service. Any of them
  can be plugged in through the interface.
- **A weighted sum of normalised lexical and semantic scores** (research R4): it needs a calibration
  per store and per model; reciprocal rank fusion needs none.
- **The store's own lexical score as the ranking** (Item 4.3): `ts_rank_cd` and an in-memory BM25
  order differently, so the evaluation would not predict production.
- **One SDK per embeddings provider** (research R5): one OpenAI-compatible wire format covers
  Mistral, OpenAI, Ollama, vLLM, Hugging Face TEI, LocalAI and llama.cpp's server.
- **An embedding model inside the agent binary** (research R5): a cgo dependency and a model
  download the operator did not choose.
- **Asking the store first and filtering by the graph afterwards** (research R3): a top 50 from the
  store could be entirely out of time and leave nothing.

## Consequences

- A postmortem is retrieved by a symptom written anywhere in it, not only in its first 600
  characters, and a postmortem about another service with the best-matching symptoms is returned,
  flagged `in_scope: false`, below an equally matching in-scope one.
- Every knowledge golden moves once, with the scorer version.
- Other knowledge sources (Google Drive, Confluence) call `PutSections` and reuse 007's ADR-0013
  items 2 and 3 for the graph side.
