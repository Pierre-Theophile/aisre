# Retrieval fixtures

The corpus of `aisre eval retrieval` (feature 008, US4, FR-042, FR-043). Each directory is one
query and the handful of documents it is asked over; the evaluation runs it through the knowledge
worker in every retrieval mode and reports recall@3 and MRR. CI gates on the floors in
`internal/eval/retrieval/thresholds.go`.

These are not graph fixtures, so `fixture verify fixtures/*/` does not run them: a retrieval
fixture has no `manifest.yaml`, and its documents are an in-memory corpus and store rather than
events replayed into PostgreSQL. That is what makes the evaluation cheap enough for every pull
request.

## Layout

```text
<id>/
├── retrieval.yaml   # the query (terms, entity scope, valid and observed instants) and the expectation
└── documents.yaml   # the documents: kind, title, registered summary, links with validity, section puts
```

`vectors.jsonl` (recorded embeddings for the hybrid mode) arrives with the semantic path.

`retrieval.yaml`:

| key | meaning |
|---|---|
| `id` | the directory name |
| `shape` | what the fixture exercises, from the list below |
| `query.terms` | the alert's words, as `knowledge_search.query_terms` |
| `query.entity_ids` | the investigation's scope |
| `query.valid_at`, `query.observed_at` | the pinned instants |
| `expect.document` | the document the query must find |
| `expect.in_scope` | the `in_scope` flag it must be cited with |
| `expect.never` | documents that must not be cited at all (written or linked after the pin) |

`documents.yaml` holds, per document, what the graph would know (`ref`, `kind`, `title`, `summary`,
`location`, `authored_at`, and `links`, each valid from `valid_from` to an optional `valid_to` and
known from `known_from`, which defaults to `valid_from`) and what a source would send to the
knowledge store (`puts`, each the whole section set at an instant, exactly as
`KnowledgeService/PutSections` takes it).

## Grading

A fixture is a hit at rank r when the expected document is the r-th cited, with the expected
`in_scope` flag, on a matched section. An in-scope document cited with no matching section is a
listing, not a retrieval, and does not count. recall@3 is the share of fixtures hit at rank ≤ 3;
MRR is the mean of 1/r, 0 for a miss.

The modes:

- `summary_only`: no store; every document is matched on its title and registered summary, which is
  what 007 retrieved over. The baseline.
- `lexical_only`: the store's sections, ranked lexically by `knowledge/hybrid@1`.

## Shapes

| shape | fixtures | what it proves |
|---|---|---|
| `symptom-past-600` | past-600-01, past-600-02 | a symptom written only past the summary is found; French scores like English |
| `reworded-symptom` | reworded-01 | the alert and the postmortem share no word: lexical misses, semantic is for this |
| `cross-service` | cross-service-01 | the same failure on another service is returned, `in_scope: false` |
| `as-of` | asof-01 | a section rewritten after the pin is matched in its earlier text; later documents never |
| `runbook` | runbook-01 | a runbook is retrieved by its procedure |
| `scope-tie` | scope-tie-01 | of two equally matching documents, the in-scope one ranks first |
| `long-postmortem` | long-postmortem-01 | a long document is one result, not one per section |
| `past-investigation` | verdict-01 | a reviewed investigation is matched on its verdict |

Every document here is synthetic. Real postmortems seed a private set that is never committed.
