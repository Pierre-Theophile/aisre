// SPDX-License-Identifier: Apache-2.0

// Package emit carries a feeder's events somewhere.
//
// Three destinations, one interface (feeder.Emitter):
//
//   - ConnectEmitter sends them to a running graph over ConnectRPC. This is what a live feeder
//     uses.
//   - MemoryEmitter keeps them, validating each one the way the server would. This is what a
//     unit test uses: it catches a telemetry payload or a missing valid time without a
//     database anywhere in sight.
//   - internal/projector/inprocess.Emitter applies them straight to a graph in this process,
//     for the conformance testkit and offline recording runs. It is kept out of this package so
//     that a connector importing emit never links the Postgres-backed projector.
//
// All three register the feeder's source before the first event (FR-018), mint the same
// deterministic checkpoint ids, and treat a REJECTED result the same way: it is an answer the
// feeder is told about, never an error that is swallowed and never an error that stops the
// stream.
package emit
