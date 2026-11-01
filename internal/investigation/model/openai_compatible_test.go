// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The openai_compatible provider: any chat-completions server at a configured base_url, which is
// how an operator runs the agent on a self-hosted model.

var selfhostedConfigPath = filepath.Join("..", "..", "..", "config", "model.selfhosted.yaml")

// selfhostedPair loads the published self-hosted example and a price table that prices its models.
func selfhostedPair(t *testing.T, baseURL string) (model.Config, model.Prices) {
	t.Helper()
	config, err := model.LoadConfig(selfhostedConfigPath)
	if err != nil {
		t.Fatalf("load the self-hosted example: %v", err)
	}
	for role, rc := range config.Roles {
		rc.BaseURL = baseURL
		rc.APIKeyEnv = ""
		config.Roles[role] = rc
	}
	rate := model.Price{Input: 0.01, Output: 0.01, CacheWrite5m: 0.01, CacheWrite1h: 0.01, CacheRead: 0.01}
	prices := model.Prices{
		Version: config.PriceTableVersion, Currency: "USD", Unit: "per_million_tokens",
		Models: map[string]model.Price{},
	}
	for _, rc := range config.Roles {
		prices.Models[rc.Model] = rate
	}
	return config, prices
}

// TestTheSelfHostedExampleValidates keeps the published example loadable.
func TestTheSelfHostedExampleValidates(t *testing.T) {
	t.Parallel()
	config, err := model.LoadConfig(selfhostedConfigPath)
	if err != nil {
		t.Fatalf("config/model.selfhosted.yaml: %v", err)
	}
	for _, role := range model.Roles {
		rc, _ := config.Role(role)
		if rc.ProviderOrDefault() != model.ProviderOpenAICompatible || rc.BaseURL == "" {
			t.Errorf("role %s = %+v, want openai_compatible with a base_url", role, rc)
		}
	}
}

// TestAnOpenAICompatibleServerAnswersAtItsBaseURL is the whole path against a real HTTP server:
// the call goes to {base_url}/chat/completions with the role's model, and the answer decodes into
// the engine's vocabulary.
func TestAnOpenAICompatibleServerAnswersAtItsBaseURL(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		paths    []string
		bodies   []string
		authSeen []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(raw))
		authSeen = append(authSeen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id": "chatcmpl-1", "object": "chat.completion", "model": "Qwen/Qwen3-32B",
		  "choices": [{"index": 0, "finish_reason": "stop",
		               "message": {"role": "assistant", "content": "checkout is degraded"}}],
		  "usage": {"prompt_tokens": 120, "completion_tokens": 8, "total_tokens": 128}
		}`)
	}))
	defer server.Close()

	config, prices := selfhostedPair(t, server.URL+"/v1/")
	client, err := model.NewClientWithTransport(config, prices, model.NewLiveTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Text() != "checkout is degraded" {
		t.Errorf("text = %q", resp.Text())
	}
	if resp.Usage.Input != 120 || resp.Usage.Output != 8 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("paths = %v, want one call to /v1/chat/completions (trailing slash on base_url trimmed)", paths)
	}
	if !strings.Contains(bodies[0], `"model":"Qwen/Qwen3-32B"`) {
		t.Errorf("the request does not name the role's model: %s", bodies[0])
	}
	if authSeen[0] != "" {
		t.Errorf("an unauthenticated server was sent Authorization %q", authSeen[0])
	}
}

// TestAnOpenAICompatibleRoleSendsItsNamedCredential: the bearer token comes from the variable the
// role names, and a live call with that variable unset is refused before it is made.
func TestAnOpenAICompatibleRoleSendsItsNamedCredential(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop",
		  "message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	config, prices := selfhostedPair(t, server.URL+"/v1")
	rc := config.Roles[model.RoleInvestigator]
	rc.APIKeyEnv = "AISRE_TEST_SELFHOSTED_KEY"
	config.Roles[model.RoleInvestigator] = rc

	t.Setenv("AISRE_TEST_SELFHOSTED_KEY", "")
	if missing := config.MissingCredentials(); len(missing) != 1 || missing[0] != "AISRE_TEST_SELFHOSTED_KEY" {
		t.Errorf("missing credentials = %v, want the role's variable", missing)
	}
	client, err := model.NewClientWithTransport(config, prices, model.NewLiveTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Complete(context.Background(), request()); err == nil ||
		!strings.Contains(err.Error(), "AISRE_TEST_SELFHOSTED_KEY") {
		t.Fatalf("a live call with the credential unset = %v, want a refusal naming the variable", err)
	}

	t.Setenv("AISRE_TEST_SELFHOSTED_KEY", "s3cret")
	client, err = model.NewClientWithTransport(config, prices, model.NewLiveTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the role's token", got)
	}
}

// TestBaseURLIsValidatedPerProvider: openai_compatible needs an http(s) base_url, and every other
// provider refuses one, because their endpoints are fixed and a recording must say what it was
// made against.
func TestBaseURLIsValidatedPerProvider(t *testing.T) {
	t.Parallel()
	base, _ := selfhostedPair(t, "http://localhost:8000/v1")

	cases := []struct {
		name    string
		rc      model.RoleConfig
		wantErr string
	}{
		{"no base_url", model.RoleConfig{Provider: model.ProviderOpenAICompatible, Model: "m"}, "names no base_url"},
		{"not a URL", model.RoleConfig{Provider: model.ProviderOpenAICompatible, Model: "m", BaseURL: "localhost:8000"}, "not an http(s) URL"},
		{"base_url on mistral", model.RoleConfig{Provider: model.ProviderMistral, Model: "mistral-small-latest", BaseURL: "http://x/v1"}, "whose endpoint and credential are fixed"},
		{"a mistral model on a self-hosted server", model.RoleConfig{Provider: model.ProviderOpenAICompatible, Model: "mistral-small-latest", BaseURL: "http://x/v1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := base
			config.Roles = map[model.Role]model.RoleConfig{}
			for role, rc := range base.Roles {
				config.Roles[role] = rc
			}
			config.Roles[model.RoleVerifier] = tc.rc
			err := config.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// houseAdapter is a provider a Go program plugs in without editing the model package. Like every
// adapter it puts its call on the client's transport, which is what records it.
type houseAdapter struct {
	http *http.Client
	url  string
}

func (a houseAdapter) Complete(ctx context.Context, _ model.RoleConfig, _ model.Request) (*model.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, strings.NewReader(`{"q":1}`))
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &model.Response{Model: "house-model", StopReason: "end_turn",
		Blocks: []model.Block{{Kind: model.BlockText, Text: strings.TrimSpace(string(answer))}}}, nil
}

func (houseAdapter) CountTokens(context.Context, model.RoleConfig, model.Request) (int64, error) {
	return 1, nil
}

// TestARegisteredProviderIsPluggedInWithoutEditingThePackage is the extension seam: register a
// name and a factory, name it in the configuration, and the client dispatches to it.
func TestARegisteredProviderIsPluggedInWithoutEditingThePackage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"answer":"from the house provider"}`)
	}))
	defer server.Close()

	const house model.Provider = "house_test_provider"
	model.RegisterProvider(house, model.ProviderSpec{
		New: func(_ model.Config, transport model.Transport) (model.Adapter, error) {
			return houseAdapter{http: &http.Client{Transport: transport}, url: server.URL + "/complete"}, nil
		},
	})

	config, prices := selfhostedPair(t, "http://localhost:8000/v1")
	rc := config.Roles[model.RoleInvestigator]
	rc.Provider, rc.BaseURL, rc.APIKeyEnv = house, "", ""
	config.Roles[model.RoleInvestigator] = rc
	if err := config.Validate(); err != nil {
		t.Fatalf("a registered provider is refused by validation: %v", err)
	}
	client, err := model.NewClientWithTransport(config, prices, model.NewLiveTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Text() != `{"answer":"from the house provider"}` {
		t.Errorf("text = %q; the client did not dispatch to the registered adapter", resp.Text())
	}

	unknown := config
	unknown.Roles = map[model.Role]model.RoleConfig{}
	for role, rc := range config.Roles {
		unknown.Roles[role] = rc
	}
	rc.Provider = "never_registered"
	unknown.Roles[model.RoleInvestigator] = rc
	if err := unknown.Validate(); err == nil {
		t.Error("an unregistered provider passed validation")
	}
}
