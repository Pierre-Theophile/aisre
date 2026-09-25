// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// `query …` (contracts/cli.md §Queries, FR-035).
//
// Every read of the graph is reachable from here, in both renderings the contract requires: a
// table for the person who was paged, and canonical JSON for the machine that will consume it
// — the same bytes a golden fixture holds, so an invocation and a recorded expectation can be
// diffed directly.
//
// Two conveniences that are contract rather than sugar:
//
//   - Instants may be written relative. `--as-of -30m` is what someone types at 03:00 when the
//     page arrived half an hour ago, and an SRE tool that insisted on RFC 3339 for that would
//     be one an SRE stops using. Absolute times are still absolute: the relative form is only
//     recognized for a value that starts with a sign.
//   - A focus is a *name a source used*, not an internal id: `otel.service.name=checkout`,
//     `k8s.deployment=shop/checkout`. `id:<entity_id>` addresses the canonical id directly, for
//     a caller that already has one out of a previous answer.

func newQueryCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query",
		Short: "Read the graph as of an instant",
		Long: "query reads the bitemporal graph. Every read takes a valid-time instant and an optional\n" +
			"observed-time instant, and answers exactly as the graph was true then, as known then\n" +
			"(constitution II).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(newQuerySubgraphCommand(global))
	cmd.AddCommand(newQueryDiffCommand(global))
	cmd.AddCommand(newQueryImpactCommand(global))
	cmd.AddCommand(newQueryPointersCommand(global))
	cmd.AddCommand(newQueryHistoryCommand(global))
	return cmd
}

// parseFocusRef parses the focus spellings contracts/cli.md publishes: `<namespace>=<value>`
// and the canonical `id:<entity_id>`.
func parseFocusRef(raw string) (graph.Ref, error) {
	trimmed := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(trimmed, query.IDNamespace+":"); ok && rest != "" {
		return graph.Ref{Namespace: query.IDNamespace, Value: rest}, nil
	}
	ref, err := graph.ParseRef(trimmed)
	if err != nil {
		return graph.Ref{}, exitErrorf(ExitUsage,
			"focus %q: write it as <namespace>=<value> (e.g. otel.service.name=checkout) or id:<entity_id>", raw)
	}
	return ref, nil
}

// parseInstant accepts an RFC 3339 timestamp, a duration relative to now (`-30m`, `+2h`), or
// the word `now`. The zero time is returned for an empty value, which every caller reads as
// "unset".
func parseInstant(flag, raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	switch {
	case value == "":
		return time.Time{}, nil
	case strings.EqualFold(value, "now"):
		return time.Now().UTC(), nil
	case value[0] == '-' || value[0] == '+':
		offset, err := time.ParseDuration(value)
		if err != nil {
			return time.Time{}, exitErrorf(ExitUsage,
				"%s %q: a relative instant is a Go duration such as -30m or +2h", flag, raw)
		}
		return time.Now().UTC().Add(offset), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, exitErrorf(ExitUsage,
			"%s %q: want an RFC 3339 instant (2026-09-01T14:32:00Z), a relative duration (-30m) or `now`", flag, raw)
	}
	return t.UTC(), nil
}

// formatInterval renders a graph interval for a table cell, printing an unbounded end as `∞`
// and a bound the graph does not actually know with a trailing `?` (FR-011).
func formatInterval(iv *graphv1.Interval) string {
	if iv == nil {
		return unknownCell
	}
	start := formatTime(iv.GetStart())
	if iv.GetStartUnknown() {
		start += "?"
	}
	end := "∞"
	if iv.GetEnd() != nil {
		end = formatTime(iv.GetEnd())
	}
	if iv.GetEndUnknown() {
		end += "?"
	}
	return fmt.Sprintf("[%s, %s)", start, end)
}
