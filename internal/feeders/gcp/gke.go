// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"strings"
	"time"

	container "google.golang.org/api/container/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// GKE: the cluster, and nothing below it (T138, FR-030, contract §7).
//
// # The rule is a prohibition, and it is the whole of this file
//
// The GKE read produces **one node per cluster** and stops. No node pool, no workload, no pod, no
// container. Everything inside the cluster belongs to the 001 Kubernetes feeder, which watches the
// API server and sees the objects as they actually are — and a second node for one workload is not a
// duplicate record, it is two entities with different histories that a diff will rank separately and
// a person will read as two things.
//
// The temptation is real and worth naming, because `clusters.get` returns `nodePools` in the same
// response: the data is *right there*, and emitting it looks like completeness. It is the opposite. A
// node pool observed from the GCP control plane and the Nodes observed by an informer are the same
// machines under two namespaces, and nothing in the graph could tell a reader that.
//
// # What the cluster node is for
//
// Two things, and neither needs anything below the cluster:
//
//  1. **the join.** The GCP side knows the cluster as `<project>/<location>/<cluster>` and the
//     Kubernetes side knows it as a bare name, because that is all a kubeconfig context carries. The
//     claim in `k8s.cluster` is what lets the published C1 rule merge them — two sources asserting one
//     identifier — so the graph holds one cluster with a GCP project on it and Kubernetes workloads
//     under it;
//  2. **the platform facts the Kubernetes feeder cannot see**: which release channel the cluster is on,
//     what version the control plane is running, whether it is Autopilot, and the maintenance policy.
//     A control-plane upgrade is a change to every workload's substrate, and the API server does not
//     report the channel that scheduled it.
//
// # The maintenance policy is a policy, exactly as Cloud SQL's is
//
// `MaintenancePolicy` is a recurring window. It is a property and never a change node, for the reason
// cloudsql.go states: a window that says "Sunday 03:00" is a calendar entry, not an event. The
// **occurrence** of a control-plane upgrade is a change, and it is the audit stream's to report.

// PayloadGKEClusters is a `container.clusters.list` response.
const PayloadGKEClusters = "gke_clusters"

// GKECluster is a cluster's coordinates.
//
// `Location` and not `Region`: a zonal cluster's location is a zone (`europe-west1-b`) and a regional
// cluster's is a region (`europe-west1`). Calling the field a region would make every zonal cluster's
// property a lie, and `cloud.region` is set only where the location is actually a region.
type GKECluster struct {
	Project  string
	Location string
	Name     string
}

// Validate refuses coordinates with an empty part.
func (c GKECluster) Validate() error {
	for name, part := range map[string]string{"project": c.Project, "location": c.Location, "cluster": c.Name} {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("%w: the %s of a GKE cluster", ErrEmptyIdentifierPart, name)
		}
	}
	return nil
}

// Value renders the cluster identifier: `<project>/<location>/<cluster>`.
func (c GKECluster) Value() string { return c.Project + "/" + c.Location + "/" + c.Name }

// Ref returns the cluster's addressing ref.
func (c GKECluster) Ref() *graphv1.Ref { return feeder.Ref(NSGKECluster, c.Value()) }

// ResourceName renders GCP's fully qualified name.
func (c GKECluster) ResourceName() string {
	return "projects/" + c.Project + "/locations/" + c.Location + "/clusters/" + c.Name
}

// The cluster properties.
const (
	// PropGKELocation is the cluster's location, which may be a zone or a region.
	PropGKELocation = "sre.gcp.gke_location"
	// PropGKELocationIsRegional says whether the location is a region rather than a zone, which is
	// what decides whether a zone failure takes the control plane with it.
	PropGKELocationIsRegional = "sre.gcp.gke_regional"
	// PropGKEControlPlaneVersion is the version the control plane is running now.
	PropGKEControlPlaneVersion = "sre.gcp.gke_control_plane_version"
	// PropGKENodeVersion is the version the nodes are running now, which lags the control plane
	// during an upgrade. Recorded separately because that lag is what an incident during an upgrade
	// is usually about.
	PropGKENodeVersion = "sre.gcp.gke_node_version"
	// PropGKEReleaseChannel is RAPID, REGULAR, STABLE or EXTENDED where the cluster is enrolled, and
	// is what says who decides when the control plane is upgraded.
	PropGKEReleaseChannel = "sre.gcp.gke_release_channel"
	// PropGKEAutopilot says whether the cluster is Autopilot, where node configuration is Google's.
	PropGKEAutopilot = "sre.gcp.gke_autopilot"
	// PropGKEStatus is the cluster status as GCP reports it.
	PropGKEStatus = "sre.gcp.gke_status"
	// PropGKEMaintenanceWindow is the recurring policy — a property, never a change.
	PropGKEMaintenanceWindow = "sre.gcp.gke_maintenance_window"
	// PropGKENodesNotRead states the prohibition as a fact about this node rather than only as a
	// comment in this file (FR-030, FR-012).
	PropGKENodesNotRead = "sre.gcp.gke_contents_not_read"
)

// GKEContentsNotRead is what the cluster node says about everything inside it.
const GKEContentsNotRead = "this connector reads the cluster and nothing inside it: no node pool, no " +
	"workload, no pod, no container. The cluster's contents belong to the Kubernetes connector, which " +
	"observes them from the API server, and a second node for one workload would be two entities with " +
	"two histories (FR-030)"

// ClusterObservation is one GKE cluster as a read saw it.
type ClusterObservation struct {
	Cluster GKECluster
	// Regional says whether Location is a region rather than a zone.
	Regional bool
	// ControlPlaneVersion and NodeVersion are the two versions in force.
	ControlPlaneVersion string
	NodeVersion         string
	// ReleaseChannel is the channel the cluster is enrolled in, empty where it is not enrolled.
	ReleaseChannel string
	// Autopilot says whether node configuration is Google's.
	Autopilot bool
	// Status is the cluster status as GCP reports it.
	Status string
	// MaintenanceWindow is the recurring policy, rendered readably.
	MaintenanceWindow string
	// CreateTime is the cluster's creation instant, zero where GCP reported none.
	CreateTime time.Time
	// Labels is the label policy's verdict on the cluster's resource labels.
	Labels Labels
}

// ObserveCluster reads one cluster (T138).
//
// The location is read from `location` and falls back to `zone`, which is the legacy spelling the API
// still populates for a zonal cluster. It is a fallback and not a guess: both fields hold the same
// fact, and one of them is deprecated.
func ObserveCluster(cluster *container.Cluster, project string, policy LabelPolicy) (ClusterObservation, error) {
	var obs ClusterObservation
	location := cluster.Location
	if location == "" {
		location = cluster.Zone
	}
	coords := GKECluster{Project: project, Location: location, Name: cluster.Name}
	if err := coords.Validate(); err != nil {
		return obs, err
	}
	obs = ClusterObservation{
		Cluster:             coords,
		Regional:            IsRegion(location),
		ControlPlaneVersion: cluster.CurrentMasterVersion,
		NodeVersion:         cluster.CurrentNodeVersion,
		ReleaseChannel:      releaseChannelOf(cluster.ReleaseChannel),
		Autopilot:           cluster.Autopilot != nil && cluster.Autopilot.Enabled,
		Status:              cluster.Status,
		MaintenanceWindow:   gkeMaintenanceWindow(cluster.MaintenancePolicy),
		Labels:              policy.Apply(project, cluster.ResourceLabels),
	}
	if cluster.CreateTime != "" {
		at, err := time.Parse(time.RFC3339Nano, cluster.CreateTime)
		if err != nil {
			return obs, fmt.Errorf("gcp: cluster %s createTime %q: %w", coords.Value(), cluster.CreateTime, err)
		}
		obs.CreateTime = at.UTC()
	}
	return obs, nil
}

// IsRegion reports whether a GKE location is a region rather than a zone.
//
// A zone is a region with a one-letter suffix — `europe-west1-b` against `europe-west1` — so the test
// is on the shape rather than on a list of regions nobody can keep current. A list would go stale the
// week Google opened a region, and it would go stale by classifying a real regional cluster as zonal.
func IsRegion(location string) bool {
	parts := strings.Split(location, "-")
	if len(parts) < 3 {
		return true
	}
	// The last part of a zone is a single letter.
	return len(parts[len(parts)-1]) != 1
}

// releaseChannelOf reads the channel, or the empty string where the cluster is not enrolled in one.
// Not enrolled is a real state — the operator upgrades the control plane themselves — so it is left
// absent rather than reported as a channel nobody chose.
func releaseChannelOf(channel *container.ReleaseChannel) string {
	if channel == nil {
		return ""
	}
	return channel.Channel
}

// gkeMaintenanceWindow renders the recurring policy as one readable string, or the empty string where
// there is none. It is a property and never a change (see the file comment).
func gkeMaintenanceWindow(policy *container.MaintenancePolicy) string {
	if policy == nil || policy.Window == nil {
		return ""
	}
	switch {
	case policy.Window.RecurringWindow != nil:
		recurring := policy.Window.RecurringWindow
		out := "recurrence=" + recurring.Recurrence
		if recurring.Window != nil {
			out += " start=" + recurring.Window.StartTime + " end=" + recurring.Window.EndTime
		}
		return out
	case policy.Window.DailyMaintenanceWindow != nil:
		return "daily start=" + policy.Window.DailyMaintenanceWindow.StartTime +
			" duration=" + policy.Window.DailyMaintenanceWindow.Duration
	default:
		return ""
	}
}

// NodeFact renders the cluster as the INFRA_RESOURCE node it is.
//
// Its valid start is the cluster's `createTime` where GCP states one. Where it does not, the start is
// **unknown** rather than a bound, and that differs from the Cloud SQL instance deliberately: the
// Kubernetes feeder already asserts this cluster with `ValidFromUnknown` set, so asserting a bound
// from this side would make the merged entity read differently according to which feeder's event
// arrived first.
func (o ClusterObservation) NodeFact(observedAt time.Time) feeder.NodeFact {
	fact := feeder.NodeFact{
		Ref:         o.Cluster.Ref(),
		Type:        graphv1.NodeType_INFRA_RESOURCE,
		DisplayName: o.Cluster.Name,
	}
	if o.CreateTime.IsZero() {
		fact.ValidAt = observedAt
		fact.ValidFromUnknown = true
	} else {
		fact.ValidAt = o.CreateTime
	}
	return fact
}

// Props renders the cluster's properties.
func (o ClusterObservation) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.Cluster.Project).
		Str(PropGKELocation, o.Cluster.Location).
		Bool(PropGKELocationIsRegional, o.Regional).
		Bool(PropGKEAutopilot, o.Autopilot).
		Str(PropGKENodesNotRead, GKEContentsNotRead).
		Str(PropEnvironment, o.Labels.Environment).
		Str(PropEnvironmentSource, o.Labels.EnvironmentSource).
		// The name the Kubernetes feeder uses, on the node as well as on the claim, so a reader
		// looking at the GCP node can see which cluster it is in kubeconfig terms.
		Str(feeder.AttrK8sClusterName, o.Cluster.Name)
	if o.Regional {
		// Only where the location IS a region. A zone in `cloud.region` would be wrong in the field
		// every cross-source join reads.
		props = props.Str(feeder.AttrCloudRegion, o.Cluster.Location)
	}
	if o.ControlPlaneVersion != "" {
		props = props.Str(PropGKEControlPlaneVersion, o.ControlPlaneVersion)
	}
	if o.NodeVersion != "" && o.NodeVersion != o.ControlPlaneVersion {
		props = props.Str(PropGKENodeVersion, o.NodeVersion)
	}
	if o.ReleaseChannel != "" {
		props = props.Str(PropGKEReleaseChannel, o.ReleaseChannel)
	}
	if o.Status != "" {
		props = props.Str(PropGKEStatus, o.Status)
	}
	if o.MaintenanceWindow != "" {
		props = props.Str(PropGKEMaintenanceWindow, o.MaintenanceWindow)
	}
	for key, value := range o.Labels.Props {
		props = props.Str("sre.gcp.label."+key, value)
	}
	return props
}

// Claims returns every identifier this feeder knows the cluster by, including the one it addresses it
// by (FR-115).
//
// The **bare name** claim in `k8s.cluster` is the one that does the work. It is how the Kubernetes
// feeder addresses the same cluster — a kubeconfig context carries a name and not a project — so the
// claim is what lets the published C1 rule merge the two sides into one entity. Without it the graph
// holds a GCP cluster with platform facts and a Kubernetes cluster with workloads, and nothing joins
// them.
func (o ClusterObservation) Claims() []Claim {
	locating := map[string]string{
		PropProject:               o.Cluster.Project,
		feeder.AttrK8sClusterName: o.Cluster.Name,
	}
	return []Claim{
		{Namespace: NSGKECluster, Value: o.Cluster.Value(),
			Why:   "the ref this feeder addresses the GKE cluster by",
			Attrs: locating},
		{Namespace: NSGKECluster, Value: o.Cluster.ResourceName(),
			Why:   "the fully qualified GKE resource name",
			Attrs: locating},
		{Namespace: feeder.NSK8sCluster, Value: o.Cluster.Name,
			Why: "the bare cluster name, which is how the Kubernetes connector addresses the same " +
				"cluster; this is the claim that lets the two sides resolve into one entity (FR-030)",
			Attrs: locating},
	}
}

// Pointers returns the cluster's console link. No metric pointer is minted: a GKE cluster's metrics
// are about the things inside it, and this connector does not model those.
func (o ClusterObservation) Pointers() []*graphv1.Pointer {
	return []*graphv1.Pointer{ConsoleLink(fmt.Sprintf(
		"https://console.cloud.google.com/kubernetes/clusters/details/%s/%s?project=%s",
		o.Cluster.Location, o.Cluster.Name, o.Cluster.Project),
		map[string]string{PropProject: o.Cluster.Project})}
}
