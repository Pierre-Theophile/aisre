# Implementation Plan: Slack Connector

**Branch**: `006-slack-connector` | **Date**: 2026-10-13 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/006-slack-connector/spec.md`, with the three defaults
of its 2026-10-13 clarification section applied pending owner confirmation.

## Summary

Three pieces on one workspace, built on contracts feature 002 already ships:

1. **Intake feeder** (`internal/feeders/slack`): polls the public channel list, matches channel names
   against a published, versioned naming rule, and calls the existing `Declare` RPC with an
   `intake.Declaration` keyed on the channel id. Socket Mode is an optional doorbell.
2. **Report sink** (`internal/investigation/render/slacksink` behind `render.Sink`): posts once, edits
   in place, on the reporter app's token, which is a different Go type from every read credential.
3. **Chat worker** (`internal/workers/chat`, off by default): `chat_mentions` and `chat_timeline`
   digests over the investigation's own origin channel, never message text.

**The approach is contract-first, as in 003–005.** A Phase 0 check of the spec against the code
found that almost everything the intake and the sink need exists: `investigate declare` already
builds the exact request the feeder must send (`internal/cli/investigate.go`), `render.NewSink`
already refuses unknown sink ids, and `report_deliveries` already records the message to edit.
What is missing is listed as gaps below; there are four, and only one touches a shared contract.

## Phase 0 — gaps found against the code

| # | gap | where | resolution |
|---|---|---|---|
| G1 | No source kind, sink id or worker name for Slack is published | `docs/schema`, `render.PublishedSinks`, worker registry | **ADR-0011** (shared-contract additions for 006), before any schema task: source kind `slack`, sink id `slack`, worker name `chat`, rule id namespace `slack/` |
| G2 | `pkg/feeder.CheckReadOnly` classifies scopes by name; Slack's Socket Mode app token carries `connections:write`, which is not a write to the workspace | `pkg/feeder/credential.go` | a third, doorbell-only credential type for the app-level token; `CheckReadOnly` still runs on the bot token, and the doorbell token can open a socket and nothing else (research R2) |
| G3 | A feeder declares **on behalf of** a Slack user. The Declare handler already keeps a caller-named `declaring_identity` and only fills the caller's when it is empty (`internal/server/investigation.go`, FR-002a/FR-066), but nothing asserts that the feeder's own principal is recorded as requester beside it, nor restricts which principals may name someone else | `internal/server` Declare handler | a `feeder:declare` permission on the feeder token, and a test that both the requester and the declarer are recorded; no proto change (research R4) |
| G4 | No sanitisation policy rows for Slack payloads | `internal/sanitise` | `policy_slack.go`, mirroring `policy_vendornotice.go` |

## Technical Context

**Language/Version**: Go 1.27 (floor; the repository tracks the latest stable toolchain)

**Primary Dependencies**: `pkg/feeder` (events, claims, `doorbell`, `quota`, `record`, `testkit`,
`CheckReadOnly`); `pkg/worker` and `worker.Testkit`; `internal/investigation/intake`
(`Declaration`, `DeclarationKey`, `RuleRef`); `internal/investigation/render` (`Sink`, `Credential`,
`NewSink`); `internal/sanitise`; `internal/campaign`. The Slack Web API (`conversations.list`,
`conversations.info`, `conversations.history`, `chat.postMessage`, `chat.update`) and Socket Mode
(`apps.connections.open` plus a WebSocket). **No Slack client library**: five methods and a socket,
hand-written, so the read-only operation list is provable (constitution X; research R1). No new
module: the doorbell's WebSocket client is hand-written as well, because the maintained libraries are
ISC- or BSD-2-Clause-licensed, outside the constitution's list (amended 2026-10-14).

**Storage**: PostgreSQL 16, unchanged. No new tables: declarations go through the existing event
log, and report references live in the existing `investigation.report_deliveries`. The naming
rule's state (last seen channel cursor) is a feeder checkpoint like every other feeder's.

**Testing**: `go test ./...`; fixtures under `fixtures/slack-*` verified by `aisre fixture verify`
(replay from empty, double delivery, seeded shuffle); `worker.Testkit` goldens for the chat
worker; a recorded Slack API double for the sink; `scripts/check-report.sh`; `golangci-lint`;
`buf lint` / `buf breaking` if G3 needs a proto field (it should not; see R4).

**Target Platform**: Linux server, the same single binary. `aisre feed slack`, sink id `slack` on
`investigate report --deliver`, and a `chat` worker registration.

**Project Type**: A connector (feeder + sink + worker) inside the existing single-module Go project.

**Performance Goals**: SC-003 — an investigation open within 60 s of channel creation by polling
alone, 10 s with the doorbell, p95. SC-005 — the report visible within 30 s of conclusion, p95.

**Constraints**: One write path only, and it is ADR-0007's (FR-003, FR-004). No message text
anywhere — not in the graph, not in recordings, not in digests (SC-006). Live and recorded digests
identical (FR-034). Tier rate limits are shared with the organisation's other apps (FR-014).

**Scale/Scope**: One workspace per source (enterprise grid: one source per team). The audited
organisation created 17 severity channels in nine months, two of them tests; the design assumes
nothing about that volume beyond the channel list fitting a paginated poll.

## Constitution Check

*GATE: passes before Phase 0; re-checked after design (no change).*

| principle | verdict | how |
|---|---|---|
| I. Graph First | pass | Intake is an event (`alert.transition`); the chat worker is a read at investigation time, like every telemetry worker; nothing reads around the graph to produce a result |
| II. Bitemporal | pass | Declared instant = channel creation (valid); observed = first poll that saw it; late discovery uses review mode and never substitutes the discovery instant |
| III. Event-sourced | pass | Key `DeclarationKey(source, channel id, instant)`; renames are later events; fixtures replay from empty, doubled and shuffled |
| IV. Telemetry stays home | pass | Chat content is never stored; digests are counts, instants, pseudonyms and permalinks |
| V. Evidence-first | pass | Every parsed field names `rule@version`; every digest carries coverage; skipped channels are reported with the excluding clause |
| VI. Resolution auditable | pass | Slug tokens become targets only through the published resolution query at the declared instant, never by fuzzy match |
| VII. Read-only by default | pass, **under the v1.1.0 exception** | The reporter app's only write is ADR-0007's report; the reader app's bot token passes `CheckReadOnly`; the doorbell token is its own type (G2). No join, invite, react or pin |
| VIII. Evaluation | pass | Recorded campaign in the private corpus, synthetic twins in public; SC-001/SC-002 replay the audit's channels |
| IX. Open schema | pass | ADR-0011 publishes the additions before any code depends on them |
| X. Simplicity | pass | No client library; no new tables; no new event type |

## Project Structure

### Documentation (this feature)

```text
specs/006-slack-connector/
├── spec.md
├── plan.md              # this file
├── research.md          # R1–R6
├── contracts/
│   └── slack-connector.md   # naming rule, scopes, chat worker capabilities, sink behaviour
├── checklists/
│   └── requirements.md
├── tasks.md
└── quickstart.md        # written in Phase 9, from the live run
```

No `data-model.md`: the feature adds no table and no event type (Phase 0). The entities in the spec
map onto `intake.Declaration`, `report_deliveries` and a feeder checkpoint.

### Source Code (repository root)

```text
docs/decisions/ADR-0011-shared-contract-additions-for-006.md
docs/connectors/slack.md

pkg/feeder/
└── credential.go                    # doorbell-only credential kind (G2)

internal/
├── slackx/                          # hand-written Slack Web API client: the five methods,
│   ├── client.go                    #   tier budgets, Retry-After, the published operation list
│   ├── operations.go
│   └── socket.go                    # Socket Mode doorbell source
├── feeders/slack/
│   ├── feeder.go                    # poll → match → Declare
│   ├── rule.go                      # naming rule, versioned (sev-channel@1)
│   ├── resolve.go                   # slug token → target via the published resolution query
│   └── checkpoint.go
├── investigation/render/slacksink/
│   └── sink.go                      # render.Sink: post once, chat.update after
├── workers/chat/
│   ├── worker.go                    # chat_mentions, chat_timeline
│   └── terms.go                     # published term-class vocabulary
├── sanitise/policy_slack.go
└── cli/
    ├── feed_slack.go                # aisre feed slack [--check]
    └── feed_slack_test.go

fixtures/
├── slack-intake-twin/               # synthetic structural twin of the recorded workspace
└── slack-chat-twin/
```

**Structure Decision**: the established connector layout (`internal/<vendor>x` client,
`internal/feeders/<vendor>`, a CLI `feed_<vendor>.go`), as 003–005 did. The chat worker gets its own
package because it is the first worker that is neither a graph worker nor a telemetry backend.

## Design decisions (detail in research.md)

- **R1 — No SDK.** The read-only claim is provable over an explicit list of five methods.
- **R2 — Three tokens, three types.** Reader bot token (`channels:read`, plus `channels:history` only
  when the chat worker is on), reporter bot token (`chat:write`, `chat:write.public`), and the
  reader app's app-level token (`connections:write`) as a doorbell-only type.
  `chat:write.public` lets the reporter post into a fresh incident channel without joining it,
  which keeps FR-003 (no join) true.
- **R3 — Membership for reading.** The reader never joins. Where it is not a member, the chat worker
  answers a typed coverage gap. Recommending a Slack workflow that invites it on channel creation is
  documentation, not code.
- **R4 — Declaring on behalf of a person.** The feeder's own principal calls `Declare` and names the
  Slack creator in the existing `Symptom.declaring_identity`; the server records the feeder as
  requester. Only a token carrying `feeder:declare` may name an identity other than its own.
- **R5 — Rule versioning.** A rule change bumps its version; already-declared channels keep the
  version that parsed them; replay uses the version recorded on the event, never the current one.
- **R6 — Slack mrkdwn rendering.** A Slack renderer for the existing FR-057c sections, Block Kit
  `section` and `context` blocks only, no `actions` blocks, truncated at a section boundary.

## Complexity Tracking

No constitution violation to justify. The one item worth naming: a **second Slack app** exists
only to keep the write credential separate (R2). One app with both scopes would be simpler and is
rejected because Slack cannot issue a token narrower than its app, so the read path would hold
`chat:write` — exactly what ADR-0007 confinement 3 forbids.
