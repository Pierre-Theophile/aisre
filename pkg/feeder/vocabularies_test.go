// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The published vocabulary page and the registry are one list (004 T028).
//
// docs/schema/pointers.md is where constitution IV's "document why a pointer cannot be expressed in
// OpenTelemetry semantic conventions" is actually discharged. That guarantee is only worth anything
// while the document covers the whole registry: a vocabulary the page does not document is a selector
// grammar a backend may be handed with nothing to read it by, and one the page documents that nothing
// registers promises a grammar no connector mints.
//
// Nothing compared the two before this, which is the same gap docs/schema/resolution.md had.
func TestThePointerVocabularyPageAndTheRegistryAreTheSameList(t *testing.T) {
	t.Parallel()

	page := documentedVocabularies(t, "../../docs/schema/pointers.md")
	if len(page) == 0 {
		t.Fatal("no vocabulary was parsed out of docs/schema/pointers.md; a comparison against " +
			"nothing is not a comparison")
	}
	for _, name := range page {
		if !slices.Contains(feeder.Vocabularies, name) {
			t.Errorf("docs/schema/pointers.md documents the vocabulary %q, which pkg/feeder does not "+
				"register: the page promises a selector grammar no connector mints", name)
		}
	}
	for _, name := range feeder.Vocabularies {
		if !slices.Contains(page, name) {
			t.Errorf("pkg/feeder registers %q, which docs/schema/pointers.md does not document: a "+
				"backend may be handed a selector in it with nothing to read it by, and constitution "+
				"IV's justification is only a guarantee while the page is the whole list", name)
		}
	}
	// The two feature-004 vocabularies are source links and carry no join roles, which the page says
	// and which this asserts is not an oversight.
	for _, name := range feeder.DeployVocabularies {
		if !slices.Contains(feeder.Vocabularies, name) {
			t.Errorf("%q is in DeployVocabularies but not in the published set", name)
		}
	}
}

// documentedVocabularies reads the back-quoted names out of the "Vocabularies" section's `###`
// headings. The parse is narrow — that section only, a heading only — so a vocabulary mentioned in
// prose elsewhere is not mistaken for a documented one.
func documentedVocabularies(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("open the published page: %v", err)
	}
	var out []string
	var inSection bool
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = strings.TrimSpace(strings.TrimPrefix(line, "## ")) == "Vocabularies"
			continue
		}
		if !inSection || !strings.HasPrefix(line, "### ") {
			continue
		}
		heading := strings.TrimPrefix(line, "### ")
		start := strings.Index(heading, "`")
		if start < 0 {
			continue
		}
		rest := heading[start+1:]
		end := strings.Index(rest, "`")
		if end < 0 {
			continue
		}
		if name := strings.TrimSpace(rest[:end]); name != "" {
			out = append(out, name)
		}
	}
	return out
}
