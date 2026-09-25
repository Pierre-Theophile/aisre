// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
)

// `fixture record-trajectory` (tasks.md T093; FR-042a, FR-063, contracts/cli.md §Fixtures).
//
// It records the layer-1 recording an incident fixture ships: the investigation its `incident:`
// block states, run against the fixture's own two sources — the graph, replayed from
// `events.jsonl` into a database of its own, and the telemetry, answered from the recorded
// `world/`. Nothing else is consulted and nothing reaches a network.
//
// It lives in the CLI rather than in `internal/fixture` for one structural reason: the graph
// worker is built over `internal/query`, and `internal/query` imports `internal/fixture` to
// answer manifest queries. Putting the recorder in `internal/fixture` would close that loop. The
// CLI is above both, which is where a tool that wires a graph to an engine belongs anyway.
//
// **The run is deterministic by construction**, so that re-recording an unchanged fixture writes
// the same bytes to the same path and a re-record is a `git status` a reviewer can read:
//
//   - the clock is **constant**, not merely injected. The first wave issues its calls in
//     parallel, so a clock that advanced on every read would advance in whatever order the
//     scheduler chose. A constant clock makes every recorded instant a function of the fixture.
//     Elapsed wall time is then zero everywhere, which is right for a recording: a replayed run
//     took no time in the world it is a recording of.
//   - the run id is derived from the trajectory's own digest (`replay.RunID`), so the file name
//     is a function of the content;
//   - `duration_ms` is cleared from every recorded answer by `replay.Normalize`, which is the
//     rule `pkg/backend.ResponseDigest` already publishes.
//
// Three recordings are possible and the corpus ships the first two by default. `--model-free` is
// the deterministic engine end to end and holds no model record at all; `--fake-model` adds
// canned turns — one proposed hypothesis, one batch of judgments — so that the **model-request
// digest matching path** the gate is built on is exercised by the corpus rather than only by a
// unit test.
//
// `--live` is the third, and it is the only one that costs money and opens a socket: it calls the
// configured providers and writes a trajectory whose model exchanges are real turns. It does not
// *replace* the fake-model recording, it joins it. The fake-model run is the network-free
// exercise of the tool path — the thing every pull request replays — and the live run is the
// production-configuration recording. Deleting either would lose something the other does not
// have.

// defaultAuditPath is the published coverage audit π₀ is read from (ADR-0005 D9).
const defaultAuditPath = eval.DefaultAuditPath

type fixtureTrajectoryOptions struct {
	dsn        string
	auditPath  string
	modelYAML  string
	pricesYAML string
	fakeModel  bool
	modelFree  bool
	live       bool
}

func newFixtureRecordTrajectoryCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureTrajectoryOptions{}

	cmd := &cobra.Command{
		Use:   "record-trajectory <dir>",
		Short: "Record an incident fixture's layer-1 trajectory",
		Long: "record-trajectory runs the investigation the fixture's `incident:` block states and\n" +
			"writes `trajectories/<run-id>.jsonl` (FR-042a).\n\n" +
			"The graph comes from replaying the fixture's own events.jsonl into a database of its\n" +
			"own; the telemetry comes from the recorded `world/`. Without --live no network is\n" +
			"opened and no model is called: --fake-model serves canned turns through the same\n" +
			"transport seam a live recording uses. --live calls the providers --model-config names\n" +
			"and records the real exchanges; it is additive, and the other two recordings stay.\n\n" +
			"The run id is derived from the trajectory's digest, so re-recording an unchanged\n" +
			"fixture rewrites the same file with the same bytes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			m, err := fixture.LoadManifest(args[0])
			if err != nil {
				return exitWith(ExitTransport, err)
			}
			if !m.IsIncident() {
				return exitErrorf(ExitUsage,
					"%s has no `incident:` block; there is no investigation to record", args[0])
			}

			prior, err := loadPrior(opts.auditPath)
			if err != nil {
				return err
			}

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			kinds := recordKinds(opts)
			// A live recording that cannot resolve a credential is refused here rather than
			// halfway through an investigation, and the refusal names the variable. The other
			// kinds reach no network and need none.
			if opts.live {
				ok, missing, err := eval.LiveAvailable(opts.modelYAML)
				if err != nil {
					return exitWith(ExitUsage, err)
				}
				if !ok {
					return exitErrorf(ExitUsage,
						"--live needs a credential for every provider %s configures; $%s is not set",
						opts.modelYAML, strings.Join(missing, ", $"))
				}
			}
			for _, kind := range kinds {
				recording, err := recordOneTrajectory(ctx, dsn, m, prior, kind, opts)
				if err != nil {
					return exitWith(ExitTransport, err)
				}
				state := "written"
				if recording.written.Unchanged {
					state = "unchanged"
				}
				if err := p.writeLine(
					"%s (%s): %s — %d records (%d model, %d worker), stop %s, digest %s",
					recording.written.Path, kind, state, recording.written.Records,
					recording.modelRecords, recording.workerRecords,
					recording.stop.Reason, short(recording.written.Digest)); err != nil {
					return err
				}
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	flags.StringVar(&opts.auditPath, "audit", defaultAuditPath,
		"the published coverage audit π₀ is read from")
	flags.StringVar(&opts.modelYAML, "model-config", "config/model.yaml", "the model configuration")
	flags.StringVar(&opts.pricesYAML, "prices", "config/prices.yaml", "the price table")
	flags.BoolVar(&opts.modelFree, "model-free", false,
		"record only the model-free run (the deterministic engine, no model records)")
	flags.BoolVar(&opts.fakeModel, "fake-model", false,
		"record only the fake-model run (canned turns through the real transport seam)")
	flags.BoolVar(&opts.live, "live", false,
		"record a live run: call the providers --model-config names and write a trajectory whose "+
			"model exchanges are real. Needs the credential each configured provider uses "+
			"($"+model.EnvMistralAPIKey+", $"+model.EnvAnthropicAPIKey+")")

	return cmd
}

// recordKinds decides which recordings to make. No flag at all means both network-free ones,
// because both is what the corpus ships: the model-free run is the MVP and the fake-model run is
// what exercises the digest matching path.
//
// `--live` is additive and never implied. It is the one kind that costs money, so it is asked for
// explicitly, and asking for it alone is the ordinary case — the other two are already recorded.
func recordKinds(opts *fixtureTrajectoryOptions) []string {
	var out []string
	if opts.modelFree {
		out = append(out, kindModelFree)
	}
	if opts.fakeModel {
		out = append(out, kindFakeModel)
	}
	if opts.live {
		out = append(out, kindLive)
	}
	if len(out) == 0 {
		return []string{kindModelFree, kindFakeModel}
	}
	return out
}

type trajectoryRecording struct {
	written       replay.Written
	stop          engine.Stop
	modelRecords  int
	workerRecords int
}

// recordOneTrajectory replays the fixture into a disposable database and records one run.
//
// The wiring itself lives in `internal/eval`: the same builder serves `fixture record-world`,
// the world-replay gate and the evaluation harness, and a second copy of it here is exactly how
// a world stops being a recording of the investigation the corpus replays.
func recordOneTrajectory(
	ctx context.Context,
	dsn string,
	m *fixture.Manifest,
	prior audit.PriorRecord,
	kind string,
	opts *fixtureTrajectoryOptions,
) (*trajectoryRecording, error) {
	harness, err := eval.NewHarness(ctx, eval.Options{
		Dir:        m.Dir,
		DSN:        dsn,
		Prior:      &prior,
		Kind:       kind,
		ModelYAML:  opts.modelYAML,
		PricesYAML: opts.pricesYAML,
	})
	if err != nil {
		return nil, err
	}
	defer harness.Close()

	// A fresh view of the world for this recording, so its miss counters are its own.
	e, err := harness.Engine(backend.NewRecordedFromWorld(harness.World()))
	if err != nil {
		return nil, err
	}
	stop, err := e.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("fixture: recording %s (%s): %w", m.ID, kind, err)
	}
	written, err := replay.Write(m.TrajectoriesDir(), e.Trajectory())
	if err != nil {
		return nil, err
	}

	out := &trajectoryRecording{written: written, stop: stop}
	for _, record := range e.Trajectory().Records() {
		switch replay.RecordKind(record) {
		case replay.KindModelRequest:
			out.modelRecords++
		case replay.KindWorkerRequest:
			out.workerRecords++
		}
	}
	return out, nil
}

// The two recordings the corpus ships. They are `internal/eval`'s constants, named here so the
// flag handling above reads without a package qualifier on every line.
const (
	kindModelFree = eval.KindModelFree
	kindFakeModel = eval.KindFakeModel
	kindLive      = eval.KindLive
)

// loadPrior reads π₀ from the published coverage audit, turning a malformed one into a usage
// error the way a command must. The reading itself is `internal/eval`'s, so the harness and the
// two recorders read the same prior.
func loadPrior(path string) (audit.PriorRecord, error) {
	prior, err := eval.LoadPrior(path)
	if err != nil {
		return audit.PriorRecord{}, exitErrorf(ExitUsage, "--audit %s: %v", path, err)
	}
	return prior, nil
}
