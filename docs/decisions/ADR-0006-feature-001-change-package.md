# ADR-0006: The feature 001 change package — what 002 asked 001 for, and why it is additive

- Status: **Accepted** (2026-09-17; realised 2026-09-18)
- Deciders: project owner (approval), Claude (design, documented here)
- Source: `specs/002-investigation-engine/spec.md` §"Dependencies on feature 001" (D1–D5),
  `specs/002-investigation-engine/plan.md` §"The feature 001 change package",
  [ADR-0005](./ADR-0005-shared-contracts-for-002-to-005.md) D1–D6. Written under
  `specs/002-investigation-engine/tasks.md` T032.

## Context

Feature 002 is an investigation engine over feature 001's graph. Five things it needs are not its
to build: the ranker's behaviour, the shape of a ranked item, the change taxonomy, the pointer
vocabulary and the event schema all belong to 001 and are part of the **published** schema
(constitution IX). ADR-0005 decided each once so that 001 could implement them in one package,
before 002 recorded a single fixture — because a fixture recorded against the old shape is a
fixture that has to be re-recorded.

The question this ADR settles is not *what* to add; ADR-0005 D1–D6 settled that. It is whether the
package can land **additively** — a MINOR bump on `sreagent.graph.v1`, no event-log
transformation, and no existing golden moved for a reason nobody asked for.

## Decision

**The package is additive, `sreagent.graph.v1` takes a MINOR bump, and no event-log
transformation is registered** (`internal/log/migrate` stays `1.0.0 → 1.0.0`). Three rules make
the blast radius exactly three fixtures, and they were decided here because the analyze pass found
them undecided:

1. **`ACTOR_KIND_UNSPECIFIED = 0` stays the enum's zero value.** `buf lint`'s
   `ENUM_ZERO_VALUE_SUFFIX` requires it, and "the source said nothing" is genuinely distinct from
   "the source named an actor the feeder could not type", which is `ACTOR_KIND_UNKNOWN`.
2. **The projector does not write `UNKNOWN` when the source is silent.** It leaves the field
   unspecified, so the canonical serializer emits nothing at all for it, and an existing golden
   with no actor kind is byte-identical after the change. `ACTOR_KIND_UNKNOWN` is written only
   where a feeder observed an actor and could not classify it, with the evidence recorded — never
   guessed.
3. **Empty `Pointer.join_keys` maps are omitted by canonical serialisation**, exactly as every
   other empty map already is, so adding the field moves no golden that has no join keys to emit.

### The contents

| id | what 001 publishes | shape |
|---|---|---|
| **D1** | the ranker's behaviour for a candidate later than `reference_at` | two-sided decay: `temporal = exp(-dt/tau)` for `dt ≥ 0`; `temporal = kappa · exp(-\|dt\|/tau_post)` for `dt < 0`, with published `kappa = 0.5` and `tau_post = tau/2`. Four properties: never the maximum; strictly below an equally distant pre-reference change; monotone in `\|dt\|`; identifiable from the response |
| **D2** | the signed distance | `RankedChange.signed_time_distance_seconds = 11` and `post_reference = 12`; field 7 (floored, unsigned) is left alone so no consumer breaks |
| **D3** | a typed actor kind | `enum ActorKind { ACTOR_KIND_UNSPECIFIED, PERSON, AUTOMATION, CONTROLLER, VENDOR, ACTOR_KIND_UNKNOWN }`; `Change.actor_kind`, passed through on `RankedChange`. `AUTOMATION` (a CI or deploy principal acting on a person's behalf) is **not** `CONTROLLER` (an autoscaler or scheduler reacting to state) — the distinction is what 002's causal ordering exonerates on |
| **D4** | join keys in the pointer vocabulary | `map<string,string> join_keys = 6` on `Pointer`, keys from the published role set (`version`, `workload`, `pod`, `host`, `trace`), values being the attribute name in that backend's vocabulary |
| **D5** | the five event bodies 002 emits or consumes | `record_investigation` (21), `submit_human_fact` (22), `reopen_investigation` (23), `label_investigation` (24), `alert_transition` (25); node types `INVESTIGATION` and `KNOWLEDGE_DOC`; edge types `WATCHES`, `INVESTIGATED`, `CONCERNS`; and an allow-listed `sre.investigation.*` property namespace so the telemetry denylist does not trip on digests and query parameters while still rejecting samples |
| **D-alert** | the `alert.transition` idempotency convention | the key is derived, not supplied: `sha256(source, stable alert identifier, group, transition instant)`, group empty for a human declaration. Flapping and no-data transitions are filtered at the feeder; **recoveries are kept, because a recovery is evidence** |
| **D-future** | future-dated valid time | ingestion already accepts `valid_at > observed_at`; what 001 owed was a fixture proving the projector's segment planning tolerates it, and the published semantic: *a fact valid from a future instant is known now and answered only when asked as-of that instant or later* |

### Announced facts

D-future is the enabling half of what later features call an **announced fact**: a vendor notice
or a scheduled maintenance is observed today and valid from a future instant. The graph stores it
the moment it is announced and answers it only from the instant it becomes valid, which is the
only reading of the bitemporal model that does not either hide the notice or invent a change that
has not happened. 003's vendor-notice feeder depends on it.

## Consequences

- One 001 work package implements D1–D6 additively. Goldens are re-recorded **only** where a
  post-reference candidate or an emitted actor kind appears — `rollout-regression-01`,
  `rollout-regression-02`, `config-change-01` — and byte identity is asserted **positively** for
  every other 001 fixture, rather than inferred from nobody noticing a diff.
- 002's fixtures are recorded after this lands. Recording them first would mean recording them
  twice.
- `scripts/check-report.sh` thresholds are unchanged: the package adds fields and a decay branch,
  not a different score.

## Realised by (2026-09-18)

`specs/002-investigation-engine/tasks.md` T019–T032 (verified 2026-09-17: `buf breaking` clean;
goldens changed only in the three named fixtures, additively — new fields, every rank and score
unchanged; all others byte-identical).

- **D1/D2** — `internal/query/rank.go` and `internal/query/rank_property_test.go`; published in
  [`docs/schema/ranking.md`](../schema/ranking.md).
- **D3** — `api/sreagent/graph/v1/graph.proto`, migration
  `internal/store/postgres/migrations/0005_actor_kind.sql`, and
  `internal/projector/observe_change.go`.
- **D4** — `Pointer.join_keys` in the proto; published in
  [`docs/schema/pointers.md`](../schema/pointers.md); read by `errors_by_version`.
- **D5 and D-alert** — event bodies 21–25 in the proto, `internal/log/alert.go`,
  `internal/projector/apply.go` and the four `apply*` files beside it; published in
  [`docs/schema/investigation.md`](../schema/investigation.md).
- **D-future** — the fixture and the query semantic in feature 001's replay corpus.
