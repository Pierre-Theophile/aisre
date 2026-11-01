# ADR-0019: Roadmap after the RCA literature review

- Status: **Proposed** (drafted 2026-11-01 at the owner's request: "if there are scientific advances
  that let us do better than competitors, we integrate them directly"; awaiting the owner's decision
  on D1–D5)
- Deciders: project owner; Claude (the analysis and the proposed order)
- Source: [`docs/reviews/2026-11-01-literature-review-agentic-rca.md`](../reviews/2026-11-01-literature-review-agentic-rca.md),
  findings F1–F8. Follows ADR-0016, which set the order 009 → 010 → 011.

## Context

The review found five results that bear directly on this project:

1. Agents whose investigation is prescribed by the program miss the decisive evidence; free harnesses
   outdiagnose them (F1).
2. The leverage is a harness that knows the system and learns it, behind a regression gate (F2).
3. The dominant failure is *omission*: decisive evidence that was reachable and never queried, with
   nothing in the answer saying so. A Grounder (a profile of the search space) and a Verifier fix a
   large share of it (F3).
4. Agents name the culprit far more often than they reconstruct the propagation path (F3).
5. Topology matters as a navigation map, not as a model feature, and it must be typed, layered and
   runtime-agnostic (F4–F7).

Two audits of the repository, run the same day, set them against what exists:

- **002's verifier is implemented and not wired.** FR-022a requires a fresh-context verifier pass
  before an investigation concludes. `verify.Check` and `verify.NewVerifier` exist with their tasks
  checked (T074, T075), but no production code path calls them.
- **002 says what it tested, not what it did not look at.** FR-014a makes coverage mandatory per
  worker response, and FR-026 names what would resolve an `unknown`. No requirement makes a conclusion
  list the signal families, sources or neighbouring entities that were reachable and never queried.
  This is exactly the failure the owner hit on his own estate on 2026-11-01.
- **002 emits no propagation path.** The output is a ranked hypothesis list (FR-019). The evaluation
  harness reconstructs a path afterwards with a hard-coded `calls` edge
  (`internal/eval/harness.go:560-594`).
- **002's first wave cannot see absence.** FR-046a compares error rate and latency. A callee that
  stops receiving calls raises neither, which is the case F1's failing agents and the statistical
  baselines both missed.
- **On a Cloud Run estate the graph has no service-to-service edge unless OpenTelemetry or Datadog
  supplies one.** The GCP feeder (003) emits `runs_on`, `depends_on` (Cloud SQL), `exposed_via` and
  `owned_by`. It does not read IAM `run.invoker` bindings, Pub/Sub push subscriptions, Eventarc or
  Cloud Scheduler. The OTel feeder ignores `messaging.*` and span kind, so asynchronous flows produce no
  edge. Edges carry no declared-or-observed provenance.
- **009 excluded topology on purpose.** Its out-of-scope list says "Inferring service dependencies;
  topology stays with the feeders". Nothing owns the reconciled topology today.

## Decisions

### D1. Amend 002 now: wire the verifier, report what was not examined, see absence, emit a path

This is a defect-level amendment to an approved, closed feature. It adds suffixed requirements and
renumbers nothing; see `specs/002-investigation-engine/spec.md`, *Session 2026-11-01*:

- **FR-022c: wire FR-022a** into every investigation's terminal path. An unverified outcome is
  marked `not_verified` and says so.
- **FR-012a, the search-space profile.** The investigator's first brief carries the scope's entities,
  edges, which sources and signal families cover each entity, and the blind spots. It is built from
  the graph as it is today; 012 enriches it.
- **FR-022b, the examination report.** A conclusion names what it examined and what it did not:
  - the signal families and sources queried for each hypothesis;
  - the ones that were reachable (a pointer or a connected source exists) and never queried;
  - the neighbouring entities on the impact surface that were never looked at.
  A hypothesis cannot reach *supported* while a signal family that the published expectation table
  marks *decisive* for its cause category was reachable and unqueried. The verifier enforces this.
- **FR-046b, absence in the first wave.** The first wave adds the request rate of each candidate's
  callees and callers before and after onset, and the edges `diff` reports as retracted in the window.
  A stopped flow becomes a candidate, not a footnote.
- **FR-019b, the propagation path.** Every hypothesis at *supported* carries a path from cause to
  symptom as typed graph edges, each edge cited or marked unverified. The harness scores that path
  instead of rebuilding one. **FR-059a** publishes its edge F1 and the omission rate of FR-022b.
- **FR-013b, guidance not procedure.** FR-013 already forbids a fixed order of calls. FR-013b adds that procedures (runbooks, postmortem lessons, any later
  learned procedure) are offered to the investigator as optional guidance with their provenance,
  and are never compiled into a fixed sequence. The deterministic first wave (FR-046a) stays: it adds
  evidence before the first turn and removes no choice from the investigator.

### D2. Feature 012 is topology discovery and reconciliation, runtime-agnostic

A new feature owns the reconciled topology that 009 left to the feeders. Its outline is in
`specs/012-topology-discovery/spec.md` with defaults for the owner to approve:

- edges carry their provenance class (declared, observed or suggested) and their source;
- a reconciliation state for each edge (confirmed, declared only, observed only, stopped);
- a reviewable topology proposal, accepted or rejected as human decisions that win for good;
- new edge sources for estates without Kubernetes, starting with Cloud Run;
- asynchronous edges from OpenTelemetry messaging attributes;
- the enrichment of the search-space profile that 002 FR-012a requires now (provenance,
  reconciliation state, mode, review status), which FR-022b's examination report reads.

Its core schema names no runtime. Kubernetes, Cloud Run, Lambda and virtual machines are adapters.

### D3. Order: the 002 amendment first, then 012 ahead of 011

1. The 002 amendment (D1) is small and fixes a live failure; it goes first.
2. 010 (MCP server) keeps its place: it reads the algebra and does not depend on topology.
3. **012 moves ahead of 011 (change-watch).** Change-watch fires when a change is followed by a
   shift on the pointers of the services in its `impact`. On a Cloud Run estate without OTel or
   Datadog topology, `impact` is the changed service alone, so change-watch would watch almost
   nothing. 012 is what gives 011 a neighbourhood.

ADR-0016 D5 (the live installation) is unchanged and remains the owner's priority for his time.

### D4. Learning from investigations is recorded as direction, not specified

OpsHarness (F2) shows the largest single gain in the review, and also that ungated learning ends
below no learning at all. 002 lists "automatic learning from feedback" as out of scope, and that stays
true for this amendment. When a later feature takes it up, it MUST admit any learned artefact (rule,
procedure, prior adjustment) only through a dual gate on the fixture corpus: strictly better on the
cases it came from, no regression on a held-out set. Constitution VIII already makes the second gate a
CI failure. No feature number is reserved.

### D5. Recorded and deferred

- **Network-level discovery (eBPF).** Ripple (F7) shows it beats passive observation across NAT, but
  it needs an agent on every host. It is useless on managed serverless, and it adds a privileged
  component. It stays out until an estate without traces needs it.
- **Terraform as a source.** 003 excluded it, and the audit log captures its effects. 012 keeps that
  exclusion by default and offers a declared topology file instead; the owner may revisit it there.
- **GPU inference RCA** (TELLER, F8). It is relevant to the owner's GPU clusters, but no GPU
  connector exists. It is a candidate after 012.
- **Graph-learned ranking models.** F4 found no reliable effect of graph structure on learned
  localisation. No GNN work is planned. Topology serves scoping, navigation and the Verifier.

## Consequences

- `specs/002-investigation-engine/spec.md` gains *Session 2026-11-01* and the suffixed requirements of
  D1. A plan delta and tasks follow the owner's approval, not this ADR.
- `specs/012-topology-discovery` is drafted with its defaults as numbered questions, following 006–009:
  the owner approves or changes them on the spec PR.
- The README status table gains a 012 row once approved, and ADR-0016's order becomes 009 → 002
  amendment → 010 → 012 → 011 → setup path.
- Evaluation gains two process metrics that the literature uses and we lacked as gates: propagation
  path edge F1 (FR-019b) and the omission rate (FR-022b). They are reported first and gated once the
  corpus has a baseline, as for pass@1.
