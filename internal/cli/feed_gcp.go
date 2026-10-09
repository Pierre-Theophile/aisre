// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed gcp` (T067, 003 FR-004, FR-009, FR-131; contracts/gcp-feeder.md).
//
// # --dry-run performs the startup gate and nothing else
//
// That is the whole of its contract, and it is the most useful thing this command does before a
// campaign starts. An operator who has just been granted a service account wants one question
// answered — *is this credential actually read-only, and does it work?* — without emitting a single
// event into the graph and without spending a single quota-bearing area call.
//
// So `--dry-run` runs `internal/gcpx`'s three layers, prints what each one found, prints what it
// could **not** verify, and exits. It does not construct an area client, does not read Cloud Run, and
// does not open a connection to the graph server. A dry run that also read "just a little" would make
// the one command an operator trusts to be harmless into a command they have to reason about.
//
// # --projects has no default, and that is FR-131
//
// No code and no checked-in default may assume a project name. An empty `--projects` is a usage error
// with a message that says why, rather than a run that reads nothing and reports success.

func init() { addFeedSubcommand(newFeedGCPCommand) }

type feedGCPOptions struct {
	orgSlug   string
	projects  []string
	regions   []string
	dryRun    bool
	recordDir string
	replayDir string
	window    time.Duration
	horizon   time.Duration
	assertBy  string
	batchSize int
	// alertIncidents is the declared capability for `projects.alerts.list`. Off by default,
	// because the surface is Public Preview and its only Go binding is a maintenance-mode client.
	alertIncidents bool
	// alertPollInterval is the cadence transitions are read at, recorded on every one of them as
	// the sampling interval.
	alertPollInterval time.Duration
	// handoff names an alert — a policy resource name, or one with `#<group>` appended — whose
	// handoff is printed after the run (FR-053).
	handoff string
	// interval, cycles and auditLookback pace the live poller (T185).
	interval      time.Duration
	cycles        int
	auditLookback time.Duration
}

func newFeedGCPCommand(global *globalOptions) *cobra.Command {
	opts := &feedGCPOptions{}
	cmd := &cobra.Command{
		Use:   "gcp",
		Short: "Feed Cloud Run, Cloud SQL, audit logs and alert transitions",
		Long: "Reads Cloud Run services and revisions, Cloud SQL instances and settings, the Cloud\n" +
			"Audit Logs admin-activity stream, Cloud Monitoring alert policies and their\n" +
			"transitions, the GKE cluster's metadata, and — where the budget allows — load\n" +
			"balancers and Cloud DNS. It writes nothing anywhere.\n\n" +
			"--dry-run performs the startup read-only gate and nothing else: no area call, no event,\n" +
			"no connection to the graph. It is the one invocation an operator can run against a\n" +
			"production project without thinking about it.\n\n" +
			"--projects has no default: no code and no checked-in default may assume a project name\n" +
			"(FR-131), so the scope is always something an operator stated.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedGCP(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.orgSlug, "org", "", "organisation slug; the source id is gcp:<org>")
	flags.StringSliceVar(&opts.projects, "projects", nil,
		"projects in scope (required; there is deliberately no default — FR-131)")
	flags.StringSliceVar(&opts.regions, "regions", nil, "regions in scope (required)")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"perform the startup read-only gate and nothing else: no area call, no event, no server connection")
	flags.StringVar(&opts.recordDir, "record", "", "also record the run to this fixture directory, "+
		"sanitised in the connector before anything touches disk under the contract table and the corpus "+
		"key in $SRE_AGENT_CORPUS_KEY (FR-137); the recording's events are derived from its sanitised payloads")
	flags.StringVar(&opts.replayDir, "replay", "", "read payloads from this fixture directory instead of GCP")
	flags.DurationVar(&opts.window, "reordering-window", gcpfeeder.DefaultReorderingWindow,
		"declared reordering window; measured, not assumed (contracts/gcp-feeder.md §5.2)")
	flags.DurationVar(&opts.horizon, "revision-horizon", 30*24*time.Hour,
		"how far back to read revisions; a query reaching past it is told so rather than answered (FR-024)")
	flags.StringVar(&opts.assertBy, "assert-read-only-by", "",
		"named individual asserting the credential is read-only (layer 3 of the gate)")
	flags.IntVar(&opts.batchSize, "batch", 256, "events per ingest batch")
	flags.BoolVar(&opts.alertIncidents, "alert-incidents", false,
		"read alert TRANSITIONS from projects.alerts.list. Off by default: the surface is Public "+
			"Preview and its only Go binding is a maintenance-mode client, so it is opted into "+
			"deliberately. With it off, ALERT nodes still come from the GA policy read and "+
			"monitor_state answers NO_DATA naming the absent source")
	flags.DurationVar(&opts.alertPollInterval, "alert-poll-interval", gcpfeeder.DefaultAlertPollInterval,
		"cadence alert transitions are read at, recorded on each one as its sampling interval; the "+
			"published ceiling is "+gcpfeeder.MaxAlertPollInterval.String()+" and is not configurable past it (SC-004)")
	flags.StringVar(&opts.handoff, "handoff", "",
		"after the run, print the investigation handoff for this alert: a policy resource name, or "+
			"one with #<group> appended for a grouped policy. It carries the alert, what it "+
			"watches, the transition instant to reason from and the pointers to execute — and no "+
			"telemetry (FR-053)")
	flags.DurationVar(&opts.interval, "interval", gcpfeeder.DefaultPollInterval,
		"time between live poll cycles. With --alert-incidents it is also the alert cadence, so it may "+
			"not exceed "+gcpfeeder.MaxAlertPollInterval.String()+" (SC-004)")
	flags.IntVar(&opts.cycles, "cycles", 0, "live poll cycles to run; 0 runs until interrupted")
	flags.DurationVar(&opts.auditLookback, "audit-lookback", gcpfeeder.DefaultAuditLookback,
		"how far back the first cycle reads the admin-activity audit stream")
	return cmd
}

func runFeedGCP(ctx context.Context, global *globalOptions, opts *feedGCPOptions, cmd *cobra.Command) error {
	if len(opts.projects) == 0 {
		return exitErrorf(ExitUsage, "feed gcp: --projects is required. The scope is operator "+
			"configuration and no code or checked-in default may assume a project name (FR-131), so "+
			"there is no default to fall back on")
	}
	if opts.replayDir == "" && len(opts.regions) == 0 {
		return exitErrorf(ExitUsage, "feed gcp: --regions is required for a live run; a region scope "+
			"of none reads nothing and would report success")
	}
	if opts.recordDir != "" && opts.replayDir != "" {
		return exitErrorf(ExitUsage, "feed gcp: --record and --replay are mutually exclusive; "+
			"re-recording a replay would produce a fixture of a fixture")
	}
	// A live recording is sanitised in the connector before anything touches disk (T187, FR-137): the
	// sanitiser is built first, from the contract table and the corpus key, and without the key the run
	// is refused here — before a directory exists and before anything is read. A failed or aborted run
	// therefore leaves sanitised bytes or none.
	var san *sanitise.Sanitiser
	if opts.recordDir != "" && opts.replayDir == "" && !opts.dryRun {
		built, err := recordingSanitiser()
		if err != nil {
			return exitErrorf(ExitUsage, "feed gcp: --record on a live run is sanitised in the connector "+
				"before anything touches disk (FR-137), and it cannot start: %v", err)
		}
		san = built
	}

	// --dry-run is the gate and nothing else. It returns before anything constructs an area client
	// or an emitter, which is what makes the claim in --help true rather than aspirational.
	if opts.dryRun {
		return runFeedGCPDryRun(ctx, opts, cmd)
	}

	if opts.orgSlug == "" {
		return exitErrorf(ExitUsage, "feed gcp: --org is required; it is the suffix of the source id, "+
			"and an empty one makes every organisation's events one source")
	}
	if opts.replayDir == "" && opts.alertIncidents {
		// The live poller reads every area at one cadence, so with transitions on, the poll interval IS
		// the sampling interval recorded on each one, and SC-004 bounds it.
		if opts.interval > gcpfeeder.MaxAlertPollInterval {
			return exitErrorf(ExitUsage, "feed gcp: --alert-incidents reads transitions at the poll "+
				"interval, and %s is past the published %s ceiling (SC-004); pass --interval %s or less",
				opts.interval, gcpfeeder.MaxAlertPollInterval, gcpfeeder.MaxAlertPollInterval)
		}
		opts.alertPollInterval = opts.interval
	}

	feederOpts := gcpfeeder.Options{
		OrgSlug:          opts.orgSlug,
		Scope:            gcpfeeder.Scope{Projects: opts.projects, Regions: opts.regions},
		ReorderingWindow: opts.window,
		Labels:           gcpfeeder.DefaultLabelPolicy(),
		Horizon: gcpfeeder.Horizon{
			Earliest: time.Now().UTC().Add(-opts.horizon),
			Reason:   gcpfeeder.HorizonConfigured,
		},
		IncidentsAPIEnabled: opts.alertIncidents,
		AlertPollInterval:   opts.alertPollInterval,
	}
	f, err := gcpfeeder.New(feederOpts)
	if err != nil {
		return exitErrorf(ExitUsage, "feed gcp: %v", err)
	}

	if opts.replayDir == "" {
		// A live run needs the gate, and the gate is what produces the credential every area
		// client is built from. There is no ordering in which a read happens first.
		f.Gate = &gcpGate{projects: opts.projects, assertBy: opts.assertBy}
	}

	var src feeder.Source
	var usage *gcpx.Usage
	var recording *deployrecord.Tee
	if opts.replayDir == "" {
		poller, live, err := newLiveGCPPoller(opts, f.Gate.(*gcpGate),
			slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)))
		if err != nil {
			return exitErrorf(ExitUsage, "feed gcp: %v", err)
		}
		src, usage = poller, live
		if san != nil {
			tee, err := deployrecord.NewTee(poller, "gcp", san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed gcp: %v", err)
			}
			recording = tee.WithPrepare(gcpfeeder.PreparePayload)
			src = recording
		}
	} else {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed gcp: %v", err)
		}
		src = fileSource
	}

	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed gcp: %v", err)
	}
	var em feeder.Emitter = connect

	runErr := f.Run(ctx, src, em)
	if usage != nil {
		reportGCPRequests(cmd, usage)
	}
	if recording != nil {
		finishGCPRecording(ctx, cmd, opts, san, recording)
	}
	if err := runErr; err != nil {
		// An operator stopping a live run is how a live run ends, not a failure.
		if opts.replayDir == "" && errors.Is(err, context.Canceled) {
			return nil
		}
		return exitErrorf(ExitTransport, "feed gcp: %v", err)
	}
	if opts.handoff != "" {
		return renderAlertHandoff(cmd, global, f, opts.handoff)
	}
	return nil
}

// renderAlertHandoff prints what an investigation of one alert starts from (T124, FR-053).
//
// An alert this run saw no incident for is reported as exactly that rather than as an empty handoff:
// "this alert has not fired in what I read" and "this alert fired and I can tell you nothing about
// it" are different sentences, and printing an empty structure for the first would make them look
// alike.
func renderAlertHandoff(cmd *cobra.Command, global *globalOptions, f *gcpfeeder.Feeder, monitor string) error {
	handoff, ok := f.HandoffFor(monitor)
	if !ok {
		return exitErrorf(ExitUsage,
			"feed gcp: no incident for alert %q in what was read. That is not an empty handoff: the "+
				"alert may not have fired in this window, the incident capability may be off "+
				"(--alert-incidents), or the reference may name a group this policy does not have",
			monitor)
	}
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	watches := make([]string, 0, len(handoff.Watches))
	for _, ref := range handoff.Watches {
		watches = append(watches, ref.GetNamespace()+"="+ref.GetValue())
	}
	selectors := make([]string, 0, len(handoff.Pointers))
	for _, pointer := range handoff.Pointers {
		selectors = append(selectors, pointer.GetSelector())
	}
	if p.json() {
		return p.writeJSON(map[string]any{
			"monitor":       handoff.Monitor.GetNamespace() + "=" + handoff.Monitor.GetValue(),
			"policy":        handoff.Policy.GetNamespace() + "=" + handoff.Policy.GetValue(),
			"group_key":     handoff.GroupKey,
			"watches":       watches,
			"reference_at":  handoff.ReferenceAt.UTC().Format(time.RFC3339),
			"state":         handoff.State,
			"severity":      handoff.Severity,
			"title":         handoff.Title,
			"selectors":     selectors,
			"origin_ref":    handoff.OriginRef,
			"sampled":       handoff.Sampled,
			"sampled_every": handoff.SampledInterval.String(),
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", handoff.Monitor.GetValue())
	fmt.Fprintf(&b, "  policy         %s\n", handoff.Policy.GetValue())
	if handoff.GroupKey != "" {
		fmt.Fprintf(&b, "  group          %s\n", handoff.GroupKey)
	}
	fmt.Fprintf(&b, "  reference_at   %s (%s)\n", handoff.ReferenceAt.UTC().Format(time.RFC3339), handoff.State)
	if handoff.Severity != "" {
		fmt.Fprintf(&b, "  severity       %s\n", handoff.Severity)
	}
	fmt.Fprintf(&b, "  sampled        every %s\n", handoff.SampledInterval)
	for _, watch := range watches {
		fmt.Fprintf(&b, "  watches        %s\n", watch)
	}
	for _, selector := range selectors {
		fmt.Fprintf(&b, "  execute        %s\n", selector)
	}
	b.WriteString("\nno telemetry: the values that made this alert fire are a digest's business, " +
		"behind the algebra, where they are bounded and sanitised (FR-053).\n")
	return p.writeRaw(b.String())
}

func runFeedGCPDryRun(ctx context.Context, opts *feedGCPOptions, cmd *cobra.Command) error {
	gateOpts := gcpx.GateOptions{Projects: opts.projects}
	if opts.assertBy != "" {
		// Acknowledged must cover the whole unverifiable set: a partial acknowledgement is refused,
		// because an assertion that skipped the item that mattered looks like coverage (gcpx/assert.go).
		gateOpts.Assertion = &gcpx.Assertion{
			By:           opts.assertBy,
			At:           time.Now().UTC().Format(time.RFC3339),
			Acknowledged: gcpx.UnverifiableAreas(),
		}
	}
	creds, err := gcpx.Gate(ctx, gateOpts)
	out := cmd.OutOrStdout()
	if err != nil {
		fmt.Fprintf(out, "read-only gate: REFUSED\n  %v\n", err)
		return exitErrorf(ExitAuth, "feed gcp --dry-run: the gate refused, so nothing would be emitted")
	}
	fmt.Fprintf(out, "read-only gate: passed for projects %s\n", strings.Join(opts.projects, ", "))
	// What could not be verified is printed with what could. A gate that reported only its
	// successes would read as a proof, and it is a tripwire: IAM inherits downward only, so a grant
	// above the project is invisible here (contracts/gcp-feeder.md §2).
	for _, area := range gcpx.UnverifiableAreas() {
		fmt.Fprintf(out, "  not verifiable: %s\n", area)
	}
	_ = creds
	fmt.Fprintf(out, "no area call was made, no event was emitted, and no connection to the graph was opened\n")
	return nil
}

// finishGCPRecording completes a live recording whatever ended the run: the payloads already on disk
// are sanitised, and the events are derived from them by a second feeder whose scope names the
// projects the way the payloads now do (deployrecord). Projects take the label shape the table gives
// them; regions are kept, as the table keeps them.
func finishGCPRecording(ctx context.Context, cmd *cobra.Command, opts *feedGCPOptions, san *sanitise.Sanitiser, recording *deployrecord.Tee) {
	errOut := cmd.ErrOrStderr()
	projects := make([]string, 0, len(opts.projects))
	for _, project := range opts.projects {
		pseudonym, err := san.LabelIdentifier(sanitise.KindProject, project)
		if err != nil {
			fmt.Fprintf(errOut, "feed gcp: the recording was not completed: %v\n", err)
			return
		}
		projects = append(projects, pseudonym)
	}
	shadow, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug:          opts.orgSlug,
		Scope:            gcpfeeder.Scope{Projects: projects, Regions: opts.regions},
		ReorderingWindow: opts.window,
		Labels:           gcpfeeder.DefaultLabelPolicy(),
		Horizon: gcpfeeder.Horizon{
			Earliest: time.Now().UTC().Add(-opts.horizon),
			Reason:   gcpfeeder.HorizonConfigured,
		},
		AlertPollInterval: opts.alertPollInterval,
	})
	if err != nil {
		fmt.Fprintf(errOut, "feed gcp: the recording was not completed: %v\n", err)
		return
	}
	events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "gcp-topology", shadow)
	if err != nil {
		fmt.Fprintf(errOut, "feed gcp: the recording was not completed: %v\n", err)
		return
	}
	fmt.Fprintf(errOut, "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
		recording.Written(), events, opts.recordDir, recording.Dropped())
}

// gcpGate adapts gcpx.Gate to the feeder's Gate interface, and keeps the credential it proved: the
// live poller's area clients are built from that one credential and no other, after the gate passed.
type gcpGate struct {
	projects []string
	assertBy string
	proved   *gcpx.Credential
}

func (g *gcpGate) Prove(ctx context.Context) (*gcpx.Credential, error) {
	opts := gcpx.GateOptions{Projects: g.projects}
	if g.assertBy != "" {
		opts.Assertion = &gcpx.Assertion{
			By:           g.assertBy,
			At:           time.Now().UTC().Format(time.RFC3339),
			Acknowledged: gcpx.UnverifiableAreas(),
		}
	}
	creds, err := gcpx.Gate(ctx, opts)
	if err != nil {
		return nil, err
	}
	g.proved = creds
	return creds, nil
}

// newLiveGCPPoller builds the live source (T185). The transport is connected on the poller's first
// read, from the credential the gate proved: Feeder.Run proves the gate before it asks the source for
// anything, so a client built here before the gate ran is not a path that exists.
func newLiveGCPPoller(opts *feedGCPOptions, gate *gcpGate, logger *slog.Logger) (*gcpfeeder.Poller, *gcpx.Usage, error) {
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget(gcpx.PublishedLimits(), usage)
	connect := func(ctx context.Context) (*gcpfeeder.Transport, error) {
		if gate.proved == nil {
			return nil, errors.New("the read-only gate has not passed, so there is no credential to read with")
		}
		transport, err := gcpfeeder.NewLiveTransport(ctx, gate.proved, budget)
		if err != nil {
			return nil, err
		}
		if opts.alertIncidents {
			incidents, err := gcpfeeder.NewLiveAlertIncidentReader(ctx, gate.proved, budget)
			if err != nil {
				return nil, err
			}
			transport.AlertIncidents = incidents
		}
		return transport, nil
	}
	poller, err := gcpfeeder.NewPoller(gcpfeeder.PollerOptions{
		Connect:       connect,
		Scope:         gcpfeeder.Scope{Projects: opts.projects, Regions: opts.regions},
		Interval:      opts.interval,
		Cycles:        opts.cycles,
		AuditLookback: opts.auditLookback,
		AuditOverlap:  opts.window,
		Logger:        logger,
	})
	if err != nil {
		return nil, nil, err
	}
	return poller, usage, nil
}

// reportGCPRequests prints what the run issued, from its own request log (SC-020): every operation is
// on the published read-only list, and the only state change permitted anywhere is the doorbell's.
func reportGCPRequests(cmd *cobra.Command, usage *gcpx.Usage) {
	out := cmd.ErrOrStderr()
	rows := usage.Requests()
	counts := map[string]int{}
	total := 0
	for _, row := range rows {
		counts[string(row.Operation)] += row.Calls
		total += row.Calls
	}
	ops := make([]string, 0, len(counts))
	for op := range counts {
		ops = append(ops, op)
	}
	slices.Sort(ops)
	fmt.Fprintf(out, "gcp request log: %d call(s) issued, %d blocked, %d state change(s); read-only honoured: %t\n",
		total, len(usage.BlockedRequests()), len(usage.StateChanges()), usage.ReadOnlyHonoured())
	for _, op := range ops {
		fmt.Fprintf(out, "  %-40s %d\n", op, counts[op])
	}
}
