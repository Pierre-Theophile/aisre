# Quickstart: validating feature 009

A validation guide, not a tutorial. Each step names the requirement it proves.

## Prerequisites

- PostgreSQL 16 reachable as `$PG_DSN`, migrated (`aisre migrate`): migration 0015, no extension beyond
  `btree_gist`.
- `make build` → `bin/aisre`.
- For step 5 only: an `embedding:` block as in 008's quickstart (hosted or self-hosted).

## 1. The catalog replays and is read as of any instant (FR-001–FR-005)

```sh
bin/aisre fixture verify fixtures/attribution/catalog-alias-01
bin/aisre catalog list --as-of 2026-10-19T14:00:00Z
bin/aisre catalog show candidate-view
```

Expected: replay from empty, doubled and shuffled is byte-identical; `catalog show` lists the aliases
`datadog.log_service=candidate-api` and `notion.value=<db>/Candidate View`, each with the decision and
principal that created it.

## 2. A postmortem filed under its human name is boosted (US1, SC-001, SC-006)

```sh
bin/aisre eval attribution fixtures/attribution/catalog-alias-01 fixtures/attribution/catalog-alias-gcp-01
go test ./internal/investigation/workers/knowledge/ -run 'CatalogScope'
```

Expected: the alert on `candidate-api` is attributed to `candidate-view` by `A1`; the *Candidate View*
postmortem is cited `in_scope: true` and ranks above the equally matching *Infra* one. The GCP twin, with
no Datadog connector, reaches the same attributions.

## 3. A correction wins, once and for all (US3, FR-040–FR-046, SC-004, SC-005)

```sh
bin/aisre attribute correct slack.channel=C0123 --entry auth
bin/aisre attribute correct datadog.monitor=777/service:auth-gw --entry auth --name datadog.log_service=auth-gw
bin/aisre attribute why datadog.monitor=778/service:auth-gw
bin/aisre fixture verify fixtures/attribution/correction-01
```

Expected: the first correction is signal-level; the second becomes an alias, and the next alert on
`auth-gw` is attributed by `A1`. `why` names the principal, the instant and the decision overridden. An
investigation pinned before the correction replays with its original attribution.

## 4. Discovery and `unknown` (US2, US4, FR-010–FR-015, FR-032, SC-002, SC-003, SC-008)

```sh
bin/aisre fixture verify fixtures/attribution/discovery-01
bin/aisre catalog proposals
bin/aisre catalog alias accept "notion.value=<db>/Candidate View" candidate-api
bin/aisre eval attribution fixtures/attribution/
```

Expected: `candidate-api` (Datadog and Cloud Run, one 001 entity) is one proposed entry; *Candidate
View* is another, with a `P2` suggestion and its score. Accepting it attributes both names. The
evaluation reports precision ≥ 0.95 and every calibration bin within 10 points; the ambiguous case is
`unknown` with two suggestions.

## 5. The classifier is optional (US5, FR-034, FR-035, SC-007)

```sh
bin/aisre eval attribution fixtures/attribution/classifier-01             # recorded outputs
go test ./pkg/attribution/... ./internal/catalog/classify/...
bin/aisre serve ...                                                       # no attribution.classifier block
```

Expected: with recorded outputs, the *Other* postmortem is attributed to `candidate-view` by `M`; with no
classifier block, `serve` logs `attribution: ruleset=attribution/rules@1 classifier=none` and makes no
network call for attribution.

## 6. Legacy maps migrate (FR-006, R11)

```sh
bin/aisre catalog import --from-notion-config deploy/notion.yaml --dry-run
bin/aisre feed notion --check --config deploy/notion.yaml
```

Expected: one alias per line of the old map; `--check` lists each Service value with the entry it maps
to, or `unmapped` or `hidden`, and no page text.
