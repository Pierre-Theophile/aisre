// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
)

// IdentityService answers "who am I?" for the authenticated caller (feature 010, research R3).
//
// The MCP server needs the caller's roles before it lists a single tool, so that a reader is not
// offered a tool it would be refused (010 FR-013) and a feeder token is turned away at once (010
// FR-014). The answer repeats what the interceptor already established from the caller's own token:
// it reveals nothing the caller does not hold, and it decides nothing, because every other RPC
// still checks its own role.
type IdentityService struct{}

var _ graphv1connect.IdentityServiceHandler = (*IdentityService)(nil)

// NewIdentityService builds the service. It has no dependency: the principal is in the context.
func NewIdentityService() *IdentityService { return &IdentityService{} }

// WhoAmI returns the authenticated principal's subject, issuer and granted roles. An anonymous
// call never reaches it (the interceptor refuses it), and the check is repeated here so that an
// in-process caller without a principal is refused too.
func (s *IdentityService) WhoAmI(ctx context.Context, _ *connect.Request[graphv1.WhoAmIRequest]) (*connect.Response[graphv1.WhoAmIResponse], error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrMissingToken)
	}
	roles := make([]string, 0, len(p.Roles))
	for _, role := range p.Roles {
		roles = append(roles, string(role))
	}
	return connect.NewResponse(&graphv1.WhoAmIResponse{
		Subject: p.Subject,
		Issuer:  p.Issuer,
		Roles:   roles,
	}), nil
}
