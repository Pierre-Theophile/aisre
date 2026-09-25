<!-- SPDX-License-Identifier: Apache-2.0 -->

# The replayable incident

The constitution defines one input format for evaluation and does it in a single sentence:

> A **replayable incident** is defined as: a graph snapshot at T + the telemetry window
> pointers + the validated root cause. The eval harness MUST accept this triple as its input
> format.
>
> — [`.specify/memory/constitution.md`](../../.specify/memory/constitution.md), principle VIII

This page says what each third of that triple *is*, in the files the repository already ships,
so that the agent feature does not get to invent an input format later. Nothing here is
aspirational: every field named below exists today, is produced by `aisre fixture record`,
and is gated on every pull request by `aisre fixture verify` (T081).

The short version:

| the triple | what it is here | where it lives |
|---|---|---|
| graph snapshot at T | the fixture's `events.jsonl` replayed from empty, read at `valid_at` with `observed_at` pinned to T | `fixtures/<id>/events.jsonl` + a manifest query + `golden/pinned/<kind>.<name>.json` |
| telemetry window pointers | the answer to the `pointers` query at T — selectors into the backends that own the telemetry, never the telemetry | a `kind: pointers` manifest query + its golden |
| validated root cause | `ground_truth.culprit_change`, the change node a human confirmed caused it | `fixtures/<id>/manifest.yaml` |

## 1. The graph snapshot at T

A snapshot is not a file. It is a **fixture plus an as-of**, and that is deliberate: a serialized
snapshot is a claim about the graph, while a fixture is the evidence that produces it.

- **The events** are `fixtures/<id>/events.jsonl` — the accepted event stream, canonical JSON,
  each line carrying the observed time the graph assigned when it first accepted the event
  (FR-023). Replaying them into an empty database reproduces the graph byte for byte, which is
  what makes "the snapshot at T" reproducible rather than remembered.
- **T is two instants, not one** (constitution II). A manifest query names `valid_at` — when the
  fact was true — and optionally `observed_at` — when the system knew it. The incident instant
  is the pair.
- **The pinned pass is the interesting one.** `fixture verify` runs every manifest query twice:
  once as written, and once with `observed_at` pinned to the manifest's `clock.end`, comparing
  against `golden/pinned/`. That second pass is US7 scenario 2 — a fact learned after the
  incident instant must be absent from the answer — and since T080 it is **mandatory**: a
  fixture with no `golden/pinned/` fails, because a fixture that does not record what the graph
  knew at T is not evidence that the graph can say what it knew at T.

In manifest terms, the snapshot is a query:

```yaml
queries:
  - name: checkout-2hop
    kind: subgraph
    focus: otel.service.name=checkout
    valid_at: 2026-09-16T09:51:22Z    # the incident instant
    hops: 2
    direction: both
```

and its two recorded answers:

```text
fixtures/baseline-topology-01/golden/subgraph.checkout-2hop.json          # as known now
fixtures/baseline-topology-01/golden/pinned/subgraph.checkout-2hop.json   # as known at clock.end
```

`fixtures/late-arriving-fact-01` is the fixture that exists purely to make the difference
visible: it carries a fact learned after the instant it is about, and the pinned answer is the
one that does not contain it.

## 2. The telemetry window pointers

The graph never holds a metric sample, a log line or a span (constitution IV). What it holds is
**pointers**: selectors that say where to look in the backend that owns the telemetry, in
OpenTelemetry vocabulary, valid over the same bitemporal intervals as everything else.

So "the telemetry window pointers" of a replayable incident is the answer to a `pointers` query
asked at T:

```yaml
  - name: payments-pointers-end
    kind: pointers
    focus: otel.service.name=payments
    valid_at: 2026-09-16T09:51:22Z
```

whose golden is a `PointersResponse` grouped by kind:

```json
{"byKind":{"METRIC":{"pointers":[{
  "backendKind":"prometheus",
  "kind":"METRIC",
  "selector":"service.name=\"payments\" AND service.namespace=\"shop\" AND metric.name=\"http.server.request.duration\"",
  "attributes":{"deployment.environment.name":"prod","service.name":"payments","service.namespace":"shop"},
  "vocabulary":"otel-semconv/1.30"}]}}}
```

Two properties matter for evaluation and both are already gated:

- **The pointers are as-of.** A service renamed after the incident must still be pointed at by
  the name it had at T, or the window an evaluator fetches is the wrong window.
  `baseline-topology-01` ships `payments-pointers-end` and `payments-pointers-mid` at two
  instants precisely so that a pointer that drifts in a graph that did not change is caught.
- **The window is the incident's own.** The "telemetry window" of the triple is
  `[clock.start, clock.end]` of the manifest — the recording's own clock, written by
  `fixture record`/`fixture assemble` from the arrival range of `payloads/index.jsonl`, not
  chosen by hand. An evaluator fetching telemetry resolves each pointer's selector against that
  interval.

The graph therefore carries *where to look* and *over what interval*, and the backend keeps the
data. That is what lets a replayable incident be 4 MB of recording rather than a telemetry dump.

## 3. The validated root cause

A change node, named by the identifier a human would use, in the manifest:

```yaml
ground_truth:
  culprit_change: k8s.change=shop/payments@rev2
  expected_top_k: 3
```

- **`culprit_change`** is the validated root cause. It is `<namespace>=<value>` — the identifier
  the source minted, not an internal entity id — so it survives re-resolution and is readable in
  a diff. `internal/fixture/report.go` matches it against the ranked change's entity id and
  against every alias the change carries, exactly, never fuzzily (constitution VI).
- **`expected_top_k`** is the bar this scenario is held to. SC-005 asks for the true culprit in
  the top three ranked changes; a fixture may state a tighter one, and the default is 3.
- It is called *validated* because a human wrote it down after the fact. Nothing in the pipeline
  derives it, and nothing may: it is the label the ranking is scored against.

The other families label different truths in the same place — `ambiguous-identity` names
`certain_pairs` and `probable_pairs` with, for each, whether the two identifiers really are one
entity — and the harness reads whichever keys its family defines. `ground_truth` is the triple's
third element generalized: "what a person, after the incident, says was true".

## What the agent feature will add

Everything above is the *substrate* half of the triple, and it is complete. What is missing is
the half an agent is scored on, and it is deliberately not built yet (YAGNI, constitution X):

- **The question.** A replayable incident today says what was true and what caused it; it does
  not say what the on-call was asked. The agent feature adds a prompt — the alert, the symptom,
  the instant it fired — as a manifest block beside `ground_truth`.
- **The answer schema.** A ranked list of hypotheses, each with a confidence and an evidence
  chain (constitution V): the event ids, the graph query parameters and the pointer queries that
  produced it. Scoring is then the same comparison the ranking metric already makes — where did
  the true culprit land — plus a calibration curve over the stated confidences, which
  `fixture verify --report` already computes for resolution scores and would compute the same
  way for hypotheses.
- **Telemetry replay.** Resolving a pointer today means calling a live backend. An evaluation
  that must be reproducible next year needs the *answers* to those pointer queries frozen
  beside the fixture — not in the graph (constitution IV forbids that), but as a recorded
  side-car keyed by pointer and window, in the spirit of `payloads/`.
- **Nothing else.** In particular, no new input format. If the agent feature needs a field, it
  is a manifest key, it is written by a recorder or by a human reviewing a diff, and it is
  verified by `fixture verify`.

## How `fixture verify --report` is the eval harness

There is no separate harness and there will not be one. The command that gates every pull
request is the command that scores a model:

```sh
bin/aisre fixture verify fixtures/* --report --report-json report.jsonl --db "$PG_DSN"
./scripts/check-report.sh report.jsonl
```

For each fixture it replays from empty, answers every manifest query at its stated instants and
again with observed time pinned, compares both against the goldens, double-delivers every event,
shuffles within each source's declared reordering window, and then measures:

| metric | what it answers | gate |
|---|---|---|
| `ranking.<query>.rank` / `top_k_hit` | where the validated root cause landed among the ranked changes | culprit inside `expected_top_k` (SC-005) |
| `calibration.certain.precision` | of the merges made with no human in the loop, how many were right | 1.0 (constitution VI) |
| `calibration.probable` | does a stated 0.70 mean 0.70 — a Brier score and a reliability table | reported, not gated (constitution V) |
| `calibration.probable_auto_merges` | did a probable rule merge on its own | 0 (FR-037) |
| `calibration.human_decisions_survive_replay` | is every human decision still recorded, attributable and in force after a rebuild | true (SC-007, FR-040) |

The markdown goes to the job summary and the JSON to `scripts/check-report.sh`, which fails the
build on any of the gates above (`.github/workflows/ci.yml`). Adding an agent to this is adding
rows to that table, not building something new — which is the whole point of deriving evaluation
from the data model rather than bolting it on (constitution VIII).

## Where the incidents come from

- **Hand-authored** fixtures state a scenario in the published event schema. They are small
  enough to read, and they are how an edge case gets a reproduction before anything can record
  one. `late-arriving-fact-01` and `retraction-with-edges-01` are these.
- **Recorded** fixtures come from a live run against a disposable cluster
  (`deploy/kind/README.md`), assembled with `aisre fixture assemble` and recorded with
  `aisre fixture record`. `baseline-topology-01`, `rollout-regression-02`, `config-change-01`
  and `feeder-gap-01` are these, and `.github/workflows/e2e-nightly.yml` produces a fresh one
  every night, including the SC-010 check that the graph built live and the graph rebuilt from
  the recording are byte-identical.

Constitution VIII says synthetic-only data is not enough for a connector to be called stable.
The same applies to an incident: a replayable incident recorded from a real cluster is the unit
this project intends to be judged on.

## See also

- [`specs/001-temporal-graph-core/contracts/fixture-format.md`](../../specs/001-temporal-graph-core/contracts/fixture-format.md) — the fixture directory and the verification steps
- [`fixtures/README.md`](../../fixtures/README.md) — the shipped fixtures and how to re-record one
- [`docs/schema/pointers.md`](../schema/pointers.md) — what a pointer is and what may be in one
- [`docs/schema/ranking.md`](../schema/ranking.md) — how changes are ranked and scored
- [`docs/benchmarks/live-run-2026-09-16.md`](../benchmarks/live-run-2026-09-16.md) — the first live run, in full
