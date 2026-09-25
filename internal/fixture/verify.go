// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	investigationbackend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Verification (FR-048, SC-003, SC-004, SC-009; contracts/fixture-format.md §Verification).
//
// Four checks, in the order the contract states them:
//
//  1. replay — a fresh database, the sources registered, every event applied in file order with
//     its recorded observed time, then every manifest query compared byte for byte with its
//     golden, and a second pass with observed time pinned to clock.end against golden/pinned/;
//  2. double-delivery — every event delivered again; each must be a DUPLICATE_NOOP and the
//     version tables must not grow by a single row;
//  3. shuffle — N seeded permutations within each source's declared reordering window, each
//     into a database of its own, compared structurally (up to entity-id relabeling) with the
//     in-order run;
//  4. expect-rejected — the fixture's must-be-rejected events were refused, for the stated
//     reason, and changed nothing.
//
// Every database comes from the caller's StoreFactory and is dropped again; verification never
// reads or writes a database it was not handed. That is not politeness, it is the only way a
// verifier can be run against a production deployment's server without being a hazard.

// Step names, as they appear in a VerifyReport.
const (
	StepReplay         = "replay"
	StepDoubleDelivery = "double-delivery"
	StepShuffle        = "shuffle"
	StepExpectRejected = "expect-rejected"
	// StepIncident runs only for a fixture carrying an `incident:` block: the ground truth is
	// well formed, the recorded world is the shape the manifest declares, and the trajectory
	// layer is reported present or absent.
	StepIncident = "incident"
	// StepTrajectory replays every file in `trajectories/` (T096, T098). It needs no database
	// and opens no socket, which is what lets the CI gate run it on every pull request in a job
	// with no Postgres and no egress.
	StepTrajectory = "trajectory-replay"
)

// DefaultShuffles is how many seeded permutations the shuffle step runs when VerifyOptions
// leaves Shuffles at zero.
const DefaultShuffles = 6

// QueryRunner answers a manifest query against the store the verifier loaded.
//
// It is implemented by the query layer, which does not exist when the verifier is first
// written. A nil runner makes the verifier skip golden comparison and say so, so that a fixture
// can be verified for replay, idempotency and order-independence before any query exists.
type QueryRunner interface {
	// Run answers q. The returned message is serialized canonically and compared with the
	// query's golden, so it must be the whole response message, not a fragment of it.
	//
	// A kind this build cannot answer is reported by returning an error wrapping
	// ErrUnsupportedQueryKind; the verifier then skips that query instead of failing.
	Run(ctx context.Context, q Query) (proto.Message, error)
}

// ErrUnsupportedQueryKind is what a QueryRunner returns for a manifest query whose engine has
// not landed in this build.
//
// It exists so that a fixture can name the queries it will eventually be verified against
// before every one of them can be answered: the phase that implements a kind records its
// golden and the verifier starts comparing it, while the kinds still outstanding are reported
// as skipped. It is never used for a query that *could* be answered and went wrong — that is a
// failure and is reported as one.
var ErrUnsupportedQueryKind = errors.New("fixture: query kind not supported by this build")

// StoreFactory hands the verifier a fresh, migrated, empty database and the function that
// disposes of it. It is called once for the replay pass and once per shuffle permutation.
type StoreFactory func(ctx context.Context) (store *postgres.Store, cleanup func(), err error)

// VerifyOptions configures a verification run.
type VerifyOptions struct {
	// SkipGoldens runs everything except the golden comparison, for a fixture whose goldens
	// have not been recorded yet.
	SkipGoldens bool
	// Shuffles is how many seeded permutations the shuffle step runs. Zero means
	// DefaultShuffles; a negative value skips the step.
	Shuffles int
	// Seed varies the permutations. The same seed produces the same permutations, so a
	// failure found in CI is reproducible locally.
	Seed int64
	// Report fills VerifyReport.Metrics.
	Report bool
	// NewRunner builds the query runner for a loaded store. Nil — or a nil result — means the
	// goldens are skipped.
	NewRunner func(*postgres.Store) QueryRunner
	// TrajectoryOnly runs the incident and trajectory-replay steps and nothing else.
	//
	// It exists for the CI gate (T098). The 001 steps — replay from empty, double-delivery,
	// shuffle — each need a database of their own, and the trajectory gate must run on every
	// pull request in a job with no Postgres and no network egress. Skipping them here is not a
	// weaker verification: they still run in the `fixtures` job, on the same fixtures, on the
	// same pull request. What this mode removes is the dependency, not the check.
	TrajectoryOnly bool
}

// StepResult is the outcome of one verification step.
type StepResult struct {
	// Name is one of the Step* constants.
	Name string `json:"name"`
	// Passed reports whether the step held.
	Passed bool `json:"passed"`
	// Detail is the human-readable outcome: what was checked when it passed, what differed
	// when it did not.
	Detail string `json:"detail"`
}

// VerifyReport is what a verification run found.
type VerifyReport struct {
	// FixtureID is the manifest's id.
	FixtureID string `json:"fixture_id"`
	// Steps are the step results, in the order the contract lists them.
	Steps []StepResult `json:"steps"`
	// Passed is true only when every step passed.
	Passed bool `json:"passed"`
	// Metrics is filled when VerifyOptions.Report is set. `ranking` and `calibration` are
	// present and null until the phases that compute them land (SC-005, SC-007).
	Metrics map[string]any `json:"metrics,omitempty"`
}

// Verify replays, double-delivers and shuffles the fixture in dir, each into a database from
// newStore, and returns what it found.
//
// A failing check is a StepResult with Passed false, not an error: verification failing is the
// harness working. An error means the run could not be carried out at all — an unreadable
// fixture, a database that could not be created, a query runner that blew up.
func Verify(ctx context.Context, newStore StoreFactory, dir string, opts VerifyOptions) (*VerifyReport, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	if opts.TrajectoryOnly {
		return verifyTrajectoryOnly(ctx, m)
	}
	events, err := ReadEvents(m.EventsPath())
	if err != nil {
		return nil, err
	}
	// The manifest's human decisions are part of the event stream, not something applied around
	// it, so every pass below — replay, double-delivery, shuffle — sees them (FR-040, SC-007).
	events, err = WithHumanDecisions(m, events)
	if err != nil {
		return nil, err
	}

	report := &VerifyReport{FixtureID: m.ID}
	metrics := map[string]any{}

	// A golden that depends on the day it is compared is not a golden. Refused before any
	// database is opened, so the message is the only thing a reader has to act on.
	if names := wallClockDependentQueries(m, events, verifyNow()); len(names) > 0 {
		report.Steps = append(report.Steps, StepResult{
			Name:   StepReplay,
			Passed: false,
			Detail: wallClockDependenceDetail(names, events, verifyNow()),
		})
		return report, nil
	}
	// Likewise refused before any database: a window that ends before the fixture's own events is a
	// mis-declaration that silently empties pinned answers (T153).
	if outside := eventsOutsideClock(m, events); outside != "" {
		report.Steps = append(report.Steps, StepResult{Name: StepReplay, Passed: false, Detail: outside})
		return report, nil
	}

	ordered, rejections, err := verifyInOrder(ctx, newStore, m, events, opts, report, metrics)
	if err != nil {
		return nil, err
	}
	if err := verifyShuffled(ctx, newStore, m, events, opts, report, metrics, ordered); err != nil {
		return nil, err
	}
	// expect-rejected comes last in the report because that is the order the contract lists
	// the checks in, even though the submission happens during the load.
	report.Steps = append(report.Steps, rejections)
	if m.IsIncident() {
		report.Steps = append(report.Steps, verifyIncident(m))
		report.Steps = append(report.Steps, verifyTrajectories(ctx, m))
	}

	report.Passed = true
	for _, step := range report.Steps {
		report.Passed = report.Passed && step.Passed
	}
	if opts.Report {
		if _, measured := metrics["ranking"]; !measured {
			// SC-005, filled by verifyInOrder for a family that names a culprit.
			metrics["ranking"] = nil
		}
		if _, measured := metrics["calibration"]; !measured {
			// SC-007, filled by verifyInOrder for a family whose ground truth labels pairs.
			metrics["calibration"] = nil
		}
		if m.GroundTruth != nil {
			metrics["ground_truth"] = m.GroundTruth
		}
		report.Metrics = metrics
	}
	return report, nil
}

// verifyInOrder runs the replay and double-delivery steps against one store. It returns the
// structural snapshot the shuffle step compares against and the expect-rejected step the load
// produced, which the caller appends after the shuffle.
func verifyInOrder(
	ctx context.Context,
	newStore StoreFactory,
	m *Manifest,
	events []Event,
	opts VerifyOptions,
	report *VerifyReport,
	metrics map[string]any,
) (string, StepResult, error) {
	store, cleanup, err := newStore(ctx)
	if err != nil {
		return "", StepResult{}, fmt.Errorf("fixture: verify %s: open store: %w", m.ID, err)
	}
	defer cleanup()

	p := projector.New(store)
	loaded, err := load(ctx, p, m, events, LoadOptions{})
	if err != nil {
		return "", StepResult{}, err
	}

	replay := StepResult{Name: StepReplay}
	if loaded.Applied == len(events) {
		replay.Passed = true
		replay.Detail = fmt.Sprintf("%d events applied from empty", len(events))
	} else {
		replay.Detail = fmt.Sprintf("%d of %d events applied (%d duplicate, %d rejected); the store was not empty or an event was refused",
			loaded.Applied, len(events), loaded.DuplicateNoop, loaded.Rejected)
	}
	if replay.Passed {
		compared, detail, ok, err := compareGoldens(ctx, m, opts, store)
		if err != nil {
			return "", StepResult{}, err
		}
		replay.Passed = ok
		replay.Detail += "; " + detail
		metrics["queries_compared"] = compared
	}
	report.Steps = append(report.Steps, replay)

	double, snapshotBefore, err := verifyDoubleDelivery(ctx, p, store, events)
	if err != nil {
		return "", StepResult{}, err
	}
	report.Steps = append(report.Steps, double)

	if opts.Report {
		metrics["events"] = len(events)
		metrics["applied"] = loaded.Applied
		metrics["duplicate_noop"] = loaded.DuplicateNoop
		metrics["rejected"] = loaded.Rejected
		metrics["sources"] = len(m.Sources)
		// The family is in the report so that a gate can hold a fixture to what its family
		// promises rather than only to what it happened to measure: a rollout-regression
		// fixture that lost its `ground_truth.culprit_change` would otherwise report no
		// ranking, and "no ranking" reads exactly like "nothing to check"
		// (scripts/check-report.sh).
		metrics["family"] = m.Family
		decisions, err := decisionsByRule(ctx, store)
		if err != nil {
			return "", StepResult{}, err
		}
		metrics["decisions_by_rule"] = decisions

		// Calibration is measured against the graph this run built, after the replay, which is
		// what makes "the human decisions survive a replay" a measurement rather than a claim
		// (SC-007, report.go).
		calibration, err := calibrationReport(ctx, store, m)
		if err != nil {
			return "", StepResult{}, err
		}
		if calibration != nil {
			metrics["calibration"] = calibration
		}

		// SC-021's two cross-source figures, measured over the pairs the manifest labelled (T175).
		// Absent on a fixture that labels none, which is what the gate reads to say the figures held
		// over nothing rather than passed.
		crossSource, err := crossSourceReport(ctx, store, m)
		if err != nil {
			return "", StepResult{}, err
		}
		if crossSource != nil {
			metrics["cross_source"] = crossSource
		}
		// SC-017's second clause over EVERY deploy-claim merge, labelled or not (004 T129).
		deployMerges, err := deployMergeReport(ctx, store)
		if err != nil {
			return "", StepResult{}, err
		}
		if deployMerges != nil {
			metrics["deploy_merges"] = deployMerges
		}

		// The ranking metric is measured against the graph this run loaded, using the same
		// queries the goldens were recorded from (SC-005, report.go).
		if opts.NewRunner != nil {
			if runner := opts.NewRunner(store); runner != nil {
				ranking, err := rankingReport(ctx, runner, m)
				if err != nil {
					return "", StepResult{}, err
				}
				if ranking != nil {
					metrics["ranking"] = ranking
				}
			}
		}
	}
	return snapshotBefore, expectRejectedStep(m, loaded), nil
}

// compareGoldens runs every manifest query and compares it with its recorded output, twice: as
// the manifest states it, and again with observed time pinned to clock.end (US7, FR-015).
//
// The pinned pass is mandatory (T080). "What did the graph know at T?" is the question US7
// exists to answer, and a fixture that does not record the answer is not evidence that the
// graph can answer it — so a manifest with no clock end, a missing golden/pinned/ directory and
// a missing pinned file are all failures of the replay step, not conditions under which the
// pass quietly does not run. Recording them is one `fixture record` away, and the recorder
// creates the directory whenever the manifest has a clock end.
func compareGoldens(ctx context.Context, m *Manifest, opts VerifyOptions, store *postgres.Store) (int, string, bool, error) {
	if opts.SkipGoldens {
		return 0, "goldens skipped (--skip-goldens)", true, nil
	}
	if opts.NewRunner == nil {
		return 0, "goldens skipped (no query runner)", true, nil
	}
	runner := opts.NewRunner(store)
	if runner == nil {
		return 0, "goldens skipped (no query runner)", true, nil
	}

	if m.Clock.End.IsZero() {
		return 0, "the manifest declares no clock.end, so the observed-time-pinned pass has no " +
			"instant to pin to; every fixture must state its clock window (US7, FR-015)", false, nil
	}
	pinned := PinnedQueries(m)
	// The directory is required only when there is something to pin. A fixture whose every query
	// states its own observed_at has no pinned twins at all (T153), and git does not keep an empty
	// directory, so demanding one would fail every such fixture on a fresh clone.
	if len(pinned) > 0 && !pinnedRecorded(m.Dir) {
		return 0, fmt.Sprintf("%s/ does not exist: every manifest query that does not state its own "+
				"observed_at needs an observed-time-pinned twin recorded at clock.end (%s). Run "+
				"`aisre fixture record` (US7, FR-015)",
				filepath.Join(GoldenDir, PinnedDir), m.Clock.End.UTC().Format(time.RFC3339)),
			false, nil
	}
	// A golden nothing compares cannot fail, and so is not evidence of anything (T153).
	orphans, err := orphanGoldens(m)
	if err != nil {
		return 0, "", false, err
	}
	if len(orphans) > 0 {
		return 0, fmt.Sprintf("%d golden file%s no manifest query produces: %s. A golden nothing "+
				"compares looks like coverage and cannot fail; `aisre fixture record` removes them",
				len(orphans), plural(len(orphans), "", "s"), strings.Join(orphans, ", ")),
			false, nil
	}

	queries := make([]Query, 0, len(m.Queries)+len(pinned))
	queries = append(queries, m.Queries...)
	queries = append(queries, pinned...)

	var (
		failures []string
		skipped  []string
	)
	compared := 0
	for _, q := range queries {
		got, err := runner.Run(ctx, q)
		if errors.Is(err, ErrUnsupportedQueryKind) {
			// A kind this build cannot answer is not a verification failure: the fixture is
			// naming a query a later phase will record. It is reported so that "skipped" can
			// never be mistaken for "passed".
			skipped = append(skipped, q.Kind+"."+q.Name)
			continue
		}
		if err != nil {
			return compared, "", false, fmt.Errorf("fixture: %s: run query %s.%s: %w", m.ID, q.Kind, q.Name, err)
		}
		path := GoldenPath(m.Dir, q)
		equal, diff, err := CompareGolden(path, got)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s.%s: %v", q.Kind, q.Name, err))
			continue
		}
		compared++
		if !equal {
			failures = append(failures, fmt.Sprintf("%s.%s differs from %s:\n%s",
				q.Kind, q.Name, filepath.Base(path), diff))
		}
	}
	if len(failures) > 0 {
		return compared, fmt.Sprintf("%d of %d goldens differ:\n%s",
			len(failures), len(queries), strings.Join(failures, "\n")), false, nil
	}
	detail := fmt.Sprintf("%d goldens match", compared)
	if len(skipped) > 0 {
		detail += fmt.Sprintf("; %d skipped (kind not supported by this build: %s)",
			len(skipped), strings.Join(skipped, ", "))
	}
	return compared, detail, true, nil
}

// pinnedRecorded reports whether the fixture has recorded the observed-time-pinned pass. A
// fixture that has not is a failing fixture (T080), not one the pass is skipped for.
func pinnedRecorded(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, GoldenDir, PinnedDir))
	return err == nil && info.IsDir()
}

// verifyDoubleDelivery re-delivers every event and checks nothing moved (FR-020, SC-004). It
// returns the structural snapshot of the loaded graph, which is the same before and after by
// definition of the step passing.
func verifyDoubleDelivery(ctx context.Context, p *projector.Projector, store *postgres.Store, events []Event) (StepResult, string, error) {
	step := StepResult{Name: StepDoubleDelivery}

	before, err := snapshot(ctx, store)
	if err != nil {
		return step, "", err
	}
	entitiesBefore, err := countRows(ctx, store, `SELECT count(*) FROM graph.entity_versions`)
	if err != nil {
		return step, "", err
	}
	edgesBefore, err := countRows(ctx, store, `SELECT count(*) FROM graph.edge_versions`)
	if err != nil {
		return step, "", err
	}

	var problems []string
	for _, event := range events {
		result, err := p.ApplyWithOptions(ctx, event.Envelope, projector.ApplyOptions{
			ObservedAt: event.ObservedAt,
			Principal:  event.principalOr(""),
		})
		if err != nil {
			return step, "", fmt.Errorf("fixture: re-deliver %s: %w", event.Envelope.GetEventId(), err)
		}
		if result.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
			problems = append(problems, fmt.Sprintf("%s: status %s, want DUPLICATE_NOOP",
				event.Envelope.GetEventId(), result.GetStatus()))
		}
	}

	entitiesAfter, err := countRows(ctx, store, `SELECT count(*) FROM graph.entity_versions`)
	if err != nil {
		return step, "", err
	}
	edgesAfter, err := countRows(ctx, store, `SELECT count(*) FROM graph.edge_versions`)
	if err != nil {
		return step, "", err
	}
	if entitiesAfter != entitiesBefore {
		problems = append(problems, fmt.Sprintf("entity_versions grew from %d to %d", entitiesBefore, entitiesAfter))
	}
	if edgesAfter != edgesBefore {
		problems = append(problems, fmt.Sprintf("edge_versions grew from %d to %d", edgesBefore, edgesAfter))
	}
	after, err := snapshot(ctx, store)
	if err != nil {
		return step, "", err
	}
	if after != before {
		problems = append(problems, "valid-time state changed on re-delivery")
	}

	if len(problems) > 0 {
		step.Detail = strings.Join(problems, "; ")
		return step, before, nil
	}
	step.Passed = true
	step.Detail = fmt.Sprintf("%d events re-delivered, all DUPLICATE_NOOP; %d entity and %d edge versions unchanged",
		len(events), entitiesBefore, edgesBefore)
	return step, before, nil
}

// verifyShuffled runs the seeded permutations, each into a database of its own.
func verifyShuffled(
	ctx context.Context,
	newStore StoreFactory,
	m *Manifest,
	events []Event,
	opts VerifyOptions,
	report *VerifyReport,
	metrics map[string]any,
	want string,
) error {
	step := StepResult{Name: StepShuffle}
	shuffles := opts.Shuffles
	if shuffles == 0 {
		shuffles = DefaultShuffles
	}
	if shuffles < 0 {
		step.Passed = true
		step.Detail = "skipped (shuffles disabled)"
		report.Steps = append(report.Steps, step)
		metrics["shuffles"] = 0
		return nil
	}

	windows := m.ReorderingWindows()
	var problems []string
	for seed := range shuffles {
		rng := rand.New(rand.NewPCG(uint64(seed), shuffleSeed+uint64(opts.Seed)))
		shuffled := shuffleWithinWindow(events, windows, rng)

		got, err := applyShuffled(ctx, newStore, m, shuffled)
		if err != nil {
			return err
		}
		if got != want {
			problems = append(problems, fmt.Sprintf("seed %d produced a different valid-time state:\n%s",
				seed, firstDifference(want, got)))
		}
	}

	if len(problems) > 0 {
		step.Detail = strings.Join(problems, "\n")
	} else {
		step.Passed = true
		step.Detail = fmt.Sprintf("%d seeded permutations produced the same valid-time state", shuffles)
	}
	report.Steps = append(report.Steps, step)
	metrics["shuffles"] = shuffles
	return nil
}

// applyShuffled loads one permutation into a database of its own and snapshots it. The store is
// disposed of before the next permutation, so a verification run holds one extra database at a
// time, not one per seed.
func applyShuffled(ctx context.Context, newStore StoreFactory, m *Manifest, shuffled []Event) (string, error) {
	store, cleanup, err := newStore(ctx)
	if err != nil {
		return "", fmt.Errorf("fixture: verify %s: open store: %w", m.ID, err)
	}
	defer cleanup()

	p := projector.New(store)
	if err := registerSources(ctx, p, m); err != nil {
		return "", err
	}
	for _, event := range shuffled {
		result, err := p.ApplyWithOptions(ctx, event.Envelope, projector.ApplyOptions{
			ObservedAt: event.ObservedAt,
			Principal:  event.principalOr(""),
		})
		if err != nil {
			return "", fmt.Errorf("fixture: apply shuffled %s: %w", event.Envelope.GetEventId(), err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			return "", fmt.Errorf("fixture: shuffled delivery of %s: status %s (%s: %s), want APPLIED",
				event.Envelope.GetEventId(), result.GetStatus(), result.GetReasonCode(), result.GetReasonDetail())
		}
	}
	return snapshot(ctx, store)
}

// expectRejectedStep turns the load's rejection findings into a step result.
func expectRejectedStep(m *Manifest, loaded *LoadReport) StepResult {
	step := StepResult{Name: StepExpectRejected}
	if len(loaded.RejectedMismatches) > 0 {
		step.Detail = strings.Join(loaded.RejectedMismatches, "; ")
		return step
	}
	step.Passed = true
	if len(m.ExpectRejected) == 0 {
		step.Detail = "fixture declares no events that must be rejected"
		return step
	}
	step.Detail = fmt.Sprintf("%d events refused with the stated reason code, graph unchanged", len(m.ExpectRejected))
	return step
}

// firstDifference renders the first line the two snapshots disagree on, which is enough to
// point at the offending entity without printing two whole graphs.
func firstDifference(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(wantLines), len(gotLines)) {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("  in order: %s\n  shuffled: %s", w, g)
		}
	}
	return "  (snapshots differ but no line does; this is a bug in the verifier)"
}

// decisionsByRule counts the resolution decisions the load produced, keyed `<kind>/<rule>`
// (constitution VI: every merge records the rule that produced it).
func decisionsByRule(ctx context.Context, store *postgres.Store) (map[string]int, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT kind, coalesce(rule_id, 'none'), count(*)
		FROM graph.resolution_decisions GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("fixture: count resolution decisions: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var (
			kind, rule string
			count      int
		)
		if err := rows.Scan(&kind, &rule, &count); err != nil {
			return nil, fmt.Errorf("fixture: count resolution decisions: %w", err)
		}
		counts[kind+"/"+rule] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixture: count resolution decisions: %w", err)
	}
	return counts, nil
}

// Markdown renders the report as a human summary, for a terminal or a pull request comment.
func (r *VerifyReport) Markdown() string {
	var out strings.Builder
	status := "FAILED"
	if r.Passed {
		status = "passed"
	}
	fmt.Fprintf(&out, "## %s — %s\n\n", r.FixtureID, status)
	out.WriteString("| step | result | detail |\n|---|---|---|\n")
	for _, step := range r.Steps {
		result := "fail"
		if step.Passed {
			result = "ok"
		}
		// Table cells cannot hold newlines or bare pipes; a multi-line diff is folded onto
		// one line so the table stays a table.
		detail := strings.ReplaceAll(strings.TrimSpace(step.Detail), "|", "\\|")
		detail = strings.ReplaceAll(detail, "\n", " ⏎ ")
		fmt.Fprintf(&out, "| %s | %s | %s |\n", step.Name, result, detail)
	}
	if len(r.Metrics) > 0 {
		out.WriteString("\n### Metrics\n\n")
		for _, key := range sortedKeys(r.Metrics) {
			// The ranking metric is the headline of a rollout-regression report and gets a
			// section of its own below; a JSON blob in a bullet list would bury it.
			if key == "ranking" && r.rankingMeasured() {
				continue
			}
			// Same for calibration: it gets a section of its own below.
			if key == "calibration" && r.calibrationMeasured() {
				continue
			}
			fmt.Fprintf(&out, "- **%s**: %s\n", key, renderMetric(r.Metrics[key]))
		}
	}
	if r.rankingMeasured() {
		ranking, _ := r.Metrics["ranking"].(map[string]any)
		out.WriteString(rankingMarkdown(ranking))
	}
	if r.calibrationMeasured() {
		calibration, _ := r.Metrics["calibration"].(map[string]any)
		out.WriteString(calibrationMarkdown(calibration))
	}
	return out.String()
}

// calibrationMeasured reports whether this run measured resolution calibration, as opposed to
// carrying the placeholder a fixture without labelled pairs gets.
func (r *VerifyReport) calibrationMeasured() bool {
	calibration, ok := r.Metrics["calibration"].(map[string]any)
	return ok && len(calibration) > 0
}

// rankingMeasured reports whether this run actually measured a ranking, as opposed to carrying
// the placeholder a fixture without ground truth gets.
func (r *VerifyReport) rankingMeasured() bool {
	ranking, ok := r.Metrics["ranking"].(map[string]any)
	return ok && len(ranking) > 0
}

func renderMetric(value any) string {
	if value == nil {
		return "_not measured in this phase_"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

// JSON renders the report as canonical JSON, so two runs of the same fixture produce the same
// bytes and a CI diff means something changed.
func (r *VerifyReport) JSON() ([]byte, error) {
	return graph.CanonicalJSON(r)
}

// verifyIncident is the incident-fixture step (002 contracts/incident-format.md §"What
// `fixture verify` does with it", FR-063).
//
// It checks the three things that can be checked without running an investigation:
//
//  1. the ground truth is well formed — LoadManifest already refused a malformed one, so
//     reaching here at all is the assertion, and the step reports what it holds so a reviewer
//     reads the class, the category and the decoy count off the verification output;
//  2. the recorded world is the shape the manifest declares. A world re-recorded at a wider hop
//     radius or a different grid would lower the miss rate with nothing in the diff to say why,
//     which is the failure `fixture record-world` exists to prevent; this is the check that
//     makes it stick;
//  3. the trajectory layer is **reported, never required**. A fixture authored before anybody
//     ran it has none, so an absent `trajectories/` is named as absent and the step still
//     passes. Replaying the ones that exist is the next step's job (StepTrajectory), kept
//     separate so that this one stays a statement about the fixture's shape.
func verifyIncident(m *Manifest) StepResult {
	incident := m.Incident
	truth := incident.GroundTruth

	notes := []string{fmt.Sprintf("ground truth %s", truth.Class())}
	if category := truth.CauseCategory(); category != "" {
		notes = append(notes, "category "+category)
	}
	notes = append(notes,
		fmt.Sprintf("%d causal step(s)", len(truth.CausalPath)),
		fmt.Sprintf("%d decisive predicate(s)", len(truth.DecisiveEvidence)),
		fmt.Sprintf("%d decoy(s)", len(truth.Decoys)),
		"provenance "+truth.Provenance.Kind)

	index, present, err := ReadWorldIndex(m.Dir)
	switch {
	case err != nil:
		return StepResult{Name: StepIncident, Passed: false, Detail: err.Error()}
	case !present:
		notes = append(notes, "world/: absent (nothing to replay a telemetry question from)")
	default:
		if mismatch := worldMatchesManifest(incident.World, index); mismatch != "" {
			return StepResult{Name: StepIncident, Passed: false, Detail: mismatch}
		}
		if index.MissRate > incident.World.MissRateThreshold {
			return StepResult{Name: StepIncident, Passed: false, Detail: fmt.Sprintf(
				"world/ miss rate %.4f is above the fixture's own threshold %.4f; the fixture is "+
					"insufficient and is not scored (contracts/incident-format.md)",
				index.MissRate, incident.World.MissRateThreshold)}
		}
		notes = append(notes, fmt.Sprintf(
			"world/: %d term(s), %d not_recorded, miss rate %.4f ≤ %.4f, hops %d, drill-down depth %d",
			index.TermCount, index.NotRecordedCount, index.MissRate,
			incident.World.MissRateThreshold, index.HopRadius, index.DrillDownDepth))
	}

	// A generated manifest (`investigate to-incident`) leaves the fields no command can derive
	// as marked placeholders. They are reported, never failed on: a fixture in progress is a
	// fixture in progress, and what would be a defect is one that looked finished (FR-055).
	placeholders, err := PlaceholderFields(m.Dir)
	if err != nil {
		return StepResult{Name: StepIncident, Passed: false, Detail: err.Error()}
	}
	if len(placeholders) > 0 {
		notes = append(notes, fmt.Sprintf("%d field(s) await a reviewer (%s): %s",
			len(placeholders), PlaceholderMarker, strings.Join(placeholders, ", ")))
	}

	trajectories, err := TrajectoryFiles(m.Dir)
	if err != nil {
		return StepResult{Name: StepIncident, Passed: false, Detail: err.Error()}
	}
	if len(trajectories) == 0 {
		notes = append(notes, "trajectories/: absent — recorded with `fixture record-trajectory`")
	} else {
		notes = append(notes, fmt.Sprintf("trajectories/: %d run(s) present; the trajectory-replay "+
			"step below is what replays them", len(trajectories)))
	}

	return StepResult{Name: StepIncident, Passed: true, Detail: strings.Join(notes, "; ")}
}

// worldMatchesManifest reports the first way a recorded world disagrees with the shape its
// fixture declares, or "" when they agree. The window grid is compared by length only: the
// index writes each pair as the recorder serialized it and re-parsing that here would be a
// second implementation of the same encoding, while a grid of a different size is the change
// that actually moves a miss rate.
func worldMatchesManifest(declared IncidentWorld, index *WorldIndex) string {
	switch {
	case index.AlgebraVersion != declared.AlgebraVersion:
		return fmt.Sprintf("world/index.json was recorded under algebra version %q but the manifest "+
			"declares %q; every term key moves between versions",
			index.AlgebraVersion, declared.AlgebraVersion)
	case int(index.HopRadius) != declared.HopRadius:
		return fmt.Sprintf("world/index.json was recorded at hop radius %d but the manifest declares %d",
			index.HopRadius, declared.HopRadius)
	case int(index.DrillDownDepth) != declared.DrillDownDepth:
		return fmt.Sprintf("world/index.json was recorded at drill-down depth %d but the manifest "+
			"declares %d", index.DrillDownDepth, declared.DrillDownDepth)
	case int(index.TermCount) != len(index.TermKeyToFile):
		return fmt.Sprintf("world/index.json says %d terms but names %d files",
			index.TermCount, len(index.TermKeyToFile))
	}
	return ""
}

// verifyTrajectoryOnly is the CI gate's verification (T096, T098): the incident block and the
// trajectory replay, and nothing that needs a database.
//
// A fixture with no `incident:` block passes trivially and says so. That is deliberate: the gate
// is pointed at `fixtures/incidents/`, but pointing it at the whole corpus should report "nothing
// to replay here" rather than fail, so that adding a 001 fixture never breaks the 002 gate.
func verifyTrajectoryOnly(ctx context.Context, m *Manifest) (*VerifyReport, error) {
	report := &VerifyReport{FixtureID: m.ID, Passed: true}
	if !m.IsIncident() {
		report.Steps = append(report.Steps, StepResult{
			Name: StepTrajectory, Passed: true,
			Detail: "not an incident fixture: no `incident:` block, so no trajectory to replay",
		})
		return report, nil
	}
	report.Steps = append(report.Steps, verifyIncident(m), verifyTrajectories(ctx, m))
	for _, step := range report.Steps {
		report.Passed = report.Passed && step.Passed
	}
	return report, nil
}

// verifyTrajectories replays every file in `trajectories/` (T096, FR-039, FR-040, FR-041).
//
// Every recorded run is put back through the seams it came out of — the strict replaying model
// transport, the published algebra keying, the recorded world beside it, the published
// likelihood-ratio table — and must reproduce itself exactly. A divergence names the first
// diverging record; an unmatched worker request names the worker, the capability and the
// parameters. Nothing opens a socket and nothing needs a database.
//
// An absent `trajectories/` is reported and passes: trajectories are recorded from a run, and a
// fixture authored before anyone has run it is a normal state of the corpus. What is *not*
// tolerated is a trajectory that is there and does not replay.
func verifyTrajectories(ctx context.Context, m *Manifest) StepResult {
	files, err := replay.Files(m.Dir)
	if err != nil {
		return StepResult{Name: StepTrajectory, Passed: false, Detail: err.Error()}
	}
	if len(files) == 0 {
		return StepResult{Name: StepTrajectory, Passed: true, Detail: "trajectories/: absent — " +
			"recorded from a run with `fixture record-trajectory`; there is nothing to replay yet"}
	}

	world, err := loadWorldFor(m)
	if err != nil {
		return StepResult{Name: StepTrajectory, Passed: false, Detail: err.Error()}
	}

	var (
		notes   []string
		checked int
		missed  int
	)
	for _, path := range files {
		result, err := replay.ReplayFile(ctx, path, world)
		if err != nil {
			return StepResult{Name: StepTrajectory, Passed: false,
				Detail: fmt.Sprintf("%s: %v", filepath.Base(path), err)}
		}
		if !result.Identical {
			return StepResult{Name: StepTrajectory, Passed: false,
				Detail: fmt.Sprintf("%s: %s", filepath.Base(path), result.Divergence.Error())}
		}
		checked += result.WorldChecked
		missed += result.NotRecorded
		notes = append(notes, fmt.Sprintf(
			"%s: identical — %d records, %d model exchange(s), %d worker call(s), %d ledger update(s), "+
				"%d not_recorded of %d checked against world/",
			result.RunID, result.Records, result.ModelExchanges, result.WorkerCalls,
			result.LedgerUpdates, result.NotRecorded, result.WorldChecked))
	}
	rate := 0.0
	if checked > 0 {
		rate = float64(missed) / float64(checked)
	}
	notes = append(notes, fmt.Sprintf("%d trajectory(ies) replayed with zero network; miss rate %.4f",
		len(files), rate))
	return StepResult{Name: StepTrajectory, Passed: true, Detail: strings.Join(notes, "; ")}
}

// loadWorldFor opens the fixture's recorded world, when it has one. A fixture with no world still
// replays its trajectory — the recorded answers are in the trajectory — it simply has nothing to
// cross-check them against.
func loadWorldFor(m *Manifest) (*investigationbackend.Recorded, error) {
	if _, err := os.Stat(filepath.Join(m.WorldDir(), "index.json")); err != nil {
		return nil, nil //nolint:nilnil // "there is no world here" is not an error
	}
	world, err := investigationbackend.NewRecorded(m.WorldDir())
	if err != nil {
		return nil, fmt.Errorf("fixture: %s: load the recorded world: %w", m.ID, err)
	}
	return world, nil
}
