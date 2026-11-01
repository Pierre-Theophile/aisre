// SPDX-License-Identifier: Apache-2.0

// Package mcpserver is aisre's Model Context Protocol server (feature 010): a thin adapter that
// lets a coding agent (Claude Code, Cursor, Codex, any MCP client) read the graph, the service
// catalog and the investigations, and start an investigation.
//
// It is a client of the public Connect API and nothing else (010 FR-002). Every tool builds the
// request the CLI builds for the same question, calls the same RPC with the caller's own
// credential, and returns the answer in the canonical JSON the CLI's `--output json` and the
// fixture goldens use, so the three surfaces agree byte for byte (010 FR-030). The API remains
// the authorisation point: the server only hides the tools the caller's roles would be refused
// (010 FR-013), which it learns from IdentityService.WhoAmI.
//
// The package must not link the store, the projector, the query engine or the server; the
// archtest rule for internal/mcpserver enforces it.
package mcpserver
