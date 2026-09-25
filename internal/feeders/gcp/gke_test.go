// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	container "google.golang.org/api/container/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// GKE: the cluster, and nothing below it (T138, FR-030).

func clusterFixture(location string) *container.Cluster {
	return &container.Cluster{
		Name:                 "inference",
		Location:             location,
		SelfLink:             "https://container.googleapis.com/v1/projects/" + sqlProject + "/locations/" + location + "/clusters/inference",
		CurrentMasterVersion: "1.31.4-gke.1183000",
		CurrentNodeVersion:   "1.31.3-gke.1056000",
		Status:               "RUNNING",
		CreateTime:           "2026-02-10T11:20:00Z",
		ReleaseChannel:       &container.ReleaseChannel{Channel: "REGULAR"},
		ResourceLabels:       map[string]string{"team": "platform"},
		MaintenancePolicy: &container.MaintenancePolicy{
			Window: &container.MaintenanceWindow{
				DailyMaintenanceWindow: &container.DailyMaintenanceWindow{StartTime: "03:00", Duration: "PT4H"},
			},
		},
		NodePools: []*container.NodePool{{Name: "gpu-pool", InitialNodeCount: 3}},
	}
}

func observeCluster(t *testing.T, cluster *container.Cluster) gcpfeeder.ClusterObservation {
	t.Helper()
	obs, err := gcpfeeder.ObserveCluster(cluster, sqlProject, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveCluster: %v", err)
	}
	return obs
}

// The bare-name claim is the one that does the work: it is how the Kubernetes connector addresses the
// same cluster, so it is what lets the two sides resolve into one entity.
func TestTheClusterClaimsTheNameTheKubernetesConnectorUses(t *testing.T) {
	obs := observeCluster(t, clusterFixture("europe-west1"))
	var addressing, kubernetes bool
	for _, claim := range obs.Claims() {
		switch claim.Namespace {
		case gcpfeeder.NSGKECluster:
			if claim.Value == obs.Cluster.Value() {
				addressing = true
			}
		case feeder.NSK8sCluster:
			if claim.Value != "inference" {
				t.Errorf("the k8s.cluster claim is %q, want the bare name; a kubeconfig context "+
					"carries a name and not a project", claim.Value)
			}
			kubernetes = true
		}
	}
	if !addressing {
		t.Error("the ref this feeder addresses the cluster by is not among its claims (FR-115)")
	}
	if !kubernetes {
		t.Error("no claim names the cluster as the Kubernetes connector knows it, so the graph would " +
			"hold a GCP cluster and a Kubernetes cluster with nothing joining them (FR-030)")
	}
	// And that namespace is declared, or testkit fails the run (FR-116).
	if !gcpfeeder.DeclaresNamespace(feeder.NSK8sCluster) {
		t.Error("k8s.cluster is claimed but not declared")
	}
}

// The node says, as a property, that nothing inside the cluster was read. A prohibition nobody can
// see from the graph is a prohibition a reader has to take on trust.
func TestTheClusterNodeStatesThatItsContentsAreNotRead(t *testing.T) {
	obs := observeCluster(t, clusterFixture("europe-west1"))
	props := propsMap(t, obs.Props())
	if props[gcpfeeder.PropGKENodesNotRead] == "" {
		t.Fatal("the cluster node does not state that its contents are not read (FR-030, FR-012)")
	}
	// Nothing below the cluster leaks into the properties, however tempting the response makes it.
	for key, value := range props {
		if strings.Contains(value, "gpu-pool") {
			t.Errorf("property %s names a node pool: %q", key, value)
		}
	}
	node := obs.NodeFact(sqlObserved)
	if node.Type != graphv1.NodeType_INFRA_RESOURCE {
		t.Errorf("node type = %s, want INFRA_RESOURCE", node.Type)
	}
}

// A zonal cluster's location is a zone, and `cloud.region` must not carry one: it is the field every
// cross-source join reads.
func TestAZonalClustersLocationIsNotReportedAsARegion(t *testing.T) {
	zonal := propsMap(t, observeCluster(t, clusterFixture("europe-west1-b")).Props())
	if zonal[feeder.AttrCloudRegion] != "" {
		t.Errorf("cloud.region = %q for a zonal cluster; the location is a zone", zonal[feeder.AttrCloudRegion])
	}
	if zonal[gcpfeeder.PropGKELocation] != "europe-west1-b" {
		t.Errorf("location = %q, want the zone recorded", zonal[gcpfeeder.PropGKELocation])
	}
	if zonal[gcpfeeder.PropGKELocationIsRegional] != "false" {
		t.Errorf("regional = %q, want false", zonal[gcpfeeder.PropGKELocationIsRegional])
	}

	regional := propsMap(t, observeCluster(t, clusterFixture("europe-west1")).Props())
	if regional[feeder.AttrCloudRegion] != "europe-west1" {
		t.Errorf("cloud.region = %q for a regional cluster, want the region",
			regional[feeder.AttrCloudRegion])
	}
	if regional[gcpfeeder.PropGKELocationIsRegional] != "true" {
		t.Errorf("regional = %q, want true", regional[gcpfeeder.PropGKELocationIsRegional])
	}
}

// The zone/region test is on the shape, not on a list of regions that would go stale the week Google
// opened one.
func TestARegionIsToldFromAZoneByShape(t *testing.T) {
	for location, want := range map[string]bool{
		"europe-west1":        true,
		"us-central1":         true,
		"me-central2":         true,
		"europe-west1-b":      false,
		"us-central1-a":       false,
		"northamerica-south1": true,
	} {
		if got := gcpfeeder.IsRegion(location); got != want {
			t.Errorf("IsRegion(%q) = %v, want %v", location, got, want)
		}
	}
}

// The control-plane version and the node version are recorded separately, because the lag between
// them during an upgrade is what an incident during an upgrade is usually about.
func TestTheControlPlaneAndNodeVersionsAreRecordedSeparately(t *testing.T) {
	props := propsMap(t, observeCluster(t, clusterFixture("europe-west1")).Props())
	if props[gcpfeeder.PropGKEControlPlaneVersion] != "1.31.4-gke.1183000" {
		t.Errorf("control plane version = %q", props[gcpfeeder.PropGKEControlPlaneVersion])
	}
	if props[gcpfeeder.PropGKENodeVersion] != "1.31.3-gke.1056000" {
		t.Errorf("node version = %q, want it recorded where it differs", props[gcpfeeder.PropGKENodeVersion])
	}
	// And where they agree, the second is omitted rather than repeated.
	same := clusterFixture("europe-west1")
	same.CurrentNodeVersion = same.CurrentMasterVersion
	if got := propsMap(t, observeCluster(t, same).Props())[gcpfeeder.PropGKENodeVersion]; got != "" {
		t.Errorf("node version = %q where it matches the control plane; a property that repeats "+
			"another is a property a reader has to compare to learn nothing", got)
	}
}

// A cluster not enrolled in a release channel is a real state — the operator upgrades the control
// plane themselves — so the property is absent rather than a channel nobody chose.
func TestAnUnenrolledClusterReportsNoReleaseChannel(t *testing.T) {
	cluster := clusterFixture("europe-west1")
	cluster.ReleaseChannel = nil
	if got := propsMap(t, observeCluster(t, cluster).Props())[gcpfeeder.PropGKEReleaseChannel]; got != "" {
		t.Errorf("release channel = %q with none configured", got)
	}
}

// The maintenance policy is a property and never a change, exactly as Cloud SQL's is.
func TestTheClusterMaintenancePolicyIsAProperty(t *testing.T) {
	props := propsMap(t, observeCluster(t, clusterFixture("europe-west1")).Props())
	if !strings.Contains(props[gcpfeeder.PropGKEMaintenanceWindow], "03:00") {
		t.Errorf("maintenance window = %q, want the daily policy recorded",
			props[gcpfeeder.PropGKEMaintenanceWindow])
	}
}

// A cluster whose createTime GCP did not report starts with an UNKNOWN valid-from, which is what the
// Kubernetes connector already asserts for the same cluster. A bound from this side would make the
// merged entity read differently according to which event arrived first.
func TestAClusterWithNoCreateTimeStartsUnknown(t *testing.T) {
	cluster := clusterFixture("europe-west1")
	cluster.CreateTime = ""
	node := observeCluster(t, cluster).NodeFact(sqlObserved)
	if !node.ValidFromUnknown {
		t.Fatal("a cluster with no createTime does not assert an unknown start")
	}
	dated := observeCluster(t, clusterFixture("europe-west1")).NodeFact(sqlObserved)
	if dated.ValidFromUnknown {
		t.Error("a cluster with a createTime asserts an unknown start anyway")
	}
	if !dated.ValidAt.Equal(time.Date(2026, 2, 10, 11, 20, 0, 0, time.UTC)) {
		t.Errorf("valid_at = %s, want the createTime", dated.ValidAt)
	}
}
