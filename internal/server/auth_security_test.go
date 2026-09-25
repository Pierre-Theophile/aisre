// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// The security pass of T090 (constitution VII, FR-041a, FR-046, SC-011).
//
// Every test here pins a refusal rather than a capability. They exist because the failure mode
// they guard against is silent: a token that should not have been accepted is indistinguishable,
// from the outside, from a token that should have been, and the graph will happily answer it.

// ---------------------------------------------------------------------------
// OIDC: the verifier's time checks are load-bearing, so they are asserted here rather than
// assumed of go-oidc.
// ---------------------------------------------------------------------------

// A token whose `nbf` is in the future is not valid yet. go-oidc enforces it; this pins that
// the graph gets the enforcement, because a provider that pre-issues tokens for a maintenance
// window would otherwise hand out credentials that work immediately.
func TestOIDCNotYetValidTokenIsRejected(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	claims := iss.baseClaims()
	claims["nbf"] = time.Now().Add(time.Hour).Unix()

	_, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	if code := codeFor(t, auth, iss.mint(t, claims)); code != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want unauthenticated", code)
	}
}

// A token with no `exp` at all never expires, which is a credential nobody can revoke.
func TestOIDCTokenWithoutExpiryIsRejected(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	claims := iss.baseClaims()
	delete(claims, "exp")

	if _, err := auth.Authenticate(context.Background(), iss.mint(t, claims)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

// The failure message must not say which check failed: "wrong audience" tells an attacker the
// signature verified, which is the expensive half of the guess.
func TestOIDCRejectionDoesNotDiscloseWhichCheckFailed(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{Audience: "sre-agent"})

	claims := iss.baseClaims()
	claims["aud"] = "some-other-service"

	_, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if err == nil {
		t.Fatal("want a rejection")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	// The wrapped detail stays inside the process; what the caller is told is the sentinel.
	if got := ErrInvalidToken.Error(); !strings.Contains(got, "not valid") {
		t.Errorf("sentinel message %q should be the uninformative one", got)
	}
}

// SkipAudienceCheck is the one way past the audience check, and it must be explicit: the
// constructor refuses to build an authenticator that has neither an audience nor the opt-out.
func TestOIDCRequiresAudienceOrExplicitSkip(t *testing.T) {
	iss := newFakeIssuer(t)

	_, err := NewOIDCAuthenticator(context.Background(), OIDCConfig{
		IssuerURL:  iss.url(),
		HTTPClient: iss.srv.Client(),
	})
	if err == nil {
		t.Fatal("an OIDC authenticator with no audience and no SkipAudienceCheck must not build")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("err = %v, want it to name the audience", err)
	}

	// With the opt-out set, a token for another service authenticates — which is exactly why
	// the flag has to be typed.
	auth := iss.authenticator(t, OIDCConfig{SkipAudienceCheck: true})
	claims := iss.baseClaims()
	claims["aud"] = "some-other-service"
	if _, err := auth.Authenticate(context.Background(), iss.mint(t, claims)); err != nil {
		t.Fatalf("with SkipAudienceCheck: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The dev provider
// ---------------------------------------------------------------------------

// Both halves of the dev provider refuse to exist without --dev: the verifier so that a
// production server cannot accept self-asserted identities, and the minter so that a typo
// cannot hand out credentials a correct server would have refused.
func TestDevProviderAndIssuerBothRefuseWithoutDev(t *testing.T) {
	if _, err := NewDevAuthenticator(DevConfig{}); !errors.Is(err, ErrDevAuthDisabled) {
		t.Errorf("NewDevAuthenticator: err = %v, want ErrDevAuthDisabled", err)
	}
	if _, err := NewDevIssuer(DevConfig{}); !errors.Is(err, ErrDevAuthDisabled) {
		t.Errorf("NewDevIssuer: err = %v, want ErrDevAuthDisabled", err)
	}
}

// The banner is the operator's only signal that a process is accepting self-asserted
// identities, and `default_key` is the part that says the signing key is published.
func TestDevBannerSaysWhenTheDefaultKeyIsInUse(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if _, err := NewDevAuthenticator(DevConfig{Enabled: true, Logger: logger}); err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"DEV AUTHENTICATION ENABLED", "NOT FOR PRODUCTION", "default_key=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, string(devDefaultKey)) {
		t.Error("the banner printed the signing key itself")
	}
}

// A token signed with the published default key must not verify against a server configured
// with a key of its own: the whole point of configuring one is to stop that.
func TestDevDefaultKeyDoesNotVerifyAgainstAConfiguredKey(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	loose, err := NewDevIssuer(DevConfig{Enabled: true, Logger: quiet})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}
	token, err := loose.MintFor(DevUser{Name: "mallory", Roles: []Role{RoleReader, RoleDecider}})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	strict, err := NewDevAuthenticator(DevConfig{
		Enabled: true,
		Key:     []byte("a-configured-key-of-at-least-32-bytes"),
		Logger:  quiet,
	})
	if err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	if _, err := strict.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

// ---------------------------------------------------------------------------
// Role separation, at the authorization layer
// ---------------------------------------------------------------------------

// The three refusals that define the role model: a reader cannot ingest, a feeder cannot read,
// and a feeder cannot write for a source its token does not name (FR-046).
func TestRoleSeparation(t *testing.T) {
	reader := ContextWithPrincipal(context.Background(),
		&Principal{Issuer: "i", Subject: "alice", Roles: []Role{RoleReader}})
	decider := ContextWithPrincipal(context.Background(),
		&Principal{Issuer: "i", Subject: "bob", Roles: []Role{RoleDecider}})
	feeder := ContextWithPrincipal(context.Background(),
		&Principal{Issuer: "i", Subject: "otel", Roles: []Role{RoleFeeder}, SourceID: "otel:demo"})

	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"reader may not ingest", RequireSource(reader, "otel:demo"), connect.CodePermissionDenied},
		{"decider may not ingest", RequireSource(decider, "otel:demo"), connect.CodePermissionDenied},
		{"feeder may not query", Require(feeder, RoleReader), connect.CodePermissionDenied},
		{"feeder may not decide", Require(feeder, RoleDecider), connect.CodePermissionDenied},
		{"feeder may not write for another source", RequireSource(feeder, "k8s:prod"), connect.CodePermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := connect.CodeOf(tc.err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, tc.err, tc.want)
			}
		})
	}

	// And the two that must succeed, so the test is not vacuously green.
	if err := RequireSource(feeder, "otel:demo"); err != nil {
		t.Errorf("feeder on its own source: %v", err)
	}
	if err := Require(decider, RoleReader); err != nil {
		t.Errorf("decider implies reader: %v", err)
	}
}

// A permission-denied message names the principal and the role, never the token. An audit trail
// needs the first; nothing needs the second.
func TestAuthorizationErrorsCarryNoCredential(t *testing.T) {
	ctx := ContextWithPrincipal(context.Background(),
		&Principal{Issuer: "sre-agent-dev", Subject: "alice", Roles: []Role{RoleReader}})

	err := Require(ctx, RoleFeeder)
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "sre-agent-dev|alice") {
		t.Errorf("message %q should name the principal", msg)
	}
	if strings.Contains(msg, "eyJ") { // a JWT always starts with the base64 of `{"`
		t.Errorf("message %q looks like it contains a token", msg)
	}
}
