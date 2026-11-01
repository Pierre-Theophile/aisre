// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
)

func profileNode(id, service string, typ graphv1.NodeType, kinds ...graphv1.PointerKind) *graphv1.NodeVersion {
	n := &graphv1.NodeVersion{EntityId: id, Type: typ,
		Aliases: []*graphv1.Ref{{Namespace: "otel.service.name", Value: service}}}
	for _, k := range kinds {
		n.Pointers = append(n.Pointers, &graphv1.Pointer{Kind: k, BackendKind: "prometheus"})
	}
	return n
}

// TestTheProfileIsBuiltFromTheNeighbourhoodRead is FR-012a on a hand-built answer: what is listed,
// what is left out, and what is named as a blind spot.
func TestTheProfileIsBuiltFromTheNeighbourhoodRead(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	weight := uint32(3)
	sub := &graphv1.SubgraphResponse{
		Focus: profileNode("e-checkout", "checkout", graphv1.NodeType_SERVICE, graphv1.PointerKind_METRIC),
		Nodes: []*graphv1.NodeVersion{
			profileNode("e-payments", "payments", graphv1.NodeType_SERVICE, graphv1.PointerKind_TRACE),
			profileNode("e-db", "db", graphv1.NodeType_THIRD_PARTY),
			profileNode("e-rollout", "rollout", graphv1.NodeType_CHANGE),
			profileNode("e-doc", "runbook", graphv1.NodeType_KNOWLEDGE_DOC),
		},
		Edges: []*graphv1.EdgeVersion{
			{SrcId: "e-checkout", DstId: "e-payments", Type: graphv1.EdgeType_CALLS, WeightClass: &weight,
				Provenance: &graphv1.Provenance{SourceId: "otel:demo"}},
			{SrcId: "e-payments", DstId: "e-db", Type: graphv1.EdgeType_DEPENDS_ON},
			{SrcId: "e-rollout", DstId: "e-payments", Type: graphv1.EdgeType_CHANGED_BY},
			{SrcId: "e-doc", DstId: "e-payments", Type: graphv1.EdgeType_CONCERNS},
		},
		Extent: &graphv1.Extent{Sources: []*graphv1.SourceExtent{
			{SourceId: "k8s:demo", LastCheckpoint: timestamppb.New(end.Add(-20 * time.Minute)),
				Gaps: []*graphv1.Interval{{Start: timestamppb.New(start.Add(10 * time.Minute)),
					End: timestamppb.New(start.Add(20 * time.Minute))}}},
			{SourceId: "otel:demo", LastCheckpoint: timestamppb.New(end.Add(-time.Minute)),
				Gaps: []*graphv1.Interval{{Start: timestamppb.New(start.Add(-3 * time.Hour)),
					End: timestamppb.New(start.Add(-2 * time.Hour))}}},
		}},
	}

	p := engine.BuildProfile(sub, start, end, end)
	if len(p.Entities) != 3 || p.Entities[0].Ref != "otel.service.name=checkout" {
		t.Fatalf("entities = %+v, want the subject first and no change or document", p.Entities)
	}
	var payments engine.ProfileEntity
	for _, ent := range p.Entities {
		if ent.ID == "e-payments" {
			payments = ent
		}
	}
	if !payments.Has(engine.FamilyTraces) || !payments.Has(engine.FamilyKnowledge) || payments.Has(engine.FamilyMetrics) {
		t.Errorf("payments families = %+v, want traces and knowledge and no metrics", payments.Families)
	}
	if len(p.Edges) != 2 {
		t.Errorf("edges = %+v, want the two between entities in scope", p.Edges)
	}

	text := p.Render()
	for _, want := range []string{
		"otel.service.name=db has no metric, log or trace pointer",
		"source k8s:demo has a gap from 2026-09-01T13:10:00Z",
		"source k8s:demo last checkpointed at 2026-09-01T14:10:00Z, 20m0s before",
		"otel.service.name=checkout -calls-> otel.service.name=payments  weight_class=3  source=otel:demo",
		"source=(none recorded)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("profile does not carry %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "otel:demo has a gap") || strings.Contains(text, "otel:demo last checkpointed") {
		t.Errorf("a gap outside the window or a fresh source was named a blind spot:\n%s", text)
	}
	if again := engine.BuildProfile(sub, start, end, end).Render(); again != text {
		t.Error("the rendering is not deterministic")
	}
}

// TestALargeProfileIsCappedAndSaysSo: the cap truncates the listing, the truncation is stated, and
// the blind spots still cover every entity in scope.
func TestALargeProfileIsCappedAndSaysSo(t *testing.T) {
	t.Parallel()

	sub := &graphv1.SubgraphResponse{Focus: profileNode("e-000", "svc-000", graphv1.NodeType_SERVICE, graphv1.PointerKind_METRIC)}
	for i := 1; i <= engine.MaxProfileEntities+5; i++ {
		sub.Nodes = append(sub.Nodes, profileNode(fmt.Sprintf("e-%03d", i), fmt.Sprintf("svc-%03d", i), graphv1.NodeType_SERVICE))
	}
	sub.Truncation = &graphv1.Truncation{Truncated: true, Reason: "total cap", TotalCap: 50}
	now := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)
	p := engine.BuildProfile(sub, now.Add(-time.Hour), now, now)

	if len(p.Entities) != engine.MaxProfileEntities || p.TotalEntities != engine.MaxProfileEntities+6 {
		t.Errorf("listed %d of %d", len(p.Entities), p.TotalEntities)
	}
	text := p.Render()
	if !strings.Contains(text, fmt.Sprintf("truncated: entities: %d of %d listed", engine.MaxProfileEntities, engine.MaxProfileEntities+6)) {
		t.Errorf("the cap is not stated:\n%s", text)
	}
	if !strings.Contains(text, "truncated: the graph truncated the neighbourhood (total cap") {
		t.Errorf("the graph's own truncation is not stated:\n%s", text)
	}
	if !strings.Contains(text, "otel.service.name=svc-045 has no metric") {
		t.Error("an entity past the cap lost its blind spot")
	}
}

// TestNoNeighbourhoodIsABlindSpot: a failed read is said, not rendered as an empty scope.
func TestNoNeighbourhoodIsABlindSpot(t *testing.T) {
	t.Parallel()
	now := time.Now()
	if text := engine.BuildProfile(nil, now, now, now).Render(); !strings.Contains(text, "the graph returned no neighbourhood") {
		t.Errorf("profile = %q", text)
	}
}
