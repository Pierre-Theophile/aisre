// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// The provider seam (Track L; FR-041, FR-042a, FR-061).
//
// The engine's model boundary was written against one vendor. A second one — Mistral, serving
// GLM — is not a second boundary: it is a second *body shape* behind the same seam. Everything
// that makes this feature what it is lives below the shape and is shared unchanged:
//
//   - the same `Transport` (transport.go), so a recording is still the exact bytes and a replay
//     is still a canonical-digest match on those bytes. Neither knows which vendor produced
//     them, which is why the two checked-in fake-model trajectories replay byte-identically
//     after this change;
//   - the same `Response`/`Block`/`Usage` vocabulary, so the loop, the ledger, the budget and
//     the trajectory are provider-agnostic. A provider's job is to translate into that
//     vocabulary, never to extend it;
//   - the same refusal discipline: a refusal is a terminal condition with a stop reason in the
//     engine's own words, recorded and never retried (FR-045b).
//
// A provider is chosen per *role*, not per client, because that is what FR-022a asks for: the
// verifier runs a different model from the investigator so the error it exists to catch is
// decorrelated, and "different model" may well mean "different vendor".

// Provider is a vendor the model boundary can speak to.
type Provider string

const (
	// ProviderAnthropic is the Anthropic Messages API. It is the default, so a configuration
	// written before this seam existed — and every trajectory recorded against one — still
	// means exactly what it meant.
	ProviderAnthropic Provider = "anthropic"
	// ProviderMistral is the Mistral chat-completions API, which also serves the GLM models.
	ProviderMistral Provider = "mistral"
)

// Providers is the published provider set, in the order a rendering lists them.
var Providers = []Provider{ProviderAnthropic, ProviderMistral}

// DefaultProvider is what an unset `provider:` means. It is Anthropic on purpose: the checked-in
// configuration, the recorded trajectories and every test predate the seam, and a default that
// changed their meaning would be a silent rewrite of the corpus.
const DefaultProvider = ProviderAnthropic

// The credential each provider needs. They are named here rather than left to a vendor SDK's own
// resolution order because a command has to decide *before* it starts whether this deployment
// can reach the models it is configured for, and say which variable is missing when it cannot.
const (
	// EnvAnthropicAPIKey is the Anthropic credential.
	EnvAnthropicAPIKey = "ANTHROPIC_API_KEY"
	// EnvMistralAPIKey is the Mistral credential.
	EnvMistralAPIKey = "MISTRAL_API_KEY"
)

// CredentialEnv returns the environment variable a provider's credential is read from.
func CredentialEnv(p Provider) string {
	switch p {
	case ProviderMistral:
		return EnvMistralAPIKey
	case ProviderAnthropic:
		return EnvAnthropicAPIKey
	default:
		return ""
	}
}

// MissingCredentials returns the environment variables this configuration needs and the
// environment does not have, in published provider order.
//
// It is what a command checks before it starts. A deployment that cannot resolve a credential for
// a provider it configures would fail on its first turn, in the middle of an incident, which is
// the worst moment to discover a configuration problem — so it is reported at startup and the
// command runs model-free instead, naming the variable that was missing.
func (c Config) MissingCredentials() []string {
	var out []string
	for _, p := range c.Providers() {
		env := CredentialEnv(p)
		if env == "" {
			continue
		}
		if strings.TrimSpace(os.Getenv(env)) == "" {
			out = append(out, env)
		}
	}
	return out
}

// modelPrefixes maps a model-id prefix to the provider that serves it.
//
// It exists so that a provider/model pair that cannot work is refused at load time rather than
// mid-investigation, and so that a canned transport can render a turn in the right body shape
// from the model id alone. It is a prefix table rather than an exhaustive list because vendors
// publish new ids faster than this file is edited, and a new `mistral-…` is still a Mistral
// model.
var modelPrefixes = []struct {
	prefix   string
	provider Provider
}{
	{"claude-", ProviderAnthropic},
	{"zai-", ProviderMistral},
	{"glm-", ProviderMistral},
	{"mistral", ProviderMistral},
	{"magistral", ProviderMistral},
	{"ministral", ProviderMistral},
	{"codestral", ProviderMistral},
	{"voxtral", ProviderMistral},
	{"devstral", ProviderMistral},
	{"pixtral", ProviderMistral},
	{"open-mistral", ProviderMistral},
	{"open-mixtral", ProviderMistral},
}

// ProviderFor returns the provider a model id belongs to, and whether the id was recognised.
//
// An unrecognised id is not an error here: a vendor may publish a name this table has never
// seen, and a configuration that names the provider explicitly is always allowed to be right.
// It is only used to *refuse a contradiction* (config.go) and to pick a body shape for a canned
// transport (modeltest).
func ProviderFor(modelID string) (Provider, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	for _, entry := range modelPrefixes {
		if strings.HasPrefix(id, entry.prefix) {
			return entry.provider, true
		}
	}
	return "", false
}

// provider is the internal seam a Client dispatches on.
//
// It is deliberately small. A provider builds a body, issues it through the client's transport
// and decodes the answer into this package's published types; it owns no retry policy, no budget
// and no recording, because those are the engine's and they are the same whoever answers.
type provider interface {
	// name is the provider this is, for an error message.
	name() Provider
	// complete issues one call and returns the decoded answer. The caller fills in the
	// transport-level fields (mode, bodies, digests), which are the same for every provider.
	complete(ctx context.Context, rc RoleConfig, req Request) (*Response, error)
	// countTokens is the pre-flight admission estimate (FR-047a).
	countTokens(ctx context.Context, rc RoleConfig, req Request) (int64, error)
}

// providerFor resolves the provider serving a role.
func (c *Client) providerFor(rc RoleConfig) (provider, error) {
	p, ok := c.providers[rc.ProviderOrDefault()]
	if !ok {
		return nil, fmt.Errorf("model config %s: no client for provider %q; the published set is %s",
			c.config.Version, rc.ProviderOrDefault(), providerList())
	}
	return p, nil
}

func providerList() string {
	names := make([]string, 0, len(Providers))
	for _, p := range Providers {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// APIError is a provider that answered with an HTTP status rather than a message.
//
// It is typed rather than a formatted string so that a caller can tell a 429 from a 400 without
// parsing prose, and so that the two providers fail the same way: the Anthropic SDK publishes
// its own typed error, and the Mistral client — which is plain net/http — publishes this one.
type APIError struct {
	// Provider is who answered.
	Provider Provider
	// Status is the HTTP status code.
	Status int
	// Body is the response body, truncated, so a reader sees the vendor's own explanation.
	Body string
}

// Error renders the failure.
func (e *APIError) Error() string {
	return fmt.Sprintf("model: %s api returned HTTP %d: %s", e.Provider, e.Status, e.Body)
}

// Retryable reports whether the status is one a caller *could* retry. This engine does not —
// a retry is a call nobody budgeted for and a turn nobody recorded (FR-045b) — but a deployment
// wrapping it may want to know.
func (e *APIError) Retryable() bool {
	return e.Status == 429 || (e.Status >= 500 && e.Status <= 599)
}
