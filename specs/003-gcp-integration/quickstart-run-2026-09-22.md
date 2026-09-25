<!-- SPDX-License-Identifier: Apache-2.0 -->

# Quickstart run — no GCP account, no mailbox, no network (T179)

**Date** 2026-09-22 · **Task** T179 · **Feature** 003
**Tree** `/home/user/sre-agent` at `cc7d59f`, on `claude/fervent-tesla-ncnm1v`.
**Environment** linux/amd64; no `GOOGLE_APPLICATION_CREDENTIALS`, no `gcloud` configuration, no
mailbox credential, and no network reachable from the commands below. PostgreSQL 16 from the
repository's own embedded distribution, run as an unprivileged user on port 5433. Fresh databases
`qs179` and `qs179b`.

Every command is recorded with its outcome. Where the output differed from what the quickstart
claims, the line says whether the **doc** drifted or the **code** did, and what was changed. The
quickstart in this directory has been corrected; this file is the evidence for each correction.

**Headline: §1–§6 and §11 reproduce. §7, §8 and §9 need a real GCP credential and §10 needs the
private incident corpus, so they were not run — and nine documented commands did not exist in the
shape the quickstart gave them.** Not one of those nine was a code defect: every one was the doc
naming a flag the CLI does not have, or a flag where the CLI takes a positional argument. A
quickstart nobody has run is a quickstart that does not work, which is the case this task exists to
find.

## §0 — Prerequisites

| # | command | outcome |
|---|---------|---------|
| P1 | `make build` | ok — `bin/aisre`, 94 MB, `-X main.version=cc7d59f` |
| P2 | `bin/aisre migrate up --db "$PG_DSN"` | **FAILED**: `unknown command "up" for "aisre migrate"`. **Doc drift** — `migrate` has no subcommand; it applies every embedded migration the database has not seen. Quickstart fixed to `bin/aisre migrate`. |
| P2' | `bin/aisre migrate --db "$PG_DSN"` | ok — 10 migrations applied, 0 already present |
| P3 | `bin/aisre fixture load fixtures/gcp-cross-source-merge-01 --db "$PG_DSN"` | ok — 42 applied, 0 duplicate, 0 rejected; "the rejection contract held" |
| P4 | `bin/aisre serve --db "$PG_DSN" --auth dev --dev --listen 127.0.0.1:8099` | ok. Prints the four-line dev-authentication warning block, then `listening`. |
| P5 | `bin/aisre dev-token --dev --user qs --roles reader,decider` | ok |

`--listen 127.0.0.1:…` is required rather than optional: `--auth dev` on a non-loopback address is
refused unless `--dev-insecure-listen` is passed, because the dev signing key is published. Feature
002's run record already noted this and §0 of this quickstart still omitted it. Now stated.

## §1 — Replay the synthetic twins

| # | command | outcome |
|---|---------|---------|
| 1.1 | `fixture verify --filter 'gcp-*,vendor-*' --report` | **FAILED**: `fixture verify` takes `<dir>...` and has no `--filter`. **Doc drift** — the flag has never existed; CI passes `fixtures/*/`. Quickstart fixed here and in six other places. |
| 1.2 | `fixture verify fixtures/gcp-*/ fixtures/vendor-*/ --report` | ok — **25 fixtures, all passed** |

The four step names the table promised were also wrong. The verifier prints `replay`,
**`double-delivery`**, `shuffle` and **`expect-rejected`**; the quickstart said `idempotency` and
`refusal`. **Doc drift**, fixed — a reader grepping the output for the documented word would find
nothing and conclude the step had not run.

Verbatim, from `fixtures/gcp-baseline-topology-01`:

```
| step | result | detail |
|---|---|---|
| replay | ok | 27 events applied from empty; 6 goldens match |
| double-delivery | ok | 27 events re-delivered, all DUPLICATE_NOOP; 8 entity and 9 edge versions unchanged |
| shuffle | ok | 6 seeded permutations produced the same valid-time state |
| expect-rejected | ok | fixture declares no events that must be rejected |
```

## §2 — The announced fact

This is the scenario the feature exists to prove, and it reproduces.

| # | command | outcome |
|---|---------|---------|
| 2.1 | `fixture verify fixtures/vendor-maintenance-future-01/` | ok |
| 2.2 | `query subgraph --focus 'vendor=…' --valid-at …` | **FAILED**: the ref is **positional** and the flag is `--as-of`, not `--valid-at`. **Doc drift**, fixed in three places. |
| 2.3 | `query subgraph 'vendor=twin-gpu' --as-of 2026-10-02T03:00:00Z --observed-at 2026-09-17T12:00:00Z --hops 2` | ok — 2 nodes, 1 edge: the vendor and `maintenance announced for twin-gpu inference-api, 2026-10-02T02:00:00Z to 2026-10-02T04:00:00Z`, joined by `changed_by` |
| 2.4 | the same with `--observed-at 2026-09-16T00:00:00Z` | ok — `vendor=twin-gpu (nothing valid at this instant)`, **0 nodes, 0 edges** |
| 2.5 | `query diff --focus … --t1 … --t2 … --reference-at …` | **FAILED**: positional ref, and the flags are `--from`, `--to` and `--reference`. **Doc drift**, fixed. |
| 2.6 | `query history --ref … --both-dimensions` | **FAILED**: positional ref, and **no such flag** — `history` always answers in both dimensions. **Doc drift**, fixed, with a line saying why there is no flag. |
| 2.7 | `query history 'gcp.cloudrun.service=…'` | ok — 6 versions, 2 resolution decisions, with `superseded` and `current` observed intervals side by side |

2.3 and 2.4 are the same valid-time question at two observed instants, and they give opposite
answers. That is the whole claim, run.

**One thing the quickstart says that no command in it demonstrates.** §2(c) promises that a future
change "is marked `post_reference` — excluded **for that stated reason**, not by scoring low". The
exclusion happens: over every window tried, `query diff` on the vendor reports `0 ranked change(s)`
and *"no change events in this window touched the neighbourhood"*. But the announced change is never
a **candidate** in this fixture, so the `post_reference` flag is never the reason a reader sees — the
reason they see is the weaker "touched no neighbourhood". The flag and its `kappa*exp(-|dt|/tau_post)`
weighting are published in the ranking explanation the same command prints, so the mechanism is
there; nothing in §2 exercises it. Left as an open item rather than changed: which of the two is the
intended operator-facing reason is a design question, not a typo.

## §3 — One announcement, two sources, one change

| # | command | outcome |
|---|---------|---------|
| 3.1 | `fixture verify fixtures/vendor-duplicate-two-sources-01/` | ok |
| 3.2 | `resolve why --a … --b …` | **FAILED**: `resolve why` takes **two positional refs**. **Doc drift**, fixed. |
| 3.3 | `resolve why 'otel.service.name=storefront-web' 'gcp.cloudrun.service=twin-production/europe-west1/storefront'` | ok, and worth quoting — see below |

Run against `gcp-cross-source-merge-01` rather than the vendor fixture, because that is where this
PR's new cross-source merge is:

```
otel.service.name=storefront-web and gcp.cloudrun.service=twin-production/europe-west1/storefront
are the SAME entity, yopycd2mkm7g4jbfopxjzp7l4l

DECIDED               KIND        BY       SCORE  RATIONALE
2026-09-21T14:30:00Z  auto_merge  rule C1  1.00   sources gcp:twin and otel:twin both claim the identifier otel.service.name=storefront; …
2026-09-21T14:30:00Z  auto_merge  rule C4  1.00   otel:twin observes service otel.service.name=storefront-web carrying
                                                  sre.gcp.revision_name=storefront-00042-bbb in project twin-production region
                                                  europe-west1, and gcp:twin knows that revision as
                                                  gcp.cloudrun.revision=twin-production/europe-west1/storefront/storefront-00042-bbb,
                                                  which belongs to gcp.cloudrun.service=twin-production/europe-west1/storefront; …
```

The revision is listed as a **separate entity** (`2i275t2j…`), which is this PR's C4 fix visible from
the outside: the observed service merged with the *service*, and the revision is cited as the
evidence rather than absorbed.

## §4 — The two rollouts, and the rollback

| # | command | outcome |
|---|---------|---------|
| 4.1 | `fixture verify --filter 'gcp-rollout-traffic-shift-01,…'` | **FAILED**: as 1.1. **Doc drift**, fixed. |
| 4.2 | `fixture verify fixtures/gcp-rollout-traffic-shift-01/ fixtures/gcp-revision-zero-traffic-01/ fixtures/gcp-rollback-01/` | ok — 3 passed |
| 4.3 | `query subgraph --focus … --valid-at <mid-rollout>` | **FAILED**: as 2.2. **Doc drift**, fixed. |
| 4.4 | `query diff 'gcp.cloudrun.service=twin-production/europe-west1/storefront' --from … --to … --reference …` | ok — 1 ranked change, `ROLLOUT`, *"revision storefront-00042-bbb created for storefront (no production traffic moved)"*, with the temporal / topological / traffic components printed separately |

4.4 is FR-016's claim in one line of output: the creation change says, in its own summary, that it
moved no traffic.

## §5 — Execute the algebra against a recorded world

| # | command | outcome |
|---|---------|---------|
| 5.1 | `backend list` | ran, but **not what the quickstart promises**: `no telemetry backend registered` on a fresh server. The output itself names the fix — `--gcp <org-slug>` prints the declaration with no credential. **Doc drift**, fixed. |
| 5.2 | `backend list --gcp twin` | ok — `gcp:twin (vendor gcp, v0.1.0, algebra 1.0.0)`, the **eight** terms and nothing else, each with exactly one cost class and `read only: true` |
| 5.3 | `worker backend --recording …` | **FAILED**: there is no `worker backend`. The subcommands are `worker call`, `worker list` and `worker record`; a term against a recorded world is `worker call <worker> <term> --mode recorded --recording <dir>`. **Doc drift**, fixed. |
| 5.4 | `fixture verify fixtures/gcp-telemetry-rejection-01/` | ok — the telemetry-carrying event is refused with its published reason code |

Verbatim from 5.2, since §5's expectation is a count:

```
  term                 cost class window cap   read only
  compare              standard   168h0m0s     true
  drill_down           cheap      2160h0m0s    true
  error_spans          standard   168h0m0s     true
  errors_by_version    standard   168h0m0s     true
  exemplars            expensive  6h0m0s       true
  monitor_state        cheap      2160h0m0s    true
  new_log_patterns     expensive  6h0m0s       true
  onset                expensive  24h0m0s      true
```

## §6 — Read the report rows

| # | command | outcome |
|---|---------|---------|
| 6.1 | `fixture verify fixtures/gcp-*/ fixtures/vendor-*/ --report` | ok — as 1.2, with metrics |
| 6.2 | `scripts/check-report.sh <report>` | ok — every assertion held; `cross-source merges: 2 certain auto-merge(s) across 1 labelled fixture(s)` |

## §7, §8, §9 — not run

`feed gcp --dry-run`, the campaign commands and the parity check all need a real GCP credential, and
§8 additionally needs the corpus HMAC key and a **named human signatory** (FR-140). Not something
this run could supply, and not something it should fake.

One thing found by reading rather than running, and it belongs here: `fixture campaign` exists and
its help says **"Not implemented yet. See specs/003-gcp-integration/spec.md FR-129–FR-132."** So §8's
three commands would fail even with a credential. That is US7's work (T153–T160), it is listed as
blocked in tasks.md, and §8 now says so instead of reading as though it were ready.

## §10 — not run

Needs the thirteen-incident corpus, whose incident-level detail is kept in a private corpus and is
not in this repository (`docs/evaluation/coverage-audit-2026-09.md`). T171, T172 and T173 are blocked
on the same thing.

Two command shapes were corrected by reading the CLI, so that whoever *does* have the corpus starts
from something that parses:

| # | quickstart said | the CLI takes |
|---|---|---|
| 10.1 | `audit coverage --corpus F --feeders a,b --out FILE.md` | `--input` (or `--incidents`), `--feeder-set` (or `--graph-config`), and `--out` is a **directory** |
| 10.2 | `investigate --replay DIR --explain-ranking` | `investigate replay --from DIR`, then `investigate get <id> --chain`; there is no `--explain-ranking` |
| 10.3 | `investigate --alert-policy <id> --replay DIR` | `investigate 'gcp.monitoring.alert_policy=<id>' --at <instant>`; there is no `--alert-policy` flag, and the namespace is `gcp.monitoring.alert_policy` |

## §11 — If a fixture goes red after Google changes an answer

| # | command | outcome |
|---|---------|---------|
| 11.1 | `fixture verify --filter gcp-baseline-topology-01 -v` | **FAILED twice**: `--filter` as 1.1, and `-v` is the **global version flag**, so it prints the version and exits. **Doc drift**, fixed to `--log-level debug`. |
| 11.2 | `fixture verify fixtures/gcp-baseline-topology-01/ --log-level debug` | ok |

## What this run changed

Nine corrections to `quickstart.md`, every one of them the doc rather than the code:

1. `migrate up` → `migrate`
2. `fixture verify --filter X` → `fixture verify fixtures/X/` (seven occurrences)
3. the step names `idempotency` and `refusal` → `double-delivery` and `expect-rejected`
4. `query subgraph --focus X --valid-at Y` → `query subgraph X --as-of Y` (three occurrences)
5. `query diff --focus X --t1 --t2 --reference-at` → `query diff X --from --to --reference`
6. `query history --ref X --both-dimensions` → `query history X`, with a line on why there is no flag
7. `resolve why --a X --b Y` → `resolve why X Y`
8. `backend list` → `backend list --gcp <org-slug>`, and `worker backend` → `worker call … --mode recorded --recording`
9. `audit coverage`'s and `investigate`'s flags, and `-v` → `--log-level debug`

And two open items recorded rather than resolved:

- **`post_reference` is never the reason an operator sees** for an excluded announced change in this
  fixture; the reason is "touched no neighbourhood". Which one §2(c) intends is a design question.
- **`fixture campaign` is not implemented**, so §8 is unreachable with or without a credential. That
  is US7's work and is already tracked as blocked.

## What this run did not verify

§7, §8, §9 and §10, for the reasons above. A quickstart is reproduced when somebody with the
credentials and the corpus runs those four sections; this record covers the seven that need neither,
and says plainly which three it did not.
