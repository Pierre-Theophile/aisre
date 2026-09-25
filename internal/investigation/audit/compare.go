// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Comparing two audit runs (T014, FR-071, FR-071a).
//
// FR-071a asks for something more specific than "did the number go up": both runs readable, the
// ceiling comparable between them, the movement attributed to the feeders that were added, and
// **a ceiling that did not move reported as such rather than omitted**. That last clause is the
// one that shapes this file. The natural implementation of a diff emits changed rows; this one
// emits every rung of the ladder, changed or not, because "we shipped the vendor-notice feeder
// and the ceiling did not move" is the single most useful sentence an audit comparison can
// produce, and a diff that drops it publishes silence instead of a result.
//
// Comparability is likewise reported rather than assumed. Two audits over different incident
// lists have ceilings that are both true and not comparable; the comparison says so, names why,
// and still prints both, because refusing to print is not more honest than printing with a
// caveat.

// Comparison is the result of comparing two audit runs, older first.
type Comparison struct {
	// Before and After identify the two runs.
	Before Ref `json:"before"`
	After  Ref `json:"after"`
	// Comparable is false when the two runs were not measured over the same incident list.
	Comparable bool `json:"comparable"`
	// Caveats say why, when Comparable is false, and are empty otherwise.
	Caveats []string `json:"caveats,omitempty"`
	// Notes are observations that do not block the comparison — most usefully, that the input
	// file changed while the incidents in it did not, which is what happens when a re-run
	// adds a feeder set to the same list.
	Notes []string `json:"notes,omitempty"`
	// CeilingDelta is After's in-force ceiling minus Before's, rounded to six decimals.
	CeilingDelta float64 `json:"ceiling_delta"`
	// PriorDelta is the movement in π₀, which is the negative of CeilingDelta.
	PriorDelta float64 `json:"prior_delta"`
	// Moved is false when the in-force ceiling did not move at all.
	Moved bool `json:"moved"`
	// FeederSets is EVERY rung of the ladder in either run, moved or not (FR-071a).
	FeederSets []FeederSetMovement `json:"feeder_sets"`
	// Added and Removed name the feeder sets that appear in only one of the runs.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// AddedFeeders are the individual feeders After's in-force configuration has and
	// Before's did not — the things the movement is attributed to.
	AddedFeeders []string `json:"added_feeders,omitempty"`
	// Incidents is the per-incident movement, available only when both runs carry items. A
	// published aggregate carries none, and the comparison is still valid without it.
	Incidents []IncidentMovement `json:"incidents,omitempty"`
	// Remainder is how the unobservable remainder changed by category.
	Remainder []RemainderMovement `json:"remainder,omitempty"`
	// Summary is the one-sentence reading of the above, in the words FR-071a uses.
	Summary string `json:"summary"`
}

// Ref identifies one side of a comparison: which audit it was, and the figures it published.
type Ref struct {
	// AuditID names the run.
	AuditID string `json:"audit_id"`
	// RunAt is when it was carried out.
	RunAt Instant `json:"run_at"`
	// CorpusLabel and InputDigest are what comparability is judged on.
	CorpusLabel string `json:"corpus_label"`
	InputDigest string `json:"input_digest"`
	// FeederSetInForce, Ceiling, Prior and ClassifiableCount are the published figures.
	FeederSetInForce  string  `json:"feeder_set_in_force"`
	Ceiling           float64 `json:"ceiling"`
	Prior             float64 `json:"prior"`
	ClassifiableCount int     `json:"classifiable_count"`
}

// FeederSetMovement is one rung of the ladder in both runs. A rung present in only one run
// carries Present{Before,After} accordingly; a rung whose ceiling is unchanged is reported with
// Moved false and Note "did not move" rather than being omitted (FR-071a).
type FeederSetMovement struct {
	// Name identifies the rung.
	Name string `json:"name"`
	// PresentBefore and PresentAfter say which runs declared it.
	PresentBefore bool `json:"present_before"`
	PresentAfter  bool `json:"present_after"`
	// CeilingBefore and CeilingAfter are its ceilings, zero where absent.
	CeilingBefore float64 `json:"ceiling_before"`
	CeilingAfter  float64 `json:"ceiling_after"`
	// ObservedBefore and ObservedAfter are the numerators behind them.
	ObservedBefore int `json:"observed_before"`
	ObservedAfter  int `json:"observed_after"`
	// Delta is CeilingAfter − CeilingBefore where both are present.
	Delta float64 `json:"delta"`
	// Moved is false when the ceiling is identical in both runs.
	Moved bool `json:"moved"`
	// AddedFeeders are the feeders this rung gained between the runs.
	AddedFeeders []string `json:"added_feeders,omitempty"`
	// Note is the human reading: "did not move", "new feeder set", "no longer measured".
	Note string `json:"note"`
}

// IncidentMovement is one incident whose classification changed under the feeder set in force.
type IncidentMovement struct {
	// IncidentRef is the opaque reference.
	IncidentRef string `json:"incident_ref"`
	// Category is its stated cause's category.
	Category Category `json:"category"`
	// VerdictBefore and VerdictAfter are its verdicts under each run's in-force feeder set.
	VerdictBefore Verdict `json:"verdict_before"`
	VerdictAfter  Verdict `json:"verdict_after"`
	// AttributedTo names the feeders added between the runs, which is the best attribution an
	// audit comparison can make: the audit measures, it does not infer causation between a
	// feeder and an incident it never saw.
	AttributedTo []string `json:"attributed_to,omitempty"`
}

// RemainderMovement is how one category of the unobservable remainder changed.
type RemainderMovement struct {
	// Category is the cause category.
	Category Category `json:"category"`
	// CountBefore and CountAfter are its incident counts in the remainder.
	CountBefore int `json:"count_before"`
	CountAfter  int `json:"count_after"`
	// Delta is CountAfter − CountBefore.
	Delta int `json:"delta"`
}

// LoadResult reads a published audit result from a JSON file.
func LoadResult(path string) (*Result, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names the audits they compare.
	if err != nil {
		return nil, fmt.Errorf("audit: read result: %w", err)
	}
	result, err := ParseResult(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: %s: %w", path, err)
	}
	return result, nil
}

// ParseResult decodes a published audit result strictly.
func ParseResult(raw []byte) (*Result, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	result := &Result{}
	if err := decoder.Decode(result); err != nil {
		return nil, fmt.Errorf("decode audit result: %w", err)
	}
	if result.AuditID == "" {
		return nil, fmt.Errorf("decode audit result: audit_id is required")
	}
	if len(result.FeederSets) == 0 {
		return nil, fmt.Errorf("decode audit result: feeder_sets is required")
	}
	return result, nil
}

// Compare compares two audit runs, older first.
func Compare(before, after *Result) (*Comparison, error) {
	if before == nil || after == nil {
		return nil, fmt.Errorf("audit: compare: both runs are required")
	}

	comparison := &Comparison{
		Before:       refOf(before),
		After:        refOf(after),
		CeilingDelta: round6(after.Ceiling - before.Ceiling),
		PriorDelta:   round6(after.Prior - before.Prior),
	}
	comparison.Moved = comparison.CeilingDelta != 0
	comparison.Comparable, comparison.Caveats, comparison.Notes = comparability(before, after)
	comparison.FeederSets = compareFeederSets(before, after)
	comparison.Added, comparison.Removed = feederSetDelta(before, after)
	comparison.AddedFeeders = addedFeeders(before, after)
	comparison.Incidents = compareIncidents(before, after, comparison.AddedFeeders)
	comparison.Remainder = compareRemainder(before, after)
	comparison.Summary = summarize(comparison)
	return comparison, nil
}

func refOf(result *Result) Ref {
	return Ref{
		AuditID:           result.AuditID,
		RunAt:             result.RunAt,
		CorpusLabel:       result.Corpus.Label,
		InputDigest:       result.InputDigest,
		FeederSetInForce:  result.FeederSetInForce,
		Ceiling:           result.Ceiling,
		Prior:             result.Prior,
		ClassifiableCount: result.ClassifiableCount,
	}
}

// comparability judges whether the two ceilings may be read against each other.
//
// FR-071a asks for the audit to be re-run "over the same incident list", and the thing that has
// to be the same is the **incidents**, not the file. A re-run after a connector ships adds a
// rung to the feeder-set ladder and a verdict per incident under it, so the input digest moves
// by construction; treating that as incomparability would make the guard fire on precisely the
// case FR-071a was written for. So the blocking conditions are the corpus, the denominator and
// the set of incident references; a moved digest over an unchanged incident set is a note.
func comparability(before, after *Result) (comparable bool, caveats, notes []string) {
	if before.Corpus.Label != after.Corpus.Label {
		caveats = append(caveats, fmt.Sprintf("different corpus: %q then %q",
			before.Corpus.Label, after.Corpus.Label))
	}
	if before.ClassifiableCount != after.ClassifiableCount {
		caveats = append(caveats, fmt.Sprintf(
			"different classifiable count: %d then %d — the ceilings rest on different denominators",
			before.ClassifiableCount, after.ClassifiableCount))
	}
	if added, removed := incidentDelta(before, after); len(added)+len(removed) > 0 {
		caveats = append(caveats, fmt.Sprintf(
			"the incident list changed: %d added, %d removed", len(added), len(removed)))
	}
	if before.InputDigest != "" && after.InputDigest != "" && before.InputDigest != after.InputDigest {
		notes = append(notes, "the input file changed (different digest); the incidents it lists did not")
	}
	return len(caveats) == 0, caveats, notes
}

// incidentDelta compares the two runs' incident references, where both carry items. A published
// aggregate carries none, and then the denominators are all there is to go on.
func incidentDelta(before, after *Result) (added, removed []string) {
	if len(before.Items) == 0 || len(after.Items) == 0 {
		return nil, nil
	}
	beforeRefs := make([]string, 0, len(before.Items))
	for _, item := range before.Items {
		beforeRefs = append(beforeRefs, item.IncidentRef)
	}
	afterRefs := make([]string, 0, len(after.Items))
	for _, item := range after.Items {
		afterRefs = append(afterRefs, item.IncidentRef)
	}
	for _, ref := range afterRefs {
		if !slices.Contains(beforeRefs, ref) {
			added = append(added, ref)
		}
	}
	for _, ref := range beforeRefs {
		if !slices.Contains(afterRefs, ref) {
			removed = append(removed, ref)
		}
	}
	return added, removed
}

func compareFeederSets(before, after *Result) []FeederSetMovement {
	names := make([]string, 0, len(before.FeederSets)+len(after.FeederSets))
	for _, set := range before.FeederSets {
		names = append(names, set.Name)
	}
	for _, set := range after.FeederSets {
		if !slices.Contains(names, set.Name) {
			names = append(names, set.Name)
		}
	}

	movements := make([]FeederSetMovement, 0, len(names))
	for _, name := range names {
		beforeSet, inBefore := before.FeederSet(name)
		afterSet, inAfter := after.FeederSet(name)
		movement := FeederSetMovement{
			Name:           name,
			PresentBefore:  inBefore,
			PresentAfter:   inAfter,
			CeilingBefore:  beforeSet.Ceiling,
			CeilingAfter:   afterSet.Ceiling,
			ObservedBefore: beforeSet.Observed,
			ObservedAfter:  afterSet.Observed,
		}
		switch {
		case inBefore && inAfter:
			movement.Delta = round6(afterSet.Ceiling - beforeSet.Ceiling)
			movement.Moved = movement.Delta != 0
			movement.AddedFeeders = missingFrom(beforeSet.Feeders, afterSet.Feeders)
			// FR-071a: reported as such rather than omitted.
			movement.Note = "did not move"
			if movement.Moved {
				movement.Note = fmt.Sprintf("%s → %s",
					percent(beforeSet.Ceiling), percent(afterSet.Ceiling))
			} else if len(movement.AddedFeeders) > 0 {
				movement.Note = "did not move despite added feeders: " +
					strings.Join(movement.AddedFeeders, ", ")
			}
		case inAfter:
			movement.AddedFeeders = slices.Clone(afterSet.Adds)
			movement.Note = "new feeder set"
		default:
			movement.Note = "no longer measured"
		}
		movements = append(movements, movement)
	}
	return movements
}

func feederSetDelta(before, after *Result) (added, removed []string) {
	for _, set := range after.FeederSets {
		if _, ok := before.FeederSet(set.Name); !ok {
			added = append(added, set.Name)
		}
	}
	for _, set := range before.FeederSets {
		if _, ok := after.FeederSet(set.Name); !ok {
			removed = append(removed, set.Name)
		}
	}
	return added, removed
}

// addedFeeders is the difference between the two in-force configurations — the feeders a
// movement in the published ceiling is attributed to.
func addedFeeders(before, after *Result) []string {
	beforeSet, _ := before.FeederSet(before.FeederSetInForce)
	afterSet, _ := after.FeederSet(after.FeederSetInForce)
	return missingFrom(beforeSet.Feeders, afterSet.Feeders)
}

func missingFrom(have, want []string) []string {
	var out []string
	for _, feeder := range want {
		if !slices.Contains(have, feeder) {
			out = append(out, feeder)
		}
	}
	return out
}

func compareIncidents(before, after *Result, added []string) []IncidentMovement {
	if len(before.Items) == 0 || len(after.Items) == 0 {
		return nil
	}
	beforeByRef := map[string]Item{}
	for _, item := range before.Items {
		beforeByRef[item.IncidentRef] = item
	}
	var movements []IncidentMovement
	for _, item := range after.Items {
		previous, ok := beforeByRef[item.IncidentRef]
		if !ok || previous.Verdict == item.Verdict {
			continue
		}
		movements = append(movements, IncidentMovement{
			IncidentRef:   item.IncidentRef,
			Category:      item.Category,
			VerdictBefore: previous.Verdict,
			VerdictAfter:  item.Verdict,
			AttributedTo:  slices.Clone(added),
		})
	}
	slices.SortStableFunc(movements, func(a, b IncidentMovement) int {
		return compareStrings(a.IncidentRef, b.IncidentRef)
	})
	return movements
}

func compareRemainder(before, after *Result) []RemainderMovement {
	counts := map[Category]*RemainderMovement{}
	order := make([]Category, 0, len(before.Remainder)+len(after.Remainder))
	note := func(category Category) *RemainderMovement {
		movement, ok := counts[category]
		if !ok {
			movement = &RemainderMovement{Category: category}
			counts[category] = movement
			order = append(order, category)
		}
		return movement
	}
	for _, row := range before.Remainder {
		note(row.Category).CountBefore = row.Count
	}
	for _, row := range after.Remainder {
		note(row.Category).CountAfter = row.Count
	}
	out := make([]RemainderMovement, 0, len(order))
	for _, category := range order {
		movement := counts[category]
		movement.Delta = movement.CountAfter - movement.CountBefore
		out = append(out, *movement)
	}
	slices.SortStableFunc(out, func(a, b RemainderMovement) int {
		return a.Category.rank() - b.Category.rank()
	})
	return out
}

func summarize(c *Comparison) string {
	var b strings.Builder
	if !c.Moved {
		fmt.Fprintf(&b, "the ceiling did not move: %s in both runs (%s, then %s)",
			percent(c.After.Ceiling), c.Before.AuditID, c.After.AuditID)
	} else {
		fmt.Fprintf(&b, "the ceiling moved from %s (%s) to %s (%s), %+.6f",
			percent(c.Before.Ceiling), c.Before.AuditID,
			percent(c.After.Ceiling), c.After.AuditID, c.CeilingDelta)
	}
	if len(c.AddedFeeders) > 0 {
		fmt.Fprintf(&b, "; attributed to the feeders added: %s", strings.Join(c.AddedFeeders, ", "))
	} else {
		b.WriteString("; no feeders were added between the runs")
	}
	if !c.Comparable {
		fmt.Fprintf(&b, "; NOT directly comparable (%s)", strings.Join(c.Caveats, "; "))
	}
	fmt.Fprintf(&b, "; π₀ %.6f → %.6f", c.Before.Prior, c.After.Prior)
	return b.String()
}

// Markdown renders the comparison the way a re-run is published beside its predecessor
// (FR-071a): both ceilings, the feeder set each was measured against, and every rung of the
// ladder including the ones that did not move.
func (c *Comparison) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Coverage audit comparison: %s → %s\n\n", c.Before.AuditID, c.After.AuditID)
	fmt.Fprintf(&b, "%s.\n\n", capitalizeFirst(c.Summary))
	fmt.Fprintf(&b, "| Run | Feeder set in force | Ceiling | π₀ | Classifiable incidents |\n|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| %s (%s) | %s | %s | %.6f | %d |\n",
		c.Before.AuditID, c.Before.RunAt, c.Before.FeederSetInForce,
		percent(c.Before.Ceiling), c.Before.Prior, c.Before.ClassifiableCount)
	fmt.Fprintf(&b, "| %s (%s) | %s | %s | %.6f | %d |\n\n",
		c.After.AuditID, c.After.RunAt, c.After.FeederSetInForce,
		percent(c.After.Ceiling), c.After.Prior, c.After.ClassifiableCount)

	b.WriteString("| Feeder set | Ceiling before | Ceiling after | Movement |\n|---|---|---|---|\n")
	for _, movement := range c.FeederSets {
		before, after := percent(movement.CeilingBefore), percent(movement.CeilingAfter)
		if !movement.PresentBefore {
			before = "-"
		}
		if !movement.PresentAfter {
			after = "-"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", movement.Name, before, after, movement.Note)
	}
	b.WriteString("\n")

	if len(c.Incidents) > 0 {
		b.WriteString("| Incident | Category | Verdict before | Verdict after | Attributed to |\n|---|---|---|---|---|\n")
		for _, movement := range c.Incidents {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				movement.IncidentRef, movement.Category, movement.VerdictBefore,
				movement.VerdictAfter, strings.Join(movement.AttributedTo, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
