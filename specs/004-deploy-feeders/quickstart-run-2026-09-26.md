# Quickstart run — a clean worktree, no platform credential (T134)

**Date** 2026-09-26 · **Task** T134 · **Feature** 004
**Tree** `git worktree add --detach <scratch>/qs-clean ebacbf7`: a detached checkout with no `bin/` and
nothing built. **Database** a fresh `qs_clean_20260926` on a local PostgreSQL 16.13. **Environment**
Linux amd64, Go 1.27.0, buf 1.73.0, no GitHub or Vercel credential anywhere in the shell.

Every command is recorded with its outcome. Where the output differed from what the quickstart
claimed, the row says whether the **doc** drifted or the **code** did, and what was changed.

**Headline: §0–§6 and §9 reproduce, and three documented commands had drifted, all doc-side.**
§7 and §8 need a credential and a named signatory, as the quickstart says.

## §0 — Prerequisites

| # | command | outcome |
|---|---------|---------|
| 0.1 | `make build` | ok, `bin/aisre` at `ebacbf7` |
| 0.2 | `bin/aisre migrate --db "$PG_DSN"` | ok — 13 migrations applied, 0 already present |

## §1 — The contract items exist

| # | command | outcome |
|---|---------|---------|
| 1.1 | `go test ./pkg/feeder/ -run 'Namespace\|Normalis'` | ok, 5 tests |
| 1.2 | `go test ./internal/resolution/ -run 'C8'` | ok — the merge, `RefusesEveryFormOfNotKnowing`, the monorepo P7, the redeploy, no key on a release, image digest vs tag |
| 1.3 | `buf lint && buf breaking --against '.git#branch=main'` | ok, clean |
| 1.4 | `go test ./pkg/feeder/ -run 'Vocab'` | ok, 5 tests |

## §2–§4 — The fixtures, and what their goldens must show

| # | command | outcome |
|---|---------|---------|
| 2.1 | `fixture verify fixtures/github-deployment-01` | passed all four steps. The golden's valid interval starts at **14:03:12**, the status's completion instant, not the run's 14:00 start or the 14:30 poll; no golden contains the token planted in `log_url`. |
| 3.1 | `fixture verify fixtures/github-status-states-01` | passed. One ROLLOUT, valid at 14:03:12, with the later `inactive` recorded as `sre.github.deactivated_at` on the same change; none for the other states. |
| 4.1 | `fixture verify fixtures/vercel-promotion-01` | passed, but **doc drift**. The staged build produces nothing and is counted (`excluded_not-promoted=1`), and the build instant is kept (`sre.vercel.built_at`). The rollout's valid start is **unknown** (`validFromUnknown: true`), not "the promotion instant" as the quickstart said: Vercel states none (research §5.2), and dating it would be the guess T084 forbids. The code is right. The doc is corrected, and now points at `vercel-preview-excluded-01` for the preview claim, which this fixture does not carry. |

## §5 — One rollout, not two

| # | command | outcome |
|---|---------|---------|
| 5.1 | `fixture verify fixtures/deploy-cross-source-merge-01` | passed — 78 events, 8 goldens |
| 5.2 | `resolve why --db "$PG_DSN" <a> <b>` | **FAILED: doc drift.** `resolve why` has no `--db`; it asks a server. Rerun as `fixture load` + `serve --auth dev --dev` + `resolve why <a> <b>` with the manifest's own pair: **SAME entity**, `auto_merge` by **C8**, score **1.00**, with the rationale naming the commit, the environment and the shared target. The doc now gives the working sequence and the two refs. |
| 5.3 | `fixture verify --report --report-json report.jsonl fixtures/*/` | **66 of 66 passed**, exit 0 |
| 5.4 | `./scripts/check-report.sh report.jsonl` | every assertion held. 320 goldens compared; 6 of 6 C8 merges explainable by the audit query; `auto_merge/C8` present in `decisions_by_rule` (4 fired). The cross-source figures are measured, not NOT MEASURED. |

## §6 — Nothing writes, and nothing leaks

| # | command | outcome |
|---|---------|---------|
| 6.1 | `go test ./internal/feeders/github/ ./internal/feeders/vercel/ -run 'ReadOnly\|Published'` | ok, 10 tests |
| 6.2 | `./scripts/check-no-secrets.sh` | clean |

## §9 — The SRE's question, one command

| # | command | outcome |
|---|---------|---------|
| 9.1 | `investigate --replay fixtures/… --at <instant> --explain-ranking` | **FAILED: doc drift, the same drift feature 003's run recorded as its row 10.2.** `investigate replay` replays an exported investigation, and there is no `--explain-ranking`. |
| 9.2 | `query diff 'gcp.cloudrun.service=twin-production/europe-west1/storefront' --at 2026-09-21T14:40:00Z` (against 5.2's server) | ok in **0.04 s**. Three ranked changes, with the table SC-016 asks for: actor, actor kind, commit, rollback, and a link that opens the run. The same with `--from/--to/--reference` gives the same three. The doc now gives this command. |

## Drift, in one list

1. §4: "valid at the promotion instant" → the valid start is unknown, and Vercel states no promotion instant.
2. §5: `resolve why --db` → `resolve why` against a server.
3. §9: `investigate --replay … --explain-ranking` → `query diff <focus> --at <instant>`.

No code drift: every behaviour the quickstart describes is what the code does, and each of the three
was a command or a claim the doc had wrong. §7 and §8 were not run, for the reasons the quickstart
gives.
