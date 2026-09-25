// SPDX-License-Identifier: Apache-2.0

// Package audit is the coverage audit of User Story 0: given a human-supplied incident list, it
// measures how much of the causal ground the graph can see, publishes the ceiling, and feeds
// π₀ = 1 − ceiling to the ledger (FR-069, FR-071).
//
// The audit measures; it never infers. No model runs anywhere in it.
//
// The files, in the order the work happens:
//
//   - vocabulary.go — the closed sets everything else is written in. There is deliberately no
//     free-text field in this package's input, so that a private audit can be transcribed into
//     it and the file still published (see docs/evaluation/coverage-audit-format.md).
//   - list.go — the input format, strictly parsed from YAML or JSON, and the refusals that keep
//     the audit a measurement: no cause, no audit.
//   - classify.go — verdict to classification, the one mapping worth stating twice: symptom_only
//     is cause_absent, because the effect being visible is not the cause being present.
//   - report.go — the published result and its two renderings, per feeder set and in aggregate.
//   - store.go — the two tables, so a confidence recorded years from now stays interpretable.
//   - compare.go — two runs, with the movement attributed to the feeders added and a ceiling
//     that did not move reported as such rather than omitted (FR-071a).
//   - guard.go — the ceiling guard: only the [ceiling-bounded] criteria, and a loud failure for
//     one carrying no annotation at all (FR-071).
//   - gaps.go — which categories of the unobservable remainder the corpus does not cover
//     (FR-071b). Detection only; T109 wires it into the evaluation report.
//   - prior.go — π₀, the hook Phase 5's ledger calls (ADR-0005 D9).
package audit
