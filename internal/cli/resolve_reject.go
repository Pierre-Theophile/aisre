// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `resolve reject <refA> <refB> --reason "..."` (contracts/cli.md, FR-040).
//
// The cheapest correction there is, and the one with the longest reach: a rejected pair is never
// merged by any rule again, and a rule that goes on matching it is filed as a conflict for
// review rather than applied.

func newResolveRejectCommand(global *globalOptions) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "reject <refA> <refB>",
		Short: "Refuse a suggested merge and block the pair",
		Long: "reject records that two identifiers name different entities. It needs the `decider`\n" +
			"role and permanently blocks every automated merge of the pair (FR-040).\n\n" +
			"It does not un-merge anything already merged; that is `resolve split`.",
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
			resp, err := clients.resolution.Reject(cmd.Context(), connect.NewRequest(&graphv1.RejectMerge{
				EntityA: a, EntityB: b, Rationale: rationale,
			}))
			if err != nil {
				return remoteError("resolve reject", err)
			}
			return renderDecision(cmd, global, "rejection of "+pairLabel(a, b), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the two identifiers are different entities (required)")
	return cmd
}
