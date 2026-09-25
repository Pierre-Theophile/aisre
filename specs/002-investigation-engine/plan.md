# Implementation Plan: Investigation Engine

**Branch**: `002-investigation-engine` | **Date**: 2026-09-17 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-investigation-engine/spec.md`

## Summary

Build the agent that reasons over the substrate of feature 001: one investigator — a language
model owning exactly one belief state — that calls read-only workers, each bound to one source
of truth, through a small typed query algebra, and returns a ranked list of hypotheses with an
evidence chain, a computed confidence and `unknown` as a first-class answer. Everything is
recorded at the worker and model boundary, so an investigation replays with no vendor account:
byte-identically in trajectory mode, and from a recorded *world* when a second run reasons its
way to a different question.

Technical approach, from [research.md](./research.md): **Go**, in the existing `sre-agent`
binary (new `investigate`, `worker`, `audit` subcommands and an `InvestigationService` on the
existing ConnectRPC server); the investigator is **`claude-fable-5-1`** driven by a
**hand-rolled agent loop** over the Anthropic Go SDK — not the Tool Runner and not the Claude
Agent SDK — because byte-exact recording and a deterministic re-issue path require owning the
loop; **deterministic Go workers** for graph, metrics (seasonal baseline + two-sided CUSUM
onset estimation), traces and knowledge, with a Drain-style template miner and an optional
`claude-haiku-4-5` labelling pass in the logs worker only; the **hypothesis ledger** in
PostgreSQL schema `investigation`, never in the model's context, with confidence computed from
typed judgments by a published deterministic rule; the **decision record** emitted as a graph
event while the recording lives beside the graph; and evaluation as new rows in the existing
`fixture verify --report`, not a second harness.

The new published contract is `api/sreagent/investigation/v1` — the investigation service, the
ledger, the typed query algebra, the digest/coverage/join-key/outcome shapes shared with
features 003 and 005, the recording format and the ground-truth schema.

## Technical Context

**Language/Version**: Go, same floor as 001 (`go 1.27`, latest stable toolchain),
`CGO_ENABLED=0`, one static binary.

**Primary Dependencies**: everything 001 already has (`pgx/v5`, `connectrpc.com/connect`,
`buf`, `cobra`, OTel SDK, `go-oidc/v3`) plus exactly one new module:
`github.com/anthropics/anthropic-sdk-go` (pure Go, `net/http` + `encoding/json`, no cgo). The
metrics change-point detector, the seasonal baseline, the Drain-style template miner and the
BM25 scorer are implemented in-repo rather than taken as dependencies — see Complexity
Tracking and research §4, §5, §7.

**Model configuration** — **providers.** Two are supported, chosen **per role** by `provider:`
in `config/model.yaml`, behind one `model.Client` and one `http.RoundTripper` seam. Per-role
rather than per-client because FR-022a asks the verifier to run a different model from the
investigator so the error it exists to catch is decorrelated, and "a different model" may well
mean "a different vendor". The credential follows the provider — `$MISTRAL_API_KEY`,
`$ANTHROPIC_API_KEY` — and a command that cannot resolve one runs model-free and names the
variable rather than failing on the first turn of an incident (FR-067).

*In force* (`config/model.yaml`, version 1.1.0; rates read 2026-09-18):

| role | provider | model id | why |
|---|---|---|---|
| investigator | mistral | `zai-glm-5-3` | a cheap large thinking model: reasoning and function calling both on, 1,048,576-token context — the same order the Anthropic investigator was chosen for — at $1.40/$4.40 per MTok, roughly a seventh of the input price and a tenth of the output price |
| verifier (fresh context, FR-022a) | mistral | `mistral-medium-latest` | a mechanical claim-to-citation check; a different model, from a different family than GLM, decorrelates the error it is there to catch; $1.50/$7.50 |
| logs worker labelling (optional, after template mining) | mistral | `ministral-8b-latest` | bounded, template-only input; the cheapest model on the platform that still calls functions; $0.15/$0.15 |

*The alternative* (`config/model.anthropic.yaml`, same version, kept valid and priced so that
switching back is a flag; research §2, ids and prices from the `claude-api` skill):

| role | provider | model id | why |
|---|---|---|---|
| investigator | anthropic | `claude-fable-5-1` | the owner's stated bar ("Fable 5.1-class reasoning"); 1M context; thinking always on; $10/$50 per MTok, cache reads $0.25/MTok |
| verifier | anthropic | `claude-opus-5` | $5/$25 |
| logs worker labelling | anthropic | `claude-haiku-4-5` | $1/$5 |

API features used, and the design constraints each imposes, are research §2 and §3: tool use
for every worker call; structured outputs (`output_config.format`, or `response_format` with a
`json_schema`) plus `strict: true` tools for typed judgments and ledger updates; prompt caching
for the stable tools → system → algebra prefix; `count_tokens` for pre-flight budget admission —
estimated locally on a provider that publishes no such endpoint — and `usage` for post-flight
accounting; mid-conversation system messages with `clear_at: "next_user_message"` for the
per-turn ledger re-render, met on Mistral by rewriting the single system message each turn;
`output_config.effort` / `reasoning_effort` in place of temperature, which is never sent to
either provider. The full mapping, including what has no equivalent and how each gap is closed,
is `contracts/prompting.md` §Providers.

**Storage**: the same PostgreSQL instance and database as 001, new schema `investigation`
(investigations, incidents, symptoms, hypotheses, judgments, evidence_items, worker_calls,
recordings, human_facts, human_reviews, labels, budget_spend, coverage_audits). Recording
*bodies* are files beside the graph (fixture directory or the configured recording root),
addressed from Postgres by key + digest + size, exactly as 001 keeps `payloads/`. The
investigation decision record is additionally emitted as a typed graph event.

**Testing**: `go test` unit and table-driven tests; trajectory replay as the plumbing gate on
every PR; `aisre fixture verify --report` extended with the investigation metrics; property
tests for ledger order-independence (`pgregory.net/rapid`, already a 001 dependency) — the
posterior must not depend on the order judgments arrived in; golden-digest tests for every
worker in recorded mode; an eval job (`eval.yml`) running the corpus × k runs against the
recorded worlds with a published detection power.

**Target Platform**: Linux and macOS (amd64, arm64), static binary and OCI image, as 001.

**Project Type**: single Go module, one binary, extended — `investigate`, `worker`, `audit`,
`incident`, and `fixture` subcommand extensions, plus `InvestigationService` served by
`aisre serve`.

**Performance Goals** (from SC-013 and FR-046a/FR-047): under the `page` profile, the
provisional prior-only ranking within 5 s of intake in ≥95% of investigations; the first
*tested* hypothesis within 120 s in ≥90%; a substantive answer within 5 minutes in ≥90%; hard
stop at 10 minutes. The deterministic first wave (no model in the loop) must complete within
the first 30 s so the model's first turn starts with evidence. Ledger recomputation after a
judgment: < 10 ms for 50 hypotheses × 500 judgments. Trajectory replay of a recorded
investigation: < 5 s, zero network.

**Constraints**: read-only credentials toward every source; no telemetry payload in the graph
and none in any event this feature emits; every claim traceable to an evidence id; confidence
computed, never verbalised; no state in which the engine waits for a human; the model may only
ask questions expressible in the published algebra; recordings bounded and redacted; replay
deterministic at the tool boundary and byte-identical in trajectory mode.

**Scale/Scope**: an investigation carries ≤ 50 hypotheses, ≤ 500 evidence items, ≤ 200 worker
calls and ≤ 40 model turns under `page`. A recorded world for one fixture is the cross product
of the algebra over a ≤ 3-hop neighbourhood (≤ 60 pointers/edges) × a window grid (≤ 12 window
pairs) — order 10³ terms, a few MB. The corpus at the evaluation gate: ≥ 8 base fixtures,
their metamorphic multiples, and ≥ 3 adversarial fixtures, run 3 times each (7 when the first 3
disagree).

**Unknowns carried into Phase 0** (all resolved in [research.md](./research.md)): the binary
boundary; loop ownership (Agent SDK vs Tool Runner vs hand-rolled); the change-point method;
whether onset estimation can live investigator-side at all under principle IV; the log
template miner; whether v1 knowledge retrieval needs embeddings; the posterior rule and the
confidence buckets; the recording key and format; the budget manager's control law; the
shape of the shared telemetry-backend contract and where it is published; the exact list of
feature 001 changes and their semver impact.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| # | Principle | Gate | Status |
|---|-----------|------|--------|
| I | Graph first | Every structural fact the investigator uses is a `QueryService` result (FR-012); the investigator reads no source directly (FR-009). The engine's own conclusion re-enters the graph as one typed event, the decision record (FR-035). The investigation store holds the *run* — ledger, judgments, calls, spend — which is the engine's working state, not shared system state, and is rebuildable from the recording + the graph. | PASS with 1 justified item (see Complexity Tracking: the investigation store) |
| II | Bitemporal or nothing | Every investigation pins a valid instant and an observed instant (FR-004); every worker call records both (FR-021); pointers are read as-of the investigation instant (FR-029, FR-030). Review mode is the explicit, recorded exception (FR-005). The decision record is bitemporal like any other fact. | PASS |
| III | Event-sourced ingestion | The decision record, the human fact, the reopen link and the label are typed, idempotent, replayable events (FR-035, FR-057a/b/e, D5). One event per transaction: an investigation emits exactly one decision record at conclusion, and each human action is its own event — no batch. Idempotency key = investigation id (+ action id). | PASS |
| IV | Telemetry stays in its backend | The graph receives identifiers, parameters, statuses, confidences, rationales, spend and *digests* — never a sample, a line or a span (spec "What is recorded, and where"; FR-034). Digests and sanitised exemplars live in the recording beside the graph. Change-point detection runs **backend-side**, so raw series never cross the digest boundary at all (research §4). SC-010 is a fixture. | PASS |
| V | Evidence-first | Every claim resolves to an evidence id (FR-022, SC-002); confidence is a deterministic function of the ledger, never a model utterance (FR-023); `unknown` is a computed outcome of an open hypothesis space (FR-019a); a fresh-context verifier gates publication (FR-022a); **calibration is published on every run, at both cadences**: per pull request from the *recorded trajectories*, with zero model calls — the confidences are ledger-computed and therefore deterministic, so a reliability table and a Brier/log-loss score fall out of replay alone — and per live run from `eval.yml`'s corpus × k (FR-059). | PASS |
| VI | Entity resolution is auditable | The subject is resolved through the published resolution path before reasoning, both identifiers are reported, and the resolution audit is an evidence item (FR-003, US1-3). Merges decided after the investigation instant are invisible under the observed-time pin, and the engine says so. | PASS |
| VII | Read-only by default | Workers may not declare a state-changing capability and are rejected at registration if they do (FR-016); the engine refuses to start with write-scoped credentials (FR-008); no remediation of any kind (FR-028). The investigation store is the tool's own state, exempt by the constitution's own wording. | PASS |
| VIII | Evaluation from day one | Fixtures extend 001's format rather than replacing it (FR-063); trajectory replay gates every PR; the corpus is multiplied metamorphically and stressed adversarially (FR-062a/b); the coverage audit measures the ceiling before any gate is set (FR-069–071). | PASS |
| IX | Open schema | `api/sreagent/investigation/v1` publishes the worker contract, the telemetry-backend algebra and digests, the ledger, the evidence item and its judgments, the budget declaration, the recording format and the ground-truth shape (FR-068). `buf breaking` gates it; the 001 changes are additive and take a MINOR bump. | PASS |
| X | Simplicity over cleverness | One binary, one external service (the Postgres 001 already requires), one new Go module (the Anthropic SDK). The change-point detector, template miner and retrieval scorer are ~600 lines in-repo rather than three dependencies, because determinism across architectures is a hard requirement (goldens are compared byte for byte). | PASS with 2 justified items |
| §Replay CI | Replay, idempotency, calibration on **every** CI run | (a) and (b) are 001's existing fixture verification, which incident fixtures extend rather than replace (FR-063), plus byte-identical trajectory replay (FR-042a). (c) is satisfied **per pull request**: `ci.yml` computes calibration — the reliability table over the five published buckets, Brier and log-loss — from the **recorded trajectories**, with **zero model calls**, because every confidence is computed by the ledger from published constants and is therefore reproduced exactly by replay; the result is published in the job summary. `eval.yml` publishes the **live-run** calibration over the corpus × k. | PASS |

**Gate result: PASS.** Proceed to Phase 0. Post-design re-check is at the end of this
document.

## Project Structure

### Documentation (this feature)

```text
specs/002-investigation-engine/
├── plan.md                      # This file
├── research.md                  # Phase 0: every decision with rationale and alternatives
├── data-model.md                # Phase 1: schema `investigation`, invariants, state machines
├── quickstart.md                # Phase 1: run, replay, evaluate, audit — end to end
├── contracts/
│   ├── investigation.proto      # Public schema: service, ledger, algebra, digests, recording
│   ├── worker-sdk.md            # The worker contract (rhymes with 001's feeder-sdk.md)
│   ├── telemetry-backend.md     # The backend contract shared with 003/005: algebra, digests,
│   │                            #   coverage, join keys, drill-down, typed outcomes
│   ├── cli.md                   # `investigate`, `worker`, `audit`, `incident`, `fixture *`
│   ├── incident-format.md       # 001 fixture format + `incident:`, `world/`, `trajectories/`
│   └── prompting.md             # The system prefix, the ledger render, the injection barrier
├── checklists/requirements.md   # (existing)
└── tasks.md                     # Phase 2 (/speckit-tasks) — NOT created by /speckit-plan
```

### Source Code (repository root)

Additions only; everything in 001's tree stays where it is.

```text
api/
└── sreagent/
    ├── graph/v1/                     # EXISTING — gains the 002 event types (MINOR, see below)
    └── investigation/v1/             # NEW — investigation.proto + generated code

internal/
├── investigation/
│   ├── engine/                       # the loop: intake, wave, turns, stop, synthesis, render
│   ├── model/                        # Anthropic client wrapper: request build, cache breakpoints,
│   │                                 #   structured outputs, usage accounting, record/replay hook
│   ├── ledger/                       # hypotheses, judgments, posterior rule, buckets, render
│   ├── workers/
│   │   ├── graph/                    # QueryService client; no model
│   │   ├── metrics/                  # onset (CUSUM + seasonal baseline), compare; no model
│   │   ├── traces/                   # error spans, latency by operation; no model
│   │   ├── logs/                     # Drain-style miner + optional haiku labelling
│   │   └── knowledge/                # graph-scoped retrieval + BM25 over the scoped set
│   ├── backend/                      # TelemetryBackend registry; `recorded` implementation
│   ├── replay/                       # trajectory recorder/replayer, world recorder/reader
│   ├── budget/                       # profiles, admission, quota share, reserve, stop reasons
│   ├── verify/                       # fresh-context verifier pass
│   ├── intake/                       # alert/human-declared normalisation, incident association
│   ├── store/                        # schema `investigation` DAO + migrations
│   ├── render/                       # verdict → ranked → timeline → narrative; deep links
│   └── audit/                        # the coverage audit (User Story 0)
└── fixture/                          # EXISTING — extended: incident block, world, trajectories,
                                      #   pass@1/pass^k/lift/harm/citation/calibration report rows

pkg/
├── feeder/                           # EXISTING public SDK
├── worker/                           # NEW public SDK: Worker, Capability, Digest, Recorder, testkit
└── backend/                          # NEW public SDK: TelemetryBackend, algebra terms, digest
                                      #   builders, coverage/join-key/drill-down helpers, testkit

fixtures/
├── announced-fact-01/                # NEW but 001-owned: valid_at after observed_at (ADR-0005 D5)
└── incidents/                        # NEW: the 16 investigation fixtures of tasks.md §Fixture
    │                                 #   inventory (see contracts/incident-format.md)
    ├── rollout-regression-01-incident/ # derived from 001's `rollout-regression-01`; the MVP
    ├── declared-incident-01/         # human-declared intake, later monitor attaches (FR-001a, FR-008c)
    ├── human-fact-reopen-01/         # US9: a fact against a concluded run, reopen, linked record
    ├── unknown-feeder-gap-01/
    ├── two-simultaneous-01/
    ├── merged-alias-01/
    ├── telemetry-rejection-01/       # SC-010: a decision record carrying a series is rejected
    ├── injection-01/                 # US6 scenario 4: retrieved content that reads as an instruction
    ├── knowledge-scope-01/           # SC-014: in-subgraph doc cited, out-of-subgraph doc never cited
    ├── unobserved-latent-bug-01/     # ─┐ the audit's 23% remainder: one fixture per category,
    ├── unobserved-client-config-01/  #  │ ground truth `unobserved` or `not_change_induced`
    ├── unobserved-business-data-01/  #  │ with that category, graded under SC-023
    ├── unobserved-credential-leak-01/# ─┘ (FR-071b)
    ├── slow-burn-01/                 # adversarial: culprit ~3 h before the alert
    ├── distant-culprit-01/           # adversarial: culprit ~3 hops away
    └── adjacent-decoy-01/            # adversarial: decoy deploy ~2 min before, adjacent service

docs/
├── schema/
│   ├── algebra.md                    # NEW — the query algebra and why it is the only vocabulary
│   ├── digests.md                    # NEW — digest shapes, coverage, join keys, outcomes
│   ├── ledger.md                     # NEW — judgments, the posterior rule, the buckets
│   └── ranking.md, pointers.md       # EXISTING — amended by the 001 change package
├── evaluation/
│   ├── investigation-metrics.md      # NEW — pass@1, pass^k, lift, harm, citation, calibration,
│   │                                 #   miss rate, and the detection power of the gate
│   └── coverage-audit-*.md           # EXISTING pattern — the audit's published output
└── decisions/                        # ADR-0005 (model + loop), ADR-0006 (001 change package)

.github/workflows/
├── ci.yml                            # EXISTING + trajectory replay gate, buf breaking on the new package
└── eval.yml                          # NEW — corpus × k runs, world replay, gated metrics
```

**Structure Decision**: one Go module, one binary, same as 001. The public Go surface grows by
exactly two packages — `pkg/worker` and `pkg/backend` — which mirror `pkg/feeder`'s shape
(`Describe`, a `Run`/`Execute` entry point, a `Recorder`, a `testkit`) so that Casey learns one
idiom. Everything else is `internal/`, so the protobuf contract stays the public surface.

**Terminology**: a *worker* is what the investigator calls (one source of truth, named
capabilities). A *telemetry backend* is what a telemetry worker calls (one vendor, the algebra).
The graph and knowledge workers have no backend; the metrics, logs and traces workers each have
exactly one, which is `recorded` in this feature and live from 003 onward.

## Design Decisions (summary; every one is argued in research.md)

1. **Same binary, new subcommands** (research §1). `aisre investigate`, `worker`, `audit`,
   `incident`, and `serve` gains `InvestigationService`. Principle X wants one binary; the
   Anthropic Go SDK is a pure-Go HTTP client, so "dependency bloat" is one module and no cgo;
   a second binary would duplicate OIDC, OTel, the store, the config and the CLI for nothing.
   The engine is inert without model configuration, so an operator who only runs the graph is
   unaffected.
2. **`claude-fable-5-1` for the investigator, hand-rolled loop** (research §2, §3). The Claude
   Agent SDK has no Go binding and is a coding-agent harness with built-in filesystem and bash
   tools we must not have (principle VII); the SDK Tool Runner (`BetaToolRunner`) owns the very
   loop this feature must own — byte-exact capture of every request and response, a re-issue
   path that replays recorded assistant turns without calling the API, anytime publication,
   the budget admission check before each call, and the diminishing-returns stop. We write the
   loop; it is roughly 300 lines and it is the feature.
3. **The recorded production configuration is the model id and its output settings; temperature
   does not exist on this model family** (research §2). Sampling parameters return 400 on
   `claude-fable-5-1`. FR-061 as it now stands asks for exactly what can be recorded: *"Every
   evaluation run MUST use the model configuration (model identifier, reasoning effort, output
   settings) used in production, and MUST record them."* That is satisfied by recording, and
   evaluating at, the exact model id, `output_config.effort`, the thinking display and the beta
   set, together with the price-table version. The spec amendment is made; this decision quotes the
   current wording rather than the superseded one (analyze I10).
4. **Append-only conversation** (research §3). On this model family, editing earlier turns
   invalidates thinking blocks. The ledger is therefore *re-rendered* as a turn-scoped
   mid-conversation system message (`clear_at: "next_user_message"`), never by rewriting an
   earlier message. That also keeps the cached prefix intact and gives the injection barrier a
   channel content can never reach.
5. **Onset estimation is an algebra term, not investigator arithmetic** (research §4). Deciding
   when the symptom started needs the series; the series may not cross the digest boundary
   (principle IV, FR-014). So `onset(pointer, search_window, method)` is published as an algebra
   term evaluated backend-side, returning an instant, an uncertainty, the method and its
   parameters — a digest like any other. Method: a seasonal-naive baseline plus a two-sided
   CUSUM, refined by binary segmentation; deterministic, O(n), one published threshold, and
   reproducible across architectures at six decimals like the ranking formula.
6. **Drain-style template mining in-repo, model optional and template-only** (research §5). The
   logs worker mines templates algorithmically first and states what the model added over it
   (FR-009a); the model never sees a raw line, only masked templates and counts.
7. **No embeddings in v1** (research §7). The graph scopes retrieval to documents linked to
   nodes in the subgraph — tens of documents, not millions — after which BM25 over the scoped
   set is both sufficient and deterministic. The published trigger for revisiting: a scoped
   candidate set routinely above 200 documents, or recall@10 on the knowledge fixtures below
   0.9.
8. **Confidence is a log-odds sum of published likelihood ratios** (research §8). Each judgment
   carries a direction and a strength from a five-point published scale; the posterior is the
   prior's log-odds plus the sum of the judgments' log-likelihood-ratios, with the *no observed
   change explains this* hypothesis carrying its own mass so the candidates never have to sum
   to one. Order-independent by construction (addition commutes), which is what the property
   test asserts. Reported in five published buckets.
9. **The recording is keyed, not sequenced, in layer 2 and sequenced in layer 1** (research §9).
   L1 is a sequence of records including the model's own requests and responses. L2 is a map
   from a canonicalised algebra term to a digest. A term the world does not hold is
   `not_recorded`, never an approximation.
10. **Evaluation is new rows in `fixture verify --report`** (research §12), not a second
    harness — FR-063, and the same decision 001 already made.
11. **A human declaration is a front door, not a mode** (research §16). A monitor transition and
    a declared incident normalise into one `alert.transition` shape distinguished by actor kind,
    so one association rule, one idempotency discipline and one investigation per incident hold
    across both. The observed instant is pinned to the *declaration* instant; target references
    are parsed by a published rule or supplied by a human, never guessed. This is what the
    coverage audit obliges: in the audited corpus people noticed the incidents and monitors
    mostly did not.
12. **The report goes back to where the incident lives** (research §16a): report-only, edited in
    place, non-blocking, never a precondition of concluding, on a credential scoped to that one
    report and separate from every read credential. The transport is a later connector; the
    contract is here.

## The feature 001 change package

Specified in 001, implemented in 001, consumed here. All of it is **additive**: `buf breaking`
passes and `sreagent.graph.v1` takes a **MINOR** bump. Goldens must be re-recorded wherever a
post-reference candidate or an actor kind appears, which is the `rollout-regression-*` and
`config-change-*` families; `scripts/check-report.sh` thresholds are unchanged.

**Decided here, because the analyze pass found it undecided (I1) and it determines how many 001
goldens move**, three rules that together make the blast radius exactly three fixtures:

1. **`ACTOR_KIND_UNSPECIFIED = 0` stays the enum's zero value.** `buf lint`'s
   `ENUM_ZERO_VALUE_SUFFIX` requires it, and "the source said nothing" is genuinely distinct from
   "the source named an actor the feeder could not type", which is `ACTOR_KIND_UNKNOWN`.
2. **The projector does not write `UNKNOWN` when the source is silent.** It leaves the field
   unspecified, so the canonical serializer emits nothing at all for it — an existing golden with
   no actor kind is byte-identical after the change. `ACTOR_KIND_UNKNOWN` is written only where a
   feeder observed an actor and could not classify it, with the evidence recorded.
3. **Empty `Pointer.join_keys` maps are omitted by canonical serialisation**, exactly as every
   other empty map already is, so adding the field moves no golden that has no join keys to emit.

Therefore **T025 re-records only the fixtures that actually have a post-reference candidate or an
emitted actor kind** — `rollout-regression-01`, `rollout-regression-02`, `config-change-01` — and
**T032 asserts byte-identical goldens for every other 001 fixture** as a positive check, not as an
absence of noticed diffs.

| id | what 001 must publish | proposed shape | why 002 cannot do it |
|----|----------------------|----------------|----------------------|
| **D1** | the ranker's behaviour for a candidate later than `reference_at` | two-sided decay: for `dt ≥ 0`, `temporal = exp(-dt/tau)` unchanged; for `dt < 0`, `temporal = kappa · exp(-|dt|/tau_post)` with published `kappa = 0.5`, `tau_post = tau/2`. Required properties 002 depends on: never the maximum; strictly below an equally distant pre-reference change; monotone in `|dt|`; identifiable from the response | the score and its formula string are 001's published contract |
| **D2** | the signed distance | **add** `int64 signed_time_distance_seconds = 11` and `bool post_reference = 12` to `RankedChange`; leave field 7 (floored) alone so no consumer breaks | the response message is 001's |
| **D3** | a typed actor kind | `enum ActorKind { ACTOR_KIND_UNSPECIFIED = 0, PERSON, AUTOMATION, CONTROLLER, VENDOR, ACTOR_KIND_UNKNOWN }` (ADR-0005 D1 — **`AUTOMATION` is a member**: a CI, bot or deploy principal acting on a person's behalf, which 004 FR-013 and 005 FR-033a require and which is *not* the same as `CONTROLLER`); `Change.actor_kind = 6`; passthrough on `RankedChange`; k8s feeder maps HPA/ReplicaSet-driven scaling to `CONTROLLER`, annotated rollouts to `PERSON`; the vendor-notice feeder of ADR-0004 needs `VENDOR` | the change taxonomy and the feeders are 001's |
| **D4** | join keys in the pointer vocabulary | `map<string,string> join_keys = 6` on `Pointer`, keys from a published role set (`version`, `workload`, `pod`, `host`, `trace`), values being the attribute/tag name in that backend's vocabulary; documented in `docs/schema/pointers.md` | `errors_by_version(pointer, window)` is unexpressible without knowing which tag carries the version |
| **D5** | the event types this feature emits, and the taxonomy they imply | new `EventEnvelope.body` members `record_investigation`, `submit_human_fact`, `reopen_investigation`, `label_investigation`, `alert_transition`; new `NodeType INVESTIGATION`, `KNOWLEDGE_DOC`; new `EdgeType WATCHES` (alert→entity), `INVESTIGATED` (investigation→entity), `CONCERNS` (document→entity); and an allow-listed `sre.investigation.*` prop namespace so the telemetry denylist does not trip on digests and query parameters while still rejecting samples | ingestion, validation and the projector are 001's |
| **D-alert** | the `alert.transition` idempotency convention | key = `(source, stable alert identifier, group, transition instant)` per ADR-0005 D2, `group` empty for human declarations; flapping and no-data transitions filtered at the feeder; recoveries kept as evidence (ADR-0003 D10, ADR-0004 D4) | the event schema and the dedupe path are 001's |
| **D-future** | future-dated valid time | ingestion already accepts `valid_at > observed_at` (`internal/log/validate.go` has no such rule). What 001 owes: a fixture proving the projector's segment planning tolerates it, and a published query semantic — *a fact valid from a future instant is known now and answered only when asked as-of that instant or later*. 003's vendor notices depend on it | the projector and the query semantics are 001's |

## Replay design

Two layers, one recorder, one key discipline.

- **Layer 1, trajectory.** `trajectories/<run-id>.jsonl`: one canonical-JSON record per line, in
  issue order, typed `model_request`, `model_response`, `worker_request`, `worker_response`,
  `ledger_update`, `human_fact`, `stop`. The model records carry the exact request body
  (messages, tools, system, `output_config`, betas) and the exact response (content blocks,
  `usage`, `stop_reason`). Replay re-issues nothing: each `model_request` is matched against the
  next recorded one by canonical digest and the recorded response is returned. A mismatch fails
  the replay loudly naming the first diverging record. Byte-identical output is the gate.
- **Layer 2, world.** `world/<algebra-term-hash>.json`: the cross product of the algebra over
  the alert's neighbourhood (every pointer on every node within the fixture's hop radius, every
  edge on the path set) × the window grid (the baseline/symptom pairs and the single windows the
  fixture declares). Keyed by `sha256(canonical(term))` where the term is the fully typed
  request with instants normalised. A miss is `not_recorded` with the term echoed; the miss rate
  is per-fixture, published, and gates the fixture's admission to the corpus at ≤ 5%.
- **What "identical" means.** Canonical JSON, exactly 001's rules: sorted keys, RFC 3339 UTC,
  six-decimal floats, stable ordering. The same serializer, reused.
- **Export** (FR-042): `aisre investigate export <id>` writes a self-contained directory —
  decision record, ledger, both recording layers, and the graph events needed to answer the
  investigation's graph queries — which replays on a machine with no credentials and no network.

## The budget manager

- **Profiles**: `page` (first tested hypothesis 90–120 s, substantive answer 5 min, hard stop
  10 min) and `review` (quality-bound, operator cap only). Published defaults; the profile
  applied is recorded on the investigation.
- **Dimensions**: wall time; cost units; per-backend and per-cost-class call budgets; maximum
  query window width; share of the backend's *remaining* quota as read from its response
  headers; concurrent investigations; daily global spend. A flat call count is never the only
  cap (FR-043).
- **Admission**: every worker call and every model call passes an admission check *before* it is
  issued — `count_tokens` for the model, the declared cost class and window width for the
  worker — so the reserve is never overrun by a call already in flight.
- **Reserve**: 15% of every dimension is held back for the closing synthesis. When the
  unreserved portion is exhausted, the engine switches to synthesis-only mode: no new evidence,
  the verifier pass, the render, the stop reason.
- **Widening**: a hypothesis whose testing was cut short has its confidence *widened* — its
  bucket moves outward by one step and the reason is recorded — rather than frozen.
- **Diminishing returns**: the stop fires when the L1 norm of the ledger's confidence vector
  changes by less than a published threshold over the last *n* worker calls. Typed stop reason.
- **Quota share**: per backend, the engine reads the remaining-quota header, records it, and
  spends at most its configured share of what remains; where a backend reports nothing it falls
  back to the absolute budget and records that it did.

## Security and the prompt-injection barrier

The worker boundary is the injection barrier (ADR-0003 D8). Concretely: worker output reaches
the model only as `tool_result` blocks; the one bounded free-text field per digest is carried
under a key the system prefix declares as untrusted, is never citable as evidence on its own
(FR-014b), and is excluded from the verifier's citation resolution. Operator instructions travel
only on the mid-conversation system channel, which no worker can write to. A detector on the
free-text and exemplar fields records an `injection_attempt` evidence item when directive
language is seen; the investigation's scope, budgets, worker set, read-only posture and output
are unaffected by anything a worker returns (FR-017), and a conformance fixture proves it.
Redaction is declared per worker and applied before recording (FR-038); people identifiers are
dropped rather than hashed, per ADR-0003 D9, in live mode as well as when recording.

## Observability and cost accounting

`internal/telemetry` is reused. Spans: per investigation, per turn, per worker call, per model
call, per algebra term. Metrics: `investigations_total{outcome,profile}`,
`investigation_duration_seconds{profile,phase}`, `worker_calls_total{worker,capability,mode,outcome}`,
`worker_call_latency{worker,capability}`, `model_tokens_total{model,class}` (class ∈ input,
cache_write, cache_read, output), `investigation_cost_units{model}`,
`backend_quota_share{backend}`, `ledger_updates_total{direction}`, `stop_reasons_total{reason}`,
`replay_divergences_total{layer}`, `not_recorded_total{fixture}`. Cost is recorded twice: as raw
token counts per model id and class — the provider-independent unit that aggregates across runs
and over time (FR-048) — and as a derived monetary figure from a versioned, checked-in price
table whose version is stored with the investigation.

## Evaluation and CI

- **Trajectory replay** runs in `ci.yml` on every PR: every fixture's recorded trajectory must
  replay byte-identically, with zero network. This is the plumbing gate. It is run as
  `fixture verify … --trajectory-only`, and it **also publishes calibration on every PR**: the
  reliability table over the five buckets and the Brier and log-loss scores, computed from the
  **recorded trajectories** with **zero model calls**, which is possible precisely because
  confidences are ledger-computed and deterministic (constitution §Replay CI (c), principle V).
  The numbers go in the job summary. Live-run calibration — the same table over runs that actually
  called the model — comes from `eval.yml`, not from CI.
- **`eval.yml`** runs the corpus × k (3, extended to 7 on disagreement) in world-replay mode with
  the production model configuration, and publishes `docs/evaluation/investigation-metrics.md`
  rows: pass@1, pass^k, lift over the prior (MRR delta), harm rate, citation validity,
  localisation/attribution/mechanism with partial credit, calibration (reliability table + Brier,
  paired against the previous version on the same fixtures), `not_recorded` miss rate, onset
  error, time-to-provisional/first-tested/conclusion, calls and quota share, cost, `unknown`
  rate and precision, replay-divergence rate. Gated: aggregate pass@1, lift > 0, harm rate,
  citation validity = 100%, any untraceable conclusion, any improvised replay, any metamorphic
  verdict change, any regression on a human-labelled case. Reported but not gated: top-k,
  calibration, cost, timing. Never gated: best-of-k.
- **Detection power is published with the gate**: 8 fixtures × 5 runs = 40 Bernoulli trials
  detects 90% → 70% reliably and cannot detect 90% → 80%; the published gate says so in the same
  sentence as its threshold.
- **Private-corpus split**: the public repository ships synthetic structural twins and runs the
  full gate on them. The private repository holds the recorded corpus and runs the same job with
  the same thresholds; the public `eval.yml` records which corpus it ran against, and a release
  statement names both results. No recorded production fixture enters the public repository
  (ADR-0003 D9).
- **The coverage audit** is already a published result, not a plan: 13 classifiable incidents,
  0 of 13 with the 001 feeders alone, 3 of 13 with deploy feeders, 8 of 13 with vendor-notice
  feeders, a ceiling of about **62%** (`docs/evaluation/coverage-audit-2026-09.md`, ADR-0004,
  FR-069a). It is re-run by `aisre audit coverage` after each connector feature ships, over
  the same incident list, publishing the new ceiling, the feeder set it was measured against and
  the target ceiling then in force, with the movement attributed to the feeders added and a
  ceiling that did not move reported as such (FR-071a). Its ceiling bounds every published
  accuracy target, and `1 − ceiling` is the prior of *no observed change explains this*.
- **The audit's unobservable remainder is corpus work, not a gap to apologise for**: latent bug,
  client-side configuration, business data change and credential leak each owe at least one
  fixture whose ground truth is `unobserved` or `not_change_induced` with that category, graded
  under SC-023. A category with no fixture is **reported as a corpus gap on every evaluation
  run** (FR-071b).

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|--------------------------------------|
| A second storage schema (`investigation`) holding the ledger, calls and spend, alongside the graph (Principle I) | The ledger must be updatable many times per second during a run, held outside the model's context, readable by graders without replaying, and consistent with the run's transaction boundaries. Emitting every judgment as a graph event would violate III's one-event-per-transaction discipline and flood the log with working state. | Keeping the ledger in the event log was rejected: a 200-call investigation would emit ~500 events of transient state whose only consumer is the run itself, and the constitution's own resolution of the tension (spec "What is recorded, and where") puts the *decision record* in the graph and the working material beside it. Keeping the ledger in memory was rejected: FR-046 requires an in-flight investigation to be inspectable, and FR-057b requires a concluded one to be reopened later. **The deviation is justified by derivability and by nothing else**: the schema can be dropped and rebuilt from the **event log** plus a **world regenerated from the recorded backend responses** (which are test data, not a system of record), so the graph remains the source of truth and the store holds no fact the graph does not. T095 is that assertion, executed. |
| One new Go module: `github.com/anthropics/anthropic-sdk-go` (Principle X) | The investigator is a language model; something must speak the Messages API, streaming, tool use, structured outputs, caching and `count_tokens`. | Hand-rolling the HTTP client was rejected: the request/response surface (content-block unions, streaming events, betas, usage) is large and moves, and getting it subtly wrong is a correctness risk in the one place the feature cannot be wrong. The SDK is pure Go over `net/http` and `encoding/json`, adds no cgo and no service. |
| Three algorithms implemented in-repo rather than taken as dependencies: CUSUM + seasonal baseline, Drain-style template mining, BM25 (Principle X) | Goldens are compared byte for byte across amd64 and arm64. Every one of these must be deterministic to the last decimal and stable across library versions for the lifetime of a fixture. Each is 150–300 lines. | Taking `go-changepoint`/`ruptures`-style libraries (PELT, BOCPD) was rejected: they bring hyper-parameters, floating-point paths we do not control, and in BOCPD's case sampling — a fixture that flips between architectures makes the whole harness meaningless, which is the same argument that already forced six-decimal rounding in the ranking formula. A search dependency (Bleve) was rejected: the graph already reduces the candidate set to tens of documents, so an index is a rebuild problem for no measurable gain (research §7). |
| Two new public Go packages (`pkg/worker`, `pkg/backend`) beside `pkg/feeder` (Principle X) | Casey writes workers and backends from this feature on; FR-010 and FR-068 require the contracts to be published and third-party-implementable. | One merged package was rejected: a worker is bound to one source of truth and answers the investigator, while a backend executes one algebra term against one vendor. Merging them would make every worker author import the vendor-side digest machinery and would blur the boundary that is simultaneously the replay boundary, the sanitisation point and the injection barrier. |

---

## Constitution Check — post-design re-evaluation

*Re-run after research.md, data-model.md, contracts/ and quickstart.md were written, and after
reconciling with spec.md Session 2026-09-17 (b) (human-declared intake, the audit result).*

| # | Principle | Post-design finding | Status |
|---|-----------|---------------------|--------|
| I | Graph first | Every structural fact is a `QueryService` answer carried with its `graph_event_ids`; the conclusion re-enters as one event. **The justified deviation, stated plainly: schema `investigation` is a second store, and the constitution says the graph is the system of record.** It is admitted only because it is *derivable* — `investigate replay --from <export>` rebuilds every row from the **event log** plus a **regenerable world**, where the world is itself regenerated from the recorded backend responses, which are test data rather than a source of truth. Nothing in the investigation store is a fact about the system that the graph does not also hold; what it adds is the run's working state. The graph therefore remains the source of truth, and T095 asserts exactly that reconstruction. | PASS with 1 justified item |
| II | Bitemporal or nothing | Both instants on the investigation, on every evidence item and on every worker call. The declared-incident path tightened this: the observed instant is pinned to the **instant of declaration**, and a later edit to severity, title or targets is a separate later event rather than a move of the pin (FR-004a). | PASS |
| III | Event-sourced ingestion | One `record_investigation` per investigation, at conclusion, in one transaction; one event per human action. Judgments, worker calls and ledger updates are never events. Idempotency keys published per event type, including the declaration's `(source, place id, declared instant)`. | PASS |
| IV | Telemetry stays in its backend | Strengthened during design: onset estimation executes **backend-side**, so the series never crosses the digest boundary at all. The graph receives identifiers, parameters, statuses, confidences, rationales, spend and digests; `telemetry-rejection-01` proves a decision record carrying a series is rejected. | PASS |
| V | Evidence-first | Confidence is a normalised naive-Bayes posterior over a closed set that always contains the open hypothesis, computed from published constants and order-independent by construction. The model has no tool that writes a confidence. Deterministic citation checking runs before the fresh-context verifier, and citation validity is a 100% gate. **Calibration is a per-PR artifact, not only a nightly one**: because confidence is computed by the ledger rather than uttered, replaying a recorded trajectory reproduces every confidence exactly, so `ci.yml` computes and publishes the reliability table and the Brier/log-loss score from the recorded trajectories with **no model call at all**; `eval.yml` publishes the live-run calibration over the corpus × k. | PASS |
| VI | Entity resolution is auditable | The subject is resolved before reasoning and the audit is an evidence item. The declared-incident path added a rule: a reference is `parsed_from_declaration` (naming the rule id) or `supplied_by_human`, and **never guessed** — which is principle VI applied to a channel title. | PASS |
| VII | Read-only by default | **Re-examined, because the feature introduces the project's first write path.** FR-057f returns the report to where the incident was declared. This is permitted by constitution **v1.1.0** (amended 2026-09-17, analyze finding C1): a confined exception for posting and editing the engine's own report in a discussion surface, with a credential scoped to that report and separate from every read credential, non-blocking, never a precondition of concluding, never on a system serving production traffic. The design meets every clause; ADR-0005 D8 records the confinement. Everything else stays read-only; no remediation exists. | PASS |
| VIII | Evaluation from day one | Incident fixtures are 001 fixtures plus three additions; no new input format. Trajectory replay gates every PR. The audit's unobservable remainder is corpus work: four categories, one fixture each, a missing category reported as a corpus gap on every run. | PASS |
| IX | Open schema | `investigation.proto` publishes the algebra, the digest and coverage contract, the ledger, the recording format and the ground-truth shape; `graph-additions.proto` states the 001 changes as a reviewable additive diff taking a MINOR bump. | PASS |
| X | Simplicity over cleverness | One binary, one external service, one new module, three in-repo algorithms, two new public packages — each argued in Complexity Tracking. No graph database, no vector index, no second harness, no plugin runtime. | PASS with 4 justified items |
| §Replay CI | Replay, idempotency, calibration on **every** CI run | Confirmed after design, and it is the ledger's determinism that makes (c) affordable: per-PR calibration is computed from the **recorded trajectories** with **zero model calls** and published in the job summary (T009, T098); live-run calibration is `eval.yml`'s (T110, T113). A per-PR gate that needed a model call would either be skipped or would make every pull request cost money, which is how calibration reporting rots. | PASS |

**Adjustments made during design** (the same discipline 001 used):

1. **Onset moved to the backend side of the digest boundary.** Written naively it would have put
   raw samples in the investigator's context — the precise thing principle IV exists to prevent.
   The algorithm stays 002's code, shipped in `pkg/backend/onset` as the algebra term's reference
   implementation; only its *execution* is backend-side. FR-029a and principle IV are both
   satisfied, and neither was when this plan started.
2. **The ledger is re-rendered by appending, never by editing.** Forced by the model family's
   preserved-thinking rule and, independently, by the cache prefix. What began as an
   implementation detail became a published property of the prompt contract.
3. **Exemplars and drill-downs became algebra terms.** "On explicit request" and "a handle you can
   present back" are unimplementable as side channels: either they are terms, or the recorded
   world is not closed and the injection barrier has a hole.
4. **The open hypothesis's prior is the audit's measured ceiling.** `π₀ = 1 − 0.62 = 0.38` today.
   This makes User Story 0 load-bearing rather than merely first, and it is the design's answer
   to "how is `unknown` computed rather than performed".

---

## Self-analysis (a `/speckit-analyze` pass, without tasks.md)

### Coverage

Every FR group maps to at least one artifact: A → data-model `investigations`/`symptoms` +
`contracts/cli.md` + proto §5–6; B → `contracts/worker-sdk.md` + proto §3; C → research §8 +
data-model `hypotheses`/`judgments` + proto §4; D → research §4 + proto §1–2; E → research §9 +
`contracts/incident-format.md` + proto §7; F → plan "budget manager" + proto `BudgetProfile`;
G → research §7; H → data-model `human_*` + proto §5; I → research §12 + `incident-format.md` +
proto §8; J → `contracts/cli.md` + plan observability; K → research §19 + `cli.md`.

Two SCs are not fully verifiable until the corpus exists and are flagged as such rather than
claimed: SC-001's gate value and SC-004's per-bucket calibration (a bucket needs ≥ 20
hypotheses).

### Findings

| # | Severity | Finding | Resolution proposed |
|---|---|---|---|
| F1 | **High** — **RESOLVED** | **The spec assumed a model temperature that this model family does not have.** | **Closed: the spec amendment has been made.** FR-061 now reads "the model configuration (model identifier, reasoning effort, output settings) used in production, and MUST record them" — no mention of temperature anywhere in the spec or the Assumptions. Design Decision 3 below quotes the current wording. Nothing further is owed. |
| F2 | **High** | **`page`'s 90–120 s first-tested target is at risk from the model itself**: single requests on this model family can run for minutes on hard problems. | Designed around, not hoped about: the *first tested hypothesis* comes from the deterministic first wave with no model in the loop (FR-046a), so SC-013's 120 s target does not sit behind a model turn. The model runs at `effort: high` (not `xhigh`/`max`) under `page`, streamed. This must be a measured task in tasks.md, not an assumption. |
| F3 | **Medium** | **FR-042b ("the algebra is the only vocabulary in which a worker may be asked a question") and FR-012 (the graph worker exposes 001's query contract) read as contradictory.** | Resolved by the three-family algebra: graph terms are typed wrappers over the published 001 RPCs, and they are not cross-producted into a world because they are answered deterministically from the replayed event log — which is what US3 already says. Worth a clarifying sentence in the spec. |
| F4 | **Medium** — **RESOLVED** | **D5 named four event types; admitting them requires taxonomy additions the spec did not enumerate** — node types `INVESTIGATION` and `KNOWLEDGE_DOC`, edge types `WATCHES`, `INVESTIGATED`, `CONCERNS`. | **Closed by ADR-0005 D3**, which is the owner decision: the taxonomy members are added, and `KNOWLEDGE_DOC`/`CONCERNS` are **kept** — the constitution requires a durable document to be linked to the graph nodes it concerns, so a document is a node with `CONCERNS` edges and a pointer, never content. D5 additionally gained a fifth event type, `alert.transition` (analyze I5, 002 FR-008b). |
| F5 | **Medium** | **Nothing feeds durable knowledge in v1.** FR-049 requires documents linked to graph nodes; no feeder emits them and document connectors are explicitly later features. | v1 corpus = past investigations (this feature emits them) + documents registered by a human through a small `knowledge link` command. Stated as a scope boundary so SC-014 is measured against what exists rather than against an empty index. |
| F6 | **Medium** | **Report delivery is the project's first write path** (FR-057f), and principle VII's wording is absolute about production systems. | Argued above and confined by design. **An ADR should record it before implementation**, alongside ADR-0005 (model + loop) and ADR-0006 (the 001 change package). |
| F7 | **Low** — **RESOLVED** | **`π₀` from the audit was a design inference**, defensible but not a spec requirement, and it binds the ledger rule to a number that moves as feeders land. | **Ratified in ADR-0005 D9**: `π₀ = 1 − ceiling` from the latest published coverage audit (0.38 at the time of writing), recomputed when the audit is re-run and recorded in every investigation's ledger with the audit id it came from, so a historical confidence stays interpretable. The field is named `no_observed_change`; `H₀` is prose only. |
| F8 | **Low** | **World recording depth is 1.** A drill-down on a drill-down answers `not_recorded` by design. | Published as a manifest field so a fixture states its own limit; counted in the miss rate; raisable per fixture at the cost of recording size. |
| F9 | **Low** | **Calibration on a small corpus is weak**, as the reviewer said. ~10 hypotheses × 8 fixtures × 3 runs ≈ 240 hypotheses, but they are not independent. | Coarse buckets, under-powered buckets reported as such rather than scored, and a Brier score *paired against the previous version on the same fixtures* rather than an absolute. |
| F10 | **Low** | **The `investigator` role is new** in the OIDC role set (`reader`, `decider`, and now `investigator`). | A 001-adjacent change to the server's authz, additive; named here so it is not discovered during implementation. |

No CRITICAL findings: no constitution violation is unjustified, and no requirement is
unimplementable as designed once F1's wording is corrected.

### Constitution tensions, and how each is resolved

- **IV vs. the recording.** An investigation's recording *is* telemetry-derived. Resolved by the
  split the spec itself makes: the decision record (identifiers, parameters, digests) is an event
  in the graph; the bulk lives beside it, keyed and bounded and redacted; the link is a digest.
  Strengthened here by moving onset backend-side so the series never crosses the boundary.
- **I vs. the investigation store.** A second schema could be a second substrate. Resolved by the
  rebuild test: drop it and replay the export. Every structural fact in it carries the graph event
  ids it came from.
- **III vs. the ledger's write rate.** ~500 judgments per investigation cannot each be an event
  without drowning the log in working state. Resolved by one decision record per investigation and
  one event per human action.
- **X vs. the new dependencies.** One module (the Anthropic SDK), three in-repo algorithms
  (determinism across architectures is a hard requirement), two public packages (the worker and
  backend boundaries are genuinely different). Each is in Complexity Tracking with the simpler
  alternative and why it was rejected.
- **VII vs. returning the report.** The first write path in the project, confined to one message
  in the incident's own discussion surface, on a credential that can do nothing else.
- **V vs. a non-deterministic reasoner.** Resolved by putting every number outside the model: the
  ledger computes confidence, the deterministic checker resolves citations, the verifier runs on
  a different model with a fresh context, and pass@1/pass^k measure what one run actually gets.

### What `tasks.md` should start with

The reviewer's order of work, with the plan's additions, and nothing reordered for convenience:

1. **The coverage audit** (`audit coverage`, FR-069–071b) — it is already a published result for
   the owner's organisation, so the task is the *command*, the machine-readable output, the
   comparison of two runs, and wiring `π₀` into the ledger rule. Everything downstream cites it.
2. **The feature 001 change package** (`contracts/graph-additions.proto`) — D1–D5, the
   `alert.transition` convention, the `watches` edge, the future-valid-time fixture. It is
   specified and implemented *in 001*, it is additive and MINOR, and 002's fixtures are recorded
   after it lands. Starting 002's fixtures before this means re-recording them.
3. **The typed query algebra and the `recorded` telemetry backend** (`investigation.proto` §1–2,
   `pkg/backend`, `worker record`) — including onset as a term. Until this exists there is nothing
   to record a world with, and until a world exists nothing can be evaluated.
4. **The hypothesis ledger with computed confidence** (`internal/investigation/ledger`, research
   §8) — including the order-independence property test. It is the central data structure and it
   is model-free, so it can and should be correct before the first model call.
5. **The engine loop, the first wave and the budget manager** — in that order, because the first
   wave is what makes SC-013 achievable without the model on the critical path.
6. **Trajectory replay as the CI gate**, before any evaluation work: it is the plumbing gate and
   every later task depends on it not being retrofitted.
7. **The adversarial and metamorphic fixtures**, then the eval job with its published gate and
   its stated detection power.

Intake (both front doors), the verifier, the renderer and the human channel interleave with 5;
the report-delivery contract lands with 5 but its transport does not, because that is a connector.
