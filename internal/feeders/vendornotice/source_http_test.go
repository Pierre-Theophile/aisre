// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
)

// The two HTTP sources (T080, T081, FR-073, FR-074, FR-075).

const statuspageSummary = `{"page":{"id":"abc123","name":"Acme GPU"},"status":{"indicator":"none"}}`

func statuspageServer(t *testing.T, routes map[string]func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, handler := range routes {
		write := handler
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { write(w) })
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func statuspageSource(t *testing.T, base string) *vn.Statuspage {
	t.Helper()
	list, err := vn.NewAllowlist([]vn.Vendor{testVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	source, err := vn.NewStatuspage(vn.StatuspageOptions{
		Client: vn.NewClient(vn.ClientOptions{}), Allowlist: list,
		Vendor: "acme-gpu", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("NewStatuspage: %v", err)
	}
	return source
}

// A scheduled maintenance becomes an announced fact keyed on scheduled_for and scheduled_until.
func TestAScheduledMaintenanceBecomesAnAnnouncement(t *testing.T) {
	t.Parallel()

	server := statuspageServer(t, map[string]func(http.ResponseWriter){
		"/api/v2/summary.json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(statuspageSummary)) },
		"/api/v2/scheduled-maintenances.json": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"scheduled_maintenances":[{
				"id":"m1","name":"Inference API maintenance","status":"scheduled",
				"scheduled_for":"2026-10-02T02:00:00Z","scheduled_until":"2026-10-02T04:00:00Z",
				"components":[{"name":"console"},{"name":"inference-api"}]}]}`))
		},
		"/api/v2/incidents.json": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"incidents":[]}`))
		},
	})

	batch, err := statuspageSource(t, server.URL).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %d announcements: %+v", len(batch.Announcements), batch)
	}
	got := batch.Announcements[0]
	if got.Kind != vn.KindMaintenance {
		t.Errorf("kind = %q", got.Kind)
	}
	if !got.Window.Start.Equal(time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)) ||
		!got.Window.End.Equal(time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %+v, want the stated scheduled_for and scheduled_until", got.Window)
	}
	// The product is the component the allowlist allows, not the first one listed: a notice whose
	// product depended on the API's ordering would be dropped or kept by coincidence.
	if got.Product != "inference-api" {
		t.Errorf("product = %q, want the allowlisted component rather than the first listed", got.Product)
	}
	if len(got.AffectedResources) != 1 || got.AffectedResources[0] != "console" {
		t.Errorf("the other stated components were lost: %v", got.AffectedResources)
	}
	if got.NoticeID != "m1" {
		t.Errorf("notice id = %q, want the vendor's own identifier so two sources merge", got.NoticeID)
	}
	if batch.Considered != 1 {
		t.Errorf("considered = %d, want the one maintenance looked at", batch.Considered)
	}
}

// The probe is the point of this adapter. Stripe and GitLab 404 on these paths and
// `status.cloud.microsoft` 401s: a source that read that as "the vendor announced nothing" would
// report a clean poll of a page that does not exist (FR-075).
func TestANonStatuspageIsAGapAndNotAQuietPage(t *testing.T) {
	t.Parallel()

	for name, probe := range map[string]func(http.ResponseWriter){
		"a 404 because the vendor does not use Statuspage": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
		},
		"a 401 because the page is private": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnauthorized)
		},
		"a 200 that is not a Statuspage summary": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`<html><body>status</body></html>`))
		},
	} {
		server := statuspageServer(t, map[string]func(http.ResponseWriter){
			"/api/v2/summary.json": probe,
			"/api/v2/scheduled-maintenances.json": func(w http.ResponseWriter) {
				t.Errorf("%s: the source read the maintenance list after the probe failed", name)
				_, _ = w.Write([]byte(`{"scheduled_maintenances":[]}`))
			},
		})
		source := statuspageSource(t, server.URL)
		batch, err := source.Read(context.Background())
		if err == nil {
			t.Errorf("%s: Read reported %+v rather than refusing; an empty poll of a page that does "+
				"not exist is the failure this feeder exists to end", name, batch)
			continue
		}
		if !strings.Contains(err.Error(), "gap") {
			t.Errorf("%s: the refusal does not say it becomes a gap: %v", name, err)
		}
		// Every cycle, not once. A source that reports the finding and then goes quiet is a source
		// whose absence nobody sees, and the configuration error stays true until somebody fixes it.
		if _, second := source.Read(context.Background()); second == nil {
			t.Errorf("%s: the second cycle reported success; the finding was cached and the gap "+
				"stopped being reported", name)
		}
	}
}

// A maintenance with no usable window is unextracted naming the field, not a guessed one (FR-073).
func TestAMaintenanceWithNoUsableWindowIsUnextracted(t *testing.T) {
	t.Parallel()

	server := statuspageServer(t, map[string]func(http.ResponseWriter){
		"/api/v2/summary.json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(statuspageSummary)) },
		"/api/v2/scheduled-maintenances.json": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"scheduled_maintenances":[{"id":"m2","scheduled_for":"soon",
				"components":[{"name":"inference-api"}]}]}`))
		},
		"/api/v2/incidents.json": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"incidents":[]}`))
		},
	})

	batch, err := statuspageSource(t, server.URL).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("an unparseable window produced %+v", batch.Announcements)
	}
	if len(batch.Unextracted) != 1 || batch.Unextracted[0].Field != "scheduled_for" {
		t.Fatalf("unextracted = %+v, want one naming scheduled_for", batch.Unextracted)
	}
	if batch.Unextracted[0].Pointer == "" {
		t.Error("the record carries no pointer, so nobody can open the original (FR-074)")
	}
}

// An unresolved incident is open-ended. Writing the instant we polled as its end would make the graph
// claim it finished when we looked.
func TestAnUnresolvedIncidentIsOpenEnded(t *testing.T) {
	t.Parallel()

	server := statuspageServer(t, map[string]func(http.ResponseWriter){
		"/api/v2/summary.json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(statuspageSummary)) },
		"/api/v2/scheduled-maintenances.json": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"scheduled_maintenances":[]}`))
		},
		"/api/v2/incidents.json": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"incidents":[{"id":"i1","status":"investigating",
				"created_at":"2026-09-20T22:14:00Z","resolved_at":null,
				"components":[{"name":"inference-api"}]}]}`))
		},
	})

	batch, err := statuspageSource(t, server.URL).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %+v", batch)
	}
	got := batch.Announcements[0]
	if got.Kind != vn.KindIncident {
		t.Errorf("kind = %q, want an incident", got.Kind)
	}
	if !got.Window.End.IsZero() {
		t.Errorf("an unresolved incident ends at %s", got.Window.End)
	}
	if got.Window.StartUnknown {
		t.Error("the incident's start is marked unknown although the page stated it")
	}
}

// A status page configured for a vendor nobody allowlisted would be polled for nothing, so it is
// refused at construction rather than every cycle.
func TestAStatuspageForAnUnallowlistedVendorIsRefused(t *testing.T) {
	t.Parallel()

	list, err := vn.NewAllowlist([]vn.Vendor{testVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	_, err = vn.NewStatuspage(vn.StatuspageOptions{
		Client: vn.NewClient(vn.ClientOptions{}), Allowlist: list,
		Vendor: "someone-else", BaseURL: "https://status.someone-else.test",
	})
	if err == nil {
		t.Fatal("a status page for a vendor nobody allowlisted was accepted")
	}
}

func changelogSource(t *testing.T, url string, format vn.ChangelogFormat, opts vn.ChangelogOptions) *vn.Changelog {
	t.Helper()
	opts.Client, opts.Vendor, opts.URL, opts.Format = vn.NewClient(vn.ClientOptions{}), "acme-gpu", url, format
	source, err := vn.NewChangelog(opts)
	if err != nil {
		t.Fatalf("NewChangelog: %v", err)
	}
	return source
}

func serve(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A stated rule places an Atom entry. What is asserted is that the *rule* did it: nothing here reads a
// title and decides what it means.
func TestAStatedRulePlacesAnAtomEntry(t *testing.T) {
	t.Parallel()

	url := serve(t, `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
		<entry><id>tag:acme,2026:1</id><title>Deprecating inference-api v1 on 2026-10-02</title>
		<link href="https://acme-gpu.test/changelog/1"/><updated>2026-09-17T10:00:00Z</updated></entry>
		<entry><id>tag:acme,2026:2</id><title>New dashboard colours</title>
		<link href="https://acme-gpu.test/changelog/2"/><updated>2026-09-16T10:00:00Z</updated></entry>
	</feed>`)

	source := changelogSource(t, url, vn.FormatAtom, vn.ChangelogOptions{Rules: []vn.EntryRule{{
		Match: regexp.MustCompile(`Deprecating (?P<product>[a-z-]+) v\d+ on (?P<start>\d{4}-\d{2}-\d{2})`),
		Kind:  vn.KindDeprecation,
	}}})
	batch, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %d announcements: %+v", len(batch.Announcements), batch)
	}
	got := batch.Announcements[0]
	if got.Product != "inference-api" || got.Kind != vn.KindDeprecation {
		t.Errorf("placed as product=%q kind=%q", got.Product, got.Kind)
	}
	if !got.Window.Start.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %s", got.Window.Start)
	}
	// The entry no rule claimed is counted as looked at and placed as nothing. That count is what
	// keeps a feed whose rules stopped matching from reporting a clean zero for ever.
	if batch.Considered != 2 {
		t.Errorf("considered = %d, want both entries", batch.Considered)
	}
	if len(batch.Unextracted) != 0 {
		t.Errorf("an entry that never claimed to be an announcement was recorded as a failure: %+v",
			batch.Unextracted)
	}
}

// A rule whose window capture does not parse is an unextracted record naming the field, not an
// announcement with a guessed instant (FR-073).
func TestARuleThatCapturesAnUnparseableWindowIsUnextracted(t *testing.T) {
	t.Parallel()

	url := serve(t, `<rss version="2.0"><channel><item>
		<guid>acme-9</guid><title>Deprecating inference-api on the 32nd of Octember</title>
		<link>https://acme-gpu.test/changelog/9</link>
		<pubDate>Thu, 17 Sep 2026 10:00:00 GMT</pubDate></item></channel></rss>`)

	source := changelogSource(t, url, vn.FormatRSS, vn.ChangelogOptions{Rules: []vn.EntryRule{{
		Match: regexp.MustCompile(`Deprecating (?P<product>[a-z-]+) on (?P<start>.+)$`),
		Kind:  vn.KindDeprecation,
	}}})
	batch, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("a rule captured %q and produced %+v", "the 32nd of Octember", batch.Announcements)
	}
	if len(batch.Unextracted) != 1 || batch.Unextracted[0].Field != "start" {
		t.Fatalf("unextracted = %+v, want one naming the start", batch.Unextracted)
	}
}

// A vendor who states no date states no date. The rule says so, the valid start is marked unknown, and
// the wording is not stored in its place (FR-069).
func TestAVagueRuleMarksTheStartUnknownAndStoresNoWording(t *testing.T) {
	t.Parallel()

	url := serve(t, `{"items":[{"id":"jf-1","title":"inference-api v1 will be removed in a future release",
		"url":"https://acme-gpu.test/changelog/jf-1","date_published":"2026-09-17T10:00:00Z"}]}`)

	source := changelogSource(t, url, vn.FormatJSONFeed, vn.ChangelogOptions{Rules: []vn.EntryRule{{
		Match:      regexp.MustCompile(`(?P<product>[a-z-]+) v\d+ will be removed in a future release`),
		Kind:       vn.KindDeprecation,
		VagueStart: true,
	}}})
	batch, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %+v", batch)
	}
	got := batch.Announcements[0]
	if !got.Window.StartUnknown || !got.Window.Start.IsZero() {
		t.Errorf("window = %+v, want a marked-unknown start", got.Window)
	}
	// And nothing anywhere on the announcement holds the vendor's sentence.
	for _, field := range []string{got.Product, got.NoticeID, got.Pointer, string(got.Kind)} {
		if strings.Contains(strings.ToLower(field), "future release") {
			t.Errorf("the vendor's wording was stored in %q (FR-069, FR-071)", field)
		}
	}
}

// HTML is read the same way as a feed, through stated rules. It is a first-class source because a large
// share of vendors publish deprecations on a page and nowhere else.
func TestAWebPageIsReadThroughTheSameStatedRules(t *testing.T) {
	t.Parallel()

	url := serve(t, `<html><body>
		<ul class="changelog">
		<li class="entry"><h3>Scheduled maintenance</h3><p>inference-api unavailable from
		2026-10-02 to 2026-10-03</p></li>
		<li class="entry"><h3>Unrelated</h3><p>We redesigned the console.</p></li>
		</ul></body></html>`)

	source := changelogSource(t, url, vn.FormatHTML, vn.ChangelogOptions{
		EntryPattern: regexp.MustCompile(`(?s)<li class="entry">(?P<entry>.*?)</li>`),
		Rules: []vn.EntryRule{{
			Match: regexp.MustCompile(
				`(?P<product>[a-z-]+) unavailable from (?P<start>\d{4}-\d{2}-\d{2}) to (?P<end>\d{4}-\d{2}-\d{2})`),
			Kind: vn.KindMaintenance,
		}},
	})
	batch, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if batch.Considered != 2 {
		t.Errorf("considered = %d, want both list items", batch.Considered)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %d announcements: %+v", len(batch.Announcements), batch)
	}
	got := batch.Announcements[0]
	if got.Product != "inference-api" || got.Window.End.IsZero() {
		t.Errorf("the page's entry was placed as %+v", got)
	}
	if !strings.HasPrefix(got.Pointer, "changelog:acme-gpu/") {
		t.Errorf("pointer = %q, want one a human can follow back to the page", got.Pointer)
	}
}

// The vendor-native document states its own fields, so it needs no rules: `begin` and `end` are RFC
// 3339 with explicit offsets and the service is a field rather than a sentence.
func TestTheVendorNativeDocumentNeedsNoStatedRules(t *testing.T) {
	t.Parallel()

	url := serve(t, `[{"id":"inc-1","number":"24001","service_name":"inference-api",
		"begin":"2026-09-20T22:14:00+02:00","end":"2026-09-20T23:40:00+02:00",
		"status_impact":"SERVICE_DISRUPTION","uri":"incidents/inc-1"},
		{"id":"inc-2","service_name":"","begin":"2026-09-19T10:00:00Z"}]`)

	source := changelogSource(t, url, vn.FormatGCPIncidents, vn.ChangelogOptions{})
	batch, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %d announcements: %+v", len(batch.Announcements), batch)
	}
	got := batch.Announcements[0]
	if got.Product != "inference-api" || got.Kind != vn.KindIncident {
		t.Errorf("placed as %+v", got)
	}
	// The explicit offset is read, not dropped: 22:14+02:00 is 20:14Z, and reading it as 22:14Z would
	// put the incident two hours after it happened.
	if !got.Window.Start.Equal(time.Date(2026, 9, 20, 20, 14, 0, 0, time.UTC)) {
		t.Errorf("start = %s, want the stated offset applied", got.Window.Start)
	}
	// And the one with no service is unextracted naming the field rather than attributed to a guess.
	if len(batch.Unextracted) != 1 || batch.Unextracted[0].Field != "service_name" {
		t.Fatalf("unextracted = %+v", batch.Unextracted)
	}
}

// A changelog with no stated rules would read every entry and place none. Refused at construction: the
// alternative is a feeder interpreting prose, which is what FR-073 forbids.
func TestAChangelogWithNoRulesIsRefused(t *testing.T) {
	t.Parallel()

	client := vn.NewClient(vn.ClientOptions{})
	for _, format := range []vn.ChangelogFormat{vn.FormatAtom, vn.FormatRSS, vn.FormatJSONFeed, vn.FormatHTML} {
		_, err := vn.NewChangelog(vn.ChangelogOptions{
			Client: client, Vendor: "acme-gpu", URL: "https://acme-gpu.test/changelog", Format: format,
		})
		if err == nil {
			t.Errorf("a %s changelog with no rules was accepted", format)
		}
	}
	// A rule with no kind is refused too: there is no default, because the kind decides the change
	// kind the graph records.
	_, err := vn.NewChangelog(vn.ChangelogOptions{
		Client: client, Vendor: "acme-gpu", URL: "https://acme-gpu.test/changelog", Format: vn.FormatAtom,
		Rules: []vn.EntryRule{{Match: regexp.MustCompile(`x`)}},
	})
	if err == nil {
		t.Error("a rule announcing no kind was accepted")
	}
	// And an HTML changelog with no entry boundary: a page has none a parser can know.
	_, err = vn.NewChangelog(vn.ChangelogOptions{
		Client: client, Vendor: "acme-gpu", URL: "https://acme-gpu.test/changelog", Format: vn.FormatHTML,
		Rules: []vn.EntryRule{{Match: regexp.MustCompile(`x`), Kind: vn.KindDeprecation}},
	})
	if err == nil {
		t.Error("an html changelog with no entry pattern was accepted")
	}
}

// A document that cannot be fetched is a gap, not an empty changelog.
func TestAChangelogThatAnswersAnErrorIsAGap(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	source := changelogSource(t, server.URL, vn.FormatAtom, vn.ChangelogOptions{Rules: []vn.EntryRule{{
		Match: regexp.MustCompile(`x`), Kind: vn.KindDeprecation,
	}}})
	if _, err := source.Read(context.Background()); err == nil {
		t.Fatal("a 500 was read as a changelog with nothing in it")
	}
}
