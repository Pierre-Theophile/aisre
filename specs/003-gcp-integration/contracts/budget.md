# The Call Budget Contract

**Feature**: 003-gcp-integration | **Date**: 2026-09-20 | Requirements: FR-146–FR-154, SC-022

Dana owns the organisation's API quotas and its Cloud Logging and Cloud Monitoring bill, and needs
to know what this costs **before** it runs and **while** it runs. This document is what she
approves.

The governing fact, established in [`research.md`](../research.md) §7 and worth stating before
anything else: **`logging.entries.list` is limited to 60 calls per minute per project, the limit
is not hierarchical, and it is shared with every human querying Cloud Logging in that project.**
One call per second, project-wide, during an incident, against the same pool as the on-call who is
trying to read the logs. Every other rule here follows from that one.

---

## 1. Endpoint classes, not a global call count

Budgets are **per endpoint class** (FR-147), because GCP's quotas are per API and per project and
a global figure would let a cheap class starve a binding one.

| class | API | documented limit | scope | binding? |
|---|---|---|---|---|
| `logging.read` | `logging.entries.list` | **60 / min** | per **project**, non-hierarchical | **yes — the binding one** |
| `monitoring.query` | `monitoring.timeSeries.list` | **not publicly documented** | per project | **yes**, and its limit must be discovered at runtime (§4) |
| `run.read` | Cloud Run Admin reads | 3,000 / 60 s | per project **per region** | no, at this estate's scale |
| `sqladmin.read` | Cloud SQL Admin `list`/`get` | 500 / min | per user per region | no |
| `monitoring.policies` | `alertPolicies.list` | undocumented; low volume | per project | no |
| `compute.read`, `dns.read` | load balancers, Cloud DNS | — | per project | no — P3, cut first (FR-057) |
| `pubsub.pull` | the doorbell subscription | — | per subscription | no |

The two marked binding are the two the **investigation** competes for, which is the coincidence
that makes this contract necessary rather than prudent.

---

## 2. Share of remaining, and the human reserve

The budget is expressed as a **share of the remaining quota** per endpoint class (FR-148), with a
**published reserve left unspent** for the humans working the incident (FR-147), and the
integration **reduces its own allowance as the remaining quota falls**.

Published defaults for the binding classes:

| class | steady-state share | human reserve, never spent | behaviour as remaining falls |
|---|---|---|---|
| `logging.read` | **40%** (24 of 60 calls/min) | **≥ 30%** (18 calls/min) | the share decays toward the reserve floor; at the floor the integration **yields** |
| `monitoring.query` | **50%** of the discovered limit | **≥ 25%** | as above |

"Yields" is specified rather than implied (FR-149, SC-022): when the integration stops for quota
it **says so in its output as a typed reason**, distinct from stopping for lack of evidence. An
investigation that stopped because it ran out of quota and an investigation that stopped because
nothing was implicated are opposite conclusions, and a consumer that cannot tell them apart will
read the first as the second at exactly the wrong moment.

---

## 3. GCP reports no remaining quota, so FR-148's two branches are not symmetric

FR-148 says: *use the reported figure where GCP reports one; self-track where it does not; and say
in the usage report which figures were vendor-reported and which were self-tracked.* Research §7
establishes that for all four relevant APIs, **GCP reports no usable real-time remaining figure**:

- **no rate-limit response headers** — no `X-RateLimit-Remaining` or equivalent; the first signal
  that the quota is gone is `HTTP 429 / RESOURCE_EXHAUSTED`;
- the **Cloud Quotas API** and **Service Usage API** expose configured *limits*, not consumption;
- the only vendor-reported usage figure is Cloud Monitoring's `serviceruntime` metrics —
  `quota/limit` (GAUGE) and `quota/rate/net_usage` (DELTA, 1-minute sampling) — from which
  remaining must be **computed** as `limit − net_usage`. It is minutes-stale, and reading it
  **itself consumes `monitoring.query` quota**.

So the design is a two-loop one, and the usage report labels each figure accordingly:

| loop | mechanism | figure is | period |
|---|---|---|---|
| **fast** | a client-side **token bucket per (API, project, region)**, seeded from the documented defaults of §1 | **self-tracked** | real time |
| **slow** | `serviceruntime.googleapis.com/quota/limit` − `quota/rate/net_usage`, filtered by `service` and `limit_name` | **vendor-reported** | minutes-stale |

The slow loop is a **reconciliation and drift check**, not a gate: it catches an out-of-band quota
increase, and it catches the integration's own accounting drifting from GCP's. It never authorises
a call the fast loop refused, because a minutes-stale figure cannot authorise anything during a
60-second incident.

This is the honest reading of FR-148 and the plan states it as such: the "vendor-reported" branch
exists and is used, but it is a **slow** signal, and every real-time decision in this integration
is self-tracked.

---

## 4. Discovering the Monitoring limit at runtime

Cloud Monitoring's `timeSeries.list` rate limit is **not published** — Google's own quota pages
decline to give a number and redirect the reader to the Quotas dashboard or
`gcloud alpha services quota list`. Since a budget expressed as a share of an unknown limit is not
a budget, the integration **discovers it at startup** from
`serviceruntime.googleapis.com/quota/limit` filtered to `service="monitoring.googleapis.com"`, and:

- records the discovered limit in the checkpoint, so a later reader knows what share was a share
  *of*;
- falls back to a **conservative configured default** where discovery fails, and says in the usage
  report that the figure was a fallback rather than discovered;
- never treats "limit unknown" as "limit unlimited".

---

## 5. Window caps

The **width of a query window** is capped per operation and cost class (FR-150). A request wider
than its cap is **narrowed to the cap, or refused with the cap named** — never issued as asked.

| cost class | terms | cap |
|---|---|---|
| `CHEAP` | `monitor_state`, `drill_down` | wide; an indexed read |
| `STANDARD` | `compare`, `errors_by_version`, `error_spans` | one aggregation over one selector and one window pair |
| `EXPENSIVE` | `new_log_patterns`, `onset` over a long search window, `exemplars` | **the tightest**, because `new_log_patterns` is the term that spends `logging.read` |

`new_log_patterns` deserves its own line. With `entries.list` at 60 calls/min and **no documented
`pageSize` maximum** — the widely-cited 1,000-entry and ~10 MB page caps appear nowhere in Google's
reference or quota pages (research §6), so the integration designs against neither and reads the
page size it actually got — a wide window over a busy service exhausts the project's entire
per-minute log-read quota in pagination alone, and it cannot be known in advance how many pages that
will be. The cap is therefore not a politeness setting: it is what stops one algebra
term from taking the on-call's log console away mid-incident. Where the cap truncates, the digest
**states what was dropped and by which criterion** (FR-099) — a truncated answer that does not say
so is worse than a refusal.

---

## 6. Deferral order

When the budget cannot cover a full cycle, work is deferred in a **published** order (FR-149),
reported, and reflected in the checkpoints as reduced coverage:

1. **load balancers and Cloud DNS** — P3, explicitly the first thing cut (FR-057, US9)
2. **the general audit stream** beyond the deploy path
3. **Cloud SQL settings and flag polling**
4. — everything below this line is what the P1 stories depend on and is deferred only when the
   alternative is failing —
5. Cloud Run topology polling
6. alert-policy and transition polling

The order is the priority order of the user stories, which is not a coincidence: it is the same
judgement, written down twice, and a deferral that reordered it would be quietly changing what the
feature is.

**A deferral is never silent.** What was deferred and why is reported, and the checkpoint records
reduced coverage — so a later query can tell "no change happened" from "we were not looking"
(FR-040, FR-149).

---

## 7. Backing off

On `429 / RESOURCE_EXHAUSTED` or a throttling response (FR-151):

- wait **at least as long as the response asks**, where it says;
- otherwise truncated exponential backoff from 1 s, which is Google's documented guidance;
- **do not lose the position already reached** — resume from where it stopped rather than
  restarting the cycle;
- record the refusal in the integration's own operational telemetry (FR-011).

For the **telemetry backend** half, a quota refusal is not an empty answer: it is the typed outcome
`QUERY_FAILED` with reason `RATE_LIMITED` **and the earliest retry instant where GCP states one**
(FR-104). Never a partial result presented as whole, and never an empty answer that reads like
"nothing happened".

---

## 8. The usage report

What Dana sees, per run (FR-152, SC-022):

| field | note |
|---|---|
| calls per GCP area | matched against the recording **to the exact call** (SC-022) |
| share of the configured budget consumed | per endpoint class |
| share of remaining quota per endpoint class | with each figure labelled **vendor-reported** or **self-tracked** (§3) |
| how much of the published human reserve was left untouched | must be 100% of the reserve, in 100% of cycles |
| quota refusals received | with the class and the retry instant |
| **the feeders' and the telemetry backend's usage, separately** | FR-113: what investigations cost, as distinct from what ingestion costs |

And, before it runs at all (FR-154), `docs/connectors/gcp.md` documents what it costs against an
estate of a stated size: **calls per hour per area at the default cadence**, and the GCP-side
products and volumes it reads — so Dana approves a number rather than an intention.

---

## 9. The one cadence that is not negotiable

Alert polling runs at **15–30 seconds** (FR-050) because SC-004 binds transition-to-observed delay
to the poll interval plus one minute at p95. That is the only cadence in this feature the budget
may not stretch, and it is cheap: alert-policy and incident polling do not touch `logging.read`,
which is where the scarcity is.

Everything else — topology, Cloud SQL, the audit stream, load balancers — runs on its own slower
cadence and is stretchable, which is what makes the deferral order of §6 usable rather than
theoretical.
