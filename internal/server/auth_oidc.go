// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Default claim names used by OIDCConfig when a field is left empty.
const (
	DefaultRolesClaim    = "roles"
	DefaultEmailClaim    = "email"
	DefaultNameClaim     = "name"
	DefaultSourceIDClaim = "source_id"
)

// OIDCConfig configures NewOIDCAuthenticator against the organisation's identity provider
// (research §7: OIDC is what every organisation already has, and FR-041a requires individual
// identity from v1).
type OIDCConfig struct {
	// IssuerURL is the provider's issuer, e.g. https://login.example.com/realms/prod. The
	// provider's discovery document must advertise exactly this value.
	IssuerURL string
	// Audience is the expected `aud` claim — normally the client ID registered for the graph.
	// Required unless SkipAudienceCheck is set.
	Audience string
	// SkipAudienceCheck disables the `aud` check. Only for providers that scope tokens some
	// other way; it widens the blast radius of a stolen token, so it is opt-in and loud.
	SkipAudienceCheck bool
	// RolesClaim is the dotted path to the roles claim, e.g. "roles" (default) or
	// "realm_access.roles" for Keycloak. The value may be a list of strings or a single
	// space-separated string.
	RolesClaim string
	// RoleMapping maps provider role names onto graph roles, e.g.
	// {"sre-reader": RoleReader, "sre-oncall": RoleDecider}. Unmapped values are ignored.
	// When nil, the identity mapping {"reader","decider","feeder"} is used.
	RoleMapping map[string]Role
	// EmailClaim, NameClaim and SourceIDClaim override the claim names for the principal's
	// email, display name and feeder source scope.
	EmailClaim    string
	NameClaim     string
	SourceIDClaim string
	// SupportedSigningAlgs restricts the accepted JWS algorithms. Defaults to whatever the
	// provider advertises, falling back to RS256.
	SupportedSigningAlgs []string
	// HTTPClient fetches discovery and JWKS documents. Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// OIDCAuthenticator validates bearer tokens issued by a configured OIDC provider.
type OIDCAuthenticator struct {
	cfg      OIDCConfig
	verifier *oidc.IDTokenVerifier
	mapping  map[string]Role
}

var _ Authenticator = (*OIDCAuthenticator)(nil)

// NewOIDCAuthenticator discovers the provider's metadata and JWKS and returns an
// Authenticator that verifies signature, issuer, audience and expiry on every call. The JWKS
// is refreshed lazily by go-oidc, so key rotation needs no restart.
//
// ctx is used for discovery and is retained for background JWKS refreshes; cancel it only
// when the server is shutting down.
func NewOIDCAuthenticator(ctx context.Context, cfg OIDCConfig) (*OIDCAuthenticator, error) {
	if cfg.IssuerURL == "" {
		return nil, errors.New("oidc: issuer URL is required")
	}
	if cfg.Audience == "" && !cfg.SkipAudienceCheck {
		return nil, errors.New("oidc: audience is required (or set SkipAudienceCheck)")
	}
	if cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, cfg.HTTPClient)
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery for %q: %w", cfg.IssuerURL, err)
	}
	verifier := provider.VerifierContext(ctx, &oidc.Config{
		ClientID:             cfg.Audience,
		SkipClientIDCheck:    cfg.SkipAudienceCheck,
		SupportedSigningAlgs: cfg.SupportedSigningAlgs,
		Now:                  cfg.Now,
	})
	return &OIDCAuthenticator{cfg: cfg, verifier: verifier, mapping: roleMapping(cfg.RoleMapping)}, nil
}

// NewOIDCAuthenticatorWithVerifier wraps an already-constructed verifier. It exists so that a
// caller that has its own key set (an offline JWKS, a second issuer) can reuse the claim
// handling without re-running discovery.
func NewOIDCAuthenticatorWithVerifier(verifier *oidc.IDTokenVerifier, cfg OIDCConfig) *OIDCAuthenticator {
	return &OIDCAuthenticator{cfg: cfg, verifier: verifier, mapping: roleMapping(cfg.RoleMapping)}
}

func roleMapping(m map[string]Role) map[string]Role {
	if len(m) > 0 {
		out := make(map[string]Role, len(m))
		for k, v := range m {
			out[strings.ToLower(k)] = v
		}
		return out
	}
	return map[string]Role{
		string(RoleReader):  RoleReader,
		string(RoleDecider): RoleDecider,
		string(RoleFeeder):  RoleFeeder,
	}
}

// Authenticate implements Authenticator. It returns ErrInvalidToken for any verification
// failure (expired, wrong audience, wrong issuer, bad signature) without disclosing which,
// ErrAnonymous when the token names no subject, and ErrFeederWithoutSource when a feeder
// token is not scoped to a source.
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, bearerToken string) (*Principal, error) {
	if strings.TrimSpace(bearerToken) == "" {
		return nil, ErrMissingToken
	}
	if a.cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, a.cfg.HTTPClient)
	}
	idToken, err := a.verifier.Verify(ctx, bearerToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: unreadable claims: %w", ErrInvalidToken, err)
	}
	p := &Principal{
		Issuer:      idToken.Issuer,
		Subject:     idToken.Subject,
		Email:       claimString(claims, orDefault(a.cfg.EmailClaim, DefaultEmailClaim)),
		DisplayName: claimString(claims, orDefault(a.cfg.NameClaim, DefaultNameClaim)),
		SourceID:    claimString(claims, orDefault(a.cfg.SourceIDClaim, DefaultSourceIDClaim)),
	}
	if p.DisplayName == "" {
		p.DisplayName = claimString(claims, "preferred_username")
	}
	if p.Issuer == "" || p.Subject == "" {
		return nil, ErrAnonymous
	}
	for _, name := range claimStrings(claims, orDefault(a.cfg.RolesClaim, DefaultRolesClaim)) {
		if role, ok := a.mapping[strings.ToLower(strings.TrimSpace(name))]; ok {
			p.Roles = appendRole(p.Roles, role)
		}
	}
	if p.Has(RoleFeeder) && p.SourceID == "" {
		return nil, ErrFeederWithoutSource
	}
	return p, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// claimValue walks a dotted claim path such as "realm_access.roles" through nested objects.
func claimValue(claims map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	var current any = claims
	for _, segment := range strings.Split(path, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = obj[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func claimString(claims map[string]any, path string) string {
	v, ok := claimValue(claims, path)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// claimStrings reads a claim that may be a JSON array of strings or a single space-separated
// string (both spellings are in the wild).
func claimStrings(claims map[string]any, path string) []string {
	v, ok := claimValue(claims, path)
	if !ok {
		return nil
	}
	switch typed := v.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return typed
	case string:
		return strings.Fields(typed)
	default:
		return nil
	}
}
