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
bin/aisre fixture load fixtures/attribution/catalog-alias-01
bin/aisre catalog list --as-of 2026-09-01T14:00:00Z
bin/aisre catalog show checkout-flow
```

Expected: replay from empty, doubled and shuffled is byte-identical; `catalog show` lists the aliases
`otel.service.name=checkout` and `notion.value=<db>/Checkout Flow`, each with the decision and
principal that created it. Each `fixture load` wants a fresh database: the attribution fixtures share
monitor ids under one source, and a second fixture's history collides with the first's.

## 2. A postmortem filed under its human name is boosted (US1, SC-001, SC-006)

```sh
bin/aisre fixture verify fixtures/attribution/catalog-alias-01 fixtures/attribution/catalog-alias-gcp-01
go test ./internal/investigation/workers/knowledge/ -run 'InScope|Graph'
bin/aisre attribute why datadog.monitor=20110
```

Expected: the incident step of each fixture holds its decisive predicates, among them the *Checkout
Flow* postmortem cited `in_scope: true` above the equally matching *Infra* one. The GCP twin, with no
Datadog connector, reaches the same attributions in its goldens. `why` shows the `A1` decision, then the correction
that confirmed it. `eval attribution` on these two cases alone exits 4 on purpose: every attribution in
them goes through a person's alias, which is reported and not gated, so there is nothing to gate (step 4
runs it on the whole set).

## 3. A correction wins, once and for all (US3, FR-040–FR-046, SC-004, SC-005)

```sh
bin/aisre fixture verify fixtures/attribution/correction-01
bin/aisre fixture load fixtures/attribution/correction-01
bin/aisre attribute why datadog.monitor=101
bin/aisre attribute why datadog.monitor=102
bin/aisre attribute correct datadog.monitor=102 --entry infra --reason "a signal-level correction"
bin/aisre attribute why datadog.monitor=102
```

Expected: monitor 101 shows the `A1` decision for *Infra*, the signal-level correction, the name-level
one that moved `datadog.log_service=candidate-api` to `candidate-view`, and the revert, each with its
principal and instant. Monitor 102 was re-attributed by the attributor at layer 0 after the name-level
correction. The new correction makes it *Infra* (`H`) and marks the previous decision superseded. The
fixture's `-0930` goldens are an investigation pinned before any correction: it still answers *Infra*.

## 4. Discovery and `unknown` (US2, US4, FR-010–FR-015, FR-032, SC-002, SC-003, SC-008)

```sh
bin/aisre fixture verify fixtures/attribution/discovery-01
bin/aisre fixture load fixtures/attribution/discovery-01          # into a fresh database
bin/aisre catalog proposals --status all
bin/aisre eval attribution fixtures/attribution/
```

Expected: `candidate-api` is a proposed entry; the Cloud Run service's `P2` suggestion for *Candidate
View* (0.65) is accepted, the `P1` one rejected, and *Other* hidden, by the fixture's own decisions.
The evaluation reports aggregate precision ≥ 0.95 and every calibration bin within 10 points, with
declared-alias attributions reported but not gated; the ambiguous case is `unknown` with its
suggestions.

## 5. The classifier is optional (US5, FR-034, FR-035, SC-007)

```sh
bin/aisre eval attribution fixtures/attribution/classifier-01             # recorded vectors
go test ./internal/catalog/classify/... ./internal/catalog/ -run 'Classifier'
bin/aisre serve ...                                                       # no attribution.classifier block
```

Expected: with recorded vectors, the five monitors on opaque workloads that no rule names are
attributed by `M`, all correctly, at a stated ~0.91; the three whose text is too thin stay `unknown`.
With no classifier block, `serve` logs `attribution: no classifier; layers 0-2 decide` with
`ruleset=attribution/rules@1 classifier=none`, and makes no network call for attribution.

## 6. Legacy maps migrate (FR-006, R11)

```sh
bin/aisre catalog import --from-notion-config internal/cli/testdata/notion-twin-02.yaml --dry-run
bin/aisre feed notion --check --config internal/cli/testdata/notion-twin-02.yaml \
  --replay fixtures/notion-knowledge-twin-02
```

Use your own 007 configuration in place of the test one. Expected: one entry and one alias per line of
the old map; `--check` lists each Service value with the entry it maps to, or `unmapped` or `hidden`,
and no page text.
