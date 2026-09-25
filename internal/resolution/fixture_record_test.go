// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/fixture/fixturetest"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The replay gate for the resolution fixture (FR-047, FR-048, SC-007, SC-011).
//
// `ambiguous-identity-01` is verified here rather than alongside the topology fixtures because
// it is this phase's evidence: it is the fixture that proves a probable rule never merges, that
// a human decision survives a full replay, and that a certain rule which disagrees with a person
// is surfaced instead of applied. Keeping the record and verify passes next to the rules they
// exercise means a change to a rule and the golden it moves land in one diff.
//
// Recording is opt-in, exactly as it is for the shipped topology fixtures:
//
//	SRE_AGENT_RECORD=1 go test ./internal/resolution -run TestRecordResolutionFixture
//
// and the diff is reviewed in the pull request. A golden nobody read is a golden nobody can
// trust (constitution VIII).

const fixturesDir = "../../fixtures"

// resolutionFixtures are the fixtures this phase owns.
var resolutionFixtures = []string{"ambiguous-identity-01"}

func TestMain(m *testing.M) { pgtest.TestMain(m) }

func newRunner(store *postgres.Store) fixture.QueryRunner {
	return query.NewRunner(query.NewEngine(store))
}

// TestRecordResolutionFixture rewrites this phase's goldens.
func TestRecordResolutionFixture(t *testing.T) {
	if os.Getenv("SRE_AGENT_RECORD") != "1" {
		t.Skip("set SRE_AGENT_RECORD=1 to rewrite the resolution fixture's goldens")
	}
	ctx := context.Background()
	factory := fixturetest.PgtestFactory(t)

	for _, name := range resolutionFixtures {
		t.Run(name, func(t *testing.T) {
			report, err := fixture.Record(ctx, factory, filepath.Join(fixturesDir, name), newRunner)
			if err != nil {
				t.Fatalf("Record: %v", err)
			}
			if len(report.Written) == 0 {
				t.Fatalf("no goldens written for %s", name)
			}
			t.Logf("%s: wrote %d golden(s) %v; skipped %v",
				name, len(report.Written), report.Written, report.Skipped)
		})
	}
}

// TestVerifyResolutionFixture replays the fixture from empty, compares every golden, double
// delivers every event and shuffles within each source's declared reordering window.
//
// The human decisions are part of the event stream, so all three passes include them: the
// replay proves they survive a rebuild (SC-007) and the double-delivery pass proves a decision
// re-submitted is a no-op rather than a second merge.
func TestVerifyResolutionFixture(t *testing.T) {
	ctx := context.Background()
	factory := fixturetest.PgtestFactory(t)

	for _, name := range resolutionFixtures {
		t.Run(name, func(t *testing.T) {
			report, err := fixture.Verify(ctx, factory, filepath.Join(fixturesDir, name), fixture.VerifyOptions{
				Shuffles:  2,
				Report:    true,
				NewRunner: newRunner,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			for _, step := range report.Steps {
				if !step.Passed {
					t.Errorf("step %s failed: %s", step.Name, step.Detail)
				}
			}
			t.Logf("%s\n%s", name, report.Markdown())
		})
	}
}
