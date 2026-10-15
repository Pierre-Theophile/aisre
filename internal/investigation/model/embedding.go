// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The embedding block (feature 008 FR-026–FR-029, contracts §3, research R5).
//
// It is validated apart from the chat roles because it is a different kind of dependency: no role
// reasons with it, and it is optional. Absent, or its key variable unset, knowledge retrieval runs
// lexical only and every digest says so (FR-027); the agent never needs an embedding provider to
// run. One provider is published, `openai_compatible`, because one wire format —
// `POST {base_url}/embeddings` — covers the hosted APIs and the self-hosted servers alike
// (Mistral, OpenAI, Ollama, vLLM, Hugging Face TEI, LocalAI, llama.cpp). A Go program embedding
// the agent plugs any other provider in by implementing knowledge.Embedder.

// EmbeddingProviderOpenAICompatible is the one published embedding provider.
const EmbeddingProviderOpenAICompatible = "openai_compatible"

// EmbeddingProviders is the published set.
var EmbeddingProviders = []string{EmbeddingProviderOpenAICompatible}

// EmbeddingConfig is the `embedding:` block.
type EmbeddingConfig struct {
	// Provider is the adapter: `openai_compatible`.
	Provider string `yaml:"provider"`
	// BaseURL is the server's API root; vectors are asked of `POST {base_url}/embeddings`.
	BaseURL string `yaml:"base_url"`
	// Model is the model name the server expects.
	Model string `yaml:"model"`
	// Dimensions is the vector length; a response of another length is refused.
	Dimensions int `yaml:"dimensions"`
	// APIKeyEnv names the environment variable holding a bearer token. Empty for an
	// unauthenticated local server.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// QueryPrefix and DocumentPrefix are prepended per purpose, for models trained with them
	// (`query: `, `passage: `).
	QueryPrefix    string `yaml:"query_prefix,omitempty"`
	DocumentPrefix string `yaml:"document_prefix,omitempty"`
}

// Validate refuses a block that would fail on the first section it embedded.
func (e EmbeddingConfig) Validate() error {
	known := false
	for _, p := range EmbeddingProviders {
		if e.Provider == p {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("embedding: provider %q is not published; the set is %s (a Go program embedding the agent may register another knowledge.Embedder)",
			e.Provider, strings.Join(EmbeddingProviders, ", "))
	}
	if e.BaseURL == "" {
		return fmt.Errorf("embedding: base_url is required; vectors are asked of POST {base_url}/embeddings")
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("embedding: base_url %q is not an http(s) URL", e.BaseURL)
	}
	if e.Model == "" {
		return fmt.Errorf("embedding: model is required")
	}
	if e.Dimensions <= 0 {
		return fmt.Errorf("embedding: dimensions must be positive; a response of another length is refused, so the length has to be stated")
	}
	return nil
}

// ModelID is the id every vector this block produces is stored under.
func (e EmbeddingConfig) ModelID() knowledge.ModelID {
	return knowledge.ModelID{Provider: e.Provider, Model: e.Model, Dimensions: e.Dimensions}
}

// APIKey resolves the bearer token, and reports whether the block can be used: false when it names
// a key variable that is unset, which means lexical only (FR-027), not an error.
func (e EmbeddingConfig) APIKey() (string, bool) {
	if e.APIKeyEnv == "" {
		return "", true
	}
	key := os.Getenv(e.APIKeyEnv)
	return key, key != ""
}
