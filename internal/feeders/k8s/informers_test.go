// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The watch, against a fake API server (T055).
//
// Docker is not available here, so there is no cluster; client-go's fake clientset is the
// standard substitute and drives the same informer machinery a real one does — list, watch,
// add, update, delete — which is what these tests are about. What it does not exercise is
// resourceVersion allocation, so the tests set them explicitly, exactly as the API server
// would.

// collector is a Pusher that keeps everything the informers deliver.
type collector struct {
	mu       sync.Mutex
	payloads []feeder.Payload
}

func (c *collector) Push(_ context.Context, p feeder.Payload) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, p)
	return nil
}

func (c *collector) snapshot() []feeder.Payload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]feeder.Payload(nil), c.payloads...)
}

// find returns the first payload of a kind whose watch event type matches, or false.
func (c *collector) find(kind, eventType string) (feeder.Payload, k8sfeeder.WatchEvent, bool) {
	for _, p := range c.snapshot() {
		if p.Kind != kind {
			continue
		}
		var event k8sfeeder.WatchEvent
		// The sync marker carries no object, so it carries no bytes to decode.
		if len(p.Bytes) > 0 {
			if err := json.Unmarshal(p.Bytes, &event); err != nil {
				continue
			}
		}
		if event.Type == eventType {
			return p, event, true
		}
	}
	return feeder.Payload{}, k8sfeeder.WatchEvent{}, false
}

// waitFor polls until cond holds or the test's deadline passes. Informers are asynchronous by
// construction; polling is what client-go's own tests do.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startInformers runs the watch against a fake clientset and returns what it delivers.
func startInformers(t *testing.T, objects ...runtime.Object) (*fake.Clientset, *collector) {
	t.Helper()
	client := fake.NewClientset(objects...)

	watch, err := k8sfeeder.NewInformers(k8sfeeder.InformerOptions{Client: client})
	if err != nil {
		t.Fatalf("new informers: %v", err)
	}
	sink := &collector{}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		if err := watch.Run(ctx, sink); err != nil && ctx.Err() == nil {
			t.Errorf("watch: %v", err)
		}
	}()
	waitFor(t, "the initial list to complete", func() bool {
		_, _, ok := sink.find(k8sfeeder.KindSync, "")
		return ok
	})
	return client, sink
}

// TestInformersDeliverTheInitialList checks the shape of a payload: its kind is the resource's
// plural name, its sequence is the object's resourceVersion, and its bytes are a watch event.
func TestInformersDeliverTheInitialList(t *testing.T) {
	_, sink := startInformers(t, checkoutDeployment())

	payload, event, ok := sink.find(k8sfeeder.KindDeployments, k8sfeeder.EventAdded)
	if !ok {
		t.Fatal("no ADDED payload for the deployment in the initial list")
	}
	if payload.Seq != 1001 {
		t.Errorf("Seq = %d, want the resourceVersion 1001", payload.Seq)
	}
	if payload.At.IsZero() {
		t.Error("At is zero; a payload records when this process was told")
	}
	var deployment appsv1.Deployment
	if err := json.Unmarshal(event.Object, &deployment); err != nil {
		t.Fatalf("decode the object: %v", err)
	}
	if deployment.Name != "checkout" || deployment.Namespace != "shop" {
		t.Errorf("object = %s/%s, want shop/checkout", deployment.Namespace, deployment.Name)
	}

	sync, _, ok := sink.find(k8sfeeder.KindSync, "")
	if !ok {
		t.Fatal("no sync marker after the initial list")
	}
	if sync.Seq != k8sfeeder.SyncPayloadSeq {
		t.Errorf("sync Seq = %d, want %d: the marker is not an object and has no resourceVersion",
			sync.Seq, k8sfeeder.SyncPayloadSeq)
	}
}

// TestInformersDeliverUpdatesAndDeletes is the other half of the watch contract.
func TestInformersDeliverUpdatesAndDeletes(t *testing.T) {
	client, sink := startInformers(t, checkoutDeployment())

	updated := checkoutDeployment()
	updated.ResourceVersion = "1007"
	updated.Spec.Replicas = ptr(int32(5))
	if _, err := client.AppsV1().Deployments("shop").Update(t.Context(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	waitFor(t, "the MODIFIED payload", func() bool {
		payload, _, ok := sink.find(k8sfeeder.KindDeployments, k8sfeeder.EventModified)
		return ok && payload.Seq == 1007
	})

	if err := client.AppsV1().Deployments("shop").Delete(t.Context(), "checkout", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "the DELETED payload", func() bool {
		_, _, ok := sink.find(k8sfeeder.KindDeployments, k8sfeeder.EventDeleted)
		return ok
	})
}

// TestInformersRedactSecretsBeforeTheyBecomePayloads is SC-009 at the watch boundary: the
// material never reaches a payload, so it can never reach a recording (spec edge case
// "secrets").
func TestInformersRedactSecretsBeforeTheyBecomePayloads(t *testing.T) {
	const material = "super-secret-value"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-secret", Namespace: "shop", ResourceVersion: "1202"},
		Data:       map[string][]byte{"token": []byte(material)},
	}
	_, sink := startInformers(t, secret)

	payload, event, ok := sink.find(k8sfeeder.KindSecrets, k8sfeeder.EventAdded)
	if !ok {
		t.Fatal("no ADDED payload for the secret")
	}
	if strings.Contains(string(payload.Bytes), material) {
		t.Fatal("the payload contains the secret's value")
	}
	var decoded corev1.Secret
	if err := json.Unmarshal(event.Object, &decoded); err != nil {
		t.Fatalf("decode the object: %v", err)
	}
	// The key name survives, because which secret a workload reads is topology.
	value, present := decoded.Data["token"]
	if !present {
		t.Fatal("the key name was dropped; a redacted Secret keeps its shape")
	}
	if !strings.HasPrefix(string(value), "sha256:") {
		t.Errorf("value = %q, want a sha256 digest", value)
	}
}

// TestInformersRedactTheAppliedManifest is finding 5 of the live run of 2026-09-16 as a test.
//
// Digesting `.data` is not enough: `kubectl apply` files the whole submitted manifest, with
// `stringData` in cleartext, under an annotation, and the old RedactSecret copied metadata
// untouched. Every Secret on every cluster anybody has ever `kubectl apply`-ed therefore
// carried its own value into `--record`.
func TestInformersRedactTheAppliedManifest(t *testing.T) {
	const material = "not-a-real-token-disposable-demo-cluster"
	manifest := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"payments-secret",` +
		`"namespace":"shop"},"stringData":{"token":"` + material + `"}}`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "payments-secret", Namespace: "shop", ResourceVersion: "573",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": manifest,
				"sre.overlay": "minimal",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate,
				APIVersion: "v1", FieldsType: "FieldsV1",
			}},
			Finalizers: []string{"example.com/keep"},
		},
		Data: map[string][]byte{"token": []byte(material)},
	}
	_, sink := startInformers(t, secret)

	payload, event, ok := sink.find(k8sfeeder.KindSecrets, k8sfeeder.EventAdded)
	if !ok {
		t.Fatal("no ADDED payload for the secret")
	}
	if strings.Contains(string(payload.Bytes), material) {
		t.Fatal("the payload contains the secret's value, in the applied-manifest annotation")
	}
	if strings.Contains(string(payload.Bytes), k8sfeeder.LastAppliedAnnotation) {
		t.Error("the payload still names the applied-manifest annotation")
	}
	var decoded corev1.Secret
	if err := json.Unmarshal(event.Object, &decoded); err != nil {
		t.Fatalf("decode the object: %v", err)
	}
	if decoded.Annotations["sre.overlay"] != "minimal" {
		t.Error("an unrelated annotation was dropped; only manifest-bearing ones go")
	}
	if len(decoded.ManagedFields) != 0 {
		t.Error("a redacted Secret keeps no managedFields")
	}
	if len(decoded.Finalizers) != 0 {
		t.Error("a redacted Secret keeps only the metadata the mappers read")
	}
	if decoded.ResourceVersion != "573" {
		t.Errorf("resourceVersion = %q, want 573: it is what the CONFIG node carries", decoded.ResourceVersion)
	}
}

// TestInformersTrimEveryKindsMetadata checks the other half of the sanitizer: a ConfigMap keeps
// its content and its field managers, and loses the applied-manifest annotation and the
// `fieldsV1` bulk that nothing reads.
func TestInformersTrimEveryKindsMetadata(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-config", Namespace: "shop", ResourceVersion: "572",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"kind":"ConfigMap","data":{"A":"1"}}`,
				"kubernetes.io/change-cause":                       "helm upgrade shop",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubectl-patch", Operation: metav1.ManagedFieldsOperationUpdate,
				APIVersion: "v1", FieldsType: "FieldsV1",
				FieldsV1: metav1.NewFieldsV1(`{"f:data":{"f:A":{}}}`),
			}},
		},
		Data: map[string]string{"A": "1"},
	}
	_, sink := startInformers(t, cm)

	payload, event, ok := sink.find(k8sfeeder.KindConfigMaps, k8sfeeder.EventAdded)
	if !ok {
		t.Fatal("no ADDED payload for the configmap")
	}
	if strings.Contains(string(payload.Bytes), k8sfeeder.LastAppliedAnnotation) {
		t.Error("the payload still carries the applied-manifest annotation")
	}
	if strings.Contains(string(payload.Bytes), "fieldsV1") {
		t.Error("the payload still carries fieldsV1, which is all of managedFields' size")
	}
	var decoded corev1.ConfigMap
	if err := json.Unmarshal(event.Object, &decoded); err != nil {
		t.Fatalf("decode the object: %v", err)
	}
	if decoded.Data["A"] != "1" {
		t.Error("a ConfigMap's content is not secret and must survive")
	}
	if decoded.Annotations["kubernetes.io/change-cause"] != "helm upgrade shop" {
		t.Error("the change cause was dropped; it is what attributes a rollout")
	}
	if len(decoded.ManagedFields) != 1 || decoded.ManagedFields[0].Manager != "kubectl-patch" {
		t.Errorf("managedFields = %+v, want the manager kept: map_changes.go reads it", decoded.ManagedFields)
	}
}

// TestInformersRefuseAMissingClient keeps the constructor's contract.
func TestInformersRefuseAMissingClient(t *testing.T) {
	if _, err := k8sfeeder.NewInformers(k8sfeeder.InformerOptions{}); err == nil {
		t.Fatal("NewInformers accepted a nil client")
	}
}
