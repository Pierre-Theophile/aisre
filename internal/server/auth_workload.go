// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"gopkg.in/yaml.v3"
)

// Workload identity: the graph trusts a short list of token issuers that are not the
// organisation's IdP — a Kubernetes cluster's ServiceAccount issuer, GitHub Actions, Google —
// and maps each exact subject it knows onto roles and a source. A feeder in a cluster then
// authenticates with the projected ServiceAccount token the kubelet already rotates for it,
// and nothing secret is stored anywhere.
//
// The trust boundary is all in this file, so every rule below is a requirement with a test
// (auth_workload_test.go), not tuning:
//
//  1. Every issuer has a required audience, checked on every token. The pod's ordinary
//     Kubernetes token, or a token minted for another service, is refused.
//  2. The grant is chosen by the key that verified the signature, never by `iss` alone. Every
//     k3s and kubeadm cluster defaults to the same issuer URL, so `iss` cannot tell one
//     cluster's tokens from another's; two entries may share an issuer only with disjoint keys.
//  3. Subjects match exactly, with no wildcard. Roles and source come from the grant only,
//     never from the token's claims.
//  4. A token's lifetime (exp − iat) is capped per issuer, so a year-long token minted with
//     `kubectl create token --duration` is refused; a token without exp or iat is refused.
//  5. Signing algorithms are pinned per issuer, and only asymmetric public keys are loaded.
//  6. Each key's thumbprint is logged at start-up, so a swapped key file is visible.
//
// Revocation is removing the entry (or the subject) and restarting: offline verification
// cannot see that a ServiceAccount was deleted. Short lifetimes bound what that costs.

// Defaults for a workload issuer entry.
const (
	// DefaultWorkloadMaxLifetime caps exp − iat when an entry does not say. A projected
	// ServiceAccount token defaults to one hour, and so does a Google identity token.
	DefaultWorkloadMaxLifetime = time.Hour
	// workloadClockSkew is how far in the future an `iat` may be before it is refused.
	workloadClockSkew = time.Minute
)

// DefaultWorkloadAlgorithms are the signing algorithms accepted when an entry names none.
var DefaultWorkloadAlgorithms = []string{oidc.RS256, oidc.ES256}

// workloadAlgorithms are the algorithms an entry may name: asymmetric only.
var workloadAlgorithms = []string{
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512,
	oidc.EdDSA,
}

// ErrUntrustedIssuer is returned, wrapped in ErrInvalidToken, when a token names an issuer
// that no workload entry trusts. A WorkloadAuthenticator with a fallback hands such a token to
// the fallback instead.
var ErrUntrustedIssuer = errors.New("token issuer is not trusted")

// WorkloadConfig is the trusted-issuers file `serve --trusted-issuers` reads.
type WorkloadConfig struct {
	Issuers []WorkloadIssuer `yaml:"issuers"`
}

// WorkloadIssuer is one trusted issuer and the subjects it may vouch for.
type WorkloadIssuer struct {
	// Name labels the entry in logs, e.g. "verda-k3s". Required, unique.
	Name string `yaml:"name"`
	// Issuer is the exact `iss` the tokens carry.
	Issuer string `yaml:"issuer"`
	// Audience is the `aud` a token must carry. Required: it is what stops a token minted for
	// another service, the Kubernetes API included, being accepted here.
	Audience string `yaml:"audience"`
	// JWKSFile is a JSON Web Key Set on disk, relative to the config file. For an issuer
	// whose discovery is not reachable, such as a cluster's private API server
	// (`kubectl get --raw /openid/v1/jwks`). Exactly one of JWKSFile and JWKSURL.
	JWKSFile string `yaml:"jwks_file"`
	// JWKSURL is an https key set fetched and refreshed at run time, for a public issuer that
	// rotates its keys (Google: https://www.googleapis.com/oauth2/v3/certs).
	JWKSURL string `yaml:"jwks_url"`
	// Algorithms are the accepted signing algorithms. Default RS256 and ES256.
	Algorithms []string `yaml:"algorithms"`
	// MaxLifetime caps exp − iat. Default one hour.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
	// RequireEmailVerified refuses a token whose `email_verified` claim is not true. Set it
	// for human identities.
	RequireEmailVerified bool `yaml:"require_email_verified"`
	// HostedDomain, when set, must equal the token's `hd` claim (a Google Workspace domain),
	// which refuses personal accounts.
	HostedDomain string `yaml:"hosted_domain"`
	// Subjects maps each exact `sub` onto what it is granted.
	Subjects map[string]WorkloadGrant `yaml:"subjects"`
}

// WorkloadGrant is what one subject is granted.
type WorkloadGrant struct {
	// Name is the display name stamped on the principal, for audit. Default: the subject.
	Name string `yaml:"name"`
	// Roles are graph role names: reader, decider, feeder, investigator.
	Roles []string `yaml:"roles"`
	// SourceID scopes a feeder's writes to one source. Required with the feeder role.
	SourceID string `yaml:"source_id"`
}

// LoadWorkloadConfig reads and parses a trusted-issuers file. Relative key-set paths are
// resolved against the file's directory.
func LoadWorkloadConfig(path string) (WorkloadConfig, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return WorkloadConfig{}, "", fmt.Errorf("trusted issuers: %w", err)
	}
	var cfg WorkloadConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return WorkloadConfig{}, "", fmt.Errorf("trusted issuers %s: %w", path, err)
	}
	return cfg, filepath.Dir(path), nil
}

// WorkloadAuthenticator verifies tokens from the trusted workload issuers.
type WorkloadAuthenticator struct {
	byIssuer map[string][]*workloadEntry
	fallback Authenticator
	now      func() time.Time
}

var _ Authenticator = (*WorkloadAuthenticator)(nil)

type workloadEntry struct {
	cfg      WorkloadIssuer
	verifier *oidc.IDTokenVerifier
	grants   map[string]grant
	maxLife  time.Duration
}

type grant struct {
	name     string
	roles    []Role
	sourceID string
}

// WorkloadOptions configures NewWorkloadAuthenticator.
type WorkloadOptions struct {
	// BaseDir resolves relative jwks_file paths.
	BaseDir string
	// Fallback authenticates a token whose issuer no entry trusts — normally the
	// organisation's OIDC provider. Nil refuses such tokens.
	Fallback Authenticator
	// Logger receives the start-up line naming every entry and key thumbprint.
	Logger *slog.Logger
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// NewWorkloadAuthenticator validates cfg and builds the verifiers. Every rule that can be
// checked without a token is checked here, so a misconfiguration stops `serve` from starting
// rather than letting it accept the wrong tokens.
func NewWorkloadAuthenticator(ctx context.Context, cfg WorkloadConfig, opts WorkloadOptions) (*WorkloadAuthenticator, error) {
	if len(cfg.Issuers) == 0 {
		return nil, errors.New("trusted issuers: no issuer listed")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	a := &WorkloadAuthenticator{byIssuer: map[string][]*workloadEntry{}, fallback: opts.Fallback, now: now}
	names := map[string]bool{}
	// thumbprints per issuer: two entries sharing an issuer must not share a key, or the key
	// would no longer say which entry's grants apply (rule 2).
	thumbs := map[string]map[string]string{}

	for i := range cfg.Issuers {
		ic := cfg.Issuers[i]
		where := fmt.Sprintf("trusted issuers: entry %d", i+1)
		if ic.Name != "" {
			where = fmt.Sprintf("trusted issuers: %q", ic.Name)
		}
		if ic.Name == "" {
			return nil, fmt.Errorf("%s: name is required", where)
		}
		if names[ic.Name] {
			return nil, fmt.Errorf("%s: name is used twice", where)
		}
		names[ic.Name] = true
		if strings.TrimSpace(ic.Issuer) == "" {
			return nil, fmt.Errorf("%s: issuer is required", where)
		}
		if strings.TrimSpace(ic.Audience) == "" {
			return nil, fmt.Errorf("%s: audience is required: without it a token minted for any "+
				"other service, the Kubernetes API included, would authenticate here", where)
		}
		algs := ic.Algorithms
		if len(algs) == 0 {
			algs = DefaultWorkloadAlgorithms
		}
		for _, alg := range algs {
			if !slices.Contains(workloadAlgorithms, alg) {
				return nil, fmt.Errorf("%s: algorithm %q is not an asymmetric JWS algorithm", where, alg)
			}
		}
		maxLife := ic.MaxLifetime
		if maxLife == 0 {
			maxLife = DefaultWorkloadMaxLifetime
		}
		if maxLife < 0 {
			return nil, fmt.Errorf("%s: max_lifetime must be positive", where)
		}
		grants, err := buildGrants(ic.Subjects)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}

		var keySet oidc.KeySet
		var keyNote []string
		switch {
		case ic.JWKSFile != "" && ic.JWKSURL != "":
			return nil, fmt.Errorf("%s: set jwks_file or jwks_url, not both", where)
		case ic.JWKSFile != "":
			path := ic.JWKSFile
			if !filepath.IsAbs(path) {
				path = filepath.Join(opts.BaseDir, path)
			}
			keys, prints, err := loadPublicKeys(path)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			if thumbs[ic.Issuer] == nil {
				thumbs[ic.Issuer] = map[string]string{}
			}
			for _, tp := range prints {
				if other, dup := thumbs[ic.Issuer][tp]; dup {
					return nil, fmt.Errorf("%s: key %s is also trusted by %q for the same issuer, so a "+
						"token it signs could not say whose subjects apply", where, tp, other)
				}
				thumbs[ic.Issuer][tp] = ic.Name
			}
			keySet = &oidc.StaticKeySet{PublicKeys: keys}
			keyNote = prints
		case ic.JWKSURL != "":
			u, err := url.Parse(ic.JWKSURL)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, fmt.Errorf("%s: jwks_url must be an https URL: anyone on a plaintext "+
					"path could serve their own keys", where)
			}
			keySet = oidc.NewRemoteKeySet(ctx, ic.JWKSURL)
			keyNote = []string{ic.JWKSURL}
		default:
			return nil, fmt.Errorf("%s: jwks_file or jwks_url is required", where)
		}

		entry := &workloadEntry{
			cfg: ic,
			verifier: oidc.NewVerifier(ic.Issuer, keySet, &oidc.Config{
				ClientID:             ic.Audience,
				SupportedSigningAlgs: algs,
				Now:                  now,
			}),
			grants:  grants,
			maxLife: maxLife,
		}
		a.byIssuer[ic.Issuer] = append(a.byIssuer[ic.Issuer], entry)
		logger.Info("trusted workload issuer",
			"name", ic.Name, "issuer", ic.Issuer, "audience", ic.Audience,
			"keys", strings.Join(keyNote, ","), "subjects", len(grants),
			"max_lifetime", maxLife.String())
	}
	return a, nil
}

func buildGrants(subjects map[string]WorkloadGrant) (map[string]grant, error) {
	if len(subjects) == 0 {
		return nil, errors.New("no subject is granted anything")
	}
	out := make(map[string]grant, len(subjects))
	for sub, g := range subjects {
		if strings.TrimSpace(sub) == "" {
			return nil, errors.New("an empty subject")
		}
		if strings.ContainsAny(sub, "*?") {
			return nil, fmt.Errorf("subject %q: subjects match exactly; wildcards are not supported", sub)
		}
		if len(g.Roles) == 0 {
			return nil, fmt.Errorf("subject %q: no role", sub)
		}
		var roles []Role
		for _, name := range g.Roles {
			role, err := ParseRole(name)
			if err != nil {
				return nil, fmt.Errorf("subject %q: %w", sub, err)
			}
			roles = appendRole(roles, role)
		}
		if slices.Contains(roles, RoleFeeder) && strings.TrimSpace(g.SourceID) == "" {
			return nil, fmt.Errorf("subject %q: the feeder role needs a source_id", sub)
		}
		if !slices.Contains(roles, RoleFeeder) && g.SourceID != "" {
			return nil, fmt.Errorf("subject %q: source_id only scopes the feeder role", sub)
		}
		name := g.Name
		if name == "" {
			name = sub
		}
		out[sub] = grant{name: name, roles: roles, sourceID: g.SourceID}
	}
	return out, nil
}

// loadPublicKeys reads a JWKS file and keeps only asymmetric public keys, returning each
// key's RFC 7638 thumbprint for the start-up log.
func loadPublicKeys(path string) ([]crypto.PublicKey, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("jwks_file: %w", err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, nil, fmt.Errorf("jwks_file %s: %w", path, err)
	}
	if len(set.Keys) == 0 {
		return nil, nil, fmt.Errorf("jwks_file %s: no key", path)
	}
	keys := make([]crypto.PublicKey, 0, len(set.Keys))
	prints := make([]string, 0, len(set.Keys))
	for i, k := range set.Keys {
		switch k.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		default:
			return nil, nil, fmt.Errorf("jwks_file %s: key %d is not an asymmetric public key; "+
				"a shared or private key does not belong in a trust file", path, i+1)
		}
		tp, err := k.Thumbprint(crypto.SHA256)
		if err != nil {
			return nil, nil, fmt.Errorf("jwks_file %s: key %d: %w", path, i+1, err)
		}
		keys = append(keys, k.Key)
		prints = append(prints, "sha256:"+hex.EncodeToString(tp)[:16])
	}
	return keys, prints, nil
}

// Authenticate implements Authenticator.
func (a *WorkloadAuthenticator) Authenticate(ctx context.Context, bearerToken string) (*Principal, error) {
	if strings.TrimSpace(bearerToken) == "" {
		return nil, ErrMissingToken
	}
	iss, err := unverifiedIssuer(bearerToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	entries := a.byIssuer[iss]
	if len(entries) == 0 {
		if a.fallback != nil {
			return a.fallback.Authenticate(ctx, bearerToken)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, ErrUntrustedIssuer)
	}
	// Each entry verifies with its own keys only, so the entry whose verifier succeeds is the
	// entry whose key signed the token, and only its grants apply (rule 2).
	for _, e := range entries {
		idToken, err := e.verifier.Verify(ctx, bearerToken)
		if err != nil {
			continue
		}
		return e.principal(idToken, a.now())
	}
	return nil, ErrInvalidToken
}

func (e *workloadEntry) principal(idToken *oidc.IDToken, now time.Time) (*Principal, error) {
	var claims struct {
		IssuedAt      *int64 `json:"iat"`
		Expiry        *int64 `json:"exp"`
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		HostedDomain  string `json:"hd"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: unreadable claims: %w", ErrInvalidToken, err)
	}
	if claims.IssuedAt == nil || claims.Expiry == nil {
		return nil, fmt.Errorf("%w: a workload token must carry iat and exp", ErrInvalidToken)
	}
	iat, exp := time.Unix(*claims.IssuedAt, 0), time.Unix(*claims.Expiry, 0)
	if iat.After(now.Add(workloadClockSkew)) {
		return nil, fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	if exp.Sub(iat) > e.maxLife {
		return nil, fmt.Errorf("%w: lifetime %s is over the %s this issuer allows",
			ErrInvalidToken, exp.Sub(iat), e.maxLife)
	}
	if e.cfg.RequireEmailVerified && !isTrue(claims.EmailVerified) {
		return nil, fmt.Errorf("%w: email is not verified", ErrInvalidToken)
	}
	if e.cfg.HostedDomain != "" && claims.HostedDomain != e.cfg.HostedDomain {
		return nil, fmt.Errorf("%w: not in the required hosted domain", ErrInvalidToken)
	}
	if idToken.Subject == "" {
		return nil, ErrAnonymous
	}
	g, ok := e.grants[idToken.Subject]
	if !ok {
		return nil, fmt.Errorf("%w: subject is not granted anything by %q", ErrInvalidToken, e.cfg.Name)
	}
	return &Principal{
		Issuer:      idToken.Issuer,
		Subject:     idToken.Subject,
		Email:       claims.Email,
		DisplayName: g.name,
		Roles:       slices.Clone(g.roles),
		SourceID:    g.sourceID,
	}, nil
}

// isTrue reads `email_verified`, which providers send as a boolean or as the string "true".
func isTrue(v any) bool {
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		return typed == "true"
	default:
		return false
	}
}

// unverifiedIssuer reads `iss` from a compact JWS without verifying it. It only chooses which
// entries to try; nothing the token says is trusted until one of them verifies it.
func unverifiedIssuer(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("not a compact JWS")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("payload is not base64url")
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("payload is not JSON")
	}
	return claims.Issuer, nil
}
