// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
)

// The catalog file import (009 T015, FR-013, research R10): the difference as human decisions,
// nothing for an unchanged file, and never undoing a person.

const catalogYAML = `version: 1
entries:
  - slug: candidate-view
    name: Candidate View
    kind: service
    owner: candidate team
    aliases:
      - datadog.log_service=candidate-api
      - { name: otel.service.name=candidate-api, environment: production }
  - slug: infra
    name: Infra
    kind: platform
hidden:
  - notion.value=3fa18ebd99c281eca8fcee88402babed/Other
`

func describe(plan catalog.Plan) string {
	var out []string
	for _, c := range plan.Changes {
		out = append(out, c.Describe)
	}
	return strings.Join(out, "\n")
}

func TestAnImportDeclaresTheDifferenceOnly(t *testing.T) {
	t.Parallel()
	f, err := catalog.ParseFile([]byte(catalogYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan := catalog.PlanImport(f, &graphv1.ListEntriesResponse{}, "imported", false)
	want := `declare entry candidate-view (Candidate View)
alias datadog.log_service=candidate-api → candidate-view
alias otel.service.name=candidate-api (production) → candidate-view
declare entry infra (Infra)
alias notion.value=3fa18ebd99c281eca8fcee88402babed/Other → hidden`
	if got := describe(plan); got != want {
		t.Fatalf("plan from empty:\n%s\nwant:\n%s", got, want)
	}

	// The catalog after that import: re-importing emits nothing.
	alias := func(ns, value, env, principal string) *graphv1.CatalogAlias {
		return &graphv1.CatalogAlias{Name: &graphv1.Ref{Namespace: ns, Value: value}, Environment: env, Principal: principal}
	}
	current := &graphv1.ListEntriesResponse{
		Entries: []*graphv1.CatalogEntry{
			{Slug: "candidate-view", Node: &graphv1.NodeVersion{DisplayName: "Candidate View"}, Kind: "service",
				Status: "confirmed", Owner: "candidate team", Aliases: []*graphv1.CatalogAlias{
					alias("datadog.log_service", "candidate-api", "", "oidc:ops"),
					alias("otel.service.name", "candidate-api", "production", "oidc:ops"),
				}},
			{Slug: "infra", Node: &graphv1.NodeVersion{DisplayName: "Infra"}, Kind: "platform", Status: "confirmed"},
		},
		Hidden: []*graphv1.CatalogAlias{alias("notion.value", "3fa18ebd99c281eca8fcee88402babed/Other", "", "oidc:ops")},
	}
	if plan := catalog.PlanImport(f, current, "imported", true); len(plan.Changes)+len(plan.Conflicts)+len(plan.Extra) != 0 {
		t.Fatalf("an unchanged file planned %+v", plan)
	}

	// A person moved a name and added one: the move is a conflict the import leaves, the addition
	// is listed, and retracted only with prune.
	current.Entries[0].Aliases[0] = alias("datadog.log_service", "candidate-api-v2", "", "oidc:pierre")
	current.Entries[1].Aliases = []*graphv1.CatalogAlias{alias("otel.service.name", "candidate-api", "production", "oidc:pierre")}
	current.Entries[0].Aliases = current.Entries[0].Aliases[:1]
	plan = catalog.PlanImport(f, current, "imported", false)
	if len(plan.Conflicts) != 1 || !strings.Contains(plan.Conflicts[0], "oidc:pierre decided infra") {
		t.Errorf("conflicts = %v", plan.Conflicts)
	}
	if len(plan.Extra) != 1 || plan.Extra[0] != "datadog.log_service=candidate-api-v2 → candidate-view" {
		t.Errorf("extra = %v", plan.Extra)
	}
	if got := describe(plan); got != "alias datadog.log_service=candidate-api → candidate-view" {
		t.Errorf("changes = %q", got)
	}
	if got := describe(catalog.PlanImport(f, current, "imported", true)); !strings.Contains(got, "retract alias datadog.log_service=candidate-api-v2") {
		t.Errorf("prune did not retract: %q", got)
	}
}

func TestACatalogFileIsValidated(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"version":     "version: 2\n",
		"slug":        "version: 1\nentries: [{slug: Candidate View, name: x}]\n",
		"kind":        "version: 1\nentries: [{slug: a, name: A, kind: team}]\n",
		"twice":       "version: 1\nentries: [{slug: a, name: A, aliases: [x.y=z]}, {slug: b, name: B, aliases: [x.y=z]}]\n",
		"alias":       "version: 1\nentries: [{slug: a, name: A, aliases: [nonsense]}]\n",
		"unknown key": "version: 1\nentries: [{slug: a, name: A, team: x}]\n",
	} {
		if _, err := catalog.ParseFile([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
