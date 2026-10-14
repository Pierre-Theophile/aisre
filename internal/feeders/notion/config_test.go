// SPDX-License-Identifier: Apache-2.0

package notion_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/feeders/notion"
)

func TestConfigurationRefusals(t *testing.T) {
	base := "workspace: acme\nprincipal: oidc:riley\n"
	db := func(body string) string {
		return base + "databases:\n  - database: 0a1b2c3d4e5f60718293a4b5c6d7e8f9\n" + body
	}
	for _, tc := range []struct{ name, yaml, want string }{
		{"no workspace", "principal: x\ndatabases: []\n", "workspace is required"},
		{"a workspace with a colon", "workspace: a:b\nprincipal: x\n", "may not contain"},
		{"no principal", "workspace: acme\n", "principal is required"},
		{"no databases", base, "no databases"},
		{"an unpublished kind", db("    kind: decision\n    entity_property: S\n    entities: {A: k8s.service=a}\n"), "kind \"decision\""},
		{"no entity property", db("    kind: runbook\n    entities: {A: k8s.service=a}\n"), "entity_property is required"},
		{"an empty map", db("    kind: runbook\n    entity_property: S\n"), "entities is empty"},
		{"four summary properties", db("    kind: runbook\n    entity_property: S\n    entities: {A: k8s.service=a}\n" +
			"    summary_properties: [a, b, c, d]\n"), "at most 3"},
		{"a bad entity ref", db("    kind: runbook\n    entity_property: S\n    entities: {A: checkout}\n"), "entities[\"A\"]"},
		{"an unknown field", base + "databases: []\nsearch_everything: true\n", "search_everything"},
		{"the same database twice", base + "databases:\n" +
			"  - {database: 0a1b2c3d4e5f60718293a4b5c6d7e8f9, kind: runbook, entity_property: S, entities: {A: k8s.service=a}}\n" +
			"  - {database: 0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9, kind: runbook, entity_property: S, entities: {A: k8s.service=a}}\n",
			"bound twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := notion.ParseConfig([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestBindingMapsValuesAndReportsTheRest(t *testing.T) {
	cfg, err := notion.ParseConfig([]byte("workspace: acme\nprincipal: x\ndatabases:\n" +
		"  - database: 0a1b2c3d4e5f60718293a4b5c6d7e8f9\n    kind: postmortem\n    entity_property: Services\n" +
		"    entities: {Checkout: k8s.service=checkout, Checkout API: k8s.service=checkout, Payments: k8s.service=payments}\n"))
	if err != nil {
		t.Fatal(err)
	}
	b := &cfg.Databases[0]
	if b.Database != "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9" {
		t.Errorf("database id %q, want the dashed form Notion returns", b.Database)
	}
	refs, unmapped := b.Map([]string{"Checkout", "Checkout API", "Search", "Payments"})
	if len(refs) != 2 || refs[0].GetValue() != "checkout" || refs[1].GetValue() != "payments" {
		t.Errorf("refs %v: two values naming one entity are one link", refs)
	}
	if len(unmapped) != 1 || unmapped[0] != "Search" {
		t.Errorf("unmapped %v", unmapped)
	}
	if cfg.PollInterval != notion.DefaultPollInterval || cfg.ReconcileEvery != notion.DefaultReconcileEvery {
		t.Errorf("defaults %s %s", cfg.PollInterval, cfg.ReconcileEvery)
	}
}
