# Data Model: GCP Integration and Vendor-Notice Feeder

**Feature**: 003-gcp-integration | **Date**: 2026-09-20 | **Phase**: 1

This document is the *entity* half of the design: what each part owns, the identifier namespaces
it may mint in, what becomes a node or an edge and what explicitly does not, how valid time is
assigned in every case, and the two state machines this feature introduces. The *wire* half is
[`contracts/`](./contracts/); the vendor-API facts behind the choices are
[`research.md`](./research.md).

Nothing here is a new persistent schema. This feature adds **no table and no store**: the feeders
emit feature 001 events and the graph is their projection, and the telemetry backend stores
nothing at all. "Entity" below means a graph entity in 001's published node and edge taxonomy,
except where a row says otherwise.

---

## 1. The three parts, and the boundary between them

| part | source id | credential | emits | reads |
|---|---|---|---|---|
| **GCP feeder** | `gcp:<org-slug>` | the read-only service account | graph events | Cloud Run, Cloud SQL, Cloud Audit Logs (admin activity), Cloud Monitoring policies and transitions, GKE cluster metadata, and — where in scope — load balancers and Cloud DNS |
| **Vendor-notice feeder** | `vendor-notice:<org-slug>` | mailbox credential + public feeds | graph events | the shared mailbox, status-page feeds, changelog feeds |
| **GCP telemetry backend** | *not a source* | the **same** service account as the GCP feeder | **nothing** — no graph event, ever | Cloud Monitoring, Cloud Logging, and a trace source if one ever exists |

Three rules govern the boundary, and each is a fixture rather than a convention:

1. **The two feeders share nothing** (FR-002). Different source identifiers, credentials,
   cadences, budgets and checkpoints. A scope change or a total failure in one leaves the other's
   history untouched, which is why a single "GCP integration" source id would have been wrong: a
   mailbox outage would then look like a topology gap.
2. **The GCP feeder and the GCP backend share exactly two things** (FR-003, FR-113): one
   credential and one per-endpoint-class quota budget. They report their usage **separately**, so
   Dana can see what investigations cost as distinct from what ingestion costs.
3. **The backend is not a source and has no checkpoint** (FR-001, FR-109). It has no position in
   the event log because it writes nothing into it. A digest is handed to its caller and is gone;
   where the caller keeps one it keeps it in its own investigation record, which is feature 002's.

---

## 2. Identifier namespaces

Every namespace this feature mints in is declared, and a `Ref` outside the declared set fails
`pkg/feeder/testkit` (FR-116). Values are chosen so that **two projects with the same service name
are never the same entity** (FR-010): project and region are *in the value*, not in a property
beside it.

| namespace | value | node | why the value is shaped this way |
|---|---|---|---|
| `gcp.cloudrun.service` | `<project>/<region>/<service>` | SERVICE | FR-010: name alone is ambiguous across projects, and `checkout` in staging is not `checkout` in production |
| `gcp.cloudrun.revision` | `<project>/<region>/<service>/<revision>` | WORKLOAD | revision names are unique within a service, not globally |
| `gcp.cloudsql.instance` | `<project>/<region>/<instance>` | INFRA_RESOURCE | as above |
| `gcp.config` | `<project>/<region>/<service>@<generation>` | CONFIG | a configuration is a *version* of a service's deployed settings, so the generation is part of its identity |
| `gcp.change` | per row of §4 | CHANGE | deterministic from what the change *is*, so re-reading is a no-op (FR-076's rule, applied to the platform half) |
| `gcp.monitoring.alert_policy` | the policy identifier GCP assigns | ALERT | GCP's own identifier is already globally unique and stable across renames; the display name is not |
| `gcp.gke.cluster` | `<project>/<location>/<cluster>` | INFRA_RESOURCE | claims only; everything below it is the 001 Kubernetes feeder's (FR-030) |
| `gcp.lb` | the fully qualified resource name | INFRA_RESOURCE | P3, and omitted entirely under budget (FR-057) |
| `vendor` | the vendor slug from the allowlist | THIRD_PARTY | configuration is the authority on vendor identity, not a parsed sender domain |
| `vendor.notice` | `<vendor>/<notice-identifier>` | CHANGE | FR-076: deterministic, so the same notice read twice — or from two sources — addresses one change |

Two namespaces are deliberately **absent**. There is no GKE workload, pod or container namespace,
because those entities belong to the 001 Kubernetes feeder and a second namespace for them would
be a second entity for one workload (FR-030). And there is no namespace for anything the backend
returns, because nothing the backend returns is an entity.

### 2.1 What the feeder addresses an entity by, it also claims

FR-115 is easy to half-satisfy: a feeder mints claims for the *other* identifiers and forgets the
one it uses itself. The rule is that the addressing ref is **also** a claim, because the
resolution layer cannot merge on an identifier it was never told about. Claims per entity:

| entity | claims |
|---|---|
| Cloud Run service | the fully qualified resource name; the service name; the declared OpenTelemetry service name where the service sets one (FR-118's certain rule depends on it); environment and team labels **on the allowlist only** (FR-124) |
| Cloud Run revision | the revision name; the container image reference; the image **digest** (the identifier a pipeline also knows) |
| Cloud SQL instance | the instance connection name (FR-120's certain rule); the fully qualified resource name |
| Change (audit-derived) | the target resource name; the revision; the image digest; the pipeline or commit reference where the audit entry carries one — which is how feature 004's Vercel and GitHub Actions changes will resolve with these rather than duplicate them |
| Alert policy | the policy identifier; the display name as a *claim*, never as identity |
| GKE cluster | the cluster as the Kubernetes feeder names it, so the two views resolve into one entity |
| Vendor | the vendor slug; every host name the allowlist maps to the vendor (FR-119's certain rule) |
| Vendor notice | vendor, product, the announced window, and the vendor's own notice identifier where it states one (FR-070) |

---

## 3. Nodes

### 3.1 SERVICE — a Cloud Run service

| field | source | rule |
|---|---|---|
| `display_name` | the service name | — |
| `props.project`, `props.region` | the resource name | identifiers, never measurements (FR-126) |
| `props.environment` | the **published, configurable mapping** from project and labels | the organisation has no uniform environment label, so the mapping is configuration and the mapping *in force* is recorded in the checkpoint — otherwise a golden encodes an undeclared assumption |
| `props.traffic_split` | the service's traffic status | a set of revision names with percentages, **versioned in valid time**, never derived from observed traffic (FR-015). It is a property of the *service version*: a rollback gives the property three versions, and a query as of 14:30 reports what was serving then |
| `pointers` | §H of the spec | versioned in valid time with the node (FR-083); additive, never replacing another source's (FR-084) |
| valid start | GCP's creation instant where it states one, else **unknown** | never the poll instant (FR-022) |
| valid end | the audit-log deletion instant; else the silence rule | retracted, never deleted (FR-023) |

**The silence rule.** A service GCP no longer lists for *N* consecutive **complete** polls is
retracted with a valid end inside the last poll it *was* observed in. Two guards matter more than
the rule: a poll that failed part-way is not a complete poll and contributes nothing to the count
(FR-012), and where the audit log states a deletion instant that instant wins over the inferred
one (FR-023). The count is configuration and is recorded in the checkpoint.

### 3.2 WORKLOAD — a Cloud Run revision

Related to its service by the published "this runs that" relation. Carries the revision name, the
container image reference and the service it belongs to. Valid start is **the revision creation
instant GCP states** — this is the one place GCP is reliably explicit, so `unknown` is not
accepted here. Bounded by the **history horizon**: revisions older than the configured horizon are
not fabricated, the horizon is recorded in the checkpoint, and a query reaching past it is told it
has, so "no revision" is never confused with "we did not look that far back" (FR-024).

### 3.3 INFRA_RESOURCE — Cloud SQL instances, the GKE cluster, load balancers

Cloud SQL: instance name, project, region, database engine and version; valid start **unknown**
where GCP does not state one (FR-027). The GKE cluster: the cluster and its identity claims, and
**nothing below it** — no workload, no pod, no container (FR-030). Load balancers: P3, and the
first thing cut under budget, with the omission stated in the checkpoint as a scope statement
rather than left as silence (FR-057).

### 3.4 CONFIG — what a revision was deployed with

Environment variable **names** and value **fingerprints**, mounted configuration references, and
secret **version references**. The rule that shapes the whole node: **no secret material, and no
value the published rules classify as sensitive, is stored** — only that the entry changed and
which version it moved to (FR-034). A fingerprint answers "did this change?" without storing what
it is, which is the only question the graph needs to answer about a value it must not hold.

### 3.5 ALERT — a Cloud Monitoring alert policy

Identified by the policy identifier. Carries the display name, the severity **where one is
stated** (never inferred), each condition's filter as a pointer in Cloud Monitoring's own
vocabulary, and a link back to the policy. Linked by `WATCHES` to the entities its conditions
watch, derived from the conditions' filters and resource labels. An alert whose targets cannot be
resolved is **kept and marked unattached** and attaches when a target appears (FR-048) — which
needs plan item 5, since `internal/projector/attach.go` does this for changes only today.

**Grouping is an identity question, not a display one.** A policy that opens incidents per grouped
resource produces **one alert per alerting group**, each with its own state history naming the
policy it belongs to, and the policy-level entity is **not** reported as alerting because one group
is (FR-049, SC-005). Getting this wrong makes N alerting groups look like one incident.

### 3.6 THIRD_PARTY — an allowlisted vendor

Created on the first announcement if it does not already exist, carrying the vendor's name and its
allowlisted products, with claims for every host name the allowlist maps to it — which is what lets
it resolve with a third party another feeder already created from an observed outbound dependency
(FR-077, FR-119).

### 3.7 OWNER — a team or owner label

From label keys **on the allowlist**, with `owned-by` edges from the entities they own. The valid
start is a **bound** — the earliest instant the labelled state of the owned entity is known to have
held, which for a Cloud Run service is its `createTime` — and the node carries
`sre.gcp.owner_valid_from_is_a_bound` so that a reader does not mistake it for the instant the team
came to own the entity, which is not a fact a label carries. Where two label keys name the same
owner differently, **both claims are kept** and the resolution layer decides in the open (FR-125).

The bound rather than an unknown start is a 2026-09-21 revision of FR-125, forced by the graph's
placeholder rule: an `owned-by` edge and both of its endpoints must agree on `valid_from_unknown`,
and the service at one end has a documented `createTime`. The reasoning is recorded with FR-125.

### 3.8 CHANGE

The subject of §4.

---

## 4. Changes, and the instant each is valid at

Every change carries an **actor** (the principal GCP records) and an **actor kind** from 001's
published set. Valid time is always the instant the *source* states, never the instant the feeder
read it (FR-041, FR-006 of the temporal model). `changed-by` edges go to every resource the entry
names that the graph can resolve; a change whose target cannot be resolved is kept, marked
unattached, and attached automatically when the target appears (FR-036).

| change | kind | ref | valid at | the thing that is easy to get wrong |
|---|---|---|---|---|
| revision created | `ROLLOUT` | `gcp.change` keyed by revision + creation instant | the revision creation instant | it is marked as **not having moved traffic**, and a revision created with 0% traffic still exists and still produced this change (FR-016, FR-018) |
| traffic split changed | `ROLLOUT` | `gcp.change` keyed by service + the instant the split took effect | **the instant the new split took effect** | this is the change whose valid time is the instant *what production served* changed. It is a separate node from the creation, distinguishable by a published property, and a rollback is its own third change rather than a deletion of the second (FR-017, edge cases) |
| Cloud Run configuration changed | `CONFIG_CHANGE` | `gcp.config` generation | the configuration revision instant | names what changed and the version it moved to; never the value (FR-033, FR-034) |
| Cloud SQL settings / flag changed | `CONFIG_CHANGE` | `gcp.change` = the audit entry identifier | the audit entry instant | old and new values **only where GCP states them**; absent is absent, not guessed (FR-031) |
| Cloud SQL maintenance | `CLOUD_MAINTENANCE` | `gcp.change` keyed by instance + window | the maintenance window | actor kind `VENDOR`; and where the maintenance is **scheduled and has not yet occurred** the valid interval is the announced window and §5's future-dated semantics apply unchanged (FR-032) |
| audit-log entry describing a change | a taxonomy kind, else `CHANGE_KIND_OTHER` + GCP's operation name | `gcp.change` = the entry's unique identifier | **the instant in the entry** | an operation with no taxonomy equivalent is emitted under "other" with GCP's own operation name, **never dropped** (FR-039). `IAM_CHANGE` and `QUOTA_CHANGE` now exist in the taxonomy, so the "other" fallback no longer covers them |
| Cloud DNS record changed | `DNS_SWITCH` | `gcp.change` = the audit entry identifier | the audit entry instant | P3 (FR-056) |
| load-balancer backend changed | `CONFIG_CHANGE` | as above | as above | the superseded `exposed-via` relation is **retracted at the instant of the swap**, not deleted (FR-056) |
| vendor maintenance notice | `CLOUD_MAINTENANCE` | `vendor.notice` | **the announced window** | §5 |
| vendor deprecation | `DEPRECATION` | `vendor.notice` | the announced effective date; **unknown** where the vendor is vague | the taxonomy now has `DEPRECATION`, so the spec's "emit as other until it exists" fallback is dead and must not be used |
| vendor status-page incident | `VENDOR_INCIDENT` | `vendor.notice` | the incident window the vendor states, closed when the vendor closes it | as above for `VENDOR_INCIDENT` |

### 4.1 Actor kind is derived from a classification, never from the string

FR-020 forbids inferring actor kind from the principal string. The derivation, in order, and it
stops at the first hit:

1. the principal is on the configured **deployment-automation list** → `AUTOMATION`;
2. GCP states the caller is a **platform service agent** → `CONTROLLER`;
3. the principal is a configured **human directory** entry, or GCP states it is a user account →
   `PERSON`;
4. the change came from the vendor-notice feeder or is a provider maintenance event → `VENDOR`;
5. otherwise → `ACTOR_KIND_UNKNOWN`, **with the evidence that was available recorded**.

Two distinctions carry weight downstream. `AUTOMATION` is not `CONTROLLER`: a CI principal
deploying is a candidate *cause*, an autoscaler reacting four minutes after onset is a candidate
*effect*, and feature 002's causal ordering exonerates on exactly that difference (ADR-0005 D1).
And `ACTOR_KIND_UNSPECIFIED` is not `ACTOR_KIND_UNKNOWN`: the first means the source said nothing
and canonical serialisation omits it, the second means an actor *was* observed and could not be
classified. A feeder that writes `UNKNOWN` where it learned nothing is claiming to have looked.

### 4.2 The proposed dependency, which is not an edge

Where a Cloud Run service's configuration, connection settings or labels name a Cloud SQL
instance, a `depends-on` edge is asserted **with the evidence that derived it recorded** (FR-028).
Where the dependency cannot be derived, the feeder emits a **proposed dependency** carrying the
suggestive evidence, a score and a rationale, for a human to confirm or reject — and asserts no
edge (FR-029). It is raised once per cycle and not re-raised while pending.

This was plan item 2, and it has **landed** (2026-09-21): `ProposeDependency`, with
`ConfirmDependency` and `RejectDependency` as its two terminal decisions, projected into
`graph.proposed_dependencies` by migration 0010. Resolution could already suggest that two
*entities* are one; it could not suggest that an *edge* exists, and the two alternatives were
inventing an edge and losing the evidence — both worse than a stated gap.

Three properties are enforced rather than intended, each with a test that has been seen to fail
without it: a proposal creates **no** edge; only a confirmation does; and a proposal a person
rejected is recorded as a `conflict` when a rule proposes it again, never reopened, whatever score
the rule now carries (FR-122). A decision also requires an authenticated principal and an existing
proposal — asserting an edge on one's own authority is what `upsert_edge` is for.

---

## 5. The announced fact — this feature's first state machine

An **announced fact** is a fact whose valid time begins after its observed time. A maintenance
window read on 17 September for 02:00–04:00 on 2 October is one. 001 already accepts it
(ADR-0006 D-future) and `fixtures/announced-fact-01` already pins it; what this feature adds is
the vocabulary that keeps an announcement from being read as an occurrence.

```
                    ┌──────────────┐
   notice read ────▶ │  announced   │ ◀── default; never skipped
                    └──────┬───────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
   vendor confirms    vendor cancels     vendor reschedules
   the window         the window         the window
        │                  │                  │
        ▼                  ▼                  ▼
  ┌───────────┐      ┌───────────┐      ┌──────────────┐
  │ confirmed │      │ cancelled │      │  superseded  │──▶ a new `announced`
  └───────────┘      └───────────┘      └──────────────┘    observation of the
                                                             SAME change ref
```

Five rules, each of which is a fixture:

1. **The valid interval is the window the vendor announced**, emitted unchanged even when it
   begins after the read instant. No ingestion rule may reject one (FR-062).
2. **Observed time is never moved forward** to match an announced window. The asymmetry is
   one-directional: valid time may lead observed time; observed time is never in the future
   (FR-063).
3. **A read as of valid `T_v` and observed `T_o` returns the change** when `T_v` is inside the
   announced window and `T_o` is at or after the observation. "What is expected to change on 2
   October, as known today" is therefore an ordinary bitemporal query and not a special case
   (FR-064).
4. **A change whose valid start is after a ranked list's reference instant is not a candidate
   cause, and is excluded for that stated reason** rather than by scoring low (FR-065). 001's
   `post_reference` and two-sided decay make this true; the plan's item for it is making it
   *stated*, because an accident is not a contract.
5. **A cancellation and a reschedule are corrections, not retractions** (FR-067, FR-068). The
   observed interval of the announcement closes and a new observation opens; the valid interval is
   never rewritten; both windows stay recoverable by an observed-time query; and **no second change
   node is created**, because the ref is deterministic from the vendor and the notice identifier.

Two further rules have no arrow on the diagram because they are about *not* transitioning. An
announced window that passes in silence stays `announced` **for ever** — never silently promoted to
something known to have happened (FR-066). And a vendor who states a window only vaguely gets a
valid start marked **unknown**, with the vendor's own wording **not** stored in place of a
timestamp (FR-069) — which is also the point at which FR-071's "no free text" and FR-069's
"don't guess" agree rather than conflict.

---

## 6. The alert state history — the second state machine

States are 001's published vocabulary (`ok`, `warn`, `alert`, `no_data`, `declared`). Every
transition is an `alert.transition` event whose valid time is **the transition instant Google
reports**, never the instant the feeder learned of it, and whose idempotency key is 001's derived
4-tuple `(source, stable alert identifier, group, transition instant)` — source `gcp`, the policy
identifier, the policy's group, and Google's instant.

That derived key is what makes the rest safe:

- **Two transports, one transition.** A doorbell-triggered poll and the next scheduled poll
  produce one event, and the result does not depend on which arrived first (FR-050, SC-004).
- **Two intakes, one trigger.** A GCP transition and a human declaration through feature 002's
  Slack transport converge on the same event shape and the same convention, so the engine sees one
  investigation trigger rather than two competing shapes (FR-046, FR-047).
- **A forged doorbell changes nothing.** Its body is never read as data, it costs at most one extra
  poll inside the configured rate limit, and it can create, alter or retract no alert, no state
  version and no event (FR-050).

Three properties of the history itself:

| property | rule |
|---|---|
| **sampled** | polling is the source of truth, so every GCP alert history is *sampled at the poll interval* and must say so, or a consumer cannot tell a quiet policy from an unwatched one (FR-051). This is plan item 3; until it lands the history is correct but silent about being sampled |
| **closures are evidence** | a closing transition is emitted and kept. It is never discarded as uninteresting (FR-052) |
| **suppression is stated** | flapping and no-data transitions do not trigger an investigation, are **still recorded** in the state history, and the suppression is stated rather than applied silently (FR-052) |

---

## 7. What is never an entity, a property or a field

The list is short and every line is enforced rather than documented:

| never stored | where the rule lives |
|---|---|
| a metric sample, a log line, a span, or any aggregate of them | FR-109, SC-013; 001's telemetry-payload validation, exercised by `gcp-telemetry-rejection-01` |
| an announcement **body**, sender or recipient address, display name, verbatim subject, quoted thread or attachment — anywhere, at any point, **including on a failed or aborted run** | FR-071, FR-072, SC-010 |
| secret material, or the value of any configuration entry the published rules classify as sensitive | FR-034 |
| a label value that measures something | FR-126 — label values are identifiers and names, never measurements |
| a label whose key is not on the allowlist | FR-124 — not as a property, not as a node, not as a claim |
| anything from the Cloud Audit Logs **data access** stream | FR-042 — not read in v1, and **no permission requested** for it |
| a hashed people identifier | FR-135, SC-019 — people identifiers are **dropped**, never hashed, because a hashed principal address is still personal data |
| a digest, or any part of one | FR-109 |

The last two are the ones a careful implementation still gets wrong. A hashed email looks like
compliance and is not, which is why the scan asserts zero people identifiers *in any form, hashed
included*. And a digest is tempting to cache as a property on the node it is about — which is
precisely the thing constitution IV exists to forbid.

---

## 8. Sanitisation classes

The contract is [`contracts/sanitisation.md`](./contracts/sanitisation.md); this is its shape.
Every field and every label key is assigned **exactly one** of three dispositions, and a field
with no assignment fails the commit rather than defaulting to either safety or convenience:

| disposition | applies to | mechanism |
|---|---|---|
| **recorded verbatim** | an explicit allowlist only | none |
| **keyed pseudonym** | infrastructure identifiers: project, service, revision, instance, cluster, namespace, host, team, environment | keyed HMAC, **consistent across the whole corpus**, so relationships and the digest join keys survive — a digest whose keys do not join is evidence about nothing |
| **dropped** | people identifiers; free-text fields; message bodies | dropped, never hashed |

Three properties of the mechanism, rather than of the policy:

1. It runs **in the connector, before anything touches disk** (FR-137). No unsanitised payload and
   no announcement body reaches disk, a log or any artifact at any point, including during a failed
   or aborted run — which is why this is a code path and not a commit hook.
2. It runs in **live** mode too (FR-110). A live investigation cannot surface what a recording
   would not be allowed to keep, so log content is reduced to masked templates before it leaves the
   process whether or not anything is being recorded. The same boundary governs what may be sent to
   a **model provider** (FR-141).
3. The **scan is not the sanitiser** (FR-138). An independent secrets-and-entropy scan and a
   personal-data scan run at commit, implemented separately, so one defect cannot both leak and
   pass. **Canary tokens** are seeded into the source data before a campaign and asserted absent; a
   surviving canary **fails the commit** rather than raising a warning (FR-139, SC-018).

And neither the HMAC key nor any mapping table is ever committed (FR-135).

---

## 9. Join keys: the published mapping

FR-102 names four keys. 002's `JoinKeys` message already has fields for all of them, so this is a
**mapping to publish**, not a schema change:

| FR-102 key | `JoinKeys` field | GCP value |
|---|---|---|
| the Cloud Run **revision** | `version` | the revision name, matching the `gcp.cloudrun.revision` value's last segment, so a digest row joins a WORKLOAD node |
| the **instance identifier** | `pod_or_host` | the Cloud SQL instance connection name — the series' "host" |
| the **trace identifier** | `trace_ids` | empty in this organisation; populated without a contract change if Cloud Trace is ever enabled |
| the **first-seen instant** | `first_seen` | per template, error kind or series |

`workload` carries the Cloud Run **service**, which is the entity a series is usually about.
Sanitisation **pseudonymises all of these rather than dropping them** (FR-102, FR-135) — the one
place where the keyed HMAC's corpus-consistency is load-bearing rather than convenient.
