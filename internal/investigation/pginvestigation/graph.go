// SPDX-License-Identifier: Apache-2.0

package pginvestigation

import (
	"context"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	graphworker "github.com/Pierre-Theophile/aisre/internal/investigation/workers/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// GraphService adapts feature 001's in-process query engine and the log's extent to the graph
// worker's graphworker.QueryService. It is what `serve` hands the runner, and what a fixture replay uses: the
// graph is replayed from events.jsonl into a database of its own and read directly, with no
// server and no network.
type GraphService struct {
	engine *query.Engine
	log    *eventlog.Log
}

var _ graphworker.QueryService = (*GraphService)(nil)

// NewGraphService returns the in-process adapter.
func NewGraphService(e *query.Engine) *GraphService {
	return &GraphService{engine: e, log: eventlog.New(e.Store())}
}

// WhyAttributed is the catalog read intake asks for the triggering signal's entries (009 FR-021).
func (s *GraphService) WhyAttributed(ctx context.Context, req *graphv1.WhyAttributedRequest) (*graphv1.WhyAttributedResponse, error) {
	return s.engine.WhyAttributed(ctx, req)
}

// Subgraph implements graphworker.QueryService.
func (s *GraphService) Subgraph(ctx context.Context, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error) {
	return s.engine.Subgraph(ctx, req)
}

// Diff implements graphworker.QueryService.
func (s *GraphService) Diff(ctx context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	return s.engine.Diff(ctx, req)
}

// Impact implements graphworker.QueryService.
func (s *GraphService) Impact(ctx context.Context, req *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error) {
	return s.engine.Impact(ctx, req)
}

// Pointers implements graphworker.QueryService.
func (s *GraphService) Pointers(ctx context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error) {
	return s.engine.Pointers(ctx, req)
}

// NodeHistory implements graphworker.QueryService.
func (s *GraphService) NodeHistory(ctx context.Context, req *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error) {
	return s.engine.NodeHistory(ctx, req)
}

// ResolutionAudit implements graphworker.QueryService.
func (s *GraphService) ResolutionAudit(ctx context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error) {
	return s.engine.ResolutionAudit(ctx, req)
}

// Extent implements graphworker.QueryService.
func (s *GraphService) Extent(ctx context.Context, _ *graphv1.ExtentRequest) (*graphv1.Extent, error) {
	return s.log.Extent(ctx)
}

// GraphOver returns the in-process graph read surface over a store.
func GraphOver(store *postgres.Store) *GraphService { return NewGraphService(query.NewEngine(store)) }
