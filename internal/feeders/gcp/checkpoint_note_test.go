// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The checkpoint's note reaches the graph (004 T043; 003 FR-057, FR-012).
//
// This feeder has built a rich checkpoint since feature 003: the poll outcome, the project and region
// scope, the audit filters and audit scope, the environment mapping, the reordering window, the
// omitted surfaces, the held traffic shifts, the recreations, the suppressions, the missing zones, the
// late arrivals and a measured `receiveTimestamp − timestamp` lag distribution. It validates that
// checkpoint. Four test files assert its `Note()`. Everything FR-057 asks a checkpoint to say about
// the scope in force was there.
//
// And none of it reached the graph. `feeder.Emitter.Checkpoint` took `(ctx, from, to, gapBefore)`, so
// the call handed over three fields and the note went out of scope — the fifth instance of this
// project's recurring shape after C4, C5, C7, `Skew` and `moved_production_traffic`: built, tested,
// and never reached. What made it invisible is that the note WAS asserted, thoroughly, one layer above
// the place it was lost.
//
// So these tests assert the note on the EVENT rather than on the type. That is the whole difference
// between the four existing test files and these two.

// The note the feeder builds is the note the checkpoint event carries.
func TestTheCheckpointNoteReachesTheEvent(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	at := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: at, Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.notes) != 1 {
		t.Fatalf("checkpoint notes = %d, want 1", len(em.notes))
	}
	note := em.notes[0]
	if note == "" {
		t.Fatal("the checkpoint event carries no note. This feeder builds one stating the scope in " +
			"force, validates it and asserts it in four test files — and for the whole of feature 003 " +
			"it stopped one call short of the graph, because the emitter could carry only three " +
			"fields (FR-057)")
	}
	// The clauses an operator reads it for. Asserted individually rather than as one golden string, so
	// a failure names which clause went missing.
	for _, want := range []string{"poll=complete", "scope=", "reordering_window="} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not state %q: %q", want, note)
		}
	}
}

// A partial poll's note says so, and reaches the event saying so.
//
// This is the clause FR-012 turns on. A partial poll and a quiet window produce the same silence in
// the events, and the checkpoint is the only thing that separates them — so a partial poll whose note
// was dropped is indistinguishable from a complete one that found nothing.
func TestAPartialPollsNoteReachesTheEvent(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	at := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: at,
			Bytes: []byte(`{"outcome":"partial","reason":"quota reserve reached"}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	note := em.notes[0]
	if !strings.Contains(note, "poll=partial") {
		t.Errorf("a partial poll's note on the event reads %q; a partial poll and a quiet window "+
			"produce the same silence in the events, and this is the only thing that separates "+
			"them (FR-012)", note)
	}
	if !strings.Contains(note, "quota reserve reached") {
		t.Errorf("the note drops the reason the poll was partial: %q", note)
	}
	// And the structured half still declares the gap, because a human-readable note is not a query.
	if len(em.gapBefore) != 1 || !em.gapBefore[0] {
		t.Errorf("gapBefore = %v, want [true]: the note is for a human and the flag is what a query "+
			"reads, so the note must not have replaced it", em.gapBefore)
	}
}
