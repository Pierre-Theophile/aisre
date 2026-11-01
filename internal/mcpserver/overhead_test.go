// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/mcpserver"
	"github.com/Pierre-Theophile/aisre/internal/server"
)

// TestOverheadBudget is SC-004: the adapter adds at most 50 ms at p95 to the API call it wraps.
// The same diff is asked directly through the API client and through the MCP tool; the budget
// is generous on purpose (the measured overhead is a few milliseconds), so the test fails on a
// regression, not on a slow CI runner.
// budget is SC-004's bound on the time the adapter adds at p95.
const budget = 50 * time.Millisecond

func TestOverheadBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	baseURL, issuer := apiServer(t, filepath.Join("..", "..", "fixtures", "rollout-regression-01"))
	token := mint(t, issuer, server.RoleReader)
	cs := session(t, baseURL, token, mcpserver.ProfileDefault)
	api := mcpserver.Connect(baseURL, nil, func() (string, error) { return token, nil })
	args := map[string]any{"focus": "otel.service.name=checkout", "from": "2026-09-01T13:00:00Z",
		"to": "2026-09-01T14:32:00Z", "hops": 2}
	req := &graphv1.DiffRequest{
		Subgraph: &graphv1.SubgraphRequest{Focus: &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
			Hops: 2, Direction: graphv1.Direction_BOTH},
		T1: ts(t, "2026-09-01T13:00:00Z"), T2: ts(t, "2026-09-01T14:32:00Z"),
	}

	// Up to three rounds, and the best round is the one held to the budget. The test shares its
	// runner with every other package's tests, and one round can land on a moment the CPU is
	// taken; a real regression in the adapter is over budget in every round.
	const n, rounds = 40, 3
	p95 := func(d []time.Duration) time.Duration {
		slices.Sort(d)
		return d[(len(d)*95)/100-1]
	}
	best := time.Duration(1<<63 - 1)
	for round := 1; round <= rounds && best > budget; round++ {
		var direct, viaMCP []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			if _, err := api.Clients.Query.Diff(context.Background(), connect.NewRequest(req)); err != nil {
				t.Fatal(err)
			}
			direct = append(direct, time.Since(start))
			start = time.Now()
			call(t, cs, "diff", args)
			viaMCP = append(viaMCP, time.Since(start))
		}
		overhead := p95(viaMCP) - p95(direct)
		t.Logf("round %d, diff on rollout-regression-01, %d calls: p95 direct %s, via MCP %s, added %s",
			round, n, p95(direct), p95(viaMCP), overhead)
		best = min(best, overhead)
	}
	if best > budget {
		t.Errorf("the MCP adapter adds %s at p95 in its best of %d rounds, budget %s (SC-004)", best, rounds, budget)
	}
}
