// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// A document attributed to a catalog entry is linked to it, and scope widens through the entry's
// aliases (009 T014, FR-022, research R7).

func TestKnowledgeScopeFollowsTheCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newCatalogGraph(t)
	service := &graphv1.Ref{Namespace: "datadog.service", Value: "production/candidate-api"}
	node := func(ref *graphv1.Ref, typ graphv1.NodeType, name string) {
		g.apply("datadog:test", "", func(env *graphv1.EventEnvelope) {
			env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
				Ref: ref, Type: typ, DisplayName: name, ValidAt: timestamppb.New(catalogStart),
			}}
		})
	}
	node(service, graphv1.NodeType_SERVICE, "candidate-api")
	attrs, _ := structpb.NewStruct(map[string]any{"deployment.environment.name": "production"})
	g.apply("datadog:test", "", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: &graphv1.CorrelateEntity{
			Subject: service, Key: &graphv1.Ref{Namespace: "datadog.log_service", Value: "candidate-api"}, Attributes: attrs,
		}}
	})
	docs := map[string]*graphv1.Ref{}
	for _, name := range []string{"filed-under-candidate-view", "filed-under-infra", "about-the-service"} {
		docs[name] = &graphv1.Ref{Namespace: "notion.page", Value: name}
		node(docs[name], graphv1.NodeType_KNOWLEDGE_DOC, name)
	}
	for _, entry := range []string{"candidate-view", "infra"} {
		g.apply("human", "oidc:pierre", func(env *graphv1.EventEnvelope) {
			env.Body = &graphv1.EventEnvelope_DeclareCatalogEntry{DeclareCatalogEntry: &graphv1.DeclareCatalogEntry{
				Slug: entry, DisplayName: entry, Kind: "service", Status: "confirmed",
			}}
		})
	}
	g.apply("human", "oidc:pierre", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_DeclareAlias{DeclareAlias: &graphv1.DeclareAlias{
			Name: &graphv1.Ref{Namespace: "datadog.log_service", Value: "candidate-api"}, EntrySlug: "candidate-view",
		}}
	})
	for doc, entry := range map[string]string{"filed-under-candidate-view": "candidate-view", "filed-under-infra": "infra"} {
		g.apply(graph.AttributorSourceID, "", func(env *graphv1.EventEnvelope) {
			env.Body = &graphv1.EventEnvelope_AttributeSignal{AttributeSignal: &graphv1.AttributeSignal{
				Signal:  docs[doc],
				Entries: []*graphv1.Attributed{{EntrySlug: entry, Layer: "alias", RuleId: "A1", Score: 1}},
				Ruleset: graph.AttributionRuleset,
			}}
		})
	}
	g.apply("datadog:test", "", func(env *graphv1.EventEnvelope) {
		env.Body = &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
			Src: docs["about-the-service"], Dst: service, Type: graphv1.EdgeType_CONCERNS, ValidAt: timestamppb.New(catalogStart),
		}}
	})

	e := query.NewEngine(g.p.Store())
	at := query.AsOf{ValidAt: g.at(g.seq), ObservedAt: g.at(g.seq)}
	inScope := func(requested string) []string {
		t.Helper()
		got, err := e.KnowledgeCandidates(ctx, []string{requested}, at)
		if err != nil {
			t.Fatalf("candidates for %s: %v", requested, err)
		}
		var out []string
		for _, c := range got {
			if !slices.Equal(c.Linked, []string{requested}) {
				t.Errorf("%s linked as %v", c.DisplayName, c.Linked)
			}
			out = append(out, c.DisplayName)
		}
		slices.Sort(out)
		return out
	}
	if got := inScope("datadog.service=production/candidate-api"); !slices.Equal(got, []string{"about-the-service", "filed-under-candidate-view"}) {
		t.Errorf("in scope for the service: %v", got)
	}
	if got := inScope("sre.catalog=candidate-view"); !slices.Equal(got, []string{"about-the-service", "filed-under-candidate-view"}) {
		t.Errorf("in scope for the entry: %v", got)
	}
	if got := inScope("sre.catalog=infra"); !slices.Equal(got, []string{"filed-under-infra"}) {
		t.Errorf("in scope for another entry: %v", got)
	}
	// Before the alias, the service's scope is its own links only.
	at = query.AsOf{ValidAt: g.at(g.seq), ObservedAt: g.at(7)}
	if got := inScope("datadog.service=production/candidate-api"); len(got) != 0 {
		t.Errorf("in scope before the alias and the attributions: %v", got)
	}
}
