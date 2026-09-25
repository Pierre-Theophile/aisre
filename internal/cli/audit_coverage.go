// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// `audit coverage` and its subcommands (T013, T014, T017, T018; contracts/cli.md §The coverage
// audit, FR-064, FR-069 to FR-071b).
//
// This is the one command in the tree that runs before anything else is worth running. It takes
// a human-supplied incident list, measures how much of the causal ground the graph can see,
// publishes the ceiling with the incident count it rests on, and hands π₀ = 1 − ceiling to the
// ledger. No model runs anywhere in it and no investigation is performed: the audit measures,
// it never infers (FR-069).
//
// It talks to `--db` directly rather than through the server, for the same reason `fixture` does:
// what it writes is a record of a measurement a human made, not an event a feeder observed, and
// there is no RPC through which a caller may assert one. `--db` is optional throughout — the
// audit's arithmetic needs no database at all, and an operator auditing a list on a laptop
// should not need one.

func newAuditCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Measure what the graph could ever explain",
		Long: "audit runs the coverage audit of spec 002 User Story 0: given a list of past incidents\n" +
			"with their alert instants and their known causes, it classifies each as cause present in\n" +
			"the graph at alert time, cause absent with a category, or undecidable with a reason, and\n" +
			"publishes the aggregate as the recall ceiling.\n\n" +
			"The ceiling bounds every published recall- and coverage-dependent target (FR-071), and\n" +
			"1 − ceiling is the prior of `no observed change explains this` (ADR-0005 D9).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(newAuditCoverageCommand(global))
	return cmd
}

type auditCoverageOptions struct {
	input         string
	incidents     string
	out           string
	dsn           string
	feederSet     string
	graphConfig   string
	aggregateOnly bool
}

func newAuditCoverageCommand(global *globalOptions) *cobra.Command {
	opts := &auditCoverageOptions{}

	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Classify an incident list and publish the recall ceiling",
		Long: "coverage reads a YAML or JSON incident list (docs/evaluation/coverage-audit-format.md),\n" +
			"classifies every incident under every declared feeder set, and publishes the ceiling of\n" +
			"the set in force together with the incident count it rests on, the per-category counts\n" +
			"and the unobservable remainder.\n\n" +
			"The input carries no free text: every field is an identifier, a date, or a member of a\n" +
			"closed set, so a private audit can be transcribed into it and the file still published.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuditCoverage(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.input, "input", "", "incident list to audit (YAML or JSON)")
	flags.StringVar(&opts.incidents, "incidents", "", "alias of --input (contracts/cli.md)")
	flags.StringVar(&opts.out, "out", "",
		"directory to write coverage-audit-<id>.json and .md into")
	flags.StringVar(&opts.dsn, "db", "",
		"PostgreSQL DSN to record the audit in (default $"+EnvDSN+"; omit to compute without storing)")
	flags.StringVar(&opts.feederSet, "feeder-set", "",
		"feeder set whose ceiling is published (default: the highest rung declared)")
	flags.StringVar(&opts.graphConfig, "graph-config", "", "alias of --feeder-set (contracts/cli.md)")
	flags.BoolVar(&opts.aggregateOnly, "aggregate-only", false,
		"drop per-incident detail from the output, for a result that is safe to publish")

	cmd.AddCommand(
		newAuditCompareCommand(global),
		newAuditGuardCommand(global),
		newAuditGapsCommand(global),
	)
	return cmd
}

func runAuditCoverage(cmd *cobra.Command, global *globalOptions, opts *auditCoverageOptions) error {
	input, err := singleValue("--input", opts.input, "--incidents", opts.incidents)
	if err != nil {
		return err
	}
	if input == "" {
		return exitErrorf(ExitUsage, "no incident list: pass --input <file>")
	}
	feederSet, err := singleValue("--feeder-set", opts.feederSet, "--graph-config", opts.graphConfig)
	if err != nil {
		return err
	}

	list, err := audit.LoadList(input)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	result, err := audit.Run(list, audit.Options{
		FeederSet:     feederSet,
		AggregateOnly: opts.aggregateOnly,
	})
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	written, err := writeAuditFiles(opts.out, result)
	if err != nil {
		return err
	}

	// --db is optional and explicit: the audit's arithmetic needs no database, and writing to
	// one because $PG_DSN happens to be exported would be a surprise. The flag has to be
	// passed; $PG_DSN then fills in its value, as everywhere else in the tree.
	if cmd.Flags().Changed("db") {
		dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
		if err != nil {
			return err
		}
		store, err := postgres.Open(cmd.Context(), dsn)
		if err != nil {
			return storeError("open database", err)
		}
		defer store.Close()
		if err := audit.Persist(cmd.Context(), store, result); err != nil {
			return storeError("record the audit", err)
		}
	}

	return renderAuditResult(cmd, global, result, written)
}

// writeAuditFiles writes the machine-readable result and the publishable Markdown into dir.
func writeAuditFiles(dir string, result *audit.Result) ([]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, exitWith(ExitUsage, fmt.Errorf("create %s: %w", dir, err))
	}

	jsonPath := filepath.Join(dir, "coverage-audit-"+result.AuditID+".json")
	// The same canonical serialization the goldens use, so a checked-in audit and a fresh run
	// of the command can be diffed byte for byte.
	encoded, err := graph.CanonicalJSON(result)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	if err := os.WriteFile(jsonPath, append(encoded, '\n'), 0o600); err != nil {
		return nil, exitWith(ExitUsage, fmt.Errorf("write %s: %w", jsonPath, err))
	}

	markdownPath := filepath.Join(dir, "coverage-audit-"+result.AuditID+".md")
	if err := os.WriteFile(markdownPath, []byte(result.Markdown()), 0o600); err != nil {
		return nil, exitWith(ExitUsage, fmt.Errorf("write %s: %w", markdownPath, err))
	}
	return []string{jsonPath, markdownPath}, nil
}

func renderAuditResult(cmd *cobra.Command, global *globalOptions, result *audit.Result, written []string) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(result)
	}

	if err := p.writeLine("audit %s over %s: ceiling %.6f (%d of %d classifiable incidents), feeder set %s",
		result.AuditID, result.Corpus.Label, result.Ceiling,
		observedOf(result), result.ClassifiableCount, result.FeederSetInForce); err != nil {
		return err
	}
	if err := p.writeLine("π₀ = 1 − ceiling = %.6f — the prior of `no observed change explains this`",
		result.Prior); err != nil {
		return err
	}

	rows := make([][]string, 0, len(result.FeederSets))
	for _, set := range result.FeederSets {
		rows = append(rows, []string{
			set.Name,
			fmt.Sprintf("%d/%d", set.Observed, set.Classifiable),
			fmt.Sprintf("%.6f", set.Ceiling),
			fmt.Sprintf("%d", set.SymptomOnly),
			fmt.Sprintf("%d", set.NotObservable),
			fmt.Sprintf("%d", set.Undecidable),
			strings.Join(set.Adds, " "),
		})
	}
	if err := p.writeTable(
		[]string{"FEEDER SET", "OBSERVED", "CEILING", "SYMPTOM ONLY", "UNOBSERVABLE", "UNDECIDABLE", "ADDS"},
		rows,
	); err != nil {
		return err
	}

	if inForce, ok := result.FeederSet(result.FeederSetInForce); ok && len(inForce.Missing) > 0 {
		if err := p.writeLine("\nmissing causes by category (the feeders worth writing next):"); err != nil {
			return err
		}
		missing := make([][]string, 0, len(inForce.Missing))
		for _, row := range inForce.Missing {
			missing = append(missing, []string{
				string(row.Category),
				fmt.Sprintf("%d", row.Count),
				fmt.Sprintf("%.6f", row.Share),
			})
		}
		if err := p.writeTable([]string{"CATEGORY", "INCIDENTS", "SHARE"}, missing); err != nil {
			return err
		}
	}

	if len(result.Remainder) > 0 {
		if err := p.writeLine("\nunobservable remainder — each category owes the corpus a fixture (FR-071b):"); err != nil {
			return err
		}
		remainder := make([][]string, 0, len(result.Remainder))
		for _, row := range result.Remainder {
			classes := make([]string, len(row.Classes))
			for i, class := range row.Classes {
				classes[i] = string(class)
			}
			remainder = append(remainder, []string{
				string(row.Category),
				strings.Join(classes, ","),
				fmt.Sprintf("%d", row.Count),
			})
		}
		if err := p.writeTable([]string{"CATEGORY", "GROUND TRUTH", "INCIDENTS"}, remainder); err != nil {
			return err
		}
	}

	for _, path := range written {
		if err := p.writeLine("wrote %s", path); err != nil {
			return err
		}
	}
	return nil
}

func observedOf(result *audit.Result) int {
	if set, ok := result.FeederSet(result.FeederSetInForce); ok {
		return set.Observed
	}
	return 0
}

func newAuditCompareCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "compare <before.json> <after.json>",
		Short: "Compare two audit runs and attribute the movement to the feeders added",
		Long: "compare reads two published audit results, older first, and reports the movement in the\n" +
			"ceiling together with the feeder sets and feeders that were added between them.\n\n" +
			"Every rung of the feeder ladder is reported, including the ones whose ceiling did not\n" +
			"move: FR-071a requires a ceiling that did not move to be reported as such rather than\n" +
			"omitted, because \"we shipped the connector and the ceiling did not move\" is a result.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			before, err := audit.LoadResult(args[0])
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			after, err := audit.LoadResult(args[1])
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			comparison, err := audit.Compare(before, after)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			return renderAuditComparison(cmd, global, comparison)
		},
	}
}

func renderAuditComparison(cmd *cobra.Command, global *globalOptions, comparison *audit.Comparison) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(comparison)
	}

	if err := p.writeLine("%s", comparison.Summary); err != nil {
		return err
	}
	rows := make([][]string, 0, len(comparison.FeederSets))
	for _, movement := range comparison.FeederSets {
		before, after := fmt.Sprintf("%.6f", movement.CeilingBefore), fmt.Sprintf("%.6f", movement.CeilingAfter)
		if !movement.PresentBefore {
			before = unknownCell
		}
		if !movement.PresentAfter {
			after = unknownCell
		}
		rows = append(rows, []string{movement.Name, before, after, movement.Note})
	}
	if err := p.writeTable([]string{"FEEDER SET", "CEILING BEFORE", "CEILING AFTER", "MOVEMENT"}, rows); err != nil {
		return err
	}

	if len(comparison.Incidents) > 0 {
		if err := p.writeLine("\nincidents that moved:"); err != nil {
			return err
		}
		moved := make([][]string, 0, len(comparison.Incidents))
		for _, movement := range comparison.Incidents {
			moved = append(moved, []string{
				movement.IncidentRef,
				string(movement.Category),
				string(movement.VerdictBefore),
				string(movement.VerdictAfter),
				strings.Join(movement.AttributedTo, " "),
			})
		}
		if err := p.writeTable(
			[]string{"INCIDENT", "CATEGORY", "BEFORE", "AFTER", "ATTRIBUTED TO"}, moved); err != nil {
			return err
		}
	}
	for _, note := range comparison.Notes {
		if err := p.writeLine("note: %s", note); err != nil {
			return err
		}
	}
	return nil
}

type auditGuardOptions struct {
	report string
	audit  string
}

func newAuditGuardCommand(global *globalOptions) *cobra.Command {
	opts := &auditGuardOptions{}

	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Fail the build when a ceiling-bounded target exceeds the ceiling",
		Long: "guard checks an evaluation report's published targets against an audit (FR-071).\n\n" +
			"It checks the [ceiling-bounded] criteria and NOTHING else: SC-001, SC-006, SC-021 and\n" +
			"any published target for culprit rank, top-k, lift or localisation. A criterion\n" +
			"annotated [not ceiling-bounded: precision|latency|invariance|validity] is reported as\n" +
			"not checked and is never scaled down to the ceiling. A criterion carrying no annotation\n" +
			"at all fails loudly: \"nobody said which side it was on\" must not read as \"it passed\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(opts.report) == "" || strings.TrimSpace(opts.audit) == "" {
				return exitErrorf(ExitUsage, "guard needs both --report <eval-report> and --audit <audit.json>")
			}
			input, err := audit.LoadGuardInput(opts.report)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			result, err := audit.LoadResult(opts.audit)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			report, err := audit.Guard(input, result)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			if err := renderAuditGuard(cmd, global, report); err != nil {
				return err
			}
			if !report.OK() {
				return exitErrorf(ExitVerification,
					"ceiling guard: %d of %d checked criteria exceed the ceiling of audit %s or are "+
						"unannotated (FR-071)", report.Failed, report.Checked, report.AuditID)
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.report, "report", "", "evaluation report publishing the criteria to check")
	flags.StringVar(&opts.audit, "audit", "", "audit result the ceiling is read from")
	return cmd
}

func renderAuditGuard(cmd *cobra.Command, global *globalOptions, report *audit.GuardReport) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(report)
	}

	if err := p.writeLine("ceiling guard against audit %s: ceiling %.6f over %d classifiable incidents (%s)",
		report.AuditID, report.Ceiling, report.ClassifiableCount, report.FeederSet); err != nil {
		return err
	}
	rows := make([][]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		annotation := string(finding.Annotation)
		if annotation == "" {
			annotation = unknownCell
		}
		rows = append(rows, []string{
			finding.Criterion,
			fmt.Sprintf("%.6f", finding.Target),
			annotation,
			string(finding.Outcome),
			finding.Detail,
		})
	}
	if err := p.writeTable(
		[]string{"CRITERION", "TARGET", "ANNOTATION", "OUTCOME", "DETAIL"}, rows); err != nil {
		return err
	}
	return p.writeLine("checked %d, passed %d, failed %d, not checked %d",
		report.Checked, report.Passed, report.Failed, report.NotChecked)
}

type auditGapsOptions struct {
	audit    string
	fixtures string
}

func newAuditGapsCommand(global *globalOptions) *cobra.Command {
	opts := &auditGapsOptions{}

	cmd := &cobra.Command{
		Use:   "gaps",
		Short: "Report categories of the unobservable remainder with no fixture",
		Long: "gaps names the categories of an audit's unobservable remainder that the evaluation\n" +
			"corpus does not cover (FR-071b). Each needs at least one fixture whose ground truth is\n" +
			"`unobserved` or `not_change_induced` carrying that category, graded under SC-023.\n\n" +
			"A gap is a warning, not a failure: the corpus being incomplete is a fact about the\n" +
			"corpus, and failing the build over it would only teach people to stop measuring. The\n" +
			"evaluation report names the gap on every run instead (wired in by T109).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(opts.audit) == "" {
				return exitErrorf(ExitUsage, "gaps needs --audit <audit.json>")
			}
			result, err := audit.LoadResult(opts.audit)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			report, err := audit.DetectGaps(result, opts.fixtures)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			return renderAuditGaps(cmd, global, report)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.audit, "audit", "", "audit result whose remainder is checked")
	flags.StringVar(&opts.fixtures, "fixtures", audit.FixtureRoot, "incident fixture directory to scan")
	return cmd
}

func renderAuditGaps(cmd *cobra.Command, global *globalOptions, report *audit.GapReport) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(report)
	}

	if err := p.writeLine("corpus gaps for audit %s (%s), scanning %s: %d fixtures, %d with a remainder ground truth",
		report.AuditID, report.FeederSet, report.FixtureRoot,
		report.FixturesScanned, report.GroundTruthFound); err != nil {
		return err
	}
	rows := make([][]string, 0, len(report.Gaps)+len(report.Covered))
	for _, covered := range report.Covered {
		rows = append(rows, []string{
			string(covered.Category), "covered", strings.Join(covered.Fixtures, " "),
		})
	}
	for _, gap := range report.Gaps {
		rows = append(rows, []string{string(gap.Category), "GAP", gap.Want})
	}
	if err := p.writeTable([]string{"CATEGORY", "STATUS", "DETAIL"}, rows); err != nil {
		return err
	}
	if report.Warning != "" {
		// Exit 0: a gap is reported, not gated.
		return p.writeLine("warning: %s", report.Warning)
	}
	return p.writeLine("every category of the unobservable remainder has a fixture")
}

// singleValue resolves a flag that has an alias, refusing a caller who sets both to different
// values rather than silently preferring one.
func singleValue(primaryName, primary, aliasName, alias string) (string, error) {
	primary, alias = strings.TrimSpace(primary), strings.TrimSpace(alias)
	switch {
	case primary != "" && alias != "" && primary != alias:
		return "", exitErrorf(ExitUsage, "%s and %s disagree: %q and %q",
			primaryName, aliasName, primary, alias)
	case primary != "":
		return primary, nil
	default:
		return alias, nil
	}
}
