// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/fixture/fixturetest"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// `fixture derive` end to end (T107, FR-062a).
//
// `internal/fixture` asserts what the transform does to the manifest and the events. What can
// only be asserted here is the half that needs a database and the engine: that a variant's
// `world/` is **regenerated from its own graph** rather than copied, that its goldens are
// re-recorded from its own events, and that the result passes the same `fixture verify` every
// hand-authored fixture passes.
//
// Two transforms are exercised rather than four, and the pair is chosen on purpose.
// `culprit-deleted` is the one that changes the ground truth, so it is the one whose world must
// be recorded *after* the rewrite — a world recorded from the parent's block would still degrade
// a culprit this fixture no longer has. `name-permuted` is the one where a copied world would be
// silently useless: every term key is a function of the pointer, every pointer names the service,
// and a variant asking about renamed services against the parent's recording would miss all of
// them. The other two would exercise the same two code paths again.

func TestADerivedVariantIsRecordedFromItsOwnGraphAndVerifies(t *testing.T) {
	ctx := context.Background()

	for _, transform := range []string{fixture.TransformCulpritDeleted, fixture.TransformNamePermuted} {
		t.Run(transform, func(t *testing.T) {
			newStore := fixturetest.PgtestFactory(t)
			out := filepath.Join(t.TempDir(), "rollout-regression-01-incident-"+transform)

			report, err := fixture.Derive(incidentFixture, fixture.DeriveOptions{
				Transform: transform, Out: out,
			})
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if err := recordVariant(ctx, newStore, report, audit.PriorRecord{}); err != nil {
				t.Fatalf("recordVariant: %v", err)
			}
			if report.WorldPending || report.GoldensPending {
				t.Fatalf("the report still says world=%t goldens=%t pending",
					report.WorldPending, report.GoldensPending)
			}

			// The world is the variant's own. Its index names the focus the variant asks about
			// and holds terms; a copied one would name the parent's focus.
			index, ok, err := fixture.ReadWorldIndex(out)
			if err != nil || !ok {
				t.Fatalf("ReadWorldIndex: ok=%t err=%v", ok, err)
			}
			if index.TermCount == 0 {
				t.Error("the regenerated world holds no terms")
			}
			m, err := fixture.LoadManifest(out)
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if index.Focus != m.Incident.Question.Subject {
				t.Errorf("the world was recorded around %q, but the variant asks about %q; a world "+
					"recorded around the parent's focus answers a different fixture's questions",
					index.Focus, m.Incident.Question.Subject)
			}
			if index.MissRate > m.Incident.World.MissRateThreshold {
				t.Errorf("miss rate %.4f above the fixture's own threshold %.4f",
					index.MissRate, m.Incident.World.MissRateThreshold)
			}

			// And the whole thing verifies, which is the contract's own requirement of a derived
			// fixture: it is a fixture, not a directory that looks like one.
			verified, err := fixture.Verify(ctx, newStore, out, fixture.VerifyOptions{
				NewRunner: newQueryRunner,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !verified.Passed {
				for _, step := range verified.Steps {
					if !step.Passed {
						t.Errorf("%s: %s", step.Name, step.Detail)
					}
				}
				t.Fatalf("the derived variant does not verify")
			}
		})
	}
}

// TestDeriveWithoutRecordingSaysTheVariantIsIncomplete.
//
// `--no-record` exists so a reviewer can look at what a transform did without waiting for a world
// to be recorded, and the one thing it must not do is let that directory be mistaken for a
// verifiable fixture.
func TestDeriveWithoutRecordingSaysTheVariantIsIncomplete(t *testing.T) {
	t.Setenv(EnvDSN, "")
	out := filepath.Join(t.TempDir(), "rollout-regression-01-incident-time-shifted")

	stdout, stderr, code := run(t, context.Background(),
		"fixture", "derive", incidentFixture,
		"--transform", fixture.TransformTimeShifted, "--out", out, "--no-record")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	if !containsText(stdout, "PENDING") {
		t.Errorf("the output does not say the variant is incomplete:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "world")); !os.IsNotExist(err) {
		t.Error("--no-record wrote a world")
	}
	if _, err := os.Stat(filepath.Join(out, fixture.ManifestFile)); err != nil {
		t.Errorf("--no-record wrote no manifest: %v", err)
	}
}

func containsText(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
