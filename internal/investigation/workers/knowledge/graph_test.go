// SPDX-License-Identifier: Apache-2.0

package knowledge_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The worker over the graph-backed corpus (007 T012): the same answers as over the in-memory
// corpus, read from a replayed fixture instead of a slice.

func TestMain(m *testing.M) { pgtest.TestMain(m) }

func TestTheGraphCorpusCitesOnlyWhatIsLinked(t *testing.T) {
	store := pgtest.Open(t)
	ctx := context.Background()
	if _, err := fixture.Load(ctx, projector.New(store), "../../../../fixtures/incidents/knowledge-scope-01", fixture.LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	w := knowledge.New(knowledge.GraphCorpus{Engine: query.NewEngine(store)})

	request := knowledge.SearchTerm(
		[]string{"otel.service.name=checkout", "otel.service.name=payments"},
		[]string{"connection", "pool", "5xx"}, 10, "hyp-1", "has this happened before?")
	request.ValidAt = timestamppb.New(time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC))
	resp, err := w.Call(ctx, worker.Request{Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	items := resp.Algebra.GetDigest().GetKnowledge().GetItems()
	if len(items) != 1 {
		t.Fatalf("cited %d documents, want the payments runbook alone: %v", len(items), items)
	}
	item := items[0]
	if item.GetDocumentId() != "github.file=ops/runbooks/payments-rollouts.md" {
		t.Errorf("cited %s", item.GetDocumentId())
	}
	if item.GetKind() != knowledge.KindRunbook || item.GetExcerpt() == "" {
		t.Errorf("kind %q, excerpt %q", item.GetKind(), item.GetExcerpt())
	}
	if item.GetLinkProvenance() != "registered by sre-agent-dev|dana" {
		t.Errorf("provenance %q", item.GetLinkProvenance())
	}
	// Authored 2026-05-14 11:00, asked about 2026-09-01 14:32: 110 whole days.
	if item.GetAgeDays() != 110 {
		t.Errorf("age %d days, want 110", item.GetAgeDays())
	}
	for _, want := range []string{"runbook", "payments rollout runbook", "https://github.com/shop/ops"} {
		if !strings.Contains(item.GetCitation(), want) {
			t.Errorf("citation %q does not carry %q", item.GetCitation(), want)
		}
	}
}

// 007 T027/T028: the postmortems `aisre feed notion` registers, read from a replayed incident.
// The one on the alert's services is cited with everything a reader needs to open it and judge
// it; the better lexical match outside the subgraph and the one written after the alert are not.
// The planted instruction in the cited summary is an excerpt, quoted, and changes nothing else.
func TestTheGraphCorpusCitesANotionPostmortem(t *testing.T) {
	store := pgtest.Open(t)
	ctx := context.Background()
	if _, err := fixture.Load(ctx, projector.New(store), "../../../../fixtures/incidents/knowledge-notion-01", fixture.LoadOptions{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	w := knowledge.New(knowledge.GraphCorpus{Engine: query.NewEngine(store)})

	alert := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	request := knowledge.SearchTerm(
		[]string{"otel.service.name=checkout", "otel.service.name=payments"},
		[]string{"connection", "pool", "5xx", "rollout"}, 10, "hyp-1", "has this happened before?")
	request.ValidAt = timestamppb.New(alert)
	request.ObservedAt = timestamppb.New(alert)
	resp, err := w.Call(ctx, worker.Request{Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	items := resp.Algebra.GetDigest().GetKnowledge().GetItems()
	if len(items) != 1 {
		t.Fatalf("cited %d documents, want the May postmortem alone: %v", len(items), items)
	}
	item := items[0]
	if item.GetDocumentId() != "notion.page=c3000000-0000-4000-8000-000000000001" {
		t.Errorf("cited %s", item.GetDocumentId())
	}
	if item.GetKind() != knowledge.KindPostmortem {
		t.Errorf("kind %q", item.GetKind())
	}
	if got := strings.Join(item.GetLinkedEntityIds(), ","); got != "otel.service.name=checkout,otel.service.name=payments" {
		t.Errorf("linked %s", got)
	}
	if item.GetLinkProvenance() != "notion/notion-db@1 configured by oidc:operator-twin" {
		t.Errorf("provenance %q", item.GetLinkProvenance())
	}
	// Authored 2026-05-14, asked about 2026-09-01 14:32: 110 whole days.
	if item.GetAgeDays() != 110 {
		t.Errorf("age %d days, want 110", item.GetAgeDays())
	}
	for _, want := range []string{"postmortem", "Checkout 5xx after a payments rollout",
		"https://www.notion.so/c3000000000040008000000000000001", "linked notion/notion-db@1"} {
		if !strings.Contains(item.GetCitation(), want) {
			t.Errorf("citation %q does not carry %q", item.GetCitation(), want)
		}
	}
	if !strings.Contains(resp.Algebra.GetDigest().GetFreeText(), "unconfirmed") {
		t.Errorf("the digest does not say a document alone is unconfirmed: %q", resp.Algebra.GetDigest().GetFreeText())
	}
}
