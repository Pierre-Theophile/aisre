// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
)

// profileOf runs a fixture model-free and returns the search-space profile its first brief would
// carry (FR-012a, T123).
func profileOf(t *testing.T, dir string) *engine.Profile {
	t.Helper()
	harness, err := eval.NewHarness(context.Background(), eval.Options{
		Dir: dir, NewStore: storeFactory(t), AuditPath: auditPath,
		Kind: eval.KindModelFree, ModelYAML: modelYAML, PricesYAML: pricesYAML,
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	t.Cleanup(harness.Close)
	e, err := harness.Engine(backend.NewRecordedFromWorld(harness.World()))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	profile := e.Profile()
	if profile == nil {
		t.Fatal("the run built no search-space profile")
	}
	t.Logf("profile:\n%s", profile.Render())
	return profile
}

// TestTheProfileNamesTheScopeAndWhatCanBeAskedOfIt: on the rollout fixture the profile lists the
// subject first, the culprit's service among the entities in scope with the families its pointers
// make queryable, and the edges between them.
func TestTheProfileNamesTheScopeAndWhatCanBeAskedOfIt(t *testing.T) {
	profile := profileOf(t, incidentFixture)

	if len(profile.Entities) < 2 {
		t.Fatalf("entities in scope = %d, want the subject and its neighbours", len(profile.Entities))
	}
	if profile.Entities[0].Ref != profile.Focus {
		t.Errorf("the first entity is %q, want the subject %q", profile.Entities[0].Ref, profile.Focus)
	}
	var payments *engine.ProfileEntity
	for i := range profile.Entities {
		if profile.Entities[i].Ref == "otel.service.name=payments" {
			payments = &profile.Entities[i]
		}
	}
	if payments == nil {
		t.Fatal("the culprit's service is not in scope")
	}
	if !payments.Has(engine.FamilyMetrics) || !payments.Has(engine.FamilyChanges) {
		t.Errorf("payments families = %+v, want metrics and changes queryable", payments.Families)
	}
	if len(profile.Edges) == 0 {
		t.Error("the profile carries no edge between the entities in scope")
	}
	if again := profileOf(t, incidentFixture).Render(); again != profile.Render() {
		t.Error("the profile is not deterministic across two runs of the same fixture")
	}
}

// TestTheProfileNamesAFeederThatHasNotReportedTheWindow: on the feeder-gap fixture the source that
// went quiet is a blind spot of the profile, as it stood at the pinned observed instant.
func TestTheProfileNamesAFeederThatHasNotReportedTheWindow(t *testing.T) {
	profile := profileOf(t, repoRoot+"/fixtures/incidents/unknown-feeder-gap-01")

	found := false
	for _, spot := range profile.BlindSpots {
		if strings.Contains(spot, "k8s:demo") {
			found = true
		}
	}
	if !found {
		t.Errorf("blind spots %q do not name the feeder k8s:demo", profile.BlindSpots)
	}
}
