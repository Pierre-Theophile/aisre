# Contract — Version stamps on logs, for any deployment type

**Date**: 2026-09-27 · **Requirements**: FR-040b–FR-040g · **Package**: `pkg/feeder/versionstamp`

This contract is **not Datadog's**. It says how any log backend finds the attribute that carries a
service's deployed version, what it does with the values, and what it answers when there is none.
Datadog is its first user; the GCP backend is its second (it fills `deploy_ref`, §4); a later log
backend implements the same list rather than re-deriving one. The question it answers — "did errors
start with a particular version?" — must not depend on how the service is deployed.

---

## 1. The principle

**The service stamps its own logs, and the join is on what the stamp says.** A deploy feeder knows
what was deployed; only the running process knows which version wrote a given line. So the stamp is
read from the logs, and it is joined to the graph through the platform-neutral `deploy.*`
vocabulary — never through a platform-specific identifier the logs happen to carry.

---

## 2. The convention list

Tried in order, each as a **separate candidate**. Version `1.0.0` of the list:

| # | candidate | form | typically set by |
|---|---|---|---|
| 1 | `version` | **tag** | unified service tagging: `DD_VERSION`, `tags.datadoghq.com/version`, `com.datadoghq.tags.version`; OpenTelemetry `service.version` as mapped by the backend |
| 2 | `service.version` | attribute | OpenTelemetry log records whose resource attributes the backend keeps as attributes |
| 3 | `version` | attribute | an application's own structured logger. **Also where libraries log their own version** — the reason the share test exists |
| 4 | `git.commit.sha` | tag or attribute | source-code integration (`DD_GIT_COMMIT_SHA`, the OCI revision label) |
| 5 | `container.image.name` + `container.image.digest` | attribute pair | container runtimes and collectors that enrich with image metadata |
| 6 | the version role of a registered platform vocabulary | attribute | e.g. `faas.version` (a Cloud Run revision) where the logs carry it |

An operator may **override** the list per service with one named attribute; an override is recorded
as `source: operator` and is subject to the same share test, so a typo reads as "not found" rather
than as a silent empty split.

---

## 3. Accepting a candidate

A candidate is accepted when, over the discovery window (default **one hour**), it is present on at
least **95 % of the service's lines and 95 % of its error-level lines**. The first accepted candidate
in list order wins. Both thresholds and the window are configuration with these published defaults.

- **Stability is not a criterion.** A service that did not deploy has a constant, correct version.
- **The error-level share is a criterion of its own.** A stamp missing from exactly the lines a crash
  handler writes would put the errors that matter in an unlabelled group.
- **Every candidate tried is recorded with its shares and a reason** — the verdict of
  [data-model.md §3](../data-model.md). The audit's case reads: `version (attribute): 0.0007 % of
  lines, 0 % of error lines — rejected: below the 95 % line share`.
- **Discovery is cheap and bounded**: one aggregate or sampled search per candidate per service per
  discovery interval, drawn from the budget like any other call, and never inside an investigation.

---

## 4. From a value to a `deploy.*` reference

Every value of the accepted attribute is normalised with `pkg/feeder/deployref.go`, in this order:

| value | becomes | notes |
|---|---|---|
| 40 or 64 hex characters | `deploy.commit_sha` | lower-cased |
| image name + `@sha256:…` digest (candidate 5, or a value already in that form) | `deploy.image` | the tag before `@` is dropped |
| any other value with no whitespace | `deploy.release` | case kept |

And the cases that produce **no** reference, each recorded on the group as
`deploy_ref_absent_reason`, never dropped and never guessed into a longer form:

| reason | example |
|---|---|
| `ABBREVIATED_SHA` | `a1b2c3d` — may be a sha, but the full form cannot be recovered |
| `MUTABLE_TAG` | `repo/app:latest` — two rollouts would share it |
| `BARE_DIGEST` | `sha256:…` with no image name — the normaliser refuses it, so the pair form exists |
| `NOT_A_STABLE_IDENTIFIER` | empty, or containing whitespace |

**What each form joins to** (the reason the guide recommends the commit sha):

| form | joins to a change from | merges through C8 |
|---|---|---|
| `deploy.commit_sha` | every deploy source that states a commit: GitHub (`internal/feeders/github/claims.go:100`), Vercel (`vercel/map.go:364`), Cloud Run where the revision carries a commit label (`gcp/rollout.go:441`), Kubernetes where the workload states one (`k8s/map_changes.go:459`) | yes |
| `deploy.image` | sources that state an image digest: Cloud Run, Kubernetes | yes |
| `deploy.release` | only a source stating the **same** release string | **no** — C8 excludes it; the group still names it for a reader |

---

## 5. The three answers

1. **A stamp was found.** The pointer carries `join_keys["version"]` = the attribute; each
   `errors_by_version` group carries its raw value, its `deploy_ref` or absent reason, and its counts.
2. **No stamp was found.** The pointer carries no version join key and does carry the verdict. The
   **engine** answers `errors_by_version` itself: `NO_DATA`, coverage absent source "no version stamp
   on this pointer", with every candidate and its shares. No backend call, identical live and
   recorded, stable for the window.
3. **The stamp stopped being present** (a later window below threshold). Discovery re-runs each
   interval; the pointer's join key and verdict are versioned in valid time with the node (FR-038),
   so an investigation of last Tuesday uses last Tuesday's verdict.

---

## 6. Rollouts inferred from a stamp (FR-040g)

When the accepted attribute's value set gains a value that normalises to `deploy.commit_sha` or
`deploy.image`, the feeder emits a `ROLLOUT` change: valid start the first indexed line carrying it,
**marked `sre.change.valid_from_is_a_bound`**, actor kind `UNKNOWN`, a correlation key with
`deployment.environment.name`, and a `changed-by` edge to the service. Details in
[data-model.md §6](../data-model.md). No change is inferred from a `deploy.release` value or from a
value with no reference; the checkpoint counts both.

---

## 7. What the published guide must contain (FR-040e)

`docs/connectors/version-stamping.md`, one section per deployment type, each giving: where the commit
is available (build time or run time), the exact variable or label to set, and how to check it on a
log line. At minimum:

| deployment type | the recipe the guide gives |
|---|---|
| **Kubernetes** | the `tags.datadoghq.com/version` label on the pod template, set to the commit by the deploy pipeline; `DD_VERSION` from the downward API |
| **Cloud Run** | `DD_VERSION` (or `OTEL_RESOURCE_ATTRIBUTES=service.version=…`) set at deploy to the commit |
| **Vercel** | `VERCEL_GIT_COMMIT_SHA`, available at run time, exported as the logger's `version` / `service.version` |
| **Containers built by GitHub Actions** | `--build-arg GIT_SHA=${{ github.sha }}` baked into `ENV DD_VERSION` and the OCI revision label |
| **Vendor-hosted runtimes and VMs** (e.g. a managed agent platform, a systemd unit) | bake the commit into the artifact at build time and export it as `DD_VERSION` or `service.version` at start; the logger attaches it to every line |

And two warnings, both from the audit:

- **A library's own `version` field is not a stamp.** If the application's logger emits a field named
  `version`, it must be the deployed version, or the stamp must be set as the tag instead.
- **Unified service tagging does not reach logs everywhere** — on ECS Fargate with Fluent Bit or
  FireLens it covers metrics and traces only — so the guide says to check a real log line, and the
  connector's verdict is that check.
