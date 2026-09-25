# Contributing to sre-agent

Thank you for considering a contribution. This project builds a bitemporal, event-sourced
graph of a production system: the substrate an AI SRE agent reasons over. Accuracy comes from
the graph, so the rules below exist to protect it. They are derived from the project
constitution (`.specify/memory/constitution.md`), which supersedes this document wherever the
two disagree.

## Ground rules, in short

1. **A feeder without a fixture is not merged.** No exceptions.
2. **Schema changes require an ADR.** No ADR, no merge.
3. **Every commit is signed off** under the Developer Certificate of Origin.
4. **Commit messages follow Conventional Commits.**
5. **History is append-only.** Nothing in the event log or in a `*_versions` table is ever
   dropped, deleted or truncated.

The rest of this document explains each of them.

## Developer Certificate of Origin (DCO)

This project uses the [Developer Certificate of Origin 1.1](https://developercertificate.org/)
instead of a CLA. You certify the origin of your contribution by signing off every commit:

```bash
git commit -s -m "feat(projector): split overlapping valid intervals on upsert"
```

`-s` appends a trailer to the commit message:

```text
Signed-off-by: Jane Developer <jane@example.com>
```

The name and email must be your real identity and must match your Git author identity. A pull
request with an unsigned commit will not merge; fix it with
`git rebase --signoff origin/main` and force-push your branch.

## A feeder without a fixture is not merged

A *connector* is the concept (an integration with one external system); a *feeder* is its
implementation against the SDK in `pkg/feeder`. Every feeder contribution must ship, in the
same pull request:

- **Recorded real-world payloads**, sanitized, under `fixtures/<fixture-id>/payloads/<source>/`.
  Synthetic-only data is not sufficient for a feeder to be marked stable.
- **The event stream it produces**, `fixtures/<fixture-id>/events.jsonl`, with recorded
  observed times.
- **Golden outputs**, `fixtures/<fixture-id>/golden/*.json`, for every query the fixture
  exercises, plus the `manifest.yaml` that declares them.

The same rule applies to any feature that changes what lands in the graph: if you cannot name
the fixture your change is validated against, the change is not ready. See
`specs/001-temporal-graph-core/contracts/fixture-format.md` for the format and
`docs/connectors/` for the feeder walkthrough.

CI replays every fixture from an empty database, double-delivers every event to prove
idempotency, and shuffles events within each source's reordering window to prove
order-independence. Regression on any golden fails the build.

## Schema changes require an ADR

`api/sreagent/graph/v1/graph.proto` is the published contract of this project — the primary
public artifact, ahead of any particular agent built on it. Third parties write feeders and
consumers against it.

If your change touches the proto (or any internal type that surfaces in a public query result
or event payload), then:

1. Write an ADR under `docs/decisions/` as `ADR-NNNN-short-title.md`, following the existing
   ones: context, decision, consequences, and the alternatives you rejected.
2. Follow semantic versioning on the schema package. A **breaking** change needs a MAJOR bump
   *and* a migration expressed as an event-log transformation, so that replaying an old log
   still reproduces the graph.
3. Pass `buf breaking` against `main`. CI runs it; run it locally with
   `buf breaking --against '.git#branch=main'`.
4. Update `docs/schema/` in the same pull request.

Renaming a field or an enum value is a breaking change even when the wire format survives,
because the JSON surface changes. Prefer adding.

## Conventional Commits

Commit subjects follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/):

```text
<type>(<scope>): <description>
```

Types in use: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`,
`revert`. Scopes are the package or area touched, for example `projector`, `query`, `schema`,
`feeder/k8s`, `fixtures`, `ci`.

A breaking change is marked with `!` after the scope and a `BREAKING CHANGE:` footer
explaining the migration:

```text
feat(schema)!: replace Pointer.selector with a structured selector

BREAKING CHANGE: Pointer.selector is removed. Event logs recorded under schema 1.x are
migrated by the transformation in internal/store/postgres/migrations/0007_pointer_selector.sql.
```

Keep the subject in the imperative mood, under 72 characters, with no trailing period.

## Before you open a pull request

```bash
make gen      # regenerate protobuf/Connect code; fails the build if stale
make build    # static binary into bin/aisre
make test     # unit, property and replay tests (needs PG_DSN, see deploy/README.md)
make lint     # golangci-lint, including the SPDX header rule
make verify   # replay every fixture and diff against its goldens
```

Every Go file — generated code included — starts with:

```go
// SPDX-License-Identifier: Apache-2.0
```

`make gen` re-applies it to generated sources automatically.

## What reviewers will ask

Reviewers check compliance with the constitution, not only correctness. Expect these
questions, and answer them in the pull request description before they are asked:

- **Does this bypass the graph?** Every feature is either a read of the graph with an as-of
  instant, or a typed idempotent event into it. Nothing else is a feature.
- **Is this bitemporal?** Every node and edge carries a valid interval and an observed
  interval, both half-open, both required. Corrections close an observed interval and open a
  new version; retractions close a valid interval; nothing is deleted.
- **Does it store telemetry?** The graph stores *pointers* to metrics, logs and traces, never
  the metrics, logs or traces themselves.
- **Is the conclusion auditable?** Anything the system asserts carries the event ids and query
  parameters that produced it, and an explicit, calibrated confidence. `unknown` is always a
  valid answer; a guess where the evidence does not support one is a defect.
- **Is it read-only toward production?** Connectors use read-only credentials. A connector
  that needs write scope is rejected.
- **Is it the simple version?** New dependencies, services and abstraction layers are
  justified in the plan's Complexity Tracking table, with the simpler alternative that was
  rejected and why.

## Reporting problems

- **Bugs and feature requests**: open a GitHub issue. For a wrong answer from the graph,
  include the fixture or the event stream that reproduces it — that is what makes it fixable.
- **Security issues**: do not open an issue. Follow `SECURITY.md`.

## Licence

By contributing, you agree that your contribution is licensed under the Apache License,
Version 2.0, as set out in `LICENSE`, and you certify the DCO above.
