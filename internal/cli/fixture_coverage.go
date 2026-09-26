// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/deploycoverage"
)

// `fixture coverage` (004 T113, FR-069, SC-001, SC-002): a deploy recording's coverage against the
// platform's own answer, every difference named. It reads files and nothing else — no database, no
// credential — so it runs the same over a public twin, a campaign recording in the private corpus, and
// a recording a live run just wrote.

type fixtureCoverageOptions struct {
	environments []string
}

func newFixtureCoverageCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureCoverageOptions{}
	cmd := &cobra.Command{
		Use:   "coverage <dir>...",
		Short: "Deploy coverage against the platform's own answer, every difference named (FR-069)",
		Long: "coverage compares, for each GitHub or Vercel source a recording holds, the deployments\n" +
			"the platform itself lists as completed and in scope against the rollout changes the\n" +
			"feeder produced, and names every difference with its reason. The platform's answer is\n" +
			"read from payloads/ by code that shares nothing with the feeder's mapper, so the feeder\n" +
			"is not asked to grade itself.\n\n" +
			"Deployments the platform listed and the scope excluded — a preview, a status that never\n" +
			"reached success — are named too, so a reader can see they were considered.\n\n" +
			"It exits 4 when any recording is incomplete: a listed deployment with no rollout, or a\n" +
			"rollout the platform's answer does not support.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFixtureCoverage(cmd, global, opts, args)
		},
	}
	cmd.Flags().StringSliceVar(&opts.environments, "environments", nil,
		"GitHub environments in scope; default: the list the recording's run declared, else production")
	return cmd
}

func runFixtureCoverage(cmd *cobra.Command, global *globalOptions, opts *fixtureCoverageOptions, dirs []string) error {
	type recording struct {
		Recording string                  `json:"recording"`
		Reports   []deploycoverage.Report `json:"reports"`
	}
	var out []recording
	incomplete := 0
	for _, dir := range dirs {
		reports, err := deploycoverage.Measure(dir, deploycoverage.Scope{GitHubEnvironments: opts.environments})
		if err != nil {
			return exitErrorf(ExitUsage, "fixture coverage: %v", err)
		}
		for _, r := range reports {
			if !r.Complete() {
				incomplete++
			}
		}
		out = append(out, recording{Recording: dir, Reports: reports})
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		if err := p.writeJSON(map[string]any{"recordings": out, "incomplete": incomplete}); err != nil {
			return err
		}
	} else {
		for _, rec := range out {
			if err := p.writeLine("## %s", rec.Recording); err != nil {
				return err
			}
			for _, r := range rec.Reports {
				if err := p.writeRaw(r.String()); err != nil {
					return err
				}
			}
		}
	}
	if incomplete > 0 {
		return exitErrorf(ExitVerification, "fixture coverage: %d platform report(s) incomplete; every "+
			"difference is named above", incomplete)
	}
	return nil
}
