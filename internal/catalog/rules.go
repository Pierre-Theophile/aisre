// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"fmt"
	"maps"
	"math"
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
//	1 alias       A1  one of its names is an alias of an entry, exactly (research R2); an alias a
//	                  person decided outranks one discovery carried over (rule D1)          1.0
//	2 probable    P1 a name equals an entry's name or alias after normalisation       0.85
//	              P2 a name's tokens overlap one's, Jaccard J ≥ 0.5                   0.45 + 0.4 J
//	              P3 a watched entity runs on, depends on or calls an aliased one     0.55
//	              P4 a watched entity has the owner of an aliased one                 0.50
//	              noisy-OR across rules per entry; attributed at ≥ τ leading the best
//	              entry below τ by δ, else `unknown` with the top three as suggestions
//	3 classifier  M   optional                                               (feature 009 PR 3)
//
// Everything here is a pure function of a Signal and the catalog Facts: no database, no clock, no
// network. That is what makes a decision explainable and the evaluation reproducible.

// Rule ids.
const (
	RuleCorrection = "H"
	RuleAlias      = "A1"
	// RuleClassifier is layer 3: the configured classifier's calibrated score (research R6).
	RuleClassifier = "M"
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

// RulesFromConfig is the ruleset a model configuration's `attribution:` block names. A threshold or
// margin other than the published ones under the published ruleset's id is refused: the id is what
// every attribution records, so it must name exactly one model (FR-038).
func RulesFromConfig(ruleset string, threshold, margin *float64) (Rules, error) {
	r := DefaultRules()
	if ruleset != "" {
		r.Ruleset = ruleset
	}
	if threshold != nil {
		r.Threshold = *threshold
	}
	if margin != nil {
		r.Margin = *margin
	}
	if r.Ruleset == graph.AttributionRuleset && (r.Threshold != DefaultThreshold || r.Margin != DefaultMargin) {
		return Rules{}, fmt.Errorf("attribution: %s is published with threshold %.2f and margin %.2f; "+
			"a different threshold or margin is a different ruleset, so give it its own id (e.g. attribution/rules@1-local)",
			graph.AttributionRuleset, DefaultThreshold, DefaultMargin)
	}
	return r, nil
}

// Alias is one current alias, as the rules read it.
type Alias struct {
	Name Name
	// Slug is the entry, or graph.HiddenEntrySlug for a hidden name.
	Slug string
	// ByCorrection says a name-level correction decided it, which makes it layer 0.
	ByCorrection bool
	// Discovered says discovery carried it over from an entity's own claims (rule D1) and no person
	// decided it. An alias a person decided outranks it: a catalog someone wrote is not overruled
	// by the one the agent proposed before they did.
	Discovered bool
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
	// Entries are every entry, for the probable rules' names.
	Entries map[string]Entry
	// EntryOwners are, per entry, the owners of the entities its aliases name (rule P4). Read only
	// for a signal that has owners.
	EntryOwners map[string][]string
}

// Entry is an entry as the probable rules read it.
type Entry struct {
	Display     string
	Status      string
	Description string
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
	byDiscovery := map[string][]string{}
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
			case alias.Discovered:
				byDiscovery[alias.Slug] = append(byDiscovery[alias.Slug], name.String())
			default:
				byName[alias.Slug] = append(byName[alias.Slug], name.String())
			}
		}
	}
	if len(hidden) > 0 {
		evidence["hidden_names"] = anyList(sortedUnique(hidden))
	}
	if len(byName) == 0 && len(byDiscovery) > 0 {
		// Nobody declared an alias for these names: the attributor's own discovery decides, and the
		// evidence says so, because a discovered alias is the attributor's inference and a declared
		// one is a person's catalog (the evaluation scores them apart).
		byName = byDiscovery
		evidence[EvidenceAliasDiscovered] = true
	} else if len(byDiscovery) > 0 {
		evidence["outranked_discovered"] = matchedEvidence(byDiscovery)
	}
	if len(byName) > 0 {
		evidence[graph.AttributionLayerAlias] = matchedEvidence(byName)
	}
	if len(byCorrection) > 0 {
		evidence["decided_by"] = graph.AttributionLayerCorrection
		return Decision{Entries: matched(byCorrection, graph.AttributionLayerCorrection, RuleCorrection,
			"a person corrected the name "), Evidence: evidence}
	}
	// Layer 2 is computed whatever decides: below the deciding layer it is supporting evidence.
	hiddenNames := map[string]bool{}
	for _, h := range hidden {
		hiddenNames[h] = true
	}
	probable := r.probable(s, f, hiddenNames)
	if len(probable) > 0 {
		scores := map[string]any{}
		for _, p := range probable {
			scores[p.Slug] = p.Score
		}
		evidence[graph.AttributionLayerProbable] = scores
	}
	if len(byName) > 0 {
		evidence["decided_by"] = graph.AttributionLayerAlias
		return Decision{Entries: matched(byName, graph.AttributionLayerAlias, RuleAlias,
			"an alias of the entry: "), Evidence: evidence}
	}
	if entries := r.decideProbable(probable); len(entries) > 0 {
		evidence["decided_by"] = graph.AttributionLayerProbable
		return Decision{Entries: entries, Evidence: evidence}
	}
	evidence["decided_by"] = "unknown"
	return Decision{Suggestions: suggestions(probable), Evidence: evidence}
}

// Probable rules (layer 2, research R5), published in ADR-0015 Item 5 and confirmed on the labelled
// set by `aisre eval attribution` (009 T028).
const (
	RuleTopology = "P3"
	RuleOwner    = "P4"

	ScoreTopology = 0.55
	ScoreOwner    = 0.50

	// maxSuggestions is how many entries an `unknown` lists.
	maxSuggestions = 3
)

// EvidenceAliasDiscovered is the evidence key an alias-layer decision carries when the aliases that
// decided were the attributor's own discoveries (rule D1), not a person's.
const EvidenceAliasDiscovered = "alias_discovered"

// scored is one entry's layer-2 score, the rules that fired, and the name that weighed most.
type scored struct {
	Attributed
	via string
}

// probable scores every entry by P1–P4, combined per entry by noisy-OR across rules, best first.
func (r Rules) probable(s Signal, f Facts, hiddenNames map[string]bool) []scored {
	type sign struct {
		score float64
		via   string
	}
	signs := map[string]map[string]sign{} // slug -> rule -> best sign
	note := func(slug, rule string, score float64, via string) {
		if signs[slug] == nil {
			signs[slug] = map[string]sign{}
		}
		if cur, ok := signs[slug][rule]; !ok || score > cur.score || (score == cur.score && via < cur.via) {
			signs[slug][rule] = sign{score: score, via: via}
		}
	}
	aliasesOf := map[string][]Alias{}
	for _, a := range f.Aliases {
		aliasesOf[a.Slug] = append(aliasesOf[a.Slug], a)
	}
	owners := map[string]bool{}
	for _, o := range s.Owners {
		owners[o] = true
	}
	for slug, entry := range f.Entries {
		if slug == graph.HiddenEntrySlug || f.Hidden[slug] || entry.Status == graph.CatalogStatusHidden {
			continue
		}
		for _, name := range s.Names {
			if hiddenNames[name.String()] {
				continue
			}
			// An alias stated for one environment says nothing about a name in another (research
			// R2), not even that the two are spelled alike.
			targets := []string{entry.Display}
			for _, a := range aliasesOf[slug] {
				if a.Name.Environment == "" || a.Name.Environment == name.Environment {
					targets = append(targets, NameText(a.Name.Ref))
				}
			}
			for _, target := range sortedUnique(targets) {
				if rule, score := NameScore(NameText(name.Ref), target); score > 0 {
					note(slug, rule, score, name.String())
				}
			}
		}
		for _, related := range s.Related {
			for _, a := range aliasesOf[slug] {
				if a.matches(related) {
					note(slug, RuleTopology, ScoreTopology, related.String())
				}
			}
		}
		for _, o := range f.EntryOwners[slug] {
			if owners[o] {
				note(slug, RuleOwner, ScoreOwner, o)
			}
		}
	}

	out := make([]scored, 0, len(signs))
	for slug, rules := range signs {
		ids := make([]string, 0, len(rules))
		for rule := range rules {
			ids = append(ids, rule)
		}
		slices.Sort(ids)
		miss, best, via := 1.0, 0.0, ""
		var why []string
		for _, rule := range ids {
			sg := rules[rule]
			miss *= 1 - sg.score
			if sg.score > best {
				best, via = sg.score, sg.via
			}
			why = append(why, rule+" "+sg.via)
		}
		score := math.Round((1-miss)*1e4) / 1e4
		out = append(out, scored{Attributed: Attributed{
			Slug: slug, Layer: graph.AttributionLayerProbable, RuleID: strings.Join(ids, "+"), Score: score,
			Rationale: "probable: " + strings.Join(why, "; "),
		}, via: via})
	}
	sortScored(out)
	return out
}

// sortScored orders candidates best first, ties by slug.
func sortScored(out []scored) {
	slices.SortFunc(out, func(a, b scored) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Slug, b.Slug)
	})
}

// decideProbable applies τ and δ: every entry at or above τ is attributed when it leads the best
// entry below τ by δ, and no two of them reached τ through the same name (one name cannot be two
// services; two names can).
func (r Rules) decideProbable(probable []scored) []Attributed {
	var above []scored
	next := 0.0
	for _, p := range probable {
		if p.Score >= r.Threshold {
			above = append(above, p)
		} else if p.Score > next {
			next = p.Score
		}
	}
	if len(above) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, p := range above {
		if p.Score-next < r.Margin || seen[p.via] {
			return nil
		}
		seen[p.via] = true
	}
	out := make([]Attributed, 0, len(above))
	for _, p := range above {
		out = append(out, p.Attributed)
	}
	slices.SortFunc(out, func(a, b Attributed) int { return strings.Compare(a.Slug, b.Slug) })
	return out
}

// suggestions are the top entries an `unknown` lists.
func suggestions(probable []scored) []Attributed {
	out := make([]Attributed, 0, maxSuggestions)
	for _, p := range probable {
		if len(out) == maxSuggestions {
			break
		}
		out = append(out, p.Attributed)
	}
	return out
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

// Name matching for the probable rules (research R5): P1 compares names after normalisation, P2
// by the overlap of their tokens.
const (
	RuleNormalised = "P1"
	RuleTokens     = "P2"

	// ScoreNormalised is P1's published score.
	ScoreNormalised = 0.85
	// tokenFloor is the Jaccard index below which P2 does not fire.
	tokenFloor = 0.5
)

// trailingKinds are the deployment suffixes P1 ignores: `candidate-api` and `candidate` are the
// same service by name.
var trailingKinds = map[string]bool{"api": true, "svc": true, "service": true, "web": true, "worker": true}

// NameText is the part of a ref a person would call the service: the last segment of its value,
// so `datadog.service=production/candidate-api` reads `candidate-api` and
// `notion.value=<database>/Candidate View` reads `Candidate View`.
func NameText(ref graph.Ref) string {
	value := ref.Value
	if i := strings.LastIndex(value, "/"); i >= 0 {
		value = value[i+1:]
	}
	return value
}

// Tokens are a name's words, lower-cased, with a trailing deployment suffix dropped.
func Tokens(s string) []string {
	tokens := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if len(tokens) > 1 && trailingKinds[tokens[len(tokens)-1]] {
		tokens = tokens[:len(tokens)-1]
	}
	return tokens
}

// Normalised is a name as P1 compares it.
func Normalised(s string) string { return strings.Join(Tokens(s), "-") }

// Slugify turns a name into an entry slug, or "" when nothing of it is left.
func Slugify(s string) string {
	slug := strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}), "-")
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	if !graph.ValidSlug(slug) {
		return ""
	}
	return slug
}

// NameScore is how strongly two names say they are the same service: P1 when they are equal after
// normalisation, P2 when their tokens overlap enough, nothing otherwise.
func NameScore(a, b string) (rule string, score float64) {
	na, nb := Normalised(a), Normalised(b)
	if na == "" || nb == "" {
		return "", 0
	}
	if na == nb {
		return RuleNormalised, ScoreNormalised
	}
	j := jaccard(Tokens(a), Tokens(b))
	if j < tokenFloor {
		return "", 0
	}
	return RuleTokens, 0.45 + 0.4*j
}

func jaccard(a, b []string) float64 {
	set := map[string]int{}
	for _, t := range sortedUnique(a) {
		set[t]++
	}
	for _, t := range sortedUnique(b) {
		set[t]++
	}
	both := 0
	for _, n := range set {
		if n == 2 {
			both++
		}
	}
	if len(set) == 0 {
		return 0
	}
	return float64(both) / float64(len(set))
}

// Layer 3 (research R6, FR-034, FR-035). The classifier is asked only when layers 0–2 left a
// signal `unknown` and no name of it is hidden; its scores are decided on with the same τ and δ
// as the probable layer.

// Evidence keys of layer 3.
const (
	EvidenceClassifier      = "classifier"
	EvidenceClassifierID    = "classifier_id"
	EvidenceClassifierError = "classifier_fallback"
)

// AsksClassifier reports whether a decision is one layer 3 may change.
func AsksClassifier(d Decision) bool {
	_, hidden := d.Evidence["hidden_names"]
	return len(d.Entries) == 0 && d.Evidence["decided_by"] == "unknown" && !hidden
}

// WithClassifier is the decision once the classifier id answered with scores: attributed when one
// entry clears τ by δ, else still `unknown`, suggesting the probable layer's candidates or, when it
// had none, the classifier's. Scores for entries not in known, or hidden, are dropped.
func (r Rules) WithClassifier(d Decision, id string, scores []Classified, known map[string]Entry, hidden map[string]bool) Decision {
	evidence := maps.Clone(d.Evidence)
	evidence[EvidenceClassifierID] = id
	byScore := map[string]any{}
	var candidates []scored
	for _, s := range scores {
		if _, ok := known[s.Slug]; !ok || hidden[s.Slug] || s.Score < 0 || s.Score > 1 {
			continue
		}
		score := math.Round(s.Score*1e4) / 1e4
		byScore[s.Slug] = score
		candidates = append(candidates, scored{
			Attributed: Attributed{Slug: s.Slug, Layer: graph.AttributionLayerClassifier, RuleID: RuleClassifier,
				Score: score, Rationale: s.Rationale},
			// Every score reads the same text, so two entries over τ are one signal read two ways:
			// `unknown`, as two entries reaching τ through one name are.
			via: "classifier",
		})
	}
	evidence[EvidenceClassifier] = byScore
	sortScored(candidates)
	if entries := r.decideProbable(candidates); len(entries) > 0 {
		evidence["decided_by"] = graph.AttributionLayerClassifier
		return Decision{Entries: entries, Evidence: evidence}
	}
	out := Decision{Suggestions: d.Suggestions, Evidence: evidence}
	if len(out.Suggestions) == 0 {
		out.Suggestions = suggestions(candidates)
	}
	return out
}

// WithClassifierFailure is the decision when the classifier errored, timed out or lacked a
// credential: layers 0–2 decide, and the evidence says why layer 3 did not (FR-035).
func WithClassifierFailure(d Decision, id string, err error) Decision {
	evidence := maps.Clone(d.Evidence)
	evidence[EvidenceClassifierID] = id
	evidence[EvidenceClassifierError] = err.Error()
	return Decision{Entries: d.Entries, Suggestions: d.Suggestions, Evidence: evidence}
}

// Classified is one classifier score, as layer 3 reads it.
type Classified struct {
	Slug      string
	Score     float64
	Rationale string
}
