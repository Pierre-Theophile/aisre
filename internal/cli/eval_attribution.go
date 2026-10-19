// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/embedding"
	"github.com/Pierre-Theophile/aisre/internal/eval/attribution"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
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
	var recordVectors bool
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
			"needs CREATEDB.\n\n" +
			"A case with a classifier.yaml runs layer 3 over its recorded vectors.jsonl. --record-vectors\n" +
			"replays those cases through the embedding model --model-config names and writes the\n" +
			"vectors they used; it is the only path that calls an embedding provider.",
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
			if recordVectors {
				return recordAttributionVectors(cmd, cases, modelConfig, fixture.NewStoreFactoryFromDSN(url), rules)
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
	cmd.Flags().BoolVar(&recordVectors, "record-vectors", false,
		"replay the cases that have a classifier through the live embedding model and write their vectors.jsonl")
	cmd.Flags().StringVar(&modelConfig, "model-config", "",
		"a model configuration whose `attribution:` block names the ruleset to evaluate (default: the published one)")
	return cmd
}

// recordAttributionVectors writes the vectors.jsonl of every case that runs the classifier.
func recordAttributionVectors(cmd *cobra.Command, cases []*attribution.Case, modelConfig string, newStore fixture.StoreFactory, rules catalog.Rules) error {
	if strings.TrimSpace(modelConfig) == "" {
		return exitErrorf(ExitUsage, "--record-vectors needs --model-config, whose embedding block names the model")
	}
	cfg, err := model.LoadConfig(modelConfig)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	live, reason, err := embedding.FromConfig(cfg.Embedding)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	if live == nil {
		return exitErrorf(ExitUsage, "--record-vectors needs an embedding model: %s", reason)
	}
	for _, c := range cases {
		if c.Calibration == nil {
			continue
		}
		recorder := embedding.NewRecorder(live)
		c.Embedder = recorder
		if _, err := attribution.Run(cmd.Context(), newStore, []*attribution.Case{c}, catalog.WithRules(rules)); err != nil {
			return exitWith(ExitTransport, err)
		}
		path := filepath.Join(c.Dir, embedding.VectorsFile)
		if err := recorder.WriteFile(path); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: recorded %s from %s\n", c.ID, embedding.VectorsFile, live.Model()); err != nil {
			return err
		}
	}
	return nil
}
