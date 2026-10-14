# Research: Notion Knowledge Connector

Decisions taken while planning feature 007. Each is reversible and records what was rejected.
Statements about Notion's API are from its public reference at `Notion-Version: 2025-09-03`; the
ones a task must confirm against a live call before relying on them are marked **verify**.

## R1. The corpus is one bitemporal query

**Decision**: `postgres.KnowledgeCorpus.Scoped(ctx, entityIDs, at)` runs one query:

- `CONCERNS` edges in `graph.edge_versions` whose `dst_id` is a requested entity, whose `valid`
  contains `at.Valid` and whose `observed` contains `at.Observed`;
- joined to the source's `KNOWLEDGE_DOC` version in `graph.node_versions` at the same two instants;
- union the past investigations of R3;
- grouped per document, so a document linked to two requested entities is one candidate with both
  in `LinkedEntityIDs`.

`HopsFromFocus` is 0 for every candidate: the worker is called with entity ids the engine already
chose from the subgraph, and the corpus does not walk the graph.

**Rationale**: the worker's contract is "scoped by the graph first, ranked second"; the scoping is
one join, and doing it in SQL keeps the `cheap` cost class honest.

**Rejected**: composing `Subgraph(focus=entity, hops=1, edge_types=[CONCERNS])` per entity — a round
trip per entity, and it cannot express R3. Embeddings — excluded by 002 research §7 and by FR-022
(there is no content to embed).

## R2. Two instants, from the request

**Decision**: `Scoped` takes `Instants{Valid, Observed}` from the `AlgebraRequest`'s `valid_at` and
`observed_at` (unset observed means valid, FR-004 of 002). The worker already reads `valid_at` for
its coverage window.

**Rationale**: valid time alone would let a replay cite a postmortem registered last week about an
incident last year whose link was back-dated; the known-time bound is what makes FR-007 (a replay
retrieves the same set) true.

## R3. Past investigations are admitted on their start and their review

**Decision**: an `INVESTIGATION` node is a candidate when it has an `INVESTIGATED` edge to a
requested entity whose `lower(valid)` is before `at.Valid`, and its version known at `at.Observed`
carries `sre.investigation.was_this_right` (a review label, 002 FR-054). Its kind is
`investigation`, its title the verdict line, its summary empty, its location the investigation id,
and its provenance `investigated by <requester>; reviewed by <labelled_by>`.

**Rationale**: `record_investigation` writes `INVESTIGATED` over the run's own interval, which is
right for "what was being investigated when" and wrong for "what do we know"; reading the edge's
start instead of changing the projector keeps 001's projection untouched. 002 FR-052 asks for
*reviewed* investigations, which the label is.

**Rejected**: projecting a second, open-ended `CONCERNS` edge from each reviewed investigation — a
projector change and a new derived fact for something one predicate expresses.

## R4. Read-only proof over five operations

**Decision**: `notionx.Surface` is a `feeder.MustReadOnlySurface` of:

| operation | why |
|---|---|
| `GET /v1/users/me` | at start: the bot user and its workspace name, checked against the configured workspace label |
| `GET /v1/databases/{id}` | resolve a configured database to its data sources and property schema |
| `POST /v1/data_sources/{id}/query` | **named query**: returns pages of the data source matching a filter and sort; the request body has no field that creates, updates or deletes anything |
| `GET /v1/pages/{id}` | only for a page that was registered and is no longer returned (R6) |
| `GET /v1/pages/{id}/properties/{property_id}` | only for a named property truncated at 25 references (R7) |

The credential report is `EvidenceOperatorAsserted`, accepted only with the signed acknowledgement
of FR-003. **verify**: that no endpoint reports an integration's capabilities (as of 2025-09-03, none
is documented).

**Rejected**: `POST /v1/search` (workspace search, FR-011); `GET /v1/blocks/{id}/children` (page
bodies, FR-022); `GET /v1/users` (people, FR-034); an SDK (its surface includes every write).

## R5. Databases and data sources

**Decision**: the operator configures database ids, as they appear in a database's URL. At start,
`GET /v1/databases/{id}` resolves each to its data sources; every data source of a bound database is
queried with the database's binding. Property names in the binding are resolved to property ids per
data source at start, and a binding whose property is missing from a data source is a start-up error
naming both.

**Rationale**: since 2025-09-03 a database is a container of one or more data sources and queries run
per data source. Operators know databases; data sources are an API detail the feeder resolves.

## R6. Change detection and end instants

**Decision**:

- **Incremental poll** (default every 15 min): query each data source with a `last_edited_time`
  filter `on_or_after` the watermark minus 2 minutes, sorted ascending; the watermark advances to
  the newest `last_edited_time` processed. Notion's `last_edited_time` is minute-rounded
  (**verify**), hence the margin; event ids are content digests, so the overlap re-emits nothing.
- **Reconciliation** (default daily, and at start): list every page of every data source. A page
  registered before and absent now is read once with `GET /v1/pages/{id}`:
  - `in_trash` (or `archived`) true, or its parent is no longer a bound data source → its edges get
    `retract_edge` with `valid_end` = that page's `last_edited_time`;
  - 404 or `object_not_found` → `valid_end` = the last instant it was seen;
  - any other failure → a coverage gap; nothing is retracted.
- **A whole database unreadable** → coverage gap for that database; nothing is retracted (spec Edge
  Cases).
- **Restore** → the page reappears; its edges are upserted again from its `last_edited_time`, a new
  interval.

**Rationale**: trashed pages are absent from query results, so absence must be confirmed, and the
`retract_edge` rule (`internal/projector/retract_edge.go`) forbids ending a fact at "now".

**Rejected**: webhooks (public ingress, spec Q5); deleting nodes (history is kept).

## R7. Truncated property values

**Decision**: query results carry at most 25 references for `title`, `rich_text`, `relation` and
`people` values (**verify**: the `has_more` marker on relations, and how truncated rich text is
signalled). For a named summary or entity property at that limit, the feeder reads the property item
endpoint, paginated, stopping at the 600-character summary cap. People properties are never read.

## R8. Sanitisation

**Decision**: `internal/sanitise/policy_notion.go`:

- drop `created_by`, `last_edited_by`, every `people` property, every property not named in a
  binding, `icon`, `cover`, `public_url`;
- reduce rich text to plain text and drop `mention` spans of type `user`;
- in recordings, pseudonymise page, database, data-source and workspace ids with the corpus key;
- keep title and named summary properties in private recordings (they are the ranking text);
- the public repository carries only synthetic twins (FR-051).

The independent scan (`fixture campaign scan`) gets Notion's user-id and email shapes, and canary
tokens are planted in the twin's dropped properties (SC-005).
