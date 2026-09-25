# Deploying and running sre-agent locally

`sre-agent` is a single static binary plus one external service: PostgreSQL 16 or newer. This
directory holds what you need to get that service up on a laptop or in CI. It is not a
production deployment.

## Contents

| Path | What it is |
|------|------------|
| `docker-compose.yml` | PostgreSQL 16 for development, tests and the quickstart |
| `kind/` | the disposable Kubernetes cluster: kind config, two demo overlays, the scripted incidents. See [`kind/README.md`](./kind/README.md) |

## Start PostgreSQL

```sh
docker compose -f deploy/docker-compose.yml up -d postgres
docker compose -f deploy/docker-compose.yml ps        # wait for "healthy"
```

The container is called `sre-agent-postgres` and exposes 5432 on the host. Credentials and
database name are all `sreagent`.

## The connection string

Everything — the binary, the test suite, the fixture harness — reads `PG_DSN`:

```sh
export PG_DSN='postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable'
```

Then:

```sh
make build
bin/aisre migrate --db "$PG_DSN"
bin/aisre serve --db "$PG_DSN" --listen :8080 --auth dev --dev
```

`--auth dev` is a local identity provider for fixtures, CI and laptops. It refuses to start
without `--dev` and prints a banner when it runs. Never expose a `--dev` server.

## Why PostgreSQL 16

The graph stores bitemporal versions as rows carrying two `tstzrange` columns — a valid
interval and an observed interval — indexed with GiST and guarded by exclusion constraints.
Range types, the `@>` and `&&` operators and their indexes are what make "as it was true at
T, as we knew it at T′" a single indexed predicate rather than a scan. Version 16 is the floor
we test against; newer is fine. See `specs/001-temporal-graph-core/research.md` §2 and §3 for
the storage comparison this follows from, and `docs/decisions/ADR-0002-language-storage-api.md`
for the decision itself.

Two logical areas live in the one database:

- `log` — the append-only event log, rejected events, duplicate deliveries, source
  checkpoints. The application role holds `INSERT` and `SELECT` here and nothing else.
- `graph` — the materialized bitemporal entity and edge versions, identity claims, resolution
  decisions. History is never deleted; the only permitted mutation is closing an open observed
  interval, through a `SECURITY DEFINER` function.

## Database roles: least privilege per process

The compose role `sreagent` owns everything, which is right for a laptop and wrong for anything
else. Three processes touch this database and they need three different things (constitution
VII, FR-035, FR-046):

| Process | Needs | Must not have |
|---|---|---|
| `migrate` | DDL on `log` and `graph` | anything at runtime — it is run once, by a human or a job |
| `serve` | `SELECT` everywhere, `INSERT` on `log.*` and `graph.*`, `UPDATE` on the observed-interval close | `DELETE`, `TRUNCATE`, `DROP`, and ownership |
| a query-only deployment | `SELECT` on `graph.*` and `log.*` | every write, so that "read-only credentials suffice for every query" (FR-035) is a property of the database and not only of the code |

Feeders never hold a database credential at all. A feeder talks to `serve` over ConnectRPC with
a feeder-role bearer token scoped to its own `source_id`; it has no DSN, and a compromised
feeder therefore cannot reach the database at any privilege. That is the whole point of putting
ingestion behind the API (see [`../docs/security.md`](../docs/security.md)).

### The read-only role for a query-only deployment

An agent, a dashboard, a notebook or a second `serve` process that only answers queries gets
this role. It can read every version and every event, and it can write nothing at all — not
even a decision, because recording a resolution decision is a write (FR-040) and a query-only
deployment does not make decisions.

```sql
-- One role, no login of its own; grant it to the humans and services that need it.
CREATE ROLE sreagent_readonly NOLOGIN;

-- Nothing is readable by default, including future tables.
REVOKE ALL ON SCHEMA graph, log FROM PUBLIC;
GRANT USAGE ON SCHEMA graph, log TO sreagent_readonly;

GRANT SELECT ON ALL TABLES IN SCHEMA graph TO sreagent_readonly;
GRANT SELECT ON ALL TABLES IN SCHEMA log   TO sreagent_readonly;

-- A migration that adds a table must not silently add a table this role cannot read, and must
-- not silently grant it more than SELECT.
ALTER DEFAULT PRIVILEGES IN SCHEMA graph GRANT SELECT ON TABLES TO sreagent_readonly;
ALTER DEFAULT PRIVILEGES IN SCHEMA log   GRANT SELECT ON TABLES TO sreagent_readonly;

-- Explicitly not granted, and worth writing down so a later reader sees the intent:
--   no INSERT, UPDATE, DELETE, TRUNCATE on any table
--   no USAGE on any sequence (an INSERT would need one)
--   no EXECUTE on the observed-interval close function
--   no CREATE on either schema

-- The login the query-only process actually uses.
CREATE ROLE sreagent_query LOGIN PASSWORD 'replace-me';
GRANT sreagent_readonly TO sreagent_query;
```

Its DSN, for `--db` or `$PG_DSN`:

```sh
export PG_DSN='postgres://sreagent_query:replace-me@db.internal:5432/sreagent?sslmode=require'
```

Prove it is read-only rather than assume it — one statement, and it must fail:

```sh
psql "$PG_DSN" -c "insert into log.sources (source_id, kind) values ('probe','probe')"
# ERROR:  permission denied for table sources
```

`aisre serve` started with that DSN comes up, answers `/healthz`, answers every query, and
fails any ingestion or resolution call at the database rather than at the API. That is a
deliberate second line: the API already refuses a reader token that tries to ingest, and the
grant means a bug in that check is still not a write.

### The serve role

```sql
CREATE ROLE sreagent_app LOGIN PASSWORD 'replace-me';
GRANT USAGE ON SCHEMA graph, log TO sreagent_app;

GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA log   TO sreagent_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA graph TO sreagent_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA log, graph TO sreagent_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA log   GRANT SELECT, INSERT ON TABLES TO sreagent_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA graph GRANT SELECT, INSERT, UPDATE ON TABLES TO sreagent_app;
```

No `DELETE` and no `TRUNCATE`, anywhere, ever. History is never deleted (constitution II), so
the application role that could delete it is a role that could violate the constitution by
accident. `UPDATE` on `graph` is needed only to close an open observed interval — a correction
opens a new row and closes the old one's observed interval, it never overwrites a fact.

The migration role is separate again and is not the role `serve` runs as: a process that can
change the schema it is reading is a process that can migrate a database an older binary then
cannot read, which is also why `serve` does not migrate on startup unless asked.

> The compose setup deliberately uses one superuser-ish role: `fixture verify` creates and drops
> a database per fixture, which needs `CREATEDB`. Do not copy that to a deployment.

## Running the tests

```sh
export PG_DSN='postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable'
make test
```

Store-backed tests need a real PostgreSQL; they use `testcontainers-go` when Docker is
available and fall back to `embedded-postgres` when it is not, so `PG_DSN` is an override
rather than a hard requirement. Each test run works in its own schema and drops it at the
end, so pointing `PG_DSN` at the compose instance is safe.

## Resetting

The data lives in the named volume `sre-agent-postgres-data`, so it survives
`docker compose down`. To start from nothing:

```sh
docker compose -f deploy/docker-compose.yml down -v
```

This deletes the event log. There is no other way to remove history — that is the point.

## No Docker?

Install PostgreSQL 16 yourself (`brew install postgresql@16`, or your distribution's
package), create the role and database, and set `PG_DSN` accordingly:

```sh
createuser --createdb sreagent
createdb --owner sreagent sreagent
psql -d postgres -c "alter role sreagent with password 'sreagent'"
```

## The disposable Kubernetes cluster

Everything above is about the one service `sre-agent` needs to run. `kind/` is about the
opposite end: a real Kubernetes cluster, thrown away afterwards, that the two reference feeders
can be pointed at so the graph they build can be checked against a graph replayed from
recordings of the same run. That is acceptance criterion FR-050 (b) and SC-010, and quickstart
§7 walks through it.

```sh
deploy/kind/up.sh                       # kind cluster + the minimal shop, idempotent
bin/aisre feed otel --source-id=otel:kind --listen-grpc=:4317 --listen-http=:4318 \
                        --window=5m --record=/tmp/live/otel &
bin/aisre feed k8s  --source-id=k8s:kind --context=kind-sre-agent-demo \
                        --owner-labels=team --record=/tmp/live/k8s &
deploy/kind/scenarios/rollout.sh        # the change the fixtures rank
deploy/kind/down.sh
```

Two overlays: `otel-demo/minimal` is a self-contained synthetic shop, shaped like
`fixtures/baseline-topology-01`, with spans produced by telemetrygen — this is what the fixtures
are recorded from. `otel-demo/full` is the real OpenTelemetry Demo with its collector teed to
sre-agent, for checking the feeder against real-world span shapes; its upstream manifest is
fetched and checksum-pinned rather than vendored.

The part worth reading before you start is how pods reach a feeder listening on your host —
`host.docker.internal` on Docker Desktop, the `kind` bridge gateway on Linux, neither of them
resolvable from inside a pod, so the address is resolved on the host and handed over in a
ConfigMap by `deploy/kind/host-endpoint.sh`.

[`kind/README.md`](./kind/README.md) has all of it: creating the cluster, deploying either
overlay, verifying that spans arrive, recording fixtures, the four scripted incidents
(`rollout`, `config-change`, `feeder-gap`, `scale`), tearing down, and what telemetrygen can and
cannot express.

## Production notes

There is no production deployment here yet. When you build one, the things that matter are:
back up the database like a system of record (it is a map of your production topology, its
owners and its change history), run the API behind your own OIDC issuer rather than
`--auth dev`, and give every feeder read-only credentials toward the system it observes.
`SECURITY.md` covers the rest.
