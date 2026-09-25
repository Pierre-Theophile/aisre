// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"context"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// Option configures a conformance run. The set grows with the checks; Phase 1 published the
// type so a worker author's test function does not change shape when they arrive.
type Option func(*options)

type options struct {
	fixtureDir string
	recorded   worker.Worker
}

// WithFixture points a check at an incident fixture directory whose world/ layer the worker is
// replayed against. Either the fixture directory or the world directory inside it works.
func WithFixture(dir string) Option {
	return func(o *options) { o.fixtureDir = dir }
}

// WithRecordedCounterpart supplies the same worker built over the **recorded** backend for the
// same world, so ModesAgree can compare the two.
//
// It is a second worker rather than a mode switch because a worker holds exactly one backend:
// "the same worker in recorded mode" means "this worker's implementation over a recorded
// backend", and passing it explicitly makes the test say which recording it is comparing against
// instead of constructing one behind the author's back.
func WithRecordedCounterpart(w worker.Worker) Option {
	return func(o *options) { o.recorded = w }
}

func resolve(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Declaration asserts that w's declaration passes the registration gate — the same gate the
// engine applies, so a worker that passes here is a worker the engine will accept.
//
// This is the one check that needs no recording, and it is the one that catches the failures
// that matter most: a capability that is not read-only, a capability with no cost class, a
// model that ran without being declared, a redaction policy with no version, and a worker that
// cannot be replayed at all.
func Declaration(t *testing.T, w worker.Worker) {
	t.Helper()

	d := w.Describe()
	if err := d.Validate(); err != nil {
		t.Fatalf("worker declaration is not registrable: %v", err)
	}

	// Describe must be pure: the harness calls it before every check and holds the worker to
	// what it said the first time.
	again := w.Describe()
	if again.Name != d.Name || again.Version != d.Version || len(again.Capabilities) != len(d.Capabilities) {
		t.Fatalf("Describe is not a pure function: first call %+v, second call %+v", d, again)
	}

	// Registration is the gate the engine actually uses, so run it rather than approximate it.
	registry := worker.NewRegistry()
	if err := registry.Register(w); err != nil {
		t.Fatalf("worker %s could not be registered: %v", d.Name, err)
	}
	for _, c := range d.Capabilities {
		if _, _, err := registry.Resolve(d.Name, c.Name); err != nil {
			t.Errorf("worker %s declared capability %s but the registry will not resolve it: %v",
				d.Name, c.Name, err)
		}
	}
	if _, _, err := registry.Resolve(d.Name, "a-capability-nobody-declared"); err == nil {
		t.Errorf("worker %s resolved an undeclared capability; an undeclared capability is not callable (FR-016)", d.Name)
	}
}

// Golden replays the fixture's recorded world through w and diffs its digests against the
// recorded answers, byte for byte — the worker analogue of feeder.testkit's event-stream
// comparison.
//
// Only the terms w declares are replayed; the rest of the world belongs to its sibling workers.
// A worker that declares a capability the world never exercises is reported, because a capability
// with no recorded response is a capability nobody has tested.
func Golden(t *testing.T, w worker.Worker, opts ...Option) {
	t.Helper()
	o := resolve(opts)
	world := loadWorld(t, o)
	d := w.Describe()

	exercised := make(map[string]int, len(d.Capabilities))
	for _, key := range world.Keys() {
		term, ok := world.Term(key)
		if !ok {
			t.Fatalf("world index names %s but holds no term for it", key)
		}
		name := backend.TermNameOf(term)
		if !d.Declares(name) {
			continue
		}
		recorded, _ := world.Answer(key)

		got, err := w.Call(context.Background(), worker.Request{
			Capability: name,
			Mode:       worker.ModeRecorded,
			Algebra: &investigationv1.AlgebraRequest{
				Term:                   term,
				DiscriminatingQuestion: "testkit:golden",
				WantExemplars:          name == backend.TermExemplars,
			},
		})
		if err != nil {
			t.Errorf("%s/%s (%s): %v", d.Name, name, key, err)
			continue
		}
		exercised[name]++
		diffResponses(t, d.Name+"/"+name, key, recorded, got.Algebra)
	}

	if len(exercised) == 0 {
		t.Fatalf("the world at %s exercises none of %s's capabilities; a worker without recorded responses as its test is not merged (constitution VIII, FR-015)",
			world.Dir, d.Name)
	}
	for _, c := range d.Capabilities {
		if exercised[c.Name] == 0 {
			t.Errorf("worker %s declares %s but the world at %s holds no answer for it; a capability with no recorded response is a capability nobody has tested",
				d.Name, c.Name, world.Dir)
		}
	}
}

// ModesAgree asserts that for the same request, ModeLive and ModeRecorded produce identical
// digests — including the coverage block and the join keys, not only the summary statistics.
// This is what makes a recording a test of the live path rather than a test of itself (SC-003).
//
// w is the worker over its live backend; WithRecordedCounterpart supplies the same worker over
// the recorded backend for the same world.
func ModesAgree(t *testing.T, w worker.Worker, opts ...Option) {
	t.Helper()
	o := resolve(opts)
	world := loadWorld(t, o)
	if o.recorded == nil {
		t.Fatalf("no recorded counterpart given: pass WithRecordedCounterpart(w). Live and recorded must produce identical digests for identical inputs (SC-003), which cannot be checked with one of them")
	}
	d := w.Describe()

	for _, key := range world.Keys() {
		term, _ := world.Term(key)
		name := backend.TermNameOf(term)
		if !d.Declares(name) {
			continue
		}
		req := func(mode worker.Mode) worker.Request {
			return worker.Request{
				Capability: name,
				Mode:       mode,
				Algebra: &investigationv1.AlgebraRequest{
					Term:                   term,
					DiscriminatingQuestion: "testkit:modes_agree",
					WantExemplars:          name == backend.TermExemplars,
				},
			}
		}

		live, err := w.Call(context.Background(), req(worker.ModeLive))
		if err != nil {
			t.Errorf("live %s/%s (%s): %v", d.Name, name, key, err)
			continue
		}
		replayed, err := o.recorded.Call(context.Background(), req(worker.ModeRecorded))
		if err != nil {
			t.Errorf("recorded %s/%s (%s): %v", d.Name, name, key, err)
			continue
		}
		if replayed.Algebra.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
			t.Errorf("%s/%s (%s) is in this world's index but the recorded worker answered NOT_RECORDED",
				d.Name, name, key)
			continue
		}
		if live.Mode != worker.ModeLive || replayed.Mode != worker.ModeRecorded {
			t.Errorf("%s/%s did not stamp the mode it ran in: live=%q recorded=%q", d.Name, name, live.Mode, replayed.Mode)
		}
		diffResponses(t, d.Name+"/"+name, key, replayed.Algebra, live.Algebra)
	}
}

// Conformance runs every check. This is what a worker author calls.
func Conformance(t *testing.T, w worker.Worker, opts ...Option) {
	t.Helper()
	t.Run("declaration", func(t *testing.T) { Declaration(t, w) })
	t.Run("golden", func(t *testing.T) { Golden(t, w, opts...) })
	t.Run("modes_agree", func(t *testing.T) { ModesAgree(t, w, opts...) })
}

func loadWorld(t *testing.T, o options) *backend.World {
	t.Helper()
	if o.fixtureDir == "" {
		t.Fatalf("no fixture given: pass WithFixture(dir). A worker without recorded responses as its test is not merged (constitution VIII, FR-015)")
	}
	world, err := backend.LoadWorld(o.fixtureDir)
	if err != nil {
		nested, nestedErr := backend.LoadWorld(o.fixtureDir + "/world")
		if nestedErr != nil {
			t.Fatalf("no world at %s or %s/world: %v", o.fixtureDir, o.fixtureDir, err)
		}
		world = nested
	}
	return world
}

// diffResponses compares two answers in canonical JSON — the same bytes a golden holds — so a
// difference in the coverage block or a join key fails as loudly as one in a summary statistic.
func diffResponses(t *testing.T, label, key string, want, got *investigationv1.AlgebraResponse) {
	t.Helper()

	wantBytes, err := canonicalComparable(want)
	if err != nil {
		t.Fatalf("%s (%s): canonicalise recorded answer: %v", label, key, err)
	}
	gotBytes, err := canonicalComparable(got)
	if err != nil {
		t.Fatalf("%s (%s): canonicalise answer: %v", label, key, err)
	}
	if string(wantBytes) == string(gotBytes) {
		return
	}
	t.Errorf("%s (%s) differs from the recording:\n recorded: %s\n  current: %s",
		label, key, firstDifference(string(wantBytes), string(gotBytes)), firstDifference(string(gotBytes), string(wantBytes)))
}

// canonicalComparable strips the fields that are properties of this call rather than of the
// answer — wall-clock duration, the mode stamp and the digest computed over them — and compares
// everything else.
func canonicalComparable(resp *investigationv1.AlgebraResponse) ([]byte, error) {
	return graph.CanonicalJSON(&investigationv1.AlgebraResponse{
		Outcome:       resp.GetOutcome(),
		Digest:        resp.GetDigest(),
		FailureReason: resp.GetFailureReason(),
		FailureDetail: resp.GetFailureDetail(),
		TermKey:       resp.GetTermKey(),
		CostClass:     resp.GetCostClass(),
	})
}

func firstDifference(a, b string) string {
	const context = 120
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	start := max(i-context/2, 0)
	end := min(i+context, len(a))
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(a) {
		suffix = "…"
	}
	return prefix + a[start:end] + suffix
}
