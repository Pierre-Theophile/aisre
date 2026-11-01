// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Streamable HTTP (010 FR-003, FR-012, research R2).
//
// The handler runs in the SDK's stateless mode: every request is authenticated by its own bearer
// token, which is forwarded to the API for every call that request makes and dropped with it. A
// session id would otherwise be a second credential that outlives the token that opened it. The
// handler holds no credential of its own and logs none.

// HTTPOptions configure the streamable HTTP handler.
type HTTPOptions struct {
	// APIURL is the aisre API's base URL.
	APIURL string
	// Transport is the round tripper towards the API (nil is http.DefaultTransport).
	Transport http.RoundTripper
	// Profile, Version, Logger and Running are as in Options.
	Profile Profile
	Version string
	Logger  *slog.Logger
	Running *RunningLimit
}

// NewHTTPHandler serves MCP over streamable HTTP.
func NewHTTPHandler(o HTTPOptions) http.Handler {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="aisre"`)
			http.Error(w, "a bearer token is required: every call is made on behalf of an individual", http.StatusUnauthorized)
			return
		}
		api := Connect(o.APIURL, o.Transport, func() (string, error) { return token, nil })
		caller, err := api.WhoAmI(r.Context())
		switch {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="aisre", error="invalid_token"`)
			http.Error(w, "the aisre API did not accept the token", http.StatusUnauthorized)
			return
		case err != nil:
			logger.Warn("mcp: identity lookup failed", "err", err)
			http.Error(w, "the aisre API could not be reached", http.StatusBadGateway)
			return
		}
		server, err := New(Options{
			Clients: api.Clients, Roles: caller.Roles, Subject: caller.Issuer + "|" + caller.Subject,
			Profile: o.Profile, Version: o.Version, Logger: logger, Running: o.Running,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{Stateless: true, Logger: logger}).ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// CheckListen refuses a listener that would receive bearer tokens in clear from beyond this host
// (010 FR-003): a non-loopback address needs TLS.
func CheckListen(addr string, tls bool) error {
	if tls {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("mcp: listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("mcp: refusing to listen on %s without TLS: clients send their bearer tokens to "+
		"this listener; pass --tls-cert and --tls-key, or listen on 127.0.0.1", addr)
}
