<!-- SPDX-License-Identifier: Apache-2.0 -->

# Connector PR checklist

One page. This is what a reviewer checks before a connector merges, and it is derived from
constitution VIII ("Evaluation From Day One") rather than from taste. A connector that cannot
tick every box is not rejected — it is `experimental`, and says so in its README.

Write the connector with [writing-a-feeder.md](./writing-a-feeder.md),
[writing-a-worker.md](./writing-a-worker.md) or [writing-a-backend.md](./writing-a-backend.md);
come back here before you open the pull request.

Sections 1–5 are written for a **feeder**. A **worker** or a **telemetry backend** ticks §4 (read-
only), §5 (observability) and §6 (paperwork) as written, and replaces §1–§3 with §7 and §8 below.

## 1. The fixture — the box that stops the merge

- [ ] `internal/feeders/<name>/testdata/<family>-01/` (in-tree) or the directory your test names
      exists and holds **recorded real payloads**, not synthetic ones. Synthetic-only test data
      means the connector cannot be marked stable (constitution VIII).
- [ ] `payloads/index.jsonl` lists every payload in arrival order, with `kind`, `at`, `seq`
      and `file`.
- [ ] `events.jsonl` was recorded **against a real graph**, so every `observedAt` is a log-assigned
      instant and not a recorder's wall clock. A fixture re-recorded offline is a working file,
      not a committed one.
- [ ] `manifest.yaml` carries `id`, `family`, `description`, `schema_version`, `sdk_version`,
      `hand_authored`, `events`, `sources` and `clock`, plus `queries` for any read this fixture
      should pin and `expect_rejected` for any event that must always be refused.
- [ ] The recording is **sanitised**. Host names, cluster names, account ids, user names, tokens
      and anything else that identifies a real estate are replaced, and `scripts/check-no-secrets.sh`
      passes on it.
- [ ] A `NOTICE` entry exists if the payloads come from a third-party demo application.

## 2. The conformance test

- [ ] `testkit.Conformance(t, f, dir)` passes with **no** `WithoutStreamComparison()`: the
      recorded stream is the contract.
- [ ] `Run`, `Shuffle` and `DoubleDeliver` are all green, which means: every event validates,
      nothing was refused, every ref is in a declared namespace, `Flush` ran before `Run`
      returned, arrival order changes nothing, and every second delivery is `DUPLICATE_NOOP`.
- [ ] The test needs no database and no network. If it additionally proves a real graph accepts
      the events, that is `testkit.WithEmitter`, not a hidden dependency.

## 3. The events themselves

- [ ] **No telemetry payload anywhere.** No metric sample, log line, span or aggregate in any
      property, at any depth (constitution IV). Pointers instead.
- [ ] Every event id is a **pure function of the payload** — a source-native identifier, a
      window key — never a clock reading, a counter or a random source (constitution III).
- [ ] Every assertion carries `ValidAt` **or** `ValidFromUnknown`, never a guessed timestamp
      and never both (FR-011).
- [ ] An `identity_claim` is emitted for **every** external identifier the source knows about an
      entity, including the one the feeder addresses it by — and the connector **merges nothing**
      itself (constitution VI).
- [ ] A `source_checkpoint` is emitted on start, on resync and after any gap, with `gap_before`
      set truthfully (FR-052).
- [ ] Property names are OpenTelemetry semantic conventions where one exists, `sre.*` otherwise.
      A connector that invents its own spelling has written a fact no resolution rule can read.
- [ ] `WeightClass` is set on `calls` edges and nil on every other edge type.

## 4. Read-only, and proving it (constitution VII, FR-046)

- [ ] `Description.RequiredScopes` documents the exact read-only permissions the connector asks
      the source system for.
- [ ] `Run` verifies at startup that the credential cannot write, and returns an error if it can
      — or the connector's README states, in as many words, that the source system cannot be
      asked and the operator must assert it.
- [ ] No mutating endpoint is called anywhere in the connector, dry-run ones included.
- [ ] For a **REST** source, that last box is mechanical rather than reviewed: the connector declares a
      `feeder.MustReadOnlySurface` whose operations are spelled `"METHOD path"`, every call goes through
      it, and `testkit.AssertSurfaceMatchesPage` compares it against `docs/connectors/<name>.md` in both
      directions. The method is the write test, so a planted `POST` fails at program start. (A source
      whose operations are not method-and-path — a cloud IAM permission set, say — carries its own
      equivalent; `internal/gcpx` is the worked example.)
- [ ] The graph-side credential is a **feeder-role token scoped to this connector's `SourceID`**,
      and nothing in the code, the logs, the flags' `--help` defaults or the fixtures contains a
      token value. See [../security.md](../security.md).

## 5. Observability and operations

- [ ] Rejections are visible: counted, logged at warning with their reason code, and surfaced to
      the operator. A connector whose events are all being refused must be diagnosable from its
      own logs (FR-024).
- [ ] The connector emits OpenTelemetry spans and metrics about its own work; a component with no
      instrumentation is not release-ready (constitution, Development Workflow).
- [ ] Live and recorded modes run **the same `Run`**. If they diverge, the recorded mode has
      stopped being the test (FR-044).

## 6. The paperwork

- [ ] SPDX header (`// SPDX-License-Identifier: Apache-2.0`) on every new file.
- [ ] A README for the connector: what it reads, which scopes, which namespaces it mints,
      which node/edge/change types it emits, and its own semver.
- [ ] `docs/connectors/` updated if the connector teaches the SDK something new.
- [ ] `CODEOWNERS` entry for the new directory.
- [ ] DCO sign-off on every commit (`git commit -s`), per [CONTRIBUTING.md](../../CONTRIBUTING.md).
- [ ] Any change to a published schema carries an ADR under `docs/decisions/`, a version bump and
      a passing `buf breaking` (constitution IX).

## 7. Worker (`pkg/worker`)

- [ ] `Describe()` is a pure function and passes `Description.Validate`: one source of truth, no
      capability with `ReadOnly = false`, exactly one published cost class per capability, both
      modes declared, a versioned redaction policy, a version.
- [ ] `ContainsModel` is **declared with its `ModelID`** if a model touches the answer anywhere —
      including a labelling pass over already-reduced data — and the digest states what the model
      added. A worker that declares no model has a test that asserts it.
- [ ] Every capability name is a published algebra term. A request outside the algebra is refused
      `outside_algebra`, naming what was asked and what is available, and the refusal is recorded.
- [ ] Responses are built through `NewResponse` and nowhere else, so every one carries a coverage
      block, the published ordering, the caps and a term key.
- [ ] Failures, timeouts and empty results are **evidence items with reasons**, never silences;
      `NO_DATA` is used only for "the window was covered and held nothing".
- [ ] If the worker uses an `After` hook: it re-orders nothing, and every field it writes that the
      backend also writes comes from the **same exported rule** the backend calls.
- [ ] `pkg/worker/testkit.Conformance` runs with a fixture — `Declaration`, `Golden` and
      `ModesAgree` — and **fails rather than skips**. Recorded responses are the test.

## 8. Telemetry backend (`pkg/backend`)

- [ ] Exactly one vendor, telemetry-family terms only, one published cost class per term, a
      versioned redaction policy and an algebra version; `Description.Validate` passes.
- [ ] Every digest carries a coverage block; fields that cannot be determined are **stated as
      undetermined**, never omitted.
- [ ] The **horizon** is enforced: a straddling window is clamped and annotated
      (`truncated_to_horizon`, `horizon`, the `truncation` criterion), a wholly-future window is
      `NO_DATA` naming the horizon, and a request with no horizon is left alone.
- [ ] `NormaliseForDigest` is used before hashing — `response_digest`, `duration_ms` and `mode`
      cleared — so live and recorded produce identical digests for the same term.
- [ ] `onset` is computed **backend-side** with the shipped estimator; no raw sample crosses the
      boundary in either direction.
- [ ] `exemplars` and `drill_down` accept only handles the backend itself minted; drill-downs are
      recorded to depth 1.
- [ ] A term whose data source the organisation does not have answers `NO_DATA` with the absent
      source named — never `QUERY_FAILED`, never a substituted source.
- [ ] `NOT_RECORDED` is never reported as `NO_DATA`: a gap in the recording is not a fact about
      production. Recorded mode makes no live call to satisfy a miss.
- [ ] The recorded world's miss rate is at or under **5 %**, which gates the fixture, not the
      engine.
- [ ] `pkg/backend/testkit.Conformance` runs with a fixture and **fails rather than skips**.
