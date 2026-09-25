// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// Unit tests for the subgraph read, over graphs built here rather than loaded from a fixture
// (T032). The fixtures prove the whole pipeline; these prove one behaviour each, on the
// smallest graph that can exhibit it, so a failure names the rule that broke.
//
// Every graph goes in through the projector, not through SQL: a query test that wrote its own
// rows could pass against a projection the projector would never produce.

// baseValid is the valid instant the hand-built graphs are true from, and observedStart the
// instant they were learned. Both are fixed so a failure is reproducible.
var (
	baseValid    = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	observeStart = time.Date(2026, 9, 1, 13, 1, 0, 0, time.UTC)
	queryAt      = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
)

const (
	testSource = "otel:test"
	testNS     = "otel.service.name"
)

// builder applies hand-written events to a store, stamping each with an observed time that
// advances by a second, the way a feeder's deliveries would.
type builder struct {
	t         *testing.T
	projector *projector.Projector
	sourceID  string
	seq       int
	observed  time.Time
}

func newBuilder(t *testing.T) *builder {
	t.Helper()
	store := pgtest.Open(t)
	p := projector.New(store)
	if err := p.RegisterSource(context.Background(), eventlog.Source{
		SourceID:      testSource,
		Kind:          "otel",
		Ordering:      "none",
		SchemaVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("register source: %v", err)
	}
	return &builder{t: t, projector: p, sourceID: testSource, observed: observeStart}
}

func (b *builder) engine() *query.Engine { return query.NewEngine(b.projector.Store()) }

// source returns a builder that writes as another feeder into the same graph. Two sources are
// what the certain resolution rules need: one source asserting an identifier twice is one
// opinion, not corroboration (research §10, rule C1).
func (b *builder) source(sourceID, kind string) *builder {
	b.t.Helper()
	if err := b.projector.RegisterSource(context.Background(), eventlog.Source{
		SourceID:      sourceID,
		Kind:          kind,
		Ordering:      "none",
		SchemaVersion: "1.0.0",
	}); err != nil {
		b.t.Fatalf("register source %s: %v", sourceID, err)
	}
	return &builder{t: b.t, projector: b.projector, sourceID: sourceID, observed: b.observed}
}

// apply submits one event with the next observed time, or with observedAt when it is set — the
// late-arriving case.
func (b *builder) apply(body any, observedAt time.Time) {
	b.t.Helper()
	b.seq++
	env := &graphv1.EventEnvelope{
		EventId:       fmt.Sprintf("%s:e%03d", b.sourceID, b.seq),
		SourceId:      b.sourceID,
		SchemaVersion: "1.0.0",
	}
	switch v := body.(type) {
	case *graphv1.UpsertNode:
		env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: v}
	case *graphv1.UpsertEdge:
		env.Body = &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: v}
	case *graphv1.IdentityClaim:
		env.Body = &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: v}
	case *graphv1.ObserveChange:
		env.Body = &graphv1.EventEnvelope_ObserveChange{ObserveChange: v}
	case *graphv1.RetractNode:
		env.Body = &graphv1.EventEnvelope_RetractNode{RetractNode: v}
	case *graphv1.RetractEdge:
		env.Body = &graphv1.EventEnvelope_RetractEdge{RetractEdge: v}
	// 003 plan item 2. The three bodies of the edge half of resolution.
	case *graphv1.ProposeDependency:
		env.Body = &graphv1.EventEnvelope_ProposeDependency{ProposeDependency: v}
	case *graphv1.ConfirmDependency:
		env.Body = &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: v}
	case *graphv1.RejectDependency:
		env.Body = &graphv1.EventEnvelope_RejectDependency{RejectDependency: v}
	default:
		b.t.Fatalf("builder: unsupported event body %T", body)
	}

	if observedAt.IsZero() {
		b.observed = b.observed.Add(time.Second)
		observedAt = b.observed
	}
	result, err := b.projector.ApplyWithOptions(context.Background(), env,
		projector.ApplyOptions{ObservedAt: observedAt})
	if err != nil {
		b.t.Fatalf("apply %s: %v", env.GetEventId(), err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		b.t.Fatalf("apply %s: status %s (%s: %s)", env.GetEventId(),
			result.GetStatus(), result.GetReasonCode(), result.GetReasonDetail())
	}
}

// node upserts a service under its telemetry name.
func (b *builder) node(name string) {
	b.t.Helper()
	props, err := structpb.NewStruct(map[string]any{"service.name": name})
	if err != nil {
		b.t.Fatalf("props: %v", err)
	}
	b.apply(&graphv1.UpsertNode{
		Ref:         &graphv1.Ref{Namespace: testNS, Value: name},
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: name,
		Props:       props,
		ValidAt:     timestamppb.New(baseValid),
	}, time.Time{})
}

// calls adds a weighted calls edge, optionally learned at a later observed instant.
func (b *builder) calls(src, dst string, weight uint32, validAt, observedAt time.Time) {
	b.t.Helper()
	if validAt.IsZero() {
		validAt = baseValid
	}
	b.apply(&graphv1.UpsertEdge{
		Src:         &graphv1.Ref{Namespace: testNS, Value: src},
		Dst:         &graphv1.Ref{Namespace: testNS, Value: dst},
		Type:        graphv1.EdgeType_CALLS,
		WeightClass: &weight,
		ValidAt:     timestamppb.New(validAt),
	}, observedAt)
}

// dependsOn adds an unweighted depends_on edge, the kind an edge-type filter has to be able to
// exclude.
func (b *builder) dependsOn(src, dst string) {
	b.t.Helper()
	b.apply(&graphv1.UpsertEdge{
		Src:     &graphv1.Ref{Namespace: testNS, Value: src},
		Dst:     &graphv1.Ref{Namespace: "k8s.configmap", Value: dst},
		Type:    graphv1.EdgeType_DEPENDS_ON,
		ValidAt: timestamppb.New(baseValid),
	}, time.Time{})
}

func (b *builder) claim(subject, claim *graphv1.Ref) {
	b.t.Helper()
	b.apply(&graphv1.IdentityClaim{Subject: subject, Claim: claim}, time.Time{})
}

// chain builds a -> b -> c -> d, each call one weight class quieter than the last.
func (b *builder) chain() {
	b.t.Helper()
	for _, name := range []string{"a", "b", "c", "d"} {
		b.node(name)
	}
	b.calls("a", "b", 4, time.Time{}, time.Time{})
	b.calls("b", "c", 3, time.Time{}, time.Time{})
	b.calls("c", "d", 1, time.Time{}, time.Time{})
}

// ask runs a subgraph query and returns the display names it found, sorted.
func ask(t *testing.T, e *query.Engine, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, []string) {
	t.Helper()
	if req.GetAsOf() == nil {
		req.AsOf = &graphv1.AsOf{ValidAt: timestamppb.New(queryAt)}
	}
	resp, err := e.Subgraph(context.Background(), req)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	names := make([]string, 0, len(resp.GetNodes()))
	for _, node := range resp.GetNodes() {
		names = append(names, node.GetDisplayName())
	}
	slices.Sort(names)
	return resp, names
}

func focus(name string) *graphv1.Ref {
	return &graphv1.Ref{Namespace: testNS, Value: name}
}

// hasPlaceholder reports whether any returned node is one the projector minted to satisfy an
// edge endpoint: it carries `sre.placeholder=true` and nothing else (projector/refs.go).
func hasPlaceholder(resp *graphv1.SubgraphResponse) bool {
	for _, node := range resp.GetNodes() {
		if node.GetProps().GetFields()["sre.placeholder"].GetBoolValue() {
			return true
		}
	}
	return false
}

func assertNames(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
}

// TestHopsBoundTheNeighbourhood: the radius is exactly N hops, counted from the focus.
func TestHopsBoundTheNeighbourhood(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	e := b.engine()

	for _, tc := range []struct {
		hops uint32
		want []string
	}{
		{1, []string{"a", "b"}},
		{2, []string{"a", "b", "c"}},
		{3, []string{"a", "b", "c", "d"}},
		{4, []string{"a", "b", "c", "d"}},
	} {
		_, names := ask(t, e, &graphv1.SubgraphRequest{
			Focus:     focus("a"),
			Hops:      tc.hops,
			Direction: graphv1.Direction_DOWNSTREAM,
		})
		assertNames(t, names, tc.want)
	}
}

// TestDefaultsAreTwoHopsBoth: an unset radius and direction mean two hops in both directions
// (research §2, FR-026), and the response states the caps it applied.
func TestDefaultsAreTwoHopsBoth(t *testing.T) {
	b := newBuilder(t)
	b.chain()

	resp, names := ask(t, b.engine(), &graphv1.SubgraphRequest{Focus: focus("b")})
	assertNames(t, names, []string{"a", "b", "c", "d"})
	if resp.GetTruncation().GetPerHopCap() != query.DefaultPerHopCap ||
		resp.GetTruncation().GetTotalCap() != query.DefaultTotalCap {
		t.Errorf("caps = %d/%d, want the published defaults %d/%d",
			resp.GetTruncation().GetPerHopCap(), resp.GetTruncation().GetTotalCap(),
			query.DefaultPerHopCap, query.DefaultTotalCap)
	}
	if resp.GetFocus().GetDisplayName() != "b" {
		t.Errorf("focus = %q, want b", resp.GetFocus().GetDisplayName())
	}
}

// TestDirectionSemantics: UPSTREAM is who calls the focus, DOWNSTREAM is what the focus calls.
// Getting this backwards would invert every blast-radius answer built on it, so it is asserted
// in both directions on the same graph.
func TestDirectionSemantics(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	e := b.engine()

	_, up := ask(t, e, &graphv1.SubgraphRequest{
		Focus: focus("b"), Hops: 1, Direction: graphv1.Direction_UPSTREAM,
	})
	assertNames(t, up, []string{"a", "b"})

	_, down := ask(t, e, &graphv1.SubgraphRequest{
		Focus: focus("b"), Hops: 1, Direction: graphv1.Direction_DOWNSTREAM,
	})
	assertNames(t, down, []string{"b", "c"})

	_, both := ask(t, e, &graphv1.SubgraphRequest{
		Focus: focus("b"), Hops: 1, Direction: graphv1.Direction_BOTH,
	})
	assertNames(t, both, []string{"a", "b", "c"})
}

// TestPerHopCapTruncatesAHub is the "hub explosion" edge case: a cache eight services talk to
// is expanded to the cap, the heaviest callers first, and the response says which node it cut.
func TestPerHopCapTruncatesAHub(t *testing.T) {
	b := newBuilder(t)
	b.node("hub")
	// Three loud callers and five progressively quieter ones, so "the cap keeps the heaviest"
	// has an unambiguous answer.
	for i, weight := range []uint32{5, 5, 5, 4, 3, 2, 1, 0} {
		name := fmt.Sprintf("svc%d", i)
		b.node(name)
		b.calls(name, "hub", weight, time.Time{}, time.Time{})
	}

	cap3 := uint32(3)
	resp, names := ask(t, b.engine(), &graphv1.SubgraphRequest{
		Focus: focus("hub"), Hops: 1, Direction: graphv1.Direction_UPSTREAM, PerHopCap: &cap3,
	})

	if len(names) != 4 {
		t.Errorf("nodes = %v, want the hub plus 3 callers", names)
	}
	tr := resp.GetTruncation()
	if !tr.GetTruncated() {
		t.Fatal("a hub expanded past the per-hop cap must report truncation (FR-026)")
	}
	if tr.GetReason() != query.ReasonPerHopCap {
		t.Errorf("reason = %q, want %q", tr.GetReason(), query.ReasonPerHopCap)
	}
	if tr.GetPerHopCap() != 3 {
		t.Errorf("per_hop_cap = %d, want 3", tr.GetPerHopCap())
	}
	if len(tr.GetTruncatedAtEntityIds()) != 1 {
		t.Errorf("truncated_at = %v, want exactly the hub", tr.GetTruncatedAtEntityIds())
	} else if tr.GetTruncatedAtEntityIds()[0] != resp.GetFocus().GetEntityId() {
		t.Errorf("truncated_at = %v, want the hub's id %s",
			tr.GetTruncatedAtEntityIds(), resp.GetFocus().GetEntityId())
	}

	// The cap keeps the heaviest traffic: every kept edge is class 5, the loudest callers.
	for _, edge := range resp.GetEdges() {
		if edge.GetWeightClass() != 5 {
			t.Errorf("kept an edge of class %d; the cap must drop the quiet ones first", edge.GetWeightClass())
		}
	}
}

// TestTotalCapStopsTheWalk: the whole-response cap bites before the radius does, and says so.
func TestTotalCapStopsTheWalk(t *testing.T) {
	b := newBuilder(t)
	b.chain()

	total := uint32(2)
	resp, names := ask(t, b.engine(), &graphv1.SubgraphRequest{
		Focus: focus("a"), Hops: 3, Direction: graphv1.Direction_DOWNSTREAM, TotalCap: &total,
	})
	assertNames(t, names, []string{"a", "b"})
	if resp.GetTruncation().GetReason() != query.ReasonTotalCap {
		t.Errorf("reason = %q, want %q", resp.GetTruncation().GetReason(), query.ReasonTotalCap)
	}
	if !resp.GetTruncation().GetTruncated() {
		t.Error("hitting the total cap is a truncation")
	}
}

// TestEdgeTypeFilter: a filter restricts which edges may be followed, so a node only reachable
// over an excluded type is absent.
func TestEdgeTypeFilter(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	b.dependsOn("a", "shop/a-config")
	e := b.engine()

	// Unfiltered, the config is reached over depends_on. It is a placeholder — an endpoint no
	// upsert_node has described — so it has no display name, which is itself the answer
	// "something called this exists and nothing has described it".
	unfiltered, all := ask(t, e, &graphv1.SubgraphRequest{Focus: focus("a"), Hops: 1})
	if len(all) != 3 {
		t.Errorf("unfiltered 1-hop = %v, want a, b and the config placeholder", all)
	}
	if !hasPlaceholder(unfiltered) {
		t.Error("the config endpoint should be returned as a placeholder, not hidden")
	}

	_, calls := ask(t, e, &graphv1.SubgraphRequest{
		Focus:     focus("a"),
		Hops:      1,
		EdgeTypes: []graphv1.EdgeType{graphv1.EdgeType_CALLS},
	})
	assertNames(t, calls, []string{"a", "b"})
}

// TestMinWeightClassFilter: asking for traffic drops the quiet edges, and with them the nodes
// only they reached. An edge that carries no weight class at all counts as 0.
func TestMinWeightClassFilter(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	b.dependsOn("a", "shop/a-config")

	min3 := uint32(3)
	_, names := ask(t, b.engine(), &graphv1.SubgraphRequest{
		Focus:          focus("a"),
		Hops:           3,
		Direction:      graphv1.Direction_DOWNSTREAM,
		MinWeightClass: &min3,
	})
	// a -> b is class 4 and b -> c class 3, so both are kept; c -> d is class 1 and the
	// depends_on edge is unweighted, so d and the config are gone.
	assertNames(t, names, []string{"a", "b", "c"})
}

// TestObservedPinningExcludesALaterLearnedEdge is US1 scenario 4 in miniature: a fact learned
// at 15:00 about 14:20 is in the answer as known now and out of it as known at 14:32.
func TestObservedPinningExcludesALaterLearnedEdge(t *testing.T) {
	b := newBuilder(t)
	b.node("a")
	b.node("b")
	b.node("late")
	b.calls("a", "b", 3, time.Time{}, time.Time{})

	learnedAt := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	trueFrom := time.Date(2026, 9, 1, 14, 20, 0, 0, time.UTC)
	b.calls("a", "late", 2, trueFrom, learnedAt)
	e := b.engine()

	_, now := ask(t, e, &graphv1.SubgraphRequest{Focus: focus("a"), Hops: 1})
	if !slices.Contains(now, "late") {
		t.Errorf("as known now = %v, want the late-arriving fact included", now)
	}

	_, pinned := ask(t, e, &graphv1.SubgraphRequest{
		Focus: focus("a"),
		Hops:  1,
		AsOf: &graphv1.AsOf{
			ValidAt:    timestamppb.New(queryAt),
			ObservedAt: timestamppb.New(queryAt),
		},
	})
	if slices.Contains(pinned, "late") {
		t.Errorf("as known at 14:32 = %v, want the fact learned at 15:00 excluded", pinned)
	}
}

// TestMergedEntityResolvesFromEitherAlias: after a certain rule merges two names, querying
// under either one returns the survivor, carrying both names as aliases and both asserted
// types as facets (FR-039, research §10).
func TestMergedEntityResolvesFromEitherAlias(t *testing.T) {
	b := newBuilder(t)
	b.node("checkout")
	b.node("peer")
	b.calls("peer", "checkout", 3, time.Time{}, time.Time{})

	// A second source — the Kubernetes feeder — describes a workload and claims the same
	// telemetry service name for it. Two sources asserting one identifier is certain rule C1,
	// so the two entities are merged with a recorded decision.
	k8s := b.source("k8s:test", "k8s")
	workload := &graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"}
	k8s.apply(&graphv1.UpsertNode{
		Ref:         workload,
		Type:        graphv1.NodeType_WORKLOAD,
		DisplayName: "checkout-deployment",
		ValidAt:     timestamppb.New(baseValid),
	}, time.Time{})
	k8s.claim(workload, &graphv1.Ref{Namespace: testNS, Value: "checkout"})
	e := b.engine()

	byService, _ := ask(t, e, &graphv1.SubgraphRequest{Focus: focus("checkout"), Hops: 1})
	byWorkload, _ := ask(t, e, &graphv1.SubgraphRequest{Focus: workload, Hops: 1})

	if byService.GetFocus().GetEntityId() != byWorkload.GetFocus().GetEntityId() {
		t.Fatalf("the two names resolve to different entities: %s and %s",
			byService.GetFocus().GetEntityId(), byWorkload.GetFocus().GetEntityId())
	}
	var aliases []string
	for _, alias := range byService.GetFocus().GetAliases() {
		aliases = append(aliases, alias.GetNamespace()+"="+alias.GetValue())
	}
	for _, want := range []string{testNS + "=checkout", "k8s.deployment=shop/checkout"} {
		if !slices.Contains(aliases, want) {
			t.Errorf("aliases = %v, want %q among them", aliases, want)
		}
	}
	if !slices.Contains(byService.GetFocus().GetFacets(), graphv1.NodeType_WORKLOAD) ||
		!slices.Contains(byService.GetFocus().GetFacets(), graphv1.NodeType_SERVICE) {
		t.Errorf("facets = %v, want both SERVICE and WORKLOAD kept", byService.GetFocus().GetFacets())
	}

	// The edge stored before the merge still names the id that was merged away; the read has to
	// resolve it, or the survivor would look unconnected.
	if len(byService.GetEdges()) != 1 {
		t.Fatalf("edges = %d, want the pre-merge edge resolved onto the survivor", len(byService.GetEdges()))
	}
	if byService.GetEdges()[0].GetDstId() != byService.GetFocus().GetEntityId() {
		t.Errorf("edge dst = %s, want the surviving id %s",
			byService.GetEdges()[0].GetDstId(), byService.GetFocus().GetEntityId())
	}
}

// TestBeforeRecordedHistory: an instant earlier than anything observed is an empty, flagged
// answer — not an error, and not a truncation.
func TestBeforeRecordedHistory(t *testing.T) {
	b := newBuilder(t)
	b.chain()

	resp, names := ask(t, b.engine(), &graphv1.SubgraphRequest{
		Focus: focus("a"),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(baseValid.Add(-30 * 24 * time.Hour))},
	})
	if len(names) != 0 || len(resp.GetEdges()) != 0 {
		t.Errorf("before history returned %v and %d edges, want nothing", names, len(resp.GetEdges()))
	}
	if resp.GetFocus() != nil {
		t.Errorf("before history returned a focus version: %v", resp.GetFocus())
	}
	if resp.GetTruncation().GetReason() != query.ReasonBeforeHistory {
		t.Errorf("reason = %q, want %q", resp.GetTruncation().GetReason(), query.ReasonBeforeHistory)
	}
	if resp.GetTruncation().GetTruncated() {
		t.Error("before recorded history is a flag, not a truncation")
	}
	if resp.GetExtent().GetEarliestObserved() == nil {
		t.Error("the response must still carry the extent that made the answer empty")
	}
}

// TestUnknownFocusIsNotFound: a name the graph has never seen is NotFound, distinct from a name
// with nothing valid at the instant, which is an empty answer.
func TestUnknownFocusIsNotFound(t *testing.T) {
	b := newBuilder(t)
	b.chain()

	_, err := b.engine().Subgraph(context.Background(), &graphv1.SubgraphRequest{
		Focus: focus("nothing-called-this"),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(queryAt)},
	})
	if err == nil {
		t.Fatal("an unknown focus must be an error, not an empty result")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %s, want %s", connect.CodeOf(err), connect.CodeNotFound)
	}
	if !errors.Is(err, query.ErrFocusNotFound) {
		t.Errorf("error %v does not wrap ErrFocusNotFound", err)
	}
}

// TestMissingValidInstantIsInvalidArgument: the graph answers as of an instant, and defaulting
// a missing one to now would silently answer a different question (constitution II).
func TestMissingValidInstantIsInvalidArgument(t *testing.T) {
	b := newBuilder(t)
	b.chain()

	_, err := b.engine().Subgraph(context.Background(), &graphv1.SubgraphRequest{Focus: focus("a")})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want %s", connect.CodeOf(err), connect.CodeInvalidArgument)
	}
}
