// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// renderCatalogNames prints, for `feed notion --check` and `feed slack --check`, each name the
// connector observed with the catalog entry it maps to, or that it is unmapped or hidden (009
// FR-060, T036). Only names are printed, never a signal's text. The catalog is read from the
// server; when it cannot be, the names are listed as not checked and the check goes on.
func renderCatalogNames(cmd *cobra.Command, global *globalOptions, names []graph.Ref) {
	out := cmd.OutOrStdout()
	byName := map[string]graph.Ref{}
	for _, n := range names {
		byName[n.String()] = n
	}
	keys := make([]string, 0, len(byName))
	for k := range byName {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	fmt.Fprintf(out, "catalog: %d name(s) observed\n", len(keys))
	if len(keys) == 0 {
		return
	}
	mapped, err := catalogNameMap(cmd, global)
	if err != nil {
		// Why goes to stderr: it names a server and an address, which the check's own output
		// must not depend on.
		fmt.Fprintf(cmd.ErrOrStderr(), "catalog: not read from %s: %v\n", global.Server, err)
		fmt.Fprintf(out, "  the catalog could not be read (see stderr); the names are not checked\n")
		for _, k := range keys {
			fmt.Fprintf(out, "  %s: not checked\n", k)
		}
		return
	}
	for _, k := range keys {
		answer, ok := mapped[k]
		if !ok {
			answer = "unmapped (aisre catalog alias add, or accept a proposal)"
		}
		fmt.Fprintf(out, "  %s: %s\n", k, answer)
	}
}

// catalogNameMap is every environment-free alias the catalog has now, spelled `ns=value`, with
// the answer the check prints for it.
func catalogNameMap(cmd *cobra.Command, global *globalOptions) (map[string]string, error) {
	clients, err := newClients(global)
	if err != nil {
		return nil, err
	}
	resp, err := clients.catalog.ListEntries(cmd.Context(), connect.NewRequest(&graphv1.ListEntriesRequest{
		AsOf: &graphv1.AsOf{ValidAt: timestamppb.Now()},
	}))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range resp.Msg.GetHidden() {
		if a.GetEnvironment() == "" {
			out[graph.RefFromProto(a.GetName()).String()] = "hidden (not a service)"
		}
	}
	for _, e := range resp.Msg.GetEntries() {
		answer := "entry " + e.GetSlug()
		switch e.GetStatus() {
		case graph.CatalogStatusHidden:
			answer = "hidden (entry " + e.GetSlug() + " is hidden)"
		case graph.CatalogStatusProposed:
			answer += " (proposed, not yet confirmed)"
		}
		for _, a := range e.GetAliases() {
			if a.GetEnvironment() == "" {
				out[graph.RefFromProto(a.GetName()).String()] = answer
			}
		}
	}
	return out, nil
}
