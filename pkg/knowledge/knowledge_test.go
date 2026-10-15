// SPDX-License-Identifier: Apache-2.0

package knowledge_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

func TestSectionDigestIsTheIdentityOfAVersion(t *testing.T) {
	t.Parallel()
	a := knowledge.Section{Label: "Cause", Text: "pool exhaustion", Origin: knowledge.OriginBody}
	if a.Digest() != (knowledge.Section{Label: "Cause", Text: "pool exhaustion", Origin: knowledge.OriginProperty}).Digest() {
		t.Error("the digest depends on the origin; it is label, part and text only")
	}
	for name, b := range map[string]knowledge.Section{
		"label": {Label: "Impact", Text: "pool exhaustion"},
		"part":  {Label: "Cause", Part: 1, Text: "pool exhaustion"},
		"text":  {Label: "Cause", Text: "pool exhaustion!"},
	} {
		if a.Digest() == b.Digest() {
			t.Errorf("a different %s has the same digest", name)
		}
	}
}

func TestModelIDString(t *testing.T) {
	t.Parallel()
	m := knowledge.ModelID{Provider: "openai_compatible", Model: "mistral-embed", Dimensions: 1024}
	if got := m.String(); got != "openai_compatible:mistral-embed@1024" {
		t.Errorf("String() = %q", got)
	}
	if (knowledge.ModelID{}).String() != "" || !(knowledge.ModelID{}).IsZero() {
		t.Error("the zero model renders as something")
	}
}

func TestBM25(t *testing.T) {
	t.Parallel()
	scores := knowledge.BM25("connection pool", []string{
		"connection pool exhaustion",
		"the reporting job was slow",
		"pool",
	})
	if scores[1] != 0 {
		t.Errorf("a text sharing no term scored %v", scores[1])
	}
	if scores[0] <= scores[2] {
		t.Errorf("two matching terms (%v) do not outscore one (%v)", scores[0], scores[2])
	}
	if got := knowledge.BM25("", []string{"anything"}); got[0] != 0 {
		t.Errorf("an empty query scored %v", got[0])
	}
}

func TestCosineAndNormalise(t *testing.T) {
	t.Parallel()
	v := knowledge.Normalise([]float32{3, 4})
	if v[0] != 0.6 || v[1] != 0.8 {
		t.Errorf("Normalise = %v", v)
	}
	if got := knowledge.Cosine([]float32{1, 0}, []float32{1, 1}); got != 0.707107 {
		t.Errorf("Cosine = %v", got)
	}
	if got := knowledge.Cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Errorf("Cosine of mismatched lengths = %v", got)
	}
}

func TestOwnsDocument(t *testing.T) {
	cases := []struct {
		source, ref string
		want        bool
	}{
		{"notion:LDMA", "notion.page=abc", true},
		{"github:org/repo", "github.file=ops/x.md", true},
		{"notion:LDMA", "github.file=x", false},
		{"notion:LDMA", "notion=abc", false},
		{"notion:LDMA", "notion.page=", false},
		{"notion", "notion.page=abc", false},
		{"notion:LDMA", "notionx.page=abc", false},
	}
	for _, c := range cases {
		if got := knowledge.OwnsDocument(c.source, c.ref); got != c.want {
			t.Errorf("OwnsDocument(%q, %q) = %v, want %v", c.source, c.ref, got, c.want)
		}
	}
}
