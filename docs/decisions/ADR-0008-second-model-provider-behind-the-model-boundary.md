# ADR-0008: A second model provider — Mistral, serving GLM, behind the model boundary

- Status: **Accepted** (2026-09-18)
- Deciders: project owner (the choice of provider and models), Claude (the seam, documented here)
- Source: `specs/002-investigation-engine/contracts/prompting.md` §Providers;
  `config/model.yaml` 1.1.0; commit `b555ceb`.

## Context

Feature 002 was built against one model provider. That is a defensible way to start and an
indefensible way to stay: a reasoning layer with exactly one vendor is a reasoning layer whose
prompt contract, whose recording format and whose cost model have never been tested against the
possibility of a second. The seam either exists or it is a claim.

Two things forced the question at once. The owner wanted the running cost down — the investigator
role is the expensive one, and an eval run over the corpus multiplies it by the corpus size — and
FR-022a requires the **verifier to run on a different model family from the investigator**, which
is a much weaker guarantee when both families come from one vendor.

## Decision

**Add Mistral as a second provider behind the existing `model.Client`, with the investigator role
on GLM (served by Mistral) and the verifier on a Mistral model.**

The seam is an internal provider interface (`internal/investigation/model/provider.go`): it builds
the request body, issues it through the **shared `http.RoundTripper`**, and decodes into the
existing `Response` / `Block` / `Usage` vocabulary. `Client.Complete` dispatches per role and fills
in mode, canonical bodies and digests **identically for both**, so nothing downstream — recording,
replay, budgets, the trajectory — knows which vendor answered. The Anthropic path moved verbatim
into `anthropic.go`; `mistral.go` is plain `net/http` and `encoding/json` against the
chat-completions API, with **no new dependency**, because the recorded request must *be* the
request rather than a rendering of what an SDK chose to send.

Roles are chosen per role in `config/model.yaml` (1.1.0): investigator `zai-glm-5-3`, verifier
`mistral-medium-latest` — a different family, which is FR-022a — and the logs labeller
`ministral-8b-latest`. The Claude roles stay valid and suite-exercised in
`config/model.anthropic.yaml`, so switching back is a flag rather than a migration. Credentials
follow the provider each role names (`MISTRAL_API_KEY` / `ANTHROPIC_API_KEY`), and `serve`,
`eval run` and `fixture record-trajectory` each check every named provider and fall back to
model-free naming the missing variable rather than failing halfway through a run.

The whole of the Anthropic↔Mistral mapping is published in
`specs/002-investigation-engine/contracts/prompting.md` §Providers.

## What it preserves

- **Recording and replay are provider-agnostic.** Both providers cross the same
  `http.RoundTripper`, so a trajectory holds the exact request and response bodies whoever produced
  them, and a replay is the same canonical-digest match with the same first-divergence report
  (FR-041, FR-042a). The checked-in fake-model trajectories still replay byte-identically, 16/16
  over 33 trajectories.
- **The injection barrier.** Rewriting the single system message each turn — which is how the
  turn-scoped ledger render is expressed on an API with no `clear_at` — is not a new channel into
  it. The system message is composed in the client from the prompt prefix and the engine's own
  ledger render, and from nothing else. A worker's output reaches the model only ever as a `tool`
  message, on both providers, and no code path exists that would put it anywhere else.
- **The prefix cache and the stable prefix.** The rewrite puts the stable prefix first and the
  live renders after it, so the cached region still matches.
- **A verifier on a different family.** The point of FR-022a, now with a vendor boundary behind it
  as well as a family one.
- **The ledger as the writer of record.** `tool_choice` exists on this API and is never sent; no
  sampling parameter is sent to either provider.

## What it gives up

Stated plainly, because a second provider that only had advantages would not have needed an ADR.

| lost | consequence |
|---|---|
| `clear_at: "next_user_message"` | the turn-scoped render is expressed as a **rewrite** of the single system message each turn — a render is live until a user message follows it, and a cleared one is dropped rather than carried. More client-side bookkeeping for the same semantics |
| the cache-write token class | `prompt_tokens` **includes** cached tokens, so the cached count is subtracted; there is no separate cache-creation class, because populating the prefix cache is billed as input. Spend is still attributable, but one class coarser |
| `count_tokens` | no pre-flight endpoint, so the budget uses a **local four-bytes-per-token estimate** over the exact body. It makes no network call and is a function of the request alone (FR-047a), which is a property the vendor endpoint did not have — but it is an estimate |
| refusal detail | Anthropic gives `stop_details.category` and `.explanation`; here the finish reason **is** the category and the explanation arrives as ordinary content. Refusals surface and are never retried, but they are less legible |
| signed thinking | the thinking chunk carries no signature, so it is decoded into a thinking block (reaching the trajectory, staying out of the text the engine reasons over) and **not** echoed back — there is nothing to carry and re-sending unsigned reasoning every turn is tokens spent on nothing |
| streaming | not implemented on this path; the loop does not stream |
| beta features | no equivalent to the `anthropic-beta` header; the set is empty and `config/model.yaml` says why |

An unrecognised stop reason passes through verbatim rather than being mapped onto a guess, which
is the same discipline `unknown` gets everywhere else in this project.

## Consequences

- The model boundary is now load-bearing rather than aspirational, and a third provider is a file
  next to `mistral.go`.
- `config/prices.yaml` carries 2026-09-18 prices for both providers, so spend stays comparable
  across the switch.
- The first live trajectory (`rollout-regression-01-incident`) ran on GLM: 3 turns, 8 tool calls,
  33 worker calls, 31,326 in / 1,903 out tokens, \$0.052, 13 s, culprit
  `k8s.change=shop/payments@rev7` first at 0.708 (`high`), stop `diminishing_returns` — and it
  replays identically with zero network, beside the model-free and fake-model recordings.
- The 16-fixture live eval showed no provider errors (\$0.515, 2m50s) and **15 fixtures excluded
  on the `not_recorded` miss rate**, because those worlds were recorded against the deterministic
  engine and a model explores differently. That is a corpus-alignment problem, not a provider one,
  and it is Phase 9 work. No `pass@1` threshold is published off this run.

## Realised by

Commit `b555ceb` (2026-09-18): `internal/investigation/model/provider.go`, `anthropic.go`,
`mistral.go`, `transport.go`; `config/model.yaml` 1.1.0, `config/model.anthropic.yaml`,
`config/prices.yaml`; the mapping in
`specs/002-investigation-engine/contracts/prompting.md` §Providers.
