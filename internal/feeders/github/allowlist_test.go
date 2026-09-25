// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The two allowlists (004 T061, T062; FR-019, FR-023, FR-024).

// Which environments are production is the operator's statement, not a flag in the payload.
func TestTheProductionEnvironmentComesFromTheOperatorsList(t *testing.T) {
	t.Parallel()
	list := github.Allowlist{Environments: []string{"production", " Prod-EU "}}

	for _, environment := range []string{"production", "prod-eu", "PROD-EU"} {
		if ok, _ := list.AllowsEnvironment(environment); !ok {
			t.Errorf("%q was excluded though the operator listed it (case and space are the two ways a "+
				"hand-maintained file differs from a payload)", environment)
		}
	}
	for _, environment := range []string{"preview", "staging", "prod", "prod-experiment", "production-2"} {
		ok, reason := list.AllowsEnvironment(environment)
		if ok {
			t.Errorf("%q was admitted; a prefix match on `prod` admits `prod-experiment`, which is "+
				"precisely the deployment an operator listing `prod-eu` did not mean", environment)
		}
		if reason != github.ReasonEnvironmentNotAllowed {
			t.Errorf("%q was excluded for %q, want %q", environment, reason, github.ReasonEnvironmentNotAllowed)
		}
	}
}

// A deployment that states no environment and one that states an unlisted environment are different
// failures, and only the first is a reason to look at the platform rather than the configuration.
func TestAnUnstatedEnvironmentIsItsOwnReason(t *testing.T) {
	t.Parallel()
	list := github.Allowlist{Environments: []string{"production"}}
	ok, reason := list.AllowsEnvironment("  ")
	if ok {
		t.Fatal("a deployment stating no environment was admitted")
	}
	if reason != github.ReasonNoEnvironmentStated {
		t.Errorf("the reason is %q, want %q; folding it into `not on the allowlist` would send an "+
			"operator to edit a list that was never the problem", reason, github.ReasonNoEnvironmentStated)
	}
}

// An empty list admits nothing. A list nobody configured is not a licence to treat every preview
// deployment as a production change.
func TestAnEmptyAllowlistAdmitsNothing(t *testing.T) {
	t.Parallel()
	var list github.Allowlist
	if ok, _ := list.AllowsEnvironment("production"); ok {
		t.Error("an unconfigured allowlist admitted `production`; empty means none, not everything")
	}
	if ok, _ := list.AllowsWorkflow("deploy"); ok {
		t.Error("an unconfigured allowlist admitted the workflow `deploy`")
	}
}

// FR-024: a run producing no deployment object can still be a rollout, and which runs those are is
// configured rather than guessed.
func TestTheDeployWorkflowListDecidesWhichRunsAreRollouts(t *testing.T) {
	t.Parallel()
	list := github.Allowlist{Workflows: []string{"Deploy to production"}}
	if ok, _ := list.AllowsWorkflow("deploy to production"); !ok {
		t.Error("the listed workflow was excluded")
	}
	ok, reason := list.AllowsWorkflow("test")
	if ok {
		t.Error("an unlisted workflow was admitted; the alternative to a list is treating every green " +
			"run in the repository as a production change")
	}
	if reason != github.ReasonWorkflowNotAllowed {
		t.Errorf("the reason is %q, want %q", reason, github.ReasonWorkflowNotAllowed)
	}
}

// FR-019: exclusions are counted by the filter that decided, never totalled alone. "340 excluded"
// cannot tell a configured filter doing its job from a vocabulary this connector has misread.
func TestExclusionsAreCountedByTheFilterThatDecided(t *testing.T) {
	t.Parallel()
	var excluded github.Exclusions
	excluded.Exclude(github.ReasonEnvironmentNotAllowed)
	excluded.Exclude(github.ReasonEnvironmentNotAllowed)
	excluded.Exclude(github.ReasonWorkflowNotAllowed)
	excluded.Exclude("")

	got := excluded.ByReason()
	switch {
	case got[github.ReasonEnvironmentNotAllowed] != 2:
		t.Errorf("the environment filter is credited with %d, want 2: %v",
			got[github.ReasonEnvironmentNotAllowed], got)
	case got[github.ReasonWorkflowNotAllowed] != 1:
		t.Errorf("the workflow filter is credited with %d, want 1: %v",
			got[github.ReasonWorkflowNotAllowed], got)
	case got["unnamed"] != 1:
		t.Errorf("an exclusion with no reason is %v; it is counted under a name rather than dropped, "+
			"because an exclusion nobody can attribute is what the counter exists to make visible", got)
	case excluded.Total() != 4:
		t.Errorf("Total() = %d, want 4", excluded.Total())
	}

	// The report is a copy, so a caller holding one cannot edit the accumulator through it.
	got[github.ReasonEnvironmentNotAllowed] = 99
	if excluded.ByReason()[github.ReasonEnvironmentNotAllowed] != 2 {
		t.Error("editing the returned map changed the accumulator")
	}
}

// And they reach the cycle's per-area report under the area that decided, with the reason intact.
func TestExclusionsReachTheAreaReportWithTheirReasons(t *testing.T) {
	t.Parallel()
	var excluded github.Exclusions
	excluded.Exclude(github.ReasonEnvironmentNotAllowed)
	excluded.Exclude(github.ReasonEnvironmentNotAllowed)
	excluded.Exclude(github.ReasonNoEnvironmentStated)

	stats := &feeder.AreaStats{}
	excluded.Publish(stats, "deployments")

	report := stats.Report()
	if len(report) != 1 || report[0].Area != "deployments" {
		t.Fatalf("the area report is %+v, want one `deployments` line", report)
	}
	if report[0].Exclusions[github.ReasonEnvironmentNotAllowed] != 2 ||
		report[0].Exclusions[github.ReasonNoEnvironmentStated] != 1 {
		t.Errorf("the area's exclusions are %v, want them kept by reason rather than totalled",
			report[0].Exclusions)
	}
	// Publishing to a nil accumulator is a no-op rather than a crash: telemetry is never a
	// precondition for feeding.
	excluded.Publish(nil, "deployments")
}
