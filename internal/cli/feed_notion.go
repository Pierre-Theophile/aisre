// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	notionfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/notion"
	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed notion` (007 US3, US5; contracts/notion-connector.md §3–§5, docs/connectors/notion.md).
//
// Two credentials: the integration secret ($SRE_AGENT_NOTION_TOKEN) reads the databases the operator
// named, through the five published operations and nothing else; the graph token (--token /
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
			"an investigation can cite it. The page body is never read; the title and up to three\n" +
			"named summary properties are.\n\n" +
			"Every poll_interval it reads the pages edited since the last poll; at start and every\n" +
			"reconcile_every it lists every page and ends the links of the pages that left.\n\n" +
			"The integration secret is read from $" + EnvNotionToken + ". Notion does not report an\n" +
			"integration's capabilities, so the configuration must acknowledge notion.capabilities\n" +
			"(created with Read content only), or the command exits 3 before any call.\n\n" +
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
		statePath := opts.state
		if statePath == "" {
			statePath = filepath.Join(filepath.Dir(opts.config), "notion-state.json")
		}
		poller, err := notionfeeder.NewPoller(notionfeeder.PollerOptions{
			Client: client, Config: cfg, StatePath: statePath, Once: opts.once, Cycles: opts.cycles,
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
