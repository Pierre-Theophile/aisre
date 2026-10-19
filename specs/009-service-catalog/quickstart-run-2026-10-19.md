# Quickstart run — 2026-10-19

**Quickstart**: [quickstart.md](./quickstart.md) · **Task**: T039

## Setup

The run used the branch head after T038, built with `make build`, against a local PostgreSQL 16. Each
step ran against a `serve --auth dev --dev --migrate` on a database created for the run, with no
`attribution.classifier` block, except where a step says a second database was needed.

## Result

§1–§6 pass. The draft quickstart was written before the fixtures, and seven places in it did not match
them; each is fixed in [quickstart.md](./quickstart.md).

| § | Command | Result |
|---|---|---|
| 1 | `fixture verify fixtures/attribution/catalog-alias-01` | passed: 38 events, 10 goldens, doubled and shuffled; the incident holds 5 decisive predicates |
| 1 | `catalog list --as-of …`; `catalog show …` | **drift**: the fixture's entry is `checkout-flow` on 2026-09-01, not `candidate-view` on 2026-10-19. With those: the aliases `otel.service.name=checkout` and `notion.value=<db>/Checkout Flow`, each with its decision and principal |
| 2 | `eval attribution catalog-alias-01 catalog-alias-gcp-01` | **drift**: exits 4 by design. All 6 attributions go through a person's alias, which is reported and not gated (ADR-0015 Item 10), so the gate has nothing to judge and says so. The step now verifies the two fixtures instead, and step 4 runs the evaluation on the whole set |
| 2 | `go test ./internal/investigation/workers/knowledge/ -run 'CatalogScope'` | **drift**: matches no test, so it passed vacuously. The in-scope boost is proven by the fixture's incident predicates and by `-run 'InScope\|Graph'`: ok |
| 2 | `attribute why datadog.monitor=20110` | `checkout-flow` by `A1`, then the correction that confirmed it, with principal and instant |
| 3 | `fixture verify` and `load` correction-01; `why` 101 and 102 | **drift**: the draft's `slack.channel=C0123` and `auth-gw` refs exist in no fixture; the step now uses correction-01's monitors. Passed: 101 shows `A1` for *Infra*, the signal-level correction, the name-level one and the revert; 102 was re-attributed at layer 0 after the name-level correction |
| 3 | `attribute correct datadog.monitor=102 --entry infra` | recorded with its principal; `why` shows *Infra* (`H`) and the previous decision superseded |
| 3 | `query subgraph datadog.monitor=101 --as-of 09:30` | the `attributed_to` edge to *Candidate View* opened at 09:10, as the fixture's pinned goldens expect |
| 4 | `fixture load fixtures/attribution/discovery-01` into the same database | **drift**: refused (`close_observed: closed_at … is not after`), because correction-01 and discovery-01 both use monitor 101 under `datadog:demo`. Loaded into a fresh database it applies all 29 events. The quickstart now says each load wants a fresh database |
| 4 | `catalog proposals --status all` | `candidate-api` proposed (D1); the Cloud Run `P2` suggestion for *Candidate View* accepted at 0.65, the `P1` one rejected, by the fixture's own decisions. The draft's `alias accept` is one of those decisions, so it is no longer a separate command |
| 4 | `eval attribution fixtures/attribution/` | gate passes: 27 labelled signals in 6 cases; aggregate precision 1.0000 on 7 inferred attributions (1 discovered, 1 probable, 5 classifier), calibration gap 0.0724; declared aliases 8 of 10, reported. Exit 0 |
| 5 | `eval attribution fixtures/attribution/classifier-01` | **drift**: the draft expected an *Other* postmortem attributed to `candidate-view`; the classifier case that was built is ten monitors on opaque workloads. 5 attributed by `M`, all right, stated 0.9144; 501–503 `unknown`. Gate passes |
| 5 | `go test ./pkg/attribution/... ./internal/catalog/classify/...` | ok; `pkg/attribution` has no tests of its own, its conformance suite runs from `classify`. The step now adds `./internal/catalog/ -run Classifier`: ok |
| 5 | `serve` with no classifier block | **drift** in wording: the line is `attribution: no classifier; layers 0-2 decide` with `ruleset=attribution/rules@1 classifier=none` as fields |
| 6 | `catalog import --from-notion-config … --dry-run` | **drift**: there is no `deploy/notion.yaml`; the step uses the test configuration. One entry and two aliases per line of the old map |
| 6 | `feed notion --check …` | `catalog: 3 name(s) observed`, each `unmapped` on a fresh catalog, no page text |

## Found on the way, and not changed here

- After a person's alias supersedes a discovered one, the discovered entry stays `proposed` with no
  names (`checkout` in §1, `candidate-api` in §4). It is harmless, but a reader may wonder what it is;
  hiding it or showing why it is empty is a follow-up.
- `catalog proposals --status` filters alias proposals only; entry proposals are always listed. The
  flag's help says so.
