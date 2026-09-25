# The change ranking

When `checkout` is failing at 14:32 and was healthy at 13:00, the graph can list every change
that happened in between. On a real system that list is long, and a long list is not an answer.
This document is the published rule that turns it into one: the score, its parameters, its
tie-break, and what happens at the edges — a change nobody can place, a change just outside the
neighbourhood, a change after the instant you are asking about.

The rule is published because a ranking nobody can reproduce is a ranking nobody should trust
(constitution V). Every ranked item carries its own components, and every response carries the
formula it was ranked with, so an operator who disagrees with an order can see exactly which
term produced it.

> Source of truth: `QueryService.Diff` in `api/sreagent/graph/v1/graph.proto`
> (`RankedChange`), implemented in `internal/query/rank.go`. The decision and its rationale are
> research §9; the requirement is FR-028; the accuracy target is SC-005.

---

## The score

For a change `c` ranked against a reference instant `T_ref`:

```
dt          = T_ref - c.valid.start          in whole seconds, SIGNED
temporal    = exp(-dt / tau)                 for dt >= 0   tau      = 1800 s by default
            = kappa * exp(-|dt| / tau_post)  for dt <  0   kappa    = 0.5
                                                           tau_post = tau / 2 = 900 s
topological = 1 / (1 + h)                    h = hops from the focus to c's nearest target
traffic     = (1 + w) / 6                    w = heaviest traffic weight class on that path

score       = 0.50*temporal + 0.35*topological + 0.15*traffic
```

Why these three, in that proportion:

- **Temporal, half the weight.** An incident is a coincidence in time first. Nothing correlates
  with a failure like having happened a minute before it. The next section is about the shape of
  that decay and about what happens on the other side of the reference instant.
- **Topological, a third.** Recency alone would rank every deploy in the company. Hop distance
  is the check on the coincidence: a rollout of an unrelated service is exactly as recent as the
  rollout of your dependency and must not rank with it.
- **Traffic, a sixth.** Not a third opinion but a tiebreak with real information in it: of two
  dependencies at the same distance, the one carrying a hundred times the requests is the one
  whose failure you would notice.

### The two sides of the curve

`dt` is signed, and the two signs are not the same question.

A change *before* the reference instant is a candidate **cause**, and the exponential says how
recently — smoothly, with no cliff at an arbitrary cutoff and no change silently dropped for
being a minute too old.

A change *after* it cannot have caused what you are asking about. It is not irrelevant either:
it may be an effect of the same incident, an autoscaler reacting to the failure, or a
remediation, and an operator wants to see it. So it is ranked, on a separate, lower, steeper
curve:

- **A ceiling of `kappa = 0.5`.** The post-reference side starts at half the temporal weight,
  so no change after the reference can ever take the maximum, and none can beat a change of the
  same distance before it.
- **`tau_post = tau / 2`.** It decays twice as fast, because "shortly after" is the only
  interesting case: an effect follows its cause closely, and something an hour later is neither
  cause nor effect.

Both parameters are derived from the one knob a caller has, `tau`, so `--tau` cannot put the
curve in a state this document does not describe.

Four properties follow, and they are asserted as properties rather than as examples
(`internal/query/rank_property_test.go`):

1. a post-reference candidate never receives the maximum temporal weight;
2. it scores strictly below an equally distant pre-reference candidate;
3. the score is monotone decreasing in `|dt|` on both sides;
4. the caller can tell it is post-reference from the response, without recomputing anything.

Why this matters enough to publish: while `reference_at` is the end of the window — the default
— nothing can follow it, and the earlier rule, which floored `dt` at 0, was harmless. Feature
002 sets `reference_at` to the **estimated symptom onset**, with half the window after it. Under
the floored rule every effect of the incident scored `temporal = 1.0`, the maximum, so the
autoscaler that reacted to the outage outranked the rollout that caused it. That is the change
ADR-0005 D4 records and this section publishes.

There are no learned weights in v1. SC-005 — the culprit in the top three on at least 90% of
rollout-regression scenarios, and first on at least 70% — is the measurement that decides
whether tuning is needed, and `aisre fixture verify --report` is what measures it.

### Parameters

| Parameter | Default | Where it comes from |
|---|---|---|
| `reference_at` | `t2`, the end of the window | `DiffRequest.reference_at`, `--reference` |
| `tau_seconds` | `1800` (30 minutes) | `DiffRequest.tau_seconds`, `--tau 30m` |
| `kappa` | `0.5` | published constant; the post-reference ceiling |
| `tau_post` | `tau / 2` | derived; the post-reference decay constant |
| `change_hop_margin` | `1` | `DiffRequest.change_hop_margin`, `--change-margin` |
| `hops` | `2` | `DiffRequest.subgraph.hops`, `--hops` |
| `hop_cap` | `hops + change_hop_margin` | derived; the furthest an attached target can be |

`tau` is the decay constant, not a half-life: at `dt = tau` a change has decayed to `1/e`
(0.368) of its temporal weight, at `2*tau` to 0.135. Widen it when you are investigating a slow
burn — a memory leak that started an hour before the page — and narrow it when the failure was
instantaneous.

`reference_at` exists because the end of a diff window is not always the moment that matters.
"What changed between 13:00 and now, ranked against when the alert fired at 14:21" is
`--from 13:00 --to now --reference 14:21`. Feature 002 uses it that way: an investigation sets
`reference_at` to the estimated **symptom onset**, not to the alert instant and not to the end
of the window, which is why the post-reference side of the curve exists.

### Components on every item

```proto
message RankedChange {
  NodeVersion change = 1;              // the change node itself: kind, summary, actor, origin_ref
  double score = 2;
  double temporal = 3; double topological = 4; double traffic = 5;
  uint32 hop_distance = 6;             // h, as used by the formula
  int64 time_distance_seconds = 7;     // max(dt, 0) — the floored distance, unchanged
  bool unattached = 8;                 // the graph could not place this change
  repeated string target_entity_ids = 9;
  string tie_break = 10;               // "change_id"
  int64 signed_time_distance_seconds = 11;  // dt, signed, NOT floored
  bool post_reference = 12;            // true iff signed_time_distance_seconds < 0
  ActorKind actor_kind = 13;           // passthrough of Change.actor_kind
}
```

`target_entity_ids` are canonical ids. The answer describes each of them once, beside the ranked
list, in `DiffResponse.change_targets`: the target's node version at t2, or at t1 for a target
that no longer existed at t2 (004 T156). This is needed because a target is usually the one
entity in the window that did not change, so no node delta carries it. A reader holding only the
ids could not say which service a change landed on, and the investigation engine tested such a
change against the subject instead.

`hop_distance` and `signed_time_distance_seconds` are the values the formula was evaluated at,
not approximations of them, so every component can be recomputed from the published fields.
`time_distance_seconds` (field 7) keeps the floored meaning it has always had — `max(dt, 0)` —
so a consumer written against the first published version of this message sees exactly what it
saw before; the signed value travels beside it in field 11. `actor_kind` is carried from the
change node unchanged: the score does not weight it, and the ranker never derives or guesses
one (see `docs/schema/queries.md` and ADR-0005 D1). The
components are rounded to six decimals, and the ordering is over the rounded score: goldens are
compared byte for byte and `exp` is free to differ in its last bit between architectures, so a
ranking that flipped between an arm64 laptop and an amd64 CI runner would make the whole
fixture harness meaningless. Six decimals is far finer than any distinction the formula can
support and far coarser than any error it can accumulate.

`DiffResponse.ranking_formula` renders the formula with the parameters that particular answer
used, reference instant included.

---

## Hop distance and path weight

`h` is the shortest hop distance from the focus to the **nearest** target of the change, over
the union of the edges valid at T1 and at T2. A change with several targets — an IaC apply that
touched six resources — is as close to the focus as the closest thing it touched.

`w` is the heaviest traffic weight class on the way there: the maximum, over the shortest paths
to that nearest target, of the maximum class along the path. Edge types other than `calls` carry
no weight class and count as 0, so an ownership or configuration path ranks on hop distance
alone (research §8, §9). A change on the focus itself is `h = 0`, `w = 0`: `topological` is 1
and the traffic term contributes its floor, which is right — there is no path to weigh.

The union of both sides is used on purpose. A change is ranked against the *shape* of the
neighbourhood, not against one instant of it: an edge that existed at T1 and was retracted by
T2 still says the two services were related while the window ran. Where an edge exists on both
sides, its T2 weight class is the one used, since the ranking is a statement about the state at
the reference instant.

---

## Which changes are candidates at all

A change is a candidate when its valid interval intersects `(t1, t2]` **and** one of:

1. it has a `changed_by` edge from a node within `hops + change_hop_margin` of the focus; or
2. it is attached to nothing at all — every target it names is unresolved (see below).

The margin is what FR-027 calls the "stated extra hop margin". It widens the search for change
*targets* only: the extra ring never adds nodes or edges to the diff itself, it only makes a
change one hop outside the neighbourhood eligible to be ranked. One hop is the default because
the thing that broke you is usually next to you, and a wider net mostly adds noise the ranking
then has to push back down. `--change-margin 2` widens it; the maximum is 4.

A change attached to a node *outside* that radius is excluded rather than ranked low. The graph
knows where that one landed, and it did not land here.

### Window boundaries

A point-in-time change is stored over `[t, t + 1µs)`, the shortest non-empty interval PostgreSQL
can hold, so that a past change does not intersect every future window (research §4). Two
consequences:

- a change at exactly `t2` is **in** the window — "what changed by 14:32" has to include what
  happened at 14:32;
- a change at exactly `t1` is also **in**, because its interval runs a microsecond past `t1`. It
  will appear twice in the answer: as the state it produced (in the "before" side of the diff)
  and as the event that produced it. That is the forgiving side of the boundary to be on for a
  window an operator picked by hand.

---

## Unattached changes

A change whose target the graph cannot resolve to any node is **never dropped** (FR-028, US2
scenario 3). The projector records the unresolved names on the change node as
`sre.change.unattached_targets` and attaches the change later if the target ever shows up
(`internal/projector/attach.go`). Until then the diff reports it with:

```
unattached  = true
h           = hop_cap + 1        one hop beyond the furthest attached change could be
w           = 0                  there is no path, so there is no traffic on it
target_entity_ids = []
```

Because `h` is beyond the reach of any attached candidate, an unattached change always ranks
below an attached change of the same age — and always above nothing at all. With the defaults
(`hops = 2`, `margin = 1`) `h = 4`, so `topological = 0.2` and `traffic = 0.167`.

A change that names an unresolved target *and* has a resolved one is not unattached: it is
ranked from the target the graph could place.

### When it attaches, and what the edge then says

The adoption convention, published because a connector must not invent its own (004 FR-018, SC-018):

- **The trigger is entity creation, not a polling cycle.** `attachWaiting` runs whenever an entity is
  created — from `createEntity` and from an entity split, which are the only two places entities are
  created, through one entry point so a future node kind that can dangle cannot be added to only one
  of them.
- **The bound is the same transaction.** The change is adopted in the transaction that creates its
  target, so there is no window in which the target exists and the edge does not. SC-018's "within one
  polling interval" is met with room to spare; the interval that actually matters is how long until
  the *target* is observed, which is the feeder's cadence rather than the graph's.
- **The edge carries both halves of its provenance**: the event that observed the change and the event
  that finally created the node (FR-034). An operator asking why the edge exists sees the whole story
  rather than the later half, and the change node is corrected at the same time — the target leaves
  its unattached list as a normal correction, old version closed, new one opened.
- **Identifiers are recorded as a property, not as claims.** FR-018 asks for every unresolvable
  identifier to be *recorded*, and `sre.change.unattached_targets` is a stated fact about the change:
  "this change names something the graph has never seen." A claim would mean something else — that
  the change *is* that thing — and would put a rollout and the service it changed on a path to being
  merged.

A rule whose answer depends on what a change is attached to must be re-asked when the change attaches,
because adoption creates the edge without re-running resolution. That is what feature 004's deferred
re-evaluation queue does for C8; see `internal/projector/retrigger.go`. Alerts take the same adoption
path and need no such thing, because no rule keys on what an alert watches.

---

## Tie-break

Ties are broken by the change's canonical entity id, ascending, and every item carries
`tie_break = "change_id"` so the rule travels with the answer rather than living only here.

Two changes at the same instant, at the same distance, over the same path are genuinely
indistinguishable to the score. What matters is that two runs of the same query agree — a
fixture golden is compared byte for byte, and an agent reasoning over "the top three" must get
the same three every time.

---

## Worked example

From `fixtures/rollout-regression-01`, the shipped rollout-regression scenario. `payments` is
rolled out to revision 7 at 14:20; the question is asked of `checkout`'s 2-hop neighbourhood
between 13:00 and 14:32, with the defaults (`tau = 1800 s`, `margin = 1`, so `hop_cap = 3`,
reference = `t2` = 14:32).

```sh
aisre query diff otel.service.name=checkout \
  --from 2026-09-01T13:00:00Z --to 2026-09-01T14:32:00Z --hops 2
```

| rank | change | dt | h | w | temporal | topological | traffic | score |
|---|---|---|---|---|---|---|---|---|
| 1 | `k8s.change=shop/payments@rev7` (rollout) | 720 | 1 | 3 | 0.670320 | 0.500000 | 0.666667 | **0.610160** |
| 2 | `k8s.change=shop/storefront@scale-1415` (scaling) | 1020 | 1 | 4 | 0.567414 | 0.500000 | 0.833333 | **0.583707** |
| 3 | `k8s.change=shop/checkout-worker@rev12` (unattached) | 600 | 4 | 0 | 0.716531 | 0.200000 | 0.166667 | **0.453266** |
| 4 | `k8s.change=shop/inventory@rev8` (rollout) | 4920 | 1 | 3 | 0.065002 | 0.500000 | 0.666667 | **0.307501** |

The arithmetic for the first three, written out:

```
payments   temporal = exp(-720/1800)  = exp(-0.400000) = 0.670320
           topological = 1/(1+1)                       = 0.500000
           traffic = (1+3)/6                           = 0.666667   checkout -> payments is class 3
           score = 0.5*0.670320 + 0.35*0.500000 + 0.15*0.666667
                 = 0.335160     + 0.175000      + 0.100000         = 0.610160

storefront temporal = exp(-1020/1800) = exp(-0.566667) = 0.567414
           topological = 1/(1+1)                       = 0.500000
           traffic = (1+4)/6                           = 0.833333   storefront -> checkout is class 4
           score = 0.283707     + 0.175000      + 0.125000         = 0.583707

worker     temporal = exp(-600/1800)  = exp(-0.333333) = 0.716531
           topological = 1/(1+4)                       = 0.200000   unattached: h = hop_cap+1 = 4
           traffic = (1+0)/6                           = 0.166667
           score = 0.358266     + 0.070000      + 0.025000         = 0.453266
```

Three things this example is built to show:

- **The culprit wins on the balance, not on one term.** The scaling decoy is five minutes
  *fresher* than the rollout and sits on a heavier edge; it still ranks second, because the
  rollout's temporal advantage over it (0.103 of a point, halved to 0.052) outweighs the
  scaling's traffic advantage (0.167, weighted to 0.025). Had the scaling happened at 14:25
  instead of 14:15 it would have ranked *first* — which is the formula working as published,
  not a defect, and is why the fixture places it where it does. If your judgement is that a
  scaling event should never outrank a code rollout, that is an argument for a kind weight in
  v2, made with the fixtures in hand.
- **Unattached is included and demoted, not dropped.** The `checkout-worker` rollout is the
  freshest change in the window (0.717 temporal, better than the culprit's 0.670) and still
  ranks third, entirely because the graph cannot place it.
- **Distance excludes, it does not merely demote.** A fifth change in the window — a rollout of
  the unrelated `reporting` service at 13:40 — does not appear at all. `reporting` is four hops
  from `checkout` (`checkout → general → shop-prod → analytics → reporting`), past the two-hop
  neighbourhood and its one-hop margin.

`aisre fixture verify fixtures/rollout-regression-01 --report` prints the same scenario as a
metric: the culprit's rank, whether it made the top *k* the fixture's `ground_truth` asks for,
and how many candidates it beat.

---

## A reference instant with changes on both sides of it

The example above has the reference at the end of the window, where nothing can follow it. This
one is the case the two-sided curve exists for, and it is how feature 002 asks the question:
the same `checkout` neighbourhood, but ranked against an **estimated symptom onset of 14:21**
rather than against the end of the window.

Two candidates, one hop from the focus, either side of the onset:

- `payments@rev7`, a rollout at **14:20** — one minute *before* the onset, on a path of weight
  class 3;
- `storefront@scale-1423`, the autoscaler adding replicas at **14:23** — two minutes *after* the
  onset, reacting to the failure, on a heavier path of weight class 4.

```
rollout    dt = +60    temporal = exp(-60/1800)          = 0.967216
                       topological = 1/(1+1)             = 0.500000
                       traffic = (1+3)/6                 = 0.666667
                       score = 0.483608 + 0.175000 + 0.100000  = 0.758608

autoscale  dt = -120   temporal = 0.5 * exp(-120/900)    = 0.437587
                       topological = 1/(1+1)             = 0.500000
                       traffic = (1+4)/6                 = 0.833333
                       score = 0.218793 + 0.175000 + 0.125000  = 0.518793
```

| rank | change | dt | post_reference | temporal | score |
|---|---|---|---|---|---|
| 1 | `payments@rev7` (rollout) | +60 | false | 0.967216 | **0.758608** |
| 2 | `storefront@scale-1423` (scaling, `CONTROLLER`) | −120 | true | 0.437587 | **0.518793** |

Under the rule feature 001 first shipped, where `dt` was floored at 0, the autoscaler's temporal
term would have been **1.0** — the maximum — for a score of **0.800000**, and the change the
incident *caused* would have ranked above the change that caused it. That is the defect ADR-0005
D4 fixes, and it is why the ceiling is `kappa` rather than 1.

Two things to read off the table:

- **The flag is in the answer, not in the arithmetic.** A caller does not have to recompute
  anything to know which side a candidate is on: `post_reference` says so and
  `signed_time_distance_seconds` says by how much. That is what lets an engine type the
  autoscaler as a candidate *effect* rather than a candidate cause — and, with
  `actor_kind = CONTROLLER` on the same item, say why.
- **Demoted, not dropped.** The autoscaler is still second, still on the list, still carrying
  its components. A change after the onset is evidence about the incident even when it is not a
  cause of it.

## Excluded, which is not the same as demoted (003 FR-065)

There is one change the list leaves out rather than demoting, and it is worth being precise about why,
because it looks like a contradiction of the paragraph above.

An **announced** change whose valid interval begins after the reference instant has not happened. A
vendor maintenance window on 2 October, read on 17 September, cannot have caused an incident on 21
September — not "is unlikely to have", cannot. It appears in `DiffResponse.excluded_changes` with
`EXCLUSION_ANNOUNCED_AFTER_REFERENCE` and a detail naming the change's valid start and the reference it
was compared to, and it does **not** appear in `changes`.

The distinction from the autoscaler is whether the change happened:

| | happened, after the reference | announced, ahead of the reference |
|---|---|---|
| example | a rollback at 02:15 after a 02:10 alert | a vendor window on 2 October |
| treatment | ranked, temporal term decayed from `kappa` | excluded, with a stated reason |
| why | may be an effect or a remediation; an operator wants it | has not occurred; it is not evidence about this incident |

So the exclusion is keyed on **both** the announcement state and the instant. Keyed on the sign of the
time distance alone it would delete the two-sided decay; keyed on the state alone it would exclude a
maintenance window already under way, which is exactly the change an operator is looking for.

Two answers, not one: a ranked list with the October window at the bottom says *"this is the least
likely of the things that could have caused it"*. An exclusion says *"this could not have caused it, and
here is the instant that settles it"*. Only the second is actionable — and the first is how a
forward-looking read gets reached by accident from a backward-looking question, which is the failure
FR-065 exists to prevent. Asking what is *expected* to change is an explicitly forward query with a
future valid instant.

---

## Related

- `docs/schema/queries.md` — the query contract the diff is part of.
- `docs/schema/temporal-model.md` — valid time, observed time, and what "as known at" means.
- `specs/001-temporal-graph-core/research.md` §8 (weight classes), §9 (this score).
- `fixtures/README.md` — the fixture family this ranking is measured on.
