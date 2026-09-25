# The Worker Contract

A **worker** is a read-only capability provider bound to **exactly one** source of truth. It is
what the investigator calls; it is never what the investigator reads around. The machine-readable
source of truth is [`investigation.proto`](./investigation.proto) §3.

## Declaration

```go
type Description struct {
    Name          string        // "graph", "metrics", "logs", "traces", "knowledge"
    SourceOfTruth string        // exactly one
    Capabilities  []Capability  // named algebra terms, typed in and out, read_only = true
    ContainsModel bool          // FR-009a — declared, not discovered
    ModelID       string        // recorded when ContainsModel
    Redaction     RedactionPolicy
    Modes         []Mode        // Live, Recorded — identical outputs for identical inputs
    Version       string
}

type Worker interface {
    Describe() Description
    Call(ctx context.Context, req Request) (Response, error)
}
```

An undeclared capability is not callable. A capability that changes state in its source is
rejected at registration (FR-016). A worker bound to more than one source of truth is rejected.

## The workers of this feature

| worker | source of truth | model? | capabilities |
|---|---|---|---|
| `graph` | the temporal graph (001's `QueryService`) | **no** | `subgraph`, `diff`, `impact`, `pointers`, `node_history`, `resolution_audit`, `extent`; both time dimensions passed through; deterministic |
| `metrics` | one telemetry backend | **no** | `compare`, `onset`, `errors_by_version` |
| `traces` | one telemetry backend | **no** | `error_spans`, `compare` over span groupings |
| `logs` | one telemetry backend | **optional** | `new_log_patterns`, `exemplars`; algorithmic template mining first, model labelling second, and the digest states what the model added |
| `knowledge` | durable knowledge linked to graph nodes | **optional** | `knowledge_search`, scoped to the investigation's subgraph; retrieval over telemetry is prohibited |

Chat and document readers are admitted on the same terms by later features. Their absence must
not block an investigation: the engine reports the source as unavailable.

## Every call carries its purpose

A request carries the **hypothesis it serves** and the **discriminating question** it is meant to
answer, and both are recorded with the call (FR-018a). A call that serves no hypothesis is
recorded as `exploratory:<reason>`.

## Every response is a digest

Identifiers, parameters, aggregates, comparisons, mined patterns, exemplar references, pointers,
join keys, drill-down handles and digests. Raw series, raw log bodies and raw span payloads are
never returned by default; bounded sanitised exemplars are returned only when asked for
explicitly, subject to the published caps, and they live in the recording and never in the graph.
Every response carries a coverage block, and a response without one is rejected.

## Modes, recording and redaction

Every worker runs **live** and **recorded** with identical outputs for identical inputs, and the
mode is recorded per call. Each worker declares the redaction it applies before recording; a
recording may not contain a value the worker declared it redacts.

## Failures are evidence

A worker failure, timeout or empty result is an evidence item with its reason, not a silence.
Retries are recorded individually and counted against budget. The affected hypotheses become
`untested` with that reason; the investigation continues.

## Content is data

Anything a worker returns is data. Instructions, directives or requests embedded in retrieved
content do not change the investigation's scope, budgets, read-only posture, worker set or
output; an attempt is recorded as an evidence item (FR-017). Structurally: worker output reaches
the model only as a tool result, and operator instructions travel only on a channel no worker can
write to.

## Testkit

`worker.Testkit` replays a recorded world through the worker and diffs its digests against
goldens, exactly as `feeder.testkit` replays payloads through a feeder and diffs events. A worker
without recorded responses as its test is not merged (constitution VIII, Casey's persona).
