# ADR-0007: Report delivery — the project's first and only write path

- Status: **Accepted** (2026-09-17; realised 2026-09-18)
- Deciders: project owner (constitution amendment), Claude (design, documented here)
- Authority: `.specify/memory/constitution.md` **v1.1.0, Principle VII**, confined exception
  "report delivery", amended 2026-09-17 after the analyze pass's finding C1.
  [ADR-0005](./ADR-0005-shared-contracts-for-002-to-005.md) D8 recorded the confinement;
  `specs/002-investigation-engine/plan.md` F6 required this ADR **before** the implementation
  merged. Requirement: 002 FR-057f.

## Context

Principle VII is absolute: *no component MUST write to any production system.* It is the reason
the project can be run against a live estate at all — a read-only tool cannot cause the incident
it is investigating.

And yet an investigation that nobody reads is an investigation that did not happen. The on-call
engineer is in a Slack thread or an incident ticket, not in a terminal, and asking them to leave
the place they are working in order to fetch the answer is how a tool gets turned off. FR-057f
therefore asks for one thing: **post the engine's own report where the incident is already being
handled.**

That is a write. It is the first in the project, and it needed a decision at the level of the
principle rather than a workaround at the level of the code. The analyze pass (finding C1) was
explicit that an ADR-level exception was not enough: the constitution itself had to be amended, or
the write was a violation. It was amended, to v1.1.0, and this record is the design the amendment
permits.

## Decision

**One report, one place, one message edited in place, on a credential that can do nothing else,
never blocking, never a production system.** Six confinements, each of which is a property of the
code rather than a deployment convention:

1. **Report-only.** What travels is a rendering of a concluded investigation — the FR-057c order
   with the FR-057d deep links — and nothing else. There is no code path carrying a command, a
   parameter, or anything a receiving system could act on.
2. **Edited in place, never re-posted.** The first delivery mints an external reference; every
   later one edits that same message. The `Sink` interface has **no "post again" method**:
   `Deliver` carries the previous reference and the driver is obliged to update it. A stream of
   re-posts is explicitly not what FR-057f asks for, and the record keeps one row per
   `(investigation, target)` so a second delivery is an `UPDATE` with `update_count + 1`.
3. **A credential scoped to `report:write` and separate from every read credential.** It is a
   distinct Go *type* from every read credential in the codebase, it carries an explicit scope,
   and it is checked against both the scope and the target it was minted for. A delivery
   credential asked to read fails with `ErrCredentialNotForReading`; a read path cannot be handed
   one by accident, because the types do not permit it. The secret is never rendered: `String`
   redacts it, so a credential in a log line or an error is a description rather than a key.
4. **Non-blocking, and never a precondition of a terminal state.** `Deliver` is called *after* an
   investigation concludes, never inside the conclusion. A failure is recorded as a result with
   outcome `failed`, not raised as an error that could propagate into the lifecycle. The type
   signature is the guarantee: delivery cannot fail a conclusion, because the conclusion does not
   call it.
5. **Recorded.** Every attempt is written to `investigation.report_deliveries`: where it went, the
   digest of what was delivered, the external reference to edit next time, first and last
   delivery, the update count and the outcome. Not the body — that is a rendering of an immutable
   investigation and is re-derivable — and not the credential.
6. **Never a production system.** The target must be a discussion surface where the incident is
   being handled. A system that serves production traffic or holds production configuration is out
   of scope, and widening this in any direction requires a further constitution amendment, not an
   ADR.

### What is deliberately absent

There is **no retry loop, no queue and no background worker**. A delivery that fails is recorded,
and the next `investigate report --deliver` tries again. Building durability here would give
delivery a state of its own, and the one thing FR-057f is insistent about is that delivery has no
state the investigation waits on.

**Transport is a connector concern.** The v1 sink is `LogSink`: it records what it *would* have
posted and posts nothing. Shipping a real transport means writing a `Sink` in a connector; the
render package neither imports nor knows about one. An unrecognised sink id is an error
(`ErrUnknownSink`), not a fallback — defaulting an unknown transport to whichever one happens to
be compiled in is exactly how a write path ends up somewhere nobody chose.

## Alternatives rejected

- **No write path at all; the operator fetches the report.** Honest, and it is what the project
  did until now. Rejected because the report's value is time-sensitive and its reader is
  elsewhere; a correct answer nobody sees during the incident is worth very little.
- **A general "actions" capability, with report delivery as its first action.** Rejected as the
  thing the principle exists to prevent. The exception is confined precisely so that the next
  write has to argue for itself from scratch rather than inherit an existing permission.
- **Re-posting rather than editing.** Simpler to implement and impossible to bound: an
  investigation that reopens would fill the thread. Editing in place is also the only shape under
  which "one message" is a checkable claim.
- **A shared credential with read scope, narrowed by policy.** Rejected: a policy is a promise, a
  separate credential type is a property. The constitution asks for separate; separate is what the
  code has.

## Consequences

- The project has exactly one write path, and it is small enough to read in one sitting.
- Any wider write — acknowledging an alert, updating a ticket field, silencing a monitor — is a
  new constitution amendment and a new ADR. This record is not a precedent for them.
- Operators configure delivery through `$SRE_AGENT_REPORT_TOKEN` and an explicit sink id; with
  neither, the outcome is `not_configured`, which is a stated result rather than a silent no-op.
- `docs/security.md` and the connector checklist both continue to say the graph-side credentials
  are read-only. They are: the delivery credential is not a graph credential and cannot read.

## Realised by (2026-09-18)

`specs/002-investigation-engine/tasks.md` T087 and T116, in feature 002 phase 7 (`f53ec15` and
earlier).

- `internal/investigation/render/delivery.go` — the `Sink` interface, `Credential` with
  `DeliveryScope = "report:write"`, the scope and target checks, the outcomes
  (`delivered` / `failed` / `not_configured`), and `LogSink`, the v1 recorded no-op transport.
- `internal/investigation/store/delivery.go` and the `investigation.report_deliveries` table in
  `internal/store/postgres/migrations/0006_investigation.sql` — one row per
  `(investigation, target)`, edited in place.
- `internal/cli/investigate_human.go` — `investigate report --deliver` and
  `SRE_AGENT_REPORT_TOKEN`.
- The conclusion path (`ConcludeInTx`, `internal/investigation/store/lifecycle.go`) mentions no
  delivery, which is confinement 4 asserted by construction.
