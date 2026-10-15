// SPDX-License-Identifier: Apache-2.0

package embedding_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/embedding"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// openAIServer answers like OpenAI's /v1/embeddings: one vector per input, [len(text), index, 1].
// fail answers the first n requests with status.
func openAIServer(t *testing.T, dims int, fail int32, status int, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) <= fail {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(status)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if seen != nil {
			*seen = append(*seen, map[string]any{"model": req.Model, "input": req.Input, "auth": r.Header.Get("Authorization")})
		}
		type datum struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []datum
		// Out of order on purpose: the adapter must place vectors by index.
		for i := len(req.Input) - 1; i >= 0; i-- {
			v := make([]float32, dims)
			v[0], v[1%dims] = float32(len(req.Input[i])), float32(i)
			v[dims-1] += 1
			data = append(data, datum{Object: "embedding", Index: i, Embedding: v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model})
	}))
}

func newAdapter(t *testing.T, url string, dims int, key string, slept *[]time.Duration) *embedding.OpenAICompatible {
	t.Helper()
	a, err := embedding.NewOpenAICompatible(embedding.OpenAICompatibleConfig{
		BaseURL: url + "/v1/", Model: "nomic-embed-text", Dimensions: dims, APIKey: key,
		QueryPrefix: "query: ", DocumentPrefix: "passage: ",
		Sleep: func(_ context.Context, d time.Duration) error {
			if slept != nil {
				*slept = append(*slept, d)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestOpenAICompatibleEmbedsInBatchesWithPrefixes(t *testing.T) {
	var seen []map[string]any
	srv := openAIServer(t, 4, 0, 0, &seen)
	defer srv.Close()
	a := newAdapter(t, srv.URL, 4, "secret-token", nil)

	if got := a.Model().String(); got != "openai_compatible:nomic-embed-text@4" {
		t.Errorf("model = %q", got)
	}
	texts := make([]string, 70)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	vectors, err := a.Embed(context.Background(), texts, knowledge.PurposeDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 70 || len(seen) != 3 {
		t.Fatalf("70 texts gave %d vectors in %d requests; want 70 in 3 (batches of %d)", len(vectors), len(seen), embedding.BatchSize)
	}
	first := seen[0]["input"].([]string)
	if len(first) != embedding.BatchSize || first[0] != "passage: x" {
		t.Errorf("first batch = %d texts starting %q; the document prefix is prepended", len(first), first[0])
	}
	if seen[0]["auth"] != "Bearer secret-token" || seen[0]["model"] != "nomic-embed-text" {
		t.Errorf("request = %v", seen[0])
	}
	for i, v := range vectors {
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if math.Abs(norm-1) > 1e-5 {
			t.Fatalf("vector %d is not L2-normalised: |v|² = %v", i, norm)
		}
	}
	// Placed by index, not by response order: text 9 is "passage: " and ten x, 19 characters,
	// and the server put that length in v[0] and 1 in v[3].
	if got := vectors[9][0] / vectors[9][3]; math.Abs(float64(got)-19) > 1e-3 {
		t.Errorf("vector 9 encodes length %v; want 19: vectors are not in input order", got)
	}

	if _, err := a.Embed(context.Background(), []string{"why"}, knowledge.PurposeQuery); err != nil {
		t.Fatal(err)
	}
	if got := seen[len(seen)-1]["input"].([]string)[0]; got != "query: why" {
		t.Errorf("query input = %q; the query prefix is prepended", got)
	}
}

func TestOpenAICompatibleRetriesHonouringRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		var slept []time.Duration
		srv := openAIServer(t, 3, 2, status, nil)
		a := newAdapter(t, srv.URL, 3, "", &slept)
		if _, err := a.Embed(context.Background(), []string{"a"}, knowledge.PurposeDocument); err != nil {
			t.Fatalf("%d twice then 200: %v", status, err)
		}
		if len(slept) != 2 || slept[0] != 2*time.Second {
			t.Errorf("%d: slept %v; want twice the server's Retry-After of 2s", status, slept)
		}
		srv.Close()
	}

	srv := openAIServer(t, 3, 100, http.StatusBadGateway, nil)
	defer srv.Close()
	a := newAdapter(t, srv.URL, 3, "", nil)
	if _, err := a.Embed(context.Background(), []string{"a"}, knowledge.PurposeQuery); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("a server that keeps failing: %v", err)
	}
}

func TestOpenAICompatibleRefusesWhatItCannotUse(t *testing.T) {
	srv := openAIServer(t, 5, 0, 0, nil)
	defer srv.Close()
	a := newAdapter(t, srv.URL, 4, "", nil) // the server returns 5 dimensions
	if _, err := a.Embed(context.Background(), []string{"a"}, knowledge.PurposeDocument); !errors.Is(err, knowledge.ErrDimensionMismatch) {
		t.Errorf("a response of another length: %v; want ErrDimensionMismatch", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"echoing the input: SECRET SECTION TEXT"}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	var slept []time.Duration
	b := newAdapter(t, bad.URL, 4, "", &slept)
	_, err := b.Embed(context.Background(), []string{"a"}, knowledge.PurposeDocument)
	if err == nil || strings.Contains(err.Error(), "SECRET") || len(slept) != 0 {
		t.Errorf("a 400 is not retried and its body, which may echo section text, is not quoted: %v (slept %v)", err, slept)
	}
	if _, err := embedding.NewOpenAICompatible(embedding.OpenAICompatibleConfig{BaseURL: "http://x"}); err == nil {
		t.Error("an adapter with no model and no dimensions was built")
	}
}
