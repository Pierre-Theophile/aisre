// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// TestClassifyMapsVerdictsToThePublishedThree is the core of T011: four verdicts, three
// classifications, and the one mapping that is easy to get wrong — `symptom_only` is
// `cause_absent`, because the effect being visible is not the cause being present.
func TestClassifyMapsVerdictsToThePublishedThree(t *testing.T) {
	t.Parallel()

	list, err := audit.ParseList([]byte(`
version: 1
audit_id: t-classify
run_at: 2026-09-17
author: test|auditor
corpus: {label: test-org, from: 2026-01-01, to: 2026-09-01}
feeder_sets: [{name: base, order: 1, feeders: [otel.spans]}]
incidents:
  - ref: T-observed
    alert_at: 2026-02-01
    cause: {category: iac_apply, class: change_induced, ref: "k8s.change=shop/api@rev3"}
    observability: {base: observed}
  - ref: T-symptom
    alert_at: 2026-02-02
    cause: {category: traffic_shift, class: change_induced}
    observability: {base: symptom_only}
  - ref: T-absent
    alert_at: 2026-02-03
    cause: {category: latent_bug, class: unobserved}
    observability: {base: not_observable}
  - ref: T-undecidable
    alert_at: 2026-02-04
    cause: {category: other, class: change_induced}
    observability: {base: {verdict: undecidable, reason: outside_retention}}
`))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}

	items, err := audit.Classify(list, "base")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	byRef := map[string]audit.Item{}
	for _, item := range items {
		byRef[item.IncidentRef] = item
	}

	tests := []struct {
		ref                 string
		classification      audit.Classification
		countsTowardCeiling bool
		owesFixture         bool
	}{
		{"T-observed", audit.ClassificationCausePresent, true, false},
		{"T-symptom", audit.ClassificationCauseAbsent, true, false},
		{"T-absent", audit.ClassificationCauseAbsent, true, true},
		{"T-undecidable", audit.ClassificationUndecidable, false, false},
	}
	for _, test := range tests {
		item, ok := byRef[test.ref]
		if !ok {
			t.Errorf("%s was not classified", test.ref)
			continue
		}
		if item.Classification != test.classification {
			t.Errorf("%s: classification = %q, want %q", test.ref, item.Classification, test.classification)
		}
		if item.CountsTowardCeiling != test.countsTowardCeiling {
			t.Errorf("%s: counts toward ceiling = %v, want %v",
				test.ref, item.CountsTowardCeiling, test.countsTowardCeiling)
		}
		if item.OwesFixture != test.owesFixture {
			t.Errorf("%s: owes fixture = %v, want %v", test.ref, item.OwesFixture, test.owesFixture)
		}
	}

	// An optional graph identifier is carried through untouched; it is never invented.
	if byRef["T-observed"].MatchedEntityID != "k8s.change=shop/api@rev3" {
		t.Errorf("matched entity = %q, want the supplied reference",
			byRef["T-observed"].MatchedEntityID)
	}
	if byRef["T-symptom"].MatchedEntityID != "" {
		t.Errorf("matched entity = %q, want empty where none was supplied",
			byRef["T-symptom"].MatchedEntityID)
	}
}

// TestClassifyItemIDsAreDeterministic: re-running the audit over the same list has to rewrite
// the same rows rather than accumulate a second copy.
func TestClassifyItemIDsAreDeterministic(t *testing.T) {
	t.Parallel()

	list := loadFixture(t, "synthetic-01")
	first, err := audit.Classify(list, "+vendor-notice")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	second, err := audit.Classify(list, "+vendor-notice")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("classification is not deterministic in length: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ItemID != second[i].ItemID {
			t.Errorf("item %d: id %q then %q", i, first[i].ItemID, second[i].ItemID)
		}
		if first[i].IncidentRef != second[i].IncidentRef {
			t.Errorf("item %d: order is not stable", i)
		}
	}
	if !strings.HasPrefix(first[0].ItemID, "synthetic-01:+vendor-notice:") {
		t.Errorf("item id = %q, want it to name the audit and the feeder set", first[0].ItemID)
	}
}

// TestClassifyIsOrderedByAlertInstant: a published item list reads as a timeline.
func TestClassifyIsOrderedByAlertInstant(t *testing.T) {
	t.Parallel()

	items, err := audit.Classify(loadFixture(t, "synthetic-01"), "feature-001")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	for i := 1; i < len(items); i++ {
		if items[i].AlertAt.Before(items[i-1].AlertAt.Time) {
			t.Fatalf("items are not ordered by alert instant: %s before %s",
				items[i].IncidentRef, items[i-1].IncidentRef)
		}
	}
}

func TestClassifyRejections(t *testing.T) {
	t.Parallel()

	if _, err := audit.Classify(nil, "base"); err == nil {
		t.Error("Classify accepted a nil list")
	}
	_, err := audit.Classify(loadFixture(t, "synthetic-01"), "+telepathy")
	if err == nil {
		t.Fatal("Classify accepted a feeder set the list never declared")
	}
	if !strings.Contains(err.Error(), "not declared by this list") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}
