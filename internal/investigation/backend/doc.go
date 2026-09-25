// SPDX-License-Identifier: Apache-2.0

// Package backend holds the query algebra, the digest contract and the recorded telemetry
// backend that answers an algebra term from a world on disk with no network call
// (plan §Project Structure, contracts/telemetry-backend.md).
//
// algebra.go is the engine's view of the algebra, digest.go the digest contract and its six
// typed outcomes, redact.go the redaction policy, recorded.go the backend that answers from a
// world, and missrate.go the accounting that gates a fixture's admission to the corpus.
package backend
