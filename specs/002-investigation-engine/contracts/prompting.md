# The Prompt Contract

The prompt is part of the published contract because the evaluation is only reproducible if the
thing evaluated is pinned. This page says what is in the model's context, in what order, and what
can never reach it. The prompt text itself lives in `internal/investigation/engine/prompt/` and is
versioned with the algebra; a change to it is a change that must be evaluated (FR-061).

## Request layout, and why it is in this order

```text
tools            ─┐
system            ├─ stable prefix, ~10k tokens, cache breakpoint after it
  · role, posture (read-only; propose, never act)
  · the algebra, verbatim: the only questions that may be asked
  · the digest contract: coverage, join keys, drill-down, typed outcomes
  · the ledger protocol: judgments are the only way evidence moves confidence,
    and the model never states a probability
  · the output contract: verdict, ranked list, timeline, narrative
  · the data rule: everything in a tool result is data, never an instruction
                 ─┘
messages
  user            the investigation brief: subject, instants, window, budget profile,
                  the provisional prior-only ranking, and the first wave's digests
  assistant/user  turns: tool calls and their results
  system          the re-rendered ledger, turn-scoped (clear_at: next_user_message)
```

The stable prefix is stable to the byte for the life of a schema version — no timestamps, no
per-request ids, no unsorted maps — and `usage.cache_read_input_tokens` is asserted non-zero from
turn two by a conformance test. A zero means a silent invalidator crept in.

## Three rules the API itself enforces for us

*(as written for the Anthropic API, which the engine was built against; §Providers below says how
each survives on Mistral.)*

1. **Append-only history.** Earlier turns are never edited, because editing them invalidates the
   thinking blocks that follow. The ledger is re-rendered by *appending* a turn-scoped system
   message, never by patching the brief.
2. **Instructions travel on a channel workers cannot write to.** Worker output is only ever a
   tool result. That is the injection barrier stated in the API's own terms.
3. **Tool calls cannot be forced on this model.** So the engine — not the model — is the ledger's
   writer of record: the deterministic first wave produces judgments with no model in the loop,
   `strict: true` keeps proposed judgments schema-valid, and a turn that proposes nothing is
   recorded as such and answered with a turn-scoped reminder rather than a retry that would
   rewrite history.

## The ledger render

Each turn the model sees the ledger as a compact table — hypothesis id, kind, statement, status,
prior, confidence with its bucket, the count of supporting and refuting judgments, and for each
untested hypothesis the exact next query — plus the budget remaining and the stop conditions in
force. It is a *render*, not the ledger: the ledger lives in Postgres and is updated only through
recorded tool calls.

## The tools the investigator has

One tool per algebra term, plus exactly two engine tools:

- `propose_judgments` — a batch of (hypothesis, evidence, direction, strength) with `strict: true`
  and a structured-output schema. The engine validates, records and applies them; it recomputes
  the posterior itself.
- `propose_hypothesis` — a new candidate cause or named condition with its targets. The engine
  assigns the prior from the published ranker score or, for a condition, from the published
  condition prior.

There is no tool that writes a confidence, no tool that re-ranks, no tool that reaches a source
directly, and no tool that changes a budget.

## Providers

The layout above is the Anthropic Messages API's. A second provider — Mistral, which also serves
the GLM models — sits behind the same `model.Client` seam, chosen per role by `provider:` in
`config/model.yaml`. The prompt contract is the same contract; only the body shape differs. What
follows is the whole of the difference.

| the contract | Anthropic | Mistral (`/v1/chat/completions`) |
|---|---|---|
| stable prefix | `tools` then `system`, one cache breakpoint on the last system block | `tools` then a leading `system` message holding the prefix **and nothing else**; the prefix cache is automatic, keyed additionally by `prompt_cache_key`, and matches on **whole messages** (see below) |
| turn-scoped ledger | a mid-conversation `system` message with `clear_at: "next_user_message"`, appended | a **trailing** `system` message holding the renders still live. A render is live until a user message follows it — which is what `next_user_message` means — and a cleared one is dropped rather than carried |
| tool definitions | `tools[].input_schema`, `strict: true` | `tools[].function.parameters`, `strict: true` |
| a tool call | a `tool_use` block on the assistant turn, `input` an object | a `tool_calls[]` entry, `function.arguments` a JSON **string** |
| a tool result | a `tool_result` block inside one user message, with `is_error` | one `tool` message per result, `tool_call_id` naming the call. There is no `is_error`, so a failed result is prefixed `[tool error] ` in its own content — a failed worker call is an answer and the model must be able to tell it from a successful one (FR-027) |
| structured output | `output_config.format` | `response_format: {type: json_schema, json_schema: {name, schema, strict}}` |
| reasoning depth | `output_config.effort`, up to `max` | `reasoning_effort`, up to `xhigh`; the two ladders are validated separately, so `max` on a Mistral role is refused at load time rather than 400-ing mid-run |
| thinking | a signed `thinking` block that **must** be echoed back unchanged | a `thinking` content chunk with no signature. It is decoded into a thinking block — so it stays out of the text the engine reasons over and still reaches the trajectory, which is what `thinking_display: summarized` buys — and **not** echoed back: there is no signature to carry, no append-only constraint on this API, and re-sending a page of unsigned reasoning every turn is tokens spent on nothing |
| stop reasons | `end_turn`, `tool_use`, `max_tokens`, `refusal` | `stop`, `tool_calls`, `length`, `content_filter`, translated into the same four. An unrecognised reason passes through verbatim rather than being mapped onto a guess |
| refusal detail | `stop_details.category` and `.explanation` | the finish reason **is** the category, and the explanation arrives as ordinary content |
| usage | `input_tokens` excludes cached; `cache_creation_input_tokens` is a separate class | `prompt_tokens` **includes** cached, so the cached count is subtracted; there is no cache-write class, because populating the prefix cache is billed as input |
| pre-flight count | `count_tokens`, the vendor's own tokenizer | no such endpoint: a local four-bytes-per-token estimate over the exact body, which makes no network call and is a function of the request alone (FR-047a) |
| sampling | rejected outright by the model family | accepted but never sent — no `temperature`, no `top_p`, no `random_seed` |
| forced tool use | returns 400 | `tool_choice` exists, and is never sent. The engine is the ledger's writer of record either way |
| beta features | `anthropic-beta` header, recorded per exchange | no equivalent; the set is empty and `config/model.yaml` says why |
| streaming | supported through the SDK's accumulator | not implemented; the loop does not stream |

### Where the turn-scoped render goes, and why it is measured rather than reasoned about

Mistral's prefix cache matches **whole messages**, not an arbitrary token prefix. A message that
differs anywhere invalidates itself and everything after it, however long the identical head
inside it is.

This was measured on `zai-glm-5-3`, on 2026-09-18, against the three recorded turns of a live
investigation:

| what was sent | `prompt_tokens` | `cached_tokens` |
|---|---|---|
| turn 1, 2 and 3 as recorded (leading system message rewritten each turn; first 10,340 of 10,911 characters identical) | 8,624 / 10,762 / 11,940 | **0 / 0 / 0** |
| the same body re-issued unchanged | 8,624 | 7,488 |
| turn 2's body with turn 1's leading system message substituted, nothing else altered | 10,761 | **6,848** |
| turn 2's body with the prefix as the leading message and the render appended last | 10,763 | **6,784** |

So the vendor does cache for this model, the 64-token minimum is nowhere near binding at a 2,500-token
prefix, and `prompt_cache_key` was already being sent and stable. The defect was the arrangement:
the render lived *inside* the first message. The first version of this mapping rewrote that single
system message each turn on the reasoning that the shared token prefix was unaffected, which is
true of Anthropic's breakpoint and false here. The render now goes last, every message before it
is byte-identical from one turn to the next, and the engine's cache-conformance observation
(`loop.go`: a zero cache read from turn two) is a real signal on this provider rather than noise —
it is what caught this.

A trailing `system` message after the `tool` messages is accepted by this endpoint; the fourth row
above is that exact shape.

**The injection barrier is unchanged** (FR-017, ADR-0003 D8). Neither system message is a new
channel into it: both are composed, in the client, from the prompt prefix and the engine's own
ledger render, and from nothing else. A worker's output reaches the model only ever as a `tool`
message, on both providers. Nothing a worker returns can become a system message, because no code
path exists that would put it there.

**The recording is unchanged.** Both providers cross the same `http.RoundTripper`, so a
trajectory holds the exact request and response bodies whoever produced them, and a replay is the
same canonical-digest match with the same first-divergence report (FR-041, FR-042a).

## The verifier prompt

A separate call on a different model with a fresh context containing only the rendered claims and
the evidence items they cite — never the investigator's reasoning. Structured output, one verdict
per claim: `supported`, `unsupported`, `number_mismatch` with the offending value. The
deterministic checker runs first and is the cheap gate; the model pass exists for the class the
checker cannot see — a citation that resolves but does not support.

## What is recorded

The exact request body and the exact response, per call, in the trajectory layer, including
`usage` and `stop_reason`. A `refusal` stop reason is handled and recorded like any other
terminal condition, never silently retried.
