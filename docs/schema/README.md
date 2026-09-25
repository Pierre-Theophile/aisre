# The published schema

The graph schema and its temporal model are the primary public artifact of this project. The
agent on top may be replaced; the schema is meant to outlive it. Third parties write feeders
against it and consumers against it, so it is versioned, documented and guarded in CI like a
public API — because it is one.

## Where it lives

| Path | What it is |
|------|------------|
| `api/sreagent/graph/v1/graph.proto` | The schema. Source of truth. |
| `api/sreagent/graph/v1/graph.pb.go` | Generated Go message types (`package graphv1`). |
| `api/sreagent/graph/v1/graphv1connect/` | Generated Connect handlers and clients. |
| `api/sreagent/investigation/v1/investigation.proto` | The investigation schema: the service, the hypothesis ledger, the query algebra, the digest contract, the recording format, the fixture ground truth. Source of truth. |
| `api/sreagent/investigation/v1/investigation.pb.go` | Generated Go message types (`package investigationv1`). |
| `api/sreagent/investigation/v1/investigationv1connect/` | Generated Connect handlers and clients. |
| `buf.yaml`, `buf.gen.yaml` | Lint, breaking-change and code-generation configuration. |
| `docs/schema/temporal-model.md` | The bitemporal model, explained from scratch. |
| `docs/schema/queries.md` | The query contract: what each read means and what it guarantees. |
| `docs/schema/ranking.md` | The published change-ranking score (FR-028). |
| `docs/schema/algebra.md` | The query algebra: the only vocabulary a worker may be asked in. |
| `docs/schema/digests.md` | The digest contract: what may cross the worker boundary. |
| `docs/schema/ledger.md` | The hypothesis ledger, the posterior rule and the verdict rule. |
| `docs/schema/investigation.md` | The events an investigation emits, and what is derived rather than recorded. |
| `docs/schema/announced-facts.md` | A fact whose valid time begins after its observed time: the asymmetry, the state machine, and why a cancellation is a correction rather than a retraction. |

Two packages are published, and both are gated by `buf lint` and `buf breaking` on every pull
request. `sreagent.graph.v1` carries the node and edge taxonomies, the change taxonomy,
pointers, the event types a feeder emits, and the query contract.
`sreagent.investigation.v1` carries the query algebra, the digest contract (which features 003
and 005 implement rather than redefining), the hypothesis ledger, the investigation service,
the recording format and the fixture ground truth. Go types are not the public surface —
`pkg/feeder`, `pkg/worker` and `pkg/backend` aside, every Go package is `internal/`.

A major bump is per package: the protobuf package carries its major version in its name, so
`sreagent.graph.v1` and `sreagent.investigation.v1` version independently and one may major
without the other.

## Generating

```sh
make gen
```

which runs:

1. `buf lint` — the schema must stay conventional.
2. `buf generate` — `protoc-gen-go` and `protoc-gen-connect-go`, both local binaries, both
   writing into `api/` with `paths=source_relative`.
3. `scripts/add-spdx-headers.sh` — protoc emits no licence header; every Go file in this
   repository carries `// SPDX-License-Identifier: Apache-2.0`, generated code included.

Generated code is committed. That keeps `go get` working without a protobuf toolchain and
makes schema changes visible in review as a diff rather than as a promise. CI regenerates and
fails if the result differs from what is checked in, so the two cannot drift.

Install the toolchain with `make tools`.

### Human-readable docs

Prose documentation of the schema is written by hand, here in `docs/schema/`, because the
parts worth explaining — what "observed time" means, when to emit a retraction rather than a
correction — are not derivable from field names. Comments in the `.proto` carry the per-field
detail and are the reference; this directory carries the model.

A rendered API reference generated from the proto (`buf generate` with a documentation
plugin, published alongside releases) is planned and not yet wired up.

## Versioning

The schema follows [semantic versioning](https://semver.org/), tagged separately from the
binary as `schema/vX.Y.Z`. The protobuf package carries the major version in its name
(`sreagent.graph.v1`), so a major bump is a new package directory living beside the old one,
and two majors can be served at once during a migration.

Every event carries the `schema_version` it was produced under, so a log written years ago
still says what it meant.

- **PATCH** — comments, documentation, nothing observable on the wire.
- **MINOR** — additive and backward compatible: a new message, a new field, a new enum value,
  a new RPC. Existing clients keep working without changes.
- **MAJOR** — anything else. See below.

Additive changes still deserve thought. A new enum value reaches old clients as the unknown
value, which is why `ChangeKind` has `CHANGE_KIND_OTHER` and a `kind_other` string: the change
taxonomy is extensible without a major bump, by design (FR-003).

## Breaking-change policy

**A breaking change requires three things together: a MAJOR version bump, an ADR, and a
migration expressed as an event-log transformation.** Missing any one of them, it does not
merge.

The third is the unusual one, and it follows from the event-sourced design: the graph is a
projection of the log, and replaying the full log from empty must reproduce it exactly. If a
schema change made old events unreplayable, every recorded incident and every fixture would
become unreadable history. So a breaking change ships with the transformation that maps old
events to new ones, and replay stays total.

What counts as breaking:

- Removing or renaming a field, message, enum, enum value, RPC or service.
- Changing a field's type, its number, or its cardinality.
- Moving a field into or out of a `oneof`.
- Renaming anything at all, even where the wire format survives — the JSON surface is part of
  the contract, and it uses names.
- Tightening validation so that events a previous version accepted are now rejected.

What does not count:

- Adding a message, field, enum value, RPC or service.
- Deprecating a field (mark it `[deprecated = true]`, keep it, reserve the number when it is
  eventually removed in a major).
- Comments, and changes to generated-code options that do not alter the wire or JSON surface.

`buf breaking --against '.git#branch=main'` runs on every pull request with the `FILE`
ruleset, so an accidental break is caught before review. A deliberate one is a new package
version, not an exception to the check.

### Where the migration lives

The event-log transformation is a `migrate.Transformer` registered in
`internal/log/migrate`: a `From` version, a `To` version, and an `Apply` that rewrites one
`EventEnvelope` from the first shape into the second. The registry resolves a *chain* — an
event recorded at 1.0.0 reaching a build that speaks 1.2.0 runs 1.0.0→1.1.0 then 1.1.0→1.2.0 —
and the append path (`internal/log/append.go`) runs that chain before validation, so what is
stored, projected and replayed is always the current shape. An envelope at a version no chain
reaches is refused with the published reason code `unknown_schema_version`, and the rejection
names the versions the build accepts (FR-024, FR-025, edge case "schema version drift"). A
transformer may rewrite any part of the payload but never the event id, the idempotency key or
the source id — those are what make a re-delivery a no-op — and it must be deterministic and
side-effect free, because it runs again on every replay. Today the graph speaks exactly one
version and the only registered transformation is the identity `1.0.0 → 1.0.0`; the mechanism
is in place before the first breaking change on purpose, so that the migration is written
against a hook the fixtures already exercise rather than one invented under pressure.

### Lint exceptions

`buf.yaml` disables four standard lint rules, each because the alternative would be a rename
of the published contract: `ENUM_VALUE_PREFIX`, `RPC_REQUEST_STANDARD_NAME`,
`RPC_RESPONSE_STANDARD_NAME` and `RPC_REQUEST_RESPONSE_UNIQUE`. The file documents the reason
for each. New messages and enums should follow the standard rules; the exceptions exist for
the shapes that are already published, not as a general licence.

## Changing the schema

1. Open an ADR under `docs/decisions/` — context, decision, consequences, rejected
   alternatives.
2. Edit `api/sreagent/graph/v1/graph.proto`.
3. `make gen`, and commit the generated files with the change.
4. Bump the version and write the changelog entry. For a major, write the event-log
   transformation too.
5. Update `docs/schema/` in the same pull request.
6. Add or update the fixture that exercises the change. A change to what lands in the graph
   without a fixture does not merge.

`CONTRIBUTING.md` has the full procedure.

## Compatibility promises

- **Unknown fields are preserved**, not dropped, per protobuf 3 semantics.
- **Unknown enum values** arrive as the raw number; treat them as "something newer than me",
  never as the zero value. Zero values are always `*_UNSPECIFIED` and always mean "not set".
- **Observed history is immutable.** A response for a given `(valid_at, observed_at)` pair is
  reproducible: it cannot change as new facts arrive, only as the schema majors.
- **One deployment, one organisation.** The schema carries no tenancy dimension; environment,
  region, cluster and account are properties or nodes inside a single graph, never separate
  graphs.

## Evaluation

The schema is guarded by CI; what it is *used for* is guarded by a published gate. The metrics an
investigation is scored on — pass@1, harm rate, confidently-wrong rate, citation validity, the
recording miss rate, and the detection power each of them has on the corpus that measures it —
are in [`docs/evaluation/investigation-metrics.md`](../evaluation/investigation-metrics.md). The
numbers the gate actually enforces are machine-readable in
[`docs/evaluation/thresholds.json`](../evaluation/thresholds.json), read by
`scripts/check-report.sh --investigation`.

Two of those numbers belong on this page because they are schema-adjacent. `coverage_ceiling`
(0.615385, audit `2026-09`) is the share of classifiable production incidents whose cause was
*observable* in the feeder set in force; no published accuracy target may exceed it, because an
investigator cannot name a cause no feeder carries. And `pass_at_1` is `null` until the first full
corpus run publishes it — while it is null the gate prints `unset` and does **not** pass as though
it had been met. It is the same discipline as `unknown` in the temporal model: the absence of a
measurement is stated, never defaulted.

## Where to go next

- `docs/schema/temporal-model.md` — valid time, observed time, and what "as known at" means.
- `docs/schema/queries.md` — the query contract, and `docs/schema/ranking.md` for the score it ranks changes by.
- `docs/schema/algebra.md`, `docs/schema/digests.md`, `docs/schema/ledger.md`,
  `docs/schema/investigation.md` — what an investigation may ask, what may come back, how belief is
  computed from it, and what of it reaches the graph.
- `docs/connectors/` — writing a feeder against the event contract.
- `specs/001-temporal-graph-core/contracts/fixture-format.md` — the replay fixture format.
- `.specify/memory/constitution.md` — the rules the schema exists to enforce.
