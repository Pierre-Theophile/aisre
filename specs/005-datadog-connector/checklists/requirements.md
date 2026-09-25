# Specification Quality Checklist: Datadog Connector

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-17
**Rescoped**: 2026-09-17 — renamed from 003 to 005 and narrowed by ADR-0004
**Feature**: [spec.md](../spec.md)

## Rescoping check (ADR-0004, 2026-09-17)

- [x] Header states the rename and the rescoping, and the feature branch reads
      `005-datadog-connector`.
- [x] The Why section is rewritten from the coverage audit's facts: no distributed tracing
      anywhere, one service's logs and a handful of monitors in Datadog, changes owned by features
      003 and 004, incidents declared in a Slack severity channel.
- [x] The three things the rescoped feature delivers are each a story at the right priority:
      **log search as a telemetry backend** (US5, P1), **monitor intake** (US2, P1), and **APM
      service-map topology as an OPTIONAL capability, off by default** (US1, demoted to P3 and
      marked conditional).
- [x] Datadog's event stream as a change source is demoted with them (US3, P3, capability
      `changes`, off by default), because features 003 and 004 read the platforms directly.
- [x] The Incident Management story is removed and replaced by a note in its place explaining
      what happened to both halves of it — the graph representation and the evaluation labels.
- [x] Nothing that survives is renumbered. Deleted requirement numbers are retired, not reused:
      **FR-070e, FR-088, FR-088a, FR-089, FR-090** are gone; **FR-008a, FR-008b, FR-040b,
      FR-049b** are added; **SC-022, SC-023, SC-024** are added; **SC-001 and SC-002** become
      conditional on the APM capability and are explicitly forbidden from passing vacuously.
- [x] A **Removed in rescoping** section lists removed, demoted, added and kept-unchanged, so a
      reviewer sees the delta without diffing two drafts.
- [x] Sanitisation, quota, identity, the private corpus and **Dependencies on feature 001** are
      kept; the last gains a note that most of its asks are now shared with 002, 003 and 004 and
      must be satisfied once in 001 rather than three times.

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain — all three were resolved in the 2026-09-17
      clarification session from the adopted external review: Q1 campaign scope (incident density,
      staging game days, boring windows, start the feeder now), Q2 sanitisation (people
      identifiers dropped, infrastructure identifiers keyed-HMAC pseudonymised, log templates,
      monitor bodies dropped, private corpus and public synthetic twins, signed manifest), Q3
      intake (polling in v1, `alert.transition` events, webhook as a doorbell). Q1's
      incident-management half was **superseded the same day by ADR-0004** rather than reopened:
      the product is unused, so the question no longer has a scope to answer.
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified — including the two the rescoping creates: a capability that is
      turned off, and an algebra operation whose data source does not exist
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Constitution alignment

- [x] III event-sourced: the feeder emits typed, idempotent, replayable events; recorded payloads
      are the test; checkpoints declare gaps **and the capabilities in force** (FR-008a, FR-012,
      FR-016, FR-077). Alert intake is itself a feeder: `alert.transition` keyed on (monitor,
      group, transition instant), so push and pull may both deliver without duplicating (FR-025,
      FR-025a).
- [x] IV telemetry stays in its backend: nothing measured is stored; every node carries pointers;
      digests are transient and never written to the graph (FR-051, FR-078, SC-007). The backend
      answers only a published typed query algebra, and every digest carries mandatory coverage,
      join keys and drill-down handles (FR-040a, FR-048a–FR-048c, SC-021). After the rescoping the
      algebra must be answerable **without tracing** (FR-040b) and must say so in a typed way when
      a data source does not exist (FR-049b, SC-023) — which is the honest form of this principle,
      not an exception to it. The tension — digests are aggregates of telemetry — is resolved in
      favour of principle V: a digest may live in the consumer's investigation record and in the
      on-disk recorded-response corpus, never in a graph node, edge, property or event, and this
      is verified rather than asserted.
- [x] IV vocabulary: Datadog-native selectors are the documented exception; entity-identifying
      attributes stay in OpenTelemetry semantic conventions so backends remain interchangeable
      (FR-035, FR-036). Being the **second** backend rather than the only one is what tests this.
- [x] V evidence-first: `no_data` with a coverage block naming the missing data source is a
      first-class answer, and the specification now requires it on the operation that will hit it
      on every investigation (FR-049b). `unknown` is not a failure mode here, it is the product.
- [x] VI resolution auditable: claims before merges, published certain and probable rules,
      conflicts surfaced, audit query answerable (FR-057–FR-064, SC-013).
- [x] VII read-only: read scopes only — **and none at all for a disabled capability** (FR-002,
      FR-008b) — refusal to start with a write-capable credential, no mutating operation anywhere,
      verified from the connector's own request log (FR-002–FR-004, FR-054, SC-015). Removing
      FR-089 removed a redundant requirement, not a safeguard: FR-004 forbids every state-changing
      Datadog operation by name, incidents included.
- [x] VIII recorded real-world payloads as tests: a sanitised campaign from the organisation's own
      Datadog, with goldens, recorded backend responses and a live parity check (FR-069–FR-080).
      The corpus is a **world recording** with a reported miss rate (FR-050a, FR-050b, SC-019); it
      is private, and the public repository carries synthetic structural twins (FR-069a).
- [x] IX open schema: new namespaces, a new pointer vocabulary, new resolution rules and the
      digest contract are all published and versioned (FR-035, FR-056, FR-058, FR-059) — and the
      digest, coverage and algebra contract is now published **once**, shared with feature 003,
      rather than introduced here.
- [x] X simplicity: capabilities that are off by default are the cheapest possible form of "do not
      build what this organisation cannot use" (FR-008a), and YAGNI is honoured by keeping the APM
      mapping specified but unbuilt-by-default rather than half-built.

## Notes

- All checklist items pass. The specification carries no open question: the three clarifications
  were answered on 2026-09-17 from the adopted external review, and the one part of Q1 that the
  coverage audit overtook the same day is marked superseded rather than reopened.
- **Carry into planning**, in this order:
  1. **Build order.** This is now feature 005, after 002 on recorded data, 003 and 004
     (ADR-0004). Two consequences the plan must absorb: the digest, coverage and typed-outcome
     contract will already exist when this starts, published by 003 — this feature implements it
     rather than defining it; and the nodes this connector attaches log pointers to will already
     be in the graph, created by 003 and 004.
  2. **The capability model** (FR-008a, FR-008b) is the first thing to design, because it decides
     what is scoped, called, tested and reported. The plan must state how a disabled capability is
     proved silent — zero scopes requested, zero calls issued, goldens byte-identical — since
     SC-024 is a gate rather than a description.
  3. **The log-only algebra** (FR-040b) is the technical heart of the rescoping: whether
     `errors_by_version` can really be answered from this organisation's logs depends on whether
     those logs carry a version, revision or image attribute. Confirm it against a real payload
     **before** planning the recording campaign, because the answer changes what the campaign has
     to record and may become a requirement on features 003 and 004 to emit that attribute.
  4. The **recording campaign** should still be planned to start before its scope is settled
     (FR-070d): monitor and log-configuration history cannot be reconstructed, so the credential
     is on the critical path ahead of every other decision here.
  5. The **private corpus and public twins** split (FR-069a) implies two CI pipelines; the plan
     must say which suite runs where, and which assertions can only be made privately.
  6. The **quota model** (FR-081a, FR-081b) depends on which Datadog endpoints expose remaining
     quota in their response headers; the plan should enumerate them and name the static fallback
     for those that do not. The binding endpoint class changes with the rescoping: span search is
     no longer used at all by default, and **log search** becomes the class shared with the humans
     working the incident.
  7. The **Dependencies on feature 001** section is smaller than it was, but not empty: the
     "watches" relation, the alert-state vocabulary with its "sampled" marker and the Datadog
     pointer-vocabulary registry entry remain specific to this feature. Everything else on that
     list is owed to 002, 003 and 004 first and must be satisfied once, not three times.
