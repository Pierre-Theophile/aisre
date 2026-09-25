// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// run executes the command tree with args and returns stdout, stderr and the exit code the
// binary would have reported. It is the closest a test can get to typing the command.
func run(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	root := NewRootCommand()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)

	err := root.ExecuteContext(ctx)
	if err != nil {
		// Execute prints the failure before exiting; the test sees the same two channels a
		// person at a terminal would.
		fmt.Fprintln(&errOut, "sre-agent:", err)
	}
	return out.String(), errOut.String(), ExitCode(err)
}

// The exit codes are a contract with whatever pipeline wraps this binary, so they are asserted
// directly rather than inferred from a command's behaviour (contracts/cli.md §Exit codes).
func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"usage", exitErrorf(ExitUsage, "bad flag"), ExitUsage},
		{"transport", exitErrorf(ExitTransport, "connection refused"), ExitTransport},
		{"auth", exitErrorf(ExitAuth, "no token"), ExitAuth},
		{"verification", exitErrorf(ExitVerification, "golden mismatch"), ExitVerification},
		{"rejected", exitErrorf(ExitRejected, "event refused"), ExitRejected},
		{
			"wrapped exit error",
			errors.Join(errors.New("context"), exitErrorf(ExitVerification, "inner")),
			ExitVerification,
		},
		{
			"connect unauthenticated",
			connect.NewError(connect.CodeUnauthenticated, errors.New("no token")),
			ExitAuth,
		},
		{
			"connect permission denied",
			connect.NewError(connect.CodePermissionDenied, errors.New("not a decider")),
			ExitAuth,
		},
		{
			"connect unavailable",
			connect.NewError(connect.CodeUnavailable, errors.New("connection refused")),
			ExitTransport,
		},
		{
			"connect unimplemented",
			connect.NewError(connect.CodeUnimplemented, errors.New("not built yet")),
			ExitTransport,
		},
		{"untagged error is cobra's own usage failure", errors.New("unknown command"), ExitUsage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// remoteError is what turns an RPC failure into one of those codes, so the translation is
// checked too.
func TestRemoteErrorCodes(t *testing.T) {
	cases := []struct {
		code connect.Code
		want int
	}{
		{connect.CodeUnauthenticated, ExitAuth},
		{connect.CodePermissionDenied, ExitAuth},
		{connect.CodeUnimplemented, ExitTransport},
		{connect.CodeUnavailable, ExitTransport},
		{connect.CodeInternal, ExitTransport},
	}
	for _, tc := range cases {
		err := remoteError("extent", connect.NewError(tc.code, errors.New("boom")))
		if got := ExitCode(err); got != tc.want {
			t.Errorf("remoteError(%v): exit %d, want %d (%v)", tc.code, got, tc.want, err)
		}
	}
}

func TestBadOutputFormatIsUsageError(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "--output", "yaml", "version")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080":  "http://localhost:8080",
		"localhost:8080":         "http://localhost:8080",
		"http://localhost:8080/": "http://localhost:8080",
		"https://graph.example":  "https://graph.example",
	}
	for input, want := range cases {
		got, err := normalizeServerURL(input)
		if err != nil {
			t.Errorf("normalizeServerURL(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := normalizeServerURL("  "); ExitCode(err) != ExitUsage {
		t.Errorf("empty --server: exit %d, want %d", ExitCode(err), ExitUsage)
	}
}

// The minted token must be accepted by the very provider `serve --auth dev --dev` builds —
// the two halves agree on the published key or the quickstart does not work.
func TestDevTokenIsAcceptedByTheDevAuthenticator(t *testing.T) {
	stdout, stderr, code := run(t, context.Background(),
		"dev-token", "--dev", "--user", "alice", "--roles", "r,d")
	if code != ExitOK {
		t.Fatalf("dev-token: exit %d (stderr %q)", code, stderr)
	}
	token := strings.TrimSpace(stdout)
	if token == "" {
		t.Fatal("dev-token printed nothing")
	}

	auth := devAuthenticator(t)
	principal, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("the dev authenticator refused a token this binary minted: %v", err)
	}
	if principal.Subject != "alice" {
		t.Errorf("subject %q, want alice", principal.Subject)
	}
	if !principal.Has(server.RoleReader) || !principal.Has(server.RoleDecider) {
		t.Errorf("roles %v, want reader and decider", principal.Roles)
	}
	if principal.Has(server.RoleFeeder) {
		t.Error("a token minted with --roles r,d carries the feeder role")
	}
}

func TestDevTokenScopesAFeederToOneSource(t *testing.T) {
	stdout, stderr, code := run(t, context.Background(),
		"dev-token", "--dev", "--user", "otel-feeder", "--roles", "f", "--source-id", "otel:demo")
	if code != ExitOK {
		t.Fatalf("dev-token: exit %d (stderr %q)", code, stderr)
	}

	principal, err := devAuthenticator(t).Authenticate(context.Background(), strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("authenticate feeder token: %v", err)
	}
	if principal.SourceID != "otel:demo" {
		t.Errorf("source_id %q, want otel:demo", principal.SourceID)
	}
}

// A feeder token with no source would be a feeder that can write for anyone (FR-046).
func TestDevTokenRefusesUnscopedFeeder(t *testing.T) {
	_, _, code := run(t, context.Background(), "dev-token", "--dev", "--user", "rogue", "--roles", "f")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
}

func TestDevTokenRequiresDevFlag(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "dev-token", "--user", "alice", "--roles", "r")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--dev") {
		t.Errorf("stderr %q does not mention --dev", stderr)
	}
}

func TestDevTokenJSONOutput(t *testing.T) {
	stdout, _, code := run(t, context.Background(),
		"--output", "json", "dev-token", "--dev", "--user", "alice", "--roles", "reader")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	var payload struct {
		Token  string   `json:"token"`
		User   string   `json:"user"`
		Issuer string   `json:"issuer"`
		Roles  []string `json:"roles"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if payload.User != "alice" || payload.Issuer != server.DefaultDevIssuer {
		t.Errorf("unexpected payload %+v", payload)
	}
	if len(payload.Roles) != 1 || payload.Roles[0] != string(server.RoleReader) {
		t.Errorf("roles %v, want [reader]", payload.Roles)
	}
}

// discardLogger keeps the dev provider's (deliberately loud) startup banner out of test output.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func devAuthenticator(t *testing.T) *server.DevAuthenticator {
	t.Helper()
	auth, err := server.NewDevAuthenticator(server.DevConfig{Enabled: true, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	return auth
}

// TestServeAnswersHealthz runs `serve` in-process against a real database, exactly as the
// quickstart's first step does, and checks the two things that step promises: the process
// listens, and `curl :8080/healthz` returns 200 with no credential.
func TestServeAnswersHealthz(t *testing.T) {
	store := pgtest.Open(t)
	dsn := store.Pool().Config().ConnString()

	addrCh := make(chan string, 1)
	previous := serveReady
	serveReady = func(addr string) { addrCh <- addr }
	t.Cleanup(func() { serveReady = previous })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		_, _, code := run(t, ctx,
			"--log-level", "error",
			"serve",
			"--db", dsn,
			"--listen", "127.0.0.1:0",
			"--auth", "dev", "--dev",
			"--migrate",
		)
		done <- code
	}()

	var addr string
	select {
	case addr = <-addrCh:
	case code := <-done:
		cancel()
		t.Fatalf("serve exited before listening, code %d", code)
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatal("serve did not start listening")
	}

	baseURL := "http://" + addr
	assertHealthy(t, baseURL+"/healthz")
	assertHealthy(t, baseURL+"/readyz")

	// The same running server answers the CLI. `extent` is implemented (T033), so a reader
	// token gets an answer: an empty graph has no observed span and no checkpoints, which is a
	// result, not an error.
	token := mintReaderToken(t)
	_, stderr, code := run(t, context.Background(),
		"extent", "--server", baseURL, "--token", token)
	if code != ExitOK {
		t.Errorf("extent against a live server: exit %d, want %d (stderr %q)", code, ExitOK, stderr)
	}

	// Without a token the same call must be an authentication failure instead.
	_, _, code = run(t, context.Background(), "extent", "--server", baseURL, "--token", "")
	if code != ExitAuth {
		t.Errorf("anonymous extent: exit %d, want %d", code, ExitAuth)
	}

	// The loop the quickstart closes: a token this binary minted is accepted by the server
	// this binary started, over the JSON protocol a third-party feeder would use.
	assertFeederCanIngest(t, baseURL)

	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("serve exited with %d, want a clean shutdown", code)
		}
	case <-time.After(30 * time.Second):
		t.Error("serve did not shut down when its context was cancelled")
	}
}

func assertHealthy(t *testing.T, url string) {
	t.Helper()

	var lastErr error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return
		}
		lastErr = errors.New(resp.Status + ": " + string(body))
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned 200: %v", url, lastErr)
}

func mintReaderToken(t *testing.T) string {
	t.Helper()
	stdout, stderr, code := run(t, context.Background(),
		"dev-token", "--dev", "--user", "alice", "--roles", "r")
	if code != ExitOK {
		t.Fatalf("dev-token: exit %d (stderr %q)", code, stderr)
	}
	return strings.TrimSpace(stdout)
}

func TestServeRejectsDevAuthWithoutDevFlag(t *testing.T) {
	_, stderr, code := run(t, context.Background(),
		"serve", "--db", "postgres://invalid/invalid", "--auth", "dev")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
}

func TestServeRejectsOIDCWithoutIssuer(t *testing.T) {
	_, _, code := run(t, context.Background(), "serve", "--db", "postgres://invalid/invalid", "--auth", "oidc")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
}

func TestMigrateAppliesEverySchemaVersion(t *testing.T) {
	store := pgtest.Open(t)
	dsn := store.Pool().Config().ConnString()

	stdout, stderr, code := run(t, context.Background(), "migrate", "--db", dsn)
	if code != ExitOK {
		t.Fatalf("migrate: exit %d (stderr %q)", code, stderr)
	}
	// pgtest already migrated the database, so every migration reports as already present.
	if !strings.Contains(stdout, "already present") {
		t.Errorf("migrate output %q does not report the migrations it found", stdout)
	}
}

// `fixture load` is the quickstart's second step and the only path that may restore recorded
// observed times (FR-023, FR-047). It runs against the database, not the server.
func TestFixtureLoadAppliesTheBaselineFixture(t *testing.T) {
	store := pgtest.Open(t)
	dsn := store.Pool().Config().ConnString()

	stdout, stderr, code := run(t, context.Background(),
		"fixture", "load", "--db", dsn, "../../fixtures/baseline-topology-01")
	if code != ExitOK {
		t.Fatalf("fixture load: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "the rejection contract held") {
		t.Errorf("fixture load output does not report the rejection contract:\n%s", stdout)
	}

	// Loading the same fixture again must change nothing: every event is a duplicate by its
	// idempotency key (FR-020).
	stdout, stderr, code = run(t, context.Background(),
		"--output", "json", "fixture", "load", "--db", dsn, "../../fixtures/baseline-topology-01")
	if code != ExitOK {
		t.Fatalf("second fixture load: exit %d (stderr %s)", code, stderr)
	}
	var report struct {
		Applied       int `json:"applied"`
		DuplicateNoop int `json:"duplicate_noop"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode load report %s: %v", stdout, err)
	}
	if report.Applied != 0 || report.DuplicateNoop == 0 {
		t.Errorf("re-loading applied %d and no-opped %d events; want 0 applied",
			report.Applied, report.DuplicateNoop)
	}
}

// `fixture verify` is what `make verify` and CI run. The goldens are skipped because the query
// layer that produces them has not landed (T031+); the replay, double-delivery and shuffle
// steps are real.
func TestFixtureVerifyShippedFixtures(t *testing.T) {
	store := pgtest.Open(t)
	dsn := store.Pool().Config().ConnString()

	stdout, stderr, code := run(t, context.Background(),
		"fixture", "verify", "--db", dsn, "--skip-goldens",
		"--shuffles", "2", "--seed", "1",
		"../../fixtures/baseline-topology-01", "../../fixtures/late-arriving-fact-01")
	if code != ExitOK {
		t.Fatalf("fixture verify: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	for _, id := range []string{"baseline-topology-01", "late-arriving-fact-01"} {
		if !strings.Contains(stdout, id) {
			t.Errorf("the report does not mention %s:\n%s", id, stdout)
		}
	}

	// The JSON rendering is the fixture package's own, one document per fixture.
	stdout, stderr, code = run(t, context.Background(),
		"--output", "json", "fixture", "verify", "--db", dsn, "--skip-goldens", "--shuffles", "0",
		"../../fixtures/baseline-topology-01")
	if code != ExitOK {
		t.Fatalf("fixture verify --output json: exit %d (stderr %s)", code, stderr)
	}
	var report struct {
		FixtureID string `json:"fixture_id"`
		Passed    bool   `json:"passed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &report); err != nil {
		t.Fatalf("decode verify report %s: %v", stdout, err)
	}
	if report.FixtureID != "baseline-topology-01" || !report.Passed {
		t.Errorf("unexpected report %+v", report)
	}
}

// A fixture directory that does not exist is a broken invocation, not a failed verification.
func TestFixtureVerifyMissingDirIsNotAVerificationFailure(t *testing.T) {
	store := pgtest.Open(t)
	dsn := store.Pool().Config().ConnString()

	_, _, code := run(t, context.Background(),
		"fixture", "verify", "--db", dsn, "--skip-goldens", "../../fixtures/does-not-exist")
	if code != ExitTransport {
		t.Fatalf("exit %d, want %d", code, ExitTransport)
	}
}

// assertFeederCanIngest registers a source and applies one event over plain JSON, with a
// feeder token minted by `dev-token`.
func assertFeederCanIngest(t *testing.T, baseURL string) {
	t.Helper()

	stdout, stderr, code := run(t, context.Background(),
		"dev-token", "--dev", "--user", "otel-feeder", "--roles", "f", "--source-id", "otel:demo")
	if code != ExitOK {
		t.Fatalf("dev-token: exit %d (stderr %q)", code, stderr)
	}
	token := strings.TrimSpace(stdout)

	post := func(procedure, body string) (int, string) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			baseURL+procedure, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", procedure, err)
		}
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(payload)
	}

	status, body := post("/sreagent.graph.v1.IngestService/RegisterSource",
		`{"sourceId":"otel:demo","kind":"otel","ordering":"none","schemaVersion":"1.0.0"}`)
	if status != http.StatusOK {
		t.Fatalf("register source: %d %s", status, body)
	}

	status, body = post("/sreagent.graph.v1.IngestService/IngestBatch", `{"events":[{
		"eventId":"otel:demo:svc:checkout@cli-test",
		"idempotencyKey":"otel:demo:svc:checkout@cli-test",
		"sourceId":"otel:demo",
		"schemaVersion":"1.0.0",
		"upsertNode":{
			"ref":{"namespace":"otel.service.name","value":"checkout"},
			"type":"SERVICE",
			"displayName":"checkout",
			"validAt":"2026-09-01T13:00:00Z"
		}
	}]}`)
	if status != http.StatusOK {
		t.Fatalf("ingest batch: %d %s", status, body)
	}
	if !strings.Contains(body, `"APPLIED"`) {
		t.Errorf("ingest batch did not apply the event: %s", body)
	}

	// A token scoped to otel:demo may not write for anybody else (FR-046).
	status, body = post("/sreagent.graph.v1.IngestService/RegisterSource",
		`{"sourceId":"k8s:demo","kind":"k8s"}`)
	if status != http.StatusForbidden {
		t.Errorf("registering a foreign source: %d %s, want 403", status, body)
	}
}
