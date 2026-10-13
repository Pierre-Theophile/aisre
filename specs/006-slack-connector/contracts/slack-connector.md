# Contract: Slack Connector

The published surface of feature 006. ADR-0011 adds the shared names; this document is their
detail.

## 1. Published names (ADR-0011)

| name | value |
|---|---|
| source kind | `slack` |
| source id | `slack:<team id>` |
| place id (declaration key, element 2) | `<channel id>` |
| origin ref (human-facing) | `slack:<team id>/<channel id>`, plus the permalink |
| sink id | `slack` |
| worker name | `chat` |
| rule id namespace | `slack/` (e.g. `slack/sev-channel@1`) |
| declaring identity | `slack:<team id>/<HMAC(user id)>`, or the operator-mapped OIDC subject |

## 2. Operation list (read-only proof)

| method | app | used by | condition |
|---|---|---|---|
| `conversations.list` (public, non-archived and archived) | reader | intake | always |
| `conversations.info` | reader | intake, chat | always |
| `conversations.history` | reader | chat | chat worker enabled, member |
| `apps.connections.open` + WebSocket | reader (app token) | doorbell | doorbell enabled |
| `chat.postMessage` | reporter | sink | first delivery |
| `chat.update` | reporter | sink | later deliveries |

No other method is compiled into `internal/slackx`. A test asserts the list.

## 3. Intake

- Poll every `--interval` (default 30 s); the doorbell may ring an early poll at most once per
  interval.
- For each channel created (or renamed into a match) inside the backfill horizon and not yet
  declared: match the configured rule; skip with a reason on exclusion; otherwise send
  `DeclareRequest` with `origin = DECLARED`, `transport = human_declared`, `fired_at = creation
  instant`, `idempotency_key = DeclarationKey("slack:<team>", channel id, fired_at)`, and targets
  with provenance `PARSED_FROM_DECLARATION` and `rule_id = slack/<rule>@<version>`.
- Channels first seen after `fired_at + 5 min` are declared in review mode.

## 4. Sink

`Deliver(ctx, cred, req)`:

1. `cred` must be scope `report:write` and minted for `req.TargetSystem = slack`.
2. Target: `req.TargetRef` if set, else the investigation's origin channel if it is `slack:`; else
   `not_configured`.
3. No previous reference → `chat.postMessage`; previous reference and changed digest →
   `chat.update`; unchanged digest → no call, outcome `delivered`.
4. Any error → outcome `failed` with Slack's error code; never returned as a Go error into the
   lifecycle.

## 5. Chat worker

| capability | in | out |
|---|---|---|
| `chat_mentions` | term class (published vocabulary), window | count, first and last instant, distinct pseudonymous authors, up to 10 permalinks, coverage |
| `chat_timeline` | window, bucket (≥ 1 min) | per-bucket message and participant counts, coverage |

Term classes (`slack/terms@1`): `rollback`, `deploy`, `flag`, `restart`, `scale`, `vendor`,
`mitigated`. Matching is algorithmic over the message text in memory; the text never leaves the
worker.

Coverage reasons: `not_enabled`, `not_member`, `private_channel`, `retention_truncated`,
`rate_limited`, `outside_origin`.
