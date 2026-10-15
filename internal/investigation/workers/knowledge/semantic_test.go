// SPDX-License-Identifier: Apache-2.0

package knowledge_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/memory"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The semantic half of knowledge/hybrid@1 and its fallback (008 T033; FR-020, FR-027, FR-028, SC-006).

// conceptEmbedder places a text on three axes by the words it contains: connection loss, disk, and
// everything else. "dropped by the peer" and "ECONNRESET" share no word and land together.
type conceptEmbedder struct {
	err   error
	calls int
}

func (c *conceptEmbedder) Model() kstore.ModelID {
	return kstore.ModelID{Provider: "openai_compatible", Model: "concepts", Dimensions: 3}
}

func (c *conceptEmbedder) Embed(_ context.Context, texts []string, _ kstore.Purpose) ([][]float32, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		lower := strings.ToLower(text)
		v := []float32{0, 0, 0.1}
		for _, w := range []string{"econnreset", "dropped", "peer", "reset"} {
			if strings.Contains(lower, w) {
				v[0]++
			}
		}
		for _, w := range []string{"disk", "volume", "inode"} {
			if strings.Contains(lower, w) {
				v[1]++
			}
		}
		out[i] = kstore.Normalise(v)
	}
	return out, nil
}

// semanticFixture: two postmortems, both in scope; one about connection resets, one about a full
// disk. Neither shares a word with the query "connections dropped by peer".
func semanticFixture(t *testing.T, e kstore.Embedder) *memory.Store {
	t.Helper()
	store := memory.New()
	storePut(t, store, "notion.page=resets", beforePin, body("Symptoms", "checkout logged ECONNRESET from the payments client"))
	storePut(t, store, "notion.page=disk", beforePin, body("Symptoms", "volume ran out of inodes"))
	ctx := context.Background()
	pending, err := store.Pending(ctx, e.Model(), 0)
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, len(pending))
	for i, p := range pending {
		texts[i] = kstore.EmbeddingText(p.Label, p.Text)
	}
	vectors, err := (&conceptEmbedder{}).Embed(ctx, texts, kstore.PurposeDocument)
	if err != nil {
		t.Fatal(err)
	}
	var dv []kstore.DigestVector
	for i, p := range pending {
		dv = append(dv, kstore.DigestVector{Digest: p.Digest, Vector: vectors[i]})
	}
	if err := store.PutVectors(ctx, e.Model(), dv); err != nil {
		t.Fatal(err)
	}
	return store
}

func semanticCorpus() corpus {
	return corpus{documents: []knowledge.Document{
		postmortem("notion.page=disk", "Reporting outage", "entity-checkout"),
		postmortem("notion.page=resets", "Checkout outage", "entity-checkout"),
	}}
}

func callSearch(t *testing.T, w *knowledge.Worker, terms ...string) *investigationv1.AlgebraResponse {
	t.Helper()
	request := knowledge.SearchTerm([]string{"entity-checkout"}, terms, 10, "hyp-1", "has this happened before?")
	request.ValidAt = timestamppb.New(pin)
	request.ObservedAt = timestamppb.New(pin)
	resp, err := w.Call(context.Background(), worker.Request{Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return resp.Algebra
}

func TestARewordedSymptomIsFoundSemantically(t *testing.T) {
	e := &conceptEmbedder{}
	store := semanticFixture(t, e)
	query := "connections dropped by peer"

	lexical := callSearch(t, knowledge.New(semanticCorpus(), knowledge.WithStore(store)), query)
	for _, item := range lexical.GetDigest().GetKnowledge().GetItems() {
		if item.GetSectionLabel() != "" {
			t.Fatalf("lexical only matched %s on %q; the fixture's query must share no word with it", item.GetDocumentId(), item.GetSectionLabel())
		}
	}
	if got := lexical.GetDigest().GetKnowledge().GetRetrievalMode(); got != knowledge.ModeLexicalOnly {
		t.Errorf("no embedder: retrieval_mode %q", got)
	}

	hybrid := callSearch(t, knowledge.New(semanticCorpus(), knowledge.WithStore(store), knowledge.WithEmbedder(e, nil)), query)
	digest := hybrid.GetDigest().GetKnowledge()
	items := digest.GetItems()
	if len(items) == 0 || items[0].GetDocumentId() != "notion.page=resets" || items[0].GetSectionLabel() != "Symptoms" {
		t.Fatalf("hybrid cited %v; want the connection-reset postmortem first, on its Symptoms section", ids(items))
	}
	if digest.GetRetrievalMode() != knowledge.ModeHybrid || digest.GetEmbeddingModel() != "openai_compatible:concepts@3" {
		t.Errorf("digest says %q %q", digest.GetRetrievalMode(), digest.GetEmbeddingModel())
	}
	executed := hybrid.GetDigest().GetExecutedQuery()
	if !strings.Contains(executed, `query="connections dropped by peer"`) || !strings.Contains(executed, "hybrid openai_compatible:concepts@3") {
		t.Errorf("the evidence chain records %q; it must carry the query text and how it was ranked (FR-032)", executed)
	}
}

func TestSemanticFailureFallsBackToLexicalAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedder *conceptEmbedder
		ready    func() bool
		says     string
	}{
		{"the provider is down", &conceptEmbedder{err: errors.New("dial tcp: connection refused")}, nil, "embedding the query failed: dial tcp: connection refused"},
		{"the index is being built", &conceptEmbedder{}, func() bool { return false }, "is still being built"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := semanticFixture(t, tc.embedder)
			w := knowledge.New(semanticCorpus(), knowledge.WithStore(store), knowledge.WithEmbedder(tc.embedder, tc.ready))
			resp := callSearch(t, w, "ECONNRESET")
			digest := resp.GetDigest().GetKnowledge()
			if digest.GetRetrievalMode() != knowledge.ModeLexicalOnly || digest.GetEmbeddingModel() != "" {
				t.Errorf("digest says %q %q; want lexical_only and no model", digest.GetRetrievalMode(), digest.GetEmbeddingModel())
			}
			if items := digest.GetItems(); len(items) == 0 || items[0].GetDocumentId() != "notion.page=resets" {
				t.Errorf("the lexical answer still stands: cited %v", ids(items))
			}
			if executed := resp.GetDigest().GetExecutedQuery(); !strings.Contains(executed, tc.says) {
				t.Errorf("executed query %q does not say %q (FR-027)", executed, tc.says)
			}
			if tc.ready != nil && tc.embedder.calls != 0 {
				t.Error("the query was embedded although the index is not ready")
			}
		})
	}
}
