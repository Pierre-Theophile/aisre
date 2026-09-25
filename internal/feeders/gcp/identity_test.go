// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Identity (T047, FR-010, FR-115, FR-116).

func svc() gcpfeeder.Service {
	return gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
}

// Two projects with the same service name are never the same entity, and the guarantee comes from the
// *value* rather than from a property beside it — because resolution keys on (namespace, value) and a
// property is only supporting evidence a rule may consult.
func TestTwoProjectsWithOneServiceNameAreNeverOneEntity(t *testing.T) {
	prod := svc()
	staging := gcpfeeder.Service{Project: "nova-staging", Region: "europe-west1", Name: "checkout"}
	if prod.Value() == staging.Value() {
		t.Fatalf("checkout in two projects has one identifier %q; FR-010 requires the project to be "+
			"in the value", prod.Value())
	}
	if !strings.Contains(prod.Value(), "nova-production") || !strings.Contains(prod.Value(), "europe-west1") {
		t.Fatalf("the identifier %q does not carry both the project and the region", prod.Value())
	}
	// The same name in two regions is also two entities.
	otherRegion := gcpfeeder.Service{Project: "nova-production", Region: "us-central1", Name: "checkout"}
	if prod.Value() == otherRegion.Value() {
		t.Fatal("one service name in two regions has one identifier")
	}
}

// FR-115 is easy to half-satisfy: mint claims for the other identifiers and forget the one you
// address the entity by. The resolution layer cannot merge on an identifier it was never told about.
func TestTheAddressingRefIsAlsoAClaim(t *testing.T) {
	service := svc()
	claims := service.Claims("checkout", []string{"OTEL_SERVICE_NAME", "ORDERS_PRIMARY_DSN"})
	if !claimed(claims, gcpfeeder.NSService, service.Value()) {
		t.Fatalf("the service does not claim the ref it is addressed by (%s=%s); the resolution layer "+
			"cannot merge on an identifier it was never told about (FR-115)",
			gcpfeeder.NSService, service.Value())
	}
	if !claimed(claims, gcpfeeder.NSService, service.ResourceName()) {
		t.Error("the service does not claim its fully qualified resource name")
	}
	if !claimed(claims, feeder.NSOTelService, "checkout") {
		t.Error("the service does not claim the OpenTelemetry service name it declares; FR-118's " +
			"certain rule depends on it")
	}

	revision := gcpfeeder.Revision{Service: service, Revision: "checkout-00042-abc"}
	revClaims := revision.Claims("europe-docker.pkg.dev/p/r/checkout@sha256:abc", "sha256:abc")
	if !claimed(revClaims, gcpfeeder.NSRevision, revision.Value()) {
		t.Errorf("the revision does not claim the ref it is addressed by (FR-115)")
	}
	if !claimed(revClaims, gcpfeeder.NSRevision, "sha256:abc") {
		t.Error("the revision does not claim its image digest, which is the identifier a build " +
			"pipeline also knows")
	}
	// Every claim carries a reason, or a reviewer reading a merge cannot tell a declared identifier
	// from a coincidence.
	for _, claim := range append(claims, revClaims...) {
		if claim.Why == "" {
			t.Errorf("claim %s=%s carries no reason", claim.Namespace, claim.Value)
		}
	}
}

// A service that declares no OpenTelemetry name claims none. A certain rule fires on evidence, not on
// a gap, so inventing a name here would make FR-118 fire on a guess.
func TestAServiceThatDeclaresNoOTelNameClaimsNone(t *testing.T) {
	for _, declared := range []string{"", "   "} {
		for _, claim := range svc().Claims(declared, nil) {
			if claim.Namespace == feeder.NSOTelService {
				t.Fatalf("a service declaring %q claimed %s=%s", declared, claim.Namespace, claim.Value)
			}
		}
	}
}

// Cloud Run spells one revision qualified in `Revision.name` and bare in
// `trafficStatuses[].revision`. Treating those as two revisions would double the graph.
func TestOneRevisionSpelledTwoWaysIsOneRevision(t *testing.T) {
	const bare = "checkout-00042-abc"
	qualified := "projects/nova-production/locations/europe-west1/services/checkout/revisions/" + bare
	if got := gcpfeeder.RevisionName(qualified); got != bare {
		t.Fatalf("RevisionName(qualified) = %q, want %q", got, bare)
	}
	if got := gcpfeeder.RevisionName(bare); got != bare {
		t.Fatalf("RevisionName(bare) = %q, want %q", got, bare)
	}
}

// An audit entry's resourceName becomes the ref of the service it changed, and the parse is strict: a
// Service with an empty region addresses a different entity, so a malformed name yields nothing.
func TestParsingAResourceNameIsStrict(t *testing.T) {
	service := svc()
	got, ok := gcpfeeder.ParseServiceResourceName(service.ResourceName())
	if !ok || got != service {
		t.Fatalf("round trip = %+v ok=%v, want %+v", got, ok, service)
	}
	// A revision's resource name extends a service's, so it still names that service.
	revision := gcpfeeder.Revision{Service: service, Revision: "checkout-00042-abc"}
	got, ok = gcpfeeder.ParseServiceResourceName(revision.ResourceName())
	if !ok || got != service {
		t.Fatalf("a revision name did not yield its service: %+v ok=%v", got, ok)
	}
	gotRev, ok := gcpfeeder.ParseRevisionResourceName(revision.ResourceName())
	if !ok || gotRev != revision {
		t.Fatalf("revision round trip = %+v ok=%v, want %+v", gotRev, ok, revision)
	}

	for _, malformed := range []string{
		"", "checkout",
		"projects/nova-production/locations/europe-west1/services/",
		"projects//locations/europe-west1/services/checkout",
		"projects/nova-production/locations//services/checkout",
		"projects/nova-production/services/checkout",
		"projects/nova-production/locations/europe-west1/jobs/checkout",
	} {
		if got, ok := gcpfeeder.ParseServiceResourceName(malformed); ok {
			t.Errorf("ParseServiceResourceName(%q) accepted it as %+v; a Service with an empty part "+
				"addresses a different entity", malformed, got)
		}
	}
}

// A ref built from an empty component resolves against something, and that something is not what the
// caller meant.
func TestEmptyIdentifierComponentsAreRefused(t *testing.T) {
	for _, bad := range []gcpfeeder.Service{
		{Region: "europe-west1", Name: "checkout"},
		{Project: "nova-production", Name: "checkout"},
		{Project: "nova-production", Region: "europe-west1"},
		{Project: "nova-production", Region: "europe-west1", Name: "  "},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Service%+v validated", bad)
		}
	}
	if err := (gcpfeeder.Revision{Service: svc()}).Validate(); err == nil {
		t.Error("a revision with no revision name validated")
	}
	if err := (gcpfeeder.Revision{Service: svc(), Revision: "r"}).Validate(); err != nil {
		t.Errorf("a valid revision was refused: %v", err)
	}
}

// Every namespace this feeder mints in is declared, or testkit fails the run (FR-116).
func TestEveryNamespaceTheFeederMintsInIsDeclared(t *testing.T) {
	for _, ns := range []string{
		gcpfeeder.NSService, gcpfeeder.NSRevision, gcpfeeder.NSSQLInstance, gcpfeeder.NSConfig,
		gcpfeeder.NSChange, gcpfeeder.NSAlertPolicy, gcpfeeder.NSGKECluster,
		gcpfeeder.NSLoadBalancer, gcpfeeder.NSVendor, gcpfeeder.NSVendorNotice,
	} {
		if !gcpfeeder.DeclaresNamespace(ns) {
			t.Errorf("%s is minted but not declared (FR-116)", ns)
		}
	}
	if gcpfeeder.DeclaresNamespace("gcp.something.invented") {
		t.Error("an undeclared namespace was reported as declared, so the check proves nothing")
	}
	// And the feeder's own Description declares them, which is what testkit reads.
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "nova",
		Scope:   gcpfeeder.Scope{Projects: []string{"nova-production"}, Regions: []string{"europe-west1"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	desc := f.Describe()
	for _, ns := range gcpfeeder.Namespaces {
		if !desc.DeclaresNamespace(ns) {
			t.Errorf("Describe() does not declare %s", ns)
		}
	}
}

// A configuration is a *version* of a service's settings, so the revision is part of its identity:
// without it every config change would address one node and the history would be one row deep.
//
// The **revision** and not the service's `generation`, which was the first spelling and was wrong: a
// Cloud Run revision is immutable, so its own generation is 1 for every revision a service ever has,
// and keying on it collapsed every configuration version onto `@1`.
func TestAConfigRefCarriesTheRevision(t *testing.T) {
	service := svc()
	first := gcpfeeder.Revision{Service: service, Revision: "checkout-00041-aaa"}.ConfigRef()
	second := gcpfeeder.Revision{Service: service, Revision: "checkout-00042-bbb"}.ConfigRef()
	if first.GetValue() == second.GetValue() {
		t.Fatalf("two revisions share the ref %q", first.GetValue())
	}
	if !strings.HasSuffix(first.GetValue(), "@checkout-00041-aaa") {
		t.Fatalf("the config ref %q does not carry its revision", first.GetValue())
	}
	if first.GetNamespace() != gcpfeeder.NSConfig {
		t.Errorf("namespace = %q, want %q", first.GetNamespace(), gcpfeeder.NSConfig)
	}
}

func claimed(claims []gcpfeeder.Claim, namespace, value string) bool {
	for _, claim := range claims {
		if claim.Namespace == namespace && claim.Value == value {
			return true
		}
	}
	return false
}
