// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Cloud Run topology (T051, T052).

// `Service.updateTime` moves for any change to the resource — an image bump, an environment
// variable, a label — so it is never a change instant. It is recorded as metadata, and the property
// name says so, because a property name is what a reader sees in a golden and what a query author
// autocompletes.
func TestServiceUpdateTimeIsMetadataAndNeverAChangeInstant(t *testing.T) {
	updated := time.Date(2026, 9, 21, 14, 37, 0, 0, time.UTC)
	svcpb := &runpb.Service{
		Name:            "projects/nova-production/locations/europe-west1/services/checkout",
		CreateTime:      timestamppb.New(created),
		UpdateTime:      timestamppb.New(updated),
		TrafficStatuses: statuses("checkout-00042-abc", 100),
	}
	obs, err := gcpfeeder.ObserveService(svcpb, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveService: %v", err)
	}
	if !obs.UpdateTime.Equal(updated) {
		t.Fatalf("updateTime = %s, want %s; it is recorded as metadata", obs.UpdateTime, updated)
	}
	props := propsMap(t, obs.Props())
	if _, recorded := props[gcpfeeder.PropUpdateTime]; !recorded {
		t.Error("updateTime is not recorded at all; a reader cannot see the resource moved")
	}
	if !strings.Contains(gcpfeeder.PropUpdateTime, "metadata_only") {
		t.Errorf("the property name %q does not warn that it is not a change instant; the one place "+
			"a warning cannot be missed is the name itself (contracts/gcp-feeder.md §3.2)",
			gcpfeeder.PropUpdateTime)
	}
	// The node's valid-from is the creation instant, not the update instant.
	if fact := obs.NodeFact(); !fact.ValidAt.Equal(created) {
		t.Fatalf("the service node is valid at %s, want its createTime %s", fact.ValidAt, created)
	}
}

// A service with no creation instant asserts an unknown start rather than guessing the poll time,
// which is the guess that would make every first poll claim the estate was created at boot (FR-011).
func TestAServiceWithNoCreateTimeAssertsAnUnknownStart(t *testing.T) {
	svcpb := &runpb.Service{Name: "projects/p/locations/r/services/s"}
	obs, err := gcpfeeder.ObserveService(svcpb, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveService: %v", err)
	}
	fact := obs.NodeFact()
	if !fact.ValidFromUnknown {
		t.Fatal("a service with no createTime did not assert an unknown start (FR-011)")
	}
	if !fact.ValidAt.IsZero() {
		t.Fatalf("it also carries a valid-at of %s", fact.ValidAt)
	}
	if fact.Type != graphv1.NodeType_SERVICE {
		t.Errorf("node type = %s, want SERVICE", fact.Type)
	}
}

// A service whose coordinates cannot be read is not emitted under a guessed ref, because a guessed
// ref merges with something (FR-010).
func TestAServiceWithUnreadableCoordinatesIsNotEmitted(t *testing.T) {
	_, err := gcpfeeder.ObserveService(&runpb.Service{Name: "checkout"}, gcpfeeder.DefaultLabelPolicy())
	if err == nil {
		t.Fatal("a service with an unparseable resource name was observed under some ref")
	}
	if !strings.Contains(err.Error(), "FR-010") {
		t.Errorf("the refusal does not name the rule: %q", err.Error())
	}
}

// The declared OpenTelemetry service name is read from a literal environment variable. A name sourced
// from a secret is not read — the feeder has no permission to and would not record it — and a name it
// cannot see is a name it does not claim, which leaves FR-118's certain rule unsatisfied. That is the
// correct outcome: a certain rule fires on evidence, not on a gap.
func TestOnlyALiterallyDeclaredOTelServiceNameIsClaimed(t *testing.T) {
	withLiteral := &runpb.Service{
		Name: "projects/p/locations/r/services/s",
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Env: []*runpb.EnvVar{{
				Name:   gcpfeeder.EnvVarOTelServiceName,
				Values: &runpb.EnvVar_Value{Value: "checkout"},
			}},
		}}},
	}
	obs, err := gcpfeeder.ObserveService(withLiteral, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveService: %v", err)
	}
	if obs.OTelServiceName != "checkout" {
		t.Fatalf("declared name = %q, want checkout", obs.OTelServiceName)
	}

	fromSecret := &runpb.Service{
		Name: "projects/p/locations/r/services/s",
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Env: []*runpb.EnvVar{{
				Name:   gcpfeeder.EnvVarOTelServiceName,
				Values: &runpb.EnvVar_ValueSource{},
			}},
		}}},
	}
	obs, err = gcpfeeder.ObserveService(fromSecret, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveService: %v", err)
	}
	if obs.OTelServiceName != "" {
		t.Fatalf("a name sourced from a secret was claimed as %q", obs.OTelServiceName)
	}
}

// A revision always has a documented `createTime`, and only the `Ready` condition is read: the other
// `Condition.type` values are not in the public contract, however often they appear in practice.
func TestARevisionReadsOnlyTheReadyCondition(t *testing.T) {
	revpb := &runpb.Revision{
		Name:       "projects/nova-production/locations/europe-west1/services/checkout/revisions/checkout-00042-abc",
		CreateTime: timestamppb.New(created),
		Containers: []*runpb.Container{{Image: "europe-docker.pkg.dev/p/r/checkout@sha256:abcdef"}},
		// Ready first, the out-of-contract condition second and disagreeing. The order matters:
		// with Ready last, an implementation that read *every* condition would still end on the
		// right answer and this test would pass against it.
		Conditions: []*runpb.Condition{
			{Type: gcpfeeder.ConditionReady, State: runpb.Condition_CONDITION_SUCCEEDED},
			{Type: "RoutesReady", State: runpb.Condition_CONDITION_FAILED},
			{Type: "ConfigurationsReady", State: runpb.Condition_CONDITION_FAILED},
		},
	}
	obs, err := gcpfeeder.ObserveRevision(revpb, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveRevision: %v", err)
	}
	if !obs.Ready || !obs.ReadyObserved {
		t.Fatalf("Ready=%v observed=%v; the failing RoutesReady condition is not in the public "+
			"contract and must not be read (contracts/gcp-feeder.md §3.2)", obs.Ready, obs.ReadyObserved)
	}
	if obs.ImageDigest != "sha256:abcdef" {
		t.Errorf("image digest = %q", obs.ImageDigest)
	}
	if fact := obs.NodeFact(); fact.Type != graphv1.NodeType_WORKLOAD || !fact.ValidAt.Equal(created) {
		t.Errorf("node = %s valid at %s, want WORKLOAD at %s", fact.Type, fact.ValidAt, created)
	}

	// "Not ready" and "Cloud Run did not say" stay distinguishable.
	silent := &runpb.Revision{Name: revpb.Name, CreateTime: timestamppb.New(created)}
	quiet, err := gcpfeeder.ObserveRevision(silent, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveRevision: %v", err)
	}
	if quiet.ReadyObserved {
		t.Error("a revision with no Ready condition reported one as observed")
	}
}

// A tag is not a digest: `checkout:v2` is mutable, two deploys can carry it, and claiming it as an
// identity would merge two revisions that ran different code.
func TestATagIsNotClaimedAsADigest(t *testing.T) {
	revpb := &runpb.Revision{
		Name:       "projects/p/locations/r/services/s/revisions/s-00001-abc",
		CreateTime: timestamppb.New(created),
		Containers: []*runpb.Container{{Image: "europe-docker.pkg.dev/p/r/checkout:v2"}},
	}
	obs, err := gcpfeeder.ObserveRevision(revpb, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveRevision: %v", err)
	}
	if obs.ImageDigest != "" {
		t.Fatalf("a tag was claimed as the digest %q", obs.ImageDigest)
	}
	for _, claim := range obs.Revision.Claims(obs.Image, obs.ImageDigest) {
		if claim.Value == "v2" {
			t.Error("a tag was claimed as an identity")
		}
	}
}

// The property recording where the split came from always says `trafficStatuses`. A golden that ever
// says otherwise is the regression cloudrun.go exists to prevent.
func TestTheSplitAlwaysRecordsThatItCameFromTheObservedField(t *testing.T) {
	svcpb := &runpb.Service{
		Name:            "projects/p/locations/r/services/s",
		CreateTime:      timestamppb.New(created),
		TrafficStatuses: statuses("s-00001-abc", 100),
	}
	obs, err := gcpfeeder.ObserveService(svcpb, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveService: %v", err)
	}
	props := propsMap(t, obs.Props())
	if props[gcpfeeder.PropTrafficSplitSource] != gcpfeeder.TrafficSplitSourceObserved {
		t.Fatalf("%s = %q, want %q", gcpfeeder.PropTrafficSplitSource,
			props[gcpfeeder.PropTrafficSplitSource], gcpfeeder.TrafficSplitSourceObserved)
	}
	if !strings.Contains(props[gcpfeeder.PropServing], "s-00001-abc") {
		t.Errorf("%s = %q, want the serving revision", gcpfeeder.PropServing, props[gcpfeeder.PropServing])
	}
}
