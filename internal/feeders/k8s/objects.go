// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Normalising Kubernetes objects (FR-043).
//
// Five workload types, two exposure types and two configuration types become three internal
// views. The mappers in map_workloads.go and map_changes.go read only those views, which is
// what lets one implementation cover Deployments and CronJobs without a switch in every
// function, and what makes the mapping testable without a cluster.
//
// Nothing here reaches outside the object it is given. A view is a pure function of one
// Kubernetes object plus Options, so two feeders replaying the same payloads in different
// orders build the same views (FR-048).

// Payload kinds. They are the directory names a recording groups payloads under, and the
// `kind` field of a watch payload, so they are part of the fixture format.
const (
	KindDeployments  = "deployments"
	KindStatefulSets = "statefulsets"
	KindDaemonSets   = "daemonsets"
	KindJobs         = "jobs"
	KindCronJobs     = "cronjobs"
	KindServices     = "services"
	KindIngresses    = "ingresses"
	KindConfigMaps   = "configmaps"
	KindSecrets      = "secrets"
	KindNodes        = "nodes"
	KindNamespaces   = "namespaces"
	// KindSync is the marker the informers push once every initial list has been delivered.
	// It carries no object; it is what makes the feeder emit its first checkpoint (FR-032).
	KindSync = "sync"
)

// Watch event types, as the Kubernetes watch API spells them.
const (
	EventAdded    = "ADDED"
	EventModified = "MODIFIED"
	EventDeleted  = "DELETED"
)

// resourceKind is everything that differs between two watched resource types.
type resourceKind struct {
	// payload is the Payload.Kind informers push and a recording stores under.
	payload string
	// idPart is the second part of every event id minted for this resource, e.g. "deploy".
	idPart string
	// refNamespace is the identifier namespace its refs live in.
	refNamespace string
	// apiPath is the group-version prefix of a resource path, e.g. "apps/v1".
	apiPath string
	// plural is the resource's plural name in that path, e.g. "deployments".
	plural string
	// nameAttr is the OpenTelemetry attribute naming this kind of object, e.g.
	// "k8s.deployment.name".
	nameAttr string
}

// The watched resource kinds. `kind` is the wire name; the map is what the payload decoder
// dispatches on.
var (
	kindDeployment = resourceKind{
		payload: KindDeployments, idPart: "deploy", refNamespace: feeder.NSK8sDeployment,
		apiPath: "apps/v1", plural: "deployments", nameAttr: feeder.AttrK8sDeploymentName,
	}
	kindStatefulSet = resourceKind{
		payload: KindStatefulSets, idPart: "sts", refNamespace: feeder.NSK8sStatefulSet,
		apiPath: "apps/v1", plural: "statefulsets", nameAttr: "k8s.statefulset.name",
	}
	kindDaemonSet = resourceKind{
		payload: KindDaemonSets, idPart: "ds", refNamespace: feeder.NSK8sDaemonSet,
		apiPath: "apps/v1", plural: "daemonsets", nameAttr: "k8s.daemonset.name",
	}
	kindJob = resourceKind{
		payload: KindJobs, idPart: "job", refNamespace: feeder.NSK8sJob,
		apiPath: "batch/v1", plural: "jobs", nameAttr: "k8s.job.name",
	}
	kindCronJob = resourceKind{
		payload: KindCronJobs, idPart: "cronjob", refNamespace: feeder.NSK8sCronJob,
		apiPath: "batch/v1", plural: "cronjobs", nameAttr: "k8s.cronjob.name",
	}
	kindService = resourceKind{
		payload: KindServices, idPart: "svc", refNamespace: feeder.NSK8sService,
		apiPath: "v1", plural: "services", nameAttr: feeder.AttrK8sServiceName,
	}
	kindIngress = resourceKind{
		payload: KindIngresses, idPart: "ing", refNamespace: feeder.NSK8sIngress,
		apiPath: "networking.k8s.io/v1", plural: "ingresses", nameAttr: feeder.AttrK8sIngressName,
	}
	kindConfigMap = resourceKind{
		payload: KindConfigMaps, idPart: "cfg", refNamespace: feeder.NSK8sConfigMap,
		apiPath: "v1", plural: "configmaps", nameAttr: feeder.AttrK8sConfigMapName,
	}
	kindSecret = resourceKind{
		payload: KindSecrets, idPart: "sec", refNamespace: feeder.NSK8sSecret,
		apiPath: "v1", plural: "secrets", nameAttr: feeder.AttrK8sSecretName,
	}
)

// WatchedResources are the payload kinds the informers produce, in the order the informers are
// started. It is also what permissions.go checks the credential against.
var WatchedResources = []string{
	KindNamespaces, KindNodes,
	KindDeployments, KindStatefulSets, KindDaemonSets, KindJobs, KindCronJobs,
	KindServices, KindIngresses, KindConfigMaps, KindSecrets,
}

// kindsByPayload maps a payload kind back to everything that differs about it, which is how a
// reconciling feeder turns an object key read out of a state file into a retraction (state.go).
var kindsByPayload = map[string]resourceKind{
	KindDeployments:  kindDeployment,
	KindStatefulSets: kindStatefulSet,
	KindDaemonSets:   kindDaemonSet,
	KindJobs:         kindJob,
	KindCronJobs:     kindCronJob,
	KindServices:     kindService,
	KindIngresses:    kindIngress,
	KindConfigMaps:   kindConfigMap,
	KindSecrets:      kindSecret,
}

// kindOfPayload resolves a payload kind, reporting whether this feeder watches it.
func kindOfPayload(payload string) (resourceKind, bool) {
	kind, known := kindsByPayload[payload]
	return kind, known
}

// key identifies an object inside its namespace: `<namespace>/<name>`, which is also the value
// half of every Ref this feeder mints.
func key(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// configReference is how a workload consumes a ConfigMap or a Secret. The value becomes the
// `sre.k8s.reference` property of the depends-on edge.
const (
	referenceEnvFrom = "envFrom"
	referenceEnv     = "env"
	referenceVolume  = "volume"
)

// configRef is one ConfigMap or Secret a workload consumes, and how.
type configRef struct {
	kind      resourceKind
	name      string
	reference string
}

// workloadView is one workload object, whatever its type, reduced to what the mapping needs.
type workloadView struct {
	kind            resourceKind
	namespace       string
	name            string
	uid             string
	resourceVersion string
	labels          map[string]string
	annotations     map[string]string
	podLabels       map[string]string
	podAnnotations  map[string]string
	nodeSelector    map[string]string
	affinityPools   []string
	replicas        *int64
	revision        string
	image           string
	managers        []metav1.ManagedFieldsEntry
	configRefs      []configRef
	// progressingAt is when the controller last reported progress on this workload. It is the
	// valid time of a rollout: the moment the new revision started being true, as the cluster
	// asserts it, rather than the moment this feeder read about it.
	progressingAt time.Time
	// observedAt is when the payload carrying this version arrived. It is not part of the
	// object; it is what an edge between two objects uses as its valid time, so that the edge
	// is a function of the pair rather than of whichever half arrived second.
	observedAt time.Time
}

// ref is the workload's identifier, e.g. `k8s.deployment=shop/checkout`.
func (w workloadView) key() string { return key(w.namespace, w.name) }

// resourcePath is the Kubernetes API path of the object, the selector of its SOURCE_LINK
// pointer and the origin of every change observed on it.
func (w workloadView) resourcePath() string {
	return resourcePath(w.kind, w.namespace, w.name)
}

// resourcePath renders `<apiPath>/namespaces/<ns>/<plural>/<name>`, the spelling
// fixtures/baseline-topology-01 records.
func resourcePath(k resourceKind, namespace, name string) string {
	return k.apiPath + "/namespaces/" + namespace + "/" + k.plural + "/" + name
}

// serviceView is a Service reduced to the selector that decides which workloads it exposes.
type serviceView struct {
	namespace       string
	name            string
	uid             string
	resourceVersion string
	labels          map[string]string
	annotations     map[string]string
	selector        map[string]string
	ports           []string
	observedAt      time.Time
}

func (s serviceView) key() string { return key(s.namespace, s.name) }

// ingressView is an Ingress reduced to its host and the services it routes to.
type ingressView struct {
	namespace       string
	name            string
	uid             string
	resourceVersion string
	labels          map[string]string
	annotations     map[string]string
	host            string
	backends        []string
	observedAt      time.Time
}

func (i ingressView) key() string { return key(i.namespace, i.name) }

// configView is a ConfigMap or a Secret. `valueHash` is set for a ConfigMap and `version` for
// a Secret: a secret's content never leaves the cluster, not even as a digest of a value an
// attacker could enumerate (constitution IV, spec edge case "secrets").
type configView struct {
	kind            resourceKind
	namespace       string
	name            string
	uid             string
	resourceVersion string
	labels          map[string]string
	annotations     map[string]string
	valueHash       string
	version         string
	// observedAt is when the payload carrying this version arrived, which is what decides
	// whether the CONFIG node it asserts is initial state or a change (Feeder.validFor).
	observedAt time.Time
}

func (c configView) key() string { return key(c.namespace, c.name) }

// nodeView is a Node reduced to the pool and region it places a workload in.
type nodeView struct {
	name            string
	resourceVersion string
	labels          map[string]string
	pool            string
	region          string
}

// newWorkloadView reduces any of the five workload types to one view.
func newWorkloadView(k resourceKind, meta metav1.ObjectMeta, spec corev1.PodSpec, podMeta metav1.ObjectMeta, replicas *int32) workloadView {
	w := workloadView{
		kind:            k,
		namespace:       meta.Namespace,
		name:            meta.Name,
		uid:             string(meta.UID),
		resourceVersion: meta.ResourceVersion,
		labels:          meta.Labels,
		annotations:     meta.Annotations,
		podLabels:       podMeta.Labels,
		podAnnotations:  podMeta.Annotations,
		nodeSelector:    spec.NodeSelector,
		affinityPools:   affinityPools(spec.Affinity),
		revision:        firstValue(meta.Annotations, AnnotationRevision),
		image:           primaryImage(spec),
		managers:        meta.ManagedFields,
		configRefs:      configRefsOf(spec),
	}
	if replicas != nil {
		n := int64(*replicas)
		w.replicas = &n
	}
	return w
}

// affinityPools collects the values a required node affinity demands for a pool label, so that
// a workload pinned with affinity rather than a nodeSelector still lands on its pool.
func affinityPools(affinity *corev1.Affinity) []string {
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	var out []string
	for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			for _, value := range expr.Values {
				out = append(out, expr.Key+"="+value)
			}
		}
	}
	return out
}

// primaryImage is the image of the first container, which is what a rollout summary names.
func primaryImage(spec corev1.PodSpec) string {
	if len(spec.Containers) == 0 {
		return ""
	}
	return spec.Containers[0].Image
}

// configRefsOf collects every ConfigMap and Secret a pod spec consumes, deduplicated by
// (kind, name) with the strongest reference kept: a ConfigMap taken through envFrom *and*
// mounted is reported once, as envFrom, because that is the reference an operator recognises.
func configRefsOf(spec corev1.PodSpec) []configRef {
	seen := map[string]int{}
	var out []configRef
	add := func(kind resourceKind, name, reference string) {
		if name == "" {
			return
		}
		id := kind.payload + "/" + name
		if i, ok := seen[id]; ok {
			if referenceRank(reference) < referenceRank(out[i].reference) {
				out[i].reference = reference
			}
			return
		}
		seen[id] = len(out)
		out = append(out, configRef{kind: kind, name: name, reference: reference})
	}

	containers := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	containers = append(containers, spec.InitContainers...)
	containers = append(containers, spec.Containers...)
	for _, c := range containers {
		for _, source := range c.EnvFrom {
			if source.ConfigMapRef != nil {
				add(kindConfigMap, source.ConfigMapRef.Name, referenceEnvFrom)
			}
			if source.SecretRef != nil {
				add(kindSecret, source.SecretRef.Name, referenceEnvFrom)
			}
		}
		for _, env := range c.Env {
			if env.ValueFrom == nil {
				continue
			}
			if env.ValueFrom.ConfigMapKeyRef != nil {
				add(kindConfigMap, env.ValueFrom.ConfigMapKeyRef.Name, referenceEnv)
			}
			if env.ValueFrom.SecretKeyRef != nil {
				add(kindSecret, env.ValueFrom.SecretKeyRef.Name, referenceEnv)
			}
		}
	}
	for _, volume := range spec.Volumes {
		if volume.ConfigMap != nil {
			add(kindConfigMap, volume.ConfigMap.Name, referenceVolume)
		}
		if volume.Secret != nil {
			add(kindSecret, volume.Secret.SecretName, referenceVolume)
		}
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ConfigMap != nil {
				add(kindConfigMap, source.ConfigMap.Name, referenceVolume)
			}
			if source.Secret != nil {
				add(kindSecret, source.Secret.Name, referenceVolume)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].kind.payload != out[j].kind.payload {
			return out[i].kind.payload < out[j].kind.payload
		}
		return out[i].name < out[j].name
	})
	return out
}

// referenceRank orders the reference kinds so that deduplication is deterministic.
func referenceRank(reference string) int {
	switch reference {
	case referenceEnvFrom:
		return 0
	case referenceVolume:
		return 1
	default:
		return 2
	}
}

// viewOfDeployment and friends adapt one typed object to a workloadView.
func viewOfDeployment(o *appsv1.Deployment) workloadView {
	w := newWorkloadView(kindDeployment, o.ObjectMeta, o.Spec.Template.Spec, o.Spec.Template.ObjectMeta, o.Spec.Replicas)
	for _, condition := range o.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && !condition.LastUpdateTime.IsZero() {
			w.progressingAt = condition.LastUpdateTime.UTC()
		}
	}
	return w
}

func viewOfStatefulSet(o *appsv1.StatefulSet) workloadView {
	return newWorkloadView(kindStatefulSet, o.ObjectMeta, o.Spec.Template.Spec, o.Spec.Template.ObjectMeta, o.Spec.Replicas)
}

func viewOfDaemonSet(o *appsv1.DaemonSet) workloadView {
	return newWorkloadView(kindDaemonSet, o.ObjectMeta, o.Spec.Template.Spec, o.Spec.Template.ObjectMeta, nil)
}

func viewOfJob(o *batchv1.Job) workloadView {
	return newWorkloadView(kindJob, o.ObjectMeta, o.Spec.Template.Spec, o.Spec.Template.ObjectMeta, o.Spec.Parallelism)
}

func viewOfCronJob(o *batchv1.CronJob) workloadView {
	template := o.Spec.JobTemplate.Spec.Template
	return newWorkloadView(kindCronJob, o.ObjectMeta, template.Spec, template.ObjectMeta, nil)
}

func viewOfService(o *corev1.Service) serviceView {
	ports := make([]string, 0, len(o.Spec.Ports))
	for _, p := range o.Spec.Ports {
		name := p.Name
		if name == "" {
			name = strconv.Itoa(int(p.Port))
		}
		ports = append(ports, name)
	}
	return serviceView{
		namespace:       o.Namespace,
		name:            o.Name,
		uid:             string(o.UID),
		resourceVersion: o.ResourceVersion,
		labels:          o.Labels,
		annotations:     o.Annotations,
		selector:        o.Spec.Selector,
		ports:           ports,
	}
}

func viewOfIngress(o *networkingv1.Ingress) ingressView {
	view := ingressView{
		namespace:       o.Namespace,
		name:            o.Name,
		uid:             string(o.UID),
		resourceVersion: o.ResourceVersion,
		labels:          o.Labels,
		annotations:     o.Annotations,
	}
	seen := map[string]bool{}
	if o.Spec.DefaultBackend != nil && o.Spec.DefaultBackend.Service != nil {
		view.backends = append(view.backends, o.Spec.DefaultBackend.Service.Name)
		seen[o.Spec.DefaultBackend.Service.Name] = true
	}
	for _, rule := range o.Spec.Rules {
		if view.host == "" {
			view.host = rule.Host
		}
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil || seen[path.Backend.Service.Name] {
				continue
			}
			seen[path.Backend.Service.Name] = true
			view.backends = append(view.backends, path.Backend.Service.Name)
		}
	}
	sort.Strings(view.backends)
	return view
}

func viewOfConfigMap(o *corev1.ConfigMap) configView {
	return configView{
		kind:            kindConfigMap,
		namespace:       o.Namespace,
		name:            o.Name,
		uid:             string(o.UID),
		resourceVersion: o.ResourceVersion,
		labels:          o.Labels,
		annotations:     o.Annotations,
		valueHash:       configMapValueHash(o),
	}
}

func viewOfSecret(o *corev1.Secret) configView {
	return configView{
		kind:            kindSecret,
		namespace:       o.Namespace,
		name:            o.Name,
		uid:             string(o.UID),
		resourceVersion: o.ResourceVersion,
		labels:          o.Labels,
		annotations:     o.Annotations,
		version:         o.ResourceVersion,
	}
}

func viewOfNode(o *corev1.Node, opts Options) nodeView {
	return nodeView{
		name:            o.Name,
		resourceVersion: o.ResourceVersion,
		labels:          o.Labels,
		pool:            opts.nodePoolOf(o.Labels),
		region:          firstValue(o.Labels, LabelRegion),
	}
}

// configMapValueHash digests a ConfigMap's content so that a change to it is visible without
// the content ever being stored (FR-008). The digest is over a canonical rendering — keys
// sorted, `key=value` lines — so two feeders reading the same ConfigMap agree.
func configMapValueHash(o *corev1.ConfigMap) string {
	keys := make([]string, 0, len(o.Data)+len(o.BinaryData))
	for k := range o.Data {
		keys = append(keys, k)
	}
	for k := range o.BinaryData {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	digest := sha256.New()
	for _, k := range keys {
		if value, ok := o.Data[k]; ok {
			fmt.Fprintf(digest, "%s=%s\n", k, value)
			continue
		}
		fmt.Fprintf(digest, "%s=%x\n", k, sha256.Sum256(o.BinaryData[k]))
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// seqOf parses a resourceVersion as the per-source sequence number. Kubernetes does not
// promise a resourceVersion is numeric, so an unparseable one is reported as "no sequence"
// rather than as a wrong one (FR-021).
func seqOf(resourceVersion string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(resourceVersion), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// matchesSelector reports whether a workload's pod labels satisfy a Service's selector. An
// empty selector matches nothing: a Service without one is routed by an operator-managed
// Endpoints object, which this feeder does not watch and must not guess at.
func matchesSelector(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, want := range selector {
		if labels[k] != want {
			return false
		}
	}
	return true
}
