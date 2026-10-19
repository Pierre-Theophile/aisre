# ADR-0016: Roadmap after the Sazabi competitive review

- Status: **Accepted** (owner decision 2026-10-19: "I let you adapt the road map")
- Deciders: project owner (the go-ahead), Claude (the order and scope, documented here)
- Source: [`docs/reviews/2026-10-19-competitor-sazabi.md`](../reviews/2026-10-19-competitor-sazabi.md).
  Follows ADR-0004, which set the order up to feature 005.

## Context

Sazabi (YC Spring 2026, $8M seed in June 2026) is a live, paid, closed-source SaaS. Its pitch is
"AI-native observability", and it works in four steps:

1. It ingests and stores the customer's logs, with metrics and traces normalized into logs.
2. It detects issues without monitors.
3. It investigates each issue against the customer's GitHub code in a sandbox.
4. It posts the cause to Slack and can open pull requests.

Coding agents use it through an MCP server and a CLI. Its buyer is a small team with no SRE and
no observability contract worth protecting.

It overlaps with us on the investigator, Slack, GitHub, GCP, Vercel and Notion. It differs on
every point that the constitution makes ours:

- It replaces observability; we read the backends a team already runs (Principle on
  interchangeable backends).
- Its substrate is logs; ours is the bitemporal graph of topology and changes.
- It is SaaS only; we are Apache-2.0 with every dependency pluggable and self-hostable.
- It opens PRs; we propose and never act (Principle VII).

The review found two places where it is ahead and we can catch up without leaving the thesis:

- It is usable from a coding agent.
- It starts work without anyone declaring an incident.

It also found a third gap that is ours alone: we have no live installation, and every recording
campaign is waiting on credentials and a signatory.

## Decisions

### D1. Positioning: an open change-and-topology substrate plus an investigator, on top of the stack you run
We are not an observability product and do not store telemetry. The README says so in one
line, contrasting with logs-first tools. This restates the thesis and changes no principle.

### D2. Feature 009 finishes first
Its PR 2 (discovery, rules, evaluation) and PR 3 (the pluggable classifier) stay next. The
catalog is what makes attribution and knowledge scoping work on real data, and every later item
reads it.

### D3. Feature 010 is a read-only MCP server
It exposes the query algebra (`subgraph`, `diff`, `impact`, `history`, `pointers`, `extent`),
`resolve why`, the catalog, and investigation start, status and reports as MCP tools. The
servers it serves are Claude Code, Cursor, Codex and any other MCP client, and it uses a
self-hosted transport (stdio and streamable HTTP).

Its boundaries:

- **Authentication:** every call names an individual, as for the API (001 authentication
  from version one). A token is scoped the way API tokens are.
- **Writes:** it writes nothing outside the tool's own stores. Starting an investigation is a
  write to our own store, as `aisre investigate` already is. The fix is written by the user's
  coding agent, with our evidence attached, so Principle VII stands unchanged and no
  amendment is needed.
- **Reuse:** it is a thin adapter over the existing gRPC/HTTP API and adds no new query
  semantics.

Why now: this is the cheapest way to meet coding agents where they already work, and the
graph's `diff` and `impact` answers are what a coding agent cannot reconstruct from logs.

### D4. Feature 011 is change-watch intake: investigations the graph starts itself
The graph sees changes as they happen: deploys (004), Cloud Run revisions (003), vendor notices
(003) and config. Change-watch is a new intake transport. When a change node is followed,
within a window, by a shift on a telemetry pointer for the services in its `impact`, it emits
the same `alert.transition` 002 already accepts, with `transport = change_watch`. Telemetry
pointers come from 003 and 005. Shifts are compared against the change's own before-window.

It is an alert source and not an anomaly-detection product:

- it watches only what follows a change;
- it reuses the existing backends' metrics and error counts;
- it never stores telemetry;
- its threshold and window are operator configuration, with a published default fitted on
  the fixture corpus.

An investigation it starts is labelled as change-triggered, so a team can mute the transport
without muting monitors.

### D5. A live installation is a roadmap item, not a footnote
The open owner-gated tasks across 003, 004, 006, 007 and 009 are worth more together than any
new connector. The first is one end-to-end live install on the owner's estate, recorded and
signed like the private campaigns. It covers GCP and Vercel feeders, Slack intake and report,
Notion knowledge, and the catalog built with the owner. It is scheduled alongside D2, ahead of
D3 and D4 for the owner's time, but it does not block their code.

### D6. Setup in minutes, after D3
Sazabi installs with two apps and a log forwarder. We need a `deploy/docker-compose.yml` path
that brings up the agent, Postgres and the connectors from one configuration file, with
`--check` on every connector. That path becomes the documented first run. It is scoped once
010 exists, because the MCP server is the first thing a new user will try.

### D7. Reaffirmed non-goals
- **No log storage and no anomaly detection over raw logs.** That is the logs-first product,
  and it would contradict the constitution's ban on RAG over telemetry and the
  bring-your-own-backend principle.
- **No autonomous pull requests.** Change-watch (D4) starts investigations; it never writes to
  production or to a repository.
- **No hosted service.** The constitution keeps hosted-service concerns out of scope.

## Consequences

- The README status table lists 009 to 011 and corrects 005's row, which still said "specified;
  not yet planned" although the connector shipped and its quickstart ran on 2026-09-27.
- `specs/010-mcp-server` and `specs/011-change-watch` are specified with the usual owner
  approval of the spec and its defaults before any plan or code. This ADR sets their order and
  boundaries, not their requirements.
- Order of implementation: finish 009 (PR 2, PR 3) while the live installation proceeds as the
  owner's time allows, then 010, then 011, then the setup path (D6).
