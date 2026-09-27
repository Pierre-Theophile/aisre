# Contract — The Datadog feeder

**Date**: 2026-09-27 · **Requirements**: sections A, C, E, G, H, J; FR-040f, FR-040g ·
**Reference**: [`specs/003-gcp-integration/contracts/gcp-feeder.md`](../../003-gcp-integration/contracts/gcp-feeder.md)

What the feeder half emits, per capability. It emits events only, through the published event log;
it never reads the graph to decide what to emit (constitution III — the reason FR-040f was
corrected). Mappings are in [data-model.md](../data-model.md); this page fixes behaviour: cadence,
identifiers, what a partial read does, and what the checkpoint says.

---

## 1. Capabilities and the checkpoint

| capability | default | emits |
|---|---|---|
| `logs` | on | SERVICE nodes for watched log sources, log pointers with the version-stamp verdict, log-observed rollouts |
| `monitors` | on | ALERT nodes, `alert.transition` events, WATCHES edges, monitor pointers |
| `tags` | on | OWNER nodes, `owned-by` edges, identity claims — from allowlisted tags only (FR-066) |
| `apm_topology` | **off** | the spec's section B; nothing while off |
| `changes` | **off** | the spec's section D; nothing while off |

Every checkpoint records: the capabilities in force; the site; the watched environments and log
sources; the version convention list and its version, with overrides; the poll cadences; the extent
actually covered and any gap; and `operator_asserted` where the startup gate could not verify the
credential. A disabled capability is **stated**, so "no alerts" never reads as "we were not looking".

---

## 2. Watched log sources (FR-040f)

Configured as `<env>/<service>` pairs, with an optional index and an optional per-service version
attribute override. For each, every discovery interval (default one hour):

1. Assert the SERVICE node `datadog.service=<env>/<service>` with its `datadog.log_service` claim and
   allowlisted identifier claims — **every time, whatever else the graph holds**. Resolution (C9)
   merges it with another feeder's node for the same service and environment.
2. Run version-stamp discovery ([version-stamping.md §3](./version-stamping.md)) and assert the log
   pointer with its join keys and verdict. The pointer changes only when the verdict does, so an
   unchanged verdict is an unchanged event id and a no-op.
3. Look for new stamped values (§4).

A log source with no lines in the window is stated in the checkpoint; its node is not retracted on
one quiet window, only under the published silence rule.

---

## 3. Monitors and transitions

- **Poll** every 15–30 s (default 20 s): `GET /api/v1/monitor?group_states=all`, paged, filtered by
  the configured tags (FR-025c).
- **Transitions** are derived by comparing each group's state with the previous poll, and **dated
  from Datadog's stated instants** — `last_triggered_ts` for a move into alert or warn,
  `last_resolved_ts` into OK, `last_nodata_ts` into no data — never from the poll instant (FR-020).
  Where no stated instant corresponds, the transition is emitted with an unknown valid start rather
  than the poll instant.
- **Key**: the published 4-tuple `(source, monitor id, group, transition instant)`; the same
  transition learned by a poll and by a doorbell-triggered poll is one event (FR-025a).
- **Every polled history is `sampled`** at the poll interval (ADR-0009 item 3). A transition that
  opened and closed between two polls is invisible to polling; the marker says so.
- **Flapping and no-data** are recorded and stated as non-triggering (FR-025c); **recoveries are
  emitted** and kept as evidence.
- **Grouped monitors**: one ALERT per alerting group; the monitor itself is never reported alerting
  because one group is (FR-022).
- **Watched entities** are resolved from the monitor's query and tags; unresolved ones are recorded
  unattached and attached on arrival (ADR-0009 item 5).
- **Monitor definitions** are asserted on change only; a deleted monitor or one leaving scope is
  retracted, never on a partial read (FR-012).

### 3.1 The doorbell (FR-025b)

An HTTP endpoint requiring the configured shared-secret header, rate-limited by a token bucket. A
valid ring enqueues "poll now" and nothing else: the body is **never parsed**. A missing or wrong
secret is dropped and counted. Implementation is the shared `pkg/feeder/doorbell` (Gap G6).

---

## 4. Log-observed rollouts (FR-040g)

Each discovery interval, for a service with an accepted stamp: aggregate the stamp's values over the
interval; for each value not seen before in this service's history, find its first indexed line
(`search`, `sort=timestamp`, limit 1) and emit the ROLLOUT of [data-model.md §6](../data-model.md).

- **Identity**: `datadog.change=<env>/<service>@<value>@<first-seen>`, so a re-run of the same
  interval re-derives the same id and is a no-op; the first-seen instant is Datadog's, not the
  poll's.
- **Only `deploy.commit_sha` and `deploy.image` values** produce a change; release-form and
  unreferenceable values are counted in the checkpoint.
- **Alternation**: a value already seen does not produce a second change when it reappears; two
  values live in one interval both produce their first-sight change, and the overlap is recorded.
- **Restarts need no memory.** The id is built from Datadog-stated facts only — the value and the
  instant of its first indexed line — so a restarted feeder asking the same question gets the same
  first line and re-sends an id already sent, which is a no-op. In-process memory of "seen" values
  only saves calls. (003's T184 is the cautionary tale: ids built from what a run remembers are not
  restart-safe.)
- **The first-seen search looks back a published horizon** (default 7 days, within retention). A
  value whose first line is older than the horizon was deployed before the connector could see it:
  no change is emitted, and the checkpoint counts it. Anything already emitted for it keeps its id,
  so the horizon moving forward never produces a second, later-dated rollout.

---

## 5. Budget and failure

- The budget is a share of the remaining quota per `X-RateLimit-Name` bucket, with the published
  human reserve left untouched and the deferral order published: transitions first, then monitor
  definitions, then discovery, then rollout detection (FR-082).
- A 429 waits at least the stated reset and resumes where it stopped (FR-083).
- A partial read — a page failure, `meta.status = timeout`, a quota stop — emits no retraction for
  what was unread and declares the gap (FR-012, FR-085).
- Usage is reported per area and per bucket, feeder and backend separately (FR-084, FR-084a).
