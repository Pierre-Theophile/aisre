<!-- SPDX-License-Identifier: Apache-2.0 -->

# Announced facts

An **announced fact** is a fact whose valid time begins after its observed time: a maintenance window
read on 17 September for 02:00–04:00 on 2 October. The graph has always been able to store one —
valid time and observed time are independent axes — and feature 003 is the first consumer to emit
them, from two sources that know nothing about each other: a vendor's notice feed, and a Cloud SQL
instance's `scheduledMaintenance.startTime`.

This document is the semantics both of them implement. It is here rather than in either connector
because the semantics belong to the **data model**: a third source that starts announcing things
implements this page, and does not get to invent its own state machine.

## 1. The asymmetry, and it is one-directional

Valid time may lead observed time. **Observed time is never in the future.**

That asymmetry is the whole of it, and the way it gets broken is not malice: a feeder reads a window
starting on 2 October, notices that its own observation instant is "before" the thing it describes,
and helpfully stamps the observation at the window's start so the two line up. The graph then says
the organisation knew on 2 October what it actually knew on 17 September, and every
"what did we know at the time?" query answers wrongly for a fortnight.

`vendornotice.AssertObservedNotInFuture(observedAt, now)` is the check, it takes the wall clock
explicitly so a fixture can assert it at a fixed instant, and it has exactly one implementation —
`internal/feeders/gcp` **calls** it rather than restating it.

## 2. The valid interval is the announced window, emitted unchanged

Nothing clamps it to now and nothing rejects it for being ahead. A window that begins in eleven days
is stored beginning in eleven days.

Where the vendor announces a start and no duration — which is what Cloud SQL does — the interval is
the announced instant and **no end is invented**. The graph gives a change with no end the shortest
non-empty interval, which is the right default: it does not intersect every future diff window, and
it does not claim a duration nobody stated.

Where the vendor is vague about the start — "in a future release" — the start is **marked unknown**
and the vendor's wording is never stored in its place. The marker matters more than it looks: without
it the change reaches the graph with no `valid_at`, which reads as the zero timestamp and lands the
announcement in 1970. That is not merely wrong but *plausibly* wrong — an ancient change ranks as
maximally distant, is never excluded as a future announcement, and looks like a fact.

## 3. Reading one is an ordinary bitemporal query

A read as of valid time `T_v` and observed time `T_o` returns the change when `T_v` is inside the
window and `T_o` is at or after the observation. There is no special case, no flag to pass, and no
second code path — which is the point of storing it this way rather than as an annotation.

## 4. The ranker excludes it, and says so

A change whose valid start is **after** a ranked list's reference instant is not a candidate cause,
and it is excluded **for that stated reason** rather than by scoring low. The distinction is
operational: a low score means "we considered it and it is unlikely", and an exclusion means "this
had not happened yet". A reader who cannot tell them apart will spend the incident wondering why the
obvious cause is ranked eighth.

## 5. The state machine

| state | what it means | who may set it |
|---|---|---|
| `ANNOUNCED` | the vendor said it will happen | the reader of the announcement |
| `CANCELLED` | the vendor withdrew it before it arrived | the reader of the withdrawal |
| `SUPERSEDED` | the vendor moved it; this observation carries the **original** window | the reader of the reschedule |
| `CONFIRMED` | somebody **observed** the window happen | only a caller that watched production |

`ANNOUNCEMENT_STATE_UNSPECIFIED` is the zero value and is **not** a synonym for `ANNOUNCED`. An
ordinary observed change leaves it unset, canonical serialisation omits it, and every change recorded
before the field existed serialises exactly as it did.

### A cancellation is a correction, not a retraction

This is the rule that is easiest to get wrong and hardest to notice afterwards.

A cancellation carries the **same ref** and the **same valid interval** as the announcement it
corrects. The observation closes, a new one opens, and the valid interval is never rewritten — so
*"what did we believe on 20 September about 2 October?"* stays answerable by an observed-time query.

A retraction would say the window stopped being true. The window was never true: it was *announced*,
and then withdrawn. Those are different facts and only one of them happened.

A reschedule is the same shape: the `SUPERSEDED` observation carries the original window, the new
`ANNOUNCED` observation carries the new one, and both stay recoverable. Whether the two share a ref
depends on whether the vendor names its notice — see §6.

### Nothing is promoted by silence

An announced window whose end has passed is **still announced**. The passage of time is not an
observation, and a feeder that promoted on it would be claiming the maintenance happened — which
nobody watched. `vendornotice.AssertNotPromotedBySilence` is the check.

The Cloud SQL connector states it more strongly still, and the reason is worth reading as an example
of when *not* to correlate: GCP publishes **no identifier** linking a `scheduledMaintenance`
announcement to the `MAINTENANCE` operation that later performed it. A tolerance window matching the
two would be a guess, and a guess that promoted an announcement would assert that a *specific*
announced window is the one that took place. So the connector emits the past maintenance as its own
observed change, never promotes, and **states the gap** on the change rather than closing it by
inference.

## 6. Identity, and what a reschedule does to it

An announcement's ref is deterministic, so that re-reading the same announcement is a no-op and the
same announcement from two sources is addressable as one change.

- Where the vendor **states an identifier**, that is the ref, and a reschedule keeps it: the vendor
  said these are the same notice, by reusing its own name for it.
- Where the vendor states none, the ref is derived from the facts — product, kind, window — so a
  different window is a **different change**. Rescheduling from "soon" to a date is a correction of
  the same notice only when the vendor says so, and the vendor says so by reusing its identifier.

GCP states no identifier for a scheduled maintenance, so the Cloud SQL connector follows the second
rule: the old window is observed again as `SUPERSEDED` carrying its original interval, and the new
window opens as a new `ANNOUNCED` change under its own ref. Both beliefs stay recoverable, which is
the property a reschedule has to preserve.

The derived form deliberately **excludes the pointer**. A message id differs between two mailboxes
carrying one notice, and a feed entry's id differs from an email's; including it would make two
readings of one announcement two changes — two adjacent rows for one window, in front of somebody
being paged.

## 7. Where this is implemented

| piece | file |
|---|---|
| the state machine and the two invariant checks | `internal/feeders/vendornotice/announce.go` |
| the Cloud SQL announced maintenance, calling those checks | `internal/feeders/gcp/cloudsql.go` |
| the field on the event body | `pkg/feeder.ChangeFact.AnnouncementState` |
| the ranker's exclusion | `internal/query` |

There is **one** implementation of each invariant. `internal/feeders/gcp` imports
`internal/feeders/vendornotice` for the two checks rather than restating them — an unusual dependency
for one connector to take on another, and the point of it: a future edit to either rule cannot leave
two connectors disagreeing about what an announcement means.

## 8. The fixtures

| fixture | what it pins |
|---|---|
| `vendor-maintenance-future-01` | a window read a fortnight early; the same graph read as-of the notice and as-of the window |
| `vendor-cancellation-01` | the correction, on the same ref, with the interval unchanged |
| `vendor-reschedule-01` | superseded carrying the original window, announced carrying the new one |
| `vendor-duplicate-two-sources-01` | one notice from two sources merging by the published rules, not by a feeder suppressing itself |
| `gcp-cloudsql-scheduled-maintenance-01` | the same announced-fact shape from a **platform** connector instead of a notice reader |

The last one is there deliberately. If the semantics were the vendor-notice connector's rather than
the data model's, the platform connector would have needed its own — and the fixture asserts the same
query behaviour from a different source, which is what says the semantics are shared.
