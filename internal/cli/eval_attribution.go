// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/eval/attribution"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// `aisre eval attribution <dir>...` — the attribution evaluation (009 T027, FR-061, research R13).
//
// Every labelled case is replayed from empty into a database of its own, the attributor runs over
// it as serve runs it, and its decisions are scored against labels.yaml: precision, coverage and a
// ten-bin calibration per layer and in aggregate. It exits 4 when the gate in
// internal/eval/attribution/score.go fails (SC-002, SC-003), as `fixture verify` does when a
// replay does. No model and no network: the classifier layer, when a case has one, replays its
// recorded outputs.

func newEvalAttributionCommand(global *globalOptions) *cobra.Command {
	var dsn, modelConfig string
	cmd := &cobra.Command{
		Use:   "attribution <dir>...",
		Short: "Measure signal attribution (precision, coverage, calibration) on the labelled cases and gate on it",
		Long: "attribution replays every labelled case under each <dir> (or <dir> itself, when it is one)\n" +
			"from an empty database, runs the attributor over it, and scores the attributor's own\n" +
			"decisions against the case's labels.yaml, per layer and in aggregate. A signal a person\n" +
			"corrected is scored on the decision the correction overruled.\n\n" +
			"It exits 4 when aggregate precision is below 0.95 (SC-002) or any populated confidence bin\n" +
			"is more than 10 points from its observed accuracy (SC-003). Attributions that follow an\n" +
			"alias a person declared are reported and not gated: when they are wrong, the catalog is.\n\n" +
			"Without --model-config the published ruleset is evaluated; with it, the ruleset its\n" +
			"`attribution:` block names. Each case gets a database of its own, so the role in --db\n" +
			"needs CREATEDB.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cases, err := attribution.LoadAll(args...)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			rules, err := attributionRules(modelConfig)
			if err != nil {
				return err
			}
			url, err := dsnFrom(dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			outcomes, err := attribution.Run(cmd.Context(), fixture.NewStoreFactoryFromDSN(url), cases, catalog.WithRules(rules))
			if err != nil {
				return exitWith(ExitTransport, err)
			}
			summary := attribution.Summarise(outcomes)
			failures := attribution.Gate(summary)

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				if err := p.writeJSON(map[string]any{
					"ruleset": rules.Ruleset, "outcomes": outcomes, "summary": summary, "failures": failures, "pass": len(failures) == 0,
				}); err != nil {
					return err
				}
			} else {
				var report strings.Builder
				attribution.Report(&report, outcomes, summary, failures)
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Ruleset `%s` (τ %.2f, δ %.2f).\n\n%s", rules.Ruleset, rules.Threshold, rules.Margin, report.String()); err != nil {
					return err
				}
			}
			if len(failures) > 0 {
				return exitErrorf(ExitVerification, "attribution gate failed: %s", strings.Join(failures, "; "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "db", "", "PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	cmd.Flags().StringVar(&modelConfig, "model-config", "",
		"a model configuration whose `attribution:` block names the ruleset to evaluate (default: the published one)")
	return cmd
}
