// SPDX-License-Identifier: Apache-2.0

// Package classify holds the shipped attribution classifiers (feature 009 research R6).
package classify

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/Pierre-Theophile/aisre/pkg/attribution"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// EmbeddingRule is the version of the embedding classifier's arithmetic: profile and signal
// texts, cosine, logistic calibration. A change to any of them is a new version.
const EmbeddingRule = "embedding/cosine@1"

// Calibration maps a cosine to a probability: 1 / (1 + e^-(Slope·cos + Intercept)). Its two
// parameters are fitted on the labelled set and published in the model configuration, because a
// cosine means something different for every embedding model.
type Calibration struct {
	Slope     float64 `yaml:"slope" json:"slope"`
	Intercept float64 `yaml:"intercept" json:"intercept"`
}

// Probability is the calibrated score of a cosine, rounded to four places as every attribution
// score is.
func (c Calibration) Probability(cosine float64) float64 {
	p := 1 / (1 + math.Exp(-(c.Slope*cosine + c.Intercept)))
	return math.Round(p*1e4) / 1e4
}

// Embedding scores a signal against each entry by the cosine of their embeddings, through any 008
// embedder: whoever configured an embedding provider for retrieval, hosted or self-hosted, has a
// classifier, and nothing new is depended on.
type Embedding struct {
	embedder    knowledge.Embedder
	calibration Calibration
}

var _ attribution.Classifier = (*Embedding)(nil)

// NewEmbedding returns the classifier over embedder with its published calibration.
func NewEmbedding(embedder knowledge.Embedder, calibration Calibration) *Embedding {
	return &Embedding{embedder: embedder, calibration: calibration}
}

// ID names the rule, the model and the calibration: changing any of them can move a score.
func (e *Embedding) ID() string {
	return fmt.Sprintf("%s %s slope=%g intercept=%g", EmbeddingRule, e.embedder.Model(), e.calibration.Slope, e.calibration.Intercept)
}

// Classify embeds the signal as a query and every profile as a document, in one call each.
func (e *Embedding) Classify(ctx context.Context, s attribution.Signal, entries []attribution.EntryProfile) ([]attribution.Scored, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	query, err := e.embedder.Embed(ctx, []string{SignalText(s)}, knowledge.PurposeQuery)
	if err != nil {
		return nil, fmt.Errorf("classify: embed the signal: %w", err)
	}
	texts := make([]string, len(entries))
	for i, entry := range entries {
		texts[i] = ProfileText(entry)
	}
	profiles, err := e.embedder.Embed(ctx, texts, knowledge.PurposeDocument)
	if err != nil {
		return nil, fmt.Errorf("classify: embed the profiles: %w", err)
	}
	if len(query) != 1 || len(profiles) != len(entries) {
		return nil, fmt.Errorf("classify: the embedder returned %d and %d vectors for 1 and %d texts", len(query), len(profiles), len(entries))
	}
	out := make([]attribution.Scored, 0, len(entries))
	for i, entry := range entries {
		cosine := knowledge.Cosine(query[0], profiles[i])
		out = append(out, attribution.Scored{
			Slug:      entry.Slug,
			Score:     e.calibration.Probability(cosine),
			Rationale: fmt.Sprintf("the signal reads like %s (cosine %.4f)", entry.DisplayName, cosine),
		})
	}
	return out, nil
}

// SignalText is what the classifier embeds for a signal: its text, then its names.
func SignalText(s attribution.Signal) string {
	lines := []string{strings.TrimSpace(s.Text)}
	for _, n := range s.Names {
		lines = append(lines, n.String())
	}
	return strings.Join(lines, "\n")
}

// ProfileText is what the classifier embeds for an entry: its name, description, aliases and the
// titles of the documents already attributed to it.
func ProfileText(p attribution.EntryProfile) string {
	lines := []string{p.DisplayName}
	if d := strings.TrimSpace(p.Description); d != "" {
		lines = append(lines, d)
	}
	for _, a := range p.Aliases {
		lines = append(lines, a.String())
	}
	lines = append(lines, p.DocumentTitles...)
	return strings.Join(lines, "\n")
}
