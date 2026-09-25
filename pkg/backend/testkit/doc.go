// SPDX-License-Identifier: Apache-2.0

// Package testkit is the conformance suite every telemetry backend must pass
// (contracts/telemetry-backend.md §8, constitution VIII).
//
// A backend is judged by the same standard as a feeder: read-only, declared capabilities,
// recorded responses as its test.
//
//   - Declaration asserts the backend's declaration is registrable: every term published and
//     in the telemetry family, exactly one published cost class per term, a redaction policy
//     with a version, and an algebra version it implements.
//   - Golden replays a recorded world through the backend and diffs every digest against the
//     fixture's goldens, byte for byte.
//   - ModesAgree asserts live mode and recorded mode produce identical digests for identical
//     requests — coverage block and join keys included (SC-003).
//
// Conformance runs all three.
//
// Every check fails rather than skips when no fixture is given: a conformance suite that is
// green before it checks anything is worse than no suite at all. The recorded world **is** the
// golden — there is no second set of expected files to keep in step with it, because a golden
// that can disagree with the recording it was derived from eventually will.
package testkit
