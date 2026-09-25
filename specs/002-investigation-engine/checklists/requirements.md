# Specification Quality Checklist: Investigation Engine

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-17
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain — **all 3 resolved**, see Notes
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

## Notes

### Clarifications resolved (Session 2026-09-17)

The three open questions were put to the owner's external reviewer; the review is recorded at
[`docs/reviews/2026-09-17-external-review-002-003.md`](../../../docs/reviews/2026-09-17-external-review-002-003.md)
and is adopted in full for everything it says about 002. The answers are in the spec's
`## Clarifications` section and applied throughout.

1. **FR-061 — evaluating a non-deterministic investigator.** Resolved: pass@1 as the headline,
   pass^k as the reliability figure, never a gate on best-of-k, an aggregate gate over
   fixtures × runs published with the regression it can detect, 3 runs extended to 7 on
   disagreement, production temperature. Ground truth gains a published shape (FR-061b). Applied in
   FR-059, FR-060, FR-061, FR-061a, FR-061b, FR-062a, FR-062b, User Story 10, SC-001, SC-016 to
   SC-021.
2. **FR-047 — budgets.** Resolved: `page` and `review` profiles with stated times, anytime rather
   than deadline-driven shape, caps per backend and cost class including window width, quota share
   read from vendor headers, one investigation per incident, a 15% synthesis reserve, typed stop
   reasons and a diminishing-returns stop. Applied in FR-008a, FR-043, FR-045, FR-045a, FR-045b,
   FR-046a, FR-047, FR-047a, FR-047b, User Story 5, SC-005, SC-013.
3. **FR-057 — human-in-the-loop and output shape.** Resolved: report-only with no blocking state, a
   non-blocking inverse channel of typed authenticated human facts weighted as strong evidence, a
   reopen-on-evidence lifecycle, a published rendering order, deep links on every evidence item, and
   a post-resolution "was this right?" label. Applied in FR-006, FR-057 to FR-057e, User Story 9,
   SC-024, SC-025.

Four items the reviewer asked to settle before the seven questions are also encoded: the coverage
audit (User Story 0, FR-069 to FR-071, SC-022), symptom-onset estimation and causal ordering
(FR-029a to FR-029d and `## Dependencies on feature 001`), the open hypothesis space with a
computed posterior (FR-019a, FR-020a, FR-020b, FR-023, SC-023), and two-layer replay with a typed
query algebra (FR-040, FR-042a to FR-042c, User Story 3, SC-018).

### Amendments recorded in Clarifications, Session 2026-09-17 (b)

Two further decisions, taken after the first coverage audit was run and recorded in
[ADR-0004](../../../docs/decisions/ADR-0004-roadmap-reorder-from-coverage-audit.md) (D4 and D5).
The aggregate audit result is published at
[`docs/evaluation/coverage-audit-2026-09.md`](../../../docs/evaluation/coverage-audit-2026-09.md);
incident-level detail is private and is not in this repository.

4. **Human-declared incidents are a first-class intake (ADR-0004 D4).** Alert intake accepts, as
   well as a monitor state transition, an incident declared by a human — in the audited
   organisation, the creation of a dated severity channel. Specified as *what*, not *how*: a
   declared incident is an `alert.transition` with actor kind *human*, carrying severity, declared
   instant, title, declaring identity, origin reference and optional target references (parsed by a
   published rule or supplied, never guessed); its observed-time pin is the declaration instant; its
   idempotency key is (source, channel or incident identifier, declared instant); it groups with a
   monitor transition for the same symptoms into ONE investigation by time and graph neighbourhood;
   and the answer is returned to where the incident lives, report-only and updated in place. Applied
   in User Story 1a, FR-001a, FR-002a, FR-002b, FR-004a, FR-008b, FR-008c, FR-057f, three new edge
   cases (channel renamed, no parseable target, severity changed later), the *Alert intake* entity,
   Assumptions and Out of scope.
5. **The coverage audit has a result, and the 23 % become fixtures (ADR-0004 D5).** The audit is no
   longer a procedure awaiting its first run: 13 classifiable production incidents, the cause
   present in the graph at alert time in 0 of 13 with the feature 001 feeders alone, 3 of 13 with
   deploy feeders, 8 of 13 with vendor-notice feeders, a ceiling of about 62 %. It is re-run after
   each connector feature ships, with the target ceiling published and the movement attributed to
   the feeders added; and each category of the unobservable remainder — latent bug, client-side
   configuration, business data change, credential leak — owes the corpus at least one `unobserved`
   or `not_change_induced` fixture. Applied in User Story 0 (first result, scenarios 5 and 6),
   FR-069, FR-069a, FR-071a, FR-071b, SC-022, and Assumptions.

### What remains for planning, not for clarification

Every open question now has a *shape*; what is still missing is *numbers*, and each must be set
with the corpus and the coverage audit in hand rather than guessed now:

- the aggregate pass@1 gate value and the corpus size it is computed over (FR-060, FR-061);
- the harm-rate threshold beyond the 5% stated in SC-016, once enough adversarial fixtures exist;
- the `not_recorded` miss-rate threshold for admitting a fixture (FR-042c, SC-018);
- the diminishing-returns threshold (FR-045a) and the exact synthesis reserve (FR-045);
- the confidence buckets and the judgment strength scale (FR-020a, FR-023);
- the per-backend cost classes, window-width caps and quota shares (FR-047a);
- the onset tolerance per fixture family (FR-029a, SC-019);
- the grouping rule's time window and neighbourhood radius for attaching a monitor transition to a
  declared incident, and the published parse rule for target references in a declaration's naming
  convention (FR-002b, FR-008c);
- the ceiling re-measurement cadence beyond "after each connector feature ships", and the target
  ceiling published with each run (FR-071a).

Feature 001 owes this feature five changes (D1 to D5 in `## Dependencies on feature 001`). D1 and
D2 are prerequisites for exonerating post-onset changes; until they land, FR-029c degrades to
"cannot exonerate on timing, and says so".

### Validation observations

- **Constitution IV vs. the investigation recording** — resolved explicitly in Requirements §E
  under *"What is recorded, and where"*: the decision record (ids, parameters, digests,
  confidences, rationale) is an event into the graph, which Principle IV permits by name; the
  worker responses, and the bounded sanitised exemplars FR-014 now allows on explicit request, live
  beside the graph as a bounded, redacted, request-keyed recording. FR-034, FR-037 and SC-010 are
  the testable form of that resolution; relaxing "never raw" to "bounded sanitised exemplars on
  request" does not touch it, because an exemplar never becomes graph state.
- **Constitution V** is carried by FR-014 and FR-014a (workers return structures with mandatory
  coverage, never dumps), FR-019 to FR-027 (evidence, computed confidence, `unknown`, conflicts),
  FR-022a (the fresh-context verifier) and SC-002, SC-004, SC-017.
- **Constitution VII** is carried by FR-008, FR-016, FR-028 and FR-047b, which makes read-only and
  the evidence requirement non-configurable; remediation is out of scope in full.
- **Constitution VIII** is carried by §I in full plus §K; the harness extends fixture verification
  rather than replacing it (FR-063), and the corpus is multiplied rather than re-formatted
  (FR-062a).
- **Constitution III vs. declared-incident intake** — a declaration is an event like any other and
  carries an idempotency key, published in FR-008b as (source, channel or incident identifier,
  declared instant), keyed on a stable identifier rather than on a channel name so that a rename is
  a no-op. FR-002a reuses the `alert.transition` shape rather than introducing a second intake type,
  and FR-002b keeps the parse of a naming convention as a recorded claim with provenance, which is
  Principle VI's rule applied to intake.
- **Constitution VII vs. returning the report to the incident's channel** — FR-057f keeps it inside
  read-only by name: the posting is a rendering of an immutable record, never an action against a
  production system, the credential that posts grants nothing beyond writing and editing that
  report and is separate from every read credential of FR-008, delivery cannot block or gate a
  terminal state, and the transport itself is out of scope here and belongs to a connector feature.
- **Constitution VIII vs. the 23 %** — FR-071b turns the audit's unobservable categories into
  corpus fixtures rather than into an untested claim, in the ground-truth shape FR-061b already
  publishes, graded by SC-023; a category with no fixture is reported as a corpus gap on every
  evaluation run, so the absence is visible rather than silent.
- **"No implementation details"** — one deliberate exception: `## Dependencies on feature 001`
  names published contract fields of 001 (`DiffRequest.reference_at`,
  `RankedChange.time_distance_seconds`). A dependency on another feature's published schema has to
  be precise enough to be actionable there; nothing in it prescribes how 002 is built.
- **"Written for non-technical stakeholders"** — as in 001, the temporal, recording and evaluation
  sections are necessarily precise, and the evaluation section now carries statistical vocabulary
  (pass@1, pass^k, mean reciprocal rank, Bernoulli trials). Each term is defined where it is used;
  personas, stories and the verdict-first rendering stay in plain language.
- All checklist items pass against the updated spec, including the Session 2026-09-17 (b)
  amendments. The feature is ready for `/speckit-plan`; the numbers listed above are plan-time
  decisions with fixtures attached, not unresolved ambiguities.
