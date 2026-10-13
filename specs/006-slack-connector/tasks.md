---

description: "Task list for feature 006 — the Slack connector"
---

# Tasks: Slack Connector

**Input**: Design documents from `/specs/006-slack-connector/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md),
[contracts/slack-connector.md](./contracts/slack-connector.md)

**Tests**: **Required, not optional.** Constitution VIII (no fixtures, no merge) and the
zero-tolerance criteria SC-004 and SC-006. Fixture tasks are interleaved with implementation.

**Organization**: one foundational phase for the four gaps of [plan.md](./plan.md#phase-0--gaps-found-against-the-code),
then the stories in priority order: US1 and US2 (P1, the MVP), US3 and US4 (P2), US5 (P3).

**Status**: draft, not started. Nothing below is to be built until the owner approves the scope and
the three defaults in the spec's clarification section.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: `[US1]`…`[US5]`. Setup, Foundational and Polish tasks carry no story label.
- Every task names the fixture it validates against; `unit` means a Go test in the package named.

## Path conventions

Per [plan.md §Project Structure](./plan.md#project-structure): client in `internal/slackx/`, feeder
in `internal/feeders/slack/`, sink in `internal/investigation/render/slacksink/`, worker in
`internal/workers/chat/`, CLI in `internal/cli/feed_slack.go`, fixtures in `fixtures/slack-*/`.

---

## Phase 1: Setup

**Purpose**: the decision record and the operation list, so nothing can issue an unpublished call.

- [ ] T001 [P] Write `docs/decisions/ADR-0011-shared-contract-additions-for-006.md`: source kind `slack`, sink id `slack`, worker name `chat`, rule namespace `slack/`, the declaring-identity form, the doorbell-only credential kind (G1, G2), with the alternatives rejected in research R2 and R4; fixture: unit
- [ ] T002 [P] Create `internal/slackx/doc.go` and `internal/feeders/slack/doc.go` stating scope and read-only posture (FR-001–FR-004); fixture: unit
- [ ] T003 Write `internal/slackx/operations.go`: the six-row operation list of contracts §2 and a test that the client exposes no other method (FR-002, FR-003); fixture: unit
- [ ] T004 [P] Write `docs/connectors/slack.md`: the two apps, their manifests and scopes (research R2), the optional invite workflow (R3), marked **not yet enforced** until T010; fixture: unit

**Checkpoint**: ADR-0011 exists before any shared name is used in code.

---

## Phase 2: Foundational

**Purpose**: the four gaps, the client and the sanitiser. Blocks every story.

- [ ] T005 [P] Add the doorbell-only credential kind to `pkg/feeder/credential.go`: opens a socket, refused by every read and write path, `String` redacted (G2); fixture: unit
- [ ] T006 [P] Add `feeder:declare` to the token scopes and restrict naming a `declaring_identity` other than the caller's to it, in `internal/server/investigation.go`; test that requester and declarer are both recorded (G3, FR-052); fixture: unit
- [ ] T007 [P] Write `internal/sanitise/policy_slack.go`: drop text, names, emails, avatars, status; HMAC user, channel and team ids; keep channel names only on rule match (G4, FR-050); fixture: unit with canaries
- [ ] T008 Implement `internal/slackx/client.go`: the five Web API methods, cursor pagination, per-method tier budgets through `pkg/feeder/quota`, `Retry-After`, priority intake > reading > delivery (FR-014, research R7); fixture: unit against a recorded double
- [ ] T009 Implement `internal/slackx/record.go`: record and replay of sanitised responses through `pkg/feeder/record`, sanitising before write (FR-050); fixture: unit
- [ ] T010 Run `CheckReadOnly` on the reader bot token at start, refusing any write scope by name, and mark `docs/connectors/slack.md` enforced (FR-002, US5 scenario 1); fixture: unit

**Checkpoint**: a read-only, rate-limited, recordable Slack client exists, and no story has started.

---

## Phase 3: User Story 1 — A severity channel opens an investigation (P1) 🎯 MVP

**Goal**: channel creation → one declaration → one investigation, without a command.

**Independent Test**: `fixtures/slack-intake-01` — three channels created (match, test, no match);
exactly one declaration, keyed on the channel id, pinned to creation.

- [ ] T011 [P] [US1] Implement `internal/feeders/slack/rule.go`: rule struct, `slack/sev-channel@1` per research R5, severity map, exclusion markers, `RuleRef` on every parsed field (FR-005, FR-008); fixture: unit table of names incl. typos and tests
- [ ] T012 [P] [US1] Implement `internal/feeders/slack/resolve.go`: slug token → target only on a unique resolution at the declared instant, else no target (FR-007); fixture: unit over `fixtures/baseline-topology-01`
- [ ] T013 [US1] Implement `internal/feeders/slack/feeder.go`: poll, match, build `DeclareRequest` exactly as `investigate declare` does, `DeclarationKey(slack:<team>, channel id, created)` (FR-006, FR-009, FR-010); fixture: `slack-intake-01`
- [ ] T014 [US1] Checkpoint and backfill: horizon (default 24 h), review mode for late discovery, true creation instant (FR-012, US1 scenarios 4–5); fixture: `slack-intake-late-01`
- [ ] T015 [US1] Renames and archives: a rename is a later fact, a rename into a match declares at the rename instant, archive is evidence and never retracts (Edge Cases, US1 scenario 2); fixture: `slack-intake-rename-01`
- [ ] T016 [P] [US1] Socket Mode doorbell in `internal/slackx/socket.go` through `pkg/feeder/doorbell`: events ring an early poll and carry nothing (FR-011); fixture: `slack-doorbell-forged-01`
- [ ] T017 [US1] `aisre feed slack` in `internal/cli/feed_slack.go`: live, `--record`, `--replay`, `--interval`, `--rule`, `--horizon`, `--doorbell`; fixture: unit
- [ ] T018 [US1] Fixtures `fixtures/slack-intake-01`, `-late-01`, `-rename-01`, `-two-channels-01`, `-private-01` as synthetic twins, each passing `aisre fixture verify` (replay, double, shuffle) (FR-053, SC-004); fixture: themselves

**Checkpoint**: the MVP trigger works on recordings; delivery still goes to `LogSink`.

---

## Phase 4: User Story 2 — The report in the incident channel (P1)

**Goal**: one message per investigation and channel, edited in place.

**Independent Test**: the sink against a recorded Slack double — post, update, no-op on identical
body; `report_deliveries.update_count = 1`.

- [ ] T019 [P] [US2] Slack renderer for the FR-057c sections: `header`, `section`, `context` blocks only, deep links as mrkdwn, truncation at a section boundary with a link (FR-023, FR-025, research R6); fixture: unit goldens
- [ ] T020 [US2] Implement `internal/investigation/render/slacksink/sink.go`: scope and target checks, post / update / no-op by digest, outcome `failed` with Slack's error code, never an error into the lifecycle (FR-020–FR-022, ADR-0007 confinements 1–5); fixture: unit against a recorded double
- [ ] T021 [US2] Default target is the investigation's own `slack:` origin, otherwise `not_configured` (FR-021, US2 scenario 5); fixture: unit
- [ ] T022 [US2] Strip `@here`, `@channel`, `@everyone` using `internal/sanitise` group-mention matching (FR-024); fixture: unit
- [ ] T023 [US2] Register `slack` in `render.NewSink` / `PublishedSinks` and accept a reporter token on `investigate report --deliver --sink slack` (FR-020); fixture: unit
- [ ] T024 [US2] Cross-credential tests: the reporter credential cannot read, the reader credential cannot deliver, both fail before any network call (US2 scenario 3); fixture: unit
- [ ] T025 [US2] Fixture `fixtures/slack-delivery-edit-01`: conclude, reopen, conclude again; one post, one update (FR-053, SC-004); fixture: itself

**Checkpoint**: US1 + US2 is the MVP — declared in Slack, answered in Slack.

---

## Phase 5: User Story 3 — The chat reader (P2, off by default)

**Goal**: digests of what people said in the incident channel, never the text.

**Independent Test**: `worker.Testkit` over `fixtures/slack-chat-01`; no text, name or email in any
digest; an injection attempt becomes an evidence item.

- [ ] T026 [P] [US3] Published term vocabulary `slack/terms@1` in `internal/workers/chat/terms.go` (FR-032); fixture: unit
- [ ] T027 [US3] Implement `internal/workers/chat/worker.go`: `chat_mentions`, `chat_timeline`, bound to the origin channel and opted-in ids only, refusal at the boundary (FR-030, FR-031, US3 scenario 3); fixture: unit
- [ ] T028 [US3] Coverage block reasons `not_enabled`, `not_member`, `private_channel`, `retention_truncated`, `rate_limited`, `outside_origin` (FR-033); fixture: unit
- [ ] T029 [US3] Injection attempts reported as evidence items and changing nothing (FR-035); fixture: `slack-chat-injection-01`
- [ ] T030 [US3] Register the `chat` worker behind a flag; with it off the engine reports the source unavailable (US3 scenario 2, SC-007); fixture: 002 corpus unchanged
- [ ] T031 [US3] Fixtures `fixtures/slack-chat-01`, `-injection-01` with `worker.Testkit` goldens, live/recorded parity (FR-034); fixture: themselves

---

## Phase 6: User Story 4 — Recorded, sanitised, replayable (P2)

**Needs the organisation**: two Slack apps installed by a workspace admin, and a named signatory.

- [ ] T032 [US4] Campaign definition for the workspace: nine months of channel metadata, the incident channels' history only if the chat reader is approved; recorded through T009 into the private corpus (FR-051); fixture: private
- [ ] T033 [US4] `fixture campaign scan` and `verify` over the recording; canaries absent; signed manifest (SC-006); fixture: private
- [ ] T034 [US4] Replay the intake over the recorded channel history against the private audit's incident list; record per-incident hits, misses and false declarations; revise the rule to `@2` if needed (FR-054, SC-001, SC-002); fixture: private
- [ ] T035 [US4] Regenerate the public synthetic twins from the recording's structure (FR-051); fixture: `slack-*`

---

## Phase 7: User Story 5 — The operator check (P3)

- [ ] T036 [US5] `aisre feed slack --check`: scopes per credential, rule and version, channels matched and skipped with the excluding clause (US5); fixture: unit over `slack-intake-01`

---

## Phase 8: Polish and verification

- [ ] T037 [P] `docs/connectors/slack.md` final: setup, both app manifests, the invite workflow, failure modes; README status row for 006
- [ ] T038 Live quickstart run against the organisation's workspace, recorded as `quickstart-run-<date>.md`: SC-003 and SC-005 measured at p95
- [ ] T039 Re-run the coverage audit with Slack intake, beside the 2026-09-22 re-run in the private corpus (FR-054)
- [ ] T040 `/speckit-analyze` over spec, plan and tasks; close findings

---

## Dependencies & execution order

- **Phase 1** first; T001 (ADR-0011) precedes any use of a shared name.
- **Phase 2** blocks every story. T005–T007 in parallel; T008 before T009 and T010.
- **Phases 3 and 4** depend only on Phase 2 and can run in parallel.
- **Phase 5** depends on Phase 2 and on T013 (the origin channel on the investigation).
- **Phase 6** needs the organisation and Phases 3–5 built; T034 decides SC-001.
- **Phases 7–8** last.

## Implementation strategy

**MVP is Phases 1–4**: a severity channel opens an investigation and the answer appears in that
channel, edited in place. That is the whole of ADR-0004 D4 and ADR-0007 in production. The chat
reader (Phase 5) is the first increment after it, and can be dropped from the feature without
touching the MVP if the owner declines Q3.
