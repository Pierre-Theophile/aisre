// SPDX-License-Identifier: Apache-2.0

// Package datadog is the feeder half of the Datadog connector (feature 005). It emits typed graph
// events and nothing else; the telemetry half is internal/backends/datadog, and the two share one
// read-only credential and one budget.
//
// What it emits, per capability (FR-008a), each independently enabled and recorded in every
// checkpoint:
//
//   - logs (on by default): a SERVICE node for every watched log source, carrying a
//     `datadog-logs/v1` pointer with the version join key and the version-stamp discovery verdict
//     (FR-040c, FR-040f); and a ROLLOUT change when the version a service stamps on its own logs
//     changes, dated from first sight and marked as a bound (FR-040g).
//   - monitors (on by default): an ALERT node per monitor or alerting group, and an
//     `alert.transition` per state change, dated from the instant Datadog states (FR-018–FR-026).
//   - tags (on by default): owners and identity claims from allowlisted tags only (FR-065–FR-068).
//   - apm_topology and changes (off by default): silent while off, and stated as off (FR-008b).
//
// Two rules shape everything here.
//
// It is read-only, verifiably: every request it can issue is on the published operation list in
// requestlog.go and docs/connectors/datadog.md, and a write-capable credential refuses the start
// (FR-003, FR-004).
//
// It never reads the graph to decide what to emit. A log source's node is asserted whether or not
// another feeder has already reported the same service, and resolution (C9) merges the two; an
// emission that depended on what other sources had delivered first would make replay order matter
// (constitution III — the reason FR-040f was corrected during planning).
//
// Contract: specs/005-datadog-connector/contracts/datadog-feeder.md.
package datadog
