// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The command tree (contracts/cli.md).
//
// Two things here are contract rather than convenience.
//
// Exit codes. An SRE puts this binary in a pipeline, and a pipeline reads the status, not the
// prose: 1 means "you typed it wrong", 2 means "the server or the network is unwell", 3 means
// "your credential is the problem", 4 means "a fixture did not match its goldens", 5 means
// "an event was rejected". Every error a command raises carries its code explicitly, so the
// mapping is data rather than a guess made at the exit.
//
// Output. `--output json` is the canonical serialization the goldens are written in
// (graph.CanonicalJSON), not a second, prettier rendering of the same data. That is what lets
// a fixture's expected output and a CLI invocation be compared byte for byte.

// Exit codes, exactly as published in contracts/cli.md §Exit codes.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitUsage is a malformed invocation: an unknown flag, a missing argument, a bad value.
	ExitUsage = 1
	// ExitTransport is a server or network failure, including an RPC this build of the server
	// does not implement.
	ExitTransport = 2
	// ExitAuth is an authentication or authorization failure.
	ExitAuth = 3
	// ExitVerification is a fixture whose replay did not match its goldens.
	ExitVerification = 4
	// ExitRejected is an event the graph refused (feeders and dry runs).
	ExitRejected = 5
)

// Environment variables the CLI honours, so that a shell can be configured once.
const (
	// EnvToken is the bearer token used when --token is not given.
	EnvToken = "SRE_AGENT_TOKEN"
	// EnvDSN is the PostgreSQL DSN used when --db is not given.
	EnvDSN = "PG_DSN"
)

// DefaultServer is the address `--server` defaults to.
const DefaultServer = "http://localhost:8080"

// version is the build version. main sets it through SetVersion so that the `-X main.version`
// link flag keeps working.
var version = "dev"

// SetVersion records the build version reported by `aisre version` and `--version`.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

// Version returns the build version.
func Version() string { return version }

// exitError is an error that knows which exit code it deserves.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitWith tags err with an exit code. A nil err stays nil.
func exitWith(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// exitErrorf builds a tagged error from a format string.
func exitErrorf(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// ExitCode maps an error onto the published exit codes.
//
// Errors raised by the commands themselves always carry their code. A ConnectRPC error that
// reaches here untagged is mapped by its code. Anything else came from cobra's own flag and
// argument handling, which is by definition a usage error.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var tagged *exitError
	if errors.As(err, &tagged) {
		return tagged.code
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return exitCodeForConnect(connectErr.Code())
	}
	return ExitUsage
}

func exitCodeForConnect(code connect.Code) int {
	switch code {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return ExitAuth
	default:
		return ExitTransport
	}
}

// globalOptions are the persistent flags every command shares.
type globalOptions struct {
	Output   string
	Server   string
	Token    string
	LogLevel string
}

// NewRootCommand builds the whole command tree. It is exported so that tests, and any embedder
// of this binary, can run a command without going through os.Exit.
func NewRootCommand() *cobra.Command {
	opts := &globalOptions{}

	root := &cobra.Command{
		Use:   "aisre",
		Short: "Bitemporal temporal system graph for an AI SRE agent",
		Long: "sre-agent serves, feeds, queries and verifies the bitemporal system graph.\n\n" +
			"Every command speaks to a running server over ConnectRPC except `serve`, `migrate`\n" +
			"and `fixture`, which talk to the database directly.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			opts.Token = tokenFrom(opts.Token, os.Getenv(EnvToken))
			if opts.Output != OutputTable && opts.Output != OutputJSON {
				return exitErrorf(ExitUsage, "--output %q: want %s or %s",
					opts.Output, OutputTable, OutputJSON)
			}
			if _, err := telemetry.ParseLogLevel(opts.LogLevel); err != nil {
				return exitErrorf(ExitUsage, "--log-level %q: want debug, info, warn or error",
					opts.LogLevel)
			}
			cmd.SilenceUsage = true
			return nil
		},
	}

	flags := root.PersistentFlags()
	flags.StringVar(&opts.Output, "output", OutputTable,
		"output format: table (human) or json (canonical, golden-comparable)")
	flags.StringVar(&opts.Server, "server", DefaultServer,
		"base URL of the sre-agent server")
	// The default is deliberately empty and $SRE_AGENT_TOKEN is read in PersistentPreRunE
	// instead. Cobra renders a non-empty default as `(default "…")` in --help, so wiring the
	// environment variable in here printed the caller's live bearer token to anyone who ran
	// `aisre --help` — in a terminal, in a CI log, in a screenshot on a ticket.
	flags.StringVar(&opts.Token, "token", "",
		"bearer token for the server (default $"+EnvToken+")")
	flags.StringVar(&opts.LogLevel, "log-level", "info",
		"log level: debug, info, warn or error")

	// Cobra reports a bad flag as a plain error; tagging it here is what makes a typo exit 1
	// rather than being mistaken for something worse.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitWith(ExitUsage, err)
	})

	root.AddCommand(
		newServeCommand(opts),
		newMigrateCommand(opts),
		newDevTokenCommand(opts),
		newExtentCommand(opts),
		newFeedCommand(opts),
		newQueryCommand(opts),
		newResolveCommand(opts),
		newFixtureCommand(opts),
		newInvestigateCommand(opts),
		newWorkerCommand(opts),
		newBackendCommand(opts),
		newKnowledgeCommand(opts),
		newAuditCommand(opts),
		newEvalCommand(opts),
		newVersionCommand(opts),
	)
	return root
}

// Execute runs the command tree and exits with the code contracts/cli.md publishes.
func Execute() {
	root := NewRootCommand()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := root.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(root.ErrOrStderr(), "sre-agent:", err)
	}
	os.Exit(ExitCode(err))
}
