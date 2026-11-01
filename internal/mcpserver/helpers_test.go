// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/mcpserver"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/server/pgserver"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The tests drive the MCP server the way a coding agent does: an MCP client over in-memory
// transports, against a real `serve` with a fixture replayed into it.

// apiServer is a running API with one fixture replayed into it, and its dev token issuer.
func apiServer(t *testing.T, fixtureDir string) (baseURL string, issuer *server.DevIssuer) {
	t.Helper()
	store := pgtest.Open(t)
	p := projector.New(store)
	report, err := fixture.Load(context.Background(), p, fixtureDir, fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("load %s: %v", fixtureDir, err)
	}
	if report.Applied == 0 {
		t.Fatalf("load %s applied no events", fixtureDir)
	}
	logger := slog.New(slog.DiscardHandler)
	auth, err := server.NewDevAuthenticator(server.DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	srv, err := server.New(server.Config{
		Listen: "127.0.0.1:0", Auth: auth, Backends: pgserver.Backends(p), Logger: logger,
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, auth.Issuer()
}

func mint(t *testing.T, issuer *server.DevIssuer, roles ...server.Role) string {
	t.Helper()
	token, err := issuer.Mint("alice", roles)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return token
}

// session connects an MCP client to a server built for the token's caller, as `aisre mcp` does
// over stdio.
func session(t *testing.T, baseURL, token string, profile mcpserver.Profile) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	api := mcpserver.Connect(baseURL, nil, func() (string, error) { return token, nil })
	caller, err := api.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	srv, err := mcpserver.New(mcpserver.Options{
		Clients: api.Clients, Roles: caller.Roles, Subject: caller.Subject, Profile: profile,
		Running: mcpserver.NewRunningLimit(0),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return connectSession(t, srv)
}

func connectSession(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	return res
}

// structured returns a result's structured content as compact JSON bytes.
func structured(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	return raw
}

func text(res *mcp.CallToolResult) string {
	out := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	return out
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func ts(t *testing.T, s string) *timestamppb.Timestamp {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return timestamppb.New(parsed)
}
