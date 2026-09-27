// SPDX-License-Identifier: Apache-2.0

// Package datadog implements the published telemetry-backend contract over Datadog's log search,
// log aggregation and monitors APIs. It does not define a contract.
//
// Its single responsibility is to answer the **telemetry family of the query algebra in full** —
// all eight terms: compare, onset, new_log_patterns, error_spans, errors_by_version, monitor_state,
// exemplars and drill_down — and never the graph or knowledge families. The algebra, the digest
// shapes, the coverage block, the join keys, the drill-down handles and the typed outcomes are
// published in specs/002-investigation-engine/contracts/telemetry-backend.md and in
// api/sreagent/investigation/v1/investigation.proto; nothing here restates them.
//
// It answers without tracing and without APM (FR-040b): new_log_patterns and errors_by_version come
// from logs alone, compare and onset from log-derived counts, monitor_state from the monitors API,
// and error_spans answers NO_DATA naming the absent span source while apm_topology is off. There is
// no public log-patterns API, so patterns are mined here from a bounded sample, and the coverage
// block says how much of the window the sample was.
//
// Nothing measured is stored. Digests are transient and never written to the graph, and raw samples
// never cross the digest boundary (constitution IV, FR-051).
//
// Version groups name their deploy identifier through pkg/feeder/versionstamp, the same rule every
// log backend uses, so an errors_by_version group resolves to the change of whichever feeder
// deployed that version (FR-040d).
//
// Contract: specs/005-datadog-connector/contracts/datadog-telemetry-backend.md; the GCP backend
// (internal/backends/gcp) is the reference implementation and is followed file for file.
package datadog
