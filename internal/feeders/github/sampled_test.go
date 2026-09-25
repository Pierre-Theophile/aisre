// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
)

// A polled history says it is polled (004 T038; FR-052).
//
//	"Polling is the source of truth for both platforms, at a configurable interval per area. A polled
//	 history MUST be marked as sampled at the poll interval, so a consumer can tell a complete history
//	 from a sampled one."
//
// ---------------------------------------------------------------------------------------------
// The trap this task walks past
//
// The obvious implementation reads the interval off `DefaultReorderingWindow`, whose own comment says
// "the window is the poll interval rather than zero" and which defaults to the same five minutes. It
// would have passed every test below.
//
// It is wrong, and the reason is that the two are different facts that currently agree. The reordering
// window is how far out of order this feeder may deliver its own events; the poll interval is how often
// it looks. The window is derived FROM the cadence — four independent paginated reads plus a doorbell
// can spread one cycle's events across a poll's length — so an operator widening the window to
// accommodate a slow paginated read, without touching their cadence, would silently change the
// sampling interval that every change in the graph claims. One field, one fact, and
// TestTheSamplingIntervalIsNotTheReorderingWindow is what holds them apart.
//
// # Why the flag rides on the change and not only on the checkpoint
//
// Different readers. A consumer asking "what was production running at 14:07?" holds a change and has
// no reason to join to a checkpoint — and whether that answer can be trusted to the minute depends on
// how coarsely the history was read. The checkpoint carries it too, because the extent a checkpoint
// claims is a polled extent: a change that began and ended between two polls is not excluded by it.

// Every change says its history was sampled, and at what interval.
func TestEveryChangeSaysItsHistoryWasSampled(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	changes := changesOf(em.events)
	if len(changes) == 0 {
		t.Fatal("no change was emitted, so this test asserts nothing")
	}
	for _, change := range changes {
		fields := change.GetProps().GetFields()
		if !fields[github.PropSampled].GetBoolValue() {
			t.Errorf("%s does not say its history was sampled. Polling is the source of truth for this "+
				"platform, so EVERY change it emits comes from a sampled history — and a consumer that "+
				"cannot tell will read a gap between two polls as a period when nothing changed",
				change.GetRef().GetValue())
		}
		if got := fields[github.PropSampledEvery].GetStringValue(); got != github.DefaultPollInterval.String() {
			t.Errorf("%s reports a sampling interval of %q, want %q", change.GetRef().GetValue(), got,
				github.DefaultPollInterval)
		}
	}
}

// A configured interval is the one recorded, not the default.
func TestTheConfiguredIntervalIsWhatAChangeReports(t *testing.T) {
	t.Parallel()
	const configured = 90 * time.Second
	f, err := github.New(github.Options{OrgSlug: "acme", Map: options(), PollInterval: configured})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	changes := changesOf(em.events)
	if len(changes) == 0 {
		t.Fatal("no change was emitted")
	}
	for _, change := range changes {
		if got := change.GetProps().GetFields()[github.PropSampledEvery].GetStringValue(); got != configured.String() {
			t.Errorf("%s reports %q though the operator configured %s; a connector that recorded the "+
				"default while polling at another cadence would be misreporting its own coverage on "+
				"every change", change.GetRef().GetValue(), got, configured)
		}
	}
	// And the checkpoint agrees with the changes. Two accounts of one cadence that could disagree
	// would be two things to diverge.
	if !strings.Contains(em.notes[0], "sampled_every="+configured.String()) {
		t.Errorf("the checkpoint reports a different interval from the changes: %q", em.notes[0])
	}
}

// The sampling interval is NOT the reordering window, and widening one does not move the other.
//
// This is the test the whole task turns on. Both default to five minutes, so any implementation that
// read the interval off the window would satisfy every other test here. Widening the window alone is
// what separates them.
func TestTheSamplingIntervalIsNotTheReorderingWindow(t *testing.T) {
	t.Parallel()
	// A slow paginated read needs a wider reordering window. The cadence has not changed.
	const widerWindow = 30 * time.Minute
	f, err := github.New(github.Options{OrgSlug: "acme", Map: options(), ReorderingWindow: widerWindow})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.Describe().ReorderingWindow; got != widerWindow {
		t.Fatalf("the feeder declares a reordering window of %s, want %s; the premise of this test is "+
			"that the window was widened", got, widerWindow)
	}
	changes := changesOf(em.events)
	if len(changes) == 0 {
		t.Fatal("no change was emitted")
	}
	for _, change := range changes {
		got := change.GetProps().GetFields()[github.PropSampledEvery].GetStringValue()
		if got == widerWindow.String() {
			t.Errorf("%s reports a sampling interval of %s — the REORDERING WINDOW. Widening the window "+
				"to accommodate a slow paginated read has silently made every change in the graph claim "+
				"its history was read six times more coarsely than it was. The two are different facts: "+
				"how far out of order this feeder may deliver, and how often it looks",
				change.GetRef().GetValue(), got)
		}
		if got != github.DefaultPollInterval.String() {
			t.Errorf("%s reports %q, want the unchanged cadence %s", change.GetRef().GetValue(), got,
				github.DefaultPollInterval)
		}
	}
	// The checkpoint reports both, separately, for the same reason.
	note := em.notes[0]
	if !strings.Contains(note, "reordering_window="+widerWindow.String()) {
		t.Errorf("the checkpoint does not report the widened window: %q", note)
	}
	if !strings.Contains(note, "sampled_every="+github.DefaultPollInterval.String()) {
		t.Errorf("the checkpoint does not report the unchanged cadence: %q", note)
	}
}

// A negative interval is refused at startup, not clamped.
//
// Clamping would be the friendlier choice and the wrong one: the interval reaches every change as a
// claim about that change's provenance, so a typo would have the connector record nonsense about its
// own coverage — silently, on every change, for the whole run. A refusal costs one restart.
func TestANegativePollIntervalIsRefusedAtStartup(t *testing.T) {
	t.Parallel()
	_, err := github.New(github.Options{OrgSlug: "acme", Map: options(), PollInterval: -time.Minute})
	if err == nil {
		t.Fatal("a negative poll interval was accepted; it is recorded on every change as the interval " +
			"that change's history was sampled at, and clamping it would bury an operator's typo in " +
			"the provenance of every change of the run (FR-052)")
	}
	if !strings.Contains(err.Error(), "FR-052") {
		t.Errorf("the refusal does not name the requirement it serves: %v", err)
	}
}

// The checkpoint says the extent it claims is a POLLED extent.
//
// Without this, an operator reading "covered [14:00, 14:30)" would take the window as continuously
// observed — and a change that began and ended between two polls inside it is not excluded by that
// extent at all.
func TestTheCheckpointSaysItsExtentIsSampled(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	note := em.notes[0]
	if !strings.Contains(note, "sampled=true") {
		t.Errorf("the checkpoint does not say its extent is sampled: %q", note)
	}
	if !strings.Contains(note, "sampled_every=") {
		t.Errorf("the checkpoint does not say at what interval: %q", note)
	}
}
