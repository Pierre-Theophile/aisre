# Phase 0 Research: Investigation Engine

**Feature**: 002-investigation-engine | **Date**: 2026-09-17

Every unknown in the plan's Technical Context is resolved below as a decision, its rationale,
and the alternatives that were rejected. Model ids, prices and API behaviour come from the
`claude-api` skill as loaded on 2026-09-17; nothing about the model is asserted from memory.

---

## §1 — One binary or two

**Decision**: the existing `sre-agent` binary gains `investigate`, `worker`, `audit` and
`incident` subcommands, `fixture` gains the investigation flags, and `serve` gains
`InvestigationService` behind `--enable-investigation`. No second binary.

**Rationale**: constitution X asks for a single static binary with minimal dependencies, and the
usual reason to split — a heavyweight runtime for the model client — does not apply here. The
Anthropic Go SDK is a pure-Go HTTP client over `net/http` and `encoding/json`: no cgo, no
service, no vendored model. A second binary would duplicate OIDC authentication, the OTel setup,
the Postgres store, the migration runner, the canonical serializer and the whole cobra tree,
and would give an operator two things to deploy, version and roll back for one product. The
engine is inert without model configuration, so the operator who only runs the graph pays
nothing but binary size.

**Alternatives considered**:
- *A second `sre-investigator` binary.* Rejected: the duplication above, and the fact that the
  engine must read the graph through the same client code and write the decision record through
  the same ingestion path. The seam would be a network hop between two halves of one product.
- *A plugin architecture (workers as external processes).* Rejected as speculative (YAGNI):
  no worker in this feature needs isolation beyond the process, and the recording boundary
  already gives us the substitutability a plugin boundary would.
- *Keeping the engine out of `serve` entirely (CLI-only).* Rejected: FR-046 requires an
  in-flight investigation to be observable and FR-057a requires a human fact to reach a running
  investigation, both of which need a server.

---

## §2 — The model, and what an investigation costs

**Decision**:

| role | model id | effort | why |
|---|---|---|---|
| investigator | `claude-fable-5-1` | `xhigh` under `review`, `high` under `page` | the owner's stated bar; 1M context; thinking always on; $10/MTok in, $50/MTok out, cache reads $0.25/MTok |
| verifier (FR-022a) | `claude-opus-5` | `high` | fresh context, mechanical claim-to-citation checking; $5/MTok in, $25/MTok out |
| logs labelling (FR-009a, optional) | `claude-haiku-4-5` | n/a | templates and counts only, never a raw line; $1/MTok in, $5/MTok out |

The model id, the effort level, the thinking display, the beta set and the price-table version
are recorded on every investigation and are what FR-061 means by "the production configuration".

**Rationale**: the investigator is the one component whose failure mode is fluent guessing, and
it is called at most a few dozen times per incident, so capability is worth more than unit
price — the reviewer's own framing is that the binding constraint during an incident is the
vendor's telemetry quota, which humans are spending at the same time, not the model bill. The
verifier is a different job: read a claim, read the digest it cites, say whether the numbers
match. Using a *different* model there is deliberate — a verifier that shares the investigator's
weights shares its blind spots, and this pass exists precisely to catch a claim the investigator
believed. Haiku 4.5 in the logs worker is bounded by construction: it sees mined templates and
counts, never a line, and its only job is to say what it added over the algorithm (FR-009a).

**Cost model, per investigation** (order of magnitude, from the published rates):

| item | tokens | rate | cost |
|---|---|---|---|
| cached prefix + history reads, ~15 turns | ~390k cache-read | $0.25/MTok | ~$0.10 |
| new input (digests, ledger renders) | ~40k | $10/MTok | ~$0.40 |
| output (thinking + tool calls + synthesis) | ~15k | $50/MTok | ~$0.75 |
| cache writes | ~50k | above base input (multiplier from the versioned price table) | ~$0.6 |
| verifier pass on `claude-opus-5` | ~20k in / 2k out | $5 / $25 per MTok | ~$0.15 |
| logs labelling on `claude-haiku-4-5` | ~5k in / 1k out | $1 / $5 per MTok | ~$0.01 |

**≈ $2 per `page` investigation; single dollars, not tens.** `review` runs longer and costs
roughly $3–10. A nightly evaluation of 8 fixtures × 3 runs is therefore ~$50, or ~$25 through
the Batch API, which is available for the `review` profile because nightly evaluation is not
latency-sensitive. These figures are what the budget manager's `cost` dimension is calibrated
against; they are recomputed from the checked-in price table, never hard-coded in prose.

**Alternatives considered**:
- *`claude-opus-5` as the investigator.* Rejected for v1 because the owner named the bar, and
  because the whole thesis under test is whether a strong reasoner adds *lift* over the
  deterministic prior; running the weaker model first would confound "the reasoning layer does
  not help" with "this reasoning layer does not help". Opus 5 remains the obvious fallback if
  the cost model changes, and the price table makes the swap a configuration change.
- *`claude-sonnet-5` for the workers.* Rejected as unnecessary: the only worker with a model is
  the logs labeller, whose input is already reduced to templates. Sonnet stays the documented
  escalation if labelling quality is measured as insufficient.
- *Fine-tuning or a local model.* Out of scope and against principle X (a second runtime,
  a training pipeline, and an unpublishable artifact).

---

## §3 — Who owns the loop, and what shape the conversation takes

**Decision**: a hand-rolled agent loop in `internal/investigation/engine`, over
`client.Messages.New` / `client.Messages.Stream` from `github.com/anthropics/anthropic-sdk-go`.
Neither the Claude Agent SDK nor the SDK's `BetaToolRunner`.

**Rationale**: three requirements of this feature are requirements *on the loop itself*.

1. **Byte-exact recording of the model boundary** (FR-042a). Layer 1 records the exact request
   body and the exact response for every turn. A helper that composes the request internally
   makes "the exact request" an implementation detail of a beta helper.
2. **A deterministic re-issue path** (FR-041). In trajectory replay the engine must *not call
   the API at all*: it matches the request it was about to send against the next recorded
   request by canonical digest and returns the recorded response. That is a substitution at the
   transport layer of the loop, and the loop has to know it is replaying.
3. **Budget admission, anytime publication and typed stops** (FR-045, FR-045a, FR-046a). Every
   turn is preceded by a `count_tokens` admission check against the remaining budget, and
   followed by a diminishing-returns evaluation over the ledger. A `RunToCompletion` helper owns
   exactly the point at which those decisions have to be made.

The Claude Agent SDK is additionally disqualified twice over: it has no Go binding, and it is
the Claude Code harness with built-in filesystem, bash and web tools — a tool surface that
would violate principle VII the moment it existed in the process. The Tool Runner is a good
helper for an application that wants the loop written for it; this feature *is* the loop.

**Conversation shape**, and the API constraints that dictate it:

- **Append-only history.** On this model family, editing an earlier turn invalidates the
  thinking blocks that follow it (preserved thinking). The ledger is therefore never patched
  into an earlier message: each turn appends a *mid-conversation system message* carrying the
  re-rendered ledger with `clear_at: "next_user_message"` (beta
  `mid-conversation-system-clear-at-2026-08-21`), which renders for one turn and then stays in
  the transcript cleared. Earlier copies are never deleted.
- **The cached prefix is `tools` → `system` → algebra reference.** Roughly 10k tokens, stable
  for the life of a schema version, with the cache breakpoint after it. Everything volatile —
  the alert, the instants, the ledger, the digests — comes after. `usage.cache_read_input_tokens`
  is asserted to be non-zero from turn two in a conformance test; a zero would mean a silent
  invalidator has crept into the prefix.
- **The operator channel is the system channel.** Worker output arrives only as `tool_result`
  blocks. Nothing a worker returns can reach the channel that carries instructions. This is what
  makes the worker boundary the injection barrier in the API's own terms, not merely in ours.
- **Forced tool use does not exist on this model** (`tool_choice: {type: "tool"|"any"}` returns
  400). The ledger therefore cannot be updated by *compelling* a tool call. Three consequences,
  all designed in: the engine — not the model — is the ledger's writer of record; the
  deterministic first wave produces judgments with no model in the loop at all; and a turn that
  proposes no judgment is recorded as such and answered with a turn-scoped system reminder
  rather than a retry that would rewrite history. `strict: true` on every tool keeps the
  arguments schema-valid without forcing the call.
- **Structured outputs** (`output_config.format`) are used for the two places a free-form answer
  would be a defect: the judgment batch a turn proposes, and the closing synthesis's machine
  form. The human rendering is derived from the structured form, never written separately
  (US1-4).
- **Streaming** for the synthesis turn (large `max_tokens`), with `eager_input_streaming: true`
  on the client tools and validation of every parsed tool input before it is executed — a
  truncated tool input is treated exactly like invalid JSON.
- **No temperature.** Sampling parameters return 400 on this model family. FR-061's "evaluate at
  the production temperature" is satisfied by pinning and recording the model id, `effort`,
  thinking display and beta set; run-to-run variation is inherent and is exactly what pass@1 and
  pass^k measure. This is a wording correction to the spec's assumption, noted in the final
  report.

**Alternatives considered**:
- *`BetaToolRunner` with per-turn hooks.* Rejected: the hooks give interception, not ownership;
  replay needs the loop to not call the API at all, and the budget needs admission *before* the
  request is composed.
- *Re-rendering the ledger by rewriting the first user message.* Rejected: it invalidates the
  cache prefix on every turn and trips the history-editing check.
- *Putting the ledger in a tool result instead of a system message.* Rejected: a `tool_result`
  is data from a worker, and the ledger is the engine's own authoritative state; conflating them
  would make the injection barrier's rule ("tool results are never instructions") ambiguous.

---

## §4 — Symptom onset: where it runs, and by what method

**Decision**: onset estimation is a **published algebra term evaluated backend-side** —
`onset(pointer, search_window, method)` — returning an instant, an uncertainty, the method and
its parameters as a digest. The method is a **seasonal-naive baseline plus a two-sided CUSUM,
with the change point refined by binary segmentation** over the residual series.

**Ownership, so that FR-029a and principle IV are both satisfied**: the *algorithm* is this
feature's code, shipped in `pkg/backend/onset` as the reference implementation of the algebra
term, and every backend calls it rather than inventing its own. Only its *execution* happens on
the vendor side of the digest boundary. FR-029a's "change-point detection belongs to this
feature's metrics worker" therefore holds — it is 002's algorithm, versioned with 002's
algebra — while no sample ever reaches the investigator.

**Rationale**: the placement question is the important one. Change-point detection needs the
series; constitution IV and FR-014 forbid the series from crossing the worker boundary to the
investigator. If the investigator estimated onset itself, the raw samples would have to be in
its context — the precise thing the whole digest contract exists to prevent. So detection runs
on the backend side of the digest boundary, and what crosses is the *estimate*, which is
evidence like any other (FR-029a requires exactly that: method, metric and selector, window,
instant, uncertainty). This also means the recorded world holds onset answers, so a replayed
investigation re-derives the same onset without the series existing anywhere in the fixture.

On the method: the requirement is a *first sustained shift* relative to normal behaviour, not an
optimal segmentation of the whole series, and the output must be byte-reproducible across amd64
and arm64 because goldens are compared byte for byte. A seasonal-naive baseline (the median of
the same clock offset over the previous *k* periods, day and week) needs no model fitting and
degrades honestly when history is short. A two-sided CUSUM over the standardised residual is
O(n), has one published threshold *h* and one published drift allowance *k*, and its run length
at the crossing gives a natural uncertainty interval. Binary segmentation on the residual within
the CUSUM's alarm window sharpens the instant. All outputs are rounded to six decimals and
whole seconds, the same discipline the ranking formula already adopted for the same reason.

Fallbacks are specified, not improvised: too little history → flat baseline, uncertainty widened
and flagged; series flat, absent or too sparse, or no crossing → `onset_unavailable`, the engine
falls back to the alert instant, says so in the output, and **exonerates nobody on timing alone**
(FR-029a, edge case "the symptom onset cannot be estimated").

**Alternatives considered**:
- *PELT.* Rejected: it is the right tool for segmenting a whole series optimally, and it brings
  a cost model and a penalty to tune. We want the first sustained departure from a baseline, and
  a penalty parameter is a knob that will be tuned on eight fixtures and overfit.
- *Bayesian online change-point detection.* Rejected: priors, hyper-parameters and a
  probabilistic output that is hard to make byte-identical across architectures. Its natural
  advantage — an online run-length posterior — buys nothing for a one-shot batch question.
- *Estimating onset from the alert's own monitor transition history.* Rejected as the primary
  method (the monitor is exactly the thing that lags), but kept as a corroborating evidence item
  where a monitor-state digest is available.
- *A third-party change-point library.* Rejected under principle X and the determinism argument
  in Complexity Tracking.

---

## §5 — The logs worker: template mining, then optionally a model

**Decision**: a Drain-style fixed-depth parse tree implemented in-repo produces masked templates
with counts, first-seen instants, and per-template join keys. The optional
`claude-haiku-4-5` pass runs *after* it, sees only templates and counts, and its contribution is
stated in the digest (`model_added`). Raw lines never reach it; bounded sanitised exemplars are
returned only on explicit request (FR-014, FR-044a) and are capped, redacted and recorded.

**Rationale**: FR-009a mandates the algorithmic reduction first, and Drain is the published
standard for it: fixed-depth tree, first-token and length bucketing, similarity threshold,
linear in the number of lines, and — the property that matters here — deterministic given an
input order, which the recorded world fixes. Implementing it in-repo (≈300 lines) keeps the
masking rules, the variable placeholders and the redaction under our control, which matters
because this is the sanitisation point: a template is what makes a log line publishable at all
(ADR-0003 D9).

**Alternatives considered**:
- *A log-parsing dependency.* Rejected: masking and redaction policy have to be ours, and a
  library version bump that changes template identity would silently invalidate every fixture.
- *Model-first summarisation.* Rejected by FR-009a and by cost: a million lines summarised by a
  model is exactly the "ocean of text" the feature exists to avoid.
- *No model at all in the logs worker.* Considered seriously, and it remains the default: the
  labelling pass is off unless configured, because the algorithm already produces the evidence,
  and the model's addition must be measurable before it is paid for.

---

## §6 — The traces worker

**Decision**: algorithmic only, no model. Two capabilities: `error_spans(edge, window)` —
counts and latency summary statistics grouped by operation and error kind over the spans that
traverse a named graph edge, with trace identifiers preserved as join keys and drill-down
handles — and a `compare` specialisation over the same grouping for baseline versus symptom.

**Rationale**: what an investigator needs from traces is "which call on this path started
failing, and by how much", which is arithmetic over groupings the backend can already do.
Anything a model would add would be narrative, and narrative that is not derived from the
grouping is not evidence.

**Alternatives considered**: a model-assisted trace-anomaly narrative — rejected under FR-009a
and because it would duplicate the investigator's job inside a worker, which FR-013a forbids
(one belief state).

---

## §7 — Knowledge retrieval: does v1 need embeddings?

**Decision**: no. v1 retrieval is **graph-scoped then lexically ranked**: the knowledge worker
asks the graph for documents linked (edge `CONCERNS`) to any entity in the investigation's
subgraph or to any candidate change, then ranks that scoped set with BM25 implemented in-repo
over title, headings and body, boosted by link proximity (hops from the focus) and recency.
Embeddings are deferred with a published trigger.

**Rationale**: the constitution already says the graph is the RAG index, and that is not a
slogan — it is what reduces the candidate set from "every document the organisation has" to
"documents attached to these twelve entities", which is tens of documents. Ranking tens of
documents does not need a vector index; it needs a defensible ordering and a citation. BM25 is
deterministic, needs no model, no index to rebuild, and no second store. Adding an embedding
index would add a dependency, an index-freshness problem, a re-embedding migration on every
document change, and a non-deterministic nearest-neighbour ordering that fixtures would have to
pin.

**Published trigger for revisiting** (so this is a measurement, not a preference): when the
graph-scoped candidate set routinely exceeds 200 documents, or recall@10 on the knowledge
fixtures falls below 0.9, embeddings earn their place and get their own ADR.

**Alternatives considered**:
- *Embeddings over the whole corpus, graph links as a re-ranker.* Rejected: it inverts the
  constitution's rule (retrieval scoped by the graph) and would retrieve documents linked to
  nothing, which FR-049 forbids.
- *A search dependency (Bleve, Tantivy bindings).* Rejected under principle X: an index for a
  set this small is machinery without a measurement behind it.
- *Model-based re-ranking of the scoped set.* Deferred; it is a one-line escalation once
  `recall@10` is measured, and measuring it first is the point.

---

## §8 — Confidence: the posterior rule, the judgment scale, the buckets

**Decision**: confidence is a **normalised naive-Bayes posterior over a closed hypothesis set
that always contains the open hypothesis**, computed in log space from published constants. The
model never emits a probability.

- **The set**: the ranked candidate changes, any named-condition hypotheses, and the mandatory
  *no observed change explains this* hypothesis, whose **field name is `no_observed_change`** —
  `H₀` is prose shorthand for it in this document and nowhere in the schema (FR-019a).
- **Priors**: a candidate's prior is its published ranker score normalised over the candidate
  set, scaled by `1 − π₀`. The prior of `no_observed_change` is **π₀, taken from the coverage
  audit's measured ceiling**. The first audit has been run and published
  (`docs/evaluation/coverage-audit-2026-09.md`, ADR-0004, FR-069a): 13 classifiable production
  incidents, the cause present in the graph at alert time in 0 of 13 with the 001 feeders alone,
  3 of 13 with deploy feeders, 8 of 13 with vendor-notice feeders — **a ceiling of about 62%, so
  `π₀ = 0.38`**. This is the single most defensible number in the design: the open hypothesis's
  prior is exactly the measured probability that the cause is invisible. It is also why the audit
  is re-run after each connector ships (FR-071a) — `π₀` is a property of the deployment's feeder
  set, is recorded on every investigation alongside the audit it came from, and moves as feeders
  land.
- **Judgments**: each judgment names one hypothesis and one evidence item, and carries a
  direction and a strength from a five-point published scale. The log-likelihood ratios are
  constants:

  | strength | LR (supports) | ln LR | LR (refutes) |
  |---|---|---|---|
  | `WEAK` | 1.5 | +0.405 | 1/1.5 |
  | `MODERATE` | 3 | +1.099 | 1/3 |
  | `STRONG` | 10 | +2.303 | 1/10 |
  | `DECISIVE` | 50 | +3.912 | 1/50 |
  | `NEUTRAL` | 1 | 0 | 1 |

- **The rule**: `posterior_i ∝ prior_i · Π_j LR_ij`, computed as
  `ln prior_i + Σ_j ln LR_ij` and normalised over the set with a numerically stable
  log-sum-exp. Rounded to six decimals.
- **Order independence**: addition commutes, so the posterior cannot depend on the order in
  which judgments arrived. This is asserted as a property test (`rapid`), because it is the
  formal statement of "two runs that gather the same evidence agree".
- **Caps**: at most one judgment per (evidence item, hypothesis) pair — a repeated call produces
  a second *evidence item*, which is correct (FR, "duplicate worker calls"), but the same
  evidence may not be counted twice against one hypothesis. A human fact enters at `STRONG`, never
  `DECISIVE`: strong evidence, not truth (FR-057a).
- **Buckets** (published, coarse until the corpus justifies finer, FR-023):
  `very_low [0, 0.10)`, `low [0.10, 0.30)`, `moderate [0.30, 0.60)`, `high [0.60, 0.85)`,
  `very_high [0.85, 1.00]`. Each carries its range in the output.
- **Widening on a cut-short test** (FR-045): the reported bucket moves outward one step, toward
  the mass the untested evidence could have moved, and the widening is recorded with its reason.
- **Calibration**: per-bucket observed frequency of "this hypothesis was the validated culprit",
  plus a Brier score, paired against the previous engine version on the same fixtures (FR-023,
  SC-004). Buckets holding fewer than 20 hypotheses are reported as under-powered rather than
  scored.

**Rationale**: the reviewer's instruction was to stop the model verbalising confidence and
compute the posterior from priors and likelihood ratios. Naive Bayes over a closed set that
includes the open hypothesis does exactly that, is a page of code, is explainable in the output
("prior 0.31, three supports at moderate, one refute at strong → 0.58"), and is auditable line
by line. The independence assumption between judgments is wrong in the usual way — two digests
from the same window are correlated — and the mitigation is the one-judgment-per-evidence-item
cap plus the coarse buckets, which is why the buckets stay coarse until calibration says
otherwise.

**Alternatives considered**:
- *Letting the model state a confidence.* Rejected by FR-023 and by the reviewer: a verbalised
  probability is not calibratable and not reproducible.
- *A learned scorer over judgment features.* Rejected: eight fixtures cannot train anything, and
  a learned confidence would be unauditable on day one.
- *Dempster–Shafer / subjective logic.* Rejected as cleverness (principle X): it buys an
  explicit ignorance mass, which `no_observed_change` already gives us in a form an SRE can read.
- *Independent per-hypothesis odds without normalisation.* Rejected: the output must rank and
  must be readable as "how likely is each of these", and an unnormalised score invites exactly
  the misreading the buckets exist to prevent.

---

## §9 — The recording: two layers, two key disciplines

**Decision**:

- **Layer 1 (trajectory)** is `trajectories/<run-id>.jsonl`, sequence-ordered, canonical JSON,
  one record per line, typed `model_request | model_response | worker_request | worker_response
  | ledger_update | human_fact | stop`. Model records carry the full request body and the full
  response including `usage` and `stop_reason`. Replay matches the request the engine is about
  to issue against the next record by canonical digest, returns the recorded response, and makes
  no network call. A mismatch fails loudly with the first diverging record named.
- **Layer 2 (world)** is `world/<sha256(canonical(term))>.json`, a map from a fully typed algebra
  term to a digest. The recorded set is the cross product of the **telemetry** family of the
  algebra over the alert neighbourhood (every pointer on every node within the fixture's declared
  hop radius; every edge on the path set) × the window grid the manifest declares, **plus every
  drill-down handle returned at depth 1** by those answers. Depth 2 and beyond return
  `not_recorded` by design; the recording depth is a manifest field, so a fixture states its own
  limit rather than hiding it.
- **The graph family is not recorded at all.** Graph terms are answered deterministically by
  replaying the fixture's `events.jsonl` into an empty database — which 001 already guarantees
  is byte-reproducible — so recording them would duplicate the event log and create a second
  source of truth for the same answer. This is exactly what the spec means by "the graph workers
  answer deterministically from the replayed event log in both layers".
- **Canonicalisation** reuses 001's serializer without modification: sorted keys, RFC 3339 UTC,
  six-decimal floats, stable array ordering, `emitDefaults=false`. Instants in a term key are
  normalised to whole seconds; window widths to whole seconds.
- **Bounds**: per-response and per-investigation size caps are published; truncation is recorded
  *in the response it applies to*, so a replay reproduces the truncation the investigator
  actually saw (FR-037).

**Rationale**: the two layers answer two different questions — "did the plumbing change?" and
"would a different reasoning path still be served?" — and they need different keys. A trajectory
is inherently ordered (it is a run); a world is inherently keyed (it is a function). Trying to
serve both from one structure would either force the world to be ordered, which is meaningless,
or force the trajectory to be keyed, which loses the sequence the divergence report depends on.

**Alternatives considered**:
- *One keyed recording for both layers.* Rejected: FR-041 requires naming the *first* diverging
  request, which needs the sequence.
- *Recording the model's outputs only as a hash.* Rejected: layer 1's whole claim is
  byte-identical replay, which requires the bytes.
- *Recording graph answers as well, for speed.* Rejected: a second source of truth for a
  deterministic answer is a source of drift, and replay of 001 fixtures is already fast.
- *Falling back to a live backend on a miss.* Prohibited by FR-040; a replay that improvises is
  evidence of nothing.

---

## §10 — The query algebra: which terms, and why that set

**Decision**: one versioned algebra, three families, published in
`contracts/investigation.proto` and rendered in `docs/schema/algebra.md`. Nothing outside it may
be asked of a worker (FR-042b); a call outside it is refused with a typed reason and recorded.

| family | terms | recorded into a world? |
|---|---|---|
| **graph** | `subgraph`, `diff`, `impact`, `pointers`, `node_history`, `resolution_audit`, `extent` — typed wrappers over 001's published RPCs, both instants passed through | **no** — answered from the replayed event log, so they are never cross-producted into a world (FR-042b) |
| **telemetry** | `compare(pointer, window_a, window_b, statistic)`, `onset(pointer, search_window, method)`, `new_log_patterns(pointer, window, baseline_window)`, `error_spans(edge, window)`, `errors_by_version(pointer, window)`, `monitor_state(pointer, window)`, `exemplars(handle, limit)`, `drill_down(handle)` | yes — the cross product |
| **knowledge** | `knowledge_search(entity_ids, query_terms, limit)`, scoped by the graph | yes — small, keyed like telemetry |

The spec names four telemetry terms as the minimum (`compare`, `new_log_patterns`,
`error_spans`, `errors_by_version`), and 003 and 005 originally restated the same four. After the
analyze pass (X1) they instead reference `contracts/telemetry-backend.md` as the single published
home and are required to serve **all eight**. This plan adds the other four:

- **`onset`**, because otherwise the samples must cross the digest boundary (§4);
- **`monitor_state`**, because 005's monitor-state digest already exists, alert recovery is
  evidence (ADR-0003 D10) and the intake needs the transition history;
- **`exemplars`**, because FR-014 requires bounded sanitised exemplars *on explicit request*, and
  "on explicit request" has to be a term or it is a side channel;
- **`drill_down`**, because FR-014b requires drill-down handles, and a handle that cannot be
  presented back is decoration.

**Rationale**: the algebra is simultaneously the replay boundary, the vendor abstraction, the
sanitisation point and the injection barrier (ADR-0003 D8). Every term must therefore be (a)
finite in its argument space so a world is finite, (b) executable by any backend, (c) answerable
as a bounded digest, and (d) free of caller-supplied query text. `exemplars` and `drill_down`
satisfy (a) only because their arguments are *handles minted by a previous answer*, which is why
the recorded world includes depth-1 drill-downs and nothing deeper.

**Alternatives considered**:
- *Letting the investigator pass a raw selector.* Prohibited by 005 FR-040a and fatal to the
  world recording (an infinite argument space) and to the injection barrier.
- *A smaller algebra (the four terms only).* Rejected: onset would have to be computed
  investigator-side (principle IV violation) and exemplars would become an undocumented escape
  hatch.
- *A larger algebra with per-vendor terms.* Rejected: a term only one backend can answer breaks
  the vendor abstraction and makes fixtures vendor-specific.

---

## §11 — Budgets: profiles, cost classes, quota share, and the control law

**Decision**:

- **Profiles** `page` and `review`, published with their targets and hard stops (FR-047), stored
  in configuration and recorded per investigation.
- **Dimensions**: wall time; cost units; per-backend call budget; per-cost-class call budget;
  maximum query window width; share of remaining vendor quota; concurrent investigations; daily
  global spend.
- **Cost classes** are declared by the backend, not guessed by the engine: each capability
  declares a class (`cheap`, `standard`, `expensive`) and the backend's `Describe` publishes the
  mapping. This keeps the engine vendor-neutral while letting an operator say "at most 10
  expensive calls".
- **Quota share**: the backend returns `remaining_quota` and `quota_window` in every digest's
  coverage block where the vendor reports it; the engine spends at most its configured share of
  what remains, records both the observed remaining quota and the share it consumed per call,
  and falls back to the absolute per-backend budget (recording that it did) where the vendor
  reports nothing.
- **Admission before issue**: `count_tokens` for a model turn, the declared class and window
  width for a worker call. A call that would breach a budget is never issued; the intent is
  recorded as an untested-hypothesis next query with its deep link.
- **Reserve**: 15% of every dimension, entered as `synthesis_only` mode.
- **Diminishing returns**: stop when `‖p_t − p_{t−n}‖₁ < ε` over the ledger's posterior vector
  across the last `n` worker calls, with published `n = 5`, `ε = 0.02` as the initial values to
  be calibrated with the corpus in hand.
- **Cost unit** (FR-048): the primary, provider-independent unit is **tokens per model id per
  class** (input, cache-write, cache-read, output) plus **worker calls per backend per cost
  class**. A monetary figure is derived from a checked-in, versioned price table, and the table
  version is stored on the investigation so historical costs remain interpretable after a price
  change.

**Rationale**: the reviewer's point that the binding constraint is the vendor quota, not the
model bill, is what makes the share-of-remaining-quota dimension primary rather than decorative;
an agent that spends the on-call's quota during the incident is worse than an agent that stops.
Expressing cost in tokens first is what makes "cost per investigation" comparable across a year
in which prices change twice.

**Alternatives considered**:
- *A flat call count.* Rejected by FR-043 and by the reviewer: it prices a cheap monitor read
  the same as an expensive span search.
- *Monetary cost as the primary unit.* Rejected: it is a moving target and not comparable over
  time; it is kept as a derived, versioned figure because operators budget in money.
- *A PID-style controller over spend.* Rejected as cleverness: the anytime shape plus a reserve
  plus a diminishing-returns test is the whole control law, and it is explainable in a sentence.

---

## §12 — Evaluation: metrics, the gate, the corpus, and where it runs

**Decision**: `aisre fixture verify --report` gains investigation rows; there is no second
harness (FR-063). Trajectory replay gates every PR in `ci.yml`; a new `eval.yml` runs the corpus
in world-replay mode.

- **Gated**: aggregate pass@1 over fixtures × runs; lift over the prior > 0; harm rate ≤ its
  published threshold; citation validity = 100%; zero untraceable conclusions; zero improvised
  replays; zero metamorphic verdict changes; zero regressions on a human-labelled case.
- **Reported, trended, not gated**: top-k hit rate, the culprit's rank, calibration, cost,
  timing, `unknown` rate and precision, onset error, quota share.
- **Never gated**: best-of-k.
- **`not_recorded` miss rate** gates a *fixture's admission to the corpus* (≤ 5%), never the
  engine's score.
- **Runs**: 3 per fixture, extended to 7 when the first 3 disagree. The published gate states
  its detection power in the same sentence as its threshold: 8 fixtures × 5 runs = 40 Bernoulli
  trials detects 90% → 70% reliably and cannot reliably detect 90% → 80%.
- **Corpus multiplication** is a command, not a directory of hand-edits:
  `aisre fixture derive <fixture> --transform <t>` produces `culprit-deleted`,
  `decoy-injected`, `time-shifted`, `name-permuted` variants, recording the transformation and
  the parent in the derived manifest's provenance (FR-062a). The invariant each variant is
  graded on is part of the transform's definition, so a variant cannot exist without its
  assertion.
- **Adversarial fixtures** are authored, not derived: slow-burn (~3 h), distant culprit (~3
  hops), adjacent decoy (~2 min). Each records the prior's own rank for the culprit so lift is
  measurable where it matters (FR-062b).
- **Private-corpus split**: the public repository ships synthetic structural twins and runs the
  full gate against them; the private repository runs the identical job against recorded
  production incidents with the identical thresholds. Each run records which corpus it ran
  against, and a release statement names both. No recorded production fixture is ever committed
  to the public repository (ADR-0003 D9).
- **The coverage audit is re-run** by `aisre audit coverage --incidents <file> --out <dir>`,
  which writes a dated machine-readable result and a markdown summary beside
  `docs/evaluation/coverage-audit-2026-09.md`, records the feeder set it measured against, and
  is the number `π₀` and every published accuracy target cite.

**Rationale**: all of it follows the reviewer's grading section and ADR-0003 D5, and the "no
second harness" decision is 001's own, already made and already true of the CI file that would
otherwise be duplicated.

**Alternatives considered**:
- *Majority-of-three as the gate.* Explicitly rejected by FR-061: a fixture solved 80% of the
  time passes a majority-of-three ~90% of the time, and eight such fixtures make a green build
  meaningless.
- *Gating on top-3.* Rejected: a 75% random baseline over four candidates, and the deterministic
  ranker already clears it on the current corpus.
- *A separate eval repository.* Rejected: the harness must move with the data model
  (constitution VIII), and the private split is about *data*, not about code.

---

## §13 — The worker and backend interfaces, and how they rhyme with `pkg/feeder`

**Decision**: two new public Go packages, each shaped like `pkg/feeder`.

```go
// pkg/worker — what the investigator calls.
type Description struct {
    Name         string        // "metrics"
    Source       string        // exactly one source of truth
    Capabilities []Capability  // named, typed, read-only
    ContainsModel bool         // FR-009a
    Redaction    RedactionPolicy
    Modes        []Mode        // Live, Recorded
    Version      string
}
type Worker interface {
    Describe() Description
    Call(ctx context.Context, req Request) (Response, error) // req.Term is an algebra term
}
```

```go
// pkg/backend — what a telemetry worker calls.
type Description struct {
    Name       string                   // "datadog", "gcp", "recorded"
    Vendor     string
    Terms      []TermName               // the algebra terms it can answer
    CostClasses map[TermName]CostClass
    Redaction  RedactionPolicy
    Version    string
}
type TelemetryBackend interface {
    Describe() Description
    Execute(ctx context.Context, req AlgebraRequest) (AlgebraResponse, error)
}
```

Both packages ship a `Recorder` (writes the world and the trajectory in the published format)
and a `testkit` (replays a recorded world through the implementation and diffs the digests
against goldens), exactly as `pkg/feeder` ships `record` and `testkit` today. A backend is
judged by the same standard as a feeder: read-only, declared capabilities, recorded responses
as its test (spec, Casey's persona).

**Rationale**: Casey already knows one idiom — `Describe` plus a single entry point plus a
recorder plus a testkit — and the cost of a second idiom is paid by every future connector
author. Splitting worker from backend is not symmetry for its own sake: the worker boundary is
where the *investigator's* contract lives (hypothesis, discriminating question, digest, coverage,
join keys), and the backend boundary is where the *vendor's* contract lives (rate limits, quota
headers, vendor query text, sanitisation). 005 implements `TelemetryBackend` and never
implements `Worker`; the metrics/logs/traces workers implement `Worker` and never speak Datadog.

**Alternatives considered**: one merged interface — rejected, see Complexity Tracking; an
interface per telemetry kind (`MetricsBackend`, `LogsBackend`) — rejected because a vendor
serves several kinds behind one credential and one quota, which is 005's own framing.

---

## §14 — The investigation store and the graph: resolving the principle I/III/IV tension

**Decision**: the run's working material (ledger, judgments, evidence items, worker calls,
spend) lives in PostgreSQL schema `investigation`; the *conclusion* is one typed graph event;
the *bulk* (digests, exemplars, model turns) is files beside the graph, addressed by key and
digest.

**Rationale**: the spec already resolves the principle IV half of this in "What is recorded, and
where", and principle I's escape clause is the rest: a derivation is permitted when it "can be
dropped and rebuilt from the event log without loss". The investigation schema satisfies that
in the exact sense that matters — drop it, and `aisre investigate replay --from <export>`
rebuilds every row from the decision record (in the graph) plus the recording (beside it). What
it must *not* be is a second place where facts about the production system live, and it is not:
every structural fact in it is a copy of a graph answer, carried with the event ids it came from.

Principle III's one-event-per-transaction discipline is honoured because an investigation emits
exactly one decision record, at conclusion, in one transaction — not one event per judgment. A
human fact, a reopen and a label are each one event for one human action.

**Alternatives considered**:
- *The ledger as graph events.* Rejected: ~500 events of transient working state per
  investigation, whose only consumer is the run itself, in a log whose value is that everything
  in it is a fact about the production system.
- *The ledger in memory only.* Rejected by FR-046 (in-flight inspection) and FR-057b (reopen).
- *A separate database.* Rejected under principle X: one more thing to operate for no isolation
  benefit; the schema separation is enough, and it keeps the decision record's transaction and
  the ledger's transaction in one place.

---

## §15 — The feature 001 change package: shape and semantic version

**Decision**: every item is additive; `sreagent.graph.v1` takes a **MINOR** bump; `buf breaking`
passes; no event-log transformation is required (constitution IX's migration clause applies to
breaking changes only). The items are D1–D5 plus the `alert.transition` convention, the
`watches` edge and the future-valid-time confirmation, enumerated in plan.md.

Three points worth stating explicitly because they are where a reviewer will look:

1. **D2 adds fields rather than re-interpreting one.** `RankedChange.time_distance_seconds`
   keeps its floored semantics; `signed_time_distance_seconds` and `post_reference` are new.
   Changing field 7's meaning would be wire-compatible and behaviourally breaking, which is the
   worst kind of change.
2. **D1 changes scores, so goldens change.** Only where a candidate is later than the reference
   instant. On the current corpus that is the `rollout-regression` and `config-change` families
   when the reference instant is moved to an estimated onset; with the default
   `reference_at = t2` nothing moves, because no candidate is later than the end of the window.
   Re-recording is therefore a bounded, reviewable diff, and the 002 fixtures are recorded after
   it lands.
3. **D5's prop namespace matters for SC-010.** The decision record carries query parameters and
   response digests. 001's telemetry denylist rejects props named `value`, `body`, `trace_id`
   and arrays of numeric samples; a digest's hex hash under `sre.investigation.evidence_digest`
   must pass, and a smuggled sample must still fail. The change is an allow-listed prefix plus a
   test that a decision record carrying a series is rejected.

**Alternatives considered**:
- *002 computing the post-reference correction itself, client-side.* Rejected by FR-029b (the
  engine must not re-rank) and by principle IX (the published formula must be the one that ran).
- *A MAJOR bump with an event-log transformation.* Unnecessary: nothing is removed or
  re-interpreted.

---

## §16 — Intake: alerts, human-declared incidents, and one investigation per incident

**Decision**: intake normalises any of three inputs — a monitor state transition, a **human
declaration**, or an explicit node reference plus an instant — into one `alert.transition` shape
distinguished by the actor kind, and from there into: subject reference(s), symptom statement,
symptom instant, window, origin reference.

A **declared incident is a first-class intake, not a special case** (FR-001a). Concretely:

- It carries actor kind *human* (`ActorKind.PERSON` in the taxonomy of D3 — FR-002a's "kind
  human"), the severity declared, the instant of declaration, a title, the authenticated
  declaring identity, the origin reference of the place the incident lives, and zero or more
  target entity references. **None of these is invented by the engine.**
- The observed instant is pinned to the **instant of declaration** — not to the instant the
  engine observed it, not to the instant the investigation began (FR-004a). A later edit to the
  severity, title or targets does not move that pin: it is a separate, later, typed event with
  its own instant and author.
- The idempotency key is `(source, channel-or-incident identifier, declared instant)`, keyed on
  the place's **stable** identifier rather than its name, so a renamed channel is still the same
  incident and a re-delivered declaration is a no-op (FR-008b).
- Target references come **either** from the declaration, parsed by a published, stated,
  configurable naming rule whose id is recorded, **or** from a human who supplies them — and the
  provenance of each is recorded and distinguishable (FR-002b). Where neither yields a reference
  the engine does not guess: the outcome is `unknown` with "name the affected service(s)" as the
  resolving action, and a later human fact naming them reopens the investigation.

**Association** to an open incident uses a published rule over both axes: proximity in time (the
symptom instant inside the open investigation's window extended by a published grace period,
default 30 minutes) **and** proximity in the graph (the same canonical subject, or an entity
within `hops ≤ 2` of one the other names). Whichever intake arrives first opens the
investigation; the later one attaches as an additional symptom with its own instant, origin
reference and actor kind. The grouping decision — with the time and neighbourhood distances it
rested on — is recorded as evidence, and a declaration the rule does **not** group opens its own
investigation rather than being dropped (FR-008c).

**Rationale**: the coverage audit is the argument. Over the audited corpus, incidents were
noticed by people and declared by opening a dated severity channel; monitor transitions fired on
few. An engine that can only be started by a monitor would not have started at all on most of the
corpus. Normalising both into one event shape — rather than bolting a "manual mode" onto the side
— is what keeps one association rule, one idempotency discipline and one investigation per
incident true across both front doors.

**Alternatives considered**:
- *A separate "manual investigation" path.* Rejected: it would duplicate intake, the association
  rule and the idempotency key, and would make FR-008a's one-investigation-per-incident
  guarantee hold only within each path.
- *Vendor incident records as the grouping key.* Rejected here: it makes intake vendor-specific
  and the reviewer explicitly defers incidents as a graph source (§5 of the review).
- *Time-window-only grouping.* Rejected: two unrelated incidents in the same half hour is the
  normal case in a large estate — hence the neighbourhood axis.
- *Parsing the affected service out of a channel title with a model.* Rejected: FR-002b requires
  a published, stated, configurable rule, and `unknown` plus "name the service" is a better
  answer than a plausible guess (constitution V).

---

## §16a — Returning the report to where the incident lives

**Decision**: where the incident was declared in a place that can hold an answer, the engine
renders its report there, **edits it in place** as the investigation progresses and concludes,
and records the delivery. The rendering order and the deep links are unchanged (FR-057c/d). The
transport belongs to a future chat connector; what this feature owns is the contract
(`ReportDelivery`) and four properties it guarantees:

1. **Report-only.** The credential that posts grants nothing beyond writing and editing that
   report and is separate from every read credential. Returning a report is not an action against
   a production system (FR-057f).
2. **In place, not a stream.** One message, updated, with an `external_message_ref` and an
   update count — not a new post per turn.
3. **Never blocking, never a precondition.** A delivery that fails is recorded with its reason;
   the investigation still reaches a terminal state.
4. **The concluded record stays immutable.** The posting is a rendering of it, not a second copy
   of it (FR-007).

**Rationale**: the report is useless in a terminal nobody is looking at during an incident that
lives in a channel. The risk this introduces is a write credential, so the design confines it:
one target, one message, one scope, separate from everything that reads. See the post-design
Constitution Check, principle VII, where this is argued rather than waved through.

**Alternatives considered**: posting each turn as a new message — rejected as noise in the one
place an on-call is trying to think; making delivery a precondition of concluding — rejected by
FR-057f and by the never-block rule; reusing a read credential with write scope — rejected by
FR-008 and principle VII.

---

## §17 — The verifier pass

**Decision**: before an investigation concludes, a second model call on `claude-opus-5`, with a
fresh context containing only (a) the rendered claims and (b) the evidence items they cite —
never the investigator's reasoning — returns a structured verdict per claim: `supported`,
`unsupported`, `number_mismatch`, with the offending value named. Unsupported and mismatched
claims are removed or demoted before rendering, and the verifier's findings are recorded as part
of the investigation (FR-022a). Citation validity is computed from this pass and is a 100% gate.

**Rationale**: the failure this catches is the one an investigator cannot catch about itself —
a number that drifted from the digest it came from while a narrative was written around it.
Fresh context is what makes it a check rather than a continuation, and a different model is what
keeps its blind spots from being the same blind spots.

**Alternatives considered**:
- *A deterministic checker only.* Kept, and run first: every number in a claim must be
  string-matchable to a field of a cited digest, and every claim must carry at least one
  evidence id. The model pass exists for the remaining class — a citation that resolves but does
  not support. Both run; the deterministic one is the cheap gate.
- *The same model, same context, "now check yourself".* Rejected: it is neither fresh nor
  independent, and it is the pattern the requirement was written against.

---

## §18 — Redaction, the injection barrier, and the private corpus

**Decision**: redaction is declared per worker and per backend and applied **before anything
reaches disk or the model** — people identifiers dropped (not hashed), infrastructure
identifiers pseudonymised with a keyed HMAC so joins survive, log bodies reduced to masked
templates, monitor bodies dropped. The engine refuses to record a response from a worker whose
declaration does not cover a field it emitted. The injection barrier is structural: worker
output is only ever a `tool_result`; the one bounded free-text field per digest is flagged
unverified, is not citable alone, and is excluded from citation resolution; instructions can only
arrive on the mid-conversation system channel, which no worker can write to. A detector records
an `injection_attempt` evidence item without changing anything about the run (FR-017).

**Rationale**: ADR-0003 D9 sets the policy and 005 owns the sanitiser; this feature owns the
*boundary* and the proof that the boundary holds — a conformance fixture whose retrieved
document contains an instruction, asserting that scope, budgets, worker set, posture and output
are unchanged and that the attempt is recorded (US6 scenario 4).

**Alternatives considered**: hashing people identifiers — rejected by ADR-0003 (a hashed email is
still personal data); sanitising at commit time only — rejected because live mode would then be
able to surface what a recording may not keep.

---

## §19 — The coverage audit (User Story 0)

**Decision**: `aisre audit coverage --incidents <file> --graph <dsn> --out <dir>`. Input is a
human-supplied list: incident id, alert instant, known cause (free text plus a category), and
optionally the entity or change identifier the cause corresponds to. For each incident the audit
asks the graph, **as of the alert instant with observed time pinned to it**, whether an entity or
change matching the stated cause existed; it classifies `cause_present`, `cause_absent` with a
category from the published set (flag flip, IaC apply, DB migration, certificate expiry,
scheduled job, third-party outage, traffic shift, other), or `undecidable` with a reason. Output
is JSON plus markdown: per-incident classification, the aggregate ceiling with its incident
count, per-category counts, and the feeder set the graph was running.

The audit **measures; it never infers a cause** (FR-069). It is the first increment, it sets
`π₀` for the posterior rule (§8), and no accuracy target may be published above its ceiling
(FR-071).

**Rationale**: it is a few hours of work that reorders the roadmap, it has already done so once
(ADR-0004), and it is the only number in this design that turns "the engine could not have known"
from an excuse into a measurement.

**Alternatives considered**: deriving causes from the graph — rejected by FR-069 and by the
obvious circularity; skipping the audit and setting targets by intuition — rejected by ADR-0003
D1 and by the fact that the first audit found 0 of 13.

---

## §20 — Numbers this plan deliberately leaves open

The spec's Assumptions section is explicit that what remains is calibration, not shape. This
plan carries initial values so that nothing is unimplementable, and marks each as
*to be set with the corpus and the coverage audit in hand*:

| number | initial value | set by |
|---|---|---|
| `π₀`, the prior of *no observed change explains this* | `1 − ceiling` from the audit (0.38 on the 2026-09 audit) | the audit, re-run per deployment |
| aggregate pass@1 gate | published with its detection power once ≥ 8 fixtures × 3 runs exist | first full corpus run |
| harm-rate threshold | 5% (SC-016) | corpus |
| `not_recorded` miss-rate threshold | 5% (SC-018) | corpus |
| diminishing-returns `n`, `ε` | 5 calls, 0.02 L1 | corpus |
| judgment LRs | 1.5 / 3 / 10 / 50 | calibration; the table is versioned with the ledger rule |
| confidence buckets | five, as in §8 | calibration; finer buckets only when a bucket holds ≥ 20 hypotheses |
| per-backend cost classes | declared by each backend | 003/005 |

---

## Backend and estimator realism

*Added 2026-09-18, Phase 8 Track J. Origin: the corpus track's finding while authoring the three
adversarial fixtures (`/tmp/live/PHASE8_G_BLOCKED`) — `pkg/backend/synthetic` offered exactly one
degradation shape, propagated nothing to callers, and the SEASONAL_CUSUM estimator returned
spurious crossings on the flat series that resulted. All three are fixed here. The changes are
additive in the API and **not** additive in the recorded bytes: every world re-records.*

### §J1 — Degradation shapes

**Decision**: `synthetic.Change` gains four optional shape fields, and the multiplier a degrading
change applies at an instant is the full factor scaled by two fractions, both 1 for the shape the
generator has always produced.

| field | meaning | zero value |
|---|---|---|
| `Factor` | how much worse the change makes its targets, fully in effect | the package constant, 24 |
| `RampSeconds` | the degradation grows linearly from 1× at the change instant to the full factor at `At + RampSeconds` | 0 — a step |
| `LoadCoupled`, `Capacity`, `CapacityFraction` | the factor applies only above a demand ceiling, scaled by the relative excess above it and capped at the full factor once demand is twice the ceiling | off |

A fraction *f* of a factor *F* is `1 + (F − 1)·f`, so a fraction of 0 is no degradation at all and
a fraction of 1 is the whole of it. `degradationAt` stays deterministic, stays rounded to six
decimals, and the seeded wobble is untouched.

**Why load coupling needed a demand curve.** A ceiling under flat demand is either always breached
or never breached, and neither is a slow burn. So the scenario also gains an optional
`DiurnalAmplitude` (with `DiurnalPeakSecondsOfDay`, default 16:00 UTC), which multiplies the
generated request rate by `1 + A·sin(2π·(t − peak + 6h)/24h)`. Its default is **0**, which is a flat
day and the exact series every world already holds, so this one is additive in the bytes too.

**Rationale for a step remaining the default.** A rollout regression *is* a step, the onset
estimator is specified against a step, and `rollout-regression-01` is the fixture the whole engine
was built against. Making the step opt-out would have re-recorded thirteen worlds to express a
property three of them need.

**The named shapes**, in `pkg/backend/synthetic/shape.go`, are how a fixture asks for one without a
number leaving Go for YAML — where it would be tuned until the fixture passed. `ShapeByName` refuses
an unknown name rather than defaulting, so a manifest asking for a shape the generator cannot
produce fails the recording instead of recording a different fixture under the same name.

| shape | the fixture's prose | the parameters |
|---|---|---|
| `step` | "rolled out and started failing" | the default: factor 24 at the change instant, propagated |
| `slow-burn` | "a ConfigMap edit lowers payments' connection-pool ceiling from 64 to 8 … the pool only starts refusing work as the afternoon traffic climbs against it" | `LoadCoupled`, `CapacityFraction: 1.10`, `DiurnalAmplitude: 0.6` — demand is 1.234439× baseline at 11:32 and 1.556310× at 14:32, so the pool degrades by **3.81×** when it is narrowed and **10.54×** when the page arrives |
| `distant-culprit` | "fx-rates … starts failing … the errors simply travel up the chain" | the same as `step`. What that fixture was missing was never a shape; it was the travelling, which is §J2 |
| `adjacent-decoy` | the ledger rollout at 14:07, two hops out | the same as `step`, for the same reason |

Two of the three adversarial fixtures therefore share their parameters with the default, and saying
so is more useful than inventing a difference to justify three entries.

### §J2 — Propagation to callers

**Decision**: a degradation travels from a changed service to the services that call it, attenuated
once per hop and delayed by one resolution step per hop. **Default on**, with a scenario-level
switch (`Propagation: NoPropagation`) for a scenario that wants the old series deliberately rather
than by omission. The switch is a two-valued named type rather than a `bool` precisely so that the
*zero value* is the new default: a `Propagation bool` would have made every scenario that predates
the field opt out silently.

**The formula**, published so it can be checked by hand:

```
E(v, t) = D(v, t) · (1 + Σ_{v → c} α · (E(c, t − Δ) − 1))
```

*E* is the multiplier a service's error rate actually carries, *D* is its own degradation from the
changes targeting it, the sum runs over the callees of *v* along propagating edges, **α = 0.6** is
the attenuation and **Δ is one resolution step** (60 s in every fixture). Applying it recursively
gives a callee *h* hops away an attenuation of α^h and a delay of h·Δ for nothing extra, which is
what a chain of timeouts looks like: each hop loses some of the signal and gains some of the delay.
The walk is bounded at four hops — one more than the longest chain any fixture holds — and carries a
visited set, so a cycle in the graph terminates.

Worked, on `checkout → payments → ledger` with a 24× step on ledger at *T*: ledger reads 24 at *T*;
payments reads `1 + 0.6·23 = 14.8` at *T*+1 min; checkout reads `1 + 0.6·13.8 = 9.28` at *T*+2 min.

An edge propagates when its type is `CALLS`, `DEPENDS_ON`, or **unset**. Unset is treated as a call
because the generator's other consumers of the same slice — `error_spans`, `weightOf`, `spanCounts`
— already do; `Edge.Type` is the new, optional, additive field a recorder fills in to keep
degradation off `RUNS_ON` and `CHANGED_BY`.

**Why this is not a modelling nicety.** `onset` is the first telemetry term every investigation
asks and it is asked on the **subject's** pointer — the alerting service. Without propagation that
series is flat by construction, so the term that anchors the whole causal ordering was an estimate
over seeded noise. This is the difference between a corpus that exercises causal ordering and one
that exercises an estimator's false-positive rate.

**Alternatives considered.** *Propagating the symptom only onto the alert's subject* — rejected: it
would put the movement where the fixture author expected it rather than where the topology says it
goes, which is writing the answer into the question. *A queueing model (Little's law, explicit
concurrency)* — rejected under principle X: it needs a service-time distribution the graph does not
hold, and the extra fidelity buys nothing a one-parameter attenuation does not.

### §J3 — The onset criterion

**Decision**: a CUSUM crossing is a *candidate*, not an answer. It is published as an onset only
when the post-crossing level is at least **`k_effect` = 3** scaled median absolute deviations of the
pre-crossing segment away from that segment's mean, on the side that crossed, **and** at least
**`min_sustained_points` = 5** samples follow it. Otherwise the answer is the typed
`no_onset_detected` with confidence 0. Both parameters are in `Params`, in `method_parameters` on
every digest, and in `contracts/telemetry-backend.md` §"The onset digest, and when there is no
onset to report".

**The evidence.** On `adjacent-decoy-01`'s recorded checkout series — 90 flat points carrying only
the generator's ±4 % seeded wobble, over the 13:02–14:32 search window — the estimator returned
**13:27 ± 840 s**, forty minutes before the true onset at 14:07 and with an uncertainty that does
not reach it. That crossing measures **1.30 MADs**, well under the floor. The regression is pinned
in `pkg/backend/onset`'s own tests, both ways: the new criterion refuses it, and relaxing the
criterion to nothing reproduces 13:27 exactly, so the test pins a fix rather than a coincidence.

**Why an effect size in MADs of the baseline** rather than an absolute floor: scaling by the
series' own dispersion makes the test mean the same thing on an error rate of 0.4 % and one of
40 %, and the MAD is robust to the very shift being measured — the same argument `robustScale`
already makes one function earlier.

**Why persistence as well**: a level that changes in the last three samples of a window has not been
observed to persist. Calling that an onset is a prediction, not a measurement, and the term's whole
job is to report what was observed.

**No engine change was needed.** `Engine.EstimateOnset` already branches on
`digest.GetUnavailable()` and carries the reason through to `Onset.Line()` — "Symptom onset: could
not be estimated (*reason*). The alert instant *t* is used as the ranking reference and nothing is
exonerated on timing alone." `no_onset_detected` is one more reason on a path that already existed,
which is the argument for making it a reason rather than a new outcome.

**Alternatives considered.** *Narrowing the search window so the CUSUM has less noise to accumulate
in* — rejected: the window comes from the question (`OnsetSearchWindow(fired_at, lookback)`), and
narrowing it to make an estimator behave would move the culprit outside it on exactly the fixture
that needs it inside — `slow-burn-01`, whose 240-minute lookback exists for that reason. *Raising
h* — rejected: h trades false positives against detection delay on **every** series, while the
criterion costs nothing on a real shift; `rollout-regression-01`'s 24× step clears the floor by more
than an order of magnitude. *Requiring the estimate to agree with a change instant* — rejected
outright: that is the answer, and an estimator that consults it is not evidence.
