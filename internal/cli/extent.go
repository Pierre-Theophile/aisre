// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `extent` (contracts/cli.md §Server and operations, FR-052).
//
// The question behind this command is "how much of reality does this graph claim to have
// seen?". An answer drawn from an hour a feeder was disconnected is not wrong so much as
// unfounded, and a consumer that cannot see the gap has no way to know. So gaps are printed as
// first-class rows, and a boundary the graph genuinely does not know is printed as unknown,
// never as a zero timestamp (FR-011).

func newExtentCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "extent",
		Short: "Show earliest and latest observed time, per-source checkpoints and known gaps",
		Long: "extent reports the observed-time span of the whole event log and, per source, the last\n" +
			"checkpoint it recorded and the gaps it admitted to. Use it to judge how complete an\n" +
			"answer from this graph can be.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.query.Extent(cmd.Context(), connect.NewRequest(&graphv1.ExtentRequest{}))
			if err != nil {
				return remoteError("extent", err)
			}
			return renderExtent(cmd, global, resp.Msg)
		},
	}
}

func renderExtent(cmd *cobra.Command, global *globalOptions, extent *graphv1.Extent) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(extent)
	}

	if err := p.writeLine("observed  %s .. %s",
		formatTime(extent.GetEarliestObserved()), formatTime(extent.GetLatestObserved())); err != nil {
		return err
	}
	if len(extent.GetSources()) == 0 {
		return p.writeLine("\nno source has recorded a checkpoint yet")
	}
	if err := p.writeLine(""); err != nil {
		return err
	}

	rows := make([][]string, 0, len(extent.GetSources()))
	for _, source := range extent.GetSources() {
		rows = append(rows, []string{
			source.GetSourceId(),
			formatTime(source.GetLastCheckpoint()),
			fmt.Sprintf("%d", len(source.GetGaps())),
			formatGaps(source.GetGaps()),
		})
	}
	return p.writeTable([]string{"SOURCE", "LAST CHECKPOINT", "GAPS", "GAP INTERVALS"}, rows)
}

// formatGaps renders the intervals a source admits it did not observe. An unknown boundary is
// printed as `unknown`, because FR-011 forbids substituting a guess for one.
func formatGaps(gaps []*graphv1.Interval) string {
	if len(gaps) == 0 {
		return unknownCell
	}
	rendered := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		start := formatTime(gap.GetStart())
		if gap.GetStartUnknown() {
			start = "unknown"
		}
		end := formatTime(gap.GetEnd())
		if gap.GetEndUnknown() {
			end = "unknown"
		}
		rendered = append(rendered, start+".."+end)
	}
	return strings.Join(rendered, " ")
}
