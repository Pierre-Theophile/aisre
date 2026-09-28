# Stamping logs with the deployed version

**Contract**: [specs/005-datadog-connector/contracts/version-stamping.md](../../specs/005-datadog-connector/contracts/version-stamping.md)
· **Package**: `pkg/feeder/versionstamp` · **Requirements**: FR-040b–FR-040g

"Did the errors start with a particular version?" has one answer for every service, however it is
deployed, as long as each log line names the version that wrote it. This guide shows how to make
your logs do that on each deployment type, and how to read what the connector decided.

It is not Datadog-specific. Every log backend that implements the contract tries the same convention
list and applies the same test.

---

## 1. What to stamp, and why the commit

Stamp every line with the **full commit sha** the process was built from. The connector turns a
stamp value into a platform-neutral reference, and the form of the value decides what that
reference can join to:

| The stamp's value | Becomes | Joins to |
|---|---|---|
| a 40- or 64-character hex commit | `deploy.commit_sha` | every deploy feeder that states a commit (GitHub, Vercel, Cloud Run with a commit label, Kubernetes with one), and merges with their rollout |
| `image@sha256:…` | `deploy.image` | Cloud Run and Kubernetes rollouts of that digest |
| anything else without whitespace (`v2.3.0`, a revision name) | `deploy.release` | only a source stating the same string; it is never merged |

These values produce **no** reference, and the group says why:

| Value | Reason |
|---|---|
| an abbreviated sha, such as `a1b2c3d` | `ABBREVIATED_SHA` |
| `repo/app:latest` | `MUTABLE_TAG` |
| a bare `sha256:…` with no image name | `BARE_DIGEST` |
| empty, or containing whitespace | `NOT_A_STABLE_IDENTIFIER` |

A new commit or image seen in a service's logs is also a **rollout** the connector can infer on its
own, dated from its first indexed line. That covers services no deploy feeder watches. A release
label is counted, and no rollout is inferred from it.

## 2. Where the connector looks

The fields below are tried in order, and the first one that passes the test in §3 wins. The
recipes in §4 aim at the first row.

| # | Field | Form |
|---|---|---|
| 1 | `version` | tag (Datadog unified service tagging: `DD_VERSION`) |
| 2 | `service.version` | attribute (OpenTelemetry resource attribute) |
| 3 | `version` | attribute (the application logger's own field) |
| 4 | `git.commit.sha` | tag, or attribute |
| 5 | `container.image.name` + `container.image.digest` | attribute pair |
| 6 | a platform's version attribute, such as `faas.version` | attribute |

An operator can name one field per service with `--version-override <env>/<service>=<name>` (a tag)
or `=@<name>` (an attribute). The override replaces the list for that service and is still tested.

## 3. The test a stamp must pass

Over the discovery window (one hour by default), the field must be on at least **95 % of the
service's lines and 95 % of its error lines**.

- The error-line share is a test of its own. A crash handler that writes without the stamp would put
  exactly the errors that matter into an unlabelled group.
- A constant value is fine. A service that did not deploy has one version, and that is correct.

## 4. Recipes, by deployment type

For each deployment type: where the commit is available, what to set, and how to check it on a real
log line (§5).

### Kubernetes

The commit is known to the deploy pipeline, at deploy time.

```yaml
# The pod template, rendered by the pipeline with the commit it deploys.
metadata:
  labels:
    tags.datadoghq.com/version: "<full commit sha>"
spec:
  containers:
    - name: app
      env:
        - name: DD_VERSION
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['tags.datadoghq.com/version']
```

With OpenTelemetry, set `OTEL_RESOURCE_ATTRIBUTES=service.version=<full commit sha>` from the same
label.

### Cloud Run

The commit is known to the pipeline that runs `gcloud run deploy`, at deploy time.

```sh
gcloud run deploy checkout --image "$IMAGE" \
  --set-env-vars "DD_VERSION=$GIT_SHA"
# or, with OpenTelemetry:
#  --set-env-vars "OTEL_RESOURCE_ATTRIBUTES=service.version=$GIT_SHA"
```

**The zero-effort recipe.** Cloud Run sets `K_REVISION` in every container, so you can stamp without
touching the pipeline:

```sh
DD_VERSION="$K_REVISION"   # set in the entrypoint, e.g. export DD_VERSION="$K_REVISION"
```

A revision name is not a commit, so it becomes a `deploy.release`. The GCP feeder states every
rollout's revision name as that same `deploy.release`, so a log group and the GCP backend's
`errors_by_version` group both resolve to the rollout, by lookup. It is never merged with another
source's rollout, because C8 does not merge on releases. Stamp the commit when you can, and the
revision when you cannot.

### Vercel

The commit is available at run time as `VERCEL_GIT_COMMIT_SHA`. Have the logger attach it to every
line:

```js
const logger = pino({ base: { service: "storefront", version: process.env.VERCEL_GIT_COMMIT_SHA } });
```

This stamps the `version` *attribute* (row 3). If the logs reach Datadog through a drain that maps
`version` to the tag, row 1 is used instead.

### Containers built by GitHub Actions

The commit is known at build time. Bake it into the image:

```dockerfile
ARG GIT_SHA
ENV DD_VERSION=$GIT_SHA
LABEL org.opencontainers.image.revision=$GIT_SHA
```

```yaml
- run: docker build --build-arg GIT_SHA=${{ github.sha }} -t "$IMAGE" .
```

This works on every runtime that runs the image: Kubernetes, Cloud Run, ECS and a plain host.

### Vendor-hosted runtimes and virtual machines

This covers a managed agent platform, a systemd unit, and anything with no deploy pipeline the
connector can see. The commit is known only at build time, so bake it into the artifact and export
it at start:

```sh
# at build
git rev-parse HEAD > REVISION
# at start (entrypoint or ExecStartPre)
export DD_VERSION="$(cat REVISION)"
```

The logger must attach `DD_VERSION` (or `service.version`) to every line. A platform that offers its
own run-time commit variable can replace the file. Check the platform's documentation for one
before building the workaround.

## 5. The environment

Unified service tagging gives each line a `service`, an `env` and a `version`. Many organisations
ship logs with a `service` and no `env`, and the connector still measures them. `--watch
<env>/<service>` watches one environment, and `--watch <service>` watches a service whose logs carry
none. Such a source cannot be matched to the same service on another platform, because C9 needs the
environment. So the connector says `missing env to match service` twice: when the source is
configured, and in every discovery checkpoint.

JSON logs often carry the environment as a field, such as `{"env": "production", ...}`. That field is
the `@env` **attribute**, not the `env` tag, and Datadog's `env:` search reads only the tag. You can
either:
- remap it in a Datadog log pipeline (a Remapper from `@env` to the `env` tag), which fixes it for
  every tool; or
- tell the connector where to read it with `--env-field @env`. The source is then measured and
  pointed at on `service:<service> @env:<env>`, and it keeps its environment, so C9 still merges
  it.

If you watch `<env>/<service>` and no line carries that environment tag, the connector first counts
the service's lines with `@env:<env>`, then the lines that carry no `env` at all. When it finds
either, it tells you which fix applies, rather than reporting a service that wrote nothing.

To add the environment, use one of:
- `DD_ENV=<env>` on the service;
- the `tags.datadoghq.com/env` label on Kubernetes;
- an `env:<env>` tag in the log pipeline.

## 6. Two warnings

- **A library's own `version` field is not a stamp.** SDKs log their own version under `version`, on
  a few start-up lines. The share test rejects it, and the verdict names it. If your application's
  logger emits a field called `version`, it must hold the deployed version. Otherwise, set the stamp
  as the tag instead.
- **Unified service tagging does not reach logs everywhere.** On ECS Fargate with Fluent Bit or
  FireLens, it covers metrics and traces only. Check a real log line; the connector's verdict is that
  check.

To check a real line in Datadog's Log Explorer, run:

```
service:checkout env:production -version:*
```

With a working stamp, this returns (almost) nothing. Run it again with `status:error` added. For the
attribute forms, search `-@service.version:*` or `-@version:*` instead.

## 7. Reading the connector's verdict

Every discovery interval, each watched log source gets three things:

- **On the service node**, versioned in valid time, so an investigation of last Tuesday uses last
  Tuesday's verdict:
  - `sre.version_stamp.attribute` is the accepted field, absent when none passed.
  - `sre.version_stamp.source` is `discovered` or `operator`.
  - `sre.version_stamp.verdict` gives each candidate's class, for example
    `version (tag) accepted; service.version (attribute) not needed`, or
    `version (attribute) rejected: below the line share`.
  - `sre.version_stamp.conventions_version` is the version of the list in §2.
- **On its `datadog-logs/v1` pointer**: the `version` join key, spelled `version` for the tag or
  `@name` for an attribute. It is absent when no stamp was found.
- **In the discovery checkpoint**: the window, and every candidate with its line and error-line
  shares. The shares move every window, so they are not on the node.

When no stamp was found, `errors_by_version` still answers. The engine returns `NO_DATA`, naming the
missing stamp and every candidate's shares, without a backend call. Fix the stamp and the next
discovery interval accepts it. The pointer gains its join key from that instant.
