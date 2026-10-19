// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// Discovery end to end (009 T021, research R9): an empty catalog fills itself with proposals, a
// proposal is raised once, a hidden name is never proposed, and accepting a suggestion is an alias.

const notionDB = "3fa18ebd99c281eca8fcee88402babed"

// doc asserts a Notion page carrying entity-property values.
func (w *world) doc(id string, values ...string) {
	list := make([]any, 0, len(values))
	for _, v := range values {
		list = append(list, v)
	}
	props, err := structpb.NewStruct(map[string]any{
		graph.NotionDatabaseProp: notionDB, graph.NotionValuesProp: list,
	})
	if err != nil {
		w.t.Fatalf("props: %v", err)
	}
	w.apply("datadog:test", "", func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref: ref("notion.page", id), Type: graphv1.NodeType_KNOWLEDGE_DOC, DisplayName: "Postmortem " + id,
			Props: props, ValidAt: timestamppb.New(w.at),
		}}
	})
}

// catalogState is the entries as "slug:status", and the alias proposals as "name|env|slug:rule:status".
func (w *world) catalogState() (entries, proposals string) {
	w.t.Helper()
	ctx := context.Background()
	list, err := query.NewEngine(w.p.Store()).ListEntries(ctx, &graphv1.ListEntriesRequest{
		AsOf: &graphv1.AsOf{ValidAt: timestamppb.New(w.at)},
	})
	if err != nil {
		w.t.Fatalf("list entries: %v", err)
	}
	var e []string
	for _, entry := range list.GetEntries() {
		e = append(e, entry.GetSlug()+":"+entry.GetStatus())
	}
	rows, err := w.p.Store().Pool().Query(ctx,
		`SELECT proposal_key || ':' || rule_id || ':' || status FROM graph.alias_proposals ORDER BY proposal_key`)
	if err != nil {
		w.t.Fatalf("read proposals: %v", err)
	}
	defer rows.Close()
	var p []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		p = append(p, s)
	}
	slices.Sort(e)
	return strings.Join(e, " "), strings.Join(p, " ")
}

func TestDiscoveryProposesTheCatalogOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t, catalog.WithDiscovery(true))
	w.service("candidate-api", "production")
	w.service("candidate-api", "staging")
	w.alert("1", "candidate-api", "production")
	w.doc("p1", "Candidate View")
	// A person says "Other" is no service before discovery ever sees it.
	w.alias(ref(graph.NotionValueNamespace, notionDB+"/Other"), "", graph.HiddenEntrySlug)
	w.doc("p2", "Other")

	w.sweep()
	entries, proposals := w.catalogState()
	if want := "candidate-api:proposed candidate-view:proposed"; entries != want {
		t.Errorf("entries %q, want %q", entries, want)
	}
	// The staging service shares the slug: P1 proposes its names for the entry. The production
	// service's entry and the Notion value are the same service by their tokens: P2.
	wantProposals := strings.Join([]string{
		"datadog.service=production/candidate-api||candidate-view:P2:pending",
		"datadog.service=staging/candidate-api||candidate-api:P1:pending",
	}, " ")
	if proposals != wantProposals {
		t.Errorf("proposals\n %q\nwant\n %q", proposals, wantProposals)
	}
	if got := w.attributed("1"); got != "candidate-api/A1" {
		t.Errorf("a signal on a proposed entry's name: %q", got)
	}

	// Raised once.
	if n := w.sweep(); n != 0 {
		t.Errorf("a second sweep appended %d events", n)
	}

	// A rejected pair stays rejected; accepting the other is an alias like any other.
	w.apply("human", person, func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_RejectAlias{RejectAlias: &graphv1.RejectAlias{
			Name: ref("datadog.service", "staging/candidate-api"), EntrySlug: "candidate-api"}}
	})
	w.alias(ref("datadog.service", "production/candidate-api"), "", "candidate-view")
	w.sweep()
	if _, proposals = w.catalogState(); proposals != strings.Join([]string{
		"datadog.service=production/candidate-api||candidate-view:P2:accepted",
		"datadog.service=staging/candidate-api||candidate-api:P1:rejected",
	}, " ") {
		t.Errorf("after the decisions: %q", proposals)
	}
	if got := w.attributed("1"); got != "candidate-view/A1" {
		t.Errorf("after accepting the suggestion: %q", got)
	}
}

func TestAPersonsAliasOutranksADiscoveredOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t, catalog.WithDiscovery(true))
	w.service("candidate-api", "production")
	w.alert("1", "candidate-api", "production")
	w.sweep()
	if got := w.attributed("1"); got != "candidate-api/A1" {
		t.Fatalf("before the catalog is written: %q", got)
	}
	w.entry("infra", "Infra", "confirmed")
	w.alias(ref("datadog.log_service", "candidate-api"), "", "infra")
	w.sweep()
	if got := w.attributed("1"); got != "infra/A1" {
		t.Errorf("a person's alias did not outrank the discovered one: %q", got)
	}
}

func TestNameScores(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b  string
		rule  string
		score float64
	}{
		{"candidate-api", "Candidate", catalog.RuleNormalised, 0.85},
		{"candidate_api", "candidate-svc", catalog.RuleNormalised, 0.85},
		{"candidate-api", "Candidate View", catalog.RuleTokens, 0.65},
		{"candidate-view-web", "Candidate View", catalog.RuleNormalised, 0.85},
		{"checkout", "Candidate View", "", 0},
		{"api", "service", "", 0},
	} {
		rule, score := catalog.NameScore(c.a, c.b)
		if rule != c.rule || (score-c.score) > 1e-9 || (c.score-score) > 1e-9 {
			t.Errorf("NameScore(%q, %q) = %s %.4f, want %s %.4f", c.a, c.b, rule, score, c.rule, c.score)
		}
	}
	if got := catalog.Slugify("Candidate View"); got != "candidate-view" {
		t.Errorf("Slugify: %q", got)
	}
}
