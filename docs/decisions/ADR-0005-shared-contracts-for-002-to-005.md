# ADR-0005: Contracts that features 002–005 each asked feature 001 for, decided once

- Status: **Accepted** (2026-09-17; realised 2026-09-18, see below)
- Context: the four specs written after the external review each list "Dependencies on
  feature 001". Several asks overlap and two conflict. They are decided here so that 001
  implements each once, additively (schema MINOR bump), before 002's fixtures are recorded.

## Decisions

### D1. Actor kind on a change
`ActorKind { PERSON, AUTOMATION, CONTROLLER, VENDOR, UNKNOWN }` on `Change`, on `RankedChange`
and in the change-observation event. `AUTOMATION` is a CI/deploy principal acting on a
person's behalf (003's concern); `CONTROLLER` is an autoscaler, scheduler or platform reacting
to state (the class 002's causal ordering exonerates after onset); `VENDOR` is a provider's
maintenance, deprecation or incident. 005's earlier `human/system/vendor/unknown` maps to this
set (`system` → `CONTROLLER` or `AUTOMATION` by derivation, else `UNKNOWN`).

### D2. `alert.transition` event
A typed event body with: source, alert ref (monitor/policy/channel), group, transition instant,
from/to state (published vocabulary: ok, warn, alert, no_data, declared, resolved), severity,
title, actor kind, declaring identity for human declarations, target refs with per-ref
provenance. Idempotency key `(source, stable alert identifier, group, transition instant)`.
Two transports for one event (poll = truth, doorbell = trigger) are a connector concern.

### D3. New taxonomy members
`EdgeType WATCHES` (alert → watched entity), `INVESTIGATED` (investigation → focus),
`CONCERNS` (knowledge document → entity). `NodeType INVESTIGATION`, `KNOWLEDGE_DOC` (kept: the
constitution requires every durable document to be linked to the graph nodes it concerns, so a
document is a node with `CONCERNS` edges and a pointer, never content). `ChangeKind` gains
`DEPRECATION`, `VENDOR_INCIDENT`, `IAM_CHANGE`, `QUOTA_CHANGE`.

### D4. Ranker behaviour for post-reference candidates
Signed time distance exposed (`signed_time_distance_seconds`, `post_reference`); candidates
after the reference instant decay on a separate, steeper curve and never receive the maximum
temporal weight; the response identifies them so the engine can type them as candidate
effects. Details in 002's plan (D1) and `docs/schema/ranking.md` on implementation.

### D5. Announced facts (future-dated valid time)
Ingestion already accepts a valid start later than the observed start. 001 publishes the
semantics from 003's spec: observed time is never in the future; an announced change is
excluded as a cause for reference instants before its valid start; cancellation and
reschedule are corrections, never retractions; a fixture pins it.

### D6. Pointer join keys and vocabularies
`Pointer.join_keys` with roles version, workload, pod, host, trace; a registry of pointer
vocabularies (Cloud Logging filter, Cloud Monitoring query, trace filter, Datadog query) each
with its documented reason for not being OpenTelemetry-expressible.

### D7. Published home of the query algebra and digest contract
`specs/002-investigation-engine/contracts/telemetry-backend.md` and `investigation.proto`
§1–3 are the single published home for the algebra terms — the **three families**: graph terms,
replayed from the event log and never world-recorded; the **eight** telemetry terms a backend
serves; and `knowledge_search` — together with the digest shape, coverage block,
join keys, drill-down handles and the six typed outcomes (`digest`, `no_data`,
`not_yet_ingested`, `query_failed`, `partial`, `not_recorded`; the outcome formerly written `ok`
is named `digest`). 003 and 005 implement it; they do not restate it.

### D8. Report delivery is the project's first write path
*Authority: constitution v1.1.0, Principle VII, confined exception "report delivery" (amended 2026-09-17 after analyze finding C1). This ADR records the confinement; it does not grant it.*
Posting an investigation report where the incident lives (002 FR-057f) is allowed under
constitution VII as a confined exception: one message, edited in place, a credential scoped
to that report and separate from every read credential, non-blocking, never a precondition
of a terminal state, transport owned by a connector. Any wider write requires a new ADR.

### D9. Prior of "no observed change explains this"
`π₀ = 1 − ceiling` from the latest published coverage audit (0.38 at the time of writing).
Recomputed when the audit is re-run; recorded in every investigation's ledger.

## Consequences
- One 001 work package implements D1–D6 additively; goldens are re-recorded only where a
  candidate is later than the reference instant.
- 002's tasks start with the coverage-audit command and this package, per ADR-0003.

## Realised by (2026-09-18)

D1–D6 landed as the feature 001 change package, recorded in
[ADR-0006](./ADR-0006-feature-001-change-package.md): additive, `sreagent.graph.v1` MINOR, no
event-log transformation. D7–D9 landed across feature 002 phases 2–8 (`f53ec15` and earlier).

| decision | realised by |
|---|---|
| D1 actor kind | `ActorKind` in `api/sreagent/graph/v1/graph.proto`, migration `0005_actor_kind.sql`, `internal/projector/observe_change.go` — unspecified where the source is silent, never guessed |
| D2 `alert.transition` | event body 25, `internal/log/alert.go` (the derived key, the published state vocabulary, the flapping and no-data suppression), `internal/projector/alert_transition.go` |
| D3 the other four event types | bodies 21–24, dispatched in `internal/projector/apply.go`; documented in `docs/schema/investigation.md` |
| D4 signed time distance | `RankedChange.signed_time_distance_seconds` and `post_reference`, `internal/query/rank.go`, published in `docs/schema/ranking.md` |
| D5 two-sided decay | `internal/query/rank.go` with `kappa = 0.5`, `tau_post = tau/2`, and the four properties asserted in `rank_property_test.go` |
| D6 join keys on pointers | `Pointer.join_keys` with the published role set, `docs/schema/pointers.md`; the `version` role is the one the redaction policy leaves in the clear |
| D7 the query algebra and digest contract | `pkg/backend`, `internal/investigation/backend`, `docs/schema/algebra.md`, `docs/schema/digests.md` |
| D8 report delivery | promoted to its own record, [ADR-0007](./ADR-0007-report-delivery-write-path.md), and implemented in `internal/investigation/render/delivery.go` and `internal/investigation/store/delivery.go` |
| D9 `π₀ = 1 − ceiling` | `internal/investigation/audit.PriorFromAudit`, recorded on every investigation with the audit id, ceiling, feeder set and incident count; published in `docs/schema/ledger.md` |
