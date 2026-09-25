# ADR-0004: Roadmap reordered by the coverage audit

- Status: **Accepted** (owner decision 2026-09-17; realised 2026-09-18, see below)
- Source: `docs/evaluation/coverage-audit-2026-09.md` (aggregate); incident-level detail is
  private. Follows ADR-0003 D1 ("measure the ceiling first").

## Context

The audit of the owner's organisation found that with the feature 001 feeders alone
(OpenTelemetry spans, Kubernetes) the true cause of 0 of 13 production incidents would have been
a node or change in the graph at alert time. With deploy feeders for the actual platforms the
figure is 3 of 13; with vendor change feeders it is 8 of 13. The organisation runs core
applications on Cloud Run and Vercel, has no distributed tracing, uses Datadog for one
service's logs and a handful of monitors, and declares incidents by creating a dated severity
channel in Slack. Missed vendor notices were the most repeated cause.

## Decisions

### D1. Feature 003 is the GCP integration plus a vendor-notice feeder
Cloud Run revisions and Cloud SQL become change and infrastructure nodes; Cloud Logging and
Cloud Monitoring become the first live telemetry backend for the investigation engine. A thin
vendor-notice feeder (maintenance and deprecation notices, status pages, API changelogs) turns
provider announcements into change nodes with a vendor actor. Together they lift the observable
ceiling from 0 to about 62 % of incidents.

### D2. Feature 004 is the deploy feeders
GitHub Actions and Vercel deployments as change nodes, with identity claims linking them to the
services they ship.

### D3. Datadog moves to feature 005, rescoped
Log search and monitor intake for the services it actually holds. The APM service-map story is
dropped for this organisation and kept only as an optional capability for organisations that
have tracing.

### D4. Alert intake accepts human-declared incidents
Creating a severity incident channel in Slack emits the same `alert.transition` event the 002
spec defines, through a Slack transport. This is the trigger that would have fired on every
incident in the corpus. Encoded in spec 002.

### D5. The 23 % become fixtures
Latent bugs, client-side configuration, business data and credential leaks are the ground truth
for `unobserved` and `not_change_induced` fixtures, so the engine is graded on saying so.

## Consequences
- `specs/003-datadog-connector` renamed to `specs/005-datadog-connector` and rescoped;
  `specs/003-gcp-integration` and `specs/004-deploy-feeders` created.
- The 001 OpenTelemetry feeder stays as the vendor-neutral reference and test bed; it has no
  input in the owner's organisation until tracing exists.
- Order of implementation: 002 on recorded data, 003, 004, then 005.

## Realised by (2026-09-18)

Feature 002, phases 1–8 (`f53ec15` and earlier), plus the spec tree the reorder produced.

- **The reorder itself** — `specs/003-gcp-integration`, `specs/004-deploy-feeders` and
  `specs/005-datadog-connector` exist in the order this ADR set; feature 002 was built and
  evaluated on recorded data throughout, which is what the reorder bought.
- **D1/D2, the audit is the gate on the published target** — `internal/investigation/audit`
  computes the ceiling, `docs/evaluation/thresholds.json` carries `coverage_ceiling = 0.615385`
  under audit `2026-09`, and `scripts/check-report.sh --investigation` refuses a published
  `pass_at_1` above it. The ceiling also enters the ledger as `π₀ = 1 − ceiling`
  (`internal/investigation/audit.PriorFromAudit`).
- **D4, human-declared incidents** — the same `alert_transition` shape with
  `actor_kind = PERSON`, an empty group key and `transport = human_declared`, keyed identically to
  a monitor transition (`internal/log/alert.go`).
- **D5, the 23 % become fixtures** — the `unobserved` verdict arm in
  `internal/investigation/engine/verdict.go` and the corpus variants that grade it
  (`fixtures/incidents/`, the metamorphic culprit-deleted generator).

The OpenTelemetry feeder stayed the vendor-neutral reference and test bed, as the consequences
section said it would.
