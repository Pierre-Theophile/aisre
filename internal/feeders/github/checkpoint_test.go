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

// The checkpoint states the scope in force (004 T043; FR-057, FR-008, FR-012).
//
// FR-057 asks a checkpoint for three things: the extent covered, the filters and scope in force, and
// whether the feeder was watching immediately before that extent. Two were carried. The middle one
// was not — and the gap was not an oversight nobody had considered: this package's doc.go said an
// operator "has to know what window was covered, **under which grant**", and the `scope` field's own
// comment said "for the checkpoint (FR-008)". Both described a checkpoint that did not exist, because
// `feeder.Emitter.Checkpoint` took `(ctx, from, to, gapBefore)` and had nowhere to put it.
//
// These tests assert the note's CONTENT rather than its presence. A test that checked the note was
// non-empty would pass on a note reading "poll=complete", which is what the feeder could already say.

// The regime is on the checkpoint, and it says whose boundary it is.
func TestTheCheckpointStatesTheGrantAndWhoEnforcesIt(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.notes) != 1 {
		t.Fatalf("checkpoint notes = %d, want 1", len(em.notes))
	}
	note := em.notes[0]

	if !strings.Contains(note, "grant=selected") {
		t.Errorf("the note does not state the regime GitHub reported: %q. FR-008 asks the checkpoint "+
			"to say WHICH regime the feeder ran under, and a checkpoint that omits it cannot "+
			"distinguish a quiet estate from a grant that covered two repositories of forty", note)
	}
	if !strings.Contains(note, "platform-enforced") {
		t.Errorf("the note does not say the boundary is the platform's: %q. The distinction is the "+
			"whole of FR-008: GitHub's repository_selection is a limit the installation cannot exceed "+
			"whatever this process does, whereas an operator's allowlist is a narrowing of what the "+
			"credential could reach — and flattening them lets a configuration mistake read as an "+
			"enforced limit", note)
	}
	if !strings.Contains(note, "gate_evidence=platform_reported") {
		t.Errorf("the note does not say HOW the regime was established: %q. The regime says what the "+
			"credential may reach and the evidence says who said so, and they are different facts", note)
	}
}

// The filters in force are on it too, and an EMPTY allowlist says what empty means.
//
// This is the case worth a test of its own. An empty environment allowlist stops the connector
// emitting anything at all — deliberately, because a list nobody configured is not a licence to treat
// every preview deployment as production. So a cycle under an empty allowlist looks exactly like a
// quiet window, and the note is the only thing that can tell the two apart.
func TestTheCheckpointStatesTheFiltersInForce(t *testing.T) {
	t.Parallel()
	configured := newFeeder(t)
	configured.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := configured.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if note := em.notes[0]; !strings.Contains(note, "environments=[production]") {
		t.Errorf("the note does not state the environment allowlist: %q", note)
	}

	// And with no allowlist at all, the note says the connector was never going to speak.
	unconfigured, err := github.New(github.Options{OrgSlug: "acme"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	unconfigured.Gate = refusingGate{}
	quiet := &capturingEmitter{}
	if err := unconfigured.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, quiet); err != nil {
		t.Fatalf("Run: %v", err)
	}
	note := quiet.notes[0]
	if !strings.Contains(note, "environments=[]") {
		t.Fatalf("the note does not state the empty allowlist: %q", note)
	}
	if !strings.Contains(note, "nothing will be emitted") {
		t.Errorf("the note states an empty allowlist without saying what empty MEANS: %q. An empty "+
			"list stops this connector emitting, so a cycle under one looks exactly like a quiet "+
			"window — and the note is the only thing that can tell an operator which they are "+
			"looking at (FR-012)", note)
	}
}

// A partial poll says so on the note, and carries the reason where the poller gave one.
func TestTheCheckpointStatesAPartialPollAndItsReason(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	payloads := rolloutPayloads(t, "complete")
	// Replace the marker with a partial one carrying a reason.
	payloads[len(payloads)-1] = feeder.Payload{
		Kind: github.PayloadPollMarker, At: pollAt,
		Bytes: []byte(`{"outcome":"partial","reason":"rate limit reached mid-window"}`),
	}
	if err := f.Run(context.Background(), &payloadSource{payloads: payloads}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	note := em.notes[0]
	if !strings.Contains(note, "poll=partial") {
		t.Errorf("a partial poll's note says %q", note)
	}
	if !strings.Contains(note, "rate limit reached mid-window") {
		t.Errorf("the note drops the reason the poller gave: %q", note)
	}
}

// A replay says there was no credential to prove, rather than leaving a silence.
//
// FR-003's gate is the thing that stops this connector emitting anything from a credential that can
// write. A replay has no credential, so the gate did not run — and a note that simply omitted the
// evidence would read identically to one whose gate passed. That is the difference between "checked"
// and "nothing to check", and it is exactly the kind of silence this project treats as a claim.
func TestAReplaysCheckpointSaysNoCredentialWasProved(t *testing.T) {
	t.Parallel()
	f := newFeeder(t) // no Gate set: this is a replay
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	note := em.notes[0]
	if !strings.Contains(note, "gate_evidence=none") {
		t.Errorf("a replay's note does not say the gate did not run: %q", note)
	}
	if !strings.Contains(note, "a replay has none") {
		t.Errorf("the note says the evidence is absent without saying why: %q. Absent evidence and a "+
			"passed check must not read the same", note)
	}
}

// The extent is still on the fact itself, not only in the prose.
//
// The note is for a human; the extent is what a query reads. Moving one into the other would have
// been an easy way to satisfy the tests above and would have broken FR-012's machine-readable half.
func TestTheCheckpointsExtentIsStillStructured(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(),
		&payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.extents) != 1 {
		t.Fatalf("extents = %d, want 1", len(em.extents))
	}
	from, to := em.extents[0][0], em.extents[0][1]
	if to.IsZero() {
		t.Error("the checkpoint's ExtentTo is unset, so the window it covered is unstated")
	}
	if from.After(to) {
		t.Errorf("the extent runs backwards: [%s, %s)", from, to)
	}
}

// The note renders the same twice over the same state, so a golden does not depend on Go's map
// iteration order. Asserted on the type directly, where the maps can be made non-trivial.
func TestTheNoteIsDeterministic(t *testing.T) {
	t.Parallel()
	checkpoint := github.Checkpoint{
		From: pollAt.Add(-time.Hour), To: pollAt,
		Scope:        feeder.CredentialScope{PlatformEnforced: true, Selection: "selected"},
		GateEvidence: "platform_reported",
		Repositories: 3,
		Environments: []string{"production", "prod-eu", "prod-us"},
		Workflows:    []string{"Deploy", "Release", "Promote"},
		TargetRules:  2,
		Excluded: map[string]int{
			"environment_not_allowed": 4, "no_environment_stated": 1, "workflow_not_allowed": 2,
		},
		Deferred: 1, Released: 5,
	}
	first := checkpoint.Note()
	for range 20 {
		if again := checkpoint.Note(); again != first {
			t.Fatalf("the note is not deterministic:\n%s\n%s", first, again)
		}
	}
	// And the sorted renderings are the ones a reader sees.
	for _, want := range []string{
		"environments=[prod-eu,prod-us,production]",
		"deploy_workflows=[Deploy,Promote,Release]",
		"excluded=[environment_not_allowed=4,no_environment_stated=1,workflow_not_allowed=2]",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the note does not contain %q:\n%s", want, first)
		}
	}
}
