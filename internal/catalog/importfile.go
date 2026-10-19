// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The catalog file (contracts §5, research R10, FR-013).
//
// An operator may keep the catalog in a versioned file. Importing it is not a second source of
// truth: the import computes the difference between the file and the catalog as of now and emits
// the human decisions that close it, signed by whoever imports. Re-importing an unchanged file
// emits nothing.
//
// An import never undoes a person. A name the file gives to one entry that a person has since given
// to another is listed as a conflict and left as it is; an alias the catalog has and the file does
// not is listed, and retracted only when the operator asks for it (`--prune`).

// FileVersion is the only catalog file version.
const FileVersion = 1

// File is a parsed catalog file.
type File struct {
	Version int         `yaml:"version"`
	Entries []FileEntry `yaml:"entries"`
	Hidden  []FileAlias `yaml:"hidden"`
}

// FileEntry is one entry of the file.
type FileEntry struct {
	Slug        string      `yaml:"slug"`
	Name        string      `yaml:"name"`
	Kind        string      `yaml:"kind"`
	Owner       string      `yaml:"owner"`
	Description string      `yaml:"description"`
	Aliases     []FileAlias `yaml:"aliases"`
}

// FileAlias is a name, written `ns=value` or `{name: ns=value, environment: production}`.
type FileAlias struct {
	Name        string `yaml:"name"`
	Environment string `yaml:"environment"`
}

// UnmarshalYAML accepts both spellings of an alias.
func (a *FileAlias) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		a.Name = node.Value
		return nil
	}
	type plain FileAlias
	return node.Decode((*plain)(a))
}

// ParseFile reads and validates a catalog file.
func ParseFile(raw []byte) (File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("catalog file: %w", err)
	}
	if f.Version != FileVersion {
		return File{}, fmt.Errorf("catalog file: version %d: want %d", f.Version, FileVersion)
	}
	seenSlug := map[string]bool{}
	seenName := map[string]string{}
	claim := func(a FileAlias, owner string) error {
		ref, err := graph.ParseRef(strings.TrimSpace(a.Name))
		if err != nil {
			return fmt.Errorf("catalog file: alias %q of %s: write it as <namespace>=<value>", a.Name, owner)
		}
		key := AliasKeyOf(ref, a.Environment)
		if prev, dup := seenName[key]; dup {
			return fmt.Errorf("catalog file: %s is given to both %s and %s", key, prev, owner)
		}
		seenName[key] = owner
		return nil
	}
	for _, e := range f.Entries {
		if !graph.ValidSlug(e.Slug) {
			return File{}, fmt.Errorf("catalog file: slug %q: lower-case letters, digits and dashes", e.Slug)
		}
		if seenSlug[e.Slug] {
			return File{}, fmt.Errorf("catalog file: slug %q appears twice", e.Slug)
		}
		seenSlug[e.Slug] = true
		if strings.TrimSpace(e.Name) == "" {
			return File{}, fmt.Errorf("catalog file: entry %s has no name", e.Slug)
		}
		if err := graph.CheckCatalogKind(e.kind()); err != nil {
			return File{}, fmt.Errorf("catalog file: entry %s: %w", e.Slug, err)
		}
		for _, a := range e.Aliases {
			if err := claim(a, e.Slug); err != nil {
				return File{}, err
			}
		}
	}
	for _, a := range f.Hidden {
		if err := claim(a, "hidden"); err != nil {
			return File{}, err
		}
	}
	return f, nil
}

func (e FileEntry) kind() string {
	if e.Kind == "" {
		return graph.CatalogKindService
	}
	return e.Kind
}

// AliasKeyOf spells a name and its environment, as the plan and the conflicts list them.
func AliasKeyOf(ref graph.Ref, environment string) string {
	if environment == "" {
		return ref.String()
	}
	return ref.String() + " (" + environment + ")"
}

// Change is one decision an import emits. Exactly one of the bodies is set.
type Change struct {
	Describe string
	Entry    *graphv1.DeclareCatalogEntry
	Alias    *graphv1.DeclareAlias
	Retract  *graphv1.RetractAlias
}

// Plan is what an import would do.
type Plan struct {
	Changes []Change
	// Conflicts are names a person has since given elsewhere; the import leaves them.
	Conflicts []string
	// Extra are aliases the catalog has and the file does not; retracted only with prune.
	Extra []string
}

// PlanImport computes the difference between the file and the catalog as of now (a ListEntries
// answer). rationale is stamped on every decision; prune retracts the extra aliases of the file's
// entries.
func PlanImport(f File, current *graphv1.ListEntriesResponse, rationale string, prune bool) Plan {
	entries := map[string]*graphv1.CatalogEntry{}
	// owner maps a current alias key to its entry slug and the alias itself.
	type owned struct {
		slug  string
		alias *graphv1.CatalogAlias
	}
	owner := map[string]owned{}
	for _, e := range current.GetEntries() {
		entries[e.GetSlug()] = e
		for _, a := range e.GetAliases() {
			owner[AliasKeyOf(graph.RefFromProto(a.GetName()), a.GetEnvironment())] = owned{e.GetSlug(), a}
		}
	}
	for _, a := range current.GetHidden() {
		owner[AliasKeyOf(graph.RefFromProto(a.GetName()), a.GetEnvironment())] = owned{graph.HiddenEntrySlug, a}
	}

	var plan Plan
	want := func(a FileAlias, slug string) {
		ref, _ := graph.ParseRef(strings.TrimSpace(a.Name))
		key := AliasKeyOf(ref, a.Environment)
		have, ok := owner[key]
		switch {
		case ok && have.slug == slug:
			return
		case ok && have.alias.GetPrincipal() != "":
			plan.Conflicts = append(plan.Conflicts, fmt.Sprintf("%s: the file says %s, %s decided %s",
				key, slugLabel(slug), have.alias.GetPrincipal(), slugLabel(have.slug)))
			return
		}
		plan.Changes = append(plan.Changes, Change{
			Describe: fmt.Sprintf("alias %s → %s", key, slugLabel(slug)),
			Alias: &graphv1.DeclareAlias{
				Name: ref.Proto(), Environment: a.Environment, EntrySlug: slug, Rationale: rationale,
			},
		})
	}

	for _, e := range f.Entries {
		have := entries[e.Slug]
		if have == nil || have.GetNode().GetDisplayName() != e.Name || have.GetKind() != e.kind() ||
			have.GetOwner() != e.Owner || have.GetDescription() != e.Description ||
			have.GetStatus() != graph.CatalogStatusConfirmed {
			verb := "declare"
			if have != nil {
				verb = "update"
			}
			plan.Changes = append(plan.Changes, Change{
				Describe: fmt.Sprintf("%s entry %s (%s)", verb, e.Slug, e.Name),
				Entry: &graphv1.DeclareCatalogEntry{
					Slug: e.Slug, DisplayName: e.Name, Kind: e.kind(), Status: graph.CatalogStatusConfirmed,
					Owner: e.Owner, Description: e.Description, Rationale: rationale,
				},
			})
		}
		for _, a := range e.Aliases {
			want(a, e.Slug)
		}
	}
	for _, a := range f.Hidden {
		want(a, graph.HiddenEntrySlug)
	}

	inFile := map[string]bool{}
	for _, e := range f.Entries {
		for _, a := range e.Aliases {
			ref, _ := graph.ParseRef(strings.TrimSpace(a.Name))
			inFile[AliasKeyOf(ref, a.Environment)] = true
		}
	}
	keys := make([]string, 0, len(owner))
	for key := range owner {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		have := owner[key]
		if inFile[key] || have.slug == graph.HiddenEntrySlug || !fileHas(f, have.slug) {
			continue
		}
		plan.Extra = append(plan.Extra, fmt.Sprintf("%s → %s", key, have.slug))
		if prune {
			plan.Changes = append(plan.Changes, Change{
				Describe: fmt.Sprintf("retract alias %s → %s", key, have.slug),
				Retract: &graphv1.RetractAlias{
					Name: have.alias.GetName(), Environment: have.alias.GetEnvironment(),
					EntrySlug: have.slug, Rationale: rationale,
				},
			})
		}
	}
	return plan
}

func fileHas(f File, slug string) bool {
	return slices.ContainsFunc(f.Entries, func(e FileEntry) bool { return e.Slug == slug })
}

func slugLabel(slug string) string {
	if slug == graph.HiddenEntrySlug {
		return "hidden"
	}
	return slug
}
