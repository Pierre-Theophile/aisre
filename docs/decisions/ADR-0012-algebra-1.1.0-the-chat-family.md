# ADR-0012: Algebra 1.1.0 — the chat family, offered only where it can be answered

- Status: **Accepted** (2026-10-13)
- Deciders: project owner ("Bump, gated", 2026-10-13), Claude (design, documented here)
- Source: `specs/006-slack-connector/spec.md` FR-030–FR-036, `tasks.md` T026–T031.

## Context

The 006 chat reader asks two questions no published term can express: how often a class of things
was said in the incident's own channel, and how busy the channel was over time. Adding a term is a
MINOR bump of the query algebra ([`docs/schema/algebra.md`](../schema/algebra.md) §Versioning).

A bump done naively moves far more than the two new terms. The algebra version is carried on every
term and is part of the canonical encoding, so `term_key = sha256(canonical(term))` would move for
every existing term, and every recorded world would stop answering. The investigator's prompt
prints the version and lists the families, and its tool list is part of the request, so every
recorded trajectory, which replays by comparing request digests, would stop replaying too. The
owner's condition for the bump was that none of that happens.

## Decision

### 1. A term carries the version that introduced it

`pkg/backend` publishes `AlgebraVersion = "1.1.0"` (the newest) and `BaseAlgebraVersion = "1.0.0"`,
and `TermVersion(name)` is the version a term was introduced in: `1.1.0` for `chat_mentions` and
`chat_timeline`, `1.0.0` for every other term. `Normalise` accepts any supported version not older
than the term's own and canonicalises it to `TermVersion`, so a `compare` term is keyed exactly as
it was under 1.0.0 whatever version a caller stamped on it.

Worlds are therefore **not migrated**. A world index still declares `1.0.0`; the loader accepts any
supported version; every existing key, and every answer behind it, is unchanged. A world can hold
chat answers next to telemetry ones, each keyed under its own version.

### 2. The chat family is offered only where it can be answered

The engine offers the chat tools, and prints the chat family in the prompt, only when both hold:

- the investigation was declared in a Slack channel (its symptom's origin is `slack:<team>` and its
  origin reference is a channel of that workspace), and
- the deployment registers a worker for the chat family (`serve --chat-reader`).

Everywhere else the tool list and the prompt are the 1.0.0 ones byte for byte, and the investigation
records `algebra_version: 1.0.0`. That is what keeps every recorded trajectory replaying unchanged.
Where the family is offered, the prompt prints `1.1.0` and the investigation records it.

### 3. The model never names a channel

Neither chat tool takes a channel. The engine fills the investigation's own declared channel, and a
chat call from an investigation with no channel is an unknown tool. The worker refuses a channel
outside the workspace it is bound to (`outside_origin`) without reading it.

### 4. The two terms

| term | arguments | answers | cost class |
|---|---|---|---|
| `chat_mentions` | channel (engine-filled), term class, window | messages matching one class of `slack/terms@1`, first and last instants, distinct participants, up to 10 permalinks, and how many messages in the window read as instructions | `standard` |
| `chat_timeline` | channel (engine-filled), window, bucket ≥ 60 s | messages and distinct participants per bucket, every bucket present, at most 288 | `standard` |

No digest carries a message's text, an author's id or a name. Participants are counted in memory
and the count is all that leaves.

### 5. A term no worker answers is a refusal, not an error

Found while doing this: a published term no registered worker declares — `knowledge_search` on a
deployment without the knowledge worker — was returned to the model as a tool error and recorded
nowhere. It is now a typed refusal: the model is told the question is not covered here, which is not
a negative answer, and the hypothesis it served is recorded untested with the query attached.

## Consequences

- Existing worlds, trajectories, prompts and keys are unchanged; the full suite replays as before.
- A recorded world built before 1.1.0 answers `NOT_RECORDED` for a chat term, as the versioning rule
  says. The two chat fixtures (`fixtures/slack-chat-01`, `slack-chat-injection-01`) are worlds of
  chat answers only.
- A future term follows the same rule: it carries its introducing version, and a family is offered
  only where a worker can answer it.
- `MAJOR` bumps are unaffected: changing an existing term's meaning still moves every key.
