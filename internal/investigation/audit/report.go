// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// The audit's published result (T012, FR-069, FR-069a, FR-070, SC-022).
//
// Two renderings, one computation. The JSON is what `compare`, the ceiling guard, the corpus-gap
// detector and the ledger's π₀ all read; the Markdown is what a person reads and what gets
// checked in beside it. Neither is derived from the other and both come from the same Result, so
// a number cannot differ between them.
//
// Four properties are contract:
//
//   - **The ceiling is published with the count it rests on** (FR-069). `0.615385` alone is a
//     statistic; `8 of 13` is a measurement. Every rendering carries both.
//   - **`symptom_only` is reported apart from the ceiling.** The effect being visible is not the
//     cause being present. Folding it in would publish a recall bound no engine could reach;
//     dropping it would hide the difference between a feeder that adds localisation and a feeder
//     that adds causes.
//   - **Undecidable leaves the denominator.** The ceiling is observed / classifiable, and an
//     incident nobody could classify is not a miss — it is an absence of measurement, counted
//     and reported as such.
//   - **Every feeder set is reported, not only the best one.** FR-071a asks for a movement
//     attributed to the feeders that were added, and that is only possible if each rung of the
//     cumulative ladder carries its own ceiling.

// Result is one audit run, in the shape every downstream consumer reads.
type Result struct {
	// Version is the result format version, which moves with ListVersion.
	Version int `json:"version"`
	// AuditID names the run. π₀ is recorded against it in every investigation (ADR-0005 D9).
	AuditID string `json:"audit_id"`
	// RunAt is when the audit was carried out.
	RunAt Instant `json:"run_at"`
	// Author is the principal who carried it out.
	Author string `json:"author"`
	// InputDigest is the SHA-256 of the canonical incident list, so two runs can be shown to
	// have been measured over the same list (FR-071a).
	InputDigest string `json:"input_digest"`
	// Provenance says how this result was produced: ProvenanceComputed for a run of
	// `audit coverage` over an incident list, ProvenanceTranscribed for an aggregate carried
	// over from a published document whose incident list is not in this repository. The first
	// audit is a published result rather than a procedure still to be run (FR-069a), and a
	// reader has to be able to tell which kind of row they are looking at.
	Provenance string `json:"provenance,omitempty"`
	// Note accompanies a transcribed result: what it carries, what it does not, and how to
	// regenerate it. It is about the audit run, never about an incident.
	Note string `json:"note,omitempty"`
	// Corpus is what was audited and what was left out.
	Corpus CorpusSummary `json:"corpus"`
	// FeederSetInForce names the feeder set whose ceiling is the published one — the highest
	// rung of the ladder unless the caller selected another.
	FeederSetInForce string `json:"feeder_set_in_force"`
	// Ceiling is that feeder set's ceiling: observed / classifiable, rounded to six decimals.
	Ceiling float64 `json:"ceiling"`
	// Prior is π₀ = 1 − Ceiling, the prior of *no observed change explains this*
	// (ADR-0005 D9, FR-019a).
	Prior float64 `json:"prior"`
	// IncidentCount is how many incidents the list carried.
	IncidentCount int `json:"incident_count"`
	// ClassifiableCount is how many of them the ceiling rests on — the count FR-069 requires
	// be published with it.
	ClassifiableCount int `json:"classifiable_count"`
	// FeederSets is every rung of the cumulative ladder, in order.
	FeederSets []FeederSetResult `json:"feeder_sets"`
	// Remainder is the unobservable remainder under the feeder set in force: the categories
	// on which the correct engine answer is `unobserved` or `not_change_induced`, each of
	// which owes the corpus a fixture (FR-071b).
	Remainder []CategoryCount `json:"remainder"`
	// Items is the per-incident detail. It is omitted from a published aggregate: incident
	// level detail stays out of this repository (FR-069a).
	Items []Item `json:"items,omitempty"`
}

// CorpusSummary is the audited period and the exclusions that produced the count.
type CorpusSummary struct {
	// Label names the corpus.
	Label string `json:"label"`
	// From and To bound the period.
	From Instant `json:"from"`
	To   Instant `json:"to"`
	// Listed is how many incidents the list carried.
	Listed int `json:"listed"`
	// ExcludedCount is how many incidents of the period were left out.
	ExcludedCount int `json:"excluded_count"`
	// Excluded counts the exclusions by reason.
	Excluded []ExclusionCount `json:"excluded"`
}

// ExclusionCount is how many incidents were excluded for one reason.
type ExclusionCount struct {
	// Reason is the coded exclusion reason.
	Reason ExclusionReason `json:"reason"`
	// Count is how many incidents carried it.
	Count int `json:"count"`
}

// FeederSetResult is one rung of the cumulative ladder with its own ceiling.
type FeederSetResult struct {
	// Name identifies the set.
	Name string `json:"name"`
	// Order is its place in the cumulative sequence.
	Order int `json:"order"`
	// Adds are the feeders this set adds to the one below it.
	Adds []string `json:"adds"`
	// Feeders is the full cumulative configuration of this set.
	Feeders []string `json:"feeders"`
	// Classifiable is the ceiling's denominator: every incident that is not undecidable.
	Classifiable int `json:"classifiable"`
	// Observed, SymptomOnly, NotObservable and Undecidable are the verdict counts.
	Observed      int `json:"observed"`
	SymptomOnly   int `json:"symptom_only"`
	NotObservable int `json:"not_observable"`
	Undecidable   int `json:"undecidable"`
	// Ceiling is Observed / Classifiable, rounded to six decimals.
	Ceiling float64 `json:"ceiling"`
	// SymptomOnlyShare and RemainderShare are the other two columns of the published table,
	// over the same denominator.
	SymptomOnlyShare float64 `json:"symptom_only_share"`
	RemainderShare   float64 `json:"remainder_share"`
	// Prior is π₀ under this feeder set.
	Prior float64 `json:"prior"`
	// Breakdown is every category present, with its verdict counts.
	Breakdown []CategoryBreakdown `json:"breakdown"`
	// Missing ranks the categories of the absent causes by count — the ranking of the feeders
	// worth writing next (FR-070).
	Missing []CategoryCount `json:"missing"`
	// Remainder is the unobservable part of Missing: the categories that no feeder in any set
	// would have caught (FR-071b).
	Remainder []CategoryCount `json:"remainder"`
}

// CategoryBreakdown is one category's verdict counts under one feeder set.
type CategoryBreakdown struct {
	// Category is the cause category.
	Category Category `json:"category"`
	// Total is how many incidents carried it.
	Total int `json:"total"`
	// Observed, SymptomOnly, NotObservable and Undecidable are its verdict counts.
	Observed      int `json:"observed"`
	SymptomOnly   int `json:"symptom_only"`
	NotObservable int `json:"not_observable"`
	Undecidable   int `json:"undecidable"`
}

// CategoryCount is a category with a count and its share of the classifiable set.
type CategoryCount struct {
	// Category is the cause category.
	Category Category `json:"category"`
	// Classes are the cause classes the incidents of this category carried, which is what a
	// fixture for it must assert as ground truth (FR-071b).
	Classes []CauseClass `json:"classes,omitempty"`
	// Count is how many incidents.
	Count int `json:"count"`
	// Share is Count over the classifiable set, rounded to six decimals.
	Share float64 `json:"share"`
}

// Provenance values for Result.Provenance.
const (
	// ProvenanceComputed is a result computed by this command from an incident list.
	ProvenanceComputed = "computed"
	// ProvenanceTranscribed is an aggregate transcribed from a published document, for an
	// audit whose incident list is private and therefore not in this repository.
	ProvenanceTranscribed = "transcribed"
)

// Options steer a single audit run.
type Options struct {
	// FeederSet selects the feeder set whose ceiling is published. Empty means the highest
	// rung of the ladder — the best realistic configuration.
	FeederSet string
	// AggregateOnly drops the per-incident items from the Result, which is how a published
	// audit keeps incident-level detail out of this repository (FR-069a).
	AggregateOnly bool
}

// Run classifies the list and computes the published result.
func Run(list *List, opts Options) (*Result, error) {
	if list == nil {
		return nil, fmt.Errorf("audit: run: no incident list")
	}
	inForce := opts.FeederSet
	if inForce == "" {
		ordered := list.orderedFeederSets()
		inForce = ordered[len(ordered)-1].Name
	}
	if !slices.Contains(list.feederSetNames(), inForce) {
		return nil, fmt.Errorf("audit: feeder set %q is not declared by this list (declared: %s)",
			inForce, oneOf(list.feederSetNames()))
	}

	digest, err := list.Digest()
	if err != nil {
		return nil, err
	}

	result := &Result{
		Version:          ListVersion,
		AuditID:          list.AuditID,
		RunAt:            list.RunAt,
		Author:           list.Author,
		InputDigest:      digest,
		Provenance:       ProvenanceComputed,
		Corpus:           summarizeCorpus(list),
		FeederSetInForce: inForce,
		IncidentCount:    len(list.Incidents),
	}

	for _, set := range list.orderedFeederSets() {
		items, err := Classify(list, set.Name)
		if err != nil {
			return nil, err
		}
		summary := summarizeFeederSet(list, set, items)
		result.FeederSets = append(result.FeederSets, summary)
		if set.Name == inForce {
			result.Ceiling = summary.Ceiling
			result.Prior = summary.Prior
			result.ClassifiableCount = summary.Classifiable
			result.Remainder = summary.Remainder
			if !opts.AggregateOnly {
				result.Items = items
			}
		}
	}
	return result, nil
}

func summarizeCorpus(list *List) CorpusSummary {
	counts := map[ExclusionReason]int{}
	for _, excluded := range list.Corpus.Excluded {
		counts[excluded.Reason]++
	}
	summary := CorpusSummary{
		Label:         list.Corpus.Label,
		From:          list.Corpus.From,
		To:            list.Corpus.To,
		Listed:        len(list.Incidents),
		ExcludedCount: len(list.Corpus.Excluded),
		Excluded:      []ExclusionCount{},
	}
	for _, reason := range exclusionOrder {
		if counts[reason] > 0 {
			summary.Excluded = append(summary.Excluded,
				ExclusionCount{Reason: reason, Count: counts[reason]})
		}
	}
	return summary
}

func summarizeFeederSet(list *List, set FeederSet, items []Item) FeederSetResult {
	summary := FeederSetResult{
		Name:      set.Name,
		Order:     set.Order,
		Adds:      slices.Clone(set.Feeders),
		Feeders:   list.cumulativeFeeders(set.Name),
		Missing:   []CategoryCount{},
		Remainder: []CategoryCount{},
	}

	byCategory := map[Category]*CategoryBreakdown{}
	missing := map[Category]int{}
	remainder := map[Category]int{}
	remainderClasses := map[Category][]CauseClass{}
	missingClasses := map[Category][]CauseClass{}

	for _, item := range items {
		breakdown, ok := byCategory[item.Category]
		if !ok {
			breakdown = &CategoryBreakdown{Category: item.Category}
			byCategory[item.Category] = breakdown
		}
		breakdown.Total++

		switch item.Verdict {
		case VerdictObserved:
			summary.Observed++
			breakdown.Observed++
		case VerdictSymptomOnly:
			summary.SymptomOnly++
			breakdown.SymptomOnly++
			missing[item.Category]++
			missingClasses[item.Category] = appendClass(missingClasses[item.Category], item.Class)
		case VerdictNotObservable:
			summary.NotObservable++
			breakdown.NotObservable++
			missing[item.Category]++
			missingClasses[item.Category] = appendClass(missingClasses[item.Category], item.Class)
			remainder[item.Category]++
			remainderClasses[item.Category] = appendClass(remainderClasses[item.Category], item.Class)
		case VerdictUndecidable:
			summary.Undecidable++
			breakdown.Undecidable++
		}
	}

	summary.Classifiable = summary.Observed + summary.SymptomOnly + summary.NotObservable
	summary.Ceiling = share(summary.Observed, summary.Classifiable)
	summary.SymptomOnlyShare = share(summary.SymptomOnly, summary.Classifiable)
	summary.RemainderShare = share(summary.NotObservable, summary.Classifiable)
	summary.Prior = PriorFromCeiling(summary.Ceiling)

	for _, category := range categoryOrder {
		if breakdown, ok := byCategory[category]; ok {
			summary.Breakdown = append(summary.Breakdown, *breakdown)
		}
	}
	summary.Missing = rankCategories(missing, missingClasses, summary.Classifiable)
	summary.Remainder = rankCategories(remainder, remainderClasses, summary.Classifiable)
	return summary
}

func appendClass(classes []CauseClass, class CauseClass) []CauseClass {
	if slices.Contains(classes, class) {
		return classes
	}
	return append(classes, class)
}

// rankCategories orders categories by count, descending, ties broken by the published order, so
// that the list reads as a ranking of the feeders worth writing next (FR-070).
func rankCategories(counts map[Category]int, classes map[Category][]CauseClass, denominator int) []CategoryCount {
	out := make([]CategoryCount, 0, len(counts))
	for category, count := range counts {
		ranked := slices.Clone(classes[category])
		slices.SortStableFunc(ranked, func(a, b CauseClass) int {
			return slices.Index(causeClassOrder, a) - slices.Index(causeClassOrder, b)
		})
		out = append(out, CategoryCount{
			Category: category,
			Classes:  ranked,
			Count:    count,
			Share:    share(count, denominator),
		})
	}
	slices.SortStableFunc(out, func(a, b CategoryCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return a.Category.rank() - b.Category.rank()
	})
	return out
}

// share is count/denominator rounded to six decimals. A zero denominator is 0, not NaN: an
// audit with nothing classifiable has no ceiling, and NaN in a published number is a bug that
// travels.
func share(count, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	return round6(float64(count) / float64(denominator))
}

// round6 rounds to six decimals, the precision `coverage_audits.ceiling` stores and the
// precision every published figure carries.
func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}

// FeederSet returns the named rung of the ladder.
func (r *Result) FeederSet(name string) (FeederSetResult, bool) {
	for _, set := range r.FeederSets {
		if set.Name == name {
			return set, true
		}
	}
	return FeederSetResult{}, false
}

// Markdown renders the result the way the published audit is written: aggregate only, never
// per-incident, so the output of a run over a private list can be checked in as it stands
// (FR-069a).
func (r *Result) Markdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Coverage audit %s (aggregate)\n\n", r.AuditID)
	fmt.Fprintf(&b,
		"Per spec 002 User Story 0 (FR-069 to FR-071b): before tuning the reasoning layer, measure the\n"+
			"fraction of real incidents whose true cause would have been a node or change in the graph at\n"+
			"alert time. Incident-level detail is kept in a private corpus, not in this repository.\n\n")
	fmt.Fprintf(&b, "**Run**: %s by `%s`. **Input digest**: `%s`.\n\n",
		r.RunAt, r.Author, r.InputDigest)

	fmt.Fprintf(&b, "**Corpus**: %s, %d classifiable incidents between %s and %s",
		r.Corpus.Label, r.ClassifiableCount, r.Corpus.From, r.Corpus.To)
	if r.Corpus.ExcludedCount > 0 {
		parts := make([]string, 0, len(r.Corpus.Excluded))
		for _, excluded := range r.Corpus.Excluded {
			parts = append(parts, fmt.Sprintf("%d %s", excluded.Count, excluded.Reason))
		}
		fmt.Fprintf(&b, " (excluded: %s)", strings.Join(parts, ", "))
	}
	if undecidable := r.IncidentCount - r.ClassifiableCount; undecidable > 0 {
		fmt.Fprintf(&b, "; %d listed but undecidable and therefore outside the denominator", undecidable)
	}
	b.WriteString(".\n\n")

	b.WriteString("| Feeder set | Cause is a node or change at alert time | Symptom visible, cause external | Not change-induced or unobservable |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, set := range r.FeederSets {
		fmt.Fprintf(&b, "| %s (%s) | %s (%d of %d) | %s (%d) | %s (%d) |\n",
			set.Name, strings.Join(set.Adds, ", "),
			percent(set.Ceiling), set.Observed, set.Classifiable,
			percent(set.SymptomOnlyShare), set.SymptomOnly,
			percent(set.RemainderShare), set.NotObservable)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b,
		"**Ceiling**: %s (%d of %d) with the feeder set in force, `%s`. "+
			"**π₀ = 1 − ceiling = %.6f** — the prior of *no observed change explains this* "+
			"(ADR-0005 D9, FR-019a).\n\n",
		percent(r.Ceiling), r.ceilingNumerator(), r.ClassifiableCount, r.FeederSetInForce, r.Prior)

	inForce, _ := r.FeederSet(r.FeederSetInForce)
	if len(inForce.Missing) > 0 {
		b.WriteString("**Missing causes by category** — the ranking of the feeders worth writing next (FR-070):\n\n")
		b.WriteString("| Category | Incidents | Share of classifiable |\n|---|---|---|\n")
		for _, row := range inForce.Missing {
			fmt.Fprintf(&b, "| %s | %d | %s |\n", row.Category, row.Count, percent(row.Share))
		}
		b.WriteString("\n")
	}

	if len(r.Remainder) > 0 {
		b.WriteString("**Unobservable remainder** — the incidents on which the correct engine answer is\n" +
			"`unobserved` or `not_change_induced`. Each category owes the evaluation corpus at least one\n" +
			"fixture carrying it as ground truth (FR-071b, SC-023):\n\n")
		b.WriteString("| Category | Cause class | Incidents | Share of classifiable |\n|---|---|---|---|\n")
		for _, row := range r.Remainder {
			classes := make([]string, len(row.Classes))
			for i, class := range row.Classes {
				classes[i] = string(class)
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s |\n",
				row.Category, strings.Join(classes, ", "), row.Count, percent(row.Share))
		}
		b.WriteString("\n")
	}

	b.WriteString("Every published **ceiling-bounded** accuracy target (FR-071) must be at or below this\n" +
		"ceiling and must cite this audit by id. Targets measuring precision, latency, invariance or\n" +
		"validity are not bounded by it and must not be scaled down to it.\n")
	return b.String()
}

// ceilingNumerator is how many incidents the feeder set in force observed.
func (r *Result) ceilingNumerator() int {
	if set, ok := r.FeederSet(r.FeederSetInForce); ok {
		return set.Observed
	}
	return 0
}

// percent renders a share the way the published audit writes it.
func percent(share float64) string {
	return fmt.Sprintf("%.0f %%", share*100)
}
