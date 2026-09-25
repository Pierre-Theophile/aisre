// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Coverage against GCP's own answer (T068, FR-144).
//
// The measurement is deliberately the least flattering one available: not "how many nodes did we
// produce" but "which of GCP's own services are missing from the graph, by name".
//
// # Differences are enumerated, never summarised
//
// FR-144 says so, and the reason is that a percentage hides the only thing that matters. "97%
// coverage" over 300 services is nine missing services, and *which* nine decides whether the number
// is fine or whether the connector is blind to every service in one region. A summary also cannot be
// acted on: an operator reading "3 missing" has to go and find them. So `Report` carries the names,
// sorted, both directions, and `Ratio` exists only for a dashboard that has already been given the
// list.
//
// # Both directions, and they mean different things
//
// - **Missing**: GCP lists it and the graph does not. A gap in the feeder — a payload it could not
//   decode, a region it did not poll, a name it could not parse.
// - **Extra**: the graph has it and GCP does not. This is *not* symmetric with the above. Inside the
//   history horizon it is usually correct — a revision GCP has since garbage-collected is still a
//   fact about the past — so it is reported separately and is not counted against coverage. Outside
//   any plausible retention it is a stale assertion the silence rule should have retracted, which is
//   a different bug with a different fix.

// Surface is the entity family a coverage comparison is about. Each is compared separately because a
// connector can be complete in one and blind in another, and one blended number would hide that.
type Surface string

// The published surfaces.
const (
	// SurfaceServices compares Cloud Run services.
	SurfaceServices Surface = "cloudrun.services"
	// SurfaceRevisions compares Cloud Run revisions inside the history horizon.
	SurfaceRevisions Surface = "cloudrun.revisions"
	// SurfaceAlertPolicies compares Cloud Monitoring alert policies.
	SurfaceAlertPolicies Surface = "monitoring.alert_policies"
)

// Comparison is one surface's sets: what GCP listed and what the feeder produced.
type Comparison struct {
	Surface Surface
	// Project and Region scope the comparison. A comparison that mixed projects would hide a
	// region the feeder never polled, because the totals would still look right.
	Project, Region string
	// GCPListed is what GCP's own list call returned, as this feeder's identifier values.
	GCPListed []string
	// Produced is what the feeder emitted.
	Produced []string
	// Horizon bounds a revision comparison. A revision GCP lists that is older than the horizon is
	// **not** counted as missing: the feeder was never asked to know about it, and counting it
	// would make the coverage number a function of the estate's age (FR-024).
	Horizon Horizon
	// CreatedAt dates each of GCPListed, for the horizon test. A value with no date is compared
	// without one, which means it is counted — the conservative direction, since an undated
	// revision the feeder did not produce is a real gap until somebody shows otherwise.
	CreatedAt map[string]time.Time
}

// Difference is one entity present on one side and not the other.
type Difference struct {
	// Value is the identifier.
	Value string
	// Why says what the difference means, which is different for the two directions.
	Why string
}

// The published difference reasons.
const (
	// DiffMissing means GCP lists it and the graph does not.
	DiffMissing = "GCP lists it and the graph does not: a gap in the feeder"
	// DiffExtraInsideHorizon means the graph has it and GCP does not, inside the horizon — which
	// is usually correct, because a garbage-collected revision is still a fact about the past.
	DiffExtraInsideHorizon = "the graph has it and GCP does not, inside the history horizon: " +
		"usually correct, since a revision GCP has garbage-collected is still a fact about the past"
	// DiffExtraOutsideHorizon means the graph has something GCP does not and no retention explains
	// it: a stale assertion the silence rule should have retracted.
	DiffExtraOutsideHorizon = "the graph has it, GCP does not, and no retention explains it: " +
		"a stale assertion the silence rule should have retracted (FR-023)"
	// DiffBelowHorizon means GCP lists it but it predates the horizon, so it is not a gap.
	DiffBelowHorizon = "GCP lists it but it predates the history horizon, so the feeder was never " +
		"asked to know about it (FR-024)"
)

// Report is one surface's coverage, with the differences enumerated.
type Report struct {
	Surface         Surface
	Project, Region string
	// Missing is what GCP has and the graph does not, sorted, with reasons.
	Missing []Difference
	// Extra is what the graph has and GCP does not, sorted, with reasons.
	Extra []Difference
	// BelowHorizon is what GCP has that predates the horizon. It is reported so that a reader can
	// see it was considered and excluded, rather than wondering why the totals do not add up.
	BelowHorizon []Difference
	// Matched is how many identifiers both sides agree on.
	Matched int
	// Comparable is how many of GCP's identifiers were in scope for the comparison: listed, minus
	// those below the horizon. It is the denominator, and it is stated rather than left implicit,
	// because a ratio whose denominator a reader has to reconstruct is a ratio they will
	// reconstruct wrongly.
	Comparable int
}

// Compare runs one surface's comparison.
func Compare(c Comparison) Report {
	report := Report{Surface: c.Surface, Project: c.Project, Region: c.Region}

	produced := map[string]bool{}
	for _, value := range c.Produced {
		produced[value] = true
	}
	listed := map[string]bool{}

	gcpListed := append([]string(nil), c.GCPListed...)
	sort.Strings(gcpListed)
	for _, value := range gcpListed {
		listed[value] = true
		if c.belowHorizon(value) {
			report.BelowHorizon = append(report.BelowHorizon, Difference{Value: value, Why: DiffBelowHorizon})
			continue
		}
		report.Comparable++
		if produced[value] {
			report.Matched++
			continue
		}
		report.Missing = append(report.Missing, Difference{Value: value, Why: DiffMissing})
	}

	extra := make([]string, 0)
	for _, value := range c.Produced {
		if !listed[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(extra)
	for _, value := range extra {
		report.Extra = append(report.Extra, Difference{Value: value, Why: c.extraReason(value)})
	}
	return report
}

// belowHorizon reports whether GCP's entry predates the history horizon.
func (c Comparison) belowHorizon(value string) bool {
	if c.Horizon.Earliest.IsZero() {
		return false
	}
	created, dated := c.CreatedAt[value]
	if !dated {
		// Undated: compared, and therefore counted if absent. An undated revision the feeder did
		// not produce is a real gap until somebody shows otherwise.
		return false
	}
	return created.Before(c.Horizon.Earliest)
}

// extraReason decides which of the two "extra" readings applies.
func (c Comparison) extraReason(value string) string {
	if c.Horizon.Earliest.IsZero() {
		return DiffExtraInsideHorizon
	}
	if created, dated := c.CreatedAt[value]; dated && created.Before(c.Horizon.Earliest) {
		return DiffExtraOutsideHorizon
	}
	return DiffExtraInsideHorizon
}

// Ratio is matched over comparable, or 1 when there is nothing to compare.
//
// It exists for a dashboard, and it is deliberately the *last* thing on this type. A reader who has
// only this number has been given the one figure FR-144 says is not enough on its own.
func (r Report) Ratio() float64 {
	if r.Comparable == 0 {
		return 1
	}
	return float64(r.Matched) / float64(r.Comparable)
}

// Complete reports whether GCP's list is fully represented. It ignores `Extra`, because an extra
// entity inside the horizon is usually correct and counting it against coverage would push a
// connector toward forgetting the past to improve its score.
func (r Report) Complete() bool { return len(r.Missing) == 0 }

// String renders the report with every difference named, which is the form FR-144 requires.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s in %s/%s: %d of %d matched", r.Surface, r.Project, r.Region, r.Matched, r.Comparable)
	if len(r.BelowHorizon) > 0 {
		fmt.Fprintf(&b, " (%d below the horizon, excluded)", len(r.BelowHorizon))
	}
	b.WriteString("\n")
	writeDifferences(&b, "missing (GCP has it, the graph does not)", r.Missing)
	writeDifferences(&b, "extra (the graph has it, GCP does not)", r.Extra)
	writeDifferences(&b, "below the horizon (not a gap)", r.BelowHorizon)
	if r.Complete() && len(r.Extra) == 0 {
		b.WriteString("  no differences\n")
	}
	return b.String()
}

func writeDifferences(b *strings.Builder, heading string, diffs []Difference) {
	if len(diffs) == 0 {
		return
	}
	fmt.Fprintf(b, "  %s, %d:\n", heading, len(diffs))
	// Every one, by name. There is no elision and no "... and 42 more": the point of enumerating is
	// that the list is actionable, and a truncated list is a summary with extra steps.
	for _, diff := range diffs {
		fmt.Fprintf(b, "    %s\n", diff.Value)
	}
	fmt.Fprintf(b, "    ^ %s\n", diffs[0].Why)
}

// Audit is a whole run's coverage: one report per surface per project and region.
type Audit struct {
	Reports []Report
}

// Add appends a report.
func (a *Audit) Add(report Report) { a.Reports = append(a.Reports, report) }

// Complete reports whether every surface is complete.
func (a Audit) Complete() bool {
	for _, report := range a.Reports {
		if !report.Complete() {
			return false
		}
	}
	return true
}

// MissingTotal is how many identifiers GCP has that the graph does not, across every surface.
func (a Audit) MissingTotal() int {
	total := 0
	for _, report := range a.Reports {
		total += len(report.Missing)
	}
	return total
}

// String renders the whole audit, surfaces in a stable order.
func (a Audit) String() string {
	reports := append([]Report(nil), a.Reports...)
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].Surface != reports[j].Surface {
			return reports[i].Surface < reports[j].Surface
		}
		if reports[i].Project != reports[j].Project {
			return reports[i].Project < reports[j].Project
		}
		return reports[i].Region < reports[j].Region
	})
	var b strings.Builder
	for _, report := range reports {
		b.WriteString(report.String())
	}
	if a.Complete() {
		b.WriteString("coverage: every surface complete against GCP's own list\n")
	} else {
		fmt.Fprintf(&b, "coverage: %d identifiers GCP lists are absent from the graph, enumerated above\n",
			a.MissingTotal())
	}
	return b.String()
}
