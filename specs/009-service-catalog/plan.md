# Implementation Plan: Service Catalog and Signal Attribution

**Branch**: `009-service-catalog` | **Date**: 2026-10-19 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/009-service-catalog/spec.md`, approved by the owner on
2026-10-19 with all six defaults (Q1–Q6), and constitution 1.2.0. Plan and tasks proposed for the
owner's approval.

## Summary

Three pieces, in dependency order:

1. **The catalog, aliases and corrections in the graph** (`internal/catalog`, the projector, a new
   `CatalogService`): catalog entries are `SERVICE` nodes in the agent's own `sre.catalog` namespace;
   aliases map a source's names to an entry without merging entities; an attributor inside `serve`
   attributes every alert, declaration, knowledge document and change by human correction or exact
   alias, and emits its decisions as events; the investigation's scope and 008's in-scope boost read
   them. This alone fixes the owner's case once the catalog is declared.
2. **Discovery and the probable rules** (US2, US4): the attributor proposes entries and alias
   suggestions from what connectors already see, scores weaker evidence with published, calibrated
   rules, answers `unknown` below a threshold, and an attribution evaluation gates precision and
   calibration in CI. The legacy Notion and Slack maps migrate into the catalog.
3. **The optional classifier** (US5): a `pkg/attribution.Classifier` interface with one shipped
   implementation built on 008's embedder, off by default, recorded for replay.

**The approach is decisions-as-events.** Every catalog change, alias, attribution and correction is a
typed event in the log, so the catalog is replayable and answerable as of any instant, every decision
is auditable, and the attributor never runs during a replay (R4). Nothing about a service is owned by
a connector or a vendor.

## Phase 0 — gaps found against the code

| # | gap | where | resolution |
|---|---|---|---|
| G1 | No connector-neutral notion of a service: Notion maps values to refs in its own config, Slack in its own flags, alerts carry whatever the monitoring tool calls a service | `internal/feeders/notion/config.go:55`, `internal/cli/feed_slack.go:576`, `internal/feeders/datadog/monitors.go:264` | catalog entries in `sre.catalog` (R1) with aliases (R2); the old maps become imports (R11) |
| G2 | The only way to say two names are one thing is a 001 merge, which would merge production and staging entities through a shared entry | `internal/projector/confirm_merge.go` | aliases are names, not merges (R2) |
| G3 | Notion's Service values are used to pick refs and then dropped; nothing downstream can attribute a page by its value | `internal/feeders/notion/feeder.go:236` | write them as `sre.notion.values` on the node (R3) |
| G4 | No event or edge records a judgement that a signal is about a service | `api/sreagent/graph/v1/graph.proto:217` | events 30–38 and `ATTRIBUTED_TO` (R8, R12) |
| G5 | Nothing in `serve` reacts to projected events by emitting new ones, except the investigation runner | `internal/investigation/runner/persist.go:294` | the attributor, on the same in-process append path (R4) |
| G6 | The investigation's subjects are only what the alert watches; 002 FR-002b forbids inferring a service from a title | `internal/investigation/intake/alert.go:115`, `runner/investigate.go:538`, 002 spec FR-002b | subjects gain attributed entries with provenance; FR-002b amended as 008 amended FR-049 (R7) |
| G7 | The knowledge worker's in-scope flag compares `CONCERNS` targets with the requested entity ids only | `internal/query/knowledge.go:72`, `workers/knowledge/knowledge.go:393` | `ATTRIBUTED_TO` counts as a link, and scope expands through entries and their aliases (R7) |

## Technical Context

**Language/Version**: Go 1.27 (floor; the repository tracks the latest stable toolchain)

**Primary Dependencies**: `internal/projector` (new handlers beside `confirm_merge.go` and
`propose_dependency.go`); `internal/resolution` (claims read by the extractors; no rule changes);
`internal/query` (catalog reads, knowledge candidates); `internal/investigation/intake` and `runner`
(subjects); `internal/feeders/notion` (values prop); `internal/cli` (catalog and attribute commands);
`internal/eval` and `fixture_calibration.go` (evaluation); `pkg/knowledge.Embedder` (classifier). **No
new Go module.**

**Storage**: PostgreSQL 16, the agent's own database: migration `0015_catalog.sql` adds
`graph.catalog_aliases` (bitemporal, `btree_gist` range index, as `0012_correlation_keys.sql`),
`graph.alias_proposals` and `graph.attribution_decisions` (the audit side of `AttributeSignal` and
`CorrectAttribution`, a projection of the log like every graph table).

**Testing**: `go test ./...`; projector tests per event including idempotency and refusal without a
principal; replay byte-identity on every new fixture (constitution III); `aisre fixture verify`; the
attribution evaluation over `fixtures/attribution/*` (R13); `pkg/attribution/classifiertest`;
`golangci-lint`; `scripts/check-specs.sh`.

**Target Platform**: Linux server, the same single binary: the attributor inside `serve`, the catalog
commands in `aisre`.

**Project Type**: new events and an edge type, an in-process event consumer, a query service, CLI
commands, and small changes to an existing feeder, intake and worker, inside the single-module Go
project.

**Performance Goals**: an attribution at layers 0–2 reads at most the signal's names, the aliases
matching them, and one hop of edges: under 20 ms p95 at the reference workload (10k nodes); it runs off
the ingest path, so ingestion latency is unchanged. Re-attribution after an alias change touches only
current signals naming that alias. The classifier adds one embedding call per signal, bounded by its
2-second timeout.

**Constraints**: no automated alias from a probable rule (FR-011, 001 SC-007); `unknown` below τ or
within δ (FR-032); no model or network required (FR-034); replay never calls the attributor or a
classifier (FR-036); the classifier sees signal text only (FR-037).

**Scale/Scope**: tens to a few hundred entries, thousands of aliases, attribution per signal at
ingestion.

## Constitution Check

*GATE: passes before Phase 0; re-checked after design (no change).* Against constitution 1.2.0.

| principle | verdict | how |
|---|---|---|
| I. Graph First | pass | Entries are nodes, attributions are edges, aliases and decisions are projections of typed events; the investigation reads scope from the graph (R1, R4, R8) |
| II. Bitemporal | pass | Entries, aliases and attribution edges carry valid and observed intervals; corrections and re-attributions close intervals, never rewrite (R2, R4) |
| III. Event-sourced | pass | Nine typed, idempotent events; the attributor's outputs are in the log, so replay from empty reproduces the graph without running it (R4) |
| IV. Telemetry stays home | pass | Signals are alerts, declarations, documents and changes already in the graph; the classifier reads titles and sections, never telemetry (FR-037) |
| V. Evidence-first | pass | Each attribution records layer, rule, ruleset, score, rationale, evidence and suggestions; `unknown` is first class; calibration is reported and gated in CI (R5, R13) |
| VI. Resolution auditable | pass | Aliases only from a person, a declared file or an entity's own certain claims; probable relations are suggestions; human corrections outrank every rule and persist across replay; `WhyAttributed` is the audit query (R2, R9) |
| VII. Read-only by default | pass | No new connector, no new external write; the classifier sends signal text only to an embedding provider the operator chose, and none is required |
| VIII. Evaluation | pass | Six labelled attribution fixtures, a vendor-free twin, a CI gate on precision and calibration (R13) |
| IX. Open schema | pass | ADR-0015 publishes the events, the edge type, the query service, the props, the configuration block and the ruleset before code depends on them (R12) |
| X. Simplicity | pass, see Complexity Tracking | No new module or service; the classifier reuses 008's embedder |
| Scope: RAG only over linked durable knowledge | pass | A document attributed to an entry is linked to it; a document linked to nothing is still not indexed (008 FR-002) |

## Project Structure

### Documentation (this feature)

```text
specs/009-service-catalog/
├── spec.md
├── plan.md              # this file
├── research.md          # R1–R13
├── data-model.md        # entry, alias, hidden name, attribution, correction, proposals, labels
├── contracts/
│   └── catalog.md       # events, edge type, CatalogService, CLI, catalog.yaml, config, Classifier
├── quickstart.md        # validation run
├── checklists/
│   └── requirements.md
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
docs/decisions/ADR-0015-shared-contract-additions-for-009.md
docs/catalog/README.md                         # operator guide: catalog, aliases, corrections, model

api/sreagent/graph/v1/graph.proto              # events 30–38, ATTRIBUTED_TO, CatalogService

pkg/attribution/                               # public: what a third-party classifier implements
├── classifier.go                              # Signal, EntryProfile, Scored, Classifier
└── classifiertest/                            # conformance suite

internal/
├── store/postgres/migrations/0015_catalog.sql
├── projector/
│   ├── catalog_entry.go                       # Propose/DeclareCatalogEntry, namespace reservation
│   ├── catalog_alias.go                       # Declare/Retract/Propose/RejectAlias
│   └── attribution.go                         # AttributeSignal, Correct/RevertCorrection → edges
├── catalog/
│   ├── names.go                               # per-kind name extractors (R3)
│   ├── rules.go                               # attribution/rules@1 (R5)
│   ├── attributor.go                          # the in-process consumer (R4)
│   ├── discovery.go                           # proposals (R9)
│   ├── importfile.go                          # catalog.yaml and the legacy maps (R10, R11)
│   └── classify/embedding.go                  # the shipped classifier (R6)
├── query/catalog.go                           # CatalogService reads
├── query/knowledge.go                         # ATTRIBUTED_TO as a link
├── server/catalog.go                          # CatalogService handlers
├── investigation/intake/alert.go              # attributed entries as subjects
├── investigation/workers/knowledge/knowledge.go  # scope through entries and aliases
├── feeders/notion/feeder.go                   # sre.notion.values
├── eval/attribution/                          # precision, coverage, calibration
└── cli/catalog.go, cli/attribute.go, cli/eval_attribution.go

fixtures/attribution/
├── catalog-alias-01/        catalog-alias-gcp-01/   discovery-01/
└── correction-01/           ambiguous-01/           classifier-01/
```

**Structure Decision**: the catalog's logic lives in a new `internal/catalog` package because it is
neither a connector nor resolution: it reads what both produce and decides something different. The
only public interface is the classifier, which is the extension point the owner's pluggability rule
asks for; the rules are the project's own published ruleset, not a plug-in.

## Delivery

Three pull requests, each green and merged on its own, as 007 and 008 were built:

1. **PR 1 — catalog, aliases, corrections, scope (US1, US3).** ADR-0015; proto and migration 0015;
   projector handlers; `CatalogService`; the attributor with layers 0 and 1; `sre.notion.values`;
   subjects and the knowledge scope; `catalog` and `attribute` commands; `catalog.yaml` import;
   fixtures `catalog-alias-01`, `catalog-alias-gcp-01`, `correction-01`.
2. **PR 2 — discovery, probable rules, `unknown`, evaluation (US2, US4).** Proposals; rules P1–P4 with
   threshold and margin; the attribution evaluation and its CI gate; the legacy-map imports and the
   connector docs; fixtures `discovery-01`, `ambiguous-01`.
3. **PR 3 — the classifier (US5).** `pkg/attribution` and its conformance suite; the embedding
   classifier with recorded outputs and fallback; `--check` reporting names per entry (FR-060); the
   operator guide; fixture `classifier-01`.

Then one owner-gated step: build the LDMA catalog with the owner from the proposals, record it in the
private repository, and measure SC-001 and SC-008 on it.

## Design decisions (detail in research.md)

- **R1 — Entries are `SERVICE` nodes in `sre.catalog`**, written only by two catalog events; merge and
  split reuse 001.
- **R2 — An alias is a name, not a merge**, so production and staging entities never collapse.
- **R3 — Names per signal kind** come from what the graph holds; only Notion gains a prop.
- **R4 — The attributor is an in-process consumer that emits events**; replay never runs it.
- **R5 — `attribution/rules@1`**: correction, alias, probable (P1–P4, noisy-OR), classifier;
  τ = 0.80, δ = 0.10, starting scores fitted on fixtures.
- **R6 — `pkg/attribution.Classifier`**, shipped on 008's embedder, off by default, recorded.
- **R7 — Scope**: attributed entries become subjects; `ATTRIBUTED_TO` is a link for knowledge; 002
  FR-002b amended.
- **R8 — `ATTRIBUTED_TO`**, distinct from `WATCHES` and `CONCERNS`.
- **R9 — Discovery proposes, never merges.**
- **R10 — `catalog.yaml`** imported as human decisions, idempotent, conflict-preserving.
- **R11 — Legacy maps keep working** and import into the catalog.
- **R12 — ADR-0015** before code.
- **R13 — Attribution evaluation** with a CI gate on precision (0.95) and calibration (10 points).

## Complexity Tracking

| item | why it is needed | simpler alternative rejected because |
|---|---|---|
| A name-to-entry alias table beside 001's identity claims | environments must stay distinct entities (R2) | 001 merges would merge production and staging through the entry |
| An in-process consumer that emits events | attributions must be events (constitution I, III) and depend on graph state | attributing in the projector would make replay call rules and models; attributing in feeders would make each connector own the catalog |
| A new edge type | an attribution is a judgement, not a monitor's configuration or a document's own link (R8) | reusing `WATCHES` or `CONCERNS` would hide wrong attributions among source facts |
| One interface in `pkg/` | the owner's pluggability rule | a built-in model would make a provider mandatory or the layer absent |
