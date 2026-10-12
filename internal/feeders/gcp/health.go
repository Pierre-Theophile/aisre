// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Personalized Service Health (T191).
//
// `projects.locations.events.list` is Google's own list of the incidents it relates to this project,
// with how it relates them (`relevance`) and the products and locations they touched. It is the status
// page filtered by Google to what concerns the project, and structured.
//
// An event becomes the change the vendor-notice connector makes of a status-feed incident, built by
// that connector's own builder: a VENDOR_INCIDENT on the `google-cloud` vendor node, its window the
// event's, its products and regions on the announcement. One builder means one set of announcement
// rules (FR-062–FR-066) and one place an incident attaches, whichever reader saw it.
//
//   - relevance NOT_IMPACTED and UNKNOWN are not emitted: Google says the project was not, or may not
//     have been, touched, and an incident in an investigation's candidate set has to earn its place.
//   - a MERGED event is not emitted: its parent carries it.
//   - a FALSE_POSITIVE event is CANCELLED: it was announced, and Google withdrew it.
//   - every other event is ANNOUNCED, as a status-feed incident is. That it ended is the window's end,
//     not a promotion (FR-066).
//
// The title and the description are prose and are never read into anything.

// PayloadHealthEvents is a `projects.locations.events.list` response.
const PayloadHealthEvents = "health_events"

// GoogleCloudVendor is the vendor a Service Health event is attributed to: the vendor-notice
// connector's own entry, so both readers attach to one node.
var GoogleCloudVendor = vendornotice.Vendor{Slug: "google-cloud", Name: "Google Cloud"}

type healthEvent struct {
	Name          string `json:"name"`
	DetailedState string `json:"detailedState"`
	Relevance     string `json:"relevance"`
	StartTime     string `json:"startTime"`
	EndTime       string `json:"endTime"`
	EventImpacts  []struct {
		Product struct {
			ProductName string `json:"productName"`
		} `json:"product"`
		Location struct {
			LocationName string `json:"locationName"`
		} `json:"location"`
	} `json:"eventImpacts"`
}

// relevantToProject is the relevance an event must have to be emitted.
var relevantToProject = map[string]bool{"IMPACTED": true, "RELATED": true, "PARTIALLY_RELATED": true}

// HealthAnnouncement turns one event into the announcement the vendor-notice builder takes, or false
// when the event is not emitted.
func HealthAnnouncement(raw json.RawMessage) (vendornotice.Announcement, graphv1.AnnouncementState, bool, error) {
	var e healthEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return vendornotice.Announcement{}, 0, false, fmt.Errorf("gcp: decoding a Service Health event: %w", err)
	}
	if !relevantToProject[e.Relevance] || e.DetailedState == "MERGED" {
		return vendornotice.Announcement{}, 0, false, nil
	}
	start, err := time.Parse(time.RFC3339Nano, e.StartTime)
	if err != nil {
		return vendornotice.Announcement{}, 0, false, fmt.Errorf("gcp: Service Health event %s has no readable start: %w", e.Name, err)
	}
	window := vendornotice.Window{Start: start.UTC()}
	if end, err := time.Parse(time.RFC3339Nano, e.EndTime); err == nil {
		window.End = end.UTC()
	}
	var products, locations []string
	seen := map[string]bool{}
	for _, impact := range e.EventImpacts {
		if p := strings.TrimSpace(impact.Product.ProductName); p != "" && !seen[p] {
			seen[p] = true
			products = append(products, p)
		}
		locations = append(locations, impact.Location.LocationName)
	}
	if len(products) == 0 {
		products = []string{"Multiple Products"}
	}
	regions := vendornotice.RegionsOf(locations)
	id := e.Name[strings.LastIndex(e.Name, "/")+1:]
	a := vendornotice.Announcement{
		Vendor: GoogleCloudVendor.Slug, Product: products[0], AlternateProducts: products[1:],
		Kind: vendornotice.KindIncident, Window: window, NoticeID: "servicehealth-" + id,
		Pointer: "servicehealth:" + e.Name, Source: vendornotice.SourceStatusPage,
		Locations: regions, AffectedResources: regions,
	}
	state := graphv1.AnnouncementState_ANNOUNCED
	if e.DetailedState == "FALSE_POSITIVE" {
		state = graphv1.AnnouncementState_CANCELLED
	}
	return a, state, true, nil
}

// applyHealthEvents maps one events list into vendor incidents.
func (f *Feeder) applyHealthEvents(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadHealthEvents, err)
	}
	for _, event := range payload.Events {
		a, state, emitted, err := HealthAnnouncement(event)
		if err != nil {
			return err
		}
		if !emitted {
			continue
		}
		if err := f.emitGoogleCloudVendor(ctx, desc, em, at); err != nil {
			return err
		}
		build := vendornotice.Announced
		if state == graphv1.AnnouncementState_CANCELLED {
			build = vendornotice.Cancelled
		}
		change, err := build(a, GoogleCloudVendor, at)
		if err != nil {
			return err
		}
		if err := emit(ctx, em, feeder.ObserveChange(desc, feeder.NewID(desc.SourceID, "change",
			change.Ref.GetValue(), state.String()), change)); err != nil {
			return err
		}
	}
	return nil
}

// emitGoogleCloudVendor emits the vendor node once per run, so an incident has something to attach to
// whether or not the vendor-notice connector is running.
func (f *Feeder) emitGoogleCloudVendor(ctx context.Context, desc feeder.Description, em feeder.Emitter, at time.Time) error {
	f.mu.Lock()
	done := f.vendorEmitted
	f.vendorEmitted = true
	f.mu.Unlock()
	if done {
		return nil
	}
	node, err := vendornotice.VendorNode(GoogleCloudVendor, at)
	if err != nil {
		return err
	}
	node.SourceObservedAt = at
	return emit(ctx, em, feeder.UpsertNode(desc, feeder.NewID(desc.SourceID, "vendor", GoogleCloudVendor.Slug), node))
}
