// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The Kubernetes feeder (T060, FR-043, FR-044, FR-046).
//
// Run is the whole connector: it reads raw watch payloads from a Source it cannot inspect —
// informers when it is live, a recording when it is under test — decodes each one into a typed
// object, and hands it to the mappers. Because the two modes share this one code path, a
// recorded fixture is not an approximation of the live feeder, it *is* the live feeder
// (FR-044).
//
// The feeder keeps one kind of memory: the latest version of every object it has been told
// about. Four facts are derived from it that a single payload cannot state — a change, which is
// the difference between two versions (map_changes.go); an `exposed-via` edge, which relates a
// Service's selector to a workload's labels; how many machines a node pool has; and the
// environment a Namespace declares for everything inside it. None of it is a cache of the
// graph: a feeder never reads the graph (constitution I).

// WatchEvent is one payload as the informers write it and a recording stores it: the watch
// event type and the object it concerns. It is the fixture format of every `k8s` payload.
type WatchEvent struct {
	// Type is ADDED, MODIFIED or DELETED.
	Type string `json:"type"`
	// Object is the Kubernetes object, as the API server rendered it.
	Object json.RawMessage `json:"object"`
}

// PermissionChecker proves a credential is read-only before the feeder emits anything
// (FR-046). permissions.go implements it over SelfSubjectRulesReview; Run skips the check only
// when there is no credential to check, which is the case when replaying a recording.
type PermissionChecker interface {
	// CheckReadOnly returns a non-nil error naming the write verbs the credential holds.
	CheckReadOnly(ctx context.Context) error
}

// Feeder watches one Kubernetes cluster.
type Feeder struct {
	opts   Options
	mapper *Mapper
	log    *slog.Logger

	// Permissions is asked whether the credential is read-only before anything is emitted.
	// Nil means there is nothing to ask — a replay has no cluster and no credential.
	Permissions PermissionChecker

	// State is where the feeder writes what it has asserted, after every checkpoint, so that
	// its successor can tell what disappeared while it was down (state.go). Nil means
	// nowhere, which is what a replay and a test want: the information they need is in the
	// `start` marker payload, not on disk.
	State StateStore

	mu    sync.Mutex
	state *state
}

var _ feeder.Feeder = (*Feeder)(nil)

// state is everything Run remembers between payloads.
//
// The second half is the *session*: one watch, from the `start` marker that opens it to the
// `sync` marker that closes its initial list. A feeder that is restarted has a new session; so
// does a replay that concatenates two processes' payloads, which is why feeder-gap-01 replays
// as the run it records rather than as one long watch.
type state struct {
	workloads map[string]workloadView
	services  map[string]serviceView
	ingresses map[string]ingressView
	configs   map[string]configView
	nodes     map[string]nodeView
	namespace map[string]string

	// listing is true while an initial list is being delivered. Nothing is emitted then; see
	// Feeder.topologyEvents for why.
	listing bool
	// sessionFrom is when this watch began: the `start` marker's instant, or the `sync`'s
	// when there is no marker. It is the extent every checkpoint of the session reports as
	// its lower bound, and the instant a gap ends.
	sessionFrom time.Time
	// started records that a `start` marker opened this session. Only then can the feeder
	// diff the new list against what it previously asserted: without a marker it cannot tell
	// "this list is a reconnection" from "these payloads are a continuing watch".
	started bool
	// syncAt is the instant the current session's initial list completed. Everything observed
	// at or before it is initial state, whose valid start is unknown (Feeder.validFor).
	syncAt time.Time
	// lastCheckpointTo is the extent the last checkpoint this Run emitted reached.
	lastCheckpointTo time.Time
	// prevCheckpoint is the coverage claimed before this session's list — from the previous
	// checkpoint of this Run, or from the `start` marker when the previous one was a previous
	// process. A gap is the hole between it and sessionFrom.
	prevCheckpoint time.Time
	// known is what had been asserted when this session's list began, by object key.
	known map[string]KnownObject
	// listed is what this session's list contains, by object key.
	listed map[string]bool
}

func newState() *state {
	return &state{
		workloads: map[string]workloadView{},
		services:  map[string]serviceView{},
		ingresses: map[string]ingressView{},
		configs:   map[string]configView{},
		nodes:     map[string]nodeView{},
		namespace: map[string]string{},
		known:     map[string]KnownObject{},
		listed:    map[string]bool{},
	}
}

// New returns a feeder for one cluster. It fails on an Options that could not produce a
// well-formed event, so that a misconfiguration is a startup error rather than a stream of
// rejections.
func New(opts Options, log *slog.Logger) (*Feeder, error) {
	normalized, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	f := &Feeder{opts: normalized, log: log, state: newState()}
	f.mapper = NewMapper(f.Describe(), normalized)
	return f, nil
}

// Options returns the normalized configuration the feeder runs with.
func (f *Feeder) Options() Options { return f.opts }

// Describe is the feeder's contract with the graph and the conformance harness.
//
// `per_source_sequence`: every event carries the object's resourceVersion as its sequence, so a
// gap in the stream is detectable. The 60-second reordering window is what a shared informer
// can actually deliver out of order under a relist, and it is taken literally by
// testkit.Shuffle, which permutes payloads inside it and demands the same events back.
func (f *Feeder) Describe() feeder.Description {
	return feeder.Description{
		SourceID:         f.opts.SourceID,
		Kind:             "k8s",
		Ordering:         feeder.OrderingPerSourceSequence,
		ReorderingWindow: f.opts.ReorderingWindow,
		RequiredScopes:   RequiredScopes(),
		Namespaces: []string{
			feeder.NSK8sCluster,
			feeder.NSK8sNodePool,
			feeder.NSK8sDeployment,
			feeder.NSK8sStatefulSet,
			feeder.NSK8sDaemonSet,
			feeder.NSK8sJob,
			feeder.NSK8sCronJob,
			feeder.NSK8sService,
			feeder.NSK8sIngress,
			feeder.NSK8sConfigMap,
			feeder.NSK8sSecret,
			feeder.NSK8sChange,
			feeder.NSOwnerTeam,
			feeder.NSAppName,
			feeder.NSOTelService,
		},
	}
}

// Run consumes payloads until the source is exhausted or ctx is done.
//
// The extent a checkpoint reports is computed from every payload of the run, never from "the
// last one I happened to see": the latter depends on delivery order, and testkit.Shuffle fails
// a feeder for it (SDK guide §6).
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		return err
	}
	// FR-046: prove the credential cannot write before emitting anything. A replay has no
	// credential, and saying so is honest; refusing would make the recorded mode untestable.
	if f.Permissions != nil {
		if err := f.Permissions.CheckReadOnly(ctx); err != nil {
			return err
		}
	}

	// The initial list is a set, not a stream: every ADDED the informers deliver before the
	// sync marker describes something that already existed, and none of it is a change. The
	// feeder therefore accumulates it and emits the whole topology at the sync, grouped by
	// kind and sorted inside each group (topologyEvents). Three things fall out of that:
	// `exposed-via` can relate a Service and a workload without caring which arrived first, a
	// node pool can say how many machines it has, and the emitted stream is byte-identical
	// however the watch delivered the list — which is what testkit.Shuffle checks (FR-048).
	f.beginRun()

	var first, last time.Time
	for {
		payload, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return err
		}

		if !payload.At.IsZero() {
			if first.IsZero() || payload.At.Before(first) {
				first = payload.At
			}
			if payload.At.After(last) {
				last = payload.At
			}
		}

		switch payload.Kind {
		case KindStart:
			if err := f.beginSession(payload); err != nil {
				return err
			}
			continue
		case KindSync:
			if err := f.closeList(ctx, em, payload); err != nil {
				return err
			}
			continue
		}
		if err := f.handle(ctx, em, payload); err != nil {
			return err
		}
		// A long-running watch checkpoints on a timer so that a quiet hour is recorded as
		// "observed and nothing happened" rather than as a hole (FR-032).
		if f.dueForCheckpoint(last) {
			if err := f.checkpointTo(ctx, em, last, 0, false, ""); err != nil {
				return err
			}
		}
	}

	// A recording that ends without a sync marker — a corpus assembled by hand, a watch that
	// was cut short — still has to report what it listed. Its list boundary is the last
	// instant it saw, which is a function of the payload set and not of its order.
	if err := f.closeListAt(ctx, em, last, 0, ""); err != nil {
		return err
	}
	if !first.IsZero() && !last.Equal(f.checkpointed()) {
		if err := f.checkpointTo(ctx, em, last, 0, false, ""); err != nil {
			return err
		}
	}
	if err := f.saveState(); err != nil {
		return err
	}
	return em.Flush(ctx)
}

// beginRun resets everything one Run remembers, so that replaying a recording twice through
// one Feeder produces the same events both times. The memory that legitimately crosses a run
// is the state file, and it reaches the feeder as a `start` marker payload, never as a field
// that survived the last call.
func (f *Feeder) beginRun() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = newState()
	f.state.listing = true
}

// beginSession opens a watch session from a `start` marker: the instant the feeder began
// looking, what the previous process had asserted, and how far its coverage reached (state.go).
//
// A marker that arrives after the sync it was supposed to precede is dropped. The two markers
// are the watch's own lifecycle, not object payloads racing each other, and a source that
// delivers them out of order has told the feeder about a session it has already closed. The
// consequence is one missed reconciliation, reported in the log, rather than a list flushed
// against the wrong session.
func (f *Feeder) beginSession(payload feeder.Payload) error {
	var start StartState
	if len(payload.Bytes) > 0 {
		if err := json.Unmarshal(payload.Bytes, &start); err != nil {
			return fmt.Errorf("k8s: decode the start marker: %w", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.state.syncAt.IsZero() && !payload.At.After(f.state.syncAt) {
		f.log.Warn("k8s: ignoring a start marker that arrived after the sync it precedes",
			"start", payload.At, "sync", f.state.syncAt)
		return nil
	}

	// What the feeder already holds is what it asserted in this process; the marker adds what
	// a previous process asserted and this one has not heard of. Both are "known before this
	// list", which is what the list is diffed against.
	known := f.currentObjects()
	for _, o := range start.KnownObjects {
		if _, held := known[o.objectKey()]; !held {
			known[o.objectKey()] = o
		}
	}
	prev := f.state.lastCheckpointTo
	if prev.IsZero() && start.PreviousCheckpoint != nil {
		prev = start.PreviousCheckpoint.UTC()
	}

	f.state.listing = true
	f.state.started = true
	f.state.sessionFrom = payload.At.UTC()
	f.state.syncAt = time.Time{}
	f.state.prevCheckpoint = prev
	f.state.known = known
	f.state.listed = map[string]bool{}
	return nil
}

// closeList handles a `sync` marker: the initial list is complete.
func (f *Feeder) closeList(ctx context.Context, em feeder.Emitter, payload feeder.Payload) error {
	return f.closeListAt(ctx, em, payload.At, payload.Seq, f.syncNote())
}

// closeListAt emits the listed topology, retracts what disappeared while the feeder was away,
// and records the extent with a gap marker when there was one.
//
// The order is deliberate: assert, then retract, then checkpoint. A retraction of something
// this list did contain would be wrong, and a checkpoint claiming coverage the events have not
// been emitted for would be a lie if the process died between the two.
func (f *Feeder) closeListAt(ctx context.Context, em feeder.Emitter, at time.Time, seq int64, note string) error {
	// One span per list/sync batch (plan.md §Observability, FR-051). It covers the whole batch
	// — assert, retract, checkpoint — because that is the unit that either lands or does not.
	ctx, span := telemetry.StartFeederSync(ctx, f.opts.SourceID, note)
	defer span.End()

	f.mu.Lock()
	if !f.state.listing {
		f.mu.Unlock()
		return nil
	}
	f.state.listing = false
	f.state.syncAt = at.UTC()
	if f.state.sessionFrom.IsZero() {
		// No `start` marker: the session's coverage is dated from the instant its list
		// completed. That is later than the instant the watch really began, and it is the only
		// bound that does not depend on which payload arrived first — the earliest arrival is
		// a property of the permutation, not of the watch (live run 2026-09-16, finding 6).
		f.state.sessionFrom = f.state.syncAt
	}
	events, err := f.topologyEvents()
	if err == nil {
		events = append(events, f.reconcile()...)
	}
	gap, gapNote := f.gapBefore()
	from, to := f.state.sessionFrom, f.state.syncAt
	f.mu.Unlock()
	if err != nil {
		return err
	}

	span.SetAttributes(attribute.Int(telemetry.SpanAttrEventCount, len(events)))
	for _, ev := range events {
		if err := f.emit(ctx, em, ev); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "sync batch")
			return err
		}
	}
	if gap && note != "" {
		note += "; " + gapNote
	} else if gap {
		note = gapNote
	}
	if err := f.checkpoint(ctx, em, from, to, seq, gap, note); err != nil {
		return err
	}
	return f.saveState()
}

// gapBefore reports whether this session began after a hole in the source's coverage, and how
// to describe it. The caller holds the lock.
//
// The threshold is what separates "the watch reconnected immediately" from "nobody was
// watching": a feeder that restarts inside its own checkpoint interval has not missed a
// checkpoint, so claiming a gap would put a permanent scar in `extent` for every routine
// restart. Two intervals is the default, and Options.GapThreshold is the knob.
func (f *Feeder) gapBefore() (bool, string) {
	prev := f.state.prevCheckpoint
	if prev.IsZero() || f.state.sessionFrom.IsZero() {
		// This source has never checkpointed: a first run is not a gap, its extent simply
		// starts where the watch did (FR-032).
		return false, ""
	}
	blind := f.state.sessionFrom.Sub(prev)
	if blind <= f.opts.GapThreshold {
		return false, ""
	}
	return true, fmt.Sprintf("feeder restarted after a %s gap; nothing was watched between %s and %s",
		blind.Round(time.Second), prev.UTC().Format(time.RFC3339Nano), f.state.sessionFrom.UTC().Format(time.RFC3339Nano))
}

// reconcile retracts everything the feeder had asserted that this session's list does not
// contain (T067, FR-052, spec edge case "feeder gap"). The caller holds the lock.
//
// **The valid end is the instant the feeder resumed looking**, which is this session's
// `extent_from` and the upper bound of the gap beside it. The object stopped existing
// somewhere inside `[previous checkpoint, sessionFrom)` and nothing can say where: the last
// positive evidence is the previous checkpoint and the first negative evidence is this list.
// `RetractNode` carries a `valid_end` and no `end_unknown` — `Interval` has one, the event does
// not, and the schema is frozen for this phase (constitution IX) — so the feeder asserts the
// one boundary it can prove rather than guessing an instant inside the hole (FR-011). Dating
// it from the *previous* checkpoint instead would be worse in both directions: it claims the
// object vanished the moment the feeder stopped watching, and in a recording whose last
// checkpoint precedes the objects' own unknown starts it retracts an interval that has not
// begun, which the projector correctly ignores. The `gap_before` checkpoint emitted alongside
// is what tells a reader the true end lies in the hole (FR-052).
//
// Only a session opened by a `start` marker reconciles. Without one the feeder cannot tell a
// reconnection's list from a watch that has simply been running, and retracting on that guess
// would empty the graph every time a corpus was replayed without markers.
func (f *Feeder) reconcile() []*graphv1.EventEnvelope {
	if !f.state.started || len(f.state.known) == 0 {
		return nil
	}
	missing := make([]KnownObject, 0, len(f.state.known))
	for key, object := range f.state.known {
		if !f.state.listed[key] {
			missing = append(missing, object)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	at := f.state.sessionFrom
	var events []*graphv1.EventEnvelope
	for _, o := range sortObjects(missing) {
		kind, known := kindOfPayload(o.Kind)
		if !known {
			f.log.Warn("k8s: the state file names an object of a kind this feeder does not watch",
				"kind", o.Kind, "name", key(o.Namespace, o.Name))
			continue
		}
		// The edges the object drove are closed by the projector as a consequence of the
		// node's retraction, at the same valid instant and recording why (edge case
		// "retracting a node with live edges"). The feeder does not re-derive them: after a
		// restart it no longer knows the Service's selector, and asserting a relationship it
		// cannot see would be a guess.
		events = append(events, f.mapper.RetractNode(kind, o.Namespace, o.Name, o.ResourceVersion, at))
		f.forget(o)
	}
	return events
}

// forget drops a retracted object from the indexes, so a later checkpoint does not write it
// back into the state file.
func (f *Feeder) forget(o KnownObject) {
	switch o.Kind {
	case KindServices:
		delete(f.state.services, key(o.Namespace, o.Name))
	case KindIngresses:
		delete(f.state.ingresses, key(o.Namespace, o.Name))
	case KindConfigMaps, KindSecrets:
		delete(f.state.configs, o.Kind+"|"+key(o.Namespace, o.Name))
	default:
		delete(f.state.workloads, o.Kind+"|"+key(o.Namespace, o.Name))
	}
}

// dueForCheckpoint reports whether the extent seen so far is a checkpoint interval past the
// last one recorded.
//
// Never while a list is in progress. A feeder reconnecting after an hour is a checkpoint
// interval past its last one on the very first payload of its new list, and a checkpoint there
// would claim coverage the list has not been emitted for — and, because a checkpoint's id is
// its extent to the second, would collide with the one the sync is about to mint and swallow
// its `gap_before`.
func (f *Feeder) dueForCheckpoint(last time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.state.listing && !f.state.lastCheckpointTo.IsZero() &&
		last.Sub(f.state.lastCheckpointTo) >= f.opts.CheckpointInterval
}

// checkpointed is the extent the last checkpoint reached.
func (f *Feeder) checkpointed() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.lastCheckpointTo
}

// checkpointTo records the extent from the session's start to `to`.
func (f *Feeder) checkpointTo(ctx context.Context, em feeder.Emitter, to time.Time, seq int64, gapBefore bool, note string) error {
	f.mu.Lock()
	from := f.state.sessionFrom
	f.mu.Unlock()
	if err := f.checkpoint(ctx, em, from, to, seq, gapBefore, note); err != nil {
		return err
	}
	return f.saveState()
}

// ListSettle is how long after the sync marker a payload still counts as part of the initial
// list rather than as a change.
//
// A shared informer's cache can be synced before its *handlers* have been told about everything
// in it: `WaitForCacheSync` is about the store, and the shared processor dispatches to handlers
// on its own goroutines. Every one of the four recordings of 2026-09-16 shows it — in
// baseline-topology-01 the six Services arrive 150 µs *after* the marker that says the list is
// complete. Informers.Run now waits for the handler registrations too, so a recording made
// today does not have the race; the ones already committed do, and their bytes are the
// evidence, not something to edit.
//
// The window is not only a workaround. A feeder that sees an object for the first time a
// millisecond after its list completed genuinely cannot tell "this existed already and the
// dispatch was late" from "this was created just now": it has no previous version to diff
// against either way. Calling it initial state — unknown valid start, FR-011 — is the honest
// answer to both, and it is what stops an `exposed-via` edge from a late Service payload
// claiming a valid time earlier than the workload it points at (live run 2026-09-16, finding 6).
//
// One second is several orders of magnitude more than the dispatch race needs and far less than
// the five-minute checkpoint interval, so nothing a watch reports as a change lands inside it
// except in the first second of a run.
const ListSettle = time.Second

// validFor is the valid time a fact asserted by a payload observed at `at` carries, and it is
// the whole of this feeder's answer to FR-021 (live run 2026-09-16, finding 6).
//
// Everything the initial list describes — everything observed no later than ListSettle after
// the list completed — is *initial state*: true now, true before, and the feeder cannot say
// since when. It carries FR-011's unknown start, so there is no timestamp to depend on delivery
// order, and the projector uses the first observation as the physical bound. Everything after
// that is a change the watch reported, and its valid time is the instant the payload that
// reported it says, for the object that payload describes.
//
// The comparison is against the sync marker's own instant rather than against arrival position,
// which is what makes it order-independent: permuting the payloads inside the reordering window
// moves which side of the marker a payload *arrives* on, and moves nothing about which side of
// it the payload *happened* on.
//
// The caller holds the lock.
func (f *Feeder) validFor(at time.Time) validTime {
	if f.isInitialState(at) || at.IsZero() || f.state.syncAt.IsZero() {
		return f.listValid()
	}
	return assertedAt(at)
}

// listValid is the valid time every fact of this session's initial list carries: the instant
// the list completed, flagged unknown (see unknownSince). The caller holds the lock.
func (f *Feeder) listValid() validTime { return unknownSince(f.state.syncAt) }

// noteListed records that this session's list contains an object, so that closeListAt can tell
// what is missing from what was merely not re-listed. A payload arriving inside the settling
// window counts too, for the reason ListSettle gives. The caller holds the lock.
func (f *Feeder) noteListed(kind, namespace, name string, at time.Time) {
	if !f.state.listing && !f.isInitialState(at) {
		return
	}
	f.state.listed[kind+"|"+key(namespace, name)] = true
}

// isInitialState reports whether a payload observed at `at` belongs to the current session's
// initial list. The caller holds the lock.
func (f *Feeder) isInitialState(at time.Time) bool {
	return !at.IsZero() && !f.state.syncAt.IsZero() && !at.After(f.state.syncAt.Add(ListSettle))
}

// currentObjects is everything the feeder has asserted and not retracted, by object key. The
// caller holds the lock.
//
// Nodes and Namespaces are absent on purpose. A node pool is ambient — it is asserted from the
// set of machines and never retracted when one leaves — and a namespace is a property of the
// objects inside it rather than an entity. Neither can go missing in a way a reconnecting
// feeder should act on.
func (f *Feeder) currentObjects() map[string]KnownObject {
	out := make(map[string]KnownObject,
		len(f.state.workloads)+len(f.state.services)+len(f.state.ingresses)+len(f.state.configs))
	for _, w := range f.state.workloads {
		o := KnownObject{Kind: w.kind.payload, Namespace: w.namespace, Name: w.name,
			UID: w.uid, ResourceVersion: w.resourceVersion}
		out[o.objectKey()] = o
	}
	for _, s := range f.state.services {
		o := KnownObject{Kind: KindServices, Namespace: s.namespace, Name: s.name,
			UID: s.uid, ResourceVersion: s.resourceVersion}
		out[o.objectKey()] = o
	}
	for _, i := range f.state.ingresses {
		o := KnownObject{Kind: KindIngresses, Namespace: i.namespace, Name: i.name,
			UID: i.uid, ResourceVersion: i.resourceVersion}
		out[o.objectKey()] = o
	}
	for _, c := range f.state.configs {
		o := KnownObject{Kind: c.kind.payload, Namespace: c.namespace, Name: c.name,
			UID: c.uid, ResourceVersion: c.resourceVersion}
		out[o.objectKey()] = o
	}
	return out
}

// saveState writes what the feeder has asserted, so that its successor can tell what
// disappeared while it was down. It is called after every checkpoint, which is what makes
// `state.json` and the last checkpoint in the graph agree.
func (f *Feeder) saveState() error {
	if f.State == nil {
		return nil
	}
	f.mu.Lock()
	objects := make([]KnownObject, 0, len(f.state.workloads))
	for _, o := range f.currentObjects() {
		objects = append(objects, o)
	}
	var last *time.Time
	if !f.state.lastCheckpointTo.IsZero() {
		to := f.state.lastCheckpointTo
		last = &to
	}
	f.mu.Unlock()

	if err := f.State.Save(State{
		SourceID:       f.opts.SourceID,
		WrittenAt:      time.Now().UTC(),
		LastCheckpoint: last,
		Objects:        objects,
	}); err != nil {
		// A feeder that cannot write its state is still a correct feeder; it has only lost
		// the ability to notice a gap next time. Saying so is better than stopping a watch.
		f.log.Error("k8s: could not write the feeder state; a restart will not detect a gap",
			"error", err.Error())
	}
	return nil
}

// syncNote is the human-readable half of the first checkpoint, naming what was listed.
func (f *Feeder) syncNote() string {
	switch len(f.opts.Namespaces) {
	case 0:
		return "initial informer list complete for all namespaces"
	case 1:
		return "initial informer list complete for namespace " + f.opts.Namespaces[0]
	default:
		return "initial informer list complete for namespaces " + strings.Join(f.opts.Namespaces, ", ")
	}
}

// checkpoint records the observed extent.
//
// It goes through Emit rather than Emitter.Checkpoint so that the note survives: the extent
// alone says how much was seen, and the note says what produced it, which is what an operator
// reads when a gap turns up in a diff. The event id is the SDK's, so a checkpoint minted here
// and one minted by Emitter.Checkpoint for the same instant are one event.
func (f *Feeder) checkpoint(ctx context.Context, em feeder.Emitter, from, to time.Time, seq int64, gapBefore bool, note string) error {
	if from.IsZero() || to.IsZero() {
		return nil
	}
	f.mu.Lock()
	f.state.lastCheckpointTo = to
	f.state.prevCheckpoint = to
	f.mu.Unlock()
	// feeder_lag_seconds{source} for a polling feeder is the age of its last checkpoint: how
	// far behind the cluster the graph is allowed to believe it is (FR-051, FR-052).
	telemetry.Default().SetFeederLag(f.opts.SourceID, time.Since(to))
	desc := f.Describe()
	return f.emit(ctx, em, feeder.SourceCheckpoint(desc, feeder.CheckpointID(desc, to), feeder.CheckpointFact{
		Meta:       feeder.Meta{Seq: seq},
		ExtentFrom: from,
		ExtentTo:   to,
		GapBefore:  gapBefore,
		Note:       note,
	}))
}

// handle decodes one payload and emits what it asserts.
func (f *Feeder) handle(ctx context.Context, em feeder.Emitter, payload feeder.Payload) error {
	var event WatchEvent
	if err := json.Unmarshal(payload.Bytes, &event); err != nil {
		return fmt.Errorf("k8s: decode %s payload: %w", payload.Kind, err)
	}
	if event.Type == "" {
		event.Type = EventAdded
	}

	events, err := f.mapPayload(payload.Kind, event, payload.At)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if err := f.emit(ctx, em, ev); err != nil {
			return err
		}
	}
	return nil
}

// emit sends one event and turns a refusal into an error naming the published reason code. A
// rejection is an answer, not a transport failure, but a feeder that emits an invalid event has
// a bug and must not carry on writing more of them.
func (f *Feeder) emit(ctx context.Context, em feeder.Emitter, ev *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, ev)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("k8s: %s refused: %s (%s)",
			ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}

// mapPayload dispatches one decoded watch event to its mapper. It is exported through
// MapPayload for tests that want the events without an emitter.
func (f *Feeder) mapPayload(kind string, event WatchEvent, at time.Time) ([]*graphv1.EventEnvelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch kind {
	case KindDeployments:
		return f.workloadPayload(kindDeployment, event, at, func(raw []byte) (workloadView, error) {
			var o appsv1.Deployment
			err := json.Unmarshal(raw, &o)
			return viewOfDeployment(&o), err
		})
	case KindStatefulSets:
		return f.workloadPayload(kindStatefulSet, event, at, func(raw []byte) (workloadView, error) {
			var o appsv1.StatefulSet
			err := json.Unmarshal(raw, &o)
			return viewOfStatefulSet(&o), err
		})
	case KindDaemonSets:
		return f.workloadPayload(kindDaemonSet, event, at, func(raw []byte) (workloadView, error) {
			var o appsv1.DaemonSet
			err := json.Unmarshal(raw, &o)
			return viewOfDaemonSet(&o), err
		})
	case KindJobs:
		return f.workloadPayload(kindJob, event, at, func(raw []byte) (workloadView, error) {
			var o batchv1.Job
			err := json.Unmarshal(raw, &o)
			return viewOfJob(&o), err
		})
	case KindCronJobs:
		return f.workloadPayload(kindCronJob, event, at, func(raw []byte) (workloadView, error) {
			var o batchv1.CronJob
			err := json.Unmarshal(raw, &o)
			return viewOfCronJob(&o), err
		})
	case KindServices:
		return f.servicePayload(event, at)
	case KindIngresses:
		return f.ingressPayload(event, at)
	case KindConfigMaps:
		return f.configPayload(kindConfigMap, event, at)
	case KindSecrets:
		return f.configPayload(kindSecret, event, at)
	case KindNodes:
		return f.nodePayload(event, at)
	case KindNamespaces:
		return f.namespacePayload(event)
	default:
		// An unknown kind is a recording made by a newer feeder, not a reason to stop.
		f.log.Debug("k8s: ignoring payload of unknown kind", "kind", kind)
		return nil, nil
	}
}

// MapPayload decodes one payload and returns the events it asserts, without emitting them. It
// is how a unit test exercises the mapping, and how `--dry-run` prints what would be sent.
func (f *Feeder) MapPayload(kind string, raw []byte, at time.Time) ([]*graphv1.EventEnvelope, error) {
	var event WatchEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("k8s: decode %s payload: %w", kind, err)
	}
	if event.Type == "" {
		event.Type = EventAdded
	}
	return f.mapPayload(kind, event, at)
}

// workloadPayload maps any of the five workload types.
func (f *Feeder) workloadPayload(kind resourceKind, event WatchEvent, at time.Time, decode func([]byte) (workloadView, error)) ([]*graphv1.EventEnvelope, error) {
	view, err := decode(event.Object)
	if err != nil {
		return nil, fmt.Errorf("k8s: decode %s: %w", kind.payload, err)
	}
	view.kind = kind
	view.observedAt = at
	id := kind.payload + "|" + view.key()

	if event.Type == EventDeleted {
		prev, known := f.state.workloads[id]
		delete(f.state.workloads, id)
		if !known {
			prev = view
		}
		return f.retractWorkload(prev, at), nil
	}

	f.noteListed(kind.payload, view.namespace, view.name, at)
	if f.state.listing {
		f.state.workloads[id] = view
		return nil, nil
	}

	env := f.opts.environmentOf(view.labels, view.annotations, f.state.namespace[view.namespace])
	events, err := f.mapper.WorkloadEvents(view, env, f.validFor(at))
	if err != nil {
		return nil, err
	}
	events = append(events, f.exposedViaForWorkload(view)...)

	// A change is the difference between two versions. ADDED carries no difference: it is the
	// informer's initial list, or the first sight of an object after a restart, and inventing
	// a rollout out of it would put a change node on every start (map_changes.go).
	if prev, known := f.state.workloads[id]; known && event.Type == EventModified {
		if rollout := f.mapper.RolloutEvent(prev, view, at); rollout != nil {
			events = append(events, rollout)
			// C8's platform side for this cluster (T136). Emitted after the change so the correlation's
			// subject exists when it is applied — the projector tolerates either order now, but a
			// recording that states the change first is the one a reader can follow.
			events = append(events, f.mapper.RolloutCorrelationEvents(rollout, prev, view, env, at)...)
		}
		if scaling := f.mapper.ScalingEvent(prev, view, at); scaling != nil {
			events = append(events, scaling)
		}
	}
	f.state.workloads[id] = view
	return events, nil
}

// retractWorkload ends the workload's interval and every relationship it drove.
func (f *Feeder) retractWorkload(w workloadView, at time.Time) []*graphv1.EventEnvelope {
	src := feeder.Ref(w.kind.refNamespace, w.key())
	events := []*graphv1.EventEnvelope{f.mapper.RetractNode(w.kind, w.namespace, w.name, w.resourceVersion, at)}
	if pool := f.mapper.NodePoolOf(w); pool != "" {
		events = append(events, f.mapper.RetractEdge("runs-on", src, f.mapper.nodePoolRef(pool), graphv1.EdgeType_RUNS_ON, w.resourceVersion, at))
	}
	if owner, _ := f.opts.ownerOf(w.labels); owner != "" {
		events = append(events, f.mapper.RetractEdge("owned-by", src, feeder.Ref(feeder.NSOwnerTeam, owner), graphv1.EdgeType_OWNED_BY, w.resourceVersion, at))
	}
	for _, ref := range w.configRefs {
		events = append(events, f.mapper.RetractEdge("depends-on", src,
			feeder.Ref(ref.kind.refNamespace, key(w.namespace, ref.name)), graphv1.EdgeType_DEPENDS_ON, w.resourceVersion, at))
	}
	for _, s := range f.sortedServices(w.namespace) {
		if matchesSelector(s.selector, w.podLabels) {
			events = append(events, f.mapper.RetractEdge("exposed-via", src,
				feeder.Ref(feeder.NSK8sService, s.key()), graphv1.EdgeType_EXPOSED_VIA, w.resourceVersion, at))
		}
	}
	return events
}

// servicePayload maps a Service and the exposures it creates.
func (f *Feeder) servicePayload(event WatchEvent, at time.Time) ([]*graphv1.EventEnvelope, error) {
	var o corev1.Service
	if err := json.Unmarshal(event.Object, &o); err != nil {
		return nil, fmt.Errorf("k8s: decode service: %w", err)
	}
	view := viewOfService(&o)
	view.observedAt = at

	if event.Type == EventDeleted {
		prev, known := f.state.services[view.key()]
		delete(f.state.services, view.key())
		if !known {
			prev = view
		}
		return f.retractService(prev, at), nil
	}

	f.state.services[view.key()] = view
	f.noteListed(KindServices, view.namespace, view.name, at)
	if f.state.listing {
		return nil, nil
	}
	env := f.opts.environmentOf(view.labels, view.annotations, f.state.namespace[view.namespace])
	events, err := f.mapper.ServiceEvents(view, env, f.validFor(at))
	if err != nil {
		return nil, err
	}
	return append(events, f.exposedViaForService(view)...), nil
}

// retractService ends the Service's interval and every exposure through it.
func (f *Feeder) retractService(s serviceView, at time.Time) []*graphv1.EventEnvelope {
	dst := feeder.Ref(feeder.NSK8sService, s.key())
	events := []*graphv1.EventEnvelope{f.mapper.RetractNode(kindService, s.namespace, s.name, s.resourceVersion, at)}
	for _, w := range f.sortedWorkloads(s.namespace) {
		if matchesSelector(s.selector, w.podLabels) {
			events = append(events, f.mapper.RetractEdge("exposed-via",
				feeder.Ref(w.kind.refNamespace, w.key()), dst, graphv1.EdgeType_EXPOSED_VIA, s.resourceVersion, at))
		}
	}
	return events
}

// ingressPayload maps an Ingress and the exposures it creates.
func (f *Feeder) ingressPayload(event WatchEvent, at time.Time) ([]*graphv1.EventEnvelope, error) {
	var o networkingv1.Ingress
	if err := json.Unmarshal(event.Object, &o); err != nil {
		return nil, fmt.Errorf("k8s: decode ingress: %w", err)
	}
	view := viewOfIngress(&o)
	view.observedAt = at

	if event.Type == EventDeleted {
		prev, known := f.state.ingresses[view.key()]
		delete(f.state.ingresses, view.key())
		if !known {
			prev = view
		}
		return f.retractIngress(prev, at), nil
	}

	f.state.ingresses[view.key()] = view
	f.noteListed(KindIngresses, view.namespace, view.name, at)
	if f.state.listing {
		return nil, nil
	}
	env := f.opts.environmentOf(view.labels, view.annotations, f.state.namespace[view.namespace])
	events, err := f.mapper.IngressEvents(view, env, f.validFor(at))
	if err != nil {
		return nil, err
	}
	return append(events, f.exposedViaForIngress(view)...), nil
}

// retractIngress ends the Ingress's interval and the exposures through it.
func (f *Feeder) retractIngress(i ingressView, at time.Time) []*graphv1.EventEnvelope {
	dst := feeder.Ref(feeder.NSK8sIngress, i.key())
	events := []*graphv1.EventEnvelope{f.mapper.RetractNode(kindIngress, i.namespace, i.name, i.resourceVersion, at)}
	for _, w := range f.workloadsBehindIngress(i) {
		events = append(events, f.mapper.RetractEdge("exposed-via",
			feeder.Ref(w.kind.refNamespace, w.key()), dst, graphv1.EdgeType_EXPOSED_VIA, i.resourceVersion, at))
	}
	return events
}

// configPayload maps a ConfigMap or a Secret, and the change when its version moves.
func (f *Feeder) configPayload(kind resourceKind, event WatchEvent, at time.Time) ([]*graphv1.EventEnvelope, error) {
	var view configView
	switch kind.payload {
	case KindSecrets:
		var o corev1.Secret
		if err := json.Unmarshal(event.Object, &o); err != nil {
			return nil, fmt.Errorf("k8s: decode secret: %w", err)
		}
		view = viewOfSecret(&o)
	default:
		var o corev1.ConfigMap
		if err := json.Unmarshal(event.Object, &o); err != nil {
			return nil, fmt.Errorf("k8s: decode configmap: %w", err)
		}
		view = viewOfConfigMap(&o)
	}
	view.observedAt = at
	id := kind.payload + "|" + view.key()

	if event.Type == EventDeleted {
		prev, known := f.state.configs[id]
		delete(f.state.configs, id)
		if !known {
			prev = view
		}
		return []*graphv1.EventEnvelope{
			f.mapper.RetractNode(kind, prev.namespace, prev.name, prev.resourceVersion, at),
		}, nil
	}

	f.noteListed(kind.payload, view.namespace, view.name, at)
	if f.state.listing {
		f.state.configs[id] = view
		return nil, nil
	}

	env := f.opts.environmentOf(view.labels, view.annotations, f.state.namespace[view.namespace])
	events, err := f.mapper.ConfigEvents(view, env, f.validFor(at))
	if err != nil {
		return nil, err
	}
	if prev, known := f.state.configs[id]; known && event.Type == EventModified {
		if change := f.mapper.ConfigChangeEvent(prev, view, f.dependentsOf(view), at); change != nil {
			events = append(events, change)
		}
	}
	f.state.configs[id] = view
	return events, nil
}

// nodePayload records a Node and asserts the cluster and pools it belongs to. A machine is not
// itself in the graph (see Mapper.ClusterEvents).
func (f *Feeder) nodePayload(event WatchEvent, at time.Time) ([]*graphv1.EventEnvelope, error) {
	var o corev1.Node
	if err := json.Unmarshal(event.Object, &o); err != nil {
		return nil, fmt.Errorf("k8s: decode node: %w", err)
	}
	if event.Type == EventDeleted {
		// A machine leaving is not a pool leaving, and this feeder does not count machines in
		// the graph's identity, only in a property. The pool's node count is re-asserted from
		// what is left.
		delete(f.state.nodes, o.Name)
	} else {
		f.state.nodes[o.Name] = viewOfNode(&o, f.opts)
	}
	if f.state.listing {
		return nil, nil
	}
	return f.mapper.ClusterEvents(f.knownNodes(), f.listValid())
}

// knownNodes is the node set in a stable order.
func (f *Feeder) knownNodes() []nodeView {
	out := make([]nodeView, 0, len(f.state.nodes))
	for _, n := range f.state.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// namespacePayload records the environment a Namespace declares. It emits nothing: a namespace
// is a grouping, and the graph carries it as the `k8s.namespace.name` property of everything
// inside it rather than as a node nothing would ever be asked about.
//
// The live path syncs this informer before any other, and a recording keeps namespace payloads
// outside the reordering window, so that an object is never mapped before its namespace's
// declaration is known.
func (f *Feeder) namespacePayload(event WatchEvent) ([]*graphv1.EventEnvelope, error) {
	var o corev1.Namespace
	if err := json.Unmarshal(event.Object, &o); err != nil {
		return nil, fmt.Errorf("k8s: decode namespace: %w", err)
	}
	if event.Type == EventDeleted {
		delete(f.state.namespace, o.Name)
		return nil, nil
	}
	if env := f.opts.environmentOf(o.Labels, o.Annotations, ""); env != "" && env != f.opts.Environment {
		f.state.namespace[o.Name] = env
	}
	return nil, nil
}

// exposedViaForWorkload emits the exposures of one workload through everything already known.
//
// The edge's valid time is the *Service's* observation time, not this workload's, and its
// identity carries the Service's resourceVersion. Both halves are what makes the relationship
// order-independent: whichever of the two objects arrives second emits the same event, because
// the exposure is a fact about the Service's selector and the graph must not record two
// different ones depending on which watch was quicker (FR-048).
func (f *Feeder) exposedViaForWorkload(w workloadView) []*graphv1.EventEnvelope {
	var events []*graphv1.EventEnvelope
	exposing := map[string]bool{}
	for _, s := range f.sortedServices(w.namespace) {
		if !matchesSelector(s.selector, w.podLabels) {
			continue
		}
		exposing[s.name] = true
		events = append(events, f.mapper.ExposedViaService(w, s, f.validFor(s.observedAt)))
	}
	for _, i := range f.sortedIngresses(w.namespace) {
		for _, backend := range i.backends {
			if exposing[backend] {
				events = append(events, f.mapper.ExposedViaIngress(w, i, f.validFor(i.observedAt)))
				break
			}
		}
	}
	return events
}

// exposedViaForService emits the exposures this Service creates, and the Ingress exposures it
// completes.
func (f *Feeder) exposedViaForService(s serviceView) []*graphv1.EventEnvelope {
	var events []*graphv1.EventEnvelope
	for _, w := range f.sortedWorkloads(s.namespace) {
		if !matchesSelector(s.selector, w.podLabels) {
			continue
		}
		events = append(events, f.mapper.ExposedViaService(w, s, f.validFor(s.observedAt)))
		for _, i := range f.sortedIngresses(s.namespace) {
			for _, backend := range i.backends {
				if backend == s.name {
					events = append(events, f.mapper.ExposedViaIngress(w, i, f.validFor(i.observedAt)))
					break
				}
			}
		}
	}
	return events
}

// exposedViaForIngress emits the exposures this Ingress creates through the Services it routes
// to.
func (f *Feeder) exposedViaForIngress(i ingressView) []*graphv1.EventEnvelope {
	events := make([]*graphv1.EventEnvelope, 0, len(i.backends))
	for _, w := range f.workloadsBehindIngress(i) {
		events = append(events, f.mapper.ExposedViaIngress(w, i, f.validFor(i.observedAt)))
	}
	return events
}

// workloadsBehindIngress is every known workload a Service named by the Ingress selects.
func (f *Feeder) workloadsBehindIngress(i ingressView) []workloadView {
	var out []workloadView
	seen := map[string]bool{}
	for _, backend := range i.backends {
		s, known := f.state.services[key(i.namespace, backend)]
		if !known {
			continue
		}
		for _, w := range f.sortedWorkloads(i.namespace) {
			if !matchesSelector(s.selector, w.podLabels) || seen[w.kind.payload+"|"+w.key()] {
				continue
			}
			seen[w.kind.payload+"|"+w.key()] = true
			out = append(out, w)
		}
	}
	return out
}

// dependentsOf is every known workload that consumes a configuration object, sorted, so that a
// change node's targets are byte-identical on every replay (FR-023).
func (f *Feeder) dependentsOf(c configView) []*graphv1.Ref {
	var refs []*graphv1.Ref
	for _, w := range f.sortedWorkloads(c.namespace) {
		for _, ref := range w.configRefs {
			if ref.kind.payload == c.kind.payload && ref.name == c.name {
				refs = append(refs, feeder.Ref(w.kind.refNamespace, w.key()))
				break
			}
		}
	}
	return refs
}

// sortedWorkloads, sortedServices and sortedIngresses iterate the indexes in a stable order.
// Ranging over a Go map is deliberately randomised, and an event stream that depended on it
// would differ between two runs of the same recording (FR-023).
func (f *Feeder) sortedWorkloads(namespace string) []workloadView {
	out := make([]workloadView, 0, len(f.state.workloads))
	for _, w := range f.state.workloads {
		if namespace == "" || w.namespace == namespace {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind.payload != out[j].kind.payload {
			return out[i].kind.payload < out[j].kind.payload
		}
		return out[i].key() < out[j].key()
	})
	return out
}

func (f *Feeder) sortedServices(namespace string) []serviceView {
	out := make([]serviceView, 0, len(f.state.services))
	for _, s := range f.state.services {
		if namespace == "" || s.namespace == namespace {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func (f *Feeder) sortedIngresses(namespace string) []ingressView {
	out := make([]ingressView, 0, len(f.state.ingresses))
	for _, i := range f.state.ingresses {
		if namespace == "" || i.namespace == namespace {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}
