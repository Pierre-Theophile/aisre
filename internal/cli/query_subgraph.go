// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// `query subgraph` (contracts/cli.md §Queries, FR-026, FR-035).
//
// This is the command an on-call engineer runs first: "show me what was around checkout at
// 14:32". The table is laid out for that moment — the focus on its own line so there is no
// hunting, then the nodes, then the edges with their traffic weights, then, if anything was
// cut, a line saying so. A truncation that is easy to miss is worse than no answer, so it is
// printed last and in full: which cap, and which nodes it cut at.
//
// `--output json` prints the SubgraphResponse in the canonical serialization goldens are
// written in, so `aisre query subgraph … --output json` and
// `fixtures/<id>/golden/subgraph.<name>.json` are comparable byte for byte.

type querySubgraphOptions struct {
	asOf       string
	observedAt string
	hops       int
	direction  string
	edgeTypes  []string
	minWeight  int
	perHopCap  int
	totalCap   int
}

func newQuerySubgraphCommand(global *globalOptions) *cobra.Command {
	opts := &querySubgraphOptions{}

	cmd := &cobra.Command{
		Use:   "subgraph <namespace>=<value>",
		Short: "Show the neighbourhood of a node as it was at an instant",
		Long: "subgraph returns the N-hop neighbourhood of a focus node as of a valid-time instant, as\n" +
			"known at an observed-time instant (default: as known now).\n\n" +
			"Direction is named from the focus: `up` follows edges INTO it (its callers and\n" +
			"dependents — who is hurt), `down` follows edges OUT of it (its callees and\n" +
			"dependencies — what could be hurting it), and `both`, the default, follows either.\n\n" +
			"Expansion is capped at 50 nodes per hop and 500 in total unless told otherwise; when a\n" +
			"cap bites, the response says which cap and at which nodes.\n\n" +
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
			resp, err := clients.query.Subgraph(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("query subgraph", err)
			}
			return renderSubgraph(cmd, global, req, resp.Msg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.asOf, "as-of", "", "valid-time instant to answer as of (required)")
	flags.StringVar(&opts.observedAt, "observed-at", "",
		"observed-time instant to answer as known at (default: as known now)")
	flags.IntVar(&opts.hops, "hops", query.DefaultHops, "neighbourhood radius in hops")
	flags.StringVar(&opts.direction, "direction", "both", "edge direction to follow: up, down or both")
	flags.StringSliceVar(&opts.edgeTypes, "edge-types", nil,
		"follow only these edge types (calls, depends-on, runs-on, deployed-by, owned-by, exposed-via, changed-by)")
	flags.IntVar(&opts.minWeight, "min-weight", 0,
		"drop edges below this traffic weight class (0-5); unweighted edges count as 0")
	flags.IntVar(&opts.perHopCap, "per-hop-cap", query.DefaultPerHopCap, "maximum fan-out expanded per node")
	flags.IntVar(&opts.totalCap, "total-cap", query.DefaultTotalCap, "maximum nodes in the response")

	return cmd
}

// request turns the flags into the published message, applying the CLI's own validation so a
// typo costs a round trip to nobody.
func (o *querySubgraphOptions) request(cmd *cobra.Command, focus string) (*graphv1.SubgraphRequest, error) {
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
	direction, err := query.ParseDirection(o.direction)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	edgeTypes, err := query.ParseEdgeTypes(o.edgeTypes)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	if o.hops < 0 {
		return nil, exitErrorf(ExitUsage, "--hops %d: want a positive number of hops", o.hops)
	}
	if o.minWeight < 0 || o.minWeight > 5 {
		return nil, exitErrorf(ExitUsage, "--min-weight %d: weight classes run from 0 to 5 (research §8)", o.minWeight)
	}

	req := &graphv1.SubgraphRequest{
		Focus:     ref.Proto(),
		AsOf:      &graphv1.AsOf{ValidAt: timestamppb.New(validAt)},
		Hops:      uint32(o.hops),
		Direction: direction,
		EdgeTypes: edgeTypes,
	}
	if !observedAt.IsZero() {
		req.AsOf.ObservedAt = timestamppb.New(observedAt)
	}
	if cmd.Flags().Changed("min-weight") {
		class := uint32(o.minWeight)
		req.MinWeightClass = &class
	}
	if o.perHopCap > 0 {
		cap := uint32(o.perHopCap)
		req.PerHopCap = &cap
	}
	if o.totalCap > 0 {
		cap := uint32(o.totalCap)
		req.TotalCap = &cap
	}
	return req, nil
}

func renderSubgraph(cmd *cobra.Command, global *globalOptions, req *graphv1.SubgraphRequest, resp *graphv1.SubgraphResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	if err := p.writeLine("focus     %s", focusLine(resp.GetFocus(), req.GetFocus())); err != nil {
		return err
	}
	if err := p.writeLine("as of     valid %s, observed %s",
		formatTime(req.GetAsOf().GetValidAt()), observedLabel(req.GetAsOf())); err != nil {
		return err
	}
	if err := p.writeLine(""); err != nil {
		return err
	}

	names := map[string]string{}
	nodeRows := make([][]string, 0, len(resp.GetNodes()))
	for _, node := range resp.GetNodes() {
		names[node.GetEntityId()] = nodeLabel(node)
		nodeRows = append(nodeRows, []string{
			node.GetEntityId(),
			nodeTypeCell(node),
			nodeLabel(node),
			formatInterval(node.GetValid()),
		})
	}
	if err := p.writeTable([]string{"ID", "TYPE", "NAME", "VALID"}, nodeRows); err != nil {
		return err
	}
	if err := p.writeLine(""); err != nil {
		return err
	}

	edgeRows := make([][]string, 0, len(resp.GetEdges()))
	for _, edge := range resp.GetEdges() {
		edgeRows = append(edgeRows, []string{
			fmt.Sprintf("%s -> %s", label(names, edge.GetSrcId()), label(names, edge.GetDstId())),
			graph.EdgeTypeFromProto(edge.GetType()).String(),
			weightCell(edge),
			formatInterval(edge.GetValid()),
		})
	}
	if err := p.writeTable([]string{"EDGE", "TYPE", "WEIGHT", "VALID"}, edgeRows); err != nil {
		return err
	}

	if err := p.writeLine("\n%d node(s), %d edge(s)", len(resp.GetNodes()), len(resp.GetEdges())); err != nil {
		return err
	}
	return p.writeLine("%s", truncationLine(names, resp.GetTruncation()))
}

// focusLine describes the node the query started from, falling back to the reference the caller
// typed when the focus has no version valid at the instant.
func focusLine(focus *graphv1.NodeVersion, ref *graphv1.Ref) string {
	if focus == nil {
		return fmt.Sprintf("%s=%s (nothing valid at this instant)", ref.GetNamespace(), ref.GetValue())
	}
	return fmt.Sprintf("%s (%s)  %s  valid %s",
		nodeLabel(focus), nodeTypeCell(focus), focus.GetEntityId(), formatInterval(focus.GetValid()))
}

func observedLabel(asOf *graphv1.AsOf) string {
	if asOf.GetObservedAt() == nil {
		return "now"
	}
	return formatTime(asOf.GetObservedAt())
}

// nodeTypeCell renders the resolved type, with the other asserted types in brackets when a
// merge gave the entity more than one (research §10). "service[workload]" is a Kubernetes
// workload and the OpenTelemetry service running on it, resolved to one entity.
func nodeTypeCell(node *graphv1.NodeVersion) string {
	resolved := graph.NodeTypeFromProto(node.GetType()).String()
	if resolved == "" {
		resolved = unknownCell
	}
	var others []string
	for _, facet := range node.GetFacets() {
		if facet != node.GetType() {
			others = append(others, graph.NodeTypeFromProto(facet).String())
		}
	}
	if len(others) == 0 {
		return resolved
	}
	return resolved + "[" + strings.Join(others, ",") + "]"
}

// nodeLabel is the name a person recognizes: the display name, or the first alias, or the id.
// A placeholder — an endpoint nothing has described yet — says so, because an unnamed row in a
// table otherwise looks like a bug rather than a fact about the graph.
func nodeLabel(node *graphv1.NodeVersion) string {
	if name := node.GetDisplayName(); name != "" {
		return name
	}
	if aliases := node.GetAliases(); len(aliases) > 0 {
		return aliases[0].GetValue() + " (undescribed)"
	}
	return node.GetEntityId() + " (undescribed)"
}

func label(names map[string]string, id string) string {
	if name, ok := names[id]; ok {
		return name
	}
	return id
}

func weightCell(edge *graphv1.EdgeVersion) string {
	if edge.WeightClass == nil {
		return unknownCell
	}
	return strconv.FormatUint(uint64(edge.GetWeightClass()), 10)
}

// truncationLine says what was left out. It is the last thing printed because it is the thing
// an operator must not miss (FR-026, edge case "hub explosion").
func truncationLine(names map[string]string, t *graphv1.Truncation) string {
	if t == nil {
		return "complete: nothing was truncated"
	}
	if t.GetReason() == query.ReasonBeforeHistory {
		return "before recorded history: the graph had observed nothing at this instant"
	}
	if !t.GetTruncated() {
		return fmt.Sprintf("complete: nothing was truncated (caps %d per hop, %d total)",
			t.GetPerHopCap(), t.GetTotalCap())
	}
	cut := make([]string, 0, len(t.GetTruncatedAtEntityIds()))
	for _, id := range t.GetTruncatedAtEntityIds() {
		cut = append(cut, label(names, id))
	}
	return fmt.Sprintf("TRUNCATED (%s; caps %d per hop, %d total) at: %s",
		t.GetReason(), t.GetPerHopCap(), t.GetTotalCap(), strings.Join(cut, ", "))
}
