# The investigation record

An investigation is a run: a question, a sequence of recorded tool calls, a belief state, and an
answer. This page is about what survives it — which of those things becomes a **fact in the
graph**, which becomes a **derived row** that can be thrown away and rebuilt, and which becomes a
**file beside the graph**. Getting that division wrong is how an event log turns into a debugging
trace, so it is published rather than left to taste.

Three storage locations, one rule each:¹

| where | what lives there | why there |
|---|---|---|
| schema `log` / `graph` | the decision record, human facts, reopen links, labels, alert transitions | each is a fact about the production system's operation, or a decision about it |
| schema `investigation` | the run: ledger, judgments, evidence items, worker and model calls, spend, reviews | working material of one run — **derived**, see below |
| files beside the graph | the recording: trajectory records, world digests, sanitised exemplars | bulk, bounded, redacted, addressed by key and digest |

The machine-readable sources of truth are
[`api/sreagent/graph/v1/graph.proto`](../../api/sreagent/graph/v1/graph.proto) (the event bodies,
fields 21–25 of `EventEnvelope.body`) and
[`api/sreagent/investigation/v1/investigation.proto`](../../api/sreagent/investigation/v1/investigation.proto)
(the ledger, the algebra, the recording format). The storage shape is
`internal/store/postgres/migrations/0006_investigation.sql`.

## The five events this feature emits

All five are additive members of `EventEnvelope.body`, all bitemporal, all idempotent, all
replay-surviving (ADR-0005 D2/D3, ADR-0006). The **working material of a run — judgments, worker
calls, ledger updates — is never eventful** (constitution III): it is rebuildable, and putting it
in the log would make the log a trace of the agent rather than a record of the system.

| event | field | idempotency key | what it carries |
|---|---|---|---|
| `record_investigation` | 21 | **the investigation id** | subjects, target entities, started/ended, outcome, stop reason, verdict line, a hypothesis summary (ids, statements, statuses, confidences, buckets, ranks), spend, model configuration, `recording_key` and `recording_digest`, requester |
| `submit_human_fact` | 22 | `human_fact:sha256(investigation_id, author, submitted_at, statement)` | kind, statement, entity refs, the interval it concerns, the authenticated author, submitted-at |
| `reopen_investigation` | 23 | `reopen:` **the child investigation id** | parent id, child id, cause (`human_fact` \| `answer`), cause ref |
| `label_investigation` | 24 | `label:sha256(investigation_id, author, labelled_at)` | `was_this_right`, author, labelled-at |
| `alert_transition` | 25 | `sha256(source_id, stable_alert_id, group_key, transition_at)` | monitor ref, group key, transition instant, from/to state, watched entities, transport, origin ref, actor kind, severity, title, declaring identity |

`alert_transition` is the one this feature **consumes** rather than produces: it is the intake
event both front doors normalise into, and it is emitted by an alert feeder (003/005). It is
listed here because the investigation record is unreadable without it — it is the fact the
investigation is about.

### What an event may carry, and what it may never

**No series, pointers only.** A decision record carries identifiers, parameters, statuses,
confidences with their buckets, ranks, rationales, spend and digests. It carries **no metric
sample, no log line, no span, and no aggregate of them**. The enforcement is in two places that
check different things (`internal/investigation/store/decision_record.go`):

- `internal/log`'s **allow-list**: every property sits under `sre.investigation.*` or the event is
  refused `prop_namespace`. That is what stops a future author attaching "just the series that
  proves it" under a plausible new key.
- `internal/log`'s **denylist**, applied *inside* the namespace: a list of numbers is a series
  whatever it is called, and a property over 4 KiB is a payload whatever it is called. A decision
  record carrying a series is rejected `telemetry_payload`, which is what SC-010 measures.

The 4 KiB per-property cap is why the hypothesis summary is built by a **published detail ladder**
rather than by serialising the ledger: `full` → `no_rationale` → `identifiers`, then truncation by
rank. The record states which level it used, how many hypotheses it carries and how many there
were, so a reader can tell a complete record from a fitted one. Statements are capped at 140
characters and rationales at 200. The full ledger is in the recording, and `recording_key` and
`recording_digest` are the pointer to it.

### Idempotency, and why each key is shaped as it is

Every key is **derived from the content**, not supplied by the caller, so a connector that forgets
to set `idempotency_key` still gets the right behaviour: a second delivery is `DUPLICATE_NOOP`,
and a second delivery whose *payload differs* is recorded as a duplicate-delivery finding — which
is exactly what a transport quietly rewriting an alert's severity should look like.²

- **One event per investigation.** The investigation id is the key of `record_investigation`, so
  "exactly one event per investigation" survives an engine that retries at conclusion.
- **One alert, whichever door it came through.** `AlertTransitionKeyParts` joins source, stable
  alert id, group and the RFC 3339 nanosecond UTC instant with a NUL byte. The webhook and the
  poll that follows it are therefore one event, and a human declaration observed many times is one
  event however often it is seen. `group_key` is empty for a declaration — pinned here rather than
  left to each connector, which is what makes the two doors one key.
- **A concluded investigation is immutable.** Reopening links a *new* investigation to the parent
  and never rewrites the parent's version, which is why the key is the child's id.
- **A human fact never blocks a run.** Contradictions are stated, not resolved. The key is over
  the statement, so re-submitting the same sentence is a no-op and a *different* sentence from the
  same author at the same instant is a second fact.

Each key carries a **kind prefix** — `alert:`, `human_fact:`, `reopen:`, `label:` — and the hashed
keys join their parts with a NUL byte, instants rendered RFC 3339 nanosecond UTC. The prefix is
not decoration: `log.events.idempotency_key` is UNIQUE across the whole log and
`record_investigation`'s key is the bare investigation id, so a reopen keyed on the *bare* child
id is the same key as that child's own decision record. The reopen is written when the child is
created and the record when it concludes, so the unprefixed spelling would silently cost every
reopened investigation its decision record. `record_investigation` keeps the bare id because it is
the one event per investigation and has nothing to collide with.

The three human events are emitted by `internal/investigation/store/human_events.go`, **in the
transaction that writes the row**, under source `human` — the same source a merge decision is
logged under (`internal/log/human_source.go`), because a person is one source whatever surface
they act through. The authenticated individual is in the body (`author`) and in the apply
principal, never in the source id. None of the three carries a property, so the
`sre.investigation.*` allow-list and the 4 KiB cap hold trivially. The observed instant is the
append (FR-019): a person stating at 17:00 what was true at 14:20 has not made the graph know it
at 14:20, so submitted-at and labelled-at are *valid* time and travel in the body.

What the projector does with each — the `INVESTIGATION` node, the `INVESTIGATED` and `CONCERNS`
edges, the alert node and its `WATCHES` edges — is in `internal/projector/` (`apply.go` dispatches
on the body; `alert_transition.go`, `submit_human_fact.go`, `reopen_investigation.go` and the
decision-record path implement it).

## The `investigation` schema is derived

Principle I says the graph is the source of truth, and schema `investigation` is a second store.
It is admitted for exactly one reason, **derivability**, and for no other:

> Every row in schema `investigation` is rebuildable from **the event log plus a regenerable
> world**.

Not from a dump of the tables — from the log and from test data. The world is regenerable:
`fixture record-world` re-records it from the recording manifest's own declared shape, and the
recorded backend responses are test data rather than a source of truth. That is what makes "the
event log plus a regenerable world" a legitimate rebuild input and `pg_dump investigation.*` not
one.

The claim is asserted rather than asserted-and-forgotten. The rebuild test exports an
investigation, `DROP SCHEMA investigation CASCADE`, forgets migration 0006, re-migrates to an
empty schema, replays the event log into the graph, answers every manifest query from it, and then
runs `investigate replay --from <dir>` to write every row back.

The **human channel has nothing to re-run** — nobody pushes the same fact twice — so its rows come
back from its three events instead, through `investigationstore.RebuildHumanChannel`, the inverse
of the emitter: reopens first (creating the child row from the parent's own question where the
schema no longer holds it), then facts with their evidence items, then labels. A fact's row id and
its evidence id are derived from the investigation id and the submitted-at instant, so a rebuilt
row carries the identifiers it had. A rebuild that needed anything the
event log and the regenerable world do not hold would mean the investigation store had quietly
become a substrate of its own, and principle I would no longer be true of this system.³

Nothing in the schema is a fact about the production system that the graph does not also hold: a
stored graph answer carries the `graph_event_ids` it came from. What the schema adds is the run's
working state.

## The recording layers

A recording is two layers and a manifest, on disk beside the graph:

```text
<recording-root>/<investigation-id>/      # or fixtures/incidents/<id>/ for a fixture
├── manifest.json          # investigation id, instants, algebra and ledger rule versions,
│                          #   redaction policy version, caps in force, digests of both layers
├── trajectories/
│   └── <run-id>.jsonl     # LAYER 1 — the sequence
└── world/
    ├── index.json         # term_key → file, the window grid and hop radius recorded, the
    │                      #   drill-down depth, the miss counter, the index digest
    └── <term_key>.json    # LAYER 2 — one digest per algebra term
```

**Layer 1, the trajectory, is a sequence.** One canonical-JSON document per line in issue order,
typed `model_request`, `model_response`, `worker_request`, `worker_response`, `ledger_update`,
`human_fact`, `stop`. It is JSONL because layer 1 must be able to name the *first diverging
record* (FR-041), which needs an order, and JSONL is the one format where "the third line changed"
is both a `git diff` and a machine's answer. The file is named by content —
`trajectories/run-<16 hex of the trajectory's own digest>.jsonl` — so re-recording an unchanged
run writes the same bytes to the same path and a re-record is a no-op a reviewer can verify with
`git status`. A run id from a clock or a UUID would make every re-record a new file and the corpus
would grow without saying anything.

**Layer 2, the world, is a map.** `term_key → digest`, not a sequence, so a consumer that asks a
*different but in-algebra* question is still served. See
[the query algebra](./algebra.md) for the key and [the digest contract](./digests.md) for the
answer. The graph family is **not** recorded: those answers come from replaying `events.jsonl`
into an empty database, which is why a recorded investigation carries graph events rather than
graph answers.

Digests bind both layers: the trajectory's digest is `sha256` of its canonical JSONL, the world
has an index digest and a per-answer digest, and `recording_digest` on the decision record refers
to them. A recording edited by hand is refused rather than replayed.

### The export

`investigate export <id> --out <dir>` writes the self-contained artifact — the one that answers
the whole investigation with no network, no database and no credential:

```text
investigation.json        the decision record and the hypothesis ledger
events.jsonl              the graph events the investigation's graph queries need
trajectories/<run>.jsonl  layer 1
world/…                   layer 2
export.json               every file with its digest, and the export digest
```

`export.json` is what refuses an artifact that has drifted. The graph events are in there, rather
than a dump of the investigation tables, for the reason above: the export is the input to the
rebuild, and the rebuild is the whole justification for the second schema.

## Related

- [The hypothesis ledger](./ledger.md) — what a decision record summarises, and the rule that
  produced its confidences.
- [The query algebra](./algebra.md) and [the digest contract](./digests.md) — what layer 2 holds.
- [The published schema](./README.md) — versioning, breaking changes and the migration rule that
  apply to these five events like any other.

---

¹ `internal/store/postgres/migrations/0006_investigation.sql`, header; `specs/002-investigation-engine/data-model.md`
§"What this feature writes into the graph".

² `internal/log/alert.go` (`AlertTransitionKey`, `AlertTransitionKeyParts`);
`internal/log/append.go` (`idempotencyKey` defaults to the event id; duplicate deliveries are
recorded with whether the payload differed).

³ `specs/002-investigation-engine/plan.md` §Complexity Tracking; `internal/investigation/replay/export.go`.
