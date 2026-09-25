// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	dns "google.golang.org/api/dns/v1"
	"google.golang.org/api/iterator"
	monitoringv3 "google.golang.org/api/monitoring/v3"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	metricpb "google.golang.org/genproto/googleapis/api/metric"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// The transport seam: the one place in this repository that constructs a per-area Google Cloud
// client (FR-008; contracts/gcp-feeder.md §3–§7).
//
// Everything above this file works against the interfaces below, which is what makes live mode and
// recorded mode **one code path** rather than two implementations that agree until they do not. The
// recorder tees the decoded payloads these methods return; the replayer hands the same values back
// from disk. So FR-008's "nothing derives from the connection" is structural rather than a rule
// somebody remembers: a caller holding one of these interfaces cannot tell which side it is on, and
// has no client to reach around them with.
//
// The payload types ARE the vendor's decoded messages — runpb.Service, loggingpb.LogEntry and so on.
// That is deliberate. A private mirror of each would be a second schema to keep in step with
// Google's, and the recorded corpus is a recording of what Google actually returned; inventing a
// shape in between would make the fixtures a recording of this code's opinion instead.
//
// Where the split with internal/gcpx falls, since both files are about talking to GCP:
//
//	internal/gcpx      POLICY. Credentials and scopes, the three-layer read-only gate, the call
//	                   budget, quota, backoff, the usage report. It touches
//	                   cloudresourcemanager and the token endpoint — the APIs that answer
//	                   questions ABOUT the credential — and never an area this feeder reads.
//	transport.go       READS. One narrow interface per area, and the only imports of the per-area
//	                   clients (run, logging, monitoring, sqladmin, pubsub).
//
// Every method here takes the budget through gcpx before it issues a call, so there is no path that
// reads GCP without being metered (contracts/budget.md).

// CloudRunReader reads Cloud Run services and revisions (contract §3.1).
//
// Revisions accept "-" as the service id, which lists every service's revisions in one location in
// one call, returned newest-first — exactly the order a history horizon walks.
type CloudRunReader interface {
	ListServices(ctx context.Context, project, region string) ([]*runpb.Service, error)
	ListRevisions(ctx context.Context, project, region, service string) ([]*runpb.Revision, error)
}

// CloudSQLReader reads Cloud SQL instances and the flag catalogue (contract §4).
//
// Note the role this needs is a CUSTOM one: roles/cloudsql.viewer grants
// cloudsql.instances.export, a data-egress capability, and disqualifies itself under FR-004
// (research §8.2). And cloudsql.flags.list does not exist as a permission at all — the flags
// endpoint is project-less and gated purely by OAuth scope, which is layer 2's business.
type CloudSQLReader interface {
	ListInstances(ctx context.Context, project string) ([]*sqladmin.DatabaseInstance, error)
	ListFlags(ctx context.Context) ([]*sqladmin.Flag, error)
	// ListOperations reads one instance's operation history, for PAST maintenance (contract §4).
	//
	// It is per instance and not per project, because `operations.list` without an `instance`
	// filter returns every operation in the project — backups, imports, restarts, connectivity
	// tests — and this feeder reads two operation types. Filtering server-side on the instance and
	// client-side on the type costs one call per instance and reads two orders of magnitude less.
	ListOperations(ctx context.Context, project, instance string) ([]*sqladmin.Operation, error)
}

// GKEReader reads GKE cluster metadata and nothing inside a cluster (contract §7, FR-030).
//
// The interface has one method on purpose. `clusters.list` returns node pools in the same response,
// and the prohibition in gke.go is that they are not emitted — but an interface that also offered
// `ListNodePools` would be an invitation, and the cheapest way to keep a prohibition is to give the
// code no way to break it.
type GKEReader interface {
	ListClusters(ctx context.Context, project string) ([]*container.Cluster, error)
}

// AuditLogReader reads the Cloud Audit Logs admin-activity stream, and log entries generally
// (contract §5).
//
// This is the binding one for quota: entries.list is 60 calls per minute per project, the limit is
// not hierarchical, and it is shared with every human querying Cloud Logging in that project
// (contracts/budget.md). The page size is whatever Google returns — its maximum is undocumented and
// the widely-cited 1,000-entry cap appears nowhere in the reference (research §6).
type AuditLogReader interface {
	ListEntries(ctx context.Context, req *loggingpb.ListLogEntriesRequest) ([]*loggingpb.LogEntry, string, error)
}

// MonitoringReader reads alert policies and time series (contract §6).
//
// Alert INCIDENTS are deliberately absent from this interface: projects.alerts is Public Preview and
// bound only in the deprecated google.golang.org/api/monitoring/v3, so it sits behind a capability
// flag in its own reader rather than in the path every poll takes (research §12).
type MonitoringReader interface {
	ListAlertPolicies(ctx context.Context, project string) ([]*monitoringpb.AlertPolicy, error)
	ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) ([]*monitoringpb.TimeSeries, error)
	// GetMetricDescriptor reads one metric type's descriptor, for its `metadata.ingestDelay`.
	//
	// It is here rather than in the telemetry backend because this file is the only one in the
	// repository that constructs a per-area client, and because the figure it fetches is the
	// same figure on both sides: the lag that decides NOT_YET_INGESTED for an investigation is
	// the lag that decides whether an ingestion run has seen everything it will see.
	GetMetricDescriptor(ctx context.Context, project, metricType string) (*metricpb.MetricDescriptor, error)
}

// ExposureReader reads the load-balancer chain and Cloud DNS (contract §7, FR-055–FR-057).
//
// It is **one** interface over both surfaces rather than two, and that is FR-057's rule expressed in the
// type: where either is outside the budget or the credential's permissions, the connector reads
// **neither**. Two interfaces would let a caller hold one and not the other, and half an exposure graph
// looks complete and answers wrongly — host names nothing resolves, or records pointing at addresses no
// node carries.
//
// The role is `roles/compute.viewer`, not `roles/compute.networkViewer`: that one grants
// `trafficdirector.networks.reportMetrics`, which writes, and disqualifies itself under FR-004
// (research §8.2). It is the one case where the wider role is the safer one.
type ExposureReader interface {
	ListForwardingRules(ctx context.Context, project string) ([]*compute.ForwardingRule, error)
	ListURLMaps(ctx context.Context, project string) ([]*compute.UrlMap, error)
	ListBackendServices(ctx context.Context, project string) ([]*compute.BackendService, error)
	// ListNetworkEndpointGroups is the hop that makes the chain a derivation rather than a name
	// match: a serverless NEG's `cloudRun.service` names the service it fronts.
	ListNetworkEndpointGroups(ctx context.Context, project, region string) ([]*compute.NetworkEndpointGroup, error)
	ListDNSRecordSets(ctx context.Context, project, zone string) ([]*dns.ResourceRecordSet, error)
	ListDNSZones(ctx context.Context, project string) ([]*dns.ManagedZone, error)
}

// Transport is the set of area readers a feeder run holds. A nil member is an area this run does not
// read — load balancers and DNS are off by default (FR-057), and the doorbell is off unless an
// operator configured one — so a nil is a stated scope rather than a missing dependency.
type Transport struct {
	Run        CloudRunReader
	SQL        CloudSQLReader
	AuditLogs  AuditLogReader
	Monitoring MonitoringReader
	// AlertIncidents reads alerting transitions. Nil unless the operator declared the capability:
	// the surface is Public Preview and its only binding is a maintenance-mode client, so a nil
	// here is the flag being off — a stated scope rather than a missing dependency.
	AlertIncidents AlertIncidentReader
	// GKE reads cluster metadata. Nil is a stated scope: an organisation with no cluster, or a
	// credential without container.clusters.get.
	GKE GKEReader
	// Exposure reads load balancers and Cloud DNS. Nil is the ordinary case, not an exception: these
	// are P3 and the first surface the deferral order drops (FR-057), so a nil here is a scope
	// statement the checkpoint carries — and every other requirement holds unchanged.
	Exposure ExposureReader
}

// liveTransport builds the real readers over one credential.
//
// The credential arrives already proved read-only: gcpx.Credential is only returned by a gate that
// passed, so there is no ordering in which a client here is constructed before the check that makes
// it safe (FR-004).
type liveTransport struct {
	creds  *gcpx.Credential
	budget *gcpx.Budget
}

// NewLiveTransport constructs the per-area clients. It is the only constructor in this repository
// that reaches Google's area APIs, and the only reason this file imports them.
func NewLiveTransport(ctx context.Context, creds *gcpx.Credential, budget *gcpx.Budget) (*Transport, error) {
	if creds == nil {
		return nil, gcpx.ErrNoCredential
	}
	lt := &liveTransport{creds: creds, budget: budget}

	opts := []option.ClientOption{option.WithTokenSource(creds.TokenSource())}

	runClient, err := run.NewServicesClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	revClient, err := run.NewRevisionsClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	logClient, err := logging.NewClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	monClient, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	seriesClient, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	sqlClient, err := sqladmin.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	gkeClient, err := container.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	// Load balancers and DNS are P3 and the first thing cut, so their clients are constructed only
	// when the caller asks for the surface. A nil Exposure is the default.
	return &Transport{
		Run:        &liveRun{lt: lt, services: runClient, revisions: revClient},
		SQL:        &liveSQL{lt: lt, svc: sqlClient},
		AuditLogs:  &liveAuditLogs{lt: lt, client: logClient},
		Monitoring: &liveMonitoring{lt: lt, policies: monClient, series: seriesClient},
		GKE:        &liveGKE{lt: lt, svc: gkeClient},
	}, nil
}

// ---- Cloud Run -------------------------------------------------------------------------------

type liveRun struct {
	lt        *liveTransport
	services  *run.ServicesClient
	revisions *run.RevisionsClient
}

func (r *liveRun) ListServices(ctx context.Context, project, region string) ([]*runpb.Service, error) {
	if err := r.lt.budget.Issue(ctx, gcpx.OpRunServicesList, project, region); err != nil {
		return nil, err
	}
	it := r.services.ListServices(ctx, &runpb.ListServicesRequest{
		Parent: "projects/" + project + "/locations/" + region,
	})
	var out []*runpb.Service
	for {
		svc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, svc)
	}
}

// ListRevisions lists a service's revisions, or every service's when service is "-".
func (r *liveRun) ListRevisions(ctx context.Context, project, region, service string) ([]*runpb.Revision, error) {
	if err := r.lt.budget.Issue(ctx, gcpx.OpRunRevisionsList, project, region); err != nil {
		return nil, err
	}
	it := r.revisions.ListRevisions(ctx, &runpb.ListRevisionsRequest{
		Parent: "projects/" + project + "/locations/" + region + "/services/" + service,
	})
	var out []*runpb.Revision
	for {
		rev, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, rev)
	}
}

// ---- Cloud SQL -------------------------------------------------------------------------------

type liveSQL struct {
	lt  *liveTransport
	svc *sqladmin.Service
}

func (s *liveSQL) ListInstances(ctx context.Context, project string) ([]*sqladmin.DatabaseInstance, error) {
	if err := s.lt.budget.Issue(ctx, gcpx.OpSQLInstancesList, project, ""); err != nil {
		return nil, err
	}
	resp, err := s.svc.Instances.List(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ListOperations pages through one instance's operations.
//
// It paginates rather than trusting the first page, because an instance that has existed for a year
// has hundreds of operations and the two this feeder wants are not necessarily on the first page. The
// page count is bounded: an unbounded loop over a paging API is a hang whenever the token stops
// advancing, which is not a hypothetical failure of Google's APIs.
func (s *liveSQL) ListOperations(ctx context.Context, project, instance string) ([]*sqladmin.Operation, error) {
	var out []*sqladmin.Operation
	token := ""
	for page := 0; page < MaxSQLOperationPages; page++ {
		if err := s.lt.budget.Issue(ctx, gcpx.OpSQLOperationsList, project, ""); err != nil {
			return out, err
		}
		call := s.svc.Operations.List(project).Instance(instance).
			MaxResults(SQLOperationPageSize).Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		resp, err := call.Do()
		if err != nil {
			return out, err
		}
		out = append(out, resp.Items...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		token = resp.NextPageToken
	}
	// The budget's own rule: stopping is stated by the caller's checkpoint, not by silently
	// returning a short list as though it were complete.
	return out, fmt.Errorf("gcp: operations.list for %s/%s did not finish within %d pages of %d; "+
		"the history is longer than this feeder reads in one poll",
		project, instance, MaxSQLOperationPages, SQLOperationPageSize)
}

// SQLOperationPageSize and MaxSQLOperationPages bound the operation read. `maxResults` on
// `operations.list` is documented as defaulting to 20; the ceiling is not documented, so a large
// value is requested and whatever comes back is accepted.
const (
	SQLOperationPageSize = 100
	MaxSQLOperationPages = 20
)

func (s *liveSQL) ListFlags(ctx context.Context) ([]*sqladmin.Flag, error) {
	if err := s.lt.budget.Issue(ctx, gcpx.OpSQLFlagsList, "", ""); err != nil {
		return nil, err
	}
	resp, err := s.svc.Flags.List().Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ---- Cloud Logging ---------------------------------------------------------------------------

type liveAuditLogs struct {
	lt     *liveTransport
	client *logging.Client
}

// ListEntries returns ONE page and its next-page token rather than draining the iterator.
//
// One page per call is the whole point at 60 calls/min per project: the caller decides whether to
// spend another, and the budget sees each page as the call it is. A method that drained internally
// would hide an unbounded number of calls behind a single metered Take, and the usage report would
// state a number that was not true.
//
// iterator.NewPager is the primitive that does this correctly. An earlier version of this method
// tried to detect the page boundary by watching PageInfo().Token after each Next() — which returns
// after the FIRST ITEM, not the first page, because the token becomes non-empty as soon as the
// iterator buffers a page. That would have metered one call and issued one per entry.
//
// pageSize 0 asks the server for its default. The maximum is undocumented — the widely-cited
// 1,000-entry cap appears nowhere in Google's reference (research §6) — so this requests no
// specific size and reads whatever came back rather than designing against a number nobody
// published.
func (a *liveAuditLogs) ListEntries(ctx context.Context, req *loggingpb.ListLogEntriesRequest) ([]*loggingpb.LogEntry, string, error) {
	project := ""
	if len(req.GetResourceNames()) > 0 {
		project = req.GetResourceNames()[0]
	}
	if err := a.lt.budget.Issue(ctx, gcpx.OpLoggingEntriesList, project, ""); err != nil {
		return nil, "", err
	}
	it := a.client.ListLogEntries(ctx, req)
	pager := iterator.NewPager(it, 0, req.GetPageToken())
	var page []*loggingpb.LogEntry
	next, err := pager.NextPage(&page)
	if err != nil {
		return nil, "", err
	}
	return page, next, nil
}

// ---- Cloud Monitoring ------------------------------------------------------------------------

type liveMonitoring struct {
	lt       *liveTransport
	policies *monitoring.AlertPolicyClient
	series   *monitoring.MetricClient
}

func (m *liveMonitoring) ListAlertPolicies(ctx context.Context, project string) ([]*monitoringpb.AlertPolicy, error) {
	if err := m.lt.budget.Issue(ctx, gcpx.OpMonitoringAlertPoliciesList, project, ""); err != nil {
		return nil, err
	}
	it := m.policies.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{
		Name: "projects/" + project,
	})
	var out []*monitoringpb.AlertPolicy
	for {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}

// GetMetricDescriptor reads one metric type's descriptor.
//
// The name is `projects/<project>/metricDescriptors/<metric.type>` — the metric type is the
// resource id and carries slashes of its own, which is why it is concatenated rather than
// path-joined.
func (m *liveMonitoring) GetMetricDescriptor(ctx context.Context, project, metricType string) (*metricpb.MetricDescriptor, error) {
	if err := m.lt.budget.Issue(ctx, gcpx.OpMonitoringMetricDescriptorsGet, project, ""); err != nil {
		return nil, err
	}
	return m.series.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{
		Name: "projects/" + project + "/metricDescriptors/" + metricType,
	})
}

func (m *liveMonitoring) ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) ([]*monitoringpb.TimeSeries, error) {
	if err := m.lt.budget.Issue(ctx, gcpx.OpMonitoringTimeSeriesList, req.GetName(), ""); err != nil {
		return nil, err
	}
	it := m.series.ListTimeSeries(ctx, req)
	var out []*monitoringpb.TimeSeries
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ts)
	}
}

// ---- Cloud Monitoring alerting incidents (T115) -------------------------------------------------

// AlertIncidentReader reads alerting incidents over a window (contract §6, research §5).
//
// It is a separate interface with a separate constructor rather than a method on MonitoringReader,
// and that separation IS the capability flag. `projects.alerts` is **Public Preview**, under Pre-GA
// terms, with Google warning that *"the labels in the response are subject to change while this
// feature is in preview"*; and it is **not in the first-party GAPIC** — the only Go binding is
// `google.golang.org/api/monitoring/v3`, whose package header says it is in maintenance mode.
//
// A Preview API through a maintenance-mode client is this feature's riskiest dependency, so nothing
// constructs the client unless an operator opted in: with the flag off there is no reader, no
// import path taken at run time, and the feature still works — ALERT nodes come from the GA policy
// read and `monitor_state` answers NO_DATA naming the absent source. A runtime `if` would have left
// the client in every process; a separate constructor leaves the decision where a reviewer can see
// it made.
type AlertIncidentReader interface {
	// ListAlerts returns the incidents whose open time falls in [start, end), newest first.
	ListAlerts(ctx context.Context, project string, start, end time.Time) ([]AlertIncident, error)
}

// liveAlertIncidents is the Preview reader.
type liveAlertIncidents struct {
	lt  *liveTransport
	svc *monitoringv3.Service
}

// NewLiveAlertIncidentReader constructs the incident reader. Call it only when the operator declared
// the capability (Options.IncidentsAPIEnabled); a nil reader is a stated scope, not a missing
// dependency.
func NewLiveAlertIncidentReader(ctx context.Context, creds *gcpx.Credential, budget *gcpx.Budget) (AlertIncidentReader, error) {
	if creds == nil {
		return nil, gcpx.ErrNoCredential
	}
	svc, err := monitoringv3.NewService(ctx, option.WithTokenSource(creds.TokenSource()))
	if err != nil {
		return nil, err
	}
	return &liveAlertIncidents{lt: &liveTransport{creds: creds, budget: budget}, svc: svc}, nil
}

// ListAlerts reads one window of incidents.
//
// The window is expressed as a filter on `open_time`, which is the field the API orders by and the
// only one that bounds the read: an incident that opened before the window and is still open is
// deliberately included by widening nothing — the caller asks about a window and gets the incidents
// that opened in it, and the state at the window's start is derived from their instants rather than
// from a second query the Preview surface does not offer.
//
// Calls are metered against `monitoring.policies`, the same class as `alertPolicies.list`. That is
// deliberate rather than lazy: both are low-volume reads on the same API and the same project
// quota, and a second class would let the budget report two numbers for one pool
// (contracts/budget.md §1).
func (a *liveAlertIncidents) ListAlerts(ctx context.Context, project string, start, end time.Time) ([]AlertIncident, error) {
	if err := a.lt.budget.Issue(ctx, gcpx.OpMonitoringAlertsList, project, ""); err != nil {
		return nil, err
	}
	call := a.svc.Projects.Alerts.List("projects/" + project).
		OrderBy("open_time desc").
		PageSize(alertIncidentPageSize).
		Context(ctx)
	if !start.IsZero() && !end.IsZero() {
		call = call.Filter(fmt.Sprintf(`open_time >= "%s" AND open_time < "%s"`,
			start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)))
	}
	resp, err := call.Do()
	if err != nil {
		return nil, err
	}
	out := make([]AlertIncident, 0, len(resp.Alerts))
	for _, alert := range resp.Alerts {
		incident, err := incidentFromAPI(alert)
		if err != nil {
			return nil, err
		}
		out = append(out, incident)
	}
	return out, nil
}

// alertIncidentPageSize is the API's documented maximum. Unlike `entries.list`, this one IS
// documented — default 50, maximum 1,000 — so asking for the maximum is asking for a published
// number rather than designing against folklore.
const alertIncidentPageSize = 1000

// incidentFromAPI converts the maintenance-mode client's struct into this package's own shape.
//
// It goes through the same JSON decoder the recorded path uses rather than field-by-field, so live
// and replay cannot diverge: there is one decoder, one set of refusals, and a recording is a
// recording of what this adapter would have produced from what Google returned.
func incidentFromAPI(alert *monitoringv3.Alert) (AlertIncident, error) {
	raw, err := alert.MarshalJSON()
	if err != nil {
		return AlertIncident{}, err
	}
	var decoded alertIncidentJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return AlertIncident{}, err
	}
	return decoded.decode()
}

// ---- GKE -------------------------------------------------------------------------------------

type liveGKE struct {
	lt  *liveTransport
	svc *container.Service
}

// ListClusters reads the clusters in a project, across every location.
//
// `-` as the location is the documented wildcard and is what makes this one call rather than one per
// region. It returns the cluster records and the node pools inside them; gke.go emits only the
// clusters (FR-030).
func (g *liveGKE) ListClusters(ctx context.Context, project string) ([]*container.Cluster, error) {
	if err := g.lt.budget.Issue(ctx, gcpx.OpContainerClustersList, project, ""); err != nil {
		return nil, err
	}
	resp, err := g.svc.Projects.Locations.Clusters.
		List("projects/" + project + "/locations/-").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Clusters, nil
}

// ---- Load balancers and DNS ------------------------------------------------------------------

// NewLiveExposureReader constructs the compute and DNS clients.
//
// It is a separate constructor from NewLiveTransport because these surfaces are opt-in: FR-057 makes
// them P3 and the first thing cut, so the default is not to read them at all, and a constructor that
// built their clients unconditionally would make "not reading them" a decision taken somewhere else.
func NewLiveExposureReader(ctx context.Context, creds *gcpx.Credential, budget *gcpx.Budget) (ExposureReader, error) {
	if creds == nil {
		return nil, gcpx.ErrNoCredential
	}
	opts := []option.ClientOption{option.WithTokenSource(creds.TokenSource())}
	computeClient, err := compute.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	dnsClient, err := dns.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &liveExposure{
		lt:      &liveTransport{creds: creds, budget: budget},
		compute: computeClient,
		dns:     dnsClient,
	}, nil
}

type liveExposure struct {
	lt      *liveTransport
	compute *compute.Service
	dns     *dns.Service
}

func (e *liveExposure) ListForwardingRules(ctx context.Context, project string) ([]*compute.ForwardingRule, error) {
	if err := e.lt.budget.Issue(ctx, gcpx.OpComputeForwardingRulesList, project, ""); err != nil {
		return nil, err
	}
	// Aggregated across every region and the global scope in one call, which is what makes this
	// surface cheap enough to read at all.
	resp, err := e.compute.ForwardingRules.AggregatedList(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []*compute.ForwardingRule
	for _, scoped := range resp.Items {
		out = append(out, scoped.ForwardingRules...)
	}
	return out, nil
}

func (e *liveExposure) ListURLMaps(ctx context.Context, project string) ([]*compute.UrlMap, error) {
	if err := e.lt.budget.Issue(ctx, gcpx.OpComputeURLMapsList, project, ""); err != nil {
		return nil, err
	}
	resp, err := e.compute.UrlMaps.AggregatedList(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []*compute.UrlMap
	for _, scoped := range resp.Items {
		out = append(out, scoped.UrlMaps...)
	}
	return out, nil
}

func (e *liveExposure) ListBackendServices(ctx context.Context, project string) ([]*compute.BackendService, error) {
	if err := e.lt.budget.Issue(ctx, gcpx.OpComputeBackendServicesList, project, ""); err != nil {
		return nil, err
	}
	resp, err := e.compute.BackendServices.AggregatedList(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []*compute.BackendService
	for _, scoped := range resp.Items {
		out = append(out, scoped.BackendServices...)
	}
	return out, nil
}

func (e *liveExposure) ListNetworkEndpointGroups(ctx context.Context, project, region string) ([]*compute.NetworkEndpointGroup, error) {
	if err := e.lt.budget.Issue(ctx, gcpx.OpComputeNetworkEndpointGroupList, project, region); err != nil {
		return nil, err
	}
	resp, err := e.compute.RegionNetworkEndpointGroups.List(project, region).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (e *liveExposure) ListDNSZones(ctx context.Context, project string) ([]*dns.ManagedZone, error) {
	if err := e.lt.budget.Issue(ctx, gcpx.OpDNSManagedZonesList, project, ""); err != nil {
		return nil, err
	}
	resp, err := e.dns.ManagedZones.List(project).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.ManagedZones, nil
}

func (e *liveExposure) ListDNSRecordSets(ctx context.Context, project, zone string) ([]*dns.ResourceRecordSet, error) {
	if err := e.lt.budget.Issue(ctx, gcpx.OpDNSResourceRecordSetList, project, ""); err != nil {
		return nil, err
	}
	resp, err := e.dns.ResourceRecordSets.List(project, zone).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Rrsets, nil
}
