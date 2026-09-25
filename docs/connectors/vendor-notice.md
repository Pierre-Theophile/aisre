<!-- SPDX-License-Identifier: Apache-2.0 -->

# The vendor-notice connector

> **Status: skeleton.** Written for the **mailbox owner** — the person who must authorise a process
> to read a mailbox their colleagues use. Sections marked _(pending)_ are filled by the task named
> beside them.

The coverage audit's finding, in one line: **the single most repeated cause across the thirteen
incidents was a vendor notice nobody read** — a maintenance window, a deprecation or a provider
incident, announced by email weeks in advance and rediscovered at 02:10 under a page. This connector
puts that announcement in the graph on the day it arrives.

Contract: [`contracts/vendor-notice-feeder.md`](../../specs/003-gcp-integration/contracts/vendor-notice-feeder.md).
Sanitisation: [`contracts/sanitisation.md`](../../specs/003-gcp-integration/contracts/sanitisation.md).
Allowlist: [`config/vendors.yaml`](../../config/vendors.yaml).

## 1. What is extracted

Typed fields and a message pointer. Nothing else.

| field | from | note |
|---|---|---|
| `vendor` | the **allowlist**, matched on the sender or the feed | configuration is the authority on vendor identity, never a parsed sender domain. A notice from an address nobody allowlisted is dropped, and the drop is counted |
| `product` | the notice's own product field, or the allowlist's `host_products` mapping | where a vendor has several allowlisted products and a notice names none, the record is **unextracted** and says which field was missing — a guess here would attribute a maintenance to the wrong product |
| `kind` | `maintenance` \| `deprecation` \| `incident` | a closed set, validated at extraction. There is no `other`: the taxonomy named `DEPRECATION` and `VENDOR_INCIDENT` on 2026-09-18, so the fallback is dead |
| `window.start`, `window.end` | the announced instants | the end is optional — a deprecation usually has none — and a **vague** start is marked unknown rather than parsed into a guess (FR-069) |
| `notice_id` | the vendor's own identifier, where it states one | this is what lets two sources carrying one notice merge into one change rather than two rows in front of somebody being paged (FR-070) |
| `affected_resources` | the resources the notice names | emitted as **identity claims** so the resolution layer attaches them; the connector invents no `depends-on` edge from them |
| `pointer` | the message id or feed-entry id | an identifier so a human can open the original. It is a pointer and **never content** |

Every one of them is a typed value that passed validation, which is what makes the derived summary
committable: a summary is built **from these fields** and never from the text, so nothing in it can
carry an instruction.

Each field is also length-bounded. A `product` longer than the bound is not truncated — it is an
**unextracted record** naming the field, because a paragraph in a product field is a parser that has
gone wrong and truncating it would hide that.

## 2. What is never stored

**The body is never written to disk. Not redacted, not truncated, not hashed — never written**
(FR-073). This is a property of the code path, not a configuration setting, and
`config/vendors.yaml` carries `store_body: false` as a value that cannot be set to true.

Announcement text is untrusted input from outside the organisation, and the typed extraction
boundary here is the same boundary that governs what may be sent to a model provider. A notice whose
fields cannot be extracted is reported as an extraction failure **with its pointer**, never dropped
and never guessed at — and since most mailbox traffic is not a notice, that path is the normal one
rather than an edge case.

Also never stored: sender and recipient addresses beyond what the allowlist matched on. People
identifiers are **dropped, not hashed** (FR-134).

## 3. The read-only guarantee, per access path

FR-007: the mailbox is read **without altering message state**. What that costs differs per path, and
the differences are not cosmetic.

| path | read-only guarantee | residual risk |
|---|---|---|
| Gmail API, `gmail.readonly` scope | scope cannot mutate; `\Seen` unaffected | domain-wide delegation is a **tenant-wide** grant — see §4 |
| IMAP, `EXAMINE` + `BODY.PEEK` | **both are required.** RFC 9051 §6.3.2 permits a read-only `SELECT` to change per-user state, and `\Seen` is exactly that. `BODY.PEEK` is what does not set it | credentials are per-mailbox; no tenant-wide grant |
| Google Group archive | **none — no API can read it** | see below |

**If the configured address is a Google Group, this connector will read nothing, forever, without
erroring.** A Group is a distribution list; its archive is reachable by no API. The remedy is to
configure a **subscribed member's mailbox** instead. `config/vendors.yaml` carries `kind` per mailbox
so the choice is recorded rather than discovered later.

### Per-user delivery is the normal case (FR-132a)

Google Cloud addresses platform notices to each project's **Essential Contacts** and to its billing
and owner principals. A shared address receives them only where somebody configured it as a contact.
So a connector that read only a shared mailbox would miss exactly the announcements this feature
exists to capture, and per-user delivery is a **first-class path, not a fallback**.

`essentialcontacts.contacts.list` is a read-only call, so the connector can report where the vendor
is configured to send notices for each in-scope project — which tells an operator which mailboxes to
authorise. It is **not implemented** and is recorded here as the follow-up it is: it locates a notice
stream without reading one, which makes it the cheapest way to answer "which mailboxes do we need?"
and it is not on the path of any requirement this feature ships.

An empty notice stream is indistinguishable from a working connector in a quiet week, so a campaign
whose configured mailbox yields nothing **fails loudly at campaign start** (FR-132b) rather than
recording silence as evidence.

## 4. The residual risk this page exists to state

**Google Workspace domain-wide delegation is a tenant-wide grant.** It cannot be scoped to one
mailbox: a service account granted `gmail.readonly` by domain-wide delegation can impersonate **any**
user in the domain and read **their** mail, and nothing in the grant limits it to the mailbox this
connector was authorised for. The scope limits the *operation* to reading; it does not limit the
*subject*.

That is a real and unavoidable property of the mechanism, not a configuration mistake, and it is why
this page is addressed to the mailbox owner. Two things reduce it, and neither eliminates it:

- **prefer IMAP with a per-mailbox credential** where the mailbox is a real Workspace mailbox, since
  the credential then reaches exactly one mailbox;
- where delegation is used, the authorising administrator is recorded in the campaign record
  (FR-140), so the grant has a named owner rather than being an artefact nobody remembers approving.

Who authorised the read, and against which mailbox, is recorded **per campaign**, in that campaign's
own `campaign.yaml` — the named person, the date, the mailbox, and the access path from the table in
§3. It is per campaign rather than once for the connector because the authorisation is a fact about
one reading of one mailbox: a person who authorised September's campaign has not thereby authorised
every future one.

No campaign has been recorded yet. The first one is Phase 9 of feature 003, which needs a credential,
mailbox access, and a **named human signatory** on the sanitisation manifest — the signature is the
point of the record, and it is not something an automated run can supply for itself.
