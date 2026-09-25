// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"encoding/json"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// TestTheTwoPublishedTransformSetsAgree.
//
// `internal/fixture` names the transforms because it generates them and `internal/eval` names them
// because it checks them, and the second may not import the first the other way round. Two
// declarations of one published set is a thing that drifts, so it is asserted rather than trusted:
// a transform added to the generator with no invariant beside it would be a variant nothing
// grades.
func TestTheTwoPublishedTransformSetsAgree(t *testing.T) {
	if len(eval.Transforms) != len(fixture.Transforms) {
		t.Fatalf("eval publishes %v, fixture publishes %v", eval.Transforms, fixture.Transforms)
	}
	for i, transform := range eval.Transforms {
		if transform != fixture.Transforms[i] {
			t.Errorf("transform %d: eval says %q, fixture says %q", i, transform, fixture.Transforms[i])
		}
	}
}

// The metamorphic invariants (T108, FR-062a, SC-020).
//
// Each transform publishes a relation between the parent's answer and the variant's, and the
// tests below assert the relation in both directions — it holds when it should and fails when it
// should — because an invariant that cannot fail is a line in a report that always reads green.

func TestTheVerdictPreservingTransformsHoldAndCanFail(t *testing.T) {
	parent := outcomeNaming(culprit)
	parent.FixtureID = "rollout-regression-01-incident"

	for _, transform := range []string{eval.TransformDecoyInjected, eval.TransformTimeShifted} {
		t.Run(transform, func(t *testing.T) {
			same := outcomeNaming(culprit)
			same.FixtureID = parent.FixtureID + "-" + transform
			if got := eval.CheckInvariance(parent, same, transform, nil); !got.Held {
				t.Errorf("an unchanged verdict failed the invariant: %s", got.Detail)
			}

			moved := outcomeNaming(decoy)
			moved.FixtureID = parent.FixtureID + "-" + transform
			got := eval.CheckInvariance(parent, moved, transform, nil)
			if got.Held {
				t.Fatal("a changed verdict held the invariant")
			}
			for _, want := range []string{moved.FixtureID, culprit, decoy} {
				if !contains(got.Detail, want) {
					t.Errorf("the failure does not name %q: %s", want, got.Detail)
				}
			}
		})
	}
}

// TestNamePermutationComparesTheImageOfTheParentsVerdict is the whole point of that transform: the
// engine must rank on structure, and the check must therefore compare the *renamed* culprit
// rather than the original name.
func TestNamePermutationComparesTheImageOfTheParentsVerdict(t *testing.T) {
	const renamed = "k8s.change=shop/svc-a3f1@rev7"
	names := eval.NameMap{culprit: renamed}
	parent := outcomeNaming(culprit)

	variant := outcomeNaming(renamed)
	variant.FixtureID = "rollout-regression-01-incident-name-permuted"
	if got := eval.CheckInvariance(parent, variant, eval.TransformNamePermuted, names); !got.Held {
		t.Errorf("the renamed culprit failed the invariant: %s", got.Detail)
	}

	// The engine answering the *original* name under the permutation is exactly the failure the
	// transform exists to catch — a ranker that recognised the name rather than the structure.
	stale := outcomeNaming(culprit)
	stale.FixtureID = variant.FixtureID
	got := eval.CheckInvariance(parent, stale, eval.TransformNamePermuted, names)
	if got.Held {
		t.Error("a variant answering the un-permuted name held the invariant")
	}
	if got.ExpectedVerdict != renamed {
		t.Errorf("expected verdict %q, want the image %q", got.ExpectedVerdict, renamed)
	}
}

// TestCulpritDeletedExpectsUnobservedWithTheSymptomLocalised is the one transform that changes the
// expected answer.
func TestCulpritDeletedExpectsUnobservedWithTheSymptomLocalised(t *testing.T) {
	parent := outcomeNaming(culprit)

	good := outcomeUnobserved()
	good.FixtureID = "rollout-regression-01-incident-culprit-deleted"
	got := eval.CheckInvariance(parent, good, eval.TransformCulpritDeleted, nil)
	if !got.Held {
		t.Errorf("`unobserved` with the symptom localised failed: %s", got.Detail)
	}
	if got.ExpectedVerdict != eval.VerdictUnobserved {
		t.Errorf("expected verdict %q", got.ExpectedVerdict)
	}

	namedDecoy := outcomeNaming(decoy)
	namedDecoy.FixtureID = good.FixtureID
	got = eval.CheckInvariance(parent, namedDecoy, eval.TransformCulpritDeleted, nil)
	if got.Held {
		t.Fatal("a culprit-deleted variant that named a decoy held the invariant")
	}
	if got.NamedDecoy != decoy {
		t.Errorf("the named decoy is not reported: %+v", got)
	}

	unknown := outcomeUnknown()
	unknown.FixtureID = good.FixtureID
	if got := eval.CheckInvariance(parent, unknown, eval.TransformCulpritDeleted, nil); got.Held {
		t.Error("`unknown` held a culprit-deleted invariant; the expected answer is `unobserved`")
	}

	unlocalised := outcomeUnobserved()
	unlocalised.FixtureID = good.FixtureID
	unlocalised.CausalPath = nil
	if got := eval.CheckInvariance(parent, unlocalised, eval.TransformCulpritDeleted, nil); got.Held {
		t.Error("`unobserved` localising nothing held the invariant")
	}
}

// TestAnUnpublishedTransformIsNotVacuouslyTrue: a variant whose transform publishes no relation
// cannot be graded metamorphically, and reporting it as held would be a green row for a check
// nobody made.
func TestAnUnpublishedTransformIsNotVacuouslyTrue(t *testing.T) {
	got := eval.CheckInvariance(outcomeNaming(culprit), outcomeNaming(culprit), "colour-inverted", nil)
	if got.Held {
		t.Error("an unpublished transform held")
	}
	if !contains(got.Detail, "colour-inverted") {
		t.Errorf("the detail does not name the transform: %s", got.Detail)
	}
}

// TestTheSummaryCountsWhatHeldAndRefusesAnEmptyRate: SC-020 is a percentage, and a percentage
// over nothing is not 100 %.
func TestTheSummaryCountsWhatHeldAndRefusesAnEmptyRate(t *testing.T) {
	if _, ok := eval.Summarise(nil).Rate(); ok {
		t.Error("an empty set reported a rate; no invariants were checked")
	}
	parent := outcomeNaming(culprit)
	held := eval.CheckInvariance(parent, outcomeNaming(culprit), eval.TransformTimeShifted, nil)
	broke := eval.CheckInvariance(parent, outcomeNaming(decoy), eval.TransformTimeShifted, nil)
	summary := eval.Summarise([]eval.InvarianceResult{held, broke})
	if summary.Checked != 2 || summary.Held != 1 || len(summary.Failures) != 1 {
		t.Errorf("summary %+v", summary)
	}
	if rate, ok := summary.Rate(); !ok || rate != 0.5 {
		t.Errorf("rate %v ok=%t, want 0.5", rate, ok)
	}
	if !contains(summary.String(), "1 of 2 held") {
		t.Errorf("the summary line does not count: %s", summary.String())
	}
}

// TestNamePermutationMapsTheNameInsideAReference is the defect Phase 8 Track K-B found: the map
// `fixture derive` publishes in `derived.json` is keyed on **bare names**, because that is what it
// permutes, while everything graded against it is a full reference. A check that looked the whole
// reference up found nothing, left it unchanged, and failed the one variant the transform exists
// to reward — the one that answered its own published culprit.
func TestNamePermutationMapsTheNameInsideAReference(t *testing.T) {
	// The shape `fixture derive` writes into the variant's `derived.json`, verbatim.
	const derived = `{
	  "parent_id": "rollout-regression-01-incident",
	  "variant_id": "rollout-regression-01-incident-name-permuted",
	  "transform": "name-permuted",
	  "events": 41,
	  "culprit": "k8s.change=shop/ledger@rev7",
	  "name_map": {"payments": "ledger", "ledger": "checkout", "checkout": "payments"},
	  "world_pending": true,
	  "goldens_pending": true
	}`
	var report struct {
		Culprit string       `json:"culprit"`
		Names   eval.NameMap `json:"name_map"`
	}
	if err := json.Unmarshal([]byte(derived), &report); err != nil {
		t.Fatalf("parse derived.json: %v", err)
	}

	// `culprit` is `k8s.change=shop/payments@rev7`: namespace, path and revision are carried
	// through, and only the name between them moves.
	if got := report.Names.Image(culprit); got != report.Culprit {
		t.Fatalf("Image(%q) = %q, want the variant's published culprit %q; the namespace, the `shop/` "+
			"path and the `@rev7` revision are not permuted and must survive the mapping",
			culprit, got, report.Culprit)
	}

	parent := outcomeNaming(culprit)
	variant := outcomeNaming(report.Culprit)
	variant.FixtureID = "rollout-regression-01-incident-name-permuted"
	got := eval.CheckInvariance(parent, variant, eval.TransformNamePermuted, report.Names)
	if !got.Held {
		t.Errorf("the variant answering its own published culprit failed the invariant: %s", got.Detail)
	}

	// A name the map does not carry is left alone, and a reference with no namespace or path is
	// mapped on its bare name.
	if got := report.Names.Image("k8s.change=shop/unknown@rev7"); got != "k8s.change=shop/unknown@rev7" {
		t.Errorf("Image of an unmapped name = %q, want it unchanged", got)
	}
	if got := report.Names.Image("otel.service.name=payments"); got != "otel.service.name=ledger" {
		t.Errorf("Image(%q) = %q, want %q", "otel.service.name=payments", got, "otel.service.name=ledger")
	}
}
