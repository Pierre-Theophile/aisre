# Contract — The Datadog pointer vocabularies

**Date**: 2026-09-27 · **Requirements**: FR-034–FR-039 · **Registry**:
[`docs/schema/pointers.md`](../../../docs/schema/pointers.md)

A pointer says where to look, never what was measured (constitution IV). A new vocabulary is a
schema change: a `Vocab…` constant and an entry in `Vocabularies` in `pkg/feeder/pointer.go`, a
`### name/vN` section under "Vocabularies" in `docs/schema/pointers.md`, and the reason the selector
cannot be expressed in `otel-semconv` — checked against each other by
`pkg/feeder/vocabularies_test.go`. This feature adds **two**, for the default capabilities. The APM
metric and span vocabularies are registered with `apm_topology`, not before.

Every Datadog pointer makes the split 003's pointers make: the **selector** is in Datadog's own
grammar because it is what runs; the **attributes** identifying the entity are OpenTelemetry
(`service.name`, `deployment.environment.name`), so a reader or a later backend can tell which entity
the pointer is about without reading Datadog's grammar (FR-036).

---

## 1. `datadog-logs/v1`

- **Kind**: `LOG`. **Backend**: `datadog`.
- **Selector**: a Datadog log-search query, restricted to a published subset: `service:<v>`,
  `env:<v>`, `index:<v>` where configured, and at most the tag and attribute terms the feeder itself
  writes. Always pins `service` and `env`. No free text, no wildcard on `service`.
- **Why not OTel semantic conventions**: the selector is executed by Datadog's search, whose grammar
  distinguishes a **tag** (`version:x`) from an **attribute** (`@version:x`) — research §2.4 shows
  they are different fields with different contents. OTel has no way to state that distinction, and
  a translated selector could match the wrong one silently.
- **Join keys**: `version` → the discovered attribute, spelled as the selector grammar spells it
  (`version` for the tag, `@version` for the attribute); `host` → `host` where the logs carry it.
  `workload`, `pod` and `trace` are omitted: the default capabilities cannot express them, and a
  role nothing can express is left out rather than guessed (`pkg/feeder` join-role rule).
- **Carries**: the version-stamp discovery verdict
  ([data-model.md §3](../data-model.md)).

## 2. `datadog-monitor/v1`

- **Kind**: the kind of what the monitor queries — `METRIC` for a metric monitor, `LOG` for a log
  monitor — plus a `SOURCE_LINK` to the monitor. A monitor whose type has no published kind (a
  composite, a synthetic check) gets the `SOURCE_LINK` only, and the omission is stated as a
  property rather than forced into the nearest kind. No new pointer kind is added.
  **Backend**: `datadog`.
- **Selector**: the monitor's query string exactly as Datadog stores it. Never rewritten: a monitor
  query is only meaningful in the monitor type's own grammar. The monitor id is the attribute
  `datadog.monitor.id` (decimal), which is what the backend executes against.
- **Why not OTel semantic conventions**: monitor queries span Datadog's metric, log and composite
  grammars, each with its own aggregation and threshold syntax; none of it is expressible as OTel
  attributes.
- **Join keys**: none by default; a monitor's `by {…}` group names are carried as attributes, not
  as roles, because they are the monitor's grouping and not the entity's.

---

## 3. Rules shared by both

- **No organisation, site host, account or credential in any selector** (FR-037). The site lives in
  configuration; a `SOURCE_LINK` to Datadog uses the configured site's app host and carries no key.
- **Pointers are versioned in valid time with their node** (FR-038): a service renamed at 14:00
  answers with the old selector as of 13:00.
- **Additive** (FR-039): on a node merged by C9 with a Cloud Run service, the GCP pointers and the
  Datadog pointers are both present.
