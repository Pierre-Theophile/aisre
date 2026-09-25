# Data Model: Investigation Engine

**Feature**: 002-investigation-engine | **Date**: 2026-09-17 | Storage: the same PostgreSQL 16+
instance and database as feature 001, new schema `investigation`.

Three storage locations, and the rule that assigns a thing to one of them:

| location | holds | rule |
|---|---|---|
| schema `graph` / `log` (001) | the decision record, human facts, reopen links, labels, as typed events | it is a *fact about the production system or a decision about it* |
| schema `investigation` (this document) | the run: ledger, judgments, evidence items, worker calls, spend, reviews | it is *working material of one run*, rebuildable from the decision record + the recording |
| files beside the graph (`<recording-root>/<investigation-id>/`, or a fixture directory) | the recording: trajectory records, world digests, sanitised exemplars | it is *bulk*, bounded, redacted, addressed by key and digest |

All timestamps `timestamptz` in UTC; all intervals `tstzrange`, half-open `[)`; all canonical
JSON follows 001's serializer (sorted keys, RFC 3339 UTC, six-decimal floats).

---

## Schema `investigation`

### investigation.incidents

One row per incident. An investigation covers an incident, not an alert (FR-008a).

| column | type | notes |
|---|---|---|
| incident_id | text PK | deterministic: `sha256(first symptom's canonical subject, first symptom instant)` |
| canonical_subject_id | text | the graph entity the first symptom resolved to |
| opened_at | timestamptz | first symptom instant |
| last_symptom_at | timestamptz | moves as symptoms attach |
| association_rule_version | text | the published rule that grouped symptoms into this incident |
| status | text | `open` \| `closed` |

### investigation.investigations

| column | type | notes |
|---|---|---|
| investigation_id | text PK | ULID-like, stable, printed in every rendering |
| incident_id | text FK→incidents | |
| reopens_investigation_id | text NULL FK→investigations | set on a reopened run (FR-057b); never an edit of the parent |
| valid_at | timestamptz NOT NULL | the instant the investigation is about (FR-004) |
| observed_at | timestamptz NOT NULL | what it is entitled to know; defaults to `valid_at` |
| review_mode | bool NOT NULL | true ⇒ `observed_at` was "now" (FR-005); recorded, never the default |
| window | tstzrange NOT NULL | the look-back window |
| onset_estimate_evidence_id | text NULL FK→evidence_items | the symptom-onset estimate used as the ranking reference |
| profile | text NOT NULL | `page` \| `review` \| operator-defined |
| lifecycle | text NOT NULL | `running` \| `concluded` \| `reopened` \| `failed` (FR-006) |
| conclusion_kind | text NULL | `partial` \| `final`, only when `lifecycle = concluded` |
| outcome | text NULL | `ranked` \| `unknown` \| `budget_exhausted` \| `failed` |
| stop_reason | text NULL | `completed` \| `budget_exhausted:<dimension>` \| `diminishing_returns` \| `worker_unavailable` \| `refused` \| `failed` |
| verdict_line | text NULL | the one line Sam acts on (FR-057c) |
| requester | text NOT NULL FK→graph.principals | authenticated individual or service identity (FR-066) |
| model_config | jsonb NOT NULL | the whole `config/model.yaml` document: **`provider` and model id per role**, `effort`, thinking display, beta set, price-table version (FR-061). The provider is part of it because a recorded run whose trajectory does not say which vendor answered is a run nobody can reproduce — two providers speak different body shapes, and the same model id under the wrong one is a 404 |
| algebra_version, ledger_rule_version, schema_version | text NOT NULL | published versions in force |
| recording_key, recording_digest | text | the stable link to the recording (FR-035) |
| decision_event_id | text NULL | the graph event emitted at conclusion |
| started_at, ended_at | timestamptz | |

Indexes: `(incident_id)`, `(lifecycle)`, `(started_at)`, `(reopens_investigation_id)`.

**Immutability**: rows are append-only after `lifecycle` leaves `running`. A completed
investigation is never updated; a later run is a new row linked by `reopens_investigation_id`
(FR-007). Enforced by `investigation.refuse_update_of_concluded()`, a trigger added in migration
0008 that rejects any `UPDATE` on a row whose **old** lifecycle is `concluded`, except the single
transition that sets `lifecycle = 'reopened'` and changes nothing else. The rows are compared as
`to_jsonb(NEW) - 'lifecycle'` against the same of `OLD`, so a column a later migration adds is
covered from the moment it exists rather than from the moment somebody remembers it.

The ordering consequence is real and is worth stating where a caller will look for it: anything
that links a conclusion to what produced it — `decision_event_id`, `recording_key`,
`recording_digest` — is written **before** the row is concluded, because afterwards the row is
closed. `internal/investigation/runner` emits the decision record and then concludes, in that
order, for exactly this reason.

### investigation.symptoms

The alert intake (FR-002). One row per alert or human declaration attached to the incident.

| column | type | notes |
|---|---|---|
| symptom_id | text PK | |
| incident_id | text FK | |
| origin_system, origin_ref | text | the alert's source and its identifier, retained (FR-002) |
| transport | text | `monitor` \| `human_declared` \| `explicit_reference` (ADR-0004 D4, FR-001a) |
| actor_kind | text NOT NULL | `monitor` \| `human` — a declaration is the same event shape, distinguished by the actor (FR-002a) |
| statement | text | the symptom in words |
| fired_at | timestamptz | for a declaration, the instant of **declaration** — never the instant the engine observed it (FR-002a, FR-004a) |
| severity, title | text NULL | as declared; never inferred (FR-002a) |
| declaring_identity | text NULL FK→graph.principals | authenticated (FR-002a) |
| idempotency_key | text UNIQUE | monitor: `sha256(monitor, group_key, transition_at)`; declaration: `(source, channel-or-incident id, declared instant)` keyed on the *stable* identifier of the place, not its name (FR-008b) |
| named_identifiers | jsonb | what the alert or declaration called things |
| target_refs | jsonb | zero or more, each with its provenance: `parsed_from_declaration` (naming a published rule id), `supplied_by_human`, or `from_monitor` (FR-002b). Never guessed. |

A parsed reference names the connector rule that produced it as **`<id>@<version>`** in the
single `rule_id` field, on the row and on the wire (`TargetRef.rule_id`). The pair is one field
because the published message has one, and widening a message three features implement is a
breaking change to buy a separator; it is still a pair and still separable —
`intake.RuleRef.String()` writes it, `intake.ParseRule` reads it back, and a rule id that already
contains an `@` is rejected rather than silently split. A value with no `@` is a rule id whose
version was not recorded, which is reported as such rather than invented. This engine never
interprets a rule's text: the registry is a connector artifact (FR-002b, U2).
| resolved_entity_ids | text[] | what they resolved to |
| resolution_evidence_id | text NULL FK→evidence_items | the resolution audit that justifies it (FR-003) |
| attached_as_additional | bool | true when it joined an incident already under investigation |
| grouping_evidence_id | text NULL FK→evidence_items | the FR-008a/FR-008c decision with the time and neighbourhood distances it rested on |

A declaration from which no target reference can be parsed and none was supplied does **not** get
a guessed entity: the investigation proceeds to outcome `unknown` with "name the affected
service(s)" as the resolving action, and a later human fact naming them reopens it and starts from
those services (FR-002b).

### investigation.report_deliveries

FR-057f: where the incident was declared in a place that can hold an answer, the report goes back
there — report-only, **edited in place**, never a stream of re-posts.

| column | type | notes |
|---|---|---|
| delivery_id | text PK, investigation_id FK | |
| target_system, target_ref | text | the stable identifier of the place the incident lives |
| external_message_ref | text NULL | what is edited in place on the next update |
| rendering_digest | text | sha256 of the rendered report delivered |
| first_delivered_at, last_updated_at | timestamptz | |
| update_count | int | |
| outcome | text | `delivered` \| `failed` \| `not_configured` |
| failure_detail | text NULL | a failed delivery is recorded; **the investigation still concludes** — delivery is never a precondition of a terminal state |

The concluded investigation stays immutable (FR-007); the posting is a *rendering* of it. The
credential that posts grants nothing beyond writing and editing that report and is separate from
every read credential (FR-057f) — see the post-design Constitution Check, principle VII.

### investigation.hypotheses

| column | type | notes |
|---|---|---|
| hypothesis_id | text PK | |
| investigation_id | text FK | |
| kind | text | `change` \| `condition` \| `no_observed_change` (exactly one row of the last kind per investigation, FR-019a) |
| statement | text | plain language |
| candidate_change_entity_id | text NULL | for `kind = change` |
| target_entity_ids | text[] | |
| causal_role | text | `cause` \| `candidate_effect` (FR-029c) |
| actor_kind | text NULL | `person` \| `controller` \| `vendor` \| `unknown`, from the ranked item (D3) |
| prior | numeric NOT NULL | from the published ranker score, or π₀ for `no_observed_change` |
| status | text NOT NULL | `proposed` \| `supported` \| `refuted` \| `inconclusive` \| `untested` \| `exonerated` |
| untested_reason | text NULL | required when `status = untested` (FR-031) |
| confidence | numeric NOT NULL | computed; never written by the model |
| confidence_bucket | text NOT NULL | one of the five published buckets, with its range rendered |
| widened | bool NOT NULL | true when the bucket was widened on a cut-short test (FR-045) |
| rank | int NOT NULL | position in the published order |
| rationale | text | derived from the ledger, not from a model's prose |
| knowledge_derived | bool | FR-051; a knowledge-only hypothesis may not reach `supported` |
| next_query | jsonb NULL | for an untested hypothesis: the exact algebra term and its deep link |

Constraint: exactly one row per investigation with `kind = 'no_observed_change'`.
Constraint: `status = 'supported'` requires at least one judgment with direction `supports`
from an evidence item whose worker is a source-of-truth worker (FR-022, FR-051).

### investigation.judgments

The only mechanism by which evidence moves a confidence (FR-020a).

| column | type | notes |
|---|---|---|
| judgment_id | text PK | |
| investigation_id | text FK | |
| hypothesis_id | text FK | |
| evidence_id | text FK→evidence_items | |
| direction | text NOT NULL | `supports` \| `refutes` \| `neutral` |
| strength | text NOT NULL | `weak` \| `moderate` \| `strong` \| `decisive` |
| ln_lr | numeric NOT NULL | the published constant for (direction, strength); stored so the posterior is reproducible from the row |
| source | text NOT NULL | `first_wave` (deterministic, no model) \| `model` \| `human_fact` \| `exoneration` \| `rollback` (a platform-stated rollback away from the named change, 004 T155; Strong at most, like `human_fact`) |
| worker_call_id | text NULL FK→worker_calls | the call it came from |
| recorded_at | timestamptz NOT NULL | |

UNIQUE `(hypothesis_id, evidence_id)` — one evidence item may move one hypothesis once (§8).
Index `(investigation_id, hypothesis_id)`.

### investigation.evidence_items

| column | type | notes |
|---|---|---|
| evidence_id | text PK | |
| investigation_id | text FK | |
| kind | text | `algebra_answer` \| `graph_answer` \| `onset_estimate` \| `resolution_audit` \| `human_fact` \| `knowledge_item` \| `worker_failure` \| `injection_attempt` \| `verifier_finding` \| `association` |
| worker, capability | text | FR-021 |
| source_of_truth | text NOT NULL DEFAULT '' | the single source of truth the answering worker declared, **as of the call** (FR-022; migration 0008). Empty means the worker speaks for none — a knowledge item, a human fact — which is why such an item cannot carry a hypothesis to `supported`. Recorded rather than re-derived from the worker registry, which may since have been renamed, retired or re-pointed |
| term | jsonb NOT NULL | the algebra term as called, canonicalised |
| valid_at, observed_at | timestamptz NOT NULL | the instants in force |
| called_at | timestamptz NOT NULL | |
| mode | text NOT NULL | `live` \| `recorded` |
| outcome | text NOT NULL | `digest` \| `no_data` \| `not_yet_ingested` \| `query_failed` \| `not_recorded` \| `partial` (FR-027; never collapsed) |
| response_digest | text | sha256 of the canonical response |
| response_key | text | key into the recording |
| coverage | jsonb NOT NULL | the mandatory coverage block; a row without one is rejected (FR-014a) |
| join_keys | jsonb | version tag, workload/pod, trace/span ids, first-seen instants (FR-014b) |
| graph_event_ids | text[] | the events the answer rests on, where applicable |
| deep_link | text NULL | resolves to the exact query, selector and window (FR-057d); NULL requires `deep_link_absent_reason` |
| deep_link_absent_reason | text NULL | |
| truncated | bool, truncation_note text | stated in the response it applies to (FR-037) |
| free_text | text NULL | at most one bounded field, flagged unverified, never citable alone (FR-014b) |

Index `(investigation_id, called_at)`, `(response_digest)`.

### investigation.worker_calls

One row per request issued, in order, including retries and duplicates (edge case "duplicate or
repeated worker calls" — both are kept, never deduplicated).

| column | type | notes |
|---|---|---|
| call_id | text PK | |
| investigation_id | text FK | |
| seq | int NOT NULL | position in the investigation; the trajectory's ordering key |
| worker, capability | text NOT NULL | |
| term_key | text NOT NULL | `sha256(canonical(term))` — the world's key |
| serves_hypothesis_id | text NULL FK→hypotheses | FR-018a |
| discriminating_question | text NOT NULL | what the call is meant to settle; `exploratory:<reason>` when it serves no hypothesis |
| mode | text NOT NULL | `live` \| `recorded` |
| outcome | text NOT NULL | `answered` \| `empty` \| `failed` \| `timed_out` \| `refused_outside_algebra` \| `not_recorded` |
| duration_ms | int | |
| cost_class | text | declared by the backend |
| backend, remaining_quota, quota_share_used | text, jsonb, numeric | recorded per call (FR-047a) |
| response_bytes, truncated | int, bool | against the published caps |
| evidence_id | text NULL FK | the evidence item it produced |

UNIQUE `(investigation_id, seq)`.

### investigation.model_calls

The model boundary, recorded with the same discipline as the worker boundary.

| column | type | notes |
|---|---|---|
| model_call_id | text PK | |
| investigation_id | text FK | |
| seq | int NOT NULL | interleaved with `worker_calls.seq` in one sequence space |
| role | text | `investigator` \| `verifier` \| `worker_logs_label` |
| model_id, effort, betas | text, text, text[] | the recorded production configuration |
| request_digest, response_digest | text | sha256 of the canonical bodies |
| input_tokens, cache_write_tokens, cache_read_tokens, output_tokens | int | from `usage` |
| stop_reason | text | including `refusal`, which is handled and recorded, never silently retried |
| duration_ms | int | |

### investigation.recordings

| column | type | notes |
|---|---|---|
| investigation_id | text PK FK | |
| layer | text | `trajectory` \| `world` (two rows per investigation) |
| root | text | filesystem path or object key prefix |
| digest | text | sha256 of the layer's canonical manifest |
| bytes | bigint | |
| term_count | int NULL | world only |
| not_recorded_count | int NULL | world only; the numerator of the miss rate |
| redaction_policy_version | text | what was applied before anything reached disk (FR-038) |

### investigation.human_facts

| column | type | notes |
|---|---|---|
| fact_id | text PK | |
| investigation_id | text FK | |
| kind | text | from a published set (`manual_action`, `vendor_notice`, `known_state`, `correction`, `other`) |
| statement | text | |
| entity_ids | text[], concerns_interval tstzrange | what it is about |
| author | text FK→graph.principals, submitted_at timestamptz | authenticated, never anonymous (FR-066) |
| weight_class | text | `strong` — never `decisive` (§8) |
| evidence_id | text FK | the evidence item it became |
| event_id | text | the graph event it was emitted as (FR-057a, D5) |
| contradicted_by_evidence_ids | text[] | stated, never resolved (FR-057a) |

### investigation.human_reviews and investigation.labels

| table | columns |
|---|---|
| `human_reviews` | `review_id` PK, `investigation_id` FK, `validated_root_cause` (change identifier, or `unobserved`, or `not_change_induced:<category>`), `hypothesis_amendments` jsonb (status/rank/confidence with rationale), `rationale` text, `author` FK, `decided_at`, `event_id`. Additive; never an edit (FR-054) |
| `labels` | `label_id` PK, `investigation_id` FK, `was_this_right` bool, `author` FK, `labelled_at`, `event_id` (FR-057e) |

### investigation.budget_spend

| column | type | notes |
|---|---|---|
| investigation_id | text PK FK | |
| profile | text | |
| limits | jsonb | every dimension as configured, including the 15% reserve |
| consumed | jsonb | wall time, cost units, tokens by model and class, calls by worker/backend/cost class, widest window used, quota share per backend |
| reserve_entered_at | timestamptz NULL | when synthesis-only mode began |
| price_table_version | text | so a historical cost stays interpretable |

### investigation.verifier_findings

| column | type | notes |
|---|---|---|
| finding_id | text PK, investigation_id FK | |
| claim_ref | text | which rendered claim |
| verdict | text | `supported` \| `unsupported` \| `number_mismatch` |
| offending_value, cited_evidence_id, note | text | |
| action_taken | text | `removed` \| `demoted` \| `kept` |

### investigation.coverage_audits / coverage_audit_items

| table | columns |
|---|---|
| `coverage_audits` | `audit_id` PK, `run_at`, `incident_count`, `ceiling` numeric, `feeder_set` jsonb, `input_digest` (of the supplied incident list), `author` FK |
| `coverage_audit_items` | `item_id` PK, `audit_id` FK, `incident_ref`, `alert_at`, `stated_cause`, `classification` (`cause_present` \| `cause_absent` \| `undecidable`), `category` (flag flip, IaC apply, DB migration, certificate expiry, scheduled job, third-party outage, traffic shift, other), `reason`, `matched_entity_id` NULL |

---

## State transitions

**Investigation lifecycle** (FR-006; no waiting state exists):

```text
running ──► concluded(final)      every hypothesis tested or diminishing returns
        ├─► concluded(partial)    budget exhausted, worker unavailable, refused
        └─► failed                intake or engine failure

concluded ──(human fact or answer that bears on it)──► reopened
reopened  ──► a NEW investigation row, linked by reopens_investigation_id,
              which itself runs → concluded under the same rules
```

The parent row is never edited; `lifecycle = 'reopened'` is the single permitted transition on a
concluded row and it only records that a child exists.

**Hypothesis status**:

```text
proposed ──► supported     ≥1 supports judgment resting on telemetry evidence or a human fact
         ├─► refuted       net evidence against, posterior below the refutation threshold
         ├─► inconclusive  tested, nothing separated it
         ├─► untested      no pointer of the needed kind, or no data for the window (reason required)
         └─► exonerated    starts after onset by more than the onset's uncertainty (FR-029c)
```

`untested` is never scored as `refuted` and is never dropped (FR-031). `exonerated` carries the
onset estimate's evidence id and is rendered as prominently as a support (FR-029c, FR-057c).

**The verdict rule — prior-only mass is not confidence** (Phase 8, FR-022, FR-023, constitution V):

A hypothesis may be **named as the verdict only if it carries at least one `supports` judgment
resting on telemetry evidence (`algebra_answer` or `onset_estimate` from a worker that declares a
source of truth) or on a human fact.** Neither a prior nor a graph answer qualifies: the ranker's
score is what put the candidate on the list, and the graph answer behind it says a change happened
near the subject, which is the reason to look rather than the evidence that it is the cause.

Three consequences, all of them enforced in `internal/investigation/ledger` and
`internal/investigation/engine` rather than left to a model's judgment:

1. **`supported` is refused** (`unsupported_status`) for a hypothesis with no such judgment. The
   verdict is derived from the status, so nothing the evidence does not support can be named.
2. **The reported bucket is capped at `moderate`** for a hypothesis nothing observed supports,
   whatever its posterior — the `unevidenced ceiling`. The computed confidence is untouched, as it
   is under widening (FR-045), and both numbers stay visible: a ranker that likes one candidate and
   a run that tested nothing can put 0.9 of the mass on it and know nothing, and publishing that as
   `high` is the failure mode `confidently_wrong` measures. The open hypothesis is evidenced by the
   refutations of its rivals: a run that ruled candidates out on telemetry has measured something,
   and a run that tested nothing has not.
3. **The answer with no qualifying support is `unknown` while the run can still test, and
   `unobserved` at a terminal stop** — `completed` or `diminishing_returns`, never a stop that cut
   the run short — with every remaining candidate recorded `untested` with its reason and next
   query, and **the symptom still localised on the subject** (SC-023, FR-071b). The open hypothesis
   reaches `supported` exactly when no rival is `supported`, which is what makes `unobserved` a
   finding rather than a shrug.

**Onset is estimated on candidate causes, not only on the subject** (Phase 8, FR-029): the engine
estimates onset on the subject and on the targets of the top three ranked candidates, and takes the
**earliest confident** estimate as the ranking reference, ties broken by the tighter uncertainty.
The chosen estimate names the entity and pointer it came from, and every estimate considered is an
`onset_estimate` evidence item in the ledger — which is what makes a decisive predicate written
over the *cause's* `estimated_onset` satisfiable at all. The subject's own estimate remains the
fallback, and the alert instant with the published "could not be estimated" line remains the last
resort (FR-029b).

**Worker call outcome** is one of the six values above; the four "no" answers — `query_failed`,
`no_data`, `not_yet_ingested`, `not_recorded` — are distinct columns of the same enum and are
rendered differently (FR-027).

**Budget**: `normal → synthesis_only (reserve entered) → stopped(typed reason)`. There is no
path from `synthesis_only` back to `normal`.

---

## What this feature writes into the graph (schema `log` / `graph`)

Four event types, all new members of `EventEnvelope.body` (D5), all bitemporal, all idempotent,
all replay-surviving. Payload shapes are canonical in
[`contracts/investigation.proto`](./contracts/investigation.proto); their effect on the graph is:

| event | idempotency key | effect |
|---|---|---|
| `record_investigation` | `investigation_id` | creates an `INVESTIGATION` node valid over `[started_at, ended_at)`, with props = outcome, stop reason, verdict line, ranked hypothesis ids with confidences and buckets, budget spend, model configuration, `sre.investigation.recording_key` and `sre.investigation.recording_digest`; and `INVESTIGATED` edges to every subject and target entity. **No telemetry payload, no digest content — only identifiers, parameters, statuses, confidences, rationales, spend and digests.** |
| `submit_human_fact` | `sha256(investigation_id, author, submitted_at, statement)` | creates a fact node? **no** — it appends the fact to the investigation's evidence as a graph event carrying kind, statement, entity refs, interval, author and time; the projector attaches it to the `INVESTIGATION` node with a `CONCERNS` edge to each entity it names |
| `reopen_investigation` | `child investigation_id` | links child to parent; the parent node version is not rewritten |
| `label_investigation` | `sha256(investigation_id, author, labelled_at)` | records the one-click "was this right?" against the investigation node |

Plus the intake event that 003/005 emit and this feature consumes: `alert_transition`, keyed
`sha256(monitor_id, group_key, transition_time)`.

**One event per transaction** (constitution III): an investigation emits exactly one
`record_investigation` at conclusion; every human action is one event. Judgments, worker calls
and ledger updates are never events.

**Telemetry rejection path** (SC-010): the decision record's props live under the allow-listed
`sre.investigation.*` prefix; the existing denylist (`value`, `body`, `trace_id`, numeric sample
arrays, props > 4 KiB) continues to apply to everything else, and a fixture asserts that a
decision record carrying a series is rejected with `reason_code = telemetry_payload`.

---

## The recording on disk

```text
<recording-root>/<investigation-id>/          # or fixtures/incidents/<id>/ for a fixture
├── manifest.json            # investigation id, instants, algebra + ledger rule versions,
│                            #   redaction policy version, caps in force, digests of both layers
├── trajectories/
│   └── <run-id>.jsonl       # sequence-ordered; model_request | model_response |
│                            #   worker_request | worker_response | ledger_update |
│                            #   human_fact | stop
└── world/
    ├── index.json           # term_key → file, plus the window grid and hop radius recorded,
    │                        #   the recording depth for drill-downs, and the miss counter
    └── <term_key>.json      # one digest per algebra term (telemetry and knowledge families)
```

Bounds: per-response cap and per-investigation cap are published in the contract; a response over
the cap is truncated and the truncation is written *into the response*, so a replay reproduces
the truncation the investigator saw. The graph family is not recorded: those answers come from
replaying `events.jsonl` into an empty database.

---

## Invariants

1. **Every hypothesis's confidence is a pure function of its row's `prior` and the `ln_lr` values
   of its judgments.** Recomputing from the rows must reproduce `confidence` exactly at six
   decimals. Asserted on every write and as a property test over judgment permutations.
2. **Exactly one `no_observed_change` hypothesis per investigation**, always present, always
   ranked and rendered (FR-019a).
3. **Every claim in a rendering resolves to ≥ 1 evidence id**, and every number in a claim
   matches a field of a cited digest (FR-022, FR-061a). Enforced by the deterministic checker
   before the verifier pass, and gated at 100%.
4. **No evidence item without a coverage block** (FR-014a); the row is rejected, not defaulted.
5. **Worker calls are never deduplicated**: a repeated identical term produces a second
   `worker_calls` row and a second `evidence_items` row, is counted against budget, and is
   answered identically.
6. **A completed investigation is immutable**; every later human action is a new row and a new
   event (FR-007, FR-054).
7. **`valid_at` and `observed_at` are present on every evidence item**; an investigation not in
   review mode can hold no evidence item whose `observed_at` is later than the investigation's
   (the observed-time pin; edge case "a merge or correction learned after the alert").
8. **The four negative outcomes are never collapsed** into one another or into "no" (FR-027).
9. **`untested` hypotheses carry a reason and a next query**; `exonerated` hypotheses carry the
   onset estimate's evidence id.
10. **The sum of normalised posteriors over an investigation's hypotheses is 1.0 ± 1e-6**,
    including `no_observed_change`.
11. **No row in schema `investigation` is a fact about the production system that is not also a
    copy of a graph answer**, carried with the `graph_event_ids` it came from. This is what makes
    the schema a derivation rather than a second substrate (principle I).

## Validation rules

Rejected at write time, with a reason code, mirroring 001's discipline:

| reason | when |
|---|---|
| `missing_coverage` | an evidence item with no coverage block |
| `outside_algebra` | a worker call whose term is not a published algebra term (FR-042b) |
| `model_confidence` | an attempt to write a confidence not produced by the ledger rule (FR-023) |
| `unsupported_status` | `status = supported` with no qualifying judgment: no `supports` judgment resting on telemetry evidence or a human fact (and, for the open hypothesis, another hypothesis is already `supported`) |
| `telemetry_payload` | a digest field carrying samples, lines or spans (the 001 denylist, applied at the worker boundary too) |
| `undeclared_redaction` | a worker emitted a field its redaction declaration does not cover |
| `write_capability` | a worker declaring a state-changing capability, at registration (FR-016) |
| `anonymous_principal` | an investigation or human decision with no authenticated identity (FR-066) |
| `duplicate_judgment` | a second judgment for the same (hypothesis, evidence) pair |

## Derived, rebuildable structures

- The **ranked order** and the **rendering** are derived from the ledger on read; nothing is
  materialised.
- The whole `investigation` schema is rebuildable: `aisre investigate replay --from <export>`
  reconstructs every row from the decision record (in the graph) plus the recording (beside it).
  Dropping the schema loses no history — which is the test principle I sets for a derivation.
- The **world index** is derivable from the world files; it is written for speed and its digest
  is checked on load.
