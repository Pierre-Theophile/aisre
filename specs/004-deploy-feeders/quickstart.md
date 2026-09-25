# Quickstart — Deploy Feeders (GitHub and Vercel)

**Date**: 2026-09-22 · **Plan**: [plan.md](./plan.md) · **Data model**: [data-model.md](./data-model.md)

Runnable validation for this feature, in the order a reviewer should run it. Every step is a command
plus what it must print; none needs a platform credential except §7 and §8, which say so.

Feature 003's quickstart run found **nine documented commands that did not exist in the shape the
document gave them** — every one a doc naming a flag the CLI does not have. So the rule here is the
one that run established: **a step that has not been executed is marked as such**, and this file is
re-run end to end before the feature is called done.

## 0. Prerequisites

```bash
export PG_DSN="postgres://…/sreagent?sslmode=disable"
make build                       # bin/aisre
bin/aisre migrate --db "$PG_DSN"
```

## 1. The contract items exist before anything consumes them

The four items feature 001 owes ([research §1](./research.md)). Each must be visible before the
feeders are built, because a connector that ships first and a contract that follows is the local
extension constitution IX forbids.

```bash
# the seven namespaces, each with its normalisation
go test ./pkg/feeder/ -run 'Namespace|Normalis'

# C8, and the three cases that must NOT merge
go test ./internal/resolution/ -run 'C8'

# the rollback marker and the two pointer vocabularies
buf lint && buf breaking --against '.git#branch=main'
go test ./pkg/feeder/ -run 'Vocab'
```

**Expected**: all pass. `buf breaking` clean — every schema change here is **additive**.

## 2. A GitHub deployment is a rollout, and the valid time is the platform's

```bash
bin/aisre fixture verify fixtures/github-deployment-01 --db "$PG_DSN"
```

**Expected**: replay from empty matches goldens; double-delivery is a no-op; the seeded shuffle
produces the same valid-time state. The golden must show the change's valid time equal to the
`deployment_status` completion instant — **not** the run's start and not the poll instant — and an
origin reference that opens the run and **carries no token**.

## 3. Only `success` produces a rollout

```bash
bin/aisre fixture verify fixtures/github-status-states-01 --db "$PG_DSN"
```

**Expected**: one rollout for the `success` transition; **zero** for `queued`, `in_progress`,
`pending`, `failure` or `error`; and the later `inactive` transition recorded as a **property of the
same change**, never a second rollout (FR-022). GitHub designates no terminal set, so this fixture is
where our published mapping is pinned ([research §2](./research.md)).

## 4. Vercel: `READY` is not enough

```bash
bin/aisre fixture verify fixtures/vercel-promotion-01 --db "$PG_DSN"
```

**Expected**: a `READY` + `STAGED` deployment produces **no** rollout; the same deployment reaching
`PROMOTED` produces one, valid at the promotion instant, with the build instant kept as a property.
Preview deployments produce none and the exclusion is **counted in the checkpoint**.

This is the step that would have caught the plausible wrong implementation: keying on `state=READY`
would have made every staged build a change node claiming production had moved.

## 5. One rollout, not two — the C8 acceptance test

```bash
bin/aisre fixture verify fixtures/deploy-cross-source-merge-01 --db "$PG_DSN"
bin/aisre resolve why --db "$PG_DSN" <entity-a> <entity-b>
```

**Expected**: the GitHub observation and feature 003's Cloud Run revision of the same rollout appear
**once** in the ranked change list; `resolve why` names **C8**, its score and the supporting claims.
The same fixture holds the pairs that must stay apart — a redeploy (same target, different commit)
and a monorepo run (same commit, different targets) — and they do.

```bash
bin/aisre fixture verify --report --report-json report.jsonl fixtures/*/ --db "$PG_DSN"
./scripts/check-report.sh report.jsonl
```

**Expected**: `auto_merge/C8` appears in `decisions_by_rule`, the cross-source figures are
**measured rather than NOT MEASURED**, and every assertion holds. A corpus where SC-004 and SC-017
are satisfied by having nothing to measure is the failure feature 003 recorded and this gate now
refuses.

## 6. Nothing writes, and nothing leaks

```bash
go test ./internal/feeders/github/ ./internal/feeders/vercel/ -run 'ReadOnly|Published'
./scripts/check-no-secrets.sh
```

**Expected**: every published operation is a read — **zero state changes on either platform**,
unlike 003 which has exactly one — and every live reader issues a constant-named published operation
before reaching the client. Over the whole corpus: zero environment-variable values in any form,
including hashed (SC-006), and zero telemetry payloads (SC-008).

## 7. Live run — **needs a credential**

```bash
bin/aisre feed github --installation <id> --app-assertion-command '<helper>' --dry-run
bin/aisre feed vercel --vercel-token "$VERCEL_TOKEN" --dry-run

# then, live: one cycle, and then continuously
bin/aisre feed github --org <org> --installation <id> --app-assertion-command '<helper>' \
  --map github.yaml --once
bin/aisre feed github --org <org> --installation <id> --app-assertion-command '<helper>' \
  --map github.yaml
bin/aisre feed vercel --org <team> --vercel-token "$VERCEL_TOKEN" --assert-read-only \
  --projects prj_abc=k8s.deployment:shop/storefront
```

`<helper>` prints a fresh GitHub App JWT on stdout each time it is run; the connector runs it at every
token renewal and never sees the App's private key ([docs/connectors/github.md](../../docs/connectors/github.md)
§7.2). The mapping file's format is documented in `internal/cli/feed_github_map.go`. Vercel reports
nothing about a token's write capability, so a live Vercel run needs `--assert-read-only`, the
operator's statement, and the checkpoint records `operator_asserted` accordingly
([docs/connectors/vercel.md](../../docs/connectors/vercel.md) §5). The Vercel credential flag is
`--vercel-token`, so that it cannot shadow the graph's own `--token`.

**Expected on `--dry-run`**: the startup gate runs and nothing else. GitHub enumerates the App
installation's repository selection and reports which regime it is in — `all` or `selected`
([research §2](./research.md)); Vercel reports configuration-within-the-grant. A credential with any
write permission is **refused, naming the offending permission**.

> **Not yet executed.** Needs a read-only GitHub App installation and a Vercel token from the
> platform owner.

## 8. The recording campaign — **needs a credential and a named signatory**

```bash
bin/aisre fixture campaign record  fixtures-private/campaign-<date> …
bin/aisre fixture campaign scan    fixtures-private/campaign-<date>
bin/aisre fixture campaign sign    fixtures-private/campaign-<date> --signer "<name>"
bin/aisre fixture campaign verify  fixtures-private/campaign-<date>
bin/aisre fixture campaign parity  <live-dir> --recorded <recording>
```

The gates feature 003 built, reused unchanged — FR-059–FR-070 are 003's requirements with the
platform names swapped, so this is a data task, not a code task.

> **Not yet executed.** `sign` requires a **named human signatory** and, where a second person
> exists, a second signature (FR-064). No automation can supply that, by design — and
> `fixture campaign verify` refuses a recording that is not covered by a signed, current manifest.

## 9. The SRE's question, in one command under thirty seconds (SC-016)

```bash
time bin/aisre investigate --replay fixtures/… --at <alert-instant> --explain-ranking
```

**Expected**: the in-scope changes in the preceding window, each with its actor, actor kind, the
commit it shipped, whether it was a rollback, and a link that opens the run. Under thirty seconds on
the recorded corpus.

---

## What this file asserts about itself

| section | runnable today | why not |
|---|---|---|
| §0–§6, §9 | ✅ | fixtures and unit tests only |
| §7 | ❌ | a read-only GitHub App installation and a Vercel token |
| §8 | ❌ | the above, plus a named human signatory (FR-064) |
