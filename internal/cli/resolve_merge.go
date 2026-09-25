// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `resolve merge <refA> <refB> --reason "..."` (contracts/cli.md, FR-040).
//
// A merge nobody suggested. It produces the same graph as `confirm` and is recorded under a
// different decision kind, so that "how many of our merges did the resolver propose first?" stays
// answerable from the decisions table — which is the measurement that tells us whether the rules
// are any good (constitution V).

func newResolveMergeCommand(global *globalOptions) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "merge <refA> <refB>",
		Short: "Merge two entities the graph did not suggest",
		Long: "merge records that two identifiers name one entity, without a suggestion behind it. It\n" +
			"needs the `decider` role and stores who decided, when and why (FR-041).",
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
			resp, err := clients.resolution.Merge(cmd.Context(), connect.NewRequest(&graphv1.ManualMerge{
				EntityA: a, EntityB: b, Rationale: rationale,
			}))
			if err != nil {
				return remoteError("resolve merge", err)
			}
			return renderDecision(cmd, global, "manual merge of "+pairLabel(a, b), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the two identifiers are the same entity (required)")
	return cmd
}
