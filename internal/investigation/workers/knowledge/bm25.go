// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 over the graph-scoped set (tasks.md T049, research §7).
//
// The constitution already says the graph is the RAG index, and that is not a slogan: it is what
// reduces the candidate set from "every document the organisation has" to "documents attached to
// these twelve entities", which is tens of documents. Ranking tens of documents does not need a
// vector index; it needs a defensible ordering and a citation.
//
// So: BM25, in-repo, deterministic, no model, no embeddings, no second store, no index to
// rebuild. The published trigger for revisiting is a measurement rather than a preference — a
// graph-scoped candidate set routinely above 200 documents, or recall@10 on the knowledge
// fixtures below 0.9 — at which point embeddings earn their place and get their own ADR.
//
// Determinism matters as much as relevance here: a golden pins the order of the cited items, so
// ties are broken on the document id and every accumulation is rounded, exactly as the ranking
// formula and the onset estimator do.

// ScorerVersion is recorded in every knowledge digest. It moves when the ranking moves, which
// changes every golden that cites a document.
const ScorerVersion = "bm25/1.0.0"

// The published BM25 parameters. They are Robertson's own defaults, which is the right place to
// start and the wrong place to tune without a measurement.
const (
	// K1 controls term-frequency saturation.
	K1 = 1.2
	// B controls length normalisation.
	B = 0.75
	// ProximityBoost is how much a document linked closer to the focus is favoured, per hop
	// saved. Link proximity is a graph fact and therefore better evidence of relevance than
	// any amount of lexical overlap.
	ProximityBoost = 0.15
	// RecencyHalfLifeDays is the half-life of the recency factor. A three-year-old postmortem
	// about a service that has been rewritten twice is worth less than last month's, and the
	// digest carries the age so a reader can see why.
	RecencyHalfLifeDays = 365.0
)

// scored is one document with its score, before the digest is built.
type scored struct {
	document Document
	score    float64
}

// rank scores documents against the query terms and returns them best first. The corpus is the
// scoped set: BM25's inverse document frequency is computed over *it* and not over some larger
// collection, because "rare in the documents attached to these entities" is the signal that
// matters, and rarity in a corpus nobody retrieved from is not.
func rank(documents []Document, queryTerms []string) []scored {
	terms := tokenise(strings.Join(queryTerms, " "))
	if len(documents) == 0 {
		return nil
	}

	bodies := make([][]string, len(documents))
	var totalLength int
	for i, doc := range documents {
		bodies[i] = tokenise(doc.SearchableText())
		totalLength += len(bodies[i])
	}
	averageLength := round6(float64(totalLength) / float64(len(documents)))
	if averageLength == 0 {
		averageLength = 1
	}

	documentFrequency := make(map[string]int, len(terms))
	for _, term := range terms {
		for _, body := range bodies {
			if containsToken(body, term) {
				documentFrequency[term]++
			}
		}
	}

	out := make([]scored, 0, len(documents))
	for i, doc := range documents {
		var score float64
		for _, term := range terms {
			frequency := countToken(bodies[i], term)
			if frequency == 0 {
				continue
			}
			idf := inverseDocumentFrequency(len(documents), documentFrequency[term])
			numerator := round6(float64(frequency) * (K1 + 1))
			denominator := round6(float64(frequency) +
				round6(K1*round6(1-B+round6(B*round6(float64(len(bodies[i]))/averageLength)))))
			if denominator == 0 {
				continue
			}
			score = round6(score + round6(idf*round6(numerator/denominator)))
		}
		score = round6(score * proximityFactor(doc.HopsFromFocus))
		score = round6(score * recencyFactor(doc.AgeDays))
		out = append(out, scored{document: doc, score: score})
	}

	// Best first, then oldest-id-first on a tie, so the ordering is total and a golden holds.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].document.ID < out[j].document.ID
	})
	return out
}

// inverseDocumentFrequency is BM25's usual smoothed form, which stays positive for a term that
// appears in every document rather than going negative and inverting the ranking.
func inverseDocumentFrequency(documents, containing int) float64 {
	numerator := round6(float64(documents) - float64(containing) + 0.5)
	denominator := round6(float64(containing) + 0.5)
	return round6(math.Log(round6(1 + round6(numerator/denominator))))
}

// proximityFactor favours a document linked closer to the investigation's focus. A document
// attached to the focus itself outranks one attached three hops away, because the graph link is
// evidence of relevance and the word overlap is only a proxy for it.
func proximityFactor(hops int) float64 {
	if hops <= 0 {
		return 1
	}
	factor := round6(1 - round6(ProximityBoost*float64(hops)))
	if factor < 0.25 {
		return 0.25
	}
	return factor
}

// recencyFactor decays with the document's age at the published half-life. It never reaches
// zero: an old runbook is worth less, not nothing, and a retrieval that could never surface one
// would hide exactly the institutional memory this corpus exists to keep.
func recencyFactor(ageDays int64) float64 {
	if ageDays <= 0 {
		return 1
	}
	return round6(math.Pow(0.5, round6(float64(ageDays)/RecencyHalfLifeDays)))
}

// tokenise lowercases and splits on anything that is not a letter or a digit, keeping tokens of
// two characters or more. It is deliberately crude: no stemming, no stop-word list, nothing that
// would have to be versioned separately from the scorer and would silently change every golden
// when it moved.
func tokenise(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) >= 2 {
			out = append(out, field)
		}
	}
	return out
}

func containsToken(tokens []string, term string) bool {
	for _, token := range tokens {
		if token == term {
			return true
		}
	}
	return false
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

// round6 is the project's published rounding, applied at every accumulation step so the ranking
// is identical on every architecture.
func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}
