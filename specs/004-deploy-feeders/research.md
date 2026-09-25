# Phase 0 research — Deploy Feeders (GitHub and Vercel)

**Date**: 2026-09-22 · **Plan**: [plan.md](./plan.md) · **Spec**: [spec.md](./spec.md)

Two kinds of finding are recorded here and they are not interchangeable. **§1 is read out of this
repository's own code** — what feature 001 has actually shipped, checked file by file, against what
the specification (written 2026-09-17, before feature 003 merged) assumed it would need. **§2 and §3
are read out of the platforms' published documentation**, cited per fact. Where a fact could not be
established from either, it is listed in §5 as open, with the question it is open on, rather than
guessed at.

---

## 1. What feature 001 still owes, checked against the merged code

The specification's "Dependencies on feature 001" lists six items. The answer differs for each.

### 1.1 Actor kind — **delivered, and the spec's fallback is now dead**

`api/sreagent/graph/v1/graph.proto` defines `ActorKind`: `PERSON`, `AUTOMATION`, `CONTROLLER`,
`VENDOR`, `UNKNOWN`, with `ACTOR_KIND_UNSPECIFIED = 0` deliberately distinct from `UNKNOWN`
("the source said nothing" versus "something the feeder cannot type"). ADR-0005 D1 settled it and
feature 003 consumes it.

**Decision**: use the enum directly. The spec's stated fallback — "feeders record the actor and hold
the kind in a declared property" — **must not be implemented**; it would mint a second, private
spelling of a field that exists, which is the drift constitution IX exists to prevent.

### 1.2 The claim-namespace registry — **absent, and it is the load-bearing gap**

`pkg/feeder/ref.go` registers Kubernetes- and OpenTelemetry-shaped namespaces (`NSServerAddress`,
`NSOTelService`, `k8s.*` and so on). **None of the seven this feature needs exists**:
`deploy.commit_sha`, `deploy.image`, `deploy.release`, `github.repo`, `github.change`,
`vercel.project`, `vercel.change`.

**Decision**: register all seven in `pkg/feeder`, each **with its value normalisation published
beside it**, as an additive change to feature 001.

**Rationale, from this repository's own history.** Feature 003's C4 rule compared an OpenTelemetry
service claim with a Cloud Run revision claim and merged the wrong entity, and separately was
order-dependent; both defects were invisible until a cross-source fixture existed. A registry that
names a namespace but does not fix its *value form* produces exactly that class of defect: two
sources that both "state the commit" but spell it `a1b2c3d` and `A1B2C3D4E5...` never meet, and the
certain rule that was supposed to join them reports nothing rather than an error. FR-041's
"unabbreviated and case-normalised" and FR-046's "byte-identical claim values" are the same
requirement seen from two ends, and they belong in the registry, not in each connector.

### 1.3 A certain rule for a shared deploy identifier — **absent; the next free id is C8**

The published certain rules are C1–C3 (`internal/resolution/certain.go`), C4/C5/C7
(`internal/resolution/gcp.go`) and C6 (`internal/resolution/vendor.go`, the vendor allowlist).
**No rule says "two change observations stating the same `deploy.commit_sha` for the same target in
the same environment are one change"** — which is the rule SC-004 rests on.

**Decision**: publish **C8** in `internal/resolution/deploy.go`, beside the other rules rather than
inside either connector.

**The conditions the spec requires it to settle** — "including what happens when the target agrees
and the commit does not, and vice versa":

| target | deploy identifier | verdict |
|---|---|---|
| agrees | agrees | **merge**, certain |
| agrees | disagrees | **do not merge.** Two different commits shipped to one target is two rollouts, which is the ordinary case of a redeploy. Merging here would collapse a rollout with the one it replaced — the exact failure 003's C4 made. |
| disagrees | agrees | **do not merge; suggest.** One commit shipped to two targets is the monorepo case, and FR-017 already says that is N changes, not one. |
| unknown either side | — | **do not merge.** A missing claim is not agreement (FR-041: a claim is omitted rather than invented). |

**Alternative rejected**: merging on the commit alone. A monorepo run deploys one commit to several
services; a rule keyed on the commit alone would merge every service in the run into one entity,
automatically and with a score of 1.0. That is FR-017's case arriving through the resolution layer
instead of the feeder.

#### 1.3.1 What Cloud Run can actually supply as C8's platform side (T135)

Checked against the vendored API surface — `cloud.google.com/go/run` v1.22.0 — rather than assumed,
because C8 is only as good as the claims both sides can mint:

| need | field | verdict |
|---|---|---|
| the commit a revision was built from | — | **absent.** `Revision` has no commit field. `Service.buildConfig` is the Cloud Run *functions* source-deploy path and its `sourceLocation` is a Cloud Storage bucket URI, not a git reference |
| the resolved image digest | `ContainerStatus.imageDigest` | **not on a revision.** It hangs off `Instance`. A `Revision` carries only `Containers[].Image`, which holds whatever the deployer wrote |
| anything else identifying a build | `Labels`, `Annotations` | free-form, "may be set by external tools" |

**Consequences, and they shape T135.**

- **`deploy.image` is sound but conditional**: available whenever the deployer pinned a digest, absent
  when they deployed a tag. A tag is never claimed — two rollouts running `:latest` are not one
  rollout, and claiming it would give C8 a false merge.
- **`deploy.commit_sha` can only come from a label**, so it is **operator configuration**
  (`commit_from_labels` in `config/gcp.yaml`, default `commit-sha`) and not a platform fact. A value
  that is not a full hex object id mints no claim, so a wrong key costs a missing join rather than a
  wrong merge.
- **The GitHub↔Cloud Run join rests entirely on that label.** A GitHub deployment states a commit and
  no image digest; Cloud Run states an image and no commit. They share nothing else. By contrast
  GitHub↔Vercel share the commit unconditionally, both as first-class fields (§3.2) — so US2's
  cross-source case is the robust one and US1's needs the operator's tooling to cooperate.

### 1.4 A rollback marker on a change — **absent, and the near-miss is worth naming**

`grep` finds `rollback_candidate` in `api/sreagent/investigation/v1/investigation.proto`, but it is
a **different concept**: the investigation engine's *suggested* rollback target — what an operator
might do next — not an observation that a rollback happened. Nothing in `graph.proto` marks a change
as one.

**Decision**: a published marker on the change in feature 001, per the spec's "a decision for 001,
not for a connector". Preference: a boolean plus the restored deployment's identifier, rather than a
distinct change kind, because a rollback **is** a rollout in every other respect and a separate kind
would make every consumer that ranks rollouts remember to include it.

### 1.5 "Unattached" and automatic attachment — **partly delivered**

`graph.proto` carries `RankedChange.unattached` and `RankedChange.target_entity_ids`, so the
**marker** exists on the read side. What is not established is the **attachment convention**: when a
target appears later, what adopts the change, and within what bound. SC-018 requires attachment
within one polling interval of the target appearing.

**Open** — see §5.1. This is the item most likely to be a smaller change than the spec assumed.

### 1.6 Pointer vocabularies for GitHub and Vercel — **absent**

`pkg/feeder/pointer.go` registers `otel-semconv/1.30`, `k8s-resource/v1`, `gcp-monitoring-filter/v3`,
`gcp-logging-query/v2` and `gcp-trace-filter/v1` (the last registered and deliberately unminted).
Neither platform has an entry.

**Decision**: register `github-resource/v1` and `vercel-resource/v1`, each with the published
statement of why its selector cannot be expressed in OpenTelemetry semantic conventions
(constitution IV). The argument is the one `k8s-resource/v1` already makes and that
`docs/schema/pointers.md` records for the GCP vocabularies: these selectors address an **object by
path** (a run, a deployment, a release), not an entity by attribute equality, and flattening a path
into attribute equalities produces a selector that stops matching the moment the platform adds a
path segment.

---

## 2. GitHub — established from the published documentation

| fact | finding | source |
|---|---|---|
| Deployment status states | `success`, `failure`, `error`, `inactive`, `in_progress`, `queued`, `pending` — **seven**. The documentation does **not** formally designate which are terminal | REST: deployments/statuses |
| Listing statuses | `GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses` | ibid. |
| The installation's repository selection | `GET /installation/repositories`, returning `total_count`, `repositories[]` **and `repository_selection`**; paginated, `per_page` max 100 | REST: apps/installations |
| Remaining quota | `GET /rate_limit` returns `limit`, `remaining`, `reset`, `used` per resource family and **does not itself count against the limit**. The `x-ratelimit-*` response headers are documented as "the authoritative source for the remaining allowance in the current window" | REST: rate-limit |

**Decision — the terminal-state set is ours to publish, because GitHub does not.** FR-022 needs
"terminal successful", and the documentation declines to say which states are terminal. So this
feature publishes the mapping rather than inferring it per call site: **`success` is the only state
that produces a ROLLOUT.** `failure` and `error` are terminal and are recorded as a failed attempt
only where the platform states one (Edge case 4) — never as a rollout. `inactive` is terminal but
means *superseded*, and FR-022 already requires it be recorded as a property of the same change
rather than a new one. `in_progress`, `queued` and `pending` produce nothing.

**Decision — the scope enumeration is `GET /installation/repositories`, and `repository_selection`
is reported.** This is what makes FR-008's clarified answer implementable: the feeder asks the
platform which repositories the installation may see rather than holding a list beside it. The
`repository_selection` field distinguishes "all repositories in the org" from "selected", which is
precisely FR-008's "the feeder MUST say which of the two regimes it is operating under".

**Decision — budget reads the headers, not the endpoint.** FR-071 wants the budget as a share of
*remaining* quota. `GET /rate_limit` is free and gives the whole picture, so it is right for the
cycle's opening reading and for the usage report; the `x-ratelimit-*` headers are authoritative
per-request and are what the in-cycle budget decrements against. Using only the endpoint would let a
cycle overspend between readings; using only the headers loses the other resource families.

---

## 3. Vercel — established from the published documentation

`GET /v7/deployments`, listing "deployments under the authenticated user or team".

### 3.1 The finding that changes US2's implementation: `readySubstate`

A deployment carries **both** `readyState`/`state` — `BLOCKED`, `BUILDING`, `CANCELED`, `DELETED`,
`ERROR`, `INITIALIZING`, `QUEUED`, `READY` — **and**, when `READY`, a `readySubstate` documented as
tracking "whether or not deployment has seen production traffic":

- `STAGED` — never seen production traffic
- `ROLLING` — in the process of gradually transitioning production traffic
- `PROMOTED` — has seen production traffic

**Decision: `state=READY` alone MUST NOT produce a rollout.** US2's acceptance is that valid time is
"the instant it became the production deployment", and `READY` means *built and available*, not
*serving*. A deployment can be `READY` + `STAGED` and never have taken traffic. The rollout condition
is `target=production` **and** `readySubstate=PROMOTED`.

This is exactly US2 acceptance scenario 5 (build-then-promote) generalised: Vercel states the
distinction, so the feeder reads it rather than assuming that ready means live. Had this been missed,
every staged deployment would have become a rollout at its build instant — a change node claiming
production moved when it had not, which is the confident-wrong-answer class this project keeps
finding.

### 3.2 The rest, as published

| need | field | note |
|---|---|---|
| environment filter (FR-032) | `target`: `production` \| `staging` \| `null`; query param `target` | preview deployments have no `production` target, so the filter is a platform-stated value rather than a heuristic |
| actor (FR-013, FR-037) | `creator.type` (e.g. `"app"`), `creator.username`, `creator.githubLogin`, `creator.uid` | **`creator.type` is the actor-kind evidence**, satisfying "the account type MUST come from the payload, never from the login string" |
| how it was triggered | `source`: `git`, `cli`, `redeploy`, `api-trigger-git-deploy`, `git-deploy-hook`, `import`, `drop`, `clone/repo`, `import/repo`, `v0-web` | corroborates actor kind: `git` behind a person's push is `PERSON`; `cli` inside CI is `AUTOMATION` |
| the commit (FR-041) | `meta` ("metadata information from the Git provider") and `attribution.commitMeta`; the list endpoint also accepts a `sha` query parameter | Vercel indexes by SHA, which is what makes the GitHub↔Vercel join a real one |
| rollback (FR-016) | `isRollbackCandidate`, and a `rollbackCandidate` query filter | **candidacy is not a rollback.** It says a deployment *can* be instantly rolled back to, not that one happened. See §5.2 |
| timestamps | `createdAt`, `created`, `buildingAt`, `ready` | `ready` is "when the deployment got ready" — see §5.2 |
| origin reference (FR-014) | `inspectorUrl` | carries no credential |
| project (FR-034) | `projectId`; query params `projectId`, `projectIds` | |
| pagination | `pagination.next`/`prev` as timestamps, plus `since`/`until` | a time-windowed poll is natural, which suits FR-057's checkpointed extent |

### 3.3 Scope: Vercel has access groups, so the regime question has an answer

FR-008's clarification left open "which regime Vercel falls under: a per-target grant to enumerate,
or configuration within the credential". The API reference lists **access groups** with
`GET /v1/access-groups/{idOrName}/projects` and
`GET /v1/access-groups` ("list access groups for a team, project or member").

**The provisional decision was WRONG, and §5.3 is now closed against the API reference (T034).**

`POST /v3/user/tokens` accepts a request-body field `projectId`, documented as *"The ID of the project
to scope this token to"*. So a Vercel token **can** be pinned by the platform, and the provisional
"configuration within the grant, the project filter is only a query parameter" is not the whole truth.

Three things follow, and they change what the connector declares:

1. **The scoping is singular.** `projectId`, not `projectIds`: one project per token. An estate with
   several projects is therefore either several tokens or one unscoped token — the platform offers no
   middle term, unlike GitHub's repository *selection*.
2. **Vercel is therefore BOTH regimes, depending on how the operator made the token.** A
   project-scoped token is a boundary the platform enforces; an unscoped user or team token is
   configuration within the grant, exactly as §3.3 first said. So the feeder must **report which one
   it is running under**, the way the GitHub side reports `repository_selection` — and must not state
   either regime as a property of the platform.
3. **A token's scope is reported back.** `token.scopes[]` appears in the creation response and in
   `GET /v6/user/tokens` ("Retrieve a list of the current User's authentication tokens"), each scope
   item carrying `createdAt`, `expiresAt`, `origin`, `sudo` and `type` — the documented example shows
   `"type": "user"`. The reference does not enumerate the `type` values and shows no project-scoped
   example, so **the spelling a project scope takes is not established** and must not be guessed;
   matching the authenticating token in that list is by `prefix`/`suffix`, which is workable rather
   than clean.

**What is still absent: any write-capability introspection.** Nothing reports "this token may not
write". `GET /v2/user` has a `limited` variant meaning the token lacks privileges to read full user
data, which is a signal about privileges and not a permission list. So FR-003's fallback stands for
Vercel — *"where the platform cannot report one, require the operator to assert read-only rather than
assume"* — and the assertion is the operator's, recorded in the checkpoint.

A read-only way to *verify* a declared project scope does exist and is worth taking: read one project
outside the declared scope and require a 403. It spends one call, it issues only a published read, and
it turns the operator's assertion into something checked rather than trusted.

---

## 4. Decisions carried from feature 003 rather than re-derived

These are settled by precedent in this repository; the research task was to confirm they transfer.

| decision | precedent | transfers? |
|---|---|---|
| The published read-only operation list is a Go table, and one `Issue`-style door is the only way to spend a call | `internal/gcpx/requestlog.go` (003 T174) | **Yes**, and it is how SC-007's "verifiable from the set of operations the feeder can issue, not only its behaviour on one run" is met structurally rather than by review |
| The doorbell is the only state change, verified by a go/ast scan that every live reader issues a published operation first | `internal/feeders/gcp/readonly_test.go` | **Yes.** Neither platform needs even that exception: nothing here acknowledges anything, so the published list should be **all reads, no exceptions** |
| Sanitisation runs in the connector before disk, with the campaign gates (`record`, `sanitise`, `scan`, `sign`, `verify`, `parity`) | `internal/cli/fixture_campaign.go`, `internal/sanitise/` | **Yes**, unchanged. FR-059–FR-070 are 003's FR-129–FR-143 with the platform names swapped, so this is configuration and fixtures, not new code |
| A cross-source fixture is the acceptance test for a certain rule, not a late verification | `fixtures/gcp-cross-source-merge-01` and the two C4 defects it exposed | **Yes**, and §1.3 makes it C8's acceptance test |
| The metric gate refuses a labelled pair whose reference the graph never saw | `scripts/check-report.sh` (2026-09-22) | **Yes** — already in place, so 004's fixtures inherit it |

---

## 5. Still open, with the question each is open on

Listed rather than guessed. Each is small and none blocks starting the contract work in §1.

**5.1 — The attachment convention (spec dependency 5). ANSWERED, from the code (T029).**

Feature 001 already adopts, and the convention is stronger than SC-018 asks for. It is
`internal/projector/attach.go`:

- **What adopts.** `attachWaiting` runs **whenever an entity is created** — the two places entities
  are created are `refs.go`'s `createEntity` and `split_entity.go`, and it is called from one entry
  point so a future node kind that can dangle cannot be added at only one of them. It finds every
  change whose unattached-targets list names the ref, creates the `changed_by` edge and removes the
  target from the list.
- **The bound.** Not a polling interval: **the same transaction**. An unattached change is adopted in
  the transaction that creates its target, so there is no window in which the target exists and the
  edge does not. SC-018's "within one polling interval" is satisfied with room to spare, and the
  interval that actually matters is how long until the *target* is observed — which is the feeder's
  cadence, not the graph's.
- **The provenance.** The edge records **both** halves: the event that carried the change and the
  event that finally created the node (FR-034), so an operator asking why the edge exists sees the
  whole story rather than the later half.
- **Not on claim arrival as such.** The trigger is entity *creation*, not claim storage. A claim
  whose subject is a ref nothing has described creates the entity and so triggers adoption; a claim
  on a ref that already exists does not, and does not need to — the change is already attached.

**What was genuinely missing, and is now T137.** Adoption created the edge but did **not** re-run the
resolution rules, so a rule whose answer depends on what a change is attached to — C8 — kept the
answer it gave when the target did not exist. That is fixed by the deferred re-evaluation queue; see
[contracts/deploy-claims.md](./contracts/deploy-claims.md) §2.1.1. Alerts take the same adoption path
and needed no such thing, because no rule keys on what an alert watches.

**5.2 — Vercel's promotion instant.** `readySubstate=PROMOTED` is a **state**, not a timestamp, and
the documented timestamps are `createdAt`, `buildingAt` and `ready`. US2 scenario 5 requires valid
time to be the promotion instant with the build instant kept as a property. *Question*: does any
endpoint state when promotion happened — a deployment-events or aliases endpoint — or must the
feeder fall back to FR-012's "mark the valid start **unknown** rather than guess"? The fallback is
already specified, so this changes fidelity, not correctness.

**5.3 — Whether a Vercel token can be scoped to projects. ANSWERED, and the provisional answer was
wrong (T034).** It can: `POST /v3/user/tokens` takes `projectId`, *"The ID of the project to scope this
token to"*. Singular, so one project per token. Vercel is therefore **both** of FR-008's regimes
depending on how the operator created the token, and the feeder reports which rather than declaring
one. See §3.3. Two things remain unestablished and are not to be guessed: the spelling a project scope
takes in `token.scopes[].type`, and any endpoint reporting write capability — there is none, so
FR-003's operator-assertion fallback stands.

**5.4 — Whether GitHub states a rollback at all.** The spec already assumes not ("most GitHub
rollbacks appear as ordinary rollouts of an earlier commit, and the spec accepts that rather than
inferring"). Vercel's `isRollbackCandidate` is candidacy, not history. *Question*: is there any
platform-stated "this deployment was rolled back to" on either side? If neither states it, US5
degrades to what FR-016 already permits, and SC-009 is satisfied by the negative fixture — a redeploy
of an older commit that is **not** flagged.

**ANSWERED (004 T121): Vercel states one; GitHub does not.** Vercel's single-project read,
`GET /v9/projects/{idOrName}`, publishes `lastAliasRequest`: `type` (`promote` | `rollback`),
`jobStatus` (`succeeded` | `failed` | `pending` | `in-progress` | `skipped`), `fromDeploymentId`,
`toDeploymentId` and `requestedAt`, checked against the API reference. The project **list** endpoint's
published schema does not carry it, so the connector reads it from the single-project read, which was
already on the published operation list (`OpProject`). GitHub's deployment and status objects state no
rollback, so a GitHub rollback stays an ordinary rollout of an earlier commit, and SC-009's negative case
covers it. A rollback is flagged only on `type = rollback` with `jobStatus = succeeded`. A `promote` is
never read as one, even of an older deployment.

---

## 6. What this phase changes about the plan

1. **Four contract items, not six.** Actor kind is done; "unattached" is half done. The contract
   phase is smaller than the specification implied, and the plan's §Phase 0 table is the record.
2. **C8 is the critical path.** SC-004 — the feature's headline property — cannot be met without it,
   and its four-way condition table (§1.3) is a design decision, not an implementation detail.
3. **Vercel's rollout condition is `readySubstate=PROMOTED`, not `state=READY`.** This lands in the
   data model and in US2's fixtures, and it is the difference between a change node that says
   production moved and one that says a build finished.
4. **GitHub's terminal-state mapping is ours to publish**, because GitHub does not designate one.
