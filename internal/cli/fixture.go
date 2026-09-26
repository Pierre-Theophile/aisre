// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// `fixture load` and `fixture verify` (contracts/cli.md §Fixtures, FR-047, FR-048).
//
// Both run in-process against `--db`, not through a running server, and that is deliberate.
//
// A fixture's events carry their recorded observed times: replaying them is what makes a
// bitemporal graph reproducible (FR-023), and the ingest RPC refuses to let a caller choose
// its own observed time — a feeder that could would be able to rewrite what the graph claims
// to have known. Going through the projector directly is the only honest way to restore a
// recording, so `fixture load` takes the database rather than the server, and needs no feeder
// token and no registered source per source in the manifest.
//
// The same holds, more strongly, for `fixture verify`: it needs a database of its own per run
// because "replay from empty" is the whole test. It creates and drops those databases through
// the store factory; the server never sees them.

func newFixtureCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fixture",
		Short: "Load and verify replay fixtures",
		Long: "Replay fixtures are recorded event streams plus golden outputs. They are the project's\n" +
			"evaluation harness (constitution VIII) and gate every change in CI.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(
		newFixtureLoadCommand(global),
		newFixtureVerifyCommand(global),
		newFixtureRecordCommand(global),
		newFixtureRecordWorldCommand(global),
		newFixtureRecordTrajectoryCommand(global),
		newFixtureDeriveCommand(global),
		newFixtureCalibrationCommand(global),
		newFixtureExportCommand(global),
		newFixtureAssembleCommand(global),
		newFixtureBenchCommand(global),
		newFixtureCampaignCommand(global),
		newFixtureCoverageCommand(global),
	)
	return cmd
}

// newQueryRunner builds the manifest query runner the verifier and the recorder answer goldens
// with. It is one function so the two can never drift: a golden recorded by `fixture record`
// and the answer `fixture verify` compares it against come from the same engine.
func newQueryRunner(store *postgres.Store) fixture.QueryRunner {
	return query.NewRunner(query.NewEngine(store))
}

type fixtureLoadOptions struct {
	dsn       string
	principal string
}

func newFixtureLoadCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureLoadOptions{}

	cmd := &cobra.Command{
		Use:   "load <dir>",
		Short: "Apply a fixture's events, with their recorded observed times, into a database",
		Long: "load reads <dir>/manifest.yaml and <dir>/events.jsonl and applies every event through\n" +
			"the projector, stamping each with the observed time the recording carries (FR-023).\n\n" +
			"It writes to --db directly rather than through a running server: only a replay may\n" +
			"choose its own observed times, so the ingest RPC — which always stamps `now` — cannot\n" +
			"restore a recording. No feeder token is needed and no source has to be registered\n" +
			"first; the manifest declares them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			store, err := postgres.Open(ctx, dsn)
			if err != nil {
				return storeError("open database", err)
			}
			defer store.Close()

			report, err := fixture.Load(ctx, projector.New(store), args[0], fixture.LoadOptions{
				Principal: opts.principal,
			})
			if err != nil {
				return exitWith(ExitTransport, err)
			}
			return renderLoadReport(cmd, global, args[0], report)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.dsn, "db", "", "PostgreSQL DSN (default $"+EnvDSN+")")
	flags.StringVar(&opts.principal, "principal", "",
		"principal credited with the fixture's human decision events (FR-041)")

	return cmd
}

func renderLoadReport(cmd *cobra.Command, global *globalOptions, dir string, report *fixture.LoadReport) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)

	if p.json() {
		results := make([]map[string]any, 0, len(report.Results))
		for _, result := range report.Results {
			row := map[string]any{
				"event_id": result.GetEventId(),
				"status":   result.GetStatus().String(),
			}
			if result.GetReasonCode() != "" {
				row["reason_code"] = result.GetReasonCode()
				row["reason_detail"] = result.GetReasonDetail()
			}
			if ts := result.GetObservedAt(); ts != nil {
				row["observed_at"] = formatTime(ts)
			}
			results = append(results, row)
		}
		return p.writeJSON(map[string]any{
			"fixture":             dir,
			"applied":             report.Applied,
			"duplicate_noop":      report.DuplicateNoop,
			"rejected":            report.Rejected,
			"rejected_mismatches": report.RejectedMismatches,
			"results":             results,
		})
	}

	if err := p.writeTable([]string{"OUTCOME", "EVENTS"}, [][]string{
		{"applied", strconv.Itoa(report.Applied)},
		{"duplicate no-op", strconv.Itoa(report.DuplicateNoop)},
		{"rejected", strconv.Itoa(report.Rejected)},
	}); err != nil {
		return err
	}
	if len(report.RejectedMismatches) == 0 {
		return p.writeLine("\n%s loaded; the rejection contract held", dir)
	}
	// An event the graph accepted where the fixture said it would be refused (or the reverse)
	// is exit 5, the rejected-event code: the events went in, but the graph did not do what the
	// recording says it does.
	if err := p.writeLine("\n%s loaded, but its rejection contract did not hold:", dir); err != nil {
		return err
	}
	for _, mismatch := range report.RejectedMismatches {
		if err := p.writeLine("  - %s", mismatch); err != nil {
			return err
		}
	}
	return exitErrorf(ExitRejected, "%s: %d rejection mismatch(es)", dir, len(report.RejectedMismatches))
}

type fixtureVerifyOptions struct {
	dsn            string
	skipGoldens    bool
	trajectoryOnly bool
	report         bool
	reportJSON     string
	shuffles       int
	seed           int64
}

func newFixtureVerifyCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureVerifyOptions{}

	cmd := &cobra.Command{
		Use:   "verify <dir>...",
		Short: "Replay each fixture from an empty database and check it against its contract",
		Long: "verify replays every named fixture from empty, double-delivers every event to prove\n" +
			"idempotency, shuffles each source's events within its declared reordering window, and\n" +
			"compares the manifest's queries against the recorded goldens.\n\n" +
			"Each fixture gets a database of its own, created and dropped by the run: \"replay from\n" +
			"empty\" is the test, so it cannot share state with anything. The role in --db therefore\n" +
			"needs the CREATEDB privilege; the database the DSN names is only ever used as the\n" +
			"maintenance connection, and is never read or written.\n\n" +
			"Exit code 4 when any fixture fails (contracts/cli.md §Exit codes).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// --trajectory-only needs no database: the 001 steps are the ones that replay from
			// empty, and the trajectory gate runs in a job that has no Postgres and no egress
			// (T098). Requiring a DSN it would never open would make the gate unrunnable.
			var newStore fixture.StoreFactory
			if !opts.trajectoryOnly {
				dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
				if err != nil {
					return err
				}
				newStore = fixture.NewStoreFactoryFromDSN(dsn)
			}

			dirs, emptyGroups, err := expandFixtureArgs(args)
			if err != nil {
				return err
			}

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			for _, group := range emptyGroups {
				if err := p.writeLine("%s: 0 fixtures (a group directory with none in it yet)", group); err != nil {
					return err
				}
			}
			failed := 0

			// --report-json collects the canonical reports of *this* run, so a CI job can
			// publish the human summary and gate on the numbers without verifying twice.
			var machine *os.File
			if opts.reportJSON != "" {
				machine, err = os.Create(opts.reportJSON)
				if err != nil {
					return exitErrorf(ExitUsage, "--report-json %s: %v", opts.reportJSON, err)
				}
				defer machine.Close()
			}

			for _, dir := range dirs {
				report, err := fixture.Verify(ctx, newStore, dir, fixture.VerifyOptions{
					SkipGoldens:    opts.skipGoldens,
					TrajectoryOnly: opts.trajectoryOnly,
					Shuffles:       shuffleCount(opts.shuffles),
					Seed:           opts.seed,
					Report:         opts.report,
					NewRunner:      newQueryRunner,
				})
				if err != nil {
					// A fixture that could not be run at all is not a verification failure —
					// it is a broken fixture or an unreachable database.
					return exitWith(ExitTransport, err)
				}
				if !report.Passed {
					failed++
				}
				if err := renderVerifyReport(p, report); err != nil {
					return err
				}
				if machine != nil {
					encoded, err := report.JSON()
					if err != nil {
						return exitWith(ExitTransport, err)
					}
					if _, err := machine.Write(append(encoded, '\n')); err != nil {
						return exitWith(ExitTransport, err)
					}
				}
			}
			if machine != nil {
				if err := machine.Close(); err != nil {
					return exitWith(ExitTransport, err)
				}
			}

			if failed > 0 {
				return exitErrorf(ExitVerification, "%d of %d fixture(s) failed verification",
					failed, len(dirs))
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	flags.BoolVar(&opts.skipGoldens, "skip-goldens", false,
		"skip the golden comparison, for a fixture whose goldens are not recorded yet")
	flags.BoolVar(&opts.trajectoryOnly, "trajectory-only", false,
		"run only the incident and trajectory-replay steps: no database, no network (FR-042a)")
	flags.BoolVar(&opts.report, "report", false,
		"emit ranking, calibration and resolution metrics")
	flags.StringVar(&opts.reportJSON, "report-json", "",
		"also write the canonical JSON report of each fixture to this file, one per line")
	flags.IntVar(&opts.shuffles, "shuffles", fixture.DefaultShuffles,
		"seeded permutations to run in the shuffle step; 0 skips it")
	flags.Int64Var(&opts.seed, "seed", 0,
		"seed for the shuffle permutations; the same seed reproduces the same run")

	return cmd
}

// expandFixtureArgs resolves the arguments of `fixture verify` into the fixture directories to
// run, and names the ones that hold none.
//
// Three shapes are handled, and the third is why this function exists:
//
//   - a fixture — a directory with a manifest.yaml — runs as itself;
//   - a file is a usage error. `fixtures/*` also matches fixtures/README.md, and the glob
//     somebody meant is `fixtures/*/`. Saying so is better than failing twenty seconds later
//     with "open fixtures/README.md/manifest.yaml: not a directory", and better than skipping
//     the argument — a verifier that silently ignores what it was handed is how a repository
//     stops testing without anybody noticing;
//   - a GROUP — a directory with no manifest.yaml of its own — expands to the immediate
//     subdirectories that have one. `fixtures/incidents/` is the case in hand: feature 002's
//     incident fixtures extend this format rather than replacing it, so `fixtures/*/` should
//     reach them without anyone maintaining a second glob. A group with no members contributes
//     nothing and is reported as empty rather than as an error, because a directory created
//     ahead of the fixtures that will live in it is a normal state of an unfinished feature.
//
// What is NOT allowed is ending up with nothing to verify: if every argument resolves to an
// empty group, that is a run which would pass by doing no work, and it is refused.
func expandFixtureArgs(args []string) (dirs, emptyGroups []string, err error) {
	for _, arg := range args {
		info, statErr := os.Stat(arg)
		switch {
		case statErr != nil:
			// A fixture directory that is not there is a broken invocation, not a failed
			// verification, and it keeps the exit code it had before this function existed.
			return nil, nil, exitWith(ExitTransport, fmt.Errorf("fixture: %s: %w", arg, statErr))
		case !info.IsDir():
			return nil, nil, exitErrorf(ExitUsage,
				"%s is not a fixture directory; a fixture is a directory, so the glob "+
					"you want is `fixtures/*/`", arg)
		}

		if _, statErr := os.Stat(filepath.Join(arg, fixture.ManifestFile)); statErr == nil {
			dirs = append(dirs, arg)
			continue
		}

		members, readErr := fixtureGroupMembers(arg)
		if readErr != nil {
			return nil, nil, exitErrorf(ExitUsage, "%s: %v", arg, readErr)
		}
		if len(members) == 0 {
			emptyGroups = append(emptyGroups, arg)
			continue
		}
		dirs = append(dirs, members...)
	}

	if len(dirs) == 0 {
		return nil, nil, exitErrorf(ExitUsage,
			"no fixtures in %v; a run that verifies nothing is not a run that passed", args)
	}
	return dirs, emptyGroups, nil
}

// fixtureGroupMembers lists the immediate subdirectories of dir that are fixtures, sorted, so
// a group verifies in the same order on every machine.
func fixtureGroupMembers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		member := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(filepath.Join(member, fixture.ManifestFile)); err != nil {
			continue
		}
		members = append(members, member)
	}
	slices.Sort(members)
	return members, nil
}

// shuffleCount translates the flag into what VerifyOptions means by it.
//
// The verifier reads 0 as "use the default" and a negative number as "skip", which is the
// right default for a library and the wrong one for a flag: somebody who types `--shuffles 0`
// means none, not six. The flag's own default is already the library's default, so 0 here is
// always a deliberate request to skip.
func shuffleCount(flagValue int) int {
	if flagValue == 0 {
		return -1
	}
	return flagValue
}

// renderVerifyReport prints one fixture's outcome: the report's own Markdown for a person, its
// own JSON for a machine. Both come from the fixture package, so the CLI cannot drift from
// what CI publishes. With several fixtures the JSON is one document per line (JSONL).
func renderVerifyReport(p *printer, report *fixture.VerifyReport) error {
	if !p.json() {
		return p.writeRaw(report.Markdown())
	}
	encoded, err := report.JSON()
	if err != nil {
		return exitWith(ExitTransport, err)
	}
	return p.writeRaw(string(encoded))
}

// `fixture record` (contracts/cli.md §Fixtures, FR-049).
//
// Recording answers the question "what does the graph say now?" and freezes it. It never
// rewrites `events.jsonl`: the events are the fixture's evidence, and a harness that could
// re-derive its own input from its own output would have stopped being able to fail. Only the
// goldens are written, and they are reviewed as a diff in the pull request that produces them.
//
// Like `verify`, it works in a database of its own — created and dropped by the run — so a
// recording cannot be contaminated by, or contaminate, anything else on the server.

type fixtureRecordOptions struct {
	dsn string
}

func newFixtureRecordCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureRecordOptions{}

	cmd := &cobra.Command{
		Use:   "record <dir>...",
		Short: "Replay each fixture into a fresh database and rewrite its golden query outputs",
		Long: "record replays every named fixture from empty, runs the queries its manifest lists, and\n" +
			"writes each answer to golden/<kind>.<name>.json in the canonical serialization. When the\n" +
			"manifest declares a clock end, the whole set is run again with observed time pinned to\n" +
			"it, into golden/pinned/.\n\n" +
			"events.jsonl is never touched. Query kinds this build cannot answer yet are skipped and\n" +
			"named in the report, never written as empty goldens.\n\n" +
			"Each fixture gets a database of its own, so the role in --db needs CREATEDB; the\n" +
			"database the DSN names is only the maintenance connection.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			newStore := fixture.NewStoreFactoryFromDSN(dsn)
			p := newPrinter(cmd.OutOrStdout(), global.Output)

			for _, dir := range args {
				report, err := fixture.Record(ctx, newStore, dir, newQueryRunner)
				if err != nil {
					return exitWith(ExitTransport, err)
				}
				if err := renderRecordReport(p, report); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")

	return cmd
}

func renderRecordReport(p *printer, report *fixture.RecordReport) error {
	if !p.json() {
		return p.writeRaw(report.Markdown())
	}
	return p.writeJSON(report)
}

// `fixture export` (contracts/cli.md §Fixtures, T083, SC-010).
//
// Export is the odd one out among the fixture commands: it does not create a database, does not
// replay anything and does not write to the graph. It runs a manifest's queries against a
// database that already exists — the live one a nightly run has been feeding — and writes the
// answers under the same names, in the same canonical serialization, that `fixture record`
// writes goldens under.
//
// That is what turns SC-010 from a claim into a file comparison:
//
//	aisre fixture export --db "$LIVE_DSN" --manifest fixtures/x/manifest.yaml --out /tmp/live
//	diff -r /tmp/live fixtures/x/golden
//
// `--compare fixtures/x` does the same comparison in-process and prints the verdict, so a CI
// job gets one line and an exit code instead of a diff nobody reads.

type fixtureExportOptions struct {
	dsn      string
	manifest string
	out      string
	compare  string
}

func newFixtureExportCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureExportOptions{}

	cmd := &cobra.Command{
		Use:   "export --manifest <path> --out <dir>",
		Short: "Run a manifest's queries against an existing database and write canonical answers",
		Long: "export answers every query in --manifest against the database --db names, exactly as it\n" +
			"stands, and writes each canonical response to <out>/<kind>.<name>.json — plus\n" +
			"<out>/pinned/ for the observed-time-pinned pass. The layout is a fixture's golden/, so\n" +
			"`diff -r <out> <fixture>/golden` is the whole live-versus-replayed comparison (SC-010).\n\n" +
			"Unlike verify and record it creates no database and replays nothing: the graph it reads\n" +
			"is the one somebody else built. It never writes to that database, so it is safe to point\n" +
			"at a live deployment with a read-only role.\n\n" +
			"With --compare <fixture-dir> the comparison is made in-process and a one-line verdict is\n" +
			"printed; exit code 4 when the two disagree.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			if opts.manifest == "" {
				return exitErrorf(ExitUsage, "--manifest is required: it names the queries to export")
			}
			if opts.out == "" {
				return exitErrorf(ExitUsage, "--out is required: it is the directory the answers are written to")
			}
			dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			store, err := postgres.Open(ctx, dsn)
			if err != nil {
				return storeError("open database", err)
			}
			defer store.Close()

			report, err := fixture.Export(ctx, store, opts.manifest, newQueryRunner, fixture.ExportOptions{
				Out:     opts.out,
				Compare: opts.compare,
			})
			if err != nil {
				return exitWith(ExitTransport, err)
			}
			if err := renderExportReport(newPrinter(cmd.OutOrStdout(), global.Output), report); err != nil {
				return err
			}
			if report.Comparison != nil && !report.Comparison.Equal {
				return exitErrorf(ExitVerification, "%s: the live graph and the recorded goldens differ",
					report.FixtureID)
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.dsn, "db", "", "PostgreSQL DSN of the graph to export (default $"+EnvDSN+")")
	flags.StringVar(&opts.manifest, "manifest", "",
		"fixture manifest naming the queries to run; the file or the directory holding it")
	flags.StringVar(&opts.out, "out", "", "directory the canonical answers are written to")
	flags.StringVar(&opts.compare, "compare", "",
		"fixture directory whose golden/ the export is diffed against; exit 4 when they differ")

	return cmd
}

func renderExportReport(p *printer, report *fixture.ExportReport) error {
	if !p.json() {
		return p.writeRaw(report.Markdown())
	}
	return p.writeJSON(report)
}

// `fixture assemble` (contracts/cli.md §Fixtures, T082).
//
// A live run has one `--record` directory per feeder, and the feeder-gap scenario restarts one
// of them into a second directory because `--record` into a directory that already holds
// payloads corrupts it. A fixture is one directory. assemble is the step between, and it exists
// as a command because the live run of 2026-09-16 did it with a Python script in /tmp and the
// nightly job cannot.
//
// It writes payloads/, events.jsonl, rejected.jsonl and a manifest with the union of the runs'
// declared sources. What a human has to decide — the description, the queries whose answers
// become goldens, the ground truth — is left for the pull request, exactly as `--record` leaves
// it.

type fixtureAssembleOptions struct {
	out         string
	id          string
	family      string
	description string
}

func newFixtureAssembleCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureAssembleOptions{}

	cmd := &cobra.Command{
		Use:   "assemble --out <dir> <recording-dir>...",
		Short: "Merge several feeders' recordings into one fixture directory",
		Long: "assemble merges the `--record` directories of a live run into a single fixture:\n" +
			"payloads renumbered per kind and re-indexed by arrival, events ordered by observed time\n" +
			"with appendedSeq renumbered across the result, rejections concatenated, and a manifest\n" +
			"carrying the union of the sources each run declared.\n\n" +
			"Recording directories are given in the order their recordings began; that order is only\n" +
			"the tie-break between two events sharing an observed time. Event lines are copied byte\n" +
			"for byte apart from their appendedSeq: they are canonical JSON the graph produced, and a\n" +
			"fixture that re-encodes them would be testing this binary's encoder.\n\n" +
			"Then add a `queries:` list and run `fixture record` and `fixture verify`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.out == "" {
				return exitErrorf(ExitUsage, "--out is required: it is the fixture directory to write")
			}
			report, err := fixture.Assemble(args, opts.out, fixture.AssembleOptions{
				ID:          opts.id,
				Family:      opts.family,
				Description: opts.description,
			})
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if !p.json() {
				return p.writeRaw(report.Markdown())
			}
			return p.writeJSON(report)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.out, "out", "", "fixture directory to write")
	flags.StringVar(&opts.id, "id", "", "fixture id (default: the base name of --out)")
	flags.StringVar(&opts.family, "family", "", "fixture family, e.g. rollout-regression")
	flags.StringVar(&opts.description, "description", "", "human summary for the manifest")

	return cmd
}
