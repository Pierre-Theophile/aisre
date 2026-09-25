// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Topology mapping (T057, FR-043, FR-008, research §12).
//
// Every function here is a pure function of one Kubernetes object, the feeder's Options and a
// valid time. Nothing reads arrival order, nothing reads a clock, and nothing looks at another
// object — with one deliberate exception, `exposed-via`, which is a relationship between two
// objects and is therefore computed from the pair (see exposedViaEvents). That exception is
// still order-independent: the edge is emitted by whichever of the two arrives second, with an
// identity derived from both, so the *set* of events is the same however the watch delivers
// them (FR-048).
//
// Two conventions are worth stating once.
//
// Ambient entities — the cluster, a node pool, an owning team — are asserted by many payloads
// and their identity is a pure function of their name, so every assertion after the first is a
// DUPLICATE_NOOP. Their valid start is genuinely unknown: the team existed before the feeder
// looked, and stamping the arrival time of whichever payload happened to be first would make
// the event depend on delivery order. They therefore carry ValidFromUnknown rather than a
// guessed instant (FR-011, SDK guide §3 "Valid time").
//
// An edge is emitted even when the object at its other end has not been seen. The projector
// creates a placeholder for an unresolvable ref, so a `depends-on` edge to a ConfigMap the
// watch has not reached yet is a fact now and a joined-up graph a moment later. Waiting would
// make the emission depend on arrival order, which is the one thing a feeder may not do.

// validTime is the valid-time stamp one fact carries: an instant the source asserts, or an
// explicit unknown start.
//
// It exists so that "when was this true from?" is answered once, by the code that knows which
// payload is the evidence for a fact, and carried through every mapper unchanged. Before it,
// every mapper took a bare `time.Time` and the only way to say "I do not know" was to invent
// an instant — which is what FR-011 forbids and what made the initial list's valid times a
// function of which payload the informers happened to deliver first (live run 2026-09-16,
// finding 6).
type validTime struct {
	at      time.Time
	unknown bool
}

// assertedAt is a valid time the source knows: the fact became true then.
func assertedAt(t time.Time) validTime { return validTime{at: t} }

// unknownSince is FR-011's unknown lower bound: true at `t`, true before it, and the source
// cannot say since when. `t` is a bound the feeder can prove — the instant its list completed —
// and the flag beside it is what stops a reader taking that bound for a beginning.
//
// Both halves matter. The flag is FR-011: an informer's list says what exists, never since
// when, and guessing is a defect. The instant is FR-021: `valid_from_unknown` alone leaves the
// projector to use each event's own observed time as the physical bound, and those differ by
// milliseconds across one batch, so the version boundary of an entity two sources describe
// lands wherever the arrival order put it. One instant for the whole list — the sync marker's,
// which is one payload's own timestamp and therefore not a function of any permutation — makes
// the segmentation identical however the list was delivered.
func unknownSince(t time.Time) validTime { return validTime{at: t, unknown: true} }

// Mapper turns Kubernetes objects into events. It holds no state; the cross-object indexes
// live in the Feeder, which passes what a mapping needs.
type Mapper struct {
	opts Options
	desc feeder.Description
}

// NewMapper returns a mapper for one feeder. opts must already be normalized.
func NewMapper(desc feeder.Description, opts Options) *Mapper {
	return &Mapper{opts: opts, desc: desc}
}

// Options returns the configuration the mapper was built with.
func (m *Mapper) Options() Options { return m.opts }

// clusterRef is the cluster every object in this feeder belongs to.
func (m *Mapper) clusterRef() *graphv1.Ref {
	return feeder.Ref(feeder.NSK8sCluster, m.opts.ClusterName)
}

// nodePoolRef identifies a pool as `<cluster>/<pool>`.
func (m *Mapper) nodePoolRef(pool string) *graphv1.Ref {
	return feeder.Ref(feeder.NSK8sNodePool, m.opts.ClusterName+"/"+pool)
}

// id mints an event id under this feeder's source.
func (m *Mapper) id(parts ...string) string {
	return feeder.NewID(m.desc.SourceID, parts...)
}

// ClusterEvents asserts the cluster, its node pools and the `runs-on` edges that place the
// pools in it, from every Node the feeder has seen.
//
// It takes the whole node set rather than one Node because two of the facts it states — how
// many machines a pool has, and which region the cluster is in — are properties of the set.
// Deriving them from "the Node that happened to arrive first" would make them depend on
// delivery order, which is the one thing a feeder may not do (FR-048).
//
// A machine is not itself a node in the graph: fixtures/baseline-topology-01 records pools,
// because a pod's machine changes on every rollout while its pool does not, and a graph of
// machines would churn without answering anything an investigation asks.
func (m *Mapper) ClusterEvents(nodes []nodeView, at validTime) ([]*graphv1.EventEnvelope, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	sorted := append([]nodeView(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

	region := ""
	for _, n := range sorted {
		if n.region != "" {
			region = n.region
			break
		}
	}
	clusterProps := feeder.NewProps().
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrDeploymentEnvironment, m.opts.Environment)
	if region != "" {
		// A cluster spanning regions has no single region, and its nodes then carry none;
		// an absent label is left absent rather than defaulted (FR-011).
		clusterProps = clusterProps.Str(feeder.AttrCloudRegion, region)
	}
	built, err := clusterProps.Build()
	if err != nil {
		return nil, err
	}

	events := []*graphv1.EventEnvelope{
		feeder.UpsertNode(m.desc, m.id("infra", "cluster/"+m.opts.ClusterName), feeder.NodeFact{
			Ref:         m.clusterRef(),
			Type:        graphv1.NodeType_INFRA_RESOURCE,
			DisplayName: m.opts.ClusterName,
			Props:       built,
			Pointers: []*graphv1.Pointer{
				feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
					"v1/clusters/"+m.opts.ClusterName,
					map[string]string{feeder.AttrK8sClusterName: m.opts.ClusterName}),
			},
			// Ambient: the cluster existed before this feeder looked at it, so the start is
			// unknown whatever the payload that mentioned it said.
			ValidAt:          at.at,
			ValidFromUnknown: true,
		}),
	}

	counts := map[string]int64{}
	labelKeys := map[string]string{}
	var pools []string
	for _, n := range sorted {
		if n.pool == "" {
			continue
		}
		if _, seen := counts[n.pool]; !seen {
			pools = append(pools, n.pool)
			labelKeys[n.pool] = m.poolLabelKey(n.labels)
		}
		counts[n.pool]++
	}
	sort.Strings(pools)
	for _, pool := range pools {
		poolEvents, err := m.nodePoolEvents(pool, labelKeys[pool], counts[pool], at)
		if err != nil {
			return nil, err
		}
		events = append(events, poolEvents...)
	}
	return events, nil
}

// poolLabelKey reports which of the configured labels the pool was read from, so that the node
// pool's SOURCE_LINK selector names the key an operator can paste into kubectl.
func (m *Mapper) poolLabelKey(labels map[string]string) string {
	for _, key := range m.opts.NodePoolLabels {
		if strings.TrimSpace(labels[key]) != "" {
			return key
		}
	}
	if len(m.opts.NodePoolLabels) > 0 {
		return m.opts.NodePoolLabels[0]
	}
	return DefaultNodePoolLabels[0]
}

// nodePoolEvents asserts a pool, how many machines it has, and its place in the cluster.
func (m *Mapper) nodePoolEvents(pool, labelKey string, nodes int64, at validTime) ([]*graphv1.EventEnvelope, error) {
	props := feeder.NewProps().
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrDeploymentEnvironment, m.opts.Environment).
		Str(feeder.PropK8sNodePool, pool)
	if nodes > 0 {
		props = props.Int(feeder.PropK8sNodeCount, nodes)
	}
	built, err := props.Build()
	if err != nil {
		return nil, err
	}
	selector := "v1/nodes?labelSelector=" + url.QueryEscape(labelKey+"="+pool)
	return []*graphv1.EventEnvelope{
		feeder.UpsertNode(m.desc, m.id("infra", "nodepool/"+m.opts.ClusterName+"-"+pool), feeder.NodeFact{
			Ref:         m.nodePoolRef(pool),
			Type:        graphv1.NodeType_INFRA_RESOURCE,
			DisplayName: pool,
			Props:       built,
			Pointers: []*graphv1.Pointer{
				feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource, selector, map[string]string{
					feeder.AttrK8sClusterName: m.opts.ClusterName,
					feeder.PropK8sNodePool:    pool,
				}),
			},
			ValidAt:          at.at,
			ValidFromUnknown: true,
		}),
		feeder.UpsertEdge(m.desc, m.id("edge", "runs-on", m.opts.ClusterName+"/"+pool+"->"+m.opts.ClusterName), feeder.EdgeFact{
			Src:              m.nodePoolRef(pool),
			Dst:              m.clusterRef(),
			Type:             graphv1.EdgeType_RUNS_ON,
			ValidAt:          at.at,
			ValidFromUnknown: true,
		}),
	}, nil
}

// WorkloadEvents is everything one workload object asserts, in the order the incremental path
// emits it: the workload itself, where it runs, who owns it, what configuration it consumes,
// and the names it is known by.
//
// The initial list does not go through here. It emits the same facts grouped by kind across
// every object (Feeder.topologyEvents), which is what makes a listed topology byte-identical
// however the watch delivered it.
func (m *Mapper) WorkloadEvents(w workloadView, env string, at validTime) ([]*graphv1.EventEnvelope, error) {
	node, err := m.WorkloadNode(w, env, at)
	if err != nil {
		return nil, err
	}
	events := []*graphv1.EventEnvelope{node}
	if edge := m.RunsOnEdge(w, at); edge != nil {
		events = append(events, edge)
	}
	owner, err := m.OwnerEvents(w, env, at)
	if err != nil {
		return nil, err
	}
	events = append(events, owner...)

	depends, err := m.DependsOnEdges(w, at)
	if err != nil {
		return nil, err
	}
	events = append(events, depends...)

	claims, err := m.ClaimEvents(w, env)
	if err != nil {
		return nil, err
	}
	return append(events, claims...), nil
}

// WorkloadNode asserts the workload itself: what it is, how many replicas it asks for, which
// revision it is on, and where its logs live.
func (m *Mapper) WorkloadNode(w workloadView, env string, at validTime) (*graphv1.EventEnvelope, error) {
	props := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrK8sNamespaceName, w.namespace).
		Str(w.kind.nameAttr, w.name).
		Str(feeder.PropK8sResourceVersion, w.resourceVersion)
	if w.replicas != nil {
		props = props.Int(feeder.PropK8sReplicas, *w.replicas)
	}
	if w.revision != "" {
		props = props.Str(feeder.PropK8sRevision, w.revision)
	}
	built, err := props.Build()
	if err != nil {
		return nil, err
	}

	linkAttrs := map[string]string{
		feeder.AttrK8sClusterName:   m.opts.ClusterName,
		feeder.AttrK8sNamespaceName: w.namespace,
		w.kind.nameAttr:             w.name,
	}
	logAttrs := map[string]string{
		feeder.AttrDeploymentEnvironment: env,
		feeder.AttrK8sNamespaceName:      w.namespace,
		w.kind.nameAttr:                  w.name,
	}
	return feeder.UpsertNode(m.desc, m.id(w.kind.idPart, w.key()+"@rv"+w.resourceVersion), feeder.NodeFact{
		Meta:        feeder.Meta{Seq: seqOf(w.resourceVersion)},
		Ref:         feeder.Ref(w.kind.refNamespace, w.key()),
		Type:        graphv1.NodeType_WORKLOAD,
		DisplayName: w.name,
		Props:       built,
		Pointers: []*graphv1.Pointer{
			feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource, w.resourcePath(), linkAttrs),
			// Where the workload's logs are, never a log line (constitution IV). The selector
			// names the two attributes that identify the workload; the pointer's attributes
			// also carry the environment, so a reader knows which one.
			//
			// The join keys say which attribute of THIS vocabulary plays which role, so that a
			// log answer can be related to a metric answer and to the graph (ADR-0005 D6).
			// Kubernetes logs carry the workload and the pod; the deployed version is not a
			// log attribute here, so the `version` role is simply absent rather than guessed.
			feeder.WithJoinKeys(
				feeder.LogPointer(m.opts.LogBackend,
					feeder.OTelSelector(logAttrs, feeder.AttrK8sNamespaceName, w.kind.nameAttr), logAttrs),
				m.opts.PointerCompat.JoinKeys(map[string]string{
					feeder.JoinRoleWorkload: w.kind.nameAttr,
					feeder.JoinRolePod:      feeder.AttrK8sPodName,
				})),
		},
		ValidAt:          at.at,
		ValidFromUnknown: at.unknown,
	}), nil
}

// NodePoolOf is where a workload runs.
//
// Kubernetes only knows that by looking at the pods, and pods are deliberately not watched:
// they churn on every rollout and would multiply the event stream by an order of magnitude for
// a fact that is stable at the pool level (research §12). So the pool is read from what the
// workload *declares* — its nodeSelector, then a required node affinity — and where it
// declares nothing, from Options.DefaultNodePool, which is configuration rather than a guess.
func (m *Mapper) NodePoolOf(w workloadView) string {
	if pool := m.opts.nodePoolOf(w.nodeSelector); pool != "" {
		return pool
	}
	for _, expr := range w.affinityPools {
		key, value, found := strings.Cut(expr, "=")
		if !found {
			continue
		}
		for _, candidate := range m.opts.NodePoolLabels {
			if key == candidate && value != "" {
				return value
			}
		}
	}
	return m.opts.DefaultNodePool
}

// RunsOnEdge places a workload on its node pool, or returns nil when no pool can be named.
func (m *Mapper) RunsOnEdge(w workloadView, at validTime) *graphv1.EventEnvelope {
	pool := m.NodePoolOf(w)
	if pool == "" {
		return nil
	}
	return feeder.UpsertEdge(m.desc,
		m.id("edge", "runs-on", w.key()+"->"+m.opts.ClusterName+"/"+pool+"@rv"+w.resourceVersion),
		feeder.EdgeFact{
			Meta:             feeder.Meta{Seq: seqOf(w.resourceVersion)},
			Src:              feeder.Ref(w.kind.refNamespace, w.key()),
			Dst:              m.nodePoolRef(pool),
			Type:             graphv1.EdgeType_RUNS_ON,
			ValidAt:          at.at,
			ValidFromUnknown: at.unknown,
		})
}

// OwnerOf is the team that owns a workload and the label key it was declared in.
func (m *Mapper) OwnerOf(w workloadView) (owner, label string) {
	if owner, label = m.opts.ownerOf(w.labels); owner != "" {
		return owner, label
	}
	return m.opts.ownerOf(w.podLabels)
}

// OwnerNode asserts an owning team. Its identity is a pure function of the team's name, so the
// second workload that names it re-asserts the same event and the graph answers DUPLICATE_NOOP.
func (m *Mapper) OwnerNode(owner, label, env string, at validTime) (*graphv1.EventEnvelope, error) {
	props, err := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.PropOwnerKind, "team").
		Str(feeder.PropOwnerSourceLabel, label).
		Build()
	if err != nil {
		return nil, err
	}
	return feeder.UpsertNode(m.desc, m.id("owner", owner), feeder.NodeFact{
		Ref:         feeder.Ref(feeder.NSOwnerTeam, owner),
		Type:        graphv1.NodeType_OWNER,
		DisplayName: owner,
		Props:       props,
		// Ambient: the team owned things before this feeder read a label saying so.
		ValidAt:          at.at,
		ValidFromUnknown: true,
	}), nil
}

// OwnedByEdge links a workload to the team that owns it.
func (m *Mapper) OwnedByEdge(w workloadView, owner string, at validTime) *graphv1.EventEnvelope {
	return feeder.UpsertEdge(m.desc, m.id("edge", "owned-by", w.key()+"->"+owner+"@rv"+w.resourceVersion), feeder.EdgeFact{
		Meta:             feeder.Meta{Seq: seqOf(w.resourceVersion)},
		Src:              feeder.Ref(w.kind.refNamespace, w.key()),
		Dst:              feeder.Ref(feeder.NSOwnerTeam, owner),
		Type:             graphv1.EdgeType_OWNED_BY,
		ValidAt:          at.at,
		ValidFromUnknown: at.unknown,
	})
}

// OwnerEvents is the owner node and the edge to it, for the incremental path.
func (m *Mapper) OwnerEvents(w workloadView, env string, at validTime) ([]*graphv1.EventEnvelope, error) {
	owner, label := m.OwnerOf(w)
	if owner == "" {
		return nil, nil
	}
	node, err := m.OwnerNode(owner, label, env, at)
	if err != nil {
		return nil, err
	}
	return []*graphv1.EventEnvelope{node, m.OwnedByEdge(w, owner, at)}, nil
}

// DependsOnEdges links a workload to every ConfigMap and Secret it consumes, recording how it
// consumes it. The CONFIG node itself is asserted by the ConfigMap or Secret payload, not
// here: this feeder never invents a version for an object it has not read.
func (m *Mapper) DependsOnEdges(w workloadView, at validTime) ([]*graphv1.EventEnvelope, error) {
	events := make([]*graphv1.EventEnvelope, 0, len(w.configRefs))
	for _, ref := range w.configRefs {
		props, err := feeder.NewProps().Str(feeder.PropK8sReference, ref.reference).Build()
		if err != nil {
			return nil, err
		}
		target := key(w.namespace, ref.name)
		events = append(events, feeder.UpsertEdge(m.desc,
			m.id("edge", "depends-on", w.key()+"->"+ref.kind.idPart+"/"+target+"@rv"+w.resourceVersion),
			feeder.EdgeFact{
				Meta:             feeder.Meta{Seq: seqOf(w.resourceVersion)},
				Src:              feeder.Ref(w.kind.refNamespace, w.key()),
				Dst:              feeder.Ref(ref.kind.refNamespace, target),
				Type:             graphv1.EdgeType_DEPENDS_ON,
				Props:            props,
				ValidAt:          at.at,
				ValidFromUnknown: at.unknown,
			}))
	}
	return events, nil
}

// ClaimEvents is every external name this feeder knows the workload by (FR-036, research §10).
//
// Three claims, and never a merge: the addressing identity itself, the `app.kubernetes.io/name`
// label, and the OpenTelemetry service-name annotation. The attributes are what makes a claim
// decidable by the certain rules — `deployment.environment.name` and `k8s.namespace.name` for
// both C2 and C3, plus `sre.k8s.claim_key` and `sre.k8s.claim_kind` so that rule C2 can tell an
// operator's declaration from a coincidence (internal/resolution/certain.go).
func (m *Mapper) ClaimEvents(w workloadView, env string) ([]*graphv1.EventEnvelope, error) {
	meta := feeder.Meta{Seq: seqOf(w.resourceVersion)}
	selfAttrs, err := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrK8sNamespaceName, w.namespace).
		Build()
	if err != nil {
		return nil, err
	}
	subject := feeder.Ref(w.kind.refNamespace, w.key())
	events := []*graphv1.EventEnvelope{
		feeder.IdentityClaim(m.desc, m.claimID(w, w.kind.refNamespace), feeder.IdentityFact{
			Meta:       meta,
			Subject:    subject,
			Claim:      subject,
			Attributes: selfAttrs,
		}),
	}

	if name := labelOf(w, LabelAppName); name != "" {
		attrs, err := m.claimAttrs(env, w.namespace, LabelAppName, "label")
		if err != nil {
			return nil, err
		}
		events = append(events, feeder.IdentityClaim(m.desc, m.claimID(w, feeder.NSAppName), feeder.IdentityFact{
			Meta:       meta,
			Subject:    subject,
			Claim:      feeder.Ref(feeder.NSAppName, name),
			Attributes: attrs,
		}))
	}
	if name := annotationOf(w, AnnotationServiceName); name != "" {
		attrs, err := m.claimAttrs(env, w.namespace, AnnotationServiceName, "annotation")
		if err != nil {
			return nil, err
		}
		events = append(events, feeder.IdentityClaim(m.desc, m.claimID(w, feeder.NSOTelService), feeder.IdentityFact{
			Meta:       meta,
			Subject:    subject,
			Claim:      feeder.Ref(feeder.NSOTelService, name),
			Attributes: attrs,
		}))
	}
	return events, nil
}

// claimAttrs builds the supporting facts a certain rule reads off a declared claim.
func (m *Mapper) claimAttrs(env, namespace, claimKey, claimKind string) (*structpb.Struct, error) {
	return feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.AttrK8sNamespaceName, namespace).
		Str(feeder.PropK8sClaimKey, claimKey).
		Str(feeder.PropK8sClaimKind, claimKind).
		Build()
}

// claimID names a claim by its subject and the namespace it is asserted in. The namespace's
// slashes become dashes, because an id is read left to right and `app.kubernetes.io/name`
// inside one reads as a path (fixtures/baseline-topology-01).
func (m *Mapper) claimID(w workloadView, namespace string) string {
	return m.id("claim", w.key()+"@"+strings.ReplaceAll(namespace, "/", "-"))
}

// ServiceEvents asserts a Service as an infrastructure resource.
func (m *Mapper) ServiceEvents(s serviceView, env string, at validTime) ([]*graphv1.EventEnvelope, error) {
	props, err := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrK8sNamespaceName, s.namespace).
		Str(feeder.AttrK8sServiceName, s.name).
		Build()
	if err != nil {
		return nil, err
	}
	attrs := map[string]string{
		feeder.AttrK8sNamespaceName: s.namespace,
		feeder.AttrK8sServiceName:   s.name,
	}
	return []*graphv1.EventEnvelope{
		feeder.UpsertNode(m.desc, m.id(kindService.idPart, s.key()+"@rv"+s.resourceVersion), feeder.NodeFact{
			Meta:        feeder.Meta{Seq: seqOf(s.resourceVersion)},
			Ref:         feeder.Ref(feeder.NSK8sService, s.key()),
			Type:        graphv1.NodeType_INFRA_RESOURCE,
			DisplayName: "svc/" + s.name,
			Props:       props,
			Pointers: []*graphv1.Pointer{
				feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
					resourcePath(kindService, s.namespace, s.name), attrs),
			},
			ValidAt:          at.at,
			ValidFromUnknown: at.unknown,
		}),
	}, nil
}

// IngressEvents asserts an Ingress, carrying the host it answers on as `server.address` so
// that a dependency discovered from spans can meet it (namespace `server.address`).
func (m *Mapper) IngressEvents(i ingressView, env string, at validTime) ([]*graphv1.EventEnvelope, error) {
	props := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrK8sNamespaceName, i.namespace).
		Str(feeder.AttrK8sIngressName, i.name)
	if i.host != "" {
		props = props.Str(feeder.AttrServerAddress, i.host)
	}
	built, err := props.Build()
	if err != nil {
		return nil, err
	}
	attrs := map[string]string{
		feeder.AttrK8sIngressName:   i.name,
		feeder.AttrK8sNamespaceName: i.namespace,
	}
	return []*graphv1.EventEnvelope{
		feeder.UpsertNode(m.desc, m.id(kindIngress.idPart, i.key()+"@rv"+i.resourceVersion), feeder.NodeFact{
			Meta:        feeder.Meta{Seq: seqOf(i.resourceVersion)},
			Ref:         feeder.Ref(feeder.NSK8sIngress, i.key()),
			Type:        graphv1.NodeType_INFRA_RESOURCE,
			DisplayName: "ing/" + i.name,
			Props:       built,
			Pointers: []*graphv1.Pointer{
				feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
					resourcePath(kindIngress, i.namespace, i.name), attrs),
			},
			ValidAt:          at.at,
			ValidFromUnknown: at.unknown,
		}),
	}, nil
}

// ConfigEvents asserts a ConfigMap or a Secret as a CONFIG node.
//
// A ConfigMap carries a digest of its content, so that a change is visible and the content is
// not. A Secret carries neither content nor digest — a digest of a short, guessable value is a
// value — only the version that changed (spec edge case "secrets", FR-008). Emitting a
// secret's material is refused by the SDK before it leaves the process
// (feeder.ReasonSecretValue), and this function is the reason that check has never fired.
func (m *Mapper) ConfigEvents(c configView, env string, at validTime) ([]*graphv1.EventEnvelope, error) {
	props := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, env).
		Str(feeder.AttrK8sClusterName, m.opts.ClusterName).
		Str(feeder.AttrK8sNamespaceName, c.namespace).
		Str(c.kind.nameAttr, c.name).
		Str(feeder.PropK8sResourceVersion, c.resourceVersion)
	switch c.kind.payload {
	case KindSecrets:
		props = props.Str(feeder.PropConfigKind, "secret").Str(feeder.PropConfigVersion, c.version)
	default:
		props = props.Str(feeder.PropConfigKind, "configmap").Str(feeder.PropConfigValueHash, c.valueHash)
	}
	built, err := props.Build()
	if err != nil {
		return nil, err
	}
	attrs := map[string]string{
		feeder.AttrK8sNamespaceName: c.namespace,
		c.kind.nameAttr:             c.name,
	}
	return []*graphv1.EventEnvelope{
		feeder.UpsertNode(m.desc, m.id(c.kind.idPart, c.key()+"@rv"+c.resourceVersion), feeder.NodeFact{
			Meta:        feeder.Meta{Seq: seqOf(c.resourceVersion)},
			Ref:         feeder.Ref(c.kind.refNamespace, c.key()),
			Type:        graphv1.NodeType_CONFIG,
			DisplayName: c.name,
			Props:       built,
			Pointers: []*graphv1.Pointer{
				feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
					resourcePath(c.kind, c.namespace, c.name), attrs),
			},
			ValidAt:          at.at,
			ValidFromUnknown: at.unknown,
		}),
	}, nil
}

// ExposedViaService links a workload to a Service that selects it. The event id carries the
// Service's resourceVersion, because the Service is the object that decides the exposure: a
// workload re-read unchanged must not mint a second edge.
func (m *Mapper) ExposedViaService(w workloadView, s serviceView, at validTime) *graphv1.EventEnvelope {
	return feeder.UpsertEdge(m.desc,
		m.id("edge", "exposed-via", w.key()+"->"+kindService.idPart+"/"+s.key()+"@rv"+s.resourceVersion),
		feeder.EdgeFact{
			Meta:             feeder.Meta{Seq: seqOf(s.resourceVersion)},
			Src:              feeder.Ref(w.kind.refNamespace, w.key()),
			Dst:              feeder.Ref(feeder.NSK8sService, s.key()),
			Type:             graphv1.EdgeType_EXPOSED_VIA,
			ValidAt:          at.at,
			ValidFromUnknown: at.unknown,
		})
}

// ExposedViaIngress links a workload to an Ingress that routes to a Service selecting it.
func (m *Mapper) ExposedViaIngress(w workloadView, i ingressView, at validTime) *graphv1.EventEnvelope {
	return feeder.UpsertEdge(m.desc,
		m.id("edge", "exposed-via", w.key()+"->"+kindIngress.idPart+"/"+i.key()+"@rv"+i.resourceVersion),
		feeder.EdgeFact{
			Meta:             feeder.Meta{Seq: seqOf(i.resourceVersion)},
			Src:              feeder.Ref(w.kind.refNamespace, w.key()),
			Dst:              feeder.Ref(feeder.NSK8sIngress, i.key()),
			Type:             graphv1.EdgeType_EXPOSED_VIA,
			ValidAt:          at.at,
			ValidFromUnknown: at.unknown,
		})
}

// RetractNode ends an object's valid interval at the instant the deletion was observed.
// Nothing is removed from history: the object stays queryable as of any instant inside its
// interval (constitution II).
func (m *Mapper) RetractNode(kind resourceKind, namespace, name, resourceVersion string, at time.Time) *graphv1.EventEnvelope {
	target := key(namespace, name)
	return feeder.RetractNode(m.desc, m.id("retract", kind.idPart+"/"+target+"@rv"+resourceVersion), feeder.NodeRetraction{
		Meta:     feeder.Meta{Seq: seqOf(resourceVersion)},
		Ref:      feeder.Ref(kind.refNamespace, target),
		ValidEnd: at,
	})
}

// RetractEdge ends a relationship's valid interval. `label` distinguishes the edges a single
// deletion ends, and the destination is named by its namespace as well as its value, so that
// two edges from one workload to two different kinds of thing with the same name — a Service
// and an Ingress both called `storefront` — never collide on one event id.
func (m *Mapper) RetractEdge(label string, src, dst *graphv1.Ref, edge graphv1.EdgeType, resourceVersion string, at time.Time) *graphv1.EventEnvelope {
	return feeder.RetractEdge(m.desc,
		m.id("retract", "edge", label, src.GetValue()+"->"+dst.GetNamespace()+"/"+dst.GetValue()+"@rv"+resourceVersion),
		feeder.EdgeRetraction{
			Meta:     feeder.Meta{Seq: seqOf(resourceVersion)},
			Src:      src,
			Dst:      dst,
			Type:     edge,
			ValidEnd: at,
		})
}

// labelOf reads a label from the object, falling back to the pod template: a chart that labels
// only the template is as clear a declaration as one that labels both.
func labelOf(w workloadView, key string) string {
	if value := firstValue(w.labels, key); value != "" {
		return value
	}
	return firstValue(w.podLabels, key)
}

// annotationOf reads an annotation from the object, falling back to the pod template.
func annotationOf(w workloadView, key string) string {
	if value := firstValue(w.annotations, key); value != "" {
		return value
	}
	return firstValue(w.podAnnotations, key)
}
