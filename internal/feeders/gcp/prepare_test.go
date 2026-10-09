// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// A recorded poll marker keeps a reason, because a partial checkpoint without one is refused, and the
// reason it keeps names operations and nothing else.
func TestARecordedPollReasonNamesOperationsOnly(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(gcpfeeder.PollMarker{
		Outcome: "partial",
		Reason: "run.revisions.list acme-prod/europe-west1: googleapi: Error 503 for acme-prod; " +
			"cloudsql.instances.list acme-prod: denied; run.revisions.list acme-prod/us-east1: timeout",
		Projects: []string{"acme-prod"},
	})
	out, err := gcpfeeder.PreparePayload(gcpfeeder.PayloadPollMarker, raw)
	if err != nil {
		t.Fatal(err)
	}
	var marker gcpfeeder.PollMarker
	if err := json.Unmarshal(out, &marker); err != nil {
		t.Fatal(err)
	}
	if want := "failed reads: cloudsql.instances.list, run.revisions.list"; marker.Reason != want {
		t.Errorf("reason = %q, want %q", marker.Reason, want)
	}
	if strings.Contains(marker.Reason, "acme") {
		t.Error("the recorded reason still names the project")
	}
	if len(marker.Projects) != 1 {
		t.Error("the pre-pass touched a field it is not about; the table pseudonymises projects")
	}
	other := []byte(`{"services":[]}`)
	if got, _ := gcpfeeder.PreparePayload(gcpfeeder.PayloadServices, other); string(got) != string(other) {
		t.Error("the pre-pass changed a payload that is not a poll marker")
	}
}
