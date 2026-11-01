// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// pathWorld is flowWorld (checkout -> payments -> ledger, a lone config, fraud with no edge in
// scope) with checkout as the subject and one answer on payments whose comparison separated.
func pathWorld(t *testing.T) *Engine {
	t.Helper()
	e := flowWorld(t)
	e.subject = Subject{EntityRef: "otel.service.name=checkout"}
	e.digests = map[string]*investigationv1.Digest{}
	if err := e.ledger.AddEvidence(ledger.EvidenceItem{
		ID: "e-1", Kind: "algebra_answer", Worker: "metrics", Capability: "compare", Outcome: "ok",
		Coverage: &investigationv1.Coverage{}, DeepLinkAbsentReason: "recorded",
		Term: &investigationv1.AlgebraTerm{Term: &investigationv1.AlgebraTerm_Compare{Compare: &investigationv1.CompareTerm{
			Pointer: &graphv1.Pointer{Kind: graphv1.PointerKind_METRIC, Selector: "payments-rate"},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	e.digests["e-1"] = &investigationv1.Digest{Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
		Comparisons: []*investigationv1.Comparison{{Direction: "up", Separable: true}},
	}}}
	return e
}

// TestAPathIsDrawnOnTheGraphAndCitesWhatMoved is FR-019b: a change on ledger reaches checkout
// through payments, against the direction of the calls; the edge into payments cites the
// comparison that moved there, and the edges into ledger and checkout, which nothing showed moving,
// are unverified.
func TestAPathIsDrawnOnTheGraphAndCitesWhatMoved(t *testing.T) {
	t.Parallel()
	e := pathWorld(t)
	p := e.pathOf(ledger.Hypothesis{ID: "h-1", Kind: ledger.KindChange, CandidateChangeEntityID: "c-ledger",
		TargetEntityIDs: []string{"e-ledger"}})
	if !p.Drawable {
		t.Fatalf("path not drawn: %+v", p)
	}
	got := p.Render()
	want := "path: c-ledger -changed_by-> otel.service.name=ledger (unverified); " +
		"otel.service.name=ledger -calls-> otel.service.name=payments (e-1); " +
		"otel.service.name=payments -calls-> otel.service.name=checkout (unverified)"
	if got != want {
		t.Errorf("path =\n  %s\nwant\n  %s", got, want)
	}
	if !p.Edges[1].AgainstEdge {
		t.Error("a callee's fault reaching its caller travels against the calls edge")
	}
}

// TestAPathTheGraphCannotDrawNamesItsMissingLink: a target no edge links to the subject is
// undrawable, with a resolving action that asks the graph one hop further.
func TestAPathTheGraphCannotDrawNamesItsMissingLink(t *testing.T) {
	t.Parallel()
	e := pathWorld(t)
	p := e.pathOf(ledger.Hypothesis{ID: "h-2", Kind: ledger.KindCondition, TargetEntityIDs: []string{"e-config"}})
	if p.Drawable || p.Missing == nil {
		t.Fatalf("path = %+v, want undrawable with a missing link", p)
	}
	if p.Missing.GetKind() != "connect_source" || !strings.Contains(p.Missing.GetStatement(), "otel.service.name=config") {
		t.Errorf("missing link = %v", p.Missing)
	}
	if p.Missing.GetNextQuery().GetGraph() == nil {
		t.Errorf("the resolving action is not a subgraph query: %v", p.Missing.GetNextQuery())
	}
	if got := PathsProto([]Path{p}).GetPaths()[0]; got.GetDrawable() || got.GetMissingLink() == nil {
		t.Errorf("proto = %v", got)
	}
}

// TestACompletionReadDrawsAPathTheNeighbourhoodCouldNot is the stop's path completion (FR-019b):
// the neighbourhood read holds config with no edge to it, the read one hop wider holds the edge
// ledger -depends_on-> config, and the path drawn over both reaches the subject through it.
func TestACompletionReadDrawsAPathTheNeighbourhoodCouldNot(t *testing.T) {
	t.Parallel()
	e := pathWorld(t)
	h := ledger.Hypothesis{ID: "h-2", Kind: ledger.KindCondition, TargetEntityIDs: []string{"e-config"}}
	if p := e.pathOf(h); p.Drawable || p.missingFrom != "otel.service.name=config" {
		t.Fatalf("before completion: path = %+v, want undrawable from config", p)
	}
	node := func(id, name string) *graphv1.NodeVersion {
		return &graphv1.NodeVersion{EntityId: id, Type: graphv1.NodeType_SERVICE,
			Aliases: []*graphv1.Ref{{Namespace: "otel.service.name", Value: name}}}
	}
	now := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	wider := BuildProfile(&graphv1.SubgraphResponse{
		Focus: node("e-config", "config"),
		Nodes: []*graphv1.NodeVersion{node("e-ledger", "ledger"), node("e-vault", "vault")},
		Edges: []*graphv1.EdgeVersion{
			{SrcId: "e-ledger", DstId: "e-config", Type: graphv1.EdgeType_DEPENDS_ON},
			{SrcId: "e-config", DstId: "e-vault", Type: graphv1.EdgeType_DEPENDS_ON},
		},
	}, now.Add(-time.Hour), now, now)
	p := e.pathOf(h, wider)
	if !p.Drawable {
		t.Fatalf("after completion: path not drawn: %+v", p)
	}
	got := p.Render()
	want := "path: otel.service.name=config -depends_on-> otel.service.name=ledger (unverified); " +
		"otel.service.name=ledger -calls-> otel.service.name=payments (e-1); " +
		"otel.service.name=payments -calls-> otel.service.name=checkout (unverified)"
	if got != want {
		t.Errorf("path =\n  %s\nwant\n  %s", got, want)
	}
}
