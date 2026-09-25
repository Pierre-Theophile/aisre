# Replay fixtures

A **fixture** is a directory holding a recorded slice of reality: the events a set of sources
emitted, and the answers the graph must give when those events are replayed into an empty
database. It is the unit of testing, evaluation and connector conformance for this project
(constitution VIII, FR-047 to FR-049).

Fixtures are how a change to the projector, the query layer or a feeder is proved not to have
changed what the graph says. `sre-agent fixture verify` replays every fixture from empty,
compares every query result byte-for-byte against `golden/`, double-delivers every event to
prove idempotency, and shuffles events within each source's reordering window to prove that
valid-time state does not depend on arrival order (FR-048).

The authoritative format is [`specs/001-temporal-graph-core/contracts/fixture-format.md`][fmt].
This file describes what is actually in this directory today and the two places where these
fixtures extend that contract.

[fmt]: ../specs/001-temporal-graph-core/contracts/fixture-format.md

## Layout

```text
fixtures/<fixture-id>/
├── manifest.yaml     # identity, sources, clock, the queries golden/ must contain
├── events.jsonl      # accepted events, one canonical JSON document per line
├── rejected.jsonl    # events that MUST be rejected (optional; see below)
├── payloads/         # recorded raw source payloads (absent from hand-authored fixtures)
└── golden/           # recorded query outputs; empty until `fixture record` runs
    └── pinned/       # the same queries with observed_at pinned to clock.end (US7)
```

## What is shipped today

| id | family | events | exercises |
|---|---|---|---|
| `baseline-topology-01` | baseline-topology | 89 (+1 rejected) | subgraph as-of, hub truncation, impact, pointers, extent, before-history, telemetry rejection |
| `late-arriving-fact-01` | late-arriving-fact | 15 | observed-time pinning (US1 scenario 4, US7 scenario 2) |
| `retraction-with-edges-01` | retraction-with-edges | 19 | retraction cascading to live edges (FR-013) |
| `rollout-regression-01` | rollout-regression | 92 | diff between two instants, ranked change candidates, unattached change (US2, SC-005) |
| `ambiguous-identity-01` | ambiguous-identity | 17 (+2 human decisions) | probable rules and suggestions, human confirm and split, the audit query, resolution calibration (US6, SC-007, SC-011) |
| `rollout-regression-02` | rollout-regression | 162 | the same ranking on a real rollout, recorded (T082, SC-005) |
| `config-change-01` | config-change | 93 | a ConfigMap edit with no rollout: a CONFIG_CHANGE node and a new value hash (T066, FR-043) |
| `feeder-gap-01` | feeder-gap | 105 | a feeder gap, a retraction inside it, and the gap in `extent` (T067, FR-052) |

Every fixture FR-047 requires is now shipped.

`fixtures/incidents/` holds feature 002's **incident fixtures** — a 001 fixture plus an
`incident:` block, a recorded `world/` and (from Phase 7) a `trajectories/` layer. It is a
group directory rather than a fixture, so `fixture verify fixtures/*/` reaches its members
without a second glob, and everything on this page applies to them unchanged. What they add is
described in [`fixtures/incidents/README.md`](incidents/README.md).

### baseline-topology-01

**Recorded**, on 2026-09-16, from the disposable kind cluster of `deploy/kind/` running the
`minimal` overlay. `payloads/` is what the two feeders were handed; `events.jsonl` is what they
emitted into a graph that had never seen them. The run is written up in
[`docs/benchmarks/live-run-2026-09-16.md`](../docs/benchmarks/live-run-2026-09-16.md), which
also lists every way it differs from the hand-authored fixture it replaced.

A small shop, stable for the whole clock window: everything is valid from the first observation
and nothing changes, so any query at any instant after that returns the same graph. Two sources:

- `k8s:kind` (ordering `per_source_sequence`, window 60s) — six workloads in namespace `shop`
  (four Deployments, one StatefulSet for `redis`, one Deployment for `payments-db`), the
  cluster and **both** node pools with `runs-on` edges, two ConfigMaps (`checkout-config` and
  the `kube-root-ca.crt` every namespace carries, each with a real value hash) and a Secret
  (version only, never a value) with `depends-on` edges, six Services and an Ingress with
  `exposed-via` edges, two owner nodes from the `team` label with `owned-by` edges, identity
  claims, and the periodic checkpoints of a live watch.
- `otel:kind` (ordering `none`, window 300s) — four service nodes, three third-party nodes
  (`payments-db` on postgresql, `stripe` as an external HTTP host, `redis` as a shared cache
  called by three services), eight `calls` edges in weight classes 2 and 1, identity claims and
  one checkpoint per closed window.

Certain rule C2 merges the four annotated workloads with their telemetry services, so
`checkout`, `payments`, `inventory` and `storefront` each resolve to one entity of type
`SERVICE` with `WORKLOAD` as a facet. `redis` and `payments-db` deliberately do **not** merge:
`deploy/kind/otel-demo/minimal/redis.yaml` says why — they are identified by their address in
telemetry, not by a service name — so each appears twice, once as the workload Kubernetes
reports and once as the third party the callers see.

`redis` is the modest hub: the `redis-3hop-cap5` query is expected to truncate at
`per_hop_cap: 5` and say so (US1 scenario 3).

Two facts about this fixture are properties of the cluster rather than of the graph, and moved
when it was recorded: the cluster is named `sre-agent-demo`, not `shop-prod`, and each node
pool holds one node rather than six. The node-pool pointer selector is
`v1/nodes?labelSelector=sre.node_pool%3Dgeneral` — the key the feeder reads by default — where
the hand-authored fixture used `node-pool`. `cluster.yaml` sets both labels so either
configuration finds the pool.

`rejected.jsonl` is the one part that is not a recording and cannot be: a feeder never emits a
telemetry payload, so no cluster can produce the event SC-009 needs. It is the hand-authored
extension described below, re-pointed at the recorded source and clock.

### late-arriving-fact-01

Four services observed from 13:00. A fifth service `inventory-search` and the
`checkout → inventory-search` call were **true from 14:20** but **first observed at 15:00**.
The two manifest queries differ only in observed time, and must differ in result: the late
fact is present as known now and absent as known at 14:32.

### retraction-with-edges-01

`legacy-cart` is valid from 13:00 with two live `calls` edges. A single `retract_node` with
`valid_end = 14:10` observed at 14:11 must cascade: both edges close at the same valid end and
the cascaded edge versions carry `closed_as_consequence_of`. **No `retract_edge` event is
emitted** — the cascade is the behaviour under test.

### rollout-regression-01

The scenario the product thesis is stated in (US2, SC-005). It starts as a copy of
`baseline-topology-01` — same events, same ids, same observed times, with one field changed:
the `payments` deployment is at `sre.k8s.revision: "6"` so that the culprit rollout can take it
to 7. On top of that baseline it adds an analytics stack that is deliberately out of reach
(`reporting` on its own node pool, four hops from `checkout`) and a window of change:

| at | change | why it is there |
|---|---|---|
| 13:10 | `inventory` rolled out to revision 8 (0.9.8) | a real decoy: one hop away, but 82 minutes stale |
| 13:40 | `reporting` rolled out to revision 3 | four hops away — must be **excluded**, not ranked low |
| 14:15 | `storefront` scaled from 3 to 6 replicas | one hop away and recent: ranks second |
| 14:20 | **`payments` rolled out to revision 7 (1.4.1)** | the culprit |
| 14:20 | `payments → payments-db` re-weighted from class 3 to class 4 | the symptom, and the edge-absorb regression |
| 14:22 | `checkout-worker` rolled out to revision 12 | target no source has described: **unattached** |

Two further facts in this fixture belong to US4 and US5 rather than to the ranking, and neither
touches a change node, so the expected order above is unaffected:

| what | why it is there |
|---|---|
| `storefront → payments` calls edge at weight class 1, from 13:00 | US4 scenario 2: storefront reaches payments directly *and* through checkout (classes 4 and 3), so `impact` must report it once, at hop 1, with the **two-hop** route as the heaviest — "heaviest" being the path whose weakest link is loudest — and one alternative counted. |
| `payments` renamed to `payments-v2` at 14:00 — display name, `service.name` and the telemetry selectors together, under the same subject ref | US5 scenario 2: `payments-pointers-1300` returns the old selectors and `payments-pointers-1432` the new ones. The subject ref does not move because the thing did not change, only what it is called; the rename does add `display_name` and `service.name` to the properties the diff reports as moved on payments. |

`ground_truth` names `k8s.change=shop/payments@rev7` with `expected_top_k: 3`, which is what
`fixture verify --report` measures. The expected ranking, the arithmetic behind it and the
reason the scaling decoy sits at 14:15 rather than 14:25 are in
[`docs/schema/ranking.md`](../docs/schema/ranking.md); the manifest's description repeats the
expected order so a reviewer can check the golden against it without running anything.

One thing this fixture deliberately does **not** do: no node or edge enters or leaves
`checkout`'s neighbourhood in the window, so `nodes_added`, `nodes_removed`, `edges_added` and
`edges_removed` are all empty and US2 scenario 2 ("lists no nodes as added or removed that were
in fact stable") is a plain assertion. Those paths are covered by unit tests in
`internal/query/diff_test.go`.

The re-weight of `payments → payments-db` at 14:20 is there on purpose, and it is the one event
in these fixtures that exists to pin a fixed bug. The edge was first asserted at 13:03, before
the claim that merges the `payments` workload with the `payments` service, so it was written
under the pre-merge OpenTelemetry id; the re-assertion at 14:20 resolves to the survivor. The
projector used to leave the first row where it was, and because the exclusion constraint keys
on the raw `src_id`/`dst_id` it saw two unrelated series and refused neither — two versions of
one relationship, both valid at 14:32, in two different weight classes. A merge now absorbs
edges the way it already absorbed node versions (research §4), so the goldens carry exactly one
`edges_changed` entry, `weight_class` 3 → 4, and `projector.CheckInvariants` asserts after every
replay that no two current edge versions share a canonical `(src, dst, type)` and a valid
instant.

### rollout-regression-02

**Recorded.** The same question as `rollout-regression-01`, asked of a cluster instead of an
author: `deploy/kind/scenarios/rollout.sh` took `shop/payments` from `nginx:1.31.5-alpine-slim`
to `1.31.6-alpine-slim` at 09:57:45Z, four minutes into a nineteen-minute run with both feeders
recording, and `checkout-diff` asks what changed in checkout's two-hop neighbourhood across it.

There are two ranked candidates and they are the same event seen twice, which is the interesting
part:

| rank | source | change | why it sits there |
|---|---|---|---|
| 1 | `k8s:kind` | `k8s.change=shop/payments@rev2`, a ROLLOUT | valid at the instant the API server recorded revision 2 |
| 2 | `otel:kind` | `service.version 1.4.0 → 1.5.0 observed in traces` | valid at the start of the aggregation window that first saw the new version, so ~2m45s older |

Both are one hop from checkout and carry the same topological and traffic components; the
temporal term is the whole difference. `ground_truth` names the Kubernetes one with
`expected_top_k: 3`, and `fixture verify --report` measures it at rank 1 of 2.

What this fixture does **not** have, and `rollout-regression-01` does: decoys. Nothing else on
the cluster changed, so there is no four-hop rollout to exclude, no stale decoy to rank below
the culprit and no unattached change. The two fixtures are complements — one tests the ordering
against a designed field, the other tests that a real rollout becomes a ranked candidate at all.

Its shuffle step used to be skipped, for the Kubernetes feeder's arrival-order bug, and it is
skipped no longer: the feeder now takes one valid-time stamp for the whole initial list, and the
fixture passes six seeded permutations. `shuffleExceptions` in
`internal/query/fixture_golden_test.go` is empty, and the history of what lived there is written
out beside it.

### ambiguous-identity-01

The fixture US6 is argued from, and the only one that ships `human_decisions`. Two sources name
one shop three times and disagree about two of them:

| pair | what the rules do | why |
|---|---|---|
| `otel.service.name=payments` / `k8s.deployment=shop/payments` | **C2 merges it** | the workload declares the service name in an annotation, and both sides agree on namespace and environment |
| `otel.service.name=checkout` / `k8s.deployment=shop/checkout-svc` | **P1 suggests it, 0.70** | the label declares a name but no environment, so no certain rule may fire; `checkout-svc` normalizes to `checkout` |
| `otel.service.name=notifications` / `k8s.deployment=ops/notifications-svc` | **P1 suggests it, 0.70 — and is wrong** | the names normalize alike but the workload is in another Kubernetes namespace; `ground_truth` says `same: false`, which is what the calibration report measures |

Then the four US6 scenarios, in order:

| at | what happens | scenario |
|---|---|---|
| 14:10 | `checkout` and `checkout-svc` stay two nodes, listed as a `pending` suggestion | 1 |
| 14:35 | `sre-agent-dev\|alice` confirms the pair; both names now return one entity and the audit names her as the deciding evidence | 2 |
| 14:50 | spans arrive for `checkout-legacy` carrying `k8s.deployment.name=checkout-svc`; **C3 would merge them**, but the entity is pinned by Alice's decision, so the disagreement is filed as a `conflict` and not applied | 3 |
| 15:00 | Alice splits the workload back out; as known at 14:40 the graph shows the merged view, as known at 15:05 the separate one | 4 |

`ground_truth` labels the pairs for `fixture verify --report`, which reports certain-rule
precision (1.00), the probable-score Brier (0.29 over two pairs — the honest number for a
two-sample fixture), zero automated merges from probable rules (SC-007), and that both human
decisions survived the replay carrying an authenticated principal (SC-011).

Recorded and verified by `internal/resolution/fixture_record_test.go`:

```
SRE_AGENT_RECORD=1 go test ./internal/resolution -run TestRecordResolutionFixture
```

The rules, scores, precedence and the audit's observed-time approximation are published in
`docs/schema/resolution.md`.

### config-change-01

**Recorded.** `deploy/kind/scenarios/config-change.sh` changed one key of
`shop/checkout-config` — `CART_TTL_SECONDS` 900 → 1800 — at 10:19:08Z, and deliberately did not
restart anything. The Kubernetes feeder keys the change on the ConfigMap's resourceVersion
(572 → 6635) and produces, in the same batch:

- a new `CONFIG` version of `checkout-config` whose `sre.config.value_hash` moved from
  `sha256:0076ad39…` to `sha256:7816f08d…`;
- a `CONFIG_CHANGE` change node targeting **both** `shop/checkout-config` and `shop/checkout` —
  the workload consumes it with `envFrom`, so the blast radius of the edit reaches the service;
- no rollout, no scaling, and no change to any workload's revision or replica count.

`checkout-diff` is the query it exists for, and it is clean: one ranked change, one changed
node with two property deltas, nothing added or removed. Its `t1` is 10:15:00Z rather than
`clock.start` on purpose — the clock begins a second before the first payload, and a diff from
there reports every Kubernetes fact as newly appeared and buries the one thing that changed.

`checkout-impact` is the other half of the scenario: the `CONFIG_CHANGE` node targets both
`shop/checkout-config` and `shop/checkout`, and the impact golden is where the blast radius of
that ConfigMap edit is asserted — `checkout-config` among checkout's dependencies, `storefront`
among its dependents. The change node itself is not in the impact answer and should not be:
change nodes are stored over `[t, t+1µs)` and so are valid at no instant a caller would name,
which is why `diff` is the query that finds them.

### feeder-gap-01

**Recorded, and it records a bug.** The Kubernetes feeder was stopped at 11:50:10Z,
`svc/checkout` was deleted at 11:50:11Z, the feeder reconnected at 12:00:16Z with the Service
still absent, and the Service was restored at 12:01:08Z. The OpenTelemetry feeder ran
throughout, so the graph was never wholly blind and the gap belongs to one source.

What the reconnection should have produced, and did not:

- **a retraction** of `k8s.service=shop/checkout` and its `exposed-via` edge, with a valid end
  inside the gap and an observed time of 12:00:16Z. The feeder lists the namespace and upserts
  what is there; it never diffs that list against what it previously asserted, so the Service
  and its edge are still valid `[10:43:42Z, ∞)` in the recorded graph;
- **a gap**: the reconnection checkpoint carries no `gap_before`, and `extent` reports `GAPS 0`
  across a ten-minute hole, because the feeder's resync counter is process-local and a restarted
  process cannot know it was ever away.

So the goldens are a regression baseline, not a statement of correct behaviour, and the manifest
says so at the top. When the feeder reconciles on reconnect and reports its gap, these goldens
must change — and that pull-request diff is the evidence the fix worked. T067 is left open for
the same reason. The detail, including the code, is in
[`docs/benchmarks/live-run-2026-09-16.md`](../docs/benchmarks/live-run-2026-09-16.md) §8.

Its shuffle step was the last one skipped and is skipped no longer. It reproduced two bugs, not
one: the Kubernetes feeder's arrival-order stamping, and — underneath it — a retraction
delivered *before* a later re-assertion of the same subject losing the interval the
re-assertion opened. This recording has a retraction of `inventory->redis` 296 s before a
re-assertion of it, and one of `k8s.service=shop/checkout` 52 s before its own, so both feeders
were made to declare windows narrow enough to forbid the swap. The swap is survived now: a
coalesced segment remembers the first instant each source restated it and a retraction splits
rather than truncates (`internal/projector/segments.go`, pinned by
`TestRetractionAndReassertionAreOrderIndependent` and
`TestRetractReassertRetractPermutations`). `k8s.DefaultReorderingWindow` is back to 60 s,
`otel.DeclaredReorderingWindow` stays 30 s because that is what its flush reasoning supports,
and the fixture passes six seeded permutations at the wider window.

## Hand-authored versus recorded

| | hand-authored | recorded |
|---|---|---|
| where the events come from | written by a human against the published schema | produced by running `payloads/` through the real feeders |
| `payloads/` | absent | present, and is the source of truth |
| `manifest.yaml` | carries `hand_authored: true` | carries `hand_authored: false` |
| regenerating `events.jsonl` | never — it is the artifact | replay `payloads/` through the feeders |

Both kinds are here, and both are meant to be. Hand-authored came first: the event schema is
the published contract (constitution IX), so writing events by hand against it is the cheapest
way to pin down expected behaviour before any feeder exists, and it keeps the projector's tests
independent of feeder bugs. `late-arriving-fact-01`, `retraction-with-edges-01`,
`rollout-regression-01` and `ambiguous-identity-01` are still hand-authored, each because it
states a case no cluster will produce on demand — a fact that arrives forty minutes late, a
four-hop decoy, two services that might be one.

The rest are recordings, because feeder conformance needs recorded payloads (constitution VIII:
"synthetic-only test data is insufficient for a connector to be marked stable"):
`baseline-topology-01`, `rollout-regression-02`, `config-change-01` and `feeder-gap-01` were
all recorded on 2026-09-16 from the kind cluster of `deploy/kind/`, described in
[`docs/benchmarks/live-run-2026-09-16.md`](../docs/benchmarks/live-run-2026-09-16.md).

One thing `fixture record` does **not** do, for either kind: replay `payloads/`. It reads
`events.jsonl` and writes `golden/`, and nothing else — so `payloads/` is never the input to a
golden. What holds a recording's payloads to account is the feeders' own conformance tests,
which replay them and require the events beside them byte for byte:
`internal/feeders/otel/feeder_test.go` replays `baseline-topology-01/payloads/traces/` directly
out of this directory, and `internal/feeders/k8s/testdata/kind-shop/` is the same recording as
that feeder's corpus. Re-recording a fixture therefore means re-running the cluster, not
re-running a command.

## Recording the goldens

Goldens are recorded for every query kind the build can answer; the rest are named as skipped
rather than written empty.

```sh
bin/sre-agent fixture record fixtures/baseline-topology-01   # writes golden/ and golden/pinned/
bin/sre-agent fixture verify fixtures/*/                     # replay + double-deliver + shuffle
bin/sre-agent fixture verify fixtures/*/ --report            # + ranking metrics (SC-005)
bin/sre-agent fixture verify fixtures/*/ --skip-goldens      # before goldens exist
make verify                                                  # the whole set, the way CI runs it

# Same recording, without a PostgreSQL of your own:
SRE_AGENT_RECORD=1 go test ./internal/query -run TestRecordShippedFixtures
```

One file per manifest query, named `<kind>.<name>.json`: `subgraph.checkout-2hop-1432.json`,
`impact.payments-impact.json`, `pointers.payments-pointers-1300.json`,
`extent.extent.json`, `diff.checkout-diff.json`, `history.legacy-cart-history.json`. The same
queries with observed time pinned to `clock.end` go in `golden/pinned/` (US7), and that pinned
set is **required**: a fixture with no `golden/pinned/`, or a golden with no pinned twin, fails
the replay step. Recorded goldens are reviewed as a
diff in the pull request that produces them — that review is the actual assertion, so a golden
that looks wrong must be fixed in the code, not accepted.

## Conventions used by these fixtures

**`events.jsonl` holds accepted events only.** Each line is the canonical protobuf-JSON
encoding of one `EventEnvelope` ([`api/sreagent/graph/v1/graph.proto`](../api/sreagent/graph/v1/graph.proto)):
lowerCamelCase field names, enum **value names** (`"SERVICE"`, `"CALLS"`, `"METRIC"`), sorted
keys, no insignificant whitespace, `emitDefaults=false`. Two fields on each line are **not** in
`EventEnvelope`; the log adds them and a loader must strip them before unmarshalling into the
proto message:

- `observedAt` — the observed-time lower bound assigned by the log at append. Replay uses this
  value, never the replay clock (FR-023). Lines are ordered by it.
- `appendedSeq` — physical log order, starting at 1 and equal to the line number.

Other conventions:

- **Event ids**: `<source_id>:<kind>:<stable-suffix>`, e.g.
  `k8s:demo:deploy:shop/checkout@rv1001`, `otel:demo:edge:checkout->payments@w1300`.
  `idempotencyKey` always equals `eventId`. `@rv<N>` is a Kubernetes resourceVersion, `@w<HHMM>`
  an OTel aggregation window start.
- **`sourceSeq` is a JSON string** (`"1001"`), because the proto field is `int64` and the
  canonical proto-JSON mapping encodes 64-bit integers as strings. `weightClass` is `uint32`
  and stays a JSON number. Only `k8s:demo` declares a sequence; it is the cluster-wide
  resourceVersion and is monotonic across the stream.
- **Timestamps** are RFC 3339 UTC, whole seconds, `Z` suffix. The format contract says
  "with nanoseconds"; protobuf-JSON omits a zero nanosecond part, and these fixtures follow
  protobuf-JSON so that a re-record produces the same bytes.
- **Valid time lives in the body** (`validAt`, `validEnd`), never at the envelope level.
- **Property names** follow OpenTelemetry semantic conventions where one exists
  (`service.name`, `service.namespace`, `service.version`, `deployment.environment.name`,
  `k8s.namespace.name`, `k8s.deployment.name`, `k8s.cluster.name`, `db.system`,
  `server.address`, `server.port`, `cloud.region`). Everything else uses the project namespace
  `sre.*` (FR-007): `sre.k8s.revision`, `sre.k8s.resource_version`, `sre.k8s.replicas`,
  `sre.k8s.node_pool`, `sre.k8s.node_count`, `sre.k8s.reference`, `sre.k8s.claim_key`,
  `sre.k8s.claim_kind`, `sre.config.kind`, `sre.config.value_hash`, `sre.config.version`,
  `sre.owner.kind`, `sre.owner.source_label`, `sre.window.seconds`.
- **Ref namespaces** are identifier namespaces, not attribute names:
  `otel.service.name`, `server.address`, `k8s.deployment` (`<ns>/<name>`), `k8s.cluster`,
  `k8s.nodepool`, `k8s.service`, `k8s.ingress`, `k8s.configmap`, `k8s.secret`,
  `app.kubernetes.io/name`, `owner.team`.
- **Join keys on METRIC pointers**: `joinKeys: {"version": "service.version"}` is what the real
  OpenTelemetry feeder emits on every service pointer
  (`internal/feeders/otel.serviceJoinKeys`), and it is what makes `errors_by_version` — "did the
  new revision carry the errors?" — a question the telemetry algebra can form at all (ADR-0005
  D6). A pointer without it is not a pointer with a weaker answer; it is a pointer the question
  cannot be asked of.

  `rollout-regression-01`, every fixture under `fixtures/incidents/`, and **every `gcp-*` fixture**
  carry it. Nothing else here does, and the reason differs by kind. The hand-authored ones
  (`late-arriving-fact-01`, `retraction-with-edges-01`) predate the field and are not asked a
  version question by any fixture, so adding it would churn their goldens for nothing; the
  recorded ones (`baseline-topology-01`, `rollout-regression-02`, `config-change-01`,
  `feeder-gap-01`) were recorded on 2026-09-16, before the field existed, and re-recording them
  means re-running the cluster rather than re-running a command. They will carry it the next
  time that happens. `ambiguous-identity-01` has no METRIC pointers at all, which is why the
  world of `merged-alias-01`, derived from it, is sixteen terms rather than a hundred and forty.

  The TRACE pointer beside the metric one deliberately does **not** carry join keys in these
  hand-authored fixtures, although the live feeder puts them on both: the version breakdown is a
  metric question, and a second copy of it behind a trace selector would mint a second
  `errors_by_version` term per pointer per window with nothing new in it.

  The `gcp-*` fixtures carry them because the GCP feeder mints them, in that vocabulary's own
  spelling: `{"version": "resource.labels.revision_name", "workload": "resource.labels.service_name"}`
  on every `cloud_run_revision` pointer, metric and log, and `{"host": "resource.labels.database_id"}`
  on a `cloudsql_database` one. A Cloud Run **revision** is the deployed version, which is why the
  version role lands on a resource label here and on `service.version` in OTel — the role is the
  same, the spelling is the backend's.

  They were re-recorded on 2026-09-22, and the size of what that unlocked is the argument for the
  field: `gcp-rollout-traffic-shift-01`'s world layer went from **0 `errors_by_version` terms to
  22**, and `gcp-cross-source-merge-01`'s from 8 (its OTel half alone) to 40. A fixture whose entire
  subject is a traffic shift between two revisions could not previously be asked whether the new
  revision carried the errors. Nothing in it was wrong; the question had no pointer to stand on.
- **Pointer vocabularies**: `otel-semconv/1.30` for METRIC, LOG and TRACE selectors;
  `k8s-resource/v1` for SOURCE_LINK paths. The `gcp-*` fixtures use `gcp-monitoring-filter/v3`,
  `gcp-logging-query/v2` and `gcp-console-url/v1`; which join roles each of those can express is
  tabulated in [`docs/schema/pointers.md`](../docs/schema/pointers.md#join-keys-which-field-plays-which-role).
- **No telemetry payloads.** No sample, log line or span ever appears in `props` — except in
  the one deliberately invalid event described next.

## Extension: `rejected.jsonl`

`contracts/fixture-format.md` says `events.jsonl` holds accepted events and lists rejected
events in `manifest.yaml` under `expect_rejected`, but assumes they live in `payloads/`. A
hand-authored fixture has no payloads, so there would be nowhere to put an event that must be
rejected. These fixtures therefore add:

- `rejected.jsonl` — events that MUST be rejected, one `EventEnvelope` per line, in the same
  canonical encoding **without** `observedAt` and `appendedSeq` (the log never accepted them,
  so neither was ever assigned). `sourceObservedAt` carries when the source produced the event.
- `manifest.yaml` keys `events:` and `rejected_events:` naming the two files.

`baseline-topology-01` ships exactly one: `otel:demo:bad-span-props-1`, an `upsert_node` whose
props carry a numeric array `latency_samples`, expected to be rejected with `reason_code:
telemetry_payload` and a `reason_detail` naming the offending field (FR-009, FR-024, SC-009).
The verifier should submit these after the accepted stream, assert `REJECTED` with the stated
reason code, and assert the graph is unchanged.

Two further manifest keys are used that the format contract's example does not show, both
drawn straight from the query contract in the proto: `direction` and `per_hop_cap` on subgraph
queries, and `observed_at` on a query that pins observed time. A query of `kind: extent` and a
query of `kind: history` are likewise used; the contract's golden file list does not name
`extent.*.json`.

> **To back-port into the spec**: `rejected.jsonl`, the `events:` / `rejected_events:` /
> `hand_authored:` manifest keys, the `direction` / `per_hop_cap` / `observed_at` query keys,
> the `extent` golden file name, the `sre.*` property namespace, the ref-namespace list, and
> the whole-second timestamp clarification all belong in
> `contracts/fixture-format.md` and `docs/schema/`. They are documented here first so that the
> fixtures could land; the contract is the place they have to end up.

## Known gaps in these fixtures

- **US5 scenario 2** (pointers use the *old* name when asked before a rename) is covered by
  `rollout-regression-01`, which renames `payments` at 14:00. `baseline-topology-01` is
  deliberately stable, so its `payments-pointers-end` and `payments-pointers-mid` must return
  **identical** results — a pointer that drifts between two instants in a graph that did not
  move is a bug, and that is the assertion those two exist for.
- **US5 scenario 1** (a metric, a log and a trace selector on one node) is covered by
  `baseline-topology-01`'s `payments`, which carries all three plus a `SOURCE_LINK` — but only
  since pointers became a **union** across the sources asserting a version rather than the
  primary assertion's list (research §4, 2026-09-16). Before that a merged workload/service kept
  one feeder's pointers and dropped the other's, and the scenario was unanswerable on any merged
  entity. Every fixture's goldens were re-recorded for it.
- **FR-011 unknown boundaries** (`validFromUnknown`) are not exercised. Every fact here asserts
  an explicit valid start.
- **Entity resolution**: `baseline-topology-01` does exercise certain rule C2 (research §10) —
  every workload claims `otel.service.name` from its `resource.opentelemetry.io/service.name`
  annotation, so the Kubernetes workload and the OTel service are one entity after ingestion.
  It does *not* exercise probable rules or human decisions; that is `ambiguous-identity-01`.


## `announced-fact-01` (hand-authored, feature 002 Phase 3)

A vendor maintenance notice observed on 20 September for a window on 2 October, then withdrawn on 25 September as a correction of the same change. Pins the announced-fact semantics of ADR-0005 D5 / spec 003 FR-062–068: excluded as a cause before its valid start, listed in a forward window, and the original announcement recoverable by pinning observed time before the withdrawal.
