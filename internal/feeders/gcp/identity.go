// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Identifier namespaces (T047, FR-010, FR-115, FR-116, data-model.md §2).
//
// # Project and region are in the value, not in a property beside it
//
// FR-010: two projects with the same service name are never the same entity. `checkout` in
// staging is not `checkout` in production, and an identifier that leaves the project out asks the
// resolution layer to merge them — which it would, correctly, on the evidence it was given.
//
// Putting the qualifier in a *property* instead is the mistake that looks tidier and is wrong.
// Resolution keys on `(namespace, value)`; a property is supporting evidence a rule may consult.
// So `gcp.cloudrun.service` = `nova-production/europe-west1/checkout`, and the project and region
// are also emitted as properties — for a reader — rather than instead of being in the value.
//
// # What the feeder addresses an entity by, it also claims
//
// FR-115 is easy to half-satisfy: a feeder mints claims for the *other* identifiers an entity has
// and forgets the one it uses itself. The resolution layer cannot merge on an identifier it was
// never told about, so the addressing ref is **also** a claim — `Claims` below always includes it,
// and identity_test.go asserts that rather than trusting the call sites to remember.

// The namespaces this feeder mints in. A Ref outside this set fails `pkg/feeder/testkit`
// (FR-116), which is what stops one connector quietly naming entities another connector owns.
const (
	// NSService is a Cloud Run service, valued `<project>/<region>/<service>`.
	NSService = "gcp.cloudrun.service"
	// NSRevision is a Cloud Run revision, valued `<project>/<region>/<service>/<revision>`.
	// Revision names are unique within a service, not globally.
	NSRevision = "gcp.cloudrun.revision"
	// NSSQLInstance is a Cloud SQL instance, valued `<project>/<region>/<instance>`.
	NSSQLInstance = "gcp.cloudsql.instance"
	// NSConfig is a version of a service's deployed settings, valued
	// `<project>/<region>/<service>@<revision>`. The revision is part of the identity because a
	// configuration *is* a version: without it, every config change would address the same node and
	// the history would be one row deep.
	//
	// The **revision** and not the service's `generation`, which was the first spelling and was
	// wrong. A Cloud Run revision is immutable, so its own `generation` is 1 for every revision a
	// service ever has — keying on it would collapse every configuration version onto `@1`. The
	// revision name is what Cloud Run itself versions a deployment by, and it is the thing whose
	// `createTime` dates the configuration change.
	NSConfig = "gcp.config"
	// NSChange is a change this feeder observed. Its value is deterministic from what the
	// change *is* (see the Change*Ref constructors), so re-reading a poll or a log window is a
	// no-op rather than a second change (FR-076).
	NSChange = "gcp.change"
	// NSAlertPolicy is a Cloud Monitoring alert policy, valued by the identifier GCP assigns.
	// That identifier is already globally unique and is stable across renames; the display name
	// is neither, and is claimed rather than used as identity.
	NSAlertPolicy = "gcp.monitoring.alert_policy"
	// NSGKECluster is a GKE cluster, valued `<project>/<location>/<cluster>`. This feeder emits
	// claims about it and nothing else: everything inside the cluster belongs to the 001
	// Kubernetes feeder, and a second namespace for those entities would be a second entity for
	// one workload (FR-030).
	NSGKECluster = "gcp.gke.cluster"
	// NSLoadBalancer is a load balancer, valued by its fully qualified resource name. P3, and
	// omitted entirely under budget (FR-057).
	NSLoadBalancer = "gcp.lb"
	// NSVendor is a third party, valued by the vendor slug from the allowlist. Configuration is
	// the authority on vendor identity, never a parsed sender domain.
	NSVendor = "vendor"
	// NSVendorNotice is a vendor announcement, valued `<vendor>/<notice-identifier>`, so the
	// same notice read twice — or read from two sources — addresses one change (FR-076).
	NSVendorNotice = "vendor.notice"
)

// Namespaces is what Describe declares, sorted. It is the closed set testkit checks against.
//
// `k8s.cluster` is in it and is not this connector's namespace, deliberately: FR-030 requires the GKE
// cluster to carry identity claims naming it **as the Kubernetes connector knows it**, and a claim in
// a namespace the description does not declare fails testkit. Declaring it is the assertion that this
// connector claims an identifier another connector owns — which is exactly what FR-115 asks a feeder
// to do and what the resolution layer needs in order to merge the two sides.
//
// `deploy.commit_sha` and `deploy.image` are in it for the same reason and one step further: they are
// nobody's namespace. They are the shared deploy vocabulary pkg/feeder publishes so that a rollout
// this connector saw and a rollout a deploy feeder saw can be recognised as one (004 T135, C8).
//
// This set is about namespaces and not about kinds. The two deploy namespaces are emitted as
// CORRELATION KEYS and not as identity claims (004 T148, rollout.go's RolloutDeployKeys) — a commit
// describes a rollout rather than naming it — and they are declared here all the same, because what
// testkit checks is that every ref a run emits lives in a namespace the description declared.
var Namespaces = []string{
	NSAlertPolicy,
	NSChange,
	NSConfig,
	feeder.NSDeployCommitSHA,
	feeder.NSDeployImage,
	NSGKECluster,
	feeder.NSK8sCluster,
	NSLoadBalancer,
	NSRevision,
	NSSQLInstance,
	NSService,
	NSVendor,
	NSVendorNotice,
}

// DeclaresNamespace reports whether ns is one this feeder mints in.
func DeclaresNamespace(ns string) bool { return slices.Contains(Namespaces, ns) }

// ErrEmptyIdentifierPart is returned for a ref built from an empty component. It is an error
// rather than a tolerated gap: `nova-production//checkout` and `nova-production/europe-west1/`
// are both refs that will resolve against something, and neither names what the caller meant.
var ErrEmptyIdentifierPart = fmt.Errorf("gcp: an identifier component is empty")

// Service is a Cloud Run service's coordinates. It exists as a type because the three parts
// travel together everywhere, and a function taking three strings is a function whose arguments
// get swapped exactly once, silently, in the one call site nobody tested.
type Service struct {
	Project string
	Region  string
	Name    string
}

// Revision is a Cloud Run revision's coordinates.
type Revision struct {
	Service
	// Revision is the revision name as Cloud Run assigns it, e.g. "checkout-00042-abc".
	Revision string
}

// Validate refuses coordinates with an empty part.
func (s Service) Validate() error {
	for name, part := range map[string]string{"project": s.Project, "region": s.Region, "service": s.Name} {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("%w: the %s of a Cloud Run service", ErrEmptyIdentifierPart, name)
		}
	}
	return nil
}

// Validate refuses coordinates with an empty part.
func (r Revision) Validate() error {
	if err := r.Service.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Revision) == "" {
		return fmt.Errorf("%w: the revision of a Cloud Run revision", ErrEmptyIdentifierPart)
	}
	return nil
}

// Value renders the service identifier: `<project>/<region>/<service>`.
func (s Service) Value() string { return s.Project + "/" + s.Region + "/" + s.Name }

// Value renders the revision identifier: `<project>/<region>/<service>/<revision>`.
func (r Revision) Value() string { return r.Service.Value() + "/" + r.Revision }

// Ref returns the service's addressing ref.
func (s Service) Ref() *graphv1.Ref { return feeder.Ref(NSService, s.Value()) }

// Ref returns the revision's addressing ref.
func (r Revision) Ref() *graphv1.Ref { return feeder.Ref(NSRevision, r.Value()) }

// ResourceName renders the fully qualified Cloud Run resource name GCP uses, which is a claim on
// every service (data-model.md §2.1) and is what an audit entry's `resourceName` carries.
func (s Service) ResourceName() string {
	return "projects/" + s.Project + "/locations/" + s.Region + "/services/" + s.Name
}

// ResourceName renders the fully qualified revision resource name.
func (r Revision) ResourceName() string {
	return r.Service.ResourceName() + "/revisions/" + r.Revision
}

// ConfigRef returns the ref of the settings this revision was deployed with.
//
// It is a method on Revision rather than on Service because a Cloud Run revision **is** the immutable
// snapshot of a service's template: the containers, the environment, the volumes and the service
// account, frozen at deploy time with a `createTime` that says when. A configuration version keyed on
// anything else would be a version whose start nobody can date.
func (r Revision) ConfigRef() *graphv1.Ref {
	return feeder.Ref(NSConfig, r.Service.Value()+"@"+r.Revision)
}

// ParseServiceResourceName reads GCP's `projects/P/locations/L/services/S` back into coordinates.
// It is how an audit entry's `resourceName` becomes the ref of the service it changed, and it is
// strict: a name that does not have this shape yields ok=false rather than a partly-filled
// Service, because a Service with an empty region addresses a different entity.
func ParseServiceResourceName(name string) (Service, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	// A revision's resource name extends a service's, so accept the prefix and ignore the rest:
	// an UpdateService entry naming a revision is still an entry about that service.
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "services" {
		return Service{}, false
	}
	svc := Service{Project: parts[1], Region: parts[3], Name: parts[5]}
	if svc.Validate() != nil {
		return Service{}, false
	}
	return svc, true
}

// ParseRevisionResourceName reads `projects/P/locations/L/services/S/revisions/R`.
func ParseRevisionResourceName(name string) (Revision, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) != 8 || parts[6] != "revisions" {
		return Revision{}, false
	}
	svc, ok := ParseServiceResourceName(name)
	if !ok {
		return Revision{}, false
	}
	rev := Revision{Service: svc, Revision: parts[7]}
	if rev.Validate() != nil {
		return Revision{}, false
	}
	return rev, true
}

// RevisionName extracts the bare revision name from a fully qualified one, or returns value
// unchanged when it is already bare. Cloud Run spells the same revision both ways depending on
// which field you read it from — `trafficStatuses[].revision` is bare, `Revision.name` is
// qualified — and a feeder that treated the two as different revisions would double the graph.
func RevisionName(value string) string {
	if idx := strings.LastIndex(value, "/revisions/"); idx >= 0 {
		return value[idx+len("/revisions/"):]
	}
	if idx := strings.LastIndex(value, "/"); idx >= 0 && strings.HasPrefix(value, "projects/") {
		return value[idx+1:]
	}
	return value
}

// Claim is one identifier this feeder knows an entity by, with the namespace it lives in.
//
// An alias of the SDK's: the type was written here first and then again, field for field, in the
// vendor-notice feeder, and the deploy feeders would have made it four. It moved to pkg/feeder rather
// than being copied a third time (004 T056). The alias keeps every use here compiling, and
// TestTheClaimsThisFeederEmitsCarryWhatThePublishedRulesRead still catches a rule whose supporting
// attribute this feeder never emits.
type Claim = feeder.Claim

// Claims returns every identifier this feeder knows a service by, **including the one it
// addresses the service by** (FR-115).
//
// `otelServiceName` is the OpenTelemetry service name the service declares, where it declares
// one, and it is passed separately rather than read from labels because FR-118's *certain*
// resolution rule depends on it: a declared name is evidence an operator wrote down, and a name
// guessed from a label is not.
func (s Service) Claims(otelServiceName string, envVarNames []string) []Claim {
	claims := []Claim{
		// The addressing ref, first and always. See the file comment.
		{Namespace: NSService, Value: s.Value(), Why: "the ref this feeder addresses the service by"},
		{Namespace: NSService, Value: s.ResourceName(), Why: "the fully qualified Cloud Run resource name"},
	}
	// The environment-variable NAMES this service defines, on the claims about the service itself.
	// The names only: a value is configuration and is dropped (FR-034), and P4's suggestion stands
	// on the name. It goes on the addressing claims rather than only on the declared-name claim
	// because P4 pairs the *service* with a Cloud SQL instance, and the declared name is a claim
	// about a telemetry identifier.
	if joined := strings.Join(sortedUniqueNames(envVarNames), " "); joined != "" {
		for i := range claims {
			claims[i].Attrs = map[string]string{AttrEnvVarNames: joined}
		}
	}
	if name := strings.TrimSpace(otelServiceName); name != "" {
		claims = append(claims, Claim{
			Namespace: feeder.NSOTelService,
			Value:     name,
			Why:       "the OpenTelemetry service name this service declares",
			// The marker C5 orients on: this side is the *declaration*, and its value is where the
			// declaration was read from. Without it the rule cannot tell the declaring side from
			// an observed name, so it would fire on two observed names — which is C1's business.
			Attrs: map[string]string{AttrDeclaredServiceName: EnvVarOTelServiceName},
		})
	}
	return claims
}

// AttrDeclaredServiceName, AttrEnvVarNames and the revision-locating attributes are the claim
// attributes the published resolution rules read. They are spelled here and asserted equal to
// `internal/resolution`'s constants in this package's tests: the two packages must agree, and the
// consequence of disagreeing is a rule that silently never fires.
//
// The revision-locating three reuse the property names the same facts are recorded under on a node
// (PropProject, PropRegion, PropRevisionLabel), because a reader looking at a claim and a reader
// looking at a node are asking the same question.
const (
	// AttrDeclaredServiceName marks a claim as the OpenTelemetry service name a Cloud Run service
	// declares, valued by the environment variable it was read from (C5, FR-118).
	AttrDeclaredServiceName = "sre.gcp.declared_service_name"
	// AttrEnvVarNames is the set of environment-variable names a service defines, joined by
	// spaces — names only, never values (P4, FR-034).
	AttrEnvVarNames = "sre.gcp.env_var_names"
)

// sortedUniqueNames trims, de-duplicates and sorts, so that a claim does not change because a
// container's environment was declared in a different order.
func sortedUniqueNames(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	slices.Sort(out)
	return out
}

// Claims returns every identifier this feeder knows a revision by, including its addressing ref.
//
// The image **digest** is claimed beside the tag-bearing reference, and that is the claim that
// does work: a deploy pipeline knows the digest, a tag is mutable, and the digest is what lets
// feature 004's build-system changes resolve against this revision rather than duplicate it.
func (r Revision) Claims(imageRef, imageDigest string) []Claim {
	claims := []Claim{
		{Namespace: NSRevision, Value: r.Value(), Why: "the ref this feeder addresses the revision by"},
		{Namespace: NSRevision, Value: r.ResourceName(), Why: "the fully qualified Cloud Run resource name"},
	}
	// The three attributes C4 locates a revision by, on every claim about it. All three are
	// required by the rule and that is not belt-and-braces: a revision name is unique within a
	// service, not globally, so `checkout-00042-abc` in two projects is two revisions, and a match
	// on the name alone would merge across projects.
	locating := map[string]string{
		PropRevisionLabel: r.Revision,
		PropProject:       r.Project,
		PropRegion:        r.Region,
	}
	if ref := strings.TrimSpace(imageRef); ref != "" {
		claims = append(claims, Claim{
			Namespace: NSRevision, Value: ref, Why: "the container image reference this revision runs",
		})
	}
	if digest := strings.TrimSpace(imageDigest); digest != "" {
		claims = append(claims, Claim{
			Namespace: NSRevision, Value: digest,
			Why: "the image digest, which is the identifier a build pipeline also knows",
		})
	}
	for i := range claims {
		// Cloned per claim rather than shared: four claims pointing at one map is a trap for
		// whoever next wants to add an attribute to only one of them.
		claims[i].Attrs = maps.Clone(locating)
	}
	return claims
}
