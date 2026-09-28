// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/doorbell"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed datadog` (T058; contracts/datadog-feeder.md, contracts/read-only-operations.md §3).
//
// # --dry-run performs the startup gate and nothing else
//
// It proves the keys work and the application key reads and nothing else, prints the capabilities
// and the operations each declares, and exits: no monitor read, no event, no connection to the graph.
//
// # The keys come from the environment
//
// DD_API_KEY and DD_APP_KEY, and DD_APP_KEY_ID so the gate can read the application key's own scopes.
// A key on a command line is in every shell history and every process listing.
//
// # Recording a live run is sanitised in the connector (T080)
//
// A live Datadog response carries service names, hosts and monitor names. --record routes every payload
// through internal/feeders/deployrecord's tee with the Datadog pre-pass: only the sanitiser's output is
// written, and the recording's events are derived from those bytes, never recorded from the live run.

func init() { addFeedSubcommand(newFeedDatadogCommand) }

type feedDatadogOptions struct {
	orgSlug        string
	site           string
	watch          []string
	monitorTags    []string
	capabilities   string
	assertBy       string
	dryRun         bool
	once           bool
	replayDir      string
	recordDir      string
	pollInterval   time.Duration
	doorbellListen string
	doorbellHeader string
	doorbellEnv    string
	batchSize      int
	overrides      []string
	quotaShare     float64
	quotaReserve   int
}

func newFeedDatadogCommand(global *globalOptions) *cobra.Command {
	opts := &feedDatadogOptions{}
	cmd := &cobra.Command{
		Use:   "datadog",
		Short: "Feed Datadog monitors, their transitions and the watched log sources",
		Long: "Reads Datadog monitors with their group states, and asserts a node for every watched\n" +
			"log source. It issues only the published read operations and writes nothing anywhere.\n\n" +
			"Keys come from DD_API_KEY, DD_APP_KEY and DD_APP_KEY_ID. --dry-run performs the startup\n" +
			"read-only gate and nothing else: no monitor read, no event, no connection to the graph.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedDatadog(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.orgSlug, "org", "", "organisation slug; the source id is datadog:<org>")
	flags.StringVar(&opts.site, "site", "", "Datadog site, e.g. datadoghq.eu (required for a live run)")
	flags.StringSliceVar(&opts.watch, "watch", nil, "watched log sources as <env>/<service>; repeatable")
	flags.StringSliceVar(&opts.monitorTags, "monitor-tags", nil,
		"a monitor is in scope only if it carries every one of these key:value tags (FR-025c)")
	flags.StringVar(&opts.capabilities, "capabilities", "logs,monitors,tags",
		"enabled capabilities; apm_topology and changes are specified and not built in this release")
	flags.StringVar(&opts.assertBy, "assert-read-only", "",
		"named individual asserting the key is read-only, for what the gate could not verify; recorded in "+
			"every checkpoint as operator_asserted")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"perform the startup read-only gate and nothing else: no monitor read, no event, no server connection")
	flags.BoolVar(&opts.once, "once", false, "one discovery tick and one monitor poll, then exit")
	flags.StringVar(&opts.replayDir, "replay", "", "read payloads from this fixture directory instead of Datadog")
	flags.StringVar(&opts.recordDir, "record", "", "write payloads and events to this fixture directory; on a "+
		"live run every payload is sanitised in the connector before a byte is written, under the corpus key "+
		"(FR-137)")
	flags.DurationVar(&opts.pollInterval, "poll-interval", ddfeeder.DefaultPollInterval,
		"monitor poll cadence, "+ddfeeder.MinPollInterval.String()+"–"+ddfeeder.MaxPollInterval.String()+
			", recorded on every transition as its sampling interval")
	flags.StringVar(&opts.doorbellListen, "doorbell-listen", "",
		"address for the doorbell, e.g. :8089; a valid ring asks for a poll now and nothing else")
	flags.StringVar(&opts.doorbellHeader, "doorbell-header", "X-Doorbell-Secret", "the doorbell's shared-secret header")
	flags.StringVar(&opts.doorbellEnv, "doorbell-secret-env", "DD_DOORBELL_SECRET",
		"environment variable holding the doorbell's shared secret")
	flags.IntVar(&opts.batchSize, "batch", 256, "events per ingest batch")
	flags.StringSliceVar(&opts.overrides, "version-override", nil,
		"a service's version field as <env>/<service>=<name> (a tag) or =@<name> (an attribute); it is the "+
			"only candidate for that service, recorded as the operator's, and still subject to the share test")
	flags.Float64Var(&opts.quotaShare, "quota-share", 0.5,
		"the fraction of what Datadog says is left in a rate-limit bucket this connector may spend (FR-081)")
	flags.IntVar(&opts.quotaReserve, "quota-reserve", 20,
		"calls left unspent in every bucket whatever the share works out to, for people and their tools (FR-081)")
	return cmd
}

func runFeedDatadog(ctx context.Context, global *globalOptions, opts *feedDatadogOptions, cmd *cobra.Command) error {
	caps, err := ddfeeder.ParseCapabilities(opts.capabilities)
	if err != nil {
		return exitErrorf(ExitUsage, "feed datadog: %v", err)
	}
	var sources []ddfeeder.LogSource
	for _, spec := range opts.watch {
		src, err := ddfeeder.ParseLogSource(spec)
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: --watch: %v", err)
		}
		sources = append(sources, src)
		if src.Env == "" {
			// On the configuration path, before anything runs: a source without an environment is
			// measured, and it is said plainly what it will not do.
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: --watch %s: %s\n", spec, ddfeeder.MissingEnvWarning(src.Service))
		}
	}
	overrides := map[string]string{}
	for _, spec := range opts.overrides {
		source, field, ok := strings.Cut(spec, "=")
		if !ok {
			return exitErrorf(ExitUsage, "feed datadog: --version-override %q is not <env>/<service>=<field>", spec)
		}
		overrides[source] = field
	}
	live := opts.replayDir == ""
	switch {
	case opts.recordDir != "" && !live:
		return exitErrorf(ExitUsage, "feed datadog: --record and --replay are mutually exclusive; "+
			"re-recording a replay would produce a fixture of a fixture")
	case live && opts.site == "":
		return exitErrorf(ExitUsage, "feed datadog: --site is required for a live run; the API host is "+
			"api.<site>, and a key is valid on one site only")
	}

	// A live recording is sanitised in the connector before anything touches disk (FR-137): the
	// sanitiser is built first, from the contract table and the corpus key, and without the key the run
	// is refused here — before a directory exists and before anything is read.
	var san *sanitise.Sanitiser
	if opts.recordDir != "" && live && !opts.dryRun {
		built, err := recordingSanitiser()
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: --record on a live run is sanitised in the connector "+
				"before anything touches disk (FR-137), and it cannot start: %v", err)
		}
		san = built
	}
	if opts.dryRun {
		return runFeedDatadogDryRun(ctx, opts, caps, cmd)
	}
	if opts.orgSlug == "" {
		return exitErrorf(ExitUsage, "feed datadog: --org is required; it is the suffix of the source id")
	}

	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
	feederOpts := ddfeeder.Options{
		OrgSlug: opts.orgSlug, Site: opts.site, Capabilities: caps, MonitorTags: opts.monitorTags,
		PollInterval: opts.pollInterval, LogSources: sources, Log: logger, VersionOverrides: overrides,
	}
	if err := feederOpts.Validate(); err != nil {
		return exitErrorf(ExitUsage, "feed datadog: %v", err)
	}

	var src feeder.Source
	var recording *deployrecord.Tee
	var bell *doorbell.Bell
	var poller *ddfeeder.Poller
	var usage func() string
	if live {
		budget, err := datadogx.NewBudget(feeder.QuotaPolicy{
			Share: opts.quotaShare, Reserve: opts.quotaReserve, StaticAllowance: 30,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: %v", err)
		}
		usage = budget.Report
		client, err := datadogClient(opts.site, caps, budget)
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: %v", err)
		}
		verdict, err := ddfeeder.Gate(ctx, ddfeeder.GateClient{Doer: client}, ddfeeder.GateOptions{
			Capabilities: caps, AppKeyID: os.Getenv("DD_APP_KEY_ID"), AssertedBy: opts.assertBy,
		})
		if err != nil {
			return exitErrorf(ExitAuth, "feed datadog: %v", err)
		}
		feederOpts.OperatorAsserted = verdict.Assertion
		chanSource := source.NewChanSource(0)
		src = chanSource
		poller = &ddfeeder.Poller{
			Pager: client, Tags: opts.monitorTags, Interval: opts.pollInterval, LogSources: sources,
			Capabilities: caps, Push: chanSource.Push,
			Measurer: datadogx.Measurer{Client: client, Cache: datadogx.NewMeasureCache()}, VersionOverrides: overrides, Usage: usage,
		}
		if opts.doorbellListen != "" {
			if bell, err = startDatadogDoorbell(ctx, opts, poller, logger); err != nil {
				return exitErrorf(ExitUsage, "feed datadog: %v", err)
			}
		}
		if san != nil {
			tee, err := deployrecord.NewTee(chanSource, ddfeeder.Kind, san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed datadog: %v", err)
			}
			recording = tee.WithPrepare(ddfeeder.PreparePayload(san))
			src = recording
		}
		go func() {
			defer chanSource.Close()
			var err error
			if opts.once {
				// A one-shot run waits for the resets Datadog states rather than leave a source
				// unmeasured because its bucket allows a call or two per window.
				if err = poller.DiscoverAndWait(ctx); err == nil {
					err = poller.PollOnce(ctx)
				}
			} else {
				err = poller.Run(ctx)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.ErrorContext(ctx, "the poller stopped", "error", err)
			}
		}()
	} else {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: %v", err)
		}
		src = fileSource
	}

	f, err := ddfeeder.New(feederOpts)
	if err != nil {
		return exitErrorf(ExitUsage, "feed datadog: %v", err)
	}
	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(), emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed datadog: %v", err)
	}
	runErr := f.Run(ctx, src, connect)
	if recording != nil {
		// Finished whatever ended the run: the payloads on disk are sanitised, and the events are
		// derived from them by a feeder configured in the recording's vocabulary.
		shadowOpts, err := ddfeeder.PseudonymousOptions(san, feederOpts)
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: %v", err)
		}
		shadow, err := ddfeeder.New(shadowOpts)
		if err != nil {
			return exitErrorf(ExitUsage, "feed datadog: %v", err)
		}
		events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "datadog", shadow)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "feed datadog: the recording was not completed: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
				recording.Written(), events, opts.recordDir, recording.Dropped())
		}
	}
	if usage != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "datadog usage: %s\n", usage())
	}
	if bell != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "doorbell: %s\n", bell.Report())
	}
	if runErr != nil {
		if live && errors.Is(runErr, context.Canceled) {
			return nil
		}
		return exitErrorf(ExitTransport, "feed datadog: %v", runErr)
	}
	return nil
}

// datadogClient builds the live client over the surface of the enabled capabilities, from the keys in
// the environment.
func datadogClient(site string, caps ddfeeder.Capabilities, budget *datadogx.Budget) (*datadogx.Client, error) {
	apiKey, appKey := os.Getenv("DD_API_KEY"), os.Getenv("DD_APP_KEY")
	if apiKey == "" || appKey == "" {
		return nil, fmt.Errorf("DD_API_KEY and DD_APP_KEY must be set; a key on the command line is in every " +
			"shell history and process listing, so there is no flag for one")
	}
	return datadogx.New(datadogx.Options{Site: site, APIKey: apiKey, AppKey: appKey, Surface: ddfeeder.SurfaceFor(caps), Budget: budget})
}

// startDatadogDoorbell serves the doorbell. A valid ring asks the poller for a poll now; the body is
// never read (contract §3.1).
func startDatadogDoorbell(ctx context.Context, opts *feedDatadogOptions, poller *ddfeeder.Poller, logger *slog.Logger) (*doorbell.Bell, error) {
	secret := os.Getenv(opts.doorbellEnv)
	bell, err := doorbell.New(doorbell.Options{Channel: opts.doorbellListen, ChannelLabel: "listen", MinInterval: ddfeeder.MinPollInterval})
	if err != nil {
		return nil, err
	}
	handler, err := doorbell.NewHTTP(doorbell.HTTPOptions{Bell: bell, Header: opts.doorbellHeader, Secret: secret, OnPoll: poller.PollNow})
	if err != nil {
		return nil, fmt.Errorf("%w (the secret is read from $%s)", err, opts.doorbellEnv)
	}
	server := &http.Server{Addr: opts.doorbellListen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.ErrorContext(ctx, "the doorbell stopped", "error", err)
		}
	}()
	return bell, nil
}

func runFeedDatadogDryRun(ctx context.Context, opts *feedDatadogOptions, caps ddfeeder.Capabilities, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "capabilities: %s\n", caps)
	ops := ddfeeder.SurfaceFor(caps).Operations()
	sort.Slice(ops, func(i, j int) bool { return ops[i] < ops[j] })
	for _, op := range ops {
		fmt.Fprintf(out, "  declares %s\n", op)
	}
	fmt.Fprintf(out, "read scopes allowed: %s\n", strings.Join(ddfeeder.AllowedScopes(caps), ", "))
	client, err := datadogClient(opts.site, caps, nil)
	if err != nil {
		return exitErrorf(ExitUsage, "feed datadog --dry-run: %v", err)
	}
	verdict, err := ddfeeder.Gate(ctx, ddfeeder.GateClient{Doer: client}, ddfeeder.GateOptions{
		Capabilities: caps, AppKeyID: os.Getenv("DD_APP_KEY_ID"), AssertedBy: opts.assertBy,
	})
	if err != nil {
		fmt.Fprintf(out, "read-only gate: REFUSED\n  %v\n", err)
		return exitErrorf(ExitAuth, "feed datadog --dry-run: the gate refused, so nothing would be emitted")
	}
	fmt.Fprintf(out, "read-only gate: passed\n%s\n", indent(verdict.String()))
	fmt.Fprintf(out, "no monitor was read, no event was emitted, and no connection to the graph was opened\n")
	return nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
