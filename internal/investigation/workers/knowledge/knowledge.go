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
	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The knowledge worker (tasks.md T049; FR-049, FR-049a, FR-050, FR-051, FR-052; 008 FR-020–FR-025).
//
// **The graph scopes and dates; the knowledge store holds the text** (constitution 1.2.0). The
// candidate set is every document the graph links to something by a `CONCERNS` edge, and every
// reviewed past investigation, as of the request's instants — nothing else is a candidate, ever, so
// the store can neither resurrect a trashed page nor invent an unlinked one. Their sections are then
// ranked by `knowledge/hybrid@1` (hybrid.go), and a document linked to an entity inside the
// request's scope is boosted rather than being the only one allowed: the same failure on another
// service is still recalled, flagged `in_scope: false` (008 US2).
//
// Three rules the rest of the file enforces:
//
//   - **Retrieval over telemetry is refused.** The knowledge corpus is durable documents people
//     wrote and past investigations; a log line is not knowledge, and a worker that would search
//     both would have two sources of truth (FR-050).
//   - **Every item is cited** with its identifier, the entities it is linked to, how the link was
//     made, its age, the section that matched and whether it is in scope. A citation without
//     provenance is an appeal to authority.
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
	// Location is where the document lives — a URL or a reference — so a person can open it.
	Location string
}

// MaxQueryChars bounds the query text a search ranks by (008 data-model §Search).
const MaxQueryChars = 1000

// synthesised is the one section every candidate carries from what the graph already holds about
// it (008 FR-015): a document's title and registered summary as `Summary`, a past investigation's
// verdict line as `Verdict`. It is what a document with nothing in the store is matched on, and
// what keeps a title matchable for one that has.
func (d Document) synthesised() rankedSection {
	label, origin, text := "Summary", kstore.OriginSummary, joinNonEmpty(d.Title, d.Summary)
	if d.Kind == KindInvestigation {
		label, origin, text = "Verdict", kstore.OriginVerdict, d.Title
	}
	return rankedSection{key: sectionKey{document: d.ID, label: label}, text: truncateRunes(text, kstore.MaxSectionChars), origin: origin}
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, ". ")
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// Instants is when a retrieval is asked: the valid instant the investigation is about, and the
// observed instant that bounds what was known (ADR-0013 item 3). A zero Observed means "what is
// known now", which is what a live run asks and a replay never does.
type Instants struct {
	Valid    time.Time
	Observed time.Time
}

// Corpus is the candidate set the worker ranks. Implementations resolve `CONCERNS` edges against
// the graph; the interface exists so the worker can be tested without a database.
type Corpus interface {
	// Candidates returns every document linked by CONCERNS to any entity, and every reviewed past
	// investigation, as of at: only links valid at at.Valid and known at at.Observed, so a replay
	// never cites what the original run could not have read (007 FR-006, FR-007; 008 FR-023).
	// Each document's LinkedEntityIDs spell the entities in entityIDs as the caller spelled them,
	// and every other linked entity by its citable reference; the worker decides scope from them.
	Candidates(ctx context.Context, entityIDs []string, at Instants) ([]Document, error)
}

// Worker is the knowledge worker.
type Worker struct {
	corpus Corpus
	store  kstore.Store
}

var _ worker.Worker = (*Worker)(nil)

// Option configures a Worker.
type Option func(*Worker)

// WithStore names the knowledge store sections are searched in. Without one, every candidate is
// matched on its synthesised section alone, which is what 007 matched on.
func WithStore(store kstore.Store) Option { return func(w *Worker) { w.store = store } }

// New returns the knowledge worker over a corpus.
func New(corpus Corpus, opts ...Option) *Worker {
	w := &Worker{corpus: corpus}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Describe returns the declaration. No generative model: retrieval is ranked by a published,
// deterministic scorer, and an embedding model, when one is configured, only places vectors.
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
	at := instantsOf(req)
	documents, err := w.corpus.Candidates(ctx, term.GetEntityIds(), at)
	if err != nil {
		return worker.Response{}, err
	}

	limit := int(term.GetLimit())
	if limit <= 0 || limit > engine.MaxKnowledgeItems {
		limit = engine.MaxKnowledgeItems
	}
	ranked, err := w.search(ctx, documents, term, at)
	if err != nil {
		return worker.Response{}, err
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	items := make([]*investigationv1.KnowledgeItem, 0, len(ranked))
	for _, entry := range ranked {
		doc := entry.document
		item := &investigationv1.KnowledgeItem{
			DocumentId:      doc.ID,
			Kind:            doc.Kind,
			LinkedEntityIds: sortedCopy(doc.LinkedEntityIDs),
			LinkProvenance:  doc.LinkProvenance,
			AuthoredAt:      timestamppb.New(doc.AuthoredAt.UTC()),
			AgeDays:         doc.AgeDays,
			Score:           entry.score,
			Citation:        citationOf(doc),
			// The excerpt is the registered summary, which is what a human wrote about the
			// document; the matched section is beside it (ADR-0014 item 1).
			Excerpt: doc.Summary,
			InScope: entry.inScope,
		}
		if entry.section != nil {
			item.SectionLabel = entry.section.key.label
			item.MatchedText = truncateRunes(entry.section.text, kstore.MaxSectionChars)
		}
		items = append(items, item)
	}

	coverage, err := engine.CoverageInput{
		SearchedEntities:  sortedCopy(term.GetEntityIds()),
		DataSource:        SourceOfTruth,
		WindowCovered:     searchedWindow(req),
		VolumeConsidered:  int64(len(documents)),
		Sampling:          "none",
		ExecutedAt:        started.UTC(),
		QuotaUndetermined: true,
		// The graph side has no indexing lag: a document is a candidate as soon as its CONCERNS
		// edge is in the graph, and matched on its synthesised section from then on.
		IngestionLag: 0,
	}.Coverage()
	if err != nil {
		return worker.Response{}, err
	}

	outcome := engine.Outcome(engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_Knowledge{Knowledge: &investigationv1.KnowledgeDigest{
			Items:         items,
			ScorerVersion: ScorerVersion,
			RetrievalMode: ModeLexicalOnly,
		}},
		Coverage: coverage,
	}})
	if len(items) == 0 {
		// Nothing is linked to these entities and nothing elsewhere matched. That is a real
		// answer — "the organisation has written nothing down about this" — and it is NO_DATA
		// rather than an empty digest.
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

// result is one ranked document, before the digest is built.
type result struct {
	document Document
	score    float64
	inScope  bool
	section  *matchedSection // the best-matching section; nil when none matched
}

// search ranks documents by knowledge/hybrid@1. A matched document is returned whether or not it
// is in scope; an in-scope document nothing in which matched is returned after them at score 0,
// because "this is what the organisation wrote about these entities" is still worth a citation,
// and it is what 007 returned. An out-of-scope document that matched nothing is not returned.
func (w *Worker) search(ctx context.Context, documents []Document, term *investigationv1.KnowledgeSearchTerm, at Instants) ([]result, error) {
	requested := make(map[string]struct{}, len(term.GetEntityIds()))
	for _, id := range term.GetEntityIds() {
		requested[id] = struct{}{}
	}
	byRef := make(map[string]Document, len(documents))
	inScope := make(map[string]bool, len(documents))
	refs := make([]string, 0, len(documents))
	synthesised := make([]rankedSection, 0, len(documents))
	for _, doc := range documents {
		if _, dup := byRef[doc.ID]; dup {
			continue
		}
		byRef[doc.ID] = doc
		refs = append(refs, doc.ID)
		for _, linked := range doc.LinkedEntityIDs {
			if _, ok := requested[linked]; ok {
				inScope[doc.ID] = true
				break
			}
		}
		synthesised = append(synthesised, doc.synthesised())
	}
	sort.Strings(refs)

	query := truncateRunes(strings.Join(term.GetQueryTerms(), " "), MaxQueryChars)
	var stored []kstore.Hit
	if w.store != nil && len(refs) > 0 {
		hits, err := w.store.Lexical(ctx, query, refs, at.Observed, RankingDepth)
		if err != nil {
			return nil, fmt.Errorf("knowledge: lexical search: %w", err)
		}
		for _, hit := range hits {
			// Defence in depth: the store was asked for candidates only, and a hit outside them
			// would be a document the graph does not vouch for.
			if _, ok := byRef[hit.DocumentRef]; ok {
				stored = append(stored, hit)
			}
		}
	}

	fused := fuse([][]rankedSection{lexicalRanking(query, synthesised, stored)}, inScope)
	out := make([]result, 0, len(fused))
	matched := make(map[string]struct{}, len(fused))
	for _, f := range fused {
		best := f.sections[0]
		out = append(out, result{document: byRef[f.ref], score: f.score, inScope: inScope[f.ref], section: &best})
		matched[f.ref] = struct{}{}
	}
	for _, ref := range refs {
		if _, ok := matched[ref]; ok || !inScope[ref] {
			continue
		}
		out = append(out, result{document: byRef[ref], inScope: true})
	}
	return out, nil
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

// instantsOf reads the request's two instants. An unset observed instant means the valid one,
// as everywhere in the algebra (002 FR-004); a request with neither asks about now.
func instantsOf(req worker.Request) Instants {
	var at Instants
	if ts := req.Algebra.GetValidAt(); ts != nil {
		at.Valid = ts.AsTime().UTC()
	}
	if ts := req.Algebra.GetObservedAt(); ts != nil {
		at.Observed = ts.AsTime().UTC()
	} else {
		at.Observed = at.Valid
	}
	if at.Valid.IsZero() {
		at.Valid = time.Now().UTC()
	}
	return at
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
	// ConfiguredBy is the principal who signed a connector's configuration, when a connector
	// registers the document rather than a person (007 ADR-0013 item 2). Empty for `knowledge link`.
	ConfiguredBy string
	// SummaryTruncated says the summary was cut to its cap. A connector sets it; a person's summary
	// is theirs and is never cut.
	SummaryTruncated bool
	// Provenance replaces the default `registered by <RegisteredBy>` link provenance, for a connector
	// whose link was made by a rule an operator configured (`notion/notion-db@1 configured by …`).
	Provenance string
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
