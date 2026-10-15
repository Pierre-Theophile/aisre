// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"strings"
	"time"
	"unicode/utf8"
)

// The payload the poller writes and a recording holds (007 T018, T021, T022): one read of one data
// source, already reduced to the fields a registration needs. Nothing that names a person is in it,
// because the client never decodes one.

// PayloadPages is the one payload kind this feeder reads.
const PayloadPages = "pages"

// The read modes.
const (
	ModeIncremental = "incremental"
	ModeReconcile   = "reconcile"
)

// Why a registered page is gone (research R6).
const (
	GoneTrashed  = "trashed"
	GoneMoved    = "moved"
	GoneNotFound = "not_found"
)

// pagesPayload is one read of one data source.
type pagesPayload struct {
	Workspace string `json:"workspace"`
	// Binding is the index of the configuration's database the read belongs to. The database id is
	// pseudonymised in a recording; the index is what a replay finds its binding by.
	Binding    int    `json:"binding"`
	Database   string `json:"database"`
	DataSource string `json:"data_source"`
	Mode       string `json:"mode"`
	// From is the lower bound of an incremental read; zero for a reconciliation.
	From time.Time `json:"edited_since,omitzero"`
	// Complete is false when the read was cut short or the database could not be read: a coverage
	// gap, and a reconciliation that is not complete retracts nothing.
	Complete bool `json:"complete"`
	// Gap says why it is not complete.
	Gap   string         `json:"gap,omitempty"`
	Pages []observedPage `json:"pages"`
	// Properties are the configured property names at least one listed page carried: what the
	// operator's report calls resolved. A name no page carries is a typo or a renamed column.
	Properties []string `json:"properties_found,omitempty"`
	// Outside counts listed pages whose parent is not a configured database (a linked view). They
	// are reported as a count only.
	Outside int `json:"outside,omitempty"`
	// EndOfCycle marks the last read of a cycle.
	EndOfCycle bool `json:"end_of_cycle,omitempty"`
}

// observedPage is one page as a read saw it, or a registered page found gone.
type observedPage struct {
	ID     string    `json:"id"`
	URL    string    `json:"url,omitempty"`
	Title  string    `json:"title,omitempty"`
	Parts  []part    `json:"summary,omitempty"`
	Values []string  `json:"values,omitempty"`
	Author time.Time `json:"authored,omitzero"`
	// Created and Edited are Notion's created_time and last_edited_time.
	Created time.Time `json:"created,omitzero"`
	Edited  time.Time `json:"edited,omitzero"`
	// Registered says an earlier read registered the page, and Previous are the entity refs it was
	// linked to then: what a change is computed against, carried in the payload so that the feeder
	// (and a replay) needs no memory of its own.
	Registered bool     `json:"registered,omitempty"`
	Previous   []string `json:"previous,omitempty"`
	// Conflict names the earlier binding that already holds the page this cycle.
	Conflict string `json:"conflict,omitempty"`
	// Gone and GoneAt: a registered page the read no longer returned, and when its links end.
	Gone   string    `json:"gone,omitempty"`
	GoneAt time.Time `json:"gone_at,omitzero"`
	// SectionsRead says the page's properties and body were read into Sections by this read (008
	// research R6): only when its last_edited_time moved since the last such read. Sections is then
	// the page's whole current set, already sanitised, and Skipped what was left out and why.
	SectionsRead bool             `json:"sections_read,omitempty"`
	Sections     []sectionPart    `json:"sections,omitempty"`
	Skipped      []skippedSection `json:"skipped_sections,omitempty"`
	// sectionsReadAt is when they were read; the poller's state keeps it, a payload does not.
	sectionsReadAt time.Time
}

// sectionPart is one part of one section of a page (008 FR-011, FR-012).
type sectionPart struct {
	Label  string `json:"label"`
	Part   int    `json:"part,omitempty"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
}

// skippedSection is something of a page that was not indexed, with its reason (contracts §6). Count
// is how many blocks of one unsupported type.
type skippedSection struct {
	Label  string `json:"label,omitempty"`
	Reason string `json:"reason"`
	Count  int    `json:"count,omitempty"`
}

// part is one named summary property's text.
type part struct {
	Property string `json:"property"`
	Text     string `json:"text"`
}

// SummaryCap is FR-022's limit, in characters.
const SummaryCap = 600

// summarise joins the parts with ` — ` and cuts at the cap on a word boundary (FR-022).
func summarise(parts []part) (string, bool) {
	var texts []string
	for _, p := range parts {
		if t := strings.Join(strings.Fields(p.Text), " "); t != "" {
			texts = append(texts, t)
		}
	}
	joined := strings.Join(texts, " — ")
	if utf8.RuneCountInString(joined) <= SummaryCap {
		return joined, false
	}
	runes := []rune(joined)
	cut := string(runes[:SummaryCap-1])
	if i := strings.LastIndexAny(cut, " \t"); i > SummaryCap/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " —") + "…", true
}

// pageURL is the page's location. A recording drops the URL, so a replay rebuilds one from the id.
func pageURL(p observedPage) string {
	if p.URL != "" {
		return p.URL
	}
	return "https://www.notion.so/" + strings.ReplaceAll(p.ID, "-", "")
}
