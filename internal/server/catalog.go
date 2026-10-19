// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// CatalogService reads the service catalog and its attributions back (feature 009, ADR-0015 item
// 6). Every method is a read and needs the `reader` role only: an on-call asking "why is this alert
// attributed to that service?" must not need a credential that can change the answer. Writes go
// through ResolutionService, where the principal is stamped.
type CatalogService struct {
	engine  *query.Engine
	metrics *telemetry.Metrics
	logger  *slog.Logger
}

var _ graphv1connect.CatalogServiceHandler = (*CatalogService)(nil)

// NewCatalogService builds the service over the projector's store.
func NewCatalogService(p *projector.Projector, metrics *telemetry.Metrics, logger *slog.Logger) *CatalogService {
	if logger == nil {
		logger = slog.Default()
	}
	return &CatalogService{engine: query.NewEngine(p.Store()), metrics: metrics, logger: logger}
}

// catalogRead is the body every method shares: authorize, time, answer.
func catalogRead[Req, Resp any](ctx context.Context, s *CatalogService, name string, req *Req,
	read func(context.Context, *Req) (*Resp, error),
) (*connect.Response[Resp], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *Resp
	if err := s.metrics.TimeQuery(ctx, name, func() error {
		var err error
		resp, err = read(ctx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ListEntries lists the catalog as of an instant (FR-005).
func (s *CatalogService) ListEntries(ctx context.Context, req *connect.Request[graphv1.ListEntriesRequest]) (*connect.Response[graphv1.ListEntriesResponse], error) {
	return catalogRead(ctx, s, "ListEntries", req.Msg, s.engine.ListEntries)
}

// GetEntry reads one entry, its aliases and its history.
func (s *CatalogService) GetEntry(ctx context.Context, req *connect.Request[graphv1.GetEntryRequest]) (*connect.Response[graphv1.GetEntryResponse], error) {
	return catalogRead(ctx, s, "GetEntry", req.Msg, s.engine.GetEntry)
}

// WhyAttributed explains a signal's attribution, or why it has none (FR-039).
func (s *CatalogService) WhyAttributed(ctx context.Context, req *connect.Request[graphv1.WhyAttributedRequest]) (*connect.Response[graphv1.WhyAttributedResponse], error) {
	return catalogRead(ctx, s, "WhyAttributed", req.Msg, s.engine.WhyAttributed)
}

// AttributedSignals lists what an entry has been attributed (FR-039).
func (s *CatalogService) AttributedSignals(ctx context.Context, req *connect.Request[graphv1.AttributedSignalsRequest]) (*connect.Response[graphv1.AttributedSignalsResponse], error) {
	return catalogRead(ctx, s, "AttributedSignals", req.Msg, s.engine.AttributedSignals)
}

// ListProposals lists what discovery proposed and waits on a person (FR-010, FR-011).
func (s *CatalogService) ListProposals(ctx context.Context, req *connect.Request[graphv1.ListProposalsRequest]) (*connect.Response[graphv1.ListProposalsResponse], error) {
	return catalogRead(ctx, s, "ListProposals", req.Msg, s.engine.ListProposals)
}
