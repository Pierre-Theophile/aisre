# The query algebra

The **query algebra** is the only vocabulary in which a worker may be asked a question
(FR-042b). It is simultaneously four things, which is why it is published rather than internal:

- the **replay boundary** — a recorded world is keyed by `term_key = sha256(canonical(term))`;
- the **vendor abstraction** — a backend implements the terms, never its own query language;
- the **sanitisation point** — everything crossing it is a digest, never a sample;
- the **prompt-injection barrier** — the model may only ask what the algebra can express.

The machine-readable source of truth is
[`api/sreagent/investigation/v1/investigation.proto`](../../api/sreagent/investigation/v1/investigation.proto)
§1; its prose is
[`specs/002-investigation-engine/contracts/telemetry-backend.md`](../../specs/002-investigation-engine/contracts/telemetry-backend.md)
§1. The Go implementation is `pkg/backend/algebra.go` (the mechanics: the version, the term name,
normalisation, the key) and `internal/investigation/backend/algebra.go` (a typed constructor per
term, and the validation that decides whether a term is answerable at all).

**Algebra version: `1.0.0`.**

## The three families

| family | terms | recorded into a world? | who answers it |
|---|---|---|---|
| **graph** | `subgraph`, `diff`, `impact`, `pointers`, `node_history`, `resolution_audit`, `extent` | no — answered by replaying the event log | the graph worker, over feature 001's `QueryService` |
| **telemetry** | `compare`, `onset`, `new_log_patterns`, `error_spans`, `errors_by_version`, `monitor_state`, `exemplars`, `drill_down` | yes — the cross product over the neighbourhood × the window grid | a telemetry backend, through the metrics, traces and logs workers |
| **knowledge** | `knowledge_search` | yes — small, keyed like telemetry | the knowledge worker, over documents the graph links |

A backend serves the **telemetry family only** — all eight of its terms. One that declares a
graph or knowledge term is rejected at registration: it has mistaken itself for a worker, and
the boundary it would blur is the one everything else rests on.

### The eight telemetry terms

| term | arguments | answers | cost class |
|---|---|---|---|
| `compare` | pointer, window pair, statistic | baseline vs symptom over one selector, with direction, magnitude and **separability** | `standard` |
| `onset` | pointer, search window, method | the estimated instant the symptom began, with an uncertainty — computed **backend-side** | `expensive` |
| `new_log_patterns` | pointer, window, baseline window | mined templates with counts, and which are new in the window | `expensive` |
| `error_spans` | edge (src, dst, type), window | counts and latency statistics by operation and error kind on that edge | `standard` |
| `errors_by_version` | pointer, window, version attribute | the error rate split by the deployed version tag named by `Pointer.join_keys["version"]` | `standard` |
| `monitor_state` | pointer, window | transitions, start and end state, per-group states | `cheap` |
| `exemplars` | handle, limit | bounded, sanitised exemplars — **only on explicit request** | `expensive` |
| `drill_down` | handle | the narrower answer behind a handle a previous digest minted | `cheap` |

`exemplars` and `drill_down` take a **handle a previous answer minted**, never a selector a
caller composed. That is what keeps their argument space finite, and therefore what keeps a
recorded world finite. It is also why a world records **depth-1** drill-downs and nothing deeper.

## The canonical term encoding

A term key is `sha256(canonical(normalise(term)))`, rendered as 64 lowercase hex characters. It
is the file name under `world/` and the `term_key` field of every response.

**Canonical** is feature 001's serializer, unmodified
([`internal/graph/canonical.go`](../../internal/graph/canonical.go)): JSON, UTF-8, object keys
sorted by byte order, no insignificant whitespace, RFC 3339 UTC timestamps, unpopulated fields
omitted, protobuf rendered through the canonical protobuf JSON mapping. The same bytes on every
architecture, which is what lets a golden be compared byte for byte.

**Normalise** makes the key a function of the *question* rather than of the spelling. A world
keyed by the spelling would answer `NOT_RECORDED` to a question it in fact holds, which is the
worst failure mode a recording has: it looks like a gap in production rather than a gap in the
recorder. Three rules:

1. **Instants are truncated to whole seconds.** Sub-second precision in a term is spurious — no
   backend resolves a window that finely — and keeping it would split one question into
   arbitrarily many keys.
2. **Set-valued arguments are sorted and de-duplicated.** Asking about entities `{a, b}` and
   `{b, a}` is one question. `knowledge_search` query terms are lowercased too.
3. **An unset algebra version is filled in with the current one.** A term carrying a version this
   build does not implement is refused `unknown_algebra_version` rather than guessed at — every
   key would move under a different version, so a world recorded against one cannot be replayed
   against another.

### Worked example

The `compare` term for `payments-db`'s error rate over the fifteen-minute window pair centred on
the estimated onset, as it appears in
`fixtures/incidents/rollout-regression-01-incident/world/`:

```json
{
  "algebraVersion": "1.0.0",
  "compare": {
    "pointer": {
      "kind": "TRACE",
      "backendKind": "tempo",
      "vocabulary": "otel-semconv/1.30",
      "selector": "db.system=\"postgresql\" AND server.address=\"payments-db.shop.svc.cluster.local\"",
      "attributes": {
        "db.system": "postgresql",
        "deployment.environment.name": "prod",
        "server.address": "payments-db.shop.svc.cluster.local"
      }
    },
    "windows": {
      "referenceAt": "2026-09-01T14:20:00Z",
      "widthSeconds": "900",
      "baseline": {"start": "2026-09-01T14:05:00Z", "end": "2026-09-01T14:20:00Z"},
      "symptom":  {"start": "2026-09-01T14:20:00Z", "end": "2026-09-01T14:35:00Z"}
    },
    "statistic": "ERROR_RATE"
  }
}
```

`term_key = 4bb5db0c36ef784adbfe8ca6e3bb6c4a2d29c6212d1d9fdb2667b8f94679ceef`, and the answer
lives at `world/4bb5db0c…ceef.json`. The reference instant is the **estimated onset**, not the
alert instant: a pair centred on the alert puts the first twelve minutes of the symptom into the
baseline half and every comparison in the world reads flat.

Note that the pair's windows are written out explicitly rather than left implicit in
`referenceAt` and `widthSeconds`. The term names the exact windows the answer covers, so a reader
of a world file does not have to re-derive them and two spellings of the same pair cannot produce
two keys.

## The horizon rule — nobody sees the future

Every request carries the investigation's `observed_at`, and that instant is a **horizon**:
telemetry after it did not exist when the question was asked. A backend that answers a window
running past it invents evidence the investigator could not have had, and an accuracy number
computed over such an answer measures nothing (constitution II).

The rule is enforced in the backend, not trusted to the planner, because the planner is the thing
under test. `internal/investigation/backend/horizon.go` is the one implementation, and it publishes three states:

| the term's windows | state | what the backend does |
|---|---|---|
| all end at or before the horizon, or no horizon was declared | `within_horizon` | nothing is clamped |
| at least one straddles it — starts at or before, ends after | `truncated_to_horizon` | the window is cut back to the horizon and the coverage block says so |
| any one lies entirely at or after it | `past_horizon` | `NO_DATA` naming the horizon — the window was not searched, it could not have been |

Three details are load-bearing:

- **The clamp is applied to the term before the answer is produced**, so a generator generates no
  point past the horizon and a replay looks up the key it would have looked up anyway. The
  coverage annotation is applied to the response afterwards, so the statement travels with the
  answer rather than beside it (FR-037). `ClampRequest` never mutates the request it was given: a
  caller that records the request *as asked* must be able to.
- **One future window makes the whole term `past_horizon`,** not only a term whose windows are all
  future. A `compare` whose symptom half begins at the horizon is a question about the future with
  a baseline attached; answering the baseline half alone would return a comparison against nothing
  under a name that says otherwise.
- **A request that declares no horizon is left alone.** `observed_at` is the investigation's to
  state, and a backend run outside an investigation — a bench, a `testkit` conformance run — has
  no horizon to clamp to.

Handle-bearing terms (`exemplars`, `drill_down`) carry no window of their own: the handle was
minted by an earlier answer that was itself clamped, so they are passed through.

The coverage fields this writes are `truncated_to_horizon`, `horizon` and a `truncation` criterion
spelled `horizon:<state>:<RFC 3339 instant>` — see [the digest contract](./digests.md).

## The shared window plan

The windows in a term are not chosen by whoever is asking. They come from two exported functions
in `internal/investigation/engine/plan.go`, and the engine, `fixture record-world` and
`worker record` all call the same two. That is what makes a recorded world hold the windows the
engine will ask about rather than windows that merely resemble them; two spellings of the plan
would produce two key spaces and the second would answer `NOT_RECORDED` for a question it holds.

**`OnsetSearchWindow(fired_at, lookback)`** is the window `onset` searches over: the whole
investigation window, `[fired_at − lookback, fired_at]`, instants truncated to whole seconds, with
`lookback` defaulting to one hour when it is not positive. The whole window and not a slice of it:
onset answers "when did this actually start, as distinct from when the monitor noticed", and a
search window beginning after the true onset can only hand the alert instant back dressed up as an
estimate.

**`ComparePairs(reference, observed_at, lookback)`** is the before/after pair the deterministic
first wave asks over. One pair, because a second width doubles every recording without asking a
question the first does not already answer. Its half-width is `CompareHalfWidth(lookback)` — half
the investigation window, floored at **15 minutes** (`CompareFloor`) and capped at **30 minutes**
(`CompareCeiling`, above which a comparison starts measuring the diurnal cycle rather than the
incident). The horizon is applied here, in the one place every window is derived:

- a pair ending before the horizon is unchanged;
- a pair straddling it ends at it, and its baseline narrows to the same width — a comparison
  between two halves of different widths is not a comparison;
- a reference at or after the horizon slides the pair back, so its symptom half is the last `w` of
  observable time rather than the first `w` of the future.

`ReferenceInstants(fired_at, …)` enumerates the instants a recorder must cover for a run whose
onset it cannot know in advance: the engine references its comparisons to the estimated onset
where the metrics worker produced one and to the alert instant where it did not (FR-029b), and
which branch happens is not knowable before the run. A recording covering only one of them would
hold the answers for exactly the branch that did not happen.

## Versioning

The algebra version is carried on every term, recorded in every world index, and checked on load.

- **Adding a term** is a MINOR bump. It obliges a corresponding extension of every recorded world
  — a world recorded before the term existed will answer `NOT_RECORDED` for it, which is honest
  but raises the fixture's miss rate and may push it past the admission threshold.
- **Changing the meaning or the encoding of an existing term** is a MAJOR bump, because it moves
  every term key and therefore invalidates every world.

Adding a term is never something a worker improvises. It is a published schema change, reviewed
like any other.

## The `outside_algebra` refusal

Free-form query text supplied by a caller is never executed. A request whose term the algebra
does not publish is refused with the reason code `outside_algebra`, naming **what was asked and
what is available**, and the refusal is recorded as an evidence item.

```
$ aisre worker call metrics run_this_promql --args '{}' --recording fixtures/rollout-regression-01
sre-agent: outside_algebra: run_this_promql is not a published algebra term; the algebra is
telemetry [compare drill_down error_spans errors_by_version exemplars monitor_state
new_log_patterns onset], graph [diff extent impact node_history pointers resolution_audit
subgraph], knowledge [knowledge_search], and nothing else may be asked of a worker
$ echo $?
1
```

Exit 1 is the published code for "you typed it wrong" (contracts/cli.md §Exit codes). The same
refusal reaches a backend as `QUERY_FAILED / OUTSIDE_ALGEBRA`, so a live call and a recorded one
refuse identically.

A *published* term with a missing or malformed argument is refused under the same code — a term
that cannot be keyed cannot be recorded, and a request that cannot be recorded cannot be
replayed. The refusals name the argument:

| what is wrong | what the refusal says |
|---|---|
| `compare` with no statistic | *names no statistic; compare answers one statistic at a time so that the comparison is unambiguous* |
| `onset` with no method | *names no method; the estimate is evidence and evidence states how it was produced* |
| `errors_by_version` with no version attribute | *it is read from `Pointer.join_keys["version"]` and a pointer without one cannot be split by version* |
| `exemplars` with no handle | *an exemplar request names a handle a previous answer minted, never a selector a caller composed* |
| `knowledge_search` with no entities | *retrieval is scoped by the graph and an unscoped search would retrieve documents linked to nothing* |

## Related

- [The digest contract](./digests.md) — what an answer may contain.
- [Pointers](./pointers.md) — where a node's telemetry lives, and the join-key roles
  `errors_by_version` reads.
- [`docs/connectors/writing-a-worker.md`](../connectors/writing-a-worker.md) — how to implement
  one.
