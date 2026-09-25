// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// The proposed-dependency review queue and its two decisions (003 FR-029, FR-122).
//
// Three commands rather than flags on `resolve suggestions`, `confirm` and `reject`, because the
// two things are not the same question. A suggestion asks *are these two names one entity?* — a
// symmetric question about refs. A proposal asks *does this edge exist?* — a DIRECTED question
// about two entities that are definitely not one. Folding them together would give a reviewer an
// `A` and a `B` with a direction nothing names, and which end depends on which is the only thing
// they need to know.
//
// What makes this worth a command at all: until it existed, a proposal was reachable only by
// reading the event log, and a proposal nobody can find is a proposal nobody will decide. The
// evidence travels with the row for the same reason — a reviewer is being asked about a
// relationship visible from neither end, so the score alone is not something a person can act on.

func newResolveProposalsCommand(global *globalOptions) *cobra.Command {
	var (
		focus  string
		status string
		limit  int
		cursor string
	)

	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "List proposed dependencies awaiting a decision",
		Long: "proposals lists the edges a rule suggested and no person has decided on, with the\n" +
			"rule, the score, the reason and the evidence it matched on (FR-029).\n\n" +
			"Nothing in this list is an edge. A proposal becomes one only when somebody runs\n" +
			"`resolve confirm-dependency`; until then no query traverses it.\n\n" +
			"Filter with --status to see what was confirmed, rejected, or is in conflict — a\n" +
			"conflict being a rule that went on proposing something a person had rejected, which\n" +
			"is recorded rather than applied.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := &graphv1.ProposedDependenciesRequest{
				Status: strings.TrimSpace(status), Cursor: cursor,
			}
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
			resp, err := clients.query.ProposedDependencies(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("resolve proposals", err)
			}
			return renderProposals(cmd, global, resp.Msg)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&focus, "for", "",
		"only proposals involving this node at either end (<namespace>=<value> or id:<entity_id>)")
	flags.StringVar(&status, "status", "",
		"only proposals in this state: "+strings.Join(query.ProposalStatuses, ", "))
	flags.IntVar(&limit, "limit", 0, "maximum proposals to return (default 50)")
	flags.StringVar(&cursor, "cursor", "", "continue a previous listing from its next_cursor")
	return cmd
}

func renderProposals(cmd *cobra.Command, global *globalOptions, resp *graphv1.ProposedDependenciesResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	rows := make([][]string, 0, len(resp.GetProposals()))
	for _, proposal := range resp.GetProposals() {
		rows = append(rows, []string{
			strconv.FormatFloat(proposal.GetScore(), 'f', 2, 64),
			proposal.GetRuleId(),
			proposal.GetStatus(),
			suggestionSide(proposal.GetSrc()),
			// The arrow is the point of the row: it is what says which end depends on which.
			graph.EdgeTypeFromProto(proposal.GetType()).String(),
			suggestionSide(proposal.GetDst()),
		})
	}
	if err := p.writeTable(
		[]string{"SCORE", "RULE", "STATUS", "DEPENDENT", "TYPE", "DEPENDENCY"}, rows); err != nil {
		return err
	}
	if len(resp.GetProposals()) == 0 {
		return p.writeLine("\nno proposed dependencies match")
	}
	if err := p.writeLine(""); err != nil {
		return err
	}
	// The rationale and the evidence in full rather than truncated: the whole point of the row is
	// that somebody can decide from it without going to look.
	for _, proposal := range resp.GetProposals() {
		if err := p.writeLine("%s  %s", proposal.GetProposalKey(), proposal.GetRationale()); err != nil {
			return err
		}
		for key, value := range proposal.GetEvidence().AsMap() {
			if err := p.writeLine("    %s: %v", key, value); err != nil {
				return err
			}
		}
		if by := proposal.GetDecidedBy(); by != "" {
			if err := p.writeLine("    decided by %s", by); err != nil {
				return err
			}
		}
	}
	if resp.GetNextCursor() != "" {
		return p.writeLine("\nmore: --cursor %s", resp.GetNextCursor())
	}
	return nil
}

func newResolveConfirmDependencyCommand(global *globalOptions) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "confirm-dependency <src> <dst>",
		Short: "Accept a proposed dependency, creating the edge",
		Long: "confirm-dependency records that <src> depends on <dst>, and CREATES the edge — which\n" +
			"is the whole meaning of a proposal not being one until confirmed. It needs the\n" +
			"`decider` role and stores who decided, when and why (FR-041).\n\n" +
			"The order matters: <src> is the dependent, <dst> is the thing depended on.\n\n" +
			"There must already be a proposal for this triple. To assert an edge on your own\n" +
			"authority with nothing having proposed it, use the ingest path instead — confirming\n" +
			"a proposal nobody made would claim the graph suggested something it never did.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, dst, err := dependencyEnds(args[0], args[1])
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
			resp, err := clients.resolution.ConfirmDependency(cmd.Context(),
				connect.NewRequest(&graphv1.ConfirmDependency{
					Src: src, Dst: dst, Type: graphv1.EdgeType_DEPENDS_ON, Rationale: rationale,
				}))
			if err != nil {
				return remoteError("resolve confirm-dependency", err)
			}
			return renderDecision(cmd, global, "dependency "+dependencyLabel(src, dst), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "",
		"why the dependency is real (required)")
	return cmd
}

func newResolveRejectDependencyCommand(global *globalOptions) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "reject-dependency <src> <dst>",
		Short: "Refuse a proposed dependency",
		Long: "reject-dependency records that <src> does not depend on <dst>. No edge is created,\n" +
			"and the decision is durable: a rule that goes on proposing the same triple is\n" +
			"recorded as being in conflict with you, never as reopening the question, whatever\n" +
			"score it now carries (FR-122).\n\n" +
			"It needs the `decider` role and stores who decided, when and why (FR-041).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, dst, err := dependencyEnds(args[0], args[1])
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
			resp, err := clients.resolution.RejectDependency(cmd.Context(),
				connect.NewRequest(&graphv1.RejectDependency{
					Src: src, Dst: dst, Type: graphv1.EdgeType_DEPENDS_ON, Rationale: rationale,
				}))
			if err != nil {
				return remoteError("resolve reject-dependency", err)
			}
			return renderDecision(cmd, global, "dependency "+dependencyLabel(src, dst), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "",
		"why the dependency is not real (required)")
	return cmd
}

// dependencyEnds parses both ends, refusing a self-dependency here rather than after a round trip.
//
// Unlike decisionRef, these stay as refs rather than being rendered to entity ids: the proposal is
// keyed on the triple, and the server resolves both ends through the merge chain exactly as the
// projector did when it filed the row.
func dependencyEnds(rawSrc, rawDst string) (*graphv1.Ref, *graphv1.Ref, error) {
	src, err := parseFocusRef(rawSrc)
	if err != nil {
		return nil, nil, err
	}
	dst, err := parseFocusRef(rawDst)
	if err != nil {
		return nil, nil, err
	}
	if src == dst {
		return nil, nil, exitErrorf(ExitUsage,
			"%s is named at both ends; a self-dependency carries no information", rawSrc)
	}
	return src.Proto(), dst.Proto(), nil
}

// dependencyLabel renders the pair in the direction asserted, because the direction is the claim.
func dependencyLabel(src, dst *graphv1.Ref) string {
	return graph.RefFromProto(src).String() + " → " + graph.RefFromProto(dst).String()
}
