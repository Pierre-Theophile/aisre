# Sanitisation

**Policy version**: `1.0.0` · **Contract**: [specs/003-gcp-integration/contracts/sanitisation.md](../../specs/003-gcp-integration/contracts/sanitisation.md) · **Code**: [`internal/sanitise`](../../internal/sanitise)

This is the operator-facing account of how a recording of this organisation's infrastructure is made
safe to commit, what the gates are, what they refuse, and the two disclosures the design accepts on
purpose. The normative statement is the contract; this document is what you need to run a campaign
and what a reviewer needs to argue with.

---

## 1. Three dispositions, and no default

Every field and every label key has exactly one of three dispositions:

| | what happens | example |
|---|---|---|
| **verbatim** | recorded as it is | a region name, an API method name, a percentage, a timestamp |
| **keyed pseudonym** | replaced by a kind-typed HMAC, stable across the whole corpus | a project, service, revision, instance, host, team |
| **dropped** | never recorded, in any form | a principal address, a message body, a caller IP, any free text |

A field with **no** disposition fails the commit. It does not default to dropped, and that is the
decision most worth understanding, because "default to safe" sounds obviously right.

It is wrong for two reasons. A default means the next field nobody thought about gets a disposition
without a human ever looking at it — and about half the time the safe-looking default is the
destructive one: dropping a join key is how a digest stops being able to say *the new revision is
failing and the old one is not*, which is the single most useful sentence the corpus contains. The
other half of the time, a "kept by default" would be the leak. So there is no default, and an
unassigned field stops the run with a question for a human:

```
sanitise: field "protoPayload.somethingNew" in gcp-audit-01 has no disposition under policy 1.0.0;
assign it in contracts/sanitisation.md §2 and in ContractPolicy — a field with no disposition fails
the commit rather than defaulting either way (FR-134)
```

The one published exception is a **label key**, and it is published rather than implicit: a key that
is not on the allowlist in [`config/gcp.yaml`](../../config/gcp.yaml) becomes nothing at all. A label
namespace is open — anyone with deploy access invents a key — so it cannot be enumerated in advance,
and dropping the unknown is the only defensible answer. An *allowlisted* label with no disposition
still fails, because that is a case a human can actually be asked about.

## 2. People are dropped, never hashed

**A hashed principal email address is still personal data.** It is stable, it is linkable across
every artifact in the corpus, and it is reversible for any address an attacker can guess — which is
every address at a company whose domain and naming convention are public. Hashing it changes who can
read it, not whether it is there.

So people identifiers are dropped, in **live mode as well as recordings**, and the assertion is zero
people identifiers *in any form, hashed included*. Three independent mechanisms carry it, which is
deliberate: any one of them could be edited away.

1. **The table cannot say otherwise.** `Policy.Validate` refuses a policy in which a field naming a
   person is anything but dropped. A future edit that flips one to a pseudonym fails at
   construction — the one place no downstream scan would look.
2. **The call site cannot ask.** `Sanitiser.Field` checks the people vocabulary *before* consulting
   the table, so the guarantee does not depend on the table being right.
3. **The canary proves it.** A person-shaped canary is asserted absent in raw form *and* under every
   pseudonym kind, which is what makes "never hashed" testable rather than merely stated.

There is no investigative loss. What an investigation needs from a change is the **actor kind** —
person, automation, unknown — which is a closed enumeration recorded verbatim. Nothing in "did this
change break that service" is answered by which individual pressed deploy.

Two rows of the table are worth calling out because they look inconsistent and are not:

- **`callerIp` is dropped, not pseudonymised**, although the contract lists IP addresses among the
  pseudonymised infrastructure identifiers. An infrastructure IP names a machine that serves traffic;
  `callerIp` names where a person was sitting when they deployed. Pseudonymising it would keep a
  stable, linkable token for one human's home connection.
- **`managed-by` is pseudonymised**, although it usually holds a tool name (`terraform`, `gcloud`)
  that would be harmless. Nothing in the value distinguishes a tool from a team, and the asymmetry
  decides it: a pseudonymised tool name costs one unreadable token in a golden, a recorded team name
  is a disclosure.

## 3. Pseudonyms, and why they must join

```
pseudonym(kind, value) = px_<kind-tag>_ + base32(HMAC-SHA256(corpus_key, kind || 0x00 || value))[:12]
```

Four properties, each load-bearing rather than tidy:

- **Consistent across a whole corpus.** The same service is the same token in a topology payload, an
  audit entry, a digest join key and a golden. A digest whose keys do not join is evidence about
  nothing.
- **Typed by kind**, and the kind is in the MAC input, not only in the prefix — so a service and an
  instance that happen to share a name do not collide into one token and silently merge two entities
  in the recorded graph. The `0x00` separator keeps the input unambiguous as kinds are added.
- **Keyed.** An unkeyed hash of an identifier drawn from a namespace an attacker can enumerate is a
  lookup table, not a pseudonym. A missing key is a hard refusal, because an unkeyed token looks
  identical to a keyed one in a golden.
- **One corpus, one key**, rotated between organisations.

### The `px_` prefix is not cosmetic

Feature 002's redactor already pseudonymises digest join keys, into `px_` + 16 hex characters, and
330 of those tokens are baked into its committed goldens. Rewriting that function would rewrite a
published corpus, so it stays as it is — and the two must still agree, because the graph this feature
records and the digests that feature answers with are **one corpus**.

They agree by **ordering**. `backend.Redactor.Pseudonymise` returns a value unchanged when it already
carries the `px_` prefix, so a value sanitised here passes through it untouched and one token appears
on both sides. This works in one direction only: a value feature 002 pseudonymised first is an
*untyped* token that this package then leaves alone, and the graph and the digest end up holding
different tokens for one service.

**Sanitise first, always.** FR-137 requires that ordering anyway (in the connector, before disk); this
is a second, independent reason for it. `internal/backends/gcp.SanitiseThenRedact` is the only
ordering the code offers, and `sanitise_test.go` asserts the handoff rather than trusting it.

## 4. Where it runs — three call sites, one policy

Sanitisation happens **in the connector, before anything touches disk**, and that is a statement
about the code path rather than about a commit hook. A hook cannot help: by the time one runs, the
unsanitised bytes have been on a laptop's disk, in a temporary directory and possibly in a crash
dump — and "including during a failed or aborted run" is precisely the case a hook never sees.

| call site | code | why it is not optional there |
|---|---|---|
| **recording** | [`internal/feeders/gcp/record.go`](../../internal/feeders/gcp/record.go), [`internal/feeders/vendornotice/sanitise.go`](../../internal/feeders/vendornotice/sanitise.go) | no unsanitised payload and no announcement body reaches disk at any point |
| **live digest** | [`internal/backends/gcp/coverage.go`](../../internal/backends/gcp/coverage.go) | a live investigation must not surface what a recording could not keep |
| **model provider** | [`internal/investigation/model/sanitise.go`](../../internal/investigation/model/sanitise.go) | nothing may be sent to a model that would not be permitted into a recording |

Each is a **type**, not a step, and each refuses to exist without a sanitiser:
`NewSanitisedRecorder(nil, sink)` is an error, so there is no ordering in which a payload reaches a
sink unsanitised. `vendornotice.Notice` has **no field a body could be stored in** — a reviewer
looking for where the body went finds that there is no field, not a field left empty, which is the
difference between a rule and a convention. And a parse failure does not wrap the parser's error,
because a parse error that quotes its input is a body in a log.

The model boundary deserves its own note: a prompt does not feel like storage, but it leaves the
building, is retained by the provider under its own terms, and may be logged at hops this project
does not control. A principal address in a prompt has a wider blast radius than the same address in a
private fixture. The guard is not optional and there is no degraded mode — the degraded mode *is* the
disclosure.

**Residual**: feature 002's `NewLiveTransport` and `NewRecordingTransport` still exist and are
unguarded, because they are its published seam. FR-141 therefore holds for the paths that go through
`NewSanitisedTransport`, and is a claim about wiring elsewhere. The wiring is asserted where it is
made.

## 5. Log content

Log entries are stored as **mined templates with their variables masked**, never as raw lines. The
masking rules are feature 002's, called rather than reimplemented, so that a recorded template and a
live one are the same string — two implementations would drift, and the direction they would drift in
is a recorded template a live answer would not have shown.

An address becomes `<email>`, an IP `<ip>`, a UUID `<uuid>`. A handle (`@someone`) survives masking,
so a line carrying one is **dropped from the answer and recorded on the manifest** rather than failing
the payload around it — a chatty log line should not cost an hour of recorded topology. Where an
exemplar is kept at all it is redacted by rule and capped in number and length by the same published
limits the live digest uses, so a recording can never contain more than a live answer would have
shown.

## 6. The gates, and their independence

Four gates, independent of each other by design.

### 6.1 The independent scan

[`scripts/check-no-secrets.sh`](../../scripts/check-no-secrets.sh) runs a secrets-and-entropy scan and
a personal-data scan at commit, **implemented separately from the sanitiser so that one defect cannot
both leak and pass**. Separately means what it says: it is a grep, in another language, that shares no
code with `internal/sanitise` and must never import it. If the disposition table grows a hole, these
rules still fire; if these rules have a hole, the table still drops the field.

The personal-data rules match **field names** rather than values, which is the opposite of the
address-shaped rule beside them and deliberate: a sanitised recording has no dropped field at all, so
the presence of the name is the finding — and a name survives the leak a value-shaped regexp misses,
because a surviving `principalEmail` field holding `px_res_abc` is a pseudonymised person and looks
like nothing.

(That sentence is written without the `field: value` punctuation on purpose. The `gcp-principal` rule
matches the field *name* followed by a colon, so a document illustrating the rule with a realistic
payload trips it — which is the rule working. The alternative is an allow marker, and a marker inside
a fenced code block renders as part of the example.)

`internal/sanitise` also has its own in-process check, `AssertArtifact`, which runs on the bytes
before they exist on disk. It is **not** this gate; it shares a defect with the rest of that package
by construction. Both exist; neither substitutes.

The scan has its own test, [`check-no-secrets_test.sh`](../../scripts/check-no-secrets_test.sh), which
plants one sample per rule **and one per alternative inside each rule** — an alternation is exactly
where a typo hides, and a seven-way rule whose sample happens to match alternative five passes while
the other six match nothing.

### 6.2 Canaries

Tokens are seeded into the source data *before* a campaign and asserted absent from every committed
artifact. **A surviving canary fails the commit rather than raising a warning** — a warning on a leak
is a leak with a paper trail.

There is no baseline entry and no allow marker for the canary rule, unlike every other rule in the
scan. An accepted secret placeholder is a judgement a human can make; "this canary survived but it is
fine" is not, because the canary was planted in the place the real value lives.

A campaign that seeded **no** canaries has not satisfied the gate, and `CanarySet.Assert` says so
rather than passing: zero survivors out of zero canaries is vacuously satisfied by a sanitiser that
does nothing.

Person-shaped canaries are checked in **hashed** form too, under every published kind. A canary that
was pseudonymised rather than dropped is invisible to a raw-token scan — the raw token really is gone
— and that is exactly the failure "in any form, hashed included" is about.

Canaries are written at a domain that is deliberately **not** one of the reserved documentation names
(`.example`, `.invalid`, `.test`): those are allow-listed by the personal-data scan as fixture
placeholders, which is correct for an author's `alice@shop.example` and would make the canary test the
allow-list instead of the sanitiser.

### 6.3 The signed manifest

Every committed recording carries a manifest stating:

- the sanitisation **policy version** (`1.0.0`);
- the **content hash** of what was signed;
- the **corpus key fingerprint** — an HMAC of a fixed label under the key itself, so two recordings
  can be known to share a pseudonym space, or known not to, after the key has been rotated away.
  The fingerprint reveals nothing about the key;
- the **named individual** who signed it off, and the date;
- **what was dropped**, by field and reason. A reader learns that a principal was present and
  removed, which is a materially different statement from the field never existing;
- where a second person exists, a **second signature**.

### 6.4 Drop rather than promise

A payload that cannot be made safe is **dropped from the recording and the drop is documented**. One
unassigned field fails the whole payload rather than recording the rest of it. A fixture is never
committed on the promise of being cleaned later, which is the promise that is never kept.

## 7. Timestamps are not sanitised, and that is a decision

Shifting or fuzzing instants would destroy the only thing the corpus exists to test: whether the
cause was in the graph **at alert time**. Valid-time exactness is asserted to the instant by SC-002
and SC-004, so a fuzzed timestamp does not weaken a test — it makes the test unable to run.

The disclosure this carries is real and is stated rather than glossed: a reader of the corpus learns
roughly when this organisation deploys, and roughly when it has incidents. That is accepted,
documented here, and is part of what a signatory signs off under §6.3. It is the only disclosure in
this document that is accepted rather than mitigated, and a campaign's sign-off is the place to
object to it.

## 8. The corpus is split

| | real sanitised recordings | synthetic structural twins |
|---|---|---|
| **where** | `sre-agent-private`, private CI | this repository, public CI |
| **what** | this organisation's payloads, sanitised under this contract | the same event shapes and sequences, with no identifier derived from the organisation |
| **suite** | the published conformance suite | the same suite |

Any claim made in the **public** repository is a claim about the **twins**, and a property that can
only be demonstrated on real payloads is labelled as such rather than implied.

If the twins pass the same suite, why record real payloads at all? Because the constitution says
synthetic-only data is insufficient for a connector to be marked stable, and because the twins are
written against the shapes we *expect* — they cannot fail in the one way that matters, which is GCP
returning something nobody predicted. That failure is what recorded `gcp-*` fixtures are for, and it
is why recording must **start** before the campaign scope is agreed.

---

## Running a campaign

```bash
# Once per campaign. The key is never committed; store it where a human chose to.
export SRE_AGENT_CORPUS_KEY=$(openssl rand -hex 32)

# Seed canaries into the source data BEFORE recording starts, and record what was planted.
aisre feed gcp --seed-canaries --campaign nova-production-2026-09

# Record. Sanitisation is in the path; there is no flag to turn it off.
aisre feed gcp --record fixtures/gcp/

# Before committing: both gates, then the manifest.
scripts/check-no-secrets.sh
scripts/check-no-secrets_test.sh
go test ./internal/sanitise/
```

A campaign that cannot produce a key, cannot name its canaries, or cannot name a signatory should not
start. Each of those is a refusal at minute zero rather than a discovery after a day of recording.
