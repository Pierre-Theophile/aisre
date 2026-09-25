<!-- SPDX-License-Identifier: Apache-2.0 -->

# The `page` profile, measured end to end — 2026-09-18

This is [T117](../../specs/002-investigation-engine/tasks.md): the six numbers SC-005 and SC-013
state as targets, measured on the whole incident corpus with the production model configuration,
plus the one structural claim behind them — that the **first wave, not the model, is what carries
the 120-second target**.

**Every target is met.**

| target (SC-005, SC-013) | measured | over | verdict |
|---|---|---|---|
| provisional ranking within **5 s** in ≥ 95 % of runs | **100 %** | 48 live runs | met |
| first **tested** hypothesis within **120 s** in ≥ 90 % | **100 %** | 48 live runs | met |
| substantive answer within **5 min** in ≥ 90 % | **97.9 %** (47 of 48) | 48 live runs | met |
| nothing past the **10-minute** hard stop | **0 runs** | 48 live runs | met |
| ledger recomputation, 50 hypotheses × 500 judgments, **< 10 ms** | **436 µs** (best of five) | in-process | met, 23× under |
| trajectory replay **< 5 s** per fixture | **0.110 – 0.237 s** | 16 fixtures, 48 trajectories | met, 21× under |

The single run outside the 5-minute line is one of `two-simultaneous-01`'s three — the fixture
whose ground truth is *two changes at the same instant on the same target*, where the model has
nothing structural to separate with and keeps looking. The other two runs of that fixture
concluded in well under the deadline.

## How it was measured

```sh
export PG_DSN="postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable"
bin/aisre eval run --fixtures fixtures/incidents --runs 3 --live \
  --corpus public --variants .variants \
  --report-json /tmp/eval-rows.jsonl --summary /tmp/eval.md \
  --doc docs/evaluation/investigation-metrics.md
./scripts/check-report.sh --investigation /tmp/eval-rows.jsonl
```

The three latencies are rows of that report — `time_to_provisional_seconds`,
`time_to_first_tested_seconds`, `time_to_conclusion_seconds`, and the
`*_within_target` shares beside them — so this page is a *reading* of the evaluation and not a
second measurement of it. The deadlines the shares are taken against are constants in
`internal/eval/report.go` (`TargetTimeToProvisional`, `TargetTimeToFirstTested`,
`TargetTimeToConclusion`, `HardStop`) for the same reason.

The other two numbers come from where they are already asserted:

```sh
go test -run TestPosteriorsAreComputedWellInsideTheBudget -v ./internal/investigation/ledger/
bin/aisre fixture verify --trajectory-only fixtures/incidents/<id>/    # timed per fixture
```

> **The ledger row is an uninstrumented figure, and the assertion knows it** (added 2026-09-21).
> The command above carries no `-race`, which is what makes 436 µs comparable from one run to the
> next. CI runs `go test -race` for everything else, and the race detector instruments every memory
> access — about **4.6×** for this computation, inside Go's documented 2–20× range — so under it the
> 23× margin reported here falls to roughly 5×, and a contended runner spends the rest. That is not
> hypothetical: the assertion failed on GitHub Actions at **12.1 ms** while the same commit measured
> 1.9–2.2 ms locally.
>
> So `TestPosteriorsAreComputedWellInsideTheBudget` scales its bound by 5× when the detector is on,
> and `ci.yml` runs that one test a second time **without** `-race` so the 10 ms figure on this page
> stays asserted by something. The uninstrumented bound is unchanged; what changed is that the
> instrumented run no longer pretends to measure it.

### What each instant means, exactly

| row | the instant it marks |
|---|---|
| `time_to_provisional_seconds` | the run's first record to its **first worker call**. The provisional ranking is π₀ over the diff and is published before any call goes out, so this is the instant the investigation had a ranking *and started testing it*. |
| `time_to_first_tested_seconds` | the run's first record to its **first ledger update** — the first judgment applied to a hypothesis, which is what "tested" means (FR-031). |
| `time_to_conclusion_seconds` | the run's first record to its **stop record**. |

**These are wall-clock offsets kept beside the recording, not the instants inside it.** A fixture
run pins the investigation's clock to the alert instant so that a re-record is byte-identical,
which means every recorded `at` in a run is the same instant and every latency computed from them
is zero — this is why these three rows read `n/a` before this measurement. `Trajectory.Elapsed`
is the second quantity: how long after the run's first record each record was made, on a real
clock, held outside the recording so that measuring a run cannot change its digest.

## The claim behind the 120-second target

**The first wave carries it, and the model cannot lose it.** The same corpus, model-free — no
model call at all, FR-067's no-vendor-account leg:

| | live (GLM via Mistral) | model-free |
|---|---:|---:|
| runs | 48 | 48 |
| mean time to first tested hypothesis | 1.544 s | **0.017 s** |
| slowest fixture's mean | 7.537 s (`merged-alias-01`) | 0.025 s (`injection-01`) |
| share within 120 s | 100 % | 100 % |
| mean time to conclusion | 17.008 s | 0.020 s |

Model-free, the first tested hypothesis lands in **17 milliseconds** — about 7 000× inside the
target. The first wave is deterministic, it is planned from π₀ before any model turn, and it
applies its own judgments; the model is not in that path.

The live column says the same thing from the other side. Twelve of the sixteen fixtures reach
their first tested hypothesis in about **30 ms** live as well, because the first wave got there
before the first model turn returned. The four that take 3.5–7.5 s — `merged-alias-01`,
`telemetry-rejection-01`, `unknown-feeder-gap-01`, `declared-incident-01` — are the fixtures where
the first wave has no candidate to test at all, so the first ledger update *is* the model's; those
are also the four whose first tested hypothesis and conclusion are the same instant. Even there
the margin to 120 s is more than fifteenfold.

So a model that got slower, or a provider that queued, would move the conclusion time and would
not move the 120-second line until it was two orders of magnitude worse than measured.

## Run provenance

| | |
|---|---|
| corpus | `fixtures/incidents`, 16 incident fixtures, 3 runs each; 47 derived metamorphic variants at 1 run each |
| model configuration | `config/model.yaml` 1.1.0, digest `26aa73eee3a6a8b3ad80a248af46848165ef73f0c06f6975268e6aa81b413e24` |
| roles | investigator `zai-glm-5-3`, verifier `mistral-medium-latest`, logs labeller `ministral-8b-latest`, all via Mistral |
| price table | `config/prices.yaml`, version 2026-09-18 |
| worlds | recorded with `fixture record-world --live-passes 3` (six on the fixtures that needed it), provenance `synthetic` |
| spend | **$0.837** over the 33 scored runs, as the `cost_usd` row reports it; about $2.4 for the whole evaluation including the excluded fixtures and the 47 variants |
| gate | `scripts/check-report.sh --investigation` exits 0; every gate passes |
| measured | 2026-09-18 into 2026-09-19 |

### Environment

| | |
|---|---|
| CPU | Apple M5, 10 logical cores |
| Memory | 16 GiB |
| OS / arch | macOS 27.0 / darwin arm64 |
| Go | go1.27.1 |
| PostgreSQL | 16 (`postgres:16-alpine`, Docker compose, same host) |

## Caveats, and they matter

1. **The telemetry is generated.** Every world under `fixtures/incidents` carries
   `provenance: synthetic`, so these are latencies of the engine, the ledger and the provider —
   not of a vendor telemetry API under load. A backend that takes a second per call moves the
   conclusion time directly and moves nothing else on this page.
2. **The latencies are over the whole corpus; the corpus aggregate is over the admitted 11.** The
   `*_within_target` shares here are recomputed from the per-fixture rows so that every fixture is
   counted, including the five excluded on their `not_recorded` miss rate. The gate's own
   corpus-scope rows cover the 33 admitted runs, where the same three shares are also 100 %.
3. **The prefix cache was cold for most of this run.** The trailing-system-message fix landed with
   it (`contracts/prompting.md` §Providers), so the conclusion times here are close to an upper
   bound for this configuration rather than a steady-state figure.
4. **No pass@1 threshold is published by this run.** It measured 63.6 % over n = 33, which is what
   `scripts/check-report.sh --investigation --set-threshold` would publish; that is the owner's
   decision and FR-071 caps it at the audit's 0.615 ceiling in any case.
