# Quickstart run, 2026-11-01

**Feature**: [spec.md](./spec.md) | **Quickstart**: [quickstart.md](./quickstart.md) | Task T025, SC-005

Run by Claude in a Linux container with PostgreSQL 16 and the binary built from this branch
(`make build`), against `fixtures/rollout-regression-01`. No MCP client application was available in
the container, so the client side is raw JSON-RPC over the process's stdin and stdout, and `curl`
over HTTP. The end-to-end test `internal/cli/mcp_test.go` drives the same path with the Go SDK's
client. Adding the server to Claude Code itself (`claude mcp add`) is left to the owner's machine.

## Steps and results

1. `aisre migrate`, `aisre serve --auth dev --dev`, `aisre fixture load fixtures/rollout-regression-01`:
   19 migrations applied, fixture loaded.
2. `aisre mcp` over stdio with a `reader,investigator` dev token:
   - `initialize` answered `{"name":"aisre","title":"aisre (algebra 1.1.0)"}` with the instructions.
   - `tools/list` returned the 13 tools.
   - `diff` on `otel.service.name=checkout`, 13:00 to 14:32, 2 hops: summary "4 ranked change(s) …
     Top-ranked: rollout payments to revision 7 (1.4.1) (score 0.610)", `aisre.partial` false,
     `aisre.injection` empty, and the structured content **equal to `golden/diff.checkout-diff.json`**.
   - `investigation_start` on a `serve` with no model configured: a tool error, because that server
     mounts no investigation service (Connect `unimplemented`). The first run printed a bare
     `404 Not Found`; the message now says the server runs no investigation service.
3. `aisre mcp --http 127.0.0.1:18090`:
   - no token: `401`; an unknown token: `401`;
   - with a reader token, `initialize` and `pointers` on `payments` at 14:32 answered ("4 pointer(s)
     in 4 kind(s)").
4. `aisre mcp --http 0.0.0.0:18091` without TLS: refused at start ("refusing to listen on
   0.0.0.0:18091 without TLS").

SC-005 (a new user gets a `diff` answer within 5 minutes from the docs alone) is met on the command
line. It is confirmed in a real MCP client when the owner first adds the server.
