// SPDX-License-Identifier: Apache-2.0

package classify_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/catalog/classify"
	"github.com/Pierre-Theophile/aisre/pkg/attribution"
	"github.com/Pierre-Theophile/aisre/pkg/attribution/classifiertest"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// words embeds a text as a bag of hashed words: deterministic, no provider, and texts that share
// words point the same way.
type words struct{}

func (words) Model() knowledge.ModelID {
	return knowledge.ModelID{Provider: "test", Model: "words", Dimensions: 64}
}

func (words) Embed(ctx context.Context, texts []string, _ knowledge.Purpose) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 64)
		for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) {
			v[sha256.Sum256([]byte(w))[0]%64]++
		}
		out[i] = knowledge.Normalise(v)
	}
	return out, nil
}

var calibration = classify.Calibration{Slope: 8, Intercept: -4}

func TestTheEmbeddingClassifierConforms(t *testing.T) {
	t.Parallel()
	classifiertest.Run(t, func(*testing.T) attribution.Classifier { return classify.NewEmbedding(words{}, calibration) })
}

func TestTheClosestProfileScoresHighest(t *testing.T) {
	t.Parallel()
	scored, err := classify.NewEmbedding(words{}, calibration).Classify(context.Background(), classifiertest.Signal, classifiertest.Entries)
	if err != nil {
		t.Fatal(err)
	}
	best := scored[0]
	for _, s := range scored[1:] {
		if s.Score > best.Score {
			best = s
		}
	}
	if best.Slug != "candidate-view" {
		t.Errorf("best %+v of %+v", best, scored)
	}
}

func TestCalibration(t *testing.T) {
	t.Parallel()
	for cosine, want := range map[float64]float64{0.5: 0.5, 1: 0.982, 0: 0.018} {
		if got := calibration.Probability(cosine); got < want-0.001 || got > want+0.001 {
			t.Errorf("Probability(%v) = %v, want %v", cosine, got, want)
		}
	}
}
