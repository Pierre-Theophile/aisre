// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"
)

// The k3s and kubeadm default: every such cluster's tokens carry it, which is rule 2's reason.
const k8sIssuer = "https://kubernetes.default.svc.cluster.local"

const feederSubject = "system:serviceaccount:sre-agent:sre-agent-feeder"

var workloadNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// cluster is one token issuer with its own key: a stand-in for a cluster's ServiceAccount
// signer.
type cluster struct {
	key *rsa.PrivateKey
	kid string
}

func newCluster(t *testing.T, kid string) *cluster {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &cluster{key: key, kid: kid}
}

// jwksFile writes the cluster's public key set, as `kubectl get --raw /openid/v1/jwks` prints it.
func (c *cluster) jwksFile(t *testing.T, dir string) string {
	t.Helper()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &c.key.PublicKey, KeyID: c.kid, Algorithm: string(jose.RS256), Use: "sig",
	}}}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, c.kid+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// claims are those of a projected ServiceAccount token with audience aisre, valid ten minutes.
func (c *cluster) claims() map[string]any {
	return map[string]any{
		"iss": k8sIssuer,
		"sub": feederSubject,
		"aud": []string{"aisre"},
		"iat": workloadNow.Add(-time.Minute).Unix(),
		"nbf": workloadNow.Add(-time.Minute).Unix(),
		"exp": workloadNow.Add(9 * time.Minute).Unix(),
	}
}

func (c *cluster) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	return mintWith(t, jose.RS256, c.key, c.kid, claims)
}

func mintWith(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: jose.JSONWebKey{Key: key, KeyID: kid, Algorithm: string(alg)}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func feederEntry(name, jwks, sourceID string) WorkloadIssuer {
	return WorkloadIssuer{
		Name:     name,
		Issuer:   k8sIssuer,
		Audience: "aisre",
		JWKSFile: jwks,
		Subjects: map[string]WorkloadGrant{
			feederSubject: {Roles: []string{"feeder"}, SourceID: sourceID},
		},
	}
}

func workloadAuth(t *testing.T, cfg WorkloadConfig, fallback Authenticator) *WorkloadAuthenticator {
	t.Helper()
	a, err := NewWorkloadAuthenticator(context.Background(), cfg, WorkloadOptions{
		Fallback: fallback,
		Now:      func() time.Time { return workloadNow },
	})
	if err != nil {
		t.Fatalf("NewWorkloadAuthenticator: %v", err)
	}
	return a
}

func TestWorkloadProjectedTokenYieldsTheGrantedFeeder(t *testing.T) {
	c := newCluster(t, "verda")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)

	p, err := a.Authenticate(context.Background(), c.mint(t, c.claims()))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Issuer != k8sIssuer || p.Subject != feederSubject || p.SourceID != "k8s:verda" ||
		!p.Has(RoleFeeder) || p.Has(RoleReader) {
		t.Fatalf("principal = %+v", p)
	}
}

// Rule 1: the pod's ordinary token is audienced for the Kubernetes API, not for the graph.
func TestWorkloadRefusesATokenForAnotherAudience(t *testing.T) {
	c := newCluster(t, "verda")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)

	claims := c.claims()
	claims["aud"] = []string{"https://kubernetes.default.svc.cluster.local", "k3s"}
	if _, err := a.Authenticate(context.Background(), c.mint(t, claims)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate = %v, want ErrInvalidToken", err)
	}
}

func TestWorkloadEntryWithoutAudienceDoesNotStart(t *testing.T) {
	c := newCluster(t, "verda")
	entry := feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda")
	entry.Audience = ""
	_, err := NewWorkloadAuthenticator(context.Background(), WorkloadConfig{Issuers: []WorkloadIssuer{entry}}, WorkloadOptions{})
	if err == nil || !strings.Contains(err.Error(), "audience is required") {
		t.Fatalf("err = %v, want audience is required", err)
	}
}

// Rule 2: another cluster with the same default issuer and the same ServiceAccount name gets
// its own entry's grant, or nothing; never the first cluster's.
func TestWorkloadGrantFollowsTheKeyNotTheIssuer(t *testing.T) {
	dir := t.TempDir()
	verda, other, stranger := newCluster(t, "verda"), newCluster(t, "other"), newCluster(t, "stranger")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", verda.jwksFile(t, dir), "k8s:verda"),
		feederEntry("other-k3s", other.jwksFile(t, dir), "k8s:other"),
	}}, nil)

	p, err := a.Authenticate(context.Background(), other.mint(t, other.claims()))
	if err != nil {
		t.Fatalf("other cluster: %v", err)
	}
	if p.SourceID != "k8s:other" {
		t.Fatalf("other cluster's token was granted %q, want k8s:other", p.SourceID)
	}

	// Same issuer, same subject, a key nobody trusts: the issuer alone grants nothing.
	if _, err := a.Authenticate(context.Background(), stranger.mint(t, stranger.claims())); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("stranger cluster: %v, want ErrInvalidToken", err)
	}
}

func TestWorkloadRefusesOneKeyTrustedByTwoEntriesOfOneIssuer(t *testing.T) {
	c := newCluster(t, "verda")
	jwks := c.jwksFile(t, t.TempDir())
	_, err := NewWorkloadAuthenticator(context.Background(), WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", jwks, "k8s:verda"),
		feederEntry("verda-again", jwks, "k8s:elsewhere"),
	}}, WorkloadOptions{})
	if err == nil || !strings.Contains(err.Error(), "also trusted by") {
		t.Fatalf("err = %v, want a duplicate-key refusal", err)
	}
}

// Rule 3: exact subjects, and roles from the grant only.
func TestWorkloadSubjectsMatchExactly(t *testing.T) {
	c := newCluster(t, "verda")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)

	for _, sub := range []string{
		"system:serviceaccount:sre-agent:default",
		"system:serviceaccount:sre-agent:sre-agent-feeder-2",
		"system:serviceaccount:other:sre-agent-feeder",
	} {
		claims := c.claims()
		claims["sub"] = sub
		if _, err := a.Authenticate(context.Background(), c.mint(t, claims)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("sub %q: %v, want ErrInvalidToken", sub, err)
		}
	}
}

func TestWorkloadIgnoresRoleAndSourceClaimsInTheToken(t *testing.T) {
	c := newCluster(t, "verda")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)

	claims := c.claims()
	claims["roles"] = []string{"decider", "reader"}
	claims["source_id"] = "github:everything"
	p, err := a.Authenticate(context.Background(), c.mint(t, claims))
	if err != nil {
		t.Fatal(err)
	}
	if p.Has(RoleDecider) || p.Has(RoleReader) || p.SourceID != "k8s:verda" {
		t.Fatalf("token claims leaked into the principal: %+v", p)
	}
}

func TestWorkloadRefusesWildcardsAndUnscopedFeeders(t *testing.T) {
	c := newCluster(t, "verda")
	jwks := c.jwksFile(t, t.TempDir())
	for name, subjects := range map[string]map[string]WorkloadGrant{
		"wildcard":         {"system:serviceaccount:sre-agent:*": {Roles: []string{"feeder"}, SourceID: "k8s:verda"}},
		"feeder no source": {feederSubject: {Roles: []string{"feeder"}}},
		"source no feeder": {feederSubject: {Roles: []string{"reader"}, SourceID: "k8s:verda"}},
		"no role":          {feederSubject: {SourceID: "k8s:verda"}},
		"unknown role":     {feederSubject: {Roles: []string{"admin"}}},
		"nothing granted":  {},
	} {
		entry := feederEntry("verda-k3s", jwks, "")
		entry.Subjects = subjects
		if _, err := NewWorkloadAuthenticator(context.Background(), WorkloadConfig{Issuers: []WorkloadIssuer{entry}}, WorkloadOptions{}); err == nil {
			t.Errorf("%s: started, want a refusal", name)
		}
	}
}

// Rule 4: `kubectl create token --duration 8760h` mints a year; the graph accepts an hour.
func TestWorkloadCapsTokenLifetime(t *testing.T) {
	c := newCluster(t, "verda")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)

	long := c.claims()
	long["exp"] = workloadNow.Add(365 * 24 * time.Hour).Unix()
	if _, err := a.Authenticate(context.Background(), c.mint(t, long)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("year-long token: %v, want ErrInvalidToken", err)
	}

	for _, missing := range []string{"iat", "exp"} {
		claims := c.claims()
		delete(claims, missing)
		if _, err := a.Authenticate(context.Background(), c.mint(t, claims)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("no %s: %v, want ErrInvalidToken", missing, err)
		}
	}

	future := c.claims()
	future["iat"] = workloadNow.Add(10 * time.Minute).Unix()
	future["exp"] = workloadNow.Add(20 * time.Minute).Unix()
	if _, err := a.Authenticate(context.Background(), c.mint(t, future)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("iat in the future: %v, want ErrInvalidToken", err)
	}

	expired := c.claims()
	expired["iat"] = workloadNow.Add(-20 * time.Minute).Unix()
	expired["exp"] = workloadNow.Add(-10 * time.Minute).Unix()
	if _, err := a.Authenticate(context.Background(), c.mint(t, expired)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired: %v, want ErrInvalidToken", err)
	}
}

// Rule 5: algorithms are pinned, and a trust file holds public keys only.
func TestWorkloadPinsAlgorithms(t *testing.T) {
	dir := t.TempDir()
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &ecKey.PublicKey, KeyID: "ec", Algorithm: string(jose.ES256)}}})
	jwks := filepath.Join(dir, "ec.json")
	if err := os.WriteFile(jwks, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := feederEntry("ec-cluster", jwks, "k8s:ec")
	entry.Algorithms = []string{"RS256"}
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{entry}}, nil)

	if _, err := a.Authenticate(context.Background(), mintWith(t, jose.ES256, ecKey, "ec", (&cluster{}).claims())); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("ES256 token against an RS256-only entry: %v, want ErrInvalidToken", err)
	}

	entry.Algorithms = []string{"HS256"}
	if _, err := NewWorkloadAuthenticator(context.Background(), WorkloadConfig{Issuers: []WorkloadIssuer{entry}}, WorkloadOptions{}); err == nil {
		t.Fatal("HS256 accepted as an algorithm")
	}
}

func TestWorkloadRefusesASymmetricKeyInTheTrustFile(t *testing.T) {
	raw, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "hmac", Algorithm: string(jose.HS256)}}})
	jwks := filepath.Join(t.TempDir(), "hmac.json")
	if err := os.WriteFile(jwks, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewWorkloadAuthenticator(context.Background(), WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("hmac", jwks, "k8s:hmac"),
	}}, WorkloadOptions{})
	if err == nil || !strings.Contains(err.Error(), "not an asymmetric public key") {
		t.Fatalf("err = %v, want a refusal of the shared key", err)
	}
}

func TestWorkloadRefusesAPlaintextKeySetURL(t *testing.T) {
	entry := feederEntry("google", "", "")
	entry.Subjects = map[string]WorkloadGrant{"1234": {Roles: []string{"reader"}}}
	entry.JWKSURL = "http://www.googleapis.com/oauth2/v3/certs"
	if _, err := NewWorkloadAuthenticator(context.Background(), WorkloadConfig{Issuers: []WorkloadIssuer{entry}}, WorkloadOptions{}); err == nil {
		t.Fatal("http jwks_url accepted")
	}
}

// Human identities: email_verified and hd are enforced when the entry asks.
func TestWorkloadHumanChecks(t *testing.T) {
	c := newCluster(t, "google")
	entry := WorkloadIssuer{
		Name: "google", Issuer: "https://accounts.google.com", Audience: "aisre-staging",
		JWKSFile: c.jwksFile(t, t.TempDir()), RequireEmailVerified: true, HostedDomain: "makipeople.com",
		Subjects: map[string]WorkloadGrant{"108000000000000000001": {Name: "Pierre", Roles: []string{"decider"}}},
	}
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{entry}}, nil)
	base := func() map[string]any {
		return map[string]any{
			"iss": "https://accounts.google.com", "sub": "108000000000000000001", "aud": "aisre-staging",
			"iat": workloadNow.Add(-time.Minute).Unix(), "exp": workloadNow.Add(59 * time.Minute).Unix(),
			"email": "pierre@makipeople.com", "email_verified": true, "hd": "makipeople.com",
		}
	}

	p, err := a.Authenticate(context.Background(), c.mint(t, base()))
	if err != nil {
		t.Fatalf("valid human token: %v", err)
	}
	if !p.Has(RoleDecider) || p.DisplayName != "Pierre" || p.Email != "pierre@makipeople.com" {
		t.Fatalf("principal = %+v", p)
	}

	unverified := base()
	unverified["email_verified"] = false
	if _, err := a.Authenticate(context.Background(), c.mint(t, unverified)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("unverified email: %v, want ErrInvalidToken", err)
	}
	personal := base()
	delete(personal, "hd")
	if _, err := a.Authenticate(context.Background(), c.mint(t, personal)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("no hosted domain: %v, want ErrInvalidToken", err)
	}
}

func TestWorkloadHandsUntrustedIssuersToTheFallback(t *testing.T) {
	c := newCluster(t, "verda")
	called := false
	fallback := AuthenticatorFunc(func(context.Context, string) (*Principal, error) {
		called = true
		return &Principal{Issuer: "https://login.example.com", Subject: "someone", Roles: []Role{RoleReader}}, nil
	})
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, fallback)

	idp := c.claims()
	idp["iss"] = "https://login.example.com"
	if _, err := a.Authenticate(context.Background(), c.mint(t, idp)); err != nil || !called {
		t.Fatalf("fallback not used: err=%v called=%v", err, called)
	}

	// A trusted issuer that fails verification never falls through to the fallback.
	called = false
	bad := c.claims()
	bad["aud"] = "something-else"
	if _, err := a.Authenticate(context.Background(), c.mint(t, bad)); !errors.Is(err, ErrInvalidToken) || called {
		t.Fatalf("trusted issuer fell through: err=%v called=%v", err, called)
	}

	noFallback := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)
	if _, err := noFallback.Authenticate(context.Background(), c.mint(t, idp)); !errors.Is(err, ErrUntrustedIssuer) {
		t.Fatalf("untrusted issuer without fallback: %v, want ErrUntrustedIssuer", err)
	}
	if _, err := noFallback.Authenticate(context.Background(), "not-a-jwt"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("garbage: %v, want ErrInvalidToken", err)
	}
}

func TestLoadWorkloadConfigResolvesRelativeKeyFiles(t *testing.T) {
	dir := t.TempDir()
	c := newCluster(t, "verda")
	c.jwksFile(t, dir)
	cfgPath := filepath.Join(dir, "trusted-issuers.yaml")
	if err := os.WriteFile(cfgPath, []byte(`issuers:
  - name: verda-k3s
    issuer: `+k8sIssuer+`
    audience: aisre
    jwks_file: verda.json
    max_lifetime: 30m
    subjects:
      "`+feederSubject+`":
        roles: [feeder]
        source_id: k8s:verda
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, base, err := LoadWorkloadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuers[0].MaxLifetime != 30*time.Minute {
		t.Fatalf("max_lifetime = %s", cfg.Issuers[0].MaxLifetime)
	}
	a, err := NewWorkloadAuthenticator(context.Background(), cfg, WorkloadOptions{BaseDir: base, Now: func() time.Time { return workloadNow }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), c.mint(t, c.claims())); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(cfgPath, []byte("issuers:\n  - name: x\n    audiance: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadWorkloadConfig(cfgPath); err == nil {
		t.Fatal("an unknown field was accepted: a typo in a trust file must not be silent")
	}
}

// The whole path a feeder takes: its projected token through the interceptor of a stream.
func TestWorkloadTokenThroughTheInterceptor(t *testing.T) {
	c := newCluster(t, "verda")
	a := workloadAuth(t, WorkloadConfig{Issuers: []WorkloadIssuer{
		feederEntry("verda-k3s", c.jwksFile(t, t.TempDir()), "k8s:verda"),
	}}, nil)
	ts := newTestServer(t, NewAuthInterceptor(a, WithAuthLogger(slog.New(slog.DiscardHandler))))

	if err := ts.feed(t, c.mint(t, c.claims())); err != nil {
		t.Fatalf("feed with the projected token: %v", err)
	}
	if p := <-ts.seen; p == nil || p.SourceID != "k8s:verda" || !p.Has(RoleFeeder) {
		t.Fatalf("principal seen by the handler = %+v", p)
	}

	apiToken := c.claims()
	apiToken["aud"] = []string{"https://kubernetes.default.svc.cluster.local"}
	err := ts.feed(t, c.mint(t, apiToken))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("feed with the pod's API token: %v, want Unauthenticated", err)
	}
}

// The example shipped next to the feeder's manifest must stay a file serve accepts.
func TestWorkloadExampleFileParses(t *testing.T) {
	cfg, _, err := LoadWorkloadConfig(filepath.Join("..", "..", "deploy", "k8s-feeder", "trusted-issuers.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Issuers) != 2 || cfg.Issuers[0].Audience != "aisre" {
		t.Fatalf("example = %+v", cfg)
	}
	if _, err := buildGrants(cfg.Issuers[0].Subjects); err != nil {
		t.Fatalf("example grants: %v", err)
	}
}
