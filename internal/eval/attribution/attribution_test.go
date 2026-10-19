// SPDX-License-Identifier: Apache-2.0

package attribution_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/eval/attribution"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

const casesDir = "../../../fixtures/attribution"

func stores(t *testing.T) fixture.StoreFactory {
	return func(context.Context) (*postgres.Store, func(), error) { return pgtest.Open(t), func() {}, nil }
}

func evaluate(t *testing.T, opts ...catalog.Option) ([]attribution.Outcome, attribution.Summary, []string) {
	t.Helper()
	cases, err := attribution.LoadAll(casesDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	outcomes, err := attribution.Run(context.Background(), stores(t), cases, opts...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	s := attribution.Summarise(outcomes)
	failures := attribution.Gate(s)
	var report strings.Builder
	attribution.Report(&report, outcomes, s, failures)
	t.Log("\n" + report.String())
	return outcomes, s, failures
}

// TestTheLabelledSetPassesTheGate is SC-002 and SC-003 on every pull request (009 T028).
func TestTheLabelledSetPassesTheGate(t *testing.T) {
	t.Parallel()
	_, s, failures := evaluate(t)
	if len(failures) > 0 {
		t.Errorf("the gate fails: %v", failures)
	}
	if s.Cases < 5 {
		t.Errorf("%d labelled cases, research R13 asks for at least five", s.Cases)
	}
	if s.Layer("probable").Entries == 0 {
		t.Error("nothing is attributed by the probable layer, so its calibration is measured on nothing")
	}
}

// TestLoweringTheThresholdFailsTheGate is the planted regression (009 T028): with τ at 0.5, weak
// evidence is attributed instead of suggested, ambiguous-01's monitor 305 goes to Billing on a
// shared word, and the gate must fail on precision, not only on calibration.
func TestLoweringTheThresholdFailsTheGate(t *testing.T) {
	t.Parallel()
	lowered := catalog.DefaultRules()
	lowered.Ruleset, lowered.Threshold = "attribution/rules@1-planted", 0.5
	_, _, failures := evaluate(t, catalog.WithRules(lowered))
	if !slices.ContainsFunc(failures, func(f string) bool { return strings.Contains(f, "SC-002") }) {
		t.Errorf("the gate does not fail on precision with the threshold lowered to 0.5: %v", failures)
	}
}
