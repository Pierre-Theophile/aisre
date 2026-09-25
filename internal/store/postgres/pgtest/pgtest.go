// SPDX-License-Identifier: Apache-2.0

// Package pgtest hands tests a migrated PostgreSQL database.
//
// It lives beside the store rather than inside it so that production code never links
// testcontainers or embedded-postgres: only test binaries import this package.
//
// A server is chosen once per process, in this order:
//
//  1. PG_DSN is set — an already-running server, used as is. Fastest, and what CI uses.
//  2. Docker is reachable — a postgres:16 container via testcontainers-go.
//  3. Otherwise — embedded-postgres on a free loopback port. The PostgreSQL 16 archive is
//     downloaded once into $XDG_CACHE_HOME/sre-agent/embedded-postgres (~/Library/Caches on
//     macOS) and reused by every later run.
//
// Set SRE_AGENT_TEST_PG to "dsn", "docker" or "embedded" to force one of them.
//
// Every call to Open creates its own database on that shared server and drops it at test
// cleanup, so tests never see each other's rows and can run in parallel.
//
// Add this to any package whose tests call Open, so the shared server is started once and
// stopped once instead of per test:
//
//	func TestMain(m *testing.M) { pgtest.TestMain(m) }
package pgtest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// containerImage is the PostgreSQL the Docker path runs. It must match the version the
// embedded path downloads, so that a constraint that passes locally passes in CI.
const containerImage = "postgres:16-alpine"

// startTimeout bounds first-run behaviour: the embedded path may have to download and extract
// ~50 MB of binaries, and the Docker path may have to pull an image.
const startTimeout = 5 * time.Minute

// Open returns a Store on a database of its own, already migrated, and registers the cleanup
// that drops it. The returned Store is closed for you when the test ends.
func Open(t *testing.T) *postgres.Store {
	t.Helper()

	srv := acquire(t)
	ctx := context.Background()

	name := databaseName(t)
	if err := adminExec(ctx, srv.adminDSN, fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		// WITH (FORCE) terminates leftover backends; without it a leaked connection would
		// make the drop hang and the next run collide with the stale database.
		drop := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name))
		if err := adminExec(context.Background(), srv.adminDSN, drop); err != nil {
			t.Logf("pgtest: drop database %s: %v", name, err)
		}
	})

	dsn, err := withDatabase(srv.adminDSN, name)
	if err != nil {
		t.Fatalf("pgtest: build dsn: %v", err)
	}
	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: open store: %v", err)
	}
	t.Cleanup(store.Close)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("pgtest: migrate: %v", err)
	}
	return store
}

// Backend reports which server the process is using: "dsn", "docker" or "embedded". It starts
// the server if it is not running yet, so it is only useful from inside a test.
func Backend(t *testing.T) string {
	t.Helper()
	return acquire(t).kind
}

// TestMain keeps the shared server alive for the whole test binary and shuts it down
// afterwards. Call it from the package's own TestMain:
//
//	func TestMain(m *testing.M) { pgtest.TestMain(m) }
func TestMain(m *testing.M) {
	shared.mu.Lock()
	shared.keepAlive = true
	shared.mu.Unlock()

	code := m.Run()
	Shutdown()
	os.Exit(code)
}

// Shutdown stops the shared server if one is running. TestMain calls it; a test binary that
// does not use TestMain does not need it, because the last Open to finish stops the server.
func Shutdown() {
	shared.mu.Lock()
	srv := shared.srv
	shared.srv = nil
	shared.refs = 0
	shared.mu.Unlock()

	if srv != nil && srv.stop != nil {
		srv.stop()
	}
}

// server is a running PostgreSQL the whole test binary shares.
type server struct {
	// adminDSN points at the maintenance database ("postgres"); per-test databases are
	// created from it and addressed by swapping the database name.
	adminDSN string
	kind     string
	stop     func()
}

var shared struct {
	mu        sync.Mutex
	srv       *server
	refs      int
	keepAlive bool
}

// acquire returns the shared server, starting it on first use, and arranges for it to be
// stopped when the last test using it finishes (unless TestMain is holding it open).
func acquire(t *testing.T) *server {
	t.Helper()

	shared.mu.Lock()
	if shared.srv == nil {
		srv, err := startServer()
		if err != nil {
			shared.mu.Unlock()
			t.Fatalf("pgtest: start postgres: %v", err)
		}
		shared.srv = srv
	}
	srv := shared.srv
	shared.refs++
	shared.mu.Unlock()

	t.Cleanup(func() {
		shared.mu.Lock()
		shared.refs--
		stop := shared.refs <= 0 && !shared.keepAlive && shared.srv != nil
		if stop {
			srv := shared.srv
			shared.srv = nil
			shared.mu.Unlock()
			if srv.stop != nil {
				srv.stop()
			}
			return
		}
		shared.mu.Unlock()
	})
	return srv
}

func startServer() (*server, error) {
	switch backendChoice() {
	case "dsn":
		dsn := os.Getenv("PG_DSN")
		if dsn == "" {
			return nil, fmt.Errorf("pgtest: SRE_AGENT_TEST_PG=dsn but PG_DSN is empty")
		}
		return &server{adminDSN: dsn, kind: "dsn"}, nil
	case "docker":
		return startContainer()
	default:
		return startEmbedded()
	}
}

// backendChoice resolves the documented precedence, honouring the SRE_AGENT_TEST_PG override.
func backendChoice() string {
	if forced := strings.TrimSpace(os.Getenv("SRE_AGENT_TEST_PG")); forced != "" {
		return forced
	}
	if os.Getenv("PG_DSN") != "" {
		return "dsn"
	}
	if dockerReachable() {
		return "docker"
	}
	return "embedded"
}

// dockerReachable probes for a usable Docker endpoint without importing the Docker client, so
// that a machine with no daemon falls through to embedded-postgres quickly instead of waiting
// on a connection timeout deep inside testcontainers.
func dockerReachable() bool {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		if strings.HasPrefix(host, "unix://") {
			return dialable("unix", strings.TrimPrefix(host, "unix://"))
		}
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			return dialable("tcp", u.Host)
		}
		return false
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/var/run/docker.sock",
		filepath.Join(home, ".docker/run/docker.sock"),
		filepath.Join(home, ".colima/default/docker.sock"),
		filepath.Join(home, ".rd/docker.sock"),
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		candidates = append(candidates, filepath.Join(runtimeDir, "docker.sock"))
	}
	for _, sock := range candidates {
		if dialable("unix", sock) {
			return true
		}
	}
	return false
}

func dialable(network, address string) bool {
	conn, err := net.DialTimeout(network, address, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func startContainer() (*server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	container, err := tcpostgres.Run(ctx, containerImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", containerImage, err)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, fmt.Errorf("connection string: %w", err)
	}
	return &server{
		adminDSN: dsn,
		kind:     "docker",
		stop:     func() { _ = testcontainers.TerminateContainer(container) },
	}, nil
}

func startEmbedded() (*server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}

	// The archive and the extracted binaries live in a stable cache so only the first run on
	// a machine pays the download; the runtime and data directories are throwaway, because
	// embedded-postgres wipes the runtime path on every Start.
	cacheDir, err := binariesCacheDir()
	if err != nil {
		return nil, err
	}
	runtimeDir, err := os.MkdirTemp("", "sre-agent-pg-")
	if err != nil {
		return nil, fmt.Errorf("create runtime directory: %w", err)
	}
	serverLog := &syncBuffer{}

	db := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Username("postgres").
		Password("postgres").
		Database("postgres").
		Port(uint32(port)).
		CachePath(cacheDir).
		BinariesPath(filepath.Join(cacheDir, "binaries")).
		RuntimePath(filepath.Join(runtimeDir, "runtime")).
		DataPath(filepath.Join(runtimeDir, "data")).
		StartTimeout(startTimeout).
		Logger(serverLog))

	if err := db.Start(); err != nil {
		_ = os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("start embedded postgres on port %d: %w\npostgres log:\n%s",
			port, err, serverLog.String())
	}

	dsn := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	return &server{
		adminDSN: dsn,
		kind:     "embedded",
		stop: func() {
			_ = db.Stop()
			_ = os.RemoveAll(runtimeDir)
		},
	}, nil
}

// binariesCacheDir is where the PostgreSQL archive and its extracted binaries are kept
// between runs: $XDG_CACHE_HOME/sre-agent/embedded-postgres, or the platform equivalent.
func binariesCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "sre-agent", "embedded-postgres")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory %s: %w", dir, err)
	}
	return dir, nil
}

// syncBuffer collects the server's own log so a start failure can report the postmaster's
// reason instead of only a timeout. It is not a *testing.T writer on purpose: the server
// outlives the test that started it, and logging to a finished test panics.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

var dbCounter atomic.Int64

// databaseName derives a unique, legal database name from the test name. PostgreSQL caps
// identifiers at 63 bytes, so the readable part is truncated and a counter keeps it unique.
func databaseName(t *testing.T) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, t.Name())
	if len(safe) > 32 {
		safe = safe[:32]
	}
	return fmt.Sprintf("sreagent_%s_%d_%d", safe, os.Getpid(), dbCounter.Add(1))
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// adminExec runs one statement on the maintenance database. CREATE/DROP DATABASE cannot run
// inside a transaction, so this deliberately uses a bare connection rather than the store.
func adminExec(ctx context.Context, dsn, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec %q: %w", sql, err)
	}
	return nil
}

// withDatabase rewrites a DSN to point at another database, supporting both the URL form
// ("postgres://...") and the keyword/value form ("host=... dbname=...").
func withDatabase(dsn, name string) (string, error) {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String(), nil
	}
	fields := strings.Fields(dsn)
	rewritten := make([]string, 0, len(fields)+1)
	for _, field := range fields {
		if strings.HasPrefix(field, "dbname=") {
			continue
		}
		rewritten = append(rewritten, field)
	}
	if len(rewritten) == 0 {
		return "", fmt.Errorf("pgtest: cannot rewrite dsn %q", dsn)
	}
	return strings.Join(append(rewritten, "dbname="+name), " "), nil
}
