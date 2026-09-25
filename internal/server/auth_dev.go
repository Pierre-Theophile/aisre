// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"gopkg.in/yaml.v3"
)

// Defaults for the dev identity provider.
const (
	// DefaultDevIssuer is the `iss` of dev tokens. It is deliberately not a URL, so that a
	// dev principal key (`sre-agent-dev|alice`) can never be mistaken for a real one.
	DefaultDevIssuer = "sre-agent-dev"
	// DefaultDevAudience is the `aud` of dev tokens.
	DefaultDevAudience = "sre-agent"
	// DefaultDevTokenTTL is how long a minted dev token stays valid.
	DefaultDevTokenTTL = 12 * time.Hour
	// minDevKeyBytes is the HS256 minimum key size.
	minDevKeyBytes = 32
)

// devDefaultKey is the shared secret used when DevConfig.Key is empty, so that
// `aisre dev-token` in one process and `aisre serve --auth dev --dev` in another
// agree without any configuration. It is a published constant: dev tokens are not a security
// boundary, which is exactly why the provider refuses to start without --dev and shouts on
// startup.
var devDefaultKey = []byte("sre-agent-dev-insecure-shared-key")

// DevUser is one entry of the dev users file (research §7: "signed tokens for named test
// users from a local file"). The file is YAML or JSON, either a top-level list or a mapping
// with a `users:` key:
//
//	users:
//	  - name: alice
//	    email: alice@example.com
//	    display_name: Alice Example
//	    roles: [reader, decider]
//	  - name: otel-feeder
//	    roles: [feeder]
//	    source_id: otel:demo
type DevUser struct {
	Name        string
	Email       string
	DisplayName string
	Roles       []Role
	SourceID    string
}

type devUserFile struct {
	Name        string   `json:"name" yaml:"name"`
	Email       string   `json:"email" yaml:"email"`
	DisplayName string   `json:"display_name" yaml:"display_name"`
	Roles       []string `json:"roles" yaml:"roles"`
	SourceID    string   `json:"source_id" yaml:"source_id"`
}

type devUsersDoc struct {
	Users []devUserFile `json:"users" yaml:"users"`
}

// DevConfig configures the dev identity provider used by fixtures, CI and laptops.
type DevConfig struct {
	// Enabled mirrors the `--dev` flag. Every dev constructor refuses to build unless it is
	// true, so that `--auth dev` alone can never silently disable real authentication.
	Enabled bool
	// Issuer and Audience default to DefaultDevIssuer and DefaultDevAudience.
	Issuer   string
	Audience string
	// Key is the HMAC-SHA256 signing secret. Empty means the published default, which is
	// what makes `dev-token` work out of the box against `serve --auth dev --dev`.
	Key []byte
	// UsersFile is an optional YAML or JSON file of named users. When set, only the users it
	// names may be minted, and the roles it declares are the ones they get.
	UsersFile string
	// TokenTTL defaults to DefaultDevTokenTTL.
	TokenTTL time.Duration
	// Logger receives the startup banner. Defaults to slog.Default().
	Logger *slog.Logger
	// Now overrides the clock, for tests.
	Now func() time.Time
}

func (c DevConfig) issuer() string {
	return orDefault(c.Issuer, DefaultDevIssuer)
}

func (c DevConfig) audience() string {
	return orDefault(c.Audience, DefaultDevAudience)
}

func (c DevConfig) key() []byte {
	if len(c.Key) > 0 {
		return c.Key
	}
	return devDefaultKey
}

func (c DevConfig) ttl() time.Duration {
	if c.TokenTTL > 0 {
		return c.TokenTTL
	}
	return DefaultDevTokenTTL
}

func (c DevConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c DevConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// devClaims is the dev token payload. It is a plain JWT, not an OIDC ID token: there is no
// provider to discover.
type devClaims struct {
	Issuer     string   `json:"iss"`
	Subject    string   `json:"sub"`
	Audience   string   `json:"aud"`
	IssuedAt   int64    `json:"iat"`
	Expiry     int64    `json:"exp"`
	Email      string   `json:"email,omitempty"`
	Name       string   `json:"name,omitempty"`
	Roles      []string `json:"roles"`
	SourceID   string   `json:"source_id,omitempty"`
	Provenance string   `json:"sre_agent_dev"`
}

// DevIssuer mints dev tokens. The `dev-token` CLI command (contracts/cli.md §Fixtures) is a
// thin wrapper over Mint.
type DevIssuer struct {
	cfg   DevConfig
	users map[string]DevUser
	order []string
}

// NewDevIssuer builds a token minter. It returns ErrDevAuthDisabled unless cfg.Enabled — the
// same guard as NewDevAuthenticator, so a CLI that forgets --dev cannot mint credentials that
// a correctly configured server would have refused anyway.
func NewDevIssuer(cfg DevConfig) (*DevIssuer, error) {
	if !cfg.Enabled {
		return nil, ErrDevAuthDisabled
	}
	if len(cfg.Key) > 0 && len(cfg.Key) < minDevKeyBytes {
		return nil, fmt.Errorf("dev: signing key must be at least %d bytes, got %d", minDevKeyBytes, len(cfg.Key))
	}
	users, order, err := loadDevUsers(cfg.UsersFile)
	if err != nil {
		return nil, err
	}
	return &DevIssuer{cfg: cfg, users: users, order: order}, nil
}

// Users returns the users declared in the users file, in file order. It is empty when no
// users file was configured, in which case any name may be minted.
func (i *DevIssuer) Users() []DevUser {
	out := make([]DevUser, 0, len(i.order))
	for _, name := range i.order {
		out = append(out, i.users[name])
	}
	return out
}

// User looks a declared user up by name.
func (i *DevIssuer) User(name string) (DevUser, bool) {
	u, ok := i.users[name]
	return u, ok
}

// Mint signs a token for user. When a users file is configured, the user must appear in it;
// roles, when non-empty, must be a subset of the roles the file grants, so that the file
// stays the single statement of who may do what. When no users file is configured, roles are
// taken at face value.
func (i *DevIssuer) Mint(user string, roles []Role) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", errors.New("dev: user name is required")
	}
	declared, known := i.users[user]
	if len(i.users) > 0 && !known {
		return "", fmt.Errorf("dev: user %q is not declared in %s", user, i.cfg.UsersFile)
	}
	if len(roles) == 0 {
		roles = declared.Roles
	} else if known {
		for _, want := range roles {
			if !hasRole(declared.Roles, want) {
				return "", fmt.Errorf("dev: user %q is not granted role %q in %s", user, want, i.cfg.UsersFile)
			}
		}
	}
	if len(roles) == 0 {
		return "", fmt.Errorf("dev: no roles for user %q; pass --roles", user)
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, string(r))
	}
	if hasRole(roles, RoleFeeder) && declared.SourceID == "" {
		return "", fmt.Errorf("dev: user %q has the feeder role but no source_id: %w", user, ErrFeederWithoutSource)
	}
	now := i.cfg.now()
	claims := devClaims{
		Issuer:     i.cfg.issuer(),
		Subject:    user,
		Audience:   i.cfg.audience(),
		IssuedAt:   now.Unix(),
		Expiry:     now.Add(i.cfg.ttl()).Unix(),
		Email:      declared.Email,
		Name:       declared.DisplayName,
		Roles:      names,
		SourceID:   declared.SourceID,
		Provenance: "true",
	}
	if claims.Name == "" {
		claims.Name = user
	}
	return signDevToken(i.cfg.key(), claims)
}

// MintFor signs a token for an explicit user description, bypassing the users file. It is
// what fixture and test helpers use to build a principal that does not exist on disk.
func (i *DevIssuer) MintFor(u DevUser) (string, error) {
	if u.Name == "" {
		return "", errors.New("dev: user name is required")
	}
	if hasRole(u.Roles, RoleFeeder) && u.SourceID == "" {
		return "", fmt.Errorf("dev: user %q has the feeder role but no source_id: %w", u.Name, ErrFeederWithoutSource)
	}
	names := make([]string, 0, len(u.Roles))
	for _, r := range u.Roles {
		names = append(names, string(r))
	}
	now := i.cfg.now()
	return signDevToken(i.cfg.key(), devClaims{
		Issuer:     i.cfg.issuer(),
		Subject:    u.Name,
		Audience:   i.cfg.audience(),
		IssuedAt:   now.Unix(),
		Expiry:     now.Add(i.cfg.ttl()).Unix(),
		Email:      u.Email,
		Name:       orDefault(u.DisplayName, u.Name),
		Roles:      names,
		SourceID:   u.SourceID,
		Provenance: "true",
	})
}

func signDevToken(key []byte, claims devClaims) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("dev: signer: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("dev: marshal claims: %w", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("dev: sign: %w", err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		return "", fmt.Errorf("dev: serialize: %w", err)
	}
	return token, nil
}

func hasRole(roles []Role, want Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// DevAuthenticator verifies tokens minted by DevIssuer. It exists so that fixtures, CI and
// laptops need no identity provider (research §7); it is never a production path.
type DevAuthenticator struct {
	cfg    DevConfig
	issuer *DevIssuer
}

var _ Authenticator = (*DevAuthenticator)(nil)

// NewDevAuthenticator builds the dev provider. It returns ErrDevAuthDisabled unless
// cfg.Enabled is true (the `--dev` flag), and logs a banner when it is: an operator who sees
// this line in production logs has a live incident.
func NewDevAuthenticator(cfg DevConfig) (*DevAuthenticator, error) {
	if !cfg.Enabled {
		return nil, ErrDevAuthDisabled
	}
	issuer, err := NewDevIssuer(cfg)
	if err != nil {
		return nil, err
	}
	logDevBanner(cfg, issuer)
	return &DevAuthenticator{cfg: cfg, issuer: issuer}, nil
}

// Issuer returns the minter that matches this authenticator, so that `serve --auth dev --dev`
// can hand out tokens without a second configuration path.
func (a *DevAuthenticator) Issuer() *DevIssuer { return a.issuer }

func logDevBanner(cfg DevConfig, issuer *DevIssuer) {
	names := make([]string, 0, len(issuer.order))
	names = append(names, issuer.order...)
	logger := cfg.logger()
	logger.Warn("############################################################")
	logger.Warn("#  DEV AUTHENTICATION ENABLED - NOT FOR PRODUCTION USE     #")
	logger.Warn("#  Identities are self-asserted and signed with a local     #")
	logger.Warn("#  key. No identity provider vouches for anyone (FR-041a). #")
	logger.Warn("############################################################",
		"issuer", cfg.issuer(),
		"audience", cfg.audience(),
		"users_file", orDefault(cfg.UsersFile, "<none>"),
		"users", names,
		"default_key", len(cfg.Key) == 0,
	)
}

// Authenticate implements Authenticator. It checks the HMAC signature, issuer, audience and
// expiry of a locally minted token. Unlike the OIDC path there is no key set to fetch, so the
// context is unused.
func (a *DevAuthenticator) Authenticate(_ context.Context, bearerToken string) (*Principal, error) {
	if strings.TrimSpace(bearerToken) == "" {
		return nil, ErrMissingToken
	}
	parsed, err := jose.ParseSigned(bearerToken, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	payload, err := parsed.Verify(a.cfg.key())
	if err != nil {
		return nil, fmt.Errorf("%w: bad signature", ErrInvalidToken)
	}
	var claims devClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: unreadable claims: %w", ErrInvalidToken, err)
	}
	if claims.Issuer != a.cfg.issuer() {
		return nil, fmt.Errorf("%w: unexpected issuer", ErrInvalidToken)
	}
	if claims.Audience != a.cfg.audience() {
		return nil, fmt.Errorf("%w: unexpected audience", ErrInvalidToken)
	}
	now := a.cfg.now()
	if claims.Expiry == 0 || !now.Before(time.Unix(claims.Expiry, 0)) {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if claims.Subject == "" {
		return nil, ErrAnonymous
	}
	p := &Principal{
		Issuer:      claims.Issuer,
		Subject:     claims.Subject,
		Email:       claims.Email,
		DisplayName: orDefault(claims.Name, claims.Subject),
		SourceID:    claims.SourceID,
	}
	for _, name := range claims.Roles {
		role, err := ParseRole(name)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
		}
		p.Roles = appendRole(p.Roles, role)
	}
	if p.Has(RoleFeeder) && p.SourceID == "" {
		return nil, ErrFeederWithoutSource
	}
	return p, nil
}

func loadDevUsers(path string) (map[string]DevUser, []string, error) {
	if path == "" {
		return map[string]DevUser{}, nil, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied dev path
	if err != nil {
		return nil, nil, fmt.Errorf("dev: read users file: %w", err)
	}
	var doc devUsersDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		// A top-level list is also accepted. JSON is a subset of YAML, so both spellings
		// of both shapes go through the same parser.
		var list []devUserFile
		if listErr := yaml.Unmarshal(raw, &list); listErr != nil {
			return nil, nil, fmt.Errorf("dev: parse users file %s: %w", path, err)
		}
		doc.Users = list
	}
	if len(doc.Users) == 0 {
		var list []devUserFile
		if err := yaml.Unmarshal(raw, &list); err == nil {
			doc.Users = list
		}
	}
	users := make(map[string]DevUser, len(doc.Users))
	order := make([]string, 0, len(doc.Users))
	for _, entry := range doc.Users {
		if entry.Name == "" {
			return nil, nil, fmt.Errorf("dev: users file %s: entry without a name", path)
		}
		if _, dup := users[entry.Name]; dup {
			return nil, nil, fmt.Errorf("dev: users file %s: duplicate user %q", path, entry.Name)
		}
		user := DevUser{
			Name:        entry.Name,
			Email:       entry.Email,
			DisplayName: orDefault(entry.DisplayName, entry.Name),
			SourceID:    entry.SourceID,
		}
		for _, name := range entry.Roles {
			role, err := ParseRole(name)
			if err != nil {
				return nil, nil, fmt.Errorf("dev: users file %s: user %q: %w", path, entry.Name, err)
			}
			user.Roles = appendRole(user.Roles, role)
		}
		if hasRole(user.Roles, RoleFeeder) && user.SourceID == "" {
			return nil, nil, fmt.Errorf("dev: users file %s: user %q: %w", path, entry.Name, ErrFeederWithoutSource)
		}
		users[entry.Name] = user
		order = append(order, entry.Name)
	}
	return users, order, nil
}
