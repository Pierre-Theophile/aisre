// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	gcpbackend "github.com/Pierre-Theophile/aisre/internal/backends/gcp"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// `backend list` (tasks.md T036, contracts/cli.md §Workers and backends, FR-047a).
//
// It prints what the budget manager spends against: the registered telemetry backends, the terms
// each serves, the published cost class of each term, and whether the backend reports a vendor
// quota. Budgets are expressed per backend **and per cost class**, never as a flat call count, so
// this table is the operator-facing half of that rule — if a term does not appear here with a
// class, there is no way to budget for it and it is not callable.

func newBackendCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backend",
		Short: "Inspect the registered telemetry backends",
		Long: "A telemetry backend executes one algebra term against one vendor and returns one bounded\n" +
			"digest. It is simultaneously the replay boundary, the vendor abstraction, the sanitisation\n" +
			"point and the prompt-injection barrier (contracts/telemetry-backend.md).\n\n" +
			"A backend serves the telemetry family and nothing else: one that declares a graph or a\n" +
			"knowledge term is rejected at registration.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(newBackendListCommand(global))
	return cmd
}

func newBackendListCommand(global *globalOptions) *cobra.Command {
	var (
		recording string
		gcpOrg    string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the registered telemetry backends, their terms, cost classes and quota reporting",
		Long: "list prints every registered backend with the algebra terms it serves, the published cost\n" +
			"class of each (cheap | standard | expensive), the widest window it answers over, the\n" +
			"redaction policy version it applies and whether it reports the vendor's remaining quota.\n\n" +
			"This build ships three: `recorded`, which answers from a world on disk and makes no network\n" +
			"call; `synthetic`, which generates a fixture's telemetry from that fixture's own graph; and\n" +
			"`gcp`, the first live one, over Cloud Monitoring and Cloud Logging.\n\n" +
			"--gcp prints the GCP backend's declaration. It states the contract rather than opening a\n" +
			"connection, because what a term costs and how wide a window it answers over are facts about\n" +
			"the declaration — and a reader asking them should not need a credential that has passed the\n" +
			"read-only gate.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			registry := sdk.NewRegistry()
			if strings.TrimSpace(recording) != "" {
				b, err := backendFor(recording)
				if err != nil {
					return err
				}
				if err := registry.Register(b); err != nil {
					return exitWith(ExitUsage, err)
				}
			}
			descriptions := registry.Descriptions()
			if slug := strings.TrimSpace(gcpOrg); slug != "" {
				declaration := gcpbackend.Declaration(slug)
				// Validated before it is printed, so the table is one that would actually
				// register: a declaration nobody could register is not worth reading.
				if err := declaration.Validate(); err != nil {
					return exitWith(ExitUsage, err)
				}
				descriptions = append(descriptions, declaration)
			}
			return renderBackendList(cmd, global, descriptions)
		},
	}
	cmd.Flags().StringVar(&recording, "recording", "",
		"directory holding a recorded world/, which registers the `recorded` backend over it")
	cmd.Flags().StringVar(&gcpOrg, "gcp", "",
		"organisation slug: also print the GCP backend's declaration, which needs no credential to state")
	return cmd
}

func renderBackendList(cmd *cobra.Command, global *globalOptions, descriptions []sdk.Description) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		rows := make([]map[string]any, 0, len(descriptions))
		for _, d := range descriptions {
			rows = append(rows, map[string]any{
				"name":             d.Name,
				"vendor":           d.Vendor,
				"terms":            d.Rows(),
				"redaction_policy": d.Redaction.GetPolicyVersion(),
				"version":          d.Version,
				"algebra_version":  d.AlgebraVersion,
				"quota_reporting":  d.QuotaReporting(),
			})
		}
		return p.writeJSON(map[string]any{
			"backends":        rows,
			"algebra_version": engine.AlgebraVersion,
			"cost_classes":    []string{"cheap", "standard", "expensive"},
		})
	}

	var b strings.Builder
	if len(descriptions) == 0 {
		fmt.Fprintf(&b, "no telemetry backend registered.\n\n"+
			"This build ships `recorded` (point --recording at a world), `synthetic` (used by\n"+
			"`fixture record-world`) and `gcp`, the first live one — pass --gcp <org-slug> to\n"+
			"print its declaration without a credential.\n")
		return p.writeRaw(b.String())
	}
	for _, d := range descriptions {
		fmt.Fprintf(&b, "%s (vendor %s, v%s, algebra %s)\n", d.Name, d.Vendor, d.Version, d.AlgebraVersion)
		fmt.Fprintf(&b, "  redaction        policy %s\n", d.Redaction.GetPolicyVersion())
		fmt.Fprintf(&b, "  quota            %s\n", d.QuotaReporting())
		fmt.Fprintf(&b, "  %-20s %-10s %-12s %s\n", "term", "cost class", "window cap", "read only")
		for _, row := range d.Rows() {
			fmt.Fprintf(&b, "  %-20s %-10s %-12s %t\n",
				row.Term, row.CostClass, windowCapOf(row), row.ReadOnly)
		}
		b.WriteString("\n")
	}
	return p.writeRaw(b.String())
}

// windowCapOf renders a term's widest window. A backend that declares none prints "profile", which
// says the profile's cap governs — and is not the same statement as "unbounded".
func windowCapOf(row sdk.TermRow) string {
	if row.MaxWindowSeconds <= 0 {
		return "profile"
	}
	return (time.Duration(row.MaxWindowSeconds) * time.Second).String()
}
