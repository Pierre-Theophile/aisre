// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	slackfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/slack"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render/slacksink"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/internal/slackx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed slack` (006 US1, US2; contracts/slack-connector.md §3–§4).
//
// # Three credentials, three variables
//
// The reader app's bot token ($SRE_AGENT_SLACK_READER_TOKEN) lists channels and nothing else; the
// graph token (--token / $SRE_AGENT_TOKEN) ingests for source `slack:<team>` and opens the
// investigation; the reporter app's bot token ($SRE_AGENT_REPORT_TOKEN) posts and edits the report.
// None of them stands in for another (constitution VII, ADR-0011).
//
// # Polling is the truth
//
// The channel list is read every --interval. A channel that matches the naming rule is declared at
// the instant Slack says it was created, so a poll that sees it late changes nothing about when
// the incident began — only, past --late-after, that the investigation runs in review mode.

func init() { addFeedSubcommand(newFeedSlackCommand) }

// Environment variables the command reads.
const (
	EnvSlackReaderToken = "SRE_AGENT_SLACK_READER_TOKEN" //nolint:gosec // the name of a variable, not a credential
	EnvSlackIdentityKey = "SRE_AGENT_SLACK_IDENTITY_KEY" //nolint:gosec // the name of a variable, not a credential
)

type feedSlackOptions struct {
	team          string
	interval      time.Duration
	horizon       time.Duration
	lateAfter     time.Duration
	cycles        int
	services      []string
	people        []string
	includeShared bool
	check         bool
	investigate   bool
	deliver       bool
	sinkID        string
	parallel      int
	dsn           string
	recordDir     string
	replayDir     string
	batchSize     int
	// slackAPI overrides Slack's base URL for the reader and the sink. Hidden: it exists for tests
	// against a recorded twin.
	slackAPI string
}

func newFeedSlackCommand(global *globalOptions) *cobra.Command {
	opts := &feedSlackOptions{}
	cmd := &cobra.Command{
		Use:   "slack",
		Short: "Open an investigation when a severity channel is created in Slack",
		Long: "Polls a workspace's public channel list with the reader app's bot token and declares\n" +
			"an incident for every channel whose name matches the naming rule (slack/sev-channel@1:\n" +
			"sev<N>-<YYYY>-<MM>-<DD>[-<slug>], test, drill and gameday excluded), at the instant\n" +
			"Slack says the channel was created. The channel's creator is the declarer.\n\n" +
			"With --investigate (the default on a live run) each declaration opens an investigation;\n" +
			"with --deliver its report is posted into the channel and edited in place as it\n" +
			"progresses, with the reporter app's token in $" + EnvDeliveryToken + ".\n\n" +
			"The reader token is read from $" + EnvSlackReaderToken + " and must hold read scopes only.\n" +
			"An unmapped creator is recorded as a workspace pseudonym under the key in\n" +
			"$" + EnvSlackIdentityKey + " (32 hex-encoded bytes), never as a Slack user id.\n\n" +
			"--check verifies the reader token and the configuration and reads no channel.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedSlack(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.team, "team", "", "workspace (team) id; a live run takes it from the token, and it must agree")
	flags.DurationVar(&opts.interval, "interval", slackfeeder.DefaultPollInterval, "how often the channel list is read")
	flags.DurationVar(&opts.horizon, "horizon", slackfeeder.DefaultHorizon,
		"how old a channel may be when first seen and still be declared")
	flags.DurationVar(&opts.lateAfter, "late-after", slackfeeder.DefaultLateAfter,
		"a channel first seen later than this after its creation is investigated in review mode")
	flags.IntVar(&opts.cycles, "cycles", 0, "how many polls to make; 0 polls until interrupted")
	flags.StringArrayVar(&opts.services, "service", nil,
		"a slug token and the entity it names, as <token>=<ns>=<value> (e.g. checkout=service.name=checkout); "+
			"repeatable. A token not mapped is never a target")
	flags.StringArrayVar(&opts.people, "person", nil,
		"a Slack user id and the deployment's OIDC subject for them, as <user id>=<subject>; repeatable")
	flags.BoolVar(&opts.includeShared, "include-shared", false,
		"also declare channels shared with another organisation (Slack Connect)")
	flags.BoolVar(&opts.check, "check", false, "verify the reader token and the configuration, read no channel, and exit")
	flags.BoolVar(&opts.investigate, "investigate", true, "open an investigation for each declaration (live runs only)")
	flags.BoolVar(&opts.deliver, "deliver", false, "post each investigation's report into its channel, edited in place")
	flags.StringVar(&opts.sinkID, "sink", render.SinkSlack,
		"the report-delivery transport ("+strings.Join(render.PublishedSinks(), ", ")+")")
	flags.IntVar(&opts.parallel, "parallel", 4, "investigations run at once; more wait their turn")
	flags.StringVar(&opts.dsn, "db", "", "PostgreSQL DSN the delivery ledger is written to (default $"+EnvDSN+")")
	flags.StringVar(&opts.recordDir, "record", "", "also record the run to this fixture directory, "+
		"sanitised before anything touches disk under the contract table and the corpus key in "+
		"$SRE_AGENT_CORPUS_KEY; non-matching channel names are blanked and creators dropped")
	flags.StringVar(&opts.replayDir, "replay", "", "read payloads from this fixture directory instead of Slack")
	flags.IntVar(&opts.batchSize, "batch", 64, "events per ingest batch")
	flags.StringVar(&opts.slackAPI, "slack-api", "", "Slack Web API base URL (tests only)")
	_ = flags.MarkHidden("slack-api")
	return cmd
}

func runFeedSlack(ctx context.Context, global *globalOptions, opts *feedSlackOptions, cmd *cobra.Command) error {
	if opts.recordDir != "" && opts.replayDir != "" {
		return exitErrorf(ExitUsage, "feed slack: --record and --replay are mutually exclusive; "+
			"re-recording a replay would produce a fixture of a fixture")
	}
	services, err := parseSlackServices(opts.services)
	if err != nil {
		return exitErrorf(ExitUsage, "feed slack: %v", err)
	}
	people, err := parseSlackPeople(opts.people)
	if err != nil {
		return exitErrorf(ExitUsage, "feed slack: %v", err)
	}
	live := opts.replayDir == ""

	var client *slackx.Client
	team := strings.TrimSpace(opts.team)
	if live {
		client, err = slackx.New(slackx.Options{Token: os.Getenv(EnvSlackReaderToken), BaseURL: opts.slackAPI})
		if err != nil {
			return exitErrorf(ExitUsage, "feed slack: $%s: %v", EnvSlackReaderToken, err)
		}
		id, err := client.AuthTest(ctx)
		if err != nil {
			return exitErrorf(ExitTransport, "feed slack: asking Slack which workspace the reader token "+
				"belongs to: %v", err)
		}
		if _, err := slackx.CheckReadOnly(id); err != nil {
			return exitErrorf(ExitUsage, "feed slack: the reader token is not read-only, and nothing was "+
				"read: %v", err)
		}
		if team != "" && team != id.TeamID {
			return exitErrorf(ExitUsage, "feed slack: --team %s, but the reader token belongs to %s", team, id.TeamID)
		}
		team = id.TeamID
		if opts.check {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "reader token: workspace %s (%s), scopes %s — read-only\n",
				id.TeamID, id.Team, strings.Join(id.Scopes, ", "))
			fmt.Fprintf(out, "source id: %s%s\n", slackfeeder.SourceIDPrefix, team)
			fmt.Fprintf(out, "rule: %s; poll every %s; horizon %s; review mode after %s\n",
				slackfeeder.DefaultRule.Ref(), opts.interval, opts.horizon, opts.lateAfter)
			fmt.Fprintf(out, "services mapped: %d; people mapped: %d\n", len(services), len(people))
			fmt.Fprintf(out, "no channel was read and no event was emitted\n")
			return nil
		}
	} else if team == "" {
		return exitErrorf(ExitUsage, "feed slack --replay: --team is required; it is the source id's suffix "+
			"and a recording carries it pseudonymised")
	}

	var identityKey []byte
	if live {
		raw := strings.TrimSpace(os.Getenv(EnvSlackIdentityKey))
		if raw == "" {
			return exitErrorf(ExitUsage, "feed slack: set $%s to %d hex-encoded bytes (`openssl rand -hex "+
				"32`); an unmapped channel creator is recorded as a workspace pseudonym under it, never as "+
				"a Slack user id (FR-052, ADR-0011 item 3). It is not the corpus key", EnvSlackIdentityKey, sanitise.KeyBytes)
		}
		identityKey, err = hex.DecodeString(raw)
		if err != nil || len(identityKey) != sanitise.KeyBytes {
			return exitErrorf(ExitUsage, "feed slack: $%s must be %d hex-encoded bytes", EnvSlackIdentityKey, sanitise.KeyBytes)
		}
	}

	var san *sanitise.Sanitiser
	if opts.recordDir != "" {
		if san, err = recordingSanitiser(); err != nil {
			return exitErrorf(ExitUsage, "feed slack: --record is sanitised before anything touches disk, "+
				"and it cannot start: %v", err)
		}
	}

	opener := &slackInvestigations{
		cmd: cmd, global: global, opts: opts, sem: make(chan struct{}, max(1, opts.parallel)),
	}
	feederOpts := slackfeeder.Options{
		TeamID: team, Services: services, People: people, IdentityKey: identityKey,
		Horizon: opts.horizon, LateAfter: opts.lateAfter, PollInterval: opts.interval,
		IncludeShared: opts.includeShared,
	}
	if live && opts.investigate {
		feederOpts.OnDeclared = opener.open
	}
	f, err := slackfeeder.New(feederOpts)
	if err != nil {
		return exitErrorf(ExitUsage, "feed slack: %v", err)
	}

	var src feeder.Source
	var recording *deployrecord.Tee
	if live {
		poller, err := slackfeeder.NewPoller(slackfeeder.PollerOptions{
			Client: client, TeamID: team, Interval: opts.interval, Horizon: opts.horizon, Cycles: opts.cycles,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed slack: %v", err)
		}
		src = poller
		if san != nil {
			tee, err := deployrecord.NewTee(poller, slackfeeder.Kind, san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed slack: %v", err)
			}
			recording = tee.WithPrepare(slackfeeder.PreparePayload(slackfeeder.DefaultRule))
			src = recording
		}
	} else {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed slack: %v", err)
		}
		src = fileSource
	}

	connectEm, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed slack: %v", err)
	}
	runErr := f.Run(ctx, src, connectEm)
	opener.wait()

	if recording != nil {
		// The shadow derives the recording's events from its sanitised payloads, in the recording's
		// vocabulary: the workspace pseudonymised, nobody mapped, no identity key — the creators are
		// not in the recording to map.
		pseudoTeam, err := san.Identifier(sanitise.KindWorkspace, team)
		if err != nil {
			return exitErrorf(ExitUsage, "feed slack: %v", err)
		}
		shadow, err := slackfeeder.New(slackfeeder.Options{
			TeamID: pseudoTeam, Services: services, Horizon: opts.horizon, LateAfter: opts.lateAfter,
			PollInterval: opts.interval, IncludeShared: opts.includeShared,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed slack: %v", err)
		}
		events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "slack", shadow)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "feed slack: the recording was not completed: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
				recording.Written(), events, opts.recordDir, recording.Dropped())
		}
	}
	if runErr != nil && ctx.Err() == nil {
		return exitErrorf(ExitTransport, "feed slack: %v", runErr)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "slack intake: %s\n", f.Report().Note())
	return nil
}

// slackInvestigations opens one investigation per declaration and, with --deliver, keeps its report
// in the channel up to date. It runs beside the poll loop, never inside it: an investigation takes
// minutes and the next severity channel must not wait for it.
type slackInvestigations struct {
	cmd    *cobra.Command
	global *globalOptions
	opts   *feedSlackOptions
	sem    chan struct{}
	wg     sync.WaitGroup
}

func (s *slackInvestigations) wait() { s.wg.Wait() }

func (s *slackInvestigations) open(ctx context.Context, d slackfeeder.Declared) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-s.sem }()
		if err := s.run(ctx, d); err != nil {
			fmt.Fprintf(s.cmd.ErrOrStderr(), "feed slack: %s: %v\n", d.OriginRef, err)
		}
	}()
}

func (s *slackInvestigations) run(ctx context.Context, d slackfeeder.Declared) error {
	client, err := newInvestigationClient(s.global)
	if err != nil {
		return err
	}
	stream, err := client.Declare(ctx, connect.NewRequest(SlackDeclareRequest(d)))
	if err != nil {
		return remoteError("declare", err)
	}
	defer func() { _ = stream.Close() }()

	var sink render.Sink
	if s.opts.deliver {
		if s.opts.sinkID == render.SinkSlack && s.opts.slackAPI != "" {
			sink = slacksink.New(slacksink.Options{BaseURL: s.opts.slackAPI})
		} else if sink, err = newDeliverySink(s.opts.sinkID); err != nil {
			return err
		}
	}
	var last *investigationv1.Investigation
	var previous string
	deliver := func(inv *investigationv1.Investigation) {
		body, err := renderReportBody(inv)
		if err != nil {
			fmt.Fprintf(s.cmd.ErrOrStderr(), "feed slack: rendering %s: %v\n", inv.GetInvestigationId(), err)
			return
		}
		result := deliverReportWith(ctx, sink, inv, body, previous)
		if result.Outcome == render.OutcomeDelivered {
			previous = result.ExternalMessageRef
		}
		note := recordDeliveryOutcome(s.cmd, s.opts.dsn, inv, result)
		fmt.Fprintf(s.cmd.ErrOrStderr(), "feed slack: %s report %s (%s) %s\n",
			inv.GetInvestigationId(), result.Outcome, result.FailureDetail, note)
	}
	for stream.Receive() {
		last = stream.Msg()
		// The first state goes to the channel at once, so the people in it see that an
		// investigation is running; the final one edits that same message.
		if sink != nil && previous == "" {
			deliver(last)
		}
	}
	if err := stream.Err(); err != nil {
		return remoteError("declare", err)
	}
	if sink != nil && last != nil {
		deliver(last)
	}
	if last != nil {
		fmt.Fprintf(s.cmd.ErrOrStderr(), "feed slack: %s opened %s (%s)\n",
			d.OriginRef, last.GetInvestigationId(), last.GetLifecycle())
	}
	return nil
}

// SlackDeclareRequest is the Declare call for one declaration (contracts/slack-connector.md §3): the
// same idempotency key the feeder's event carries, so the event and the investigation are one
// incident however many times either is sent.
func SlackDeclareRequest(d slackfeeder.Declared) *investigationv1.DeclareRequest {
	targets := make([]*investigationv1.TargetRef, 0, len(d.Targets))
	named := make([]*graphv1.Ref, 0, len(d.Targets))
	for _, ref := range d.Targets {
		targets = append(targets, &investigationv1.TargetRef{
			Ref: ref, Provenance: investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION, RuleId: d.RuleRef,
		})
		named = append(named, ref)
	}
	profile := "page"
	if d.ReviewMode {
		profile = "review"
	}
	return &investigationv1.DeclareRequest{
		Declaration: &investigationv1.Symptom{
			OriginSystem:      d.SourceID,
			OriginRef:         d.OriginRef,
			Transport:         intake.TransportHumanDeclared,
			Statement:         d.Title,
			FiredAt:           timestamppb.New(d.DeclaredAt),
			Origin:            investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
			Severity:          d.Severity,
			Title:             d.Title,
			DeclaringIdentity: d.Declarer,
			TargetRefs:        targets,
			NamedIdentifiers:  named,
			IdempotencyKey:    d.IdempotencyKey,
		},
		Profile: profile,
	}
}

// parseSlackServices reads `<token>=<ns>=<value>` entries.
func parseSlackServices(raw []string) (map[string]*graphv1.Ref, error) {
	out := map[string]*graphv1.Ref{}
	for _, entry := range raw {
		token, rest, ok := strings.Cut(strings.TrimSpace(entry), "=")
		token = strings.ToLower(strings.TrimSpace(token))
		if !ok || token == "" {
			return nil, fmt.Errorf("--service %q: write it as <token>=<ns>=<value>", entry)
		}
		ref, err := parseFocusRef(rest)
		if err != nil {
			return nil, fmt.Errorf("--service %q: %w", entry, err)
		}
		if _, dup := out[token]; dup {
			return nil, fmt.Errorf("--service: token %q is mapped twice", token)
		}
		out[token] = ref.Proto()
	}
	return out, nil
}

// parseSlackPeople reads `<user id>=<subject>` entries.
func parseSlackPeople(raw []string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range raw {
		user, subject, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || strings.TrimSpace(user) == "" || strings.TrimSpace(subject) == "" {
			return nil, fmt.Errorf("--person %q: write it as <slack user id>=<oidc subject>", entry)
		}
		out[strings.TrimSpace(user)] = strings.TrimSpace(subject)
	}
	return out, nil
}
