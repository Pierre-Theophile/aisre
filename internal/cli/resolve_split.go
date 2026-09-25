// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `resolve split <ref> --detach <ns>=<value>[,...] --reason "..."` (contracts/cli.md, FR-039).
//
// Undoing a merge. The detached identifiers move to a new entity; the original keeps its id and
// its history, and a query as of an instant before the split still returns the merged view
// (US6 scenario 4). Nothing is deleted, here or in the graph.

func newResolveSplitCommand(global *globalOptions) *cobra.Command {
	var (
		reason string
		detach []string
	)

	cmd := &cobra.Command{
		Use:   "split <ref>",
		Short: "Detach identifiers from an entity into one of their own",
		Long: "split reverses a merge. The identifiers named by --detach leave the entity and become a\n" +
			"new one; everything else stays where it is.\n\n" +
			"History is kept: a query as of an instant before the split still shows the merged view,\n" +
			"and the identifiers that left still resolve — to the entity they now belong to (FR-039).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entity, err := decisionRef(args[0])
			if err != nil {
				return err
			}
			refs, err := parseDetachRefs(detach)
			if err != nil {
				return err
			}
			rationale, err := requireReason(reason)
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.resolution.Split(cmd.Context(), connect.NewRequest(&graphv1.SplitEntity{
				EntityId: entity, DetachClaims: refs, Rationale: rationale,
			}))
			if err != nil {
				return remoteError("resolve split", err)
			}
			names := make([]string, 0, len(refs))
			for _, ref := range refs {
				names = append(names, ref.GetNamespace()+"="+ref.GetValue())
			}
			return renderDecision(cmd, global,
				"split of "+entity+" detaching "+strings.Join(names, ", "), resp.Msg)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&reason, "reason", "", "why the identifiers are not the same entity (required)")
	flags.StringSliceVar(&detach, "detach", nil,
		"identifiers to detach, as <namespace>=<value> (required, repeatable or comma-separated)")
	return cmd
}
