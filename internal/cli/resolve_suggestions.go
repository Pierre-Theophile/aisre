// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// `resolve suggestions [--for <ref>] [--status pending]` (contracts/cli.md, FR-033).
//
// The work queue of a conservative resolver. Highest score first, because that is the order a
// person should spend attention in, and with the rationale in full rather than truncated: the
// whole point of the row is that somebody can decide from it without going to look.

func newResolveSuggestionsCommand(global *globalOptions) *cobra.Command {
	var (
		focus  string
		status string
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "suggestions",
		Short: "List entity-resolution pairs awaiting a decision",
		Long: "suggestions lists the pairs a probable rule matched and no person has decided on, with\n" +
			"the rule, the score and the reason (FR-033).\n\n" +
			"Nothing in this list has been merged: a probable rule may only suggest (ADR-0001 D6).\n" +
			"Filter with --status to see what was confirmed, rejected, or is in conflict with a\n" +
			"human decision.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := &graphv1.SuggestionsRequest{Status: strings.TrimSpace(status), Cursor: cursor}
			if strings.TrimSpace(focus) != "" {
				ref, err := parseFocusRef(focus)
				if err != nil {
					return err
				}
				req.Focus = ref.Proto()
			}
			if limit < 0 {
				return exitErrorf(ExitUsage, "--limit %d: want a positive number", limit)
			}
			req.Limit = uint32(limit)

			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.query.Suggestions(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("resolve suggestions", err)
			}
			return renderSuggestions(cmd, global, resp.Msg)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&focus, "for", "", "only pairs involving this node (<namespace>=<value> or id:<entity_id>)")
	flags.StringVar(&status, "status", "",
		"only pairs in this state: "+strings.Join(query.SuggestionStatuses, ", "))
	flags.IntVar(&limit, "limit", 0, "maximum pairs to return (default 50)")
	flags.StringVar(&cursor, "cursor", "", "continue a previous listing from its next_cursor")
	return cmd
}

func renderSuggestions(cmd *cobra.Command, global *globalOptions, resp *graphv1.SuggestionsResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	rows := make([][]string, 0, len(resp.GetSuggestions()))
	for _, s := range resp.GetSuggestions() {
		rows = append(rows, []string{
			strconv.FormatFloat(s.GetScore(), 'f', 2, 64),
			s.GetRuleId(),
			s.GetStatus(),
			suggestionSide(s.GetA()),
			suggestionSide(s.GetB()),
		})
	}
	if err := p.writeTable([]string{"SCORE", "RULE", "STATUS", "A", "B"}, rows); err != nil {
		return err
	}
	if len(resp.GetSuggestions()) == 0 {
		return p.writeLine("\nno suggestions match")
	}
	if err := p.writeLine(""); err != nil {
		return err
	}
	for _, s := range resp.GetSuggestions() {
		if err := p.writeLine("%s  %s", s.GetPairKey(), s.GetRationale()); err != nil {
			return err
		}
	}
	if resp.GetNextCursor() != "" {
		return p.writeLine("\nmore: --cursor %s", resp.GetNextCursor())
	}
	return nil
}

// suggestionSide names one half of a pair the way a person would recognize it: the first
// identifier a source claimed, falling back to the display name and then the canonical id.
func suggestionSide(node *graphv1.NodeVersion) string {
	if aliases := node.GetAliases(); len(aliases) > 0 {
		return aliases[0].GetNamespace() + "=" + aliases[0].GetValue()
	}
	if name := node.GetDisplayName(); name != "" {
		return name
	}
	return node.GetEntityId()
}
