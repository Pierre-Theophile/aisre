// SPDX-License-Identifier: Apache-2.0

package graph_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Every value of the published enum must have a Go spelling, and that spelling must be the
// mechanical lowercase of the protobuf enum value name. This fails the moment the proto gains
// a node type that types.go does not know about, which is the point.
func TestNodeTypeCoversProtoEnum(t *testing.T) {
	t.Parallel()

	for number, name := range graphv1.NodeType_name {
		p := graphv1.NodeType(number)
		got := graph.NodeTypeFromProto(p)

		want := strings.ToLower(name)
		if name == "NODE_TYPE_UNSPECIFIED" {
			want = ""
		}
		if string(got) != want {
			t.Errorf("NodeTypeFromProto(%s) = %q, want %q", name, got, want)
		}
		if back := got.Proto(); back != p {
			t.Errorf("round trip of %s gave %s", name, back)
		}
		if wantValid := name != "NODE_TYPE_UNSPECIFIED"; got.Valid() != wantValid {
			t.Errorf("%q.Valid() = %v, want %v", got, got.Valid(), wantValid)
		}
	}
}

func TestEdgeTypeCoversProtoEnum(t *testing.T) {
	t.Parallel()

	for number, name := range graphv1.EdgeType_name {
		p := graphv1.EdgeType(number)
		got := graph.EdgeTypeFromProto(p)

		want := strings.ToLower(name)
		if name == "EDGE_TYPE_UNSPECIFIED" {
			want = ""
		}
		if string(got) != want {
			t.Errorf("EdgeTypeFromProto(%s) = %q, want %q", name, got, want)
		}
		if back := got.Proto(); back != p {
			t.Errorf("round trip of %s gave %s", name, back)
		}
	}
}

// The constitution's "Definitions" section names the minimum taxonomy; these constants are
// what the store writes and the CLI prints, so they are pinned here too.
func TestTypeConstants(t *testing.T) {
	t.Parallel()

	nodes := map[graph.NodeType]graphv1.NodeType{
		graph.NodeTypeService:       graphv1.NodeType_SERVICE,
		graph.NodeTypeWorkload:      graphv1.NodeType_WORKLOAD,
		graph.NodeTypeInfraResource: graphv1.NodeType_INFRA_RESOURCE,
		graph.NodeTypeConfig:        graphv1.NodeType_CONFIG,
		graph.NodeTypeFeatureFlag:   graphv1.NodeType_FEATURE_FLAG,
		graph.NodeTypeDBSchema:      graphv1.NodeType_DB_SCHEMA,
		graph.NodeTypeThirdParty:    graphv1.NodeType_THIRD_PARTY,
		graph.NodeTypeOwner:         graphv1.NodeType_OWNER,
		graph.NodeTypeAlert:         graphv1.NodeType_ALERT,
		graph.NodeTypeChange:        graphv1.NodeType_CHANGE,
	}
	for domain, p := range nodes {
		if domain.Proto() != p {
			t.Errorf("%q maps to %s, want %s", domain, domain.Proto(), p)
		}
	}
	if graph.NodeTypeInfraResource != "infra_resource" || graph.EdgeTypeChangedBy != "changed_by" {
		t.Error("the string spelling of a type changed; that is a breaking schema change")
	}

	edges := map[graph.EdgeType]graphv1.EdgeType{
		graph.EdgeTypeCalls:      graphv1.EdgeType_CALLS,
		graph.EdgeTypeDependsOn:  graphv1.EdgeType_DEPENDS_ON,
		graph.EdgeTypeRunsOn:     graphv1.EdgeType_RUNS_ON,
		graph.EdgeTypeDeployedBy: graphv1.EdgeType_DEPLOYED_BY,
		graph.EdgeTypeOwnedBy:    graphv1.EdgeType_OWNED_BY,
		graph.EdgeTypeExposedVia: graphv1.EdgeType_EXPOSED_VIA,
		graph.EdgeTypeChangedBy:  graphv1.EdgeType_CHANGED_BY,
	}
	for domain, p := range edges {
		if domain.Proto() != p {
			t.Errorf("%q maps to %s, want %s", domain, domain.Proto(), p)
		}
	}
}

func TestParseTypes(t *testing.T) {
	t.Parallel()

	if got, err := graph.ParseNodeType("change"); err != nil || got != graph.NodeTypeChange {
		t.Errorf("ParseNodeType(change) = %q, %v", got, err)
	}
	if _, err := graph.ParseNodeType("CHANGE"); err == nil {
		t.Error("ParseNodeType must be case sensitive")
	}
	if _, err := graph.ParseNodeType(""); err == nil {
		t.Error("the unspecified type must not parse")
	}
	if got, err := graph.ParseEdgeType("depends_on"); err != nil || got != graph.EdgeTypeDependsOn {
		t.Errorf("ParseEdgeType(depends_on) = %q, %v", got, err)
	}
	if _, err := graph.ParseEdgeType("depends-on"); err == nil {
		t.Error("the hyphenated spelling must not parse")
	}
	// An unknown value from a newer schema degrades to unspecified rather than panicking.
	if got := graph.NodeTypeFromProto(graphv1.NodeType(9999)); got != graph.NodeTypeUnspecified {
		t.Errorf("unknown proto enum gave %q", got)
	}
}

func TestRef(t *testing.T) {
	t.Parallel()

	ref := graph.Ref{Namespace: "otel.service.name", Value: "checkout"}
	if got := ref.String(); got != "otel.service.name=checkout" {
		t.Errorf("String() = %q", got)
	}
	if p := ref.Proto(); p.GetNamespace() != ref.Namespace || p.GetValue() != ref.Value {
		t.Errorf("Proto() = %v", p)
	}
	if back := graph.RefFromProto(ref.Proto()); back != ref {
		t.Errorf("round trip gave %v", back)
	}
	if got := graph.RefFromProto(nil); !got.IsZero() {
		t.Errorf("RefFromProto(nil) = %v, want the zero ref", got)
	}

	parsed, err := graph.ParseRef("k8s.deployment=demo/checkout-svc")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if parsed != (graph.Ref{Namespace: "k8s.deployment", Value: "demo/checkout-svc"}) {
		t.Errorf("ParseRef gave %v", parsed)
	}
	// Only the first `=` separates, so values may contain one.
	withEquals, err := graph.ParseRef("label=app=checkout")
	if err != nil || withEquals.Value != "app=checkout" {
		t.Errorf("ParseRef(label=app=checkout) = %v, %v", withEquals, err)
	}
	for _, bad := range []string{"", "checkout", "=checkout", "ns="} {
		if _, err := graph.ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) must fail", bad)
		}
	}
	// Round trip through the string form, which is what fixtures and the CLI exchange.
	if again, err := graph.ParseRef(ref.String()); err != nil || again != ref {
		t.Errorf("String/ParseRef round trip gave %v, %v", again, err)
	}
}
