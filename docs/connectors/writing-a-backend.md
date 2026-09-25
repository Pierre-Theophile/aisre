# Writing a telemetry backend

A **telemetry backend** speaks for exactly one vendor — Datadog, Cloud Monitoring, Loki, Tempo —
and answers the eight terms of the telemetry algebra over it. It is the third connector shape in
this project, after [the feeder](./writing-a-feeder.md) and [the worker](./writing-a-worker.md),
and it is almost certainly the one you want if you are adding a vendor: the metrics, logs and
traces workers already exist and will use your backend unchanged.

```go
import "github.com/Pierre-Theophile/aisre/pkg/backend"
```

### Getting the SDK, and where the code lives

The module is **not published to a proxy yet** and carries no version tag, so `go get
github.com/Pierre-Theophile/aisre` does not resolve. Work from a clone: a `replace` directive or a
`go work` workspace onto the checkout, exactly as
[writing a feeder §Getting the SDK](./writing-a-feeder.md#getting-the-sdk) sets out.

Then decide where the package lives, because that decides what you may import:

- **Inside this repository** — the ordinary case, and how `synthetic` and `recorded` are written.
- **Under `examples/<name>/`, as its own Go module** with `replace github.com/Pierre-Theophile/aisre
  => ../..`, the shape [`examples/mcp-feeder`](../../examples/mcp-feeder) takes. Use it when the
  vendor SDK must not be linked into the `sre-agent` binary — which is the usual case for a
  backend. Its import path still begins with `github.com/Pierre-Theophile/aisre/`, so it may still
  import `internal/…`. Keep its `go.sum` covering what the root module imports, or CI fails.
- **A module of your own.** You get `pkg/backend` and `pkg/backend/testkit`, and nothing under
  `internal/`.

**The honest limitation, stated before you build on it.** The pieces this page tells you to use —
`backend.NewResponse` (the only permitted response constructor), the coverage helpers,
`ClampRequest`/`AnnotateHorizon`, `NewWindow`/`NewWindowPair`, `metrics.Separates` — are all under
`internal/` today. `pkg/backend` exports the interfaces, the algebra, the digest rule, the recorder
and the conformance suite, but not yet the means to *build* a conforming response. So write your
backend inside this repository or under `examples/` until those are promoted to `pkg/`. That is a
packaging gap rather than a design decision, and it is the one thing to check has changed before
you start an out-of-tree backend.

There is also no plugin loader: a backend is registered in-process
(`pkg/backend.NewRegistry().Register`) or served over `TelemetryBackendService`. Compiling it in is
the supported path today.

Every Go file in this repository carries `// SPDX-License-Identifier: Apache-2.0` as its first
line (`scripts/add-spdx-headers.sh`).

The rule that shapes everything below: **the backend implements the vendor's contract, the worker
implements the investigator's.** Rate limits, quota headers, vendor query text and sanitisation are
yours. Hypotheses, discriminating questions and whether a comparison separates one are not.

## 1. The interface

```go
type TelemetryBackend interface {
    Describe() Description
    Execute(ctx context.Context, req *AlgebraRequest) (*AlgebraResponse, error)
}
```

Two methods, and the same two shapes a feeder has: a declaration that is checked at registration,
and one entry point that does the work. `Describe` must be a **pure function** — it is called by
the registry, by `backend list` and by the budget manager, and a declaration that changed between
calls would mean the budget was computed against a contract the call did not honour.

`pkg/backend` aliases the generated protobuf types (`AlgebraRequest`, `AlgebraResponse`, `Digest`,
`Coverage`, `JoinKeys`, `DrillDown`, `Handle`, `Window`, `WindowPair`, …) rather than restating
them, so a backend author and the engine read the same types out of the same package and the two
cannot drift.

## 2. The declaration

```go
type Description struct {
    Name           string                  // "datadog:prod", "recorded"
    Vendor         string                  // exactly one
    Terms          []string                // telemetry family only — all eight, or a subset
    Capabilities   []Capability            // per term: read-only, cost class, widest window
    CostClasses    map[string]CostClass    // cheap | standard | expensive, one per term
    Redaction      *RedactionPolicy        // with a version
    Version        string                  // recorded in every digest it returns
    AlgebraVersion string                  // the algebra it implements
}
```

`Description.Validate` is the registration gate, and every refusal names what was wrong and what
was expected — because a backend author reading that message is usually reading it *instead of*
the contract:

| refused when | what it means |
|---|---|
| no name, no vendor | a backend speaks for exactly one vendor and is identified by one name |
| no terms | it can never be called |
| a graph or knowledge term | it has mistaken itself for a worker; see [the algebra](../schema/algebra.md#the-three-families) |
| a term with no cost class, or the unspecified class | an unbudgetable term is an unbounded one |
| a capability with `ReadOnly = false` | `write_capability` (FR-016) |
| a redaction policy with no version | `undeclared_redaction` — a recording must say what was applied to it |
| an algebra version this build does not implement | every term key moves under a different version |

The **widest window** in a capability is a real declaration, not documentation: it is what lets the
planner ask a question your vendor can answer instead of discovering the limit as a failure.

## 3. Answering a term

The eight terms, their arguments and their cost classes are in
[`docs/schema/algebra.md`](../schema/algebra.md). Four obligations are yours and not the worker's.

**Serve `onset` backend-side.** The vendor query returns the series *to your process*; you run
`onset.EstimateOnset(series, search, onset.DefaultParams())` there and return only the estimate,
its uncertainty, the method (`onset.MethodName`, `seasonal_naive_cusum_binseg`) and its parameters.
The estimator's own `onset.Version` goes into the digest, so an estimate stays attributable after
the method changes. Raw samples never cross the digest boundary in either direction — that is the whole
reason the term exists rather than being investigator-side arithmetic. Call the shipped estimator;
do not invent your own, or two backends will disagree about when the same incident started.

**Answer a term whose data source you do not have.** `NO_DATA`, with a coverage block naming the
absent source and what was in fact searched, identically in live and recorded mode and stable for
the whole window. Never `QUERY_FAILED`, never an unexplained empty digest, never a substituted
source. `error_spans` where the organisation has no trace data is the canonical case, and the
rendering tells the consumer to stop retrying rather than to try again.

**Mint handles, never accept selectors.** `exemplars` and `drill_down` take a handle a previous
answer *of yours* minted. Encode whatever you need inside it; it is opaque to the caller. This is
what keeps the argument space finite, and therefore what keeps a recorded world finite. A world
records depth-1 drill-downs and nothing deeper.

**Refuse what is outside the algebra.** A request whose term the algebra does not publish is
`QUERY_FAILED / OUTSIDE_ALGEBRA`, naming what was asked and what is available, and the refusal is
recorded as an evidence item. Free-form query text supplied by a caller is never executed. That
is the prompt-injection barrier, and it only holds if every backend holds it.

## 4. Coverage is mandatory, and the horizon is yours to enforce

Every digest carries a **coverage block**: what was searched, the window actually covered, the
volume considered, the sampling applied, what was truncated and by which criterion, your indexing
lag, and the vendor's remaining quota where it reports one. A digest without one is rejected
`missing_coverage`. A field you cannot determine is **stated as undetermined, never omitted** —
`volumeUndetermined`, `ingestionLagUndetermined`, `quotaUndetermined`.

The **horizon** is the part a vendor implementation is most likely to get wrong, because the vendor
will happily answer a window that runs into the future. A request's `observed_at` is the
investigation's horizon; telemetry after it did not exist when the question was asked, and a
backend that answers past it invents evidence the investigator could not have had.

`internal/investigation/backend/horizon.go` implements the rule, and your backend should call it
rather than re-derive it (it is one of the `internal/` pieces named above):

```go
import engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"

clamped, horizon, state := engine.ClampRequest(req)   // never mutates req
// … answer `clamped` …
engine.AnnotateHorizon(resp, horizon, state)          // writes truncated_to_horizon + horizon
```

- a window ending at or before the horizon is untouched;
- a window straddling it is **cut back** and the coverage block says so (`truncated_to_horizon`,
  `horizon`, and a `truncation` criterion) — a narrowed window that does not announce itself is
  worse than a refusal;
- a term with **any** window lying wholly at or after the horizon is `NO_DATA` naming the horizon;
- a request declaring no horizon is left alone: a bench or a conformance run has none to clamp to.

The clamp is applied to the term *before* the answer is produced, so a generator generates no point
past the horizon and a replay looks up the key it would have looked up anyway.

## 5. The digest rule

Every response carries a `response_digest` = `sha256(canonical(response))` over the response with
**three fields cleared first**: `response_digest`, `duration_ms` and `mode`. The clearing is one
exported function — `pkg/backend.NormaliseForDigest` — and `pkg/backend.ResponseDigest` calls it.

A backend that computes the digest differently produces recordings this implementation refuses to
replay. The `mode` clearing is the one that bites: live and recorded must produce **identical
digests for the same term**, and the recorder clears the field on the way to disk while every
reader stamps its own afterwards. See [the digest contract](../schema/digests.md).

The other half of the same trap: if your backend computes a value a worker also annotates — the
comparison's `separable` flag is the one case in this feature — call the **same exported rule** the
worker calls (`metrics.Separates`). Two spellings of one rule produce a recording whose
`response_digest` its own content does not hash to.

## 6. Redaction happens before anything leaves the process

Declared in `RedactionPolicy` and applied **in live mode as well as when recording**, so a live
investigation cannot surface what a recording would not be allowed to keep. Policy version
`1.0.0`:

- **people identifiers dropped, not hashed** — a stable hash of a person is still a person: it
  joins across every digest in a corpus and is re-identifiable from any one known example;
- **infrastructure identifiers pseudonymised** with a keyed HMAC, consistently — `px_<16 hex>` —
  because a digest whose keys do not join is evidence about nothing;
- **the `version` join key left alone** — it is what joins a metric to a change in the graph, and
  it is the single most decisive piece of evidence in a rollout regression;
- **log content reduced to masked templates** before it is returned at all, with the published
  masks `<email> <url> <uuid> <ip> <ts> <hex> <dur> <path> <num>`, idempotently;
- **monitor bodies dropped** — prose a human wrote about production is the one thing the injection
  barrier cannot bound.

A backend that emits a field its declaration does not cover is rejected `undeclared_redaction` at
the boundary, not in a corpus review months later.

## 7. Recording a world

A **world** is layer 2 of a recording: a map from a canonicalised algebra term to a digest.

```text
world/
├── index.json       # algebra version, hop radius, drill-down depth, window grid,
│                    #   term_key → file, term count, not_recorded count, miss rate,
│                    #   redaction policy version, index digest
└── <term_key>.json  # one recorded answer per term
```

`pkg/backend.Recorder` is the interface; `NewRecorderWithOptions` is the implementation.

```go
rec, _ := backend.NewRecorderWithOptions(dir, opts)
resp, _ := live.Execute(ctx, req)
_ = rec.Record(ctx, req, resp)   // keyed by the request's canonicalised term
digest, _ := rec.Close(ctx)      // writes index.json, returns the index digest
```

Two disciplines make a world trustworthy, and both are the recorder's:

- **Recording the same term twice with different answers is an error.** A key with two values is a
  world that cannot be replayed deterministically, and the second write is far more likely to be a
  bug in the recorder's enumeration than a real disagreement.
- **The index digest is checked on load.** It covers the sorted (term key → file, response digest)
  triples, so adding, removing or altering any answer moves it. A world edited by hand is refused
  rather than replayed.

`--out` is any directory, and the `Recorder` can be driven straight from a test: execute the terms
you want recorded against your live backend, `Record` each, `Close`, and you have a `testdata/world`
the conformance suite can replay. That is the small path, and it needs no graph. The two commands
below are the large path, which enumerates the neighbourhood from a graph and is how an incident
fixture's world is produced.

```bash
# First recording: shape from flags.
aisre worker record \
  --out fixtures/incidents/<id>/world \
  --focus otel.service.name=checkout \
  --window 2026-09-01T13:00:00Z..2026-09-01T14:32:00Z \
  --hops 2 --grid 900,3600 \
  --culprit k8s.change=shop/payments@rev7

# Re-record: shape from the fixture's own manifest, so it cannot silently change.
aisre fixture record-world fixtures/incidents/<id>
```

Both call the same two exported window functions the engine calls — `engine.OnsetSearchWindow` and
`engine.ComparePairs` — so the world holds the windows the engine will ask about rather than
windows that merely resemble them. A recorder that derives its own windows drifts from the engine,
and every term the engine issues then keys to an answer the world does not hold. That is not
hypothetical; it is what happened, and it is why the functions are exported.

### `not_recorded` is not `no_data`

In recorded mode a backend answers from the world and **never falls through to a live call**. An
in-algebra term the world does not hold is `NOT_RECORDED`, with the term echoed back — never
`NO_DATA`, which would say "nothing happened in production", and never an approximation.

The **miss rate** — in-algebra requests answered `NOT_RECORDED` — is published on every
verification run and gates the **fixture's** admission to the corpus at 5 %, never the engine's
score. A world that does not hold the answer to a reasonable question has told us something about
the *recorder*, and scoring the engine down for it would reward an engine that asks fewer
questions.

## 8. The synthetic backend is the reference implementation

`pkg/backend/synthetic` is a complete, readable backend with no vendor behind it: it generates the
telemetry a declared incident shape implies, then answers the algebra over it. Read it before
writing yours — it is the shortest path from "what does `compare` actually return?" to a working
answer, and it is the backend the corpus is generated with.

`pkg/backend/onset` is the shipped onset estimator, and `internal/investigation/backend` holds
`NewResponse` — the only constructor of a response in this repository. It computes the term key
and the response digest, applies the published ordering and the caps, writes any truncation into
the response, and refuses a digest with no coverage block. You do not have to remember any of
that; you have to not build a response some other way.

## 9. The conformance suite

`pkg/backend/testkit` is the suite every backend must pass — the same standard as a feeder:
read-only, declared capabilities, **recorded responses as its test**.

```go
func TestConformance(t *testing.T) {
    dir := "testdata/world"                       // or an incident fixture's world/

    testkit.Conformance(t, myBackend(), testkit.WithFixture(dir))
}
```

Three checks, run together by `Conformance`:

- **`Declaration`** — the registration gate, run rather than approximated: every term published and
  in the telemetry family, exactly one published cost class per term, a versioned redaction policy,
  an algebra version.
- **`Golden`** — every term in the world replayed through the backend and diffed against the
  recorded answer **byte for byte**. The world *is* the golden; there is no second set of expected
  files to keep in step with it, because a golden that can disagree with the recording it was
  derived from eventually will.
- **`ModesAgree`** — live and recorded produce identical digests for identical requests, **coverage
  block and join keys included**, not only the summary statistics (SC-003). This is what makes a
  recording a test of the live path rather than a test of itself.

Every check **fails rather than skips** when no fixture is given: a conformance suite that is green
before it checks anything is worse than no suite at all.

There is no `aisre worker conformance` command. Conformance is a Go test in your package, so it
runs where the backend does and in the same CI job. The CLI's part is `backend list` (your terms,
cost classes and quota reporting as the operator sees them) and `worker call … --mode recorded
--recording <dir>` for re-running one answer by hand.

## 10. Determinism

Goldens are compared byte for byte and CI runs on both amd64 and arm64.

- **Round at every accumulation step.** Six decimals, `math.Round(v*1e6)/1e6`. Go may fuse
  `a*b + c` into a single fused multiply-add on arm64 and not on amd64, and the two differ in the
  last bits; rounding each intermediate means both architectures compare the same quantities.
- **Never iterate a map in an ordering-sensitive path.** Sort first.
- **Do not put wall-clock time in an answer.** Duration is a property of the call, not of the
  answer, and the response digest excludes it.

## 11. Versioning

- Your `Version` is recorded in every digest, so an answer stays attributable after you change.
- Adding a term is a MINOR algebra bump and obliges every recorded world to be extended.
- Changing a term's meaning or encoding is MAJOR: every term key moves and every world is
  invalidated.
- Changing how a number is computed bumps the component's own version (the estimator's `Version`,
  the miner's `MinerVersion`, the scorer's `ScorerVersion`) and re-records the goldens.

## 12. The rules, in one list

1. One vendor per backend. The telemetry family only, all of it you can serve.
2. `Describe` is pure; every term carries exactly one published cost class.
3. Read-only: no saved views, no notebooks, no scheduled queries, no vendor object of any kind.
4. Every digest carries a coverage block; undetermined fields are stated, not omitted.
5. The horizon is enforced here: clamp, annotate, and `NO_DATA` for a wholly-future window.
6. `NormaliseForDigest` before hashing, or your recordings will not replay.
7. Redaction applies in live mode too; an undeclared field is rejected at the boundary.
8. `onset` is computed backend-side with the shipped estimator; samples never leave the process.
9. Handles are minted, never accepted; drill-downs are recorded to depth 1.
10. `NOT_RECORDED` is a gap in the recording; `NO_DATA` is a fact about production. Never the one
    for the other.
11. Recorded responses are your test, or your backend is not merged.

### Reference

- [`docs/schema/algebra.md`](../schema/algebra.md) — the terms, the keys, the horizon rule, the
  shared window plan.
- [`docs/schema/digests.md`](../schema/digests.md) — what an answer may contain, the caps, the
  outcomes, the digest rule.
- [`specs/002-investigation-engine/contracts/telemetry-backend.md`](../../specs/002-investigation-engine/contracts/telemetry-backend.md)
  — the contract this page explains.
- [`docs/connectors/writing-a-worker.md`](./writing-a-worker.md) — the other half of the boundary.
- [`docs/connectors/checklist.md`](./checklist.md) — what a reviewer checks before you merge.
