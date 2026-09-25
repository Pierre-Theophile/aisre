// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The fail-closed matrix of `serve` (T090, constitution VII, FR-041a, FR-046).
//
// Each case here is a configuration that would start a server accepting identities it should
// not. The test asserts the process refuses to start, and exits 1 while doing it: an operator
// reading a pipeline status has to be able to tell a misconfiguration from an outage.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestServeAuthFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		opts    serveOptions
		wantErr string
	}{
		{
			name:    "dev auth without --dev",
			opts:    serveOptions{authMode: authModeDev, listen: "127.0.0.1:8080"},
			wantErr: "--dev",
		},
		{
			name:    "dev auth on every interface",
			opts:    serveOptions{authMode: authModeDev, dev: true, listen: ":8080"},
			wantErr: "--dev-insecure-listen",
		},
		{
			name:    "dev auth on a routable address",
			opts:    serveOptions{authMode: authModeDev, dev: true, listen: "10.0.0.7:8080"},
			wantErr: "--dev-insecure-listen",
		},
		{
			name:    "dev auth on 0.0.0.0",
			opts:    serveOptions{authMode: authModeDev, dev: true, listen: "0.0.0.0:8080"},
			wantErr: "--dev-insecure-listen",
		},
		{
			name: "a users file does not make a routable dev listener safe",
			opts: serveOptions{
				authMode: authModeDev, dev: true, listen: "10.0.0.7:8080", devUsers: "users.yaml",
			},
			wantErr: "--dev-insecure-listen",
		},
		{
			name:    "oidc without an issuer",
			opts:    serveOptions{authMode: authModeOIDC, oidcAudience: "sre-agent"},
			wantErr: "--oidc-issuer",
		},
		{
			name:    "oidc without an audience and without the opt-out",
			opts:    serveOptions{authMode: authModeOIDC, oidcIssuer: "https://login.example.com"},
			wantErr: "--oidc-audience",
		},
		{
			name: "oidc with both an audience and the opt-out",
			opts: serveOptions{
				authMode: authModeOIDC, oidcIssuer: "https://login.example.com",
				oidcAudience: "sre-agent", oidcSkipAudience: true,
			},
			wantErr: "contradict",
		},
		{
			name: "a plaintext issuer outside development",
			opts: serveOptions{
				authMode: authModeOIDC, oidcIssuer: "http://login.example.com", oidcAudience: "sre-agent",
			},
			wantErr: "not https",
		},
		{
			name:    "an unknown auth mode",
			opts:    serveOptions{authMode: "none", listen: "127.0.0.1:8080"},
			wantErr: "--auth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			_, err := newAuthenticator(context.Background(), &opts, quietLogger())
			if err == nil {
				t.Fatal("the server must refuse to start")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("exit code = %d, want %d (a usage error)", got, ExitUsage)
			}
		})
	}
}

// The configurations that must be allowed, or the guard is just an obstruction. Only the
// authenticator is built: no database and no listener is touched.
func TestServeAuthAcceptsTheSafeConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts serveOptions
	}{
		{"dev on loopback by address", serveOptions{authMode: authModeDev, dev: true, listen: "127.0.0.1:8080"}},
		{"dev on loopback by name", serveOptions{authMode: authModeDev, dev: true, listen: "localhost:8080"}},
		{"dev on IPv6 loopback", serveOptions{authMode: authModeDev, dev: true, listen: "[::1]:8080"}},
		{"dev off-loopback, deliberately", serveOptions{
			authMode: authModeDev, dev: true, listen: ":8080", devInsecureListen: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if _, err := newAuthenticator(context.Background(), &opts, quietLogger()); err != nil {
				t.Fatalf("newAuthenticator: %v", err)
			}
		})
	}
}

// TLS on the issuer is required unless --dev says it is a local mock. The OIDC cases stop at
// the URL check: going further would mean discovering a provider that does not exist.
func TestCheckIssuerTLS(t *testing.T) {
	for _, tc := range []struct {
		issuer  string
		dev     bool
		wantErr bool
	}{
		{"https://login.example.com/realms/prod", false, false},
		{"https://login.example.com/realms/prod", true, false},
		{"http://login.example.com/realms/prod", false, true},
		{"http://127.0.0.1:5556/dex", true, false},
		{"login.example.com", false, true},
		{"ftp://login.example.com", true, true},
	} {
		err := checkIssuerTLS(tc.issuer, tc.dev)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkIssuerTLS(%q, dev=%v) = %v, wantErr %v", tc.issuer, tc.dev, err, tc.wantErr)
		}
	}
}

func TestIsLoopbackListen(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"127.0.0.5:0", true},
		{":8080", false},
		{"", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"10.0.0.7:8080", false},
		// A name that is not `localhost` resolves at connect time, possibly elsewhere, so it
		// is never taken for loopback.
		{"graph.internal:8080", false},
	} {
		got, err := isLoopbackListen(tc.listen)
		if err != nil {
			t.Errorf("isLoopbackListen(%q): %v", tc.listen, err)
			continue
		}
		if got != tc.want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", tc.listen, got, tc.want)
		}
	}
}
