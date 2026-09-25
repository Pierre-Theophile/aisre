// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Generating the recorded corpus (T061).
//
// Docker is not available on this machine, so there is no cluster to record from: the payloads
// under testdata/ are synthesized here instead, from the objects
// deploy/kind/otel-demo/minimal/ applies and the node labels deploy/kind/cluster.yaml sets. That
// makes them synthetic, which constitution VIII says is not enough for a connector to be called
// stable — the manifests say so, and T058 replaces them with a real recording by swapping the
// directory, not by changing any code, because the feeder cannot tell the difference (FR-044).
//
// Regenerate with:
//
//	SRE_AGENT_GEN_TESTDATA=1 go test ./internal/feeders/k8s -run TestGenerateTestdata
//
// The output is byte-for-byte reproducible: every resourceVersion and every timestamp is a
// literal below, and nothing reads a clock.

// genEnv is the environment variable that arms the generator. Regenerating a corpus is a
// deliberate act: the corpus is the test.
const genEnv = "SRE_AGENT_GEN_TESTDATA"

// The fixture clock. The baseline starts at the same instant as fixtures/baseline-topology-01,
// which is what lets it be compared with the 40 events that fixture records for `k8s:demo`.
var (
	fixtureStart = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	fixtureSync  = time.Date(2026, 9, 1, 13, 1, 20, 0, time.UTC)
)

// fixtureOptions is the configuration both corpora were recorded with. `feed k8s --replay`
// reproduces them exactly when given the same flags.
func fixtureOptions() k8sfeeder.Options {
	return k8sfeeder.Options{
		SourceID:    "k8s:demo",
		ClusterName: "shop-prod",
		Namespaces:  []string{"shop"},
		Environment: "prod",
	}
}

func TestGenerateTestdata(t *testing.T) {
	if os.Getenv(genEnv) == "" {
		t.Skipf("set %s=1 to regenerate the recorded corpora", genEnv)
	}
	writeCorpus(t, "testdata/baseline-shop", baseline(t),
		"the shop topology of deploy/kind/otel-demo/minimal as one informer list: "+
			"6 nodes in one pool, 4 Deployments, 2 Services, 1 Ingress, 1 ConfigMap, 1 Secret")
	writeCorpus(t, "testdata/rollout-scale-config", rolloutPayloads(t),
		"the same list, then a payments rollout, a storefront scale-up, a checkout-config "+
			"change and the deletion of the checkout Service")
}

// writeCorpus replays payloads through the feeder with both recorders in place, which is
// exactly what `feed k8s --record` does (the SDK guide §9).
func writeCorpus(t *testing.T, dir string, payloads []feeder.Payload, description string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clear %s: %v", dir, err)
	}
	f, err := k8sfeeder.New(fixtureOptions(), nil)
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	desc := f.Describe()

	src := record.Wrap(source.NewSliceSource(payloads), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(func() time.Time { return fixtureStart }))
	events := record.Emitter(memory, dir)

	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := src.Err(); err != nil {
		t.Fatalf("record payloads: %v", err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("record events: %v", err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%d events were refused; the first is %s (%s)",
			len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family:         "k8s-topology",
		Description:    description + ". Synthetic: authored from deploy/kind/, not recorded from a cluster (T058 replaces it).",
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		ExpectRejected: events.Rejections(),
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Logf("%s: %d payloads, %d events", dir, src.Count(), events.Accepted())
}

// payloadOf renders one watch event the way informers.go does.
func payloadOf(t *testing.T, kind, eventType string, object any, rv string, at time.Time) feeder.Payload {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	bytes, err := json.Marshal(k8sfeeder.WatchEvent{Type: eventType, Object: encoded})
	if err != nil {
		t.Fatalf("marshal watch event: %v", err)
	}
	var seq int64
	if _, err := fmt.Sscanf(rv, "%d", &seq); err != nil {
		seq = 0
	}
	return feeder.Payload{Kind: kind, At: at, Seq: seq, Bytes: bytes}
}

// baseline is the informer's initial list of the shop.
func baseline(t *testing.T) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	out = append(out, payloadOf(t, k8sfeeder.KindNamespaces, k8sfeeder.EventAdded, shopNamespace(), "100", fixtureStart))
	for i := 1; i <= 6; i++ {
		rv := fmt.Sprintf("%d", 899+i)
		out = append(out, payloadOf(t, k8sfeeder.KindNodes, k8sfeeder.EventAdded, generalNode(i, rv), rv, fixtureStart))
	}
	for _, d := range baselineDeployments() {
		out = append(out, payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, d, d.ResourceVersion, fixtureStart))
	}
	for _, s := range baselineServices() {
		out = append(out, payloadOf(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, s, s.ResourceVersion, fixtureStart))
	}
	ing := storefrontIngress()
	out = append(out, payloadOf(t, k8sfeeder.KindIngresses, k8sfeeder.EventAdded, ing, ing.ResourceVersion, fixtureStart))
	cm := checkoutConfig("1201", map[string]string{
		"CART_TTL_SECONDS": "900",
		"PRICING_ROUNDING": "half-even",
		"FEATURE_FLAGS":    "express-checkout",
	})
	out = append(out, payloadOf(t, k8sfeeder.KindConfigMaps, k8sfeeder.EventAdded, cm, cm.ResourceVersion, fixtureStart))
	secret := paymentsSecret("1202")
	out = append(out, payloadOf(t, k8sfeeder.KindSecrets, k8sfeeder.EventAdded, secret, secret.ResourceVersion, fixtureStart))
	out = append(out, feeder.Payload{Kind: k8sfeeder.KindSync, At: fixtureSync, Seq: 1500})
	return out
}

// rolloutPayloads is the baseline list followed by four things happening, each far enough apart
// that the declared 60-second reordering window cannot swap them: a change is the difference
// between two versions of an object, and two versions that could arrive in either order are
// not a difference anyone can name (map_changes.go).
func rolloutPayloads(t *testing.T) []feeder.Payload {
	t.Helper()
	out := baseline(t)

	rollout := paymentsDeployment()
	rollout.ResourceVersion = "1050"
	rollout.Annotations[k8sfeeder.AnnotationRevision] = "8"
	rollout.Annotations[k8sfeeder.AnnotationChangeCause] = "deploy-bot"
	rollout.Spec.Template.Spec.Containers[0].Image = "nginx:1.31.6-alpine-slim"
	rolloutAt := time.Date(2026, 9, 1, 13, 5, 0, 0, time.UTC)
	rollout.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:           appsv1.DeploymentProgressing,
		Status:         corev1.ConditionTrue,
		LastUpdateTime: metav1.NewTime(rolloutAt),
	}}
	out = append(out, payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventModified, rollout, rollout.ResourceVersion, rolloutAt))

	scale := storefrontDeployment()
	scale.ResourceVersion = "1060"
	scale.Spec.Replicas = ptr(int32(6))
	scaleAt := time.Date(2026, 9, 1, 13, 10, 0, 0, time.UTC)
	scale.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager:   "horizontal-pod-autoscaler",
		Operation: metav1.ManagedFieldsOperationUpdate,
		Time:      ptrTime(metav1.NewTime(scaleAt)),
	}}
	out = append(out, payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventModified, scale, scale.ResourceVersion, scaleAt))

	config := checkoutConfig("1210", map[string]string{
		"CART_TTL_SECONDS": "60",
		"PRICING_ROUNDING": "half-even",
		"FEATURE_FLAGS":    "express-checkout",
	})
	configAt := time.Date(2026, 9, 1, 13, 15, 0, 0, time.UTC)
	out = append(out, payloadOf(t, k8sfeeder.KindConfigMaps, k8sfeeder.EventModified, config, config.ResourceVersion, configAt))

	deleted := checkoutService()
	deleteAt := time.Date(2026, 9, 1, 13, 20, 0, 0, time.UTC)
	out = append(out, payloadOf(t, k8sfeeder.KindServices, k8sfeeder.EventDeleted, deleted, deleted.ResourceVersion, deleteAt))
	return out
}

func shopNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:            "shop",
		ResourceVersion: "100",
		Labels: map[string]string{
			"app.kubernetes.io/part-of":   "shop",
			"deployment.environment.name": "prod",
		},
	}}
}

// generalNode is one worker of the single pool the demo workloads run on. The pool label is
// `node-pool`, which is the key fixtures/baseline-topology-01 writes into the node pool's
// SOURCE_LINK selector (deploy/kind/cluster.yaml sets both spellings).
func generalNode(i int, rv string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:            fmt.Sprintf("shop-prod-general-%d", i),
		ResourceVersion: rv,
		Labels: map[string]string{
			"node-pool":                     "general",
			"topology.kubernetes.io/region": "eu-west-1",
		},
	}}
}

func baselineDeployments() []*appsv1.Deployment {
	return []*appsv1.Deployment{
		checkoutDeployment(), inventoryDeployment(), paymentsDeployment(), storefrontDeployment(),
	}
}

// shopDeployment is the shape every workload in deploy/kind/otel-demo/minimal shares.
func shopDeployment(name, team, rv string) *appsv1.Deployment {
	labels := map[string]string{
		k8sfeeder.LabelAppName:      name,
		"app.kubernetes.io/part-of": "shop",
		"team":                      team,
	}
	annotations := map[string]string{
		k8sfeeder.AnnotationServiceName: name,
		k8sfeeder.AnnotationRevision:    "7",
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "shop",
			UID:             stableUID(name),
			ResourceVersion: rv,
			Labels:          labels,
			Annotations:     annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{k8sfeeder.LabelAppName: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{k8sfeeder.AnnotationServiceName: name},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "app",
					Image: "nginx:1.31.5-alpine-slim",
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
				}}},
			},
		},
	}
}

func checkoutDeployment() *appsv1.Deployment {
	d := shopDeployment("checkout", "team-checkout", "1001")
	d.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "checkout-config"},
		},
	}}
	return d
}

func inventoryDeployment() *appsv1.Deployment {
	return shopDeployment("inventory", "team-payments", "1002")
}

func paymentsDeployment() *appsv1.Deployment {
	d := shopDeployment("payments", "team-payments", "1003")
	d.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "payments-secret", MountPath: "/etc/payments", ReadOnly: true,
	}}
	d.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name:         "payments-secret",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "payments-secret"}},
	}}
	return d
}

func storefrontDeployment() *appsv1.Deployment {
	return shopDeployment("storefront", "team-checkout", "1004")
}

func baselineServices() []*corev1.Service {
	return []*corev1.Service{checkoutService(), storefrontService()}
}

func shopService(name, rv string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "shop",
			ResourceVersion: rv,
			Labels: map[string]string{
				k8sfeeder.LabelAppName:      name,
				"app.kubernetes.io/part-of": "shop",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{k8sfeeder.LabelAppName: name},
			Ports: []corev1.ServicePort{{
				Name: "http", Port: 80, TargetPort: intstr.FromString("http"),
			}},
		},
	}
}

func checkoutService() *corev1.Service   { return shopService("checkout", "1101") }
func storefrontService() *corev1.Service { return shopService("storefront", "1102") }

func storefrontIngress() *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "storefront",
			Namespace:       "shop",
			ResourceVersion: "1120",
			Labels: map[string]string{
				k8sfeeder.LabelAppName:      "storefront",
				"app.kubernetes.io/part-of": "shop",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr("nginx"),
			Rules: []networkingv1.IngressRule{{
				Host: "shop.example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: "storefront",
							Port: networkingv1.ServiceBackendPort{Name: "http"},
						}},
					}}},
				},
			}},
		},
	}
}

func checkoutConfig(rv string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "checkout-config",
			Namespace:       "shop",
			ResourceVersion: rv,
			Labels: map[string]string{
				k8sfeeder.LabelAppName:      "checkout",
				"app.kubernetes.io/part-of": "shop",
			},
		},
		Data: data,
	}
}

// paymentsSecret is recorded exactly as the informer would hand it on: the value is already a
// digest of itself, because informers.go redacts a Secret before it ever becomes a payload.
func paymentsSecret(rv string) *corev1.Secret {
	return k8sfeeder.RedactSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "payments-secret",
			Namespace:       "shop",
			ResourceVersion: rv,
			Labels: map[string]string{
				k8sfeeder.LabelAppName:      "payments",
				"app.kubernetes.io/part-of": "shop",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"payment_provider_token": []byte("not-a-real-token-disposable-demo-cluster"),
		},
	})
}

func ptr[T any](v T) *T { return &v }

func ptrTime(t metav1.Time) *metav1.Time { return &t }

// stableUID renders a reproducible UID for an object, so that regenerating the corpus produces
// the same bytes. A real cluster's UIDs are random; nothing the feeder emits depends on them,
// which is why replacing this corpus with a real recording (T058) changes no event.
func stableUID(name string) k8stypes.UID {
	return k8stypes.UID("00000000-0000-4000-8000-" + pad(name))
}

func pad(name string) string {
	const width = 12
	digest := ""
	for _, r := range name {
		digest += fmt.Sprintf("%02x", byte(r))
	}
	for len(digest) < width {
		digest += "0"
	}
	return digest[:width]
}
