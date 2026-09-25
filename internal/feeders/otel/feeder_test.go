// SPDX-License-Identifier: Apache-2.0

package otel_test

import (
	"path/filepath"
	"testing"

	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

// Conformance (T061, US3 scenarios 1–3).
//
// testdata/baseline-shop is the acceptance test of this connector: its events.jsonl is the
// twenty-three `otel:demo` lines of fixtures/baseline-topology-01, copied unchanged, and the
// payloads are synthetic OTLP exports that must reproduce them exactly. It is replayed in
// `compat` id format because that fixture was hand-authored with `@w1300` window keys; every
// other corpus, and every live run, uses the default `full` format.

// baselineFeeder is the feeder under the baseline corpus's own configuration.
func baselineFeeder() *otelfeeder.Feeder {
	return &otelfeeder.Feeder{
		SourceID: "otel:demo",
		IDFormat: otelfeeder.IDFormatCompat,
		// This corpus's events.jsonl is the fixture's own lines, recorded before ADR-0005 D6
		// added `Pointer.join_keys`, so it is replayed under the pointer shape it was recorded
		// with — the same move IDFormat already makes for its `@w1300` window keys. A live run
		// emits the join keys, and testdata/rollout-and-retract is recorded with them.
		PointerCompat: feeder.PointerCompatNoJoinKeys,
	}
}

// recordedFixture is the shipped fixture this feeder was recorded into (T063). Its payloads/
// holds both feeders' recordings side by side; this feeder skips every payload whose kind is
// not `traces` (feeder.go), and testkit compares only the events.jsonl lines carrying this
// feeder's source id, so the fixture replays here with no copy of it in testdata/.
const recordedFixture = "../../../fixtures/baseline-topology-01"

// kindFeeder is the feeder as the live run of 2026-09-16 configured it — the default `full` id
// format and the default five-minute window, which is the command line deploy/kind/README.md
// documents.
func kindFeeder() *otelfeeder.Feeder {
	// PointerCompat, as for baselineFeeder: fixtures/baseline-topology-01 was recorded on
	// 2026-09-16, before ADR-0005 D6. Re-recording it to pick up the join keys would move four
	// fixtures' goldens, which 002 tasks.md T025 and T032 deliberately forbid; the fixture
	// takes them at its next recording.
	return &otelfeeder.Feeder{SourceID: "otel:kind", PointerCompat: feeder.PointerCompatNoJoinKeys}
}

// TestConformanceRecordedFixture is the recorded half of this connector's acceptance (T063,
// constitution VIII: "synthetic-only test data is insufficient for a connector to be marked
// stable"). testdata/baseline-shop proves the feeder against OTLP somebody wrote; this proves
// it against 849 exports and 1714 spans telemetrygen actually sent to a receiver on a host,
// and it proves them against the fixture the project ships rather than against a copy.
func TestConformanceRecordedFixture(t *testing.T) {
	testkit.Conformance(t, kindFeeder(), recordedFixture)
}

func TestConformanceRecordedFixtureAgainstAGraph(t *testing.T) {
	testkit.Conformance(t, kindFeeder(), recordedFixture,
		testkit.WithShuffles(2),
		testkit.WithEmitter(func(t *testing.T, d feeder.Description) testkit.ResultEmitter {
			return emit.NewProjectorEmitter(projector.New(pgtest.Open(t)), d)
		}))
}

func TestConformanceBaselineShop(t *testing.T) {
	testkit.Conformance(t, baselineFeeder(), filepath.Join("testdata", "baseline-shop"))
}

// TestConformanceBaselineShopAgainstAGraph re-runs the same checks with a real projector over a
// test database beside the in-memory emitter, so that "the graph accepts this" is proven by the
// graph rather than by the SDK's copy of its validator.
func TestConformanceBaselineShopAgainstAGraph(t *testing.T) {
	testkit.Conformance(t, baselineFeeder(), filepath.Join("testdata", "baseline-shop"),
		testkit.WithShuffles(2),
		testkit.WithEmitter(func(t *testing.T, d feeder.Description) testkit.ResultEmitter {
			return emit.NewProjectorEmitter(projector.New(pgtest.Open(t)), d)
		}))
}

// TestConformanceRolloutAndRetract replays six windows of change: a version transition, a
// weight class that drops and is believed only after a second window, and a call path that
// goes quiet and is retracted with the valid end of the last window it was seen in.
func TestConformanceRolloutAndRetract(t *testing.T) {
	testkit.Conformance(t, &otelfeeder.Feeder{SourceID: "otel:demo"},
		filepath.Join("testdata", "rollout-and-retract"))
}

// TestCorpusManifests keeps the two manifests honest: they are loaded by the same strict
// loader `fixture verify` uses, and what they declare about the feeder must be what the feeder
// declares about itself.
func TestCorpusManifests(t *testing.T) {
	for _, dir := range []string{"baseline-shop", "rollout-and-retract"} {
		t.Run(dir, func(t *testing.T) {
			manifest, err := fixture.LoadManifest(filepath.Join("testdata", dir))
			if err != nil {
				t.Fatalf("load manifest: %v", err)
			}
			if manifest.ID != dir {
				t.Errorf("id = %q, want the directory name %q", manifest.ID, dir)
			}
			desc := baselineFeeder().Describe()
			if len(manifest.Sources) != 1 {
				t.Fatalf("declares %d sources, want 1", len(manifest.Sources))
			}
			src := manifest.Sources[0]
			if src.SourceID != desc.SourceID || src.Kind != desc.Kind ||
				src.Ordering != desc.Ordering.String() || src.ReorderingWindow != desc.ReorderingWindow {
				t.Errorf("manifest source %+v does not match Describe() %+v", src, desc)
			}
		})
	}
}
