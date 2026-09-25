// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed otel` (contracts/cli.md, FR-042, FR-044).
//
// One command, three modes, one code path through the feeder:
//
//	live     receivers → ChanSource → Feeder → ConnectEmitter
//	record   the same, with the payloads and the events teed into a fixture directory
//	replay   FileSource → Feeder → ConnectEmitter, or → stdout with --dry-run
//
// The feeder cannot tell which of them it is in, which is what makes "the recorded mode is the
// test" true rather than aspirational (FR-044). What the command adds around it is
// operational: where to listen, where to send, and what to do with a refusal.

func init() { addFeedSubcommand(newFeedOtelCommand) }

// feedSinkBuffer is how many exports may wait for the feeder before the receiver starts
// dropping them. At the demo's rate that is minutes of slack; the receiver never blocks an
// exporter for longer than its push timeout whatever this is set to.
const feedSinkBuffer = 1024

// feedHeartbeat is how often a live run advances its clock while nothing is arriving, so that
// a window with no traffic still closes and its checkpoint still says the feeder was watching.
const feedHeartbeat = 15 * time.Second

type feedOtelOptions struct {
	sourceID     string
	listenGRPC   string
	listenHTTP   string
	window       time.Duration
	retractAfter int
	idFormat     string
	recordDir    string
	replayDir    string
	dryRun       bool
	batchSize    int
}

func newFeedOtelCommand(global *globalOptions) *cobra.Command {
	opts := &feedOtelOptions{}
	cmd := &cobra.Command{
		Use:   "otel",
		Short: "Receive OTLP traces and emit service topology",
		Long: "Derives the structural half of what spans say — which services exist, which of them\n" +
			"call which and how hard, which third parties they depend on, and when a version\n" +
			"changed — and emits it as typed events (FR-042).\n\n" +
			"No span, no metric sample and no log line is ever stored: a node carries pointers to\n" +
			"where its telemetry lives (constitution IV). Traffic is a coarse weight class per\n" +
			"window, never a rate.\n\n" +
			"Exit codes: 0 ok, 1 usage, 2 transport, 3 auth, 5 an event the graph refused.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedOtel(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.sourceID, "source-id", "",
		"this feeder's identity, e.g. otel:prod-eu1 (required; the token must be scoped to it)")
	flags.StringVar(&opts.listenGRPC, "listen-grpc", otelfeeder.DefaultGRPCAddr,
		"OTLP/gRPC listen address, or empty to disable")
	flags.StringVar(&opts.listenHTTP, "listen-http", otelfeeder.DefaultHTTPAddr,
		"OTLP/HTTP listen address (POST "+otelfeeder.TracesPath+", protobuf or JSON), or empty to disable")
	flags.DurationVar(&opts.window, "window", otelfeeder.DefaultWindow,
		"aggregation window, aligned to the wall clock")
	flags.IntVar(&opts.retractAfter, "retract-after", otelfeeder.DefaultRetractAfter,
		"retract a calls edge after this many windows without observing it (negative never retracts)")
	flags.StringVar(&opts.idFormat, "id-format", string(otelfeeder.IDFormatFull),
		"window spelling in event ids: full (@w20260901T1300Z) or compat (@w1300, for fixtures written that way)")
	flags.StringVar(&opts.recordDir, "record", "",
		"record raw payloads and emitted events into this fixture directory")
	flags.StringVar(&opts.replayDir, "replay", "",
		"replay a recorded fixture directory instead of listening")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"print the events instead of sending them; exit 5 if any would be refused")
	flags.IntVar(&opts.batchSize, "batch-size", emit.DefaultBatchSize,
		"how many events are sent to the graph per call")
	return cmd
}

func runFeedOtel(cmd *cobra.Command, global *globalOptions, opts *feedOtelOptions) error {
	if strings.TrimSpace(opts.sourceID) == "" {
		return exitErrorf(ExitUsage, "--source-id is required: it is the identity every event is stamped with")
	}
	if opts.window <= 0 {
		return exitErrorf(ExitUsage, "--window %s: must be positive", opts.window)
	}
	format := otelfeeder.IDFormat(opts.idFormat)
	if !format.Valid() {
		return exitErrorf(ExitUsage, "--id-format %q: want %s or %s",
			opts.idFormat, otelfeeder.IDFormatFull, otelfeeder.IDFormatCompat)
	}
	live := opts.replayDir == ""
	if live && opts.listenGRPC == "" && opts.listenHTTP == "" {
		return exitErrorf(ExitUsage,
			"nothing to do: give --replay a recording, or a --listen-grpc or --listen-http address")
	}

	level, err := telemetry.ParseLogLevel(global.LogLevel)
	if err != nil {
		return exitErrorf(ExitUsage, "--log-level %q", global.LogLevel)
	}
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))

	stopTelemetry, err := feederTelemetry(cmd.Context(), opts.sourceID, log)
	if err != nil {
		return err
	}
	defer stopTelemetry()

	f := &otelfeeder.Feeder{
		SourceID:     opts.sourceID,
		Window:       opts.window,
		RetractAfter: opts.retractAfter,
		IDFormat:     format,
		Logger:       log,
	}
	if live {
		f.Heartbeat = feedHeartbeat
	}

	if opts.recordDir != "" && opts.batchSize != 1 {
		log.Info("otel: --record needs one graph answer per event; using --batch-size=1", "requested", opts.batchSize)
		opts.batchSize = 1
	}
	src, stop, err := feedOtelSource(cmd.Context(), opts, log)
	if err != nil {
		return err
	}
	defer stop()

	em, finish, err := feedOtelEmitter(cmd, global, opts, f.Describe(), log)
	if err != nil {
		return err
	}

	if opts.recordDir != "" {
		payloads := record.Wrap(src, opts.recordDir)
		events := record.Emitter(em, opts.recordDir)
		src, em = payloads, events
		defer feedOtelWriteManifest(cmd, opts, f.Describe(), payloads, events, log)
	}

	runErr := f.Run(cmd.Context(), src, em)
	if runErr != nil && !errors.Is(runErr, io.EOF) && !errors.Is(runErr, context.Canceled) {
		// A refusal is reported by finish with the published exit code; anything else is a
		// transport failure or a bug.
		if code := finish(runErr); code != nil {
			return code
		}
		return exitWith(ExitTransport, runErr)
	}
	return finish(nil)
}

// feedOtelSource builds the input: a recording, or the OTLP receivers.
func feedOtelSource(ctx context.Context, opts *feedOtelOptions, log *slog.Logger) (feeder.Source, func(), error) {
	if opts.replayDir != "" {
		src, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return nil, nil, exitWith(ExitUsage, err)
		}
		log.Info("otel: replaying a recording", "dir", opts.replayDir, "payloads", src.Len())
		return src, func() {}, nil
	}

	sink := source.NewChanSource(feedSinkBuffer)
	receiver := &otelfeeder.Receiver{
		Sink:     sink,
		GRPCAddr: opts.listenGRPC,
		HTTPAddr: opts.listenHTTP,
		Logger:   log,
	}
	if err := receiver.Start(ctx); err != nil {
		return nil, nil, exitWith(ExitTransport, err)
	}
	return sink, func() {
		if err := receiver.Shutdown(context.WithoutCancel(ctx)); err != nil {
			log.Warn("otel: receiver shutdown", "error", err.Error())
		}
	}, nil
}

// feedOtelEmitter builds the output and the function that decides what the command exits with.
func feedOtelEmitter(cmd *cobra.Command, global *globalOptions, opts *feedOtelOptions,
	desc feeder.Description, log *slog.Logger,
) (feeder.Emitter, func(error) error, error) {
	if opts.dryRun {
		dry := &dryRunEmitter{desc: desc, printer: newPrinter(cmd.OutOrStdout(), global.Output)}
		return dry, func(runErr error) error {
			if dry.refused > 0 {
				return exitErrorf(ExitRejected, "%d of %d events would be refused; the first is %s",
					dry.refused, dry.emitted+dry.refused, dry.firstRefusal)
			}
			if runErr != nil {
				return exitWith(ExitTransport, runErr)
			}
			log.Info("otel: dry run complete", "events", dry.emitted)
			return nil
		}, nil
	}

	if _, err := normalizeServerURL(global.Server); err != nil {
		return nil, nil, err
	}
	em, err := emit.NewConnectEmitter(global.Server, global.Token, desc,
		emit.WithBatchSize(opts.batchSize),
		emit.WithLogger(log),
		emit.WithResultFunc(func(result *graphv1.IngestResult) {
			if result.GetStatus() == graphv1.IngestResult_REJECTED {
				log.Error("otel: the graph refused an event",
					"event_id", result.GetEventId(),
					"reason_code", result.GetReasonCode(),
					"reason_detail", result.GetReasonDetail())
			}
		}))
	if err != nil {
		return nil, nil, exitWith(ExitUsage, err)
	}
	return em, func(runErr error) error {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), clientTimeout)
		defer cancel()
		closeErr := em.Close(closeCtx)
		stats := em.Stats()
		log.Info("otel: run finished",
			"applied", stats.Applied, "duplicate_noop", stats.DuplicateNoop,
			"rejected", stats.Rejected, "batches", stats.Batches, "dropped", stats.Dropped)
		if stats.Rejected > 0 {
			return exitErrorf(ExitRejected, "the graph refused %d events; see the log for the reason codes",
				stats.Rejected)
		}
		if runErr != nil {
			return exitWith(ExitTransport, runErr)
		}
		return exitWith(ExitTransport, closeErr)
	}, nil
}

// feedOtelWriteManifest completes a recording, so that `--record` leaves a directory
// `fixture verify` can take (the SDK guide §9).
func feedOtelWriteManifest(cmd *cobra.Command, opts *feedOtelOptions, desc feeder.Description,
	payloads *record.PayloadRecorder, events *record.EventRecorder, log *slog.Logger,
) {
	if err := payloads.Err(); err != nil {
		log.Error("otel: recording payloads failed", "error", err.Error())
	}
	if err := events.Err(); err != nil {
		log.Error("otel: recording events failed", "error", err.Error())
	}
	err := record.WriteManifest(opts.recordDir, record.Manifest{
		Family: "otel-topology",
		Description: fmt.Sprintf(
			"%d OTLP exports recorded by `feed otel` over %s windows, id format %s. "+
				"Sanitize the host names and add a queries: list before committing.",
			payloads.Count(), opts.window, opts.idFormat),
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		ExpectRejected: events.Rejections(),
	})
	if err != nil {
		log.Error("otel: writing the fixture manifest failed", "error", err.Error())
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d payloads and %d events to %s\n",
		payloads.Count(), events.Accepted(), opts.recordDir)
}

// dryRunEmitter prints what the feeder would send and validates it exactly as the graph would
// (feeder.Validate is the server's own validator), so a dry run is a real answer about whether
// the events are acceptable rather than a rehearsal of the printing.
type dryRunEmitter struct {
	desc         feeder.Description
	printer      *printer
	emitted      int
	refused      int
	firstRefusal string
}

var _ feeder.Emitter = (*dryRunEmitter)(nil)

func (e *dryRunEmitter) Emit(_ context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	if rejection := feeder.Validate(ev); rejection != nil {
		e.refused++
		if e.firstRefusal == "" {
			e.firstRefusal = fmt.Sprintf("%s: %s (%s)",
				ev.GetEventId(), rejection.ReasonCode, rejection.ReasonDetail)
		}
		return &graphv1.IngestResult{
			EventId:      ev.GetEventId(),
			Status:       graphv1.IngestResult_REJECTED,
			ReasonCode:   rejection.ReasonCode,
			ReasonDetail: rejection.ReasonDetail,
		}, nil
	}
	e.emitted++
	if err := e.print(ev); err != nil {
		return nil, err
	}
	return &graphv1.IngestResult{EventId: ev.GetEventId(), Status: graphv1.IngestResult_APPLIED}, nil
}

func (e *dryRunEmitter) print(ev *graphv1.EventEnvelope) error {
	if e.printer.json() {
		return e.printer.writeJSON(ev)
	}
	return e.printer.writeLine("%s\t%s\t%s", ev.GetEventId(), eventBodyName(ev), eventSubject(ev))
}

// Checkpoint is routed through Emit so that a checkpoint is printed and validated like every
// other event. This feeder does not use it — it builds its checkpoints itself, because the
// extent of an aggregation window is the window and its id says so — but the interface has it
// and an Emitter that silently dropped one would be a trap for the next connector.
func (e *dryRunEmitter) Checkpoint(ctx context.Context, fact feeder.CheckpointFact) error {
	_, err := e.Emit(ctx, feeder.SourceCheckpoint(e.desc, feeder.CheckpointID(e.desc, fact.ExtentTo), fact))
	return err
}

func (e *dryRunEmitter) Flush(context.Context) error { return nil }

// eventBodyName is the event's type, for the table rendering.
func eventBodyName(ev *graphv1.EventEnvelope) string {
	switch ev.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return "upsert_node"
	case *graphv1.EventEnvelope_UpsertEdge:
		return "upsert_edge"
	case *graphv1.EventEnvelope_RetractNode:
		return "retract_node"
	case *graphv1.EventEnvelope_RetractEdge:
		return "retract_edge"
	case *graphv1.EventEnvelope_ObserveChange:
		return "observe_change"
	case *graphv1.EventEnvelope_IdentityClaim:
		return "identity_claim"
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return "source_checkpoint"
	default:
		return "unknown"
	}
}

// eventSubject is what the event is about, for the table rendering.
func eventSubject(ev *graphv1.EventEnvelope) string {
	switch body := ev.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return feeder.RefString(body.UpsertNode.GetRef())
	case *graphv1.EventEnvelope_UpsertEdge:
		return feeder.RefString(body.UpsertEdge.GetSrc()) + " -> " + feeder.RefString(body.UpsertEdge.GetDst())
	case *graphv1.EventEnvelope_RetractNode:
		return feeder.RefString(body.RetractNode.GetRef())
	case *graphv1.EventEnvelope_RetractEdge:
		return feeder.RefString(body.RetractEdge.GetSrc()) + " -> " + feeder.RefString(body.RetractEdge.GetDst())
	case *graphv1.EventEnvelope_ObserveChange:
		return feeder.RefString(body.ObserveChange.GetRef())
	case *graphv1.EventEnvelope_IdentityClaim:
		return feeder.RefString(body.IdentityClaim.GetSubject()) + " claims " +
			feeder.RefString(body.IdentityClaim.GetClaim())
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return formatTime(body.SourceCheckpoint.GetExtentFrom()) + " .. " +
			formatTime(body.SourceCheckpoint.GetExtentTo())
	default:
		return ""
	}
}
