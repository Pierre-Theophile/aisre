// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	graphworker "github.com/Pierre-Theophile/aisre/internal/investigation/workers/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The in-process graph read surface.
//
// Feature 001's query engine answers six of the seven reads the graph worker declares; the
// seventh — `Extent`, how much of reality this graph claims to have seen — is the log's own and
// the engine does not re-export it. This is the one line that joins them, and it exists here
// rather than in `internal/query` because the pairing is the *investigation's* requirement: the
// engine's worker interface names seven methods, and a surface that silently lacked one would
// fail at the extent consultation, in the middle of an incident, rather than at the type checker.

// graphSurface is the query engine plus the log's extent.
type graphSurface struct {
	*query.Engine
	log *eventlog.Log
}

var _ graphworker.QueryService = (*graphSurface)(nil)

// GraphOver returns the in-process graph read surface over a store: what `serve` hands the runner
// and what a test hands it when it has a real database rather than a stub.
func GraphOver(store *postgres.Store) graphworker.QueryService {
	return &graphSurface{Engine: query.NewEngine(store), log: eventlog.New(store)}
}

// Extent reports the observed-time span of the log and each source's coverage (FR-032, FR-052).
//
// The request carries no parameters today; it is taken rather than ignored so that a later
// narrowing of the question is not a signature change across three packages.
func (g *graphSurface) Extent(ctx context.Context, _ *graphv1.ExtentRequest) (*graphv1.Extent, error) {
	return g.log.Extent(ctx)
}
