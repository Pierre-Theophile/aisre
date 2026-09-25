// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// A poll that failed part-way (004 T044; FR-056, Edge case 13).
//
// FR-056 has three clauses and they are not equally hard:
//
//	"A poll that fails part-way MUST emit no retraction for the unread part and MUST checkpoint only
//	 the extent it completed, declaring the gap."
//
// The gap is already asserted by TestAPartialPollMakesTheNextCheckpointDeclareAGap, and the design
// behind it is worth restating because it looks off by one: the gap a partial poll creates is in the
// window AFTER its extent, so it is declared on the NEXT checkpoint's `GapBefore` rather than on the
// partial poll's own. `GapBefore` means "I was not watching immediately before this extent", and the
// tail of the partial window is exactly what nobody watched.
//
// ---------------------------------------------------------------------------------------------
// What the no-retraction clause is worth today, stated rather than implied
//
// Neither deploy feeder calls `feeder.RetractNode` or `feeder.RetractEdge` anywhere, so the clause
// holds **structurally**: there is no code path that could retract, on a partial poll or any other.
// A test that presented itself as guarding a behaviour would be overstating that.
//
// It is still worth writing, because the tempting future mistake is specific and close. A GitHub
// deployment can be DEACTIVATED, this feeder already reads the deactivation instant, and it records
// it as a PROPERTY of the change. Retracting the node instead would read as the natural thing to do —
// and on a partial poll it would retract things the poll simply had not got to yet, turning an unread
// tail into an assertion that production stopped existing. So this is a regression guard aimed at a
// change somebody will plausibly make, not a tautology dressed as a test.

// A partial poll emits no retraction of any kind for the part it did not read.
//
// The fixture matters more than the assertion here, and the first cut of this test got it wrong. It
// ran `rolloutPayloads(t, "partial")` — which CONTAINS the deployment statuses — so every deployment
// was resolvable, nothing was deferred, and there was no unread part for a retraction to be about.
// Planting a retraction on the deferral path did not fail it: a vacuous test for the exact case it
// names. It now runs the payloads with the statuses STRIPPED, which is what "the unread part" means,
// and the same planted retraction fails it.
func TestAPartialPollEmitsNoRetraction(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: withoutStatuses(t, "partial")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The precondition is a DEFERRAL, not a non-empty event stream. Zero events is the correct
	// outcome here and is the whole point: a deployment whose statuses have not arrived is not a
	// change yet, so a cycle that read one and nothing else has nothing to say — and the failure this
	// test guards is a cycle that decides to say something anyway.
	if f.Deferred() == 0 {
		t.Fatal("nothing was deferred, so there is no unread part for a retraction to be about and " +
			"this test would pass over a feeder that retracted one")
	}
	for _, event := range em.events {
		if event.GetRetractNode() != nil {
			t.Errorf("%s retracts a node on a partial poll. The unread tail of the window is work not "+
				"done yet, and retracting for it asserts that something stopped existing when the only "+
				"fact is that nobody looked (FR-056)", event.GetEventId())
		}
		if event.GetRetractEdge() != nil {
			t.Errorf("%s retracts an edge on a partial poll, for the same reason", event.GetEventId())
		}
	}
}

// A deployment whose statuses never arrived is DEFERRED, and the checkpoint says so.
//
// This is the positive half of the clause above, and it is what makes "no retraction" more than an
// absence: the unread deployment has to be accounted for somewhere, or a reader cannot tell it from
// one that was read and produced nothing. FR-073's distinction at the level of one object — work not
// done yet is not work decided against.
func TestADeploymentWithNoStatusesIsDeferredRatherThanRetracted(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: withoutStatuses(t, "partial")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := changesOf(em.events); len(got) != 0 {
		t.Errorf("%d change(s) emitted for a deployment whose statuses never arrived. A deployment "+
			"payload says a rollout was REQUESTED; the statuses say whether anything reached "+
			"production and when, and emitting on the deployment alone dates every rollout at its "+
			"request and calls it successful before it was", len(got))
	}
	if len(em.notes) != 1 {
		t.Fatalf("checkpoint notes = %d, want 1", len(em.notes))
	}
	note := em.notes[0]
	if !strings.Contains(note, "deferred=1") {
		t.Errorf("the checkpoint does not count the unread deployment as deferred: %q. Without the "+
			"count it is indistinguishable from a deployment that was read and excluded, and only one "+
			"of those two means the feeder still owes an answer (FR-073)", note)
	}
	if strings.Contains(note, "released=1") {
		t.Errorf("the checkpoint counts the deployment as released although no status was read: %q", note)
	}
}

// The partial poll's checkpoint covers only the extent it completed — the marker's own instant, never
// a later one.
//
// The failure this guards is quiet: a checkpoint whose ExtentTo ran past what the poll actually read
// would tell the graph that a window was covered when its tail was not, and every later query over
// that window would read the silence as absence. The gap flag on the NEXT checkpoint does not undo
// it, because the flag describes the next extent's leading edge, not this one's trailing edge.
func TestAPartialPollCheckpointsOnlyWhatItCompleted(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	payloads := rolloutPayloads(t, "partial")
	markerAt := payloads[len(payloads)-1].At
	if err := f.Run(context.Background(), &payloadSource{payloads: payloads}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.extents) != 1 {
		t.Fatalf("extents = %d, want 1", len(em.extents))
	}
	from, to := em.extents[0][0], em.extents[0][1]
	if !to.Equal(markerAt) {
		t.Errorf("the partial poll checkpointed up to %s, but the poll marker's instant is %s. A "+
			"checkpoint that runs past what the poll read tells the graph a window was covered when "+
			"its tail was not, and every later query over it reads the silence as absence (FR-056)",
			to, markerAt)
	}
	if from.After(to) {
		t.Errorf("the extent runs backwards: [%s, %s)", from, to)
	}
	if !strings.Contains(em.notes[0], "poll=partial") {
		t.Errorf("the partial poll's own checkpoint does not say it was partial: %q", em.notes[0])
	}
}

// And the gap is declared on the NEXT checkpoint, not this one — which is the off-by-one the design
// deliberately has, and worth pinning so nobody "fixes" it.
//
// A partial poll read the head of its window and not the tail. Its own extent is honest about what it
// covered, so it follows no gap; the window after it is the unwatched one. Moving the flag onto the
// partial poll's own checkpoint would claim the feeder was not watching before an extent it did in
// fact read.
func TestThePartialPollsOwnCheckpointDeclaresNoGapBeforeIt(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "partial")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.gapBefore) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(em.gapBefore))
	}
	if em.gapBefore[0] {
		t.Error("the partial poll's own checkpoint declares a gap BEFORE it. It read the head of its " +
			"window, so that extent follows no gap; the unwatched window is the one after it, which " +
			"is why the flag belongs on the next checkpoint (FR-057)")
	}
}

// A retraction body is reachable in the schema, so the guard above is checking something the event
// type permits rather than something the type forbids.
//
// Without this, TestAPartialPollEmitsNoRetraction would pass just as well if `GetRetractNode` did
// not exist — the classic vacuous guard. This asserts the hole is real and the feeder is choosing not
// to use it.
func TestARetractionIsExpressibleSoTheGuardIsNotVacuous(t *testing.T) {
	t.Parallel()
	desc := feeder.Description{SourceID: "github:acme", Kind: "github", SchemaVersion: "1.0.0"}
	event := feeder.RetractNode(desc, "github:acme:retraction:1", feeder.NodeRetraction{
		Ref:      feeder.Ref(feeder.NSGitHubChange, "acme/monorepo#4321"),
		ValidEnd: time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC),
	})
	if event.GetRetractNode() == nil {
		t.Fatal("a retraction event does not read back as one, so the guard in " +
			"TestAPartialPollEmitsNoRetraction could pass over any event at all")
	}
}

// withoutStatuses is the rollout window with the deployment statuses removed: a deployment GitHub told
// us about whose outcome this cycle has not read. That is what FR-056 means by "the unread part", and
// a test about it that keeps the statuses is testing the read part instead.
func withoutStatuses(t *testing.T, outcome string) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	for _, payload := range rolloutPayloads(t, outcome) {
		if payload.Kind == github.PayloadDeploymentStatuses {
			continue
		}
		out = append(out, payload)
	}
	return out
}
