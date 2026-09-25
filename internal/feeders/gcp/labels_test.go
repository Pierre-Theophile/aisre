// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Labels (T049, FR-124, FR-125, FR-126).

// An unlisted key becomes **nothing** — not a property, not a node, not a claim. The three are
// asserted separately because dropping one and keeping another is the plausible half-measure.
func TestAnUnlistedLabelKeyBecomesNothing(t *testing.T) {
	policy := gcpfeeder.DefaultLabelPolicy()
	out := policy.Apply("nova-production", map[string]string{
		"environment":  "production",
		"jira-ticket":  "PLAT-4412",
		"release-name": "autumn-sprint",
		"owner-name":   "a person's name",
	})

	if _, kept := out.Props["jira-ticket"]; kept {
		t.Error("an unlisted label became a property (FR-124)")
	}
	if _, kept := out.Props["release-name"]; kept {
		t.Error("an unlisted label became a property")
	}
	if len(out.Props) != 1 {
		t.Fatalf("kept %d properties, want 1 (only the allowlisted environment): %v", len(out.Props), out.Props)
	}
	// The drop is recorded with a reason: "this key is not on the allowlist" and "nobody set this
	// label" are different facts about the estate.
	dropped := map[string]string{}
	for _, outcome := range out.Dropped() {
		dropped[outcome.Key] = outcome.Dropped
	}
	for _, key := range []string{"jira-ticket", "release-name", "owner-name"} {
		if dropped[key] == "" {
			t.Errorf("%s was dropped with no recorded reason", key)
		}
	}
}

// FR-126. A label value that measures something never becomes a property: it changes without a
// deploy, a telemetry backend is what answers about it, and a graph property holding one is a stale
// number that looks authoritative.
func TestALabelValueThatMeasuresSomethingNeverBecomesAProperty(t *testing.T) {
	measurements := []string{"8", "2.5", "250ms", "99.9%", "2Gi", "512mb", "4cpu", "1500rps", "30s", "-1"}
	for _, value := range measurements {
		if !gcpfeeder.IsMeasurement(value) {
			t.Errorf("IsMeasurement(%q) = false; it measures something (FR-126)", value)
		}
	}
	// And the names and identifiers that must survive, or the rule is unusable: a version tag, a
	// service name that starts with a digit, a region.
	names := []string{"v2", "2024-autumn", "s3-proxy", "checkout", "europe-west1", "8x-large",
		"payments-v2", "1-2-3-service", "gen2"}
	for _, value := range names {
		if gcpfeeder.IsMeasurement(value) {
			t.Errorf("IsMeasurement(%q) = true; it names something", value)
		}
	}

	// End to end: an allowlisted key holding a measurement is dropped anyway.
	out := gcpfeeder.DefaultLabelPolicy().Apply("nova-production", map[string]string{
		"component": "250ms",
		"service":   "checkout",
	})
	if _, kept := out.Props["component"]; kept {
		t.Error("an allowlisted label holding a measurement became a property (FR-126)")
	}
	if out.Props["service"] != "checkout" {
		t.Errorf("an allowlisted label holding a name was dropped: %v", out.Props)
	}
}

// The environment ladder: the label the resource declares, then the published project mapping, then
// `unknown` — never a guess, and never "production" by default.
func TestTheEnvironmentLadderPrefersTheLabelThenTheProjectThenUnknown(t *testing.T) {
	policy := gcpfeeder.DefaultLabelPolicy()
	policy.EnvironmentFromProject = map[string]string{"nova-production": "production"}

	declared := policy.Apply("nova-production", map[string]string{"environment": "prod-eu"})
	if declared.Environment != "prod-eu" || declared.EnvironmentSource != gcpfeeder.EnvironmentFromLabel {
		t.Fatalf("a declared environment gave %q from %q", declared.Environment, declared.EnvironmentSource)
	}

	mapped := policy.Apply("nova-production", map[string]string{})
	if mapped.Environment != "production" || mapped.EnvironmentSource != gcpfeeder.EnvironmentFromProject {
		t.Fatalf("a mapped project gave %q from %q", mapped.Environment, mapped.EnvironmentSource)
	}

	unmapped := policy.Apply("some-other-project", map[string]string{})
	// Against the literal, not against the constant. Comparing the derived value to
	// EnvironmentUnknown passes whatever that constant is set to — including "production", which is
	// the exact value config/gcp.yaml says an unmapped project must never get.
	if unmapped.Environment != "unknown" {
		t.Fatalf("an unmapped project gave %q, want \"unknown\"; an unmapped project is unknown, "+
			"never a guess and never production by default", unmapped.Environment)
	}
	if gcpfeeder.EnvironmentUnknown != "unknown" {
		t.Fatalf("EnvironmentUnknown is %q; config/gcp.yaml publishes \"unknown\"", gcpfeeder.EnvironmentUnknown)
	}
	if unmapped.EnvironmentSource != gcpfeeder.EnvironmentFromDefault {
		t.Errorf("the default environment reports source %q", unmapped.EnvironmentSource)
	}

	// An environment label holding a measurement does not become an environment: the label is read
	// from the *kept* properties, so the measurement rule applies first.
	measured := policy.Apply("some-other-project", map[string]string{"environment": "3"})
	if measured.Environment != gcpfeeder.EnvironmentUnknown {
		t.Errorf("an environment label holding a measurement became the environment %q", measured.Environment)
	}
}

// The published default carries no project mapping, and that is FR-131 rather than an oversight: no
// code and no checked-in default may assume a project name.
func TestThePublishedDefaultAssumesNoProjectName(t *testing.T) {
	policy := gcpfeeder.DefaultLabelPolicy()
	if len(policy.EnvironmentFromProject) != 0 {
		t.Fatalf("the default policy maps %d projects; no checked-in default may assume a project "+
			"name (FR-131): %v", len(policy.EnvironmentFromProject), policy.EnvironmentFromProject)
	}
	if policy.EnvironmentDefault != gcpfeeder.EnvironmentUnknown {
		t.Errorf("the default environment is %q, want %q", policy.EnvironmentDefault, gcpfeeder.EnvironmentUnknown)
	}
}

// An owner label becomes an owner; no owner label means no OWNER node, not an owner called "unknown"
// — a node nobody owns is a node somebody will try to page (FR-125).
func TestNoOwnerLabelMeansNoOwnerRatherThanAnUnknownOwner(t *testing.T) {
	policy := gcpfeeder.DefaultLabelPolicy()
	withTeam := policy.Apply("nova-production", map[string]string{"team": "payments"})
	if withTeam.Owner != "payments" {
		t.Fatalf("Owner = %q, want payments", withTeam.Owner)
	}
	without := policy.Apply("nova-production", map[string]string{"service": "checkout"})
	if without.Owner != "" {
		t.Fatalf("Owner = %q with no team label; an OWNER node nobody owns is a node somebody will "+
			"try to page (FR-125)", without.Owner)
	}
}

// Outcomes are sorted by key, because they reach a checkpoint note and a note that reorders makes a
// fixture fail for no reason.
func TestLabelOutcomesAreDeterministic(t *testing.T) {
	policy := gcpfeeder.DefaultLabelPolicy()
	labels := map[string]string{"team": "payments", "environment": "production", "zzz": "x", "aaa": "y"}
	first := policy.Apply("nova-production", labels)
	for i := 0; i < 5; i++ {
		again := policy.Apply("nova-production", labels)
		if len(again.Outcomes) != len(first.Outcomes) {
			t.Fatalf("outcome count moved between runs")
		}
		for j := range first.Outcomes {
			if again.Outcomes[j] != first.Outcomes[j] {
				t.Fatalf("outcome %d moved between runs: %+v vs %+v", j, again.Outcomes[j], first.Outcomes[j])
			}
		}
	}
}
