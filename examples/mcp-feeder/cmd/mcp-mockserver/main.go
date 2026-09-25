// SPDX-License-Identifier: Apache-2.0

// Command mcp-mockserver runs the mock ownership directory on the stdio transport.
//
// It is what `mcp-feeder --mcp-cmd` spawns by default, and what you point any other MCP
// client at to see the responses the connector is written against:
//
//	go run ./cmd/mcp-mockserver
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Pierre-Theophile/aisre/examples/mcp-feeder/mockserver"
)

func main() {
	seed := flag.Int64("seed", 1, "seed selecting the on-call rotation; the same seed is the same directory")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := mockserver.New(mockserver.Options{Seed: *seed}).Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp-mockserver: %v", err)
	}
}
