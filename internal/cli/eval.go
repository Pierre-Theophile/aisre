// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// `aisre eval` — the evaluation job's command line (T109, T112, T113; FR-058, FR-059,
// FR-060, contracts/cli.md §Fixtures and evaluation).
//
// **Why this is not `fixture verify --report`.** It was going to be. `fixture verify` is what
// `ci.yml` runs on every pull request, under `--trajectory-only`, in a job with **no Postgres and
// no egress**, and it has to stay fast enough to be worth running there. The world-replay
// evaluation is the opposite animal: it needs a database per fixture, it runs the corpus k times,
// and with a live model configuration it costs money and minutes. Hanging that off the same verb
// would mean one of two bad outcomes — either the PR gate slows down to the evaluation's pace, or
// the evaluation hides behind a flag on a command whose documented promise is "this is cheap".
//
// So the split is by cadence, and each command keeps one promise:
//
//	fixture verify … --trajectory-only   every pull request, zero network, zero model calls
//	eval run …                           nightly and on demand, world replay, corpus × k
//
// `fixture verify --report` keeps doing exactly what 001 made it do — ranking and resolution
// metrics over a fixture directory — and gains nothing from this feature, so an incident fixture
// still verifies in CI at CI's speed.
//
// Both write JSONL, and `scripts/check-report.sh` reads both: `--investigation` selects the
// metric rows this command writes, and the bare form the per-fixture verification reports.

func newEvalCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run the incident corpus and publish the investigation metrics",
		Long: "eval is the evaluation job: the corpus × k in world-replay mode, scored against each\n" +
			"fixture's ground truth, published as the rows `scripts/check-report.sh --investigation`\n" +
			"gates on (FR-058, FR-059, FR-060).\n\n" +
			"It is deliberately separate from `fixture verify`, which is the per-pull-request gate and\n" +
			"must stay runnable with no database and no network.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(newEvalRunCommand(global), newEvalPowerCommand(global))
	return cmd
}

type evalRunOptions struct {
	fixtures   string
	dsn        string
	auditPath  string
	modelYAML  string
	pricesYAML string
	runs       int
	extendTo   int
	modelFree  bool
	fakeModel  bool
	live       bool
	batch      bool
	corpus     string
	variants   string
	reportJSON string
	summary    string
	doc        string
}

func newEvalRunCommand(global *globalOptions) *cobra.Command {
	opts := &evalRunOptions{}

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Investigate every incident fixture k times against its recorded world and score it",
		Long: "run investigates each fixture's own `incident:` question, k times (3, extended to 7 only\n" +
			"when the first 3 disagree — FR-061), with the graph replayed from that fixture's\n" +
			"events.jsonl into a database of its own and every worker answered from its recorded\n" +
			"world. Nothing reaches a network unless a live model configuration is supplied.\n\n" +
			"It writes one metric row per line to --report-json: {metric, scope, fixture, value, n,\n" +
			"detail}. `scripts/check-report.sh --investigation <file>` is the gate over those rows.\n\n" +
			"--model-free runs the deterministic engine end to end with no model call at all. Every\n" +
			"number such a run publishes is labelled `model-free`, because it is not the production\n" +
			"result and must never be read as one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEval(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.fixtures, "fixtures", "fixtures/incidents",
		"the corpus: a fixture directory, or a group directory holding them")
	flags.StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	flags.StringVar(&opts.auditPath, "audit", eval.DefaultAuditPath,
		"the published coverage audit π₀ and the coverage ceiling are read from (FR-071)")
	flags.StringVar(&opts.modelYAML, "model-config", "config/model.yaml",
		"the model configuration; its digest is recorded with every number (FR-061)")
	flags.StringVar(&opts.pricesYAML, "prices", "config/prices.yaml", "the price table costs are derived from")
	flags.IntVar(&opts.runs, "runs", eval.DefaultRuns, "runs per fixture")
	flags.IntVar(&opts.extendTo, "extend-to", eval.DefaultExtendTo,
		"total runs a fixture is extended to when its first --runs verdicts disagree (FR-061)")
	flags.BoolVar(&opts.modelFree, "model-free", false,
		"run the deterministic engine with no model call; every published number is labelled model-free")
	flags.BoolVar(&opts.fakeModel, "fake-model", false,
		"run with canned model turns through the real transport seam")
	flags.BoolVar(&opts.live, "live", false,
		"call the providers --model-config names for real; needs the credential each configured "+
			"provider uses ($"+model.EnvMistralAPIKey+", $"+model.EnvAnthropicAPIKey+"). Without "+
			"one the run falls back to model-free and says which variable was missing")
	flags.BoolVar(&opts.batch, "batch", false,
		"RESERVED: submit the corpus through the Batch API where a run is not latency-bound. "+
			"internal/investigation/model has no batch client yet, so this flag is accepted, "+
			"reported and does nothing")
	flags.StringVar(&opts.corpus, "corpus", "public",
		"which corpus this run is measuring: public (synthetic twins) or private (recorded)")
	flags.StringVar(&opts.variants, "variants", "",
		"directory of derived fixtures (`fixture derive`) to check the metamorphic invariants "+
			"over; without it the invariance gate prints NOT RUN rather than passing (FR-062a)")
	flags.StringVar(&opts.reportJSON, "report-json", "",
		"write the metric rows here, one JSON document per line")
	flags.StringVar(&opts.summary, "summary", "",
		"write the Markdown summary here, for a job summary")
	flags.StringVar(&opts.doc, "doc", "",
		"regenerate the tables section of this document from the rows, keeping its prose "+
			"(docs/evaluation/investigation-metrics.md)")

	return cmd
}

// runEval is the whole job: build a harness per fixture, run it under the policy, grade it,
// and publish.
func runEval(cmd *cobra.Command, global *globalOptions, opts *evalRunOptions) error {
	ctx := cmd.Context()
	p := newPrinter(cmd.OutOrStdout(), global.Output)

	dirs, emptyGroups, err := expandFixtureArgs([]string{opts.fixtures})
	if err != nil {
		return err
	}
	for _, group := range emptyGroups {
		if err := p.writeLine("%s: 0 fixtures (a group directory with none in it yet)", group); err != nil {
			return err
		}
	}

	kind := eval.KindModelFree
	switch {
	case opts.live && !opts.modelFree:
		// A live run is the only one that costs money, so it is asked for explicitly — and a
		// credential the configuration needs and the environment does not have is a fallback to
		// model-free with the variable named, never a corpus that dies on fixture 3 of 16.
		ok, missing, err := eval.LiveAvailable(opts.modelYAML)
		if err != nil {
			return exitWith(ExitUsage, err)
		}
		if ok {
			kind = eval.KindLive
		} else if err := p.writeLine(
			"--live asked for a live run but $%s is not set, so this run is model-free and every "+
				"number it publishes is labelled as such (FR-067)",
			strings.Join(missing, ", $")); err != nil {
			return err
		}
	case opts.fakeModel && !opts.modelFree:
		kind = eval.KindFakeModel
	}
	if opts.batch {
		if err := p.writeLine(
			"--batch is reserved: `internal/investigation/model` has no Batch API client, so this run " +
				"used ordinary requests. The flag exists so the workflow that will want it does not " +
				"have to be rewritten around it"); err != nil {
			return err
		}
	}

	in := eval.Input{
		Corpus:         opts.corpus,
		FixtureRoot:    corpusRoot(opts.fixtures),
		AuditPath:      opts.auditPath,
		ModelFree:      kind == eval.KindModelFree,
		RunsPerFixture: opts.runs,
	}
	policy := eval.RunPolicy{Runs: opts.runs, ExtendTo: opts.extendTo}

	parents := map[string]*eval.RunOutcome{}
	for _, dir := range dirs {
		each, err := evalOneFixture(ctx, p, dir, kind, policy, opts)
		if err != nil {
			return err
		}
		if each == nil {
			continue
		}
		in.Runs = append(in.Runs, each.Rows...)
		if each.First != nil {
			parents[each.First.FixtureID] = each.First
		}
		if in.ModelConfigDigest == "" {
			in.ModelConfigDigest, in.ModelConfigVersion = each.Digest, each.Version
		}
	}
	if opts.variants != "" {
		in.Invariance, err = evalVariants(ctx, p, opts.variants, kind, parents, opts)
		if err != nil {
			return err
		}
	}
	if len(in.Runs) == 0 {
		return exitErrorf(ExitUsage,
			"no incident fixture under %s carries an `incident:` block; a run that scored nothing "+
				"is not a run that passed", opts.fixtures)
	}

	report := eval.Build(in)
	return publishEval(p, report, opts)
}

// evalOneFixture runs and grades one fixture, and returns its rows plus the model configuration
// the runs used.
//
// A fixture with no `incident:` block is reported and skipped rather than refused: a corpus
// directory may legitimately hold a 001 fixture that a reviewer put there, and failing the whole
// evaluation over it would be the wrong lesson.
func evalOneFixture(
	ctx context.Context,
	p *printer,
	dir, kind string,
	policy eval.RunPolicy,
	opts *evalRunOptions,
) (*fixtureEval, error) {
	m, isIncident, err := incidentManifest(dir)
	if err != nil {
		return nil, err
	}
	if !isIncident {
		return nil, p.writeLine("%s: skipped — no `incident:` block, so there is nothing to investigate", dir)
	}

	dsn := opts.dsn
	if dsn == "" {
		dsn = os.Getenv(EnvDSN)
	}
	harness, err := eval.NewHarness(ctx, eval.Options{
		Dir:        dir,
		DSN:        dsn,
		AuditPath:  opts.auditPath,
		Kind:       kind,
		ModelYAML:  opts.modelYAML,
		PricesYAML: opts.pricesYAML,
	})
	if err != nil {
		return nil, exitWith(ExitTransport, err)
	}
	defer harness.Close()

	set, err := harness.RunUnderPolicy(ctx, policy)
	if err != nil {
		return nil, exitWith(ExitTransport, err)
	}
	truth := m.Incident.GroundTruth
	grades := set.Grade(truth, m.Incident.Question.FiredAt)

	rows := eval.RunRowsFrom(set, grades, truth, eval.RowOptions{
		MissRateThreshold: m.Incident.World.MissRateThreshold,
		WorldDigests:      worldDigests(harness.World()),
	})
	if err := p.writeLine("%s", set.String()); err != nil {
		return nil, err
	}
	out := &fixtureEval{Rows: rows, Digest: set.ModelConfigDigest, First: set.First()}
	if out.First != nil {
		out.Version = out.First.ModelConfigVersion
	}
	return out, nil
}

// fixtureEval is one fixture's contribution to the report, plus the first run's outcome — which
// is the parent a metamorphic variant is compared against (FR-062a). The **first** run, not the
// best one, for the same reason pass@1 is the first run: production gets one.
type fixtureEval struct {
	Rows    []eval.RunRow
	Digest  string
	Version string
	First   *eval.RunOutcome
}

// evalVariants runs the derived fixtures under --variants and checks each against the invariant
// its transform defines (T108's rows, FR-062a, SC-020).
//
// The variants are **generated, never checked in** (`aisre fixture derive` writes them, and
// records a world for each), so this takes a directory rather than deriving them itself: deriving
// a variant needs a world recorded against its regenerated graph, which is `fixture derive`'s job
// and not the evaluator's. A run with no --variants publishes the gate as NOT RUN rather than as
// a pass, which is what `check-report.sh` prints.
func evalVariants(
	ctx context.Context,
	p *printer,
	dir, kind string,
	parents map[string]*eval.RunOutcome,
	opts *evalRunOptions,
) ([]eval.InvarianceResult, error) {
	dirs, _, err := expandFixtureArgs([]string{dir})
	if err != nil {
		return nil, err
	}
	single := eval.RunPolicy{Runs: 1, ExtendTo: 1}
	results := make([]eval.InvarianceResult, 0, len(dirs))

	for _, variant := range dirs {
		derived, ok, err := fixture.ReadDeriveReport(variant)
		if err != nil {
			return nil, exitWith(ExitTransport, err)
		}
		if !ok {
			if err := p.writeLine(
				"%s: skipped — no %s, so nothing says which parent and which transform it came from "+
					"(a variant with no invariant cannot be graded metamorphically)",
				variant, fixture.DeriveReportFile); err != nil {
				return nil, err
			}
			continue
		}
		parent, ok := parents[derived.ParentID]
		if !ok {
			if err := p.writeLine(
				"%s: skipped — its parent %s was not in this run, and an invariant is a statement "+
					"about two runs", variant, derived.ParentID); err != nil {
				return nil, err
			}
			continue
		}
		each, err := evalOneFixture(ctx, p, variant, kind, single, opts)
		if err != nil {
			return nil, err
		}
		if each == nil || each.First == nil {
			continue
		}
		results = append(results,
			eval.CheckInvariance(parent, each.First, derived.Transform, derived.NameMap))
	}
	return results, nil
}

// publishEval writes the rows, the Markdown and the document.
func publishEval(p *printer, report *eval.Report, opts *evalRunOptions) error {
	if opts.reportJSON != "" {
		encoded, err := eval.JSONL(report.Rows)
		if err != nil {
			return exitWith(ExitTransport, err)
		}
		if err := os.WriteFile(opts.reportJSON, encoded, 0o600); err != nil {
			return exitErrorf(ExitUsage, "--report-json %s: %v", opts.reportJSON, err)
		}
	}
	markdown := report.Markdown()
	if opts.summary != "" {
		if err := os.WriteFile(opts.summary, []byte(markdown), 0o600); err != nil {
			return exitErrorf(ExitUsage, "--summary %s: %v", opts.summary, err)
		}
	}
	if opts.doc != "" {
		if err := report.WriteDoc(opts.doc); err != nil {
			return exitWith(ExitTransport, err)
		}
	}
	if p.json() {
		return p.writeJSON(report.Rows)
	}
	return p.writeRaw(markdown)
}

// corpusRoot is the directory the corpus-gap detector scans.
//
// `--fixtures` may name one fixture — which is what a developer reproducing a single case types —
// and scanning a single fixture directory for gaps would report every category of the audit's
// unobservable remainder as uncovered, which is a statement about the argument rather than about
// the corpus. So a fixture directory is answered with its parent, and a group directory with
// itself.
func corpusRoot(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, fixture.ManifestFile)); err == nil {
		return filepath.Dir(dir)
	}
	return dir
}

// worldDigests reads the recorded world's own response digests, so citation validity is checked
// against the recording rather than against the citation's own claim about itself.
func worldDigests(world *sdk.World) map[string]string {
	if world == nil {
		return nil
	}
	out := map[string]string{}
	for _, key := range world.Keys() {
		answer, ok := world.Answer(key)
		if !ok {
			continue
		}
		out[key] = answer.GetResponseDigest()
	}
	return out
}

// incidentManifest loads a fixture's manifest and reports whether it is an incident.
func incidentManifest(dir string) (*fixture.Manifest, bool, error) {
	m, err := fixture.LoadManifest(dir)
	if err != nil {
		return nil, false, exitWith(ExitTransport, err)
	}
	return m, m.IsIncident(), nil
}

// `aisre eval power` — the detection-power statement, as a command (T112).
//
// It is a command rather than arithmetic in `scripts/check-report.sh` for one reason: the script
// is POSIX shell with `jq` and `awk`, and the statement needs a square root, a normal quantile
// and a bisection. Re-implementing those in awk would be a second, divergent copy of a rule the
// project states once, in `internal/eval/metrics.go` — and a detection-power statement that
// disagreed with the report's own would be worse than none.
//
// The method is printed with every answer, because a number like "detects a drop to 76 %" means
// nothing without the test that produced it.

type evalPowerOptions struct {
	n             int
	baseline      float64
	direction     string
	detectable    float64
	undetectable  float64
	zeroTolerance bool
	quiet         bool
}

func newEvalPowerCommand(global *globalOptions) *cobra.Command {
	opts := &evalPowerOptions{}

	cmd := &cobra.Command{
		Use:   "power",
		Short: "What regression a gate of n Bernoulli trials can actually detect",
		Long: "power states what a gate can see at the n a run actually had: fixtures × runs as\n" +
			"Bernoulli trials, under the normal approximation to the binomial, one-sided, α = 0.05,\n" +
			"power 0.8. The method is printed with the answer.\n\n" +
			"--zero-tolerance states the same thing for a gate whose threshold is 0 — any untraceable\n" +
			"conclusion, any improvised replay, any metamorphic verdict change — which fires on the\n" +
			"first occurrence and whose real limit is how intermittent a defect it can miss.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if opts.n < 0 {
				return exitErrorf(ExitUsage, "--n must not be negative")
			}
			if opts.zeroTolerance {
				if p.json() {
					return p.writeJSON(map[string]any{
						"n":              opts.n,
						"zero_tolerance": true,
						"detectable_at":  eval.ZeroToleranceDetectable(opts.n),
						"alpha":          eval.PowerAlpha,
						"sentence":       eval.ZeroToleranceSentence(opts.n),
					})
				}
				return p.writeLine("%s", eval.ZeroToleranceSentence(opts.n))
			}
			if opts.baseline <= 0 || opts.baseline > 1 {
				return exitErrorf(ExitUsage, "--baseline must be in (0, 1]; got %v", opts.baseline)
			}
			power := eval.DetectionPowerFor(opts.n, opts.baseline, opts.direction)
			if p.json() {
				return p.writeJSON(map[string]any{
					"power":    power,
					"sentence": power.Sentence(),
					"example":  power.Example(opts.detectable, opts.undetectable),
				})
			}
			if opts.quiet {
				return p.writeLine("%s", power.Sentence())
			}
			if err := p.writeLine("%s", power.Sentence()); err != nil {
				return err
			}
			return p.writeLine("%s", power.Example(opts.detectable, opts.undetectable))
		},
	}

	flags := cmd.Flags()
	flags.IntVar(&opts.n, "n", 40, "Bernoulli trials: fixtures × runs")
	flags.Float64Var(&opts.baseline, "baseline", 0.9, "the rate the gate is set at")
	flags.StringVar(&opts.direction, "direction", eval.DirectionMin,
		"which way a regression moves this gate: min (a fall, e.g. pass@1) or max (a rise, e.g. harm rate)")
	flags.Float64Var(&opts.detectable, "detectable", 0.70, "the alternative in the published example")
	flags.Float64Var(&opts.undetectable, "undetectable", 0.80, "the second alternative in the example")
	flags.BoolVar(&opts.zeroTolerance, "zero-tolerance", false,
		"state the power of a gate whose threshold is 0")
	flags.BoolVar(&opts.quiet, "quiet", false, "print the sentence and nothing else")

	return cmd
}
