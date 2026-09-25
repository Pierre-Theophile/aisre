// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// QueryService.Pointers — where to look for telemetry, over RPC (FR-030, FR-035, T068).
//
// The handler carries no policy of its own. What is worth saying here is what it deliberately
// does *not* do: it never touches the backends the pointers name. A pointer is a sentence that
// finds telemetry, and returning it is the whole of this RPC's job — fetching what it points at
// would put metric and log payloads on the graph's read path, which constitution IV forbids.

// Pointers returns a node's telemetry pointers as of an instant, grouped by kind (FR-030).
func (s *QueryService) Pointers(ctx context.Context, req *connect.Request[graphv1.PointersRequest]) (*connect.Response[graphv1.PointersResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.PointersResponse
	err := s.Metrics().TimeQuery(ctx, "Pointers", func() error {
		var err error
		resp, err = s.engine.Pointers(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
