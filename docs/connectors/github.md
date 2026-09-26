<!-- SPDX-License-Identifier: Apache-2.0 -->

# The GitHub connector

> **Status: written in full against the code (004 T130), with two halves still unmeasured.** Every
> section describes what the connector does today, and the two tables a test compares to the code (§2
> and §6) are the code. What is not here is anything a live installation would have to supply: no
> recording campaign has run (T114 is blocked on a credential and a signatory), so §5 publishes the
> *shape* of what a cycle spends and **no calls-per-hour figure**, and the live-run half of SC-007 is
> open. A number written here that was not measured is worse than an empty cell, because an operator
> approves this page and then stops asking.

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

That last sentence holds for an installation configured as this table says, and the connector does not
check that it was. The startup gate (§7) refuses any permission GitHub reports at a level other than
`read` or `none`; it does **not** refuse a *read* permission beyond these four. An App that was also
granted, say, `issues: read` passes the gate. `feed github --dry-run` prints every permission GitHub
reported, so a wider grant is visible to the operator who runs it, but it is not a refusal. Where it
exists, §3's guarantee for that area rests on the connector's published surface (§2) alone, and not on
the grant.

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

### The three-layer gate

The table above is enforced, not aspirational, because three separate things have to agree before a
request leaves the process (FR-003, FR-004, SC-007). Each is a different fact and each fails
differently, which is why there are three:

| layer | what it checks | where | how it refuses |
|---|---|---|---|
| 1. the credential | GitHub's own `permissions` object, returned when the installation token is minted, holds no value but `read` or `none` | `internal/feeders/github/gate.go` over `feeder.CheckReadOnly` | the run does not start, and the refusal names each offending `permission=level`. It is re-run on every token renewal (§7.3), and a renewal it refuses ends the run |
| 2. the surface | the operation is on the table above, and its method is `GET` or `HEAD` | `pkg/feeder/readonly.go`, with this table in `internal/feeders/github/requestlog.go` | `Surface.Issuable` returns an `UnpublishedOperationError` before any quota is spent. A table containing a write **panics at program start** (`MustReadOnlySurface`), so a binary with a writable surface cannot exist |
| 3. the door | every call names its operation to `Issuer.Issue`, which checks layer 2, then the budget, and records the attempt in a per-operation request log | `pkg/feeder/issue.go`; the transport's one request method, `get`, calls it first and takes the HTTP method from the operation's own spelling | the call is not made, and the log records it as **blocked** (not on the surface) or **refused** (over budget), separately from what was issued |

The page and the table are compared by `TestThePublishedPageAndTheEnforcedSurfaceAreTheSameList`, and
`StateChanges()` on the surface is asserted to be zero.

### What the gate cannot prove

Worth stating narrowly, because a guarantee overstated is worse than one stated plainly:

- **It proves what this connector may issue, not what the API does.** A `GET` that GitHub implements
  with a side effect would pass every layer. Nothing inside a client can see that; the mitigation
  is that every operation is a published read of a published resource.
- **Layer 1 refuses writes, not breadth.** A read permission beyond the four in §1 passes (see §1). The
  guarantee that the connector never *reads* an area it was not published for is layer 2's, not the
  credential's.
- **Layer 1 is checked when a token is minted, not per call.** A token is proved at startup and again at
  each renewal, five minutes before it expires. Between those two instants the connector does not ask
  again.
- **Layer 3 proves the method, not the exact path.** The HTTP method is taken from the operation, so a
  published `GET` cannot be sent as anything else. The concrete URL is built at the call site from the
  operation's own path template (`fill`), and `TestThePathIsDerivedFromThePublishedOperation` asserts
  it. That is a test over today's call sites, not something the door enforces. A future call site that
  issued one published read under another's name would still issue only a read, but it would be
  metered and logged under the wrong row.
- **The one-door property is structural, not scanned.** The contract describes a go/ast test that
  every reader method issues a constant-named operation before reaching the client, which is how the GCP
  connector is held (`internal/feeders/gcp/readonly_test.go`). This connector has no such scan. What
  holds instead is that `transport.go` contains exactly one method that performs a request, and that
  method calls `Issue` first. The one other request in the package is the token mint in `gate.go`, which
  §7.1 explains.
- **It says nothing about the installation's other holders.** If the same App is used by something
  else, this process's read-only proof is about this process.
- **The recorded-request-log half of SC-007 does not exist yet, and neither does the live-run half.**
  The request log is kept in memory by `Issuer.RequestLog()`, but `feed github` neither prints nor
  persists it. A replay reads payloads from disk and issues no request at all, so the fixture corpus
  has no request log to inspect. What is verified today is the capability, over the unit surface: the
  stronger half, and the half a live run could not have given on its own. The live figure needs a
  credential (T114).

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

It is a doorbell in the other direction: something calls us. It means "poll now" and nothing else.
Its body is never parsed, trusted or stored (FR-053), so it issues no GitHub operation of its own and
appears nowhere in §2.

**It is built and it is not wired.** `internal/feeders/github/doorbell.go` implements the endpoint as
an `http.Handler` (T039–T042), and its tests carry every guarantee below. But `feed github` serves no
HTTP endpoint and has no flag for a webhook secret, and nothing connects a ring to the poller. A webhook
configured on the GitHub side today reaches nothing, and the connector runs on polling alone. That is
the correct degraded state rather than a broken one, because polling is the source of truth whether or
not a doorbell exists (FR-052). A doorbell only ever makes a scheduled poll happen sooner.

### Why it cannot copy the GCP doorbell's guarantee

The GCP doorbell *pulls*. It asks a subscription how many messages are waiting and gets back an int, so
no notification body ever enters the process. GitHub *pushes*, and it signs **over the body**, so
verifying the sender requires holding the bytes. The body arrives whether anybody wants it or not. The
guarantee here is therefore narrower, and it lives in the function signatures, where a reviewer can
see it:

| rule | how it is enforced |
|---|---|
| the body is never parsed or stored | it exists as a local inside `ServeHTTP`, bounded at `MaxDoorbellBody` (64 KiB). `Doorbell` has no field it could be kept in, and the package has **no type a webhook payload could be decoded into** |
| only one function sees the bytes | `verify` takes them and returns a **bool**. It cannot hand them anywhere |
| a notification becomes at most a number | `Ring` takes an **int**, as the GCP doorbell's does. Whatever the body said, it becomes a count |
| the sender is verified in constant time | HMAC-SHA256 over the body against `X-Hub-Signature-256`, compared with `hmac.Equal`. The older `X-Hub-Signature` (HMAC-SHA1) is refused on its prefix, because accepting it would let the sender choose how strong the check is |
| a flood costs at most one poll per interval | a token bucket, one token per `DefaultDoorbellMinInterval` (the poll interval), burst `DefaultDoorbellBurst` (1). A thousand notifications inside one interval, forged or genuine, earn one poll |
| failures are counted, by reason | `unsigned`, `bad_signature`, `body_too_large`, `wrong_method` and `rate_limited`, beside `poll_now`, in `DoorbellReport`. A doorbell being probed and a doorbell nobody rings are different facts, and only the counters tell them apart |
| a doorbell without a secret does not exist | `NewDoorbell` refuses an empty secret (`ErrNoSecret`). An unauthenticated webhook is not a weaker doorbell but a public endpoint that makes this connector poll |

The response says nothing about what was decided: `204` for any verified notification, including one
the bucket dropped, and `401` for anything that did not verify. A distinguishable response would let a
prober measure the bucket.

### What the doorbell cannot prove

- **A genuine notification can be replayed, and it verifies.** GitHub's signature covers the body and
  nothing else: no timestamp, no nonce. The endpoint keeps no record of delivery ids, so a captured,
  correctly signed notification sent again passes `verify`. What bounds it is the bucket, not the
  signature. A replay earns at most the one poll per interval that anything else earns, and that poll
  reads the API, so the worst it achieves is one extra read of the truth (SC-012).
- **Constant time is asserted from the source, not from behaviour.** `hmac.Equal` and `==` return the
  same answer for every input and differ only in how long they take, so no functional test can tell
  them apart. A timing test on a shared CI runner would either flake or pass on the vulnerable code.
  `TestTheSignatureComparisonIsConstantTime` therefore reads `doorbell.go` and requires the named
  function. It proves the call is there. It does not prove the comparison's timing.
- **A fixture cannot carry the central claim.** `fixtures/github-doorbell-forged-01` pins the rollout
  and its single version. It cannot show that a second reading of the same window adds no fact, because
  a replay applies a recorded event log that deduplicates on event id before anything compares
  contents. `TestTwoReadingsOfOneWindowProduceOneChange` carries that claim instead: a change's event id
  derives from the deployment and its target, and never from the poll instant, the attempt or a
  notification's delivery id.

## 5. What it costs

**The number an operator approves is not on this page yet, and that is stated rather than filled
in.** FR-076 asks for calls per hour per area at the default cadence against an estate of a stated
size, matched against a recording. No recording exists: the campaign (T114) is blocked on a read-only
installation and a named signatory. `feed github` also does not print its own usage. The budget keeps
per-family spending (`Issuer.Report()`) and a per-operation request log (`Issuer.RequestLog()`), but
nothing in `internal/cli` calls either, so a live run leaves no usage report behind. FR-075's report is
unwired and no task in `tasks.md` names it.

What can be published without measuring is the **shape** of a cycle: which reads it makes, and what
each one grows with. Every figure below is a constant in the code, not an observation.

| read | calls per cycle | grows with |
|---|---|---|
| `GET /rate_limit` | 1 | nothing |
| `GET /installation/repositories` | one per page of 100 repositories in the grant | the size of the grant |
| `GET /repos/{owner}/{repo}/actions/runs` | per repository, one per page of runs created since the window opened (`created>=`) | activity |
| `GET /repos/{owner}/{repo}/deployments` | per repository **and per production environment**, one per page of that environment's **whole** deployment history | **history, not activity**: GitHub takes no time filter on this list, so the window is applied after decoding |
| `GET /repos/{owner}/{repo}/deployments/{deployment_id}/statuses` | one per page, for each deployment created or updated in the window, and for each one still carried unsettled | activity |
| `GET /repos/{owner}/{repo}/deployments/{deployment_id}` | one per deployment carried unsettled that the list no longer returned, for up to `PendingHorizon` (24 h) | unfinished rollouts |
| `GET /repos/{owner}/{repo}/releases` | per repository, one per page of the **whole** release history | **history**: no time filter here either |
| `GET /repos/{owner}/{repo}/actions/runs/{run_id}`, `GET /repos/{owner}/{repo}/releases/{release_id}` | none. Both are published and neither is issued by the live cycle today | — |

Pages are 100 items (`perPage`, GitHub's maximum), and a list stops at `pageCap`, 20 pages. At the
default `--interval` of five minutes, the per-hour figure is at most twelve times the per-cycle one:
the interval is a wait between the end of one cycle and the start of the next, so a slow cycle makes
fewer of them.

**One consequence of the page cap is a limit, not a cost.** The deployment and release lists read
history, so a repository whose deployment history in one environment, or whose release history, runs
past 20 pages makes that list stop at the cap on **every** cycle. A capped list is a truncated read, and
a truncated read makes the cycle `partial` (§8), so the window never advances for that installation and
every checkpoint declares a gap. Nothing is lost unless more than 20 pages of 100 changed inside one
window: GitHub lists newest first, so the pages the window needs come first. But the checkpoint will report partial coverage indefinitely. The code reads this
way. It has not been observed, because no installation that large has been read.

### How the budget paces a cycle

The budget is a share of the **remaining** quota:
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

A family is learned from a response and remembered per operation, so an operation's first call has no
family. That call is metered against **`unobserved`** — not against a guessed `core`, because a guess that turns
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

### 5.4 Stopping for quota, and what is deferred

A call the budget refuses returns a typed `feeder.QuotaYieldError`, carrying the family, what was left,
the reserve and the platform's reset instant. It is never an empty result (FR-073). A list that yields
part-way returns what it read, with the yield as the cause of its `PartialListError`. The cycle becomes
`partial`, and the poll marker's reason names the read that stopped, so "we stopped for quota" and "we
looked and found nothing" do not read alike in the checkpoint.

**The deferral order is the read order, and nothing more deliberate than that.** FR-073 asks for a
*published* priority order. No separate one exists in the code: when the budget yields, what is
deferred is every read after that point in §8's order — the remaining workflow runs, deployments,
statuses and releases of the current repository, then the repositories after it by name. This page
publishes that order because it is the one the code follows. It is not a ranking anybody chose by
value, and a repository named late in the alphabet is the first to go unread.

A primary-limit exhaustion never reaches the in-list wait of §5.3: the budget sees zero remaining in the
response headers and yields at the door on the next call. The wait is for a refusal that arrives with
allowance still on the clock, which is what a secondary limit is. It is bounded by `MaxRateLimitWait`
(ten minutes) across one list, and the refused page is re-read from GitHub's own cursor rather than the
list restarting (FR-074). A refusal that states no instant at all is not guessed at: the list is
reported partial rather than retried on a backoff this connector invented.

The wait lives in the shared paginated read, which the deployment, status and release lists use. The
installation's repositories and the workflow runs are paged by loops of their own, because GitHub wraps
both in an object, and those loops do not wait: a rate-limited page there makes the read partial, and
the next cycle re-reads the same window.

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
checked every time a token is minted — at startup and at each renewal (§7.3), not on each cycle or each
call. The Vercel connector's rests on an operator's assertion, because Vercel reports
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
