# Contract: the deploy namespaces and the C8 rule

> **Amended by T148.** The three `deploy.*` namespaces are **correlation keys** and not identity
> claims — see §1.2. The rest of this contract holds as written; where it says "claim" of a
> `deploy.*` value, read "correlation key".

**Status**: proposed (feature 004) · **Owner**: feature 001's published contracts (constitution IX)

This is the contract feature 004 needs from feature 001 and cannot implement locally. It is the
difference between "one rollout, not two" (SC-004) working and a certain rule that is published,
registered, evaluated on every claim and **never fires** — the failure feature 003 recorded for C4,
C5 and C7 before a cross-source fixture existed.

## 1. Namespaces

Registered in `pkg/feeder/ref.go`, each **with its normalisation**. A namespace whose value form is
unfixed does not deliver the join it exists for.

| namespace | value | normalisation | stated by |
|---|---|---|---|
| `deploy.commit_sha` | the commit a deployment shipped | the full hex object id, **lower-cased** — 40 digits for git's SHA-1 format or 64 for its SHA-256 format; an abbreviated sha is normalised only if the source states the full one elsewhere, otherwise the claim is **omitted** | GitHub, Vercel, and any future deploy source |
| `deploy.image` | the container image deployed | the **immutable digest** form, `<name>@<algorithm>:<hex>`, with any tag dropped; a reference stating only a tag is **omitted** | GitHub, Cloud Run (003) |
| `deploy.release` | the release identifier | as the platform states it | GitHub |
| `github.repo` | the repository shipping a service | `<owner>/<repository>`, lower-cased | GitHub |
| `github.change` | a GitHub run, deployment or release | the platform's stable identifier, never the display name | GitHub |
| `vercel.project` | the Vercel project | the project **id**, not the name — stable across rename | Vercel |
| `vercel.change` | a Vercel deployment | the deployment `uid` | Vercel |

**Supporting attribute, not part of the value**: the environment. `checkout` in production and
`checkout` in staging are one namespace and one value with different supporting attributes, so a rule
can require agreement on the environment without the namespace multiplying.

**Omission over invention** (FR-041): where a platform states no value for a claim, the claim is
absent. A missing claim is never agreement.

### 1.1 Two places the implementation is stricter than the prose above was

Both were settled while implementing `pkg/feeder/deployref.go` and are recorded here rather than left
as a difference between the contract and the code.

- **An image stating only a tag is omitted, not carried.** The first wording asked for "the digest
  form where the platform states both", which left open what to do when only a tag is stated. The
  answer is nothing: a tag names whatever is there now, not what was deployed, so two unrelated
  rollouts both running `:latest` — or both running `:v3` after a re-tag — would share a
  `deploy.image` value and **C8 would merge two rollouts that are not the same rollout**. That is the
  confidently-wrong-answer class, and it is worse than the merge a missing claim costs.
- **A commit is accepted at 40 **or** 64 hex digits.** The first wording fixed 40, which is git's
  SHA-1 object id. Git's SHA-256 object format is 64 digits, and refusing it would silently drop a
  claim a repository genuinely stated. The two lengths cannot collide, so accepting both costs
  nothing; anything shorter is an abbreviation and is still refused.

A third point is **not** a stricter rule but a limit worth stating: "the platform's stable identifier,
never the display name" is not a property of a string — `checkout-api` is a plausible id and a
plausible name — so no normaliser can enforce it. `feeder.StableIdentifier` checks only what is
checkable (non-empty, no whitespace) and says so. What enforces the rest is the connector reading the
id field, with a test asserting the ref changes when the id changes and not when the name does.

### 1.2 The `deploy.*` namespaces are correlation keys, not identity claims (T148)

This contract was written on one assumption the graph could not honour: that a deploy identifier is
storable as an identity claim. `graph.identity_claims` is constrained `UNIQUE (namespace, value,
source_id)`, so one identifier value from one source belongs to **exactly one entity** — and a monorepo
deployment is three changes sharing one commit (FR-017). The value could land on only one of them,
whichever the projector processed first, so the graph's state depended on delivery order. Four of the
seven US1 fixtures failed their shuffle step (FR-048) on exactly that, which is what the shuffle step
is for.

§2.3 below records the narrowing of C1 that the same vocabulary forced, and calls it "the larger
finding". It was not large enough. Narrowing C1 stopped the wrong *merge*; it did not stop the wrong
*storage*, because the premise underneath both is that the graph has one way to say two observations are
related.

It needs two:

- an **identity claim** is a name for one entity — `otel.service.name=checkout`;
- a **correlation key** is a value several entities share — a commit, an image, a release, a trace, a
  build.

So `graph.correlation_keys` is the second table (migration `0012`), unique per
`(entity_id, namespace, value, source_id)`: many entities may carry one key, and one entity may not
carry the same key from one source twice, which keeps a redelivered event a no-op. The event is
`CorrelateEntity`; a rule that reads one declares `EvalCorrelation` instead of `Eval`, and `Register`
refuses a rule that declares both or neither. The event log **refuses** an identity claim in any of the
three namespaces, reason code `correlation_as_identity`, so the mistake is caught on a feeder's first
run rather than found later as an order-dependent graph.

Identity gets *stronger* by the separation: with these namespaces out, `identity_claims` is injective
again and C1's premise holds without a list of exceptions.

**`github.repo` is deliberately not one of them.** The refusal sees a namespace and not a subject, and
`acme/storefront` does name exactly one repository — the deploy feeders address a rollout's *target* by
it, which mints an entity, and every entity gets a primary identity claim in the namespace that
addressed it. A registry holding `github.repo` would promise a refusal the projector itself breaks. Its
danger is a wrong subject rather than a wrong namespace, which is what §2.3's C1 narrowing guards, and
the GitHub connector emits it as a **property** of the change rather than as either kind.

The full published account is `docs/schema/resolution.md` §1a.

## 2. C8 — one shared deploy identifier, one rollout

`internal/resolution/deploy.go`. Certain. The id is **C8**: C6 is the vendor allowlist and C7 the
Cloud SQL connection name, so C8 is the next free id.

**Fires when** two change observations from **different sources** state the **same deploy identifier**
(`deploy.commit_sha`, or `deploy.image` by digest) for the **same target** in the **same
environment**.

| target | deploy identifier | verdict | why |
|---|---|---|---|
| agrees | agrees | **merge** | the two sources are observing one rollout |
| agrees | **disagrees** | **no merge** | two commits to one target is a **redeploy** — two rollouts. Merging collapses a rollout with the one it replaced, which is the defect C4 shipped with in 003 |
| **disagrees** | agrees | **no merge; suggest** | one commit to several targets is the monorepo case, which FR-017 already makes N changes |
| unknown either side | — | **no merge** | a missing claim is not agreement |

**Specificity**: above C1's 10 — when both fire, the recorded reason should be the deploy identifier,
which a reviewer can check against a deployment, rather than "two sources used the same identifier",
which is a tautology.

**Same-source pairs are excluded**, as C1 and C7 exclude them: one source asserting its own commit
twice is one opinion, not corroboration.

### 2.1 How "the same target" is decided, settled while implementing

The two sources do not spell a target the same way and never will: GitHub says
`github.repo=acme/storefront`, Cloud Run says `gcp.cloudrun.service=proj/region/storefront`. A rule
comparing the identifiers two sources state would be a rule that **can never fire** — which is exactly
how C7 shipped, reading one namespace on both sides so that the only pair it could match was one that
could not exist.

What makes two targets the same target is that **resolution already merged them**, by C1, C4 or C5.
So C8 asks the graph, through a new `ClaimStore.ChangeTargets` — the second graph question a rule may
ask, after P3's `SharedOwner`. The comparison is **set intersection**: a change usually has several
targets (003's revision-created change names the Cloud Run service *and* the revision), and only the
service side can ever agree with GitHub's, so intersecting is right where guessing which target is
"the service" would not be.

This makes C8 **order-dependent** unless it re-fires: if the deploy keys are stored before the two
target identities merge, the intersection is empty at that moment and nothing re-evaluates. Rules run
only when a key is stored (`internal/projector/apply.go`), so the re-trigger is explicit work. There
are **three** occasions, one per party to the comparison, and the list was two long until
`deploy-cross-source-merge-01` was built:

1. the **target** arrives last — a change attaches to a target that did not exist when the key arrived
   (`attachWaiting`, in `internal/projector/attach.go`);
2. the **change** arrives last — a key is stored on a change nobody has described yet, so its subject
   is minted as a bare entity with no targets at all, and the observation that supplies them comes
   afterwards (`internal/projector/observe_change.go`);
3. the **targets merge**, by C1, C4 or C5, so an intersection that was empty is not
   (`internal/projector/refs.go`).

The second was missing from this contract and from the code. The fixture's shuffle step found it: C8
merged in arrival order and not under permutation, which is the order dependence FR-021 forbids. Both
orders are a real recording — a platform reports a revision and a pipeline reports having deployed it,
from two separate polls, in whichever order the polls return — so this was not a theoretical gap.

It is worth recording *why* it was missed, because the same shape will recur. Occasions 1 and 3 are
both "something else moved", which is what a re-trigger obviously means. Occasion 2 is the rule's own
subject arriving late, which does not feel like a graph change at all. It is one. That is the
`c4FromService` lesson generalised, and a fixture's shuffle step is what catches its absence — twice
now, in the same place, for the same reason.

#### 2.1.2 A queued re-evaluation is dated from the last event that changed its answer

Settled in T139, and the task's own framing was wrong. It recorded a limit — a row whose instant has
been overtaken cannot be applied, the queue wedges, and the fix is "a decision about the audit trail":
record the overtaking, or re-derive the work.

Neither is available. Both decide something at **drain** time, and the drain's timing is the caller's
choice: `ApplyWithOptions` drains immediately, a batching caller drains between batches. The same events
under a different batch size would then produce a different graph, which FR-021 forbids and
`TestReplayIsIndependentOfBatchSize` is there to catch.

The premise was wrong too. The limit is not reachable only after a crash; it is reachable in ordinary
batched replay, because the drain runs between batches and any later event in the same batch can open
the versions that make an earlier row's instant too old. A test reproduces it.

The answer is upstream of the drain. Three call sites queue this work, and the last to run is the one
that completed the rule's precondition — usually the merge that made two targets one. The insert kept the
first instant (`ON CONFLICT DO NOTHING`), so that final re-queue was dropped and the row carried the
instant of a deploy key stored long before the answer could have been yes. It now keeps the **newest**
instant, which is what the row means: *this rule's answer last changed here*. That is a function of the
events rather than of when the drain ran, so it is identical at any batch size, and late enough to be
legal for the same reason it is correct.

#### 2.1.1 The re-trigger is deferred, and that is forced rather than chosen

Re-evaluating in the transaction that moved the target set **does not work**, and the reason is a
property of the temporal model rather than an implementation detail. A merge closes the observed
interval of the versions it absorbs, and `close_observed` refuses to close an interval at the instant it
opened — correctly, because a version that existed for no time is not a version. So a second merge in
one transaction tries to close what the first just opened and is refused, which is the guard
`runResolutionRules` already documents for its own two-pass absorb.

So the work is **queued** — `graph.pending_resolution`, migration 0011 — and drained afterwards, one
claim per transaction. Three consequences worth stating:

- **The merge lands on the drain, not on the event.** For an ordinary `Apply` that is microseconds
  later, because it drains itself as soon as its transaction commits. A caller batching events through
  `ApplyInTx` owns its transaction and cannot be drained from inside, so it drains between batches;
  `internal/fixture`'s loader is the worked example, and a replay that skipped it would produce a
  different graph from a live run.
- **The drained re-evaluation is observed strictly after the event that queued it**, by the smallest
  representable step. It is the event's instant rather than `now()` because a replay has to learn things
  at the same instants as the original run; it is *after* it because the graph genuinely learned this in
  a later transaction. Valid time is untouched: what a change asserts about the world is what its source
  said.
- **Two triggers, and neither is redundant.** A late attachment covers two sources naming one target ref
  that no node describes yet — the Vercel feeder claiming `github.repo` is the real case. A target merge
  covers two changes that were attached all along to what turned out to be one entity. A test for each
  fails when the other trigger is removed.

### 2.2 The suggestion is P7, not a non-certain C8 match

`Rule.Certain` says a rule may merge automatically and `CertainRules()` is what the projector trusts.
A rule listed as certain that sometimes emitted a suggestion would make that listing a half-truth and
make a recorded decision's rule id ambiguous about whether it merged. So the monorepo row of the table
is **P7**, published in its own right, score 0.7. The next free probable id is P7 and not P6: P6 is the
vendor rule.

### 2.3 C1 had to be narrowed, and that is the larger finding

C1 is published with `Namespaces: ["*"]` and fires on **any** namespace: "the same identifier in the
same namespace denotes one entity". Registering this vocabulary made that premise false in four
places, and the consequences are worse than the one C8 was written to avoid:

| namespace | what C1 would have merged |
|---|---|
| `deploy.commit_sha` | a monorepo run ships one commit to several services, so **every change in the release** collapses into one — certain, score 1.0, no human in the loop. The "merge on the commit alone" alternative this document rejected, arriving through C1 instead of C8 |
| `deploy.image` | the same image runs in staging and in production, and a redeploy of one digest is a second rollout |
| `deploy.release` | `v2.3.0` is a tag many repositories use in the same week |
| `github.repo` | **two services.** A repository is a claim on the service it ships and never a node (data-model §2.2), so in a monorepo `checkout` and `web` both claim it |

So `internal/resolution` now publishes `IdentifyingNamespace`, and C1 refuses a namespace whose value
several entities share. The list is explicit rather than derived from a prefix: a namespace is
non-identifying because of what its values mean, and a rule guessing from the spelling would merge on
the first namespace somebody named inconsistently. `github.change`, `vercel.change` and
`vercel.project` stay identifying — each names one thing — and a test asserts C1 still fires on them,
so the guard narrows C1 rather than switching it off.

Since T148 (§1.2) the three `deploy.*` rows of that table cannot reach C1 at all: the event log refuses
a claim in them. The guard stays for two reasons — the log is append-only, so a database replaying
events appended before the refusal existed still has such claims to project, and a guard that exists
only at the front door stops applying the moment anything writes to the projection by another route.
For `github.repo`, which is not a correlation namespace, this guard is the only one there is.

## 3. What this contract owes back

| item | where | status |
|---|---|---|
| Actor kind on a change | `graph.proto` `ActorKind` | ✅ delivered (ADR-0005 D1) |
| The seven namespaces above | `pkg/feeder/ref.go` | ✅ delivered |
| Correlation keys as a kind: table, event, rule trigger, log refusal | `0012`, `graph.proto`, `internal/{graph,log,resolution,projector}` | ✅ delivered (T148) |
| C8 | `internal/resolution/deploy.go` | ✅ delivered |
| A rollback marker on a change | `graph.proto` | ❌ to build — `investigation.proto`'s `rollback_candidate` is the engine's *suggestion*, a different concept |
| "Unattached" + the attachment convention | `graph.proto` has the marker; the convention is unconfirmed | ⚠️ partial |
| `github-resource/v1`, `vercel-resource/v1` | `pkg/feeder/pointer.go` | ❌ to build |

## 4. How it is proved

**`fixtures/deploy-cross-source-merge-01`** — one rollout, two sources: the GitHub feeder's
observation and feature 003's Cloud Run revision, sharing a `deploy.commit_sha`. It is the
**acceptance test for C8**, not a late verification, because a corpus in which every fixture is
single-source measures SC-004 and SC-017 as "100% of nothing".

It must also carry the pairs that must **not** merge, for the same reason 003's fixture carries its
`distinct_pairs`: a rule generous enough to merge everything satisfies the rate on its own.

- a redeploy — same target, different commit — held apart
- a monorepo run — same commit, different targets — held apart, N changes
