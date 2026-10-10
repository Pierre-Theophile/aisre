// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"testing"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
)

// The shipped allowlist against announcements shaped like the seven Google entries a live run read on
// 2026-10-09, which it dropped every one of. Each now has the reason an operator would give.
func TestTheShippedAllowlistKeepsWhatThisOrganisationRunsOn(t *testing.T) {
	t.Parallel()
	cfg, err := vn.LoadConfig("../../../config/vendors.yaml")
	if err != nil {
		t.Fatal(err)
	}
	list, err := cfg.Allowlist()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		announcement vn.Announcement
		inScope      bool
		product      string
		reason       vn.DropReason
	}{
		{"a global outage naming products we use", vn.Announcement{Vendor: "google-cloud", Product: "Multiple Products",
			AlternateProducts: []string{"Vertex AI", "Cloud Run", "Google Cloud SQL"}, Locations: []string{"global", "us-west1"}},
			true, "cloud run", ""},
		{"a global network incident", vn.Announcement{Vendor: "google-cloud", Product: "Multiple Products",
			AlternateProducts: []string{"Virtual Private Cloud (VPC)"}, Locations: []string{"global"}},
			true, "virtual private cloud (vpc)", ""},
		{"our region", vn.Announcement{Vendor: "google-cloud", Product: "Google Cloud SQL", Locations: []string{"europe-west1"}},
			true, "google cloud sql", ""},
		{"a maintenance mail stating no location", vn.Announcement{Vendor: "google-cloud", Product: "Google Cloud SQL"},
			true, "google cloud sql", ""},
		{"a product we use, elsewhere", vn.Announcement{Vendor: "google-cloud", Product: "Google Cloud Storage",
			Locations: []string{"us-central1"}}, false, "", vn.DropLocationNotAllowlisted},
		{"products we do not use", vn.Announcement{Vendor: "google-cloud", Product: "Multiple Products",
			AlternateProducts: []string{"Vertex AI Gemini API"}, Locations: []string{"global"}}, false, "", vn.DropProductNotAllowlisted},
		{"a product we do not use", vn.Announcement{Vendor: "google-cloud", Product: "VMware Engine",
			Locations: []string{"europe-west1"}}, false, "", vn.DropProductNotAllowlisted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := list.DecideAnnouncement(tc.announcement)
			if got.InScope != tc.inScope || (tc.inScope && got.Product != tc.product) || (!tc.inScope && got.Drop.Reason != tc.reason) {
				t.Errorf("decision = %+v; want in scope %t product %q reason %q", got, tc.inScope, tc.product, tc.reason)
			}
		})
	}
}
