# The Replayable Incident, extended

This is feature 001's [fixture format](../../001-temporal-graph-core/contracts/fixture-format.md)
plus what an agent is scored on. **No new input format** (FR-063): an incident fixture is a 001
fixture directory with three additions — an `incident:` block in the manifest, a `world/`
directory and a `trajectories/` directory — and everything 001's `fixture verify` already does to
it still happens.

```text
fixtures/incidents/<incident-id>/
├── manifest.yaml            # 001's manifest + the `incident:` block below
├── payloads/                # 001: recorded raw source payloads
├── events.jsonl             # 001: accepted events, canonical JSON, with observedAt
├── golden/ , golden/pinned/ # 001: query answers, and the mandatory observed-time-pinned twin
├── world/                   # NEW: the recorded world (layer 2)
│   ├── index.json           # algebra version, hop radius, drill-down depth, window grid,
│   │                        #   term_key → file, term count, not_recorded count, miss rate,
│   │                        #   live coverage (passes, model config digests, term keys)
│   └── <term_key>.json      # one digest per algebra term
└── trajectories/            # NEW: recorded runs (layer 1)
    └── <run-id>.jsonl       # model + worker requests and responses, ledger updates, stop
```

## The `incident:` block

```yaml
incident:
  question:                              # what the on-call was asked
    transport: monitor                   # monitor | human_declared | explicit_reference
    origin_system: datadog
    origin_ref: "monitor:12345:group=env:prod"
    statement: "checkout error rate above threshold"
    fired_at: 2026-09-01T14:32:00Z
    subject: otel.service.name=checkout
    lookback: 90m
    profile: page

  ground_truth:                          # FR-061b — the published shape, every field required
    culprit: k8s.change=shop/payments@rev7   # or `unobserved`, or
                                             # `not_change_induced: <category>`
    causal_path:                         # graded separately, partial credit
      - entity: k8s.change=shop/payments@rev7
        via: changed-by
      - entity: otel.service.name=payments
        via: calls
      - entity: otel.service.name=checkout
    decisive_evidence:                   # predicates over digests, never strings
      - term: {errors_by_version: {pointer: "metric:payments:http.errors", window: "14:20..14:35"}}
        field_path: versions[version=rev7].error_rate
        op: gt
        value: 0.2
    decoys:                              # every non-culprit candidate the prior ranks, with its role
      - change: k8s.change=shop/storefront@scale-1415
        causal_role: candidate_effect    # candidate_effect | coincident | stale | out_of_scope |
                                         # unattached | not_separable
    exonerating_evidence:                # per decoy; an EMPTY list is legal only for a decoy whose
                                         # causal_role is not_separable and means "cannot be ruled out"
      k8s.change=shop/storefront@scale-1415:
        - term: {onset: {pointer: "metric:checkout:http.errors", search_window: "13:00..14:32"}}
          field_path: estimated_onset
          op: lt
          value: 2026-09-01T14:15:00Z    # the scaling started after onset ⇒ candidate effect
    knowability_time: 2026-09-01T14:36:00Z   # before this instant, `unknown` is CORRECT
    onset: {at: 2026-09-01T14:21:30Z, tolerance_seconds: 120}
    prior_rank_of_culprit: 1             # so lift is measurable where the prior fails
    provenance:
      kind: recorded                     # synthetic | recorded | derived
      sanitiser_policy_version: "1.2.0"
      derived_from: null                 # derived fixtures name their parent…
      transformation: null               # …and the transformation applied

  world:
    hop_radius: 2
    drill_down_depth: 1                  # deeper handles answer not_recorded, by design
    window_grid:                         # SUPPLEMENTS the engine's own plan; see below
      - {reference_at: 2026-09-01T14:21:30Z, width_seconds: 900}
      - {reference_at: 2026-09-01T14:21:30Z, width_seconds: 3600}
    algebra_version: "1.0.0"
    miss_rate_threshold: 0.05
    shape: step                          # the degradation shape; see below. Default: step

  runs: 3                                # extended to 7 when the first 3 disagree
```

### What `world:` declares, and what it does not

`fixture record-world` records the **union of two sets**, and the block above declares only the
second of them.

1. **Every term the deterministic engine issues for this fixture's own `incident.question`.** The
   recorder runs that engine — the same wiring `fixture record-trajectory --model-free` runs, over
   the same replayed graph — against the world's generator, and records everything it asks. This
   set is not declared anywhere because it is not a choice: it is whatever the engine's published
   plan derives from the question, and writing it into the manifest would be writing down an
   answer that can go stale.
2. **The cross product** of (every pointer in the neighbourhood) × (`window_grid`) × (the
   recorder's statistic set), plus `monitor_state`, log patterns, `onset`, `errors_by_version`
   where a version join key exists, `error_spans` per edge, and the depth-1 drill-downs those
   answers mint. This is the supplement a *model's* exploration draws on, and it is declared,
   because it is the fixture's own shape and widening it must be an edit a reviewer sees.

The grid also carries the engine's own comparison pairs, derived from the question through the
same exported functions the engine calls (`engine.OnsetSearchWindow`, `engine.ComparePairs`), so
that the branch where onset cannot be estimated and the alert instant becomes the reference is
covered too.

### `shape:` — how the culprit degrades what it touches

`shape` names the degradation shape the generator stamps onto the scenario it builds from this
fixture's graph. It is additive and optional: a manifest that names none gets `step`, which is
what every world recorded before the shapes existed holds.

| name | what it generates |
| --- | --- |
| `step` | *(the default)* the culprit's targets jump to the full factor at the change instant and stay there, and the degradation propagates to their callers. This is what a rollout regression is. |
| `slow-burn` | a **ceiling** is lowered onto the target (a pool sized just above its baseline demand) under a **rising demand curve** (a 0.6 diurnal amplitude, trough at 04:00 UTC, peak at 16:00). The target is barely affected while demand sits near the ceiling and worse in proportion to the excess above it, so the same change is mild when it lands and several times worse hours later. |
| `distant-culprit` | a step at the far end of a call chain, which reaches the alerting service attenuated once per hop and one resolution step later per hop. Parameters as `step`; it is named separately so a manifest can say what it relies on. |
| `adjacent-decoy` | a step two hops out, propagated, so the alerting service's own series moves and `onset` on the subject has something real to find. Parameters as `step`, for the same reason. |

Two of the four share their parameters with `step`, and saying so is more useful than inventing a
difference to justify four entries. The exact numbers live in `pkg/backend/synthetic/shape.go`
rather than in YAML, so a shape cannot be tuned per fixture until a fixture passes; the manifest
chooses between published shapes and nothing else.

A name the generator does not publish is **refused** — by the manifest loader, and again by the
recorder — with the list of names. A fixture that asked for a shape and silently got another one
would be a different fixture under the same id, and its world would answer a story its prose does
not tell.

The shape is a property of the *story*, not of the graph: nothing in an event log says whether a
change made its targets worse at once, over an hour, or only once demand climbed into a ceiling it
lowered. That is why it is declared here and why changing it requires re-recording `world/` — the
shape decides every number in it.

3. **Every term a *live* investigator asked**, when `fixture record-world --live-passes N` was
   run (N defaults to 0). The recorder runs the production model configuration against the same
   generator N times and files every term those runs issue. This set is not declared in the
   manifest either, for the same reason as set 1 and one more: it is not a *choice*, it is a
   measurement, and it changes when the model or the prompt changes.

   It exists because sets 1 and 2 are what a *deterministic* engine and a *reviewer* ask, and a
   live model asks neither. The first live evaluation of this corpus excluded 15 of its 16
   fixtures on their miss rate — 0.111 to 0.667 against the 0.05 bar — with no defect in the
   model, the engine or the worlds: they were simply questions nobody had recorded an answer to.
   The generator can answer any in-algebra term, so the fix is to record what a live run asks
   rather than to widen the admission bar until the corpus passes.

   Nothing about the recording depends on what the model *said*. The model chooses which
   questions are put; the generator — deterministic, seeded by the fixture — answers every one of
   them. That is why the coverage is reproducible without a model: `world/index.json` names the
   live term keys in a `liveCoverage` block, and a later `--live-passes 0` re-record re-asks
   exactly those terms and writes the same bytes.

   ```json
   "liveCoverage": {
     "passes": 3,
     "modelConfigDigests": ["<sha256 of config/model.yaml>"],
     "termKeys": ["...", "..."]
   }
   ```

   `passes` is cumulative across recordings and `modelConfigDigests` holds every configuration
   that contributed, so a world accumulated across a model change says so rather than averaging
   it away. The block is **absent** from a world no live pass contributed to, which is what keeps
   such a world's bytes — and its index digest — exactly as they were.

Why set 1 exists at all: a recorder that enumerated only a grid has to *derive* the windows the
engine will use, and two derivations of "the window" from different inputs do not stay equal. The
first version of this format had exactly that, and the result was a fixture whose world and whose
engine intersected in nothing — `15 not_recorded of 15 checked`, miss rate 1.0000, with no single
line in either file that was wrong.

## What `fixture verify` does with it

1. **Everything 001 already does**: replay from empty, answer every manifest query at its stated
   instants and again with observed time pinned, byte-compare both against `golden/`, double-deliver,
   shuffle within each source's reordering window.
2. **Trajectory replay** of every file in `trajectories/`: byte-identical, zero network, and any
   unmatched request fails loudly naming the worker, capability and parameters. This is the
   plumbing gate and it runs on every pull request.
3. **World replay**, `runs` times: the engine investigates `incident.question` with every worker
   in recorded mode and the graph replayed from `events.jsonl`. Every in-algebra question is
   either served from `world/` or typed `not_recorded` — never improvised, never live.
4. **Scoring** against `ground_truth`, producing the report rows in
   `docs/evaluation/investigation-metrics.md`.

## Grading rules that are easy to get wrong

- **`knowability_time` makes `unknown` correct.** Asked before that instant, `unknown` scores as
  a pass and naming the culprit scores as a failure. Asked after it, the decisive evidence is
  expected to be found.
- **A culprit-deleted variant expects `unobserved`**, with the symptoms still localised. A run
  that names a decoy fails.
- **Time-shifted, name-permuted and far-decoy variants must not change the verdict.** Name
  permutation is there to check that the engine ranks on structure rather than on
  guilty-sounding names; a divergence fails the run and names the variant.
- **The miss rate gates the fixture, not the engine.** Above `miss_rate_threshold` the fixture is
  reported as insufficient and is not scored.
- **The prior's own rank is recorded per fixture**, so lift is measured where it matters.

## Derived fixtures

`aisre fixture derive <dir> --transform <t>` writes a new fixture whose `provenance` names
its parent and its transformation, and whose `ground_truth` is the transform's published image of
the parent's. A derived fixture cannot exist without the invariant it is graded on, because the
invariant is part of the transform's definition.

## Decoy roles (added 2026-09-17 with the first incident corpus)

`ground_truth.decoys[].causal_role` names why a ranked non-culprit is not the cause, so the
grader can check the engine gave the *right* reason, not only the right rank:

| role | meaning | expected engine behaviour |
|---|---|---|
| `candidate_effect` | it started after the estimated onset | exonerated by the causal ordering, shown as an effect |
| `coincident` | unrelated change inside the window | refuted by evidence on its own target |
| `stale` | far earlier than onset, prior ranks it low | refuted or left untested with a stated reason |
| `out_of_scope` | beyond the hop margin | not a candidate; must not be invented |
| `unattached` | its target never resolved | reported unattached at reduced rank |
| `not_separable` | same instant, same target as the culprit | both reported, tie-break stated, neither exonerated |

An empty `exonerating_evidence` list is permitted only for `not_separable`; for every other role
at least one predicate must exist, otherwise the fixture fails validation.
