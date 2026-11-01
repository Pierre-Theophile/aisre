// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/embedding"

	"gopkg.in/yaml.v3"
)

// Role is one of the three published model roles. The set is closed: a fourth role is a change
// to what this feature does, not a configuration value.
type Role string

const (
	// RoleInvestigator runs the investigation loop.
	RoleInvestigator Role = "investigator"
	// RoleVerifier runs the fresh-context pass that checks every rendered claim back against
	// the evidence it cites (FR-022a).
	RoleVerifier Role = "verifier"
	// RoleLogsLabeller runs the optional labelling pass over mined log templates (FR-009a).
	RoleLogsLabeller Role = "logs_labeller"
)

// Roles is the published role set, in the order a rendering lists them.
var Roles = []Role{RoleInvestigator, RoleVerifier, RoleLogsLabeller}

// RoleConfig is one role's model and its output settings.
//
// There is no temperature and no thinking budget: sampling parameters do not exist on this
// model family and return a 400, and thinking is always on for the investigator's model. What
// FR-061 asks to be recorded is what can be: the model id, the effort, the thinking display and
// the beta set.
type RoleConfig struct {
	// Provider is the vendor serving this role: "anthropic" or "mistral". Empty means
	// "anthropic", so a configuration written before the seam existed — and every trajectory
	// recorded against one — still means exactly what it meant (provider.go).
	Provider Provider `yaml:"provider"`
	// Model is the exact model identifier, e.g. "claude-fable-5-1". Never a date-suffixed
	// variant: the published ids are complete as they stand.
	Model string `yaml:"model"`
	// Purpose says why this model is in this role, so a later reader can tell a deliberate
	// choice from an accident.
	Purpose string `yaml:"purpose"`
	// Effort is the reasoning effort, one of low, medium, high, xhigh, max. Empty means the
	// parameter is not sent, which is required for models that reject it.
	Effort string `yaml:"effort"`
	// ThinkingDisplay is "summarized", "omitted", "updates", or empty for not sent. It controls
	// visibility only: thinking happens and is billed the same under every setting.
	ThinkingDisplay string `yaml:"thinking_display"`
	// BaseURL is the API root of an openai_compatible server; calls go to
	// `POST {base_url}/chat/completions`. It is required for openai_compatible and refused for
	// every other provider, whose endpoint is fixed. The configuration is recorded on every
	// investigation (FR-061), so a recording always says which server it was made against.
	BaseURL string `yaml:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding an openai_compatible server's bearer
	// token. Empty for an unauthenticated local server.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
}

// ProviderOrDefault is the provider this role runs on, with the empty value resolved.
func (r RoleConfig) ProviderOrDefault() Provider {
	if r.Provider == "" {
		return DefaultProvider
	}
	return r.Provider
}

// efforts is the published effort set per provider. Anything else is refused rather than passed
// through to be refused by the API mid-investigation.
//
// The two vendors do not publish the same ladder: Anthropic's `output_config.effort` tops out at
// `max`, Mistral's `reasoning_effort` at `xhigh` and adds `none` and `minimal`. A single union
// would accept `max` on a Mistral role and turn a configuration error into a 400 on the first
// turn of an incident.
var efforts = map[Provider]map[string]struct{}{
	ProviderAnthropic: {"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {}},
	ProviderMistral:   {"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}},
	// The OpenAI `reasoning_effort` vocabulary. A server that does not reason ignores it or
	// refuses it; leave effort empty for such a model.
	ProviderOpenAICompatible: {"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}},
}

// effortAllowed reports whether a provider publishes an effort level.
func effortAllowed(p Provider, effort string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	_, ok := efforts[p][effort]
	return ok
}

// effortSet renders a provider's published effort set for an error message.
func effortSet(p Provider) string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(efforts[p]))
	for name := range efforts[p] {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// displays is the published thinking-display set.
var displays = map[string]struct{}{
	"summarized": {}, "omitted": {}, "updates": {},
}

// effortlessModels are the models that reject the effort parameter. Sending it to one of them is
// a 400 in the middle of a run, so it is refused at load time instead.
var effortlessModels = map[string]struct{}{
	"claude-haiku-4-5": {},
}

// Config is config/model.yaml: the model configuration this feature runs in production, and
// therefore the one every evaluation run must use and record (FR-061).
type Config struct {
	// Version is this document's version, recorded on every investigation. A change to
	// anything in the file is a change that must be evaluated, so the version moves with it.
	Version string `yaml:"version"`
	// Roles is the three published roles. All three are required: an unconfigured role is a
	// role that would silently fall back to a default nobody recorded.
	Roles map[Role]RoleConfig `yaml:"roles"`
	// Betas is the beta feature set the engine depends on, recorded with every run because a
	// beta that changes behaviour changes the run.
	Betas []string `yaml:"betas"`
	// PriceTableVersion links this configuration to the price table cost figures are derived
	// from, so a historical monetary figure stays interpretable (FR-048).
	PriceTableVersion string `yaml:"price_table_version"`
	// Embedding is the embedding model knowledge retrieval places sections and queries with
	// (feature 008 FR-026, contracts §3). Optional: without it retrieval is lexical only.
	Embedding *embedding.Config `yaml:"embedding,omitempty"`
	// Attribution is the service catalog's decision model (feature 009, contracts §6). Optional:
	// without it the published ruleset runs with its published threshold and margin.
	Attribution *AttributionConfig `yaml:"attribution,omitempty"`
}

// Validate refuses a configuration that would fail somewhere less convenient — mid-run, or
// silently.
func (c Config) Validate() error {
	if c.Version == "" {
		return fmt.Errorf("model config: version is required; it is recorded on every investigation (FR-061)")
	}
	if c.PriceTableVersion == "" {
		return fmt.Errorf("model config %s: price_table_version is required; a cost with no price table behind it is not interpretable later (FR-048)", c.Version)
	}
	for _, role := range Roles {
		rc, ok := c.Roles[role]
		if !ok {
			return fmt.Errorf("model config %s: role %s is not configured; an unconfigured role would fall back to a default nobody recorded", c.Version, role)
		}
		if rc.Model == "" {
			return fmt.Errorf("model config %s: role %s names no model", c.Version, role)
		}
		if err := rc.validateProvider(c.Version, role); err != nil {
			return err
		}
		if rc.Effort != "" {
			p := rc.ProviderOrDefault()
			if !effortAllowed(p, rc.Effort) {
				return fmt.Errorf("model config %s: role %s declares effort %q; %s publishes %s",
					c.Version, role, rc.Effort, p, effortSet(p))
			}
			if _, rejects := effortlessModels[rc.Model]; rejects {
				return fmt.Errorf("model config %s: role %s sets effort %q on %s, which rejects the parameter; leave it empty",
					c.Version, role, rc.Effort, rc.Model)
			}
		}
		if rc.ThinkingDisplay != "" {
			if _, ok := displays[rc.ThinkingDisplay]; !ok {
				return fmt.Errorf("model config %s: role %s declares thinking display %q; the published set is summarized, omitted, updates",
					c.Version, role, rc.ThinkingDisplay)
			}
		}
	}
	if c.Embedding != nil {
		if err := c.Embedding.Validate(); err != nil {
			return fmt.Errorf("model config %s: %w", c.Version, err)
		}
	}
	if c.Attribution != nil {
		if err := c.Attribution.Validate(); err != nil {
			return fmt.Errorf("model config %s: %w", c.Version, err)
		}
	}
	for role := range c.Roles {
		if role != RoleInvestigator && role != RoleVerifier && role != RoleLogsLabeller {
			return fmt.Errorf("model config %s: role %s is not one of the published roles %v", c.Version, role, Roles)
		}
	}
	return nil
}

// validateProvider refuses a provider nobody publishes and a provider/model pair that cannot
// work.
//
// The pair check is the one that earns its keep: `zai-glm-5-3` under `provider: anthropic` is a
// configuration that parses, validates against every other rule, and then 404s on the first turn
// of an incident. A model id this build has never heard of is *not* refused — vendors publish
// names faster than this table is edited, and a configuration that names its provider explicitly
// is allowed to be right.
func (r RoleConfig) validateProvider(version string, role Role) error {
	p := r.ProviderOrDefault()
	if !knownProvider(p) {
		return fmt.Errorf("model config %s: role %s declares provider %q; the published set is %s",
			version, role, p, providerList())
	}
	if p == ProviderOpenAICompatible {
		// Any model can sit behind an OpenAI-compatible server — a self-hosted vLLM serving a
		// Mistral model is the ordinary case — so the model-id table has nothing to refuse here.
		// What this provider cannot run without is the server's address.
		if strings.TrimSpace(r.BaseURL) == "" {
			return fmt.Errorf("model config %s: role %s uses provider %s but names no base_url",
				version, role, p)
		}
		u, err := url.Parse(r.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("model config %s: role %s: base_url %q is not an http(s) URL",
				version, role, r.BaseURL)
		}
		return nil
	}
	if r.BaseURL != "" || r.APIKeyEnv != "" {
		return fmt.Errorf("model config %s: role %s sets base_url or api_key_env under provider %q, "+
			"whose endpoint and credential are fixed; use provider %s to point at another server",
			version, role, p, ProviderOpenAICompatible)
	}
	if inferred, ok := ProviderFor(r.Model); ok && inferred != p {
		return fmt.Errorf(
			"model config %s: role %s puts %s behind provider %q, but %s is served by %q; "+
				"the pair would 404 on the first turn of an incident",
			version, role, r.Model, p, r.Model, inferred)
	}
	return nil
}

// Providers returns the distinct providers this configuration uses, in published order. It is
// what a command checks credentials against: a deployment needs a key for every provider it
// configures and for none that it does not.
func (c Config) Providers() []Provider {
	seen := map[Provider]bool{}
	out := make([]Provider, 0, len(Providers))
	for _, published := range Providers {
		for _, role := range Roles {
			if rc, ok := c.Roles[role]; ok && rc.ProviderOrDefault() == published && !seen[published] {
				seen[published] = true
				out = append(out, published)
			}
		}
	}
	// Registered providers follow the published ones, in role order.
	for _, role := range Roles {
		if rc, ok := c.Roles[role]; ok && !seen[rc.ProviderOrDefault()] {
			seen[rc.ProviderOrDefault()] = true
			out = append(out, rc.ProviderOrDefault())
		}
	}
	return out
}

// Role returns one role's configuration.
func (c Config) Role(role Role) (RoleConfig, bool) {
	rc, ok := c.Roles[role]
	return rc, ok
}

// Price is one model's rates, in the table's currency per million tokens. Every class is
// recorded separately because they are billed separately and because the budget manager reasons
// about them separately: a cached prefix is not an input prefix.
type Price struct {
	// Role is which role this model serves, carried so that a price table and a model
	// configuration that have drifted apart say so.
	Role string `yaml:"role"`
	// Input is uncached input.
	Input float64 `yaml:"input"`
	// Output is generated tokens, thinking included.
	Output float64 `yaml:"output"`
	// CacheWrite5m and CacheWrite1h are the two published TTLs.
	CacheWrite5m float64 `yaml:"cache_write_5m"`
	CacheWrite1h float64 `yaml:"cache_write_1h"`
	// CacheRead is a cache hit.
	CacheRead float64 `yaml:"cache_read"`
}

// Prices is config/prices.yaml.
type Prices struct {
	// Version is the price-table version stored on every investigation. A price change is a new
	// version, never an edit of an existing one's numbers.
	Version string `yaml:"version"`
	// Currency and Unit say what the numbers mean, so nobody has to assume.
	Currency string `yaml:"currency"`
	Unit     string `yaml:"unit"`
	// Source says where the rates came from and what they do not cover.
	Source string `yaml:"source"`
	// Models maps a model id to its rates.
	Models map[string]Price `yaml:"models"`
}

// Validate refuses a price table that could produce a wrong or unattributable cost.
func (p Prices) Validate() error {
	if p.Version == "" {
		return fmt.Errorf("prices: version is required; it is stored with every investigation")
	}
	if p.Currency == "" || p.Unit == "" {
		return fmt.Errorf("prices %s: currency and unit are required; a bare number is not a price", p.Version)
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("prices %s: no models priced", p.Version)
	}
	for id, price := range p.Models {
		for name, rate := range map[string]float64{
			"input":          price.Input,
			"output":         price.Output,
			"cache_write_5m": price.CacheWrite5m,
			"cache_write_1h": price.CacheWrite1h,
			"cache_read":     price.CacheRead,
		} {
			if rate <= 0 {
				return fmt.Errorf("prices %s: model %s has %s = %v; an unpriced class silently costs nothing, which is the one answer that is never true",
					p.Version, id, name, rate)
			}
		}
	}
	return nil
}

// LoadConfig reads and validates config/model.yaml.
func LoadConfig(path string) (Config, error) {
	var c Config
	if err := loadYAML(path, &c); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// LoadPrices reads and validates config/prices.yaml.
func LoadPrices(path string) (Prices, error) {
	var p Prices
	if err := loadYAML(path, &p); err != nil {
		return Prices{}, err
	}
	if err := p.Validate(); err != nil {
		return Prices{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// LoadPair reads both documents and refuses the pair when they do not agree: the configuration
// names a price table, and every model it configures must be in it. A run that cannot price
// itself is a run whose spend is unreportable (FR-048).
func LoadPair(configPath, pricesPath string) (Config, Prices, error) {
	c, err := LoadConfig(configPath)
	if err != nil {
		return Config{}, Prices{}, err
	}
	p, err := LoadPrices(pricesPath)
	if err != nil {
		return Config{}, Prices{}, err
	}
	if c.PriceTableVersion != p.Version {
		return Config{}, Prices{}, fmt.Errorf(
			"model config %s names price table %s but %s is version %s; the two are recorded together and must agree",
			c.Version, c.PriceTableVersion, pricesPath, p.Version)
	}
	for _, role := range Roles {
		rc := c.Roles[role]
		if _, priced := p.Models[rc.Model]; !priced {
			return Config{}, Prices{}, fmt.Errorf(
				"model config %s puts %s in role %s, but price table %s does not price it; a run that cannot price itself cannot report its spend",
				c.Version, rc.Model, role, p.Version)
		}
	}
	return c, p, nil
}

func loadYAML(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true) // an unknown key is a typo, and a typo that is ignored is a default nobody chose
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
