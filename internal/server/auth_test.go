// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// ---------------------------------------------------------------------------
// A fake OIDC issuer: discovery document, JWKS and a signing key, served over
// httptest. It is the stand-in for the organisation's identity provider.
// ---------------------------------------------------------------------------

type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

// testKey is generated once: 2048-bit RSA key generation is slow enough to dominate the test
// run if every case pays for it.
var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyErr  error
)

func sharedTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		testKey, testKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if testKeyErr != nil {
		t.Fatalf("generate test key: %v", testKeyErr)
	}
	return testKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{key: sharedTestKey(t), kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.url(),
			"authorization_endpoint":                f.url() + "/auth",
			"token_endpoint":                        f.url() + "/token",
			"jwks_uri":                              f.url() + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       f.key.Public(),
			KeyID:     f.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) url() string {
	if f.srv == nil {
		return ""
	}
	return f.srv.URL
}

func (f *fakeIssuer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: f.kid, Algorithm: string(jose.RS256)}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return token
}

// baseClaims is a well-formed reader token. Tests override individual claims.
func (f *fakeIssuer) baseClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   f.url(),
		"sub":   "3f0e-user",
		"aud":   "sre-agent",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"email": "alice@example.com",
		"name":  "Alice Example",
		"roles": []string{"reader"},
	}
}

func (f *fakeIssuer) authenticator(t *testing.T, cfg OIDCConfig) *OIDCAuthenticator {
	t.Helper()
	cfg.IssuerURL = f.url()
	if cfg.Audience == "" && !cfg.SkipAudienceCheck {
		cfg.Audience = "sre-agent"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = f.srv.Client()
	}
	a, err := NewOIDCAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// OIDC authenticator
// ---------------------------------------------------------------------------

func TestOIDCValidTokenYieldsPrincipalWithRoles(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	claims := iss.baseClaims()
	claims["roles"] = []string{"reader", "decider", "not-a-role"}
	p, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if got, want := p.Key(), iss.url()+"|3f0e-user"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if p.Email != "alice@example.com" {
		t.Errorf("Email = %q", p.Email)
	}
	if p.DisplayName != "Alice Example" {
		t.Errorf("DisplayName = %q", p.DisplayName)
	}
	if len(p.Roles) != 2 {
		t.Errorf("Roles = %v, want the two known roles only", p.Roles)
	}
	if !p.Has(RoleDecider) {
		t.Error("want decider")
	}
}

func TestOIDCExpiredTokenIsUnauthenticated(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	claims := iss.baseClaims()
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()

	_, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	if code := codeFor(t, auth, iss.mint(t, claims)); code != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want unauthenticated", code)
	}
}

func TestOIDCWrongAudienceIsUnauthenticated(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{Audience: "sre-agent"})

	claims := iss.baseClaims()
	claims["aud"] = "some-other-service"

	_, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestOIDCForeignSignatureIsRejected(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	// A token minted by an issuer we do not trust, replaying our issuer's claims.
	other := newFakeIssuer(t)
	other.kid = "attacker-key"
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	other.key = otherKey

	claims := iss.baseClaims()
	if _, err := auth.Authenticate(context.Background(), other.mint(t, claims)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestOIDCNestedRolesClaimAndRoleMapping(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{
		RolesClaim: "realm_access.roles",
		RoleMapping: map[string]Role{
			"sre-graph-read":   RoleReader,
			"sre-graph-decide": RoleDecider,
		},
	})

	claims := iss.baseClaims()
	delete(claims, "roles")
	claims["realm_access"] = map[string]any{"roles": []string{"sre-graph-decide", "offline_access"}}

	p, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.Has(RoleDecider) || !p.Has(RoleReader) {
		t.Fatalf("roles = %v, want decider (implying reader)", p.Roles)
	}
	if len(p.Roles) != 1 {
		t.Errorf("roles = %v, want only the mapped decider role", p.Roles)
	}
}

func TestOIDCFeederWithoutSourceIDIsRejected(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	claims := iss.baseClaims()
	claims["roles"] = []string{"feeder"}

	_, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if !errors.Is(err, ErrFeederWithoutSource) {
		t.Fatalf("err = %v, want ErrFeederWithoutSource", err)
	}
}

func TestOIDCFeederCarriesSourceID(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})

	claims := iss.baseClaims()
	claims["roles"] = []string{"feeder"}
	claims["source_id"] = "k8s:prod"

	p, err := auth.Authenticate(context.Background(), iss.mint(t, claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.SourceID != "k8s:prod" {
		t.Fatalf("SourceID = %q", p.SourceID)
	}
	if p.Has(RoleReader) {
		t.Error("a feeder must not imply reader")
	}
}

func TestOIDCEmptyTokenIsMissing(t *testing.T) {
	iss := newFakeIssuer(t)
	auth := iss.authenticator(t, OIDCConfig{})
	if _, err := auth.Authenticate(context.Background(), "   "); !errors.Is(err, ErrMissingToken) {
		t.Fatalf("err = %v, want ErrMissingToken", err)
	}
}

func TestNewOIDCAuthenticatorRequiresIssuerAndAudience(t *testing.T) {
	if _, err := NewOIDCAuthenticator(context.Background(), OIDCConfig{Audience: "x"}); err == nil {
		t.Error("want an error without an issuer URL")
	}
	if _, err := NewOIDCAuthenticator(context.Background(), OIDCConfig{IssuerURL: "https://x.invalid"}); err == nil {
		t.Error("want an error without an audience")
	}
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

func TestDeciderImpliesReader(t *testing.T) {
	p := &Principal{Issuer: "i", Subject: "s", Roles: []Role{RoleDecider}}
	if !p.Has(RoleReader) {
		t.Error("decider must imply reader")
	}
	if !p.Has(RoleDecider) {
		t.Error("decider must have decider")
	}
	if p.Has(RoleFeeder) {
		t.Error("decider must not imply feeder")
	}

	reader := &Principal{Issuer: "i", Subject: "s", Roles: []Role{RoleReader}}
	if reader.Has(RoleDecider) {
		t.Error("reader must not imply decider")
	}

	var nilPrincipal *Principal
	if nilPrincipal.Has(RoleReader) {
		t.Error("a nil principal has no roles")
	}
}

func TestParseRoles(t *testing.T) {
	got, err := ParseRoles("r, d ,reader")
	if err != nil {
		t.Fatalf("ParseRoles: %v", err)
	}
	if len(got) != 2 || got[0] != RoleReader || got[1] != RoleDecider {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseRoles("r,admin"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("err = %v, want ErrUnknownRole", err)
	}
	if got, err := ParseRoles(""); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
		wantErr            bool
	}{
		{name: "bearer", header: "Bearer abc", want: "abc"},
		{name: "lowercase scheme", header: "bearer abc", want: "abc"},
		{name: "missing", header: "", wantErr: true},
		{name: "other scheme", header: "Basic abc", wantErr: true},
		{name: "empty token", header: "Bearer   ", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Authorization", tc.header)
			}
			got, err := BearerToken(h)
			if tc.wantErr {
				if !errors.Is(err, ErrMissingToken) {
					t.Fatalf("err = %v, want ErrMissingToken", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Require / RequireSource
// ---------------------------------------------------------------------------

func TestRequire(t *testing.T) {
	if code := connect.CodeOf(Require(context.Background(), RoleReader)); code != connect.CodeUnauthenticated {
		t.Errorf("anonymous: code = %v, want unauthenticated", code)
	}

	ctx := ContextWithPrincipal(context.Background(), &Principal{Issuer: "i", Subject: "s", Roles: []Role{RoleReader}})
	if err := Require(ctx, RoleReader); err != nil {
		t.Errorf("reader: %v", err)
	}
	if code := connect.CodeOf(Require(ctx, RoleDecider)); code != connect.CodePermissionDenied {
		t.Errorf("decider: code = %v, want permission denied", code)
	}
}

func TestRequireSource(t *testing.T) {
	ctx := ContextWithPrincipal(context.Background(), &Principal{
		Issuer: "i", Subject: "otel-feeder", Roles: []Role{RoleFeeder}, SourceID: "otel:demo",
	})
	if err := RequireSource(ctx, "otel:demo"); err != nil {
		t.Errorf("own source: %v", err)
	}
	if code := connect.CodeOf(RequireSource(ctx, "k8s:prod")); code != connect.CodePermissionDenied {
		t.Errorf("other source: code = %v, want permission denied", code)
	}

	reader := ContextWithPrincipal(context.Background(), &Principal{Issuer: "i", Subject: "s", Roles: []Role{RoleReader}})
	if code := connect.CodeOf(RequireSource(reader, "otel:demo")); code != connect.CodePermissionDenied {
		t.Errorf("reader: code = %v, want permission denied", code)
	}
}

// ---------------------------------------------------------------------------
// Dev provider
// ---------------------------------------------------------------------------

func TestDevProviderRefusesWhenNotEnabled(t *testing.T) {
	if _, err := NewDevAuthenticator(DevConfig{}); !errors.Is(err, ErrDevAuthDisabled) {
		t.Fatalf("NewDevAuthenticator: err = %v, want ErrDevAuthDisabled", err)
	}
	if _, err := NewDevIssuer(DevConfig{}); !errors.Is(err, ErrDevAuthDisabled) {
		t.Fatalf("NewDevIssuer: err = %v, want ErrDevAuthDisabled", err)
	}
}

func TestDevProviderLogsBannerWhenEnabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if _, err := NewDevAuthenticator(DevConfig{Enabled: true, Logger: logger}); err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	if !strings.Contains(buf.String(), "DEV AUTHENTICATION ENABLED") {
		t.Fatalf("banner not logged, got:\n%s", buf.String())
	}
}

func devProvider(t *testing.T, cfg DevConfig) *DevAuthenticator {
	t.Helper()
	cfg.Enabled = true
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	}
	a, err := NewDevAuthenticator(cfg)
	if err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	return a
}

func TestDevMintAndAuthenticateRoundTrip(t *testing.T) {
	auth := devProvider(t, DevConfig{})
	token, err := auth.Issuer().Mint("alice", []Role{RoleReader, RoleDecider})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Key() != DefaultDevIssuer+"|alice" {
		t.Errorf("Key() = %q", p.Key())
	}
	if !p.Has(RoleDecider) || !p.Has(RoleReader) {
		t.Errorf("roles = %v", p.Roles)
	}
}

func TestDevTokenFromAnotherProcessVerifies(t *testing.T) {
	// `aisre dev-token` and `aisre serve --auth dev --dev` are separate processes
	// with separate configs; the published default key is what makes them agree.
	minter, err := NewDevIssuer(DevConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}
	token, err := minter.Mint("carol", []Role{RoleReader})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := devProvider(t, DevConfig{}).Authenticate(context.Background(), token); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestDevRejectsExpiredAndForeignTokens(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	minter, err := NewDevIssuer(DevConfig{Enabled: true, TokenTTL: time.Hour, Now: func() time.Time { return past }})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}
	expired, err := minter.Mint("alice", []Role{RoleReader})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	auth := devProvider(t, DevConfig{})
	if _, err := auth.Authenticate(context.Background(), expired); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired: err = %v, want ErrInvalidToken", err)
	}

	foreign, err := NewDevIssuer(DevConfig{Enabled: true, Key: []byte("a-different-secret-at-least-32-bytes")})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}
	token, err := foreign.Mint("mallory", []Role{RoleDecider})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := auth.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("foreign key: err = %v, want ErrInvalidToken", err)
	}

	wrongAudience, err := NewDevIssuer(DevConfig{Enabled: true, Audience: "another-service"})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}
	token, err = wrongAudience.Mint("mallory", []Role{RoleDecider})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := auth.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong audience: err = %v, want ErrInvalidToken", err)
	}
}

func TestDevUsersFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")
	const doc = `users:
  - name: alice
    email: alice@example.com
    display_name: Alice Example
    roles: [reader, decider]
  - name: bob
    email: bob@example.com
    roles: [r]
  - name: otel-feeder
    roles: [feeder]
    source_id: otel:demo
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write users file: %v", err)
	}
	auth := devProvider(t, DevConfig{UsersFile: path})
	issuer := auth.Issuer()

	if got := len(issuer.Users()); got != 3 {
		t.Fatalf("Users() = %d entries, want 3", got)
	}
	if _, ok := issuer.User("alice"); !ok {
		t.Fatal("alice not declared")
	}

	// Roles default to what the file grants.
	token, err := issuer.Mint("alice", nil)
	if err != nil {
		t.Fatalf("Mint alice: %v", err)
	}
	p, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate alice: %v", err)
	}
	if !p.Has(RoleDecider) || p.Email != "alice@example.com" || p.DisplayName != "Alice Example" {
		t.Fatalf("principal = %+v", p)
	}

	// The file, not the caller, decides what a user may do.
	if _, err := issuer.Mint("bob", []Role{RoleDecider}); err == nil {
		t.Error("want an error minting a role bob was not granted")
	}
	if _, err := issuer.Mint("eve", []Role{RoleReader}); err == nil {
		t.Error("want an error minting for an undeclared user")
	}

	// A feeder token carries its source scope.
	token, err = issuer.Mint("otel-feeder", nil)
	if err != nil {
		t.Fatalf("Mint feeder: %v", err)
	}
	p, err = auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate feeder: %v", err)
	}
	if p.SourceID != "otel:demo" {
		t.Fatalf("SourceID = %q", p.SourceID)
	}
	if err := RequireSource(ContextWithPrincipal(context.Background(), p), "otel:demo"); err != nil {
		t.Fatalf("RequireSource: %v", err)
	}
}

func TestDevUsersFileRejectsFeederWithoutSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	const doc = `{"users":[{"name":"broken","roles":["feeder"]}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewDevIssuer(DevConfig{Enabled: true, UsersFile: path}); !errors.Is(err, ErrFeederWithoutSource) {
		t.Fatalf("err = %v, want ErrFeederWithoutSource", err)
	}
}

func TestDevUsersFileRejectsUnknownRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, []byte("users:\n  - name: x\n    roles: [admin]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewDevIssuer(DevConfig{Enabled: true, UsersFile: path}); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("err = %v, want ErrUnknownRole", err)
	}
}

func TestDevMintForBypassesUsersFile(t *testing.T) {
	issuer, err := NewDevIssuer(DevConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}
	if _, err := issuer.MintFor(DevUser{Name: "f", Roles: []Role{RoleFeeder}}); !errors.Is(err, ErrFeederWithoutSource) {
		t.Fatalf("err = %v, want ErrFeederWithoutSource", err)
	}
	token, err := issuer.MintFor(DevUser{Name: "f", Roles: []Role{RoleFeeder}, SourceID: "k8s:prod"})
	if err != nil {
		t.Fatalf("MintFor: %v", err)
	}
	p, err := devProvider(t, DevConfig{}).Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.SourceID != "k8s:prod" {
		t.Fatalf("SourceID = %q", p.SourceID)
	}
}

// ---------------------------------------------------------------------------
// Connect interceptor, exercised over a real HTTP round trip
// ---------------------------------------------------------------------------

const (
	testUnaryProcedure  = "/sreagent.test.v1.AuthTest/Ping"
	testStreamProcedure = "/sreagent.test.v1.AuthTest/Feed"
	testHealthProcedure = "/grpc.health.v1.Health/Check"
)

type testServer struct {
	url  string
	seen chan *Principal
}

func newTestServer(t *testing.T, interceptor *AuthInterceptor) *testServer {
	t.Helper()
	ts := &testServer{seen: make(chan *Principal, 8)}
	opts := connect.WithInterceptors(interceptor)

	record := func(ctx context.Context) {
		p, _ := PrincipalFrom(ctx)
		select {
		case ts.seen <- p:
		default:
		}
	}

	mux := http.NewServeMux()
	for _, procedure := range []string{testUnaryProcedure, testHealthProcedure} {
		mux.Handle(procedure, connect.NewUnaryHandler(procedure,
			func(ctx context.Context, _ *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
				record(ctx)
				return connect.NewResponse(wrapperspb.String("pong")), nil
			}, opts))
	}
	mux.Handle(testStreamProcedure, connect.NewClientStreamHandler(testStreamProcedure,
		func(ctx context.Context, stream *connect.ClientStream[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			record(ctx)
			for stream.Receive() {
			}
			if err := stream.Err(); err != nil {
				return nil, err
			}
			return connect.NewResponse(wrapperspb.String("ok")), nil
		}, opts))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ts.url = srv.URL
	return ts
}

func (ts *testServer) ping(t *testing.T, procedure, token string) error {
	t.Helper()
	client := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		http.DefaultClient, ts.url+procedure)
	req := connect.NewRequest(wrapperspb.String("ping"))
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	_, err := client.CallUnary(context.Background(), req)
	return err
}

func (ts *testServer) feed(t *testing.T, token string) error {
	t.Helper()
	client := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		http.DefaultClient, ts.url+testStreamProcedure)
	stream := client.CallClientStream(context.Background())
	if token != "" {
		stream.RequestHeader().Set("Authorization", "Bearer "+token)
	}
	if err := stream.Send(wrapperspb.String("event")); err != nil {
		// Send reports a broken stream as io.EOF; CloseAndReceive carries the real error.
		_, closeErr := stream.CloseAndReceive()
		return closeErr
	}
	_, err := stream.CloseAndReceive()
	return err
}

func devInterceptor(t *testing.T, opts ...AuthInterceptorOption) (*AuthInterceptor, *DevIssuer) {
	t.Helper()
	auth := devProvider(t, DevConfig{})
	opts = append(opts, WithAuthLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	return NewAuthInterceptor(auth, opts...), auth.Issuer()
}

func TestInterceptorRejectsAnonymousCalls(t *testing.T) {
	interceptor, _ := devInterceptor(t)
	ts := newTestServer(t, interceptor)

	err := ts.ping(t, testUnaryProcedure, "")
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("unary: code = %v (err %v), want unauthenticated", code, err)
	}
	if err := ts.feed(t, ""); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stream: code = %v (err %v), want unauthenticated", connect.CodeOf(err), err)
	}
}

func TestInterceptorRejectsInvalidToken(t *testing.T) {
	interceptor, _ := devInterceptor(t)
	ts := newTestServer(t, interceptor)
	if code := connect.CodeOf(ts.ping(t, testUnaryProcedure, "not.a.token")); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want unauthenticated", code)
	}
}

func TestInterceptorPassesPrincipalToHandler(t *testing.T) {
	interceptor, issuer := devInterceptor(t)
	ts := newTestServer(t, interceptor)

	token, err := issuer.Mint("alice", []Role{RoleDecider})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := ts.ping(t, testUnaryProcedure, token); err != nil {
		t.Fatalf("unary: %v", err)
	}
	p := <-ts.seen
	if p == nil || p.Subject != "alice" || !p.Has(RoleReader) {
		t.Fatalf("handler saw %+v", p)
	}

	if err := ts.feed(t, token); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if p := <-ts.seen; p == nil || p.Subject != "alice" {
		t.Fatalf("streaming handler saw %+v", p)
	}
}

func TestInterceptorPublicProcedures(t *testing.T) {
	interceptor, _ := devInterceptor(t, WithPublicProcedures("/grpc.health.v1.Health/"))
	ts := newTestServer(t, interceptor)

	if err := ts.ping(t, testHealthProcedure, ""); err != nil {
		t.Fatalf("health without a token: %v", err)
	}
	if p := <-ts.seen; p != nil {
		t.Fatalf("public handler saw a principal: %+v", p)
	}
	if code := connect.CodeOf(ts.ping(t, testUnaryProcedure, "")); code != connect.CodeUnauthenticated {
		t.Fatalf("non-public procedure: code = %v, want unauthenticated", code)
	}

	exact := NewAuthInterceptor(nil, WithPublicProcedures(testHealthProcedure))
	if !exact.IsPublic(testHealthProcedure) {
		t.Error("exact match must be public")
	}
	if exact.IsPublic(testUnaryProcedure) {
		t.Error("unrelated procedure must not be public")
	}
}

func TestInterceptorRejectsPrincipalWithoutIdentity(t *testing.T) {
	// FR-041: an authenticator that returns a principal with no subject is an anonymous
	// credential, and must not reach a handler.
	anonymous := AuthenticatorFunc(func(context.Context, string) (*Principal, error) {
		return &Principal{Issuer: "i", Roles: []Role{RoleReader}}, nil
	})
	interceptor := NewAuthInterceptor(anonymous, WithAuthLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	ts := newTestServer(t, interceptor)
	if code := connect.CodeOf(ts.ping(t, testUnaryProcedure, "anything")); code != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want unauthenticated", code)
	}
}

func TestInterceptorPreservesAuthenticatorConnectCode(t *testing.T) {
	custom := AuthenticatorFunc(func(context.Context, string) (*Principal, error) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("idp is down"))
	})
	interceptor := NewAuthInterceptor(custom, WithAuthLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	ts := newTestServer(t, interceptor)
	if code := connect.CodeOf(ts.ping(t, testUnaryProcedure, "anything")); code != connect.CodeUnavailable {
		t.Fatalf("code = %v, want unavailable", code)
	}
}

// codeFor runs one token through an interceptor and returns the resulting connect code.
func codeFor(t *testing.T, auth Authenticator, token string) connect.Code {
	t.Helper()
	interceptor := NewAuthInterceptor(auth, WithAuthLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	ts := newTestServer(t, interceptor)
	return connect.CodeOf(ts.ping(t, testUnaryProcedure, token))
}
