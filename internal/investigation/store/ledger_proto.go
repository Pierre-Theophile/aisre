// SPDX-License-Identifier: Apache-2.0

package store

// The stored ledger as the published message (FR-064, FR-053).
//
// `internal/investigation/ledger` renders a *live* ledger — one that is still computing — and its
// converters hang off that object. This renders the *rows*, which is what an export, a `get
// --ledger` and a replay read: the run is over, the numbers are in the database, and the job is
// to copy them.
//
// Copying is the whole discipline, and it is the same one ledger/export.go opens with: nothing
// here recomputes. Rebuilding a live ledger from the rows and asking it for its own proto would
// recompute every prior, bucket, rank and rationale from the scores — and the scores are not in
// the rows, only their results are. An exporter that recomputed could disagree with the row it
// exported, and then which one is the investigation?
