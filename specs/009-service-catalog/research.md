# Research: Service Catalog and Signal Attribution

Decisions taken while planning feature 009. Each is reversible and records what was rejected. Every
statement about existing code names the file it was read from on `main` at `6312515c`.

## R1. A catalog entry is a `SERVICE` node in the agent's own namespace

**Decision**: an entry is a node of type `service` whose ref is `sre.catalog=<slug>`, where the slug is
the entry's stable identifier (lowercase, `[a-z0-9-]`, minted from the first display name and never
changed by a rename). Its props carry `sre.catalog.kind` (`service` | `platform`), `sre.catalog.status`
(`proposed` | `confirmed` | `hidden`), `sre.catalog.owner` and `sre.catalog.description`. Entries are
written only by two new events, `DeclareCatalogEntry` (a person, principal required) and
`ProposeCatalogEntry` (the attributor, R4). The projector refuses an `UpsertNode`, `IdentityClaim` or
edge from any other source that names the `sre.catalog` namespace, so no connector can write the
catalog by accident (FR-004).

Merging two entries and splitting one reuse 001's `ManualMerge` and `SplitEntity` unchanged: an entry
is an entity like any other, and 001 already records, audits and reverses those decisions.

**Rationale**: an entry has to be a graph entity so that edges can point at it (R8), the subgraph and
the knowledge worker can reach it, and the as-of machinery answers "what was the catalog at T"
(FR-002) for free.

**Rejected**: a catalog table outside the graph (constitution I; it would not replay); reusing
`UpsertNode` with a human source (it has no principal and no status semantics, and would let any
feeder registered under that source rewrite the catalog).

## R2. An alias is a name, not a merge

**Decision**: an alias is `(namespace, value, environment?) → entry`, recorded by `DeclareAlias` and
ended by `RetractAlias`, projected into a bitemporal `graph.catalog_aliases` table (valid and observed
intervals, the deciding event, the principal or certain rule). A signal names an entry when one of its
names (R3) matches an alias: namespace and value exactly, and the environment when both state one. An
alias without an environment matches every environment.

Aliases do **not** merge the entry with the entities that carry the name.

**Rationale**: the spec's Q6 says an alias is identity, and FR-050 restricts who may create one to a
certain rule, a declared file or a person. The obvious implementation, a 001 merge between the entry
and the Datadog or OpenTelemetry entity, is wrong for one reason: environments. `candidate-api` in
production and in staging are two entities (001, 005 rule C9), and both are *Candidate View*. Merging
both into the entry would merge them into each other, and every diff, subgraph and blast radius of a
production investigation would start carrying staging changes. A name-to-entry table keeps the catalog
environment-agnostic while the graph's entities stay environment-specific.

**Rejected**: (a) merges, for the reason above; (b) an `IdentityClaim` on the entry, because 001's
certain rule C1 merges any two entities carrying the same claim, which is (a) again by another path;
(c) one entry per environment, which doubles every alias and asks the operator to curate the same
service three times.

## R3. What a signal's names are, per kind

**Decision**: the attributor reads each signal's names from what the graph already holds about it,
through one extractor per kind. Only the Notion feeder needs a change.

| signal | node | names | text (classifier only) |
|---|---|---|---|
| alert or policy transition | `ALERT` (005, 003) | the refs it `WATCHES`, its unattached watch refs, and every identity claim of each watched entity | title, message |
| declared incident | `ALERT` with origin `declared` (006) | the targets 006's rule parsed, and the slug tokens as `slack.slug=<token>` | title |
| knowledge document | `KNOWLEDGE_DOC` (007) | each value of the entity property as `notion.value=<database>/<value>` | indexed sections (008) |
| change | `CHANGE` (001, 004) | its targets and each target's identity claims | summary |

The Notion feeder writes the raw values on the node as `sre.notion.values` (a list of strings), always,
whether or not a legacy `entities` map is configured (R11). Values were previously only used to pick
refs and then dropped (`internal/feeders/notion/feeder.go:236`).

**Rationale**: a signal's names already exist in the graph for every kind except Notion values. Reading
them there, rather than adding a "names" field to every event, keeps every feeder unchanged and keeps
replay byte-identical for all recorded fixtures.

## R4. The attributor is an in-process consumer that emits events

**Decision**: a component in `serve` (`internal/catalog/attributor`) observes projected events of the
four signal kinds and every catalog, alias or correction event. For each signal it evaluates the layers
(R5) against the graph as of the signal's observed instant and appends an `AttributeSignal` event under
the source `sre.attributor`, through the same in-process path the investigation runner uses for its
decision record (`investigationstore.EmitDecisionRecord`). The projector turns that event into
`ATTRIBUTED_TO` edges (R8).

Replay never runs the attributor: the log already holds its decisions, so a replay from empty projects
the same edges (constitution III). Re-attribution happens when an alias, correction or entry status
changes: every signal still current (an alert not recovered, a document not trashed, a change inside
its look-back window) whose decision came from a lower layer than the change, or was `unknown`, is
re-evaluated, and a new `AttributeSignal` closes the old edge's observed interval and opens a new one.
History is never rewritten (constitution II).

**Rationale**: attribution must be an event to be replayable and auditable (constitution I, III, V),
and it must run after projection because the rules read the graph.

**Rejected**: attribution inside the projector (the projector would call rules and, later, a model:
non-deterministic replay); attribution inside each feeder (each connector would need the whole catalog,
and Datadog's feeder would again be the place services are decided).

## R5. The decision model: `attribution/rules@1`

**Decision**: four layers, evaluated in order; the first that decides wins, and the lower layers'
scores are still recorded as supporting evidence.

| layer | rule | fires when | score |
|---|---|---|---|
| 0 correction | `H` | a correction exists for this signal, or a name-level correction for one of its names | 1.0 |
| 1 alias | `A1` | a name matches an alias exactly (R2) | 1.0 |
| 2 probable | `P1` | a name equals an alias or an entry's display name after normalisation (case, separators, a trailing `-api`/`-svc`/`-service`/`-web`/`-worker`) | 0.85 |
| 2 probable | `P2` | name tokens overlap an entry's alias or display-name tokens, Jaccard ≥ 0.5 | 0.45 + 0.4 × J |
| 2 probable | `P3` | a watched or target entity has a valid `runs_on`, `depends_on` or `calls` edge to an entity one of whose claims is an alias of the entry | 0.55 |
| 2 probable | `P4` | a watched or target entity has an `owned_by` edge to the same owner as an entity aliased to the entry | 0.5 |
| 3 classifier | `M` | a classifier is configured (R6) | its calibrated score |

Within layer 2 the scores of different rules for the same entry combine by noisy-OR
(1 − Π(1 − sᵢ)), so two weak, independent signs outrank one. A signal is attributed automatically to
every entry at or above **τ = 0.80** that leads the next entry by at least **δ = 0.10**; otherwise the
layer answers `unknown` and the top three entries are listed as suggestions. Several entries may be
attributed when each reaches τ through a different name (a monitor grouped by service, a postmortem
filed under two services).

The rule scores above are starting values. Task T024 fits them on the labelled set (R13) and the
published values are whatever passes SC-002 and SC-003; the ruleset id changes (`@2`) whenever a score,
τ or δ changes, and every attribution records the ruleset it ran under (FR-038).

**Rationale**: the owner asked for a decision model. A layered model is the one that can honour every
other requirement at once: human decisions first (constitution VI), certainty before probability
(001 SC-007), an explicit `unknown` (constitution V), and an optional learned layer that never blocks.
Every layer is cheap and deterministic except the classifier, and the classifier's output is recorded.

**Rejected**: a single learned classifier over everything (no `unknown` floor, nothing explainable to an
on-call, and it would make a model mandatory); a weighted sum of rules (scores stop being probabilities,
so they cannot be calibrated); Datadog's or Backstage's service list as layer 1 (the owner's point).

## R6. The classifier is an interface; the shipped one reuses 008's embedder

**Decision**: `pkg/attribution.Classifier` with one method, `Classify(ctx, Signal, []EntryProfile)
([]Scored, error)`, where an entry profile is the entry's display name, description, aliases and the
titles of documents attributed to it. The shipped implementation, `embedding`, embeds the signal text and
each profile through any `pkg/knowledge.Embedder` (008), takes the cosine, and maps it to a probability
with a logistic calibration whose two parameters are fitted on the labelled set and published in the
model configuration. It is off unless the model configuration has an `attribution.classifier` block.
A classifier that errors, times out (2 s default) or lacks a credential leaves the decision to layers
0–2 and the evidence says so (FR-035).

For fixtures, classifier outputs are recorded by input digest beside 008's `vectors.jsonl`, so replays
and CI need no provider (FR-036).

**Rationale**: no new dependency: whoever configured an embedding provider for 008, hosted or
self-hosted, has a classifier. A language-model classifier can be plugged in later behind the same
interface.

**Rejected**: shipping a language-model classifier first (cost per signal, harder to calibrate, and it
would need prompt-injection hardening for every alert title); a bundled local model (a binary-size and
licensing cost for a layer that is optional).

## R7. Investigation scope and the knowledge boost

**Decision**: intake adds every entry the triggering signal is attributed to, as of the investigation's
observed instant, to the investigation's subjects with provenance `attribution` (layer, rule, ruleset,
score, decision event). `query.KnowledgeCandidates` (`internal/query/knowledge.go:72`) treats an
`ATTRIBUTED_TO` edge from a document exactly like a `CONCERNS` edge, and the knowledge worker's in-scope
flag (`internal/investigation/workers/knowledge/knowledge.go:393`) is true when a document is linked to
an entry in the subjects, or to an entity whose names alias such an entry. 008's ranking
(`knowledge/hybrid@1`, ×1.5) is unchanged.

002 FR-002b says the engine "does not infer a service from a title", and its parse rules are a
connector artifact. Attribution layers 2–3 can infer from a title. FR-002b is amended, as 008 amended
002 FR-049: a target may also come from an attribution at or above τ, whose rule id and ruleset are
recorded exactly as FR-002b already requires for a parse rule; below τ the engine still proceeds to
`unknown` with "name the affected service(s)".

**Rationale**: scope is the only thing attribution changes in an investigation; ranking, budgets and
the rest of 002 and 008 stay as approved.

## R8. `ATTRIBUTED_TO` is a new edge type

**Decision**: `EdgeType.ATTRIBUTED_TO` (signal → entry). Props: `sre.attribution.layer`, `.rule_id`,
`.ruleset`, `.score`, `.decision_event_id`, and `.principal` for a correction. Valid from the signal's
own valid start; observed from the decision. A superseding decision closes the observed interval.

**Rejected**: reusing `WATCHES` for alerts and `CONCERNS` for documents. `WATCHES` means what the
monitor observes, which is a fact about the monitor; an attribution is a judgement about it, and
conflating them would make a wrong attribution indistinguishable from a monitor's own configuration.

## R9. Discovery: proposals, never merges

**Decision**: when a `SERVICE` entity appears (after 001's certain rules have run) and none of its claims
is aliased, the attributor emits `ProposeCatalogEntry` with a slug from its first name and its claims as
the entry's aliases, status `proposed` (FR-010, FR-014). Notion values and Slack slugs with no alias
become proposed entries too. When a probable rule (P1, P2) relates two proposed entries, or a name to an
entry, it emits `ProposeAlias` with score, rule and evidence; `DeclareAlias` accepts it, `RejectAlias`
rejects it durably (a later identical proposal is recorded as a conflict, as 003's `RejectDependency`
does). Proposals are raised once and not re-raised while pending.

**Rationale**: the claims of one 001 entity are its names by 001's own certain rules, so carrying them
over is not a new inference. Anything probable stays a suggestion (FR-011, 001 SC-007).

## R10. The catalog file

**Decision**: `catalog.yaml`, versioned by the operator: entries (slug, display name, kind, owner,
description, hidden names) and aliases (namespace, value, environment). `aisre catalog import <file>`
computes the difference against the catalog as of now and emits the human decisions that close it,
signed by the importing principal (FR-013). Re-importing an unchanged file emits nothing. An import
never retracts a decision made after the file's last import by someone else: it lists the conflict and
leaves it (spec edge case).

## R11. Migrating 007's and 006's maps

**Decision**: both maps keep working. The Notion `entities` map and Slack's `--service` flags still
produce the edges and targets they produce today, and become optional. `aisre catalog import
--from-notion-config <file>` and `--from-slack-flags` translate them into entries and aliases
(`notion.value=<database>/<value>` and `slack.slug=<token>`), one alias per line of the old map. The
connector documents mark the maps deprecated; removing them is a later breaking change with its own
migration.

## R12. Schema additions, ADR-0015

**Decision**: one ADR publishes, before any code depends on them: events `ProposeCatalogEntry` (30),
`DeclareCatalogEntry` (31), `DeclareAlias` (32), `RetractAlias` (33), `ProposeAlias` (34), `RejectAlias`
(35), `AttributeSignal` (36), `CorrectAttribution` (37), `RevertCorrection` (38); `EdgeType.ATTRIBUTED_TO`;
`graph.v1.CatalogService` (`ListEntries`, `GetEntry`, `ListProposals`, `WhyAttributed`,
`AttributedSignals`); the `sre.catalog.*`, `sre.attribution.*` and `sre.notion.values` props; the
`attribution:` block of the model configuration; and the ruleset `attribution/rules@1`. All additive,
following ADR-0013 and ADR-0014.

## R13. Attribution evaluation

**Decision**: `fixtures/attribution/<case>/` holds a recorded event stream, a catalog file and
`labels.yaml` (signal ref → expected entries or `unknown`). `aisre eval attribution` replays each case,
runs the attributor, and reports per layer and in aggregate: precision of automatic attributions,
coverage, and calibration (ten bins, the existing `fixture_calibration` report). CI fails when aggregate
precision is below 0.95 (SC-002) or any populated bin's accuracy is more than 10 points from its stated
confidence (SC-003). Cases at minimum: `catalog-alias-01` (US1, Datadog), `catalog-alias-gcp-01` (the
vendor-free twin, SC-006), `discovery-01` (US2), `correction-01` (US3), `ambiguous-01` (US4) and
`classifier-01` (US5, recorded outputs). The LDMA case lives in the private repository, as 008's does.
