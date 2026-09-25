// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// QueryService.Diff — what changed between two instants, ranked (FR-027, FR-028, FR-035, T040).
//
// Like Subgraph, the handler is thin: the role the call needs and the latency it is measured
// by belong here, everything that decides the answer lives in internal/query.
//
// Authorization is `reader`, and that is worth stating plainly for this method in particular.
// A diff is the query an incident is run from, so it is the one a deployment is most tempted to
// hand a powerful token; FR-035's promise that read-only credentials suffice is only kept if
// the most useful read asks for nothing more than the least useful one.

// Diff returns what changed in a subgraph between two instants, with ranked change candidates
// (FR-027, FR-028).
func (s *QueryService) Diff(ctx context.Context, req *connect.Request[graphv1.DiffRequest]) (*connect.Response[graphv1.DiffResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.DiffResponse
	err := s.Metrics().TimeQuery(ctx, "Diff", func() error {
		var err error
		resp, err = s.engine.Diff(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
