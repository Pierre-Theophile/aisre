// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// `query impact` (contracts/cli.md §Queries, FR-029, FR-035).
//
// The command that answers "who do I tell?". It prints two tables and the order is the order
// the question is asked in: **downstream first** — the things that depend on the focus, the
// ones that break if it breaks — and then upstream, the things it depends on, which is where
// the cause might be. Each row carries the hop distance, the traffic weight of the heaviest
// path, how many other routes there are, and the route itself in display names, because an
// operator who is about to page somebody wants to see *why* they are on the list.
//
// The two words are the opposite way round from `--direction` on `query subgraph`, and that is
// deliberate: `direction` names the arrows a walk follows, `downstream`/`upstream` name the way
// consequence flows along them. docs/schema/queries.md states the mapping; the table headers
// repeat it so nobody has to remember.
//
// `--output json` prints the ImpactResponse in the canonical serialization goldens are written
// in, so an invocation and `fixtures/<id>/golden/impact.<name>.json` are comparable byte for
// byte.

type queryImpactOptions struct {
	asOf       string
	observedAt string
	maxHops    int
	totalCap   int
}

func newQueryImpactCommand(global *globalOptions) *cobra.Command {
	opts := &queryImpactOptions{}

	cmd := &cobra.Command{
		Use:   "impact <namespace>=<value>",
		Short: "Show the blast radius of a node as it was at an instant",
		Long: "impact returns the weighted blast radius of a focus node as of a valid-time instant, as\n" +
			"known at an observed-time instant (default: as known now), in two lists.\n\n" +
			"`downstream` holds the entities that DEPEND ON the focus — its callers and dependents,\n" +
			"who breaks if it breaks. `upstream` holds the entities the focus DEPENDS ON — its\n" +
			"callees and dependencies, what could be breaking it. (Note that this is the opposite\n" +
			"sense from `query subgraph --direction`, which names arrows rather than consequence.)\n\n" +
			"Each item is listed once, at its shortest hop distance, with the heaviest path shown:\n" +
			"the route whose weakest link carries the most traffic. Edges with no traffic weight\n" +
			"count as class 0, so an ownership or config route ranks on hop distance alone.\n\n" +
			"Instants may be written as RFC 3339 (2026-09-01T14:32:00Z), relative to now (-30m) or\n" +
			"as `now`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := opts.request(cmd, args[0])
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.query.Impact(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("query impact", err)
			}
			return renderImpact(cmd, global, req, resp.Msg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.asOf, "as-of", "", "valid-time instant to answer as of (required)")
	flags.StringVar(&opts.observedAt, "observed-at", "",
		"observed-time instant to answer as known at (default: as known now)")
	flags.IntVar(&opts.maxHops, "max-hops", query.DefaultImpactHops, "how far the blast radius reaches")
	flags.IntVar(&opts.totalCap, "total-cap", query.DefaultTotalCap, "maximum nodes expanded per direction")

	return cmd
}

func (o *queryImpactOptions) request(cmd *cobra.Command, focus string) (*graphv1.ImpactRequest, error) {
	ref, err := parseFocusRef(focus)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.asOf) == "" {
		return nil, exitErrorf(ExitUsage, "--as-of is required: the graph answers as of an instant")
	}
	validAt, err := parseInstant("--as-of", o.asOf)
	if err != nil {
		return nil, err
	}
	observedAt, err := parseInstant("--observed-at", o.observedAt)
	if err != nil {
		return nil, err
	}
	if o.maxHops < 1 {
		return nil, exitErrorf(ExitUsage, "--max-hops %d: a blast radius is at least one hop", o.maxHops)
	}
	if o.maxHops > query.MaxHops {
		return nil, exitErrorf(ExitUsage, "--max-hops %d: the maximum is %d", o.maxHops, query.MaxHops)
	}

	req := &graphv1.ImpactRequest{
		Focus: ref.Proto(),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(validAt)},
	}
	if !observedAt.IsZero() {
		req.AsOf.ObservedAt = timestamppb.New(observedAt)
	}
	if cmd.Flags().Changed("max-hops") {
		hops := uint32(o.maxHops)
		req.MaxHops = &hops
	}
	if o.totalCap > 0 {
		cap := uint32(o.totalCap)
		req.TotalCap = &cap
	}
	return req, nil
}

func renderImpact(cmd *cobra.Command, global *globalOptions, req *graphv1.ImpactRequest, resp *graphv1.ImpactResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	names := impactNames(req, resp)
	if err := p.writeLine("focus     %s=%s", req.GetFocus().GetNamespace(), req.GetFocus().GetValue()); err != nil {
		return err
	}
	if err := p.writeLine("as of     valid %s, observed %s",
		formatTime(req.GetAsOf().GetValidAt()), observedLabel(req.GetAsOf())); err != nil {
		return err
	}
	if err := p.writeLine("summary   %d downstream dependent(s), %d upstream dependency(ies)\n",
		len(resp.GetDownstream()), len(resp.GetUpstream())); err != nil {
		return err
	}

	if err := renderImpactSide(p, names,
		"downstream — depends on the focus; breaks if it breaks", resp.GetDownstream()); err != nil {
		return err
	}
	if err := p.writeLine(""); err != nil {
		return err
	}
	if err := renderImpactSide(p, names,
		"upstream — the focus depends on these; could be breaking it", resp.GetUpstream()); err != nil {
		return err
	}
	if err := p.writeLine(""); err != nil {
		return err
	}
	return p.writeLine("%s", truncationLine(names, resp.GetTruncation()))
}

// renderImpactSide prints one of the two lists, heading included, so the two tables can never
// be read the wrong way round.
func renderImpactSide(p *printer, names map[string]string, heading string, items []*graphv1.ImpactItem) error {
	if err := p.writeLine("%s", heading); err != nil {
		return err
	}
	if len(items) == 0 {
		return p.writeLine("  (none within the radius)")
	}
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{
			nodeLabel(item.GetNode()),
			nodeTypeCell(item.GetNode()),
			strconv.FormatUint(uint64(item.GetHopDistance()), 10),
			strconv.FormatUint(uint64(item.GetWeightClass()), 10),
			strconv.FormatUint(uint64(item.GetAlternativePaths()), 10),
			pathCell(names, item.GetHeaviestPath()),
		})
	}
	return p.writeTable([]string{"NODE", "TYPE", "HOP", "WEIGHT", "ALT PATHS", "HEAVIEST PATH"}, rows)
}

// impactNames maps every entity id in the answer to the name a person recognizes, so a path of
// base32 ids can be printed as a route through the system.
//
// The focus is not one of the items — it is not in its own blast radius — but it is the first
// element of every path, so it is named from the reference the caller typed.
func impactNames(req *graphv1.ImpactRequest, resp *graphv1.ImpactResponse) map[string]string {
	names := map[string]string{}
	for _, items := range [][]*graphv1.ImpactItem{resp.GetDownstream(), resp.GetUpstream()} {
		for _, item := range items {
			names[item.GetNode().GetEntityId()] = nodeLabel(item.GetNode())
			if path := item.GetHeaviestPath(); len(path) > 0 {
				if _, named := names[path[0]]; !named {
					names[path[0]] = req.GetFocus().GetValue()
				}
			}
		}
	}
	return names
}

// pathCell renders the heaviest path as the route it is. The focus is the first element and is
// kept: a path that did not say where it started would be unreadable once the list is sorted.
func pathCell(names map[string]string, path []string) string {
	if len(path) == 0 {
		return unknownCell
	}
	hops := make([]string, 0, len(path))
	for _, id := range path {
		hops = append(hops, label(names, id))
	}
	return strings.Join(hops, " -> ")
}
