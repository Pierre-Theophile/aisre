// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// The numbers this file pins are the published September 2026 aggregate's numbers, reproduced
// from a synthetic list (fixtures/audits/synthetic-01). If one of them moves, either the audit's
// arithmetic changed or the fixture did, and both are things a reviewer must see.
const (
	wantCeiling = 0.615385 // 8 of 13
	wantPrior   = 0.384615 // π₀ = 1 − ceiling
)

func runFixture(t *testing.T, name string, opts audit.Options) *audit.Result {
	t.Helper()
	result, err := audit.Run(loadFixture(t, name), opts)
	if err != nil {
		t.Fatalf("Run(%s): %v", name, err)
	}
	return result
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 5e-7 }

// TestRunReproducesThePublishedAggregate is the test SC-022 turns into code: a ceiling of about
// 62 %, stated with the incident count it rests on, with 0 of 13 / 3 of 13 / 8 of 13 per feeder
// set, and the missing causes counted by category.
func TestRunReproducesThePublishedAggregate(t *testing.T) {
	t.Parallel()
	result := runFixture(t, "synthetic-01", audit.Options{})

	if got, want := result.FeederSetInForce, "+vendor-notice"; got != want {
		t.Errorf("feeder set in force = %q, want %q (the highest rung by default)", got, want)
	}
	if !closeTo(result.Ceiling, wantCeiling) {
		t.Errorf("ceiling = %v, want %v", result.Ceiling, wantCeiling)
	}
	if !closeTo(result.Prior, wantPrior) {
		t.Errorf("π₀ = %v, want %v", result.Prior, wantPrior)
	}
	if got, want := result.ClassifiableCount, 13; got != want {
		t.Errorf("classifiable count = %d, want %d — the ceiling must be published with the count it rests on", got, want)
	}
	if got, want := result.IncidentCount, 13; got != want {
		t.Errorf("incident count = %d, want %d", got, want)
	}
	if got, want := result.Corpus.ExcludedCount, 3; got != want {
		t.Errorf("excluded = %d, want %d", got, want)
	}

	ladder := []struct {
		name          string
		observed      int
		symptomOnly   int
		notObservable int
		ceiling       float64
	}{
		{"feature-001", 0, 2, 11, 0},
		{"+deploy", 3, 2, 8, 0.230769},
		{"+vendor-notice", 8, 2, 3, wantCeiling},
	}
	if len(result.FeederSets) != len(ladder) {
		t.Fatalf("feeder sets = %d, want %d", len(result.FeederSets), len(ladder))
	}
	for i, want := range ladder {
		got := result.FeederSets[i]
		if got.Name != want.name {
			t.Errorf("feeder_sets[%d].name = %q, want %q (cumulative order)", i, got.Name, want.name)
		}
		if got.Observed != want.observed || got.SymptomOnly != want.symptomOnly ||
			got.NotObservable != want.notObservable {
			t.Errorf("%s: observed/symptom/not-observable = %d/%d/%d, want %d/%d/%d",
				want.name, got.Observed, got.SymptomOnly, got.NotObservable,
				want.observed, want.symptomOnly, want.notObservable)
		}
		if !closeTo(got.Ceiling, want.ceiling) {
			t.Errorf("%s: ceiling = %v, want %v", want.name, got.Ceiling, want.ceiling)
		}
		if !closeTo(got.Prior, 1-want.ceiling) {
			t.Errorf("%s: π₀ = %v, want %v", want.name, got.Prior, 1-want.ceiling)
		}
		// symptom_only is reported apart from the ceiling on every rung: 2 of 13 = 15 %,
		// exactly as the published table's middle column reads.
		if !closeTo(got.SymptomOnlyShare, 0.153846) {
			t.Errorf("%s: symptom-only share = %v, want 0.153846", want.name, got.SymptomOnlyShare)
		}
	}
}

// TestSymptomOnlyIsNotCountedTowardTheCeiling states the rule directly: the effect being visible
// is not the cause being present.
func TestSymptomOnlyIsNotCountedTowardTheCeiling(t *testing.T) {
	t.Parallel()
	result := runFixture(t, "synthetic-01", audit.Options{})

	inForce, ok := result.FeederSet("+vendor-notice")
	if !ok {
		t.Fatal("+vendor-notice missing from the result")
	}
	if inForce.Observed+inForce.SymptomOnly+inForce.NotObservable != inForce.Classifiable {
		t.Errorf("the three verdict counts do not sum to the classifiable set: %d+%d+%d != %d",
			inForce.Observed, inForce.SymptomOnly, inForce.NotObservable, inForce.Classifiable)
	}
	if !closeTo(inForce.Ceiling, float64(inForce.Observed)/float64(inForce.Classifiable)) {
		t.Error("the ceiling is not observed / classifiable")
	}
}

// TestUndecidableLeavesTheDenominator: an incident nobody could classify is not a miss.
func TestUndecidableLeavesTheDenominator(t *testing.T) {
	t.Parallel()

	list, err := audit.ParseList([]byte(`
version: 1
audit_id: t-undecidable
run_at: 2026-09-17
author: test|auditor
corpus: {label: test-org, from: 2026-01-01, to: 2026-09-01}
feeder_sets: [{name: base, order: 1, feeders: [otel.spans]}]
incidents:
  - ref: T-01
    alert_at: 2026-02-01
    cause: {category: iac_apply, class: change_induced}
    observability: {base: observed}
  - ref: T-02
    alert_at: 2026-03-01
    cause: {category: other, class: change_induced}
    observability: {base: {verdict: undecidable, reason: cause_never_established}}
`))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	result, err := audit.Run(list, audit.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.IncidentCount != 2 {
		t.Errorf("incident count = %d, want 2", result.IncidentCount)
	}
	if result.ClassifiableCount != 1 {
		t.Errorf("classifiable count = %d, want 1 — the undecidable incident leaves the denominator",
			result.ClassifiableCount)
	}
	if !closeTo(result.Ceiling, 1) {
		t.Errorf("ceiling = %v, want 1 (1 of 1 classifiable observed)", result.Ceiling)
	}
	set, _ := result.FeederSet("base")
	if set.Undecidable != 1 {
		t.Errorf("undecidable = %d, want 1", set.Undecidable)
	}

	var undecidable audit.Item
	for _, item := range result.Items {
		if item.IncidentRef == "T-02" {
			undecidable = item
		}
	}
	if undecidable.Classification != audit.ClassificationUndecidable {
		t.Errorf("T-02 classification = %q, want undecidable", undecidable.Classification)
	}
	if undecidable.Reason != audit.ReasonCauseNeverEstablished {
		t.Errorf("T-02 reason = %q, want cause_never_established", undecidable.Reason)
	}
	if undecidable.CountsTowardCeiling {
		t.Error("an undecidable item counts toward the ceiling")
	}
}

// TestMissingCausesRankTheFeedersWorthWritingNext is FR-070: the categories read as a ranking.
func TestMissingCausesRankTheFeedersWorthWritingNext(t *testing.T) {
	t.Parallel()
	result := runFixture(t, "synthetic-01", audit.Options{FeederSet: "feature-001"})

	set, ok := result.FeederSet("feature-001")
	if !ok {
		t.Fatal("feature-001 missing from the result")
	}
	if len(set.Missing) == 0 {
		t.Fatal("no missing categories reported with the 001 feeders alone")
	}
	if set.Missing[0].Category != audit.CategoryThirdPartyOutage {
		t.Errorf("top missing category = %q, want third_party_outage (3 incidents, the most)",
			set.Missing[0].Category)
	}
	if set.Missing[0].Count != 3 {
		t.Errorf("third_party_outage count = %d, want 3", set.Missing[0].Count)
	}
	for i := 1; i < len(set.Missing); i++ {
		if set.Missing[i].Count > set.Missing[i-1].Count {
			t.Fatalf("missing categories are not ranked by count: %v", set.Missing)
		}
	}
	// The whole miss is 13 with the 001 feeders alone: 11 unobservable plus 2 symptom-only.
	total := 0
	for _, row := range set.Missing {
		total += row.Count
	}
	if total != 13 {
		t.Errorf("missing categories sum to %d, want 13", total)
	}
}

// TestRemainderNamesTheCategoriesThatOweFixtures is FR-071b's half of the result.
func TestRemainderNamesTheCategoriesThatOweFixtures(t *testing.T) {
	t.Parallel()
	result := runFixture(t, "synthetic-01", audit.Options{})

	want := map[audit.Category]audit.CauseClass{
		audit.CategoryLatentBug:               audit.CauseUnobserved,
		audit.CategoryClientSideConfiguration: audit.CauseNotChangeInduced,
		audit.CategoryBusinessDataChange:      audit.CauseNotChangeInduced,
	}
	if len(result.Remainder) != len(want) {
		t.Fatalf("remainder has %d categories, want %d: %+v", len(result.Remainder), len(want), result.Remainder)
	}
	for _, row := range result.Remainder {
		class, ok := want[row.Category]
		if !ok {
			t.Errorf("unexpected remainder category %q", row.Category)
			continue
		}
		if len(row.Classes) != 1 || row.Classes[0] != class {
			t.Errorf("%s: classes = %v, want [%s]", row.Category, row.Classes, class)
		}
		if row.Count != 1 {
			t.Errorf("%s: count = %d, want 1", row.Category, row.Count)
		}
	}
	// Symptom-only incidents are missing causes but they are NOT unobservable: they must not
	// appear in the remainder, or the corpus would owe a fixture for a cause a feeder can see.
	for _, row := range result.Remainder {
		if row.Category == audit.CategoryTrafficShift {
			t.Error("a symptom-only category reached the unobservable remainder")
		}
	}
}

// TestFeederSetSelection: `--feeder-set` publishes a lower rung's ceiling, which is what the
// first audit did when it reported "0 of 13 with the feature 001 feeders alone".
func TestFeederSetSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		set     string
		ceiling float64
		prior   float64
	}{
		{"feature-001", 0, 1},
		{"+deploy", 0.230769, 0.769231},
		{"+vendor-notice", wantCeiling, wantPrior},
	}
	for _, test := range tests {
		t.Run(test.set, func(t *testing.T) {
			t.Parallel()
			result := runFixture(t, "synthetic-01", audit.Options{FeederSet: test.set})
			if !closeTo(result.Ceiling, test.ceiling) {
				t.Errorf("ceiling = %v, want %v", result.Ceiling, test.ceiling)
			}
			if !closeTo(result.Prior, test.prior) {
				t.Errorf("π₀ = %v, want %v", result.Prior, test.prior)
			}
		})
	}
}

func TestRunRejectsAnUndeclaredFeederSet(t *testing.T) {
	t.Parallel()

	_, err := audit.Run(loadFixture(t, "synthetic-01"), audit.Options{FeederSet: "+telepathy"})
	if err == nil {
		t.Fatal("Run accepted a feeder set the list never declared")
	}
	if !strings.Contains(err.Error(), "not declared by this list") {
		t.Errorf("error = %v, want it to say the set is undeclared", err)
	}
}

// TestAggregateOnlyCarriesNoIncidentDetail is FR-069a's privacy clause as a test: the published
// artifact must be publishable.
func TestAggregateOnlyCarriesNoIncidentDetail(t *testing.T) {
	t.Parallel()

	full := runFixture(t, "synthetic-01", audit.Options{})
	if len(full.Items) != 13 {
		t.Fatalf("items = %d, want 13", len(full.Items))
	}

	aggregate := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true})
	if len(aggregate.Items) != 0 {
		t.Errorf("aggregate carries %d items, want none", len(aggregate.Items))
	}
	if !closeTo(aggregate.Ceiling, full.Ceiling) {
		t.Error("dropping the items changed the ceiling")
	}

	encoded, err := json.Marshal(aggregate)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, ref := range []string{"SYN-01", "SYN-13", "incident_ref"} {
		if strings.Contains(string(encoded), ref) {
			t.Errorf("the aggregate JSON leaks %q", ref)
		}
	}
}

// TestItemsCarryTheComposedStatedCause: the database column is NOT NULL and the format carries
// no prose, so the value is composed from the two closed-set fields.
func TestItemsCarryTheComposedStatedCause(t *testing.T) {
	t.Parallel()
	result := runFixture(t, "synthetic-01", audit.Options{})

	for _, item := range result.Items {
		if item.StatedCause == "" {
			t.Fatalf("%s: stated_cause is empty", item.IncidentRef)
		}
		want := string(item.Category) + "/" + string(item.Class)
		if item.StatedCause != want {
			t.Errorf("%s: stated_cause = %q, want %q", item.IncidentRef, item.StatedCause, want)
		}
	}
}

// TestMarkdownIsPublishable checks the human rendering carries what FR-069/FR-069a require and
// nothing it must not: the ceiling with its count, π₀, every rung, and no incident reference.
func TestMarkdownIsPublishable(t *testing.T) {
	t.Parallel()

	markdown := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true}).Markdown()

	for _, want := range []string{
		"62 %", "8 of 13", "0.384615", "feature-001", "+deploy", "+vendor-notice",
		"latent_bug", "client_side_configuration", "business_data_change",
		"2 security_incident", "1 unclassifiable",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown is missing %q\n---\n%s", want, markdown)
		}
	}
	for _, unwanted := range []string{"SYN-01", "SYN-11"} {
		if strings.Contains(markdown, unwanted) {
			t.Errorf("markdown leaks the incident reference %q", unwanted)
		}
	}
}

// TestResultRoundTripsThroughJSON: `compare` and the guard read what `audit coverage` wrote, and
// the decoder is strict, so a field added to Result and not to the reader would break them.
func TestResultRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	result := runFixture(t, "synthetic-01", audit.Options{})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := audit.ParseResult(encoded)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if !closeTo(decoded.Ceiling, result.Ceiling) || decoded.AuditID != result.AuditID {
		t.Errorf("round trip changed the result: %+v", decoded)
	}
	if len(decoded.Items) != len(result.Items) {
		t.Errorf("round trip lost items: %d, want %d", len(decoded.Items), len(result.Items))
	}
}
