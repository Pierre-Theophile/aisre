// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

const healthEvents = `{"events":[
 {"name":"projects/proj-a/locations/global/events/e1","category":"INCIDENT","detailedState":"RESOLVED","relevance":"RELATED",
  "startTime":"2026-05-28T15:31:40Z","endTime":"2026-05-28T15:45:53.676323Z",
  "eventImpacts":[{"product":{"productName":"Cloud Run"},"location":{"locationName":"europe-west1"}},
                  {"product":{"productName":"Cloud Run"},"location":{"locationName":"europe-west1-b"}},
                  {"product":{"productName":"Google Cloud SQL"},"location":{"locationName":"europe-west1-c"}}]},
 {"name":"projects/proj-a/locations/global/events/e2","detailedState":"FALSE_POSITIVE","relevance":"PARTIALLY_RELATED",
  "startTime":"2026-06-01T10:00:00Z","eventImpacts":[{"product":{"productName":"Cloud Logging"},"location":{"locationName":"global"}}]},
 {"name":"projects/proj-a/locations/global/events/e3","detailedState":"MERGED","relevance":"RELATED","startTime":"2026-06-02T10:00:00Z"},
 {"name":"projects/proj-a/locations/global/events/e4","detailedState":"RESOLVED","relevance":"NOT_IMPACTED","startTime":"2026-06-03T10:00:00Z"}]}`

// An incident Google relates to the project is the vendor incident the status feed would make of it,
// on the vendor node; one Google says did not touch the project, or merged into another, is not
// emitted; one Google withdrew is cancelled (T191).
func TestServiceHealthEventsBecomeVendorIncidentsOnTheVendor(t *testing.T) {
	t.Parallel()
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "acme",
		Scope:   gcpfeeder.Scope{Projects: []string{pollProject}, Regions: []string{pollRegion}},
		Horizon: gcpfeeder.Horizon{Earliest: pollStart.Add(-365 * 24 * time.Hour), Reason: gcpfeeder.HorizonConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := emit.NewMemoryEmitter(f.Describe())
	payloads := []feeder.Payload{
		{Kind: gcpfeeder.PayloadHealthEvents, At: pollStart, Bytes: []byte(healthEvents)},
		{Kind: gcpfeeder.PayloadPollMarker, At: pollStart, Bytes: []byte(`{"outcome":"complete","projects":["` + pollProject + `"],"regions":["` + pollRegion + `"]}`)},
	}
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), memory); err != nil {
		t.Fatal(err)
	}
	changes := map[string]*graphv1.ObserveChange{}
	vendorNodes := 0
	for _, ev := range memory.Events() {
		if c := ev.GetObserveChange(); c != nil && c.GetChange().GetKind() == graphv1.ChangeKind_VENDOR_INCIDENT {
			changes[c.GetRef().GetValue()] = c
		}
		if n := ev.GetUpsertNode(); n != nil && n.GetType() == graphv1.NodeType_THIRD_PARTY {
			vendorNodes++
		}
	}
	if vendorNodes != 1 {
		t.Errorf("%d vendor nodes emitted, want the google-cloud node once", vendorNodes)
	}
	if len(changes) != 2 {
		t.Fatalf("%d incidents emitted, want 2: the related one and the withdrawn one, not the merged or unrelated", len(changes))
	}
	for ref, c := range changes {
		if len(c.GetTargets()) != 1 || c.GetTargets()[0].GetValue() != "google-cloud" {
			t.Errorf("%s targets %v, want the google-cloud vendor node", ref, c.GetTargets())
		}
		switch {
		case strings.HasSuffix(ref, "servicehealth-e1"):
			if c.GetChange().GetAnnouncementState() != graphv1.AnnouncementState_ANNOUNCED ||
				!c.GetValidAt().AsTime().Equal(time.Date(2026, 5, 28, 15, 31, 40, 0, time.UTC)) {
				t.Errorf("e1 = %s at %s, want ANNOUNCED at its start", c.GetChange().GetAnnouncementState(), c.GetValidAt().AsTime())
			}
		case strings.HasSuffix(ref, "servicehealth-e2"):
			if c.GetChange().GetAnnouncementState() != graphv1.AnnouncementState_CANCELLED {
				t.Errorf("the false positive is %s, want CANCELLED", c.GetChange().GetAnnouncementState())
			}
		default:
			t.Errorf("unexpected incident %s", ref)
		}
	}
	one := []byte(`{"name":"projects/p/locations/global/events/x","detailedState":"RESOLVED","relevance":"IMPACTED",
		"startTime":"2026-05-28T15:31:40Z","eventImpacts":[{"product":{"productName":"Cloud Run"},"location":{"locationName":"europe-west1-b"}},
		{"product":{"productName":"Google Cloud SQL"},"location":{"locationName":"europe-west1"}}]}`)
	a, _, emitted, err := gcpfeeder.HealthAnnouncement(one)
	if err != nil || !emitted {
		t.Fatalf("an impacted event was not emitted: %v", err)
	}
	if strings.Join(a.Locations, ",") != "europe-west1" || a.Product != "Cloud Run" || strings.Join(a.AlternateProducts, ",") != "Google Cloud SQL" {
		t.Errorf("announcement %+v; want the zone reduced to its region and both products named", a)
	}
}
