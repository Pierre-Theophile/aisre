# Specification Quality Checklist: Deploy Feeders — GitHub and Vercel

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-17
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [ ] No [NEEDS CLARIFICATION] markers remain — **two remain, both scope questions for the owner**:
      1. **FR-008**, which repositories and Vercel projects are in scope for v1 and for the first
         recording campaign (every repository with a deployment, release or deploy workflow, or a
         named allowlist). This decides what the campaign costs and what "not in scope" means in a
         checkpoint; there is no safe default, because reading every repository in an organisation
         is a quota decision the platform owner owns.
      2. **FR-017**, where the repository↔service mapping for a monorepo comes from — connector
         configuration, or a manifest committed in the repository. The *shape* of the monorepo
         answer is decided (one change per target, not one change with several targets); only the
         source of the mapping is open, and the two options differ in who maintains it.
      Everything else that was open is decided in the spec rather than deferred: the monorepo
      unit (FR-017), the rollback flag being platform-stated only (FR-016), valid time at
      completion or promotion (FR-012, FR-033), and the claim keys and their normalisation
      (FR-041, FR-046).
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

- [x] I graph first: both feeders do exactly one thing — emit typed events into the graph. Neither
      executes a pointer, returns a digest or holds shared state of its own (FR-001, FR-051).
- [x] II bitemporal: valid time is the platform's completion or promotion instant, never the poll
      instant; an unstated completion instant is marked unknown rather than guessed (FR-012,
      FR-033, SC-003).
- [x] III event-sourced: deterministic event ids as a pure function of the payload, idempotency
      keyed on (origin, attempt, target), checkpoints that declare gaps and the filters in force,
      and fixtures that replay from empty, double-deliver as a no-op and survive shuffling
      (FR-020, FR-056, FR-057, FR-066, SC-011).
- [x] IV telemetry stays in its backend: logs are pointers, never content; no run log, build log or
      metric is fetched or stored; the zero-payload check is a fixture rather than an assertion
      (FR-029, FR-040, FR-047, FR-067, SC-008).
- [x] V evidence-first: every change carries its origin link and its claims, so a ranked entry can
      be opened and checked by a human (FR-014, SC-016).
- [x] VI resolution auditable: both feeders mint claims and merge nothing; disagreement surfaces as
      a suggestion; every merge involving a deploy claim is explainable by the audit query
      (FR-041–FR-044, SC-017).
- [x] VII read-only: read-only credentials, refusal to start on a write-capable credential, no
      mutating operation issuable, and secret values never requested (FR-002–FR-005, SC-006,
      SC-007).
- [x] VIII evaluation from day one: recorded real payloads from the organisation's own GitHub and
      Vercel are the corpus, private with public synthetic twins, including a fixture that pairs a
      deploy feeder's rollout with a platform feeder's rollout so "one rollout, not two" is pinned
      by a test (FR-059–FR-070, SC-004).
- [x] IX open schema: the deploy claim keys, their value normalisation, the new namespaces, the
      rollback marker and the pointer vocabularies are all published and versioned in feature 001
      rather than invented locally — see **Dependencies on feature 001**.
- [x] X simplicity: two thin feeders, one mapping, no new node types, no repository or project
      taxonomy, no telemetry backend (FR-030, FR-051, Out of scope).

## Notes

- The two open markers are scope questions for the owner, not design gaps; both are answerable in
  one conversation and neither blocks planning of the mapping, the transports or the corpus.
- **Carry into planning**, in this order:
  1. The **claim keys and their normalisation** (FR-041, FR-046) must be agreed with feature 003
     and published **once**, before either feeder is written. They are the whole mechanism behind
     SC-004, and two feeders minting subtly different commit or artefact values is the failure
     mode that produces two adjacent entries for one rollout.
  2. The **Dependencies on feature 001** section is the plan's second input: the actor kind on a
     change, the claim-namespace registry with value forms, a certain rule for shared deploy
     identifiers, the rollback marker, the unattached-and-auto-attach convention, and the pointer
     vocabulary entries. Each needs a decision record in 001 before the matching requirement here
     can be implemented as specified. The actor kind and the unattached convention are shared with
     003 and 005 and must be satisfied once, not three times.
  3. The **cross-feature fixture** (FR-070) needs a window recorded on both sides at once. It is
     cheap while the incident is recent and impossible later, so it is on the critical path with
     the credential rather than after the feeders are written.
  4. The **environment and deploy-workflow allowlists** (FR-023, FR-024) decide how much of a
     GitHub organisation is read; they interact directly with the quota model (FR-071, FR-072) and
     with the first open marker.
  5. The **quota model** depends on which endpoints report remaining quota in their response
     headers; the plan should enumerate them per platform and name the static fallback for those
     that do not.
