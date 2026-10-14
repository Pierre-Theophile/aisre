// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The knowledge corpus read (007 T010–T012; FR-006–FR-008, ADR-0013 item 3).

func loadFixture(t *testing.T, name string) (*query.Engine, *projector.Projector) {
	t.Helper()
	store := pgtest.Open(t)
	p := projector.New(store)
	if _, err := fixture.Load(context.Background(), p, filepath.Join(fixturesDir, name), fixture.LoadOptions{}); err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return query.NewEngine(store), p
}

func candidateRefs(candidates []query.KnowledgeCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Ref)
	}
	return out
}

var (
	paymentsRunbook  = "github.file=ops/runbooks/payments-rollouts.md"
	reportingRunbook = "github.file=ops/runbooks/reporting-nightly.md"
	at1432           = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
)

// TestKnowledgeIsScopedByTheGraph is SC-014 against the real graph: the in-subgraph runbook is a
// candidate for checkout and payments, the lexically better reporting runbook is not.
func TestKnowledgeIsScopedByTheGraph(t *testing.T) {
	engine, _ := loadFixture(t, "incidents/knowledge-scope-01")
	ctx := context.Background()

	got, err := engine.KnowledgeCandidates(ctx,
		[]string{"otel.service.name=checkout", "otel.service.name=payments"}, query.AsOf{ValidAt: at1432})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if refs := candidateRefs(got); !slices.Equal(refs, []string{paymentsRunbook}) {
		t.Fatalf("candidates = %v, want only %s", refs, paymentsRunbook)
	}
	doc := got[0]
	if doc.Type != graph.NodeTypeKnowledgeDoc {
		t.Errorf("type = %s, want knowledge_doc", doc.Type)
	}
	if want := []string{"otel.service.name=checkout", "otel.service.name=payments"}; !slices.Equal(doc.Linked, want) {
		t.Errorf("linked = %v, want %v, spelled as requested", doc.Linked, want)
	}
	if doc.DisplayName != "payments rollout runbook" || doc.Props["kind"] != "runbook" || doc.Props["summary"] == nil {
		t.Errorf("document read as %q with props %v", doc.DisplayName, doc.Props)
	}

	got, err = engine.KnowledgeCandidates(ctx, []string{"otel.service.name=reporting"}, query.AsOf{ValidAt: at1432})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if refs := candidateRefs(got); !slices.Equal(refs, []string{reportingRunbook}) {
		t.Errorf("reporting candidates = %v, want only %s", refs, reportingRunbook)
	}

	got, err = engine.KnowledgeCandidates(ctx, []string{"otel.service.name=nobody-wrote-about"}, query.AsOf{ValidAt: at1432})
	if err != nil || len(got) != 0 {
		t.Errorf("an unknown entity gave %v, %v; want nothing and no error", got, err)
	}
}

// TestKnowledgeIsReadAsOfBothInstants: a link valid only after the valid instant, or known only
// after the observed instant, is not a candidate, so a replay cannot cite what the run could not
// have read (FR-006, FR-007).
func TestKnowledgeIsReadAsOfBothInstants(t *testing.T) {
	engine, _ := loadFixture(t, "incidents/knowledge-scope-01")
	ctx := context.Background()
	refs := []string{"otel.service.name=checkout", "otel.service.name=payments"}

	for _, tc := range []struct {
		name   string
		at     query.AsOf
		want   []string
		linked []string
	}{
		{"before the link was valid", query.AsOf{ValidAt: time.Date(2026, 9, 1, 13, 5, 0, 0, time.UTC)}, nil, nil},
		{"node known, no edge known yet",
			query.AsOf{ValidAt: at1432, ObservedAt: time.Date(2026, 9, 1, 13, 6, 12, 0, time.UTC)}, nil, nil},
		{"the payments edge known, the checkout edge not yet",
			query.AsOf{ValidAt: at1432, ObservedAt: time.Date(2026, 9, 1, 13, 6, 17, 0, time.UTC)},
			[]string{paymentsRunbook}, []string{"otel.service.name=payments"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := engine.KnowledgeCandidates(ctx, refs, tc.at)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if r := candidateRefs(got); !slices.Equal(r, tc.want) {
				t.Fatalf("candidates = %v, want %v", r, tc.want)
			}
			if len(got) == 1 && !slices.Equal(got[0].Linked, tc.linked) {
				t.Errorf("linked = %v, want %v", got[0].Linked, tc.linked)
			}
		})
	}
}

// TestReviewedInvestigationsAreKnowledge: a past investigation is a candidate once a review label
// is known, from the instant its run began, and the human fact's CONCERNS edge from an
// investigation never makes it a "document" (FR-008, research R3).
func TestReviewedInvestigationsAreKnowledge(t *testing.T) {
	engine, p := loadFixture(t, "incidents/human-fact-reopen-01")
	ctx := context.Background()
	refs := []string{"otel.service.name=checkout"}
	later := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	got, err := engine.KnowledgeCandidates(ctx, refs, query.AsOf{ValidAt: later})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unreviewed investigations were candidates: %v", candidateRefs(got))
	}

	labelledAt := time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC)
	label := &graphv1.EventEnvelope{
		EventId:       "sre-agent:demo:label:inv-2026-09-01-0007",
		SourceId:      "sre-agent:demo",
		SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_LabelInvestigation{LabelInvestigation: &graphv1.LabelInvestigation{
			InvestigationId: "inv-2026-09-01-0007", WasThisRight: true, Author: "sre-agent-dev|dana",
			LabelledAt: timestamppb.New(labelledAt),
		}},
	}
	if result, err := p.Apply(ctx, label, labelledAt.Add(5*time.Second)); err != nil {
		t.Fatalf("label: %v", err)
	} else if result.GetStatus() == graphv1.IngestResult_REJECTED {
		t.Fatalf("label rejected: %v", result)
	}

	got, err = engine.KnowledgeCandidates(ctx, refs, query.AsOf{ValidAt: later})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if r := candidateRefs(got); !slices.Equal(r, []string{"sre.investigation.id=inv-2026-09-01-0007"}) {
		t.Fatalf("candidates = %v, want the reviewed investigation only", r)
	}
	if got[0].Type != graph.NodeTypeInvestigation || got[0].Props[query.InvestigationLabelProp] == nil {
		t.Errorf("candidate read as %s with props %v", got[0].Type, got[0].Props)
	}
	if want := time.Date(2026, 9, 1, 14, 33, 0, 0, time.UTC); !got[0].LinkedSince.Equal(want) {
		t.Errorf("linked since %s, want the run's start %s", got[0].LinkedSince, want)
	}

	// Before the label was known, a replay does not see it.
	got, err = engine.KnowledgeCandidates(ctx, refs, query.AsOf{ValidAt: later, ObservedAt: labelledAt})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a label not yet known made a candidate: %v", candidateRefs(got))
	}

	// Before the run began, it is not knowledge yet.
	got, err = engine.KnowledgeCandidates(ctx, refs, query.AsOf{ValidAt: time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an investigation that had not begun was a candidate: %v", candidateRefs(got))
	}
}

func TestInvestigationLabelPropMatchesTheProjector(t *testing.T) {
	if query.InvestigationLabelProp != projector.InvestigationLabelProp {
		t.Errorf("query reads %q, the projector writes %q", query.InvestigationLabelProp, projector.InvestigationLabelProp)
	}
}
