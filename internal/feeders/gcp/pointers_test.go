// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"errors"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Pointers (T059, FR-079–FR-081, FR-085).

func serviceObservation() gcpfeeder.ServiceObservation {
	return gcpfeeder.ServiceObservation{
		Service:         gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"},
		OTelServiceName: "checkout",
		Labels:          gcpfeeder.DefaultLabelPolicy().Apply("nova-production", map[string]string{"environment": "production"}),
	}
}

// `run.googleapis.com/request_count` is written against both `cloud_run_revision` and
// `cloud_run_instance`, and `cloud_run_instance` carries no `revision_name` label. An unpinned
// selector silently picks up series with no revision, and `errors_by_version` then merges or drops
// groups without saying so — it would not error, it would answer, and the answer would exonerate a
// bad deploy.
func TestAMetricSelectorMustPinTheResourceType(t *testing.T) {
	_, err := gcpfeeder.MetricPointer(`metric.type="run.googleapis.com/request_count"`, nil)
	if err == nil {
		t.Fatal("a Monitoring selector with no resource.type was accepted")
	}
	if !errors.Is(err, gcpfeeder.ErrNoResourceType) {
		t.Fatalf("the refusal is %v, want ErrNoResourceType", err)
	}
	if !strings.Contains(err.Error(), gcpfeeder.ResourceTypeCloudRunInstance) {
		t.Errorf("the refusal does not say which resource type would be picked up: %q", err.Error())
	}

	pinned, err := gcpfeeder.MetricPointer(
		`metric.type="run.googleapis.com/request_count" AND resource.type="cloud_run_revision"`, nil)
	if err != nil {
		t.Fatalf("a pinned selector was refused: %v", err)
	}
	if pinned.GetVocabulary() != feeder.VocabGCPMonitoringFilter {
		t.Errorf("vocabulary = %q, want %q", pinned.GetVocabulary(), feeder.VocabGCPMonitoringFilter)
	}
}

// And the rule holds for every pointer the feeder actually mints, not only for the helper.
func TestEveryMintedMetricSelectorPinsTheResourceType(t *testing.T) {
	servicePointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	revisionPointers, err := gcpfeeder.RevisionPointers(gcpfeeder.RevisionObservation{
		Revision: gcpfeeder.Revision{
			Service:  gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"},
			Revision: "checkout-00042-abc",
		},
	}, "production")
	if err != nil {
		t.Fatalf("RevisionPointers: %v", err)
	}

	metrics := 0
	for _, pointer := range append(servicePointers, revisionPointers...) {
		if pointer.GetKind() != graphv1.PointerKind_METRIC {
			continue
		}
		metrics++
		if !strings.Contains(pointer.GetSelector(), `resource.type="`+gcpfeeder.ResourceTypeCloudRunRevision+`"`) {
			t.Errorf("a minted metric selector does not pin the resource type: %s", pointer.GetSelector())
		}
	}
	if metrics == 0 {
		t.Fatal("no metric pointer was minted, so the assertion above checked nothing")
	}

	// FR-079's required signals are all present on a service.
	selectors := strings.Join(selectorsOf(servicePointers), "\n")
	for _, want := range []string{
		gcpfeeder.MetricRequestCount, gcpfeeder.MetricRequestLatency,
		`metric.labels.response_code_class="5xx"`, "severity >= WARNING",
	} {
		if !strings.Contains(selectors, want) {
			t.Errorf("a service carries no pointer selecting %q (FR-079)", want)
		}
	}
	if !hasKind(servicePointers, graphv1.PointerKind_LOG) {
		t.Error("a service carries no log pointer (FR-079)")
	}
	if !hasKind(servicePointers, graphv1.PointerKind_SOURCE_LINK) {
		t.Error("a service carries no link back to the GCP console (FR-079)")
	}
}

// A revision-scoped selector is what makes `errors_by_version` answerable at all: without
// `revision_name` in the selector, "is the new revision failing and the old one not" has no query
// behind it.
func TestARevisionPointerPinsTheRevision(t *testing.T) {
	pointers, err := gcpfeeder.RevisionPointers(gcpfeeder.RevisionObservation{
		Revision: gcpfeeder.Revision{
			Service:  gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"},
			Revision: "checkout-00042-abc",
		},
	}, "production")
	if err != nil {
		t.Fatalf("RevisionPointers: %v", err)
	}
	for _, pointer := range pointers {
		if pointer.GetKind() == graphv1.PointerKind_SOURCE_LINK {
			continue
		}
		if !strings.Contains(pointer.GetSelector(), `resource.labels.revision_name="checkout-00042-abc"`) {
			t.Errorf("a revision pointer does not pin the revision: %s", pointer.GetSelector())
		}
	}
	// The attributes identifying the entity are OpenTelemetry conventions (FR-081).
	attrs := pointers[0].GetAttributes()
	if attrs[feeder.AttrCloudRegion] != "europe-west1" || attrs[feeder.AttrServiceName] != "checkout" {
		t.Errorf("the pointer attributes are not in OTel conventions: %v", attrs)
	}
	if attrs[gcpfeeder.PropRevisionLabel] != "checkout-00042-abc" {
		t.Errorf("the pointer does not say which revision it is about: %v", attrs)
	}
}

// An environment of `unknown` is not attached as a deployment environment: `unknown` is a first-class
// answer about the estate, and a pointer attribute claiming it as an environment would make a backend
// group by a value nothing carries.
func TestAnUnknownEnvironmentIsNotAttachedAsAnEnvironment(t *testing.T) {
	obs := serviceObservation()
	obs.Labels = gcpfeeder.DefaultLabelPolicy().Apply("some-other-project", map[string]string{})
	pointers, err := gcpfeeder.ServicePointers(obs)
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	for _, pointer := range pointers {
		if value, set := pointer.GetAttributes()[feeder.AttrDeploymentEnvironment]; set {
			t.Fatalf("a pointer claims environment %q when the environment is unknown", value)
		}
	}
}

// FR-079's "carrying no credential". A console link is a URL, and a URL is where a token ends up.
func TestNoMintedPointerCarriesACredential(t *testing.T) {
	servicePointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	if idx, class := gcpfeeder.PointersCarryNoCredential(servicePointers); idx >= 0 {
		t.Fatalf("pointer %d carries %s: %s", idx, class, servicePointers[idx].GetSelector())
	}

	// The check itself works, or the assertion above proves nothing.
	planted := append([]*graphv1.Pointer(nil), servicePointers...)
	planted = append(planted, gcpfeeder.ConsoleLink(
		"https://console.cloud.google.com/run?project=p&access_token=ya29.something", nil))
	if idx, class := gcpfeeder.PointersCarryNoCredential(planted); idx < 0 {
		t.Fatal("a planted access token was not found, so the check proves nothing")
	} else if class == "" {
		t.Error("the finding does not name the class of credential")
	}
}

// FR-085: the absence of tracing is a stated fact about the entity rather than a fabricated pointer,
// so a consumer can tell "no trace pointer because there is no tracing" from "nobody wrote one".
func TestNoTracePointerIsMintedAndTheAbsenceIsStated(t *testing.T) {
	pointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	if hasKind(pointers, graphv1.PointerKind_TRACE) {
		t.Fatal("a trace pointer was minted; this organisation has no trace data source (FR-085)")
	}
	if gcpfeeder.TracePointerAbsence == "" {
		t.Fatal("the absence carries no stated reason")
	}
	if !strings.Contains(gcpfeeder.TracePointerAbsence, "FR-085") {
		t.Errorf("the stated absence does not name the rule: %q", gcpfeeder.TracePointerAbsence)
	}
}

func selectorsOf(pointers []*graphv1.Pointer) []string {
	out := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		out = append(out, pointer.GetSelector())
	}
	return out
}

func hasKind(pointers []*graphv1.Pointer, kind graphv1.PointerKind) bool {
	for _, pointer := range pointers {
		if pointer.GetKind() == kind {
			return true
		}
	}
	return false
}
