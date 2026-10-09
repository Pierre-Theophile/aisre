// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	vendornotice "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed vendor-notice` (T090, 003 FR-002, FR-058, FR-059, FR-132b).
//
// # A separate credential from the GCP feeder, and a separate command
//
// FR-002, and the separate command is how it is enforced rather than promised. The two feeders read
// different systems with different failure modes: a mailbox that stops answering must not consume the
// GCP feeder's budget or advance its extent, and a GCP outage must not make the graph believe no vendor
// announced anything. One command reading both would share a credential, a cadence and a checkpoint, and
// sharing any of the three couples one source's silence to the other's.
//
// # --allowlist has no default
//
// The allowlist *is* the configuration: which vendors, which products, which senders, which pages, and
// what an announcement looks like. There is no default file and no built-in vendor (FR-132b), so a run
// always reads something an operator wrote.
//
// # --dry-run validates the configuration and reads nothing
//
// It loads the file, builds every source, reports what would be polled, and exits. No mailbox is opened,
// no page is fetched, and nothing reaches the graph. It is the invocation for checking a new allowlist
// entry before a campaign, and the one an operator can run without thinking about it.

func init() { addFeedSubcommand(newFeedVendorNoticeCommand) }

type feedVendorNoticeOptions struct {
	configPath string
	orgSlug    string
	dryRun     bool
	recordDir  string
	replayDir  string
	cycles     int
	batchSize  int
}

func newFeedVendorNoticeCommand(global *globalOptions) *cobra.Command {
	opts := &feedVendorNoticeOptions{}
	cmd := &cobra.Command{
		Use:   "vendor-notice",
		Short: "Feed vendor maintenance, deprecation and incident announcements",
		Long: "Reads a shared mailbox, vendor status pages and vendor changelogs, and records every\n" +
			"allowlisted announcement as a CHANGE whose valid time may begin after the instant it was\n" +
			"observed — the project's first announced facts. It writes nothing anywhere, and the\n" +
			"mailbox is read in a way that cannot mark a message as read.\n\n" +
			"--allowlist has no default: which vendors, which products, which senders and what an\n" +
			"announcement looks like are all configuration an operator states (FR-132b).\n\n" +
			"The credential, cadence, budget and checkpoint are separate from `feed gcp`'s (FR-002):\n" +
			"a mailbox that stops answering must not make the graph believe GCP went quiet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedVendorNotice(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.configPath, "allowlist", "",
		"path to the vendor allowlist and source configuration (required; there is deliberately no default)")
	flags.StringVar(&opts.orgSlug, "org", "",
		"organisation slug, overriding the file's; the source id is vendor-notice:<org>")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"validate the configuration, report what would be polled, and read nothing")
	flags.StringVar(&opts.recordDir, "record", "", "also record the run to this fixture directory, "+
		"sanitised in the connector before anything touches disk under the contract table and the corpus "+
		"key in $SRE_AGENT_CORPUS_KEY (FR-137); the recording's events are derived from its sanitised payloads")
	flags.StringVar(&opts.replayDir, "replay", "", "read payloads from this fixture directory instead of the vendors")
	flags.IntVar(&opts.cycles, "cycles", 1, "how many poll cycles to run; 0 runs until interrupted")
	flags.IntVar(&opts.batchSize, "batch", 256, "events per ingest batch")
	return cmd
}

func runFeedVendorNotice(ctx context.Context, global *globalOptions, opts *feedVendorNoticeOptions, cmd *cobra.Command) error {
	if opts.replayDir == "" && opts.configPath == "" {
		return exitErrorf(ExitUsage, "feed vendor-notice: --allowlist is required. Which vendors, "+
			"which products, which senders and what an announcement looks like are all configuration "+
			"an operator states, and no vendor is built in (FR-132b)")
	}
	if opts.recordDir != "" && opts.replayDir != "" {
		return exitErrorf(ExitUsage, "feed vendor-notice: --record and --replay are mutually "+
			"exclusive; re-recording a replay would produce a fixture of a fixture")
	}

	var cfg vendornotice.Config
	if opts.configPath != "" {
		loaded, err := vendornotice.LoadConfig(opts.configPath)
		if err != nil {
			return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
		}
		cfg = loaded
	}
	if opts.orgSlug != "" {
		cfg.Org = opts.orgSlug
	}
	if cfg.Org == "" {
		return exitErrorf(ExitUsage, "feed vendor-notice: --org is required when the configuration "+
			"does not name one; it is the suffix of the source id, and an empty one makes every "+
			"organisation's events one source")
	}

	if opts.dryRun {
		return runFeedVendorNoticeDryRun(cfg, opts, cmd)
	}

	// A live recording is sanitised in the connector before anything touches disk (T188, FR-137): the
	// sanitiser is built first, and without the corpus key the run is refused here, before a
	// directory exists and before a mailbox is opened.
	var san *sanitise.Sanitiser
	if opts.recordDir != "" && opts.replayDir == "" {
		built, err := recordingSanitiser()
		if err != nil {
			return exitErrorf(ExitUsage, "feed vendor-notice: --record on a live run is sanitised in the "+
				"connector before anything touches disk (FR-137), and it cannot start: %v", err)
		}
		san = built
	}

	list, err := cfg.Allowlist()
	if err != nil {
		return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
	}
	feederOpts := vendornotice.Options{
		OrgSlug:          cfg.Org,
		Allowlist:        list,
		ReorderingWindow: cfg.ReorderingWindow,
		// A campaign whose configured mailbox yields nothing fails at campaign start rather than
		// recording silence as evidence (FR-132b). A replay is exempt: a fixture of a quiet cycle is
		// a legitimate fixture.
		RefuseEmptyNoticeStream: opts.replayDir == "",
	}
	f, err := vendornotice.New(feederOpts)
	if err != nil {
		return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
	}

	var src feeder.Source
	var recording *deployrecord.Tee
	if opts.replayDir != "" {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
		}
		src = fileSource
	} else {
		holder := vendornotice.NewHTTPSources(vendornotice.ClientOptions{})
		sources, err := cfg.Sources(list, holder)
		if err != nil {
			return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
		}
		poller, err := vendornotice.NewPoller(vendornotice.PollerOptions{
			Sources: sources, Cadence: cfg.Cadence, Cycles: opts.cycles,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
		}
		src = poller
		if san != nil {
			tee, err := deployrecord.NewTee(poller, "vendornotice", san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
			}
			recording = tee.WithPrepare(vendornotice.PreparePayload)
			src = recording
		}
	}

	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed vendor-notice: %v", err)
	}
	var em feeder.Emitter = connect
	runErr := f.Run(ctx, src, em)
	if recording != nil {
		// The shadow derives the recording's events from its sanitised payloads. Its allowlist is the
		// live one unchanged, because vendor and product are recorded verbatim; an empty stream is
		// not refused, since the live run already said whatever there was to say about it.
		shadowOpts := feederOpts
		shadowOpts.RefuseEmptyNoticeStream = false
		shadow, err := vendornotice.New(shadowOpts)
		if err != nil {
			return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
		}
		events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "vendor-notice", shadow)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "feed vendor-notice: the recording was not completed: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
				recording.Written(), events, opts.recordDir, recording.Dropped())
		}
	}
	if err := runErr; err != nil {
		return exitErrorf(ExitTransport, "feed vendor-notice: %v", err)
	}
	// The per-cycle report is the point of the whole thing (FR-078): "the graph knows about no
	// upcoming vendor change" and "the feeder read nothing" are indistinguishable from the graph
	// alone, and only one of them is good news.
	fmt.Fprintf(cmd.OutOrStdout(), "vendor-notice cycle: %s\n", f.Report().Note())
	return nil
}

func runFeedVendorNoticeDryRun(cfg vendornotice.Config, opts *feedVendorNoticeOptions, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	list, err := cfg.Allowlist()
	if err != nil {
		return exitErrorf(ExitUsage, "feed vendor-notice --dry-run: %v", err)
	}
	holder := vendornotice.NewHTTPSources(vendornotice.ClientOptions{})
	sources, err := cfg.Sources(list, holder)
	if err != nil {
		return exitErrorf(ExitUsage, "feed vendor-notice --dry-run: %v", err)
	}
	fmt.Fprintf(out, "configuration: valid\n")
	fmt.Fprintf(out, "source id: %s%s\n", vendornotice.SourceIDPrefix, cfg.Org)
	fmt.Fprintf(out, "allowlisted vendors: %d\n", list.Len())
	for _, source := range sources {
		fmt.Fprintf(out, "  would poll: %s\n", source.Kind())
	}
	if cfg.Cadence > 0 {
		fmt.Fprintf(out, "cadence: %s\n", cfg.Cadence)
	} else {
		fmt.Fprintf(out, "cadence: one cycle per invocation\n")
	}
	fmt.Fprintf(out, "no mailbox was opened, no page was fetched, no event was emitted, and no "+
		"connection to the graph was opened\n")
	_ = opts
	_ = time.Now
	return nil
}
