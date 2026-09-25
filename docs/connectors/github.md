<!-- SPDX-License-Identifier: Apache-2.0 -->

# The GitHub connector

> **Status: skeleton.** Only the sections feature 004's Phase 1 delivers are filled: the credential
> and its scopes (§1), the published read-only operation surface (§2), and what is deliberately never
> read (§3). Sections marked _(pending)_ name the task that fills them. A number written here that was
> not measured is worse than an empty cell, because an operator approves this page and then stops
> asking.

What this connector reads, what it costs, and the proof it can only read. It exists so that the
person who owns the organisation's GitHub installation approves **a set of operations rather than an
intention**.

Contract: [`specs/004-deploy-feeders/contracts/read-only-operations.md`](../../specs/004-deploy-feeders/contracts/read-only-operations.md).
Platform facts, as established from the published documentation:
[`research.md`](../../specs/004-deploy-feeders/research.md) §2.

## 1. The credential, and where the scope comes from

A **GitHub App installation**, not a personal access token. The difference is the point of FR-008: an
App installation carries a **repository selection the platform enforces**, so the set of repositories
this connector can read is what somebody granted it, and a repository outside the selection is not
skipped by policy — it is unreachable.

The connector **enumerates that selection from GitHub at startup** (`GET /installation/repositories`)
rather than trusting a list written in configuration, and its checkpoint records both the selection
and the `repository_selection` value GitHub reports, so a run states which regime it was under (T033).

| permission | access | why |
|---|---|---|
| Deployments | read-only | the rollout and its statuses (US1) |
| Actions | read-only | workflow runs — the deploy pipeline's conclusion and its actor |
| Contents | read-only | releases live under it; **no file, tree or diff is ever requested** (§3) |
| Metadata | read-only | mandatory for any installation |

Nothing else is requested. In particular the installation asks for **no** `Secrets`, `Variables`,
`Issues`, `Pull requests`, `Members` or `Administration` permission at any access level, so the
guarantees in §3 are enforced by the grant and not only by this connector's restraint.

## 2. The published read-only operation surface

Every operation this connector may issue, by name. An operation absent from this list is refused by
`Surface.Issuable` before any quota is spent, rather than by review (FR-004).

Each operation is spelled `METHOD path`, and that spelling **is** the write test: `GET` and `HEAD` are
reads by the definition of the verbs, so no vendor naming convention has to be guessed at. There is no
`POST`, `PUT`, `PATCH` or `DELETE` anywhere below, and — unlike the GCP connector, which acknowledges
a Pub/Sub doorbell — **there is no declared state change at all**. The surface's state-change count is
zero and a test asserts it.

| area | operation | why |
|---|---|---|
| budget | `GET /rate_limit` | the remaining quota the cycle's budget is a share of; free against the primary limit, so once a cycle |
| deployments | `GET /repos/{owner}/{repo}/deployments` | a deployment is a rollout of a commit to an environment |
| deployments | `GET /repos/{owner}/{repo}/deployments/{deployment_id}` | one rollout, when a status names a deployment the list window no longer covers |
| deployments | `GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses` | how the rollout ended, and when; no status is designated terminal by the API |
| releases | `GET /repos/{owner}/{repo}/releases` | a tagged release is a change even where no deployment records it |
| releases | `GET /repos/{owner}/{repo}/releases/{release_id}` | one release, by id, when a deployment references it |
| scope | `GET /installation/repositories` | the repositories the installation was granted, enumerated rather than configured |
| workflow runs | `GET /repos/{owner}/{repo}/actions/runs` | the deploy pipeline, its conclusion and the actor that started it |
| workflow runs | `GET /repos/{owner}/{repo}/actions/runs/{run_id}` | one run, to close an extent whose last page is already out of the list window |

**The table above and the code are one list.** It lives in
[`internal/feeders/github/requestlog.go`](../../internal/feeders/github/requestlog.go), and this page
is parsed and compared against it by a test in both directions, area and reason included — a row here
that the process would refuse fails the build, and so does an operation the process carries that this
page does not publish. That matters more than it sounds: this page is what an operator approves, and
an approval that can drift from the system is an approval of a document.

### What this cannot prove

Worth stating narrowly, because a guarantee overstated is worse than one stated plainly:

- **It proves what this connector may issue, not what the API does.** A `GET` that GitHub implements
  with a side effect would pass every check here. Nothing inside a client can see that; the mitigation
  is that every operation is a published read of a published resource.
- **It says nothing about the installation's other holders.** If the same App is used by something
  else, this process's read-only proof is about this process.
- **The live-run half of SC-007 is open.** What is verified today is the capability — the stronger
  half, and the half a live run could not have given on its own — over the unit surface. The recorded
  request log over the corpus is filled by the fixture work (US1), and the live figure when a
  credential exists.

## 3. What is deliberately never read

As much of the contract as §2, because an operator granting an installation is granting more than this
connector uses:

- **Secrets and Actions variables, in any form** — not their values, not their names, not the fact
  that one exists. A deploy feeder has no question that needs them, and the installation asks for no
  permission that would allow it.
- **Job logs and step output.** A run's conclusion is a fact about a rollout; its logs are the
  application's own output, and build text routinely contains tokens somebody pasted.
- **Repository contents, diffs and file trees.** The commit sha is an identifier the graph joins on
  (FR-041); the code at that sha is not evidence about a rollout.
- **Issues, pull requests, reviews and comments.** Human discussion is not a change to a production
  system, and it is the part of a repository most likely to carry personal data.
- **Members, teams and permissions.** The actor on a change comes from the payload recording it
  (FR-013); the organisation's membership graph is a different subject.

Writes that are out of scope with **no configuration that enables them**: dispatching, re-running or
cancelling a workflow; creating or updating a deployment or a deployment status; creating or editing a
release; writing a comment, a check or a commit status.

## 4. The inbound webhook

_(pending — T039–T042.)_ It is a doorbell in the other direction: something calls us. It enqueues
"poll now" and its body is never parsed, trusted or stored (FR-053), so it issues no GitHub operation
of its own and appears nowhere in §2.

## 5. What it costs

_(pending — T035, T037, and the usage report.)_ The budget is a share of the **remaining** quota:
`GET /rate_limit` for the cycle's opening reading and the usage report, and the `x-ratelimit-*`
response headers for the in-cycle decrement. The endpoint alone would let a cycle overspend between
readings; the headers alone would lose the other resource families.

The headers are `x-ratelimit-limit`, `x-ratelimit-remaining`, `x-ratelimit-used`,
`x-ratelimit-reset` and **`x-ratelimit-resource`** — the last of which names the family the request
counted against, so the connector learns that from the response rather than maintaining its own map
from endpoint to family. GitHub documents the `x-ratelimit-*` headers as *"the authoritative source
for your current rate limit status"*, which is why they and not the endpoint drive the in-cycle
decrement.

`GET /rate_limit` is **free against the primary limit but can count against the secondary one**, which
is why it is read once per cycle rather than freely. An earlier draft of this page said it "costs
nothing to ask"; that was wrong in a direction that would have made a busy cycle spend secondary
allowance on measuring itself.

### 5.1 The cycle's first call, and why a fallback allowance is required

A family is learned from a response, so before the first response of a cycle there is no family. That
one call is metered against **`unobserved`** — not against a guessed `core`, because a guess that turns
out wrong spends one bucket's allowance out of another's, and not against `unnamed`, which already means
something else (the platform answered with numbers but did not name the bucket). A usage report showing
anything beyond the cycle's opening call under `unobserved` is a signal in its own right: the
`x-ratelimit-resource` header stopped arriving.

A family with no reading falls to the configured **static allowance**, so a connector configured without
one cannot open its cycle at all. That is deliberate and it is loud: the alternative is a first call
nothing bounds. The reserve does **not** apply to it: the reserve protects what the platform says is
left, and a static allowance is not a platform statement. An earlier rule applied the floor to the
static reading too, which made any allowance smaller than the reserve worth zero calls. The live cycle
(004 T157) found this, because with the default reserve of 500 it could not make the free
`GET /rate_limit` that would have given it real numbers. Once the platform has reported a family, the
reserve binds that family as before. `feed github` configures 30 calls: one first call per published
operation, with room for retries.

### 5.2 Pagination, and what a short window says

Every page is a separate trip through the metered door — a cycle that paged freely once admitted would
meter one call and make twenty. Pages are followed by GitHub's own `Link` cursor rather than an
incremented page number: on a collection that changes while it is being read, a page number re-slices
it and a cursor does not.

A list that stops before the end returns **the items it read and a refusal saying so**, and the reason
is distinguishable: the operator's page cap, the budget's yield, or the platform's own refusal. The
items already read are true, and a caller that discarded them would turn a declared gap into a larger
one nobody declared (FR-056, FR-073).

### 5.3 A 403 is two different answers

GitHub uses `403` both for the secondary rate limit and for a permission the credential does not have.
They want opposite responses — one is waited out, the other is a scope an operator has to grant and no
amount of waiting fixes it — so a `403` counts as rate limiting only where the response itself says so,
by asking to be retried or by reporting nothing remaining. `429` is unambiguous and is always a wait.
The rate-limit headers on a refusal are read before the status is judged, because a `403` for a spent
allowance is the response whose numbers matter most.

## 6. What a deployment status means

GitHub's deployment API **designates no terminal state**. It documents seven values a status may take
and says nothing about which of them end a deployment, so any connector reading them is deciding — and
a decision made implicitly, scattered across call sites, is one nobody can review. This table is the
decision. A test parses it and compares it to the code in both directions, so a row here that the
process does not honour fails the build, and so does a state the process rules on that this page does
not publish.

| state | outcome | why |
|---|---|---|
| `error` | failed_attempt | as `failure`; GitHub distinguishes the two by cause rather than by outcome |
| `failure` | failed_attempt | a deploy was attempted and did not land; nothing in production moved |
| `inactive` | property | a newer deployment superseded this one, which is a fact about the rollout that already happened rather than a rollout of its own (FR-022) |
| `in_progress` | nothing | nothing in production has moved yet |
| `pending` | nothing | nothing in production has moved |
| `queued` | nothing | nothing in production has moved |
| `success` | rollout | something is running in production that was not running before |

Three consequences worth stating plainly, because each is a place a reader could reasonably expect
something else:

**A failed deploy is recorded and is not a change.** An operator asking what changed around 14:03 wants
to know a deploy was attempted and did not land — but nothing in production moved, and a ROLLOUT change
for it would put a change node on the graph for an event that changed nothing.

**`inactive` is a property of the rollout that already happened.** Read as a rollout of its own it would
double every deploy, and date the second one at the moment the *first* stopped serving. An `inactive`
with no preceding success yields nothing at all: there is no rollout for it to be a property of, and
attaching it to one would invent it.

**A state outside this table produces nothing, and is counted under its own spelling.** A connector that
guessed would eventually type a new state as a production change on the strength of its name. The count
is per spelling because "4 excluded" cannot tell a configured filter doing its job from a vocabulary
this connector has misread.

### 6.1 When a rollout happened

The valid time is the platform's **completion instant** — the moment the `success` status was recorded.
Not the run's start, not the build instant, and not the instant this connector polled: the first two
are before anything was serving, and the last dates a change to when we noticed it (FR-012, SC-003).

Where GitHub reports a success with no instant, the valid start is recorded as **unknown** rather than
filled in. Where a deployment carries more than one success, the earliest is the rollout's instant —
that is when it first went live — and the fact that there was more than one is reported, because FR-028
keys a re-run as a new change and two successes against one deployment id is something to surface
rather than average.

## 7. The startup gate, and the one call that is not a read

Before anything is emitted, the connector proves the credential read-only and reports what it is
scoped to. Both answers are on the installation token's own response — the granular `permissions`
object and `repository_selection` — so `feed github --dry-run` answers *"is this credential read-only,
and what can it reach"* while **issuing no operation from §2 at all**.

That makes this gate the **strong** form of FR-003: the permissions are the platform's own statement,
checked on every cycle. The Vercel connector's rests on an operator's assertion, because Vercel reports
nothing about write capability — and a checkpoint that printed "read-only: yes" for both would flatten
the stronger claim into the weaker one, which is why the evidence is printed as `platform_reported` or
`operator_asserted` rather than as a verdict
([contracts/read-only-operations.md](../../specs/004-deploy-feeders/contracts/read-only-operations.md) §3.1).

### 7.1 Minting a token is a POST, and it is not on the surface

A GitHub App obtains its credential by POSTing to `/app/installations/{id}/access_tokens`. §2 refuses
every method but `GET` and `HEAD`, so that call is **not** on the published surface and must never be:
a surface containing a POST is not a read-only surface, and the test asserting its absence is what
makes the whole claim checkable.

This is a distinction rather than an exception. §2 governs what the connector reads **from the estate
it observes**. Minting a token is not reading the estate — it is how the connector authenticates, it
changes nothing an operator runs, and its result is a credential rather than an observation.
Constitution VII forbids writes to production systems; obtaining a token writes to no system anybody
deploys.

### 7.2 The App's private key is not a flag

There is no flag for a path to it. A connector that read a private key from a file would hold it in its
process far longer than the single request that needs it, and would put its path in a command line and
a shell history. The assertion is supplied instead, in one of two ways:

- `--app-assertion` takes one short-lived App JWT. It serves `--dry-run` and `--once`, but not a long
  run: GitHub caps an assertion at ten minutes.
- `--app-assertion-command` names a **credential helper**: a command run each time the installation
  token is renewed, which prints a fresh JWT on stdout. This is the shape of git's credential helpers
  and kubectl's exec plugins. The key stays wherever the helper keeps it: a KMS signer, a vault, an
  agent. The helper's stdout is never logged.

### 7.3 Every renewal is proved again

An installation token lasts an hour, so a live run renews it, five minutes before expiry. **Every
renewal passes the same read-only check as the first.** An operator can change an App's permissions at
any time; a connector that checked at 09:00 and kept renewing would be running at 15:00 on whatever it
had been granted since. A renewal the check refuses ends the run, with the credential exit code. It
does not just make one cycle partial, because FR-003 is not a property that may lapse for a cycle.

## 8. The live cycle

`feed github` without `--replay` polls every `--interval` (five minutes by default). Each cycle reads,
in this order:

1. `GET /rate_limit`, the cycle's opening reading (§5).
2. The installation's repositories.
3. Per repository, in name order:
   - the workflow runs created in the window;
   - the deployments of each production environment in `--environments` or the `--map` file;
   - each deployment's statuses;
   - the releases published in the window.
4. A poll marker, stating whether the cycle read everything it meant to.

The cycle produces **the same payloads a recording holds**. A test proves it: GitHub answering with
`github-deployment-01`'s own bodies produces that fixture's `events.jsonl` byte for byte.

- **The window.** It runs from the last complete cycle's start, less one reordering window. The first
  cycle reads `--lookback` back; widen it for a backfill.
- **Unfinished deployments.** A deployment seen without a terminal status (or `inactive`) is re-read
  by id every cycle until it settles, even after the list stops returning it. A rollout that was
  `in_progress` at 14:05 and succeeded at 14:07 is found either way. After 24 hours it is let go; the
  feeder has already counted it deferred.
- **A partial cycle holds the window.** Any failed or truncated read makes the cycle `partial`, and the
  marker names the read and the reason. The next cycle starts from the same instant, so a transient
  502 costs one cycle of latency instead of leaving a hole in the history.
- **Only decoded fields.** Payloads are re-encoded from the transport's domain types, never passed
  through from GitHub. A deployment's `description` and `payload`, a status's `description` and a
  release's `body` therefore never reach a payload or a recording, even before a sanitiser runs.

**`--record` on a live run** records through the sanitiser, in the connector, before anything
touches disk (004 T104, FR-137). The corpus key comes from `$SRE_AGENT_CORPUS_KEY`, and without it the
run is refused before a directory exists. The live feeder gets the platform's payloads; the recording
gets only the sanitiser's output, under the rows and shapes in the sanitisation contract §2.4. The
recording's events are derived from those sanitised payloads, not recorded from the live run.
