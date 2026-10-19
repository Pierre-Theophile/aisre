# ADR-0015: The shared contract additions for feature 009

- Status: **Accepted** (2026-10-19)
- Deciders: project owner (the 009 scope, its six defaults, the plan and the tasks, 2026-10-19),
  Claude (design, documented here)
- Source: `specs/009-service-catalog/plan.md` §"Phase 0 — gaps found against the code" (G1–G7),
  `research.md` R1–R13, `contracts/catalog.md`. Written under `specs/009-service-catalog/tasks.md`
  T001.

## Context

Until feature 009, what a "service" is depended on the connector that saw it. The Notion feeder
mapped its Service values to graph refs in its own configuration (007), Slack mapped channel-slug
tokens in its own flags (006), and an alert watched whatever its monitoring tool called a service
(003, 005). Nothing said that the Notion value *Candidate View* and the Datadog service
`candidate-api` are one service, so 008's in-scope boost never fired on the owner's data.

The owner's direction (2026-10-19): no vendor is the source of truth, because a customer may not run
that vendor at all. The agent keeps its own catalog, attributes every signal to it automatically,
and lets a person correct a wrong attribution. Each item below is read by more than one package or
by people outside the project, so it is decided here (constitution IX).

## Decision

### Item 1 — nine events, all additive to `sreagent.graph.v1.EventEnvelope`

| field | event | who may send it |
|---|---|---|
| 30 | `ProposeCatalogEntry` | the source `sre.attributor` |
| 31 | `DeclareCatalogEntry` | an authenticated person |
| 32 | `DeclareAlias` | an authenticated person |
| 33 | `RetractAlias` | an authenticated person |
| 34 | `ProposeAlias` | the source `sre.attributor` |
| 35 | `RejectAlias` | an authenticated person |
| 36 | `AttributeSignal` | the source `sre.attributor` |
| 37 | `CorrectAttribution` | an authenticated person |
| 38 | `RevertCorrection` | an authenticated person |

Shapes are those of `contracts/catalog.md` §1. A person's event without a principal is refused with
`missing_principal` (001 FR-041). An attributor event from any other source is refused with
`catalog_namespace_reserved`, as is any `UpsertNode`, `UpsertEdge`, `IdentityClaim` or
`CorrelateEntity` that names the `sre.catalog` namespace: no connector can write the catalog.

### Item 2 — a catalog entry is a `SERVICE` node in `sre.catalog`

An entry's ref is `sre.catalog=<slug>`, slug `[a-z0-9-]{1,63}`, minted once and never changed by a
rename. Its props are `sre.catalog.kind` (`service` | `platform`), `sre.catalog.status` (`proposed` |
`confirmed` | `hidden`), `sre.catalog.owner` and `sre.catalog.description`. Both entry events carry
the entry's whole state and fold under one lane, so the latest decision about an entry is its
current version, and history is kept as for every node. Merging and splitting entries reuse 001's
`ManualMerge` and `SplitEntity`.

### Item 3 — an alias is a name, not a merge

An alias maps `(namespace, value, environment?)` to an entry. It never merges the entry with the
entities that carry that name: `candidate-api` in production and in staging stay two entities (001,
005 C9), both aliased to *Candidate View*. Aliases are created only by a person, a declared catalog
file, a name-level correction, or a proposed entry carrying its own entity's claims (research R9);
never by a probable rule (001 SC-007). The reserved entry `sre.catalog=-` marks a hidden name
("not a service").

### Item 4 — `EdgeType.ATTRIBUTED_TO = 11`

Signal → entry, from an `AttributeSignal` or a `CorrectAttribution`. Props:
`sre.attribution.layer` (`correction` | `alias` | `probable` | `classifier`),
`sre.attribution.rule_id`, `sre.attribution.ruleset`, `sre.attribution.score`,
`sre.attribution.decision_event_id`, and `sre.attribution.principal` on a correction. A superseding
decision closes the edge's observed interval. It is not `WATCHES` (a monitor's own configuration) and
not `CONCERNS` (a document's own link): an attribution is a judgement about a signal, and a wrong one
must stay distinguishable from a source fact.

### Item 5 — the ruleset `attribution/rules@1`

Layers in order, the first that decides wins: 0 correction (`H`, 1.0), 1 exact alias (`A1`, 1.0),
2 probable rules (`P1`–`P4`, research R5, combined by noisy-OR per entry), 3 an optional classifier
(`M`). Automatic attribution at score ≥ τ = 0.80 and a lead of δ = 0.10 over the next entry;
otherwise `unknown` with the top three suggestions. A change to any rule score, τ or δ is a new
ruleset version, and every attribution records the version it ran under.

Published scores (feature 009 PR 2, `internal/catalog/rules.go`):

| rule | fires when | score |
|---|---|---|
| `P1` | a signal's name equals an entry's display name or an alias's name once normalised (lower case, separators, and deployment suffixes such as `api`, `svc`, `web` dropped) | 0.85 |
| `P2` | their token sets overlap with Jaccard J ≥ 0.5 | 0.45 + 0.4 J |
| `P3` | the signal's entity `runs_on`, `depends_on` or `calls` an entity aliased to the entry | 0.55 |
| `P4` | the signal's entity has the entry's owner | 0.50 |

Two entries reaching τ through the same name give `unknown`. An alias stated for one
environment is not a `P1`/`P2` target for a name in another. Discovery (research R9) proposes an
entry for a `SERVICE` entity no alias names and carries its identity claims as aliases with rule
`D1`; an alias a person declared outranks a discovered one, and a decision taken on discovered
aliases says so in its evidence (`alias_discovered`). The starting scores were confirmed, not moved,
on the labelled set: every attribution the probable layer took there is right, and every weaker
score stays a suggestion (Item 10).

### Item 6 — `graph.v1.CatalogService`

`ListEntries`, `GetEntry`, `WhyAttributed` and `AttributedSignals` (feature 009 PR 1), and
`ListProposals` (PR 2), each taking 001's `AsOf` and authenticated like every query.

### Item 7 — props written by connectors

`sre.notion.values` on every Notion `KNOWLEDGE_DOC`: the raw values of the entity property, and
`sre.notion.database`, the database the page was read from, so the attributor can name a page by
`notion.value=<database>/<value>`. Slack slug tokens are named
`slack.slug=<token>`.

### Item 8 — 002 FR-002b is amended

A declared incident's or an alert's target may also come from an attribution at or above τ, whose
rule id and ruleset are recorded exactly as FR-002b requires for a parse rule. Below τ the engine
still proceeds to `unknown` with "name the affected service(s)".

### Item 9 — the `attribution:` block of the model configuration

`ruleset`, `threshold`, `margin`, and an optional `classifier` (feature 009 PR 3): `kind`
(`embedding`, the one shipped), `timeout` (2 s by default) and `calibration` (`slope`, `intercept`).
Absent, the defaults of Item 5 apply and no classifier runs.

The classifier interface is `pkg/attribution.Classifier`, proven by `pkg/attribution/classifiertest`.
Layer 3 is asked only about a signal layers 0–2 left `unknown` with no hidden name, and decided on with
τ and δ; one text over τ for two entries stays `unknown`. An error, a timeout or a missing credential
leaves the decision to layers 0–2, with `classifier_fallback` in the evidence. The calibration is
per embedding model: for `mistral-embed` it is slope 72.4 and intercept −53.6, fitted on
`fixtures/attribution/classifier-01` (logistic regression, L2 penalty 0.001 on the slope).

### Item 10 — the attribution gate

`aisre eval attribution <dir>...` replays each labelled case in `fixtures/attribution`, runs the
attributor, and scores its own decisions against `labels.yaml`: precision, coverage and ten-bin
calibration per layer and in aggregate. A signal a person corrected is scored on the decision the
correction overruled, so corrections are part of the set (FR-046). CI's test job fails when
aggregate precision is below 0.95 (SC-002) or a populated bin is more than 10 points from its
observed accuracy (SC-003); a planted regression that lowers τ to 0.5 must fail it.

The aggregate is the attributor's own inference: discovered aliases, the probable layer and the
classifier. An attribution that follows an alias a person declared is reported and not gated: its
1.0 is a person's word, and when it is wrong the catalog is wrong, which a correction fixes (US3).

## Alternatives rejected

- **Aliases as 001 merges** (research R2): the entry would merge production and staging entities into
  each other, and every diff and subgraph of a production investigation would carry staging changes.
- **An identity claim on the entry** (research R2): 001's certain rule C1 would make the same merge.
- **A catalog table outside the graph** (research R1): it would not replay (constitution I, III).
- **Attributing inside the projector** (research R4): replay would call rules and, later, a model.
- **Attributing inside each feeder** (research R4): each connector would own the catalog again.
- **Reusing `WATCHES` or `CONCERNS`** (research R8): a wrong attribution would read as a source fact.
- **A vendor's service list as layer 1**: the owner's point; a customer may not run that vendor.

## Consequences

- Every connector's names for a service resolve to one entry, so 008's boost fires whichever tool
  raised the alert and wherever the postmortem was filed.
- 007's Notion `entities` map and 006's `--service` flags keep working and become imports into the
  catalog (feature 009 PR 2).
- Graph schema 1.0.0 gains events and an edge type, additively; `buf breaking` passes.
