# The Sanitisation Contract

> **Published as part of feature 003** (FR-134). Feature 005 (Datadog) shares it rather than
> writing a second one, and its version is recorded in **every** fixture manifest and in every
> `RedactionPolicy` a backend declares.

**Version**: `1.0.0` | **Feature**: 003-gcp-integration | **Date**: 2026-09-20

This document states, per field and per label key, exactly one of three dispositions: **recorded
verbatim**, **replaced by a keyed pseudonym**, or **dropped**. A field with no disposition is not
a field that defaults to safe — it **fails the commit**, because a default is how the next field
nobody thought about gets recorded.

---

## 1. Why the three dispositions are not two

The obvious design has two: keep or remove. It fails on the thing that makes a recording worth
having. A digest's **join keys** are what let its answer be correlated with the graph and with
other digests, and *a digest whose keys do not join is evidence about nothing*
(`telemetry-backend.md` §2). Drop the revision name and `errors_by_version` stops being able to
say "the new revision is failing and the old one is not" — which is the single most useful
sentence the corpus contains. So infrastructure identifiers must survive in a form that is stable
across the whole corpus, and that is the pseudonym.

The reverse mistake is the one that matters more. **A hashed principal email address is still
personal data.** It is stable, it is linkable, and it is reversible for any address an attacker can
guess — which is every address at a company whose domain and naming convention are public. So
people identifiers are **dropped, never hashed** (FR-135), in live mode as well as in recordings,
and the scan asserts zero people identifiers *in any form, hashed included* (SC-019).

---

## 2. The dispositions

### 2.1 Dropped — never recorded, in any form

| field | note |
|---|---|
| principal email addresses, user identifiers, display names | including `protoPayload.authenticationInfo.principalEmail` and every delegation entry beside it |
| message sender and recipient addresses | FR-072 |
| message **bodies** | FR-071: never to the graph, disk, a log or any artifact, **including on a failed or aborted run** |
| message subjects, copied verbatim | a subject may be summarised into typed fields; it is not stored as itself |
| quoted threads, attachments | attachments are **not opened** in v1 |
| customer identifiers appearing in log samples | |
| secret material; configuration values classified sensitive | FR-034 |
| every free-text field not on the §2.2 allowlist | FR-136 |

### 2.2 Recorded verbatim — the allowlist, and it is short

| field | why it is safe |
|---|---|
| GCP region names (`europe-west1`, …) | a closed, public vocabulary |
| database engine and version (`POSTGRES_16`, …) | public product identifiers |
| GCP API method names (`google.cloud.run.v2.Services.UpdateService`) | public API surface; and FR-039 requires the operation name be recorded for "other"-kind changes |
| change kinds, actor kinds, alert states, announcement states, typed outcomes, failure reasons, cost classes | published closed enumerations |
| the **rollback marker** (`change.rollback`) | a boolean about structure. There is no value a boolean could carry that names anybody, and recording it as a pseudonym would make a corpus that cannot test FR-016's own clause — that a rollback is flagged (004 T140) |
| percentages in a traffic split, counts, durations, statistics | numbers about structure, not about people |
| the **vendor slug** and **product** from the allowlist | configuration the organisation authored |
| pointer **vocabulary names and versions** | published identifiers |
| timestamps | see §4 |

Everything not on this list and not in §2.1 is §2.3.

### 2.3 Keyed pseudonym — infrastructure identifiers

Project, service, revision, instance, cluster, namespace, host name, IP address, team,
environment, resource names, alert policy identifiers, load-balancer and DNS record names, the
vendor's own notice identifier, and the **deployment or version a rollback restored**
(`change.rolled_back_to`) or **moved away from** (`change.rolled_back_from`, 004 T155).

That last one is taken under kind **`revision`**, and the kind is the decision rather than the
disposition. Property 1 below is what forces it: a rollback and the rollout it undoes are joined
through this value — the restored deployment's identifier appears here on the rollback *and* as the
identifier of the change that first shipped it — and property 2 says two kinds of one name give two
tokens. A mismatch would hand one deployment two pseudonyms, and 004's acceptance scenario, that the
rollback is distinguishable from the rollout it undoes and both appear with their own valid times,
would fail in exactly the sanitised corpus that exists to check it. `revision` is the published kind
for "one built, deployable version of a service", which a Vercel deployment and a Cloud Run revision
both are, so every deployed-version identifier takes it (004 T140, FR-016). `rolled_back_from` takes it
for the sharper version of the same reason: the investigation engine joins it to the rollout that
shipped that deployment to credit the hypothesis naming it, and a second token would cut that join.
The same rule for empty values applies to it.

An **empty** `rolled_back_to` is not an unassigned field. FR-016 makes an empty value alongside
`rollback: true` mean "a rollback whose target we have not been told", never "not a rollback"; the
sanitiser is asked about a value only when the source stated one.

```
pseudonym(kind, value) = kind_prefix + base32(HMAC-SHA256(corpus_key, kind || 0x00 || value))[:12]
```

Four properties, and each is load-bearing rather than tidy:

1. **Consistent across a whole corpus** (FR-135). The same service pseudonymises to the same token
   in a topology payload, an audit entry, a digest join key and a golden — otherwise the graph and
   the digests stop joining and the corpus tests nothing.
2. **Typed by `kind`**, so a service and an instance that happen to share a name do not collide
   into one pseudonym, which would silently merge two entities in the recorded graph.
3. **Keyed**, so the mapping is not recoverable by anyone holding the corpus. **Neither the key nor
   any mapping table is ever committed** (FR-135).
4. **Stable across a campaign, rotated between organisations.** One corpus, one key.

### 2.4 Shapes, for identifiers a token cannot stand in for (004 T104, T106)

A deploy payload carries identifiers that break if a `px_` token replaces them. A GitHub repository or
deployment id is a JSON **number** the connector decodes as an integer. A **commit** is forty hex
characters, and the claim normaliser refuses anything else, so a token there is a commit C8 can no
longer join on. A deployment's **URL** is where GitHub states the deployment's repository. So a
pseudonym row carries a **shape**; every shape is cut from the same keyed HMAC, domain-separated by
kind, and properties 1–4 above hold for each:

| shape | output | a value of the wrong shape |
|---|---|---|
| token (the default) | `kind_prefix + base32(…)[:12]` | — |
| digits | 15 decimal digits, no leading zero; a JSON number stays a number | dropped |
| hex | lower-case hex of the input's own length | dropped: `target_commitish` is a branch name as often as a sha, and a branch name is free text |
| template | the URL with each placeholder segment (`{kind}`, `{kind:digits}`) replaced under its own kind | dropped: a URL of an unknown shape is one whose identifiers nobody located |

`organisation` and `repository` tokens are **case-folded**, because GitHub compares those names without
case. `Acme` in a URL and `acme` in the grant are one organisation, and must be one token.

The deploy feeders' rows are rooted at `github.<payload kind>` and `vercel.<payload kind>`, the address
`Sanitiser.JSON` gives a payload's fields, so none of them can answer for a GCP field. The table
(`ContractPolicy`) is the source of truth. In summary:

- **Dropped:** every login and account id (deployment and status creators, run actors, release
  authors, Vercel's `creator.uid`); a deployment's `ref`; a status's `log_url`, `environment_url` and
  `target_url` (set by the deployer, on any host, and where tokens arrive); a release's `name` and
  `html_url`; a poll marker's `reason`.
- **Pseudonymised:**

  | identifier | kind and shape |
  |---|---|
  | repository ids, and Vercel's linked `repoId` | `repository`, digits |
  | repository names, and Vercel's `link.repo` | `repository`, token |
  | the owning organisation, and Vercel's `link.org` | `organisation`, token |
  | commits, on both platforms | `commit`, hex |
  | GitHub deployment ids, Vercel deployment uids, and the alias request's from/to | `revision`, digits or token |
  | run, release and status ids | `run`, `release`, `deployment_status`, digits |
  | Vercel project ids and names | `project`, token |
  | Vercel variable ids | `resource`, token |
  | the deployment, statuses, run, log and inspector URLs | template |

  The shared kinds are what keep the corpus joined **across the two platforms**: Vercel's `link.org`,
  `link.repo` and `repoId`, and a commit Vercel states, pseudonymise exactly as GitHub's own do. So C8's
  `deploy.commit_sha` join survives sanitisation (T107).
- **Verbatim, and argued:**
  - The platform's **account type** (`type`, Vercel's `creator.type`) is the evidence the actor
    kind is derived from, and it names nobody.
  - **Environment and workflow names** are what the operator's allowlist matches (FR-023, FR-024).
    A pseudonymised `production` is a recording that replays to nothing.
  - A **release tag** is the `deploy.release` key.
  - A variable's **key** is the configuration change's subject. Its value is dropped, in all three
    forms.

  Everything verbatim still passes the person check and the canaries.

---

## 3. Log content

Log entries are stored as **mined templates with their variables masked**, never as raw lines
(FR-136, FR-090). The miner is feature 002's Drain-style miner, so a recorded template and a live
one are produced by the same code.

Where an **exemplar** is kept at all it is redacted by rule, and capped in number and length by
**the same published limits the live digest uses** (FR-136, FR-096) — so a recording can never
contain more than a live answer would have shown. Exemplars are returned only on explicit request
and never by default, in both modes.

---

## 4. Timestamps are not sanitised, and that is deliberate

Shifting or fuzzing timestamps would destroy the only thing the corpus exists to test: whether the
cause was in the graph **at alert time**. Valid-time exactness is asserted to the instant in SC-002
and SC-004. So instants are recorded as they are, and the disclosure they carry — roughly when this
organisation deploys and roughly when it has incidents — is accepted, documented here, and is part
of what a signatory signs off under §6.

---

## 5. Where sanitisation runs

**In the connector, before anything touches disk** (FR-137), which is a statement about the code
path and not about a commit hook. Three call sites, one policy:

| call site | why it is not optional there |
|---|---|
| the **recording** path | the obvious one: no unsanitised GCP payload and no announcement body may reach disk at any point, including during a failed or aborted run. For the deploy feeders it is `deployrecord.Tee` (004 T104): the live feeder receives the platform's payload, the recording's writer is only ever handed the sanitiser's output, and the recording's events are **derived** from the sanitised payloads rather than recorded from the live run, which carries real identifiers |
| the **live digest** path | FR-110: a live investigation must not be able to surface what a recording would not be allowed to keep. People identifiers are dropped in live mode too, and log content is reduced to masked templates before it leaves the process |
| the **model provider** boundary | FR-141: nothing may be sent to a model that would not be permitted into a recording under the same contract and policy version |

A backend that emits a field its declared `RedactionPolicy` does not cover is rejected
(`undeclared_redaction`, `telemetry-backend.md` §6). The policy declared by this feature's backend
names this document and this version.

---

## 6. Commit gates

Four, and they are independent of each other by design:

1. **The independent scan** (FR-138). A secrets-and-entropy scan and a personal-data scan run at
   commit, **implemented separately from the sanitiser**, so that one defect cannot both leak and
   pass. `scripts/check-no-secrets.sh` extended, not a second scanner.
2. **Canaries** (FR-139). Tokens are seeded into the source data *before* a campaign and asserted
   absent from every committed artifact. **A surviving canary fails the commit** rather than
   raising a warning — the distinction that makes the gate real (SC-018).
3. **The signed manifest** (FR-140). Every committed recording carries a manifest naming: the
   sanitisation policy **version**, the content hash of what was signed, the named individual who
   signed it off, the date, and **what was dropped**. Where a second person exists, a second
   signature.
4. **Drop rather than promise.** A payload that cannot be made safe is **dropped from the
   recording and the drop is documented** (FR-140). A fixture is never committed on the promise of
   being cleaned later — which is the promise that is never kept.

### 6.1 Each gate has to be *run* by something

Gate 3 is the one that shows why this needs saying. `sanitise.Manifest.Check` encoded it —
correctly, with tests — and **no command called it**, so for a while a recording could be committed
with no manifest, with a manifest whose hash no longer matched the bytes on disk, or with one
signature where the campaign declared two, and nothing anywhere would object. `scan` and `sanitise`
do not close that: they read the recording's *contents* for people and canaries, and never look at
the manifest.

`fixture campaign verify` is the caller. It reports the three failures separately, because the
remedy differs: nobody signed this (run `sign`); somebody signed *different bytes* from the ones on
disk; or FR-140's second reader has not read it. The signatory count comes from the campaign record
rather than a flag, so a one-person sign-off cannot wave itself through on a team of five.

### 6.2 The manifest records a digest per file, not only one over the whole recording

The single `content_hash` is the gate — it answers *"has anything moved"*. It cannot answer *"which
file"*, and a reviewer told that one of several hundred files was edited after signing has been told
nothing they can act on. So `recording:` is a list of `{path, sha256}` rather than a list of paths,
and a failed verification names the files **edited**, **added** or **removed** since signing.

Both digests come from one read of each file. Reading twice would let the two disagree if the file
changed in between, which is the one case the gate exists for.

It also makes a third state reportable, and distinguishing it matters: when every file matches its
recorded digest and the overall hash still differs, the recording did **not** move — the hash was
written by something other than `HashRecording`, or edited by hand. Reporting that as "a file was
edited" would send a reviewer hunting a change that is not in the files.

---

## 7. The corpus is split

| | real sanitised recordings | synthetic structural twins |
|---|---|---|
| **where** | `sre-agent-private`, private CI | this repository, public CI |
| **what** | the organisation's payloads, sanitised under this contract | the same event shapes, the same sequences, the same digest, coverage and failure contract, with **no identifier derived from the organisation** |
| **suite** | the published conformance suite | the same suite |

Both run the same suite (FR-128). Any claim made in the **public** repository is a claim about the
**twins**, and a property that can only be demonstrated on real payloads is labelled as such rather
than implied.

This is also the answer to a question the split invites: if the twins pass the same suite, why
record real payloads at all? Because constitution VIII says synthetic-only data is insufficient for
a connector to be marked stable, and because the twins are written against the shapes we *expect* —
they cannot fail in the one way that matters, which is GCP returning something nobody predicted.
That failure is what `gcp-*` fixtures recorded from the real estate are for, and it is why FR-130
requires recording to **start** before the campaign scope is agreed.
