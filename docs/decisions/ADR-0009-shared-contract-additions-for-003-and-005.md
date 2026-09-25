# ADR-0009: The shared contract additions for features 003 and 005

- Status: **Accepted** (2026-09-21)
- Deciders: project owner (approval), Claude (design, documented here)
- Source: `specs/003-gcp-integration/plan.md` §"What feature 001 still owes this feature",
  `specs/003-gcp-integration/contracts/pointer-vocabularies.md`,
  [ADR-0005](./ADR-0005-shared-contracts-for-002-to-005.md),
  [ADR-0006](./ADR-0006-feature-001-change-package.md). Written under
  `specs/003-gcp-integration/tasks.md` T008.

## Context

Feature 003 is the first **vendor** connector: a GCP feeder, a GCP telemetry backend and a
vendor-notice feeder. Seven things it needs are not its to invent — they are additions to the
**published** schema (constitution IX), and five of them are needed identically by feature 005
(Datadog).

This is one decision record rather than seven because the alternative is seven ADRs whose context
sections are the same paragraph, and because they land as one `buf breaking` run: either the
package is additive or it is not, and that question is asked once.

**Two of the seven were not inherited from the spec's dependency list.** Items 6 and 7 were found
by reading the shipped `investigation.proto` against FR-100 and FR-104 and noticing that two
requirements had nowhere to put their answer. That is recorded here because it says something
about method: the dependency list was written from the spec, and reading the spec against the
*code* found things the spec against the *plan* did not.

## Decision

**All seven land additively. `sreagent.graph.v1` and `sreagent.investigation.v1` each take a MINOR
bump, and no event-log transformation is registered** (`internal/log/migrate` stays `1.0.0 →
1.0.0`). `buf breaking --against '.git#branch=main'` reports every addition as additive, and no
golden moves, because every new field is absent from canonical JSON when unset.

### Item 1 — three GCP pointer vocabularies

`gcp-monitoring-filter/v3`, `gcp-logging-query/v2`, `gcp-trace-filter/v1`, registered in
`pkg/feeder/pointer.go` with their selector grammars in `docs/schema/pointers.md`.

**This is item one because nothing else in the telemetry path can run without it**: the backend
cannot execute a pointer whose vocabulary is unregistered, and it may not translate one into
something it does understand.

All three make the same split — the **selector** is in GCP's own query language because it is what
will actually be executed, and the **attributes** stay in OpenTelemetry semantic conventions so a
reader can tell which *entity* a pointer is about. The entity is portable; the query is not.
Constitution IV requires the "why not OTel" paragraph, and for the Logging query language it is
substantive rather than formal: severity comparison, RE2, field-existence tests and boolean
structure have no attribute-equality form, and flattening a severity range into an equality set
would change what a stored pointer matches the next time Google adds a severity level.

`gcp-trace-filter/v1` is registered and **deliberately unminted**: this organisation has no trace
data source, and FR-085 requires that absence be a stated fact rather than a fabricated pointer.
Registering the name now is what lets `error_spans` go live with no schema change if Cloud Trace is
ever enabled.

### Item 2 — the proposed dependency

`ProposeDependency`, with `ConfirmDependency` and `RejectDependency` as its two terminal decisions;
`graph.proposed_dependencies` (migration 0010); a `ProposedDependencies` query RPC and the two
decision RPCs.

Resolution could already suggest that two **refs** name one **entity**. Nothing could suggest that
an **edge** exists between two **different** entities, so a feeder holding suggestive but
inconclusive evidence had two options and both are worse than a stated gap: assert the edge anyway,
or drop the evidence.

**The decision bodies were not in the original item.** The item named the proposal alone, but the
requirement that a decision "persist across replay and outrank any automated match" (FR-029,
FR-122) cannot be met by a row updated in place — the next replay recomputes it — and
`ConfirmMerge`/`RejectMerge` are typed for entity *identity*, so reusing them would file a
dependency decision in the identity ledger. The scope grew by two messages for a reason that is
recorded here rather than absorbed silently.

The invariant: **a proposal is never an edge until a person confirms it.** No query traverses the
table and only `ConfirmDependency` writes an edge.

### Item 3 — the `sampled` marker on alert state history

`AlertTransition.sampled` and `sampled_interval_seconds` (FR-051).

A webhook-delivered transition history is complete; a **polled** one is a sequence of observations
at an interval, and any transition that opened and closed between two polls is invisible. Both
arrive as `AlertTransition`, and with no marker a reader could not tell them apart — so a gap in a
polled history read as "the alert did not fire" when it meant "we were not looking when it did".
It is the distinction `source_checkpoint.gap_before` draws for a feed, drawn for one alert.

### Item 4 — the announcement-state vocabulary

`AnnouncementState` — `ANNOUNCED`, `CONFIRMED`, `CANCELLED`, `SUPERSEDED` — on `Change`
(FR-062–FR-068).

It keeps an announcement from being read as an occurrence. A maintenance window read on 17
September for 2 October is a fact about the future, and a ranker treating it as an occurrence would
implicate a window that has not run. The transitions are corrections in the constitution's sense:
a reschedule becomes `SUPERSEDED` plus a new `ANNOUNCED` observation of the **same** change ref,
never a rewritten valid interval.

`ANNOUNCEMENT_STATE_UNSPECIFIED = 0` means *not an announcement at all*, deliberately **not** a
synonym for `ANNOUNCED` — the same rule ADR-0006 applied to `ACTOR_KIND_UNSPECIFIED`, and what
keeps every change ever recorded serialising exactly as it did.

### Item 5 — unattachment and auto-attach for alerts

An alert whose watched entity has not been observed records it as unattached and links it when the
entity appears, with the same two-sided provenance the change path records (FR-048).

**This one is not purely additive, and it is the only item in the package that is not.** It changes
behaviour that feature 002 shipped and tested.

Before: an unresolved `watches` ref went through `applyUpsertEdge`, which mints a **placeholder**
endpoint for an entity nothing has described. So the WATCHES edge existed immediately, pointing at
an entity with no type and no display name. `TestAlertTransitionIsAnOrdinaryNodeAssertion` asserted
exactly that.

After: no edge until the target exists; the alert carries `sre.alert.unattached_watches`; the edge is
created on arrival, dated from the alert's earliest current version and naming both the transition
that wanted it and the event that created the target.

The reason to change it rather than add the marking alongside the placeholder — which would have been
additive and would have kept the 002 test green — is that an edge to a placeholder is **worse than no
edge**, in one specific way. An investigation walking WATCHES out of a firing alert cannot tell
*"watches a service we know about"* from *"watches a name we have never seen"*: both are an edge to
an entity, and the difference is only visible to something that goes and inspects the endpoint.
FR-048's "marked unattached" exists so the gap is a stated fact, and a graph that cannot say what it
does not know will be read as knowing.

Two design points that are not obvious and are easy to get wrong:

- **The edge is dated from the alert's earliest current version, not from the segment that carries
  the unattached list.** An alert's *state* is segmented in valid time; *what it watches* is not. An
  alert that fired and recovered before its target appeared has two current versions, and dating the
  edge from whichever one still carries the list dates it from the **last** transition — so the
  graph would say the alert started watching at its recovery.
- **The waiting query is bounded by observed time** (`lower(observed) <= observedAt`). Attaching
  rewrites the waiting node, and closing an observed interval before it opened is refused by the
  database — reachable whenever a node-creating event is observed *earlier* than the version waiting
  for it, which a late-arriving fact does routinely. The same hazard was **latent in the change
  path** and simply unexercised; the guard covers both.

### Item 6 — `Digest.vendor_request_id`

FR-100 requires every digest to carry its query, its execution instant **and the platform's request
identifier**. The first two had homes; the third had none.

Deliberately not `coverage.data_source`, which names a metric namespace rather than one request,
and deliberately not `free_text`, which is flagged unverified and never citable on its own
(002 FR-014b) — a request identifier is the most checkable fact in a digest, and the one field that
cannot be cited would make it useless for what it exists for: taking a digest to the vendor's
support and asking what happened on *that* call.

### Item 7 — `FailureReason.OUTSIDE_RETENTION`, with `AlgebraResponse.retention_horizon`

The mirror of `NOT_YET_INGESTED` for a window too **old** rather than too new (FR-104).

The distinction from `NO_DATA` is the entire point. `NO_DATA` means the backend looked, the window
was covered, and nothing was there — a finding. `OUTSIDE_RETENTION` means the window was **never
covered**, because the data aged out before anyone asked. Reporting the second as the first tells
an investigation that nothing happened during exactly the window it cannot see, which is the most
confident wrong answer this system can produce. Cloud Run's `run.googleapis.com/*` metrics retain
for six weeks, so it binds immediately rather than theoretically.

`retention_horizon` was added alongside it and was not in the item: without it, the refusal says
"not this window" and leaves the caller to find the boundary by bisection, spending quota to learn
a number the vendor already publishes.

### Item 8 — `DiffResponse.excluded_changes`, and `ExclusionReason` (added during US2)

FR-065 says a change whose valid start is after a ranked list's reference instant is not a candidate
cause and is **excluded for that stated reason** rather than by scoring low. The plan called the rule a
contract and left the mechanism unstated, so nothing carried the statement: a response could only rank
a change or silently omit it, and "omitted with no reason" is the one answer an operator cannot act on.

`ExcludedChange` carries the change, an `ExclusionReason` and a detail naming the two instants the
decision was made on, so a reader can check it without recomputing. It is a separate field rather than
a flag on `RankedChange`, because an excluded change is **not ranked** — giving it a score is the
behaviour the requirement exists to forbid.

The exclusion is keyed on the announcement state **and** the instant, which is what reconciles FR-065
with ADR-0005 D4's two-sided decay. D4 keeps a change that happened after the reference, at a decayed
score, because it may be an effect or a remediation and an operator wants it. FR-065's case is a change
that has not happened at all: an ANNOUNCED window ahead of the reference. Keyed on the sign of the time
distance alone, the rule would delete D4; keyed on the state alone, it would exclude a maintenance
already under way from the one list that should contain it.

### Item 9 — `ObserveChange.valid_from_unknown` (added during US2)

FR-069 requires a vendor's vague window — "in a future release" — to have its valid start **marked
unknown** rather than guessed. `NodeFact` and `EdgeFact` have carried `valid_from_unknown` since 001;
`ChangeFact` did not, so the only way to express it was to leave `valid_at` unset.

That reads as the zero timestamp, and the change lands in 1970. The reason this is worse than an
ordinary bug is that it is *plausible*: an ancient change ranks as maximally distant, is never excluded
as a future announcement, and sits in the graph looking like a fact about something that happened
before the company existed.

With the marker, the projector starts the interval at the observation and records the start as a bound
— the earliest instant anybody can show, stated as such. It is the same treatment a workload that
already existed when the feeder first listed it receives, which is the point: an unknown start is one
concept and should not have two spellings.

## Consequences

- **`sreagent.graph.v1` and `sreagent.investigation.v1` take a MINOR bump each.** No MAJOR, no
  event-log transformation, no re-recorded goldens.
- **`specs/002-investigation-engine/contracts/investigation.proto` is now byte-identical to the
  shipped proto.** Reconciling it for items 6 and 7 exposed drift wider than the plan recorded: the
  copy also lacked `Coverage.truncated_to_horizon`, `Coverage.horizon`, the whole `FinalHypothesis`
  message and `StopRecord.final_ledger`. A contract copy that drifts is worse than no copy, because
  it is read as authoritative; keeping it identical is now the rule.
- **Feature 005 inherits items 1, 6 and 7 unchanged**, and items 2 and 4 if its connector proposes
  dependencies or reads vendor announcements. That is why this is one ADR for 003 **and** 005.
- **Two additions were found by reading code against requirements rather than plan against spec.**
  Worth repeating on the next feature: the dependency list is a hypothesis about what is missing,
  and the proto is the evidence.
- **Items 8 and 9 were found later still — by writing the property test US2 asked for.** Both
  requirements were in the spec and called contracts in the plan, and neither had a mechanism: FR-065
  had nowhere to state an exclusion, and FR-069 had nowhere to mark an unknown start. A property test
  written against generated inputs is what surfaced them, because the first thing such a test does is
  ask what the code actually returns. The lesson is narrower than "write property tests": a
  requirement the plan calls a contract still needs a task that makes something carry it.

## Alternatives considered

**Seven ADRs.** Rejected: identical context sections, and the additivity question is asked once
across the whole package.

**Translating GCP selectors into `otel-semconv`** so no new vocabulary is needed. Rejected, and it
is the decision most likely to be revisited by someone who has not read the grammar: the Logging
query language's severity comparisons, regular expressions and boolean structure have no
equality form, so the translation is lossy — and a pointer is *stored* and read back years later
(FR-083), which makes a lossy translation a pointer that quietly stops matching.

**Folding proposed dependencies into `graph.suggestions`.** Rejected: identity is symmetric and
that table stores its pair ordered, while a dependency is directed. "checkout depends on orders-db"
and its reverse are different claims, only one of which is true, and an ordered pair files them
under one row meaning neither.

**Reusing `ConfirmMerge`/`RejectMerge` for dependency decisions.** Rejected: "these two refs are
one thing" and "this edge exists" are different claims, and sharing the body would put dependency
decisions in the identity ledger where an audit of merges would have to filter them out.
