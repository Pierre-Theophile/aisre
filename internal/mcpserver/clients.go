// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
)

// TokenSource returns the caller's bearer token for one API call. Over stdio it re-reads the
// token file (010 FR-011); over HTTP it returns the token of the request being served (FR-012).
type TokenSource func() (string, error)

// bearer attaches the token to every request, streams included (the same choice as the CLI's
// bearerTransport: an interceptor would not reach a stream's single HTTP request).
type bearer struct {
	base   http.RoundTripper
	source TokenSource
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := b.source()
	if err != nil {
		return nil, err
	}
	if token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return b.base.RoundTrip(req)
}

// API is a connection to the aisre API on behalf of one caller.
type API struct {
	Clients Clients
	ident   graphv1connect.IdentityServiceClient
}

// Connect builds the clients for baseURL with the caller's token. base is the round tripper to
// use (nil is http.DefaultTransport). The HTTP client has no overall timeout because an
// investigation is a stream; every call carries its own context.
func Connect(baseURL string, base http.RoundTripper, token TokenSource) *API {
	if base == nil {
		base = http.DefaultTransport
	}
	if token == nil {
		token = func() (string, error) { return "", nil }
	}
	client := &http.Client{Transport: &bearer{base: base, source: token}}
	return &API{
		Clients: Clients{
			Query:         graphv1connect.NewQueryServiceClient(client, baseURL),
			Catalog:       graphv1connect.NewCatalogServiceClient(client, baseURL),
			Investigation: investigationv1connect.NewInvestigationServiceClient(client, baseURL),
		},
		ident: graphv1connect.NewIdentityServiceClient(client, baseURL),
	}
}

// Caller is who the API says the caller is.
type Caller struct {
	Subject string
	Issuer  string
	Roles   Roles
}

// ErrUnauthenticated is returned by WhoAmI when the API did not accept the token.
var ErrUnauthenticated = errors.New("mcp: the aisre API did not accept the token")

// WhoAmI asks the API who the caller is (research R3).
func (a *API) WhoAmI(ctx context.Context) (Caller, error) {
	resp, err := a.ident.WhoAmI(ctx, connect.NewRequest(&graphv1.WhoAmIRequest{}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			return Caller{}, ErrUnauthenticated
		}
		return Caller{}, err
	}
	return Caller{
		Subject: resp.Msg.GetSubject(),
		Issuer:  resp.Msg.GetIssuer(),
		Roles:   Roles(resp.Msg.GetRoles()),
	}, nil
}
