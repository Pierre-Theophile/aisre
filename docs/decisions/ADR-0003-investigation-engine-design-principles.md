# ADR-0003: Design principles for the investigation engine, adopted from external review

- Status: **Accepted** (2026-09-18; proposed 2026-09-17, accepted on the owner's approval of the
  002/003 specs and realised by feature 002 phases 1–8)
- Date: 2026-09-17
- Source: `docs/reviews/2026-09-17-external-review-002-003.md` (outside reviewer), adopted by the
  project owner. Encoded as requirements in `specs/002-investigation-engine/spec.md` and
  `specs/003-datadog-connector/spec.md`.

## Decisions

### D1. Measure the ceiling before building the reasoning layer
The first deliverable of feature 002 is a coverage audit: for the last N real incidents, would the
true cause have been a node in the graph at alert time? The fraction is the upper bound on recall
and decides whether the next feeder outranks the reasoning layer.

### D2. Rank against symptom onset, not alert time
Onset is estimated by change-point detection on the alerting metric. Changes after onset are
exonerated. System-originated changes (autoscalers, operators reacting) are typed apart from
human-originated ones. This is a change to the 001 ranker (reference instant and actor kind).

### D3. The hypothesis space is open
Every investigation carries an explicit "no observed change explains this" hypothesis with its
own probability mass. Confidence is computed deterministically from a hypothesis ledger fed by
typed judgments (supports / refutes / neutral with strength); the model never verbalises a
probability. Coarse buckets until the corpus justifies finer ones.

### D4. Two-layer replay
L1 replays a recorded trajectory, model outputs included, byte-identical, as the plumbing gate on
every change. L2 replays a recorded *world*: the cross product of a small typed query algebra
(compare, new_log_patterns, error_spans, errors_by_version) over the alert neighbourhood and
window, so any reasoning path can be served. Workers may only ask questions expressible in the
algebra. `not_recorded` is distinct from `no_data`; the miss rate is a fixture-quality metric.

### D5. Evaluation is pass@1 with lift over the prior
Headline metric pass@1 (mean over runs); reliability pass^k; never gate on best-of-k. The gate is
an aggregate over fixtures × runs whose detection power is stated. Report lift over the
deterministic ranker (MRR delta) and harm rate. Citation validity is a 100 % hard gate. Fixtures
carry culprit | unobserved | not_change_induced, a causal path, decisive and exonerating evidence
as predicates over digests, a knowability time, and provenance. The corpus is multiplied
metamorphically (culprit deletion, decoy injection, time shift, name permutation) and
adversarially (slow burn, distant culprit, adjacent decoy).

### D6. Anytime budgets, quota-aware
Prior-only ranking within seconds; a deterministic first wave of evidence without a model; `page`
and `review` profiles; caps per backend and cost class expressed as a share of remaining vendor
quota; one investigation per incident; a 15 % synthesis reserve; untested hypotheses reported
with their next queries; confidence widens on exhaustion; typed stop reasons.

### D7. Report-only, with an inverse human channel
No blocking wait on a human. Humans push typed, authenticated, non-blocking facts weighted as
strong evidence, not truth. Investigations reopen on new evidence. Output order: verdict line,
ranked list with exonerations, timeline, narrative; every evidence item deep-links to its query.

### D8. Deterministic workers, one belief state
Graph, metrics and trace workers contain no model. Text sources (logs, docs, chat, postmortems)
may. Reasoning stays in one agent; evidence gathering is parallel. Every worker call carries the
hypothesis and the discriminating question; every digest carries coverage and join keys; bounded
sanitised exemplars are available on explicit request. A fresh-context verifier checks each
claim against its cited evidence before publication.

### D9. Real recordings stay private
Production recordings are sanitised in the connector before disk (people identifiers dropped,
infrastructure identifiers keyed-HMAC pseudonyms, logs as mined templates, monitor bodies
dropped), scanned at commit, canary-tested, and kept in a private corpus with private CI. The
public repository ships synthetic structural twins.

### D10. Alert intake is a feeder with two transports
Idempotent `alert.transition` events keyed on monitor, group and transition time; polling is the
source of truth, a webhook is a doorbell that only triggers a poll.

## Consequences
- Feature 001 gains follow-ups: onset-relative ranking, change actor kind, a "watches" relation
  for alerts, an alert-transition event mapping.
- Feature 002's order of work: coverage audit → onset and causal ordering → query algebra and
  world-recorded fixtures → hypothesis ledger with computed confidence → adversarial and
  metamorphic fixtures.
- The eval harness reports pass@1, pass^k, lift, harm rate and citation validity instead of a
  single top-k number.

## Realised by (2026-09-18)

Feature 002, phases 1–8 (`specs/002-investigation-engine/tasks.md`; the phase-8 commit is
`f53ec15`, and everything before it).

- **D1, measure the ceiling first** — `internal/investigation/audit` (`audit coverage`, the
  machine-readable report, the two-run comparison) and `docs/evaluation/coverage-audit-2026-09.md`.
  The measured ceiling is `π₀`'s source in `internal/investigation/ledger`.
- **D2–D5, onset, causal ordering, the algebra, the ledger** — `pkg/backend/onset`,
  `internal/investigation/engine` (`plan.go`, `causal.go`, `firstwave.go`, `verdict.go`),
  `pkg/backend` + `internal/investigation/backend`, `internal/investigation/ledger`. The published
  prose is `docs/schema/algebra.md`, `docs/schema/digests.md` and `docs/schema/ledger.md`.
- **D6–D8, evidence-backed claims and the injection barrier** — the deterministic citation checker
  and the verifier pass in `internal/investigation/verify`, the refusal path in
  `pkg/worker`/`pkg/backend` (`outside_algebra`), and the recorded injection attempts in the
  ledger's evidence items.
- **D9, real recordings stay private** — the redaction policy in
  `internal/investigation/backend/redact.go`, applied in live mode as well as when recording, and
  the synthetic corpus under `fixtures/incidents/`.
- **D10, alert intake is a feeder with two transports** — the `alert_transition` event body
  (`api/sreagent/graph/v1/graph.proto` field 25), its derived idempotency key
  (`internal/log/alert.go`), and the projector's alert node and `WATCHES` edges
  (`internal/projector/alert_transition.go`).

The consequences the ADR predicted held: the 001 follow-ups became the change package recorded in
ADR-0006, and the eval harness reports pass@1, pass^k, lift, harm rate and citation validity
rather than a single top-k number (`docs/evaluation/investigation-metrics.md`,
`docs/evaluation/thresholds.json`).
