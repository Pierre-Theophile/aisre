// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

func TestNewClientCarriesTheRecordedConfiguration(t *testing.T) {
	t.Parallel()

	cfg, prices, err := model.LoadPair(anthropicConfigPath, pricesPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	client, err := model.NewClient(cfg, prices)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.Config().Version != cfg.Version {
		t.Errorf("client config version = %q, want %q", client.Config().Version, cfg.Version)
	}
	if client.Prices().Version != prices.Version {
		t.Errorf("client price table = %q, want %q", client.Prices().Version, prices.Version)
	}

	id, err := client.ModelFor(model.RoleInvestigator)
	if err != nil {
		t.Fatalf("ModelFor(investigator): %v", err)
	}
	if id != "claude-fable-5-1" {
		t.Errorf("investigator model = %q, want claude-fable-5-1", id)
	}
	if _, err := client.ModelFor("summariser"); err == nil {
		t.Error("ModelFor accepted a role nobody published")
	}
}

// TestTheProductionConfigurationRunsOnMistral pins what production actually calls. The provider
// is part of the recorded configuration (FR-061): a run whose trajectory does not say which
// vendor answered is a run nobody can reproduce.
func TestTheProductionConfigurationRunsOnMistral(t *testing.T) {
	t.Parallel()

	cfg, prices, err := model.LoadPair(configPath, pricesPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client, err := model.NewClient(cfg, prices)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	for role, want := range map[model.Role]string{
		model.RoleInvestigator: "zai-glm-5-3",
		model.RoleVerifier:     "mistral-medium-latest",
		model.RoleLogsLabeller: "ministral-8b-latest",
	} {
		id, err := client.ModelFor(role)
		if err != nil {
			t.Fatalf("ModelFor(%s): %v", role, err)
		}
		if id != want {
			t.Errorf("%s model = %q, want %q", role, id, want)
		}
		provider, err := client.Provider(role)
		if err != nil {
			t.Fatalf("Provider(%s): %v", role, err)
		}
		if provider != model.ProviderMistral {
			t.Errorf("%s provider = %q, want mistral", role, provider)
		}
	}
}

func TestNewClientRefusesAMismatchedPriceTable(t *testing.T) {
	t.Parallel()

	cfg, prices, err := model.LoadPair(anthropicConfigPath, pricesPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	prices.Version = "1999-01-01"

	if _, err := model.NewClient(cfg, prices); err == nil {
		t.Fatal("NewClient accepted a price table the configuration does not name")
	} else if !strings.Contains(err.Error(), "must agree") {
		t.Fatalf("error %q does not explain the disagreement", err)
	}
}
