# Specification Quality Checklist: Topology Discovery and Reconciliation

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-11-01
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
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
- [ ] Feature meets measurable outcomes defined in Success Criteria — pending the owner's approval of
  Q1–Q11 and the thresholds of SC-001 to SC-003
- [x] No implementation details leak into specification

## Notes

- The spec names existing project vocabulary (001's node and edge types, `impact`, `diff`, 003's
  sanitisation contract, 009's catalog file, 002 FR-012a and FR-022b) and the external configuration
  objects it reads (IAM invoker bindings, push subscriptions, URL maps), because those are the user-facing
  facts the operator reviews. It names no storage, library or model.
- The eleven forks (Q1–Q11) are written as defaults, not [NEEDS CLARIFICATION] markers, following 006–009:
  the owner approves or changes them on the spec PR.
- Constitution: no amendment is needed. The provenance class and the `mode`/`role` attributes are
  additive schema changes under Principle IX (minor version, migration as an event-log transformation).
  Reconciliation is a rebuildable projection (I, III). Every new source is read-only (VII) and
  environment-variable values are never stored (IV and 003's sanitisation contract).
- Depends on the 002 amendment of 2026-11-01 (FR-012a, FR-019b, FR-022b, FR-046b, FR-059a) and on
  ADR-0019 D2–D3, which place slice A of this feature ahead of 011 and slice B after it (Q11).
- For the plan, not the spec: the reconciliation state is a new projection rule. Under ADR-0018 a rule
  is moved into the domain when it is changed, so a new rule is written in the domain from the start,
  not as SQL in the projector. FR-044's reduction of a value to host and port happens inside the
  feeder, before the value reaches any shared code.
