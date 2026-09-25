// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Join keys on the GCP feeder's pointers (ADR-0005 D6, FR-102).
//
// A join key says which field of *this pointer's vocabulary* plays a published role, so that two
// answers can be related to each other and to the graph. The roles are a closed set of five and a
// role the backend cannot express is left out rather than guessed at, which means the interesting
// assertions here are as much about which roles are **absent** as about which are present.

func revisionObservationForJoinKeys() gcpfeeder.RevisionObservation {
	return gcpfeeder.RevisionObservation{
		Revision: gcpfeeder.Revision{
			Service:  gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"},
			Revision: "checkout-00042-abc",
		},
	}
}

// Every Cloud Run pointer that is executed — metric or log — names the revision as the version and
// the service as the workload, in the filter-field spelling the query actually uses, and names
// nothing else.
func TestEveryExecutableCloudRunPointerNamesTheRevisionAsTheVersion(t *testing.T) {
	servicePointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	revisionPointers, err := gcpfeeder.RevisionPointers(revisionObservationForJoinKeys(), "production")
	if err != nil {
		t.Fatalf("RevisionPointers: %v", err)
	}

	want := map[string]string{
		feeder.JoinRoleVersion:  gcpfeeder.FieldRevisionName,
		feeder.JoinRoleWorkload: gcpfeeder.FieldServiceName,
	}
	executed := 0
	for _, set := range [][]*graphv1.Pointer{servicePointers, revisionPointers} {
		for _, pointer := range set {
			if pointer.GetKind() == graphv1.PointerKind_SOURCE_LINK {
				continue
			}
			executed++
			if got := pointer.GetJoinKeys(); !maps.Equal(got, want) {
				t.Errorf("join_keys = %v, want %v, on %s", got, want, pointer.GetSelector())
			}
		}
	}
	// Four metric pointers and a log pointer on the service, two and one on the revision.
	if executed != 8 {
		t.Fatalf("%d executable pointers were checked, want 8: a set that shrank silently would "+
			"make every assertion above vacuous", executed)
	}
}

// The roles GCP cannot express are absent, and each absence is a fact about GCP.
//
// `pod` is the one worth a test of its own rather than a comment: `instance_id` exists only on
// `cloud_run_instance`, and every selector here pins `cloud_run_revision` precisely because
// `cloud_run_instance` carries no `revision_name`. The two resources are mutually exclusive, so a
// pointer that named both a revision and an instance would be naming a field its own query cannot
// return.
func TestACloudRunPointerClaimsNoPodNoHostAndNoTrace(t *testing.T) {
	pointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	for _, pointer := range pointers {
		for _, role := range []string{feeder.JoinRolePod, feeder.JoinRoleHost, feeder.JoinRoleTrace} {
			if got, ok := pointer.GetJoinKeys()[role]; ok {
				t.Errorf("a Cloud Run pointer claims the %q role as %q: Cloud Run is serverless, "+
					"the per-instance label lives on %s which carries no revision, and this estate "+
					"has no trace source (FR-085)", role, got, gcpfeeder.ResourceTypeCloudRunInstance)
			}
		}
	}
}

// A console link carries no join keys. Its vocabulary is a URL: there are no tags to name, so the
// roles have nowhere to land.
func TestASourceLinkCarriesNoJoinKeys(t *testing.T) {
	pointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	links := 0
	for _, pointer := range pointers {
		if pointer.GetKind() != graphv1.PointerKind_SOURCE_LINK {
			continue
		}
		links++
		if got := pointer.GetJoinKeys(); len(got) != 0 {
			t.Errorf("a console link declares join keys %v; a URL has no tags for a role to name", got)
		}
	}
	if links == 0 {
		t.Fatal("no source link was minted, so the assertion above checked nothing")
	}
}

// A Cloud SQL pointer names the instance connection name as the host, and names no version: a
// managed database has no deployed version in its metric labels, so there is no field to split an
// error rate by.
func TestACloudSQLPointerNamesTheInstanceAsTheHostAndNoVersion(t *testing.T) {
	obs := gcpfeeder.InstanceObservation{
		Instance: gcpfeeder.SQLInstance{Project: "nova-production", Region: "europe-west1", Name: "orders-db"},
	}
	pointers, err := obs.Pointers()
	if err != nil {
		t.Fatalf("Pointers: %v", err)
	}
	want := map[string]string{feeder.JoinRoleHost: gcpfeeder.FieldDatabaseID}
	metricPointers := 0
	for _, pointer := range pointers {
		if pointer.GetKind() != graphv1.PointerKind_METRIC {
			continue
		}
		metricPointers++
		if got := pointer.GetJoinKeys(); !maps.Equal(got, want) {
			t.Errorf("join_keys = %v, want %v, on %s", got, want, pointer.GetSelector())
		}
	}
	if metricPointers == 0 {
		t.Fatal("no Cloud SQL metric pointer was minted, so the assertion above checked nothing")
	}
}

// An alert policy's condition filter carries no join keys, and this is the case the derivation has
// to get right rather than the case it is convenient for.
//
// The filter is the policy author's, minted exactly as stated — it may pin no `resource.type` at
// all, or pin one this feeder has never heard of. Declaring `version -> resource.labels.revision_name`
// on a filter watching a Pub/Sub subscription would name a label that does not exist, and a
// consumer splitting by it would get a confident answer over one merged group.
func TestAnAlertConditionFilterCarriesNoJoinKeysWhenItsResourceIsNotOurs(t *testing.T) {
	obs := gcpfeeder.AlertPolicyObservation{
		Policy:      gcpfeeder.AlertPolicy{Project: "nova-production", ID: "1234567890"},
		DisplayName: "checkout 5xx",
		Conditions: []gcpfeeder.AlertCondition{{
			DisplayName: "subscription backlog",
			Kind:        "threshold",
			Filter: `metric.type="pubsub.googleapis.com/subscription/num_undelivered_messages" ` +
				`AND resource.type="pubsub_subscription"`,
		}},
	}
	pointers, err := obs.Pointers()
	if err != nil {
		t.Fatalf("Pointers: %v", err)
	}
	for _, pointer := range pointers {
		if got := pointer.GetJoinKeys(); len(got) != 0 {
			t.Errorf("an alert condition over %q declares join keys %v; the fields named do not "+
				"exist on that resource", pointer.GetSelector(), got)
		}
	}
}

// Every field a minted pointer names is a filterable field of the vocabulary it is declared in.
//
// The spelling matters and is easy to get wrong in the direction that looks right: a returned
// series' label map is keyed by the bare `revision_name`, and a join key spelled that way names no
// field the filter language has. The roles themselves are checked in the internal test beside this
// one, against the declared maps — WithJoinKeys drops an unpublished role silently, so by the time
// a pointer exists the evidence of a typo is already gone.
func TestEveryFieldAMintedPointerNamesIsFilterable(t *testing.T) {
	servicePointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	sql, err := gcpfeeder.InstanceObservation{
		Instance: gcpfeeder.SQLInstance{Project: "nova-production", Region: "europe-west1", Name: "orders-db"},
	}.Pointers()
	if err != nil {
		t.Fatalf("Pointers: %v", err)
	}
	declared := 0
	for _, pointer := range append(servicePointers, sql...) {
		for role, field := range pointer.GetJoinKeys() {
			declared++
			if !strings.HasPrefix(field, "resource.labels.") && !strings.HasPrefix(field, "metric.labels.") {
				t.Errorf("join_keys[%q] = %q, which is not a filterable field of this vocabulary: "+
					"the spelling is the one the query uses, not the one a returned series' label "+
					"map is keyed by", role, field)
			}
		}
	}
	if declared == 0 {
		t.Fatal("no join key was declared at all, so the assertion above checked nothing")
	}
}

// The payoff, and the reason this change exists at all: `errors_by_version` is expressible over a
// GCP pointer.
//
// The worker refuses a pointer that declares no `version` join key rather than guessing at a tag —
// which meant that before this change no GCP metric pointer could serve the one term whose entire
// job is "the new revision is failing and the old one is not". The refusal was right; the missing
// key was the defect.
func TestErrorsByVersionIsExpressibleOverAGCPServicePointer(t *testing.T) {
	pointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	var metric *graphv1.Pointer
	for _, pointer := range pointers {
		if pointer.GetKind() == graphv1.PointerKind_METRIC &&
			strings.Contains(pointer.GetSelector(), gcpfeeder.MetricRequestCount) {
			metric = pointer
			break
		}
	}
	if metric == nil {
		t.Fatal("no request-count metric pointer was minted")
	}

	request, err := metrics.ErrorsByVersionTerm(metric, nil, "h1", "is the new revision failing?")
	if err != nil {
		t.Fatalf("ErrorsByVersionTerm over a GCP pointer: %v", err)
	}
	if got := request.GetTerm().GetErrorsByVersion().GetVersionAttribute(); got != gcpfeeder.FieldRevisionName {
		t.Errorf("the term splits by %q, want %q", got, gcpfeeder.FieldRevisionName)
	}

	// The other half: strip the key and the worker refuses. Without this, the test above would
	// pass just as happily if the refusal had been removed instead of the key supplied.
	stripped := &graphv1.Pointer{
		Kind: metric.GetKind(), BackendKind: metric.GetBackendKind(),
		Vocabulary: metric.GetVocabulary(), Selector: metric.GetSelector(),
		Attributes: metric.GetAttributes(),
	}
	if _, err := metrics.ErrorsByVersionTerm(stripped, nil, "h1", "q"); err == nil {
		t.Error("a pointer declaring no version join key was accepted; splitting a metric by a " +
			"guessed tag is the confident wrong answer the refusal exists to prevent")
	}
}

// The role set the feeder can express is the role set it declares, listed once so that adding a
// role to the map without deciding it is expressible fails here.
func TestTheDeclaredRoleSetIsTheOneTheVocabularyCanExpress(t *testing.T) {
	pointers, err := gcpfeeder.ServicePointers(serviceObservation())
	if err != nil {
		t.Fatalf("ServicePointers: %v", err)
	}
	var roles []string
	for _, pointer := range pointers {
		roles = append(roles, slices.Collect(maps.Keys(pointer.GetJoinKeys()))...)
	}
	slices.Sort(roles)
	roles = slices.Compact(roles)
	want := []string{feeder.JoinRoleVersion, feeder.JoinRoleWorkload}
	slices.Sort(want)
	if !slices.Equal(roles, want) {
		t.Errorf("Cloud Run pointers declare the roles %v, want %v", roles, want)
	}
}

// A selector pinning a resource this feeder has no published mapping for gets **no** join keys.
//
// This is the arm of the derivation that decides what happens to everything not written here, and
// it is the one with no caller today: the mapping is chosen from the selector's own `resource.type`
// pin, so a future pointer over a resource nobody has mapped must come out with an empty map rather
// than inherit Cloud Run's fields. Inheriting them would name `revision_name` on a resource that
// has no revision, which is the shape that answers confidently and wrongly.
func TestAPointerOverAnUnmappedResourceInheritsNoJoinKeys(t *testing.T) {
	pointer, err := gcpfeeder.MetricPointer(
		`metric.type="pubsub.googleapis.com/subscription/num_undelivered_messages" `+
			`AND resource.type="pubsub_subscription"`,
		map[string]string{gcpfeeder.PropProject: "nova-production"})
	if err != nil {
		t.Fatalf("MetricPointer: %v", err)
	}
	if got := pointer.GetJoinKeys(); len(got) != 0 {
		t.Errorf("a pointer over pubsub_subscription declares %v; that resource has no revision "+
			"and no Cloud Run service, so every field named is one the query cannot return", got)
	}
}
