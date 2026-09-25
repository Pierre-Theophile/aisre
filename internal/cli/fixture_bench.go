// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// `fixture bench` (contracts/cli.md §Fixtures, research §2, T091).
//
// This is the command behind the storage decision. Constitution X says no graph database may be
// introduced without a reproducible benchmark on the reference workload showing PostgreSQL
// cannot meet the budget; `fixture bench --scale 10k` is that benchmark, and its output is
// committed under `docs/benchmarks/` so the decision can be re-argued from evidence rather than
// from memory.
//
// Like `verify` and `record`, it works in databases of its own — created and dropped by the run
// — so it can be pointed at the same server as a real graph without touching it. It needs two:
// one for the live ingestion and the queries, one for the replay measurement, which by
// definition has to start from an empty projection.
//
// Progress goes to stderr, so `--output json` on stdout stays a clean data channel.

type fixtureBenchOptions struct {
	dsn         string
	scale       string
	seed        int64
	queries     int
	out         string
	skipReplay  bool
	keepDB      bool
	note        string
	batch       int
	replayBatch int
	replayOnly  bool
}

func newFixtureBenchCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureBenchOptions{}

	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Generate the reference workload and measure it against SC-001, SC-002 and ADR-0002",
		Long: "bench generates a synthetic organisation — entities with a power-law degree\n" +
			"distribution, calls/depends-on/runs-on/owned-by relationships, change nodes and\n" +
			"identity claims from two sources that merge about 30% of services — applies it through\n" +
			"the projector, and measures 1,000 randomized queries of each family in research §2.\n\n" +
			"It reports p50/p95/p99 per family, ingestion throughput, full replay wall time and the\n" +
			"resulting database size, and states pass or fail against SC-001 (2-hop p95 < 1 s),\n" +
			"SC-002 (24 h diff p95 < 2 s) and the ADR-0002 exit criteria.\n\n" +
			"Each run creates and drops its own databases, so the role in --db needs CREATEDB; the\n" +
			"database the DSN names is only the maintenance connection.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFixtureBench(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	flags.StringVar(&opts.scale, "scale", fixture.BenchScale10k.Name,
		"workload size: 10k (the reference workload) or 1k (a smoke test)")
	flags.Int64Var(&opts.seed, "seed", 1,
		"seed for the generated workload and the randomized queries; the same seed reproduces the run")
	flags.IntVar(&opts.queries, "queries", fixture.DefaultBenchQueries,
		"randomized queries per family")
	flags.StringVar(&opts.out, "out", "",
		"directory to write <date>-scale-<scale>.md and .json to; empty prints the report instead")
	flags.BoolVar(&opts.skipReplay, "skip-replay", false,
		"skip the full-replay measurement (the longest step); the ADR-0002 replay criterion is then reported as not measured")
	flags.BoolVar(&opts.keepDB, "keep-db", false,
		"do not drop the generated databases, so the run can be profiled with EXPLAIN ANALYZE")
	flags.StringVar(&opts.note, "note", "",
		"free-text note recorded in the report's environment table, e.g. \"Docker compose postgres:16-alpine\"")
	flags.IntVar(&opts.batch, "batch", fixture.DefaultBenchBatch,
		"events per transaction while the workload is generated")
	flags.IntVar(&opts.replayBatch, "replay-batch", projector.DefaultReplayBatchSize,
		"events per transaction during the replay measurement; 1 measures the transaction-per-event loop")
	flags.BoolVar(&opts.replayOnly, "replay-only", false,
		"measure only the full replay: write the generated log to a database of its own and replay it into an empty projection, with no live ingestion and no queries")

	return cmd
}

func runFixtureBench(cmd *cobra.Command, global *globalOptions, opts *fixtureBenchOptions) error {
	ctx := cmd.Context()

	scale, err := fixture.BenchScaleByName(opts.scale)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	if opts.queries < 0 {
		return exitErrorf(ExitUsage, "--queries %d: must not be negative", opts.queries)
	}
	if opts.replayOnly && opts.skipReplay {
		return exitErrorf(ExitUsage, "--replay-only and --skip-replay ask for opposite things")
	}

	dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
	if err != nil {
		return err
	}

	stderr := cmd.ErrOrStderr()
	started := time.Now()
	report, err := fixture.RunBench(ctx, fixture.NewStoreFactoryFromDSN(dsn), fixture.BenchOptions{
		Scale:           scale,
		Seed:            opts.seed,
		Queries:         opts.queries,
		SkipReplay:      opts.skipReplay,
		KeepDatabases:   opts.keepDB,
		BatchSize:       opts.batch,
		ReplayBatchSize: opts.replayBatch,
		ReplayOnly:      opts.replayOnly,
		NewEngine:       func(s *postgres.Store) fixture.BenchEngine { return query.NewEngine(s) },
		Progress: func(format string, args ...any) {
			fmt.Fprintf(stderr, "[%6.0fs] %s\n", time.Since(started).Seconds(), fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		return exitWith(ExitTransport, err)
	}
	if opts.note != "" {
		report.Environment.Note = opts.note
	}

	if opts.out != "" {
		markdownPath, jsonPath, err := report.WriteTo(opts.out)
		if err != nil {
			return exitWith(ExitTransport, err)
		}
		fmt.Fprintf(stderr, "wrote %s and %s\n", markdownPath, jsonPath)
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		encoded, err := report.JSON()
		if err != nil {
			return exitWith(ExitTransport, err)
		}
		if err := p.writeRaw(string(encoded)); err != nil {
			return err
		}
	} else if err := p.writeRaw(report.Markdown()); err != nil {
		return err
	}

	// Exit 4 — the verification code — when a published gate was missed: a benchmark that
	// reports a regression with exit 0 is a benchmark CI will ignore.
	if !report.Passed {
		return exitErrorf(ExitVerification,
			"benchmark at scale %s missed a published criterion; see the Criteria table", report.Scale)
	}
	return nil
}
