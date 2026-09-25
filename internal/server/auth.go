// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

// Role is a coarse permission granted to a principal by the identity provider.
//
// The taxonomy is fixed (research §7): the graph has exactly three things a caller can be
// allowed to do — read it, decide entity-resolution questions about it, and feed events into
// it. Anything finer is a policy decision that belongs above this layer.
type Role string

const (
	// RoleReader may run every query in contracts/cli.md §Queries (FR-035).
	RoleReader Role = "reader"
	// RoleDecider may record entity-resolution decisions (FR-040, FR-041). It implies
	// RoleReader: a decision is meaningless without seeing what is being decided.
	RoleDecider Role = "decider"
	// RoleFeeder may ingest events for the single source named by the token's `source_id`
	// claim. It implies nothing else: a feeder cannot query the graph or decide anything.
	RoleFeeder Role = "feeder"
	// RoleInvestigator may run investigations and push human input at them: Investigate,
	// Declare, Replay, Reopen and SubmitHumanFact (002 FR-064, plan F10). It implies
	// RoleReader, because an investigation is a very elaborate read of the graph and a caller
	// who may run one may certainly read what it produced.
	//
	// It is deliberately *not* implied by RoleDecider. Deciding an entity-resolution question
	// and spending an investigation's budget against a vendor's quota are different
	// authorities, and an operator who grants one has not thereby granted the other.
	RoleInvestigator Role = "investigator"
)

// AllRoles lists every role the graph understands, in increasing order of privilege.
var AllRoles = []Role{RoleReader, RoleDecider, RoleFeeder, RoleInvestigator}

// ParseRole maps a role name, or one of its single-letter CLI aliases (`r`, `d`, `f`; see
// contracts/cli.md `dev-token --roles r,d`), onto a Role. Matching is case-insensitive and
// tolerant of surrounding whitespace.
func ParseRole(s string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "r", "reader":
		return RoleReader, nil
	case "d", "decider":
		return RoleDecider, nil
	case "f", "feeder":
		return RoleFeeder, nil
	case "i", "investigator":
		return RoleInvestigator, nil
	default:
		return "", fmt.Errorf("%w: %q (want one of reader|decider|feeder|investigator)",
			ErrUnknownRole, s)
	}
}

// ParseRoles parses a comma-separated role list such as `r,d` or `reader,decider`. Empty
// items are ignored; duplicates are collapsed. An empty list is not an error — it yields a
// principal that can authenticate but is authorized for nothing.
func ParseRoles(csv string) ([]Role, error) {
	var out []Role
	for _, item := range strings.Split(csv, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		role, err := ParseRole(item)
		if err != nil {
			return nil, err
		}
		out = appendRole(out, role)
	}
	return out, nil
}

func appendRole(roles []Role, role Role) []Role {
	for _, existing := range roles {
		if existing == role {
			return roles
		}
	}
	return append(roles, role)
}

// Principal is the authenticated individual (or feeder) behind a call. FR-041a requires one
// on every query and every decision from the first version; FR-041 requires that resolution
// decisions record it.
type Principal struct {
	// Issuer is the `iss` claim: the identity provider that vouched for this identity.
	Issuer string
	// Subject is the `sub` claim: the provider-stable identifier of the individual.
	Subject string
	// Email is the `email` claim, when the provider releases one. Informational only —
	// never an identity key, because email addresses are reassigned.
	Email string
	// DisplayName is the `name` (or `preferred_username`) claim, for human-readable audit.
	DisplayName string
	// Roles are the roles the provider granted, after mapping through OIDCConfig.RoleMapping.
	Roles []Role
	// SourceID is the `source_id` claim, set only on feeder tokens. It scopes ingestion to
	// exactly one source (FR-046: a feeder may not write on another feeder's behalf).
	SourceID string
}

// Key returns the stable principal identifier `iss|sub` stored in `graph.principals.principal`
// and stamped on resolution decisions (data-model.md §graph.principals, FR-041).
func (p *Principal) Key() string {
	return p.Issuer + "|" + p.Subject
}

// Has reports whether the principal holds role, applying the two implications in the model:
// RoleDecider implies RoleReader (research §7), and so does RoleInvestigator (002 plan F10).
// RoleFeeder implies nothing.
func (p *Principal) Has(role Role) bool {
	if p == nil {
		return false
	}
	for _, held := range p.Roles {
		if held == role {
			return true
		}
		if role == RoleReader && (held == RoleDecider || held == RoleInvestigator) {
			return true
		}
	}
	return false
}

// String renders the principal for logs: the key plus the roles, never the raw token.
func (p *Principal) String() string {
	if p == nil {
		return "<anonymous>"
	}
	names := make([]string, 0, len(p.Roles))
	for _, role := range p.Roles {
		names = append(names, string(role))
	}
	return fmt.Sprintf("%s[%s]", p.Key(), strings.Join(names, ","))
}

// Authenticator turns a bearer token into a Principal, or fails. Implementations must treat
// every input as hostile and must never return a Principal with an empty Issuer or Subject:
// an unidentifiable caller is an anonymous caller, which FR-041 forbids.
type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (*Principal, error)
}

// AuthenticatorFunc adapts a plain function to Authenticator.
type AuthenticatorFunc func(ctx context.Context, bearerToken string) (*Principal, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(ctx context.Context, bearerToken string) (*Principal, error) {
	return f(ctx, bearerToken)
}

// Authentication and authorization failures. Every one of these maps to exit code 3 at the
// CLI (contracts/cli.md §Exit codes).
var (
	// ErrMissingToken is returned when a call carries no `Authorization: Bearer` header.
	ErrMissingToken = errors.New("no bearer token in Authorization header")
	// ErrInvalidToken is returned when a token fails signature, issuer, audience or expiry
	// validation. It deliberately carries no detail about which check failed.
	ErrInvalidToken = errors.New("bearer token is not valid")
	// ErrAnonymous is returned when a token verifies but names no individual.
	ErrAnonymous = errors.New("bearer token does not identify an individual")
	// ErrFeederWithoutSource is returned when a token carries the feeder role but no
	// `source_id` claim, so its writes could not be scoped to one source.
	ErrFeederWithoutSource = errors.New("feeder token carries no source_id claim")
	// ErrUnknownRole is returned when a role name is not one of reader, decider, feeder,
	// investigator.
	ErrUnknownRole = errors.New("unknown role")
	// ErrDevAuthDisabled is returned by the dev provider constructors when --dev was not set.
	ErrDevAuthDisabled = errors.New("dev authentication requires --dev; refusing to start")
)

type principalContextKey struct{}

// ContextWithPrincipal returns ctx carrying p. The auth interceptor calls it on every
// authenticated request; tests and in-process callers may call it directly.
func ContextWithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFrom returns the authenticated principal carried by ctx, if any.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*Principal)
	return p, ok && p != nil
}

// Require returns nil when ctx carries a principal holding role. It returns a
// connect.CodeUnauthenticated error when there is no principal (an anonymous call, forbidden
// by FR-041a) and a connect.CodePermissionDenied error when the principal lacks the role.
// Handlers call it first thing: `if err := server.Require(ctx, server.RoleDecider); err != nil`.
func Require(ctx context.Context, role Role) error {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, ErrMissingToken)
	}
	if !p.Has(role) {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("principal %s lacks role %q", p.Key(), role))
	}
	return nil
}

// RequireSource returns nil when ctx carries a feeder principal scoped to sourceID. Ingestion
// handlers use it so that a feeder token can only append events for its own source.
func RequireSource(ctx context.Context, sourceID string) error {
	if err := Require(ctx, RoleFeeder); err != nil {
		return err
	}
	p, _ := PrincipalFrom(ctx)
	if p.SourceID != sourceID {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("principal %s is scoped to source %q, not %q", p.Key(), p.SourceID, sourceID))
	}
	return nil
}

// BearerToken extracts the token from an `Authorization: Bearer <token>` header. It returns
// ErrMissingToken when the header is absent, empty, or uses another scheme.
func BearerToken(h http.Header) (string, error) {
	value := h.Get("Authorization")
	if value == "" {
		return "", ErrMissingToken
	}
	const prefix = "bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", ErrMissingToken
	}
	token := strings.TrimSpace(value[len(prefix):])
	if token == "" {
		return "", ErrMissingToken
	}
	return token, nil
}

// AuthInterceptor authenticates every ConnectRPC call — unary and streaming, over gRPC,
// gRPC-Web and JSON alike — and puts the resulting Principal in the handler's context.
//
// It authenticates only; it does not authorize. Handlers decide which role they need with
// Require or RequireSource, because that varies per RPC (FR-035: read-only credentials must
// suffice for every query; FR-041: decisions need an individual).
type AuthInterceptor struct {
	auth   Authenticator
	public []string
	logger *slog.Logger
}

// AuthInterceptorOption configures NewAuthInterceptor.
type AuthInterceptorOption func(*AuthInterceptor)

// WithPublicProcedures exempts procedures from authentication — the health check and any
// other unauthenticated endpoint. A value ending in `/` matches every procedure under that
// service, so `WithPublicProcedures("/grpc.health.v1.Health/")` opens the whole health
// service. Handlers reached this way see no principal in their context.
func WithPublicProcedures(procedures ...string) AuthInterceptorOption {
	return func(i *AuthInterceptor) { i.public = append(i.public, procedures...) }
}

// WithAuthLogger sets the logger used for rejection records. Defaults to slog.Default().
func WithAuthLogger(l *slog.Logger) AuthInterceptorOption {
	return func(i *AuthInterceptor) {
		if l != nil {
			i.logger = l
		}
	}
}

// NewAuthInterceptor builds the interceptor. Pass it to ConnectRPC handlers with
// connect.WithInterceptors.
func NewAuthInterceptor(a Authenticator, opts ...AuthInterceptorOption) *AuthInterceptor {
	i := &AuthInterceptor{auth: a, logger: slog.Default()}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

var _ connect.Interceptor = (*AuthInterceptor)(nil)

// IsPublic reports whether procedure is exempt from authentication.
func (i *AuthInterceptor) IsPublic(procedure string) bool {
	for _, p := range i.public {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(procedure, p) {
				return true
			}
			continue
		}
		if procedure == p {
			return true
		}
	}
	return false
}

func (i *AuthInterceptor) authenticate(ctx context.Context, procedure string, header http.Header) (context.Context, error) {
	if i.IsPublic(procedure) {
		return ctx, nil
	}
	token, err := BearerToken(header)
	if err != nil {
		i.reject(procedure, err)
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	principal, err := i.auth.Authenticate(ctx, token)
	if err != nil {
		i.reject(procedure, err)
		var connectErr *connect.Error
		if errors.As(err, &connectErr) {
			return ctx, connectErr
		}
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if principal == nil || principal.Issuer == "" || principal.Subject == "" {
		i.reject(procedure, ErrAnonymous)
		return ctx, connect.NewError(connect.CodeUnauthenticated, ErrAnonymous)
	}
	return ContextWithPrincipal(ctx, principal), nil
}

func (i *AuthInterceptor) reject(procedure string, err error) {
	if i.logger == nil {
		return
	}
	// The token itself is never logged: it is a credential.
	i.logger.Warn("rejected unauthenticated call", "procedure", procedure, "error", err.Error())
}

// WrapUnary implements connect.Interceptor.
func (i *AuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		authed, err := i.authenticate(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(authed, req)
	}
}

// WrapStreamingClient implements connect.Interceptor. Authentication is a server-side
// concern, so the client side is a pass-through.
func (i *AuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor. It authenticates before the first
// message is read, so an unauthenticated ingestion stream (FR-017) is refused immediately.
func (i *AuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		authed, err := i.authenticate(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(authed, conn)
	}
}

// UnaryOnly returns the unary half of the interceptor as a connect.UnaryInterceptorFunc, for
// callers that want to compose only unary behaviour.
func (i *AuthInterceptor) UnaryOnly() connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(i.WrapUnary)
}
