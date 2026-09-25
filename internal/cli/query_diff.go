// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// `query diff` (contracts/cli.md §Queries, FR-027, FR-028, FR-035).
//
// This is the command the incident is actually run from: "checkout was fine at 13:00 and is
// failing now — what changed?". The table is ordered the way that question is answered, and
// deliberately not the way the response message is laid out:
//
//  1. the ranked changes first, because that is the answer;
//  2. then the deltas — what the graph itself looks like differently — because that is the
//     evidence for it;
//  3. then the ranking formula and any truncation, because an operator who disagrees with the
//     order must be able to see what produced it without leaving the terminal (constitution V).
//
// `--output json` prints the DiffResponse in the canonical serialization goldens are written
// in, so a command run during an incident and the fixture that reproduces it are comparable
// byte for byte.

type queryDiffOptions struct {
	at         string
	window     time.Duration
	from       string
	to         string
	observedAt string
	reference  string
	tau        time.Duration
	changeHops int
	hops       int
	direction  string
	edgeTypes  []string
	minWeight  int
	perHopCap  int
	totalCap   int
}

func newQueryDiffCommand(global *globalOptions) *cobra.Command {
	opts := &queryDiffOptions{}

	cmd := &cobra.Command{
		Use:   "diff <namespace>=<value>",
		Short: "Show what changed around a node between two instants, ranked",
		Long: "diff compares the neighbourhood of a focus node at two valid-time instants, as known at\n" +
			"one observed-time instant, and ranks the change events of the window by how likely they\n" +
			"are to matter.\n\n" +
			"The ranking is published (docs/schema/ranking.md): recency against the reference instant,\n" +
			"hop distance from the focus, and the traffic on the path between them. Every item carries\n" +
			"its own components, and the formula used is printed with the answer.\n\n" +
			"Changes whose target the graph could not resolve are listed too, at reduced rank and\n" +
			"flagged `unattached`; they are never dropped.\n\n" +
			"From an alert instant alone: `--at <instant>` asks for the changes of the --window\n" +
			"(default 2h) before it, ranked against it, and prints for each one the actor, the actor\n" +
			"kind, the commit it shipped, whether it was a rollback and a link that opens the run\n" +
			"(004 SC-016).\n\n" +
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
			resp, err := clients.query.Diff(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("query diff", err)
			}
			return renderDiff(cmd, global, req, resp.Msg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.at, "at", "",
		"the alert instant: the window is the --window before it and the ranking is measured to it "+
			"(instead of --from/--to)")
	flags.DurationVar(&opts.window, "window", DefaultAlertWindow,
		"how far before --at the window reaches")
	flags.StringVar(&opts.from, "from", "", "valid-time instant the window starts at (required unless --at)")
	flags.StringVar(&opts.to, "to", "", "valid-time instant the window ends at (required unless --at)")
	flags.StringVar(&opts.observedAt, "observed-at", "",
		"observed-time instant to answer as known at (default: as known now)")
	flags.StringVar(&opts.reference, "reference", "",
		"instant temporal proximity is measured to (default: --to)")
	flags.DurationVar(&opts.tau, "tau", query.DefaultTau,
		"temporal decay constant of the ranking score")
	flags.IntVar(&opts.changeHops, "change-margin", query.DefaultChangeHopMargin,
		"how many hops past the neighbourhood a change's target may lie and still be considered")
	flags.IntVar(&opts.hops, "hops", query.DefaultHops, "neighbourhood radius in hops")
	flags.StringVar(&opts.direction, "direction", "both", "edge direction to follow: up, down or both")
	flags.StringSliceVar(&opts.edgeTypes, "edge-types", nil,
		"follow only these edge types (calls, depends-on, runs-on, deployed-by, owned-by, exposed-via, changed-by)")
	flags.IntVar(&opts.minWeight, "min-weight", 0,
		"drop edges below this traffic weight class (0-5); unweighted edges count as 0")
	flags.IntVar(&opts.perHopCap, "per-hop-cap", query.DefaultPerHopCap, "maximum fan-out expanded per node")
	flags.IntVar(&opts.totalCap, "total-cap", query.DefaultTotalCap, "maximum nodes per side of the diff")

	return cmd
}

// DefaultAlertWindow is how far before an alert instant `--at` looks: long enough to hold the deploy an
// incident usually follows, short enough that the ranking's recency term still separates candidates.
const DefaultAlertWindow = 2 * time.Hour

// resolveWindow resolves the window from either --at/--window or --from/--to, and returns the alert instant
// when --at was used (zero otherwise).
func (o *queryDiffOptions) resolveWindow() (from, to, alertAt time.Time, err error) {
	at := strings.TrimSpace(o.at)
	explicit := strings.TrimSpace(o.from) != "" || strings.TrimSpace(o.to) != ""
	switch {
	case at != "" && explicit:
		return from, to, alertAt, exitErrorf(ExitUsage,
			"--at and --from/--to both given: --at is the alert instant and derives the window itself")
	case at != "":
		if o.window <= 0 {
			return from, to, alertAt, exitErrorf(ExitUsage, "--window %s: want a positive duration", o.window)
		}
		if alertAt, err = parseInstant("--at", at); err != nil {
			return from, to, alertAt, err
		}
		return alertAt.Add(-o.window), alertAt, alertAt, nil
	case strings.TrimSpace(o.from) == "" || strings.TrimSpace(o.to) == "":
		return from, to, alertAt, exitErrorf(ExitUsage,
			"--from and --to are both required, or --at: a diff is a statement about a window")
	}
	if from, err = parseInstant("--from", o.from); err != nil {
		return from, to, alertAt, err
	}
	if to, err = parseInstant("--to", o.to); err != nil {
		return from, to, alertAt, err
	}
	return from, to, alertAt, nil
}

// request turns the flags into the published message.
func (o *queryDiffOptions) request(cmd *cobra.Command, focus string) (*graphv1.DiffRequest, error) {
	ref, err := parseFocusRef(focus)
	if err != nil {
		return nil, err
	}
	t1, t2, alertAt, err := o.resolveWindow()
	if err != nil {
		return nil, err
	}
	if !t1.Before(t2) {
		return nil, exitErrorf(ExitUsage, "--from %s is not before --to %s",
			t1.Format(time.RFC3339), t2.Format(time.RFC3339))
	}
	observedAt, err := parseInstant("--observed-at", o.observedAt)
	if err != nil {
		return nil, err
	}
	reference, err := parseInstant("--reference", o.reference)
	if err != nil {
		return nil, err
	}
	if reference.IsZero() && !alertAt.IsZero() {
		// The alert instant is what a cause has to precede, so the ranking is measured to it.
		reference = alertAt
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
	if o.changeHops < 0 {
		return nil, exitErrorf(ExitUsage, "--change-margin %d: want a positive number of hops", o.changeHops)
	}
	if o.tau <= 0 {
		return nil, exitErrorf(ExitUsage, "--tau %s: the decay constant must be positive", o.tau)
	}
	if o.minWeight < 0 || o.minWeight > 5 {
		return nil, exitErrorf(ExitUsage, "--min-weight %d: weight classes run from 0 to 5 (research §8)", o.minWeight)
	}

	sub := &graphv1.SubgraphRequest{
		Focus:     ref.Proto(),
		Hops:      uint32(o.hops),
		Direction: direction,
		EdgeTypes: edgeTypes,
	}
	if cmd.Flags().Changed("min-weight") {
		class := uint32(o.minWeight)
		sub.MinWeightClass = &class
	}
	if o.perHopCap > 0 {
		cap := uint32(o.perHopCap)
		sub.PerHopCap = &cap
	}
	if o.totalCap > 0 {
		cap := uint32(o.totalCap)
		sub.TotalCap = &cap
	}

	req := &graphv1.DiffRequest{
		Subgraph: sub,
		T1:       timestamppb.New(t1),
		T2:       timestamppb.New(t2),
	}
	if !observedAt.IsZero() {
		req.ObservedAt = timestamppb.New(observedAt)
	}
	if !reference.IsZero() {
		req.ReferenceAt = timestamppb.New(reference)
	}
	if cmd.Flags().Changed("tau") {
		seconds := o.tau.Seconds()
		req.TauSeconds = &seconds
	}
	if cmd.Flags().Changed("change-margin") {
		margin := uint32(o.changeHops)
		req.ChangeHopMargin = &margin
	}
	return req, nil
}

func renderDiff(cmd *cobra.Command, global *globalOptions, req *graphv1.DiffRequest, resp *graphv1.DiffResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	names := diffNames(resp)
	if err := p.writeLine("focus     %s=%s", req.GetSubgraph().GetFocus().GetNamespace(),
		req.GetSubgraph().GetFocus().GetValue()); err != nil {
		return err
	}
	if err := p.writeLine("window    valid (%s, %s], observed %s",
		formatTime(req.GetT1()), formatTime(req.GetT2()), diffObservedLabel(req)); err != nil {
		return err
	}
	if err := p.writeLine("summary   %d ranked change(s); nodes +%d/-%d/~%d; edges +%d/-%d/~%d\n",
		len(resp.GetChanges()), len(resp.GetNodesAdded()), len(resp.GetNodesRemoved()),
		len(resp.GetNodesChanged()), len(resp.GetEdgesAdded()), len(resp.GetEdgesRemoved()),
		len(resp.GetEdgesChanged())); err != nil {
		return err
	}

	if err := renderRankedChanges(p, names, resp.GetChanges()); err != nil {
		return err
	}
	if err := renderProvenance(p, resp.GetChanges()); err != nil {
		return err
	}
	if err := renderNodeDeltas(p, resp); err != nil {
		return err
	}
	if err := renderEdgeDeltas(p, names, resp); err != nil {
		return err
	}

	if formula := resp.GetRankingFormula(); formula != "" {
		if err := p.writeLine("\nranking   %s", formula); err != nil {
			return err
		}
	}
	return p.writeLine("%s", truncationLine(names, resp.GetTruncation()))
}

// renderRankedChanges prints the answer: the changes of the window, in rank order, each with
// the components that put it there.
func renderRankedChanges(p *printer, names map[string]string, changes []*graphv1.RankedChange) error {
	if len(changes) == 0 {
		return p.writeLine("no change events in this window touched the neighbourhood")
	}
	rows := make([][]string, 0, len(changes))
	for i, item := range changes {
		unattached := "-"
		if item.GetUnattached() {
			unattached = "unattached"
		}
		rows = append(rows, []string{
			strconv.Itoa(i + 1),
			strconv.FormatFloat(item.GetScore(), 'f', 6, 64),
			changeKindCell(item.GetChange()),
			changeSummary(item.GetChange()),
			strings.Join(targetLabels(names, item.GetTargetEntityIds()), ", "),
			strconv.FormatUint(uint64(item.GetHopDistance()), 10),
			(time.Duration(item.GetTimeDistanceSeconds()) * time.Second).String(),
			unattached,
		})
	}
	if err := p.writeTable([]string{"RANK", "SCORE", "KIND", "SUMMARY", "TARGET", "HOP", "AGE", "FLAG"}, rows); err != nil {
		return err
	}

	components := make([][]string, 0, len(changes))
	for i, item := range changes {
		components = append(components, []string{
			strconv.Itoa(i + 1),
			strconv.FormatFloat(item.GetTemporal(), 'f', 6, 64),
			strconv.FormatFloat(item.GetTopological(), 'f', 6, 64),
			strconv.FormatFloat(item.GetTraffic(), 'f', 6, 64),
			item.GetTieBreak(),
		})
	}
	if err := p.writeLine(""); err != nil {
		return err
	}
	return p.writeTable([]string{"RANK", "TEMPORAL", "TOPOLOGICAL", "TRAFFIC", "TIE-BREAK"}, components)
}

// renderNodeDeltas prints the nodes that appeared, vanished or moved.
func renderNodeDeltas(p *printer, resp *graphv1.DiffResponse) error {
	if len(resp.GetNodesAdded()) > 0 {
		if err := p.writeLine("\nnodes added"); err != nil {
			return err
		}
		if err := p.writeTable([]string{"ID", "TYPE", "NAME", "VALID"}, nodeRows(resp.GetNodesAdded())); err != nil {
			return err
		}
	}
	if len(resp.GetNodesRemoved()) > 0 {
		if err := p.writeLine("\nnodes removed"); err != nil {
			return err
		}
		if err := p.writeTable([]string{"ID", "TYPE", "NAME", "VALID"}, nodeRows(resp.GetNodesRemoved())); err != nil {
			return err
		}
	}
	if len(resp.GetNodesChanged()) == 0 {
		return nil
	}
	if err := p.writeLine("\nnodes changed"); err != nil {
		return err
	}
	var rows [][]string
	for _, delta := range resp.GetNodesChanged() {
		name := nodeLabel(delta.GetAfter())
		for _, prop := range delta.GetDeltas() {
			rows = append(rows, []string{
				name, prop.GetKey(),
				formatPropValue(prop.GetOld()), formatPropValue(prop.GetNew()),
			})
			name = ""
		}
	}
	return p.writeTable([]string{"NODE", "KEY", "OLD", "NEW"}, rows)
}

// renderEdgeDeltas prints the relationships that appeared, vanished or moved.
func renderEdgeDeltas(p *printer, names map[string]string, resp *graphv1.DiffResponse) error {
	for _, section := range []struct {
		title string
		edges []*graphv1.EdgeVersion
	}{
		{"edges added", resp.GetEdgesAdded()},
		{"edges removed", resp.GetEdgesRemoved()},
	} {
		if len(section.edges) == 0 {
			continue
		}
		if err := p.writeLine("\n%s", section.title); err != nil {
			return err
		}
		rows := make([][]string, 0, len(section.edges))
		for _, edge := range section.edges {
			rows = append(rows, []string{
				fmt.Sprintf("%s -> %s", label(names, edge.GetSrcId()), label(names, edge.GetDstId())),
				graph.EdgeTypeFromProto(edge.GetType()).String(),
				weightCell(edge),
				formatInterval(edge.GetValid()),
			})
		}
		if err := p.writeTable([]string{"EDGE", "TYPE", "WEIGHT", "VALID"}, rows); err != nil {
			return err
		}
	}

	if len(resp.GetEdgesChanged()) == 0 {
		return nil
	}
	if err := p.writeLine("\nedges changed"); err != nil {
		return err
	}
	var rows [][]string
	for _, delta := range resp.GetEdgesChanged() {
		edge := delta.GetAfter()
		name := fmt.Sprintf("%s -> %s (%s)", label(names, edge.GetSrcId()), label(names, edge.GetDstId()),
			graph.EdgeTypeFromProto(edge.GetType()).String())
		for _, prop := range delta.GetDeltas() {
			rows = append(rows, []string{
				name, prop.GetKey(),
				formatPropValue(prop.GetOld()), formatPropValue(prop.GetNew()),
			})
			name = ""
		}
	}
	return p.writeTable([]string{"EDGE", "KEY", "OLD", "NEW"}, rows)
}

func nodeRows(nodes []*graphv1.NodeVersion) [][]string {
	rows := make([][]string, 0, len(nodes))
	for _, node := range nodes {
		rows = append(rows, []string{
			node.GetEntityId(), nodeTypeCell(node), nodeLabel(node), formatInterval(node.GetValid()),
		})
	}
	return rows
}

// diffNames maps the entity ids the response mentions to the names a person recognizes. A diff
// carries no full node set, so an id that appears only as a change target stays an id.
func diffNames(resp *graphv1.DiffResponse) map[string]string {
	names := map[string]string{}
	add := func(nodes ...*graphv1.NodeVersion) {
		for _, node := range nodes {
			if node.GetEntityId() != "" {
				names[node.GetEntityId()] = nodeLabel(node)
			}
		}
	}
	add(resp.GetNodesAdded()...)
	add(resp.GetNodesRemoved()...)
	for _, delta := range resp.GetNodesChanged() {
		add(delta.GetBefore(), delta.GetAfter())
	}
	for _, item := range resp.GetChanges() {
		add(item.GetChange())
	}
	add(resp.GetChangeTargets()...)
	return names
}

func targetLabels(names map[string]string, ids []string) []string {
	if len(ids) == 0 {
		return []string{unknownCell}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, label(names, id))
	}
	return out
}

// changeKindCell renders the change's kind, falling back to the extensible `kind_other` a
// source may use for a kind the taxonomy does not name yet (FR-003).
func changeKindCell(node *graphv1.NodeVersion) string {
	change := node.GetChange()
	if change == nil {
		return unknownCell
	}
	if other := change.GetKindOther(); other != "" {
		return other
	}
	return change.GetKind().String()
}

func changeSummary(node *graphv1.NodeVersion) string {
	if summary := node.GetChange().GetSummary(); summary != "" {
		return summary
	}
	return nodeLabel(node)
}

// formatPropValue renders one side of a property delta. A key that is absent on one side prints
// as "-", which is how an added or dropped property is told from a changed one.
func formatPropValue(value *structpb.Value) string {
	if value == nil {
		return unknownCell
	}
	if s, ok := value.GetKind().(*structpb.Value_StringValue); ok {
		return s.StringValue
	}
	encoded, err := value.MarshalJSON()
	if err != nil {
		return unknownCell
	}
	return string(encoded)
}

func diffObservedLabel(req *graphv1.DiffRequest) string {
	if req.GetObservedAt() == nil {
		return "now"
	}
	return formatTime(req.GetObservedAt())
}
