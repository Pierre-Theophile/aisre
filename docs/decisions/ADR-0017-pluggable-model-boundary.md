# ADR-0017: A pluggable model boundary, with a self-hosted provider

- Status: **Accepted** (owner decision 2026-11-01: architecture remediation after the Clean
  Architecture and DDD audit)
- Deciders: project owner (the go-ahead), Claude (the design, documented here)
- Amends: ADR-0008 (second model provider behind the model boundary)

## Context

The project rule of 2026-10-15 says every external dependency is pluggable and has a
self-hostable option. Embeddings, the knowledge store, feeders and telemetry backends already
were. The language model was not:

- the provider seam was an unexported interface over a closed set (Anthropic, Mistral), so a
  program embedding the agent could not add one without editing the package;
- neither provider could be pointed at another server. The Mistral endpoint is a constant
  (ADR-0008) because "a configurable base URL is a way for a recording to have been made against
  something other than what it says it was", and the Anthropic adapter exposed no base URL;
- the engine and the verifier linked the Anthropic SDK through the model package.

## Decision

1. **The seam is published.** `model.Adapter` (`Complete`, `CountTokens`) is the interface a
   provider implements, and `model.RegisterProvider(name, spec)` makes it available to every
   client built afterwards. A registered name is accepted by configuration validation. Every
   adapter still puts its calls on the client's `Transport`, so recording and replay are
   unchanged for every provider.
2. **A self-hosted provider ships.** `provider: openai_compatible` speaks the chat-completions
   format at each role's `base_url` (`POST {base_url}/chat/completions`), with an optional
   bearer token read from the variable the role names in `api_key_env`. It covers Ollama, vLLM,
   LocalAI, llama.cpp's server, TGI and OpenAI. It reuses the Mistral body and decoder, which
   already were that format. `config/model.selfhosted.yaml` is a worked example.
3. **The recording rationale is kept, not dropped.** `base_url` is part of the role
   configuration, and the configuration is recorded on every investigation (FR-061), so a
   recording made through `openai_compatible` always says which server answered it. The fixed
   endpoints of `anthropic` and `mistral` stay fixed: setting `base_url` or `api_key_env` under
   either is refused, with a message pointing at `openai_compatible`.
4. **The vendor SDK moves to its adapter.** The Anthropic adapter is now
   `internal/investigation/model/anthropic`, which registers the `anthropic` provider when
   imported. The CLI, the evaluation harness and the model test kit import it; the model
   package, the engine, the verifier, the budget, the runner and replay do not link the SDK, and
   `internal/archtest` fails the build if they start to. A client asked for `anthropic` in a
   build that did not link the adapter says which package to import.

## Consequences

- `model.NewClient` and `NewClientWithTransport` no longer take Anthropic SDK request options.
  Nothing outside the model package passed any. `anthropic.New` still takes them for a program
  that needs to.
- A self-hosted model still has to be priced: the budget is enforced in money and an unpriced
  model is refused (FR-048). The operator prices it at their own compute cost.
- The provider registry is process-global, like `database/sql` drivers. Registering the same
  name twice replaces the earlier registration.
