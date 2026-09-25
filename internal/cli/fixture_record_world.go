// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	algebra "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	graphworker "github.com/Pierre-Theophile/aisre/internal/investigation/workers/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// `fixture record-world` (tasks.md T039, contracts/cli.md §Fixtures and evaluation, FR-042b).
//
// This is the fixture-scoped wrapper around `worker record`, and the difference between the two
// is the entire point of having both.
//
// `worker record` takes a focus, a window, a hop radius and a grid **from flags**, and is how a
// world is first recorded. `fixture record-world` takes all four **from the fixture's own
// manifest**, so that a re-record cannot silently change a fixture's shape. A world recorded at
// hop radius 2 and re-recorded at hop radius 3 is a different fixture wearing the same name: the
// miss rate would fall, the corpus would look healthier, and nothing in the diff would say why.
// Reading the shape from the manifest makes changing it an edit a reviewer sees.
//
// The fixture is replayed into a database of its own first, exactly as `fixture record` does, so
// the world is recorded against the graph the fixture's own `events.jsonl` produces and not
// against whatever happens to be in a shared database.

// incidentBlock is the `incident:` block of a fixture manifest, as much of it as recording needs.
// It is parsed here rather than added to internal/fixture's Manifest struct because the rest of
// the block — the ground truth, the runs — belongs to the evaluation harness (T097 onward), and
// this command needs only the world's declared shape.
type incidentBlock struct {
	Incident struct {
		Question struct {
			Subject  string    `yaml:"subject"`
			FiredAt  time.Time `yaml:"fired_at"`
			Lookback string    `yaml:"lookback"`
		} `yaml:"question"`
		GroundTruth struct {
			Culprit string `yaml:"culprit"`
		} `yaml:"ground_truth"`
		World struct {
			HopRadius      int    `yaml:"hop_radius"`
			DrillDownDepth int    `yaml:"drill_down_depth"`
			AlgebraVersion string `yaml:"algebra_version"`
			Shape          string `yaml:"shape"`
			WindowGrid     []struct {
				ReferenceAt  time.Time `yaml:"reference_at"`
				WidthSeconds int       `yaml:"width_seconds"`
			} `yaml:"window_grid"`
			MissRateThreshold float64 `yaml:"miss_rate_threshold"`
		} `yaml:"world"`
	} `yaml:"incident"`
}

// fixtureGroundTruth is the 001-shaped `ground_truth:` mapping a hand-authored fixture carries.
// `rollout-regression-01` names its culprit there, and a fixture that names one there and not in
// an `incident:` block is still a fixture whose world can be recorded honestly.
type fixtureGroundTruth struct {
	GroundTruth struct {
		CulpritChange string `yaml:"culprit_change"`
	} `yaml:"ground_truth"`
	Clock struct {
		Start time.Time `yaml:"start"`
		End   time.Time `yaml:"end"`
	} `yaml:"clock"`
}

func newFixtureRecordWorldCommand(global *globalOptions) *cobra.Command {
	var dsn string
	var auditPath string
	var livePasses int
	var modelYAML string
	var pricesYAML string

	cmd := &cobra.Command{
		Use:   "record-world <dir>...",
		Short: "Re-record the world/ layer of a fixture from that fixture's own manifest",
		Long: "record-world replays the fixture into a database of its own and re-records its `world/`\n" +
			"layer: the cross product of the telemetry algebra over the neighbourhood and the window\n" +
			"grid, plus the depth-1 drill-downs.\n\n" +
			"The focus, window grid, hop radius and drill-down depth come from the fixture's own\n" +
			"manifest rather than from flags, so a re-record cannot silently change the fixture's\n" +
			"shape — a world re-recorded at a wider hop radius would lower the miss rate and nothing\n" +
			"in the diff would say why. A re-record of an unchanged fixture is byte-identical.\n\n" +
			"Each fixture gets a database of its own, so the role in --db needs CREATEDB.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := dsnFrom(dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			prior, err := loadPrior(auditPath)
			if err != nil {
				return err
			}
			if livePasses < 0 {
				return exitErrorf(ExitUsage, "--live-passes cannot be negative")
			}
			// A live pass that cannot resolve a credential is refused here rather than halfway
			// through a recording, and the refusal names the variable.
			if livePasses > 0 {
				ok, missing, err := eval.LiveAvailable(modelYAML)
				if err != nil {
					return exitWith(ExitUsage, err)
				}
				if !ok {
					return exitErrorf(ExitUsage,
						"--live-passes needs a credential for every provider %s configures; $%s is not set",
						modelYAML, strings.Join(missing, ", $"))
				}
			}
			opts := worldRecordOptions{
				livePasses: livePasses,
				modelYAML:  modelYAML,
				pricesYAML: pricesYAML,
			}
			newStore := fixture.NewStoreFactoryFromDSN(resolved)
			for _, dir := range args {
				report, err := recordFixtureWorld(cmd.Context(), newStore, dir, prior, opts)
				if err != nil {
					return exitWith(ExitTransport, err)
				}
				if err := renderWorldReport(cmd, global, report); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	cmd.Flags().StringVar(&auditPath, "audit", defaultAuditPath,
		"the published coverage audit the engine pass reads π₀ from, so it plans the investigation "+
			"the corpus will replay rather than a differently-prioritised one")
	cmd.Flags().IntVar(&livePasses, "live-passes", 0,
		"after the deterministic pass and the grid, run the live engine this many times and record "+
			"every term it asks. It costs money and opens a socket; 0 records exactly the bytes it "+
			"recorded before")
	cmd.Flags().StringVar(&modelYAML, "model-config", eval.DefaultModelYAML,
		"the model configuration the live passes run under; its digest is written into world/index.json")
	cmd.Flags().StringVar(&pricesYAML, "prices", eval.DefaultPricesYAML, "the price table")
	return cmd
}

// worldRecordOptions is what `record-world` was asked for beyond the fixture's own manifest. The
// shape of the world still comes from the manifest — these say how hard to look for the questions
// a live investigator would put, and under which configuration.
type worldRecordOptions struct {
	livePasses int
	modelYAML  string
	pricesYAML string
}

// recordFixtureWorld replays one fixture and re-records its world.
func recordFixtureWorld(
	ctx context.Context, newStore fixture.StoreFactory, dir string, prior audit.PriorRecord, opts worldRecordOptions,
) (*worldReport, error) {
	manifest, err := fixture.LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	spec, err := worldSpecFromManifest(dir, manifest)
	if err != nil {
		return nil, err
	}

	events, err := fixture.ReadEvents(manifest.EventsPath())
	if err != nil {
		return nil, err
	}
	events, err = fixture.WithHumanDecisions(manifest, events)
	if err != nil {
		return nil, err
	}

	store, cleanup, err := newStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("fixture: record-world %s: open store: %w", manifest.ID, err)
	}
	defer cleanup()

	loaded, err := fixture.Load(ctx, projector.New(store), dir, fixture.LoadOptions{})
	if err != nil {
		return nil, err
	}
	if loaded.Applied != len(events) {
		return nil, fmt.Errorf(
			"fixture: record-world %s: %d of %d events applied (%d duplicate, %d rejected); a world recorded from an incomplete graph would describe a topology that never existed",
			manifest.ID, loaded.Applied, len(events), loaded.DuplicateNoop, loaded.Rejected)
	}

	queryEngine := query.NewEngine(store)
	if manifest.IsIncident() {
		// The engine pass. It runs the *same* wiring `fixture record-trajectory --model-free`
		// runs, over the same graph, against the world's own generator — so every term the
		// deterministic engine will ask on replay is in the recording by construction rather
		// than by a derivation the recorder happens to share with it.
		spec.Plan = fixtureEnginePass(manifest, prior, queryEngine)
		if opts.livePasses > 0 {
			digest, _, err := eval.ModelConfiguration(opts.modelYAML)
			if err != nil {
				return nil, err
			}
			spec.Live = fixtureLivePass(manifest, prior, queryEngine, opts)
			spec.LivePasses = opts.livePasses
			spec.ModelConfigDigest = digest
		}
	}
	return recordWorld(ctx, queryEngine, spec)
}

// fixtureEnginePass runs the fixture's own investigation, model-free, against the world's
// generator, recording every telemetry term it issues.
//
// The engine it builds is `internal/eval`'s, which is the same builder `fixture
// record-trajectory` and the world-replay gate use. That is the whole point: a recorder wiring
// its own engine would record a world for an investigation the corpus never replays.
//
// Two properties make this worth doing rather than widening the cross product until it happens to
// contain the right windows. It is **exact**: the recorded set is the issued set, so no derivation
// can drift out from under it. And it is **self-limiting**: the engine asks what an investigation
// asks, which is tens of terms, not the hundreds a grid wide enough to contain them by luck would
// produce.
//
// The graph family never reaches the recorder — graph answers come from replaying `events.jsonl`
// and recording them would be a second source of truth for a deterministic answer — so the graph
// worker is wired to the live query engine and only the three telemetry workers see the wrapper.
func fixtureEnginePass(m *fixture.Manifest, prior audit.PriorRecord, queryEngine *query.Engine) worldEnginePass {
	return func(ctx context.Context, backend *synthetic.Backend, recorder sdk.Recorder) (*enginePassReport, error) {
		taping := newRecordingBackend(backend, recorder, m.Incident.Question.FiredAt.UTC())
		wiring := eval.EngineWiring{
			Manifest:  m,
			Prior:     prior,
			Graph:     graphworker.NewEngineService(queryEngine),
			Telemetry: taping,
			Kind:      eval.KindModelFree,
			// The pass records what a *live* backend answered: these bytes become the recording,
			// and stamping them `recorded` before anything had recorded them would be a claim
			// about provenance that is not true yet.
			Mode: worker.ModeLive,
		}
		stop, err := runRecordingPass(ctx, m, wiring)
		if err != nil {
			return nil, err
		}
		return taping.report(stop), nil
	}
}

// fixtureLivePass runs the *live* engine — the production model configuration, real provider
// calls — against the world's own generator, `opts.livePasses` times, recording every term it
// issues (Phase 9 Track M).
//
// The deterministic pass above records what the engine's published plan derives from the
// question. That is a lower bound on what an investigation asks: a model chasing a hypothesis
// reaches for a statistic nobody derived, a pointer one hop further out, a log pattern on a
// neighbour. The first live evaluation of this corpus excluded 15 of 16 fixtures on their miss
// rate for precisely that reason — 0.111 to 0.667 against a 0.05 bar — and none of those misses
// was a defect in the model or in the engine. They were questions the recording did not hold.
//
// The generator can answer any in-algebra term, so the honest fix is to record what a live run
// asks rather than to widen the admission bar until the corpus passes. Nothing about the
// recording depends on what the model *said*: the model chooses which questions get asked, and
// the generator — deterministic, seeded by the fixture — answers every one of them. That is why
// a world recorded this way still re-records byte-identically with no model at all.
//
// Several passes rather than one because a live model is not a function: two runs of the same
// investigation take different paths, and the union of a handful of them is a far better estimate
// of what the corpus will ask than any single run.
func fixtureLivePass(
	m *fixture.Manifest, prior audit.PriorRecord, queryEngine *query.Engine, opts worldRecordOptions,
) worldEnginePass {
	return func(ctx context.Context, backend *synthetic.Backend, recorder sdk.Recorder) (*enginePassReport, error) {
		taping := newRecordingBackend(backend, recorder, m.Incident.Question.FiredAt.UTC())
		var stop string
		for pass := range opts.livePasses {
			wiring := eval.EngineWiring{
				Manifest:   m,
				Prior:      prior,
				Graph:      graphworker.NewEngineService(queryEngine),
				Telemetry:  taping,
				Kind:       eval.KindLive,
				ModelYAML:  opts.modelYAML,
				PricesYAML: opts.pricesYAML,
				Mode:       worker.ModeLive,
				// One investigation id across the passes, deliberately: the id is part of the
				// prompt prefix, and a prefix that changed per pass would defeat the provider's
				// cache on every one of them.
				InvestigationID: "inv-" + m.ID + "-live-world",
			}
			last, err := runRecordingPass(ctx, m, wiring)
			if err != nil {
				return nil, fmt.Errorf("fixture: record-world %s: live pass %d of %d: %w",
					m.ID, pass+1, opts.livePasses, err)
			}
			stop = last
		}
		return taping.report(stop), nil
	}
}

// runRecordingPass builds one engine for a recording pass and runs it to a stop.
func runRecordingPass(ctx context.Context, m *fixture.Manifest, wiring eval.EngineWiring) (string, error) {
	e, err := eval.NewEngine(wiring)
	if err != nil {
		return "", fmt.Errorf("fixture: record-world %s: build the engine: %w", m.ID, err)
	}
	stop, err := e.Run(ctx)
	if err != nil {
		return "", fmt.Errorf("fixture: record-world %s: the engine pass failed: %w", m.ID, err)
	}
	return string(stop.Reason), nil
}

// recordingBackend answers from one telemetry backend and files every answer into the world being
// recorded.
//
// It is the seam that makes "the world holds what the engine asked" true by construction. The
// recorder is a map keyed by the canonicalised term, so a term the engine asks twice is filed
// once and a term two candidates share costs nothing; recording the same key with a different
// answer is refused by the recorder itself, which is the check that the generator is
// deterministic.
type recordingBackend struct {
	inner    sdk.TelemetryBackend
	recorder sdk.Recorder
	// horizon is the investigation's observed_at. It is carried so the wrapper can *assert* the
	// clamp rather than assume it: the backend already cuts every window back to the horizon,
	// and a term that still ran past it after clamping would be a future question filed under a
	// key no replay can ever ask for.
	horizon time.Time

	mu      sync.Mutex
	keys    map[string]struct{}
	handles []*investigationv1.Handle
}

func newRecordingBackend(inner sdk.TelemetryBackend, recorder sdk.Recorder, horizon time.Time) *recordingBackend {
	return &recordingBackend{inner: inner, recorder: recorder, horizon: horizon, keys: map[string]struct{}{}}
}

var _ sdk.TelemetryBackend = (*recordingBackend)(nil)

// Describe passes the wrapped backend's contract through unchanged: the wrapper adds recording,
// not capability, and a worker must not be able to tell it is there.
func (b *recordingBackend) Describe() sdk.Description { return b.inner.Describe() }

// Execute answers the term and records the answer.
func (b *recordingBackend) Execute(ctx context.Context, req *algebra.Request) (*algebra.Response, error) {
	resp, err := b.inner.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	// A NOT_RECORDED answer is what a *world* says about a term it does not hold and is never a
	// term it holds, so it is not filed. The generator does not produce one; the guard is here so
	// that a backend that did would fail the recording loudly rather than write a hole into it.
	if resp.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
		return nil, fmt.Errorf(
			"fixture: record-world: the generator answered NOT_RECORDED for %s; a world cannot record "+
				"the absence of an answer as an answer", algebra.TermName(req.GetTerm()))
	}
	// The world is keyed by the term that was actually answerable, not by the term as asked:
	// a window running past the investigation's observed_at was cut back to it by the backend
	// (constitution II), and filing the answer under the unclamped key would put a future window
	// into the recording and make every replay miss. A window lying entirely past the horizon is
	// not filed at all — the recorded backend derives that NO_DATA from the horizon itself.
	clampedReq, _, horizonState := algebra.ClampRequest(req)
	if horizonState == algebra.PastHorizon {
		return resp, nil
	}
	// The assert. Clamping is idempotent, so a clamped term that is not WithinHorizon is a term
	// the horizon rule would refuse, and filing it would put a question about the future into a
	// recording that claims to hold only what was observable.
	if _, state := algebra.ClampTerm(clampedReq.GetTerm(), b.horizon); !b.horizon.IsZero() && state != algebra.WithinHorizon {
		return nil, fmt.Errorf(
			"fixture: record-world: %s survived clamping to the horizon %s as %v; a world must not hold a term the horizon rule refuses",
			algebra.TermName(clampedReq.GetTerm()), b.horizon.Format(time.RFC3339), state)
	}
	key, err := algebra.TermKey(clampedReq.GetTerm())
	if err != nil {
		return nil, err
	}
	if err := b.recorder.Record(ctx, clampedReq, resp); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.keys == nil {
		b.keys = map[string]struct{}{}
	}
	b.keys[key] = struct{}{}
	b.handles = append(b.handles, handlesOf(resp)...)
	return resp, nil
}

// report is what this wrapper filed, for the recording's own report and for the index's live
// coverage. The keys are sorted, so a world's provenance does not depend on the order a
// parallel first wave happened to answer in.
func (b *recordingBackend) report(stop string) *enginePassReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.keys))
	for key := range b.keys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return &enginePassReport{
		Terms:   len(keys),
		Keys:    keys,
		Handles: append([]*investigationv1.Handle(nil), b.handles...),
		Stop:    stop,
	}
}

// worldSpecFromManifest reads the fixture's declared shape. Everything it needs is in the
// manifest, and where the manifest is silent the rule is published here rather than guessed at
// per fixture.
func worldSpecFromManifest(dir string, manifest *fixture.Manifest) (worldSpec, error) {
	var spec worldSpec

	raw, err := os.ReadFile(filepath.Join(dir, fixture.ManifestFile))
	if err != nil {
		return spec, fmt.Errorf("fixture: record-world: read manifest: %w", err)
	}
	var incident incidentBlock
	if err := yaml.Unmarshal(raw, &incident); err != nil {
		return spec, fmt.Errorf("fixture: record-world: parse incident block: %w", err)
	}
	var legacy fixtureGroundTruth
	if err := yaml.Unmarshal(raw, &legacy); err != nil {
		return spec, fmt.Errorf("fixture: record-world: parse ground truth: %w", err)
	}

	focusRef := incident.Incident.Question.Subject
	if focusRef == "" {
		// A fixture with no `incident:` block yet — every 001 fixture, today — takes its focus
		// from the first query the manifest lists. That is the node the fixture is *about*, and
		// using it keeps the world aligned with the goldens rather than with a flag somebody
		// typed once.
		for _, q := range manifest.Queries {
			if q.Focus != "" {
				focusRef = q.Focus
				break
			}
		}
	}
	if focusRef == "" {
		return spec, fmt.Errorf(
			"fixture %s declares neither incident.question.subject nor a query with a focus; there is no node to take a neighbourhood around", manifest.ID)
	}
	focus, err := graph.ParseRef(focusRef)
	if err != nil {
		return spec, fmt.Errorf("fixture %s: focus %q: %w", manifest.ID, focusRef, err)
	}

	from, to := legacy.Clock.Start, legacy.Clock.End
	if from.IsZero() || to.IsZero() {
		from, to = manifest.Clock.Start, manifest.Clock.End
	}
	// The reference instant is the instant the fixture is *asked about*, not the end of its
	// clock. For an incident fixture that is the alert's `fired_at`; for a 001 fixture with no
	// incident block it is the latest instant any of its queries names, which is the instant its
	// goldens were recorded at. Centring the window grid anywhere else would record a world
	// whose symptom window sits after everything the fixture describes.
	if asked := latestQueryInstant(manifest); !asked.IsZero() && asked.After(from) {
		to = asked
	}
	if firedAt := incident.Incident.Question.FiredAt; !firedAt.IsZero() {
		to = firedAt
		if lookback, err := time.ParseDuration(incident.Incident.Question.Lookback); err == nil && lookback > 0 {
			from = firedAt.Add(-lookback)
		}
	}
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return spec, fmt.Errorf(
			"fixture %s declares no usable window: the clock must have a start and an end, or the incident block a fired_at and a lookback", manifest.ID)
	}

	hops := incident.Incident.World.HopRadius
	if hops == 0 {
		// The published default, and the radius every query in this repository's fixtures uses.
		// It is recorded in the index either way, so a fixture that takes the default still
		// states it.
		hops = 2
		for _, q := range manifest.Queries {
			if q.Hops > 0 {
				hops = q.Hops
				break
			}
		}
	}

	grid := make([]*investigationv1.WindowPair, 0, len(incident.Incident.World.WindowGrid))
	for _, entry := range incident.Incident.World.WindowGrid {
		at := entry.ReferenceAt
		if at.IsZero() {
			at = to
		}
		if entry.WidthSeconds <= 0 {
			return spec, fmt.Errorf("fixture %s: a window_grid entry has no positive width_seconds", manifest.ID)
		}
		grid = append(grid, algebra.NewWindowPair(at, time.Duration(entry.WidthSeconds)*time.Second))
	}
	if len(grid) == 0 {
		// The published default grid: a fifteen-minute and a one-hour pair around the window's
		// end. It is written into the index, so a fixture that takes the default still states
		// what it holds.
		grid = []*investigationv1.WindowPair{
			algebra.NewWindowPair(to, 15*time.Minute),
			algebra.NewWindowPair(to, time.Hour),
		}
	}

	// The engine's own windows, derived from the question by the same functions the engine calls
	// (`engine.OnsetSearchWindow`, `engine.ComparePairs`). They supplement the manifest grid
	// rather than replacing it: the grid is what a *model* may explore and is a property of the
	// fixture a reviewer edits, while these are what the deterministic engine will certainly ask
	// and are a property of the question. Both belong in the recording, and deriving them here
	// through the engine's own functions is what stops the two from drifting apart again.
	var engineOnset *investigationv1.Window
	if firedAt := incident.Incident.Question.FiredAt; !firedAt.IsZero() {
		lookback, err := eval.QuestionLookback(incident.Incident.Question.Lookback)
		if err != nil {
			return spec, err
		}
		engineOnset = engine.OnsetSearchWindow(firedAt, lookback)
		// The horizon is `fired_at`: that is the `observed_at` the harness runs an incident
		// fixture at (internal/eval, `Instants{ValidAt: firedAt, ObservedAt: firedAt}`) and
		// therefore the instant every request carries. Passing it here is what keeps the grid the
		// recorder enumerates and the windows the engine asks about the same windows — a recorder
		// that enumerated unclamped pairs would file answers under keys the engine never asks for
		// and miss on every one it does (constitution II; backend/horizon.go).
		for _, at := range engine.ReferenceInstants(firedAt) {
			grid = append(grid, engine.ComparePairs(at, firedAt, lookback)...)
		}
	}
	grid = sortedDistinctPairs(grid)

	culprit := incident.Incident.GroundTruth.Culprit
	if culprit == "" {
		culprit = legacy.GroundTruth.CulpritChange
	}
	var culprits []string
	// `unobserved` and `not_change_induced:<category>` are ground truths that name no change.
	// A world for such a fixture degrades nothing, which is exactly right: the symptom is real
	// and no change explains it.
	if culprit != "" && culprit != "unobserved" && !hasPrefix(culprit, "not_change_induced") {
		culprits = []string{culprit}
	}

	return worldSpec{
		Focus:       focus,
		From:        from.UTC(),
		To:          to.UTC(),
		Hops:        hops,
		Grid:        grid,
		EngineOnset: engineOnset,
		Culprits:    culprits,
		Seed:        manifest.ID,
		Horizon:     incident.Incident.Question.FiredAt.UTC(),
		Shape:       incident.Incident.World.Shape,
		OutputDir:   filepath.Join(dir, "world"),
	}, nil
}

// sortedDistinctPairs puts the window grid into one total order and drops the duplicates a
// manifest entry and an engine-derived entry may coincide on. A world's shape must not depend on
// which of the two named a pair first.
func sortedDistinctPairs(pairs []*investigationv1.WindowPair) []*investigationv1.WindowPair {
	sort.SliceStable(pairs, func(i, j int) bool {
		a, b := pairs[i].GetReferenceAt().AsTime(), pairs[j].GetReferenceAt().AsTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return pairs[i].GetWidthSeconds() < pairs[j].GetWidthSeconds()
	})
	out := make([]*investigationv1.WindowPair, 0, len(pairs))
	seen := map[string]struct{}{}
	for _, pair := range pairs {
		key := fmt.Sprintf("%s/%d", pair.GetReferenceAt().AsTime().UTC().Format(time.RFC3339), pair.GetWidthSeconds())
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, pair)
	}
	return out
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

// latestQueryInstant is the latest instant any manifest query names — a diff's `t2`, a
// subgraph's `valid_at`, a ranked diff's `reference_at`. It is the instant the fixture's goldens
// were recorded at, and therefore the instant a world recorded beside them should be about.
func latestQueryInstant(manifest *fixture.Manifest) time.Time {
	var latest time.Time
	for _, q := range manifest.Queries {
		for _, at := range []time.Time{q.T2, q.ValidAt, q.ReferenceAt} {
			if !at.IsZero() && at.After(latest) {
				latest = at.UTC()
			}
		}
	}
	return latest
}
