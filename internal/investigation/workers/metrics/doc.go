// SPDX-License-Identifier: Apache-2.0

// Package metrics is the metrics worker: compare, onset and errors_by_version over one
// telemetry backend. No model (contracts/worker-sdk.md).
//
// It also serves monitor_state, because a recovery is evidence and the intake needs the
// transition history. Whether a comparison separates the hypothesis it was asked to settle is
// the worker's own judgement, at the published threshold.
package metrics
