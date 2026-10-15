// SPDX-License-Identifier: Apache-2.0

package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The openai_compatible adapter (feature 008 FR-029, research R5).
//
// One wire format covers every server the owner might choose: Mistral, OpenAI, Ollama's
// `/v1/embeddings`, vLLM, Hugging Face TEI, LocalAI and llama.cpp's server all accept
// `{"model": …, "input": [texts]}` and answer `{"data": [{"index": i, "embedding": [floats]}]}`.
// A response of another length than the configured dimensions is refused, and every vector is
// L2-normalised here, so a store's cosine is a dot product whatever the server returned.

// The adapter's published constants.
const (
	// BatchSize is how many texts one request carries.
	BatchSize = 32
	// QueryTimeout bounds the embedding of a search query: a search waits this long at most before
	// it runs lexical only (FR-027).
	QueryTimeout = 10 * time.Second
	// DocumentTimeout bounds one batch of section texts; the indexer retries a batch that fails.
	DocumentTimeout = 60 * time.Second
	// MaxAttempts is how many times one request is tried when the server answers 429 or 5xx.
	MaxAttempts = 4
	// maxRetryAfter caps a server's Retry-After, so a misbehaving server cannot park a search.
	maxRetryAfter = 30 * time.Second
)

// OpenAICompatibleConfig is what the adapter needs; the model configuration's `embedding:` block
// carries it.
type OpenAICompatibleConfig struct {
	// BaseURL is the API root; requests go to BaseURL + "/embeddings".
	BaseURL string
	// Model is the model name the server expects.
	Model string
	// Dimensions is the vector length every response must have.
	Dimensions int
	// APIKey is sent as a bearer token when set.
	APIKey string
	// QueryPrefix and DocumentPrefix are prepended per purpose.
	QueryPrefix    string
	DocumentPrefix string
	// HTTPClient defaults to a client with no overall timeout (each request has its own).
	HTTPClient *http.Client
	// Sleep defaults to a context-aware time.Sleep; tests replace it.
	Sleep func(context.Context, time.Duration) error
}

// OpenAICompatible embeds through any server speaking the OpenAI embeddings API.
type OpenAICompatible struct {
	cfg OpenAICompatibleConfig
}

var _ knowledge.Embedder = (*OpenAICompatible)(nil)

// ProviderOpenAICompatible is the provider half of every ModelID this adapter produces.
const ProviderOpenAICompatible = "openai_compatible"

// NewOpenAICompatible returns the adapter.
func NewOpenAICompatible(cfg OpenAICompatibleConfig) (*OpenAICompatible, error) {
	if cfg.BaseURL == "" || cfg.Model == "" || cfg.Dimensions <= 0 {
		return nil, errors.New("embedding: openai_compatible needs a base_url, a model and positive dimensions")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	return &OpenAICompatible{cfg: cfg}, nil
}

// Model names the model every vector comes from.
func (o *OpenAICompatible) Model() knowledge.ModelID {
	return knowledge.ModelID{Provider: ProviderOpenAICompatible, Model: o.cfg.Model, Dimensions: o.cfg.Dimensions}
}

// Embed returns one L2-normalised vector per text, in order.
func (o *OpenAICompatible) Embed(ctx context.Context, texts []string, purpose knowledge.Purpose) ([][]float32, error) {
	prefix, timeout := o.cfg.DocumentPrefix, DocumentTimeout
	if purpose == knowledge.PurposeQuery {
		prefix, timeout = o.cfg.QueryPrefix, QueryTimeout
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += BatchSize {
		end := min(start+BatchSize, len(texts))
		batch := make([]string, 0, end-start)
		for _, text := range texts[start:end] {
			batch = append(batch, prefix+text)
		}
		vectors, err := o.embedBatch(ctx, batch, timeout)
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (o *OpenAICompatible) embedBatch(ctx context.Context, batch []string, timeout time.Duration) ([][]float32, error) {
	body, err := json.Marshal(embeddingsRequest{Model: o.cfg.Model, Input: batch})
	if err != nil {
		return nil, fmt.Errorf("embedding: encode: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		vectors, retryAfter, err := o.post(ctx, body, len(batch), timeout)
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if retryAfter < 0 || attempt == MaxAttempts {
			break
		}
		if err := o.cfg.Sleep(ctx, retryAfter); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// post makes one request. retryAfter is negative when the failure is not worth retrying.
func (o *OpenAICompatible) post(ctx context.Context, body []byte, n int, timeout time.Duration) ([][]float32, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, -1, fmt.Errorf("embedding: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if o.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	resp, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, fmt.Errorf("embedding: %s: %w", o.Model(), ctx.Err())
		}
		// A connection that failed outright is retried like a 503: a self-hosted server
		// restarting is the common case.
		return nil, time.Second, fmt.Errorf("embedding: %s: %w", o.Model(), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, time.Second, fmt.Errorf("embedding: %s: read: %w", o.Model(), err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, retryAfter(resp.Header.Get("Retry-After")),
			fmt.Errorf("embedding: %s: server answered %d", o.Model(), resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not quoted: a server's error text can echo the input, which is section text.
		return nil, -1, fmt.Errorf("embedding: %s: server answered %d", o.Model(), resp.StatusCode)
	}
	var decoded embeddingsResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, -1, fmt.Errorf("embedding: %s: decode the response: %w", o.Model(), err)
	}
	if len(decoded.Data) != n {
		return nil, -1, fmt.Errorf("embedding: %s: asked for %d vectors, got %d", o.Model(), n, len(decoded.Data))
	}
	out := make([][]float32, n)
	for _, d := range decoded.Data {
		if d.Index < 0 || d.Index >= n || out[d.Index] != nil {
			return nil, -1, fmt.Errorf("embedding: %s: response index %d is out of range or repeated", o.Model(), d.Index)
		}
		if len(d.Embedding) != o.cfg.Dimensions {
			return nil, -1, fmt.Errorf("embedding: %s: %w: got %d", o.Model(), knowledge.ErrDimensionMismatch, len(d.Embedding))
		}
		out[d.Index] = knowledge.Normalise(d.Embedding)
	}
	return out, 0, nil
}

// retryAfter reads a Retry-After header in seconds, defaulting to one second and capped.
func retryAfter(header string) time.Duration {
	wait := time.Second
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds >= 0 {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil {
		wait = time.Until(at)
	}
	if wait < 0 {
		wait = 0
	}
	return min(wait, maxRetryAfter)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
