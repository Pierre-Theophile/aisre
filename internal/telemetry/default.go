// SPDX-License-Identifier: Apache-2.0

package telemetry

import "sync/atomic"

// The process-wide Metrics (FR-051).
//
// Most of the tool passes *Metrics explicitly: the server hands it to its services, the server
// hands it to the projector, and a test constructs one of its own. Two call sites cannot do
// that without dragging an instrument through code that has nothing to do with telemetry —
// a feeder's window emitter and its sync loop, both of which are constructed from a public
// struct whose fields are the connector's contract, not the graph's.
//
// So there is one default, set once at startup beside otel.SetTracerProvider and read through
// a nil-safe accessor. It is the same shape as the OTel globals it sits next to, and it is the
// only global in the package: everything else stays explicit.

var defaultMetrics atomic.Pointer[Metrics]

// SetDefault installs the process-wide Metrics. `aisre serve` and `aisre feed` call it
// once, immediately after Setup; a test may call it and restore the previous value.
func SetDefault(m *Metrics) { defaultMetrics.Store(m) }

// Default returns the process-wide Metrics, or nil when none was installed. Every method on a
// nil *Metrics is a no-op, so the result never needs a check:
//
//	telemetry.Default().SetFeederLag(sourceID, lag)
func Default() *Metrics { return defaultMetrics.Load() }
