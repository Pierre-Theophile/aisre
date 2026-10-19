// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"reflect"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The old maps as a catalog file (009 T030): one entry per entity, every name sent to it an alias.
func TestTheLegacyMapsAreACatalog(t *testing.T) {
	t.Parallel()
	checkout := graph.Ref{Namespace: "otel.service.name", Value: "checkout"}
	f, err := catalog.FileFromLegacy([]catalog.LegacyLine{
		catalog.NotionLegacyLine("db1", "Checkout", checkout),
		catalog.SlackLegacyLine("checkout", checkout),
		catalog.NotionLegacyLine("db1", "Payments", graph.Ref{Namespace: "otel.service.name", Value: "payments"}),
		catalog.NotionLegacyLine("db2", "Payments", graph.Ref{Namespace: "service.name", Value: "payments"}),
		catalog.SlackLegacyLine("search", graph.Ref{Namespace: graph.CatalogNamespace, Value: "search"}),
	})
	if err != nil {
		t.Fatalf("FileFromLegacy: %v", err)
	}
	want := []catalog.FileEntry{
		{Slug: "checkout", Name: "Checkout", Aliases: []catalog.FileAlias{
			{Name: "otel.service.name=checkout"}, {Name: "notion.value=db1/Checkout"}, {Name: "slack.slug=checkout"}}},
		{Slug: "payments", Name: "Payments", Aliases: []catalog.FileAlias{
			{Name: "otel.service.name=payments"}, {Name: "notion.value=db1/Payments"}}},
		{Slug: "payments-2", Name: "Payments", Aliases: []catalog.FileAlias{
			{Name: "service.name=payments"}, {Name: "notion.value=db2/Payments"}}},
		{Slug: "search", Name: "search", Aliases: []catalog.FileAlias{{Name: "slack.slug=search"}}},
	}
	if !reflect.DeepEqual(f.Entries, want) {
		t.Errorf("entries\n %+v\nwant\n %+v", f.Entries, want)
	}
}
