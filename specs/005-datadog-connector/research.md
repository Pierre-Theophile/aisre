# Phase 0 research — Datadog Connector

**Date**: 2026-09-27 · **Plan**: [plan.md](./plan.md) · **Spec**: [spec.md](./spec.md)

Three kinds of finding are recorded here, and they are not interchangeable. **§1 is read out of this
repository's code**: what features 001–004 have shipped, checked file by file against what the
specification (written 2026-09-17) assumed. **§2 is read out of Datadog's published documentation**,
cited per fact. **§3 is read out of the organisation's own Datadog**, from the read-only look on
2026-09-27 that led to the spec's clarification. Where a fact could not be established from any of
them it is listed in §5 as open, with the question it is open on.

---

## 1. What the code delivers, and what it does not

### 1.1 Delivered, and used as-is

| item | evidence | use |
|---|---|---|
| The telemetry-backend SDK: `Describe`/`Execute`, the eight telemetry terms, the refusal of everything else | `pkg/backend/backend.go:23-137`; `registry.go:132-211` | implement it; the spec's section F is already a published contract |
| The six typed outcomes, `NO_DATA`, `NOT_YET_INGESTED` and `NOT_RECORDED` among them | `pkg/backend/backend.go:60-78`; `investigation.proto:107,117` | FR-049, FR-049a |
| Cost classes, checked at registration | `backend.go:79-90, 198-240`; `registry.go:48-97` | one class per term |
| The coverage block, with an undetermined flag per field | `investigation.proto:140-161` | FR-048a |
| Recorded mode, world recordings, the miss rate | `internal/investigation/backend/recorded.go`; `missrate.go`; `pkg/backend/record.go` | FR-050–FR-050b unchanged |
| The GCP backend, a complete reference implementation | `internal/backends/gcp/*` | followed file for file; its `absent(...)` is the FR-049b pattern |
| `ALERT` and `WATCHES` | `graph.proto:32, 44` | **the spec's "watches" dependency is already met** |
| `alert.transition` with `sampled`, the 4-tuple key, the state vocabulary, flap suppression | `graph.proto:235, 471-503`; `internal/log/alert.go:25-127`; `internal/feeders/gcp/monitoring.go:1020-1062` | FR-024, FR-025, FR-025c; the GCP feeder is the pattern |
| Actor kind | `graph.proto:65-72` | FR-033a |
| Unattached alerts and changes, auto-attach | `internal/projector/attach.go`; ADR-0009 item 5 | FR-019, FR-030 |
| The `deploy.*` normalisers, and `deploy.*` as correlation keys rather than identity claims | `pkg/feeder/deployref.go`; `pkg/feeder/claim.go:50-100` | FR-040d; a Datadog rollout emits a correlation key with `deployment.environment.name`, as `internal/feeders/gcp/map.go:315,323` does |
| C8 | `internal/resolution/deploy.go:66-213` | FR-040g's merge; its preconditions in §1.3 |

### 1.2 The six gaps

Each is stated with the decision taken; plan.md tabulates them with their resolution.

**G1 — a version group cannot name its deploy identifier.** `VersionBreakdown`
(`investigation.proto:242`) carries the version, counts, rate, join keys and a drill-down.
**Decision**: add `deploy_ref` and `deploy_ref_absent_reason` (investigation.v1 MINOR, ADR-0010).
They are generic: the GCP backend fills `deploy_ref` where a revision's image digest is known, so the
property is proved on two backends rather than one.

**G2 — `errors_by_version` with no attribute never reaches a backend.** The engine refuses it as
outside the algebra (`internal/investigation/backend/algebra.go:380`), and the world cross product
only enumerates the term when a pointer carries `join_keys["version"]`
(`internal/cli/worker_record.go:776`). FR-040b's "`NO_DATA` naming every convention searched" is
therefore unreachable. **Decision**: the **engine** answers it, from the pointer alone: no version join
key ⇒ `NO_DATA`, coverage naming "no version stamp on this pointer" and the discovery verdict's
candidates. *Alternatives rejected*: widening the term so the backend answers it (every recorded world
would need extending, and every backend would re-implement the same refusal); leaving it unasked (the
investigation would never learn why the split is unavailable, which FR-040b exists to prevent).

**G3 — the read-only surface refuses POST.** `readMethods = {"GET","HEAD"}` and
`MustReadOnlySurface` panics otherwise (`pkg/feeder/readonly.go:83`). Datadog's log search and
aggregation are POST (§2.1). **Decision**: named query operations — a POST is declarable only as an
individual operation carrying a published justification, listed in `docs/connectors/datadog.md` and
tested; the method rule stays for everything else (ADR-0010). *Alternatives rejected*: allowing POST
by method, which turns FR-004's "verifiable from the operation list" into trust; not answering log
terms, which leaves the backend contract proved on one vendor.

**G4 — no environment-checked rule for a Datadog log service.** C1 accepts any namespace outside the
shared-property list (`internal/resolution/certain.go:99-136`), so a Datadog claim of
`otel.service.name` would merge a staging and a production service; nothing else reads a Datadog
namespace. **Decision**: claim `datadog.log_service` with the environment attribute, and publish
**C9**: equal normalised name **and** equal stated environment (and agreeing Kubernetes namespace or
cluster where both state one). It is the spec's FR-059 generalised from APM services to log sources.
**The C1 environment hazard predates this feature** and is recorded for its own change, not fixed here.

**G5 — the quota reader is GitHub's.** It reads `X-RateLimit-Reset` as an epoch and the family from
`X-RateLimit-Resource` (`pkg/feeder/quota.go:88-113`). Datadog's reset is relative and its family is
named differently (§2.3). **Decision**: a Datadog reader beside the GitHub one, tested against
recorded headers.

**G6 — the doorbell and the campaign record are GCP-shaped.** The doorbell's payload-free `Ring` and
token bucket are right, but it drains Pub/Sub, lives in the GCP package and has no HTTP endpoint or
secret check (`internal/feeders/gcp/doorbell.go`); the campaign record's scope is projects, regions and
a mailbox (`internal/campaign/record.go:76-91`). **Decision**: lift the doorbell into
`pkg/feeder/doorbell` with an HTTP transport requiring the shared-secret header (FR-025b); add
organisation, site, environments and indexes to the campaign scope.

### 1.3 Preconditions that change individual requirements

- **C8 excludes `deploy.release`** (`deploy.go:63-66`), requires both changes to state the same
  `deployment.environment.name` (`:168-174`), and requires a shared target after merges (`:183,
  201-207`). So a log-observed rollout merges only once C9 has made the two services one entity, and
  only for a commit sha or an image digest.
- **The image normaliser refuses a bare digest** (`deployref.go:67`), which is what
  `container.image.digest` usually holds. The convention is the pair name + digest; a digest alone is
  a group with `BARE_DIGEST` and no ref.
- **The sanitiser has no per-source tables**; `NewPolicy` is documented as the way 005 builds its own
  (`internal/sanitise/policy.go:115`), and an unassigned field fails closed (`:83-95`).
- **Nothing Datadog-specific exists in Go** beyond test fixtures and proto comments; there is no Slack
  intake package either (declarations go through `intake/declared.go`).
