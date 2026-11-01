# ADR-0018: Application ports, and the projector and query engine as Postgres adapters

- Status: **Accepted** (owner decision 2026-11-01: second pass of the Clean Architecture and DDD
  remediation, option "ports and acknowledged adapters")
- Deciders: project owner (the choice of scope), Claude (the design, documented here)
- Builds on: ADR-0002 (protobuf types as the domain model), ADR-0017 (pluggable model boundary)

## Context

The first remediation pass (#55 to #58) added `internal/archtest`, took infrastructure out of the
domain packages, published the model boundary and put the catalog behind ports. Three application
packages still reached storage directly:

- `investigation/runner` took a `*postgres.Store` and called the DAOs of `investigation/store`,
  a package that mixed pure values (incidents, conclusions, calls, human facts, the decision
  record) with their SQL;
- the graph and knowledge workers carried their own adapters over the query engine and the
  event log;
- `server` built its handlers from a `*projector.Projector` and constructed query engines itself.

Two further steps were considered and not taken:

1. **Moving the projector's folding rules into the domain**, leaving SQL alone in an adapter. It
   would make the rules testable without a database and allow a second read model (SQLite for a
   local mode, DuckDB for analytics, XTDB for native bitemporality). It is roughly 14k lines of
   code with a high regression risk, and no second read model is planned.
2. **Replacing protobuf types with domain types.** ADR-0002 chose the generated types on
   purpose, `buf breaking` already guards the contract, and the cost is a near rewrite for no
   user-visible gain.

## Decision

1. **The projector and the query engine are adapters.** `internal/projector` is the Postgres
   write side of the graph (it folds events into bitemporal tables) and `internal/query` its
   Postgres read side. The pure rules they apply live in `internal/graph`, `internal/event` and
   `internal/resolution`. Application packages do not import them.
2. **Application packages declare the ports they use.**
   - `runner.Persistence`: `Investigations`, `Ledgers`, `Lifecycle`, `Evidence`, `DecisionLog`.
     The runner also takes its knowledge corpus as `knowledge.Corpus`.
   - `server.Backends`: `EventLog`, `GraphReader`, `CatalogReader`, `PrincipalRecorder`,
     `Database`.
   - `catalog.Store` and `catalog.EventLog` (from #58).
3. **The values move out of the DAO package.** `internal/investigation/record` holds what an
   investigation writes down, with its validation and its event envelopes. `investigation/store`
   holds only SQL.
4. **Wiring packages join ports to Postgres.** `investigation/pginvestigation`,
   `server/pgserver` and `catalog/pgcatalog` build the ports over the store, the projector and the
   query engine. Only the composition roots (`cmd/`, `internal/cli`, `internal/eval`) import them.
5. **The rules are enforced.** `internal/archtest` forbids the runner, `record`, the investigation
   workers, `server` and `catalog` from reaching pgx, the Postgres store, the event log, the
   projector or the query engine, and fails when a package other than a composition root imports a
   wiring package.

## Consequences

- Every application package can be tested with in-memory implementations of its ports.
- The read side is swappable at the port: a different `GraphReader` can serve the API without
  touching the handlers.
- A second *read model* (one fed from the event log) still needs the projector's folding rules,
  which stay in SQL. If one is ever wanted, the rule to move first is the one being changed:
  extract it into `internal/graph` when it is next modified, so the debt shrinks without a
  dedicated rewrite.
- Protobuf remains the domain vocabulary (ADR-0002, unchanged).
