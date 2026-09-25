// SPDX-License-Identifier: Apache-2.0

// Package store is the data-access layer for schema investigation: the ledger, the judgments,
// the evidence items, the worker and model calls, the spend and the coverage audits
// (data-model.md §Schema investigation).
//
// Every row here is working material of one run, rebuildable from the decision record in the
// graph plus the recording beside it (plan §Complexity Tracking).
//
// Phase 5 lands the ledger DAO and the decision record; Phase 6 adds the rest of the run's
// working material (worker calls, model calls, spend, reviews).
package store
