// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// NodeType is the Go spelling of the published sreagent.graph.v1.NodeType enum.
//
// The string form is the mechanical lowercase of the protobuf enum value name
// (`INFRA_RESOURCE` -> `infra_resource`); the unspecified value is the empty string. These
// strings are what the store writes to the `type` text columns (data-model.md) and what the
// CLI prints, so they are part of the published contract: changing one is a breaking change.
type NodeType string

// Node types. Constitution "Definitions" requires at least these; `change` is first class.
const (
	NodeTypeUnspecified   NodeType = ""
	NodeTypeService       NodeType = "service"
	NodeTypeWorkload      NodeType = "workload"
	NodeTypeInfraResource NodeType = "infra_resource"
	NodeTypeConfig        NodeType = "config"
	NodeTypeFeatureFlag   NodeType = "feature_flag"
	NodeTypeDBSchema      NodeType = "db_schema"
	NodeTypeThirdParty    NodeType = "third_party"
	NodeTypeOwner         NodeType = "owner"
	NodeTypeAlert         NodeType = "alert"
	NodeTypeChange        NodeType = "change"
	// Added by the feature 001 change package for 002 (ADR-0005 D3). An investigation is a
	// decision record the graph holds like any other fact; a knowledge document is what
	// graph-scoped retrieval indexes. Neither carries telemetry.
	NodeTypeInvestigation NodeType = "investigation"
	NodeTypeKnowledgeDoc  NodeType = "knowledge_doc"
)

var (
	nodeTypeToProto = map[NodeType]graphv1.NodeType{
		NodeTypeUnspecified:   graphv1.NodeType_NODE_TYPE_UNSPECIFIED,
		NodeTypeService:       graphv1.NodeType_SERVICE,
		NodeTypeWorkload:      graphv1.NodeType_WORKLOAD,
		NodeTypeInfraResource: graphv1.NodeType_INFRA_RESOURCE,
		NodeTypeConfig:        graphv1.NodeType_CONFIG,
		NodeTypeFeatureFlag:   graphv1.NodeType_FEATURE_FLAG,
		NodeTypeDBSchema:      graphv1.NodeType_DB_SCHEMA,
		NodeTypeThirdParty:    graphv1.NodeType_THIRD_PARTY,
		NodeTypeOwner:         graphv1.NodeType_OWNER,
		NodeTypeAlert:         graphv1.NodeType_ALERT,
		NodeTypeChange:        graphv1.NodeType_CHANGE,
		NodeTypeInvestigation: graphv1.NodeType_INVESTIGATION,
		NodeTypeKnowledgeDoc:  graphv1.NodeType_KNOWLEDGE_DOC,
	}
	protoToNodeType = invertNodeTypes(nodeTypeToProto)
)

func invertNodeTypes(m map[NodeType]graphv1.NodeType) map[graphv1.NodeType]NodeType {
	out := make(map[graphv1.NodeType]NodeType, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// Proto returns the protobuf enum value, or NODE_TYPE_UNSPECIFIED for an unknown string.
func (t NodeType) Proto() graphv1.NodeType { return nodeTypeToProto[t] }

// String implements fmt.Stringer.
func (t NodeType) String() string { return string(t) }

// Valid reports whether t is a known node type other than the unspecified one.
func (t NodeType) Valid() bool {
	_, ok := nodeTypeToProto[t]
	return ok && t != NodeTypeUnspecified
}

// NodeTypeFromProto converts a protobuf enum value to its Go spelling. Unknown values (a newer
// schema seen by an older binary) yield NodeTypeUnspecified.
func NodeTypeFromProto(p graphv1.NodeType) NodeType { return protoToNodeType[p] }

// ParseNodeType converts the canonical string form back to a NodeType.
func ParseNodeType(s string) (NodeType, error) {
	t := NodeType(s)
	if !t.Valid() {
		return NodeTypeUnspecified, fmt.Errorf("graph: unknown node type %q", s)
	}
	return t, nil
}

// EdgeType is the Go spelling of the published sreagent.graph.v1.EdgeType enum. The string
// form follows the same mechanical rule as NodeType and is equally part of the contract.
type EdgeType string

// Edge types. Constitution "Definitions" requires at least these.
const (
	EdgeTypeUnspecified EdgeType = ""
	EdgeTypeCalls       EdgeType = "calls"
	EdgeTypeDependsOn   EdgeType = "depends_on"
	EdgeTypeRunsOn      EdgeType = "runs_on"
	EdgeTypeDeployedBy  EdgeType = "deployed_by"
	EdgeTypeOwnedBy     EdgeType = "owned_by"
	EdgeTypeExposedVia  EdgeType = "exposed_via"
	EdgeTypeChangedBy   EdgeType = "changed_by"
	// Added by the feature 001 change package for 002 (ADR-0005 D3).
	EdgeTypeWatches      EdgeType = "watches"      // alert -> the entity it watches
	EdgeTypeInvestigated EdgeType = "investigated" // investigation -> a subject or target
	EdgeTypeConcerns     EdgeType = "concerns"     // knowledge document -> entity
)

var (
	edgeTypeToProto = map[EdgeType]graphv1.EdgeType{
		EdgeTypeUnspecified:  graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED,
		EdgeTypeCalls:        graphv1.EdgeType_CALLS,
		EdgeTypeDependsOn:    graphv1.EdgeType_DEPENDS_ON,
		EdgeTypeRunsOn:       graphv1.EdgeType_RUNS_ON,
		EdgeTypeDeployedBy:   graphv1.EdgeType_DEPLOYED_BY,
		EdgeTypeOwnedBy:      graphv1.EdgeType_OWNED_BY,
		EdgeTypeExposedVia:   graphv1.EdgeType_EXPOSED_VIA,
		EdgeTypeChangedBy:    graphv1.EdgeType_CHANGED_BY,
		EdgeTypeWatches:      graphv1.EdgeType_WATCHES,
		EdgeTypeInvestigated: graphv1.EdgeType_INVESTIGATED,
		EdgeTypeConcerns:     graphv1.EdgeType_CONCERNS,
	}
	protoToEdgeType = invertEdgeTypes(edgeTypeToProto)
)

func invertEdgeTypes(m map[EdgeType]graphv1.EdgeType) map[graphv1.EdgeType]EdgeType {
	out := make(map[graphv1.EdgeType]EdgeType, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// Proto returns the protobuf enum value, or EDGE_TYPE_UNSPECIFIED for an unknown string.
func (t EdgeType) Proto() graphv1.EdgeType { return edgeTypeToProto[t] }

// String implements fmt.Stringer.
func (t EdgeType) String() string { return string(t) }

// Valid reports whether t is a known edge type other than the unspecified one.
func (t EdgeType) Valid() bool {
	_, ok := edgeTypeToProto[t]
	return ok && t != EdgeTypeUnspecified
}

// EdgeTypeFromProto converts a protobuf enum value to its Go spelling. Unknown values yield
// EdgeTypeUnspecified.
func EdgeTypeFromProto(p graphv1.EdgeType) EdgeType { return protoToEdgeType[p] }

// ParseEdgeType converts the canonical string form back to an EdgeType.
func ParseEdgeType(s string) (EdgeType, error) {
	t := EdgeType(s)
	if !t.Valid() {
		return EdgeTypeUnspecified, fmt.Errorf("graph: unknown edge type %q", s)
	}
	return t, nil
}

// Ref identifies an entity by an external namespace and value, for example
// ("otel.service.name", "checkout"). Feeders address entities this way so they never need to
// know canonical ids (data-model.md, "Refs use {namespace, value}").
type Ref struct {
	Namespace string
	Value     string
}

// String renders the ref as `namespace=value`, the spelling used by fixture manifests and the
// CLI. ParseRef is its inverse.
func (r Ref) String() string { return r.Namespace + "=" + r.Value }

// IsZero reports whether both halves of the ref are empty.
func (r Ref) IsZero() bool { return r.Namespace == "" && r.Value == "" }

// EntityID returns the deterministic canonical id this ref hashes to (see ids.go).
func (r Ref) EntityID() string { return EntityID(r.Namespace, r.Value) }

// Proto converts the ref to its protobuf form.
func (r Ref) Proto() *graphv1.Ref {
	return &graphv1.Ref{Namespace: r.Namespace, Value: r.Value}
}

// RefFromProto converts a protobuf ref; a nil message yields the zero Ref.
func RefFromProto(p *graphv1.Ref) Ref {
	return Ref{Namespace: p.GetNamespace(), Value: p.GetValue()}
}

// ParseRef parses the `namespace=value` spelling. The first `=` separates the two halves, so a
// value may itself contain `=`.
func ParseRef(s string) (Ref, error) {
	ns, value, ok := strings.Cut(s, "=")
	if !ok || ns == "" || value == "" {
		return Ref{}, fmt.Errorf("graph: malformed ref %q, want namespace=value", s)
	}
	return Ref{Namespace: ns, Value: value}, nil
}

// Pointer is the published Pointer message, aliased rather than redefined.
//
// A pointer says *where to look* for telemetry in the backend that owns it; it never carries
// telemetry (constitution IV). Its shape — kind, backend, vocabulary, selector, attributes —
// is fixed by the schema, and every producer and consumer of pointers exchanges the protobuf
// message, so a parallel Go struct would only add a conversion that can drift.
type Pointer = graphv1.Pointer
