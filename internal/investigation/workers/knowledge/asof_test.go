// SPDX-License-Identifier: Apache-2.0

package knowledge_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/memory"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// Sections from the knowledge store, read as of the request's observed instant (008 T012, US1 and
// US3, FR-023, SC-003).

var (
	pin       = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	beforePin = pin.Add(-30 * 24 * time.Hour)
	afterPin  = pin.Add(2 * time.Hour)
)

func postmortem(id, title string, linked ...string) knowledge.Document {
	return knowledge.Document{ID: id, Kind: knowledge.KindPostmortem, Title: title,
		Summary: "A short summary that names no symptom.", LinkedEntityIDs: linked,
		LinkProvenance: "registered by sre@example.com", AuthoredAt: beforePin}
}

func storePut(t *testing.T, s kstore.Store, doc string, at time.Time, sections ...kstore.Section) {
	t.Helper()
	if _, err := s.Put(context.Background(), doc, sections, at); err != nil {
		t.Fatalf("put %s: %v", doc, err)
	}
}

func body(label, text string) kstore.Section {
	return kstore.Section{Label: label, Text: text, Origin: kstore.OriginBody}
}

func searchAt(t *testing.T, w *knowledge.Worker, observed time.Time, terms ...string) []*investigationv1.KnowledgeItem {
	t.Helper()
	request := knowledge.SearchTerm([]string{"entity-checkout"}, terms, 10, "hyp-1", "has this happened before?")
	request.ValidAt = timestamppb.New(pin)
	request.ObservedAt = timestamppb.New(observed)
	resp, err := w.Call(context.Background(), worker.Request{Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return resp.Algebra.GetDigest().GetKnowledge().GetItems()
}

// TestASymptomDeepInAPostmortemIsMatched is US1's first scenario, lexically: the symptom is only in
// a body section, nowhere in the title or summary 007 matched on.
func TestASymptomDeepInAPostmortemIsMatched(t *testing.T) {
	t.Parallel()
	s := memory.New()
	storePut(t, s, "pm-1", beforePin,
		body("Timeline", strings.Repeat("The on-call engineer looked at dashboards. ", 20)),
		body("Root cause", "TLS handshake timeouts from the payment gateway after a certificate rotation"))
	w := knowledge.New(corpus{documents: []knowledge.Document{
		postmortem("pm-1", "Checkout incident", "entity-checkout"),
		postmortem("pm-2", "Another checkout incident", "entity-checkout"),
	}}, knowledge.WithStore(s))

	items := searchAt(t, w, pin, "tls", "handshake", "timeouts")
	if len(items) == 0 || items[0].GetDocumentId() != "pm-1" {
		t.Fatalf("cited %v, want pm-1 first", ids(items))
	}
	if items[0].GetSectionLabel() != "Root cause" || !strings.Contains(items[0].GetMatchedText(), "TLS handshake") {
		t.Errorf("matched %q: %q, want the Root cause section", items[0].GetSectionLabel(), items[0].GetMatchedText())
	}
}

// TestAsOfSections covers US3's scenarios against the store: a section rewritten after the pin is
// matched in its earlier text, a section added after the pin is never matched, and a live read
// sees the latest text.
func TestAsOfSections(t *testing.T) {
	t.Parallel()
	s := memory.New()
	storePut(t, s, "pm-1", beforePin, body("Root cause", "connection pool exhaustion in payments"))
	storePut(t, s, "pm-1", afterPin, body("Root cause", "a slow index on the orders table"))
	storePut(t, s, "pm-2", afterPin, body("Root cause", "connection pool exhaustion again, written later"))
	w := knowledge.New(corpus{documents: []knowledge.Document{
		postmortem("pm-1", "Checkout incident", "entity-checkout"),
		postmortem("pm-2", "Second checkout incident", "entity-checkout"),
	}}, knowledge.WithStore(s))

	atPin := searchAt(t, w, pin, "pool", "exhaustion")
	if len(atPin) == 0 || atPin[0].GetDocumentId() != "pm-1" || atPin[0].GetSectionLabel() != "Root cause" {
		t.Fatalf("at the pin cited %v, want pm-1 matched in its earlier text", ids(atPin))
	}
	for _, item := range atPin {
		if item.GetDocumentId() == "pm-2" && item.GetSectionLabel() != "" {
			t.Errorf("pm-2 was matched on %q, a section known only after the pin", item.GetSectionLabel())
		}
	}
	if items := searchAt(t, w, pin, "index", "orders"); len(items) > 0 && items[0].GetSectionLabel() != "" {
		t.Errorf("the rewrite known after the pin was matched: %v on %q", ids(items), items[0].GetSectionLabel())
	}
	later := searchAt(t, w, afterPin, "index", "orders")
	if len(later) == 0 || later[0].GetDocumentId() != "pm-1" || later[0].GetSectionLabel() != "Root cause" {
		t.Errorf("observed after the rewrite, cited %v, want pm-1 on its new text", ids(later))
	}
}

// TestADocumentTheGraphDoesNotVouchForIsNeverCited: the store holds sections of a document no
// longer in the graph (a trashed page); the worker never cites it, whatever matches (FR-002).
func TestADocumentTheGraphDoesNotVouchForIsNeverCited(t *testing.T) {
	t.Parallel()
	s := memory.New()
	storePut(t, s, "pm-trashed", beforePin, body("Root cause", "connection pool exhaustion"))
	w := knowledge.New(corpus{documents: []knowledge.Document{postmortem("pm-1", "Checkout incident", "entity-checkout")}}, knowledge.WithStore(s))
	if got := ids(searchAt(t, w, pin, "pool", "exhaustion")); slices.Contains(got, "pm-trashed") {
		t.Errorf("cited %v, which includes a document the graph does not hold", got)
	}
}
