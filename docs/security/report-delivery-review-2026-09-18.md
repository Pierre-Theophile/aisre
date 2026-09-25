<!-- SPDX-License-Identifier: Apache-2.0 -->

# Security review: the first write path, and the `investigator` role

**Date** 2026-09-18 · **Task** T116 · **Feature** 002, investigation engine
**Authorities** constitution v1.1.0 Principle VII (the confined report-delivery exception),
spec FR-008, FR-038, FR-057f, FR-066, plan F6/F10, ADR-0005 D8.

This is a review of the only code in the project that writes anywhere but its own stores, and of
the only new authority the feature adds to the identity model. It is written to be read against
the code: every check names the file, the finding, and either the fix and its test, or the reason
the property already held.

---

## 1. Threat model

**What is being protected.** Three things, in order of how badly they would be missed.

1. **The production estate.** The project reads it and must never act on it. One write path now
   exists — posting an investigation report into the place the incident is being handled — and
   the constitution grants it as a *confined exception*, not as a capability.
2. **The record.** An investigation is immutable once concluded (FR-007) and is the ground truth
   the evaluation corpus learns from. A path that could rewrite, block or silently drop part of
   that record is as serious as a write to production.
3. **The people and the telemetry in the corpus.** Incident fixtures are shared with graders and
   replayed by strangers. A person's e-mail address or a raw log line in one is a disclosure that
   git keeps for ever.

**Who is assumed hostile.** Anyone who can reach the RPC surface with a credential of some kind;
a connector author who writes a Sink; an operator who misconfigures a deployment; and a fixture
author in a hurry. The model does *not* assume a hostile database or a hostile reviewer with
commit rights — those are outside what code can defend.

**What an attacker would try.**

| # | Attack | Where it would land |
|---|--------|---------------------|
| A1 | Use the delivery credential to read production telemetry | `render.Credential` |
| A2 | Widen the delivery credential's scope by configuration | `render.NewCredential` |
| A3 | Point a delivery at a system that serves production traffic | the sink registry, the target |
| A4 | Make delivery block, or fail, a conclusion | `render.Deliver`, `store.RecordDelivery` |
| A5 | Turn one message into a stream, or overwrite another investigation's answer | the delivery identity |
| A6 | Read the delivery credential out of a log, the store or a trajectory | the recorders |
| A7 | Reach ingestion or entity resolution with an `investigator` token | `server.Require` |
| A8 | Call an investigation RPC anonymously, or with a feeder token | the auth interceptor |
| A9 | Commit a secret, a person or a raw telemetry body in a recording | `scripts/check-no-secrets.sh` |

---

## 2. One target, one message, edited in place

*Constitution VII: "post, and later edit in place, its own investigation report". FR-057f:
"updated **in place** … rather than re-posted as a stream of messages".*

### Finding 1.1 — two investigations shared one message (defect, fixed)

`render.LogSink` remembered the last rendering under `targetSystem|targetRef`, while the ledger's
canonical row identity (`store.DeliveryIDFor`) is `(investigation, target system, target ref)`.
Two investigations of the same incident channel — the ordinary case for a re-opened investigation
(FR-057b), which is a *new* investigation linked to the old one — would therefore mint the same
message reference, and the second would edit away an answer a human had already read.

**Fix.** `DeliveryRequest.Identity()` and `render.DeliveryIdentity()` derive the delivery's
identity exactly as the ledger does, the sink keys on it, and `LogSink.Last` takes the
investigation id. **Tests** `TestOneInvestigationOneMessagePerTarget` (render),
`TestDeliveryIdentityMatchesTheLedgerRowIdentity` (store) — the second pins the two derivations to
each other, since neither package may import the other.

### Finding 1.2 — the reference was remembered, never derived (weakness, fixed)

`PreviousMessageRef` comes from the investigation's `deliveries`, which are written by
`store.RecordDelivery`. Nothing outside a test calls `RecordDelivery` yet — the v1 transport is a
stub — so in practice every `investigate report --deliver` ran as a *first* delivery. Had the sink
minted a random reference, that would have been a new message each time: exactly the stream
FR-057f forbids.

**Fix.** The v1 sink derives its reference from the delivery identity rather than minting one, so
a caller that has forgotten the previous reference still edits the one message. **Test**
`TestRedeliveryEditsInPlaceWithoutRememberedState`. A real transport that mints its own reference
must persist it; see residual risk R1.

### Finding 1.3 — a failed delivery never blocks a conclusion (holds)

`Deliver` returns a `DeliveryResult` and no error, so a caller cannot write
`if err := Deliver(...); err != nil { return err }`. `store.ConcludeInTx` (lifecycle.go) mentions
no delivery at all, and `TestDeliveryIsEditedInPlaceAndNeverBlocksAConclusion` concludes an
investigation after a failed delivery. Holds; no change.

### Finding 1.4 — a panicking connector produced an unrecordable result (defect, fixed)

`Deliver` recovered from a panicking sink with `defer func() { _ = recover() }()` and an unnamed
return, so the caller got the **zero** `DeliveryResult`: outcome `""`. That is not one of the three
outcomes; `store.RecordDelivery` refuses it, and the CLI printed `delivery failed and was
recorded:` with an empty reason. A connector bug therefore cost the ledger its record of what
happened — the one thing a failure is supposed to leave behind.

**Fix.** A named return; the recover writes `OutcomeFailed` with the panic value as the failure
detail, plus the digest and the instant. **Tests** `TestAPanickingConnectorIsARecordedFailure`
(render), `TestEveryOutcomeTheRendererProducesIsStorable` (store, against a real database).

---

## 3. A credential that can do nothing else

*Constitution VII: "a credential scoped to writing that report and nothing else, separate from
every read credential".*

### Finding 2.1 — the scope is checked at the sink (holds)

`LogSink.Deliver` calls `cred.Secret(DeliveryScope)` before doing anything, so a credential whose
purpose is anything but `report:write` fails at the transport and not at a review. `NewCredential`
refuses any other scope outright, so the exception cannot be widened by configuration (A2).
`TestDeliveryCredentialCannotBeUsedForReading`. Holds.

### Finding 2.2 — a credential for one place could write to another (defect, fixed)

`Credential.TargetSystem` documented that "a credential for one place is not a credential for
another", and nothing checked it: a credential minted for `slack:acme` would have been handed to a
sink delivering to `jira:other-company`, because the target travels in the *request* and the
credential was only consulted for its secret. On the v1 no-op sink that is harmless; on a real
transport it is a report posted into a place its credential was never granted (A3).

**Fix.** `Deliver` refuses when `cred.TargetSystem != req.TargetSystem`, before the transport sees
the secret, with `ErrCredentialTarget`. **Test**
`TestACredentialForOnePlaceIsNotACredentialForAnother`, which also asserts the transport was never
reached.

### Finding 2.3 — no read credential can become a delivery credential (holds, test added)

The two are separate *types*: `render.Credential` has no constructor from a bearer token, and the
CLI reads `$SRE_AGENT_REPORT_TOKEN` (`EnvDeliveryToken`), never `$SRE_AGENT_TOKEN` (`EnvToken`).
The absent variable yields `not_configured`, never a fallback. There was no test for the
*non*-fallback, which is the part a refactor would silently break. **Test added**
`TestDeliveryNeverFallsBackToTheReadToken` (cli).

The three server roles are refused by construction: a `reader`, `feeder` or `investigator` token
is a bearer string for the graph's own RPCs, and there is no code path that turns one into a
`render.Credential` — `NewCredential` demands the literal scope `report:write`, which no role
name spells.

### Finding 2.4 — the secret is never logged, stored or recorded (holds, tests added)

- `Credential.String()` redacts; `TestDeliveryCredentialNeverPrintsItsSecret`.
- `LogSink` logs the investigation, the target, the message reference, the digest and the byte
  count — not the secret and not the report body. **Test added** `TestLogSinkNeverLogsTheSecret`,
  which asserts both absences.
- The CLI prints neither on any path. **Test added** `TestTheDeliveryCredentialIsNeverPrinted`.
- The store: `investigation.report_deliveries` has no credential column, and
  `store.DeliveryRecord` no credential field (`grep -rn "Credential" internal/investigation/store`
  is empty). The table's own comment says what it stores and why the body is not in it.
- The trajectory and world recorders: the engine imports `render` only for the *report* —
  `render.NewReport` and `render.ResolutionsFor` in `internal/investigation/runner/investigate.go`
  — and never for `Deliver`, `Credential` or any sink. The delivery credential is read in
  `deliverReport` (cli) and never leaves that function; `grep -rn Credential
  internal/investigation/store` is empty, and the only environment variable read anywhere in
  `internal/investigation` is the *model* credential
  (`internal/investigation/model/{provider,mistral}.go`).

---

## 4. Never a production system

*Constitution VII: "the surface MUST NOT be a system that serves production traffic or holds
production configuration".*

### Finding 3.1 — there was no sink registry at all (gap, fixed)

`deliverReport` constructed `render.NewLogSink(nil)` inline. There was nothing to name a
transport, and therefore nothing that could refuse one: the moment a second sink existed, choosing
it would have been a code change or a silently defaulted string.

**Fix.** `render.PublishedSinks()` lists what this build admits — `log`, and only `log` — and
`render.NewSink(id, logger)` resolves it. There is **no default**: an unknown id is
`ErrUnknownSink`, not the sink that happens to be compiled in. `investigate report` takes
`--sink`, defaulting to `log`, and an unpublished value is refused while the command still exits
0 (delivery never fails a command). **Tests** `TestSinkRegistryAdmitsOnlyThePublishedSinks`
(render, including `""`, `"Log"`, `"log "` and `"../log"`), `TestUnknownSinkIsRefusedNotDefaulted`
(cli). The render test also pins the published list to exactly `[log]`, so adding a transport is a
deliberate act with this document to update.

### Finding 3.2 — the v1 transport posts nothing (holds)

`LogSink` is a recorded no-op: it writes a structured log line and returns a synthetic reference.
Nothing leaves the process, which is what makes "the surface must not be a production system"
trivially true of this build. A real transport is a connector's job (ADR-0005 D8), and the
connector — not this package — will have to argue that its surface qualifies.

---

## 5. The `investigator` role

*Plan F10; FR-066: "Anonymous or shared credentials MUST be refused."*

### Everything the role can reach

**RPCs** (`internal/server/investigation.go`, one `Require` as the first statement of every
handler):

| RPC | Role required |
|-----|---------------|
| `Get`, `List`, `Export` | `reader` (so: reader, decider, investigator) |
| `Investigate`, `Declare`, `Replay`, `Reopen`, `SubmitHumanFact` | `investigator` |
| `Review`, `Label` | `decider` **or** `investigator` |

`investigator` implies `reader` and nothing else (`Principal.Has`); it is deliberately *not*
implied by `decider`. There are ten RPCs and no eleventh: `TestNoServedRPCDeliversAReport`
reflects over the handler interface and fails if the method set changes, so a new RPC forces a
role decision and an update to this document.

**CLI paths** reachable with an investigator token: `investigate` (run), `declare`, `get`,
`list`, `export`, `replay`, `reopen`, `fact`, `review`, `label`, `report` and `to-incident`, plus
every read-only `query …`. Operator tooling (`worker …`, `backend list`, `audit coverage`,
`fixture …`, `migrate`, `serve`) is CLI-only and either local or requires its own credential.

**What it writes.** Rows in schema `investigation` through the DAOs, and — through the engine's
own append path, never through an RPC — the investigation event bodies of the 001 contract:
`record_investigation`, `submit_human_fact`, `reopen_investigation` and `label_investigation`,
with the `INVESTIGATED` edge they carry. (The fifth body 002 added to the envelope,
`alert_transition`, is a *feeder's*: nothing under `internal/investigation` constructs one.) Graph
props are confined to the allow-listed `sre.investigation.*` prefix
(`internal/projector/record_investigation.go`), so an investigation cannot write a property
outside its own namespace.

### Finding 4.1 — the role reaches nothing else (holds, test added)

An investigator token is refused by `IngestService` (`IngestBatch`, `RegisterSource` and the
streaming `Ingest`, all `RequireSource` → feeder scoped to one source) and by every
`ResolutionService` decision (`Confirm`, `Reject`, `Split`, `Merge`, all `decider`), and is
admitted by graph reads. There was no test that said so. **Test added**
`TestInvestigatorRoleReachesNothingBeyondItsGrant`.

### Finding 4.2 — anonymous and feeder credentials are refused (holds)

`TestRoleMapping` already asserts, for all ten RPCs, that a feeder token is refused and that an
anonymous call is refused. The interceptor refuses a principal with an empty issuer or subject
(`ErrAnonymous`) before any handler runs, including on streams, so an ingestion stream opened
without a token dies before its first message is read. Holds; no change.

### Finding 4.3 — the role never reaches the report sink (holds by construction)

No RPC delivers a report; delivery is a CLI path holding a credential the server process never
sees. This is the property that keeps the delivery credential off every server, and
`TestNoServedRPCDeliversAReport` is what will notice if that stops being true.

---

## 6. The CI grep guard

*FR-038, and the corpus-sharing argument in §1.*

### Finding 5.1 — the guard covered manifests, not recordings (gap, fixed)

`scripts/check-no-secrets.sh` refused private keys, cloud access keys, bearer tokens, plaintext
`stringData` and non-local database passwords. It said nothing about the three classes a
*recording* leaks: a vendor API key, a person, and a raw telemetry body. `fixtures/incidents/**`
— including every `world/`, `trajectories/` and `golden/` file — was in scope of the scan but not
of any rule that matched what those files contain.

**Fix.** Five new hard rules:

| Rule | Refuses |
|------|---------|
| `api-key` | `sk-…`, `pk-…`, `rk-…`, `glpat-…`, `npm_…`, `hf_…`, and `api_key`/`access_token`/`secret_key` assigned a 20+ character value |
| `people-identifier` | an e-mail address or an `@handle`, outside a closed allow-list |
| `raw-span-payload` | `traceId`/`spanId` with a raw hex value, and OTLP markers (`resourceSpans`, `scopeSpans`, `startTimeUnixNano`, …) |
| `telemetry-instance-id` | a `service.instance.id` value — it names one production process |
| `unredacted-log-line` | a `template` longer than 200 characters, or one still carrying an IP, a UUID or an e-mail |

The people allow-list is closed and spelled out in the script: the graph's own entity-key
namespaces (`payments@otel.service.name` and `redis@app.kubernetes.io` are entity keys, not
people), the RFC 2606 / RFC 6761 reserved documentation names a fixture author may use for a
placeholder (`alice@shop.example` is a placeholder, `alice@shop.io` is a person), and the three
Slack group mentions that name nobody. The filter runs the way the existing published-DSN filter
does: one match outside the allow-list makes the file a finding.

The 200-character template cap is the redaction contract read backwards. A masked template is
short by construction — the longest in the whole corpus today is about 70 characters — and a
`template` value several hundred characters long is a raw log line that never went through the
miner.

### Finding 5.2 — the guard had never been seen to fail (gap, fixed)

**Fix.** `scripts/check-no-secrets_test.sh` copies the guard outside any git work tree, plants one
sample of every refused shape, and asserts (a) the guard fails, (b) each of the eight rules is
named in the output, (c) the output never quotes the value it found, and (d) fixture placeholders
and entity keys are *not* flagged. It scans only the planted tree — the real corpus is scanned by
`check-no-secrets.sh` in the same job, and scanning it twice doubled the slowest step in the lint
job (~100 s over the corpus as it stands) for no extra information. `ci.yml`'s `lint` job runs
`check-no-secrets.sh` and now `check-no-secrets_test.sh`, on `pull_request` to `main` (and on push
to `main`), so both run on every pull request.

Re-run at the end of this task, after Track M's fixture re-recording: **clean**.

---

## 7. Residual risks

| # | Risk | Why it is accepted, and what would close it |
|---|------|--------------------------------------------|
| R1 | **Nothing writes the delivery ledger yet.** `store.RecordDelivery` has no caller outside tests: the CLI holds the result and prints it. `update_count` therefore stays at zero and `first_delivered_at` is never set in a real deployment. | The v1 transport is a stub that posts nothing, so there is nothing to record about. Closed when a transport connector ships: it must record each attempt, and the reference it mints must be persisted rather than re-derived (finding 1.2). |
| R2 | **The target is taken from a declared symptom's origin** (`deliverySplit`), which is data a human supplied at declaration time. A declaration naming an unexpected origin would aim the delivery at it. | Bounded by finding 2.2 — the credential must be minted for that same target system — and by the sink registry. A transport connector should additionally hold an allow-list of the places it may write to. |
| R3 | **`LogSink` keeps the last rendering in memory** for the life of the process. A long-lived server delivering many reports holds many report bodies in RAM. | The sink is a test and demonstration driver; the CLI constructs one per invocation. A real transport keeps nothing. |
| R4 | **The grep guard is a grep.** It catches the shapes this project produces. A novel credential format, a person's name written as prose, or a telemetry body in a field it does not know about will pass. | Stated in the script's own header and unchanged by this review: anything cleverer would be trusted, and a trusted scanner that is wrong is worse than a grep that is honest. The rules grow when a new shape is found. |
| R5 | **`Deliver` recovers from a panicking connector**, which means a connector may leave its own state inconsistent while the investigation carries on. | Deliberate: FR-057f's "never a precondition of a terminal state" outranks a connector's internal consistency. The panic is recorded as a failure with its value in the detail. |
| R6 | **A delivery credential valid for a place where an investigation was never declared** is refused as `not_configured` rather than logged as an attempt. | A misconfiguration, not an attack signal, and the alternative is a log line per unconfigured deployment. |

---

## 8. Verification

```
go test -count=1 ./internal/investigation/render/... ./internal/investigation/store/... \
  ./internal/server/... ./internal/cli/...
scripts/check-no-secrets.sh
scripts/check-no-secrets_test.sh
```

The quickstart run of T118 (`specs/002-investigation-engine/quickstart-run-2026-09-18.md`) found
two further defects on the read path — `investigate get` returned no ledger, and `investigate
fact` did not reopen a concluded investigation — neither of which is a confinement question, both
fixed there with their own tests.

New tests from this review: `TestOneInvestigationOneMessagePerTarget`,
`TestRedeliveryEditsInPlaceWithoutRememberedState`, `TestAPanickingConnectorIsARecordedFailure`,
`TestACredentialForOnePlaceIsNotACredentialForAnother`,
`TestSinkRegistryAdmitsOnlyThePublishedSinks`, `TestLogSinkNeverLogsTheSecret` (render);
`TestDeliveryIdentityMatchesTheLedgerRowIdentity`, `TestEveryOutcomeTheRendererProducesIsStorable`
(store); `TestDeliveryNeverFallsBackToTheReadToken`, `TestUnknownSinkIsRefusedNotDefaulted`,
`TestTheDeliveryCredentialIsNeverPrinted` (cli);
`TestInvestigatorRoleReachesNothingBeyondItsGrant`, `TestNoServedRPCDeliversAReport` (server).
