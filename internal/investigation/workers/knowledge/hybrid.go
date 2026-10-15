// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"sort"

	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The scorer `knowledge/hybrid@1` (008 research R4, ADR-0014 item 4).
//
// Sections are ranked, not documents: one lexical ranking and, when an embedder is configured, one
// semantic ranking. The two are fused by reciprocal rank fusion, which needs no calibration between
// a BM25 score and a cosine — they differ per store and per model, and a weighted sum of them would
// have to be re-tuned for each. A document scores its best section, so one long postmortem cannot
// fill the result list, and a document linked to an entity inside the request's scope is boosted:
// the graph link is evidence of relevance that no amount of word overlap is. Linked elsewhere, a
// document is still eligible, which is the whole point of 008 — the same failure on another service
// is exactly the postmortem an on-call engineer wants to read.
//
// Determinism matters as much as relevance: a golden pins the order of the cited items, so every
// accumulation is rounded and every tie is broken on the document reference, the section label and
// the part, in that order.

// ScorerVersion is recorded in every knowledge digest. A change to any constant below is a new
// version, which changes every golden that cites a document.
const ScorerVersion = "knowledge/hybrid@1"

// The published constants of knowledge/hybrid@1.
const (
	// RRFK is reciprocal rank fusion's k: a section at rank r in a ranking contributes 1/(k+r).
	RRFK = 60
	// RankingDepth is how many sections each ranking contributes.
	RankingDepth = 50
	// ScopeBoost multiplies the score of a document linked to an entity in the request's scope.
	ScopeBoost = 1.5
	// CarriedSections is how many of its best sections a document carries.
	CarriedSections = 2
)

// The retrieval modes a digest reports (008 FR-027).
const (
	ModeHybrid      = "hybrid"
	ModeLexicalOnly = "lexical_only"
)

// sectionKey identifies a section within a search: a document, a label, a part.
type sectionKey struct {
	document string
	label    string
	part     int
}

func (a sectionKey) less(b sectionKey) bool {
	if a.document != b.document {
		return a.document < b.document
	}
	if a.label != b.label {
		return a.label < b.label
	}
	return a.part < b.part
}

// rankedSection is one entry of a ranking, best first. Its position in the slice is its rank.
type rankedSection struct {
	key    sectionKey
	text   string
	origin kstore.Origin
}

// matchedSection is a section with its fused score.
type matchedSection struct {
	rankedSection
	score float64
}

// fusedDocument is one document's place in the fused ranking.
type fusedDocument struct {
	ref      string
	score    float64
	sections []matchedSection // best first, at most CarriedSections
}

// fuse applies knowledge/hybrid@1 to rankings and returns the matched documents, best first.
// inScope says which documents are linked to an entity in the request's scope. A ranking deeper
// than RankingDepth is cut to it.
func fuse(rankings [][]rankedSection, inScope map[string]bool) []fusedDocument {
	scores := map[sectionKey]*matchedSection{}
	for _, ranking := range rankings {
		if len(ranking) > RankingDepth {
			ranking = ranking[:RankingDepth]
		}
		for i, s := range ranking {
			contribution := kstore.Round6(1 / float64(RRFK+i+1))
			entry, ok := scores[s.key]
			if !ok {
				entry = &matchedSection{rankedSection: s}
				scores[s.key] = entry
			}
			entry.score = kstore.Round6(entry.score + contribution)
		}
	}

	byDocument := map[string][]matchedSection{}
	for _, entry := range scores {
		byDocument[entry.key.document] = append(byDocument[entry.key.document], *entry)
	}
	out := make([]fusedDocument, 0, len(byDocument))
	for ref, sections := range byDocument {
		sort.Slice(sections, func(i, j int) bool {
			if sections[i].score != sections[j].score {
				return sections[i].score > sections[j].score
			}
			return sections[i].key.less(sections[j].key)
		})
		if len(sections) > CarriedSections {
			sections = sections[:CarriedSections]
		}
		score := sections[0].score
		if inScope[ref] {
			score = kstore.Round6(score * ScopeBoost)
		}
		out = append(out, fusedDocument{ref: ref, score: score, sections: sections})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if out[i].ref != out[j].ref {
			return out[i].ref < out[j].ref
		}
		return out[i].sections[0].key.less(out[j].sections[0].key)
	})
	return out
}

// lexicalRanking orders sections by BM25 against query, over their label and text, keeping only
// sections that share a term with it (ADR-0014 item 4.3). The store selected the stored ones; the
// order is the worker's, so two conformant stores give the same ranking. A stored section with the
// key of a synthesised one replaces it.
func lexicalRanking(query string, synthesised []rankedSection, stored []kstore.Hit) []rankedSection {
	byKey := map[sectionKey]rankedSection{}
	for _, s := range synthesised {
		byKey[s.key] = s
	}
	for _, hit := range stored {
		key := sectionKey{document: hit.DocumentRef, label: hit.Label, part: hit.Part}
		byKey[key] = rankedSection{key: key, text: hit.Text, origin: hit.Origin}
	}
	sections := make([]rankedSection, 0, len(byKey))
	for _, s := range byKey {
		sections = append(sections, s)
	}
	sort.Slice(sections, func(i, j int) bool { return sections[i].key.less(sections[j].key) })

	texts := make([]string, len(sections))
	for i, s := range sections {
		texts[i] = s.key.label + " " + s.text
	}
	scores := kstore.BM25(query, texts)

	type scoredSection struct {
		section rankedSection
		score   float64
	}
	matched := make([]scoredSection, 0, len(sections))
	for i, s := range sections {
		if scores[i] > 0 {
			matched = append(matched, scoredSection{section: s, score: scores[i]})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].score > matched[j].score })
	out := make([]rankedSection, 0, len(matched))
	for _, m := range matched {
		out = append(out, m.section)
	}
	return out
}
