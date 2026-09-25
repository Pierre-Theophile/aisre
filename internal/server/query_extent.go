// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// QueryService.Extent — how much of reality this graph claims to have seen (FR-052).
//
// It is the smallest query in the contract and the one every other answer should be read
// against. "checkout called payments at 14:32" means one thing when the log covers 13:00 to
// 15:00 with no gaps, and quite another when the Kubernetes feeder admits it was disconnected
// between 14:20 and 14:45. Consumers cannot make that judgement unless the graph publishes its
// own coverage, so the subgraph response carries an Extent too and this RPC exposes it alone.
//
// The data comes straight from the log (log.Extent): the observed-time span of every event
// ever appended, plus each source's last checkpoint and the gaps it declared. Nothing is
// derived from the projection, because a gap is a statement about what was *delivered*, not
// about what happened to land in the graph.

// Extent reports the observed-time span of the log and each source's coverage (FR-052).
func (s *QueryService) Extent(ctx context.Context, _ *connect.Request[graphv1.ExtentRequest]) (*connect.Response[graphv1.Extent], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var extent *graphv1.Extent
	err := s.Metrics().TimeQuery(ctx, "Extent", func() error {
		var err error
		extent, err = s.projector.Log().Extent(ctx)
		return err
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(extent), nil
}
