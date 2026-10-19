// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
)

// `attribute …` (feature 009, contracts/catalog.md §4, FR-039–FR-045).
//
// "Which service is this alert about, and why?" and "no, it is about that one". `why` is a read;
// `correct` and `revert` are human decisions, signed and printed with their principal. A correction
// outranks every automated layer and survives every re-attribution until it is reverted.

func newAttributeCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attribute",
		Short: "Explain and correct which catalog entries a signal is attributed to",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(newAttributeWhyCommand(global), newAttributeCorrectCommand(global), newAttributeRevertCommand(global))
	return cmd
}

func attributedLabel(items []*graphv1.Attributed) string {
	if len(items) == 0 {
		return "unknown"
	}
	out := make([]string, 0, len(items))
	for _, a := range items {
		out = append(out, fmt.Sprintf("%s (%s %s, %.2f)", a.GetEntrySlug(), a.GetLayer(), a.GetRuleId(), a.GetScore()))
	}
	return strings.Join(out, ", ")
}

func newAttributeWhyCommand(global *globalOptions) *cobra.Command {
	var asOf string
	cmd := &cobra.Command{
		Use:   "why <signal-ref>",
		Short: "Show what a signal is attributed to, by which rule, and every decision before",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseFocusRef(args[0])
			if err != nil {
				return err
			}
			at, err := catalogAsOf(asOf)
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.catalog.WhyAttributed(cmd.Context(), connect.NewRequest(&graphv1.WhyAttributedRequest{
				Signal: ref.Proto(), AsOf: at,
			}))
			if err != nil {
				return remoteError("attribute why", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			var b strings.Builder
			current := resp.Msg.GetCurrent()
			fmt.Fprintf(&b, "%s: %s\n", ref, attributedLabel(current.GetEntries()))
			if current == nil {
				b.WriteString("no decision yet\n")
			}
			for _, d := range resp.Msg.GetHistory() {
				by := d.GetPrincipal()
				if by == "" {
					by = d.GetRuleset()
				}
				state := ""
				if d.GetSupersededByEventId() != "" {
					state = " — superseded by " + d.GetSupersededByEventId()
				}
				fmt.Fprintf(&b, "  %s  %s %s by %s: %s%s\n", formatTime(d.GetDecidedAt()), d.GetKind(),
					d.GetEventId(), by, attributedLabel(d.GetEntries()), state)
				if len(d.GetSuggestions()) > 0 {
					fmt.Fprintf(&b, "      suggestions: %s\n", attributedLabel(d.GetSuggestions()))
				}
			}
			return p.writeRaw(b.String())
		},
	}
	cmd.Flags().StringVar(&asOf, "as-of", "", "the answer as it was then (RFC 3339, -30m or now)")
	return cmd
}

func newAttributeCorrectCommand(global *globalOptions) *cobra.Command {
	var entries []string
	var name, env, reason string
	var none bool
	cmd := &cobra.Command{
		Use:   "correct <signal-ref> --entry <slug>... [--name <ns>=<value>]",
		Short: "Say which entries a signal is about; with --name, every signal carrying that name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signal, err := parseFocusRef(args[0])
			if err != nil {
				return err
			}
			if len(entries) == 0 && !none {
				return exitErrorf(ExitUsage, "--entry is required; --none says the signal is about no catalog entry")
			}
			if len(entries) > 0 && none {
				return exitErrorf(ExitUsage, "--entry and --none contradict each other")
			}
			for _, slug := range entries {
				if err := checkSlug(slug); err != nil {
					return err
				}
			}
			msg := &graphv1.CorrectAttribution{
				Signal: signal.Proto(), EntrySlugs: entries, Scope: "signal", Environment: env,
				Rationale: decisionReason(reason, "attribute correct "+signal.String()),
			}
			what := "correction of " + signal.String()
			if name != "" {
				ref, err := parseName(name)
				if err != nil {
					return err
				}
				if len(entries) != 1 {
					return exitErrorf(ExitUsage, "a name-level correction names exactly one --entry")
				}
				msg.Scope, msg.Name = "name", ref.Proto()
				what = "correction of the name " + catalog.AliasKeyOf(ref, env)
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.resolution.CorrectAttribution(cmd.Context(), connect.NewRequest(msg))
			if err != nil {
				return remoteError("attribute correct", err)
			}
			return renderDecision(cmd, global, what, resp.Msg)
		},
	}
	cmd.Flags().StringSliceVar(&entries, "entry", nil, "the entry slug(s) the signal is about")
	cmd.Flags().BoolVar(&none, "none", false, "the signal is about no catalog entry")
	cmd.Flags().StringVar(&name, "name", "", "correct the name, not only this signal: <namespace>=<value>")
	cmd.Flags().StringVar(&env, "env", "", "with --name: only in this environment")
	cmd.Flags().StringVar(&reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

func newAttributeRevertCommand(global *globalOptions) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "revert <correction-event-id>",
		Short: "Undo a correction from now on; it stays in the history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return exitErrorf(ExitUsage, "name the correction by its event id (attribute why prints it)")
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.resolution.RevertCorrection(cmd.Context(), connect.NewRequest(&graphv1.RevertCorrection{
				CorrectionEventId: id, Rationale: decisionReason(reason, "attribute revert "+id),
			}))
			if err != nil {
				return remoteError("attribute revert", err)
			}
			return renderDecision(cmd, global, "revert of "+id, resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}
