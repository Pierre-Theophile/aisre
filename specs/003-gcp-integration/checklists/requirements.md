# Specification Quality Checklist: GCP Integration and Vendor-Notice Feeder

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-17
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

GCP product and API surfaces are **named** throughout — Cloud Run, Cloud SQL, Cloud Audit Logs,
Cloud Monitoring, Cloud Logging, Cloud Trace, Cloud DNS, Pub/Sub — because they are the source
system this feature reads, and a specification that would not say which system it reads would be
untestable. No client library, module layout, transport, data structure or code organisation
appears anywhere.

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain — the two that did are **answered in session
      2026-09-21**: FR-131's campaign scope is the production project, all regions, with scope itself
      configuration; and FR-132's notice source is a shared engineering mailbox, with FR-132a making
      per-user delivery a first-class path and FR-132b making the address configuration. One owner
      action remains outside the spec's control and is named in FR-132: confirming whether that
      address is a mailbox or a Google Group, and authorising the read.
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Constitution alignment

- [x] **II bitemporal.** This is the first feature whose value depends on the second time
      dimension doing something it has never been asked to do: a vendor notice is an **announced
      fact**, valid in the future and observed now (FR-062–FR-064). Corrections follow the
      constitution exactly — a cancelled or rescheduled notice closes an observation and opens a
      new one, and never rewrites a valid interval (FR-067, FR-068, SC-008). Unknown valid starts
      are marked, never guessed (FR-022, FR-069).
- [x] **III event-sourced.** Both feeders emit typed, idempotent, replayable events; recorded
      payloads are the test; checkpoints declare gaps and record the scope in force (FR-012,
      FR-026, FR-040, FR-142). Alert intake is itself a feeder: `alert.transition` keyed on
      (policy, group, transition instant), so a doorbell-triggered poll and a scheduled poll —
      and a GCP transition and a human declaration — converge without duplicating (FR-046,
      FR-050).
- [x] **IV telemetry stays in its backend.** Nothing measured is stored; every node carries
      pointers; digests are transient and never written to the graph (FR-109, SC-013). The backend
      answers only a published typed algebra, and every digest carries mandatory coverage, join
      keys and drill-down handles (FR-087, FR-101–FR-103, SC-015). The traffic split is a property
      asserted by the platform, not a measurement of observed traffic (FR-015).
- [x] **IV vocabulary.** GCP-native selectors are the documented exception; entity-identifying
      attributes stay in OpenTelemetry semantic conventions so backends remain interchangeable
      (FR-080, FR-081).
- [x] **V evidence-first.** Every digest carries its query, its execution instant and the
      platform's request identifier (FR-100); every derived dependency records the evidence that
      derived it (FR-028); every undecidable dependency becomes a proposal rather than an edge
      (FR-029); `unknown` is a first-class answer for actor kind (FR-020) and `no_data`,
      `not_yet_ingested`, `not_recorded` and `query_failed` are four distinct outcomes (FR-104,
      FR-105).
- [x] **VI resolution auditable.** Claims before merges, published certain and probable rules,
      conflicts surfaced, two projects with the same name never merged, audit query answerable
      (FR-115–FR-123, SC-021).
- [x] **VII read-only.** Read-only roles only, refusal to start with a write-capable principal
      verified by permission test or effective-role listing, no mutating operation anywhere,
      verified from the integration's own request log (FR-003–FR-005, FR-112, SC-020). The one
      state change in the whole feature — acknowledging a message on the operator's own doorbell
      subscription — is declared as a named exception rather than hidden (FR-006), and the feature
      runs correctly without it. The mailbox is read without altering message state (FR-007).
- [x] **VIII evaluation from day one.** Recorded real payloads as tests, with goldens, recorded
      backend responses, a live-versus-recorded parity check on the 001 pattern, and a world
      recording with a reported miss rate (FR-127–FR-144, SC-014, SC-016, SC-017). The corpus is
      private; the public repository carries synthetic structural twins (FR-128). The headline
      outcome is the **re-run coverage audit** (FR-145, SC-023).
- [x] **IX open schema.** New namespaces, three new pointer vocabularies, new resolution rules,
      the announcement-state vocabulary and the digest contract are all published and versioned;
      everything the feature needs that the schema lacks is collected under **Dependencies on
      feature 001** rather than invented locally.
- [x] **X simplicity.** No new store, no new service. The vendor-notice feeder is deliberately
      thin: typed field extraction and a message pointer, with the body never stored at all — the
      simplest thing that defeats the most repeated cause in the corpus.

## Notes

- **The number to beat is in the spec.** SC-023 binds this feature to the ADR-0004 figure: a
  re-run of the published coverage-audit method over the same thirteen-incident corpus must show
  the true cause was a node or change in the graph at alert time for **at least 8 of 13**, with
  per-incident attribution published. SC-024 demands the specific demonstration — a missed vendor
  notice in the top three of a ranked change list for a real incident. If either fails, the feature
  did not do what ADR-0004 funded it to do, however well the other 23 criteria pass.
- **Carry into planning**, in this order:
  1. **Future-dated valid time** is the one genuinely new thing in this feature and the only item
     on the 001 dependency list with no precedent anywhere in the project. The shape already
     allows it; the *semantics* — legality, the as-of read for a future valid instant, the
     ranker's explicit exclusion of changes valid after the reference instant, the announcement
     state vocabulary, and cancellation-as-correction — need a decision record in 001 before any
     of FR-062–FR-068 can be implemented as specified.
  2. The rest of **Dependencies on feature 001**: the `deprecation` / `vendor_incident` change
     kinds, the actor kind with a member for automation acting on a person's behalf, the
     `alert.transition` event or mapping plus the key convention, the "watches" relation, the
     alert-state vocabulary and "sampled" marker, the **proposed-dependency** convention (new —
     resolution can suggest that two entities are one but not that an edge exists), the unattached
     and auto-attach convention, the join-key vocabulary, the pointer-vocabulary registry entries,
     and a published home for the digest, coverage and algebra contract.
  3. **Three shared contracts must be agreed once, not three times.** The actor-kind set, the
     `alert.transition` convention and the digest/coverage/algebra contract are each needed by
     002, 003 and 005. The plan must say who owns each decision and when it lands, or the three
     features will each build a variant.
  4. **Credential and mailbox access are on the critical path ahead of every other decision**
     (FR-130): neither topology history nor an announcement stream can be reconstructed
     retrospectively, so recording starts before the two open clarifications are answered.
  5. **The private corpus and public twins split** implies two CI pipelines; the plan must say
     which suite runs where and which assertions can only be made privately.
  6. **The quota model** depends on which GCP APIs expose remaining quota in a response and which
     do not; the plan should enumerate them and name the self-tracked fallback for the rest
     (FR-148).
  7. **Cut order is specified, not discovered.** FR-149 publishes it: load balancers and DNS
     first, then the general audit stream, before anything the P1 stories depend on. User Story 9
     is explicitly the first thing cut.
