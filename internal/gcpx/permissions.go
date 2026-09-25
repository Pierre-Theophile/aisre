// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"fmt"
	"sort"
	"strings"

	cloudresourcemanager "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Layer 1 of the read-only gate: ask GCP what this principal may do, and refuse to start if it may
// write anywhere this integration reads (FR-004, FR-112, SC-020; research §8).
//
// The reasoning is the one internal/feeders/k8s/permissions.go established and it transfers
// verbatim: *"no component writes to production" is not a promise a connector can make by being
// careful. It is a property of the credential it holds, and the only honest way to state it is to
// ask the system.* A project-owner service account pointed at production is a loaded gun whether or
// not the code calls a mutating method.
//
// `testIamPermissions` is the right primitive and has one excellent property: **no IAM role is
// required to call it.** The gate therefore costs the principal nothing, which is what makes it
// acceptable as an always-on startup check rather than a thing operators disable.
//
// ---------------------------------------------------------------------------------------------
// What this layer is NOT, stated here so nobody upgrades the claim later.
//
// It is **weaker than Kubernetes' SelfSubjectRulesReview**: it tests a SUPPLIED LIST and returns the
// held subset. There is no GCP API that enumerates the permissions a caller holds, and every
// candidate that looks like one fails for the same reason — asking costs more privilege than this
// connector should hold. Policy Troubleshooter needs roles/iam.securityReviewer on the ORGANIZATION
// (2,547 permissions, including secretmanager.secrets.list and cloudkms.cryptoKeyVersions.list):
// granting that so a read-only connector can introspect itself inverts the control (research §8).
//
// So the honest claim, which docs/connectors/gcp.md makes and this comment will not quietly inflate:
// **a zero-cost, high-value tripwire that reliably catches the common failure — someone binding
// roles/editor to the connector's service account at project level — and not a proof.** What it
// cannot reach, layer 3 makes an operator assert by name.

// The permission limit per call is NOT documented, and this code must not pretend it is.
//
// No maximum appears in the v1 or v3 references, the IAM testing guide, four live discovery
// documents, or the google/iam/v1 proto — only the wildcard restriction. The "100" that surfaces in
// search results belongs to a DIFFERENT method, queryTestablePermissions.pageSize. So: chunk at a
// tunable size, tolerate INVALID_ARGUMENT, halve on failure, and never assert a documented limit.
const (
	// DefaultPermissionChunk is a starting size, not a known limit.
	DefaultPermissionChunk = 100
	// MinPermissionChunk is where halving stops. Below this the API is refusing for some reason
	// other than size, and continuing to halve would turn one bad response into twenty.
	MinPermissionChunk = 5
)

// GateLayer names which layer objected, because FR-004 requires the refusal to say.
type GateLayer string

const (
	LayerIAM        GateLayer = "layer 1: IAM testIamPermissions"
	LayerTokenScope GateLayer = "layer 2: OAuth token scope"
	LayerAssertion  GateLayer = "layer 3: operator assertion"
)

// GateResult is the evidence a gate run produced: what each layer checked, what it found, and — the
// part that matters most — what it could not verify.
//
// FR-004's clause about naming the unverified part and requiring an operator assertion is not a
// hedge. Given research §8.1 it is the only truthful design, so the unverified list is a first-class
// field here rather than a log line.
type GateResult struct {
	// Held is every write permission the principal turned out to hold. Non-empty means refusal.
	Held []string
	// Tested is how many permissions layer 1 actually asked about, so a reader can tell a clean
	// answer from a small question.
	Tested int
	// Unverifiable names, by area, what no layer could check. It is never empty in practice —
	// Cloud SQL, Cloud Monitoring and log-entry reads have no per-resource test at all.
	Unverifiable []string
	// Scopes is layer 2's finding.
	Scopes []string
	// ScopeWiderThanAsked is true when the environment supplied a credential broader than the
	// read-only set this integration requests — cloud-platform rather than
	// cloud-platform.read-only. Not fatal by itself; reported loudly because layer 1 is blind to it.
	ScopeWiderThanAsked bool
	// AssertedBy is the operator who asserted the untestable remainder (layer 3).
	AssertedBy string
	// ChunkSize is the size layer 1 settled on after any halving, recorded because it is evidence
	// about an undocumented limit that the next reader would otherwise have to rediscover.
	ChunkSize int
}

// GateRefusal is the typed refusal. It names the layer, which FR-004 requires.
type GateRefusal struct {
	Layer  GateLayer
	Detail string
	Result *GateResult
}

func (e *GateRefusal) Error() string {
	return fmt.Sprintf("gcpx: refusing to start — %s: %s", e.Layer, e.Detail)
}

// WritePermissions is the enumerated set layer 1 tests, per area this integration reads.
//
// Enumerated rather than derived, because there is no API that answers "what could this principal
// write". A list that is too short is a gate that passes things it should not, so the bias
// throughout is to include a permission that may not apply rather than to leave one out.
func WritePermissions() []string {
	perms := []string{
		// Impersonation, first because it is the one people forget. A principal that can
		// impersonate is not read-only however clean the rest looks: it can mint a token for
		// something that writes (research §8).
		"iam.serviceAccounts.getAccessToken",
		"iam.serviceAccounts.signJwt",
		"iam.serviceAccounts.signBlob",
		"iam.serviceAccounts.implicitDelegation",
		"iam.serviceAccounts.actAs",

		// Cloud Run.
		"run.services.create", "run.services.update", "run.services.delete",
		"run.services.setIamPolicy", "run.revisions.delete", "run.jobs.run",
		"run.routes.invoke",

		// Cloud SQL. instances.export is here because it is why roles/cloudsql.viewer
		// disqualifies itself: a long-running export that writes a dump to a bucket is data
		// egress with a side effect (research §8.2).
		"cloudsql.instances.create", "cloudsql.instances.update", "cloudsql.instances.delete",
		"cloudsql.instances.export", "cloudsql.instances.import", "cloudsql.instances.restoreBackup",
		"cloudsql.backupRuns.create", "cloudsql.backupRuns.delete", "cloudsql.backupRuns.export",
		"cloudsql.databases.create", "cloudsql.databases.update", "cloudsql.databases.delete",
		"cloudsql.users.create", "cloudsql.users.update", "cloudsql.users.delete",

		// Cloud Logging. logMetrics.create is the one new_log_patterns would want and may not
		// have: creating a log-based metric is a mutation and is not retroactive anyway
		// (research §6).
		"logging.logMetrics.create", "logging.logMetrics.update", "logging.logMetrics.delete",
		"logging.sinks.create", "logging.sinks.update", "logging.sinks.delete",
		"logging.buckets.create", "logging.buckets.update", "logging.buckets.delete",
		"logging.logs.delete",

		// Cloud Monitoring.
		"monitoring.timeSeries.create",
		"monitoring.alertPolicies.create", "monitoring.alertPolicies.update",
		"monitoring.alertPolicies.delete",
		"monitoring.notificationChannels.create", "monitoring.notificationChannels.update",
		"monitoring.notificationChannels.delete",
		"monitoring.dashboards.create", "monitoring.dashboards.update", "monitoring.dashboards.delete",

		// Pub/Sub. consume is the ONE declared exception (FR-006) and is tested anyway: the
		// accurate statement is that holding it at all is the exception, so the gate should
		// report it rather than pretend it is absent (research §8.3).
		"pubsub.topics.create", "pubsub.topics.update", "pubsub.topics.delete",
		"pubsub.topics.publish",
		"pubsub.subscriptions.create", "pubsub.subscriptions.update", "pubsub.subscriptions.delete",

		// Compute and DNS, read only where enabled at all (FR-057).
		"compute.urlMaps.insert", "compute.urlMaps.update", "compute.urlMaps.delete",
		"compute.backendServices.insert", "compute.backendServices.update",
		"compute.backendServices.delete",
		"compute.forwardingRules.insert", "compute.forwardingRules.delete",
		"dns.managedZones.create", "dns.managedZones.update", "dns.managedZones.delete",
		"dns.changes.create", "dns.resourceRecordSets.create", "dns.resourceRecordSets.update",
		"dns.resourceRecordSets.delete",

		// GKE metadata only; the workloads belong to the 001 Kubernetes feeder (FR-030).
		"container.clusters.create", "container.clusters.update", "container.clusters.delete",

		// Project-level. Anyone holding these holds everything above by implication.
		"resourcemanager.projects.setIamPolicy", "resourcemanager.projects.delete",
		"resourcemanager.projects.update",
		"serviceusage.services.enable", "serviceusage.services.disable",
	}
	sort.Strings(perms)
	return perms
}

// UnverifiableAreas is what layer 1 cannot reach and layer 3 must therefore assert (research §8.1).
//
// Listed by name rather than summarised, because "some things could not be checked" is exactly the
// sentence that lets a reader assume the remainder is small.
func UnverifiableAreas() []string {
	return []string{
		"child-resource bindings: IAM inherits DOWNWARD only, so a clean project-level answer does " +
			"not prove the principal lacks write on one Cloud Run service, one subscription or one " +
			"managed zone",
		"Cloud SQL, Cloud Monitoring and log-entry reads: no per-resource testIamPermissions exists " +
			"at all, so this gap cannot be closed by enumerating harder",
		"cloudsql.flags.list: not an IAM permission — the flags endpoint is project-less and gated " +
			"purely by OAuth scope, which is layer 2's business",
		"deny policies: whether testIamPermissions subtracts them is undocumented. This fails SAFE " +
			"— a deny-blocked write may still be reported as held, so the integration refuses to " +
			"start: a false positive, not a false negative",
		"conditional bindings: the method takes no request context, so a time- or " +
			"attribute-conditioned write grant may evaluate differently at test time than at use time",
		"PAB policies and VPC Service Controls: not documented as evaluated, and VPC-SC is not IAM",
		"everything that is not GCP IAM: in-database GRANTs, Workspace OAuth scopes, API keys, and " +
			"resources in other projects",
	}
}

// IAMTester is the one call layer 1 makes. An interface so the gate is testable without a network:
// a gate that could only be exercised against real GCP is a gate nobody runs.
type IAMTester interface {
	TestPermissions(ctx context.Context, project string, permissions []string) ([]string, error)
}

// liveIAMTester calls cloudresourcemanager.projects.testIamPermissions.
type liveIAMTester struct {
	svc   *cloudresourcemanager.Service
	usage *Usage
}

// NewIAMTester builds the live tester over a discovered credential.
func NewIAMTester(ctx context.Context, tokenSource option.ClientOption, usage *Usage) (IAMTester, error) {
	svc, err := cloudresourcemanager.NewService(ctx, tokenSource)
	if err != nil {
		return nil, fmt.Errorf("gcpx: cloud resource manager client: %w", err)
	}
	return &liveIAMTester{svc: svc, usage: usage}, nil
}

func (t *liveIAMTester) TestPermissions(ctx context.Context, project string, permissions []string) ([]string, error) {
	t.usage.record(EndpointClass("resourcemanager.testIamPermissions"), "gate|"+project, 1, true, 0, 0)
	resp, err := t.svc.Projects.TestIamPermissions(project,
		&cloudresourcemanager.TestIamPermissionsRequest{Permissions: permissions}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Permissions, nil
}

// TestWritePermissions asks, in chunks, which of the enumerated write permissions the principal
// holds on project.
//
// The chunking is the interesting part, and it is written to be honest about not knowing the limit:
// start at a tunable size, and on INVALID_ARGUMENT halve and retry rather than fail. That converges
// on whatever the real limit is without this code ever claiming to know it, and it records the size
// it settled on so the evidence outlives the run.
func TestWritePermissions(ctx context.Context, tester IAMTester, project string, permissions []string, chunk int) ([]string, int, error) {
	if chunk <= 0 {
		chunk = DefaultPermissionChunk
	}
	var held []string
	for i := 0; i < len(permissions); {
		end := min(i+chunk, len(permissions))
		got, err := tester.TestPermissions(ctx, project, permissions[i:end])
		if err != nil {
			if isInvalidArgument(err) && chunk > MinPermissionChunk {
				chunk /= 2
				continue // same i, smaller bite
			}
			return nil, chunk, fmt.Errorf("gcpx: test permissions on %s: %w", project, err)
		}
		held = append(held, got...)
		i = end
	}
	sort.Strings(held)
	return held, chunk, nil
}

func isInvalidArgument(err error) bool {
	var apiErr *googleapi.Error
	if ok := errorsAs(err, &apiErr); ok {
		return apiErr.Code == 400
	}
	return strings.Contains(err.Error(), "INVALID_ARGUMENT")
}
