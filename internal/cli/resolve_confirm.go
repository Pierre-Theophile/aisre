// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `resolve confirm <refA> <refB> --reason "..."` (contracts/cli.md, FR-040).
//
// The command US6 exists for: the graph has listed a pair as a suggestion and a person says yes.
// From then on both names return one entity, the audit names the person as the deciding
// evidence, and no automated rule may change its mind about them (FR-040).

func newResolveConfirmCommand(global *globalOptions) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "confirm <refA> <refB>",
		Short: "Accept a suggested merge",
		Long: "confirm records that two identifiers name one entity. It needs the `decider` role and\n" +
			"stores who decided, when and why (FR-041).\n\n" +
			"The decision outranks every automated rule from then on: a rule that later wants to\n" +
			"merge either side into something else is recorded as a conflict rather than applied.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := decisionRef(args[0])
			if err != nil {
				return err
			}
			b, err := decisionRef(args[1])
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
			resp, err := clients.resolution.Confirm(cmd.Context(), connect.NewRequest(&graphv1.ConfirmMerge{
				EntityA: a, EntityB: b, Rationale: rationale,
			}))
			if err != nil {
				return remoteError("resolve confirm", err)
			}
			return renderDecision(cmd, global, "merge of "+pairLabel(a, b), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the two identifiers are the same entity (required)")
	return cmd
}
