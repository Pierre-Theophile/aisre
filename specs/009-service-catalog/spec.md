# Feature Specification: Service Catalog and Signal Attribution

**Feature Branch**: `009-service-catalog`

**Created**: 2026-10-19

**Status**: Draft, defaults proposed for the owner's approval

**Input**: User description: "I don't think Datadog should be the source of truth (a customer may not
use Datadog at all). We should have our own internal mapping and automatically identify which service
a signal belongs to. The user must be able to correct it if the automatic classification is wrong.
Maybe a decision model à la Jev can help?" (owner, 2026-10-19, translated from French)

## Why this feature exists

Feature 008 boosts a postmortem by ×1.5 when it is linked to an entity inside the investigation's
scope. On the owner's real data that boost never fires:

- The investigation's scope comes from what the alert watches: a Datadog service name such as
  `candidate-api`.
- The postmortem's link comes from 007's operator map, which turns a Notion *Service* value
  (*Candidate View*, *Infra*, *Other*) into a graph entity reference. Those references are still
  placeholders, and nothing says that *Candidate View* and `candidate-api` are the same service.
- 006's Slack declarations have a third map of their own (`--service <token>=<ns>=<value>`).

Each connector carries its own private idea of what a service is, and the obvious fix (copy Datadog's
service list everywhere) makes Datadog the source of truth, which an open-source agent cannot do: a
customer may run Grafana, GCP Cloud Monitoring, Prometheus or nothing but Slack.

What this feature adds, in priority order:

1. **A service catalog owned by the agent**: the list of services the operator's organisation
   actually talks about, each with every name it goes by in every source. No vendor's list is
   authoritative; every connector's view of a service is an alias of a catalog entry.
2. **Automatic attribution of every signal to a catalog service** (an alert, a declared incident, a
   postmortem, a change), by a published, layered decision model whose every decision carries its
   rule, confidence and rationale, and which answers `unknown` rather than guess.
3. **Correction by the user**: anyone authenticated can say "this signal is about service X", or
   "this name means service X", and the correction wins over every automated decision from then on,
   across replays, and teaches the model for the next signal.

## Clarifications

### Defaults proposed on 2026-10-19, for the owner's approval

Each default is written into the requirements below. The owner approves or changes them on the spec
pull request, as for 006, 007 and 008.

- Q1: What is the source of truth for services? → **Default: the agent's own catalog, held in the
  graph as events like every other fact.** A catalog entry is a canonical `SERVICE` entity with a
  stable identifier, a display name, a kind (`service` or `platform`, the latter for shared
  infrastructure such as *Infra*), an optional owner and description, and its aliases. External
  catalogs and connectors (Datadog, OpenTelemetry `service.name`, Kubernetes, Cloud Run, Backstage,
  Notion values, Slack slugs) only **propose** entries and aliases; none is authoritative.
  (FR-001–FR-006)
- Q2: How is the catalog filled? → **Default: discovered, then curated.** The agent proposes entries
  from what its connectors already observe and lists them for review; the operator confirms, renames,
  merges, splits or hides them. An operator may also declare the catalog in a versioned file (for
  GitOps), which is imported as human decisions. A proposed entry is usable for attribution at once,
  marked unconfirmed, so a fresh install is not blocked on review. (FR-010–FR-015)
- Q3: Which signals are attributed? → **Default: every signal that can start or inform an
  investigation**: alerts and policy transitions from any monitoring connector, declared incidents
  (Slack), knowledge documents (Notion postmortems and runbooks), and changes (deploys, flag flips).
  Telemetry itself is never attributed or stored (constitution IV). (FR-020–FR-022)
- Q4: How is a signal attributed (the "decision model")? → **Default: a layered, deterministic model
  first, with an optional learned layer behind a published interface.** In order of precedence:
  1. a **human correction** for that signal or that name;
  2. an **alias** match on a structured identifier the signal carries (a monitor's service tag, a
     span's `service.name`, a Notion select value, a Slack slug), certain because someone declared it;
  3. **probable rules** on weaker evidence (name similarity, shared namespace, repository or owner, the
     signal's position in the graph's topology), each with a published, calibrated score;
  4. an optional **model-based classifier** (embeddings or a language model over the signal's own
     text, such as a postmortem's symptoms or a Slack message), off by default, which the operator
     plugs in like 008's embedding provider.
  A signal is attributed automatically only at or above a published confidence threshold; below it
  the answer is `unknown` with ranked suggestions. The owner's "à la Jev" reference is not yet
  understood and may change this default. (FR-030–FR-039)
- Q5: How does a user correct an attribution, and what does a correction change? → **Default: from
  the command line and the API, authenticated, recorded as a human decision (001 FR-040, FR-041).**
  A correction names either one signal ("this alert is about *Candidate View*") or a name ("the
  Datadog service `candidate-api` is *Candidate View*"); a name-level correction becomes an alias and
  applies to every later signal carrying that name. A correction applies from the instant it is
  recorded: replays pinned before it are unchanged, and review mode sees it. (FR-040–FR-046)
- Q6: How does this relate to 001's entity resolution? → **Default: aliases are identity, attribution
  is not.** An alias says two names denote one service; it is created only by a certain rule, a
  declared catalog file or a human decision, exactly as 001 requires for merges (no automated merge
  from a probable rule, 001 SC-007). An attribution says a signal is *about* a service; it may be
  automatic at a calibrated confidence because it only ranks and scopes, it never merges entities.
  Both are reversible, audited and overridden by humans. No constitution amendment is needed.
  (FR-030, FR-050–FR-052)

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A postmortem filed under its human name is boosted for the alert's service (Priority: P1)

A Datadog monitor on `candidate-api` fires. The postmortem that explains the failure is filed in Notion
under *Candidate View*. The operator's catalog has one entry, *Candidate View*, with the aliases
`candidate-api` (Datadog service) and *Candidate View* (Notion Service value). The alert is attributed
to *Candidate View* by alias, the investigation's scope includes it, and 008's in-scope boost ranks the
postmortem first. Its citation says the document is linked inside the investigation's scope, through
the catalog entry.

**Why this priority**: it is the outcome that motivated the feature; without it 008's boost never fires
on real data.

**Independent Test**: replay a fixture with a Datadog alert on `candidate-api` and two postmortems with
equally matching symptoms, one filed under *Candidate View* and one under *Infra*; assert the *Candidate
View* postmortem ranks first and is cited as in scope.

**Acceptance Scenarios**:

1. **Given** a catalog entry with aliases in two sources, **When** a signal from either source names it,
   **Then** the signal is attributed to that entry with the alias as its rule.
2. **Given** a postmortem and an alert attributed to the same entry, **Then** the postmortem is in the
   investigation's scope and receives 008's boost.
3. **Given** the same fixture without Datadog (a GCP Cloud Monitoring policy on the Cloud Run service
   `candidate-api`), **Then** the outcome is the same: no step depends on a specific vendor.

---

### User Story 2 - The agent proposes the catalog from what it already sees (Priority: P1)

A new operator connects Datadog, Cloud Run and Notion. Before any configuration, the agent lists the
services it proposes: `candidate-api` seen in Datadog and Cloud Run in production (one entry, by 001's
certain rules), *Candidate View* from Notion, and the probable match between them with its score and
rationale. The operator confirms the match, names the entry *Candidate View*, marks *Other* as "not a
service", and the catalog is ready.

**Why this priority**: a catalog nobody fills is the placeholder map again; discovery is what makes it
usable on day one.

**Independent Test**: replay the LDMA recording and a Datadog/Cloud Run topology fixture with an empty
catalog; assert the proposed entries, the suggested aliases with scores, and that confirming one
suggestion attributes every signal carrying either name.

**Acceptance Scenarios**:

1. **Given** names that 001's certain rules already resolve, **Then** they are proposed as one entry.
2. **Given** names that only a probable rule relates, **Then** they are proposed as separate entries with
   a suggestion to alias them, never merged automatically.
3. **Given** a declared catalog file, **Then** its entries and aliases are imported as human decisions and
   take precedence over discovery.

---

### User Story 3 - A user corrects a wrong attribution, once (Priority: P1)

A Slack incident channel `sev2-2026-10-19-candidates-cant-log-in` is attributed by name similarity to
*Candidate View*. It was the authentication service. The on-call runs one command saying this incident
is about *Auth*. The investigation's scope is corrected from that instant, the correction is recorded
with their identity, and a replay of the earlier, pinned investigation still shows what the agent knew
at the time. A free-text slug like this one is corrected for this signal only. When the on-call instead
says the Datadog service `auth-gw` is *Auth*, that is a name-level correction, and every later alert on
`auth-gw` is attributed to *Auth* without asking again.

**Why this priority**: the owner made correction a requirement; an attribution that cannot be fixed
will be distrusted and switched off.

**Independent Test**: attribute a signal automatically, record a correction, re-run resolution and a
full replay; assert the correction wins in both, the earlier pinned investigation is unchanged, and the
audit query names the person, the instant and the overridden decision.

**Acceptance Scenarios**:

1. **Given** an automated attribution, **When** a user corrects it, **Then** the correction takes
   precedence regardless of the automated score, persists across replays and re-attribution, and records
   the user's authenticated identity.
2. **Given** a name-level correction, **Then** it becomes an alias and the next signal carrying that name
   is attributed by alias.
3. **Given** a correction recorded after an investigation's pinned instant, **Then** replaying that
   investigation yields the original attribution; review mode yields the corrected one.
4. **Given** a later automated rule that disagrees with a correction, **Then** the correction stands and
   the disagreement is listed for review (001 FR-040).

---

### User Story 4 - The agent says `unknown` rather than guess (Priority: P2)

A GCP uptime check on an IP address fires. Nothing the signal carries names a catalog entry, and the
probable rules score below the threshold. The investigation starts with the target `unknown`, lists the
two best-scoring suggestions with their rationale, and proceeds as 002 FR-026 already requires for an
unresolved target. Once someone attributes it, later checks on that IP are attributed by alias.

**Why this priority**: a wrong service poisons scope and ranking; an honest `unknown` costs little.

**Independent Test**: a signal with only weak evidence; assert `unknown`, the ranked suggestions with
scores, and no attribution edge.

**Acceptance Scenarios**:

1. **Given** a best score below the threshold, **Then** the signal is unattributed and the suggestions
   are listed.
2. **Given** two entries scoring within a published margin of each other, **Then** the signal is
   unattributed, even above the threshold.

---

### User Story 5 - A postmortem filed under *Other* is attributed from its content (Priority: P3)

A postmortem's Service value is *Other*, which the operator marked "not a service". With a model-based
classifier configured, the agent reads the postmortem's indexed sections, proposes *Candidate View*
with a confidence above the threshold, and links the document. Without a classifier, the page is
reported by `--check` as unattributed, as 007 does today for unmapped values.

**Why this priority**: useful on real data, but it needs the optional learned layer.

**Independent Test**: the LDMA recording with one page set to *Other*; with the recorded classifier,
assert the expected entry and score; without it, assert `unknown` and the `--check` report.

**Acceptance Scenarios**:

1. **Given** no classifier configured, **Then** attribution uses rules only and the evidence says so.
2. **Given** a classifier that fails or times out, **Then** attribution falls back to rules and the
   signal is never blocked.

### Edge Cases

- **One signal, several services**: a monitor grouped by service, or a postmortem filed under two
  services, is attributed to each entry separately, each with its own decision.
- **Same name, different environments**: `checkout` in staging and in production are different
  entities (001, 005 C9); an alias carries the environment when the source states it.
- **An entry renamed**: the stable identifier does not change; attributions and aliases follow it.
- **Two entries merged or one split by the operator**: past attributions are re-pointed from the
  decision's instant, never rewritten (constitution II).
- **A name hidden as "not a service"** (*Other*, *misc*): signals carrying only that name are
  unattributed by rule; it is never proposed again.
- **A catalog file and a human correction disagree**: the most recent human decision wins, and the
  conflict is listed; a file re-import never silently undoes a correction.
- **A classifier's text contains an instruction to the engine**: it is data (002 FR-017).
- **Platform entries**: an alert on a shared database attributed to *Infra* puts *Infra* in scope; 008's
  boost applies to documents linked to *Infra* exactly as to a service.

## Requirements *(mandatory)*

### A. The catalog

- **FR-001**: The agent MUST hold a service catalog of its own: canonical entries with a stable
  identifier, display name, kind (`service` or `platform`), optional owner and description.
- **FR-002**: Catalog entries and every change to them MUST be graph events (constitution I, III), so
  the catalog is replayable and answerable as of any instant (constitution II).
- **FR-003**: Each entry MUST carry its aliases: the names it goes by in each source, each with its
  source, namespace, environment when stated, and the decision that created it.
- **FR-004**: No connector, vendor or external catalog MAY be required for the catalog to exist or to
  be authoritative; every external list is a proposer.
- **FR-005**: The catalog MUST be readable through the query API and the command line, as of any
  instant, with each entry's aliases and the provenance of each.
- **FR-006**: 007's Notion value map and 006's Slack service map MUST be replaced by catalog aliases;
  existing configurations MUST keep working, imported as declared aliases, with a published migration.

### B. Filling the catalog

- **FR-010**: The agent MUST propose entries from identities its connectors observe, grouped by 001's
  certain rules, and MUST list them for review with their sources.
- **FR-011**: Names that only a probable rule relates MUST be proposed as an alias suggestion with
  score and rationale, never aliased automatically (001 FR-033, SC-007).
- **FR-012**: The operator MUST be able to confirm, rename, merge, split, hide (mark "not a service")
  and restore entries, and to accept or reject alias suggestions.
- **FR-013**: The operator MUST be able to declare entries and aliases in a versioned file; an import
  MUST be recorded as human decisions attributed to the importing identity.
- **FR-014**: A proposed, unconfirmed entry MUST be usable for attribution and MUST be marked
  unconfirmed wherever it is shown or cited.
- **FR-015**: A hidden name MUST NOT be proposed again until restored.

### C. What is attributed

- **FR-020**: Every alert or policy transition, declared incident, knowledge document and change
  MUST be attributed to zero or more catalog entries when it enters the graph.
- **FR-021**: An investigation's scope MUST include the entries its triggering signal is attributed
  to, so 008's in-scope boost and 002's subgraph use them.
- **FR-022**: A knowledge document attributed to an entry MUST be linked to it, so it satisfies 008
  FR-002 without a per-connector map.

### D. The decision model

- **FR-030**: Attribution MUST apply, in order: human corrections, alias matches on structured
  identifiers, probable rules, and the optional classifier; the first layer that decides wins.
- **FR-031**: Every attribution MUST record the layer and rule, a numeric confidence, a human-readable
  rationale and the evidence it used (constitution V, VI).
- **FR-032**: Automatic attribution MUST happen only at or above a published threshold and only when the
  best entry leads the next by a published margin; otherwise the answer MUST be `unknown` with ranked
  suggestions.
- **FR-033**: Probable-rule and classifier confidences MUST be calibrated on a labelled fixture set and
  the calibration reported in CI (constitution V).
- **FR-034**: The classifier MUST be reached only through a published interface, declared in the
  versioned model configuration, off by default; the default deployment MUST need no network access
  and no model to attribute.
- **FR-035**: A classifier failure, timeout or missing credential MUST fall back to the rule layers and
  MUST be recorded; it MUST never block a signal or an investigation.
- **FR-036**: Attribution MUST be deterministic for a given graph state, catalog, configuration and
  recorded classifier output.
- **FR-037**: The classifier MUST see only the signal's own text (alert title and message, declaration
  text, a document's indexed sections), never telemetry (constitution IV).
- **FR-038**: Rules and thresholds MUST be versioned, and each attribution MUST record the version.
- **FR-039**: An audit query MUST answer, for any signal, "why is this attributed to this service?" and
  for any entry, "which signals are attributed to it, and how?".

### E. Correction

- **FR-040**: An authenticated user MUST be able to correct an attribution for one signal, or to declare
  that a name means an entry, from the command line and the API.
- **FR-041**: A correction MUST be recorded as a human decision with the user's authenticated identity,
  the instant, and the decision it overrides (001 FR-040, FR-041).
- **FR-042**: A correction MUST take precedence over every automated decision regardless of score, and
  MUST persist across replays and re-attribution.
- **FR-043**: A name-level correction MUST create an alias that applies to every later signal carrying
  that name; a signal-level correction MUST apply to that signal only.
- **FR-044**: A correction MUST apply from its recorded instant; investigations pinned before it MUST
  replay unchanged, and review mode MUST see it.
- **FR-045**: Corrections MUST be undoable, keeping their full history.
- **FR-046**: Corrections MUST be part of the labelled set the calibration of FR-033 is measured on.

### F. Relation to entity resolution

- **FR-050**: Aliases MUST be created only by a certain rule, a declared catalog file or a human
  decision.
- **FR-051**: Attributions MUST NOT merge entities; reversing an attribution MUST NOT change identity.
- **FR-052**: The audit query of 001 FR-039 MUST cover aliases as it covers merges.

### G. Operator and evaluation

- **FR-060**: `--check` on any connector MUST report, per observed name, the entry it maps to, or that it
  is unmapped or hidden, without printing signal text.
- **FR-061**: An attribution evaluation MUST report, on a labelled fixture set, precision of automatic
  attributions, coverage (share attributed rather than `unknown`) and calibration, per layer; CI MUST
  fail when precision falls below the published threshold.
- **FR-062**: The fixture set MUST include a vendor-free case (no Datadog), a correction case, an
  ambiguous case and the LDMA case of User Story 1.

### Key Entities

- **Catalog entry**: a canonical `SERVICE` entity owned by the agent: identifier, display name, kind,
  owner, description, confirmed or proposed, hidden or not.
- **Alias**: a name in a source (namespace, value, environment) that denotes a catalog entry, with the
  decision that created it.
- **Signal**: anything attributed: alert transition, declared incident, knowledge document, change.
- **Attribution**: a signal-to-entry decision with layer, rule, version, confidence, rationale,
  evidence and instants; automated or human.
- **Correction**: a human attribution or alias decision, with identity, instant and the overridden
  decision.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the LDMA recording with a catalog built by confirming the proposed suggestions, every
  postmortem filed under a real service is linked to a catalog entry, and 008's in-scope boost fires for
  every retrieval fixture whose alert and postmortem concern the same service.
- **SC-002**: On the labelled attribution set, automatic attributions are correct at least 95 % of the
  time; every other signal is `unknown`, never silently wrong beyond that rate.
- **SC-003**: Stated confidences are calibrated: across bins, the observed accuracy is within 10 points of
  the stated confidence.
- **SC-004**: 100 % of corrections survive a full replay and re-attribution, and 100 % carry an
  authenticated identity.
- **SC-005**: After a name-level correction, the next signal carrying that name is attributed by alias
  in 100 % of cases, without a further human action.
- **SC-006**: The vendor-free fixture (no Datadog connector) reaches the same attributions as its Datadog
  twin.
- **SC-007**: With no classifier configured, every signal is still attributed or `unknown`, every
  investigation completes, and no network call is made for attribution.
- **SC-008**: A new operator reaches a usable catalog (every alerting service attributed) by reviewing
  suggestions only, without writing configuration, on the LDMA and topology fixtures.

## Out of scope for this feature

- Correcting an attribution from Slack (a reaction or a reply on the report): Slack writes are confined
  to the report (constitution VII); reading a correction from Slack is a candidate for a later feature.
- A user interface beyond the command line and the API.
- Syncing the catalog back to an external catalog (Backstage, Datadog Service Catalog): no writes.
- Inferring service dependencies; topology stays with the feeders.
- Attributing raw telemetry (constitution IV).

## Assumptions

- Features 001–008 are merged; 001's resolution, human decisions and audit query are extended, not
  replaced, and 008's ranking is unchanged: this feature changes only which entities are in scope and
  which entities a document is linked to.
- The agent is open source and self-hostable: every dependency this feature adds is optional and
  pluggable, with a no-network default (owner, 2026-10-15).
- Tens to a few hundred services per deployment; attribution runs per signal at ingestion.
- The owner's LDMA workspace and the recorded Datadog topology are the first real cases; the real
  service names come from the owner during review of the proposed catalog.
