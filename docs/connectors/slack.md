<!-- SPDX-License-Identifier: Apache-2.0 -->

# The Slack connector

Written for the **workspace administrator** who installs the two Slack apps, and the operator who
runs `aisre feed slack`.

When someone opens a severity channel — `#sev2-2026-10-14-checkout-latency` — the connector declares
an incident at the instant Slack says the channel was created, opens an investigation, and posts the
report into that channel, editing the same message as the investigation progresses. It never joins a
channel, never reads a message unless the chat reader is turned on for that channel, and never posts
anything but that one report.

Specification: [`specs/006-slack-connector/`](../../specs/006-slack-connector/spec.md).
Contract: [`contracts/slack-connector.md`](../../specs/006-slack-connector/contracts/slack-connector.md).
Shared names and decisions: [ADR-0011](../decisions/ADR-0011-shared-contract-additions-for-006.md).

## 1. Two apps, four credentials

Slack issues one bot token per app, carrying every scope the app has. A reader and a writer in one
app would put a write scope on the read path, so there are two apps.

| credential | app | scopes | variable | may |
|---|---|---|---|---|
| reader bot token (`xoxb-`) | **aisre reader** | `channels:read`; `channels:history` only with the chat reader | `SRE_AGENT_SLACK_READER_TOKEN` | list public channels and read one channel's details |
| doorbell app-level token (`xapp-`), optional | **aisre reader** | `connections:write` | `SRE_AGENT_SLACK_APP_TOKEN` | open the reader app's Socket Mode connection, and nothing else |
| reporter bot token (`xoxb-`) | **aisre reporter** | `chat:write`, `chat:write.public` | `SRE_AGENT_REPORT_TOKEN` | post one message per investigation and edit it |
| graph token | — | `feeder` and `investigator` roles, `source_id: slack:<team id>` | `SRE_AGENT_TOKEN` | ingest the declarations and open the investigations |

Both Slack tokens are checked against what Slack itself reports (`X-OAuth-Scopes`):

- the reader must hold `:read` and `:history` scopes only, or `feed slack` stops before reading a
  channel;
- the reporter must hold `chat:write` and `chat:write.public` and nothing else, and must belong to the
  workspace the incident was declared in, or the sink posts nothing and records a failed delivery.

User tokens (`xoxp-`) and app-level tokens (`xapp-`) are refused for both. The app-level token is
the doorbell's alone: `connections:write` is named like a write but writes nothing to the workspace,
so rather than teach the read-only gate an exception, the token is a type of its own
(`feeder.DoorbellCredential`) that renders as redacted and can only be handed to the call that opens
the socket. If Slack reports it holds anything beyond `connections:write`, the doorbell stays off.

The graph token's `source_id` must be `slack:<team id>`: only a feeder scoped to the declaration's own
source may name someone else as the declarer (ADR-0011 item 2).

## 2. The published read-only operation surface

Every request the reader app can make. An operation absent from this list is refused by
`Surface.Issuable` before it leaves the process. Each is a `GET`; there is no join, post, reaction,
pin, rename or archive anywhere below.

| area | operation | why |
|---|---|---|
| auth | `GET /api/auth.test` | which workspace the token belongs to, and the scopes Slack reports it holds, for the read-only gate (FR-002) |
| intake | `GET /api/conversations.list` | the public channel list, which is where a severity channel is created (FR-005, FR-010) |
| intake | `GET /api/conversations.info` | one channel's creation instant, creator and archive state, for a channel the doorbell or a reader names (FR-006) |
| chat | `GET /api/conversations.history` | the incident channel's own messages, read in memory for the chat worker's digests; only when it is enabled (FR-030) |

**The table above and the code are one list.** It lives in
[`internal/slackx/client.go`](../../internal/slackx/client.go), and a test parses this page and
compares it with the code in both directions, area and reason included.
`conversations.history` is refused in code unless the chat reader is enabled.

### The report sink's operations

The reporter app is a separate type with its own list, in
[`internal/investigation/render/slacksink`](../../internal/investigation/render/slacksink/sink.go):

| operation | when |
|---|---|
| `GET /api/auth.test` | once per process, before the first write: the workspace and the scopes |
| `POST /api/chat.postMessage` | the first delivery of an investigation's report |
| `POST /api/chat.update` | every later delivery: the same message, edited |

A reopened investigation is a new record linked to its parent, and its report edits the parent's
message: the channel asked one question and gets one answer, kept current
([`fixtures/slack-delivery-edit-01`](../../fixtures/slack-delivery-edit-01/delivery.json)). If the
message was deleted in Slack, the next delivery posts it again and the ledger carries the new
reference. Mentions in the report (`@here`, `@channel`, `@everyone`, user groups, users) are
neutralised, links are not unfurled, and a report longer than a Slack message is cut with a line
saying so.

### The doorbell's operation

With `--doorbell`, one more call, on the app-level token and outside the read surface above:
`POST /api/apps.connections.open`, which returns the URL of a WebSocket the connector then holds open.
Over it the connector sends only pongs and envelope acknowledgements (`{"envelope_id": …}`). It reads
each message's type and envelope id and nothing else: an event is counted, never decoded, and the
count can only bring the next poll of the channel list sooner — at most one early poll per 10 s,
however many events arrive ([`internal/slackx/socket.go`](../../internal/slackx/socket.go)).

### The budget

Slack limits each method per app and workspace and reports neither the limit nor what is left, so
the reader draws from a static per-minute allowance at each method's published tier — 20 a minute for
`conversations.list`, 50 for `conversations.info` and `conversations.history`, 100 for `auth.test` —
and the run's usage report says `static_fallback` rather than present it as a measurement. Intake
comes first: the chat reader leaves a quarter of each minute for it. A `429` closes the method until
Slack's `Retry-After` has passed; the next poll then marks a gap. The reporter is a separate app,
which Slack meters separately, so delivery never competes with intake.

## 3. What is declared

The default rule is `slack/sev-channel@1`:

```
sev<N>-<YYYY>-<MM>-<DD>[-<slug>]      e.g. sev2-2026-10-14-checkout-latency
```

| field | from |
|---|---|
| place | the channel id; a rename never re-declares |
| declared instant | the channel's creation instant, as Slack states it |
| severity | `sev<N>` |
| title | the channel name at first sight |
| declarer | the operator's OIDC subject for the creator (`--person U0123=<subject>`), else `slack:<team>/u_<HMAC(team, user)>` under `SRE_AGENT_SLACK_IDENTITY_KEY` |
| targets | none: the [service catalog](../../specs/009-service-catalog/spec.md) attributes the incident from its slug tokens (`slack.slug=<token>`); with the deprecated `--service` flags, the tokens they map |

Not declared, each with its reason in the per-poll report: a name the rule does not match; an
exclusion marker (`test`, `drill`, `gameday`); an impossible date; a channel older than `--horizon`
(24 h) when first seen; a channel shared with another organisation unless `--include-shared`; a
private channel. The public list does not include private channels; one that appears anyway — made
private after it was created — is skipped before its name is read, and a channel made private after
its declaration keeps it.

A channel first seen more than `--late-after` (5 min) after its creation is still declared at its
creation instant, and its investigation runs in **review mode**.

## 4. Running it

```sh
export SRE_AGENT_SLACK_READER_TOKEN=xoxb-…      # reader app
export SRE_AGENT_REPORT_TOKEN=xoxb-…            # reporter app
export SRE_AGENT_SLACK_IDENTITY_KEY=$(openssl rand -hex 32)   # store it; pseudonyms change with it
export SRE_AGENT_SLACK_APP_TOKEN=xapp-…         # optional: the reader app's app-level token
aisre feed slack --check                        # every credential, and what the rule matches
aisre feed slack --deliver --doorbell
```

**Deprecated: `--service <token>=<ns>=<value>`.** Before the catalog, the operator mapped slug
tokens to graph entities on the command line. The flags keep working as they did in 006, and the
declaration targets the entities they map. Move them into the catalog once, then drop them:

```sh
aisre catalog import --from-slack-flags checkout=service.name=checkout --dry-run
aisre catalog import --from-slack-flags checkout=service.name=checkout
```

Each flag becomes an entry named after the token, with the aliases `slack.slug=<token>` and the
entity it named; a declared incident whose channel carries the token is then attributed to it. The
flags will be removed in a later version.

`--check` verifies each credential it is given, prints the rule and its version, reads the channel
list once, and lists the channels created inside the horizon that the rule matched and those it
skipped, each with the clause that excluded it. Channels the rule does not match at all, and private
ones, are counted and never named. It reads no message and emits nothing; with `--replay <dir>` it
does the same over a recording.

Polling every 30 s is the truth. `--doorbell` adds the Socket Mode connection, which only cuts that
wait short; when it cannot connect the run carries on polling and its report says the doorbell was
unreachable. `--rule` names the naming rule by its published reference (`slack/sev-channel@1` is the
only one).

## 5. The chat reader (off by default)

`aisre serve --enable-investigation --chat-reader` registers the **chat worker**. It reads the
incident channel's history through the reader app, which then needs `channels:history` and must be
**invited to the channel by a person**: it never joins one, and a channel it is not in is answered
`not_member`. The worker counts and never quotes:

| term | answers |
|---|---|
| `chat_mentions` | how many messages matched one class of `slack/terms@1` (`deploy`, `flag`, `mitigated`, `restart`, `rollback`, `scale`, `vendor`), when the first and last were, how many people wrote them, links to up to 10 of them, and how many messages in the window read as instructions |
| `chat_timeline` | messages and people per bucket (one minute or wider) over a window |

No message text, author id or name leaves the call that read it. Bot messages, the report this
deployment posts, join notices and thread replies are not counted. The model never names a channel:
the engine passes the investigation's own, and the chat tools are offered only to an investigation
declared in this workspace ([ADR-0012](../decisions/ADR-0012-algebra-1.1.0-the-chat-family.md)).
What people said is a reason to look, never a finding; a message that reads as an instruction is
counted and recorded as an attempt, and changes nothing.

An answer that could not read says why: `not_enabled`, `not_member`, `private_channel`,
`retention_truncated` (set `--chat-retention` to the plan's history limit), `rate_limited` or
`outside_origin`.

## 6. Recording

`--record <dir>` writes a fixture sanitised before anything touches disk, under the corpus key in
`SRE_AGENT_CORPUS_KEY`: the workspace and channel ids become pseudonyms, channel names the rule's
pattern does not match are blanked, and creators are dropped (people are never hashed into a
recording). A replayed declaration names `slack:<team>/creator-not-recorded`.

## 7. Installing the two apps

Create each app from a manifest (api.slack.com → *Create New App* → *From an app manifest*), install
it to the workspace, and copy its bot token.

The reader. Drop `event_subscriptions` and set `socket_mode_enabled: false` if you will not use the
doorbell; add `channels:history` only if you turn the chat reader on.

```yaml
display_information:
  name: aisre reader
features:
  bot_user:
    display_name: aisre reader
oauth_config:
  scopes:
    bot:
      - channels:read
      # - channels:history    # only with `serve --chat-reader`
settings:
  event_subscriptions:
    bot_events:
      - channel_created
      - channel_rename
      - channel_archive
  socket_mode_enabled: true
```

For the doorbell, create an app-level token under *Basic Information → App-Level Tokens* with the
single scope `connections:write`; that is `SRE_AGENT_SLACK_APP_TOKEN`.

The reporter:

```yaml
display_information:
  name: aisre reporter
features:
  bot_user:
    display_name: aisre
oauth_config:
  scopes:
    bot:
      - chat:write
      - chat:write.public
settings:
  socket_mode_enabled: false
```

`chat:write.public` lets the reporter post in a public channel it has not joined, so neither app
ever joins a channel.

### Letting the chat reader into a channel

Only for the chat reader, which needs membership to read history. The connector never joins or
invites; a person, or the organisation's own tooling, does. Either someone in the incident channel
runs `/invite @aisre reader`, or whatever creates severity channels in your organisation (an incident
bot, a script) adds the reader app when it creates one, with its own credentials. Until then the
chat worker answers `not_member` and the investigation carries on without it.

## 8. Failure modes

| what happens | what the connector does |
|---|---|
| the reader token holds a write scope | `feed slack` stops before reading a channel, naming the scope |
| the reporter token holds more than `chat:write`, `chat:write.public`, or belongs to another workspace | nothing is posted; the delivery is recorded `failed` and the investigation concludes regardless |
| Slack answers `429` | the method is closed until `Retry-After`; the next poll marks a gap, and the gap's checkpoint says the silence was ignorance |
| the channel list cannot be read | the same: a gap, never an absence |
| the doorbell cannot connect, or its token is refused | polling carries on; the run's doorbell report shows the failure |
| a forged or replayed Socket Mode event | at most one early poll; the event's content reaches nothing ([`fixtures/slack-doorbell-forged-01`](../../fixtures/slack-doorbell-forged-01/manifest.yaml)) |
| the report message is deleted in Slack | the next delivery posts it again |
| the feeder restarts while an investigation is running | the declaration's key makes it the same investigation; when its delivery is in the ledger (`--db`), its report edits the message already in the channel |
| the reader app is not in the incident channel | the chat worker answers `not_member`; intake is unaffected |
