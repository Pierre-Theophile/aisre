// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
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
	// ProviderOpenAICompatible is any server speaking the OpenAI chat-completions wire format at
	// a configured `base_url`: OpenAI itself, and the self-hosted servers (Ollama, vLLM,
	// LocalAI, llama.cpp, TGI). It is the provider an operator who runs their own model uses.
	ProviderOpenAICompatible Provider = "openai_compatible"
)

// Providers is the published provider set, in the order a rendering lists them.
var Providers = []Provider{ProviderAnthropic, ProviderMistral, ProviderOpenAICompatible}

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
		// openai_compatible names its variable per role (RoleConfig.APIKeyEnv); a registered
		// provider declares its own.
		if spec, ok := registered(p); ok {
			return spec.CredentialEnv
		}
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
	seen := map[string]bool{}
	add := func(env string) {
		if env == "" || seen[env] {
			return
		}
		seen[env] = true
		if strings.TrimSpace(os.Getenv(env)) == "" {
			out = append(out, env)
		}
	}
	for _, p := range c.Providers() {
		add(CredentialEnv(p))
	}
	// An openai_compatible role names its own variable, and may name none: a local server
	// usually takes no credential.
	for _, role := range Roles {
		if rc, ok := c.Roles[role]; ok && rc.ProviderOrDefault() == ProviderOpenAICompatible {
			add(rc.APIKeyEnv)
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

// Adapter is the seam a Client dispatches on: one vendor's wire format.
//
// It is deliberately small. An adapter builds a body, issues it through the client's transport
// and decodes the answer into this package's published types; it owns no retry policy, no budget
// and no recording, because those are the engine's and they are the same whoever answers.
//
// The adapters this repository ships are registered by name: mistral and openai_compatible by
// this package, anthropic by internal/investigation/model/anthropic, which a binary links by
// importing it. A Go program embedding the agent plugs another vendor in the same way, with
// RegisterProvider, without editing this package.
type Adapter interface {
	// Complete issues one call and returns the decoded answer. The caller fills in the
	// transport-level fields (mode, bodies, digests), which are the same for every provider.
	Complete(ctx context.Context, rc RoleConfig, req Request) (*Response, error)
	// CountTokens is the pre-flight admission estimate (FR-047a).
	CountTokens(ctx context.Context, rc RoleConfig, req Request) (int64, error)
}

// AdapterFactory builds an adapter over the client's transport. Every call the adapter makes
// must cross that transport: it is what records a live run and answers a replayed one.
type AdapterFactory func(config Config, transport Transport) (Adapter, error)

// ProviderSpec is what a provider registers.
type ProviderSpec struct {
	// New builds the adapter.
	New AdapterFactory
	// Efforts is the published effort set; empty means the provider takes no effort parameter.
	Efforts []string
	// CredentialEnv is the environment variable the credential is read from, if any.
	CredentialEnv string
}

var (
	registryMu sync.RWMutex
	registry   = map[Provider]ProviderSpec{}
)

// RegisterProvider makes a provider available to every Client built afterwards. Registering a
// name twice replaces the earlier registration, which is what lets a test substitute an adapter.
func RegisterProvider(name Provider, spec ProviderSpec) {
	if name == "" || spec.New == nil {
		panic("model: RegisterProvider needs a name and a factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = spec
	if _, published := efforts[name]; !published || len(spec.Efforts) > 0 {
		set := map[string]struct{}{}
		for _, e := range spec.Efforts {
			set[e] = struct{}{}
		}
		efforts[name] = set
	}
}

func registered(name Provider) (ProviderSpec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	spec, ok := registry[name]
	return spec, ok
}

// knownProvider reports whether a name is published or registered.
func knownProvider(p Provider) bool {
	for _, published := range Providers {
		if p == published {
			return true
		}
	}
	_, ok := registered(p)
	return ok
}

// providerFor resolves the adapter serving a role.
func (c *Client) providerFor(rc RoleConfig) (Adapter, error) {
	name := rc.ProviderOrDefault()
	if p, ok := c.providers[name]; ok {
		return p, nil
	}
	if name == ProviderAnthropic {
		return nil, fmt.Errorf("model config %s: provider %q is not linked into this build; import "+
			"github.com/Pierre-Theophile/aisre/internal/investigation/model/anthropic", c.config.Version, name)
	}
	return nil, fmt.Errorf("model config %s: no client for provider %q; the published set is %s",
		c.config.Version, name, providerList())
}

func providerList() string {
	seen := map[Provider]bool{}
	names := make([]string, 0, len(Providers))
	for _, p := range Providers {
		seen[p] = true
		names = append(names, string(p))
	}
	registryMu.RLock()
	for p := range registry {
		if !seen[p] {
			names = append(names, string(p))
		}
	}
	registryMu.RUnlock()
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
