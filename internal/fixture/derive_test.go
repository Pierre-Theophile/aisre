// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// The metamorphic generator (T107, FR-062a, SC-020).
//
// Nothing here opens a database. `Derive` writes a manifest, an events file and the payloads; the
// world and the goldens are recorded above this package and are asserted end to end by
// `internal/cli`'s own derive test. What is asserted here is the part that must be a pure
// function of the parent: the events the transform moved, the image it takes of the ground truth,
// and that running it twice produces the same bytes.

const incidentParent = "../../fixtures/incidents/rollout-regression-01-incident"

func derive(t *testing.T, transform string, opts ...func(*fixture.DeriveOptions)) (string, *fixture.DeriveReport) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "rollout-regression-01-incident-"+transform)
	options := fixture.DeriveOptions{Transform: transform, Out: out}
	for _, apply := range opts {
		apply(&options)
	}
	report, err := fixture.Derive(incidentParent, options)
	if err != nil {
		t.Fatalf("Derive %s: %v", transform, err)
	}
	return out, report
}

// loadVariant loads the generated manifest, which is the assertion that a variant is a *fixture*
// — the loader is strict, and anything the generator wrote that the format does not publish would
// be refused here rather than three steps later.
func loadVariant(t *testing.T, dir string) *fixture.Manifest {
	t.Helper()
	m, err := fixture.LoadManifest(dir)
	if err != nil {
		t.Fatalf("the generated manifest does not load: %v", err)
	}
	return m
}

// TestEveryTransformProducesALoadableFixtureThatNamesItsParent is the property the contract
// states about all four: a variant records its parent and its transformation, and is a fixture.
func TestEveryTransformProducesALoadableFixtureThatNamesItsParent(t *testing.T) {
	for _, transform := range fixture.Transforms {
		t.Run(transform, func(t *testing.T) {
			dir, report := derive(t, transform)
			m := loadVariant(t, dir)

			if m.ID != filepath.Base(dir) {
				t.Errorf("id %q, want the directory name %q", m.ID, filepath.Base(dir))
			}
			provenance := m.Incident.GroundTruth.Provenance
			if provenance.Kind != "derived" {
				t.Errorf("provenance kind %q, want derived", provenance.Kind)
			}
			if provenance.DerivedFrom != "rollout-regression-01-incident" {
				t.Errorf("derived_from %q", provenance.DerivedFrom)
			}
			if !strings.HasPrefix(provenance.Transformation, transform) {
				t.Errorf("transformation %q does not name the transform", provenance.Transformation)
			}
			if !report.WorldPending || !report.GoldensPending {
				t.Error("the report claims the world and goldens are recorded; this package cannot record either")
			}
			if m.HandAuthored {
				t.Error("a generated variant is marked hand_authored; `fixture record` must be free to " +
					"rewrite events that came from a generator")
			}
			// The generator's own account of itself travels with the variant, because the
			// invariance check reads the permutation from it rather than re-deriving it.
			if _, ok, err := fixture.ReadDeriveReport(dir); err != nil || !ok {
				t.Errorf("ReadDeriveReport: ok=%t err=%v", ok, err)
			}
		})
	}
}

// TestDerivingTwiceYieldsIdenticalBytes: a generator that did not would make every metamorphic
// failure unreproducible, and "regenerate it rather than committing it" would stop being a
// defensible choice.
func TestDerivingTwiceYieldsIdenticalBytes(t *testing.T) {
	for _, transform := range fixture.Transforms {
		t.Run(transform, func(t *testing.T) {
			first, _ := derive(t, transform)
			second, _ := derive(t, transform)
			for _, name := range []string{fixture.ManifestFile, "events.jsonl", fixture.DeriveReportFile} {
				a := readFile(t, filepath.Join(first, name))
				b := readFile(t, filepath.Join(second, name))
				if !bytes.Equal(a, b) {
					t.Errorf("%s differs between two derivations (%d vs %d bytes)", name, len(a), len(b))
				}
			}
		})
	}
}

// TestCulpritDeletedRemovesTheCulpritAndTakesTheGroundTruthToUnobserved is the one transform that
// changes the expected answer, and the invariant it is graded on is that change.
func TestCulpritDeletedRemovesTheCulpritAndTakesTheGroundTruthToUnobserved(t *testing.T) {
	parent := loadVariant(t, incidentParent)
	dir, report := derive(t, fixture.TransformCulpritDeleted)
	m := loadVariant(t, dir)

	if m.Incident.GroundTruth.Culprit != fixture.CulpritUnobserved {
		t.Errorf("culprit %q, want %q", m.Incident.GroundTruth.Culprit, fixture.CulpritUnobserved)
	}
	if m.Incident.GroundTruth.Class() != fixture.CulpritUnobserved {
		t.Errorf("class %q", m.Incident.GroundTruth.Class())
	}
	if report.Removed == 0 {
		t.Error("nothing was removed")
	}

	// The symptom survives: the path still ends where the parent's did, which is what SC-023
	// grades a culprit-deleted variant on.
	parentPath := parent.Incident.GroundTruth.CausalPath
	variantPath := m.Incident.GroundTruth.CausalPath
	if len(variantPath) == 0 {
		t.Fatal("the causal path is empty; `unobserved` with nothing localised is a shrug")
	}
	if variantPath[len(variantPath)-1].Entity != parentPath[len(parentPath)-1].Entity {
		t.Errorf("the symptom moved from %q to %q",
			parentPath[len(parentPath)-1].Entity, variantPath[len(variantPath)-1].Entity)
	}
	if variantPath[0].Entity == parent.Incident.GroundTruth.Culprit {
		t.Error("the causal path still starts at the deleted culprit")
	}

	// No event names the culprit any more — which is the whole transform.
	body := readFile(t, filepath.Join(dir, "events.jsonl"))
	if bytes.Contains(body, []byte("shop/payments@rev7")) {
		t.Error("an event still names the deleted culprit")
	}
	// The decoys survive, because naming one is what the variant is graded against.
	if len(m.Incident.GroundTruth.Decoys) != len(parent.Incident.GroundTruth.Decoys) {
		t.Errorf("%d decoys, parent had %d; a culprit-deleted variant is graded on not naming one",
			len(m.Incident.GroundTruth.Decoys), len(parent.Incident.GroundTruth.Decoys))
	}
}

// TestDecoyInjectedAddsOnePlausibleChangeWithSomethingAgainstIt.
func TestDecoyInjectedAddsOnePlausibleChangeWithSomethingAgainstIt(t *testing.T) {
	parent := loadVariant(t, incidentParent)
	dir, report := derive(t, fixture.TransformDecoyInjected)
	m := loadVariant(t, dir)

	if report.Added != 1 || report.InjectedDecoy == "" {
		t.Fatalf("added=%d decoy=%q", report.Added, report.InjectedDecoy)
	}
	if m.Incident.GroundTruth.Culprit != parent.Incident.GroundTruth.Culprit {
		t.Errorf("the culprit moved to %q; only culprit-deleted changes the expected answer",
			m.Incident.GroundTruth.Culprit)
	}
	if len(m.Incident.GroundTruth.Decoys) != len(parent.Incident.GroundTruth.Decoys)+1 {
		t.Errorf("%d decoys, want one more than the parent's %d",
			len(m.Incident.GroundTruth.Decoys), len(parent.Incident.GroundTruth.Decoys))
	}
	var found bool
	for _, decoy := range m.Incident.GroundTruth.Decoys {
		if decoy.Entity != report.InjectedDecoy {
			continue
		}
		found = true
		if decoy.CausalRole != "coincident" {
			t.Errorf("the injected decoy's role is %q, want coincident", decoy.CausalRole)
		}
	}
	if !found {
		t.Fatalf("the injected change %s is not declared a decoy", report.InjectedDecoy)
	}
	// The exonerating predicate has to exist, and has to be written over the manifest's own
	// window grid, which is what makes it a question the regenerated world answers.
	predicates := m.Incident.GroundTruth.ExoneratingEvidence[report.InjectedDecoy]
	if len(predicates) == 0 {
		t.Fatal("the injected decoy is exonerated by nothing, which only `not_separable` may be")
	}
	windows, _ := predicates[0].Term["compare"].(map[string]any)
	if windows == nil {
		t.Fatalf("the exonerating predicate is not a compare: %+v", predicates[0].Term)
	}
	// The injected change is inside the question's window, which is what makes it plausible.
	if report.Events != len(mustReadEvents(t, parent.EventsPath()))+1 {
		t.Errorf("%d events, want one more than the parent's", report.Events)
	}
}

// TestTimeShiftMovesEveryInstantAndNothingElse.
func TestTimeShiftMovesEveryInstantAndNothingElse(t *testing.T) {
	parent := loadVariant(t, incidentParent)
	const offset = 48 * time.Hour
	dir, report := derive(t, fixture.TransformTimeShifted, func(o *fixture.DeriveOptions) { o.Offset = offset })
	m := loadVariant(t, dir)

	if report.Offset != offset {
		t.Errorf("offset %s, want %s", report.Offset, offset)
	}
	for _, pair := range []struct {
		name      string
		want, got time.Time
	}{
		{"fired_at", parent.Incident.Question.FiredAt.Add(offset), m.Incident.Question.FiredAt},
		{"knowability_time", parent.Incident.GroundTruth.KnowabilityTime.Add(offset), m.Incident.GroundTruth.KnowabilityTime},
		{"clock.start", parent.Clock.Start.Add(offset), m.Clock.Start},
		{"clock.end", parent.Clock.End.Add(offset), m.Clock.End},
		{"onset.at", parent.Incident.GroundTruth.Onset.At.Add(offset), m.Incident.GroundTruth.Onset.At},
		{"window_grid[0]", parent.Incident.World.WindowGrid[0].ReferenceAt.Add(offset), m.Incident.World.WindowGrid[0].ReferenceAt},
	} {
		if !pair.got.Equal(pair.want) {
			t.Errorf("%s = %s, want %s", pair.name, pair.got.Format(time.RFC3339), pair.want.Format(time.RFC3339))
		}
	}
	// The relation between the instants is what must not move: the question is still the same
	// distance ahead of the onset.
	parentGap := parent.Incident.Question.FiredAt.Sub(parent.Incident.GroundTruth.Onset.At)
	variantGap := m.Incident.Question.FiredAt.Sub(m.Incident.GroundTruth.Onset.At)
	if parentGap != variantGap {
		t.Errorf("the gap between onset and alert moved from %s to %s; the transform is meant to be "+
			"causally neutral", parentGap, variantGap)
	}
	if m.Incident.GroundTruth.Culprit != parent.Incident.GroundTruth.Culprit {
		t.Errorf("the culprit moved to %q", m.Incident.GroundTruth.Culprit)
	}
	// The events moved too, or the graph and the question would be a day apart.
	body := readFile(t, filepath.Join(dir, "events.jsonl"))
	if bytes.Contains(body, []byte("2026-09-01T14:20:00Z")) {
		t.Error("an event still carries an un-shifted instant")
	}
}

// TestNamePermutationIsANamespacePreservingBijectionAppliedEverywhere.
func TestNamePermutationIsANamespacePreservingBijectionAppliedEverywhere(t *testing.T) {
	parent := loadVariant(t, incidentParent)
	dir, report := derive(t, fixture.TransformNamePermuted)
	m := loadVariant(t, dir)

	if len(report.NameMap) < 2 {
		t.Fatalf("the permutation moves %d name(s)", len(report.NameMap))
	}
	images := map[string]string{}
	for name, image := range report.NameMap {
		if name == image {
			t.Errorf("%q maps to itself; the permutation is meant to be a derangement", name)
		}
		if previous, clash := images[image]; clash {
			t.Errorf("%q and %q both map to %q; the map is not a bijection", previous, name, image)
		}
		images[image] = name
	}

	// The culprit is renamed through the same map, and its namespace and revision survive: a
	// permutation that moved either would be changing the structure rather than the names.
	if m.Incident.GroundTruth.Culprit == parent.Incident.GroundTruth.Culprit {
		t.Fatal("the culprit was not renamed")
	}
	if !strings.HasPrefix(m.Incident.GroundTruth.Culprit, "k8s.change=shop/") ||
		!strings.HasSuffix(m.Incident.GroundTruth.Culprit, "@rev7") {
		t.Errorf("the culprit %q lost its namespace or its revision", m.Incident.GroundTruth.Culprit)
	}
	if want := "k8s.change=shop/" + report.NameMap["payments"] + "@rev7"; m.Incident.GroundTruth.Culprit != want {
		t.Errorf("the culprit is %q, want the image %q", m.Incident.GroundTruth.Culprit, want)
	}

	// Every decoy goes through the same map, or the fixture would declare decoys the graph no
	// longer holds.
	if len(m.Incident.GroundTruth.Decoys) != len(parent.Incident.GroundTruth.Decoys) {
		t.Fatalf("%d decoys, parent had %d", len(m.Incident.GroundTruth.Decoys), len(parent.Incident.GroundTruth.Decoys))
	}
	for i, decoy := range m.Incident.GroundTruth.Decoys {
		if decoy.Entity == parent.Incident.GroundTruth.Decoys[i].Entity {
			t.Errorf("decoy %s was not renamed", decoy.Entity)
		}
		if _, declared := m.Incident.GroundTruth.ExoneratingEvidence[decoy.Entity]; !declared {
			t.Errorf("decoy %s has no exonerating evidence; the map did not reach the keys", decoy.Entity)
		}
	}

	// And the events: the culprit's own change is gone from them under its old name, so the
	// regenerated world cannot answer with it. The *workload* name may legitimately reappear —
	// the permutation is a derangement, so some other service is now called `payments` — which
	// is exactly why the assertion is over the change ref and not over the bare name.
	body := readFile(t, filepath.Join(dir, "events.jsonl"))
	if bytes.Contains(body, []byte("shop/payments@rev7")) {
		t.Error("an event still names the parent's culprit change")
	}
	if !bytes.Contains(body, []byte("shop/"+report.NameMap["payments"]+"@rev7")) {
		t.Errorf("no event names the renamed culprit shop/%s@rev7", report.NameMap["payments"])
	}
}

// TestTheSeedDecidesThePermutation: a variant is reproducible by name, and a different seed is a
// different variant.
func TestTheSeedDecidesThePermutation(t *testing.T) {
	_, first := derive(t, fixture.TransformNamePermuted)
	_, again := derive(t, fixture.TransformNamePermuted)
	if first.NameMap["payments"] != again.NameMap["payments"] {
		t.Error("the default seed produced two different permutations")
	}
	_, other := derive(t, fixture.TransformNamePermuted,
		func(o *fixture.DeriveOptions) { o.Seed = "a-different-seed" })
	if other.NameMap["payments"] == first.NameMap["payments"] {
		t.Error("a different seed produced the same permutation")
	}
}

// TestDeriveRefusesWhatItCannotDo: an unpublished transform, a parent with no incident block, and
// a non-empty output directory. Each would otherwise produce a directory that looks like a
// fixture and is not one.
func TestDeriveRefusesWhatItCannotDo(t *testing.T) {
	out := t.TempDir()

	if _, err := fixture.Derive(incidentParent, fixture.DeriveOptions{
		Transform: "colour-inverted", Out: filepath.Join(out, "v"),
	}); err == nil {
		t.Error("an unpublished transform was accepted")
	}
	if _, err := fixture.Derive("../../fixtures/rollout-regression-01", fixture.DeriveOptions{
		Transform: fixture.TransformTimeShifted, Out: filepath.Join(out, "v2"),
	}); err == nil {
		t.Error("a parent with no `incident:` block was accepted")
	}

	occupied := filepath.Join(out, "occupied")
	if err := os.MkdirAll(occupied, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "world.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Derive(incidentParent, fixture.DeriveOptions{
		Transform: fixture.TransformTimeShifted, Out: occupied,
	}); err == nil {
		t.Error("a non-empty output directory was overwritten without --force")
	}
	if _, err := fixture.Derive(incidentParent, fixture.DeriveOptions{
		Transform: fixture.TransformTimeShifted, Out: occupied, Force: true,
	}); err != nil {
		t.Errorf("--force was refused: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a path this test built
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func mustReadEvents(t *testing.T, path string) []fixture.Event {
	t.Helper()
	events, err := fixture.ReadEvents(path)
	if err != nil {
		t.Fatalf("ReadEvents %s: %v", path, err)
	}
	return events
}

// TestARefusedTransformLeavesNothingBehind is the defect Phase 8 Track K-B found: Derive creates
// the output directory before it can know whether the transform applies — the answer is in the
// parent's events — so a refusal used to leave an empty directory where a variant was not written.
// The next run then found that directory non-empty, refused for an entirely different reason, and
// left a reviewer to delete it by hand.
func TestARefusedTransformLeavesNothingBehind(t *testing.T) {
	// A fixture whose truth is already `unobserved` has no culprit to delete, which is a refusal
	// the transform can only reach after the directory exists.
	const unobservedParent = "../../fixtures/incidents/unobserved-latent-bug-01"
	out := filepath.Join(t.TempDir(), "unobserved-latent-bug-01-culprit-deleted")

	_, err := fixture.Derive(unobservedParent, fixture.DeriveOptions{
		Transform: fixture.TransformCulpritDeleted, Out: out,
	})
	if err == nil {
		t.Fatal("a parent whose truth is already `unobserved` was accepted for culprit-deleted")
	}
	if reason := fixture.RefusalReason(err); reason != fixture.ReasonUnusableGroundTruth {
		t.Errorf("refusal reason = %q, want %q; a caller that skips a parent rather than failing a batch "+
			"has to tell a refusal from a broken run without reading prose (%v)",
			reason, fixture.ReasonUnusableGroundTruth, err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("the refused derivation left %s behind (%v); a refusal writes nothing", out, statErr)
	}

	// The same refusal, twice, is the same refusal: the second run is not a report about the
	// wreckage of the first.
	_, again := fixture.Derive(unobservedParent, fixture.DeriveOptions{
		Transform: fixture.TransformCulpritDeleted, Out: out,
	})
	if fixture.RefusalReason(again) != fixture.ReasonUnusableGroundTruth {
		t.Errorf("the second refusal reads %v, want the same refusal as the first", again)
	}
}

// TestDeriveDoesNotRemoveADirectoryItDidNotCreate is the other half of the rule: a --force
// derivation over somebody's existing variant must not take it with it when it refuses.
func TestDeriveDoesNotRemoveADirectoryItDidNotCreate(t *testing.T) {
	const unobservedParent = "../../fixtures/incidents/unobserved-latent-bug-01"
	out := filepath.Join(t.TempDir(), "existing")
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(out, "manifest.yaml")
	if err := os.WriteFile(keep, []byte("id: somebody-elses-variant\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.Derive(unobservedParent, fixture.DeriveOptions{
		Transform: fixture.TransformCulpritDeleted, Out: out, Force: true,
	}); err == nil {
		t.Fatal("the refusal did not happen, so this test asserts nothing")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a refused --force derivation removed a directory it did not create: %v", err)
	}
}
