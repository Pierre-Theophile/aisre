# Data model — Datadog Connector

**Date**: 2026-09-27 · **Plan**: [plan.md](./plan.md) · **Research**: [research.md](./research.md)

What a Datadog object becomes in the graph, and what a digest carries back. Every row maps something
Datadog **states**, or something a service **stamps on its own logs**, to something the graph holds.
Nothing is derived from a string nobody stated; where a value is absent the row says what happens
instead. Rows for the two off-by-default capabilities (`apm_topology`, `changes`) are the spec's data
mapping unchanged and are not repeated here.

---

## 1. Service node for a watched log source (FR-040f)

One per configured log source: a Datadog `service` in one `env` of one organisation.

| field | source | rule |
|---|---|---|
| ref | `datadog.service` = `<env>/<service>` | local namespace, as `gcp.*` are; asserted for **every** watched source, never conditional on graph state |
| correlation key `datadog.log_service` | the log `service` value, with `deployment.environment.name` from the log `env` | what C9 reads (§4). A correlation, not an identity claim: this source states the same name for its production and staging nodes. Not `otel.service.name`: C1 would merge across environments on it |
| other claims | identifiers the logs state on most lines — e.g. the platform's own agent or workload id | each on the tag allowlist (FR-066); none invented |
| type | `SERVICE` | |
| valid start | unknown, unless Datadog states a first-seen instant | FR-017 |
| pointers | the log pointer (§5), the monitor pointers of monitors that watch it | additive on a merged entity (FR-039) |
| version join key | the discovered attribute (§3) on the log pointer | absent when discovery found none; the verdict is recorded either way |

A service another feeder also reports (a Cloud Run service with the same OpenTelemetry name and
environment) is **merged by C9**, and from then on the log pointer, the monitor pointers and the
log-observed rollouts belong to one entity.

---

## 2. Alert and alert transition (US2)

Unchanged from the spec's data mapping, on shipped types: `ALERT` node, `WATCHES` edge,
`alert.transition` with the published 4-tuple key.

| field | source | rule |
|---|---|---|
| ref | `datadog.monitor` = monitor id; a grouped monitor adds the group | one alert per alerting group (FR-022) |
| transition valid time | the instant Datadog reports for the transition | **never** the poll instant (FR-020) |
| idempotency key | `(datadog source, monitor id, group, transition instant)` | poll and doorbell deliver one event (FR-025a) |
| `sampled`, `sampled_interval_seconds` | the poll interval | set on every polled history (ADR-0009 item 3) |
| state | `ok`, `warn`, `alert`, `no_data` | the published vocabulary (`internal/log/alert.go:70-77`) |
| suppression | flapping and no-data | recorded in the history; stated as not triggering (FR-025c) |
| watched entities | the monitor's query and tags, resolved | unresolvable ⇒ unattached, auto-attached later (ADR-0009 item 5) |
| pointers | the monitor query (`datadog-monitor/v1`), a link to the monitor | body of the notification message dropped (FR-074) |

---

## 3. Version stamp discovery verdict (FR-040c)

The outcome of asking "which attribute carries this service's deployed version?", recorded **on the
log pointer and in the checkpoint**. It is configuration-derived metadata about where to look, not
telemetry: it holds attribute names and shares, never a log line or a count of errors.

| field | meaning |
|---|---|
| `attribute` | the accepted attribute, e.g. `version`; empty when none qualified |
| `source` | `discovered` or `operator` (a per-service override wins and is recorded as such) |
| `window` | the discovery window the shares were measured over |
| `candidates[]` | every convention tried, in order: `name`, `line_share`, `error_line_share`, `accepted`, `reason` |
| `value_forms` | for the accepted attribute: the share of its values that normalise to `deploy.commit_sha`, `deploy.image`, `deploy.release`, or nothing |
| `conventions_version` | the version of the published list in force |

**Acceptance** is by share, not by stability: a candidate qualifies when it is present on at least
the published share of the service's lines **and** of its error-level lines (research §3.1). Whether
the value changed during the window is not a criterion. The audit's case — `version` on 255 of 37 M
lines, all start-up lines of an SDK — is rejected with its share stated.

---

## 4. Rule C9 — a Datadog log service is an OpenTelemetry service (FR-059, generalised)

| side A | side B | fires when | certain |
|---|---|---|---|
| correlation `datadog.log_service` = *n*, attr `deployment.environment.name` = *e* | `otel.service.name` = *n*, attr `deployment.environment.name` = *e* (observed or declared) | names equal after the published normalisation **and** both environments stated and equal; where both sides state a Kubernetes namespace or cluster, those agree too | yes |

It runs from both sides — when the correlation is stored and when the claim is — because it pairs two
kinds of event (`Rule.CrossKind`). It never fires on an unstated environment. The pair with equal names and different environments is
a fixture of its own and must stay apart. `resolve why` names C9, the two claims and the
environment.

---

## 5. Log pointer

| field | value |
|---|---|
| kind | `LOG` |
| backend | `datadog` |
| vocabulary | `datadog-logs/v1` (research §2.4) |
| selector | a Datadog log-search query pinning `service` and `env`, and the index where one is configured; **no** organisation, site host or credential (FR-037) |
| attributes | OTel: `service.name`, `deployment.environment.name` (FR-036) |
| `join_keys` | `version` → the discovered attribute (§3); `host` → the log `host` where the logs carry one; the other roles omitted where Datadog cannot express them |
| discovery verdict | §3, attached to the pointer |

---

## 6. Log-observed rollout (FR-040g)

| field | source | rule |
|---|---|---|
| kind | — | `ROLLOUT` |
| ref | `datadog.change` = `<env>/<service>@<version>@<first-seen>` | one per version value first seen, not per alternation |
| valid start | the first instant a line carrying the new version was indexed | **a bound**: the rollout happened at or before it. Marked with the published property `sre.change.valid_from_is_a_bound` (new in `pkg/feeder`, generalising 003's owner-local marker) |
| target | `changed-by` edge to the §1 service | |
| actor kind | unspecified unless the logs state one | never inferred from a name (FR-033a); not `UNKNOWN`, which would claim an actor was observed (corrected 2026-09-27) |
| correlation key | the value's `deploy.commit_sha` or `deploy.image`, with `deployment.environment.name` | what C8 reads. A `deploy.release` value is recorded but C8 excludes it, so such a rollout never merges — the stamping guide says so |
| not emitted when | the value normalises to no `deploy.*` form | stated in the checkpoint, never guessed |
| overlap | two values alternating in one window (canary, rolling deploy) | one change on first sight of each; the overlap interval is recorded on both |

**Merge.** When a deploy feeder observed the same rollout, C9 first makes the two services one
entity, and then C8 merges the two changes (same commit, same environment, shared target). The
merged change carries both instants and **the stated one is the rollout instant** (FR-040g); the
bound stays visible as a property. How the projector chooses the merged valid start is pinned by
`datadog-log-rollout-merge-01` (research §5, open item O3).

---

## 7. Digest additions

### 7.1 `VersionBreakdown` (investigation.v1, additive — Gap 1)

| field | new | rule |
|---|---|---|
| `version` | | the raw stamped value, pseudonymised consistently when recorded (FR-048b) |
| `errors`, `total`, `error_rate` | | from the aggregate API; `total` is the same filter without the error-level clause |
| `join_keys` | | `version` set to the raw value |
| `deploy_ref` | ✅ | the normalised `deploy.commit_sha` / `deploy.image` / `deploy.release` ref; absent when the value has none |
| `deploy_ref_absent_reason` | ✅ | why not: `ABBREVIATED_SHA`, `MUTABLE_TAG`, `BARE_DIGEST`, `NOT_A_STABLE_IDENTIFIER` |

### 7.2 `NO_DATA` for an unstamped pointer (Gap 2)

Answered by the **engine**, not a backend, when the pointer carries no `version` join key: the
outcome `NO_DATA`, coverage naming the absent source as "no version stamp on this pointer" and
listing each convention from the pointer's discovery verdict with its shares. It is a function of
the pointer alone, so it is identical live and recorded and needs no world entry.

---

## 8. Configuration recorded in every checkpoint and fixture manifest

Capabilities in force (FR-008a); site; environments and log sources watched (FR-007); the version
convention list and its version, with any per-service overrides; poll cadences; the budget, reserve
and window caps (section J); the sanitisation policy version (FR-072).
