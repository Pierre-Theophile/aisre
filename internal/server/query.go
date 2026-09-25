// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"

	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The query surface (FR-035, FR-041a).
//
// This file holds the service itself and nothing else: every RPC lives in a file of its own
// (query_subgraph.go, query_diff.go, query_impact.go, query_pointers.go, query_history.go,
// query_extent.go, query_resolution.go), each thin enough to read in one screen, each doing the
// same two things before it delegates — check the role and start the latency timer.
//
// The role is `reader` for every one of them, without exception. "Read-only credentials MUST
// suffice for every query" (FR-035) is only a meaningful promise if the read path actually asks
// for nothing more, so no query handler may reach for RequireSource or the decider role.
//
// The failure modes stay distinguishable from the code alone: no token is Unauthenticated, a
// feeder token is PermissionDenied, and a request the engine cannot answer carries the code the
// situation deserves — NotFound for a name the graph has never heard of, InvalidArgument for a
// question that is not well formed. An operator can tell from the status whether to fix their
// credential or their request.
//
// Adding an RPC: a new file, a method on *QueryService, `Require(ctx, RoleReader)` first, and
// the engine call inside Metrics().TimeQuery so query_latency{rpc} keeps reporting.

// QueryService implements sreagent.graph.v1.QueryService.
type QueryService struct {
	projector *projector.Projector
	engine    *query.Engine
	metrics   *telemetry.Metrics
	logger    *slog.Logger
}

var _ graphv1connect.QueryServiceHandler = (*QueryService)(nil)

// NewQueryService returns the query handler. metrics may be nil; logger defaults to
// slog.Default().
func NewQueryService(p *projector.Projector, metrics *telemetry.Metrics, logger *slog.Logger) *QueryService {
	if logger == nil {
		logger = slog.Default()
	}
	return &QueryService{
		projector: p,
		engine:    query.NewEngine(p.Store()),
		metrics:   metrics,
		logger:    logger,
	}
}

// Engine is the read engine the query handlers answer from.
func (s *QueryService) Engine() *query.Engine { return s.engine }

// Projector is the graph the query engine reads through.
func (s *QueryService) Projector() *projector.Projector { return s.projector }

// Metrics is the instrument set query handlers record their latency on.
func (s *QueryService) Metrics() *telemetry.Metrics { return s.metrics }

// Logger is the structured logger query handlers should use.
func (s *QueryService) Logger() *slog.Logger { return s.logger }
