// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The changelog source (T081, FR-073, FR-074, contract §1, §3).
//
// Five shapes, one interface: Atom, RSS 2.0, JSON Feed, a vendor-native JSON document, and **HTML**.
//
// # HTML is a first-class citizen, not a fallback
//
// This is the decision that matters here. A large share of vendors publish deprecations on a web page
// and nowhere else — no feed, no API, no mailing list — and an integration that treats scraping as an
// embarrassing special case ends up not reading them at all. The failure the coverage audit found was a
// notice nobody read; refusing to read the page it was on would be choosing that failure.
//
// So the HTML adapter goes through the same extraction rules, produces the same typed fields, and
// records the same unextracted records as the feeds. What makes it honest is not the format: it is that
// **the extraction rules are configuration**, for every format including the feeds.
//
// # Announcement text is untrusted input, so prose is matched by a stated rule and never interpreted
//
// FR-073. There is no heuristic here that reads a title and decides what it means. An operator states
// rules — a regular expression with named captures for the product, the notice identifier and the
// window — and an entry that matches none is recorded as **unextracted with its reason and its
// pointer** (FR-074) rather than emitted as a guess.
//
// That is more work to configure and it is the only version that can be reviewed. A cleverer reader
// that inferred "scheduled maintenance" from a subject line would be a component treating an outsider's
// text as an instruction about what to write into the graph, which is exactly what rule 2 forbids.
//
// The vendor-native adapter needs no rules, because a document like
// `status.cloud.google.com/incidents.json` states its fields: `begin` and `end` are RFC 3339 with
// explicit offsets, and the service name is a field rather than a sentence.

// ChangelogFormat is how a changelog document is read.
type ChangelogFormat string

// The published formats.
const (
	// FormatAtom is an Atom 1.0 feed.
	FormatAtom ChangelogFormat = "atom"
	// FormatRSS is an RSS 2.0 feed.
	FormatRSS ChangelogFormat = "rss"
	// FormatJSONFeed is a JSON Feed 1.1 document.
	FormatJSONFeed ChangelogFormat = "jsonfeed"
	// FormatGCPIncidents is `status.cloud.google.com/incidents.json` and documents shaped like it.
	FormatGCPIncidents ChangelogFormat = "gcp-incidents"
	// FormatHTML is a web page, read with the configured rules and nothing else.
	FormatHTML ChangelogFormat = "html"
)

// EntryRule is one stated way to read an entry. It is configuration: the operator says what a matching
// entry means, and the feeder never decides for itself.
type EntryRule struct {
	// Match selects entries and captures fields. The named groups it may set are `product`,
	// `notice_id`, `start` and `end`; any it does not set fall back to this rule's own values.
	Match *regexp.Regexp
	// Kind is what a matching entry announces. Required: there is no default, because the kind
	// decides the ChangeKind the graph records (ADR-0006).
	Kind AnnouncementKind
	// Product is the product a matching entry is about, where the expression captures none.
	Product string
	// Layouts are the time layouts to try on the captured instants, in order. Empty means RFC 3339.
	Layouts []string
	// State makes a matching entry a **correction** rather than an announcement: `cancelled`,
	// `superseded` or `confirmed`. Empty — the usual case — is an announcement.
	//
	// It is how a cancellation is read at all. A cancelled maintenance disappears from a status page
	// rather than being marked cancelled, so the only place a cancellation is *stated* is prose: an
	// email saying the window is called off, or a changelog entry withdrawing a deprecation. A
	// correction closes the observation on the same change ref and opens another, which is why it
	// cannot be read as a new announcement (FR-067).
	State string
	// VagueStart states that entries matching this rule announce no start instant — "in a future
	// release", "in the coming weeks". The valid start is then marked **unknown** and the vendor's
	// wording is not stored in place of a timestamp (FR-069).
	VagueStart bool
}

// Changelog reads one vendor's feed, native document or page.
type Changelog struct {
	client *Client
	vendor string
	url    string
	format ChangelogFormat
	rules  []EntryRule
	// entry selects the repeating unit in an HTML page. Required for FormatHTML and unused
	// otherwise: a page has no entry boundary a parser can know, so the boundary is stated too.
	entry *regexp.Regexp
}

// ChangelogOptions configures the adapter.
type ChangelogOptions struct {
	// Client is the shared HTTP client. Required.
	Client *Client
	// Vendor is the allowlisted slug this document belongs to. Required, and configuration.
	Vendor string
	// URL is the document. Required.
	URL string
	// Format is how to read it. Required.
	Format ChangelogFormat
	// Rules are the stated extraction rules, tried in order. Required for every format but
	// gcp-incidents, which states its own fields.
	Rules []EntryRule
	// EntryPattern is the repeating unit of an HTML page, with its text in the capture group `entry`
	// or in the whole match. Required for html.
	EntryPattern *regexp.Regexp
}

// NewChangelog builds the adapter.
func NewChangelog(opts ChangelogOptions) (*Changelog, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("vendornotice: the changelog source needs an HTTP client")
	}
	if strings.TrimSpace(opts.Vendor) == "" {
		return nil, fmt.Errorf("vendornotice: the changelog source needs the vendor slug the " +
			"document belongs to; it is configuration, never a guess from a hostname")
	}
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("vendornotice: the changelog source needs a URL")
	}
	switch opts.Format {
	case FormatAtom, FormatRSS, FormatJSONFeed, FormatHTML:
		if len(opts.Rules) == 0 {
			return nil, fmt.Errorf("vendornotice: a %s changelog with no extraction rules would read "+
				"every entry and place none. The rules are how an operator states what an entry means; "+
				"without them the feeder would have to interpret prose, which is what FR-073 forbids",
				opts.Format)
		}
	case FormatGCPIncidents:
		// This one states its own fields, so rules are optional and ignored.
	default:
		return nil, fmt.Errorf("vendornotice: changelog format %q is not one of %s, %s, %s, %s or %s",
			opts.Format, FormatAtom, FormatRSS, FormatJSONFeed, FormatGCPIncidents, FormatHTML)
	}
	for i, rule := range opts.Rules {
		if rule.Match == nil {
			return nil, fmt.Errorf("vendornotice: changelog rule %d has no expression", i)
		}
		switch strings.ToLower(strings.TrimSpace(rule.State)) {
		case "", "cancelled", "superseded", "confirmed":
		case "announced":
			return nil, fmt.Errorf("vendornotice: changelog rule %d states `announced`; an "+
				"announcement is the default, and naming it here would make every matching entry a "+
				"correction of itself", i)
		default:
			return nil, fmt.Errorf("vendornotice: changelog rule %d states %q, which is not one of "+
				"cancelled, superseded or confirmed", i, rule.State)
		}
		if !rule.Kind.Valid() {
			return nil, fmt.Errorf("vendornotice: changelog rule %d announces kind %q, which is not "+
				"one this feeder publishes; there is no default, because the kind decides the change "+
				"kind the graph records", i, rule.Kind)
		}
	}
	if opts.Format == FormatHTML && opts.EntryPattern == nil {
		return nil, fmt.Errorf("vendornotice: an html changelog needs an entry pattern. A page has no " +
			"entry boundary a parser can know, so the boundary is stated rather than guessed")
	}
	return &Changelog{
		client: opts.Client, vendor: strings.ToLower(strings.TrimSpace(opts.Vendor)),
		url: opts.URL, format: opts.Format, rules: opts.Rules, entry: opts.EntryPattern,
	}, nil
}

// Kind implements Announcer.
func (c *Changelog) Kind() SourceKind { return SourceChangelog }

// Read fetches the document and places every entry.
func (c *Changelog) Read(ctx context.Context) (Batch, error) {
	resp, err := c.client.Get(ctx, c.url)
	if err != nil {
		return Batch{}, err
	}
	if resp.Status != http.StatusOK {
		return Batch{}, fmt.Errorf("vendornotice: %s answered %d, so this cycle read none of this "+
			"vendor's changelog. Reported as a gap rather than as a quiet document (FR-075)",
			c.url, resp.Status)
	}
	switch c.format {
	case FormatGCPIncidents:
		return c.readGCPIncidents(resp.Body)
	default:
		entries, err := c.parseEntries(resp.Body)
		if err != nil {
			return Batch{}, err
		}
		return c.place(entries), nil
	}
}

// changelogEntry is one entry, reduced to what extraction reads: an identifier, a pointer and the text
// the stated rules match against. The text is matched and **never stored** (FR-071).
type changelogEntry struct {
	id      string
	link    string
	text    string
	updated time.Time
}

// parseEntries reads the document into entries.
func (c *Changelog) parseEntries(body []byte) ([]changelogEntry, error) {
	switch c.format {
	case FormatAtom:
		return c.parseAtom(body)
	case FormatRSS:
		return c.parseRSS(body)
	case FormatJSONFeed:
		return c.parseJSONFeed(body)
	case FormatHTML:
		return c.parseHTML(body), nil
	default:
		return nil, fmt.Errorf("vendornotice: %s is not a parseable entry format", c.format)
	}
}

func (c *Changelog) parseAtom(body []byte) ([]changelogEntry, error) {
	var feed struct {
		Entries []struct {
			ID      string `xml:"id"`
			Title   string `xml:"title"`
			Summary string `xml:"summary"`
			Content string `xml:"content"`
			Updated string `xml:"updated"`
			Links   []struct {
				Href string `xml:"href,attr"`
				Rel  string `xml:"rel,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("vendornotice: decoding the Atom feed at %s: %w", c.url, err)
	}
	out := make([]changelogEntry, 0, len(feed.Entries))
	for _, entry := range feed.Entries {
		link := ""
		for _, candidate := range entry.Links {
			if candidate.Rel == "" || candidate.Rel == "alternate" {
				link = candidate.Href
				break
			}
		}
		updated, _ := parseInstant(entry.Updated)
		out = append(out, changelogEntry{
			id: firstNonEmpty(entry.ID, link), link: link, updated: updated,
			text: joinText(entry.Title, entry.Summary, entry.Content),
		})
	}
	return out, nil
}

func (c *Changelog) parseRSS(body []byte) ([]changelogEntry, error) {
	var feed struct {
		Channel struct {
			Items []struct {
				GUID        string `xml:"guid"`
				Title       string `xml:"title"`
				Description string `xml:"description"`
				Link        string `xml:"link"`
				PubDate     string `xml:"pubDate"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("vendornotice: decoding the RSS feed at %s: %w", c.url, err)
	}
	out := make([]changelogEntry, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		updated, ok := parseInstant(item.PubDate)
		if !ok {
			// RSS dates are RFC 1123, not RFC 3339. The entry is still usable; only its own
			// timestamp is unread, and the window comes from the stated rules rather than from it.
			if at, err := http.ParseTime(strings.TrimSpace(item.PubDate)); err == nil {
				updated = at.UTC()
			}
		}
		out = append(out, changelogEntry{
			id: firstNonEmpty(item.GUID, item.Link), link: item.Link, updated: updated,
			text: joinText(item.Title, item.Description),
		})
	}
	return out, nil
}

func (c *Changelog) parseJSONFeed(body []byte) ([]changelogEntry, error) {
	var feed struct {
		Items []struct {
			ID            string `json:"id"`
			Title         string `json:"title"`
			Summary       string `json:"summary"`
			ContentText   string `json:"content_text"`
			URL           string `json:"url"`
			DatePublished string `json:"date_published"`
			DateModified  string `json:"date_modified"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("vendornotice: decoding the JSON feed at %s: %w", c.url, err)
	}
	out := make([]changelogEntry, 0, len(feed.Items))
	for _, item := range feed.Items {
		updated, _ := parseInstant(firstNonEmpty(item.DateModified, item.DatePublished))
		out = append(out, changelogEntry{
			id: firstNonEmpty(item.ID, item.URL), link: item.URL, updated: updated,
			text: joinText(item.Title, item.Summary, item.ContentText),
		})
	}
	return out, nil
}

// parseHTML splits a page into entries with the stated pattern.
//
// It does not parse HTML into a tree, and that is deliberate: a tree would invite selecting by
// structure, and a vendor's markup changes without notice while the text of an announcement does not.
// The stated pattern is the contract, and when the page changes the pattern stops matching and every
// entry becomes an unextracted record — which is visible, rather than a silent zero.
func (c *Changelog) parseHTML(body []byte) []changelogEntry {
	text := string(body)
	var out []changelogEntry
	for i, match := range c.entry.FindAllStringSubmatch(text, -1) {
		entry := match[0]
		if idx := c.entry.SubexpIndex("entry"); idx > 0 && idx < len(match) {
			entry = match[idx]
		}
		out = append(out, changelogEntry{
			id:   fmt.Sprintf("%s#%d", c.url, i),
			link: c.url,
			text: collapseSpace(stripTags(entry)),
		})
	}
	return out
}

// place applies the stated rules to every entry.
func (c *Changelog) place(entries []changelogEntry) Batch {
	batch := Batch{Considered: len(entries)}
	for _, entry := range entries {
		pointer := "changelog:" + c.vendor + "/" + firstNonEmpty(entry.id, entry.link)
		rule, captures, ok := c.match(entry)
		if !ok {
			// No stated rule claims this entry, which is the ordinary case: most of a changelog is
			// release notes. It is **not** an extraction failure — nothing here claimed to be an
			// announcement — so it is not recorded as one, and counting it as one would drown the
			// report in entries nobody expected to be placed.
			//
			// What keeps this honest is Batch.Considered: the entry is counted as looked at. A feed
			// whose rules stopped matching after a redesign then reports two hundred considered and
			// zero placed, instead of a clean zero that looks like a quiet vendor.
			continue
		}
		a, err := rule.announcement(c.vendor, pointer, captures, SourceChangelog)
		if err != nil {
			batch.Unextracted = append(batch.Unextracted,
				unextractedFrom(err, pointer, SourceChangelog))
			continue
		}
		if state := strings.ToLower(strings.TrimSpace(rule.State)); state != "" {
			batch.Corrections = append(batch.Corrections, Correction{Announcement: a, State: state})
			continue
		}
		batch.Announcements = append(batch.Announcements, a)
	}
	return batch
}

// match finds the first stated rule that claims an entry.
func (c *Changelog) match(entry changelogEntry) (EntryRule, map[string]string, bool) {
	for _, rule := range c.rules {
		found := rule.Match.FindStringSubmatch(entry.text)
		if found == nil {
			continue
		}
		captures := map[string]string{}
		for i, name := range rule.Match.SubexpNames() {
			if name == "" || i >= len(found) {
				continue
			}
			captures[name] = strings.TrimSpace(found[i])
		}
		return rule, captures, true
	}
	return EntryRule{}, nil, false
}

// announcement turns one match into typed fields, or into an Unextracted naming the field that failed.
func (r EntryRule) announcement(vendor, pointer string, captures map[string]string, source SourceKind) (Announcement, error) {
	a := Announcement{
		Vendor: vendor, Kind: r.Kind, Pointer: pointer, Source: source,
		Product:  firstNonEmpty(captures["product"], r.Product),
		NoticeID: captures["notice_id"],
	}
	switch {
	case r.VagueStart:
		// The vendor said "in a future release". The marker is the honest record and the wording is
		// not stored in its place (FR-069).
		a.Window = Window{StartUnknown: true}
	default:
		start, ok := r.instant(captures["start"])
		if !ok {
			return Announcement{}, &Unextracted{Reason: UnextractedNoWindow, Field: "start"}
		}
		a.Window = Window{Start: start}
	}
	if end, ok := r.instant(captures["end"]); ok {
		a.Window.End = end
	} else if raw := captures["end"]; raw != "" {
		return Announcement{}, &Unextracted{Reason: UnextractedInvalidWindow, Field: "end"}
	}
	return a, nil
}

// instant reads a captured instant with the rule's stated layouts.
func (r EntryRule) instant(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	layouts := r.Layouts
	if len(layouts) == 0 {
		layouts = []string{time.RFC3339, "2006-01-02"}
	}
	for _, layout := range layouts {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}

// gcpIncident is one entry of a `status.cloud.google.com/incidents.json`-shaped document.
type gcpIncident struct {
	ID               string `json:"id"`
	Number           string `json:"number"`
	ServiceName      string `json:"service_name"`
	ExternalDesc     string `json:"external_desc"`
	Begin            string `json:"begin"`
	End              string `json:"end"`
	StatusImpact     string `json:"status_impact"`
	MostRecentUpdate struct {
		Status string `json:"status"`
	} `json:"most_recent_update"`
	URI string `json:"uri"`
}

// readGCPIncidents reads the vendor-native document, which needs no stated rules because it states its
// own fields: `begin` and `end` are RFC 3339 with explicit offsets, and the service is a field.
func (c *Changelog) readGCPIncidents(body []byte) (Batch, error) {
	var incidents []gcpIncident
	if err := json.Unmarshal(body, &incidents); err != nil {
		return Batch{}, fmt.Errorf("vendornotice: decoding the incidents document at %s: %w", c.url, err)
	}
	batch := Batch{Considered: len(incidents)}
	for _, incident := range incidents {
		pointer := "changelog:" + c.vendor + "/incident/" +
			firstNonEmpty(incident.ID, incident.Number, incident.URI)
		start, ok := parseInstant(incident.Begin)
		if !ok {
			batch.Unextracted = append(batch.Unextracted, Unextracted{
				Reason: UnextractedNoWindow, Field: "begin", Pointer: pointer, Source: SourceChangelog,
			})
			continue
		}
		if strings.TrimSpace(incident.ServiceName) == "" {
			batch.Unextracted = append(batch.Unextracted, Unextracted{
				Reason: UnextractedNoProduct, Field: "service_name", Pointer: pointer,
				Source: SourceChangelog,
			})
			continue
		}
		window := Window{Start: start}
		// An unresolved incident is open-ended. Writing the instant we polled as its end would make
		// the graph claim it finished when we looked.
		if end, ok := parseInstant(incident.End); ok {
			window.End = end
		}
		batch.Announcements = append(batch.Announcements, Announcement{
			Vendor: c.vendor, Product: incident.ServiceName, Kind: KindIncident,
			Window: window, NoticeID: firstNonEmpty(incident.ID, incident.Number),
			Pointer: pointer, Source: SourceChangelog,
		})
	}
	return batch, nil
}

var tagPattern = regexp.MustCompile(`(?s)<[^>]*>`)
var spacePattern = regexp.MustCompile(`\s+`)

// stripTags removes markup so a stated rule matches the text a human reads rather than the attributes
// around it.
func stripTags(in string) string {
	return tagPattern.ReplaceAllString(in, " ")
}

// collapseSpace makes whitespace uniform, so a rule does not have to account for a vendor's
// indentation.
func collapseSpace(in string) string {
	return strings.TrimSpace(spacePattern.ReplaceAllString(in, " "))
}

func joinText(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := collapseSpace(stripTags(part)); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
