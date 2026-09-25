<!-- SPDX-License-Identifier: Apache-2.0 -->

# mcp-feeder

A feeder whose transport is the [Model Context Protocol](https://modelcontextprotocol.io),
written against the official Go SDK. It is the **spike T093**, not a supported connector: the
point is the shape a connector takes when it reads an MCP server, as the pattern for future
Slack / Notion / Datadog connectors.

**Read [`docs/connectors/mcp.md`](../../docs/connectors/mcp.md) first** — the mapping, the
limitations and the honest assessment live there. This file is just how to run it.

It is a Go module of its own, with a `replace` onto the working tree, so that the `sre-agent`
binary does not link the MCP SDK.

```
mockserver/         a real MCP server standing in for a vendor's: two read-only tools and a resource
feeder/             the feeder.Feeder — the client, the polling Source, and the mapping
testdata/           the recorded fixture: raw MCP responses in, events.jsonl out
cmd/mcp-feeder/     the connector CLI
cmd/mcp-mockserver/ the mock server on stdio
```

## Run it

```sh
go test ./...        # conformance (run + shuffle + double-deliver) and a live run over stdio

# dry run: spawn the mock server, poll it twice, validate the events in memory
go run ./cmd/mcp-feeder --dry-run --polls 2 --interval 1ms \
    --environment prod --mcp-cmd "go run ./cmd/mcp-mockserver"

# against a development graph
aisre serve --auth dev --dev &
TOKEN=$(aisre dev-token --dev --user mcp-feeder --roles feeder --source-id mcp:acme-directory)
go run ./cmd/mcp-feeder --polls 2 --interval 1ms --environment prod \
    --mcp-cmd "go run ./cmd/mcp-mockserver" \
    --server http://127.0.0.1:8080 --token "$TOKEN"

# against a hosted MCP server over streamable HTTP
go run ./cmd/mcp-feeder --mcp-url https://mcp.example.com/mcp --dry-run --polls 1
```

## Re-record the fixture

```sh
go run ./cmd/mcp-feeder --dry-run --polls 2 --interval 1ms --environment prod \
    --mcp-cmd "go run ./cmd/mcp-mockserver" \
    --record testdata/directory-01 --observed-at 2026-09-16T09:05:00Z
```

The mock server's answers are a pure function of its seed and of how many times it has been
polled, so re-recording reproduces the same bytes. `--observed-at` pins the observed time the
in-memory emitter stamps, which keeps `events.jsonl` byte-stable; observed time is assigned by
the graph and is never part of what a feeder is compared on.
