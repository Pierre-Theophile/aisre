// SPDX-License-Identifier: Apache-2.0

package embedding_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/embedding"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// fixedEmbedder places a text by its length and first byte, and counts what it was asked.
type fixedEmbedder struct{ asked int }

func (f *fixedEmbedder) Model() knowledge.ModelID {
	return knowledge.ModelID{Provider: "openai_compatible", Model: "fixed", Dimensions: 3}
}

func (f *fixedEmbedder) Embed(_ context.Context, texts []string, _ knowledge.Purpose) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		f.asked++
		out[i] = knowledge.Normalise([]float32{float32(len(t)), float32(t[0]), 0.5})
	}
	return out, nil
}

func TestRecordedReplaysExactlyWhatWasRecorded(t *testing.T) {
	ctx := context.Background()
	live := &fixedEmbedder{}
	rec := embedding.NewRecorder(live)
	sections := []string{knowledge.EmbeddingText("Root cause", "pool exhausted"), knowledge.EmbeddingText("Summary", "rolled back")}
	want, err := rec.Embed(ctx, sections, knowledge.PurposeDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Embed(ctx, append(sections, "x"), knowledge.PurposeDocument); err != nil {
		t.Fatal(err)
	}
	if live.asked != 3 {
		t.Errorf("the recorder asked the live embedder %d times; a text already recorded is not asked again", live.asked)
	}
	query, _ := rec.Embed(ctx, []string{"connections dropped"}, knowledge.PurposeQuery)

	var buf bytes.Buffer
	if err := rec.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "pool exhausted") {
		t.Error("the recording carries a section's text; it is keyed by the text's hash only")
	}
	var again bytes.Buffer
	_ = rec.Write(&again)
	if buf.String() != again.String() {
		t.Error("two writes of the same recording differ")
	}

	replay, err := embedding.ReadRecorded(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Model() != live.Model() || replay.Len() != 4 {
		t.Errorf("model %v with %d texts; want %v with 4", replay.Model(), replay.Len(), live.Model())
	}
	got, err := replay.Embed(ctx, sections, knowledge.PurposeDocument)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("section %d replayed %v; recorded %v", i, got[i], want[i])
			}
		}
	}
	if q, err := replay.Embed(ctx, []string{"connections dropped"}, knowledge.PurposeQuery); err != nil || q[0][0] != query[0][0] {
		t.Errorf("query replay: %v", err)
	}

	// Refused, never approximated: an unknown text, and a known text under the other purpose.
	if _, err := replay.Embed(ctx, []string{"never recorded"}, knowledge.PurposeQuery); !errors.Is(err, embedding.ErrNotRecorded) {
		t.Errorf("unknown text: %v", err)
	}
	if _, err := replay.Embed(ctx, []string{"connections dropped"}, knowledge.PurposeDocument); !errors.Is(err, embedding.ErrNotRecorded) {
		t.Errorf("a query text asked as a document: %v", err)
	}
	if _, err := embedding.ReadRecorded(strings.NewReader("")); err == nil {
		t.Error("an empty recording was accepted")
	}
	bad := strings.Replace(buf.String(), `"dimensions":3`, `"dimensions":4`, 1)
	if _, err := embedding.ReadRecorded(strings.NewReader(bad)); !errors.Is(err, knowledge.ErrDimensionMismatch) {
		t.Errorf("a recording whose vectors are not the model's length: %v", err)
	}
}
