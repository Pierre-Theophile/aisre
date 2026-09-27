# Quickstart — Datadog Connector

**Date**: 2026-09-27 · **Plan**: [plan.md](./plan.md) · **Data model**: [data-model.md](./data-model.md)

Runnable validation, in the order a reviewer should run it. Every step is a command and what it must
print. §0–§6 were run end to end on a clean worktree on 2026-09-27, and the drift that run exposed is
fixed here: [quickstart-run-2026-09-27.md](./quickstart-run-2026-09-27.md). §7 and §8 need a
credential and say so.

## 0. Prerequisites

```bash
export PG_DSN="postgres://…/sreagent?sslmode=disable"
make build
bin/aisre migrate --db "$PG_DSN"
```

## 1. The contract changes landed, additively

```bash
buf lint && buf breaking --against '.git#branch=main'     # VersionBreakdown.deploy_ref is additive
go test ./pkg/feeder/ -run 'ReadOnly|NamedQuery'           # POST only as a named, listed operation
go test ./pkg/feeder/ -run 'Datadog|Bucket'                # relative reset, X-RateLimit-Name buckets
go test ./pkg/feeder/ -run 'Vocab'                          # datadog-logs/v1, datadog-monitor/v1 registered
go test ./internal/resolution/ -run 'C9'                    # merges on name + environment only
go test ./internal/investigation/backend/ -run 'Unstamped'  # engine answers NO_DATA, no backend call
```

**Expected**: all pass; `buf breaking` clean; no golden moves.

## 2. Version stamps, on any deployment type

```bash
go test ./pkg/feeder/versionstamp/...
bin/aisre fixture verify fixtures/datadog-errors-by-version-01 --db "$PG_DSN"
bin/aisre fixture verify fixtures/datadog-log-source-01 --db "$PG_DSN"
go test ./internal/investigation/backend/ -run 'Unstamped' -v
```

**Expected**: in `datadog-errors-by-version-01` each group names its `deploy.commit_sha` ref, and a
group of abbreviated shas carries `ABBREVIATED_SHA` and no ref. In `datadog-log-source-01`, `search`
has the audit's shape (an SDK `@version` on start-up lines only): its pointer carries no version join
key, and on that pointer `errors_by_version` answers `NO_DATA`, listing each convention with its line
and error-line shares (`TestTheUnstampedPointerIsAnsweredByTheEngine`). No separate
`datadog-unstamped-01` was built (T050). The GCP backend's `errors_by_version` fills `deploy_ref` too, proving the rule is not
Datadog's.

## 3. The backend answers all eight terms without tracing

```bash
bin/aisre fixture verify fixtures/datadog-backend-logs-01 --db "$PG_DSN"
```

**Expected**: replay-from-empty, double delivery and shuffle pass; the world recording answers every
in-algebra request with a miss rate at or below the threshold; `error_spans` answers `NO_DATA` naming
the absent span source; `new_log_patterns` states its sample against the window's total; zero APM or
span calls in the request log.

## 4. Monitors become alerts; the doorbell is not data

```bash
bin/aisre fixture verify fixtures/datadog-monitor-transitions-01 --db "$PG_DSN"
bin/aisre fixture verify fixtures/datadog-grouped-monitor-01 --db "$PG_DSN"
bin/aisre fixture verify fixtures/datadog-doorbell-forged-01 --db "$PG_DSN"
```

**Expected**: valid times equal Datadog's stated instants; histories marked `sampled`; one alert per
alerting group; forged rings cost at most one poll and write nothing.

## 5. One service, one rollout — whatever deployed it

```bash
bin/aisre fixture verify fixtures/datadog-log-service-merge-01 --db "$PG_DSN"
bin/aisre fixture verify fixtures/datadog-log-rollout-01 --db "$PG_DSN"
bin/aisre fixture verify fixtures/datadog-log-rollout-merge-01 --db "$PG_DSN"
```

**Expected**:
- `datadog-log-service-merge-01`: the Datadog log service and a Cloud Run service with the same OTel
  name and environment are **one entity by C9**; the same name in staging stays apart.
- `datadog-log-rollout-01`: a service on a platform no feeder covers gets a ROLLOUT from its stamp,
  marked `valid_from_is_a_bound`.
- `datadog-log-rollout-merge-01`: the same commit deployed by Cloud Run appears **once** in the ranked
  change list, merged by C8, carrying the stated instant as its rollout instant (research §5 O3).

And the pieces that came after the first draft of this file:

```bash
bin/aisre fixture verify fixtures/datadog-tags-01 --db "$PG_DSN"              # owners and identity claims from tags
bin/aisre fixture verify fixtures/datadog-monitor-to-owner-01 --db "$PG_DSN"  # SC-016's corpus
go test ./internal/cli/ -run 'SC016FromADatadogMonitorIDAlone'                 # monitor id → alert, watched, changes, owner, pointers
```

```bash
bin/aisre fixture verify --report --report-json report.jsonl fixtures/*/ --db "$PG_DSN"
./scripts/check-report.sh report.jsonl
```

**Expected**: every fixture passes; `auto_merge/C9` and `auto_merge/C8` both appear in
`decisions_by_rule` with a Datadog pair.

## 6. Nothing writes, nothing leaks

```bash
go test ./internal/feeders/datadog/ ./internal/backends/datadog/ -run 'Published|Surface|NeverDeclared|NoSelector|Canary|Clean'
./scripts/check-no-secrets.sh
```

**Expected**: every issuable request is on [the operation list](./contracts/read-only-operations.md);
no canary seeded into a recording survives into any fixture; no credential or site host in any
pointer.

## 6b. The budget holds to the exact call

```bash
bin/aisre fixture verify fixtures/datadog-rate-limited-01 --db "$PG_DSN"
go test ./internal/feeders/datadog/ -run 'TheBudgetHoldsToTheExactCall'
```

**Expected**: under a falling remaining quota and a 429, discovery stops at the reserve (typed
`quota`), the poll is partial (typed `rate_limited`) and resumes at the stated reset, and the calls
served, counted and reported are one number.

## 7. Live — **needs a credential**

```bash
bin/aisre feed datadog --site datadoghq.eu --dry-run
bin/aisre feed datadog --site datadoghq.eu --watch production/<service> --once
```

**Expected on `--dry-run`**: the capabilities in force and the operations each declares; the startup
gate's verdict — scopes verified, or `operator_asserted` required with the reason (research §5 O2).
**Expected on the first live run against today's organisation**: the version-stamp verdict for its one
service reads "no stamp", listing each convention and its shares — until the stamp is added per
`docs/connectors/version-stamping.md`, after which the verdict names the attribute.

> **Not yet executed.** Needs a read-only Datadog key from the administrator.

## 8. The recording campaign — **needs a credential and a named signatory**

The `fixture campaign` flow from 003, with the Datadog scope fields (plan Gap G6). `sign` requires a
named human signatory (FR-076); no automation supplies one.

> **Not yet executed.**

---

## What this file asserts about itself

| section | runnable once built | why not |
|---|---|---|
| §0–§6b | ✅ run 2026-09-27 | fixtures and unit tests only |
| §7 | ❌ | a read-only Datadog key |
| §8 | ❌ | the above, plus a named signatory |
