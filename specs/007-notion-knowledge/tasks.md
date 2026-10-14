---

description: "Task list for feature 007 — the Notion knowledge connector"
---

# Tasks: Notion Knowledge Connector

**Input**: Design documents from `/specs/007-notion-knowledge/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[data-model.md](./data-model.md), [contracts/notion-connector.md](./contracts/notion-connector.md),
[quickstart.md](./quickstart.md)

**Tests**: **Required, not optional.** Constitution VIII (no fixtures, no merge) and the
zero-tolerance criteria SC-002, SC-004 and SC-005. Fixture tasks are interleaved with implementation.

**Organization**: one setup phase for the decision record and the operation list, one foundational
phase for the client, the sanitiser and the corpus interface, then the stories in dependency order.
US2 comes before US3 and US1 although all three are P1: it is useful alone (hand-registered
documents and reviewed investigations start being cited) and US1 cannot be met without it.

**Status**: scope, the five defaults, the plan and these tasks approved by the owner on 2026-10-14
(all three PRs); in implementation.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US5]`. Setup, Foundational and Polish tasks carry no story label.
- Every task names the fixture it validates against; `unit` means a Go test in the package named.

## Path conventions

Per [plan.md §Project Structure](./plan.md#project-structure): corpus in
`internal/store/postgres/knowledge.go`, client in `internal/notionx/`, feeder in
`internal/feeders/notion/`, CLI in `internal/cli/feed_notion.go`, fixtures in `fixtures/notion-*/`
and `fixtures/incidents/knowledge-notion-01/`.

---

## Phase 1: Setup

**Purpose**: the decision record and the operation list, so nothing can issue an unpublished call.

- [X] T001 [P] Write `docs/decisions/ADR-0013-shared-contract-additions-for-007.md`: source kind `notion`, source id `notion:<workspace label>`, ref namespace `notion.page`, rule `notion/notion-db@1`, the `configured_by` prop, the `Corpus.Scoped` instants (G2, G4), with the alternatives rejected in research R1, R3 and R4; fixture: unit Note: `docs/decisions/ADR-0013-shared-contract-additions-for-007.md`, items 1–4.
- [X] T002 [P] Write `docs/connectors/notion.md`: integration setup with Read content only, sharing the databases, the configuration of contracts §4, the operation table of contracts §2, marked **not yet enforced** until T009; fixture: unit Note: marked enforced in the same PR, since T009 landed with it.
- [X] T003 Write `internal/notionx/operations.go`: `Surface` as `feeder.MustReadOnlySurface` of the five operations of contracts §2, the data-source query declared as a named query with its justification, and a test that parses the table in `docs/connectors/notion.md` and compares it with `Surface` in both directions (FR-001, research R4); fixture: unit Note: `Surface` lives in `internal/notionx/client.go` beside `Do`; `TestSurfaceMatchesThePublishedPage` compares it with the page.

**Checkpoint**: ADR-0013 exists before any shared name is used in code.

---

## Phase 2: Foundational

**Purpose**: the corpus interface change, the client and the sanitiser. Blocks every story.

- [X] T004 Change `knowledge.Corpus.Scoped` to `Scoped(ctx, entityIDs []string, at Instants)` with `Instants{Valid, Observed time.Time}` taken in `Worker.Call` from the request's `valid_at` and `observed_at` (unset observed = valid), and update the test doubles in `internal/investigation/workers/knowledge/knowledge_test.go` so a document whose link starts after `Valid` is not returned (G2, FR-006, research R2); fixture: unit Note: `knowledge.Instants`; the in-memory double uses `AuthoredAt` as the link's start, and `TestTheCorpusIsAskedAtTheRequestsInstants` pins the pass-through.
- [X] T005 [P] Write `internal/sanitise/policy_notion.go`: drop `created_by`, `last_edited_by`, people properties, unnamed properties, `icon`, `cover`, `public_url`; rich text to plain text with user mentions removed; HMAC page, database, data-source and workspace ids in recordings (FR-023, FR-050, research R8); fixture: unit with canary tokens in every dropped field Note: the poller reduces each page before it becomes a payload, and the client never decodes a person, an icon, a cover or a public URL, so the table covers the reduced payload; the pre-pass drops the page URL. New pseudonym kinds `page`, `database`, `data_source`.
- [X] T006 Implement `internal/notionx/client.go`: the five operations only, `Notion-Version: 2025-09-03`, cursor pagination (`start_cursor`, `has_more`), the integration secret held in a `feeder` read credential that never prints; fixture: unit against a recorded double Note: `notionx.Secret` renders as redacted; every call goes through a `feeder.Issuer`.
- [X] T007 [P] Implement `internal/notionx/budget.go`: a static allowance of 3 requests per second through `feeder.QuotaBudget` reported as `static_fallback`, `Retry-After` on 429 closes the budget until the instant named, a poll cut short returns a typed `PartialPollError` (FR-044); fixture: unit Note: a call over the allowance waits for the next second; a Retry-After beyond 30 s returns `PartialPollError`.
- [X] T008 Implement recording of sanitised exchanges through `deployrecord.NewTee`, as `internal/cli/feed_slack.go` does, with a `PreparePayload` in `internal/feeders/notion/record.go` that applies T005 before anything is written (FR-050); fixture: unit Note: `notion.PreparePayload`, and `Config.ForRecording` for the shadow; `TestARecordingCarriesNoIdentifierInTheClear`.
- [X] T009 Report credential evidence `operator_asserted` at start and refuse to start (exit 3, `ExitAuth`) unless the configuration carries `acknowledge_read_only: [notion.capabilities]`; check `GET /v1/users/me`'s workspace name against the configured label; mark `docs/connectors/notion.md` enforced (FR-003, G5, US5 scenario 2); fixture: unit Note: `notionx.CheckReadOnly` refuses without the acknowledgement before any call; `feed notion` checks `users/me` against the label (exit 3).

**Checkpoint**: a read-only, rate-limited, recordable Notion client exists, the worker asks the corpus with instants, and no story has started.

---

## Phase 3: User Story 2 — documents and past investigations are retrieved live (Priority: P1) 🎯 MVP

**Goal**: a running deployment answers `knowledge_search` from the graph.

**Independent Test**: `serve --enable-investigation` over a database where `knowledge link` registered a runbook; an investigation on the linked entity cites it. `knowledge-scope-01` passes against the Postgres corpus.

- [X] T010 [US2] Implement `internal/store/postgres/knowledge.go`: `KnowledgeCorpus.Scoped` as the one query of research R1 — `CONCERNS` edges into the requested entities valid at `Valid` and observed at `Observed`, joined to the `KNOWLEDGE_DOC` version at the same instants, grouped per document with every linked entity — reading `kind`, `summary`, `location`, `authored_at`, `link_provenance` and computing `AgeDays` at `Valid` (FR-006); fixture: unit against `pgtest` Note: done as `query.Engine.KnowledgeCandidates` in `internal/query/knowledge.go` rather than in `internal/store/postgres`, because the query engine already owns merge redirects, reference resolution and the observed-instant predicate; `knowledge.GraphCorpus` adapts it. Candidates are restricted to source entities of type `knowledge_doc`, since a human fact also writes `CONCERNS` from an investigation.
- [X] T011 [US2] Add past investigations to the same query: an `INVESTIGATION` node whose `INVESTIGATED` edge to a requested entity started before `Valid` and whose version known at `Observed` carries `sre.investigation.was_this_right`; kind `investigation`, title the verdict line, location the investigation id, provenance `investigated by <requester>; reviewed by <labelled_by>` (FR-008, G3, research R3); fixture: unit against `pgtest`, an unreviewed run excluded Note: in the same query; `TestReviewedInvestigationsAreKnowledge` over `human-fact-reopen-01` plus a label event.
- [X] T012 [US2] Run the knowledge worker's existing goldens against `KnowledgeCorpus` as well as the in-memory corpus, so the two cannot drift, in `internal/store/postgres/knowledge_test.go`; fixture: `knowledge-scope-01` Note: `internal/investigation/workers/knowledge/graph_test.go` and `internal/query/knowledge_test.go`, both over `knowledge-scope-01`.
- [X] T013 [US2] Register the knowledge worker over `KnowledgeCorpus` in `internal/investigation/runner/runner.go` beside the graph worker when the caller has not registered one, with `runner.Config.Knowledge` to supply or disable it (FR-005, G1); fixture: unit Note: `runner.Config.NoKnowledgeWorker`; `TestTheKnowledgeWorkerIsRegisteredByDefault`.
- [X] T014 [US2] Add `--no-knowledge-worker` to `internal/cli/serve.go` and pass the corpus through `runner.Config`; without the flag the worker is registered (FR-005, US2 scenarios 1–2, contracts §7); fixture: unit
- [X] T015 [US2] Register the knowledge worker in `internal/eval/harness.go` `NewEngine` over the fixture's replayed graph, and re-run the 002 evaluation corpus to show outcomes unchanged where no document is linked (SC-006, 002 SC-014); fixture: `knowledge-scope-01`, the 002 corpus Note: `EngineWiring.Knowledge`, set by the harness when it replays the fixture itself and by `fixture record-world`. The tool set offered to the model was already static, so recorded trajectories and the 002 evaluation tests are unchanged.
- [X] T016 [US2] Replay determinism: the same investigation replayed twice retrieves the same candidate set, and a document registered after the pin is not a candidate (FR-007, US1 scenario 3); fixture: unit in `internal/eval` Note: covered at the corpus by `TestKnowledgeIsReadAsOfBothInstants` (the observed bound is what makes a replay retrieve the same set).

**Checkpoint**: knowledge is cited live; US2 is shippable on its own.

---

## Phase 4: User Story 3 — Notion pages become and stay knowledge (Priority: P1)

**Goal**: configured databases feed `KNOWLEDGE_DOC` nodes and `CONCERNS` edges, kept current.

**Independent Test**: `fixture verify fixtures/notion-knowledge-twin-01` from empty, doubled and shuffled.

- [X] T017 [P] [US3] Implement `internal/feeders/notion/binding.go`: parse and validate the configuration of contracts §4 (kinds closed, ≤ 3 summary properties, entity refs parse, unique databases), resolve each database to its data sources and property names to ids at start, error naming database and property when one is missing (FR-010, FR-030, research R5); fixture: unit Note: `internal/feeders/notion/config.go`. Property names are resolved from the pages a query returns rather than at start: the data-source schema read is not among the five published operations.
- [X] T018 [P] [US3] Implement `internal/feeders/notion/page.go`: page → `knowledge.LinkRequest` with ref `notion.page=<id>`, title, summary from the named properties joined with ` — ` and capped at 600 characters on a word boundary with `summary_truncated`, authored instant from the named date or `created_time`, location the page URL, entities through the map; unmapped values returned for the report; a page with no mapped value yields none (FR-020–FR-023, FR-031, FR-032); fixture: unit Note: `page.go` holds the payload and the summary rule; the mapping to events is in `feeder.go` through `LinkRequest.Props`.
- [X] T019 [US3] Extend `knowledge.LinkRequest.Events` with `configured_by`, `summary_truncated` and per-edge `rule`, `database`, `property`, `value` props, and content-digest event ids (data-model §Event ids), keeping `knowledge link`'s output unchanged when they are empty (FR-033, FR-034, FR-041); fixture: unit, `knowledge link` goldens unchanged Note: `LinkRequest` gains `ConfiguredBy`, `SummaryTruncated`, `Provenance` and `Props`; edge props are added by the connector. The value prop is `property_value`, because the graph refuses a prop named `value`.
- [X] T020 [US3] Implement truncated-value completion through `GET /v1/pages/{id}/properties/{property_id}` for a named summary or entity property at 25 references, stopping at the summary cap (research R7); fixture: unit against the recorded double Note: `Client.PropertyItems`, called by the poller for a named property at the limit.
- [X] T021 [US3] Implement `internal/feeders/notion/feeder.go` incremental poll: per data source, `last_edited_time` on or after the watermark minus 2 minutes, ascending; emit node and edge upserts at the page's `last_edited_time`; edges for values no longer present retracted at that instant; advance the watermark in the checkpoint (FR-040–FR-042, research R6); fixture: unit Note: the poller keeps a state file; each payload carries the refs a page had, so the feeder keeps no memory and a replay is the live run.
- [X] T022 [US3] Implement reconciliation in the same file: list every page; a registered page no longer returned is read once by id — trashed, archived or moved out ends its edges at its `last_edited_time`, a 404 at its last-seen instant, any other failure is a coverage gap; a database unreadable is a coverage gap and retracts nothing; a restored page opens a new interval (FR-043, research R6); fixture: unit Note: in the poller (`absent`); an incomplete read retracts nothing.
- [X] T023 [US3] Resolve a page bound by two databases to the first binding's kind and report the conflict (Edge Cases); fixture: unit Note: a page belongs to the binding whose data source is its parent, else the first to list it in the cycle.
- [X] T024 [US3] Author `fixtures/notion-knowledge-twin-01/`: a synthetic postmortem database and runbook database with a linked page, an edit, a value added and removed, a trash and restore, a page in two databases, an unmapped value, a truncated property, a rate-limited partial poll and an injection attempt in a summary (FR-052); `fixture record` and `verify` pass from empty, doubled and shuffled; fixture: notion-knowledge-twin-01 Note: generated by the real poller over a scripted twin (`TestGenerateNotionFixture`); reordering window 0, since reads are emitted in order.
- [X] T025 [US3] Implement `internal/cli/feed_notion.go`: `aisre feed notion --config [--once] [--db] [--record]` running T021 on the interval and T022 at start and every `reconcile_every`, appending through the existing event path with source id `notion:<label>`; fixture: unit in `internal/cli/feed_notion_test.go` Note: `--server`/`--token` replace the contract's draft `--db`; `--check` is PR 3 (T029).

**Checkpoint**: Notion pages are in the graph and follow their edits.

---

## Phase 5: User Story 1 — an investigation cites the last similar postmortem (Priority: P1)

**Goal**: the outcome the owner asked for, end to end.

**Independent Test**: `fixtures/incidents/knowledge-notion-01` — linked postmortem cited; out-of-subgraph and later postmortems never cited.

- [ ] T026 [US1] Author `fixtures/incidents/knowledge-notion-01/`: an incident on `checkout` whose graph holds three Notion postmortems from the T024 twin — one linked to `checkout` and older than the pin, one linked to a service outside the subgraph that is the better lexical match, one authored after the pin — plus one injection attempt in a summary (US1 scenarios 1–3, 5); fixture: knowledge-notion-01
- [ ] T027 [US1] Evaluation test in `internal/eval/`: the linked postmortem is cited with identifier, linked entity, provenance `notion/notion-db@1 configured by …`, age and location; the other two are cited in 0 % of runs; scope, budgets and verdict are unchanged by the injection (SC-001 shape, SC-002, FR-017 of 002); fixture: knowledge-notion-01
- [ ] T028 [US1] Assert that a hypothesis resting only on the cited postmortem is reported knowledge-derived and never reaches `supported` without a source-of-truth worker (US1 scenario 4, 002 FR-051); fixture: knowledge-notion-01

**Checkpoint**: the MVP the owner asked for is demonstrable on synthetic data.

---

## Phase 6: User Story 4 — recorded, sanitised and replayable (Priority: P2)

**Goal**: the feeder tested against how the organisation actually fills in its databases.

**Independent Test**: `fixture campaign scan` and `verify` pass over the private recording.

- [ ] T029 [P] [US4] Add Notion user-id and email shapes to the independent scan in `internal/campaign` and plant canary tokens in the twin's dropped properties (SC-005); fixture: notion-knowledge-twin-01
- [ ] T030 [US4] Record the organisation's postmortem and runbook databases into the private corpus with `feed notion --record` and pass `fixture campaign scan` and `verify` there (FR-051, US4 scenario 1); fixture: private corpus. **Needs the owner**: the integration created and shared, and where the postmortems live confirmed.
- [ ] T031 [US4] Re-run the coverage audit with the index populated from the recording, incident by incident: earlier postmortem on the same entities, cited or not, and record it beside the earlier re-runs in `docs/evaluation/` without private content (FR-053, SC-001, US4 scenario 2); fixture: private corpus. **Needs the owner**, after T030.

**Checkpoint**: SC-001 measured on real data.

---

## Phase 7: User Story 5 — the operator can see what was registered and why (Priority: P3)

**Goal**: a skeptical operator can trust the index.

**Independent Test**: `feed notion --check` on the twin prints the report of contracts §6.

- [ ] T032 [US5] Implement `--check` in `internal/cli/feed_notion.go`: credential evidence and acknowledgement, workspace label, per database its kind, data sources, resolved properties, pages registered and skipped with the closed set of reasons, pages outside the configured databases as a count only, the budget spent; emits nothing (US5 scenarios 1–2, contracts §6); fixture: unit over notion-knowledge-twin-01
- [ ] T033 [US5] Print the same registration report at the end of each reconciliation in the feeder's log, without summaries or bodies; fixture: unit

**Checkpoint**: all stories complete.

---

## Phase 8: Polish & cross-cutting

- [ ] T034 [P] Add the 007 row to `README.md`'s feature table and walk `docs/connectors/checklist.md` for the Notion connector, recording each box in the PR; fixture: unit (`scripts/check-specs.sh`)
- [ ] T035 [P] Run `./scripts/add-spdx-headers.sh`, `./scripts/check-no-secrets.sh`, `golangci-lint run` and `go test ./...` with `PG_DSN` set; fixture: CI
- [ ] T036 Walk [quickstart.md](./quickstart.md) §1–§4 and §6 on the twin and record the run in `quickstart.md`; §5 live once T030 is possible; fixture: notion-knowledge-twin-01

---

## Dependencies & execution order

- **Setup (T001–T003)** → **Foundational (T004–T009)** → stories.
- **US2 (T010–T016)** depends only on T004. It is the MVP and can ship as its own pull request.
- **US3 (T017–T025)** depends on T003, T005–T009; it does not need US2 to be built, only to be cited.
- **US1 (T026–T028)** depends on US2 and on T024.
- **US4 (T029–T031)**: T029 after T024; T030 and T031 need the owner (integration, location of postmortems).
- **US5 (T032–T033)** after T025.
- **Polish** last.

```text
T001,T002 ─► T003 ─► T006 ─► T007,T008 ─► T009 ─► US3 ─┐
T004 ─► US2 ───────────────────────────────────────────┴─► US1 ─► US4(T029) ─► US5 ─► Polish
T005 ─────────────► (T008, US3)                        T030 ─► T031 (owner)
```

## Parallel opportunities

- T001 and T002; T005 and T007 once T006 has its types.
- US2 (T010–T016) and the US3 client-independent tasks (T017, T018) touch different packages and can run side by side.
- T029 alongside T032.

## Implementation strategy

1. **PR 1 — US2 alone** (T001, T004, T010–T016): knowledge is cited live today. Smallest change that alters what an investigator sees.
2. **PR 2 — the Notion feeder** (T002, T003, T005–T009, T017–T025): Notion pages enter the graph.
3. **PR 3 — end to end and operator view** (T026–T029, T032–T036).
4. **Owner-gated** (T030, T031): the live recording and the audit re-run, once the integration exists and the postmortems' location is confirmed.
