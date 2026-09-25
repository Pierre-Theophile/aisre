# Quickstart: the Investigation Engine

Every scenario below runs with **no vendor account, no vendor credentials and no network access
to any telemetry backend** (FR-067, SC-011). That is the point of the feature: the engine is
complete and evaluable before feature 003 exists.

> Every command here was run end to end on a clean machine with no vendor account on 2026-09-18
> (T118); what each one printed, and every drift that run fixed, is recorded in
> [quickstart-run-2026-09-18.md](quickstart-run-2026-09-18.md). Where a step needs a model, that
> record says what it does instead without one.

## Prerequisites

```sh
# 001's prerequisites, unchanged
docker compose -f deploy/docker-compose.yml up -d     # Postgres
make build                                            # bin/aisre
bin/aisre migrate --db "$PG_DSN"                  # now also creates schema `investigation`

# the one thing this feature adds: the credential of every provider config/model.yaml names
export MISTRAL_API_KEY=...        # production runs Mistral, serving GLM as the investigator
# export ANTHROPIC_API_KEY=...    # only for --model-config config/model.anthropic.yaml
# --listen must be loopback with --auth dev: the dev key is published, so the server refuses to
# offer dev identities to a network. --recording-root is the directory holding the `world/` the
# telemetry workers answer from; point it at the incident fixture the scenario uses.
bin/aisre serve --db "$PG_DSN" --listen 127.0.0.1:8080 --auth dev --dev \
  --enable-investigation \
  --model-config config/model.yaml \
  --budget-profiles config/budgets.yaml \
  --recording-root fixtures/incidents/rollout-regression-01-incident &
export SRE_AGENT_TOKEN=$(bin/aisre dev-token --dev --user alice --roles reader,decider,investigator)
```

`config/model.yaml` pins the production configuration and is recorded on every investigation:

```yaml
investigator: {provider: mistral, model: zai-glm-5-3,           effort: high}
verifier:     {provider: mistral, model: mistral-medium-latest}   # a different model — FR-022a
logs_label:   {provider: mistral, model: ministral-8b-latest}     # off until its contribution is measured
price_table:  "2026-09-18"
```

`config/model.anthropic.yaml` is the same three roles on the Anthropic models, kept valid and
priced, so switching providers back is a flag: pass it to `--model-config` and set
`$ANTHROPIC_API_KEY` instead. A command that cannot resolve the credential its configuration
names runs **model-free** and logs which variable was missing.

**One database per incident fixture.** Two incident fixtures share entity identifiers, so loading
a second one into a database that already holds another is refused by the observed-interval
constraint. Give each scenario its own database (`createdb`, then `migrate`), which is what
`fixture verify` does for itself.

> **Trajectory replay and the evaluation harness need no API key at all** — replay reissues
> nothing. Only scenarios 1, 5, 6 and 10 below call the model.

---

## 1 — Investigate a recorded incident

```sh
bin/aisre fixture load fixtures/incidents/rollout-regression-01-incident --db "$PG_DSN"

# the recorded world is chosen once, at the server: `serve --recording-root <fixture dir>` above.
bin/aisre investigate otel.service.name=checkout \
  --at 2026-09-01T14:32:00Z --lookback 90m --profile page \
  --watch
```

**Run §8 first.** Until a coverage audit is published, π₀ = 1: *no observed change explains this*
enters at confidence 1.0 and every candidate at 0, and the engine says so at startup (FR-069a).
The ranking below is the ranking of a system that knows its own ceiling.

**With no vendor credential** the run is model-free (the engine logs which variable was missing):
steps 1 and 2 below happen, the first wave supports the culprit in the evidence chain, and the
verdict line is `unknown` with stop reason `COMPLETED` rather than the named rollback candidate.
The rest of this section describes the run *with* a model.

**Expect, in this order:**

1. Within ~2 s, the **provisional** prior-only ranking, explicitly labelled provisional and
   untested — the graph's own deterministic answer, which costs milliseconds (FR-046a, SC-013).
2. The **first wave** landing with no model in the loop: for each top candidate, error rate and
   latency before/after onset, new log patterns, error spans on the path edges, and errors split
   by version tag.
3. The **verdict line**, naming the decisive fact and the rollback candidate.
4. The **ranked list**, with each hypothesis's evidence for *and against* — exonerations rendered
   as prominently as supports — including the mandatory *no observed change explains this*
   hypothesis with its own confidence.
5. The **timeline** of candidate changes and the estimated symptom onset.
6. The **narrative**, last, containing no claim absent from the above.

Check the parts that matter:

```sh
bin/aisre investigate get <id> --output json | jq '.ledger.hypotheses[0]'
#   → culprit `k8s.change=shop/payments@rev7`, status "SUPPORTED", a computed confidence with its
#     bucket range, and supportingEvidenceIds that are not empty

bin/aisre investigate get <id> --output json \
  | jq '.ledger.hypotheses[] | select(.kind=="NO_OBSERVED_CHANGE") | .confidence'
#   → present and scored, always (FR-019a)

bin/aisre investigate get <id> --output json | jq '.ledger.evidence[].coverage' | head
#   → every evidence item carries a coverage block; a missing one is a rejected write
```

**The autoscaler check** (US2 scenario 6, FR-029c/d): the `storefront` scaling change started
after the estimated onset, so it is typed `CANDIDATE_EFFECT`, `EXONERATED`, cites the onset
estimate's evidence id, and is reported as controller-originated — not as an equal suspect.

---

## 1a — Investigate an incident a human declared

No monitor fired. Someone opened a dated severity channel — which, in the audited organisation,
is the trigger that fired on *every* incident while monitor transitions fired on few.

```sh
bin/aisre investigate declare \
  --severity sev2 --at 2026-09-01T14:30:00Z \
  --title "2026-09-01 checkout errors" \
  --origin slack:C0123456789 \
  --service otel.service.name=checkout --lookback 90m
```

(with `serve --recording-root fixtures/incidents/declared-incident-01`)

**Expect**: exactly one investigation; its **observed instant equal to the declaration instant**
(not to the instant the engine saw the declaration, and not to the instant it began); an intake
record carrying actor kind `human`, the declaring identity, the severity, the title and the origin
reference; and each target reference carrying its provenance — `parsed_from_declaration` naming
the rule id, or `supplied_by_human` — never guessed.

```sh
# re-deliver the identical declaration: nothing changes (FR-008b)
bin/aisre investigate declare --severity sev2 --at 2026-09-01T14:30:00Z \
  --title "2026-09-01 checkout errors" --origin slack:C0123456789 --service otel.service.name=checkout
bin/aisre investigate list --incident <incident-id> | wc -l    # → 1

# a monitor fires minutes later on a neighbouring service: it ATTACHES (FR-008c)
bin/aisre investigate --alert fixtures/incidents/declared-incident-01/late-monitor.json
bin/aisre investigate get <id> --output json \
  | jq '{symptoms: [.symptoms[] | {transport, actorKind, firedAt}], grouping: .symptoms[1].groupingEvidenceId}'
#   → two symptoms, one investigation, and the grouping decision recorded as evidence with the
#     time and neighbourhood distances it rested on
```

**When no service can be parsed and none was supplied**, the outcome is `unknown` with "name the
affected service(s)" as the resolving action, nothing is guessed, and a later human fact naming
them reopens the investigation and starts from those services:

```sh
bin/aisre investigate declare --severity sev2 --at … --title "2026-09-01 everything is slow" --origin slack:C09…
#   → outcome UNKNOWN, resolution kind = confirm_identity
bin/aisre investigate fact <id> --kind known_state --statement "it is checkout" --entity otel.service.name=checkout
#   → reopened, linked, and proceeding from checkout
```

**The report goes back to where the incident lives** — report-only, edited in place, never a
stream of re-posts, and never a precondition of concluding:

```sh
bin/aisre investigate report <id>            # render only
bin/aisre investigate report <id> --deliver  # update in place
bin/aisre investigate get <id> --output json | jq '.deliveries[0] | {outcome, updateCount}'
```

A delivery that fails is recorded and the investigation still reaches a terminal state.

---

## 2 — Follow an evidence item back to its query

```sh
bin/aisre investigate get <id> --chain | head -40

# the exact term the evidence item issued, lifted out of the machine rendering rather than
# retyped: `pointer` is an object and `windows` carries the two ranges and the reference instant.
ARGS=$(bin/aisre investigate get <id> --output json \
  | jq -c '.ledger.evidence[] | select(.evidenceId=="e-18") | .term.compare')
bin/aisre worker call metrics compare --args "$ARGS" \
  --mode recorded --recording fixtures/incidents/rollout-regression-01-incident
```

**Expect**: the digest printed by hand is byte-identical to the one in the evidence chain
(SC-025, FR-057d). Every evidence item in both renderings carries a deep link, or says why it has
none.

---

## 3 — Replay, in both layers, with the network off

```sh
bin/aisre investigate export <id> --out /tmp/inv-export

# layer 1: byte-identical, model outputs included — the plumbing gate
scripts/netns.sh bin/aisre investigate replay --from /tmp/inv-export --layer trajectory
#   → "identical: true"; exit 0. Any unmatched request fails loudly naming worker, capability and
#     the exact parameters, and exits 4.

# layer 2: a different reasoning path, served from the recorded world. The export carries a
# world only when the run recorded one beside itself (`<recording-root>/<investigation-id>/world`);
# a run answered from a fixture's own world exports layer 1 alone, and the corpus's world replay
# is `fixture verify` below.
scripts/netns.sh bin/aisre investigate replay --from /tmp/inv-export --layer world \
  --report-json /tmp/world.json
jq '{notRecordedCount, missRate}' /tmp/world.json
#   → misses are typed `not_recorded`, counted, and never improvised; missRate ≤ 0.05
```

Two replays that differ report the difference **as a difference in the reasoning layer**, naming
the first diverging worker request — never as a difference in the data (FR-041).

`investigate replay --from <dir>` runs **in the process you typed it into**: a recording is a
directory, and re-issuing it through the same seams opens no socket, needs no server and needs no
database. `--remote` sends the same request to a server instead, for an artifact that lives beside
one. `scripts/netns.sh` above is not decoration — it is the same thing `ci.yml`'s `trajectory-replay`
job does around every command, so that "zero network" is a property of the sandbox rather than a
claim about the code. It is **Linux-only** (`unshare`): on macOS run the same commands without it
and rely on CI for the sandbox property.

The corpus's own recordings replay the same way, without an export:

```sh
# every incident fixture with a trajectories/ directory, no Postgres, no model
scripts/netns.sh bin/aisre fixture verify --trajectory-only fixtures/incidents/*/

# the reliability table over the five confidence buckets, zero model calls
scripts/netns.sh bin/aisre fixture calibration --fixtures fixtures/incidents \
  --out /tmp/calibration.json --summary /tmp/calibration.md
```

And a fixture's recording is made — once, and reproducibly — with:

```sh
bin/aisre fixture record-trajectory fixtures/incidents/rollout-regression-01-incident
#   → trajectories/run-<digest>.jsonl, written twice: the model-free run and a fake-model run.
#     Re-running it on an unchanged fixture reports "unchanged" and writes the same bytes.
```

---

## 4 — The answers that are not "no"

```sh
# its own database (see Prerequisites), and `serve --recording-root` pointed at this fixture
bin/aisre fixture load fixtures/incidents/unknown-feeder-gap-01 --db "$PG_DSN"
bin/aisre investigate otel.service.name=payments --at … --lookback 2h --output json \
  | jq '.ledger.evidence[].outcome' | sort | uniq -c
```

**Expect** the published outcome vocabulary — `DIGEST` (an answer), and the five that are not one:
`NO_DATA`, `NOT_YET_INGESTED`, `QUERY_FAILED`, `PARTIAL`, `NOT_RECORDED` — to be distinct values,
rendered differently. Only `NO_DATA` is evidence that nothing happened. A model-free run reaches
`DIGEST`, `PARTIAL` and `NOT_RECORDED`; the others need the terms a model-driven run issues.

The outcome is `UNKNOWN`, the feeder gap is named as a coverage limitation, every affected
hypothesis carries reduced confidence *stating the gap as the reason*, and the resolutions name
at least one concrete thing that would change the answer, each with its next query as a deep
link.

---

## 5 — Bound the spend and watch it stop cleanly

```sh
bin/aisre investigate … --profile page --output json | jq '.spend'

# an operator's own cap is a budget profile, not a flag: copy the published file, change the
# number, and start the server with it — `serve --budget-profiles /tmp/tight.yaml`.
sed 's/"\*": 40/"*": 4/' config/budgets.yaml > /tmp/tight.yaml
bin/aisre investigate … --profile page \
  --output json | jq '{outcome, stopReason, untested: [.ledger.hypotheses[]|select(.status=="UNTESTED")|{statement, nextQueryDeepLink}]}'
```

This scenario needs a model: model-free there is no model turn to refuse, the run completes and
the spend block reports the profile with nothing spent against it.

**Expect** the second run to terminate as `BUDGET_EXHAUSTED` with a typed stop reason naming the
budget, a **written** synthesis produced from the 15% reserve (not a sentence cut in half), the
hypotheses it left untested each with the exact next query as a deep link, and the confidence of
every cut-short hypothesis **widened** rather than frozen. Spend is reported against every
dimension, including tokens by model and class and the quota share observed per backend.

---

## 6 — Push a fact; the investigation reopens

```sh
bin/aisre investigate fact <id> \
  --kind manual_action \
  --statement "the load balancer was drained by hand at 14:05" \
  --entity k8s.service=shop/edge-lb --from 2026-09-01T14:05:00Z
```

**Expect**: a new, linked investigation in state `reopened`; the original readable exactly as
produced; the fact in the evidence chain with its author and time, weighted `strong` and never
`decisive`; and where telemetry contradicts it, **both kept** with the contradiction stated in
the affected hypothesis. At no point does any investigation enter a state in which it is waiting
for a human (SC-024).

---

## 7 — Review it, and turn it into a corpus incident

```sh
bin/aisre investigate review <id> \
  --root-cause k8s.change=shop/payments@rev7 --reason "confirmed in the postmortem"
bin/aisre investigate label <id> --right
bin/aisre investigate to-incident <id> --out fixtures/incidents/from-review-01
bin/aisre fixture verify fixtures/incidents/from-review-01 --report --db "$PG_DSN"
```

**Expect** the harness to accept it on the first run with no hand-editing (SC-012), and the review
to survive a full replay of the event log, still attributable and still in force (SC-015):

> **Known gap (2026-09-18).** `to-incident` writes the export half — `events.jsonl`,
> `export.json`, `investigation.json`, `trajectories/` — and not yet the `manifest.yaml` with the
> `incident:` block, so `fixture verify` answers "no fixtures" on its output. See the run record.

```sh
bin/aisre fixture verify fixtures/incidents/from-review-01 --db "$PG_DSN" \
  --report --report-json /tmp/verify.jsonl
jq '.metrics.calibration.human_decisions_survive_replay' /tmp/verify.jsonl   # → true
```

---

## 8 — Measure the ceiling before trusting any number

```sh
bin/aisre audit coverage --incidents incidents.yaml --out docs/evaluation/

# the shape, and something to run today: the synthetic audit the corpus ships
bin/aisre audit coverage --incidents fixtures/audits/synthetic-01/incidents.yaml \
  --out /tmp/audit --db "$PG_DSN"
```

`incidents.yaml` is human-supplied — incident, alert instant, known cause, category — and the
audit **measures; it never infers a cause**. Expect per-incident classifications, the aggregate
ceiling with the incident count it rests on, the missing causes counted by category, and the
feeder set it was measured against. The ceiling is the upper bound on every published accuracy
target (FR-071) and is the prior of *no observed change explains this*.

---

## 9 — The graph must be no dirtier for any of this

```sh
# <investigation-entity-id> is the `investigation`-type node the run recorded (record_investigation),
# not the investigation id; nothing at the CLI maps one to the other yet.
bin/aisre query history 'id:<investigation-entity-id>' --output json | jq '.versions[0].props'
#   → every key under `sre.investigation.*`: hypotheses, model_config, outcome, recording_digest,
#     recording_key, requester, spend, stop_reason, verdict_line. No samples.

bin/aisre fixture verify fixtures/incidents/telemetry-rejection-01 --output json \
  | jq '.steps[] | select(.name=="expect-rejected")'
#   → passed: "1 events refused with the stated reason code, graph unchanged" — the fixture
#     proves that a decision record carrying a series is REJECTED (SC-010)
```

---

## 10 — Run the evaluation

This is `eval.yml`'s **public leg**, reproduced locally, model-free. It needs Postgres and
nothing else: no vendor account, no key, no network (FR-067).

```sh
bin/aisre eval run --fixtures fixtures/incidents --runs 3 --model-free \
  --db "$PG_DSN" \
  --report-json /tmp/eval-rows.jsonl \
  --summary /tmp/eval.md \
  --doc docs/evaluation/investigation-metrics.md
./scripts/check-report.sh --investigation /tmp/eval-rows.jsonl
```

`eval run` is a different command from `fixture verify` on purpose: `fixture verify
--trajectory-only` is the **per-pull-request** gate and must stay runnable with no database and
no egress, while this is the world-replay evaluation that runs the corpus k times. Pass
`--live` (with the credential each configured provider needs) to run the production model
configuration; with
`--model-free` **every number is labelled `model-free`** in the rows, the summary and the
document, because it is not the production result.

**Expect** one JSONL row per metric per scope — `{"metric": …, "scope": "fixture"|"corpus",
"fixture": …, "value": …, "n": …, "detail": …}` — publishing, per fixture and over the corpus:
pass@1 and pass^k; lift over the prior (the investigator's MRR minus the deterministic ranker's,
whose rank per fixture comes from `ground_truth.prior_rank_of_culprit`); the harm rate and the
confidently-wrong rate; citation validity (a citation whose answer is `not_recorded` or
`query_failed` has no digest to match and is not counted as invalid — how much `not_recorded` a
fixture produces is the miss rate's business; a horizon-truncated answer is checked structurally,
because the recorded backend re-digests it after stamping the horizon in); the culprit's rank and
top-k (reported, not the headline); localisation, attribution and mechanism scored separately with
partial credit against `causal_path`; the `not_recorded` miss rate; onset error and its
within-tolerance flag; the time to the provisional ranking, the first tested hypothesis and
conclusion, and for each of those three the share of runs inside its published deadline (5 s,
120 s, 5 min; constants in `internal/eval/report.go`) plus the count of runs past the ten-minute
hard stop — SC-013 is stated as shares, and the latencies are real wall time for a model-free run
too, kept in `Trajectory.Elapsed` beside the recording rather than inside it; calls per capability
and backend quota share; cost; the `unknown` rate and the precision of non-`unknown` outcomes; the
replay-divergence rate; agreement with human corrections where labels exist (`n/a` otherwise);
the metamorphic invariance rows; and the **corpus-gap line** naming every category of the audit's
unobservable remainder with no fixture. `--doc` regenerates the tables in
`docs/evaluation/investigation-metrics.md` between its generated markers and leaves every line of
its prose alone.

**Expect** `check-report.sh --investigation` to print the gate table, then the detection power of
each gate at this run's own n, then the reported-never-gated metrics, then the excluded fixtures
and the corpus gaps. It fails the build on the gated metrics only — lift ≤ 0, harm rate above
5 %, confidently wrong above 5 %, citation validity below 100 %, any untraceable conclusion, any
improvised replay or replay divergence, any metamorphic verdict change, any regression on a
human-labelled case — and on aggregate pass@1 **once a threshold has been published**. Until
then:

```text
  aggregate pass@1               unset        UNSET      measured 0.5625 over n=48; the threshold
                                                         is set by the first full corpus run (FR-060)
```

which is neither a pass nor a failure. (0.5625 is the model-free figure measured on 2026-09-18
over the 16 incident fixtures at `--runs 3`; the published threshold is set from a run of the
production configuration, not from this one.)

The `lift over the prior` gate is a **corpus** gate. Model-free, the investigator *is* the
deterministic ranker, so on a single fixture whose prior already ranks the culprit first the lift
is 0 and the gate fails; over the corpus it is positive (0.078 on that same run) because the
ordering and the first wave beat the prior where the prior is wrong. After the first full corpus run, publish it once, by hand:

```sh
./scripts/check-report.sh --investigation --set-threshold /tmp/eval-rows.jsonl
```

It refuses a value above the coverage ceiling of the audit it cites (FR-071) and refuses to
overwrite a published threshold without `--force`; it writes `docs/evaluation/thresholds.json`,
which is then committed and reviewed like any other change.

**Expect** each gate to be printed with the regression it can actually detect, from
`aisre eval power` — normal approximation to the binomial, one-sided, α = 0.05, power 0.8 for
a rate gate, and `1 − α^(1/n)` for a zero-tolerance one:

```sh
bin/aisre eval power --n 40 --baseline 0.9
# 40 Bernoulli trials detects 90 % → 70 % reliably (needs n ≥ 20) and
# cannot detect 90 % → 80 % (needs n ≥ 69)
```

### Multiply and stress the corpus

The variants are **generated, never checked in**, so they go outside `fixtures/` and the
evaluation is pointed at them with `--variants`. Without that flag the invariance gate prints
`NOT RUN` rather than passing, because a gate that held over nothing did not hold.

```sh
mkdir -p /tmp/variants
for dir in fixtures/incidents/*/; do
  id=$(basename "$dir")
  for t in culprit-deleted decoy-injected time-shifted name-permuted; do
    bin/aisre fixture derive "$dir" --transform "$t" \
      --out "/tmp/variants/$id-$t" --db "$PG_DSN" --force ||
      echo "$id $t: not applicable to this fixture"
  done
done
bin/aisre eval run --fixtures fixtures/incidents --runs 3 --model-free \
  --variants /tmp/variants --db "$PG_DSN" --report-json /tmp/eval-rows.jsonl
./scripts/check-report.sh --investigation /tmp/eval-rows.jsonl
```

`fixture derive` refuses a transform that has no meaning for a fixture — a culprit-deleted variant
of an incident whose ground truth is already `unobserved` — and the loop reports each one rather
than swallowing it.

**Expect** the time-shifted, name-permuted and far-decoy variants to produce the *same verdict* as
the original, and the culprit-deleted variant to produce `unobserved` with the symptoms still
localised — a run that names a decoy fails, naming the variant (SC-020).

On the three adversarial fixtures (`slow-burn-01`, `distant-culprit-01`, `adjacent-decoy-01`) the
deterministic ranker does *not* put the culprit first, and the report prints the investigator's
rank, the prior's rank, and the lift between them per fixture (SC-021).

---

## 11 — Conformance checks that are easy to forget

| check | command | expected |
|---|---|---|
| a retrieved document contains an instruction | `fixture verify fixtures/incidents/injection-01 --db "$PG_DSN"` | scope, budgets, worker set, posture and output unchanged; the attempt recorded as an evidence item |
| a worker is asked something outside the algebra | `worker call metrics raw_query --args '…'` | exit 1, `outside_algebra`, naming what is available; the refusal recorded |
| the engine is started with a write-scoped credential | `investigate …` with a feeder token | exit 3, "your token does not carry the role this call needs" |
| an anonymous token | `investigate …` | exit 3 |
| the prefix is actually cached | `eval run --report-json rows.jsonl` then `jq 'select(.metric == "cache_read_ratio" and .scope == "corpus")' rows.jsonl` | the ratio of cached to total input tokens, from the recorded per-call `Usage`. Reported and **never gated**. A model-free run answers `n/a` — it made no model call, so there was no input to cache — and only a run with a credential measures it; a collapse between two releases means a silent invalidator crept into the prompt prefix. The published tables in `docs/evaluation/investigation-metrics.md` carry it from the next live run |
| the ledger is order-independent | `go test ./internal/investigation/ledger -run Property` | passes over permuted judgment orders |
