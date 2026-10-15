// SPDX-License-Identifier: Apache-2.0

package embedding_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/embedding"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/memory"
)

// flakyEmbedder fails while failing is true.
type flakyEmbedder struct {
	fixedEmbedder
	failing bool
	model   knowledge.ModelID
}

func (f *flakyEmbedder) Model() knowledge.ModelID {
	if f.model.IsZero() {
		return f.fixedEmbedder.Model()
	}
	return f.model
}

func (f *flakyEmbedder) Embed(ctx context.Context, texts []string, p knowledge.Purpose) ([][]float32, error) {
	if f.failing {
		return nil, errors.New("connection refused")
	}
	return f.fixedEmbedder.Embed(ctx, texts, p)
}

func put(t *testing.T, store knowledge.Store, ref string, n int, at time.Time) {
	t.Helper()
	sections := make([]knowledge.Section, n)
	for i := range sections {
		sections[i] = knowledge.Section{Label: "Section", Part: i, Text: "text " + ref + string(rune('a'+i%26)), Origin: knowledge.OriginBody}
	}
	if _, err := store.Put(context.Background(), ref, sections, at); err != nil {
		t.Fatal(err)
	}
}

func TestIndexerBackfillsThenServes(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	put(t, store, "notion.page=a", 150, t0)
	put(t, store, "notion.page=b", 3, t0)

	now := t0
	live := &flakyEmbedder{failing: true}
	idx := embedding.NewIndexer(store, live, nil, embedding.WithClock(func() time.Time { return now }))
	if idx.Ready() {
		t.Fatal("an indexer that has embedded nothing is ready")
	}

	// The provider is down: the round fails, nothing serves, and the lag grows.
	now = now.Add(5 * time.Minute)
	if err := idx.Drain(ctx); err == nil {
		t.Fatal("a round over a failing embedder succeeded")
	}
	status := idx.Status()
	if idx.Ready() || status.Pending != 153 || status.Lag != 5*time.Minute || status.LastError == "" {
		t.Errorf("status while the provider is down = %+v", status)
	}

	live.failing = false
	now = now.Add(time.Minute)
	if err := idx.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	status = idx.Status()
	if !idx.Ready() || status.Pending != 0 || status.Lag != 0 || status.Embedded != 153 || status.LastError != "" {
		t.Errorf("status after the backfill = %+v", status)
	}
	if pending, _ := store.Pending(ctx, live.Model(), 0); len(pending) != 0 {
		t.Errorf("%d sections still pending", len(pending))
	}
	hits, err := store.Semantic(ctx, knowledge.Normalise([]float32{1, 1, 1}), live.Model(), []string{"notion.page=b"}, now, 5)
	if err != nil || len(hits) != 3 {
		t.Errorf("semantic search after the backfill: %d hits, %v", len(hits), err)
	}

	// A new page arrives: still ready (the model's backfill is done), embedded on the next round.
	put(t, store, "notion.page=c", 2, now)
	if !idx.Ready() {
		t.Error("a section arriving after the backfill unreadied the model")
	}
	if err := idx.Drain(ctx); err != nil || idx.Status().Embedded != 155 {
		t.Errorf("the next round: %v, %+v", err, idx.Status())
	}

	// A model change is a new indexer with no vectors: not ready until its own backfill (FR-028).
	other := embedding.NewIndexer(store, &flakyEmbedder{model: knowledge.ModelID{Provider: "openai_compatible", Model: "other", Dimensions: 3}}, nil)
	if other.Ready() {
		t.Error("a new model serves before its backfill")
	}
	if err := other.Drain(ctx); err != nil || !other.Ready() || other.Status().Embedded != 155 {
		t.Errorf("the new model's backfill: %v, %+v", err, other.Status())
	}
}

func TestIndexerRunStopsWithItsContext(t *testing.T) {
	store := memory.New()
	put(t, store, "notion.page=a", 1, time.Now())
	idx := embedding.NewIndexer(store, &fixedEmbedder{}, nil, embedding.WithInterval(time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { idx.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for !idx.Ready() {
		select {
		case <-deadline:
			t.Fatal("the indexer never became ready")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}
