// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// A page's sections (feature 008 FR-010–FR-014, research R6).
//
// Each indexed property becomes one section labelled with the property name. The body becomes one
// section per heading, labelled with the heading's text; text before the first heading is the
// section BodyLabel. A section longer than knowledge.MaxSectionChars is split into parts on a word
// boundary, every part keeping the label. Before anything is split, every label and text is
// sanitised: user mentions were dropped when the rich text was read, and e-mail addresses, @handles
// and secret shapes are replaced by sanitise.Redacted. A section nothing is left of is skipped with
// its reason. What a section never carries is a person: the client decodes no author, no people
// property and no comment.

// The body limits of research R6.
const (
	// MaxBodyDepth is how deep the body is read: top-level blocks are depth 1.
	MaxBodyDepth = 3
	// MaxBodyBlocks is how many blocks of one page are read.
	MaxBodyBlocks = 1000
	// BodyLabel labels the body text before the first heading.
	BodyLabel = "Body"
)

// The reasons a section, or part of a body, is not indexed (contracts §6). A closed set; an
// unsupported block's reason is SkipUnsupported followed by its type.
const (
	SkipEmptied       = "emptied by sanitisation"
	SkipTruncated     = "over 1,000 blocks; truncated"
	SkipUnsupported   = "unsupported block"
	SkipManySections  = "over 500 sections; truncated"
	sectionLabelSpace = " "
)

// BodyReader reads a block's children. *notionx.Client satisfies it.
type BodyReader interface {
	BlockChildren(ctx context.Context, id, cursor string) (notionx.BlockPage, error)
}

// The block types research R6 reads, by what is done with them.
var (
	headingBlocks = map[string]bool{"heading_1": true, "heading_2": true, "heading_3": true}
	textBlocks    = map[string]bool{
		"paragraph": true, "bulleted_list_item": true, "numbered_list_item": true, "to_do": true,
		"quote": true, "callout": true, "toggle": true, "code": true, "table_row": true,
	}
	// containerBlocks carry no text of their own; their children are read.
	containerBlocks = map[string]bool{"table": true, "column_list": true, "column": true, "synced_block": true}
	// silentBlocks carry nothing to index and are not worth reporting.
	silentBlocks = map[string]bool{"divider": true, "table_of_contents": true, "breadcrumb": true}
	// leafBlocks are never descended into: another page's content is that page's to index.
	leafBlocks = map[string]bool{"child_page": true, "child_database": true, "link_to_page": true}
)

// readBody reads a page's blocks in document order, to MaxBodyDepth and at most MaxBodyBlocks. It
// reports whether the body was cut at the limit.
func readBody(ctx context.Context, r BodyReader, pageID string) ([]notionx.Block, bool, error) {
	var out []notionx.Block
	truncated := false
	var walk func(id string, depth int) error
	walk = func(id string, depth int) error {
		cursor := ""
		for {
			page, err := r.BlockChildren(ctx, id, cursor)
			if err != nil {
				return err
			}
			for _, block := range page.Blocks {
				if len(out) == MaxBodyBlocks {
					truncated = true
					return nil
				}
				out = append(out, block)
				if !block.HasChildren || depth >= MaxBodyDepth || leafBlocks[block.Type] || block.SyncedFrom {
					continue
				}
				if err := walk(block.ID, depth+1); err != nil {
					return err
				}
				if truncated {
					return nil
				}
			}
			if !page.HasMore || page.NextCursor == "" {
				return nil
			}
			cursor = page.NextCursor
		}
	}
	if err := walk(pageID, 1); err != nil {
		return nil, false, err
	}
	return out, truncated, nil
}

// namedText is one indexed property's text.
type namedText struct {
	Name string
	Text string
}

// draft is a section before it is sanitised and split.
type draft struct {
	label  string
	origin knowledge.Origin
	lines  []string
}

// buildSections turns a page's indexed properties and body blocks into its sections and the list
// of what was skipped.
func buildSections(props []namedText, blocks []notionx.Block, truncated bool) ([]sectionPart, []skippedSection) {
	var drafts []draft
	for _, p := range props {
		drafts = append(drafts, draft{label: p.Name, origin: knowledge.OriginProperty, lines: []string{p.Text}})
	}

	unsupported := map[string]int{}
	body := draft{label: BodyLabel, origin: knowledge.OriginBody}
	for _, block := range blocks {
		switch {
		case headingBlocks[block.Type]:
			drafts = append(drafts, body)
			body = draft{label: block.Text(), origin: knowledge.OriginBody}
		case textBlocks[block.Type]:
			if t := block.Text(); t != "" {
				body.lines = append(body.lines, t)
			}
		case block.Type == "synced_block" && block.SyncedFrom:
			unsupported[block.Type]++
		case containerBlocks[block.Type], silentBlocks[block.Type]:
		default:
			unsupported[block.Type]++
		}
	}
	drafts = append(drafts, body)

	var (
		out     []sectionPart
		skipped []skippedSection
		seen    = map[string]int{}
	)
	for _, d := range drafts {
		text := strings.TrimSpace(strings.Join(d.lines, "\n"))
		if text == "" {
			continue // a heading with nothing under it, or no text before the first heading
		}
		label := sectionLabel(d.label)
		text, _ = sanitise.RedactText(text)
		if sanitise.OnlyRedactions(text) {
			skipped = append(skipped, skippedSection{Label: label, Reason: SkipEmptied})
			continue
		}
		// Two headings with one text are two sections: the second is labelled "<text> (2)".
		seen[label]++
		if n := seen[label]; n > 1 {
			label = fmt.Sprintf("%s (%d)", label, n)
		}
		for i, part := range splitText(text, knowledge.MaxSectionChars) {
			out = append(out, sectionPart{Label: label, Part: i, Text: part, Origin: string(d.origin)})
		}
	}
	if len(out) > knowledge.MaxSections {
		out = out[:knowledge.MaxSections]
		skipped = append(skipped, skippedSection{Reason: SkipManySections})
	}
	if truncated {
		skipped = append(skipped, skippedSection{Reason: SkipTruncated})
	}
	types := make([]string, 0, len(unsupported))
	for t := range unsupported {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		skipped = append(skipped, skippedSection{Reason: SkipUnsupported + " " + t, Count: unsupported[t]})
	}
	return out, skipped
}

// sectionLabel sanitises a label and keeps it within knowledge.MaxLabelChars; an empty one (a
// heading with no text) is the body label.
func sectionLabel(label string) string {
	label = strings.Join(strings.Fields(label), sectionLabelSpace)
	label, _ = sanitise.RedactText(label)
	if label == "" {
		return BodyLabel
	}
	if utf8.RuneCountInString(label) > knowledge.MaxLabelChars {
		label = strings.TrimSpace(string([]rune(label)[:knowledge.MaxLabelChars-1])) + "…"
	}
	return label
}

// splitText cuts text into parts of at most limit characters: at the last line break before the
// limit when there is one in its second half, else at the last space there, else at the limit.
func splitText(text string, limit int) []string {
	var parts []string
	runes := []rune(text)
	for len(runes) > limit {
		cut := lastIn(runes, limit, '\n')
		if cut < 0 {
			cut = lastIn(runes, limit, ' ')
		}
		if cut < 0 {
			cut = limit
		}
		if part := strings.TrimSpace(string(runes[:cut])); part != "" {
			parts = append(parts, part)
		}
		runes = []rune(strings.TrimLeft(string(runes[cut:]), " \n"))
	}
	if part := strings.TrimSpace(string(runes)); part != "" {
		parts = append(parts, part)
	}
	return parts
}

// lastIn is the index of the last r in runes[limit/2+1 : limit+1], or -1.
func lastIn(runes []rune, limit int, r rune) int {
	for i := limit; i > limit/2; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// toKnowledge converts a payload's sections for the store.
func toKnowledge(parts []sectionPart) []knowledge.Section {
	out := make([]knowledge.Section, 0, len(parts))
	for _, p := range parts {
		out = append(out, knowledge.Section{Label: p.Label, Part: p.Part, Text: p.Text, Origin: knowledge.Origin(p.Origin)})
	}
	return out
}
