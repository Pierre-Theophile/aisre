// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/storetest"
)

// TestKnowledgeStoreConformance: the default knowledge store passes the published suite unmodified
// (008 T019, FR-005–FR-007).
func TestKnowledgeStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) knowledge.Store { return postgres.NewKnowledgeStore(pgtest.Open(t)) })
}

// TestKnowledgeStoreMatchesAnyTerm: an alert's words are rarely all in one section, so the lexical
// search matches any of them (an AND would miss the section that names three of four).
func TestKnowledgeStoreMatchesAnyTerm(t *testing.T) {
	s := postgres.NewKnowledgeStore(pgtest.Open(t))
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.Put(ctx, "doc=a", []knowledge.Section{
		{Label: "Cause racine", Text: "Le pool de connexions PostgreSQL était épuisé.", Origin: knowledge.OriginBody},
	}, at); err != nil {
		t.Fatalf("put: %v", err)
	}
	hits, err := s.Lexical(ctx, "pool connexions épuisé gateway timeout", []string{"doc=a"}, at, 10)
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the French section matched on three of five terms", hits)
	}
	// Quotes and tsquery operators in an alert are words, not syntax.
	if _, err := s.Lexical(ctx, `pool' & ! ( "x" | :*`, []string{"doc=a"}, at, 10); err != nil {
		t.Errorf("an alert with tsquery syntax in it failed: %v", err)
	}
}

// TestKnowledgeStoreRefusesTwoChangesAtOneInstant: a version known over an empty interval would be
// history nobody could read; the second change at the same instant is refused, not lost.
func TestKnowledgeStoreRefusesTwoChangesAtOneInstant(t *testing.T) {
	s := postgres.NewKnowledgeStore(pgtest.Open(t))
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	section := func(text string) []knowledge.Section {
		return []knowledge.Section{{Label: "Cause", Text: text, Origin: knowledge.OriginBody}}
	}
	if _, err := s.Put(ctx, "doc=a", section("first"), at); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Put(ctx, "doc=a", section("second"), at); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("err = %v, want a refusal of the second change at one instant", err)
	}
}
