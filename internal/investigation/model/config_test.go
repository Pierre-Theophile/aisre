// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The checked-in configuration, from this package up to the repository root.
var (
	configPath          = filepath.Join("..", "..", "..", "config", "model.yaml")
	anthropicConfigPath = filepath.Join("..", "..", "..", "config", "model.anthropic.yaml")
	pricesPath          = filepath.Join("..", "..", "..", "config", "prices.yaml")
)

// TestCheckedInConfigLoads is the one that matters: the configuration this feature ships with
// must load, validate, and agree with its price table. FR-061 requires every evaluation run to
// use the production configuration and to record it, which starts with the production
// configuration being readable.
//
// Both checked-in configurations are asserted, because both are shipped and either may be put in
// force with a flag: `config/model.yaml` is production (Mistral, serving GLM) and
// `config/model.anthropic.yaml` is the alternative. A file that only one of them exercises is a
// file that rots.
func TestCheckedInConfigLoads(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		path     string
		provider model.Provider
		models   map[model.Role]string
		betas    []string
	}{
		{
			name:     "production (mistral, serving glm)",
			path:     configPath,
			provider: model.ProviderMistral,
			models: map[model.Role]string{
				model.RoleInvestigator: "zai-glm-5-3",
				model.RoleVerifier:     "mistral-medium-latest",
				model.RoleLogsLabeller: "ministral-8b-latest",
			},
			// No betas: `anthropic-beta` is an Anthropic header, and the turn-scoped ledger
			// re-render it enabled there is met here by rewriting the single system message.
			betas: nil,
		},
		{
			name:     "the alternative (anthropic)",
			path:     anthropicConfigPath,
			provider: model.ProviderAnthropic,
			models: map[model.Role]string{
				model.RoleInvestigator: "claude-fable-5-1",
				model.RoleVerifier:     "claude-opus-5",
				model.RoleLogsLabeller: "claude-haiku-4-5",
			},
			betas: []string{"mid-conversation-system-clear-at-2026-08-21"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertCheckedInConfig(t, tc.path, tc.provider, tc.models, tc.betas)
		})
	}
}

func assertCheckedInConfig(
	t *testing.T,
	path string,
	provider model.Provider,
	want map[model.Role]string,
	wantBetas []string,
) {
	t.Helper()

	cfg, prices, err := model.LoadPair(path, pricesPath)
	if err != nil {
		t.Fatalf("load the checked-in configuration: %v", err)
	}

	// The version is what is recorded on every investigation, so an unversioned configuration
	// is an uninterpretable historical run.
	if cfg.Version == "" {
		t.Error("model config has no version")
	}
	if cfg.PriceTableVersion != prices.Version {
		t.Errorf("model config names price table %q, prices.yaml is %q", cfg.PriceTableVersion, prices.Version)
	}

	// The three roles and their models, spelled out. A model swap is a change that must be
	// evaluated (FR-061), so it fails this test rather than passing quietly.
	for role, id := range want {
		rc, ok := cfg.Role(role)
		if !ok {
			t.Errorf("role %s is not configured", role)
			continue
		}
		if rc.Model != id {
			t.Errorf("role %s runs %s, want %s; a model change must be evaluated, not merged", role, rc.Model, id)
		}
		if rc.ProviderOrDefault() != provider {
			t.Errorf("role %s runs on provider %s, want %s", role, rc.ProviderOrDefault(), provider)
		}
		if rc.Purpose == "" {
			t.Errorf("role %s says nothing about why this model is in it", role)
		}
	}

	// The verifier runs a different model from the investigator, which is what decorrelates the
	// error it exists to catch from the error that produced it (FR-022a).
	if cfg.Roles[model.RoleVerifier].Model == cfg.Roles[model.RoleInvestigator].Model {
		t.Error("the verifier runs the investigator's own model; FR-022a asks for a different one")
	}

	// A configuration uses the providers it names and no others, which is what a command checks
	// credentials against.
	if got := cfg.Providers(); !reflect.DeepEqual(got, []model.Provider{provider}) {
		t.Errorf("providers = %v, want exactly [%s]", got, provider)
	}

	// The investigator's depth is controlled by effort, because thinking is always on and its
	// budget is not configurable on this model family.
	if investigator, _ := cfg.Role(model.RoleInvestigator); investigator.Effort == "" {
		t.Error("the investigator declares no effort; it is the only control over reasoning depth on this model family")
	}

	// The logs labeller must NOT declare effort: its model rejects the parameter, and a 400 in
	// the middle of an investigation is the worst place to discover that.
	if labeller, _ := cfg.Role(model.RoleLogsLabeller); labeller.Effort != "" {
		t.Errorf("the logs labeller declares effort %q on a model that rejects it", labeller.Effort)
	}

	// The beta set is recorded because a beta that changes behaviour changes the run. On the
	// Anthropic configuration that means the per-turn ledger re-render; on Mistral it means
	// nothing, and an empty set said so.
	for _, beta := range wantBetas {
		if !contains(cfg.Betas, beta) {
			t.Errorf("betas = %v, want %s", cfg.Betas, beta)
		}
	}
	if len(wantBetas) == 0 && len(cfg.Betas) != 0 {
		t.Errorf("betas = %v, want none on a provider that has no beta header", cfg.Betas)
	}

	// Every configured model is priced, in every class. An unpriced class silently costs
	// nothing, which is the one answer that is never true.
	for _, role := range model.Roles {
		rc, _ := cfg.Role(role)
		price, ok := prices.Models[rc.Model]
		if !ok {
			t.Errorf("model %s (role %s) is not in price table %s", rc.Model, role, prices.Version)
			continue
		}
		if price.Input <= 0 || price.Output <= 0 || price.CacheRead <= 0 ||
			price.CacheWrite5m <= 0 || price.CacheWrite1h <= 0 {
			t.Errorf("model %s is not fully priced: %+v", rc.Model, price)
		}
	}
}

// TestConfigRoundTrips asserts the configuration survives a serialise/parse cycle unchanged.
// It is what makes "the run recorded its model configuration" mean something: the copy written
// into investigation.investigations.model_config and into the decision record is the same
// document that was loaded, not a lossy rendering of it.
func TestConfigRoundTrips(t *testing.T) {
	t.Parallel()

	cfg, prices, err := model.LoadPair(configPath, pricesPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, tc := range []struct {
		name     string
		original any
		decoded  any
	}{
		{"model config", cfg, new(model.Config)},
		{"price table", prices, new(model.Prices)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := yaml.Marshal(tc.original)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := yaml.Unmarshal(encoded, tc.decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := reflect.ValueOf(tc.decoded).Elem().Interface()
			if !reflect.DeepEqual(tc.original, got) {
				t.Errorf("round trip changed the document:\n before: %+v\n  after: %+v", tc.original, got)
			}
		})
	}
}

func TestConfigValidationRejections(t *testing.T) {
	t.Parallel()

	base := func() model.Config {
		return model.Config{
			Version:           "1.0.0",
			PriceTableVersion: "2026-06-24",
			Roles: map[model.Role]model.RoleConfig{
				model.RoleInvestigator: {Model: "claude-fable-5-1", Effort: "high", ThinkingDisplay: "summarized"},
				model.RoleVerifier:     {Model: "claude-opus-5", Effort: "high"},
				model.RoleLogsLabeller: {Model: "claude-haiku-4-5"},
			},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the baseline configuration should be valid: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*model.Config)
		mustSay string
	}{
		{
			name:    "no version",
			mutate:  func(c *model.Config) { c.Version = "" },
			mustSay: "version is required",
		},
		{
			name:    "no price table",
			mutate:  func(c *model.Config) { c.PriceTableVersion = "" },
			mustSay: "price_table_version is required",
		},
		{
			name:    "a missing role",
			mutate:  func(c *model.Config) { delete(c.Roles, model.RoleVerifier) },
			mustSay: "not configured",
		},
		{
			name: "effort on a model that rejects it",
			mutate: func(c *model.Config) {
				rc := c.Roles[model.RoleLogsLabeller]
				rc.Effort = "low"
				c.Roles[model.RoleLogsLabeller] = rc
			},
			mustSay: "rejects the parameter",
		},
		{
			name: "an unpublished effort",
			mutate: func(c *model.Config) {
				rc := c.Roles[model.RoleInvestigator]
				rc.Effort = "extreme"
				c.Roles[model.RoleInvestigator] = rc
			},
			mustSay: "anthropic publishes high, low, max, medium, xhigh",
		},
		{
			name: "an unpublished thinking display",
			mutate: func(c *model.Config) {
				rc := c.Roles[model.RoleInvestigator]
				rc.ThinkingDisplay = "verbose"
				c.Roles[model.RoleInvestigator] = rc
			},
			mustSay: "summarized, omitted, updates",
		},
		{
			name:    "a role nobody published",
			mutate:  func(c *model.Config) { c.Roles["summariser"] = model.RoleConfig{Model: "claude-opus-5"} },
			mustSay: "not one of the published roles",
		},
		{
			name: "a provider nobody published",
			mutate: func(c *model.Config) {
				rc := c.Roles[model.RoleInvestigator]
				rc.Provider = "openai"
				c.Roles[model.RoleInvestigator] = rc
			},
			mustSay: "the published set is anthropic, mistral",
		},
		{
			// The one that earns its keep: a pair that parses, validates against every other
			// rule and then 404s on the first turn of an incident.
			name: "a model behind the wrong provider",
			mutate: func(c *model.Config) {
				rc := c.Roles[model.RoleInvestigator]
				rc.Provider = model.ProviderAnthropic
				rc.Model = "zai-glm-5-3"
				c.Roles[model.RoleInvestigator] = rc
			},
			mustSay: `served by "mistral"`,
		},
		{
			name: "a mistral role reaching for an anthropic effort",
			mutate: func(c *model.Config) {
				rc := c.Roles[model.RoleInvestigator]
				rc.Provider = model.ProviderMistral
				rc.Model = "zai-glm-5-3"
				rc.Effort = "max"
				c.Roles[model.RoleInvestigator] = rc
			},
			mustSay: "mistral publishes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Fatalf("error %q does not say %q", err, tc.mustSay)
			}
		})
	}
}

func TestLoadPairRefusesADisagreement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "model.yaml")
	prices := filepath.Join(dir, "prices.yaml")
	write(t, cfg, `
version: "1.0.0"
price_table_version: "1999-01-01"
roles:
  investigator: {model: claude-fable-5-1, purpose: x, effort: high, thinking_display: summarized}
  verifier: {model: claude-opus-5, purpose: x, effort: high, thinking_display: omitted}
  logs_labeller: {model: claude-haiku-4-5, purpose: x, effort: "", thinking_display: ""}
betas: []
`)
	write(t, prices, `
version: "2026-06-24"
currency: USD
unit: per_million_tokens
source: test
models:
  claude-fable-5-1: {role: investigator, input: 10, output: 50, cache_write_5m: 12.5, cache_write_1h: 20, cache_read: 0.25}
  claude-opus-5: {role: verifier, input: 5, output: 25, cache_write_5m: 6.25, cache_write_1h: 10, cache_read: 0.5}
  claude-haiku-4-5: {role: logs_labeller, input: 1, output: 5, cache_write_5m: 1.25, cache_write_1h: 2, cache_read: 0.1}
`)

	_, _, err := model.LoadPair(cfg, prices)
	if err == nil {
		t.Fatal("LoadPair accepted a configuration naming a price table version that does not exist")
	}
	if !strings.Contains(err.Error(), "must agree") {
		t.Fatalf("error %q does not explain the disagreement", err)
	}
}

// An unknown key is a typo, and a typo that is ignored is a default nobody chose.
func TestLoadRefusesAnUnknownKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "model.yaml")
	write(t, path, `
version: "1.0.0"
price_table_version: "2026-06-24"
temperature: 0.7
roles:
  investigator: {model: claude-fable-5-1, purpose: x, effort: high, thinking_display: summarized}
  verifier: {model: claude-opus-5, purpose: x, effort: high, thinking_display: omitted}
  logs_labeller: {model: claude-haiku-4-5, purpose: x, effort: "", thinking_display: ""}
betas: []
`)
	if _, err := model.LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted a temperature, which does not exist on this model family")
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, each := range haystack {
		if each == needle {
			return true
		}
	}
	return false
}
