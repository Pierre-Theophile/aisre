# Data model — Deploy Feeders (GitHub and Vercel)

**Date**: 2026-09-22 · **Plan**: [plan.md](./plan.md) · **Research**: [research.md](./research.md)

What a platform object becomes in the graph. Every row is a mapping from something a platform
**states** to something the graph holds; nothing here is derived from a string the platform did not
state, which is the rule FR-013, FR-016 and FR-037 each restate for their own field.

---

## 1. Rollout change — the unit of this feature

One completed deployment **of one target**. The unit is `(origin, target)`, not `(origin)`: a
monorepo run deploying three services is three rollout changes (FR-017), because the platform
observes rollouts one-per-service and a one-to-many change could not be merged pairwise against
them.

| field | source | rule |
|---|---|---|
| kind | — | `ROLLOUT` |
| valid time | the platform's stated completion instant | **never** the run's start, the build instant, or the instant the feeder learned of it. Absent ⇒ valid start marked **unknown**, never guessed (FR-012) |
| observed time | the graph, at acceptance | neither clock corrects the other (Edge case 14) |
| target | the resolved service | one target per change; unresolvable ⇒ kept **unattached** with every carried identifier recorded (FR-018, SC-018) |
| actor | the triggering account, as stated | |
| actor kind | `ActorKind` (§3) | **never** inferred from the actor's name (FR-013) |
| origin reference | the platform URL of the run or deployment | carries no credential and no token (FR-014); emitted both as the change's origin **and** as a `SOURCE_LINK` pointer |
| rollback flag | only where the platform states it | never inferred from commit ordering, from the deployment being older, or from a revert-shaped message (FR-016, SC-009) |
| claims | §2 | omitted rather than invented where the platform states no value (FR-041) |
| idempotency key | `(source, origin run or deployment id, attempt, target)` | FR-020; a re-run is a **new** change, and the earlier one is not amended or retracted (Edge case 3) |

### 1.1 When a rollout exists, per platform

| platform | condition | why |
|---|---|---|
| **GitHub** | a `deployment_status` reaches **`success`** on a deployment whose environment is on the production allowlist | GitHub publishes seven states and **designates none as terminal**, so this feature publishes the mapping (research §2). `failure`/`error` are terminal and are a failed attempt only where the platform states one — never a rollout. `inactive` means *superseded* and is a property of the same change (FR-022). `in_progress`/`queued`/`pending` produce nothing |
| **Vercel** | `target = production` **and** `readySubstate = PROMOTED` | `state = READY` means built and available, **not serving**. Vercel states the difference — `STAGED` has never seen production traffic — so a rollout keyed on `READY` alone would claim production moved when it had not (research §3.1) |

### 1.2 What produces no rollout, and is counted rather than silent

A test or lint run with no deployment; a run not on the deploy-workflow allowlist; a preview
deployment; a `STAGED` Vercel deployment; a deployment that never reached a terminal successful
state. Each exclusion is **counted and stated in the checkpoint** (FR-019, FR-023, FR-032), so a
later reader can tell "nothing deployed" from "we were not looking at that environment".

---

## 2. Claims — how one rollout becomes one change

Seven namespaces, none of which exists in `pkg/feeder/ref.go` today (research §1.2). Each is
registered **with its value normalisation**, because a namespace whose value form is unfixed is a
certain rule that silently never fires.

### 2.1 Deploy correlation keys — stated by any source observing the same rollout

These three are **correlation keys** and not identity claims (T148, `graph.correlation_keys`): each is a
value several entities share, and the event log refuses a claim in any of them. See
[contracts/deploy-claims.md §1.2](contracts/deploy-claims.md).

| namespace | value form | note |
|---|---|---|
| `deploy.commit_sha` | full 40-hex, **lower-cased**; never abbreviated | the join GitHub↔Vercel↔Cloud Run rests on. Kept as stated even if the commit no longer exists — the feeder does not verify reachability (Edge case 8) |
| `deploy.image` | the immutable digest form where the platform states both digest and tag | prefer digest over mutable tag (FR-041) |
| `deploy.release` | the platform's release identifier | |

### 2.2 Target claims — what links the shipping system to the running one

| namespace | value form | note |
|---|---|---|
| `github.repo` | `<owner>/<repository>`, lower-cased | a repository is never a node (FR-030). It addresses the *target* of a rollout, so it is an identity and not a correlation key (T148); the GitHub connector itself carries it as a **property** of the change, because no published rule reads it |
| `vercel.project` | the project id | the project *name* is a property; the id is stable across rename (Edge case 9) |
| `github.change`, `vercel.change` | the platform's stable object identifier | keyed on the numeric/stable id, so a rename is a property change, never a delete-and-create |

The environment is a **supporting attribute** on the target claim, not part of its value (FR-042) —
so `checkout` in production and `checkout` in staging are distinguishable without minting two
namespaces.

### 2.3 The rule that consumes them: C8

`internal/resolution/deploy.go`. Two change observations are one change when they state the **same
deploy identifier for the same target in the same environment**. The four-way condition table is in
[research §1.3](./research.md); the case that matters is that **target agreeing while the commit
disagrees does not merge** — that is a redeploy, and merging it would collapse a rollout with the one
it replaced.

Neither feeder merges anything itself and neither suppresses its own observation (FR-043). A pair
that cannot be merged becomes a resolution **suggestion** with its score and rationale (FR-044).

---

## 3. Actor kind

Feature 001's published enum, used directly (research §1.1): `PERSON`, `AUTOMATION`, `CONTROLLER`,
`VENDOR`, `UNKNOWN`, with `ACTOR_KIND_UNSPECIFIED` distinct from `UNKNOWN`.

| platform evidence | kind |
|---|---|
| GitHub: push or manual dispatch by a user account | `PERSON` |
| GitHub: **schedule** | `CONTROLLER` — machinery reacting to state, which 002's causal ordering may exonerate after onset |
| GitHub: repository dispatch, or a workflow triggered by a workflow | `AUTOMATION` |
| GitHub: app or bot account (`type` in the payload) | `AUTOMATION` |
| Vercel: `creator.type` plus `source` (`git` behind a person's push, `cli` inside CI) | as stated |
| neither states it | `UNKNOWN`, with the evidence recorded |

**The account type comes from the payload, never from the login string** (FR-027, SC-005) — and
SC-005 requires a fixture in which a human's login resembles a bot's and vice versa, so the rule is
tested rather than asserted.

---

## 4. Configuration change

| field | rule |
|---|---|
| kind | `CONFIG` |
| key | the variable or setting name |
| **value** | **never carried, in any form** — not plaintext, not ciphertext, not truncated, not hashed — and the feeder never requests the decrypted value (FR-038, SC-006) |
| environment | which environment it applies to; preview-only scope is excluded by the same filter, counted |
| version | the platform-assigned version identifier |
| actor, actor kind | as §3 |
| edge | `changed-by` to the mapped service |

Emitted **independently of deployments**: never folded into the following rollout and never delayed
until one occurs (FR-039). A config change at 03:10 and a rollout at 09:05 both answer "what changed
before 09:30", and neither is folded into the other.

---

## 5. Pointers

`SOURCE_LINK` and `LOG` on every change (FR-047). **Additive**: a deploy feeder's pointers never
replace or suppress pointers another source attached to the same entity (FR-050). Neither feeder
executes a pointer (FR-001) — running a log pointer is 003's and 005's contribution.

Two vocabularies to register in `pkg/feeder/pointer.go` (research §1.6), each with the published
statement of why its selector is not expressible in OpenTelemetry semantic conventions: these address
an **object by path** — a run, a deployment, a release — not an entity by attribute equality.

---

## 6. Transports

**Polling is the truth and is marked sampled** at the poll interval, so a consumer can tell a
complete history from a sampled one (FR-052). The webhook is a **doorbell only**: it enqueues "poll
now" and its body is never parsed, trusted or stored (FR-053). Signature verified with a
**constant-time** comparison; unsigned or failing notifications dropped and counted; the endpoint
rate limited, so a forgery costs at most one extra poll and can never create, alter or retract
anything (FR-054, SC-012).

Both transports usable together, and **the result must not depend on the order they deliver in**
(FR-055).

---

## 7. Checkpoints

Carry the completed extent, the scope in force, and **whether the feeder was watching immediately
before that extent** (FR-057). A partial poll yields no retraction: only the completed extent is
checkpointed and the gap is declared (Edge case 13).

The scope in force is what lets a query tell "nothing deployed" from "not in scope at that time"
(FR-008) — and, because GitHub's scope is the installation's repository selection enumerated at
startup and Vercel's is configuration within the token's grant, the checkpoint also records **which
of the two regimes** the feeder is running under.

---

## 8. Entities this feature deliberately does not create

Repositories, branches, pull requests, reviews, code diffs, issues, projects and teams. A repository
is an identity claim on the service it ships (FR-030); a commit is a claim on a change, never a node
with its contents. Secret and environment-variable **values**, test results and review comments are
never read at all.
