# ADR-0011: The shared contract additions for feature 006

- Status: **Accepted** (2026-10-13)
- Deciders: project owner (approval of the 006 scope and its three defaults, 2026-10-13), Claude
  (design, documented here)
- Source: `specs/006-slack-connector/plan.md` §"Phase 0 — gaps found against the code" (G1–G3),
  `research.md` R2 and R4. Written under `specs/006-slack-connector/tasks.md` T001.

## Context

Feature 006 is the Slack connector: a severity channel opens an investigation, the report goes back
into that channel, and an optional reader turns the channel's chat into digests. Most of what it
needs already exists — `investigate declare`, `intake.DeclarationKey`, `render.Sink`, the delivery
ledger. Six things are not Slack's to decide, because they name something other features and
operators read, or they change who may say what to the server (constitution IX).

## Decision

### Item 1 — published names

| name | value |
|---|---|
| source kind | `slack` |
| source id | `slack:<team id>` — one workspace, one source |
| place (declaration key, element 2) | the channel id |
| origin system / origin ref of a declared symptom | `slack:<team id>` / `slack:<team id>/<channel id>` |
| report sink id | `slack` |
| worker name | `chat` |
| rule id namespace | `slack/` (`slack/sev-channel@1`, `slack/terms@1`) |

The origin system equals the source id on purpose. A report goes back to the declared symptom's
origin (`deliverySplit`), and `render.Deliver` refuses a credential minted for any other target
system, so the reporter credential is minted for `slack:<team id>` and can write to that workspace
only.

### Item 2 — declaring on someone's behalf is a connector's right (G3)

The Declare handler already keeps a caller-named `declaring_identity`. It now accepts one that is
not the caller's own **only** from a principal holding the `feeder` role whose `source_id` equals
the declaration's origin system. An investigator declares as themselves; a feeder for another source
cannot speak for this one. The caller is still recorded as the requester beside the declarer.

No new role and no proto change: the feeder role scoped to one source is exactly "a connector for
this place", and it is what the token issuer already mints.

### Item 3 — the declaring identity

In order: the deployment's OIDC subject when an operator mapped the Slack user to one; otherwise
`slack:<team id>/u_<HMAC(team id "/" user id)>` under a key the deployment holds
(`SRE_AGENT_SLACK_IDENTITY_KEY`), stable for as long as the key is. Never derived from an email.

A **recording** carries no creator at all: the project's sanitiser drops people identifiers rather
than hashing them under the corpus key (`sanitise.ErrPeopleNeverHashed`), and a replayed
declaration names `slack:<team id>/creator-not-recorded`. The deployment key and the corpus key are
different keys for different purposes and are never the same value.

### Item 4 — targets come from an operator service map

A slug token in the channel name becomes a target only when the operator's service map names it
(`--service checkout=<ref>`). The specification said "resolves to exactly one graph entity by the
published resolution query", but a feeder never reads the graph (constitution I: feeders emit, the
engine reads), and a resolution query at intake time would make the declaration depend on graph
state at an instant nobody records. A token that is not in the map stays in the title, as before.

### Item 5 — two Slack apps, and where the sink lives

Slack issues one bot token per app carrying all of that app's scopes, so the read path and the write
path are two apps (research R2). The **reader** holds `channels:read` (and `channels:history` only
when the chat worker is on); `internal/slackx` publishes its four GET operations and refuses user and
app-level tokens. The **reporter** holds `chat:write` and `chat:write.public`, lives in
`internal/investigation/render/slacksink`, and has its own two-operation list (`chat.postMessage`,
`chat.update`).

`render.PublishedSinks` lists `slack`, but `render.NewSink` stays connector-agnostic (ADR-0005 D8):
asked for `slack`, it refuses with a pointer to the connector, and the CLI builds the Slack sink
itself. The render package still imports no transport.

### Item 6 — the doorbell credential (G2)

Socket Mode needs an app-level token (`xapp-`) whose only scope, `connections:write`, opens the
app's own socket. It gets a type of its own that can open that socket and nothing else, rather than
teaching `CheckReadOnly` that one `:write` scope is harmless. The doorbell only shortens the wait
before the next poll; polling every 30 s remains the truth. It is optional and is built after the
MVP.

## Consequences

- No proto change and no event-log migration. `investigation.v1` is unchanged.
- A principal that used to set a foreign `declaring_identity` without the feeder role is now
  refused. Nothing in the repository did.
- The chat worker's digests report counts of distinct participants, not author pseudonyms, for the
  reason of item 3.
- The Slack connector's recordings carry channel names only where the naming rule's pattern matches
  them, workspace and channel ids as pseudonyms, and no people.

## Alternatives considered

- **One Slack app with both scopes.** The read path would hold a write scope.
- **A user token.** It acts as a person and reaches whatever they can.
- **Any investigator may name a declaring identity.** Anyone could open an incident in someone
  else's name.
- **The raw Slack user id as the declaring identity.** It works, and it puts a stable people
  identifier into every investigation for no gain over a keyed pseudonym.
- **Hash the creator into recordings under the corpus key.** The sanitiser's rule is that people are
  dropped, not hashed; one connector is not the place to change it.
- **Resolve slug tokens against the graph at intake.** Feeders do not read the graph.
