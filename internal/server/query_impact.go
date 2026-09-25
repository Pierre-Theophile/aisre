// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// QueryService.Impact — the weighted blast radius, over RPC (FR-029, FR-035, T065).
//
// Thin, like every query handler: the role the call needs and the latency it is measured by
// live here, and everything that decides what the answer is lives in internal/query. The role
// is `reader` and nothing more, because FR-035's promise that read-only credentials suffice for
// every query is only honest if the read path asks for nothing else.
//
// The engine's errors already carry the code the situation deserves — an unknown focus is
// NotFound, a missing as-of instant or a radius past the maximum is InvalidArgument — so they
// are passed through rather than reclassified.

// Impact returns the weighted blast radius of a node as of an instant (FR-029).
func (s *QueryService) Impact(ctx context.Context, req *connect.Request[graphv1.ImpactRequest]) (*connect.Response[graphv1.ImpactResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.ImpactResponse
	err := s.Metrics().TimeQuery(ctx, "Impact", func() error {
		var err error
		resp, err = s.engine.Impact(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
