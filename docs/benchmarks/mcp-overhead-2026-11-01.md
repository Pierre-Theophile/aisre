<!-- SPDX-License-Identifier: Apache-2.0 -->

# MCP adapter overhead, 2026-11-01

Feature 010 SC-004: the MCP server adds at most 50 ms at p95 to the API call it wraps.

Measured by `internal/mcpserver/overhead_test.go` (`TestOverheadBudget`), which runs in CI and fails
above the budget. The same `diff` (checkout, 2 hops, 13:00 to 14:32 on `fixtures/rollout-regression-01`)
is asked 40 times directly through the Connect client and 40 times through the MCP tool over
in-memory transports, against an in-process `serve` on PostgreSQL 16.

| | p95 |
|---|---|
| direct API call | 19.3 ms |
| through the MCP tool | 34.8 ms |
| **added by the adapter** | **15.5 ms** |

Machine: a shared Linux container (cloud session), so the absolute numbers are noisy; the added time
is what matters. Most of it is the extra JSON work on each answer: the canonical rendering, the
injection scan over every string, and the SDK's validation of the input against the schema. That is
the cost of FR-030 and FR-041, and it is well inside the budget.
