// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/fixture/fixturetest"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// These are the constitution's named checks run through the harness that will run them in CI
// (constitution VIII, FR-047, FR-048, SC-003, SC-004, SC-009). The projector's own fixture test
// asserts the same behaviour against the projection directly; this one asserts that
// `fixture verify` reports it.

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// TestLoadBaseline is T027's check: the baseline fixture loads whole, and its one deliberately
// invalid event is refused for the stated reason without moving the graph (SC-009).
func TestLoadBaseline(t *testing.T) {
	ctx := context.Background()
	store := pgtest.Open(t)

	report, err := fixture.Load(ctx, projector.New(store), filepath.Join(fixturesDir, "baseline-topology-01"), fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if report.Applied != 89 {
		t.Errorf("applied = %d, want 89", report.Applied)
	}
	if report.DuplicateNoop != 0 {
		t.Errorf("duplicate_noop = %d, want 0 on a fresh store", report.DuplicateNoop)
	}
	if report.Rejected != 1 {
		t.Errorf("rejected = %d, want 1 (the telemetry-payload event)", report.Rejected)
	}
	if len(report.RejectedMismatches) > 0 {
		t.Errorf("rejection contract did not hold: %s", strings.Join(report.RejectedMismatches, "; "))
	}
	if len(report.Results) != 90 {
		t.Errorf("results = %d, want 90 (89 accepted + 1 rejected)", len(report.Results))
	}

	// The rejection is the last result, and it names the offending field (FR-024).
	last := report.Results[len(report.Results)-1]
	if last.GetReasonCode() != "telemetry_payload" {
		t.Errorf("reason_code = %q, want telemetry_payload", last.GetReasonCode())
	}
	if !strings.Contains(last.GetReasonDetail(), "latency_samples") {
		t.Errorf("reason_detail = %q, want it to name latency_samples", last.GetReasonDetail())
	}
}

// TestLoadIsIdempotent is the load half of FR-020: loading the same fixture twice into the same
// store applies nothing the second time.
func TestLoadIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := pgtest.Open(t)
	p := projector.New(store)
	dir := filepath.Join(fixturesDir, "late-arriving-fact-01")

	if _, err := fixture.Load(ctx, p, dir, fixture.LoadOptions{}); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	again, err := fixture.Load(ctx, p, dir, fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if again.Applied != 0 || again.DuplicateNoop != 15 {
		t.Errorf("second load applied %d and no-opped %d, want 0 and 15", again.Applied, again.DuplicateNoop)
	}
}

// TestVerifyShippedFixtures is FR-048 end to end, minus the goldens, which do not exist until
// the query layer does.
func TestVerifyShippedFixtures(t *testing.T) {
	for _, id := range shipped {
		t.Run(id, func(t *testing.T) {
			ctx := context.Background()
			report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), filepath.Join(fixturesDir, id),
				fixture.VerifyOptions{SkipGoldens: true, Report: true})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}

			if report.FixtureID != id {
				t.Errorf("fixture_id = %q, want %q", report.FixtureID, id)
			}
			assertSteps(t, report, fixture.StepReplay, fixture.StepDoubleDelivery, fixture.StepShuffle, fixture.StepExpectRejected)
			if !report.Passed {
				t.Errorf("verification failed:\n%s", report.Markdown())
			}

			if got := report.Metrics["shuffles"]; got != fixture.DefaultShuffles {
				t.Errorf("shuffles = %v, want the default %d", got, fixture.DefaultShuffles)
			}
			if report.Metrics["ranking"] != nil || report.Metrics["calibration"] != nil {
				t.Error("ranking and calibration must stay nil until the phases that measure them land")
			}

			encoded, err := report.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			if !strings.Contains(string(encoded), `"fixture_id":"`+id+`"`) {
				t.Errorf("JSON report does not name the fixture: %s", encoded)
			}
			if !strings.Contains(report.Markdown(), id) {
				t.Errorf("Markdown report does not name the fixture:\n%s", report.Markdown())
			}
		})
	}
}

// TestVerifyBaselineMetrics checks the --report numbers on the fixture that has something to
// report: four certain-rule merges, one per workload that declares its service name
// (fixtures/README.md, research §10 C2).
func TestVerifyBaselineMetrics(t *testing.T) {
	ctx := context.Background()
	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), filepath.Join(fixturesDir, "baseline-topology-01"),
		fixture.VerifyOptions{SkipGoldens: true, Report: true, Shuffles: -1})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed {
		t.Fatalf("verification failed:\n%s", report.Markdown())
	}

	if got := report.Metrics["events"]; got != 89 {
		t.Errorf("events = %v, want 89", got)
	}
	if got := report.Metrics["applied"]; got != 89 {
		t.Errorf("applied = %v, want 89", got)
	}
	if got := report.Metrics["rejected"]; got != 1 {
		t.Errorf("rejected = %v, want 1", got)
	}
	// The double-delivery step is only meaningful if it counted something.
	double := stepNamed(t, report, fixture.StepDoubleDelivery)
	if !strings.Contains(double.Detail, "89 events re-delivered") {
		t.Errorf("double-delivery detail = %q, want it to report 89 re-deliveries", double.Detail)
	}
	if strings.Contains(double.Detail, "0 entity") {
		t.Errorf("double-delivery detail = %q, want a non-empty graph to have been compared", double.Detail)
	}

	decisions, ok := report.Metrics["decisions_by_rule"].(map[string]int)
	if !ok {
		t.Fatalf("decisions_by_rule = %T, want map[string]int", report.Metrics["decisions_by_rule"])
	}
	if decisions["auto_merge/C2"] != 4 {
		t.Errorf("auto_merge/C2 = %d, want 4 (one per workload declaring its service name)", decisions["auto_merge/C2"])
	}
}

// TestVerifyShufflesDisabled pins the two ends of the Shuffles option. Zero means the documented
// default, so a caller who leaves VerifyOptions empty gets full verification; a negative value is
// the explicit way to skip the step, for a run that only wants replay and idempotency.
func TestVerifyShufflesDisabled(t *testing.T) {
	ctx := context.Background()
	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), filepath.Join(fixturesDir, "late-arriving-fact-01"),
		fixture.VerifyOptions{SkipGoldens: true, Report: true, Shuffles: -1})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed {
		t.Fatalf("verification failed:\n%s", report.Markdown())
	}

	step := stepNamed(t, report, fixture.StepShuffle)
	if !strings.Contains(step.Detail, "skipped") {
		t.Errorf("shuffle detail = %q, want it to say the step was skipped", step.Detail)
	}
	if got := report.Metrics["shuffles"]; got != 0 {
		t.Errorf("shuffles = %v, want 0", got)
	}
}

// TestVerifySeededShufflesAreReproducible checks that a run is worth reproducing: the same seed
// must make the same permutations, or a CI failure could not be chased locally.
func TestVerifySeededShufflesAreReproducible(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(fixturesDir, "retraction-with-edges-01")
	opts := fixture.VerifyOptions{SkipGoldens: true, Shuffles: 2, Seed: 1234}

	first, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), dir, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	second, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), dir, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !first.Passed || !second.Passed {
		t.Fatalf("verification failed:\n%s\n%s", first.Markdown(), second.Markdown())
	}
	if first.Markdown() != second.Markdown() {
		t.Errorf("two runs with the same seed reported differently:\n%s\n%s", first.Markdown(), second.Markdown())
	}
}

// TestVerifyWrongReasonCodeFails is the harness's own negative: a fixture that states the wrong
// reason code for its rejected event must fail verification, and say which code it got. Without
// this, `expect-rejected` passing would prove nothing.
func TestVerifyWrongReasonCodeFails(t *testing.T) {
	ctx := context.Background()
	dir := copyFixture(t, filepath.Join(fixturesDir, "baseline-topology-01"),
		"reason_code: telemetry_payload", "reason_code: missing_valid_time")

	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), dir,
		fixture.VerifyOptions{SkipGoldens: true, Shuffles: -1})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Passed {
		t.Fatalf("verification passed on a fixture that states the wrong reason code:\n%s", report.Markdown())
	}

	step := stepNamed(t, report, fixture.StepExpectRejected)
	if step.Passed {
		t.Errorf("%s passed; it is the step that should have caught this", fixture.StepExpectRejected)
	}
	if !strings.Contains(step.Detail, "missing_valid_time") || !strings.Contains(step.Detail, "telemetry_payload") {
		t.Errorf("detail = %q, want both the stated and the actual reason code", step.Detail)
	}
	// Everything else still held: the mutation is in the manifest, not in the events.
	if s := stepNamed(t, report, fixture.StepReplay); !s.Passed {
		t.Errorf("replay failed too: %s", s.Detail)
	}
	if s := stepNamed(t, report, fixture.StepDoubleDelivery); !s.Passed {
		t.Errorf("double-delivery failed too: %s", s.Detail)
	}
}

// TestVerifyUnexercisedExpectation checks the other half of the expectation contract: a manifest
// that names an event no rejected.jsonl contains must not quietly pass.
func TestVerifyUnexercisedExpectation(t *testing.T) {
	ctx := context.Background()
	dir := copyFixture(t, filepath.Join(fixturesDir, "baseline-topology-01"),
		"event_id: otel:kind:bad-span-props-1", "event_id: otel:demo:never-sent")

	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), dir,
		fixture.VerifyOptions{SkipGoldens: true, Shuffles: -1})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Passed {
		t.Fatalf("verification passed with an expectation nothing exercised:\n%s", report.Markdown())
	}
	if step := stepNamed(t, report, fixture.StepExpectRejected); !strings.Contains(step.Detail, "otel:demo:never-sent") {
		t.Errorf("detail = %q, want it to name the event that was never submitted", step.Detail)
	}
}

func assertSteps(t *testing.T, report *fixture.VerifyReport, want ...string) {
	t.Helper()
	if len(report.Steps) != len(want) {
		t.Fatalf("steps = %d, want %d: %+v", len(report.Steps), len(want), report.Steps)
	}
	for i, name := range want {
		if report.Steps[i].Name != name {
			t.Errorf("step %d = %q, want %q", i, report.Steps[i].Name, name)
		}
		if report.Steps[i].Detail == "" {
			t.Errorf("step %s has no detail; a report nobody can read is not a report", name)
		}
	}
}

func stepNamed(t *testing.T, report *fixture.VerifyReport, name string) fixture.StepResult {
	t.Helper()
	for _, step := range report.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("no %s step in %+v", name, report.Steps)
	return fixture.StepResult{}
}

// copyFixture copies a fixture into a scratch directory, replacing one string in its manifest.
// Mutating a copy rather than the shipped fixture is what lets the harness be tested against a
// fixture that is wrong on purpose.
func copyFixture(t *testing.T, src, old, replacement string) string {
	t.Helper()
	dir := t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if entry.Name() == fixture.ManifestFile {
			mutated := strings.Replace(string(raw), old, replacement, 1)
			if mutated == string(raw) {
				t.Fatalf("manifest does not contain %q, so the mutation would be a no-op", old)
			}
			raw = []byte(mutated)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	return dir
}

// TestReportRendering pins the two shapes the CLI prints. It needs no database: the renderers
// are pure functions of a report.
func TestReportRendering(t *testing.T) {
	t.Parallel()

	report := &fixture.VerifyReport{
		FixtureID: "baseline-topology-01",
		Passed:    false,
		Steps: []fixture.StepResult{
			{Name: fixture.StepReplay, Passed: true, Detail: "63 events applied from empty; goldens skipped (--skip-goldens)"},
			{Name: fixture.StepDoubleDelivery, Passed: true, Detail: "63 events re-delivered, all DUPLICATE_NOOP"},
			{Name: fixture.StepShuffle, Passed: false, Detail: "seed 2 produced a different valid-time state:\n  in order: a\n  shuffled: b"},
			{Name: fixture.StepExpectRejected, Passed: true, Detail: "1 event refused"},
		},
		Metrics: map[string]any{"events": 63, "ranking": nil},
	}

	markdown := report.Markdown()
	if !strings.HasPrefix(markdown, "## baseline-topology-01 — FAILED") {
		t.Errorf("markdown does not open with the verdict:\n%s", markdown)
	}
	for _, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, "| ") && strings.Contains(line, "\n") {
			t.Errorf("a table row carries a newline, which breaks the table: %q", line)
		}
	}
	if !strings.Contains(markdown, "_not measured in this phase_") {
		t.Errorf("a nil metric is not rendered as unmeasured:\n%s", markdown)
	}

	encoded, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	// Canonical: sorted keys, no insignificant whitespace, so a CI diff means a real change.
	want := `{"fixture_id":"baseline-topology-01","metrics":{"events":63,"ranking":null},"passed":false,`
	if !strings.HasPrefix(string(encoded), want) {
		t.Errorf("JSON = %s\nwant it to start with %s", encoded, want)
	}
}

// ---------- T080: the observed-time-pinned pass is mandatory ----------

// unsupportedRunner answers every query with ErrUnsupportedQueryKind.
//
// It is what lets these tests reach the pinned-goldens checks without linking the query engine:
// the verifier only skips the golden comparison when there is no runner at all, so a runner that
// supports nothing still proves that the checks around the comparison fire.
type unsupportedRunner struct{}

func (unsupportedRunner) Run(context.Context, fixture.Query) (proto.Message, error) {
	return nil, fixture.ErrUnsupportedQueryKind
}

func newUnsupportedRunner(*postgres.Store) fixture.QueryRunner { return unsupportedRunner{} }

// copyFixtureFiles copies a fixture's files — manifest, events, rejections — and none of its
// directories, so the copy has no golden/ and no golden/pinned/.
func copyFixtureFiles(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	return dir
}

// TestVerifyRequiresAPinnedGoldenDirectory is T080: a fixture with no golden/pinned/ fails the
// replay step instead of quietly verifying one pass fewer.
func TestVerifyRequiresAPinnedGoldenDirectory(t *testing.T) {
	ctx := context.Background()
	dir := copyFixtureFiles(t, filepath.Join(fixturesDir, "late-arriving-fact-01"))

	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), dir,
		fixture.VerifyOptions{Shuffles: -1, NewRunner: newUnsupportedRunner})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	replay := stepNamed(t, report, fixture.StepReplay)
	if replay.Passed {
		t.Fatalf("the replay step passed without golden/pinned/: %s", replay.Detail)
	}
	if !strings.Contains(replay.Detail, "pinned") {
		t.Errorf("the failure does not name the missing directory: %s", replay.Detail)
	}
	if report.Passed {
		t.Error("the report passed overall")
	}
}

// TestVerifyRequiresAClockEnd is the other half: the pinned pass needs an instant to pin to, so
// a manifest that states no clock end cannot record one and is a failing fixture.
func TestVerifyRequiresAClockEnd(t *testing.T) {
	ctx := context.Background()
	dir := copyFixture(t, filepath.Join(fixturesDir, "late-arriving-fact-01"), "\n  end:", "\n  #end:")

	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t), dir,
		fixture.VerifyOptions{Shuffles: -1, NewRunner: newUnsupportedRunner})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	replay := stepNamed(t, report, fixture.StepReplay)
	if replay.Passed {
		t.Fatalf("the replay step passed with no clock.end: %s", replay.Detail)
	}
	if !strings.Contains(replay.Detail, "clock.end") {
		t.Errorf("the failure does not name the missing clock end: %s", replay.Detail)
	}
}

// TestVerifyShippedFixturesHavePinnedGoldens is the inventory check the task asks for, in both
// directions (004 T153): every query that does NOT state its own observed_at has a pinned twin, and
// every query that DOES has none.
//
// The second half is new. The pinned pass used to re-date every query to clock.end, including queries
// that already state an observed instant — a question nobody wrote, whose goldens were duplicates at
// best and, in T153's case, blank answers that read as coverage. A pinned golden for a self-pinned query
// is now an orphan that verify refuses, and this is the inventory of that rule.
func TestVerifyShippedFixturesHavePinnedGoldens(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixturesDir, err)
	}
	found, pinnedTotal := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// A directory with no manifest is not a fixture: `fixtures/incidents/` is the group
		// holding feature 002's incident fixtures, not a fixture itself.
		if _, err := os.Stat(filepath.Join(fixturesDir, entry.Name(), fixture.ManifestFile)); err != nil {
			continue
		}
		found++
		dir := filepath.Join(fixturesDir, entry.Name())
		m, err := fixture.LoadManifest(dir)
		if err != nil {
			t.Fatalf("%s: LoadManifest: %v", entry.Name(), err)
		}
		pinnedTotal += len(fixture.PinnedQueries(m))
		t.Run(entry.Name(), func(t *testing.T) {
			if m.Clock.End.IsZero() {
				t.Fatal("no clock.end, so the pinned pass has no instant to pin to")
			}
			for _, q := range fixture.PinnedQueries(m) {
				plain := q
				plain.Pinned = false
				plain.ObservedAt = time.Time{}
				// A query whose kind this build cannot answer has neither golden, which is why the
				// check is conditional on the plain one existing.
				if _, err := os.Stat(fixture.GoldenPath(dir, plain)); err != nil {
					continue
				}
				if _, err := os.Stat(fixture.GoldenPath(dir, q)); err != nil {
					t.Errorf("%s.%s states no observed_at and has no pinned twin: %v", q.Kind, q.Name, err)
				}
			}
			for _, q := range m.Queries {
				if q.ObservedAt.IsZero() {
					continue
				}
				q.Pinned = true
				if _, err := os.Stat(fixture.GoldenPath(dir, q)); err == nil {
					t.Errorf("%s.%s states its own observed_at and still has a pinned twin; it is already "+
						"deterministic, and re-dating it to clock.end asks a question nobody wrote (T153)",
						q.Kind, q.Name)
				}
			}
		})
	}
	if found == 0 {
		t.Fatal("no fixtures found")
	}
	if pinnedTotal == 0 {
		t.Fatal("no shipped fixture has a single query to pin, so this inventory checks nothing")
	}
}
