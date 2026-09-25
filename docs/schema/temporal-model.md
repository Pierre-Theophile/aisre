# The temporal model

This document explains how time works in the graph. It assumes no prior knowledge of the
project. If you read only one page before writing a feeder or a query, read this one — almost
every surprising behaviour elsewhere follows from what is here.

## The problem

An alert fires at 14:32. You want to know what the system looked like at that moment, and
what had changed in the hours before.

A graph that stores only the current state cannot answer either question. As soon as the
incident is over, the topology has moved on. So the graph has to keep history.

But "keep history" is not enough, because there are two different histories, and confusing
them is how monitoring tools end up lying to you:

- The service `payments` **stopped receiving traffic from `checkout` at 14:20.**
- **We found that out at 15:00**, when the feeder's retraction window closed.

Ask "was there traffic from checkout to payments at 14:32?" and the answer depends on which
of those you mean. Asked today, the answer is no — it had already stopped. Asked *as the
on-call engineer would have seen it at 14:32*, the answer is yes, because nothing yet knew
otherwise.

Both answers are correct, and both are needed. The first tells you what actually happened.
The second tells you why the on-call engineer did what they did — and it is the only honest
basis for a postmortem.

So every fact in this graph carries **two** time dimensions.

## Valid time and observed time

Every version of every node and every edge carries two intervals. Both are mandatory; a fact
missing either is rejected at ingestion.

**Valid time** — `[valid_from, valid_to)` — is when the fact was true **in the production
system**. "The `checkout` service ran version 4.2 from 13:00 to 14:20." This is the world's
timeline. A feeder asserts it, based on what the source system says.

**Observed time** — `[observed_from, observed_to)` — is when **the graph knew** the fact.
"We learned about that between 13:01 and 15:00." This is our timeline. The graph assigns it;
a feeder cannot set it. Elsewhere you may see this called *transaction time* or *system time*;
they mean the same thing.

The two are independent. A fact can be:

|                          | learned quickly | learned late |
|--------------------------|-----------------|--------------|
| **true now**             | ordinary        | a backfill, or a slow feeder |
| **true in the past**     | ordinary history | a late-arriving fact — the interesting case |

The second row, second column is what breaks single-timeline systems. It is routine here.

### Why "observed" rather than just "created"

Observed time is an interval, not an instant, because knowledge ends as well as begins. When
we later learn that something we believed was wrong, the old belief does not vanish — its
observed interval **closes**. It stays in the database, queryable, forever. That is what makes
"what did we believe at 14:32?" answerable at all.

`observed_to` unset means "this is what we believe right now". That is how the graph
represents current knowledge: not a separate table, just the rows whose observed interval is
still open.

## Half-open intervals

Both intervals are **half-open**: the start is included, the end is excluded. Written
`[start, end)`.

If `checkout` ran version 4.2 over `[13:00, 14:20)` and version 4.3 over `[14:20, ∞)`, then:

- at 13:00 → 4.2
- at 14:19:59.999999999 → 4.2
- at **14:20** → **4.3**, not 4.2, and not both

This is not a detail. Half-open intervals tile a timeline with no gaps and no overlaps, so
every instant belongs to exactly one version of a given fact. Closed intervals would make
14:20 ambiguous — two versions would claim it — and a diff computed over them would
double-count every boundary. Every interval in this schema, in the API and in every fixture,
is half-open. When you write a feeder, the `valid_end` you send is the first instant at which
the fact is **no longer** true.

An unset `end` means unbounded: still true, or still believed. Not "unknown" — see next.

## Unknown boundaries

A feeder often knows a fact without knowing when it started. The OTel feeder sees a span at
13:00 telling it that `checkout` calls `payments`; that call has probably existed for months.

Guessing a timestamp is prohibited. The graph would then assert something no source ever
said, and every diff across that boundary would show a change that never happened.

Instead, the boundary is marked **unknown**, explicitly:

```proto
message Interval {
  google.protobuf.Timestamp start = 1;
  google.protobuf.Timestamp end = 2;   // unset = unbounded
  bool start_unknown = 3;
  bool end_unknown = 4;
}
```

A flag is needed because a range alone cannot distinguish "began before time itself" from "we
do not know when it began". Physically, an unknown start is stored as the first observation
time with `start_unknown = true`, so the row is still indexable; the flag tells you not to
read that timestamp as a claim.

In queries:

- **unknown start** reads as "no earlier than the first observation". A query before that
  instant does not return the fact, and says so rather than implying the fact was false.
- **unknown end** reads as "still true".

The same discipline runs through the whole system: `unknown` is a first-class answer, and
producing a guess where the evidence does not support one is a defect, not a convenience.

## Correction versus retraction

These are the two ways a fact can stop being what it was. They are different operations with
different meanings, and choosing the wrong one corrupts the history.

### Correction — "we were wrong"

The world did not change. Our belief about it did.

The feeder reported that `checkout` is owned by `team-a`. At 15:00 a better source says it
has been owned by `team-b` since 13:00. Nothing happened in production at 15:00; we simply
had it wrong.

A correction **closes the observed interval of the previous version and opens a new version**
with a new observed start. Valid time describes the corrected reality.

| version | valid | observed | owner |
|---------|-------|----------|-------|
| v1 | `[13:00, ∞)` | `[13:01, 15:00)` | team-a |
| v2 | `[13:00, ∞)` | `[15:00, ∞)` | team-b |

Both rows remain. Ask "who owned checkout at 14:00?" and you get `team-b` — that is the
corrected truth. Ask "who did we *think* owned checkout at 14:00?" — that is, valid time
14:00 **as known at** 14:30 — and you get `team-a`, which is what the paging rules used.

### Retraction — "it stopped being true"

Our belief was right. The world moved.

The `checkout → payments` call really did stop at 14:20. We found out at 15:00.

A retraction **sets the valid end** of the fact. The retraction is itself an observation, with
its own observed start, so a query with an observed time before 15:00 still sees the fact as
true and unbounded.

| version | valid | observed |
|---------|-------|----------|
| v1 | `[13:00, ∞)` | `[13:01, 15:00)` |
| v2 | `[13:00, 14:20)` | `[15:00, ∞)` |

The rule of thumb: **did production change, or did our information change?** Production
changed → retraction (or a new version with a later valid start). Information changed →
correction. Getting this wrong is not a cosmetic error; a rollout recorded as a correction
disappears from the ranked change list, and a correction recorded as a retraction invents a
production event that never happened.

Retracting a node cascades to its edges: an edge cannot outlive an endpoint. Cascaded edge
versions carry `closed_as_consequence_of` pointing at the version that caused it, so the
cascade is visible rather than implied.

### What never happens

**Nothing is ever deleted.** No `UPDATE` rewrites a fact in place, no `DELETE` removes a
version, and history has no horizon — the graph answers as-of queries for any instant since
its first observation. The one permitted mutation in the entire store is closing an open
observed interval, and even that only ever goes from open to closed, never back. CI refuses
any migration that would drop, delete from or truncate the log or a version table.

Retention limits, if they are ever introduced, require amending the project constitution.
Nothing in the design may assume one exists.

## "As known at" queries

Every query takes two instants:

```proto
message AsOf {
  google.protobuf.Timestamp valid_at = 1;    // required
  google.protobuf.Timestamp observed_at = 2; // unset = now
}
```

- `valid_at` — the moment in the production system you are asking about.
- `observed_at` — the moment of knowledge you are asking from. Omit it for "as we know it
  now".

The result is the graph exactly as it was true at `valid_at`, as known at `observed_at`. In
storage that is one predicate per dimension — `valid @> valid_at AND observed @> observed_at`
— which is why both are range-indexed columns rather than derived at query time.

The four useful combinations:

| `valid_at` | `observed_at` | What you get | When you want it |
|------------|---------------|--------------|------------------|
| 14:32 | now | What was really true at 14:32, with everything we have since learned. | Root-cause analysis. The default. |
| 14:32 | 14:32 | What the graph could have told you at 14:32. | Reconstructing a decision. Postmortems: "why did we look at the database first?" |
| now | now | Current state. | Dashboards, exploration. |
| 14:32 | 15:30 | What we believed at 15:30 about 14:32. | Auditing a specific report or alert. |

Running the same query at both `observed_at = valid_at` and `observed_at = now` and diffing
the two is the direct answer to "what did we not know at the time?" — often the most useful
question a postmortem can ask.

### Reproducibility

Because observed history is immutable, a query for a given `(valid_at, observed_at)` pair
always returns the same thing. New facts arriving tomorrow change what `observed_at = now`
returns; they cannot change what `observed_at = 14:32` returns. A link to a query result is a
link to evidence, not to a moving target.

## A worked example

One edge, four events, and what the graph says at each stage.

The facts, as the world experienced them:

- 13:00 — `checkout` starts calling `payments`.
- 14:20 — a rollout of `payments` removes the call.

The events, as the graph received them:

| # | arrives | event | effect |
|---|---------|-------|--------|
| 1 | 13:01 | `UpsertEdge(checkout → payments, valid_at: unknown)` | v1: valid `[13:01, ∞)` with `start_unknown`, observed `[13:01, ∞)` |
| 2 | 15:00 | `RetractEdge(checkout → payments, valid_end: 14:20)` | v1 observed closes at 15:00; v2: valid `[13:01, 14:20)`, observed `[15:00, ∞)` |
| 3 | 15:30 | `ObserveChange(rollout of payments, valid_at: 14:19)` | change node + `changed-by` edge |
| 4 | 16:10 | a second feeder reports the call actually began at 12:40 | v2 observed closes at 16:10; v3: valid `[12:40, 14:20)`, observed `[16:10, ∞)`, no `start_unknown` |

Note what event 1 did *not* do: it did not claim the call began at 13:01. It marked the start
unknown, which is why event 4 is a correction that adds information rather than one that
contradicts a guess.

Now the queries, all run today:

| query | answer |
|-------|--------|
| `valid_at = 14:32`, `observed_at` = now | No edge. It ended at 14:20. |
| `valid_at = 14:32`, `observed_at = 14:32` | Edge present, valid `[13:01, ∞)`, start unknown. This is what the on-call saw. |
| `valid_at = 13:00`, `observed_at` = now | Edge present — v3 says it began at 12:40. |
| `valid_at = 13:00`, `observed_at = 15:30` | No edge. At 15:30 we still believed it began at 13:01. |
| `valid_at = 14:00`, `observed_at = 14:30` | Edge present, still believed unbounded. |

The third and fourth rows are the point of the whole design. Same instant in the world, two
different states of knowledge, both faithfully reproducible, neither overwriting the other.

## What this means for a feeder

1. **Send valid time; never send observed time.** The graph stamps observed time at
   acceptance and stores it on the event row, so replaying the log reproduces the same graph
   byte for byte. A `source_observed_at` field exists for informational purposes and does not
   affect the projection.
2. **Mark unknown boundaries as unknown.** Do not substitute "now" for "we didn't see it
   start".
3. **Retract when the world changed; correct when you were wrong.** Re-sending a corrected
   `UpsertNode` or `UpsertEdge` is a correction. `RetractNode` / `RetractEdge` with a
   `valid_end` is a retraction.
4. **Out-of-order delivery is fine.** The projector never reorders the log. Valid-time state
   is a pure function of the event *set*, because each event carries its own valid time and
   the projector splits or closes overlapping versions accordingly. Declare your ordering
   guarantee and reordering window and let arrival order be what it is.
5. **Re-delivery is free.** Every event carries an idempotency key; a repeat is a no-op.
   CI double-delivers every fixture event to prove it.

## Reference

- Requirements: `specs/001-temporal-graph-core/spec.md` §B, FR-010 to FR-016.
- Storage representation, and the alternatives rejected:
  `specs/001-temporal-graph-core/research.md` §3.
- Tables and invariants: `specs/001-temporal-graph-core/data-model.md`.
- The rule this all serves: `.specify/memory/constitution.md`, Principle II — "Bitemporal or
  nothing".
