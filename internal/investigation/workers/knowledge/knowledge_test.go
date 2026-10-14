// SPDX-License-Identifier: Apache-2.0

package knowledge_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
	"github.com/Pierre-Theophile/aisre/pkg/worker/testkit"
)

// The knowledge worker (tasks.md T049; FR-049, FR-049a, FR-050, FR-051, FR-052).

var authored = time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

// corpus is an in-memory stand-in for the graph's CONCERNS edges. The worker's contract is that
// it retrieves only what is linked, so the test's corpus deliberately holds documents that are
// linked to entities outside the requested set: SC-014 is that they are never cited.
//
// It also honours the instants the way the graph does, with AuthoredAt standing in for the link's
// start: a document written after the valid instant is not a candidate (007 FR-006).
type corpus struct{ documents []knowledge.Document }

func (c corpus) Scoped(_ context.Context, entityIDs []string, at knowledge.Instants) ([]knowledge.Document, error) {
	wanted := make(map[string]struct{}, len(entityIDs))
	for _, id := range entityIDs {
		wanted[id] = struct{}{}
	}
	out := make([]knowledge.Document, 0, len(c.documents))
	for _, doc := range c.documents {
		if doc.AuthoredAt.After(at.Valid) {
			continue
		}
		for _, linked := range doc.LinkedEntityIDs {
			if _, ok := wanted[linked]; ok {
				out = append(out, doc)
				break
			}
		}
	}
	return out, nil
}

// leakyCorpus returns everything it has, linked or not — the mistake a real implementation could
// make. The worker must still cite only what is linked.
type leakyCorpus struct{ documents []knowledge.Document }

func (c leakyCorpus) Scoped(context.Context, []string, knowledge.Instants) ([]knowledge.Document, error) {
	return c.documents, nil
}

func documents() []knowledge.Document {
	return []knowledge.Document{
		{
			ID: "pm-2026-04", Kind: knowledge.KindPostmortem,
			Title:           "Checkout error rate spike after payments rollout",
			Summary:         "A payments deploy raised the checkout error rate; rolled back in twelve minutes.",
			LinkedEntityIDs: []string{"entity-payments", "entity-checkout"},
			LinkProvenance:  "registered by sre@example.com", AuthoredAt: authored, AgeDays: 90,
			HopsFromFocus: 0, Location: "notion://pm-2026-04",
		},
		{
			ID: "rb-checkout", Kind: knowledge.KindRunbook,
			Title:           "Checkout runbook",
			Summary:         "How to page, how to roll back, and which dashboards to open.",
			LinkedEntityIDs: []string{"entity-checkout"},
			LinkProvenance:  "registered by sre@example.com", AuthoredAt: authored, AgeDays: 400,
			HopsFromFocus: 0, Location: "notion://rb-checkout",
		},
		{
			ID: "adr-9", Kind: knowledge.KindDecision,
			Title:           "Analytics warehouse partitioning",
			Summary:         "Why the analytics warehouse partitions by day rather than by tenant.",
			LinkedEntityIDs: []string{"entity-analytics"},
			LinkProvenance:  "registered by data@example.com", AuthoredAt: authored, AgeDays: 30,
			HopsFromFocus: 4, Location: "notion://adr-9",
		},
	}
}

func TestRetrievalIsScopedByTheGraph(t *testing.T) {
	t.Parallel()

	w := knowledge.New(corpus{documents: documents()})
	resp, err := w.Call(context.Background(), worker.Request{
		Capability: engine.TermKnowledgeSearch,
		Mode:       worker.ModeRecorded,
		Algebra:    knowledge.SearchTerm([]string{"entity-checkout", "entity-payments"}, []string{"checkout", "rollout"}, 10, "hyp-1", "has this happened before?"),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	items := resp.Algebra.GetDigest().GetKnowledge().GetItems()
	if len(items) != 2 {
		t.Fatalf("cited %d documents, want the two linked to the requested entities: %v", len(items), items)
	}
	for _, item := range items {
		if item.GetDocumentId() == "adr-9" {
			t.Error("cited a document linked to no requested entity; SC-014 is that it is never cited")
		}
		if item.GetCitation() == "" || item.GetLinkProvenance() == "" {
			t.Errorf("document %s is cited with no citation or no link provenance; that is an appeal to authority", item.GetDocumentId())
		}
		if len(item.GetLinkedEntityIds()) == 0 {
			t.Errorf("document %s names no linked entities", item.GetDocumentId())
		}
	}
	// The postmortem is about the rollout and is far fresher than the runbook, so it ranks first.
	if items[0].GetDocumentId() != "pm-2026-04" {
		t.Errorf("first citation = %s, want pm-2026-04", items[0].GetDocumentId())
	}
	if items[0].GetScore() <= items[1].GetScore() {
		t.Errorf("citations are not ordered by score: %v then %v", items[0].GetScore(), items[1].GetScore())
	}
}

// TestALeakyCorpusIsFilteredAnyway: the scoping is enforced in the worker, not only promised by
// the corpus.
func TestALeakyCorpusIsFilteredAnyway(t *testing.T) {
	t.Parallel()

	w := knowledge.New(leakyCorpus{documents: documents()})
	resp, err := w.Call(context.Background(), worker.Request{
		Capability: engine.TermKnowledgeSearch,
		Mode:       worker.ModeRecorded,
		Algebra:    knowledge.SearchTerm([]string{"entity-checkout"}, []string{"runbook"}, 10, "hyp-1", "what does the runbook say?"),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	for _, item := range resp.Algebra.GetDigest().GetKnowledge().GetItems() {
		if item.GetDocumentId() == "adr-9" {
			t.Fatal("a document linked to no requested entity was cited even though the worker filters")
		}
	}
}

// TestRetrievalOverTelemetryIsRefused: a log line is not knowledge, and a worker that searched
// both would have two sources of truth (FR-050).
func TestRetrievalOverTelemetryIsRefused(t *testing.T) {
	t.Parallel()

	w := knowledge.New(corpus{documents: documents()})
	_, err := w.Call(context.Background(), worker.Request{
		Capability: engine.TermNewLogPatterns,
		Mode:       worker.ModeRecorded,
		Algebra:    &engine.Request{DiscriminatingQuestion: "what is in the logs?"},
	})
	if err == nil {
		t.Fatal("the knowledge worker answered a telemetry term")
	}
	if !strings.Contains(err.Error(), "prohibited") {
		t.Errorf("refusal %q does not say retrieval over telemetry is prohibited", err)
	}
}

// TestNothingLinkedIsNoData: "the organisation has written nothing down about this" is a real
// answer and is NO_DATA, not an empty digest.
func TestNothingLinkedIsNoData(t *testing.T) {
	t.Parallel()

	w := knowledge.New(corpus{documents: documents()})
	resp, err := w.Call(context.Background(), worker.Request{
		Capability: engine.TermKnowledgeSearch,
		Mode:       worker.ModeRecorded,
		Algebra:    knowledge.SearchTerm([]string{"entity-nobody-wrote-about"}, []string{"anything"}, 10, "hyp-1", "anything known?"),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Algebra.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Errorf("outcome = %s, want NO_DATA", resp.Algebra.GetOutcome())
	}
	if resp.Algebra.GetDigest().GetCoverage() == nil {
		t.Error("the answer carries no coverage block")
	}
}

// TestEveryKnowledgeDigestSaysItIsUnconfirmed: a hypothesis resting only on a document may not
// reach `supported`, and the digest says so where the citation is (FR-051).
func TestEveryKnowledgeDigestSaysItIsUnconfirmed(t *testing.T) {
	t.Parallel()

	w := knowledge.New(corpus{documents: documents()})
	resp, err := w.Call(context.Background(), worker.Request{
		Capability: engine.TermKnowledgeSearch,
		Mode:       worker.ModeRecorded,
		Algebra:    knowledge.SearchTerm([]string{"entity-checkout"}, []string{"checkout"}, 10, "hyp-1", "has this happened before?"),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := resp.Algebra.GetDigest().GetFreeText()
	for _, want := range []string{"knowledge-derived", "unconfirmed", "supported"} {
		if !strings.Contains(text, want) {
			t.Errorf("the digest note does not mention %q: %q", want, text)
		}
	}
	if !strings.HasPrefix(text, engine.FreeTextPrefix) {
		t.Errorf("the note is not flagged unverified: %q", text)
	}
}

// instantsCorpus records the instants it was asked at.
type instantsCorpus struct{ asked *knowledge.Instants }

func (c instantsCorpus) Scoped(_ context.Context, _ []string, at knowledge.Instants) ([]knowledge.Document, error) {
	*c.asked = at
	return nil, nil
}

// TestTheCorpusIsAskedAtTheRequestsInstants: the worker passes the request's valid and observed
// instants through, so a replay asks the graph what the original run could have read (007 FR-006,
// FR-007; ADR-0013 item 3).
func TestTheCorpusIsAskedAtTheRequestsInstants(t *testing.T) {
	t.Parallel()

	valid := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	observed := valid.Add(5 * time.Minute)
	for _, tc := range []struct {
		name         string
		observed     *time.Time
		wantObserved time.Time
	}{
		{"observed given", &observed, observed},
		{"observed unset means valid", nil, valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked knowledge.Instants
			request := knowledge.SearchTerm([]string{"entity-checkout"}, []string{"checkout"}, 10, "hyp-1", "known?")
			request.ValidAt = timestamppb.New(valid)
			if tc.observed != nil {
				request.ObservedAt = timestamppb.New(*tc.observed)
			}
			w := knowledge.New(instantsCorpus{asked: &asked})
			if _, err := w.Call(context.Background(), worker.Request{
				Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request,
			}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if !asked.Valid.Equal(valid) || !asked.Observed.Equal(tc.wantObserved) {
				t.Errorf("corpus asked at %+v, want valid %s observed %s", asked, valid, tc.wantObserved)
			}
		})
	}
}

// TestADocumentWrittenAfterTheInstantIsNotCited: retrieval is as of the investigation's instant.
func TestADocumentWrittenAfterTheInstantIsNotCited(t *testing.T) {
	t.Parallel()

	request := knowledge.SearchTerm([]string{"entity-checkout"}, []string{"checkout"}, 10, "hyp-1", "known?")
	request.ValidAt = timestamppb.New(authored.Add(-24 * time.Hour))
	w := knowledge.New(corpus{documents: documents()})
	resp, err := w.Call(context.Background(), worker.Request{
		Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if n := len(resp.Algebra.GetDigest().GetKnowledge().GetItems()); n != 0 {
		t.Errorf("cited %d documents written after the instant asked about", n)
	}
}

func TestDeclaration(t *testing.T) {
	t.Parallel()
	w := knowledge.New(corpus{documents: documents()})
	testkit.Declaration(t, w)

	d := w.Describe()
	if d.ContainsModel {
		t.Error("the v1 knowledge worker declares a model; BM25 over tens of documents needs none (research §7)")
	}
	if len(d.Capabilities) != 1 || !d.Declares(engine.TermKnowledgeSearch) {
		t.Errorf("capabilities = %v, want exactly knowledge_search", d.Capabilities)
	}
}

// TestLinkRegistersANodeAndItsEdgesAndNeverTheContent (FR-049a, FR-052).
func TestLinkRegistersANodeAndItsEdgesAndNeverTheContent(t *testing.T) {
	t.Parallel()

	request := knowledge.LinkRequest{
		DocumentRef: &graphv1.Ref{Namespace: "notion.page", Value: "9f2c"},
		Kind:        knowledge.KindRunbook,
		Title:       "Checkout runbook",
		Summary:     "How to page, how to roll back.",
		Location:    "https://notion.example/9f2c",
		EntityRefs: []*graphv1.Ref{
			{Namespace: "otel.service.name", Value: "checkout"},
			{Namespace: "otel.service.name", Value: "payments"},
		},
		RegisteredBy: "sre@example.com",
		AuthoredAt:   authored,
	}
	events, err := request.Events("knowledge:human", authored)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("produced %d events, want one node plus two CONCERNS edges", len(events))
	}
	node := events[0].GetUpsertNode()
	if node.GetType() != graphv1.NodeType_KNOWLEDGE_DOC {
		t.Errorf("node type = %s, want KNOWLEDGE_DOC", node.GetType())
	}
	if node.GetDisplayName() != "Checkout runbook" {
		t.Errorf("display name = %q", node.GetDisplayName())
	}
	for _, event := range events[1:] {
		if event.GetUpsertEdge().GetType() != graphv1.EdgeType_CONCERNS {
			t.Errorf("edge type = %s, want CONCERNS", event.GetUpsertEdge().GetType())
		}
	}

	// Registering the same document twice produces the same event ids, so it is a duplicate
	// no-op rather than a second node.
	again, err := request.Events("knowledge:human", authored)
	if err != nil {
		t.Fatalf("events again: %v", err)
	}
	for i := range events {
		if events[i].GetEventId() != again[i].GetEventId() {
			t.Errorf("event %d id moved between two identical registrations", i)
		}
	}
}

func TestLinkRefusesARegistrationThatCannotBeCited(t *testing.T) {
	t.Parallel()

	valid := knowledge.LinkRequest{
		DocumentRef:  &graphv1.Ref{Namespace: "notion.page", Value: "9f2c"},
		Kind:         knowledge.KindRunbook,
		Title:        "Checkout runbook",
		EntityRefs:   []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout"}},
		RegisteredBy: "sre@example.com",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid registration was refused: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*knowledge.LinkRequest)
		want   string
	}{
		{"no document reference", func(r *knowledge.LinkRequest) { r.DocumentRef = nil }, "document reference"},
		{"an unpublished kind", func(r *knowledge.LinkRequest) { r.Kind = "wiki" }, "not published"},
		{"no entities", func(r *knowledge.LinkRequest) { r.EntityRefs = nil }, "linked to nothing"},
		{"no principal", func(r *knowledge.LinkRequest) { r.RegisteredBy = "" }, "registering identity"},
		{"no title", func(r *knowledge.LinkRequest) { r.Title = "" }, "title"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			request := valid
			tc.mutate(&request)
			err := request.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a registration with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}
