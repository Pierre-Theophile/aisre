<!-- SPDX-License-Identifier: Apache-2.0 -->

# Knowledge retrieval: stores, embedders and the evaluation

Written for the **operator** who decides where postmortem text is kept and which embedding model,
if any, ranks it; and for anyone **plugging in** another store or embedding provider.

An investigation's `knowledge_search` cites the postmortems, runbooks and reviewed investigations
that read like the incident. The graph decides which documents are candidates and as of when (they
are linked to an entity by a `CONCERNS` edge valid at the investigation's instants); the knowledge
store holds what those documents say, cut into sections; the scorer `knowledge/hybrid@1` ranks the
sections. Every dependency here is optional and replaceable: the agent runs with no embedding
provider at all, and the default store is the agent's own PostgreSQL.

Specification: [`specs/008-postmortem-rag/`](../../specs/008-postmortem-rag/spec.md). Interfaces and
wire: [`contracts/knowledge-store.md`](../../specs/008-postmortem-rag/contracts/knowledge-store.md).
Where sections come from: [the Notion connector, §4a](../connectors/notion.md#4a-what-is-indexed-feature-008).

## 1. How a search is ranked

1. **Candidates** come from the graph: every document linked to anything, and every reviewed past
   investigation, valid at the investigation's valid instant and known at its observed instant. The
   store can neither resurrect a trashed page nor invent an unlinked one.
2. **Lexical**: the store returns its 50 best sections of those candidates for the query's words,
   known at the observed instant; the worker orders them, with each candidate's title and registered
   summary, by BM25.
3. **Semantic**, when an embedding model is configured and its index is built: the query is embedded
   and the store returns the 50 sections closest to it by cosine.
4. The two rankings are fused by reciprocal rank (k = 60); a document scores its best section and
   carries its best two; a document linked to an entity inside the investigation's scope scores
   ×1.5. A document linked elsewhere stays eligible and is cited `in_scope: false`.

Every knowledge digest records `retrieval_mode` (`hybrid` or `lexical_only`), the embedding model
(`provider:model@dimensions`), and the query text. When the semantic half was configured but not
used, the executed query says why: the index is still being built, or the embedding call failed.

The investigator asks `knowledge_search` itself, with words or free text up to 1,000 characters, and
the deterministic first wave asks it once before the first model turn, with the alert's own words,
scoped to the subject and the entities the top candidate changes landed on. Either way, a document
is a reason to look and never a finding: a hypothesis resting on one alone stays unconfirmed.

## 2. Stores

| store | flag | durable | what it is for |
|---|---|---|---|
| PostgreSQL (default) | `--knowledge-store postgres` | yes | production: the agent's own database, migration 0014; no extension, cosine computed in process |
| memory | `--knowledge-store memory` | no | trials, evaluation, tests; the reference implementation |

Swapping one for the other is the flag and nothing else (SC-008). CI runs the retrieval evaluation
over both and requires identical results (`TestTheCorpusScoresTheSameOnEveryShippedStore`).

**Another store** (pgvector, OpenSearch, Qdrant, a file) is a Go package implementing
[`pkg/knowledge.Store`](../../pkg/knowledge/store.go): `Put`, `Lexical`, `Semantic`, `Pending`,
`PutVectors`. It is conformant when it passes the published suite unmodified:

```go
func TestMyStore(t *testing.T) {
    storetest.Run(t, func(t *testing.T) knowledge.Store { return mystore.New(...) })
}
```

The suite checks put/replace/close semantics, reads as of instants before, between and after
versions, the documents filter, phrase matches first, cosine order, dimension refusals, idempotence,
deterministic ties and concurrent puts. A store only selects; the worker orders, so two conformant
stores give the same ranking.

## 3. Embedders

The embedding model is the optional `embedding:` block of the model configuration
(`config/model.yaml`), recorded with every investigation like the rest of the file:

```yaml
embedding:
  provider: openai_compatible        # the one published adapter
  base_url: https://api.mistral.ai/v1
  model: mistral-embed
  dimensions: 1024                   # a response of another length is refused
  api_key_env: MISTRAL_API_KEY       # optional: a local server needs none
  query_prefix: ""                   # optional, for models trained with prefixes
  document_prefix: ""
```

`openai_compatible` speaks `POST {base_url}/embeddings` with `{"model", "input": [...]}`, in batches of
32, with a 10-second limit on a query, retries on 429 and 5xx honouring `Retry-After`, and L2
normalisation of every vector. One wire format reaches the hosted APIs and the self-hosted servers:

| server | `base_url` | example model, dimensions | prefixes | verified |
|---|---|---|---|---|
| Mistral | `https://api.mistral.ai/v1` | `mistral-embed`, 1024 | none | **yes**: the recorded vectors of `fixtures/retrieval` and `knowledge-notion-02` |
| llama.cpp server (`llama-cpp-python[server]` or `llama-server --embedding`) | `http://localhost:8000/v1` | `nomic-embed-text-v1.5` GGUF, 768 | `search_query: `, `search_document: ` | **yes**: quickstart §4, 2026-10-15 |
| OpenAI | `https://api.openai.com/v1` | `text-embedding-3-small`, 1536 | none | not yet |
| Ollama | `http://localhost:11434/v1` | `nomic-embed-text`, 768 | `search_query: `, `search_document: ` | not yet |
| vLLM | `http://localhost:8000/v1` | any embedding model it serves | per model | not yet |
| Hugging Face TEI | `http://localhost:8080/v1` | `BAAI/bge-small-en-v1.5`, 384 | none | not yet |
| LocalAI | `http://localhost:8080/v1` | per model | per model | not yet |

"Not yet" means the server documents the endpoint and nobody has run this adapter against it; a row
moves to "yes" when someone has. Another provider with another wire format is a Go package
implementing [`pkg/knowledge.Embedder`](../../pkg/knowledge/embedder.go) (`Model`, `Embed`).

### What happens without one, or when it fails

- **No block, or its `api_key_env` unset**: `serve` logs `knowledge: retrieval is lexical only` with the
  reason, and every digest says `retrieval_mode: lexical_only`. Nothing else changes.
- **The server is down or slow** at search time: that search runs lexical only, its executed query says
  `semantic ranking not used: embedding the query failed: …`, and the investigation carries on.
- **A new model** (or the first start): `serve` runs an indexer that embeds every current section and
  stores the vectors under the model's id. Until that backfill has completed once, searches run lexical
  only and say the index is still being built; a search never ranks by a half-built index and never
  mixes two models. The indexer then embeds new sections every 30 seconds, and logs a failed round with
  the pending count and lag.

What leaves the agent is section text, already sanitised (no person's name or e-mail, no secret
shape), and the query: the alert's words, or what the investigator typed. Point `base_url` at a
self-hosted server if neither may leave your network.

## 4. The evaluation

```sh
bin/aisre eval retrieval fixtures/retrieval/
```

Each fixture under `fixtures/retrieval/` is one query, a handful of documents and the document it must
find ([README](../../fixtures/retrieval/README.md)). The evaluation runs each in three modes —
`summary_only` (007's retrieval), `lexical_only` and `hybrid` — and prints recall@3 and MRR per fixture
and in aggregate. The hybrid mode replays each fixture's `vectors.jsonl`, recorded from one named model,
through the recorded embedder: CI needs no provider and no key, and a ranking is the same on every
machine. The gate (`internal/eval/retrieval/thresholds.go`) fails when a document a fixture says must
never be cited is cited, lexical falls below its measured floors, hybrid recall@3 falls below 0.90
(SC-001), hybrid does worse than lexical, or a fixture has no recorded vectors.

Re-record the vectors after changing a fixture, or to measure another model:

```sh
MISTRAL_API_KEY=… bin/aisre eval retrieval --record-vectors fixtures/retrieval/
```

`--record-vectors` uses the `embedding:` block of `--model-config` (default `config/model.yaml`). A
recording holds the model's id and, per text, the SHA-256 of the text and its vector — never the text.
To compare a self-hosted model, point `--model-config` at a copy whose block names it, record into a
scratch copy of the corpus, and run the evaluation there.
