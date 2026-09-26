# Entity resolution

> Status: published with feature 001 (`schema/v1`). Requirements: FR-031, FR-033, FR-036–FR-041a.
> Decisions: ADR-0001 D6 and D7. Research: `specs/001-temporal-graph-core/research.md` §10.

A pod, a telemetry service name, a repository, a Terraform resource and a Slack channel may be
the same entity under five names. Working out that they are is the hard problem of this project,
and constitution VI says it must be done in the open: **every identifier is stored as a claim
before any merge, every merge records the rule and the score and a human-readable reason, every
merge and split is reversible, and a human decision outranks every rule for ever.**

This document is the published contract for all of that: the rules, their scores, the
normalization they use, the precedence between them, the states a suggestion moves through, how
the audit query answers "as of when", what a human decision's event id is, and what the
calibration report measures.

---

## 1. Claims

Every identifier a source reports is stored in `graph.identity_claims` **before** any merge
decision is taken (FR-036). A claim is:

| field | meaning |
|---|---|
| `claim_id` | `sha256(namespace, value, source_id)`, so re-asserting a claim is the same claim |
| `entity_id` | the entity it currently points at, after merge redirects |
| `namespace`, `value` | the identifier itself, e.g. `otel.service.name` = `checkout` |
| `attributes` | the supporting facts the source shipped with it |
| `source_id`, `event_id`, `observed_at` | who said it, in which event, and when the graph learned it |

A claim is unique per `(namespace, value, source_id)`: one source re-asserting an identifier is
one opinion, whichever event carried it.

That uniqueness is the whole point of the table, and it is also its boundary: an identifier names
**one** entity. A value several entities share is a different kind of fact, and it lives in
`graph.correlation_keys` — §1a.

## 1a. Correlation keys

> Added by feature 004 (T148).

A **correlation key** is a value several entities share: a commit, an image digest, a release tag, a
repository, a trace id, a build number. It answers "what does this thing have in common with others",
where a claim answers "what is this thing called".

| field | meaning |
|---|---|
| `correlation_id` | `sha256(entity_id, namespace, value, source_id)` — the entity is part of it, which is exactly what a claim id may not carry |
| `entity_id` | the entity the key is on, after merge redirects |
| `namespace`, `value` | the shared value, e.g. `deploy.commit_sha` = `8f5b…` |
| `attributes` | what the source shipped with it, and what a rule **corroborates** with |
| `source_id`, `event_id`, `observed_at` | who said it, in which event, and when the graph learned it |

Unique per `(entity_id, namespace, value, source_id)`: many entities may carry one key, and one entity
may not carry the same key from one source twice — so a redelivered event is still a no-op.

### Which of the two kinds a value is

Ask whether the value **names** the entity or **describes** it. `otel.service.name=checkout` names a
service: two sources saying it are naming one thing, and only one thing can be called that. A commit
describes a rollout: a monorepo run ships one commit to three services and a redeploy ships it again,
so three changes legitimately carry it and none of them owns it.

The published correlation namespaces are `deploy.commit_sha`, `deploy.image` and `deploy.release`
(`internal/graph/correlation.go`). The event log **refuses** an identity claim in any of
them, with reason code `correlation_as_identity`: getting this wrong in the identity direction is the
expensive mistake, because `graph.identity_claims` would hold the value for whichever entity the
projector reached first and the graph's state would then depend on the order events arrived in. It does
**not** refuse a correlation in a namespace the registry has never heard of: a new connector correlates
on things this schema does not know, and a registry that had to be edited before a feeder could ship
would make the list a bottleneck rather than a vocabulary. The asymmetry is deliberate — a correlation
stored as a name corrupts resolution, while a name stored as a correlation merely fails to merge.

### A correlation never merges anything by itself

It is the first half of "correlated, then corroborated". **C8** merges two rollouts sharing a deploy
key only where they also agree on an environment *and* already share a target the graph itself
resolved. That is why nothing about this table resembles `merged_into`, and why a rule that reads it
declares `EvalCorrelation` rather than `Eval`: a rule's trigger is part of what it is, and `Register`
refuses a rule that declares both or neither.

### Why `github.repo` is not one of them

The refusal sees a namespace, not a subject. So a namespace belongs on this list only if its values name
**nothing** — if no entity anywhere could legitimately be addressed by one. A commit, an image digest
and a release tag are of that kind.

`acme/storefront` is not: it names exactly one repository, and the deploy feeders address the *target*
of a rollout by it, which mints an entity — and every entity gets a primary identity claim in the
namespace that addressed it. A registry holding `github.repo` would promise a refusal the projector
itself breaks.

Its danger is a wrong **subject** rather than a wrong namespace: claimed on a change it is shared by
every change the repository ships, and claimed on a service by every service in a monorepo. That is what
the C1 narrowing below guards, and no feeder emits it as a claim at all — the GitHub connector carries
the repository as a *property* of the change.

### What this replaced

`deploy.commit_sha`, `deploy.image` and `deploy.release` were identity claims first, and so was
`github.repo`. The fixture corpus is what found it: four of the seven feature-004 US1 fixtures failed
their shuffle step (FR-048), because `github.repo` and `deploy.commit_sha` were emitted on every change
a deployment produced and only the first one could hold them. Narrowing C1 away from the namespaces
(§"C1 does not fire on every namespace") stopped the wrong *merge*; it did not stop the wrong
*storage*.

---

### Identifier namespaces the rules compare

| namespace | shape | emitted by |
|---|---|---|
| `otel.service.name` | the OpenTelemetry `service.name` | the topology feeder (observed), and the Kubernetes feeder when a label or annotation *declares* one |
| `k8s.deployment` | `<namespace>/<name>` | the Kubernetes feeder |

### Claim attributes the rules read

| key | meaning |
|---|---|
| `k8s.namespace.name` | the Kubernetes namespace a workload lives in |
| `k8s.deployment.name` | the Deployment name carried on OpenTelemetry resource attributes |
| `service.namespace` | the OpenTelemetry `service.namespace` |
| `deployment.environment.name` | the environment; `checkout` in staging is not `checkout` in production |
| `sre.k8s.claim_key` | the label or annotation a declared service name was read from |

The label and annotation keys a workload may declare its service name in are configurable; the
defaults are `app.kubernetes.io/name` and `resource.opentelemetry.io/service.name`.

---

## 2. The published rules

A rule is **certain** or **probable** (FR-037). *Only a certain rule may cause an automated
merge.* A probable rule may only produce a suggestion — that is ADR-0001 D6, and SC-007 states it
as a number: zero automated merges from probable rules, measured on every fixture.

### Certain rules (auto-merge, score 1.0)

| id | what it requires | why it is certain |
|---|---|---|
| **C1** | two *different sources* assert the same `(namespace, value)` | the same identifier in the same namespace denotes one entity; one source saying it twice is not corroboration |
| **C2** | a Kubernetes workload declares an OpenTelemetry service name in a configured label or annotation, that name is claimed by an observed service, `k8s.namespace.name` = `service.namespace`, and both state the **same environment** | somebody configured it to be true |
| **C3** | a service's spans carry `k8s.deployment.name` + `k8s.namespace.name` naming a Kubernetes workload the graph knows | the instrumentation is running inside that workload |
| **C4** | an observed service's attributes name the Cloud Run revision it runs in — revision name **and** project **and** region | a revision name is unique within a service, not globally, so all three are required or the rule merges across projects |
| **C5** | a Cloud Run service declares an OpenTelemetry service name, an observed service claims it, and both state the same environment | somebody configured it to be true |
| **C6** | the vendor allowlist maps a vendor to host names, and an observed outbound dependency's server address is one of them | somebody wrote that host name next to that vendor; it rests on a configured assertion and not on a resemblance |
| **C7** | two sources claim the same Cloud SQL instance connection name — the instance as GCP lists it, and the address a caller reached it at | the connection name is globally unique and is the string every client is configured with |
| **C8** | two change observations from **different sources** state the same deploy identifier (`deploy.commit_sha`, or `deploy.image` by digest) for a target they **share**, in the same environment | the identifier is one both platforms quote rather than resemble, and the shared target is a merge resolution already made |

Each requires an assertion that is only true because someone configured it — never a
resemblance. A missing environment on either side of C2 is **not** a match: a certain rule may
not gamble on `checkout` in staging being `checkout` in production.

When several certain rules fire on one pair, the most **specific** one is recorded as the reason:
C2 (30) = C5 (30) > C4 (25) = C6 (25) > C3 (20) > C7 (15) = C8 (15) > C1 (10). "This workload declares this service name"
is a fact an operator can check; C3's resource attributes are a consequence of it.

### C1 does not fire on every namespace

C1's premise — "the same identifier in the same namespace denotes one entity" — holds for a namespace
whose values **name** a thing. It does not hold for one whose value is a fact several entities share,
and `IdentifyingNamespace` in `internal/resolution` is the published list of the exceptions:

| namespace | why equality is not identity |
|---|---|
| `deploy.commit_sha` | a monorepo run ships one commit to several services, so every change in the release shares the value |
| `deploy.image` | the same image runs in staging and in production, and a redeploy of one digest is a second rollout |
| `deploy.release` | `v2.3.0` is a tag many repositories use in the same week |
| `github.repo` | a repository is a claim on the service it ships and never a node, so in a monorepo two services share it |

Without that guard, registering the deploy vocabulary would have made C1 merge every change in a
monorepo release — certainly, with a score of 1.0 and no human in the loop. The two `deploy.*`
identifiers still do work: **C8** keys on them, with the agreement on a shared target that C1 has no
way to require. `github.change`, `vercel.change` and `vercel.project` each name one thing and stay
identifying.

Since feature 004 T148 the three `deploy.*` namespaces are **correlation keys** (§1a) and the event log
refuses a claim in any of them, so C1 should never see one. `github.repo` is not on that list — §1a says
why — so for it this guard is the only one there is. It stays for the deploy namespaces too, for two
reasons. The log is
append-only, so a database replaying events appended before that refusal existed still has such claims
to project. And a guard that exists only at the front door stops applying the moment anything writes to
the projection by another route.

### Probable rules (suggest only)

| id | what it requires | score |
|---|---|---|
| **P1** | normalized-name equality across a Kubernetes name and an observed telemetry service name | **0.70** |
| **P3** | P1, and both entities have an `owned_by` edge to the same owner entity | **0.80** |
| **P2** | P1, and both sides state the **same** `deployment.environment.name` | **0.85** |
| **P4** | a Cloud SQL instance's name appears inside the name of an environment variable a Cloud Run service defines | **0.60** |
| **P5** | two entities share an owning team and their names are similar after normalisation | **0.40** |
| **P6** | two announcements name the same allowlisted vendor and product, state windows that are neither identical nor disjoint, and share no notice identifier | **0.50** |
| **P7** | two change observations from different sources state the same deploy identifier but share **no** target | **0.70** |

Specificity orders them P2 (30) > P6 (25) > P3 (20) > P4 (15) = P7 (15) > P1 (10) > P5 (5), so a pair
on which several fire is recorded under the strongest evidence.

P7 is the other half of C8's table: one commit shipped to several targets is the monorepo case, which
is N changes rather than one, so it is a suggestion a person settles in a second by looking. It is a
separate rule rather than a non-certain match from C8, because a rule listed as certain that sometimes
only suggests would make `CertainRules()` a half-truth and a recorded decision's rule id ambiguous
about whether it merged.

The scores are **published constants**, not tuned parameters. Changing one is a schema change
that shows up in a pull request diff, and §7 is how it is judged.

A probable rule compares a *Kubernetes* name with an *observed* name, never two of the same kind.
Two telemetry service names that resemble each other are two services with similar names, which
is the normal state of a system.

### C8's condition table, and the three cases that must not merge

> Added by feature 004 (T131). Code: `internal/resolution/deploy.go`. Contract:
> [`deploy-claims.md` §2](../../specs/004-deploy-feeders/contracts/deploy-claims.md). Acceptance
> fixture: [`deploy-cross-source-merge-01`](../../fixtures/deploy-cross-source-merge-01/manifest.yaml).

C8 is the rule SC-004 rests on: a rollout that a deploy pipeline and a platform both observed appears
**once**. It fires on a correlation key (§1a), `deploy.commit_sha` or `deploy.image` in digest form,
and it merges two change observations only when **all three** of these hold:

1. **the deploy identifier agrees.** Both changes carry the same key, from different sources.
   Candidates are found by looking the value up, so two changes with different values never meet at all;
2. **the target agrees.** The two changes share a target entity **in the graph**, after merge
   redirects (`ClaimStore.ChangeTargets`, compared by set intersection). It is not a comparison of the
   strings the sources used: GitHub says `github.repo=acme/storefront` and Cloud Run says
   `gcp.cloudrun.service=proj/region/storefront`, and what makes them one target is that C1, C4 or C5
   already merged them;
3. **the environment agrees, and both sides state it.** `deployment.environment.name`, a supporting
   attribute on the key rather than part of its value.

Each case that must not merge shares all but one of those. That is why the fixture holds them. A rule
generous enough to merge on any two conditions would pass a corpus that only held the merge, and fail
here.

| target | deploy identifier | environment | verdict | exercised by |
|---|---|---|---|---|
| shared | agrees | stated, agrees | **merge, C8** | `why-the-two-rollouts-are-one` (GitHub and Cloud Run) and `why-the-vercel-promotion-is-the-pipelines-rollout` (GitHub and Vercel) in `deploy-cross-source-merge-01`; `TestC8MergesTwoSourcesObservingOneRollout` |
| shared | **disagrees** | stated, agrees | **no merge, and no suggestion**: the redeploy | `why-the-redeploy-is-not-the-rollout-it-replaced`; `TestC8DoesNotMergeARedeploy` |
| **none shared** | agrees | stated, agrees | **no merge; a P7 suggestion**: the monorepo run | `why-one-commit-to-two-services-is-two-rollouts` and the queue in `the-monorepo-run-is-suggested-not-merged`; `TestP7SuggestsTheMonorepoCaseAndC8RefusesIt` |
| shared | agrees | **stated on both, different** | **no merge, and no suggestion**: a promotion or a canary | `why-the-canary-is-not-the-production-rollout` (deployment 4322, environment `canary`, against the production revision) |
| any | agrees | **unstated on either side** | **no merge, and no suggestion** | no fixture; `TestC8RefusesEveryFormOfNotKnowing` |
| any | **unknown**: absent on either side, or not a full lower-case hex commit, or an image without a digest | any | **nothing**: no key, so no candidate | `TestC8RefusesEveryFormOfNotKnowing`, `TestC8MergesOnAnImageDigestAndRefusesATag` |

Two more exclusions sit outside the table because they concern *who* is speaking rather than what was
said. Two keys from **one source** are one opinion, as for C1 and C7, and a key on anything but a
**change** is ignored: merging on it would put a change together with a service. `deploy.release` is not
a C8 identifier at all (`TestC8DoesNotKeyOnAReleaseIdentifier`), because `v2.3.0` is a tag many
repositories use in the same week.

#### The redeploy: one target, two commits

Two commits shipped to one service are **two rollouts**, the second replacing the first. Merging them
would collapse a rollout with the one it replaced: an investigation asking what changed at 14:18 would
find one change whose facts belong to two instants, and the rollout that actually preceded the incident
would be indistinguishable from the one before it. That is the defect feature 003's C4 shipped with.

Here it is **unreachable by construction** rather than avoided by a check. C8 looks candidates up by the
key's value, so two changes carrying different commits are never compared. There is no suggestion
either, because there is nothing to suggest: a service receiving a new commit is the ordinary sequence
of deploys, not an ambiguity for a person to settle. The fixture carries the redeploy twice: within the
pipeline (deployments 4320 and 4321 to `storefront`) and across sources (the pipeline's older deploy of
`web` against Vercel's promotion of the newer one). Both are `distinct_pairs` in its ground truth.

#### The monorepo run: one commit, several targets

A monorepo pipeline ships one commit to several services, and FR-017 makes that **N changes**, one per
target. An investigation asking what changed on `checkout` must get an answer about `checkout`. One
commit reaching several services is the normal case, not evidence that the services are one.
[`github-monorepo-01`](../../fixtures/github-monorepo-01/manifest.yaml) asserts the N-changes half from
a single source: one deployment mapped to three services is three changes.

This case, unlike the redeploy, is a **suggestion (P7, 0.70)** and not silence. The commit is strong
evidence that the two observations are related, and the only question left is whether the pipeline
shipped to one target or several. A person answers that in a second by looking. P7 needs two sources,
so the single-source monorepo fixture raises none. `deploy-cross-source-merge-01` does: its suggestions
golden holds twelve pending P7 suggestions and no merge, for one repository's deployments observed by
three sources. That count is what the choice costs in review work on a monorepo, and it is measured on
this fixture only.

A suggestion is P7, a separate probable rule, and not a non-certain C8 match. A rule listed as certain
that sometimes only suggested would make `CertainRules()` a half-truth and a recorded decision's rule
id ambiguous about whether it merged.

**A change with no target yet also lands here.** Before its target entity exists, a change has nothing
to share, so C8 cannot fire and P7 does. This is the order dependence the re-trigger exists for: when
the change attaches, when its own observation arrives after its key, or when two target identities
merge, the key is re-evaluated (`internal/projector/retrigger.go`,
[`deploy-claims.md` §2.1](../../specs/004-deploy-feeders/contracts/deploy-claims.md)). C8 then merges,
and the pending suggestion on the same pair is confirmed (§5). Building the fixture's Vercel half found
this case the hard way: every Vercel pair came out as P7 until the Vercel feeder described the project
it serves, because no target entity existed to share.

#### A different or unstated environment: one commit, one target, two rollouts

The same commit reaches the same service in staging and then in production. That is the ordinary shape
of a **promotion**, and a canary is the same shape inside one service. Each is a real rollout at its own
instant. Merged, they would be one change with one valid time, so at least one of them would be dated
at a moment it did not happen, and an investigation ranking changes by onset would be ranking the wrong
instant for the change that reached production. The
fixture's canary, deployment 4322 in environment `canary`, carries the same commit to the same
`storefront` service as the production revision it must not merge with. Its comment says it is the
pair that makes the third condition load-bearing. Delete the environment check and nothing else in
the corpus notices.

An **unstated** environment is refused for the reason C2 refuses one: two unstated environments are not
an agreed one, and a certain rule merges with no human in the loop. It may not gamble that an
unlabelled rollout is the production one. No P7 suggestion is raised in either environment case,
because the environment check comes first and a pair in two environments is two rollouts, not a
question. No fixture carries the unstated case; the unit test does.

---

## 3. Name normalization

`NormalizeName` reduces a name to the form the probable rules compare, in this order:

1. take the last `/`-separated segment — a `k8s.deployment` identifier is `<namespace>/<name>`
   and the namespace is not part of the name;
2. case-fold;
3. replace every run of non-alphanumeric characters with a single `-`;
4. strip configured suffixes, repeatedly, refusing any strip that would empty the name;
5. remove the remaining separators.

Default suffixes: `-svc`, `-service`, `-deploy`, `-deployment`, `-app`.

| input | normalized |
|---|---|
| `checkout-svc` | `checkout` |
| `checkout-service` | `checkout` |
| `checkout-svc-deploy` | `checkout` |
| `shop/checkout-svc` | `checkout` |
| `ops/notifications-svc` | `notifications` |
| `Checkout_SVC` | `checkout` |
| `check-out` | `checkout` |
| `svc` | `svc` (a name that *is* a suffix is left alone) |
| `checkout-v2` | `checkoutv2` (a version suffix is not stripped, so it distinguishes names) |

Step 1 is why `shop/notifications-svc` and `ops/notifications-svc` both normalize to
`notifications`. That is deliberate — it is exactly the case P1 is supposed to *suggest* rather
than merge, and `fixtures/ambiguous-identity-01` ships it as a labelled false positive.

---

## 4. Type resolution and the type guard

A Kubernetes workload and the OpenTelemetry service running on it are **one entity under two
names** (ADR-0001 D7), so C2 and C3 merge them rather than linking them with an edge. That keeps
rollouts, scaling and config changes at hop 0 from the service an alert names.

The merged entity's `type` follows a published precedence, highest first:

```
SERVICE > WORKLOAD > THIRD_PARTY > INFRA_RESOURCE > DB_SCHEMA > CONFIG > FEATURE_FLAG > OWNER > ALERT > CHANGE
```

Every type a source asserted is kept in `facets`, and a type change caused by a merge is a new
version (bitemporal, auditable).

**The type guard.** Two entities whose asserted types are both *below* WORKLOAD in that order and
differ are never matched by a probable rule. A `CONFIG` and an `OWNER` that share a name are a
coincidence, not evidence. The guard does not apply at or above WORKLOAD, because that is the
case the graph exists to merge. An unknown type on either side does not trigger the guard: an
entity nothing has described yet carries a provisional type from its namespace, and refusing
would make a suggestion depend on which event happened to arrive first.

---

## 5. Precedence

> **human decision > certain rule > probable suggestion**

In the order the projector applies it:

1. **Rejected pair.** A pair a person rejected is never merged again by any rule. A rule that
   goes on matching it is recorded as a `conflict` suggestion, not applied.
2. **Pinned entity.** An entity a person confirmed or manually merged is *frozen*: a later rule —
   certain or probable — that would merge it with a **third** entity is recorded as a `conflict`,
   not applied. This is the half of FR-040 that is easy to miss. Somebody who says *checkout-svc
   is checkout* has also said *checkout-svc is not anything else*, and a rule that later merges it
   into something else has overridden them just as surely as one that un-merged it.
3. **Certain match.** Merged automatically, with an `auto_merge` decision naming the rule, the
   score, the rationale and the supporting claims. A `pending` suggestion on the same pair becomes
   `confirmed`.
4. **Probable match.** Filed as a suggestion. Nothing merges.

A later human decision may reverse an earlier one: a `confirm` supersedes a `reject` on the same
pair, and a `split` supersedes the `confirm` or `manual_merge` that pinned the entity. Superseding
never rewrites history — the old decision row stays, with `superseded_by` pointing at what
replaced it.

### Merge mechanics

The survivor is the entity that appeared in the log first (lowest minimum `appended_seq` over its
claims, lexicographically smaller id on a tie), which makes a replay reproduce the same survivor
and therefore the same ids. Nothing is deleted: the merged-away entity keeps its id, its version
rows and its history, gains a `merged_into` redirect, and its claims are re-pointed so every name
it was known by resolves to the survivor. Its current node **and edge** versions are closed in
observed time and what produced them is re-applied to the survivor, so a merge is
order-independent.

### Split mechanics

A `split` detaches named identifiers into a **new** entity. The new entity's id is
`EntityID(namespace, value)` of the first detached identifier, or — when that id is already taken,
which it is whenever the identifier minted an entity that was later merged away —
`SplitEntityID(preferred, event_id)`. A split never resurrects the merged-away id; instead that
id's redirect is re-pointed at the new entity, so the identifier still resolves, to what the
person says owns it now.

Both sides' versions are re-derived from the assertions behind the original's current versions,
partitioned by the identity each assertion named, and both entities' `facets` are recomputed.

**Known limitation:** a split does not re-point edges. A relationship asserted against a detached
identifier stays on the original entity until its source asserts it again, at which point it lands
on the new one through ordinary segmentation.

---

## 6. Suggestions and the audit

### Suggestion states

`graph.suggestions` is a current view, one row per unordered pair
(`pair_key = min(id)|max(id)`).

```
                 ┌──────────── human confirm / certain rule ───────────► confirmed
   (rule fires)  │
        ──────► pending ──────── human reject ────────────────────────► rejected
                 │
                 └──────────── rule disagrees with a human decision ──► conflict
```

| status | meaning |
|---|---|
| `pending` | a probable rule matched; nobody has decided |
| `confirmed` | a person confirmed it, or a certain rule merged the pair |
| `rejected` | a person rejected the pair, or a split took it apart |
| `conflict` | a rule matched a pair a human decision blocks; **surfaced, never applied** |

A pair a person has decided is never reopened by a rule. A rule that keeps matching it moves the
row to `conflict` and leaves the decision alone.

`Suggestions` (FR-033) is paged, ordered by **score descending, pair key ascending**, and
filterable by focus node and status. Each side carries the entity's *identity* — id, resolved
type, facets, display name, aliases — not a version as of an instant: a suggestion is a question
the graph has no opinion about yet, and giving it a valid interval would invite a reader to think
it had one.

### The resolution audit and its observed-time approximation

`ResolutionAudit(a, b, observed_at)` (FR-031) answers whether two identifiers resolve to one
entity **as of an observed instant**, and returns the claims, the decisions in observed order,
and the canonical id.

Answering it exactly would need a bitemporal `merged_into`. There is none: a redirect is a single
column with one value, so the projection only knows where an identifier points *now*. What the
graph does keep bitemporally is the **decision history**, and that is what the audit reconstructs
from:

> A merge decision is **in force at O** when `decided_at ≤ O` and either nothing superseded it or
> the decision that superseded it was taken **after** O. Two identifiers are the same entity at O
> when the merge decisions in force at O connect them.

The approximation is precise about everything a decision records and blind to nothing else,
because every merge and every split writes one:

- a merge followed by a split is exact, because the split supersedes the merge decisions it undoes
  and the supersession carries its own `decided_at`;
- `decided_at` is the observed time of the event that carried the decision, so decisions and
  claims sit on one timeline;
- claims are filtered by their own `observed_at`, which is exact — a claim row records when the
  graph learned it and never moves.

Two consequences worth stating:

- the **canonical id** is reported only when the two identifiers do resolve to one entity at O.
  Naming a canonical id for two things the graph says are different would answer a question nobody
  asked.
- the audit's seed is the id an identifier *mints* (`EntityID(namespace, value)`), never today's
  claim rows, because a merge re-points claims and reading them would make every instant before a
  merge report the pair as already merged.

The same single-valued-redirect limitation affects the **alias list** of a node version read at an
observed instant before a later split: aliases are resolved through the current redirect table, so
a name that has since moved to another entity is not listed under the one it belonged to then.
The audit is the exact answer for identity questions at an instant.

---

## 7. Human decisions

Four decisions, all ordinary events in the log (FR-040):

| event | CLI | effect |
|---|---|---|
| `confirm_merge` | `resolve confirm <refA> <refB> --reason …` | merge and pin |
| `manual_merge` | `resolve merge <refA> <refB> --reason …` | merge and pin, with no suggestion behind it |
| `reject_merge` | `resolve reject <refA> <refB> --reason …` | block the pair permanently |
| `split_entity` | `resolve split <ref> --detach ns=value[,…] --reason …` | detach identifiers into a new entity |

All four require the **`decider`** role (FR-041a); `resolve suggestions` and `resolve why` need
only `reader`, so reviewing what the graph believes never needs a credential that can change it.
Every resolution command exits **3** on an authentication or authorization failure.

A decision **names entities by a name a source used** (`otel.service.name=checkout`,
`k8s.deployment=shop/checkout-svc`) or by `id:<entity_id>`. Naming by name is what lets a decision
be replayed into a graph rebuilt from empty, where canonical ids may legitimately differ.

### A decision with no principal is refused

The projector rejects any decision event whose principal is empty, with reason code
**`missing_principal`**, *before it reaches the log*. An anonymous decision is not a weaker
decision; it is not a decision at all (FR-041, SC-011). A decision naming an entity the graph has
never observed is refused with `ref_unresolvable`: a decision may not create an entity.

Every accepted decision upserts `graph.principals` (`principal` = `<iss>|<sub>`, e.g.
`sre-agent-dev|alice`), so an audit can say who decided without asking an identity provider that
may no longer know.

### Deterministic decision event ids

A decision needs an idempotency key and the request message carries no nonce, so the id is derived
from the decision itself:

```
human:<kind>:<subject>:<sha256(principal | rationale)[:12]>
```

- `kind` is `confirm`, `reject`, `merge` or `split`;
- `subject` is the references, **sorted**, joined by `|` — so `confirm a b` and `confirm b a` are
  one decision, which they are. For a split it is the entity followed by the detached identifiers;
- the digest covers the principal **and** the rationale.

Two consequences, both deliberate:

- the same person making the same decision about the same pair with the same reason twice is a
  **`DUPLICATE_NOOP`**. A double click, a retried request or a fixture loaded twice changes
  nothing.
- the same decision with a **different** rationale is a different event, because the rationale is
  part of what constitution VI requires to be recorded. Somebody adding the reason they forgot
  gets a second decision row, and the audit shows both.

`source_id` is `human`, registered at server start with kind `human` and ordering `none`.

---

## 8. Calibration reporting

`aisre fixture verify --report` fills `Metrics["calibration"]` for fixtures of the
**`ambiguous-identity`** family, from ground truth the manifest states independently of what the
rules decided:

```yaml
ground_truth:
  certain_pairs:
    - pair: [otel.service.name=payments, k8s.deployment=shop/payments]
      same: true
  probable_pairs:
    - pair: [otel.service.name=checkout, k8s.deployment=shop/checkout-svc]
      same: true
    - pair: [otel.service.name=notifications, k8s.deployment=ops/notifications-svc]
      same: false
```

Three measurements, answering three different questions:

| metric | question | expectation |
|---|---|---|
| `certain.precision` | of the merges made with no human in the loop, how many were right? | **1.00.** A certain rule may miss a merge; it may not make a wrong one. |
| `probable.brier_score` + `probable.reliability` | does 0.70 mean 0.70? | not a gate; it is the evidence behind the published constants |
| `probable_auto_merges` | did a probable rule ever merge anything? | **0** (SC-007) |
| `human_decisions_survive_replay` | after the replay, is every decision still recorded, attributable and in effect? | **true** (SC-007, SC-011) |

The Brier score is the mean squared error between the stated score and the truth (0 is perfect;
a constant 0.5 scores 0.25). The reliability table groups the same data into fifths, because
"0.70 was right half the time on two samples" says more than one aggregate number — and on a
hand-authored fixture the sample size is the first thing a reader needs to see.

"Still in effect" is checked structurally, not by trusting the row: for every `confirm` and
`manual_merge` still in force, the two entities it names must resolve to one; for every `reject`
and `split`, they must not.

---

## 9. Where this lives in the code

| concern | file |
|---|---|
| rule registry, `Evaluate`, `EvaluateCorrelation` | `internal/resolution/rules.go` |
| C1–C3 | `internal/resolution/certain.go` |
| C8, P7 | `internal/resolution/deploy.go` |
| the published correlation namespaces | `internal/graph/correlation.go` |
| `correlation_as_identity` | `internal/log/validate.go` |
| storing and re-pointing correlation keys | `internal/projector/refs.go` |
| the C8 re-trigger queue | `internal/projector/retrigger.go`, migration `0012` |
| P1–P3, normalization, type guard | `internal/resolution/probable.go` |
| running the rules, merging, absorbing | `internal/projector/refs.go` |
| suggestions, precedence, decision rows | `internal/projector/suggest.go` |
| human decisions | `internal/projector/{confirm_merge,reject_merge,manual_merge,split_entity}.go` |
| audit query and its approximation | `internal/query/audit.go` |
| suggestions query | `internal/query/suggestions.go` |
| RPC surface | `internal/server/{resolution,query_resolution}.go` |
| CLI | `internal/cli/resolve*.go` |
| calibration report | `internal/fixture/report.go` |
| worked example | `fixtures/ambiguous-identity-01/` |
