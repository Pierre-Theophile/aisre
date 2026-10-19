// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	notionfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/notion"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed notion` (007 US3, US5; contracts/notion-connector.md §3–§5, docs/connectors/notion.md).
//
// Two credentials: the integration secret ($SRE_AGENT_NOTION_TOKEN) reads the databases the operator
// named, through the six published operations and nothing else; the graph token (--token /
// $SRE_AGENT_TOKEN) ingests for source `notion:<workspace>`. Before any call, the configuration must
// acknowledge `notion.capabilities` (exit 3 otherwise), and the integration's workspace must be the
// configured label.

func init() { addFeedSubcommand(newFeedNotionCommand) }

// EnvNotionToken holds the integration secret.
const EnvNotionToken = "SRE_AGENT_NOTION_TOKEN" //nolint:gosec // the name of a variable, not a credential

type feedNotionOptions struct {
	config    string
	state     string
	once      bool
	check     bool
	cycles    int
	recordDir string
	replayDir string
	batchSize int
	// notionAPI overrides Notion's base URL. Hidden: it exists for tests against a recorded twin.
	notionAPI string
}

func newFeedNotionCommand(global *globalOptions) *cobra.Command {
	opts := &feedNotionOptions{}
	cmd := &cobra.Command{
		Use:   "notion",
		Short: "Register the postmortems and runbooks of named Notion databases as knowledge",
		Long: "Reads the Notion databases named in --config and registers each page whose entity\n" +
			"property maps to a graph entity as a KNOWLEDGE_DOC with one CONCERNS edge per entity, so\n" +
			"an investigation can cite it. The title and up to three named summary properties are\n" +
			"registered with it; the named index_properties and the page body, by heading, are sent\n" +
			"as sections to the agent's knowledge store, sanitised, so a search finds a page by what\n" +
			"it says. A body is read again only when the page was edited.\n\n" +
			"Every poll_interval it reads the pages edited since the last poll; at start and every\n" +
			"reconcile_every it lists every page and ends the links of the pages that left.\n\n" +
			"The integration secret is read from $" + EnvNotionToken + ". Notion does not report an\n" +
			"integration's capabilities, so the configuration must acknowledge notion.capabilities\n" +
			"(created with Read content only), or the command exits 3 before any call.\n\n" +
			"--check reads every configured database once and prints, per database, its data sources,\n" +
			"which configured properties were found, the pages registered and each page that was not,\n" +
			"with the reason, and per page the sections that would be indexed (label, origin, size)\n" +
			"and what was skipped and why, never their text; it sends nothing to the graph.\n\n" +
			"See docs/connectors/notion.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedNotion(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.config, "config", "", "the connector's configuration (required)")
	flags.StringVar(&opts.state, "state", "", "where the connector keeps what it registered "+
		"(default notion-state.json beside --config)")
	flags.BoolVar(&opts.once, "once", false, "one reconciliation, then exit")
	flags.BoolVar(&opts.check, "check", false, "read everything, emit nothing, print what would be registered and why, and exit; "+
		"the state file is read and never written")
	flags.IntVar(&opts.cycles, "cycles", 0, "how many cycles to run; 0 runs until interrupted")
	flags.StringVar(&opts.recordDir, "record", "", "also record the run, sanitised, to this fixture directory "+
		"($SRE_AGENT_CORPUS_KEY is the pseudonym key)")
	flags.StringVar(&opts.replayDir, "replay", "", "read payloads from this fixture directory instead of Notion")
	flags.IntVar(&opts.batchSize, "batch", 64, "events per ingest batch")
	flags.StringVar(&opts.notionAPI, "notion-api", "", "Notion API base URL (tests only)")
	_ = flags.MarkHidden("notion-api")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func runFeedNotion(ctx context.Context, global *globalOptions, opts *feedNotionOptions, cmd *cobra.Command) error {
	if opts.check && opts.recordDir != "" {
		return exitErrorf(ExitUsage, "feed notion: --check emits nothing, so there is nothing for --record to record")
	}
	if opts.recordDir != "" && opts.replayDir != "" {
		return exitErrorf(ExitUsage, "feed notion: --record and --replay are mutually exclusive; "+
			"re-recording a replay would produce a fixture of a fixture")
	}
	cfg, err := notionfeeder.LoadConfig(opts.config)
	if err != nil {
		return exitErrorf(ExitUsage, "feed notion: %v", err)
	}
	// The gate first: a configuration that does not acknowledge the integration's capabilities makes
	// no call at all (contracts §3).
	if _, err := notionx.CheckReadOnly(cfg.Workspace, cfg.AcknowledgeReadOnly); err != nil {
		return exitErrorf(ExitAuth, "feed notion: %v", err)
	}
	live := opts.replayDir == ""

	f, err := notionfeeder.New(notionfeeder.Options{Config: cfg})
	if err != nil {
		return exitErrorf(ExitUsage, "feed notion: %v", err)
	}

	var src feeder.Source
	var recording *deployrecord.Tee
	var budget *notionx.Budget
	var san *sanitise.Sanitiser
	var integrationWorkspace string
	if live {
		secret, err := notionx.NewSecret(os.Getenv(EnvNotionToken))
		if err != nil {
			return exitErrorf(ExitAuth, "feed notion: $%s: %v", EnvNotionToken, err)
		}
		budget = notionx.NewBudget(notionx.BudgetOptions{})
		client, err := notionx.New(notionx.Options{Secret: secret, BaseURL: opts.notionAPI, Budget: budget})
		if err != nil {
			return exitErrorf(ExitUsage, "feed notion: %v", err)
		}
		me, err := client.UsersMe(ctx)
		var apiErr *notionx.APIError
		switch {
		case errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403):
			return exitErrorf(ExitAuth, "feed notion: Notion refused the integration secret: %v", err)
		case err != nil:
			return exitErrorf(ExitTransport, "feed notion: asking Notion which workspace the integration belongs to: %v", err)
		case !strings.EqualFold(strings.TrimSpace(me.Workspace), cfg.Workspace):
			return exitErrorf(ExitAuth, "feed notion: the configuration names workspace %q, but the integration "+
				"belongs to %q; a secret from another workspace cannot feed this source", cfg.Workspace, me.Workspace)
		}
		integrationWorkspace = me.Workspace
		statePath := opts.state
		if statePath == "" {
			statePath = filepath.Join(filepath.Dir(opts.config), "notion-state.json")
		}
		poller, err := notionfeeder.NewPoller(notionfeeder.PollerOptions{
			Client: client, Config: cfg, StatePath: statePath, Once: opts.once || opts.check, Cycles: opts.cycles,
			ReadOnlyState: opts.check,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed notion: %v", err)
		}
		src = poller
		if opts.recordDir != "" {
			if san, err = recordingSanitiser(); err != nil {
				return exitErrorf(ExitUsage, "feed notion: --record is sanitised before anything touches disk, "+
					"and it cannot start: %v", err)
			}
			tee, err := deployrecord.NewTee(poller, notionfeeder.Kind, san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed notion: %v", err)
			}
			recording = tee.WithPrepare(notionfeeder.PreparePayload())
			src = recording
		}
	} else {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed notion: %v", err)
		}
		src = fileSource
	}

	if opts.check {
		// Everything is read and nothing leaves the process: the events go to memory and are dropped.
		memory := emit.NewMemoryEmitter(f.Describe())
		if err := f.Run(ctx, src, memory); err != nil {
			return exitErrorf(ExitTransport, "feed notion --check: %v", err)
		}
		renderNotionCheck(cmd, cfg, opts.config, integrationWorkspace, f.Report(), budget)
		renderCatalogNames(cmd, global, notionNames(memory.Events()))
		return nil
	}

	connectEm, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(), emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed notion: %v", err)
	}
	runErr := f.Run(ctx, src, connectEm)

	if recording != nil {
		// The shadow derives the recording's events in the recording's vocabulary: the workspace and
		// database ids pseudonymised and the signing principal, a person, not recorded.
		shadowCfg, err := cfg.ForRecording(func(kind, value string) (string, error) {
			return san.Identifier(sanitise.Kind(kind), value)
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed notion: %v", err)
		}
		shadow, err := notionfeeder.New(notionfeeder.Options{Config: shadowCfg})
		if err != nil {
			return exitErrorf(ExitUsage, "feed notion: %v", err)
		}
		events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "notion", shadow)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "feed notion: the recording was not completed: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
				recording.Written(), events, opts.recordDir, recording.Dropped())
		}
	}
	if runErr != nil && (!live || !errors.Is(runErr, context.Canceled)) {
		return exitErrorf(ExitTransport, "feed notion: %v", runErr)
	}
	renderNotionReport(cmd, f.Report())
	if budget != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "notion budget: %s\n", budget.Report())
	}
	return nil
}

// renderNotionReport prints the registration report of contracts §6: per database, what was
// registered and what was skipped and why. Titles are printed; summaries never are.
func renderNotionReport(cmd *cobra.Command, r notionfeeder.Report) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", r.Note())
	for _, d := range r.Databases {
		fmt.Fprintf(out, "database %s (%s): %d registered, %d gone\n", d.Database, d.Kind, len(d.Registered), d.Gone)
		var skipped []string
		for _, s := range d.Skipped {
			skipped = append(skipped, fmt.Sprintf("  skipped %q: %s", s.Title, s.Reason))
		}
		sort.Strings(skipped)
		for _, line := range skipped {
			fmt.Fprintln(out, line)
		}
	}
	for _, gap := range r.Gaps {
		fmt.Fprintf(out, "coverage gap: %s\n", gap)
	}
}

// renderNotionSections prints what was indexed of each page whose sections were read (contracts
// §6, 008 FR-040): per page, one line per section with its origin and size, then what was skipped
// and why. Never section text.
func renderNotionSections(cmd *cobra.Command, d notionfeeder.DatabaseReport) {
	out := cmd.OutOrStdout()
	ids := make([]string, 0, len(d.Sections))
	for id := range d.Sections {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := d.Sections[ids[i]], d.Sections[ids[j]]
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return ids[i] < ids[j]
	})
	fmt.Fprintf(out, "  sections read: %d page(s)\n", len(ids))
	for _, id := range ids {
		page := d.Sections[id]
		fmt.Fprintf(out, "    %q: %d section(s)\n", page.Title, len(page.Sections))
		for _, line := range page.Sections {
			fmt.Fprintf(out, "      %s\n", line)
		}
		for _, line := range page.Skipped {
			fmt.Fprintf(out, "      skipped %s\n", line)
		}
	}
}

// renderNotionCheck prints `--check` (contracts §6, spec US5): what the integration's capabilities
// rest on, the workspace, and per database its data sources, the configured properties found, the
// pages registered and each page not registered with its reason. Titles are printed; summaries and
// bodies never are.
func renderNotionCheck(cmd *cobra.Command, cfg *notionfeeder.Config, configPath, workspace string,
	r notionfeeder.Report, budget *notionx.Budget) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "notion check: nothing was sent to the graph\n")
	fmt.Fprintf(out, "capabilities: not verifiable through Notion's API; %s acknowledges %s "+
		"(an integration created with Read content only)\n", configPath, notionx.CapabilitiesAcknowledgement)
	if workspace != "" {
		fmt.Fprintf(out, "workspace: %q, the integration's own (source notion:%s)\n", workspace, cfg.Workspace)
	} else {
		fmt.Fprintf(out, "workspace: %q, not asked of Notion: this check read a recording\n", cfg.Workspace)
	}
	for i, d := range r.Databases {
		b := cfg.Databases[i]
		fmt.Fprintf(out, "\ndatabase %s (%s)\n", d.Database, d.Kind)
		sources := "none could be read"
		if len(d.DataSources) > 0 {
			sources = strings.Join(d.DataSources, ", ")
		}
		fmt.Fprintf(out, "  data sources: %s\n", sources)
		property := func(role, name string) {
			found := "found"
			if !d.PropertiesFound[name] {
				found = "NOT FOUND on any page; check the spelling"
			}
			fmt.Fprintf(out, "  property %q (%s): %s\n", name, role, found)
		}
		property("entities", b.EntityProperty)
		for _, name := range b.SummaryProperties {
			property("summary", name)
		}
		for _, name := range b.Indexed() {
			if !slices.Contains(b.SummaryProperties, name) {
				property("indexed", name)
			}
		}
		if b.AuthoredProperty != "" {
			property("authored", b.AuthoredProperty)
		}
		body := "indexed by heading"
		if !b.BodyIndexed() {
			body = "not indexed (index_body: false)"
		}
		fmt.Fprintf(out, "  page bodies: %s\n", body)
		printSorted := func(header string, lines []string) {
			fmt.Fprintf(out, "  %s: %d\n", header, len(lines))
			sort.Strings(lines)
			for _, line := range lines {
				fmt.Fprintln(out, "    "+line)
			}
		}
		var registered, skipped []string
		for _, title := range d.Registered {
			registered = append(registered, fmt.Sprintf("%q", title))
		}
		for _, s := range d.Skipped {
			skipped = append(skipped, fmt.Sprintf("%q: %s", s.Title, s.Reason))
		}
		printSorted("registered", registered)
		printSorted("not registered", skipped)
		renderNotionSections(cmd, d)
		fmt.Fprintf(out, "  ended (trashed, moved or deleted): %d\n", d.Gone)
		fmt.Fprintf(out, "  outside the configured databases: %d\n", d.Outside())
	}
	for _, gap := range r.Gaps {
		fmt.Fprintf(out, "coverage gap: %s\n", gap)
	}
	if budget != nil {
		fmt.Fprintf(out, "notion budget: %s\n", budget.Report())
	}
}

// notionNames are the catalog names of the pages a run registered: `notion.value=<database>/<value>`
// for every value of their entity property.
func notionNames(events []*graphv1.EventEnvelope) []graph.Ref {
	var out []graph.Ref
	for _, ev := range events {
		node := ev.GetUpsertNode()
		if node == nil || node.GetType() != graphv1.NodeType_KNOWLEDGE_DOC {
			continue
		}
		fields := node.GetProps().GetFields()
		database := fields[graph.NotionDatabaseProp].GetStringValue()
		for _, v := range fields[graph.NotionValuesProp].GetListValue().GetValues() {
			if value := v.GetStringValue(); database != "" && value != "" {
				out = append(out, catalog.NotionValueRef(database, value))
			}
		}
	}
	return out
}
