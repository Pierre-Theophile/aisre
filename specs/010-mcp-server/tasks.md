---

description: "Task list for feature 010 — read-only MCP server for coding agents"
---

# Tasks: Read-only MCP Server for Coding Agents

**Input**: Design documents from `/specs/010-mcp-server/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[contracts/mcp-tools.md](./contracts/mcp-tools.md), [quickstart.md](./quickstart.md)

**Tests**: **Required, not optional.** Constitution VIII: every tool is tested against a loaded fixture,
and the graph tools against the goldens the CLI is already tested against.

**Organization**: grouped by the plan's pull requests. PR 2 delivers US1 and US5 over stdio; PR 3 adds
the investigation tools, streamable HTTP and the baseline profile (US2, US3, US4).

**Status**: spec approved by the owner on 2026-11-01 (all eight defaults, "Go"); in implementation.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US5]`. Setup, Foundational and Polish tasks carry no story label.
- Every task names the fixture it validates against; `unit` means a Go test in the package named.

---

# PR 2 — graph and catalog over stdio

## Phase 1: Setup

- [ ] T001 Add `github.com/modelcontextprotocol/go-sdk` v1.8.0 to the main `go.mod`, and record it in `NOTICE` with its licence (research R1); fixture: unit (`go mod verify`, CI lint)
- [ ] T002 Add `IdentityService.WhoAmI` to `api/sreagent/graph/v1/graph.proto` per contracts §Identity RPC, regenerate with `buf generate`; `buf lint` and `buf breaking` pass (research R3); fixture: unit (CI schema job)
- [ ] T003 Write `internal/server/identity.go` and mount it in `internal/server/server.go`: any authenticated principal, returns subject, issuer and roles; unauthenticated is refused (research R3); fixture: unit

## Phase 2: Foundational

- [ ] T004 Write `internal/mcpserver/server.go`: `New(Options)` building an `*mcp.Server` for a set of roles and a profile, with `instructions` per FR-042 and name/version/algebra version per FR-004; fixture: unit
- [ ] T005 Write `internal/mcpserver/answer.go`: canonical JSON structured content, text summary, `_meta` `aisre.as_of`, `aisre.partial`/`aisre.continuation` (research R7), API errors as tool errors with the Connect code (FR-034); fixture: unit
- [ ] T006 Add the `internal/mcpserver` rule to `internal/archtest/archtest_test.go`: it must not link the infrastructure list, `internal/server`, `internal/query`, `internal/projector` or `internal/store` (FR-002); fixture: unit (archtest)

## Phase 3: User Story 1 — a coding agent asks what changed (P1)

- [ ] T007 [US1] Write `internal/mcpserver/tools_graph.go`: `subgraph`, `diff`, `impact`, `history`, `pointers`, `extent`, `resolve_why` with the CLI's inputs and defaults (contracts §Tools, research R4); fixture: rollout-regression-01, baseline-topology-01
- [ ] T008 [US1] Write `internal/mcpserver/tools_catalog.go`: `catalog_list`, `catalog_show`, `attribution_why`; fixture: attribution fixtures under `fixtures/attribution`
- [ ] T009 [US1] Write `internal/mcpserver/resources.go`: `aisre://algebra` (embedded `docs/schema/algebra.md`) and `aisre://catalog` (FR-028, FR-029); fixture: unit
- [ ] T010 [US1] Write `internal/cli/mcp.go`: `aisre mcp` over stdio with the global `--server`, `--token`, `--token-file`; calls `WhoAmI` at start, refuses a feeder-only principal (FR-011, FR-014); fixture: unit
- [ ] T011 [US1] Parity test in `internal/mcpserver/parity_test.go`: every graph tool's structured content equals the fixture golden the CLI test compares with (SC-001); fixture: rollout-regression-01, baseline-topology-01
- [ ] T012 [US1] Tool-list test: every tool has a description, an input schema and the read-only annotation; a reader sees the reader tools, a token without roles sees none, a call to an unlisted tool is refused (FR-013, FR-026, FR-027, SC-002, SC-003); fixture: unit

## Phase 4: User Story 5 — injected text is flagged (P2)

- [ ] T013 [US5] Run `injection.Scan` over every string of every answer and list matches in `_meta.aisre.injection`, leaving the answer unchanged (FR-040, FR-041, research R6); fixture: injection-01
- [ ] T014 [US5] Test that no answer carries a credential or secret reference value (FR-043); fixture: injection-01, slack-chat-injection-01

## Phase 5: Polish (PR 2)

- [ ] T015 [P] Write `docs/mcp.md` with Claude Code and Cursor/Codex setup and the tool table; link it from `README.md` and `docs/connectors/mcp.md` (FR-063); fixture: n/a
- [ ] T016 Per-call log line and metrics (FR-062, research R9); fixture: unit

---

# PR 3 — investigations, HTTP and the baseline profile

## Phase 6: User Story 2 — read the investigator's verdict (P1)

- [ ] T017 [US2] Write `investigation_list` and `investigation_get` in `internal/mcpserver/tools_invest.go`, provisional runs marked as such; parity with `aisre investigate get --output json`; fixture: rollout-regression-01-incident

## Phase 7: User Story 3 — start an investigation (P2)

- [ ] T018 [US3] Write `investigation_start`: explicit reference, alert or declaration (with idempotency key), `origin_system = mcp`, returns on the first stream message and drains in the background (FR-050–FR-052, FR-054, research R5); fixture: rollout-regression-01-incident, declared-incident-01
- [ ] T019 [US3] Write `internal/mcpserver/limits.go`: running investigations per subject, default 3, `--max-running`; refusal says the count and the bound (FR-053); fixture: unit

## Phase 8: User Story 4 — the evaluation baseline (P2)

- [ ] T020 [US4] Add `--profile baseline` registering only graph and catalog tools (FR-060, research R8); test the listed set; fixture: unit
- [ ] T021 [US4] Expose a Go entry point the evaluation harness can call to serve a recorded world under the `baseline` profile over in-memory transports, so the 002 simple-harness baseline can drive a model through it (FR-061); fixture: rollout-regression-01-incident

## Phase 9: Streamable HTTP

- [ ] T022 Write `internal/mcpserver/http.go` and `aisre mcp --http`: stateless streamable HTTP, bearer from each request forwarded to the API and never stored, `WhoAmI` per request, 401 without a token, refusal to start off loopback without TLS (FR-003, FR-012, research R2); fixture: unit
- [ ] T023 Security test: a request's token is never reused for another request, never logged, and a feeder token gets 403 (SC-003); fixture: unit

## Phase 10: Polish (PR 3)

- [ ] T024 Benchmark the added latency per tool over the corpus and record it in `docs/benchmarks/` (SC-004); fixture: rollout-regression-01, baseline-topology-01
- [ ] T025 Run `quickstart.md` end to end and record the run in `specs/010-mcp-server/quickstart-run-2026-11-01.md` (SC-005); fixture: rollout-regression-01
- [ ] T026 Update the README status row and 002's simple-harness task to point at the baseline entry point; fixture: n/a
