// SPDX-License-Identifier: Apache-2.0

// Package storetest is the conformance suite of knowledge.Store (contracts/knowledge-store.md §1,
// 008 FR-007). A store is conformant when Run passes against it unmodified; both shipped stores
// run it in CI, and a third-party store (pgvector, a vector database, anything) proves itself the
// same way.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// NewStore returns an empty store. Run calls it once per subtest, so each sees a fresh store.
type NewStore func(t *testing.T) knowledge.Store

// The instants every subtest uses: three versions apart, so a read before, between and after each
// is unambiguous.
var (
	t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

var model = knowledge.ModelID{Provider: "test", Model: "fixed", Dimensions: 3}

// Run runs the conformance suite against stores made by newStore.
func Run(t *testing.T, newStore NewStore) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s knowledge.Store)
	}{
		{"PutOpensVersions", putOpensVersions},
		{"PutIsIdempotent", putIsIdempotent},
		{"PutReplacesAndCloses", putReplacesAndCloses},
		{"PutRefusesInvalidSections", putRefusesInvalidSections},
		{"AsOfReads", asOfReads},
		{"DocumentsFilter", documentsFilter},
		{"LexicalFindsTheExactPhraseFirst", lexicalFindsTheExactPhraseFirst},
		{"LexicalReturnsOnlyMatches", lexicalReturnsOnlyMatches},
		{"LexicalTiesAreDeterministic", lexicalTiesAreDeterministic},
		{"LexicalHonoursK", lexicalHonoursK},
		{"SemanticOrdersByCosine", semanticOrdersByCosine},
		{"SemanticIsAsOf", semanticIsAsOf},
		{"DimensionMismatchIsRefused", dimensionMismatchIsRefused},
		{"PendingAndPutVectors", pendingAndPutVectors},
		{"ConcurrentPutsToDifferentDocuments", concurrentPutsToDifferentDocuments},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t))
		})
	}
}

func put(t *testing.T, s knowledge.Store, doc string, at time.Time, sections ...knowledge.Section) knowledge.PutResult {
	t.Helper()
	result, err := s.Put(context.Background(), doc, sections, at)
	if err != nil {
		t.Fatalf("put %s at %s: %v", doc, at.Format(time.RFC3339), err)
	}
	return result
}

func section(label, text string) knowledge.Section {
	return knowledge.Section{Label: label, Text: text, Origin: knowledge.OriginBody}
}

func lexical(t *testing.T, s knowledge.Store, query string, docs []string, at time.Time, k int) []knowledge.Hit {
	t.Helper()
	hits, err := s.Lexical(context.Background(), query, docs, at, k)
	if err != nil {
		t.Fatalf("lexical %q: %v", query, err)
	}
	return hits
}

func render(hits []knowledge.Hit) string {
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, fmt.Sprintf("%d:%s/%s#%d", h.Rank, h.DocumentRef, h.Label, h.Part))
	}
	return strings.Join(parts, " ")
}

func putOpensVersions(t *testing.T, s knowledge.Store) {
	got := put(t, s, "doc=a", t0, section("Impact", "checkout returned 5xx"), section("Cause", "pool exhaustion"))
	if got != (knowledge.PutResult{Opened: 2}) {
		t.Errorf("first put = %+v, want 2 opened", got)
	}
	hits := lexical(t, s, "pool exhaustion", []string{"doc=a"}, t0, 10)
	if len(hits) != 1 || hits[0].Label != "Cause" || hits[0].Text != "pool exhaustion" {
		t.Fatalf("hits = %s, want doc=a/Cause", render(hits))
	}
	want := section("Cause", "pool exhaustion")
	if hits[0].Digest != want.Digest() || hits[0].Origin != knowledge.OriginBody || hits[0].Rank != 1 {
		t.Errorf("hit = %+v, want digest %s, origin body, rank 1", hits[0], want.Digest())
	}
}

func putIsIdempotent(t *testing.T, s knowledge.Store) {
	put(t, s, "doc=a", t0, section("Impact", "checkout returned 5xx"))
	got := put(t, s, "doc=a", t1, section("Impact", "checkout returned 5xx"))
	if got != (knowledge.PutResult{Unchanged: 1}) {
		t.Errorf("repeated put = %+v, want 1 unchanged", got)
	}
	// Unchanged means the version known from t0 is still the one: it is visible before t1.
	if hits := lexical(t, s, "checkout", []string{"doc=a"}, t0, 10); len(hits) != 1 {
		t.Errorf("after an idempotent put, the version known at t0 is gone: %s", render(hits))
	}
}

func putReplacesAndCloses(t *testing.T, s knowledge.Store) {
	put(t, s, "doc=a", t0, section("Impact", "checkout returned 5xx"), section("Cause", "pool exhaustion"))
	got := put(t, s, "doc=a", t1, section("Impact", "checkout returned 502 errors"))
	if got != (knowledge.PutResult{Opened: 1, Closed: 2}) {
		t.Errorf("replacing put = %+v, want 1 opened and 2 closed (one changed, one dropped)", got)
	}
	if hits := lexical(t, s, "pool", []string{"doc=a"}, t1, 10); len(hits) != 0 {
		t.Errorf("a section dropped at t1 still matches at t1: %s", render(hits))
	}
	if hits := lexical(t, s, "pool", []string{"doc=a"}, t0, 10); len(hits) != 1 {
		t.Errorf("a section dropped at t1 no longer matches at t0: %s", render(hits))
	}
}

func putRefusesInvalidSections(t *testing.T, s knowledge.Store) {
	tooMany := make([]knowledge.Section, knowledge.MaxSections+1)
	for i := range tooMany {
		tooMany[i] = section(fmt.Sprintf("s%d", i), "text")
	}
	for name, sections := range map[string][]knowledge.Section{
		"empty label":   {section("", "text")},
		"empty text":    {section("Impact", "  ")},
		"over-long":     {section("Impact", strings.Repeat("x", knowledge.MaxSectionChars+1))},
		"duplicate key": {section("Impact", "a"), section("Impact", "b")},
		"too many":      tooMany,
	} {
		if _, err := s.Put(context.Background(), "doc=a", sections, t0); !errors.Is(err, knowledge.ErrInvalidSections) {
			t.Errorf("%s: err = %v, want ErrInvalidSections", name, err)
		}
	}
	if hits := lexical(t, s, "text", []string{"doc=a"}, t1, 10); len(hits) != 0 {
		t.Errorf("a refused put left sections behind: %s", render(hits))
	}
}

func asOfReads(t *testing.T, s knowledge.Store) {
	put(t, s, "doc=a", t1, section("Cause", "connection pool exhaustion"))
	put(t, s, "doc=a", t2, section("Cause", "a bad index on orders"))

	if hits := lexical(t, s, "pool", []string{"doc=a"}, t0, 10); len(hits) != 0 {
		t.Errorf("before the first version, matched %s", render(hits))
	}
	if hits := lexical(t, s, "pool", []string{"doc=a"}, t1, 10); len(hits) != 1 {
		t.Errorf("at the first version's instant, matched %s, want it", render(hits))
	}
	if hits := lexical(t, s, "pool", []string{"doc=a"}, t1.Add(30*time.Minute), 10); len(hits) != 1 {
		t.Errorf("between the versions, matched %s, want the first", render(hits))
	}
	if hits := lexical(t, s, "pool", []string{"doc=a"}, t2, 10); len(hits) != 0 {
		t.Errorf("after the rewrite, the earlier text still matched: %s", render(hits))
	}
	if hits := lexical(t, s, "index", []string{"doc=a"}, t2, 10); len(hits) != 1 {
		t.Errorf("after the rewrite, the new text did not match: %s", render(hits))
	}
	if hits := lexical(t, s, "index", []string{"doc=a"}, t1, 10); len(hits) != 0 {
		t.Errorf("before the rewrite, the later text matched: %s", render(hits))
	}
}

func documentsFilter(t *testing.T, s knowledge.Store) {
	put(t, s, "doc=a", t0, section("Cause", "pool exhaustion"))
	put(t, s, "doc=b", t0, section("Cause", "pool exhaustion"))
	if hits := lexical(t, s, "pool", []string{"doc=b"}, t1, 10); len(hits) != 1 || hits[0].DocumentRef != "doc=b" {
		t.Errorf("filtered to doc=b, got %s", render(hits))
	}
	if hits := lexical(t, s, "pool", nil, t1, 10); len(hits) != 0 {
		t.Errorf("no documents named, got %s; the graph names the candidates, the store never widens them", render(hits))
	}
}

func lexicalFindsTheExactPhraseFirst(t *testing.T, s knowledge.Store) {
	put(t, s, "doc=a", t0,
		section("Timeline", "the on-call engineer opened the dashboard and paged the payments team"),
		section("Cause", "connection pool exhaustion in payments after the rollout"))
	put(t, s, "doc=b", t0,
		section("Impact", "the reporting job was slow; nobody was paged"))
	hits := lexical(t, s, "connection pool exhaustion", []string{"doc=a", "doc=b"}, t1, 10)
	if len(hits) == 0 || hits[0].DocumentRef != "doc=a" || hits[0].Label != "Cause" {
		t.Errorf("hits = %s, want doc=a/Cause first", render(hits))
	}
}

func lexicalReturnsOnlyMatches(t *testing.T, s knowledge.Store) {
	put(t, s, "doc=a", t0, section("Cause", "pool exhaustion"), section("Impact", "slow checkout"))
	hits := lexical(t, s, "exhaustion", []string{"doc=a"}, t1, 10)
	if len(hits) != 1 || hits[0].Label != "Cause" {
		t.Errorf("hits = %s, want only the matching section", render(hits))
	}
	if hits := lexical(t, s, "", []string{"doc=a"}, t1, 10); len(hits) != 0 {
		t.Errorf("an empty query matched %s", render(hits))
	}
}

func lexicalTiesAreDeterministic(t *testing.T, s knowledge.Store) {
	// Identical texts score identically, so the order is the tie-break alone.
	put(t, s, "doc=b", t0, section("Cause", "pool exhaustion"))
	put(t, s, "doc=a", t0, section("Zeta", "pool exhaustion"), section("Alpha", "pool exhaustion"))
	want := "1:doc=a/Alpha#0 2:doc=a/Zeta#0 3:doc=b/Cause#0"
	for range 3 {
		if got := render(lexical(t, s, "pool exhaustion", []string{"doc=a", "doc=b"}, t1, 10)); got != want {
			t.Fatalf("tied hits = %s, want %s (document ref, then label, then part)", got, want)
		}
	}
}

func lexicalHonoursK(t *testing.T, s knowledge.Store) {
	for i := range 5 {
		put(t, s, fmt.Sprintf("doc=%d", i), t0, section("Cause", "pool exhaustion"))
	}
	docs := []string{"doc=0", "doc=1", "doc=2", "doc=3", "doc=4"}
	if hits := lexical(t, s, "pool", docs, t1, 2); len(hits) != 2 || hits[1].Rank != 2 {
		t.Errorf("k=2 returned %s", render(hits))
	}
}

func vectors(t *testing.T, s knowledge.Store, pairs map[knowledge.Section][]float32) {
	t.Helper()
	var dvs []knowledge.DigestVector
	for sec, v := range pairs {
		dvs = append(dvs, knowledge.DigestVector{Digest: sec.Digest(), Vector: knowledge.Normalise(append([]float32(nil), v...))})
	}
	if err := s.PutVectors(context.Background(), model, dvs); err != nil {
		t.Fatalf("put vectors: %v", err)
	}
}

func semanticOrdersByCosine(t *testing.T, s knowledge.Store) {
	near := section("Cause", "pool exhaustion")
	mid := section("Impact", "slow checkout")
	far := section("Timeline", "paged at noon")
	put(t, s, "doc=a", t0, near, mid, far)
	vectors(t, s, map[knowledge.Section][]float32{near: {1, 0, 0}, mid: {1, 1, 0}, far: {0, 0, 1}})

	hits, err := s.Semantic(context.Background(), []float32{1, 0, 0}, model, []string{"doc=a"}, t1, 10)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if got, want := render(hits), "1:doc=a/Cause#0 2:doc=a/Impact#0 3:doc=a/Timeline#0"; got != want {
		t.Errorf("semantic order = %s, want %s", got, want)
	}
	other := knowledge.ModelID{Provider: "test", Model: "other", Dimensions: 3}
	if hits, err := s.Semantic(context.Background(), []float32{1, 0, 0}, other, []string{"doc=a"}, t1, 10); err != nil || len(hits) != 0 {
		t.Errorf("another model's search returned %s, %v; vectors are per model", render(hits), err)
	}
}

func semanticIsAsOf(t *testing.T, s knowledge.Store) {
	before := section("Cause", "pool exhaustion")
	after := section("Cause", "bad index")
	put(t, s, "doc=a", t1, before)
	put(t, s, "doc=a", t2, after)
	vectors(t, s, map[knowledge.Section][]float32{before: {1, 0, 0}, after: {0, 1, 0}})

	at := func(when time.Time) string {
		hits, err := s.Semantic(context.Background(), []float32{0, 1, 0}, model, []string{"doc=a"}, when, 10)
		if err != nil {
			t.Fatalf("semantic: %v", err)
		}
		texts := make([]string, 0, len(hits))
		for _, h := range hits {
			texts = append(texts, h.Text)
		}
		return strings.Join(texts, "|")
	}
	if got := at(t0); got != "" {
		t.Errorf("before any version: %q", got)
	}
	if got := at(t1); got != "pool exhaustion" {
		t.Errorf("between versions: %q, want the first text only", got)
	}
	if got := at(t2); got != "bad index" {
		t.Errorf("after the rewrite: %q, want the second text only", got)
	}
}

func dimensionMismatchIsRefused(t *testing.T, s knowledge.Store) {
	sec := section("Cause", "pool exhaustion")
	put(t, s, "doc=a", t0, sec)
	err := s.PutVectors(context.Background(), model, []knowledge.DigestVector{{Digest: sec.Digest(), Vector: []float32{1, 0}}})
	if !errors.Is(err, knowledge.ErrDimensionMismatch) {
		t.Errorf("put of a 2-vector for a 3-model: err = %v, want ErrDimensionMismatch", err)
	}
	_, err = s.Semantic(context.Background(), []float32{1, 0}, model, []string{"doc=a"}, t1, 10)
	if !errors.Is(err, knowledge.ErrDimensionMismatch) {
		t.Errorf("search with a 2-vector for a 3-model: err = %v, want ErrDimensionMismatch", err)
	}
}

func pendingAndPutVectors(t *testing.T, s knowledge.Store) {
	first := section("Cause", "pool exhaustion")
	second := section("Impact", "slow checkout")
	put(t, s, "doc=a", t0, first)
	put(t, s, "doc=a", t1, first, second)
	// The same text in another document shares the digest and is embedded once.
	put(t, s, "doc=b", t1, first)

	pending, err := s.Pending(context.Background(), model, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 || pending[0].Digest != first.Digest() || pending[1].Digest != second.Digest() {
		t.Fatalf("pending = %+v, want the two digests oldest first", pending)
	}
	if pending[0].Text != first.Text || pending[0].Label != first.Label {
		t.Errorf("pending carries %+v, want the section's label and text", pending[0])
	}
	if limited, _ := s.Pending(context.Background(), model, 1); len(limited) != 1 {
		t.Errorf("limit 1 returned %d", len(limited))
	}

	vectors(t, s, map[knowledge.Section][]float32{first: {1, 0, 0}})
	vectors(t, s, map[knowledge.Section][]float32{first: {1, 0, 0}}) // idempotent
	pending, err = s.Pending(context.Background(), model, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Digest != second.Digest() {
		t.Errorf("after vectors for the first digest, pending = %+v", pending)
	}
	other := knowledge.ModelID{Provider: "test", Model: "other", Dimensions: 3}
	if all, _ := s.Pending(context.Background(), other, 10); len(all) != 2 {
		t.Errorf("another model's pending = %d digests, want 2: vectors are per model", len(all))
	}
}

func concurrentPutsToDifferentDocuments(t *testing.T, s knowledge.Store) {
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Put(context.Background(), fmt.Sprintf("doc=%d", i),
				[]knowledge.Section{section("Cause", fmt.Sprintf("pool exhaustion number %d", i))}, t0)
		}()
	}
	wg.Wait()
	docs := make([]string, n)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent put %d: %v", i, errs[i])
		}
		docs[i] = fmt.Sprintf("doc=%d", i)
	}
	if hits := lexical(t, s, "exhaustion", docs, t1, 50); len(hits) != n {
		t.Errorf("after %d concurrent puts, %d sections match", n, len(hits))
	}
}
