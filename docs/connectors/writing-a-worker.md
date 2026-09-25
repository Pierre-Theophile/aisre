# Writing a worker

A **worker** is a read-only capability provider bound to exactly one source of truth. It is what
the investigator calls; it is never what the investigator reads around.

This is the second half of [writing a feeder](./writing-a-feeder.md). A feeder tells the graph
*what exists and what changed*; a worker answers *what the telemetry says about it*. The two
halves of a connector share a credential and a quota, and nothing else. On purpose: a feeder
writes events and a worker returns digests, and the day those two paths can touch is the day a
metric sample can reach the graph (constitution IV).

If you are implementing a **vendor** — Datadog, Cloud Monitoring, Loki — you almost certainly
want [writing a backend](./writing-a-backend.md), not this page. Read §1 to find out which you
are.

```go
import "github.com/Pierre-Theophile/aisre/pkg/worker"
```

### Getting the SDK, and where the code lives

The module is **not published to a proxy yet** and carries no version tag, so `go get
github.com/Pierre-Theophile/aisre` does not resolve. Work from a clone, exactly as
[writing a feeder §Getting the SDK](./writing-a-feeder.md#getting-the-sdk) sets out: a `replace`
directive or a `go work` workspace onto the checkout.

Where the code lives is a decision worth making before you start, because it decides what you may
import.

| your package's import path | may import `internal/…`? | what that means |
|---|---|---|
| inside this repository (`internal/investigation/workers/<name>`) | yes | the ordinary case, and how all five workers here are written |
| under `examples/<name>/`, its own Go module with a `replace` onto `../..` — the shape [`examples/mcp-feeder`](../../examples/mcp-feeder) takes | yes, the path prefix still starts with `github.com/Pierre-Theophile/aisre/` | a connector that must not be linked into the `sre-agent` binary, e.g. one dragging in a vendor SDK. Keep its own `go.mod`/`go.sum`, and keep the `go.sum` covering whatever the root module imports, or CI fails |
| a module of your own (`example.com/my-worker`) | **no** | you get `pkg/worker`, `pkg/backend` and both testkits, and nothing under `internal/`. See the note below before choosing this |

**The honest limitation.** Several helpers this page names — `workers.Base`, `workers.Redaction()`,
`backend.NewResponse`, `engine.NewRecorded` — are under `internal/` today, so a worker in a module
of its own can implement the interfaces and run the conformance suite but cannot use them. Until
they are promoted to `pkg/`, write your worker inside this repository or under `examples/`. This is
a packaging gap, not a design decision.

Every Go file in this repository carries `// SPDX-License-Identifier: Apache-2.0` as its first
line, generated code included (`scripts/add-spdx-headers.sh`).

### It rhymes with `pkg/feeder`

The two SDKs are deliberately the same shape, so that reading one teaches you the other:

| | `pkg/feeder` | `pkg/worker` | `pkg/backend` |
|---|---|---|---|
| the declaration | `Describe() Description` — namespaces, ordering, reordering window | `Describe() Description` — source of truth, capabilities, model, redaction, modes | `Describe() Description` — vendor, terms, cost classes, redaction |
| the entry point | `Run(ctx, Source, Emitter)` — payloads in, events out | `Call(ctx, Request) (Response, error)` — one algebra term, one digest | `Execute(ctx, *AlgebraRequest) (*AlgebraResponse, error)` |
| the gate | `Description.Validate` at source registration | `Description.Validate`, run by `worker.Registry.Register` | `Description.Validate`, run by the backend registry |
| the recorder | `pkg/feeder/record` — payloads and the events they produced | — (a worker is recorded through its backend) | `pkg/backend.Recorder` — a **world**, `term_key → digest` |
| the testkit | `pkg/feeder/testkit` — `Run`, `Shuffle`, `DoubleDeliver`, `Conformance` | `pkg/worker/testkit` — `Declaration`, `Golden`, `ModesAgree`, `Conformance` | `pkg/backend/testkit` — the same three |
| the rule underneath | recorded real payloads are your test | recorded responses are your test | recorded responses are your test |

`SDKVersion` is `0.1.0` in all three and is recorded with every declaration: MINOR adds helpers,
MAJOR changes the interfaces.

## 1. Worker or backend?

They are different jobs and the split is not symmetry for its own sake.

| | worker | telemetry backend |
|---|---|---|
| whose contract it implements | the **investigator's**: hypothesis, discriminating question, digest, coverage, join keys | the **vendor's**: rate limits, quota headers, vendor query text, sanitisation |
| bound to | exactly one source of truth | exactly one vendor |
| interface | `Describe() Description` + `Call(ctx, Request) (Response, error)` | `Describe() Description` + `Execute(ctx, *AlgebraRequest) (*AlgebraResponse, error)` |
| the ones that exist | `graph`, `metrics`, `traces`, `logs`, `knowledge` | `recorded`, `synthetic`; feature 003's GCP backend is the first live one |
| where it is documented | this page | [writing a backend](./writing-a-backend.md) |

Feature 005 implements `TelemetryBackend` and never implements `Worker`. The metrics, logs and
traces workers implement `Worker` and never speak Datadog. If you are adding a vendor, you are
writing a backend, and the existing metrics/logs/traces workers will use it unchanged.

The five workers that exist, and what each is bound to:

| worker | source of truth | contains a model? | capabilities |
|---|---|---|---|
| `graph` | the temporal graph (001's `QueryService`) | no | `subgraph`, `diff`, `impact`, `pointers`, `node_history`, `resolution_audit`, `extent` |
| `metrics` | one telemetry backend | no | `compare`, `onset`, `errors_by_version` |
| `traces` | one telemetry backend | no | `error_spans`, `compare` over span groupings |
| `logs` | one telemetry backend | optional | `new_log_patterns`, `exemplars` |
| `knowledge` | durable knowledge linked to graph nodes | optional | `knowledge_search`, scoped to the investigation's subgraph |

**One source of truth per worker.** Two sources is two workers, and a worker bound to more than one
is rejected at registration — because an answer whose provenance cannot be established is not
evidence.

## 2. The declaration

```go
type Description struct {
    Name          string        // "metrics"
    SourceOfTruth string        // exactly one
    Capabilities  []Capability  // named algebra terms, read_only = true, one cost class each
    ContainsModel bool          // FR-009a — declared, not discovered
    ModelID       string        // required when ContainsModel
    Redaction     RedactionPolicy
    Modes         []Mode        // Live and Recorded, both mandatory
    Version       string
}
```

`Description.Validate` is the registration gate, and it refuses — with a published reason code —
a declaration the investigator must not be handed:

| refusal | when |
|---|---|
| `write_capability` | a capability with `ReadOnly = false`. A worker may not change state in its source, and the point of declaring it is that nobody has to trust the implementation. |
| `undeclared_capability` | no name, no source of truth, no capabilities, a duplicate capability, an unpublished cost class, `ContainsModel` with no `ModelID` (or the reverse), a mode that is neither `live` nor `recorded`, only one of the two modes, no version. |
| `undeclared_redaction` | a redaction policy with no version. A recording must say what was applied to it. |

Two of those deserve a sentence each.

**Both modes are mandatory.** A worker that cannot be replayed cannot be merged (constitution
VIII). There is no single-mode spelling of the declaration, so this is not something to remember.

**`ContainsModel` is declared, not discovered.** If a model touches your answer anywhere — even a
labelling pass over already-reduced data — say so and name the model. The logs worker is the only
one in this feature that may, its pass is off unless configured, and when it runs the digest
states what it added over the algorithm (`LogDigest.model_added`). The graph, metrics and traces
workers contain no model and their tests assert it.

Each capability is declared, not just named:

```go
type Capability struct {
    Name      string         // a published algebra term; anything else is rejected
    ReadOnly  bool           // must be true — `write_capability` otherwise
    CostClass CostClass      // cheap | standard | expensive; the zero value is rejected
    MaxWindow time.Duration  // widest window this capability answers over; 0 = the profile's cap
}
```

`MaxWindow` is a real declaration and not documentation: it is what lets the planner ask a question
you can answer instead of discovering your limit as a failure.

### Registering it

```go
reg := worker.NewRegistry()
if err := reg.Register(myWorker()); err != nil {   // runs Describe().Validate()
    return err
}
```

**Registration is the gate.** A declaration that does not pass never reaches the investigator, so
"this worker is read-only" and "this worker can be replayed" are properties of the *set* rather
than promises made by each implementation. Registering a second worker under a name already taken
is refused too: two workers answering to one name is an answer whose provenance cannot be
established.

### The two shapes it moves

```go
type Request struct {
    Capability string                          // an algebra term the worker declared
    Mode       Mode                            // live | recorded
    Algebra    *investigationv1.AlgebraRequest // term, both time dimensions, hypothesis, question
}

type Response struct {
    Worker, Capability string
    Mode               Mode
    Algebra            *investigationv1.AlgebraResponse  // outcome, digest, term key, digest hash
    Graph              proto.Message                     // the graph worker only
}
```

`Response.Graph` carries feature 001's own response messages beside the digest, and only the graph
worker sets it. It travels beside rather than inside because the digest's eight bodies are the
shapes a *telemetry* answer takes, and re-expressing a subgraph as a metric digest would lose
exactly the structure an investigator needs. Nothing about the boundary is weakened: a graph
response is already identifiers, versions and structure, and 001 admits no sample into the graph
in the first place.

## 3. Answering a term

```go
type Request struct {
    Capability string   // an algebra term name the worker declared
    Mode       Mode     // live | recorded
    Algebra    *investigationv1.AlgebraRequest
}
```

The request carries the **hypothesis it serves** and the **discriminating question** it is meant
to settle, and both are recorded with the call (FR-018a). A call that serves no hypothesis
records `exploratory:<reason>`; a call that says nothing about why it was made is refused by
`worker.Caller` before your worker sees it.

Every answer goes through `internal/investigation/backend.NewResponse`, which is the only
constructor of a response in the repository. It computes the term key and the response digest,
applies the published ordering and the caps, writes any truncation into the response, and refuses
a digest with no coverage block. You do not have to remember any of that; you have to not build a
response some other way.

If your worker holds exactly one telemetry backend — as metrics, traces and logs do — embed
`internal/investigation/workers.Base` and you inherit the capability check, the algebra
validation, the mode stamp and the coverage check:

```go
func New(b workers.Backend) *Worker {
    return &Worker{base: workers.Base{
        Declaration: worker.Description{
            Name:          "metrics",
            SourceOfTruth: workers.SourceOfTruthOf(b),
            Capabilities: []worker.Capability{
                workers.Capability(engine.TermCompare),
                workers.Capability(engine.TermOnset),
            },
            ContainsModel: false,
            Redaction:     workers.Redaction(),
            Modes:         workers.BothModes(),
            Version:       "0.1.0",
        },
        Backend: b,
        After:   annotate,  // optional: what the worker knows and the backend does not
    }}
}
```

### The `After` hook, and its two rules

`After` is where a worker adds its own judgement to an answer the backend produced. The metrics
worker uses it for exactly one thing — deciding whether a comparison **separates** the hypothesis
it was asked to settle — because separability is a statement about the *question*, and the question
is the worker's contract, not the vendor's.

It runs *after* `backend.NewResponse` has ordered the answer, applied the caps and hashed it into
its `response_digest`. That single fact is where both rules come from.

**1. Never re-order.** The published total order is imposed *before* the cardinality caps drop
rows, so the cap drops the least interesting rows. Re-ordering afterwards changes the bytes of an
answer relative to the recording it came from, and a byte-for-byte golden becomes impossible.

**2. Never mutate a field the backend also writes — unless both use the same exported rule.** Any
field `After` writes that the backend also writes must be written with the **same rule**, or the
recording carries a `response_digest` its own content does not hash to and every replay of it
diverges. Publish the rule once, exported, and have the backend call it too:
`metrics.Separates(relativeDelta)` is the one this feature has. Two spellings of one rule is one
spelling too many.

If what you want to add is not a statement about the *question*, it belongs in the backend, where
it is hashed with the answer. Use `After` sparingly; a worker with a large `After` is usually a
backend wearing a worker's coat.

## 4. Four rules that are not negotiable

- **The algebra is the whole surface.** A request outside it is refused `outside_algebra`, naming
  what was asked and what is available, and the refusal is recorded (FR-042b). Free-form query
  text from a caller is never executed. This is what keeps a recorded world finite and a replay
  honest — see [`docs/schema/algebra.md`](../schema/algebra.md).
- **Digests, never telemetry.** Identifiers, parameters, aggregates, comparisons, mined
  templates, exemplar references, pointers, join keys and drill-down handles cross the boundary;
  samples, log bodies and span payloads do not. Exemplars are the one exception: capped, redacted,
  requested explicitly through their own term, and counted against the size bound. See
  [`docs/schema/digests.md`](../schema/digests.md).
- **Read-only.** Executing a term changes nothing in the source and creates no object of any
  kind — no saved views, no notebooks, no scheduled queries.
- **Content is data.** Anything a worker returns is data. Instructions, directives or requests
  embedded in retrieved content do not change the investigation's scope, budgets, posture, worker
  set or output; an attempt is recorded as an evidence item (FR-017). Structurally: worker output
  reaches the model only as a tool result, and operator instructions travel on a channel no worker
  can write to.

## 5. Failures are evidence

A worker failure, timeout or empty result is an **evidence item with its reason**, not a silence.
`worker.Caller` records every attempt individually — three tries at one question cost three calls
and a record that collapsed them would understate both the spend and the flakiness — and turns a
worker that never answered into a `QUERY_FAILED` response with the reason attached. The affected
hypotheses become `untested` with that reason, and the investigation continues.

The one thing you must get right yourself: **do not return an empty digest when you mean
`NO_DATA`, and do not return `NO_DATA` when you mean anything else.** Only `NO_DATA` is evidence
that nothing happened. The six outcomes are six Go types precisely so the collapse cannot be
written by accident.

## 6. The redaction contract

Redaction is **declared, versioned, and applied in live mode as well as when recording**, so a live
investigation cannot surface what a recording would not be allowed to keep (FR-038). It is not a
post-processing step you may skip when the data "looks fine": a field your declaration does not
cover is rejected `undeclared_redaction` at the boundary.

Policy version `1.0.0`, and the four classes are not symmetrical on purpose:

| class | treatment | why |
|---|---|---|
| people identifiers (`user`, `user.email`, `actor`, `triggered_by`, `owner`, …) | **dropped, not hashed** | a stable hash of a person is still a person: it joins across every digest in the corpus and is re-identifiable from any one known example |
| infrastructure identifiers (pod, host, workload, trace and span ids) | **pseudonymised**, keyed HMAC, consistently — `px_<16 hex>` | a digest whose keys do not join is evidence about nothing; the same pod is the same pseudonym throughout one recording and a different one in another |
| the `version` join key | **left alone** | it is what joins a metric to a change in the graph, the single most decisive piece of evidence in a rollout regression |
| log content | reduced to **masked templates** before it is returned at all | a template is what makes a log line publishable; the masks are `<email> <url> <uuid> <ip> <ts> <hex> <dur> <path> <num>`, and masking is idempotent |
| monitor bodies | **dropped** | prose a human wrote about production is the one thing the injection barrier cannot bound |

Two consequences worth stating plainly. Dropping is not the safe default everywhere — dropping the
`version` key would make a rollout unprovable, and "redact everything" is how a corpus becomes
unusable. And pseudonymisation is *per recording*: a pseudonym that was stable across recordings
would be a cross-corpus join key, which is the thing it exists to prevent.

`workers.Redaction()` returns the policy the workers in this feature declare. If yours needs a
class that is not in the table, that is a schema change and an ADR, not a local decision.

If you are implementing the vendor side — `Describe`/`Execute`, the coverage block, the horizon
clamp, `onset` backend-side, minting handles, recording a world — that is a **backend**, and it has
its own page: [writing a backend](./writing-a-backend.md).

## 7. Recording and replay

A **world** is a map from a canonicalised algebra term to a digest:

```text
world/
├── index.json       # algebra version, hop radius, drill-down depth, window grid,
│                    #   term_key → file, term count, not_recorded count, miss rate,
│                    #   redaction policy version, index digest
└── <term_key>.json  # one recorded answer per term
```

The world is recorded through a **backend**, not through your worker — `pkg/backend.Recorder`, and
[writing a backend §7](./writing-a-backend.md#7-recording-a-world) has the detail. From the command
line it is two commands:

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

The index digest and each answer's own digest are **checked on load**. A world edited by hand is
refused rather than replayed: a recording that can drift is a recording that proves nothing.

In recorded mode your worker answers from the world and **never falls through to a live call**.
An in-algebra term the world does not hold is `NOT_RECORDED` with the term echoed back — never
`NO_DATA`, which would say "nothing happened in production", and never an approximation. The miss
rate (in-algebra requests answered `NOT_RECORDED`) gates **the fixture's** admission to the
corpus at 5 %, never the engine's score: a world that does not hold the answer to a reasonable
question has told us about the *recorder*, and scoring the engine down for it would reward an
engine that asks fewer questions.

## 8. The conformance suite

```go
func TestConformance(t *testing.T) {
    live := myBackend()                       // over the vendor, or a deterministic generator
    dir  := recordAWorld(t, live)             // or an existing fixture's world/
    recorded, _ := engine.NewRecorded(dir)

    testkit.Conformance(t, myWorker(recorded),
        testkit.WithFixture(dir),
        testkit.WithRecordedCounterpart(myWorker(recorded)))

    testkit.ModesAgree(t, myWorker(live),
        testkit.WithFixture(dir),
        testkit.WithRecordedCounterpart(myWorker(recorded)))
}
```

Three checks, and `pkg/backend/testkit` has the same three for a backend:

- **Declaration** — the registration gate, run rather than approximated, plus the assertion that
  an undeclared capability is not callable.
- **Golden** — every term in the world replayed through the worker and diffed against the
  recorded answer **byte for byte**. The world *is* the golden; there is no second set of expected
  files to keep in step with it. It also reports any capability the world never exercises, because
  a capability with no recorded response is a capability nobody has tested.
- **ModesAgree** — live and recorded produce identical digests for identical requests, **coverage
  block and join keys included**, not only the summary statistics (SC-003). This is what makes a
  recording a test of the live path rather than a test of itself.

All three **fail rather than skip** when no fixture is given: a conformance suite that is green
before it checks anything is worse than no suite at all.

**Recorded responses are your test.** Not a hand-written table of expected values that drifts from
the recording, not a mock that agrees with whatever you wrote last — the world your worker replays
*is* the golden, and `ModesAgree` is what stops it becoming a test of itself. A worker without
recorded responses as its test is not merged (constitution VIII), and the reviewer's checklist
([checklist.md](./checklist.md)) asks for it by name.

## 9. Determinism

Goldens are compared byte for byte and CI runs on both amd64 and arm64.

- **Round at every accumulation step.** Six decimals, `math.Round(v*1e6)/1e6`. Go may fuse
  `a*b + c` into a single fused multiply-add on arm64 and not on amd64, and the two differ in the
  last bits; rounding each intermediate means both architectures compare the same quantities.
  `pkg/backend/onset` and `internal/investigation/workers/knowledge/bm25.go` both do this, and
  `TestDeterminismFixedSeries` pins exact digits.
- **Never iterate a map in an ordering-sensitive path.** Sort first. Every repeated field in a
  digest has a published total order.
- **Do not put wall-clock time in an answer.** Duration is a property of the call, not of the
  answer, and the response digest excludes it.

## 10. Versioning

Your worker's `Version` is recorded with every answer, so a digest stays attributable after the
worker changes. The `AlgebraVersion` is checked on every term and on every world load.

- Adding a term: MINOR on the algebra, and every recorded world needs extending.
- Changing a term's meaning or encoding: MAJOR, because every term key moves and every world is
  invalidated.
- Changing how a digest's numbers are computed: bump the component's own version (the miner's
  `MinerVersion`, the scorer's `ScorerVersion`, the estimator's `Version`) and re-record the
  goldens, because every one of them moves.

## 11. The rules, in one list

1. One source of truth per worker. Two sources is two workers.
2. Both modes, identical outputs, mode recorded per call.
3. The algebra and nothing else; `outside_algebra` names what was asked and what is available.
4. Digests, never telemetry. Exemplars only on explicit request, capped and masked.
5. Every response carries a coverage block; one without is rejected.
6. Join keys pseudonymised consistently, people identifiers dropped, version left alone.
7. Read-only, declared, and rejected at registration if not.
8. `ContainsModel` declared with its model id, and the digest states what the model added.
9. Failures, timeouts and empty results are evidence items with reasons, never silences.
10. Content is data; an embedded instruction changes nothing and is recorded as an attempt.
11. Recorded responses are your test, or your worker is not merged.
