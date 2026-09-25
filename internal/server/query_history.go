// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// QueryService.NodeHistory — every version of a node, over RPC (FR-032, FR-035, T070).
//
// The one query in the contract that takes no as-of instant, because pinning observed time
// would hide the corrections it exists to show. It still needs only the `reader` role: the
// history of a node is a read of the graph like any other, and an auditor who may not write
// must be able to run it (constitution V, FR-035).

// NodeHistory returns every version of a node and the resolution decisions about it (FR-032).
func (s *QueryService) NodeHistory(ctx context.Context, req *connect.Request[graphv1.NodeHistoryRequest]) (*connect.Response[graphv1.NodeHistoryResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.NodeHistoryResponse
	err := s.Metrics().TimeQuery(ctx, "NodeHistory", func() error {
		var err error
		resp, err = s.engine.NodeHistory(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
