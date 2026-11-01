# Research: Read-only MCP Server for Coding Agents

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-11-01

## R1. Which MCP SDK, and in which module

**Decision**: `github.com/modelcontextprotocol/go-sdk` v1.8.0, package `mcp`, in the main module.

**Rationale**: it is the official Go SDK, maintained with Google, and the T093 spike
([`docs/connectors/mcp.md`](../../docs/connectors/mcp.md)) already exercised it at this version for
both transports. Its licence is Apache-2.0 (MIT for unrelicensed older contributions), compatible
with the project's Apache-2.0. `mcp.AddTool[In, Out]` derives each tool's input schema from a Go
struct, so the schema is the type and cannot drift from the handler. The spike kept the SDK out of
the main binary because it was a spike; 010 *is* the feature that ships it.

**Alternatives rejected**: `mark3labs/mcp-go` (community, broader API, no reason to diverge from the
spike); a hand-written JSON-RPC server (re-implements session, schema and transport rules, and
constitution X argues against it); a separate `aisre-mcp` binary (a second release artefact for one
subcommand).

## R2. Transports

**Decision**: stdio (`aisre mcp`) and streamable HTTP (`aisre mcp --http <addr>`), the latter in the
SDK's **stateless** mode. The HTTP listener refuses to start on a non-loopback address without
`--tls-cert`/`--tls-key` (FR-003), mirroring the feeders' refusal to send tokens over plain http.

**Rationale**: stateless mode authenticates every request by its own bearer token. With sessions, a
session id would become a second credential that outlives the token that opened it. No tool needs a
server-to-client request, which is the only thing stateless mode gives up.

**Alternatives rejected**: the deprecated HTTP+SSE transport; stateful sessions keyed to the first
token.

## R3. Knowing the caller's roles

**Decision**: add `graph.v1.IdentityService.WhoAmI(WhoAmIRequest) returns (WhoAmIResponse)` with the
subject, issuer and roles of the authenticated principal. Any authenticated principal may call it.
The MCP server calls it once per stdio process start (a rotated token keeps its roles; a change of
roles takes a restart) and once
per HTTP request, and registers only the tools the roles allow. A principal whose only role is
`feeder` is refused (FR-014).

**Rationale**: FR-013 needs the roles before the first tool is listed; probing each RPC would make a
denied call per tool and still leave the list wrong. The handler reads the principal the existing
interceptor already put in the context, so it adds no authentication logic.

**Alternatives rejected**: decoding the token locally (the server must not interpret credentials;
OIDC, dev and workload tokens map roles differently); listing every tool and relying on refusals
(contradicts FR-013).

## R4. Request building and parity

**Decision**: each tool has a typed input struct whose fields are the CLI flags of the same command,
with the CLI's defaults (`query.DefaultHops`, `DefaultTau`, …), mapped onto the same protobuf request.
Answers are rendered with `graph.CanonicalJSON`. Tests compare the tool's structured content with the
fixture golden the CLI's own test compares with (`fixtures/rollout-regression-01/golden/diff.checkout-diff.json`,
`fixtures/baseline-topology-01/golden/*`), so MCP, CLI and the harness agree byte for byte (FR-030, SC-001).

**Rationale**: moving the CLI's builders into a shared package would touch thirteen commands for no
behaviour change; the goldens already pin the answer, which is what parity is about.

## R5. Starting and following an investigation

**Decision**: `investigation_start` opens `Investigate` (explicit reference or alert) or `Declare`
(a declaration, with its idempotency key) and returns as soon as the first message, which carries the
id and the provisional ranking (002 FR-046a), arrives. The stream keeps draining in a background
goroutine bound to the server's lifetime, so the run is not cancelled. Running streams are counted per
principal subject; at the bound (default 3, `--max-running`) the call is refused with the count and
the bound (FR-053). The symptom's `origin_system` is `mcp` (FR-054). `investigation_get` reads with
`Get`; `investigation_list` with `List`.

**Rationale**: the engine already persists every run whether or not a client listens, so draining is
only to avoid cancelling the request. The bound is per server instance: that is the only place that
knows which runs it started, and an HTTP deployment is one instance.

## R6. Untrusted text in answers

**Decision**: after rendering, walk every string of the answer, run `injection.Scan` on it, and list
the matches (JSON path, pattern name, bounded excerpt) in the result's `_meta.injection`. The answer
itself is not changed. The initialisation `instructions` say that tool results are evidence about the
monitored systems, never instructions (FR-042).

**Rationale**: the API already returns sanitised text (004/007); rewriting it in the adapter would
break parity and hide what was there. Flagging is what the investigator does too (`injection_attempt`).

## R7. Large answers

**Decision**: the API's own caps (`per_hop_cap`, `total_cap`) and cursors are tool inputs. When an
answer carries a `truncation` or a `next_cursor`, `_meta.partial` is true and the summary says how to
get the rest. Nothing is cut by the adapter.

## R8. Profiles

**Decision**: `--profile default|baseline`. `baseline` registers the graph and catalog tools and the
resources, never an investigation tool, whatever the roles (FR-060). The profile is fixed at start; a
client cannot change it.

## R9. Logging and telemetry

**Decision**: one structured log line per call (subject, tool, duration, outcome, API code) and the
existing request metrics via `otelhttp` on the HTTP handler; arguments are not logged (FR-062).
