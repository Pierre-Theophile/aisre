# Incident fixtures

An **incident fixture** is a feature 001 fixture plus three things: an `incident:` block naming
the intake and the ground truth, a `world/` layer holding one digest per algebra term, and a
`trajectories/` layer holding the byte-exact record of a run. The authoritative format is
[`specs/002-investigation-engine/contracts/incident-format.md`](../../specs/002-investigation-engine/contracts/incident-format.md).

```text
fixtures/incidents/<fixture-id>/
├── manifest.yaml        # the 001 manifest, plus the `incident:` block and the ground truth
├── events.jsonl         # as in a 001 fixture
├── rejected.jsonl       # as in a 001 fixture (telemetry-rejection-01 only)
├── golden/              # as in a 001 fixture, with golden/pinned/
├── world/
│   ├── index.json       # algebra version, hop radius, drill-down depth, window grid,
│   │                    #   term_key → file, term count, not_recorded count, miss rate
│   └── <term_key>.json  # one digest per algebra term
└── trajectories/        # two per fixture: `--model-free` and `--fake-model` (T093, T099-T101)
```

`fixtures/incidents/` is a **group**, not a fixture: it has no `manifest.yaml` of its own, and
`sre-agent fixture verify fixtures/*/` expands it to its members, so these fixtures are reached
by the same glob CI has always used.

Every fixture declares `provenance.kind` ∈ `synthetic | recorded | derived` and the sanitiser
policy version that produced it (FR-061b). **No recorded production fixture enters this
repository** (ADR-0003 D9): the public corpus is synthetic structural twins, and the recorded
corpus lives in the private repository and runs the same gate with the same thresholds.

## What is shipped today

Sixteen fixtures. Every one of them passes `sre-agent fixture verify` — replay from empty,
goldens, pinned goldens, double delivery, six seeded shuffles, expect-rejected — plus the
`incident` and `trajectory-replay` steps described below, at a world miss rate of **0.0000**
against a threshold of 0.05 and with **two recorded trajectories each**, replayed byte-identical
with zero network.

`prior rank` is `incident.ground_truth.prior_rank_of_culprit`: the deterministic ranker's own
position for the culprit, read off that fixture's `golden/diff.*.json` rather than estimated.
`0` means the prior does not rank the culprit at all — which is the right answer where the
ground truth is `unobserved` and there is no culprit to rank.

| id | provenance | ground truth | world terms | miss rate | trajectories | prior rank |
|---|---|---|---|---|---|---|
| `rollout-regression-01-incident` | derived from `rollout-regression-01` | `change_induced` — payments@rev7 | 607 | 0.0000 | 2 | 1 |
| `declared-incident-01` | synthetic | `unobserved` | 156 | 0.0000 | 2 | 0 |
| `human-fact-reopen-01` | synthetic | `change_induced` — payments@rev7 | 316 | 0.0000 | 2 | 1 |
| `unknown-feeder-gap-01` | synthetic | `change_induced` — payments@rev7, not knowable at `fired_at` | 312 | 0.0000 | 2 | 0 |
| `two-simultaneous-01` | synthetic | `change_induced` — payments@rev7, with a `not_separable` twin | 316 | 0.0000 | 2 | 2 |
| `merged-alias-01` | derived from `ambiguous-identity-01` | `unobserved` | 32 | 0.0000 | 2 | 0 |
| `telemetry-rejection-01` | synthetic | `unobserved` | 212 | 0.0000 | 2 | 0 |
| `injection-01` | synthetic | `change_induced` — payments@rev7 | 316 | 0.0000 | 2 | 1 |
| `knowledge-scope-01` | synthetic | `change_induced` — payments@rev7 | 330 | 0.0000 | 2 | 1 |
| `unobserved-latent-bug-01` | synthetic | `unobserved`, category `latent_bug` | 288 | 0.0000 | 2 | 0 |
| `unobserved-client-config-01` | synthetic | `not_change_induced: client_side_configuration` | 288 | 0.0000 | 2 | 0 |
| `unobserved-business-data-01` | synthetic | `not_change_induced: business_data_change` | 288 | 0.0000 | 2 | 0 |
| `unobserved-credential-leak-01` | synthetic | `unobserved`, category `credential_leak` | 290 | 0.0000 | 2 | 0 |
| `slow-burn-01` | synthetic (adversarial, T099) | `change_induced` — payments-pool@rv5512 | 337 | 0.0000 | 2 | **4** |
| `distant-culprit-01` | synthetic (adversarial, T100) | `change_induced` — fx-rates@rev4 | 523 | 0.0000 | 2 | **4** |
| `adjacent-decoy-01` | synthetic (adversarial, T101) | `change_induced` — ledger@rev5 | 436 | 0.0000 | 2 | **4** |

What each fixture asserts is in its own `manifest.yaml`, at length; that description is the
documentation and this table is the index into it. Five thousand and forty-seven recorded terms
across the sixteen; the whole directory is 25 MB.

`bin/sre-agent audit coverage gaps --audit docs/evaluation/coverage-audit-2026-09.json` reports
**zero gaps** against this corpus: every category of the 2026-09 audit's unobservable remainder
(`client_side_configuration`, `business_data_change`, `credential_leak`) has a fixture, and
`latent_bug` — the category that is symptom-visible under the feeder set in force — has one too.

### The three adversarial fixtures (FR-062b, SC-021)

SC-021 asks for at least three fixtures on which the deterministic prior does **not** rank the
culprit first, so that lift over the prior (FR-061a) is measurable where it matters rather than
only where the ranker was already right. These are they, and on each of them the prior places
the culprit **fourth**:

| id | what defeats the prior | culprit | prior's own first choice |
|---|---|---|---|
| `slow-burn-01` | **time.** The culprit is 3 h 00 m before the alert, so its temporal weight is `exp(-10800/1800)` = 0.002479 and the term that carries half the score contributes nothing | a ConfigMap edit lowering payments' connection pool from 64 to 8 at 11:32 | the autoscaler widening storefront at 14:18 (score 0.613545 against the culprit's 0.276239) |
| `distant-culprit-01` | **distance.** The culprit is 3 hops out along `checkout → payments → ledger → fx-rates`, so `topological = 1/(1+3)` = 0.25 costs it 0.0875 of score against every one-hop candidate | an fx-rates rollout at 14:12 | the autoscaler widening storefront at 14:26 (0.709365 against 0.444209) |
| `adjacent-decoy-01` | **both, and a remediation.** A human rollout two minutes before the page, one hop out, beats a real cause 25 minutes old and two hops out | a ledger rollout at 14:07 | the on-call's own `shop/inventory@rev9` at 14:30 (0.742753 against 0.433966) |

Three things about them a reviewer should know before reading the manifests.

- **The ranks are measured, not asserted.** Each fixture's `checkout-diff` golden is a `diff`
  over the investigation's own window at `hops: 2` — the same call
  `engine.NeighbourhoodHops` makes — so `golden/diff.checkout-diff.json` *is* the prior's
  answer, and `prior_rank_of_culprit` is read off it.
- **`distant-culprit-01` is the one fixture with `world.hop_radius: 3`.** At the corpus default
  of 2 the recorded world would hold no pointer on fx-rates and the ground truth would name
  evidence the corpus cannot produce. The radius is the *evidence's* reach; the engine still
  reads a two-hop neighbourhood, so its deterministic first wave cannot resolve the culprit's
  target and falls back to the subject. That gap is the fixture.
- **They are not solved by the model-free engine, and they are not meant to be.** The recorded
  model-free trajectories end `completed` with the culprit un-promoted on all three. That is
  what makes lift measurable: the prior contributes 1/4 of a reciprocal rank and everything
  above it has to be earned with evidence that is in `world/` and that only a wider
  investigation asks for.

The metamorphic variants (`*-culprit-deleted`, `*-decoy-injected`, `*-time-shifted`,
`*-name-permuted`) remain a generator rather than files (`fixture derive`, T107).

## The `incident` verification step

`fixture verify` runs one extra step on a fixture carrying an `incident:` block:

1. **the ground truth is well formed.** `LoadManifest` refuses a malformed one, so reaching the
   step at all is the assertion; the step reports the class, the category, the causal-path
   length, the decisive-predicate count, the decoy count and the provenance, so a reviewer reads
   them off the verification output. The same validation runs with no database in
   `internal/fixture/incident_test.go`, which walks this directory rather than a list — a
   fixture added here is validated whether or not anybody remembered a list;
2. **the recorded world is the shape the manifest declares** — algebra version, hop radius,
   drill-down depth, term count against the file count — and its miss rate is at or below the
   fixture's own `miss_rate_threshold`. A world re-recorded at a wider hop radius would lower
   the miss rate with nothing in the diff to say why; this is the check that makes
   `fixture record-world`'s manifest-driven shape stick;
3. **the trajectory layer is reported, never required.** The step counts what is in
   `trajectories/` and passes whether or not anything is; *replaying* them is the separate
   `trajectory-replay` step (T096), and the separation is deliberate — the corpus has to be
   verifiable before the replayer lands. Every fixture here now carries two, so both steps are
   exercised on every fixture on every pull request.

## Two conventions this corpus adds to the published shape

Both are additive, both are optional, and both exist because a field the format leaves implicit
turned out to be the thing a reviewer most wants to see. They are described here rather than in
the contract because the contract is owned by the spec; fold them in when it is next revised.

**`ground_truth.decoys`** — a list of `{entity, causal_role, why}`. `exonerating_evidence` is
keyed by decoy already; this says *what each decoy is*, which is the difference between "we
ruled it out" and "we understood it". The published roles:

| role | meaning |
|---|---|
| `candidate_effect` | it happened **after** the onset, so it is downstream of the symptom (an autoscaler or an operator reacting) |
| `coincident` | near the onset, adjacent in the graph, and touching nothing on the causal path |
| `stale` | old enough that the symptom would have appeared earlier |
| `out_of_scope` | outside the neighbourhood; it must be **excluded**, not ranked low |
| `unattached` | its target is a thing no source has described |
| `not_separable` | the evidence in this fixture cannot tell it from the culprit (FR-025) |

Every decoy must have an `exonerating_evidence` entry, and the manifest loader refuses a fixture
where one does not: a decoy with nothing against it is a second culprit.

**An empty exonerating-evidence list** means "the evidence in this fixture cannot rule this
candidate out", and it is legal for exactly one role, `not_separable`. `two-simultaneous-01` is
the fixture it exists for: a rollout and a ConfigMap edit at the same instant, on the same
target, carried by the same pods and therefore tagged with the same `service.version`. Every
predicate that convicts one convicts the other. Writing `[]` there is a statement, not an
omission, and the loader enforces the distinction.

The tie runs all the way through the prior: `golden/diff.checkout-diff.json` scores both changes
at 0.61016 and separates them only by the deterministic `change_id` tie-break, which puts the
ConfigMap edit first. The culprit's prior rank on that fixture is therefore **2**, not 1, and the
table above says so. It is the honest number — the fixture's whole subject is that nothing in the
evidence prefers one over the other, and a prior rank of 1 would have claimed the ranker broke
the tie in the culprit's favour.

## Recording a fixture

```sh
bin/sre-agent fixture record            fixtures/incidents/<id>   # golden/ and golden/pinned/
bin/sre-agent fixture record-world      fixtures/incidents/<id>   # world/, from the manifest's own shape
bin/sre-agent fixture record-trajectory fixtures/incidents/<id> --model-free
bin/sre-agent fixture record-trajectory fixtures/incidents/<id> --fake-model
bin/sre-agent fixture verify            fixtures/incidents/*/     # the whole contract

# Both recordings, without a PostgreSQL of your own:
SRE_AGENT_RECORD=1 go test ./internal/query -run TestRecordShippedFixtures
```

The order matters: goldens, then the world (which runs the engine over the graph the goldens
were recorded from), then the trajectories (which are answered from that world). Running them
in any other order records a layer against a substrate that has already moved.

`fixture record-world` takes the focus, window grid, hop radius and drill-down depth from the
fixture's own manifest rather than from flags, so a re-record cannot silently change a fixture's
shape. `worker record` is the flag-driven command underneath it, and is how a world is first
recorded for a fixture that has no manifest yet.

**All three recordings are byte-reproducible.** Re-running the whole sequence over the whole
corpus leaves `git status` empty, and that property is the check a reviewer should run before
believing anything else on this page.

**The telemetry in every `world/` here is generated**, by `pkg/backend/synthetic`, from the
fixture's own graph — a stand-in until feature 003's GCP backend records a real one. Every
fixture therefore carries `provenance.kind: synthetic` (or `derived`, from a synthetic parent),
and no accuracy claim resting on these worlds may be published as though it rested on a
recording.

## The two numbers that decide a world's size

A world is the cross product of **(every pointer on every node within the hop radius) × (the
window grid) × (the recorder's statistic set: `ERROR_RATE`, `P95`, `RATE`)**, plus
`monitor_state`, `new_log_patterns` and `onset` per pointer, `errors_by_version` per pointer
that publishes a `version` join key, `error_spans` per edge, and the **depth-1** drill-downs and
exemplars all of those mint. The drill-downs are typically 60 % of the files.

Two decisions follow, and every fixture here takes both:

- **one grid entry, not two.** Each further entry doubles the directory and settles nothing the
  first does not. The entries are in the manifest, so widening the grid is an edit a reviewer
  sees;
- **the grid is centred on the estimated onset, never on the alert instant.** This is the
  correction the Phase 4 review asked for and it is the difference between a useful world and a
  useless one: `rollout-regression-01-incident`'s alert fires at 14:32 and its onset is at
  14:20, so a pair centred on the alert puts the first twelve minutes of the symptom into the
  *baseline* half and every comparison in the world reads flat. Centred on the onset, payments'
  error rate reads 0.44 % before and 10.7 % after.

There is a third entry in every world that is **not** in any manifest's grid, and it is the
one that made the whole corpus roughly double in size when the Phase 8 re-record ran. Since
Phase 7 the recorder covers `engine.ReferenceInstants` as well — the alert instant, because the
branch in which the metrics worker produces no onset estimate and the engine falls back to
`fired_at` is a branch the recording has to hold too — and then runs the engine itself and
records every term that run issues. The twelve worlds recorded in Phase 6 went from 103–145
terms to 212–302, and `merged-alias-01` from 16 to 33. That is what the trajectory-replay gate
costs, and the miss rate of 0.0000 on every fixture is what it buys: before the engine pass
existed the gate reported 15 `not_recorded` of 15 on the one fixture that had a trajectory.

Under those caps thirteen of the sixteen worlds come in at or under 1.3 MB. The exceptions are
`rollout-regression-01-incident` at 2.3 MB, because it inherits 001's full shop — sixteen nodes
in checkout's two-hop neighbourhood, fifteen telemetry pointers, twenty-three edges —
`distant-culprit-01` at 1.9 MB, which pays for its third hop, and `adjacent-decoy-01` at 1.6 MB.
The MVP fixture's world replaced the one T039 recorded under
`fixtures/rollout-regression-01/`, which is gone: T039's note says this task moves the layer
into the incident fixture, and keeping two worlds over one event stream would have meant
maintaining a near-duplicate. `docs/schema/algebra.md` and `docs/schema/digests.md` take their
worked examples from the new location.

## How the synthetic telemetry travels, and when it refuses to say anything

**A degradation propagates.** `pkg/backend/synthetic` degrades the entities a change targets and
then carries that degradation callee-ward along the call edges, by the formula published in
`specs/002-investigation-engine/research.md` §Backend and estimator realism:

```
E(v, t) = D(v, t) · (1 + Σ_{v → c} α · (E(c, t − Δ) − 1)),   α = 0.6,  Δ = one resolution step
```

`D` is the entity's own degradation — what the changes targeting it do to it — and the sum runs
over its callees. Applying it recursively gives a service `h` hops from the burning one an
attenuation of `0.6^h` and a delay of `h·Δ`, which is what a chain of timeouts looks like: each
hop loses some of the signal and gains some of the lag. The walk is bounded at **four hops**, one
more than the longest chain any fixture in this directory has, and the whole behaviour can be
switched off by a scenario that sets `Propagation: NoPropagation` — kept as a deliberate switch
rather than as something you get by omission.

So in `rollout-regression-01-incident` payments' error rate rises by the change's own factor and
**checkout's own metric moves too**, attenuated and one step later, which is what makes `onset`
on the alerting subject — the first telemetry term every investigation asks — a question about a
real signal rather than an estimate over noise. The `error_spans` on the `checkout → payments`
CALLS edge still carry `version: rev7`, and they are still the predicate that ties the symptom to
the culprit rather than to payments in general.

**An onset that is not there is refused, in so many words.** The estimator
(`pkg/backend/onset`) reports a crossing as an onset only when

* the shifted level sits at least `k_effect = 3.0` scaled median absolute deviations from the
  baseline level — under the generator's own ±4 % wobble, seeded noise cannot manufacture that —
  and
* it is held for at least `min_sustained_points = 5`, which at a one-minute resolution is five
  minutes: the shortest thing this project is willing to call a symptom rather than a spike, and
  the same duration the corpus's own monitors are specified over ("above 5 % for 5 minutes").

A series that crosses but clears neither bar answers the typed `unavailable: no_onset_detected`
rather than an instant. That is why no fixture in this directory documents a spurious onset any
more: `adjacent-decoy-01` used to record the engine estimating 13:27 from a crossing in flat
noise — measured after the fact at 1.30 MADs, well under the floor — and an estimate this
corpus's telemetry does not support is now absent rather than wrong.

Every ground truth in this directory is still written over what its world actually holds rather
than over what a real backend would hold: a ground truth naming evidence the corpus cannot
produce fails every run for a reason that has nothing to do with the engine.

**Nobody sees the future.** Every predicate below is stated over a window that ends at or before
its fixture's `fired_at`, because that is the instant the investigation observes the world at and
telemetry after it does not exist (constitution II). A backend truncates a window that runs past
it and says so in the coverage block; a window wholly past it answers `no_data` naming the
horizon. A `knowability_time` after `fired_at` is therefore a deliberate statement that the
answer is **not** knowable when the question is asked — `unknown-feeder-gap-01` is the fixture
that exists for it — and not an artefact of a window that ran into the future.

## The `trajectories/` layer, and what the recordings in it do and do not prove

**Every fixture in this directory carries two network-free recordings**, and
`rollout-regression-01-incident` carries a third from a live run. All are recorded by
`sre-agent fixture record-trajectory <dir>`, which runs the investigation the fixture's
`incident:` block states against the fixture's own two sources — the graph, replayed from
`events.jsonl` into a database of its own, and the telemetry, answered from `world/`.
`rollout-regression-01-incident` was the first to carry any (T093); the other fifteen were
recorded in Phase 8 so that the trajectory-replay gate covers the whole corpus rather than one
fixture of it.

That includes the fixtures whose ground truth is `unobserved` or `not_change_induced` and the
ones that expect `unknown`. There was a case for leaving those out — there is nothing for the
engine to find, so what is a recording of the search worth? — and it is the wrong case. A gate
that only covers the fixtures with an answer cannot catch a regression that turns *no answer*
into a confident wrong one, which is the failure SC-023 exists for. `telemetry-rejection-01`
and `injection-01` record and replay too: nothing in this corpus is exempt, and no fixture
needed to be forced or skipped.

- **The model-free run holds zero `model_request` records.** The engine ran with no model client
  at all: the provisional ranking, the onset estimate, the causal ordering and the deterministic
  first wave, then a typed stop. That is a complete investigation with a smaller claim (tasks.md
  §MVP), and the gate over it still exercises the worker path, the ledger path and the stop path
  byte for byte.
- **The fake-model run holds three.** Its turns come from `internal/investigation/model/modeltest`
  through the same `model.Client` and the same `http.RoundTripper` seam a live run uses — one
  proposed hypothesis, one batch of judgments, then a verdict. It exists so that the
  **model-request digest matching path** the gate is built on is exercised by the corpus rather
  than only by a unit test: without a `model_request` in the recording, the strict replaying
  transport is never asked to match anything.
- **The live run holds the real turns.** `rollout-regression-01-incident` also ships
  `run-1a532f7a56a3adfb.jsonl`, recorded with `fixture record-trajectory … --live` against the
  production model configuration (`config/model.yaml`: Mistral, serving `zai-glm-5-3` as the
  investigator). Three real model turns, eight tool calls, and the culprit
  `k8s.change=shop/payments@rev7` ranked first at confidence 0.708.

**A live recording does not replace the other two; it joins them**, and the three answer different
questions. The model-free run is the deterministic engine's own claim, provable with no vendor at
all. The fake-model run is the network-free exercise of the tool path, and it is the one every
pull request replays — it must keep working with no credential in the environment, which a live
recording cannot promise. The live run is the production-configuration recording FR-061 asks for:
what the configured models actually did, byte for byte, replayable afterwards with zero network.
Deleting any of the three would lose something the other two do not have.

All three are byte-reproducible. Re-running `fixture record-trajectory` on an unchanged fixture writes
the same bytes to the same path, because the run id is derived from the digest of the trajectory
itself, the clock is pinned to the fixture's own `fired_at`, and `duration_ms` — wall-clock time,
which is not part of what "the same answer" means — is cleared before the file is written. The
first wave issues its calls in parallel, so each block of worker calls is written in canonical
digest order rather than in the order the scheduler happened to produce.

### The corpus-alignment finding, and how it was closed

The first run of the gate passed and reported a **replay miss rate of 1.0** — every telemetry
term the engine issued missed the recorded world, 15 of 15. It is worth keeping the diagnosis
here, because the failure was invisible in both files and the fix is a rule rather than a patch.

The world held one grid entry, centred on the estimated onset at 14:20 with a width of 900 s. The
engine's `onset` term searched `[fired_at − lookback, fired_at]` — 13:02 to 14:32 — while the
world's `onset` terms had been recorded over the grid-derived window 14:05 to 14:35. Different
term keys, so `not_recorded`; so no onset estimate; so the engine fell back to the alert instant
as its reference and asked every comparison at 14:32 ± 1800 s while the world held 14:20 ± 900 s.
One missed term cascaded into all of them. Neither derivation was wrong on its own — that is the
whole problem with having two.

**What closed it: the recorder no longer derives the engine's windows, it runs the engine.**
`fixture record-world` now makes an engine pass before the cross product — the same wiring
`fixture record-trajectory --model-free` uses, over the same replayed graph, against the world's
own generator — and records every term that run issues. The world is a superset of the engine's
plan by construction, not by a derivation that has to be kept in step. The window arithmetic that
*is* shared lives in one exported place (`internal/investigation/engine/plan.go`:
`OnsetSearchWindow`, `CompareHalfWidth`, `ComparePairs`) and both the engine and the recorder call
it.

Two engine defects were fixed at the same time, because they were suppressing the same class of
miss. `engine.DecodeCall` now stamps both instants on every graph read, so a `pointers` call is no
longer refused by 001's graph; and the engine learns the canonical-entity-id ↔ reference
translation from the neighbourhood it reads first, so a change's `target_entity_ids` reach
`pointers` as references and `error_spans` names its two ends by entity id, which is what the term
and the recording both mean by those fields. The two shims in
`internal/cli/fixture_trajectory.go` that used to paper over both are gone.

The result: **565 terms, miss rate 0.0000, 0 not_recorded of 14 checked**, against the fixture's
own `world.miss_rate_threshold` of 0.05. The world grew from 1.2 MB to 2.3 MB, which is what the
gate costs and is cheap for what it buys — the containment is asserted directly in
`internal/cli/fixture_trajectory_test.go` so that it cannot quietly lapse again.

The Phase 8 re-record extended that result to the whole corpus, which is the thing the finding
above was always about: **sixteen fixtures, thirty-two trajectories, every one replayed
byte-identical with zero network, and a miss rate of 0.0000 on every fixture** — 0
`not_recorded` of 0 to 14 terms checked, depending on how far each fixture's engine run gets.
One fixture at 0.0000 proves the alignment is reachable; sixteen prove it is the recorder's
property and not that fixture's luck.

Track L added the thirty-third trajectory, and it moved one number worth naming: the live run on
`rollout-regression-01-incident` asks for **3 terms of 20 the recorded world does not hold**, a
miss rate of 0.0577 on that recording. That is the model asking a question the deterministic wave
never asks, which is exactly what a live investigator is for, and all three are answered as
`not_recorded` — a typed refusal with a reason, not a silence (FR-027). The *world's* own miss
rate is still 0.0000.

**Widening the world to close that gap was tried and does not converge**, and it is worth writing
down so nobody tries it twice. Adding the three window-grid entries the live run reached for made
the world hold them — and 2.5 MB of world became 6.0 MB, and the *recording* then disagreed with
the world it was recorded against, because the trajectory had recorded those terms as
`not_recorded` and the world now answered them with data. Re-recording the trajectory against the
wider world would let the model spend its turns somewhere new, and the next gap is the new gap. A
world is a recording; a live investigator can always reach outside it. So the invariant asserted
in `internal/cli/fixture_trajectory_test.go` is the one that is actually true: **strict
containment for every model-free recording** — the wiring `fixture record-world` reproduces, and
the thing the Phase 7 finding below was about — and, for a live recording, that every term the
world cannot answer comes back typed rather than silent.
