// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// BenchEngine is the read side the benchmark measures.
//
// It is an interface rather than *query.Engine because internal/query already imports this
// package for the golden-query runner, so the dependency has to point this way. The CLI hands
// the real engine in, exactly as `fixture verify` hands in its QueryRunner, and the two can
// never drift because both are the same engine.
type BenchEngine interface {
	Subgraph(ctx context.Context, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error)
	Diff(ctx context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error)
	Impact(ctx context.Context, req *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error)
}

// BenchEngineFactory builds the read engine over one store.
type BenchEngineFactory func(*postgres.Store) BenchEngine

// The measured query families (research §2).
//
// Six families, 1,000 randomized queries each, every one of them a question an SRE actually
// asks: what did this neighbourhood look like at 14:32, what does it look like three hops out
// with the published caps, what changed in the last hour / day / week, and what is downstream
// of this service.
//
// Two choices keep the numbers honest.
//
// The focus of every query is drawn from the *focus set* — every service plus the 200
// highest-degree entities — rather than uniformly from the population. A uniform draw would
// spend most of its samples on leaves with two neighbours, and the p95 would say nothing about
// the hub queries that decide whether the budget is met.
//
// The as-of instant is drawn from the whole 90-day span, not from the end of it. A query about
// last Tuesday has to look through every version written since, which is the bitemporal cost
// this storage decision is being judged on.

// benchQuery is one family: a name, what it does, and the closure that runs one sample.
type benchQuery struct {
	name   string
	detail string
	run    func(ctx context.Context, e BenchEngine, rng *rand.Rand) error
}

// runBenchQueries measures every family and returns their percentiles.
func runBenchQueries(ctx context.Context, store *postgres.Store, g *benchGenerator, opts BenchOptions) ([]BenchLatency, error) {
	engine := opts.NewEngine(store)
	families := benchFamilies(g)
	summaries := make([]BenchLatency, 0, len(families))

	for _, family := range families {
		opts.progress("running %d %s queries", opts.queries(), family.name)
		//nolint:gosec // reproducibility, not unpredictability: each family gets its own stream.
		rng := rand.New(rand.NewPCG(uint64(opts.Seed), hashName(family.name)))
		samples := make([]time.Duration, 0, opts.queries())
		for i := range opts.queries() {
			start := time.Now()
			if err := family.run(ctx, engine, rng); err != nil {
				return nil, fmt.Errorf("fixture: bench query %s #%d: %w", family.name, i, err)
			}
			samples = append(samples, time.Since(start))
		}
		summary := summarize(family.name, family.detail, samples)
		opts.progress("  %s p50=%.0fms p95=%.0fms p99=%.0fms",
			family.name, summary.P50Ms, summary.P95Ms, summary.P99Ms)
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// benchFamilies builds the six families against one generated topology.
func benchFamilies(g *benchGenerator) []benchQuery {
	focus := func(rng *rand.Rand) *graphv1.Ref {
		return g.entities[g.focuses[rng.IntN(len(g.focuses))]].Ref.Proto()
	}
	// validAt draws an instant from the part of the span that has topology in it: the first 1%
	// is the estate being created, and an as-of instant inside it would measure a graph that
	// does not exist yet.
	validAt := func(rng *rand.Rand) time.Time {
		structural := time.Duration(float64(g.scale.Span) * 0.01)
		window := g.scale.Span - structural
		return BenchEpoch.Add(structural + time.Duration(rng.Int64N(int64(window))))
	}
	subgraphReq := func(rng *rand.Rand, hops uint32, capped bool) *graphv1.SubgraphRequest {
		req := &graphv1.SubgraphRequest{
			Focus:     focus(rng),
			AsOf:      &graphv1.AsOf{ValidAt: timestamppb.New(validAt(rng))},
			Hops:      hops,
			Direction: graphv1.Direction_BOTH,
		}
		if capped {
			perHop, total := uint32(50), uint32(500)
			req.PerHopCap, req.TotalCap = &perHop, &total
		}
		return req
	}
	diff := func(name, detail string, window time.Duration) benchQuery {
		return benchQuery{name: name, detail: detail,
			run: func(ctx context.Context, e BenchEngine, rng *rand.Rand) error {
				t2 := validAt(rng)
				t1 := t2.Add(-window)
				if t1.Before(BenchEpoch) {
					t1 = BenchEpoch
				}
				_, err := e.Diff(ctx, &graphv1.DiffRequest{
					Subgraph: subgraphReq(rng, 2, false),
					T1:       timestamppb.New(t1),
					T2:       timestamppb.New(t2),
				})
				return err
			}}
	}

	return []benchQuery{
		{
			name:   BenchQuerySubgraph2,
			detail: "2 hops, both directions, random focus and random valid instant",
			run: func(ctx context.Context, e BenchEngine, rng *rand.Rand) error {
				_, err := e.Subgraph(ctx, subgraphReq(rng, 2, false))
				return err
			},
		},
		{
			name:   BenchQuerySubgraph3,
			detail: "3 hops with the published caps (50 per hop, 500 total)",
			run: func(ctx context.Context, e BenchEngine, rng *rand.Rand) error {
				_, err := e.Subgraph(ctx, subgraphReq(rng, 3, true))
				return err
			},
		},
		diff(BenchQueryDiff1h, "1 h window over a 2-hop neighbourhood, with ranked changes", time.Hour),
		diff(BenchQueryDiff24h, "24 h window over a 2-hop neighbourhood, with ranked changes", 24*time.Hour),
		diff(BenchQueryDiff7d, "7 d window over a 2-hop neighbourhood, with ranked changes", 7*24*time.Hour),
		{
			name:   BenchQueryImpact,
			detail: "blast radius, weight-ranked, default caps",
			run: func(ctx context.Context, e BenchEngine, rng *rand.Rand) error {
				_, err := e.Impact(ctx, &graphv1.ImpactRequest{
					Focus: focus(rng),
					AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(validAt(rng))},
				})
				return err
			},
		},
	}
}

// hashName gives each family its own deterministic random stream, so adding a family does not
// shift the queries the others run and change their numbers.
func hashName(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
