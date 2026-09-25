---

description: "Task list for feature 002 — Investigation Engine"
---

# Tasks: Investigation Engine

**Input**: Design documents from `/specs/002-investigation-engine/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md,
`docs/decisions/ADR-0003…ADR-0005`, `.specify/memory/constitution.md`, and feature 001 shipped.

**Tests**: Required, as in 001. Constitution VIII makes fixtures and tests non-optional; every
task below names the fixture(s) it validates against, or `bench`, or `unit` when a fixture is not
the right instrument. A task with no test is not done.

**Organization**: the phase order is the reviewer's order of work, taken from plan.md §"What
tasks.md should start with". It is **not reordered; parallelised only where the dependency graph
permits** (see §Parallel Opportunities):

1. Phase 2 — the coverage audit and `π₀` (model-free; everything downstream cites it)
2. Phase 3 — the feature 001 change package (specified and implemented **in 001**)
3. Phase 4 — the query algebra, `pkg/backend`, the recorded backend, world recording
4. Phase 5 — the hypothesis ledger with computed confidence (model-free)
5. Phase 6 — the six fixtures Phase 6 itself consumes (T097, T102–T106, moved here by analyze O1),
   then the engine loop, the deterministic first wave, the budget manager (+ intake, verifier,
   renderer, human channel, CLI, RPCs, telemetry, which interleave)
6. Phase 7 — trajectory recording and trajectory replay as the CI gate, before any evaluation work
7. Phase 8 — the adversarial fixtures and the metamorphic generator, then the eval job with its
   published gate

Because that order cuts across user stories, **story labels are per task rather than per phase**
from Phase 2 onward. Phase 1, Phase 3 (which is 001's work) and Phase 9 carry no story label.

## Format: `[ID] [P?] [Story] Description → FR refs; fixture: <ids>`

- **[P]**: parallelizable (different files, no unmet dependency)
- **[USn]**: user story from spec.md (`US0`, `US1`, `US1a`, `US2`…`US10`)
- Fixture ids refer to directories under `fixtures/` and `fixtures/incidents/`
  (`contracts/incident-format.md`); `unit` and `bench` keep 001's meaning

## Fixture inventory

### Feature 001 fixtures (reused, plus the one new 001-owned fixture the change package adds)

No new recording; goldens are re-recorded only where Phase 3 says.

| id | kind | reused for |
|---|---|---|
| `rollout-regression-01` | hand-authored (001 T043) | parent of `rollout-regression-01-incident`; D1/D2 golden re-record |
| `rollout-regression-02` | recorded (001 T082) | D3 actor kind on real payloads; D1 golden re-record |
| `config-change-01` | recorded (001 T066) | D1/D3 golden re-record; config-change candidate class |
| `feeder-gap-01` | recorded (001 T067) | parent of `unknown-feeder-gap-01`; FR-032 gap flagging |
| `ambiguous-identity-01` | hand-authored (001 T077) | parent of `merged-alias-01`; FR-003 resolution audit as evidence |
| `baseline-topology-01` | recorded (001 T058/T063) | pointer join keys (D4), `WATCHES` edge |
| `announced-fact-01` | **hand-authored, new, 001-owned** (created by T031) | ADR-0005 D5: a fact whose `valid_at` is after its `observed_at`; proves the projector's segment planning tolerates it and pins the published query semantic. Lives under `fixtures/`, not `fixtures/incidents/` — it is a 001 fixture, not an incident |

### New incident fixtures (`fixtures/incidents/`, per `contracts/incident-format.md`)

| id | authored how | created in | exercises |
|---|---|---|---|
| `rollout-regression-01-incident` | **derived** from `rollout-regression-01`: directory, `world/` and `incident:` block in T097 (Phase 6); `trajectories/` recorded in Phase 7 from the first live run | T097 (+ T093) | US1, US2, US3; the MVP; first wave; L1 and L2 replay |
| `declared-incident-01` | **hand-authored** | T103 | US1a: human-declared intake, idempotency, a monitor attaching later (FR-001a, FR-002a/b, FR-004a, FR-008b/c) |
| `human-fact-reopen-01` | **hand-authored** | T104 | US9: a fact pushed against a concluded run, reopen, linked record (FR-057a/b, SC-024) |
| `unknown-feeder-gap-01` | **derived** from `feeder-gap-01` | T105 | US4: window overlaps a source gap → `unknown` with a resolving action (FR-032, SC-007) |
| `two-simultaneous-01` | **hand-authored** | T105 | FR-025: two plausible changes not separable by the evidence |
| `merged-alias-01` | **derived** from `ambiguous-identity-01` | T105 | FR-062: the alert names an alias of a merged entity |
| `telemetry-rejection-01` | **hand-authored** | T106 | SC-010: a decision record carrying a series is rejected `telemetry_payload` |
| `injection-01` | **hand-authored** | T106 | US6 scenario 4 / FR-017: retrieved content that reads as an instruction |
| `knowledge-scope-01` | **hand-authored** | T106 | US6 / SC-014: in-subgraph doc cited, out-of-subgraph doc never cited |
| `unobserved-latent-bug-01` | **hand-authored** | T102 | FR-071b, SC-023 — ground truth `unobserved`, category `latent_bug` |
| `unobserved-client-config-01` | **hand-authored** | T102 | FR-071b, SC-023 — `not_change_induced: client_side_configuration` |
| `unobserved-business-data-01` | **hand-authored** | T102 | FR-071b, SC-023 — `not_change_induced: business_data_change` |
| `unobserved-credential-leak-01` | **hand-authored** | T102 | FR-071b, SC-023 — `unobserved`, category `credential_leak` |
| `slow-burn-01` | **hand-authored** (adversarial) | T099 | FR-062b, SC-021 — culprit ≈ 3 h before the alert; prior fails |
| `distant-culprit-01` | **hand-authored** (adversarial) | T100 | FR-062b, SC-021 — culprit ≈ 3 hops away; prior fails |
| `adjacent-decoy-01` | **hand-authored** (adversarial) | T101 | FR-062b, SC-021 — decoy ≈ 2 min before, adjacent service; prior fails |

### Generated, not checked in

| id | kind | created by |
|---|---|---|
| `*-culprit-deleted`, `*-decoy-injected`, `*-time-shifted`, `*-name-permuted` | **metamorphic variants** | `aisre fixture derive` (T107) — a **generator**, not files; each variant records its parent and transformation, and is graded on the invariant the transform defines (FR-062a, SC-020) |

### Notes on provenance

- Every incident fixture declares `provenance.kind` ∈ `synthetic | recorded | derived` and the
  sanitiser policy version that produced it (FR-061b).
- **Real recordings stay private** (ADR-0003 D9). The public repository ships synthetic structural
  twins and runs the full gate on them; the recorded corpus lives in the private repository and
  runs the same job with the same thresholds. No recorded production fixture enters this
  repository, and `eval.yml` records which corpus it ran against (T113).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: the new published contract, the package skeletons, the schema, and the CI jobs that
later phases fill in. Nothing story-specific.

- [x] T001 Create the 002 tree per plan §Project Structure: `internal/investigation/{engine,model,ledger,workers/{graph,metrics,traces,logs,knowledge},backend,replay,budget,verify,intake,store,render,audit}`, `pkg/worker/`, `pkg/backend/`, `fixtures/incidents/`, `docs/schema/{algebra.md,digests.md,ledger.md}`, `docs/evaluation/investigation-metrics.md`, each with a `doc.go` or a stub heading → plan §Project Structure; fixture: n/a
- [x] T002 Copy `specs/002-investigation-engine/contracts/investigation.proto` to `api/sreagent/investigation/v1/investigation.proto`; extend `buf.gen.yaml` for the new package; `make gen`; commit generated Go and connect-go code → FR-068, FR-064; fixture: unit
- [x] T003 [P] Add `buf lint` + `buf breaking` coverage for `sreagent.investigation.v1` in `.github/workflows/ci.yml` and `buf.yaml` (new package starts at v1, breaking checked against `main` from the first merge) → FR-068, constitution IX; fixture: n/a
- [x] T004 [P] Write `internal/store/postgres/migrations/0006_investigation.sql` (**`0006`, not `0005`: the 001 change package's `0005_actor_kind.sql` in T020 lands first, and migrations are numbered in the order they are applied**): schema `investigation` with `incidents, investigations, symptoms, report_deliveries, hypotheses, judgments, evidence_items, worker_calls, model_calls, recordings, human_facts, human_reviews, labels, budget_spend, verifier_findings, coverage_audits, coverage_audit_items`, the check constraints of data-model §Invariants 1/2/4/7/10, and grants that exclude DDL for the service role; migration up/down test → data-model §Schema `investigation`; fixture: unit
- [x] T005 [P] Extend `scripts/check-migrations.sh` to refuse `DROP`/`DELETE`/`TRUNCATE` against `investigation.investigations`, `investigation.evidence_items`, `investigation.judgments` and `investigation.coverage_audits`, matching 001's log/graph rule → constitution III, FR-007; fixture: n/a
- [x] T006 [P] Skeleton `pkg/worker/`: `Worker`, `Description`, `Capability`, `RedactionPolicy`, `Mode`, `Request`, `Response` per `contracts/worker-sdk.md`, with registration-time validation stubs and `pkg/worker/testkit/` entry points → FR-010, FR-011; fixture: unit
- [x] T007 [P] Skeleton `pkg/backend/`: `TelemetryBackend` (`Describe`, `Execute`), `AlgebraRequest`/`AlgebraResponse` aliases over the generated types, `backend.Recorder` and `backend.Testkit` entry points per `contracts/telemetry-backend.md` §8 → FR-068; fixture: unit
- [x] T008 [P] Add `github.com/anthropics/anthropic-sdk-go` to `go.mod`; add `config/model.yaml` (investigator `claude-fable-5-1`, verifier `claude-opus-5`, logs labeller `claude-haiku-4-5`, `output_config.effort`, thinking display, beta set) and `config/prices.yaml` with a `version` field; a test asserting the config round-trips and its version is recorded → plan §Model configuration, FR-061, FR-048; fixture: unit
- [x] T009 [P] Add CI job stubs: a `trajectory-replay` job in `ci.yml` running `fixture verify … --trajectory-only` (no fixtures yet, exits 0 with "0 trajectories") **with a per-PR calibration step in the same job** — the reliability table over the five published buckets plus Brier and log-loss, computed from the **recorded trajectories** with **zero model calls** (confidences are ledger-computed and deterministic, so replay reproduces them exactly), written to the job summary and reporting "0 hypotheses" while there are no fixtures; and a new `.github/workflows/eval.yml` with the **eval gate placeholder** (`scripts/check-report.sh --investigation` invoked but the aggregate pass@1 threshold unset and loudly reported as `unset`, per FR-060) and a `corpus: [public, private]` matrix whose `private` leg is skipped when the private corpus is absent → FR-058, FR-060, SC-004, constitution §Replay CI (c), plan §Evaluation and CI; fixture: n/a

**Checkpoint**: `make gen lint test` green; `buf breaking` green on both packages; `0006` applies and rolls back; `eval.yml` runs and reports "no thresholds set" rather than passing silently.

---

## Phase 2: User Story 0 — Measure the ceiling before building on it (P1, prerequisite) 🎯 first increment

**Goal**: `aisre audit coverage` over a human-supplied incident list, machine-readable, two
runs comparable, the ceiling published, and `π₀ = 1 − ceiling` wired into the ledger rule. **No
model anywhere in this phase.**

**Independent Test**: `audit coverage --incidents <list> --out docs/evaluation/` reproduces the
published 2026-09 aggregate (13 classifiable, 0/3/8 by feeder set, ≈62 %) from the recorded input,
and `audit coverage compare` against a second run attributes the movement to the feeders added —
without running a single investigation.

- [x] T010 [US0] Implement `internal/investigation/audit/list.go`: the incident-list input format (incident id, alert instant, known cause free text, category, optional entity/change ref), YAML and JSON, strict parsing, and a refusal when the list supplies no cause — the audit **measures, it never infers** → FR-069; fixture: unit
- [x] T011 [US0] Implement `internal/investigation/audit/classify.go`: for each incident query the graph as-of the alert instant **with observed time pinned to it**, classify `cause_present` / `cause_absent` with a category from the published set (flag flip, IaC apply, DB migration, certificate expiry, scheduled job, third-party outage, traffic shift, latent bug, client-side configuration, business data change, credential leak, other) / `undecidable` with a reason → FR-069, FR-070; fixture: baseline-topology-01, rollout-regression-01 (as the graph under audit)
- [x] T012 [US0] Implement `internal/investigation/audit/report.go`: JSON + markdown output carrying the per-incident classification, the aggregate ceiling, **the incident count it rests on**, per-category counts, and the feeder set the graph was running; persist to `investigation.coverage_audits`/`coverage_audit_items` → FR-069, FR-069a, SC-022; fixture: unit
- [x] T013 [US0] Implement `internal/cli/audit_coverage.go`: `audit coverage --incidents <file> [--out docs/evaluation/] [--graph-config <name>]`, `--output table|json` per 001's conventions → FR-064, contracts/cli.md; fixture: unit
- [x] T014 [US0] Implement `audit coverage compare <a.json> <b.json>`: both runs readable, the ceiling comparable, the movement attributed to the feeders added, and **a ceiling that did not move reported as such rather than omitted** → FR-071, FR-071a; fixture: unit (two recorded audit outputs)
- [x] T015 [US0] Publish the first audit in the machine-readable shape: regenerate `docs/evaluation/coverage-audit-2026-09.md` from `audit coverage` over the recorded (private) list, keeping incident-level detail out of this repository, and check in the aggregate JSON beside it → FR-069a, SC-022; fixture: n/a. **Done as far as this repository can be taken:** `docs/evaluation/coverage-audit-2026-09.json` is checked in in the exact shape `audit coverage --aggregate-only` emits, marked `provenance: transcribed`, and every figure in it is asserted against the published Markdown by `internal/investigation/audit` (published_test.go). **Remaining, and it needs data that is not in this repository:** the owner transcribes the private list per `docs/evaluation/coverage-audit-format.md` and runs `aisre audit coverage --input <private> --out docs/evaluation/ --aggregate-only`, which overwrites both files with a computed result carrying the per-category counts and a real input digest.
- [x] T016 [US0] Implement `internal/investigation/ledger/prior.go`: `π₀ = 1 − ceiling` read from the latest published audit, recorded on every investigation together with the audit id it came from, so a historical confidence stays interpretable → ADR-0005 D9, FR-023, FR-019a; fixture: unit
- [x] T017 [US0] Extend `scripts/check-report.sh` with the **ceiling guard**, scoped exactly as FR-071 scopes it: check **only the `[ceiling-bounded]` criteria** — SC-001, SC-006, SC-021, and any published target for culprit rank, top-k, lift or localisation — failing the build when one exceeds the ceiling of the audit it cites, and requiring each to name its audit. Criteria annotated `[not ceiling-bounded: precision|latency|invariance|validity]` in spec §"Which criteria the coverage ceiling bounds" are **not** checked and MUST NOT be scaled down to the ceiling; the guard fails loudly if a criterion carries no annotation at all → FR-071, SC-022; fixture: unit
- [x] T018 [US0] Implement **corpus-gap detection** in `internal/investigation/audit/gaps.go`: given an audit and the set of fixture directories under `fixtures/incidents/`, compute which categories of the audit's unobservable remainder have no fixture, as a typed, machine-readable result. Detection only — the **wiring into the evaluation report is T109's**, in Phase 8, because that is where the report writer lives (moved by analyze O2) → FR-071b; fixture: unit (until T102 lands the four fixtures)

**Checkpoint**: the audit is on the record, machine-readable and comparable; `π₀` has a value and a provenance; no gate in the repository exceeds the ceiling. Quickstart §8 reproduces.

---

## Phase 3: The feature 001 change package (Foundational — blocks Phases 4–8)

**Purpose**: D1–D5 and the `alert.transition` convention from `contracts/graph-additions.proto`
and ADR-0005. **These tasks are specified here and implemented in 001's directories**; all of it
is additive, `buf breaking` must report a MINOR-compatible diff, and 002's fixtures are recorded
only after it lands (recording them earlier means re-recording them).

**⚠️ CRITICAL**: no incident fixture may be recorded until this phase is complete.

- [x] T019 [P] D3 schema: add `enum ActorKind { ACTOR_KIND_UNSPECIFIED = 0, PERSON = 1, AUTOMATION = 2, CONTROLLER = 3, VENDOR = 4, ACTOR_KIND_UNKNOWN = 5 }` to `api/sreagent/graph/v1/graph.proto` — **`ACTOR_KIND_UNSPECIFIED = 0` stays the zero value** because `buf lint`'s `ENUM_ZERO_VALUE_SUFFIX` requires it and because "the source said nothing" is genuinely distinct from `ACTOR_KIND_UNKNOWN` ("an actor was named and could not be typed") — plus `Change.actor_kind = 6`, `RankedChange.actor_kind = 13`, and the field on the change-observation event body → ADR-0005 D1, 002 FR-029d; fixture: unit
- [x] T020 D3 persistence: migration `internal/store/postgres/migrations/0005_actor_kind.sql` (**`0005`: the 001 change package is applied before 002's schema, which is `0006` in T004**) adding the column to the change projection, and `internal/projector/observe_change.go` writing it — **where the source is silent the projector writes nothing and the field is left `ACTOR_KIND_UNSPECIFIED`**, so canonical serialisation omits it and an existing golden does not move; `ACTOR_KIND_UNKNOWN` is written only where a feeder observed an actor it could not classify, with the evidence recorded, and **never guessed** → ADR-0005 D1, 002 FR-029d, plan §"The feature 001 change package" (I1); fixture: unit
- [x] T021 D3 feeders emit it: `internal/feeders/k8s/map_changes.go` maps ReplicaSet/HPA-driven scaling → `CONTROLLER` ("a controller reacting to state") and an annotated rollout → `PERSON` ("a person"), a CI or deploy principal → `AUTOMATION`; `internal/feeders/otel/emit.go` maps a `service.version` transition → `ACTOR_KIND_UNKNOWN` unless an annotation says otherwise; testkit conformance tests updated → ADR-0005 D1, 002 FR-029d; fixture: rollout-regression-01, rollout-regression-02, config-change-01
- [x] T022 D1+D2 ranking: in `internal/query/rank.go` compute the **signed** distance `dt = T_ref − c.valid.start`, keep field 7 floored so no 001 consumer breaks, add `signed_time_distance_seconds = 11` and `post_reference = 12` to `RankedChange`, and implement the two-sided decay — `temporal = exp(-dt/tau)` for `dt ≥ 0`, `temporal = kappa·exp(-|dt|/tau_post)` for `dt < 0`, published `kappa = 0.5`, `tau_post = tau/2` → ADR-0005 D4, plan §D1/D2, 002 FR-029b; fixture: unit + rollout-regression-01
- [x] T023 [P] D1 properties: table + `rapid` tests asserting the four properties 002 depends on — a post-reference candidate never receives the maximum temporal weight; it scores strictly below an equally distant pre-reference candidate; the score is monotone decreasing in `|dt|` on both sides; the caller can identify it as post-reference from the response → ADR-0005 D4, 002 FR-029c; fixture: unit
- [x] T024 D1 documentation: update `docs/schema/ranking.md` with the two-sided curve, `kappa`, `tau_post`, the signed distance, the `post_reference` flag and the new `ranking_formula` string, plus the note that 002 sets `reference_at` to the estimated onset → 001's published-schema rule, 002 FR-029b; fixture: n/a
- [x] T025 D1/D3 goldens: re-record goldens **only where a post-reference candidate or an emitted actor kind appears** — exactly `rollout-regression-01`, `rollout-regression-02`, `config-change-01`, and no others, because an unspecified actor kind and an empty `join_keys` map both serialise to nothing (T019, T020, T026) — and prove the diff is confined to the new fields and the changed temporal component; `scripts/check-report.sh` thresholds unchanged → plan §"The feature 001 change package" (I1); fixture: rollout-regression-01, rollout-regression-02, config-change-01
- [x] T026 [P] D4 join keys: add `map<string,string> join_keys = 6` to `Pointer` with the published role set (`version`, `workload`, `pod`, `host`, `trace`); emit them from `internal/feeders/otel/emit.go` (`service.version`, `k8s.deployment.name`) and `internal/feeders/k8s/map_workloads.go`; assert that **an empty `join_keys` map is omitted by canonical serialisation**, exactly as every other empty map already is, so a pointer with no join keys produces a byte-identical golden; document the roles and the vocabulary registry in `docs/schema/pointers.md` → ADR-0005 D6, 002 FR-014b; fixture: baseline-topology-01
- [x] T027 [P] D5 taxonomy: add `NodeType INVESTIGATION = 11`, `KNOWLEDGE_DOC = 12`; `EdgeType WATCHES = 8`, `INVESTIGATED = 9`, `CONCERNS = 10`; `ChangeKind` gains `DEPRECATION`, `VENDOR_INCIDENT`, `IAM_CHANGE`, `QUOTA_CHANGE`; projector accepts them and the invariant checker covers the new edge kinds → ADR-0005 D3, 002 FR-035; fixture: unit
- [x] T028 D5 event bodies: add `record_investigation = 21`, `submit_human_fact = 22`, `reopen_investigation = 23`, `label_investigation = 24`, `alert_transition = 25` to `EventEnvelope.body` with the message shapes of `contracts/graph-additions.proto`, and write the projector handlers `internal/projector/{record_investigation.go,submit_human_fact.go,reopen_investigation.go,label_investigation.go,alert_transition.go}` — `INVESTIGATION` node over `[started_at, ended_at)`, `INVESTIGATED` edges to subjects and targets, `CONCERNS` edges from a fact's entities, `WATCHES` edges from the alert to what it watches, the parent version never rewritten on reopen → ADR-0005 D2/D3, 002 FR-035, 002 FR-057a/b/e, data-model §"What this feature writes into the graph"; fixture: unit
- [x] T029 D5 validation: extend `internal/log/validate.go` with the **allow-listed `sre.investigation.*` prop namespace** so digests, query parameters and confidences pass while the existing telemetry denylist (`value`, `body`, `trace_id`, numeric sample arrays, props > 4 KiB) still rejects samples; one rejected sample per new reason in the table-driven test → 002 FR-034, 002 SC-010; fixture: unit (fixture assertion lands with `telemetry-rejection-01`, T106)
- [x] T030 D-alert idempotency: implement the **published 4-tuple** key `sha256(source, stable_alert_id, group, transition_at)` in the `alert_transition` append path — one key for both front doors: for a monitor transition `stable_alert_id` is the monitor or policy identifier and `group` is the monitor's group key; for a **human declaration** `stable_alert_id` is the stable identifier of the place the incident lives, `transition_at` is the declared instant and **`group` is empty**; filter flapping and no-data transitions at the feeder; **keep recoveries, because a recovery is evidence**; re-delivery under a seen key is `DUPLICATE_NOOP` → ADR-0005 D2, ADR-0003 D10, ADR-0004 D4, 002 FR-008b; fixture: unit
- [x] T031 [P] D-future announced facts: *(done 2026-09-17 as `fixtures/announced-fact-01`; the announcement state (announced/cancelled/superseded) is carried in the change summary for now — a typed state field is 003's FR-062–068 work)*  hand-author `fixtures/announced-fact-01` (a fact whose `valid_at` is after its `observed_at`) proving the projector's segment planning tolerates it, and publish the query semantic in `docs/schema/temporal-model.md` — *known now, answered only when the query's `valid_at` reaches it; `Extent` reports it as known; a subgraph as-of an earlier valid instant must not contain it* → ADR-0005 D5, plan §D-future; fixture: announced-fact-01
- [x] T032 Prove the package is additive: *(verified 2026-09-17: `buf breaking` clean; goldens changed only in rollout-regression-01/02 and config-change-01, additively — new fields, every rank and score unchanged; all other fixtures byte-identical)*  `buf breaking` against `main` reports **MINOR-compatible** for `sreagent.graph.v1`, no event-log transformation is required (`internal/log/migrate/` stays `v1→v1`), and **assert byte identity positively** — every 001 fixture other than the three named in T025 has goldens byte-identical to `main`, which holds because the projector leaves an unset actor kind `ACTOR_KIND_UNSPECIFIED` and canonical serialisation omits both that and an empty `join_keys` map (I1); write `docs/decisions/ADR-0006-feature-001-change-package.md` recording the diff and its semver → 002 FR-068, constitution IX; fixture: all 001 fixtures

**Checkpoint**: `sreagent.graph.v1` at MINOR+1; all 001 fixtures green (three with re-recorded goldens); actor kind, signed distance, join keys, the new taxonomy, the five event bodies and the allow-list are live. 002 may now record fixtures.

---

## Phase 4: The query algebra, `pkg/backend`, the recorded backend and the workers

**Goal**: until this exists there is nothing to record a world with, and until a world exists
nothing can be evaluated. Model-free except the logs worker's optional labelling pass.

**Primary stories**: US3 (replay from a recording), US2 (every hypothesis tested).

**Independent Test**: `worker record --out fixtures/incidents/rollout-regression-01-incident/world
--focus otel.service.name=checkout --window …` produces a world whose `index.json` digests match on
a second run, and `worker call metrics compare --mode recorded` returns the identical digest to
`--mode live` against the same recording.

### The algebra and the digest contract

- [x] T033 [US3] *(done 2026-09-17: mechanics in `pkg/backend/algebra.go` — the version, term names, `Normalise`, `TermKey`, the refusal — with the typed constructors and `Validate` in `internal/investigation/backend/algebra.go`; the split keeps `term_key` in the published SDK, which `pkg/backend/record.go` needs, without an import cycle)*  Implement `internal/investigation/backend/algebra.go`: Go types for every published term (`compare`, `onset`, `new_log_patterns`, `error_spans`, `errors_by_version`, `monitor_state`, `exemplars`, `drill_down`, `knowledge_search`, and the `GraphTerm` family), the canonical term encoding reusing 001's `internal/graph/canonical.go`, `term_key = sha256(canonical(term))` with instants normalised, an algebra version constant, and the `outside_algebra` refusal (exit 1, naming what was asked and what is available, recorded) → FR-042b; fixture: unit
- [x] T034 [P] *(done 2026-09-17: the six outcomes are six Go types with `MeansNothingHappened()`; the published total ordering is applied before the caps so truncation drops the least interesting rows)*  Implement `internal/investigation/backend/digest.go`: `Coverage`, `JoinKeys`, `DrillDown`, `Truncated`, the per-family digests, the six typed outcomes (`DIGEST`, `NO_DATA`, `NOT_YET_INGESTED`, `QUERY_FAILED`, `PARTIAL`, `NOT_RECORDED`) kept distinct at the type level and rendered differently, the one bounded free-text field flagged unverified and non-citable, the per-response size and cardinality caps with the truncation written **into** the response, and rejection with `missing_coverage` when a digest carries no coverage block → FR-014, FR-014a, FR-014b, FR-027, FR-037; fixture: unit
- [x] T035 [P] *(done 2026-09-17: masking rules shared with the Drain miner, so a template, an exemplar and the redactor cannot drift)*  Implement `internal/investigation/backend/redact.go`: `RedactionPolicy` applied **in live mode as well as when recording**, people identifiers **dropped, not hashed** (ADR-0003 D9), infrastructure identifiers pseudonymised with a keyed HMAC consistently so joins survive, log content reduced to masked templates, monitor bodies dropped; a backend emitting a field its declaration does not cover is rejected `undeclared_redaction` → FR-038; fixture: unit

### `pkg/backend` and the recorded backend

- [x] T036 [US3] *(done 2026-09-17: `Description` gains an optional `Capabilities` field carrying the read-only flag; `backend list` in `internal/cli/worker_backend.go`)*  Implement `pkg/backend/registry.go` and `describe.go`: registration validating that every declared term is published **and belongs to the telemetry family** (a backend declaring a graph or knowledge term is rejected), that every capability declares exactly one cost class from the published closed set `cheap|standard|expensive` (`CostClass` in `investigation.proto` §3 — `COST_CLASS_UNSPECIFIED` and anything unpublished are rejected), and that a state-changing capability is rejected `write_capability`; `backend list` CLI printing the terms and their cost classes → FR-016, FR-047a, contracts/telemetry-backend.md §1/§7; fixture: unit
- [x] T037 [US3] *(done 2026-09-17; miss accounting exposed through `Recorded.Misses()`)*  Implement `internal/investigation/backend/recorded.go`: the `recorded` `TelemetryBackend` reading `world/<term_key>.json`, making **no network call and never falling through to live**, and returning typed `NOT_RECORDED` with the term echoed for an in-algebra miss → FR-039, FR-040, FR-042c, FR-067; fixture: unit
- [x] T038 [US3] *(done 2026-09-17: the index digest **and** each answer's own digest are checked on load, so a hand-edited world is refused)*  Implement `pkg/backend/record.go`: the world writer in the published layout (`world/index.json` with algebra version, hop radius, drill-down depth, window grid, `term_key → file`, term count, `not_recorded` count, miss rate; `world/<term_key>.json` per digest), keyed by term and both instants, with the index digest checked on load → FR-036, FR-037, data-model §"The recording on disk"; fixture: unit
- [x] T039 [US3] *(done 2026-09-17: recorded `fixtures/rollout-regression-01/world` — 526 terms, 300 depth-1 drill-downs, byte-identical re-record. The telemetry comes from `pkg/backend/synthetic`, a stand-in until 003's GCP backend records a real world; T097 moves the layer into the incident fixture)*  Implement `internal/cli/worker_record.go` **and `internal/cli/fixture_record_world.go`**: `worker record --out <dir> --focus <ns>=<v> --window T1..T2 [--hops 2] [--grid <spec>]` — the cross product of the telemetry algebra over the neighbourhood (every pointer on every node within the hop radius, every edge on the path set) × the window grid, plus **depth-1** drill-downs, recording the depth as a manifest field so a fixture states its own limit — plus **`fixture record-world <dir>`**, the fixture-scoped wrapper that re-records the `world/` layer of an existing incident fixture by reading the focus, window grid, hop radius and drill-down depth from that fixture's own manifest rather than from flags, so a re-record cannot silently change a fixture's shape. Both are in `contracts/cli.md`; a re-record of an unchanged fixture must be byte-identical → FR-042b, plan F8; fixture: rollout-regression-01
- [x] T040 [US3] *(done 2026-09-17)*  Implement the miss-rate accounting: per-fixture and per-corpus `not_recorded` counts published on every verification run, the `miss_rate_threshold` (0.05) gating **a fixture's admission to the corpus, never the engine's score**, and a fixture above it reported as insufficient rather than silently scored → FR-042c, SC-018; fixture: unit

### `pkg/worker` and the five workers

- [x] T041 *(done 2026-09-17: `pkg/worker/call.go` holds the call path — per-attempt records, typed failures, mode check; `worker list|call` in `internal/cli/worker.go`)*  Implement `pkg/worker/registry.go`: declaration validation (exactly one source of truth, every capability read-only and named, `ContainsModel` + `ModelID` declared not discovered, redaction declared, both modes present), rejection of an undeclared capability at call time, per-call mode recording, and worker failures/timeouts/empty results recorded as evidence items with their reason with retries recorded individually; `worker list` and `worker call <worker> <term> --args <json> [--mode live|recorded]` CLI → FR-010, FR-011, FR-015, FR-016, FR-018, FR-057d; fixture: unit
- [x] T042 [US1] *(done 2026-09-17: the 001 response travels beside the digest in `worker.Response.Graph`; coverage comes from `Extent`, so a source gap is visible on every graph answer)*  Implement `internal/investigation/workers/graph/`: typed wrappers over 001's published `QueryService` RPCs — `subgraph`, `diff`, `impact`, `pointers`, `node_history`, `resolution_audit`, `extent` — passing **both** time dimensions through, deterministic, no model, and **not** cross-producted into a world because they are answered from the replayed event log (plan F3) → FR-009, FR-012, FR-042b; fixture: rollout-regression-01, baseline-topology-01
- [x] T043 [P] [US2] *(done 2026-09-17: separability is the worker's judgement, at the published 20 % threshold)*  Implement `internal/investigation/workers/metrics/compare.go`: `compare(pointer, window_pair, statistic)` returning per-series summaries, the direction and magnitude of the difference and whether it separates the hypothesis, with coverage, join keys and drill-down handles → FR-030, FR-009a (no model); fixture: unit
- [x] T044 [P] [US2] *(done 2026-09-17: `errors_by_version` refuses a pointer that declares no `version` join key rather than guessing the attribute)*  Implement `errors_by_version(pointer, window, version_attribute)` reading the attribute from `Pointer.join_keys["version"]` (D4), and `monitor_state(pointer, window)` returning transitions and per-group states → FR-042b, FR-014b; fixture: unit
- [x] T045 [US2] *(done 2026-09-17)*  Implement `pkg/backend/onset/`: the `onset(pointer, search_window, method)` term as the algebra's reference implementation, executed **backend-side** so the series never crosses the digest boundary — seasonal-naive baseline + two-sided CUSUM refined by binary segmentation, O(n), one published threshold, returning the estimated instant, an uncertainty, the method and its parameters → FR-029a, constitution IV, research §4; fixture: unit
- [x] T046 [US2] *(done 2026-09-17: `TestDeterminismFixedSeries` pins 2026-09-01T13:25:00Z ±120 s, statistic 0.187386)*  Determinism test for the onset estimator across architectures: the same recorded series produces the same instant and uncertainty to **six decimals on arm64 and amd64**, enforced by explicit rounding at each accumulation step exactly as the ranking formula does; run on both runners in `ci.yml` → SC-019, plan §Complexity Tracking; fixture: unit
- [x] T047 [P] [US2] *(done 2026-09-17)*  Implement `internal/investigation/workers/traces/`: `error_spans(edge, window)` returning counts and latency statistics by operation and error kind on that edge, and `compare` over span groupings; algorithmic, no model → FR-011, FR-009a; fixture: unit
- [x] T048 [US2] *(done 2026-09-17: the labelling pass is off unless configured and flips `ContainsModel` when it is)*  Implement `internal/investigation/workers/logs/`: an in-repo Drain-style template miner, `new_log_patterns(pointer, window, baseline_window)` returning masked templates with counts and which are new, `exemplars(handle, limit)` bounded, sanitised and **only on explicit request**, and an optional `claude-haiku-4-5` labelling pass over templates only whose contribution the digest states explicitly — the model never sees a raw line → FR-009a, FR-014; fixture: unit
- [x] T049 [US6] *(done 2026-09-17: `knowledge link` emits `upsert_node` + `CONCERNS` edges, so no new event type was needed)*  Implement `internal/investigation/workers/knowledge/`: retrieval **scoped by the graph** to documents linked by `CONCERNS` to nodes in the investigation's subgraph or to its candidate changes, BM25 over the scoped set (deterministic, no embeddings), every item cited with identifier, linked entities, link provenance and age; retrieval over telemetry refused; a hypothesis resting only on a document is reported knowledge-derived and unconfirmed and may not reach `supported`; plus **`knowledge link`** to register a human-written document as a `KNOWLEDGE_DOC` node with its `CONCERNS` edges and a pointer to where it lives — **never its content** — recording the registering identity, the instant and each link's provenance (the v1 corpus is past investigations + human-registered documents, plan F5) → FR-049, FR-049a, FR-050, FR-051, FR-052, contracts/cli.md; fixture: unit (goldens with `knowledge-scope-01`, T106)
- [x] T050 *(done 2026-09-17: the world **is** the golden — no second set of expected files to drift from it)*  Implement `pkg/worker/testkit/` and `pkg/backend/testkit/`: replay a recorded world through a worker or backend and diff its digests against goldens byte for byte (the worker analogue of `feeder.testkit`), assert live-mode and recorded-mode digests are identical **including coverage and join keys**, and assert no worker without recorded responses can be merged → FR-015, SC-003, constitution VIII; fixture: unit

**Checkpoint**: `worker list` shows five workers with declarations; `worker call` answers each term in both modes with identical digests; a world records and re-reads with a stable index digest; an out-of-algebra call is refused and recorded.

---

## Phase 5: The hypothesis ledger with computed confidence

**Goal**: the central data structure, held outside the model's context, model-free, correct before
the first model call.

**Primary stories**: US2 (hypotheses are tested, not asserted), US4 (`unknown` is computed).

**Independent Test**: `go test ./internal/investigation/ledger -run Property` passes over permuted
judgment orders, and a hand-worked example ("prior 0.31, three supports at moderate, one refute at
strong → 0.58") reproduces to six decimals.

- [x] T051 [US2] Implement `internal/investigation/ledger/types.go` and `internal/investigation/store/ledger_dao.go`: `Hypothesis` (statement, kind, candidate cause, target entities, status, prior, confidence, rationale, supporting/refuting evidence ids) and `Judgment` (one hypothesis, one evidence item, direction, strength), persisted to schema `investigation`, with the `duplicate_judgment` rejection enforcing **at most one judgment per (evidence item, hypothesis) pair** → FR-020, FR-020a; fixture: unit
- [x] T052 [US2] Implement `internal/investigation/ledger/posterior.go`: `ln posterior_i = ln prior_i + Σ_j ln LR_ij` normalised by a numerically stable log-sum-exp, the published LR table (`WEAK 1.5`, `MODERATE 3`, `STRONG 10`, `DECISIVE 50`, `NEUTRAL 1`, refutes = reciprocal), candidate priors from the published ranker score normalised over the set and scaled by `1 − π₀` (from T016), rounding to six decimals, and the five published buckets `very_low [0,0.10) · low [0.10,0.30) · moderate [0.30,0.60) · high [0.60,0.85) · very_high [0.85,1.00]` each carrying its range into the output; < 10 ms for 50 hypotheses × 500 judgments → FR-023, SC-004, research §8; fixture: unit
- [x] T053 [P] [US2] Property test `internal/investigation/ledger/posterior_property_test.go` with `rapid`: the posterior is invariant under every permutation of judgment arrival order, and recomputation from the rows reproduces the stored `confidence` exactly at six decimals — the formal statement of "two runs that gather the same evidence agree" → FR-023, data-model §Invariants 1; fixture: unit
- [x] T054 [US4] Implement the mandatory open hypothesis: **exactly one** `no_observed_change` hypothesis per investigation with prior `π₀`, always present, always ranked, scored and rendered like any other, and the invariant that normalised posteriors sum to `1.0 ± 1e-6` over the set including it → FR-019a, data-model §Invariants 2 and 10; fixture: unit
- [x] T055 [US2] Implement the status machine and the write-time validation rules: `proposed → supported | refuted | inconclusive | untested | exonerated`; `unsupported_status` when `supported` has no qualifying judgment from a source-of-truth worker; `model_confidence` when anything but the ledger rule writes a confidence; `untested` requires a reason **and a next query** and is never scored as refuted or dropped; `exonerated` carries the onset estimate's evidence id → FR-020, FR-023, FR-031, data-model §Validation rules; fixture: unit
- [x] T056 [P] [US2] Implement conflict and non-separability reporting: where evidence conflicts both sides are reported and reflected in the confidence, and choosing a side without stating why is impossible by construction; where two candidates cannot be separated they are reported together at comparable confidence with **what would separate them** named → FR-024, FR-025; fixture: unit (goldens with `two-simultaneous-01`, T105)
- [x] T057 [US2] Implement `internal/investigation/ledger/render.go` and `export.go`: the compact turn-scoped table (hypothesis id, kind, statement, status, prior, confidence with its bucket, counts of supporting and refuting judgments, the exact next query for each untested hypothesis, budget remaining, stop conditions in force) and the exported ledger readable by graders and reviewers **without replaying the run** → FR-020b; fixture: unit
- [x] T058 [P] [US2] Implement confidence **widening** on a cut-short test: the reported bucket moves outward one step toward the mass the untested evidence could have moved, and the widening is recorded with its reason — never frozen at the last computed value → FR-045; fixture: unit
- [x] T059 [US1] Implement `internal/investigation/store/decision_record.go`: emit exactly **one** `record_investigation` event at conclusion, in one transaction, idempotency key = investigation id, carrying identifiers, statuses, confidences with buckets, ranks, rationales, spend, model configuration, `recording_key` and `recording_digest` under the `sre.investigation.*` prefix — and **no telemetry payload**; judgments, worker calls and ledger updates are never events → FR-033, FR-034, FR-035, constitution III/IV; fixture: unit (rejection proof with `telemetry-rejection-01`, T106)

**Checkpoint**: the ledger computes, recomputes and normalises deterministically; order-independence holds under `rapid`; no path exists by which a model writes a confidence.

---

## Phase 6: The engine loop, the deterministic first wave and the budget manager

**Goal**: the feature itself. The first wave comes **before** the model on the critical path,
which is what makes SC-013 achievable. Intake, the verifier, the renderer and the human channel
interleave here; the report-delivery **contract** lands here and its transport does not. The six
fixture-authoring tasks this phase consumes (T097, T102–T106) open it, moved here from Phases 7 and
8 by analyze O1 so that no task in this phase names a test that does not yet exist.

**Primary stories**: US1, US1a, US2, US4, US5, US6, US7, US8, US9.

**Independent Test**: `investigate otel.service.name=checkout --at 2026-09-01T14:32:00Z --profile page --watch` against the recorded world of `rollout-regression-01-incident` (recorded by T097, first in this phase) streams a provisional ranking within 5 s, a first wave, then model turns, and concludes with a verdict, a ranked list carrying exonerations, a timeline and a narrative — every claim citing an evidence id.

### The fixtures Phase 6 consumes (moved here from Phases 7 and 8 by analyze O1)

These six tasks authored fixtures that Phase 6's own tasks name as their test. They were listed in
Phases 7 and 8, *after* their consumers; they are moved here unchanged in id and in content, before
the tasks that consume them. They are six independent fixture-authoring tasks and may run in
parallel with each other. **None of them may start before Phase 3 lands** — recording a fixture
before the 001 change package means recording it twice.

- [x] T097 [US3] *(done 2026-09-17: `fixtures/incidents/rollout-regression-01-incident` — the parent's 95 events copied byte for byte and 10 goldens re-recorded identical to it, so the derivation is a file comparison. World: 275 terms, 1.2 MB, hop radius 2, drill-down depth 1, **one** grid entry centred on the estimated onset 14:20 rather than on the alert instant at 14:32 — centred on the alert, twelve minutes of symptom fall in the baseline half and every comparison reads flat; miss rate 0. The parent's METRIC pointers gained `join_keys: {version: service.version}`, without which `errors_by_version` is not a term the algebra can form; only that field moved in its goldens and the ranked order is unchanged. T039's world under `fixtures/rollout-regression-01/` moved here, as T039's note said it would, and `docs/schema/{algebra,digests}.md` take their worked examples from the new location)* *(moved here from Phase 7 by analyze O1 — Phase 6 consumes this fixture)* Create `fixtures/incidents/rollout-regression-01-incident/`: derive the directory from 001's `rollout-regression-01`, record the **world** with `worker record` (hop radius 2, drill-down depth 1, the declared window grid), and author the `incident:` block including `ground_truth` (culprit `k8s.change=shop/payments@rev7`, causal path, decisive and exonerating evidence as predicates over digests, `knowability_time`, labelled onset with tolerance, `prior_rank_of_culprit`, provenance `derived`). The **trajectory** layer is not authored here — it is recorded in Phase 7 from the first live run, by T093's recorder → FR-061b, FR-062, quickstart §1; fixture: rollout-regression-01-incident
- [x] T102 [US10] *(done 2026-09-17: all four ship, each with a small graph, a decoy that is the only change in its window, and a world recorded with **no** culprit so that every comparison in it reads flat and nothing supports any candidate. `bin/aisre audit coverage gaps --audit docs/evaluation/coverage-audit-2026-09.json` now reports zero gaps; the three tests that asserted the empty-corpus state — T018's own, the published-aggregate test and the CLI's — were flipped to assert the coverage, with the empty-corpus behaviour kept as its own case)* *(moved here from Phase 8 by analyze O1 — Phase 6 consumes this fixture)* Hand-author the four unobservable-remainder fixtures — `unobserved-latent-bug-01`, `unobserved-client-config-01`, `unobserved-business-data-01`, `unobserved-credential-leak-01` — each with ground truth `unobserved` or `not_change_induced` carrying its category, each graded so that the `no_observed_change` hypothesis must rank first and naming a decoy is a failure → FR-071b, SC-023, ADR-0004 D5; fixture: unobserved-latent-bug-01, unobserved-client-config-01, unobserved-business-data-01, unobserved-credential-leak-01
- [x] T103 [P] [US1a] *(done 2026-09-17: the declaration, an identical re-delivery under a different event id that leaves the alert node at one version, a Datadog monitor firing five minutes later on payments, and a declaration whose target text no rule can read and which therefore carries no WATCHES edge at all — 12 goldens, the last of them the file in which "the targets could not be parsed" is an assertion rather than a sentence)* *(moved here from Phase 8 by analyze O1 — Phase 6 consumes this fixture)* Hand-author `fixtures/incidents/declared-incident-01`: a human declaration with no monitor, a re-delivery of the identical declaration that changes nothing, a monitor firing minutes later on a neighbouring service that **attaches**, a declaration whose targets cannot be parsed concluding `unknown` with `confirm_identity`, and the human-supplied services reopening it → FR-001a, FR-002a, FR-002b, FR-004a, FR-008b, FR-008c, quickstart §1a; fixture: declared-incident-01
- [x] T104 [P] [US9] *(done 2026-09-17: a concluded decision record, a typed and signed human fact claiming a manual rollback at 14:25, a recorded world in which payments is still failing at 14:35 and so contradicts it, a reopen into a new linked investigation that reports both, and the original readable and unchanged in `history.parent-history`)* *(moved here from Phase 8 by analyze O1 — Phase 6 consumes this fixture)* Hand-author `fixtures/incidents/human-fact-reopen-01`: a concluded investigation, a typed human fact contradicted by telemetry, both kept with the contradiction stated, a reopened linked record and the original readable and unchanged → FR-057a, FR-057b, SC-015, SC-024, quickstart §6; fixture: human-fact-reopen-01
- [x] T105 [US10] *(done 2026-09-17: `unknown-feeder-gap-01` is hand-authored rather than derived from `feeder-gap-01`, and the manifest says why — that recording pins the feeder NOT reporting its gap (`extent` says GAPS 0), so a fixture that needs the gap to be nameable cannot come from it. Here the culprit lands inside the hole and is backfilled at 14:40:30, so `knowability_time` is after `fired_at` and `unknown` with the gap named is the correct answer at the alert instant; the two diff goldens differ only in observed time and must differ in result. `two-simultaneous-01` puts a rollout and a ConfigMap edit on the same target at the same second, carried by the same pods and so tagged with the same `service.version`, which is what makes them genuinely inseparable; its decoy carries `causal_role: not_separable` and an empty exonerating-evidence list, the one case in which empty is legal. `merged-alias-01` is derived from `ambiguous-identity-01`, asked by the Kubernetes alias of the C2-merged entity)* *(moved here from Phase 8 by analyze O1 — Phase 6 consumes this fixture)* Author the three required-shape fixtures: `unknown-feeder-gap-01` (derived from `feeder-gap-01`, window overlapping the gap, expected `unknown`), `two-simultaneous-01` (two plausible simultaneous changes reported together at comparable confidence with what would separate them), `merged-alias-01` (derived from `ambiguous-identity-01`, the alert naming an alias of a merged entity) → FR-024, FR-025, FR-062, SC-007; fixture: unknown-feeder-gap-01, two-simultaneous-01, merged-alias-01
- [x] T106 [US10] *(done 2026-09-17: `telemetry-rejection-01` ships a `record_investigation` in `rejected.jsonl` carrying a seven-sample series under `sre.investigation.hypotheses`, refused `telemetry_payload` with the graph unchanged, beside the accepted twin that carries confidences and a `recording_digest` and no samples. `injection-01` puts instruction-like text in two free-text fields the model will see — a SCALING change's summary and a registered runbook's summary — over an ordinary payments-rollout incident whose verdict must not move. `knowledge-scope-01` links an in-subgraph runbook and an out-of-subgraph one written to be the *better* lexical match, on a `reporting` service that is its own component and so outside checkout's subgraph at any hop radius)* *(moved here from Phase 8 by analyze O1 — Phase 6 consumes this fixture)* Author the three conformance fixtures: `telemetry-rejection-01` (a decision record carrying a series is rejected with `reason_code = telemetry_payload`), `injection-01` (retrieved content reading as an instruction leaves scope, budgets, worker set, posture and output unchanged and records the attempt), `knowledge-scope-01` (an in-subgraph document cited, an out-of-subgraph document cited in 0 % of runs) → FR-017, FR-034, SC-010, SC-014, quickstart §9, §11; fixture: telemetry-rejection-01, injection-01, knowledge-scope-01

### The model boundary and the loop

- [X] T060 [US1] Implement `internal/investigation/model/client.go`: the Anthropic Go SDK wrapper — request build, cache breakpoint after the tools+system prefix, structured outputs (`output_config.format`) with `strict: true` tools, `count_tokens` pre-flight, `usage` post-flight accounting by class (input, cache_write, cache_read, output), streaming, and the record/replay hook that later serves layer 1 → research §2/§3, FR-048; fixture: unit
- [X] T061 [US1] Implement `internal/investigation/engine/prompt/`: the stable prefix per `contracts/prompting.md` (role and posture, the algebra verbatim, the digest contract, the ledger protocol, the output contract, the data rule), **stable to the byte** for the life of a schema version — no timestamps, no per-request ids, no unsorted maps — versioned with the algebra; conformance test asserting `usage.cache_read_input_tokens > 0` from turn two → FR-061, quickstart §11; fixture: unit
- [X] T062 [US1] Implement the tool set: one tool per algebra term plus exactly two engine tools — `propose_judgments` (batch of hypothesis/evidence/direction/strength, `strict: true`, schema-validated, applied and recomputed by the **engine**) and `propose_hypothesis` (candidate cause or named condition with targets, prior assigned by the engine from the published ranker score or the published condition prior). No tool writes a confidence, re-ranks, reaches a source or changes a budget → FR-023, FR-020a, contracts/prompting.md; fixture: unit
- [X] T063 [US1] Implement `internal/investigation/engine/loop.go`: the hand-rolled agent loop — append-only history (earlier turns are never edited, so thinking blocks stay valid), the ledger re-rendered each turn as a **turn-scoped mid-conversation system message** (`clear_at: "next_user_message"`), no forced tool use, the **engine as the ledger's writer of record**, a turn that proposes nothing recorded as such and answered with a turn-scoped reminder rather than a history-rewriting retry, and exactly **one belief state** per investigation with evidence gathering parallelised but reasoning never forked → FR-013, FR-013a, FR-009, research §3; fixture: unit
- [X] T064 [P] [US5] Implement typed stop reasons in `internal/investigation/engine/stop.go`: `completed`, `budget_exhausted` naming the budget, `diminishing_returns`, `worker_unavailable`, `refused`, `failed`, each recorded as a typed event and present in **both** renderings; an API `refusal` stop reason handled and recorded like any other terminal condition, never silently retried → FR-045b; fixture: unit

### The deterministic first wave (no model in the loop)

- [X] T065 [US1] Implement `internal/investigation/engine/provisional.go`: the graph's own ranking published as a **provisional, explicitly untested** answer **within 5 s** of intake (FR-046a's published figure, not "within seconds"), prior-only, streamed by `--watch` and by the `Investigate` stream's first message → FR-046a, SC-013; fixture: rollout-regression-01-incident
- [X] T066 [US1] Implement `internal/investigation/engine/firstwave.go`: for each top candidate, run in parallel with no model in the loop — error rate and latency **before and after onset**, new log patterns, error spans on the path edges, and errors split by version tag — producing evidence items and judgments directly; completes within the first 30 s; and assert at record time that **the first-wave queries are always present in a fixture's recorded world** → FR-046a, SC-013, plan F2; fixture: rollout-regression-01-incident

### The budget manager

- [X] T067 [US5] Implement `internal/investigation/budget/profiles.go`: published `page` (first tested hypothesis 90–120 s, **substantive answer** — at least one hypothesis at status `supported` or `refuted` together with a verdict line that passed the citation checker, per FR-047 — within 5 min, hard stop 10 min) and `review` (quality-bound, operator cap only) profiles, selectable by alert priority, each stating its targets and hard stop, loaded from `--budget-profiles <file>`, and the profile applied recorded on every investigation → FR-043, FR-047; fixture: unit
- [X] T068 [US5] Implement `internal/investigation/budget/admission.go`: every model call and every worker call passes admission **before it is issued** — `count_tokens` for the model, the backend-declared cost class and the requested window width for the worker — so a reserve is never overrun by a call in flight; a call that would breach a budget is never issued and its intent is recorded as an untested hypothesis's next query with its deep link → FR-043, FR-047a; fixture: unit
- [X] T069 [P] [US5] Implement quota share: read `remaining_quota` and `quota_window` from every digest's coverage block, spend at most the configured share of what remains, record the observed remaining quota and the share consumed **per call**, and fall back to the absolute per-backend budget where the vendor reports nothing — recording that it did → FR-047a; fixture: unit
- [X] T070 [US5] Implement the **exactly 15 %** reserve (a published constant, asserted by the fixture, not an approximation) and `synthesis_only` mode: on exhaustion of the unreserved portion the engine stops gathering evidence and runs the verifier, the render and the stop reason from the reserve, so an exhausted investigation still produces a written answer; there is no path back to `normal`; continuing past a budget and discarding partial results are both impossible → FR-045, SC-005; fixture: unit
- [X] T071 [P] [US5] Implement the diminishing-returns stop: fire when `‖p_t − p_{t−n}‖₁ < ε` over the ledger's posterior vector across the last `n` worker calls, published initial values `n = 5`, `ε = 0.02`, with a typed stop reason → FR-045a; fixture: unit
- [X] T072 [US5] Implement `internal/investigation/budget/spend.go`: consumption reported against **every** budget in machine-readable form including calls per worker and elapsed time; the primary cost unit is **tokens per model id per class plus worker calls per backend per cost class**, provider-independent and comparable across runs; a monetary figure derived from the versioned price table whose version is stored on the investigation → FR-044, FR-048, SC-008; fixture: unit
- [X] T073 [P] [US5] Implement operator caps and the non-configurable set: wall time, cost, per-backend and per-cost-class budgets, maximum query window width, concurrent investigations and daily global spend are configurable; the evidence requirement, the read-only posture, the recording of calls and the verification pass are **not** and any attempt to relax them is refused with a reason → FR-043, FR-047b; fixture: unit

### Verification

- [X] T074 [US2] Implement `internal/investigation/verify/checker.go`: the **deterministic** citation checker running first and cheapest — every claim resolves to ≥ 1 evidence id and every number in a claim matches a field of the digest it cites; a claim failing either is removed or demoted before the model pass → FR-022, FR-061a, SC-002, SC-017; fixture: unit
- [X] T075 [US2] Implement `internal/investigation/verify/verifier.go`: the fresh-context pass on a **different model** (`claude-opus-5`) containing only the rendered claims and the evidence items they cite — never the investigator's reasoning — with structured output `supported | unsupported | number_mismatch` per claim, findings recorded on the investigation, and the free-text digest field excluded from citation resolution → FR-022a, SC-017, research §17; fixture: unit

### Intake (both front doors)

- [X] T076 [US1] Implement `internal/investigation/intake/alert.go`: normalise a monitor `alert.transition` into subject entity reference(s), a symptom statement, the observed symptom instant and the window; retain the origin reference; pin **both** instants with the observed instant defaulting to the symptom instant; and implement `--review-mode`, recorded on the investigation and never the default for an alert-driven run → FR-001, FR-002, FR-004, FR-005; fixture: rollout-regression-01-incident
- [X] T077 [US1a] Implement `internal/investigation/intake/declared.go`: a human declaration is a **first-class intake with no monitor**, normalising into the same `alert.transition` shape distinguished by actor kind `PERSON`, carrying severity, instant of declaration, title, authenticated declaring identity, origin reference of the place the incident lives and zero or more targets — none of them invented; the observed instant pinned to the **instant of declaration**, a later edit to severity, title or targets entering as a separate later event rather than moving the pin; idempotency key `(source, stable place id, declared instant)` so re-delivery is a no-op returning the same investigation; targets `parsed_from_declaration` or `supplied_by_human`, **never guessed**, with the provenance recorded and distinguishable — and where a target was parsed, the investigation records **only the rule id and the rule version**, because the **parse-rule registry is a connector artifact** owned and versioned by the connector that observes declarations in that place, and this engine neither defines the rules nor interprets their text (U2); the idempotency key is the published 4-tuple with an empty `group` (T030) → FR-001a, FR-002a, FR-002b, FR-004a, FR-008b; fixture: declared-incident-01
- [X] T078 [US1a] Implement `internal/investigation/intake/group.go`: the published association rule — one investigation covers one **incident**, not one alert; group a declared incident and a monitor transition for the same symptoms by proximity in time and proximity in the graph neighbourhood; whichever arrives first opens the investigation and the later attaches as an additional symptom with its own instant, origin reference and actor kind; the grouping decision with the time and neighbourhood distances it rested on is recorded as **evidence**; an ungrouped declaration opens its own investigation rather than being dropped; two investigations for one incident are a defect whichever path produced them → FR-008a, FR-008c; fixture: declared-incident-01
- [X] T079 [US1] Implement subject resolution in `internal/investigation/intake/resolve.go`: resolve to a canonical graph entity through the published resolution path **before reasoning**, report both the identifier given and the entity resolved to, attach the resolution audit as an evidence item, never proceed on a guessed identity, and produce an `unknown` outcome under FR-026 for an unresolvable subject — noting that merges decided after the investigation instant are invisible under the observed-time pin and saying so → FR-003, constitution VI; fixture: merged-alias-01, ambiguous-identity-01
- [X] T080 [P] [US4] Implement extent consultation: before relying on a window, consult the graph's `extent` and flag any hypothesis whose evidence falls inside a known source gap, carrying the gap into the rendering and into the confidence → FR-032; fixture: unknown-feeder-gap-01, feeder-gap-01
- [X] T081 [US2] Implement `internal/investigation/engine/causal.go`: obtain pointers valid at the investigation instant and **never construct a selector of its own** for an entity the graph can describe; obtain ranked candidates with `reference_at` set to the **estimated onset** and never re-rank them, expressing any disagreement as a hypothesis with evidence; type a candidate starting later than onset by more than the onset's uncertainty as a **candidate effect**, exonerate it with the onset estimate cited unless independent evidence supports it as a cause, and carry exonerations as first-class evidence; use and state the **actor kind**, so a controller-produced scaling event is never presented as an equal suspect to a human-originated change; where onset cannot be estimated, say so, fall back to the alert instant and **exonerate nobody on timing alone** → FR-029, FR-029a, FR-029b, FR-029c, FR-029d, SC-009, SC-019; fixture: rollout-regression-01-incident, adjacent-decoy-01

### Lifecycle, the human channel and the evaluation loop

- [X] T082 [US9] Implement `internal/investigation/store/lifecycle.go`: statuses exactly `running`, `concluded` (qualified partial or final), `reopened`, `failed`, with **no state in which the engine waits for a human**; the terminal outcome (`ranked`, `unknown`, `budget_exhausted`, `failed`) a separate property from the status; a completed investigation immutable, every later addition a separate linked record; `lifecycle = 'reopened'` the single permitted transition on a concluded row → FR-006, FR-007, SC-024; fixture: unit
- [X] T083 [US9] Implement human facts and reopen: a typed fact pushable into a **running or concluded** investigation at any time, carrying the authenticated individual and the time, entering the evidence log as an evidence item, non-blocking, weighted at `STRONG` and **never `DECISIVE`** — where telemetry contradicts it both are kept and the contradiction is stated in the affected hypotheses; a fact bearing on a concluded investigation moves it to `reopened` and produces a **new linked record** with the concluded record readable exactly as produced → FR-057a, FR-057b, SC-024; fixture: human-fact-reopen-01
- [X] T084 [US7] [US8] Implement human review, the label and the corpus hand-off: `investigate review` records the validated root cause, amendments to any hypothesis's status or rank and a rationale, attributable, additive, surviving a full replay and **taking precedence over the engine's scores wherever the two are compared**; `investigate label --right|--wrong` records the one-click "was this right?"; `investigate to-incident` turns a reviewed investigation into a corpus incident in one command **with no hand-editing** that passes the harness on first run; and the engine's agreement with human corrections is reported as a metric on every evaluation run → FR-054, FR-055, FR-056, FR-057e, SC-012, SC-015; fixture: rollout-regression-01-incident

### Rendering, `unknown`, and returning the report

- [X] T085 [US1] Implement `internal/investigation/render/`: the published order — (1) a one-line verdict naming the decisive fact and the rollback candidate **or stating there is none**, (2) the ranked hypotheses each with evidence for and against and **exonerations rendered as prominently as supports**, (3) a timeline of candidate changes and the estimated onset, (4) the narrative, which never precedes the verdict and contains no claim absent from them; every evidence item in **both** renderings carries a deep link resolving to the exact query, selector and window, or states why none exists; and a guard rejecting any output that proposes, prepares or names a remediation — statements about what to do next are limited to what to investigate or observe → FR-019, FR-028, FR-057c, FR-057d, SC-025; fixture: rollout-regression-01-incident
- [X] T086 [US4] Implement the `unknown` terminal outcome: list the hypotheses considered with their statuses and evidence and name at least one concrete resolving action — a source, a pointer, an identity confirmation, a wider window — rendered as "what would resolve this" with the evidence each would change; the engine is **report-only** toward humans, never blocks and reaches a terminal state whether or not anyone responds → FR-026, FR-057, SC-007, SC-024; fixture: unknown-feeder-gap-01, unobserved-latent-bug-01
- [X] T087 [US1a] Implement the report-delivery **contract** in `internal/investigation/render/delivery.go` and `investigate report <id> [--deliver]`: report-only, **updated in place** rather than re-posted as a stream, carrying the rendering order and the deep links, non-blocking, never a precondition of a terminal state, a failed delivery recorded while the investigation still concludes, on a credential granting nothing beyond writing and editing that one report and separate from every read credential; the transport is a **stub** with a recorded no-op driver, because it is a connector concern → FR-057f, ADR-0005 D8, constitution VII; fixture: declared-incident-01

### Interface, authorization, telemetry and the barrier

- [X] T088 Implement `internal/cli/investigate*.go`: `investigate`, `investigate declare`, `get`, `list`, `watch`, `export`, `replay`, `fact`, `review`, `label`, `report`, `to-incident`, each producing **both** renderings from the same run, inheriting 001's `--output`, `--server`, `--as-of`/`--observed-at` and exit codes (`0 ok · 1 usage · 2 transport · 3 auth · 4 verification`) → FR-046, FR-053, FR-064, contracts/cli.md; fixture: rollout-regression-01-incident
- [X] T089 Implement `internal/server/investigation.go`: `InvestigationService` — the complete **dual-surface subset of FR-064**: `Investigate` and `Declare` streaming the anytime shape, `Get`, `Replay`, `Reopen`, `SubmitHumanFact`, `Review`, `Label`, and **`List` and `Export`**, the two the analyze pass found missing (G1); operator tooling (`audit coverage`, `worker *`, `backend list`, `knowledge link`, `fixture *`) is deliberately **not** served and stays CLI-only — behind a **new `investigator` role** added to the OIDC role set beside `reader` and `decider`; refuse to start with write-scoped credentials (exit 3) and refuse an anonymous or shared credential (`anonymous_principal`, exit 3); `serve --enable-investigation --model-config --recording-root --budget-profiles`, and without the flag the binary behaves exactly as it does today → FR-008, FR-064, FR-066, FR-067, plan F10; fixture: unit
- [X] T090 [P] [US6] Implement the injection barrier: worker output reaches the model only as `tool_result` blocks; the one bounded free-text field per digest travels under a key the system prefix declares untrusted, is never citable as evidence on its own and is excluded from the verifier's citation resolution; operator instructions travel only on the mid-conversation system channel no worker can write to; a detector over free-text and exemplar fields records an `injection_attempt` evidence item, and the investigation's scope, budgets, worker set, read-only posture and output are provably unaffected → FR-017, ADR-0003 D8; fixture: injection-01
- [X] T091 Implement evidence-item recording in `internal/investigation/store/evidence.go`: every item records the worker, the capability, the exact parameters as called, the valid and observed instants in force, the time of the call, the mode, a reference to the recorded response and its digest, and the graph event ids or entity versions it rests on; every call carries **the hypothesis it serves and the discriminating question**, a call serving none recorded as `exploratory:<reason>`; calls are **never deduplicated**; every call and response is recorded in the order issued with timings; and an item whose `observed_at` is later than a non-review investigation's is rejected → FR-018a, FR-021, FR-033, data-model §Invariants 5 and 7; fixture: unit
- [X] T092 [P] Implement `internal/investigation/telemetry.go` over 001's `internal/telemetry`: spans per investigation, turn, worker call, model call and algebra term; metrics `investigations_total{outcome,profile}`, `investigation_duration_seconds{profile,phase}`, `worker_calls_total{worker,capability,mode,outcome}`, `worker_call_latency`, `model_tokens_total{model,class}`, `investigation_cost_units{model}`, `backend_quota_share{backend}`, `ledger_updates_total{direction}`, `stop_reasons_total{reason}`, `replay_divergences_total{layer}`, `not_recorded_total{fixture}`; a test asserting every instrument exists → FR-065; fixture: unit

**Checkpoint**: an end-to-end investigation runs against a recorded world with a real model call, concludes with a verified, cited, ordered report, and its spend is reported against every budget. Quickstart §1, §1a, §2, §4, §5, §6, §7 reproduce.

---

## Phase 7: Trajectory replay as the CI gate (User Story 3)

**Goal**: the plumbing gate, before any evaluation work, so it is never retrofitted.

**What is left here after analyze O1**: **trajectory recording and replay only**. The fixture
directories, their `world/` layers and their `incident:` blocks are authored in Phase 6, because
Phase 6's tasks name them as their test. `rollout-regression-01-incident`'s **trajectory** layer is
produced *here*, by T093's recorder, from the first live run through the Phase 6 loop — it cannot
exist earlier, because there is no run to record until the loop runs.

**Independent Test**: `investigate replay --from <export> --layer trajectory` reports
`identical: true` and exits 0 with the network off; mutating one recorded byte makes it exit 4
naming the first diverging record.

**The first recorded trajectories, and what they do not yet prove (2026-09-18, Track E).**
`fixtures/incidents/rollout-regression-01-incident/trajectories/` holds two runs, both produced by
`aisre fixture record-trajectory`:

- `run-e034e9552165…` is **model-free and holds zero `model_request` records**. There is no
  `ANTHROPIC_API_KEY` in this environment and no live run has been made, so the engine ran with no
  model client: the provisional ranking, the onset estimate, the causal ordering, the deterministic
  first wave, a typed stop. The gate over it still exercises the worker, ledger and stop paths byte
  for byte. **A model-bearing trajectory from a live run replaces it** the first time anybody
  records one, and that is the point at which this note is deleted.
- `run-ddf8646113c7…` carries three model exchanges from a `modeltest` transport through the real
  `model.Client` and the real RoundTripper seam, so the **model-request digest matching path** the
  gate is built on is exercised by the corpus and not only by a unit test.

Both re-record byte-identically. Track E recorded three findings here — two engine defects shimmed
in `internal/cli/fixture_trajectory.go`, and a miss rate of 1.0 against the recorded world. **All
three are closed by Track F below**, the shims are deleted and both trajectories are re-recorded;
the run ids in this paragraph are Track E's and no longer exist.

**Phase 7 Track D (2026-09-18)** — the `server.Runner` adapter and the store follow-ups Phase 6
flagged. Not a task of its own: it is what makes T089's service do anything, and it is recorded
here so the next reader knows where the assembly lives.

- **`internal/investigation/runner`** (new): `runner.New(runner.Config)` returns the `server.Runner`
  `serve --enable-investigation` now passes to `NewInvestigationService` — it used to pass `nil`, so
  `Investigate`, `Declare`, `Reopen`, `Export` and `Replay` all answered Unimplemented.
  `Investigate`/`Declare` run one pipeline: normalise (both front doors) → resolve → group → open
  (idempotent on the published key, FR-008b) → consult the extent → publish the provisional
  prior-only answer (FR-046a) → run the engine → persist ledger, evidence, worker and model calls,
  spend and the layer-1 trajectory → emit exactly one decision record → conclude. `Reopen` is the
  same pipeline from a concluded parent, linked by `reopens_investigation_id`, with the fact in the
  child's ledger as evidence weighted `strong` (FR-057a, FR-057b). `Export` and `Replay` delegate
  to Track E's `replay.Export`/`replay.Replay`, with the runner implementing `replay.ExportSource`.
  **A nil model client is a supported configuration**: every test in the package runs model-free,
  with no API key (FR-067).
- **`serve --enable-investigation`** builds it. The model client is created only when
  `--model-config` is given *and* `$ANTHROPIC_API_KEY` is set; otherwise the server logs a notice
  and runs model-free rather than failing on its first turn. `--recording-root` doubles as the
  place a recorded `world/` is read from, since contracts/cli.md gives `serve` four flags and none
  of them names a telemetry source. A deployment with no world gets
  `runner.UnconfiguredTelemetry`, which declares every telemetry term and refuses each with a
  reason, so a configuration gap is recorded evidence rather than an engine failure (FR-027).
- **Migration 0008** adds `investigation.evidence_items.source_of_truth` (FR-022: the declaration
  is stored rather than re-derived from a registry that may have moved) and
  `investigation.refuse_update_of_concluded()`, the trigger data-model.md already specified for
  FR-007. Its ordering consequence: the decision record is emitted *before* the conclusion.
- **`ledger.Exonerate`** kept its rationale in the hypothesis's rationale string, which every later
  `recompute` rebuilt from the rows and therefore erased. The reason is now held per exoneration
  and re-rendered; `Ledger.ExonerationReason` reads it back.
- Known gaps, left deliberately: the engine's `Run` owns its own phase order and takes no per-turn
  callback, so the runner emits the provisional answer and then the concluded one rather than a
  state per turn — the anytime *shape* holds, the granularity does not, and closing it needs an
  engine hook. The recorded world of `rollout-regression-01-incident` is centred on the estimated
  onset (14:20) with a 900 s grid, which the engine's own query plan cannot reach: its onset search
  window is the investigation window and every recorded onset term spans 14:05–14:35, so a run over
  that fixture answers `not_recorded` throughout. Aligning `worker record`'s grid with the engine's
  plan is Phase 7/8 work in files Track D does not own.

- [x] T093 [US3] *(done 2026-09-18: `internal/investigation/replay/recorder.go` — `trajectories/<run-id>.jsonl`, one canonical-JSON record per line, the run id derived from the trajectory's own digest so a re-record of an unchanged fixture writes the same bytes to the same path. `Write` calls `engine.Trajectory.JSONL()` rather than re-implementing the rendering. Two normalisations, both forced by the engine as it stands and both documented in `Normalize`: `duration_ms` is cleared (the rule `ResponseDigest` already publishes), and each maximal block of worker records is emitted in canonical request-digest order, because `RunFirstWave` issues its calls in goroutines and the arrival order is the scheduler's, not the run's. Reader validates sequence, types, pairing and the single terminal stop)*  Implement `internal/investigation/replay/recorder.go`: layer 1 as `trajectories/<run-id>.jsonl`, one canonical-JSON record per line in **issue order**, typed `model_request`, `model_response`, `worker_request`, `worker_response`, `ledger_update`, `human_fact`, `stop`; model records carry the exact request body (messages, tools, system, `output_config`, betas) and the exact response (content blocks, `usage`, `stop_reason`); canonical digests computed with 001's serializer — sorted keys, RFC 3339 UTC, six-decimal floats, stable ordering → FR-033, FR-036, FR-042a; fixture: unit
- [x] T094 [US3] *(done 2026-09-18: `internal/investigation/replay/replayer.go`. Layer 1 re-issues every recorded model request through `model.NewReplayingTransport` — a real RoundTrip against the strict replayer — re-keys every worker request through `backend.TermKey`, recomputes every answer's digest with `pkg/backend.ResponseDigest`, cross-checks it against `world/` where the world holds the term (a miss is counted, never improvised), and re-derives every judgment's ln LR from the published table. A last whole-file check refuses a recording whose content no longer hashes to its own name, which is what catches an edit to a field nothing keys on. `investigate replay --from <dir>` now runs **locally** by default (a recording is a directory; `--remote` asks a server), prints `identical=true` and exits 0; a divergence exits 4 naming the first diverging record. Layer 1 deliberately does not re-run the engine: graph answers are not recorded by design, so an engine re-run needs Postgres and the per-PR gate must not)*  Implement `internal/investigation/replay/replayer.go`: the re-issue path that **re-issues nothing** — each `model_request` matched against the next recorded one by canonical digest and the recorded response returned, every worker request served from the recording, **zero network**; an unmatched worker request fails the replay naming the worker, capability and parameters; falling back to live, returning empty or substituting a similar recording is impossible; any divergence is reported as reasoning-layer divergence **identifying the first diverging record**, and `investigate replay` exits 4 → FR-039, FR-040, FR-041, SC-003, SC-018; fixture: rollout-regression-01-incident
- [x] T095 [US3] *(done 2026-09-18: `internal/investigation/replay/export.go` + the existing `investigate export <id> --out <dir>`. The artifact is `investigation.json`, `events.jsonl`, `trajectories/`, `world/` and `export.json`, whose per-file digests bind it and which refuses an artifact that has drifted. `ExportSource` is a four-method interface the runner satisfies, so `replay` stays free of the store. The rebuild test is written in exactly the form the task asks for: export, `DROP SCHEMA investigation CASCADE`, forget migration 0006, re-migrate to an empty schema, replay the event log into the graph and answer every manifest query from it, then `investigate replay --from <dir>`. Writing the rows back into the rebuilt schema is the runner's half)*  Implement `internal/investigation/replay/export.go` and `investigate export <id> --out <dir>`: the self-contained artifact — decision record, hypothesis ledger, **both** recording layers, and the graph events needed to answer the investigation's graph queries — plus the rebuild test that drops schema `investigation` and reconstructs every row **from the event log plus a regenerable world** — the world regenerated from the recorded backend responses, which are test data and not a source of truth — with `investigate replay --from <dir>`. Assert it in exactly that form, because it is what keeps the graph the source of truth and is the whole justification for the second schema under principle I (plan §Complexity Tracking, analyze C3): a rebuild that needed anything the event log and the regenerable world do not hold would mean the investigation store had become a substrate of its own → FR-042, SC-011, data-model §Derived structures; fixture: rollout-regression-01-incident
- [x] T096 [US3] *(done 2026-09-18: `VerifyOptions.TrajectoryOnly` and a new `trajectory-replay` step in `internal/fixture/verify.go`; `fixture verify --trajectory-only` skips the three Postgres-backed 001 steps and needs no DSN at all, so the gate runs in a job with neither Postgres nor egress. The `incident:` block was already parsed in full by `incident.go` (T097) and is extended rather than forked. `internal/fixture/calibration.go` holds the maths — reliability table over the five published buckets, observed accuracy against `ground_truth`, Brier and log-loss, buckets under 20 hypotheses reported as under-powered rather than scored — and says so in a comment for T110, which calls the same functions over live runs. `fixture calibration --fixtures … --out … --summary …` is its CLI edge)*  Extend `internal/fixture/{manifest.go,verify.go}` for incident fixtures: parse the `incident:` block (`question`, `ground_truth`, `world`, `runs`), the `world/` and `trajectories/` directories, keep **everything 001 already does** to the directory (replay from empty, manifest queries at their instants and again observed-time-pinned, byte-compare both, double-deliver, shuffle), then run trajectory replay of every file in `trajectories/` — **no new input format** → FR-063, FR-058, contracts/incident-format.md; fixture: rollout-regression-01-incident
- [x] T098 [US3] *(done 2026-09-18: `ci.yml`'s `trajectory-replay` job builds the binary, proves its sandbox has no route (`unshare --map-root-user --net` + a `/dev/tcp` probe that must fail), then runs `fixture verify --trajectory-only` over every `fixtures/incidents/*/` that has a `trajectories/` directory and `fixture calibration` inside the same namespace, appending the markdown to `$GITHUB_STEP_SUMMARY`. `unshare -rn` was chosen over a firewall action or an egress proxy because it is in util-linux on `ubuntu-latest`, needs no privilege the runner does not already give, and is the same command quickstart §3 tells a reader to run — a namespace with no interface has nothing to trust. The mutate-one-byte proof is in Go, not CI: `TestOneMutatedByteIsADivergenceNamingTheFirstDivergingRecord` over four kinds of mutation, plus `TestAMutatedRecordingExitsFourAndNamesTheFirstDivergingRecord` and `TestInvestigateReplayExitsFourOnADivergence` for the exit code)*  Wire the **trajectory-replay gate** into `.github/workflows/ci.yml` as `fixture verify … --trajectory-only`: every incident fixture's recorded trajectory replays byte-identically with zero network on every pull request; the job fails on any divergence or any improvised response, and network egress is blocked for the job so "zero network" is enforced rather than asserted. In the same job and on the same pull request, **compute calibration from those recorded trajectories with zero model calls** — the reliability table over the five buckets with observed accuracy per bucket, Brier and log-loss, under-powered buckets reported as under-powered — and **publish it in the job summary**. This is what satisfies constitution §Replay CI (c) on *every* run; the live-run calibration over the corpus × k stays in `eval.yml` (T110, T113) → FR-042a, FR-023, SC-003, SC-004, SC-018, constitution §Replay CI; fixture: all incident fixtures

**Phase 7 Track F (2026-09-18)** — closing the corpus-alignment finding, the two shims and the
replayer digest. Not a task of its own: it is what makes T093–T098's gate measure something.

- **The finding is resolved. `fixture verify --trajectory-only` now reports `0 not_recorded of 14
  checked, miss rate 0.0000`** on `rollout-regression-01-incident`, against the fixture's declared
  threshold of 0.05. It was 15 of 15 and 1.0000.

  The cause was two derivations of "the window". The engine's `onset` term searched
  `[fired_at − lookback, fired_at]`; `fixture record-world` recorded its `onset` terms over the
  manifest grid's window. Different keys, so `not_recorded`, so no onset estimate, so the engine
  fell back to the alert instant and every comparison after it missed as well — one term's miss
  cascading into all of them, with nothing in either file that was wrong on its own.

  The fix is that **the recorder no longer derives the engine's windows, it runs the engine**.
  `fixture record-world` makes an engine pass before the cross product — the same wiring
  `fixture record-trajectory --model-free` uses, via one shared builder (`newFixtureEngine`), over
  the same replayed graph and against the world's own generator — and records every term it
  issues. The world is a superset of the engine's plan by construction. The window arithmetic that
  is genuinely shared is exported once in `internal/investigation/engine/plan.go`
  (`OnsetSearchWindow`, `CompareHalfWidth`, `ComparePairs`, `ReferenceInstants`) and called from
  both sides; `internal/cli/fixture_trajectory_test.go` asserts the containment directly so it
  cannot lapse back.

- **Both shims are deleted** (`refResolvingGraph` is gone from `internal/cli/fixture_trajectory.go`).
  `engine.DecodeCall` stamps `as_of` on every graph read from the instants it already held, and the
  engine learns the canonical-entity-id ↔ reference translation from a neighbourhood read it now
  makes first — so `target_entity_ids` reach `pointers` as references and `error_spans` names its
  ends by entity id, which is what the term means by those fields. It also names the edge's
  published type instead of assuming `calls`. Regressions in
  `internal/investigation/engine/identity_test.go`.

- **The replayer digest defect was `mode`, not `duration_ms`.** `pkg/backend.ResponseDigest` hashed
  the response's `mode`, while the world recorder cleared it before hashing and every reader stamps
  its own afterwards — so a runner-recorded answer claimed a digest its own content no longer
  hashed to. The rule is now one function, `pkg/backend.NormaliseForDigest` (clears
  `response_digest`, `duration_ms`, `mode`), published in `contracts/telemetry-backend.md` §5.1
  because a third-party backend must match it. Existing worlds were unaffected: the recorder was
  already clearing all three on the way to disk. `runner_test.go` now asserts `identical: true`
  where it used to accept "identical or a typed divergence" — that wording was accommodating this
  defect, not deferring to the replay gate as its comment claimed.

- **The anytime stream is per-turn** (FR-046a). `engine.Config.OnState` is called after the
  provisional ranking, after the first wave and after every model turn; the runner renders each
  snapshot through the same `Investigation` view the final answer uses. Observed on the fake-model
  run: 6 states over 3 model turns, evidence counts `[6 9 17 17 17 17]` — monotone, so a
  subscriber renders the latest rather than reconciling it.

- **Calibration is scoreable from recordings.** `StopRecord.final_ledger` (additive; `buf breaking`
  passes) carries the belief state a run ended at — hypothesis, posterior, published bucket,
  status, and every reference the graph publishes for the candidate change. `fixture calibration`
  scores from it and reports **Brier 0.004935 · log-loss 0.054990 over 11 hypotheses**; it was
  0 hypotheses and no score. Every bucket is still under-powered against SC-004's bar of 20 and the
  table says so — that is the corpus being small, and it is what T110 grows.

- **The rebuild test's other half** (T095, SC-011):
  `TestTheInvestigationRowsRebuildFromTheEventLogAndARegenerableWorld` in
  `internal/investigation/runner` drops schema `investigation`, re-migrates
  to empty, re-runs the same question against the same graph and the same regenerable world, and
  compares every hypothesis with its posterior and bucket, every evidence item and every judgment
  against the exported ones. They match. **No substrate leak**: nothing in schema `investigation`
  needed anything the event log and the regenerable world do not hold.

**Checkpoint**: the plumbing gate is green in CI on at least one incident fixture; quickstart §3 reproduces in both layers.

---

## Phase 8: Adversarial fixtures, the metamorphic generator and the evaluation job (User Story 10)

**Goal**: fixtures that tell a good investigator from a lucky one, then the gate with its
**stated detection power**.

**Independent Test**: `fixture verify fixtures/incidents/* --report --report-json /tmp/report.jsonl`
followed by `./scripts/check-report.sh /tmp/report.jsonl` publishes every row of FR-059 and fails
only on the gated metrics, printing each gate with the regression it can actually detect.

### The corpus — the adversarial fixtures and the generator

The required-shape, conformance, declared-incident, human-fact and unobservable-remainder fixtures
(T097, T102–T106) moved to Phase 6 by analyze O1, because Phase 6 consumes them. What is left here
is the adversarial set — the fixtures on which the deterministic prior fails, which nothing before
Phase 8 needs — and the metamorphic generator.

  *Amended 2026-09-18 after the first run on origin*: `ubuntu-latest` (24.04) refuses `unshare --map-root-user`, and the no-route pre-check passed vacuously because a failing `unshare` also returns non-zero. The job now goes through `scripts/netns.sh` (unprivileged namespace when allowed, otherwise root creates it and `setpriv` drops back to the runner user) and the pre-check asserts the sandbox exists before asserting it has no route.
- [x] T099 [P] [US10] *(done 2026-09-18: the culprit is a ConfigMap edit lowering payments' connection pool at 11:32, exactly 3 h before the alert, with a 240 m lookback so the onset search reaches it; two fresher decoys inside the last half hour and a stale one 42 min before the onset put it FOURTH of four at prior score 0.276239 against the autoscaler's 0.613545, read off `golden/diff.checkout-diff.json` rather than estimated. The world holds the second trap too: payments' error-rate comparison referenced to the alert instant reads `flat` (25.9 % against 25.7 %, both halves inside the symptom) and referenced to the true onset reads `up` (1.07 % against 25.58 %). 294 world terms, miss rate 0.0000, two trajectories)* Hand-author `fixtures/incidents/slow-burn-01`: the culprit change ≈ 3 h before the alert, with its world and a `ground_truth` recording **the prior's own rank for the culprit** so lift is measurable where it matters → FR-062b, SC-021; fixture: slow-burn-01
- [x] T100 [P] [US10] *(done 2026-09-18: the chain is checkout → payments → ledger → fx-rates and the culprit is an fx-rates rollout at hop 3 — inside the candidate set by exactly the one hop `query.DefaultChangeHopMargin` allows past `engine.NeighbourhoodHops`, so the fixture tests ranking rather than exclusion. `world.hop_radius: 3`, the only fixture in the corpus that departs from the default, because at 2 the world holds no pointer on fx-rates and the ground truth would name evidence the corpus cannot produce; the manifest says so. Prior rank 4 of 5 (0.444209 against the autoscaler's 0.709365). 446 world terms, miss rate 0.0000, two trajectories)* Hand-author `fixtures/incidents/distant-culprit-01`: the culprit ≈ 3 hops from the alerting node, world recorded at a hop radius that contains it, prior rank recorded → FR-062b, SC-021; fixture: distant-culprit-01
- [x] T101 [P] [US10] *(done 2026-09-18: the decoy is the on-call's OWN remediation — a human rollout of inventory at 14:30, two minutes before the page, one hop out — and it takes the prior's top slot at 0.742753 against the culprit's 0.433966, a ledger rollout 25 min earlier two hops out; prior rank 4 of 5. The role is `candidate_effect` and the manifest argues it: it started 23 min after the onset and was caused by the incident, so the causal ordering is what exonerates it, while `shop/storefront@rev12` at 14:04 is the `coincident` three minutes the other side of the onset and has to be refuted on its own target — both exoneration routes on one graph. Per-decoy exonerating evidence is four predicate pairs over `onset`, `compare`, `errors_by_version` and `error_spans`, all checked against what `world/` actually holds. 384 world terms, miss rate 0.0000, two trajectories)* Hand-author `fixtures/incidents/adjacent-decoy-01`: a decoy deployed ≈ 2 min before the alert on an adjacent service, the true culprit earlier, exonerating evidence declared per decoy, prior rank recorded → FR-062b, SC-021; fixture: adjacent-decoy-01
- [x] T107 [US10] *(done 2026-09-18: `internal/fixture/derive.go` + `fixture derive`. The manifest is edited as a `yaml.Node` tree, not round-tripped through a struct, because half a fixture manifest is prose — the `note:` on the window grid, the `why:` on every decoy — and a struct round trip would delete all of it. `culprit-deleted` takes `decisive_evidence` to the union of the parent's exonerating predicates, which is what actually settles the question once the culprit is gone; `decoy-injected` writes its exonerating predicate over the manifest's own window grid so the regenerated world can answer it; `name-permuted` is a seeded derangement of the LAST ref segment only, so namespaces and revisions survive and `payments` ends up called something innocent. All four variants of `rollout-regression-01-incident` pass `fixture verify` with a 0.0000 miss rate, and deriving twice is byte-identical)* Implement `internal/fixture/derive.go` and `fixture derive <dir> --transform culprit-deleted|decoy-injected|time-shifted|name-permuted --out <dir>` as a **generator, not checked-in files**: each variant records its parent, its transformation and the published image of the parent's ground truth, so the invariant it is graded on is part of the transform's definition → FR-062a; fixture: rollout-regression-01-incident (as parent)
- [x] T108 [US10] *(done 2026-09-18: check logic here, rows in T109. It landed in `internal/eval/invariance.go` rather than `internal/fixture/report.go` — the check compares two graded `RunOutcome`s and `internal/fixture` sits below the engine, so it cannot see one. `CheckInvariance(parent, variant, transform, names)` returns a flat `InvarianceResult` with plain exported fields that T109's `InvarianceRow` reads. Measured end to end on the four generated variants: decoy-injected, time-shifted and name-permuted hold; **culprit-deleted does not** — with `shop/payments@rev7` removed the engine names the decoy `shop/inventory@rev8` instead of `unobserved`. That is the SC-020 failure the transform exists to find, and it is an engine finding rather than a generator defect)* Implement the metamorphic invariance checks in `internal/fixture/report.go`: culprit-deleted ⇒ the answer becomes `unobserved` **with the symptoms still localised** and naming a decoy fails; decoy-injected, time-shifted and name-permuted ⇒ the verdict does **not** change, a divergence failing the run and naming the variant — the permutation existing to test that the engine ranks on structure rather than on guilty-sounding names → FR-062a, SC-020; fixture: derived variants of rollout-regression-01-incident, slow-burn-01, adjacent-decoy-01

### The evaluation job

- [x] T109 [US10] [US8] *(done 2026-09-18: `internal/eval/{metrics.go,report.go,gaps.go}` — the rows are `{metric, scope: fixture|corpus, fixture, value, n, detail}`, one JSON document per line, with a **nullable** value: a corpus with no human labels publishes `human_agreement` as null with a reason rather than 0.00. `metrics.go` holds the arithmetic and knows nothing about how a run was produced, so every number is tested against an answer computed by hand (`report_test.go`: pass@1 = 2/3, a lift of exactly 0 from ranks 1/2/0 against a prior of 2/2/2, partial credit at 0.5 and 2/3, a harm case, an excluded fixture, the doc splice). `report.go` adapts Track H's `RunSet`/`GradeResult` through `RunRowsFrom`, and emits T108's `InvarianceResult`s as `metamorphic_invariance.<transform>` rows. **T018's wiring is `gaps.go`**: `audit.DetectGaps` is called on every run — not optionally, it is inside `Build` — and emits one `corpus_gaps` line plus a row per category. Two findings worth keeping: citation validity must **exempt the graph worker**, whose answers the recorded world deliberately does not hold (it replays from `events.jsonl` instead), or a sound corpus reports 66.7 %; and the miss rate excludes a fixture from every corpus aggregate while still publishing its own rows. Wired as **`aisre eval run`**, a new verb rather than a flag on `fixture verify`, so the per-pull-request gate stays runnable with no database and no egress — recorded in contracts/cli.md §Why the evaluation is its own verb)*  Extend `internal/fixture/report.go` with the investigation rows, **and wire T018's corpus-gap detection into this report writer** (the wiring half of T018, moved here by analyze O2): pass@1 and pass^k per fixture and over the corpus; lift over the prior (the investigator's MRR minus the deterministic ranker's on the same corpus and runs); harm rate; citation validity; the culprit's rank and top-k (reported, not the headline); localisation, attribution and mechanism scored **separately with partial credit** against `causal_path`; `not_recorded` miss rate; onset error wherever a fixture labels one; time to provisional, to first tested hypothesis and to conclusion; worker calls and backend quota share; cost; `unknown` rate and the precision of non-`unknown` outcomes; the replay-divergence rate; the agreement with human corrections; and the **corpus-gap line**, emitted by calling T018's detector with this run's audit and fixture set so a category of the unobservable remainder with no fixture is named on every evaluation run → FR-056, FR-059, FR-071b, SC-006, SC-008, SC-013; fixture: all incident fixtures
- [x] T110 [P] [US10] *(done 2026-09-18: the remainder — `CompareCalibration(current, previous CalibrationReport) CalibrationDelta` and `fixture calibration --previous <json>`. Pairing needed `Reliability.ByFixture`: the restriction to the shared fixtures has to happen BEFORE the average and an average cannot be un-averaged, so a report publishing only the aggregate could not be paired at all. Per-bucket deltas are nil where either side is under-powered. Two runs sharing no fixture are reported as not compared, with the reason, rather than as zero movement)* Implement calibration reporting in its own file, `internal/fixture/calibration.go` (its own file is what makes the `[P]` true beside T109, which owns `report.go`): a reliability table over the five published buckets with observed accuracy per bucket against that bucket's range, a Brier (and log-loss) score **paired against the previous version on the same fixtures**, and buckets holding fewer than 20 hypotheses reported as **under-powered rather than scored**. The same function serves both cadences: `eval.yml` calls it over live runs, and `ci.yml` calls it over **recorded trajectories with zero model calls** on every pull request (T009, T098) → FR-023, SC-004, plan F9, constitution §Replay CI (c); fixture: all incident fixtures
- [x] T111 [US10] *(done 2026-09-18: `internal/eval/{policy.go,knowability.go}`. `RunPolicy{Runs:3, ExtendTo:7}` extends once, on the first three verdicts, and never re-decides — a policy that kept extending while the answers disagreed would run until it was lucky. pass@1 is `RunSet.First()` and nothing in the package reduces the runs to a vote. `Grade` evaluates the `decisive_evidence` predicates against the digests the run actually **cited**, not against what the world holds, because those are different claims. Decoy reason mismatches are published as `DecoyReasonsMatch`/`PassWithReasons` rather than folded into `Pass`: naming a decoy is a wrong answer and belongs in the headline, calling one `inconclusive` where the table says `refuted` is a finding about how, and an evaluation that could not tell them apart would send people to fix the wrong thing)* Implement `knowability_time` grading and the run policy: asked before the knowability instant, `unknown` scores as a **pass** and naming the culprit as a failure; each fixture runs 3 times, extended to 7 **only when the first 3 disagree**; the model configuration used is production's and is recorded; majority-of-three is never the gate → FR-061, FR-061b, SC-007; fixture: all incident fixtures
- [x] T112 [US10] *(done 2026-09-18: `investigation_gate()` in `scripts/check-report.sh` reads the rows. `docs/evaluation/thresholds.json` holds `pass_at_1: null` until the first full corpus run; while null the gate prints `UNSET`, emits `gate pass_at_1 … status=unset`, annotates the job and is **neither a pass nor a failure**. Every other gate is in force from the first run, and `--investigation --set-threshold <rows> [--force]` is the once-only, by-hand publication, refusing a value above the audit's coverage ceiling (FR-071) and refusing to overwrite a published one. Detection power is a **Go helper**, `aisre eval power`, not awk: the statement needs a square root, a normal quantile and a bisection, and a second copy in the script would be a second definition of a rule stated once — it reproduces FR-061's published sentence exactly (40 trials: 90→70 needs n ≥ 20, 90→80 needs n ≥ 69). Two refinements the first run forced: a **direction** (`harm_rate` regresses by rising, so a min-only statement would have said "no regression is detectable" of it), and a `1 − α^(1/n)` form for the zero-tolerance gates, which fire on the first occurrence and whose real limit is how intermittent a defect they can miss (6.1 % at n = 48). A metamorphic gate with no variants prints `NOT RUN` with an annotation rather than passing. `internal/cli/eval_test.go` drives the real script over rows files and asserts exit codes and printed lines)*  Extend `scripts/check-report.sh` with the investigation gates and their **stated detection power**. The aggregate pass@1 threshold is **set by the first full corpus run and not before** (FR-060): until that run has published it, the gate prints the threshold as `unset` in the job summary and in the machine-readable report and does **not** pass silently as though it had been met, while every other gate below is in force from the first run. Then: fail on aggregate pass@1 below the published threshold once set, lift ≤ 0, harm rate above 5 %, citation validity below 100 %, any untraceable conclusion, any improvised replay, any metamorphic verdict change, any regression on a corrected or human-labelled case; report but never gate top-k, calibration, cost and timing; gate a **fixture's admission** on its miss rate and never the engine's score; **never** gate best-of-k; and print with each threshold the sentence "8 fixtures × 5 runs = 40 Bernoulli trials detects 90 % → 70 % reliably and cannot detect 90 % → 80 %" → FR-060, FR-061, FR-061a, SC-001, SC-016, SC-017, SC-018, SC-021; fixture: all incident fixtures
- [x] T113 [US10] *(done 2026-09-18: `.github/workflows/eval.yml` — nightly plus `workflow_dispatch`, a `public`/`private` matrix with `fail-fast: false`, a Postgres service, `aisre eval run` in world-replay mode, then `scripts/check-report.sh --investigation`. The private leg needs **both** `secrets.PRIVATE_CORPUS_REPO` and `secrets.PRIVATE_CORPUS_TOKEN`; missing either, it **skips visibly** with the specific reason in the job summary and in the release statement, and never as a pass. Without `ANTHROPIC_API_KEY` the job runs `--model-free` and labels every number in the rows, the summary and the artifact name, so it cannot be mistaken for the production result. `--batch` is passed and **reserved**: `internal/investigation/model` has no Batch API client (it speaks Messages and the streaming transport), so the flag is accepted, reported and does nothing — passing it now means the workflow needs no edit when the batch client lands, which the workflow comment says. A `release-statement` job composes both legs from their artifacts; the nightly on `main` opens or updates a pull request with the regenerated `docs/evaluation/investigation-metrics.md` through `peter-evans/create-pull-request@v7` and never pushes to main. `actionlint` clean. Quickstart §10 reproduces the public leg locally with `--model-free`)*  Complete `.github/workflows/eval.yml`: the corpus × k in **world-replay** mode with the production model configuration, live model calls, the **Batch API** where a run is not latency-bound, publishing `docs/evaluation/investigation-metrics.md`; nightly plus on demand; and the **private-corpus split** — the public leg runs the full gate on synthetic structural twins, the private leg runs the same job with the same thresholds on the recorded corpus, each run records which corpus it ran against, and a release statement names both results → FR-058, FR-060, FR-067, ADR-0003 D9, plan §Evaluation and CI; fixture: all incident fixtures

**Checkpoint**: ≥ 8 base fixtures plus their metamorphic multiples plus 3 adversarial fixtures run 3× each; the gate is published with its detection power; quickstart §10 reproduces.

### Phase 8 Track J (2026-09-18) — backend and estimator realism

Not a numbered task: this is the follow-up Track G recorded rather than worked around
(`/tmp/live/PHASE8_G_BLOCKED` names the three gaps exactly). All of it is in
`pkg/backend/synthetic` and `pkg/backend/onset`; no fixture, no proto and no manifest was edited.
Design notes are research.md §Backend and estimator realism; the estimator's published criterion
and defaults are contracts/telemetry-backend.md §"The onset digest, and when there is no onset to
report".

- **J1 — degradation shapes.** `synthetic.Change` gains `Factor`, `RampSeconds`, and
  `LoadCoupled`/`Capacity`/`CapacityFraction`; the scenario gains an optional `DiurnalAmplitude`
  so a demand ceiling has something to be crossed by. Every zero value is the step this generator
  has always produced, so a scenario that sets none of them is byte-identical. Named shapes live
  in `shape.go` — `step`, `slow-burn`, `distant-culprit`, `adjacent-decoy` — as Go values a
  recorder stamps onto the scenario it built from the graph, which keeps the manifest contract
  unchanged and keeps the numbers out of YAML where they would be tuned until a fixture passed.
  `slow-burn` is the only one of the three that needed new parameters; the other two needed J2.
- **J2 — propagation.** `E(v, t) = D(v, t)·(1 + Σ α·(E(c, t − Δ) − 1))`, α = 0.6 per hop, Δ one
  resolution step, bounded at four hops with a visited set. **Default on**, with
  `Propagation: NoPropagation` for the old series; the switch is a named two-valued type rather
  than a `bool` so that the zero value is the new default. Without it the **subject's own series
  is flat**, which makes `onset` — the first telemetry term every investigation asks — an estimate
  over seeded noise.
- **J3 — the onset criterion.** A CUSUM crossing is published as an onset only when the
  post-crossing level is ≥ `k_effect` = 3 scaled MADs of the pre-crossing segment from its mean,
  **and** ≥ `min_sustained_points` = 5 samples follow it; otherwise the typed
  `no_onset_detected`, confidence 0. Reproduced exactly from the recorded world: on
  `adjacent-decoy-01`'s checkout series the old estimator returned **13:27 ± 840 s** for a true
  onset at 14:07, on 1.30 MADs of effect. **No engine change was needed** —
  `Engine.EstimateOnset` already branches on `unavailable` and falls back to the alert instant
  saying so, which is the argument for a new reason rather than a new outcome.

**Owed, and not done here: every recorded world is stale.** All sixteen fixtures under
`fixtures/incidents/` need `fixture record-world`, and their trajectories re-recording after that.
Two independent reasons, so no fixture escapes: (a) propagation changes the error-rate series, the
log lines, the span counts, the monitor state and `errors_by_version` for every entity with a
callee or a caller; (b) `method_parameters` on **every** onset digest now carries `k_effect` and
`min_sustained_points`, which changes its `responseDigest` — and all sixteen worlds hold onset
records (3 to 45 each, 384 in total). `adjacent-decoy-01`'s manifest paragraph about the 13:27
estimate, and `slow-burn-01`'s about the step-not-a-ramp, both describe behaviour that no longer
exists and should be re-stated in the same re-record. The full Go suite is green against the
**current** recordings, because replay reads the recording rather than the generator: the staleness
surfaces on the next `record-world`, not in `go test`.


### Phase 8 Track K-A (2026-09-18) — the verdict rule, onset on candidate causes, decoy reasons

Not a numbered task: this is the engine half of the Track K follow-up, worked in
`internal/investigation/engine` and `internal/investigation/ledger` only (no fixture, proto or
manifest was edited). It answers the evaluation's two worst numbers — `confidently_wrong` at
18.75 %, and the metamorphic `culprit-deleted` variant of `rollout-regression-01-incident`
answering `k8s.change=shop/inventory@rev8` where the only correct answer is `unobserved`. The rule
is stated once in data-model.md §State transitions ("The verdict rule — prior-only mass is not
confidence") and read from there by the status machine, the reported bucket and the engine's
conclusion.

- **K1 — the verdict rule.** `ledger.EvidencedSupport` / `ledger.TelemetryEvidence`: a hypothesis
  may be named only with a `supports` judgment resting on telemetry evidence (`algebra_answer` or
  `onset_estimate` from a worker declaring a source of truth) or on a human fact. `supported` is
  refused otherwise (`unsupported_status`), so a prior and the graph answer that ranked a candidate
  can no longer name it. The **reported bucket is capped at `moderate`** for a hypothesis nothing
  observed supports (`ledger.UnevidencedCeiling`), the computed confidence untouched, exactly as
  under widening; the open hypothesis counts a telemetry-backed refutation of a rival as its own
  evidence, so a run that ruled things out may report `unobserved` above the cap and a run that
  tested nothing may not. `engine.Verdict` publishes the three arms — `named`, `unknown` while the
  run can still test, `unobserved` at a `completed` or `diminishing_returns` stop where nothing
  earned support — and `Engine.Stop` settles it: every still-`proposed` candidate is filed
  `untested` with its reason and next query, and the open hypothesis is named with the symptom
  still localised on the subject (`ledger.WithOpenStatement`). Tests:
  `TestWithTheCulpritDeletedTheVerdictIsUnobserved`, `TestTheCulpritIsNamedWhenTheEvidenceSupportsIt`
  (engine); `TestAHypothesisNothingObservedSupportsRendersNoHigherThanModerate`,
  `TestAGraphAnswerAloneNeverNamesAHypothesis`,
  `TestTheOpenHypothesisIsSupportedOnlyWhenNothingElseIs` (ledger).
- **K2 — onset on candidate causes.** `Engine.EstimateOnset` now estimates onset on the subject
  *and* on the targets of the top `OnsetCandidates` = 3 ranked candidates, cheap calls admitted by
  the budget like any other, and takes the **earliest confident** estimate as the ranking
  reference (ties to the tighter uncertainty). `Onset` carries `EntityRef`, `PointerID` and every
  estimate `Considered`, each an `onset_estimate` evidence item — which is what makes a decisive
  predicate written over the *cause's* `estimated_onset` satisfiable at all. The subject's estimate
  is still the fallback and the alert instant with the published "could not be estimated" line is
  still the last resort. Test: `TestOnsetIsEstimatedOnTheCandidateCausesToo`.
- **K3 — decoys get a stated reason.** A candidate whose own target is flat across the reference —
  every `compare` a digest that did not separate or moved down, and no `errors_by_version`
  concentration — is **refuted** on that silence (`flatTarget`, `refutes`/`moderate`, recorded
  against the first comparison rather than beside its neutral judgment), instead of being left
  `inconclusive` with nothing said. Candidates the wave does not test are filed `untested` with a
  reason and the exact next query: `StaleBefore` = 2τ = 3600 s before the reference, and the
  wave's own cap of `FirstWaveCandidates`. Test:
  `TestEveryDecoyLeavesTheWaveWithAStatedReason`.
- **K-B's horizon, one engine change.** `engine.ComparePairs(reference, observedAt, lookback)`:
  the symptom half never reaches past the investigation's `observed_at`. A pair before the horizon
  is unchanged, one that straddles it ends at it with both halves narrowed equally, and a reference
  at or after it slides the pair back. The first wave builds its comparison from the plan's pair,
  so the engine asks for the windows the recorder records. Test:
  `TestTheWindowPlanIsOneDerivation` (three cases).

**Waiting on the re-record** (fixtures untouched, as Track J's note already owes):
`TestThePublishedRulesReachTheGroundTruthOnTheRecordedWorld` in
`internal/investigation/engine/fixture_test.go` fails against the current
`rollout-regression-01-incident` world — its recorded compare, `errors_by_version` and onset terms
carry windows that reach past `observed_at`, which the horizon rule now answers `no_data`. The
recorded trajectories are stale for the same reason plus the new onset calls and the changed
ledger updates.


### Phase 8 Track K-B (2026-09-18) — the horizon, the grader's defects, the shapes and the manifests

Not a numbered task: the corpus-and-tooling half of the Track K follow-up, worked in
`internal/investigation/backend`, `pkg/backend/synthetic`, `internal/eval`, `internal/fixture`,
`internal/cli`, `scripts/check-report.sh`, `docs/evaluation/thresholds.json`, the two contracts
and the fixture manifests and READMEs. No `world/`, `golden/`, `trajectories/` or `events.jsonl`
was edited: the corpus is re-recorded after this track, and every number those files hold is the
re-record's to write.

- **K4 — nobody sees the future.** An investigation's `observed_at` is a horizon, and telemetry
  after it does not exist (constitution II). `internal/investigation/backend/horizon.go` publishes
  the rule — `ClampWindow`, `ClampTerm`, `ClampRequest`, `AnnotateHorizon` — and **both** backends
  obey it: a window straddling the horizon is truncated to it and says so in its own coverage
  block (`coverage.truncated_to_horizon`, `coverage.horizon`, and a `horizon:…` truncation
  criterion, two additive proto fields), and a term carrying any window wholly at or past it is
  `NO_DATA` naming the horizon. A term with a wholly-future window is refused rather than
  narrowed, because a window is non-empty by contract and there is no clamp of the future that is
  still a window; that is what made `engine.ComparePairs` take the horizon (Track K-A). The clamp
  runs **before** the answer is produced, so the generator generates no sample past the horizon
  and the term key an answer carries is the key of the window that was answerable — which is why
  `fixture record-world` now carries the horizon through both the engine pass and the cross
  product, and files answers under the clamped key the engine will ask for. A request naming no
  `observed_at` declares no horizon: a bench or a conformance run is not an investigation.
  Documented in contracts/telemetry-backend.md §2.1.
- **K5 — two grader defects and the ceiling's note.** (a) `eval.NameMap.Image` is keyed on bare
  names and was being handed full references, so a name-permuted variant that answered its own
  published culprit graded as a divergence; it now maps the name component inside a reference with
  the namespace, the path and the `@revision` carried through. (b) `fixture.Derive` removes the
  output directory it created when a transform is refused and returns a typed `fixture.Refusal`
  (`RefusalReason`) — a refusal used to leave an empty directory the next run refused for an
  entirely different reason. (c) `docs/evaluation/thresholds.json` carries a `note` stating FR-071
  in full: the published pass@1 may never exceed the audit's coverage ceiling (0.615385), and the
  cap binds the synthetic leg **by design**, because on generated telemetry every culprit is
  observable by construction. `scripts/check-report.sh` prints that note beside the ceiling guard.
- **K6 — the shapes are wired and the manifests reconciled.** `incident.world.shape` is an
  additive manifest key (default `step`, unknown names refused with the list), parsed in
  `internal/fixture/incident.go`, applied in `buildScenario`, documented in
  contracts/incident-format.md §`world:`. `slow-burn-01` declares `slow-burn` and its note is
  re-stated (about 3.8× at 11:32 rising to about 10.5× at 14:32 under a 0.6 diurnal amplitude, the
  onset still at the change instant); `adjacent-decoy-01` and `distant-culprit-01` declare `step`
  and the paragraph describing the 13:27 spurious onset as a recorded property is gone — the
  estimator refuses that crossing now (1.30 MADs under the `k_effect` floor of 3). Manifests whose
  decisive or exonerating windows ran past their own `fired_at` were rewritten to end at it, and
  `knowability_time` with them: `rollout-regression-01-incident` moves from 14:35 to **14:25:30**,
  the instant its own `otel:demo:ckpt:w1420` checkpoint — extent [14:20, 14:25), observed at
  14:25:30 — makes the decisive evidence satisfiable from data ≤ `fired_at`; the four fixtures
  that inherited 14:35 and the three `unobserved-*` ones move to 14:25 on the same reasoning
  (five sustained one-minute points after the 14:20 onset); `declared-incident-01` moves from
  14:47 to 14:32. `unknown-feeder-gap-01` and `telemetry-rejection-01` keep a knowability after
  `fired_at`, which is what those two fixtures are *for*. `two-simultaneous-01`'s
  `prior_rank_of_culprit` is 1 → **2**: its own `golden/diff.checkout-diff.json` scores both twins
  at 0.61016 and the `change_id` tie-break puts the ConfigMap edit first.
  `fixtures/incidents/README.md` §"How the synthetic telemetry travels…" is rewritten to the
  propagation formula and the onset criterion.

### Phase 8 Track K-C (2026-09-18) — the re-record and the first published baseline

Every `world/` and `trajectories/` layer under `fixtures/incidents/` was re-recorded against
K-A's and K-B's engine, and the three `# re-read after re-record` fixtures had their predicates
re-read off the world the re-record wrote. The corpus is 16 fixtures, **5048 recorded terms**
(from 4936, redistributed: `declared-incident-01` 288 → 156 as the horizon rule refuses the
window grid's wholly-future symptom half, `distant-culprit-01` 446 → 523 and
`rollout-regression-01-incident` 565 → 608 as onset-on-candidates adds terms), 32 trajectories,
world miss rate 0.0000 everywhere. Three predicates moved, each to what the world holds:
`adjacent-decoy-01`'s payments `ERROR_RATE` direction `flat` → `up` (0.8431 % → 11.7089 % over
the pair ending 14:22), `distant-culprit-01`'s the same `flat` → `up` (0.4948 % → 4.0728 %), and
`slow-burn-01`'s `errors_by_version[rv5512].error_rate` floor `> 0.05` → `> 0.04` (the world
reads 0.042640 over 11:32..11:47, so the 5 % floor was simply false under the `slow-burn` shape;
`knowability_time` stays at 11:47 because 4 % is cleared inside that first quarter-hour).

One defect was found and fixed, in `pkg/backend/synthetic` and
`internal/investigation/workers/metrics`: the generator set `Comparison.separable` from
`direction != "flat"` (its ±0.1 direction threshold) while the metrics worker's `annotate` hook
re-states it at the published `SeparationThreshold` of 0.20. Two spellings of one rule, and where
they disagreed the worker mutated a digest `backend.NewResponse` had already hashed, so the
recording carried a `response_digest` its own content did not hash to. `slow-burn-01`'s 12.1 %
ramp is the first comparison in the corpus where they disagree, and it failed the trajectory
replay. The rule is now exported once as `metrics.Separates(relativeDelta)` and the generator
calls it.

**Phase 8 baseline (model-free, 2026-09-18).** `eval run --runs 3 --model-free` over the 16
fixtures with 47 derived metamorphic variants, gated by `scripts/check-report.sh
--investigation`: **pass@1 0.5625** over n=48 (the threshold stays UNSET — it is published once,
by hand, by the first full corpus run, and FR-071 caps it at the audit's 0.615385); **lift over
the prior +0.078125** (investigator MRR − prior MRR, gated `> 0`); **harm rate 0.0 %**;
**confidently wrong 0.0 %**; citation validity 100 % over 666 citations (663 after Track K-D); untraceable conclusions
0; improvised replays 0; replay divergence 0; **metamorphic verdict changes 0 of 47** — all five
culprit-deleted variants answered `unobserved` with the symptom still localised, and the sixteen
name-permuted and sixteen time-shifted variants produced their parents' verdicts with no false
failures; corpus gaps 0. Reported, never gated: pass^k 0.5625, culprit rank 0.6875, top-k 0.5,
localisation 0.9375, attribution 0.6875, mechanism 0.4479, `unknown` rate 0. **Calibration Brier
0.107598**, log-loss 0.626801 over 104 hypotheses from the 32 recorded trajectories, with the
three upper buckets under-powered at n < 20. Corpus size: 16 fixtures, 5048 terms, 32
trajectories, 25 MB. The two adversarial fixtures the engine still misses — `adjacent-decoy-01`
and `distant-culprit-01`, culprit ranked second on both — each still lift +0.25 over a prior that
ranked it fourth; `slow-burn-01` lifts +0.75 and passes. Nothing was tuned to reach these
numbers.

### Phase 8 Track K-D (2026-09-18) — the admission race closed, and the corpus re-recorded on it

K-C left one defect open, and this track closes it. `budget.Manager.Admit` decided and **booked
nothing** — the counters moved only in `RecordWorkerCall`, after the call had returned — while
`engine.RunFirstWave` issued the candidates' telemetry calls concurrently. The per-cost-class cap
was therefore a check-then-act race: `rollout-regression-01-incident` plans 7 `expensive` calls
against the 6 the reserve leaves of 8, and about 10 runs in 12 admitted all seven.

Both halves are fixed, because either alone still leaves the outcome to the scheduler.
**Admission books.** `Admit` increments the call's cost class, backend, worker and share of the
backend's reported quota — and, for a model call, its pre-flight token estimate — under the same
lock that read them, and returns a `budget.Reservation`. `RecordWorkerCall` reconciles it:
release the estimate and book the actual, both under one lock, so no admission can observe the
moment between them; a call that is never issued releases its booking and the headroom returns
(`internal/investigation/budget/reservation.go`). **Admission is ordered.** `RunFirstWave`
pre-admits every planned request sequentially, in the wave's own plan order — candidate rank,
then query order within a candidate — before fanning out, and then issues the admitted set in
parallel exactly as before (`engine.preAdmit` / `engine.issueAdmitted`). Which call loses when a
cap binds is now a function of the plan and never of goroutine scheduling, and a refused request
is filed with its typed reason; a candidate that the refusal left with no judgment at all is
recorded `untested` with that reason and the exact next query (FR-031, FR-047a).

Closing the race exposed a second defect it had been masking. A per-cost-class refusal called
`refuseIntoReserve`, so the first wave spending the unreserved 6 of 8 `expensive` calls flipped
the **whole** engine into `synthesis_only` — and `Run` consults `budgetStop()` before turn 1, so a
run with a model configured stopped `budget_exhausted` having said nothing, and the same evidence
that names `payments@rev7` model-free scored `unknown`. Under the race that branch appeared in
about one run in six; deterministic, it would have been every run. FR-045 makes termination follow
"exhaustion of any budget", and the unreserved portion of one class is not that budget's
exhaustion — it is the point past which that class is reserved; and the closing synthesis the
reserve exists to pay for (the verifier pass, the render, the stop reason) spends no `expensive`
worker call, so holding 15 % of that class back buys the synthesis nothing. A class-cap refusal is
therefore **local**: the call is not issued, the intent is filed, the hypothesis is reported
untested, and the mode moves only when **no** class has unreserved headroom left, which is
evidence gathering genuinely being over. Wall time, model tokens and the per-backend cap are
unchanged — those the synthesis does spend.

Two tests hold it. `TestConcurrentAdmissionsNeverExceedTheCap` runs 7 concurrent admissions
against a cap of 6, two hundred times, and admits exactly 6 every time — with the booking removed
it fails inside the first ten rounds. `TestTheFirstWaveAdmitsTheSameSetEveryRunWhenACapBinds` runs
the wave 20 times under an operator cap chosen to bind inside the wave and nowhere earlier, and
asserts the admitted set, the refused set, the ledger digest, the trajectory digest and the call
book are identical every run. `go test -count=10 ./internal/eval/ ./internal/investigation/engine/
./internal/investigation/budget/` is green.

**What the re-record changed.** All sixteen fixtures were re-recorded; exactly one moved.
`rollout-regression-01-incident`: world **608 → 607 terms** (the refused query is no longer
enumerated), model-free trajectory `605f2469e363` → `289e4ad69b3f` (27 → 26 worker calls, the
correct 6 of 8 `expensive`), fake-model `6c57e83da859` → `1106981d140b` (26 worker calls, 3 model
exchanges, stop `completed` — not `budget_exhausted`). Twelve consecutive `record-trajectory
--model-free` runs on it wrote no new file and the same bytes every time. Corpus: **5048 → 5047
terms**. `fixture verify` is green on all sixteen.

**Phase 8 baseline, revised (model-free, 2026-09-18, Track K-D).** Everything gated is unchanged:
pass@1 **0.5625** over n=48 (threshold still UNSET), lift **+0.078125**, harm rate 0, confidently
wrong 0, citation validity 100 %, untraceable conclusions 0, improvised replays 0, replay
divergence 0, metamorphic verdict changes 0 of 47, corpus gaps 0. Three numbers moved with the one
refused call: **citation validity is now over 663 citations** (was 666), **`worker_calls` 666 →
663**, and **calibration Brier 0.107598 → 0.106447** (log-loss 0.626801 → 0.623145) over the same
104 hypotheses. Reported-never-gated is unchanged: pass^k 0.5625, culprit rank 0.6875, top-k 0.5,
localisation 0.9375, attribution 0.6875, mechanism 0.4479, `unknown` rate 0.

---

## Phase 9: Polish & Cross-Cutting Concerns

- [x] T114 [P] *(done 2026-09-18, Track N: `docs/schema/algebra.md` gained the horizon rule and the shared window plan (`OnsetSearchWindow`, `ComparePairs`, `CompareHalfWidth`); `digests.md` gained the horizon coverage fields, the digest rule (`NormaliseForDigest`) and `Separates`; `ledger.md` gained the verdict rule and the `moderate` unevidenced ceiling; `docs/schema/investigation.md` is new — the five event bodies 21–25 with their idempotency keys, the derived-schema argument and the two recording layers. `docs/evaluation/investigation-metrics.md` is Track M's; `docs/schema/README.md` gained an Evaluation pointer to it and to `thresholds.json` instead)* Write the schema documentation: `docs/schema/algebra.md` (the terms and why the algebra is the only vocabulary), `docs/schema/digests.md` (digest shapes, coverage, join keys, drill-downs, the six typed outcomes), `docs/schema/ledger.md` (judgments, the LR table, the posterior rule, the buckets, `π₀` and its audit), `docs/schema/investigation.md` (the events this feature emits and their idempotency keys), and update `docs/evaluation/investigation-metrics.md` with the gate and its detection power → FR-068, FR-023, FR-059, constitution IX; fixture: n/a
- [x] T115 [P] *(done 2026-09-18, Track N: `writing-a-worker.md` revised — the `pkg/feeder`/`pkg/worker`/`pkg/backend` rhyme table, the registry as the gate, `Request`/`Response`, the `After` hook's two rules, a redaction-contract section, and the backend half split out; `docs/connectors/writing-a-backend.md` is new. The fresh-reader pass was run against `examples/mcp-feeder`'s structure: it found no "getting the SDK" or "where the code lives" section, no statement that `NewResponse`, the horizon clamp and the window constructors are `internal/`-only (so an out-of-tree backend cannot yet follow the page), no named onset entry point, and no small path to a `testdata/world`; all four are now in the docs. `checklist.md` gained §7 worker and §8 backend)* Write `docs/connectors/writing-a-worker.md` and `docs/connectors/writing-a-backend.md`: the `pkg/worker` and `pkg/backend` idioms as they rhyme with `pkg/feeder` (`Describe`, an entry point, a `Recorder`, a `testkit`), the declaration rules, the redaction contract and the "recorded responses as your test" rule; timed by a fresh reader and revised → FR-010, FR-068, SC-003; fixture: unit
- [x] T116 *(done 2026-09-18, Track O: `docs/security/report-delivery-review-2026-09-18.md` — threat model, nine checks, four defects fixed with tests. The sink keyed its remembered message on the target alone, so two investigations of one channel shared a message; a panicking connector returned the zero result, an outcome the ledger refuses to store; a credential minted for one place could write to another; and there was no sink registry at all, so an unknown transport would have been served by whichever sink was compiled in. Added `render.DeliveryIdentity` (pinned to `store.DeliveryIDFor` by a test), `ErrCredentialTarget`, `NewSink`/`PublishedSinks` and `investigate report --sink`. The `investigator` role's reach is now a test: graph reads yes, ingestion and entity resolution no, report sink nowhere on the wire. The grep guard gained `api-key`, `people-identifier` (with a closed allow-list: entity keys and RFC 2606/6761 documentation names), `raw-span-payload`, `telemetry-instance-id` and `unredacted-log-line`, and `scripts/check-no-secrets_test.sh` plants one sample of each and fails if any rule stays silent; `ci.yml` runs both on every pull request)* Security pass: review the **first write path** (report delivery) against constitution VII — one target, one message edited in place, a credential that can do nothing else, separate from every read credential, non-blocking, recorded — and the new `investigator` role's scopes; extend the CI grep guard so no secret value, no people identifier and no raw telemetry body appears under `fixtures/incidents/**` or any checked-in recording; verify the engine refuses write-scoped and anonymous credentials → FR-008, FR-038, FR-066, ADR-0005 D8, plan F6/F10; fixture: all incident fixtures (grep guard)
- [x] T117 *(done 2026-09-19, Track M: `docs/benchmarks/investigation-page-profile-2026-09-18.md`. Every target met over 48 live runs of the 16-fixture corpus: provisional ranking within 5 s in 100 %, first tested hypothesis within 120 s in 100 %, substantive answer within 5 min in 97.9 % (the one exception is a run of `two-simultaneous-01`, the not-separable fixture), nothing past the 10-minute hard stop, ledger recomputation 436 µs best-of-five against a 10 ms budget, trajectory replay 0.110–0.237 s per fixture against 5 s. The 120 s target is the first wave's: model-free, with no model call at all, the first tested hypothesis lands in 17 ms — and live, twelve of sixteen fixtures reach it in ~30 ms because the first wave got there before the first model turn returned. The three `time_to_*` rows read `n/a` before this because they were computed from the recorded instants, which a fixture run pins to the alert instant so a re-record is byte-identical; `Trajectory.Elapsed` now keeps the wall-clock offsets beside the recording, and `internal/eval/report.go` publishes the `*_within_target` shares and `runs_past_hard_stop` beside the means, because SC-013 is stated as shares and a mean hides the runs the target is about)* Measure the `page` profile end to end and record it: provisional ranking within 5 s in ≥ 95 % of runs, first **tested** hypothesis within 120 s in ≥ 90 %, substantive answer within 5 min in ≥ 90 %, nothing past the 10-minute hard stop, ledger recomputation < 10 ms for 50 × 500, trajectory replay < 5 s; publish to `docs/benchmarks/` and confirm the first wave — not the model — is what carries the 120 s target → SC-005, SC-013, plan F2; fixture: all incident fixtures, bench
- [x] T118 *(done 2026-09-18, Track O: run in a `git worktree` at HEAD against a fresh database with no `MISTRAL_API_KEY`/`ANTHROPIC_API_KEY`; every command and outcome recorded in `specs/002-investigation-engine/quickstart-run-2026-09-18.md`, linked from the quickstart header. Eleven drifts: nine in the doc (`dev-token --dev`, `serve --listen` loopback, the `rollout-regression-01-incident` path, `investigate --recording` which has never existed — the world is chosen by `serve --recording-root`, one database per fixture, the `worker call` argument spelling, `--cap`, the `--report-json` path and key, the `telemetry_payload` grep, the stale pass@1 figure) and two in the code: `investigate get` returned no ledger at all, so `--ledger/--chain/--evidence` and four quickstart sections were empty (`server.WithLedgerReader`, `TestGetReturnsTheLedger`), and `investigate fact` did not reopen a concluded investigation although FR-057b, the CLI contract and its own `--help` all said it did (`TestFactAgainstAConcludedInvestigationReopensIt`). Three findings left for other tracks: `to-incident` writes no `manifest.yaml`, an export cannot carry the world layer of a run served from a fixture's world, and `cache_read_ratio` is documented and unimplemented. The whole corpus evaluation runs model-free and every gate passes: pass@1 0.5625 over n=48, lift 0.078)* Run `quickstart.md` §1–§11 and §1a end to end on a clean machine with no vendor account; fix drift; record the run → FR-067, SC-011, constitution VIII; fixture: all incident fixtures
- [x] T119 [P] *(done 2026-09-18, Track N: ADR-0003/0004/0005 are **Accepted** with dated "Realised by" paragraphs naming the packages that realised each decision; ADR-0006 (the 001 change package) did not exist and was written from spec §Dependencies-on-001 and plan §"The feature 001 change package", Accepted, realised by migration 0005 and the graph proto additions; `ADR-0007-report-delivery-write-path.md` records the first write path per constitution VII v1.1.0 and plan F6; `ADR-0008` records the 2026-09-18 second-provider decision. `ls docs/decisions` is 0001–0008)* Update ADR statuses: ADR-0003, ADR-0004 and ADR-0005 → accepted with the implementation that realised them; ADR-0006 (the 001 change package, T032) → accepted; and open `docs/decisions/ADR-0007-report-delivery-write-path.md` recording the project's first write path **before** T087 merges, per plan F6 → constitution IX; fixture: n/a

### Track M (2026-09-18): live-coverage worlds, the live eval, and four defects a deterministic corpus hid

T117 and the corpus behind it, run in parallel with Tracks N and O and touching only
`fixtures/incidents/**`, `internal/cli/{fixture_record_world.go,fixture_derive.go,worker_record.go}`,
`internal/eval/**`, `internal/investigation/{engine,model,replay}/**`, `pkg/backend/record*.go`,
`contracts/{incident-format.md,prompting.md}`, `docs/benchmarks/**`,
`docs/evaluation/investigation-metrics.md` and these task lines.

The first live evaluation of this corpus excluded **15 of its 16 fixtures** on their `not_recorded`
miss rate, and nothing about that was a defect in the model, the engine or the worlds. The worlds
were recorded from the deterministic plan and the manifest grid; a live investigator asks other
questions. `fixture record-world --live-passes N` now runs the production configuration against the
same generator N times and files every term it issues, declaring the result in `world/index.json`
as a `liveCoverage` block — the pass count, the model configuration digests, and the term keys —
so that a later `--live-passes 0` re-record carries the coverage forward by re-filing those exact
answers and writes the same bytes. Miss rates fell from 0.111–0.667 to 0.016–0.185 and **eleven of
the sixteen fixtures are now admitted** at n = 33, with every gate passing: pass@1 0.636 (no
threshold published), lift +0.045, citation validity 100 % of 675, metamorphic 0 of 47.

Getting there turned up four defects that a deterministic corpus had been hiding, each of which a
live run exposed by asking something the deterministic plan never asks:

- **Two worlds under one key.** Drill-down handles were deduplicated on `value` alone, but a handle
  is a value *and* the term that minted it, and the same target is routinely minted by two parents.
  Which one survived depended on the order the passes ran in, so a world stopped re-recording
  byte-identically the first time a live pass contributed to it.
- **The horizon at replay.** A window past `observed_at` is clamped before the backend keys it, so
  the answer is filed under the clamped term — but the replayer keyed both its answer check and its
  world lookup from the *unclamped* request, and compared the world's un-annotated digest against
  the trajectory's annotated one. A sound recording was reported as a divergence.
- **The trajectory was in completion order.** `TestTheFirstWaveAdmitsTheSameSetEveryRunWhenACapBinds`
  failed under two concurrent test runs and passed alone. Admission had been lifted out of the
  wave's goroutines; the *records* had not, and `duration_ms` — wall-clock time — was being recorded
  into them. Both were only made canonical by `replay.Normalize` on the way to disk, so everything
  that read the trajectory without writing it was reading the scheduler.
- **The prefix cache never hit.** `cached_tokens` was 0 on every live turn despite a stable prefix
  and a stable `prompt_cache_key`, because the ledger render was rewritten *inside* the leading
  system message and this provider matches whole-message prefixes. Measured, fixed by moving the
  render to a trailing system message, and written down in `contracts/prompting.md` §Providers.

The citation-validity gate was also conflating two things: a `not_recorded` answer is an answer,
faithfully recorded, and how much of it a fixture produces is the miss rate's business. Counting it
again as an invalid citation made a 100 % gate about fabricated evidence unsatisfiable for any
fixture whose miss rate was not exactly zero.

### Track O (2026-09-18): the security pass and the quickstart run

T116 and T118, run in parallel with Tracks M and N and touching only
`internal/investigation/render/**`, `internal/investigation/store/delivery.go`,
`internal/server/**`, `internal/cli/{serve.go,investigate*.go}`, `scripts/check-no-secrets*.sh`,
the guard job in `.github/workflows/ci.yml`, `specs/002-investigation-engine/quickstart.md` and
its run record, `docs/security/**` and these task lines.

The two tasks turned out to be one task. The security pass asked what the first write path can
actually reach, and the quickstart run asked what a person following the documentation actually
gets; both answers were found the same way — by running the thing and reading what came back
rather than reading what it says it does. Four confinement defects and two behaviour drifts came
out of it, each with a test that fails without the fix.

### Track N (2026-09-18): schema documentation, connector docs, ADR statuses

T114, T115 and T119, run in parallel with Tracks M and O and touching only `docs/schema/**`,
`docs/connectors/**`, `docs/decisions/**` and these task lines.

- **The schema pages are complete** (T114). `algebra.md` and `digests.md` were partial: the horizon
  rule, the shared window plan, the coverage fields a clamp writes, the digest rule and `Separates`
  were in the code and in the contracts but not in the published prose. `ledger.md` was missing the
  verdict rule — prior-only mass is not confidence — which is the one number-shaped rule a second
  implementation could not have reproduced from this directory. `investigation.md` is new and is
  the page a reader needs to tell a fact in the graph from a derived row from a file on disk.
- **The connector directory now covers all three shapes** (T115): feeder, worker, backend. The
  worker page keeps the investigator's contract and the backend page takes the vendor's, with the
  split stated in both. The fresh-reader pass found a real packaging gap rather than a wording one
  — `backend.NewResponse`, `ClampRequest`/`AnnotateHorizon`, `NewWindow`/`NewWindowPair` and
  `metrics.Separates` are all `internal/`, so a backend in a module of its own can implement the
  interfaces and run the conformance suite but cannot build a conforming response. The docs say so
  plainly rather than describing a path that does not exist; promoting those to `pkg/` is a
  follow-up, not a doc fix.
- **The decision record is contiguous** (T119): 0001–0008, with 0006 written after the fact from
  the spec and plan it was always supposed to record, 0007 recording the confined write path the
  constitution was amended for, and 0008 recording the second model provider and — the part worth
  keeping — what it gives up.

### Track L (2026-09-18): a second model provider — Mistral, serving GLM

Out-of-phase work, recorded here because it changes what production runs. The engine's model
boundary spoke one vendor; it now speaks two, chosen **per role** by `provider:` in
`config/model.yaml`, behind the same `model.Client` and the same `http.RoundTripper` seam.

- **The seam** (`internal/investigation/model/provider.go`). An internal `provider` interface —
  build a body, issue it through the shared `Transport`, decode into the existing
  `Response`/`Block`/`Usage` types — with `Client.Complete` dispatching on the role's provider and
  filling in the transport-level fields (mode, canonical bodies, digests) identically for both.
  The Anthropic path moved to `anthropic.go` with only its receiver changed, so it is byte-identical
  and the two checked-in fake-model trajectories still replay; `fixture verify --trajectory-only`
  is 16/16 with 33 trajectories.
- **The Mistral client** (`mistral.go`), plain `net/http` + `encoding/json`, no new dependency.
  The full mapping — including the four things with no equivalent and how each is closed — is
  `contracts/prompting.md` §Providers. The load-bearing one: Anthropic's `clear_at:
  "next_user_message"` becomes *rewriting the single system message each turn*, stable prefix
  first, then the renders still live, cleared ones dropped. The injection barrier is untouched:
  worker output reaches the model only ever as a `tool` message (FR-017).
- **The configuration** (version 1.1.0, price table `2026-09-18`). `config/model.yaml`:
  investigator `zai-glm-5-3`, verifier `mistral-medium-latest` (a different family — FR-022a),
  logs labeller `ministral-8b-latest`. `config/model.anthropic.yaml` is the same three roles on
  the Anthropic models, checked in, validated by the suite and priced, so switching back is a
  flag. `config/prices.yaml` prices both. Credentials follow the provider each role names.
- **The CLI.** `serve --enable-investigation`, `eval run` and `fixture record-trajectory` check
  the credential of every provider the configuration actually names, not `$ANTHROPIC_API_KEY`
  alone. `fixture record-trajectory --live` and `eval run --live` are new.
- **The first live trajectory.** `fixtures/incidents/rollout-regression-01-incident/trajectories/
  run-1a532f7a56a3adfb.jsonl`: 3 real GLM turns, 8 tool calls, 33 worker calls, stop
  `diminishing_returns`, 31,326 input / 1,903 output tokens ≈ $0.052, 13 s wall, and the culprit
  `k8s.change=shop/payments@rev7` ranked first at confidence 0.708 (bucket `high`). It replays
  identically with zero network. It **joins** the model-free and fake-model recordings rather than
  replacing either; `fixtures/incidents/README.md` now says why.
- **What the live corpus run exposed, and what it is.** `eval run --fixtures fixtures/incidents
  --runs 1 --live` completed all 16 fixtures for $0.515 and 2m50s, and **15 of 16 were excluded on
  their `not_recorded` miss rate**: a live investigator asks for terms the recorded `world/` does
  not hold, because every world was recorded against the deterministic and fake-model runs. The
  gate is doing what it says — "this gates the fixture, never the engine" — but it leaves the
  corpus aggregate resting on one admitted run, so `lift_over_prior` and `citation_validity` fail
  on an n of 1 and 12. **Closing this is `fixture record-world` over the corpus against a live
  engine**, which is a corpus change and not Track L's to make. No pass@1 threshold was published.
- **The containment invariant was rescoped, once, deliberately.**
  `TestTheRecordedWorldIsASupersetOfWhatTheEngineAsks` asserted that every telemetry term in every
  checked-in trajectory is held by the recorded world. A live recording breaks that by design: the
  model asks what the deterministic plan never derives. Widening the fixture's grid until the world
  held the three terms this run reached for was tried — it grew the world from 2.5 MB to 6.0 MB and
  then put the recording and the world in direct disagreement, because the trajectory had recorded
  those terms as `not_recorded`, and re-recording the trajectory only moves the gap. So the test
  now asserts strict containment on every **model-free** trajectory — which is the wiring
  `fixture record-world` reproduces and the exact failure Phase 7 Track F opened with — and, on a
  recording with a model in the loop, that every uncontained term came back typed `not_recorded`
  rather than silently (FR-027). `fixtures/incidents/README.md` records the same finding.
- **Cache reads were zero on every live turn.** The prefix cache is asked for correctly — the
  system message opens with the byte-identical stable prefix and a `prompt_cache_key` derived
  from it — but this provider reported `cached_tokens: 0` throughout, so the engine's cache
  conformance observation (FR-061) fires from turn two on every live run. Worth measuring against
  a longer run before drawing a conclusion; at $1.40/MTok the cost of being wrong is small.

- [x] T120 *(closed 2026-09-20 — see "Closure record" under the Definition of done below)* Close the feature: assert the **Definition of done for 002** below in one CI run — trajectory-replay gate green, eval gate green with its stated detection power, the coverage audit re-run and published, quickstart §1–§11 and §1a reproduced — and record the result beside 001's FR-050 closure → 001's definition-of-done discipline, FR-071a, SC-022; fixture: all

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)** → everything. **Phase 2 (audit)** depends only on Phase 1 and on 001's graph;
  it is model-free and can start immediately.
- **Phase 3 (the 001 change package)** blocks every fixture recording in Phases 7 and 8, and blocks
  T081 (onset-referenced ranking, needs D1/D2/D3), T044 (needs D4), T059 and T083 (need D5).
  It does **not** block Phases 4 and 5.
- **Phase 4** depends on T002 (the proto) and, for T044/T045, on T026 (join keys). **Phase 5**
  depends on T016 (`π₀`) and T002 only — it overlaps Phase 4 entirely.
- **Phase 6** depends on Phase 4 (workers answer), Phase 5 (the ledger computes) **and — since
  analyze O1 moved the fixtures it consumes into it — on Phase 3**, which must land before T097 or
  T102–T106 record anything. Within it: the six fixture-authoring tasks (T097, T102–T106) come
  first and in parallel, because every task after them names one as its test; then T060–T063 before
  T065–T066; budgets (T067–T073) before T070's reserve is meaningful; intake (T076–T079) can start
  as soon as T042 lands.
- **Phase 7** depends on Phase 6 having produced a run worth recording — it records that run's
  trajectory and replays it; the fixture directories it replays into already exist from Phase 6.
  **Phase 8** depends on Phase 7 (a trajectory that verifies) and adds only the adversarial
  fixtures, the generator and the eval job.
- **Phase 9** after Phase 8, except T114/T115 which can start once Phase 4 lands and T119 (ADR-0007)
  which must land **before** T087.

### Story Dependencies

- **US0** is a prerequisite, not a competitor: it is a deliverable of the first increment and its
  input is a list of incidents, not a running engine.
- **US1** (Phases 4→6) is the spine. **US1a** adds a front door and needs T076's normalisation.
- **US2** needs the ledger (Phase 5) and the workers (Phase 4). **US3** needs Phase 7.
- **US4** (`unknown`) needs T054 (the `no_observed_change` hypothesis) and T086. **US5** (budgets) is self-contained inside Phase 6.
- **US6** (knowledge) needs T027's `KNOWLEDGE_DOC`/`CONCERNS` and T049.
- **US7/US8** (review, corrections) need T082's lifecycle. **US9** needs T082 and T083.
- **US10** needs everything and is Phase 8.

### Parallel Opportunities

- **Phase 2 runs in parallel with Phase 3**: the audit touches no 001 code the change package edits.
- **Phase 3 and Phase 4 overlap once T002 lands**: the algebra, `pkg/backend` and the recorded
  backend need none of D1–D5; only T044 (join keys), T081 (signed distance, actor kind) and T059
  (event bodies) wait.
- **Phases 4 and 5 overlap completely** — the ledger is model-free and worker-free.
- Inside Phase 4: T033–T035 in parallel; then T036–T040 (the backend half) in parallel with
  T041–T050 (the worker half); the five workers in parallel with each other.
- Inside Phase 6: the budget track (T067–T073), the verification track (T074–T075) and the intake
  track (T076–T079) are three independent tracks after T063.
- Inside Phase 6: T097 and T102–T106 are six independent fixture-authoring tasks and run in
  parallel with each other, ahead of everything that consumes them.
- Inside Phase 8: T099–T101 are three independent adversarial fixture-authoring tasks.

### Parallel Example: Phases 3 + 4 + 5

```bash
# Track A (in 001's directories) — the change package
T019 → T020 → T021 → T025 ; T022 → T023 → T024 ; T026 ; T027 → T028 → T029 → T030 ; T031 ; T032

# Track B — algebra and backends, after T002
T033 → T034 → T035 → T036 → T037 → T038 → T039 → T040

# Track C — workers, after T034
T041 → {T042, T043, T045→T046, T047, T048, T049} → T050

# Track D — the ledger, after T016
T051 → T052 → {T053, T054, T055, T056} → T057 → T058
```

---

## Implementation Strategy

### Increment 1 — the ceiling (Phases 1–2)

The audit is published, `π₀` has a value and a provenance, and no gate in the repository exceeds
the ceiling. This is a few days of work that can reorder the roadmap, and it already has once.

### Increment 2 — the substrate (Phase 3)

The 001 change package lands additively; `sreagent.graph.v1` takes a MINOR bump. Nothing in 002
is recorded before this.

### MVP — "prior + first wave + ledger", with no model call (Phases 1–5 + T065, T066, T093–T097)

Phases 1–5 plus the provisional ranking, the deterministic first wave and layer-1 replay give a
**replayed investigation on `rollout-regression-01-incident` that makes no model call at all**:
the graph's ranking as a provisional answer, the first wave's comparisons as evidence items, typed
judgments, a computed posterior with `no_observed_change` carrying `π₀`, and a ledger a reviewer can read. That is
already useful — it is the anytime answer an on-call sees in the first thirty seconds — and it is
the honest floor the model has to beat, which is exactly what "lift over the prior" measures.

**Stop and validate here.** Demo: quickstart §1 up to the provisional ranking, §2, §3 layer 1.

### Increment 4 — the reasoning layer (Phase 6)

The loop, the budgets, the verifier, both front doors, the human channel and the renderer. Demo:
quickstart §1, §1a, §4, §5, §6, §7.

### Increment 5 — the gates (Phases 7–8)

Trajectory replay first, because it is the plumbing gate and retrofitting it is how it rots. Then
the corpus, the metamorphic generator and the eval job with its published detection power.

### Definition of done for 002

Tied to 001's FR-050 discipline, closed by T120:

1. **The trajectory-replay gate is green** in `ci.yml`: every incident fixture replays
   byte-identically with zero network, and any unmatched request fails loudly (SC-003, SC-018).
2. **The eval gate is green with its stated power**: `eval.yml` runs the corpus × k in world-replay
   mode against the production model configuration; aggregate pass@1 meets the published threshold,
   lift over the prior is > 0, harm rate ≤ 5 %, citation validity = 100 %, no untraceable
   conclusion, no improvised replay, no metamorphic verdict change, no regression on a
   human-labelled case — and every threshold is printed together with the regression it can
   actually detect (SC-001, SC-016, SC-017, SC-020, SC-021).
3. **The coverage audit has been re-run and published** after this feature's connectors, with the
   ceiling, the feeder set it was measured against and the target ceiling then in force, the
   movement attributed and a ceiling that did not move reported as such; no published target
   exceeds it (FR-071a, SC-022).
4. **Quickstart §1–§11 and §1a reproduce** on a clean machine with **no vendor account and no network to
   any telemetry backend** (FR-067, SC-011).

### Closure record (T120, 2026-09-20)

Asserted on origin `main` after the Phase 9 push, in the runs attached to the closing commits in
GitHub Actions:

1. **Trajectory-replay gate green** — `ci.yml` job `trajectory replay + calibration` on
   `c0f97e0` (run 35454830246): 16 of 16 incident fixtures, 48 trajectories (model-free,
   fake-model and one live GLM recording each) replayed byte-identically inside a network
   namespace with no route, calibration published from the recordings with zero model calls.
   The same run's `test` job failed on a data race **inside a test helper** (the fake clock
   `testClock` in `internal/investigation/engine/engine_test.go` was advanced from the first
   wave's concurrent goroutines without a lock; the engine itself was not racy). Fixed in the
   closing commit; `CGO_ENABLED=1 go test -race ./...` is green over all 41 packages, and the
   CI run attached to the closing commit is the one that asserts this item end to end.
2. **Eval gate green with its stated power** — `eval.yml` run 35454830331 on `c0f97e0`: public
   leg 16 fixtures × 3 runs plus 47 metamorphic variants, `check-report.sh --investigation`
   exit 0, every gate in force passing (lift +0.078, harm 0, confidently wrong 0, citation
   validity 100 % of 663, replay divergence 0, improvised replays 0, metamorphic changes 0/47,
   corpus gaps 0), detection power printed per gate (n = 48 detects a pass@1 drop from 56 % to
   38.6 %; harm 5 % → 14.4 %; zero-tolerance gates catch a defect present in ≥ 6.1 % of runs).
   The run is **labelled MODEL-FREE** because the repository holds no model credential; the same
   gate ran live on the owner's machine with the production configuration (GLM via Mistral,
   `docs/evaluation/investigation-metrics.md`, 11 of 16 admitted, pass@1 0.636 at n = 33, every
   gate passing). The private leg skipped visibly for want of a private corpus secret, and the
   release statement names both legs. **The aggregate pass@1 threshold is deliberately still
   `unset`**: the live figure (0.636) exceeds the audit ceiling (0.615), and FR-071 as written
   refuses to publish a target above it — an owner decision, recorded here rather than forced.
3. **Coverage audit re-run and published unchanged** — `aisre audit coverage` over the
   private transcription (audit `2026-09`, 13 classifiable incidents) reproduces the published
   `docs/evaluation/coverage-audit-2026-09.json` exactly: ceiling 0.615385, ladder 0 / 3 / 8
   under feature-001 / +deploy / +vendor-notice, feeder set in force `+vendor-notice`. The
   ceiling did not move and is reported as such: this feature added a reasoning layer and no
   production connector, so the feeder set it was measured against is unchanged (FR-071a).
   The ceiling guard in every eval run reads that file and reports "nothing is ceiling-bounded
   yet" (no published target).
4. **Quickstart reproduced** — `quickstart-run-2026-09-18.md` (beside this file): §Prerequisites,
   §1–§11 and §1a on a clean worktree with a fresh database and no vendor credential; nine
   documentation drifts and two code drifts fixed in place (`investigate get` returned no ledger
   over the API; a fact against a concluded investigation did not reopen it).

Left open, tracked as the Phase 9 follow-up list. **Items 1–6 were closed by Track P
(2026-09-20); each is struck through with how.**

1. ~~`submit_human_fact`, `reopen_investigation` and `label_investigation` have schema and
   projector paths but no emitter in this feature.~~ **Closed:**
   `internal/investigation/store/human_events.go` emits all three from the transaction that
   writes the row, under source `human` (`internal/log/human_source.go`, shared with the
   resolution path), with content-derived idempotency keys — the reopen key needed a `reopen:`
   prefix because the published bare-child-id spelling collides with `record_investigation`'s
   key and was costing every reopened investigation its decision record.
   `RebuildHumanChannel` is the inverse, and the rebuild test
   (`TestTheInvestigationRowsRebuildFromTheEventLogAndARegenerableWorld`) now pushes a fact,
   reopens and labels, drops the schema, and asserts all three come back from the log.
2. ~~`investigate to-incident` writes no manifest.~~ **Closed:**
   `internal/cli/investigate_incident_manifest.go` writes a complete `manifest.yaml` — the 001
   fields, sources derived from the exported events, and the `incident:` block with the question,
   the reviewer's culprit, the prior rank read off the ledger's own priors, the knowability time,
   `provenance: recorded` and the published `world:` defaults. What no command can derive is
   written structurally valid and marked `# TODO(reviewer)`; `fixture.PlaceholderFields` reports
   those and `fixture verify` prints them rather than passing silently
   (`TestToIncidentWritesAManifestTheHarnessAccepts`).
3. ~~`Runner.WorldDir` and `Runner.persist`'s context.~~ **Closed:** `WorldDir` now finds the
   world the run actually read — `<root>/<id>/world`, then the configured `WorldDir` (wired from
   `serve`'s own `worldDirOf`), then the recording root and `<root>/world` — each required to
   hold an `index.json` (`TestAnExportFromAFixtureServedRunCarriesLayerTwo`). `persist`,
   `conclude` and `failed` run on `detachedWrite` (`context.WithoutCancel` bounded by
   `PersistWindow`), and a caller that hangs up after the engine reached a terminal state no
   longer leaves a permanently `failed` row
   (`TestACallerThatGoesAwayStillGetsAConcludedRow`).
4. ~~`RecordDelivery` is not on the `--deliver` path.~~ **Closed:** `investigate report
   --deliver` records every outcome through `RecordDelivery`/`DeliveryIDFor`, non-blocking — a
   ledger it cannot write is a printed note and exit 0 — behind a `--db` flag defaulting to
   `$PG_DSN` (`contracts/cli.md` updated). Two deliveries to one target are one row with
   `update_count` 2 (`TestReportDeliveryIsRecordedInTheLedger`).
5. ~~`cache_read_ratio` is documented and unimplemented.~~ **Closed:** implemented in
   `internal/eval/report.go` at fixture and corpus scope, cached input over total input from the
   recorded per-call `Usage`, `n/a` for a model-free run, reported and never gated, published in
   the Markdown renderer
   (`TestTheCacheReadRatioIsPublishedAndAModelFreeRunSaysNotApplicable`). The published tables
   were **not** regenerated: they are the live GLM run and a model-free regeneration would
   replace a production result with the deterministic engine's. The row joins them at the next
   live run, which the document says beside its generated markers.
6. ~~`replay.Normalize`'s doc disagrees with its worker-block sort; `check-report.sh` reads the
   first corpus row only.~~ **Closed:** the comment now states what the code does — every worker
   block is sorted by request digest, including a strictly paired one, because a paired block is
   indistinguishable from a sequential one and sorting only the interleaved ones would make the
   run id depend on machine load — with `TestAStrictlyPairedWorkerBlockIsSortedToo` pinning it,
   and the 16 corpus trajectories replay byte-identically unchanged. `check-report.sh` refuses a
   rows file carrying two corpus rows for one metric, naming the metric
   (`TestTwoCorpusRowsForOneMetricIsRefused`).

Still open: five fixtures remain excluded from the live eval on miss rates 0.057–0.185; backend
constructors live under `internal/` so an out-of-tree backend cannot follow the connector guide
(promote with the first external connector, 003).

---

## Notes

- Every task touching `fixtures/` regenerates goldens with 001's `fixture record` and worlds with
  **`fixture record-world`, which T039 implements** alongside `worker record` — `worker record`
  takes a focus, window and hop radius from flags and is how a world is first recorded;
  `fixture record-world <dir>` re-records an existing incident fixture's `world/` layer from that
  fixture's own manifest, so a re-record cannot silently change its shape. Golden and world diffs
  are reviewed in the PR like code.
- Phase 3's tasks are **001's work**, listed here because 002 cannot start its fixtures without
  them and because the two features are reviewed together (ADR-0005, ADR-0006).
- `SC-001`'s gate value and `SC-004`'s per-bucket calibration are **not fully verifiable until the
  corpus exists** (plan §Self-analysis). They are flagged as such in the report rather than claimed:
  the gate value is **set by the first full corpus run** (FR-060, T112) and reported as `unset`
  until then rather than passing silently, and an under-powered bucket is reported as under-powered
  rather than scored (T110). Calibration itself is published on **every** pull request, from the
  recorded trajectories with zero model calls (T009, T098, constitution §Replay CI (c)); only its
  live-run counterpart waits for `eval.yml`.
- Numbers carried as initial values and marked for calibration with the corpus in hand:
  `π₀` (from the audit), the aggregate pass@1 gate, harm rate 5 %, miss rate 5 %, diminishing
  returns `n = 5` / `ε = 0.02`, the judgment LRs `1.5 / 3 / 10 / 50`, the five buckets
  (research §20).
- The prompt text in `internal/investigation/engine/prompt/` is **part of the published contract**
  and is versioned with the algebra: a change to it is a change that must be evaluated (FR-061).

### Bug fixes / follow-ups

- **2026-09-17 — the coverage-audit category vocabulary was six values too narrow (FR-070,
  FR-071b).** The twelve values of `0006_investigation.sql` came from FR-070's list of causes the
  feature-001 feeders miss, and named none of the most common change-induced causes: a
  deployment, a configuration change, a cloud/provider maintenance window, a vendor API change or
  deprecation, a capacity or quota limit, an unplanned provider infrastructure incident.
  Transcribing the September 2026 audit put **6 of its 13 incidents under `other`**, which makes
  the category breakdown rank no feeder (FR-070) and the unobservable remainder name no fixture
  (FR-071b), and weakens `audit coverage gaps`. Fixed additively — nothing renamed, nothing
  removed — by `internal/store/postgres/migrations/0007_audit_categories.sql` (18 values),
  mirrored in `internal/investigation/audit/vocabulary.go`, documented in
  `docs/evaluation/coverage-audit-format.md`, and exercised by the `fixtures/audits/synthetic-01`
  and `synthetic-02` incidents (verdicts, ceilings and π₀ unchanged: 0 / 3 / 8, `8/13 = 0.615385`,
  π₀ `0.384615`). The vocabulary test now reads the highest-numbered migration that *defines*
  `coverage_audit_items_category_check` rather than naming 0006.
- **Done 2026-09-17 (was owed by the above)**: the owner's **private transcription** of the September 2026 audit
  was re-done with the finer categories — the six incidents filed as `other` are a
  `deployment` / `configuration_change` / `cloud_maintenance` / `vendor_api_change` /
  `capacity_limit` / `infrastructure_incident` question each — and
  `docs/evaluation/coverage-audit-2026-09.json` regenerated from it:

  ```sh
  # the input stays outside this repository; the audit needs no database and no server
  aisre audit coverage --input ~/private/audit/incidents-2026-09.yaml \
                           --aggregate-only --out /tmp/audit-2026-09
  cp /tmp/audit-2026-09/coverage-audit-2026-09.json docs/evaluation/coverage-audit-2026-09.json
  ```

  `--out` writes `coverage-audit-<audit_id>.{json,md}`, so it is pointed at a scratch directory
  and only the JSON is copied in: the Markdown beside it is hand-written narrative, not a
  rendering. `--aggregate-only` is what keeps the per-incident rows out of the published file
  (FR-069a). The aggregate checked in today is the coarse one and is deliberately left as it
  stands until that re-transcription happens; the vocabulary note in
  `docs/evaluation/coverage-audit-2026-09.md` says so in the published file itself. Nothing
  downstream moves when it is regenerated: the ceiling, π₀ and the ladder count verdicts, not
  categories — only the category breakdown, the `missing` ranking and the remainder get finer.
