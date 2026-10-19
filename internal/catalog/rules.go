// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The decision model, `attribution/rules@1` (research R5, ADR-0015 item 5).
//
// Layers, in order; the first that decides wins:
//
//	0 correction  H   a person corrected this signal, or one of its names (a name-level correction
//	                  is an alias the correction decided)                                  1.0
//	1 alias       A1  one of its names is an alias of an entry, exactly (research R2)      1.0
//	2 probable    P1–P4, noisy-OR per entry                                  (feature 009 PR 2)
//	3 classifier  M   optional                                               (feature 009 PR 3)
//
// Everything here is a pure function of a Signal and the catalog Facts: no database, no clock, no
// network. That is what makes a decision explainable and the evaluation reproducible.

// Rule ids.
const (
	RuleCorrection = "H"
	RuleAlias      = "A1"
)

// Default threshold and margin (ADR-0015 item 5). A change to either is a new ruleset version.
const (
	DefaultThreshold = 0.80
	DefaultMargin    = 0.10
)

// Rules is one published ruleset and its parameters.
type Rules struct {
	Ruleset   string
	Threshold float64
	Margin    float64
}

// DefaultRules is `attribution/rules@1` with its published parameters.
func DefaultRules() Rules {
	return Rules{Ruleset: graph.AttributionRuleset, Threshold: DefaultThreshold, Margin: DefaultMargin}
}

// Alias is one current alias, as the rules read it.
type Alias struct {
	Name Name
	// Slug is the entry, or graph.HiddenEntrySlug for a hidden name.
	Slug string
	// ByCorrection says a name-level correction decided it, which makes it layer 0.
	ByCorrection bool
}

// Facts is the part of the catalog a decision reads.
type Facts struct {
	// Corrected says a signal-level correction stands; Correction is its entries, possibly none
	// ("this is about no catalog service").
	Corrected  bool
	Correction []string
	// Aliases are every current alias.
	Aliases []Alias
	// Hidden are the entries whose status is hidden: never attributed to.
	Hidden map[string]bool
}

// Attributed is one entry of a decision.
type Attributed struct {
	Slug      string
	Layer     string
	RuleID    string
	Score     float64
	Rationale string
}

// Decision is what the rules decided about one signal: the entries, or none (`unknown`), the
// suggestions below the threshold, and the evidence each layer saw.
type Decision struct {
	Entries     []Attributed
	Suggestions []Attributed
	Evidence    map[string]any
}

// Decide runs the layers in order.
func (r Rules) Decide(s Signal, f Facts) Decision {
	evidence := map[string]any{"names": nameStrings(s.Names)}
	if f.Corrected {
		entries := make([]Attributed, 0, len(f.Correction))
		for _, slug := range sortedUnique(f.Correction) {
			entries = append(entries, Attributed{
				Slug: slug, Layer: graph.AttributionLayerCorrection, RuleID: RuleCorrection, Score: 1,
				Rationale: "a person corrected this signal",
			})
		}
		evidence["decided_by"] = graph.AttributionLayerCorrection
		return Decision{Entries: entries, Evidence: evidence}
	}

	byName, byCorrection, hidden := map[string][]string{}, map[string][]string{}, []string{}
	for _, name := range s.Names {
		for _, alias := range f.Aliases {
			if !alias.matches(name) {
				continue
			}
			switch {
			case alias.Slug == graph.HiddenEntrySlug:
				hidden = append(hidden, name.String())
			case f.Hidden[alias.Slug]:
				hidden = append(hidden, name.String())
			case alias.ByCorrection:
				byCorrection[alias.Slug] = append(byCorrection[alias.Slug], name.String())
			default:
				byName[alias.Slug] = append(byName[alias.Slug], name.String())
			}
		}
	}
	if len(hidden) > 0 {
		evidence["hidden_names"] = anyList(sortedUnique(hidden))
	}
	if len(byName) > 0 {
		evidence[graph.AttributionLayerAlias] = matchedEvidence(byName)
	}
	if len(byCorrection) > 0 {
		evidence["decided_by"] = graph.AttributionLayerCorrection
		return Decision{Entries: matched(byCorrection, graph.AttributionLayerCorrection, RuleCorrection,
			"a person corrected the name "), Evidence: evidence}
	}
	if len(byName) > 0 {
		evidence["decided_by"] = graph.AttributionLayerAlias
		return Decision{Entries: matched(byName, graph.AttributionLayerAlias, RuleAlias,
			"an alias of the entry: "), Evidence: evidence}
	}
	evidence["decided_by"] = "unknown"
	return Decision{Evidence: evidence}
}

// matches applies research R2: namespace and value exactly, and the environment when the alias
// states one. An alias without one matches every environment; an alias with one does not match a
// name that states none.
func (a Alias) matches(n Name) bool {
	if a.Name.Ref != n.Ref {
		return false
	}
	return a.Name.Environment == "" || a.Name.Environment == n.Environment
}

func matched(bySlug map[string][]string, layer, rule, why string) []Attributed {
	slugs := make([]string, 0, len(bySlug))
	for slug := range bySlug {
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)
	out := make([]Attributed, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, Attributed{
			Slug: slug, Layer: layer, RuleID: rule, Score: 1,
			Rationale: why + strings.Join(sortedUnique(bySlug[slug]), ", "),
		})
	}
	return out
}

func matchedEvidence(bySlug map[string][]string) map[string]any {
	out := make(map[string]any, len(bySlug))
	for slug, names := range bySlug {
		out[slug] = anyList(sortedUnique(names))
	}
	return out
}

func nameStrings(names []Name) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, n.String())
	}
	return out
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// anyList is a string list in the shape google.protobuf.Struct accepts.
func anyList(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
