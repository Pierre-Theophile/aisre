// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The knowledge worker (tasks.md T049; FR-049, FR-049a, FR-050, FR-051, FR-052).
//
// Retrieval is **scoped by the graph first and ranked second**. The candidate set is the
// documents linked by a `CONCERNS` edge to a node in the investigation's subgraph or to one of
// its candidate changes — nothing else is a candidate, ever. Only then is that set ranked, by
// BM25 in bm25.go. Inverting the two, as an embedding-first design would, retrieves documents
// linked to nothing, which FR-049 forbids and which is exactly how a retrieval system starts
// citing a postmortem about a different service with a similar name.
//
// Three rules the rest of the file enforces:
//
//   - **Retrieval over telemetry is refused.** The knowledge corpus is durable documents people
//     wrote and past investigations; a log line is not knowledge, and a worker that would search
//     both would have two sources of truth (FR-050).
//   - **Every item is cited** with its identifier, the entities it is linked to, how the link was
//     made, and its age. A citation without provenance is an appeal to authority.
//   - **A hypothesis resting only on a document is knowledge-derived and unconfirmed, and may not
//     reach `supported`** (FR-051). The worker marks its digest so the ledger can enforce that;
//     it is stated here because the marking is meaningless if the reason is not.
//
// `knowledge link` registers a human-written document as a `KNOWLEDGE_DOC` node with a `CONCERNS`
// edge per entity and a pointer to where it lives — **never its content** (FR-049a, FR-052).

// Name is the worker's published name.
const Name = "knowledge"

// Version is the worker's own version, recorded with every answer.
const Version = "0.1.0"

// SourceOfTruth is the one system this worker speaks for: the graph's knowledge layer.
const SourceOfTruth = "graph:knowledge"

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

// Document is one candidate from the scoped set. It carries what the graph knows about the
// document — never the document itself.
type Document struct {
	// ID is the document's identifier, which is also its citation key.
	ID string
	// Kind is one of the published kinds.
	Kind string
	// Title is the human title, registered with the node.
	Title string
	// Summary is the one-line description supplied when the document was registered. It is
	// **not** the document's content: the whole point of FR-052 is that the graph holds a
	// pointer to where a document lives, so what is ranked here is what a human wrote *about*
	// the document, plus its title.
	Summary string
	// LinkedEntityIDs are the entities it CONCERNS.
	LinkedEntityIDs []string
	// LinkProvenance says how the link was made: the rule or the person that made it.
	LinkProvenance string
	// AuthoredAt is when it was written.
	AuthoredAt time.Time
	// AgeDays is its age at the investigation's reference instant.
	AgeDays int64
	// HopsFromFocus is how far its nearest linked entity is from the investigation's focus.
	HopsFromFocus int
	// Location is where the document lives — a URL or a reference — so a person can open it.
	Location string
}

// SearchableText is what BM25 ranks: the title and the registered summary. Nothing else is
// available, and that is the design rather than a limitation.
func (d Document) SearchableText() string {
	return strings.TrimSpace(d.Title + " " + d.Summary)
}

// Corpus is the scoped set the worker ranks. Implementations resolve `CONCERNS` edges against
// the graph; the interface exists so the worker can be tested without a database and so a later
// feature can widen the corpus without touching the ranking.
type Corpus interface {
	// Scoped returns the documents linked by CONCERNS to any of entityIDs. It returns only
	// linked documents: an implementation that returned anything else would defeat the scoping
	// the constitution requires.
	Scoped(ctx context.Context, entityIDs []string) ([]Document, error)
}

// Worker is the knowledge worker.
type Worker struct {
	corpus Corpus
}

var _ worker.Worker = (*Worker)(nil)

// New returns the knowledge worker over a corpus.
func New(corpus Corpus) *Worker { return &Worker{corpus: corpus} }

// Describe returns the declaration. No model in v1: BM25 over tens of documents is deterministic
// and sufficient, and model re-ranking is a one-line escalation once recall@10 is measured
// (research §7).
func (w *Worker) Describe() worker.Description {
	return worker.Description{
		Name:          Name,
		SourceOfTruth: SourceOfTruth,
		Capabilities: []worker.Capability{
			{Name: engine.TermKnowledgeSearch, ReadOnly: true, CostClass: worker.CostClassCheap},
		},
		ContainsModel: false,
		Redaction:     workers.Redaction(),
		Modes:         workers.BothModes(),
		Version:       Version,
	}
}

// Call answers `knowledge_search`.
func (w *Worker) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	if req.Capability != engine.TermKnowledgeSearch {
		if engine.FamilyOf(req.Capability) == "telemetry" {
			return worker.Response{}, worker.Reject(worker.ReasonOutsideAlgebra,
				"worker %s was asked for %s; retrieval over telemetry is prohibited — a log line is not knowledge, and a worker that searched both would have two sources of truth (FR-050)",
				Name, req.Capability)
		}
		return worker.Response{}, worker.Reject(worker.ReasonUndeclaredCapability,
			"worker %s did not declare capability %s; it declares %s", Name, req.Capability, engine.TermKnowledgeSearch)
	}
	term := req.Algebra.GetTerm().GetKnowledgeSearch()
	if term == nil {
		return worker.Response{}, engine.OutsideAlgebra(engine.TermName(req.Algebra.GetTerm()))
	}
	if err := engine.Validate(req.Algebra.GetTerm()); err != nil {
		return worker.Response{}, err
	}

	started := time.Now()
	documents, err := w.corpus.Scoped(ctx, term.GetEntityIds())
	if err != nil {
		return worker.Response{}, err
	}
	// Defence in depth: a corpus implementation that returned an unlinked document would break
	// SC-014, so the scoping is checked here as well as promised there.
	documents = keepLinked(documents, term.GetEntityIds())

	limit := int(term.GetLimit())
	if limit <= 0 || limit > engine.MaxKnowledgeItems {
		limit = engine.MaxKnowledgeItems
	}
	ranked := rank(documents, term.GetQueryTerms())
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	items := make([]*investigationv1.KnowledgeItem, 0, len(ranked))
	for _, entry := range ranked {
		doc := entry.document
		items = append(items, &investigationv1.KnowledgeItem{
			DocumentId:      doc.ID,
			Kind:            doc.Kind,
			LinkedEntityIds: sortedCopy(doc.LinkedEntityIDs),
			LinkProvenance:  doc.LinkProvenance,
			AuthoredAt:      timestamppb.New(doc.AuthoredAt.UTC()),
			AgeDays:         doc.AgeDays,
			Score:           entry.score,
			Citation:        citationOf(doc),
			// The excerpt is the registered summary, which is what a human wrote about the
			// document. It is never the document's content: the graph holds a pointer, not a
			// copy (FR-052).
			Excerpt: doc.Summary,
		})
	}

	coverage, err := engine.CoverageInput{
		SearchedEntities:  sortedCopy(term.GetEntityIds()),
		DataSource:        SourceOfTruth,
		WindowCovered:     searchedWindow(req),
		VolumeConsidered:  int64(len(documents)),
		Sampling:          "none",
		ExecutedAt:        started.UTC(),
		QuotaUndetermined: true,
		// The knowledge layer has no indexing lag: a document is retrievable as soon as its
		// CONCERNS edge is in the graph.
		IngestionLag: 0,
	}.Coverage()
	if err != nil {
		return worker.Response{}, err
	}

	outcome := engine.Outcome(engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_Knowledge{Knowledge: &investigationv1.KnowledgeDigest{
			Items:         items,
			ScorerVersion: ScorerVersion,
		}},
		Coverage: coverage,
	}})
	if len(items) == 0 {
		// Nothing is linked to these entities. That is a real answer — "the organisation has
		// written nothing down about this" — and it is NO_DATA rather than an empty digest.
		outcome = engine.NoData{Coverage: coverage}
	}

	resp, err := engine.NewResponse(engine.ResponseInput{
		Request:        req.Algebra,
		Outcome:        outcome,
		Mode:           req.Mode.String(),
		CostClass:      worker.CostClassCheap,
		Duration:       time.Since(started),
		BackendVersion: Version,
		Vocabulary:     ScorerVersion,
		ExecutedQuery:  fmt.Sprintf("knowledge_search(entities=%v, terms=%v)", term.GetEntityIds(), term.GetQueryTerms()),
		FreeText:       UnconfirmedNotice,
	})
	if err != nil {
		return worker.Response{}, err
	}
	return worker.Response{Worker: Name, Capability: req.Capability, Mode: req.Mode, Algebra: resp}, nil
}

// UnconfirmedNotice is the bounded free-text field every knowledge digest carries. It is the
// sentence FR-051 turns into a rule, said in the digest so that a reader of the evidence sees it
// where the citation is rather than only in the report's footnotes.
const UnconfirmedNotice = "knowledge-derived: a document says this. A hypothesis resting only on a document " +
	"is unconfirmed and may not reach `supported` until a source-of-truth worker agrees (FR-051)."

// keepLinked drops any document that is not linked to one of the requested entities. SC-014 is
// "an in-subgraph document is cited and an out-of-subgraph document is never cited", and this is
// where "never" is enforced rather than assumed.
func keepLinked(documents []Document, entityIDs []string) []Document {
	wanted := make(map[string]struct{}, len(entityIDs))
	for _, id := range entityIDs {
		wanted[id] = struct{}{}
	}
	out := make([]Document, 0, len(documents))
	for _, doc := range documents {
		for _, linked := range doc.LinkedEntityIDs {
			if _, ok := wanted[linked]; ok {
				out = append(out, doc)
				break
			}
		}
	}
	return out
}

func citationOf(doc Document) string {
	parts := []string{doc.Kind + " " + doc.ID}
	if doc.Title != "" {
		parts = append(parts, "“"+doc.Title+"”")
	}
	if !doc.AuthoredAt.IsZero() {
		parts = append(parts, doc.AuthoredAt.UTC().Format("2006-01-02"))
	}
	if doc.Location != "" {
		parts = append(parts, doc.Location)
	}
	if doc.LinkProvenance != "" {
		parts = append(parts, "linked "+doc.LinkProvenance)
	}
	return strings.Join(parts, ", ")
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func searchedWindow(req worker.Request) *engine.Window {
	at := req.Algebra.GetValidAt()
	if at == nil {
		at = timestamppb.New(time.Unix(0, 0).UTC())
	}
	return &engine.Window{Start: at, End: timestamppb.New(at.AsTime().Add(time.Second))}
}

// SearchTerm builds a `knowledge_search` request scoped to entity ids that MUST be inside the
// investigation's subgraph.
func SearchTerm(entityIDs, queryTerms []string, limit uint32, hypothesisID, question string) *engine.Request {
	return &engine.Request{
		Term:                   engine.KnowledgeSearch(entityIDs, queryTerms, limit),
		ServesHypothesisId:     hypothesisID,
		DiscriminatingQuestion: question,
	}
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
}

// Validate refuses a registration that would produce an uncitable document.
func (r LinkRequest) Validate() error {
	if r.DocumentRef.GetNamespace() == "" || r.DocumentRef.GetValue() == "" {
		return fmt.Errorf("knowledge link needs a document reference in <namespace>=<value> form")
	}
	if !ValidKind(r.Kind) {
		return fmt.Errorf("knowledge link kind %q is not published; the set is %v", r.Kind, Kinds())
	}
	if len(r.EntityRefs) == 0 {
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

	props, err := structpb.NewStruct(map[string]any{
		"kind":            r.Kind,
		"summary":         r.Summary,
		"location":        r.Location,
		"registered_by":   r.RegisteredBy,
		"registered_at":   at.UTC().Format(time.RFC3339),
		"authored_at":     authoredAt(r.AuthoredAt),
		"link_provenance": "registered by " + r.RegisteredBy,
	})
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
