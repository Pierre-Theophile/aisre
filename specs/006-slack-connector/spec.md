# Feature Specification: Slack Connector

**Feature Branch**: `006-slack-connector`

**Created**: 2026-10-13

**Status**: Approved by the owner 2026-10-13 (all three defaults); in implementation

**Input**: User description: "The next feature after 005. ADR-0004 D4 says the trigger that would
have fired on every incident in the corpus is a person creating a dated severity channel in Slack,
and ADR-0007 says the report goes back where the incident is being handled. Feature 002 built both
ends — the declaration front door and the report-delivery sink interface — and left the transport
to a connector. Build that connector: incident-channel intake, a real Slack report sink, and a
read-only chat reader for the incident channel. Read-only except for the one confined write the
constitution permits."

## Why this feature exists

Features 001 to 005 are built. The graph knows the cloud platform, the deploys, the vendor notices
and the one Datadog estate; the engine can investigate an incident and render a report. **Nothing
starts an investigation on its own, and nothing delivers the report to anyone.** Today a person has
to run `investigate declare` by hand and then `investigate report --deliver`, whose only sink,
`LogSink`, records what it *would* have posted and posts nothing (ADR-0007).

The coverage audit (`docs/evaluation/coverage-audit-2026-09.md`) is unambiguous about where the
real trigger lives:

- The organisation's incident system **is** a Slack channel naming convention: a person creates a
  dated severity channel, and that is the declaration. The incident-management product is unused.
- Most incidents were **noticed by people**. Monitor transitions — the only automatic trigger the
  engine has today, through feature 005 — caught few of them.
- So, with features 001–005 alone, the engine would have opened an investigation automatically on
  **almost none** of the thirteen audited incidents, even though it could have found the cause of
  about 62 % of them.

This feature closes that gap. It is the smallest thing that turns the project from a tool someone
runs after the fact into an investigator that is already working when the on-call opens the
channel, and whose answer appears in that channel. It is one feature and not three because the
intake, the reader and the sink share one workspace, one rate-limit budget and one vocabulary of
stable channel identifiers — but they run as **two Slack apps with two separate credentials**,
because one of them writes and Slack cannot narrow a token below its app's scopes (constitution VII,
ADR-0007 confinement 3).

What it adds, in priority order:

1. **Incident-channel intake.** A channel whose name matches a published, versioned naming rule
   becomes a declaration (`alert.transition` into `declared`, keyed on the channel's stable id),
   with severity and date parsed by the rule, never guessed (002 FR-001a, FR-002a, FR-002b,
   FR-004a, FR-008b).
2. **The report in the incident channel.** A Slack sink behind the existing `render.Sink` interface
   posts the engine's report once and edits that same message in place on every later delivery,
   on a credential that can do nothing else (ADR-0007).
3. **A chat reader.** The worker contract already admits "chat readers ... by later features"
   (002 contracts/worker-sdk.md). This is the first: a read-only worker over the incident channel's
   own history, returning digests — who-is-doing-what is out; *what people said changed, and when*
   is in — so the investigator can test a hypothesis against "we rolled back at 14:40".

## Clarifications

### Session 2026-10-13 — defaults confirmed by the owner

These forks change scope or security posture. Each default was written into the requirements
below, and the owner approved all three as written on 2026-10-13.

- Q1: Intake transport — poll the channel list, or subscribe to Slack's event stream? → **Default:
  polling is the truth, every 30 seconds, and Socket Mode events are an optional doorbell** that
  only triggers an early poll, exactly the 005 Q3 posture (`pkg/feeder/doorbell`). Socket Mode needs
  no public ingress. A webhook (Events API over HTTP) is not offered in v1. (FR-010–FR-014)
- Q2: Which person is the declaring identity (002 FR-002a requires an authenticated individual)?
  → **Default: the Slack user who created the channel, as Slack authenticated them, recorded as a
  workspace-scoped pseudonymous identity** (`slack:<team>/<HMAC(user id)>`). An operator may map
  a Slack user to the deployment's OIDC subject; the mapping is configuration, never inferred from
  an email address. (FR-006, FR-052)
- Q3: Is the chat reader in v1? → **Default: yes, at P2, scoped to the incident channel only** and
  off unless enabled. Reading any other channel (team channels, `#deploys`) is out of scope.
  (US3, FR-030–FR-039)

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A severity channel opens an investigation (Priority: P1)

Morgan, on call, creates `#sev2-2026-10-14-checkout-latency` because customers report slow
checkouts. Within a minute, without anyone running a command, an investigation is open, pinned to
the instant the channel was created, with severity `sev2`, the title from the channel name and
topic, and — where the rule can parse one — `checkout` as a target service.

**Why this priority**: it is the trigger that would have fired on every incident in the audited
corpus (ADR-0004 D4). Everything else in this feature is worth less without it.

**Independent Test**: replay a recorded workspace in which three channels are created — one that
matches the rule, one that matches but is a test channel, one that does not match — and assert
exactly one `alert.transition` into `declared`, keyed on the matching channel's stable id with an
empty group, pinned to its creation instant, and one investigation opened.

**Acceptance Scenarios**:

1. **Given** the naming rule `sev-channel@1`, **When** a channel named `sev2-2026-10-14-checkout-latency`
   is created at 09:12:03Z, **Then** one declaration is emitted with severity `sev2`, declared
   instant 09:12:03Z, origin `slack:<channel id>`, and a target `checkout` with provenance
   `parsed_from_declaration` and rule `sev-channel@1`.
2. **Given** the channel above has already been declared, **When** it is renamed, re-polled, or
   delivered twice, **Then** no second investigation opens: the key is the channel id, never the
   name (002 FR-008b), and the rename is recorded as a later, separate fact.
3. **Given** a matching channel whose slug names nothing the graph resolves, **When** it is
   declared, **Then** the investigation opens anyway with no target and reaches `unknown` with
   "name the affected service(s)" as the resolving action (002 FR-002b).
4. **Given** a matching channel created eight days ago and discovered only now (first start,
   outage), **When** it is polled, **Then** it is declared with its true creation instant and
   **review mode**, never with the discovery instant (002 FR-004a), and only if it is inside the
   configured backfill horizon.
5. **Given** a channel whose name matches but whose date part is not the creation date (a channel
   created on the 15th for an incident on the 14th), **Then** the pin is the creation instant and
   the parsed date is recorded as a claim beside it, not substituted for it.

---

### User Story 2 - The report appears in the incident channel, once, and is kept current (Priority: P1)

When the investigation from Story 1 concludes, its report appears as one message in
`#sev2-2026-10-14-checkout-latency`. If the investigation is reopened with new evidence and
concludes again, the same message is edited; the channel never receives a second copy.

**Why this priority**: an investigation nobody reads did not happen (ADR-0007). The sink interface,
the delivery record and the constitution amendment exist; only the transport is missing.

**Independent Test**: run the Slack sink against a recorded Slack API double; deliver, re-deliver
with a changed body, re-deliver with an identical body; assert one `chat.postMessage`, one
`chat.update`, and no call for the identical body, and the `report_deliveries` row shows
`update_count = 1`.

**Acceptance Scenarios**:

1. **Given** a concluded investigation whose intake origin is a Slack channel, **When** delivery
   runs with sink `slack`, **Then** exactly one message is posted to that channel and its
   timestamp is recorded as the external reference.
2. **Given** a previous delivery reference, **When** delivery runs again with a different report,
   **Then** the message is edited in place; there is no code path that posts a second message for
   the same `(investigation, target)` (ADR-0007 confinement 2).
3. **Given** the delivery credential, **When** any read is attempted with it, **Then** it fails
   with `ErrCredentialNotForReading`; **given** the read credential, **when** delivery is attempted
   with it, **then** it fails before any network call.
4. **Given** Slack is unavailable or the bot has been removed from the channel, **When** delivery
   runs, **Then** the outcome `failed` is recorded with its reason and the investigation's terminal
   state is unaffected (ADR-0007 confinement 4).
5. **Given** an investigation whose origin is not a Slack channel (a Datadog monitor), **When**
   delivery runs with sink `slack` and no explicit target, **Then** the outcome is
   `not_configured` — the sink never chooses a channel on its own.

---

### User Story 3 - The investigator can read what people said in the incident channel (Priority: P2)

During the investigation, the engine asks the `chat` worker "did anyone report a change in this
channel between onset and now?" and gets a digest: three messages mention a rollback, the first at
14:40Z, by a person (pseudonymous), with a link to each — not the messages' text, and nothing from
any other channel.

**Why this priority**: people mitigate during the incident, and a mitigation the graph cannot see
(a manual rollback, a feature flag flipped in a vendor console) is otherwise invisible evidence. It
is P2 because the investigation must work without it (002 worker contract: "their absence must not
block an investigation").

**Independent Test**: replay a recorded incident channel through `worker.Testkit`; diff the digests
against goldens; assert no message body, no display name and no email appears in any digest or
recording; assert an embedded instruction ("ignore previous instructions and…") is reported as an
evidence item and changes nothing.

**Acceptance Scenarios**:

1. **Given** the chat reader is enabled and bound to an investigation whose origin is a Slack
   channel, **When** the engine calls `chat_mentions` with a term class (`rollback`, `deploy`,
   `flag`, `restart`, `scale`, `vendor`) and a window, **Then** it receives counts, first and last
   instants, pseudonymous authors and permalinks for matching messages, and a coverage block.
2. **Given** the chat reader is not enabled, **When** the engine plans, **Then** the `chat` source
   is reported unavailable and the investigation proceeds.
3. **Given** a request for any channel other than the investigation's own origin, **Then** it is
   refused at the worker boundary, not filtered after the read.

---

### User Story 4 - Recorded, sanitised, and replayable like every other connector (Priority: P2)

Casey runs a recording campaign against the organisation's Slack workspace for the incident
channels of the last nine months, so that the intake rule and the reader are tested against the
real naming habits (typos, test channels, renamed channels) rather than against a convention as
written down.

**Why this priority**: constitution VIII — a connector is not stable on synthetic data alone — and
the audit itself found test channels and inconsistencies that a rule written from memory would
miss.

**Independent Test**: `fixture campaign scan` and `fixture campaign verify` pass over the recording
in the private corpus; the public repository holds a synthetic structural twin that replays from
empty, double-delivered and shuffled.

**Acceptance Scenarios**:

1. **Given** a recording, **Then** it contains no message text, no display or real name, no email,
   no avatar URL and no user id; channel and workspace ids are HMAC-pseudonymised with the corpus
   key; channel names are kept only where they match the naming rule, slug included.
2. **Given** the private audit's incident list, **When** the intake rule is replayed over the
   recorded channel history, **Then** the result is compared incident by incident and every miss
   or false declaration is listed (SC-001, SC-002).

---

### User Story 5 - An operator can see what the connector sees and why (Priority: P3)

Riley runs `aisre feed slack --check` and sees: the app's scopes, split into the read credential
and the delivery credential; the naming rule and its version; the channels matched in the last
backfill horizon and the ones skipped with the reason (`test channel`, `no rule match`,
`outside horizon`, `archived before declaration`).

**Why this priority**: it is how a skeptical operator trusts an automatic trigger, but nothing in
Stories 1–4 depends on it.

**Acceptance Scenarios**:

1. **Given** a read token that carries any write scope, **Then** the feeder refuses to start
   (`WriteCapableError`), naming the scope.
2. **Given** a channel that was skipped, **Then** `--check` names the rule clause that excluded it.

---

### Edge Cases

- **Channel renamed after declaration**: the key does not move and nothing is re-declared (002
  FR-007). A channel renamed *into* a matching name is declared at its creation instant if it was
  created inside the horizon, and not at all otherwise: Slack's channel list carries no rename
  instant to declare at. (Amended 2026-10-13.)
- **Channel archived and unarchived**: archive is evidence of resolution, not a retraction; it
  never closes or deletes the investigation. Unarchive does not re-declare.
- **Test and drill channels**: names matching the rule but carrying a configured exclusion marker
  (`test`, `drill`, `gameday` by default) are skipped and reported; game-day channels may be opted
  in as labelled fixtures instead.
- **Two channels for one incident** (one per team): two declarations, grouped or not by 002's
  published association rule (FR-008c) — this connector never merges them.
- **Private incident channels**: not visible to a bot that is not a member; reported as a coverage
  gap ("private channels are not observed") rather than silently missed.
- **Severity outside the rule's vocabulary** (`sev0`, `p1`): the severity is recorded verbatim; the
  rule version declares which spellings it maps.
- **The bot is removed from a channel mid-investigation**: reads return a typed coverage gap;
  delivery records `failed`; neither blocks.
- **Rate limiting during an incident**: Slack tier limits are shared with the humans' own apps; the
  connector spends from a declared budget, backs off on `Retry-After`, and intake outranks reading.
- **Slack Connect / shared channels**: an external organisation's members are never named, not
  even pseudonymously, and the channel is read only if the operator opted it in.
- **Workspace migration or enterprise grid**: the team id is part of every stable identifier, so two
  workspaces never collide.
- **Prompt injection in a channel**: content is data (002 FR-017); attempts are evidence items.

## Requirements *(mandatory)*

### A. Scope and posture

- **FR-001**: The connector MUST hold **two credentials that come from two separate Slack app
  installations**, because a Slack app's bot token carries every scope the app was granted and
  cannot be narrowed per call: a **reader app** (channel metadata and, when the reader is enabled,
  history of channels it is a member of) and a **reporter app** whose only scopes post and edit its
  own messages. In code they MUST be distinct types, as ADR-0007 confinement 3 requires.
- **FR-002**: The read credential MUST be verified read-only at start (`pkg/feeder.CheckReadOnly`);
  a read token carrying any write scope MUST be refused, naming the scope.
- **FR-003**: The connector MUST NOT create, rename, archive, join, invite to, pin in, react in or
  otherwise modify any channel, and MUST NOT post anything other than the rendered report.
- **FR-004**: Slack is a discussion surface, not a production system (constitution VII v1.1.0); this
  feature MUST NOT add a second write path of any kind.

### B. Incident-channel intake

- **FR-005**: A channel MUST become a declaration only when its name matches a **published,
  versioned naming rule** owned by this connector. The rule id and version MUST be recorded on every
  parsed field (002 FR-002b, `intake.RuleRef`).
- **FR-006**: The declaration MUST carry: source `slack:<team id>`, place = channel id, declared
  instant = channel creation instant (or rename instant, Edge Cases), severity and title as parsed,
  the declaring identity of FR-052, and zero or more targets with provenance `parsed_from_declaration`.
- **FR-007**: The rule MUST NOT guess a target. A slug token becomes a target only if the operator's
  service map names it (`--service <token>=<ns>=<value>`); otherwise no target is emitted and the
  token is kept in the title. (Amended 2026-10-13, ADR-0011 item 4: a feeder does not read the graph,
  so the "published resolution query at the declared instant" became operator configuration.)
- **FR-008**: The default rule (`sev-channel@1`) MUST match `sev<N>-<YYYY>-<MM>-<DD>-<slug>` and be
  replaceable by configuration. Exclusion markers MUST be configurable.
- **FR-009**: The idempotency key MUST be `intake.DeclarationKey(source, channel id, declared
  instant)`; any number of polls, doorbell rings or restarts MUST open at most one investigation per
  channel.

### C. Transport

- **FR-010**: Polling of the channel list MUST be the truth, at a default interval of 30 s.
- **FR-011**: Socket Mode events MAY be enabled as a **doorbell** that only triggers an early poll,
  through `pkg/feeder/doorbell`; no event body may create, alter or retract anything.
- **FR-012**: On start, the connector MUST backfill channels created within a configured horizon
  (default 24 h) and declare them in review mode with their true instants.
- **FR-013**: Ordering MUST be declared per source; a channel observed out of order is placed by its
  creation instant, never by delivery order.
- **FR-014**: The connector MUST spend Slack API calls from a declared budget per method tier, honour
  `Retry-After`, and prioritise intake over reading over delivery retries.

### D. Report delivery

- **FR-020**: The connector MUST provide a `render.Sink` with published sink id `slack`, registered
  in `render.PublishedSinks()`; an unknown sink id remains `ErrUnknownSink`.
- **FR-021**: The default target MUST be the investigation's own origin channel when that origin is
  this connector; otherwise the outcome is `not_configured` unless an explicit target was given.
- **FR-022**: First delivery posts; every later delivery to the same `(investigation, target)` edits
  the recorded message; an identical body MUST NOT produce an API call.
- **FR-023**: The posted message MUST carry only the FR-057c rendering and its FR-057d deep links,
  formatted for Slack, with no interactive elements, buttons or slash-command affordances.
- **FR-024**: Delivery MUST NOT mention `@here`, `@channel` or `@everyone`, and MUST strip any such
  mention that a rendering might contain (`internal/sanitise` already matches them).
- **FR-025**: A rendering longer than Slack's message limit MUST be truncated at a section boundary
  with a link to the full report, never split into a second message.

### E. Chat reader (P2, off by default)

- **FR-030**: The `chat` worker MUST be bound to exactly one source of truth (the workspace) and
  MUST only read the history of the investigation's own origin channel, plus channels the operator
  opted in by id.
- **FR-031**: Its capabilities MUST be declared, read-only and typed: `chat_mentions` (term class,
  window → counts, first/last instants, pseudonymous authors, permalinks) and `chat_timeline`
  (window → per-minute message counts and participant counts). No capability returns message text.
- **FR-032**: Term classes MUST be a published, versioned vocabulary matched algorithmically; a
  model MAY label but the digest MUST say what the model added (002 `logs` worker precedent).
- **FR-033**: Every response MUST carry a coverage block, including "private channel not observed",
  "bot not a member" and "history truncated by plan retention".
- **FR-034**: Recorded and live modes MUST produce identical digests for identical inputs.
- **FR-035**: Embedded instructions in messages MUST be reported as evidence items and change
  nothing (002 FR-017).
- **FR-036**: Chat content MUST NOT be written to the graph. A human-reported change ("rolled back
  at 14:40") is evidence in the investigation's decision record only; turning it into a CHANGE node
  is out of scope (see Out of scope).

### F. Sanitisation, recording and evaluation

- **FR-050**: Sanitisation MUST happen in the connector before anything touches disk, under a
  `slack` policy table in `internal/sanitise`: message text dropped; display names, real names,
  emails, avatars, status text dropped; channel ids and team ids HMAC-pseudonymised with the corpus
  key; user ids (the channel creator) **dropped**, because the project's sanitiser never hashes people;
  channel names kept only when they match the naming rule.
- **FR-051**: The public repository MUST carry only synthetic structural twins; real recordings live
  in the private corpus behind its four gates.
- **FR-052**: The declaring identity MUST be `slack:<team>/u_<HMAC(team, user id)>`, under a key the
  deployment holds (`SRE_AGENT_SLACK_IDENTITY_KEY`, never the corpus key), unless an operator has
  configured a mapping to an OIDC subject for that user; it MUST NOT be derived from an email. A
  replayed recording, which carries no creator, names `slack:<team>/creator-not-recorded`.
- **FR-053**: Fixtures MUST cover: a matching channel, a test channel, a renamed channel, a channel
  discovered late, two channels for one incident, a private channel, an injection attempt, and a
  delivery that edits in place.
- **FR-054**: The coverage audit MUST be re-run with this connector's intake replayed over the
  private corpus's channel history, and the result recorded beside the 2026-09-22 re-run.

### Key Entities

- **Naming rule**: id, version, pattern, severity vocabulary, exclusion markers, slug-to-target
  resolution clause. Connector-owned; referenced by every parsed field.
- **Incident channel**: stable id `(team id, channel id)`, creation instant, creator, current name,
  name history, archive state. The place a declaration lives; not a graph node in v1.
- **Declaration**: the existing 002 `intake.Declaration`, filled by this connector.
- **Report message**: `(investigation, channel)` → message timestamp, recorded in the existing
  `investigation.report_deliveries` row.
- **Chat digest**: counts, instants, pseudonymous authors and permalinks; never text.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Replayed over the private corpus's channel history, the intake opens an investigation
  for **all 13** audited incidents (the audit found every one of them through its severity channel),
  and any miss is reported with the rule clause that caused it and fails the gate.
- **SC-002**: Over the same replay, **zero** investigations are opened for the excluded test
  channels or for non-incident channels.
- **SC-003**: Live, an investigation is open within **60 seconds** of the channel's creation at the
  default interval, and within **10 seconds** with the doorbell enabled, at p95.
- **SC-004**: Across any sequence of restarts, double deliveries and renames in the conformance
  suite, each incident channel produces **exactly one** investigation and **at most one** report
  message.
- **SC-005**: The report is visible in the incident channel within **30 seconds** of the
  investigation concluding, at p95, when Slack is reachable.
- **SC-006**: **No** recording, digest, log line or report contains message text, a person's name or
  email, or a clear user id — asserted by the independent scan and by canary tokens.
- **SC-007**: With the chat reader disabled, investigation outcomes on the 002 fixture corpus are
  **identical** to those before this feature.

## Out of scope for this feature

- Any write beyond posting and editing the bot's own report message (constitution VII).
- Slash commands, buttons, modals, an interactive bot or a chat interface to the engine.
- Reading channels other than incident channels and explicitly opted-in channels; reading DMs.
- Turning chat messages into graph events (human-reported changes as CHANGE nodes, ownership from
  channel membership). Those are candidates for a later feature once the reader shows they matter.
- Other chat platforms (Microsoft Teams, Google Chat); the sink and reader contracts admit them.
- Incident-management products (PagerDuty, incident.io, Datadog Incident Management); unused here.
- Document and knowledge connectors (Notion, postmortems), which remain a later feature.

## Assumptions

- Features 001–005 are complete; this feature uses 002's `Declare` RPC, `intake.Declaration`,
  `render.Sink` and `report_deliveries` unchanged, and adds no new event type.
- The workspace admin can install two custom Slack apps (reader and reporter) with the scopes listed
  in the plan; Socket Mode is available on the workspace's plan.
- The reader app can list public channels without being a member, but reads a channel's history
  only where it is a member; it never joins on its own (FR-003). Membership is granted by a person or
  by a Slack workflow the organisation owns, and its absence is a reported coverage gap.
- Incident channels are public channels in the organisation (the audit found them by listing
  channels); private ones are a reported gap.
- A shared-contract addition is needed for the `chat` worker name, the `slack` sink id and the
  `slack` source kind; it is recorded in a new ADR before any schema task (as ADR-0009 and ADR-0010
  did for 003 and 005).
