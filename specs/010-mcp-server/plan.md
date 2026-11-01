# Implementation Plan: Read-only MCP Server for Coding Agents

**Branch**: `010-mcp-server` | **Date**: 2026-11-01 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/010-mcp-server/spec.md`, approved by the owner on
2026-11-01 with all eight defaults (Q1–Q8), and constitution 1.2.0.

## Summary

`aisre mcp` is a new subcommand that serves the Model Context Protocol over stdio or streamable HTTP. It
is a **client of the existing Connect API** and nothing else: every tool builds the same protobuf
request the CLI builds, calls the same RPC, and renders the answer with the same canonical JSON
(`graph.CanonicalJSON`) the CLI's `--output json` and the fixture goldens use. Parity is therefore
tested against the goldens that already exist, not against a second rendering.

The adapter lives in a new package, `internal/mcpserver`, that depends only on the generated Connect
client interfaces, `internal/graph` (canonical JSON), `internal/injection` (the published detector) and
the MCP Go SDK. An archtest rule forbids it from linking the store, the projector, the query engine or
the server. The SDK is the one the T093 spike already vetted (`github.com/modelcontextprotocol/go-sdk`
v1.8.0, Apache-2.0/MIT); it now becomes a dependency of the main module (research R1).

One additive RPC is needed: `graph.v1.IdentityService.WhoAmI`, which returns the caller's subject and
roles, so that the server can list only the tools the caller may call (FR-013) and refuse a feeder token
(FR-014) before any query. It adds no query semantics; the API stays the authorisation point (FR-015).

## Phase 0 — gaps found against the code

| # | gap | where | resolution |
|---|---|---|---|
| G1 | No way for a client to learn its own roles: the API only refuses after the fact | `internal/server/auth.go` (`Principal`), no RPC | `IdentityService.WhoAmI` (R3) |
| G2 | The CLI builds requests inside `cobra` option methods; nothing reusable outside `internal/cli` | `internal/cli/query_*.go` | `internal/mcpserver` builds requests from typed tool inputs; parity is asserted against the goldens (R4) |
| G3 | The main module does not link an MCP SDK; only `examples/mcp-feeder` does, on purpose | `examples/mcp-feeder/go.mod`, `docs/connectors/mcp.md` | adopt the same SDK and version in the main module (R1) |
| G4 | An investigation is a server stream; a tool call must return at once | `InvestigationService.Investigate` | read the stream's first message, keep the stream draining in the background, bounded per caller (R5) |
| G5 | The injection detector reports on investigator inputs only | `internal/injection` | run it over every string of every answer, report in `_meta` (R6) |

## Technical Context

**Language/Version**: Go 1.27 (repository floor).

**Primary Dependencies**: `github.com/modelcontextprotocol/go-sdk` v1.8.0 (`mcp` package: `Server`,
`AddTool`, `StdioTransport`, `StreamableHTTPHandler`); `connectrpc.com/connect` (existing clients).

**Storage**: none. The server holds no state beyond the running-investigation counters (R5).

**Testing**: `go test` with the in-memory MCP transports against an in-process `serve` on a loaded
fixture (the `queryServer` pattern of `internal/cli/query_test.go`); goldens under `fixtures/*/golden`.

**Target Platform**: the developer's machine (stdio) or beside `serve` (HTTP), Linux/macOS.

**Project Type**: CLI subcommand of the existing binary.

**Performance Goals**: ≤ 50 ms p95 added per call (SC-004); measured by a benchmark over the corpus.

**Constraints**: read-only except `investigation_start` (FR-025); no credential held in HTTP mode
(FR-012); TLS required off loopback (FR-003).

**Scale/Scope**: 13 tools, 2 resources, 2 profiles.

## Constitution Check

| Principle | How this plan complies |
|---|---|
| I. Graph First | every answer is a graph query already defined by 001/009; nothing is computed outside it |
| II. Bitemporal or Nothing | every graph tool takes `valid_at`/`observed_at` and the answer carries them (FR-031) |
| III. Event-Sourced Ingestion | no ingestion path is exposed (FR-025) |
| IV. Telemetry Stays in Its Backend | the server stores nothing; pointers are returned as pointers |
| V. Evidence-First | investigation answers are returned whole, with citations and the examination report |
| VI. Entity Resolution Is Auditable | `resolve_why` exposes the audit; no decision can be made over MCP |
| VII. Read-Only by Default | one write, to aisre's own store, as `aisre investigate` (ADR-0016 D3); asserted by SC-002's test |
| VIII. Evaluation From Day One | parity against existing goldens; the `baseline` profile feeds 002 T131 |
| IX. Open Schema | `WhoAmI` is additive; `buf breaking` passes |
| X. Simplicity | a thin adapter, one SDK already vetted, no new store |

No violation; Complexity Tracking is empty.

## Project Structure

### Documentation (this feature)

```text
specs/010-mcp-server/
├── spec.md
├── plan.md               # this file
├── research.md           # R1–R8
├── contracts/mcp-tools.md
├── quickstart.md
├── checklists/requirements.md
└── tasks.md
```

### Source Code (repository root)

```text
api/sreagent/graph/v1/graph.proto        # + IdentityService.WhoAmI
internal/server/identity.go              # WhoAmI handler
internal/mcpserver/
├── server.go        # New(Options): builds an *mcp.Server for a caller's roles and a profile
├── tools_graph.go   # subgraph, diff, impact, history, pointers, extent, resolve_why
├── tools_catalog.go # catalog_list, catalog_show, attribution_why
├── tools_invest.go  # investigation_list, investigation_get, investigation_start
├── resources.go     # aisre://guide, aisre://catalog
├── answer.go        # canonical JSON, summary, _meta (as_of, partial, injection)
├── limits.go        # per-caller running-investigation bound
├── http.go          # streamable HTTP handler, bearer forwarding, TLS rule
└── *_test.go
internal/cli/mcp.go                      # `aisre mcp [--http addr] [--profile] [--max-running]`
internal/archtest/archtest_test.go       # + rule for internal/mcpserver
docs/mcp.md                              # client setup (Claude Code, Cursor, Codex)
```

## Delivery

Two pull requests, each merged when green:

1. **Spec, plan, tasks**.
2. **The server** (US1–US5): `WhoAmI`, the package, every tool and resource, stdio and streamable
   HTTP, the baseline profile, archtest rule, parity and security tests, docs, benchmark and quickstart
   run. The tasks keep the two-phase grouping they were planned in; both phases ship together because
   the adapter is small. 002 T131 follows.

## Design decisions (detail in research.md)

- R1 SDK: the official Go SDK at the spike's version; typed tool inputs give JSON Schemas for free.
- R2 Transports: stdio for local agents; streamable HTTP for shared use, refusing plain HTTP off loopback.
- R3 Identity: `WhoAmI` per stdio session start and per HTTP session; tool calls still forward the token.
- R4 Parity: tool inputs map 1:1 onto request fields with the CLI's defaults; tests compare with goldens.
- R5 Investigations: first stream message returned; background drain; per-subject bound per instance.
- R6 Untrusted text: detector over every string in the answer; matches listed in `_meta.injection`.
- R7 Paging: the API's own caps and cursors; `_meta.partial` mirrors `truncation`/`next_cursor`.
- R8 Baseline profile: `baseline` lists graph and catalog tools only; enforced at registration.

## Complexity Tracking

None.
