// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// QueryService.Subgraph — the foundational read, over RPC (FR-026, FR-035, T033).
//
// The handler is thin on purpose. Everything that decides what the answer is lives in
// internal/query; what belongs here is the part that is about being a server: the role the
// call needs, and the latency it is measured by.
//
// Authorization comes first and is `reader`. FR-035 promises that read-only credentials
// suffice for every query, and the only way to keep that promise honest is for the read path
// to ask for nothing more — no feeder scope, no decider role, nothing that would tempt a
// deployment into handing an analyst a token that can also write.
//
// The engine's errors are already Connect errors carrying the code the situation deserves: an
// unknown focus reference is NotFound (the name is not in the graph), a missing as-of instant
// is InvalidArgument, and a database failure is passed through as an internal error. Rewriting
// them here would flatten that distinction.

// Subgraph returns the N-hop neighbourhood of a focus node as of an instant (FR-026).
func (s *QueryService) Subgraph(ctx context.Context, req *connect.Request[graphv1.SubgraphRequest]) (*connect.Response[graphv1.SubgraphResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.SubgraphResponse
	err := s.Metrics().TimeQuery(ctx, "Subgraph", func() error {
		var err error
		resp, err = s.engine.Subgraph(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
