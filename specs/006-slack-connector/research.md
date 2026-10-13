# Research: Slack Connector

Decisions taken while planning feature 006. Each is reversible and records what was rejected.

## R1. No Slack client library

**Decision**: hand-write the five Web API methods and the Socket Mode handshake in
`internal/slackx`.

**Rationale**: the read-only claim (FR-002, constitution VII) is proved over a published operation
list, as `internal/datadogx` and `internal/gcpx` do. A full client exposes hundreds of methods,
including every write; proving that none is reachable is harder than not compiling them.

**Rejected**: `slack-go/slack` — mature, but its surface is the whole API and it would be the
largest dependency in the module for five calls.

## R2. Three tokens, three types

**Decision**:

| token | app | scopes | Go type | may |
|---|---|---|---|---|
| reader bot (`xoxb-`) | reader | `channels:read`; `channels:history` only when the chat worker is enabled | feeder read credential, `CheckReadOnly` passes | list channels, read info, read history where a member |
| doorbell app-level (`xapp-`) | reader | `connections:write` | new doorbell-only kind | open a Socket Mode connection, nothing else |
| reporter bot (`xoxb-`) | reporter | `chat:write`, `chat:write.public` | `render.Credential`, scope `report:write` | post and edit its own message |

**Rationale**: Slack issues one bot token per app with all of that app's scopes, so two credentials
of different power need two apps (spec FR-001). `connections:write` is named like a write but only
opens the app's own socket; giving it a type of its own keeps `CheckReadOnly` strict for everything
else instead of teaching it an exception. `chat:write.public` lets the reporter post in a public
channel it has not joined, so nothing ever joins a channel.

**Rejected**: one app with both scopes (the read path would hold a write scope); a user token
(acts as a person, broader than any bot); `channels:join` on the reader (a write to membership).

## R3. Reading needs membership; the connector never joins

**Decision**: the chat worker reads only channels the reader app is a member of, and answers a typed
coverage gap ("bot not a member") otherwise. `docs/connectors/slack.md` shows how an organisation
can add the app to every new severity channel with its own Slack workflow.

**Rationale**: `conversations.history` requires membership for a bot; joining is a write to the
workspace. Intake is unaffected: `conversations.list` and `conversations.info` work on every public
channel without membership.

## R4. Declaring on behalf of a person

**Decision**: the feeder authenticates as itself, names the channel creator in
`Symptom.declaring_identity` as `slack:<team>/<HMAC(user id)>` (or the operator-mapped OIDC subject),
and the server records the feeder as requester. Only a principal with `feeder:declare` may name an
identity other than its own.

**Rationale**: the handler already accepts a caller-named identity (`internal/server/investigation.go`);
what is missing is the permission that stops any caller from naming anyone, and a test.

**Rejected**: resolving the Slack user to an email and matching it to OIDC (people data, and an
inference rather than a fact).

## R5. The naming rule is versioned data, not code paths

**Decision**: a rule is `{id, version, pattern, severity map, exclusion markers, slug clause}`,
compiled in, selectable by configuration. Every parsed field records `id@version`
(`intake.RuleRef`). Replay uses the version on the event.

**Default `sev-channel@1`**: `^sev(?P<sev>[0-9])-(?P<y>\d{4})-(?P<m>\d{2})-(?P<d>\d{2})-(?P<slug>[a-z0-9-]+)$`,
exclusions `test`, `drill`, `gameday` as whole slug tokens.

**Open for the recording campaign**: the real channel history will show variants (`sev-2`, `inc-`,
typos). Each variant either becomes a clause of `@2` or a documented miss; SC-001 decides.

## R6. Slack rendering

**Decision**: a Slack renderer over the existing FR-057c report sections using Block Kit `header`,
`section` and `context` blocks only; FR-057d deep links as mrkdwn links; no `actions`, `input` or
interactive blocks. Over Slack's limit (50 blocks, 3 000 characters per section), truncate at a
section boundary and end with a link to the full report.

**Rejected**: attachments (legacy); splitting into several messages (breaks "one message, edited in
place", ADR-0007 confinement 2).

## R7. Rate limits

**Decision**: per-method budgets from Slack's published tiers (`conversations.list` tier 2,
`conversations.history` tier 3, `chat.postMessage` special tier ~1/s per channel), shared through
`pkg/feeder/quota`, with priority intake > reading > delivery retry, and `Retry-After` honoured.

**Rationale**: limits are per app per workspace; the two-app split also means delivery never
competes with intake for the same budget.
