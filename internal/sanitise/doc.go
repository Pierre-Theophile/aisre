// SPDX-License-Identifier: Apache-2.0

// Package sanitise applies the published sanitisation contract before anything touches disk.
//
// Its single responsibility is to turn a recorded vendor payload into one that may be committed:
// each field and label key recorded verbatim, replaced by a keyed pseudonym, or dropped. A field
// with no disposition **fails the commit** rather than defaulting to safe, because a default is
// how the next field nobody thought about gets recorded (FR-134).
//
// Two properties are the whole point of it being a package rather than a step in the recorder.
// It runs **in the connector, before disk** (FR-137), so a recording taken without it cannot be
// cleaned up afterwards — the unsanitised bytes were never written. And it is shared with feature
// 005 rather than reimplemented, so one contract version governs every corpus and appears in every
// fixture manifest.
//
// People identifiers are dropped, not hashed; infrastructure identifiers are pseudonymised with a
// keyed HMAC; log lines are reduced to mined templates; email bodies are never stored at all.
//
// Contract: specs/003-gcp-integration/contracts/sanitisation.md (version 1.0.0).
package sanitise
