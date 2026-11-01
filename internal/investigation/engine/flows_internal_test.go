// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// flowWorld is checkout calling payments calling ledger, with a lone config entity no edge
// reaches, and a retracted edge payments -> fraud in the window.
func flowWorld(t *testing.T) *Engine {
	t.Helper()
	ptr := func(sel string) *graphv1.Pointer {
		return &graphv1.Pointer{Kind: graphv1.PointerKind_METRIC, Selector: sel}
	}
	node := func(id, name string, typ graphv1.NodeType, ptrs ...*graphv1.Pointer) *graphv1.NodeVersion {
		return &graphv1.NodeVersion{EntityId: id, Type: typ, Pointers: ptrs,
			Aliases: []*graphv1.Ref{{Namespace: "otel.service.name", Value: name}}}
	}
	calls := func(src, dst string) *graphv1.EdgeVersion {
		return &graphv1.EdgeVersion{SrcId: src, DstId: dst, Type: graphv1.EdgeType_CALLS}
	}
	nodes := []*graphv1.NodeVersion{
		node("e-payments", "payments", graphv1.NodeType_SERVICE, ptr("payments-rate")),
		node("e-ledger", "ledger", graphv1.NodeType_SERVICE, ptr("ledger-rate")),
		node("e-config", "config", graphv1.NodeType_CONFIG),
		node("e-fraud", "fraud", graphv1.NodeType_SERVICE, ptr("fraud-rate")),
	}
	focus := node("e-checkout", "checkout", graphv1.NodeType_SERVICE, ptr("checkout-rate"))
	now := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	profile := BuildProfile(&graphv1.SubgraphResponse{
		Focus: focus, Nodes: nodes,
		Edges: []*graphv1.EdgeVersion{calls("e-checkout", "e-payments"), calls("e-payments", "e-ledger")},
	}, now.Add(-time.Hour), now, now)

	book, err := ledger.New("inv-x", audit.PriorRecord{Prior: 0.4, AuditID: "a", Ceiling: 0.6, IncidentCount: 13})
	if err != nil {
		t.Fatal(err)
	}
	catalogue := NewCatalogue()
	catalogue.NoteNode(focus)
	for _, n := range nodes {
		catalogue.NoteNode(n)
	}
	e := &Engine{ledger: book, catalogue: catalogue, profile: profile, flowHypotheses: map[string]string{}}
	e.noteFallenEdges(&graphv1.DiffResponse{EdgesRemoved: []*graphv1.EdgeVersion{calls("e-payments", "e-fraud")}})
	return e
}

// TestTheWaveComparesCallersCalleesAndRetractedEdges is FR-046b's selection: a candidate on payments
// reaches its caller and its callee, a retracted edge adds its callee, and a candidate on an entity
// no edge reaches is a blind spot rather than an invented comparison.
func TestTheWaveComparesCallersCalleesAndRetractedEdges(t *testing.T) {
	t.Parallel()
	e := flowWorld(t)
	flows := e.planFlows([]candidatePlan{
		{hypothesisID: "h-1", targetRef: "otel.service.name=payments"},
		{hypothesisID: "h-2", targetRef: "otel.service.name=config"},
	})
	var got []string
	for _, f := range flows {
		got = append(got, f.entityRef+" <- "+f.hypothesisID)
	}
	want := []string{
		"otel.service.name=ledger <- h-1",
		"otel.service.name=checkout <- h-1",
		"otel.service.name=fraud <- ",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("flows = %v, want %v", got, want)
	}
	blind := strings.Join(e.Profile().BlindSpots, "\n")
	if !strings.Contains(blind, "otel.service.name=config, the target of h-2, has no caller or callee") {
		t.Errorf("the candidate with no neighbour is not a blind spot:\n%s", blind)
	}
}

// TestAFallOpensAnUnjudgedStoppedFlowCandidate: a separable fall of at least half opens a
// stopped_flow condition with no judgment, scaled by its magnitude; a smaller fall and a rise open
// nothing, and the same entity is never opened twice.
func TestAFallOpensAnUnjudgedStoppedFlowCandidate(t *testing.T) {
	t.Parallel()
	e := flowWorld(t)
	rate := func(id, sel string, relative float64, direction string) *Answer {
		return &Answer{
			EvidenceID: id, Capability: "compare",
			Request: &investigationv1.AlgebraRequest{Term: &investigationv1.AlgebraTerm{
				Term: &investigationv1.AlgebraTerm_Compare{Compare: &investigationv1.CompareTerm{
					Pointer: &graphv1.Pointer{Kind: graphv1.PointerKind_METRIC, Selector: sel},
				}}}},
			Response: &investigationv1.AlgebraResponse{
				Outcome: investigationv1.TermOutcome_DIGEST,
				Digest: &investigationv1.Digest{Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
					Comparisons: []*investigationv1.Comparison{{
						Statistic: investigationv1.Statistic_RATE, RelativeDelta: relative,
						Direction: direction, Separable: true,
					}},
				}}},
			},
		}
	}
	opened, err := e.openStoppedFlows([]*Answer{
		rate("e-1", "fraud-rate", -0.97, "down"),
		rate("e-2", "ledger-rate", -0.3, "down"),
		rate("e-3", "checkout-rate", 0.8, "up"),
		rate("e-4", "fraud-rate", -0.9, "down"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 {
		t.Fatalf("opened = %v, want one stopped flow, on fraud", opened)
	}
	h, _ := e.ledger.Hypothesis(opened[0])
	if h.Kind != ledger.KindCondition || h.Status != ledger.StatusProposed ||
		len(h.TargetEntityIDs) != 1 || h.TargetEntityIDs[0] != "e-fraud" {
		t.Errorf("hypothesis = %+v", h)
	}
	if !strings.Contains(h.Statement, "otel.service.name=fraud stopped") || !strings.Contains(h.Statement, "97%") {
		t.Errorf("statement = %q", h.Statement)
	}
	if got := e.CategoryOf(h); got != CategoryStoppedFlow {
		t.Errorf("category = %q, want %q", got, CategoryStoppedFlow)
	}
}
