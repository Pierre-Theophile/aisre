<!-- SPDX-License-Identifier: Apache-2.0 -->

# The GCP connector

> **Status: skeleton.** The sections below are the ones FR-154 and SC-020 require, and each is
> filled from measurement rather than intention. Every table marked _(pending)_ is filled by the
> task named beside it; a number written here that was not measured is worse than an empty cell,
> because an operator approves this page and then stops asking.

What this connector reads, what it costs, and the proof it can only read. It exists so that Dana,
who owns the organisation's API quotas and its Cloud Logging and Cloud Monitoring bill, **approves a
number rather than an intention** (FR-154).

Contract: [`specs/003-gcp-integration/contracts/gcp-feeder.md`](../../specs/003-gcp-integration/contracts/gcp-feeder.md).
Budget: [`contracts/budget.md`](../../specs/003-gcp-integration/contracts/budget.md) and
[`config/gcp-budget.yaml`](../../config/gcp-budget.yaml). Vendor-API facts, including four
assumptions that turned out to be wrong: [`research.md`](../../specs/003-gcp-integration/research.md).

## 1. The read-only role set, per area

The three-layer gate (FR-003–FR-005, FR-112, SC-020): the connector refuses to start with a
write-capable principal, verifies by permission test or effective-role listing, and issues no
mutating operation anywhere — verified **from the integration's own request log**, not from intent.

Two findings here are not what a reader would guess, and both are why this table is per-area rather
than "use the Viewer roles":

- **`roles/cloudsql.viewer` is unusable.** It grants `cloudsql.instances.export`, a data-egress
  capability, which disqualifies it under FR-004. A **custom role** is required.
- **`roles/compute.networkViewer` is unusable** for the same class of reason: it grants
  `trafficdirector.networks.reportMetrics`, which writes. The broader `roles/compute.viewer` is the
  cleaner choice, which is the one case where the wider role is the safer one.

| area | role | why not the obvious one |
|---|---|---|
| Cloud Run | `roles/run.viewer` | nothing narrower exists, and it grants no export |
| Cloud Logging | `roles/logging.viewer` | **not** `roles/logging.privateLogViewer`, which grants `logging.privateLogEntries.list` — the data-access stream this connector never reads (FR-042) |
| Cloud Monitoring | `roles/monitoring.viewer` | it covers `alertPolicies.list`, `timeSeries.list` and `metricDescriptors.get`; the Preview `projects.alerts.list` is covered by the same role |
| **Cloud SQL** | **a custom role** — see below | `roles/cloudsql.viewer` grants `cloudsql.instances.export`, a data-egress capability, and disqualifies itself under FR-004 |
| GKE | `roles/container.clusterViewer` | it reads cluster metadata and nothing inside a cluster, which is exactly FR-030's boundary |
| load balancers (P3) | `roles/compute.viewer` | **not** `roles/compute.networkViewer`, which grants `trafficdirector.networks.reportMetrics` — a write. The one case where the wider role is the safer one |
| Cloud DNS (P3) | `roles/dns.reader` | nothing narrower exists |
| the doorbell (off by default) | a custom role with `pubsub.subscriptions.consume`, bound **resource-level to one subscription** | see §2.1: there is no read-only pull at any granularity |

### The custom Cloud SQL role, in full

`roles/cloudsql.viewer` is the role a reader reaches for and it cannot be used: among its
permissions is `cloudsql.instances.export`, which writes a database's contents to Cloud Storage.
That is a data-egress capability held by a connector that never needs it, and FR-004 disqualifies a
credential that *can* do something it must not — not merely one that does.

So the role is assembled from the permissions this connector actually issues:

```yaml
# gcloud iam roles create sreAgentCloudSqlReader --project=PROJECT --file=this
title: "SRE Agent Cloud SQL reader"
description: >
  Read-only Cloud SQL access for the sre-agent connector. Deliberately excludes
  cloudsql.instances.export, which roles/cloudsql.viewer grants and which is a data-egress
  capability this connector never needs (FR-004).
stage: GA
includedPermissions:
  - cloudsql.instances.list
  - cloudsql.instances.get
  # `cloudsql.flags.list` is deliberately absent: the flags endpoint is project-less and gated
  # purely by OAuth scope, so there is no permission to grant. See internal/feeders/gcp/transport.go.
  #
  # `cloudsql.databases.list` was here and has been removed. It reads database NAMES only and is
  # harmless, and no call site issues it — which is the whole reason it is gone. A role assembled
  # from "the permissions this connector actually issues" cannot carry one it does not, and T174's
  # comparison of this page against the enforced list is what found it. A grant nobody uses is a
  # grant nobody reviews.
```

`operations.list` needs no permission of its own beyond `cloudsql.instances.get`.

## 2. The published read-only operation list

Every operation this connector may issue, by name. An operation absent from this list is refused by
`internal/gcpx` rather than by review (FR-005).

| area | operation | class |
|---|---|---|
| Cloud Run | `run.services.list`, `run.services.get`, `run.revisions.list`, `run.revisions.get` | `run.read` |
| Cloud Logging | `logging.logEntries.list` — **admin activity only** | `logging.read` |
| Cloud Monitoring | `monitoring.alertPolicies.list`, `monitoring.timeSeries.list`, `monitoring.metricDescriptors.get` | `monitoring.policies`, `monitoring.query` |
| Cloud Monitoring (Preview, opt-in) | `monitoring.alerts.list` | `monitoring.policies` |
| Cloud SQL | `cloudsql.instances.list`, `cloudsql.instances.get`, `cloudsql.operations.list`, `cloudsql.flags.list` | `sqladmin.read` |
| GKE | `container.clusters.list`, `container.clusters.get` | `container.read` |
| load balancers (P3) | `compute.forwardingRules.list`, `compute.urlMaps.list`, `compute.backendServices.list`, `compute.networkEndpointGroups.list` | `compute.read` |
| Cloud DNS (P3) | `dns.managedZones.list`, `dns.resourceRecordSets.list` | `dns.read` |
| the doorbell (off by default) | `pubsub.subscriptions.pull` | `pubsub.pull` |

Every one of them is a **list or a get**. There is no `create`, no `update`, no `patch`, no `delete`
and no `export` anywhere in the list, and that is checkable rather than asserted: `internal/gcpx`
classifies an operation by name before it is issued and refuses one outside this set (FR-005).

**The table above and the code are one list.** It lives in
[`internal/gcpx/requestlog.go`](../../internal/gcpx/requestlog.go) and this page is parsed and
compared against it by a test, in both directions — a row here that the process would refuse fails
the build, and so does an operation the process carries that this page does not publish. That
matters more than it sounds: this page is what Dana approves, and an approval that can drift from
the system is an approval of a document.

A call spends quota through one door, `Budget.Issue`, which takes the **operation** and derives its
endpoint class from the table rather than from the call site. Three drifts stop being possible at
once: a call site cannot issue an unpublished operation, cannot meter a call against the wrong quota
pool, and cannot reach a Google client without appearing in the request log below.

**The telemetry backend is in this list too.** Executing a term creates no GCP object: a
`compare` reads `timeSeries.list`, a `new_log_patterns` reads `entries.list` and mines the sample in
memory, and neither writes a monitoring resource, a saved query or a log sink (FR-112). A backend
that created a saved query to answer a question would be a connector that changes the thing it is
measuring.

### How this is verified, and the one part that is not

SC-020 asks for read-only *"verified from its own recorded request log"* and, pointedly, *"from the
set of operations the integration can issue, not only from one run's behaviour"*. One run proves
nothing about the branch it did not take, so four separate assertions carry it (T174):

| what is asserted | where | how it is falsified |
|---|---|---|
| every operation on the list is a `list` or a `get`, and exactly one row declares a state change — the doorbell | `internal/gcpx/requestlog_test.go` | plant a write, a `validate`, or a second declared change on the list |
| an operation not on the list is refused **before any quota is spent**, on a metered budget and an unmetered one alike | same | make `Issuable` stop consulting the list |
| this page and the enforced list are the same list, operation and area both | same | drop a row here, add one here, or rename an area |
| every method that reaches a Google client first issues a published operation, named by a constant | `internal/feeders/gcp/readonly_test.go` | delete the metering from one reader, or compute the operation name |

The request log itself is per consumer, so FR-112's half is legible rather than argued: the
telemetry backend spends the same budget through the same readers, its rows say `backend`, and every
one of them is a read. Executing a term creates no monitoring resource, no saved query and no log
sink. A backend that created a saved query to answer a question would be a connector that changes
the thing it is measuring.

**What is not verified here.** The criterion also asks for the live run, and that half is open: it
needs a real credential against a real organisation, which is the same blocker as the sanitisation
campaign (US7). What is verified is the capability — which is the stronger half, and the half a live
run could not have given on its own — over the unit surface and every 003 fixture. The live-run
figure is recorded when the credential exists, and until then this page says so rather than implying
a run that did not happen.

### What the three-layer gate cannot prove

The gate — refuse a write-capable principal, verify by permission test or effective-role listing,
issue no mutating operation — is what makes the list above enforced rather than aspirational. It is
worth being precise about its limits, because a guarantee overstated is worse than one stated
narrowly:

- **It proves what the credential may do, not what the API does.** A `list` that Google implements
  with a side effect would pass every layer. Nothing in a client can see that, and the mitigation is
  that the operations are all published reads of published resources.
- **Layer 2 is a point-in-time check.** An IAM binding added a second after startup is not seen
  until the next run. The connector re-proves on every start and the checkpoint records that the
  layer ran; it does not poll IAM.
- **It says nothing about the credential's *other* holders.** If the same service account is used by
  something else, this process's read-only proof is about this process.
- **On a replay there is no credential at all**, so the layer does not run — and the checkpoint
  records that it did not, rather than reporting a check that did not happen.

### The one declared exception

`pubsub.subscriptions.consume`, the alert doorbell (FR-006). Four facts, and they have to be read
together because each one alone reads more comfortably than the truth:

1. **It is not read-only, by IAM or by semantics.** The permission authorises `pull`,
   `acknowledge`, `modifyAckDeadline` and `seek` **together** — one permission, four verbs — and
   acknowledging removes the message from the backlog. So pulling a subscription changes state, and
   the plan does not get to call it read-only. It gets to call it *declared*.
2. **`roles/pubsub.viewer` cannot pull.** It grants `pubsub.subscriptions.get` and `.list`, which
   describe a subscription and never consume from one. A reader looking for the narrow role finds
   that it does not do the job.
3. **There is no read-only pull at any granularity — not even with a custom role.** A custom role
   is assembled from published permissions, and the published permission that permits pulling is
   the one above. There is nothing finer to grant. This is the sentence that makes the exception an
   exception rather than an unfinished narrowing.
4. **So the exception is *holding the grant*, not using it carefully.** It is bound
   **resource-level to one subscription the operator created for this connector** — never
   project-level, never on a subscription anything else consumes — and it is **off by default**.

And the reason it is acceptable at all is that nothing depends on it. **Polling is the source of
truth.** A doorbell only makes a scheduled poll happen sooner; the transition is read from
`projects.alerts.list` and never from the message. The integration runs correctly with the doorbell
absent, and every fixture in the corpus runs that way.

#### What a notification can and cannot do

The rules are in the signatures rather than in this page
(`internal/feeders/gcp/doorbell.go`), which is what makes them checkable:

| rule | how it is enforced |
|---|---|
| the body is never parsed, trusted or stored | `Doorbell.Ring` takes **no payload argument**. A function that cannot receive the body cannot read it, and widening it is a diff a reviewer sees |
| the one operation on the subscription yields no content | `DoorbellSource.Drain` returns an **int** — how many were waiting. There is no value in the process for a body to be |
| a flood costs at most the rate limit | a token bucket, one token per 15 s, burst 1. Fifty notifications and one earn the same single poll, because the poll reads the whole window regardless |
| a forged, replayed or malformed notification creates, alters or retracts nothing | the only thing a ring can return is *poll now*, and the poll reads the API. The most a forger achieves is an extra read of the truth |
| "quiet" and "unreachable" stay different | a drain that fails is counted in the checkpoint's doorbell report and does **not** suppress the scheduled poll |

`fixtures/gcp-forged-doorbell-01` is the assertion: a forged notification, and a graph that does not
move.

## 3. Calls per hour per area, at the default cadence

The number Dana approves (FR-154, SC-022). Measured against an estate of the stated size, and
matched against the recording **to the exact call**.

**Estate the figures are derived against** — the one plan.md states, written out so the arithmetic is
checkable and so the number changes visibly when the estate does:

| | count |
|---|---:|
| projects | 2 |
| regions | 2 |
| Cloud Run services | 12 |
| live revisions inside the history horizon | 120 |
| Cloud SQL instances | 8 |
| Cloud Monitoring alert policies | 40 |
| GKE clusters | 1 |

At the cadences `config/gcp-budget.yaml` publishes, and with each area's calls per cycle written out so
a reader can redo the multiplication rather than trust it:

| area | cadence | calls per cycle | calls/hour | endpoint class | share of that class's hourly quota |
|---|---|---|---:|---|---:|
| Cloud Run topology | 60 s | 4 (`services.list`, one per project × region) + 12 (`revisions.list`, one per service) = **16** | 960 | `run.read` | 0.5% of 180,000 (per project × region) |
| general audit stream | 60 s | 2 windows × ~2 pages = **4** | 240 | `logging.read` | **6.7%** of 3,600 — see below |
| Cloud SQL | 5 m | 2 (`instances.list`) + 8 (`operations.list`) + 1 (`flags.list`) = **11** | 132 | `sqladmin.read` | 0.4% of 30,000 |
| alerts | 20 s | 2 (`alertPolicies.list`) + 2 (`alerts.list`) = **4** | 720 | `monitoring.policies` | limit undocumented; metered, never assumed unlimited |
| load balancers and DNS | 30 m | P3, and the first thing deferred | — | `compute.read`, `dns.read` | limits undocumented |

**Ingestion's share of the binding class is 6.7%, and that is not the figure to plan against.** The
40% share and the 30% reserve are for the class as a whole, and the rest of that 40% is the
*investigation* half: `new_log_patterns` mines log bodies over a bounded sample, and one investigation
can spend more `logging.read` in a minute than ingestion spends in an hour. The two are metered apart
(FR-113) precisely because "what does ingestion cost" and "what do investigations cost" are different
questions with different owners, and one total answers neither.

So the number to approve is the **pair**: ingestion holds ~7% of `logging.read` steadily, and
investigations may take the rest of the 40% in bursts, never touching the 30% reserve — which
`internal/gcpx`'s usage report asserts per run, and `TestTheHumanReserveIsNeverSpent` asserts as code.

The binding constraint, stated before the table so it is not read as a footnote:
**`logging.entries.list` is 60 calls/min per project, not hierarchical, and shared with every human
querying Cloud Logging in that project.** The connector's steady-state share is 40% and it never
spends the 30% human reserve, in 100% of cycles.

## 4. GCP-side products and volumes read

| product | what is read | volume at default cadence |
|---|---|---|
| Cloud Run Admin | `services.list`, `revisions.list` — the serving topology and the observed traffic split | 16 responses/minute; a service response is order 10 KB, a revision list order 10² KB |
| Cloud Logging | `entries.list` on the **admin-activity** stream only, filtered to the configured services and operations | ~4 pages/minute, each whatever the server returns; no data-access stream is read and no permission for it is requested |
| Cloud Monitoring | `alertPolicies.list`, `projects.alerts.list` (behind the Preview capability flag), `timeSeries.list` and `metricDescriptors.get` on demand for an investigation | 4 responses per 20 s for alerts; time series only while an investigation is running |
| Cloud SQL Admin | `instances.list`, `operations.list` per instance, `flags.list` | 11 responses per 5 minutes |
| GKE | `clusters.list` with `-` as the location wildcard | 1 response per project per poll; the cluster only, nothing inside it |
| Cloud Storage, BigQuery, Pub/Sub topics, IAM | **nothing is read.** They appear only as resource names inside audit entries, recorded as strings | — |

Nothing above is a data plane: no object, no row, no message body, no log body beyond the bounded
sample `new_log_patterns` mines and discards. What the graph keeps is identifiers, typed facts and
pointers (FR-109).

## 5. Binary size, before and after

SC-020's figure, and Complexity Tracking C1's: a connector that doubles a single static binary has a
cost that appears in no quota.

Measured 2026-09-21 on `main` at the Phase 2B commit, `CGO_ENABLED=0 make build`:

| | bytes | |
|---|---:|---|
| before the vendor modules | 88,608,928 | 85 MiB |
| with the GCP SDKs linked | 91,938,976 | 88 MiB |
| **cost of the six modules** | **3,330,048** | **3.2 MiB, +3.8%** |

> **The shipped binary is unchanged today, and that is not the same number.** `feed gcp` is still a
> stub, so nothing in `cmd/aisre` imports `internal/feeders/gcp` and the linker drops the SDKs
> entirely — the binary on disk is byte-identical to the one before the modules were added. The 3.2
> MiB above was measured by linking the transport into the CLI deliberately and removing the probe
> again, because "no change yet" is a fact about the wiring rather than about the dependency, and C1
> asks what the dependency costs.
>
> Retake this measurement when the feeder is wired (Phase 3), since the figure above is a lower
> bound: it links the transport seam, not the per-area read paths that will follow it.

`cloud.google.com/go/pubsub/v2` is **not** in `go.mod` yet. `go mod tidy` drops a module nothing
imports, and the doorbell — the one place Pub/Sub is used — is off by default and arrives with its
own task. It will add to the figure above when it lands.

## 5.1 The commit a rollout shipped, and why it is configuration

Feature 004's certain rule C8 merges a rollout this connector saw with the same rollout a deploy
feeder saw, by keying on a shared deploy identifier. This connector supplies two, and they are not
equally available — which is worth stating plainly, because the join depends on it.

Both are emitted as **correlation keys** rather than identity claims (004 T148,
[resolution.md §1a](../schema/resolution.md)): a commit describes a rollout rather than naming one, so
several changes legitimately carry it and `graph.identity_claims` — unique per
`(namespace, value, source_id)` — could hold it for only one of them.

| key | where it comes from | when it is there |
|---|---|---|
| `deploy.image` | the image reference the revision runs, **digest form only** | whenever the deployer pinned a digest. A `--image=repo:tag` deploy states no digest anywhere, and a tag yields no key: two rollouts running `:latest` are not one rollout |
| `deploy.commit_sha` | a **label**, named by `commit_from_labels` in [`config/gcp.yaml`](../../config/gcp.yaml) | only when your deploy tooling sets it, and only when the key is on the label allowlist |

**The Cloud Run v2 API states no commit anywhere.** Checked against the published surface rather than
assumed: `Revision` has no commit field, and no resolved-digest field either — `ContainerStatus` with
its `imageDigest` hangs off `Instance`, not off a revision. `Service.buildConfig` is the functions
source-deploy path and its `sourceLocation` is a Cloud Storage bucket URI, not a git reference. So the
commit can only come from a label whatever deployed the revision chose to set, which is a convention
of your pipeline and not something the platform publishes.

That is why the key is configuration with `commit-sha` as a **default rather than an answer**, and why
the cost of it being wrong is bounded: a label whose value is not a full hex commit object id mints no
claim at all, and an abbreviation is never padded (FR-041). A wrong key costs a missing join, never a
wrong merge.

It matters because the two platforms state different halves. A GitHub deployment states a commit and
no image digest; Cloud Run states an image and no commit. **So the commit label is the only thing a
GitHub rollout and a Cloud Run rollout can agree on** — without it, C8 can still join Cloud Run to
another source that states the image digest, but the GitHub side needs the label.

The label is read from the *kept* properties, so a key the allowlist drops is not read. That is
deliberate: the allowlist is a disposal boundary as well as an observation one (FR-137), and reading a
commit around it would make a claim out of a label an operator chose not to observe.

## 6. What this connector does not read

- **No data-access log stream, and no permission for it is requested** (FR-042). Admin activity is
  always written and cannot be disabled, which is why it is a dependable change stream; data access is
  off by default, enormous, and disproportionate to its value for change detection. The prohibition is
  enforced rather than intended: an entry whose `logName` names the data-access stream is **refused**
  — `ErrDataAccessStream`, asserted in `internal/feeders/gcp/auditlog_test.go` — so a recording cannot
  smuggle one in, and `logging.privateLogEntries.list` appears nowhere in the operation list above.
- **No load balancer or DNS record by default** (FR-057). They are P3 and the first surface the
  published deferral order drops, so a run that does not read them is the ordinary case — and the
  checkpoint names **both** surfaces and why. It is both or neither: the load balancers without DNS
  would produce host names nothing resolves, and DNS without the load balancers records pointing at
  addresses no node carries, so either half alone is a graph that looks complete and answers wrongly.
  Where they *are* read, the role is `roles/compute.viewer` and `roles/dns.reader` —
  `roles/compute.networkViewer` is not an alternative, because it grants
  `trafficdirector.networks.reportMetrics`, which writes.
- **No GKE workload**, and no node the Kubernetes feeder owns (FR-030). In this organisation the
  deployed unit is a Cloud Run revision; the one inference cluster stays with the feature 001
  Kubernetes feeder, and this connector reads only the cluster's metadata.
- **No trace data source**, because none exists. `error_spans` answers `no_data` with a coverage
  block naming the absent source (FR-091), and becomes live with no contract change if Cloud Trace
  is enabled later.
- **Nothing measured is stored.** Every node carries pointers; digests are transient and never
  written to the graph (FR-109, SC-013).

## 7. Cloud SQL, configuration and dependencies

### 7.1 A configuration fingerprint needs a key, and without one no fingerprint is stored

The connector records what a Cloud Run revision was deployed with: environment variable **names**,
value **fingerprints**, mounted configuration references, and secret **version references**. It never
records a configuration value and never records secret material (FR-034).

A fingerprint answers *"did this change?"* without storing what changed to. For that to be true it
has to be a **keyed** HMAC:

```yaml
gcp:
  config:
    # A high-entropy secret, from the operator's secret store. Not a checked-in value.
    fingerprint_key_env: SRE_AGENT_GCP_CONFIG_FINGERPRINT_KEY
```

An unkeyed digest of a configuration value is not a redaction. Configuration values come from a
small, guessable space — `true`, `8080`, `production`, a region, an image tag — so `sha256("true")`
is a lookup away from `"true"` for anybody holding the graph, and a digest of a password is an
unsalted password hash.

**With no key configured the connector stores no fingerprint at all.** The entry's *name* is still
recorded, the configuration node carries
`sre.gcp.config_fingerprints_unavailable` saying why, and a configuration diff reports the affected
entries as **undetectable** rather than unchanged. An absent fingerprint reads as "we did not say",
which is true; a crackable one reads as a redaction, which is false.

The key is mixed with the entry's **name** as well as its value, so two entries holding one value do
not fingerprint alike — otherwise the graph would quietly answer "is `DB_PASSWORD` the same string as
`ADMIN_PASSWORD`?".

### 7.2 Two derivations assert a dependency; everything else suggests one

A `depends-on` edge is an assertion: a blast radius traverses it and a person reading a hop-1 list
believes it. So it is asserted only from something **somebody configured to be true**, and the
deriving evidence is recorded on the edge (FR-028):

| derivation | read from | why it is an assertion |
|---|---|---|
| `cloudsql_volume` | `template.volumes[].cloudSqlInstance.instances` | an operator attached the instance and Cloud Run acts on it: the proxy is mounted, the socket exists |
| `env_var_value` | an environment variable whose **value is** a connection name | the operator put the instance's dialling string into the service's configuration |

The second reads a value and records none: what reaches the graph is "the dependency was derived from
the value of `DATABASE_URL`" — the fact, the evidence, and no value. The search for a connection name
inside a value is a **whole-token match against a strict shape** (three parts, the middle one shaped
like a GCP region), because a substring search would find a colon in a hostname and assert a
dependency on an instance that does not exist.

Everything else is a **proposed dependency** — recorded, ranked, reviewable, and never an edge until a
human confirms it (FR-029):

| rule | score | evidence |
|---|---:|---|
| `D1` | 0.6 | a Cloud SQL instance's name appears inside the name of an environment variable the service defines, and nothing in the configuration carries the instance's connection name |

`D1` matches the identity rule `P4`'s score deliberately: it is the same evidence, and two numbers for
one observation would be two numbers a reviewer has to reconcile. It refuses to fire on an instance
name shorter than four characters (`db` is inside `DB_HOST` and almost every variable name in
existence) and on an instance in another project (two projects are two environments until somebody
says otherwise, FR-010).

A proposal is raised **once per cycle**. The durable half — not re-raised while pending — is the
projector's: a decision about a proposal survives replay and outranks any later automated match
(FR-122).

### 7.3 A published resolution rule for the instance connection name

`C7` (FR-120) merges two sources that claim the same Cloud SQL **instance connection name**
(`<project>:<region>:<instance>`), carried as the claim attribute
`sre.gcp.instance_connection_name`. It is *certain* because the connection name is globally unique and
is the string every client that reaches the instance is configured with — two sources reporting it are
quoting one configured identifier rather than resembling each other.

It exists alongside `C1` rather than inside it because the common case is that the two sources address
the instance **differently** — `<project>/<region>/<instance>`, the fully qualified resource name, a
name an operator typed — and carry the connection name as a supporting attribute of whichever
identifier they use. `C7` compares the attribute, so the values need not be the same string.

**Where the other side comes from, and how this rule was dead.** The rule searched one namespace —
`gcp.cloudsql.instance` — on *both* sides, while refusing two claims from one source. This connector
is the only thing that claims in that namespace, so the only pair it could ever match was one that
cannot exist: published, registered, evaluated on every claim, and unfireable. Widening the fixture
corpus would not have found it, because the data it needed was excluded by the query rather than
missing.

The observed side is an **outbound dependency's address**. On Cloud Run, GKE and anywhere the Cloud
SQL Auth Proxy runs, a client reaches the instance through a Unix socket the connector mounts at
`/cloudsql/<project>:<region>:<instance>` — so the caller's own `server.address` *contains the
connection name verbatim*, which is exactly the "quoting one configured identifier" this rule rests
on, seen from the client end. `internal/feeders/otel` lifts it onto the claim under this spelling, and
`C7` now compares across the two namespaces.

That translation is **not** gated on `cloud.platform`, unlike the Cloud Run revision attributes C4
reads, and the difference is deliberate. That gate exists because `faas.version` means a revision on
Cloud Run and an alias on Lambda — the platform is what disambiguates the attribute. `/cloudsql/` is
a Cloud SQL mount wherever it appears and the string under it is globally unique by construction, so
a platform gate here would only lose true matches, chiefly on GKE and Compute Engine where the proxy
is most used. The shape is the evidence, so the shape is checked strictly on both sides: three
non-empty, whitespace-free, colon-separated parts, or no attribute at all.

### 7.4 Three things Cloud SQL reports that are deliberately **not** changes

- **`settings.maintenanceWindow` and `settings.denyMaintenancePeriods`** are a recurring policy:
  properties of the instance, never change nodes. A window that says "Sunday 03:00" is a calendar
  entry, not an event, and a change stream carrying every calendar edit would bury the edits that
  moved production. An edit to the policy stays recoverable from the node's own property history.
- **The instance state** (`RUNNABLE`, `SUSPENDED`) is a property. A state that changed is a change the
  audit stream reports, and deriving one from two polls would date it at the poll.
- **The flag catalogue** (`flags.list`) is neither a node nor a change. It is held to annotate a flag
  change — `sre.gcp.sql_flags_requiring_restart` names the changed flags the catalogue says need a
  restart — and a `flags.list` response with no flag change beside it produces no events at all.

### 7.5 An announced maintenance is never confirmed by this connector

`DatabaseInstance.scheduledMaintenance.startTime` is a genuinely future-dated instant, so this is the
one place the platform connector emits an **announced fact**, with the vendor-notice connector's
semantics unchanged — the same state machine, the same correction rules, the same ranking exclusion.
The two invariant checks (FR-063, FR-066) are **called from that connector** rather than restated, so
there is one implementation of each.

GCP publishes **no identifier** linking a `scheduledMaintenance` announcement to the `MAINTENANCE`
operation that later performed it. So a past maintenance is emitted as its own observed change and the
announcement is **never promoted to `CONFIRMED`**: correlating them would be a guess, and a guess that
promoted an announcement would assert that a specific announced window is the one that happened. The
gap is stated on the change (`sre.gcp.sql_announced_not_correlated`) rather than closed by inference.

A **reschedule** is observed as it happens: the previous poll's window is re-observed as `SUPERSEDED`
carrying its original interval, and the new window opens as `ANNOUNCED`. The two carry different
refs, which follows the vendor-notice connector's derived-identifier rule — a notice the vendor names
keeps one ref across a reschedule, and a notice with no identifier is keyed on its window. GCP names
none. A window **withdrawn before it arrives** is `CANCELLED`; a window that simply stops being
announced **after its start has passed** produces nothing, and the checkpoint is the only place a
reader learns it is no longer announced.

### 7.6 GKE: the cluster, and nothing below it

The GKE read produces one node per cluster and stops — no node pool, no workload, no pod, no
container (FR-030). `clusters.list` returns node pools in the same response, and the prohibition is
that they are not emitted; the node states it as a property
(`sre.gcp.gke_contents_not_read`) so a reader does not have to take it on trust.

The cluster claims the **bare name** in `k8s.cluster`, which is how the Kubernetes connector addresses
the same cluster — a kubeconfig context carries a name and not a project. That claim is what lets the
published `C1` rule merge the two sides, so the graph holds one cluster with a GCP project on it and
Kubernetes workloads under it. Its valid start is left **unknown** where GCP states no `createTime`,
matching what the Kubernetes connector asserts, so the merged entity does not read differently
according to which connector's event arrived first.

A **zonal** cluster's location is a zone, so `cloud.region` is set only where the location is actually
a region — the zone/region test is on the shape (`europe-west1` against `europe-west1-b`) rather than
against a list of regions that would go stale the week Google opened one.
