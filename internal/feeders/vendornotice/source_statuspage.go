// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The Atlassian Statuspage source (T080, FR-075, research §9, contract §1).
//
// Statuspage publishes two endpoints this feeder reads, both unauthenticated JSON:
//
//	/api/v2/scheduled-maintenances.json   keyed on scheduled_for / scheduled_until
//	/api/v2/incidents.json                keyed on created_at / resolved_at
//
// # The capability probe, and why it is not an optimisation
//
// Coverage is **not** universal. Stripe and GitLab return 404 on these paths;
// `status.cloud.microsoft` returns 401. A source that read a 404 as "this vendor has announced
// nothing" would report a clean poll of a page that does not exist — which is FR-075's failure exactly,
// and the one this whole feeder was built to end.
//
// So the source probes `/api/v2/summary.json` first and **refuses to report an empty cycle** when the
// probe says this is not a Statuspage. The refusal is an error from Read, which the poller records as a
// gap, which reaches the checkpoint: the graph is told this source covered nothing.
//
// It refuses **every cycle**, not once. Caching the finding and going quiet afterwards is the tempting
// design and it is wrong: a source that stops reporting is a source whose absence nobody sees, and
// "this is not a Statuspage" stays true until somebody changes the configuration. A gap every cycle is
// noisy on purpose — it is a configuration error, and the noise is what gets it fixed.
//
// # One notice is one announcement
//
// A maintenance may list several components. The announcement's product is the first component the
// allowlist allows, in the order the API listed them; the rest are recorded as affected resources. That
// keeps one notice as one change, which is what the deterministic ref of FR-076 addresses — the
// alternative, one announcement per component, would collapse them all onto one ref anyway and make
// the product depend on which arrived last.
//
// Where no component matches, the first component's name is carried through **unchanged** so the
// feeder's own allowlist decision produces an accurate drop reason. Scope is the feeder's decision and
// this source does not pre-empt it; it consults the allowlist only to pick which of several stated
// products the notice is about.
//
// # What this source cannot tell you
//
// A cancelled maintenance **disappears from the list**. Statuspage has no cancelled state for one:
// the statuses are scheduled, in_progress, verifying and completed. So this source can never report a
// cancellation, and a notice that vanishes between two polls is indistinguishable from one the page
// stopped listing for its own reasons. The mailbox is where a cancellation comes from (FR-067), and
// that is a fact about the coverage of this source rather than a gap in the graph.

// Statuspage reads one vendor's Atlassian Statuspage.
type Statuspage struct {
	client *Client
	list   *Allowlist
	// vendor is the slug this page belongs to. It is **configuration**: a status page belongs to a
	// vendor because somebody said so, never because a hostname resembled one (FR-059).
	vendor string
	base   string
	// incidents says whether to read incidents as well as scheduled maintenances. A deployment that
	// only cares about announced windows can turn it off; the default is on, because an unannounced
	// vendor incident is the other half of what this feeder is for.
	incidents bool
}

// StatuspageOptions configures the adapter.
type StatuspageOptions struct {
	// Client is the shared HTTP client. Required.
	Client *Client
	// Allowlist is used to pick which stated product a notice is about. Required.
	Allowlist *Allowlist
	// Vendor is the allowlisted slug this page belongs to. Required, and configuration.
	Vendor string
	// BaseURL is the page's root, e.g. `https://status.acme-gpu.test`. Required.
	BaseURL string
	// SkipIncidents reads scheduled maintenances only.
	SkipIncidents bool
}

// NewStatuspage builds the adapter.
func NewStatuspage(opts StatuspageOptions) (*Statuspage, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("vendornotice: the status-page source needs an HTTP client")
	}
	if opts.Allowlist == nil {
		return nil, fmt.Errorf("vendornotice: the status-page source needs the allowlist to pick " +
			"which stated product a notice is about")
	}
	if strings.TrimSpace(opts.Vendor) == "" {
		return nil, fmt.Errorf("vendornotice: the status-page source needs the vendor slug the page " +
			"belongs to; a page belongs to a vendor because somebody configured it, never because a " +
			"hostname resembled one")
	}
	if _, known := opts.Allowlist.Vendor(opts.Vendor); !known {
		return nil, fmt.Errorf("vendornotice: the status-page source names vendor %q, which is not "+
			"allowlisted; every announcement it read would be dropped and the page would be polled "+
			"for nothing", opts.Vendor)
	}
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("vendornotice: the status-page source needs a base URL")
	}
	return &Statuspage{
		client: opts.Client, list: opts.Allowlist, vendor: strings.ToLower(opts.Vendor),
		base: base, incidents: !opts.SkipIncidents,
	}, nil
}

// Kind implements Announcer.
func (s *Statuspage) Kind() SourceKind { return SourceStatusPage }

// Read probes, then reads both endpoints.
func (s *Statuspage) Read(ctx context.Context) (Batch, error) {
	if err := s.probe(ctx); err != nil {
		return Batch{}, err
	}
	var batch Batch
	maintenances, err := s.readMaintenances(ctx)
	if err != nil {
		return Batch{}, err
	}
	batch.Announcements = append(batch.Announcements, maintenances.Announcements...)
	batch.Unextracted = append(batch.Unextracted, maintenances.Unextracted...)
	batch.Considered += maintenances.Considered
	if s.incidents {
		incidents, err := s.readIncidents(ctx)
		if err != nil {
			return Batch{}, err
		}
		batch.Announcements = append(batch.Announcements, incidents.Announcements...)
		batch.Unextracted = append(batch.Unextracted, incidents.Unextracted...)
		batch.Considered += incidents.Considered
	}
	return batch, nil
}

// probe asks whether this is a Statuspage at all.
//
// A 200 carrying a `page` object is the only answer that means yes. Anything else — a 404 because the
// vendor does not use Statuspage, a 401 because the page is private, HTML because a proxy answered —
// is a refusal, and the refusal names what came back so that the gap says something actionable.
func (s *Statuspage) probe(ctx context.Context) error {
	url := s.base + "/api/v2/summary.json"
	resp, err := s.client.Get(ctx, url)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return fmt.Errorf("vendornotice: %s answered %d, so this is not an Atlassian Statuspage this "+
			"feeder can read. Reported as a gap rather than an empty poll, every cycle: a 404 read as "+
			"\"the vendor announced nothing\" is a clean report about a page that does not exist "+
			"(FR-075)", url, resp.Status)
	}
	var summary struct {
		Page *struct {
			ID string `json:"id"`
		} `json:"page"`
	}
	if err := json.Unmarshal(resp.Body, &summary); err != nil || summary.Page == nil {
		return fmt.Errorf("vendornotice: %s answered 200 without a Statuspage summary in it, so "+
			"something other than a status page is answering. Reported as a gap rather than an empty "+
			"poll (FR-075)", url)
	}
	return nil
}

// statuspageComponent is one affected component.
type statuspageComponent struct {
	Name string `json:"name"`
}

// statuspageMaintenance is one scheduled maintenance, with only the fields this feeder reads. Every
// field it does not name is body text as far as this feeder is concerned, and body text has no field
// to travel in (FR-071).
type statuspageMaintenance struct {
	ID             string                `json:"id"`
	Name           string                `json:"name"`
	Status         string                `json:"status"`
	ScheduledFor   string                `json:"scheduled_for"`
	ScheduledUntil string                `json:"scheduled_until"`
	Shortlink      string                `json:"shortlink"`
	Components     []statuspageComponent `json:"components"`
}

type statuspageIncident struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	Status     string                `json:"status"`
	CreatedAt  string                `json:"created_at"`
	ResolvedAt string                `json:"resolved_at"`
	Components []statuspageComponent `json:"components"`
}

func (s *Statuspage) readMaintenances(ctx context.Context) (Batch, error) {
	url := s.base + "/api/v2/scheduled-maintenances.json"
	resp, err := s.client.Get(ctx, url)
	if err != nil {
		return Batch{}, err
	}
	if resp.Status != http.StatusOK {
		return Batch{}, fmt.Errorf("vendornotice: %s answered %d", url, resp.Status)
	}
	var body struct {
		ScheduledMaintenances []statuspageMaintenance `json:"scheduled_maintenances"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return Batch{}, fmt.Errorf("vendornotice: decoding %s: %w", url, err)
	}
	batch := Batch{Considered: len(body.ScheduledMaintenances)}
	for _, m := range body.ScheduledMaintenances {
		pointer := "statuspage:" + s.vendor + "/maintenance/" + m.ID
		window, ok := statuspageWindow(m.ScheduledFor, m.ScheduledUntil)
		if !ok {
			batch.Unextracted = append(batch.Unextracted, Unextracted{
				Reason: UnextractedNoWindow, Field: "scheduled_for", Pointer: pointer,
				Source: SourceStatusPage,
			})
			continue
		}
		s.place(&batch, m.Components, Announcement{
			Vendor: s.vendor, Kind: KindMaintenance, Window: window,
			NoticeID: m.ID, Pointer: pointer, Source: SourceStatusPage,
		})
	}
	return batch, nil
}

func (s *Statuspage) readIncidents(ctx context.Context) (Batch, error) {
	url := s.base + "/api/v2/incidents.json"
	resp, err := s.client.Get(ctx, url)
	if err != nil {
		return Batch{}, err
	}
	if resp.Status != http.StatusOK {
		return Batch{}, fmt.Errorf("vendornotice: %s answered %d", url, resp.Status)
	}
	var body struct {
		Incidents []statuspageIncident `json:"incidents"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return Batch{}, fmt.Errorf("vendornotice: decoding %s: %w", url, err)
	}
	batch := Batch{Considered: len(body.Incidents)}
	for _, incident := range body.Incidents {
		pointer := "statuspage:" + s.vendor + "/incident/" + incident.ID
		// An incident's window opens when it started and closes when it was resolved. An unresolved
		// one is open-ended, which is the honest shape: it has not ended yet, and writing "now" as
		// its end would make the graph claim it finished at the instant we polled.
		window, ok := statuspageWindow(incident.CreatedAt, incident.ResolvedAt)
		if !ok {
			batch.Unextracted = append(batch.Unextracted, Unextracted{
				Reason: UnextractedNoWindow, Field: "created_at", Pointer: pointer,
				Source: SourceStatusPage,
			})
			continue
		}
		s.place(&batch, incident.Components, Announcement{
			Vendor: s.vendor, Kind: KindIncident, Window: window,
			NoticeID: incident.ID, Pointer: pointer, Source: SourceStatusPage,
		})
	}
	return batch, nil
}

// place fills in the product and the affected resources from the stated components.
func (s *Statuspage) place(batch *Batch, components []statuspageComponent, a Announcement) {
	names := make([]string, 0, len(components))
	for _, component := range components {
		if name := strings.TrimSpace(component.Name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		// No component at all. The product is left empty and the feeder drops it as
		// `no_product_stated`, which is the accurate reason: a vendor-wide notice attributed to
		// every product would put one window on everything.
		batch.Announcements = append(batch.Announcements, a)
		return
	}
	a.Product = names[0]
	if vendor, known := s.list.Vendor(s.vendor); known {
		for _, name := range names {
			if allowed(vendor, name) {
				a.Product = name
				break
			}
		}
	}
	for _, name := range names {
		if !strings.EqualFold(name, a.Product) {
			a.AffectedResources = append(a.AffectedResources, name)
		}
	}
	batch.Announcements = append(batch.Announcements, a)
}

// allowed reports whether a stated component name is one of the vendor's allowlisted products.
func allowed(vendor Vendor, product string) bool {
	wanted := strings.ToLower(strings.TrimSpace(product))
	for _, candidate := range vendor.Products {
		if strings.ToLower(strings.TrimSpace(candidate)) == wanted {
			return true
		}
	}
	return false
}

// statuspageWindow reads the pair of instants.
//
// A missing or unparseable start is **not** a vague window: Statuspage states an instant or states
// nothing, and a field that should hold RFC 3339 and does not is a parse failure rather than a vendor
// being imprecise. It becomes an unextracted record naming the field, which is what FR-073 asks for —
// the vague-start marker is for a vendor who wrote "in the coming weeks" in prose, and there is no
// prose here.
func statuspageWindow(from, until string) (Window, bool) {
	start, ok := parseInstant(from)
	if !ok {
		return Window{}, false
	}
	window := Window{Start: start}
	if end, ok := parseInstant(until); ok {
		window.End = end
	}
	return window, true
}

// parseInstant reads an RFC 3339 instant, accepting the offset forms vendors actually send.
func parseInstant(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}
