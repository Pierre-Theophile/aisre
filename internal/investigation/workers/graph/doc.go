// SPDX-License-Identifier: Apache-2.0

// Package graph is the graph worker: typed wrappers over feature 001's published QueryService
// RPCs, both time dimensions passed through, deterministic and model-free (contracts/worker-sdk.md).
//
// It holds no telemetry backend and contains no model, and its answers are never recorded into a
// world: they come from replaying the fixture's event log, which feature 001 already guarantees
// is byte-reproducible.
package graph
