// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The byte-level pass must not fire on this project's own entity keys or on a fixture author's
// placeholder. An assertion that fires on every graph node in the corpus is an assertion somebody
// turns off within a day, and a turned-off assertion is worse than none — it reads as a guarantee.
func TestTheArtifactScanDoesNotFireOnThisProjectsOwnEntityKeys(t *testing.T) {
	for _, clean := range []string{
		`{"subject": "payments@otel.service.name"}`,
		`{"workload": "storefront@app.kubernetes.io"}`,
		`{"author": "alice@shop.example"}`,
		`{"reviewer": "bob@example.com"}`,
		`{"notify": "@channel"}`,
		// A log attribute a connector writes into a selector or a join key names a field (005).
		`{"selector": "service:search @env:production"}`,
		`{"joinKeys": {"version": "@version"}}`,
		`{"note": "the environment is read from @env. A Remapper would help"}`,
		`{"note": "or @deployment.environment.name"}`,
		`{"service": "px_svc_abcdefghijkl", "region": "europe-west1"}`,
	} {
		if class := sanitise.PeopleInArtifact([]byte(clean)); class != "" {
			t.Errorf("the scan reported %s in %s, which names nobody", class, clean)
		}
	}
}

// And it must fire on the thing it exists for, or the test above is the whole of it.
func TestTheArtifactScanFiresOnARealAddressOrHandle(t *testing.T) {
	if class := sanitise.PeopleInArtifact([]byte(`{"author": "jane.doe@acme-corp.io"}`)); class != "an email address" {
		t.Errorf("the scan reported %q for a real address", class)
	}
	if class := sanitise.PeopleInArtifact([]byte(`{"note": "ask @envoy-team about it"}`)); class != "an @handle" {
		t.Errorf("a handle that merely starts like a log attribute passed: %q", class)
	}
	if class := sanitise.PeopleInArtifact([]byte(`{"note": "ask @jane-doe about it"}`)); class != "an @handle" {
		t.Errorf("the scan reported %q for a handle", class)
	}
	// The class is returned, never the value: a report that quoted what it found would put the
	// address in a CI log.
	if class := sanitise.PeopleInArtifact([]byte(`{"author": "jane.doe@acme-corp.io"}`)); class == "jane.doe@acme-corp.io" {
		t.Error("the scan returned the value it found")
	}
}

// A GCP service account is infrastructure, not a person (found by T153).
//
// This exemption was missing, and the consequence was not subtle: a Cloud Run revision carries
// `serviceAccount: runtime@<project>.iam.gserviceaccount.com`, so the people check fired on the
// ordinary case. Every real GCP recording would have failed the commit gate, and a gate that fails on
// the ordinary case is a gate somebody switches off — the same failure the entity-key and
// documentation-name exemptions were written to prevent.
//
// It was found by pointing `fixture campaign scan` at a committed twin fixture, which is what a gate
// nothing had ever been run against looks like.
func TestAServiceAccountIsNotAPerson(t *testing.T) {
	t.Parallel()

	// Every service-account form GCP mints, and they are all `gserviceaccount.com` — a Google-owned
	// domain that cannot hold a human mailbox, which is what makes the exemption precise.
	notPeople := []string{
		`{"serviceAccount":"runtime@twin-production.iam.gserviceaccount.com"}`,
		`{"serviceAccount":"twin-production@appspot.gserviceaccount.com"}`,
		`{"serviceAccount":"123456789-compute@developer.gserviceaccount.com"}`,
		`{"serviceAccount":"123456789@cloudservices.gserviceaccount.com"}`,
		`{"principal":"service-123@gcp-sa-cloudrun.iam.gserviceaccount.com"}`,
		`{"principal":"service-123@serverless-robot-prod.iam.gserviceaccount.com"}`,
	}
	for _, body := range notPeople {
		if class := sanitise.PeopleInArtifact([]byte(body)); class != "" {
			t.Errorf("%s was reported as carrying %s; a service account is an infrastructure "+
				"identifier, and a check that fires on `serviceAccount` fires on every Cloud Run "+
				"revision there is", body, class)
		}
	}

	// The exemption is narrow: a person at a look-alike domain is still a person, or it would be a
	// hole rather than an exemption.
	people := []string{
		`{"principalEmail":"casey@gserviceaccount.com.example.net"}`,
		`{"principalEmail":"casey@notgserviceaccount.io"}`,
		`{"principalEmail":"casey@twin-production.iam.example.org"}`,
	}
	for _, body := range people {
		if class := sanitise.PeopleInArtifact([]byte(body)); class == "" {
			t.Errorf("%s was not reported; the service-account exemption must match the Google "+
				"domain itself, not anything containing it, or it is a hole", body)
		}
	}
}
