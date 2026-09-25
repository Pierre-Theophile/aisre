// SPDX-License-Identifier: Apache-2.0

// Package testkit is the conformance suite every worker must pass, the worker analogue of
// pkg/feeder/testkit (contracts/worker-sdk.md §Testkit, constitution VIII).
//
// Three checks:
//
//   - Declaration asserts the worker's own declaration passes the registration gate: one
//     source of truth, every capability read-only and priced, a declared model where there is
//     one, a redaction policy with a version, and both modes present.
//   - Golden replays a recorded world through the worker and diffs its digests against the
//     fixture's goldens byte for byte.
//   - ModesAgree asserts that live mode and recorded mode produce identical digests for
//     identical inputs — coverage block and join keys included, not only the summary
//     statistics (SC-003).
//
// Conformance runs all three. A worker author writes one test function, points it at a
// recorded incident fixture, and finds out whether the worker is mergeable. A worker without
// recorded responses as its test is not merged.
//
// Every check fails rather than skips when no fixture is given, because a conformance suite that
// is green before it checks anything is worse than no suite at all. ModesAgree takes the worker
// over its live backend and, through WithRecordedCounterpart, the same worker over the recorded
// one: comparing a worker with itself would prove only that it is deterministic.
package testkit
