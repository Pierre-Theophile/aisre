// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The legacy maps (009 T030, FR-006).
//
// Before the catalog, two connectors mapped their own names to graph entities in their own
// configuration: 007's Notion `entities` map (a property value to an entity) and 006's Slack
// `--service` flags (a channel-slug token to an entity). Each line says that a connector's name and
// an entity are one service, which is exactly what an entry with two aliases says. FileFromLegacy
// turns the lines into a catalog file, so importing it goes through the same plan as any file: the
// difference with the catalog as of now, signed by whoever imports, never undoing a person.

// LegacyLine is one line of an old map: a connector's name for a service, how people spell it, and
// the entity the map sent it to.
type LegacyLine struct {
	Name    graph.Ref
	Display string
	Target  graph.Ref
}

// NotionLegacyLine is a line of a Notion binding's `entities` map: the value, as the attributor
// names a page by it, to the entity.
func NotionLegacyLine(database, value string, target graph.Ref) LegacyLine {
	return LegacyLine{Name: NotionValueRef(database, value), Display: value, Target: target}
}

// SlackLegacyLine is a `--service <token>=<ns>=<value>` flag.
func SlackLegacyLine(token string, target graph.Ref) LegacyLine {
	return LegacyLine{Name: graph.Ref{Namespace: graph.SlackSlugNamespace, Value: token}, Display: token, Target: target}
}

// FileFromLegacy is the catalog the lines describe: one entry per target entity, named by the
// first of its spellings in sorted order, whose aliases are the target and every name sent to it.
// A target already in the catalog's namespace is that entry, and only the names are aliased to it.
func FileFromLegacy(lines []LegacyLine) (File, error) {
	byTarget := map[string][]LegacyLine{}
	for _, l := range lines {
		key := l.Target.String()
		byTarget[key] = append(byTarget[key], l)
	}
	targets := make([]string, 0, len(byTarget))
	for t := range byTarget {
		targets = append(targets, t)
	}
	slices.Sort(targets)

	f := File{Version: FileVersion}
	taken := map[string]bool{}
	for _, t := range targets {
		group := byTarget[t]
		slices.SortFunc(group, func(a, b LegacyLine) int { return strings.Compare(a.Display, b.Display) })
		entry := FileEntry{Name: group[0].Display}
		target := group[0].Target
		if target.Namespace == graph.CatalogNamespace {
			entry.Slug = target.Value
		} else {
			entry.Slug = freeSlug(Slugify(entry.Name), taken)
			entry.Aliases = append(entry.Aliases, FileAlias{Name: target.String()})
		}
		if !graph.ValidSlug(entry.Slug) {
			return File{}, fmt.Errorf("catalog: %q makes no entry identifier; name the entry in a catalog file instead", entry.Name)
		}
		taken[entry.Slug] = true
		seen := map[string]bool{}
		for _, l := range group {
			if n := l.Name.String(); !seen[n] {
				seen[n] = true
				entry.Aliases = append(entry.Aliases, FileAlias{Name: n})
			}
		}
		f.Entries = append(f.Entries, entry)
	}
	return f, nil
}

// freeSlug is slug, or slug-2, slug-3… when an earlier entry of the same import took it.
func freeSlug(slug string, taken map[string]bool) string {
	if !taken[slug] {
		return slug
	}
	for i := 2; ; i++ {
		if s := fmt.Sprintf("%s-%d", slug, i); !taken[s] {
			return s
		}
	}
}
