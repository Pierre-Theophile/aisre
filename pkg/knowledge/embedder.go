// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"context"
	"fmt"
	"math"
)

// ModelID names an embedding model: who serves it, its name there, and its vector length.
type ModelID struct {
	Provider   string
	Model      string
	Dimensions int
}

// String renders the id as `provider:model@dimensions`, the form recorded in every digest.
func (m ModelID) String() string {
	if m.Provider == "" && m.Model == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s@%d", m.Provider, m.Model, m.Dimensions)
}

// IsZero reports whether no model is named.
func (m ModelID) IsZero() bool { return m == ModelID{} }

// Purpose says what a text is embedded for. Some models embed a query and a document differently.
type Purpose string

// The published purposes.
const (
	PurposeDocument Purpose = "document"
	PurposeQuery    Purpose = "query"
)

// Embedder turns text into vectors.
type Embedder interface {
	// Model names the model every vector comes from.
	Model() ModelID
	// Embed returns one L2-normalised vector per text. purpose is "document" or "query", for
	// providers that embed the two differently; others ignore it.
	Embed(ctx context.Context, texts []string, purpose Purpose) ([][]float32, error)
}

// Normalise scales v to unit length in place and returns it. A zero vector is returned unchanged.
func Normalise(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	norm := math.Sqrt(sum)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

// Cosine is the cosine similarity of two vectors of equal length, rounded to six decimals so a
// ranking is identical on every architecture. Vectors of different lengths score 0.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return Round6(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
