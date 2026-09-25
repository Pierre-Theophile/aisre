// SPDX-License-Identifier: Apache-2.0

package gcpx_test

import (
	"context"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// The call budget (FR-146–FR-149, SC-022; contracts/budget.md).
//
// The property everything here protects: `logging.entries.list` is 60 calls per minute per project,
// not hierarchical, and shared with every human querying Cloud Logging in that project. One call per
// second, project-wide, during an incident, against the same pool as the on-call trying to read the
// logs. The reserve is what stops this integration taking the last of it.

func loggingLimit() gcpx.ClassLimit {
	return gcpx.ClassLimit{
		Class:            gcpx.ClassLoggingRead,
		Limit:            60,
		Period:           time.Minute,
		Scope:            gcpx.ScopeProject,
		SteadyStateShare: 0.40, // 24 of 60
		HumanReserve:     0.30, // 18 of 60, never spent
	}
}

func TestTheHumanReserveIsNeverSpent(t *testing.T) {
	t.Parallel()
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, usage)
	ctx := context.Background()

	// Spend until it refuses. With a 60-call limit, a 40% share and an 18-call reserve, the
	// integration may take the tokens above the reserve and not one more.
	var taken int
	var yield *gcpx.YieldError
	for i := 0; i < 200; i++ {
		err := budget.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", "")
		if err == nil {
			taken++
			continue
		}
		y, ok := gcpx.Yielded(err)
		if !ok {
			t.Fatalf("take %d returned %v, want a typed yield", i, err)
		}
		yield = y
		break
	}

	if yield == nil {
		t.Fatal("the budget never yielded; it would spend the humans' reserve during an incident")
	}
	// The direction matters and an earlier version of this test had it backwards, which made the
	// assertion vacuous: it flagged `Remaining > Reserve`, but the budget refuses WHILE tokens are
	// still at or above the reserve, so that is the healthy case. The breach is yielding with LESS
	// than the reserve left — meaning calls were served out of it before anything stopped.
	if yield.Remaining < yield.Reserve {
		t.Errorf("yielded with %d remaining against a reserve of %d: calls were served out of the "+
			"humans' reserve before the budget stopped", yield.Remaining, yield.Reserve)
	}
	if yield.Reserve != 18 {
		t.Errorf("reserve = %d, want 18 (30%% of 60)", yield.Reserve)
	}
	if yield.RetryAt.IsZero() {
		t.Error("the yield names no retry instant; a caller cannot tell when to come back")
	}

	// SC-022's figure: 100% of the reserve in 100% of cycles.
	if !usage.ReserveHonoured() {
		t.Error("the usage report says the reserve was touched")
	}
}

func TestAYieldIsDistinguishableFromFindingNothing(t *testing.T) {
	t.Parallel()
	// FR-149. An investigation that stopped for quota and one that stopped because nothing was
	// implicated are opposite conclusions, and a consumer that cannot tell them apart will read
	// the first as the second at exactly the wrong moment.
	usage := gcpx.NewUsage(gcpx.ConsumerBackend)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, usage)
	ctx := context.Background()

	var err error
	for i := 0; i < 200 && err == nil; i++ {
		err = budget.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", "")
	}
	if err == nil {
		t.Fatal("never yielded")
	}
	y, ok := gcpx.Yielded(err)
	if !ok {
		t.Fatalf("a quota refusal is not recognisable as one: %v", err)
	}
	if y.Class != gcpx.ClassLoggingRead {
		t.Errorf("yield names class %q, want %q", y.Class, gcpx.ClassLoggingRead)
	}
	// And an ordinary error is NOT a yield, so the check cannot be satisfied by accident.
	if _, ok := gcpx.Yielded(context.Canceled); ok {
		t.Error("context.Canceled was reported as a quota yield")
	}
}

func TestBucketsAreSeparatePerProject(t *testing.T) {
	t.Parallel()
	// The limit is PER PROJECT and not hierarchical (research §7). One shared bucket would let a
	// busy project starve a quiet one of quota it never had a claim on.
	budget := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, gcpx.NewUsage(gcpx.ConsumerFeeder))
	ctx := context.Background()

	for i := 0; i < 200; i++ {
		if err := budget.Issue(ctx, gcpx.OpLoggingEntriesList, "busy", ""); err != nil {
			break
		}
	}
	if err := budget.Issue(ctx, gcpx.OpLoggingEntriesList, "quiet", ""); err != nil {
		t.Errorf("a second project was refused because the first spent its own quota: %v", err)
	}
}

func TestAnUnknownLimitIsMeteredAndAllowedRatherThanRefused(t *testing.T) {
	t.Parallel()
	// "Limit unknown" is never "limit unlimited" — but it is also not a reason to disable the
	// integration. An API whose limit Google does not publish is metered so the usage report can
	// say what was spent, and allowed so a technicality does not take the feature out.
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{{
		Class: gcpx.ClassMonitoringPolicies, Limit: 0, Period: time.Minute, Scope: gcpx.ScopeProject,
	}}, usage)

	for i := 0; i < 50; i++ {
		if err := budget.Issue(context.Background(), gcpx.OpMonitoringAlertPoliciesList, "proj", ""); err != nil {
			t.Fatalf("call %d refused on a class with no documented limit: %v", i, err)
		}
	}
	rows := usage.Rows()
	if len(rows) == 0 || rows[0].Calls != 50 {
		t.Errorf("unlimited-as-far-as-we-know calls were not metered: %+v", rows)
	}
}

func TestFeederAndBackendUsageAreCountedApart(t *testing.T) {
	t.Parallel()
	// FR-113. "What do investigations cost" and "what does ingestion cost" are different questions
	// with different owners, and one total answers neither.
	root := gcpx.NewUsage(gcpx.ConsumerFeeder)
	feeder := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, root.For(gcpx.ConsumerFeeder))
	backend := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, root.For(gcpx.ConsumerBackend))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = feeder.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", "")
	}
	_ = backend.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", "")

	byConsumer := root.Consumers()
	if got := totalCalls(byConsumer[gcpx.ConsumerFeeder]); got != 3 {
		t.Errorf("feeder calls = %d, want 3", got)
	}
	if got := totalCalls(byConsumer[gcpx.ConsumerBackend]); got != 1 {
		t.Errorf("backend calls = %d, want 1", got)
	}
}

func TestBackoffHonoursTheVendorWhenItAsksForLonger(t *testing.T) {
	t.Parallel()
	// Waiting less than the response asked is how a backoff becomes a second rate-limit breach.
	b := gcpx.DefaultBackoff()
	if got := b.Delay(0, 30*time.Second); got < 30*time.Second {
		t.Errorf("delay = %s with a vendor Retry-After of 30s; the vendor knows something we do not", got)
	}
	// And jitter keeps the computed delay inside its cap, so a fleet spreads rather than
	// synchronising into a second thundering herd against the same pool.
	for i := range 10 {
		if got := b.Delay(i, 0); got > b.Max {
			t.Errorf("attempt %d delay %s exceeds the cap %s", i, got, b.Max)
		}
	}
}

func totalCalls(rows []gcpx.ClassUsage) int {
	var n int
	for _, r := range rows {
		n += r.Calls
	}
	return n
}

func TestAQuietHourDoesNotFundABurstThroughTheReserve(t *testing.T) {
	t.Parallel()
	// The refill ceiling is the SHARE, not the limit, and this is the case that makes the
	// difference visible. Without a ceiling at the share, an idle period accumulates tokens up to
	// the full limit — and the next burst spends straight through the humans' reserve in one
	// second, which is precisely what the reserve exists to prevent.
	//
	// An earlier version of this file could not catch that: it took calls in a tight loop, so no
	// wall-clock time passed, refill added nothing, and the ceiling was never reached either way.
	// The test passed against a budget with the ceiling deliberately removed.
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, usage)

	clock := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	budget.SetClock(func() time.Time { return clock })
	ctx := context.Background()

	// One call to seed the bucket, then an hour of silence.
	if err := budget.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", ""); err != nil {
		t.Fatalf("first take: %v", err)
	}
	clock = clock.Add(time.Hour)

	// Drain. With a 60/min limit, a 40% share and an 18-call reserve, the most this may serve
	// after any amount of idling is the share — 24 — and never into the reserve.
	var served int
	for i := 0; i < 500; i++ {
		if err := budget.Issue(ctx, gcpx.OpLoggingEntriesList, "proj", ""); err != nil {
			break
		}
		served++
	}

	if served > 24 {
		t.Errorf("served %d calls in one burst after an idle hour; the share is 24, so the bucket "+
			"refilled past it and the burst reached into the reserve", served)
	}
	if !usage.ReserveHonoured() {
		t.Error("the usage report says the reserve was spent during the burst")
	}
}

// A cold start may spend its SHARE and not the limit (T161).
//
// It was the other way round until the decay made it visible: the bucket seeded at `Limit`, so a fresh
// process could serve `Limit − reserve` calls in its first second — 42 of 60 for the binding class
// rather than the 24 its share allows. `refill` caps at the share, so the excess was unreachable ever
// again, which made it a cold-start burst only. That is exactly when an integration is most likely to
// be starting *because* something is wrong, and when a human is most likely to be querying the same
// project.
func TestAColdStartMaySpendItsShareAndNotTheLimit(t *testing.T) {
	t.Parallel()
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget([]gcpx.ClassLimit{loggingLimit()}, usage)
	clock := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	budget.SetClock(func() time.Time { return clock })

	// The clock never moves, so nothing refills: whatever this serves came from the seed.
	var served int
	for range 500 {
		if err := budget.Issue(context.Background(), gcpx.OpLoggingEntriesList, "proj", ""); err != nil {
			break
		}
		served++
	}
	// The share is 24 and the reserve is 18, so a cold start has 6 calls to spend before it paces.
	if served > 6 {
		t.Errorf("a cold start served %d calls with the clock frozen; the share is 24 against a "+
			"reserve of 18, so the seed must leave 6 — anything more means the bucket was seeded "+
			"above the share", served)
	}
	if served == 0 {
		t.Error("a cold start served nothing; the share exists to be used")
	}
	if !usage.ReserveHonoured() {
		t.Error("the usage report says the reserve was spent during the cold start")
	}
}
