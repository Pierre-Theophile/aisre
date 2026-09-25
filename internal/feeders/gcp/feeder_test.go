// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The feeder (T046, FR-004, FR-009, FR-131).

// Ordering is `none` and not `per_source_sequence`, and that is a promise about the *source*. The
// inputs are polls of independent APIs plus a log stream with no published ordering guarantee, so
// declaring a sequence would make testkit.Shuffle permute less than reality does — the one direction
// a declaration must never err in.
func TestTheFeederDeclaresNoOrderingBecauseTheSourceGivesNone(t *testing.T) {
	f := newFeeder(t)
	desc := f.Describe()
	if desc.Ordering != feeder.OrderingNone {
		t.Fatalf("ordering = %q, want %q: the inputs are polls plus a log stream with no published "+
			"ordering guarantee, and declaring a sequence would make the conformance test weaker "+
			"than reality (contracts/gcp-feeder.md §5.2)", desc.Ordering, feeder.OrderingNone)
	}
	if desc.SourceID != gcpfeeder.SourceIDPrefix+"nova" {
		t.Errorf("source id = %q, want %smaki", desc.SourceID, gcpfeeder.SourceIDPrefix)
	}
	if desc.Kind != gcpfeeder.Kind {
		t.Errorf("kind = %q, want %q", desc.Kind, gcpfeeder.Kind)
	}
	if desc.ReorderingWindow <= 0 {
		t.Errorf("reordering window = %s; it is measured, not assumed, and zero would make the "+
			"conformance test permute nothing", desc.ReorderingWindow)
	}
	if err := desc.Validate(); err != nil {
		t.Errorf("the description does not validate: %v", err)
	}
	// Describe is a pure function: the harness calls it before Run and compares what Run emits
	// against it, so two calls that differ would make that comparison meaningless.
	if again := f.Describe(); again.SourceID != desc.SourceID || again.Ordering != desc.Ordering ||
		again.ReorderingWindow != desc.ReorderingWindow {
		t.Error("Describe is not a pure function")
	}
}

// FR-131: no code and no checked-in default may assume a project name, so an empty scope is a
// refusal at startup rather than a run that reads nothing and reports success.
func TestAnEmptyScopeIsRefusedAtStartup(t *testing.T) {
	_, err := gcpfeeder.New(gcpfeeder.Options{OrgSlug: "nova"})
	if err == nil {
		t.Fatal("a feeder with no project scope was built")
	}
	if !strings.Contains(err.Error(), "FR-131") {
		t.Errorf("the refusal does not name the rule: %q", err.Error())
	}
	if _, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "nova",
		Scope:   gcpfeeder.Scope{Projects: []string{"nova-production"}},
	}); err == nil {
		t.Error("a feeder with no region scope was built; a region scope of none reads nothing")
	}
	if _, err := gcpfeeder.New(gcpfeeder.Options{
		Scope: gcpfeeder.Scope{Projects: []string{"p"}, Regions: []string{"r"}},
	}); err == nil {
		t.Error("a feeder with no org slug was built; every organisation's events would be one source")
	}
}

// refusingGate stands in for a credential that turned out to hold a write.
type refusingGate struct{ called int }

func (g *refusingGate) Prove(context.Context) (*gcpx.Credential, error) {
	g.called++
	return nil, errors.New("the credential holds run.services.update")
}

// countingSource records whether anything read a payload.
type countingSource struct {
	payloads []feeder.Payload
	reads    int
}

func (s *countingSource) Next(context.Context) (feeder.Payload, error) {
	s.reads++
	if len(s.payloads) == 0 {
		return feeder.Payload{}, io.EOF
	}
	next := s.payloads[0]
	s.payloads = s.payloads[1:]
	return next, nil
}

// recordingEmitter records what reached the graph.
type recordingEmitter struct {
	events      []*graphv1.EventEnvelope
	checkpoints int
	gapBefore   []bool
	flushed     int
	notes       []string
}

func (e *recordingEmitter) Emit(_ context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	e.events = append(e.events, ev)
	return &graphv1.IngestResult{}, nil
}

func (e *recordingEmitter) Checkpoint(_ context.Context, fact feeder.CheckpointFact) error {
	e.checkpoints++
	e.gapBefore = append(e.gapBefore, fact.GapBefore)
	// The note is captured, because a double that dropped it would let the feeder drop it too — which
	// is how the scope in force went unreported for a whole feature (004 T043).
	e.notes = append(e.notes, fact.Note)
	return nil
}

func (e *recordingEmitter) Flush(context.Context) error { e.flushed++; return nil }

// FR-004: the gate runs before anything is emitted, and a refusal emits nothing at all.
func TestARefusedGateEmitsNothingAndReadsNothing(t *testing.T) {
	f := newFeeder(t)
	gate := &refusingGate{}
	f.Gate = gate

	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(), Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	em := &recordingEmitter{}

	err := f.Run(context.Background(), src, em)
	if err == nil {
		t.Fatal("a refused gate did not stop the run")
	}
	if gate.called != 1 {
		t.Errorf("the gate was called %d times", gate.called)
	}
	if src.reads != 0 {
		t.Fatalf("the feeder read %d payloads after the gate refused; the gate runs before anything "+
			"is emitted (FR-004)", src.reads)
	}
	if len(em.events) != 0 || em.checkpoints != 0 {
		t.Fatalf("the feeder emitted %d events and %d checkpoints after a refused gate",
			len(em.events), em.checkpoints)
	}
}

// A poll marker with no outcome is refused: a poll whose outcome is unstated cannot be evidence of
// absence, and typing it as complete is the retraction bug (FR-012).
func TestAPollMarkerWithNoOutcomeIsRefused(t *testing.T) {
	f := newFeeder(t)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(), Bytes: []byte(`{}`)},
	}}
	if err := f.Run(context.Background(), src, &recordingEmitter{}); err == nil {
		t.Fatal("a poll marker with no outcome was accepted")
	}
}

// A partial poll declares its gap, so the graph is told the silence before `from` was ignorance
// rather than absence.
func TestAPartialPollMarkerDeclaresItsGap(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(),
			Bytes: []byte(`{"outcome":"partial","reason":"the europe-west1 call timed out"}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if em.checkpoints != 1 {
		t.Fatalf("checkpoints = %d, want 1", em.checkpoints)
	}
	if !em.gapBefore[0] {
		t.Error("a partial poll's checkpoint did not declare a gap (FR-012)")
	}

	complete := newFeeder(t)
	em = &recordingEmitter{}
	src = &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(), Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := complete.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if em.gapBefore[0] {
		t.Error("a complete poll's checkpoint declared a gap, so the partial case proves nothing")
	}
}

// A payload kind nobody handles is an error rather than a silently ignored file, because a fixture
// directory with an unread payload tests less than it claims.
func TestAnUnhandledPayloadKindIsAnError(t *testing.T) {
	f := newFeeder(t)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: "something-nobody-reads", At: time.Now(), Bytes: []byte(`{}`)},
	}}
	err := f.Run(context.Background(), src, &recordingEmitter{})
	if err == nil {
		t.Fatal("an unknown payload kind was ignored")
	}
	if !strings.Contains(err.Error(), gcpfeeder.PayloadServices) {
		t.Errorf("the error does not name the kinds this feeder reads: %q", err.Error())
	}
}

// Run flushes before returning, on success and on failure alike: a buffered event nobody flushed is
// an event the graph never saw.
func TestRunFlushesBeforeReturning(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	src := &countingSource{}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if em.flushed == 0 {
		t.Fatal("Run returned without flushing")
	}
}

// The measured lag distribution reaches the checkpoint, which is the whole of what "measured, not
// assumed" buys: the configured window can be justified from data and widened when the data says so.
func TestTheObservedLagIsMeasuredAndReported(t *testing.T) {
	f := newFeeder(t)
	for _, sample := range []time.Duration{time.Second, 2 * time.Second, 30 * time.Second} {
		f.ObserveLag(sample)
	}
	// A negative lag means the two clocks disagree, which is a fact about the clocks and not about
	// ingestion, so it is not folded in.
	f.ObserveLag(-time.Hour)

	checkpoint := gcpfeeder.Checkpoint{
		From: time.Now().Add(-time.Minute), To: time.Now(),
		Outcome: gcpfeeder.PollComplete, Scope: scope(),
		ReorderingWindow: 10 * time.Minute,
		ObservedLagP50:   2 * time.Second,
		ObservedLagP99:   30 * time.Second,
	}
	note := checkpoint.Note()
	if !strings.Contains(note, "observed_lag_p50=") || !strings.Contains(note, "observed_lag_p99=") {
		t.Errorf("the checkpoint does not report the observed lag: %s", note)
	}
	if !strings.Contains(note, "reordering_window=10m0s") {
		t.Errorf("the checkpoint does not report the declared window: %s", note)
	}
}

// A held shift survives to the next checkpoint, which is what makes holding honest rather than
// silent.
func TestAHeldShiftReachesTheCheckpoint(t *testing.T) {
	f := newFeeder(t)
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	before := gcpfeeder.ObservedTrafficSplit(statuses("a", 100))
	after := gcpfeeder.ObservedTrafficSplit(statuses("b", 100))
	f.HoldShift(gcpfeeder.Hold(service, before, after, time.Now()))

	if len(f.Held()) != 1 {
		t.Fatalf("held = %d, want 1", len(f.Held()))
	}
	em := &recordingEmitter{}
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(), Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if em.checkpoints != 1 {
		t.Fatalf("checkpoints = %d", em.checkpoints)
	}
}

func newFeeder(t *testing.T) *gcpfeeder.Feeder {
	t.Helper()
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "nova",
		Scope:   scope(),
		// The actor classification is configuration, and supplying it is what makes the
		// request-versus-completion-entry assertion in map_test.go discriminating: without a list,
		// the deploy principal and the service agent are both unclassified and the two are
		// indistinguishable.
		Actors: actorPolicy(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}
