// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"fmt"
	"sort"
	"time"
)

// The published read-only operation list, and the request log that proves it was honoured
// (FR-005, FR-112, SC-020; docs/connectors/gcp.md §2).
//
// ---------------------------------------------------------------------------------------------
// Why a list of OPERATIONS and not only a classifier over permissions.
//
// classify.go answers "may this credential write?", which is the gate's question at startup. It is
// a necessary layer and it is not SC-020's: SC-020 asks what the integration is **capable of
// issuing**, "verified from the set of operations the integration can issue, not only from one
// run's behaviour". A credential that holds only reads still leaves the question of which reads,
// how many, and whether the page an operator approved is the page the code implements.
//
// So the list below is the source of truth and every call goes through it. `Budget.Issue` looks an
// operation up here, derives its endpoint class from the table rather than from the call site, and
// refuses an operation the list does not carry. Three drifts stop being possible at once:
//
//   - a call site cannot issue an operation that is not published, because the constant would not
//     exist;
//   - a call site cannot meter an operation against the wrong class, because it does not name one;
//   - the published page cannot drift from the code, because a test parses the page's table and
//     compares it to this map (requestlog_test.go).
//
// The one thing this cannot prove is stated on the page itself, under "What the three-layer gate
// cannot prove": a `list` that Google implements with a side effect would pass every layer here.
// Nothing inside a client can see that.

// Operation is one GCP API operation this integration may issue, in the `service.resource.verb`
// spelling §2's table uses.
//
// For most rows that spelling IS the IAM permission, which is the right identifier: it is what an
// operator grants, what §1's role table is written in, and what classify.go reads. Three rows are
// the method's name instead, because no permission of their own exists, and they are worth naming
// because a reader who assumes otherwise would go looking for a grant that cannot be made:
//
//	cloudsql.operations.list     authorised by cloudsql.instances.get; no permission of its own
//	cloudsql.flags.list          the endpoint is project-less and gated purely by OAuth scope
//	pubsub.subscriptions.pull    the METHOD; the permission it needs is subscriptions.consume,
//	                             which is why the doorbell cannot be granted read-only at any
//	                             granularity (§2.1)
//
// Naming the method rather than inventing a permission is deliberate. An operator reads this page to
// answer "what can this thing do", and the honest answer for those three is a call, not a grant.
type Operation string

// The published list. Every constant here appears in docs/connectors/gcp.md §2, and a test fails
// the build if the two ever disagree in either direction.
const (
	// Cloud Run: the serving topology (US1).
	OpRunServicesList  Operation = "run.services.list"
	OpRunServicesGet   Operation = "run.services.get"
	OpRunRevisionsList Operation = "run.revisions.list"
	OpRunRevisionsGet  Operation = "run.revisions.get"

	// Cloud Logging: admin activity only. The data-access stream is never read (FR-042).
	OpLoggingEntriesList Operation = "logging.logEntries.list"

	// Cloud Monitoring: the GA surface, shared by the feeder and the telemetry backend.
	OpMonitoringAlertPoliciesList    Operation = "monitoring.alertPolicies.list"
	OpMonitoringTimeSeriesList       Operation = "monitoring.timeSeries.list"
	OpMonitoringMetricDescriptorsGet Operation = "monitoring.metricDescriptors.get"
	// OpMonitoringAlertsList is Public Preview and opt-in. It is on the list because the
	// integration is capable of issuing it once the capability is declared, and SC-020 asks
	// what the integration can issue rather than what one run did.
	OpMonitoringAlertsList Operation = "monitoring.alerts.list"

	// Cloud SQL (US5). `cloudsql.instances.export` is deliberately absent and is why
	// roles/cloudsql.viewer is unusable — see §1's custom role.
	OpSQLInstancesList  Operation = "cloudsql.instances.list"
	OpSQLInstancesGet   Operation = "cloudsql.instances.get"
	OpSQLOperationsList Operation = "cloudsql.operations.list"
	OpSQLFlagsList      Operation = "cloudsql.flags.list"

	// GKE: cluster metadata, and nothing inside a cluster (FR-030).
	OpContainerClustersList Operation = "container.clusters.list"
	OpContainerClustersGet  Operation = "container.clusters.get"

	// Load balancers (P3, US9).
	OpComputeForwardingRulesList      Operation = "compute.forwardingRules.list"
	OpComputeURLMapsList              Operation = "compute.urlMaps.list"
	OpComputeBackendServicesList      Operation = "compute.backendServices.list"
	OpComputeNetworkEndpointGroupList Operation = "compute.networkEndpointGroups.list"

	// Cloud DNS (P3, US9).
	OpDNSManagedZonesList      Operation = "dns.managedZones.list"
	OpDNSResourceRecordSetList Operation = "dns.resourceRecordSets.list"

	// The doorbell, off by default: the ONE declared state change anywhere (FR-006).
	OpPubSubSubscriptionsPull Operation = "pubsub.subscriptions.pull"
)

// OperationSpec is what the published page says about one operation.
type OperationSpec struct {
	// Area is the page's first column: the operator-facing grouping a role is granted for.
	Area string
	// Class is the endpoint class the call is metered against. It lives here rather than at the
	// call site so a call cannot be metered against the wrong pool by a copy-paste.
	Class EndpointClass
	// StateChange is empty for every read. On the one declared exception it names the change, in
	// the words the read-only statement uses — so a request log can be read for state changes
	// without anyone having to know which row is special.
	StateChange string
	// Preview marks an operation behind a declared capability that is off unless the operator
	// turns it on. It is still published, because capability-of-issuing is the question.
	Preview bool
}

// doorbellStateChange is the only non-empty StateChange in this file, and the only one SC-020
// permits: "the acknowledgement of messages on the operator's dedicated doorbell subscription".
const doorbellStateChange = "acknowledges messages on the operator's dedicated doorbell subscription"

var readOnlyOperations = map[Operation]OperationSpec{
	OpRunServicesList:  {Area: "Cloud Run", Class: ClassRunRead},
	OpRunServicesGet:   {Area: "Cloud Run", Class: ClassRunRead},
	OpRunRevisionsList: {Area: "Cloud Run", Class: ClassRunRead},
	OpRunRevisionsGet:  {Area: "Cloud Run", Class: ClassRunRead},

	OpLoggingEntriesList: {Area: "Cloud Logging", Class: ClassLoggingRead},

	OpMonitoringAlertPoliciesList:    {Area: "Cloud Monitoring", Class: ClassMonitoringPolicies},
	OpMonitoringTimeSeriesList:       {Area: "Cloud Monitoring", Class: ClassMonitoringQuery},
	OpMonitoringMetricDescriptorsGet: {Area: "Cloud Monitoring", Class: ClassMonitoringQuery},
	OpMonitoringAlertsList:           {Area: "Cloud Monitoring (Preview, opt-in)", Class: ClassMonitoringPolicies, Preview: true},

	OpSQLInstancesList:  {Area: "Cloud SQL", Class: ClassSQLAdminRead},
	OpSQLInstancesGet:   {Area: "Cloud SQL", Class: ClassSQLAdminRead},
	OpSQLOperationsList: {Area: "Cloud SQL", Class: ClassSQLAdminRead},
	OpSQLFlagsList:      {Area: "Cloud SQL", Class: ClassSQLAdminRead},

	OpContainerClustersList: {Area: "GKE", Class: ClassContainerRead},
	OpContainerClustersGet:  {Area: "GKE", Class: ClassContainerRead},

	OpComputeForwardingRulesList:      {Area: "load balancers (P3)", Class: ClassComputeRead},
	OpComputeURLMapsList:              {Area: "load balancers (P3)", Class: ClassComputeRead},
	OpComputeBackendServicesList:      {Area: "load balancers (P3)", Class: ClassComputeRead},
	OpComputeNetworkEndpointGroupList: {Area: "load balancers (P3)", Class: ClassComputeRead},

	OpDNSManagedZonesList:      {Area: "Cloud DNS (P3)", Class: ClassDNSRead},
	OpDNSResourceRecordSetList: {Area: "Cloud DNS (P3)", Class: ClassDNSRead},

	OpPubSubSubscriptionsPull: {
		Area:        "the doorbell (off by default)",
		Class:       ClassPubSubPull,
		StateChange: doorbellStateChange,
	},
}

// UnpublishedOperationError is the refusal SC-020 turns on: a call named an operation the published
// list does not carry, so it is not issued.
//
// It is unreachable through the constants above — that is the point. It exists for the case an
// operation is added to a call site by name, or by a future caller outside this repository, and it
// makes the failure a refusal at the call rather than a review finding after the fact.
type UnpublishedOperationError struct {
	Op Operation
}

func (e *UnpublishedOperationError) Error() string {
	return fmt.Sprintf("gcp: %q is not on the published read-only operation list, so it was not "+
		"issued; the list is docs/connectors/gcp.md §2 and internal/gcpx/requestlog.go (FR-005, SC-020)", e.Op)
}

// WriteOperationError is defence in depth behind UnpublishedOperationError: an operation that IS on
// the list but classifies as a write. A test asserts the list holds no such row, so reaching this
// means the list was edited and the test that guards it was edited with it.
type WriteOperationError struct {
	Op Operation
}

func (e *WriteOperationError) Error() string {
	return fmt.Sprintf("gcp: %q is on the published list but classifies as a write; the integration "+
		"issues no mutating operation (FR-005, SC-020)", e.Op)
}

// Issuable decides whether an operation may be issued at all, before any quota is spent.
//
// It is deliberately independent of the budget: a nil budget is an unmetered run — a replay from
// disk, a unit test — and "we are not counting calls" is never a reason to stop checking what may
// be called.
func Issuable(op Operation) (OperationSpec, error) {
	spec, ok := readOnlyOperations[op]
	if !ok {
		return OperationSpec{}, &UnpublishedOperationError{Op: op}
	}
	if spec.StateChange != "" {
		// The declared exception. It is allowed, it is named, and the request log records the
		// change so it appears in the report rather than only in the prose.
		return spec, nil
	}
	if IsWrite(string(op)) {
		return OperationSpec{}, &WriteOperationError{Op: op}
	}
	return spec, nil
}

// Operations returns the published list, sorted, so the page and the report are generated from the
// same source the gate enforces.
func Operations() []Operation {
	out := make([]Operation, 0, len(readOnlyOperations))
	for op := range readOnlyOperations {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SpecOf returns one operation's published entry.
func SpecOf(op Operation) (OperationSpec, bool) {
	spec, ok := readOnlyOperations[op]
	return spec, ok
}

// RequestRow is one line of the request log: an operation, who issued it, and how often.
//
// It is per consumer and per operation rather than per call, because the artifact SC-020 checks is
// "every operation the integration issued", and a row per call would be a payload log — which this
// deliberately is not. Nothing here is derived from a response.
type RequestRow struct {
	Consumer  Consumer
	Operation Operation
	Area      string
	Class     EndpointClass
	// Calls is how many times the operation was issued and served; Refused how many times the
	// budget yielded before it was issued. A refusal is not a request: it never reached GCP.
	Calls   int
	Refused int
	// Blocked is how many times the operation was NOT issued because the published list does not
	// carry it, and Refusal is why.
	//
	// These exist so the log records an ATTEMPT rather than only what succeeded, and that is
	// deliberate: SC-020's claim is that every request the integration is CAPABLE of issuing is on
	// the published list, so an attempt the list stopped falsifies the claim even though no write
	// reached GCP. A log that recorded only permitted calls would report a clean run in exactly the
	// case an operator most needs told — and ReadOnlyHonoured would be a guard that cannot fail.
	Blocked int
	Refusal string
	// StateChange is copied from the published spec, so a reader of the log alone can answer
	// SC-020's second half — "the only state change anywhere is the doorbell acknowledgement" —
	// without consulting the table.
	StateChange string
	First       time.Time
	Last        time.Time
}

// ReadOnly reports whether this row changed nothing in the organisation's projects.
func (r RequestRow) ReadOnly() bool { return r.StateChange == "" }
