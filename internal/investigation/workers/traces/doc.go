// SPDX-License-Identifier: Apache-2.0

// Package traces is the traces worker: error_spans over an edge and compare over span
// groupings. No model (contracts/worker-sdk.md).
//
// It asks about an edge rather than a service: "payments is slow" is a symptom, "checkout's call
// to payments is slow" is a location, and the graph is what makes the second askable.
package traces
