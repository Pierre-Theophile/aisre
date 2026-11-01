// SPDX-License-Identifier: Apache-2.0

// Package event holds the admission rules of the event log: what makes an envelope acceptable
// (Validate and its rejection reasons), the accepted schema versions, a feeder's declared
// contract (Source), alert-transition keys and flap suppression, and the envelope helpers
// EventType and BodyMessage.
//
// These rules are domain policy, not storage. They live here, free of any database driver, so
// that the public feeder SDK (pkg/feeder) and the investigation engine can apply them without
// pulling the Postgres-backed log (internal/log) into their build. internal/log applies the same
// rules at append time.
package event
