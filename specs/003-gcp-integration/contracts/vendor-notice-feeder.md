# The Vendor-Notice Feeder Contract

**Source id**: `vendor-notice:<org-slug>` | **Kind**: `vendor-notice` | **Feature**: 003
**Schema version**: `1.0.0` | **Ordering**: `none` | **Date**: 2026-09-20

The coverage audit's finding, in one line: **the single most repeated cause across the thirteen
incidents was a vendor notice nobody read** — a maintenance window, a deprecation or a provider
incident, announced by email weeks in advance and rediscovered at 02:10 under a page. This feeder
exists to put that announcement in the graph on the day it arrives, as a change whose valid time
is the window the vendor announced.

It is a `pkg/feeder.Feeder` like any other. What is unusual about it is entirely in two places:
the facts it emits are about the **future**, and its input is **untrusted text from outside the
organisation** that must never be stored.

---

## 1. Three sources, one shape

| source | cadence | what it yields |
|---|---|---|
| **shared mailbox** | polled; read-only and **state-preserving** (FR-007) | maintenance, deprecation and incident emails |
| **status-page feeds** | polled at a configurable cadence, respecting any limit the publisher states, identifying itself honestly (FR-075) | provider-declared incidents and scheduled maintenances |
| **changelog feeds** | as above | announced removals and breaking changes |

Each is independently enableable and the feeder works with **any subset** present (FR-058). A
source that is unreachable produces a **gap in the checkpoint, not silence** (FR-075) — the
distinction between "the vendor announced nothing" and "we could not ask".

All three are implementations of `pkg/feeder.Source`, so recorded mode is the same code path and
FR-008's "nothing may derive from the connection itself" holds structurally.

### 1.1 The mailbox is read-only in a stronger sense than usual

The feeder must not send, reply, forward, delete, move, archive, label **or change the read state
of** any message (FR-007). The last one is the trap: on most mail APIs, *reading a message is a
write*. Where the access mechanism would alter message state as a side effect of reading, the
feeder **uses a mode that does not, or does not read at all** — there is no third option and no
configuration flag that permits one. The per-path guarantees are research §9.

---

## 2. The allowlist is the filter, and the drop is visible

The feeder acts on an explicit **allowlist of vendors and their products** (FR-059). The shared
mailbox is an ordinary mailbox that receives provider mail, and *most of what arrives in it is not
a notice* — so the allowlist and the extraction-failure path carry real traffic and are not edge
cases.

An announcement whose vendor or product is not on the allowlist is **dropped, counted, and visible
as a drop with its reason** in the feeder's own operational telemetry. It is not emitted "just in
case": an unused product's maintenance window is noise that dilutes the one signal this feeder
exists to carry.

Adding a vendor is **configuration, not a code change** (Assumptions). The published default is
drawn from the organisation's actual providers: an LLM API provider, a GPU host, a
voice-infrastructure vendor, a serverless-GPU vendor and the ATS vendors.

---

## 3. Extraction: typed, validated, and never an instruction

**Announcement text is untrusted input from outside the organisation** (FR-073). Anyone who can
send mail to the shared mailbox can put text in front of this feeder, and a status page is a third
party's mutable document. Three rules follow:

1. Extraction produces values **validated against the published typed schema**. The extracted
   fields are: vendor, product, announcement kind, announced window, affected resources, and the
   vendor's notice identifier where it states one.
2. **No text from any announcement is ever treated as an instruction by any component of the
   system.** The typed extraction boundary is the same boundary that governs what may be sent to a
   model provider (FR-141) — nothing crosses it that would not be permitted into a recording.
3. An extraction that cannot produce a valid typed result **fails loudly, naming the field**,
   rather than emitting a guess (FR-073).

An announcement the feeder cannot confidently place — no recognisable vendor, no window, no
product — is recorded as **unextracted** with its reason and its message pointer, so a human can
see what the feeder could not read. It is **not emitted as a change** (FR-074).

---

## 4. What survives a message, and what does not

**No message body may ever be stored** — in the graph, on disk, in a log, or in any artifact, at
any point, **including on a failed or aborted run** (FR-071). What survives is:

| kept | dropped |
|---|---|
| the six extracted typed fields | the body |
| a **bounded derived summary**, built from the extracted fields and subject to the same redaction rules and scans as any other committed content | sender and recipient addresses, display names |
| a **pointer to the message identifier**, so a human can open the original in the mailbox | subjects copied verbatim, quoted threads |
| | attachments — **not opened** and not stored in v1 |

The derived summary is built **from the extracted fields**, not from the text. That is the whole
difference between a summary and an excerpt, and it is why the summary can be committed at all.

The corresponding success criterion is absolute: **zero** message bodies, addresses, display
names, verbatim subjects or attachments reach the graph, disk, a log or any artifact over the whole
corpus including aborted runs (SC-010).

---

## 5. The event: an announced fact

Each in-scope announcement becomes a CHANGE node with actor kind **`VENDOR`**, an actor naming the
vendor, a link back to the announcement, and a kind:

| announcement | `ChangeKind` |
|---|---|
| a maintenance window | `CLOUD_MAINTENANCE` |
| an announced removal or breaking change | `DEPRECATION` |
| a provider-declared incident | `VENDOR_INCIDENT` |

> **The spec's fallback is dead.** FR-060 says that *until the published taxonomy names
> `deprecation` and `vendor_incident`, emit them under `CHANGE_KIND_OTHER` with the vendor-stated
> kind recorded*. The taxonomy named them on 2026-09-18 (ADR-0006 D3:
> `ChangeKind.DEPRECATION = 10`, `VENDOR_INCIDENT = 11`). The fallback **must not be used**, and a
> fixture asserting an `OTHER`-kind deprecation would be asserting a regression.

**Ref**: `vendor.notice` = `<vendor>/<notice-identifier>`, or — absent a vendor-stated identifier —
derived deterministically from the source, the product and the announced window (FR-076). This is
what makes re-reading a no-op and what makes the same announcement from two sources addressable as
one change.

**Targets**: the **THIRD_PARTY node for the vendor** (FR-061). Where the announcement names
specific affected resources — a region, an API endpoint, a model or product name — those are
emitted as **identity claims** so the resolution layer can attach them. The feeder does **not**
invent a `depends-on` edge from a service to a vendor, and does not duplicate one the graph already
holds.

**Valid interval**: the window the vendor announced, and §6.

---

## 6. Facts about the future

The full state machine, its five rules and the two non-transitions are
[`data-model.md` §5](../data-model.md#5-the-announced-fact--this-features-first-state-machine).
The contract-level statement is four lines:

- the valid interval **is** the announced window, emitted unchanged even when it starts after the
  read instant, and no ingestion rule may reject it (FR-062);
- **observed time is never moved forward** to match it (FR-063);
- a change whose valid start is after a ranked list's reference instant is **not a candidate
  cause, and is excluded for that stated reason** rather than by scoring low (FR-065, SC-007);
- a cancellation or a reschedule is a **correction of the same change** — the observation closes, a
  new one opens, the valid interval is never rewritten, and **no second change node is created**
  (FR-067, FR-068, SC-008).

An **announcement state** — `announced`, `confirmed`, `cancelled`, `superseded` — is carried on
every announced change, defaults to `announced`, and is never promoted to `confirmed` without an
observation that the window happened (FR-066). This vocabulary does not exist in the schema yet; it
is item 4 of the plan's [§What feature 001 still owes this feature](../plan.md#what-feature-001-still-owes-this-feature),
and User Story 2 is not complete until it lands.

---

## 7. Duplicates: one announcement, two sources, one change

The same maintenance window announced by **both** email and the vendor's status page must produce
**exactly one** change node (FR-070, SC-009). The mechanism is the one the constitution requires
(VI) and not the one that is easier:

- the feeder emits **identity claims** naming the vendor, the product, the announced window and the
  vendor's own notice identifier where it states one;
- **the published rules merge them. The feeder merges nothing itself**;
- the feeder does **not suppress its own observation** because another source may carry the same
  announcement — suppression is a silent merge with no audit trail;
- where no shared identifier exists, the pair is raised as a **resolution suggestion** with its
  score and rationale.

The two published rules are **C1** and **P6**. When both sources state the same vendor, product and
window, the composite identifier is the same string and C1 — two sources asserting one identifier —
merges them; that is the ordinary case and it is certain. When the windows differ, nothing shared is
left, and P6 suggests the pair at score 0.5: same vendor, same allowlisted product, windows that are
not disjoint, and no notice identifier in common.

P6 can only ever suggest, because the case it cannot tell apart from a duplicate is a real one — a
vendor announcing two separate maintenance windows for one product in the same month. A rule that
merged those would hide one of them, which is the same harm as the duplicate, arriving silently.

The failure this guards against is concrete and visible: two adjacent entries for one window in the
ranked change list, at 02:10, in front of someone who is being paged.

---

## 8. The vendor node

The feeder creates a THIRD_PARTY node for every allowlisted vendor it has an announcement for, if
one does not already exist, carrying the vendor's name and its allowlisted products, and emits
identity claims **including the host names the allowlist maps to the vendor** (FR-077).

Those host-name claims are what satisfy FR-119's *certain* resolution rule: when an observed
outbound dependency's server address matches a host name the allowlist maps to a vendor, the vendor
the notices are about and the third party the graph already observed from traffic are one entity.
The rule is **C6**, and it is certain because it rests on **a configured assertion**, not on a
resemblance — somebody wrote that host name next to that vendor on purpose.

The host claims carry `sre.vendor.allowlisted_host`, valued by the vendor slug, and that marker is
what C6 orients on: exactly one side of the pair must carry it. Without it the rule cannot tell the
allowlist's assertion from an observation of the same host, and two entries mapping one host to two
vendors is a **disagreement** for a person to settle rather than a merge to make (FR-122). C6 ranks
above C1, which also fires on the pair, so the recorded reason names the allowlist rather than
reading "two sources used the same string" — a merge a reviewer cannot check is a merge nobody
reviews.

---

## 9. Per-cycle reporting

The feeder reports, per cycle (FR-078): announcements **read**, **extracted**, **dropped by
allowlist**, **failed to extract**, and **merged**.

The purpose of each number is the same: so that *"the graph knows about no upcoming vendor
change"* can be distinguished from *"the feeder read nothing"*. Those two are indistinguishable
from the graph alone, and only one of them is good news.

---

## 10. Conformance

| gate | assertion |
|---|---|
| replay | every fixture replays from empty to its golden |
| idempotency | double delivery is a no-op; re-reading an announcement changes nothing (FR-076) |
| shuffle | permuting inside the declared reordering window changes no valid-time state |
| announced-fact | a query as of a valid instant inside the announced window, observed after the read, returns the change; as of an earlier valid instant it does not (SC-006) |
| correction | after a cancellation or a reschedule, an observed-time query before the correction still reports the prior belief; zero valid intervals are rewritten (SC-008) |
| duplicate | a two-source announcement yields exactly one change node, merged by a published rule and not by the feeder (SC-009) |
| containment | zero bodies, addresses, display names, verbatim subjects or attachments anywhere, including on aborted runs (SC-010) |
| allowlist | a non-allowlisted notice emits nothing, is counted, and the reason is visible |

The campaign must include at least one of each of: a future-dated maintenance window, a
deprecation, a provider incident, a **cancellation**, a **reschedule**, and a **duplicate delivered
by two sources** (FR-129). The last three are the ones a synthetic corpus would not have thought
to include, and they are where the design is either right or quietly wrong.
