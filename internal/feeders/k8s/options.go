// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Configuration of the Kubernetes feeder (FR-043, research §12).
//
// Everything a mapping depends on that is not in the object itself lives here, and nothing
// here is read from the cluster at mapping time. That is not tidiness: a fact derived from
// anything but the object and this struct would depend on what the watch happened to deliver
// first, and the shuffle check (FR-048) exists to catch exactly that.

// Defaults for Options. They are the values fixtures/baseline-topology-01 was recorded with,
// so a reader comparing the fixture to the code does not have to guess which knob produced
// which string.
const (
	// DefaultEnvironment is the environment asserted when nothing in the cluster declares one.
	DefaultEnvironment = "prod"
	// DefaultEnvironmentLabel is the label or annotation a namespace or an object uses to
	// declare its environment. It is the OpenTelemetry attribute name, which is what makes a
	// Kubernetes declaration and a telemetry resource attribute comparable (rule C2).
	DefaultEnvironmentLabel = "deployment.environment.name"
	// DefaultOwnerLabel is the label an object uses to name the team that owns it.
	DefaultOwnerLabel = "team"
	// DefaultNodePool is the pool a workload is assumed to run on when it does not say.
	// Kubernetes only knows where a workload runs by looking at its pods, and pods are
	// deliberately not watched (research §12), so a workload with no nodeSelector and no node
	// affinity has no observable pool: this is a configured assertion, not an observation.
	DefaultNodePool = "general"
	// DefaultLogBackend is the backend a workload's LOG pointer names.
	DefaultLogBackend = "loki"
	// DefaultCheckpointInterval is how often a running feeder records its extent (FR-032).
	DefaultCheckpointInterval = 5 * time.Minute
	// DefaultReorderingWindow is how far out of order two of this feeder's own events may be
	// observed, as it declares them in Describe (research §12, FR-021, FR-048).
	//
	// It is not a property of the watch. The feeder reads its informers from one goroutine and
	// hands every event to one emitter in delivery order; it never reorders. What can reorder
	// is the transport underneath: `pkg/feeder/emit` buffers up to DefaultBatchSize events for
	// DefaultFlushInterval (one second) and the graph stamps observed time at acceptance, so
	// two events accepted in the same flush may be stamped either way round. Sixty seconds is
	// sixty flushes and comfortably more than a whole informer initial list — the 32-object
	// list of the kind corpus spans 211 ms — so it is a promise about the transport with room
	// to spare, and it gives the shuffle check a burst worth permuting.
	//
	// **Sixty seconds is provably safe now, and it was not before.** It was cut to 5 s in
	// Phase 9 for one reason: on reconnection the feeder retracts what disappeared while it
	// was blind and a watch event may re-assert the same object seconds later
	// (`fixtures/feeder-gap-01` has exactly that, 51.8 s apart), and a retraction delivered
	// *before* a later re-assertion of the same subject used to lose the interval the
	// re-assertion opened. That was the projector's documented ordering caveat, and it is
	// fixed: a coalesced segment remembers the first instant each source restated it and a
	// retraction splits rather than truncates (internal/projector/segments.go,
	// rememberRestatement and expandRestatements, pinned by
	// TestRetractionAndReassertionAreOrderIndependent and
	// TestRetractReassertRetractPermutations). A delete and a recreate of the same object have
	// no lower bound on their spacing, and that class is now survived at any spacing, so the
	// declaration is a statement about this feeder's transport rather than a hedge against the
	// graph.
	//
	// The one ordering still not survived — a retraction delivered *before* the assertion it
	// ends, which leaves nothing in the projection to cut — is not one this feeder can produce:
	// it never emits a retraction for an object it has not first asserted.
	DefaultReorderingWindow = 60 * time.Second
	// DefaultGapThresholdFactor multiplies CheckpointInterval to give the default
	// GapThreshold. A feeder restarted inside its own checkpoint interval has missed no
	// checkpoint and has nothing to admit to; one that comes back later was not watching, and
	// says so (FR-052, spec edge case "feeder gap"). Two intervals leaves room for a restart
	// that straddles one.
	DefaultGapThresholdFactor = 2
)

// Well-known Kubernetes keys the feeder reads. They are the keys deploy/kind/otel-demo writes
// and fixtures/baseline-topology-01 records.
const (
	// LabelAppName is the label a workload declares its OpenTelemetry service name in
	// (research §10, rule C2).
	LabelAppName = "app.kubernetes.io/name"
	// AnnotationServiceName is the annotation that declares the same thing explicitly.
	AnnotationServiceName = "resource.opentelemetry.io/service.name"
	// AnnotationRevision is the Deployment revision kubectl and the controller maintain.
	AnnotationRevision = "deployment.kubernetes.io/revision"
	// AnnotationChangeCause is what `kubectl --record` and most delivery tools write.
	AnnotationChangeCause = "kubernetes.io/change-cause"
	// LabelRegion is the OpenTelemetry-comparable region label every managed node carries.
	LabelRegion = "topology.kubernetes.io/region"
)

// DefaultNodePoolLabels are the node labels the feeder reads a pool name from, in order.
// deploy/kind/cluster.yaml writes both spellings for exactly this reason.
var DefaultNodePoolLabels = []string{"sre.node_pool", "node-pool"}

// Options configures a Feeder. The zero value is not usable; call Normalize, which fills in
// every default and validates what cannot have one.
type Options struct {
	// SourceID is the feeder's identity, e.g. "k8s:demo". Required.
	SourceID string
	// PointerCompat replays a corpus recorded before a pointer field existed. Zero — the
	// default, and what a live run wants — emits every field this SDK knows, join keys
	// included (ADR-0005 D6).
	PointerCompat feeder.PointerCompat
	// ClusterName names the cluster every object is asserted to belong to. Required: the
	// Kubernetes API does not know its own cluster's name, so an operator must say it.
	ClusterName string
	// Namespaces restricts the watch. Empty means every namespace.
	Namespaces []string
	// LabelSelector restricts the watch to objects carrying these labels. Empty means all.
	LabelSelector string
	// OwnerLabels are the label keys an owner is read from, in order. Empty means
	// DefaultOwnerLabel.
	OwnerLabels []string
	// Environment is the environment asserted when no object and no namespace declares one.
	Environment string
	// EnvironmentLabel is the label or annotation key an environment is declared in.
	EnvironmentLabel string
	// NodePoolLabels are the node labels and workload nodeSelector keys a pool is read from.
	NodePoolLabels []string
	// DefaultNodePool is the pool asserted for a workload that does not select one.
	DefaultNodePool string
	// LogBackend is the backend named by the LOG pointer on every workload.
	LogBackend string
	// CheckpointInterval is how often Run records its extent while the watch is quiet.
	CheckpointInterval time.Duration
	// ReorderingWindow is the window the feeder declares in Describe.
	ReorderingWindow time.Duration
	// GapThreshold is how long a hole between the previous process's last checkpoint and this
	// one's first list has to be before the feeder reports it as a gap. Zero means
	// DefaultGapThresholdFactor times CheckpointInterval.
	GapThreshold time.Duration
	// CommitLabels are the pod-template label or annotation keys a rollout's commit is read from, in
	// order of preference (004 T149). Empty — the default — reads none, and a Kubernetes rollout then
	// carries no `deploy.commit_sha` at all. See rolloutCommit for why it is opt-in and why only the
	// pod template is read.
	CommitLabels []string
	// AllowWriteCredentials disables the read-only credential refusal of FR-046. It exists
	// for one case — kind's default admin kubeconfig during development — and logs a warning
	// every time. deploy/kind/rbac/ is the documented path that never needs it.
	AllowWriteCredentials bool
}

// Normalize fills in defaults and rejects an Options that could not produce a well-formed
// event.
func (o Options) Normalize() (Options, error) {
	out := o
	out.SourceID = strings.TrimSpace(out.SourceID)
	out.ClusterName = strings.TrimSpace(out.ClusterName)
	if out.SourceID == "" {
		return Options{}, errors.New("k8s: SourceID is required; it is the identity every event and every token is scoped to")
	}
	if out.ClusterName == "" {
		return Options{}, fmt.Errorf("k8s: ClusterName is required for source %s; the Kubernetes API does not know what its cluster is called", out.SourceID)
	}
	if out.Environment == "" {
		out.Environment = DefaultEnvironment
	}
	if out.EnvironmentLabel == "" {
		out.EnvironmentLabel = DefaultEnvironmentLabel
	}
	if len(out.OwnerLabels) == 0 {
		out.OwnerLabels = []string{DefaultOwnerLabel}
	}
	if len(out.NodePoolLabels) == 0 {
		out.NodePoolLabels = DefaultNodePoolLabels
	}
	if out.DefaultNodePool == "" {
		out.DefaultNodePool = DefaultNodePool
	}
	if out.LogBackend == "" {
		out.LogBackend = DefaultLogBackend
	}
	if out.CheckpointInterval <= 0 {
		out.CheckpointInterval = DefaultCheckpointInterval
	}
	if out.ReorderingWindow <= 0 {
		out.ReorderingWindow = DefaultReorderingWindow
	}
	if out.GapThreshold <= 0 {
		out.GapThreshold = DefaultGapThresholdFactor * out.CheckpointInterval
	}
	out.OwnerLabels = trimmed(out.OwnerLabels)
	out.NodePoolLabels = trimmed(out.NodePoolLabels)
	out.Namespaces = trimmed(out.Namespaces)
	return out, nil
}

// trimmed drops empty entries and surrounding whitespace, so that `--owner-labels team,`
// means what it looks like.
func trimmed(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// environmentOf resolves the environment of one object.
//
// The object's own declaration wins, then its namespace's, then the configured default. The
// namespace half is read from a map the feeder built from Namespace payloads, and the live
// path syncs the namespace informer before any other so that the map is populated first; a
// replay keeps namespace payloads ahead of the window for the same reason. Where neither
// declares one the answer is configuration, never a guess (FR-011).
func (o Options) environmentOf(labels, annotations map[string]string, namespaceEnv string) string {
	if env := firstValue(annotations, o.EnvironmentLabel); env != "" {
		return env
	}
	if env := firstValue(labels, o.EnvironmentLabel); env != "" {
		return env
	}
	if namespaceEnv != "" {
		return namespaceEnv
	}
	return o.Environment
}

// ownerOf returns the first owner label an object carries, and the key it was read from.
func (o Options) ownerOf(labels map[string]string) (owner, key string) {
	for _, candidate := range o.OwnerLabels {
		if value := strings.TrimSpace(labels[candidate]); value != "" {
			return value, candidate
		}
	}
	return "", ""
}

// nodePoolOf returns the pool a selector names, or "" when it names none.
func (o Options) nodePoolOf(selector map[string]string) string {
	for _, key := range o.NodePoolLabels {
		if value := strings.TrimSpace(selector[key]); value != "" {
			return value
		}
	}
	return ""
}

// firstValue reads one key out of a possibly nil map.
func firstValue(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[key])
}
