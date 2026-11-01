// SPDX-License-Identifier: Apache-2.0

// Package knowledgedoc is the vocabulary of a KNOWLEDGE_DOC in the graph: the published document
// kinds, and the link request that registers a document as a node with its CONCERNS edges.
//
// It belongs to the knowledge context, not to the investigation worker that reads documents back:
// the Notion feeder and `aisre knowledge link` write documents with it, and the knowledge worker
// reads them. Keeping it here lets the ingestion side register a document without importing an
// investigation worker.
package knowledgedoc

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The published document kinds. The set is closed so that a fixture can assert on it and a
// `knowledge link` invocation cannot invent a category.
const (
	// KindPostmortem is a written incident retrospective.
	KindPostmortem = "postmortem"
	// KindRunbook is an operational procedure.
	KindRunbook = "runbook"
	// KindDecision is an architecture or operations decision record.
	KindDecision = "decision"
	// KindInvestigation is one of this engine's own concluded investigations, which is half of
	// what the v1 corpus is made of (plan F5).
	KindInvestigation = "investigation"
)

// Kinds is the published set, sorted.
func Kinds() []string {
	return []string{KindDecision, KindInvestigation, KindPostmortem, KindRunbook}
}

// ValidKind reports whether kind is published.
func ValidKind(kind string) bool {
	for _, candidate := range Kinds() {
		if candidate == kind {
			return true
		}
	}
	return false
}

// ---- knowledge link --------------------------------------------------------------------------

// LinkRequest registers a durable document a human wrote. Everything in it is supplied by that
// human; nothing is inferred, and the document's content is not among the fields (FR-049a,
// FR-052).
type LinkRequest struct {
	// DocumentRef identifies the document in its own system, e.g. `notion.page=abc123`.
	DocumentRef *graphv1.Ref
	// Kind is one of the published kinds.
	Kind string
	// Title is the human title.
	Title string
	// Summary is the one-line description the registering person wrote. It is what BM25 ranks
	// and what a digest quotes, and it is theirs rather than the document's.
	Summary string
	// Location is where the document lives.
	Location string
	// EntityRefs are the entities it concerns, one CONCERNS edge each.
	EntityRefs []*graphv1.Ref
	// RegisteredBy is the authenticated identity registering it. A knowledge link with no
	// principal is refused: a citation whose provenance is anonymous is an appeal to authority.
	RegisteredBy string
	// AsOf is the instant the link becomes valid. Zero means the registration instant.
	AsOf time.Time
	// AuthoredAt is when the document was written, when the person says.
	AuthoredAt time.Time
	// ConfiguredBy is the principal who signed a connector's configuration, when a connector
	// registers the document rather than a person (007 ADR-0013 item 2). Empty for `knowledge link`.
	ConfiguredBy string
	// SummaryTruncated says the summary was cut to its cap. A connector sets it; a person's summary
	// is theirs and is never cut.
	SummaryTruncated bool
	// Provenance replaces the default `registered by <RegisteredBy>` link provenance, for a connector
	// whose link was made by a rule an operator configured (`notion/notion-db@1 configured by …`).
	Provenance string
	// Unlinked registers a document that names no entity, for the service catalog to attribute from
	// the names it carries (009 FR-006, research R3). Only a connector that records those names on
	// the node sets it; a person linking a document names what it concerns.
	Unlinked bool
}

// Validate refuses a registration that would produce an uncitable document.
func (r LinkRequest) Validate() error {
	if r.DocumentRef.GetNamespace() == "" || r.DocumentRef.GetValue() == "" {
		return fmt.Errorf("knowledge link needs a document reference in <namespace>=<value> form")
	}
	if !ValidKind(r.Kind) {
		return fmt.Errorf("knowledge link kind %q is not published; the set is %v", r.Kind, Kinds())
	}
	if len(r.EntityRefs) == 0 && !r.Unlinked {
		return fmt.Errorf(
			"knowledge link names no entities; retrieval is scoped by the graph, so a document linked to nothing is a document that can never be retrieved (FR-049)")
	}
	if r.RegisteredBy == "" {
		return fmt.Errorf(
			"knowledge link names no registering identity; the registering identity, the instant and each link's provenance are recorded (FR-049a)")
	}
	if r.Title == "" {
		return fmt.Errorf("knowledge link needs a title; it is half of what the ranking has to work with")
	}
	return nil
}

// Events renders a link request as the graph events that register it: one `upsert_node` for the
// `KNOWLEDGE_DOC` and one `upsert_edge` `CONCERNS` per entity.
//
// No new event type was needed for this, which is the right outcome: registering a document is
// observing a node and some edges, and inventing a bespoke event for it would have added a
// projector path and a migration for something the graph already expresses.
func (r LinkRequest) Events(sourceID string, at time.Time) ([]*graphv1.EventEnvelope, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	validAt := r.AsOf
	if validAt.IsZero() {
		validAt = at
	}
	validAt = validAt.UTC().Truncate(time.Second)

	props, err := structpb.NewStruct(r.Props(at))
	if err != nil {
		return nil, fmt.Errorf("knowledge link: properties: %w", err)
	}

	events := []*graphv1.EventEnvelope{{
		EventId:       eventID(sourceID, r.DocumentRef, "node", validAt),
		SourceId:      sourceID,
		SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         r.DocumentRef,
			Type:        graphv1.NodeType_KNOWLEDGE_DOC,
			DisplayName: r.Title,
			Props:       props,
			ValidAt:     timestamppb.New(validAt),
		}},
	}}
	for _, entity := range r.EntityRefs {
		events = append(events, &graphv1.EventEnvelope{
			EventId:       eventID(sourceID, r.DocumentRef, "edge:"+entity.GetNamespace()+"="+entity.GetValue(), validAt),
			SourceId:      sourceID,
			SchemaVersion: "1.0.0",
			Body: &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
				Src:     r.DocumentRef,
				Dst:     entity,
				Type:    graphv1.EdgeType_CONCERNS,
				Props:   props,
				ValidAt: timestamppb.New(validAt),
			}},
		})
	}
	return events, nil
}

// Props are the properties a registration writes on the `KNOWLEDGE_DOC` node, registered at `at`. The
// corpus reads `kind`, `summary`, `location`, `authored_at` and `link_provenance` back (graph.go). A
// connector's own additions appear only when set, so `knowledge link` writes what it always wrote.
func (r LinkRequest) Props(at time.Time) map[string]any {
	provenance := r.Provenance
	if provenance == "" {
		provenance = "registered by " + r.RegisteredBy
	}
	props := map[string]any{
		"kind":            r.Kind,
		"summary":         r.Summary,
		"location":        r.Location,
		"registered_by":   r.RegisteredBy,
		"registered_at":   at.UTC().Format(time.RFC3339),
		"authored_at":     authoredAt(r.AuthoredAt),
		"link_provenance": provenance,
	}
	if r.ConfiguredBy != "" {
		props["configured_by"] = r.ConfiguredBy
	}
	if r.SummaryTruncated {
		props["summary_truncated"] = true
	}
	return props
}

func authoredAt(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// eventID is deterministic, so registering the same document twice is a duplicate no-op rather
// than a second node.
func eventID(sourceID string, ref *graphv1.Ref, part string, at time.Time) string {
	return fmt.Sprintf("%s:knowledge:%s=%s:%s:%d", sourceID, ref.GetNamespace(), ref.GetValue(), part, at.Unix())
}
