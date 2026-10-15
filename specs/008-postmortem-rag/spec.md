# Feature Specification: Postmortem Retrieval Over Full Content

**Feature Branch**: `008-postmortem-rag`

**Created**: 2026-10-15

**Status**: Approved by the owner 2026-10-15 (all five defaults and the constitution amendment); planning

**Input**: User description: "Postmortems should not be ingested into the graph but RAG-ed to allow
retrieval on them; the temporal graph is there for events. That is how a state-of-the-art AI SRE
agent should be built." The owner asked to move forward with this on 2026-10-15, after running 007's
`feed notion --check` against a real workspace.

## Why this feature exists

Feature 007 made Notion postmortems and runbooks durable knowledge. Running its `--check` against the
owner's real "SRE Agent Post Mortem" database (seven postmortems, 2026-10-15) showed what that design
leaves on the table:

- **Retrieval reads almost nothing.** 007 ranks a page's title plus at most 600 characters of named
  properties, and never reads the page body (007 Q2, 002 FR-049a: "the document's content MUST NOT be
  stored"). The real postmortems carry "Signals / Symptoms", "Root cause" and "Fix / Mitigation"
  fields of 250 to 1,750 characters each. The 600-character cap keeps roughly the first field; the
  symptoms an investigator would actually match on ("Error getting context", "router discovered 0
  engines", "webhooks failing validation") are cut.
- **Retrieval is lexical only.** BM25 over a title and a summary misses a postmortem whose words differ
  from the live incident's ("LLM calls 404" versus "upstream returned not found").
- **Retrieval is a hard filter on the graph.** Only documents linked to an entity inside the
  investigation's subgraph are candidates (002 FR-049). A vLLM router failure filed under *Infra* is
  never shown when *Candidate View* breaks the same way, though that is exactly the lesson to recall.

The owner's direction: the temporal graph holds events, topology and time; postmortem **content** is
retrieved by a real retrieval index. This feature builds that index and changes the knowledge worker
to use it, while keeping the two things only the graph can give: **which entities a document
concerns**, and **when the agent could have known it**.

What it adds, in priority order:

1. **Retrieval over the whole postmortem**, by section, ranked by meaning and by words together, and
   queried with the live incident's symptoms.
2. **The graph as a ranking signal and a clock, not a gate**: documents linked to the investigated
   entities rank higher, documents about other services can still be retrieved, and nothing written
   or edited after the investigation's pinned instant is ever retrieved.
3. **An evaluation of retrieval itself**, so "state of the art" is a measured claim.

## Clarifications

### Session 2026-10-15 — defaults approved by the owner

Each of these changes a decision already approved for 002 or 007, or the constitution. Each default
is written into the requirements below; the owner approved all five as written on 2026-10-15, and the
constitution was amended to 1.2.0 in the same change.

- Q1: Where does postmortem content live? → **Default: in a knowledge store separate from the graph,
  owned by the agent.** The graph keeps 007's thin `KNOWLEDGE_DOC` node (title, kind, location, dates)
  and its `CONCERNS` edges, unchanged. The store keeps each document's indexed sections, one version per
  page edit. This requires a **constitution amendment**: "The graph is the RAG index" becomes "The graph
  scopes and dates the RAG index; content lives in a knowledge store, and every document in it is
  linked to the graph nodes it concerns." 002 FR-049a's "content MUST NOT be stored" is amended to
  apply to the graph only. (FR-001–FR-004)
- Q2: What is indexed? → **Default: the whole page, split by section.** Each named text property
  becomes a labelled section ("Signals / Symptoms", "Root cause", "Fix / Mitigation", ...), and the page
  body becomes sections at its headings. People properties, comments and attachments are still never
  read; user mentions and email addresses are removed before anything is stored. (FR-010–FR-015)
- Q3: How are results ranked? → **Default: hybrid.** A lexical score and a semantic score are fused,
  then documents linked to an entity in the investigation's subgraph receive a boost. A document about
  another entity is still eligible. (FR-020–FR-025)
- Q4: What is the query? → **Default: the live incident's symptoms.** The investigator searches with
  free text it chooses (alert text, error messages, the hypothesis under test); the engine also runs one
  search at investigation start from the triggering alert, so a matching postmortem is in context before
  the first hypothesis. (FR-030–FR-032)
- Q5: Which model computes the semantic score? → **Default: any embedding provider the operator
  chooses, behind one published interface, declared in the versioned model configuration like every
  other model role.** The project is open source, so no dependency is imposed: a hosted API, an
  OpenAI-compatible endpoint, or a self-hosted model on the operator's own hardware are all first-class.
  The shipped configuration uses the production model provider (Mistral today) as an example, not a
  requirement. Without a configured provider, retrieval runs lexical only and says so in the evidence
  chain; it never fails an investigation. (FR-026–FR-029)
- Q6 (added by the owner on 2026-10-15): Must the knowledge store be pluggable too? → **Yes.** The
  knowledge store is an interface with a default that needs no external service (embedded, on the
  agent's own disk), and operators may plug in an external or self-hosted search or vector store
  instead. Every implementation passes the same conformance suite. (FR-005–FR-007)

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An investigation recalls the postmortem whose symptoms match (Priority: P1)

Screenings fail to start in the US. The agent's logs show "Error getting context" with timeouts. Three
weeks earlier the same symptom was a sequential scan of a 17 GB table past the agent's 10-second timeout,
and the postmortem says so in its "Signals / Symptoms" and "Root cause" fields, deep past the first 600
characters. The investigation opens and, before its first hypothesis, cites that postmortem's "Root
cause" section, with the page's title, age, location, the section it matched, and the service link. The
hypothesis it suggests is knowledge-derived until a source-of-truth worker confirms it.

**Why this priority**: it is the outcome the owner asked for, and the one 007 cannot deliver on the real
database.

**Independent Test**: replay the recorded LDMA database over an incident fixture whose alert carries one
postmortem's symptom phrased differently from the page; assert that postmortem is cited, with the
matching section, in the top three results.

**Acceptance Scenarios**:

1. **Given** a postmortem whose matching text is only in its "Signals / Symptoms" section, **When** the
   investigation searches with the incident's symptoms, **Then** the postmortem is returned and the cited
   section is "Signals / Symptoms".
2. **Given** a symptom worded differently from the postmortem ("LLM calls 404" against "upstream returned
   not found"), **Then** the postmortem is still in the top three results when semantic scoring is
   available.
3. **Given** a hypothesis that rests only on a retrieved postmortem, **Then** it is reported
   knowledge-derived and unconfirmed (002 FR-051, unchanged).
4. **Given** a section that reads as an instruction to the engine, **Then** it is data: scope, budgets,
   workers, posture and output are unchanged and the attempt is recorded (002 FR-017).

---

### User Story 2 - The same failure on another service is still recalled (Priority: P1)

The candidate frontend's LLM calls start failing with 404s. The postmortem that explains it, a vLLM
router discovering zero engines, is filed under *Infra*, which is not in the investigation's subgraph.
The investigation still retrieves it, ranked below any equally good match linked to the investigated
service, and the citation says the document is linked to *Infra*, not to the investigated entity.

**Why this priority**: cross-service recall is the main gain over 007's hard filter.

**Independent Test**: two postmortems with equally matching symptoms, one linked inside the subgraph and
one outside; assert both are returned, the linked one ranks first, and the other's citation names the
entity it is actually linked to.

**Acceptance Scenarios**:

1. **Given** a matching postmortem linked only outside the subgraph, **Then** it is returned and its
   citation states it is linked outside the investigation's scope.
2. **Given** two equally matching postmortems, one linked inside the subgraph, **Then** the linked one
   ranks higher.

---

### User Story 3 - A replay never cites what the agent could not have known (Priority: P1)

An investigation of a September incident is replayed in October. The postmortem of that very incident
was written afterwards, and another postmortem was edited after the incident to add a new root cause.
The replay cites neither the later postmortem nor the later edit: it retrieves exactly what the original
run could have retrieved.

**Why this priority**: as-of correctness is what makes evaluation honest; without it every replayed
incident cites its own postmortem and the scores mean nothing.

**Independent Test**: index a postmortem, edit one section, then replay an investigation pinned between
the two versions; assert the earlier section text is the one matched and cited, and that a page created
after the pinned instant is never returned.

**Acceptance Scenarios**:

1. **Given** a page created after the investigation's pinned instant, **Then** it is never returned.
2. **Given** a section edited after the pinned instant, **Then** the version known at the pinned instant is
   matched and cited.
3. **Given** a page trashed or moved out of its database, **Then** it is retrievable as of instants before
   that and not after, as 007 already does for its links.
4. **Given** the same investigation replayed twice, **Then** the retrieved set and its order are identical.

---

### User Story 4 - Retrieval quality is measured (Priority: P2)

A maintainer changes the chunking, the fusion weights or the embedding model. The evaluation reports, per
fixture, whether the expected postmortem was retrieved in the top three and at which rank, and the change
is accepted only if the aggregate does not regress.

**Why this priority**: the owner wants a state-of-the-art agent; that claim needs a number that CI keeps.

**Independent Test**: run the retrieval evaluation on the fixture set before and after a deliberate
regression (for example, dropping the body); assert the report shows the drop and the gate fails.

**Acceptance Scenarios**:

1. **Given** the retrieval fixture set, **When** the evaluation runs, **Then** it reports recall at three
   and mean reciprocal rank, per fixture and in aggregate, for hybrid and for lexical-only scoring.
2. **Given** a change that lowers aggregate recall at three below the published threshold, **Then** CI
   fails.

---

### User Story 5 - The operator sees what was indexed (Priority: P3)

The operator runs 007's `--check`. It now also reports, per page, the sections that would be indexed and
their sizes, and each section skipped with its reason, without printing any section's text.

**Independent Test**: `--check` on the LDMA recording lists each page's section names and sizes and no
section text.

**Acceptance Scenarios**:

1. **Given** a page with an empty body and three filled properties, **Then** `--check` lists three sections.
2. **Given** a section emptied by sanitisation, **Then** it is listed as skipped with its reason.

### Edge Cases

- A page with no mapped entity: it is not indexed (constitution: every document is linked), and `--check`
  reports it as 007 does.
- A very long page: sections are split further at a published size so that one section never dominates
  ranking; each part is cited with its section name.
- The embedding provider fails mid-run or rate-limits: that search falls back to lexical only, the
  evidence chain says so, and the investigation continues.
- The embedding model is changed: the index is rebuilt for the new model before it serves; results from
  two models are never fused in one search.
- A section contains a secret or a token: sanitisation redacts recognised secret shapes before storing;
  a section that is only a secret is skipped.
- Past investigations (002 FR-052) and hand-registered documents: indexed on the same terms, from what they
  already carry (an investigation's summary and conclusion; a registered document's summary).
- The knowledge store is unavailable: the knowledge worker answers "unavailable", never an empty result
  that reads as "no matching postmortem".

## Requirements *(mandatory)*

### A. Where knowledge lives

- **FR-001**: Document content MUST be stored in a knowledge store separate from the graph. The graph MUST
  NOT gain any field or event that carries section text.
- **FR-002**: Every document in the knowledge store MUST correspond to a `KNOWLEDGE_DOC` node with at least
  one `CONCERNS` edge; a document with no edge MUST NOT be indexed.
- **FR-003**: The knowledge store MUST keep every version of every section with the instant it became
  known, so retrieval can be answered as of any instant.
- **FR-004**: The constitution's Scope & Non-Goals entry on RAG (amended to 1.2.0 on 2026-10-15) and 002
  FR-049/FR-049a MUST be read as Q1 states. RAG over telemetry stays prohibited.
- **FR-005**: The knowledge store MUST be reached only through a published interface, so an operator can
  replace the default with an external or self-hosted store by configuration, without changing the agent.
- **FR-006**: The default knowledge store MUST need no external service and no network access.
- **FR-007**: A published conformance suite MUST cover every store behaviour this feature relies on
  (versioning, as-of reads, lexical and semantic search, rebuild), and every shipped implementation MUST
  pass it.

### B. What is indexed

- **FR-010**: The Notion feeder MUST read each configured page's named text properties and its body, and
  MUST NOT read people properties, comments or attachments.
- **FR-011**: Each named property MUST become one section labelled with the property name; the body MUST
  become sections at its headings, labelled with the heading text.
- **FR-012**: Sections longer than a published size MUST be split into parts that each keep the section
  label.
- **FR-013**: User mentions, email addresses and recognised secret shapes MUST be removed before a section
  is stored, indexed, recorded or logged.
- **FR-014**: The operator MUST name which properties are indexed, as 007 does for summary properties, now
  without a count limit; the body MUST be indexed unless the operator turns it off per database.
- **FR-015**: Past investigations and hand-registered documents MUST be indexed from the text they already
  carry, so one search covers all durable knowledge.

### C. Ranking

- **FR-020**: Each search MUST score sections both lexically and semantically and fuse the two into one
  ranking by a published, versioned method.
- **FR-021**: A section of a document linked to an entity in the investigation's subgraph or to its
  candidate changes MUST receive a published boost; documents linked elsewhere MUST remain eligible.
- **FR-022**: Results MUST be returned per document, carrying its best-matching sections, so one long
  postmortem cannot fill the result list.
- **FR-023**: Retrieval MUST only consider document and section versions known at the investigation's
  observed instant and valid at its valid instant.
- **FR-024**: Ranking MUST be deterministic for a given index, query, model configuration and pinned
  instant.
- **FR-025**: Every citation MUST carry the document's identifier, title, location, age, the matched
  section's label, the entities the document is linked to with each link's provenance, and whether those
  entities are inside the investigation's scope (extends 002 FR-050).
- **FR-026**: The embedding model MUST be declared in the versioned model configuration and recorded with
  every investigation (002 FR-061).
- **FR-027**: With no embedding provider configured, without its credential, or when the embedding call
  fails, retrieval MUST run lexical only and record that in the evidence chain.
- **FR-028**: Changing the embedding model MUST rebuild the semantic index before it serves; one search
  MUST never mix scores from two models.
- **FR-029**: The embedding provider MUST be reached only through a published interface; the project MUST
  ship at least a hosted-API adapter and an adapter for a self-hosted or OpenAI-compatible endpoint, and
  no provider MUST be required for the agent to run.

### D. Queries

- **FR-030**: `knowledge_search` MUST accept free-text query terms chosen by the investigator, in addition to
  the entity scope it already carries.
- **FR-031**: At investigation start the engine MUST run one search built from the triggering alert's text
  and the affected entities, and place its results in the investigator's context as knowledge-derived.
- **FR-032**: Query text MUST be recorded in the evidence chain with the results it returned.

### E. Operator, recording and evaluation

- **FR-040**: `feed notion --check` MUST report, per page, the sections that would be indexed with their
  sizes, and each skipped section with its reason, and MUST NOT print section text.
- **FR-041**: Recordings MUST include sanitised section text so a replay can rebuild the index; real
  workspace recordings stay private as 007 FR-050 requires.
- **FR-042**: A retrieval evaluation MUST report recall at three and mean reciprocal rank per fixture and in
  aggregate, for hybrid and lexical-only scoring, and CI MUST fail when aggregate recall at three falls
  below the published threshold.
- **FR-043**: The evaluation set MUST include at least one cross-service case (User Story 2) and one as-of
  case (User Story 3).

### Key Entities

- **Knowledge document**: 002/007's `KNOWLEDGE_DOC` node, unchanged in the graph: identity, kind, title,
  location, dates, `CONCERNS` edges.
- **Section**: a labelled piece of a document's text (a named property or a body heading), sanitised, with
  the instant it became known and, when replaced, the instant it stopped being current.
- **Search**: a query text, an entity scope, a pinned instant and a model configuration version; returns
  ranked documents with their matched sections.
- **Retrieval fixture**: an incident's alert text and pinned instant, the expected document, and whether
  it is linked inside the incident's scope.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the retrieval fixture set, the expected postmortem is in the top three results for at least
  90 % of fixtures with semantic scoring, and hybrid scoring is never below lexical-only in aggregate.
- **SC-002**: On the owner's seven LDMA postmortems, a symptom taken from a section past the first 600
  characters retrieves its postmortem in the top three for every page where such a section exists; 007's
  retrieval does so for none.
- **SC-003**: In replays, documents and section versions known only after the pinned instant are cited in
  0 % of runs.
- **SC-004**: A cross-service postmortem with the best-matching symptoms is returned in the top three, and
  an equally matching in-scope postmortem always ranks above it.
- **SC-005**: No graph event, log line or report contains section text; no stored section, recording or log
  contains a person's name or email or a planted canary secret.
- **SC-008**: Swapping the embedding provider or the knowledge store for another shipped implementation
  needs only a configuration change, and the conformance suite and retrieval evaluation pass on each.
- **SC-006**: With no embedding provider configured, every investigation still completes, and its evidence chain
  states that retrieval was lexical only.
- **SC-007**: A page edited in Notion is retrievable with its new text within 20 minutes at the default
  poll interval, at p95.

## Out of scope for this feature

- RAG over telemetry (constitution, unchanged).
- Other document sources (Slack threads, Google Docs, Confluence); the section model admits them later.
- A model-based re-ranker or query rewriting; candidates for a later feature once the evaluation exists.
- Turning a postmortem's timeline into graph events, or inferring entities from its text.
- Any write to Notion.

## Assumptions

- Features 001–007 are merged; 007's feeder, configuration, `--check` and recording format are extended,
  not replaced. 007's owner-gated T030 (record a real workspace) is done after this feature, so the
  recording carries sections.
- The agent is open source and self-hostable: no requirement in this feature may depend on a specific
  vendor, and every external dependency it adds is optional and replaceable (owner, 2026-10-15).
- Tens to low thousands of documents per deployment; no distributed index is needed.
- The owner's "SRE Agent Post Mortem" database in the LDMA workspace is the first real corpus; its seven
  postmortems seed the retrieval fixture set, sanitised and kept private.
