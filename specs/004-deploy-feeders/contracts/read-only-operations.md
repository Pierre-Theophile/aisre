# Contract: the published read-only operation lists

**Status**: proposed (feature 004)

SC-007 asks for zero writes, "verified from the feeders' own recorded request logs over the full
corpus and the live run", and FR-004 sharpens it: write-incapability must be **verifiable by
inspecting the set of operations the feeder can issue, not only its behaviour on one run**.

That phrasing is the whole design. A run that happened not to write proves nothing about the next
run; what proves it is that there is no operation in the table that writes.

## 1. The shape, carried from feature 003

Feature 003 built this in `internal/gcpx/requestlog.go` and it transfers unchanged in shape:

1. the published operation list is a **Go table**, not a document;
2. every call goes through **one door** that takes the operation and looks it up — an unpublished
   operation is refused **before any quota is spent**;
3. the underlying `take` is **unexported**, so nothing can spend a call without naming what it
   issues;
4. a go/ast test asserts every live reader method issues a constant-named published operation before
   reaching the platform client;
5. the published page and the enforced table are compared **in both directions**, so neither can
   drift.

## 2. What differs here: there is no doorbell exception

003's table has exactly one state change — acknowledging a Pub/Sub doorbell. **Neither feeder here
acknowledges anything**, so both lists are **all reads, no exceptions**, and the test that 003 writes
as "exactly one state change, and the log names it" becomes **"zero state changes"**.

The inbound webhook is a doorbell in the other direction: something calls *us*. It triggers a poll
and its body is never read (FR-053), so it issues no platform operation at all.

## 3. GitHub

Published in `docs/connectors/github.md`. Reads only: the installation's repository selection,
workflow runs, deployments, deployment statuses, releases, and the rate-limit endpoint.

**Out of scope with no configuration that enables it** (spec): dispatching, re-running or cancelling
a workflow; creating or updating a deployment or a status; creating or editing a release; writing a
comment, check or commit status. Secrets and Actions variables are never read in any form.

### 3.1 What each platform will report about its own credential (T032, T033, T034)

Established from the API references, because FR-003's fallback depends on it:

| | can the holder read what the credential PERMITS? | can it read the SCOPE the credential is bound to? |
|---|---|---|
| **GitHub** | **yes** — `POST /app/installations/{id}/access_tokens` returns a `permissions` object with the granular permission keys, alongside `repository_selection`. The connector mints its own installation token, so it sees this every cycle | **yes** — `GET /installation/repositories` returns `total_count`, `repositories` and `repository_selection` on an installation token |
| **Vercel** | **no.** Nothing reports write capability. `GET /v2/user` has a `limited` form meaning the token lacks privileges to read full user data, which is a signal about privileges rather than a permission list | **partly** — `POST /v3/user/tokens` takes `projectId` ("The ID of the project to scope this token to"), and `token.scopes[]` is reported at creation and through `GET /v6/user/tokens`. The `type` values are not enumerated, so the spelling a project scope takes is **not** established |

So the read-only gate is **asymmetric, and says so**: GitHub's is a platform statement the connector
verifies every cycle; Vercel's rests on the operator asserting read-only, which FR-003 permits
precisely for this case and which the checkpoint records as an assertion rather than a fact.

Where an operator declares a Vercel project scope it is checkable read-only: read one project outside
the declared scope and require a `403`. One call, a published read, and the assertion stops being
merely trusted.

## 4. Vercel

Published in `docs/connectors/vercel.md`. Reads only: deployments (list and get), projects, and the
environment-variable **metadata** — keys, environments and versions, never values.

**Out of scope**: promoting, rolling back, redeploying or deleting a deployment; changing an
environment variable or project setting. The decrypted value of an environment variable is never
requested (FR-038).

## 5. Budget

FR-071 wants the budget as a **share of the remaining quota** where the platform reports one.

- **GitHub**: `GET /rate_limit` is free and reports `limit`/`remaining`/`reset`/`used` per resource
  family — right for the cycle's opening reading and the usage report. The `x-ratelimit-*` response
  headers are documented as authoritative per request, and are what the in-cycle budget decrements
  against. Endpoint alone would let a cycle overspend between readings; headers alone lose the other
  families.
- **Vercel**: whether a remaining quota is reported at all is still to confirm — §5.3 answered the
  *scope* question, not this one. Where no remaining quota is reported, the budget falls back to a
  static one and **the usage report says which it used** (FR-071).

A published reserve is left unspent and the feeder yields rather than competes (FR-072). Stopping for
quota is a **typed reason**, distinct from having found nothing (FR-073).
