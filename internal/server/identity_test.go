// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"slices"
	"testing"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// TestWhoAmIRepeatsThePrincipal: the answer is the principal the interceptor established, roles as
// granted (implications are not expanded: a client applies them, as Principal.Has does).
func TestWhoAmIRepeatsThePrincipal(t *testing.T) {
	ctx := ContextWithPrincipal(context.Background(), &Principal{
		Issuer: "https://idp.example", Subject: "alice", Roles: []Role{RoleInvestigator},
	})
	resp, err := NewIdentityService().WhoAmI(ctx, connect.NewRequest(&graphv1.WhoAmIRequest{}))
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if resp.Msg.GetSubject() != "alice" || resp.Msg.GetIssuer() != "https://idp.example" {
		t.Errorf("identity = %s|%s, want https://idp.example|alice", resp.Msg.GetIssuer(), resp.Msg.GetSubject())
	}
	if !slices.Equal(resp.Msg.GetRoles(), []string{"investigator"}) {
		t.Errorf("roles = %v, want [investigator]", resp.Msg.GetRoles())
	}
}

// TestWhoAmIRefusesAnAnonymousCall: no principal, no answer.
func TestWhoAmIRefusesAnAnonymousCall(t *testing.T) {
	_, err := NewIdentityService().WhoAmI(context.Background(), connect.NewRequest(&graphv1.WhoAmIRequest{}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("err = %v, want unauthenticated", err)
	}
}
