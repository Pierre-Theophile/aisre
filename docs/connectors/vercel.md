<!-- SPDX-License-Identifier: Apache-2.0 -->

# The Vercel connector

> **Status: partial.** Filled: the credential and where its scope comes from (§1), the published
> read-only operation surface (§2), what is deliberately never read (§3), and the live cycle and its
> gate (§5). Sections marked _(pending)_ name the task that fills them. A number
> written here that was not measured is worse than an empty cell, because an operator approves this
> page and then stops asking.

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

**Verifying the declared scope, read-only.** Where the operator declares a project scope, the
connector can check it rather than trust it: read one project outside the declared scope and require a
`403`. One call, a published read, and the operator's assertion becomes something checked.

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

The `/v9` on the environment endpoint is **provisional**: it is the version this connector is written
against, and it is confirmed against the live API by the same work that closes research §5.3 (T034,
T095). It is published now because FR-004's question is what the connector is *capable of issuing*, and
an operation held back from the page until it is proven would be an operation an operator never
approved.

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

### What this cannot prove

- **It proves what this connector may issue, not what the API does.** A `GET` that Vercel implements
  with a side effect would pass every check here.
- **It says nothing about the token's other holders.** If the same token is used by something else,
  this process's read-only proof is about this process.
- **The live-run half of SC-007 is open**, as it is for GitHub: what is verified today is the
  capability, over the unit surface, with the recorded request log filled by the fixture work (US2).

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

_(pending — T035–T037, and the usage report.)_ Whether Vercel reports a remaining quota is
[research §5.3](../../specs/004-deploy-feeders/research.md). Where a platform reports none the budget
falls back to a static one, and **the usage report says which of the two it used** (FR-071) rather than
presenting a static budget as a share of something measured.

`feed vercel` bootstraps on a static allowance of 30 calls. Once Vercel's rate-limit headers report a
window, it spends at most `--quota-share` (default 0.5) of what is left, and never the last
`--quota-reserve` (default 20) calls. The reserve binds measured windows only: a static allowance is
not a statement from the platform.

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
