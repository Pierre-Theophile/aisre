---

description: "Task list for feature 009 — service catalog and signal attribution"
---

# Tasks: Service Catalog and Signal Attribution

**Input**: Design documents from `/specs/009-service-catalog/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[data-model.md](./data-model.md), [contracts/catalog.md](./contracts/catalog.md),
[quickstart.md](./quickstart.md)

**Tests**: **Required, not optional.** Constitution VIII (no fixtures, no merge), the precision and
calibration gate of FR-061, and replay byte-identity for every new event (constitution III).

**Organization**: grouped by the plan's three pull requests. PR 1 delivers US1 and US3 on a declared
catalog; PR 2 adds discovery and the probable rules (US2, US4); PR 3 the classifier (US5).

**Status**: spec, plan and these tasks approved by the owner on 2026-10-19 (all three PRs); in implementation.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US5]`. Setup, Foundational and Polish tasks carry no story label.
- Every task names the fixture it validates against; `unit` means a Go test in the package named.

## Path conventions

Per [plan.md §Project Structure](./plan.md#project-structure): the catalog logic in `internal/catalog/`,
projector handlers in `internal/projector/`, reads in `internal/query/catalog.go`, the classifier
interface in `pkg/attribution/`, fixtures in `fixtures/attribution/`.

---

# PR 1 — catalog, aliases, corrections, scope

## Phase 1: Setup

**Purpose**: publish the shared names before any code depends on them.

- [x] T001 Write `docs/decisions/ADR-0015-shared-contract-additions-for-009.md`: events 30–38, `EdgeType.ATTRIBUTED_TO`, `graph.v1.CatalogService`, the `sre.catalog.*`, `sre.attribution.*` and `sre.notion.values` props, the reserved namespace and its reason code, the `attribution:` configuration block, the ruleset `attribution/rules@1`, and the 002 FR-002b amendment, with the alternatives rejected in research R1, R2, R4 and R8 (research R12); fixture: unit (`scripts/check-specs.sh`)
- [x] T002 Add contracts §1–§3 to `api/sreagent/graph/v1/graph.proto` and regenerate with `buf generate`; `buf lint` and `buf breaking` against `main` pass (research R12); fixture: unit (CI schema job)
- [x] T003 Amend 002 FR-002b in `specs/002-investigation-engine/spec.md` as research R7 states, citing this feature, as 008 amended FR-049; fixture: unit (`scripts/check-specs.sh`)

**Checkpoint**: ADR-0015 and the schema merged in this PR before any dependent code.

---

## Phase 2: Foundational

**Purpose**: the tables, the projector handlers and the reads. Blocks every story.

- [x] T004 Write `internal/store/postgres/migrations/0015_catalog.sql`: `graph.catalog_aliases`, `graph.alias_proposals`, `graph.attribution_decisions`, bitemporal with `btree_gist` range indexes as `0012_correlation_keys.sql` does (data-model §Alias, §Attribution); fixture: unit (migration test)
- [x] T005 Write `internal/projector/catalog_entry.go`: `ProposeCatalogEntry` and `DeclareCatalogEntry` as `service` nodes in `sre.catalog`, status transitions of data-model §Catalog entry, refusal without a principal, and refusal of any other event naming `sre.catalog` with `catalog_namespace_reserved` (FR-001–FR-004, research R1); fixture: unit, idempotency and double delivery
- [x] T006 Write `internal/projector/catalog_alias.go`: `DeclareAlias` and `RetractAlias` into `graph.catalog_aliases`, reassignment closes the previous row, hidden names as the reserved entry `sre.catalog=-` (FR-003, FR-015, FR-050, data-model §Alias, §Hidden name); fixture: unit
- [x] T007 Write `internal/projector/attribution.go`: `AttributeSignal` into `ATTRIBUTED_TO` edges and decision rows, superseding decisions closing observed intervals, `CorrectAttribution` (signal- and name-level, the latter also an alias row) and `RevertCorrection` (FR-031, FR-041–FR-045, research R8); fixture: unit
- [x] T008 Write `internal/query/catalog.go` and `internal/server/catalog.go`: `ListEntries`, `GetEntry`, `WhyAttributed`, `AttributedSignals` as of both instants, authenticated (FR-005, FR-039, FR-052); fixture: unit
- [x] T009 Write `internal/catalog/names.go`: the per-kind name extractors of research R3 for alerts, declarations, knowledge documents and changes, read as of the signal's observed instant; fixture: unit against `fixtures/datadog-apm-topology-01` and `fixtures/slack-intake-01`
- [x] T010 [P] Write `sre.notion.values` on every `KNOWLEDGE_DOC` from `internal/feeders/notion/feeder.go`, with or without a legacy `entities` map, and regenerate the Notion twins' goldens (G3, research R3); fixture: notion-knowledge-twin-01, notion-knowledge-twin-02

**Checkpoint**: entries, aliases and decisions project and replay; nothing attributes yet.

---

## Phase 3: User Story 1 — a postmortem filed under its human name is boosted (Priority: P1)

**Goal**: layers 0 and 1, scope and the knowledge boost, on a declared catalog.

**Independent Test**: an alert on `candidate-api` and two equally matching postmortems under *Candidate View* and *Infra*; the first ranks first and is cited in scope (spec US1).

- [x] T011 [US1] Write `internal/catalog/rules.go` with layers 0 (`H`) and 1 (`A1`) of research R5, environment matching of research R2, and the ruleset id; fixture: unit
- [x] T012 [US1] Write `internal/catalog/attributor.go`: the in-process consumer of research R4, emitting `AttributeSignal` under `sre.attributor` through the runner's append path, re-attributing current signals when an alias, correction or entry status changes, never running during replay (FR-020, FR-030, FR-036); wire it into `internal/cli/serve.go`; fixture: unit
- [x] T013 [US1] Add attributed entries as subjects with provenance `attribution` in `internal/investigation/intake/alert.go` and the runner (FR-021, research R7); fixture: unit
- [x] T014 [US1] Treat `ATTRIBUTED_TO` from a document as a link in `internal/query/knowledge.go`, and compute the in-scope flag through entries and their aliases in `internal/investigation/workers/knowledge/knowledge.go` (FR-022, research R7); 008's goldens must not change; fixture: knowledge-scope-01, retrieval/*
- [x] T015 [US1] Write `internal/catalog/importfile.go` and `aisre catalog import <catalog.yaml>`: the difference against the catalog as of now as human decisions, idempotent, conflicts listed and never undone (FR-013, research R10, contracts §5); fixture: unit
- [x] T016 [US1] Write `fixtures/attribution/catalog-alias-01/`: a Datadog alert on `candidate-api`, a declared catalog, two postmortems with equally matching symptoms under *Candidate View* and *Infra*; labels; an incident manifest asserting the *Candidate View* postmortem is cited `in_scope: true` at rank 1 (SC-001); verified by `aisre fixture verify`; fixture: catalog-alias-01
- [x] T017 [US1] Write `fixtures/attribution/catalog-alias-gcp-01/`: the same incident from a GCP Cloud Monitoring policy on the Cloud Run service, with no Datadog source, and the same labels (SC-006); fixture: catalog-alias-gcp-01

**Checkpoint**: the owner's case works with a declared catalog.

---

## Phase 4: User Story 3 — a user corrects a wrong attribution, once (Priority: P1)

**Goal**: corrections from the command line and the API.

**Independent Test**: an automated attribution, a correction, a re-attribution and a full replay; the correction wins in both, the earlier pinned investigation is unchanged (spec US3).

- [x] T018 [US3] Write `internal/cli/catalog.go` and `internal/cli/attribute.go`: every command of contracts §4 except `proposals`, `alias accept|reject` and the legacy imports (PR 2), each printing the event appended and the principal (FR-012, FR-040); fixture: unit (`internal/cli/testdata`)
- [x] T019 [US3] Assert in `internal/catalog/attributor_test.go` that a correction outranks every layer, survives re-attribution, that a name-level correction attributes the next signal by `A1`, and that a later disagreeing automated decision is recorded as a conflict (FR-042, FR-043, SC-005); fixture: unit
- [x] T020 [US3] Write `fixtures/attribution/correction-01/`: an automated attribution, a signal-level and a name-level correction, an investigation pinned before them and one after, and a revert; verified by `aisre fixture verify`, asserting the pinned investigation replays unchanged and every correction carries its principal (FR-044, FR-045, SC-004); fixture: correction-01

**Checkpoint**: PR 1 complete; merge when green.

---

# PR 2 — discovery, probable rules, `unknown`, evaluation

## Phase 5: User Story 2 — the agent proposes the catalog (Priority: P1)

**Goal**: proposals from what connectors already see.

**Independent Test**: an empty catalog over a topology fixture and the Notion twin; the proposed entries, a scored alias suggestion, and accepting it attributes both names (spec US2).

- [x] T021 [US2] Write `internal/catalog/discovery.go`: `ProposeCatalogEntry` for each `SERVICE` entity with no aliased claim, after 001's certain rules, and for unaliased Notion values and Slack slugs; raised once, not re-raised while pending; hidden names never proposed (FR-010, FR-014, FR-015, research R9); fixture: unit
- [x] T022 [US2] Add `ProposeAlias` and `RejectAlias` handling to `internal/projector/catalog_alias.go`, a rejected pair staying rejected and a later proposal recorded as a conflict (FR-011, research R9); fixture: unit
- [x] T023 [US2] Add `ListProposals` to `CatalogService` and `aisre catalog proposals`, `alias accept`, `alias reject` (FR-010, FR-012); fixture: unit (`internal/cli/testdata`)
- [x] T024 [US2] Write `fixtures/attribution/discovery-01/`: `candidate-api` from Datadog and Cloud Run in production, *Candidate View* and *Other* from the Notion twin, an empty catalog; asserting one proposed entry per 001 entity, a `P2` suggestion with score, *Other* hidden by a decision in the stream, and both names attributed after acceptance (SC-008); fixture: discovery-01

**Checkpoint**: a fresh install has a usable, unconfirmed catalog.

---

## Phase 6: User Story 4 — the agent says `unknown` rather than guess (Priority: P2)

**Goal**: probable rules, threshold, margin, and the evaluation that keeps them honest.

**Independent Test**: a signal with only weak evidence is `unknown` with ranked suggestions and no edge (spec US4).

- [x] T025 [US4] Add rules `P1`–`P4`, noisy-OR within the layer, τ, δ and the suggestions of research R5 to `internal/catalog/rules.go`, reading τ and δ from the `attribution:` configuration block (FR-030–FR-032, FR-038); fixture: unit with hand-computed scores
- [x] T026 [US4] Write `fixtures/attribution/ambiguous-01/`: an uptime check on an IP, a monitor whose name matches two entries within δ, and a monitor matched only by topology; labels expecting `unknown`, `unknown` and the topology entry; fixture: ambiguous-01
- [x] T027 [US4] Write `internal/eval/attribution/` and `aisre eval attribution <dir>...`: precision, coverage and ten-bin calibration per layer and in aggregate, reusing the `fixture_calibration` report; exit 4 under the gate (FR-061, research R13); fixture: attribution/*
- [x] T028 [US4] Fit the starting scores of `P1`–`P4` on the labelled set, publish them as `attribution/rules@1` in `internal/catalog/rules.go` and ADR-0015, and add the gate (precision ≥ 0.95, every populated bin within 10 points) to `ci.yml`'s test job with a planted regression that lowers τ and fails (FR-033, FR-062, SC-002, SC-003); fixture: attribution/*
- [x] T029 [US4] Add a correction to each labelled case and include them in the calibration set (FR-046); fixture: attribution/*

**Checkpoint**: SC-002 and SC-003 gated in CI.

---

## Phase 7: Legacy maps

**Purpose**: 007's and 006's maps become catalog aliases (FR-006, research R11).

- [x] T030 Add `--from-notion-config` and `--from-slack-flags` to `aisre catalog import`, one alias per line of the old map, with `--dry-run`; fixture: unit against `fixtures/incidents/knowledge-notion-01` and `fixtures/slack-intake-01` configurations
- [x] T031 [P] Mark the Notion `entities` map and Slack's `--service` flags deprecated in `docs/connectors/notion.md` and `docs/connectors/slack.md`, with the import command, and make `entities` optional in `internal/feeders/notion/config.go` (FR-006); fixture: notion-knowledge-twin-01, notion-knowledge-twin-02

**Checkpoint**: PR 2 complete; merge when green.

---

# PR 3 — the classifier

## Phase 8: User Story 5 — a postmortem filed under *Other* is attributed from its content (Priority: P3)

**Goal**: the optional learned layer.

**Independent Test**: the Notion twin with one page under *Other*: with recorded classifier outputs, the expected entry and score; without a classifier, `unknown` and the `--check` report (spec US5).

- [ ] T032 [US5] Write `pkg/attribution/classifier.go` (contracts §7) and `pkg/attribution/classifiertest/` (determinism on recorded input, scores in [0, 1], no entry outside the profiles, deadline honoured) (FR-034); fixture: unit
- [ ] T033 [US5] Write `internal/catalog/classify/embedding.go`: the classifier of research R6 over any `pkg/knowledge.Embedder`, entry profiles from the catalog and attributed document titles, logistic calibration from configuration; passes `classifiertest` (FR-034, FR-037); fixture: unit
- [ ] T034 [US5] Wire layer 3 into the attributor: the `attribution.classifier` block of the model configuration, the 2-second timeout, fallback to layers 0–2 recorded in the evidence on error, timeout or missing credential, and recorded outputs by input digest for fixtures (FR-035, FR-036, SC-007); fixture: unit
- [ ] T035 [US5] Write `fixtures/attribution/classifier-01/` with recorded classifier outputs, fit the calibration parameters on it and the other labelled cases, and add layer 3 to the CI gate (FR-033); fixture: classifier-01
- [ ] T036 [US5] Report each observed name with its entry, or `unmapped` or `hidden`, in `aisre feed notion --check` and `aisre feed slack --check`, without printing signal text (FR-060); fixture: unit (`internal/cli/testdata`)

**Checkpoint**: PR 3 code complete.

---

## Phase 9: Polish

- [ ] T037 [P] Write `docs/catalog/README.md`: the catalog, aliases versus merges, the decision model and its ruleset, corrections, the classifier and how to plug another one, the evaluation; link it from `README.md`; fixture: unit (`scripts/check-specs.sh` link check)
- [ ] T038 [P] Add OpenTelemetry spans and metrics to the attributor (decisions by layer, `unknown` rate, classifier latency and fallbacks) per the constitution's self-observability gate; fixture: unit
- [ ] T039 Run [quickstart.md](./quickstart.md) steps 1–6 and record the run as `specs/009-service-catalog/quickstart-run-<date>.md`; fixture: attribution/*

**Checkpoint**: PR 3 complete; merge when green.

---

# Owner-gated

- [ ] T040 Build the LDMA catalog with the owner from `aisre catalog proposals` over the private LDMA recording and the recorded Datadog topology, record it in the private repository beside `campaign-2026-10-15/notion-ldma-01`, and label its signals; needs the owner's real service names; fixture: private LDMA recording
- [ ] T041 Measure SC-001 and SC-008 on the private set: every postmortem filed under a real service linked to an entry, 008's in-scope boost firing on every retrieval fixture whose alert and postmortem concern the same service, and the catalog reached by reviewing proposals only; fixture: private LDMA recording

---

## Dependencies & execution order

- **PR 1**: T001 → T002 → T004; T003 independent; T004 → T005, T006, T007 → T008; T009, T010 after T002; US1 (T011–T017) after T005–T009; US3 (T018–T020) after T012.
- **PR 2**: US2 (T021–T024) after PR 1; US4 (T025–T029) after T021; T030–T031 after T015.
- **PR 3**: T032 → T033 → T034 → T035; T036 after T021; Polish (T037–T039) after T035.
- **Owner-gated** after PR 3 merges.

```text
T001─►T002─►T004─►T005,T006,T007─►T008 ─┐
T003 ; T009,T010 (after T002) ──────────┴─► US1(T011–T017) ─► US3(T018–T020)          [PR 1]
US2(T021–T024) ─► US4(T025–T029) ; T030,T031                                         [PR 2]
T032─►T033─►T034─►T035 ; T036 ─► Polish T037–T039                                    [PR 3]
owner-gated T040─►T041
```

## Parallel opportunities

- T005, T006 and T007 once T004 lands; T009 and T010 alongside them.
- T016 and T017 once T014 lands.
- T026 alongside T025; T030 and T031 alongside Phase 6.
- T037 and T038.

## Implementation strategy

1. **PR 1 — catalog, aliases, corrections, scope** (T001–T020): the owner's case works as soon as the
   catalog is declared, and wrong attributions can be corrected.
2. **PR 2 — discovery, rules, evaluation** (T021–T031): the catalog fills itself, weak evidence is scored
   and calibrated, and the legacy maps migrate.
3. **PR 3 — the classifier** (T032–T039): documents and free text attributed from their content, optional.
4. **Owner-gated** (T040–T041): the real LDMA catalog and SC-001, SC-008.
