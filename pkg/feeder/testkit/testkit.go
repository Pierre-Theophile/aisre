// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Defaults for a conformance run.
const (
	// DefaultShuffles is how many seeded permutations Shuffle runs.
	DefaultShuffles = 6
	// DefaultSeed makes the permutations reproducible: a failure in CI is the same failure
	// locally.
	DefaultSeed uint64 = 0x5E3A6E17
	// DefaultTimeout bounds one replay. A recorded run is a few thousand payloads through a
	// pure function; a minute is a hang, not a slow test.
	DefaultTimeout = time.Minute
	// replayClock is the observed time the in-memory emitter stamps. Observed time is
	// assigned by the graph and is never part of what a feeder is compared on, so pinning it
	// keeps the comparison about the events.
	replayClockRFC3339 = "2000-01-01T00:00:00Z"
)

// ResultEmitter is an Emitter a conformance check can read the results back from. The emitters
// in pkg/feeder/emit all satisfy it.
type ResultEmitter interface {
	feeder.Emitter
	// Results returns one result per Emit call, in call order.
	Results() []*graphv1.IngestResult
}

// Option configures a conformance run.
type Option func(*options)

type options struct {
	newEmitter func(t *testing.T, d feeder.Description) ResultEmitter
	shuffles   int
	seed       uint64
	timeout    time.Duration
	skipStream bool
}

// WithEmitter adds a second Emitter beside the in-memory one.
//
// Pass emit.NewProjectorEmitter over a test database to make every check additionally prove
// that a real graph accepts, and deduplicates, what the feeder emits. Every event goes to both
// emitters and the results reported are the extra emitter's, so the checks become strictly
// stronger; nothing they already asserted is given up. The function is called once per replay,
// so each replay gets a graph of its own unless the caller shares one deliberately — except in
// DoubleDeliver, where both passes deliberately share one, since that is the whole point.
func WithEmitter(fn func(t *testing.T, d feeder.Description) ResultEmitter) Option {
	return func(o *options) { o.newEmitter = fn }
}

// WithShuffles sets how many seeded permutations Shuffle runs. Zero means DefaultShuffles.
func WithShuffles(n int) Option {
	return func(o *options) { o.shuffles = n }
}

// WithSeed varies the permutations.
func WithSeed(seed uint64) Option {
	return func(o *options) { o.seed = seed }
}

// WithTimeout bounds one replay.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithoutStreamComparison skips the comparison against the fixture's events.jsonl, for a
// fixture that does not have one yet. Everything else still runs, so a feeder under
// development is still held to validity, order independence and idempotency.
func WithoutStreamComparison() Option {
	return func(o *options) { o.skipStream = true }
}

func resolve(opts []Option) options {
	o := options{shuffles: DefaultShuffles, seed: DefaultSeed, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	if o.shuffles <= 0 {
		o.shuffles = DefaultShuffles
	}
	if o.timeout <= 0 {
		o.timeout = DefaultTimeout
	}
	return o
}

// RunResult is what one replay produced.
type RunResult struct {
	// Description is what the feeder said about itself.
	Description feeder.Description
	// FixtureDir is the directory that was replayed.
	FixtureDir string
	// Payloads is how many raw payloads the feeder was given.
	Payloads int
	// Events are the events it emitted and the emitter accepted, in order.
	Events []*graphv1.EventEnvelope
	// Results are the emitter's answers, one per Emit call.
	Results []*graphv1.IngestResult
	// Compared is how many events were compared against the fixture's events.jsonl. Zero
	// means the fixture has none, or the comparison was skipped.
	Compared int
}

// Run replays a fixture's payloads through f and asserts the result is a valid, expected event
// stream (contracts/feeder-sdk.md §Recording and testing).
//
// It checks, in order: the description is well formed; the feeder consumes the recording
// without error; every event validates against the published schema; nothing was refused;
// every identity is in a namespace the feeder declared; Flush was called before Run returned;
// and the stream equals the fixture's events.jsonl for this feeder's source id, compared as
// canonical JSON with the log-added observedAt and appendedSeq left out.
func Run(t *testing.T, f feeder.Feeder, fixtureDir string, opts ...Option) *RunResult {
	t.Helper()
	o := resolve(opts)
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		t.Fatalf("testkit: Describe() is not usable: %v", err)
	}

	src, err := source.NewFileSource(fixtureDir)
	if err != nil {
		t.Fatalf("testkit: read payloads from %s: %v", fixtureDir, err)
	}
	if src.Len() == 0 {
		t.Fatalf("testkit: %s has no payloads; a feeder without a recorded corpus is not testable (constitution VIII)",
			source.PayloadsRoot(fixtureDir))
	}

	em, memory := newEmitter(t, o, desc)
	result := &RunResult{Description: desc, FixtureDir: fixtureDir, Payloads: src.Len()}
	replay(t, o, f, src, em)

	result.Results = em.Results()
	result.Events = memory.Events()
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("testkit: %d of %d events do not validate; the first is %s",
			len(rejected), len(result.Results), describeResult(rejected[0]))
	}
	if memory.Flushes() == 0 {
		t.Error("testkit: the feeder returned from Run without calling Emitter.Flush; buffered events would be lost")
	}
	assertNoRejections(t, result.Results)
	assertDeclaredNamespaces(t, desc, result.Events)

	if !o.skipStream {
		result.Compared = compareStream(t, desc, fixtureDir, result.Events)
	}
	return result
}

// Shuffle re-runs the feeder with its payloads permuted inside its own declared reordering
// window and asserts it emits the same facts (FR-021, FR-048).
//
// A feeder declares how far out of order its source may deliver; that declaration is a promise
// this check takes literally. The comparison is on the *set* of events, not their order: a
// feeder is free to emit in whatever order the payloads arrived, but every event carries the
// valid time its source asserted, so an identical set is an identical valid-time state
// (research §5). Declaring a wider window than the truth therefore makes this check stricter,
// never weaker.
func Shuffle(t *testing.T, f feeder.Feeder, fixtureDir string, opts ...Option) {
	t.Helper()
	o := resolve(opts)
	desc := f.Describe()

	payloads, err := source.ReadPayloads(fixtureDir)
	if err != nil {
		t.Fatalf("testkit: read payloads from %s: %v", fixtureDir, err)
	}
	want := fingerprint(t, replayOnce(t, o, f, desc, payloads))

	for i := range o.shuffles {
		//nolint:gosec // a seeded, reproducible permutation; not a security decision
		rng := rand.New(rand.NewPCG(o.seed, uint64(i)+1))
		permuted := shuffleWithinWindow(payloads, desc.ReorderingWindow, rng)
		got := fingerprint(t, replayOnce(t, o, f, desc, permuted))
		if diff := diffFingerprints(want, got); diff != "" {
			t.Fatalf("testkit: shuffle %d inside the declared reordering window (%s) changed what the feeder emitted:\n%s",
				i+1, desc.ReorderingWindow, diff)
		}
	}
}

// DoubleDeliver replays the fixture twice into the same graph and asserts every event of the
// second run is a DUPLICATE_NOOP (FR-020, SC-004).
//
// This is the check that catches a non-deterministic event id. A feeder that mints an id from
// a timestamp, a counter or a random source passes every other check and fails this one, which
// is the whole reason it exists.
func DoubleDeliver(t *testing.T, f feeder.Feeder, fixtureDir string, opts ...Option) {
	t.Helper()
	o := resolve(opts)
	desc := f.Describe()
	em, _ := newEmitter(t, o, desc)

	for pass := 1; pass <= 2; pass++ {
		src, err := source.NewFileSource(fixtureDir)
		if err != nil {
			t.Fatalf("testkit: read payloads from %s: %v", fixtureDir, err)
		}
		before := len(em.Results())
		replay(t, o, f, src, em)
		results := em.Results()[before:]
		if len(results) == 0 {
			t.Fatalf("testkit: pass %d emitted nothing", pass)
		}
		if pass == 1 {
			assertNoRejections(t, results)
			continue
		}
		for _, result := range results {
			if result.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
				t.Fatalf("testkit: re-delivering %s answered %s, want DUPLICATE_NOOP; the feeder's event ids are not a pure function of the payloads (%s)",
					result.GetEventId(), result.GetStatus(), describeResult(result))
			}
		}
	}
}

// Conformance runs Run, Shuffle and DoubleDeliver as subtests. It is what a connector's test
// file calls.
func Conformance(t *testing.T, f feeder.Feeder, fixtureDir string, opts ...Option) {
	t.Helper()
	t.Run("run", func(t *testing.T) { Run(t, f, fixtureDir, opts...) })
	t.Run("shuffle", func(t *testing.T) { Shuffle(t, f, fixtureDir, opts...) })
	t.Run("double-deliver", func(t *testing.T) { DoubleDeliver(t, f, fixtureDir, opts...) })
}

// newEmitter builds the emitter for one replay.
//
// An in-memory emitter is always present, because it is the only one that keeps the envelopes
// and reports what failed validation; a caller's extra emitter is teed beside it. The in-memory
// one is deliberately not strict: a refused event must not abort the replay before the checks
// have a chance to report it with its reason code and field.
func newEmitter(t *testing.T, o options, desc feeder.Description) (ResultEmitter, *emit.MemoryEmitter) {
	t.Helper()
	clock, err := time.Parse(time.RFC3339, replayClockRFC3339)
	if err != nil { // unreachable: the constant is a literal
		t.Fatalf("testkit: replay clock: %v", err)
	}
	memory := emit.NewMemoryEmitter(desc,
		emit.WithStrict(false),
		emit.WithMemoryClock(func() time.Time { return clock }))
	if o.newEmitter == nil {
		return memory, memory
	}
	extra := o.newEmitter(t, desc)
	if extra == nil {
		t.Fatal("testkit: WithEmitter returned nil")
	}
	return &teeEmitter{desc: desc, memory: memory, extra: extra}, memory
}

// teeEmitter sends every event to the in-memory emitter and then to the caller's, reporting
// the caller's results: those come from a real graph and are the stronger statement.
type teeEmitter struct {
	desc   feeder.Description
	memory *emit.MemoryEmitter
	extra  ResultEmitter
}

var _ ResultEmitter = (*teeEmitter)(nil)

func (e *teeEmitter) Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	if _, err := e.memory.Emit(ctx, ev); err != nil {
		return nil, err
	}
	return e.extra.Emit(ctx, ev)
}

func (e *teeEmitter) Checkpoint(ctx context.Context, fact feeder.CheckpointFact) error {
	// Routed through Emit rather than delegated, so both emitters see the same bytes.
	_, err := e.Emit(ctx, feeder.SourceCheckpoint(e.desc, feeder.CheckpointID(e.desc, fact.ExtentTo), fact))
	return err
}

func (e *teeEmitter) Flush(ctx context.Context) error {
	if err := e.memory.Flush(ctx); err != nil {
		return err
	}
	return e.extra.Flush(ctx)
}

func (e *teeEmitter) Results() []*graphv1.IngestResult { return e.extra.Results() }

// replay runs the feeder once, bounded by the configured timeout.
func replay(t *testing.T, o options, f feeder.Feeder, src feeder.Source, em feeder.Emitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), o.timeout)
	defer cancel()
	if err := f.Run(ctx, src, em); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("testkit: Run: %v", err)
	}
	if err := em.Flush(ctx); err != nil {
		t.Fatalf("testkit: Flush: %v", err)
	}
}

// replayOnce runs the feeder over a payload slice with a throwaway emitter and returns what it
// emitted.
func replayOnce(t *testing.T, o options, f feeder.Feeder, desc feeder.Description, payloads []feeder.Payload) []*graphv1.EventEnvelope {
	t.Helper()
	em, memory := newEmitter(t, o, desc)
	replay(t, o, f, source.NewSliceSource(payloads), em)
	if memory != nil {
		if rejected := memory.Rejected(); len(rejected) > 0 {
			t.Fatalf("testkit: %d events were refused; the first is %s", len(rejected), describeResult(rejected[0]))
		}
		return memory.Events()
	}
	return appliedEvents(t, em)
}

// appliedEvents is the fallback for an emitter that reports results but not events: a caller's
// own emitter is not required to keep envelopes, so the conformance checks that need them fall
// back to what it did keep.
func appliedEvents(t *testing.T, em ResultEmitter) []*graphv1.EventEnvelope {
	t.Helper()
	if keeper, ok := em.(interface {
		Events() []*graphv1.EventEnvelope
	}); ok {
		return keeper.Events()
	}
	return nil
}

// assertNoRejections fails on the first refusal, quoting the reason code and the field, which
// is everything a feeder author needs to fix it.
func assertNoRejections(t *testing.T, results []*graphv1.IngestResult) {
	t.Helper()
	for _, result := range results {
		if result.GetStatus() == graphv1.IngestResult_REJECTED {
			t.Fatalf("testkit: the graph refused %s", describeResult(result))
		}
	}
}

// assertDeclaredNamespaces holds a feeder to the identifier namespaces it said it would use.
// Two connectors minting identities in each other's namespaces is how entity resolution
// quietly stops working, and a Description is where that is declared.
func assertDeclaredNamespaces(t *testing.T, desc feeder.Description, events []*graphv1.EventEnvelope) {
	t.Helper()
	if len(desc.Namespaces) == 0 {
		return
	}
	seen := map[string]string{}
	for _, ev := range events {
		for _, ref := range refsOf(ev) {
			if ref.GetNamespace() == "" || desc.DeclaresNamespace(ref.GetNamespace()) {
				continue
			}
			if _, dup := seen[ref.GetNamespace()]; !dup {
				seen[ref.GetNamespace()] = ev.GetEventId()
			}
		}
	}
	if len(seen) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(seen))
	var lines []string
	for _, ns := range names {
		lines = append(lines, fmt.Sprintf("  %s (first in %s)", ns, seen[ns]))
	}
	t.Fatalf("testkit: the feeder emitted refs in namespaces its Description does not declare:\n%s\ndeclared: %s",
		strings.Join(lines, "\n"), strings.Join(desc.Namespaces, ", "))
}

// refsOf collects every identity an event names.
func refsOf(ev *graphv1.EventEnvelope) []*graphv1.Ref {
	switch body := ev.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return []*graphv1.Ref{body.UpsertNode.GetRef()}
	case *graphv1.EventEnvelope_UpsertEdge:
		return []*graphv1.Ref{body.UpsertEdge.GetSrc(), body.UpsertEdge.GetDst()}
	case *graphv1.EventEnvelope_RetractNode:
		return []*graphv1.Ref{body.RetractNode.GetRef()}
	case *graphv1.EventEnvelope_RetractEdge:
		return []*graphv1.Ref{body.RetractEdge.GetSrc(), body.RetractEdge.GetDst()}
	case *graphv1.EventEnvelope_ObserveChange:
		return append([]*graphv1.Ref{body.ObserveChange.GetRef()}, body.ObserveChange.GetTargets()...)
	case *graphv1.EventEnvelope_IdentityClaim:
		return []*graphv1.Ref{body.IdentityClaim.GetSubject(), body.IdentityClaim.GetClaim()}
	default:
		return nil
	}
}

// compareStream compares the emitted events with the fixture's recorded stream, for this
// feeder's source only: a fixture may be the recording of several feeders, and each is
// responsible for its own share of the events.
func compareStream(t *testing.T, desc feeder.Description, fixtureDir string, got []*graphv1.EventEnvelope) int {
	t.Helper()
	path := filepath.Join(fixtureDir, fixture.DefaultEventsFile)
	if _, err := os.Stat(path); err != nil {
		t.Logf("testkit: %s has no %s, so the emitted stream was not compared; record one with pkg/feeder/record",
			fixtureDir, fixture.DefaultEventsFile)
		return 0
	}
	recorded, err := fixture.ReadEvents(path)
	if err != nil {
		t.Fatalf("testkit: read %s: %v", path, err)
	}

	var want []*graphv1.EventEnvelope
	for _, event := range recorded {
		if event.Envelope.GetSourceId() == desc.SourceID {
			want = append(want, event.Envelope)
		}
	}
	if len(want) == 0 {
		t.Fatalf("testkit: %s holds no events for source %s; either the fixture was recorded by a different feeder or the source id changed",
			path, desc.SourceID)
	}

	for i := range min(len(want), len(got)) {
		wantLine := canonical(t, want[i])
		gotLine := canonical(t, got[i])
		if wantLine != gotLine {
			t.Fatalf("testkit: event %d differs from %s\n  recorded: %s\n  emitted:  %s",
				i+1, filepath.Base(path), wantLine, gotLine)
		}
	}
	if len(want) != len(got) {
		t.Fatalf("testkit: the feeder emitted %d events for source %s, %s records %d",
			len(got), desc.SourceID, filepath.Base(path), len(want))
	}
	return len(want)
}

// fingerprint renders a set of events as sorted canonical lines, the order-independent form
// the shuffle check compares.
func fingerprint(t *testing.T, events []*graphv1.EventEnvelope) []string {
	t.Helper()
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		lines = append(lines, canonical(t, ev))
	}
	slices.Sort(lines)
	return lines
}

// diffFingerprints returns a human-readable difference, or "" when the two sets are equal.
func diffFingerprints(want, got []string) string {
	if slices.Equal(want, got) {
		return ""
	}
	missing := setDifference(want, got)
	extra := setDifference(got, want)
	var out strings.Builder
	fmt.Fprintf(&out, "  in-order run emitted %d events, shuffled run %d\n", len(want), len(got))
	for _, line := range firstN(missing, 3) {
		fmt.Fprintf(&out, "  only in the in-order run: %s\n", line)
	}
	for _, line := range firstN(extra, 3) {
		fmt.Fprintf(&out, "  only in the shuffled run: %s\n", line)
	}
	return out.String()
}

func setDifference(a, b []string) []string {
	have := make(map[string]int, len(b))
	for _, line := range b {
		have[line]++
	}
	var out []string
	for _, line := range a {
		if have[line] > 0 {
			have[line]--
			continue
		}
		out = append(out, line)
	}
	return out
}

func firstN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[:n]
}

func canonical(t *testing.T, ev *graphv1.EventEnvelope) string {
	t.Helper()
	raw, err := graph.CanonicalJSON(ev)
	if err != nil {
		t.Fatalf("testkit: canonical json for %s: %v", ev.GetEventId(), err)
	}
	return string(raw)
}

func describeResult(result *graphv1.IngestResult) string {
	return fmt.Sprintf("%s: %s (%s)", result.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
}

// shuffleWithinWindow permutes payloads so that no payload overtakes one more than window
// older than it, which is exactly the promise a Description makes.
//
// A window of zero returns the payloads untouched: a feeder that promises strict order is
// entitled to be tested in strict order. Payloads with no arrival time — a payload set
// assembled by hand rather than recorded — are all inside every window, so such a fixture is
// permuted freely.
func shuffleWithinWindow(payloads []feeder.Payload, window time.Duration, rng *rand.Rand) []feeder.Payload {
	if window <= 0 || len(payloads) < 2 {
		return slices.Clone(payloads)
	}
	queue := slices.Clone(payloads)
	out := make([]feeder.Payload, 0, len(queue))
	for len(queue) > 0 {
		limit := 1
		for limit < len(queue) && queue[limit].At.Sub(queue[0].At) <= window {
			limit++
		}
		pick := rng.IntN(limit)
		out = append(out, queue[pick])
		queue = append(queue[:pick], queue[pick+1:]...)
	}
	return out
}
