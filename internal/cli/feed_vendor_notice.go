// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	vendornotice "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
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
	flags.StringVar(&opts.recordDir, "record", "", "also write every payload and event to this fixture directory")
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

	list, err := cfg.Allowlist()
	if err != nil {
		return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
	}
	f, err := vendornotice.New(vendornotice.Options{
		OrgSlug:          cfg.Org,
		Allowlist:        list,
		ReorderingWindow: cfg.ReorderingWindow,
		// A campaign whose configured mailbox yields nothing fails at campaign start rather than
		// recording silence as evidence (FR-132b). A replay is exempt: a fixture of a quiet cycle is
		// a legitimate fixture.
		RefuseEmptyNoticeStream: opts.replayDir == "",
	})
	if err != nil {
		return exitErrorf(ExitUsage, "feed vendor-notice: %v", err)
	}

	var src feeder.Source
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
	}

	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed vendor-notice: %v", err)
	}
	var em feeder.Emitter = connect

	if opts.recordDir != "" {
		payloads := record.Wrap(src, opts.recordDir)
		events := record.Emitter(em, opts.recordDir)
		src, em = payloads, events
		defer writeVendorNoticeManifest(cmd, opts, f.Describe(), payloads, events)
	}

	if err := f.Run(ctx, src, em); err != nil {
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

// writeVendorNoticeManifest completes a recording so that `--record` leaves a fixture directory.
func writeVendorNoticeManifest(cmd *cobra.Command, opts *feedVendorNoticeOptions, desc feeder.Description,
	payloads *record.PayloadRecorder, events *record.EventRecorder,
) {
	out := cmd.ErrOrStderr()
	if err := payloads.Err(); err != nil {
		fmt.Fprintf(out, "vendor-notice: recording payloads failed: %v\n", err)
	}
	if err := events.Err(); err != nil {
		fmt.Fprintf(out, "vendor-notice: recording events failed: %v\n", err)
	}
	if err := record.WriteManifest(opts.recordDir, record.Manifest{
		Family: "vendor-notice",
		Description: fmt.Sprintf("%d vendor-notice payloads recorded by `feed vendor-notice`. "+
			"Add a queries: list and record the goldens before committing.", payloads.Count()),
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		ExpectRejected: events.Rejections(),
	}); err != nil {
		fmt.Fprintf(out, "vendor-notice: writing the fixture manifest failed: %v\n", err)
		return
	}
	fmt.Fprintf(out, "recorded %d payloads and %d events to %s\n",
		payloads.Count(), events.Accepted(), opts.recordDir)
}
