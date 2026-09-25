# Investigation metrics

The numbers the investigation engine is judged on, how they are computed, which of them can fail
a build, and — for every one that can — **the regression it is actually able to detect**.

Two cadences produce them, and they must not be confused:

| cadence | what runs | cost |
|---|---|---|
| **every pull request** (`ci.yml`) | `fixture verify … --trajectory-only`: every recorded trajectory replays byte-identically with **zero network**, and `fixture calibration` publishes the reliability table from those recordings with **zero model calls** | seconds |
| **nightly and on demand** (`eval.yml`) | `aisre eval run`: the corpus × k in **world-replay** mode with the production model configuration, scored against each fixture's ground truth, gated by `scripts/check-report.sh --investigation` | minutes, and money |

`eval run` is a separate command from `fixture verify` on purpose. `fixture verify` is the
per-pull-request gate and has to stay runnable in a job with no Postgres and no egress; the
evaluation needs a database per fixture and runs the corpus k times. Splitting by cadence keeps
each command's promise honest (`contracts/cli.md` §Fixtures and evaluation).

## Reproducing the public leg locally

```sh
export PG_DSN="postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable"
bin/aisre eval run --fixtures fixtures/incidents --runs 3 --model-free \
  --report-json /tmp/eval-rows.jsonl --summary /tmp/eval.md \
  --doc docs/evaluation/investigation-metrics.md
./scripts/check-report.sh --investigation /tmp/eval-rows.jsonl
```

`--model-free` makes **no model call at all** — FR-067's requirement that the engine be fully
operable with no vendor account — and every number such a run publishes is labelled `model-free`
in the rows, in the summary and in the job artifact's name, because a model-free number that
could be mistaken for the production result is the one way this report could mislead somebody
who was not looking for it.

## The row shape

One JSON document per line, which is what `check-report.sh` gates on:

```json
{"metric": "pass_at_1", "scope": "corpus", "fixture": "", "value": 0.3125, "n": 48,
 "detail": "gated: the aggregate over fixtures × runs (FR-060)"}
```

* `scope` is `fixture` (one incident over its k runs) or `corpus` (every fixture × every run).
* `value` is **nullable**, and null is not zero. A corpus with no human labels has no agreement
  figure; publishing `0.00` for it would read as "the engine agreed with nobody". The `detail`
  says why a null is null.
* `n` is what the value rests on — runs, claims, hypotheses, Bernoulli trials. A rate with no `n`
  behind it is not a measurement.
* A per-key breakdown spells the key into the metric name: `worker_calls.metrics/compare`,
  `backend_quota_share.metrics`, `corpus_gaps.latent_bug`,
  `metamorphic_invariance.name-permuted`.

## The gate, and what it can actually detect

**The aggregate pass@1 threshold is set by the first full corpus run and not before** (FR-060).
It lives in [`thresholds.json`](thresholds.json) as `null` until then; while it is null the gate
prints it as `unset` in the job summary and on a machine-readable line, annotates the GitHub job,
and **does not pass as though it had been met**. It is set once, by hand, with:

```sh
./scripts/check-report.sh --investigation --set-threshold /tmp/eval-rows.jsonl
```

which refuses a value above the **coverage ceiling** of the audit it cites (FR-071) and refuses
to overwrite a published threshold without `--force`.

Every other gate is in force from the first run:

| metric | gate | side of the line |
|---|---|---|
| `pass_at_1` | ≥ the published threshold, once set; `unset` until then | **gated**, ceiling-bounded |
| `lift_over_prior` | > 0 — the investigator's MRR minus the deterministic ranker's on the same corpus and the same runs (FR-061a) | **gated**, ceiling-bounded |
| `harm_rate` | ≤ 5 % — the prior put the culprit first and the investigation did not (FR-061a) | **gated** |
| `confidently_wrong` | ≤ 5 % — a non-culprit named at `high` or `very_high` | **gated** |
| `citation_validity` | = 100 % — every cited evidence id resolves to a digest the recording holds (FR-061a) | **gated**, not ceiling-bounded |
| `untraceable_conclusions` | = 0 | **gated** |
| `improvised_replays` | = 0 — a `not_recorded` served as though recorded | **gated** |
| `replay_divergence_rate` | = 0 | **gated**, not ceiling-bounded |
| `metamorphic_verdict_changes` | = 0 (SC-020); `NOT RUN` when no variant was checked, which is not a pass | **gated** |
| `human_label_regressions` | = 0 — a corrected case this run got wrong | **gated** |
| `not_recorded_miss_rate` | > the fixture's threshold ⇒ that **fixture** is excluded and listed | gates a fixture's **admission**, never the engine's score |
| `culprit_rank`, `top_k_hit_rate` | — | reported, never the headline |
| `localisation`, `attribution`, `mechanism` | — | reported, partial credit against `causal_path` |
| calibration (reliability table, Brier, log-loss) | — | reported; published on **every** pull request from recorded trajectories |
| `cost_usd`, `cache_read_ratio`, the three timings, `worker_calls.*`, `backend_quota_share.*` | — | reported and trended |
| `unknown_rate`, `non_unknown_precision` | — | reported (SC-007, SC-023) |
| `corpus_gaps` | — | reported: a category of the audit's unobservable remainder with no fixture (FR-071b) |
| `best_of_k` | — | **NEVER gated, under any circumstances** (FR-060) |

### Detection power

A threshold is only a gate if it says what it can catch. Every gate prints that sentence at the
run's own n, computed by `aisre eval power` from the same arithmetic the report uses
(`internal/eval/metrics.go`), so the script and the report cannot drift:

* **Rate gates** use the **normal approximation to the binomial, one-sided, α = 0.05, power 0.8**:
  n ≥ (z<sub>α</sub>√(p₀(1−p₀)) + z<sub>β</sub>√(p₁(1−p₁)))² ⁄ (p₀−p₁)².
  The direction matters and is stated: `pass_at_1` regresses by **falling**, `harm_rate` by
  **rising**.
  The published worked example comes out of exactly this formula:
  **8 fixtures × 5 runs = 40 Bernoulli trials detects 90 % → 70 % reliably (needs n ≥ 20) and
  cannot detect 90 % → 80 % (needs n ≥ 69).**
* **Zero-tolerance gates** need no significance test — they fire on the first occurrence. What
  they can miss is an *intermittent* defect, and the honest statement is the complement: over n
  trials a defect occurring in a fraction p of runs is caught with 95 % confidence when
  p ≥ 1 − α^(1/n). At n = 48 that is **6.1 %**.

```sh
bin/aisre eval power --n 48 --baseline 0.9              # a min gate
bin/aisre eval power --n 48 --baseline 0.05 --direction max
bin/aisre eval power --n 48 --zero-tolerance
```

## Corpora

The public repository ships synthetic structural twins and runs the full gate on them; the
recorded corpus lives in a private repository and runs the **same job with the same thresholds**
(ADR-0003 D9). `eval.yml` runs both legs; the private leg **skips visibly, with a reason**, when
`PRIVATE_CORPUS_REPO`/`PRIVATE_CORPUS_TOKEN` are not configured, and a release statement names
both results — including the skipped one. Every run records which corpus produced its numbers and
the digest of the model configuration it used (FR-061), in the rows and in the job summary.

## Corpus gaps (FR-071b)

Every category of the audit's unobservable remainder — latent bug, client-side configuration,
business data change, credential leak — owes the corpus at least one fixture whose ground truth is
`unobserved` or `not_change_induced` carrying that category. `internal/investigation/audit`'s
detector is called on **every** evaluation run, and a category with no fixture is named in a row
of its own. A gap is a warning, never a build failure: the corpus being incomplete is a fact about
the corpus, and failing the build over it would only teach people to stop measuring.

## The calibration table, as published on every pull request (T098, T110)

`aisre fixture calibration --fixtures fixtures/incidents --out calibration.json --summary
calibration.md` reads the corpus's **recorded trajectories** and writes the table below. It makes
**zero model calls**, opens no database and opens no socket — it runs in `ci.yml`'s
`trajectory-replay` job inside a network namespace with no interface. That is affordable per pull
request for one reason: every confidence in this system is computed by the ledger from published
constants (FR-023), so a replay reproduces it exactly and there is nothing to sample.

The arithmetic is `internal/fixture/calibration.go`. `eval.yml`'s live-run calibration (T110)
calls the same functions over the corpus × k and adds the paired comparison against the previous
version on the same fixtures; it does not re-derive any of these numbers.

### Format

| bucket | range | n | correct | observed accuracy | mean confidence |
|---|---|---:|---:|---|---:|
| very_low | [0.00, 0.10) | … | … | … or `under-powered (n < 20)` | … |
| low | [0.10, 0.30) | … | … | … | … |
| moderate | [0.30, 0.60) | … | … | … | … |
| high | [0.60, 0.85) | … | … | … | … |
| very_high | [0.85, 1.00] | … | … | … | … |

followed by one line: `**Brier … · log-loss …** over N hypotheses.`

Four rules the format encodes, each of which is a way the table could otherwise mislead.

1. **Every published bucket gets a row, including the empty ones.** A table that omitted its
   empty buckets would hide the finding that the corpus never produces a `very_low` confidence.
2. **A bucket holding fewer than 20 hypotheses is reported as under-powered, never scored**
   (SC-004). An observed accuracy over three hypotheses can only be 0, 0.33, 0.67 or 1.0, and
   printing one of those as a measurement is how a calibration report becomes decoration.
3. **Brier and log-loss are over everything scored**, so a distribution that is well calibrated
   bucket by bucket while being confidently wrong overall still shows up. Log-loss clamps the
   probability at 1e-15, which is stated rather than silent: it is the one place the score is not
   the textbook formula.
4. **A corpus that scores nothing says so.** The table prints `Brier: not scored` and a sentence
   naming the reason, because `0 hypotheses` must never be read as `perfectly calibrated`.

### Where the numbers come from, and what is missing today

A layer-1 trajectory records the **judgments** a run applied — direction, strength, the
likelihood ratio from the published table — and not the posterior they produced. The posterior
travels on the **decision record**, which `investigate export` writes as `investigation.json`. So
the collector reads, per fixture: every file in `trajectories/` (validated, which is why a corpus
whose recordings do not parse cannot score as "nothing to report"), and an `investigation.json`
beside them when there is one.

## Latest evaluation run

The tables below are the **live** run named in their header, and are regenerated only by a live
`eval run --doc`. A model-free run publishes the same rows with model-free numbers and must not
be written here: it would replace a production result with the deterministic engine's and the
label alone would not stop somebody quoting it.

One consequence, stated so it is not read as an omission: **`cache_read_ratio` is not in these
tables yet.** It was implemented after this run, it is `n/a` for a model-free run by construction
— no model call, so no input to cache — and it appears here at the next live run. Its shape is
asserted in `internal/eval`'s own tests
(`TestTheCacheReadRatioIsPublishedAndAModelFreeRunSaysNotApplicable`) rather than by a
regeneration that would cost the live numbers.

<!-- BEGIN GENERATED: eval rows -->

### Investigation eval — public corpus

Generated 2026-09-19T06:50:05Z · fixtures `fixtures/incidents` · audit `docs/evaluation/coverage-audit-2026-09.json` · model configuration 1.1.0 (digest 26aa73eee3a6a8b3ad80a248af46848165ef73f0c06f6975268e6aa81b413e24)

#### Gated (corpus)

| metric | value | n | note |
|---|---:|---:|---|
| `pass_at_1` | 63.6 % | 33 | gated: the aggregate over fixtures × runs (FR-060) |
| `lift_over_prior` | 0.0455 | 33 | gated: must be > 0 — a reasoning layer that does not beat the prior is not worth its cost |
| `harm_rate` | 0.0 % | 33 | 0 of 33 — gated: threshold 5 % |
| `confidently_wrong` | 0.0 % | 33 | 0 of 33 — gated |
| `citation_validity` | 100.0 % | 675 | 675 of 675 — gated: must be 100 % |
| `untraceable_conclusions` | 0 | 675 | gated: must be 0 |
| `replay_divergence_rate` | 0.0 % | 33 | 0 of 33 — gated: must be 0 |
| `improvised_replays` | 0 | 33 | gated: must be 0 |
| `metamorphic_verdict_changes` | 0 | 47 | gated: any variant whose verdict changed fails the build (SC-020) |
| `human_label_regressions` | 0 | 0 | gated: a corrected case this run got wrong |

#### Per fixture

| fixture | pass@1 | pass^k | mean rank | investigator MRR | prior MRR | lift | citations | miss rate | admitted |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| `adjacent-decoy-01` | 0.0 % | 0.0 % | 2.0000 | 0.5000 | 0.2500 | 0.2500 | 100.0 % | 4.5 % | yes |
| `adjacent-decoy-01-culprit-deleted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `adjacent-decoy-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `adjacent-decoy-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `adjacent-decoy-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `declared-incident-01` | 100.0 % | 100.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 3.8 % | yes |
| `declared-incident-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `declared-incident-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `distant-culprit-01` | 0.0 % | 0.0 % | 2.0000 | 0.5000 | 0.2500 | 0.2500 | 100.0 % | 4.5 % | yes |
| `distant-culprit-01-culprit-deleted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `distant-culprit-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `distant-culprit-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `distant-culprit-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `human-fact-reopen-01` | 100.0 % | 100.0 % | 1.0000 | 1.0000 | 1.0000 | 0.0000 | 100.0 % | 4.4 % | yes |
| `human-fact-reopen-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `human-fact-reopen-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `injection-01` | 100.0 % | 100.0 % | 1.0000 | 1.0000 | 1.0000 | 0.0000 | 100.0 % | 1.6 % | yes |
| `injection-01-culprit-deleted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `injection-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `injection-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `injection-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `knowledge-scope-01` | 0.0 % | 0.0 % | 1.0000 | 1.0000 | 1.0000 | 0.0000 | 100.0 % | 13.8 % | **no** |
| `knowledge-scope-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `knowledge-scope-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `merged-alias-01` | 0.0 % | 0.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 0.0 % | yes |
| `merged-alias-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `merged-alias-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `merged-alias-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `rollout-regression-01-incident` | 100.0 % | 100.0 % | 1.0000 | 1.0000 | 1.0000 | 0.0000 | 100.0 % | 4.2 % | yes |
| `rollout-regression-01-incident-culprit-deleted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `rollout-regression-01-incident-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `rollout-regression-01-incident-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `rollout-regression-01-incident-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `slow-burn-01` | 100.0 % | 100.0 % | 1.0000 | 1.0000 | 0.2500 | 0.7500 | 100.0 % | 5.7 % | **no** |
| `slow-burn-01-culprit-deleted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `slow-burn-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `slow-burn-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `slow-burn-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `telemetry-rejection-01` | 0.0 % | 0.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 0.0 % | yes |
| `telemetry-rejection-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `telemetry-rejection-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `two-simultaneous-01` | 0.0 % | 0.0 % | 2.0000 | 0.5000 | 0.5000 | 0.0000 | 100.0 % | 15.4 % | **no** |
| `two-simultaneous-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `two-simultaneous-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unknown-feeder-gap-01` | 0.0 % | 0.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 11.1 % | **no** |
| `unknown-feeder-gap-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unknown-feeder-gap-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-business-data-01` | 100.0 % | 100.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 2.2 % | yes |
| `unobserved-business-data-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-business-data-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-business-data-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-client-config-01` | 100.0 % | 100.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 4.5 % | yes |
| `unobserved-client-config-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-client-config-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-client-config-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-credential-leak-01` | 100.0 % | 100.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 4.3 % | yes |
| `unobserved-credential-leak-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-credential-leak-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-credential-leak-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-latent-bug-01` | 100.0 % | 100.0 % | 0.0000 | 0.0000 | 0.0000 | 0.0000 | 100.0 % | 18.5 % | **no** |
| `unobserved-latent-bug-01-decoy-injected` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-latent-bug-01-name-permuted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |
| `unobserved-latent-bug-01-time-shifted` | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | yes |

#### Reported, not gated (corpus)

| metric | value | n |
|---|---:|---:|
| `pass_hat_k` | 63.6 % | 11 |
| `best_of_k` | 1.0000 | 33 |
| `investigator_mrr` | 0.3636 | 33 |
| `prior_mrr` | 0.3182 | 33 |
| `culprit_rank` | 0.6364 | 33 |
| `top_k_hit_rate` | 45.5 % | 33 |
| `localisation` | 0.9091 | 33 |
| `attribution` | 0.7273 | 33 |
| `mechanism` | 0.3788 | 33 |
| `unknown_rate` | 0.0 % | 33 |
| `non_unknown_precision` | 63.6 % | 33 |
| `human_agreement` | n/a | 0 |
| `cost_usd` | 0.8366 | 33 |
| `worker_calls` | 675 | 14 |

#### Metamorphic invariance (FR-062a, SC-020)

| variant | transform | held | detail |
|---|---|---|---|
| `adjacent-decoy-01-culprit-deleted` | `culprit-deleted` | yes | culprit-deleted of adjacent-decoy-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "unobserved", invariant requires "unobserved" |
| `adjacent-decoy-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of adjacent-decoy-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "k8s.change=shop/payments-config@rv8800", invariant requires "k8s.change=shop/payments-config@rv8800" |
| `adjacent-decoy-01-name-permuted` | `name-permuted` | yes | name-permuted of adjacent-decoy-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "k8s.change=shop/ledger@rv8800", invariant requires "k8s.change=shop/ledger@rv8800" |
| `adjacent-decoy-01-time-shifted` | `time-shifted` | yes | time-shifted of adjacent-decoy-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "k8s.change=shop/payments-config@rv8800", invariant requires "k8s.change=shop/payments-config@rv8800" |
| `declared-incident-01-name-permuted` | `name-permuted` | yes | name-permuted of declared-incident-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `declared-incident-01-time-shifted` | `time-shifted` | yes | time-shifted of declared-incident-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `distant-culprit-01-culprit-deleted` | `culprit-deleted` | yes | culprit-deleted of distant-culprit-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "unobserved", invariant requires "unobserved" |
| `distant-culprit-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of distant-culprit-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "k8s.change=shop/payments-config@rv8800", invariant requires "k8s.change=shop/payments-config@rv8800" |
| `distant-culprit-01-name-permuted` | `name-permuted` | yes | name-permuted of distant-culprit-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "k8s.change=shop/inventory@rv8800", invariant requires "k8s.change=shop/inventory@rv8800" |
| `distant-culprit-01-time-shifted` | `time-shifted` | yes | time-shifted of distant-culprit-01: parent answered "k8s.change=shop/payments-config@rv8800", variant answered "k8s.change=shop/payments-config@rv8800", invariant requires "k8s.change=shop/payments-config@rv8800" |
| `human-fact-reopen-01-name-permuted` | `name-permuted` | yes | name-permuted of human-fact-reopen-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/inventory@rev7", invariant requires "k8s.change=shop/inventory@rev7" |
| `human-fact-reopen-01-time-shifted` | `time-shifted` | yes | time-shifted of human-fact-reopen-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments@rev7", invariant requires "k8s.change=shop/payments@rev7" |
| `injection-01-culprit-deleted` | `culprit-deleted` | yes | culprit-deleted of injection-01: parent answered "k8s.change=shop/payments@rev7", variant answered "unobserved", invariant requires "unobserved" |
| `injection-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of injection-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments@rev7", invariant requires "k8s.change=shop/payments@rev7" |
| `injection-01-name-permuted` | `name-permuted` | yes | name-permuted of injection-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/checkout@rev7", invariant requires "k8s.change=shop/checkout@rev7" |
| `injection-01-time-shifted` | `time-shifted` | yes | time-shifted of injection-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments@rev7", invariant requires "k8s.change=shop/payments@rev7" |
| `knowledge-scope-01-name-permuted` | `name-permuted` | yes | name-permuted of knowledge-scope-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments-rollouts.md@rev7", invariant requires "k8s.change=shop/payments-rollouts.md@rev7" |
| `knowledge-scope-01-time-shifted` | `time-shifted` | yes | time-shifted of knowledge-scope-01: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments@rev7", invariant requires "k8s.change=shop/payments@rev7" |
| `merged-alias-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of merged-alias-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `merged-alias-01-name-permuted` | `name-permuted` | yes | name-permuted of merged-alias-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `merged-alias-01-time-shifted` | `time-shifted` | yes | time-shifted of merged-alias-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `rollout-regression-01-incident-culprit-deleted` | `culprit-deleted` | yes | culprit-deleted of rollout-regression-01-incident: parent answered "k8s.change=shop/payments@rev7", variant answered "unobserved", invariant requires "unobserved" |
| `rollout-regression-01-incident-decoy-injected` | `decoy-injected` | yes | decoy-injected of rollout-regression-01-incident: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments@rev7", invariant requires "k8s.change=shop/payments@rev7" |
| `rollout-regression-01-incident-name-permuted` | `name-permuted` | yes | name-permuted of rollout-regression-01-incident: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/storefront@rev7", invariant requires "k8s.change=shop/storefront@rev7" |
| `rollout-regression-01-incident-time-shifted` | `time-shifted` | yes | time-shifted of rollout-regression-01-incident: parent answered "k8s.change=shop/payments@rev7", variant answered "k8s.change=shop/payments@rev7", invariant requires "k8s.change=shop/payments@rev7" |
| `slow-burn-01-culprit-deleted` | `culprit-deleted` | yes | culprit-deleted of slow-burn-01: parent answered "k8s.change=shop/payments-pool@rv5512", variant answered "unobserved", invariant requires "unobserved" |
| `slow-burn-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of slow-burn-01: parent answered "k8s.change=shop/payments-pool@rv5512", variant answered "k8s.change=shop/payments-pool@rv5512", invariant requires "k8s.change=shop/payments-pool@rv5512" |
| `slow-burn-01-name-permuted` | `name-permuted` | yes | name-permuted of slow-burn-01: parent answered "k8s.change=shop/payments-pool@rv5512", variant answered "k8s.change=shop/checkout@rv5512", invariant requires "k8s.change=shop/checkout@rv5512" |
| `slow-burn-01-time-shifted` | `time-shifted` | yes | time-shifted of slow-burn-01: parent answered "k8s.change=shop/payments-pool@rv5512", variant answered "k8s.change=shop/payments-pool@rv5512", invariant requires "k8s.change=shop/payments-pool@rv5512" |
| `telemetry-rejection-01-name-permuted` | `name-permuted` | yes | name-permuted of telemetry-rejection-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `telemetry-rejection-01-time-shifted` | `time-shifted` | yes | time-shifted of telemetry-rejection-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `two-simultaneous-01-name-permuted` | `name-permuted` | yes | name-permuted of two-simultaneous-01: parent answered "k8s.change=shop/payments-config@rv7781", variant answered "k8s.change=shop/inventory@rv7781", invariant requires "k8s.change=shop/inventory@rv7781" |
| `two-simultaneous-01-time-shifted` | `time-shifted` | yes | time-shifted of two-simultaneous-01: parent answered "k8s.change=shop/payments-config@rv7781", variant answered "k8s.change=shop/payments-config@rv7781", invariant requires "k8s.change=shop/payments-config@rv7781" |
| `unknown-feeder-gap-01-name-permuted` | `name-permuted` | yes | name-permuted of unknown-feeder-gap-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `unknown-feeder-gap-01-time-shifted` | `time-shifted` | yes | time-shifted of unknown-feeder-gap-01: parent answered "unobserved", variant answered "unobserved", invariant requires "unobserved" |
| `unobserved-business-data-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of unobserved-business-data-01: parent answered "not_change_induced: business_data_change", variant answered "not_change_induced: business_data_change", invariant requires "not_change_induced: business_data_change" |
| `unobserved-business-data-01-name-permuted` | `name-permuted` | yes | name-permuted of unobserved-business-data-01: parent answered "not_change_induced: business_data_change", variant answered "not_change_induced: business_data_change", invariant requires "not_change_induced: business_data_change" |
| `unobserved-business-data-01-time-shifted` | `time-shifted` | yes | time-shifted of unobserved-business-data-01: parent answered "not_change_induced: business_data_change", variant answered "not_change_induced: business_data_change", invariant requires "not_change_induced: business_data_change" |
| `unobserved-client-config-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of unobserved-client-config-01: parent answered "not_change_induced: client_side_configuration", variant answered "not_change_induced: client_side_configuration", invariant requires "not_change_induced: client_side_configuration" |
| `unobserved-client-config-01-name-permuted` | `name-permuted` | yes | name-permuted of unobserved-client-config-01: parent answered "not_change_induced: client_side_configuration", variant answered "not_change_induced: client_side_configuration", invariant requires "not_change_induced: client_side_configuration" |
| `unobserved-client-config-01-time-shifted` | `time-shifted` | yes | time-shifted of unobserved-client-config-01: parent answered "not_change_induced: client_side_configuration", variant answered "not_change_induced: client_side_configuration", invariant requires "not_change_induced: client_side_configuration" |
| `unobserved-credential-leak-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of unobserved-credential-leak-01: parent answered "not_change_induced: credential_leak", variant answered "not_change_induced: credential_leak", invariant requires "not_change_induced: credential_leak" |
| `unobserved-credential-leak-01-name-permuted` | `name-permuted` | yes | name-permuted of unobserved-credential-leak-01: parent answered "not_change_induced: credential_leak", variant answered "not_change_induced: credential_leak", invariant requires "not_change_induced: credential_leak" |
| `unobserved-credential-leak-01-time-shifted` | `time-shifted` | yes | time-shifted of unobserved-credential-leak-01: parent answered "not_change_induced: credential_leak", variant answered "not_change_induced: credential_leak", invariant requires "not_change_induced: credential_leak" |
| `unobserved-latent-bug-01-decoy-injected` | `decoy-injected` | yes | decoy-injected of unobserved-latent-bug-01: parent answered "not_change_induced: latent_bug", variant answered "not_change_induced: latent_bug", invariant requires "not_change_induced: latent_bug" |
| `unobserved-latent-bug-01-name-permuted` | `name-permuted` | yes | name-permuted of unobserved-latent-bug-01: parent answered "not_change_induced: latent_bug", variant answered "not_change_induced: latent_bug", invariant requires "not_change_induced: latent_bug" |
| `unobserved-latent-bug-01-time-shifted` | `time-shifted` | yes | time-shifted of unobserved-latent-bug-01: parent answered "not_change_induced: latent_bug", variant answered "not_change_induced: latent_bug", invariant requires "not_change_induced: latent_bug" |

#### Corpus gaps (FR-071b)

every unobservable-remainder category of audit 2026-09 has at least one fixture under fixtures/incidents (16 fixtures scanned, 4 carrying remainder ground truth)

| category | covered | detail |
|---|---|---|
| `client_side_configuration` | yes | covered by unobserved-client-config-01 |
| `business_data_change` | yes | covered by unobserved-business-data-01 |
| `credential_leak` | yes | covered by unobserved-credential-leak-01 |

#### Detection power

n = 33 Bernoulli trials detects a drop from 64 % to 42.6 % or below; a regression to anything above 42.6 % is invisible at this n (normal approximation to the binomial, one-sided, α = 0.05, power = 0.8)

For reference at this n: 33 Bernoulli trials cannot detect 64 % → 70 % (needs n ≥ 343) and cannot detect 64 % → 80 % (needs n ≥ 48).

<!-- END GENERATED: eval rows -->
