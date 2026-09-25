// SPDX-License-Identifier: Apache-2.0

// Command mcp-feeder runs the MCP ownership-directory connector.
//
// Against the bundled mock server, with nothing else running:
//
//	go run ./cmd/mcp-feeder --dry-run --polls 2 --interval 1ms \
//	    --environment prod --mcp-cmd "go run ./cmd/mcp-mockserver"
//
// Against a development graph:
//
//	aisre serve --auth dev --dev &
//	TOKEN=$(aisre dev-token --dev --user mcp-feeder --roles feeder --source-id mcp:acme-directory)
//	go run ./cmd/mcp-feeder --mcp-cmd "go run ./cmd/mcp-mockserver" \
//	    --server http://127.0.0.1:8080 --token "$TOKEN" --interval 30s
//
// Re-recording the fixture (the exact command that produced testdata/directory-01):
//
//	go run ./cmd/mcp-feeder --dry-run --polls 2 --interval 1ms --environment prod \
//	    --mcp-cmd "go run ./cmd/mcp-mockserver" \
//	    --record testdata/directory-01 --observed-at 2026-09-16T09:05:00Z
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"

	mcpfeeder "github.com/Pierre-Theophile/aisre/examples/mcp-feeder/feeder"
	"github.com/Pierre-Theophile/aisre/examples/mcp-feeder/mockserver"
)

func main() {
	var (
		mcpCmd     = flag.String("mcp-cmd", "", "command to spawn an MCP server on stdio, e.g. \"go run ./cmd/mcp-mockserver\"")
		mcpURL     = flag.String("mcp-url", "", "streamable-HTTP endpoint of an MCP server; an alternative to --mcp-cmd")
		sourceID   = flag.String("source-id", "mcp:"+mockserver.ServerName, "this connector's source id; the feeder token is scoped to it")
		serverName = flag.String("server-name", mockserver.ServerName, "MCP server name; every pointer's backend_kind is mcp:<name>")
		env        = flag.String("environment", "", "deployment environment the directory describes, e.g. prod; empty says nothing")
		interval   = flag.Duration("interval", time.Minute, "how often to poll the MCP server")
		polls      = flag.Int("polls", 0, "stop after this many polls; 0 polls until interrupted")
		recordDir  = flag.String("record", "", "record payloads and events into this fixture directory")
		replayDir  = flag.String("replay", "", "replay a recorded fixture instead of connecting to a server")
		dryRun     = flag.Bool("dry-run", false, "validate events in memory instead of sending them to a graph")
		server     = flag.String("server", "", "graph base URL, e.g. http://127.0.0.1:8080")
		token      = flag.String("token", "", "feeder-role token scoped to --source-id")
		observedAt = flag.String("observed-at", "", "RFC 3339 observed time for --dry-run, so a recording is byte-stable; empty means now")
	)
	flag.Parse()

	if err := run(context.Background(), options{
		mcpCmd: *mcpCmd, mcpURL: *mcpURL, sourceID: *sourceID, serverName: *serverName,
		environment: *env, interval: *interval, polls: *polls, recordDir: *recordDir,
		replayDir: *replayDir, dryRun: *dryRun, server: *server, token: *token,
		observedAt: *observedAt,
	}); err != nil {
		log.Fatalf("mcp-feeder: %v", err)
	}
}

type options struct {
	mcpCmd, mcpURL       string
	sourceID, serverName string
	environment          string
	interval             time.Duration
	polls                int
	recordDir, replayDir string
	dryRun               bool
	server, token        string
	observedAt           string
}

func run(ctx context.Context, o options) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	f := &mcpfeeder.Feeder{SourceID: o.sourceID, ServerName: o.serverName, Environment: o.environment}
	desc := f.Describe()

	src, closeSrc, err := openSource(ctx, o, f)
	if err != nil {
		return err
	}
	defer closeSrc()

	em, closeEm, err := openEmitter(o, desc)
	if err != nil {
		return err
	}
	defer closeEm()

	var (
		payloadRecorder *record.PayloadRecorder
		eventRecorder   *record.EventRecorder
	)
	if o.recordDir != "" {
		payloadRecorder = record.Wrap(src, o.recordDir)
		eventRecorder = record.Emitter(em, o.recordDir)
		src, em = payloadRecorder, eventRecorder
	}

	if err := f.Run(ctx, src, em); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if payloadRecorder != nil {
		if err := payloadRecorder.Err(); err != nil {
			return err
		}
		if err := eventRecorder.Err(); err != nil {
			return err
		}
		if err := record.WriteManifest(o.recordDir, record.Manifest{
			Family:         "mcp-directory",
			Description:    "one MCP ownership directory, two polls: three channels, four runbooks, three announced flag flips",
			Sources:        []record.ManifestSource{record.SourceOf(desc)},
			ExpectRejected: eventRecorder.Rejections(),
		}); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "recorded %d payloads and %d events into %s\n",
			payloadRecorder.Count(), eventRecorder.Accepted(), o.recordDir)
	}
	return nil
}

// openSource builds the input: a recording, a spawned server, or an HTTP endpoint.
func openSource(ctx context.Context, o options, f *mcpfeeder.Feeder) (feeder.Source, func(), error) {
	if o.replayDir != "" {
		src, err := source.NewFileSource(o.replayDir)
		if err != nil {
			return nil, nil, err
		}
		return src, func() {}, nil
	}

	var (
		client *mcpfeeder.Client
		err    error
	)
	switch {
	case o.mcpCmd != "" && o.mcpURL != "":
		return nil, nil, errors.New("--mcp-cmd and --mcp-url are alternatives; give one")
	case o.mcpCmd != "":
		// The command is split on whitespace, which is enough for the shapes an MCP server
		// is launched with (`npx -y @vendor/mcp-server`, `go run ./cmd/...`) and avoids
		// handing operator input to a shell.
		fields := strings.Fields(o.mcpCmd)
		client, err = mcpfeeder.Dial(ctx, fields[0], fields[1:]...)
	case o.mcpURL != "":
		client, err = mcpfeeder.DialHTTP(ctx, o.mcpURL, nil)
	default:
		return nil, nil, errors.New("give --mcp-cmd, --mcp-url or --replay")
	}
	if err != nil {
		return nil, nil, err
	}
	if name := client.ServerName(); name != "" && name != f.ServerName {
		// Not fatal, but worth saying: every pointer this run writes will name
		// mcp:<--server-name>, and that is what a reader will use to find the server again.
		log.Printf("connected to MCP server %q but pointers will say %q (--server-name)", name, f.ServerName)
	}
	return mcpfeeder.NewPollSource(client, o.interval, o.polls), func() { _ = client.Close() }, nil
}

// openEmitter builds the output: memory for a dry run, the graph otherwise.
func openEmitter(o options, desc feeder.Description) (feeder.Emitter, func(), error) {
	if o.dryRun {
		opts := []emit.MemoryOption{emit.WithStrict(true)}
		if o.observedAt != "" {
			at, err := time.Parse(time.RFC3339, o.observedAt)
			if err != nil {
				return nil, nil, fmt.Errorf("--observed-at %q: %w", o.observedAt, err)
			}
			opts = append(opts, emit.WithMemoryClock(func() time.Time { return at.UTC() }))
		}
		em := emit.NewMemoryEmitter(desc, opts...)
		return em, func() {
			fmt.Fprintf(os.Stderr, "dry run: %d events accepted, %d refused\n", len(em.Events()), len(em.Rejected()))
		}, nil
	}
	if o.server == "" || o.token == "" {
		return nil, nil, errors.New("--server and --token are required unless --dry-run")
	}
	// Batch size 1 whenever a recording is being written: record.Emitter needs the graph's
	// answer per event, and a batching emitter has not asked yet.
	batch := emit.DefaultBatchSize
	if o.recordDir != "" {
		batch = 1
	}
	em, err := emit.NewConnectEmitter(o.server, o.token, desc, emit.WithBatchSize(batch))
	if err != nil {
		return nil, nil, err
	}
	return em, func() { _ = em.Close(context.Background()) }, nil
}
