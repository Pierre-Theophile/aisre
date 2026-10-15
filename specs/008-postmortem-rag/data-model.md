# Data Model: Postmortem Retrieval Over Full Content

The graph's `KNOWLEDGE_DOC` node and `CONCERNS` edge are unchanged (002, 007, ADR-0013). Everything
below lives in the knowledge store or on the wire.

## Section

The unit of retrieval: one labelled piece of a document's text.

| field | type | rule |
|---|---|---|
| `document_ref` | string | the node's ref, e.g. `notion.page=<id>`; namespace must belong to the writing source (R7) |
| `label` | string | the property name, or the heading text; `Body` for text before the first heading; ≤ 200 chars |
| `part` | int | 0 unless the section was split (FR-012) |
| `text` | string | sanitised plain text (FR-013); 1 to 1,200 characters; split on paragraph, then sentence boundaries |
| `digest` | string | SHA-256 of `label`, `part` and `text`; identity of a version |
| `origin` | enum | `property` \| `body` \| `summary` (synthesised from a registered summary) \| `verdict` (past investigation) |

**Validation**: empty text after sanitisation → not stored, reported `skipped: emptied by sanitisation`;
a label repeated in one document gets ` (2)`, ` (3)` appended in order.

## Section version

| field | type | rule |
|---|---|---|
| section fields | | as above |
| `known_from` | timestamp | instant the server accepted it |
| `known_to` | timestamp, open | set when a different digest arrives for the same `(document_ref, label, part)`, or when a put for the document omits that section |

**State**: open → closed. Never deleted, never edited.

A put is the whole current section set of one document: sections absent from the put are closed at
the put's instant, so a deleted heading stops matching from then on.

## Section embedding

| field | type | rule |
|---|---|---|
| `digest` | string | the section version's digest |
| `model` | ModelID | `provider/model@dimensions`, e.g. `openai_compatible:mistral-embed@1024` |
| `vector` | float32[dimensions] | L2-normalised at write |
| `computed_at` | timestamp | for the ingestion-lag figure only |

Keyed by `(digest, model)`: identical text in two documents is embedded once. Changing the configured
model starts a backfill; until it completes, searches run lexical only and say why (FR-028).

## Candidate (read from the graph, R3)

| field | source |
|---|---|
| `document_ref`, `kind`, `title`, `location`, `authored_at` | the `KNOWLEDGE_DOC` version at the pin |
| `linked_entity_ids`, `link_provenance` | `CONCERNS` edges valid at T and known at the pin |
| `in_scope` | any linked entity ∈ request `entity_ids` |

## Search

| field | rule |
|---|---|
| `query_text` | the request's `query_terms` joined by spaces; ≤ 1,000 chars |
| `entity_ids` | the scope, for the boost |
| `at` | `{valid, observed}` from the request (007 R2) |
| `limit` | ≤ `engine.MaxKnowledgeItems` documents |
| `embedding_model` | the configured ModelID, or none |

Result: documents ranked by `knowledge/hybrid@1`, each with its best two sections, its score, its
`in_scope` flag, and the digest's `retrieval_mode`.

## Retrieval fixture

```text
fixtures/retrieval/<id>/
├── manifest.yaml     # query text, entity scope, valid/observed instants, expected document, expected in_scope
├── documents.yaml    # documents, kinds, links (with validity), section versions (with known_from)
└── vectors.jsonl     # model id, then one line per section digest and one for the query
```
