<!-- SPDX-License-Identifier: Apache-2.0 -->

# The Vercel connector

> **Status: written in full against the code (004 T130), with three things still unestablished.**
> Every section describes what the connector does today, and the operation table in §2 is compared to
> the code by a test. Not established, and said so where each one matters: which regime a token puts
> the connector in (T034 is open, §1); whether Vercel reports a remaining quota at all (§4); and any
> figure a live team would have to supply, since no recording campaign has run (T114 is blocked). A
> number written here that was not measured is worse than an empty cell, because an operator approves
> this page and then stops asking.

What this connector reads, what it costs, and the proof it can only read.

Contract: [`specs/004-deploy-feeders/contracts/read-only-operations.md`](../../specs/004-deploy-feeders/contracts/read-only-operations.md).
Platform facts, as established from the published documentation:
[`research.md`](../../specs/004-deploy-feeders/research.md) §3.

## 1. The credential, and where the scope comes from

A Vercel **access token**. Which *regime* that puts the connector in depends on how you made it, and
the connector reports which rather than assuming one.

**A Vercel token can be pinned to a project by the platform.** `POST /v3/user/tokens` takes a
`projectId` — *"The ID of the project to scope this token to"*. So there are two cases and they are
not equivalent:

| how the token was made | regime | what bounds what the connector can read |
|---|---|---|
| with `projectId` | the platform enforces it | **one project**, and nothing else is reachable |
| without it, for a user or team | configuration within the grant | the operator's configured project list, which is a *narrowing* of what the token could read rather than a boundary anyone holds |

The scoping is **singular** — `projectId`, not `projectIds` — so an estate with several projects is
either several tokens or one unscoped token. Vercel offers no middle term, which is the real difference
from GitHub's repository *selection* rather than the one an earlier draft of this page claimed.

A token's scope is reported back, in `token.scopes[]` at creation and through
`GET /v6/user/tokens`. Each scope item carries a `type`; the reference documents the example value
`user` and does not enumerate the rest, so **the spelling a project scope takes is not established**
and this connector does not guess it. Identifying the authenticating token in that list is by
`prefix`/`suffix`.

**Verifying a declared scope, read-only, is possible and not built.** Where an operator declares a
project scope, it could be checked rather than trusted: read one project outside the declared scope and
require a `403`. One call, a published read, and the operator's assertion would become something
checked. That is T034's design, and T034 is open. Today `feed vercel --dry-run` prints
`token regime: NOT ESTABLISHED` and names what is missing, and a live run reports its scope as the
projects named by `--projects`, "the token may reach more", which is the configuration regime stated
honestly. A command that printed a platform-enforced boundary from a guessed `scopes[].type` value would
make the stronger claim on no evidence.

### What Vercel does not report, and what follows

**Nothing reports whether a token may write.** There is no permission or scope introspection that says
"read-only". `GET /v2/user` has a `limited` form meaning the token lacks privileges to read full user
data, which is a signal about privileges and not a permission list.

So FR-003's fallback applies here and not on the GitHub side: **where the platform cannot report what a
credential permits, the operator must assert read-only rather than the connector assume it.** That
assertion is configuration, it is recorded in the checkpoint, and this page says plainly that it is an
assertion — because a guarantee resting on an operator's word is worth less than one resting on a
platform's, and pretending otherwise is the failure the three-layer gate exists to avoid.

## 2. The published read-only operation surface

Every operation this connector may issue, by name. An operation absent from this list is refused by
`Surface.Issuable` before any quota is spent, rather than by review (FR-004).

Each operation is spelled `METHOD path`, and that spelling **is** the write test: `GET` and `HEAD` are
reads by the definition of the verbs. There is no `POST`, `PUT`, `PATCH` or `DELETE` anywhere below,
and **no declared state change of any kind** — nothing is promoted, rolled back, redeployed, cancelled
or deleted. The surface's state-change count is zero and a test asserts it.

Vercel versions its API in the path, so the version is part of an operation's identity: changing `/v7`
to `/v8` is a change to this published surface, not an implementation detail.

| area | operation | why |
|---|---|---|
| configuration | `GET /v9/projects/{idOrName}/env` | environment-variable keys, environments and versions; never a decrypted value |
| deployments | `GET /v7/deployments` | the rollouts, their target, their state and their production substate |
| deployments | `GET /v13/deployments/{idOrUrl}` | one deployment, when the list window no longer covers a referenced id |
| projects | `GET /v9/projects` | the project a deployment belongs to, which is what maps it to a service |
| projects | `GET /v9/projects/{idOrName}` | one project, by id or name, as the operator configured it |

**The table above and the code are one list.** It lives in
[`internal/feeders/vercel/requestlog.go`](../../internal/feeders/vercel/requestlog.go), and this page
is parsed and compared against it by a test in both directions, area and reason included.

The `/v9` on the environment endpoint is **provisional**. It is the version this connector is written
against, and T095 checked its response schema against the published reference. It has not been
exercised against a live team, which waits on a credential (T114). It is published now because FR-004's
question is what the connector is *capable of issuing*, and an operation held back from the page until
it is proven would be an operation an operator never approved.

### Ready is not live

The one platform fact that changes what this connector emits, and it belongs on the operator's page
because it changes what the graph will say. A deployment carries both `state`/`readyState` and, once
`READY`, a `readySubstate` of `STAGED`, `ROLLING` or `PROMOTED`, documented as tracking whether it has
seen production traffic.

`READY` means **built and available**, not serving. A rollout is therefore `target=production` **and**
`readySubstate=PROMOTED`, and its valid time is the instant it became the production deployment
([research §3.1](../../specs/004-deploy-feeders/research.md)). For the same reason
`isRollbackCandidate` is not a rollback: it says a deployment *can* be rolled back to, not that one
happened.

### The three-layer gate, and why its first layer is the weak one

The same three layers as the GitHub connector ([github.md §2](./github.md)), with one of them
materially weaker. The asymmetry is deliberate and it is printed, not hidden:

| layer | what it checks | where | strength here |
|---|---|---|---|
| 1. the credential | the operator asserted the token is read-only | `assertedVercelGate` in `internal/cli/feed_vercel.go`, over `feeder.CheckReadOnly` | **an assertion, not a proof.** Vercel reports nothing about what a token may write (§1), so a live run is refused without `--assert-read-only`, and the evidence is recorded as `operator_asserted` rather than `platform_reported` |
| 2. the surface | the operation is on the table above, and its method is `GET` or `HEAD` | `pkg/feeder/readonly.go`, with this table in `internal/feeders/vercel/requestlog.go` | as strong as GitHub's. A table containing a write panics at program start |
| 3. the door | every call names its operation to `Issuer.Issue`, which checks layer 2, then the budget, and records the attempt | `pkg/feeder/issue.go`; the transport's one request method, `get`, calls it first and takes the HTTP method from the operation's spelling | as strong as GitHub's |

So on Vercel the read-only claim rests on layers 2 and 3. Those hold whatever the token can do: the
process has no operation to issue that writes. What layer 1 adds on GitHub is a second, independent
barrier: a process that somehow issued a write would still hold a credential the platform refuses it
with. Here that barrier exists only if the operator made the token read-only, and nothing checks that
they did. A write-capable token is refused on GitHub and accepted on Vercel, and the checkpoint's
`operator_asserted` is where that difference is recorded.

### What the gate cannot prove

- **It proves what this connector may issue, not what the API does.** A `GET` that Vercel implements
  with a side effect would pass every layer.
- **Layer 1 proves nothing about the token.** It records that somebody said so. A write-capable token
  given with `--assert-read-only` runs, and nothing in the process can detect it.
- **Layer 3 proves the method, not the exact path.** The HTTP method comes from the operation, so a
  published `GET` cannot be sent as anything else. But the transport writes each concrete path beside
  the operation constant (`c.url("/v7/deployments", …)` with `OpDeployments`), rather than deriving it
  from the template as the GitHub transport does. Tests pin the paths the live cycle issues (the
  project, its environment metadata and the deployments list, in `poller_test.go` and
  `transport_test.go`), but nothing ties every constant to its path. A mismatch would still issue only
  a read, but it would be metered and logged under the wrong row, and it could be a read this page does
  not publish.
- **The one-door property is structural, not scanned.** `transport.go` has exactly one method that
  performs a request, and it calls `Issue` first. No go/ast test asserts this, unlike the GCP connector.
- **It says nothing about the token's other holders.** If the same token is used by something else,
  this process's read-only proof is about this process.
- **The recorded-request-log half of SC-007 does not exist yet, and neither does the live-run half.**
  As for GitHub, `Issuer.RequestLog()` is kept in memory and `feed vercel` neither prints nor persists
  it, and a replay issues no request. What is verified today is the capability, over the unit surface.

## 3. What is deliberately never read

- **The decrypted value of an environment variable, ever** (FR-038). The key, its environments and its
  version are what a CONFIG change is about; the value is the secret itself, and a feeder that asked
  for it would be putting production credentials into the graph. The endpoint on the surface returns
  metadata, and the query that would decrypt is not on it.
- **Build and function logs.** A deployment's state is a fact about a rollout; its build output is the
  application's own text.
- **The deployed files.** The file-listing and file-contents endpoints are absent: the graph joins on
  the commit sha, not on what was built from it.
- **Team members and access groups.** The actor on a change comes from the payload recording it
  (`creator.type`, FR-037); who else can log in is a different subject.

Writes that are out of scope with **no configuration that enables them**: promoting, rolling back,
redeploying, cancelling or deleting a deployment; changing an environment variable or a project
setting.

## 4. What it costs

**No calls-per-hour figure is published here, because none has been measured.** FR-076 asks for one
against an estate of a stated size, matched against a recording, and no recording exists (T114). Nor
does a live run report its own usage: the budget keeps per-family spending and a per-operation request
log (`Issuer.Report()`, `Issuer.RequestLog()`), but `feed vercel` prints neither. FR-075's usage report
is unwired, for this connector as for GitHub.

### The shape of a cycle

What can be said without measuring is which reads a cycle makes and what each grows with. For each
followed project (§5):

| read | calls per cycle | grows with |
|---|---|---|
| `GET /v9/projects` | 1, and only when there is no `--projects` mapping | nothing |
| `GET /v9/projects/{idOrName}` | 1 per followed project | projects followed |
| `GET /v9/projects/{idOrName}/env` | 1 per followed project | projects followed |
| `GET /v7/deployments` | 1 per followed project, filtered by the platform to `target=production` and created since the window opened | projects followed |
| `GET /v13/deployments/{idOrUrl}` | 1 per production deployment carried unsettled, for up to `PendingHorizon` (24 h), and 1 per new `lastAliasRequest` naming a deployment | unsettled rollouts, promotions and rollbacks |

At the default `--interval` of five minutes, the per-hour figure is at most twelve times the per-cycle
one: the interval is a wait between cycles, so a slow cycle makes fewer of them. The preview filter is
sent to the platform, so a preview deployment is one this connector never pays for.

**Every list is one request, and that is a limit.** The deployments and projects lists are read as a
single page. The transport does not follow Vercel's `pagination.next`, and the poller sends no `limit`,
so each list returns whatever the platform's default page holds. A window with more production
deployments for one project than one page returns is read short, and the cycle is **not** marked
partial for it: nothing in the response is checked for a further page. The same holds for a team with
more projects than one page returns, when there is no `--projects` mapping. This is how the code reads,
not something a live team has shown, and Vercel's default page size is not established on this page.

### How the budget paces it

Whether Vercel reports a remaining quota at all is **not established**. The contract leaves it open
([contracts/read-only-operations.md](../../specs/004-deploy-feeders/contracts/read-only-operations.md)
§5); research §5.3 answered the scope question, not this one. So the budget is written for both
answers, and the usage report says which one it used rather than presenting a static allowance as a
share of something measured (FR-071).

- **If Vercel sends `x-ratelimit-limit` and `x-ratelimit-remaining`**, the transport reads them from
  every response, including a refusal. From then on an operation is metered against what Vercel said is
  left: at most `--quota-share` (default 0.5) of it per window, and never the last `--quota-reserve`
  (default 20) calls. Vercel is not known to name its bucket the way GitHub's `x-ratelimit-resource`
  does, so such a reading is filed under the family `unnamed`.
- **Before any response has said anything**, an operation is metered against the static allowance,
  which `feed vercel` sets to 30 calls. The reserve does not apply to it, because a static allowance is
  the operator's bootstrap and not a statement from the platform.

**If Vercel turns out to send no rate-limit headers, a long run stops.** The static allowance is not
per cycle. It is spent down across the run and refilled only by a platform reading, and without headers
none arrives. A run following one project makes at least three calls per cycle, so it would spend the
allowance in at most ten cycles and report every cycle after that as a quota stop. The stop is typed
and the checkpoint says so, which is the right failure. It is still a failure, and whether it happens
depends on the unestablished fact above. This is how the code reads today; no live run has tested it.

### What a refusal costs

A non-200 from Vercel is recorded with the platform's `Retry-After` and whether its headers reported
nothing left, and it makes the cycle **partial**. The next cycle re-reads the same window. Unlike the
GitHub connector, nothing waits out a `429` inside the cycle: the retry comes at the next interval,
whatever `Retry-After` asked for. FR-074's "wait at least as long as the response asks" is therefore met
only when `Retry-After` is shorter than the interval, and resuming is at the granularity of a whole
window rather than a page.

## 5. The live cycle, and the gate it runs behind

`feed vercel` without `--replay` polls every `--interval` (five minutes by default). It follows the
projects named by `--projects`, or every project the team lists when there is no mapping. For each
project, in id order, a cycle reads:

1. **The project, by id** (`GET /v9/projects/{id}`) rather than from the listing, because only the
   single read states `lastAliasRequest`, which is where Vercel says a rollback or a promotion
   happened (research §5.4). A listing-only cycle would never see a rollback.
2. **Its environment-variable metadata.** Never a value, as §3 says.
3. **Its production deployments created in the window**, with the target filter sent to the platform.

The cycle ends with a poll marker. Its payloads are the ones a recording holds: a test serving
`vercel-config-change-01`'s own bodies gets that fixture's events byte for byte.

- **A rollout is a state, so unsettled deployments are followed.** `readySubstate=PROMOTED` is what
  makes a deployment a rollout, and the listing's window is on creation. A deployment seen staged at
  14:00 and promoted at 14:18 is not listed by the 14:20 cycle, so every unsettled production
  deployment (not PROMOTED, ERROR, CANCELED or DELETED) is re-read by uid each cycle, until it settles
  or for 24 hours.
- **A new alias request reads the deployment it names.** This is how a promotion of, or a rollback to,
  a deployment built last week is seen at all.
- **Promotions are dated by the poll.** PROMOTED carries no instant (research §5.2), so a promotion is
  dated `promotedAt` where the metadata states it, and otherwise by the poll that saw it.
- **A partial cycle holds its window.** A failed read makes the cycle partial and names the read; the
  next cycle re-reads the same window.
- **`meta` is narrowed.** A deployment's `meta` and `attribution.commitMeta` are cut down to the keys
  the mapper reads (the commit and `promotedAt`). The commit message and the author's name and login,
  which a Git provider puts beside the commit, stop at the poller.

**The gate is the operator's assertion, and it says so.** Vercel reports nothing about what a token may
write, so the platform-reported proof GitHub gets (github.md §7) is not available. A live run is
refused unless `--assert-read-only` is given, and the gate then records `operator_asserted` rather than
`platform_reported`. What holds regardless of the token is the surface: every operation this connector
can issue is a GET, and the query that decrypts a variable is not among them.

The Vercel credential flag is `--vercel-token`. It was `--token`, which shadowed the graph's own global
`--token`, so a live run could authenticate to Vercel and then fail to reach the graph.

**`--record` on a live run** records through the sanitiser, in the connector, before anything
touches disk (004 T104, FR-137). The corpus key comes from `$SRE_AGENT_CORPUS_KEY`, and without it the
run is refused before a directory exists. The live feeder gets the platform's payloads; the recording
gets only the sanitiser's output, under the rows and shapes in the sanitisation contract §2.4. The
recording's events are derived from those sanitised payloads, not recorded from the live run.

**There is no inbound webhook.** Nothing in this connector listens for a Vercel notification, so there
is no doorbell to forge and no body to refuse. Polling is the only transport, which is also the source
of truth on GitHub, where a doorbell exists and is not yet wired (github.md §4).
