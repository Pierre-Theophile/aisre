// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
)

// Talking to the server (FR-035, FR-041a).
//
// The credential is attached by a RoundTripper rather than a Connect interceptor so that it
// covers streams as well as unary calls: an ingestion stream is one HTTP request, and a
// per-call interceptor would not reach it.
//
// The clients speak the Connect protocol over HTTP/1.1. The server answers Connect, gRPC and
// gRPC-Web on the same routes, and Connect over HTTP/1.1 is the variant that survives every
// proxy an operator's laptop sits behind. Feeders, which need bidirectional streaming, use
// gRPC over h2c instead (pkg/feeder).

// clientTimeout bounds a single CLI call. Queries have a budget of a few seconds
// (research §2); a minute is generous enough to cover a cold cache and short enough that a
// hung command fails instead of hanging a pipeline.
const clientTimeout = 60 * time.Second

// bearerTransport attaches the caller's credential to every request.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		// The request must not be mutated in place: net/http may retry it.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// clientSet is one connected client per service.
type clientSet struct {
	ingest     graphv1connect.IngestServiceClient
	query      graphv1connect.QueryServiceClient
	resolution graphv1connect.ResolutionServiceClient
}

// newClients builds the clients for the configured server and token.
func newClients(opts *globalOptions) (*clientSet, error) {
	base, err := normalizeServerURL(opts.Server)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Transport: &bearerTransport{base: http.DefaultTransport, token: opts.Token},
		Timeout:   clientTimeout,
	}
	return &clientSet{
		ingest:     graphv1connect.NewIngestServiceClient(httpClient, base),
		query:      graphv1connect.NewQueryServiceClient(httpClient, base),
		resolution: graphv1connect.NewResolutionServiceClient(httpClient, base),
	}, nil
}

// normalizeServerURL accepts `host:port` as well as a full URL, because that is what people
// type.
func normalizeServerURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", exitErrorf(ExitUsage, "--server is empty")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", exitErrorf(ExitUsage, "--server %q is not a URL: %v", raw, err)
	}
	if parsed.Host == "" {
		return "", exitErrorf(ExitUsage, "--server %q names no host", raw)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// remoteError turns an RPC failure into an error carrying the exit code contracts/cli.md
// promises, with a message that says what the caller should do about it.
func remoteError(what string, err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return exitErrorf(ExitTransport, "%s: %v", what, err)
	}
	switch connectErr.Code() {
	case connect.CodeUnauthenticated:
		return exitErrorf(ExitAuth,
			"%s: not authenticated (%v). Set $%s, or pass --token; `aisre dev-token --dev --user <you> --roles r` mints one against a dev server.",
			what, connectErr.Message(), EnvToken)
	case connect.CodePermissionDenied:
		return exitErrorf(ExitAuth,
			"%s: your token does not carry the role this call needs (%v)", what, connectErr.Message())
	case connect.CodeUnimplemented:
		return exitErrorf(ExitTransport,
			"%s: the server does not implement this call yet (%v). It arrives with the query phase; the server and CLI must be the same version.",
			what, connectErr.Message())
	case connect.CodeUnavailable:
		return exitErrorf(ExitTransport,
			"%s: cannot reach the server (%v). Is `aisre serve` running and is --server correct?",
			what, connectErr.Message())
	default:
		return exitErrorf(ExitTransport, "%s: %s: %s", what, connectErr.Code(), connectErr.Message())
	}
}

// tokenFrom resolves the bearer token from the flag, falling back to the environment.
//
// It exists so that the environment is read when the command runs rather than when the flag is
// declared: cobra renders a flag's default into `--help`, so a token wired in as a default is a
// token printed to anyone who asks for help.
func tokenFrom(flagValue, envValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	return envValue
}

// dsnFrom resolves the database DSN from the flag, falling back to the environment.
func dsnFrom(flagValue, envValue string) (string, error) {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue, nil
	}
	if strings.TrimSpace(envValue) != "" {
		return envValue, nil
	}
	return "", exitErrorf(ExitUsage,
		"no database: pass --db or set $%s (e.g. postgres://sreagent:sreagent@localhost:5432/sreagent?sslmode=disable)",
		EnvDSN)
}

// storeError tags a database failure. Opening or migrating a database is a server-side
// failure from the CLI's point of view, so it exits 2.
func storeError(what string, err error) error {
	return exitWith(ExitTransport, fmt.Errorf("%s: %w", what, err))
}
