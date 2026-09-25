// SPDX-License-Identifier: Apache-2.0

// Package ledger is the hypothesis ledger: hypotheses, judgments, the published posterior rule
// and its likelihood-ratio table, the five confidence buckets, and the turn-scoped render
// (plan §Project Structure, research §8).
//
// Confidence is computed here and nowhere else: no model utterance ever writes one (FR-023).
//
// The published rule, the likelihood-ratio table and the bucket boundaries are documented in
// `docs/schema/ledger.md`; `internal/investigation/store` persists what this package computes.
package ledger
