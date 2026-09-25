// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The watch (T055, FR-043, research §12).
//
// Shared informers over eleven resource types. An informer lists once, watches from the
// resourceVersion the list returned, and re-lists when the watch expires, which is exactly the
// shape the graph wants: the initial list is the extent a checkpoint reports, and every watch
// event after it is an incremental fact (constitution III — no truncate-and-reload, ever).
//
// Three decisions are worth their own sentence.
//
//   - **Pods are not watched.** They churn on every rollout and every node drain, and nothing
//     the graph answers is asked at pod granularity: "which pool does checkout run on" is
//     stable, "which node does pod checkout-7f9c-x2k4 run on" is noise with a half-life of
//     hours. The cost is that a workload's pool is read from what it declares (Mapper.NodePoolOf).
//   - **A Secret's values never enter a payload.** The informer replaces every value with a
//     digest of itself before the payload is built, drops `managedFields`, drops every
//     annotation carrying a manifest of the object — `kubectl apply` files the whole Secret,
//     `stringData` and all, under `last-applied-configuration` — and keeps only the metadata
//     the mappers read, so a recording made against a real cluster and committed to a
//     repository carries key names and versions and no material at all (spec edge case
//     "secrets", SC-009). ConfigMaps keep their content: it is not secret, and a recorded
//     fixture that cannot show what a configuration change changed is not much of a fixture.
//   - **`At` is receipt time.** Not the object's creationTimestamp, not a managedFields stamp:
//     those are valid times the mappers read off the object themselves. `At` says when *this
//     process* was told, which is what a recording replays in order and what the reordering
//     window is measured in.

// SyncPayloadSeq is the sequence number carried by the marker payload pushed once every
// informer has finished its initial list. It is zero: the marker is not an object and has no
// resourceVersion, and inventing one would put a fictional gap in the source's sequence.
const SyncPayloadSeq int64 = 0

// Pusher is where informers deliver payloads. *source.ChanSource is the implementation the
// live path uses; a test supplies its own.
type Pusher interface {
	// Push hands one payload on, blocking until it is taken or ctx is done.
	Push(ctx context.Context, p feeder.Payload) error
}

// InformerOptions configures the watch.
type InformerOptions struct {
	// Client is the Kubernetes client. A fake clientset works and is what the unit tests use.
	Client kubernetes.Interface
	// Namespaces restricts the watch. Empty watches every namespace.
	Namespaces []string
	// LabelSelector restricts the namespaced resources to objects carrying these labels. It
	// is not applied to Nodes or Namespaces, which the feeder reads for the cluster's shape
	// rather than for its workloads.
	LabelSelector string
	// Resync is how often the informers re-deliver their caches. Zero disables it: a resync
	// re-asserts facts that have not changed, which the graph answers DUPLICATE_NOOP, so it
	// costs bandwidth and buys nothing the watch does not already give.
	Resync time.Duration
	// Start is what the previous process of this source left behind (state.go). When it is
	// non-nil, Run pushes a `start` marker payload carrying it before the initial list, which
	// is what lets a reconnecting feeder notice its gap and retract what vanished during it —
	// and what puts the same information into a recording, so that `--replay` reconciles
	// exactly as the live run did (FR-044, FR-052).
	//
	// Nil pushes no marker: the feeder then behaves as it always has, listing and upserting,
	// which is what a corpus recorded before this existed replays as.
	Start *StartState
	// Now is the clock stamped on every payload. Nil means time.Now.
	Now func() time.Time
	// Log receives dropped-payload and decode warnings.
	Log *slog.Logger
	// QueueSize bounds the buffer between the informers and the feeder. Zero means
	// DefaultQueueSize.
	QueueSize int
}

// DefaultQueueSize is how many payloads may wait for the feeder before one is dropped. A drop
// is reported and recovered from by the next checkpoint's gap marker, never silently.
const DefaultQueueSize = 1024

// Informers is a running watch over one cluster.
type Informers struct {
	opts      InformerOptions
	now       func() time.Time
	log       *slog.Logger
	factories []informers.SharedInformerFactory

	// registrations are the handler registrations, one per informer. They are what Run waits
	// for before pushing the sync marker: a factory's WaitForCacheSync reports that the
	// *store* is filled, while the shared processor dispatches to handlers on its own
	// goroutines, so the marker could — and on every recording of 2026-09-16 did — overtake
	// part of the list it claims to close.
	registrations []cache.ResourceEventHandlerRegistration

	mu       sync.Mutex
	out      chan feeder.Payload
	buffered []feeder.Payload
}

// NewInformers builds the shared informer factories and registers the handlers. Nothing is
// started until Run.
//
// One factory is created per watched namespace, plus a cluster-scoped one for Nodes and
// Namespaces. That is how client-go expresses "watch these namespaces and nothing else"; a
// single cluster-wide factory filtered afterwards would read every object in the cluster,
// which is both slower and a wider credential than the feeder asks for.
func NewInformers(opts InformerOptions) (*Informers, error) {
	if opts.Client == nil {
		return nil, errors.New("k8s: InformerOptions.Client is required")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	in := &Informers{opts: opts, now: now, log: log}

	cluster := informers.NewSharedInformerFactoryWithOptions(opts.Client, opts.Resync)
	in.factories = append(in.factories, cluster)
	in.register(KindNamespaces, cluster.Core().V1().Namespaces().Informer())
	in.register(KindNodes, cluster.Core().V1().Nodes().Informer())

	namespaces := opts.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, namespace := range namespaces {
		factory := informers.NewSharedInformerFactoryWithOptions(opts.Client, opts.Resync,
			informers.WithNamespace(namespace),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) {
				o.LabelSelector = opts.LabelSelector
			}))
		in.factories = append(in.factories, factory)
		in.register(KindDeployments, factory.Apps().V1().Deployments().Informer())
		in.register(KindStatefulSets, factory.Apps().V1().StatefulSets().Informer())
		in.register(KindDaemonSets, factory.Apps().V1().DaemonSets().Informer())
		in.register(KindJobs, factory.Batch().V1().Jobs().Informer())
		in.register(KindCronJobs, factory.Batch().V1().CronJobs().Informer())
		in.register(KindServices, factory.Core().V1().Services().Informer())
		in.register(KindIngresses, factory.Networking().V1().Ingresses().Informer())
		in.register(KindConfigMaps, factory.Core().V1().ConfigMaps().Informer())
		in.register(KindSecrets, factory.Core().V1().Secrets().Informer())
	}
	return in, nil
}

// register wires one informer's handlers to the payload queue.
func (in *Informers) register(kind string, informer cache.SharedIndexInformer) {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { in.enqueue(kind, EventAdded, obj) },
		UpdateFunc: func(_, obj any) { in.enqueue(kind, EventModified, obj) },
		DeleteFunc: func(obj any) { in.enqueue(kind, EventDeleted, obj) },
	}
	registration, err := informer.AddEventHandler(handler)
	if err != nil {
		// AddEventHandler only fails on an informer that has already stopped, which cannot
		// happen here: nothing is started until Run.
		in.log.Error("k8s: could not register informer handler", "kind", kind, "error", err)
		return
	}
	in.registrations = append(in.registrations, registration)
}

// enqueue turns one watch event into a payload and hands it to the sink.
func (in *Informers) enqueue(kind, eventType string, obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		// The watch missed the deletion and the informer reconstructed it. The object inside
		// is the last state known, which is exactly what a retraction needs.
		obj = tombstone.Obj
	}
	payload, err := in.payloadOf(kind, eventType, obj)
	if err != nil {
		in.log.Warn("k8s: dropping a watch event that could not be encoded",
			"kind", kind, "type", eventType, "error", err)
		return
	}
	in.sink(payload)
}

// sink hands a payload to Run's loop. Before Run there is no loop, so payloads are buffered:
// an informer delivers its initial list the instant it is started, and dropping that would
// mean the feeder never hears about the objects that already existed.
func (in *Informers) sink(p feeder.Payload) {
	in.mu.Lock()
	out := in.out
	if out == nil {
		in.buffered = append(in.buffered, p)
		in.mu.Unlock()
		return
	}
	in.mu.Unlock()
	select {
	case out <- p:
	default:
		// The feeder is slower than the cluster. Blocking here would stall the informer's
		// shared queue for every other resource, so the payload is dropped and the loss is
		// reported: a checkpoint with gapBefore is the honest record of it (FR-032).
		in.log.Warn("k8s: payload queue full, dropping a watch event",
			"kind", p.Kind, "seq", p.Seq)
	}
}

// payloadOf renders one watch event as the fixture-format payload.
func (in *Informers) payloadOf(kind, eventType string, obj any) (feeder.Payload, error) {
	object, ok := obj.(runtime.Object)
	if !ok {
		return feeder.Payload{}, fmt.Errorf("watch event carried %T, not a Kubernetes object", obj)
	}
	if secret, isSecret := object.(*corev1.Secret); isSecret {
		object = RedactSecret(secret)
	} else {
		object = SanitizeMetadata(object)
	}
	accessor, err := meta(object)
	if err != nil {
		return feeder.Payload{}, err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return feeder.Payload{}, err
	}
	bytes, err := json.Marshal(WatchEvent{Type: eventType, Object: encoded})
	if err != nil {
		return feeder.Payload{}, err
	}
	return feeder.Payload{
		Kind:  kind,
		At:    in.now().UTC(),
		Seq:   seqOf(accessor.GetResourceVersion()),
		Bytes: bytes,
	}, nil
}

// meta reads the object metadata every watched resource has.
func meta(object runtime.Object) (metav1.Object, error) {
	accessor, ok := object.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("object of type %T has no metadata", object)
	}
	return accessor, nil
}

// LastAppliedAnnotation is the annotation `kubectl apply` writes the whole submitted manifest
// into, verbatim. On a Secret that manifest contains `stringData` in cleartext, which is why
// digesting `.data` alone left the material on disk (live run 2026-09-16, finding 5).
const LastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// RedactSecret returns a copy of a Secret carrying no material anywhere.
//
// The key names survive, because knowing that `payments-secret` holds `provider_token` is
// topology and knowing its value is a breach; the digest survives because a change to it is
// what makes a secret rotation observable. What never survives is the material — not in a
// payload, not in a recording committed to a repository, not in an event (constitution IV,
// spec edge case "secrets"). The CONFIG node the mapper builds from this carries neither: only
// the resourceVersion (Mapper.ConfigEvents).
//
// Three things beyond `.data` and `.stringData` are removed, and the live run of 2026-09-16 is
// why the list is this long: digesting the values is not enough on a cluster anybody has ever
// run `kubectl apply` against.
//
//   - Every annotation carrying a JSON manifest of the object, starting with
//     `kubectl.kubernetes.io/last-applied-configuration`. That one annotation held the whole
//     Secret, `stringData` included, in cleartext (scrubAnnotations).
//   - `managedFields` entirely. Nothing reads a Secret's field managers — ConfigChangeEvent
//     passes none — and each entry enumerates the object's own field paths.
//   - Every other metadata field. What is left is what the mappers read: name, namespace, uid,
//     resourceVersion, labels, the surviving annotations, creationTimestamp and
//     ownerReferences. A recording is a public artifact; anything in it that nothing reads is
//     a liability rather than an omission.
func RedactSecret(s *corev1.Secret) *corev1.Secret {
	out := s.DeepCopy()
	out.StringData = nil
	out.ObjectMeta = metav1.ObjectMeta{
		Name:              s.Name,
		Namespace:         s.Namespace,
		UID:               s.UID,
		ResourceVersion:   s.ResourceVersion,
		CreationTimestamp: s.CreationTimestamp,
		Labels:            s.Labels,
		Annotations:       scrubAnnotations(s.Annotations),
		OwnerReferences:   s.OwnerReferences,
	}
	if len(out.Data) == 0 {
		return out
	}
	keys := make([]string, 0, len(out.Data))
	for k := range out.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		digest := sha256.Sum256(out.Data[k])
		out.Data[k] = []byte("sha256:" + hex.EncodeToString(digest[:]))
	}
	return out
}

// SanitizeMetadata returns a copy of any watched object with the two metadata fields a
// recording must not carry removed: manifest-bearing annotations, and the bulk of
// `managedFields`.
//
// A ConfigMap's `last-applied-configuration` is not secret — its content is on the CONFIG node
// as a digest and in the payload as itself — but it is a second, stale copy of the object
// inside the object, and on any other kind it is pure noise. It goes for every kind, so that
// "the annotation is never in a payload" is a property of the recorder rather than a property
// of which resource you happened to record.
//
// `managedFields` keeps its manager, operation, apiVersion and time, and loses `fieldsV1`.
// That is the whole of its size and the only part that enumerates the object's own contents;
// the four fields that stay are exactly what map_changes.go reads to attribute a rollout or a
// scaling to the field manager that performed it (FR-043 `Change.actor`). Dropping the array
// outright would make every future recording silently unattributed.
func SanitizeMetadata(object runtime.Object) runtime.Object {
	accessor, err := meta(object)
	if err != nil {
		// Not a metadata-carrying object; nothing to sanitize, and payloadOf reports it.
		return object
	}
	annotations := scrubAnnotations(accessor.GetAnnotations())
	managed := trimManagedFields(accessor.GetManagedFields())
	if len(annotations) == len(accessor.GetAnnotations()) && managed == nil {
		return object
	}
	out := object.DeepCopyObject()
	copied, err := meta(out)
	if err != nil {
		return object
	}
	copied.SetAnnotations(annotations)
	if managed != nil {
		copied.SetManagedFields(managed)
	}
	return out
}

// scrubAnnotations drops every annotation whose value is a JSON object describing the resource
// itself — `kubectl.kubernetes.io/last-applied-configuration` by name, and anything else
// holding a `data` or `stringData` key, which is what a Secret's or a ConfigMap's manifest
// looks like whatever key an operator's tooling filed it under.
//
// It returns nil for an object that had none, so that a sanitized payload of an object with no
// annotations is byte-identical to the original.
func scrubAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return annotations
	}
	out := make(map[string]string, len(annotations))
	for k, v := range annotations {
		if k == LastAppliedAnnotation || carriesResourceData(v) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// carriesResourceData reports whether an annotation value is a JSON object with a `data` or
// `stringData` member, which is the shape of a serialized ConfigMap or Secret.
func carriesResourceData(value string) bool {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return false
	}
	_, data := fields["data"]
	_, stringData := fields["stringData"]
	return data || stringData
}

// trimManagedFields removes the `fieldsV1` half of every managed-fields entry, returning nil
// when there was nothing to trim.
func trimManagedFields(entries []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
	trim := false
	for i := range entries {
		if entries[i].FieldsV1 != nil || entries[i].FieldsType != "" || entries[i].Subresource != "" {
			trim = true
			break
		}
	}
	if !trim {
		return nil
	}
	out := make([]metav1.ManagedFieldsEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, metav1.ManagedFieldsEntry{
			Manager:    entry.Manager,
			Operation:  entry.Operation,
			APIVersion: entry.APIVersion,
			Time:       entry.Time,
		})
	}
	return out
}

// Run starts every informer, waits for the initial lists, pushes a `sync` marker, and then
// forwards watch events until ctx is done.
//
// The marker is what makes the feeder emit its first checkpoint: it is the moment the graph
// can be told "everything that existed at this instant has been reported", which is the
// difference between "nothing happened" and "nobody was watching" (FR-032).
func (in *Informers) Run(ctx context.Context, dst Pusher) error {
	in.mu.Lock()
	queueSize := in.opts.QueueSize
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	in.out = make(chan feeder.Payload, queueSize)
	buffered := in.buffered
	in.buffered = nil
	out := in.out
	in.mu.Unlock()

	// The `start` marker goes first, before a single object: it is the instant this process
	// began watching, which is where its coverage resumes and where a gap ends.
	if in.opts.Start != nil {
		marker, err := StartPayload(in.now().UTC(), *in.opts.Start)
		if err != nil {
			return err
		}
		if err := dst.Push(ctx, marker); err != nil {
			return err
		}
	}

	stop := make(chan struct{})
	defer close(stop)
	for _, factory := range in.factories {
		factory.Start(stop)
	}

	// Forward what the handlers produced while the queue did not exist yet — the initial
	// lists, which are most of what a fresh feeder has to say.
	for _, p := range buffered {
		if err := dst.Push(ctx, p); err != nil {
			return err
		}
	}

	synced := make(chan struct{})
	go func() {
		defer close(synced)
		for _, factory := range in.factories {
			factory.WaitForCacheSync(stop)
		}
		// And then for the handlers: a registration's HasSynced reports that *this handler*
		// has been given the whole initial list, which is what the sync marker is supposed to
		// mean. Without it the marker races the list it closes, and every fact the list
		// asserts after it looks like a change (Feeder.ListSettle).
		waiters := make([]cache.InformerSynced, 0, len(in.registrations))
		for _, registration := range in.registrations {
			waiters = append(waiters, registration.HasSynced)
		}
		cache.WaitForCacheSync(stop, waiters...)
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-synced:
			synced = nil
			// Drain whatever the initial lists produced before announcing the sync, so that
			// the checkpoint really does cover everything already reported.
			if err := drain(ctx, out, dst); err != nil {
				return err
			}
			if err := dst.Push(ctx, feeder.Payload{Kind: KindSync, At: in.now().UTC(), Seq: SyncPayloadSeq}); err != nil {
				return err
			}
		case p := <-out:
			if err := dst.Push(ctx, p); err != nil {
				return err
			}
		}
	}
}

// drain forwards everything currently queued without blocking.
func drain(ctx context.Context, out <-chan feeder.Payload, dst Pusher) error {
	for {
		select {
		case p := <-out:
			if err := dst.Push(ctx, p); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}
