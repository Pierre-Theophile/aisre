# Connector SDK Contract: `pkg/feeder`

The only public Go API. Semver-versioned together with the schema; a feeder declares the
schema version it emits. Every future connector (Datadog, GCP, GitHub, Slack, Notion, ...)
implements this interface. The two reference feeders are the conformance examples.

## Interfaces

```go
package feeder

// Feeder turns one external system into typed graph events.
type Feeder interface {
    Describe() Description
    // Run blocks until ctx is done. It reads from its Source and writes to em.
    Run(ctx context.Context, src Source, em Emitter) error
}

type Description struct {
    SourceID          string        // e.g. "k8s:prod-eu1"
    Kind              string        // "k8s", "otel", "datadog", ...
    SchemaVersion     string        // event schema version emitted
    Ordering          Ordering      // PerSourceSequence | None
    ReorderingWindow  time.Duration // FR-021 shuffle tolerance
    RequiredScopes    []string      // documented read-only scopes
}

// Source abstracts live vs recorded input so both modes share one code path (FR-044).
type Source interface {
    // Next returns the next raw payload (opaque bytes + metadata) or io.EOF.
    Next(ctx context.Context) (Payload, error)
}
type Payload struct { Kind string; At time.Time; Seq int64; Bytes []byte }

// Emitter validates and forwards events. Validation errors are returned, never swallowed.
type Emitter interface {
    Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error)
    Checkpoint(ctx context.Context, from, to time.Time, gapBefore bool) error
}
```

Helpers: `feeder.NewID(sourceID, parts ...string)` builds deterministic event ids;
`feeder.Ref(ns, v)`; `feeder.Props()` builder that rejects denylisted telemetry keys at
compile-time-ish (typed setters) and at runtime; `feeder.WeightClass(rps float64) uint32`.

## Recording and testing

- `record.Wrap(src Source, dir string) Source` tees payloads into `<dir>/payloads/<kind>/`.
- `record.Emitter(em Emitter, dir string) Emitter` tees accepted events into
  `<dir>/events.jsonl` with the server-assigned `observed_at`.
- `testkit.Run(t, f Feeder, fixtureDir)` replays `payloads/` through `f` with an in-memory
  Emitter, then asserts: every event validates; event stream equals `events.jsonl` (canonical
  JSON); and, when a database is available, the projected graph equals `golden/`.
- `testkit.Shuffle(t, f, fixtureDir)` re-runs with payloads shuffled within
  `Description().ReorderingWindow` and asserts identical valid-time state.
- `testkit.DoubleDeliver(t, f, fixtureDir)` asserts all second deliveries are
  `DUPLICATE_NOOP`.

## Rules a feeder must follow (enforced by testkit where possible)

1. Never emit telemetry payloads (validated by Emitter; testkit fails the run).
2. Read-only credentials; verify at startup and refuse otherwise (FR-046).
3. Deterministic event ids from source-native identifiers (object UID + resourceVersion;
   span-window key), never random.
4. Emit `identity_claim` for every external identifier you know about the entity; let the
   graph resolve.
5. Emit `source_checkpoint` on start, on resync, and after any gap.
6. Ship a fixture directory or the feeder is not merged (constitution VIII).

## Versioning

- SDK: semver; MINOR adds helpers, MAJOR changes interfaces.
- Feeder: its own semver; `Description.SchemaVersion` pins the event schema; fixtures record
  both versions in `manifest.yaml`.
- Schema evolution: when the event schema bumps MAJOR, a transformation `vN→vN+1` is shipped
  in `internal/log/migrate` and fixtures recorded under vN remain replayable.
