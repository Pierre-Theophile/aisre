# Feature Specification: Investigation Engine

**Feature Branch**: `002-investigation-engine`

**Created**: 2026-09-17

**Status**: Draft

**Input**: User description: "The investigation engine: the AI SRE agent that reasons over the
temporal system graph built in feature 001. One reasoning agent — the investigator — owns an
investigation and calls specialised, read-only workers, each bound to one source of truth
(graph, metrics, logs, traces, chat, docs, durable knowledge). It takes an alert and a window
and returns a structured investigation: ranked hypotheses, each tested through pointers, each
carrying an evidence chain and a calibrated confidence, with `unknown` a valid answer. Every
worker call and response is recorded, so an investigation can be replayed from its recording
without a vendor account. Command line and programmatic interface only. Read-only: the engine
proposes, it never acts."

## Why this feature exists

Feature 001 built the substrate and stopped there, deliberately: a bitemporal, event-sourced
graph that can say what the system looked like at 14:32, what changed since 13:00, who is in the
blast radius, and where each node's telemetry lives. A command-line tool stands in for the agent.
This feature is the agent.

The thesis is one sentence: **the model reasons over a structure, not over an ocean of text.**
An investigator that is handed a ranked list of four candidate changes, a two-hop neighbourhood
with traffic weights, and the exact selectors that would confirm or refute each candidate is
solving a small, well-posed problem. An investigator handed a million log lines is guessing
fluently. Accuracy comes from the substrate; the reasoning layer's job is to decide *what to ask
next* and to be honest about what the answers showed.

Three things follow from that, and they are what this feature is really about:

1. **The order of steps is not fixed.** An incident is not a pipeline. The investigator chooses
   its next question — diff the neighbourhood, pull the pointers, compare a before and after
   window, read the postmortem from the last time this happened — within a budget the operator
   sets. Quality is the bar, not a prescribed sequence.
2. **Adaptive reasoning stays evaluable because the tool boundary is recorded.** Every worker
   call and every response is written into the investigation's evidence chain, and an
   investigation can be replayed against those recordings — in two layers, because an
   investigator that reasons differently asks different questions. One layer records the
   trajectory, model outputs included, and replays it byte for byte. The other records a *world*:
   every answer to a small published query algebra over the alert's neighbourhood and window, so
   that a different reasoning path is still served from the recording rather than from the
   network. The graph queries are deterministic in both; what varies between two runs is the
   reasoning, and the harness reports it as exactly that.
3. **Nothing is asserted that cannot be audited.** Every hypothesis cites the events, the query
   parameters and the worker responses that produced it; every confidence is a number that is
   measured for calibration in CI; and `unknown`, with a statement of what evidence was missing,
   is a first-class answer rather than a failure.

This feature is specified alongside feature 003 (the first vendor telemetry connector and its
live backend) and implemented **first**, entirely on recorded data. That is not a compromise: an
engine that can only be exercised with a vendor account is an engine nobody can evaluate, fork or
contribute to.

## Clarifications

### Session 2026-09-17

The open questions in this specification were put to an outside reviewer; the review is recorded
verbatim at `docs/reviews/2026-09-17-external-review-002-003.md` and is adopted. Every answer
below is applied to the stories, requirements, entities, edge cases and success criteria of this
document; where an answer contradicted earlier text, that text was replaced rather than annotated.

- Q: What counts as a passing run for a non-deterministic investigator, and what ground truth must
  the corpus carry? (FR-061) → A: pass@1 — the mean over runs — is the headline, because
  production gets one run; pass^k (all k runs pass) is the reliability figure; best-of-k is
  measured but never gated on, because it describes capability rather than what the on-call
  experiences. The gate is the aggregate over fixtures × runs, published together with the drop it
  can actually detect (8 fixtures × 5 runs is 40 Bernoulli trials: it reliably detects 90% → 70%
  and cannot reliably detect 90% → 80%). Run 3 and extend to 7 only when the first 3 disagree.
  Evaluate under the production model configuration (model, reasoning effort, output settings; the models in use expose no temperature). Ground truth carries: the culprit as a change id, or
  `unobserved`, or `not_change_induced` with a category; the causal path as an entity and edge
  chain; decisive evidence as predicates over digests, plus the exonerating evidence for each
  decoy; the knowability time; and provenance with the sanitiser policy version.
- Q: Which numbers decide whether the reasoning layer is worth running at all? (FR-059, FR-060,
  SC-001) → A: lift over the prior — the investigator's mean reciprocal rank minus the
  deterministic ranker's on the same corpus — is the headline; the harm rate, the fraction of runs
  in which the model talked itself out of a correct rank 1, is gated; citation validity is a hard
  100% gate, meaning every claim resolves to an evidence id and every number inside a claim
  matches the digest it cites. Top-3 hit rate stops being the headline: over four candidates its
  random baseline is 75%, and the deterministic ranker already puts the culprit first on every
  fixture of the current corpus, so on that corpus the investigator can only tie or do harm. The
  culprit's rank is still reported on every run.
- Q: What budgets does a P1 investigation get, and which of them may the operator cap? (FR-047) →
  A: two profiles. `page`: the first tested hypothesis in 90–120 s, a substantive answer by 5
  minutes, a hard stop at 10. `review`: quality-bound, for postmortems and nightly evaluation. The
  shape is anytime, not deadline-driven: the prior-only ranking is published within 5 seconds,
  labelled provisional, and a deterministic first wave runs in parallel with no model in the loop.
  Caps are per backend and per cost class rather than a flat call count, and include the width of
  the query window; the binding constraint is the vendor quota, not model cost, so the agent's
  budget is expressed as a share of the remaining quota read from the vendor's response headers.
  One investigation per incident, not per alert. The operator may cap wall time, cost, per-backend
  budget, concurrency and daily global spend, and may never relax the evidence requirement or the
  read-only posture. On exhaustion: exactly 15 % of the budget is reserved for the closing synthesis;
  untested hypotheses are listed with the exact next queries as deep links; confidence widens
  rather than freezes; the stop reason is recorded as a typed event; and a diminishing-returns
  stop applies before the ceiling is reached. Query failed, no data, not yet ingested and
  `not_recorded` are four distinct answers and are never collapsed.
- Q: May the investigator ask a human a question and block on the answer, and what does Sam want
  first in the output? (FR-057) → A: report-only; a blocking "waiting for human" state does not
  exist. The inverse channel ships in v1: the on-call pushes facts at any time as typed,
  authenticated, non-blocking events in the evidence log, weighted as strong evidence rather than
  as truth, and the engine says so when telemetry contradicts them. Questions for the human are
  rendered as "what would resolve this", and an answer reopens the investigation; the lifecycle is
  running, concluded (partial or final), reopened. Rendering order is: a one-line verdict with the
  decisive fact and the rollback candidate; the ranked list with evidence for and against and
  exonerations shown as prominently as supports; a timeline of changes and symptom onset; the
  narrative last. Every evidence item is a deep link to the exact query and window. After the
  incident is resolved, a one-click "was this right?" label is collected.
- Q: What has to be settled before the engine is worth tuning? (User Story 0, FR-069 to FR-071) →
  A: the coverage audit. Over the last N real incidents — twenty by default — measure the fraction
  whose true cause would have been a node in the graph at alert time. That fraction is the hard
  ceiling on recall, and the categories that are missing (flag flips, IaC applies, database
  migrations, certificate expiries, cron, third-party outages, traffic shifts) rank the feeders
  worth writing next. It is a deliverable of 002's first increment, its input is the incident
  list, and its output reorders the roadmap.
- Q: What instant is a change ranked against, and how are changes that are effects rather than
  causes handled? (FR-029a to FR-029d, "Dependencies on feature 001") → A: rank against the
  estimated symptom onset, not the alert instant — monitors lag by minutes — with onset estimated
  by change-point detection on the alerting metric. Changes that start after onset are exonerated
  as causes and typed as candidate effects rather than ranked as suspects, and changes originated
  by an autoscaler or other system controller are typed distinctly from human-originated ones. Two
  consequences fall on feature 001 and are listed in "Dependencies on feature 001": the ranker
  must handle a reference instant that is earlier than some candidates without giving them the
  maximum temporal score, and a change must carry a typed actor kind.
- Q: How is confidence produced, and what happens when no observed change explains the symptom?
  (FR-019a, FR-020a, FR-020b, FR-023) → A: the hypothesis space is open — an explicit "no observed
  change explains this" hypothesis carries its own probability mass alongside the ranked
  candidates, so `unknown` is a computed result rather than a prompt behaviour. Confidence is
  computed from the hypothesis ledger and never verbalised by the model: each piece of evidence
  becomes a typed judgment (supports, refutes or neutral, with a strength) against a named
  hypothesis, and the posterior is a deterministic function of the prior and the likelihood
  ratios. Confidence is reported in coarse buckets until the corpus is large enough to justify
  finer ones, and calibration is measured with Brier or log loss paired across versions on the
  same fixtures.
- Q: What does "replayable" mean for an investigator that does not ask the same questions twice?
  (User Story 3, FR-040, FR-042a to FR-042c) → A: two layers. L1, trajectory replay, records the
  model's outputs as well as the workers' answers, so a replay is byte-identical; it is the
  plumbing gate on every pull request. L2, world replay, records a world: the cross product of a
  small typed query algebra over the alert neighbourhood and window — at minimum
  `compare(pointer, window_a, window_b)`, `new_log_patterns(pointer, window)`,
  `error_spans(edge, window)` and `errors_by_version(pointer, window)`, the full published set
  being the three families of `contracts/telemetry-backend.md` §1 (amended by Session (c), X1) —
  so any reasoning path can be served from it. Workers may
  only be asked questions expressible in that algebra. A miss returns a typed `not_recorded`,
  distinct from `no_data`, and the miss rate is published as a fixture-quality metric.
- Q: Which workers contain a model, and what must cross the worker boundary? (FR-009a, FR-013a,
  FR-014, FR-018a, FR-020b, FR-022a) → A: most workers contain none — the graph worker is a typed
  API, the metrics worker is change-point detection against a seasonality baseline, the traces
  worker is algorithmic. Logs, docs, chat and postmortem readers may carry a model with its own
  context, after algorithmic template mining has been tried first. Reasoning stays in one agent
  with one belief state: evidence gathering is parallelised, reasoning is not. The call carries
  the hypothesis it serves and the discriminating question it is meant to answer. Every digest
  carries a mandatory coverage statement (what was searched, the data volume, sampling,
  truncation, lag), drill-down handles and preserved join keys; bounded sanitised exemplars are
  returned on explicit request rather than never; one bounded free-text field is allowed and is
  flagged unverified. The central data structure is the hypothesis ledger, held outside the model
  context, updated through tool calls, re-rendered each turn and read by graders. A fresh-context
  verifier checks every claim against its cited evidence before the investigation concludes.
- Q: How does the corpus become able to tell a good investigator from a lucky one? (User Story 10,
  FR-062a, FR-062b) → A: multiply it mechanically and stress it deliberately. Metamorphic
  variants: deleting the culprit event must flip the expected answer to `unobserved`; injecting
  far-away decoys, shifting every timestamp and permuting entity names must not change the verdict
  (name permutation tests structure against guilty-sounding names). Adversarial fixtures are built
  where the published prior fails: a slow-burn change three hours before the alert, a culprit
  three hops away, and a decoy deploy two minutes before the alert on an adjacent service.

### Session 2026-09-17 (b)

Two decisions taken after the first coverage audit was actually run against the owner's
organisation, and recorded in
[ADR-0004](../../docs/decisions/ADR-0004-roadmap-reorder-from-coverage-audit.md) (D4 and D5). The
aggregate result is published at
[`docs/evaluation/coverage-audit-2026-09.md`](../../docs/evaluation/coverage-audit-2026-09.md);
incident-level detail is private and is not in this repository. Both answers are applied to the
stories, requirements, entities, edge cases and success criteria below.

- Q: The audit found that most real incidents were noticed by people rather than by a monitor, and
  that the organisation declares an incident by creating a dated severity channel. Does alert intake
  accept an incident declared by a human? (FR-001a, FR-002a, FR-002b, FR-004a, FR-008b, FR-008c,
  FR-057f, User Story 1a) → A: yes, and it is a first-class intake rather than a special case. A
  declared incident is an `alert.transition` like any other, distinguished by an actor of kind
  *human*; it carries the severity declared, the instant of declaration, a title, the declaring
  identity, the origin reference of the place the incident lives, and zero or more target entity
  references — parsed from the declaration's own naming convention by a published rule, or supplied
  by the declarer, never guessed. It opens an investigation whose observed-time pin is the
  declaration instant, and its idempotency key is (source, channel or incident identifier, declared
  instant). The investigation's output is returned to the place the incident lives, report-only and
  updated in place rather than re-posted. A declared incident and a monitor transition for the same
  symptoms attach to ONE investigation (FR-008a) through the published time-and-neighbourhood
  grouping rule. This feature defines the event shape and the declared intake path only: the
  transport that observes a declaration and the delivery that carries the report back are a
  connector concern — a future chat connector — exactly as webhook and polling intake already are.
- Q: What does the first coverage audit now oblige this feature to do? (User Story 0, FR-069a,
  FR-071a, FR-071b, SC-022) → A: the audit is a result, not a plan, and its aggregate is the number
  every accuracy target cites. Over 13 classifiable production incidents, the true cause would have
  been a node or change in the graph at alert time in 0 with the feature 001 feeders alone, in 3
  with deploy feeders for the platforms actually in use, and in 8 with vendor-notice feeders — a
  ceiling of about 62 %. The audit is re-run after each connector feature ships, over the same
  incident list, with the new ceiling and the target ceiling in force published and the movement
  attributed to the feeders added. The remaining 23 % is not a gap to close but a set of answers to
  get right: latent bug, client-side configuration, business data change and credential leak each
  owe the corpus at least one fixture whose ground truth is `unobserved` or `not_change_induced`
  (FR-061b), graded under SC-023.

### Session 2026-09-17 (c)

Remediation of the `/speckit-analyze` cross-artifact pass over spec.md, plan.md and tasks.md. Each
bullet names the analyze finding ids it closes; none of them changes intent, and no requirement was
renumbered — only suffixed ids were added.

- Q: Which telemetry terms must every backend serve, and where does the term list live? (X1, I3 —
  FR-042b, Key Entities "Query algebra term") → A: [`contracts/telemetry-backend.md`](./contracts/telemetry-backend.md)
  §1 is the single published home of the algebra's three families. Graph terms are typed wrappers
  over 001's query RPCs, answered from the replayed event log and **never cross-producted into a
  world**; the telemetry family is **all eight** terms — `compare`, `onset`, `new_log_patterns`,
  `error_spans`, `errors_by_version`, `monitor_state`, `exemplars`, `drill_down` — and is what a
  backend serves; knowledge is `knowledge_search`. Features 003 and 005 reference that document
  rather than restating a four-term subset.
- Q: What is the published actor-kind set, and does a monitor transition have one? (X2, I6, I12 —
  FR-029d, FR-002a, Key Entities "Alert intake") → A: `{PERSON, AUTOMATION, CONTROLLER, VENDOR,
  UNKNOWN}` (ADR-0005 D1), written with the enum name and the prose gloss in parentheses. A monitor
  transition carries **no actor kind**; the front door an intake came through is a separate field,
  the **origin kind**, with values `monitor` and `declared`.
- Q: What is the idempotency key of an `alert.transition`, and is `alert.transition` a 001
  dependency? (X3, I5 — FR-008b, "Dependencies on feature 001" D5) → A: the 4-tuple **(source,
  stable alert identifier, group, transition instant)**, `group` empty for a human declaration
  (ADR-0005 D2); and yes — `alert.transition` is the fifth event type D5 asks 001 for.
- Q: What does the coverage ceiling actually bound, and what is a "substantive answer"? (A1, A3, A4,
  A2 — FR-071, SC-022, FR-047, FR-045, FR-046a, FR-060) → A: the ceiling bounds recall- and
  coverage-dependent targets only (culprit rank, top-k, lift, localisation); every success criterion
  is annotated `[ceiling-bounded]` or `[not ceiling-bounded: precision|latency|invariance|validity]`
  in a table after the criteria. A **substantive answer** is at least one hypothesis at status
  *supported* or *refuted* with a verdict line that passed the citation checker. The synthesis
  reserve is **exactly 15 %**, the provisional ranking lands **within 5 s**, and the aggregate pass@1
  threshold is **set by the first full corpus run** — reported as `unset` until then, never passing
  silently.
- Q: Which capabilities need both surfaces, what registers durable knowledge, and where do cost
  classes and parse rules live? (G1, G4, U1, U2, D2, I11 — FR-064, FR-049a, FR-047a, FR-002b,
  FR-061a, FR-027) → A: the dual surface covers `investigate`/`declare`/`get`/`replay`/`reopen`/
  `fact`/`report`/`label`/`list`/`export`; operator tooling is CLI-only by design. **FR-049a** adds
  human registration of a durable document (`knowledge link`) as the thing that feeds the v1
  knowledge corpus. Cost classes are the published set `cheap|standard|expensive`; the target-ref
  parse-rule registry is a **connector** artifact and an investigation records only the rule id and
  version. Lift, harm rate and citation validity are defined once in FR-061a, and the six typed
  outcomes are named `digest · no_data · not_yet_ingested · query_failed · partial · not_recorded`
  everywhere.

## Personas

- **Sam, on-call SRE.** Paged at 14:32, ten tabs open, four minutes of patience. Sam runs one
  command with the alert and gets back a short ranked list of what probably broke it, each item
  with the evidence that supports it and the confidence it deserves. Sam's test of the feature is
  brutal and simple: was the thing at the top the thing that was wrong, and could Sam tell from
  the output whether to trust it?
- **Riley, incident reviewer and postmortem author.** Reads investigations after the fact, days
  later, in the calm. Riley needs the whole chain — every question the investigator asked, every
  answer it got, in order — to write a postmortem that survives review, and to mark what the root
  cause actually was. Riley's marking is what turns a past incident into a new evaluation case.
  Riley also owns the coverage audit (User Story 0) — the measurement of how many real incidents
  this engine could ever have explained — because Riley is the person who already knows what
  caused them.
- **The operator.** Runs the engine for the organisation. Cares about what an investigation
  costs, how long it takes, how many calls it makes into telemetry backends and how much of the
  shared vendor quota it takes during an incident, and about being able to cap all of those. Reads
  the ceiling the coverage audit measures and decides what to fund next from it. May cap effort
  and spend; may never relax the evidence requirement or the read-only posture. Also cares that
  the thing cannot write to anything.
- **Casey, connector author** (from 001, unchanged). Writes feeders and, from this feature on,
  workers. A worker is judged by the same standard as a feeder: read-only, declared capabilities,
  recorded responses as its test.

## User Scenarios & Testing *(mandatory)*

### User Story 0 - Measure the ceiling before building on it (Priority: P1, prerequisite)

Before the engine is worth tuning, somebody has to know what it could ever explain. Riley takes
the last N real incidents — twenty by default — and asks one question of each: at the instant the
alert fired, would the true cause have been a node in the graph? Flag flips, Terraform applies,
database migrations, certificate expiries, cron jobs, third-party outages and traffic shifts are
not there with only the OpenTelemetry and Kubernetes feeders. The fraction that would have been
present is the hard upper bound on recall; the categories that were missing are a ranked list of
the feeders worth writing next.

**Why this priority**: it is a few hours of work that can reorder the roadmap. If half of real
causes are invisible to the graph at alert time, the next feeder is worth more than any
improvement to the reasoning layer, and every accuracy number in this specification is a
percentage of that ceiling rather than of the whole problem. It is a prerequisite rather than a
competitor to User Story 1: it is a deliverable of this feature's first increment, and its input
is a list of incidents, not a running engine.

**First result (September 2026)**: the audit has been run once, against the owner's organisation —
13 classifiable production incidents over nine months, two security incidents excluded and one
unclassifiable. With the feature 001 feeders alone, the true cause would have been a node or change
in the graph at alert time in **0 of 13**; with deploy feeders for the platforms actually in use,
**3 of 13**; with vendor-notice feeders, **8 of 13** — a ceiling of about **62 %**. The aggregate is
published at
[`docs/evaluation/coverage-audit-2026-09.md`](../../docs/evaluation/coverage-audit-2026-09.md) and
incident-level detail is kept private. It did what this story exists to do: it reordered the roadmap
([ADR-0004](../../docs/decisions/ADR-0004-roadmap-reorder-from-coverage-audit.md)), and it is the
ceiling every accuracy target in this specification now cites. The remaining 23 % — latent bugs,
client-side configuration, business data changes and credential leaks — are the incidents on which
the correct answer is `unobserved` or `not_change_induced`, and each of those categories owes the
corpus a fixture.

**Independent Test**: run the audit over a named list of past incidents — twenty by default, or
the organisation's whole classifiable set where it holds fewer — and obtain the machine-readable
result — per incident, whether the cause would have been a node at alert time
and, if not, under which category; in aggregate, the ceiling and the per-category counts — without
running a single investigation.

**Acceptance Scenarios**:

1. **Given** a list of past incidents with their alert instants and their known causes, **When**
   the audit runs, **Then** each incident is classified as *cause present in the graph at alert
   time*, *cause absent* with a named category, or *undecidable* with a reason, and the aggregate
   fraction is published as the recall ceiling with the incident count it rests on.
2. **Given** the audit has run, **When** an evaluation gate or an accuracy target is proposed,
   **Then** it cites the ceiling, and no target is set above it.
3. **Given** the audit reports a category that accounts for a large share of the missing causes,
   **When** the roadmap is next ordered, **Then** the feeder for that category ranks above further
   work on the reasoning layer, and the decision cites the audit.
4. **Given** the audit is re-run later against a larger incident list or a graph with more
   feeders, **When** the results are compared, **Then** both runs are readable, the ceiling is
   comparable between them, and the movement is attributable to the feeders that were added.
5. **Given** a connector feature has shipped and added feeders to the graph, **When** the audit is
   re-run over the same incident list, **Then** the new ceiling is published beside the previous
   one together with the feeder set it was measured against and the target ceiling then in force,
   and the difference is attributed to the feeders that were added — including when the ceiling did
   not move.
6. **Given** the audit classifies an incident as not change-induced or unobservable under a named
   category, **When** the evaluation corpus is next assembled, **Then** that category is represented
   by at least one fixture whose ground truth is `unobserved` or `not_change_induced` with that
   category, and a category with no such fixture is reported as a gap in the corpus.

---

### User Story 1 - Investigate an alert and get ranked hypotheses (Priority: P1)

Sam is paged: `checkout` error rate above threshold at 14:32. Sam runs one investigation,
naming the alert and the window to look back over. Within seconds the prior-only ranking is on
screen, labelled provisional. The engine resolves the alert to a node in the graph, estimates when
the symptom actually started, reads the neighbourhood and the ranked changes as of that onset
instant, runs a deterministic first wave of comparisons, decides what else it needs, and returns a
short ranked list of hypotheses. Each hypothesis says what it claims — a named change, a named
condition, or that no observed change explains this — which nodes it concerns, what evidence
supports and what refutes it, how confident the engine is, and why. The answer opens with one
line: the verdict, the decisive fact, and the change that would be rolled back. That is the
sentence Sam acts on; everything after it is there to be checked.

**Why this priority**: This is the feature. Everything else in this spec makes this output
trustworthy, cheap or reviewable; without it there is nothing to trust, pay for or review.

**Independent Test**: Run an investigation against the `rollout-regression-01` recording with an
alert on `checkout` at 14:32 and a one-and-a-half hour look-back. The validated culprit change
appears in the ranked hypotheses at or above the fixture's stated rank, and every hypothesis
carries a non-empty evidence chain and a confidence.

**Acceptance Scenarios**:

1. **Given** a recorded incident in which one change caused the failure among decoy changes,
   **When** Sam investigates the alert, **Then** the response is a ranked list of hypotheses in
   which the culprit change appears within the corpus's stated top-k, each hypothesis naming its
   candidate cause, its target nodes, its status, its confidence and its evidence.
2. **Given** the same investigation, **When** Sam reads any single hypothesis, **Then** every
   claim in it resolves to at least one evidence item, and every evidence item names the worker,
   the capability, the exact parameters it was called with, both time instants, and the response
   it produced.
3. **Given** an alert that names a service the graph knows by a different identifier, **When** the
   engine resolves the alert, **Then** it reports the canonical entity it investigated and the
   identifier it started from, and the resolution is part of the evidence chain.
4. **Given** the investigation completes, **When** Sam asks for machine-readable output, **Then**
   the same investigation is returned as a structured document with stable field names, and the
   human-readable rendering contains no claim absent from the structured form.
5. **Given** the investigation completes, **When** Sam reads the human-readable output, **Then**
   it is ordered: a one-line verdict naming the decisive fact and the rollback candidate; the
   ranked list, each item carrying the evidence for and against it with exonerations shown as
   prominently as supports; a timeline of the changes and the estimated symptom onset; the
   narrative last.
6. **Given** any evidence item in either rendering, **When** Sam follows it, **Then** it is a deep
   link to the exact query, selector and window that produced it, so that the query can be re-run
   by hand and the number checked.
7. **Given** the investigation has only just started, **When** Sam looks at it, **Then** the
   prior-only ranking from the graph is already available, is labelled provisional and untested,
   and is never presented as a tested conclusion.
8. **Given** no candidate change survives its test, **When** the investigation reports, **Then**
   "no observed change explains this" is present as a ranked hypothesis carrying its own
   confidence, rather than being expressed only as the absence of an answer.

---

### User Story 1a - Investigate a declared incident (Priority: P1)

Nobody was paged. Something was wrong, a human saw it, and the first thing that happened was a
person *declaring* an incident — in the audited organisation, by opening a dated severity channel
named for the day and for the thing that hurt. That declaration is a trigger the engine must accept,
because in the audited corpus it is the trigger that fired on every incident while monitor
transitions fired on few. The declaration carries a severity, an instant, a title and — sometimes —
the name of a service. The engine opens one investigation from it, pinned to the instant of
declaration, returns its answer to the place the incident lives, and keeps that answer up to date in
place as it learns more. If a monitor fires for the same symptoms minutes later, it attaches to that
same investigation rather than starting a second one. What this story fixes is the event shape and
the declared intake path; the transport that observes the declaration and carries the report back is
a connector concern (see "Out of scope for this feature").

**Why this priority**: the coverage audit found that most real incidents were noticed by people. An
engine that can only be started by a monitor would not have started at all on most of the corpus.
This is the investigation of User Story 1 with a different front door, so it is small — and without
it the feature's reach is whatever the monitors happen to cover.

**Independent Test**: submit a declared incident — severity, declared instant, title, declaring
identity, no monitor transition — against a recording, and obtain one investigation whose observed
instant equals the declared instant, whose intake record carries the declaring identity and the
origin reference, and whose output is the same structured investigation User Story 1 returns.
Re-submitting the identical declaration produces no second investigation.

**Acceptance Scenarios**:

1. **Given** a human declares an incident with a severity, an instant and a title, and no monitor
   has fired, **When** intake processes it, **Then** exactly one investigation opens, its observed
   instant is the declaration instant, and the intake record carries actor kind *human*, the
   declaring identity, the severity, the title and the origin reference of the place it was
   declared.
2. **Given** the declaration's naming convention or title yields a service the graph knows, **When**
   the engine resolves the subject, **Then** it investigates that entity, states that the reference
   came from the declaration and under which published rule, and the resolution is part of the
   evidence chain.
3. **Given** a declaration from which no target entity can be parsed and none was supplied, **When**
   the investigation reports, **Then** the outcome is `unknown`, the resolving action asks for the
   affected service(s), and no entity is guessed; **and when** a human later supplies them as a
   typed fact, **Then** the investigation reopens and proceeds from those services.
4. **Given** the same declaration is delivered twice, **When** intake processes the second delivery,
   **Then** nothing changes: the idempotency key (source, channel or incident identifier, declared
   instant) matches the first, and no second investigation exists.
5. **Given** a declared incident and, minutes later, a monitor transition on a service inside the
   declaration's graph neighbourhood, **When** intake processes the transition, **Then** it attaches
   to the existing investigation as an additional symptom with its own instant and actor kind, the
   grouping decision is recorded as evidence, and exactly one investigation covers the incident.
6. **Given** the investigation is running and later concludes, **When** its output is returned to
   the place the incident was declared, **Then** the rendering order and the deep links of User
   Story 1 apply unchanged, the earlier report is updated in place rather than duplicated, and
   nothing the engine posts is an action against a production system.

---

### User Story 2 - Every hypothesis is tested, not merely asserted (Priority: P1)

A ranked change is a suspicion. The engine turns it into a tested hypothesis: it takes the
pointers of the nodes the hypothesis concerns, as they were valid at the investigation instant,
and compares telemetry across a baseline window and a symptom window through those pointers. The
comparison — selector, both windows, what moved, by how much, in which direction — becomes
evidence, and the hypothesis's status and confidence follow from it. Where no pointer of the
needed kind exists, the hypothesis is reported as untested and says so.

Causal order is part of the test, not a detail of presentation. A monitor fires minutes after the
thing it is watching goes wrong, so the engine first estimates the symptom's onset by
change-point detection on the alerting metric and ranks candidates against that instant rather
than against the page. A change that started after onset cannot have caused it: it is typed as a
candidate effect and exonerated, and the exoneration is evidence of the same standing as a
support. The autoscaler reacting to a flood of retries is the common case, which is why a change
made by a controller is typed differently from a change made by a person.

**Why this priority**: This is what separates the engine from a ranked list the graph already
produces. It is also where constitution V is either honoured or lost: a conclusion drawn from a
comparison anybody can re-run is auditable; a conclusion drawn from a model's impression of a log
dump is not.

**Independent Test**: Run an investigation on a recording in which the culprit's latency pointer
shows a step change and a decoy's does not. The culprit hypothesis reaches *supported* citing the
before/after comparison; the decoy reaches *refuted* or *inconclusive* citing its own; both
comparisons are in the record with their selectors and windows.

**Acceptance Scenarios**:

1. **Given** a hypothesis naming target nodes that carry a metric pointer, **When** the engine
   tests it, **Then** the evidence contains a comparison over that selector across a baseline
   window and a symptom window, with the statistic used, the direction and the magnitude.
2. **Given** the comparison shows no change at the symptom window, **When** the engine scores the
   hypothesis, **Then** the hypothesis is not reported as supported, and its status and confidence
   state that the test refuted or failed to support it.
3. **Given** a hypothesis whose target nodes have no pointer of the kind the test needs, **When**
   the engine reports, **Then** the hypothesis is present with status *untested* and a reason
   naming the missing pointer kind — it is neither dropped nor treated as refuted.
4. **Given** a node renamed after the investigation instant, **When** the engine fetches its
   pointers, **Then** it uses the selectors valid at the investigation instant, and the evidence
   records the instant the pointers were read as of.
5. **Given** the alerting metric moves before the alert fires, **When** the engine ranks
   candidates, **Then** it does so against the estimated onset rather than the alert instant, and
   the onset estimate — its method, the metric and window it used, and its uncertainty — is an
   evidence item like any other.
6. **Given** a candidate change whose start is after the estimated onset by more than the stated
   uncertainty, **When** the engine scores it, **Then** it is typed as a candidate effect,
   exonerated as a cause with the onset estimate cited, and — where the change was made by an
   autoscaler or another system controller rather than by a person — reported as such.

---

### User Story 3 - Replay an investigation from its recording (Priority: P1)

An investigation carries its own recording, in two layers, because a non-deterministic
investigator does not ask the same questions twice and "replayed with zero live calls" would
otherwise hold only for the run that produced the recording.

*Trajectory replay* records the investigator's own outputs alongside every worker request and
response, so a replay is byte-identical to the run it came from. That is the plumbing gate: it
runs on every pull request and it catches everything except a change in the reasoning.

*World replay* records a world rather than a trajectory: the answers to every question of a small
published query algebra, over the alert's neighbourhood and window, as a cross product. A second
run that reasons its way to a different question is still served from the recording. Workers may
only be asked what the algebra can express, which is what makes the cross product finite. A
question inside the algebra that the world does not hold comes back as a typed `not_recorded` —
a different answer from "no data" — and the rate of those misses is how the quality of a fixture
is measured.

The graph workers answer deterministically from the replayed event log in both layers. Any
difference between an original run and a replay is attributable to the reasoning layer and is
reported as such, never hidden.

**Why this priority**: Without this, an adaptive investigator is unevaluable and the project's
evaluation-from-day-one principle dies at the first vendor dependency. It is also what lets 002
ship before 003.

**Independent Test**: Record an investigation, export it, replay it on a machine with no network
access to any telemetry backend and no vendor credentials, in both layers. In trajectory replay
the run is byte-identical, the investigator's outputs included. In world replay, a second run that
asks different questions of the same algebra is answered entirely from the recorded world, its
misses are typed `not_recorded` and counted, and no live call is attempted in either layer.

**Acceptance Scenarios**:

1. **Given** a recorded investigation replayed in trajectory mode, **When** it is replayed,
   **Then** no call leaves the machine and the replay is byte-identical to the recorded run —
   the model's own outputs included — and any unmatched request fails the replay loudly, naming
   the worker, the capability and the exact parameters.
2. **Given** a replay in world mode in which the investigator asks a question the algebra can
   express but the recorded world does not hold, **When** the query is issued, **Then** the answer
   is a typed `not_recorded` naming the exact term, distinct from "no data" and from "the query
   failed", the investigation continues and reports the miss, and the miss is counted towards the
   fixture's miss rate — falling back to a live backend, returning an empty result, or
   approximating from a similar recording remain prohibited.
3. **Given** two replays of the same recorded investigation, **When** their outputs differ,
   **Then** the difference is reported as a difference in the reasoning layer, with the first
   diverging worker request identified, and never as a difference in the data.
4. **Given** an investigation recorded against live backends and the same investigation replayed,
   **When** the two evidence chains are compared, **Then** every worker response that both runs
   requested is identical.
5. **Given** a worker is asked something that cannot be expressed in the published query algebra,
   **When** the call is made, **Then** it is refused with a typed reason and recorded, so that
   what a worker may be asked and what a fixture must record are the same set.

---

### User Story 4 - Say "unknown" when the evidence is not there (Priority: P2)

Sometimes nothing changed near the alert, the telemetry is thin, the feeder had a gap, or two
things happened at once and the evidence cannot separate them. The engine says so: the outcome is
`unknown`, with the hypotheses it did consider and why each was inconclusive, and — the part Sam
actually uses — a list of what would resolve it: which source is missing, which pointer does not
exist, which window has no coverage, which identifier the graph could not resolve.

`unknown` is computed, not performed. The candidate list always carries one further hypothesis —
*no observed change explains this* — with its own prior and its own probability mass, so the
engine is never forced to spread confidence across four suspects when the real answer is a fifth
thing nobody fed into the graph. When that hypothesis wins, the outcome is `unknown` and the
resolving actions are the feeders and pointers that would have made the real cause visible.

**Why this priority**: A confident wrong answer at 14:32 costs more than no answer. This is the
behaviour that makes the confidence numbers mean anything, and it is a constitution V requirement,
not a nicety. It is P2 only because it is measured against the P1 output, not because it is
optional.

**Independent Test**: Run an investigation on a recording whose expected outcome is `unknown` —
an alert during a feeder gap, or a symptom with no change in the window. The outcome is `unknown`,
no hypothesis is marked supported, and the "what would resolve this" list names the gap or the
missing source.

**Acceptance Scenarios**:

1. **Given** a window in which the graph knows of no change near the alerting node and no
   telemetry comparison separates any candidate, **When** the investigation completes, **Then**
   the outcome is `unknown`, and every hypothesis considered is listed with status *inconclusive*
   or *refuted* and its evidence.
2. **Given** the investigation instant falls inside a recorded feeder gap, **When** the engine
   reports, **Then** the gap is named in the output as a coverage limitation, the affected
   hypotheses carry reduced confidence stating the gap as the reason, and the outcome is not
   presented as complete.
3. **Given** an `unknown` outcome, **When** Sam reads it, **Then** it names at least one concrete
   thing that would change the answer — a source to connect, a pointer to add, an identity to
   confirm, a window to widen — expressed as something a person can act on, each carrying the exact
   next query as a deep link where one exists.
4. **Given** an incident whose true cause was never ingested by any feeder, **When** the
   investigation completes, **Then** *no observed change explains this* is the top-ranked
   hypothesis with its confidence, and the outcome is `unknown` because that hypothesis won — not
   because the engine declined to choose.

---

### User Story 5 - Bound and see the cost (Priority: P2)

The operator sets what an investigation may spend: wall-clock time, per-backend and per-cost-class
call budgets, the width of the windows it may query, a share of the vendor's remaining quota, and
cost units. Every investigation reports what it actually spent against each. Two profiles are
published: `page`, for a live incident, where the first tested hypothesis lands in 90–120 seconds,
a substantive answer by five minutes and a hard stop at ten; and `review`, quality-bound, for
postmortems and nightly evaluation.

The shape is anytime rather than deadline-driven. The prior-only ranking — the graph's own
deterministic answer, which costs milliseconds — is published within 5 seconds and labelled
provisional. A deterministic first wave then runs in parallel with no model in the loop: for each
top candidate, error rate and latency before and after, new log patterns, error spans on the path
edges, and errors split by version tag. The model's first turn therefore starts with evidence in
hand rather than with a blank page. When a budget runs out, the investigation stops cleanly with
about 15% held back for the closing synthesis, and returns what it has — ranked hypotheses so far,
a typed stop reason, widened rather than frozen confidence, and the untested hypotheses with the
exact next query for each as a deep link. It also stops early when the last few questions stopped
moving the ledger.

**Why this priority**: An adaptive agent with no ceiling is an unbounded bill and an unbounded
page. The operator will not run this in production without a cap, and the eval harness cannot
report cost per investigation unless the engine measures it.

**Independent Test**: Run the same recorded incident twice, once with an ample budget and once
with a worker-call budget deliberately set below what the first run used. The first completes and
reports its spend; the second terminates as budget-exhausted, returns partial ranked hypotheses,
and lists what remained untested.

**Acceptance Scenarios**:

1. **Given** budgets configured for time, worker calls and cost, **When** an investigation
   completes, **Then** its output reports consumption against each budget in machine-readable form.
2. **Given** a budget is exhausted mid-investigation, **When** the engine stops, **Then** it
   returns the hypotheses it has with their current status and confidence, marks the outcome
   budget-exhausted, and names the hypotheses it left untested and the questions it did not ask.
3. **Given** an in-flight investigation, **When** the operator inspects it, **Then** the calls made
   so far and the spend against each budget are visible before the investigation finishes.
4. **Given** two different alert priorities with different configured budgets, **When**
   investigations run, **Then** each uses its own budget and records which budget profile applied.
5. **Given** the `page` profile, **When** an investigation runs, **Then** the provisional
   prior-only ranking is available within 5 seconds, the first tested hypothesis within 120 seconds,
   a substantive answer within five minutes, and the run stops at ten minutes whatever its state.
6. **Given** a backend whose responses report the caller's remaining quota, **When** the engine
   plans its next queries, **Then** it spends no more than its configured share of that remaining
   quota, and the share it observed and the share it used are both recorded.
7. **Given** several alerts fire for one incident, **When** the engine intakes them, **Then** one
   investigation covers the incident and the later alerts attach to it as additional symptoms;
   a second investigation for the same incident is not started.
8. **Given** the last several worker calls have not changed any hypothesis's status or confidence
   beyond a published threshold, **When** the engine decides what to do next, **Then** it stops on
   a typed diminishing-returns reason rather than spending the rest of the budget.

---

### User Story 6 - Surface what we already wrote down (Priority: P2)

Somebody has seen this before. The knowledge worker retrieves durable knowledge — postmortems,
runbooks, architecture decisions, past investigations — but only what is **linked to the nodes
this investigation is about**. The graph is the index: a runbook attached to `payments`, a
postmortem about the last connection-pool exhaustion on the same database, the investigation from
the previous rollout regression. Retrieved documents are cited with their link back to the graph,
and are treated as context, never as evidence of the system's current state.

**Why this priority**: It is where an agent beats a fast SRE rather than merely matching one, and
the constitution already scopes it: retrieval is permitted over durable knowledge linked to graph
nodes, and nowhere else.

**Independent Test**: Run an investigation on a recording whose affected nodes have a linked
runbook and a linked past postmortem, plus unrelated documents linked to distant nodes. The linked
documents are cited; the unrelated ones are not retrieved.

**Acceptance Scenarios**:

1. **Given** a postmortem linked to a node inside the investigation's subgraph, **When** the
   engine investigates, **Then** the document is retrieved and cited with its identifier, its link
   to the node, and its age.
2. **Given** a document linked only to nodes outside the investigation's subgraph, **When** the
   engine retrieves knowledge, **Then** that document is not returned.
3. **Given** a runbook states that a symptom is usually caused by a given condition, **When** the
   engine uses it, **Then** the resulting hypothesis is labelled as knowledge-derived and is either
   confirmed through a source-of-truth worker or reported as unconfirmed — the document alone never
   makes a hypothesis *supported*.
4. **Given** a retrieved document contains text that reads as an instruction to the engine, **When**
   it is processed, **Then** it is treated as data: the investigation's scope, budgets, read-only
   posture and output are unchanged, and the attempt is recorded.

---

### User Story 7 - Review an investigation and close the evaluation loop (Priority: P3)

Days later, Riley opens the investigation. The whole chain is there in order: every question, its
parameters, its answer, the hypotheses as they rose and fell, the budget spent. Riley marks what
the root cause actually was — agreeing with the engine or overruling it — and exports the
investigation as a replayable incident: the graph snapshot and pointers from 001, plus the
question that was asked and the validated answer. That incident joins the corpus, and from then on
every change to the engine is measured against it.

**Why this priority**: It is how the corpus grows from real incidents rather than hand-authored
ones, which is the only way the accuracy claim stays honest. P3 because the first corpus can be
built from existing fixtures.

**Independent Test**: Take a completed investigation, mark a validated root cause through the
review interface, export it, and run the eval harness over the exported incident without editing
any file by hand.

**Acceptance Scenarios**:

1. **Given** a completed investigation, **When** Riley opens it, **Then** the full evidence chain
   is presented in the order the investigator produced it, with each worker call, its parameters,
   its response and the time it took.
2. **Given** Riley marks a validated root cause, **When** the decision is recorded, **Then** it
   carries the authenticated individual who made it and when, and it does not alter the original
   investigation — the correction is additive and both are readable.
3. **Given** a reviewed investigation with a validated root cause, **When** it is exported, **Then**
   the result is accepted by the evaluation harness as a replayable incident with no hand-editing,
   and replaying it reproduces the recorded worker responses.
4. **Given** the graph has been rebuilt from its event log since the review, **When** the review
   decision is read, **Then** it is still present, still attributable, and still in force.

---

### User Story 8 - A correction teaches the next evaluation (Priority: P3)

Sam or Riley disagrees with a ranking: the engine put a scaling event first when the rollout was
the cause. The correction is persisted against that investigation — which hypothesis was wrong,
which should have ranked where, and why — attributable and durable. It does not silently rewrite
history; it becomes a labelled case that later evaluation runs are scored against.

**Why this priority**: Feedback that lands nowhere is feedback nobody gives twice. This closes the
loop from "the agent was wrong" to "the agent is measured on having been wrong". P3 because the
corpus and the harness must exist first.

**Independent Test**: Record a correction on a completed investigation, then run the evaluation
harness: the corrected case appears in the corpus with the human's ranking as ground truth, and
the engine's score on it is reported.

**Acceptance Scenarios**:

1. **Given** a completed investigation, **When** a human corrects a hypothesis's status, rank or
   confidence, **Then** the correction is stored with its author, time and rationale, and the
   original investigation remains readable exactly as produced.
2. **Given** corrections exist, **When** the evaluation harness runs, **Then** the corrected cases
   are part of the corpus and the engine's agreement with the human labels is reported as a metric.
3. **Given** a correction and an automated score disagree, **When** the corpus is read, **Then**
   the human label takes precedence and the disagreement is visible rather than silently resolved.

---

### User Story 9 - The on-call pushes a fact, and the investigation reopens (Priority: P2)

Sam knows something the telemetry does not: the load balancer was drained by hand at 14:05, the
vendor has just posted an outage, the migration everybody forgot was run from a laptop. Sam pushes
that fact into the investigation at any time — while it runs or after it has concluded. Nothing
blocks: the engine never waits for a human and has no state in which it is waiting. The fact
enters the evidence log as a typed, authenticated item and is weighted as strong evidence rather
than as truth; where telemetry contradicts it, the engine says so plainly and keeps both. If the
investigation had already concluded, the fact reopens it, and the reopened run is a linked record
rather than an edit. The questions the engine would like a human to answer are in the output as
"what would resolve this", never as a prompt that stalls the run.

**Why this priority**: a diagnosis that cannot take the one thing the on-call knows is a diagnosis
the on-call works around. Never blocking is what makes it safe to run this on a page at all: a
paged human who walks away must not leave an investigation hanging.

**Independent Test**: conclude an investigation on a recording, then submit a typed human fact
that contradicts its top hypothesis. The investigation reopens as a linked record, the fact is in
the evidence chain with its author and time, the contradicted hypothesis loses confidence with the
contradiction stated in the output, and the original concluded record is still readable unchanged.

**Acceptance Scenarios**:

1. **Given** a running investigation, **When** a human submits a typed fact, **Then** it is
   recorded as an authenticated evidence item, the ledger is updated, and at no point does the
   investigation enter a state in which it is waiting for a human.
2. **Given** a concluded investigation, **When** a human submits a fact that bears on it, **Then**
   the investigation moves to *reopened*, the new run is a separate linked record, and the
   concluded record remains readable exactly as produced.
3. **Given** a human fact and a telemetry comparison that contradict each other, **When** the
   engine scores the affected hypothesis, **Then** both are kept in the evidence chain, the
   contradiction is stated in the output, and neither is silently dropped — the human fact carries
   a strong weight, not the last word.
4. **Given** a concluded investigation, **When** the incident is resolved and the reviewer answers
   the one-click "was this right?" question, **Then** the label is stored against the
   investigation with its author and time and becomes part of the evaluation corpus.
5. **Given** the engine has a question only a person can answer, **When** it reports, **Then** the
   question appears under "what would resolve this" with the evidence it would change, and the
   investigation still reaches a terminal state without it.

---

### User Story 10 - Fixtures that tell a good investigator from a lucky one (Priority: P2)

On the corpus as it stands, the deterministic ranker already puts the culprit first on every
fixture. That corpus cannot distinguish an investigator that reasons from one that repeats the
prior, and "top three of four candidates" has a 75% random baseline. So the corpus is multiplied
and stressed. Multiplied mechanically: delete the culprit event and the expected answer becomes
*unobserved*; inject far-away decoys, shift every timestamp, permute the entity names — none of
those may change the verdict, and the permutation is there to check that the engine ranks on
structure rather than on guilty-sounding names. Stressed deliberately: a slow-burn change three
hours before the alert, a culprit three hops away, a decoy deploy two minutes before the alert on
an adjacent service — cases where the published ranking formula is wrong on purpose, and where
lift over the prior is the only thing that can show up.

Each fixture carries ground truth in a fixed shape: the culprit as a change id or `unobserved` or
`not_change_induced` with a category; the causal path from cause to symptom as an entity and edge
chain, so localisation, attribution and mechanism can be graded separately and partial credit is
possible; the decisive evidence as predicates over digests rather than strings, plus the
exonerating evidence for each decoy; the knowability time, being the earliest observed instant at
which the decisive evidence existed — before which `unknown` is the *correct* answer; and
provenance, being synthetic, recorded or derived, with the sanitiser policy version.

**Why this priority**: without it every accuracy number in this specification is measured against
a bar the substrate already clears alone, and no change to the reasoning layer can be shown to
have helped or harmed.

**Independent Test**: generate the metamorphic variants of one recorded incident and run the
harness. The timestamp-shifted, name-permuted and decoy-injected variants produce the same verdict
as the original; the culprit-deleted variant produces `unobserved`. On the three adversarial
fixtures, the deterministic ranker does not put the culprit first, and the investigator's lift
over it is reported.

**Acceptance Scenarios**:

1. **Given** a fixture and its metamorphic variants, **When** the harness runs, **Then** the
   timestamp-shift, entity-name-permutation and far-decoy variants yield the same verdict as the
   original, and any divergence fails the run naming the variant.
2. **Given** a fixture whose culprit event has been deleted, **When** the harness runs, **Then**
   the expected answer is `unobserved` with the symptoms localised, and a run that names a decoy
   as the culprit fails.
3. **Given** an adversarial fixture on which the deterministic ranker does not put the culprit
   first, **When** the harness runs, **Then** the investigator's rank, the prior's rank and the
   lift between them are all reported, and the gate is on the lift.
4. **Given** a fixture with a knowability time, **When** the engine is asked before that instant,
   **Then** `unknown` is the correct answer and is scored as correct; asked after it, the decisive
   evidence is expected to be found.
5. **Given** any fixture, **When** its ground truth is read, **Then** it carries the culprit, the
   causal path, the decisive evidence predicates, the exonerating evidence for each decoy, the
   knowability time and the provenance with the sanitiser policy version.

---

### Edge Cases

- **Alert on an unknown node**: the alert names an identifier the graph cannot resolve to any
  entity. The engine reports which identifier it could not place, surfaces any pending resolution
  suggestions for it, and either investigates nothing (outcome `unknown`, with "confirm this
  identity" as the resolving action) or investigates a caller-confirmed node — never a guessed one.
- **Alert during a feeder gap**: the window overlaps a recorded gap in a source's coverage. The
  engine reads the graph's extent, names the gap in the output, lowers the confidence of every
  hypothesis that depends on the affected source, and never presents the answer as complete.
- **Conflicting evidence**: two workers, or two sources inside one worker's answer, disagree (the
  graph reports a property conflict; the metric says recovered while the log says still failing).
  Both are kept in the evidence chain, the conflict is stated in the hypothesis, and the confidence
  reflects it. Silently picking a side is a defect.
- **Two simultaneous changes**: two plausible candidate changes with nearly identical scores and
  neither separable by the available telemetry. The engine reports both, at similar confidence,
  states explicitly that it could not separate them, and names what would (a pointer, a longer
  window, a canary comparison). It does not break the tie by preference.
- **Budget exhausted mid-investigation**: the investigation terminates cleanly at the boundary,
  having reserved exactly 15 % of the budget for the closing synthesis so that the answer is written
  rather than cut off. It returns what it has with a typed stop reason, widens the confidence of
  every hypothesis whose testing was cut short rather than freezing it at its last value, and lists
  the untested hypotheses with the exact next query for each as a deep link. Partial results are
  never presented as complete, and never discarded.
- **Recorded backend missing a query**: the answer depends on the layer. In trajectory replay,
  where the run is expected to be identical, an unmatched request fails the replay loudly and names
  it. In world replay, where a different reasoning path is the point, a question the algebra can
  express but the recorded world does not hold returns a typed `not_recorded`; the run continues,
  the miss is reported and counted towards the fixture's miss rate. In both layers, approximating
  from a nearby recording, silently returning empty, or falling through to a live backend are
  defects — a replay that improvises is not evidence of anything.
- **A question outside the query algebra**: a worker is asked something the published algebra
  cannot express. The call is refused with a typed reason and recorded. This is a designed limit,
  not a failure: what a worker may be asked and what a fixture must record are the same set, and
  the set is what makes the recorded world finite.
- **A worker that wants to return telemetry payloads**: a telemetry worker's natural answer is a
  series, a log page or a span set. What reaches the investigator — and therefore what is recorded
  — MUST be a bounded, structured digest: comparisons, counts, mined patterns, exemplar
  identifiers, pointers, join keys, drill-down handles and a mandatory coverage statement, never a
  dump. Raw material is not forbidden outright, because a stack trace is often the decisive fact:
  the investigator MAY ask for a bounded number of sanitised exemplars, and the worker MUST cap,
  sanitise and record them. Exemplars live in the recording beside the graph and never in the
  graph itself, which is what keeps constitution IV intact. One bounded free-text field per digest
  is allowed and is flagged unverified. Oversized responses are truncated at the published cap
  with the truncation recorded (see "What is recorded, and where", below).
- **Investigations on merged entities**: the alert names one alias of an entity that has been
  merged. The engine investigates the canonical entity, reports the aliases it covers, and cites
  the resolution audit as evidence. If the merge was decided *after* the investigation instant, the
  observed-time pin means the engine sees the pre-merge world and says so; a replay reproduces that
  same pre-merge view.
- **A merge or correction learned after the alert**: any fact whose observed time is later than the
  investigation instant is invisible to the investigation by default. An investigation run in
  review mode, which is allowed to know everything learned since, MUST record that it did so, and
  MUST NOT be compared against a pinned run as if the two were the same question.
- **Duplicate or repeated worker calls**: the investigator asks the same question twice. The second
  call is recorded, counted against budget, and answered identically; the evidence chain shows both
  rather than deduplicating them away.
- **Worker unavailable or slow**: a worker errors or exceeds its per-call time allowance. The
  failure is an evidence item of its own, the affected hypotheses are marked untested with that
  reason, and the investigation continues within budget rather than aborting.
- **Investigation of an incident older than a source's retention**: the graph answers as of any
  instant, but a telemetry backend may no longer hold the window. The engine distinguishes "the
  data says no" from "the backend no longer has the data" and reports the second as a coverage
  limitation, not as a refutation. Four answers are kept apart and never collapsed into "no": the
  query failed; the window is covered and holds nothing; the data exists but was not yet ingested
  at the observed instant in force; and the recorded world does not hold this question
  (`not_recorded`).
- **A change that is an effect, not a cause**: the autoscaler scales the front end two minutes
  after the symptom starts, because it is reacting to the retries the incident produced. Ranked
  against the alert instant it looks like a fresh, close, heavily-trafficked suspect. Ranked
  against the estimated onset it starts *after* the symptom, so it is typed as a candidate effect
  and exonerated, with the onset estimate cited and the actor — a controller, not a person —
  stated. A change that started after onset is never given the maximum temporal score for being
  the most recent thing that happened.
- **The symptom onset cannot be estimated**: the alerting metric is flat, absent, too sparse, or
  the change-point detector has no confidence. The engine falls back to the alert instant, says in
  the output that it did so, widens the uncertainty it attaches to every exoneration, and does not
  exonerate any candidate on timing alone.
- **No observed change explains the symptom**: every candidate is refuted or exonerated. The
  explicit *no observed change explains this* hypothesis takes the remaining mass and ranks first;
  the outcome is `unknown`; the resolving actions name the feeders and pointers that would have
  made such a cause visible. This is a correct answer, and the corpus contains fixtures whose
  ground truth is exactly it.
- **A human fact contradicts the telemetry**: the on-call says the load balancer was drained by
  hand; the metric comparison says nothing moved. Both stay in the evidence chain, the
  contradiction is stated in the affected hypothesis, and the human fact is weighted as strong
  evidence rather than as truth. Neither silently wins.
- **A fact arrives after the investigation concluded**: the investigation moves to *reopened* and
  the new run is a linked record. The concluded record is never edited, and a comparison between
  the two is a comparison of two different questions.
- **Several alerts for one incident**: alerts keep firing as the blast radius widens. One
  investigation covers the incident; later alerts attach to it as additional symptoms, with their
  own instants, and each is visible in the timeline. Starting a second investigation for the same
  incident is a defect, not a duplicate.
- **The declared incident's channel is renamed**: the place an incident was declared changes its
  name after the fact, so the name the investigation was opened from no longer resolves. The
  investigation is keyed on that place's stable identifier and the declared instant (FR-008b), never
  on its name, so a rename changes nothing that matters: the investigation continues, the new name
  is recorded as a later alias of the same origin reference, and the report is still updated in
  place. A rename MUST NOT open a second investigation, and MUST NOT retroactively re-target an
  investigation whose target references were parsed from the old name — that parse keeps its
  provenance and its instant, and is contradicted only by evidence or by a human fact.
- **A declared incident with no parseable target**: the declaration names a severity and a time but
  nothing the graph can resolve to an entity. The engine does not guess. It opens the investigation,
  pins observed time to the declaration, and reports the outcome `unknown` with "name the affected
  service(s)" as the resolving action — and it waits for nobody. When a human pushes those services
  as a typed fact, the investigation reopens and proceeds from them, starting from the declared
  services rather than from a guess. Where the declaration's naming convention *does* yield a
  service, that parse is an evidence item with its own provenance and can be contradicted like any
  other.
- **The declared severity changes later**: an incident declared sev3 is raised to sev1 an hour in,
  or quietly lowered. The severity is a property of the declaration, recorded with the instant it
  changed and the person who changed it, as an additive event rather than an edit. The investigation
  is not restarted and its observed-time pin does not move. A raised severity MAY select a different
  budget profile for work still to be done, and that switch is recorded with its own typed reason;
  it never rewrites the work already done, and it never edits a concluded record.
- **The vendor quota is nearly spent**: the backend reports little remaining quota, which humans
  are also using during the incident. The engine keeps within its configured share of what
  remains, records the share it observed, prefers the cheaper cost class and the narrower window,
  and stops on a typed budget reason rather than competing with the on-call for the quota.
- **The prior was already right**: the deterministic ranking put the culprit first and the
  investigator's reasoning moved it down. This is *harm*, it is measured as such on every
  evaluation run, and it is gated. An engine that cannot beat the prior must at least not damage
  it.

## Requirements *(mandatory)*

### Functional Requirements

#### A. Intake and the shape of an investigation

- **FR-001**: The engine MUST expose an `investigate` operation that accepts either an alert or a
  node reference plus an instant, together with a look-back window, and returns one structured
  investigation. Both a command-line interface and a programmatic interface MUST offer it; no
  other interface is in scope.
- **FR-001a**: Intake MUST accept, in addition to a monitor state transition, an incident
  **declared by a human**. A declared incident MUST be a valid intake in its own right: it opens an
  investigation under FR-001 with no monitor having fired, and the engine MUST NOT require a monitor
  transition to exist before it will investigate.
- **FR-002**: Intake MUST normalise its input into: the subject entity reference(s), a symptom
  statement, the instant the symptom was observed, and the window to reason over. Where the input
  is an alert from an external system, the origin reference MUST be retained.
- **FR-002a**: A declared incident MUST normalise into the same `alert.transition` shape as a
  monitor transition, distinguished by the intake's **origin kind** — the published values `monitor`
  and `declared`, a field separate from the actor kind of FR-029d — and MUST carry: the severity declared, the instant of declaration, a human-readable title, the
  authenticated declaring identity, the origin reference of the place the incident lives, and zero
  or more target entity references. The engine MUST NOT invent any of these, and MUST NOT
  substitute the instant it observed the declaration for the instant of declaration.
- **FR-002b**: Target entity references on a declared incident MUST come either from the
  declaration itself — parsed from its title or naming convention by a published, stated,
  configurable rule — or from a human who supplies them, at declaration time or later. The
  **registry of parse rules is a connector artifact**, owned and versioned by the connector that
  observes declarations in that place; this feature neither defines the rules nor interprets their
  text. What an investigation records is the **rule id and the rule version that produced the
  reference**, nothing more, so a later reader can resolve the rule from the connector that owns it.
  The provenance of each reference MUST be recorded and MUST be distinguishable between the two. Where
  neither yields a reference, the engine MUST NOT guess one: it proceeds under FR-003 and FR-026 to
  the outcome `unknown` with "name the affected service(s)" as the resolving action, and MUST accept
  those services when a human supplies them as a fact under FR-057a, reopening the investigation
  under FR-057b and starting from the declared services.
- **FR-003**: The engine MUST resolve the subject to a canonical graph entity before reasoning,
  MUST report both the identifier it was given and the entity it resolved to, and MUST NOT proceed
  on a guessed identity. An unresolvable subject MUST produce an outcome under FR-026.
- **FR-004**: Every investigation MUST pin two instants: the valid instant it is about and the
  observed instant it is entitled to know. The observed instant MUST default to the symptom
  instant, so that facts the system learned later are invisible to the investigation.
- **FR-004a**: For a declared incident, the observed instant of FR-004 MUST be pinned to the instant
  of declaration — not to the instant the engine observed the declaration, and not to the instant
  investigation began. A later change to the declaration's severity, title or target references MUST
  NOT move that pin: it enters the investigation as a separate, later, typed event under FR-007,
  carrying its own instant and author.
- **FR-005**: An investigation MAY be run in review mode, in which the observed instant is "now".
  Review mode MUST be recorded on the investigation and MUST NOT be the default for an alert-driven
  run.
- **FR-006**: Every investigation MUST carry a stable identifier, a lifecycle status, start and
  end times, the budget profile applied, and the authenticated identity that requested it. The
  lifecycle statuses are exactly *running*, *concluded* (qualified as partial or final), *reopened*
  and *failed*; no status in which the engine waits for a human may exist. The terminal outcome — a
  ranked answer, `unknown`, budget-exhausted or failed — is a separate property from the lifecycle
  status, so that a partial conclusion and an `unknown` conclusion remain distinguishable.
- **FR-007**: A completed investigation MUST be immutable. Later additions — human review, human
  corrections, re-runs — MUST be recorded as separate, linked records, never as edits.
- **FR-008**: The engine MUST be operable with read-only credentials for every source it reads, and
  MUST refuse to start with credentials that grant write access where the source distinguishes the
  two.
- **FR-008a**: One investigation MUST cover one incident, not one alert. An alert the intake
  associates with an incident already under investigation MUST attach to it as an additional
  symptom, with its own instant and origin reference, and MUST NOT start a second investigation.
  The association rule MUST be published, and the association itself MUST be recorded as evidence.
- **FR-008b**: Every `alert.transition` — monitor transition and human declaration alike — MUST be
  keyed on the same published 4-tuple **(source, stable alert identifier, group, transition
  instant)** (ADR-0005 D2). For a monitor transition the stable alert identifier is the monitor or
  policy identifier and `group` is the monitor's group key; for a human declaration the stable
  alert identifier is the stable identifier of the place the incident lives — never its name — the
  transition instant is the declared instant, and **`group` is empty**. Re-delivery under a seen key
  MUST be a no-op and MUST NOT open a second investigation, however many times the transition or
  declaration is observed.
- **FR-008c**: The published association rule of FR-008a MUST group a declared incident and a
  monitor transition for the same symptoms into ONE investigation, by proximity in time and by
  proximity in the graph neighbourhood of the entities each names. Whichever arrives first opens the
  investigation; the later one MUST attach to it as an additional symptom carrying its own instant,
  origin reference and actor kind. The grouping decision, with the time and neighbourhood distances
  it rested on, MUST be recorded as evidence, and a declared incident that the rule does not group
  MUST open its own investigation rather than be dropped. Two investigations for one incident are a
  defect under FR-008a whichever intake path produced them.

#### B. The investigator and its workers

- **FR-009**: Exactly one reasoning agent — the investigator — MUST own an investigation. All
  access to any source of truth MUST be through workers; the investigator MUST NOT read any source
  directly.
- **FR-009a**: Every worker MUST declare whether it contains a language model. The graph, metrics
  and traces workers MUST NOT contain one: graph access is a typed query contract, symptom
  detection is change-point detection against a seasonality baseline, and trace analysis is
  algorithmic. Logs, documents, chat and postmortem readers MAY contain one with its own context,
  MUST first attempt the algorithmic reduction — template mining, pattern counting — and MUST state
  in the digest what the model added over it.
- **FR-010**: A worker MUST declare: its name, the single source of truth it is bound to, its named
  capabilities with typed inputs and outputs, that it is read-only, how its responses are recorded,
  and the redaction it applies. An undeclared capability MUST NOT be callable.
- **FR-011**: The worker set MUST include, as distinct workers: a graph reader, a metrics reader, a
  logs reader, a traces reader, and a knowledge reader. The contract MUST admit chat and document
  readers on the same terms, to be supplied by later features. No worker may be bound to more than
  one source of truth.
- **FR-012**: The graph worker MUST expose the published query contract of feature 001 — subgraph
  as-of, diff with ranked changes, impact, pointers, node history, resolution audit, extent — and
  MUST pass through both time dimensions. It MUST be deterministic: the same parameters against the
  same graph state MUST produce the same response.
- **FR-013**: The order of worker calls MUST NOT be fixed. The investigator decides what to ask
  next from what it has learned, subject only to budgets. No specific sequence may be required for
  an investigation to be valid, and the specification MUST NOT be read as prescribing one.
- **FR-013a**: Exactly one belief state MUST exist per investigation, held by the investigator.
  Evidence gathering MAY be parallelised across workers; reasoning MUST NOT be forked, duplicated
  or delegated. Two components holding two opinions about the same hypothesis is a defect.
- **FR-014**: Every worker response delivered to the investigator MUST be a structured, bounded
  digest: identifiers, parameters, aggregates, comparisons, mined patterns, exemplar references,
  pointers, join keys, drill-down handles and digests. Raw metric series, raw log bodies and raw
  span payloads MUST NOT be returned by default. A worker MUST return a bounded number of sanitised
  exemplars when the investigator asks for them explicitly — a stack trace is often the decisive
  fact — subject to published per-response size and cardinality caps; exemplars live in the
  investigation recording and MUST NOT reach the graph. Any truncation MUST be stated in the
  response.
- **FR-014a**: Every worker response MUST carry a coverage statement, and a response without one
  MUST be rejected: what was searched, over what window, the volume of data considered, any
  sampling applied, any truncation applied, and the ingestion lag or freshness of the source at the
  time of the call.
- **FR-014b**: Worker responses MUST preserve the join keys that let separate answers be related —
  at minimum the deployment or version tag, the workload or pod identity, trace and span
  identifiers, and first-seen instants — and MUST offer drill-down handles by which the
  investigator can request the next level of detail. At most one bounded free-text field per
  response is permitted; it MUST be flagged unverified and MUST NOT be citable as evidence on its
  own.
- **FR-015**: Every worker MUST run in two modes with identical outputs for identical inputs: live
  against its source, and recorded against a recording. The mode used MUST be recorded per call.
- **FR-016**: A worker MUST NOT expose any capability that changes state in its source. A worker
  declaring such a capability MUST be rejected at registration.
- **FR-017**: Content returned by any worker MUST be treated as data. Instructions, directives or
  requests embedded in retrieved content MUST NOT change the investigation's scope, budgets,
  read-only posture, worker set or output; an attempt MUST be recorded as an evidence item.
- **FR-018**: Worker failures, timeouts and empty results MUST be recorded as evidence items with
  their reason. Retries MUST be recorded individually and counted against budget.
- **FR-018a**: Every worker call MUST carry the hypothesis it serves and the discriminating
  question it is meant to answer, and both MUST be recorded with the call. A call that serves no
  hypothesis MUST be recorded as exploratory, with a stated reason.

#### C. Hypotheses, evidence and confidence

- **FR-019**: An investigation's primary output MUST be a ranked list of hypotheses.
- **FR-019a**: The hypothesis space MUST be open. Every investigation MUST carry, alongside its
  candidate causes, an explicit *no observed change explains this* hypothesis with its own prior
  and its own share of the probability mass, ranked, scored and rendered like any other. `unknown`
  is then a computed result rather than a behaviour of the reasoning layer.
- **FR-020**: Every hypothesis MUST carry: a statement in plain language, its candidate cause (a
  change entity or a named condition), the target entities it concerns, a status (proposed,
  supported, refuted, inconclusive, untested), a numeric confidence, a rationale, and references to
  the evidence items that produced it.
- **FR-020a**: Evidence MUST bear on hypotheses through typed judgments, not prose. Each judgment
  MUST name one hypothesis and one evidence item, MUST state a direction — supports, refutes or
  neutral — and a strength from a published scale, and MUST be recorded. A judgment is the only way
  evidence may change a hypothesis's confidence.
- **FR-020b**: The hypothesis ledger MUST be the investigation's central structure and MUST be held
  outside the model's context: every hypothesis with its prior, the evidence ids for and against it,
  the typed judgments, its status and its computed confidence. It MUST be updated only through
  recorded tool calls, MUST be re-rendered into the investigator's context each turn, MUST be part
  of the exported investigation, and MUST be readable by graders and reviewers without replaying
  the run.
- **FR-021**: Every evidence item MUST record: the worker, the capability, the exact parameters as
  called, the valid and observed instants in force, the time of the call, the mode (live or
  recorded), a reference to the recorded response and its digest, and the graph event identifiers
  or entity versions it rests on where applicable.
- **FR-022**: Every statement in an investigation's output MUST be traceable to at least one
  evidence item or graph element. A hypothesis with no supporting evidence MUST NOT be reported as
  supported.
- **FR-022a**: Before an investigation concludes, a verifier pass MUST check every claim in the
  output against the evidence it cites, from a fresh context that does not contain the
  investigator's reasoning. A claim whose citation does not support it, or whose numbers do not
  match the cited digest, MUST be removed or demoted, and the verifier's findings MUST be recorded
  as part of the investigation.
- **FR-023**: Confidence MUST be computed, never verbalised. It MUST be a deterministic function of
  the hypothesis ledger — the priors and the likelihood ratios implied by the typed judgments — so
  that the same ledger always yields the same confidence; the model MUST NOT be the source of a
  confidence number. Confidence MUST be reported in published coarse buckets until the corpus is
  large enough to justify a finer scale, and each bucket MUST carry its stated range. Calibration
  MUST be measured and published on every evaluation run with a proper scoring rule (Brier or log
  loss), paired against the previous version on the same fixtures, and observed accuracy per bucket
  MUST be reported against that bucket's range.
- **FR-024**: Where evidence conflicts, the engine MUST report the conflict with both sides and
  reflect it in the confidence. Choosing a side without stating why is prohibited.
- **FR-025**: Where two or more candidates cannot be separated by the available evidence, the
  engine MUST report them together at comparable confidence, state that they were not separable,
  and name what would separate them.
- **FR-026**: `unknown` MUST be a valid terminal outcome. An `unknown` outcome MUST list the
  hypotheses considered with their statuses and evidence, and MUST name at least one concrete
  thing — a source, a pointer, an identity confirmation, a wider window — that would resolve it.
- **FR-027**: Coverage limitations MUST be distinguished from negative findings, and four answers
  MUST be kept distinct, each typed and each rendered differently: the query failed; the window is
  covered and contains nothing; the data exists but had not been ingested as of the observed instant
  in force; and the recorded world does not hold this question (`not_recorded`). Only the second is
  evidence that nothing happened, and none of the other three may be reported as if it were. These
  are four of the **six published outcomes** — `digest`, `no_data`, `not_yet_ingested`,
  `query_failed`, `partial`, `not_recorded` (`contracts/telemetry-backend.md` §4, ADR-0005 D7) —
  which are the names used in every feature that implements this contract; `partial` likewise names
  what is missing and MUST NOT be reported as `no_data`.
- **FR-028**: The engine MUST NOT propose, prepare or execute any remediation. Its output is a
  diagnosis with evidence; any statement about what to do next MUST be limited to what to
  investigate or observe.

#### D. Testing hypotheses through pointers

- **FR-029**: For every hypothesis naming target entities, the engine MUST obtain those entities'
  pointers as valid at the investigation instant, and MUST use them to reach telemetry. It MUST NOT
  construct selectors of its own for entities the graph can describe.
- **FR-029a**: The engine MUST estimate the instant at which the symptom began, by change-point
  detection on the alerting metric over a window starting before the alert, and MUST NOT assume the
  alert instant is the onset. The estimate MUST be an evidence item carrying the method, the metric
  and selector, the window, the estimated instant and an uncertainty. Where onset cannot be
  estimated, the engine MUST say so in the output, fall back to the alert instant, and MUST NOT
  exonerate any candidate on timing alone.
- **FR-029b**: The engine MUST obtain its ranked candidate changes with the ranking's reference
  instant set to the estimated onset, rather than to the alert instant or the end of the window. It
  MUST NOT re-rank the candidates itself; any disagreement with the published ranking MUST be
  expressed as a hypothesis with evidence. The changes this requires of feature 001 are listed in
  "Dependencies on feature 001".
- **FR-029c**: A candidate change whose valid start is later than the estimated onset by more than
  the onset's stated uncertainty MUST be typed as a candidate effect and exonerated as a cause, with
  the onset estimate cited, unless independent evidence supports it as a cause — in which case that
  evidence MUST be stated. Exonerations MUST be first-class evidence, carried in the ledger and
  rendered as prominently as supports.
- **FR-029d**: Changes MUST be distinguished by the kind of actor behind them, drawn from feature
  001's published set, whose minimum membership is **`PERSON` ("human"), `AUTOMATION` (CI, a bot or
  a deploy principal acting on a person's behalf), `CONTROLLER` (an autoscaler, scheduler or
  platform reacting to state), `VENDOR` (a provider's maintenance, deprecation or incident) and
  `UNKNOWN`** (ADR-0005 D1). The distinction MUST be used when ranking and exonerating, and MUST be
  stated in the output: a scaling event produced by a `CONTROLLER` reacting to the symptom MUST NOT
  be presented to the reader as an equal suspect to a `PERSON`- or `AUTOMATION`-originated change.
  A **monitor state transition carries no actor kind at all** — nothing made it happen — and the
  engine MUST NOT synthesise one: the kind of front door an intake came through is a separate field
  on the intake, with the published values `monitor` and `declared` (FR-002a).
- **FR-030**: A telemetry test MUST be a comparison between a baseline window and a symptom window
  over the same selector, and its evidence MUST record: the selector, both windows, the statistic
  compared, the direction and magnitude of the difference, and whether it supports, refutes or
  fails to separate the hypothesis.
- **FR-031**: Where a hypothesis cannot be tested because no pointer of the required kind exists,
  or because the backend has no data for the window, the hypothesis MUST be reported with status
  *untested* and the reason. It MUST NOT be dropped, and MUST NOT be scored as refuted.
- **FR-032**: Before relying on a window, the engine MUST consult the graph's extent and MUST flag
  any hypothesis whose evidence falls inside a known source gap.

#### E. Recording and replay

##### What is recorded, and where

Principle IV forbids the graph from storing metric samples, log lines or spans. An investigation's
recording is, by construction, what the investigator saw — which is telemetry-derived. The tension
is real and is resolved by splitting the artifact in two:

- **The investigation decision record** is an event into the graph, which Principle IV explicitly
  permits ("telemetry retrieved at query time ... MUST NOT be written back to the graph, except as
  a *decision record* under Principle V"). It carries identifiers, query parameters, statuses,
  confidences, rationales, spend, and *references and digests* of worker responses. It carries no
  telemetry payload, and is therefore subject to the graph's existing rejection path like any other
  event.
- **The recording** — the worker requests and their responses, in both the trajectory layer and
  the world layer, including any sanitised exemplars a worker returned — lives beside the graph, in
  the investigation store and, for fixtures, in the incident's own directory, exactly as feature
  001's recorded source payloads already do. It is never an event, is never queryable as graph
  state, is keyed by request (or by algebra term), is bounded by published size caps, and is
  redacted according to each worker's declaration.

The graph therefore gains no telemetry and loses no auditability: the chain is in the graph, the
bulk is beside it, and the link between them is a digest.

- **FR-033**: Every worker call and its response MUST be recorded as part of the investigation's
  evidence chain, in the order issued, with timings.
- **FR-034**: Recordings MUST be stored outside the graph. No recording content MUST ever be
  emitted as a graph event. The graph MUST receive only the decision record described above.
- **FR-035**: The investigation decision record MUST be emitted as a typed event, MUST be
  bitemporal like any other fact, MUST survive a full replay of the event log, and MUST link to its
  recording by a stable key and digest.
- **FR-036**: Recordings MUST be keyed by worker, capability, canonicalised parameters and both
  time instants, so that lookup during replay is exact rather than approximate.
- **FR-037**: Recordings MUST be bounded: published per-response and per-investigation size caps,
  with any truncation recorded in the response it applies to, so that a replay reproduces exactly
  what the investigator saw — including the truncation.
- **FR-038**: Each worker MUST declare the redaction it applies before recording (secrets,
  credentials, personal data). A recording MUST NOT contain a value the worker declared it redacts.
- **FR-039**: An investigation MUST be replayable from its recording with no live backend, no
  vendor account and no network access to any source.
- **FR-040**: During *trajectory* replay, a worker request with no matching recorded response MUST
  fail the replay and name the worker, capability and parameters that were unmatched. During *world*
  replay, a request the query algebra can express but the recorded world does not hold MUST return a
  typed `not_recorded` naming the term, after which the investigation continues and reports the
  miss. In both layers, falling back to a live backend, returning an empty result, or substituting a
  similar recording is prohibited.
- **FR-041**: Replay MUST be deterministic at the tool boundary: identical requests MUST yield
  identical responses. Any divergence between an original run and a replay MUST be reported as
  reasoning-layer divergence, identifying the first diverging request.
- **FR-042**: An investigation MUST be exportable as a self-contained artifact — the decision
  record, the hypothesis ledger, the recording in both layers, and the graph events needed to answer
  its graph queries — that replays unchanged on another machine.
- **FR-042a**: Trajectory replay (layer 1) MUST record the investigator's own outputs alongside
  every worker request and response, so that replaying a recorded investigation is byte-identical to
  the run that produced it. Trajectory replay MUST run in continuous integration on every change and
  is the gate on the plumbing: intake, worker boundary, ledger updates, verification, rendering and
  export.
- **FR-042b**: World replay (layer 2) MUST record a world rather than a trajectory: the answers to
  every term of a published, typed query algebra, evaluated over the alert's neighbourhood and
  window as a cross product, so that a run taking a different reasoning path is still served from
  the recording. The algebra MUST include at least `compare(pointer, window_a, window_b)`,
  `new_log_patterns(pointer, window)`, `error_spans(edge, window)` and
  `errors_by_version(pointer, window)`; MUST be versioned and published like any other schema; and
  MUST be the only vocabulary in which a worker may be asked a question — a call outside the algebra
  MUST be refused with a typed reason and recorded. The algebra has three families, published in
  `contracts/telemetry-backend.md` §1: **graph terms are typed wrappers over feature 001's published
  query RPCs (FR-012), answered deterministically from the replayed event log, and are therefore NOT
  cross-producted into a world**; the telemetry family is what a world records; the knowledge family
  is recorded like telemetry. Requiring a graph term to be in the world would be requiring the event
  log to be recorded twice.
- **FR-042c**: A world-replay question inside the algebra that the recording does not hold MUST be
  answered `not_recorded`, a type distinct from "no data", from "not yet ingested" and from a query
  failure. The per-fixture and per-corpus miss rate MUST be published on every evaluation run as a
  measure of fixture quality; a fixture whose miss rate exceeds the published threshold MUST be
  reported as insufficient rather than silently scored.

#### F. Budgets and cost

- **FR-043**: The operator MUST be able to configure, per investigation, a wall-time budget, a cost
  budget, per-backend and per-cost-class call budgets, and a maximum query window width, grouped as
  named budget profiles. A flat total call count is insufficient on its own and MUST NOT be the only
  cap.
- **FR-044**: Every investigation MUST report its consumption against every budget in
  machine-readable form, including calls per worker and elapsed time.
- **FR-045**: On exhaustion of any budget, the investigation MUST terminate cleanly, return the
  hypotheses it has with their statuses and confidences, carry the budget-exhausted outcome with a
  typed stop reason, and name the hypotheses it left untested together with the exact next query for
  each, expressed as a deep link a person can follow. **Exactly 15 %** of every budget MUST be held
  in reserve for the closing synthesis — a published constant, not an approximation, so that two
  implementations reserve the same amount and a fixture can assert it — so that an exhausted investigation still produces a written
  answer rather than stopping mid-sentence. The confidence of any hypothesis whose testing was cut
  short MUST widen rather than freeze at its last computed value. Continuing past a budget and
  discarding partial results are both prohibited.
- **FR-045a**: The engine MUST stop early on diminishing returns: when successive worker calls stop
  changing any hypothesis's status or confidence by more than a published threshold, it MUST
  conclude with a typed diminishing-returns stop reason rather than spend the remaining budget.
- **FR-045b**: Every stop MUST carry a typed reason — completed, budget exhausted naming the budget,
  diminishing returns, worker unavailable, refused, or failed — recorded as a typed event and
  present in both renderings.
- **FR-046**: An in-flight investigation's progress MUST be observable: worker calls made so far
  and spend against each budget, before the investigation finishes.
- **FR-046a**: An investigation MUST be anytime rather than deadline-driven. The graph's own
  deterministic ranking MUST be published as a provisional, explicitly untested answer **within
  5 seconds** of intake (SC-013), and a deterministic first wave of comparisons MUST run in parallel with no
  model in the loop — for each top candidate at least: error rate and latency before and after
  onset, new log patterns, error spans on the path edges, and errors split by version tag — so that
  the reasoning layer's first turn begins with evidence in hand. The first-wave queries MUST always
  be present in a fixture's recorded world.
- **FR-047**: Budget profiles MUST be selectable by alert priority, the defaults MUST be published,
  and the profile applied MUST be recorded on every investigation. Two profiles MUST exist. `page`,
  for a live incident: the first tested hypothesis within 90–120 seconds, a substantive answer
  within 5 minutes, a hard stop at 10 minutes. A **substantive answer** is defined once, here, and
  used unchanged by SC-013: an output carrying **at least one hypothesis at status *supported* or
  *refuted*, together with a verdict line that has passed the citation checker** (FR-022). A
  provisional ranking is not a substantive answer; neither is a set of hypotheses all still
  *proposed* or *untested*. `review`, quality-bound, for postmortems, corpus
  building and nightly evaluation: no wall-time target beyond the operator's cap. Further profiles
  MAY be defined; every profile MUST state its targets and its hard stop.
- **FR-047a**: Budgets MUST be expressed per backend and per cost class rather than as a flat call
  count, MUST cap the width of the query window a worker may request, and MUST express the engine's
  allowance as a share of the backend's *remaining* quota wherever the backend reports it in its
  responses — the binding constraint during an incident is the vendor quota that humans are spending
  at the same time, not the cost of the model. The remaining quota observed and the share consumed
  MUST be recorded per call.
- **FR-047b**: The operator MUST be able to cap wall time, cost, per-backend and per-cost-class
  budgets, concurrent investigations, and daily global spend. The operator MUST NOT be able to relax
  the evidence requirement, the read-only posture, the recording of calls or the verification pass:
  those are not configuration.
- **FR-048**: Cost MUST be reported in a unit that aggregates across investigations and is
  independent of any single provider's price list, so that cost per investigation is comparable
  across runs and over time.

#### G. Knowledge retrieval

- **FR-049**: The knowledge worker MUST retrieve only durable knowledge — postmortems, runbooks,
  architecture decisions, past investigations — and MUST scope retrieval to documents linked to
  entities inside the investigation's subgraph or to its candidate changes. Retrieval over
  telemetry is prohibited.
- **FR-049a**: A human MUST be able to register a durable document as durable knowledge, by
  identifier and location, linked to the graph entities it concerns. Registration MUST create a
  knowledge-document node with an edge to each entity named and a pointer to where the document
  lives; **the document's content MUST NOT be stored**. The registering identity, the instant of
  registration and the provenance of each link MUST be recorded. This is what feeds the v1
  knowledge corpus alongside the past investigations this feature emits (FR-052): no feeder emits
  documents, so a corpus that is not registered is empty, and SC-014 is measured against what has
  been registered.
- **FR-050**: Every retrieved knowledge item MUST be cited with its identifier, the entity or
  entities it is linked to, the provenance of that link, and its age.
- **FR-051**: Knowledge is context, not system state. A hypothesis resting only on a retrieved
  document MUST be reported as knowledge-derived and unconfirmed, and MUST NOT reach status
  *supported* until a source-of-truth worker confirms it.
- **FR-052**: A completed, reviewed investigation MUST become retrievable durable knowledge, linked
  to the entities it concerned, available to later investigations on the same entities.

#### H. Human review, correction and the evaluation loop

- **FR-053**: The engine MUST present a completed investigation's full evidence chain in the order
  it was produced: every worker call, its parameters, its response, its timing, and the hypothesis
  states that followed from it.
- **FR-054**: A human MUST be able to record, against a completed investigation: the validated root
  cause, amendments to any hypothesis's status or rank, and a rationale. Every such decision MUST
  carry the authenticated individual who made it and when, MUST be additive rather than an edit,
  MUST survive a full replay, and MUST take precedence over the engine's own scores wherever the
  two are compared.
- **FR-055**: A reviewed investigation with a validated root cause MUST be exportable as a
  replayable incident — the graph snapshot and telemetry window pointers of feature 001, plus the
  question that was asked and the validated answer — accepted by the evaluation harness with no
  hand-editing.
- **FR-056**: Human corrections MUST be part of the evaluation corpus, and the engine's agreement
  with them MUST be reported as a metric on every evaluation run.
- **FR-057**: The engine's interaction with a human during an investigation MUST be report-only. It
  MUST NOT block on a human at any point, MUST NOT have a state in which it is waiting for one, and
  MUST reach a terminal state whether or not any human responds. Questions for a human MUST be
  rendered as part of "what would resolve this", each with the evidence it would change.
- **FR-057a**: A human MUST be able to push a fact into a running or concluded investigation at any
  time. Such a fact MUST be typed, MUST carry the authenticated individual and the time, MUST enter
  the evidence log as an evidence item, and MUST be non-blocking. It MUST be weighted as strong
  evidence rather than as truth: where telemetry contradicts it, both MUST be kept, the
  contradiction MUST be stated in the affected hypotheses, and neither may be silently dropped.
- **FR-057b**: An investigation MUST support being reopened. A human fact or an answer that bears on
  a concluded investigation MUST move it to *reopened* and produce a new linked record; the
  concluded record MUST remain readable exactly as produced. The lifecycle is running → concluded
  (partial or final) → reopened, and a reopened investigation concludes again under the same rules.
- **FR-057c**: The human-readable rendering MUST be ordered: (1) a one-line verdict naming the
  decisive fact and the rollback candidate, or stating that there is none; (2) the ranked
  hypotheses, each with its evidence for and against, exonerations rendered as prominently as
  supports; (3) a timeline of the candidate changes and the estimated symptom onset; (4) the
  narrative. The narrative MUST NOT precede the verdict or the ranked list, and MUST contain no
  claim absent from them.
- **FR-057d**: Every evidence item, in both renderings, MUST carry a deep link that resolves to the
  exact query, selector and window that produced it, so that a reader can re-run it by hand. An
  evidence item with no resolvable link MUST say why.
- **FR-057e**: After the incident is resolved, the engine MUST collect a single-question "was this
  right?" label against the investigation, stored with its author and time, additive like any other
  human decision, and MUST make it part of the evaluation corpus.
- **FR-057f**: Where the incident was declared by a human in a place that can hold an answer, the
  investigation's output MUST be returned to that place. It MUST be report-only; it MUST be updated
  **in place** as the investigation progresses and concludes, rather than re-posted as a stream of
  messages; and the concluded record itself remains immutable under FR-007, the posting being a
  rendering of it. The posting MUST carry the rendering order of FR-057c and the deep links of
  FR-057d, MUST NOT block the investigation, and MUST NOT be a precondition of reaching a terminal
  state — a delivery that fails is recorded and the investigation still concludes. Returning a
  report is not an action against a production system: the credential that posts it MUST grant
  nothing beyond writing and editing that report, and MUST be separate from every read credential of
  FR-008. The transport that carries it is a connector concern and is out of scope here; this
  feature defines what is returned, not how it travels.

#### I. Evaluation

- **FR-058**: An evaluation harness MUST run the engine over a corpus of replayable incidents, with
  observed time pinned to each incident's instant and every worker in recorded mode, requiring no
  vendor account. It MUST support both replay layers: trajectory replay for the plumbing gate, world
  replay for scoring the reasoning.
- **FR-059**: Each evaluation run MUST publish, in machine-readable form: pass@1 and pass^k, per
  fixture and over the corpus; lift over the prior, the harm rate and citation validity, each **as
  defined in FR-061a**; the rank of the validated root cause among the hypotheses and the top-k hit rate, both
  reported and neither the headline; localisation, attribution and mechanism scored separately
  against the ground-truth causal path, with partial credit; confidence calibration as a reliability
  table over the published buckets with a Brier or log-loss score paired against the previous
  version on the same fixtures; the world-replay `not_recorded` miss rate; onset estimation error
  wherever a fixture labels an onset; time to the provisional ranking, to the first tested
  hypothesis and to conclusion; worker calls and backend quota share per investigation; cost per
  investigation; the rate of `unknown` outcomes and the precision of non-`unknown` outcomes; and the
  replay-divergence rate.
- **FR-060**: The **aggregate pass@1 threshold is set by the first full corpus run** and not
  before: it cannot be guessed ahead of the corpus that measures it, and setting it from a partial
  corpus would be inventing the number the gate exists to defend. Until that run has published it,
  the gate MUST report the threshold as **`unset`** — loudly, in the job summary and in the
  machine-readable report — and MUST NOT pass silently as though a threshold had been met. Every
  other gate below is in force from the first run. The evaluation MUST distinguish gated metrics
  from reported metrics, and the gate MUST fail the build on: the aggregate pass@1 over
  fixtures × runs falling below the published threshold once it is set; lift over the prior falling to or below zero; the harm rate exceeding its published
  threshold; citation validity below 100% — the three as defined in FR-061a; any untraceable conclusion; any replay that improvised a
  missing response; any metamorphic variant whose verdict changed; and any regression against a
  corrected or human-labelled case. Top-k hit rate, calibration, cost and timing are reported and
  trended, not gated on an absolute value. The `not_recorded` miss rate gates a fixture's admission
  to the corpus, never the engine's score. Best-of-k MUST NOT be gated on under any circumstances.
- **FR-061**: A passing run for a non-deterministic investigator MUST be defined as follows and
  applied uniformly. pass@1 — the mean outcome over runs — is the headline, because production gets
  one run. pass^k — all k runs passing — is the reliability figure and MUST be published beside it.
  Best-of-k MAY be computed to describe capability but MUST NOT be gated on. The gate MUST be on the
  aggregate over fixtures × runs, and the published gate MUST state the regression it can actually
  detect: 8 fixtures × 5 runs is 40 Bernoulli trials, which detects a drop from 90% to 70% reliably
  and cannot reliably detect 90% to 80%. Each fixture MUST be run 3 times, extended to 7 only when
  the first 3 disagree. Every evaluation run MUST use the model configuration (model identifier, reasoning effort, output settings) used
  in production, and MUST record them. Majority-of-three MUST NOT be used as the gate: a fixture
  solved 80% of the time passes a majority-of-three about 90% of the time, and eight such fixtures
  would let a green build mean nothing.
- **FR-061a**: The primary quality metrics MUST be lift over the prior, the harm rate and citation
  validity, and **this requirement is their single definition**; FR-059 and FR-060 refer to it and
  MUST NOT restate it.
  - **Lift over the prior** = the investigator's mean reciprocal rank of the validated culprit
    **minus** the deterministic ranker's, over the same corpus and the same runs.
  - **Harm rate** = the fraction of runs in which the deterministic prior placed the validated
    culprit first and the investigation did not.
  - **Citation validity** = the fraction of claims for which the claim resolves to at least one
    evidence id **and** every number appearing in the claim matches a field of the digest it cites.
    It is a hard 100 % gate.
- **FR-061b**: Every corpus incident MUST carry ground truth in a published shape: the culprit as a
  change identifier, or `unobserved`, or `not_change_induced` with a category; the causal path from
  cause to symptom as a chain of entities and edges, so that localisation, attribution and mechanism
  are graded separately and with partial credit; the decisive evidence as predicates over response
  digests rather than as strings, together with the exonerating evidence for each decoy; the
  knowability time, being the earliest observed instant at which the decisive evidence existed, such
  that `unknown` asked before that instant is scored as correct; and provenance — synthetic,
  recorded or derived — with the sanitiser policy version that produced it.
- **FR-062**: The corpus MUST include at least: an incident whose expected outcome is `unknown`, an
  incident with two simultaneous plausible changes, an incident whose window overlaps a feeder gap,
  and an incident whose alert names an alias of a merged entity.
- **FR-062a**: The corpus MUST be multiplied mechanically, and the derived fixtures MUST be graded
  on the invariant each was built for: deleting the culprit event MUST change the expected answer to
  `unobserved` with the symptoms still localised; injecting far-away decoys, shifting every
  timestamp by a constant, and permuting entity names MUST NOT change the verdict — the permutation
  exists to test that the engine ranks on structure rather than on guilty-sounding names. Derived
  fixtures MUST record their provenance and the transformation applied.
- **FR-062b**: The corpus MUST include fixtures on which the deterministic prior fails, at minimum:
  a slow-burn change roughly three hours before the alert; a culprit roughly three hops from the
  alerting node; and a decoy deployed about two minutes before the alert on an adjacent service.
  Each MUST record the prior's own rank for the culprit, so that lift can be measured where it
  matters.
- **FR-063**: Evaluation MUST extend the existing fixture verification rather than introduce a
  separate harness, and MUST NOT require a new input format beyond the replayable incident plus the
  question and answer fields this feature adds.

#### J. Interface and operability

- **FR-064**: Every capability of this feature MUST be reachable through a command-line interface,
  producing human-readable and machine-readable output from the same run. A named **dual-surface
  subset** MUST additionally be reachable through the programmatic interface, with the two surfaces
  producing the same claims from the same run: `investigate` (run), `declare`, `get`, `replay`,
  `reopen`, `fact`, `report`, `label`, `list` and `export`. **Operator tooling is command-line only
  by design** — `audit coverage`, `worker record`, `worker call`, `worker list`, `backend list`,
  `knowledge link` and `fixture derive`/`record-world` are run by an operator at a terminal or in
  CI, not by a caller over the wire, and publishing them as RPCs would widen the served surface for
  no consumer. No chat interface and no graphical interface are in scope.
- **FR-065**: The engine MUST emit telemetry about its own operation — investigations started and
  completed by outcome, worker calls and latencies by worker and capability, budget consumption,
  failures — in the same form the rest of the system uses.
- **FR-066**: Every investigation, and every human review decision, MUST record the authenticated
  individual or service identity responsible for it. Anonymous or shared credentials MUST be
  refused.
- **FR-067**: The engine MUST be fully operable with no vendor connector configured, using recorded
  backends only. Shipping and evaluating this feature MUST NOT depend on any vendor account.
- **FR-068**: The public artifacts of this feature — the worker contract, the investigation record,
  the hypothesis ledger, the evidence item and its typed judgments, the query algebra, the budget
  declaration, and the incident-format extension including the ground-truth shape — MUST be
  published as versioned, machine-readable definitions with documentation, under the project's
  existing schema versioning rules.

#### K. The coverage audit

- **FR-069**: This feature's first increment MUST deliver a coverage audit. Given a list of past
  incidents with their alert instants and their known causes — supplied by the operator or the
  reviewer, never derived by the engine — it MUST classify each incident as *cause present in the
  graph at alert time*, *cause absent* with a named category, or *undecidable* with a reason, and
  MUST publish the aggregate fraction as the recall ceiling, in machine-readable form, with the
  number of incidents it rests on. The default list is the last 20 incidents, or the
  organisation's whole classifiable set where it holds fewer; the count the ceiling rests on MUST
  be published with it either way.
- **FR-069a**: The first audit MUST be carried as a recorded result rather than as a procedure still
  to be run. Its aggregate — 13 classifiable production incidents over nine months; the cause
  present in the graph at alert time in 0 of 13 with the feature 001 feeders alone, 3 of 13 with
  deploy feeders for the platforms actually in use, and 8 of 13 with vendor-notice feeders; a
  ceiling of about 62 % — MUST be published with the feeder set each figure was measured against
  and with the exclusions that produced the count, and every later run MUST be comparable to it.
  Incident-level detail MUST remain outside this repository.
- **FR-070**: The audit MUST report the missing causes by category — at minimum feature flag flips,
  infrastructure-as-code applies, database migrations, certificate expiries, scheduled jobs,
  third-party outages and traffic shifts — with counts, so that the categories read as a ranking of
  the feeders worth writing next.
- **FR-071**: The ceiling bounds **recall- and coverage-dependent targets only** — those whose
  value cannot exceed the share of incidents whose cause is a node or a change in the graph at alert
  time: the rank of the validated culprit, the top-k hit rate, lift over the prior, and localisation
  against the ground-truth causal path. No such target or gate may be set above the measured
  ceiling, and each MUST cite the audit it rests on. Targets that measure **precision, latency,
  invariance or validity** are NOT bounded by it and MUST NOT be scaled down to it: citation
  validity is 100 % whether or not the cause was observable, a replay is byte-identical or it is
  not, a 5-second provisional ranking does not become slower because a feeder is missing, and
  answering `unknown` correctly is the *complement* of the ceiling rather than a casualty of it.
  Every success criterion states which side it is on, in the table after the criteria below, and the
  ceiling guard checks the ceiling-bounded set and nothing else. The audit MUST be repeatable
  against a longer list or a graph with more feeders, and successive runs MUST be comparable, so
  that a feeder's effect on the ceiling is measurable rather than asserted.
- **FR-071a**: The audit MUST be re-run after each connector feature ships, over the same incident
  list, and each run MUST publish the ceiling it measured, the feeder set it was measured against,
  and the target ceiling then in force. The difference from the previous run MUST be attributed to
  the feeders that were added; a ceiling that did not move MUST be reported as such rather than
  omitted. No connector feature is complete until its effect on the ceiling has been measured and
  published.
- **FR-071b**: Every category of the audit's unobservable remainder — at minimum **latent bug**,
  **client-side configuration**, **business data change** and **credential leak** — MUST be
  represented in the evaluation corpus by at least one fixture whose ground truth is `unobserved` or
  `not_change_induced` carrying that category, in the shape FR-061b publishes. Those fixtures MUST
  be graded under SC-023: on them the correct answer is that no observed change explains the
  symptom, and naming a decoy is a failure. A category with no fixture MUST be reported as a gap in
  the corpus on every evaluation run.

### Key Entities

- **Investigation**: one run, covering one incident rather than one alert. Identifier, subject
  entity references, symptom statement(s) with the alerts that raised them, valid instant, observed
  instant, estimated symptom onset, window, mode (alert-driven or review), budget profile, requester
  identity, lifecycle status (running, concluded partial, concluded final, reopened, failed),
  outcome with its typed stop reason, spend, start and end times, and a link to the investigation it
  reopens where applicable. Links to its hypothesis ledger, evidence items, worker call records and
  recording.
- **Alert intake**: what the investigation was asked, in either of its two forms — a monitor state
  transition, or an incident declared by a human. Origin system and reference (the stable identifier
  of the place the incident lives, not its name); **origin kind**, whose published values are
  `monitor` and `declared` — a field of its own, *not* an actor kind: a monitor transition carries
  **no actor kind at all**, because nothing made it happen, while a declaration carries the actor
  kind `PERSON` alongside its origin kind `declared`; the declaring identity where there is one;
  symptom statement; the fired-at or declared instant; the declared
  severity and title where the incident was declared; the identifiers it named with the provenance
  of each (the parse rule's id and version, or supplied by a human); the idempotency
  key it was keyed on — always the 4-tuple (source, stable alert identifier, group, transition
  instant), with `group` empty for a human declaration; and the canonical entities those identifiers resolved to (with the resolution
  evidence). Where an intake attached to an existing investigation instead of opening one, the
  grouping decision that attached it and the distances it rested on.
- **Hypothesis**: a candidate explanation. Statement, candidate cause (a change entity, a named
  condition, or the explicit *no observed change explains this*), target entities, prior, status
  (proposed, supported, refuted, inconclusive, untested, exonerated), causal role (cause or
  candidate effect), computed confidence with its bucket, rationale, rank, references to the
  judgments and evidence items that produced it, and — where applicable — the knowledge items it was
  derived from and whether it was confirmed.
- **Hypothesis ledger**: the investigation's central structure, held outside the model's context.
  Every hypothesis with its prior, the evidence ids for and against it, its typed judgments, its
  status and its computed confidence, plus the version of the posterior rule used. Updated only
  through recorded tool calls, re-rendered into the investigator's context each turn, exported with
  the investigation, and readable by graders and reviewers without replaying the run.
- **Judgment**: one typed link between one evidence item and one hypothesis. Direction (supports,
  refutes, neutral), strength on the published scale, the worker call it came from, and the time it
  was recorded. The only mechanism by which evidence may move a confidence.
- **Symptom onset estimate**: when the symptom actually began. Method, alerting metric and selector,
  the window examined, the estimated instant, an uncertainty, and a fallback flag when onset could
  not be estimated and the alert instant was used instead. Recorded as an evidence item and used as
  the ranking's reference instant.
- **Human fact**: a typed, authenticated, non-blocking statement pushed into an investigation by a
  person. Kind, statement, entities and instants it concerns, author, time, and the weight class
  applied. Strong evidence, never truth; contradictions with telemetry are stated, not resolved.
- **Evidence item**: one fact the investigation rests on. Worker, capability, parameters as called,
  valid and observed instants, call time, mode, recorded-response reference and digest, and the
  graph event identifiers or entity versions involved.
- **Worker**: a read-only capability provider bound to exactly one source of truth. Name, source,
  declared capabilities with typed inputs and outputs, recording behaviour, redaction declaration,
  supported modes.
- **Worker call record**: one request and its response. Request key (worker, capability,
  canonicalised parameters, instants), response digest, size and truncation, duration, outcome
  (answered, empty, failed, timed out), cost attributed, sequence position in the investigation.
- **Budget and spend**: the profile applied — wall time and its targets, cost, per-backend and
  per-cost-class call budgets, maximum query window width, the share of remaining vendor quota
  allowed, the synthesis reserve — and the measured consumption against each, including the
  remaining quota observed per backend.
- **Outcome**: the terminal state of an investigation — resolved with a ranked list, `unknown`,
  budget-exhausted, or failed — with its typed stop reason (completed, budget exhausted naming the
  budget, diminishing returns, worker unavailable, refused, failed), the verdict line, and, for
  `unknown` and for every untested hypothesis, what would resolve it with the exact next query as a
  deep link.
- **Human review**: a person's judgement on a completed investigation. Validated root cause,
  hypothesis amendments, rationale, authenticated individual, time. Additive and durable.
- **Knowledge item**: a durable document retrieved during an investigation. Identifier, kind
  (postmortem, runbook, decision record, past investigation), the entities it is linked to, the
  provenance of that link, age, and the citation used.
- **Investigation recording**: what makes an investigation replayable, in two layers, stored beside
  the graph, bounded and redacted. *Trajectory*: the keyed worker requests and responses plus the
  investigator's own outputs, sufficient for a byte-identical replay. *World*: the answers to every
  term of the query algebra over the alert neighbourhood and window, as a cross product, sufficient
  to serve a different reasoning path; a term it does not hold answers `not_recorded`.
- **Query algebra term**: one question a worker may be asked. The full term list, its three
  families and which of them a recorded world holds are published in
  `contracts/telemetry-backend.md` §1, which is their single home: **graph** terms are typed
  wrappers over feature 001's query RPCs and are replayed from the event log rather than recorded
  into a world; the **telemetry** family is the eight terms a telemetry backend serves and is what a
  world cross-products; **knowledge** is `knowledge_search`. A term carries its typed arguments —
  pointer or edge, and the window(s) — and the algebra version. Nothing outside the algebra may be
  asked of a worker.
- **Coverage audit**: the measured ceiling on recall. The incident list it was run over with each
  incident's alert instant and known cause, the per-incident classification (cause present, cause
  absent with a category, undecidable with a reason), the aggregate fraction, the per-category
  counts, and the graph configuration — which feeders were present — it was measured against.
- **Replayable incident (extended)**: feature 001's triple — graph snapshot at T, telemetry window
  pointers, validated root cause — plus the question asked, the recorded world and trajectory that
  make the question answerable offline, and ground truth in its published shape: the culprit as a
  change id or `unobserved` or `not_change_induced` with a category; the causal path as an entity
  and edge chain; the decisive evidence as predicates over digests, with the exonerating evidence
  for each decoy; the knowability time; and provenance — synthetic, recorded or derived — with the
  sanitiser policy version. A derived fixture also carries the transformation that produced it and
  the fixture it came from.

## Dependencies on feature 001

Ranking against symptom onset, and exonerating the changes that came after it, are requirements on
how this feature *uses* the ranker (FR-029a to FR-029d). Some of what they need cannot be supplied
by 002 at all. The list below is what feature 001 owes this feature; each item is a change to 001,
specified there, and none of it is implemented here.

- **D1 — a change that starts after the reference instant must not score as the freshest thing that
  happened.** The published score computes `dt = T_ref - c.valid.start` *floored at zero*
  (`docs/schema/ranking.md`), so a change one minute after the reference instant scores
  `temporal = 1.0`, the maximum. Once the reference instant moves from the end of the window to the
  estimated onset, every effect of the incident — the autoscaler reacting to retries above all —
  becomes the top-ranked suspect. Feature 001 MUST define and publish the behaviour for candidates
  later than the reference instant: at minimum they MUST NOT receive the maximum temporal weight,
  and the caller MUST be able to identify them as post-reference.
- **D2 — the signed time distance.** `RankedChange.time_distance_seconds` carries the floored `dt`,
  so a change ten seconds before the reference instant and one ten minutes after it are
  indistinguishable in the response. 002 must tell them apart in order to exonerate (FR-029c).
  Feature 001 MUST expose either a signed distance or an explicit post-reference flag on every
  ranked item.
- **D3 — a typed actor kind on changes.** A change node carries an actor, but not a typed *kind*.
  002 requires a published actor kind — at minimum `PERSON`, `AUTOMATION`, `CONTROLLER`, `VENDOR`
  and `UNKNOWN` (ADR-0005 D1) — in the change taxonomy, in the event schema, and on the ranked item, so that FR-029d
  can type and render the distinction. Whether the score weights the actor kind is 001's decision;
  carrying it is not optional.
- **D4 — join keys in the pointer vocabulary.** `errors_by_version(pointer, window)` (FR-042b) is
  expressible only if a pointer names the attribute identifying the deployed version, and the same
  holds for the workload, pod and trace identifiers that FR-014b requires workers to preserve.
  Feature 001 MUST publish those join keys as part of the pointer vocabulary, in OpenTelemetry
  terms.
- **D5 — the event types this feature emits.** The investigation decision record (FR-035), the typed
  human fact (FR-057a), the reopen link (FR-057b), the "was this right?" label (FR-057e) and
  **`alert.transition` (FR-008b)** — the fifth, and the one both intake front doors normalise into,
  carrying source, alert reference, group, transition instant, from/to state, severity, title,
  actor kind where there is one, declaring identity for a human declaration, and target refs with
  per-ref provenance — are events into the graph. Feature 001 MUST admit all five in its published
  event schema and its telemetry rejection path, under its existing versioning rules, and MUST
  publish the idempotency key of `alert.transition` as the 4-tuple of FR-008b (ADR-0005 D2).

Two things are explicitly **not** dependencies:

- **The reference instant itself.** `DiffRequest.reference_at` already accepts a caller-supplied
  instant, so passing the estimated onset needs no change to the query contract. What D1 and D2 ask
  for is what the ranker does with it.
- **Onset estimation.** Change-point detection on the alerting metric belongs to this feature's
  metrics worker (FR-029a). It reads telemetry through pointers and writes nothing; the graph
  neither computes nor stores an onset.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The investigator beats the prior it is given. Measured as pass@1 over at least three
  runs per fixture under the production model configuration, its mean reciprocal rank over the corpus is
  strictly greater than the deterministic ranker's on the same fixtures, and the aggregate pass@1
  over fixtures × runs meets the published gate — which is published together with the size of
  regression it can actually detect. The rank of the validated root cause and the top-k hit rate are
  reported on every run; neither is the headline, because on the current corpus the ranker alone
  already places the culprit first and "top three of four" has a 75% random baseline.
- **SC-002**: 100% of statements in every investigation output are traceable to at least one
  evidence item or graph element; zero unsupported conclusions across the corpus. This is the
  **deterministic half** of traceability — the checker resolves every claim to an evidence id
  without a model — and SC-017 is the **verified half** that additionally requires every number to
  match its digest and a fresh-context verifier to confirm the claim. Both must hold; neither
  replaces the other.
- **SC-003**: 100% of recorded investigations replay with every worker request matched from the
  recording, zero live calls, and worker responses identical to those recorded. This is the
  **deterministic half** of replay — the worker boundary — and SC-018 is the **verified half**
  covering both layers: byte-identical trajectory replay and a published, bounded `not_recorded`
  miss rate in world replay. Both must hold.
- **SC-004**: Confidence is computed from the hypothesis ledger rather than stated by the model, and
  is calibrated within its published buckets: every evaluation run publishes a reliability table
  over those buckets and a Brier or log-loss score paired against the previous version on the same
  fixtures, and observed accuracy in every bucket holding at least 20 hypotheses lies within the
  bucket's stated range.
- **SC-005**: 95% of investigations complete within the wall-time budget in force, and 100% of
  those that do not terminate cleanly with a typed stop reason, a written synthesis produced from
  the reserved budget, and partial results that are never presented as complete.
- **SC-006**: Measured as pass@1, when the engine returns a non-`unknown` outcome its top
  hypothesis is the validated root cause in at least 85% of corpus incidents, while the `unknown`
  rate on incidents that do have a validated root cause stays at or below 20%.
- **SC-007**: On every incident whose expected outcome is `unknown`, the engine returns `unknown`
  and names at least one concrete resolving action; zero such incidents produce a supported
  hypothesis.
- **SC-008**: 100% of investigations report worker calls and cost per investigation, and the
  evaluation run publishes both as aggregates over the corpus.
- **SC-009**: 100% of hypotheses whose target entities carry a pointer of the needed kind are
  tested with a before/after comparison recorded in the evidence; every remaining hypothesis is
  labelled untested with a reason.
- **SC-010**: Zero telemetry payloads reach the graph: every event this feature emits passes the
  graph's telemetry rejection path, and a fixture demonstrates that the decision record contains
  only identifiers, parameters, digests, confidences and rationale.
- **SC-011**: An exported investigation replays successfully on a machine with no vendor
  credentials and no network access to any telemetry backend, in 100% of exported cases.
- **SC-012**: A reviewer can turn a completed investigation into a corpus incident in a single
  command with no hand-editing, and the resulting incident passes the harness on first run.
- **SC-013**: Under the `page` profile, the provisional prior-only ranking is available within 5
  seconds of intake in at least 95% of investigations, the first *tested* hypothesis within 120
  seconds in at least 90%, and a substantive answer within 5 minutes in at least 90%; no
  investigation runs past the 10-minute hard stop.
- **SC-014**: For every corpus incident that has durable knowledge linked to its affected entities,
  the relevant document is cited in at least 90% of runs, and documents linked only to entities
  outside the investigation's subgraph are cited in 0%.
- **SC-015**: 100% of human review decisions and corrections survive a full replay of the event
  log, remain attributable to an authenticated individual, and remain in force.
- **SC-016**: The harm rate — the fraction of runs in which the deterministic prior placed the
  validated culprit first and the investigation did not — is at most 5% across the corpus, and the
  build fails above that value.
- **SC-017**: Citation validity is 100%: every claim in every investigation resolves to an evidence
  id, every number inside a claim matches the digest it cites, and a fresh-context verifier pass
  confirms it. Any failure fails the build. This is the **verified half** of SC-002's deterministic
  traceability check, not a restatement of it.
- **SC-018**: Replay holds in both layers — the **verified half** of SC-003's deterministic worker
  boundary: 100% of recorded investigations replay byte-identically in trajectory mode, and in world mode every question asked inside the algebra is either answered
  from the recorded world or typed `not_recorded` — never improvised — with the per-fixture miss
  rate published and at or below 5% for every fixture admitted to the corpus.
- **SC-019**: On every fixture that labels a symptom onset, the estimated onset falls within the
  fixture's stated tolerance in at least 90% of runs, and every candidate change that starts after
  onset is exonerated with the onset estimate cited in at least 90% of runs.
- **SC-020**: The metamorphic invariants hold: 100% of timestamp-shifted, entity-name-permuted and
  far-decoy variants produce the same verdict as their original, and 100% of culprit-deleted
  variants produce `unobserved` rather than naming a decoy.
- **SC-021**: The corpus contains at least three fixtures on which the deterministic prior does not
  rank the culprit first, and on those fixtures the investigator's lift over the prior is positive
  and is reported per fixture.
- **SC-022**: The coverage audit is published before any accuracy gate is set, and its first result
  is on the record rather than pending: 13 classifiable incidents classified (two security incidents
  excluded, one unclassifiable), a recall ceiling of about 62 % stated with that incident count and
  with the feeder set it was measured against — 0 of 13 with the feature 001 feeders alone, 3 of 13
  with deploy feeders, 8 of 13 with vendor-notice feeders — missing causes counted by category, and
  every published **ceiling-bounded** accuracy target (FR-071) at or below that ceiling. Every later
  run is published the same way, after the connector feature that prompted it, and is comparable to
  the first.
- **SC-023**: On every corpus incident whose ground truth is `unobserved`, the explicit *no observed
  change explains this* hypothesis ranks first in at least 90% of runs, and no decoy is reported as
  supported in any run.
- **SC-024**: No investigation ever waits for a human: 100% of runs reach a terminal state without
  human input, and 100% of human facts submitted against a concluded investigation produce a
  reopened, linked record with the original readable and unchanged.
- **SC-025**: 100% of evidence items, in both renderings, carry a deep link resolving to the exact
  query, selector and window, or state why no link exists; and 100% of human-readable outputs follow
  the published order — verdict, ranked list with exonerations, timeline, narrative.

### Which criteria the coverage ceiling bounds (FR-071)

The ceiling measured by the coverage audit bounds only what depends on the cause being observable.
Everything else is graded at its own value, on every corpus, whatever the ceiling is.

| criterion | ceiling |
|---|---|
| SC-001 (pass@1, MRR, lift over the prior) | `[ceiling-bounded]` |
| SC-006 (top hypothesis is the validated root cause) | `[ceiling-bounded]` |
| SC-021 (lift on the fixtures where the prior fails) | `[ceiling-bounded]` |
| SC-002, SC-004, SC-008, SC-009, SC-010, SC-012, SC-014, SC-015, SC-017, SC-019, SC-022, SC-024, SC-025 | `[not ceiling-bounded: validity]` |
| SC-003, SC-011, SC-018, SC-020 | `[not ceiling-bounded: invariance]` |
| SC-005, SC-013 | `[not ceiling-bounded: latency]` |
| SC-007, SC-016, SC-023 | `[not ceiling-bounded: precision]` |

The ceiling guard in the build checks the three `[ceiling-bounded]` rows against the audit each
cites, and nothing else.

## Assumptions

- Feature 001 is complete and is the only substrate: every structural fact the investigator uses
  comes from the published query contract, never from a source directly.
- Features 002 and 003 are specified together; 002 is implemented first, against recorded data, and
  carries no vendor dependency. Live telemetry backends — the first being a vendor connector —
  arrive in 003 and must satisfy the same worker contract with identical outputs in both modes.
- Chat and document readers are declared in the worker contract but ship with later features. Their
  absence must not block an investigation; the engine reports the source as unavailable.
- The recorded telemetry backend for v1 is populated from incident recordings, not from a live
  vendor: pointer query in, recorded response out.
- Alert-driven investigations pin observed time to the alert instant (ADR-0001 D2). Review mode
  exists, is explicit, and is recorded.
- Ranking of changes remains the deterministic graph query published in feature 001. The
  investigator consumes that ranking — asked with the reference instant set to the estimated symptom
  onset — and does not replace it; any disagreement with it is expressed as a hypothesis with
  evidence. The changes this requires of 001 are listed in "Dependencies on feature 001" and are
  assumed to land before the evaluation gate is set; until they do, post-onset changes cannot be
  exonerated on timing and the engine says so.
- Confidence is a property of a hypothesis, not of the investigation as a whole; it is computed from
  the hypothesis ledger by a published, deterministic rule, and an investigation's outcome is
  derived from its hypotheses' statuses and confidences.
- The evaluation corpus starts from the fixtures shipped with feature 001, is multiplied
  metamorphically and extended with adversarial fixtures (User Story 10), and grows further through
  reviewed real investigations (User Story 7).
- The coverage audit's input — a list of past incidents with their alert instants and known causes —
  exists outside the engine and is supplied by the operator or the reviewer. The audit measures; it
  does not infer causes.
- How a human declares an incident is organisation-specific, and is configuration rather than a rule
  of this feature. In the audited organisation it is the creation of a dated severity channel named
  `#sevN-YYYY-MM-DD-<slug>`, from which the severity, the date and any target references are read by
  a published, configurable rule; another organisation's convention — a record in an incident
  tracker, a declared page — is the same event shape reached by a different transport. The
  convention is never load-bearing on its own: an unparseable declaration is still a valid
  investigation (FR-002b).
- The first coverage audit has been run (September 2026) and its aggregate is published. The ceiling
  it measured — about 62 % with the best realistic feeder set — is the number every accuracy target
  in this specification cites until a later run replaces it, and the 23 % it could not reach is
  ground truth for `unobserved` and `not_change_induced` fixtures rather than a target to close.
- The production model configuration (model identifier, reasoning effort, output settings; no temperature exists on the models in use) is known, are used by the evaluation harness,
  and are recorded with every run. A change to either is a change that must be evaluated.
- Vendor quota accounting depends on backends reporting remaining quota in their responses. Where a
  backend does not, the engine falls back to its configured absolute per-backend budget and records
  that it did.
- The sanitiser and its policy versioning are defined by the connector feature (003). This feature
  only records the policy version that produced each fixture, as part of ground-truth provenance.
- The three questions that were open in this specification — FR-047 (budgets), FR-057
  (human-in-the-loop and output form) and FR-061 (what counts as a passing run) — are answered in
  Clarifications (Session 2026-09-17) and applied to the requirements above. What remains for
  planning is calibration of the published numbers: the pass@1 gate value, the harm-rate and
  miss-rate thresholds, the diminishing-returns threshold, the confidence buckets, the judgment
  strength scale and the per-backend cost classes. Each is a number to be set with the corpus and
  the coverage audit in hand, not an unanswered question of shape.

## Out of scope for this feature

- Remediation of any kind: proposing, preparing, gating or executing an action against a production
  system. The engine diagnoses; the constitution forbids the rest until it is amended.
- Chat interfaces and graphical interfaces. Command line and programmatic interface only.
- Vendor connectors and live telemetry backends: those are feature 003, and this feature must be
  complete and evaluable without them.
- Retrieval-augmented generation over telemetry. Retrieval is limited to durable knowledge linked
  to graph entities.
- Changes to the graph schema, the query contract or the ranking formula of feature 001. This
  feature names the five it depends on in "Dependencies on feature 001"; they are specified and
  implemented in 001, not here.
- Blocking human interaction of any kind: approval steps, confirmation prompts, or any state in
  which the engine waits for a person. The human channel is non-blocking in both directions.
- Extending the query algebra beyond its published terms during an investigation. New terms are a
  published schema change with a corresponding extension of every recorded world, not something a
  worker or the investigator may improvise.
- Alert intake transports — webhooks, polling, deduplication against a vendor's incident records,
  and the chat transport that observes a human declaring an incident and carries the report back to
  where it was declared — together with the sanitisation policy applied before anything reaches
  disk. All belong to connector features; a future chat connector owns the declared-incident
  transport and the delivery. This feature defines the declared-incident event shape, the declared
  intake path and what is returned (FR-001a, FR-002a, FR-002b, FR-008b, FR-057f), consumes
  normalised intake, and records the sanitiser policy version.
- Scheduling and scoping the recording campaign (which environments, which game days, which
  periods). This feature defines what a fixture must carry, not how the organisation goes about
  collecting them.
- Automatic learning from feedback: corrections are persisted and evaluated against, but no
  automated tuning of ranking or confidence from them is in scope here.
- Multi-investigation correlation, incident timelines spanning several alerts, and deduplication of
  concurrent pages.
