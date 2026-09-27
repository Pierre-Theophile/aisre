# The Telemetry Backend Contract

> **This document is the published home of the algebra, the digest, the coverage block, the join
> keys, the drill-down handles and the typed outcomes.** Feature 005 (Datadog) and feature 003
> (GCP) implement it; they do not define their own. The machine-readable source of truth is
> [`investigation.proto`](./investigation.proto) §1–§3; this page is its prose.

A **telemetry backend** executes one algebra term against one vendor and returns one bounded
digest. It is simultaneously the replay boundary, the vendor abstraction, the sanitisation point
and the prompt-injection barrier (ADR-0003 D8). It emits no graph events — that is the feeder
half of a connector, and the two halves share a credential and a quota, nothing else.

## 1. The algebra is the whole surface

The algebra has **three families**, and this document is the published home of all three:

| family | terms | recorded into a world? |
|---|---|---|
| **graph** | `subgraph`, `diff`, `impact`, `pointers`, `node_history`, `resolution_audit`, `extent` — typed wrappers over feature 001's published `QueryService` RPCs, both time dimensions passed through | **no** — answered deterministically from the replayed event log, so they are never cross-producted into a world |
| **telemetry** | the **eight** terms tabulated below: `compare`, `onset`, `new_log_patterns`, `error_spans`, `errors_by_version`, `monitor_state`, `exemplars`, `drill_down` | yes — the cross product |
| **knowledge** | `knowledge_search(entity_ids, query_terms, limit)`, scoped by the graph | yes — small, keyed like telemetry |

**A telemetry backend serves the telemetry family only** — all eight of its terms — and nothing
else. The graph family is served by the graph worker against 001's RPCs and the knowledge family
by the knowledge worker; neither is a backend concern. A backend that declares a graph or
knowledge term is rejected at registration.

A backend serves the published terms and **nothing else**. Free-form query text supplied by a
caller is never executed. A request outside the algebra is refused with
`QUERY_FAILED / OUTSIDE_ALGEBRA`, naming what was asked for and what is available, and the
refusal is recorded.

| term | arguments | answers |
|---|---|---|
| `compare` | pointer, window pair, statistic | baseline vs symptom over one selector, with direction, magnitude and separability |
| `onset` | pointer, search window, method | the estimated instant the symptom began, with an uncertainty — computed **backend-side**, because the series may not cross this boundary |
| `new_log_patterns` | pointer, window, baseline window | mined templates with counts, which are new in the window |
| `error_spans` | edge (src, dst, type), window | counts and latency statistics by operation and error kind on that edge |
| `errors_by_version` | pointer, window, version attribute | error rate split by the deployed version tag named by `Pointer.join_keys["version"]` (D4), each group naming its `deploy.*` reference where the version has one. A pointer with **no** version join key is answered by the engine, never a backend: `NO_DATA`, absent source "version stamp on this service's logs", naming every published stamp convention searched (005 FR-040b, ADR-0010 item 2) |
| `monitor_state` | pointer, window | transitions, start and end state, per-group states |
| `exemplars` | handle, limit | bounded, sanitised exemplars — **only on explicit request** |
| `drill_down` | handle | the narrower answer behind a handle a previous digest minted |

Terms are versioned with the digest contract. Adding one is a published schema change and a
corresponding extension of every recorded world — never something a worker improvises.

**A term whose data source the organisation does not have is still answered**, and answered the
same way by every backend: the typed outcome `NO_DATA` with a coverage block **naming the absent
source** as the reason and naming what was in fact searched. Never `QUERY_FAILED`, never an
unexplained empty digest, never a substituted source, and never a silent single group. The answer
is identical in live and recorded mode and is stable for the whole window, so a consumer can
conclude "this cannot be checked here" instead of retrying. `error_spans` where there is no trace
data source is the canonical case (003 FR-091, 005 FR-040b/FR-049b).

**`onset` is served backend-side by every backend.** The vendor query (Cloud Monitoring, Datadog)
returns the series **to the backend process**, which runs the published change-point method there
and returns only the onset digest — the estimated instant, its uncertainty, the method and its
parameters. Raw samples never cross the digest boundary in either direction, which is the whole
reason the term exists rather than being investigator-side arithmetic (constitution IV).

### The onset digest, and when there is no onset to report

The digest is `OnsetDigest`: `estimated_onset`, `uncertainty_seconds`, `method`,
`method_parameters`, `examined`, and the pair `unavailable` / `unavailable_reason`. The reference
implementation is `pkg/backend/onset` and every backend calls it rather than inventing its own.

**A crossing is not an onset.** A CUSUM given a wide enough search window will accumulate past its
threshold on noise alone, and the earliest such crossing is a confident instant describing nothing:
every causal-ordering decision downstream of it — which candidates started before the symptom,
which are exonerated as effects — is then keyed to an instant that describes no event. So before an
instant is published it has to clear a **published effect-size criterion**, and a crossing that does
not clear it is reported as `unavailable` with `unavailable_reason: no_onset_detected` rather than
as the earliest crossing.

Two conditions, both necessary, both stated in units the series itself supplies so that neither can
be tuned against a fixture:

| condition | statement |
|---|---|
| **effect size** | the mean of the run beginning at the estimated instant sits at least `k_effect` scaled median absolute deviations of the **pre-onset** segment away from that segment's mean, on the side the CUSUM crossed |
| **persistence** | at least `min_sustained_points` samples follow the estimated instant, so the shift has been *observed* to persist rather than predicted to |

A pre-onset segment shorter than two samples supplies no dispersion, so a crossing at the very head
of the window is refused for the same reason: there is nothing to be different from.

**The published defaults**, recorded in `method_parameters` on every onset digest:

| parameter | key | default |
|---|---|---|
| drift allowance | `k` | 0.5 |
| decision threshold | `h` | 5.0 |
| seasonal periods | `seasonal_periods` | 3 |
| seasonal period | `seasonal_period_seconds` | 86400 |
| minimum series length | `min_points` | 8 |
| **minimum effect size** | `k_effect` | **3.0** |
| **minimum sustained run** | `min_sustained_points` | **5** |

**Confidence.** The estimator computes a confidence in [0, 1] — `min(1, effect / (2·k_effect))`, so
an estimate exactly at the criterion carries 0.5 and one at twice it carries 1 — and an unavailable
estimate carries **0**. There is no `confidence` field in `OnsetDigest` at algebra version 1.0.0, so
on the wire **confidence 0 is `unavailable: true` with a stated reason**; a numeric field is an
additive change a later algebra version can make. Nothing reads a numeric confidence today, and
adding a field to buy one would be a schema change for no consumer.

**What the engine does with `no_onset_detected`** is exactly what it does with `not_recorded` and
`NO_DATA`: it falls back to the alert instant, **says so in the output**, and exonerates nobody on
timing alone (`internal/investigation/engine/causal.go`, FR-029a). The four other reasons —
`absent`, `too_sparse`, `flat`, `no_crossing` — are unchanged and take the same path. None of the
five is "we did not find anything": a method that cannot tell *nothing happened* from *we could not
look* exonerates innocent changes on timing alone.

## 2. Every digest carries the same four things

1. **Coverage** (`Coverage`), and a digest without one is invalid: what was searched, the window
   actually covered, the volume considered, the sampling applied, what was truncated and by which
   criterion, and the backend's indexing lag at the instant of execution. A field that cannot be
   determined is stated as undetermined, never omitted. Where the vendor reports it, coverage
   also carries the **remaining quota** and its window — this is what the budget manager spends a
   share of.
2. **Join keys** (`JoinKeys`): version tag, workload, pod or host, trace and span identifiers,
   first-seen instants. Sanitisation pseudonymises these consistently rather than dropping them —
   *a digest whose keys do not join is evidence about nothing.*
3. **Drill-down handles** (`DrillDown`): for every group, pattern, series or error kind, a handle
   the caller can present back, and a human link that opens the same query over the same window.
   A handle is a reference, never an embedded payload.
4. **Its own evidence**: the exact query as sent, the pointer and node it came from, the requested
   window and reference instant, the execution instant, the vendor's request identifier where
   there is one, and the backend and vocabulary versions.

At most **one bounded free-text field** per digest. It is flagged unverified and is never citable
as evidence on its own.

### 2.1 Nobody sees the future: the horizon

An investigation observes the world at one instant, and states it: `AlgebraRequest.observed_at`.
**Telemetry after that instant does not exist** (constitution principle II). Not "is out of
policy", not "is uninteresting" — it is not data the investigator could have had, and an accuracy
number computed over it measures nothing.

Every backend, live or recorded, MUST therefore apply the horizon before it answers:

| the window asked for | what the backend does |
| --- | --- |
| ends at or before `observed_at` | answered as asked; nothing is clamped |
| starts before `observed_at`, ends after it | **truncated to `observed_at`** and answered over the truncated window |
| any window of the term lies entirely at or after `observed_at` | **`NO_DATA`**, with the horizon as the reason |

A truncated answer says so in its own coverage block — never beside it (FR-037):

* `coverage.truncated_to_horizon = true`,
* `coverage.horizon = observed_at`,
* `coverage.truncation` names the criterion: `horizon:truncated_to_horizon:<RFC3339>`, or
  `horizon:past_horizon:<RFC3339>` on the `NO_DATA`.

Three consequences are contract rather than convenience.

**The clamp happens before the answer is produced, not after.** A generator generates no sample
past the horizon and a vendor query is sent with the truncated window, so "the coverage was
clamped" and "the data was clamped" cannot come apart. The term key an answer carries is the key
of the window that was *actually answerable*, which is what lets a recorded world be looked up by
the same key a live call would have used — a recorder files the answer under the clamped term too.

**A wholly-future window is refused rather than narrowed.** A window is half-open and non-empty
by contract (§1), so there is no clamp of a window that begins at the horizon that is still a
window. A `compare` whose symptom half begins at `observed_at` is a question about the future with
a baseline attached; answering the baseline half alone would return a comparison against nothing
under a name that says otherwise. Asking a comparison that *ends* at the horizon is the planner's
job; refusing to pretend it did is the backend's.

**A request that names no `observed_at` declares no horizon.** A bench, a conformance run or a
world recording taken outside an investigation has no instant to be honest about, and a backend
that clamped one of those to an instant nobody named would be inventing policy. The rule binds
investigations, which is precisely where the accuracy claims come from.

In recorded mode the rule is enforced by the recorded backend itself rather than trusted to the
recording: a world may have been recorded by a vendor or a generator that had no horizon to
respect, and serving what it holds for a future window would launder the future through the
replay. The recorded backend answers the `NO_DATA` without consulting the world — a window in the
future holds nothing in *any* recording, and a `NOT_RECORDED` there would blame the fixture for a
fact about time — and re-states the truncation on an answer whose recorded coverage overstates it,
recomputing `response_digest` over what actually leaves the process (§5.1).

## 3. What may never be in a digest

Raw metric samples, raw log bodies, raw span payloads, or any aggregate presented as one. A
metric digest carries per-series statistics and a comparison; a log digest carries masked
templates and counts; a trace digest carries groups and latency summaries. Exemplars are the one
exception and they are capped, redacted, requested explicitly, and counted against the size
bound. Nothing in a digest ever reaches the graph (constitution IV).

## 4. Typed outcomes, never collapsed

`DIGEST` · `NO_DATA` (valid query, covered window, nothing in it) · `NOT_YET_INGESTED` (the
window is inside the indexing lag, so an empty answer means nothing) · `QUERY_FAILED` (with a
reason from the published set) · `PARTIAL` (what is missing is named) · `NOT_RECORDED` (recorded
mode only: in-algebra, not held by this world).

Only `NO_DATA` is evidence that nothing happened. Reporting any of the others as if it were is a
defect (FR-027).

### 4.1 `QUERY_FAILED / OUTSIDE_RETENTION` — the mirror of `NOT_YET_INGESTED`

Added by feature 003 (003 FR-104). `NOT_YET_INGESTED` covers a window too **new** for the vendor to
have indexed; `OUTSIDE_RETENTION` covers one too **old** for it to still hold, and before this
existed there was nowhere honest to put that.

The distinction from `NO_DATA` is the entire point, and collapsing it is the same defect as above
wearing a different hat. `NO_DATA` says the backend looked, the window was covered, and nothing was
there — a finding an investigation may reason from. `OUTSIDE_RETENTION` says the window was **never
covered**, because the data aged out before anyone asked. Reporting the second as the first tells an
investigation that nothing happened during exactly the window it cannot see, which is the most
confident wrong answer this system is capable of producing.

It is not a hypothetical: Cloud Run's `run.googleapis.com/*` metrics retain for **six weeks**, so
any question about an incident older than that is outside retention rather than empty.

`AlgebraResponse.retention_horizon` carries the horizon the vendor states, so a caller learns in one
refusal how far back it **could** have asked — rather than discovering the boundary by bisection,
spending quota to learn a number the vendor already publishes.

### 4.2 `Digest.vendor_request_id`

Added by feature 003 (003 FR-100), which requires every digest to carry its query, its execution
instant **and the platform's request identifier**. The first two had homes — `executed_query` and
`coverage.executed_at` — and the third had none.

It is deliberately **not** `coverage.data_source`, which names a metric namespace or an index rather
than one request, and deliberately not `free_text`, which is flagged unverified and is never citable
on its own (FR-014b). A request identifier is the most checkable fact in a digest; putting it in the
one field that cannot be cited would make it useless for the thing it exists for — taking a digest to
the vendor's support and asking what happened on *that* call.

Empty where the vendor returns no identifier, which is a fact about the vendor rather than a
deficiency in the digest.

## 5. Two modes, one output

A backend runs **live** against its vendor and **recorded** against a recording, and for the same
request the two MUST produce **identical digests** — coverage block and join keys included, not
only the summary statistics. In recorded mode the backend makes no network call and never falls
through to a live call to satisfy a miss.

### 5.1 What `response_digest` is computed over

`response_digest` is `sha256(canonical(response))` over the response with exactly **three fields
cleared** first, and a third-party backend that computes it differently will produce recordings
this implementation refuses to replay. The three, and why each is out:

| field | why it is not part of "the same answer" |
|---|---|
| `response_digest` | a digest of itself is not a function of the answer |
| `duration_ms` | wall-clock time differs between two runs that agree on every fact |
| `mode` | `live` and `recorded` describe *how the call was served*, not what it says |

The third is the one that bites. The rule above requires the two modes to produce identical
digests for the same term, and a digest that hashed `mode` could not: the recorder clears the
field on the way to disk, while every reader stamps its own afterwards, so recorder and reader
would hash different bytes for the one answer. A backend that computes the digest itself must
therefore clear all three before hashing; `pkg/backend.NormaliseForDigest` is the reference
implementation and `pkg/backend.ResponseDigest` calls it.

`canonical(...)` is feature 001's canonical JSON: sorted keys, RFC 3339 UTC instants, protobuf
lowerCamel field names, no insignificant whitespace.

A recorded window is a **world**, not a trajectory: it is a map from a question to an answer, so
a consumer that asks a different but in-algebra question is still served. It covers the union of

  - **every term the deterministic engine issues for the fixture's own question**, obtained by
    running that engine against the generator and recording what it asked, and
  - the cross product of the algebra over the alert neighbourhood and the window grid, plus the
    depth-1 drill-downs those answers mint, which is the supplement a model's exploration draws
    on.

The first of the two is what makes a world a superset by construction rather than by coincidence.
A recorder that enumerated only a grid has to *derive* the windows the engine will ask about, and
a derivation that lives in the recorder is a derivation that can drift from the one in the engine
— which is exactly what happened: the engine took its onset search window from the question and
the recorder took its from the manifest grid, the two never intersected, and every term the
engine issued keyed to an answer the world did not hold.

The **miss rate** — in-algebra requests answered `NOT_RECORDED` — is published on every
verification run and gates a fixture's admission to the corpus, never the engine's score.

## 6. Redaction happens before anything leaves the process

Declared in `RedactionPolicy` and applied in live mode as well as when recording, so a live
investigation cannot surface what a recording would not be allowed to keep: people identifiers
**dropped, not hashed**; infrastructure identifiers pseudonymised with a keyed HMAC, consistently,
so joins survive; log content reduced to masked templates; monitor bodies dropped. A backend that
emits a field its declaration does not cover is rejected (`undeclared_redaction`).

## 7. Read-only, priced by a published class

Executing a term changes nothing in the vendor and creates no vendor object of any kind —
no saved views, no notebooks, no scheduled queries. A backend declaring a state-changing
capability is rejected at registration.

Every capability declares exactly one **cost class** from the published set, which is what the
budget manager spends against (FR-047a):

| class | meaning |
|---|---|
| `cheap` | an indexed lookup or a monitor-state read: negligible vendor quota, safe to issue freely inside the window cap |
| `standard` | an aggregation over one selector and one window pair — the ordinary `compare`, `errors_by_version`, `error_spans` call |
| `expensive` | a scan-shaped term: `new_log_patterns` over a wide window, `onset` over a long search window, `exemplars` |

The set is closed and is published as `CostClass` in [`investigation.proto`](./investigation.proto)
§3. A declaration naming anything else is rejected at registration, and budgets are expressed per
backend **and per cost class**, never as a flat call count.

## 8. The Go SDK

`pkg/backend`, shaped like `pkg/feeder`:

```go
type TelemetryBackend interface {
    Describe() Description                                        // terms, cost classes, redaction
    Execute(ctx context.Context, req AlgebraRequest) (AlgebraResponse, error)
}
```

plus `backend.Recorder` (writes a world in the published layout) and `backend.Testkit` (replays a
recorded world through the implementation and diffs digests against goldens). A backend is judged
by the same standard as a feeder: read-only, declared capabilities, recorded responses as its
test.
