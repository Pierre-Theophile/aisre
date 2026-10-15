// SPDX-License-Identifier: Apache-2.0

package embedding

import (
	"fmt"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// FromConfig builds the embedder a model configuration's `embedding:` block names. It returns a
// nil embedder and the reason when retrieval must run lexical only: no block, or its key variable
// unset (FR-027). Only a block that is present, usable and still cannot be built is an error.
func FromConfig(cfg *model.EmbeddingConfig) (knowledge.Embedder, string, error) {
	if cfg == nil {
		return nil, "the model configuration has no embedding block", nil
	}
	key, ok := cfg.APIKey()
	if !ok {
		return nil, fmt.Sprintf("the embedding block names $%s, which is not set", cfg.APIKeyEnv), nil
	}
	switch cfg.Provider {
	case model.EmbeddingProviderOpenAICompatible:
		e, err := NewOpenAICompatible(OpenAICompatibleConfig{
			BaseURL: cfg.BaseURL, Model: cfg.Model, Dimensions: cfg.Dimensions, APIKey: key,
			QueryPrefix: cfg.QueryPrefix, DocumentPrefix: cfg.DocumentPrefix,
		})
		if err != nil {
			return nil, "", err
		}
		return e, "", nil
	default:
		return nil, "", fmt.Errorf("embedding: provider %q is not published", cfg.Provider)
	}
}
