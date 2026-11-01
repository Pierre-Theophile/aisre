// SPDX-License-Identifier: Apache-2.0

// Package pgserver wires the server's ports to their Postgres implementations: the projector is
// the event log, the query engine reads the graph and the catalog, and the store records principals
// and answers the probes. Only the composition root (internal/cli) and tests import it.
package pgserver

import (
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/server"
)

// Backends builds the server's backends over the projector and its store.
func Backends(p *projector.Projector) server.Backends {
	engine := query.NewEngine(p.Store())
	return server.Backends{
		Log:        p,
		Graph:      engine,
		Catalog:    engine,
		Principals: p.Store(),
		Database:   p.Store(),
	}
}
