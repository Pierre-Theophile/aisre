# Implementation Plan: Notion Knowledge Connector

**Branch**: `007-notion-knowledge` | **Date**: 2026-10-14 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/007-notion-knowledge/spec.md`, approved by the owner on
2026-10-14 with all five defaults (Q1–Q5).

## Summary

Two pieces, in dependency order:

1. **A graph-backed knowledge corpus, registered live** (`internal/investigation/workers/knowledge`
   plus `internal/store/postgres`): the `Corpus` the knowledge worker already defines, implemented
   as one bitemporal query over `graph.edge_versions` at the investigation's pinned instant, and
   registered by the runner and the evaluation harness beside the graph worker. This is Story 2 and
   it is useful on its own: documents registered with `knowledge link` and reviewed past
   investigations start being cited.
2. **A Notion feeder** (`internal/notionx` client, `internal/feeders/notion`, `aisre feed notion`):
   polls operator-named databases and emits, per page, exactly the events `knowledge link` emits by
   hand — one `upsert_node` `KNOWLEDGE_DOC` and one `upsert_edge` `CONCERNS` per mapped entity —
   plus `retract_edge` when a link goes away. No new event type, no new table.

**The approach is contract-first, as in 003–006.** A Phase 0 reading of the code against the spec
found the event shapes, the node and edge types, the ranking, the citation and the read-only
machinery already in place. What is missing is the five gaps below.

## Phase 0 — gaps found against the code

| # | gap | where | resolution |
|---|---|---|---|
| G1 | No `Corpus` implementation reads the graph, and neither `runner.New` nor `eval.NewEngine` registers the knowledge worker, so a live `knowledge_search` answers `NoWorkerError` | `internal/investigation/workers/knowledge`, `internal/investigation/runner/runner.go:190`, `internal/eval/harness.go:751` | `postgres.KnowledgeCorpus` (R1) and registration beside the graph worker, on by default, with `serve --no-knowledge-worker` to turn it off |
| G2 | `Corpus.Scoped(ctx, entityIDs)` takes no instant, so even a graph-backed corpus would answer "now", and a replay would cite documents written after the incident | `knowledge.go:124` | `Scoped(ctx, entityIDs, at Instants)` with valid and known instants from the request's `valid_at` and the investigation's pin (R2); an internal interface, so no published contract moves |
| G3 | Past investigations link to entities by `INVESTIGATED` edges whose validity is **the run's interval**, so "valid at T" never finds a past run | `internal/projector/record_investigation.go:128` | the corpus admits an `INVESTIGATION` node whose `INVESTIGATED` edge **started** before T and which carries a review label known before T (R3); the projector is unchanged |
| G4 | No source kind, rule namespace or document-ref namespace for Notion is published, though `knowledge link`'s help already uses `notion.page=` as its example | `docs/schema`, `docs/decisions` | **ADR-0013** before any schema-dependent task: source kind `notion`, source id `notion:<workspace label>`, ref namespace `notion.page`, rule `notion/notion-db@1` |
| G5 | Notion's API does not report an integration's capabilities, so `CheckReadOnly` cannot see the token's power | `pkg/feeder/credential.go` | use the existing `EvidenceOperatorAsserted` path with a signed acknowledgement (FR-003); the proof of read-only is the published operation surface (R4) |

## Technical Context

**Language/Version**: Go 1.27 (floor; the repository tracks the latest stable toolchain)

**Primary Dependencies**: `pkg/feeder` (events, `ReadOnlySurface` with one named query,
`QuotaBudget`, `record`, `testkit`, `EvidenceOperatorAsserted`); `internal/investigation/workers/
knowledge` (`LinkRequest.Events`, `Document`, BM25); `internal/sanitise`; `internal/campaign`. The
Notion REST API at `Notion-Version: 2025-09-03`: `GET /v1/users/me`, `GET /v1/databases/{id}`,
`POST /v1/data_sources/{id}/query`, `GET /v1/pages/{id}`, `GET /v1/pages/{id}/properties/{property_id}`.
**No Notion client library and no new module**: five operations, hand-written, so the read-only
claim is proved over the list (R4).

**Storage**: PostgreSQL 16, unchanged. Pages become graph nodes and edges through the event log.
The corpus is one read query over `graph.edge_versions` and `graph.node_versions`. The feeder's
watermark per data source is a feeder checkpoint like every other feeder's.

**Testing**: `go test ./...`; fixtures under `fixtures/notion-*` verified by `aisre fixture verify`
(replay from empty, double delivery, seeded shuffle); the knowledge worker's goldens re-run against
the Postgres corpus; `knowledge-scope-01` through the evaluation harness with the worker registered;
a recorded Notion API double; `golangci-lint`; `scripts/check-specs.sh`.

**Target Platform**: Linux server, the same single binary: `aisre feed notion [--check]`, and the
knowledge worker inside `serve --enable-investigation`.

**Project Type**: a connector (feeder only, no sink, no worker of its own) plus the wiring of an
existing worker, inside the single-module Go project.

**Performance Goals**: SC-003 — a page edit reflected within 20 minutes at the 15-minute interval,
p95. The corpus query answers in the knowledge worker's `cheap` cost class: one indexed query per
`knowledge_search`, bounded by the number of entities asked.

**Constraints**: no page body ever fetched (FR-022); no person ever recorded (FR-023, FR-034); no
write to Notion (FR-001–FR-004); retrieval as of the pin, so a replay never cites a later document
(FR-006, FR-007). Notion allows about three requests per second per integration (FR-044).

**Scale/Scope**: one workspace per source; tens of databases at most; hundreds to low thousands of
pages. A full reconciliation of a thousand pages is ten queries of 100.

## Constitution Check

*GATE: passes before Phase 0; re-checked after design (no change).*

| principle | verdict | how |
|---|---|---|
| I. Graph First | pass | Documents are `KNOWLEDGE_DOC` nodes with `CONCERNS` edges; the corpus reads the graph and nothing else; retrieval is scoped by edges before ranking |
| II. Bitemporal | pass | Edge validity starts at the page's observed link instant and ends when it is observed gone; the corpus reads at the investigation's valid **and** known instants, so a replay is stable |
| III. Event-sourced | pass | Event ids derive from page id, content digest and instant; re-polls and restarts emit nothing new; fixtures replay from empty, doubled and shuffled |
| IV. Telemetry stays home | pass | No telemetry is involved; no page body is stored either |
| V. Evidence-first | pass | Every edge carries `rule@version`, database, property and value; skipped pages are reported with the excluding clause; citations carry provenance and age (002 FR-050) |
| VI. Resolution auditable | pass | Entities come only from the operator's map; nothing is matched by name or text |
| VII. Read-only by default | pass | Five operations, four `GET` and one named-query `POST` justified individually; credential evidence `operator_asserted` with a signed acknowledgement (G5); no sink |
| VIII. Evaluation | pass | Recorded campaign in the private corpus, synthetic twins in public; SC-001 re-runs the audit with the index populated |
| IX. Open schema | pass | ADR-0013 publishes the additions before code depends on them |
| X. Simplicity | pass | No client library, no new table, no new event type, no new worker |
| Scope: RAG only over linked durable knowledge | pass | This feature is that clause's first feeder |

## Project Structure

### Documentation (this feature)

```text
specs/007-notion-knowledge/
├── spec.md
├── plan.md              # this file
├── research.md          # R1–R7
├── data-model.md        # database binding, node and edge props, the corpus query
├── contracts/
│   └── notion-connector.md   # published names, operation list, configuration, report
├── quickstart.md        # validation run
├── checklists/
│   └── requirements.md
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
docs/decisions/ADR-0013-shared-contract-additions-for-007.md
docs/connectors/notion.md

internal/
├── store/postgres/
│   └── knowledge.go                 # KnowledgeCorpus: CONCERNS at T, reviewed INVESTIGATED before T
├── investigation/
│   ├── workers/knowledge/knowledge.go   # Scoped gains the instants (G2)
│   └── runner/runner.go             # registers the knowledge worker beside the graph worker
├── eval/harness.go                  # same, over the fixture's replayed graph
├── notionx/                         # hand-written client: five operations, budget, Retry-After
│   ├── client.go
│   ├── operations.go                # the published ReadOnlySurface
│   └── budget.go
├── feeders/notion/
│   ├── feeder.go                    # poll → bind → emit, reconcile daily
│   ├── binding.go                   # database binding and the value → entity map (notion-db@1)
│   └── page.go                      # page → LinkRequest; summary build, people stripped
├── sanitise/policy_notion.go
└── cli/
    ├── feed_notion.go               # aisre feed notion [--check]
    └── serve.go                     # --no-knowledge-worker

fixtures/
├── notion-knowledge-twin-01/        # synthetic structural twin of a postmortem and a runbook database
└── incidents/knowledge-notion-01/   # SC-001/SC-002: linked, out-of-subgraph and later postmortems
```

**Structure Decision**: the established connector layout (`internal/<vendor>x`,
`internal/feeders/<vendor>`, `cli/feed_<vendor>.go`) as 003–006. The corpus lives in
`internal/store/postgres` beside the other read queries rather than in the worker package, so the
worker stays testable without a database, as it is today.

## Design decisions (detail in research.md)

- **R1 — The corpus is one query.** `CONCERNS` edges into the requested entities, valid at T and
  known at the pin, joined to their `KNOWLEDGE_DOC` node version at the same instants.
- **R2 — Two instants.** Valid time is the request's `valid_at`; known time is the investigation's
  observed pin, so a document registered after the run began is not a candidate on replay.
- **R3 — Past investigations.** Admitted when their `INVESTIGATED` edge started before T and a
  review label was known before T; ranked on the verdict line, which the engine wrote.
- **R4 — Read-only proof.** Five operations; the data-source query is the one `POST`, declared as a
  named query with its justification; the credential is `operator_asserted`.
- **R5 — Notion's 2025-09-03 data-source model.** A database id resolves to its data sources;
  queries run per data source; a database with several data sources binds all of them.
- **R6 — Change detection.** Incremental polls filter on `last_edited_time` from the watermark minus
  a skew margin; the daily reconciliation lists every page, and a page no longer returned is read
  once by id: trashed or moved out, its links end at Notion's own edit instant; gone entirely, at
  the last instant it was seen. Never at "now" (the `retract_edge` rule).
- **R7 — Truncated property values.** Query results carry at most 25 references per title, rich
  text or relation property; a value that hits the limit is completed with the property-item
  endpoint, on a page already inside a configured database.

## Complexity Tracking

No constitution violation to justify. Two items worth naming:

- **The corpus is one hand-written SQL query** rather than a composition of `Subgraph` calls. A
  `Subgraph` at T cannot see past investigations (G3), and one call per entity would cost a round
  trip per entity; the query is tested against the in-memory corpus's goldens so the two cannot
  drift.
- **`Scoped` changes signature.** It is internal and has two implementations (the test double and
  this one), so changing it is cheaper than a second interface.
