// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The one process that fronts the graph (FR-017, FR-035, FR-041a, FR-051).
//
// Everything the graph exposes is mounted here, on one listener, behind one authentication
// interceptor. Three things are worth knowing:
//
//   - The transport is ConnectRPC over h2c. Feeders speak gRPC, so HTTP/2 has to work without
//     TLS in a cluster that terminates TLS at its ingress; h2c gives that without making every
//     laptop generate certificates. The same endpoint answers Connect and gRPC-Web, and the
//     same procedure answers a plain `curl -H 'Content-Type: application/json'`, which is what
//     makes the JSON half of FR-035 free.
//   - Authentication is an interceptor, authorization is the handler's job. The interceptor
//     proves who is calling (or refuses); each RPC then states the role it needs with Require
//     or RequireSource, because "read-only credentials suffice for every query" (FR-035) and
//     "a feeder may only write for its own source" (FR-046) are different rules.
//   - The health endpoints are plain HTTP and public. A load balancer has no token, and a
//     liveness probe that needed one would fail closed on an identity-provider outage.
//
// QueryService and ResolutionService are registered here from the first version, and as of the
// impact, pointers and history engines (US4, US5, US7) every RPC either answers or refuses on
// its own merits. Authorization is still checked before anything else, so an operator reading a
// status can always tell a credential problem — Unauthenticated, PermissionDenied — from a
// problem with the question they asked.

// instrumentationName is the OpenTelemetry instrumentation scope of this package.
const instrumentationName = "github.com/Pierre-Theophile/aisre/internal/server"

// Defaults for Config.
const (
	// DefaultListen is the address `serve` binds when none is given.
	DefaultListen = ":8080"
	// DefaultShutdownTimeout bounds how long a graceful shutdown waits for in-flight calls —
	// an ingestion stream can be long-lived, so the wait is finite.
	DefaultShutdownTimeout = 15 * time.Second
	// healthTimeout bounds the database check behind the health endpoints. A probe that hangs
	// is worse than a probe that fails.
	healthTimeout = 2 * time.Second
)

// Health endpoint paths. They are plain HTTP, public, and never part of a Connect service.
const (
	// HealthPath answers 200 while the process is up and its database answers a ping.
	HealthPath = "/healthz"
	// ReadyPath answers 200 only once the schema migrations have been applied, so an
	// orchestrator does not route ingestion at a process that would reject every event.
	ReadyPath = "/readyz"
)

// Config configures New.
type Config struct {
	// Listen is the TCP address to bind, e.g. ":8080" or "127.0.0.1:0" for a random port.
	// Empty means DefaultListen.
	Listen string
	// Auth verifies bearer tokens. Required: a graph with no authenticator would accept
	// anonymous callers, which FR-041a forbids.
	Auth Authenticator
	// Projector is the only way events reach the graph. Required.
	Projector *projector.Projector
	// Metrics receives the instruments from plan §Observability. A nil *Metrics is a no-op,
	// so tests may leave it unset.
	Metrics *telemetry.Metrics
	// Investigation, when non-nil, mounts InvestigationService (002 FR-064). It is set by
	// `serve --enable-investigation` and by nothing else: without the flag the engine is
	// absent and the binary behaves exactly as it does today (002 plan F10).
	Investigation *InvestigationService
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// ShutdownTimeout defaults to DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
}

// Server is the HTTP/2 server carrying the graph's RPC services and health endpoints.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	listener net.Listener
	http     *http.Server
	handler  http.Handler
}

// New binds Config.Listen and builds the mux. It returns as soon as the port is held, so a
// caller (and a test) can read Addr before Serve is running.
func New(cfg Config) (*Server, error) {
	if cfg.Auth == nil {
		return nil, errors.New("server: an Authenticator is required; every call must name an individual (FR-041a)")
	}
	if cfg.Projector == nil {
		return nil, errors.New("server: a Projector is required")
	}
	if cfg.Projector.Store() == nil {
		return nil, errors.New("server: the Projector has no store")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{cfg: cfg, logger: logger}

	interceptors := connect.WithInterceptors(NewAuthInterceptor(cfg.Auth, WithAuthLogger(logger)))

	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.handleHealth)
	mux.HandleFunc(ReadyPath, s.handleReady)
	mux.Handle(graphv1connect.NewIngestServiceHandler(
		NewIngestService(cfg.Projector, cfg.Metrics, logger), interceptors))
	mux.Handle(graphv1connect.NewQueryServiceHandler(
		NewQueryService(cfg.Projector, cfg.Metrics, logger), interceptors))
	mux.Handle(graphv1connect.NewResolutionServiceHandler(
		NewResolutionService(cfg.Projector, cfg.Metrics, logger), interceptors))
	if cfg.Investigation != nil {
		mux.Handle(investigationv1connect.NewInvestigationServiceHandler(cfg.Investigation, interceptors))
	}

	// otelhttp wraps the mux so every RPC gets a server span (FR-051). The health endpoints are
	// filtered out: a probe every second would otherwise be the loudest thing in the trace
	// backend and say nothing about the graph.
	s.handler = otelhttp.NewHandler(mux, "sre-agent",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != HealthPath && r.URL.Path != ReadyPath
		}),
	)

	listener, err := net.Listen("tcp", listenAddr(cfg.Listen))
	if err != nil {
		return nil, fmt.Errorf("server: listen on %s: %w", listenAddr(cfg.Listen), err)
	}
	s.listener = listener
	s.http = &http.Server{
		Handler: s.handler,
		// HTTP/1.1 for `curl` and the Connect protocol, cleartext HTTP/2 for gRPC. TLS is
		// terminated by the ingress in every deployment this targets, and a graph that needed
		// certificates before a feeder could speak gRPC would not be runnable on a laptop.
		Protocols: unencryptedHTTP2(),
		// No ReadTimeout or WriteTimeout: the ingestion stream (FR-017) is long-lived by
		// design and either one would cut it. The header timeout still bounds a client that
		// connects and says nothing.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// unencryptedHTTP2 is the protocol set the listener accepts: HTTP/1.1, plus HTTP/2 with no TLS
// both by prior knowledge (what a gRPC client does) and by upgrade.
func unencryptedHTTP2() *http.Protocols {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return protocols
}

func listenAddr(listen string) string {
	if listen == "" {
		return DefaultListen
	}
	return listen
}

// Addr is the address actually bound, which is how a caller discovers the port after asking
// for `:0`.
func (s *Server) Addr() string {
	if s.listener == nil {
		return listenAddr(s.cfg.Listen)
	}
	return s.listener.Addr().String()
}

// Handler is the fully wrapped handler, for an in-process test that would rather not dial.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve accepts connections until ctx is cancelled, then drains in-flight calls and returns.
// It returns nil on a graceful shutdown.
func (s *Server) Serve(ctx context.Context) error {
	// The `human` source is declared up front, so that a decision taken a second after startup
	// is appended rather than refused for an unregistered source (FR-018). It is best-effort:
	// ResolutionService registers it again, idempotently, before the first decision, so a
	// database that is slow or a context that is already going away must not stop the server
	// coming up — the health endpoints exist to report that condition.
	if err := RegisterHumanSource(ctx, s.cfg.Projector); err != nil {
		s.logger.Warn("could not declare the human decision source at startup; "+
			"it will be declared before the first decision", "error", err.Error())
	}

	// The two observable gauges — suggestions_pending and feeder_lag_seconds — are questions
	// about the graph, not counters, so they are answered on every metric collection by reading
	// it (FR-051, FR-052). Registering here rather than in New means a Server that was built and
	// never served does not leave a callback holding a pool it will not use.
	s.cfg.Projector.RegisterObservables(s.cfg.Metrics)

	errCh := make(chan error, 1)
	go func() {
		err := s.http.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	s.logger.Info("listening", "addr", s.Addr())

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// The shutdown deadline must not inherit the cancelled context, or Shutdown would return
	// immediately and kill the calls it is supposed to drain.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout())
	defer cancel()

	s.logger.Info("shutting down", "addr", s.Addr(), "timeout", s.shutdownTimeout())
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// A timed-out drain is reported, not hidden: an operator needs to know the process
		// cut an ingestion stream.
		s.logger.Warn("graceful shutdown did not finish", "error", err.Error())
		_ = s.http.Close()
	}
	return <-errCh
}

// Close releases the listener without serving. It is for a caller that built a Server and then
// failed before Serve; after Serve has returned it is a no-op.
func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	err := s.http.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) shutdownTimeout() time.Duration {
	if s.cfg.ShutdownTimeout > 0 {
		return s.cfg.ShutdownTimeout
	}
	return DefaultShutdownTimeout
}

// handleHealth answers the liveness probe: the process is up and its database answers.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	if err := s.cfg.Projector.Store().Pool().Ping(ctx); err != nil {
		s.logger.Warn("health check failed", "error", err.Error())
		writePlain(w, http.StatusServiceUnavailable, "database unavailable\n")
		return
	}
	writePlain(w, http.StatusOK, "ok\n")
}

// handleReady answers the readiness probe: the database is reachable *and* migrated, so the
// process can actually serve.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	store := s.cfg.Projector.Store()
	if err := store.Pool().Ping(ctx); err != nil {
		s.logger.Warn("readiness check failed", "error", err.Error())
		writePlain(w, http.StatusServiceUnavailable, "database unavailable\n")
		return
	}
	versions, err := store.AppliedVersions(ctx)
	if err != nil || len(versions) == 0 {
		s.logger.Warn("readiness check failed: schema not migrated")
		writePlain(w, http.StatusServiceUnavailable, "schema not migrated; run `aisre migrate`\n")
		return
	}
	writePlain(w, http.StatusOK, "ready\n")
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
