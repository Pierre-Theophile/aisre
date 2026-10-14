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

## 1. Two apps, three credentials

Slack issues one bot token per app, carrying every scope the app has. A reader and a writer in one
app would put a write scope on the read path, so there are two apps.

| credential | app | scopes | variable | may |
|---|---|---|---|---|
| reader bot token (`xoxb-`) | **aisre reader** | `channels:read`; `channels:history` only with the chat reader | `SRE_AGENT_SLACK_READER_TOKEN` | list public channels and read one channel's details |
| reporter bot token (`xoxb-`) | **aisre reporter** | `chat:write`, `chat:write.public` | `SRE_AGENT_REPORT_TOKEN` | post one message per investigation and edit it |
| graph token | — | `feeder` and `investigator` roles, `source_id: slack:<team id>` | `SRE_AGENT_TOKEN` | ingest the declarations and open the investigations |

Both Slack tokens are checked against what Slack itself reports (`X-OAuth-Scopes`):

- the reader must hold `:read` and `:history` scopes only, or `feed slack` stops before reading a
  channel;
- the reporter must hold `chat:write` and `chat:write.public` and nothing else, and must belong to the
  workspace the incident was declared in, or the sink posts nothing and records a failed delivery.

User tokens (`xoxp-`) and app-level tokens (`xapp-`) are refused for both.

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

If the message was deleted in Slack, the next delivery posts it again and the ledger carries the new
reference. Mentions in the report (`@here`, `@channel`, `@everyone`, user groups, users) are
neutralised, links are not unfurled, and a report longer than a Slack message is cut with a line
saying so.

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
| targets | slug tokens the operator mapped (`--service checkout=service.name=checkout`); an unmapped token stays in the title |

Not declared, each with its reason in the per-poll report: a name the rule does not match; an
exclusion marker (`test`, `drill`, `gameday`); an impossible date; a channel older than `--horizon`
(24 h) when first seen; a channel shared with another organisation unless `--include-shared`. Private
channels never appear in the public list, so they are never seen.

A channel first seen more than `--late-after` (5 min) after its creation is still declared at its
creation instant, and its investigation runs in **review mode**.

## 4. Running it

```sh
export SRE_AGENT_SLACK_READER_TOKEN=xoxb-…      # reader app
export SRE_AGENT_REPORT_TOKEN=xoxb-…            # reporter app
export SRE_AGENT_SLACK_IDENTITY_KEY=$(openssl rand -hex 32)   # store it; pseudonyms change with it
aisre feed slack --check                        # verifies the reader token, reads no channel
aisre feed slack --deliver --service checkout=service.name=checkout
```

Polling every 30 s is the truth. Socket Mode, when it is added, will only cut that wait short.

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
