// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The CI-sized benchmark smoke test (T091).
//
// The reference workload is a million events and tens of minutes; that belongs in bench.yml,
// weekly. What belongs in `go test` is the proof that the harness itself still works — that the
// generator produces the shape it claims, that every query family answers, that the report
// evaluates its criteria — because a benchmark that has silently stopped running is worse than
// no benchmark, and the week it is needed is the week it will be discovered broken.
//
// So this runs the same code at `1k` with a handful of queries and no replay, and it is guarded
// by SRE_AGENT_BENCH=1 because even that generates 100,000 events and takes minutes, which is
// too long for the default `go test ./...`.
//
//	SRE_AGENT_BENCH=1 PG_DSN=... go test ./internal/fixture -run TestBenchSmoke -v

func TestBenchSmoke(t *testing.T) {
	if os.Getenv("SRE_AGENT_BENCH") != "1" {
		t.Skip("set SRE_AGENT_BENCH=1 to run the benchmark smoke test (it generates 100k events)")
	}

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("PG_DSN is required: the benchmark creates and drops databases of its own")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	report, err := fixture.RunBench(ctx, fixture.NewStoreFactoryFromDSN(dsn), fixture.BenchOptions{
		Scale:      fixture.BenchScale1k,
		Seed:       1,
		Queries:    5,
		SkipReplay: true,
		NewEngine:  func(s *postgres.Store) fixture.BenchEngine { return query.NewEngine(s) },
		Progress:   func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("RunBench: %v", err)
	}

	// The workload is the shape research §2 asks for, at a tenth of the size.
	if report.Ingest.Events != fixture.BenchScale1k.Events {
		t.Errorf("ingested %d events, want %d", report.Ingest.Events, fixture.BenchScale1k.Events)
	}
	if report.Graph.EdgeVersions < fixture.BenchScale1k.Edges {
		t.Errorf("graph holds %d edge versions, want at least the %d generated",
			report.Graph.EdgeVersions, fixture.BenchScale1k.Edges)
	}
	// About 30% of services are merged by certain rule C2, which is what puts the read path
	// through the merge redirects (research §2).
	if report.Graph.Merges == 0 {
		t.Error("no entity was merged; the workload is supposed to exercise C2 resolution")
	}

	// Every family answered, and nothing measured zero — a family that silently returned an
	// error-free empty answer in no time would look like a spectacular result.
	want := []string{
		fixture.BenchQuerySubgraph2, fixture.BenchQuerySubgraph3,
		fixture.BenchQueryDiff1h, fixture.BenchQueryDiff24h, fixture.BenchQueryDiff7d,
		fixture.BenchQueryImpact,
	}
	measured := map[string]fixture.BenchLatency{}
	for _, l := range report.Latencies {
		measured[l.Name] = l
	}
	for _, name := range want {
		l, ok := measured[name]
		if !ok {
			t.Errorf("query family %s was not measured", name)
			continue
		}
		if l.Count == 0 || l.P95Ms <= 0 {
			t.Errorf("query family %s measured n=%d p95=%.3fms", name, l.Count, l.P95Ms)
		}
	}

	// The report states a verdict against the published criteria, which is the part CI acts on.
	if len(report.Criteria) == 0 {
		t.Fatal("the report evaluated no criteria")
	}
	for _, c := range report.Criteria {
		if c.Gate && !c.Passed {
			t.Errorf("%s %s: target %s, measured %s", c.ID, c.What, c.Target, c.Actual)
		}
	}
	if report.DatabaseBytes <= 0 {
		t.Error("the report recorded no database size")
	}
	if report.Environment.PostgresVersion == "" {
		t.Error("the report recorded no PostgreSQL version")
	}
	if md := report.Markdown(); len(md) == 0 {
		t.Error("the report rendered no Markdown")
	}
	if _, err := report.JSON(); err != nil {
		t.Errorf("report JSON: %v", err)
	}
}

// TestBenchScalesAreWellFormed is cheap and unguarded: it checks the published shapes without
// touching a database, so a typo in a scale is caught on every run rather than weekly.
func TestBenchScalesAreWellFormed(t *testing.T) {
	for _, scale := range fixture.BenchScales {
		structural := scale.Entities + scale.Edges + scale.Changes + scale.Claims
		if scale.Events < structural {
			t.Errorf("scale %s: %d events cannot carry %d structural events",
				scale.Name, scale.Events, structural)
		}
		if scale.Span <= 0 {
			t.Errorf("scale %s: span is %s", scale.Name, scale.Span)
		}
		resolved, err := fixture.BenchScaleByName(scale.Name)
		if err != nil {
			t.Errorf("BenchScaleByName(%q): %v", scale.Name, err)
			continue
		}
		if resolved != scale {
			t.Errorf("BenchScaleByName(%q) returned a different scale", scale.Name)
		}
	}
	if _, err := fixture.BenchScaleByName("100k"); err == nil {
		t.Error("BenchScaleByName accepted an unpublished scale")
	}
}
