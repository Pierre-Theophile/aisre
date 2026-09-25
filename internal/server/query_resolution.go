// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The two read-only halves of entity resolution, over RPC (FR-031, FR-033, FR-035).
//
// Both ask for `reader` and nothing more. Seeing why the graph thinks two names are one thing,
// and seeing what it has not dared decide, are reads: an analyst with a read-only credential
// must be able to audit every merge the graph made (constitution VI), and requiring the decider
// role to *look* would push deployments into handing out write-capable tokens for review work.
// Deciding is ResolutionService, and that is where the decider role is demanded.

// ResolutionAudit answers "why are these two identifiers the same entity?" (FR-031).
func (s *QueryService) ResolutionAudit(ctx context.Context, req *connect.Request[graphv1.ResolutionAuditRequest]) (*connect.Response[graphv1.ResolutionAuditResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.ResolutionAuditResponse
	err := s.Metrics().TimeQuery(ctx, "ResolutionAudit", func() error {
		var err error
		resp, err = s.engine.ResolutionAudit(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// Suggestions lists entity-resolution pairs awaiting a human decision (FR-033).
func (s *QueryService) Suggestions(ctx context.Context, req *connect.Request[graphv1.SuggestionsRequest]) (*connect.Response[graphv1.SuggestionsResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.SuggestionsResponse
	err := s.Metrics().TimeQuery(ctx, "Suggestions", func() error {
		var err error
		resp, err = s.engine.Suggestions(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ProposedDependencies lists proposed edges awaiting a human decision (003 FR-029).
//
// `reader`, like the two above and for the same reason: seeing what the graph has not dared decide
// is a read, and demanding the decider role merely to look would push deployments into handing out
// write-capable tokens for review work. Deciding is ResolutionService.
func (s *QueryService) ProposedDependencies(ctx context.Context, req *connect.Request[graphv1.ProposedDependenciesRequest]) (*connect.Response[graphv1.ProposedDependenciesResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	var resp *graphv1.ProposedDependenciesResponse
	err := s.Metrics().TimeQuery(ctx, "ProposedDependencies", func() error {
		var err error
		resp, err = s.engine.ProposedDependencies(ctx, req.Msg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
