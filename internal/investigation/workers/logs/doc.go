// SPDX-License-Identifier: Apache-2.0

// Package logs is the logs worker: an in-repo Drain-style template miner, new_log_patterns,
// bounded exemplars on explicit request, and an optional template-only labelling pass whose
// contribution the digest states (FR-009a, contracts/worker-sdk.md).
//
// The model never sees a raw log line.
//
// The miner runs first, always. The model may only label what it produced, and the digest states
// what that added over the algorithm.
package logs
