# Quickstart run — a clean machine, no vendor account (T118)

**Date** 2026-09-18 · **Task** T118 · **Feature** 002
**Tree** `git worktree add /tmp/live/qs-clean HEAD` at `b555ceb`, a detached checkout with no
`bin/`, no `.envrc` and nothing built.
**Environment** `MISTRAL_API_KEY` and `ANTHROPIC_API_KEY` unset for every command below (the
shell that ran them never had them); fresh database `qs_clean_20260918` on the local Postgres;
`PATH=$HOME/go/bin:/opt/homebrew/bin:$PATH`; macOS 27 (darwin/arm64).

Every command is recorded with its outcome. Where the output differed from what the quickstart
claims, the line says whether the **doc** drifted or the **code** did, and what was changed.

## Prerequisites

| # | command | outcome |
|---|---------|---------|
| P1 | `docker compose -f deploy/docker-compose.yml up -d` | already running (`sre-agent-postgres`); reused |
| P2 | `make build` | ok, 5.3 s, `bin/aisre` at `b555ceb` |
| P3 | `bin/aisre migrate --db "$PG_DSN"` | ok — 8 migrations applied to the fresh database, including `0006_investigation.sql` |
| P4 | `bin/aisre dev-token --user alice --roles reader,decider,investigator` | **FAILED**: `dev-token needs --dev`. **Doc drift** — `internal/cli/dev_token.go` has documented `--dev` since 001 and the server refuses dev auth without it. Quickstart fixed to `dev-token --dev …`. |
| P5 | `bin/aisre serve --db … --auth dev --dev --enable-investigation …` | **FAILED**: `--auth dev on ":8080": dev tokens are signed with a published key … Listen on loopback`. **Doc drift** — the Prerequisites block omits `--listen 127.0.0.1:8080`. Quickstart fixed. |
| P5' | same with `--listen 127.0.0.1:8080` and `--recording-root /tmp/live/qs-recordings` | ok. `/healthz` → 200. Logs the three warnings the feature promises: no recording root world, `$MISTRAL_API_KEY` unset so the engine runs **model-free** rather than failing (FR-067), and π₀ = 1 until a coverage audit is published. |

Note on `--recording-root /var/lib/sre-agent/recordings`: an FHS path that does not exist on a
Mac and is not created by the server. Left as the example it is, with the quickstart now saying
so.

## §1 — Investigate a recorded incident

| # | command | outcome |
|---|---------|---------|
| 1.1 | `fixture load fixtures/incidents/rollout-regression-01 --db "$PG_DSN"` | **FAILED**: no such manifest. **Doc drift** — the corpus directory is `fixtures/incidents/rollout-regression-01-incident` (`fixtures/rollout-regression-01` is the 001 graph fixture). Quickstart fixed here and in §2. |
| 1.2 | `fixture load fixtures/incidents/rollout-regression-01-incident --db "$PG_DSN"` | ok — 95 events applied, 0 rejected |
| 1.3 | `investigate otel.service.name=checkout --at … --recording fixtures/…` | **FAILED**: `unknown flag: --recording`. **Doc drift** — `investigate` has no such flag and contracts/cli.md never gave it one; the recorded world is chosen once, at the server, with `serve --recording-root <dir>`. Quickstart fixed (§1, §1a, §4). |
| 1.4 | same without `--recording`, server started with `--recording-root fixtures/incidents/rollout-regression-01-incident` | ok, exit 0. The anytime shape arrives as promised: the provisional prior-only ranking labelled provisional, then the concluded run. Model-free, so no model turns: outcome `UNKNOWN`, stop reason `COMPLETED`, detail "no model is configured; the investigation is the prior-only ranking, the causal ordering and the deterministic first wave". |
| 1.5 | `investigate get <id> --output json \| jq '.ledger.hypotheses[0]'` | **FAILED**: `.ledger` was `null`. **Code drift** — `InvestigationDAO.Get` deliberately does not load the ledger, and nothing else did either, so the read path served a verdict line with nothing under it and `investigate get --ledger/--chain/--evidence` printed empty sections. Fixed in code: `server.LedgerReader` + `server.WithLedgerReader`, wired in `internal/cli/serve.go`; `Get` attaches the ledger, `List` deliberately does not. Test `TestGetReturnsTheLedger`. |
| 1.6 | the three §1 `jq` checks, after the fix | ok — 5 hypotheses, 32 evidence items, every evidence item carries a `coverage` block, and `NO_OBSERVED_CHANGE` is present and scored. With no coverage audit published π₀ = 1, so it ranks first at confidence 1.0 and the change hypotheses at 0 — the engine warns about exactly this at startup (FR-069a). §8 must be run before §1's numbers mean anything; the quickstart now says so. |

**Observation (not fixed, not doc drift).** The first §1 run was piped into `head -50`, which closed
the stream; the server-side persist then failed with `context canceled` and the investigation was
recorded `FAILED`. Because the same question is idempotent (FR-008b), every later run of §1
returned that same terminal row — the fixture could only be investigated again after the database
was dropped. `Runner.failed` already detaches its context for exactly this reason; `Runner.persist`
does not. Worth a task for the engine track.

## §1a — A human declaration

| # | command | outcome |
|---|---------|---------|
| 1a.1 | `investigate declare --severity sev2 --at 2026-09-01T14:30:00Z --title … --origin slack:C0123456789 --service otel.service.name=checkout --lookback 90m` | ok, exit 0 — the declaration **attached to the open incident** as an additional symptom (FR-008c), and the timeline shows both symptoms in order. |

## §2 — Follow an evidence item back to its query

| # | command | outcome |
|---|---------|---------|
| 2.1 | `investigate get <id> --chain` | ok after the §1.5 fix — 138 lines, each judgment naming its evidence and each evidence item either a deep link or why it has none ("the answer came from a recorded world, which has no console to link to"). |
| 2.2 | `worker call metrics compare --args … --mode recorded --recording fixtures/incidents/rollout-regression-01-incident` | ok — response digest `f9970556…` **byte-identical** to evidence `e-18` in the chain (SC-025, FR-057d). **Doc drift**: the quickstart's `--args` used `{"pointer":"…","windows":{"reference_at":…,"width_seconds":…}}`; the published spelling is `pointer` as an object and `windows` with `baseline`/`symptom` ranges. Quickstart fixed, and it now shows how to lift the exact term out of the chain with `jq`. |

## §3 — Replay, both layers

| # | command | outcome |
|---|---------|---------|
| 3.1 | `investigate export <id> --out /tmp/live/inv-export` | ok — digest `2931f207cfd0`, 71 trajectory records, **0 world terms** |
| 3.2 | `investigate replay --from … --layer trajectory` | ok — `identical=true, not_recorded=0, miss rate 0.0000`, exit 0. Run **without** `scripts/netns.sh`: it is Linux-only (`unshare`), and the zero-network property is enforced in CI's `trajectory-replay` job, which wraps the same command. |
| 3.3 | `investigate replay --from … --layer world` | **FAILED**: `has no recorded world to replay`. Cause: the engine answers telemetry from `<recording-root>/world`, while the exporter looks for a world *beside the run* at `<recording-root>/<investigation-id>/world` (`Runner.WorldDir`). A run served by a fixture's world therefore exports no layer 2. Recorded as a finding; the quickstart now says the world layer is present when the run recorded its own world, and points at `fixture verify` for world replay over the corpus. |
| 3.4 | `fixture verify --trajectory-only <dir>` for each of the 16 incident fixtures | **16/16 ok**, no database, no model |
| 3.5 | `fixture calibration --fixtures fixtures/incidents --out … --summary …` | ok — 33 recorded trajectories over 16 fixtures, zero model calls; Brier 0.102844, log-loss 0.600693 over 109 hypotheses; buckets under n=20 reported as under-powered rather than scored (SC-004) |

## §4 — The four answers that are not "no"

| # | command | outcome |
|---|---------|---------|
| 4.1 | `fixture load fixtures/incidents/unknown-feeder-gap-01 --db "$PG_DSN"` into the database §1 had used | **FAILED**: `close_observed: closed_at … is not after the observed lower bound …`. Two incident fixtures share entity identifiers, so the second replay contradicts the first's observed intervals. **Doc drift** — a corpus incident is loaded into a database of its own (`fixture verify` does exactly that internally). The quickstart now says so and creates one per scenario. |
| 4.2 | the same into a fresh `qs_gap` database | ok — loaded, rejection contract held |
| 4.3 | `investigate otel.service.name=payments --at … --lookback 2h --output json`, then the outcome histogram | ok — outcome `UNKNOWN` as claimed. The evidence outcomes present model-free are `DIGEST` ×9, `NOT_RECORDED` ×1, `PARTIAL` ×1. The other typed outcomes (`NO_DATA`, `NOT_YET_INGESTED`, `QUERY_FAILED`) are produced by terms only a model-driven run reaches. **Doc drift**: the section is called "the four answers" but the published vocabulary has **five** — ADR-0005 renamed `ok` to `digest` and added `partial`. Quickstart fixed to name all five and to say which of them a model-free run reaches. |

## §5 — Bound the spend

| # | command | outcome |
|---|---------|---------|
| 5.1 | `investigate … --output json \| jq '.spend'` | ok, but model-free the spend block is `{"limits":{"name":"page"}}`: no tokens, no calls priced, because nothing was spent. |
| 5.2 | `investigate … --cap calls_per_backend=4` | **FAILED**: `unknown flag: --cap`. **Doc drift** — there is no `--cap` on `investigate` and contracts/cli.md never defined one; an operator changes a cap by copying `config/budgets.yaml`, editing the number and passing it to `serve --budget-profiles <file>`. Quickstart fixed. |
| 5.3 | the same through a tight profile (`calls_per_backend "*": 2`) | the run still concluded `COMPLETED`: model-free there is no model turn to refuse, so `BUDGET_EXHAUSTED` is not reachable without a vendor account. The quickstart already says §5 calls the model; it now says explicitly what §5 does *not* show on a clean machine. |

## §6 — Push a fact; the investigation reopens

| # | command | outcome |
|---|---------|---------|
| 6.1 | `investigate fact <id> --kind manual_action --statement … --entity k8s.service=shop/edge-lb --from …` | **FAILED the expectation**: the fact was recorded and the command printed "a reopen runs a new linked investigation from this fact" — but nothing reopened, and no CLI command exists to reopen. **Code drift**: FR-057b ("a human fact … MUST move it to *reopened* and produce a new linked record"), contracts/cli.md (`fact` "reopens a concluded investigation as a linked record") and the command's own `--help` all said otherwise. Fixed at the CLI: `fact` reads the lifecycle first and calls `Reopen` for a concluded investigation (which records the same fact, so it is not written twice) and `SubmitHumanFact` otherwise. Test `TestFactAgainstAConcludedInvestigationReopensIt`. |
| 6.2 | the same after the fix | ok — "reopened as inv-6b6c2ddadf91dceb". Parent `REOPENED` and readable, child `CONCLUDED` with `reopensInvestigationId` pointing at the parent, the fact recorded against the parent with its author and weight class `strong` (FR-057a). |

## §7 — Review it, and turn it into a corpus incident

| # | command | outcome |
|---|---------|---------|
| 7.1 | `investigate review <id> --root-cause … --reason …` | ok — "review recorded … root cause k8s.change=shop/payments@rev7, 0 amendments" |
| 7.2 | `investigate label <id> --right` | ok |
| 7.3 | `investigate to-incident <id> --out …` | ok, exit 0 — wrote `events.jsonl`, `export.json`, `investigation.json`, `trajectories/` |
| 7.4 | `fixture verify <that dir> --report --db "$PG_DSN"` | **FAILED**: `no fixtures in [...]`. The directory has no `manifest.yaml`, so the harness does not recognise it as an incident. **Code gap, not fixed here**: `to-incident` writes the export half and not the incident manifest FR-055/SC-012 requires. The manifest format is the corpus track's surface and it is being re-recorded concurrently; fixing it blind would collide. Recorded as a finding and flagged in the quickstart. |
| 7.5 | `… --report-json - \| jq '.calibration.human_decisions_survive_replay'` | **Doc drift** — `--report-json` takes a *file*, and the key is at `.metrics.calibration.human_decisions_survive_replay`. Quickstart fixed. |

## §8 — Measure the ceiling

| # | command | outcome |
|---|---------|---------|
| 8.1 | `audit coverage --incidents fixtures/audits/synthetic-01/incidents.yaml --out … --db "$PG_DSN"` | ok — per-incident classifications, the ceiling with its incident count, the per-category counts and the unobservable remainder with the categories that owe the corpus a fixture. **Doc drift**: the quickstart's bare `incidents.yaml` names no file that exists; it now points at the synthetic audit in the corpus and says the real list is human-supplied. |

## §9 — The graph must be no dirtier

| # | command | outcome |
|---|---------|---------|
| 9.1 | `query history 'id:<investigation-entity-id>' \| jq '.versions[0].props'` | ok once the entity id is known: every property is under `sre.investigation.*` — hypotheses, model_config, outcome, recording_digest, recording_key, requester, spend, stop_reason, verdict_line — and there is no sample, series or span among them. **Gap**: nothing at the CLI maps an investigation id to its graph entity id (`query subgraph` does not follow `INVESTIGATED`), so the id had to be read out of the database by hand. Recorded; the quickstart now says what the identifier is. |
| 9.2 | `grep -c '"reasonCode":"telemetry_payload"' <(fixture verify fixtures/incidents/telemetry-rejection-01 --output json)` | **FAILED**: 0 matches. The JSON report carries the step and its detail ("1 events refused with the stated reason code, graph unchanged"), not the reason code as a field. **Doc drift** — quickstart fixed to `jq '.steps[] \| select(.name=="expect-rejected")'`, which is what actually proves SC-010. The property itself holds: the step passes. |

## §10 — Run the evaluation, model-free

| # | command | outcome |
|---|---------|---------|
| 10.1 | `eval run --fixtures fixtures/incidents/<one> --runs 1 --model-free --db … --report-json … --summary …` | ok, 0.6 s, 86 rows |
| 10.2 | `eval run --fixtures fixtures/incidents --runs 3 --model-free …` (the corpus) | ok, 4.9 s, 636 rows over 16 fixtures |
| 10.3 | `scripts/check-report.sh --investigation <rows>` on the corpus run | **exit 0** — every gate passes: lift over the prior 0.078125 (n=48), harm rate 0, confidently wrong 0, citation validity 1.0 (n=663), untraceable conclusions 0, improvised replays 0, replay divergence 0; aggregate pass@1 **0.5625 over n=48**, UNSET and therefore not gating. The metamorphic gate prints `NOT RUN` with no variants, exactly as documented. **Doc drift**: the quickstart quotes "measured 0.3125 over n=48" — a stale number. Updated to the measured 0.5625 and marked as the model-free figure. |
| 10.4 | the same on a **single** fixture | the `lift over the prior` gate **FAILS** (0 over n=1): model-free the investigator *is* the deterministic ranker on a fixture whose prior already ranks the culprit first. Not drift — but the quickstart now says the lift gate is a corpus-level gate and why a one-fixture model-free run fails it. |
| 10.5 | `fixture derive <dir> --transform {culprit-deleted,decoy-injected,time-shifted,name-permuted} --out /tmp/live/variants/… --db … --force` for two fixtures | 8/8 ok |
| 10.6 | `eval run … --variants /tmp/live/variants …` then `check-report.sh` | ok — `metamorphic verdict changes 0 PASS over n=8`; exit 0 |
| 10.7 | `eval power --n 40 --baseline 0.9` | ok — prints exactly the two sentences the quickstart quotes |
| — | `--doc docs/evaluation/investigation-metrics.md` | **not run**: that document belongs to the corpus track, which is regenerating it in parallel. |

## §11 — Conformance checks

| check | outcome |
|---|---|
| a retrieved document contains an instruction (`fixture verify fixtures/incidents/injection-01`) | **ok** — every step passes, including trajectory replay of both runs with zero network |
| a worker asked outside the algebra (`worker call metrics raw_query`) | **ok** — exit 1, `outside_algebra`, naming the whole published algebra |
| a write-scoped credential (feeder token) | **ok** — exit 3, "your token does not carry the role this call needs … lacks role \"investigator\"" |
| an anonymous token | **ok** — exit 3, naming how to mint one |
| the prefix is actually cached (`jq '.model.cache_read_ratio'`) | **FAILED**: there is no `model` block and `cache_read_ratio` appears **nowhere in the code** — only in the quickstart. The claim is unimplemented; recorded as a finding and the row now says so rather than pretending. |
| the ledger is order-independent (`go test ./internal/investigation/ledger -run Property`) | **ok** |

## Summary

* 23 commands as written; **11 drifts**: 9 fixed in the quickstart, 2 fixed in code
  (`investigate get` returned no ledger; `investigate fact` did not reopen a concluded
  investigation), 3 recorded as findings for other tracks (`to-incident` writes no manifest,
  `investigate export` cannot carry the world layer of a run served from a fixture,
  `cache_read_ratio` is documented and unimplemented).
* No vendor account, no `MISTRAL_API_KEY`, no `ANTHROPIC_API_KEY`, no egress: every scenario that
  does not require a model ran to completion, and the ones that do (§1's verdict, §4's full
  outcome set, §5's budget exhaustion) degrade exactly as FR-067 promises — the engine logs which
  variable was missing and runs model-free.
* `scripts/netns.sh` was not used: it is Linux-only. The zero-network property it enforces is
  enforced in CI's `trajectory-replay` job around the same commands.
