// SPDX-License-Identifier: Apache-2.0

// Package testkit is the conformance suite every feeder must pass (FR-045, FR-048, SC-008).
//
// Three checks, from contracts/feeder-sdk.md:
//
//   - Run replays a fixture's payloads and asserts that every event validates and that the
//     stream equals the fixture's recorded events.jsonl.
//   - Shuffle re-runs with the payloads permuted inside the feeder's own declared reordering
//     window and asserts the feeder emits the same set of facts.
//   - DoubleDeliver runs twice and asserts every second delivery is DUPLICATE_NOOP.
//
// Conformance runs all three. A connector author writes one test function, points it at a
// recorded directory, and finds out whether the connector is mergeable.
//
// Everything here works without a database. Hand it an Emitter over a real graph with
// WithEmitter and the same three checks additionally prove the graph accepts what the feeder
// emits; golden query outputs are compared by `aisre fixture verify`, which owns the
// fixture as a whole rather than one feeder's share of it.
package testkit
