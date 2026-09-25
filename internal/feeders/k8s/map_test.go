// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Mapping, one object at a time (T057, T059).
//
// The conformance corpora prove the whole stream; these prove the cases a corpus of one healthy
// shop cannot contain — a workload pinned by affinity, a ConfigMap referenced two ways at once,
// a rollout with no revision annotation, a deletion.
//
// Everything goes through Feeder.MapPayload, which is the surface `--dry-run` and the informers
// both use, so a test here is a test of the path the feeder actually takes.

// mapOne decodes one watch event through a fresh feeder and returns the events it asserts.
func mapOne(t *testing.T, f *k8sfeeder.Feeder, kind, eventType string, object any, at time.Time) []*graphv1.EventEnvelope {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := json.Marshal(k8sfeeder.WatchEvent{Type: eventType, Object: encoded})
	if err != nil {
		t.Fatalf("marshal watch event: %v", err)
	}
	events, err := f.MapPayload(kind, raw, at)
	if err != nil {
		t.Fatalf("map %s: %v", kind, err)
	}
	for _, ev := range events {
		if rejection := feeder.Validate(ev); rejection != nil {
			t.Fatalf("%s does not validate: %s (%s)", ev.GetEventId(), rejection.ReasonCode, rejection.ReasonDetail)
		}
	}
	return events
}

// edgeTo finds the first edge of a type and returns its destination and properties.
func edgeTo(events []*graphv1.EventEnvelope, edge graphv1.EdgeType) (*graphv1.UpsertEdge, bool) {
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertEdge)
		if ok && body.UpsertEdge.GetType() == edge {
			return body.UpsertEdge, true
		}
	}
	return nil, false
}

func changeOf(events []*graphv1.EventEnvelope, kind graphv1.ChangeKind) (*graphv1.ObserveChange, bool) {
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_ObserveChange)
		if ok && body.ObserveChange.GetChange().GetKind() == kind {
			return body.ObserveChange, true
		}
	}
	return nil, false
}

// TestNodePoolDerivation: a workload's pool comes from what it declares, and where it declares
// nothing from configuration — never from a guess, because pods are not watched and the real
// placement is not observable (research §12).
func TestNodePoolDerivation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*appsv1.Deployment)
		want string
	}{
		{
			name: "nodeSelector",
			edit: func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.NodeSelector = map[string]string{"sre.node_pool": "memory"}
			},
			want: "shop-prod/memory",
		},
		{
			name: "the second configured label",
			edit: func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.NodeSelector = map[string]string{"node-pool": "spot"}
			},
			want: "shop-prod/spot",
		},
		{
			name: "required node affinity",
			edit: func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "node-pool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"batch"},
						}}}},
					},
				}}
			},
			want: "shop-prod/batch",
		},
		{
			name: "nothing declared falls back to the configured default",
			edit: func(*appsv1.Deployment) {},
			want: "shop-prod/general",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := checkoutDeployment()
			tc.edit(d)
			events := mapOne(t, testFeeder(t), k8sfeeder.KindDeployments, k8sfeeder.EventAdded, d, fixtureStart)
			edge, ok := edgeTo(events, graphv1.EdgeType_RUNS_ON)
			if !ok {
				t.Fatal("no runs-on edge")
			}
			if got := edge.GetDst().GetValue(); got != tc.want {
				t.Errorf("runs-on -> %s, want %s", got, tc.want)
			}
			if ns := edge.GetDst().GetNamespace(); ns != feeder.NSK8sNodePool {
				t.Errorf("destination namespace = %s, want %s", ns, feeder.NSK8sNodePool)
			}
		})
	}
}

// TestConfigReferences: every way a pod spec can name a ConfigMap or a Secret becomes one
// depends-on edge that records how (FR-043).
func TestConfigReferences(t *testing.T) {
	d := checkoutDeployment()
	container := &d.Spec.Template.Spec.Containers[0]
	container.Env = []corev1.EnvVar{{
		Name: "API_KEY",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "api-key"}, Key: "key",
		}},
	}}
	d.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: "tls",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: "tls-cert"},
			}}},
		}},
	}}

	events := mapOne(t, testFeeder(t), k8sfeeder.KindDeployments, k8sfeeder.EventAdded, d, fixtureStart)
	want := map[string]string{
		"k8s.configmap=shop/checkout-config": "envFrom",
		"k8s.secret=shop/api-key":            "env",
		"k8s.secret=shop/tls-cert":           "volume",
	}
	got := map[string]string{}
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertEdge)
		if !ok || body.UpsertEdge.GetType() != graphv1.EdgeType_DEPENDS_ON {
			continue
		}
		got[feeder.RefString(body.UpsertEdge.GetDst())] =
			body.UpsertEdge.GetProps().GetFields()[feeder.PropK8sReference].GetStringValue()
	}
	if len(got) != len(want) {
		t.Fatalf("got %d depends-on edges %v, want %d %v", len(got), got, len(want), want)
	}
	for ref, reference := range want {
		if got[ref] != reference {
			t.Errorf("%s: sre.k8s.reference = %q, want %q", ref, got[ref], reference)
		}
	}
}

// TestConfigMapAndSecretNodes is the secrets edge case: a ConfigMap carries a digest of its
// content and a Secret carries neither content nor digest, only its version (FR-008).
func TestConfigMapAndSecretNodes(t *testing.T) {
	f := testFeeder(t)

	cm := checkoutConfig("1201", map[string]string{"CART_TTL_SECONDS": "900"})
	events := mapOne(t, f, k8sfeeder.KindConfigMaps, k8sfeeder.EventAdded, cm, fixtureStart)
	props := nodeProps(t, events)
	if props[feeder.PropConfigKind] != "configmap" {
		t.Errorf("sre.config.kind = %q, want configmap", props[feeder.PropConfigKind])
	}
	if hash := props[feeder.PropConfigValueHash]; len(hash) != len("sha256:")+64 {
		t.Errorf("sre.config.value_hash = %q, want a sha256 digest", hash)
	}
	if _, present := props[feeder.PropConfigVersion]; present {
		t.Error("a ConfigMap carries a value hash, not a version")
	}

	secret := paymentsSecret("1202")
	events = mapOne(t, f, k8sfeeder.KindSecrets, k8sfeeder.EventAdded, secret, fixtureStart)
	props = nodeProps(t, events)
	if props[feeder.PropConfigKind] != "secret" {
		t.Errorf("sre.config.kind = %q, want secret", props[feeder.PropConfigKind])
	}
	if props[feeder.PropConfigVersion] != "1202" {
		t.Errorf("sre.config.version = %q, want the resourceVersion", props[feeder.PropConfigVersion])
	}
	if _, present := props[feeder.PropConfigValueHash]; present {
		t.Error("a Secret node carries a digest of its content; a short value's digest is the value")
	}
}

func nodeProps(t *testing.T, events []*graphv1.EventEnvelope) map[string]string {
	t.Helper()
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertNode)
		if !ok {
			continue
		}
		out := map[string]string{}
		for key, value := range body.UpsertNode.GetProps().GetFields() {
			out[key] = value.GetStringValue()
		}
		return out
	}
	t.Fatal("no upsert_node event")
	return nil
}

// TestClaimsOnlyForWhatIsDeclared: a workload with no service-name label and no annotation
// claims only the identity this feeder addresses it by. Inventing the other two from the object
// name would be a resemblance, and resemblance is what the probable rules are for (research §10).
func TestClaimsOnlyForWhatIsDeclared(t *testing.T) {
	d := checkoutDeployment()
	d.Labels = map[string]string{"team": "team-checkout"}
	d.Annotations = map[string]string{k8sfeeder.AnnotationRevision: "7"}
	d.Spec.Template.Labels = map[string]string{"role": "checkout"}
	d.Spec.Template.Annotations = nil

	events := mapOne(t, testFeeder(t), k8sfeeder.KindDeployments, k8sfeeder.EventAdded, d, fixtureStart)
	var claims []string
	for _, ev := range events {
		if body, ok := ev.GetBody().(*graphv1.EventEnvelope_IdentityClaim); ok {
			claims = append(claims, feeder.RefString(body.IdentityClaim.GetClaim()))
		}
	}
	if len(claims) != 1 || claims[0] != "k8s.deployment=shop/checkout" {
		t.Fatalf("claims = %v, want only the addressing identity", claims)
	}
}

// TestClaimAttributesAreWhatTheRulesRead pins the attribute keys internal/resolution/certain.go
// looks for. A claim missing them is one no certain rule can decide on.
func TestClaimAttributesAreWhatTheRulesRead(t *testing.T) {
	events := mapOne(t, testFeeder(t), k8sfeeder.KindDeployments, k8sfeeder.EventAdded,
		checkoutDeployment(), fixtureStart)

	want := map[string]map[string]string{
		"otel.service.name=checkout": {
			feeder.AttrDeploymentEnvironment: "prod",
			feeder.AttrK8sNamespaceName:      "shop",
			feeder.PropK8sClaimKey:           k8sfeeder.AnnotationServiceName,
			feeder.PropK8sClaimKind:          "annotation",
		},
		"app.kubernetes.io/name=checkout": {
			feeder.AttrDeploymentEnvironment: "prod",
			feeder.AttrK8sNamespaceName:      "shop",
			feeder.PropK8sClaimKey:           k8sfeeder.LabelAppName,
			feeder.PropK8sClaimKind:          "label",
		},
		"k8s.deployment=shop/checkout": {
			feeder.AttrDeploymentEnvironment: "prod",
			feeder.AttrK8sNamespaceName:      "shop",
			feeder.AttrK8sClusterName:        "shop-prod",
		},
	}
	seen := map[string]bool{}
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_IdentityClaim)
		if !ok {
			continue
		}
		claim := feeder.RefString(body.IdentityClaim.GetClaim())
		expected, known := want[claim]
		if !known {
			t.Errorf("unexpected claim %s", claim)
			continue
		}
		seen[claim] = true
		attrs := body.IdentityClaim.GetAttributes().GetFields()
		if len(attrs) != len(expected) {
			t.Errorf("%s has %d attributes, want %d", claim, len(attrs), len(expected))
		}
		for key, value := range expected {
			if attrs[key].GetStringValue() != value {
				t.Errorf("%s: %s = %q, want %q", claim, key, attrs[key].GetStringValue(), value)
			}
		}
	}
	if len(seen) != len(want) {
		t.Errorf("emitted %d of %d claims", len(seen), len(want))
	}
}

// TestEnvironmentFromTheNamespace: an object that declares no environment inherits the one its
// Namespace declares, which is what deploy/kind/otel-demo/minimal/namespaces.yaml sets.
func TestEnvironmentFromTheNamespace(t *testing.T) {
	f := testFeeder(t)
	staging := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:            "shop",
		ResourceVersion: "100",
		Labels:          map[string]string{"deployment.environment.name": "staging"},
	}}
	mapOne(t, f, k8sfeeder.KindNamespaces, k8sfeeder.EventAdded, staging, fixtureStart)

	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, checkoutDeployment(), fixtureStart)
	if env := nodeProps(t, events)[feeder.AttrDeploymentEnvironment]; env != "staging" {
		t.Errorf("deployment.environment.name = %q, want the namespace's staging", env)
	}
}

// TestRolloutFromARevisionChange is T059's main case.
func TestRolloutFromARevisionChange(t *testing.T) {
	f := testFeeder(t)
	before := paymentsDeployment()
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, before, fixtureStart)

	after := paymentsDeployment()
	after.ResourceVersion = "1050"
	after.Annotations[k8sfeeder.AnnotationRevision] = "8"
	after.Annotations[k8sfeeder.AnnotationChangeCause] = "deploy-bot"
	after.Spec.Template.Spec.Containers[0].Image = "nginx:1.31.6-alpine-slim"
	at := fixtureStart.Add(5 * time.Minute)

	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventModified, after, at)
	change, ok := changeOf(events, graphv1.ChangeKind_ROLLOUT)
	if !ok {
		t.Fatal("no ROLLOUT change")
	}
	if got := change.GetRef().GetValue(); got != "shop/payments@rev8" {
		t.Errorf("ref = %q, want shop/payments@rev8", got)
	}
	if got := change.GetChange().GetSummary(); got != "rollout payments to revision 8 (1.31.6-alpine-slim)" {
		t.Errorf("summary = %q", got)
	}
	if got := change.GetChange().GetActor(); got != "deploy-bot" {
		t.Errorf("actor = %q, want the change cause", got)
	}
	if got := change.GetChange().GetOriginRef(); got != "apps/v1/namespaces/shop/deployments/payments" {
		t.Errorf("originRef = %q", got)
	}
	if len(change.GetTargets()) != 1 || feeder.RefString(change.GetTargets()[0]) != "k8s.deployment=shop/payments" {
		t.Errorf("targets = %v, want the workload", change.GetTargets())
	}
	if !change.GetValidAt().AsTime().Equal(at) {
		t.Errorf("validAt = %s, want %s", change.GetValidAt().AsTime(), at)
	}
}

// TestAddedIsNeverAChange: the informer's initial list, and the first sight of an object after a
// restart, are both ADDED. Reading either as a rollout would put a change node on every start.
func TestAddedIsNeverAChange(t *testing.T) {
	f := testFeeder(t)
	first := paymentsDeployment()
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, first, fixtureStart)

	restarted := paymentsDeployment()
	restarted.ResourceVersion = "1050"
	restarted.Annotations[k8sfeeder.AnnotationRevision] = "8"
	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, restarted, fixtureStart.Add(time.Hour))
	if _, ok := changeOf(events, graphv1.ChangeKind_ROLLOUT); ok {
		t.Fatal("an ADDED payload produced a rollout")
	}
}

// TestRolloutWithoutARevisionAnnotation covers the workload types no controller annotates: the
// image is what changed, and it names the change.
func TestRolloutWithoutARevisionAnnotation(t *testing.T) {
	f := testFeeder(t)
	before := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "node-agent", Namespace: "shop", ResourceVersion: "2001"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "agent:1.0.0"}}},
		}},
	}
	mapOne(t, f, k8sfeeder.KindDaemonSets, k8sfeeder.EventAdded, before, fixtureStart)

	after := before.DeepCopy()
	after.ResourceVersion = "2002"
	after.Spec.Template.Spec.Containers[0].Image = "agent:1.1.0"
	events := mapOne(t, f, k8sfeeder.KindDaemonSets, k8sfeeder.EventModified, after, fixtureStart.Add(time.Minute))

	change, ok := changeOf(events, graphv1.ChangeKind_ROLLOUT)
	if !ok {
		t.Fatal("no ROLLOUT change for a DaemonSet image change")
	}
	if got := change.GetRef().GetValue(); got != "shop/node-agent@rev1.1.0" {
		t.Errorf("ref = %q, want the image tag to name the change", got)
	}
}

// TestScalingAttribution tells an autoscaler from an operator.
func TestScalingAttribution(t *testing.T) {
	at := fixtureStart.Add(75 * time.Minute) // 14:15 UTC, the spelling the fixture publishes
	for _, tc := range []struct {
		manager string
		want    string
	}{
		{"horizontal-pod-autoscaler", "hpa/shop/storefront"},
		{"kubectl-scale", "kubectl-scale"},
	} {
		t.Run(tc.manager, func(t *testing.T) {
			f := testFeeder(t)
			mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, storefrontDeployment(), fixtureStart)

			scaled := storefrontDeployment()
			scaled.ResourceVersion = "1060"
			scaled.Spec.Replicas = ptr(int32(6))
			scaled.ManagedFields = []metav1.ManagedFieldsEntry{{
				Manager:   tc.manager,
				Operation: metav1.ManagedFieldsOperationUpdate,
				Time:      ptrTime(metav1.NewTime(at)),
			}}
			events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventModified, scaled, at)

			change, ok := changeOf(events, graphv1.ChangeKind_SCALING)
			if !ok {
				t.Fatal("no SCALING change")
			}
			if got := change.GetRef().GetValue(); got != "shop/storefront@scale-1415" {
				t.Errorf("ref = %q, want shop/storefront@scale-1415", got)
			}
			if got := change.GetChange().GetSummary(); got != "scale storefront from 3 to 6 replicas" {
				t.Errorf("summary = %q", got)
			}
			if got := change.GetChange().GetActor(); got != tc.want {
				t.Errorf("actor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConfigChangeNeedsAConsumer: a ConfigMap nobody reads produces no change node. A change
// with no blast radius ranks above nothing and explains nothing.
func TestConfigChangeNeedsAConsumer(t *testing.T) {
	f := testFeeder(t)
	mapOne(t, f, k8sfeeder.KindConfigMaps, k8sfeeder.EventAdded,
		checkoutConfig("1201", map[string]string{"A": "1"}), fixtureStart)
	events := mapOne(t, f, k8sfeeder.KindConfigMaps, k8sfeeder.EventModified,
		checkoutConfig("1210", map[string]string{"A": "2"}), fixtureStart.Add(time.Minute))
	if _, ok := changeOf(events, graphv1.ChangeKind_CONFIG_CHANGE); ok {
		t.Fatal("an unreferenced ConfigMap produced a change node")
	}

	// Now the workload that consumes it is known, and the same edit is a change with a target.
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, checkoutDeployment(), fixtureStart)
	events = mapOne(t, f, k8sfeeder.KindConfigMaps, k8sfeeder.EventModified,
		checkoutConfig("1211", map[string]string{"A": "3"}), fixtureStart.Add(2*time.Minute))
	change, ok := changeOf(events, graphv1.ChangeKind_CONFIG_CHANGE)
	if !ok {
		t.Fatal("no CONFIG_CHANGE for a referenced ConfigMap")
	}
	if got := change.GetRef().GetValue(); got != "shop/configmap/checkout-config@rv1211" {
		t.Errorf("ref = %q", got)
	}
	targets := []string{}
	for _, ref := range change.GetTargets() {
		targets = append(targets, feeder.RefString(ref))
	}
	if len(targets) != 2 || targets[0] != "k8s.configmap=shop/checkout-config" ||
		targets[1] != "k8s.deployment=shop/checkout" {
		t.Errorf("targets = %v, want the config node and the workload reading it", targets)
	}
}

// TestTouchedButUnchangedConfigMapIsNotAChange: an annotation edit or a re-apply bumps the
// resourceVersion without changing what the workload reads.
func TestTouchedButUnchangedConfigMapIsNotAChange(t *testing.T) {
	f := testFeeder(t)
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, checkoutDeployment(), fixtureStart)
	mapOne(t, f, k8sfeeder.KindConfigMaps, k8sfeeder.EventAdded,
		checkoutConfig("1201", map[string]string{"A": "1"}), fixtureStart)

	touched := checkoutConfig("1202", map[string]string{"A": "1"})
	touched.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}
	events := mapOne(t, f, k8sfeeder.KindConfigMaps, k8sfeeder.EventModified, touched, fixtureStart.Add(time.Minute))
	if _, ok := changeOf(events, graphv1.ChangeKind_CONFIG_CHANGE); ok {
		t.Fatal("a ConfigMap whose content did not change produced a change node")
	}
}

// TestDeletionRetractsTheWorkloadAndItsEdges: nothing is deleted from history; the valid
// intervals are closed at the instant the deletion was observed (constitution II).
func TestDeletionRetractsTheWorkloadAndItsEdges(t *testing.T) {
	f := testFeeder(t)
	mapOne(t, f, k8sfeeder.KindServices, k8sfeeder.EventAdded, checkoutService(), fixtureStart)
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, checkoutDeployment(), fixtureStart)

	at := fixtureStart.Add(time.Hour)
	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventDeleted, checkoutDeployment(), at)

	var nodes, edges []string
	for _, ev := range events {
		switch body := ev.GetBody().(type) {
		case *graphv1.EventEnvelope_RetractNode:
			nodes = append(nodes, feeder.RefString(body.RetractNode.GetRef()))
			if !body.RetractNode.GetValidEnd().AsTime().Equal(at) {
				t.Errorf("validEnd = %s, want the deletion instant", body.RetractNode.GetValidEnd().AsTime())
			}
		case *graphv1.EventEnvelope_RetractEdge:
			edges = append(edges, body.RetractEdge.GetType().String()+" -> "+feeder.RefString(body.RetractEdge.GetDst()))
		}
	}
	if len(nodes) != 1 || nodes[0] != "k8s.deployment=shop/checkout" {
		t.Errorf("retracted nodes = %v, want the workload", nodes)
	}
	want := map[string]bool{
		"RUNS_ON -> k8s.nodepool=shop-prod/general":        true,
		"OWNED_BY -> owner.team=team-checkout":             true,
		"DEPENDS_ON -> k8s.configmap=shop/checkout-config": true,
		"EXPOSED_VIA -> k8s.service=shop/checkout":         true,
	}
	if len(edges) != len(want) {
		t.Fatalf("retracted edges = %v, want %d", edges, len(want))
	}
	for _, edge := range edges {
		if !want[edge] {
			t.Errorf("unexpected retraction %q", edge)
		}
	}
}

// TestWorkloadKinds: every watched workload type mints refs in its own namespace, so a
// StatefulSet and a Deployment of the same name are two entities.
func TestWorkloadKinds(t *testing.T) {
	f := testFeeder(t)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cart", Namespace: "shop", ResourceVersion: "3001"},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(2)), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "redis:7"}}},
		}},
	}
	events := mapOne(t, f, k8sfeeder.KindStatefulSets, k8sfeeder.EventAdded, sts, fixtureStart)
	assertNodeRef(t, events, "k8s.statefulset=shop/cart")

	cron := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "shop", ResourceVersion: "4001"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "batch:2"}},
			}},
		}}},
	}
	events = mapOne(t, f, k8sfeeder.KindCronJobs, k8sfeeder.EventAdded, cron, fixtureStart)
	assertNodeRef(t, events, "k8s.cronjob=shop/nightly")
}

func assertNodeRef(t *testing.T, events []*graphv1.EventEnvelope, want string) {
	t.Helper()
	for _, ev := range events {
		if body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertNode); ok {
			if got := feeder.RefString(body.UpsertNode.GetRef()); got != want {
				t.Errorf("node ref = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatalf("no upsert_node event; wanted %s", want)
}

// TestOptionsRefuseAnUnusableConfiguration keeps a misconfiguration a startup error rather than
// a stream of rejections.
func TestOptionsRefuseAnUnusableConfiguration(t *testing.T) {
	if _, err := k8sfeeder.New(k8sfeeder.Options{ClusterName: "shop-prod"}, nil); err == nil {
		t.Error("a feeder with no SourceID was accepted")
	}
	if _, err := k8sfeeder.New(k8sfeeder.Options{SourceID: "k8s:demo"}, nil); err == nil {
		t.Error("a feeder with no ClusterName was accepted")
	}
}
