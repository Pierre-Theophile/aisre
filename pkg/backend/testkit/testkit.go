// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"context"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend"
)

// Option configures a conformance run.
type Option func(*options)

type options struct {
	fixtureDir string
}

// WithFixture points a check at an incident fixture directory whose world/ layer the backend
// is replayed against. Either the fixture directory or the world directory inside it works.
func WithFixture(dir string) Option {
	return func(o *options) { o.fixtureDir = dir }
}

func resolve(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Declaration asserts that b's declaration is registrable: every term published and in the
// telemetry family, exactly one published cost class per term, a redaction policy with a
// version, and an algebra version.
//
// This is the check that needs no recording, and it catches the failure that matters most: a
// backend reaching past the algebra. A backend that declares a graph or knowledge term has
// mistaken itself for a worker, and the boundary it would blur is simultaneously the replay
// boundary, the sanitisation point and the injection barrier.
func Declaration(t *testing.T, b backend.TelemetryBackend) {
	t.Helper()

	d := b.Describe()
	if err := d.Validate(); err != nil {
		t.Fatalf("backend declaration is not registrable: %v", err)
	}

	// Describe must be a pure function: every digest records the version it says here.
	again := b.Describe()
	if again.Name != d.Name || again.Version != d.Version || len(again.Terms) != len(d.Terms) {
		t.Fatalf("Describe is not a pure function: first call %+v, second call %+v", d, again)
	}

	for _, term := range d.Terms {
		if family := backend.FamilyOf(term); family != backend.FamilyTelemetry {
			t.Errorf("backend %s serves %s, which is a %q term; a telemetry backend serves the telemetry family only",
				d.Name, term, family)
		}
	}

	// Registration is the gate the engine actually uses, so run it rather than approximate it.
	registry := backend.NewRegistry()
	if err := registry.Register(b); err != nil {
		t.Fatalf("backend %s could not be registered: %v", d.Name, err)
	}
	if _, _, err := registry.Resolve(d.Name, "a-term-the-algebra-does-not-publish"); err == nil {
		t.Errorf("backend %s resolved a term outside the algebra; the algebra is the whole surface", d.Name)
	}
}

// Golden replays the fixture's recorded world through b and diffs every digest against the
// recorded answer, byte for byte.
//
// The world **is** the golden. There is no second set of expected files to keep in step with it,
// which is deliberate: a golden that can disagree with the recording it was derived from is a
// golden that will, and the disagreement would be invisible until a replay failed for reasons
// nobody could localise.
func Golden(t *testing.T, b backend.TelemetryBackend, opts ...Option) {
	t.Helper()
	world := loadWorld(t, resolve(opts))

	var compared int
	for _, key := range world.Keys() {
		term, ok := world.Term(key)
		if !ok {
			t.Fatalf("world index names %s but holds no term for it", key)
		}
		recorded, _ := world.Answer(key)

		got, err := b.Execute(context.Background(), &backend.AlgebraRequest{
			Term:                   term,
			DiscriminatingQuestion: "testkit:golden",
		})
		if err != nil {
			t.Errorf("%s (%s): %v", backend.TermNameOf(term), key, err)
			continue
		}
		compared++
		diffResponses(t, backend.TermNameOf(term), key, recorded, got)
	}
	if compared == 0 {
		t.Fatalf("the world at %s holds no answers; a backend with no recorded responses as its test is not merged (constitution VIII)",
			world.Dir)
	}
}

// ModesAgree asserts that live mode and recorded mode produce identical digests for identical
// requests — coverage block and join keys included, not only the summary statistics (SC-003).
//
// It builds the recorded backend over the same world and compares it with b term by term. That
// is what makes a recording a test of the live path rather than a test of itself: if the two
// disagree anywhere, one of them is wrong and a replayed investigation is not evidence about a
// live one.
func ModesAgree(t *testing.T, b backend.TelemetryBackend, opts ...Option) {
	t.Helper()
	world := loadWorld(t, resolve(opts))
	recorded := engine.NewRecordedFromWorld(world)

	for _, key := range world.Keys() {
		term, _ := world.Term(key)
		req := &backend.AlgebraRequest{Term: term, DiscriminatingQuestion: "testkit:modes_agree"}

		live, err := b.Execute(context.Background(), req)
		if err != nil {
			t.Errorf("live %s (%s): %v", backend.TermNameOf(term), key, err)
			continue
		}
		replayed, err := recorded.Execute(context.Background(), req)
		if err != nil {
			t.Errorf("recorded %s (%s): %v", backend.TermNameOf(term), key, err)
			continue
		}
		if replayed.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
			t.Errorf("%s (%s) is in this world's index but the recorded backend answered NOT_RECORDED",
				backend.TermNameOf(term), key)
			continue
		}
		diffResponses(t, backend.TermNameOf(term), key, replayed, live)
	}
}

// Conformance runs every check. This is what a backend author calls.
func Conformance(t *testing.T, b backend.TelemetryBackend, opts ...Option) {
	t.Helper()
	t.Run("declaration", func(t *testing.T) { Declaration(t, b) })
	t.Run("golden", func(t *testing.T) { Golden(t, b, opts...) })
	t.Run("modes_agree", func(t *testing.T) { ModesAgree(t, b, opts...) })
}

// loadWorld resolves the fixture directory and loads its world, failing rather than skipping when
// there is none: a backend must not be merged on the strength of a check nobody ran.
func loadWorld(t *testing.T, o options) *backend.World {
	t.Helper()
	if o.fixtureDir == "" {
		t.Fatalf("no fixture given: pass WithFixture(dir). A backend without recorded responses as its test is not merged (constitution VIII, FR-015)")
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

// diffResponses compares two answers in canonical JSON — the same bytes a golden holds — so that
// a difference in the coverage block or a join key fails as loudly as a difference in a summary
// statistic. Comparing only the response digest would be shorter and would tell a reader nothing
// about *what* moved.
func diffResponses(t *testing.T, term, key string, want, got *backend.AlgebraResponse) {
	t.Helper()

	wantBytes, err := canonicalComparable(want)
	if err != nil {
		t.Fatalf("%s (%s): canonicalise recorded answer: %v", term, key, err)
	}
	gotBytes, err := canonicalComparable(got)
	if err != nil {
		t.Fatalf("%s (%s): canonicalise answer: %v", term, key, err)
	}
	if string(wantBytes) == string(gotBytes) {
		return
	}
	t.Errorf("%s (%s) differs from the recording:\n recorded: %s\n  current: %s",
		term, key, firstDifference(string(wantBytes), string(gotBytes)), firstDifference(string(gotBytes), string(wantBytes)))
}

// canonicalComparable strips the fields that are properties of *this* call rather than of the
// answer: wall-clock duration, the mode stamp, and the response digest computed over them.
// Everything else — the digest body, the coverage block, every join key — is compared.
func canonicalComparable(resp *backend.AlgebraResponse) ([]byte, error) {
	clone, err := backend.ResponseDigest(resp) // validates the response is well formed
	if err != nil {
		return nil, err
	}
	_ = clone
	stripped := &investigationv1.AlgebraResponse{
		Outcome:       resp.GetOutcome(),
		Digest:        resp.GetDigest(),
		FailureReason: resp.GetFailureReason(),
		FailureDetail: resp.GetFailureDetail(),
		TermKey:       resp.GetTermKey(),
		CostClass:     resp.GetCostClass(),
	}
	return graph.CanonicalJSON(stripped)
}

// firstDifference trims a long canonical rendering to the neighbourhood of its first divergence,
// so a failing test names the field that moved instead of printing two kilobytes of JSON.
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
