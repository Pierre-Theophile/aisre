// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// `feed` (contracts/cli.md §Feeders, FR-044).
//
// Every feeder is a subcommand of this one, and every one of them takes the same three shapes:
// live against the real source, `--record` to a fixture directory while running live, and
// `--replay` a recorded fixture with `--dry-run` to see what it would emit. The recorded mode
// is the test (FR-044), so the flags are part of the connector contract rather than a
// debugging convenience.
//
// Subcommands register themselves rather than being listed here, so two connectors being
// written at the same time never edit the same file:
//
//	// in feed_myvendor.go
//	func init() { addFeedSubcommand(newFeedMyvendorCommand) }
//
//	func newFeedMyvendorCommand(global *globalOptions) *cobra.Command { … }
//
// A factory rather than a built command, because a subcommand needs the global flags — the
// graph's address and token live there — and because a command built once at package init
// would be shared by every command tree the process builds, flag values included, which makes
// it unusable from a test that builds the tree twice.

// feedSubcommand builds one `feed` subcommand from the global flags.
type feedSubcommand func(global *globalOptions) *cobra.Command

// feedSubcommands is the registry every feeder file appends itself to from init().
var feedSubcommands []feedSubcommand

// addFeedSubcommand registers a `feed` subcommand factory. Call it from an init() in the
// file that defines the subcommand.
func addFeedSubcommand(build feedSubcommand) {
	feedSubcommands = append(feedSubcommands, build)
}

// newFeedCommand builds the `feed` tree from whatever has registered itself.
func newFeedCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feed",
		Short: "Run a connector: read one external system, emit typed graph events",
		Long: "A feeder turns one external system into typed, idempotent events in the temporal\n" +
			"graph. It never writes to the source system (constitution VII) and never stores\n" +
			"telemetry payloads (constitution IV); it attaches pointers to where the telemetry\n" +
			"lives instead.\n\n" +
			"Every feeder runs live, records what it saw with --record, and replays a recording\n" +
			"with --replay. The recorded mode is the test (FR-044).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	for _, build := range feedSubcommands {
		if sub := build(global); sub != nil {
			cmd.AddCommand(sub)
		}
	}
	return cmd
}

// feederTelemetry installs the OTel SDK and the process-wide Metrics for one feeder run, so a
// connector emits telemetry about its own operation exactly as the graph does (FR-051: "the
// graph *and both feeders*").
//
// There is no flag: the collector is configured with the standard OTEL_EXPORTER_OTLP_*
// environment variables, which is what the sidecar or the agent a feeder runs beside already
// sets. With none of them set, Setup installs real providers with no exporter and the feeder
// behaves identically — self-observability is never a precondition for feeding.
//
// The returned function flushes and releases both; call it with defer.
func feederTelemetry(ctx context.Context, sourceID string, log *slog.Logger) (func(), error) {
	shutdown, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:        telemetry.DefaultServiceName,
		ServiceVersion:     version,
		Logger:             log,
		ResourceAttributes: []attribute.KeyValue{attribute.String("sre.source.id", sourceID)},
	})
	if err != nil {
		return nil, exitWith(ExitTransport, err)
	}
	metrics, err := telemetry.NewMetrics(nil)
	if err != nil {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = shutdown(flushCtx)
		return nil, exitWith(ExitTransport, err)
	}
	telemetry.SetDefault(metrics)

	return func() {
		telemetry.SetDefault(nil)
		_ = metrics.Close()
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdown(flushCtx); err != nil {
			log.Warn("telemetry shutdown", "error", err.Error())
		}
	}, nil
}
