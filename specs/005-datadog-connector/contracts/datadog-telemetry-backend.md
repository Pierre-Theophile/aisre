# Contract — The Datadog telemetry backend

**Date**: 2026-09-27 · **Requirements**: section F (FR-040–FR-056) · **Reference**:
[`specs/003-gcp-integration/contracts/gcp-telemetry-backend.md`](../../003-gcp-integration/contracts/gcp-telemetry-backend.md)

The algebra, the digest shapes, the typed outcomes and the coverage block are published once, in
[`specs/002-investigation-engine/contracts/telemetry-backend.md`](../../002-investigation-engine/contracts/telemetry-backend.md),
and are **not restated here**. This page says how Datadog answers each term: which surface, which
cost class, which window cap, and what Datadog adds to or cannot supply for the coverage block. It
follows the GCP backend's contract section for section so the two can be read side by side.

---

## 1. Declaration

```go
backend.Description{
    Kind:           "datadog",
    Terms:          []string{"compare", "onset", "new_log_patterns", "error_spans",
                             "errors_by_version", "monitor_state", "exemplars", "drill_down"},
    Capabilities:   …, // per term, every one ReadOnly: true
    CostClasses:    …, // §2
    Redaction:      …, // the Datadog sanitisation table, FR-072
    AlgebraVersion: "1.0.0",
}
```

All eight terms are declared in every configuration. A term whose data source is absent is still
answered, with `NO_DATA` naming the absent source (FR-049b) — never undeclared.

---

## 2. Which surface serves which term

| term | surface | cost class | window cap (default) |
|---|---|---|---|
| `compare` | logs **aggregate** (count over each window); a monitor's own query where the pointer is a monitor | `STANDARD` | 24 h per window |
| `onset` | logs aggregate as a count timeseries, **change point computed backend-side** with `pkg/backend/onset` | `EXPENSIVE` | 6 h search window |
| `new_log_patterns` | logs **search**, a bounded paged sample, **mined backend-side** — there is no patterns API | `EXPENSIVE` | 2 h, the tightest |
| `errors_by_version` | logs aggregate grouped by the pointer's version attribute; paged search where the attribute is not groupable (§3.1) | `STANDARD` | 24 h |
| `monitor_state` | monitors API, `group_states=all` | `CHEAP` | wide |
| `error_spans` | **absent** unless `apm_topology` is enabled: `NO_DATA`, absent source "no span data source configured for this organisation" | `STANDARD` | n/a |
| `exemplars` | logs search, on explicit request only, masked and capped | `EXPENSIVE` | capped |
| `drill_down` | whichever surface minted the handle | `CHEAP` | the handle's |

With `apm_topology` enabled, `compare`, `onset` and `error_spans` gain the APM metric and span
surfaces, and the coverage block names which one answered (FR-040b's last paragraph). That row set is
specified with the capability and not built before it.

---

## 3. Terms with Datadog-specific rules

### 3.1 `errors_by_version`

- The version attribute comes from the term (`version_attribute`, read from the pointer's
  `join_keys["version"]`). A pointer without one never reaches this backend: the engine answers it
  (Gap G2, [version-stamping.md §5](./version-stamping.md)).
- Two aggregates over the window, grouped by the attribute: one with the error-level clause for
  `errors`, one without for `total`. `missing` is set to a published sentinel so lines without the
  stamp form their own, named group rather than vanishing.
- Each group is normalised per [version-stamping.md §4](./version-stamping.md) into `deploy_ref` or
  `deploy_ref_absent_reason`.
- **If the attribute is not groupable** (research §5 O1): the answer is computed from a paged search
  up to the published line cap, `coverage.sampling` states the sample and its size, and the digest
  is never presented as exact.

### 3.2 `new_log_patterns`

A paged search over the window and the baseline, capped at the published line count; templates are
mined in the backend with variables masked (FR-075), compared with the baseline, and returned with
counts, first-seen instants and the published pattern and exemplar caps. Coverage states the sample
size against the window's total from an aggregate count, so a reader knows what fraction was mined.

### 3.3 `monitor_state`

From `group_states`: per-group status and the stated `last_triggered_ts`, `last_resolved_ts`,
`last_nodata_ts`. Transitions reported are those stated instants inside the window; the digest says
the history is **sampled at the poll interval** where it derives from polls, never implying a complete
history.

---

## 4. Outcomes Datadog makes specific

| outcome | when |
|---|---|
| `NOT_YET_INGESTED` | the window's end is later than now minus the observed indexing lag; the lag is measured as the gap between the newest indexed line's timestamp and the execution instant, stated in coverage |
| `PARTIAL` | `meta.status = timeout`, or `meta.warnings` non-empty, or a page limit reached before the window was covered; what is missing is named |
| `QUERY_FAILED / RATE_LIMITED` | a 429, with the earliest retry from `X-RateLimit-Reset` (seconds from now) |
| `QUERY_FAILED / NOT_PERMITTED` | a 403 on a declared operation: the credential lacks the scope |
| `QUERY_FAILED / OUTSIDE_RETENTION` | the window starts before the index's retention (ADR-0009 item 7), with the horizon |

---

## 5. Coverage and evidence

Every digest carries: the query as sent (the JSON body, canonicalised); the index(es) searched; the
window actually covered; the lines considered and, for sampled terms, the sample; the indexing lag;
the remaining quota and its window from the rate-limit headers, keyed by `X-RateLimit-Name`; and
`vendor_request_id` where Datadog returns one (ADR-0009 item 6). A field that cannot be determined is
marked undetermined, never omitted.

---

## 6. Join keys and drill-down

Join keys preserved on every group and pattern: `version` (the raw stamp), `host` where the logs carry
one, and first-seen instants. Drill-down handles are references; each also carries a deep link
opening the same query over the same window in Datadog for a human, with no credential in it.

---

## 7. Two modes, one output

Recorded mode answers from the world recording with byte-identical digests, coverage and join keys
included (FR-050, SC-019), and never calls Datadog. `NOT_RECORDED` for an in-algebra request the
world lacks; the miss rate is reported on every verification run.
