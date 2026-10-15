// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/eval/retrieval"
)

// `aisre eval retrieval <dir>` — the knowledge retrieval evaluation (008 T016, FR-042, US4).
//
// Unlike `eval run` it needs no database, no recorded world and no model: each retrieval fixture is
// an in-memory corpus and store, so it is cheap enough for every pull request, and ci.yml runs it
// there. It prints recall@3 and MRR per fixture and per retrieval mode, and exits 4 when the gate
// in internal/eval/retrieval/thresholds.go fails, as `fixture verify` does when a replay does.

func newEvalRetrievalCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "retrieval <dir>",
		Short: "Measure knowledge retrieval (recall@3, MRR) on the retrieval fixtures and gate on it",
		Long: "retrieval runs every retrieval fixture under <dir> (or <dir> itself, when it is one)\n" +
			"through the knowledge worker in each retrieval mode, prints recall@3 and MRR per fixture\n" +
			"and in aggregate, and exits 4 when the gate fails: a document a fixture says must never\n" +
			"be cited was cited, lexical retrieval fell below its measured floor, or it did worse than\n" +
			"matching on the registered summary alone.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fixtures, err := retrieval.LoadAll(args[0])
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			results, err := retrieval.Run(cmd.Context(), fixtures)
			if err != nil {
				return exitWith(ExitVerification, err)
			}
			aggregates := retrieval.Summarise(results)
			failures := retrieval.Gate(aggregates)

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				if err := p.writeJSON(map[string]any{
					"results": results, "aggregates": aggregates, "failures": failures, "pass": len(failures) == 0,
				}); err != nil {
					return err
				}
			} else {
				var report strings.Builder
				retrieval.Report(&report, results, aggregates, failures)
				if _, err := fmt.Fprint(cmd.OutOrStdout(), report.String()); err != nil {
					return err
				}
			}
			if len(failures) > 0 {
				return exitErrorf(ExitVerification, "retrieval gate failed: %s", strings.Join(failures, "; "))
			}
			return nil
		},
	}
}
