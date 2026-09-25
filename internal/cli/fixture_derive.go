// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// `fixture derive` (tasks.md T107; FR-062a, contracts/cli.md §Fixtures, contracts/incident-format.md
// §Derived fixtures).
//
// The metamorphic generator's command line. `internal/fixture` writes the manifest, the events and
// the payloads; this command completes the variant, because the two things still missing — the
// recorded `world/` and the `golden/` files — are both functions of the **regenerated graph** and
// both need a database and the engine, neither of which `internal/fixture` may reach.
//
// The order matters and is not arbitrary. The goldens come from the variant's own events, and the
// world's engine pass needs the variant's own `incident:` block: a world recorded before the
// manifest's ground truth was rewritten would degrade the culprit a culprit-deleted variant no
// longer has. Both therefore run after the derivation, over the directory it just wrote.
//
// Nothing here copies the parent's `world/`. A copied world answers with the parent's names and
// the parent's instants, so a name-permuted variant would ask about one service and be told about
// another, and every answer would come back `not_recorded` — a miss rate of 1.0 that looks like
// an engine defect and is a generator defect.

type fixtureDeriveOptions struct {
	transform string
	out       string
	dsn       string
	auditPath string
	offset    time.Duration
	seed      string
	force     bool
	skipWorld bool
}

func newFixtureDeriveCommand(global *globalOptions) *cobra.Command {
	opts := &fixtureDeriveOptions{}

	cmd := &cobra.Command{
		Use:   "derive <dir> --transform <t> --out <dir>",
		Short: "Generate a metamorphic variant of an incident fixture",
		Long: "derive writes the variant of an incident fixture that a transform defines: a complete\n" +
			"fixture directory whose `provenance` names its parent and its transformation and whose\n" +
			"`ground_truth` is the transform's published image of the parent's (FR-062a).\n\n" +
			"The four transforms:\n" +
			"  culprit-deleted  the culprit's events are removed; the ground truth becomes\n" +
			"                   `unobserved` with the same symptoms, and naming a decoy fails.\n" +
			"  decoy-injected   one more plausible change inside the window on an adjacent entity,\n" +
			"                   declared `coincident` with an exonerating predicate the world answers.\n" +
			"  time-shifted     every instant moves by a fixed offset; the verdict must not move.\n" +
			"  name-permuted    a deterministic, namespace-preserving bijection over entity names,\n" +
			"                   seeded from the parent id; the verdict, mapped through it, must not\n" +
			"                   move. It is what tests that the engine ranks on structure rather than\n" +
			"                   on guilty-sounding names.\n\n" +
			"Variants are generated, not committed: deriving twice yields identical bytes, so a\n" +
			"failing invariant is reproduced by re-running this command rather than by finding the\n" +
			"directory somebody generated last month.\n\n" +
			"`world/` is re-recorded from the variant's own graph and `golden/` re-recorded from its\n" +
			"own events, so the variant passes `fixture verify`. Both need --db.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := fixture.Derive(args[0], fixture.DeriveOptions{
				Transform: opts.transform,
				Out:       opts.out,
				Offset:    opts.offset,
				Seed:      opts.seed,
				Force:     opts.force,
			})
			if err != nil {
				return exitWith(ExitUsage, err)
			}

			if !opts.skipWorld {
				if err := completeVariant(cmd, opts, report); err != nil {
					return err
				}
			}
			if err := fixture.WriteDeriveReport(report); err != nil {
				return exitWith(ExitTransport, err)
			}
			return renderDeriveReport(cmd, global, report)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.transform, "transform", "",
		"culprit-deleted | decoy-injected | time-shifted | name-permuted")
	flags.StringVar(&opts.out, "out", "",
		"the variant's directory; its base name becomes the variant's id")
	flags.StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN whose role has CREATEDB (default $"+EnvDSN+")")
	flags.StringVar(&opts.auditPath, "audit", defaultAuditPath,
		"the published coverage audit the world's engine pass reads π₀ from")
	flags.DurationVar(&opts.offset, "offset", fixture.DefaultTimeShift,
		"how far time-shifted moves every instant; a whole number of days keeps the seasonal "+
			"baseline aligned")
	flags.StringVar(&opts.seed, "seed", "",
		"seeds name-permuted's bijection (default: the parent's id, so a variant is reproducible by name)")
	flags.BoolVar(&opts.force, "force", false, "replace a non-empty output directory")
	flags.BoolVar(&opts.skipWorld, "no-record", false,
		"write the manifest and events only, recording neither the world nor the goldens. The "+
			"variant will NOT pass `fixture verify`; it is for inspecting what a transform did.")
	_ = cmd.MarkFlagRequired("transform")
	_ = cmd.MarkFlagRequired("out")

	return cmd
}

// completeVariant records the two things a variant needs that are functions of its own graph.
func completeVariant(cmd *cobra.Command, opts *fixtureDeriveOptions, report *fixture.DeriveReport) error {
	dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
	if err != nil {
		return err
	}
	prior, err := loadPrior(opts.auditPath)
	if err != nil {
		return err
	}
	if err := recordVariant(cmd.Context(), fixture.NewStoreFactoryFromDSN(dsn), report, prior); err != nil {
		return exitWith(ExitTransport, err)
	}
	return nil
}

// recordVariant re-records a variant's goldens and its world, in that order.
//
// The goldens first, because they are the cheaper of the two and a variant whose graph does not
// answer its own manifest queries is a variant whose world is not worth recording. Both come from
// the variant's own directory: nothing here reads the parent.
func recordVariant(
	ctx context.Context, newStore fixture.StoreFactory, report *fixture.DeriveReport, prior audit.PriorRecord,
) error {
	if _, err := fixture.Record(ctx, newStore, report.Dir, newQueryRunner); err != nil {
		return err
	}
	report.GoldensPending = false
	// A derived variant's world is recorded with no live pass: the coverage it needs is its
	// parent's, transformed, and buying it again with real model calls per variant would multiply
	// the corpus's cost by the number of transforms for a world the generator answers either way.
	if _, err := recordFixtureWorld(ctx, newStore, report.Dir, prior, worldRecordOptions{}); err != nil {
		return err
	}
	report.WorldPending = false
	return nil
}

func renderDeriveReport(cmd *cobra.Command, global *globalOptions, report *fixture.DeriveReport) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(report)
	}
	if err := p.writeLine("%s: %s of %s — %d events (%d removed, %d added), culprit %s",
		report.VariantID, report.Transform, report.ParentID,
		report.Events, report.Removed, report.Added, report.Culprit); err != nil {
		return err
	}
	if report.InjectedDecoy != "" {
		if err := p.writeLine("  injected decoy: %s", report.InjectedDecoy); err != nil {
			return err
		}
	}
	if report.Offset != 0 && report.Transform == fixture.TransformTimeShifted {
		if err := p.writeLine("  every instant shifted by %s", report.Offset); err != nil {
			return err
		}
	}
	if len(report.NameMap) > 0 {
		if err := p.writeLine("  %d names permuted; the map is in %s and is what the invariance check "+
			"maps the parent's verdict through", len(report.NameMap), fixture.DeriveReportFile); err != nil {
			return err
		}
	}
	state := "recorded"
	if report.WorldPending || report.GoldensPending {
		state = "PENDING — this variant will not pass `fixture verify` until they are recorded"
	}
	return p.writeLine("  world and goldens: %s\n  written to %s", state, report.Dir)
}
