// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strconv"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `query history` (contracts/cli.md §Queries, FR-032, FR-035).
//
// The only query with no `--as-of`, and the table is laid out to show why. Every row is one
// version, oldest knowledge first, with both intervals side by side: `valid` says when the fact
// was true in production, `observed` says when the graph believed it. A correction is two rows
// whose valid intervals agree and whose observed intervals abut — production did not move, the
// graph changed its mind — and that is exactly the reading an operator comes here for.
//
// Two columns do work that is easy to miss:
//
//   - STATE says `current` when the observed interval is still open and `superseded` when it
//     has been closed. A superseded row is not wrong; it is what was believed then.
//   - ENTITY is printed whenever the node's history contains versions stored under more than
//     one entity id. That happens after a merge: the absorbed entity's versions keep their own
//     id for ever (research §4), and the decisions table underneath says which decision
//     absorbed them and when.
//
// The decisions table is the second half of the answer and is never omitted when there is one:
// a history that showed the versions but not the resolution decisions behind them would make a
// merge look like a spontaneous change of shape (constitution VI).

func newQueryHistoryCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history <namespace>=<value>",
		Short: "Show every version of a node across observed time",
		Long: "history returns all versions of a node, superseded ones included, in the order the graph\n" +
			"learned them, plus every resolution decision taken about it.\n\n" +
			"It takes no instant. Pinning observed time would hide the corrections this query exists\n" +
			"to show: a correction never overwrites, it closes the observed interval of the old\n" +
			"version and opens a new one (constitution II).\n\n" +
			"Versions of entities that were merged away are included, under the entity id they are\n" +
			"stored against, so the history of a merged node does not start at the merge.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseFocusRef(args[0])
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			req := &graphv1.NodeHistoryRequest{Focus: ref.Proto()}
			resp, err := clients.query.NodeHistory(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("query history", err)
			}
			return renderHistory(cmd, global, req, resp.Msg)
		},
	}
	return cmd
}

func renderHistory(cmd *cobra.Command, global *globalOptions, req *graphv1.NodeHistoryRequest, resp *graphv1.NodeHistoryResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	if err := p.writeLine("focus     %s=%s", req.GetFocus().GetNamespace(), req.GetFocus().GetValue()); err != nil {
		return err
	}
	if err := p.writeLine("summary   %d version(s), %d resolution decision(s)\n",
		len(resp.GetVersions()), len(resp.GetDecisions())); err != nil {
		return err
	}

	if len(resp.GetVersions()) == 0 {
		if err := p.writeLine("no versions recorded for this node"); err != nil {
			return err
		}
	} else if err := renderHistoryVersions(p, resp.GetVersions()); err != nil {
		return err
	}

	if len(resp.GetDecisions()) == 0 {
		return nil
	}
	if err := p.writeLine("\nresolution decisions"); err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.GetDecisions()))
	for _, d := range resp.GetDecisions() {
		score := unknownCell
		if d.GetScore() != 0 {
			score = strconv.FormatFloat(d.GetScore(), 'f', 2, 64)
		}
		principal := d.GetPrincipal()
		if principal == "" {
			principal = unknownCell
		}
		rows = append(rows, []string{
			formatTime(d.GetDecidedAt()),
			d.GetKind(),
			ruleCell(d.GetRuleId()),
			score,
			principal,
			d.GetRationale(),
		})
	}
	return p.writeTable([]string{"DECIDED", "KIND", "RULE", "SCORE", "PRINCIPAL", "RATIONALE"}, rows)
}

// renderHistoryVersions prints the version table, adding the ENTITY column only when it has
// something to say — a history confined to one entity id would otherwise repeat the same value
// on every row.
func renderHistoryVersions(p *printer, versions []*graphv1.NodeVersion) error {
	multiEntity := false
	for _, version := range versions[1:] {
		if version.GetEntityId() != versions[0].GetEntityId() {
			multiEntity = true
			break
		}
	}

	headers := []string{"VERSION", "VALID", "OBSERVED", "STATE", "TYPE", "NAME"}
	if multiEntity {
		headers = append([]string{"ENTITY"}, headers...)
	}

	rows := make([][]string, 0, len(versions))
	for _, version := range versions {
		row := []string{
			version.GetVersionId(),
			formatInterval(version.GetValid()),
			formatInterval(version.GetObserved()),
			observedState(version.GetObserved()),
			nodeTypeCell(version),
			nodeLabel(version),
		}
		if multiEntity {
			row = append([]string{version.GetEntityId()}, row...)
		}
		rows = append(rows, row)
	}
	return p.writeTable(headers, rows)
}

// observedState turns an observed interval into the word an operator reads: a version whose
// observed interval is still open is what the graph believes now; a closed one is what it
// believed until something corrected it.
func observedState(observed *graphv1.Interval) string {
	if observed.GetEnd() == nil {
		return "current"
	}
	return "superseded"
}

func ruleCell(ruleID string) string {
	if ruleID == "" {
		return unknownCell
	}
	return ruleID
}
