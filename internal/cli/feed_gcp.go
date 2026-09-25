// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
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
	flags.StringVar(&opts.recordDir, "record", "", "also write every payload and event to this fixture "+
		"directory. Refused on a live run until the sanitised recording path is wired: recording a "+
		"real GCP response would write it to disk unsanitised (FR-137, US7 T153-T155)")
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
	// Recording a LIVE run is refused until the sanitised recording path is wired (FR-137, US7
	// T153–T155).
	//
	// This guard is here rather than beside the live-poller refusal below, and the order matters.
	// Today a live run is refused anyway because the poller is not wired, so `--record` is
	// unreachable and the hazard cannot occur — which is exactly the situation in which it would be
	// forgotten. FR-137 is a property of RECORDING, not of the poller being ready, so the refusal
	// belongs to the flag that asks for a recording.
	//
	// What is missing is not the boundary. `internal/feeders/gcp.SanitisedRecorder` is the boundary,
	// it cannot be constructed without a Sanitiser, and it is the only route from this connector to a
	// Sink. What is missing is that nothing routes `--record` through it: `record.Wrap` writes the
	// payload it was handed. For the synthetic twins that is harmless, because the twins are already
	// clean by construction. For a real GCP response it would write the response to disk, which is
	// the one thing FR-137 forbids — and it forbids it *including during a failed or aborted run*,
	// which is why no commit hook can stand in: by the time a hook runs the bytes have been on a
	// laptop's disk.
	//
	// So whoever wires the live poller meets this refusal and has to wire the sanitiser with it. That
	// is the point of putting it here while it is unreachable.
	if opts.recordDir != "" && opts.replayDir == "" {
		return exitErrorf(ExitUsage, "feed gcp: --record on a live run is refused: the sanitised "+
			"recording path is not wired yet, and recording a live GCP response would write it to "+
			"disk unsanitised, which FR-137 forbids at any point including during a failed or "+
			"aborted run. A recording taken without the sanitiser cannot be cleaned afterwards — "+
			"the bytes were already written. See internal/feeders/gcp/record.go for the boundary "+
			"this must be routed through, and US7 T153-T155 for the task that routes it")
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

	// The live poller lands with the area tasks. Until it does, a live run says so rather than
	// starting, emitting nothing and exiting zero — which is the failure mode that makes an empty
	// graph look like a working connector.
	if opts.replayDir == "" {
		return exitErrorf(ExitUsage, "feed gcp: the live poller is not wired yet; use --dry-run to "+
			"run the read-only gate, or --replay <dir> to feed from a recording. A live run that "+
			"emitted nothing and exited zero would look like a working connector")
	}

	fileSource, err := source.NewFileSource(opts.replayDir)
	if err != nil {
		return exitErrorf(ExitUsage, "feed gcp: %v", err)
	}
	var src feeder.Source = fileSource

	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed gcp: %v", err)
	}
	var em feeder.Emitter = connect

	// --record leaves a directory `fixture verify` can take. It wraps both ends of the feeder rather
	// than only the payloads, because a recording that stored what went in but not what came out
	// would need the feeder re-run to be verified — and then the fixture would test whatever the
	// feeder does today rather than what it did when the recording was made.
	if opts.recordDir != "" {
		payloads := record.Wrap(src, opts.recordDir)
		events := record.Emitter(em, opts.recordDir)
		src, em = payloads, events
		defer writeGCPManifest(cmd, opts, f.Describe(), payloads, events)
	}

	if err := f.Run(ctx, src, em); err != nil {
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

// writeGCPManifest completes a recording so that `--record` leaves a fixture directory.
//
// The description is deliberately unfinished: it says what was recorded and then says what a human
// must still do. A manifest that read as complete would invite committing a fixture with no queries,
// which verifies nothing and looks like coverage.
func writeGCPManifest(cmd *cobra.Command, opts *feedGCPOptions, desc feeder.Description,
	payloads *record.PayloadRecorder, events *record.EventRecorder,
) {
	out := cmd.ErrOrStderr()
	if err := payloads.Err(); err != nil {
		fmt.Fprintf(out, "gcp: recording payloads failed: %v\n", err)
	}
	if err := events.Err(); err != nil {
		fmt.Fprintf(out, "gcp: recording events failed: %v\n", err)
	}
	if err := record.WriteManifest(opts.recordDir, record.Manifest{
		Family: "gcp-topology",
		Description: fmt.Sprintf(
			"%d GCP payloads recorded by `feed gcp` over projects %s. "+
				"Add a queries: list and record the goldens before committing.",
			payloads.Count(), strings.Join(opts.projects, ",")),
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		ExpectRejected: events.Rejections(),
	}); err != nil {
		fmt.Fprintf(out, "gcp: writing the fixture manifest failed: %v\n", err)
		return
	}
	fmt.Fprintf(out, "recorded %d payloads and %d events to %s\n",
		payloads.Count(), events.Accepted(), opts.recordDir)
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

// gcpGate adapts gcpx.Gate to the feeder's Gate interface.
type gcpGate struct {
	projects []string
	assertBy string
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
	return gcpx.Gate(ctx, opts)
}
