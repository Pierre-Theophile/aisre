---

description: "Task list for feature 008 — postmortem retrieval over full content"
---

# Tasks: Postmortem Retrieval Over Full Content

**Input**: Design documents from `/specs/008-postmortem-rag/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[data-model.md](./data-model.md), [contracts/knowledge-store.md](./contracts/knowledge-store.md),
[quickstart.md](./quickstart.md)

**Tests**: **Required, not optional.** Constitution VIII (no fixtures, no merge), the zero-tolerance
criterion SC-003 and the retrieval gate of FR-042. Fixture tasks are interleaved with implementation.

**Organization**: grouped by the plan's three pull requests. Inside each, the stories it delivers in
dependency order. US2 and US3 (scope as boost, as-of sections) come first because they change the
worker's contract and are testable on the memory store alone; US1 needs Notion sections (PR 2) and
embeddings (PR 3) to be met in full.

**Status**: spec approved by the owner 2026-10-15; plan and tasks awaiting the owner's approval.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US5]`. Setup, Foundational and Polish tasks carry no story label.
- Every task names the fixture it validates against; `unit` means a Go test in the package named.

## Path conventions

Per [plan.md §Project Structure](./plan.md#project-structure): interfaces in `pkg/knowledge/`, the
Postgres store in `internal/store/postgres/knowledge_store.go`, embedders in `internal/embedding/`,
the worker in `internal/investigation/workers/knowledge/`, Notion sections in
`internal/feeders/notion/sections.go`, fixtures in `fixtures/retrieval/` and
`fixtures/notion-knowledge-twin-02/`.

---

# PR 1 — store, ranker, schema

## Phase 1: Setup

**Purpose**: publish the shared names before any code depends on them.

- [ ] T001 Write `docs/decisions/ADR-0014-shared-contract-additions-for-008.md`: the `KnowledgeItem` fields 10–12, `KnowledgeDigest` fields 3–4, `knowledge.v1.KnowledgeService/PutSections`, the `pkg/knowledge` `Store` and `Embedder` interfaces, the scorer `knowledge/hybrid@1`, the `embedding:` configuration block, with the alternatives rejected in research R2, R4 and R5 and a reference to constitution 1.2.0 (G3, research R9); fixture: unit (`scripts/check-specs.sh`)
- [ ] T002 Add the fields of contracts §4 to `api/sreagent/investigation/v1/investigation.proto`, create `api/sreagent/knowledge/v1/knowledge.proto` with `KnowledgeService`, and regenerate with `buf generate`; `buf lint` and `buf breaking` against `main` pass (FR-025, research R9); fixture: unit (CI schema job)

**Checkpoint**: ADR-0014 merged in this PR before any dependent code.

---

## Phase 2: Foundational

**Purpose**: the public interfaces, the memory store and the conformance suite. Blocks every story.

- [ ] T003 Write `pkg/knowledge/store.go` and `pkg/knowledge/embedder.go`: `Store`, `Embedder`, `Section`, `Hit`, `ModelID` (with `String()` as `provider:model@dimensions`), `Purpose`, `PendingSection`, `DigestVector`, the section digest function and the 1,200-character and 500-section limits, exactly as contracts §1–§2 and data-model §Section (FR-005, FR-012); fixture: unit
- [ ] T004 [P] Write `pkg/knowledge/storetest/storetest.go`: `Run(t, newStore)` covering every behaviour listed in contracts §1 "Conformance" (FR-007); fixture: unit
- [ ] T005 Implement `pkg/knowledge/memory/memory.go`: an in-memory `Store`, lexical by the existing BM25 (moved from `internal/investigation/workers/knowledge/bm25.go` to `pkg/knowledge/memory/bm25.go`), semantic by exact cosine; passes `storetest.Run` (FR-003, FR-007); fixture: unit
- [ ] T006 Add `KnowledgeDocuments(ctx, at AsOf)` to `internal/query/knowledge.go`: every `KNOWLEDGE_DOC` version known at the pin with at least one `CONCERNS` edge valid and known at the instants, with all linked entity ids and link provenance, plus the reviewed past investigations of 007 R3; keep `KnowledgeCandidates` as a thin filter over it for existing callers (G2, FR-002, research R3); fixture: unit against `internal/query/knowledge_test.go`'s graph

**Checkpoint**: a third-party store could be written and checked against `storetest` from here.

---

## Phase 3: User Story 2 — the same failure on another service is still recalled (Priority: P1)

**Goal**: scope ranks instead of gating.

**Independent Test**: two equally matching postmortems, one in scope and one out; both returned, the in-scope one first, the other flagged `in_scope: false` (spec US2).

- [ ] T007 [US2] Write `internal/investigation/workers/knowledge/hybrid.go`: `knowledge/hybrid@1` as research R4 (RRF k = 60 over up to two section rankings, best section per document with the best two carried, ×1.5 in scope, ties by document ref, label, part); `ScorerVersion = "knowledge/hybrid@1"` (FR-020–FR-022, FR-024); fixture: unit with hand-computed ranks
- [ ] T008 [US2] Rewrite `Worker.Call` in `internal/investigation/workers/knowledge/knowledge.go`: candidates from `KnowledgeDocuments` (T006), sections from the `Store` restricted to candidate refs, ranking by T007; replace `keepLinked` by the `in_scope` flag; fill `section_label`, `in_scope`, `matched_text`, `retrieval_mode = lexical_only` (FR-021, FR-025); the worker takes a `Store` and an optional `Embedder` in `New`; fixture: unit
- [ ] T009 [US2] Synthesise sections for documents with no stored sections: a registered summary becomes `Summary` (origin `summary`), a past investigation's verdict line becomes `Verdict` (origin `verdict`), read from the node version at the pin (FR-015, data-model §Section); fixture: unit with a `knowledge link` document and a reviewed investigation
- [ ] T010 [US2] Update the knowledge worker's goldens and `fixtures/incidents/knowledge-scope-01` for the new contract: the out-of-subgraph document is now returned with `in_scope: false` and ranked below the in-scope one, and SC-014's "never cited" assertion becomes "never ranked above an equally matching in-scope document"; fix the fixture's predicate path to `knowledge.items[*].document_id` (known defect, 007); fixture: knowledge-scope-01
- [ ] T011 [US2] Register the memory store for the evaluation harness in `internal/eval/harness.go`, loading sections from a fixture's `documents.yaml` when present (research R10); fixture: knowledge-notion-01

**Checkpoint**: US2 testable on the memory store; SC-004 asserted.

---

## Phase 4: User Story 3 — a replay never cites what the agent could not have known (Priority: P1)

**Goal**: as-of correctness for sections.

**Independent Test**: a section edited after the pin is matched in its earlier text; a page created after the pin is never returned (spec US3).

- [ ] T012 [US3] Pass the request's observed instant to `Store.Lexical` and `Store.Semantic` in `knowledge.go`, and assert in `internal/investigation/workers/knowledge/asof_test.go` the four acceptance scenarios of US3 (FR-023, SC-003); fixture: unit
- [ ] T013 [US3] Add `fixtures/incidents/knowledge-asof-01/`: a postmortem written after the incident, a section rewritten after the pin, a page trashed before the pin; verified by `aisre fixture verify` (replay from empty, doubled, shuffled), asserting 0 citations of later knowledge (SC-003); fixture: knowledge-asof-01

**Checkpoint**: SC-003 holds on the memory store.

---

## Phase 5: User Story 4 — retrieval quality is measured, lexical (Priority: P2)

**Goal**: the retrieval evaluation and its gate, lexical only for now.

**Independent Test**: dropping body sections from a fixture fails the gate (spec US4).

- [ ] T014 [P] [US4] Define the retrieval fixture format of data-model §Retrieval fixture in `internal/eval/retrieval/fixture.go` with a loader and validation; fixture: unit
- [ ] T015 [US4] Write `fixtures/retrieval/`: at least eight synthetic fixtures modelled on the shapes of real postmortems (symptom past 600 characters, reworded symptom, cross-service, as-of, runbook, two equally matching in-scope and out-of-scope) (FR-043); fixture: retrieval/*
- [ ] T016 [US4] Implement `aisre eval retrieval <dir>` in `internal/cli/eval_retrieval.go` and `internal/eval/retrieval/run.go`: recall@3 and MRR per fixture and in aggregate, per mode; exit 1 under the gate (FR-042); fixture: retrieval/*
- [ ] T017 [US4] Add the lexical gate to `.github/workflows/eval.yml` with the baseline measured on T015 written into `internal/eval/retrieval/thresholds.go`, and a planted regression test that drops body sections and expects failure (FR-042); fixture: retrieval/*

**Checkpoint**: PR 1 complete; merge when green.

---

# PR 2 — sections from Notion, stored in Postgres

## Phase 6: The default store and the ingest path (Foundational for PR 2)

- [ ] T018 Write `internal/store/postgres/migrations/0014_knowledge.sql`: schema `knowledge`, `section_versions` (document_ref, label, part, digest, origin, text, `tsvector` generated on `simple`, `known` tstzrange with a GiST index via `btree_gist`, a unique open version per `(document_ref, label, part)`), `section_embeddings` (digest, model, dimensions, `real[]`, computed_at, primary key `(digest, model)`) (data-model, research R2); fixture: unit (`pgtest`)
- [ ] T019 Implement `internal/store/postgres/knowledge_store.go`: the `Store` of contracts §1, lexical by `websearch_to_tsquery('simple', …)` and `ts_rank_cd`, semantic by exact cosine over an incrementally refreshed in-process cache of the configured model's vectors; passes `storetest.Run` (FR-005–FR-007, research R2); fixture: unit with `PG_DSN`
- [ ] T020 Implement `internal/server/knowledge.go`: the `PutSections` handler with the same token and source scoping as `IngestBatch`, namespace ownership per source kind, the limits of contracts §4, and registration in the server mux; `serve` builds the Postgres store by default and logs the store and retrieval mode (FR-005, FR-006, research R7); fixture: unit
- [ ] T021 [P] Add a `SectionsEmitter` to `pkg/feeder/emit` (Connect and memory implementations) that sends one document's sections after its graph events have been flushed (research R7); fixture: unit

---

## Phase 7: User Story 1 — lexical part (Priority: P1)

**Goal**: Notion pages become sections; an investigation finds a symptom past 600 characters by words.

**Independent Test**: a lexical-only investigation over twin 02 cites the postmortem whose symptom sits past character 600 (spec US1 scenario 1).

- [ ] T022 [US1] Add `GET /v1/blocks/{id}/children` to `notionx.Surface` in `internal/notionx/client.go` and to the operation table in `docs/connectors/notion.md` §2 together; `TestSurfaceMatchesThePublishedPage` passes (FR-010, G5, research R6); fixture: unit
- [ ] T023 [US1] Implement `internal/feeders/notion/sections.go`: named properties (the binding's `index_properties`, no count limit) to sections; body blocks to sections per research R6 (depth 3, 1,000-block cap, headings start sections, skipped types counted); split at 1,200 characters (FR-010–FR-012, FR-014); fixture: unit with recorded block payloads
- [ ] T024 [US1] Extend `internal/sanitise/policy_notion.go` to section text: user mentions dropped before text is built, email addresses and the secret shapes of `internal/sanitise` replaced by `[redacted]`, a section left empty skipped with its reason (FR-013); fixture: unit with canary tokens in property, heading, paragraph and toggle text
- [ ] T025 [US1] Wire sections into `internal/feeders/notion/feeder.go`: read bodies only for pages whose `last_edited_time` moved, send through T021 after the page's events, extend the binding configuration with `index_properties` (default: the 007 `summary_properties`) and `index_body: true` (FR-014); fixture: notion-knowledge-twin-02
- [ ] T026 [US1] Add `fixtures/notion-knowledge-twin-02/`: a postmortem database with long property sections and bodies with headings, toggles and an embed, generated by the real poller over a scripted twin as 007's twin 01 is; recording includes sanitised section text (FR-041); fixture: notion-knowledge-twin-02
- [ ] T027 [US1] Add `fixtures/incidents/knowledge-notion-02/`: an incident whose alert matches a twin-02 postmortem only in its "Signals / Symptoms" section past character 600; the investigation cites it with that `section_label` in the top three, lexical only (US1 scenario 1); fixture: knowledge-notion-02

**Checkpoint**: US1 lexical holds on twin 02.

---

## Phase 8: User Story 5 — the operator sees what was indexed (Priority: P3)

**Independent Test**: `--check` on twin 02 lists sections and no text (spec US5).

- [ ] T028 [US5] Extend `feed notion --check` in `internal/cli/feed_notion.go` with contracts §6: per page, sections with origin and size, skipped sections with reasons, never text (FR-040); fixture: notion-knowledge-twin-02 golden output

**Checkpoint**: PR 2 complete; merge when green.

---

# PR 3 — embeddings and the first wave

## Phase 9: User Story 1 — semantic part and first wave (Priority: P1)

**Goal**: reworded symptoms are found; the most similar postmortems are in context before the first hypothesis.

**Independent Test**: a reworded symptom retrieves its postmortem in the top three with recorded vectors; the first wave's knowledge question is in the evidence chain (spec US1 scenarios 2–4).

- [ ] T029 [US1] Add the optional `embedding:` block of contracts §3 to `internal/investigation/model/config.go` with its own validation (provider set `openai_compatible`, `dimensions > 0`, `base_url` parseable), recorded with the configuration (FR-026); fixture: unit
- [ ] T030 [P] [US1] Implement `internal/embedding/openaicompat.go`: `POST {base_url}/embeddings`, optional bearer from `api_key_env`, per-purpose prefixes, batches of 32, 10-second query timeout, retries on 429 and 5xx honouring `Retry-After`, dimension check, L2 normalisation (FR-029, research R5); fixture: unit against an `httptest` server shaped like the OpenAI response
- [ ] T031 [P] [US1] Implement `internal/embedding/recorded.go`: an `Embedder` that replays a fixture's `vectors.jsonl` and refuses unknown texts (research R10); fixture: unit
- [ ] T032 [US1] Implement `internal/embedding/indexer.go`: a loop in `serve` that drains `Store.Pending` for the configured model, embeds, `PutVectors`, and reports pending count and lag; a model change starts a backfill and searches stay lexical until it completes (FR-028, SC-007); fixture: unit with the memory store
- [ ] T033 [US1] Enable the semantic path in `knowledge.go`: embed the query with purpose `query`, pass `Store.Semantic`'s ranking into `knowledge/hybrid@1`, set `retrieval_mode` and `embedding_model`; any embedder error falls back to lexical for that search and is recorded (FR-020, FR-027, SC-006); fixture: unit with a failing embedder
- [ ] T034 [US1] Accept free-text `query_terms` up to 1,000 characters in `engine.Validate` for `knowledge_search` and record the query text in `ExecutedQuery` (FR-030, FR-032); fixture: unit
- [ ] T035 [US1] Add the knowledge question to `internal/investigation/engine/firstwave.go` per research R8: `knowledge_search` over the top candidates' entities with the alert's title and non-identifying label values, limit 5, entered as knowledge-derived evidence; add `"knowledge_search"` to `FirstWaveQueries` and update the fixture recorder's expectation (FR-031); fixture: knowledge-notion-02
- [ ] T036 [US1] Add a reworded-symptom case to `fixtures/incidents/knowledge-notion-02/` replayed through the `recorded` embedder: the postmortem is cited in the top three in hybrid mode and missed in lexical-only mode (US1 scenario 2); fixture: knowledge-notion-02

---

## Phase 10: User Story 4 — the hybrid gate (Priority: P2)

- [ ] T037 [US4] Record vectors for `fixtures/retrieval/*` from one named model with `aisre eval retrieval --record-vectors`, commit `vectors.jsonl`, and add the hybrid gate (recall@3 ≥ 0.90, hybrid ≥ lexical) to `internal/eval/retrieval/thresholds.go` and `.github/workflows/eval.yml` (FR-042, SC-001); fixture: retrieval/*
- [ ] T038 [US4] Run `storetest` and the retrieval evaluation for both shipped stores in CI, and assert a store swap needs only configuration (SC-008); fixture: retrieval/*

---

## Phase 11: Polish & cross-cutting

- [ ] T039 [P] Write `docs/knowledge/retrieval.md`: stores (default, how to plug another and run `storetest`), embedders (the `openai_compatible` table of servers with the ones verified marked, prefixes, dimensions), lexical-only behaviour, the evaluation; link it from `docs/connectors/notion.md`; fixture: unit (`scripts/check-specs.sh`)
- [ ] T040 [P] Update `README.md`'s feature table and `CLAUDE.md`-referenced docs for 008; run `./scripts/add-spdx-headers.sh`, `./scripts/check-no-secrets.sh`, `golangci-lint run` and `go test ./...` with `PG_DSN` set; fixture: CI
- [ ] T041 Walk [quickstart.md](./quickstart.md) §1–§5 on the twins with a self-hosted embeddings server and record the run in `quickstart.md`; fixture: notion-knowledge-twin-02

---

# Owner-gated

- [ ] T042 Record the owner's LDMA "SRE Agent Post Mortem" database with `aisre feed notion --record` (007 T030, now carrying sections), keep it private, and build the private retrieval set from its seven postmortems; needs the owner's go and the service-to-entity map; fixture: private LDMA recording
- [ ] T043 Measure SC-002 on the private set: every postmortem with a section past character 600 is retrieved in the top three from a symptom taken from that section, and report 007's retrieval on the same queries for comparison; re-run the investigation audit (007 T031); fixture: private LDMA recording

---

## Dependencies & execution order

- **PR 1**: T001 → T002; T003 → T004, T005; T006 independent of T003–T005; US2 (T007–T011) after T003–T006; US3 (T012–T013) after T008; US4 lexical (T014–T017) after T008 and T011.
- **PR 2**: T018 → T019 → T020; T021 independent; US1 lexical (T022–T027) after T020 and T021; US5 (T028) after T026.
- **PR 3**: T029 → T030, T031 → T032 → T033; T034 independent; T035 after T033 and T034; T036 after T033; T037–T038 after T033.
- **Polish** after PR 3's code; **owner-gated** after PR 3 merges.

```text
T001─►T002 ─────────────────────────────────────┐
T003─►T004,T005 ─┐                              ├─► US2(T007–T011) ─► US3(T012–T013) ─► US4(T014–T017)   [PR 1]
T006 ────────────┘                              │
T018─►T019─►T020 ─┬─► T022–T027 (US1 lexical) ─► T028 (US5)                                              [PR 2]
T021 ─────────────┘
T029─►T030,T031─►T032─►T033─►T035,T036,T037,T038 ; T034─►T035                                  [PR 3]
Polish T039–T041 ─► owner-gated T042─►T043
```

## Parallel opportunities

- T004 and T005 once T003 lands; T006 alongside both.
- T014 alongside T012–T013.
- T021 alongside T018–T020; T022 and T024 alongside T023.
- T030 and T031; T039 and T040.

## Implementation strategy

1. **PR 1 — store, ranker, schema** (T001–T017): scope ranks instead of gating, as-of sections, the evaluation; useful alone because hand-registered documents and past investigations are ranked by the new scorer.
2. **PR 2 — Notion sections in Postgres** (T018–T028): the real postmortems become searchable by their full text.
3. **PR 3 — embeddings and the first wave** (T029–T041): reworded symptoms, the knowledge question before the first hypothesis, the hybrid gate.
4. **Owner-gated** (T042–T043): the LDMA recording and SC-002.
