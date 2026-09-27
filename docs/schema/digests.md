# The digest contract

A **digest** is everything a worker or a telemetry backend is allowed to return: identifiers,
parameters, aggregates, comparisons, mined templates, exemplar references, pointers, join keys
and drill-down handles. Never a metric sample, a log body or a span payload (constitution IV).

The machine-readable source of truth is
[`api/sreagent/investigation/v1/investigation.proto`](../../api/sreagent/investigation/v1/investigation.proto)
§2; its prose is
[`specs/002-investigation-engine/contracts/telemetry-backend.md`](../../specs/002-investigation-engine/contracts/telemetry-backend.md)
§2–§6. The Go implementation is `internal/investigation/backend/digest.go` (the outcomes, the
coverage block, the caps) and `redact.go` (the redaction policy).

`internal/investigation/backend.NewResponse` is the **only** constructor of a response in this
repository. Everything a consumer relies on — a coverage block on every digest, a bounded size, a
stable key, a total ordering — holds because there is no other way to make one.

## The four things every digest carries

1. **Coverage** — what was searched, the window actually covered, the volume considered, the
   sampling applied, what was truncated and by which criterion, the backend's indexing lag, the
   vendor's remaining quota where it reports one, and whether the window was cut back to the
   investigation's horizon. A digest without a coverage block is rejected `missing_coverage`
   (FR-014a). A field that cannot be determined is **stated as undetermined, never omitted**:
   `volumeUndetermined`, `ingestionLagUndetermined`, `quotaUndetermined`.

   The horizon fields are `truncatedToHorizon` and `horizon`, written together with a
   `truncation` criterion spelled `horizon:<state>:<RFC 3339 instant>` — appended after a
   semicolon if the answer was already truncated for another reason, so one criterion never
   overwrites another. They are set by `AnnotateCoverageHorizon`
   (`internal/investigation/backend/horizon.go`) whenever the request's `observed_at` cut the
   window short, and the covered window in the same block is moved back to the horizon with them:
   a narrowed window that does not announce itself is worse than a refusal. See
   [the horizon rule](./algebra.md#the-horizon-rule--nobody-sees-the-future).
2. **Join keys** — version, workload, pod or host, trace and span ids, first-seen instants.
   Pseudonymised consistently rather than dropped: *a digest whose keys do not join is evidence
   about nothing* (FR-014b).
3. **Drill-down handles** — a reference the caller can present back, plus a human link that
   opens the same query over the same window. Never an embedded payload.
4. **Its own evidence** — the exact query as sent, the backend and vocabulary versions, the deep
   link, and the term key and response digest that make it citable.

Only `QUERY_FAILED` may carry no coverage, because a query that never ran covered nothing.

## The six typed outcomes, never collapsed

They are six Go **types**, not six values of one enum with a comment
(`internal/investigation/backend/digest.go`). The defect FR-027 names — reporting
`NOT_YET_INGESTED`, `QUERY_FAILED` or `NOT_RECORDED` as if it were `NO_DATA` — is a defect
precisely because the six read alike once they are collapsed into "the answer was empty". Making
them six types means a caller that wants "nothing happened" has to write down `NoData`, and a
renderer has to say something different about each.

| outcome | Go type | means nothing happened? | renders as |
|---|---|---|---|
| `DIGEST` | `DigestOutcome` | no | `digest` |
| `NO_DATA` | `NoData` | **yes** | *no data: the window was covered and held nothing* |
| `NOT_YET_INGESTED` | `NotYetIngested` | no | *not yet ingested: the window is inside the backend's indexing lag, so an empty answer means nothing* |
| `QUERY_FAILED` | `QueryFailed` | no | *query failed (rate_limited): …* |
| `PARTIAL` | `Partial` | no | *partial: missing …* |
| `NOT_RECORDED` | `NotRecorded` | no | *not recorded: … is in the algebra but this world does not hold it; it is a gap in the recording, not a fact about production* |

`Outcome.MeansNothingHappened()` is true for exactly one of them. That is the whole of FR-027,
expressed so it cannot be written wrong by accident.

**`NO_DATA` also answers a term whose data source the organisation does not have.** The coverage
block names the absent source and what was in fact searched, the answer is identical in live and
recorded mode and stable for the whole window, and the rendering tells the consumer to stop
retrying: *"this organisation has no trace data; the question cannot be checked here."* Never
`QUERY_FAILED`, never an unexplained empty digest, never a substituted source.

## The per-family digest shapes

| family | body | rows |
|---|---|---|
| metric | `MetricDigest` | `SeriesSummary` (tags, point count, interval, resolution, statistics, join keys, drill-down) and `Comparison` (statistic, baseline, symptom, deltas, direction, **separable**) |
| log | `LogDigest` | `LogPattern` (masked template, count, baseline count, new-in-window, status, join keys, drill-down) plus `counts_by_status`, `miner_version`, `model_used`, `model_added` |
| trace | `TraceDigest` | `SpanGroup` (operation, error kind, count, latency statistics, join keys, drill-down) |
| monitor | `MonitorStateDigest` | `MonitorTransition` plus start state, end state, per-group states |
| onset | `OnsetDigest` | the estimated instant, an uncertainty, the method and its parameters, or `unavailable` with a reason |
| errors by version | `ErrorsByVersionDigest` | `VersionBreakdown` (version, errors, total, error rate, join keys, drill-down, and `deploy_ref` or `deploy_ref_absent_reason`) plus the version attribute |
| exemplars | `ExemplarDigest` | `Exemplar` (masked text, redaction applied, join keys) plus the cap |
| knowledge | `KnowledgeDigest` | `KnowledgeItem` (document id, kind, linked entities, link provenance, authored at, age, score, citation, excerpt) plus the scorer version |

### A version group's deploy reference

Each `VersionBreakdown` names the deployed version in the platform-neutral `deploy.*` vocabulary, so
the group joins to the change a deploy feeder recorded, whichever platform deployed it (005 FR-040d,
ADR-0010 item 1). `pkg/feeder/versionstamp` normalises the raw value, the same way on every backend:

| raw value | `deploy_ref` |
|---|---|
| 40 or 64 hex characters | `deploy.commit_sha`, lower-cased |
| `image@sha256:…` | `deploy.image`, with the tag dropped |
| any other value with no whitespace | `deploy.release` (joins by lookup, and never merges through C8) |

When there is no reference, `deploy_ref_absent_reason` says why, and the raw `version` is kept:

- `ABBREVIATED_SHA`: the value looks like a commit, but the full form cannot be recovered.
- `MUTABLE_TAG`: an image tag two rollouts could share, such as `:latest`.
- `BARE_DIGEST`: an image digest with no image name.
- `NOT_A_STABLE_IDENTIFIER`: the value is empty or contains whitespace.

`DEPLOY_REF_ABSENT_REASON_UNSPECIFIED` means the group has a reference, or the backend predates the
field. It never means "none". How a service gets a stamp at all is
[docs/connectors/version-stamping.md](../connectors/version-stamping.md).

### Size and cardinality caps

| what | cap |
|---|---|
| series per metric digest | 50 |
| comparisons | 50 |
| mined log templates | 50 |
| span groups | 50 |
| version breakdowns | 25 |
| monitor transitions | 100 |
| knowledge items | 20 |
| exemplars | 10, and 512 bytes each |
| the one free-text field | 512 bytes |
| the whole response, canonically encoded | 64 KiB |

Exceeding a cap is **not an error**: the answer is truncated and the truncation is written
**into** the response, so a replay reproduces the truncation the investigator actually saw
(FR-037). The published criteria are `cardinality_cap`, `response_size_cap` and `free_text_cap`.
A response that still will not fit under the byte cap after sixteen reduction passes is refused,
because an answer nobody can bound is an answer nobody can budget.

Every repeated field is put in a published **total order before the cap is applied**, so the cap
drops the least interesting rows: new templates first then most frequent, errored span groups
first then largest, highest error rate first, highest score first. The ordering lives in
`NewResponse` rather than in each backend or worker, because whichever ran last would otherwise
decide the bytes and a byte-for-byte golden would be impossible.

### The one bounded free-text field

At most one per digest. It is prefixed `unverified: ` wherever it is rendered or recorded, so a
reader who quotes it quotes the marker too, and it is **never citable as evidence on its own**
(FR-014b). The knowledge worker uses it for exactly one sentence:

> `unverified: knowledge-derived: a document says this. A hypothesis resting only on a document is unconfirmed and may not reach `supported` until a source-of-truth worker agrees (FR-051).`

## The digest rule — what "the same answer" means

Every response carries a `response_digest`: `sha256(canonical(response))` over the response with
exactly **three fields cleared first**. The clearing is one exported function,
`pkg/backend.NormaliseForDigest`, and `pkg/backend.ResponseDigest` is the only thing in this
repository that calls it — a third-party backend that computes the digest differently produces
recordings this implementation refuses to replay.

| field cleared | why it is not part of "the same answer" |
|---|---|
| `response_digest` | a digest of itself is not a function of the answer |
| `duration_ms` | wall-clock time differs between two runs that agree on every fact |
| `mode` | `live` and `recorded` describe *how the call was served*, not what it says |

The third is the one that bites, and it is why the rule is a published function rather than a
convention. The contract requires **live and recorded mode to produce identical digests for the
same term** — coverage block and join keys included, not only the summary statistics.¹ A digest
that hashed `mode` could not: the recorder clears the field on the way to disk (`Recorder.Record`)
while every reader stamps its own afterwards (`workers.Base.Call`, `backend.Recorded.Execute`), so
the two sides would hash different bytes for the one answer and every replay would report a
corrupt recording.

`canonical(…)` is feature 001's canonical JSON, unmodified: sorted keys, RFC 3339 UTC instants,
protobuf lowerCamel field names, no insignificant whitespace.

## `Separates` — one rule, one place

A `Comparison` carries a boolean `separable`, and it is not a matter of taste. The rule is
`internal/investigation/workers/metrics.Separates(relativeDelta)`: a comparison separates when
`|relativeDelta| ≥ 0.20` — the published `SeparationThreshold` — and does not otherwise.

The threshold is deliberately generous. Reporting a real difference as non-separable costs one
more query; reporting noise as separable convicts or exonerates a change on nothing.

Two further conditions make a comparison non-separable whatever its delta: both sides zero (there
is nothing to compare), and a pair missing one of its two windows (a single-window answer is a
measurement, not a comparison).

It is exported because it must be applied in **exactly one place by everyone who applies it**. The
backend that generates a comparison — `pkg/backend/synthetic` here — states `separable` on what it
produces, and the worker re-states it on every answer it passes on. Two spellings of one rule is
one spelling too many: where they disagree, the worker's annotation changes a digest that has
already been hashed, and the recording that results carries a `response_digest` its own content
does not hash to.

Remember what the flag does and does not say. `separable: false` means *this comparison does not
discriminate* — a different sentence from *this change is innocent*. Failing to discriminate is
not evidence of innocence, and the ledger records it as a `neutral` judgment rather than a
refutation.

## Redaction

Declared in `RedactionPolicy`, applied **in live mode as well as when recording**, so a live
investigation cannot surface what a recording would not be allowed to keep. Policy version
`1.0.0`:

| class | treatment | why |
|---|---|---|
| people identifiers (`user`, `user.email`, `actor`, `triggered_by`, `owner`, …) | **dropped, not hashed** | a stable hash of a person is still a person: it joins across every digest in the corpus and is re-identifiable from any one known example (ADR-0003 D9) |
| infrastructure identifiers (pod, host, workload, trace and span ids) | **pseudonymised with a keyed HMAC, consistently** — `px_<16 hex>` | a digest whose keys do not join is evidence about nothing; the same pod is the same pseudonym in every digest of one recording and a different one in another |
| the **version** join key | **left alone** | it is what joins a metric to a change in the graph (ADR-0005 D6), which is the single most decisive piece of evidence in a rollout regression |
| log content | reduced to **masked templates** before it is returned at all | a template is what makes a log line publishable; the masking rules are ours and are versioned with the fixtures |
| monitor bodies | **dropped** | prose a human wrote about production is the one thing the injection barrier cannot bound |

The published masks are `<email> <url> <uuid> <ip> <ts> <hex> <dur> <path> <num>`, and masking is
idempotent. A backend that emits a field its declaration does not cover is rejected
`undeclared_redaction` — at the boundary, not in a corpus review months later.

## Worked example

`compare(payments-db error rate, 14:20 ± 15 min, ERROR_RATE)` from
`fixtures/incidents/rollout-regression-01-incident/world/4bb5db0c…ceef.json`, abridged:

```json
{
  "outcome": "DIGEST",
  "termKey": "4bb5db0c36ef784adbfe8ca6e3bb6c4a2d29c6212d1d9fdb2667b8f94679ceef",
  "responseDigest": "…",
  "costClass": "STANDARD",
  "digest": {
    "backendVersion": "0.1.0",
    "executedQuery": "compare(db.system=\"postgresql\" AND server.address=\"payments-db.shop.svc.cluster.local\", ERROR_RATE)",
    "coverage": {
      "dataSource": "metrics",
      "searchedEntities": ["je6t6xjo7zcswtrotfarz2rt3z"],
      "windowActuallyCovered": {"start": "2026-09-01T14:20:00Z", "end": "2026-09-01T14:35:00Z"},
      "volumeConsidered": "30",
      "sampling": "none",
      "executedAt": "2026-09-01T14:32:00Z",
      "ingestionLagUndetermined": true,
      "quotaUndetermined": true
    },
    "metric": {
      "comparisons": [{
        "statistic": "ERROR_RATE",
        "baseline": 0.012006, "symptom": 0.01175,
        "absoluteDelta": -0.000256, "relativeDelta": -0.021323,
        "direction": "flat"
      }],
      "series": [{
        "tags": {"service": "payments-db", "window": "baseline"},
        "pointCount": "15", "resolutionSeconds": "60",
        "intervalCovered": {"start": "2026-09-01T14:05:00Z", "end": "2026-09-01T14:20:00Z"},
        "statistics": {"ERROR_RATE": 0.012006, "MAX": 0.012274, "MEAN": 0.012006},
        "joinKeys": {
          "version": "baseline",
          "workload": "px_94214c146b6af4b7",
          "podOrHost": "px_c2bef8fd074c813a",
          "firstSeen": "2026-09-01T13:02:00Z"
        },
        "drillDown": {"handle": {"value": "eyJrIjoibWV0cmlj…", "mintedByTermKey": "4bb5db0c…ceef", "depth": 1}}
      }]
    }
  }
}
```

Read it as the contract intends:

- the **comparison** says `direction: flat` and the metrics worker sets `separable: false` on it,
  because the relative delta is -2.1 % and the published separation threshold is 20 %. That is
  *"this comparison does not discriminate"*, which is a different sentence from *"this change is
  innocent"* — failing to discriminate is not evidence of innocence;
- the **join keys** carry the version in the clear and the pod and workload as pseudonyms, so
  this digest joins to a change in the graph and to the log digest for the same pod, and to
  nothing outside this recording;
- the **coverage block** says which window was actually covered, and states the indexing lag and
  the quota as undetermined rather than omitting them;
- the **drill-down handle** is depth 1. Presenting it back gives the per-instance breakdown; a
  handle minted by *that* answer would be depth 2, which a world does not record and which
  answers `NOT_RECORDED` by design.

---

¹ `specs/002-investigation-engine/contracts/telemetry-backend.md` §5 and §5.1; the Go rule is
`pkg/backend/algebra.go` (`NormaliseForDigest`, `ResponseDigest`).

## Related

- [The query algebra](./algebra.md) — what may be asked.
- [Pointers](./pointers.md) — the join-key role vocabulary.
- [`docs/connectors/writing-a-worker.md`](../connectors/writing-a-worker.md) — building one.
