# Feature Specification: Notion Knowledge Connector

**Feature Branch**: `007-notion-knowledge`

**Created**: 2026-10-14

**Status**: Approved by the owner 2026-10-14 (scope and all five defaults); planning

**Input**: User description: "007 document connectors: a read-only Notion connector that feeds the
investigation engine's knowledge index with postmortems and runbooks, so an investigation can cite
how a similar past incident was resolved." The owner chose this scope (Notion, postmortems and
runbooks) on 2026-10-14, over a broader set of document sources.

## Why this feature exists

Feature 002 built a knowledge worker that retrieves durable documents — postmortems, runbooks,
decisions, past investigations — scoped by the graph: only a document linked by a `CONCERNS` edge
to an entity in the investigation's subgraph is ever a candidate (002 FR-049, constitution "the
graph is the RAG index"). Feature 002 also recorded, as plan finding F5, that **nothing feeds that
index**: no feeder emits documents, so the corpus is whatever a person registers one at a time with
`knowledge link`, and in practice it is empty. ADR-0001 and the 006 spec both name document
connectors as the later feature that closes this; this is that feature.

Reading the code for this spec found a second gap that has to close first: **the knowledge worker
is not registered in a running deployment.** It exists, is tested against an in-memory corpus, and
`knowledge link` writes its nodes and edges, but no implementation of its corpus reads the graph and
`serve` never registers it, so a live investigation that asks `knowledge_search` gets "no registered
worker" (`engine.NoWorkerError`). A connector feeding an index nobody reads would change nothing an
investigator sees.

What it adds, in priority order:

1. **The knowledge worker, live.** A corpus that resolves `CONCERNS` edges in the graph as of the
   investigation's pinned instant, registered by `serve`, so registered documents and past
   investigations are actually retrieved and cited (002 FR-049–FR-052, SC-014).
2. **Notion postmortems and runbooks as durable knowledge.** A read-only feeder that turns each page
   of an operator-named Notion database into a `KNOWLEDGE_DOC` node with a pointer back to the page
   and one `CONCERNS` edge per entity the page names, exactly what `knowledge link` produces by hand,
   kept current as pages are edited, moved or archived.
3. **The same recording, sanitisation and replay discipline as every other connector**, and an
   operator check that shows which pages were registered, which were not, and why.

## Clarifications

### Session 2026-10-14 — defaults approved by the owner

These forks change scope or security posture. Each default is written into the requirements below;
the owner approved all five as written on 2026-10-14.

- Q1: Which pages are knowledge? → **Default: only pages inside Notion databases the operator names
  by id, each database declared as one published kind (`postmortem` or `runbook`).** Pages shared
  with the integration outside those databases are ignored and counted, never registered. Workspace
  search is not used to find documents. (FR-010–FR-012)
- Q2: What text does retrieval rank, given that 002 FR-049a forbids storing a document's content?
  → **Default: the page title plus up to three short text properties the operator names (for
  example "Summary", "Root cause", "Resolution"), capped at 600 characters together.** These are
  what a person wrote *about* the incident in a structured field, the same thing `knowledge link
  --summary` holds. The page body is never fetched. A page whose named properties are empty is
  ranked on its title alone. (FR-020–FR-023)
- Q3: How does a page get linked to graph entities? → **Default: from one operator-named property
  (a select, multi-select or relation listing the affected services), through an operator map from
  property value to graph entity**, the 006 FR-007 posture. Nothing is inferred from the title or
  text. A value the map does not name is reported; a page with no mapped entity is not registered,
  because a document linked to nothing can never be retrieved (002 FR-049). (FR-030–FR-033)
- Q4: Who is the registering identity (002 FR-049a)? → **Default: the connector, as
  `notion:<workspace>` under the published rule `notion-db@1`, plus the authenticated operator who
  signed the configuration.** Notion people (author, last editor, owners) are never recorded.
  (FR-034)
- Q5: Transport? → **Default: poll each database for pages edited since the last poll, every 15
  minutes, with a full reconciliation daily.** Notion webhooks need a public endpoint and are not
  offered in v1. Edits become later facts; a page archived, trashed or moved out of a database
  closes its links at the instant it was observed gone, and history is kept. (FR-040–FR-044)

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An investigation cites the postmortem of the last similar incident (Priority: P1)

The checkout service is slow again. Six months ago it was slow because a connection pool was
exhausted after a deploy doubled the worker count, and the team wrote a postmortem in the
"Postmortems" database in Notion, with `checkout` in its Services property and two lines under
"Resolution". The investigation opens; among its findings is a citation of that postmortem — its
title, its age, the entity it is linked to, how that link was made, and a link that opens the page
— and the hypothesis it suggests ("pool exhaustion after a scale-up") is marked knowledge-derived
until the metrics worker confirms or refutes it.

**Why this priority**: it is the outcome the owner asked for, and it needs both the live worker and
the feeder; neither is worth anything alone.

**Independent Test**: replay a recorded Notion database with three postmortems — one linked to the
investigated service, one linked to a service outside the subgraph that is the better lexical
match, one written after the incident's pinned instant — over an incident fixture; assert the first
is cited, the other two are cited in 0 % of runs.

**Acceptance Scenarios**:

1. **Given** a postmortem page linked to an entity inside the investigation's subgraph, **When** the
   engine runs `knowledge_search`, **Then** the page is returned and cited with its identifier, its
   linked entity, the link's provenance (`notion-db@1`, the property, the configuring operator), its
   age, and its location.
2. **Given** a postmortem linked only to entities outside the subgraph, **Then** it is never
   returned (002 FR-049), however well its title matches.
3. **Given** a postmortem created or linked after the investigation's pinned instant, **When** the
   investigation is run or replayed, **Then** it is not a candidate: retrieval is as of the pinned
   instant, so a replay never cites knowledge the investigator could not have had.
4. **Given** a hypothesis that rests only on a cited postmortem, **Then** it is reported
   knowledge-derived and unconfirmed and does not reach *supported* without a source-of-truth worker
   (002 FR-051).
5. **Given** a page whose title or summary reads as an instruction to the engine, **Then** it is
   data: scope, budgets, worker set, posture and output are unchanged and the attempt is recorded
   (002 FR-017).

---

### User Story 2 - Documents registered by hand and past investigations are retrieved live (Priority: P1)

Riley registered a runbook with `knowledge link` last month and the team has reviewed a dozen
investigations since. A new investigation on the same service cites the runbook and the most
relevant past investigation, without Notion configured at all.

**Why this priority**: it is the prerequisite of Story 1 and closes a gap that already exists in
002: today none of this is retrieved by a running deployment.

**Independent Test**: start `serve --enable-investigation` against a database where `knowledge link`
registered a runbook; run an investigation whose subgraph contains the linked entity; assert
`knowledge_search` is answered (not `no registered worker`) and the runbook is cited. The existing
`knowledge-scope-01` fixture passes against the graph-backed corpus as it does against the
in-memory one.

**Acceptance Scenarios**:

1. **Given** a deployment with investigation enabled, **Then** the knowledge worker is registered by
   default and `knowledge_search` is answered.
2. **Given** an operator who disables it, **Then** the term is reported unavailable and the
   investigation proceeds, as for any absent worker.
3. **Given** a document whose `CONCERNS` edge was closed before the pinned instant, **Then** it is
   not a candidate.

---

### User Story 3 - Notion pages become and stay knowledge, read-only (Priority: P1)

Casey configures the feeder with the Postmortems and Runbooks database ids, the Services property,
the map from Services values to graph entities, and the Summary and Resolution properties. Within one
poll, every page with a mapped service is a `KNOWLEDGE_DOC`. When someone edits a postmortem's
Resolution, the next poll records the new summary as a later fact; when someone archives a runbook,
its links close at the instant it was seen gone.

**Why this priority**: without it Story 1 has nothing to cite.

**Independent Test**: replay a recorded database through the feeder from empty, double-delivered
and shuffled; assert one node per page, one edge per mapped entity, the edit as a later version, the
archive as closed links, and that the recorded client made no call outside its published read-only
surface.

**Acceptance Scenarios**:

1. **Given** a page in a configured database with two mapped services, **Then** one `KNOWLEDGE_DOC`
   node keyed on the page's stable id and two `CONCERNS` edges are emitted, with kind, title, the
   capped summary, authored instant and location.
2. **Given** the same page polled again unchanged, any number of times, **Then** nothing new is
   emitted.
3. **Given** a page whose Services property changes from `checkout` to `checkout, payments`, **Then**
   one edge is added at the observed instant and the existing one is untouched.
4. **Given** a page archived, trashed, or moved out of every configured database, **Then** its edges
   are closed at the observed instant; the node and its history remain.
5. **Given** a page whose services the map does not name, **Then** it is not registered and the
   operator check lists it with the unmapped values.

---

### User Story 4 - Recorded, sanitised and replayable (Priority: P2)

Casey records the organisation's postmortem and runbook databases so the feeder is tested against
how people actually fill them in: empty Resolution fields, services spelled three ways, pages moved
between databases.

**Why this priority**: constitution VIII — a connector is not stable on synthetic data alone.

**Independent Test**: `fixture campaign scan` and `verify` pass over the recording in the private
corpus; the public repository carries a synthetic structural twin that replays from empty,
double-delivered and shuffled.

**Acceptance Scenarios**:

1. **Given** a recording, **Then** it contains no page body, no person (author, editor, people
   properties, mentions), no email, and no property the operator did not name; page, database and
   workspace ids are pseudonymised with the corpus key.
2. **Given** the private audit's incidents, **When** the recording is replayed with each incident,
   **Then** the report lists, incident by incident, whether an earlier postmortem on the same
   entities existed and whether it was cited (SC-001).

---

### User Story 5 - An operator can see what the connector registered and why (Priority: P3)

Riley runs `aisre feed notion --check` and sees, per configured database: its kind, the properties
used, how many pages were registered, and each page that was not, with the reason (`no mapped
entity`, `unmapped value`, `outside the configured databases`, `archived`), plus the statement that
the integration's capabilities cannot be verified through Notion's API and what was acknowledged
instead.

**Why this priority**: it is how a skeptical operator trusts the index, but nothing above depends on
it.

**Acceptance Scenarios**:

1. **Given** a page that was not registered, **Then** `--check` names the clause that excluded it.
2. **Given** a configuration without the read-only acknowledgement of FR-003, **Then** the feeder
   refuses to start and says what to acknowledge.

### Edge Cases

- **A page in two configured databases** (a linked view, or moved and copied): one node, keyed on
  the page id; its kind is the first database's in configuration order, and the conflict is
  reported.
- **A postmortem with no services filled in**: not registered; reported. It would be retrievable by
  nothing, so registering it would only grow a corpus nobody can reach.
- **A service renamed in the graph**: the map names graph entities by stable reference; an edge to an
  entity that no longer resolves is reported by the graph's own invariant check, not silently moved.
- **A very long summary property**: truncated at the cap on a word boundary and marked truncated;
  the full text stays in Notion behind the link.
- **A page restored from trash**: its links reopen at the observed instant, as a new interval.
- **Notion rate limiting** (about three requests per second per integration): the feeder spends from a
  declared budget, honours `Retry-After`, and a partial poll is recorded as a coverage gap, never as
  "no pages".
- **The integration loses access to a database** (unshared): reported as a coverage gap; nothing is
  closed, because absence of access is not evidence the documents are gone.
- **Prompt injection in a title or summary**: data, recorded as an evidence item (002 FR-017,
  `injection-01` precedent).
- **A runbook that tells the reader to run a command**: retrieval cites it; nothing in this project
  executes anything (constitution VII).

## Requirements *(mandatory)*

### A. Scope and posture

- **FR-001**: The feeder MUST only read. Its client MUST publish a closed list of the read
  operations it calls (`feeder.ReadOnlySurface`), and a test MUST assert that a recorded run makes no
  call outside it.
- **FR-002**: The feeder MUST NOT create, edit, comment on, move, archive or share any page or
  database, and MUST NOT read any page outside the configured databases.
- **FR-003**: Notion does not let an integration's capabilities be read through its API, so the
  read-only check of `pkg/feeder.CheckReadOnly` cannot be performed on the token. The operator MUST
  acknowledge, in the signed configuration, that the integration was created with read-content
  capability only and no user-information capability; without that acknowledgement the feeder MUST
  refuse to start (the 003 `UnverifiableAreas` posture).
- **FR-004**: This feature MUST NOT add a write path of any kind; Notion is not a report-delivery
  surface in v1.

### B. Live knowledge retrieval

- **FR-005**: The knowledge worker MUST be registered by `serve` whenever investigation is enabled,
  unless the operator disables it.
- **FR-006**: Its corpus MUST resolve `CONCERNS` edges in the graph as of the investigation's
  pinned instant, returning only documents linked to the requested entities and valid at that
  instant.
- **FR-007**: Replaying an investigation MUST retrieve the same candidate set as the original run.
- **FR-008**: Concluded, reviewed investigations MUST be candidates on the same terms (002 FR-052),
  if they are not already.

### C. Which pages

- **FR-010**: The feeder MUST read only databases the operator names by id; each MUST be declared
  with one published kind, `postmortem` or `runbook`.
- **FR-011**: The feeder MUST NOT use workspace search to discover documents.
- **FR-012**: Pages shared with the integration outside the configured databases MUST be counted
  and reported, never read beyond their identifier.

### D. What is registered

- **FR-020**: Each page MUST become one `KNOWLEDGE_DOC` node keyed on its stable page id, carrying
  kind, title, summary, authored instant (the page's creation instant, or a date property the
  operator names), and location (the page's URL).
- **FR-021**: The summary MUST be built only from the text properties the operator names (at most
  three), in the order named, capped at 600 characters together, truncation marked.
- **FR-022**: The page body MUST NOT be fetched, and no content beyond FR-020 and FR-021 MUST be
  stored (002 FR-049a).
- **FR-023**: Rich-text formatting, mentions of people, and inline links MUST be reduced to plain
  text with people removed before anything is stored.

### E. Links to the graph

- **FR-030**: Links MUST come only from one operator-named property per database (select,
  multi-select or relation), through an operator map from value to graph entity reference.
- **FR-031**: Nothing MUST be inferred from the title, summary or body.
- **FR-032**: A value the map does not name MUST be reported, never guessed; a page with no mapped
  value MUST NOT be registered.
- **FR-033**: Each `CONCERNS` edge MUST carry its provenance: the rule (`notion-db@1`), the database,
  the property, and the value that produced it.
- **FR-034**: The registering identity MUST be `notion:<workspace>` with the rule reference and the
  operator principal that signed the configuration; no Notion person MUST be recorded.

### F. Transport and change

- **FR-040**: The feeder MUST poll each configured database for pages edited since its last
  watermark, at a default interval of 15 minutes, and reconcile every page in full once a day.
- **FR-041**: Re-delivering, re-polling or restarting MUST produce no duplicate node, edge or
  version (idempotent on page id and content digest).
- **FR-042**: A changed title, summary, kind or link set MUST be recorded as a later fact at the
  observed instant; earlier versions remain queryable.
- **FR-043**: A page archived, trashed or moved out of every configured database MUST have its links
  closed at the instant it was observed gone; a restored page MUST have them reopened as a new
  interval.
- **FR-044**: Calls MUST be spent from a declared budget, honour `Retry-After`, and a poll cut short
  MUST be recorded as a coverage gap.

### G. Sanitisation, recording and evaluation

- **FR-050**: Sanitisation MUST happen in the connector before anything touches disk, under a
  `notion` policy table: page bodies never fetched; people, emails and non-named properties dropped;
  page, database and workspace ids pseudonymised with the corpus key in recordings.
- **FR-051**: The public repository MUST carry only synthetic structural twins.
- **FR-052**: Fixtures MUST cover: a linked postmortem, an out-of-subgraph postmortem that is the
  better lexical match, a postmortem authored after the pinned instant, an edited page, an archived
  and restored page, an unmapped service value, a page in two databases, an injection attempt, and
  a rate-limited partial poll.
- **FR-053**: The coverage audit MUST be re-run with the knowledge index populated from the private
  corpus's databases, and the result recorded beside the earlier re-runs.

### Key Entities

- **Database binding**: database id, kind, entity property, value-to-entity map, summary properties
  (at most three), optional authored-date property. Operator configuration, signed.
- **Knowledge document**: the existing 002 `KNOWLEDGE_DOC` node, keyed on the Notion page id.
- **Concerns link**: the existing `CONCERNS` edge, with FR-033 provenance and a validity interval.
- **Registration report**: per database, pages registered and pages skipped with reasons; never
  page content.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every audited incident where an earlier postmortem in a configured database is
  linked to the incident's affected entities, the investigation cites that postmortem in at least
  90 % of runs (002 SC-014, measured against what is registered).
- **SC-002**: Documents linked only to entities outside the investigation's subgraph, or valid only
  after its pinned instant, are cited in 0 % of runs.
- **SC-003**: A page created or edited in a configured database is reflected in the index within
  20 minutes at the default interval, at p95.
- **SC-004**: Zero calls outside the published read-only surface across the conformance suite and a
  recorded live run.
- **SC-005**: No recording, node, log line or report contains a page body, a person's name or
  email — asserted by the independent scan and by canary tokens planted in the fixtures.
- **SC-006**: With the feeder not configured, investigation outcomes on the 002 fixture corpus are
  identical to those with only hand-registered documents.

## Out of scope for this feature

- Any write to Notion, including posting the report there.
- Reading page bodies, comments or attachments; embeddings or semantic search over content.
- Notion workspace search, wikis and pages outside configured databases.
- Other document sources (Google Drive, Confluence, GitHub markdown runbooks); the feeder contract
  admits them as later features.
- Architecture decision records as a kind; `decision` is published but not fed by this connector
  in v1.
- Inferring entities from text, or turning a postmortem's timeline into graph events.
- Notion webhooks.

## Assumptions

- Features 001–006 are complete. This feature uses 002's `KNOWLEDGE_DOC` node, `CONCERNS` edge and
  the knowledge worker unchanged in contract, and adds no new event type.
- The organisation keeps (or will keep) postmortems and runbooks as pages of Notion databases with a
  property naming the affected services. If they live as loose pages, Q1's default needs revisiting
  before planning.
- A workspace admin can create an internal Notion integration with read-content capability only and
  share the chosen databases with it.
- A shared-contract addition is needed for the `notion` source kind and the `notion.page` document
  reference namespace; it is recorded in a new ADR before any schema task, as ADR-0011 did for 006.
