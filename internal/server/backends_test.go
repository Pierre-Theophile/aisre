// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// backendsFor is pgserver.Backends, which this package's own tests cannot import without a cycle.
func backendsFor(p *projector.Projector) Backends {
	engine := query.NewEngine(p.Store())
	return Backends{Log: p, Graph: engine, Catalog: engine, Principals: p.Store(), Database: p.Store()}
}
