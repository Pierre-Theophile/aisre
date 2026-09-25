// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/runner"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// `serve` (contracts/cli.md §Server and operations).
//
// Three decisions are worth stating, because each is the opposite of what a convenience-first
// CLI would do.
//
// Migrations are not applied on startup. A process that silently migrates on boot will, on the
// day a rollback happens, migrate a database an older binary then cannot read. `--migrate` is
// opt-in and `migrate` is its own command; without either, a process on an unmigrated database
// starts and reports not-ready (server.ReadyPath) rather than mutating the schema.
//
// Dev authentication needs two flags, not one, and a third to leave the loopback interface.
// `--auth dev` alone does nothing: the provider refuses to build without `--dev`, so no
// configuration typo can turn a production deployment into one that accepts self-asserted
// identities (see internal/server/auth_dev.go). Because the dev signing key is a published
// constant, `--auth dev` on anything but a loopback address additionally needs
// `--dev-insecure-listen`: otherwise the port is a credential-minting service for anyone who
// can reach it. With `--auth oidc`, `--dev` means only "this is a local mock provider, a
// plaintext issuer is expected"; every other check still applies.
//
// Telemetry is not a precondition. With no `--otel-endpoint` the SDK is installed with no
// exporter: instrumented code behaves identically and the graph starts without a collector
// (FR-051, research §15).

// Auth modes accepted by --auth.
const (
	authModeOIDC = "oidc"
	authModeDev  = "dev"
)

// serveReady is a test hook: when set, it is called with the bound address once the listener
// is held and before Serve blocks. Production leaves it nil.
var serveReady func(addr string)

type serveOptions struct {
	dsn               string
	listen            string
	authMode          string
	dev               bool
	devInsecureListen bool
	devUsers          string
	oidcIssuer        string
	oidcAudience      string
	oidcSkipAudience  bool
	oidcRolesClaim    string
	otelEndpoint      string
	otelInsecure      bool
	migrate           bool
	shutdownTimeout   time.Duration

	// The investigation engine (002 FR-064, T089). Without --enable-investigation the engine
	// is absent and the binary behaves exactly as it does today: the handler is not registered
	// and the served surface is unchanged.
	enableInvestigation bool
	modelConfig         string
	recordingRoot       string
	budgetProfiles      string
}

func newServeCommand(global *globalOptions) *cobra.Command {
	opts := &serveOptions{}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the graph API, ingestion and resolution services",
		Long: "serve binds one listener carrying the Ingest, Query and Resolution services over\n" +
			"ConnectRPC (gRPC, gRPC-Web and JSON on the same routes), plus the public /healthz and\n" +
			"/readyz endpoints.\n\n" +
			"Schema migrations are NOT applied automatically: run `aisre migrate` first, or pass\n" +
			"--migrate to apply them as part of startup.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.dsn, "db", "", "PostgreSQL DSN (default $"+EnvDSN+")")
	flags.StringVar(&opts.listen, "listen", server.DefaultListen, "address to listen on")
	flags.StringVar(&opts.authMode, "auth", authModeOIDC, "identity provider: oidc or dev")
	flags.BoolVar(&opts.dev, "dev", false,
		"enable the local dev identity provider; required by --auth dev and never for production")
	flags.BoolVar(&opts.devInsecureListen, "dev-insecure-listen", false,
		"allow --auth dev on a non-loopback address; the dev key is published, so this exposes "+
			"the graph to anyone who can reach the port")
	flags.StringVar(&opts.devUsers, "dev-users", "",
		"YAML or JSON file of dev users and their roles (--auth dev only)")
	flags.StringVar(&opts.oidcIssuer, "oidc-issuer", "",
		"OIDC issuer URL, e.g. https://login.example.com/realms/prod (https required unless --dev)")
	flags.StringVar(&opts.oidcAudience, "oidc-audience", "",
		"expected OIDC audience (the client id registered for the graph)")
	flags.BoolVar(&opts.oidcSkipAudience, "oidc-skip-audience", false,
		"accept tokens issued for any audience; only for a provider that scopes tokens some "+
			"other way, and it widens the blast radius of a stolen token")
	flags.StringVar(&opts.oidcRolesClaim, "oidc-roles-claim", server.DefaultRolesClaim,
		"dotted path to the roles claim, e.g. realm_access.roles")
	flags.StringVar(&opts.otelEndpoint, "otel-endpoint", "",
		"OTLP collector endpoint (default $OTEL_EXPORTER_OTLP_ENDPOINT; empty exports nothing)")
	flags.BoolVar(&opts.otelInsecure, "otel-insecure", false,
		"disable TLS to the OTLP collector (host:port endpoints only)")
	flags.BoolVar(&opts.migrate, "migrate", false,
		"apply embedded schema migrations before serving")
	flags.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", server.DefaultShutdownTimeout,
		"how long to drain in-flight calls on SIGTERM")
	flags.BoolVar(&opts.enableInvestigation, "enable-investigation", false,
		"serve the InvestigationService (002 FR-064); without it the engine is absent")
	flags.StringVar(&opts.modelConfig, "model-config", "",
		"model configuration file for the investigator and verifier roles (--enable-investigation only)")
	flags.StringVar(&opts.recordingRoot, "recording-root", "",
		"directory the trajectory and world recordings are written under (--enable-investigation only)")
	flags.StringVar(&opts.budgetProfiles, "budget-profiles", "",
		"budget profile file (--enable-investigation only)")

	return cmd
}

func runServe(cmd *cobra.Command, global *globalOptions, opts *serveOptions) error {
	ctx := cmd.Context()

	logger, err := newLogger(global.LogLevel, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:    telemetry.DefaultServiceName,
		ServiceVersion: version,
		Endpoint:       opts.otelEndpoint,
		Insecure:       opts.otelInsecure,
		Logger:         logger,
	})
	if err != nil {
		return exitWith(ExitTransport, fmt.Errorf("telemetry: %w", err))
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(flushCtx); err != nil {
			logger.Warn("telemetry shutdown", "error", err.Error())
		}
	}()

	metrics, err := telemetry.NewMetrics(nil)
	if err != nil {
		return exitWith(ExitTransport, fmt.Errorf("telemetry: %w", err))
	}
	defer func() { _ = metrics.Close() }()
	// The process-wide default, for the two call sites that cannot be handed one (see
	// telemetry/default.go). Everything else takes it explicitly.
	telemetry.SetDefault(metrics)
	defer telemetry.SetDefault(nil)

	// The identity provider is built before the database is opened: a misconfigured --auth is
	// a usage error the operator can fix without a database, and a process that would accept
	// the wrong identities must not get as far as holding a connection to the graph.
	auth, err := newAuthenticator(ctx, opts, logger)
	if err != nil {
		return err
	}

	dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
	if err != nil {
		return err
	}
	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		return storeError("open database", err)
	}
	defer store.Close()

	if opts.migrate {
		if err := store.Migrate(ctx); err != nil {
			return storeError("apply migrations", err)
		}
		logger.Info("schema migrations applied")
	}

	cfg := server.Config{
		Listen:          opts.listen,
		Auth:            auth,
		Projector:       projector.New(store, projector.WithMetrics(metrics)),
		Metrics:         metrics,
		Logger:          logger,
		ShutdownTimeout: opts.shutdownTimeout,
	}
	if opts.enableInvestigation {
		// The store is always available and so, now, is the engine. A deployment with no model
		// configuration still runs investigations — the provisional ranking, the causal ordering
		// and the deterministic first wave, with no vendor account and no network to one
		// (FR-067) — which is why the runner is built here unconditionally rather than only when
		// `--model-config` is given.
		investigations, err := newInvestigationRunner(ctx, store, cfg.Projector, opts, logger)
		if err != nil {
			return err
		}
		cfg.Investigation = server.NewInvestigationService(
			investigations,
			// The projector, so that the human channel served over RPC emits its three events
			// in the transactions that write their rows (FR-057a/b/e).
			investigationstore.NewInvestigationDAO(store,
				investigationstore.WithEventLog(cfg.Projector)), logger,
			// `investigate get` returns the run's ledger: the hypotheses, the judgments and the
			// evidence under the verdict. Without this the read path serves a verdict line with
			// nothing beneath it and `--ledger`, `--chain` and `--evidence` print empty sections.
			server.WithLedgerReader(investigationstore.NewLedgerDAO(store)))
		logger.Info("investigation service enabled",
			"model_config", opts.modelConfig,
			"recording_root", opts.recordingRoot,
			"budget_profiles", opts.budgetProfiles)
	}
	srv, err := server.New(cfg)
	if err != nil {
		return exitWith(ExitTransport, err)
	}
	if serveReady != nil {
		serveReady(srv.Addr())
	}

	if err := srv.Serve(ctx); err != nil {
		return exitWith(ExitTransport, fmt.Errorf("serve: %w", err))
	}
	return nil
}

// newAuthenticator builds the identity provider named by --auth.
//
// Every check below fails closed: the flag that weakens authentication has to be typed, and
// the flag that is missing is never inferred. A process that would accept the wrong identities
// must not start at all, which is why this runs before the database is even opened.
func newAuthenticator(ctx context.Context, opts *serveOptions, logger *slog.Logger) (server.Authenticator, error) {
	switch opts.authMode {
	case authModeDev:
		// The dev signing key is a published constant, so a dev server on a routable address
		// is an open graph: anyone who can reach the port can mint `alice` with every role.
		// Loopback is the only address where that is contained, and leaving it needs a second
		// deliberate flag (constitution VII, FR-041a).
		if err := checkDevListen(opts.listen, opts.devUsers, opts.devInsecureListen); err != nil {
			return nil, err
		}
		auth, err := server.NewDevAuthenticator(server.DevConfig{
			Enabled:   opts.dev,
			UsersFile: opts.devUsers,
			Logger:    logger,
		})
		if err != nil {
			return nil, exitErrorf(ExitUsage,
				"--auth dev: %v (pass --dev to confirm you mean it; never use it in production)", err)
		}
		return auth, nil
	case authModeOIDC:
		if opts.oidcIssuer == "" {
			return nil, exitErrorf(ExitUsage,
				"--auth oidc requires --oidc-issuer (or use --auth dev --dev locally)")
		}
		// The audience check is what stops a token minted for another service being replayed
		// at the graph. Turning it off is legitimate for a provider that scopes tokens some
		// other way, but it is never the default and never inferred from a missing flag.
		if opts.oidcAudience == "" && !opts.oidcSkipAudience {
			return nil, exitErrorf(ExitUsage,
				"--auth oidc requires --oidc-audience; pass --oidc-skip-audience only if your "+
					"provider scopes tokens some other way, and know that it lets a token minted "+
					"for any other service authenticate here")
		}
		if opts.oidcAudience != "" && opts.oidcSkipAudience {
			return nil, exitErrorf(ExitUsage,
				"--oidc-audience and --oidc-skip-audience contradict each other: pass one")
		}
		// Discovery and JWKS travel over this URL. Without TLS anyone on the path can serve
		// their own key set and mint whatever identity they like.
		if err := checkIssuerTLS(opts.oidcIssuer, opts.dev); err != nil {
			return nil, err
		}
		if opts.oidcSkipAudience {
			logger.Warn("OIDC audience check disabled by --oidc-skip-audience; "+
				"a token minted for any other service will authenticate here",
				"issuer", opts.oidcIssuer)
		}
		auth, err := server.NewOIDCAuthenticator(ctx, server.OIDCConfig{
			IssuerURL:         opts.oidcIssuer,
			Audience:          opts.oidcAudience,
			SkipAudienceCheck: opts.oidcSkipAudience,
			RolesClaim:        opts.oidcRolesClaim,
		})
		if err != nil {
			return nil, exitWith(ExitTransport, err)
		}
		return auth, nil
	default:
		return nil, exitErrorf(ExitUsage, "--auth %q: want %s or %s", opts.authMode, authModeOIDC, authModeDev)
	}
}

// checkIssuerTLS refuses a plaintext OIDC issuer outside development. `--dev` is the one escape
// hatch, for a mock provider on localhost; it is also what the dev provider itself demands, so
// the flag means the same thing in both modes.
func checkIssuerTLS(issuer string, dev bool) error {
	parsed, err := url.Parse(issuer)
	if err != nil {
		return exitErrorf(ExitUsage, "--oidc-issuer %q is not a URL: %v", issuer, err)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" {
		return exitErrorf(ExitUsage,
			"--oidc-issuer %q: want an https:// URL", issuer)
	}
	if dev {
		return nil
	}
	return exitErrorf(ExitUsage,
		"--oidc-issuer %q is not https: discovery and the key set travel over it, so anyone on "+
			"the path could serve their own keys. Pass --dev if this is a local mock provider.",
		issuer)
}

// checkDevListen refuses the published dev signing key on an address other than loopback.
//
// A users file does not change the answer: it restricts *who* may be minted, not who may mint,
// and the key that signs them is still the published constant.
func checkDevListen(listen, usersFile string, allowed bool) error {
	if allowed {
		return nil
	}
	loopback, err := isLoopbackListen(listen)
	if err != nil {
		return exitErrorf(ExitUsage, "--listen %q: %v", listen, err)
	}
	if loopback {
		return nil
	}
	detail := ""
	if usersFile != "" {
		detail = " (--dev-users restricts which identities may be minted, not who may mint them: " +
			"the signing key is the same published constant)"
	}
	return exitErrorf(ExitUsage,
		"--auth dev on %q: dev tokens are signed with a published key, so anyone who can reach "+
			"that address can mint any identity with any role%s. Listen on loopback, use --auth "+
			"oidc, or pass --dev-insecure-listen if you accept that.", listen, detail)
}

// isLoopbackListen reports whether a listen address is reachable only from this host. An empty
// host — `:8080`, the default — binds every interface and is not.
func isLoopbackListen(listen string) (bool, error) {
	if listen == "" {
		listen = server.DefaultListen
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false, err
	}
	switch host {
	case "", "0.0.0.0", "[::]", "::":
		return false, nil
	case "localhost":
		return true, nil
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// A host name that is not `localhost` has to be resolved to be judged, and a name
		// that resolves differently later would make this a check in name only. Refuse.
		return false, nil
	}
	return ip.IsLoopback(), nil
}

// newLogger builds the structured logger from --log-level. It writes to stderr so that stdout
// stays a clean data channel for `--output json`.
func newLogger(level string, out interface{ Write([]byte) (int, error) }) (*slog.Logger, error) {
	parsed, err := telemetry.ParseLogLevel(level)
	if err != nil {
		return nil, exitErrorf(ExitUsage, "--log-level %q: want debug, info, warn or error", level)
	}
	return telemetry.NewLogger(telemetry.LogConfig{
		Level:  parsed,
		Format: telemetry.LogFormatJSON,
		Output: out,
		Attrs:  []slog.Attr{slog.String("service.version", version)},
	}), nil
}

// Building the investigation engine for a served deployment (002 T089, FR-064, FR-067).
//
// Four flags, and the shape of the thing they configure follows from one rule: **a deployment
// that has configured nothing still runs investigations.** That is FR-067 — "fully operable with
// no vendor connector configured" — and it is what makes the feature testable on a laptop and
// safe to enable on a graph nobody has wired telemetry into yet. So every dependency below has a
// working default, and each default is the honest one rather than the convenient one:
//
//	--model-config     absent, or no API key ⇒ model-free: the prior-only ranking, the causal
//	                   ordering and the deterministic first wave. Logged, never silent.
//	--recording-root   absent ⇒ no trajectory is written and the row says so; present with a
//	                   `world/` ⇒ the telemetry workers answer from it.
//	--budget-profiles  absent ⇒ the two published profiles.
//	π₀                 no published coverage audit ⇒ 1.0, so every candidate sits at prior 0 and
//	                   the honest answer is `unknown` until a ceiling is measured (FR-069a).

// newInvestigationRunner assembles the engine `serve --enable-investigation` hands the service.
func newInvestigationRunner(
	ctx context.Context,
	store *postgres.Store,
	proj *projector.Projector,
	opts *serveOptions,
	logger *slog.Logger,
) (*runner.Runner, error) {
	profiles, err := budget.LoadProfiles(opts.budgetProfiles)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}

	telemetry, mode := investigationTelemetry(opts.recordingRoot, logger)
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{
		metrics.New(telemetry), logs.New(telemetry), traces.New(telemetry),
	} {
		if err := registry.Register(w); err != nil {
			return nil, exitWith(ExitUsage, fmt.Errorf("investigation workers: %w", err))
		}
	}

	client, err := investigationModelClient(opts.modelConfig, logger)
	if err != nil {
		return nil, err
	}

	prior, err := investigationstore.LatestPrior(ctx, store)
	if err != nil {
		return nil, storeError("read the published coverage audit", err)
	}
	if prior.AuditID == "" {
		logger.Warn("investigation engine: no coverage audit has been published, so π₀ = 1: " +
			"every candidate enters at prior 0 and every investigation reports `unknown` until " +
			"`aisre audit coverage` measures a ceiling (FR-069a)")
	}

	investigations, err := runner.New(runner.Config{
		Store:         store,
		Projector:     proj,
		Graph:         runner.GraphOver(store),
		Workers:       registry,
		Profiles:      profiles,
		Model:         client,
		RecordingRoot: opts.recordingRoot,
		// The world the telemetry backend above is answering from, so an export carries the
		// layer 2 the run actually read rather than looking for one nothing writes.
		WorldDir: worldDirOf(opts.recordingRoot),
		Prior:    prior,
		Mode:     mode,
		Logger:   logger,
	})
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	return investigations, nil
}

// investigationTelemetry chooses the telemetry backend and the mode every worker call is stamped
// with (FR-015).
//
// `--recording-root` doubles as the place recordings are *written* and the place a recorded world
// is *read from*, which is what the published flag set allows: contracts/cli.md gives `serve` four
// flags and none of them names a telemetry source separately. A root holding a `world/` answers
// from it in `recorded` mode; a root holding none gets the unconfigured backend, which refuses
// every telemetry term with a reason rather than leaving the engine to fail on a capability
// nobody declared.
func investigationTelemetry(recordingRoot string, logger *slog.Logger) (sdk.TelemetryBackend, worker.Mode) {
	if dir := worldDirOf(recordingRoot); dir != "" {
		recorded, err := engine.NewRecorded(dir)
		if err == nil {
			logger.Info("investigation engine: telemetry answered from a recorded world",
				"world", dir)
			return recorded, worker.ModeRecorded
		}
		logger.Warn("investigation engine: could not load the recorded world; telemetry will be "+
			"refused with a reason rather than guessed at",
			"world", dir, "error", err.Error())
	}
	logger.Warn("investigation engine: no telemetry recording is configured, so every telemetry " +
		"term is refused with a reason and every hypothesis it would have tested is reported " +
		"untested. Point --recording-root at a directory holding a world/ (see `worker record`); " +
		"this build ships no live telemetry backend")
	return runner.UnconfiguredTelemetry(
		"no telemetry backend is configured on this server: --recording-root names no recorded " +
			"world, and this build ships no live telemetry backend"), worker.ModeLive
}

// investigationModelClient builds the model boundary, or returns nil for a model-free deployment.
//
// Both conditions have to hold: a model configuration has to be named **and** a credential has to
// be resolvable. Either one alone is a deployment that would fail on its first turn, in the middle
// of an incident, which is the worst moment to discover a configuration problem — so it is
// reported at startup and the server runs model-free instead of refusing to start. The engine is
// useful without a model; it is not useful halfway through failing.
//
// The price table is read from `prices.yaml` beside the model configuration, because the two are
// recorded together and a cost figure is only interpretable against the table that produced it
// (FR-048). The model configuration names the version it expects and the client refuses a
// mismatch, so a stale file is an error at startup rather than a wrong number in a report.
func investigationModelClient(configPath string, logger *slog.Logger) (*model.Client, error) {
	if strings.TrimSpace(configPath) == "" {
		logger.Info("investigation engine: no --model-config, so every run is the prior-only " +
			"ranking, the causal ordering and the deterministic first wave (FR-067)")
		return nil, nil //nolint:nilnil // a nil client is the published model-free configuration
	}
	pricesPath := filepath.Join(filepath.Dir(configPath), "prices.yaml")
	config, prices, err := model.LoadPair(configPath, pricesPath)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	// The credential follows the *provider each role names*, not a single vendor: a configuration
	// may put the investigator on one and the verifier on another (FR-022a), and a deployment
	// needs a key for every provider it configures and for none that it does not.
	if missing := config.MissingCredentials(); len(missing) > 0 {
		logger.Warn("investigation engine: --model-config is set but $"+strings.Join(missing, ", $")+
			" is not, so the engine runs model-free rather than failing on its first turn (FR-067)",
			"model_config", configPath, "providers", providerNames(config))
		return nil, nil //nolint:nilnil // a nil client is the published model-free configuration
	}
	client, err := model.NewClient(config, prices)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	logger.Info("investigation engine: model configured",
		"model_config", configPath, "version", config.Version,
		"price_table", prices.Version, "providers", providerNames(config))
	return client, nil
}

// providerNames renders a configuration's providers for a log line.
func providerNames(config model.Config) string {
	providers := config.Providers()
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, string(p))
	}
	return strings.Join(names, ",")
}

// EnvAnthropicAPIKey is the Anthropic credential. It is the model package's constant, named here
// so the CLI's published spelling does not move; the Mistral one beside it is
// model.EnvMistralAPIKey.
const EnvAnthropicAPIKey = model.EnvAnthropicAPIKey
