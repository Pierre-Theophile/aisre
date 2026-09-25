// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"sort"
	"strings"
)

// The metamorphic invariance checks (T108; FR-062a, SC-020,
// contracts/incident-format.md §Grading rules that are easy to get wrong).
//
// A metamorphic test does not need a ground truth for the variant: it needs the *relation*
// between the parent's answer and the variant's, and the transform is what publishes that
// relation. That is why the four transforms are defined as generators (T107) rather than as
// checked-in fixtures — a variant that cannot state its own invariant is a fixture nobody can
// grade — and why the check below takes the transform by name.
//
// The four relations, from FR-062a:
//
//   - **culprit-deleted** ⇒ the answer becomes `unobserved`, with the symptoms still localised,
//     and naming a decoy fails. This is the one transform that changes the expected answer;
//   - **decoy-injected** ⇒ the verdict does not change. A far-away plausible change must not
//     out-rank the real one;
//   - **time-shifted** ⇒ the verdict does not change. Shifting every instant by a constant
//     changes nothing causal, so an engine whose answer moves is reading absolute time;
//   - **name-permuted** ⇒ the verdict, mapped through the permutation, does not change. This is
//     the transform that exists to test that the engine ranks on **structure** rather than on
//     guilty-sounding names, which is why the culprit and every decoy are renamed through the
//     same bijection and the check compares the *image* of the parent's verdict.
//
// A divergence fails the run and **names the variant and both verdicts**, because "a metamorphic
// invariant broke" is not a sentence anybody can act on and "rollout-regression-01-incident
// answered k8s.change=shop/payments@rev7 and its name-permuted variant answered
// k8s.change=shop/inventory@rev8" is.

// The four published transforms, spelled as `fixture derive --transform` takes them.
const (
	// TransformCulpritDeleted removes the culprit's events; the expected answer becomes
	// `unobserved`.
	TransformCulpritDeleted = "culprit-deleted"
	// TransformDecoyInjected adds a plausible change on an adjacent entity inside the window.
	TransformDecoyInjected = "decoy-injected"
	// TransformTimeShifted shifts every instant by a fixed offset.
	TransformTimeShifted = "time-shifted"
	// TransformNamePermuted applies a deterministic, namespace-preserving bijection over entity
	// names.
	TransformNamePermuted = "name-permuted"
)

// Transforms is the published set, in the order the contract lists them.
var Transforms = []string{
	TransformCulpritDeleted, TransformDecoyInjected, TransformTimeShifted, TransformNamePermuted,
}

// InvarianceResult is one parent/variant pair checked against the invariant its transform
// defines.
//
// It is a flat struct with exported fields and no rendering of its own, so that the report
// writer (T109) can emit it as an `InvarianceRow` without this package knowing anything about the
// report's shape, and so that a divergence carries its own sentence wherever it is printed.
type InvarianceResult struct {
	// Parent and Variant are the fixture ids, and Transform the relation being checked.
	Parent    string `json:"parent"`
	Variant   string `json:"variant"`
	Transform string `json:"transform"`
	// Held is the answer. A transform this package does not publish is reported as not held,
	// with the reason, rather than as vacuously true.
	Held bool `json:"held"`
	// ParentVerdict and VariantVerdict are what the two runs answered, always published —
	// including on a pass, because a reviewer reading a green report wants to see what was
	// compared.
	ParentVerdict  string `json:"parent_verdict"`
	VariantVerdict string `json:"variant_verdict"`
	// ExpectedVerdict is what the invariant required of the variant: the parent's verdict, its
	// image under the permutation, or `unobserved`.
	ExpectedVerdict string `json:"expected_verdict"`
	// Detail is the sentence a failing run prints. It names the variant and both verdicts.
	Detail string `json:"detail,omitempty"`
	// Localised is meaningful for culprit-deleted only: the symptom is still placed.
	Localised bool `json:"localised,omitempty"`
	// NamedDecoy is the decoy a culprit-deleted variant named, when it named one. Naming a
	// decoy is the specific failure SC-020 counts.
	NamedDecoy string `json:"named_decoy,omitempty"`
}

// NameMap is the transform's own bijection over entity names, as `name-permuted` publishes it in
// the variant's provenance. It maps a parent name to its image.
//
// The map is keyed on **bare names** — `payments`, `checkout` — because that is what
// `fixture derive` permutes: the last `/`-separated segment of a ref value, with the namespace
// prefix and any `@revision` suffix deliberately left alone (internal/fixture/derive.go,
// `permutableNames`). Everything this package compares against it, on the other hand, is a *full
// reference*: a verdict is `k8s.change=shop/payments@rev7`, never `payments`. Image is where the
// two spellings are reconciled.
type NameMap map[string]string

// Image maps a name through the permutation, returning it unchanged where the map does not name
// it — which is correct for every identifier the transform left alone.
//
// A full reference is mapped **component-wise**: the namespace (`k8s.change=`), the path prefix
// (`shop/`) and the revision suffix (`@rev7`) are carried through untouched and only the name
// segment between them is mapped, which is exactly the substitution `fixture derive` applied to
// the variant's events. Before this was true the check compared `k8s.change=shop/payments@rev7`
// against itself, and a name-permuted variant that answered its own published culprit — the one
// answer the transform exists to reward — was graded as a divergence.
func (m NameMap) Image(name string) string {
	if m == nil || name == "" {
		return name
	}
	// A map keyed on whole references still works: the transform publishes bare names, but a
	// caller that wrote down a reference means it.
	if image, ok := m[name]; ok {
		return image
	}
	namespace, value := "", name
	if at := strings.LastIndex(name, "="); at >= 0 {
		namespace, value = name[:at+1], name[at+1:]
	}
	path := ""
	if at := strings.LastIndex(value, "/"); at >= 0 {
		path, value = value[:at+1], value[at+1:]
	}
	revision := ""
	if at := strings.Index(value, "@"); at >= 0 {
		value, revision = value[:at], value[at:]
	}
	image, ok := m[value]
	if !ok {
		return name
	}
	return namespace + path + image + revision
}

// CheckInvariance checks one variant's outcome against its parent's, under the invariant the
// transform defines.
//
// `names` is the transform's own map and is used only by `name-permuted`; the other three pass
// nil. It is a parameter rather than a lookup because the map is a property of the *variant* —
// `fixture derive` writes it into the variant's provenance — and a checker that re-derived it
// would be checking its own re-derivation.
func CheckInvariance(parent, variant *RunOutcome, transform string, names NameMap) InvarianceResult {
	result := InvarianceResult{Transform: transform}
	if parent == nil || variant == nil {
		result.Detail = "the invariant needs both the parent's run and the variant's; one of them is missing"
		return result
	}
	result.Parent, result.Variant = parent.FixtureID, variant.FixtureID
	result.ParentVerdict, result.VariantVerdict = parent.Verdict, variant.Verdict

	switch transform {
	case TransformCulpritDeleted:
		return checkCulpritDeleted(parent, variant, result)
	case TransformDecoyInjected, TransformTimeShifted:
		result.ExpectedVerdict = parent.Verdict
	case TransformNamePermuted:
		result.ExpectedVerdict = names.Image(parent.Verdict)
	default:
		result.Detail = fmt.Sprintf(
			"%q is not one of the published transforms %v, so there is no invariant to check; a variant "+
				"whose transform publishes no relation cannot be graded metamorphically (FR-062a)",
			transform, Transforms)
		return result
	}

	if variantNames(variant, result.ExpectedVerdict) {
		result.Held = true
		return result
	}
	result.Detail = fmt.Sprintf(
		"the %s variant %s of %s answered %q where the invariant requires %q (the parent answered %q); "+
			"a divergence fails the run (FR-062a, SC-020)",
		transform, result.Variant, result.Parent, result.VariantVerdict, result.ExpectedVerdict,
		result.ParentVerdict)
	return result
}

// checkCulpritDeleted is the one transform that changes the expected answer.
func checkCulpritDeleted(parent, variant *RunOutcome, result InvarianceResult) InvarianceResult {
	_ = parent
	result.ExpectedVerdict = VerdictUnobserved
	result.Localised = len(variant.CausalPath) > 0

	if variant.VerdictClass() == "change_induced" {
		result.NamedDecoy = variant.Verdict
		result.Detail = fmt.Sprintf(
			"the culprit-deleted variant %s of %s named %s; with the culprit's events removed the expected "+
				"answer is `unobserved`, and naming any remaining candidate is naming a decoy (FR-062a, SC-020)",
			result.Variant, result.Parent, variant.Verdict)
		return result
	}
	if variant.VerdictClass() != VerdictUnobserved {
		result.Detail = fmt.Sprintf(
			"the culprit-deleted variant %s of %s answered %q; the expected answer is `unobserved` with the "+
				"symptom still localised (FR-062a)", result.Variant, result.Parent, variant.Verdict)
		return result
	}
	if !result.Localised {
		result.Detail = fmt.Sprintf(
			"the culprit-deleted variant %s of %s answered `unobserved` but localised nothing; the symptoms "+
				"must still be localised (FR-062a, SC-023)", result.Variant, result.Parent)
		return result
	}
	result.Held = true
	return result
}

// variantNames reports whether the variant's verdict is the expected one, in any spelling the
// graph publishes for it.
func variantNames(variant *RunOutcome, want string) bool {
	if variant.Verdict == want {
		return true
	}
	for _, ref := range variant.VerdictRefs {
		if ref == want {
			return true
		}
	}
	return false
}

// String renders the result as the one line a job summary shows.
func (r InvarianceResult) String() string {
	if r.Held {
		return fmt.Sprintf("%s %s: held (%s answered %q, as the parent did)",
			r.Transform, r.Variant, r.Variant, r.VariantVerdict)
	}
	return r.Detail
}

// InvarianceSummary aggregates results, which is what SC-020's "100 %" is measured over.
type InvarianceSummary struct {
	// Checked and Held are the counts, and ByTransform the same split by transform so a single
	// broken relation is visible rather than averaged away.
	Checked     int            `json:"checked"`
	Held        int            `json:"held"`
	ByTransform map[string]int `json:"by_transform,omitempty"`
	// Failures are the failing results, in the order they were checked.
	Failures []InvarianceResult `json:"failures,omitempty"`
}

// Summarise aggregates a set of results.
func Summarise(results []InvarianceResult) InvarianceSummary {
	out := InvarianceSummary{ByTransform: map[string]int{}}
	for _, result := range results {
		out.Checked++
		if result.Held {
			out.Held++
			out.ByTransform[result.Transform]++
			continue
		}
		out.Failures = append(out.Failures, result)
	}
	if len(out.ByTransform) == 0 {
		out.ByTransform = nil
	}
	return out
}

// Rate is the share of invariants that held, and false when nothing was checked — an empty
// corpus is not a corpus whose invariants all hold.
func (s InvarianceSummary) Rate() (float64, bool) {
	if s.Checked == 0 {
		return 0, false
	}
	return float64(s.Held) / float64(s.Checked), true
}

// String renders the summary for a job summary.
func (s InvarianceSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "metamorphic invariants: %d of %d held", s.Held, s.Checked)
	if len(s.ByTransform) > 0 {
		keys := make([]string, 0, len(s.ByTransform))
		for key := range s.ByTransform {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", key, s.ByTransform[key]))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	for _, failure := range s.Failures {
		fmt.Fprintf(&b, "\n  - %s", failure.Detail)
	}
	return b.String()
}
