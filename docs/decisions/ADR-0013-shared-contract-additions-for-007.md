# ADR-0013: The shared contract additions for feature 007

- Status: **Accepted** (2026-10-14)
- Deciders: project owner (approval of the 007 scope, its five defaults, the plan and the tasks,
  2026-10-14), Claude (design, documented here)
- Source: `specs/007-notion-knowledge/plan.md` §"Phase 0 — gaps found against the code" (G1–G5),
  `research.md` R1–R4. Written under `specs/007-notion-knowledge/tasks.md` T001.

## Context

Feature 007 feeds the knowledge index from Notion and makes a running deployment read it. The
`KNOWLEDGE_DOC` node, the `CONCERNS` edge, the knowledge worker and `knowledge link` all exist
(002, ADR-0005 D3). What does not is a published name for Notion as a source, a published shape for
the links a connector makes, and a way for the knowledge worker to ask the graph *as of* an
instant. Each is read by more than one feature or by operators, so it is decided here rather than
inside the connector (constitution IX).

## Decision

### Item 1 — published names

| name | value |
|---|---|
| source kind | `notion` |
| source id | `notion:<workspace label>` — one workspace, one source; the label is the operator's, checked against the integration's workspace at start |
| document ref namespace | `notion.page` (value: the page id) — already the example in `knowledge link --help` |
| rule | `notion/notion-db@1` |
| registering identity | the source id; the operator principal who signed the configuration is recorded beside it as `configured_by` |

### Item 2 — what a connector-made link carries

A `KNOWLEDGE_DOC` registered by a connector carries the props `knowledge link` writes, plus
`configured_by` and `summary_truncated`. Each `CONCERNS` edge it makes carries `rule`, `database`,
`property` and `value`: the clause that produced it. A link made by hand carries none of them, and
nothing reads their absence as a fault.

### Item 3 — the knowledge corpus is read as of two instants

`knowledge.Corpus.Scoped` takes the request's valid instant and observed instant. A document is a
candidate only when its `CONCERNS` edge was valid at the first and known at the second, so a replay
retrieves exactly what the original run could have retrieved. The interface is internal; the
published `knowledge_search` term does not change.

A reviewed past investigation is a candidate when its `INVESTIGATED` edge to a requested entity
**started** before the valid instant and a review label was known at the observed instant. The
projector writes that edge over the run's own interval, which is right for "what was being
investigated when"; reading its start is how the corpus asks "what was learned before" without a
second, derived edge.

### Item 4 — a credential whose power the platform will not report

Notion does not report an integration's capabilities through its API, so `CheckReadOnly` cannot
classify the token. The connector uses the existing `operator_asserted` evidence, and refuses to
start unless its signed configuration acknowledges `notion.capabilities` — that the integration was
created with read-content capability only. The read-only claim itself rests on the published
operation surface (`notionx.Surface`), which compiles five operations, one of them a named-query
`POST`.

## Alternatives rejected

- **A `Subgraph` call per entity as the corpus** (research R1): one round trip per entity, and it
  cannot see past investigations, whose edges end with the run.
- **Projecting an open-ended `CONCERNS` edge from each reviewed investigation** (research R3): a
  projector change and a derived fact for what one predicate says.
- **Workspace search to find documents** (spec Q1): it reaches pages nobody chose, and its results
  depend on the integration's sharing rather than on a configuration someone signed.
- **A Notion SDK** (research R4): its surface includes every write.

## Consequences

- A running deployment answers `knowledge_search`; until this ADR it answered "no registered worker".
- Documents and reviewed investigations registered after an incident are never cited in its replay.
- Other document connectors (Google Drive, Confluence) reuse items 2 and 3 as they are and add their
  own item 1.
