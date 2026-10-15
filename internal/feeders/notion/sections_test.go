// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

func block(id, typ, text string, children bool) notionx.Block {
	b := notionx.Block{ID: id, Type: typ, HasChildren: children}
	if text != "" {
		b.RichText = []notionx.RichText{{Type: "text", PlainText: text}}
	}
	return b
}

type blockTree map[string][]notionx.Block

func (t blockTree) BlockChildren(_ context.Context, id, cursor string) (notionx.BlockPage, error) {
	children := t[id]
	start := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "%d", &start)
	}
	end := min(start+100, len(children))
	page := notionx.BlockPage{Blocks: children[start:end]}
	if end < len(children) {
		page.HasMore, page.NextCursor = true, fmt.Sprint(end)
	}
	return page, nil
}

func TestBuildSectionsByHeading(t *testing.T) {
	props := []namedText{{Name: "Signals / Symptoms", Text: "checkout p99 over 4s"}, {Name: "Root cause", Text: "pool too small"}}
	blocks := []notionx.Block{
		block("1", "paragraph", "Opened by the on-call.", false),
		block("2", "heading_1", "Timeline", false),
		block("3", "bulleted_list_item", "14:02 page fired", false),
		block("4", "toggle", "Details", true),
		block("5", "paragraph", "inside the toggle", false),
		block("6", "embed", "", false),
		block("7", "heading_2", "Empty heading", false),
		block("8", "heading_2", "Timeline", false),
		block("9", "table", "", true),
		{ID: "10", Type: "table_row", Cells: [][]notionx.RichText{{{Type: "text", PlainText: "a"}}, {{Type: "text", PlainText: "b"}}}},
		block("11", "image", "", false),
		block("12", "embed", "", false),
		{ID: "13", Type: "synced_block", SyncedFrom: true},
		block("14", "divider", "", false),
	}
	sections, skipped := buildSections(props, blocks, false)

	var got []string
	for _, s := range sections {
		got = append(got, fmt.Sprintf("%s/%d/%s: %s", s.Label, s.Part, s.Origin, s.Text))
	}
	want := []string{
		"Signals / Symptoms/0/property: checkout p99 over 4s",
		"Root cause/0/property: pool too small",
		"Body/0/body: Opened by the on-call.",
		"Timeline/0/body: 14:02 page fired\nDetails\ninside the toggle",
		"Timeline (2)/0/body: a | b",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sections:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var reasons []string
	for _, s := range skipped {
		reasons = append(reasons, fmt.Sprintf("%s=%d", s.Reason, s.Count))
	}
	if strings.Join(reasons, ",") != "unsupported block embed=2,unsupported block image=1,unsupported block synced_block=1" {
		t.Errorf("skipped = %v", reasons)
	}
}

func TestBuildSectionsSplitsLongSectionsIntoParts(t *testing.T) {
	long := strings.Repeat("connection pool exhausted after the rollout ", 80) // ~3,500 characters
	sections, _ := buildSections([]namedText{{Name: "Signals / Symptoms", Text: long}}, nil, false)
	if len(sections) < 3 {
		t.Fatalf("%d parts, want at least 3", len(sections))
	}
	var joined []string
	for i, s := range sections {
		if s.Label != "Signals / Symptoms" || s.Part != i {
			t.Errorf("part %d is %q/%d", i, s.Label, s.Part)
		}
		if n := utf8.RuneCountInString(s.Text); n > knowledge.MaxSectionChars || n == 0 {
			t.Errorf("part %d has %d characters", i, n)
		}
		if strings.HasSuffix(s.Text, " connectio") {
			t.Errorf("part %d is cut inside a word", i)
		}
		joined = append(joined, s.Text)
	}
	if strings.Join(strings.Fields(strings.Join(joined, " ")), " ") != strings.Join(strings.Fields(long), " ") {
		t.Error("the parts do not add up to the section")
	}
}

func TestBuildSectionsRedactsAndSkipsWhatIsLeftEmpty(t *testing.T) {
	person, _ := sanitise.NewCanary(sanitise.CanaryPerson, "property")
	secret, _ := sanitise.NewCanary(sanitise.CanarySecret, "paragraph")
	heading, _ := sanitise.NewCanary(sanitise.CanaryPerson, "heading")
	toggle, _ := sanitise.NewCanary(sanitise.CanarySecret, "toggle")
	props := []namedText{{Name: "Summary", Text: "paged " + person.Carrier + " at 14:02"}}
	blocks := []notionx.Block{
		block("1", "heading_1", "Contact "+heading.Carrier, false),
		block("2", "paragraph", "the key was "+secret.Carrier, false),
		block("3", "toggle", toggle.Carrier, false),
		block("4", "heading_1", "Escalation", false),
		block("5", "paragraph", person.Carrier, false),
	}
	sections, skipped := buildSections(props, blocks, false)
	for _, s := range sections {
		if strings.Contains(s.Label+s.Text, sanitise.CanaryPrefix) {
			t.Errorf("a canary survived: %+v", s)
		}
	}
	if len(sections) != 2 || sections[0].Text != "paged [redacted] at 14:02" ||
		sections[1].Label != "Contact [redacted]" || sections[1].Text != "the key was [redacted]\n[redacted]" {
		t.Errorf("sections = %+v", sections)
	}
	if len(skipped) != 1 || skipped[0] != (skippedSection{Label: "Escalation", Reason: SkipEmptied}) {
		t.Errorf("skipped = %+v", skipped)
	}
}

func TestReadBodyStopsAtDepthAndAtTheBlockLimit(t *testing.T) {
	tree := blockTree{
		"page": {block("a", "toggle", "one", true), block("b", "child_page", "", true)},
		"a":    {block("a1", "toggle", "two", true)},
		"a1":   {block("a2", "toggle", "three", true)},
		"a2":   {block("a3", "paragraph", "four: too deep", false)},
		"b":    {block("b1", "paragraph", "another page's content", false)},
	}
	blocks, truncated, err := readBody(context.Background(), tree, "page")
	if err != nil || truncated {
		t.Fatalf("readBody: %v, truncated %v", err, truncated)
	}
	var texts []string
	for _, b := range blocks {
		texts = append(texts, b.Text())
	}
	if strings.Join(texts, ",") != "one,two,three," {
		t.Errorf("read %q; depth 3 and no child page", texts)
	}

	var many []notionx.Block
	for i := range MaxBodyBlocks + 50 {
		many = append(many, block(fmt.Sprint(i), "paragraph", "x", false))
	}
	blocks, truncated, err = readBody(context.Background(), blockTree{"page": many}, "page")
	if err != nil || !truncated || len(blocks) != MaxBodyBlocks {
		t.Fatalf("%d blocks, truncated %v, %v", len(blocks), truncated, err)
	}
	_, skipped := buildSections(nil, blocks, truncated)
	if len(skipped) != 1 || skipped[0].Reason != SkipTruncated {
		t.Errorf("skipped = %+v", skipped)
	}
}
