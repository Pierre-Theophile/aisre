// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"testing"

	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// knowledge/hybrid@1 against hand-computed ranks (008 T007, research R4).

func sec(doc, label string) rankedSection {
	return rankedSection{key: sectionKey{document: doc, label: label}, text: label}
}

func TestHybridFusesByReciprocalRank(t *testing.T) {
	t.Parallel()
	lexical := []rankedSection{sec("a", "Cause"), sec("b", "Impact"), sec("a", "Timeline")}
	semantic := []rankedSection{sec("b", "Impact"), sec("c", "Body")}
	got := fuse([][]rankedSection{lexical, semantic}, map[string]bool{})

	// b/Impact: 1/62 + 1/61 = 0.016129 + 0.016393 = 0.032522
	// a/Cause:  1/61                                = 0.016393
	// c/Body:   1/62                                = 0.016129
	want := []struct {
		ref   string
		score float64
		best  string
	}{{"b", 0.032522, "Impact"}, {"a", 0.016393, "Cause"}, {"c", 0.016129, "Body"}}
	if len(got) != len(want) {
		t.Fatalf("fused %d documents, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ref != w.ref || got[i].score != w.score || got[i].sections[0].key.label != w.best {
			t.Errorf("rank %d = %s %.6f %s, want %s %.6f %s", i+1, got[i].ref, got[i].score, got[i].sections[0].key.label, w.ref, w.score, w.best)
		}
	}
	// a carries its two sections, best first: Cause (rank 1) then Timeline (rank 3, 1/63).
	if len(got[1].sections) != 2 || got[1].sections[1].key.label != "Timeline" || got[1].sections[1].score != 0.015873 {
		t.Errorf("a carries %+v, want Cause then Timeline at 0.015873", got[1].sections)
	}
}

func TestHybridBoostsScope(t *testing.T) {
	t.Parallel()
	// b is second in the one ranking but in scope: 1/62 × 1.5 = 0.024194 beats 1/61 = 0.016393.
	got := fuse([][]rankedSection{{sec("a", "Cause"), sec("b", "Cause")}}, map[string]bool{"b": true})
	if got[0].ref != "b" || got[0].score != 0.024194 || got[1].score != 0.016393 {
		t.Errorf("fused %s %.6f, %s %.6f; want b 0.024194 then a 0.016393", got[0].ref, got[0].score, got[1].ref, got[1].score)
	}
}

func TestHybridCarriesAtMostTwoSectionsAndCutsRankingsAtDepth(t *testing.T) {
	t.Parallel()
	long := make([]rankedSection, 0, RankingDepth+5)
	for i := range RankingDepth + 5 {
		long = append(long, sec("doc", string(rune('A'+i%26))+string(rune('a'+i/26))))
	}
	got := fuse([][]rankedSection{long}, nil)
	if len(got) != 1 || len(got[0].sections) != CarriedSections {
		t.Fatalf("fused %+v, want one document carrying %d sections", got, CarriedSections)
	}
	if got[0].score != kstore.Round6(1.0/61) {
		t.Errorf("score %v, want its best section's", got[0].score)
	}
}

func TestHybridTiesAreDeterministic(t *testing.T) {
	t.Parallel()
	// Two rankings that place a and b symmetrically: equal scores, so the document ref decides.
	got := fuse([][]rankedSection{{sec("b", "X"), sec("a", "X")}, {sec("a", "X"), sec("b", "X")}}, nil)
	if got[0].ref != "a" || got[0].score != got[1].score {
		t.Errorf("tie broken to %s (%v vs %v), want a by document ref", got[0].ref, got[0].score, got[1].score)
	}
}

func TestLexicalRankingPrefersAStoredSectionOverTheSynthesisedOneWithItsKey(t *testing.T) {
	t.Parallel()
	synthesised := []rankedSection{{key: sectionKey{document: "a", label: "Summary"}, text: "nothing relevant"}}
	stored := []kstore.Hit{{DocumentRef: "a", Label: "Summary", Text: "connection pool exhaustion", Origin: kstore.OriginProperty}}
	got := lexicalRanking("pool", synthesised, stored)
	if len(got) != 1 || got[0].text != "connection pool exhaustion" || got[0].origin != kstore.OriginProperty {
		t.Errorf("ranking = %+v, want the stored section alone", got)
	}
}
