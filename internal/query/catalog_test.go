// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The catalog reads (feature 009, FR-005, FR-039): what the catalog was, and why a signal was
// attributed as it was, at an instant — including an instant before a correction.

var catalogStart = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

type catalogGraph struct {
	t   *testing.T
	p   *projector.Projector
	seq int
}

func newCatalogGraph(t *testing.T) *catalogGraph {
	t.Helper()
	p := projector.New(pgtest.Open(t))
	for _, src := range []string{"datadog:test", "human", graph.AttributorSourceID} {
		if err := p.RegisterSource(context.Background(), eventlog.Source{
			SourceID: src, Kind: "test", Ordering: "none", SchemaVersion: "1.0.0",
		}); err != nil {
			t.Fatalf("register %s: %v", src, err)
		}
	}
	return &catalogGraph{t: t, p: p}
}

// at is the observed instant of the n-th event.
func (g *catalogGraph) at(n int) time.Time { return catalogStart.Add(time.Duration(n) * time.Minute) }

func (g *catalogGraph) apply(source, principal string, body func(*graphv1.EventEnvelope)) string {
	g.t.Helper()
	g.seq++
	env := &graphv1.EventEnvelope{SourceId: source, SchemaVersion: "1.0.0"}
	env.EventId = source + ":" + string(rune('a'+g.seq))
	env.IdempotencyKey = env.EventId
	body(env)
	r, err := g.p.ApplyWithOptions(context.Background(), env,
		projector.ApplyOptions{ObservedAt: g.at(g.seq), Principal: principal})
	if err != nil || r.GetStatus() != graphv1.IngestResult_APPLIED {
		g.t.Fatalf("apply %s: %v %s %s", env.GetEventId(), err, r.GetReasonCode(), r.GetReasonDetail())
	}
	return env.GetEventId()
}

func TestCatalogReadsAsOf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newCatalogGraph(t)
	alert := &graphv1.Ref{Namespace: "datadog.monitor", Value: "12345"}

	g.apply("datadog:test", "", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref: alert, Type: graphv1.NodeType_ALERT, DisplayName: "candidate-api error rate",
			ValidAt: timestamppb.New(catalogStart),
		}}
	})
	for _, entry := range [][2]string{{"candidate-view", "Candidate View"}, {"infra", "Infra"}} {
		g.apply("human", "oidc:pierre", func(env *graphv1.EventEnvelope) {
			env.Body = &graphv1.EventEnvelope_DeclareCatalogEntry{DeclareCatalogEntry: &graphv1.DeclareCatalogEntry{
				Slug: entry[0], DisplayName: entry[1], Kind: "service", Status: "confirmed",
			}}
		})
	}
	g.apply("human", "oidc:pierre", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_DeclareAlias{DeclareAlias: &graphv1.DeclareAlias{
			Name: &graphv1.Ref{Namespace: "datadog.service", Value: "candidate-api"}, EntrySlug: "infra",
		}}
	})
	automated := g.apply(graph.AttributorSourceID, "", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_AttributeSignal{AttributeSignal: &graphv1.AttributeSignal{
			Signal:  alert,
			Entries: []*graphv1.Attributed{{EntrySlug: "infra", Layer: "alias", RuleId: "A1", Score: 1}},
			Ruleset: graph.AttributionRuleset,
		}}
	})
	beforeCorrection := g.at(g.seq)
	correction := g.apply("human", "oidc:pierre", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_CorrectAttribution{CorrectAttribution: &graphv1.CorrectAttribution{
			Signal: alert, EntrySlugs: []string{"candidate-view"}, Scope: "signal", Rationale: "wrong team",
		}}
	})

	engine := query.NewEngine(g.p.Store())
	now := &graphv1.AsOf{ValidAt: timestamppb.New(catalogStart.Add(time.Hour))}
	then := &graphv1.AsOf{ValidAt: now.GetValidAt(), ObservedAt: timestamppb.New(beforeCorrection)}

	list, err := engine.ListEntries(ctx, &graphv1.ListEntriesRequest{AsOf: now})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.GetEntries()) != 2 || list.GetEntries()[1].GetSlug() != "infra" ||
		len(list.GetEntries()[1].GetAliases()) != 1 || list.GetEntries()[1].GetAliases()[0].GetPrincipal() != "oidc:pierre" {
		t.Fatalf("entries: %v", list.GetEntries())
	}
	if _, err := engine.ListEntries(ctx, &graphv1.ListEntriesRequest{}); err == nil {
		t.Error("a catalog read without a valid instant answered")
	}

	why, err := engine.WhyAttributed(ctx, &graphv1.WhyAttributedRequest{Signal: alert, AsOf: now})
	if err != nil {
		t.Fatalf("why: %v", err)
	}
	if why.GetCurrent().GetEventId() != correction || why.GetCurrent().GetPrincipal() != "oidc:pierre" ||
		len(why.GetHistory()) != 2 || why.GetHistory()[0].GetSupersededByEventId() != correction {
		t.Fatalf("why now: %v", why)
	}
	// Before the correction, the graph believed the automated decision and knew of nothing else.
	why, err = engine.WhyAttributed(ctx, &graphv1.WhyAttributedRequest{Signal: alert, AsOf: then})
	if err != nil {
		t.Fatalf("why then: %v", err)
	}
	if why.GetCurrent().GetEventId() != automated || len(why.GetHistory()) != 1 ||
		why.GetHistory()[0].GetSupersededByEventId() != "" {
		t.Fatalf("why then: %v", why)
	}

	for slug, want := range map[string]int{"candidate-view": 1, "infra": 0} {
		signals, err := engine.AttributedSignals(ctx, &graphv1.AttributedSignalsRequest{Slug: slug, AsOf: now})
		if err != nil {
			t.Fatalf("signals of %s: %v", slug, err)
		}
		if len(signals.GetSignals()) != want {
			t.Errorf("signals of %s now: %d, want %d", slug, len(signals.GetSignals()), want)
		}
	}
	signals, err := engine.AttributedSignals(ctx, &graphv1.AttributedSignalsRequest{Slug: "infra", AsOf: then})
	if err != nil || len(signals.GetSignals()) != 1 || signals.GetSignals()[0].GetHow().GetRuleId() != "A1" ||
		signals.GetSignals()[0].GetDecisionEventId() != automated {
		t.Fatalf("signals of infra before the correction: %v %v", signals, err)
	}

	entry, err := engine.GetEntry(ctx, &graphv1.GetEntryRequest{Slug: "candidate-view", AsOf: now})
	if err != nil || entry.GetEntry().GetStatus() != "confirmed" || len(entry.GetHistory()) == 0 {
		t.Fatalf("get entry: %v %v", entry, err)
	}
}
