# Replay Fixture Format

A fixture is a directory. It is the unit of testing, evaluation and connector conformance.

```text
fixtures/<fixture-id>/
├── manifest.yaml
├── payloads/
│   ├── k8s/…jsonl            # recorded raw watch events (one JSON per line)
│   └── otel/…pb | …jsonl     # recorded OTLP ExportTraceServiceRequest payloads
├── events.jsonl              # accepted events, canonical JSON, + observedAt/appendedSeq added by the log
├── rejected.jsonl            # events that MUST be rejected (hand-authored fixtures; recorded ones use payloads/)
└── golden/
    ├── pinned/               # same queries with observed_at pinned to clock.end (US7); required
    ├── <kind>.<name>.json    # one file per manifest query; kinds: subgraph, diff, impact,
    │                         # pointers, history, audit, suggestions, extent
    ├── diff.<name>.json
    ├── impact.<name>.json
    ├── pointers.<name>.json
    ├── history.<name>.json
    ├── audit.<name>.json
    └── suggestions.<name>.json
```

## manifest.yaml

```yaml
id: rollout-regression-01
family: rollout-regression          # baseline-topology | rollout-regression | config-change |
                                    # late-arriving-fact | ambiguous-identity |
                                    # retraction-with-edges | feeder-gap
description: payments rolled out at 14:20 among three decoy rollouts
hand_authored: false                 # true when events.jsonl was written by hand (no payloads/)
events: events.jsonl
rejected_events: rejected.jsonl     # optional
schema_version: 1.0.0
sdk_version: 0.1.0
sources:
  - source_id: k8s:demo
    kind: k8s
    ordering: per_source_sequence
    reordering_window: 60s
  - source_id: otel:demo
    kind: otel
    ordering: none
    reordering_window: 300s
clock:
  start: 2026-09-01T13:00:00Z
  end:   2026-09-01T14:40:00Z
queries:                            # what golden/ must contain
  - name: checkout-2hop-1432
    kind: subgraph
    focus: otel.service.name=checkout
    valid_at: 2026-09-01T14:32:00Z
    observed_at: 2026-09-01T14:32:00Z   # optional; unset = now — but see the rule below
    hops: 2
    direction: both                   # upstream | downstream | both
    per_hop_cap: 50                   # optional
  - name: checkout-diff
    kind: diff
    focus: otel.service.name=checkout
    t1: 2026-09-01T13:00:00Z
    t2: 2026-09-01T14:32:00Z
ground_truth:                       # used by `fixture verify --report`
  culprit_change: k8s:demo:deploy/payments@rev7
  expected_top_k: 3
  cross_source_pairs:               # 003 SC-021: must merge under a CERTAIN rule
    - pair: [otel.service.name=storefront-web, gcp.cloudrun.service=p/r/storefront]
      same: true
      rule: C4                      # optional: the rule expected to do it
  distinct_pairs:                   # the other half — pairs that must stay apart
    - pair: [gcp.cloudrun.revision=p/r/s/rev41, gcp.cloudrun.revision=p/r/s/rev42]
      same: false
expect_rejected:                    # events in payloads that MUST be rejected (SC-009)
  - event_id: otel:demo:bad-span-props-1
    reason_code: telemetry_payload
human_decisions:                    # replayed as events from principal dev:alice
  - confirm: [otel.service.name=checkout, k8s.deployment=demo/checkout-svc]
    at: 2026-09-01T14:35:00Z
    reason: same service
```

## Canonical serialization (used for events.jsonl and golden/*)

- JSON, UTF-8, keys sorted, no insignificant whitespace, one document per file (goldens) or
  per line (events).
- Timestamps in canonical protobuf-JSON form: RFC 3339 UTC, `Z` suffix, fractional digits only
  when non-zero (0/3/6/9 digits, as protojson emits). 64-bit integers (`sourceSeq`) are JSON
  strings, per proto JSON mapping; `appendedSeq` is a log-added plain integer.
- Each `events.jsonl` line is an `EventEnvelope` in proto JSON plus two log-added fields:
  `observedAt` (assigned at append; replay MUST use it) and `appendedSeq` (line number from 1).
  Loaders strip or ignore these two before `protojson.Unmarshal`.
- Arrays of nodes/edges ordered by `version_id`; ranked lists keep rank order; maps by key.
- Protobuf → JSON via the canonical proto JSON mapping, `emitDefaults=false`.

## Verification (`aisre fixture verify`)

1. Fresh database → register sources → apply `events.jsonl` in file order, using each line's
   `observed_at` (not the clock) → run every manifest query → byte-compare with `golden/`, then
   run the whole set again with `observed_at` pinned to `clock.end` and byte-compare with
   `golden/pinned/`.

   **An unpinned query is refused when the fixture is still waiting on observations.** "Now"
   is a fine question and a meaningless golden when any event's `observedAt` lies after the day
   the fixture is verified: every day before that instant sees a different prefix of the log, so
   the golden would encode the recording date. `fixture verify` fails the replay step naming
   the queries (added 2026-09-20 after `announced-fact-01` went red in CI the morning its first
   announcement became visible while its cancellation had not). Pin `observed_at` on such
   queries, usually to `clock.end`.

   **The pinned pass is mandatory** (T080, US7 scenario 2, FR-015). Every manifest query has a
   pinned twin; a manifest with no `clock.end`, an absent `golden/pinned/` directory, or a
   golden with no twin fails this step. "What did the graph know at T?" is the question US7
   exists to answer, and a fixture that does not record the answer is not evidence that the
   graph can answer it. `aisre fixture record` writes both sets in one run, so recording a
   twin is never extra work. A query whose kind this build cannot answer is skipped in both
   passes and named in the report, and has a golden in neither.
2. Re-apply `events.jsonl`; every result must be `DUPLICATE_NOOP`; goldens unchanged.
3. For each source, shuffle its events within `reordering_window`; apply; every manifest
   query with `observed_at` unset must match goldens **up to entity-id relabeling**: entities
   are matched by alias set, then compared structurally (valid-time state identical, ids may
   differ because canonical ids derive from the first-seen claim; see research §4).
4. `--report`: for `rollout-regression` fixtures, rank of `culprit_change`; for
   `ambiguous-identity`, precision of certain rules and calibration of probable scores.

## Recording (`aisre fixture record`)

Replays `payloads/` through the feeders named in `sources`, applies `human_decisions` at the
stated times, writes `events.jsonl` and `golden/` — including `golden/pinned/`, which is
written whenever the manifest has a `clock.end`. Diffs are reviewed in the PR.
