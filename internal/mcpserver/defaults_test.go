// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/record"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// TestDefaultsMatchTheCLI pins the defaults this package repeats to their sources, so a tool and
// the CLI command of the same name cannot drift apart (research R4). The test may link
// internal/query; the package may not.
func TestDefaultsMatchTheCLI(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"hops", defaultHops, query.DefaultHops},
		{"per-hop cap", defaultPerHopCap, query.DefaultPerHopCap},
		{"total cap", defaultTotalCap, query.DefaultTotalCap},
		{"impact hops", defaultImpactHops, query.DefaultImpactHops},
		{"max hops", maxHops, query.MaxHops},
		{"list limit", defaultListLimit, record.DefaultListLimit},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d here, %d in the CLI's source", c.name, c.got, c.want)
		}
	}
	if idNamespace != query.IDNamespace {
		t.Errorf("id namespace %q, want %q", idNamespace, query.IDNamespace)
	}
}
