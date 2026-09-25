# The query contract

Every read of the graph answers one shape of question: *what was true at instant T, as far as
we knew at instant O?* That shape is not a convention — it is the whole point of the substrate
(constitution II), and it is why every query in this contract takes two instants rather than
one.

This document is the human companion to `api/sreagent/graph/v1/graph.proto`. The proto is the
source of truth for field names and types; this file says what the fields *mean* and what the
engine does with them. Queries land user story by user story; the sections below cover the ones
that are implemented.

| Query | RPC | CLI | Status |
|---|---|---|---|
| Subgraph as-of | `QueryService.Subgraph` | `aisre query subgraph` | implemented (FR-026) |
| Extent | `QueryService.Extent` | `aisre extent` | implemented (FR-052) |
| Diff with ranked changes | `QueryService.Diff` | `aisre query diff` | implemented (FR-027, FR-028) |
| Impact | `QueryService.Impact` | `aisre query impact` | implemented (FR-029) |
| Pointers | `QueryService.Pointers` | `aisre query pointers` | implemented (FR-030) |
| Node history | `QueryService.NodeHistory` | `aisre query history` | implemented (FR-032) |
| Resolution audit / suggestions | `ResolutionAudit`, `Suggestions` | `aisre resolve …` | not yet (FR-031, FR-033) |

Every query needs the `reader` role and nothing more. Read-only credentials suffice for the
whole contract (FR-035); an RPC that asked for more would break that promise.

---

## The two instants

```
as_of.valid_at     when the facts were true in the production system   REQUIRED
as_of.observed_at  when the graph knew them                            optional
```

`valid_at` is required and is never defaulted. Silently answering "now" for a caller who forgot
to say *when* would answer a different question from the one asked, and the caller would have no
way to tell. A request without it is `INVALID_ARGUMENT`.

`observed_at` unset means **as known now**: every fact whose observed interval is still open.
Setting it rewinds the graph's knowledge, which is how you reconstruct what an alert could have
told you at the time rather than what a later correction revealed. The two questions differ
exactly when a fact arrived late or was corrected:

```sh
# What was true at 14:32, as we understand it today.
aisre query subgraph otel.service.name=checkout --as-of 2026-09-01T14:32:00Z

# What was true at 14:32, as far as anyone knew at 14:32.
aisre query subgraph otel.service.name=checkout \
  --as-of 2026-09-01T14:32:00Z --observed-at 2026-09-01T14:32:00Z
```

On the command line an instant may be written as RFC 3339 (`2026-09-01T14:32:00Z`), relative to
now (`-30m`, `+2h`), or as `now`.

---

## Subgraph

> `QueryService.Subgraph(SubgraphRequest) → SubgraphResponse` — FR-026, US1.

The neighbourhood of one node, as of an instant: what it talks to, what talks to it, what it
runs on, what it depends on, who owns it, and how much traffic flows over each relationship.

### Parameters

| Field | Default | Meaning |
|---|---|---|
| `focus` | — | The node to start from, as a `{namespace, value}` ref. Required. |
| `as_of.valid_at` | — | Required. See [The two instants](#the-two-instants). |
| `as_of.observed_at` | as known now | Pins the graph's knowledge to an instant. |
| `hops` | `2` | Radius. Maximum 8; beyond three hops the neighbourhood is the whole graph and the question is really an impact query. |
| `direction` | `BOTH` | `UPSTREAM`, `DOWNSTREAM` or `BOTH`. See below. |
| `edge_types` | all | Follow only these edge types. |
| `min_weight_class` | unset | Drop edges below this traffic weight class (0–5, research §8). |
| `per_hop_cap` | `50` | Maximum fan-out expanded from any one node. |
| `total_cap` | `500` | Maximum nodes in the whole response. |

On the command line the focus is written `<namespace>=<value>` — `otel.service.name=checkout`,
`k8s.deployment=shop/checkout` — or `id:<entity_id>` for a canonical id taken from a previous
answer. Edge types accept either spelling: `depends_on` or `depends-on`.

A focus reference the graph has never seen is `NOT_FOUND`. That is deliberately distinct from a
reference that resolves to an entity with nothing valid at the instant, which is an empty
answer: "I have never heard of this" and "this did not exist then" are different facts.

### Direction

Direction is named from the point of view of the node that is failing, because that is the
question being asked at 03:00:

| Value | Follows | Reads as |
|---|---|---|
| `UPSTREAM` | edges pointing **into** the focus | its callers and dependents — **who is hurt** |
| `DOWNSTREAM` | edges pointing **out of** the focus | its callees and dependencies — **what could be hurting it** |
| `BOTH` (default) | either | everything within N hops, whichever way the arrow points |

So with `checkout —calls→ payments`, `payments` is DOWNSTREAM of `checkout` and `checkout` is
UPSTREAM of `payments`. `checkout —runs_on→ node-pool` makes the node pool a downstream
dependency of checkout. The rule is about the arrow, not about the edge type: every edge in the
taxonomy points from the dependent thing to the thing it depends on, so "downstream" is always
"what I rely on".

`--direction up|down|both` on the command line.

### Response

```
focus       the focus node's version at the instant (also present in `nodes`)
nodes       every node in the neighbourhood, ordered by version_id
edges       every edge between them, ordered by version_id
truncation  what, if anything, was left out
extent      how much of reality the graph claims to have seen (FR-052)
```

`nodes` includes the focus, so the response is self-contained: every edge's `src_id` and
`dst_id` name a node that is present. `focus` is a convenience pointer to the same version.

Ordering is by `version_id` in both lists, which is deterministic and independent of traversal
order — that is what lets a response be frozen as a golden fixture and compared byte for byte
(contracts/fixture-format.md).

Each `NodeVersion` carries its `valid` and `observed` intervals, its resolved `type` and all
asserted `facets`, its `display_name` and `aliases`, its flattened `props`, any `conflicts`, its
`pointers`, and its `provenance` — the event ids that produced it and the source of the first of
them (FR-034). Each `EdgeVersion` carries both intervals, its `type`, its `weight_class` where
the edge type has one, its `props`, its `provenance`, and `closed_as_consequence_of` when it was
closed by the retraction of one of its endpoints.

### Truncation

A neighbourhood query has to be bounded or a hub will drag in most of the graph and answer a
question nobody asked (spec edge case "hub explosion"). Two caps do that:

- **`per_hop_cap`** (default 50) applies **per frontier node**, not per hop in total. A hop that
  crosses one hub and four quiet services loses the hub's tail, not four fifths of everything.
  Within a node's fan-out the edges kept are the heaviest first, ordered by weight class
  descending (edges with no weight class last), then by the neighbour's entity id, then by edge
  type, then by version id. That order is stable, so two runs over the same data cut in the same
  place.
- **`total_cap`** (default 500) bounds the whole node set, focus included. When it is reached the
  walk stops.

Whenever a cap bites, the response says so:

```
truncation.truncated                = true
truncation.reason                   = "per_hop_cap" | "total_cap"
truncation.per_hop_cap              = the cap that was applied
truncation.total_cap                = the cap that was applied
truncation.truncated_at_entity_ids  = the nodes whose expansion was cut
```

`reason` holds the first cap that bit, since that is the one that shaped the answer;
`truncated_at_entity_ids` lists every node that was cut whatever the reason. The caps are
reported even when nothing was truncated, so a consumer can always see the limits the answer was
computed under. The CLI prints a `TRUNCATED (…) at: …` line last, where it cannot be missed.

A truncated answer is a partial answer, not a wrong one: re-ask with a larger cap, a narrower
`edge_types` filter, or a `min_weight_class`.

### Before recorded history

An `as_of.valid_at` earlier than anything the graph has ever observed returns an **empty result
with a flag**, not an error (spec edge case "query before history"):

```
nodes = [], edges = []
focus unset
truncation.reason    = "before_recorded_history"
truncation.truncated = false        ← nothing was cut; the graph was simply not watching yet
extent               = the coverage that made the answer empty
```

`truncated` stays false on purpose. Truncation means "there was more and we dropped it"; this
means "there was nothing to have". The `extent` in the same response is what lets a consumer say
*why*: the log's earliest observation is after the instant asked about.

The test is made on the focus alone. If the graph holds a version of the focus valid at the
instant — a fact back-dated before the graph started watching — the answer is the ordinary one,
because the graph evidently does know something about that moment.

### Merged entities

A Kubernetes deployment and the OpenTelemetry service running on it are one entity under two
names (research §10). After the resolution layer merges them:

- either name resolves to the surviving entity, so
  `query subgraph k8s.deployment=shop/checkout` and
  `query subgraph otel.service.name=checkout` return the same node;
- the node's `entity_id` is the survivor's, its `aliases` list every identifier any source has
  claimed for it, and its `facets` keep every type that was asserted while `type` holds the one
  chosen by the published precedence (`SERVICE > WORKLOAD > THIRD_PARTY > INFRA_RESOURCE >
  DB_SCHEMA > CONFIG > FEATURE_FLAG > OWNER > ALERT > CHANGE`);
- edges stored **before** the merge still name the id that was merged away. The query layer
  resolves both endpoints through `merged_into` at read time rather than rewriting history, so a
  pre-merge edge and a post-merge edge are reported identically. The CLI renders such a node as
  `service[workload]`.

Merge chains (A merged into B, B later into C) are followed to the survivor.

### Placeholders

An edge endpoint that no `upsert_node` has ever described — a callee seen only in a span, a
ConfigMap referenced before the Kubernetes feeder listed it — exists as a **placeholder**: an
entity whose only property is `sre.placeholder = true`.

Placeholders are returned as they are. "Something called this exists and nothing has described
it" is an answer, and one an SRE can act on; hiding them would leave edges pointing at nothing.
They carry their aliases, so the name the source used is visible, and the CLI labels them
`(undescribed)`.

### Property conflicts

`props` is a flat `google.protobuf.Struct`: the value a consumer reads, with provenance stripped.
Underneath, the graph stores one record per source per key, so that when two sources disagree
neither is thrown away (data-model.md, edge case "conflicting sources").

When sources disagree about a key:

- `props[key]` holds the **first** asserted value, ordered by source id, so a consumer always
  finds a value there;
- `conflicts` gains an entry naming the key and listing **every** asserted value with the
  `source_id` and `event_id` that asserted it.

The graph never picks a winner. A consumer that cares about the disagreement reads `conflicts`;
one that does not gets a usable value and is not silently misled about the graph's confidence.
Two sources asserting the *same* value for a key is corroboration, not a conflict: both records
are kept internally, no `conflicts` entry is emitted.

### Worked examples

```sh
# The 2-hop neighbourhood of checkout at the moment of the alert.
aisre query subgraph otel.service.name=checkout --as-of 2026-09-01T14:32:00Z

# Who calls payments, over calls edges carrying real traffic, right now.
aisre query subgraph otel.service.name=payments \
  --as-of now --direction up --edge-types calls --min-weight 2

# Three hops out of a shared cache, capped hard so the answer stays readable.
aisre query subgraph otel.service.name=redis \
  --as-of -30m --hops 3 --per-hop-cap 5

# The same read as canonical JSON — the bytes a golden fixture holds.
aisre query subgraph otel.service.name=checkout \
  --as-of 2026-09-01T14:32:00Z --output json
```

---

## Diff

> `QueryService.Diff(DiffRequest) → DiffResponse` — FR-027, FR-028, US2.

What changed in a neighbourhood between two valid instants, and which change events of that
window are worth looking at first. It is the same neighbourhood read as Subgraph, taken twice.

### Parameters

| Field | Default | Meaning |
|---|---|---|
| `subgraph` | — | The neighbourhood: `focus`, `hops`, `direction`, `edge_types`, `min_weight_class`, caps. Its `as_of` is ignored — the two instants below are the question. |
| `t1`, `t2` | — | The window. Required, and `t1` must precede `t2`, otherwise `INVALID_ARGUMENT`. |
| `observed_at` | as known now | Applied to **both** sides. |
| `reference_at` | `t2` | The instant temporal proximity is measured to. |
| `tau_seconds` | `1800` | The ranking's decay constant. |
| `change_hop_margin` | `1` | How far past the neighbourhood a change's target may lie. |

```sh
aisre query diff otel.service.name=checkout \
  --from 2026-09-01T13:00:00Z --to 2026-09-01T14:32:00Z --hops 2

# Ranked against the moment the alert fired rather than the end of the window.
aisre query diff otel.service.name=checkout \
  --from -2h --to now --reference 2026-09-01T14:21:00Z --tau 15m
```

One observed instant governs both sides, always. A diff whose halves were read from different
states of knowledge would report the arrival of a late fact as a change in production, which is
the exact confusion bitemporality exists to prevent. `--observed-at` rewinds both together.

### Response

```
nodes_added / nodes_removed      node versions that entered or left the neighbourhood
nodes_changed                    NodeDelta: before, after, and a PropertyDelta per moved key
edges_added / edges_removed      edge versions that appeared or went away
edges_changed                    EdgeDelta, keyed `<src_id>|<dst_id>|<type>`
changes                          the ranked change candidates, in rank order
ranking_formula                  the exact formula and parameters used
truncation                       the union of both sides' truncations
```

Identity is the entity, not the version: nodes are matched on `entity_id` and edges on
`(src, dst, type)`, so a new version of the same fact is a **change**, never a removal followed
by an addition. A node whose properties are identical across the window is not reported at all,
even if its version id moved — a new version that says the same thing is not a change to
production, and reporting it would drown the real deltas.

`added` and `removed` are about the *neighbourhood*, not about existence: a node that existed all
along but only became reachable from the focus at T2 is added, and a node still in the graph but
no longer reachable is removed. The edge that brought it in or took it out is in the same
answer.

Property deltas cover the flattened `props` plus two pseudo-keys, `display_name` and `type`, in
sorted key order; an edge delta adds the `weight_class` pseudo-key. A key present on one side
only is reported with the other side unset, which distinguishes "the property appeared" from
"the value changed".

### Ranked changes

Every change whose valid interval intersects `(t1, t2]` and whose target lies within
`hops + change_hop_margin` of the focus, plus every change in the window that is attached to
nothing at all. Each carries its score, the three components, its hop and time distance, whether
it is unattached, the entity ids it changed, and the tie-break rule.

The score, its defaults, the tie-break, the treatment of unattached changes and a worked example
with the numbers are in **[`ranking.md`](ranking.md)**.

---

## Impact

> `QueryService.Impact(ImpactRequest) → ImpactResponse` — FR-029, US4.

The blast radius of a node as of an instant, in two ordered lists: who breaks if it breaks, and
what could be breaking it. It is the same as-of expansion Subgraph does, reported as a ranked
answer rather than as a picture, because at 03:00 the question is "who do I tell?".

### The direction mapping — read this once

The two lists are named for the flow of **consequence**. `direction` on a subgraph request is
named for the **arrows**. They run opposite ways, and the two names are therefore inverted:

| Impact list | Holds | Reached by following | Subgraph `direction` |
|---|---|---|---|
| `downstream` | entities that **depend on** the focus — its callers and dependents; **who breaks** | edges pointing **into** the focus, transitively | `UPSTREAM` |
| `upstream` | entities the focus **depends on** — its callees and dependencies; **what could be breaking it** | edges pointing **out of** the focus, transitively | `DOWNSTREAM` |

With `checkout —calls→ payments`, the impact of `payments` lists **checkout as a downstream
dependent** (US4 scenario 1) — while a subgraph read of `payments --direction up` is what finds
it. Both statements are true and they are about different things: the arrow points from checkout
to payments, the damage travels from payments to checkout.

The inversion is not an accident of implementation and it will not be quietly fixed: "downstream
of an outage" is how operators speak, and "downstream along the edge" is how the taxonomy is
defined. Naming them apart is the honest option.

### Parameters

| Field | Default | Meaning |
|---|---|---|
| `focus` | — | The node to compute the blast radius of. Required. |
| `as_of.valid_at` | — | Required. See [The two instants](#the-two-instants). |
| `as_of.observed_at` | as known now | Pins the graph's knowledge to an instant. |
| `max_hops` | `3` | How far the radius reaches, in each direction. Maximum 8. Past the maximum is `INVALID_ARGUMENT`, never a silent clamp; so is an explicit `0`, which asks for a blast radius with nothing in it. |
| `total_cap` | `500` | Maximum nodes expanded **per direction**. |

The per-hop cap is not settable: impact applies the same default as a subgraph (50 per frontier
node) so that a hub cannot drag most of the graph into one answer, and reports it in
`truncation.per_hop_cap` like any other cap in force.

```sh
aisre query impact otel.service.name=payments --as-of now
aisre query impact otel.service.name=payments --as-of 2026-09-01T14:32:00Z --max-hops 2
```

### Response

```
downstream  ImpactItem per dependent, heaviest first
upstream    ImpactItem per dependency, heaviest first
truncation  what, if anything, was left out
```

Each `ImpactItem` carries:

```
node               the node's version at the instant, in full (FR-034: provenance inline)
hop_distance       the SHORTEST distance from the focus, over the edges that were followed
heaviest_path      entity ids from the focus to the item, focus first, along the heaviest path
weight_class       the traffic weight of that heaviest path
alternative_paths  how many OTHER distinct simple paths reach the item
```

An entity appears **once per list**, at its shortest hop distance. `hop_distance` and
`heaviest_path` answer two different questions and are allowed to disagree: a service reachable
directly over a quiet edge and indirectly over two loud ones has hop distance 1 and a two-hop
heaviest path. That is US4 scenario 2 and `fixtures/rollout-regression-01` records it.

### What "heaviest" means

> The heaviest path is the one whose **weakest link** carries the most traffic.

Formally it maximizes the minimum weight class along the path; ties go to the path with fewer
hops, then to the lexicographically smallest sequence of entity ids. The last clause exists so
the choice is total and two runs over the same graph name the same path.

The definition is a bottleneck, not a sum and not a maximum, because a blast radius is decided
by the narrowest part of the route a failure travels: a hop that carries almost nothing does not
become important by sitting next to a hop that carries everything.

`weight_class` on the item is that minimum — the bottleneck of the chosen path — which is why an
item's weight can be lower than every individual edge on the way to it.

**Edges with no traffic weight count as class 0.** Only `calls` edges carry one (FR-006), so an
ownership, configuration or placement route has weight 0, every such item ties on weight, and the
ordering falls back to hop distance. That is exactly what research §9 prescribes, and it is why
a node pool sorts below a database in the same list.

### Alternative paths

`alternative_paths` counts the distinct simple paths from the focus to the item, up to
`max_hops`, **other than the one shown**. `0` means "this is the only way in".

Enumeration is capped at 100 paths per item, so the count **saturates**: a reported 100 reads as
"100 or more", never as an exact 100. A whole-answer budget bounds the enumeration as well; if it
runs out, the response says so with `truncation.reason = "path_budget"` rather than quietly
returning fewer routes.

### Ordering

```
weight_class descending, then hop_distance ascending, then entity_id ascending
```

Total, and therefore reproducible: this is what lets an impact response be frozen as a golden and
compared byte for byte. Read it top-down — the loudest, nearest things first.

### Truncation and before-history

Both are exactly as Subgraph reports them: `truncation.reason` is the first cap that bit
(`per_hop_cap`, `total_cap`, `path_budget`), `truncated_at_entity_ids` names every node that was
cut, and an `as_of.valid_at` before anything the graph observed returns two empty lists flagged
`before_recorded_history` with `truncated` false.

---

## Pointers

> `QueryService.Pointers(PointersRequest) → PointersResponse` — FR-030, FR-008, US5.

Where to look for telemetry about a node, grouped by kind. The full vocabulary — kinds, backend
kinds, selector grammars, and how a vendor connector maps them — is in
**[`pointers.md`](pointers.md)**; this section is the query contract.

### Parameters

| Field | Default | Meaning |
|---|---|---|
| `focus` | — | The node. Required. |
| `as_of.valid_at` | — | Required. |
| `as_of.observed_at` | as known now | Pins the graph's knowledge to an instant. |

The instant is required and is the whole point. Pointers are versioned in time like any other
property (FR-008), so a lookup dated before a rename returns the selectors that worked then —
US5 scenario 2 — and a lookup that always answered "now" would send an investigator working a
13:00 incident to a dashboard that did not exist at 13:00.

```sh
aisre query pointers otel.service.name=payments --as-of 2026-09-01T13:00:00Z
aisre query pointers otel.service.name=payments --as-of now --output json
```

### Response

```
node     the node's version at the instant, with its display name and aliases
by_kind  map from PointerKind NAME to the pointers of that kind
```

`by_kind` is keyed by the enum's **name** — `METRIC`, `LOG`, `TRACE`, `DASHBOARD`,
`SOURCE_LINK` — so a consumer holding no copy of the enum can still read it. A kind the node has
nothing for is **absent**, not present and empty: "there is no log selector for this node" is
said by the key not being there. A pointer whose kind a source left unset is grouped under
`POINTER_KIND_UNSPECIFIED` rather than dropped.

Within a kind, pointers are ordered by `backend_kind`, then `selector`, then `vocabulary`.

A focus that resolves to an entity with nothing valid at the instant is an **empty answer** with
`node` unset — not an error; a reference the graph has never heard of is `NOT_FOUND`. A merged
entity answers under either of its names.

One limitation worth knowing before reading a golden: a version carries **one** pointer list,
taken from its primary assertion, so pointers are not merged across sources the way `props` are.
[`pointers.md`](pointers.md#one-caveat-pointers-are-not-merged-across-sources) has the detail.

---

## Node history

> `QueryService.NodeHistory(NodeHistoryRequest) → NodeHistoryResponse` — FR-032, US7.

Every version this node has ever had, and every resolution decision taken about it. This is the
query a correction is audited from.

### It takes no instant

Deliberately. Every other read answers "what was true at T, as known at O" and therefore shows
*one* version of a thing; this one answers the question underneath that — **what has the graph
believed about this node, and when did it change its mind?** Pinning observed time would hide
exactly the rows it exists to show.

```sh
aisre query history otel.service.name=payments
aisre query history k8s.deployment=shop/payments --output json
```

### Response

```
versions   every stored version, ordered by observed start, then valid start, then version id
decisions  every resolution decision naming the entity, oldest first
```

Read it down the page as the graph learned things. A **correction** is two rows whose valid
intervals agree and whose observed intervals abut: production did not move, the graph changed its
mind (constitution II — a correction never overwrites). A **retraction** is two rows where the
superseded one is open-ended in valid time and the current one is bounded at the instant the fact
stopped being true.

### Merged-away entities are in the list

A merge does not move the rows it absorbed: they keep their own `entity_id` and are closed in
observed time (research §4). Dropping them would make the history of a merged node begin at the
merge, which is the opposite of auditable, so they are included.

That is also how they are flagged. Every other query reports a version under the **canonical**
entity id, so a response is internally consistent with its edges; **NodeHistory reports each
version under the entity id it is stored against**. A version whose `entity_id` is not the one
the reference resolved to is an absorbed one — the `decisions` list in the same response says
which decision absorbed it, and its bounded observed interval says when. No published field is
repurposed to carry a flag, because the identity of the row already carries it honestly.

The CLI prints an `ENTITY` column only when a history spans more than one entity id, and a
`STATE` column that reads `current` for an open observed interval and `superseded` for a closed
one.

### Decisions

Loaded through the same reader `resolve why` uses, so "why does this node look like this?" and
"why are these two the same thing?" can never give two different accounts of one decision. Each
carries its kind, rule, score, rationale, principal and the event that recorded it
(constitution VI).

---

## Extent

> `QueryService.Extent(ExtentRequest) → Extent` — FR-052.

How much of reality this graph claims to have seen: the observed-time span of the whole event
log, and per source its last checkpoint and the gaps it admitted to.

Read every other answer against it. "checkout called payments at 14:32" means one thing when the
log covers 13:00 to 15:00 with no gaps and quite another when a feeder was disconnected between
14:20 and 14:45. A boundary the graph genuinely does not know is reported as unknown, never as a
zero timestamp (FR-011).

The subgraph response embeds an `Extent` for the same reason, so a consumer never has to make a
second call to judge the first.

```sh
aisre extent
aisre extent --output json
```

---

## Determinism and goldens

Query responses are frozen as golden fixtures and compared byte for byte in CI (constitution
VIII, SC-003), which requires every answer to be a pure function of the graph and the request:

- node and edge lists are ordered by `version_id`;
- per-hop truncation cuts in a documented, stable order;
- a focus reference that resolves ambiguously — several identity claims on one identifier
  pointing at entities a human has not reconciled — resolves to the lexicographically smallest
  surviving id;
- `--output json` from the CLI is the same canonical serialization (`graph.CanonicalJSON`:
  sorted keys, RFC 3339 UTC timestamps, protobuf JSON mapping) that goldens are written in, so a
  command line invocation and a recorded expectation can be diffed directly.

Goldens are regenerated with `aisre fixture record <dir>…`, or, without a PostgreSQL of your
own, with:

```sh
SRE_AGENT_RECORD=1 go test ./internal/query -run TestRecordShippedFixtures
```

Both replay the fixture from empty into a database of its own, run every query the manifest
lists, and write `golden/<kind>.<name>.json` plus, when the manifest declares a `clock.end`, the
observed-time-pinned pass in `golden/pinned/`. Neither ever rewrites `events.jsonl`. Query kinds
this build cannot answer yet are skipped and named in the report rather than written as empty
goldens, and `fixture verify` skips them in the same way.
