// SPDX-License-Identifier: Apache-2.0

// Package gcp implements the published telemetry-backend contract over Cloud Monitoring, Cloud
// Logging and Cloud Trace. It does not define a contract.
//
// Its single responsibility is to answer the **telemetry family of the query algebra in full** —
// all eight terms: compare, onset, new_log_patterns, error_spans, errors_by_version, monitor_state,
// exemplars and drill_down — and never the graph or knowledge families. The algebra, the digest
// shapes, the coverage block, the join keys, the drill-down handles and the six typed outcomes are
// published in specs/002-investigation-engine/contracts/telemetry-backend.md §1 and in
// api/sreagent/investigation/v1/investigation.proto; nothing here restates them.
//
// Nothing measured is stored. Digests are transient and never written to the graph, and raw samples
// never cross the digest boundary — onset in particular is served backend-side, so Cloud Monitoring
// returns the series to this process, which runs the published change-point method and returns only
// the onset digest (constitution IV, FR-109).
//
// Read the shipped proto, not the contract copy: api/.../investigation.proto carries
// Coverage.truncated_to_horizon and Coverage.horizon (fields 14–15) that the 002 contract copy does
// not, and both are mandatory on a horizon-clamped answer.
//
// Contract: specs/003-gcp-integration/contracts/gcp-telemetry-backend.md. Every Google call goes
// through internal/gcpx.
package gcp
