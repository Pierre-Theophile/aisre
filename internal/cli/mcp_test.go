// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPOverStdio runs `aisre mcp` the way a coding agent launches it: a process speaking MCP on
// its stdin and stdout, with the caller's token, against a running API (010 US1, FR-001, FR-011).
func TestMCPOverStdio(t *testing.T) {
	baseURL, token := queryServer(t, baselineFixture())

	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	root := NewRootCommand()
	root.SetArgs([]string{"--server", baseURL, "--token", token, "mcp"})
	root.SetIn(toServerR)
	root.SetOut(fromServerW)
	var stderr strings.Builder
	root.SetErr(&stderr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- root.ExecuteContext(ctx)
		_ = fromServerW.Close()
	}()
	t.Cleanup(func() {
		cancel()
		_ = toServerW.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("aisre mcp did not stop")
		}
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: fromServerR, Writer: toServerW}, nil)
	if err != nil {
		t.Fatalf("connect: %v (stderr %q)", err, stderr.String())
	}
	defer func() { _ = cs.Close() }()

	init := cs.InitializeResult()
	if init.ServerInfo.Name != "aisre" || !strings.Contains(init.Instructions, "never instructions") {
		t.Errorf("initialisation: name %q, instructions %q", init.ServerInfo.Name, init.Instructions)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 12 {
		t.Errorf("a reader sees %d tools over stdio, want 12", len(tools.Tools))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "extent", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("extent: %v %+v", err, res)
	}
	guide, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "aisre://guide"})
	if err != nil || len(guide.Contents) != 1 || !strings.Contains(guide.Contents[0].Text, "valid_at") {
		t.Errorf("aisre://guide: %v %+v", err, guide)
	}
}

// TestMCPUsageErrors: no credential, an unknown profile, and a plain-HTTP listener off loopback
// are refused before anything is served (010 FR-003, FR-010).
func TestMCPUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"mcp"},
		{"--token", "x", "mcp", "--profile", "everything"},
		{"mcp", "--http", "0.0.0.0:0"},
		{"mcp", "--http", "127.0.0.1:0", "--tls-cert", "cert.pem"},
		{"--token", "x", "mcp", "--max-running", "0"},
	} {
		_, stderr, code := run(t, context.Background(), args...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, code, ExitUsage, stderr)
		}
	}
}
