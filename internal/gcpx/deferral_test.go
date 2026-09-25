// SPDX-License-Identifier: Apache-2.0

package gcpx_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// The published behaviour around the token buckets (T161–T164; FR-149, FR-150).

// The decay: inside the band the integration paces itself, and the pacing gets slower the closer to
// the reserve. Without it the share is spent as fast as it can be and the integration hits the floor
// dead, which honours the reserve and still crowds the humans right up to it.
func TestTheBudgetPacesItselfAsTheReserveApproaches(t *testing.T) {
	clock := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{{
		Class: gcpx.ClassLoggingRead, Limit: 60, Period: time.Minute,
		Scope: gcpx.ScopeProject, SteadyStateShare: 0.40, HumanReserve: 0.30,
	}}, gcpx.NewUsage(gcpx.ConsumerFeeder))
	budget.SetClock(func() time.Time { return clock })

	// The share is 24 tokens and the reserve is 18, so the headroom is 6 and the band is the lower 3.
	// Spending with the clock frozen drains the bucket: the first calls go through freely, and the
	// ones inside the band are paced.
	var free, paced int
	for range 10 {
		err := budget.Issue(context.Background(), gcpx.OpLoggingEntriesList, "nova-production", "")
		if err == nil {
			free++
			continue
		}
		yielded, ok := gcpx.Yielded(err)
		if !ok {
			t.Fatalf("Take: %v", err)
		}
		if yielded.Kind != gcpx.YieldPaced {
			t.Fatalf("the reserve was reached after %d calls; the share is 24 against a reserve of 18, "+
				"so it should pace before it yields: %v", free, err)
		}
		paced++
	}
	if free == 0 {
		t.Fatal("no call went through at a full bucket; the share exists to be used, and an " +
			"integration that paced from the first call would simply have a smaller share")
	}
	if paced == 0 {
		t.Fatalf("no call was paced after %d went through; the decay never engaged, so the integration "+
			"sprints to the reserve and stops dead (FR-149)", free)
	}
}

// A paced yield says it is a delay and names when the call becomes allowed; a reserve yield says the
// floor is the floor. Both are yields and they are different facts about the run.
func TestAPacedYieldIsDistinctFromTheReserveFloor(t *testing.T) {
	clock := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{{
		Class: gcpx.ClassLoggingRead, Limit: 10, Period: time.Minute,
		Scope: gcpx.ScopeProject, SteadyStateShare: 1.0, HumanReserve: 0.30,
	}}, gcpx.NewUsage(gcpx.ConsumerFeeder))
	budget.SetClock(func() time.Time { return clock })

	var lastPaced, lastReserve *gcpx.YieldError
	for range 20 {
		err := budget.Issue(context.Background(), gcpx.OpLoggingEntriesList, "nova-production", "")
		if err == nil {
			continue
		}
		yielded, ok := gcpx.Yielded(err)
		if !ok {
			t.Fatalf("Take: %v", err)
		}
		switch yielded.Kind {
		case gcpx.YieldPaced:
			lastPaced = yielded
		case gcpx.YieldReserve:
			lastReserve = yielded
		}
		// The clock advances enough to clear the pacing but not the reserve, so both kinds are seen.
		clock = clock.Add(2 * time.Second)
		budget.SetClock(func() time.Time { return clock })
	}
	if lastPaced == nil {
		t.Fatal("no paced yield was produced")
	}
	if !strings.Contains(lastPaced.Error(), "paced") {
		t.Errorf("a paced yield does not say so: %q", lastPaced.Error())
	}
	if lastPaced.RetryAt.IsZero() {
		t.Error("a paced yield names no instant at which the call becomes allowed")
	}
	if lastReserve != nil && !strings.Contains(lastReserve.Error(), "never spent") {
		t.Errorf("a reserve yield does not say the reserve is never spent: %q", lastReserve.Error())
	}
	// Whichever kind it is, a caller asking "did we stop for quota" gets the same answer — and it is
	// never "we looked and found nothing".
	if lastPaced.StopReason() != gcpx.StopQuota {
		t.Errorf("stop reason = %q, want %q", lastPaced.StopReason(), gcpx.StopQuota)
	}
	if lastPaced.StopReason().IsAbsence() {
		t.Error("a quota stop was reported as evidence of absence; an investigation that ran out of " +
			"quota and one that found nothing implicated are opposite conclusions (FR-149)")
	}
	if !gcpx.StopEvidence.IsAbsence() {
		t.Error("StopEvidence is not evidence of absence, which leaves nothing that is")
	}
	if gcpx.StopDeferred.IsAbsence() {
		t.Error("deferred work was reported as evidence of absence")
	}
}

// The deferral order is published, and the code follows the same order the configuration states. A
// documented order the code does not follow is worse than no order, because an operator plans around it.
func TestTheDeferralOrderMatchesTheConfiguration(t *testing.T) {
	raw, err := os.ReadFile("../../config/gcp-budget.yaml")
	if err != nil {
		t.Fatalf("read the budget configuration: %v", err)
	}
	block := regexp.MustCompile(`(?s)deferral_order:\n(.*?)\n\n`).FindStringSubmatch(string(raw))
	if block == nil {
		t.Fatal("config/gcp-budget.yaml publishes no deferral_order")
	}
	var configured []string
	for _, line := range strings.Split(block[1], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(line, "- "), "#", 2)[0])
		if name != "" {
			configured = append(configured, name)
		}
	}
	order := gcpx.DeferralOrder()
	if len(configured) != len(order) {
		t.Fatalf("the configuration lists %d areas and the code %d: %v vs %v",
			len(configured), len(order), configured, order)
	}
	for i := range order {
		if string(order[i]) != configured[i] {
			t.Errorf("position %d: the code defers %q and the configuration %q", i, order[i], configured[i])
		}
	}
	// And the P3 surface is first, which FR-057 states in as many words.
	if order[0] != gcpx.AreaLoadBalancersAndDNS {
		t.Errorf("the first area deferred is %q, want the load balancers and DNS (FR-057)", order[0])
	}
}

// Deferral stops before the essential areas unless the caller says it means to, and says which it
// dropped and why. A report that did not distinguish them would let "we dropped the load balancers"
// and "we stopped reading the topology" look alike.
func TestDeferralStopsBeforeTheEssentialAreasUnlessAsked(t *testing.T) {
	// Asking for more than there is to drop cheaply stops at the essential line.
	cheap := gcpx.Defer(5, gcpx.ClassLoggingRead, false)
	if len(cheap) != 3 {
		t.Fatalf("deferred %d areas without permission to touch the essential ones, want the three "+
			"non-essential: %v", len(cheap), cheap)
	}
	for _, deferral := range cheap {
		if deferral.Area.Essential() {
			t.Errorf("%s was deferred without permission; dropping it costs a P1 story", deferral.Area)
		}
		if deferral.Reason == "" {
			t.Errorf("%s was deferred with no reason, so the checkpoint says nothing a reader can act on",
				deferral.Area)
		}
		if deferral.Class != gcpx.ClassLoggingRead {
			t.Errorf("%s does not name the class that ran short", deferral.Area)
		}
	}

	// With permission, the essential ones follow — and their reason says what it costs.
	all := gcpx.Defer(5, gcpx.ClassLoggingRead, true)
	if len(all) != 5 {
		t.Fatalf("deferred %d areas with permission, want all five", len(all))
	}
	var essential *gcpx.Deferral
	for i := range all {
		if all[i].Area.Essential() {
			essential = &all[i]
			break
		}
	}
	if essential == nil {
		t.Fatal("no essential area was deferred even with permission")
	}
	if !strings.Contains(essential.Reason, "P1") {
		t.Errorf("deferring %s does not say a P1 story's coverage is reduced: %q",
			essential.Area, essential.Reason)
	}

	// The summary is in the published order whatever order the deferrals arrive in.
	shuffled := []gcpx.Deferral{
		{Area: gcpx.AreaCloudSQLSettings, Class: gcpx.ClassSQLAdminRead, Reason: "x"},
		{Area: gcpx.AreaLoadBalancersAndDNS, Class: gcpx.ClassComputeRead, Reason: "y"},
	}
	summary := gcpx.SummariseDeferrals(shuffled)
	if strings.Index(summary, string(gcpx.AreaLoadBalancersAndDNS)) >
		strings.Index(summary, string(gcpx.AreaCloudSQLSettings)) {
		t.Errorf("the summary is not in the published order: %q", summary)
	}
}

// A request wider than its cap is narrowed to the cap, and the tightest cap is on the class that
// spends the binding quota.
func TestAWindowWiderThanItsCapIsNarrowedAndTheCapIsNamed(t *testing.T) {
	logging, published := gcpx.WindowCapFor(gcpx.ClassLoggingRead)
	if !published {
		t.Fatal("no window cap is published for logging.read, which is the binding class")
	}
	monitoring, _ := gcpx.WindowCapFor(gcpx.ClassMonitoringQuery)
	cheap, _ := gcpx.WindowCapFor(gcpx.ClassRunRead)
	if logging >= monitoring || monitoring >= cheap {
		t.Errorf("the caps are not tightest-on-the-binding-class: logging %s, monitoring %s, cheap %s",
			logging, monitoring, cheap)
	}

	narrowed, wasNarrowed := gcpx.CapWindow(gcpx.ClassLoggingRead, 30*24*time.Hour)
	if !wasNarrowed || narrowed != logging {
		t.Errorf("a 30-day window on logging.read became %s (narrowed=%v), want the cap %s",
			narrowed, wasNarrowed, logging)
	}
	// A window inside its cap is untouched, and says it was untouched.
	same, touched := gcpx.CapWindow(gcpx.ClassLoggingRead, time.Hour)
	if touched || same != time.Hour {
		t.Errorf("a one-hour window was narrowed to %s", same)
	}
	// A class with no published cap says so rather than returning a cap of zero, which would refuse
	// every request.
	if _, published := gcpx.WindowCapFor(gcpx.ClassPubSubPull); published {
		t.Error("a cap is published for pubsub.pull, which has no window to cap")
	}
	if got, narrowed := gcpx.CapWindow(gcpx.ClassPubSubPull, time.Hour); narrowed || got != time.Hour {
		t.Errorf("an uncapped class narrowed a window to %s", got)
	}

	// And the refusal, for a caller that cannot use a narrowed answer, names the cap.
	err := &gcpx.ErrWindowTooWide{
		Class: gcpx.ClassLoggingRead, Requested: 30 * 24 * time.Hour, Cap: logging,
	}
	if !strings.Contains(err.Error(), logging.String()) {
		t.Errorf("the refusal does not name the cap: %v", err)
	}
}

// The usage report matches the calls made, to the exact call (SC-022).
//
// "Matches the recording exactly" is a claim about *counting*, and the way it goes wrong is a refused
// call counted as a call made — so the report would say the integration spent quota it never spent, and
// an operator reconciling it against GCP's own metrics would find a discrepancy nobody could explain.
func TestTheUsageReportCountsAllowedAndRefusedCallsApart(t *testing.T) {
	clock := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	usage := gcpx.NewUsage(gcpx.ConsumerBackend)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{{
		Class: gcpx.ClassLoggingRead, Limit: 60, Period: time.Minute,
		Scope: gcpx.ScopeProject, SteadyStateShare: 0.40, HumanReserve: 0.30,
	}}, usage)
	budget.SetClock(func() time.Time { return clock })

	var allowed, refused int
	for range 20 {
		if err := budget.Issue(context.Background(), gcpx.OpLoggingEntriesList, "nova-production", ""); err != nil {
			refused++
			continue
		}
		allowed++
	}
	if allowed == 0 || refused == 0 {
		t.Fatalf("allowed %d and refused %d; the test needs both to compare them", allowed, refused)
	}

	rows := usage.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want one per class per key", len(rows))
	}
	row := rows[0]
	if row.Allowed != allowed {
		t.Errorf("the report says %d calls were allowed and %d were: a report that does not match the "+
			"calls made cannot be reconciled against GCP's own metrics (SC-022)", row.Allowed, allowed)
	}
	if row.Refused != refused {
		t.Errorf("the report says %d calls were refused and %d were", row.Refused, refused)
	}
	// And the refusals are NOT counted as calls against the quota: the integration did not spend them.
	if row.Calls != allowed {
		t.Errorf("the report counts %d calls against the quota and only %d were issued; a refused call "+
			"spends nothing, and counting it would claim quota this integration never used",
			row.Calls, allowed)
	}
	if row.RetryAt.IsZero() {
		t.Error("the report names no retry instant after a refusal, so a caller cannot tell when to " +
			"come back")
	}
	if !usage.ReserveHonoured() {
		t.Error("the report says the reserve was spent")
	}
}
