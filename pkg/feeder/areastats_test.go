// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"sync"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Operational telemetry per platform area (004 T046, FR-009).
//
// The area is the unit an operator already approves — it is the first column of the published
// operation surface — so a report indexed the same way is answerable without joining two
// vocabularies.

func reportOf(t *testing.T, stats *feeder.AreaStats, area string) feeder.AreaReport {
	t.Helper()
	for _, line := range stats.Report() {
		if line.Area == area {
			return line
		}
	}
	t.Fatalf("no report line for area %q: %+v", area, stats.Report())
	return feeder.AreaReport{}
}

// A refused call is recorded, not dropped. A report counting only calls that went out would look
// identical whether the budget was comfortable or the connector spent the cycle being refused.
func TestARefusedCallIsRecordedAsAnAttempt(t *testing.T) {
	t.Parallel()

	var stats feeder.AreaStats
	for i := 0; i < 7; i++ {
		stats.Call("deployments", true)
	}
	for i := 0; i < 3; i++ {
		stats.Call("deployments", false)
	}

	line := reportOf(t, &stats, "deployments")
	if line.Calls != 7 {
		t.Errorf("calls = %d, want 7", line.Calls)
	}
	if line.Refusals != 3 {
		t.Errorf("refusals = %d, want 3; a report that counted only calls that went out cannot tell a "+
			"comfortable budget from a cycle spent being refused", line.Refusals)
	}
}

// Exclusions are counted by the filter that decided (FR-019), never totalled.
func TestExclusionsAreCountedByTheFilterThatDecided(t *testing.T) {
	t.Parallel()

	var stats feeder.AreaStats
	for i := 0; i < 340; i++ {
		stats.Excluded("deployments", "environment_filter")
	}
	for i := 0; i < 12; i++ {
		stats.Excluded("deployments", "no_terminal_success")
	}

	line := reportOf(t, &stats, "deployments")
	if got := line.Exclusions["environment_filter"]; got != 340 {
		t.Errorf("environment_filter = %d, want 340", got)
	}
	if got := line.Exclusions["no_terminal_success"]; got != 12 {
		t.Errorf("no_terminal_success = %d, want 12", got)
	}
	if len(line.Exclusions) != 2 {
		t.Errorf("exclusions = %v; a bare total cannot tell a configured filter doing its job from a "+
			"connector that has gone silent while reporting a healthy number", line.Exclusions)
	}

	// An unattributable exclusion is the exact case this counter exists to make visible, so it is
	// recorded under a name rather than dropped.
	stats.Excluded("deployments", "")
	if got := reportOf(t, &stats, "deployments").Exclusions["unnamed"]; got != 1 {
		t.Errorf("unnamed exclusions = %d, want 1: hiding one would leave a connector's silence looking "+
			"like a configured filter doing its job", got)
	}
}

// A deferral is "not now"; an exclusion is "not a change". Folding them together would report a
// connector that ran out of budget as one that found nothing.
func TestADeferralIsNotAnExclusion(t *testing.T) {
	t.Parallel()

	var stats feeder.AreaStats
	stats.Deferred("releases")
	stats.Deferred("releases")
	stats.Excluded("releases", "draft")

	line := reportOf(t, &stats, "releases")
	if line.Deferred != 2 {
		t.Errorf("deferred = %d, want 2", line.Deferred)
	}
	if total := len(line.Exclusions); total != 1 || line.Exclusions["draft"] != 1 {
		t.Errorf("exclusions = %v, want just the one draft; work not done YET is not work decided "+
			"not to be a change", line.Exclusions)
	}
}

// The report is per area and in name order, so two runs of the same shape read the same.
func TestTheReportIsPerAreaAndOrdered(t *testing.T) {
	t.Parallel()

	var stats feeder.AreaStats
	stats.Call("workflow runs", true)
	stats.Call("deployments", true)
	stats.Events("deployments", 4, 1)
	stats.Call("budget", true)

	report := stats.Report()
	if len(report) != 3 {
		t.Fatalf("report has %d areas, want 3: %+v", len(report), report)
	}
	for i := 1; i < len(report); i++ {
		if report[i-1].Area >= report[i].Area {
			t.Errorf("report is not in area order: %q before %q", report[i-1].Area, report[i].Area)
		}
	}
	deployments := reportOf(t, &stats, "deployments")
	if deployments.Emitted != 4 || deployments.Rejected != 1 {
		t.Errorf("events = %d emitted / %d rejected, want 4/1",
			deployments.Emitted, deployments.Rejected)
	}

	// An unattributed count is refused rather than pooled under the empty name, because pooling would
	// make it look attributed.
	stats.Call("", true)
	stats.Excluded("", "whatever")
	if len(stats.Report()) != 3 {
		t.Errorf("a count with no area joined the report: %+v", stats.Report())
	}
}

// The report is a copy: a caller holding one cannot be surprised by a later cycle, and cannot edit
// the counters through it.
func TestTheReportDoesNotAliasTheCounters(t *testing.T) {
	t.Parallel()

	var stats feeder.AreaStats
	stats.Excluded("deployments", "environment_filter")

	report := reportOf(t, &stats, "deployments")
	report.Exclusions["environment_filter"] = 999
	report.Exclusions["invented"] = 1

	fresh := reportOf(t, &stats, "deployments")
	if fresh.Exclusions["environment_filter"] != 1 || len(fresh.Exclusions) != 1 {
		t.Errorf("the counters were edited through a report: %v", fresh.Exclusions)
	}
}

// Several areas polled at once share one accumulator, so it has to be safe for concurrent use.
func TestConcurrentRecordingIsSafe(t *testing.T) {
	t.Parallel()

	var stats feeder.AreaStats
	var wg sync.WaitGroup
	for _, area := range []string{"deployments", "workflow runs", "releases"} {
		wg.Add(1)
		go func(area string) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				stats.Call(area, i%3 != 0)
				stats.Excluded(area, "environment_filter")
				stats.Events(area, 1, 0)
			}
		}(area)
	}
	wg.Wait()

	for _, line := range stats.Report() {
		if line.Calls+line.Refusals != 200 {
			t.Errorf("%s recorded %d calls and %d refusals, want 200 together",
				line.Area, line.Calls, line.Refusals)
		}
		if line.Exclusions["environment_filter"] != 200 {
			t.Errorf("%s recorded %d exclusions, want 200",
				line.Area, line.Exclusions["environment_filter"])
		}
	}
}
