<!-- SPDX-License-Identifier: Apache-2.0 -->

# aisre for coding agents (MCP)

`aisre mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server. It lets a coding
agent (Claude Code, Cursor, Codex, or any MCP client) ask aisre what a developer fixing an incident
needs: what changed upstream of the failing service, what that change can reach, where its
telemetry lives, which service a name belongs to, and what aisre's investigation concluded.

It reads, and it can start an investigation in aisre's own store. It never changes the systems aisre
observes: the fix is written by your coding agent, with aisre's evidence attached (constitution VII,
[ADR-0016](decisions/ADR-0016-roadmap-after-the-sazabi-review.md) D3). Specification:
[`specs/010-mcp-server`](../specs/010-mcp-server/spec.md).

## Set it up

You need a running aisre API (`aisre serve`) and a token for yourself with the `reader` role, plus
`investigator` if the agent may start investigations.

### Claude Code

```sh
claude mcp add aisre -- aisre mcp --server https://aisre.internal --token-file ~/.config/aisre/token
```

### Cursor, Codex and other stdio clients

Configure a stdio server with the command `aisre` and the arguments
`mcp --server <url> --token-file <path>`. For Cursor, in `.cursor/mcp.json`:

```json
{ "mcpServers": { "aisre": { "command": "aisre",
  "args": ["mcp", "--server", "https://aisre.internal", "--token-file", "/home/me/.config/aisre/token"] } } }
```

For Codex, in `~/.codex/config.toml`:

```toml
[mcp_servers.aisre]
command = "aisre"
args = ["mcp", "--server", "https://aisre.internal", "--token-file", "/home/me/.config/aisre/token"]
```

The token file is re-read on every call, so a rotated token is picked up without restarting the
agent.

### A shared server over streamable HTTP

```sh
aisre mcp --http 0.0.0.0:8090 --tls-cert cert.pem --tls-key key.pem --server https://aisre.internal
```

Each client sends its own `Authorization: Bearer <token>`. The server forwards it to the API for
that request only, stores none and logs none, and runs without sessions so that a session id never
stands in for a token. It refuses to listen beyond loopback without TLS.

## Tools

| tool | answers | role |
|---|---|---|
| `extent` | what time range the graph covers; which sources have gaps | reader |
| `diff` | what changed around a node in a window, ranked, with the formula | reader |
| `subgraph` | the neighbourhood of a node as of an instant | reader |
| `impact` | what a node can hurt, and what it depends on | reader |
| `pointers` | where its metrics, logs, traces and dashboards live | reader |
| `history` | every version of a node, and who corrected it | reader |
| `resolve_why` | whether two identifiers are one entity, and why | reader |
| `catalog_list`, `catalog_show` | the service catalog and every name a service goes by | reader |
| `attribution_why` | which service a signal belongs to, and why | reader |
| `investigation_list`, `investigation_get` | investigations, with verdict, evidence and citations | reader |
| `investigation_start` | start an investigation of a node or a declared incident | investigator |

Resources: `aisre://guide` (which tool answers which question) and `aisre://catalog`.

Only the tools your roles allow are listed. `--profile baseline` lists the graph and catalog tools
only; the evaluation uses it to score a general agent against the engine
([ADR-0019](decisions/ADR-0019-roadmap-after-the-rca-literature-review.md) D6).

## What an answer looks like

- `structuredContent` is the API's answer in canonical JSON, the same bytes as
  `aisre … --output json` and the fixture goldens.
- The text content is a short summary.
- `_meta["aisre.as_of"]` gives the instants the answer is about; `_meta["aisre.partial"]` is true
  when a cap was reached, with `_meta["aisre.continuation"]` saying what to raise.
- `_meta["aisre.injection"]` lists retrieved text that reads as an instruction (a log line, a
  commit message, a document). It is returned unchanged and flagged: it is evidence about the
  system, never an instruction to the agent.
- An API failure is a tool error with `_meta["aisre.code"]`.

`investigation_start` returns at once with the investigation's id and its first, provisional
ranking; follow it with `investigation_get`. One caller may have three investigations running
through a server (`--max-running`). An investigation of a node started this way records `mcp` as
its origin, so it can be told apart; a declared incident keeps its own origin, which is what makes
declaring it twice, from the CLI or from MCP, one investigation.
