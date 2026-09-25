// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// `fixture calibration` (tasks.md T096, T098; FR-023, SC-004, constitution §Replay CI (c)).
//
// The reliability table over the five published confidence buckets, computed from the corpus's
// **recorded trajectories with zero model calls**. It is run in the same CI job as the trajectory
// gate, on every pull request, and its markdown goes to the job summary.
//
// It needs no database, no credential and no network. Confidences are ledger-computed and
// deterministic, which is the whole reason a per-pull-request calibration is affordable at all:
// there is nothing to sample and nothing to call.
//
// The arithmetic lives in `internal/fixture/calibration.go`, not here, because `eval.yml`'s
// live-run calibration (T110) calls the same functions over the corpus × k. This command is the
// thin edge that reads the corpus and writes two files.

func newFixtureCalibrationCommand(global *globalOptions) *cobra.Command {
	var (
		fixtures    string
		out         string
		summary     string
		previous    string
		minSamples  int
		failOnEmpty bool
	)

	cmd := &cobra.Command{
		Use:   "calibration",
		Short: "The reliability table over the recorded trajectories, with zero model calls",
		Long: "calibration reads every incident fixture's recorded trajectories and decision record and\n" +
			"publishes the reliability table over the five confidence buckets, with Brier and log-loss\n" +
			"(FR-023, SC-004).\n\n" +
			"A bucket holding fewer than --min-samples hypotheses is reported as under-powered rather\n" +
			"than scored: an observed accuracy over three hypotheses is not a measurement.\n\n" +
			"No model is called, no database is opened and no socket is opened.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := fixture.CollectCalibration(fixtures, minSamples)
			if err != nil {
				return exitWith(ExitTransport, err)
			}
			if err := fixture.WriteCalibration(report, out, summary); err != nil {
				return exitWith(ExitTransport, err)
			}

			// The paired comparison (T110, SC-004). It is computed only when a previous report is
			// named: a delta against nothing is not a delta, and printing zero movement for a
			// first run would be the one number a reader must never see.
			var delta *fixture.CalibrationDelta
			if previous != "" {
				before, err := fixture.ReadCalibration(previous)
				if err != nil {
					return exitWith(ExitUsage, err)
				}
				delta = fixture.CompareCalibration(report, before)
			}

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				body, err := report.JSON()
				if err != nil {
					return exitWith(ExitTransport, err)
				}
				if err := p.writeRaw(string(body) + "\n"); err != nil {
					return err
				}
				if delta != nil {
					deltaBody, err := delta.JSON()
					if err != nil {
						return exitWith(ExitTransport, err)
					}
					return p.writeRaw(string(deltaBody) + "\n")
				}
				return nil
			}
			if err := p.writeRaw(report.RenderMarkdown()); err != nil {
				return err
			}
			if delta != nil {
				if err := p.writeRaw("\n" + delta.RenderMarkdown()); err != nil {
					return err
				}
				if summary != "" {
					if err := appendToSummary(summary, delta); err != nil {
						return exitWith(ExitTransport, err)
					}
				}
			}
			// An empty table is the normal state of a corpus whose runs have not been exported
			// yet, so it is not a failure by default. `--fail-on-empty` is there for the day the
			// corpus is supposed to be scoring and silently stops.
			if failOnEmpty && report.Scored == 0 {
				return exitErrorf(ExitVerification,
					"calibration scored 0 hypotheses across %d fixture(s)", report.Fixtures)
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&fixtures, "fixtures", "fixtures/incidents",
		"the incident corpus, or one fixture directory")
	flags.StringVar(&out, "out", "", "write the machine-readable table to this file")
	flags.StringVar(&summary, "summary", "", "write the markdown summary to this file")
	flags.StringVar(&previous, "previous", "",
		"a calibration report from the previous version, as --out wrote it. The Brier and log-loss "+
			"scores are paired against it over the fixtures the two runs SHARE (SC-004): comparing two "+
			"averages taken over different corpora measures the corpus, not the engine.")
	flags.IntVar(&minSamples, "min-samples", fixture.MinBucketSamples,
		"a bucket below this many hypotheses is reported as under-powered rather than scored")
	flags.BoolVar(&failOnEmpty, "fail-on-empty", false,
		"exit 4 when the corpus scores no hypotheses at all")

	return cmd
}

// appendToSummary adds the paired comparison to the markdown file the reliability table was
// written to, so a CI job summary carries both and a reader sees the movement beside the table it
// is a movement of.
func appendToSummary(path string, delta *fixture.CalibrationDelta) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // the path the caller named
	if err != nil {
		return fmt.Errorf("fixture: append to %s: %w", path, err)
	}
	defer file.Close()
	if _, err := file.WriteString("\n" + delta.RenderMarkdown()); err != nil {
		return fmt.Errorf("fixture: append to %s: %w", path, err)
	}
	return nil
}
