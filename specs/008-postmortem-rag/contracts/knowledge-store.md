# Contract: the knowledge store, the embedder and their wire

Published by ADR-0014. Everything here is what a third party implements or calls; anything not here
is internal and may change.

## 1. `pkg/knowledge.Store`

```go
type Store interface {
    // Put replaces the current section set of one document, at instant at. Sections absent from
    // sections are closed at at; a section whose digest equals its open version is a no-op.
    Put(ctx context.Context, documentRef string, sections []Section, at time.Time) (PutResult, error)

    // Lexical returns up to k sections of the given documents, known at observed, ranked by the
    // store's lexical relevance to query. Rank order is deterministic for a given store state.
    Lexical(ctx context.Context, query string, documents []string, observed time.Time, k int) ([]Hit, error)

    // Semantic returns up to k sections of the given documents, known at observed, that have a
    // vector for model, ranked by cosine similarity to vector.
    Semantic(ctx context.Context, vector []float32, model ModelID, documents []string, observed time.Time, k int) ([]Hit, error)

    // Pending lists section digests known now that have no vector for model, oldest first.
    Pending(ctx context.Context, model ModelID, limit int) ([]PendingSection, error)

    // PutVectors stores vectors for model; idempotent by (digest, model).
    PutVectors(ctx context.Context, model ModelID, vectors []DigestVector) error
}
```

`Hit` carries `DocumentRef`, `Label`, `Part`, `Text`, `Digest`, `Rank` (1-based). `ModelID` is
`{Provider, Model string; Dimensions int}`, rendered `provider:model@dimensions`.

**Conformance** (`pkg/knowledge/storetest.Run(t, newStore)`): put/replace/close semantics; as-of reads
at instants before, between and after versions; documents filter; lexical finds an exact phrase
section first; semantic orders by cosine on fixed vectors; dimension mismatch refused; idempotent puts
and vector writes; deterministic ties; concurrent puts to different documents. Both shipped stores run
it in CI; a third-party store is conformant when it passes it unmodified.

## 2. `pkg/knowledge.Embedder`

```go
type Embedder interface {
    Model() ModelID
    // Embed returns one L2-normalised vector per text. purpose is "document" or "query", for
    // providers that embed the two differently; others ignore it.
    Embed(ctx context.Context, texts []string, purpose Purpose) ([][]float32, error)
}
```

Shipped: `openai_compatible` (production), `recorded` (replays a fixture's `vectors.jsonl`; tests and
the evaluation only).

## 3. Configuration — the `embedding:` block of the model configuration

| key | required | meaning |
|---|---|---|
| `provider` | yes | `openai_compatible` (the published set; a Go program embedding the agent may register more) |
| `base_url` | yes | the server's API root; `POST {base_url}/embeddings` |
| `model` | yes | the model name the server expects |
| `dimensions` | yes | vector length; a response of another length is refused |
| `api_key_env` | no | environment variable holding a bearer token; absent for unauthenticated local servers |
| `query_prefix`, `document_prefix` | no | strings prepended per purpose, for models that expect them (e.g. `query: `) |

The block is optional. Absent, or its key variable unset: retrieval is lexical only and every digest
says `retrieval_mode: lexical_only` (FR-027). The block is part of the versioned file and is recorded
with every investigation (FR-026).

## 4. Wire

### `sreagent.knowledge.v1.KnowledgeService/PutSections`

```proto
message PutSectionsRequest {
  string source_id = 1;            // e.g. "notion:LDMA"; must match the token's source
  string document_ref = 2;         // e.g. "notion.page=<id>"; namespace owned by the source
  repeated Section sections = 3;   // the document's whole current set
}
message Section { string label = 1; uint32 part = 2; string text = 3; string origin = 4; }
message PutSectionsResponse { uint32 opened = 1; uint32 closed = 2; uint32 unchanged = 3; }
```

Refusals: `PermissionDenied` for a source or namespace the token does not own; `InvalidArgument` for a
section over 1,200 characters, an empty label, or more than 500 sections.

### Additive fields (investigation.v1)

| message | field | number | meaning |
|---|---|---|---|
| `KnowledgeItem` | `section_label` | 10 | label of the best-matching section |
| `KnowledgeItem` | `in_scope` | 11 | a linked entity is inside the request's scope |
| `KnowledgeItem` | `matched_text` | 12 | that section's sanitised text, ≤ 1,200 chars |
| `KnowledgeDigest` | `retrieval_mode` | 3 | `hybrid` or `lexical_only` |
| `KnowledgeDigest` | `embedding_model` | 4 | the ModelID string, empty when lexical only |

`excerpt` keeps its meaning (the registered summary). `scorer_version` reads `knowledge/hybrid@1`.

## 5. Scorer `knowledge/hybrid@1`

As research R4: RRF with k = 60 over lexical and semantic section ranks (top 50 each), best section per
document, ×1.5 in scope, ties by document ref, label, part. A change to any constant is a new version.

As built (ADR-0014 item 4): every candidate also carries one section synthesised from what the graph
holds (`Summary`: title and registered summary; `Verdict` for a past investigation). The lexical
ranking orders the union of the store's top 50 lexical sections and the synthesised ones by BM25
(k1 1.2, b 0.75): the store selects, the worker orders. An in-scope document nothing in which matched
is returned at score 0 after the matched ones; an out-of-scope one is returned only when it matched.

## 6. `feed notion --check` additions

Per registered page: one line per section, `label (origin, N chars)`, and each skipped section with its
reason: `emptied by sanitisation`, `over 1,000 blocks; truncated`, `unsupported block <type>` (counted
per type, not listed). Never section text.
