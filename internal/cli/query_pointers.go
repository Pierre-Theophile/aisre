// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// `query pointers` (contracts/cli.md §Queries, FR-030, FR-035).
//
// "I have a suspect node; open the right tabs." The table is grouped by kind because that is
// how the tabs are grouped — metrics here, logs there, traces over there — and each row says
// which backend the selector is for and which vocabulary it is written in, so a selector is
// never copied into the wrong query bar.
//
// The instant matters and the header says so. Pointers are versioned like any other property
// (FR-008), so asking about 13:00 gives the selectors that were true at 13:00: if the service
// was renamed at 14:00, the old name is what comes back, which is the point (US5 scenario 2).
//
// What this command never does is fetch anything the pointers name. The graph says where to
// look; looking is somebody else's job, and doing it here would put telemetry on the read path
// (constitution IV).

type queryPointersOptions struct {
	asOf       string
	observedAt string
}

func newQueryPointersCommand(global *globalOptions) *cobra.Command {
	opts := &queryPointersOptions{}

	cmd := &cobra.Command{
		Use:   "pointers <namespace>=<value>",
		Short: "Show where to look for a node's telemetry, as of an instant",
		Long: "pointers returns the telemetry selectors attached to a node as of a valid-time instant,\n" +
			"as known at an observed-time instant (default: as known now), grouped by kind: METRIC,\n" +
			"LOG, TRACE, DASHBOARD, SOURCE_LINK.\n\n" +
			"A pointer is never telemetry: it is the backend, the vocabulary and the selector that\n" +
			"find it. Pointers are versioned in time like any other property, so the instant decides\n" +
			"which selectors come back — ask before a rename and you get the old name.\n\n" +
			"Instants may be written as RFC 3339 (2026-09-01T14:32:00Z), relative to now (-30m) or\n" +
			"as `now`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := opts.request(args[0])
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.query.Pointers(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("query pointers", err)
			}
			return renderPointers(cmd, global, req, resp.Msg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.asOf, "as-of", "", "valid-time instant to answer as of (required)")
	flags.StringVar(&opts.observedAt, "observed-at", "",
		"observed-time instant to answer as known at (default: as known now)")

	return cmd
}

func (o *queryPointersOptions) request(focus string) (*graphv1.PointersRequest, error) {
	ref, err := parseFocusRef(focus)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.asOf) == "" {
		return nil, exitErrorf(ExitUsage, "--as-of is required: pointers are versioned in time")
	}
	validAt, err := parseInstant("--as-of", o.asOf)
	if err != nil {
		return nil, err
	}
	observedAt, err := parseInstant("--observed-at", o.observedAt)
	if err != nil {
		return nil, err
	}
	req := &graphv1.PointersRequest{
		Focus: ref.Proto(),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(validAt)},
	}
	if !observedAt.IsZero() {
		req.AsOf.ObservedAt = timestamppb.New(observedAt)
	}
	return req, nil
}

// pointerKindOrder is the order the groups are printed in: the three telemetry kinds first, in
// the order an investigation uses them, then the links out to other systems. Any kind not named
// here — a future one, or the unspecified one — follows in alphabetical order, so nothing is
// ever silently dropped from the output.
var pointerKindOrder = []string{"METRIC", "LOG", "TRACE", "DASHBOARD", "SOURCE_LINK"}

func renderPointers(cmd *cobra.Command, global *globalOptions, req *graphv1.PointersRequest, resp *graphv1.PointersResponse) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(resp)
	}

	if err := p.writeLine("focus     %s", focusLine(resp.GetNode(), req.GetFocus())); err != nil {
		return err
	}
	if err := p.writeLine("as of     valid %s, observed %s",
		formatTime(req.GetAsOf().GetValidAt()), observedLabel(req.GetAsOf())); err != nil {
		return err
	}
	if err := p.writeLine(""); err != nil {
		return err
	}

	kinds := pointerKinds(resp.GetByKind())
	if len(kinds) == 0 {
		return p.writeLine("no pointers on this node at this instant")
	}
	for i, kind := range kinds {
		if i > 0 {
			if err := p.writeLine(""); err != nil {
				return err
			}
		}
		if err := p.writeLine("%s", kind); err != nil {
			return err
		}
		list := resp.GetByKind()[kind]
		rows := make([][]string, 0, len(list.GetPointers()))
		for _, pointer := range list.GetPointers() {
			rows = append(rows, []string{
				pointer.GetBackendKind(),
				pointer.GetVocabulary(),
				pointer.GetSelector(),
			})
		}
		if err := p.writeTable([]string{"BACKEND", "VOCABULARY", "SELECTOR"}, rows); err != nil {
			return err
		}
	}
	return nil
}

// pointerKinds returns the kinds present, in the published reading order followed by anything
// else alphabetically.
func pointerKinds(byKind map[string]*graphv1.PointerList) []string {
	rest := make([]string, 0, len(byKind))
	for kind := range byKind {
		if !slices.Contains(pointerKindOrder, kind) {
			rest = append(rest, kind)
		}
	}
	slices.Sort(rest)

	out := make([]string, 0, len(byKind))
	for _, kind := range pointerKindOrder {
		if _, ok := byKind[kind]; ok {
			out = append(out, kind)
		}
	}
	return append(out, rest...)
}
