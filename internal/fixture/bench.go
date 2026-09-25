// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The reference-workload benchmark (T091, research §2, SC-001, SC-002, ADR-0002).
//
// Constitution X forbids introducing a graph database unless a reproducible benchmark on the
// reference workload shows that PostgreSQL cannot meet the query budget. This file is that
// benchmark: it is the evidence behind the storage decision, so everything it does is fixed by
// research §2 rather than chosen for convenience.
//
// # What it generates
//
// A synthetic organisation of ~10k entities, 100k relationships and 1M events over 90 days,
// with 50k change nodes and 200k identity claims from two sources. Two properties of the shape
// matter more than the totals:
//
//   - The degree distribution is a power law, built by preferential attachment. A uniform graph
//     would make every 2-hop neighbourhood the same size and the p95 meaningless; real systems
//     have hubs — the shared database, the auth service — and it is the hub queries that decide
//     whether the budget is met.
//   - About 30% of services are merged by certain rule C2 from claims by two sources, so the
//     query path is exercised against a graph that has been through entity resolution, with the
//     merge redirects every read has to follow.
//
// Everything is derived from a seed: the same seed produces the same graph, the same queries
// and therefore comparable numbers across runs and machines.
//
// # What it measures
//
// Per research §2: ingestion throughput, full replay wall time, and p50/p95/p99 over 1,000
// randomized queries each of subgraph 2-hop as-of, subgraph 3-hop with caps, diff over 1 h,
// 24 h and 7 d, and impact. The report states pass or fail against SC-001, SC-002 and the
// ADR-0002 exit criteria, because a benchmark whose conclusion has to be worked out by hand is
// a benchmark nobody will act on.

// Benchmark source ids. Two, because C2 needs two sources to corroborate one service name and
// because feeder lag and provenance are only interesting with more than one feeder.
const (
	// BenchSourceK8s is the synthetic cluster feeder: workloads, infrastructure, config, and
	// the declared service names that make C2 fire.
	BenchSourceK8s = "bench:k8s"
	// BenchSourceOTel is the synthetic telemetry feeder: services, call edges with weight
	// classes, third-party dependencies and observed service names.
	BenchSourceOTel = "bench:otel"
)

// BenchEpoch is the first instant of the generated workload. It is fixed rather than relative
// to `now` so that two runs of the same seed produce byte-identical events, which is what makes
// a regression in the numbers attributable to the code rather than to the calendar.
var BenchEpoch = time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)

// BenchScale is one workload size. Only the shape is a parameter; the proportions between
// entities, edges, changes and claims are research §2's and are not tunable.
type BenchScale struct {
	// Name is what `--scale` accepts: "10k" or "1k".
	Name string
	// Entities, Edges, Changes and Claims are the counts to generate.
	Entities int
	Edges    int
	Changes  int
	Claims   int
	// Events is the total event count, churn included. It must be at least
	// Entities+Edges+Changes+Claims; the remainder is property and weight-class churn spread
	// over the span.
	Events int
	// Span is the valid-time window the workload covers.
	Span time.Duration
}

// The two published scales. `10k` is the reference workload of constitution X and SC-001;
// `1k` is its tenth, small enough to run on a pull request as a smoke test.
var (
	// BenchScale10k is the reference workload: 10k entities, 100k edges, 1M events, 50k
	// changes, 200k claims, 90 days.
	BenchScale10k = BenchScale{
		Name: "10k", Entities: 10_000, Edges: 100_000, Changes: 50_000, Claims: 200_000,
		Events: 1_000_000, Span: 90 * 24 * time.Hour,
	}
	// BenchScale1k is the CI-sized smoke: the same shape at a tenth of the size.
	BenchScale1k = BenchScale{
		Name: "1k", Entities: 1_000, Edges: 10_000, Changes: 5_000, Claims: 20_000,
		Events: 100_000, Span: 90 * 24 * time.Hour,
	}
)

// BenchScales are the scales `--scale` accepts, smallest first.
var BenchScales = []BenchScale{BenchScale1k, BenchScale10k}

// BenchScaleByName resolves a `--scale` value.
func BenchScaleByName(name string) (BenchScale, error) {
	for _, scale := range BenchScales {
		if scale.Name == name {
			return scale, nil
		}
	}
	return BenchScale{}, fmt.Errorf("fixture: unknown scale %q; want 1k or 10k", name)
}

// churn returns how many events are left for property and weight-class churn once the
// structural events are accounted for.
func (s BenchScale) churn() int {
	used := s.Entities + s.Edges + s.Changes + s.Claims
	if s.Events <= used {
		return 0
	}
	return s.Events - used
}

// mergedServices is how many services the C2 claims merge: about 30% of them (research §2).
func (s BenchScale) mergedServices() int { return (s.Entities * 40 / 100) * 30 / 100 }

// BenchOptions configures RunBench.
type BenchOptions struct {
	// Scale is the workload size. Zero value means BenchScale10k.
	Scale BenchScale
	// NewEngine builds the read engine the query families are measured against. Required:
	// internal/query cannot be imported here (see BenchEngine).
	NewEngine BenchEngineFactory
	// Seed makes the run reproducible. Zero is a valid seed.
	Seed int64
	// Queries is how many randomized queries to run per family. Zero means
	// DefaultBenchQueries.
	Queries int
	// KeepDatabases leaves the generated databases in place instead of dropping them, and
	// reports their names. It is what makes "profile the slowest query" possible: an
	// EXPLAIN ANALYZE needs the data to still be there when the run has finished.
	KeepDatabases bool
	// SkipReplay leaves out the replay measurement, which is the longest single step. The
	// report says so, and the ADR-0002 replay criterion is reported as not measured.
	SkipReplay bool
	// Progress receives one line per phase and per progress tick. Nil discards them.
	Progress func(format string, args ...any)
	// BatchSize is how many events share one transaction during generation. Zero means
	// DefaultBenchBatch. It does not change what is written — one event is still one unit of
	// work for the projector — only how many commits the generation costs.
	BatchSize int
	// ReplayBatchSize is how many events share one transaction during the replay
	// measurement. Zero means projector.DefaultReplayBatchSize; 1 measures the
	// transaction-per-event loop the 2026-09-16 finding was about.
	ReplayBatchSize int
	// ReplayOnly measures the replay and nothing else: the generated log is written to a
	// database of its own and replayed into an empty projection, with no live ingestion and
	// no queries. It exists because the replay is the one measurement that has to be repeated
	// on its own — it is the ADR-0002 criterion that was open — and paying 38 minutes of
	// ingestion and 6,000 queries to re-measure it is how a number stops being re-measured.
	// The report says which criteria were not measured; it is not a substitute for a full run.
	ReplayOnly bool
}

// Benchmark defaults.
const (
	// DefaultBenchQueries is research §2's figure: 1,000 randomized queries per family.
	DefaultBenchQueries = 1000
	// DefaultBenchBatch is how many events share a transaction while the workload is being
	// generated.
	DefaultBenchBatch = 500
	// benchProgressEvery is how often generation reports progress.
	benchProgressEvery = 25_000
)

func (o BenchOptions) queries() int {
	if o.Queries <= 0 {
		return DefaultBenchQueries
	}
	return o.Queries
}

func (o BenchOptions) batchSize() int {
	if o.BatchSize <= 0 {
		return DefaultBenchBatch
	}
	return o.BatchSize
}

func (o BenchOptions) replayBatchSize() int {
	if o.ReplayBatchSize <= 0 {
		return projector.DefaultReplayBatchSize
	}
	return o.ReplayBatchSize
}

func (o BenchOptions) progress(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(format, args...)
	}
}

// RunBench generates the workload into a database of its own, measures ingestion, replay and
// the query families, and returns the report.
//
// newStore is the same factory `fixture verify` uses: it creates a uniquely named database,
// migrates it and drops it afterwards. The benchmark never touches an existing graph.
func RunBench(ctx context.Context, newStore StoreFactory, opts BenchOptions) (*BenchReport, error) {
	if opts.Scale.Name == "" {
		opts.Scale = BenchScale10k
	}
	if opts.NewEngine == nil {
		return nil, fmt.Errorf("fixture: BenchOptions.NewEngine is required")
	}
	scale := opts.Scale
	report := &BenchReport{
		Scale:     scale.Name,
		Seed:      opts.Seed,
		Queries:   opts.queries(),
		StartedAt: time.Now().UTC(),
		Workload: BenchWorkload{
			Entities: scale.Entities, Edges: scale.Edges, Changes: scale.Changes,
			Claims: scale.Claims, Events: scale.Events, SpanDays: int(scale.Span / (24 * time.Hour)),
		},
	}

	store, release, err := newStore(ctx)
	if err != nil {
		return nil, err
	}
	defer keepOrRelease(ctx, store, release, opts, "workload")

	env, err := describeEnvironment(ctx, store)
	if err != nil {
		return nil, err
	}
	report.Environment = env

	gen := newBenchGenerator(scale, opts.Seed)
	report.Workload.Events = len(gen.schedule)

	if opts.ReplayOnly {
		// The same workload, the same seed, therefore the same log — only the replay is
		// measured. The store opened above is used for the environment table and then left
		// empty.
		replay, err := measureReplay(ctx, newStore, gen, opts)
		if err != nil {
			return nil, err
		}
		report.Replay = replay
		opts.progress("replayed %d events in %s (%.0f events/s)", replay.Events,
			roundDur(time.Duration(replay.Seconds*float64(time.Second))), replay.PerSecond)
		report.FinishedAt = time.Now().UTC()
		report.evaluate(false, true)
		return report, nil
	}

	// ---- ingestion -------------------------------------------------------------------
	opts.progress("generating and applying %d events at scale %s (seed %d)", len(gen.schedule), scale.Name, opts.Seed)
	proj := projector.New(store)
	if err := registerBenchSources(ctx, proj); err != nil {
		return nil, err
	}

	ingestStart := time.Now()
	applied, err := gen.apply(ctx, proj, opts)
	if err != nil {
		return nil, err
	}
	report.Ingest = BenchIngest{
		Events:    applied,
		Seconds:   time.Since(ingestStart).Seconds(),
		PerSecond: float64(applied) / time.Since(ingestStart).Seconds(),
	}
	opts.progress("ingested %d events in %s (%.0f events/s)",
		applied, roundDur(time.Duration(report.Ingest.Seconds*float64(time.Second))), report.Ingest.PerSecond)

	counts, err := countGraph(ctx, store)
	if err != nil {
		return nil, err
	}
	report.Graph = counts
	opts.progress("graph holds %d entities, %d entity versions, %d edge versions, %d claims, %d merges",
		counts.Entities, counts.EntityVersions, counts.EdgeVersions, counts.Claims, counts.Merges)

	// ---- queries ---------------------------------------------------------------------
	summaries, err := runBenchQueries(ctx, store, gen, opts)
	if err != nil {
		return nil, err
	}
	report.Latencies = summaries

	size, err := databaseSize(ctx, store)
	if err != nil {
		return nil, err
	}
	report.DatabaseBytes = size

	// ---- replay ----------------------------------------------------------------------
	if !opts.SkipReplay {
		replay, err := measureReplay(ctx, newStore, gen, opts)
		if err != nil {
			return nil, err
		}
		report.Replay = replay
		opts.progress("replayed %d events in %s", replay.Events, roundDur(time.Duration(replay.Seconds*float64(time.Second))))
	}

	report.FinishedAt = time.Now().UTC()
	report.evaluate(opts.SkipReplay, false)
	return report, nil
}

// registerBenchSources declares the two synthetic feeders (FR-018).
func registerBenchSources(ctx context.Context, p *projector.Projector) error {
	for _, src := range []eventlog.Source{
		{SourceID: BenchSourceK8s, Kind: "k8s", Ordering: "per_source_sequence",
			ReorderingWindow: 60 * time.Second, SchemaVersion: feeder.SchemaVersion},
		{SourceID: BenchSourceOTel, Kind: "otel", Ordering: "none",
			ReorderingWindow: 300 * time.Second, SchemaVersion: feeder.SchemaVersion},
	} {
		if err := p.RegisterSource(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

// benchEntity is one generated entity and the facts a rule may read about it.
type benchEntity struct {
	Ref  graph.Ref
	Type graphv1.NodeType
	Name string
	// K8sNamespace is the Kubernetes namespace the entity lives in, and doubles as the OTel
	// `service.namespace` of the service that runs in it — which is the agreement C2 requires.
	K8sNamespace string
	// Env is the deployment environment, the other half of C2's agreement.
	Env string
}

// benchEdge is one generated relationship.
type benchEdge struct {
	Src, Dst    int
	Type        graphv1.EdgeType
	WeightClass uint32
}

// Event kinds in the schedule.
const (
	benchKindNode uint8 = iota
	benchKindEdge
	benchKindChange
	benchKindClaim
	benchKindNodeChurn
	benchKindEdgeChurn
)

// benchSlot is one scheduled event: when it happens and what it is. The schedule is built and
// sorted before anything is generated, so the stream arrives in valid-time order, which is what
// a real pair of feeders would produce.
type benchSlot struct {
	at   time.Time
	kind uint8
	idx  int32
}

// benchGenerator holds the generated topology and the event schedule.
type benchGenerator struct {
	scale    BenchScale
	entities []benchEntity
	edges    []benchEdge
	// services, hubs and focuses are the query seeds: every randomized query picks its focus
	// from focuses, which is services plus the highest-degree entities so that the p95 is not
	// dominated by leaves.
	services []int
	focuses  []int
	// mergePairs are the services a C2 claim pair merges, as indices into entities.
	mergePairs []benchMergePair
	schedule   []benchSlot
	// k8sSeq numbers the Kubernetes source's events, which declares per-source ordering.
	k8sSeq int64
}

// benchMergePair is one C2 merge: a workload that declares a service name and the service that
// telemetry observes under it.
type benchMergePair struct {
	Workload int
	Service  int
}

// Namespaces and environments the workload is spread over.
const (
	benchNamespaces  = 20
	benchClusterName = "bench"
)

var benchEnvs = []string{"prod", "prod", "prod", "staging"}

func newBenchGenerator(scale BenchScale, seed int64) *benchGenerator {
	//nolint:gosec // a benchmark wants a reproducible stream, not a cryptographic one.
	rng := rand.New(rand.NewPCG(uint64(seed), 0x9E3779B97F4A7C15))
	g := &benchGenerator{scale: scale}
	g.buildEntities(rng)
	g.buildEdges(rng)
	g.buildMergePairs(rng)
	g.buildSchedule(rng)
	return g
}

// buildEntities lays out the population. The proportions are a plausible mid-size estate: most
// things are services and the workloads that run them, with a long tail of infrastructure,
// configuration, vendors and owning teams.
func (g *benchGenerator) buildEntities(rng *rand.Rand) {
	n := g.scale.Entities
	counts := []struct {
		typ   graphv1.NodeType
		share int
	}{
		{graphv1.NodeType_SERVICE, 40},
		{graphv1.NodeType_WORKLOAD, 25},
		{graphv1.NodeType_INFRA_RESOURCE, 15},
		{graphv1.NodeType_CONFIG, 10},
		{graphv1.NodeType_THIRD_PARTY, 7},
		{graphv1.NodeType_OWNER, 3},
	}
	g.entities = make([]benchEntity, 0, n)
	for _, c := range counts {
		want := n * c.share / 100
		if c.typ == graphv1.NodeType_OWNER {
			// The last bucket absorbs the rounding, so the total is exactly n.
			want = n - len(g.entities)
		}
		for i := range want {
			g.entities = append(g.entities, g.newEntity(c.typ, i, rng))
		}
	}
	for i, e := range g.entities {
		if e.Type == graphv1.NodeType_SERVICE {
			g.services = append(g.services, i)
		}
	}
}

func (g *benchGenerator) newEntity(typ graphv1.NodeType, i int, rng *rand.Rand) benchEntity {
	ns := fmt.Sprintf("ns-%02d", rng.IntN(benchNamespaces))
	env := benchEnvs[rng.IntN(len(benchEnvs))]
	switch typ {
	case graphv1.NodeType_SERVICE:
		name := fmt.Sprintf("svc-%05d", i)
		return benchEntity{Ref: graph.Ref{Namespace: feeder.NSOTelService, Value: name},
			Type: typ, Name: name, K8sNamespace: ns, Env: env}
	case graphv1.NodeType_WORKLOAD:
		name := fmt.Sprintf("wl-%05d", i)
		return benchEntity{Ref: graph.Ref{Namespace: feeder.NSK8sDeployment, Value: ns + "/" + name},
			Type: typ, Name: name, K8sNamespace: ns, Env: env}
	case graphv1.NodeType_INFRA_RESOURCE:
		name := fmt.Sprintf("node-%05d", i)
		return benchEntity{Ref: graph.Ref{Namespace: feeder.NSK8sNode, Value: benchClusterName + "/" + name},
			Type: typ, Name: name, K8sNamespace: ns, Env: env}
	case graphv1.NodeType_CONFIG:
		name := fmt.Sprintf("cfg-%05d", i)
		return benchEntity{Ref: graph.Ref{Namespace: feeder.NSK8sConfigMap, Value: ns + "/" + name},
			Type: typ, Name: name, K8sNamespace: ns, Env: env}
	case graphv1.NodeType_THIRD_PARTY:
		name := fmt.Sprintf("dep-%05d.vendor.example", i)
		return benchEntity{Ref: graph.Ref{Namespace: feeder.NSServerAddress, Value: name},
			Type: typ, Name: name, K8sNamespace: ns, Env: env}
	default:
		name := fmt.Sprintf("team-%04d", i)
		return benchEntity{Ref: graph.Ref{Namespace: feeder.NSOwnerTeam, Value: name},
			Type: typ, Name: name, K8sNamespace: ns, Env: env}
	}
}

// buildEdges wires the population by preferential attachment, which is what produces the hubs.
//
// Each edge type draws its destination from a pool that every chosen destination is appended
// to, so a node that has already been chosen is more likely to be chosen again: the classic
// rich-get-richer construction, and the reason a handful of entities end up with thousands of
// neighbours while most have a handful.
func (g *benchGenerator) buildEdges(rng *rand.Rand) {
	byType := map[graphv1.NodeType][]int{}
	for i, e := range g.entities {
		byType[e.Type] = append(byType[e.Type], i)
	}
	services := byType[graphv1.NodeType_SERVICE]
	workloads := byType[graphv1.NodeType_WORKLOAD]
	infra := byType[graphv1.NodeType_INFRA_RESOURCE]
	configs := byType[graphv1.NodeType_CONFIG]
	vendors := byType[graphv1.NodeType_THIRD_PARTY]
	owners := byType[graphv1.NodeType_OWNER]

	plans := []struct {
		typ      graphv1.EdgeType
		share    int
		src, dst []int
		weighted bool
	}{
		{graphv1.EdgeType_CALLS, 55, services, services, true},
		{graphv1.EdgeType_DEPENDS_ON, 20, services, concat(configs, vendors), false},
		{graphv1.EdgeType_RUNS_ON, 15, workloads, infra, false},
		{graphv1.EdgeType_OWNED_BY, 10, concat(services, workloads), owners, false},
	}

	seen := make(map[benchEdgeKey]bool, g.scale.Edges)
	g.edges = make([]benchEdge, 0, g.scale.Edges)
	for _, plan := range plans {
		want := g.scale.Edges * plan.share / 100
		if len(plan.src) == 0 || len(plan.dst) == 0 {
			continue
		}
		pool := slices.Clone(plan.dst)
		for attempt := 0; len(g.edges) < cap(g.edges) && want > 0 && attempt < want*20; attempt++ {
			src := plan.src[rng.IntN(len(plan.src))]
			dst := pool[rng.IntN(len(pool))]
			if src == dst {
				continue
			}
			key := benchEdgeKey{Src: src, Dst: dst, Type: plan.typ}
			if seen[key] {
				continue
			}
			seen[key] = true
			edge := benchEdge{Src: src, Dst: dst, Type: plan.typ}
			if plan.weighted {
				edge.WeightClass = benchWeightClass(rng)
			}
			g.edges = append(g.edges, edge)
			// Rich get richer: the chosen destination goes back into the pool.
			pool = append(pool, dst)
			want--
		}
	}
	g.buildFocuses()
}

type benchEdgeKey struct {
	Src, Dst int
	Type     graphv1.EdgeType
}

// benchWeightClass draws a traffic class with a realistic skew: most call paths are quiet, a
// few carry everything (pkg/feeder §WeightClass).
func benchWeightClass(rng *rand.Rand) uint32 {
	switch r := rng.IntN(100); {
	case r < 35:
		return 1
	case r < 70:
		return 2
	case r < 90:
		return 3
	case r < 98:
		return 4
	default:
		return feeder.MaxWeightClass
	}
}

// buildFocuses picks the entities randomized queries focus on: every service, plus the 200
// highest-degree entities of any type, so the measured p95 includes the hub queries rather than
// averaging them away.
func (g *benchGenerator) buildFocuses() {
	degree := make([]int, len(g.entities))
	for _, e := range g.edges {
		degree[e.Src]++
		degree[e.Dst]++
	}
	ranked := make([]int, len(g.entities))
	for i := range ranked {
		ranked[i] = i
	}
	sort.SliceStable(ranked, func(a, b int) bool { return degree[ranked[a]] > degree[ranked[b]] })

	seen := map[int]bool{}
	for _, i := range ranked[:min(200, len(ranked))] {
		if degree[i] == 0 {
			continue
		}
		seen[i] = true
		g.focuses = append(g.focuses, i)
	}
	for _, i := range g.services {
		if !seen[i] && degree[i] > 0 {
			g.focuses = append(g.focuses, i)
		}
	}
	if len(g.focuses) == 0 {
		g.focuses = append(g.focuses, 0)
	}
	slices.Sort(g.focuses)
}

// buildMergePairs pairs about 30% of services with a workload, to be merged by certain rule C2.
//
// The pair has to agree on namespace and environment for C2 to fire, so the workload is
// rewritten to the service's, which is exactly the configuration an operator would have: a
// deployment in `ns-07/prod` declaring `app.kubernetes.io/name: svc-00042` and telemetry
// reporting `service.namespace=ns-07` for the same name.
func (g *benchGenerator) buildMergePairs(rng *rand.Rand) {
	workloads := make([]int, 0, len(g.entities))
	for i, e := range g.entities {
		if e.Type == graphv1.NodeType_WORKLOAD {
			workloads = append(workloads, i)
		}
	}
	rng.Shuffle(len(workloads), func(a, b int) { workloads[a], workloads[b] = workloads[b], workloads[a] })

	want := min(g.scale.mergedServices(), len(workloads), len(g.services))
	for i := range want {
		service := g.services[i]
		workload := workloads[i]
		g.entities[workload].K8sNamespace = g.entities[service].K8sNamespace
		g.entities[workload].Env = g.entities[service].Env
		g.mergePairs = append(g.mergePairs, benchMergePair{Workload: workload, Service: service})
	}
}

// buildSchedule lays every event on the timeline and sorts it, so the stream a feeder would
// have produced is the stream the projector sees.
//
// Structure first and quickly — the estate exists on day one — then claims in the first fifth
// of the span (resolution settles early, and the rest of the workload runs against a merged
// graph), then changes and churn spread over the whole 90 days.
func (g *benchGenerator) buildSchedule(rng *rand.Rand) {
	scale := g.scale
	span := scale.Span
	structural := time.Duration(float64(span) * 0.01)
	claimWindow := time.Duration(float64(span) * 0.2)

	total := len(g.entities) + len(g.edges) + scale.Changes + scale.Claims + scale.churn()
	g.schedule = make([]benchSlot, 0, total)

	at := func(window time.Duration, i, n int) time.Time {
		if n <= 1 {
			return BenchEpoch
		}
		return BenchEpoch.Add(time.Duration(float64(window) * float64(i) / float64(n)))
	}
	for i := range g.entities {
		g.schedule = append(g.schedule, benchSlot{at: at(structural/2, i, len(g.entities)),
			kind: benchKindNode, idx: int32(i)}) //nolint:gosec // bounded by Entities
	}
	for i := range g.edges {
		g.schedule = append(g.schedule, benchSlot{at: at(structural, i, len(g.edges)).Add(structural / 2),
			kind: benchKindEdge, idx: int32(i)}) //nolint:gosec // bounded by Edges
	}
	for i := range scale.Claims {
		offset := structural + time.Duration(rng.Int64N(int64(claimWindow)))
		g.schedule = append(g.schedule, benchSlot{at: BenchEpoch.Add(offset),
			kind: benchKindClaim, idx: int32(i)}) //nolint:gosec // bounded by Claims
	}
	for i := range scale.Changes {
		offset := structural + time.Duration(rng.Int64N(int64(span-structural)))
		g.schedule = append(g.schedule, benchSlot{at: BenchEpoch.Add(offset),
			kind: benchKindChange, idx: int32(i)}) //nolint:gosec // bounded by Changes
	}
	for i := range scale.churn() {
		offset := structural + time.Duration(rng.Int64N(int64(span-structural)))
		kind := benchKindNodeChurn
		if i%10 >= 7 {
			kind = benchKindEdgeChurn
		}
		g.schedule = append(g.schedule, benchSlot{at: BenchEpoch.Add(offset),
			kind: kind, idx: int32(i)}) //nolint:gosec // bounded by churn()
	}

	sort.SliceStable(g.schedule, func(a, b int) bool { return g.schedule[a].at.Before(g.schedule[b].at) })
}

// benchDesc is the SDK description each synthetic source emits under.
func benchDesc(sourceID string) feeder.Description {
	if sourceID == BenchSourceK8s {
		return feeder.Description{SourceID: BenchSourceK8s, Kind: "k8s",
			Ordering: feeder.OrderingPerSourceSequence, ReorderingWindow: 60 * time.Second}
	}
	return feeder.Description{SourceID: BenchSourceOTel, Kind: "otel",
		Ordering: feeder.OrderingNone, ReorderingWindow: 300 * time.Second}
}

// sourceFor says which feeder owns an entity: the telemetry one names services and vendors, the
// cluster one names everything it can see in the API server.
func (g *benchGenerator) sourceFor(i int) string {
	switch g.entities[i].Type {
	case graphv1.NodeType_SERVICE, graphv1.NodeType_THIRD_PARTY:
		return BenchSourceOTel
	default:
		return BenchSourceK8s
	}
}

// event renders one scheduled slot as an envelope.
func (g *benchGenerator) event(slot benchSlot) (*graphv1.EventEnvelope, error) {
	switch slot.kind {
	case benchKindNode:
		return g.nodeEvent(int(slot.idx), slot.at, 0)
	case benchKindEdge:
		return g.edgeEvent(int(slot.idx), slot.at, 0)
	case benchKindChange:
		return g.changeEvent(int(slot.idx), slot.at)
	case benchKindClaim:
		return g.claimEvent(int(slot.idx), slot.at)
	case benchKindNodeChurn:
		return g.nodeEvent(int(slot.idx)%len(g.entities), slot.at, int(slot.idx)+1)
	case benchKindEdgeChurn:
		return g.edgeEvent(int(slot.idx)%len(g.edges), slot.at, int(slot.idx)+1)
	default:
		return nil, fmt.Errorf("fixture: unknown bench slot kind %d", slot.kind)
	}
}

func (g *benchGenerator) nodeEvent(i int, at time.Time, revision int) (*graphv1.EventEnvelope, error) {
	e := g.entities[i]
	source := g.sourceFor(i)
	props := feeder.NewProps().
		Str("deployment.environment.name", e.Env).
		Str("k8s.namespace.name", e.K8sNamespace).
		Int("bench.revision", int64(revision))
	built, err := props.Build()
	if err != nil {
		return nil, fmt.Errorf("fixture: bench node props: %w", err)
	}
	id := fmt.Sprintf("%s:node:%d:%d", source, i, revision)
	return feeder.UpsertNode(benchDesc(source), id, feeder.NodeFact{
		Meta:        g.meta(source),
		Ref:         e.Ref.Proto(),
		Type:        e.Type,
		DisplayName: e.Name,
		Props:       built,
		ValidAt:     at,
	}), nil
}

func (g *benchGenerator) edgeEvent(i int, at time.Time, revision int) (*graphv1.EventEnvelope, error) {
	edge := g.edges[i]
	source := g.sourceFor(edge.Src)
	fact := feeder.EdgeFact{
		Meta:    g.meta(source),
		Src:     g.entities[edge.Src].Ref.Proto(),
		Dst:     g.entities[edge.Dst].Ref.Proto(),
		Type:    edge.Type,
		ValidAt: at,
	}
	if edge.Type == graphv1.EdgeType_CALLS {
		// Churn on a call edge is a weight-class transition, which is the only thing a real
		// feeder re-states an edge for (research §8).
		class := edge.WeightClass
		if revision > 0 {
			class = 1 + uint32(revision)%feeder.MaxWeightClass //nolint:gosec // bounded by MaxWeightClass
		}
		fact.WeightClass = feeder.WeightClassPtr(class)
	}
	id := fmt.Sprintf("%s:edge:%d:%d", source, i, revision)
	return feeder.UpsertEdge(benchDesc(source), id, fact), nil
}

// benchChangeKinds are the change kinds the workload contains: rollouts, scaling and
// configuration changes, which is what research §2 asks for.
var benchChangeKinds = []graphv1.ChangeKind{
	graphv1.ChangeKind_ROLLOUT, graphv1.ChangeKind_ROLLOUT, graphv1.ChangeKind_ROLLOUT,
	graphv1.ChangeKind_SCALING, graphv1.ChangeKind_CONFIG_CHANGE,
}

func (g *benchGenerator) changeEvent(i int, at time.Time) (*graphv1.EventEnvelope, error) {
	kind := benchChangeKinds[i%len(benchChangeKinds)]
	// Targets are drawn from the focus set, so changes land on the entities queries look at —
	// a change nobody can reach is a change no diff would ever rank.
	primary := g.focuses[i%len(g.focuses)]
	targets := []*graphv1.Ref{g.entities[primary].Ref.Proto()}
	if i%3 == 0 {
		secondary := g.focuses[(i*7+1)%len(g.focuses)]
		if secondary != primary {
			targets = append(targets, g.entities[secondary].Ref.Proto())
		}
	}
	source := BenchSourceK8s
	id := fmt.Sprintf("%s:change:%d", source, i)
	return feeder.ObserveChange(benchDesc(source), id, feeder.ChangeFact{
		Meta:    g.meta(source),
		Ref:     graph.Ref{Namespace: feeder.NSK8sChange, Value: fmt.Sprintf("bench/change-%07d", i)}.Proto(),
		Kind:    kind,
		Summary: fmt.Sprintf("%s of %s", kind, g.entities[primary].Name),
		Actor:   "bench-bot",
		Targets: targets,
		ValidAt: at,
	}), nil
}

// claimEvent produces either one half of a C2 merge pair or an ordinary, non-merging claim.
//
// The first 2×len(mergePairs) claims are the merge pairs, emitted adjacently so that the second
// half of a pair finds the first already stored — which is what makes C2 fire. Everything after
// them is a claim in a namespace the probable rules do not read, with a value unique to its
// source, so the bulk of the 200k neither merges anything nor drags the rule engine into a
// quadratic scan.
func (g *benchGenerator) claimEvent(i int, at time.Time) (*graphv1.EventEnvelope, error) {
	if pair := i / 2; pair < len(g.mergePairs) {
		return g.mergeClaimEvent(g.mergePairs[pair], pair, i%2 == 0, at)
	}
	n := i - 2*len(g.mergePairs)
	subject := g.entities[g.focuses[n%len(g.focuses)]]
	source := BenchSourceOTel
	if n%2 == 1 {
		source = BenchSourceK8s
	}
	attrs, err := feeder.NewProps().
		Str("deployment.environment.name", subject.Env).
		Str("service.namespace", subject.K8sNamespace).
		Build()
	if err != nil {
		return nil, fmt.Errorf("fixture: bench claim attributes: %w", err)
	}
	// The claim namespace is `server.address`, which no certain or probable rule pairs on, and
	// the value is unique per (source, n), so C1 has nothing to corroborate either.
	claim := graph.Ref{
		Namespace: feeder.NSServerAddress,
		Value:     fmt.Sprintf("ep-%07d.%s.bench.internal", n, shortSource(source)),
	}
	id := fmt.Sprintf("%s:claim:%d", source, n)
	return feeder.IdentityClaim(benchDesc(source), id, feeder.IdentityFact{
		Meta:       g.meta(source),
		Subject:    subject.Ref.Proto(),
		Claim:      claim.Proto(),
		Attributes: attrs,
	}), nil
}

// mergeClaimEvent emits one side of a C2 pair: the Kubernetes workload declaring the service
// name it runs, or the telemetry source observing that service name.
func (g *benchGenerator) mergeClaimEvent(pair benchMergePair, n int, declared bool, at time.Time) (*graphv1.EventEnvelope, error) {
	service := g.entities[pair.Service]
	claim := service.Ref // otel.service.name=<name>, claimed by both sides

	props := feeder.NewProps().Str(resolution.AttrEnvironment, service.Env)
	subject := service.Ref
	source := BenchSourceOTel
	if declared {
		subject = g.entities[pair.Workload].Ref
		source = BenchSourceK8s
		props.Str(resolution.AttrClaimKey, resolution.DefaultServiceNameClaimKeys[0]).
			Str(resolution.AttrK8sNamespace, service.K8sNamespace)
	} else {
		props.Str(resolution.AttrServiceNamespace, service.K8sNamespace)
	}
	attrs, err := props.Build()
	if err != nil {
		return nil, fmt.Errorf("fixture: bench merge claim attributes: %w", err)
	}
	id := fmt.Sprintf("%s:merge-claim:%d", source, n)
	return feeder.IdentityClaim(benchDesc(source), id, feeder.IdentityFact{
		Meta:       g.meta(source),
		Subject:    subject.Proto(),
		Claim:      claim.Proto(),
		Attributes: attrs,
	}), nil
}

// meta numbers the Kubernetes source's events, which declares per-source ordering, and leaves
// the telemetry source unnumbered, which does not.
func (g *benchGenerator) meta(source string) feeder.Meta {
	if source != BenchSourceK8s {
		return feeder.Meta{}
	}
	g.k8sSeq++
	return feeder.Meta{Seq: g.k8sSeq}
}

func shortSource(source string) string {
	if source == BenchSourceK8s {
		return "k8s"
	}
	return "otel"
}

// apply streams the whole schedule into the projector, batching events into transactions.
//
// The unit of work is still one event — ApplyInTx appends and projects each on its own, so the
// graph moves exactly as it would through the ingest RPC — but committing every 500 of them
// removes the commit cost from a measurement that is about the projection, not about fsync.
func (g *benchGenerator) apply(ctx context.Context, p *projector.Projector, opts BenchOptions) (int, error) {
	applied := 0
	batch := opts.batchSize()
	for start := 0; start < len(g.schedule); start += batch {
		end := min(start+batch, len(g.schedule))
		err := p.Store().WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			for _, slot := range g.schedule[start:end] {
				env, err := g.event(slot)
				if err != nil {
					return err
				}
				result, err := p.ApplyInTx(ctx, tx, env, projector.ApplyOptions{ObservedAt: slot.at})
				if err != nil {
					return err
				}
				if result.GetStatus() == graphv1.IngestResult_REJECTED {
					return fmt.Errorf("fixture: bench event %s rejected: %s: %s",
						env.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
				}
			}
			return nil
		})
		if err != nil {
			return applied, err
		}
		applied += end - start
		if applied%benchProgressEvery < batch {
			opts.progress("  applied %d/%d events", applied, len(g.schedule))
		}
	}
	return applied, nil
}

// appendOnly writes the same stream into a second database's log without projecting it, which
// is the setup a replay measurement needs: the log has to be there, the graph must not.
func (g *benchGenerator) appendOnly(ctx context.Context, p *projector.Projector, opts BenchOptions) error {
	batch := opts.batchSize()
	l := p.Log()
	for start := 0; start < len(g.schedule); start += batch {
		end := min(start+batch, len(g.schedule))
		err := p.Store().WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			for _, slot := range g.schedule[start:end] {
				env, err := g.event(slot)
				if err != nil {
					return err
				}
				result, err := l.Append(ctx, env, eventlog.AppendOptions{ObservedAt: slot.at, Tx: tx})
				if err != nil {
					return err
				}
				if result.GetStatus() == graphv1.IngestResult_REJECTED {
					return fmt.Errorf("fixture: bench event %s rejected on append: %s",
						env.GetEventId(), result.GetReasonCode())
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if (start+batch)%benchProgressEvery < batch {
			opts.progress("  logged %d/%d events for replay", end, len(g.schedule))
		}
	}
	return nil
}

// measureReplay times a full replay of the event log into an empty projection (constitution III,
// ADR-0002 exit criterion "replay of 1M events > 60 min").
//
// It needs its own database because a replay must run against an empty projection: the graph
// tables are append-only and replaying onto a populated one collides, which is the correct
// failure. The generator regenerates the identical stream from the same seed, appends it to
// that database's log without projecting, and only the Replay call is timed.
func measureReplay(ctx context.Context, newStore StoreFactory, g *benchGenerator, opts BenchOptions) (BenchReplay, error) {
	opts.progress("preparing the replay database (log only, no projection)")
	store, release, err := newStore(ctx)
	if err != nil {
		return BenchReplay{}, err
	}
	defer keepOrRelease(ctx, store, release, opts, "replay")

	p := projector.New(store)
	if err := registerBenchSources(ctx, p); err != nil {
		return BenchReplay{}, err
	}
	// The generator's own event numbering is stateful (the Kubernetes sequence), so it is reset
	// before the second pass or the two streams would not be the same events.
	g.k8sSeq = 0
	if err := g.appendOnly(ctx, p, opts); err != nil {
		return BenchReplay{}, err
	}

	batch := opts.replayBatchSize()
	opts.progress("replaying the log into an empty projection (%d events per transaction)", batch)
	start := time.Now()
	report, err := p.ReplayWithOptions(ctx, projector.ReplayOptions{
		BatchSize: batch,
		Progress: func(done, total int64) {
			// One line per benchProgressEvery events: a replay that says nothing for an hour
			// is indistinguishable from a replay that has hung.
			if done%benchProgressEvery < int64(batch) {
				elapsed := time.Since(start)
				opts.progress("  replayed %d/%d events in %s (%.0f events/s)",
					done, total, roundDur(elapsed), float64(done)/elapsed.Seconds())
			}
		},
	})
	if err != nil {
		return BenchReplay{}, err
	}
	elapsed := time.Since(start)
	return BenchReplay{
		Measured:  true,
		Events:    report.Events,
		Seconds:   elapsed.Seconds(),
		PerSecond: float64(report.Events) / elapsed.Seconds(),
		BatchSize: batch,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func concat[T any](parts ...[]T) []T {
	var out []T
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func roundDur(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}

func countGraph(ctx context.Context, store *postgres.Store) (BenchGraphCounts, error) {
	var c BenchGraphCounts
	err := store.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM graph.entities WHERE merged_into IS NULL),
		       (SELECT count(*) FROM graph.entity_versions),
		       (SELECT count(*) FROM graph.edge_versions),
		       (SELECT count(*) FROM graph.identity_claims),
		       (SELECT count(*) FROM graph.entities WHERE merged_into IS NOT NULL),
		       (SELECT count(*) FROM graph.suggestions),
		       (SELECT count(*) FROM log.events)`).
		Scan(&c.Entities, &c.EntityVersions, &c.EdgeVersions, &c.Claims, &c.Merges,
			&c.Suggestions, &c.LoggedEvents)
	if err != nil {
		return c, fmt.Errorf("fixture: count benchmark graph: %w", err)
	}
	return c, nil
}

func databaseSize(ctx context.Context, store *postgres.Store) (int64, error) {
	var size int64
	if err := store.Pool().QueryRow(ctx,
		`SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		return 0, fmt.Errorf("fixture: read database size: %w", err)
	}
	return size, nil
}

// summarize turns a set of measured durations into the percentiles research §2 asks for.
func summarize(name, detail string, samples []time.Duration) BenchLatency {
	out := BenchLatency{Name: name, Detail: detail, Count: len(samples)}
	if len(samples) == 0 {
		return out
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	out.MeanMs = ms(total / time.Duration(len(sorted)))
	out.P50Ms = ms(percentile(sorted, 0.50))
	out.P95Ms = ms(percentile(sorted, 0.95))
	out.P99Ms = ms(percentile(sorted, 0.99))
	out.MaxMs = ms(sorted[len(sorted)-1])
	return out
}

// percentile is the nearest-rank percentile of a sorted slice.
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*q+0.5) - 1
	return sorted[max(0, min(rank, len(sorted)-1))]
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// byName sorts latency summaries into the published order.
func byName(a, b BenchLatency) int { return cmp.Compare(a.Name, b.Name) }

// keepOrRelease drops the benchmark database, or keeps it and says where it is.
func keepOrRelease(ctx context.Context, store *postgres.Store, release func(), opts BenchOptions, role string) {
	if !opts.KeepDatabases {
		release()
		return
	}
	var name string
	if err := store.Pool().QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		name = "unknown"
	}
	opts.progress("keeping the %s database %q (drop it with DROP DATABASE %q)", role, name, name)
	store.Close()
}
