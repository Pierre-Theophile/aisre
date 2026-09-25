// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Reconnection (T067, FR-052, spec edge case "feeder gap").
//
// The live run of 2026-09-16 found that a restarted feeder cannot tell it was away: it lists,
// upserts, and says nothing about what has gone. These tests drive the two halves of the fix
// without a cluster — the state a process leaves behind, and the `start` marker payload that
// carries it to its successor.

// gapOptions is a feeder that treats anything over a minute as a gap, so a test does not have
// to span ten.
func gapOptions() k8sfeeder.Options {
	opts := kindOptions()
	opts.GapThreshold = time.Minute
	return opts
}

// watchPayload renders one watch event the way the informers do.
func watchPayload(t *testing.T, kind, eventType string, object any, seq int64, at time.Time) feeder.Payload {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	bytes, err := json.Marshal(k8sfeeder.WatchEvent{Type: eventType, Object: encoded})
	if err != nil {
		t.Fatalf("marshal watch event: %v", err)
	}
	return feeder.Payload{Kind: kind, At: at, Seq: seq, Bytes: bytes}
}

func gapService(name, rv string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", ResourceVersion: rv,
			UID: types.UID("uid-" + name)},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": name}},
	}
}

func gapDeployment(name, rv string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", ResourceVersion: rv,
			UID: types.UID("uid-deploy-" + name)},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
			},
		},
	}
}

// runFeeder replays payloads through a feeder writing its state into store, and returns what it
// emitted.
func runFeeder(t *testing.T, store k8sfeeder.StateStore, payloads []feeder.Payload) []*graphv1.EventEnvelope {
	t.Helper()
	f, err := k8sfeeder.New(gapOptions(), nil)
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	f.State = store
	clock := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	em := emit.NewMemoryEmitter(f.Describe(), emit.WithStrict(true),
		emit.WithMemoryClock(func() time.Time { return clock }))
	if err := f.Run(context.Background(), source.NewSliceSource(payloads), em); err != nil {
		t.Fatalf("run: %v", err)
	}
	return em.Events()
}

// TestRestartRetractsWhatVanishedAndReportsTheGap is T067's verification clause at the feeder's
// own boundary: two processes, one object deleted between them, and the second process must say
// both things — the gap it was blind for, and the object that is no longer there.
func TestRestartRetractsWhatVanishedAndReportsTheGap(t *testing.T) {
	dir := t.TempDir()
	store := k8sfeeder.FileStateStore{Dir: dir}
	first := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	// Process one: a Deployment and two Services.
	one := []feeder.Payload{
		watchPayload(t, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, gapDeployment("checkout", "100"), 100, first),
		watchPayload(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, gapService("checkout", "200"), 200, first),
		watchPayload(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, gapService("payments", "201"), 201, first),
		{Kind: k8sfeeder.KindSync, At: first.Add(time.Second)},
	}
	if events := runFeeder(t, store, one); len(events) == 0 {
		t.Fatal("the first process emitted nothing")
	}

	state, err := k8sfeeder.LoadState(dir, gapOptions().SourceID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.LastCheckpoint == nil || !state.LastCheckpoint.Equal(first.Add(time.Second)) {
		t.Fatalf("last checkpoint = %v, want the sync instant %s", state.LastCheckpoint, first.Add(time.Second))
	}
	if len(state.Objects) != 3 {
		t.Fatalf("state records %d objects, want the Deployment and both Services", len(state.Objects))
	}

	// Process two, an hour later, with svc/checkout gone. The marker is what the informers push
	// from the state file before their initial list.
	second := time.Date(2026, 9, 16, 11, 0, 0, 0, time.UTC)
	marker, err := k8sfeeder.StartPayload(second, state.Start())
	if err != nil {
		t.Fatalf("start payload: %v", err)
	}
	two := []feeder.Payload{
		marker,
		watchPayload(t, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, gapDeployment("checkout", "100"), 100, second),
		watchPayload(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, gapService("payments", "201"), 201, second),
		{Kind: k8sfeeder.KindSync, At: second.Add(time.Second)},
	}
	events := runFeeder(t, k8sfeeder.FileStateStore{Dir: dir}, two)

	var (
		retracted  []string
		checkpoint *graphv1.SourceCheckpoint
	)
	for _, ev := range events {
		if r := ev.GetRetractNode(); r != nil {
			retracted = append(retracted, r.GetRef().GetNamespace()+"="+r.GetRef().GetValue())
			if got := r.GetValidEnd().AsTime(); !got.Equal(second) {
				t.Errorf("retraction valid_end = %s, want the reconnection %s", got, second)
			}
		}
		if c := ev.GetSourceCheckpoint(); c != nil && c.GetGapBefore() {
			checkpoint = c
		}
	}

	if len(retracted) != 1 || retracted[0] != feeder.NSK8sService+"=shop/checkout" {
		t.Errorf("retracted %v, want exactly the Service that vanished", retracted)
	}
	if checkpoint == nil {
		t.Fatal("no checkpoint carried gap_before; a restart an hour later is a gap")
	}
	if got := checkpoint.GetExtentFrom().AsTime(); !got.Equal(second) {
		t.Errorf("extent_from = %s, want the instant the watch resumed %s", got, second)
	}
	if got := checkpoint.GetExtentTo().AsTime(); !got.Equal(second.Add(time.Second)) {
		t.Errorf("extent_to = %s, want the sync instant", got)
	}
	// The gap the graph reports is [previous checkpoint, extent_from), and the retraction's
	// valid end is that upper bound: the first instant the feeder could see again.
	if checkpoint.GetNote() == "" {
		t.Error("the gap checkpoint carries no note; an operator reading a diff needs the reason")
	}
}

// TestRestartWithinTheThresholdIsNotAGap keeps the other half honest: a feeder restarted inside
// its own checkpoint interval has missed nothing, and claiming a gap would leave a permanent
// scar in `extent` for every routine restart.
func TestRestartWithinTheThresholdIsNotAGap(t *testing.T) {
	dir := t.TempDir()
	store := k8sfeeder.FileStateStore{Dir: dir}
	first := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	runFeeder(t, store, []feeder.Payload{
		watchPayload(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, gapService("payments", "201"), 201, first),
		{Kind: k8sfeeder.KindSync, At: first.Add(time.Second)},
	})
	state, err := k8sfeeder.LoadState(dir, gapOptions().SourceID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	second := first.Add(10 * time.Second)
	marker, err := k8sfeeder.StartPayload(second, state.Start())
	if err != nil {
		t.Fatalf("start payload: %v", err)
	}
	for _, ev := range runFeeder(t, store, []feeder.Payload{
		marker,
		watchPayload(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, gapService("payments", "201"), 201, second),
		{Kind: k8sfeeder.KindSync, At: second.Add(time.Second)},
	}) {
		if c := ev.GetSourceCheckpoint(); c != nil && c.GetGapBefore() {
			t.Errorf("a %s restart was reported as a gap: %s", second.Sub(first), c.GetNote())
		}
		if r := ev.GetRetractNode(); r != nil {
			t.Errorf("nothing vanished, but %s was retracted", r.GetRef().GetValue())
		}
	}
}

// TestFirstRunIsNotAGap is the case the old resync counter got right and everything else wrong:
// a feeder that has never checkpointed has nothing to admit to.
func TestFirstRunIsNotAGap(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	state, err := k8sfeeder.LoadState(dir, gapOptions().SourceID)
	if err != nil {
		t.Fatalf("load state from an empty directory: %v", err)
	}
	if state.LastCheckpoint != nil {
		t.Fatalf("an empty state directory reported a previous checkpoint: %v", state.LastCheckpoint)
	}
	marker, err := k8sfeeder.StartPayload(at, state.Start())
	if err != nil {
		t.Fatalf("start payload: %v", err)
	}
	for _, ev := range runFeeder(t, k8sfeeder.FileStateStore{Dir: dir}, []feeder.Payload{
		marker,
		watchPayload(t, k8sfeeder.KindServices, k8sfeeder.EventAdded, gapService("payments", "201"), 201, at),
		{Kind: k8sfeeder.KindSync, At: at.Add(time.Second)},
	}) {
		if c := ev.GetSourceCheckpoint(); c != nil && c.GetGapBefore() {
			t.Errorf("the first list of a fresh source was reported as a gap: %s", c.GetNote())
		}
	}
}

// TestStateFileIsAtomicAndScoped covers the two ways a state directory is misused: a file left
// half-written by a killed process, and one directory shared by two sources.
func TestStateFileIsAtomicAndScoped(t *testing.T) {
	dir := t.TempDir()
	store := k8sfeeder.FileStateStore{Dir: dir}
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	if err := store.Save(k8sfeeder.State{
		SourceID:       "k8s:kind",
		LastCheckpoint: &at,
		Objects:        []k8sfeeder.KnownObject{{Kind: k8sfeeder.KindServices, Namespace: "shop", Name: "payments"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Saving again must leave one file, not a litter of temporaries.
	if err := store.Save(k8sfeeder.State{SourceID: "k8s:kind", LastCheckpoint: &at}); err != nil {
		t.Fatalf("save again: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) != 1 || filepath.Base(entries[0]) != k8sfeeder.StateFile {
		t.Errorf("state directory holds %v, want only %s", entries, k8sfeeder.StateFile)
	}

	if _, err := k8sfeeder.LoadState(dir, "k8s:other"); err == nil {
		t.Error("loading another source's state was accepted; that would retract one cluster's objects from another's graph")
	}
}
