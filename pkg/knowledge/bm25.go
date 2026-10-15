// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"math"
	"strings"
	"unicode"
)

// BM25, the lexical ranking the knowledge worker orders sections by and the memory store searches
// with (ADR-0014 item 4).
//
// In-repo, deterministic, no model: every accumulation is rounded to six decimals so a ranking is
// identical on every architecture, and the tokeniser is deliberately crude (no stemming, no
// stop-word list) so nothing about it has to be versioned separately from the scorer. Moved here
// from the knowledge worker by feature 008 so the store and the worker rank with one function.

// The published BM25 parameters: Robertson's defaults.
const (
	// K1 controls term-frequency saturation.
	K1 = 1.2
	// B controls length normalisation.
	B = 0.75
)

// BM25 scores each text against query, in the order given. Inverse document frequency is computed
// over texts themselves, so "rare among these candidates" is the signal. A text sharing no term
// with the query scores 0.
func BM25(query string, texts []string) []float64 {
	terms := uniqueTokens(Tokenise(query))
	scores := make([]float64, len(texts))
	if len(texts) == 0 || len(terms) == 0 {
		return scores
	}

	bodies := make([][]string, len(texts))
	var totalLength int
	for i, text := range texts {
		bodies[i] = Tokenise(text)
		totalLength += len(bodies[i])
	}
	averageLength := Round6(float64(totalLength) / float64(len(texts)))
	if averageLength == 0 {
		averageLength = 1
	}

	documentFrequency := make(map[string]int, len(terms))
	for _, term := range terms {
		for _, body := range bodies {
			if countToken(body, term) > 0 {
				documentFrequency[term]++
			}
		}
	}

	for i := range texts {
		var score float64
		for _, term := range terms {
			frequency := countToken(bodies[i], term)
			if frequency == 0 {
				continue
			}
			idf := inverseDocumentFrequency(len(texts), documentFrequency[term])
			numerator := Round6(float64(frequency) * (K1 + 1))
			denominator := Round6(float64(frequency) +
				Round6(K1*Round6(1-B+Round6(B*Round6(float64(len(bodies[i]))/averageLength)))))
			if denominator == 0 {
				continue
			}
			score = Round6(score + Round6(idf*Round6(numerator/denominator)))
		}
		scores[i] = score
	}
	return scores
}

// inverseDocumentFrequency is BM25's smoothed form, which stays positive for a term present in
// every text rather than going negative and inverting the ranking.
func inverseDocumentFrequency(texts, containing int) float64 {
	numerator := Round6(float64(texts) - float64(containing) + 0.5)
	denominator := Round6(float64(containing) + 0.5)
	return Round6(math.Log(Round6(1 + Round6(numerator/denominator))))
}

// Tokenise lowercases and splits on anything that is not a letter or a digit, keeping tokens of
// two characters or more.
func Tokenise(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len([]rune(field)) >= 2 {
			out = append(out, field)
		}
	}
	return out
}

func uniqueTokens(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func countToken(tokens []string, term string) int {
	var n int
	for _, token := range tokens {
		if token == term {
			n++
		}
	}
	return n
}

// Round6 is the project's published rounding, applied at every accumulation step.
func Round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}
