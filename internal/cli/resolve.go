// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// `resolve …` (contracts/cli.md §Resolution, FR-031, FR-033, FR-039, FR-040, FR-041).
//
// Six commands, two of which read and four of which decide. The split matters more than it
// looks: `resolve suggestions` and `resolve why` need only a reader token, so reviewing what the
// graph believes about identity never requires a credential that can change it. The four
// deciding commands require the `decider` role and fail with exit code 3 without it — not with a
// generic error, because an SRE reading a pipeline log has to be able to tell "your token is
// wrong" from "the server is down" without opening the output.
//
// References are written the way every other command writes them: `otel.service.name=checkout`,
// `k8s.deployment=shop/checkout-svc`, or `id:<entity_id>` for a canonical id out of a previous
// answer. Naming an entity by a name a source used rather than by an id is what lets the same
// decision be replayed into a graph rebuilt from empty, where the ids may legitimately differ
// (research §4).

func newResolveCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Inspect and decide entity-resolution questions",
		Long: "resolve shows what the graph believes about identity and lets a person correct it.\n\n" +
			"`suggestions` and `why` are reads. `confirm`, `reject`, `merge` and `split` are\n" +
			"decisions: they need the `decider` role, they record who made them, and they outrank\n" +
			"every automated rule from then on (FR-040, FR-041).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(
		newResolveSuggestionsCommand(global),
		newResolveWhyCommand(global),
		newResolveConfirmCommand(global),
		newResolveRejectCommand(global),
		newResolveMergeCommand(global),
		newResolveSplitCommand(global),
		// 003 plan item 2: the edge half of resolution.
		newResolveProposalsCommand(global),
		newResolveConfirmDependencyCommand(global),
		newResolveRejectDependencyCommand(global),
	)
	return cmd
}

// decisionRef parses one side of a decision and renders it back in the spelling the server
// expects. Parsing here rather than server-side is what makes a typo cost a usage error instead
// of a round trip and a rejected event.
func decisionRef(raw string) (string, error) {
	ref, err := parseFocusRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Namespace == query.IDNamespace {
		return query.IDNamespace + ":" + ref.Value, nil
	}
	return ref.String(), nil
}

// requireReason refuses a decision with no rationale. Constitution VI requires every merge to
// record a human-readable reason, and a reason nobody wrote is not one; the graph would accept
// the event and the next person to read the audit would learn nothing.
func requireReason(reason string) (string, error) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return "", exitErrorf(ExitUsage,
			"--reason is required: every resolution decision records why it was made (constitution VI)")
	}
	return trimmed, nil
}

// parseDetachRefs parses the `--detach ns=value,...` list of a split.
func parseDetachRefs(values []string) ([]*graphv1.Ref, error) {
	refs := make([]*graphv1.Ref, 0, len(values))
	for _, raw := range values {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		ref, err := graph.ParseRef(trimmed)
		if err != nil {
			return nil, exitErrorf(ExitUsage,
				"--detach %q: write each claim as <namespace>=<value> (e.g. k8s.deployment=shop/checkout-svc)", raw)
		}
		refs = append(refs, ref.Proto())
	}
	if len(refs) == 0 {
		return nil, exitErrorf(ExitUsage,
			"--detach is required: a split has to say which identifiers leave the entity")
	}
	return refs, nil
}

// renderDecision prints what the graph did with a decision.
//
// DUPLICATE_NOOP is reported as plainly as APPLIED rather than as a failure: the event id of a
// human decision is derived from the decision itself, so re-running the same command with the
// same reason is the same decision and changes nothing (server/resolution.go). Saying "already
// recorded" is the honest answer and exits 0.
func renderDecision(cmd *cobra.Command, global *globalOptions, what string, result *graphv1.IngestResult) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(result)
	}
	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		return p.writeLine("%s recorded as %s (observed %s)",
			what, result.GetEventId(), formatTime(result.GetObservedAt()))
	case graphv1.IngestResult_DUPLICATE_NOOP:
		return p.writeLine("%s was already recorded as %s; nothing changed",
			what, result.GetEventId())
	case graphv1.IngestResult_REJECTED:
		return exitErrorf(ExitRejected, "%s was refused (%s): %s",
			what, result.GetReasonCode(), result.GetReasonDetail())
	default:
		return exitErrorf(ExitTransport, "%s: unexpected status %s", what, result.GetStatus())
	}
}

// pairLabel names the pair a decision is about, for the one-line confirmation.
func pairLabel(a, b string) string { return fmt.Sprintf("%s ↔ %s", a, b) }
