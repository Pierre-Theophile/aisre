# The hypothesis ledger

The **ledger** is the investigation's central data structure and its only belief state. It is held
outside the model's context, updated only through recorded tool calls, re-rendered into the
investigator's context each turn, and exported with the investigation so a reviewer or a grader can
read it without replaying the run (FR-020b).

The machine-readable source of truth is
[`api/sreagent/investigation/v1/investigation.proto`](../../api/sreagent/investigation/v1/investigation.proto)
§4; the storage shape is `internal/store/postgres/migrations/0006_investigation.sql`; the
implementation is `internal/investigation/ledger` and `internal/investigation/store`.

Everything on this page is **published**: a number here is a number a second implementation must
reproduce, and it moves only with `ledger_rule_version`, recorded on every investigation.

- Ledger rule version: **1.0.0**

## Confidence is computed, never verbalised

```text
ln posterior_i = ln prior_i + Σ_j ln LR_ij          normalised by a stable log-sum-exp
```

rounded to six decimals (FR-023). The model has no tool that writes a confidence; an attempt to
supply one is refused with `model_confidence`.

Three properties make that line worth resting on:

- **Order independence.** Addition commutes, so the posterior cannot depend on the order judgments
  arrived in. This is asserted as a `rapid` property test over every permutation of a judgment
  multiset — the formal statement of "two runs that gather the same evidence agree" (Invariant 1).
- **Reproducibility from the rows.** `hypotheses.prior` and `judgments.ln_lr` are columns, so
  recomputing from the database alone reproduces the published confidence exactly at six decimals,
  years later and whatever this page says by then.
- **Exact normalisation.** The six-decimal rounding is a *largest-remainder apportionment* over
  1 000 000 millionths, not an independent rounding of each number: each share takes its floor in
  millionths and the leftover units go to the largest fractional remainders, ties broken by
  hypothesis id. The set therefore sums to exactly `1.000000`. Rounding each number on its own
  would drift up to 2.5 × 10⁻⁵ over 50 hypotheses, which is 25 times the tolerance of the deferred
  `hypotheses_posteriors_sum_to_one` trigger (Invariant 10).

### Priors

A candidate's prior is its **published ranker score** (`docs/schema/ranking.md`), normalised over
the candidate set and scaled by `1 − π₀`. The `no_observed_change` hypothesis takes **π₀**, the
coverage audit's measured ceiling subtracted from one:

```text
π₀ = 1 − ceiling                       internal/investigation/audit.PriorFromAudit
```

With the published first audit — 13 classifiable incidents, a measured ceiling of
`8/13 = 0.615385` (`docs/evaluation/coverage-audit-2026-09.md`, ADR-0004) — **π₀ = 0.384615**.

**No audit means π₀ = 1.** Every candidate then sits at prior 0 and the open hypothesis holds all
the mass, so an engine with no measured ceiling reports `unknown` rather than a confident guess.
That is not a defensive default; it is the only honest reading of "nothing measured says any cause
is observable here".

π₀ is recorded on every investigation together with the audit id, the ceiling, the feeder set and
the incident count it rests on (ADR-0005 D9), because a confidence stored in 2026 is only
interpretable in 2028 if the ceiling it was computed against is still readable.

## The likelihood-ratio table

A judgment names **one hypothesis and one evidence item**, and carries a **direction** and a
**strength** from the published five-point scale. It is the only mechanism by which evidence moves
a confidence (FR-020a).

| strength | LR (`supports`) | ln LR | LR (`refutes`) | ln LR (`refutes`) |
|---|---|---|---|---|
| `weak` | 1.5 | +0.405465 | 1/1.5 | −0.405465 |
| `moderate` | 3 | +1.098612 | 1/3 | −1.098612 |
| `strong` | 10 | +2.302585 | 1/10 | −2.302585 |
| `decisive` | 50 | +3.912023 | 1/50 | −3.912023 |
| (`neutral`, any strength) | 1 | 0 | 1 | 0 |

The ln values are the six-decimal rounding of each ratio, written out rather than computed:
`judgments.ln_lr` is `numeric(12,6)`, so a value with more precision could not survive the
round-trip, and a posterior recomputed from the rows would not reproduce the live one. A test
asserts each constant equals `round6(ln LR)`, so the table cannot drift from its own documentation.

The scale is deliberately coarse — four rungs an SRE can hold in their head — and no rung decides
an investigation on its own: even a decisive judgment leaves the open hypothesis a share of the
mass, because the set is closed and always contains it.

### Caps and sources

- **At most one judgment per `(evidence item, hypothesis)` pair.** The same fact counted twice is
  the same fact believed twice. A repeated worker call produces a second *evidence item*, which is
  correct; a second judgment on the same pair is refused with `duplicate_judgment`, in the ledger
  and by `judgments_one_per_pair` in the schema.
- **A human fact enters at `strong`, never `decisive`** (FR-057a). It is strong evidence, not
  truth: one assertion may not end an investigation.
- **An exoneration enters at `decisive` `refutes`** by default, sourced `exoneration`. It rests on
  an `onset_estimate` evidence item, and it is an argument from the clock — a change that starts
  after onset by more than the onset's own uncertainty is not a candidate cause. Exonerating also
  re-roles the hypothesis as `candidate_effect` and is rendered as prominently as a support
  (FR-029c, FR-057c).
- **A neutral judgment is recorded and moves nothing.** "We looked and it did not separate" is a
  different answer from "we did not look", and the ledger keeps them apart.

## The five confidence buckets

Confidence is reported in published coarse buckets until the corpus justifies a finer scale, and
**each bucket carries its range** wherever it is rendered (FR-023).

| bucket | range |
|---|---|
| `very_low` | `[0.00, 0.10)` |
| `low` | `[0.10, 0.30)` |
| `moderate` | `[0.30, 0.60)` |
| `high` | `[0.60, 0.85)` |
| `very_high` | `[0.85, 1.00]` |

Ranges are half-open except the top one, which closes at 1.0. Calibration is measured per bucket on
every evaluation run — observed frequency of "this hypothesis was the validated culprit" plus a
Brier or log-loss score, paired against the previous engine version on the same fixtures (SC-004).
A bucket holding fewer than 20 hypotheses is reported as under-powered rather than scored.

### Widening on a cut-short test (FR-045)

When a budget runs out mid-test, freezing the last computed number publishes a precision the run
did not earn. So the **reported bucket moves one published step outward**, toward the mass the
untested evidence could have moved, and the widening is recorded with its reason
(`widened`, `widened_reason`).

What widens is the reported bucket, never the computed confidence: the number must stay exactly
what the rule produced, or recomputation from the rows would no longer reproduce it.

- `down` — the cut-short query was expected to bear against the hypothesis, **and whenever nothing
  is known about which way it would have gone**. Unearned confidence is the failure mode the
  constitution guards against.
- `up` — the cut-short query was the one expected to confirm it.

One step, never more, and clamped at the ends of the scale: two steps from `very_high` would land in
`moderate` and claim something no missing query justifies.

## The verdict rule — prior-only mass is not confidence

A ranker that likes one candidate and a run that tested nothing produce a posterior near 1 and know
nothing. The rule that keeps the two apart is published, stated once in code
(`internal/investigation/ledger/status.go`, `widen.go`) and carried in the rendering itself so the
model reasoning over the table reads it beside the numbers:

> verdict rule: prior-only mass is not confidence — a hypothesis is named only with a supporting
> judgment from telemetry evidence or a human fact, and one without is reported no higher than
> `moderate` whatever its posterior.

**What counts as evidence for naming.** `Ledger.EvidencedSupport` reads the judgments in recorded
order — so the answer is stable across replays — and returns the first `supports` judgment whose
source is a human fact, or whose evidence item is *telemetry evidence*: kind `algebra_answer` or
`onset_estimate`, **with a non-empty `source_of_truth`**. `TelemetryEvidence` is a function of the
stored row rather than of a worker registry, because a ledger read back in 2029 must still be able
to say what its confidences rested on, and the registry will have moved on.

What does **not** count, and why:

| not evidence | why |
|---|---|
| the prior | a reason to look, not a reason to believe |
| `graph_answer` | it says a change happened near the subject — which is what put the hypothesis on the list, and cannot also be the evidence that it is the cause |
| `knowledge_item` | a document says what happened last time (FR-051) |
| `resolution_audit`, `injection_attempt` | statements about the graph's structure or about an attack, not measurements of this incident |

**The unevidenced ceiling.** A hypothesis nothing observed supports is *reported* in
`UnevidencedCeiling` = **`moderate`** at most, however high its computed confidence. The computed
number is left exactly where the posterior rule put it — recomputation from the rows must still
reproduce it — and both numbers stay visible in the rendering. The ceiling composes with the
cut-short widening above: widening moves the reported bucket one step outward, and the ceiling then
clamps it.

**The open hypothesis is evidenced differently.** Nothing supports `no_observed_change` directly,
so its evidence is the *refutations of its rivals*: a run that tested the candidates and ruled them
out has measured something; a run that tested nothing has not. It may reach `supported` only while
no rival is `supported`, which is what stops it being asserted over a candidate the evidence backs.
A rival carrying one supporting judgment and two refuting ones does not block it — that hypothesis
is refuted, the run looked and ruled it out.

**The three verdicts** the engine derives from this rule (`internal/investigation/engine/verdict.go`)
— never stated by a model:

| verdict | when |
|---|---|
| `named` | the hypothesis carries an evidenced supporting judgment and is therefore `supported` |
| `unknown` | nothing qualifies yet and the run can still test: the honest interim answer |
| `unobserved` | the run *finished* and nothing qualified — every candidate untested, refuted, exonerated or tested-and-not-separated |

`unobserved` is a finding, not a shrug: it is the answer on every incident whose cause no feeder in
this deployment can see (FR-071b, SC-023), and it is what the culprit-deleted metamorphic variant
asks for. It is reached **only** from a stop that means the run finished. A run cut short by a
budget, a dead worker or a refusal has not established that nothing explains the symptom and says
`unknown` instead. Either way the symptom stays localised on the subject: "we do not know what
changed" and "we do not know what is broken" are different things, and only the first is true here.

## The status machine

```text
proposed ──► supported     ≥ 1 supports judgment from a source-of-truth worker
         ├─► refuted       the net evidence bears against it
         ├─► inconclusive  tested, nothing separated it
         ├─► untested      no pointer of the needed kind, or no data for the window
         └─► exonerated    starts after onset by more than the onset's uncertainty
```

A status never returns to `proposed`: what was learned stays learned. Every other transition is
permitted, because the ledger is a live belief state and evidence arriving late is the normal case.

## Write-time rejections

Published reason codes, matching `internal/log`'s discipline for events. A caller matches on the
string, so these are schema.

| reason | when |
|---|---|
| `model_confidence` | a confidence, prior, bucket or rank arriving from outside the ledger rule, or a judgment carrying a likelihood ratio that is not the published constant for its (direction, strength) |
| `unsupported_status` | `supported` with no `supports` judgment from a source-of-truth worker; a knowledge-only hypothesis that no source-of-truth worker has confirmed (FR-051); a second `no_observed_change` hypothesis |
| `duplicate_judgment` | a second judgment for the same `(hypothesis, evidence)` pair |
| `missing_coverage` | an evidence item with no coverage block (FR-014a) |

Two further rules are errors rather than reason codes, because they are programming mistakes the
schema also refuses: `untested` without **both** a reason and the exact next query (FR-031), and
`exonerated` without the onset estimate's evidence id (Invariant 9).

## Conflict and non-separability

- **Conflict** (FR-024): a hypothesis with evidence on both sides is reported with **both** lists
  and both sides are in the number — the posterior sums every ln LR, not the ones that agree.
  Choosing a side without stating why is impossible by construction, because there is no field that
  holds one side.
- **Non-separability** (FR-025): candidates whose confidences differ by no more than the published
  **separation epsilon, 0.05**, are reported together, said to be non-separable, and accompanied by
  what would separate them — the recorded next query on each. Where nothing recorded would separate
  them, the group says so; that is a finding, not an omission. Membership is "within epsilon of the
  group's leader", so a chain of near-neighbours is not collapsed into one group spanning half the
  mass. Refuted and exonerated hypotheses take no part. The open hypothesis does: being unable to
  separate a candidate from *no observed change explains this* is exactly the finding `unknown`
  exists to report.

## The open hypothesis

Every investigation carries **exactly one** hypothesis of kind `no_observed_change`, always
present, always ranked, scored and rendered like any other (FR-019a). Its field name is spelled
`no_observed_change` everywhere; `H₀` is prose shorthand used in `research.md` and nowhere in the
schema.

That is what makes `unknown` a computed outcome of an open hypothesis space rather than a behaviour
of the reasoning layer: when the evidence does not move mass onto a candidate, it stays where π₀
put it.

Enforcement is in three places, because "always present" is not something a caller can be trusted
to remember on the one incident where it matters: the ledger creates it at construction; the schema
holds a partial unique index for it; and the deferred sum trigger fails loudly for any set that
omits it and does not renormalise.

## Three renderings of one structure

| rendering | what it is for |
|---|---|
| `investigationv1.Ledger` (proto) | the wire form: the RPC and the export command |
| canonical JSON (001's serializer) | recording and replay; `sha256` of it is the ledger digest a trajectory's `ledger_update` record carries |
| the compact table (`Render`) | the turn-scoped text re-injected into the model's context each turn |

The compact table carries, per hypothesis: id, kind, statement (truncated to a published width),
status, prior, confidence with its bucket, and the counts of supporting, refuting and neutral
judgments; then the untested hypotheses with their reason and exact next query, the exonerations
with their onset evidence, the conflicts with both sides, the non-separable groups with what would
separate them, and the budget remaining and stop conditions in force.

Evidence ids are sorted in the export rather than kept in arrival order, so two runs that gathered
the same evidence in different orders export the same bytes; the `judgments` list keeps recording
order, because that one really is a sequence.

## What reaches the graph

Exactly one `record_investigation` event per investigation, emitted once at conclusion, in one
transaction, with the **investigation id as its idempotency key**. Judgments, worker calls and
ledger updates are never events (constitution III).

It carries identifiers, statuses, confidences with their buckets and ranges, ranks, rationales,
spend, model configuration and the recording's key and digest — under the allow-listed
`sre.investigation.*` namespace, with the telemetry denylist applying inside it. A decision record
carrying a series is rejected with `telemetry_payload` (SC-010).

Because a graph property is capped at 4 KiB, the hypothesis summary descends a published detail
ladder until it fits — `full` → `no_rationale` → `identifiers`, then truncation by rank — and the
record states which level it used, how many hypotheses it carries and how many there were. The full
ledger is in the recording beside the graph, which is what `recording_key` and `recording_digest`
point at.
