// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/mcpserver"
)

// `aisre mcp` (feature 010): the read-only MCP server for coding agents.
//
// Over stdio (the default) the server is started by the coding agent and speaks for the caller
// whose credential the global --token/--token-file give, re-read on every call. With --http it
// serves streamable HTTP and holds no credential: each request's own bearer token is forwarded.
// Either way it is a client of the API at --server and nothing else.

type mcpOptions struct {
	httpAddr   string
	tlsCert    string
	tlsKey     string
	profile    string
	maxRunning int
}

func newMCPCommand(global *globalOptions) *cobra.Command {
	o := &mcpOptions{}
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the graph, the catalog and investigations to coding agents over MCP",
		Long: "mcp is a Model Context Protocol server for coding agents (Claude Code, Cursor, Codex and\n" +
			"any MCP client). It answers the graph queries (subgraph, diff, impact, history, pointers,\n" +
			"extent, resolve_why), reads the service catalog and the investigations, and can start an\n" +
			"investigation. It changes nothing in the systems aisre observes.\n\n" +
			"It is a client of the API at --server, with the caller's own credential: over stdio the\n" +
			"global --token or --token-file; over --http the bearer token of each request, which is\n" +
			"forwarded and never stored. Only the tools the caller's roles allow are listed.\n\n" +
			"--profile baseline lists only the graph and catalog tools, for the evaluation's\n" +
			"general-agent baseline (ADR-0019 D6).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd, global, o)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&o.httpAddr, "http", "", "serve streamable HTTP on this address instead of stdio")
	flags.StringVar(&o.tlsCert, "tls-cert", "", "TLS certificate for --http (required off loopback)")
	flags.StringVar(&o.tlsKey, "tls-key", "", "TLS key for --http (required off loopback)")
	flags.StringVar(&o.profile, "profile", string(mcpserver.ProfileDefault), "tool set: default or baseline")
	flags.IntVar(&o.maxRunning, "max-running", mcpserver.DefaultMaxRunning,
		"investigations one caller may have running through this server")
	return cmd
}

func runMCP(cmd *cobra.Command, global *globalOptions, o *mcpOptions) error {
	profile, err := mcpserver.ParseProfile(o.profile)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	if o.maxRunning < 1 {
		return exitErrorf(ExitUsage, "--max-running %d: want at least 1", o.maxRunning)
	}
	base, err := normalizeServerURL(global.Server)
	if err != nil {
		return err
	}
	// stdout is the protocol over stdio: every log line goes to stderr.
	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
	running := mcpserver.NewRunningLimit(o.maxRunning)

	if o.httpAddr != "" {
		return serveMCPHTTP(cmd, global, o, base, profile, running, logger)
	}

	if err := checkCredentialTransport(global, base); err != nil {
		return err
	}
	source := tokenSource(global)
	if source == nil {
		return exitErrorf(ExitUsage, "mcp: no credential: pass --token or --token-file (or $%s, $%s); "+
			"every call is made on behalf of an individual", EnvToken, EnvTokenFile)
	}
	api := mcpserver.Connect(base, nil, mcpserver.TokenSource(source))
	ctx, cancel := context.WithTimeout(cmd.Context(), clientTimeout)
	caller, err := api.WhoAmI(ctx)
	cancel()
	if err != nil {
		return remoteError("mcp", err)
	}
	server, err := mcpserver.New(mcpserver.Options{
		Clients: api.Clients, Roles: caller.Roles, Subject: caller.Issuer + "|" + caller.Subject,
		Profile: profile, Version: version, Logger: logger, Running: running,
	})
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	logger.Info("mcp: serving over stdio", "server", base, "subject", caller.Subject,
		"roles", caller.Roles, "profile", profile)
	transport := &mcp.IOTransport{
		Reader: io.NopCloser(cmd.InOrStdin()),
		Writer: nopWriteCloser{cmd.OutOrStdout()},
	}
	if err := server.Run(cmd.Context(), transport); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return exitWith(ExitTransport, err)
	}
	return nil
}

// mcpListenReady is a test hook called with the bound address.
var mcpListenReady = func(string) {}

func serveMCPHTTP(cmd *cobra.Command, global *globalOptions, o *mcpOptions, base string,
	profile mcpserver.Profile, running *mcpserver.RunningLimit, logger *slog.Logger) error {
	useTLS := o.tlsCert != "" || o.tlsKey != ""
	if useTLS && (o.tlsCert == "" || o.tlsKey == "") {
		return exitErrorf(ExitUsage, "--tls-cert and --tls-key go together")
	}
	if err := mcpserver.CheckListen(o.httpAddr, useTLS); err != nil {
		return exitWith(ExitUsage, err)
	}
	// The tokens forwarded to the API are the clients'; the API must not be reached in clear
	// beyond loopback either.
	if !global.InsecureTransport {
		if err := checkCredentialTransport(&globalOptions{Token: "forwarded"}, base); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", o.httpAddr)
	if err != nil {
		return exitWith(ExitUsage, err)
	}
	srv := &http.Server{
		Handler: mcpserver.NewHTTPHandler(mcpserver.HTTPOptions{
			APIURL: base, Profile: profile, Version: version, Logger: logger, Running: running,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-cmd.Context().Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("mcp: serving streamable HTTP", "addr", listener.Addr().String(), "server", base,
		"profile", profile, "tls", useTLS)
	mcpListenReady(listener.Addr().String())
	if useTLS {
		err = srv.ServeTLS(listener, o.tlsCert, o.tlsKey)
	} else {
		err = srv.Serve(listener)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return exitWith(ExitTransport, err)
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
