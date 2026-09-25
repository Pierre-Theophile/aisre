// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `resolve why <refA> <refB>` (contracts/cli.md, FR-031, constitution VI).
//
// "Why are these two the same entity?" answered the way an operator needs it answered: the verdict
// first, then the claims the graph had, then the decisions in the order they were taken, with the
// principal on the human ones. A merge nobody can explain gets undone by the next skeptical
// operator; this is the command that stops that happening.
//
// `--observed-at` rewinds the answer. The reconstruction is exact for everything a decision
// records — see internal/query/audit.go for what it approximates and why.

func newResolveWhyCommand(global *globalOptions) *cobra.Command {
	var observedAt string

	cmd := &cobra.Command{
		Use:   "why <refA> <refB>",
		Short: "Explain whether two identifiers are the same entity",
		Long: "why answers whether two identifiers resolve to one entity and shows the evidence: the\n" +
			"claims, the rules, the scores and the human decisions, in the order they were observed\n" +
			"(FR-031).\n\n" +
			"With --observed-at it answers as the graph would have answered then.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			refA, err := parseFocusRef(args[0])
			if err != nil {
				return err
			}
			refB, err := parseFocusRef(args[1])
			if err != nil {
				return err
			}
			observed, err := parseInstant("--observed-at", observedAt)
			if err != nil {
				return err
			}
			req := &graphv1.ResolutionAuditRequest{A: refA.Proto(), B: refB.Proto()}
			if !observed.IsZero() {
				req.ObservedAt = timestamppb.New(observed)
			}

			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.query.ResolutionAudit(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("resolve why", err)
			}
			return renderAudit(cmd, global, req, resp.Msg)
		},
	}
	cmd.Flags().StringVar(&observedAt, "observed-at", "",
		"answer as the graph would have answered at this instant (default: as known now)")
	return cmd
}

func renderAudit(cmd *cobra.Command, global *globalOptions, req *graphv1.ResolutionAuditRequest, resp *graphv1.ResolutionAuditResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	verdict := "DIFFERENT entities"
	if resp.GetSameEntity() {
		verdict = "the SAME entity, " + resp.GetCanonicalId()
	}
	if err := p.writeLine("%s=%s and %s=%s are %s",
		req.GetA().GetNamespace(), req.GetA().GetValue(),
		req.GetB().GetNamespace(), req.GetB().GetValue(), verdict); err != nil {
		return err
	}
	if err := p.writeLine("as known at %s\n", observedOrNow(req.GetObservedAt())); err != nil {
		return err
	}

	claimRows := make([][]string, 0, len(resp.GetClaims()))
	for _, claim := range resp.GetClaims() {
		claimRows = append(claimRows, []string{
			formatTime(claim.GetObservedAt()),
			claim.GetSourceId(),
			claim.GetClaim().GetNamespace() + "=" + claim.GetClaim().GetValue(),
			claim.GetEntityId(),
		})
	}
	if err := p.writeTable([]string{"OBSERVED", "SOURCE", "CLAIM", "ENTITY"}, claimRows); err != nil {
		return err
	}
	if err := p.writeLine(""); err != nil {
		return err
	}

	decisionRows := make([][]string, 0, len(resp.GetDecisions()))
	for _, d := range resp.GetDecisions() {
		decisionRows = append(decisionRows, []string{
			formatTime(d.GetDecidedAt()),
			d.GetKind(),
			ruleOrPrincipal(d),
			scoreCell(d.GetScore()),
			d.GetRationale(),
		})
	}
	if err := p.writeTable([]string{"DECIDED", "KIND", "BY", "SCORE", "RATIONALE"}, decisionRows); err != nil {
		return err
	}
	if len(resp.GetDecisions()) == 0 {
		return p.writeLine("\nno resolution decision has been taken about this pair")
	}
	return nil
}

// ruleOrPrincipal says who decided: the person when there was one, the rule otherwise. A row
// with neither is a decision the graph could not attribute, which FR-041 forbids, so printing
// the gap is better than hiding it.
func ruleOrPrincipal(d *graphv1.ResolutionDecision) string {
	if principal := strings.TrimSpace(d.GetPrincipal()); principal != "" {
		return principal
	}
	if rule := strings.TrimSpace(d.GetRuleId()); rule != "" {
		return "rule " + rule
	}
	return unknownCell
}

func scoreCell(score float64) string {
	if score == 0 {
		return unknownCell
	}
	return strconv.FormatFloat(score, 'f', 2, 64)
}

func observedOrNow(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "now"
	}
	return formatTime(ts)
}
